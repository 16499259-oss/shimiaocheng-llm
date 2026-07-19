package proxy

// Comprehensive QA test suite for the wildcard passthrough handler.
//
// These tests augment the engineer's passthrough_test.go with coverage for the
// gaps called out in docs/prd-mcp-passthrough.md §8 and the design doc:
//   - upstream 5xx forwarded verbatim (non-stream)
//   - request body budget -> 413 (P0-5)
//   - per-user concurrency cap -> 429 (P0-4)
//   - fixed route-mode resolution + missing fixed provider -> 503 (P0-6)
//   - all HTTP methods forwarded verbatim (P0-5 / Q5)
//   - response hop-by-hop header stripping (design Q8)
//   - "none" auth scheme (key hidden, optional explicit header) (P1-13)
//   - raw query preserved verbatim incl. special chars
//   - client X-Api-Key sub-key stripped (key hiding)
//   - call_logs Model convention "<METHOD> <subPath>" (P0-12)
//   - pure-function unit tests for buildPassthroughTarget / injectUpstreamAuth /
//     isStreamingResponse / methodPathModel
//
// Reuses the engineer's newTestGateway / doPassthrough / assertErrType /
// defaultUpstreamHandler helpers (same package) and adds qaNewGateway for the
// tests that need to override max_body_size / max_concurrency / route_mode.

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/provider"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
	"llm_api_gateway/internal/security"
)

// qaOpts configures qaNewGateway for tests that need to override the
// per-user limits / route mode that newTestGateway hard-codes.
type qaOpts struct {
	passthroughOn    bool
	allowPassthrough bool
	scheme           string
	authHeader       string
	extra            map[string]string
	endpointOverride string
	totalLimit       int  // quota total limit; <=0 means default 100000
	quotaExhausted   bool // when true, force quota_total_limit = 0 (immediately exhausted)
	upstreamH        http.HandlerFunc
	routeMode        string // default "auto"
	fixedProvider    string // used when routeMode == "fixed"
	maxBodySize      int64  // default 1048576
	maxConcurrency   int    // default 10
}

// qaNewGateway is a configurable variant of the engineer's newTestGateway that
// also returns the in-memory *sql.DB so call-log assertions are possible, and
// allows overriding per-user limits / route mode.
func qaNewGateway(t *testing.T, opts qaOpts) (*httptest.Server, string, *sql.DB) {
	t.Helper()

	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	upstreamH := opts.upstreamH
	if upstreamH == nil {
		upstreamH = defaultUpstreamHandler
	}
	upstream := httptest.NewServer(upstreamH)
	t.Cleanup(upstream.Close)

	endpoint := upstream.URL
	if opts.endpointOverride != "" {
		endpoint = opts.endpointOverride
	}

	database, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	store := provider.NewProviderStore(database.Conn, kek)
	scheme := opts.scheme
	if scheme == "" {
		scheme = "x-api-key"
	}
	authHeader := opts.authHeader
	if authHeader == "" {
		authHeader = "X-Api-Key"
	}
	if _, err := store.CreateProvider("Anthropic", "anthropic", endpoint,
		"sk-real-upstream-key", false, opts.allowPassthrough, authHeader, scheme, opts.extra, 0, 0, 0, 0, ""); err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	subKey := auth.GenerateSubKey("pptest", 1)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	now := db.Now()
	routeMode := opts.routeMode
	if routeMode == "" {
		routeMode = "auto"
	}
	maxBody := opts.maxBodySize
	if maxBody <= 0 {
		maxBody = 1048576
	}
	maxConc := opts.maxConcurrency
	if maxConc <= 0 {
		maxConc = 10
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, route_mode, fixed_provider, max_body_size, max_concurrency, created_at, updated_at)
		 VALUES (?, 'x', ?, ?, 'user', 'active', ?, ?, ?, ?, ?, ?)`,
		"pptest", subHash, subPreview, routeMode, opts.fixedProvider, maxBody, maxConc, now, now,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	totalLimit := opts.totalLimit
	if totalLimit <= 0 {
		totalLimit = 100000
	}
	if opts.quotaExhausted {
		totalLimit = 0
	}
	if _, err := database.Conn.Exec(
		`INSERT INTO quotas (user_id, quota_5h_limit, quota_5h_used, quota_total_limit, quota_total_used, quota_token_total_limit, quota_token_total_used, window_start, updated_at)
		 VALUES (1, 1000, 0, ?, 0, 0, 0, ?, ?)`,
		totalLimit, now, now,
	); err != nil {
		t.Fatalf("insert quota: %v", err)
	}

	routerInst := router.NewRouter(database.Conn, store)
	multiplierEng := quota.NewMultiplierEngine(database.Conn)
	quotaChecker := quota.NewChecker(database.Conn, multiplierEng, 5)

	h := &PassthroughHandler{
		QuotaChecker:       quotaChecker,
		MultiplierEng:      multiplierEng,
		Router:             routerInst,
		PassthroughEnabled: func() bool { return opts.passthroughOn },
		SyncTimeout:        5 * time.Second,
		StreamTimeout:      5 * time.Second,
	}
	authMW := auth.NewMiddleware(database.Conn)
	gw := httptest.NewServer(authMW.SubKeyAuth(h))
	t.Cleanup(gw.Close)
	return gw, subKey, database.Conn
}

// TestPassthrough_Upstream5xxForwarded verifies an upstream 5xx is forwarded
// verbatim (status + body) and is NOT treated as streaming.
func TestPassthrough_Upstream5xxForwarded(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"boom 503"}`))
	})
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		upstreamH:        upstreamH,
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 forwarded, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "boom 503") {
		t.Errorf("upstream 5xx body not forwarded: %q", string(body))
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got == "no" {
		t.Error("non-streaming 5xx should not carry X-Accel-Buffering: no")
	}
}

