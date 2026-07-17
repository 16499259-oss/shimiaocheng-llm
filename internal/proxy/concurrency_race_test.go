package proxy

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestTryAcquireConcurrencyRaceBoundary is a -race concurrency boundary test:
// with a cap of N, firing N+K goroutines that all call tryAcquireConcurrency
// concurrently must yield exactly N successes and K rejections. After releasing
// the N acquired slots the counter must return to 0, and — observed via a
// concurrent watcher — the live counter must never stray outside [0, N] (no
// negative, no over-cap) at any instant.
func TestTryAcquireConcurrencyRaceBoundary(t *testing.T) {
	const userID = 78001
	const N = 8
	const K = 6

	ForgetConcurrency(userID)

	total := N + K
	results := make([]bool, total)

	var wg sync.WaitGroup
	start := make(chan struct{})
	done := make(chan struct{})

	// Watcher samples the in-flight counter while the acquisitions race.
	go func() {
		for {
			select {
			case <-done:
				return
			default:
				if v, ok := userConcurrency.Load(userID); ok {
					c := atomic.LoadInt64(v.(*int64))
					if c < 0 || c > int64(N) {
						t.Errorf("counter out of bounds [0,%d] at some instant: got %d", N, c)
						return
					}
				}
			}
		}
	}()

	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = tryAcquireConcurrency(userID, N)
		}(i)
	}
	close(start)
	wg.Wait()
	close(done)

	oks, fails := 0, 0
	for _, ok := range results {
		if ok {
			oks++
		} else {
			fails++
		}
	}
	if oks != N {
		t.Fatalf("expected exactly %d successful acquires, got %d (fails=%d)", N, oks, fails)
	}
	if fails != K {
		t.Fatalf("expected exactly %d rejected acquires, got %d", K, fails)
	}

	// Release exactly the acquired slots; counter must return to 0.
	for i := 0; i < N; i++ {
		releaseConcurrency(userID)
	}
	if v, ok := userConcurrency.Load(userID); ok {
		if c := atomic.LoadInt64(v.(*int64)); c != 0 {
			t.Fatalf("expected counter 0 after releasing all acquired slots, got %d", c)
		}
	}
}

// TestTryAcquireRejectedDoesNotRelease verifies the failed-acquire path rolls
// back its own increment and does NOT call releaseConcurrency. A caller that
// (correctly) skips the deferred release on rejection must therefore not
// double-free / drive the counter negative.
func TestTryAcquireRejectedDoesNotRelease(t *testing.T) {
	const userID = 78002
	ForgetConcurrency(userID)

	if !tryAcquireConcurrency(userID, 1) {
		t.Fatal("first acquire should succeed")
	}
	if tryAcquireConcurrency(userID, 1) {
		t.Fatal("second acquire must be rejected at cap=1")
	}

	// Simulate the correct caller: release ONLY the successful acquire. The
	// rejected attempt must not have left a dangling increment.
	releaseConcurrency(userID)

	if v, ok := userConcurrency.Load(userID); ok {
		c := atomic.LoadInt64(v.(*int64))
		if c < 0 {
			t.Fatalf("counter went negative — double-free detected: %d", c)
		}
		if c != 0 {
			t.Fatalf("counter should be 0 after releasing only the successful acquire, got %d", c)
		}
	}
}

// TestAcquireSuccessThenEarlyReturnReleases models the handler's contract:
// after a successful acquire, an early return (e.g. JSON parse failure) is
// always guarded by `defer releaseConcurrency`, so the counter returns to its
// prior value. Here we mirror that deferred release directly.
func TestAcquireSuccessThenEarlyReturnReleases(t *testing.T) {
	const userID = 78003
	ForgetConcurrency(userID)

	if !tryAcquireConcurrency(userID, 1) {
		t.Fatal("acquire should succeed")
	}
	func() {
		defer releaseConcurrency(userID) // mirrors handler: defer releaseConcurrency(userID)
		// early return simulated; nothing else runs
	}()

	if v, ok := userConcurrency.Load(userID); ok {
		if c := atomic.LoadInt64(v.(*int64)); c != 0 {
			t.Fatalf("counter should be 0 after early-return release, got %d", c)
		}
	}
}
