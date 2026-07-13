package admin

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/timeutil"
)

// createUserRequest is the JSON body for POST /admin/api/users.
type createUserRequest struct {
	Username        string `json:"username"`
	Quota5hLimit    int    `json:"quota_5h_limit"`
	QuotaTotalLimit int    `json:"quota_total_limit"`
	ExpiresAt       string `json:"expires_at"`
}

// updateUserRequest is the JSON body for PUT /admin/api/users/{id}.
type updateUserRequest struct {
	Quota5hLimit    *int    `json:"quota_5h_limit"`
	QuotaTotalLimit *int    `json:"quota_total_limit"`
	Status          *string `json:"status"`
	RegenerateKey   *bool   `json:"regenerate_key"`
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

	// Generate sub-key
	subKey := auth.GenerateSubKey(h.SubKeySalt, 0) // userID is not yet known, use 0 as placeholder
	// Regenerate with a proper approach: generate, then we'll store
	subKeyHash := auth.HashSubKey(subKey)
	subKeyPreview := auth.SubKeyPreview(subKey)

	// Users created via admin have no password (only admin accounts have passwords)
	emptyPassHash := "$2a$10$placeholder" // This is just a placeholder; user accounts use sub-keys

	user, err := models.CreateUser(
		h.DB, req.Username, emptyPassHash, subKeyHash, subKeyPreview,
		"user", "active", req.ExpiresAt, req.Quota5hLimit, req.QuotaTotalLimit,
	)
	if err != nil {
		log.Printf("ERROR: create user: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to create user: " + err.Error()})
		return
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
		"status":            user.Status,
		"created_at":        user.CreatedAt,
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

	// Update quota limits
	if req.Quota5hLimit != nil || req.QuotaTotalLimit != nil {
		if err := models.UpdateQuotaLimits(h.DB, userID, req.Quota5hLimit, req.QuotaTotalLimit); err != nil {
			log.Printf("ERROR: update quota limits: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Failed to update quota"})
			return
		}
		response["quota_updated"] = true
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
