package models

import (
	"database/sql"
	"fmt"
	"time"
)

// Quota represents a user's quota record.
type Quota struct {
	ID                   int64           `json:"id"`
	UserID               int64           `json:"user_id"`
	Quota5hLimit         int             `json:"quota_5h_limit"`
	Quota5hUsed          int             `json:"quota_5h_used"`
	QuotaTotalLimit      int             `json:"quota_total_limit"`
	QuotaTotalUsed       int             `json:"quota_total_used"`
	QuotaTokenTotalLimit int             `json:"quota_token_total_limit"` // 0 = unlimited (no Token cap)
	QuotaTokenTotalUsed  int             `json:"quota_token_total_used"`  // cumulative Token usage (prompt+completion)
	WindowStart          string          `json:"window_start"`
	UpdatedAt            string          `json:"updated_at"`
	FixedMultiplier      sql.NullFloat64 `json:"fixed_multiplier"`
}

// QuotaStatus is returned by the /v1/quota endpoint.
type QuotaStatus struct {
	Quota5hLimit             int    `json:"quota_5h_limit"`
	Quota5hUsed              int    `json:"quota_5h_used"`
	Quota5hRemaining         int    `json:"quota_5h_remaining"`
	QuotaTotalLimit          int    `json:"quota_total_limit"`
	QuotaTotalUsed           int    `json:"quota_total_used"`
	QuotaTotalRemaining      int    `json:"quota_total_remaining"`
	QuotaTokenTotalLimit     int    `json:"quota_token_total_limit"`     // 0 = unlimited
	QuotaTokenTotalUsed      int    `json:"quota_token_total_used"`      // cumulative used
	QuotaTokenTotalRemaining int    `json:"quota_token_total_remaining"` // 0 when unlimited (frontend treats as infinite)
	TotalTokens              int64  `json:"total_tokens"`
	TotalTokensToday         int64  `json:"total_tokens_today"`
	WindowResetAt            string `json:"window_reset_at"`
	Status                   string `json:"status"`
}

// GetQuota retrieves the quota record for a user.
func GetQuota(db *sql.DB, userID int64) (*Quota, error) {
	q := &Quota{}
	err := db.QueryRow(
		`SELECT id, user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used,
		        quota_token_total_limit, quota_token_total_used, window_start, updated_at, fixed_multiplier
		 FROM quotas WHERE user_id = ?`, userID,
	).Scan(&q.ID, &q.UserID, &q.Quota5hLimit, &q.Quota5hUsed, &q.QuotaTotalLimit, &q.QuotaTotalUsed,
		&q.QuotaTokenTotalLimit, &q.QuotaTokenTotalUsed, &q.WindowStart, &q.UpdatedAt, &q.FixedMultiplier)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get quota: %w", err)
	}
	return q, nil
}

// AtomicDeductQuota atomically deducts effective_calls from both 5h and total
// quotas, and also gates on the cumulative Token limit.
//
// Zero-means semantics (audit L2):
//   - Count quotas (quota_5h_limit / quota_total_limit): 0 is NOT a valid value.
//     The gate treats a 0 count limit as "already exhausted" (used + calls <= 0
//     can never hold for calls >= 1), so a 0 would silently lock the user out.
//     The admin edit API therefore REJECTS a count quota of 0 with 400 (see
//     internal/admin/users.go) and never stores it; any legacy row that still
//     holds 0 is likewise blocked here. This is intentionally different from the
//     cumulative Token cap, where 0 means "unlimited".
//   - Cumulative Token cap (quota_token_total_limit): 0 means unlimited, so the
//     gate opens unconditionally for that dimension ("(limit = 0 OR used < limit)").
//
// Token soft gate and the multiplier deviation (audit L1):
// The Token gate is a pure column comparison "quota_token_total_used <
// quota_token_total_limit". The billed Token counter is multiplier-scaled
// (AddTokenUsage stores ceil((prompt+completion)*multiplier)), but the actual
// token counts are only known AFTER the upstream responds — so the gate cannot
// look ahead by the request's billed increment. Consequently a single request
// may push used past the cap by up to one billed increment, and the next request
// is then blocked. This overage widens at higher multipliers and is accepted by
// design (a soft gate, consistent with Token accounting happening after the
// response is sent). Tightening it would require the token estimate at request
// time, which is unavailable without a tokenizer, so the logic is left as-is;
// handler/quota.go clamps the reported remaining to >= 0 so the overage never
// surfaces as negative (audit F2).
//
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
		   AND quota_total_used + ? <= quota_total_limit
		   AND (quota_token_total_limit = 0 OR quota_token_total_used < quota_token_total_limit)`,
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

// AddTokenUsage accumulates Token usage (prompt_tokens + completion_tokens) for
// a user AFTER a successful response is sent. It is a fire-and-forget accounting
// step: a delta <= 0 is a no-op, and a non-nil error is the caller's
// responsibility to log (never fatal to the request). The 5h/total deduction is
// handled separately by AtomicDeductQuota.
func AddTokenUsage(db *sql.DB, userID int64, delta int) error {
	if delta <= 0 {
		return nil
	}
	_, err := db.Exec(
		`UPDATE quotas SET quota_token_total_used = quota_token_total_used + ? WHERE user_id = ?`,
		delta, userID,
	)
	if err != nil {
		return fmt.Errorf("add token usage: %w", err)
	}
	return nil
}

// UpdateQuotaTokenTotalLimit sets the user's cumulative Token cap. A limit of 0
// (the default) means unlimited. This does not reset the already-accumulated
// usage, so lowering the limit below current usage takes effect on the next
// request (self-consistent: the next request is blocked until usage decreases).
func UpdateQuotaTokenTotalLimit(db *sql.DB, userID int64, limit int) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE quotas SET quota_token_total_limit = ?, updated_at = ? WHERE user_id = ?`,
		limit, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update quota token total limit: %w", err)
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

// GetFixedMultiplier returns the fixed_multiplier for a user from the quotas table.
// Returns sql.NullFloat64 where Valid=false means no per-user override is set
// (caller should fall back to the global time-based multiplier).
func GetFixedMultiplier(db *sql.DB, userID int64) (sql.NullFloat64, error) {
	var result sql.NullFloat64
	err := db.QueryRow(
		`SELECT fixed_multiplier FROM quotas WHERE user_id = ?`, userID,
	).Scan(&result)
	if err == sql.ErrNoRows {
		return sql.NullFloat64{}, nil
	}
	if err != nil {
		return sql.NullFloat64{}, fmt.Errorf("get fixed multiplier: %w", err)
	}
	return result, nil
}

// UpdateFixedMultiplier updates a user's fixed_multiplier in the quotas table.
// Pass nil to clear the override (reset to global).
func UpdateFixedMultiplier(db *sql.DB, userID int64, multiplier *float64) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE quotas SET fixed_multiplier = ?, updated_at = ? WHERE user_id = ?`,
		multiplier, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update fixed multiplier: %w", err)
	}
	return nil
}
