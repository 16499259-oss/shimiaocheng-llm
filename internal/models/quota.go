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
	QuotaToken5hLimit    int             `json:"quota_token_5h_limit"`    // 0 = unlimited (Token cap within 5h window)
	QuotaToken5hUsed     int             `json:"quota_token_5h_used"`     // Token used within current 5h window
	QuotaTokenWeekLimit  int             `json:"quota_token_week_limit"`  // 0 = unlimited (Token cap within rolling 7d)
	QuotaTokenWeekUsed   int             `json:"quota_token_week_used"`   // Token used within rolling 7d bucket
	WeekStart            string          `json:"week_start"`              // fixed phase anchor for the 7-day weekly Token bucket (admin-settable; never bumped by the gate)
	MonthStart           string          `json:"month_start"`             // rolling-30d (month) Token bucket anchor
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
	QuotaToken5hLimit        int    `json:"quota_token_5h_limit"`        // 0 = unlimited
	QuotaToken5hUsed         int    `json:"quota_token_5h_used"`         // used within current 5h window
	QuotaToken5hRemaining    int    `json:"quota_token_5h_remaining"`    // limit>0 ? max(0,limit-used) : 0
	QuotaTokenWeekLimit      int    `json:"quota_token_week_limit"`      // 0 = unlimited
	QuotaTokenWeekUsed       int    `json:"quota_token_week_used"`       // used within rolling 7d bucket
	QuotaTokenWeekRemaining  int    `json:"quota_token_week_remaining"`  // limit>0 ? max(0,limit-used) : 0
	TotalTokens              int64  `json:"total_tokens"`
	TotalTokensToday         int64  `json:"total_tokens_today"`
	WindowResetAt            string `json:"window_reset_at"`
	MonthResetAt             string `json:"month_reset_at"` // month_start + 30d (rolling-30d Token bucket next reset)
	Status                   string `json:"status"`
	ExpiresAt                string `json:"expires_at"` // user account expiry; "" means permanent (no expiry)
}

// GetQuota retrieves the quota record for a user.
func GetQuota(db *sql.DB, userID int64) (*Quota, error) {
	q := &Quota{}
	err := db.QueryRow(
		`SELECT id, user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used,
		        quota_token_total_limit, quota_token_total_used,
		        quota_token_5h_limit, quota_token_5h_used, quota_token_week_limit, quota_token_week_used, week_start,
		        month_start, window_start, updated_at, fixed_multiplier
		 FROM quotas WHERE user_id = ?`, userID,
	).Scan(&q.ID, &q.UserID, &q.Quota5hLimit, &q.Quota5hUsed, &q.QuotaTotalLimit, &q.QuotaTotalUsed,
		&q.QuotaTokenTotalLimit, &q.QuotaTokenTotalUsed,
		&q.QuotaToken5hLimit, &q.QuotaToken5hUsed, &q.QuotaTokenWeekLimit, &q.QuotaTokenWeekUsed, &q.WeekStart,
		&q.MonthStart, &q.WindowStart, &q.UpdatedAt, &q.FixedMultiplier)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get quota: %w", err)
	}
	return q, nil
}

// AtomicDeductQuota atomically deducts effective_calls from both 5h and total
// quotas, and also gates on the cumulative / 5h-window / weekly Token limits.
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
//   - 5h-window / weekly Token caps (quota_token_5h_limit / quota_token_week_limit):
//     also 0 = unlimited, using the same "(limit = 0 OR used < limit)" idiom.
//
// Token soft gate and the multiplier deviation (audit L1):
// The Token gate is a pure column comparison "quota_token_total_used <
// quota_token_total_limit" (and its 5h / weekly analogues). The billed Token
// counter is multiplier-scaled (AddTokenUsage stores ceil((prompt+completion)*
// multiplier)), but the actual token counts are only known AFTER the upstream
// responds — so the gate cannot look ahead by the request's billed increment.
// Consequently a single request may push used past the cap by up to one billed
// increment, and the next request is then blocked. This overage widens at higher
// multipliers and is accepted by design (a soft gate, consistent with Token
// accounting happening after the response is sent). Tightening it would require
// the token estimate at request time, which is unavailable without a tokenizer,
// so the logic is left as-is; handler/quota.go clamps the reported remaining to
// >= 0 so the overage never surfaces as negative (audit F2).
//
// CRITICAL (主理人齐活林纠正): the Token columns (total / 5h / weekly) are
// NEVER accumulated inside this gate's SET clause. The gate only performs the
// atomic checks and, for the weekly bucket, a lazy "cycle changed → reset used
// to 0 + re-anchor week_cycle_start to the new 7-day cycle (aligned to the
// fixed admin-set anchor week_start)" — see AtomicDeductQuota's UPDATE below
// (CASE WHEN week_cycle_start <> newCycle). The old rolling behaviour that
// bumped week_start to now is gone: week_start is now a FIXED phase anchor and
// is never modified by the gate. All Token accounting is deferred to
// AddTokenUsage, which runs after a successful response. Accruing
// effectiveCalls (a CALL count) into a Token column would be a category error
// and over-count Token usage.
//
// alignedCycleStartUTC returns the start of the 7-day cycle containing now,
// given a fixed phase anchor weekStart (both interpreted as UTC instants).
// If now is before weekStart, the cycle starts at weekStart itself. This makes
// the weekly bucket recur every 7 days from the admin-set anchor.
func alignedCycleStartUTC(weekStart, now time.Time) time.Time {
	if now.Before(weekStart) {
		return weekStart
	}
	k := int64(now.Sub(weekStart) / (7 * 24 * time.Hour))
	return weekStart.Add(time.Duration(k) * 7 * 24 * time.Hour)
}

