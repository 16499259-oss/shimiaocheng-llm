// Package proxy contains an integration-level test for the strict no-fallback
// invariant: when the time-window (or default) target provider is unreachable,
// the handler must return 502 and must NOT silently downgrade to the other
// (default) provider.
package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
	"llm_api_gateway/internal/security"
)

// openProxyTestDB opens an isolated temp-file SQLite DB, runs migrations, and
// registers cleanup.
func openProxyTestDB(t *testing.T) *db.DB {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "proxy_nofb_*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp db: %v", err)
	}
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// newProxyTestRouter builds a Router from config for proxy integration tests.
func newProxyTestRouter(t *testing.T, database *db.DB, cfg *config.Config) *router.Router {
	t.Helper()
	os.Setenv("GATEWAY_KEK_ENV", "test-proxy-kek-32bytes!!!!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}
	store := provider.NewProviderStore(database.Conn, kek)
	if err := store.SeedFromConfig(cfg); err != nil {
		t.Fatalf("seed from config: %v", err)
	}
	return router.NewRouter(database.Conn, store)
}

// TestHandler_ServeHTTP_StrictNoFallbackReturns502 is an end-to-end check of
// the "hit-the-window, never fall back" invariant. The default provider is
// openai (so it is always selected). openai points at a DEAD upstream address.
// The gateway must attempt openai, fail with 502, and never silently serve the
// request via zhipu. We prove this two ways: the HTTP status is 502, and the
// persisted call log records provider_id = "openai" (not "zhipu").
func TestHandler_ServeHTTP_StrictNoFallbackReturns502(t *testing.T) {
	database := openProxyTestDB(t)

	// A user with a known sub-key and ample quota.
	subKey := auth.GenerateSubKey("qa", 1)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	if _, err := models.CreateUser(database.Conn, "qa", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 0, models.DefaultMaxConcurrency); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// A dead upstream: start a server, capture its URL, then close it so any
	// connection to that address is refused (simulates target provider down).
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	os.Setenv("OPENAI_API_KEY", "sk-openai")
	defer os.Unsetenv("ZHIPU_API_KEY")
	defer os.Unsetenv("OPENAI_API_KEY")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: false},
			{ID: "openai", Endpoint: deadURL, APIKeyEnv: "OPENAI_API_KEY", IsDefault: true},
		},
		ModelMappings: []config.ModelMapping{
			{External: "glm-5.2", PerProvider: map[string]string{"zhipu": "glm-5.2", "openai": "gpt-4o"}},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	creds.Set("openai", "sk-openai")

	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)

	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return cfg.Providers[0].Endpoint },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
	}

	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 (strict no-fallback), got %d; body=%s", rec.Code, rec.Body.String())
	}

	// The gateway must have attempted the target (openai), NOT downgraded to zhipu.
	logs, err := models.QueryCallLogs(database.Conn, models.CallLogFilter{UserID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("query call logs: %v", err)
	}
	if len(logs.Data) == 0 {
		t.Fatalf("expected a call log row to be written")
	}
	last := logs.Data[0]
	if last.ProviderID != "openai" {
		t.Fatalf("no-fallback violated: call log provider_id = %q, want openai", last.ProviderID)
	}
	if last.StatusCode != 502 {
		t.Fatalf("expected call log status_code = 502, got %d", last.StatusCode)
	}
}

// TestHandler_ServeHTTP_QuotaExceededReturns429 verifies the quota-exceeded
// (rate-limit) path: a user with no remaining quota is rejected with 429 and a
// call log row is written with status_code = 429. nginx then passes this 429
// through untouched (proxy_intercept_errors defaults to off), so the client
// receives the gateway's own JSON 429 body.
func TestHandler_ServeHTTP_QuotaExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("qa2", 2)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, "qa2", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 5, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Genuinely exhaust the total call-count quota. Count limit 0 now means
	// UNLIMITED (since 2026-07-21), so we seed used == limit instead of using 0.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_total_used = 5 WHERE user_id = ?`, u.ID,
	); err != nil {
		t.Fatalf("seed total used: %v", err)
	}

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)

	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return cfg.Providers[0].Endpoint },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
	}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)

	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 (quota exceeded), got %d; body=%s", rec.Code, rec.Body.String())
	}

	logs, err := models.QueryCallLogs(database.Conn, models.CallLogFilter{UserID: 1, Limit: 10})
	if err != nil {
		t.Fatalf("query logs: %v", err)
	}
	if len(logs.Data) == 0 {
		t.Fatalf("expected a call log row")
	}
	if logs.Data[0].StatusCode != 429 {
		t.Fatalf("expected call log status_code 429, got %d", logs.Data[0].StatusCode)
	}
}
