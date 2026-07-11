// Package quota_test contains tests for the quota package's multiplier engine,
// with a focus on the Asia/Shanghai time-zone normalization fix.
package quota_test

import (
	"os"
	"testing"
	"time"

	"llm_api_gateway/internal/db"
	"llm_api_gateway/internal/quota"
)

// newMultiplierTestDB opens an isolated temp-file SQLite DB and runs migrations.
func newMultiplierTestDB(t *testing.T) *db.DB {
	t.Helper()

	f, err := os.CreateTemp(t.TempDir(), "mult_tz_test_*.db")
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
	t.Cleanup(func() { _ = database.Close() })
	return database
}

// TestMultiplierEngine_TimeZoneShanghai verifies that the effective multiplier
// is evaluated in Asia/Shanghai regardless of the host's local time zone.
//
// We create a rule matching only the first hour of the (Shanghai) day
// (00:00-01:00, multiplier 2.0). We then call GetEffectiveMultiplier with a
// timestamp that is 16:30 UTC — which is 00:30 Asia/Shanghai. The function must
// normalize to Asia/Shanghai and select the 2.0 multiplier. The pre-fix code
// (comparing against bare local time) would have returned 1.0 on a UTC host.
func TestMultiplierEngine_TimeZoneShanghai(t *testing.T) {
	database := newMultiplierTestDB(t)
	eng := quota.NewMultiplierEngine(database.Conn)

	if _, err := eng.Create("00:00", "01:00", 2.0, "*"); err != nil {
		t.Fatalf("failed to create midnight multiplier rule: %v", err)
	}

	// 16:30 UTC on 2026-01-01 == 00:30 Asia/Shanghai -> in [00:00, 01:00).
	now := time.Date(2026, 1, 1, 16, 30, 0, 0, time.UTC)
	got := eng.GetEffectiveMultiplier(now)
	if got != 2.0 {
		t.Fatalf("expected multiplier 2.0 (Asia/Shanghai 00:30), got %v (tz normalization broken?)", got)
	}
}

// TestMultiplierEngine_TimeZoneShanghai_OutsideWindow is a control case: a time
// that is 03:30 Asia/Shanghai must NOT match the 00:00-01:00 rule (=> 1.0).
func TestMultiplierEngine_TimeZoneShanghai_OutsideWindow(t *testing.T) {
	database := newMultiplierTestDB(t)
	eng := quota.NewMultiplierEngine(database.Conn)

	if _, err := eng.Create("00:00", "01:00", 2.0, "*"); err != nil {
		t.Fatalf("failed to create midnight multiplier rule: %v", err)
	}

	// 19:30 UTC on 2026-01-01 == 03:30 Asia/Shanghai -> outside [00:00, 01:00).
	now := time.Date(2026, 1, 1, 19, 30, 0, 0, time.UTC)
	got := eng.GetEffectiveMultiplier(now)
	if got != 1.0 {
		t.Fatalf("expected multiplier 1.0 (Asia/Shanghai 03:30 outside window), got %v", got)
	}
}
