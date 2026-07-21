// Package models_test contains tests for the dual Token-window (5h + weekly)
// soft-gate introduced alongside the cumulative Token cap.
package models_test

import (
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// seedTokenWindowUser creates a user with generous count quotas and zeroes the
// Token caps (unlimited) so only the dimension under test can trip the gate.
// It returns the new user ID.
func seedTokenWindowUser(t *testing.T, database *db.DB) int64 {
	t.Helper()
	return newQuotaUser(t, database, 1_000_000, 1_000_000)
}

// TestAtomicDeductQuota_Token5hWindowBlocksWhenExhausted verifies the 5h-window
// Token gate blocks when the 5h Token used has reached its cap (and the 5h count
// window is still fresh, so the block is correctly attributed to the Token dim).
func TestAtomicDeductQuota_Token5hWindowBlocksWhenExhausted(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_5h_limit = 10, quota_token_5h_used = 10 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("seed 5h token window: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when 5h Token window exhausted")
	}
}

// TestAtomicDeductQuota_TokenWeekWindowBlocksWhenExhausted verifies the weekly
// Token gate blocks when the weekly used has reached its cap, with the fixed
// phase anchor (week_start) and the current cycle start (week_cycle_start) both
// set to now so the cyclic reset does NOT kick in.
func TestAtomicDeductQuota_TokenWeekWindowBlocksWhenExhausted(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	now := time.Now().Format(time.RFC3339)
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_week_limit = 10, quota_token_week_used = 10, week_start = ?, week_cycle_start = ? WHERE user_id = ?`,
		now, now, userID,
	); err != nil {
		t.Fatalf("seed week token window: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when weekly Token window exhausted")
	}
}

// TestAtomicDeductQuota_TokenTotalBlocksWhenExhausted pins the cumulative Token
// gate (the original behaviour) still blocks at its cap.
func TestAtomicDeductQuota_TokenTotalBlocksWhenExhausted(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 10); err != nil {
		t.Fatalf("set token total limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 10); err != nil {
		t.Fatalf("seed token total usage: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when cumulative Token cap reached")
	}
}

// TestAtomicDeductQuota_ZeroTokenLimitsDoNotBlock documents that a limit of 0
// means unlimited for ALL three Token dimensions — the gate must not block on a
// 0 limit even when used is high (used values come from legacy/accumulated data).
func TestAtomicDeductQuota_ZeroTokenLimitsDoNotBlock(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	// Seed large "used" values but leave all caps at 0 (unlimited).
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_5h_used = 999, quota_token_week_used = 999 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("seed token used: %v", err)
	}
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 0); err != nil {
		t.Fatalf("clear token total limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 999); err != nil {
		t.Fatalf("seed token total usage: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction allowed when all Token caps are 0 (unlimited)")
	}
}

// TestAtomicDeductQuota_WeekWindowCyclicReset verifies the fixed-phase weekly
// bucket reset: when the admin-set anchor (week_start) is in the past and the
// stored cycle start (week_cycle_start) no longer matches the cycle containing
// now, the gate zeroes quota_token_week_used and advances week_cycle_start to
// the current cycle. Crucially, the FIXED anchor week_start is NEVER bumped —
// that is what makes the bucket recur every 7 days from the admin-set phase
// instead of rolling.
func TestAtomicDeductQuota_WeekWindowCyclicReset(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	anchor := time.Now().Add(-8 * 24 * time.Hour) // fixed phase anchor, 8 days ago
	anchorStr := anchor.Format(time.RFC3339)
	// Seed: anchor 8 days ago, the stored cycle start also 8 days ago (so it is
	// stale vs the cycle containing now), weekly used = 9, cap = 10.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_week_limit = 10, quota_token_week_used = 9, week_start = ?, week_cycle_start = ? WHERE user_id = ?`,
		anchorStr, anchorStr, userID,
	); err != nil {
		t.Fatalf("seed stale week window: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction allowed after the stale weekly cycle was reset")
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaTokenWeekUsed != 0 {
		t.Fatalf("expected weekly used reset to 0 after cyclic reset, got %d", q.QuotaTokenWeekUsed)
	}
	// The fixed anchor must remain exactly the seeded value (NOT bumped to now).
	if q.WeekStart != anchorStr {
		t.Fatalf("week_start must stay the fixed anchor, got %q want %q", q.WeekStart, anchorStr)
	}
	// week_cycle_start must have advanced to the cycle containing now.
	nowUTC := time.Now().UTC()
	k := int64(nowUTC.Sub(anchor) / (7 * 24 * time.Hour))
	expectedCycle := anchor.Add(time.Duration(k) * 7 * 24 * time.Hour).Format(time.RFC3339)
	var gotCycle string
	if err := database.Conn.QueryRow(`SELECT week_cycle_start FROM quotas WHERE user_id = ?`, userID).Scan(&gotCycle); err != nil {
		t.Fatalf("read week_cycle_start: %v", err)
	}
	if gotCycle != expectedCycle {
		t.Fatalf("week_cycle_start advanced to wrong cycle: got %q want %q", gotCycle, expectedCycle)
	}
	// Sanity: the 5h count counters still advanced for this request.
	if q.Quota5hUsed != 1 || q.QuotaTotalUsed != 1 {
		t.Fatalf("expected 5h/total used advanced by 1, got 5h=%d total=%d", q.Quota5hUsed, q.QuotaTotalUsed)
	}
}

