package adaptive

import (
	"sync"
	"testing"
	"time"
)

func TestLimiterFailOpenNil(t *testing.T) {
	var l *Limiter
	if !l.TryAcquire("p") {
		t.Fatal("nil limiter must admit (fail open)")
	}
	l.Release("p") // must not panic
	l.Record("p", time.Millisecond, true)
	if got := l.Snapshot(); len(got) != 0 {
		t.Fatalf("nil limiter snapshot = %v, want empty", got)
	}
	if _, ok := l.StateFor("p"); ok {
		t.Fatal("nil limiter must report no state")
	}
	if !l.HasHeadroom("p") || l.InFlight("p") != 0 {
		t.Fatal("nil limiter must report headroom and 0 in-flight")
	}
}

func TestLimiterGradientShrinksOnSlowdown(t *testing.T) {
	l := New(Config{})
	// Warm baseline: 10 fast samples at 5ms.
	for i := 0; i < 10; i++ {
		l.Record("slow", 5*time.Millisecond, true)
	}
	before := l.Limit("slow")
	// Provider slows to 400ms: a handful of slow samples must shrink the cap.
	for i := 0; i < 5; i++ {
		l.Record("slow", 400*time.Millisecond, true)
	}
	after := l.Limit("slow")
	if after >= before {
		t.Fatalf("limit did not shrink on slowdown: before %.2f after %.2f", before, after)
	}
	if after > 6 {
		t.Fatalf("limit after 5x 400ms samples = %.2f, want it near the floor (<=6)", after)
	}
	st, ok := l.StateFor("slow")
	if !ok || st.Samples != 15 {
		t.Fatalf("state = %+v ok=%v, want 15 samples", st, ok)
	}
	if st.BaselineMs < 4 || st.BaselineMs > 6 {
		t.Fatalf("baseline = %.2fms, want ~5ms (the fast minimum)", st.BaselineMs)
	}
	if st.RTTMs < 100 {
		t.Fatalf("smoothed RTT = %.2fms, want >100ms after slow samples", st.RTTMs)
	}
}

func TestLimiterGradientRecoversOnFast(t *testing.T) {
	l := New(Config{})
	for i := 0; i < 5; i++ {
		l.Record("p", 5*time.Millisecond, true)
	}
	for i := 0; i < 5; i++ {
		l.Record("p", 400*time.Millisecond, true)
	}
	shrunk := l.Limit("p")
	// Fast again: the cap must grow back. Recovery is gradual by
	// design (Gradient2 grows by ~queueSize per sample once the
	// smoothed RTT has decayed back near the baseline, which
	// takes ~15 samples at smoothing 0.2), so 30 fast samples.
	for i := 0; i < 30; i++ {
		l.Record("p", 5*time.Millisecond, true)
	}
	if got := l.Limit("p"); got <= shrunk+2 {
		t.Fatalf("limit did not recover on fast RTTs: shrunk %.2f, now %.2f", shrunk, got)
	}
}

func TestLimiterCapEnforcedAndReleased(t *testing.T) {
	l := New(Config{InitialLimit: 2, MinLimit: 2, MaxLimit: 2})
	if !l.TryAcquire("p") || !l.TryAcquire("p") {
		t.Fatal("first two acquires must succeed at cap 2")
	}
	if l.TryAcquire("p") {
		t.Fatal("third acquire must be refused at cap 2")
	}
	if !l.HasHeadroom("p") {
		// HasHeadroom reports false at the cap; covered by the refusal above.
	}
	l.Release("p")
	if !l.TryAcquire("p") {
		t.Fatal("acquire after release must succeed")
	}
	l.Release("p")
	l.Release("p")
	l.Release("p") // over-release must clamp at 0, not go negative
	if got := l.InFlight("p"); got != 0 {
		t.Fatalf("in-flight after over-release = %d, want 0", got)
	}
	if !l.TryAcquire("p") {
		t.Fatal("acquire after over-release must succeed")
	}
}

func TestLimiterFailuresDoNotMoveCap(t *testing.T) {
	l := New(Config{})
	for i := 0; i < 5; i++ {
		l.Record("p", 5*time.Millisecond, true)
	}
	before := l.Limit("p")
	// Failed calls (429s, refusals) carry no serving RTT: they must
	// not move the gradient.
	for i := 0; i < 5; i++ {
		l.Record("p", 400*time.Millisecond, false)
	}
	if got := l.Limit("p"); got != before {
		t.Fatalf("failed samples moved the cap: before %.2f after %.2f", before, got)
	}
}

func TestLimiterClamps(t *testing.T) {
	l := New(Config{InitialLimit: 20, MinLimit: 2, MaxLimit: 100})
	// Extreme slowdown: cap must stop at the floor, never below it.
	for i := 0; i < 50; i++ {
		l.Record("p", 10*time.Second, true)
	}
	if got := l.Limit("p"); got < DefaultMinLimit {
		t.Fatalf("limit %.2f below floor %.2f", got, DefaultMinLimit)
	}
	// Extreme fast run: cap must stop at the ceiling.
	l2 := New(Config{InitialLimit: 99, MinLimit: 2, MaxLimit: 100})
	for i := 0; i < 50; i++ {
		l2.Record("p", time.Millisecond, true)
	}
	if got := l2.Limit("p"); got > DefaultMaxLimit {
		t.Fatalf("limit %.2f above ceiling %.2f", got, DefaultMaxLimit)
	}
}

func TestLimiterConcurrentSafe(t *testing.T) {
	l := New(Config{})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if l.TryAcquire("p") {
					l.Record("p", 5*time.Millisecond, true)
					l.Release("p")
				}
			}
		}()
	}
	wg.Wait()
	if got := l.InFlight("p"); got != 0 {
		t.Fatalf("in-flight after concurrent use = %d, want 0", got)
	}
}

func TestLimiterSnapshotSorted(t *testing.T) {
	l := New(Config{})
	l.Record("zeta", 5*time.Millisecond, true)
	l.Record("alpha", 5*time.Millisecond, true)
	snap := l.Snapshot()
	if len(snap) != 2 || snap[0].Provider != "alpha" || snap[1].Provider != "zeta" {
		t.Fatalf("snapshot not sorted by provider: %+v", snap)
	}
}