// SetQuotaWeekStart writes the fixed phase anchor week_start (RFC3339 UTC; ""
// means clear → use now). It re-anchors week_cycle_start to the cycle containing
// now so the 7-day phase shifts, but it does NOT touch quota_token_week_used:
// the in-progress weekly usage carries over and is naturally reset to 0 only
// when the new cycle boundary is crossed (see AtomicDeductQuota's
// CASE WHEN week_cycle_start <> newCycle). Clearing usage on every anchor change
// was a hidden side-effect that surprised admins; the cycle boundary is the
// single source of truth for resets.
func SetQuotaWeekStart(db *sql.DB, userID int64, startRFC3339 string) error {
	var t time.Time
	var err error
	if startRFC3339 == "" {
		t = time.Now().UTC()
	} else {
		t, err = time.Parse(time.RFC3339, startRFC3339)
		if err != nil {
			return fmt.Errorf("invalid week_start %q: %w", startRFC3339, err)
		}
	}
	t = t.UTC()
	cycleStart := alignedCycleStartUTC(t, time.Now().UTC())
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = db.Exec(
		`UPDATE quotas SET week_start = ?, week_cycle_start = ?, updated_at = ? WHERE user_id = ?`,
		t.Format(time.RFC3339), cycleStart.Format(time.RFC3339), now, userID,
	)
	if err != nil {
		return fmt.Errorf("set quota week start: %w", err)
	}
	return nil
}

