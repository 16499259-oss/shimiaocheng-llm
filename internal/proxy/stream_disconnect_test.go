package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// TestEnsureStreamUsage pins the include_usage injection used by the streaming
// path: for stream:true we MUST force stream_options.include_usage=true so the
// upstream returns a usage chunk (otherwise many OpenAI-compatible providers
// omit it and the stream path bills 0 Tokens). It must be a no-op for
// non-streaming bodies and must never override an explicit user false.
func TestEnsureStreamUsage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring expected in output
	}{
		{
			name: "stream true injects include_usage",
			in:   `{"model":"glm-5.2","stream":true}`,
			want: `"include_usage":true`,
		},
		{
			name: "stream true with existing empty stream_options",
			in:   `{"model":"glm-5.2","stream":true,"stream_options":{}}`,
			want: `"include_usage":true`,
		},
		{
			name: "explicit include_usage true preserved",
			in:   `{"stream":true,"stream_options":{"include_usage":true}}`,
			want: `"include_usage":true`,
		},
		{
			name: "explicit include_usage false honored (no override)",
			in:   `{"stream":true,"stream_options":{"include_usage":false}}`,
			want: `"include_usage":false`,
		},
		{
			name: "stream false untouched",
			in:   `{"stream":false}`,
			want: `"stream":false`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := ensureStreamUsage([]byte(c.in))
			if !bytes.Contains(out, []byte(c.want)) {
				t.Fatalf("input=%s -> output=%s, want substring %q", c.in, string(out), c.want)
			}
		})
	}
}

// TestHandler_ServeHTTPStream_AccountsOnClientDisconnect is the regression test
// for the billing-accuracy bug: a client that aborts the SSE stream mid-flight
// (e.g. stops generation) must STILL be billed for the usage already streamed,
// because the upstream Tokens were already consumed. Before the fix the handler
// returned early on context cancellation and never called AddTokenUsage, so the
// used Token counter was silently under-counted.
func TestHandler_ServeHTTPStream_AccountsOnClientDisconnect(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "disc", "disc")

	// Upstream emits a usage frame, then keeps the stream open with heartbeat
	// comments so the gateway keeps looping and re-checking the client context.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		usage := map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
		data, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": "hi"}}},
			"usage":   usage,
		})
		_, _ = w.Write([]byte("data: " + string(data) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = w.Write([]byte(": ping\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
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
	gw := httptest.NewServer(auth.NewMiddleware(database.Conn).SubKeyAuth(h))
	defer gw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gw.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+subKey)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("client do: %v", err)
	}
	defer resp.Body.Close()

	// Consume the first frame (the usage chunk), then abort the client side.
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("read first frame: %v", err)
	}
	cancel()

	// The handler must bill the already-streamed usage on disconnect.
	deadline := time.Now().Add(3 * time.Second)
	for {
		q, err := models.GetQuota(database.Conn, userID)
		if err == nil && q.QuotaTokenTotalUsed == 15 {
			return
		}
		if time.Now().After(deadline) {
			got := -1
			if q, e := models.GetQuota(database.Conn, userID); e == nil {
				got = q.QuotaTokenTotalUsed
			}
			t.Fatalf("expected quota_token_total_used==15 after client disconnect (billing the streamed usage), got %d", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
