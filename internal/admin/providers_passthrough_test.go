// Package admin — passthrough / MCP provider API round-trip tests.
package admin

import (
	"bytes"
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

// newProviderTestHandler builds an admin.Handler wired with a ProviderStore
// + Router over a migrated temp DB, plus a ServeMux that has the provider
// CRUD routes registered. Routing through the mux is required because
// HandleUpdateProvider/HandleDeleteProvider read r.PathValue("slug"), which
// is only populated when a request is dispatched through a registered
// pattern (a bare httptest.NewRequest leaves it empty).
func newProviderTestHandler(t *testing.T) (*http.ServeMux, *provider.ProviderStore) {
	t.Helper()
	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	f, err := os.CreateTemp(t.TempDir(), "admin_provider_test_*.db")
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

	store := provider.NewProviderStore(database.Conn, kek)
	routerInst := router.NewRouter(database.Conn, store)

	h := &Handler{
		DB:            database.Conn,
		ProviderStore: store,
		Router:        routerInst,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/api/providers", h.HandleCreateProvider)
	mux.HandleFunc("PUT /admin/api/providers/{slug}", h.HandleUpdateProvider)

	return mux, store
}

// TestCreateProvider_PassthroughFields verifies the POST /admin/api/providers
// API persists the new passthrough / auth fields and the snapshot picks
// them up.
func TestCreateProvider_PassthroughFields(t *testing.T) {
	mux, store := newProviderTestHandler(t)

	body := map[string]any{
		"name":              "Anthropic",
		"slug":              "anthropic",
		"endpoint":          "https://api.anthropic.com",
		"api_key":           "sk-test",
		"is_default":        false,
		"allow_passthrough": true,
		"auth_header":       "X-Api-Key",
		"auth_scheme":       "x-api-key",
		"extra_headers":     map[string]string{"anthropic-version": "2023-06-01"},
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateProvider expected 201, got %d; body=%s", rec.Code, rec.Body.String())
	}

	// Assert the fields were persisted.
	g, err := store.GetProvider("anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if !g.AllowPassthrough {
		t.Error("allow_passthrough not persisted")
	}
	if g.AuthScheme != "x-api-key" || g.AuthHeader != "X-Api-Key" {
		t.Errorf("auth fields not persisted: scheme=%q header=%q", g.AuthScheme, g.AuthHeader)
	}
	if g.ExtraHeaders != `{"anthropic-version":"2023-06-01"}` {
		t.Errorf("extra_headers not persisted: %q", g.ExtraHeaders)
	}

	// Assert the in-memory snapshot (used by the passthrough handler) has them.
	table, err := store.BuildProviderTable()
	if err != nil {
		t.Fatalf("BuildProviderTable: %v", err)
	}
	entry, ok := table.Providers["anthropic"]
	if !ok {
		t.Fatal("anthropic missing from provider table")
	}
	if !entry.AllowPassthrough || entry.AuthScheme != "x-api-key" {
		t.Error("snapshot missing passthrough auth fields")
	}
	if entry.ExtraHeaders["anthropic-version"] != "2023-06-01" {
		t.Errorf("snapshot extra_headers not parsed: %#v", entry.ExtraHeaders)
	}
}

// TestUpdateProvider_PassthroughFields verifies the PUT /admin/api/providers/{slug}
// API applies the new passthrough fields.
func TestUpdateProvider_PassthroughFields(t *testing.T) {
	mux, store := newProviderTestHandler(t)

	// Create a baseline provider.
	createBody := map[string]any{
		"name":     "Anthropic",
		"slug":     "anthropic",
		"endpoint": "https://api.anthropic.com",
		"api_key":  "sk-test",
	}
	cb, _ := json.Marshal(createBody)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", bytes.NewReader(cb))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateProvider expected 201, got %d", rec.Code)
	}

	// Update to enable passthrough + bearer auth + extra header.
	updateBody := map[string]any{
		"allow_passthrough": true,
		"auth_scheme":       "bearer",
		"auth_header":       "Authorization",
		"extra_headers":     map[string]string{"x-foo": "bar"},
	}
	ub, _ := json.Marshal(updateBody)
	req2 := httptest.NewRequest(http.MethodPut, "/admin/api/providers/anthropic", bytes.NewReader(ub))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("UpdateProvider expected 200, got %d; body=%s", rec2.Code, rec2.Body.String())
	}

	g, err := store.GetProvider("anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if !g.AllowPassthrough {
		t.Error("allow_passthrough not updated")
	}
	if g.AuthScheme != "bearer" || g.AuthHeader != "Authorization" {
		t.Errorf("auth fields not updated: scheme=%q header=%q", g.AuthScheme, g.AuthHeader)
	}
	if g.ExtraHeaders != `{"x-foo":"bar"}` {
		t.Errorf("extra_headers not updated: %q", g.ExtraHeaders)
	}
}