// TestPassthrough_AllMethodsForwarded verifies every whitelisted HTTP method is
// forwarded verbatim (method + sub-path preserved; body for PUT/PATCH).
func TestPassthrough_AllMethodsForwarded(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
	})
	methods := []struct {
		method string
		body   string
	}{
		{"GET", ""},
		{"POST", `{"x":1}`},
		{"PUT", `{"x":2}`},
		{"PATCH", `{"x":3}`},
		{"DELETE", ""},
	}
	for _, m := range methods {
		resp := doPassthrough(t, gw, subKey, m.method, "/v1/messages", "", m.body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", m.method, resp.StatusCode)
		}
		if got := resp.Header.Get("X-U-Method"); got != m.method {
			t.Errorf("%s: upstream method = %q, want %q", m.method, got, m.method)
		}
		if got := resp.Header.Get("X-U-Path"); got != "/v1/messages" {
			t.Errorf("%s: upstream path = %q, want /v1/messages", m.method, got)
		}
		if m.body != "" {
			if got := resp.Header.Get("X-U-Body"); got != m.body {
				t.Errorf("%s: upstream body = %q, want %q", m.method, got, m.body)
			}
		}
		resp.Body.Close()
	}
}

// TestPassthrough_ResponseHopByHopStripped verifies hop-by-hop response headers
// (Connection / Trailers) are dropped before reaching the client, while normal
// headers and (for non-stream) the absence of X-Accel-Buffering are preserved.
func TestPassthrough_ResponseHopByHopStripped(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Connection", "keep-alive") // hop-by-hop
		w.Header().Set("Trailers", "X-Foo")        // hop-by-hop
		w.Header().Set("X-Up-Proprietary", "yes")  // should be forwarded
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		upstreamH:        upstreamH,
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if _, ok := resp.Header["Connection"]; ok {
		t.Error("hop-by-hop Connection header must not be forwarded")
	}
	if _, ok := resp.Header["Trailers"]; ok {
		t.Error("hop-by-hop Trailers header must not be forwarded")
	}
	if got := resp.Header.Get("X-Up-Proprietary"); got != "yes" {
		t.Errorf("non-hop-by-hop X-Up-Proprietary = %q, want yes", got)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got == "no" {
		t.Error("non-streaming response should not set X-Accel-Buffering: no")
	}
}

// TestPassthrough_NoneAuthScheme verifies the "none" auth scheme honours the
// configured auth header (injecting the real key under it) while the client
// sub-key is always hidden. The "inject nothing when no header is configured"
// branch is covered by TestInjectUpstreamAuth (pure function).
func TestPassthrough_NoneAuthScheme(t *testing.T) {
	// Case A: scheme "none" with the helper's defaulted auth header
	// ("X-Api-Key") -> the real key is injected under that header, and the
	// client sub-key Authorization is stripped.
	gwA, subKeyA := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "none",
		authHeader:       "", // helper defaults this to "X-Api-Key"
	})
	respA := doPassthrough(t, gwA, subKeyA, "POST", "/mcp", "", `{}`)
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("caseA: expected 200, got %d", respA.StatusCode)
	}
	if got := respA.Header.Get("X-U-Auth"); got != "sk-real-upstream-key" {
		t.Errorf("caseA: upstream X-Api-Key = %q, want the real injected key", got)
	}
	if got := respA.Header.Get("X-U-Authz"); got != "" {
		t.Errorf("caseA: upstream Authorization should be empty (sub-key hidden), got %q", got)
	}

	// Case B: scheme "none" + explicit custom auth header -> injects under it.
	upstreamB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-U-CustomAuth", r.Header.Get("X-Custom-Auth"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	gwB, subKeyB := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "none",
		authHeader:       "X-Custom-Auth",
		upstreamH:        upstreamB,
	})
	respB := doPassthrough(t, gwB, subKeyB, "POST", "/mcp", "", `{}`)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusOK {
		t.Fatalf("caseB: expected 200, got %d", respB.StatusCode)
	}
	if got := respB.Header.Get("X-U-CustomAuth"); got != "sk-real-upstream-key" {
		t.Errorf("caseB: X-Custom-Auth = %q, want the real injected key", got)
	}
}

