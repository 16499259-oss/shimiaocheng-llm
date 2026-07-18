// Package proxy contains tests for the cumulative Token-quota 429 classification.
package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// newTokenTestUser creates a user with a known sub-key and ample count quota,
// returning the plaintext sub-key (for Bearer auth) and the user ID.
func newTokenTestUser(t *testing.T, database *db.DB, username, salt string) (subKey string, userID int64) {
	t.Helper()
	subKey = auth.GenerateSubKey(salt, 1)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	u, err := models.CreateUser(database.Conn, username, "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 1_000_000, models.DefaultMaxConcurrency)
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return subKey, u.ID
}

// doTokenQuotaRequest arms a Handler (legacy getters only — no upstream is ever
// reached because the quota gate fails first) and issues a chat request as the
// given sub-key user, returning the recorded response.
func doTokenQuotaRequest(t *testing.T, database *db.DB, subKey string) *httptest.ResponseRecorder {
	t.Helper()
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)
	h := &Handler{
		APIKeyGetter:   func() string { return "sk-dummy" },
		EndpointGetter: func() string { return "http://127.0.0.1:9" }, // never reached
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Compaction:     CompactionTrim,
	}
	wrapped := auth.NewMiddleware(database.Conn).SubKeyAuth(h)

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

// TestHandler_ServeHTTP_TokenQuotaExceededReturns429 verifies that when a user's
// cumulative Token usage has reached the configured cap, the proxy returns 429
// with type=token_quota_exceeded (中文文案「Token 额度已用尽」), distinct from the
// count-quota type. This exercises the OR relationship at the proxy boundary.
func TestHandler_ServeHTTP_TokenQuotaExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "tk", "tk")

	// Cap = 100, already used = 100 -> next request must be blocked on Token.
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 100); err != nil {
		t.Fatalf("set token limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 100); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if resp.Error.Type != "token_quota_exceeded" {
		t.Fatalf("expected type=token_quota_exceeded, got %q (body=%s)", resp.Error.Type, rec.Body.String())
	}
	if resp.Error.Message != "Token 额度已用尽" {
		t.Fatalf("expected message=Token 额度已用尽, got %q", resp.Error.Message)
	}
	if resp.Error.Code != "token_quota_exceeded" {
		t.Fatalf("expected code=token_quota_exceeded, got %q", resp.Error.Code)
	}
}

// TestHandler_ServeHTTP_CountQuotaExceededReturns429 is the count-dimension
// counterpart: with an unlimited Token cap but an exhausted 5h window, the proxy
// must return 429 with type=quota_exceeded (NOT token_quota_exceeded). Together
// with the Token test this pins down the OR classification at the proxy edge.
func TestHandler_ServeHTTP_CountQuotaExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "cnt", "cnt")

	// Exhaust the 5h window; Token cap stays 0 (unlimited).
	if _, err := database.Conn.Exec(`UPDATE quotas SET quota_5h_limit = 1, quota_5h_used = 1 WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("exhaust 5h: %v", err)
	}

	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if resp.Error.Type != "quota_exceeded" {
		t.Fatalf("expected type=quota_exceeded, got %q (body=%s)", resp.Error.Type, rec.Body.String())
	}
}

// TestHandler_ServeHTTPSync_AccumulatesTokenUsage is the REGRESSION test for the
// sync (non-streaming) token-accounting bug: the SYNC response path wrote
// call_logs but never called AddTokenUsage, so every non-streaming request's
// Token usage was missing from quota_token_total_used (the user-panel total).
// It mocks an upstream that returns a chat completion WITH a usage block and
// asserts that the sync path accumulates prompt_tokens+completion_tokens into
// the cumulative Token quota.
func TestHandler_ServeHTTPSync_AccumulatesTokenUsage(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "sync", "sync")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer upstream.Close()

	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)
	h := &Handler{
		APIKeyGetter:   func() string { return "sk-dummy" },
		EndpointGetter: func() string { return upstream.URL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Compaction:     CompactionTrim,
	}
	wrapped := auth.NewMiddleware(database.Conn).SubKeyAuth(h)

	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.QuotaTokenTotalUsed != 15 {
		t.Fatalf("sync path must accumulate prompt+completion (10+5=15) into quota_token_total_used, got %d", q.QuotaTokenTotalUsed)
	}
}
