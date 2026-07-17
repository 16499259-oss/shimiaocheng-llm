package proxy

import (
	"testing"
)

func TestTryAcquireConcurrencyUnlimited(t *testing.T) {
	// A cap of 0 means unlimited: every attempt is allowed and the counter is
	// never driven negative.
	if !tryAcquireConcurrency(77001, 0) {
		t.Fatal("unlimited (cap=0) should always allow")
	}
	releaseConcurrency(77001)
	if !tryAcquireConcurrency(77001, 0) {
		t.Fatal("unlimited (cap=0) should still allow after release")
	}
	releaseConcurrency(77001)
}

func TestTryAcquireConcurrencyCap(t *testing.T) {
	const userID = 77002
	const capN = 3

	// First `capN` attempts must succeed.
	for i := 0; i < capN; i++ {
		if !tryAcquireConcurrency(userID, capN) {
			t.Fatalf("attempt %d within cap should be allowed", i)
		}
	}
	// The next one must be rejected.
	if tryAcquireConcurrency(userID, capN) {
		t.Fatal("attempt beyond cap should be rejected")
	}
	// Releasing one frees a slot again.
	releaseConcurrency(userID)
	if !tryAcquireConcurrency(userID, capN) {
		t.Fatal("after one release, a request should be allowed again")
	}
	// Cleanup so we don't leak the counter.
	for i := 0; i < capN; i++ {
		releaseConcurrency(userID)
	}
}

func TestTryAcquireConcurrencyNegativeCapTreatedAsUnlimited(t *testing.T) {
	// A negative cap should behave like unlimited (defensive against bad input).
	if !tryAcquireConcurrency(77003, -5) {
		t.Fatal("negative cap should be treated as unlimited")
	}
	releaseConcurrency(77003)
}