// TestPassthrough_QuerySpecialChars verifies the raw query string (including
// URL-encoded special characters) is preserved verbatim to the upstream.
func TestPassthrough_QuerySpecialChars(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
	})
	rawQuery := "foo=bar%20baz&qux=1%2B2"
	resp := doPassthrough(t, gw, subKey, "GET", "/sse", rawQuery, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-U-Query"); got != rawQuery {
		t.Errorf("upstream query = %q, want %q (raw query must be preserved verbatim)", got, rawQuery)
	}
}

// TestPassthrough_ClientXApiKeyStripped verifies a client-supplied X-Api-Key
// (a second place a sub-key might hide) is stripped and never reaches upstream.
func TestPassthrough_ClientXApiKeyStripped(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "bearer",
		authHeader:       "Authorization",
	})
	url := gw.URL + "/v1/passthrough/mcp"
	req, _ := http.NewRequest("POST", url, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+subKey)
	req.Header.Set("X-Api-Key", "client-leaked-subkey") // client attempts to pass sub-key as X-Api-Key
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// Bearer scheme injects Authorization only; client's X-Api-Key must be gone.
	if got := resp.Header.Get("X-U-Auth"); got != "" {
		t.Errorf("client X-Api-Key leaked to upstream: %q", got)
	}
	if got := resp.Header.Get("X-U-Authz"); got != "Bearer sk-real-upstream-key" {
		t.Errorf("upstream Authorization = %q, want Bearer <realkey>", got)
	}
}

// TestPassthrough_BodyLimitExceeded verifies a request body larger than the
// per-user budget is rejected with 413 request_entity_too_large (P0-5).
func TestPassthrough_BodyLimitExceeded(t *testing.T) {
	gw, subKey, _ := qaNewGateway(t, qaOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "bearer",
		authHeader:       "Authorization",
		maxBodySize:      100,
	})
	big := strings.Repeat("x", 500)
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", big)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
	assertErrType(t, resp, "request_entity_too_large")
}

