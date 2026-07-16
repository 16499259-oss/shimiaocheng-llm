// Package proxy tests for the per-user body budget auto-compaction feature.
package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/config"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/router"
)

func TestCompactMessagesToBudget(t *testing.T) {
	system := `{"role":"system","content":"You are a helpful assistant."}`
	userMsg := func(n int) string {
		ch := byte('A' + n%26)
		return `{"role":"user","content":"msg ` + string(ch) + ` ` + strings.Repeat("x", 40) + `"}`
	}

	var msgs []string
	msgs = append(msgs, system)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, userMsg(i))
	}
	body := []byte(`{"model":"glm-5.2","messages":[` + strings.Join(msgs, ",") + `]}`)

	// Within budget -> returned unchanged.
	if got := compactMessagesToBudget(body, 1<<20); len(got) != len(body) {
		t.Fatalf("within-budget body should be unchanged, got len %d want %d", len(got), len(body))
	}

	// Over budget -> trimmed, system preserved, fewer messages, fits budget.
	const budget = 700
	got := compactMessagesToBudget(body, budget)
	if len(got) > budget {
		t.Fatalf("compacted body len %d exceeds budget %d", len(got), budget)
	}
	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("compacted body not valid JSON: %v", err)
	}
	if len(parsed.Messages) >= 11 {
		t.Fatalf("expected messages to be trimmed, got %d", len(parsed.Messages))
	}
	var first struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(parsed.Messages[0], &first); err != nil || first.Role != "system" {
		t.Fatalf("system message not preserved at front: %+v", first)
	}

	// Invalid JSON -> unchanged.
	if got := compactMessagesToBudget([]byte("not json"), 10); string(got) != "not json" {
		t.Fatalf("invalid JSON should be returned unchanged")
	}

	// No messages field -> unchanged.
	noMsgs := `{"model":"glm-5.2"}`
	if got := compactMessagesToBudget([]byte(noMsgs), 5); string(got) != noMsgs {
		t.Fatalf("body without messages should be unchanged")
	}

	// budget <= 0 -> unchanged.
	if got := compactMessagesToBudget(body, 0); len(got) != len(body) {
		t.Fatalf("budget<=0 should be unchanged")
	}
}

// TestHandler_ServeHTTP_CompactsOverBudgetRequest verifies the end-to-end
// behaviour: a request that exceeds the user's per-request body budget is
// auto-compacted (history trimmed, system preserved) and forwarded to the
// upstream with HTTP 200 — never a 413.
func TestHandler_ServeHTTP_CompactsOverBudgetRequest(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("qc", 7)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	// Small per-user budget (1000 bytes) forces compaction of an oversized request.
	if _, err := models.CreateUser(database.Conn, "qc", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 1000); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// Echo upstream captures the (compacted) request body and returns 200.
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: upstream.URL, APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)

	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return upstream.URL },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
		Compaction:     CompactionTrim,
	}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	// Oversized request: system + many large user messages (>1000 bytes total).
	var msgs []string
	msgs = append(msgs, `{"role":"system","content":"You are a helpful assistant."}`)
	for i := 0; i < 12; i++ {
		msgs = append(msgs, `{"role":"user","content":"`+strings.Repeat("y", 80)+`"}`)
	}
	body := []byte(`{"model":"glm-5.2","messages":[` + strings.Join(msgs, ",") + `]}`)
	if len(body) <= 1000 {
		t.Fatalf("test setup: body should exceed budget, got %d", len(body))
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (auto-compacted, not 413), got %d; body=%s", rec.Code, rec.Body.String())
	}

	var parsed struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("upstream received invalid JSON: %v", err)
	}
	if len(parsed.Messages) >= 13 {
		t.Fatalf("expected upstream to receive a trimmed message list, got %d", len(parsed.Messages))
	}
	var first struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(parsed.Messages[0], &first); err != nil || first.Role != "system" {
		t.Fatalf("system message must be preserved at front: %+v", first)
	}
	if !strings.Contains(first.Content, "helpful") {
		t.Fatalf("system content not preserved: %q", first.Content)
	}
	// The forwarded (compacted) body must fit the user's 1000-byte budget.
	if len(gotBody) > 1000+128 {
		t.Fatalf("forwarded body len %d exceeds budget+margin", len(gotBody))
	}
}

// TestHandler_ServeHTTP_OverCeilingStill413 verifies that a request above the
// absolute 32MB ceiling is still rejected with 413, even with compaction on.
func TestHandler_ServeHTTP_OverCeilingStill413(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("qc3", 9)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	if _, err := models.CreateUser(database.Conn, "qc3", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 1<<20); err != nil {
		t.Fatalf("create user: %v", err)
	}

	os.Setenv("ZHIPU_API_KEY", "sk-zhipu")
	defer os.Unsetenv("ZHIPU_API_KEY")

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "zhipu", Endpoint: "https://zhipu.example/v1", APIKeyEnv: "ZHIPU_API_KEY", IsDefault: true},
		},
	}
	creds := router.NewCredentialStore()
	creds.Set("zhipu", "sk-zhipu")
	rt := newProxyTestRouter(t, database, cfg)
	multEng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, multEng, 5)

	h := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return cfg.Providers[0].Endpoint },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
		Compaction:     CompactionTrim,
	}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	// A body larger than the 32MB ceiling (35MB of padding).
	huge := make([]byte, 35<<20)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(huge))
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for over-ceiling request, got %d; body=%s", rec.Code, rec.Body.String())
	}
}
