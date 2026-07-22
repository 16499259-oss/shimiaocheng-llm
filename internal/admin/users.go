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
	"llm_api_gateway/internal/proxy"
	"llm_api_gateway/internal/timeutil"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// createUserRequest is the JSON body for POST /admin/api/users.
type createUserRequest struct {
	Username             string   `json:"username"`
	Quota5hLimit         int      `json:"quota_5h_limit"`
	QuotaTotalLimit      int      `json:"quota_total_limit"`
	QuotaTokenTotalLimit int      `json:"quota_token_total_limit"` // 0 = 不限制（默认）
	QuotaToken5hLimit    int      `json:"quota_token_5h_limit"`    // 0 = 不限制（默认）
	QuotaTokenWeekLimit  int      `json:"quota_token_week_limit"`  // 0 = 不限制（默认）
	QuotaWeekStart       *string  `json:"quota_week_start"`        // nil = 不改(回退 now)；"" = 清除；RFC3339 UTC（与 update/create 表单一致）
	ExpiresAt            string   `json:"expires_at"`
	RouteMode            string   `json:"route_mode"`
	FixedProvider        string   `json:"fixed_provider"`
	FixedMultiplier      *float64 `json:"fixed_multiplier"`
	MaxBodySize          int      `json:"max_body_size"`
	MaxConcurrency       *int     `json:"max_concurrency"` // nil = apply default (10); 0 = unlimited; >0 = cap
}

