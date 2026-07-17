package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/timeutil"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// createUserRequest is the JSON body for POST /admin/api/users.
type createUserRequest struct {
	Username             string   `json:"username"`
	Quota5hLimit         int      `json:"quota_5h_limit"`
	QuotaTotalLimit      int      `json:"quota_total_limit"`
	QuotaTokenTotalLimit *int     `json:"quota_token_total_limit"` // 0/nil = unlimited
	ExpiresAt            string   `json:"expires_at"`
	RouteMode            string   `json:"route_mode"`
	FixedProvider        string   `json:"fixed_provider"`
	FixedMultiplier      *float64 `json:"fixed_multiplier"`
	MaxBodySize          int      `json:"max_body_size"`
	MaxConcurrency       int      `json:"max_concurrency"`
}

// updateUserRequest is the JSON body for PUT /admin/api/users/{id}.
type updateUserRequest struct {
	Quota5hLimit         *int     `json:"quota_5h_limit"`
	QuotaTotalLimit      *int     `json:"quota_total_limit"`
	QuotaTokenTotalLimit *int     `json:"quota_token_total_limit"` // 0/nil = unlimited
	Status               *string  `json:"status"`
	RegenerateKey        *bool    `json:"regenerate_key"`
	RouteMode            *string  `json:"route_mode"`
	FixedProvider        *string  `json:"fixed_provider"`
	FixedMultiplier      *float64 `json:"fixed_multiplier"`
	FixedMultiplierClear bool     `json:"fixed_multiplier_clear"`
	MaxBodySize          *int     `json:"max_body_size"`
	MaxConcurrency       *int     `json:"max_concurrency"`
}

// extendUserRequest is the JSON body for POST /admin/api/users/{id}/extend.
type extendUserRequest struct {
	Days  int    `json:"days"`
	Until string `json:"until"`
}

