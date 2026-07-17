// Package handler contains tests for the /v1/quota endpoint's cumulative
// Token-quota fields.
package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// openQuotaHandlerTestDB opens an isolated temp-file SQLite DB, runs migrations,
// and registers cleanup.
func openQuotaHandlerTestDB(t *testing.T) *db.DB {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "handler_quota_*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	_ = f.Close()
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

// TestQuotaHandler_ReturnsTokenFields verifies the /v1/quota endpoint exposes the
// new cumulative Token fields, and that remaining is forced to 0 when the cap is
// 0 (unlimited) so the frontend can treat it as infinite.
func TestQuotaHandler_ReturnsTokenFields(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("qh", 3)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, "qhuser", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Set cap=100, used=30.
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, u.ID, 100); err != nil {
		t.Fatalf("set token limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, u.ID, 30); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	doGet := func() models.QuotaStatus {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
		req.Header.Set("Authorization", "Bearer "+subKey)
		rec := httptest.NewRecorder()
		wrapped.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
		}
		var status models.QuotaStatus
		if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
			t.Fatalf("decode quota status: %v", err)
		}
		return status
	}

	status := doGet()
	if status.QuotaTokenTotalLimit != 100 {
		t.Fatalf("expected quota_token_total_limit == 100, got %d", status.QuotaTokenTotalLimit)
	}
	if status.QuotaTokenTotalUsed != 30 {
		t.Fatalf("expected quota_token_total_used == 30, got %d", status.QuotaTokenTotalUsed)
	}
	if status.QuotaTokenTotalRemaining != 70 {
		t.Fatalf("expected quota_token_total_remaining == 70, got %d", status.QuotaTokenTotalRemaining)
	}

	// Unlimited case: cap=0 -> remaining forced to 0 (frontend treats as infinite).
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, u.ID, 0); err != nil {
		t.Fatalf("reset token limit: %v", err)
	}
	statusUnlimited := doGet()
	if statusUnlimited.QuotaTokenTotalLimit != 0 {
		t.Fatalf("expected quota_token_total_limit == 0, got %d", statusUnlimited.QuotaTokenTotalLimit)
	}
	if statusUnlimited.QuotaTokenTotalRemaining != 0 {
		t.Fatalf("expected quota_token_total_remaining == 0 when unlimited, got %d", statusUnlimited.QuotaTokenTotalRemaining)
	}
}
