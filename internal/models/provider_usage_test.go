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
	cases := []struct {
		used, limit int64
		want        bool
	}{
		{0, 0, false},   // unlimited never low
		{0, 100, false}, // zero usage never low
		{89, 100, false},
		{90, 100, true}, // ratio exactly 0.9 -> low
		{100, 100, true},
		{900, 1000, true},
		{150, 100, true}, // over limit
		{10, 100, false},
	}
	for _, c := range cases {
		if got := IsLowBalance(c.used, c.limit); got != c.want {
			t.Errorf("IsLowBalance(%d,%d)=%v want %v", c.used, c.limit, got, c.want)
		}
	}
}

func TestBuildProviderUsageView(t *testing.T) {
	// Unlimited provider (limits == 0).
	v := BuildProviderUsageView(ProviderRecord{Slug: "p1", MonthlyTokenLimit: 0, MonthlyCallLimit: 0}, nil, "2026-01-01T00:00:00+08:00")
	if !v.TokenUnlimited || !v.CallUnlimited {
		t.Error("expected unlimited flags true")
	}
	if v.TokenRemaining != -1 || v.CallRemaining != -1 {
		t.Errorf("expected -1 remaining, got tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if v.TokenLow || v.CallLow {
		t.Error("unlimited must never be flagged low")
	}

	// Within limit.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p2", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100},
		&ProviderMonthlyUsage{Slug: "p2", TokenUsed: 500, CallUsed: 40}, "w")
	if v.TokenRemaining != 500 || v.CallRemaining != 60 {
		t.Errorf("remaining wrong: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if v.TokenLow || v.CallLow {
		t.Error("within-limit should not be low")
	}
	if v.TokenUnlimited || v.CallUnlimited {
		t.Error("should be limited")
	}

	// Over limit -> low + negative remaining.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p3", MonthlyTokenLimit: 1000, MonthlyCallLimit: 100},
		&ProviderMonthlyUsage{Slug: "p3", TokenUsed: 1500, CallUsed: 120}, "w")
	if v.TokenRemaining != -500 || v.CallRemaining != -20 {
		t.Errorf("over-limit remaining wrong: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
	}
	if !v.TokenLow || !v.CallLow {
		t.Error("over limit should be flagged low")
	}

	// nil usage -> treats used as zero, remaining == limit.
	v = BuildProviderUsageView(ProviderRecord{Slug: "p4", MonthlyTokenLimit: 100, MonthlyCallLimit: 10}, nil, "w")
	if v.TokenUsed != 0 || v.CallUsed != 0 {
		t.Error("nil usage should yield zero used")
	}
	if v.TokenRemaining != 100 || v.CallRemaining != 10 {
		t.Errorf("remaining should equal limit: tok=%d call=%d", v.TokenRemaining, v.CallRemaining)
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
