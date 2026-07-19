package models

import (
	"database/sql"
	"testing"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// insertEdgeLog inserts a call_log for the given provider with an explicit
// created_at (RFC3339, Asia/Shanghai). Used by the boundary tests below.
func insertEdgeLog(t *testing.T, conn *sql.DB, provider, created string, pt, ct, ec int) {
	t.Helper()
	if _, err := conn.Exec(
		`INSERT INTO call_logs (user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, status_code, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "m", provider, pt, ct, pt+ct, ec, 200, created,
	); err != nil {
		t.Fatalf("insert edge log: %v", err)
	}
}

// TestAggregateProviderUsage_WindowBoundary verifies the rolling-window
// boundary is INCLUSIVE on the lower edge: a call_log whose created_at is
// exactly equal to windowStart is counted, but a call_log one second earlier
// is excluded. This is the core correctness property of the text comparison
// `created_at >= windowStart` used by the aggregation query.
func TestAggregateProviderUsage_WindowBoundary(t *testing.T) {
	conn := usageTestDB(t)
	windowStart := RollingWindowStart()

	wsTime, err := time.Parse(time.RFC3339, windowStart)
	if err != nil {
		t.Fatalf("parse windowStart %q: %v", windowStart, err)
	}
	// Exactly at the window start (inclusive) -> MUST be counted.
	atStart := wsTime.Format(time.RFC3339)
	// One second BEFORE the window start -> MUST be excluded.
	beforeStart := wsTime.Add(-1 * time.Second).Format(time.RFC3339)
	// Well inside the window (sanity: normal in-window rows counted).
	inside := time.Now().Add(-1 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)

	// All three rows belong to the same provider so we can assert the exact
	// summed amounts: only `inside` + `atStart` should be included.
	insertEdgeLog(t, conn, "openai", inside, 100, 50, 2)      // 150 tok, 2 calls
	insertEdgeLog(t, conn, "openai", atStart, 100, 50, 3)     // 150 tok, 3 calls
	insertEdgeLog(t, conn, "openai", beforeStart, 9999, 1, 9) // must be excluded

	res, err := AggregateProviderUsage(conn, windowStart)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	u, ok := res["openai"]
	if !ok {
		t.Fatal("openai missing from aggregation")
	}
	// 150 + 150 = 300 tokens; 2 + 3 = 5 calls; the beforeStart row excluded.
	if u.TokenUsed != 300 {
		t.Errorf("openai token_used=%d want 300 (boundary row mis-handled)", u.TokenUsed)
	}
	if u.CallUsed != 5 {
		t.Errorf("openai call_used=%d want 5 (boundary row mis-handled)", u.CallUsed)
	}
}

// TestAggregateProviderUsage_MultiProviderGrouping verifies that usage is
// correctly GROUPed BY provider_id: each in-window provider's token/call
// totals are computed independently and not cross-contaminated.
func TestAggregateProviderUsage_MultiProviderGrouping(t *testing.T) {
	conn := usageTestDB(t)
	in := time.Now().Add(-1 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)

	// openai: two in-window rows.
	insertEdgeLog(t, conn, "openai", in, 100, 50, 2) // 150 tok, 2 calls
	insertEdgeLog(t, conn, "openai", in, 100, 50, 3) // 150 tok, 3 calls
	// zhipu: one in-window row.
	insertEdgeLog(t, conn, "zhipu", in, 200, 100, 4) // 300 tok, 4 calls
	// anthropic: one out-of-window row (must not pollute any bucket).
	out := time.Now().Add(-40 * 24 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	insertEdgeLog(t, conn, "anthropic", out, 5000, 5000, 99)

	res, err := AggregateProviderUsage(conn, RollingWindowStart())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	ou, ok := res["openai"]
	if !ok {
		t.Fatal("openai missing from aggregation")
	}
	if ou.TokenUsed != 300 || ou.CallUsed != 5 {
		t.Errorf("openai = tok=%d call=%d want 300/5", ou.TokenUsed, ou.CallUsed)
	}

	zu, ok := res["zhipu"]
	if !ok {
		t.Fatal("zhipu missing from aggregation")
	}
	if zu.TokenUsed != 300 || zu.CallUsed != 4 {
		t.Errorf("zhipu = tok=%d call=%d want 300/4", zu.TokenUsed, zu.CallUsed)
	}

	if _, ok := res["anthropic"]; ok {
		t.Error("out-of-window anthropic log must not appear in aggregation")
	}
}