// updateUserRequest is the JSON body for PUT /admin/api/users/{id}.
type updateUserRequest struct {
	Quota5hLimit         *int     `json:"quota_5h_limit"`
	QuotaTotalLimit      *int     `json:"quota_total_limit"`
	QuotaTokenTotalLimit *int     `json:"quota_token_total_limit"` // nil = 不改；0 = 不限制
	QuotaToken5hLimit    *int     `json:"quota_token_5h_limit"`    // nil = 不改；0 = 不限制
	QuotaTokenWeekLimit  *int     `json:"quota_token_week_limit"`  // nil = 不改；0 = 不限制
	QuotaWeekStart       *string  `json:"quota_week_start"`        // nil = 不改；"" = 清除(回退 now)；RFC3339 UTC
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
	Days            int    `json:"days"`
	Until           string `json:"until"`
	ResetTokenStats bool   `json:"reset_token_stats"`
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

	// Count quotas: since 2026-07-21 a limit of 0 means "unlimited" (unified
	// with the Token cap). So 0 is preserved and stored as-is; only negative
	// values are rejected. A blank form field arrives as 0 and yields an
	// unlimited call-count user, matching UpdateUser's contract.
	if req.Quota5hLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_5h_limit must be >= 0 (0 = 不限制)"})
		return
	}
	if req.QuotaTotalLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_total_limit must be >= 0 (0 = 不限制)"})
		return
	}
	if req.RouteMode == "" {
		req.RouteMode = "auto"
	}

	// Normalize expires_at at the API boundary (accept RFC3339 or a bare date;
	// any other format is rejected so a malformed value can never be stored as
	// a permanent key — see audit F1).
	normExpires, err := models.NormalizeExpiry(req.ExpiresAt)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	req.ExpiresAt = normExpires

	// Per-request body cap. <=0 falls back to 1MB; cap at the nginx ceiling.
	maxBodySize := req.MaxBodySize
	if maxBodySize <= 0 {
		maxBodySize = models.DefaultMaxBodySize
	}
	if maxBodySize > models.MaxBodySizeCeiling {
		maxBodySize = models.MaxBodySizeCeiling
	}

	// Per-user concurrency cap. Unified contract with the edit endpoint:
	//   nil (field absent) -> apply default cap (10); 0 -> unlimited; positive N -> cap.
	//   negative -> 400; above the hard ceiling -> 400 ("并发上限不能超过 200").
	maxConc := models.DefaultMaxConcurrency
	if req.MaxConcurrency != nil {
		v := *req.MaxConcurrency
		if v < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_concurrency must be >= 0"})
			return
		}
		if v > models.MaxConcurrencyHardLimit {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "并发上限不能超过 200"})
			return
		}
		maxConc = v
	}

	// Users created via admin have no password (only admin accounts have passwords)
	emptyPassHash := "$2a$10$placeholder" // This is just a placeholder; user accounts use sub-keys

	// Insert the user row first with empty sub-key placeholders, then generate
	// the real sub-key bound to the actual user.ID and UPDATE it below. This
	// avoids generating a throwaway key that is discarded right after the INSERT.
	// A non-positive fixed multiplier is meaningless (storing 0 would make the
	// user's effective call count 0 on every request). Normalize to "no fixed
	// multiplier" (nil) so it is never persisted as 0 — the panel's "set to 0 =
	// clear" intent is honoured on create too (audit LOW: 固定倍率 0=清除 实际无效).
	if req.FixedMultiplier != nil && *req.FixedMultiplier <= 0 {
		req.FixedMultiplier = nil
	}

	user, err := models.CreateUser(
		h.DB, req.Username, emptyPassHash, "", "",
		"user", "active", req.ExpiresAt, req.RouteMode, req.FixedProvider,
		req.Quota5hLimit, req.QuotaTotalLimit, req.FixedMultiplier, maxBodySize, maxConc,
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

	// Reject a negative cumulative Token cap (it would brick the user via the
	// atomic gate with no other error surfaced). 0 = unlimited (no-op),
	// positive = applied (see audit F3).
	if req.QuotaTokenTotalLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_total_limit must be >= 0"})
		return
	}
	if req.QuotaTokenTotalLimit > 0 {
		if err := models.UpdateQuotaTokenTotalLimit(h.DB, user.ID, req.QuotaTokenTotalLimit); err != nil {
			log.Printf("ERROR: set token limit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to set token limit: " + err.Error()})
			return
		}
	}

	// Reject negative window Token caps (same bricking risk as the cumulative
	// cap). 0 / omitted = unlimited (no-op). Both the 5h-window and weekly caps
	// are written in a SINGLE statement so a create that sets BOTH dimensions
	// does not clobber the other (the partial-update path in UpdateUser instead
	// reads the current caps first and overrides only the provided dimension).
	if req.QuotaToken5hLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_5h_limit must be >= 0"})
		return
	}
	if req.QuotaTokenWeekLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_week_limit must be >= 0"})
		return
	}
	if req.QuotaToken5hLimit > 0 || req.QuotaTokenWeekLimit > 0 {
		if err := models.UpdateQuotaTokenWindowLimits(h.DB, user.ID, req.QuotaToken5hLimit, req.QuotaTokenWeekLimit); err != nil {
			log.Printf("ERROR: set token window limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to set token window limits: " + err.Error()})
			return
		}
	}

	// Honor a configured weekly start anchor (fixed 7-day phase). The create
	// form sends RFC3339 UTC (via toISOString); "" or omitted falls back to now
	// inside SetQuotaWeekStart. This mirrors the edit path, which already sets it.
	if req.QuotaWeekStart != nil {
		if err := models.SetQuotaWeekStart(h.DB, user.ID, *req.QuotaWeekStart); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid quota_week_start (must be RFC3339): " + err.Error()})
			return
		}
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
	// Build the detail as a JSON object via json.Marshal so values containing
	// quotes or other special characters are always correctly escaped (a raw
	// fmt.Sprintf concatenation could otherwise produce invalid JSON).
	auditDetailMap := map[string]interface{}{
		"username":       req.Username,
		"route_mode":     req.RouteMode,
		"fixed_provider": req.FixedProvider,
	}
	if req.FixedMultiplier != nil {
		auditDetailMap["fixed_multiplier"] = *req.FixedMultiplier
	} else {
		auditDetailMap["fixed_multiplier"] = nil
	}
	detailBytes, _ := json.Marshal(auditDetailMap)
	auditDetail := string(detailBytes)
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"user.create", "user", strconv.FormatInt(user.ID, 10), auditDetail, time.Now().Format(time.RFC3339),
	)

	// Return the sub-key in plaintext (only time)
	user.SubKeyPreview = actualSubKeyPreview
	response := map[string]any{
		"id":                      user.ID,
		"username":                user.Username,
		"sub_key":                 actualSubKey,
		"sub_key_preview":         actualSubKeyPreview,
		"quota_5h_limit":          user.Quota5hLimit,
		"quota_5h_used":           user.Quota5hUsed,
		"quota_total_limit":       user.QuotaTotalLimit,
		"quota_total_used":        user.QuotaTotalUsed,
		"quota_token_total_limit": req.QuotaTokenTotalLimit,
		"quota_token_total_used":  0,
		"quota_token_5h_limit":    req.QuotaToken5hLimit,
		"quota_token_5h_used":     0,
		"quota_token_week_limit":  req.QuotaTokenWeekLimit,
		"quota_token_week_used":   0,
		"route_mode":              user.RouteMode,
		"fixed_provider":          user.FixedProvider,
		"status":                  user.Status,
		"max_body_size":           user.MaxBodySize,
		"max_concurrency":         user.MaxConcurrency,
		"created_at":              user.CreatedAt,
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

	// Reject invalid count-quota values (audit L2 / F3):
	//   * negative -> meaningless; reject.
	//   * zero     -> UNLIMITED. Since 2026-07-21 a 0 count limit means
	//                 "call-count not restricted" and the gate opens
	//                 unconditionally (see internal/models/quota.go). This lets
	//                 an admin meter a user only by Token usage. We therefore
	//                 ACCEPT 0 here (mirroring the cumulative Token cap, where 0
	//                 also means unlimited). Only negative values are rejected.
	// The cumulative Token cap (quota_token_total_limit) is handled separately
	// below: there 0 means unlimited and is allowed.
	if req.Quota5hLimit != nil && *req.Quota5hLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_5h_limit must be >= 0 (0 = 不限制)"})
		return
	}
	if req.QuotaTotalLimit != nil && *req.QuotaTotalLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_total_limit must be >= 0 (0 = 不限制)"})
		return
	}
	if req.QuotaTokenTotalLimit != nil && *req.QuotaTokenTotalLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_total_limit must be >= 0"})
		return
	}
	if req.QuotaToken5hLimit != nil && *req.QuotaToken5hLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_5h_limit must be >= 0"})
		return
	}
	if req.QuotaTokenWeekLimit != nil && *req.QuotaTokenWeekLimit < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "quota_token_week_limit must be >= 0"})
		return
	}

	// Update quota limits
	if req.Quota5hLimit != nil || req.QuotaTotalLimit != nil {
		if err := models.UpdateQuotaLimits(h.DB, userID, req.Quota5hLimit, req.QuotaTotalLimit); err != nil {
			log.Printf("ERROR: update quota limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update quota"})
			return
		}
		response["quota_updated"] = true
	}

	// Update cumulative Token cap (nil = unchanged; 0 = unlimited).
	if req.QuotaTokenTotalLimit != nil {
		if err := models.UpdateQuotaTokenTotalLimit(h.DB, userID, *req.QuotaTokenTotalLimit); err != nil {
			log.Printf("ERROR: update token limit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update token limit"})
			return
		}
		if q, gerr := models.GetQuota(h.DB, userID); gerr == nil {
			response["quota_token_total_limit"] = q.QuotaTokenTotalLimit
			response["quota_token_total_used"] = q.QuotaTokenTotalUsed
		}
	}
	// Update 5h-window and weekly Token caps (nil = unchanged; 0 = unlimited).
	// Read the current caps first so a partial update (only one dimension set)
	// preserves the other dimension instead of resetting it to 0.
	if req.QuotaToken5hLimit != nil || req.QuotaTokenWeekLimit != nil {
		var cur *models.Quota
		if c, gerr := models.GetQuota(h.DB, userID); gerr == nil {
			cur = c
		}
		t5h, tw := 0, 0
		if cur != nil {
			t5h, tw = cur.QuotaToken5hLimit, cur.QuotaTokenWeekLimit
		}
		if req.QuotaToken5hLimit != nil {
			t5h = *req.QuotaToken5hLimit
		}
		if req.QuotaTokenWeekLimit != nil {
			tw = *req.QuotaTokenWeekLimit
		}
		if err := models.UpdateQuotaTokenWindowLimits(h.DB, userID, t5h, tw); err != nil {
			log.Printf("ERROR: update token window limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update token window limits"})
			return
		}
		if q, gerr := models.GetQuota(h.DB, userID); gerr == nil {
			response["quota_token_5h_limit"] = q.QuotaToken5hLimit
			response["quota_token_5h_used"] = q.QuotaToken5hUsed
			response["quota_token_week_limit"] = q.QuotaTokenWeekLimit
			response["quota_token_week_used"] = q.QuotaTokenWeekUsed
		}
	}

	// Update weekly quota start anchor (fixed 7-day phase). Per product decision,
	// writing a new anchor ALSO zeroes the current weekly Token usage
	// (SetQuotaWeekStart). "" means clear → fall back to now.
	if req.QuotaWeekStart != nil {
		if err := models.SetQuotaWeekStart(h.DB, userID, *req.QuotaWeekStart); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid quota_week_start (must be RFC3339): " + err.Error()})
			return
		}
		if q, gerr := models.GetQuota(h.DB, userID); gerr == nil {
			response["quota_week_start"] = q.WeekStart
			response["quota_token_week_used"] = q.QuotaTokenWeekUsed
		}
		detail, _ := json.Marshal(map[string]any{"user_id": userID, "week_start": *req.QuotaWeekStart})
		_, _ = h.DB.Exec(
			`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			"quota_week_start_set", "user", strconv.FormatInt(userID, 10), string(detail), time.Now().Format(time.RFC3339),
		)
		response["quota_week_start_set"] = true
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
		if v > models.MaxConcurrencyHardLimit {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "并发上限不能超过 200"})
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
		if *req.FixedMultiplier <= 0 {
			// A 0 (or negative) fixed multiplier is meaningless — treat it as a
			// request to CLEAR the multiplier back to the global default. This
			// makes the panel's "set to 0 = clear" behaviour actually take effect
			// instead of silently no-op'ing when the field is sent without the
			// dedicated clear flag (audit LOW: 固定倍率 0=清除 实际无效).
			if err := models.UpdateFixedMultiplier(h.DB, userID, nil); err != nil {
				log.Printf("ERROR: clear fixed multiplier (0): %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to clear fixed multiplier"})
				return
			}
			response["fixed_multiplier"] = nil
			routeChanged = true
		} else if *req.FixedMultiplier < 0.1 || *req.FixedMultiplier > 100.0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "fixed_multiplier must be between 0.1 and 100.0"})
			return
		} else {
			if err := models.UpdateFixedMultiplier(h.DB, userID, req.FixedMultiplier); err != nil {
				log.Printf("ERROR: update fixed multiplier: %v", err)
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update fixed multiplier"})
				return
			}
			response["fixed_multiplier"] = req.FixedMultiplier
			routeChanged = true
		}
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
		// Normalize the explicit until date at the boundary (accept RFC3339 or a
		// bare date; reject anything else — audit F1).
		norm, err := models.NormalizeExpiry(req.Until)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		newExpiresAt = norm
	case req.Days > 0:
		// Calculate from current expiry (or NOW if permanent).
		base := now
		if oldExpiresAt != "" {
			if parsed, ok := models.ParseExpiry(oldExpiresAt); ok {
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

	// Write audit log for the expiry extension.
	detail := fmt.Sprintf("expires_at: %s → %s", oldExpiresAt, newExpiresAt)
	if oldExpiresAt == "" {
		detail = fmt.Sprintf("expires_at: (permanent) → %s", newExpiresAt)
	}
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"extend", "user", strconv.FormatInt(userID, 10), detail, now.Format(time.RFC3339),
	)

	// If reset_token_stats is requested, zero all usage buckets (call-count +
	// Token, all three Token dimensions) and restart all window anchors.
	// An audit entry is written so the operation is traceable.
	if req.ResetTokenStats {
		if err := models.ResetQuotaUsage(h.DB, userID, now.Format(time.RFC3339)); err != nil {
			log.Printf("ERROR: reset quota usage for user %d: %v", userID, err)
		} else {
			auditDetail := `{"dimensions":["calls","tokens-5h","tokens-week","tokens-month"],"trigger":"extend","operator":"admin"}`
			_, _ = h.DB.Exec(
				`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
				 VALUES (?, ?, ?, ?, ?)`,
				"quota_reset", "user", strconv.FormatInt(userID, 10), auditDetail, now.Format(time.RFC3339),
			)
		}
	}

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

// ResetUsage handles POST /admin/api/users/{id}/reset-usage.
// Zeroes all usage buckets (call-count + Token) and restarts every window
// anchor. Does not touch the user's expiry date or any other field.
//
// An optional `?scope=` query parameter selects WHICH buckets to reset:
//   - "calls"  → only call-count usage (5h + total) and the 5h window anchor
//   - "tokens" → only Token usage (5h / week / month) and the Token window anchors
//   - "all" (default, also when absent) → everything (ResetQuotaUsage)
func (h *Handler) ResetUsage(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	userID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid user ID"})
		return
	}

	user, err := models.GetUserByID(h.DB, userID)
	if err != nil || user == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "User not found"})
		return
	}

	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = "all"
	}
	if scope != "all" && scope != "calls" && scope != "tokens" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "scope must be one of: all, calls, tokens"})
		return
	}

	now := time.Now().Format(time.RFC3339)
	var resetErr error
	var dimensions []string
	switch scope {
	case "calls":
		resetErr = models.ResetCallCount(h.DB, userID, now)
		dimensions = []string{"calls", "tokens-5h-window"}
	case "tokens":
		resetErr = models.ResetTokenStats(h.DB, userID, now)
		dimensions = []string{"tokens-5h", "tokens-week", "tokens-month"}
	default: // all
		resetErr = models.ResetQuotaUsage(h.DB, userID, now)
		dimensions = []string{"calls", "tokens-5h", "tokens-week", "tokens-month"}
	}
	if resetErr != nil {
		log.Printf("ERROR: reset quota usage for user %d: %v", userID, resetErr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to reset usage"})
		return
	}

	// Audit trail.
	dimJSON, _ := json.Marshal(dimensions)
	auditDetail := fmt.Sprintf(`{"dimensions":%s,"trigger":"manual_reset","operator":"admin"}`, string(dimJSON))
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"quota_reset", "user", strconv.FormatInt(userID, 10), auditDetail, now,
	)

	scopeLabel := map[string]string{"calls": "调用次数", "tokens": "Token 用量", "all": "全部用量"}[scope]
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "用户 " + user.Username + " 的「" + scopeLabel + "」已重置",
	})
}

// batchWeekStartRequest is the JSON body for POST /admin/api/users/batch-week-start.
type batchWeekStartRequest struct {
	UserIDs   []int64 `json:"user_ids"`
	WeekStart string  `json:"week_start"` // RFC3339 UTC; the client interprets local Asia/Shanghai before sending
}

// BatchSetWeekStart applies a fixed weekly quota start anchor to multiple users
// in one call. Per product decision each user's current weekly Token usage is
// also zeroed (models.SetQuotaWeekStart). Failures are per-user: succeeded
// users are NOT rolled back, and the response lists every user's ok/error so
// the admin sees exactly what happened.
func (h *Handler) BatchSetWeekStart(w http.ResponseWriter, r *http.Request) {
	var req batchWeekStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request body"})
		return
	}
	if len(req.UserIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user_ids is required"})
		return
	}
	// Validate the week_start format up front so a malformed value rejects the
	// whole batch instead of silently failing per-row.
	if _, err := time.Parse(time.RFC3339, req.WeekStart); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid week_start (must be RFC3339 UTC)"})
		return
	}

	type batchResult struct {
		ID    int64  `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	results := make([]batchResult, 0, len(req.UserIDs))
	succeeded := 0
	for _, id := range req.UserIDs {
		if err := models.SetQuotaWeekStart(h.DB, id, req.WeekStart); err != nil {
			results = append(results, batchResult{ID: id, OK: false, Error: err.Error()})
		} else {
			results = append(results, batchResult{ID: id, OK: true})
			succeeded++
		}
	}

	detail, _ := json.Marshal(map[string]any{
		"user_ids":   req.UserIDs,
		"week_start": req.WeekStart,
		"succeeded":  succeeded,
		"failed":     len(req.UserIDs) - succeeded,
	})
	_, _ = h.DB.Exec(
		`INSERT INTO audit_logs (action, target_type, target_id, detail, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"quota_week_start_batch", "user", "0", string(detail), time.Now().Format(time.RFC3339),
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"results":   results,
		"succeeded": succeeded,
		"failed":    len(req.UserIDs) - succeeded,
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

	// Best-effort: drop the in-process in-flight counter so it does not linger
	// (the gateway's concurrency map is otherwise append-only). In-flight
	// requests hold the old pointer and still decrement it on completion.
	proxy.ForgetConcurrency(userID)

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
