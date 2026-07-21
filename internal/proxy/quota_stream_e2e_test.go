// Package proxy contains ADDITIONAL end-to-end and streaming coverage tests for
// the two quota features under QA review:
//   - Feature A: cumulative Token total limit — the STREAM response path must
//     account tokens too (PR #12 fixed the sync-only gap), and the billed counter
//     (quota_token_total_used) is multiplier-scaled while call_logs keeps raw usage.
//   - Feature B: call-count limit — firing N requests must exhaust the 5h window so
//     request N+1 returns 429 (type=quota_exceeded); effectiveCalls = ceil(multiplier)
//     is what is deducted; a count limit of 0 means UNLIMITED (since 2026-07-21,
//     unified with the Token cap); and when BOTH dimensions are exhausted the proxy
//     reports type=token_quota_exceeded.
//
// These complement handler_token_test.go (which covers sync token accounting and the
// basic Token/count 429 classification) by exercising the STREAM path and the full
// request-loop exhaustion behaviour through httptest.
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

// sseUpstream returns an httptest.Server that emulates an upstream LLM streaming
// response: one SSE data frame carrying a usage block (prompt+completion+tokens)
// followed by the [DONE] sentinel. The handler parses usage from the first data
// frame and credits it to quota_token_total_used.
func sseUpstream(t *testing.T, promptTokens, completionTokens int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		usage := map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		}
		data, _ := json.Marshal(map[string]any{
			"id":      "x",
			"object":  "chat.completion.chunk",
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "hi"}}},
			"usage":   usage,
		})
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

// doStreamQuotaRequest issues a streaming chat request as the given sub-key user
// against a Handler wired to the supplied mock upstream (legacy getters only).
func doStreamQuotaRequest(t *testing.T, database *db.DB, subKey, upstreamURL string) *httptest.ResponseRecorder {
	t.Helper()
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)
	h := &Handler{
		APIKeyGetter:   func() string { return "sk-dummy" },
		EndpointGetter: func() string { return upstreamURL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Compaction:     CompactionTrim,
	}
	wrapped := auth.NewMiddleware(database.Conn).SubKeyAuth(h)
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

// TestHandler_ServeHTTPStream_AccumulatesTokenUsage is the STREAM-path counterpart
// of the sync regression test: the SSE response path must also credit
// prompt_tokens+completion_tokens to quota_token_total_used (PR #12 — early builds
// only accounted the streaming path's call_logs and under-counted the Token total).
func TestHandler_ServeHTTPStream_AccumulatesTokenUsage(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "stream", "stream")

	upstream := sseUpstream(t, 10, 5)
	defer upstream.Close()

	rec := doStreamQuotaRequest(t, database, subKey, upstream.URL)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.QuotaTokenTotalUsed != 15 {
		t.Fatalf("stream path must accumulate prompt+completion (10+5=15) into quota_token_total_used, got %d; body=%s", q.QuotaTokenTotalUsed, rec.Body.String())
	}
}

// TestHandler_ServeHTTPStream_TokenUsageAppliesMultiplier pins the multiplier-on-
// Token-accounting fix for the STREAM path: the billed counter (quota_token_total_used)
// is charged raw_usage × multiplier (15 × 2.0 = 30), while call_logs keeps the real
// upstream usage (15) for honest auditing.
func TestHandler_ServeHTTPStream_TokenUsageAppliesMultiplier(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "streammult", "streammult")

	mult := 2.0
	if err := models.UpdateFixedMultiplier(database.Conn, userID, &mult); err != nil {
		t.Fatalf("set fixed multiplier: %v", err)
	}

	upstream := sseUpstream(t, 10, 5)
	defer upstream.Close()

	rec := doStreamQuotaRequest(t, database, subKey, upstream.URL)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.QuotaTokenTotalUsed != 30 {
		t.Fatalf("billed Token must be raw_usage × multiplier (15 × 2.0 = 30), got %d", q.QuotaTokenTotalUsed)
	}

	var loggedTotal int
	if err := database.Conn.QueryRow(
		`SELECT total_tokens FROM call_logs WHERE user_id = ? ORDER BY id DESC LIMIT 1`, userID,
	).Scan(&loggedTotal); err != nil {
		t.Fatalf("read call_logs total_tokens: %v", err)
	}
	if loggedTotal != 15 {
		t.Fatalf("call_logs.total_tokens must stay raw (15) for auditing, got %d", loggedTotal)
	}
}

// newCountExhaustHandler builds a Handler wired to a JSON (non-streaming) mock
// upstream that always returns 200 with a small usage block. Used by the e2e
// count/token exhaustion tests.
func newCountExhaustHandler(t *testing.T, database *db.DB, upstreamURL string) http.Handler {
	t.Helper()
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)
	h := &Handler{
		APIKeyGetter:   func() string { return "sk-dummy" },
		EndpointGetter: func() string { return upstreamURL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Compaction:     CompactionTrim,
	}
	return auth.NewMiddleware(database.Conn).SubKeyAuth(h)
}