// CreateUser handles POST /admin/api/users.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	if req.Username == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Username is required"})
		return
	}

	// Apply defaults
	if req.Quota5hLimit <= 0 {
		req.Quota5hLimit = h.Default5hLimit
	}
	if req.QuotaTotalLimit <= 0 {
		req.QuotaTotalLimit = h.DefaultTotalLimit
	}
	if req.RouteMode == "" {
		req.RouteMode = "auto"
	}

	// Per-request body cap. <=0 falls back to 1MB; cap at the nginx ceiling.
	maxBodySize := req.MaxBodySize
	if maxBodySize <= 0 {
		maxBodySize = models.DefaultMaxBodySize
	}
	if maxBodySize > models.MaxBodySizeCeiling {
		maxBodySize = models.MaxBodySizeCeiling
	}

	// Generate sub-key
	subKey := auth.GenerateSubKey(h.SubKeySalt, 0) // userID is not yet known, use 0 as placeholder
	// Regenerate with a proper approach: generate, then we'll store
	subKeyHash := auth.HashSubKey(subKey)
	subKeyPreview := auth.SubKeyPreview(subKey)

	// Users created via admin have no password (only admin accounts have passwords)
	emptyPassHash := "$2a$10$placeholder" // This is just a placeholder; user accounts use sub-keys

	user, err := models.CreateUser(
		h.DB, req.Username, emptyPassHash, subKeyHash, subKeyPreview,
		"user", "active", req.ExpiresAt, req.RouteMode, req.FixedProvider,
		req.Quota5hLimit, req.QuotaTotalLimit, req.FixedMultiplier, maxBodySize,
	)
	if err != nil {
		log.Printf("ERROR: create user: %v", err)
		// A duplicate username triggers the users.username UNIQUE constraint.
		// Surface a 409 Conflict with a clear message instead of a generic 500,
		// so API clients get actionable feedback (keeping errors explicit, never silent).
		if isUniqueConstraintError(err) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "username already exists"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create user: " + err.Error()})
		return
	}

	// Apply the optional cumulative Token cap (0/nil = unlimited). Done right
	// after user creation so the response can reflect the persisted value.
	if req.QuotaTokenTotalLimit != nil {
		if err := models.UpdateQuotaTokenTotalLimit(h.DB, user.ID, *req.QuotaTokenTotalLimit); err != nil {
			log.Printf("ERROR: update quota token total limit on create: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to set token limit: " + err.Error()})
			return
		}
	}

	// Apply a per-user concurrency cap if the admin supplied one (>0). A value
	// of 0 (or absent) leaves the DB default (10); unlimited is set via the edit
	// form (PUT max_concurrency=0).
	if req.MaxConcurrency > 0 {
		if err := models.UpdateUserMaxConcurrency(h.DB, user.ID, req.MaxConcurrency); err != nil {
			log.Printf("ERROR: update max concurrency on create: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to set concurrency limit: " + err.Error()})
			return
		}
		user.MaxConcurrency = req.MaxConcurrency
	}

	// Now regenerate the sub-key with the actual user ID as input
	actualSubKey := auth.GenerateSubKey(h.SubKeySalt, user.ID)
	actualSubKeyHash := auth.HashSubKey(actualSubKey)
	actualSubKeyPreview := auth.SubKeyPreview(actualSubKey)

	if err := models.RegenerateUserKey(h.DB, user.ID, actualSubKeyHash, actualSubKeyPreview); err != nil {
		log.Printf("ERROR: update user key: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to set user key"})
		return
	}

	// Write audit log for user creation (including route/multiplier info).
	auditDetail := fmt.Sprintf(`{"username":"%s","route_mode":"%s","fixed_provider":"%s"`, req.Username, req.RouteMode, req.FixedProvider)
	if req.FixedMultiplier != nil {
		auditDetail += fmt.Sprintf(`,"fixed_multiplier":%v`, *req.FixedMultiplier)
	} else {
		auditDetail += `,"fixed_multiplier":null`
	}
	auditDetail += "}"
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"user.create", "user", strconv.FormatInt(user.ID, 10), auditDetail, time.Now().Format(time.RFC3339),
	)

	// Return the sub-key in plaintext (only time)
	user.SubKeyPreview = actualSubKeyPreview
	response := map[string]any{
		"id":                user.ID,
		"username":          user.Username,
		"sub_key":           actualSubKey,
		"sub_key_preview":   actualSubKeyPreview,
		"quota_5h_limit":    user.Quota5hLimit,
		"quota_5h_used":     user.Quota5hUsed,
		"quota_total_limit": user.QuotaTotalLimit,
		"quota_total_used":  user.QuotaTotalUsed,
		"route_mode":        user.RouteMode,
		"fixed_provider":    user.FixedProvider,
		"status":            user.Status,
		"max_body_size":     user.MaxBodySize,
		"max_concurrency":   user.MaxConcurrency,
		"created_at":        user.CreatedAt,
	}
	// Cumulative Token quota fields (0 = unlimited). Reflect the value the admin
	// supplied, if any; otherwise the default of 0 (unlimited) applies.
	response["quota_token_total_limit"] = 0
	response["quota_token_total_used"] = 0
	if req.QuotaTokenTotalLimit != nil {
		response["quota_token_total_limit"] = *req.QuotaTokenTotalLimit
	}
	if req.FixedMultiplier != nil {
		response["fixed_multiplier"] = *req.FixedMultiplier
	}

	writeJSON(w, http.StatusCreated, response)
}

// ListUsers handles GET /admin/api/users.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := models.ListUsers(h.DB)
	if err != nil {
		log.Printf("ERROR: list users: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to list users"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"data": users})
}