// TestPassthrough_FixedProviderRouting verifies fixed route mode resolves the
// configured provider slug, and an unknown slug yields 503 no_provider (P0-6).
func TestPassthrough_FixedProviderRouting(t *testing.T) {
	// Case A: fixed mode resolves the configured provider slug.
	gwA, subKeyA, _ := qaNewGateway(t, qaOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "x-api-key",
		authHeader:       "X-Api-Key",
		routeMode:        "fixed",
		fixedProvider:    "anthropic",
	})
	respA := doPassthrough(t, gwA, subKeyA, "POST", "/mcp", "", `{}`)
	defer respA.Body.Close()
	if respA.StatusCode != http.StatusOK {
		t.Fatalf("caseA: expected 200, got %d", respA.StatusCode)
	}
	if got := respA.Header.Get("X-U-Auth"); got != "sk-real-upstream-key" {
		t.Errorf("caseA: upstream did not receive injected real key: %q", got)
	}

	// Case B: fixed mode with unknown slug -> 503 no_provider.
	gwB, subKeyB, _ := qaNewGateway(t, qaOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "x-api-key",
		authHeader:       "X-Api-Key",
		routeMode:        "fixed",
		fixedProvider:    "does-not-exist",
	})
	respB := doPassthrough(t, gwB, subKeyB, "POST", "/mcp", "", `{}`)
	defer respB.Body.Close()
	if respB.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("caseB: expected 503, got %d", respB.StatusCode)
	}
	assertErrType(t, respB, "no_provider")
}

// TestPassthrough_ConcurrencyLimit verifies that once a user's concurrent slot
// is taken, a second in-flight request is rejected with 429
// concurrency_limit_exceeded (P0-4). Uses a blocking upstream so the first
// request holds its slot deterministically.
func TestPassthrough_ConcurrencyLimit(t *testing.T) {
	ForgetConcurrency(1) // clean slate keyed by user id 1
	block := make(chan struct{})
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		<-block // hold the connection open until the test releases it
	})
	gw, subKey, _ := qaNewGateway(t, qaOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "bearer",
		authHeader:       "Authorization",
		maxConcurrency:   1,
		upstreamH:        upstreamH,
	})

	type result struct{ code int }
	r1 := make(chan result, 1)
	go func() {
		resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
		code := resp.StatusCode
		resp.Body.Close()
		r1 <- result{code}
	}()
	// Give request 1 time to acquire the single concurrency slot.
	time.Sleep(100 * time.Millisecond)

	resp2 := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	code2 := resp2.StatusCode
	if code2 == http.StatusTooManyRequests {
		assertErrType(t, resp2, "concurrency_limit_exceeded")
	} else {
		t.Errorf("request 2: expected 429 concurrency_limit_exceeded, got %d", code2)
	}
	resp2.Body.Close()

	// Release the upstream so request 1 can finish and free its slot.
	close(block)
	res1 := <-r1
	if res1.code != http.StatusOK {
		t.Errorf("request 1: expected 200, got %d", res1.code)
	}
}

