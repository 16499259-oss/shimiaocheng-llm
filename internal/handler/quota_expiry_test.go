package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llm_api_gateway/internal/auth"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// TestQuotaHandler_ReturnsExpiresAt verifies the /v1/quota endpoint propagates
// the user's expires_at through to the response payload (fix: user-expiry-display).
// The self-service /user/ panel relies on this field to show account expiry.
//
// Covers two states:
//   - a concrete future expiry: response expires_at must equal the stored value
//   - a permanent user (empty string): response expires_at must be "" (永久)
func TestQuotaHandler_ReturnsExpiresAt(t *testing.T) {
	database := openQuotaHandlerTestDB(t)

	cases := []struct {
		name      string
		expiresAt string
	}{
		{
			name:      "with_expiry",
			expiresAt: time.Date(2099, 12, 31, 23, 59, 59, 0, time.UTC).Format(time.RFC3339),
		},
		{
			name:      "permanent_empty",
			expiresAt: "",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subKey := auth.GenerateSubKey("exp", int64(i))
			subHash := auth.HashSubKey(subKey)
			subPreview := auth.SubKeyPreview(subKey)
			u, err := models.CreateUser(database.Conn, "exp_"+tc.name, "pw", subHash, subPreview,
				"user", "active", tc.expiresAt, "auto", "", 1000, 1000, nil, 0, models.DefaultMaxConcurrency)
			if err != nil {
				t.Fatalf("create user: %v", err)
			}

			// Mirror the handler's read path: the canonical stored value is what
			// the endpoint must return.
			fetched, err := models.GetUserByID(database.Conn, u.ID)
			if err != nil || fetched == nil {
				t.Fatalf("get user by id: %v", err)
			}
			wantExpiresAt := fetched.ExpiresAt

			multEng := quota.NewMultiplierEngine(database.Conn)
			h := &QuotaHandler{DB: database.Conn, MultEng: multEng, ResetInterval: 5}
			authMW := auth.NewMiddleware(database.Conn)
			wrapped := authMW.SubKeyAuth(h)

			req := httptest.NewRequest(http.MethodGet, "/v1/quota", nil)
			req.Header.Set("Authorization", "Bearer "+subKey)
			rec := httptest.NewRecorder()
			wrapped.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
			}

			// The decoded struct must carry the expected ExpiresAt.
			var status models.QuotaStatus
			if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
				t.Fatalf("decode quota status: %v", err)
			}
			if status.ExpiresAt != wantExpiresAt {
				t.Fatalf("expected ExpiresAt == %q, got %q", wantExpiresAt, status.ExpiresAt)
			}

			// The raw JSON must actually contain the expires_at key (so the
			// frontend contract is honored even if the value is empty).
			var raw map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("decode raw: %v", err)
			}
			if _, ok := raw["expires_at"]; !ok {
				t.Fatalf("response JSON missing expires_at key")
			}
		})
	}
}
