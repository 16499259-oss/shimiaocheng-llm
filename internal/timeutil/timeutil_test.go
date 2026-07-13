package timeutil

import (
	"testing"
	"time"
)

// shanghai constructs a time in the canonical Asia/Shanghai location so that
// IsInRange/MatchDay evaluate against the intended wall-clock values.
func shanghai(y int, mo time.Month, d, h, mi int) time.Time {
	return time.Date(y, mo, d, h, mi, 0, 0, ShanghaiTZ)
}

func TestShanghaiTZ_ResolvesToUTC8(t *testing.T) {
	if ShanghaiTZ == nil {
		t.Fatal("ShanghaiTZ is nil")
	}
	// A known UTC instant must land on +08:00.
	utc := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	loc := utc.In(ShanghaiTZ)
	if loc.Hour() != 8 {
		t.Errorf("expected UTC 00:00 -> Shanghai 08:00, got %02d:00", loc.Hour())
	}
	if _, offset := loc.Zone(); offset != 8*3600 {
		t.Errorf("expected +08:00 offset, got %+ds", offset)
	}
}

func TestIsInRange_NormalWindow(t *testing.T) {
	// Range 14:00-18:00 (half-open: 14:00 inclusive, 18:00 exclusive).
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"13:59 before start", shanghai(2026, 7, 13, 13, 59), false},
		{"14:00 at start (inclusive)", shanghai(2026, 7, 13, 14, 0), true},
		{"15:30 inside", shanghai(2026, 7, 13, 15, 30), true},
		{"17:59 inside", shanghai(2026, 7, 13, 17, 59), true},
		{"18:00 at end (exclusive)", shanghai(2026, 7, 13, 18, 0), false},
		{"18:01 after end", shanghai(2026, 7, 13, 18, 1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsInRange("14:00", "18:00", c.now); got != c.want {
				t.Errorf("IsInRange(14:00,18:00,%s) = %v, want %v", c.now.Format("15:04"), got, c.want)
			}
		})
	}
}

func TestIsInRange_OvernightWindow(t *testing.T) {
	// Range 22:00-06:00 spans midnight.
	cases := []struct {
		name string
		now  time.Time
		want bool
	}{
		{"21:59 before start", shanghai(2026, 7, 13, 21, 59), false},
		{"22:00 at start (inclusive)", shanghai(2026, 7, 13, 22, 0), true},
		{"23:30 inside", shanghai(2026, 7, 13, 23, 30), true},
		{"00:30 inside (next day)", shanghai(2026, 7, 14, 0, 30), true},
		{"05:59 inside", shanghai(2026, 7, 14, 5, 59), true},
		{"06:00 at end (exclusive)", shanghai(2026, 7, 14, 6, 0), false},
		{"12:00 outside", shanghai(2026, 7, 14, 12, 0), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsInRange("22:00", "06:00", c.now); got != c.want {
				t.Errorf("IsInRange(22:00,06:00,%s) = %v, want %v", c.now.Format("15:04"), got, c.want)
			}
		})
	}
}

func TestIsInRange_DegenerateEqualEndpoints(t *testing.T) {
	// start == end yields a range that never matches (cur < end is always false).
	if IsInRange("12:00", "12:00", shanghai(2026, 7, 13, 12, 0)) {
		t.Error("expected empty range (start==end) to never match, got true")
	}
}

func TestMatchDay_AllDays(t *testing.T) {
	// 2026-07-13 is Monday; weekday numbers follow time.Time.Weekday().
	mon := shanghai(2026, 7, 13, 12, 0) // Monday (1)
	sat := shanghai(2026, 7, 18, 12, 0) // Saturday (6)
	sun := shanghai(2026, 7, 19, 12, 0) // Sunday (0)

	if !MatchDay("*", mon) || !MatchDay("", mon) {
		t.Error("'*' and '' must match every day")
	}
	// Weekdays mask 1-5.
	if !MatchDay("1,2,3,4,5", mon) {
		t.Error("Monday should match weekday mask 1-5")
	}
	if MatchDay("1,2,3,4,5", sat) {
		t.Error("Saturday should NOT match weekday mask 1-5")
	}
	// Weekend mask 0,6.
	if !MatchDay("0,6", sun) || !MatchDay("0,6", sat) {
		t.Error("weekend mask 0,6 must match Sat/Sun")
	}
	if MatchDay("0,6", mon) {
		t.Error("Monday should NOT match weekend mask 0,6")
	}
	// Unknown day in mask should not match.
	if MatchDay("2,3", sun) {
		t.Error("Sunday should not match mask 2,3")
	}
}