// TestHandler_ServeHTTPEndToEnd_CountExhaustion is the requested end-to-end case:
// with a 5h limit of N, fire N requests (each deducting 1 effective call under the
// default ×1 multiplier); the first N must return 200 and the (N+1)-th must return
// 429 with type=quota_exceeded. It also asserts the count counters were decremented
// exactly N times.
func TestHandler_ServeHTTPEndToEnd_CountExhaustion(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "cntend", "cntend")

	// Cap the 5h window at 3; total and Token must stay unlimited for this case.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 3, quota_5h_used = 0 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("set 5h limit=3: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	handler := newCountExhaustHandler(t, database, upstream.URL)
	doReq := func() *httptest.ResponseRecorder {
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+subKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// First 3 requests succeed.
	for i := 0; i < 3; i++ {
		rec := doReq()
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.Quota5hUsed != 3 {
		t.Fatalf("expected quota_5h_used == 3 after 3 requests, got %d", q.Quota5hUsed)
	}

	// 4th request must be blocked with the count 429.
	rec := doReq()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 4th request, got %d (body=%s)", rec.Code, rec.Body.String())
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
	if resp.Error.Type != "quota_exceeded" {
		t.Fatalf("expected type=quota_exceeded, got %q (body=%s)", resp.Error.Type, rec.Body.String())
	}
	if resp.Error.Code != "quota_exceeded" {
		t.Fatalf("expected code=quota_exceeded, got %q", resp.Error.Code)
	}
	// Per design-token-total-limit.md the count 429文案 is "Quota exceeded"
	// (English); the Token 429 is the one that reads "Token 额度已用尽".
	if resp.Error.Message != "Quota exceeded" {
		t.Fatalf("expected message=Quota exceeded, got %q", resp.Error.Message)
	}
}

// TestHandler_ServeHTTPEndToEnd_TokenExhaustion is the end-to-end Token case: a
// fixed ×10 multiplier makes each request bill ceil(2 × 10) = 20 Tokens. With a
// Token cap of 40, two requests succeed (used 0→20→40) and the third is blocked
// before the gate (40 < 40 is false) with type=token_quota_exceeded. This also
// proves the sync path accounts under a multiplier.
func TestHandler_ServeHTTPEndToEnd_TokenExhaustion(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "tokend", "tokend")

	mult := 10.0
	if err := models.UpdateFixedMultiplier(database.Conn, userID, &mult); err != nil {
		t.Fatalf("set fixed multiplier: %v", err)
	}
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 40); err != nil {
		t.Fatalf("set token limit=40: %v", err)
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	handler := newCountExhaustHandler(t, database, upstream.URL)
	doReq := func() *httptest.ResponseRecorder {
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":false}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+subKey)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// Two requests succeed (billed 20 each).
	for i := 0; i < 2; i++ {
		rec := doReq()
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d expected 200, got %d (body=%s)", i+1, rec.Code, rec.Body.String())
		}
	}
	q, _ := models.GetQuota(database.Conn, userID)
	if q.QuotaTokenTotalUsed != 40 {
		t.Fatalf("expected token used == 40 after 2 requests, got %d", q.QuotaTokenTotalUsed)
	}

	// Third request: gate 40 < 40 is false -> blocked with token 429.
	rec := doReq()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on 3rd request, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
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
}

// TestHandler_ServeHTTP_BothQuotasExhaustedReportsTokenType locks the documented
// boundary (design-token-total-limit.md §"拦截分类优先级"): when BOTH the count and
// Token dimensions are simultaneously exhausted, the atomic gate already decided to
// block, and the read-back classification reports type=token_quota_exceeded (the
// block itself is always correct; only the label favours Token).
func TestHandler_ServeHTTP_BothQuotasExhaustedReportsTokenType(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "both", "both")

	// Exhaust 5h AND set Token used == limit.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 1, quota_5h_used = 1 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("exhaust 5h: %v", err)
	}
	if err := models.UpdateQuotaTokenTotalLimit(database.Conn, userID, 100); err != nil {
		t.Fatalf("set token limit: %v", err)
	}
	if err := models.AddTokenUsage(database.Conn, userID, 100); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}

	// The quota gate fails before any upstream is contacted, so the helper's
	// unreachable endpoint is fine.
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
	if resp.Error.Type != "token_quota_exceeded" {
		t.Fatalf("expected both-exhausted to report token_quota_exceeded, got %q (body=%s)", resp.Error.Type, rec.Body.String())
	}
}

// TestHandler_ServeHTTP_CountLimitZeroUnlimitedAllows verifies the post-2026-07-21
// semantics flip at the proxy edge: a count quota limit of 0 now means UNLIMITED
// (previously it locked the user out). The gate must let the request through; the
// unreachable upstream then fails the forward, but it must NOT be a quota 429.
func TestHandler_ServeHTTP_CountLimitZeroUnlimitedAllows(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "legzero", "legzero")

	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 0 WHERE user_id = ?`, userID,
	); err != nil {
		t.Fatalf("set 5h limit=0: %v", err)
	}

	// The quota gate must NOT block (0 = unlimited). The request reaches the
	// unreachable upstream and fails there, but it must not be a quota 429.
	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code == http.StatusTooManyRequests {
		var resp struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		t.Fatalf("count limit 0 must be unlimited, but got 429 (type=%q, body=%s)", resp.Error.Type, rec.Body.String())
	}
}
