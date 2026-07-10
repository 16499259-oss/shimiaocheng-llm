package models

import (
	"database/sql"
	"fmt"
	"time"
)

// Quota represents a user's quota record.
type Quota struct {
	ID              int64  `json:"id"`
	UserID          int64  `json:"user_id"`
	Quota5hLimit    int    `json:"quota_5h_limit"`
	Quota5hUsed     int    `json:"quota_5h_used"`
	QuotaTotalLimit int    `json:"quota_total_limit"`
	QuotaTotalUsed  int    `json:"quota_total_used"`
	WindowStart     string `json:"window_start"`
	UpdatedAt       string `json:"updated_at"`
}

// QuotaStatus is returned by the /v1/quota endpoint.
type QuotaStatus struct {
	Quota5hLimit      int    `json:"quota_5h_limit"`
	Quota5hUsed       int    `json:"quota_5h_used"`
	Quota5hRemaining  int    `json:"quota_5h_remaining"`
	QuotaTotalLimit   int    `json:"quota_total_limit"`
	QuotaTotalUsed    int    `json:"quota_total_used"`
	QuotaTotalRemaining int  `json:"quota_total_remaining"`
	TotalTokens         int64 `json:"total_tokens"`
	TotalTokensToday    int64 `json:"total_tokens_today"`
	WindowResetAt     string `json:"window_reset_at"`
	Status            string `json:"status"`
}

// GetQuota retrieves the quota record for a user.
func GetQuota(db *sql.DB, userID int64) (*Quota, error) {
	q := &Quota{}
	err := db.QueryRow(
		`SELECT id, user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used, window_start, updated_at
		 FROM quotas WHERE user_id = ?`, userID,
	).Scan(&q.ID, &q.UserID, &q.Quota5hLimit, &q.Quota5hUsed, &q.QuotaTotalLimit, &q.QuotaTotalUsed, &q.WindowStart, &q.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get quota: %w", err)
	}
	return q, nil
}

// AtomicDeductQuota atomically deducts effective_calls from both 5h and total quotas.
// Returns true if the deduction succeeded, false if quota was insufficient.
func AtomicDeductQuota(db *sql.DB, userID int64, effectiveCalls int) (bool, error) {
	now := time.Now().Format(time.RFC3339)
	result, err := db.Exec(
		`UPDATE quotas
		 SET quota_5h_used = quota_5h_used + ?,
		     quota_total_used = quota_total_used + ?,
		     updated_at = ?
		 WHERE user_id = ?
		   AND quota_5h_used + ? <= quota_5h_limit
		   AND quota_total_used + ? <= quota_total_limit`,
		effectiveCalls, effectiveCalls, now,
		userID,
		effectiveCalls, effectiveCalls,
	)
	if err != nil {
		return false, fmt.Errorf("atomic deduct quota: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected: %w", err)
	}

	return rowsAffected == 1, nil
}

// UpdateQuotaLimits updates a user's quota limits.
func UpdateQuotaLimits(db *sql.DB, userID int64, quota5hLimit, quotaTotalLimit *int) error {
	now := time.Now().Format(time.RFC3339)

	if quota5hLimit != nil && quotaTotalLimit != nil {
		_, err := db.Exec(
			`UPDATE quotas SET quota_5h_limit = ?, quota_total_limit = ?, updated_at = ? WHERE user_id = ?`,
			*quota5hLimit, *quotaTotalLimit, now, userID,
		)
		return err
	}
	if quota5hLimit != nil {
		_, err := db.Exec(
			`UPDATE quotas SET quota_5h_limit = ?, updated_at = ? WHERE user_id = ?`,
			*quota5hLimit, now, userID,
		)
		return err
	}
	if quotaTotalLimit != nil {
		_, err := db.Exec(
			`UPDATE quotas SET quota_total_limit = ?, updated_at = ? WHERE user_id = ?`,
			*quotaTotalLimit, now, userID,
		)
		return err
	}
	return nil
}

// Reset5hQuota resets all users' 5h quota to 0 and updates window_start.
func Reset5hQuota(db *sql.DB, resetIntervalHours int) error {
	now := time.Now()
	windowIndex := now.Hour() / resetIntervalHours
	windowHour := windowIndex * resetIntervalHours
	windowStart := time.Date(now.Year(), now.Month(), now.Day(), windowHour, 0, 0, 0, now.Location())

	_, err := db.Exec(
		`UPDATE quotas SET quota_5h_used = 0, window_start = ?, updated_at = ?`,
		windowStart.Format(time.RFC3339), now.Format(time.RFC3339),
	)
	return err
}

// CompensateQuotaReset resets quota for users whose window_start is before the current window.
func CompensateQuotaReset(db *sql.DB, resetIntervalHours int) error {
	now := time.Now()
	windowIndex := now.Hour() / resetIntervalHours
	windowHour := windowIndex * resetIntervalHours
	currentWindowStart := time.Date(now.Year(), now.Month(), now.Day(), windowHour, 0, 0, 0, now.Location())

	_, err := db.Exec(
		`UPDATE quotas SET quota_5h_used = 0, window_start = ?, updated_at = ?
		 WHERE window_start < ?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
		currentWindowStart.Format(time.RFC3339),
	)
	return err
}
