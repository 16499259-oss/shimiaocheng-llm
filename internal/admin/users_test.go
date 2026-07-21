// Package admin contains tests for the per-user concurrency cap at the admin
// handler level (T5/T6): create/edit validation of max_concurrency, including
// the default-when-absent, unlimited(0), capped, negative-rejected, and
// over-hard-limit(>200)-rejected contracts.
package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
)

// newAdminTestHandler builds an admin.Handler backed by a migrated temp DB
// with just enough wiring to exercise CreateUser / UpdateUser.
func newAdminTestHandler(t *testing.T) *Handler {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "admin_test_*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp db: %v", err)
	}
	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return &Handler{
		DB:                database.Conn,
		SubKeySalt:        "test-salt-32bytes-abcdefghijklmnop",
		Default5hLimit:    100,
		DefaultTotalLimit: 1000,
	}
}

// adminCreateUser posts a new user (optionally with max_concurrency) and returns
// the created user id and the decoded JSON response.
func adminCreateUser(t *testing.T, h *Handler, username string, mc *int) (int64, map[string]any) {
	t.Helper()
	body := map[string]any{"username": username}
	if mc != nil {
		body["max_concurrency"] = *mc
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateUser(%s) expected 201, got %d; body=%s", username, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	id, ok := resp["id"].(float64)
	if !ok {
		t.Fatalf("response missing id: %s", rec.Body.String())
	}
	return int64(id), resp
}

// T5: max_concurrency absent -> default (10).
func TestAdminCreateUser_MaxConcurrencyNilDefaultsTo10(t *testing.T) {
	h := newAdminTestHandler(t)
	_, resp := adminCreateUser(t, h, "c-nil", nil)
	got, ok := resp["max_concurrency"].(float64)
	if !ok || int(got) != models.DefaultMaxConcurrency {
		t.Fatalf("expected max_concurrency=%d (default), got %v", models.DefaultMaxConcurrency, resp["max_concurrency"])
	}
}

// T5: max_concurrency=0 -> unlimited (persisted as 0).
func TestAdminCreateUser_MaxConcurrencyZeroUnlimited(t *testing.T) {
	h := newAdminTestHandler(t)
	mc := 0
	id, resp := adminCreateUser(t, h, "c-zero", &mc)
	got, ok := resp["max_concurrency"].(float64)
	if !ok || int(got) != 0 {
		t.Fatalf("expected max_concurrency=0 (unlimited), got %v", resp["max_concurrency"])
	}
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.MaxConcurrency != 0 {
		t.Fatalf("DB max_concurrency expected 0, got %d", u.MaxConcurrency)
	}
}

// T5: max_concurrency=50 -> capped (persisted as 50).
func TestAdminCreateUser_MaxConcurrencyPositive(t *testing.T) {
	h := newAdminTestHandler(t)
	mc := 50
	id, resp := adminCreateUser(t, h, "c-pos", &mc)
	got, ok := resp["max_concurrency"].(float64)
	if !ok || int(got) != 50 {
		t.Fatalf("expected max_concurrency=50, got %v", resp["max_concurrency"])
	}
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.MaxConcurrency != 50 {
		t.Fatalf("DB max_concurrency expected 50, got %d", u.MaxConcurrency)
	}
}

// T5: max_concurrency=-1 -> 400.
func TestAdminCreateUser_MaxConcurrencyNegativeRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	mc := -1
	body, _ := json.Marshal(map[string]any{"username": "c-neg", "max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for negative max_concurrency, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// T5: max_concurrency=201 (>hard limit 200) -> 400.
func TestAdminCreateUser_MaxConcurrencyOverHardLimitRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	mc := 201
	body, _ := json.Marshal(map[string]any{"username": "c-over", "max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.CreateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for max_concurrency > 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// T6: edit max_concurrency=201 (>hard limit) -> 400.
func TestAdminUpdateUser_MaxConcurrencyOverHardLimitRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "u-over", nil)
	mc := 201
	body, _ := json.Marshal(map[string]any{"max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for update max_concurrency > 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// T6: edit max_concurrency=-1 -> 400.
func TestAdminUpdateUser_MaxConcurrencyNegativeRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "u-neg", nil)
	mc := -1
	body, _ := json.Marshal(map[string]any{"max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for update negative max_concurrency, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// T6: edit max_concurrency=0 -> unlimited (persisted as 0).
func TestAdminUpdateUser_MaxConcurrencyZeroUnlimited(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "u-zero", nil)
	mc := 0
	body, _ := json.Marshal(map[string]any{"max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for update max_concurrency=0, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if got, ok := resp["max_concurrency"].(float64); !ok || int(got) != 0 {
		t.Fatalf("expected response max_concurrency=0, got %v", resp["max_concurrency"])
	}
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.MaxConcurrency != 0 {
		t.Fatalf("DB max_concurrency expected 0, got %d", u.MaxConcurrency)
	}
}

// T6: edit max_concurrency=75 -> capped (persisted as 75).
func TestAdminUpdateUser_MaxConcurrencyPositive(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "u-pos", nil)
	mc := 75
	body, _ := json.Marshal(map[string]any{"max_concurrency": mc})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for update max_concurrency=75, got %d; body=%s", rec.Code, rec.Body.String())
	}
	u, err := models.GetUserByID(h.DB, id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if u.MaxConcurrency != 75 {
		t.Fatalf("DB max_concurrency expected 75, got %d", u.MaxConcurrency)
	}
}

// L2: edit quota_5h_limit = 0 -> 200 (unlimited). Since 2026-07-21 a count
// limit of 0 means "call-count not restricted" and the gate opens
// unconditionally, so the API ACCEPTS it (mirrors the Token cap, where 0 also
// means unlimited). Only negative values are rejected.
func TestAdminUpdateUser_Zero5hLimitAcceptedAsUnlimited(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "q5h-zero", nil)
	zero := 0
	body, _ := json.Marshal(map[string]any{"quota_5h_limit": zero})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for update quota_5h_limit=0 (unlimited), got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil || q == nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.Quota5hLimit != 0 {
		t.Fatalf("5h cap persisted as %d, want 0 (unlimited)", q.Quota5hLimit)
	}
}

// L2: edit quota_total_limit = 0 -> 200 (unlimited). Same rationale as the 5h
// cap: a 0 count limit means "call-count not restricted" and is accepted.
func TestAdminUpdateUser_ZeroTotalLimitAcceptedAsUnlimited(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "qt-zero", nil)
	zero := 0
	body, _ := json.Marshal(map[string]any{"quota_total_limit": zero})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for update quota_total_limit=0 (unlimited), got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil || q == nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaTotalLimit != 0 {
		t.Fatalf("total cap persisted as %d, want 0 (unlimited)", q.QuotaTotalLimit)
	}
}

// L2+: edit quota_token_total_limit = 0 -> 200 (NOT rejected). Unlike the count
// quotas, a 0 cumulative Token cap means "unlimited", so the API must accept it.
// This is the positive counterpart to the count-quota 0-rejection contract and
// proves an admin can "unlimit" a user's Token cap. The value is persisted as 0.
func TestAdminUpdateUser_ZeroTokenTotalLimitAllowed(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "qtok-zero", nil)
	zero := 0
	body, _ := json.Marshal(map[string]any{"quota_token_total_limit": zero})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+strconv.FormatInt(id, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for update quota_token_total_limit=0 (unlimited), got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if got, ok := resp["quota_token_total_limit"].(float64); !ok || int(got) != 0 {
		t.Fatalf("expected response quota_token_total_limit=0, got %v", resp["quota_token_total_limit"])
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil || q == nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaTokenTotalLimit != 0 {
		t.Fatalf("token total cap expected 0 (unlimited), got %d", q.QuotaTokenTotalLimit)
	}
}
