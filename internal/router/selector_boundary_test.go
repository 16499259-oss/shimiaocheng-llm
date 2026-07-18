// Package router contains additional boundary tests for the routing selector,
// focused on the time-window edges, overnight windows, the strict no-fallback
// invariant, and model-rewrite corner cases.
package router

import (
	"os"
	"sync"
	"testing"
	"time"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/timeutil"
)

// TestResolveProvider_SeedWindowBoundaries validates the seeded rule
// 14:00-18:01 -> openai at exact second-level boundaries. The window is
// half-open [14:00, 18:01): 14:00 included, 18:01 excluded.
func TestResolveProvider_SeedWindowBoundaries(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	cases := []struct {
		name     string
		now      time.Time
		wantProv string
	}{
		{"13:59:59_before", time.Date(2026, 1, 1, 13, 59, 59, 0, timeutil.ShanghaiTZ), "zhipu"},
		{"14:00:00_start", time.Date(2026, 1, 1, 14, 0, 0, 0, timeutil.ShanghaiTZ), "openai"},
		{"18:00:59_last", time.Date(2026, 1, 1, 18, 0, 59, 0, timeutil.ShanghaiTZ), "openai"},
		{"18:01:00_end", time.Date(2026, 1, 1, 18, 1, 0, 0, timeutil.ShanghaiTZ), "zhipu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov, err := r.ResolveProvider(c.now)
			if err != nil {
				t.Fatalf("%s: ResolveProvider error: %v", c.name, err)
			}
			if prov.ID != c.wantProv {
				t.Fatalf("%s: want provider %s, got %s", c.name, c.wantProv, prov.ID)
			}
		})
	}
}

// TestResolveProvider_OvernightRoutingRule validates an overnight window that
// wraps past midnight (23:00-01:00). 23:30 and 00:30 are inside; 01:00 is the
// excluded upper bound; 22:59 is outside.
func TestResolveProvider_OvernightRoutingRule(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	if _, err := database.Conn.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"openai", "23:00", "01:00", "*", "Asia/Shanghai", 1,
	); err != nil {
		t.Fatalf("insert overnight rule: %v", err)
	}
	// Reload to pick up the new rule.
	if err := r.Reload(); err != nil {
		t.Fatalf("reload after insert overnight rule: %v", err)
	}

	cases := []struct {
		name     string
		now      time.Time
		wantProv string
	}{
		{"23:30_inside", time.Date(2026, 1, 1, 23, 30, 0, 0, timeutil.ShanghaiTZ), "openai"},
		{"00:30_inside", time.Date(2026, 1, 2, 0, 30, 0, 0, timeutil.ShanghaiTZ), "openai"},
		{"01:00:00_end", time.Date(2026, 1, 2, 1, 0, 0, 0, timeutil.ShanghaiTZ), "zhipu"},
		{"22:59_outside", time.Date(2026, 1, 1, 22, 59, 0, 0, timeutil.ShanghaiTZ), "zhipu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prov, err := r.ResolveProvider(c.now)
			if err != nil {
				t.Fatalf("%s: ResolveProvider error: %v", c.name, err)
			}
			if prov.ID != c.wantProv {
				t.Fatalf("%s: want provider %s, got %s", c.name, c.wantProv, prov.ID)
			}
		})
	}
}

// TestResolveProvider_NoFallbackWhenTargetHasNoKey verifies the strict
// no-fallback invariant at the routing layer: even when the window-matched
// target provider has NO credential (simulating "down"/unconfigured), the
// router still resolves to that target and never silently downgrades to the
// default provider.
func TestResolveProvider_NoFallbackWhenTargetHasNoKey(t *testing.T) {
	database := newRouterTestDB(t)
	// Build a config where openai has an empty API key env var.
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
			{ID: "openai", Endpoint: "https://openai.example/v1", APIKeyEnv: "OPENAI_EMPTY_KEY", IsDefault: false},
		},
	}
	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")
	os.Unsetenv("OPENAI_EMPTY_KEY") // intentionally empty

	r := newRouterWithConfig(t, database, cfg)

	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ) // inside seed window
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider error: %v", err)
	}
	if prov.ID != "openai" {
		t.Fatalf("strict no-fallback violated: window matched openai but resolved to %s", prov.ID)
	}
	if prov.APIKey != "" {
		t.Fatalf("expected empty openai key (simulated down), got %q", prov.APIKey)
	}
}

