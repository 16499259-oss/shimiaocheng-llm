package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// TestHandler_ServeHTTPSync_RefundsCountOnUpstreamError verifies that when the
// upstream returns a non-2xx response the already-deducted call-count quota is
// REFUNDED (audit MEDIUM: 上游非200 仍扣次数不退款). A successful request deducts
// 1 call; a failed one must leave the count at 0, not 1.
func TestHandler_ServeHTTPSync_RefundsCountOnUpstreamError(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "refund", "refund")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
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

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	if q.Quota5hUsed != 0 || q.QuotaTotalUsed != 0 {
		t.Fatalf("failed upstream must refund count (got 5h=%d total=%d, want 0/0)",
			q.Quota5hUsed, q.QuotaTotalUsed)
	}
}

// TestHandler_ServeHTTPSync_StillDeductsOnSuccess confirms the refund path does
// NOT fire on a normal 200 response: the call-count quota must remain deducted.
func TestHandler_ServeHTTPSync_StillDeductsOnSuccess(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "okrefund", "okrefund")

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
	if q.Quota5hUsed != 1 || q.QuotaTotalUsed != 1 {
		t.Fatalf("successful request must keep its deduction (got 5h=%d total=%d, want 1/1)",
			q.Quota5hUsed, q.QuotaTotalUsed)
	}
}
