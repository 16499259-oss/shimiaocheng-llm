package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/router"
	"llm_api_gateway/internal/security"
)

// newProvidersTestHandler builds an admin.Handler wired with a provider store and
// router (ProviderStore + Router are required so CreateProvider's hot-reload works)
// on a migrated temp DB, and returns a ServeMux with GET /api/providers routed to
// HandleListProviders. r.PathValue is populated from the registered pattern.
func newProvidersTestHandler(t *testing.T) (*http.ServeMux, *provider.ProviderStore) {
	t.Helper()
	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "admin_providers_test_*.db")
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

	store := provider.NewProviderStore(database.Conn, kek)
	routerInst := router.NewRouter(database.Conn, store)
	h := &Handler{
		DB:            database.Conn,
		ProviderStore: store,
		Router:        routerInst,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/providers", h.HandleListProviders)
	return mux, store
}

// TestHandleListProviders_ReturnsMonthlyLimits verifies the F5 backend contract:
// the provider list API ALWAYS returns monthly_token_limit / monthly_call_limit
// on every provider object. The frontend's F5 fix decides "unlimited" from the
// provider's OWN limit field rather than from whether the (separate) usage
// subquery succeeded, so this contract must hold for the degraded path to be
// reliable. We assert both key PRESENCE and correct VALUES.
func TestHandleListProviders_ReturnsMonthlyLimits(t *testing.T) {
	mux, store := newProvidersTestHandler(t)

	// Capped provider: token=500000, call=5000.
	if _, err := store.CreateProvider("OpenAI", "openai", "https://api.openai.com", "sk", false, false, "Authorization", "bearer", nil, 500000, 5000, 0, 0, ""); err != nil {
		t.Fatalf("create openai: %v", err)
	}
	// Unlimited provider: both limits 0.
	if _, err := store.CreateProvider("Zhipu", "zhipu", "https://api.zhipu.com", "sk", false, false, "Authorization", "bearer", nil, 0, 0, 0, 0, ""); err != nil {
		t.Fatalf("create zhipu: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/providers", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}

	// Decode as a list of raw objects so we can assert explicit field presence
	// (a struct decode would mask an absent field as its zero value).
	var resp struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(resp.Data))
	}

	bySlug := map[string]map[string]json.RawMessage{}
	for _, p := range resp.Data {
		var slug string
		if err := json.Unmarshal(p["slug"], &slug); err != nil {
			t.Fatalf("decode slug: %v", err)
		}
		bySlug[slug] = p
	}

	assertLimitField := func(slug string, wantTok, wantCall int64) {
		raw, ok := bySlug[slug]
		if !ok {
			t.Fatalf("provider %q missing from list response", slug)
		}
		// Presence: the keys must EXIST in the object.
		tokRaw, hasTok := raw["monthly_token_limit"]
		callRaw, hasCall := raw["monthly_call_limit"]
		if !hasTok {
			t.Fatalf("provider %q response missing field monthly_token_limit", slug)
		}
		if !hasCall {
			t.Fatalf("provider %q response missing field monthly_call_limit", slug)
		}
		var gotTok, gotCall int64
		if err := json.Unmarshal(tokRaw, &gotTok); err != nil {
			t.Fatalf("decode monthly_token_limit for %q: %v", slug, err)
		}
		if err := json.Unmarshal(callRaw, &gotCall); err != nil {
			t.Fatalf("decode monthly_call_limit for %q: %v", slug, err)
		}
		if gotTok != wantTok {
			t.Errorf("provider %q monthly_token_limit = %d, want %d", slug, gotTok, wantTok)
		}
		if gotCall != wantCall {
			t.Errorf("provider %q monthly_call_limit = %d, want %d", slug, gotCall, wantCall)
		}
	}

	// Capped provider carries the configured values.
	assertLimitField("openai", 500000, 5000)
	// Unlimited provider still exposes both fields (present, value 0).
	assertLimitField("zhipu", 0, 0)
}