// TestPassthrough_CallLogModelConvention verifies the P0 call-log convention
// records Model as "<METHOD> <subPath>" so passthrough calls are observable.
func TestPassthrough_CallLogModelConvention(t *testing.T) {
	gw, subKey, conn := qaNewGateway(t, qaOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "bearer",
		authHeader:       "Authorization",
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{"jsonrpc":"2.0"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", resp.StatusCode, string(body))
	}

	var model string
	var status int
	var pid string
	var eff int
	if err := conn.QueryRow(
		`SELECT model, status_code, provider_id, effective_calls FROM call_logs ORDER BY id DESC LIMIT 1`,
	).Scan(&model, &status, &pid, &eff); err != nil {
		t.Fatalf("query call_logs: %v", err)
	}
	if model != "POST /mcp" {
		t.Errorf("call_log model = %q, want %q", model, "POST /mcp")
	}
	if status != http.StatusOK {
		t.Errorf("call_log status_code = %d, want 200", status)
	}
	if pid != "anthropic" {
		t.Errorf("call_log provider_id = %q, want anthropic", pid)
	}
	if eff < 1 {
		t.Errorf("call_log effective_calls = %d, want >= 1", eff)
	}
}

// ── Pure-function unit tests (no network / DB) ──────────────────────────────

func TestMethodPathModel(t *testing.T) {
	cases := []struct {
		method, sub, want string
	}{
		{"POST", "/mcp", "POST /mcp"},
		{"GET", "", "GET"},
		{"PUT", "/x", "PUT /x"},
		{"DELETE", "/d", "DELETE /d"},
		{"PATCH", "/p", "PATCH /p"},
	}
	for _, c := range cases {
		if got := methodPathModel(c.method, c.sub); got != c.want {
			t.Errorf("methodPathModel(%q,%q) = %q, want %q", c.method, c.sub, got, c.want)
		}
	}
}

func TestIsStreamingResponse(t *testing.T) {
	cases := []struct {
		name string
		ct   string
		te   string
		clen int64
		want bool
	}{
		{"event-stream", "text/event-stream", "", 0, true},
		{"application/stream", "application/stream+json", "", 0, true},
		{"x-ndjson", "application/x-ndjson", "", 0, true},
		{"chunked-te", "", "chunked", 0, true},
		{"neg-clen", "application/json", "", -1, true},
		{"plain-json", "application/json", "", 100, false},
		{"empty", "", "", 0, false},
	}
	for _, c := range cases {
		resp := &http.Response{Header: http.Header{}}
		if c.ct != "" {
			resp.Header.Set("Content-Type", c.ct)
		}
		if c.te != "" {
			resp.Header.Set("Transfer-Encoding", c.te)
		}
		resp.ContentLength = c.clen
		if got := isStreamingResponse(resp); got != c.want {
			t.Errorf("%s: isStreamingResponse = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestBuildPassthroughTarget(t *testing.T) {
	cases := []struct {
		endpoint, sub, q, want string
	}{
		{"https://api.x.com/", "/mcp", "", "https://api.x.com/mcp"},
		{"https://api.x.com", "/mcp", "a=1", "https://api.x.com/mcp?a=1"},
		{"https://api.x.com/", "", "x=1", "https://api.x.com?x=1"},
		{"https://api.x.com", "/v1/messages", "", "https://api.x.com/v1/messages"},
		{"https://api.x.com/", "/mcp", "foo=bar%20baz", "https://api.x.com/mcp?foo=bar%20baz"},
	}
	for _, c := range cases {
		if got := buildPassthroughTarget(c.endpoint, c.sub, c.q); got != c.want {
			t.Errorf("buildPassthroughTarget(%q,%q,%q) = %q, want %q", c.endpoint, c.sub, c.q, got, c.want)
		}
	}
}

func TestInjectUpstreamAuth(t *testing.T) {
	extra := map[string]string{"Anthropic-Version": "2023-06-01"}
	cases := []struct {
		name, scheme, header, key string
		wantHeader, wantVal       string
	}{
		{"bearer-default", "bearer", "", "K", "Authorization", "Bearer K"},
		{"bearer-custom-header", "bearer", "X-Auth", "K", "X-Auth", "Bearer K"},
		{"x-api-key-default", "x-api-key", "", "K", "X-Api-Key", "K"},
		{"x-api-key-custom", "x-api-key", "X-Custom", "K", "X-Custom", "K"},
		{"none-with-header", "none", "X-Foo", "K", "X-Foo", "K"},
		{"unknown-fallback-bearer", "weird", "", "K", "Authorization", "Bearer K"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", "http://up/mcp", nil)
		prov := provider.ProviderEntry{
			APIKey:       c.key,
			AuthScheme:   c.scheme,
			AuthHeader:   c.header,
			ExtraHeaders: extra,
		}
		injectUpstreamAuth(req, prov)
		if got := req.Header.Get(c.wantHeader); got != c.wantVal {
			t.Errorf("%s: %s = %q, want %q", c.name, c.wantHeader, got, c.wantVal)
		}
		if got := req.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Errorf("%s: extra header Anthropic-Version = %q, want 2023-06-01", c.name, got)
		}
	}
	// "none" with no auth header must inject NOTHING.
	req, _ := http.NewRequest("POST", "http://up/mcp", nil)
	injectUpstreamAuth(req, provider.ProviderEntry{AuthScheme: "none", APIKey: "K"})
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("none scheme: Authorization = %q, want empty", got)
	}
	if got := req.Header.Get("X-Api-Key"); got != "" {
		t.Errorf("none scheme: X-Api-Key = %q, want empty", got)
	}
}
