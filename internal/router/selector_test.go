// Package router_test contains tests for the routing selector.
package router

import (
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/timeutil"
)

// newRouterTestDB opens an isolated temp-file SQLite database, runs migrations,
// and registers cleanup. The migrations seed a 14:00-18:01 -> openai rule.
func newRouterTestDB(t *testing.T) *db.DB {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "router_test_*.db")
	if err != nil {
		t.Fatalf("failed to create temp db file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp db file: %v", err)
	}

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	t.Cleanup(func() { _ = database.Close() })
	return database
}

// testConfig returns a config with both zhipu (default) and openai providers,
// plus a glm-5.2 mapping.
func testConfig() *config.Config {
	return &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
			{ID: "openai", Endpoint: "https://openai.example/v1", APIKeyEnv: "OPENAI_API_KEY", IsDefault: false},
		},
		ModelMappings: []config.ModelMapping{
			{
				External: "glm-5.2",
				PerProvider: map[string]string{
					"zhipu":  "glm-5.2",
					"openai": "gpt-4o",
				},
			},
		},
	}
}

func TestNewRouter_DefaultProvider(t *testing.T) {
	database := newRouterTestDB(t)
	creds := NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	creds.Set("openai", "sk-openai")

	r := NewRouter(database.Conn, testConfig(), creds)

	// Outside the 14:00-18:01 window -> default (zhipu) provider, with creds.
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider returned error: %v", err)
	}
	if prov.ID != "zhipu" {
		t.Fatalf("expected default provider zhipu, got %q", prov.ID)
	}
	if prov.Endpoint != "https://zhipu.example/v1" {
		t.Fatalf("unexpected endpoint: %q", prov.Endpoint)
	}
	if prov.APIKey != "sk-zhipu" {
		t.Fatalf("expected zhipu key from credential store, got %q", prov.APIKey)
	}
}

func TestResolveProvider_WindowHitReturnsB(t *testing.T) {
	database := newRouterTestDB(t)
	creds := NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	creds.Set("openai", "sk-openai")

	r := NewRouter(database.Conn, testConfig(), creds)

	// 15:00 Asia/Shanghai matches the seeded 14:00-18:01 -> openai rule.
	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider returned error: %v", err)
	}
	if prov.ID != "openai" {
		t.Fatalf("window hit should resolve to openai, got %q (must NOT fall back to zhipu/A)", prov.ID)
	}
	if prov.APIKey != "sk-openai" {
		t.Fatalf("expected openai key, got %q", prov.APIKey)
	}
}

func TestResolveProvider_NoProvidersConfigured(t *testing.T) {
	// No providers at all -> error (not a silent default).
	r := NewRouter(nil, &config.Config{}, NewCredentialStore())
	_, err := r.ResolveProvider(time.Now())
	if err == nil {
		t.Fatalf("expected error when no providers configured")
	}
}

func TestResolveProvider_DBFailureFallsBackToDefault(t *testing.T) {
	// nil DB -> loadRules returns nil -> fall back to default provider, no panic.
	creds := NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	r := NewRouter(nil, testConfig(), creds)

	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ) // would hit openai if DB worked
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("expected fallback to default, got error: %v", err)
	}
	if prov.ID != "zhipu" {
		t.Fatalf("DB failure must fall back to default zhipu, got %q", prov.ID)
	}
}

func TestRewriteModel_Mapping(t *testing.T) {
	r := NewRouter(nil, testConfig(), NewCredentialStore())

	if got := r.RewriteModel("glm-5.2", "openai"); got != "gpt-4o" {
		t.Fatalf("RewriteModel(glm-5.2, openai) = %q, want %q", got, "gpt-4o")
	}
	if got := r.RewriteModel("glm-5.2", "zhipu"); got != "glm-5.2" {
		t.Fatalf("RewriteModel(glm-5.2, zhipu) = %q, want %q", got, "glm-5.2")
	}
}

func TestRewriteModel_Passthrough(t *testing.T) {
	r := NewRouter(nil, testConfig(), NewCredentialStore())

	// Unknown external model -> returned unchanged (no error).
	if got := r.RewriteModel("some-new-model", "openai"); got != "some-new-model" {
		t.Fatalf("RewriteModel should passthrough unknown model, got %q", got)
	}
	// Known external but missing per-provider mapping -> passthrough.
	if got := r.RewriteModel("glm-5.2", "anthropic"); got != "glm-5.2" {
		t.Fatalf("RewriteModel missing provider mapping should passthrough, got %q", got)
	}
}

func TestCredentialStore_SharedHolder(t *testing.T) {
	store := NewCredentialStore()

	// Same holder instance is returned for a given provider id.
	h1 := store.Holder("zhipu")
	h2 := store.Holder("zhipu")
	if h1 != h2 {
		t.Fatalf("Holder must return the same instance for the same provider id")
	}

	store.Set("zhipu", "k1")
	if store.Get("zhipu") != "k1" {
		t.Fatalf("expected k1, got %q", store.Get("zhipu"))
	}
	// Admin-style hot update through the shared holder.
	store.Holder("zhipu").Set("k2")
	if store.Get("zhipu") != "k2" {
		t.Fatalf("expected shared holder update to k2, got %q", store.Get("zhipu"))
	}
}

func TestTimeZoneLock_IsInRange(t *testing.T) {
	// 15:00 Asia/Shanghai is in [14:00, 18:00).
	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ)
	if !timeutil.IsInRange("14:00", "18:00", now) {
		t.Fatalf("expected 15:00 to be in [14:00, 18:00)")
	}

	// 18:00 is excluded (half-open upper bound).
	eighteen := time.Date(2026, 1, 1, 18, 0, 0, 0, timeutil.ShanghaiTZ)
	if timeutil.IsInRange("14:00", "18:00", eighteen) {
		t.Fatalf("expected 18:00 to be excluded from [14:00, 18:00)")
	}

	// Overnight range 22:00-06:00: 02:00 is inside.
	twoAM := time.Date(2026, 1, 1, 2, 0, 0, 0, timeutil.ShanghaiTZ)
	if !timeutil.IsInRange("22:00", "06:00", twoAM) {
		t.Fatalf("expected 02:00 to be inside overnight [22:00, 06:00)")
	}

	// Day-of-week match: 2026-01-01 is a Thursday (weekday 4).
	if !timeutil.MatchDay("1,2,3,4,5", now) {
		t.Fatalf("expected Thursday (4) to match weekday mask 1-5")
	}
	if timeutil.MatchDay("0,6", now) {
		t.Fatalf("expected Thursday to NOT match weekend mask 0,6")
	}
}
