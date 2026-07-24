// Package admin — tests for the weekly quota start-anchor feature (T5/T6):
// the dedicated single-user POST /api/users/{id}/week-start action, the general
// edit endpoint (which must IGNORE the anchor entirely), and the batch
// POST /api/users/batch-week-start endpoint.
package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"llm_api_gateway/internal/models"
)

// TestAdminUpdateUser_IgnoresWeekStart verifies the general edit endpoint does
// NOT touch the weekly start anchor at all — even when quota_week_start is sent
// (valid or malformed). Setting the anchor is a separate, dedicated action
// (POST /api/users/{id}/week-start), so editing base settings (concurrency,
// status, limits, …) can never reset a user's weekly Token usage. This is the
// fix for the 2026-07-24 "editing concurrency wiped the week usage" bug.
func TestAdminUpdateUser_IgnoresWeekStart(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weekuser", nil)

	// Seed a known, non-zero in-progress weekly usage + an anchor.
	anchor0 := time.Now().UTC().Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := h.DB.Exec(`UPDATE quotas SET quota_token_week_used = 7, week_start = ?, week_cycle_start = ? WHERE user_id = ?`, anchor0, anchor0, id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Edit concurrency only, but ALSO send a (different) quota_week_start — the
	// edit endpoint must ignore it.
	anchor1 := time.Now().UTC().Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"max_concurrency": 5, "quota_week_start": anchor1})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+itoa(id), bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateUser expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}

	q, err := models.GetQuota(h.DB, id)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.WeekStart != anchor0 {
		t.Fatalf("edit must NOT change week_start; got %q, want %q", q.WeekStart, anchor0)
	}
	if q.QuotaTokenWeekUsed != 7 {
		t.Fatalf("edit must NOT reset weekly used; got %d, want 7", q.QuotaTokenWeekUsed)
	}
}

// TestAdminUpdateUser_IgnoresWeekStartInvalid verifies a malformed anchor sent to
// the general edit endpoint is simply ignored (200, no change) — it is no longer
// the edit endpoint's concern to validate it.
func TestAdminUpdateUser_IgnoresWeekStartInvalid(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weekbad", nil)
	anchor0 := time.Now().UTC().Add(-3 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := h.DB.Exec(`UPDATE quotas SET week_start = ?, week_cycle_start = ? WHERE user_id = ?`, anchor0, anchor0, id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"quota_week_start": "nope"})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+itoa(id), bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateUser with invalid week_start expected 200 (ignored), got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, _ := models.GetQuota(h.DB, id)
	if q.WeekStart != anchor0 {
		t.Fatalf("week_start should be unchanged, got %q want %q", q.WeekStart, anchor0)
	}
}

// TestAdminSetUserWeekStart verifies the dedicated single-user endpoint sets the
// anchor, zeroes the in-progress weekly Token usage, and writes an audit entry.
func TestAdminSetUserWeekStart(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weeksingle", nil)
	if _, err := h.DB.Exec(`UPDATE quotas SET quota_token_week_used = 7 WHERE user_id = ?`, id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	anchor := time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"week_start": anchor})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+itoa(id)+"/week-start", bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.SetUserWeekStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SetUserWeekStart expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, err := models.GetQuota(h.DB, id)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.WeekStart != anchor {
		t.Fatalf("WeekStart = %q, want %q", q.WeekStart, anchor)
	}
	if q.QuotaTokenWeekUsed != 0 {
		t.Fatalf("expected weekly used RESET to 0 on anchor change, got %d", q.QuotaTokenWeekUsed)
	}
	var cnt int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action='quota_week_start_set' AND target_id=?`, itoa(id)).Scan(&cnt); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected 1 quota_week_start_set audit row, got %d", cnt)
	}
}

// TestAdminSetUserWeekStart_IdempotentSameAnchor verifies re-submitting the SAME
// anchor is a no-op: usage is NOT zeroed and no new audit row is written.
func TestAdminSetUserWeekStart_IdempotentSameAnchor(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weekidemp", nil)
	anchor := time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	if _, err := h.DB.Exec(`UPDATE quotas SET quota_token_week_used = 7, week_start = ?, week_cycle_start = ? WHERE user_id = ?`, anchor, anchor, id); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body, _ := json.Marshal(map[string]any{"week_start": anchor})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/"+itoa(id)+"/week-start", bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.SetUserWeekStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SetUserWeekStart expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	q, _ := models.GetQuota(h.DB, id)
	if q.QuotaTokenWeekUsed != 7 {
		t.Fatalf("re-submitting same anchor must NOT zero usage, got %d want 7", q.QuotaTokenWeekUsed)
	}
	var cnt int
	if err := h.DB.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action='quota_week_start_set' AND target_id=?`, itoa(id)).Scan(&cnt); err != nil {
		t.Fatalf("audit query: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("same-anchor submit must NOT write audit, got %d", cnt)
	}
}

// TestAdminBatchSetWeekStart verifies the batch endpoint applies a fixed anchor
// to every listed user and reports success for all.
func TestAdminBatchSetWeekStart(t *testing.T) {
	h := newAdminTestHandler(t)
	id1, _ := adminCreateUser(t, h, "wbatch1", nil)
	id2, _ := adminCreateUser(t, h, "wbatch2", nil)

	anchor := time.Now().UTC().Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	payload, _ := json.Marshal(map[string]any{
		"user_ids":   []int64{id1, id2},
		"week_start": anchor,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/batch-week-start", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.BatchSetWeekStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("BatchSetWeekStart expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Succeeded int              `json:"succeeded"`
		Failed    int              `json:"failed"`
		Results   []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if resp.Succeeded != 2 || resp.Failed != 0 {
		t.Fatalf("expected succeeded=2 failed=0, got succeeded=%d failed=%d results=%v", resp.Succeeded, resp.Failed, resp.Results)
	}
	for _, id := range []int64{id1, id2} {
		q, err := models.GetQuota(h.DB, id)
		if err != nil {
			t.Fatalf("GetQuota %d: %v", id, err)
		}
		if q.WeekStart != anchor {
			t.Fatalf("user %d WeekStart = %q, want %q", id, q.WeekStart, anchor)
		}
	}
}

// TestAdminBatchSetWeekStart_EmptyRejected verifies a batch with no user_ids is
// rejected up front.
func TestAdminBatchSetWeekStart_EmptyRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	payload, _ := json.Marshal(map[string]any{"user_ids": []int64{}, "week_start": time.Now().UTC().Format(time.RFC3339)})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/batch-week-start", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.BatchSetWeekStart(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("BatchSetWeekStart empty expected 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// itoa is a tiny helper to format a user id into a path segment.
func itoa(id int64) string {
	return strconv.FormatInt(id, 10)
}
