package models

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/timeutil"
)

// usageTestDB opens a migrated temp SQLite DB and returns its *sql.DB.
func usageTestDB(t *testing.T) *sql.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "usage_models_test_*.db")
	if err != nil {
		t.Fatalf("temp db: %v", err)
	}
	f.Close()
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// call_logs has a FK on users(id); seed a user so usage inserts are valid.
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at)
		 VALUES ('usage-test-user', 'x', 'x', 'x', 'user', 'active', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return database.Conn
}

func TestIsLowBalance(t *testing.T) {
	// remainingRatio is the "remaining threshold": flag red when the remaining
	// fraction drops below it (boundary inclusive). limit<=0 is never low.
	cases := []struct {
		used, limit    int64
		remainingRatio float64
		want           bool
	}{
		{0, 0, 0.10, false},      // unlimited (limit<=0) never low
		{0, 100, 0.10, false},    // zero usage never low
		{899, 1000, 0.10, false}, // 10.1% remaining -> not low
		{900, 1000, 0.10, true},  // exactly 10% remaining -> low
		{500, 1000, 0.10, false}, // 50% remaining -> not low
		{1000, 1000, 0.10, true}, // over limit -> low
		{150, 100, 0.10, true},   // over limit -> low
		{10, 100, 0.10, false},   // 90% remaining -> not low
		// Stricter per-provider override (remaining<20% flags).
		{400, 1000, 0.20, false}, // 60% remaining -> not low
		{850, 1000, 0.20, true},  // 15% remaining < 20% -> low
	}
	for _, c := range cases {
		if got := IsLowBalance(c.used, c.limit, c.remainingRatio); got != c.want {
			t.Errorf("IsLowBalance(%d,%d,%.2f)=%v want %v", c.used, c.limit, c.remainingRatio, got, c.want)
		}
	}
}

