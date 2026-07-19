// Package admin — low-balance threshold (P2) request validation tests.
package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCreateProvider_InvalidLowRatio verifies that an out-of-range low-balance
// threshold is rejected with HTTP 400 and that no provider is persisted.
func TestCreateProvider_InvalidLowRatio(t *testing.T) {
	mux, store := newProviderTestHandler(t)

	body := map[string]any{
		"name":                    "BadAnthropic",
		"slug":                    "bad-anthropic",
		"endpoint":                "https://api.anthropic.com",
		"api_key":                 "sk-test",
		"monthly_token_low_ratio": 1.5, // > 1.0 -> invalid
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for ratio=1.5, got %d; body=%s", rec.Code, rec.Body.String())
	}

	// No provider should have been created.
	g, err := store.GetProvider("bad-anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if g != nil {
		t.Error("provider should NOT be persisted when ratio is invalid")
	}
}

// TestCreateProvider_ValidLowRatio verifies a valid ratio (<=1.0) is accepted
// and persisted to the provider record.
func TestCreateProvider_ValidLowRatio(t *testing.T) {
	mux, store := newProviderTestHandler(t)

	body := map[string]any{
		"name":                    "Anthropic",
		"slug":                    "anthropic",
		"endpoint":                "https://api.anthropic.com",
		"api_key":                 "sk-test",
		"monthly_token_low_ratio": 0.20,
		"monthly_call_low_ratio":  0.15,
	}
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/providers", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body=%s", rec.Code, rec.Body.String())
	}

	g, err := store.GetProvider("anthropic")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if g == nil {
		t.Fatal("provider not persisted")
	}
	if g.MonthlyTokenLowRatio != 0.20 {
		t.Errorf("monthly_token_low_ratio not persisted: got %v want 0.20", g.MonthlyTokenLowRatio)
	}
	if g.MonthlyCallLowRatio != 0.15 {
		t.Errorf("monthly_call_low_ratio not persisted: got %v want 0.15", g.MonthlyCallLowRatio)
	}
}

// TestUpdateProvider_InvalidLowRatio verifies that an out-of-range ratio in a
// partial PUT update is rejected with HTTP 400.
func TestUpdateProvider_InvalidLowRatio(t *testing.T) {
	mux, _ := newProviderTestHandler(t)

	// Baseline provider.
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
		t.Fatalf("create expected 201, got %d", rec.Code)
	}

	// Invalid partial update.
	updateBody := map[string]any{
		"monthly_token_low_ratio": 2.0, // > 1.0 -> invalid
	}
	ub, _ := json.Marshal(updateBody)
	req2 := httptest.NewRequest(http.MethodPut, "/admin/api/providers/anthropic", bytes.NewReader(ub))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for update ratio=2.0, got %d; body=%s", rec2.Code, rec2.Body.String())
	}
}
