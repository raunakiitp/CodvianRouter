package router

import (
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

type breakerEventSink struct {
	degraded  chan degradedCall
	recovered chan string
}

type degradedCall struct {
	provider    string
	consecutive int
	trips       int
	openFor     time.Duration
}

func (s *breakerEventSink) ProviderDegraded(p string, n, trips int, openFor time.Duration) {
	s.degraded <- degradedCall{p, n, trips, openFor}
}
func (s *breakerEventSink) ProviderRecovered(p string) {
	s.recovered <- p
}

// T7: breaker events are edge-triggered and grounded in the real
// breaker state — the first CLOSED->OPEN trip fires provider.degraded
// with the actual consecutive/trip/openFor numbers, a failure while
// the breaker is already open fires nothing, a probe-failure re-open
// fires again, and only a success that clears an OPEN breaker fires
// provider.recovered (a healthy provider's routine successes are
// silent). All calls are async, so the test waits on the sink's
// channels.
func TestBreakerTransitionEventsEdgeTriggered(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 3})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 4), recovered: make(chan string, 4)}
	eng.SetBreakerEvents(sink)

	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	eng.mu.Unlock()
	select {
	case d := <-sink.degraded:
		if d.provider != "p" || d.consecutive != 3 || d.trips != 1 || d.openFor <= 0 {
			t.Errorf("degraded = %+v, want p, 3 consecutive, 1 trip, openFor>0", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no provider.degraded on first trip")
	}
	select {
	case p := <-sink.recovered:
		t.Fatalf("recovered fired without an open breaker: %q", p)
	default:
	}

	// A fourth failure while OPEN must NOT fire again (steady state).
	eng.mu.Lock()
	eng.breakerFailureLocked("p", "upstream 5xx")
	eng.mu.Unlock()
	select {
	case d := <-sink.degraded:
		t.Fatalf("degraded fired again while breaker already open: %+v", d)
	case <-time.After(150 * time.Millisecond):
	}

	// Recovering clears the OPEN breaker and fires provider.recovered.
	eng.breakerSuccess("p")
	select {
	case p := <-sink.recovered:
		if p != "p" {
			t.Errorf("recovered = %q, want p", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no provider.recovered after open breaker cleared")
	}

	// A success for a provider that never tripped is silent (no state).
	eng.breakerSuccess("p2")
	select {
	case p := <-sink.recovered:
		t.Fatalf("recovered fired for a never-tripped provider: %q", p)
	default:
	}
}

// A probe (HALF_OPEN) failure reopens the breaker: it is a NEW open
// cycle, so provider.degraded fires again with the escalated trip count.
func TestBreakerProbeFailureRefiresDegraded(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 3})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 4), recovered: make(chan string, 4)}
	eng.SetBreakerEvents(sink)

	// Trip the breaker once (first degraded already consumed/ignored).
	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	<-sink.degraded
	// A half-open probe is in flight and now fails: reopen, escalated.
	eng.breakers["p"].probeInFlight = true
	eng.breakerFailureLocked("p", "probe 5xx")
	eng.mu.Unlock()

	select {
	case d := <-sink.degraded:
		if d.trips != 2 {
			t.Errorf("probe-refire trips = %d, want 2 (escalated)", d.trips)
		}
		if d.consecutive != 3 {
			t.Errorf("probe-refire consecutive = %d, want 3", d.consecutive)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no degraded on probe re-open")
	}
}
