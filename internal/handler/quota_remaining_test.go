package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// F2: when token usage exceeds the cap (soft gate, allowed by design), the
// remaining field must clamp to 0 instead of going negative.
func TestQuotaHandler_TokenOverageRemainingClamped(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("qh", 7)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, "overuser", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// cap=100, used=120 (overage).
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, u.ID, 100); err != nil {
		t.Fatalf("set token limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, u.ID, 120); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	wrapped := auth.NewMiddleware(database.Conn).SubKeyAuth(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var status models.QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.QuotaTokenTotalRemaining < 0 {
		t.Fatalf("remaining = %d, want >= 0 (clamped)", status.QuotaTokenTotalRemaining)
	}
	if status.QuotaTokenTotalRemaining != 0 {
		t.Fatalf("remaining = %d, want 0 for overage", status.QuotaTokenTotalRemaining)
	}
}

// F2: 5h remaining clamps to >= 0 even if the stored used somehow exceeds the
// limit (defensive; the atomic gate normally prevents this).
func TestQuotaHandler_5hRemainingClamped(t *testing.T) {
	database := openQuotaHandlerTestDB(t)
	subKey := auth.GenerateSubKey("qh", 11)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, "over5h", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 50, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// Force an overage directly.
	if _, err := database.Conn.Exec(`UPDATE quotas SET quota_5h_used = 60 WHERE user_id = ?`, u.ID); err != nil {
		t.Fatalf("force overage: %v", err)
	}
	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	wrapped := auth.NewMiddleware(database.Conn).SubKeyAuth(h)
	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var status models.QuotaStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if status.Quota5hRemaining < 0 {
		t.Fatalf("5h remaining = %d, want >= 0", status.Quota5hRemaining)
	}
}
