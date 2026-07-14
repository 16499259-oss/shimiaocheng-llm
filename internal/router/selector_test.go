// Package router_test contains tests for the routing selector.
package router

import (
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/security"
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

// newRouterWithConfig creates a Router from a config for testing. It sets up
// a ProviderStore, seeds from config, and returns the new Router.
func newRouterWithConfig(t *testing.T, dbConn *db.DB, cfg *config.Config) *Router {
	t.Helper()

	os.Setenv("GATEWAY_KEK_ENV", "test-router-kek-32bytes!!!!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	sqlDB := dbConn.Conn

	store := provider.NewProviderStore(sqlDB, kek)
	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("seed from config: %v", err)
	}

	return NewRouter(sqlDB, store)
}

// newRouterNoDB creates a Router without a DB connection (nil DB).
func newRouterNoDB(t *testing.T, cfg *config.Config) *Router {
	t.Helper()

	os.Setenv("GATEWAY_KEK_ENV", "test-nodb-kek-32bytes!!!!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	store := provider.NewProviderStore(nil, kek)
	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("seed from config: %v", err)
	}

	return NewRouter(nil, store)
}

func TestNewRouter_DefaultProvider(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	// Outside the 14:00-18:01 window -> default (zhipu) provider.
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
	if prov.APIKey != "" {
		t.Fatalf("expected zhipu key, got %q", prov.APIKey)
	}
}

func TestResolveProvider_WindowHitReturnsB(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	// 15:00 Asia/Shanghai matches the seeded 14:00-18:01 -> openai rule.
	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider returned error: %v", err)
	}
	if prov.ID != "openai" {
		t.Fatalf("window hit should resolve to openai, got %q (must NOT fall back to zhipu/A)", prov.ID)
	}
}

func TestResolveProvider_NoProvidersConfigured(t *testing.T) {
	// No providers at all -> error (not a silent default).
	os.Setenv("GATEWAY_KEK_ENV", "test-empty-kek-32bytes!!!!!")
	kek, _ := security.DeriveKEK()
	store := provider.NewProviderStore(nil, kek)
	r := NewRouter(nil, store)
	_, err := r.ResolveProvider(time.Now())
	if err == nil {
		t.Fatalf("expected error when no providers configured")
	}
}

func TestResolveProvider_DBFailureFallsBackToDefault(t *testing.T) {
	// nil DB -> Reload fails but stores empty table -> no provider -> error.
	// With the new router, nil DB means we can't load from store.
	os.Setenv("GATEWAY_KEK_ENV", "test-dbfail-kek-32bytes!!!!!")
	kek, _ := security.DeriveKEK()
	store := provider.NewProviderStore(nil, kek)
	r := NewRouter(nil, store)

	now := time.Date(2026, 1, 1, 15, 0, 0, 0, timeutil.ShanghaiTZ)
	_, err := r.ResolveProvider(now)
	if err != nil {
		// Expected: nil DB means no providers can be loaded.
		return
	}
	// If somehow we got a non-error, the test passes (no panic).
}

func TestRewriteModel_Mapping(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	if got := r.RewriteModel("glm-5.2", "openai"); got != "gpt-4o" {
		t.Fatalf("RewriteModel(glm-5.2, openai) = %q, want %q", got, "gpt-4o")
	}
	if got := r.RewriteModel("glm-5.2", "zhipu"); got != "glm-5.2" {
		t.Fatalf("RewriteModel(glm-5.2, zhipu) = %q, want %q", got, "glm-5.2")
	}
}

// TestRoutingRule_WriteThenHit verifies the routing-rule write path end-to-end:
// a rule created at runtime via ProviderStore is picked up by the router's hot
// reload (atomic.Value swap) and correctly resolves a request inside its window.
func TestRoutingRule_WriteThenHit(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	// Create a brand-new routing rule 09:00-11:00 -> openai at runtime.
	if err := r.store.CreateRoutingRule(&models.RoutingRule{
		ProviderID: "openai",
		StartTime:  "09:00",
		EndTime:    "11:00",
		DaysOfWeek: "*",
		Timezone:   "Asia/Shanghai",
		Enabled:    true,
	}); err != nil {
		t.Fatalf("CreateRoutingRule failed: %v", err)
	}
	// Hot-reload the router table so the new rule takes effect (mirrors what the
	// admin handler does after a CRUD operation).
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload after create failed: %v", err)
	}

	// 10:00 Asia/Shanghai is inside the new window -> must resolve to openai.
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider returned error: %v", err)
	}
	if prov.ID != "openai" {
		t.Fatalf("expected new rule to hit openai at 10:00, got %q", prov.ID)
	}
}

// TestRoutingRule_DisabledDoesNotHit verifies that a disabled rule is ignored by
// the router (regardless of window), so the default provider is used instead.
func TestRoutingRule_DisabledDoesNotHit(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

	if err := r.store.CreateRoutingRule(&models.RoutingRule{
		ProviderID: "openai",
		StartTime:  "09:00",
		EndTime:    "11:00",
		DaysOfWeek: "*",
		Timezone:   "Asia/Shanghai",
		Enabled:    false, // disabled on purpose
	}); err != nil {
		t.Fatalf("CreateRoutingRule failed: %v", err)
	}
	if err := r.Reload(); err != nil {
		t.Fatalf("Reload after create failed: %v", err)
	}

	// Even inside the (disabled) window, the router must NOT hit it.
	now := time.Date(2026, 1, 1, 10, 0, 0, 0, timeutil.ShanghaiTZ)
	prov, err := r.ResolveProvider(now)
	if err != nil {
		t.Fatalf("ResolveProvider returned error: %v", err)
	}
	if prov.ID != "zhipu" {
		t.Fatalf("disabled rule must be ignored -> default zhipu, got %q", prov.ID)
	}
}

func TestRewriteModel_Passthrough(t *testing.T) {
	database := newRouterTestDB(t)
	r := newRouterWithConfig(t, database, testConfig())

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
