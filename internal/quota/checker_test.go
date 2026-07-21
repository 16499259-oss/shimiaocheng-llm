// Package quota_test contains tests for the quota package's checker and multiplier engine.
package quota_test

import (
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/models"
	"llm_api_gateway/internal/quota"
)

// newQuotaTestDB opens an isolated temp-file SQLite database (zero CGO, modernc driver),
// runs migrations, and registers cleanup so the file is removed automatically.
func newQuotaTestDB(t *testing.T) *db.DB {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "quota_test_*.db")
	if err != nil {
		t.Fatalf("failed to create temp db file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("failed to close temp db file: %v", err)
	}

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	if err := db.RunMigrations(database); err != nil {
		t.Fatalf("failed to run migrations: %v", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})
	return database
}

// TestCheckAndDeduct_Success verifies that a user with sufficient quota is allowed
// and that exactly `effectiveCalls` are deducted from both 5h and total counters.
func TestCheckAndDeduct_Success(t *testing.T) {
	database := newQuotaTestDB(t)

	created, err := models.CreateUser(
		database.Conn,
		"alice", "pw-hash", "subkey-hash-alice", "sk-alice...",
		"user", "active", "", "auto", "",
		100,  // quota_5h_limit
		1000, // quota_total_limit
		nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	userID := created.ID

	eng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, eng, 5)

	allowed, err := checker.CheckAndDeduct(userID, 1)
	if err != nil {
		t.Fatalf("CheckAndDeduct returned error: %v", err)
	}
	if !allowed {
		t.Fatalf("expected allowed=true for user with sufficient quota")
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota failed: %v", err)
	}
	if q == nil {
		t.Fatalf("expected quota record to exist")
	}
	if q.Quota5hUsed != 1 {
		t.Fatalf("expected quota_5h_used=1, got %d", q.Quota5hUsed)
	}
	if q.QuotaTotalUsed != 1 {
		t.Fatalf("expected quota_total_used=1, got %d", q.QuotaTotalUsed)
	}
}

// TestCheckAndDeduct_Exceeded verifies that a user whose total quota is exhausted is
// rejected (allowed=false) and that nothing is deducted.
func TestCheckAndDeduct_Exceeded(t *testing.T) {
	database := newQuotaTestDB(t)

	created, err := models.CreateUser(
		database.Conn,
		"bob", "pw-hash", "subkey-hash-bob", "sk-bob...",
		"user", "active", "", "auto", "",
		100, // quota_5h_limit
		5,   // quota_total_limit (positive; exhausted below by seeding used=limit)
		nil, 0, models.DefaultMaxConcurrency,
	)
	if err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	userID := created.ID

	// Exhaust the total quota (used == limit) so the next check is blocked.
	if _, err := database.Conn.Exec(`UPDATE quotas SET quota_total_used = 5 WHERE user_id = ?`, userID); err != nil {
		t.Fatalf("seed total used: %v", err)
	}

	eng := quota.NewMultiplierEngine(database.Conn)
	checker := quota.NewChecker(database.Conn, eng, 5)

	allowed, err := checker.CheckAndDeduct(userID, 1)
	if err != nil {
		t.Fatalf("CheckAndDeduct returned error: %v", err)
	}
	if allowed {
		t.Fatalf("expected allowed=false for user with no remaining quota")
	}

	q, err := models.GetQuota(database.Conn, userID)
	if err != nil {
		t.Fatalf("GetQuota failed: %v", err)
	}
	if q == nil {
		t.Fatalf("expected quota record to exist")
	}
	if q.Quota5hUsed != 0 {
		t.Fatalf("expected quota_5h_used to stay 0, got %d", q.Quota5hUsed)
	}
	if q.QuotaTotalUsed != 5 {
		t.Fatalf("expected quota_total_used to stay at the seeded 5 (blocked deduction must not deduct), got %d", q.QuotaTotalUsed)
	}
}

// TestMultiplierEngine_GetEffectiveMultiplier verifies the engine returns a positive,
// sane multiplier. A full-day rule with multiplier 2.0 should be selected.
func TestMultiplierEngine_GetEffectiveMultiplier(t *testing.T) {
	database := newQuotaTestDB(t)

	eng := quota.NewMultiplierEngine(database.Conn)
	if _, err := eng.Create("00:00", "23:59", 2.0, "*"); err != nil {
		t.Fatalf("failed to create multiplier rule: %v", err)
	}

	got := eng.GetEffectiveMultiplier(time.Now())
	if got <= 0 {
		t.Fatalf("expected positive effective multiplier, got %v", got)
	}
	if got != 2.0 {
		t.Fatalf("expected effective multiplier 2.0 (full-day rule), got %v", got)
	}
}
