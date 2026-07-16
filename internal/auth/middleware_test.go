package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// setupTestDB opens a throwaway SQLite DB and applies all migrations.
func setupTestDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth_test.db")
	database, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// createUser inserts a user and returns its plaintext sub-key.
func createUser(t *testing.T, database *db.DB, role, status, expiresAt, routeMode, fixedProvider string) string {
	t.Helper()
	subKey := GenerateSubKey("test-salt", time.Now().UnixNano())
	subKeyHash := HashSubKey(subKey)
	subKeyPreview := SubKeyPreview(subKey)
	if _, err := models.CreateUser(
		database.Conn,
		"user_"+subKey[:8], "phash", subKeyHash, subKeyPreview,
		role, status, expiresAt, routeMode, fixedProvider,
		100, 10000, nil, 0,
	); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return subKey
}

// okHandler is the "next" handler used for positive cases.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

// ctxHandler captures auth context values set by the middleware.
type capturedCtx struct {
	userID        int64
	role          string
	routeMode     string
	fixedProvider string
}

func ctxHandler(got *capturedCtx) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.userID = GetUserID(r)
		got.role, _ = r.Context().Value(CtxKeyUserRole).(string)
		got.routeMode = GetRouteMode(r)
		got.fixedProvider = GetFixedProvider(r)
		w.WriteHeader(http.StatusOK)
	})
}

type errBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
}

func decodeErr(t *testing.T, rr *httptest.ResponseRecorder) errBody {
	t.Helper()
	var b errBody
	if err := json.Unmarshal(rr.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, rr.Body.String())
	}
	return b
}

func TestSubKeyAuth_MissingHeader(t *testing.T) {
	m := NewMiddleware(setupTestDB(t).Conn)
	rr := httptest.NewRecorder()
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "invalid_api_key" {
		t.Errorf("error type = %q, want invalid_api_key", b.Error.Type)
	}
}

func TestSubKeyAuth_InvalidFormat(t *testing.T) {
	m := NewMiddleware(setupTestDB(t).Conn)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-key")
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "invalid_api_key" {
		t.Errorf("error type = %q, want invalid_api_key", b.Error.Type)
	}
}

func TestSubKeyAuth_ValidNonAdmin(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := createUser(t, database, "user", "active", "", "auto", "")
	var got capturedCtx
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(ctxHandler(&got)).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.userID <= 0 {
		t.Errorf("userID not set in context: %d", got.userID)
	}
	if got.role != "user" {
		t.Errorf("role = %q, want user", got.role)
	}
	if got.routeMode != "auto" {
		t.Errorf("routeMode = %q, want auto", got.routeMode)
	}
}

func TestSubKeyAuth_FixedProviderContext(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := createUser(t, database, "user", "active", "", "fixed", "zhipu")
	var got capturedCtx
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(ctxHandler(&got)).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.routeMode != "fixed" || got.fixedProvider != "zhipu" {
		t.Errorf("route_mode/fixed_provider = %q/%q, want fixed/zhipu", got.routeMode, got.fixedProvider)
	}
}

func TestSubKeyAuth_Expired(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	expired := time.Now().Add(-time.Hour).Format(time.RFC3339)
	subKey := createUser(t, database, "user", "active", expired, "auto", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "key_expired" {
		t.Errorf("error type = %q, want key_expired", b.Error.Type)
	}
}

func TestSubKeyAuth_Disabled(t *testing.T) {
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := createUser(t, database, "user", "disabled", "", "auto", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "key_revoked" {
		t.Errorf("error type = %q, want key_revoked", b.Error.Type)
	}
}

func TestSubKeyAuth_DeletedReturns401(t *testing.T) {
	// A deleted key is filtered out by GetUserBySubKeyHash, so the middleware
	// treats it as "key not found" -> 401 (not 403).
	database := setupTestDB(t)
	m := NewMiddleware(database.Conn)
	subKey := createUser(t, database, "user", "deleted", "", "auto", "")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (deleted key is invisible)", rr.Code)
	}
}

func TestSubKeyAuth_AdminExemptFromExpiry(t *testing.T) {
	// CreateUser forces admins to no-expiry, so we insert an admin with a past
	// expires_at directly to prove the Role != "admin" guard short-circuits.
	database := setupTestDB(t)
	subKey := GenerateSubKey("test-salt", time.Now().UnixNano())
	if _, err := database.Conn.Exec(
		`INSERT INTO users (username, password_hash, sub_key_hash, sub_key_preview, role, status, expires_at, route_mode, fixed_provider, created_at, updated_at)
		 VALUES (?, 'phash', ?, 'sk-prev', 'admin', 'active', ?, 'auto', '', datetime('now'), datetime('now'))`,
		"admin_exp", HashSubKey(subKey), time.Now().Add(-time.Hour).Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert admin: %v", err)
	}
	m := NewMiddleware(database.Conn)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	m.SubKeyAuth(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin with past expiry status = %d, want 200 (admin exempt)", rr.Code)
	}
}

func TestAdminSessionAuthAPI_NoCookie(t *testing.T) {
	m := NewMiddleware(setupTestDB(t).Conn)
	rr := httptest.NewRecorder()
	m.AdminSessionAuthAPI(okHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/api/users", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "not_authenticated" {
		t.Errorf("error type = %q, want not_authenticated", b.Error.Type)
	}
}

func TestAdminSessionAuthAPI_InvalidCookie(t *testing.T) {
	m := NewMiddleware(setupTestDB(t).Conn)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/users", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: "bogus-token"})
	m.AdminSessionAuthAPI(okHandler()).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if b := decodeErr(t, rr); b.Error.Type != "session_expired" {
		t.Errorf("error type = %q, want session_expired", b.Error.Type)
	}
}

func TestAdminSessionAuthAPI_ValidSession(t *testing.T) {
	database := setupTestDB(t)
	token := GenerateSessionToken()
	if _, err := models.CreateSession(database.Conn, token, 24); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	m := NewMiddleware(database.Conn)
	var got capturedCtx
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/users", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: token})
	m.AdminSessionAuthAPI(ctxHandler(&got)).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got.role != "admin" {
		t.Errorf("role = %q, want admin", got.role)
	}
}

func TestAdminSessionAuth_RedirectsWhenUnauthenticated(t *testing.T) {
	m := NewMiddleware(setupTestDB(t).Conn)
	rr := httptest.NewRecorder()
	m.AdminSessionAuth(okHandler()).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/admin/users", nil))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 redirect", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", loc)
	}
}
