// Package proxy contains tests for the dual Token-window (5h + weekly) 429
// classification at the gateway edge.
package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// decodeProxyError decodes the {error:{type,message,code}} body of a 429.
func decodeProxyError(t *testing.T, rec *httptest.ResponseRecorder) struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
} {
	t.Helper()
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 429 body: %v (body=%s)", err, rec.Body.String())
	}
	return resp.Error
}

// TestHandler_ServeHTTP_Token5hQuotaExceededReturns429 verifies that when the
// 5h-window Token cap is reached (with a fresh 5h window), the proxy returns 429
// with type=token_5h_quota_exceeded and the Chinese message 「5 小时内 Token 已超限」.
func TestHandler_ServeHTTP_Token5hQuotaExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "tk5h", "tk5h")

	// 5h Token cap = 100, already used = 100; keep the 5h window fresh.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_5h_limit = 100, quota_token_5h_used = 100, window_start = ? WHERE user_id = ?`,
		time.Now().Format(time.RFC3339), userID,
	); err != nil {
		t.Fatalf("seed 5h token window: %v", err)
	}

	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	e := decodeProxyError(t, rec)
	if e.Type != "token_5h_quota_exceeded" {
		t.Fatalf("expected type=token_5h_quota_exceeded, got %q (body=%s)", e.Type, rec.Body.String())
	}
	if e.Message != "5 小时内 Token 已超限" {
		t.Fatalf("expected message=5 小时内 Token 已超限, got %q", e.Message)
	}
	if e.Code != "token_5h_quota_exceeded" {
		t.Fatalf("expected code == type, got %q", e.Code)
	}
}

// TestHandler_ServeHTTP_TokenWeekQuotaExceededReturns429 verifies that when the
// weekly (rolling-7d) Token cap is reached (with a fresh week_start), the proxy
// returns 429 with type=token_week_quota_exceeded and 「本周 Token 已超限」.
func TestHandler_ServeHTTP_TokenWeekQuotaExceededReturns429(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "tkwk", "tkwk")

	// Weekly Token cap = 100, already used = 100; keep week_start fresh so the
	// lazy reset in the gate does NOT clear it.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_week_limit = 100, quota_token_week_used = 100, week_start = ? WHERE user_id = ?`,
		time.Now().Format(time.RFC3339), userID,
	); err != nil {
		t.Fatalf("seed week token window: %v", err)
	}

	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	e := decodeProxyError(t, rec)
	if e.Type != "token_week_quota_exceeded" {
		t.Fatalf("expected type=token_week_quota_exceeded, got %q (body=%s)", e.Type, rec.Body.String())
	}
	if e.Message != "本周 Token 已超限" {
		t.Fatalf("expected message=本周 Token 已超限, got %q", e.Message)
	}
	if e.Code != "token_week_quota_exceeded" {
		t.Fatalf("expected code == type, got %q", e.Code)
	}
}

// TestHandler_ServeHTTP_Token5hStaleWindowFallsThroughToCount pins the
// freshness guard: a 5h Token dimension that is numerically exhausted but whose
// 5h window is STALE (overdue for its reset) must NOT be blamed for the block.
// The block is then correctly attributed to the count quota (quota_exceeded),
// because a stale window would have been reset on the next request.
func TestHandler_ServeHTTP_Token5hStaleWindowFallsThroughToCount(t *testing.T) {
	database := openProxyTestDB(t)
	subKey, userID := newTokenTestUser(t, database, "tksw", "tksw")

	// Exhaust BOTH the count quota and the 5h Token window, but make the 5h
	// window stale (>5h old) so the Token attribution is suppressed.
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_5h_limit = 5, quota_5h_used = 5,
		                  quota_token_5h_limit = 100, quota_token_5h_used = 100,
		                  window_start = ? WHERE user_id = ?`,
		time.Now().Add(-6*time.Hour).Format(time.RFC3339), userID,
	); err != nil {
		t.Fatalf("seed stale 5h window: %v", err)
	}

	rec := doTokenQuotaRequest(t, database, subKey)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	e := decodeProxyError(t, rec)
	if e.Type != "quota_exceeded" {
		t.Fatalf("expected stale 5h-window to fall through to quota_exceeded, got %q (body=%s)", e.Type, rec.Body.String())
	}
}
