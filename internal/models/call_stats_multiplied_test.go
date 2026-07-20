// Package models_test contains tests for the models package.
package models_test

import (
	"testing"
	"time"

	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/timeutil"
)

// expectedMultiplied is the reference implementation of the caliber-consistency
// formula (mirrors internal/handler/quota.sumMultipliedTokens and the billed
// quota counter): per-row billed tokens = ceil((prompt+completion)*multiplier),
// summed over the rows. Used as the authoritative expected value so the
// AggregateCallStats output can be cross-checked against the user-panel口径.
func expectedMultiplied(rows []struct {
	p, c int
	m    float64
}) int {
	total := 0
	for _, r := range rows {
		m := r.m
		if m == 0 {
			m = 1.0
		}
		billed := int(ceilFloat(float64(r.p+r.c) * m))
		total += billed
	}
	return total
}

// ceilFloat is a tiny local ceil so the test does not import math just for an
// expected-value computation (keeps the test self-contained and obvious).
func ceilFloat(x float64) float64 {
	i := int64(x)
	if float64(i) == x {
		return float64(i)
	}
	if x > 0 {
		return float64(i + 1)
	}
	return float64(i)
}

// TestAggregateCallStats_TokensIncludeMultiplier is the primary caliber-consistency
// test for the admin 调用记录汇总 panel. It seeds rows with DIFFERENT multipliers
// across two users and two models, then asserts:
//
//   - CallStats.Tokens.Total equals Σ ceil((prompt+completion)*multiplier_used)
//     (the same value the user-panel / billed quota counter reports), and
//   - it does NOT equal the raw Σ total_tokens (audit) column — proving the
//     summary is now multiplier-inflated, not a naive raw sum.
//   - The by_model and by_user breakdowns are themselves multiplied and their
//     totals each sum back to the grand total (口径一致 across levels).
func TestAggregateCallStats_TokensIncludeMultiplier(t *testing.T) {
	database := newModelsTestDB(t)

	alice, err := models.CreateUser(
		database.Conn, "mult-alice", "pw", "sub-a", "sk-a...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(alice) failed: %v", err)
	}
	bob, err := models.CreateUser(
		database.Conn, "mult-bob", "pw", "sub-b", "sk-b...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(bob) failed: %v", err)
	}

	// user -> (model, prompt, completion, multiplier)
	seed := []struct {
		uid   int64
		model string
		p, c  int
		m     float64
	}{
		{alice.ID, "glm-5.2", 10, 5, 3.0},  // ceil(15*3) = 45
		{alice.ID, "gpt-4o", 10, 5, 1.0},   // ceil(15*1) = 15
		{bob.ID, "glm-5.2", 100, 100, 2.0}, // ceil(200*2) = 400
	}
	for _, r := range seed {
		if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           r.uid,
			Model:            r.model,
			ProviderID:       "zhipu",
			PromptTokens:     r.p,
			CompletionTokens: r.c,
			TotalTokens:      r.p + r.c, // raw audit value (must NOT be what we sum)
			EffectiveCalls:   1,
			MultiplierUsed:   r.m,
			StatusCode:       200,
		}); err != nil {
			t.Fatalf("InsertCallLog failed: %v", err)
		}
	}

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCallStats returned error: %v", err)
	}

	// Grand multiplied total = 45 + 15 + 400 = 460.
	wantTotal := expectedMultiplied([]struct {
		p, c int
		m    float64
	}{
		{10, 5, 3.0}, {10, 5, 1.0}, {100, 100, 2.0},
	})
	if wantTotal != 460 {
		t.Fatalf("test setup: expected reference multiplied total == 460, got %d", wantTotal)
	}
	if stats.Tokens.Total != wantTotal {
		t.Fatalf("expected multiplied Tokens.Total == %d, got %d", wantTotal, stats.Tokens.Total)
	}

	// Negative control: the raw Σ total_tokens column = 15+15+200 = 230, which
	// must NOT equal the multiplied summary — otherwise the caliber fix regressed.
	rawSum := 15 + 15 + 200
	if stats.Tokens.Total == rawSum {
		t.Fatalf("BUG: aggregated Tokens.Total equals raw total_tokens sum (%d); multiplier not applied", rawSum)
	}

	// Invariant: grand prompt + completion == total.
	if stats.Tokens.Prompt+stats.Tokens.Completion != stats.Tokens.Total {
		t.Fatalf("grand token breakdown mismatch: prompt(%d)+completion(%d) != total(%d)",
			stats.Tokens.Prompt, stats.Tokens.Completion, stats.Tokens.Total)
	}

	// TotalCalls = number of request rows (3); EffectiveCalls = Σ effective_calls (3).
	if stats.TotalCalls != 3 {
		t.Fatalf("expected TotalCalls == 3, got %d", stats.TotalCalls)
	}
	if stats.EffectiveCalls != 3 {
		t.Fatalf("expected EffectiveCalls == 3, got %d", stats.EffectiveCalls)
	}

	// --- by_model breakdown: glm-5.2 (45+400=445, calls=2), gpt-4o (15, calls=1) ---
	byModel := map[string]models.ModelBreakdown{}
	for _, m := range stats.ByModel {
		byModel[m.Model] = m
	}
	gm := byModel["glm-5.2"]
	if gm.Calls != 2 || gm.Tokens.Total != 445 {
		t.Fatalf("expected glm-5.2 calls=2 total=445, got calls=%d total=%d", gm.Calls, gm.Tokens.Total)
	}
	if gm.Tokens.Prompt+gm.Tokens.Completion != gm.Tokens.Total {
		t.Fatalf("glm-5.2 breakdown mismatch: prompt(%d)+completion(%d) != total(%d)",
			gm.Tokens.Prompt, gm.Tokens.Completion, gm.Tokens.Total)
	}
	go4 := byModel["gpt-4o"]
	if go4.Calls != 1 || go4.Tokens.Total != 15 {
		t.Fatalf("expected gpt-4o calls=1 total=15, got calls=%d total=%d", go4.Calls, go4.Tokens.Total)
	}

	// --- by_user breakdown: alice (60), bob (400) ---
	byUser := map[string]models.UserBreakdown{}
	for _, u := range stats.ByUser {
		byUser[u.Username] = u
	}
	ua := byUser["mult-alice"]
	if ua.Tokens.Total != 60 || ua.Calls != 2 {
		t.Fatalf("expected alice total=60 calls=2, got total=%d calls=%d", ua.Tokens.Total, ua.Calls)
	}
	if ua.Tokens.Prompt+ua.Tokens.Completion != ua.Tokens.Total {
		t.Fatalf("alice breakdown mismatch: prompt(%d)+completion(%d) != total(%d)",
			ua.Tokens.Prompt, ua.Tokens.Completion, ua.Tokens.Total)
	}
	ub := byUser["mult-bob"]
	if ub.Tokens.Total != 400 || ub.Calls != 1 {
		t.Fatalf("expected bob total=400 calls=1, got total=%d calls=%d", ub.Tokens.Total, ub.Calls)
	}

	// --- caliber consistency across levels: breakdown totals sum to grand total ---
	var modelSum, userSum int
	for _, m := range stats.ByModel {
		modelSum += m.Tokens.Total
	}
	for _, u := range stats.ByUser {
		userSum += u.Tokens.Total
	}
	if modelSum != stats.Tokens.Total {
		t.Fatalf("by_model totals (%d) must sum to grand Tokens.Total (%d)", modelSum, stats.Tokens.Total)
	}
	if userSum != stats.Tokens.Total {
		t.Fatalf("by_user totals (%d) must sum to grand Tokens.Total (%d)", userSum, stats.Tokens.Total)
	}
}

