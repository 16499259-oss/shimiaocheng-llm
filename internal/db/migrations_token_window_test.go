package db

import (
	"testing"

	"llm_api_gateway/internal/models"
)

// TestRunMigrations_TokenWindowColumnsAddedAndIdempotent verifies the five new
// Token-window columns (quota_token_5h_limit / _used, quota_token_week_limit /
// _used, week_start) are added exactly once and survive a second migration run,
// and that a legacy user present BEFORE the migration exposes the new columns
// with their documented defaults (0 / 0 / 0 / 0) and a backfilled week_start.
func TestRunMigrations_TokenWindowColumnsAddedAndIdempotent(t *testing.T) {
	database := openBaseSchemaDB(t)

	// Seed a legacy user+quota BEFORE migrating so we can verify the migration
	// backfills week_start for pre-existing rows and that the new columns
	// coexist with already-populated data.
	seedID := insertBaseUser(t, database, "tw1", "hash-tw1", "sk-tw1...")

	// First run adds the columns + backfills week_start for the legacy row.
	if err := RunMigrations(database); err != nil {
		t.Fatalf("first RunMigrations: %v", err)
	}
	newCols := []string{
		"quota_token_5h_limit",
		"quota_token_5h_used",
		"quota_token_week_limit",
		"quota_token_week_used",
		"week_start",
	}
	for _, col := range newCols {
		if !columnExists(database, "quotas", col) {
			t.Fatalf("column quotas.%s missing after first migration", col)
		}
	}

	// Second run must be a no-op (idempotent) and must not error.
	if err := RunMigrations(database); err != nil {
		t.Fatalf("second RunMigrations: %v", err)
	}
	for _, col := range newCols {
		if !columnExists(database, "quotas", col) {
			t.Fatalf("column quotas.%s missing after second migration", col)
		}
	}

	// The legacy row must expose the new columns with their documented defaults
	// (limits 0 = unlimited, used 0) and a backfilled week_start (the migration
	// sets week_start = datetime('now') for rows that lacked it).
	q, err := models.GetQuota(database.Conn, seedID)
	if err != nil {
		t.Fatalf("GetQuota: %v", err)
	}
	if q.QuotaToken5hLimit != 0 || q.QuotaToken5hUsed != 0 {
		t.Fatalf("expected 5h-window Token columns defaulted to 0, got limit=%d used=%d",
			q.QuotaToken5hLimit, q.QuotaToken5hUsed)
	}
	if q.QuotaTokenWeekLimit != 0 || q.QuotaTokenWeekUsed != 0 {
		t.Fatalf("expected weekly Token columns defaulted to 0, got limit=%d used=%d",
			q.QuotaTokenWeekLimit, q.QuotaTokenWeekUsed)
	}
	if q.WeekStart == "" {
		t.Fatalf("expected week_start to be backfilled by the migration for the legacy row")
	}
}