// TestAddTokenUsage_AccumulatesThreeColumns verifies that the response-time
// accounting step adds the SAME billed delta to the cumulative, 5h-window and
// weekly Token columns — never the call count.
func TestAddTokenUsage_AccumulatesThreeColumns(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	if err := models.AddTokenUsage(database.Conn, userID, 25); err != nil {
		t.Fatalf("AddTokenUsage: %v", err)
	}
	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaTokenTotalUsed != 25 {
		t.Fatalf("cumulative used = %d, want 25", q.QuotaTokenTotalUsed)
	}
	if q.QuotaToken5hUsed != 25 {
		t.Fatalf("5h-window used = %d, want 25", q.QuotaToken5hUsed)
	}
	if q.QuotaTokenWeekUsed != 25 {
		t.Fatalf("weekly used = %d, want 25", q.QuotaTokenWeekUsed)
	}

	// A second accounting must accumulate further (same delta to all three).
	if err := models.AddTokenUsage(database.Conn, userID, 7); err != nil {
		t.Fatalf("AddTokenUsage(2): %v", err)
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.QuotaTokenTotalUsed != 32 || q2.QuotaToken5hUsed != 32 || q2.QuotaTokenWeekUsed != 32 {
		t.Fatalf("after second add: got total=%d 5h=%d week=%d, want 32/32/32",
			q2.QuotaTokenTotalUsed, q2.QuotaToken5hUsed, q2.QuotaTokenWeekUsed)
	}

	// A non-positive delta is a safe no-op.
	if err := models.AddTokenUsage(database.Conn, userID, 0); err != nil {
		t.Fatalf("AddTokenUsage(0): %v", err)
	}
	q3, _ := models.GetQuota(database.Conn, userID)
	if q3.QuotaTokenTotalUsed != 32 {
		t.Fatalf("non-positive delta must be a no-op, used = %d", q3.QuotaTokenTotalUsed)
	}
}

// TestReset5hQuota_ResetsToken5hUsed verifies the 5h window reset path (scheduler)
// zeroes quota_token_5h_used together with the count quota.
func TestReset5hQuota_ResetsToken5hUsed(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	if _, err := database.Conn.Exec(`UPDATE quotas SET quota_token_5h_used = 5 WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("seed 5h token used: %v", err)
	}
	if err := models.Reset5hQuota(database.Conn, 5); err != nil {
		t.Fatalf("Reset5hQuota: %v", err)
	}
	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaToken5hUsed != 0 {
		t.Fatalf("expected 5h Token used reset to 0, got %d", q.QuotaToken5hUsed)
	}
}

// TestCompensateQuotaReset_ResetsToken5hUsed verifies the startup-compensation
// reset (window_start in the past) also zeroes quota_token_5h_used.
func TestCompensateQuotaReset_ResetsToken5hUsed(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	// Force a window_start older than the current window so Compensate resets it.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_5h_used = 5, window_start = ? WHERE user_id = ?`,
		time.Now().Add(-6*time.Hour).Format(time.RFC3339), userID,
	); err != nil {
		t.Fatalf("seed stale 5h window: %v", err)
	}
	if err := models.CompensateQuotaReset(database.Conn, 5); err != nil {
		t.Fatalf("CompensateQuotaReset: %v", err)
	}
	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaToken5hUsed != 0 {
		t.Fatalf("expected 5h Token used reset to 0, got %d", q.QuotaToken5hUsed)
	}
}

// TestUpdateQuotaTokenWindowLimits verifies the admin setter writes both the 5h
// and weekly Token caps, leaving the cumulative cap untouched.
func TestUpdateQuotaTokenWindowLimits(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	if err := models.UpdateQuotaTokenWindowLimits(database.Conn, userID, 50, 80); err != nil {
		t.Fatalf("UpdateQuotaTokenWindowLimits: %v", err)
	}
	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaToken5hLimit != 50 {
		t.Fatalf("5h Token limit = %d, want 50", q.QuotaToken5hLimit)
	}
	if q.QuotaTokenWeekLimit != 80 {
		t.Fatalf("weekly Token limit = %d, want 80", q.QuotaTokenWeekLimit)
	}
	if q.QuotaTokenTotalLimit != 0 {
		t.Fatalf("cumulative Token limit should stay 0 (untouched), got %d", q.QuotaTokenTotalLimit)
	}
}
