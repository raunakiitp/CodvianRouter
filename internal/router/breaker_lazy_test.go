// Lazy recovery for the circuit breaker (PARITY.md:524,
// OmniRoute RESILIENCE_GUIDE "Lazy recovery"): when OPEN
// expires, reads refresh the state to HALF_OPEN (already true
// via breakerAllow), and the same reads — and the next failure —
// decay a stale DEGRADED back to CLOSED. No background timer.
//
// These tests drive breakerFailure directly (the breaker-counted
// failure entry point) with an OAuth profile (degrades at 5,
// opens at 8) and a 50 ms open window, so the decay hold is
// 50 ms and a 500 ms sleep is a 10x margin — no wall-clock
// flakiness. Without the decay, a DEGRADED breaker reads
// DEGRADED forever and a failure after a long quiet period
// counts consecutively (6th failure, still DEGRADED) instead
// of fresh (1st failure, CLOSED).
package router

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// lazyEngine builds an engine whose provider "a" takes the OAuth
// breaker profile (degrade 5 / open 8) with a 50 ms open window,
// so the lazy-recovery decay hold is 50 ms.
func lazyEngine() *Engine {
	return NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, Config{
		HedgeDelay:     -1,
		BreakerOpenFor: 50 * time.Millisecond,
		AuthProfileFor: func(providerID string) AuthProfile { return AuthProfileOAuth },
	})
}

// TestBreakerStaleDegradedDecaysToClosedOnRead: five failures
// degrade provider a; immediately it reads DEGRADED, but after
// a whole decay hold with no new failure the same read reports
// CLOSED. The count stays (read-only decay) until the next
// failure or a success.
func TestBreakerStaleDegradedDecaysToClosedOnRead(t *testing.T) {
	eng := lazyEngine()
	for i := 0; i < 5; i++ {
		eng.breakerFailure("a", "boom")
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerDegraded || st.Open {
		t.Fatalf("fresh: %+v, want DEGRADED, not open", st)
	}
	if eng.BreakerDegradedForProvider("a") != true {
		t.Fatalf("fresh: BreakerDegradedForProvider = false, want true")
	}
	time.Sleep(500 * time.Millisecond)
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerClosed || st.Open {
		t.Fatalf("stale: %+v, want CLOSED (decayed), not open", st)
	}
	if eng.BreakerDegradedForProvider("a") {
		t.Fatalf("stale: BreakerDegradedForProvider = true, want false (decayed to CLOSED)")
	}
	// The read decayed the state, not the history: the count
	// survives until the next failure or a success.
	if st := breakerStatesByID(eng)["a"]; st.ConsecutiveFailures != 5 {
		t.Fatalf("stale: consecutive failures = %d, want 5 (read-only decay keeps the count)", st.ConsecutiveFailures)
	}
}

// TestBreakerNextFailureAfterQuietHoldCountsFresh: five failures
// degrade provider a; after a whole decay hold with no new
// failure, the next failure counts as the FIRST (1, CLOSED),
// not the sixth (6, still DEGRADED).
func TestBreakerNextFailureAfterQuietHoldCountsFresh(t *testing.T) {
	eng := lazyEngine()
	for i := 0; i < 5; i++ {
		eng.breakerFailure("a", "boom")
	}
	time.Sleep(500 * time.Millisecond)
	eng.breakerFailure("a", "late")
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerClosed || st.ConsecutiveFailures != 1 {
		t.Fatalf("after quiet hold + 1 failure: %+v, want CLOSED with 1 consecutive failure (fresh count)", st)
	}
}

// TestBreakerStaleDegradedPersistsAcrossReload: the lazy-recovery
// clock survives a restart. Five failures, reload at once: still
// DEGRADED (the last failure was recent). Reload again after a
// whole decay hold: CLOSED. Without lastFailureAt in the store
// file, the reloaded state would have an unknown clock and never
// decay.
func TestBreakerStaleDegradedPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breakers.json")
	cfg := Config{
		HedgeDelay:       -1,
		BreakerOpenFor:   50 * time.Millisecond,
		BreakerStorePath: path,
		AuthProfileFor:   func(providerID string) AuthProfile { return AuthProfileOAuth },
	}
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	for i := 0; i < 5; i++ {
		eng.breakerFailure("a", "boom")
	}
	eng2 := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	if st := breakerStatesByID(eng2)["a"]; st.State != BreakerDegraded || st.ConsecutiveFailures != 5 {
		t.Fatalf("after reload (fresh): %+v, want DEGRADED with 5 failures", st)
	}
	time.Sleep(500 * time.Millisecond)
	eng3 := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	if st := breakerStatesByID(eng3)["a"]; st.State != BreakerClosed {
		t.Fatalf("after reload (stale): %+v, want CLOSED (lastFailureAt survived the restart, so the decay still applies)", st)
	}
}

// TestBreakerOpenKeepsEscalationAfterQuietHold: the staleness
// reset must NOT touch an OPEN/HALF_OPEN breaker. Eight failures
// trip provider a (1 trip); after the open window elapses, a
// failure delivered without a probe claim reopens it as the
// NEXT trip (trips 2, escalated) — not as a fresh first failure.
func TestBreakerOpenKeepsEscalationAfterQuietHold(t *testing.T) {
	eng := lazyEngine()
	for i := 0; i < 8; i++ {
		eng.breakerFailure("a", "boom")
	}
	if st := breakerStatesByID(eng)["a"]; !st.Open || st.Trips != 1 {
		t.Fatalf("tripped: %+v, want OPEN with 1 trip", st)
	}
	time.Sleep(500 * time.Millisecond)
	eng.breakerFailure("a", "late")
	if st := breakerStatesByID(eng)["a"]; !st.Open || st.Trips != 2 || st.ConsecutiveFailures != 9 {
		t.Fatalf("after quiet hold + failure on open breaker: %+v, want OPEN with 2 trips, 9 failures (escalation kept, not reset)", st)
	}
}
