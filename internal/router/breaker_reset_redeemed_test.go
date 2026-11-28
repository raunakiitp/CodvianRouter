package router

import (
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// A4-7 provider.reset_redeemed: a reset redemption is BANKED when a
// provider's breaker trips open, and REDEEMED (emitting the event) when an
// operator claims it via ResetBreaker / ResetAllBreakers. The event is
// edge-triggered and grounded in real bank state: it fires only when a
// banked redemption is actually claimed, never for a reset with nothing
// banked (e.g. a merely-degraded provider), and a natural recovery voids
// the banked redemption so a later manual reset of a fresh state cannot
// redeem it. These tests drive the real engine (package-internal breaker
// calls), never a fake sink that fabricates the event.
func TestProviderResetRedeemedOnBankedRedemption(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 3})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 4), recovered: make(chan string, 4), redeemed: make(chan redeemedCall, 4)}
	eng.SetBreakerEvents(sink)

	before := time.Now()
	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	eng.mu.Unlock()
	// The trip consumed the degraded report.
	<-sink.degraded

	// The reset must now be banked (the trip banked it).
	eng.mu.Lock()
	bankedAt, banked := eng.resetBank["p"]
	eng.mu.Unlock()
	if !banked || bankedAt.Before(before) {
		t.Fatalf("after trip: banked=%v bankedAt=%v (want banked at trip time)", banked, bankedAt)
	}

	if !eng.ResetBreaker("p") {
		t.Fatalf("ResetBreaker(\"p\") = false, want true (breaker is tripped)")
	}
	select {
	case r := <-sink.redeemed:
		if r.provider != "p" {
			t.Errorf("reset_redeemed provider = %q, want p", r.provider)
		}
		if r.bankedAt.Before(before) {
			t.Errorf("reset_redeemed bankedAt = %v, want >= trip time", r.bankedAt)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no provider.reset_redeemed after a banked reset was redeemed")
	}
	// The redemption was claimed: nothing remains banked.
	eng.mu.Lock()
	_, still := eng.resetBank["p"]
	eng.mu.Unlock()
	if still {
		t.Fatal("reset remains banked after redemption")
	}
}

// A provider with breaker state but no open trip (degraded) can still be
// reset, but there is no banked redemption to redeem, so provider.reset_redeemed
// must NOT fire — an emit with nothing banked would be a no-op event.
func TestProviderResetRedeemedNotFiredWhenNothingBanked(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 8})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 4), recovered: make(chan string, 4), redeemed: make(chan redeemedCall, 4)}
	eng.SetBreakerEvents(sink)

	// 5 failures < threshold 8 => degraded, never open, nothing banked.
	eng.mu.Lock()
	for i := 0; i < 5; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	eng.mu.Unlock()

	if !eng.ResetBreaker("p") {
		t.Fatalf("ResetBreaker(\"p\") = false, want true (degraded breaker holds state)")
	}
	select {
	case r := <-sink.redeemed:
		t.Fatalf("provider.reset_redeemed fired without a banked redemption: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
}

// Natural recovery voids the banked reset: a reset-all / later reset of a
// provider that already recovered must not redeem a stale banked entry. And
// a fresh trip banks a fresh redemption, which its reset redeems.
func TestProviderResetRedeemedVoidedByRecovery(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 3})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 4), recovered: make(chan string, 4), redeemed: make(chan redeemedCall, 4)}
	eng.SetBreakerEvents(sink)

	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	eng.mu.Unlock()
	<-sink.degraded

	// Natural recovery clears the breaker AND voids the banked reset.
	eng.breakerSuccess("p")
	eng.mu.Lock()
	_, still := eng.resetBank["p"]
	eng.mu.Unlock()
	if still {
		t.Fatal("banked reset survived natural recovery (must be voided)")
	}

	// The consecutive breaker is gone, but the failure-window record from
	// the trip still exists, so ResetBreaker returns true (it clears the
	// window). With the bank voided there is nothing to redeem, so
	// provider.reset_redeemed must NOT fire.
	if !eng.ResetBreaker("p") {
		t.Fatalf("ResetBreaker(\"p\") after recovery = false, want true (window record still present)")
	}
	select {
	case r := <-sink.redeemed:
		t.Fatalf("reset_redeemed fired for a voided (recovered) reset: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}

	// A fresh trip re-banks a fresh redemption, which its reset redeems
	// (banking is edge-triggered per open cycle).
	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("p", "upstream 5xx")
	}
	eng.mu.Unlock()
	<-sink.degraded
	if !eng.ResetBreaker("p") {
		t.Fatalf("second ResetBreaker(\"p\") = false, want true")
	}
	select {
	case r := <-sink.redeemed:
		if r.provider != "p" {
			t.Errorf("second reset_redeemed provider = %q, want p", r.provider)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no provider.reset_redeemed after re-trip + reset")
	}
}

// ResetAllBreakers redeems every provider's banked redemption and fires
// provider.reset_redeemed once per provider.
func TestProviderResetRedeemedResetAllBreakers(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{BreakerThreshold: 3})
	sink := &breakerEventSink{degraded: make(chan degradedCall, 8), recovered: make(chan string, 4), redeemed: make(chan redeemedCall, 8)}
	eng.SetBreakerEvents(sink)

	eng.mu.Lock()
	for i := 0; i < 3; i++ {
		eng.breakerFailureLocked("a", "upstream 5xx")
		eng.breakerFailureLocked("b", "upstream 5xx")
	}
	eng.mu.Unlock()
	<-sink.degraded // a
	<-sink.degraded // b

	got := eng.ResetAllBreakers()
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("ResetAllBreakers() = %v, want [a b]", got)
	}
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case r := <-sink.redeemed:
			seen[r.provider] = true
		case <-deadline:
			t.Fatalf("reset_all reset_redeemed: saw %v, want both a and b", seen)
		}
	}
	if !seen["a"] || !seen["b"] {
		t.Fatalf("reset_all reset_redeemed providers = %v, want both a and b", seen)
	}
}
