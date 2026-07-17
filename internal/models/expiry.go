package models

import (
	"fmt"
	"strings"
	"time"

	"llm_api_gateway/internal/timeutil"
)

// NormalizeExpiry normalizes an API-supplied expiry value to a canonical
// RFC3339 string in Asia/Shanghai. It accepts:
//   - ""                       -> ("", nil): no expiry
//   - RFC3339 (with/without tz) -> canonical Shanghai RFC3339
//   - bare date "2006-01-02"   -> that day at 23:59:59 +08:00
//
// Any other format returns an error so callers reject the input at the API
// boundary (fail-closed) instead of persisting an unparseable value that would
// otherwise be silently treated as "permanent" by the auth middleware.
func NormalizeExpiry(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(timeutil.ShanghaiTZ).Format(time.RFC3339), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, timeutil.ShanghaiTZ)
		return end.Format(time.RFC3339), nil
	}
	return "", fmt.Errorf("invalid expiry format: %q (expect RFC3339 or YYYY-MM-DD)", s)
}

// ParseExpiry parses a stored expiry string into a time in Asia/Shanghai.
// ok is false only when the value is non-empty but unparseable — callers must
// treat that as "already expired" (fail-closed). An empty string also returns
// ok=false, which callers interpret as "no expiry" via an outer guard.
func ParseExpiry(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(timeutil.ShanghaiTZ), true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, timeutil.ShanghaiTZ), true
	}
	return time.Time{}, false
}
