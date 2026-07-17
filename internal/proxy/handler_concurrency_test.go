package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

// TestHandler_ServeHTTP_ConcurrencyExceededReturns429 is the integration test
// for the per-user concurrency cap at the ServeHTTP level (T3 + T9):
//   - a user with MaxConcurrency = 2 gets a 429 on the 3rd simultaneous
//     in-flight request, with a JSON body containing "concurrency_limit_exceeded"
//     and a "Retry-After: 1" header;
//   - because the handler's QuotaChecker is wired, a minimal call_log row is
//     written (status=429, error_msg=concurrency_limit_exceeded) — T9.
//
// To make the test deterministic we use a blocking upstream so the first two
// in-flight requests hold their concurrency slots until we release them,
// guaranteeing the 3rd is rejected by the cap rather than by a timing race.
func TestHandler_ServeHTTP_ConcurrencyExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("qa-conc", 91001)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	user, err := models.CreateUser(database.Conn, "qa-conc", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 0, 2)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := user.ID

	// Blocking upstream: holds the in-flight request (and thus its slot) until
	// we signal release via the `released` channel. `acceptC` lets the test
	// wait until both slots are occupied before firing the 3rd request.
	released := make(chan struct{})
	acceptC := make(chan struct{}, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptC <- struct{}{}
		<-released
	}))
	defer srv.Close()

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: srv.URL, APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)

	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return srv.URL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
	}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	makeReq := func() *http.Request {
		body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+subKey)
		return req
	}

	var (
		wg   sync.WaitGroup
		recs = make([]*httptest.ResponseRecorder, 3)
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			recs[i] = httptest.NewRecorder()
			wrapped.ServeHTTP(recs[i], makeReq())
		}(i)
	}
	// Wait until both in-flight requests have reached the blocking upstream
	// (i.e. both concurrency slots are held).
	<-acceptC
	<-acceptC

	// Now fire the 3rd request; it must be rejected by the cap before the
	// upstream is ever touched.
	recs[2] = httptest.NewRecorder()
	wrapped.ServeHTTP(recs[2], makeReq())

	// Release the two blocked requests and wait for them to finish.
	close(released)
	wg.Wait()

	third := recs[2]
	if third.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd concurrent request expected 429, got %d; body=%s", third.Code, third.Body.String())
	}
	if !strings.Contains(third.Body.String(), "concurrency_limit_exceeded") {
		t.Fatalf("429 body expected to contain concurrency_limit_exceeded, got: %s", third.Body.String())
	}
	if got := third.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("expected Retry-After: 1 header, got %q", got)
	}

	// T9: a call_log row must exist for the concurrency rejection.
	logs, err := models.QueryCallLogs(database.Conn, models.CallLogFilter{UserID: uid, Limit: 20})
	if err != nil {
		t.Fatalf("query call logs: %v", err)
	}
	found := false
	for _, l := range logs.Data {
		if l.StatusCode == 429 && l.ErrorMsg == "concurrency_limit_exceeded" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a call_log row with status_code=429 and error_msg=concurrency_limit_exceeded; got %+v", logs.Data)
	}
}

// TestHandler_ServeHTTP_AcquireReleasedOnParseError verifies the "no leaky slot"
// contract (T8) at the ServeHTTP level: with MaxConcurrency = 1, the first
// request acquires the slot, then fails to parse an invalid JSON body and
// returns 400. The deferred releaseConcurrency must free the slot so the
// following request passes the concurrency gate (reaching the upstream) instead
// of being wrongly 429'd.
func TestHandler_ServeHTTP_AcquireReleasedOnParseError(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("qa-rel", 91002)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	if _, err := models.CreateUser(database.Conn, "qa-rel", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 0, 1); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Dead upstream: any request that passes the concurrency gate and reaches
	// the upstream returns 502. We only care that the slot is freed.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: deadURL, APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)
	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return deadURL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
	}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	// Request 1: invalid JSON body -> acquire succeeds, parse fails -> 400.
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2", this is not valid json`)))
	req1.Header.Set("Authorization", "Bearer "+subKey)
	rec1 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid JSON body, got %d; body=%s", rec1.Code, rec1.Body.String())
	}

	// Request 2: slot must be free now; a valid request should pass the gate
	// (and hit the dead upstream -> 502). It must NOT be 429.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	req2.Header.Set("Authorization", "Bearer "+subKey)
	rec2 := httptest.NewRecorder()
	wrapped.ServeHTTP(rec2, req2)
	if rec2.Code == http.StatusTooManyRequests {
		t.Fatalf("slot was not released after parse error: 2nd request wrongly 429'd; body=%s", rec2.Body.String())
	}
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 (dead upstream) after slot released, got %d; body=%s", rec2.Code, rec2.Body.String())
	}
}