// TestAggregateCallStats_TokensFractionalMultiplierCeil proves the per-row
// "multiply then ceil" semantics with a FRACTIONAL multiplier: a non-integer
// product must be rounded UP, not truncated/floored, and must NOT equal the raw
// total_tokens column. This isolates theceil behaviour from the integer cases.
//
// Seed row: p=10, c=5, m=2.5 -> ceil(15*2.5) = ceil(37.5) = 38.
func TestAggregateCallStats_TokensFractionalMultiplierCeil(t *testing.T) {
	database := newModelsTestDB(t)

	owner, err := models.CreateUser(
		database.Conn, "ceil-owner", "pw", "sub-c", "sk-c...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}
	if _, err := models.InsertCallLog(database.Conn, &models.CallLog{
		UserID:           owner.ID,
		Model:            "glm-5.2",
		ProviderID:       "zhipu",
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15, // raw audit
		EffectiveCalls:   1,
		MultiplierUsed:   2.5,
		StatusCode:       200,
	}); err != nil {
		t.Fatalf("InsertCallLog failed: %v", err)
	}

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{})
	if err != nil {
		t.Fatalf("AggregateCallStats returned error: %v", err)
	}
	if stats.Tokens.Total != 38 {
		t.Fatalf("expected ceiled fractional Tokens.Total == 38, got %d", stats.Tokens.Total)
	}
	// Negative controls: not truncated (37) and not the raw column (15).
	if stats.Tokens.Total == 37 {
		t.Fatalf("Tokens.Total looks truncated (no ceil): got 37, expected 38")
	}
	if stats.Tokens.Total == 15 {
		t.Fatalf("Tokens.Total equals raw total_tokens; multiplier not applied")
	}
	// Breakdown invariant holds for the single row too.
	if stats.Tokens.Prompt+stats.Tokens.Completion != stats.Tokens.Total {
		t.Fatalf("single-row breakdown mismatch: prompt(%d)+completion(%d) != total(%d)",
			stats.Tokens.Prompt, stats.Tokens.Completion, stats.Tokens.Total)
	}
}

