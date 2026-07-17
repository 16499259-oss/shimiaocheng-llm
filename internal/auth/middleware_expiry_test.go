package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
)

// insertRawUser inserts a user with an arbitrary expires_at string (bypassing
// the API normalization) so we can test how the middleware treats malformed or
// bare-date values already present in the DB.
func insertRawUser(t *testing.T, database *db.DB, role, status, expiresAt string) string {
	t.Helper()
	subKey := GenerateSubKey("test-salt", time.Now().UnixNano())
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, expires_at, route_mode, fixed_provider, created_at, updated_at)
		 VALUES (?, 'phash', ?, 'sk-prev', ?, ?, ?, 'auto', '', datetime('now'), datetime('now'))`,
		"u_"+subKey[:8], HashSubKey(subKey), role, status, expiresAt,
	); err != nil {
		t.Fatalf("insert raw user: %v", err)
	}
	return subKey
}

// F1: a malformed (non-empty, non-parseable) expiry must fail-closed -> 403.
func TestSubKeyAuth_MalformedExpiryFailClosed(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := insertRawUser(t, database, "user", "active", "2026/08/15")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (fail-closed)", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "key_expired" {
		t.Errorf("error type = %q, want key_expired", b.Error.Type)
	}
}

// F1: a future bare date is now treated as a real expiry -> request allowed.
func TestSubKeyAuth_BareDateFutureAllowed(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := insertRawUser(t, database, "user", "active", "2099-01-01")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (future bare date is valid)", rr.Code)
	}
}

// F1: a past bare date is honored as expired -> 403 (previously silent permanent).
func TestSubKeyAuth_BareDatePastExpired(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := insertRawUser(t, database, "user", "active", "2000-01-01")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (past bare date expired)", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "key_expired" {
		t.Errorf("error type = %q, want key_expired", b.Error.Type)
	}
}
