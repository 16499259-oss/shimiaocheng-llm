package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// callQuotaEndpoint drives GET /v1/quota for a freshly minted user carrying the
// given expiresAt, and returns the recorded response. It isolates the expiry
// propagation path so each edge-case test below stays focused and declarative.
func callQuotaEndpoint(t *testing.T, database *db.DB, expiresAt string) *httptest.ResponseRecorder {
	t.Helper()

	subKey := auth.GenerateSubKey("exp", time.Now().UnixNano())
	subHash := auth.HashSubKey(subKey)
	subPreview := auth.SubKeyPreview(subKey)
	if _, err := models.CreateUser(database.Conn, "exp_"+subKey, "pw", subHash, subPreview,
		"user", "active", expiresAt, "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency); err != nil {
		t.Fatalf("create user: %v", err)
	}

	multEng := quota.NewMultiplierEngine(database.Conn)
	h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
	authMW := auth.NewMiddleware(database.Conn)
	wrapped := authMW.SubKeyAuth(h)

	req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
	req.Header.Set("Authorization", "Bearer "+subKey)
	rec := httptest.NewRecorder()
	wrapped.ServeHTTP(rec, req)
	return rec
}

// TestQuotaHandler_ExpiresAt_Scenarios verifies the /v1/quota endpoint returns
// the stored expires_at verbatim for every user that actually reaches the
// handler. The handler must NOT validate, normalize, or reformat — the
// frontend's renderExpiryRow() owns the interpretation (永久 / 已过期 /
// X天后到期). If the backend ever "helpfully" rewrites the value, this table
// catches it.
//
// Note on scope: /v1/quota is gated by authMW.SubKeyAuth, and the self-service
// panel calls it with the user's sub-key. The auth middleware fails CLOSED —
// accounts with a past or unparseable expires_at are rejected with 403
// key_expired before the handler runs (see internal/auth/middleware_expiry_test.go).
// Those two shapes are therefore NOT reachable here and are intentionally
// excluded; this test only covers the propagation contract for reachable users.
func TestQuotaHandler_ExpiresAt_Scenarios(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	cases := []struct {
		name      string
		expiresAt string
	}{
		{
			name:      "future_rfc3339_utc",
			expiresAt: "2099-12-31T23:59:59Z",
		},
		{
			// Offset-bearing timestamps must survive byte-for-byte. A naive
			// conversion to UTC/Z would still parse in JS, but it is a contract
			// change this test deliberately forbids.
			name:      "future_with_offset",
			expiresAt: "2027-06-15T08:30:00+08:00",
		},
		{
			// Permanent users carry an empty string (NOT NULL DEFAULT '').
			name:      "permanent_empty",
			expiresAt: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := callQuotaEndpoint(t, database, tc.expiresAt)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
			}

			var status models.QuotaStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode quota status: %v", err)
			}
			if status.ExpiresAt != tc.expiresAt {
				t.Fatalf("expected ExpiresAt == %q (verbatim), got %q", tc.expiresAt, status.ExpiresAt)
			}
		})
	}
}

// TestQuotaHandler_ExpiresAt_JSONContract hardens the frontend contract at the
// wire level: the expires_at key must always be present and be a JSON string
// (never null / number / object). Critically, a permanent user must serialize
// as the empty string "" — renderExpiryRow relies on `!expiresAt` to render
// 永久, which a JSON null would also satisfy, but a missing/typed-wrong field
// would break the dashboard.
func TestQuotaHandler_ExpiresAt_JSONContract(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	rec := callQuotaEndpoint(t, database, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}

	val, ok := raw["expires_at"]
	if !ok {
		t.Fatalf("response JSON must contain the expires_at key")
	}
	if val == nil {
		t.Fatalf("expires_at must be a string for permanent users, got JSON null")
	}
	valStr, ok := val.(string)
	if !ok {
		t.Fatalf("expires_at must be a JSON string, got %T (%v)", val, val)
	}
	if valStr != "" {
		t.Fatalf("expected empty string for a permanent user, got %q", valStr)
	}
}
