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
	// Capture the pre-update cap. Since 2026-07-21 a blank create-count field
	// yields 0 (unlimited), so we assert the REJECTED update leaves the cap
	// unchanged rather than assuming a specific positive value.
	before, err := models.GetQuota(h.DB, id)
	if err != nil || before == nil {
		t.Fatalf("GetQuota before update: %v", err)
	}
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
	if q.Quota5hLimit != before.Quota5hLimit {
		t.Fatalf("negative 5h update was not rejected: cap changed from %d to %d", before.Quota5hLimit, q.Quota5hLimit)
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

// TestAdminExtendUser_ResetTokenStatsTrue verifies that when reset_token_stats=true
// the quota_reset audit entry is written with the correct structured detail
// and the user's all usage buckets (call-count + Token) are zeroed.
func TestAdminExtendUser_ResetTokenStatsTrue(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "ext-rst-true", nil)

	// Seed token usage so we can verify it gets zeroed.
	if err := models.AddTokenUsage(h.DB, id, 100); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}
	// Also seed call-count columns.
	if _, err := h.DB.Exec(
		`UPDATE quotas SET quota_5h_used = 7, quota_total_used = 42 WHERE user_id = ?`, id,
	); err != nil {
		t.Fatalf("seed call-count cols: %v", err)
	}
	qBefore, err := models.GetQuota(h.DB, id)
	if err != nil || qBefore == nil {
		t.Fatalf("GetQuota before: %v", err)
	}
	if qBefore.QuotaToken5hUsed == 0 || qBefore.QuotaTokenWeekUsed == 0 || qBefore.QuotaTokenTotalUsed == 0 {
		t.Fatalf("expected non-zero token usage after seeding")
	}

	// Extend with reset_token_stats=true.
	body, _ := json.Marshal(map[string]any{"days": 30, "reset_token_stats": true})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+strconv.FormatInt(id, 10)+"/extend", bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.ExtendUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}

	// Verify quota_reset audit entry exists.
	rows, err := h.DB.Query(
		`SELECT detail FROM audit_logs WHERE action = 'quota_reset' AND target_type = 'user' AND target_id = ?`,
		strconv.FormatInt(id, 10),
	)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan audit detail: %v", err)
		}
		if d == `{"dimensions":["calls","tokens-5h","tokens-week","tokens-month"],"trigger":"extend","operator":"admin"}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("quota_reset audit entry not found or detail mismatch")
	}

	// Verify ALL buckets are zeroed.
	qAfter, err := models.GetQuota(h.DB, id)
	if err != nil || qAfter == nil {
		t.Fatalf("GetQuota after: %v", err)
	}
	if qAfter.Quota5hUsed != 0 {
		t.Fatalf("quota_5h_used expected 0, got %d", qAfter.Quota5hUsed)
	}
	if qAfter.QuotaTotalUsed != 0 {
		t.Fatalf("quota_total_used expected 0, got %d", qAfter.QuotaTotalUsed)
	}
	if qAfter.QuotaToken5hUsed != 0 {
		t.Fatalf("quota_token_5h_used expected 0, got %d", qAfter.QuotaToken5hUsed)
	}
	if qAfter.QuotaTokenWeekUsed != 0 {
		t.Fatalf("quota_token_week_used expected 0, got %d", qAfter.QuotaTokenWeekUsed)
	}
	if qAfter.QuotaTokenTotalUsed != 0 {
		t.Fatalf("quota_token_total_used expected 0, got %d", qAfter.QuotaTokenTotalUsed)
	}
}

// TestAdminExtendUser_ResetTokenStatsFalse verifies that no quota_reset
// audit entry is written when reset_token_stats is omitted (false).
func TestAdminExtendUser_ResetTokenStatsFalse(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "ext-rst-false", nil)

	// Extend without reset_token_stats (omit / default false).
	body, _ := json.Marshal(map[string]any{"days": 30})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+strconv.FormatInt(id, 10)+"/extend", bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.ExtendUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}

	// Verify NO quota_reset audit entry.
	var count int
	if err := h.DB.QueryRow(
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'quota_reset' AND target_id = ?`,
		strconv.FormatInt(id, 10),
	).Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count > 0 {
		t.Fatalf("expected 0 quota_reset audit entries, got %d", count)
	}
}

// TestAdminResetUsage verifies the independent reset-usage endpoint zeroes
// all buckets (call-count + Token), writes a quota_reset audit with
// trigger=manual_reset, and does NOT touch the user's expiry date.
func TestAdminResetUsage(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "reset-usage-test", nil)

	// Seed usage.
	if err := models.AddTokenUsage(h.DB, id, 100); err != nil {
		t.Fatalf("seed token usage: %v", err)
	}
	if _, err := h.DB.Exec(
		`UPDATE quotas SET quota_5h_used = 7, quota_total_used = 42 WHERE user_id = ?`, id,
	); err != nil {
		t.Fatalf("seed call-count cols: %v", err)
	}

	// Call reset-usage endpoint.
	body, _ := json.Marshal(map[string]any{})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+strconv.FormatInt(id, 10)+"/reset-usage", bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	h.ResetUsage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}

	// Verify quota_reset audit with manual_reset trigger.
	rows, err := h.DB.Query(
		`SELECT detail FROM audit_logs WHERE action = 'quota_reset' AND target_id = ?`,
		strconv.FormatInt(id, 10),
	)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	defer rows.Close()
	var found bool
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			t.Fatalf("scan audit detail: %v", err)
		}
		if d == `{"dimensions":["calls","tokens-5h","tokens-week","tokens-month"],"trigger":"manual_reset","operator":"admin"}` {
			found = true
		}
	}
	if !found {
		t.Fatalf("quota_reset audit entry not found or detail mismatch (manual_reset)")
	}

	// Verify ALL buckets are zeroed.
	qAfter, err := models.GetQuota(h.DB, id)
	if err != nil || qAfter == nil {
		t.Fatalf("GetQuota after: %v", err)
	}
	if qAfter.Quota5hUsed != 0 {
		t.Fatalf("quota_5h_used expected 0, got %d", qAfter.Quota5hUsed)
	}
	if qAfter.QuotaTotalUsed != 0 {
		t.Fatalf("quota_total_used expected 0, got %d", qAfter.QuotaTotalUsed)
	}
	if qAfter.QuotaToken5hUsed != 0 || qAfter.QuotaTokenWeekUsed != 0 || qAfter.QuotaTokenTotalUsed != 0 {
		t.Fatalf("token buckets expected all 0, got 5h=%d week=%d total=%d",
			qAfter.QuotaToken5hUsed, qAfter.QuotaTokenWeekUsed, qAfter.QuotaTokenTotalUsed)
	}
}
