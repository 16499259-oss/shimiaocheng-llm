package models

import (
	"testing"
	"time"
)

func TestNormalizeExpiry_Empty(t *testing.T) {
	got, err := NormalizeExpiry("")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestNormalizeExpiry_RFC3339CanonicalShanghai(t *testing.T) {
	in := "2026-08-15T10:00:00Z"
	got, err := NormalizeExpiry(in)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	orig, _ := time.Parse(time.RFC3339, in)
	parsed, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("result not RFC3339: %q (%v)", got, err)
	}
	// The instant must be preserved (UTC 10:00 -> Shanghai 18:00, same moment).
	if !parsed.Equal(orig) {
		t.Fatalf("instant not preserved: %v vs %v", parsed, orig)
	}
	if got[len(got)-6:] != "+08:00" {
		t.Fatalf("expected +08:00 offset, got %q", got)
	}
}

func TestNormalizeExpiry_BareDate(t *testing.T) {
	got, err := NormalizeExpiry("2026-08-15")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := "2026-08-15T23:59:59+08:00"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestNormalizeExpiry_Invalid(t *testing.T) {
	for _, bad := range []string{"2026/08/15", "not-a-date", "15-08-2026"} {
		if _, err := NormalizeExpiry(bad); err == nil {
			t.Fatalf("NormalizeExpiry(%q): expected error, got nil", bad)
		}
	}
}

func TestParseExpiry_EmptyAndMalformed(t *testing.T) {
	if _, ok := ParseExpiry(""); ok {
		t.Fatal("empty should be ok=false")
	}
	if _, ok := ParseExpiry("2026/08/15"); ok {
		t.Fatal("malformed should be ok=false (fail-closed)")
	}
}

func TestParseExpiry_RFC3339AndBareDate(t *testing.T) {
	if _, ok := ParseExpiry("2026-08-15T23:59:59+08:00"); !ok {
		t.Fatal("RFC3339 should parse")
	}
	got, ok := ParseExpiry("2026-08-15")
	if !ok {
		t.Fatal("bare date should parse")
	}
	if got.Hour() != 23 || got.Minute() != 59 {
		t.Fatalf("bare date should resolve to end of day, got %v", got)
	}
}
