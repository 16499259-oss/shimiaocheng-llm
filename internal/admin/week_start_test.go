// Package admin — tests for the weekly quota start-anchor feature (T5/T6):
// single-user PUT /api/users/{id} with quota_week_start, and the batch
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

// TestAdminUpdateUser_WeekStartSetsAnchor verifies the single-user edit endpoint
// writes the fixed phase anchor, zeroes the current weekly Token usage, and
// returns the new anchor in the response.
func TestAdminUpdateUser_WeekStartSetsAnchor(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weekuser", nil)

	anchor := time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{"quota_week_start": anchor})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+itoa(id), bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateUser week_start expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}
	if resp["quota_week_start"] != anchor {
		t.Fatalf("response quota_week_start = %v, want %v", resp["quota_week_start"], anchor)
	}

	q, err := models.GetQuota(h.DB, id)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.WeekStart != anchor {
		t.Fatalf("stored WeekStart = %q, want %q", q.WeekStart, anchor)
	}
	if q.QuotaTokenWeekUsed != 0 {
		t.Fatalf("expected weekly used reset to 0, got %d", q.QuotaTokenWeekUsed)
	}
}

// TestAdminUpdateUser_WeekStartInvalidRejected verifies a malformed RFC3339
// anchor is rejected with 400 (no partial write).
func TestAdminUpdateUser_WeekStartInvalidRejected(t *testing.T) {
	h := newAdminTestHandler(t)
	id, _ := adminCreateUser(t, h, "weekbad", nil)

	body, _ := json.Marshal(map[string]any{"quota_week_start": "nope"})
	req := httptest.NewRequest(http.MethodPut, "/admin/api/users/"+itoa(id), bytes.NewReader(body))
	req.SetPathValue("id", itoa(id))
	rec := httptest.NewRecorder()
	h.UpdateUser(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("UpdateUser invalid week_start expected 400, got %d; body=%s", rec.Code, rec.Body.String())
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
		"user_ids":  []int64{id1, id2},
		"week_start": anchor,
	})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/users/batch-week-start", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.BatchSetWeekStart(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("BatchSetWeekStart expected 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Succeeded int                  `json:"succeeded"`
		Failed    int                  `json:"failed"`
		Results   []map[string]any     `json:"results"`
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
