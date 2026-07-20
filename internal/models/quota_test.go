// Package models_test contains tests for the models package.
package models_test

import (
	"testing"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// newQuotaUser creates a user with the given 5h/total limits and returns its ID.
func newQuotaUser(t *testing.T, database *db.DB, limit5h, limitTotal int) int64 {
	t.Helper()
	u, err := models.CreateUser(
		database.Conn,
		"quota-user", "pw-hash", "sub-hash-quota", "sk-quota...",
		"user", "active", "", "auto", "",
		limit5h, limitTotal, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	return u.ID
}

// TestAtomicDeductQuota_AtBoundary verifies the exact-limit boundary: when the
// used amount plus the requested deduction equals the limit, the deduction is
// allowed (the constraint uses <=). This guards against an off-by-one that would
// silently block the last permitted call.
func TestAtomicDeductQuota_AtBoundary(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 5, 1000) // 5h limit = 5

	// Pre-fill the 5h window to 4 used.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 4 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("pre-fill 5h used: %v", err)
	}

	// Deduct exactly the remaining 1 unit -> should succeed (4+1 <= 5).
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction at exact boundary to succeed")
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota failed: %v", err)
	}
	if q.Quota5hUsed != 5 {
		t.Fatalf("expected quota_5h_used == 5, got %d", q.Quota5hUsed)
	}

	// Now another deduction of 1 would exceed (5+1 > 5) -> must be rejected.
	ok2, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota(2nd) returned error: %v", err)
	}
	if ok2 {
		t.Fatalf("expected deduction beyond boundary to be rejected")
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.Quota5hUsed != 5 {
		t.Fatalf("expected quota_5h_used to stay 5 after rejection, got %d", q2.Quota5hUsed)
	}
}

// TestAtomicDeductQuota_FiveHourExhaustedBlocks verifies that exhaustion of the
// 5h window (while total quota is still abundant) blocks the call and leaves
// both counters unchanged — the deduction is all-or-nothing across both limits.
func TestAtomicDeductQuota_FiveHourExhaustedBlocks(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 3, 10000) // 5h limit = 3, total huge

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 3 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("pre-fill 5h used: %v", err)
	}

	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when 5h window exhausted")
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota failed: %v", err)
	}
	if q.Quota5hUsed != 3 {
		t.Fatalf("expected quota_5h_used to stay 3, got %d", q.Quota5hUsed)
	}
	if q.QuotaTotalUsed != 0 {
		t.Fatalf("expected quota_total_used to stay 0, got %d", q.QuotaTotalUsed)
	}
}

// TestAtomicDeductQuota_MultiStep verifies sequential deductions accumulate
// correctly across both the 5h and total counters until the limit is hit.
func TestAtomicDeductQuota_MultiStep(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 5, 10)

	for i := 0; i < 5; i++ {
		ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
		if err != nil {
			t.Fatalf("deduct step %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("deduct step %d should succeed (within limit)", i)
		}
	}
	// After 5 successful 1-unit deductions, both counters == 5.
	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 5 || q.QuotaTotalUsed != 5 {
		t.Fatalf("expected both counters == 5, got 5h=%d total=%d", q.Quota5hUsed, q.QuotaTotalUsed)
	}

	// 6th deduction exceeds both limits -> rejected.
	ok, _ := models.AtomicDeductQuota(database.Conn, userID, 1)
	if ok {
		t.Fatalf("expected 6th deduction to be rejected")
	}
}