// Returns true if the deduction succeeded, false if quota was insufficient.
func AtomicDeductQuota(db *sql.DB, userID int64, effectiveCalls int) (bool, error) {
	// Weekly bucket (fixed phase anchor): week_start is the admin-set anchor and
	// is NEVER bumped by the gate. We compute the start of the 7-day cycle
	// containing now from that anchor and compare it against week_cycle_start
	// (the cycle the accumulated quota_token_week_used belongs to). When they
	// differ, the gate resets the weekly Token usage to 0 and re-anchors
	// week_cycle_start to the new cycle — the bucket recurs every 7 days from the
	// anchor instead of the old rolling "bump to now" behaviour. String ordering
	// on RFC3339 timestamps equals chronological ordering, so week_cycle_start <>
	// newCycle is well-defined for both the migration default and UTC values.
	monthCutoff := time.Now().Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	nowTime := time.Now().Format(time.RFC3339)

	// Read the fixed phase anchor to compute the current 7-day cycle start.
	// week_start is changed only by the admin (SetQuotaWeekStart), so there is
	// no race between this read and the UPDATE below; a stale read simply
	// self-heals on the next request.
	var weekStartStr string
	if err := db.QueryRow(`SELECT week_start FROM quotas WHERE user_id = ?`, userID).Scan(&weekStartStr); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("read week_start: %w", err)
	}
	nowUTC := time.Now().UTC()
	weekStart := nowUTC
	if parsed, perr := time.Parse(time.RFC3339, weekStartStr); perr == nil && !parsed.IsZero() {
		weekStart = parsed
	}
	newCycle := alignedCycleStartUTC(weekStart, nowUTC).Format(time.RFC3339)

	result, err := db.Exec(
		`UPDATE quotas
		 SET quota_5h_used = quota_5h_used + ?,
		     quota_total_used = quota_total_used + ?,
		     quota_token_week_used = CASE WHEN week_cycle_start <> ? THEN 0 ELSE quota_token_week_used END,
		     week_cycle_start = CASE WHEN week_cycle_start <> ? THEN ? ELSE week_cycle_start END,
		     quota_token_total_used = CASE WHEN month_start < ? THEN 0 ELSE quota_token_total_used END,
		     month_start = CASE WHEN month_start < ? THEN ? ELSE month_start END,
		     updated_at = ?
		 WHERE user_id = ?
		   AND quota_5h_used + ? <= quota_5h_limit
		   AND quota_total_used + ? <= quota_total_limit
		   AND (quota_token_total_limit = 0 OR (CASE WHEN month_start < ? THEN 0 ELSE quota_token_total_used END) < quota_token_total_limit)
		   AND (quota_token_5h_limit = 0 OR quota_token_5h_used < quota_token_5h_limit)
		   AND (quota_token_week_limit = 0 OR (CASE WHEN week_cycle_start <> ? THEN 0 ELSE quota_token_week_used END) < quota_token_week_limit)`,
		effectiveCalls, effectiveCalls, // SET quota_5h_used, quota_total_used
		newCycle,            // SET quota_token_week_used CASE — reset判定
		newCycle, newCycle,  // SET week_cycle_start CASE — reset判定 + 新周期起点
		monthCutoff,         // SET quota_token_total_used CASE — reset判定
		monthCutoff, nowTime, // SET month_start CASE — reset判定 + bump 值
		nowTime,            // SET updated_at
		userID,             // WHERE user_id
		effectiveCalls, effectiveCalls, // WHERE 次数闸门 (5h / total)
		monthCutoff,        // WHERE 月 token 闸门 CASE 判定
		newCycle,           // WHERE 周 token 闸门 CASE 判定
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
		`UPDATE quotas
		 SET quota_token_total_used = quota_token_total_used + ?,
		     quota_token_5h_used = quota_token_5h_used + ?,
		     quota_token_week_used = quota_token_week_used + ?
		 WHERE user_id = ?`,
		delta, delta, delta, userID,
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

// UpdateQuotaTokenWindowLimits sets the user's 5h-window and weekly rolling
// Token caps in a single statement. A limit of 0 (the default) means unlimited
// for each dimension. Like UpdateQuotaTokenTotalLimit, this does NOT reset the
// already-accumulated usage — that is intentional: lowering a cap below current
// usage takes effect on the next request (self-consistent, the next request is
// blocked until usage decreases via the 5h reset / weekly lazy reset).
func UpdateQuotaTokenWindowLimits(db *sql.DB, userID int64, token5hLimit, tokenWeekLimit int) error {
	now := time.Now().Format(time.RFC3339)
	_, err := db.Exec(
		`UPDATE quotas SET quota_token_5h_limit = ?, quota_token_week_limit = ?, updated_at = ? WHERE user_id = ?`,
		token5hLimit, tokenWeekLimit, now, userID,
	)
	if err != nil {
		return fmt.Errorf("update quota token window limits: %w", err)
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
		`UPDATE quotas SET quota_5h_used = 0, quota_token_5h_used = 0, window_start = ?, updated_at = ?`,
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
		`UPDATE quotas SET quota_5h_used = 0, quota_token_5h_used = 0, window_start = ?, updated_at = ?
		 WHERE window_start < ?`,
		now.Format(time.RFC3339), now.Format(time.RFC3339),
		currentWindowStart.Format(time.RFC3339),
	)
	return err
}

// ResetQuotaUsage zeroes ALL usage buckets — call-count (5h + total) and
// Token (5h / week / month) — and restarts every window anchor (window_start,
// week_start, month_start) to now. This gives the user a clean "full revive":
// every quota gate re-opens immediately on the next request.
//
// The 5h Token bucket reuses the shared 5h window (window_start) per PR #27;
// the gate does NOT lazy-reset the 5h Token bucket (unlike week / month), so
// it relies on the cron Reset5hQuota / CompensateQuotaReset to clear
// quota_token_5h_used every 5h. Bumping window_start here restarts that
// shared window for both call-count and Token 5h buckets.
//
// A single atomic UPDATE — idempotent and safe to call repeatedly. now is an
// RFC3339 local-time string; when empty, the local now is used as a fallback.
func ResetQuotaUsage(db *sql.DB, userID int64, now string) error {
	if now == "" {
		now = time.Now().Format(time.RFC3339)
	}
	_, err := db.Exec(
		`UPDATE quotas
		 SET quota_5h_used = 0,
		     quota_total_used = 0,
		     window_start = ?,
		     quota_token_5h_used = 0,
		     quota_token_week_used = 0,
		     quota_token_total_used = 0,
		     week_start = ?,
		     week_cycle_start = ?,
		     month_start = ?,
		     updated_at = ?
		 WHERE user_id = ?`,
		now, now, now, now, now, userID,
	)
	if err != nil {
		return fmt.Errorf("reset quota usage: %w", err)
	}
	return nil
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
