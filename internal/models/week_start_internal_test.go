package models

import (
	"testing"
	"time"
)

// TestAlignedCycleStartUTC exercises the fixed-phase 7-day cycle math that the
// gate uses to decide when the weekly Token bucket resets. It is a pure
// function (white-box) so we assert its exact behaviour across the tricky edges.
func TestAlignedCycleStartUTC(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // fixed phase anchor: 2026-01-01 00:00 UTC

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{"now equals anchor", base, base},
		{"same-day (k=0)", base.Add(3 * 24 * time.Hour), base}, // +3d → still cycle [anchor, anchor+7d)
		{"one cycle later (k=1)", base.Add(8 * 24 * time.Hour), base.Add(7 * 24 * time.Hour)},
		{"cycle boundary (exactly +7d)", base.Add(7 * 24 * time.Hour), base.Add(7 * 24 * time.Hour)},
		{"two cycles later (k=2)", base.Add(15 * 24 * time.Hour), base.Add(14 * 24 * time.Hour)},
		{"three cycles later (k=3)", base.Add(22 * 24 * time.Hour), base.Add(21 * 24 * time.Hour)},
		{"future anchor (now before anchor)", base.Add(-48 * time.Hour), base}, // cycle starts at the anchor itself
		{"many cycles (k=10)", base.Add(73 * 24 * time.Hour), base.Add(70 * 24 * time.Hour)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AlignedCycleStartUTC(base, c.now)
			if !got.Equal(c.want) {
				t.Fatalf("AlignedCycleStartUTC(%v, %v) = %v, want %v", base, c.now, got, c.want)
			}
		})
	}
}