// UpdateUser handles PUT /admin/api/users/{id}.
func (h *Handler) UpdateUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	// Verify user exists
	user, err := models.GetUserByID(h.DB, userID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}

	response := map[string]any{}
	routeChanged := false

	// Update quota limits
	if req.Quota5hLimit != nil || req.QuotaTotalLimit != nil {
		if err := models.UpdateQuotaLimits(h.DB, userID, req.Quota5hLimit, req.QuotaTotalLimit); err != nil {
			log.Printf("ERROR: update quota limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update quota"})
			return
		}
		response["quota_updated"] = true
	}

	// Update cumulative Token cap (0/nil = unlimited). Lowering the limit below
	// current usage blocks on the next request (self-consistent).
	if req.QuotaTokenTotalLimit != nil {
		if err := models.UpdateQuotaTokenTotalLimit(h.DB, userID, *req.QuotaTokenTotalLimit); err != nil {
			log.Printf("ERROR: update quota token total limit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update token quota"})
			return
		}
		if q, gErr := models.GetQuota(h.DB, userID); gErr == nil && q != nil {
			response["quota_token_total_limit"] = q.QuotaTokenTotalLimit
			response["quota_token_total_used"] = q.QuotaTokenTotalUsed
		} else {
			response["quota_token_total_limit"] = *req.QuotaTokenTotalLimit
			response["quota_token_total_used"] = 0
		}
	}

	// Update status
	if req.Status != nil {
		if *req.Status != "active" && *req.Status != "disabled" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Status must be 'active' or 'disabled'"})
			return
		}
		if err := models.UpdateUserStatus(h.DB, userID, *req.Status); err != nil {
			log.Printf("ERROR: update user status: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update status"})
			return
		}
		response["status"] = *req.Status
	}

	// Update route_mode / fixed_provider (admin users are protected).
	if req.RouteMode != nil || req.FixedProvider != nil {
		if user.Role == "admin" {
			log.Printf("WARNING: ignoring route_mode/fixed_provider update for admin user %d", userID)
		} else {
			rm := user.RouteMode
			fp := user.FixedProvider
			if req.RouteMode != nil {
				rm = *req.RouteMode
			}
			if req.FixedProvider != nil {
				fp = *req.FixedProvider
			}
			if err := models.UpdateUserRoute(h.DB, userID, rm, fp); err != nil {
				log.Printf("ERROR: update user route: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update route"})
				return
			}
			response["route_mode"] = rm
			response["fixed_provider"] = fp
			routeChanged = true
		}
	}

	// Update per-request body cap.
	if req.MaxBodySize != nil {
		v := *req.MaxBodySize
		if v <= 0 {
			v = models.DefaultMaxBodySize
		}
		if v > models.MaxBodySizeCeiling {
			v = models.MaxBodySizeCeiling
		}
		if err := models.UpdateUserMaxBodySize(h.DB, userID, v); err != nil {
			log.Printf("ERROR: update max body size: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update max body size"})
			return
		}
		response["max_body_size"] = v
	}

	// Update per-user concurrency cap (0 = unlimited).
	if req.MaxConcurrency != nil {
		v := *req.MaxConcurrency
		if v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_concurrency must be >= 0"})
			return
		}
		if err := models.UpdateUserMaxConcurrency(h.DB, userID, v); err != nil {
			log.Printf("ERROR: update max concurrency: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update max concurrency"})
			return
		}
		response["max_concurrency"] = v
		detail := fmt.Sprintf("max_concurrency → %d", v)
		if v == 0 {
			detail = "max_concurrency → 不限"
		}
		_, _ = h.DB.Exec(
			`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			"user.update", "user", strconv.FormatInt(userID, 10), detail, time.Now().Format(time.RFC3339),
		)
	}

	// Update fixed_multiplier. fixed_multiplier_clear takes priority —
	// it distinguishes "clear to NULL" from "field not present in JSON".
	if req.FixedMultiplierClear {
		if err := models.UpdateFixedMultiplier(h.DB, userID, nil); err != nil {
			log.Printf("ERROR: clear fixed multiplier: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to clear fixed multiplier"})
			return
		}
		response["fixed_multiplier"] = nil
		routeChanged = true
	} else if req.FixedMultiplier != nil {
		// Validate range 0.1–100.0 to match global multiplier semantics.
		if *req.FixedMultiplier < 0.1 || *req.FixedMultiplier > 100.0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fixed_multiplier must be between 0.1 and 100.0"})
			return
		}
		if err := models.UpdateFixedMultiplier(h.DB, userID, req.FixedMultiplier); err != nil {
			log.Printf("ERROR: update fixed multiplier: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update fixed multiplier"})
			return
		}
		response["fixed_multiplier"] = req.FixedMultiplier
		routeChanged = true
	}

	// Write audit log for route/multiplier changes.
	if routeChanged {
		detailParts := []string{}
		if req.RouteMode != nil && user.Role != "admin" {
			detailParts = append(detailParts, fmt.Sprintf("route_mode: %s → %s", user.RouteMode, *req.RouteMode))
		}
		if req.FixedProvider != nil && user.Role != "admin" {
			detailParts = append(detailParts, fmt.Sprintf("fixed_provider: %q → %q", user.FixedProvider, *req.FixedProvider))
		}
		if req.FixedMultiplierClear {
			detailParts = append(detailParts, "fixed_multiplier → cleared (NULL)")
		} else if req.FixedMultiplier != nil {
			detailParts = append(detailParts, fmt.Sprintf("fixed_multiplier → %v", *req.FixedMultiplier))
		}
		detail := ""
		for i, p := range detailParts {
			if i > 0 {
				detail += "; "
			}
			detail += p
		}
		_, _ = h.DB.Exec(
			`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			"user.route_update", "user", strconv.FormatInt(userID, 10), detail, time.Now().Format(time.RFC3339),
		)
	}

	// Regenerate key
	if req.RegenerateKey != nil && *req.RegenerateKey {
		newSubKey := auth.GenerateSubKey(h.SubKeySalt, userID)
		newSubKeyHash := auth.HashSubKey(newSubKey)
		newSubKeyPreview := auth.SubKeyPreview(newSubKey)

		if err := models.RegenerateUserKey(h.DB, userID, newSubKeyHash, newSubKeyPreview); err != nil {
			log.Printf("ERROR: regenerate key: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to regenerate key"})
			return
		}

		response["new_sub_key"] = newSubKey
		response["sub_key_preview"] = newSubKeyPreview
		response["old_key_disabled"] = true
	}

	writeJSON(w, http.StatusOK, response)
}

