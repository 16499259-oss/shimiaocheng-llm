// Package handler provides HTTP handlers for the public API endpoints.
package handler

import (
	"database/sql"
	"encoding/json"
	"log"
	"math"
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
		MonthResetAt:            computeMonthResetAt(quotaRecord.MonthStart),
		WeekResetAt:             computeWeekResetAt(quotaRecord.WeekStart),
		WeekStart:               quotaRecord.WeekStart,
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
	//
	// IMPORTANT: the cumulative / today counters are recomputed from the RAW
	// call_logs rows, each multiplied by that row's own multiplier_used (then
	// ceiled). This makes the displayed Token consumption match the
	// multiplier-inflated value the quota columns already bill — see the
	// user-panel display fix. call_logs.total_tokens is the AUDIT (raw,
	// unmultiplied) value and is intentionally NOT used here.
	var totalTokens, totalTokensToday int64
	today := time.Now().Format("2006-01-02")
	if totalTokens, err = sumMultipliedTokens(h.DB, userID, ""); err != nil {
		log.Printf("WARN: /v1/quota total tokens: %v", err)
	}
	if totalTokensToday, err = sumMultipliedTokens(h.DB, userID, today); err != nil {
		log.Printf("WARN: /v1/quota total tokens today: %v", err)
	}
	status.TotalTokens = totalTokens
	status.TotalTokensToday = totalTokensToday

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// computeMonthResetAt derives the rolling-30-day Token bucket's next reset
// time from the stored month_start anchor (RFC3339). It returns "" when the
// anchor is empty (e.g. a legacy row whose month_start was not yet set),
// so the frontend can simply hide the field. The +30d offset mirrors the
// gate's month-window cutoff (now-30d) in AtomicDeductQuota, keeping the
// displayed "next reset" consistent with the lazy-reset boundary.
func computeMonthResetAt(monthStart string) string {
	if monthStart == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, monthStart)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, 30).Format(time.RFC3339)
}

// computeWeekResetAt derives the next reset time of the fixed-phase 7-day Token
// bucket from the stored week_start anchor (RFC3339). It mirrors
// computeMonthResetAt: an empty anchor yields "" so the frontend can hide the
// field. The week_start anchor only moves when the admin changes it, so the
// next reset is the start of the 7-day cycle containing now plus 7 days — the
// same boundary the gate uses to lazily zero quota_token_week_used. Reusing
// models.AlignedCycleStartUTC keeps the displayed "next reset" perfectly in
// sync with the actual cycle boundary.
func computeWeekResetAt(weekStart string) string {
	if weekStart == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, weekStart)
	if err != nil {
		return ""
	}
	cycleStart := models.AlignedCycleStartUTC(t, time.Now().UTC())
	return cycleStart.Add(7 * 24 * time.Hour).Format(time.RFC3339)
}

// sumMultipliedTokens recomputes the multiplier-inflated Token consumption for a
// user directly from the RAW call_logs rows. Each row stores its own
// multiplier_used (the rate actually in effect at call time, defaulting to 1.0
// for pre-multiplier history), so no migration or zero-guard is needed — every
// historical row is reproducible. The per-row value is ceil((prompt_tokens +
// completion_tokens) * multiplier_used); we sum in Go because modernc.org/sqlite
// does not guarantee a CEIL math function.
//
// When since is non-empty it is applied as a "created_at >= since" filter (the
// handler passes the local "2006-01-02" date string to isolate "today"). The
// comparison is a plain text comparison on RFC3339/timestamp strings, which is
// well-ordered for the SH-normalized timestamps the gateway writes.
func sumMultipliedTokens(db *sql.DB, userID int64, since string) (int64, error) {
	q := `SELECT prompt_tokens, completion_tokens, multiplier_used FROM call_logs WHERE user_id = ?`
	args := []interface{}{userID}
	if since != "" {
		q += ` AND created_at >= ?`
		args = append(args, since)
	}
	rows, err := db.Query(q, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var total int64
	for rows.Next() {
		var p, c int
		var m float64
		if err := rows.Scan(&p, &c, &m); err != nil {
			return 0, err
		}
		total += int64(math.Ceil(float64(p+c) * m))
	}
	return total, rows.Err()
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