// TestRewriteModel_CaseSensitivity verifies that external model names are
// matched case-insensitively (e.g. "GLM-5.2" matches stored "glm-5.2"), while
// provider IDs remain case-sensitive.
func TestRewriteModel_CaseSensitivity(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	// External model name case-insensitive: "GLM-5.2" -> "glm-5.2" -> openai -> "gpt-4o".
	if got := r.RewriteModel("GLM-5.2", "openai"); got != "gpt-4o" {
		t.Fatalf("external case-insensitive match should resolve to gpt-4o, got %q", got)
	}
	// Provider IDs remain case-sensitive: "OPENAI" != "openai".
	if got := r.RewriteModel("glm-5.2", "OPENAI"); got != "glm-5.2" {
		t.Fatalf("provider-id case mismatch should passthrough, got %q", got)
	}
}

// TestRewriteModel_EmptyConfigPassthrough verifies that with no model mappings
// configured, every external name passes through unchanged.
func TestRewriteModel_EmptyConfigPassthrough(t *testing.T) {
	database := newRouterTestDB(t)
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "openai", Endpoint: "https://openai.example/v1", APIKeyEnv: "OPENAI_API_KEY", IsDefault: true},
	}}
	os.Setenv("OPENAI_API_KEY", "sk-openai")
	defer os.Unsetenv("OPENAI_API_KEY")
	r := newRouterWithConfig(t, database, cfg)

	if got := r.RewriteModel("anything", "openai"); got != "anything" {
		t.Fatalf("empty model mappings should passthrough, got %q", got)
	}
}

// TestRewriteModel_EmptyRealModelPassthrough verifies the `real != ""` guard:
// an explicitly empty per-provider real model yields passthrough.
func TestRewriteModel_EmptyRealModelPassthrough(t *testing.T) {
	database := newRouterTestDB(t)
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "openai", Endpoint: "https://openai.example/v1", APIKeyEnv: "OPENAI_API_KEY", IsDefault: true},
		},
		ModelMappings: []config.ModelMapping{
			{External: "m", PerProvider: map[string]string{"openai": ""}},
		},
	}
	os.Setenv("OPENAI_API_KEY", "sk-openai")
	defer os.Unsetenv("OPENAI_API_KEY")
	r := newRouterWithConfig(t, database, cfg)

	if got := r.RewriteModel("m", "openai"); got != "m" {
		t.Fatalf("empty real model should passthrough, got %q", got)
	}
}

// TestCredentialStore_ConcurrentAccess is a race-detector smoke test for the
// thread-safe credential store (RWMutex path). Run with -race to catch races.
func TestCredentialStore_ConcurrentAccess(t *testing.T) {
	store := NewCredentialStore()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			store.Set("p", "k"+string(rune('0'+n%10)))
			_ = store.Get("p")
			_ = store.Holder("p").Get()
		}(i)
	}
	wg.Wait()
}

// TestResolveProvider_WindowHitProviderNotConfigured is the REPRODUCTION for a
// P1 finding against the strict no-fallback invariant (AGENTS.md §6:
// "命中即走，绝不回退"). When a time window matches a provider that is NOT
// present in the configured providers, the router returns an error.
func TestResolveProvider_WindowHitProviderNotConfigured(t *testing.T) {
	database := newRouterTestDB(t) // seeds 14:00-18:01 -> openai

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")
	r := newRouterWithConfig(t, database, cfg)

	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ) // inside seed window
	prov, err := r.ResolveProvider(now)

	// DESIRED: a window that matched openai (not configured) must NOT silently
	// resolve to zhipu. It should error.
	if err == nil && prov.ID == "zhipu" {
		t.Fatalf("P1 reproduction: window matched 'openai' (not configured) but router silently fell back to default %q; expected an error / no silent downgrade (AGENTS.md §6 strict no-fallback)", prov.ID)
	}
}

