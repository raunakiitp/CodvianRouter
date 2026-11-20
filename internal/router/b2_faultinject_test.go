// B2-13 pinning tests — chaos-mode fault injection (faultinject.go,
// commit 1403ffe). Every test FAILS if the feature is off: the
// failover test serves from the faulted provider, the delay test
// shows no stall, and the MaxHits test never disarms.
package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestB2FaultInjectFailoverToFallback: an armed FaultFail on provider
// "a" must make the request fail over to "b" — and "a" must never be
// dialled (the fault intercepts in callChat before any provider call).
// The chain retries "a" (same-provider retries, MaxAttempts=5
// default) so the trace holds 4 injected "a" attempts + 1 serving
// "b" attempt — every "a" attempt carries the injection error.
func TestB2FaultInjectFailoverToFallback(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 503}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp == nil || resp.Content != "bee" {
		t.Fatalf("faulted failover = %v, %v, want bee + nil", resp, err)
	}
	if a.chatCalls() != 0 {
		t.Fatalf("faulted provider called %d times, want 0 (fault intercepts before dialling)", a.chatCalls())
	}
	if b.chatCalls() != 1 {
		t.Fatalf("fallback called %d times, want 1", b.chatCalls())
	}
	if len(trace.Attempts) != 5 {
		t.Fatalf("trace attempts = %d, want 5 (4 injected 'a' attempts + 1 serving 'b', MaxAttempts=5 default)", len(trace.Attempts))
	}
	aAttempts := 0
	for i, at := range trace.Attempts {
		if at.Provider == "a" {
			aAttempts++
			if !strings.Contains(at.Error, "chaos fault injection") {
				t.Fatalf("attempt %d: error = %q, want the chaos fault injection error", i, at.Error)
			}
		}
	}
	if last := trace.Attempts[len(trace.Attempts)-1]; last.Provider != "b" || last.Error != "" {
		t.Fatalf("last attempt = %+v, want provider b with no error (b served)", last)
	}
	faults := eng.ListFaults()
	if len(faults) != 1 || faults[0].Hits != aAttempts {
		t.Fatalf("ListFaults = %+v, want 1 fault with %d hits (one per 'a' attempt)", faults, aAttempts)
	}
}

// TestB2FaultInjectDisarmRestoresNormalRouting: after DisarmFault the
// same chain serves from "a" again, with no fault left armed.
func TestB2FaultInjectDisarmRestoresNormalRouting(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 503}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	if !eng.DisarmFault("a", "m") {
		t.Fatalf("DisarmFault = false, want true (fault was armed)")
	}
	if eng.DisarmFault("a", "m") {
		t.Fatalf("second DisarmFault = true, want false (already disarmed)")
	}
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp == nil || resp.Content != "aye" {
		t.Fatalf("after disarm = %v, %v, want aye + nil", resp, err)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 0 {
		t.Fatalf("after disarm: a=%d b=%d calls, want a=1 b=0", a.chatCalls(), b.chatCalls())
	}
	if len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %d, want 1 (no failover after disarm)", len(trace.Attempts))
	}
	if got := eng.ListFaults(); len(got) != 0 {
		t.Fatalf("ListFaults = %+v, want empty after disarm", got)
	}
}

// TestB2FaultInjectDelayStallsThenProceeds: a FaultDelay stalls the
// attempt for the configured delay and then lets the real provider
// call proceed — a slow provider, not a dead one.
func TestB2FaultInjectDelayStallsThenProceeds(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultDelay, Delay: 150 * time.Millisecond}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	start := time.Now()
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	elapsed := time.Since(start)
	if err != nil || resp == nil || resp.Content != "aye" {
		t.Fatalf("delayed call = %v, %v, want aye + nil", resp, err)
	}
	if elapsed < 120*time.Millisecond {
		t.Fatalf("delayed call took %v, want >= 120 ms (armed 150 ms stall)", elapsed)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("provider called %d times, want 1 (delay proceeds to the real call)", a.chatCalls())
	}
	if faults := eng.ListFaults(); len(faults) != 1 || faults[0].Hits != 1 {
		t.Fatalf("ListFaults = %+v, want 1 fault with 1 hit", faults)
	}
}

// TestB2FaultInjectMaxHitsAutoDisarm: a fault with MaxHits 2 fires
// twice, then auto-disarms — the third matching attempt proceeds
// normally and the registry is empty. Checked at the applyFault
// level, where one call is one hit (no chain retries in the way).
func TestB2FaultInjectMaxHitsAutoDisarm(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 503, MaxHits: 2}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	att := Attempt{ProviderID: "a", Model: "m"}
	for i := 0; i < 2; i++ {
		if _, injected := eng.applyFault(ctx, att, "x"); !injected {
			t.Fatalf("hit %d: want the fault to fire, it did not", i+1)
		}
	}
	if _, injected := eng.applyFault(ctx, att, "x"); injected {
		t.Fatalf("hit 3: want no injection (auto-disarmed at MaxHits 2), it fired")
	}
	if got := eng.ListFaults(); len(got) != 0 {
		t.Fatalf("ListFaults = %+v, want empty (auto-disarmed at MaxHits)", got)
	}
}

