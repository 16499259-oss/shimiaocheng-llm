// Package proxy — privacy test: raw request body must NOT be logged on JSON
// parse failure unless Debug mode is explicitly enabled.
package proxy

import (
	"bytes"
	"log"
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

// TestHandler_DebugBodyDump verifies that a malformed (unparseable) request
// body is never written to logs in production (Debug=false), so user
// conversation content cannot leak into log sinks. With Debug=true the raw
// body IS dumped, aiding troubleshooting.
func TestHandler_DebugBodyDump(t *testing.T) {
	database := openProxyTestDB(t)

	subKey := auth.GenerateSubKey("dbg", 3)
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	if _, err := models.CreateUser(database.Conn, "dbg", "pw", subHash, subPreview,
		"user", "active", "", "auto", "", 1_000_000, 1_000_000, nil, 1<<20, models.DefaultMaxConcurrency); err != nil {
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

	// A malformed JSON body that embeds a unique secret marker.
	const marker = "LEAK_MARKER_9f3a2b"
	malformed := []byte(`{"model":"glm-5.2","messages":[` + marker + `]`)

	authMW := auth.NewMiddleware(database.Conn)

	oldOut := log.Writer()
	defer log.SetOutput(oldOut)

	// ── Case 1: Debug=false (production default) ──
	var buf1 bytes.Buffer
	log.SetOutput(&buf1)
	h1 := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return cfg.Providers[0].Endpoint },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
		Compaction:     CompactionTrim,
		Debug:          false,
	}
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(malformed))
	req1.Header.Set("Authorization", "Bearer "+subKey)
	authMW.SubKeyAuth(h1).ServeHTTP(rec1, req1)
	log.SetOutput(oldOut)

	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("Debug=false: expected 400 for malformed JSON, got %d; body=%s", rec1.Code, rec1.Body.String())
	}
	if strings.Contains(buf1.String(), marker) {
		t.Fatalf("Debug=false: raw request body leaked into logs:\n%s", buf1.String())
	}

	// ── Case 2: Debug=true (troubleshooting) ──
	var buf2 bytes.Buffer
	log.SetOutput(&buf2)
	h2 := &Handler{
		APIKeyGetter:   func() string { return creds.Get("zhipu") },
		EndpointGetter: func() string { return cfg.Providers[0].Endpoint },
		QuotaChecker:   checker,
		MultiplierEng:  multEng,
		Router:         rt,
		Compaction:     CompactionTrim,
		Debug:          true,
	}
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(malformed))
	req2.Header.Set("Authorization", "Bearer "+subKey)
	authMW.SubKeyAuth(h2).ServeHTTP(rec2, req2)
	log.SetOutput(oldOut)

	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("Debug=true: expected 400 for malformed JSON, got %d; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(buf2.String(), marker) {
		t.Fatalf("Debug=true: expected raw request body in logs for troubleshooting, got:\n%s", buf2.String())
	}
}
