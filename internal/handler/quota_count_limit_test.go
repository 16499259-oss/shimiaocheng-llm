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

// L2 (frontend contract): a count quota limit of 0 is an invalid/degenerate
// value. Although the admin API now rejects it on edit, a legacy row may still
// hold 0. The /v1/quota endpoint must not crash and must report the limit as 0
// with remaining clamped to 0, so the user panel's renderCountRow guard can
// render it safely (no divide-by-zero / NaN on the client).
func TestQuotaHandler_CountLimitZeroNoCrash(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	subKey := auth.GenerateSubKey("qh", 42)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, "zerocount", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Force both count limits to 0 (legacy / degenerate) plus some usage.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 0, quota_total_limit = 0,
		                quota_5h_used = 3, quota_total_used = 9 WHERE user_id = ?`, u.ID,
	); err != nil {
		t.Fatalf("set zero count limits: %v", err)
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

	// Limit 0 must be reported as-is (so the client guards it) and remaining
	// must never be negative.
	if status.Quota5hLimit != 0 {
		t.Fatalf("expected quota_5h_limit == 0, got %d", status.Quota5hLimit)
	}
	if status.QuotaTotalLimit != 0 {
		t.Fatalf("expected quota_total_limit == 0, got %d", status.QuotaTotalLimit)
	}
	if status.Quota5hRemaining < 0 {
		t.Fatalf("5h remaining = %d, want >= 0", status.Quota5hRemaining)
	}
	if status.QuotaTotalRemaining < 0 {
		t.Fatalf("total remaining = %d, want >= 0", status.QuotaTotalRemaining)
	}
}