// TestAtomicDeductQuota_TokenSoftGatePermitsSingleOverage documents the audit L1
// deviation: the Token gate is a pure column comparison
// (quota_token_total_used < quota_token_total_limit) evaluated BEFORE the
// request's billed Token increment is known (the increment is only computed from
// the upstream response, after the gate has already decided). So a request whose
// deduction is allowed may still push the cumulative used PAST the cap by up to
// one billed increment — exactly what a high multiplier amplifies. The overage is
// accepted by design (a soft gate) and the NEXT request is blocked once used >= limit.
func TestAtomicDeductQuota_TokenSoftGatePermitsSingleOverage(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1000, 1000) // generous count quotas

	// Token cap = 10, already used = 9.
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 10); err != nil {
		t.Fatalf("set token limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 9); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	// Gate: 9 < 10 -> allow the request even though the response will bill more.
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction allowed while used (9) < limit (10)")
	}

	// Post-response accounting: a high-multiplier window bills, say, 5 more tokens
	// (representative of ceil((prompt+completion)*multiplier)).
	if err := models.AddTokenUsage(database.Conn, userID, 5); err != nil {
		t.Fatalf("add token usage: %v", err)
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 14 {
		t.Fatalf("expected token used == 14 (over cap 10), got %d", q.QuotaTokenTotalUsed)
	}

	// Next request: used (14) >= limit (10) -> blocked.
	ok2, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota(2): %v", err)
	}
	if ok2 {
		t.Fatalf("expected next request blocked once used >= limit")
	}
}

// TestResetQuotaUsage verifies ALL usage buckets — call-count (5h + total) and
// Token (5h / week / month) — are zeroed, and ALL window anchors (window_start,
// week_start, month_start) are bumped to now.
func TestResetQuotaUsage(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 100, 10000)

	// Seed token-usage buckets to non-zero values.
	if err := models.AddTokenUsage(database.Conn, userID, 50); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}
	// Also seed call-count columns.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_used = 7, quota_total_used = 42 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("seed call-count cols: %v", err)
	}

	// Force all anchors to known old values so the bump is detectable.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET window_start = '2020-01-01T00:00:00Z', week_start = '2020-01-01T00:00:00Z', month_start = '2020-01-01T00:00:00Z' WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("set old anchors: %v", err)
	}

	// Capture pre-reset state.
	qBefore, err := models.GetQuota(database.Conn, userID)
	if err != nil || qBefore == nil {
		t.Fatalf("GetQuota before: %v", err)
	}
	oldWindowStart := qBefore.WindowStart
	oldWeekStart := qBefore.WeekStart
	oldMonthStart := qBefore.MonthStart

	// Sanity: buckets are non-zero.
	if qBefore.Quota5hUsed == 0 || qBefore.QuotaTotalUsed == 0 {
		t.Fatalf("expected call-count buckets to be non-zero after seeding")
	}
	if qBefore.QuotaToken5hUsed == 0 || qBefore.QuotaTokenWeekUsed == 0 || qBefore.QuotaTokenTotalUsed == 0 {
		t.Fatalf("expected token buckets to be non-zero after seeding")
	}

	// Reset.
	if err := models.ResetQuotaUsage(database.Conn, userID, ""); err != nil {
		t.Fatalf("ResetQuotaUsage: %v", err)
	}

	qAfter, err := models.GetQuota(database.Conn, userID)
	if err != nil || qAfter == nil {
		t.Fatalf("GetQuota after: %v", err)
	}

	// Call-count buckets MUST be zero.
	if qAfter.Quota5hUsed != 0 {
		t.Fatalf("quota_5h_used expected 0, got %d", qAfter.Quota5hUsed)
	}
	if qAfter.QuotaTotalUsed != 0 {
		t.Fatalf("quota_total_used expected 0, got %d", qAfter.QuotaTotalUsed)
	}

	// Token buckets MUST be zero.
	if qAfter.QuotaToken5hUsed != 0 {
		t.Fatalf("quota_token_5h_used expected 0, got %d", qAfter.QuotaToken5hUsed)
	}
	if qAfter.QuotaTokenWeekUsed != 0 {
		t.Fatalf("quota_token_week_used expected 0, got %d", qAfter.QuotaTokenWeekUsed)
	}
	if qAfter.QuotaTokenTotalUsed != 0 {
		t.Fatalf("quota_token_total_used expected 0, got %d", qAfter.QuotaTokenTotalUsed)
	}

	// ALL window anchors MUST be bumped forward.
	if qAfter.WindowStart == oldWindowStart {
		t.Fatalf("window_start expected to be bumped, but still %q", qAfter.WindowStart)
	}
	if qAfter.WeekStart == oldWeekStart {
		t.Fatalf("week_start expected to be bumped, but still %q", qAfter.WeekStart)
	}
	if qAfter.MonthStart == oldMonthStart {
		t.Fatalf("month_start expected to be bumped, but still %q", qAfter.MonthStart)
	}
}