// GetUserCalls handles GET /admin/api/users/{id}/calls.
func (h *Handler) GetUserCalls(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	// Parse query params
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	filter := models.CallLogFilter{
		UserID: userID,
		From:   from,
		To:     to,
		Page:   page,
		Limit:  limit,
	}

	result, err := models.QueryCallLogs(h.DB, filter)
	if err != nil {
		log.Printf("ERROR: query call logs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to query call logs"})
		return
	}

	writeJSON(w, http.StatusOK, result)
}

// ExtendUser handles POST /admin/api/users/{id}/extend.
func (h *Handler) ExtendUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	// Verify user exists.
	user, err := models.GetUserByID(h.DB, userID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}

	// Admin users never expire — refuse extension.
	if user.Role == "admin" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Admin users do not expire"})
		return
	}

	var req extendUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}

	now := time.Now().In(timeutil.ShanghaiTZ)
	var newExpiresAt string
	oldExpiresAt := user.ExpiresAt

	switch {
	case req.Until != "":
		// Use the explicit until date directly.
		newExpiresAt = req.Until
	case req.Days > 0:
		// Calculate from current expiry (or NOW if permanent).
		base := now
		if oldExpiresAt != "" {
			parsed, err := time.Parse(time.RFC3339, oldExpiresAt)
			if err == nil {
				base = parsed
			}
		}
		newExpiresAt = base.AddDate(0, 0, req.Days).Format(time.RFC3339)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Provide either 'days' or 'until'"})
		return
	}

	if err := models.ExtendUserExpiry(h.DB, userID, newExpiresAt); err != nil {
		log.Printf("ERROR: extend user expiry: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to extend user expiry"})
		return
	}

	// Write audit log.
	detail := fmt.Sprintf("expires_at: %s → %s", oldExpiresAt, newExpiresAt)
	if oldExpiresAt == "" {
		detail = fmt.Sprintf("expires_at: (permanent) → %s", newExpiresAt)
	}
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"extend", "user", strconv.FormatInt(userID, 10), detail, now.Format(time.RFC3339),
	)

	// Format message for response.
	parsedNew, _ := time.Parse(time.RFC3339, newExpiresAt)
	dateDisplay := newExpiresAt
	if parsedNew.Unix() > 0 {
		dateDisplay = parsedNew.In(timeutil.ShanghaiTZ).Format("2006-01-02")
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"expires_at": newExpiresAt,
		"message":    "有效期已更新至 " + dateDisplay,
	})
}

// DeleteUser handles DELETE /admin/api/users/{id}.
// Hard-deletes the user row from the database. Sub-keys and call logs are
// automatically cleaned up via ON DELETE CASCADE foreign key constraints.
func (h *Handler) DeleteUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	// Check user exists
	user, err := models.GetUserByID(h.DB, userID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}

	// Prevent self-deletion of admin
	if user.Role == "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "Cannot delete admin user"})
		return
	}

	if _, err := h.DB.Exec("DELETE FROM users WHERE id = ?", userID); err != nil {
		log.Printf("ERROR: delete user %d: %v", userID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to delete user"})
		return
	}

	log.Printf("User %d (%s) deleted", userID, user.Username)
	writeJSON(w, http.StatusOK, map[string]string{"message": "User deleted"})
}

// isUniqueConstraintError reports whether err is (or wraps) a SQLite UNIQUE
// constraint violation. The modernc.org/sqlite driver enables extended result
// codes by default, so a duplicate-key error carries code SQLITE_CONSTRAINT_UNIQUE (2067).
// A message-based fallback is kept for robustness against driver variations.
func isUniqueConstraintError(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return true
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
