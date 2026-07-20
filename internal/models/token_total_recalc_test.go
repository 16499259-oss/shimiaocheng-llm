// Package models_test contains tests for the models package.
package models_test

import (
	"testing"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// recalcQuotaTokenTotalUsedSQL is the ONE-TIME production backfill that the team
// lead runs manually against the production database. It recomputes each user's
// cumulative Token usage (quota_token_total_used) as the SUM over that user's
// call_logs of ceil((prompt_tokens+completion_tokens) * multiplier_used).
//
// SQLite has no ceil() for arbitrary reals, so we use the idiom
// CAST(x + 0.999999 AS INTEGER), which truncates toward zero and therefore
// equals ceil(x) for every non-negative x whose fractional part is >= 0.000001
// (always true for our billed Token increments). A correlated subquery scopes
// the SUM to the outer quotas.user_id, and COALESCE(..., 0) keeps users with no
// call_logs at 0.
//
// IMPORTANT: this is TEST-ONLY documentation of the SQL. The recalc is NOT
// resident code — it is wired into no migration and no handler. It is kept here
// purely as a reproducible proof of correctness that the lead can re-run before
// applying the same statement to the production DB.
const recalcQuotaTokenTotalUsedSQL = `UPDATE quotas SET quota_token_total_used = COALESCE((SELECT SUM(CAST((prompt_tokens+completion_tokens)*multiplier_used+0.999999 AS INTEGER)) FROM call_logs WHERE call_logs.user_id=quotas.user_id),0)`

// insertCallLogRaw inserts a call_log row directly (bypassing models.InsertCallLog)
// with explicit prompt/completion/multiplier so the recalc math is deterministic
// and independent of any hashing/timestamp logic inside the production path.
func insertCallLogRaw(t *testing.T, database *db.DB, userID int64, prompt, completion int, multiplier float64) {
	t.Helper()
	if _, err := database.Conn.Exec(
		`INSERT INTO call_logs (user_id, model, prompt_tokens, completion_tokens, multiplier_used, status_code, total_tokens, effective_calls)
		 VALUES (?, 'glm-5.2', ?, ?, ?, 200, ?, 1)`,
		userID, prompt, completion, multiplier, prompt+completion,
	); err != nil {
		t.Fatalf("insert call_log (user=%d m=%v) failed: %v", userID, multiplier, err)
	}
}

// getQuotaTokenTotalUsed reads a single user's quota_token_total_used column.
func getQuotaTokenTotalUsed(t *testing.T, database *db.DB, userID int64) int {
	t.Helper()
	var v int
	if err := database.Conn.QueryRow(
		`SELECT quota_token_total_used FROM quotas WHERE user_id = ?`, userID,
	).Scan(&v); err != nil {
		t.Fatalf("select quota_token_total_used (user=%d) failed: %v", userID, err)
	}
	return v
}

// TestTokenTotalRecalc_MultiplierAwareAndPerUser is the proof of correctness for
// the one-time recalc SQL (recalcQuotaTokenTotalUsedSQL):
//
//   - alice (target user) gets 3 call_logs:
//     m=3.0, (p=10,c=5) -> (15)*3.0=45.0 -> CAST(45.999999)=45
//     m=1.0, (p=10,c=5) -> (15)*1.0=15.0 -> CAST(15.999999)=15
//     m=2.5, (p=10,c=5) -> (15)*2.5=37.5 -> CAST(38.499999)=38  (ceil)
//     => expected quota_token_total_used == 45 + 15 + 38 = 98.
//
//   - A reverse assertion proves the multiplier is actually applied: the raw
//     (no-multiplier) sum for alice would be (10+5)*3 = 45. If the recalc
//     ignored multiplier_used it would yield 45, so we assert the result is NOT 45.
//
//   - bob (a DIFFERENT user) gets his own log (m=1.0, 10+5=15). His total must be
//     exactly 15 and must NOT leak into alice's 98 — proving the correlated
//     subquery is correctly scoped per user_id and users do not cross-contaminate.
//
//   - carol gets NO call_logs at all; COALESCE(...,0) must leave her at 0,
//     confirming users with no usage are not mis-billed.
func TestTokenTotalRecalc_MultiplierAwareAndPerUser(t *testing.T) {
	database := newModelsTestDB(t)

	alice, err := models.CreateUser(
		database.Conn,
		"recalc-alice", "pw-hash", "sub-recalc-alice", "sk-alice...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(alice) failed: %v", err)
	}
	bob, err := models.CreateUser(
		database.Conn,
		"recalc-bob", "pw-hash", "sub-recalc-bob", "sk-bob...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(bob) failed: %v", err)
	}
	carol, err := models.CreateUser(
		database.Conn,
		"recalc-carol", "pw-hash", "sub-recalc-carol", "sk-carol...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(carol) failed: %v", err)
	}

	// alice's 3 logs (the values from the spec).
	insertCallLogRaw(t, database, alice.ID, 10, 5, 3.0) // -> 45
	insertCallLogRaw(t, database, alice.ID, 10, 5, 1.0) // -> 15
	insertCallLogRaw(t, database, alice.ID, 10, 5, 2.5) // -> ceil(37.5) = 38

	// bob's own log (m=1.0, 10+5=15) — proves alice's total is not contaminated.
	insertCallLogRaw(t, database, bob.ID, 10, 5, 1.0) // -> 15

	// carol intentionally gets NO call_logs.

	// Run the one-time recalc exactly as the lead will run it in production.
	if _, err := database.Conn.Exec(recalcQuotaTokenTotalUsedSQL); err != nil {
		t.Fatalf("recalc UPDATE failed: %v", err)
	}

	// 1) alice == 98 (multiplier-aware cumulative total).
	aliceUsed := getQuotaTokenTotalUsed(t, database, alice.ID)
	if aliceUsed != 98 {
		t.Fatalf("expected alice quota_token_total_used == 98 (multiplier-aware), got %d", aliceUsed)
	}

	// 2) Reverse assertion: a no-multiplier recalc would give 45 (raw sum of
	//    (10+5)*3). If we got 45, the multiplier was ignored — fail loudly.
	if aliceUsed == 45 {
		t.Fatalf("recalc IGNORES the multiplier (got 45 = raw sum of prompt+completion); expected 98")
	}

	// 3) per-user scoping: bob must equal his own sum (15), proving alice's 98
	//    did not bleed into bob and vice-versa.
	bobUsed := getQuotaTokenTotalUsed(t, database, bob.ID)
	if bobUsed != 15 {
		t.Fatalf("expected bob quota_token_total_used == 15, got %d", bobUsed)
	}

	// 4) COALESCE path: carol has no logs -> stays at 0 (not mis-billed).
	carolUsed := getQuotaTokenTotalUsed(t, database, carol.ID)
	if carolUsed != 0 {
		t.Fatalf("expected carol (no call_logs) quota_token_total_used == 0, got %d", carolUsed)
	}
}