// TestB2FaultInjectMaxHitsAutoDisarmAcrossRequests: the same
// auto-disarm end to end. Each request burns 4 hits on "a" (initial
// + 3 same-provider retries under the MaxAttempts=5 default) before
// "b" serves, so MaxHits 8 covers exactly two requests — the third
// serves from "a".
func TestB2FaultInjectMaxHitsAutoDisarmAcrossRequests(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 503, MaxHits: 8}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	for i, want := range []string{"bee", "bee", "aye"} {
		resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
		if err != nil || resp == nil || resp.Content != want {
			t.Fatalf("request %d = %v, %v, want %s + nil", i+1, resp, err, want)
		}
	}
	if a.chatCalls() != 1 || b.chatCalls() != 2 {
		t.Fatalf("call counts: a=%d b=%d, want a=1 b=2 (fault fired twice per request, then disarmed)", a.chatCalls(), b.chatCalls())
	}
	if got := eng.ListFaults(); len(got) != 0 {
		t.Fatalf("ListFaults = %+v, want empty (auto-disarmed at MaxHits)", got)
	}
}

// TestB2FaultInjectErrorTypes: the injected error is a real provider
// error type — 429 unwraps to *RateLimitError, 402 to *QuotaError,
// anything else to *UpstreamError carrying the injected status — so
// the taxonomy, breaker and quota learning treat it exactly like
// the genuine failure. The message still names the injection.
func TestB2FaultInjectErrorTypes(t *testing.T) {
	ctx := context.Background()
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, Config{HedgeDelay: -1})

	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 429}); err != nil {
		t.Fatalf("ArmFault 429 = %v, want nil", err)
	}
	res, injected := eng.applyFault(ctx, Attempt{ProviderID: "a", Model: "m"}, "x")
	if !injected || res.err == nil {
		t.Fatalf("429 fault: injected=%v err=%v, want injected failure", injected, res.err)
	}
	var rl *provider.RateLimitError
	if !errors.As(res.err, &rl) {
		t.Fatalf("429 fault error = %T (%v), want *provider.RateLimitError", res.err, res.err)
	}
	if !res.called {
		t.Fatalf("429 fault: called=false, want true (failure flows through the normal path)")
	}
	if !strings.Contains(res.err.Error(), "chaos fault injection") {
		t.Fatalf("429 fault error = %q, want the message to name the injection", res.err.Error())
	}

	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 402}); err != nil {
		t.Fatalf("ArmFault 402 = %v, want nil", err)
	}
	res, injected = eng.applyFault(ctx, Attempt{ProviderID: "a", Model: "m"}, "x")
	if !injected || res.err == nil {
		t.Fatalf("402 fault: injected=%v err=%v, want injected failure", injected, res.err)
	}
	var qe *provider.QuotaError
	if !errors.As(res.err, &qe) {
		t.Fatalf("402 fault error = %T (%v), want *provider.QuotaError", res.err, res.err)
	}

	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 500}); err != nil {
		t.Fatalf("ArmFault 500 = %v, want nil", err)
	}
	res, injected = eng.applyFault(ctx, Attempt{ProviderID: "a", Model: "m"}, "x")
	if !injected || res.err == nil {
		t.Fatalf("500 fault: injected=%v err=%v, want injected failure", injected, res.err)
	}
	var ue *provider.UpstreamError
	if !errors.As(res.err, &ue) || ue.StatusCode != 500 {
		t.Fatalf("500 fault error = %T (%v), want *provider.UpstreamError with status 500", res.err, res.err)
	}
}

// TestB2FaultInjectModelNarrowing: a fault armed for one model does
// not fire for another model of the same provider.
func TestB2FaultInjectModelNarrowing(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{HedgeDelay: -1})
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "other", Kind: FaultFail, StatusCode: 503}); err != nil {
		t.Fatalf("ArmFault = %v, want nil", err)
	}
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b"), coolReq("m"))
	if err != nil || resp == nil || resp.Content != "aye" {
		t.Fatalf("other-model fault = %v, %v, want aye + nil (fault must not fire for model m)", resp, err)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("provider a called %d times, want 1 (fault is for model 'other', not 'm')", a.chatCalls())
	}
	if faults := eng.ListFaults(); len(faults) != 1 || faults[0].Hits != 0 {
		t.Fatalf("ListFaults = %+v, want 1 fault with 0 hits", faults)
	}
}

// TestB2FaultInjectOffByDefault: with no fault armed the engine
// behaves exactly as before — "a" serves, nothing is stalled, and
// ArmFault rejects invalid specs.
func TestB2FaultInjectOffByDefault(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{HedgeDelay: -1})
	resp, trace, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp == nil || resp.Content != "aye" {
		t.Fatalf("no fault armed = %v, %v, want aye + nil", resp, err)
	}
	if a.chatCalls() != 1 || len(trace.Attempts) != 1 {
		t.Fatalf("no fault armed: a=%d calls, %d attempts, want 1/1", a.chatCalls(), len(trace.Attempts))
	}
	if got := eng.ListFaults(); len(got) != 0 {
		t.Fatalf("ListFaults = %+v, want empty on a fresh engine", got)
	}
	if err := eng.ArmFault(FaultSpec{Kind: FaultFail}); err == nil {
		t.Fatalf("ArmFault with no provider: want an error, got nil")
	}
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Kind: FaultKind(99)}); err == nil {
		t.Fatalf("ArmFault with bad kind: want an error, got nil")
	}
	if err := eng.ArmFault(FaultSpec{ProviderID: "a", Model: "m", Kind: FaultFail, StatusCode: 99}); err == nil {
		t.Fatalf("ArmFault with status 99: want an error, got nil")
	}
	eng.ClearFaults()
	if got := eng.ListFaults(); len(got) != 0 {
		t.Fatalf("ListFaults after ClearFaults = %+v, want empty", got)
	}
}
