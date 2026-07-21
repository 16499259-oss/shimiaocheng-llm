// Package models_test contains ADDITIONAL coverage tests for the two quota
// features under QA review:
//   - Feature A: cumulative Token total limit (3rd, OR-combined dimension)
//   - Feature B: call-count limit (5h window + cumulative total, with multiplier)
//
// These tests target coverage gaps left by quota_test.go / quota_token_test.go:
//   - total-window exhaustion blocks while 5h has head-room (and vice-versa is
//     already covered by TestAtomicDeductQuota_FiveHourExhaustedBlocks)
//   - a COUNT quota limit of 0 means UNLIMITED (since 2026-07-21, unified with
//     the Token cap where 0 = unlimited) — no longer a lockout
//   - audit L4: an admin can lower quota_token_total_limit BELOW the accumulated
//     usage, which must block the next request (self-consistent)
//   - effectiveCalls (ceil(multiplier)) is deducted from BOTH counters at once
//   - the 5h window reset machinery (Reset5hQuota / CompensateQuotaReset) that
//     backs the quota scheduler actually zeroes quota_5h_used
package models_test

import (
	"testing"
	"time"

	"llm_api_gateway/internal/models"
)

// TestAtomicDeductQuota_TotalExhaustedBlocks verifies Feature B's cumulative TOTAL
// dimension: when quota_total_used has reached quota_total_limit (but the 5h window
// still has head-room), the deduction is blocked and BOTH counters stay unchanged
// (the atomic UPDATE is all-or-nothing across the 5h and total limits).
func TestAtomicDeductQuota_TotalExhaustedBlocks(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1000, 5) // total limit = 5, 5h huge

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_total_used = 5 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("pre-fill total used: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when total quota exhausted")
	}

	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTotalUsed != 5 {
		t.Fatalf("expected quota_total_used to stay 5, got %d", q.QuotaTotalUsed)
	}
	if q.Quota5hUsed != 0 {
		t.Fatalf("expected quota_5h_used to stay 0, got %d", q.Quota5hUsed)
	}
}

// TestAtomicDeductQuota_CountLimitZeroUnlimited verifies a COUNT quota limit of
// 0 now means "call-count not restricted": the gate opens unconditionally via
// `(quota_5h_limit = 0 OR used + calls <= limit)`. (Since 2026-07-21 the count
// cap is unified with the Token cap, where 0 also means unlimited; the old
// "0 = legacy lockout" behaviour was intentionally removed.)
func TestAtomicDeductQuota_CountLimitZeroUnlimited(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1000, 1000) // generous count limits

	// Force a degenerate 5h limit of 0.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 0 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("set 5h limit=0: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction ALLOWED when 5h limit=0 (unlimited)")
	}
}

// TestAtomicDeductQuota_TokenLimitBelowUsedBlocks verifies audit L4 for Feature A:
// an admin (or migration) can set quota_token_total_limit BELOW the already
// accumulated quota_token_total_used. Because the gate is a pure column comparison
// `quota_token_total_used < quota_token_total_limit`, the next request is then
// blocked — the behaviour is self-consistent even though it looks surprising.
func TestAtomicDeductQuota_TokenLimitBelowUsedBlocks(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1000, 1000) // generous count limits

	// Accumulate usage first, then lower the cap below it.
	if err := models.AddTokenUsage(database.Conn, userID, 70); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 10); err != nil {
		t.Fatalf("lower token limit to 10: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when token limit (10) < used (70)")
	}
}

// TestAtomicDeductQuota_EffectiveCallsMultiplierDeduction verifies Feature B's
// multiplier mapping: a multiplier M produces effectiveCalls = ceil(M), which must
// be deducted from BOTH the 5h and total counters in a single atomic step. This is
// the mechanism behind "a ×3 window costs 3 calls per request" (PR #13).
func TestAtomicDeductQuota_EffectiveCallsMultiplierDeduction(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 30, 30) // both limits = 30

	// First "×3" batch: 0 + 3 <= 30 -> allowed, used becomes 3 on both counters.
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 3)
	if err != nil {
		t.Fatalf("deduct 3 (1st): %v", err)
	}
	if !ok {
		t.Fatalf("expected first ×3 deduction to succeed")
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 3 || q.QuotaTotalUsed != 3 {
		t.Fatalf("expected both counters == 3, got 5h=%d total=%d", q.Quota5hUsed, q.QuotaTotalUsed)
	}

	// Jump used close to the limit, then a 3-unit deduction that would exceed.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 28, quota_total_used = 28 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("bump used to 28: %v", err)
	}
	ok2, err := models.AtomicDeductQuota(database.Conn, userID, 3) // 28 + 3 = 31 > 30
	if err != nil {
		t.Fatalf("deduct 3 (2nd): %v", err)
	}
	if ok2 {
		t.Fatalf("expected deduction blocked when 28+3 would exceed limit 30")
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.Quota5hUsed != 28 || q2.QuotaTotalUsed != 28 {
		t.Fatalf("expected counters unchanged at 28 after block, got 5h=%d total=%d", q2.Quota5hUsed, q2.QuotaTotalUsed)
	}
}

