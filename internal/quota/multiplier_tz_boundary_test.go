// Package quota_test contains additional boundary tests for the multiplier
// engine, focusing on the Asia/Shanghai time-zone normalization when the host
// machine's local time zone is NOT Asia/Shanghai, plus overnight windows.
package quota_test

import (
	"testing"
	"time"

	"llm_api_gateway/internal/quota"
	"llm_api_gateway/internal/timeutil"
)

// TestMultiplierEngine_TimeZoneNonShanghaiLocal simulates a production host
// whose local time zone is NOT Asia/Shanghai (here we force time.Local = UTC)
// and verifies the effective multiplier is still evaluated in Asia/Shanghai.
//
// Rule: 09:00-10:00 Asia/Shanghai, multiplier 3.0.
//   - 01:30 UTC == 09:30 Asia/Shanghai  -> inside  -> expect 3.0
//   - 02:30 UTC == 10:30 Asia/Shanghai  -> outside -> expect 1.0
//
// Pre-fix code comparing against bare local time would return 1.0 for the
// first case on a UTC host.
func TestMultiplierEngine_TimeZoneNonShanghaiLocal(t *testing.T) {
	database := newMultiplierTestDB(t)
	eng := quota.NewMultiplierEngine(database.Conn)

	if _, err := eng.Create("09:00", "10:00", 3.0, "*"); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	// Force the machine's local time zone to a non-Shanghai zone (UTC).
	orig := time.Local
	time.Local = time.UTC
	defer func() { time.Local = orig }()

	inWindow := time.Date(2026, 1, 1, 1, 30, 0, 0, time.Local) // 01:30 UTC == 09:30 Shanghai
	if got := eng.GetEffectiveMultiplier(inWindow); got != 3.0 {
		t.Fatalf("expected multiplier 3.0 (Asia/Shanghai 09:30), got %v (tz normalization broken)", got)
	}

	outWindow := time.Date(2026, 1, 1, 2, 30, 0, 0, time.Local) // 02:30 UTC == 10:30 Shanghai
	if got := eng.GetEffectiveMultiplier(outWindow); got != 1.0 {
		t.Fatalf("expected multiplier 1.0 (Asia/Shanghai 10:30 outside window), got %v", got)
	}
}

// TestMultiplierEngine_OvernightRule validates an overnight multiplier window
// that wraps past midnight (23:00-01:00). 00:30 is inside; 01:00 is the
// excluded upper bound; 23:30 is inside.
func TestMultiplierEngine_OvernightRule(t *testing.T) {
	database := newMultiplierTestDB(t)
	eng := quota.NewMultiplierEngine(database.Conn)

	if _, err := eng.Create("23:00", "01:00", 2.0, "*"); err != nil {
		t.Fatalf("create rule: %v", err)
	}

	inEarly := time.Date(2026, 1, 1, 23, 30, 0, 0, timeutil.ShanghaiTZ) // inside
	if got := eng.GetEffectiveMultiplier(inEarly); got != 2.0 {
		t.Fatalf("expected 2.0 at 23:30 Shanghai, got %v", got)
	}

	inLate := time.Date(2026, 1, 2, 0, 30, 0, 0, timeutil.ShanghaiTZ) // inside (past midnight)
	if got := eng.GetEffectiveMultiplier(inLate); got != 2.0 {
		t.Fatalf("expected 2.0 at 00:30 Shanghai (overnight), got %v", got)
	}

	endExclusive := time.Date(2026, 1, 2, 1, 0, 0, 0, timeutil.ShanghaiTZ) // excluded upper bound
	if got := eng.GetEffectiveMultiplier(endExclusive); got != 1.0 {
		t.Fatalf("expected 1.0 at 01:00 Shanghai (excluded), got %v", got)
	}
}
