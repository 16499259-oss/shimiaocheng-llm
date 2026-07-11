// Package timeutil provides time-zone-safe helpers for window/routing decisions.
//
// All time-window and routing comparisons in the gateway MUST go through this
// package so that the canonical time zone (Asia/Shanghai, UTC+8) is applied
// consistently. Never compare windows against a bare time.Now() in local time;
// always use timeutil.IsInRange / timeutil.MatchDay after .In(timeutil.ShanghaiTZ).
package timeutil

import (
	"strconv"
	"strings"
	"time"

	// Embed the IANA time-zone database so Asia/Shanghai resolves correctly
	// even on stripped/minimal systems without /usr/share/zoneinfo.
	_ "time/tzdata"
)

// ShanghaiTZ is the canonical time zone for every window/routing decision in the
// gateway (the business operates on UTC+8). If the IANA database is somehow
// unavailable, we fall back to a fixed +08:00 zone, which matches Asia/Shanghai
// (no DST).
var ShanghaiTZ *time.Location

func init() {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Fallback: a fixed UTC+8 zone (no DST), identical offset to Asia/Shanghai.
		loc = time.FixedZone("Asia/Shanghai", 8*3600)
	}
	ShanghaiTZ = loc
}

// IsInRange reports whether now falls within the half-open interval
// [start, end) expressed as "HH:MM" strings, evaluated in the Asia/Shanghai
// time zone.
//
// Both endpoints are compared lexicographically as the time-of-day of now. This
// correctly handles overnight ranges where start > end (e.g. "22:00"-"06:00").
// For a normal range "14:00"-"18:00", 14:00 is included and 18:00 is excluded.
func IsInRange(start, end string, now time.Time) bool {
	cur := now.In(ShanghaiTZ).Format("15:04")
	if start <= end {
		// Normal range: e.g. 14:00-18:00
		return cur >= start && cur < end
	}
	// Overnight range: e.g. 22:00-06:00
	return cur >= start || cur < end
}

// MatchDay reports whether the weekday of now matches the given weekMask,
// evaluated in the Asia/Shanghai time zone.
//
// weekMask format:
//   - "*" or ""         -> every day
//   - "1,2,3,4,5"       -> weekdays
//   - "0,6"             -> weekends
//
// Weekday numbers follow time.Time.Weekday(): 0=Sun, 1=Mon, ..., 6=Sat.
func MatchDay(weekMask string, now time.Time) bool {
	if weekMask == "*" || weekMask == "" {
		return true
	}
	wd := strconv.Itoa(int(now.In(ShanghaiTZ).Weekday()))
	for _, d := range strings.Split(weekMask, ",") {
		if strings.TrimSpace(d) == wd {
			return true
		}
	}
	return false
}