// TestReset5hQuota_ResetsUsedToZero verifies the 5h window reset (backing the
// quota scheduler's periodic reset) zeroes quota_5h_used for ALL users and rebases
// window_start, while leaving the total and Token counters untouched.
func TestReset5hQuota_ResetsUsedToZero(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 10, 1000)

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 7 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("pre-fill 5h used: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 11); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	if err := models.Reset5hQuota(database.Conn, 5); err != nil {
		t.Fatalf("Reset5hQuota: %v", err)
	}

	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 0 {
		t.Fatalf("expected quota_5h_used reset to 0, got %d", q.Quota5hUsed)
	}
	if q.QuotaTotalUsed != 0 {
		t.Fatalf("expected quota_total_used unchanged at 0, got %d", q.QuotaTotalUsed)
	}
	if q.QuotaTokenTotalUsed != 11 {
		t.Fatalf("expected token usage preserved at 11, got %d", q.QuotaTokenTotalUsed)
	}
	if q.WindowStart == "" {
		t.Fatalf("expected window_start to be rebased to a non-empty value")
	}
}

// TestCompensateQuotaReset_ResetsStaleWindows verifies the scheduler's compensation
// path: a user whose window_start is BEFORE the current window (e.g. the server was
// down across a window boundary) gets quota_5h_used reset to 0, so they are not
// permanently starved of their 5h quota.
func TestCompensateQuotaReset_ResetsStaleWindows(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 10, 1000)

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 7, window_start = '2000-01-01T00:00:00+08:00' WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("set stale window: %v", err)
	}

	if err := models.CompensateQuotaReset(database.Conn, 5); err != nil {
		t.Fatalf("CompensateQuotaReset: %v", err)
	}

	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 0 {
		t.Fatalf("expected stale 5h window reset to 0, got %d", q.Quota5hUsed)
	}
}

// TestCompensateQuotaReset_KeepsCurrentWindow verifies idempotency: a user already
// inside the CURRENT window is NOT double-reset by a compensation tick (so usage
// accumulated this window is preserved across scheduler runs).
func TestCompensateQuotaReset_KeepsCurrentWindow(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 10, 1000)

	now := time.Now().Format(time.RFC3339)
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 4, window_start = ? WHERE user_id = ?`, now, userID,
	); err != nil {
		t.Fatalf("set current window: %v", err)
	}

	if err := models.CompensateQuotaReset(database.Conn, 5); err != nil {
		t.Fatalf("CompensateQuotaReset: %v", err)
	}

	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 4 {
		t.Fatalf("expected current-window usage preserved at 4, got %d", q.Quota5hUsed)
	}
}

// intPtr is a tiny helper for the *int params of UpdateQuotaLimits.
func intPtr(v int) *int { return &v }

// TestUpdateQuotaLimits_SetsBoth exercises the admin-facing helper that writes the
// two COUNT-quota limits together (the positive path used by admin UpdateUser).
// It also covers the partial-update form (only one limit supplied) so the other
// limit is left untouched.
func TestUpdateQuotaLimits_SetsBoth(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 100, 1000)

	if err := models.UpdateQuotaLimits(database.Conn, userID, intPtr(7), intPtr(77)); err != nil {
		t.Fatalf("UpdateQuotaLimits(both): %v", err)
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hLimit != 7 || q.QuotaTotalLimit != 77 {
		t.Fatalf("expected 5h=7 total=77, got 5h=%d total=%d", q.Quota5hLimit, q.QuotaTotalLimit)
	}

	// Partial update: only the total limit changes; 5h must be preserved.
	if err := models.UpdateQuotaLimits(database.Conn, userID, nil, intPtr(99)); err != nil {
		t.Fatalf("UpdateQuotaLimits(total only): %v", err)
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.Quota5hLimit != 7 || q2.QuotaTotalLimit != 99 {
		t.Fatalf("expected 5h=7 total=99 after partial update, got 5h=%d total=%d", q2.Quota5hLimit, q2.QuotaTotalLimit)
	}
}

// TestUpdateFixedMultiplier_SetsAndClears verifies the per-user multiplier override
// (which drives both effectiveCalls = ceil(multiplier) and the billed-Token scaling
// on the proxy path) is written and cleared (nil -> NULL) correctly, and that
// GetFixedMultiplier reads it back.
func TestUpdateFixedMultiplier_SetsAndClears(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1000, 1000)

	m := 3.5
	if err := models.UpdateFixedMultiplier(database.Conn, userID, &m); err != nil {
		t.Fatalf("set fixed multiplier: %v", err)
	}
	fm, err := models.GetFixedMultiplier(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetFixedMultiplier: %v", err)
	}
	if !fm.Valid || fm.Float64 != 3.5 {
		t.Fatalf("expected fixed multiplier 3.5, got valid=%v val=%v", fm.Valid, fm.Float64)
	}

	// Clear to NULL -> the proxy falls back to the global time-based multiplier.
	if err := models.UpdateFixedMultiplier(database.Conn, userID, nil); err != nil {
		t.Fatalf("clear fixed multiplier: %v", err)
	}
	fm2, _ := models.GetFixedMultiplier(database.Conn, userID)
	if fm2.Valid {
		t.Fatalf("expected fixed multiplier cleared (NULL), got valid=%v val=%v", fm2.Valid, fm2.Float64)
	}
}