func TestBuildProviderUsageView(t *testing.T) {
	// Unlimited provider (limits == 0): never low regardless of global ratio.
	v := BuildProviderUsageView(ProviderRecord{Slug: "p1", MonthlyTokenLimit: 0, MonthlyCallLimit: 0}, nil, nil, 0.10, 0.10)
	if !v.TokenUnlimited || !v.CallUnlimited {
		t.Error("expected unlimited flags true")
	}
	if v.TokenRemaining != -1 || v.CallRemaining != -1 {
		t.Errorf("expected -1 remaining, got tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if v.TokenLow || v.CallLow {
		t.Error("unlimited must never be flagged low")
	}

	// Within limit, global remaining ratio 0.10.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p2", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100},
		&ProviderMonthlyUsage{Slug: "p2", TokenUsed: 500, CallUsed: 40}, nil, 0.10, 0.10)
	if v.TokenRemaining != 500 || v.CallRemaining != 60 {
		t.Errorf("remaining wrong: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if v.TokenLow || v.CallLow {
		t.Error("within-limit should not be low")
	}
	if v.TokenUnlimited || v.CallUnlimited {
		t.Error("should be limited")
	}

	// Per-provider override: token ratio 0.20 (remaining<20% flags), call uses global 0.10.
	// Token used 850/1000 = 15% remaining < 20% -> low. Call used 95/100 = 5% remaining <10% -> low.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p3", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100,
		MonthlyTokenLowRatio: 0.20, MonthlyCallLowRatio: 0.10},
		&ProviderMonthlyUsage{Slug: "p3", TokenUsed: 850, CallUsed: 95}, nil, 0.10, 0.10)
	if !v.TokenLow {
		t.Error("p3 token should be low (15% remaining < 20% override)")
	}
	if !v.CallLow {
		t.Error("p3 call should be low (5% remaining < 10% global)")
	}

	// Per-provider independent: token low but call NOT low.
	// Token used 920/1000 = 8% remaining (<10% global) -> low. Call used 50/100 = 50% remaining -> not low.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p4", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100},
		&ProviderMonthlyUsage{Slug: "p4", TokenUsed: 920, CallUsed: 50}, nil, 0.10, 0.10)
	if !v.TokenLow {
		t.Error("p4 token should be low (8% remaining)")
	}
	if v.CallLow {
		t.Error("p4 call should NOT be low (50% remaining)")
	}

	// Over limit -> low + negative remaining.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p5", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100},
		&ProviderMonthlyUsage{Slug: "p5", TokenUsed: 1500, CallUsed: 120}, nil, 0.10, 0.10)
	if v.TokenRemaining != -500 || v.CallRemaining != -20 {
		t.Errorf("over-limit remaining wrong: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if !v.TokenLow || !v.CallLow {
		t.Error("over limit should be flagged low")
	}

	// nil usage -> treats used as zero, remaining == limit.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p6", MonthlyTokenLimit: 100, MonthlyCallLimit: 10}, nil, nil, 0.10, 0.10)
	if v.TokenUsed != 0 || v.CallUsed != 0 {
		t.Error("nil usage should yield zero used")
	}
	if v.TokenRemaining != 100 || v.CallRemaining != 10 {
		t.Errorf("remaining should equal limit: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if v.TokenLow || v.CallLow {
		t.Error("zero usage never low")
	}
}

// TestBuildProviderUsageView_PerProviderIndependence verifies the P2 per-provider
// override resolution: a provider-level ratio > 0 wins over the global default
// (0 means "inherit global"), and token / call dimensions are resolved
// independently. Global remaining threshold is 0.10 (flag when <10% remains).
func TestBuildProviderUsageView_PerProviderIndependence(t *testing.T) {
	const gTok, gCall = 0.10, 0.10

	// Provider A: per-provider TOKEN override = 0.10, CALL inherits global 0.10.
	// Token used 920/1000 = 8% remaining -> low (token 剩8%标红).
	// Call used 80/100 = 20% remaining -> not low (call 剩20%不标红).
	v := BuildProviderUsageView(ProviderRecord{
		Slug: "a", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100,
		MonthlyTokenLowRatio: 0.10, MonthlyCallLowRatio: 0,
	}, &ProviderMonthlyUsage{Slug: "a", TokenUsed: 920, CallUsed: 80}, nil, gTok, gCall)
	if !v.TokenLow {
		t.Error("A: token should be low (8% remaining < 10% override)")
	}
	if v.CallLow {
		t.Error("A: call should NOT be low (20% remaining > 10% global)")
	}

	// Provider B: stricter TOKEN override = 0.05 (flag when <5% remaining).
	// Same token usage 920/1000 = 8% remaining: 8% > 5% override -> NOT low,
	// but the GLOBAL 0.10 would have flagged it. This proves the per-provider
	// override value (not the global default) is what drives token.
	v = BuildProviderUsageView(ProviderRecord{
		Slug: "b", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100,
		MonthlyTokenLowRatio: 0.05, MonthlyCallLowRatio: 0,
	}, &ProviderMonthlyUsage{Slug: "b", TokenUsed: 920, CallUsed: 80}, nil, gTok, gCall)
	if v.TokenLow {
		t.Error("B: token should NOT be low (8% remaining > 5% override); proves override is honoured, not global")
	}
	if v.CallLow {
		t.Error("B: call should NOT be low (20% remaining > 10% global)")
	}

	// Provider C: token inherits global (0), CALL override = 0.05.
	// Token used 920/1000 = 8% remaining -> low under global 0.10.
	// Call used 80/100 = 20% remaining -> not low under call override 0.05.
	// Demonstrates the two dimensions resolve their OWN thresholds.
	v = BuildProviderUsageView(ProviderRecord{
		Slug: "c", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100,
		MonthlyTokenLowRatio: 0, MonthlyCallLowRatio: 0.05,
	}, &ProviderMonthlyUsage{Slug: "c", TokenUsed: 920, CallUsed: 80}, nil, gTok, gCall)
	if !v.TokenLow {
		t.Error("C: token should be low (8% remaining < 10% global)")
	}
	if v.CallLow {
		t.Error("C: call should NOT be low (20% remaining > 5% override)")
	}
}

func TestAggregateProviderUsage(t *testing.T) {
	conn := usageTestDB(t)
	in := time.Now().Add(-1 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	out := time.Now().Add(-40 * 24 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)

	insertLog := func(provider, created string, pt, ct, ec int) {
		if _, err := conn.Exec(
			`INSERT INTO call_logs (user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, status_code, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			1, "m", provider, pt, ct, pt+ct, ec, 200, created,
		); err != nil {
			t.Fatalf("insert log: %v", err)
		}
	}
	insertLog("openai", in, 100, 50, 2)
	insertLog("openai", in, 100, 50, 3)
	insertLog("zhipu", out, 9999, 1, 9) // out of the rolling window

	res, err := AggregateProviderUsage(conn, RollingWindowStart())
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	ou, ok := res["openai"]
	if !ok {
		t.Fatal("openai missing from aggregation")
	}
	if ou.TokenUsed != 300 {
		t.Errorf("openai token_used=%d want 300", ou.TokenUsed)
	}
	if ou.CallUsed != 5 {
		t.Errorf("openai call_used=%d want 5", ou.CallUsed)
	}
	if _, ok := res["zhipu"]; ok {
		t.Error("out-of-window zhipu log must be excluded from aggregation")
	}
}

func TestGetProviderUsage(t *testing.T) {
	conn := usageTestDB(t)
	in := time.Now().Add(-1 * time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	if _, err := conn.Exec(
		`INSERT INTO call_logs (user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, status_code, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "m", "openai", 200, 100, 300, 4, 200, in,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	u, err := GetProviderUsage(conn, "openai", RollingWindowStart())
	if err != nil {
		t.Fatalf("get usage: %v", err)
	}
	if u.TokenUsed != 300 || u.CallUsed != 4 {
		t.Errorf("got tok=%d call=%d want 300/4", u.TokenUsed, u.CallUsed)
	}
}

// TestCurrentCycleWindow validates the fixed 30-day cycle window computation
// for various cycleStartDate inputs.
func TestCurrentCycleWindow(t *testing.T) {
	now := time.Now().In(timeutil.ShanghaiTZ)
	today := now.Format("2006-01-02")

	tests := []struct {
		name          string
		cycleStart    string
		wantStartSame bool // start == cycleStart when today is within first 30 days
	}{
		{"today", today, true},
		{"31 days ago", now.AddDate(0, 0, -31).Format("2006-01-02"), false},
		{"60 days ago", now.AddDate(0, 0, -60).Format("2006-01-02"), false},
		{"empty fallback", "", true},
		{"unparseable fallback", "not-a-date", false}, // fallback to today, won't match the bad input
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end := CurrentCycleWindow(tt.cycleStart)
			if start == "" || end == "" {
				t.Fatal("empty start/end")
			}
			// start must be <= today < end
			if start > today {
				t.Errorf("start=%s > today=%s", start, today)
			}
			if today >= end {
				t.Errorf("today=%s >= end=%s (should be within cycle)", today, end)
			}
			// end is exactly start + 30 days
			st, _ := time.ParseInLocation("2006-01-02", start, timeutil.ShanghaiTZ)
			en, _ := time.ParseInLocation("2006-01-02", end, timeutil.ShanghaiTZ)
			if diff := en.Sub(st).Hours() / 24; diff != 30 {
				t.Errorf("cycle span = %v days, want 30", diff)
			}
			if tt.wantStartSame && start != tt.cycleStart && tt.cycleStart != "" {
				t.Errorf("expected start==cycleStart (%s), got %s", tt.cycleStart, start)
			}
		})
	}

	// Cross-month boundary: verify N increases after 30 days.
	// With cycleStart = "2026-01-15", today is past that.
	start, end := CurrentCycleWindow("2026-01-15")
	st, _ := time.ParseInLocation("2006-01-02", start, timeutil.ShanghaiTZ)
	en, _ := time.ParseInLocation("2006-01-02", end, timeutil.ShanghaiTZ)
	// start should be a multiple of 30 days from 2026-01-15
	anchor, _ := time.ParseInLocation("2006-01-02", "2026-01-15", timeutil.ShanghaiTZ)
	offset := int(st.Sub(anchor).Hours() / 24)
	if offset%30 != 0 {
		t.Errorf("start offset %d days not a multiple of 30", offset)
	}
	if en.Sub(st).Hours()/24 != 30 {
		t.Errorf("span not 30 days")
	}
}

// allocationTestDB creates a test DB with users, quotas, providers, and call_logs
// tables suitable for allocation tests.
func allocationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	conn := usageTestDB(t)

	// Seed a provider so GetProviderAllocation has a fixed_provider to match.
	if _, err := conn.Exec(
		`INSERT INTO providers (name, slug, endpoint, encrypted_key, is_default, enabled, monthly_token_limit, monthly_call_limit, cycle_start_date, created_at, updated_at)
		 VALUES ('test', 'test', 'https://test', X'00', 0, 1, 1000, 100, '2026-01-01', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	return conn
}

// seedAllocUser inserts a user + quota pair for allocation testing.
func seedAllocUser(t *testing.T, conn *sql.DB, username string, fixedProvider, status, expiresAt string, tokenLimit, callLimit int) {
	t.Helper()
	res, err := conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, fixed_provider, expires_at, created_at, updated_at)
		 VALUES (?, 'x', ?, ?, 'user', ?, ?, ?, datetime('now'), datetime('now'))`,
		username, "hash-"+username, "pre-"+username, status, fixedProvider, expiresAt,
	)
	if err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	uid, _ := res.LastInsertId()
	if _, err := conn.Exec(
		`INSERT INTO quotas (user_id, quota_token_total_limit, quota_total_limit, window_start, updated_at)
		 VALUES (?, ?, ?, datetime('now'), datetime('now'))`,
		uid, tokenLimit, callLimit,
	); err != nil {
		t.Fatalf("seed quota for %s: %v", username, err)
	}
}

// TestGetProviderAllocation verifies cross-table allocation aggregation with
// proper 0-semantics (Token: 0=unlimited, Call: 0=locked).
func TestGetProviderAllocation(t *testing.T) {
	conn := allocationTestDB(t)

	// User A: fixed_provider=test, active, not expired, token=1000, call=100.
	seedAllocUser(t, conn, "a", "test", "active", "", 1000, 100)
	// User B: fixed_provider=test, active, token=0 (unlimited), call=50.
	seedAllocUser(t, conn, "b", "test", "active", "", 0, 50)
	// User C: fixed_provider=other (should be excluded).
	seedAllocUser(t, conn, "c", "other", "active", "", 500, 50)
	// User D: fixed_provider=test, disabled (should be excluded).
	seedAllocUser(t, conn, "d", "test", "disabled", "", 500, 50)
	// User E: fixed_provider=test, active, expired yesterday.
	yesterday := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	seedAllocUser(t, conn, "e", "test", "active", yesterday, 500, 50)
	// User F: fixed_provider=test, active, token=0, call=0 (token=unlimited, call=locked).
	seedAllocUser(t, conn, "f", "test", "active", "", 0, 0)

	alloc, err := GetProviderAllocation(conn, "test")
	if err != nil {
		t.Fatalf("GetProviderAllocation: %v", err)
	}

	// allocated_tokens: A(1000) + B(0, excluded) + F(0, excluded) = 1000
	if alloc.AllocatedTokens != 1000 {
		t.Errorf("allocated_tokens=%d want 1000", alloc.AllocatedTokens)
	}
	// allocated_calls: A(100) + B(50) + F(0, excluded) = 150
	if alloc.AllocatedCalls != 150 {
		t.Errorf("allocated_calls=%d want 150", alloc.AllocatedCalls)
	}
	// unlimited_user_count: B(token=0) + F(token=0) = 2
	if alloc.UnlimitedUserCount != 2 {
		t.Errorf("unlimited_user_count=%d want 2", alloc.UnlimitedUserCount)
	}
}

// TestGetProviderAllocation_NoUsers verifies that a provider with no fixed users
// returns zeros across the board (not an error).
func TestGetProviderAllocation_NoUsers(t *testing.T) {
	conn := allocationTestDB(t)
	alloc, err := GetProviderAllocation(conn, "test")
	if err != nil {
		t.Fatalf("GetProviderAllocation(no users): %v", err)
	}
	if alloc.AllocatedTokens != 0 || alloc.AllocatedCalls != 0 || alloc.UnlimitedUserCount != 0 {
		t.Errorf("expected all-zero allocation, got %+v", alloc)
	}
}

// TestGetProviderAllocationDetails verifies the per-user breakdown: it returns
// only active, non-expired, fixed-provider-matched users, and that cycle-window
// usage is summed correctly (in-cycle rows counted, out-of-cycle rows excluded).
func TestGetProviderAllocationDetails(t *testing.T) {
	conn := allocationTestDB(t)

	seedAllocUser(t, conn, "a", "test", "active", "", 1000, 100)
	seedAllocUser(t, conn, "b", "test", "active", "", 0, 50)
	seedAllocUser(t, conn, "c", "other", "active", "", 500, 50)
	seedAllocUser(t, conn, "d", "test", "disabled", "", 500, 50)
	yesterday := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	seedAllocUser(t, conn, "e", "test", "active", yesterday, 500, 50)

	// Compute the cycle window exactly like the handler does.
	cycleStart, _ := CurrentCycleWindow("2026-01-01")
	windowStart := cycleStart + "T00:00:00+08:00"
	// Out-of-cycle timestamp: 2 days before the window start.
	ws, _ := time.Parse(time.RFC3339, windowStart)
	outOfCycle := ws.Add(-48 * time.Hour).Format(time.RFC3339)

	// Seed call_logs for user "a".
	var uidA int64
	if err := conn.QueryRow(`SELECT id FROM users WHERE username = 'a'`).Scan(&uidA); err != nil {
		t.Fatalf("find user a: %v", err)
	}
	execLog := func(uid int64, when string, pt, ct, calls int, mult ...float64) {
		m := 1.0
		if len(mult) > 0 {
			m = mult[0]
		}
		if _, err := conn.Exec(
			`INSERT INTO call_logs (user_id, provider_id, model, prompt_tokens, completion_tokens, effective_calls, status_code, created_at, multiplier_used)
			 VALUES (?, 'test', 'glm', ?, ?, ?, 200, ?, ?)`,
			uid, pt, ct, calls, when, m,
		); err != nil {
			t.Fatalf("seed call_log: %v", err)
		}
	}
	execLog(uidA, windowStart, 100, 20, 3)     // in-cycle, mult 1.0 → 120
	execLog(uidA, windowStart, 50, 50, 2, 2.0) // in-cycle, mult 2.0 → ceil(100*2)=200
	execLog(uidA, outOfCycle, 999, 999, 9)    // out-of-cycle → excluded

	details, err := GetProviderAllocationDetails(conn, "test", windowStart)
	if err != nil {
		t.Fatalf("GetProviderAllocationDetails: %v", err)
	}

	// Only a (active) and b (active) are included; c/d/e excluded.
	if len(details) != 2 {
		t.Fatalf("detail rows = %d, want 2 (a,b); got %+v", len(details), details)
	}

	// Sorted by token_used DESC, then username → a first.
	a, b := details[0], details[1]
	if a.Username != "a" {
		t.Fatalf("details[0].username = %q, want a", a.Username)
	}
	if b.Username != "b" {
		t.Fatalf("details[1].username = %q, want b", b.Username)
	}
	// Cycle-window usage, billed (multiplier-scaled): 120@1.0 + 200@2.0 = 320 tokens, 5 calls.
	if a.TokenUsed != 320 {
		t.Errorf("a.TokenUsed = %d, want 320 (120@1.0 + 200@2.0)", a.TokenUsed)
	}
	if a.CallUsed != 5 {
		t.Errorf("a.CallUsed = %d, want 5 (3+2)", a.CallUsed)
	}
	if a.QuotaTokenTotalLimit != 1000 {
		t.Errorf("a.QuotaTokenTotalLimit = %d, want 1000", a.QuotaTokenTotalLimit)
	}
	// b: token=0 (unlimited), no call_logs → 0 usage.
	if b.QuotaTokenTotalLimit != 0 {
		t.Errorf("b.QuotaTokenTotalLimit = %d, want 0 (unlimited)", b.QuotaTokenTotalLimit)
	}
	if b.TokenUsed != 0 || b.CallUsed != 0 {
		t.Errorf("b usage = %d/%d, want 0/0", b.TokenUsed, b.CallUsed)
	}

	// A provider with no fixed users returns an empty (non-nil) slice.
	none, err := GetProviderAllocationDetails(conn, "nope", windowStart)
	if err != nil {
		t.Fatalf("GetProviderAllocationDetails(nope): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("details for unknown provider = %d rows, want 0", len(none))
	}
}

// TestAllocationLow verifies that AllocationLow is computed independently of
// consumption low flags, using the same IsLowBalance logic.
func TestAllocationLow(t *testing.T) {
	// Provider with limit 1000, global threshold 0.10.
	// Allocated 950 → 5% remaining → IsLowBalance(950, 1000, 0.10) = true.
	p := ProviderRecord{
		Slug: "test", Name: "Test",
		MonthlyTokenLimit: 1000,
		MonthlyCallLimit:  100,
	}
	alloc := &ProviderAllocation{
		AllocatedTokens: 950,
		AllocatedCalls:  10,
	}
	view := BuildProviderUsageView(p, nil, alloc, 0.10, 0.10)
	if !view.AllocationLow {
		t.Error("950/1000 tokens (5% remaining) should trigger AllocationLow")
	}
	if view.TokenLow || view.CallLow {
		t.Error("nil usage should not trigger TokenLow/CallLow")
	}

	// Allocated within threshold: 500/1000 tokens (50% remaining) → not low.
	alloc2 := &ProviderAllocation{AllocatedTokens: 500, AllocatedCalls: 10}
	view2 := BuildProviderUsageView(p, nil, alloc2, 0.10, 0.10)
	if view2.AllocationLow {
		t.Error("500/1000 tokens should NOT trigger AllocationLow")
	}

	// Call dimension triggers AllocationLow: tokens fine but calls over.
	alloc3 := &ProviderAllocation{AllocatedTokens: 500, AllocatedCalls: 95}
	view3 := BuildProviderUsageView(p, nil, alloc3, 0.10, 0.10)
	if !view3.AllocationLow {
		t.Error("95/100 calls (5% remaining) should trigger AllocationLow via call dimension")
	}

	// Unlimited provider (limit <= 0): never low.
	pUnlimited := ProviderRecord{Slug: "u", Name: "U", MonthlyTokenLimit: 0, MonthlyCallLimit: 0}
	allocU := &ProviderAllocation{AllocatedTokens: 99999, AllocatedCalls: 99999}
	viewU := BuildProviderUsageView(pUnlimited, nil, allocU, 0.10, 0.10)
	if viewU.AllocationLow {
		t.Error("unlimited provider must never be flagged allocation-low")
	}
}

// TestBuildProviderUsageView_CycleInfo verifies that cycle start/end and
// days remaining are populated correctly.
func TestBuildProviderUsageView_CycleInfo(t *testing.T) {
	now := time.Now().In(timeutil.ShanghaiTZ)
	today := now.Format("2006-01-02")

	p := ProviderRecord{
		Slug: "test", Name: "Test",
		CycleStartDate:    today,
		MonthlyTokenLimit: 1000,
		MonthlyCallLimit:  100,
	}
	view := BuildProviderUsageView(p, nil, nil, 0.10, 0.10)

	if view.CycleStart != today {
		t.Errorf("CycleStart=%s want %s", view.CycleStart, today)
	}
	if view.CycleEnd == "" || view.CycleEnd <= today {
		t.Errorf("CycleEnd=%s should be after today %s", view.CycleEnd, today)
	}
	if view.CycleDaysRemaining < 0 || view.CycleDaysRemaining > 30 {
		t.Errorf("CycleDaysRemaining=%d out of range [0,30]", view.CycleDaysRemaining)
	}
	// WindowStart should match CycleStart.
	if view.WindowStart != view.CycleStart {
		t.Errorf("WindowStart=%s != CycleStart=%s", view.WindowStart, view.CycleStart)
	}
}

// TestGetAutoUserAllocationByProvider verifies the shared-pool attribution:
// auto users' finite quotas are split across the providers they actually used
// (by billed-usage share, no double-count), zero-usage auto users stay in the
// pool, and unlimited auto users contribute real usage but no finite quota.
func TestGetAutoUserAllocationByProvider(t *testing.T) {
	conn := usageTestDB(t)

	// Two providers for auto traffic to land on.
	for _, slug := range []string{"p-a", "p-b"} {
		if _, err := conn.Exec(
			`INSERT INTO providers (name, slug, endpoint, encrypted_key, is_default, enabled, monthly_token_limit, monthly_call_limit, cycle_start_date, created_at, updated_at)
			 VALUES (?, ?, 'https://x', X'00', 0, 1, 100000, 100000, '2026-01-01', datetime('now'), datetime('now'))`,
			slug, slug,
		); err != nil {
			t.Fatalf("seed provider %s: %v", slug, err)
		}
	}

	seedAuto := func(username string, tokenLimit, callLimit int) int64 {
		res, err := conn.Exec(
			`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, route_mode, created_at, updated_at)
			 VALUES (?, 'x', ?, 'p', 'user', 'active', 'auto', datetime('now'), datetime('now'))`,
			username, "h-"+username,
		)
		if err != nil {
			t.Fatalf("seed auto user %s: %v", username, err)
		}
		uid, _ := res.LastInsertId()
		if _, err := conn.Exec(
			`INSERT INTO quotas (user_id, quota_token_total_limit, quota_total_limit, window_start, updated_at)
			 VALUES (?, ?, ?, datetime('now'), datetime('now'))`,
			uid, tokenLimit, callLimit,
		); err != nil {
			t.Fatalf("seed quota %s: %v", username, err)
		}
		return uid
	}
	seedLog := func(uid int64, provider, when string, pt, ct, calls int, mult float64) {
		if _, err := conn.Exec(
			`INSERT INTO call_logs (user_id, provider_id, model, prompt_tokens, completion_tokens, effective_calls, status_code, created_at, multiplier_used)
			 VALUES (?, ?, 'glm', ?, ?, ?, 200, ?, ?)`,
			uid, provider, pt, ct, calls, when, mult,
		); err != nil {
			t.Fatalf("seed auto call_log: %v", err)
		}
	}

	now := time.Now().Format(time.RFC3339) // within the rolling-30d window
	// u1: finite quota 1000/100, traffic split 60% p-a / 40% p-b (billed tokens).
	uid1 := seedAuto("auto1", 1000, 100)
	seedLog(uid1, "p-a", now, 60, 0, 6, 1.0) // billed 60
	seedLog(uid1, "p-b", now, 40, 0, 4, 1.0) // billed 40
	// u2: finite quota 500/50, ZERO usage → stays in the shared pool.
	seedAuto("auto2", 500, 50)
	// u3: token-unlimited (0) + call-locked (0), with mult-2.0 traffic on p-a.
	uid3 := seedAuto("auto3", 0, 0)
	seedLog(uid3, "p-a", now, 100, 0, 10, 2.0) // billed = ceil(100*2.0)=200

	res, err := GetAutoUserAllocationByProvider(conn, RollingWindowStart())
	if err != nil {
		t.Fatalf("GetAutoUserAllocationByProvider: %v", err)
	}

	// Pool totals count only FINITE quotas (u1 1000 + u2 500 = 1500 token;
	// 100 + 50 = 150 call). u3 is token-unlimited (0) + call-locked (0) → excluded.
	if res.PoolTokenTotal != 1500 {
		t.Errorf("PoolTokenTotal = %d, want 1500", res.PoolTokenTotal)
	}
	if res.PoolCallTotal != 150 {
		t.Errorf("PoolCallTotal = %d, want 150", res.PoolCallTotal)
	}
	if res.UnlimitedUserCount != 1 {
		t.Errorf("UnlimitedUserCount = %d, want 1 (auto3)", res.UnlimitedUserCount)
	}

	bySlug := map[string]AutoProviderLoad{}
	for _, l := range res.ByProvider {
		bySlug[l.ProviderSlug] = l
	}
	if len(bySlug) != 2 {
		t.Fatalf("ByProvider len = %d, want 2 (p-a, p-b)", len(bySlug))
	}

	// p-a: u1 share 60% of 1000 = 600 token, 60% of 100 = 60 call;
	//      + u3 real usage (no quota share since unlimited/locked): 200 token, 10 call.
	pa := bySlug["p-a"]
	if pa.TokenQuotaShare != 600 {
		t.Errorf("p-a TokenQuotaShare = %d, want 600", pa.TokenQuotaShare)
	}
	if pa.CallQuotaShare != 60 {
		t.Errorf("p-a CallQuotaShare = %d, want 60", pa.CallQuotaShare)
	}
	if pa.AutoTokenUsage != 260 { // 60 (u1) + 200 (u3)
		t.Errorf("p-a AutoTokenUsage = %d, want 260", pa.AutoTokenUsage)
	}
	if pa.AutoCallUsage != 16 { // 6 (u1) + 10 (u3)
		t.Errorf("p-a AutoCallUsage = %d, want 16", pa.AutoCallUsage)
	}

	// p-b: u1 share 40% of 1000 = 400 token, 40% of 100 = 40 call.
	pb := bySlug["p-b"]
	if pb.TokenQuotaShare != 400 {
		t.Errorf("p-b TokenQuotaShare = %d, want 400", pb.TokenQuotaShare)
	}
	if pb.CallQuotaShare != 40 {
		t.Errorf("p-b CallQuotaShare = %d, want 40", pb.CallQuotaShare)
	}
	if pb.AutoTokenUsage != 40 {
		t.Errorf("p-b AutoTokenUsage = %d, want 40", pb.AutoTokenUsage)
	}
	if pb.AutoCallUsage != 4 {
		t.Errorf("p-b AutoCallUsage = %d, want 4", pb.AutoCallUsage)
	}

	// No double-count: attributed quota across providers == sum of finite
	// quotas of users who actually generated usage (u1=1000 token/100 call;
	// u2 zero-usage stays in pool; u3 unlimited → 0). And never exceeds pool.
	var sumTok, sumCall int64
	for _, l := range res.ByProvider {
		sumTok += l.TokenQuotaShare
		sumCall += l.CallQuotaShare
	}
	if sumTok != 1000 {
		t.Errorf("attributed token = %d, want 1000 (only used users)", sumTok)
	}
	if sumCall != 100 {
		t.Errorf("attributed call = %d, want 100", sumCall)
	}
	if sumTok > res.PoolTokenTotal || sumCall > res.PoolCallTotal {
		t.Errorf("attributed (%d/%d) exceeds pool (%d/%d) — double-count?",
			sumTok, sumCall, res.PoolTokenTotal, res.PoolCallTotal)
	}
}

// TestGetAutoUserAllocationByProvider_NoUsers verifies that with no auto users
// (or none with quotas) the result is empty and the pool is zero — not an error.
func TestGetAutoUserAllocationByProvider_NoUsers(t *testing.T) {
	conn := usageTestDB(t)
	res, err := GetAutoUserAllocationByProvider(conn, RollingWindowStart())
	if err != nil {
		t.Fatalf("GetAutoUserAllocationByProvider(no users): %v", err)
	}
	if len(res.ByProvider) != 0 {
		t.Errorf("ByProvider = %d rows, want 0", len(res.ByProvider))
	}
	if res.PoolTokenTotal != 0 || res.PoolCallTotal != 0 || res.UnlimitedUserCount != 0 {
		t.Errorf("expected zero pool, got %+v", res)
	}
}
