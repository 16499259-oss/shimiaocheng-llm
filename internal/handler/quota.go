// Package handler provides HTTP handlers for the public API endpoints.
package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// QuotaHandler handles the /v1/quota endpoint for user self-service quota queries.
type QuotaHandler struct {
	DB            *sql.DB
	MultEng       *quota.MultiplierEngine
	ResetInterval int
}

// ServeHTTP handles GET /v1/quota.
func (h *QuotaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeQuotaError(w, http.StatusMethodNotAllowed, "Method not allowed", "method_not_allowed")
		return
	}

	userID := auth.GetUserID(r)
	if userID == 0 {
		writeQuotaError(w, http.StatusUnauthorized, "Not authenticated", "not_authenticated")
		return
	}

	// Get user info
	user, err := models.GetUserByID(h.DB, userID)
	if err != nil || user == nil {
		writeQuotaError(w, http.StatusInternalServerError, "Failed to get user info", "internal_error")
		return
	}

	// Get quota info
	quotaRecord, err := models.GetQuota(h.DB, userID)
	if err != nil || quotaRecord == nil {
		writeQuotaError(w, http.StatusInternalServerError, "Failed to get quota info", "internal_error")
		return
	}

	// Calculate next window reset time
	windowStart, err := time.Parse(time.RFC3339, quotaRecord.WindowStart)
	if err != nil {
		// Fallback: compute from now
		now := time.Now()
		windowIndex := now.Hour() / h.ResetInterval
		windowHour := (windowIndex + 1) * h.ResetInterval
		if windowHour >= 24 {
			windowHour = 0
			now = now.AddDate(0, 0, 1)
		}
		windowStart = time.Date(now.Year(), now.Month(), now.Day(), windowHour, 0, 0, 0, now.Location())
	} else {
		windowStart = windowStart.Add(time.Duration(h.ResetInterval) * time.Hour)
	}

	status := models.QuotaStatus{
		Quota5hLimit:        quotaRecord.Quota5hLimit,
		Quota5hUsed:         quotaRecord.Quota5hUsed,
		Quota5hRemaining:    quotaRecord.Quota5hLimit - quotaRecord.Quota5hUsed,
		QuotaTotalLimit:     quotaRecord.QuotaTotalLimit,
		QuotaTotalUsed:      quotaRecord.QuotaTotalUsed,
		QuotaTotalRemaining: quotaRecord.QuotaTotalLimit - quotaRecord.QuotaTotalUsed,
		// Cumulative Token quota. When the cap is 0 (unlimited) the remaining
		// field is forced to 0 so the frontend treats it as "infinite" and hides
		// the progress bar; otherwise it is limit - used.
		QuotaTokenTotalLimit:     quotaRecord.QuotaTokenTotalLimit,
		QuotaTokenTotalUsed:      quotaRecord.QuotaTokenTotalUsed,
		QuotaTokenTotalRemaining: 0,
		WindowResetAt:            windowStart.Format(time.RFC3339),
		Status:                   user.Status,
	}
	if quotaRecord.QuotaTokenTotalLimit > 0 {
		status.QuotaTokenTotalRemaining = quotaRecord.QuotaTokenTotalLimit - quotaRecord.QuotaTokenTotalUsed
	}

	// Query token stats for this user
	var totalTokens, totalTokensToday int64
	today := time.Now().Format("2006-01-02")
	h.DB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM call_logs WHERE user_id = ?`, userID).Scan(&totalTokens)
	h.DB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM call_logs WHERE user_id = ? AND created_at >= ?`, userID, today).Scan(&totalTokensToday)
	status.TotalTokens = totalTokens
	status.TotalTokensToday = totalTokensToday

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func writeQuotaError(w http.ResponseWriter, statusCode int, message, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
			"code":    errType,
		},
	})
}
