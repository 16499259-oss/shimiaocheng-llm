package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"llm_api_gateway/internal/models"
)

// F1: bare-date expires_at is normalized and stored as canonical RFC3339.
func TestAdminCreateUser_BareDateExpiryNormalized(t *testing.T) {
	h := newAdminTestHandler(t)
	body, _ := json.Marshal(map[string]any{"username": "exp-bare", "expires_at": "2026-08-15"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body=%s", rec.Code, rec.Body.String())
	}
	u, err := models.GetUserByID(h.DB, mustID(t, rec))
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.ExpiresAt != "2026-08-15T23:59:59+08:00" {
		t.Fatalf("expires_at = %q, want 2026-08-15T23:59:59+08:00", u.ExpiresAt)
	}
}

// F1: malformed expires_at is rejected (400), not silently stored.
func TestAdminCreateUser_InvalidExpiryRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	body, _ := json.Marshal(map[string]any{"username": "exp-bad", "expires_at": "2026/08/15"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed expiry, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// F1: extend with a bare-date until is normalized and stored canonically.
func TestAdminExtendUser_BareDateUntilNormalized(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "ext-bare", nil)
	body, _ := json.Marshal(map[string]any{"until": "2026-12-31"})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+strconv.FormatInt(id, 10)+"/extend", bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.ExtendUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.ExpiresAt != "2026-12-31T23:59:59+08:00" {
		t.Fatalf("extended expires_at = %q, want 2026-12-31T23:59:59+08:00", u.ExpiresAt)
	}
}

// F3: negative token cap on edit is rejected (400) and not persisted.
func TestAdminUpdateUser_NegativeTokenLimitRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "tok-neg", nil)
	neg := -1
	body, _ := json.Marshal(map[string]any{"quota_token_total_limit": neg})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative token cap, got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil || q == nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaTokenTotalLimit != 0 {
		t.Fatalf("token cap persisted as %d, want 0 (rejected)", q.QuotaTokenTotalLimit)
	}
}

// F3: negative 5h cap on edit is rejected (400) and not persisted.
func TestAdminUpdateUser_Negative5hLimitRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "q5h-neg", nil)
	neg := -5
	body, _ := json.Marshal(map[string]any{"quota_5h_limit": neg})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative 5h cap, got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil || q == nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.Quota5hLimit <= 0 {
		t.Fatalf("5h cap persisted as %d, want positive (rejected)", q.Quota5hLimit)
	}
}

// mustID extracts the created user id from a CreateUser response recorder.
func mustID(t *testing.T, rec *httptest.ResponseRecorder) int64 {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, ok := resp["id"].(float64)
	if !ok {
		t.Fatalf("missing id: %s", rec.Body.String())
	}
	return int64(id)
}
