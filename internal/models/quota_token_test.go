// Package models_test contains additional tests for the cumulative Token-quota
// feature (the third, OR-combined quota dimension).
package models_test

import (
	"testing"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// setTokenColumns sets both the cumulative Token cap and the already-accumulated
// usage directly, bypassing the domain helpers, so the AtomicDeductQuota WHERE
// clause can be exercised at exact boundary values.
func setTokenColumns(t *testing.T, database *db.DB, userID int64, limit, used int) {
	t.Helper()
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_total_limit = ?, quota_token_total_used = ? WHERE user_id = ?`,
		limit, used, userID,
	); err != nil {
		t.Fatalf("set token columns: %v", err)
	}
}

// newQuotaUserNamed creates a user with the given 5h/total limits and a UNIQUE
// username + sub-key hash (so it can be called repeatedly within one test DB
// without colliding on the users.username / users.sub_key_hash UNIQUE indexes).
func newQuotaUserNamed(t *testing.T, database *db.DB, limit5h, limitTotal int, name string) int64 {
	t.Helper()
	u, err := models.CreateUser(
		database.Conn,
		name, "pw-hash", "sub-hash-"+name, "sk-"+name+"...",
		"user", "active", "", "auto", "",
		limit5h, limitTotal, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(%s) failed: %v", name, err)
	}
	return u.ID
}

// TestAtomicDeductQuota_TokenLimitZeroAlwaysAllows verifies the backward-compatible
// default: when the cumulative Token cap is 0 (unlimited), the deduction succeeds
// regardless of how large the accumulated usage already is. 存量用户默认 0 不受影响。
func TestAtomicDeductQuota_TokenLimitZeroAlwaysAllows(t *testing.T) {
	database := newModelsTestDB(t)
	// Generous count limits so only the Token dimension is under test.
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)

	// Even with a huge accumulated Token usage, limit=0 must never block.
	setTokenColumns(t, database, userID, 0, 9_999_999)
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if !ok {
		t.Fatalf("expected deduction to succeed when token limit=0 (unlimited), even with large used")
	}
}

// TestAtomicDeductQuota_TokenLimitExceededBlocks verifies that with a positive
// cap, once accumulated usage reaches the limit the next request is blocked
// (ok=false) and neither counter is changed.
func TestAtomicDeductQuota_TokenLimitExceededBlocks(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)

	// used == limit -> blocked.
	setTokenColumns(t, database, userID, 100, 100)
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil {
		t.Fatalf("AtomicDeductQuota returned error: %v", err)
	}
	if ok {
		t.Fatalf("expected deduction blocked when token used >= limit (100/100)")
	}

	// Neither the 5h/total counters nor the Token counter should change.
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 100 {
		t.Fatalf("expected token used to stay 100 after block, got %d", q.QuotaTokenTotalUsed)
	}
	if q.Quota5hUsed != 0 || q.QuotaTotalUsed != 0 {
		t.Fatalf("expected count counters unchanged, got 5h=%d total=%d", q.Quota5hUsed, q.QuotaTotalUsed)
	}

	// used strictly greater than limit must also block.
	setTokenColumns(t, database, userID, 100, 150)
	ok2, _ := models.AtomicDeductQuota(database.Conn, userID, 1)
	if ok2 {
		t.Fatalf("expected deduction blocked when token used > limit (150/100)")
	}
}

// TestAtomicDeductQuota_TokenGateThenAddTokenUsage reflects the real request
// lifecycle: AtomicDeductQuota only GATES (it does not touch quota_token_total_used),
// and AddTokenUsage performs the post-response accumulation. Together they implement
// "放行且 used 递增" for the Token dimension — the gate allows while there is
// head-room, then the caller accounts the actual tokens afterward.
func TestAtomicDeductQuota_TokenGateThenAddTokenUsage(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)
	setTokenColumns(t, database, userID, 100, 50) // used < limit

	// Gate allows.
	ok, err := models.AtomicDeductQuota(database.Conn, userID, 1)
	if err != nil || !ok {
		t.Fatalf("gate should allow (ok=%v err=%v)", ok, err)
	}
	// Token used is unchanged immediately after the gate (increment is separate).
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 50 {
		t.Fatalf("token used should still be 50 right after gate, got %d", q.QuotaTokenTotalUsed)
	}
	// Post-response accounting increments (used 递增).
	if err := models.AddTokenUsage(database.Conn, userID, 30); err != nil {
		t.Fatalf("AddTokenUsage: %v", err)
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.QuotaTokenTotalUsed != 80 {
		t.Fatalf("expected token used == 80 after accounting, got %d", q2.QuotaTokenTotalUsed)
	}
}

// TestAtomicDeductQuota_TokenOrCountBlocking verifies the OR semantics across the
// three quota dimensions: a request is blocked if ANY dimension is exhausted, and
// allowed only when ALL dimensions have head-room.
func TestAtomicDeductQuota_TokenOrCountBlocking(t *testing.T) {
	database := newModelsTestDB(t)

	// Case A: count head-room OK, Token EXHAUSTED -> blocked (Token dimension).
	userA := newQuotaUserNamed(t, database, 1_000_000, 1_000_000, "tok-orcount-a")
	setTokenColumns(t, database, userA, 100, 100) // token used == limit
	if ok, _ := models.AtomicDeductQuota(database.Conn, userA, 1); ok {
		t.Fatalf("A: expected block when Token exhausted (count OK)")
	}

	// Case B: Token head-room OK, 5h EXHAUSTED -> blocked (count dimension).
	userB := newQuotaUserNamed(t, database, 5, 1_000_000, "tok-orcount-b")
	if _, err := database.Conn.Exec(`UPDATE quotas SET quota_5h_used = 5 WHERE user_id = ?`, userB); err != nil {
		t.Fatalf("prefill 5h: %v", err)
	}
	setTokenColumns(t, database, userB, 100_000, 0) // token plenty of room
	if ok, _ := models.AtomicDeductQuota(database.Conn, userB, 1); ok {
		t.Fatalf("B: expected block when 5h exhausted (Token OK)")
	}

	// Case C: BOTH dimensions have head-room -> allowed.
	userC := newQuotaUserNamed(t, database, 1_000_000, 1_000_000, "tok-orcount-c")
	setTokenColumns(t, database, userC, 100, 50) // token used < limit
	if ok, err := models.AtomicDeductQuota(database.Conn, userC, 1); err != nil || !ok {
		t.Fatalf("C: expected allow when both dimensions have head-room (ok=%v err=%v)", ok, err)
	}
}

// TestAddTokenUsage_Accumulates verifies that AddTokenUsage monotonically
// accumulates the cumulative Token usage across multiple calls.
func TestAddTokenUsage_Accumulates(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)

	if err := models.AddTokenUsage(database.Conn, userID, 30); err != nil {
		t.Fatalf("AddTokenUsage(30): %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 20); err != nil {
		t.Fatalf("AddTokenUsage(20): %v", err)
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 50 {
		t.Fatalf("expected cumulative used == 50, got %d", q.QuotaTokenTotalUsed)
	}
}

// TestAddTokenUsage_NonPositiveIsNoop verifies that a delta <= 0 is a safe no-op
// (returns nil, leaves usage unchanged) — guards against spurious -1 deltas or
// accidental zero-accounting from upstream parse failures.
func TestAddTokenUsage_NonPositiveIsNoop(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)

	// Seed a known baseline via a positive delta.
	if err := models.AddTokenUsage(database.Conn, userID, 40); err != nil {
		t.Fatalf("seed AddTokenUsage(40): %v", err)
	}

	if err := models.AddTokenUsage(database.Conn, userID, 0); err != nil {
		t.Fatalf("AddTokenUsage(0) should return nil, got %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, -5); err != nil {
		t.Fatalf("AddTokenUsage(-5) should return nil, got %v", err)
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 40 {
		t.Fatalf("expected used unchanged at 40 after non-positive deltas, got %d", q.QuotaTokenTotalUsed)
	}
}

// TestUpdateQuotaTokenTotalLimit_SetsCap verifies that setting the cumulative
// Token cap takes effect and does NOT reset the already-accumulated usage.
func TestUpdateQuotaTokenTotalLimit_SetsCap(t *testing.T) {
	database := newModelsTestDB(t)
	userID := newQuotaUser(t, database, 1_000_000, 1_000_000)

	// Accumulate some usage first.
	if err := models.AddTokenUsage(database.Conn, userID, 70); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 500); err != nil {
		t.Fatalf("UpdateQuotaTokenTotalLimit(500): %v", err)
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalLimit != 500 {
		t.Fatalf("expected token limit == 500, got %d", q.QuotaTokenTotalLimit)
	}
	if q.QuotaTokenTotalUsed != 70 {
		t.Fatalf("expected accumulated usage preserved at 70, got %d", q.QuotaTokenTotalUsed)
	}

	// Lowering below current usage takes effect (gate blocks on next request).
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 10); err != nil {
		t.Fatalf("UpdateQuotaTokenTotalLimit(10): %v", err)
	}
	q2, _ := models.GetQuota(database.Conn, userID)
	if q2.QuotaTokenTotalLimit != 10 {
		t.Fatalf("expected token limit == 10, got %d", q2.QuotaTokenTotalLimit)
	}
}
