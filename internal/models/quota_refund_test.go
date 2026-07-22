package models

import (
	"path/filepath"
	"testing"

	"llm_api_gateway/internal/db"
)

// TestRefundQuotaUsage locks the refund semantics used when an upstream request
// fails (non-2xx / transport error): the previously-deducted call-count quota is
// returned, clamped at 0 so it can never go negative, and a non-positive delta is
// a safe no-op (audit MEDIUM: 上游非200 仍扣次数不退款).
func TestRefundQuotaUsage(t *testing.T) {
	f, err := filepath.Abs(filepath.Join(t.TempDir(), "refund_test.db"))
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatal(err)
	}

	u, err := CreateUser(database.Conn, "refunduser", "pw", "hash", "prev",
		"user", "active", "", "auto", "", 100, 100, nil, 1<<20, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a deducted count of 3 (a request that then failed upstream).
	if _, err := AtomicDeductQuota(database.Conn, u.ID, 3); err != nil {
		t.Fatal(err)
	}
	q, _ := GetQuota(database.Conn, u.ID)
	if q.Quota5hUsed != 3 || q.QuotaTotalUsed != 3 {
		t.Fatalf("after deduct: 5h=%d total=%d, want 3/3", q.Quota5hUsed, q.QuotaTotalUsed)
	}

	// Refund 3 -> back to 0.
	if err := RefundQuotaUsage(database.Conn, u.ID, 3); err != nil {
		t.Fatal(err)
	}
	q, _ = GetQuota(database.Conn, u.ID)
	if q.Quota5hUsed != 0 || q.QuotaTotalUsed != 0 {
		t.Fatalf("after refund: 5h=%d total=%d, want 0/0", q.Quota5hUsed, q.QuotaTotalUsed)
	}

	// Over-refund must clamp at 0 (never negative).
	if err := RefundQuotaUsage(database.Conn, u.ID, 5); err != nil {
		t.Fatal(err)
	}
	q, _ = GetQuota(database.Conn, u.ID)
	if q.Quota5hUsed != 0 {
		t.Fatalf("over-refund should clamp at 0, got %d", q.Quota5hUsed)
	}

	// Non-positive delta is a no-op.
	if err := RefundQuotaUsage(database.Conn, u.ID, 0); err != nil {
		t.Fatal(err)
	}
}
