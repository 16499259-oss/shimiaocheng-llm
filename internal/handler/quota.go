// Package handler provides HTTP handlers for the public API endpoints.
package handler

import (
	"database/sql"
	"encoding/json"
	"log"
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
		Quota5hRemaining:    max(0, quotaRecord.Quota5hLimit-quotaRecord.Quota5hUsed),
		QuotaTotalLimit:     quotaRecord.QuotaTotalLimit,
		QuotaTotalUsed:      quotaRecord.QuotaTotalUsed,
		QuotaTotalRemaining: max(0, quotaRecord.QuotaTotalLimit-quotaRecord.QuotaTotalUsed),
		// Cumulative Token quota. When the cap is 0 (unlimited) the remaining
		// field is forced to 0 so the frontend treats it as "infinite" and hides
		// the progress bar. Otherwise it is clamped to >= 0: Token accounting
		// happens after the response is sent, so used may transiently exceed the
		// cap (a soft gate, accepted by design) and must never surface a negative
		// remaining (audit F2).
		QuotaTokenTotalLimit:     quotaRecord.QuotaTokenTotalLimit,
		QuotaTokenTotalUsed:      quotaRecord.QuotaTokenTotalUsed,
		QuotaTokenTotalRemaining: 0,
		// 5h-window Token quota. Like the cumulative cap, remaining is forced to
		// 0 when the cap is 0 (unlimited) so the frontend hides the progress bar.
		QuotaToken5hLimit:     quotaRecord.QuotaToken5hLimit,
		QuotaToken5hUsed:      quotaRecord.QuotaToken5hUsed,
		QuotaToken5hRemaining: 0,
		// Weekly (rolling 7d) Token quota, same semantics as above.
		QuotaTokenWeekLimit:     quotaRecord.QuotaTokenWeekLimit,
		QuotaTokenWeekUsed:      quotaRecord.QuotaTokenWeekUsed,
		QuotaTokenWeekRemaining: 0,
		WindowResetAt:           windowStart.Format(time.RFC3339),
		Status:                  user.Status,
		// Propagate the user's account expiry to the self-service panel so the
		// /user/ dashboard can show it (fix: user-expiry-display). An empty
		// string means "permanent" — the frontend renders that as 「永久」.
		ExpiresAt: user.ExpiresAt,
	}
	if quotaRecord.QuotaTokenTotalLimit > 0 {
		status.QuotaTokenTotalRemaining = max(0, quotaRecord.QuotaTokenTotalLimit-quotaRecord.QuotaTokenTotalUsed)
	}
	if quotaRecord.QuotaToken5hLimit > 0 {
		status.QuotaToken5hRemaining = max(0, quotaRecord.QuotaToken5hLimit-quotaRecord.QuotaToken5hUsed)
	}
	if quotaRecord.QuotaTokenWeekLimit > 0 {
		status.QuotaTokenWeekRemaining = max(0, quotaRecord.QuotaTokenWeekLimit-quotaRecord.QuotaTokenWeekUsed)
	}

	// Query token stats for this user. Scan errors are logged but not fatal: on
	// failure the counters stay 0 and the quota decision above is unaffected
	// (audit F6).
	var totalTokens, totalTokensToday int64
	today := time.Now().Format("2006-01-02")
	if err := h.DB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM call_logs WHERE user_id = ?`, userID).Scan(&totalTokens); err != nil {
		log.Printf("WARN: /v1/quota total tokens: %v", err)
	}
	if err := h.DB.QueryRow(`SELECT COALESCE(SUM(total_tokens), 0) FROM call_logs WHERE user_id = ? AND created_at >= ?`, userID, today).Scan(&totalTokensToday); err != nil {
		log.Printf("WARN: /v1/quota total tokens today: %v", err)
	}
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
