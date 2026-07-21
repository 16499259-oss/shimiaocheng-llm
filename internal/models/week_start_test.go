package models_test

import (
	"testing"
	"time"

	"llm_api_gateway/internal/models"
)

// TestSetQuotaWeekStart verifies the admin setter writes the fixed phase anchor,
// zeroes the current weekly Token usage, and aligns week_cycle_start to the
// cycle containing now (so the very next request is in the right cycle).
func TestSetQuotaWeekStart(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	// Anchor 3 days in the past → now is inside the first cycle [anchor, anchor+7d).
	anchor := time.Now().UTC().Add(-3 * 24 * time.Hour)
	anchorStr := anchor.Format(time.RFC3339)

	if err := models.SetQuotaWeekStart(database.Conn, userID, anchorStr); err != nil {
		t.Fatalf("SetQuotaWeekStart: %v", err)
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.WeekStart != anchorStr {
		t.Fatalf("WeekStart = %q, want %q", q.WeekStart, anchorStr)
	}
	if q.QuotaTokenWeekUsed != 0 {
		t.Fatalf("expected weekly used zeroed, got %d", q.QuotaTokenWeekUsed)
	}
	var cycle string
	if err := database.Conn.QueryRow(`SELECT week_cycle_start FROM quotas WHERE user_id = ?`, userID).Scan(&cycle); err != nil {
		t.Fatalf("read week_cycle_start: %v", err)
	}
	if cycle != anchorStr {
		t.Fatalf("week_cycle_start = %q, want %q (anchor, since now is in the first cycle)", cycle, anchorStr)
	}
}

// TestSetQuotaWeekStart_InvalidRejected ensures a malformed RFC3339 string is
// rejected rather than written.
func TestSetQuotaWeekStart_InvalidRejected(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)
	if err := models.SetQuotaWeekStart(database.Conn, userID, "not-a-time"); err == nil {
		t.Fatalf("expected error for invalid week_start")
	}
}

// TestWeeklyBucketMultiCycleSkip verifies a single gate pass can skip MULTIPLE
// expired cycles at once (no per-cycle drift): with a 21-day-old anchor and the
// stored cycle start still at the anchor, the gate must jump week_cycle_start
// straight to anchor+21d (3 full cycles) and reset the usage to 0.
func TestWeeklyBucketMultiCycleSkip(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	anchor := time.Now().UTC().Add(-21 * 24 * time.Hour)
	anchorStr := anchor.Format(time.RFC3339)
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_week_limit = 10, quota_token_week_used = 9, week_start = ?, week_cycle_start = ? WHERE user_id = ?`,
		anchorStr, anchorStr, userID,
	); err != nil {
		t.Fatalf("seed multi-cycle window: %v", err)
	}

	if ok, err := models.AtomicDeductQuota(database.Conn, userID, 1); err != nil || !ok {
		t.Fatalf("deduct: ok=%v err=%v", ok, err)
	}

	var cycle string
	var used int
	if err := database.Conn.QueryRow(`SELECT week_cycle_start, quota_token_week_used FROM quotas WHERE user_id = ?`, userID).Scan(&cycle, &used); err != nil {
		t.Fatalf("read: %v", err)
	}
	expectedCycle := anchor.Add(21 * 24 * time.Hour).Format(time.RFC3339)
	if cycle != expectedCycle {
		t.Fatalf("week_cycle_start = %q, want %q (3 cycles skipped)", cycle, expectedCycle)
	}
	if used != 0 {
		t.Fatalf("expected usage reset to 0 across multi-cycle skip, got %d", used)
	}
}

// TestWeeklyBucketFutureAnchor verifies a future phase anchor does NOT reset the
// current cycle: until now crosses the anchor, the cycle start equals the anchor
// itself, so in-progress usage keeps accumulating (no spurious reset).
func TestWeeklyBucketFutureAnchor(t *testing.T) {
	database := newModelsTestDB(t)
	userID := seedTokenWindowUser(t, database)

	anchor := time.Now().UTC().Add(2 * 24 * time.Hour) // 2 days in the future
	anchorStr := anchor.Format(time.RFC3339)
	if _, err := database.Conn.Exec(
		`UPDATE quotas SET quota_token_week_limit = 10, quota_token_week_used = 5, week_start = ?, week_cycle_start = ? WHERE user_id = ?`,
		anchorStr, anchorStr, userID,
	); err != nil {
		t.Fatalf("seed future-anchor window: %v", err)
	}

	if ok, err := models.AtomicDeductQuota(database.Conn, userID, 1); err != nil || !ok {
		t.Fatalf("deduct: ok=%v err=%v", ok, err)
	}

	var cycle string
	var used int
	if err := database.Conn.QueryRow(`SELECT week_cycle_start, quota_token_week_used FROM quotas WHERE user_id = ?`, userID).Scan(&cycle, &used); err != nil {
		t.Fatalf("read: %v", err)
	}
	if cycle != anchorStr {
		t.Fatalf("week_cycle_start = %q, want %q (future anchor unchanged)", cycle, anchorStr)
	}
	if used != 5 {
		t.Fatalf("expected usage unchanged (5) before anchor reached, got %d", used)
	}
}

// TestBatchSetWeekStartIsPerUser uses the model setter in a loop to confirm the
// per-user semantics: a bad id in the middle does not prevent neighbours from
// being set (mirrors the admin BatchSetWeekStart handler contract).
func TestBatchSetWeekStartIsPerUser(t *testing.T) {
	database := newModelsTestDB(t)
	goodID := seedTokenWindowUser(t, database)
	anchor := time.Now().UTC().Add(-1 * 24 * time.Hour).Format(time.RFC3339)
	// A non-existent id is tolerated (no FK enforced by this UPDATE on quotas).
	if err := models.SetQuotaWeekStart(database.Conn, 999999, anchor); err != nil {
		t.Fatalf("SetQuotaWeekStart on missing id should not error: %v", err)
	}
	if err := models.SetQuotaWeekStart(database.Conn, goodID, anchor); err != nil {
		t.Fatalf("SetQuotaWeekStart good id: %v", err)
	}
	q, err := models.GetQuota(database.Conn, goodID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.WeekStart != anchor {
		t.Fatalf("WeekStart = %q, want %q", q.WeekStart, anchor)
	}
}