// TestAggregateCallStats_TimeFilterMultiplied is a regression guard ensuring the
// by_user LEFT JOIN + time-filter path (which previously 500'd on an ambiguous
// created_at column) still returns correctly multiplier-inflated tokens. We pin
// one in-window and one out-of-window call under a multiplier and assert only
// the in-window row counts, with its tokens multiplied.
func TestAggregateCallStats_TimeFilterMultiplied(t *testing.T) {
	database := newModelsTestDB(t)

	owner, err := models.CreateUser(
		database.Conn, "tf-owner", "pw", "sub-tf", "sk-tf...",
		"user", "active", "", "auto", "",
		1000, 100000, nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser(owner) failed: %v", err)
	}

	insertAt := func(p, c int, m float64, when string) {
		id, err := models.InsertCallLog(database.Conn, &models.CallLog{
			UserID:           owner.ID,
			Model:            "glm-5.2",
			ProviderID:       "zhipu",
			PromptTokens:     p,
			CompletionTokens: c,
			TotalTokens:      p + c,
			EffectiveCalls:   1,
			MultiplierUsed:   m,
			StatusCode:       200,
		})
		if err != nil {
			t.Fatalf("InsertCallLog failed: %v", err)
		}
		if _, err := database.Conn.Exec(
			"UPDATE call_logs SET created_at = ? WHERE id = ?", when, id,
		); err != nil {
			t.Fatalf("override created_at failed: %v", err)
		}
	}

	now := time.Now().In(timeutil.ShanghaiTZ)
	from := now.AddDate(0, 0, -7).Format(time.RFC3339)
	to := now.Format(time.RFC3339)
	inWindow := now.Add(-1 * time.Hour).Format(time.RFC3339)
	outWindow := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)

	// In-window: p=10,c=5,m=4.0 -> ceil(15*4)=60.
	insertAt(10, 5, 4.0, inWindow)
	// Out-of-window: p=10,c=5,m=4.0 -> would also be 60 but must be excluded.
	insertAt(10, 5, 4.0, outWindow)

	stats, err := models.AggregateCallStats(database.Conn, models.CallLogFilter{
		From: from,
		To:   to,
	})
	if err != nil {
		t.Fatalf("AggregateCallStats(with From/To) returned error: %v", err)
	}
	if stats.TotalCalls != 1 {
		t.Fatalf("expected 1 in-window call, got %d", stats.TotalCalls)
	}
	// Only the in-window (x4) row counts -> 60, not 120.
	if stats.Tokens.Total != 60 {
		t.Fatalf("expected multiplied in-window Tokens.Total == 60, got %d", stats.Tokens.Total)
	}
	if len(stats.ByUser) != 1 {
		t.Fatalf("expected exactly 1 by_user entry (in-window only), got %d", len(stats.ByUser))
	}
	if stats.ByUser[0].Tokens.Total != 60 {
		t.Fatalf("expected by_user multiplied total == 60, got %d", stats.ByUser[0].Tokens.Total)
	}
}
