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
		limit5h, limitTotal, nil,
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