// TestResolveProvider_OverlappingRules_NarrowerWins is the REPRODUCTION of the
// production bug: an admin added a narrower override rule (xunfei 00:01-10:00)
// that overlaps an older, broader base rule (zhipu 00:01-12:29). With the old
// first-match-by-id behaviour the broader (lower-id) rule shadowed the override
// and auto users silently stayed on zhipu. The fix sorts by (priority DESC,
// narrower-window-first, id ASC) so the narrower override wins within the
// overlap without any manual reordering.
func TestResolveProvider_OverlappingRules_NarrowerWins(t *testing.T) {
	database := newRouterTestDB(t)
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
			{ID: "xunfei", Endpoint: "https://xunfei.example/v1", APIKeyEnv: "XUNFEI_API_KEY", IsDefault: false},
		},
	}
	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	os.Setenv("XUNFEI_API_KEY", "sk-xunfei")
	defer os.Unsetenv("ZHIPU_API_KEY")
	defer os.Unsetenv("XUNFEI_API_KEY")
	r := newRouterWithConfig(t, database, cfg)

	// Insert the two overlapping rules in id order: base (zhipu, broader) first,
	// override (xunfei, narrower) second — mirroring the production scenario.
	if _, err := database.Conn.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"zhipu", "00:01", "12:29", "*", "Asia/Shanghai", 1, 0,
	); err != nil {
		t.Fatalf("insert base rule: %v", err)
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"xunfei", "00:01", "10:00", "*", "Asia/Shanghai", 1, 0,
	); err != nil {
		t.Fatalf("insert override rule: %v", err)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("reload after insert: %v", err)
	}

	// Inside the overlap (09:00) the narrower xunfei rule must win.
	at0900 := time.Date(2026, 1, 1, 9, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(at0900)
	if err != nil {
		t.Fatalf("ResolveProvider(09:00) error: %v", err)
	}
	if prov.ID != "xunfei" {
		t.Fatalf("overlap @09:00: expected narrower override 'xunfei', got %q", prov.ID)
	}

	// Outside the override but inside the base window (11:00) -> base zhipu.
	at1100 := time.Date(2026, 1, 1, 11, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err = r.ResolveProvider(at1100)
	if err != nil {
		t.Fatalf("ResolveProvider(11:00) error: %v", err)
	}
	if prov.ID != "zhipu" {
		t.Fatalf("outside override @11:00: expected base 'zhipu', got %q", prov.ID)
	}
}

// TestResolveProvider_PriorityOverridesNarrower verifies that an explicit
// higher priority beats the narrower-window tiebreak — giving admins clear
// control when they DO want the broader rule to win.
func TestResolveProvider_PriorityOverridesNarrower(t *testing.T) {
	database := newRouterTestDB(t)
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
			{ID: "xunfei", Endpoint: "https://xunfei.example/v1", APIKeyEnv: "XUNFEI_API_KEY", IsDefault: false},
		},
	}
	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	os.Setenv("XUNFEI_API_KEY", "sk-xunfei")
	defer os.Unsetenv("ZHIPU_API_KEY")
	defer os.Unsetenv("XUNFEI_API_KEY")
	r := newRouterWithConfig(t, database, cfg)

	// Base zhipu (broader) gets priority 10; override xunfei (narrower) stays 0.
	if _, err := database.Conn.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"zhipu", "00:01", "12:29", "*", "Asia/Shanghai", 1, 10,
	); err != nil {
		t.Fatalf("insert base rule: %v", err)
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO provider_routing_rules (provider_id, start_time, end_time, days_of_week, timezone, enabled, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"xunfei", "00:01", "10:00", "*", "Asia/Shanghai", 1, 0,
	); err != nil {
		t.Fatalf("insert override rule: %v", err)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("reload after insert: %v", err)
	}

	// Even though xunfei is narrower, zhipu's higher priority wins at 09:00.
	at0900 := time.Date(2026, 1, 1, 9, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(at0900)
	if err != nil {
		t.Fatalf("ResolveProvider(09:00) error: %v", err)
	}
	if prov.ID != "zhipu" {
		t.Fatalf("priority should beat narrower window @09:00: expected 'zhipu', got %q", prov.ID)
	}
}
