package admin

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/router"
	"llm_api_gateway/internal/security"
	"llm_api_gateway/internal/timeutil"
)

// newUsageTestHandler builds an admin.Handler with the provider-usage routes
// registered on a ServeMux (route patterns are required so r.PathValue is
// populated for the {slug} handler). Returns the mux, store, and DB conn.
func newUsageTestHandler(t *testing.T) (*http.ServeMux, *provider.ProviderStore, *sql.DB) {
	t.Helper()
	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "admin_usage_test_*.db")
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
	h := &Handler{DB: database.Conn, ProviderStore: store, Router: routerInst}

	// call_logs has a FK on users(id); seed a user so usage inserts are valid.
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, created_at, updated_at)
		 VALUES ('usage-test-user', 'x', 'x', 'x', 'user', 'active', datetime('now'), datetime('now'))`,
	); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/provider-usage", h.HandleListProviderUsage)
	mux.HandleFunc("GET /api/providers/{slug}/usage", h.HandleGetProviderUsage)
	return mux, store, database.Conn
}

func insertInWindowLog(t *testing.T, conn *sql.DB, provider string, promptTokens, effectiveCalls int) {
	t.Helper()
	now := time.Now().Add(-time.Hour).In(timeutil.ShanghaiTZ).Format(time.RFC3339)
	if _, err := conn.Exec(
		`INSERT INTO call_logs (user_id, model, provider_id, prompt_tokens, completion_tokens, total_tokens, effective_calls, status_code, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		1, "m", provider, promptTokens, 0, promptTokens, effectiveCalls, 200, now,
	); err != nil {
		t.Fatalf("insert log: %v", err)
	}
}

func TestHandleListProviderUsage(t *testing.T) {
	mux, store, conn := newUsageTestHandler(t)

	if _, err := store.CreateProvider("OpenAI", "openai", "https://api.openai.com", "sk", false, false, "Authorization", "bearer", nil, 1000, 10); err != nil {
		t.Fatalf("create openai: %v", err)
	}
	if _, err := store.CreateProvider("Zhipu", "zhipu", "https://api.zhipu.com", "sk", false, false, "Authorization", "bearer", nil, 0, 0); err != nil {
		t.Fatalf("create zhipu: %v", err)
	}
	// openai is over both limits in the window.
	insertInWindowLog(t, conn, "openai", 1500, 12)

	req := httptest.NewRequest(http.MethodGet, "/api/provider-usage", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Data []models.ProviderUsageView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("expected 2 views (all providers), got %d", len(resp.Data))
	}

	bySlug := map[string]models.ProviderUsageView{}
	for _, v := range resp.Data {
		bySlug[v.Slug] = v
	}

	a := bySlug["openai"]
	if a.TokenUsed != 1500 || a.CallUsed != 12 {
		t.Errorf("openai used tok=%d call=%d", a.TokenUsed, a.CallUsed)
	}
	if !a.TokenLow || !a.CallLow {
		t.Error("openai over limit should be flagged low")
	}
	if a.TokenRemaining != -500 || a.CallRemaining != -2 {
		t.Errorf("openai remaining tok=%d call=%d", a.TokenRemaining, a.CallRemaining)
	}
	if a.MonthlyTokenLimit != 1000 || a.MonthlyCallLimit != 10 {
		t.Error("limits not carried into view")
	}

	b := bySlug["zhipu"]
	if !b.TokenUnlimited || !b.CallUnlimited {
		t.Error("zhipu (0 limits) should be unlimited")
	}
	if b.TokenRemaining != -1 || b.CallRemaining != -1 {
		t.Errorf("zhipu remaining should be -1, got tok=%d call=%d", b.TokenRemaining, b.CallRemaining)
	}
	if b.TokenUsed != 0 {
		t.Error("zhipu with no calls should report zero used")
	}
}

func TestHandleGetProviderUsage(t *testing.T) {
	mux, store, conn := newUsageTestHandler(t)
	if _, err := store.CreateProvider("OpenAI", "openai", "https://api.openai.com", "sk", false, false, "Authorization", "bearer", nil, 1000, 10); err != nil {
		t.Fatalf("create: %v", err)
	}
	insertInWindowLog(t, conn, "openai", 1500, 12)

	// Existing provider -> 200 with computed view.
	req := httptest.NewRequest(http.MethodGet, "/api/providers/openai/usage", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data models.ProviderUsageView `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Data.TokenUsed != 1500 || !resp.Data.TokenLow {
		t.Errorf("openai view wrong: used=%d low=%v", resp.Data.TokenUsed, resp.Data.TokenLow)
	}

	// Missing provider -> 404.
	req2 := httptest.NewRequest(http.MethodGet, "/api/providers/nope/usage", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing provider, got %d", rec2.Code)
	}
}
