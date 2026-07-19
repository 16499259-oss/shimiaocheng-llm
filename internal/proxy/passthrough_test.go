package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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

// defaultUpstreamHandler is the upstream stub used by most tests. It records
// what it received into echo headers so the gateway response (which forwards
// upstream headers back) can be asserted against.
func defaultUpstreamHandler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("X-U-Auth", r.Header.Get("X-Api-Key"))
	w.Header().Set("X-U-Authz", r.Header.Get("Authorization"))
	w.Header().Set("X-U-Path", r.URL.Path)
	w.Header().Set("X-U-Query", r.URL.RawQuery)
	w.Header().Set("X-U-Method", r.Method)
	w.Header().Set("X-U-Body", string(body))
	w.Header().Set("X-U-Anthropic", r.Header.Get("Anthropic-Version"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true,"echo":` + strconv.Quote(string(body)) + `}`))
}

type gatewayOpts struct {
	passthroughOn    bool
	allowPassthrough bool
	scheme           string
	authHeader       string
	extra            map[string]string
	endpointOverride string
	totalLimit       int  // quota total limit; <=0 means default 100000
	quotaExhausted   bool // when true, insert quota_total_limit=0 (immediately exhausted)
	upstreamH        http.HandlerFunc
}

// newTestGateway builds a fully-wired PassthroughHandler backed by an
// in-memory DB, a seeded Anthropic-style passthrough provider, and a user
// with a valid sub-key + quota. It returns the gateway httptest server and
// the client sub-key to authenticate with.
func newTestGateway(t *testing.T, opts gatewayOpts) (*httptest.Server, string) {
	t.Helper()

	os.Setenv("GATEWAY_KEK_ENV", "test-kek-for-unit-tests!!")
	kek, err := security.DeriveKEK()
	if err != nil {
		t.Fatalf("derive KEK: %v", err)
	}

	// Upstream stub.
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
	_, err = store.CreateProvider("Anthropic", "anthropic", endpoint,
		"sk-real-upstream-key", false, opts.allowPassthrough, authHeader, scheme, opts.extra, 0, 0, 0, 0, "")
	if err != nil {
		t.Fatalf("seed provider: %v", err)
	}

	// User with a known sub-key + quota.
	subKey := auth.GenerateSubKey("pptest", 1)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	now := db.Now()
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, route_mode, fixed_provider, max_body_size, max_concurrency, created_at, updated_at)
		 VALUES (?, 'x', ?, ?, 'user', 'active', 'auto', '', 1048576, 10, ?, ?)`,
		"pptest", subHash, subPreview, now, now,
	); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	totalLimit := opts.totalLimit
	if totalLimit <= 0 {
		totalLimit = 100000
	}
	// An explicit exhausted state (quota_total_limit = 0) cannot be expressed
	// via totalLimit alone, because 0 is also the zero-value that means
	// "use the default". quotaExhausted makes the intent unambiguous.
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
	return gw, subKey
}

// doPassthrough issues a request to the gateway's /v1/passthrough subtree.
func doPassthrough(t *testing.T, gw *httptest.Server, subKey, method, subPath, rawQuery, body string) *http.Response {
	t.Helper()
	url := gw.URL + "/v1/passthrough" + subPath
	if rawQuery != "" {
		url += "?" + rawQuery
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+subKey)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

// TestPassthrough_ForwardAndKeyHiding verifies wildcard forwarding
// (sub-path + query + body preserved), key hiding (real key injected,
// client sub-key never reaches upstream), and extra-header injection.
func TestPassthrough_ForwardAndKeyHiding(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "x-api-key",
		authHeader:       "X-Api-Key",
		extra:            map[string]string{"anthropic-version": "2023-06-01"},
	})

	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "foo=bar",
		`{"jsonrpc":"2.0"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-U-Path"); got != "/mcp" {
		t.Errorf("upstream path = %q, want /mcp", got)
	}
	if got := resp.Header.Get("X-U-Query"); got != "foo=bar" {
		t.Errorf("upstream query = %q, want foo=bar", got)
	}
	if got := resp.Header.Get("X-U-Method"); got != "POST" {
		t.Errorf("upstream method = %q, want POST", got)
	}
	if got := resp.Header.Get("X-U-Body"); got != `{"jsonrpc":"2.0"}` {
		t.Errorf("upstream body = %q, want the original JSON", got)
	}
	// Key hiding: real upstream key injected, client sub-key MUST NOT appear.
	if got := resp.Header.Get("X-U-Auth"); got != "sk-real-upstream-key" {
		t.Errorf("upstream auth = %q, want the real injected key", got)
	}
	if got := resp.Header.Get("X-U-Authz"); got != "" {
		t.Errorf("upstream Authorization should be empty (sub-key hidden), got %q", got)
	}
	if got := resp.Header.Get("X-U-Auth"); strings.Contains(got, subKey) {
		t.Error("client sub-key leaked to upstream auth")
	}
	// Extra static header injected.
	if got := resp.Header.Get("X-U-Anthropic"); got != "2023-06-01" {
		t.Errorf("extra header anthropic-version = %q, want 2023-06-01", got)
	}
}

// TestPassthrough_BearerInjection verifies the "bearer" scheme injects
// "Authorization: Bearer <realkey>" and the client sub-key is stripped.
func TestPassthrough_BearerInjection(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		scheme:           "bearer",
		authHeader:       "Authorization",
	})

	resp := doPassthrough(t, gw, subKey, "POST", "/v1/messages", "", `{"model":"claude"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-U-Authz"); got != "Bearer sk-real-upstream-key" {
		t.Errorf("upstream Authorization = %q, want Bearer <realkey>", got)
	}
	if got := resp.Header.Get("X-U-Auth"); got != "" {
		t.Errorf("X-Api-Key should be empty for bearer scheme, got %q", got)
	}
}

// TestPassthrough_GlobalSwitchOff verifies the global master switch
// (P0 default off behaviour) rejects with 403 passthrough_disabled.
func TestPassthrough_GlobalSwitchOff(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    false,
		allowPassthrough: true,
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	assertErrType(t, resp, "passthrough_disabled")
}

// TestPassthrough_ProviderDisabled verifies the per-provider gate: even
// with the global switch on, a provider with allow_passthrough=false is 403.
func TestPassthrough_ProviderDisabled(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: false,
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
	assertErrType(t, resp, "passthrough_disabled")
}

// TestPassthrough_QuotaExceeded verifies quota exhaustion yields 429.
func TestPassthrough_QuotaExceeded(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		quotaExhausted:   true, // immediately exhausted (quota_total_limit = 0)
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	assertErrType(t, resp, "quota_exceeded")
}

// TestPassthrough_StreamingSSE verifies an upstream SSE stream is
// forwarded live and marked X-Accel-Buffering: no.
func TestPassthrough_StreamingSSE(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, evt := range []string{"data: 1", "data: 2", "data: done"} {
			_, _ = w.Write([]byte(evt + "\n"))
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	})
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		upstreamH:        upstreamH,
	})
	resp := doPassthrough(t, gw, subKey, "GET", "/sse", "", "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("expected X-Accel-Buffering: no, got %q", got)
	}
	if _, ok := resp.Header["Content-Length"]; ok {
		t.Error("streaming response should not carry Content-Length")
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	if !strings.Contains(got, "data: 1") || !strings.Contains(got, "data: done") {
		t.Errorf("SSE body not forwarded: %q", got)
	}
}

// TestPassthrough_Upstream4xx verifies an upstream 4xx is forwarded
// verbatim (status + body, no wrapping).
func TestPassthrough_Upstream4xx(t *testing.T) {
	upstreamH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"mcp not found"}`))
	})
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		upstreamH:        upstreamH,
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 forwarded, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mcp not found") {
		t.Errorf("upstream 4xx body not forwarded: %q", string(body))
	}
}

// TestPassthrough_UpstreamUnreachable verifies an unreachable upstream
// yields 502 upstream_error.
func TestPassthrough_UpstreamUnreachable(t *testing.T) {
	gw, subKey := newTestGateway(t, gatewayOpts{
		passthroughOn:    true,
		allowPassthrough: true,
		endpointOverride: "http://127.0.0.1:1", // closed port
	})
	resp := doPassthrough(t, gw, subKey, "POST", "/mcp", "", `{}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	assertErrType(t, resp, "upstream_error")
}

// assertErrType checks the gateway JSON error body's "type" field.
func assertErrType(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if body.Error.Type != want {
		t.Errorf("error type = %q, want %q", body.Error.Type, want)
	}
}
