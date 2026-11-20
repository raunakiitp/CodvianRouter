// M4.3 — model lockout: unit tests for the ladder + engine tests
// proving a dead model is skipped (not retried first) on the next
// request, and serves again after success or expiry.
package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

func TestModelLockoutLadder(t *testing.T) {
	m := NewModelLockout(LockoutConfig{Enabled: true, BaseCooldown: 120 * time.Second, MaxCooldown: 30 * time.Minute, MaxSteps: 10})
	// Disabled tables never lock — tested on a second table below.
	if _, locked := m.Locked("a", "", "m"); locked {
		t.Fatalf("fresh triple must not be locked")
	}
	// 1st failure: 120 s. 2nd: 240 s. 3rd: 480 s.
	_, until1 := m.Record("a", "", "m", 404)
	if until1.IsZero() {
		t.Fatalf("404 must lock the triple")
	}
	d1 := time.Until(until1)
	if d1 < 115*time.Second || d1 > 120*time.Second {
		t.Fatalf("1st lockout = %v, want ~120s", d1)
	}
	_, until2 := m.Record("a", "", "m", 404)
	d2 := time.Until(until2)
	if d2 < 235*time.Second || d2 > 240*time.Second {
		t.Fatalf("2nd lockout = %v, want ~240s", d2)
	}
	// A 500 is NOT a lockout code (OmniRoute's list has no 500):
	// it neither locks nor climbs the ladder.
	m2 := NewModelLockout(LockoutConfig{Enabled: true})
	if locked, _ := m2.Record("a", "", "m", 500); locked {
		t.Fatalf("500 must not lock the triple")
	}
	// A 400 is not a lockout code either.
	if locked, _ := m2.Record("a", "", "m", 400); locked {
		t.Fatalf("400 must not lock the triple")
	}
	// Every listed code locks.
	for _, code := range []int{403, 404, 429, 502, 503, 504} {
		mm := NewModelLockout(LockoutConfig{Enabled: true})
		if locked, _ := mm.Record("a", "", "m", code); !locked {
			t.Fatalf("status %d must lock the triple", code)
		}
	}
	// The ladder caps at MaxCooldown: 10 steps of doubling from
	// 120 s would be 61440 s, but the cap is 30 min.
	m3 := NewModelLockout(LockoutConfig{Enabled: true, BaseCooldown: 120 * time.Second, MaxCooldown: 30 * time.Minute, MaxSteps: 10})
	var last time.Time
	for i := 0; i < 12; i++ {
		_, last = m3.Record("a", "", "m", 404)
	}
	d := time.Until(last)
	if d < 29*time.Minute || d > 30*time.Minute {
		t.Fatalf("capped lockout = %v, want ~30m", d)
	}
	// Success clears: the next failure earns the base again.
	m3.Clear("a", "", "m")
	if _, locked := m3.Locked("a", "", "m"); locked {
		t.Fatalf("cleared triple must not be locked")
	}
	// A disabled table is a no-op on every method.
	off := NewModelLockout(LockoutConfig{})
	if locked, _ := off.Record("a", "", "m", 404); locked {
		t.Fatalf("disabled lockout must never lock")
	}
	if _, locked := off.Locked("a", "", "m"); locked {
		t.Fatalf("disabled lockout must never report locked")
	}
	if got := off.Locks(); len(got) != 0 {
		t.Fatalf("disabled lockout Locks() = %v, want empty", got)
	}
}

func TestModelLockoutExpiryRestartsLadder(t *testing.T) {
	m := NewModelLockout(LockoutConfig{Enabled: true, BaseCooldown: 50 * time.Millisecond, MaxCooldown: time.Minute, MaxSteps: 10})
	m.Record("a", "", "m", 404)
	m.Record("a", "", "m", 404) // 2nd rung: ~100 ms
	if _, locked := m.Locked("a", "", "m"); !locked {
		t.Fatalf("triple must be locked after 2 failures")
	}
	time.Sleep(150 * time.Millisecond)
	if _, locked := m.Locked("a", "", "m"); locked {
		t.Fatalf("triple must unlock after the cooldown ends")
	}
	// A failure after expiry earns the BASE cooldown again, not
	// the 3rd rung: the ladder restarts.
	_, until := m.Record("a", "", "m", 404)
	d := time.Until(until)
	if d < 40*time.Millisecond || d > 50*time.Millisecond {
		t.Fatalf("post-expiry lockout = %v, want ~50ms (base, ladder restarted)", d)
	}
}

// TestModelLockoutSkipsDeadModel: with the lockout on, a 404 dead
// model is called once, then skipped on the next request (trace:
// skipped_model_lockout) while the healthy sibling serves.
// Toggle: with the lockout off (default), the dead model is
// retried first on every request.
func TestModelLockoutSkipsDeadModel(t *testing.T) {
	ctx := context.Background()
	dead404 := &provider.UpstreamError{StatusCode: 404, Retryable: true, Body: "model not found"}
	lockCfg := Config{HedgeDelay: -1, ModelLockout: LockoutConfig{Enabled: true}}
	eng := NewEngine(map[string]provider.Client{"a": errClient(dead404), "b": okClient("bee")}, nil, lockCfg)
	chain := chainFor("a", "b")
	resp, _, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("req1 = %v, %v, want bee + nil", resp, err)
	}
	resp, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("req2 = %v, %v, want bee + nil", resp, err)
	}
	if got := traceOutcome(trace, "skipped_model_lockout"); got != 1 {
		t.Fatalf("req2 trace outcomes = %v, want exactly 1 skipped_model_lockout", traceOutcomes(trace))
	}
	// The dead model was dialled exactly once across both requests.
	a := eng.clients["a"].(*fakeClient)
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("dead model called %d times, want 1 (locked after the first 404)", got)
	}
	// The lockout is observable: one live lock on (a, "", m).
	locks := eng.ModelLockouts()
	if len(locks) != 1 || locks[0].ProviderID != "a" || locks[0].Model != "m" || locks[0].Failures != 1 {
		t.Fatalf("ModelLockouts() = %+v, want one lock on (a, m) with 1 failure", locks)
	}

	// Lockout off (the default): the dead model is retried first
	// on every request — the old behaviour, unchanged.
	eng2 := NewEngine(map[string]provider.Client{"a": errClient(dead404), "b": okClient("bee")}, nil, Config{HedgeDelay: -1})
	if _, _, err := eng2.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("off req1: %v", err)
	}
	if _, trace2, err := eng2.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("off req2: %v", err)
	} else if got := traceOutcome(trace2, "skipped_model_lockout"); got != 0 {
		t.Fatalf("lockout off: trace has %d skipped_model_lockout, want 0", got)
	}
	if got := eng2.clients["a"].(*fakeClient).chatCalls(); got != 2 {
		t.Fatalf("lockout off: dead model called %d times, want 2 (retried every request)", got)
	}
}

// TestModelLockoutSuccessClears: a model that recovers after its
// lock expires serves again at once — the success clears the lock
// (a lock that expired is gone; a success on the triple never
// leaves a stale entry behind). Note the lock itself is what
// prevents re-dialling while it runs: recovery is only observable
// after expiry, exactly like OmniRoute's lockout.
func TestModelLockoutSuccessClears(t *testing.T) {
	ctx := context.Background()
	dead404 := &provider.UpstreamError{StatusCode: 404, Retryable: true, Body: "model not found"}
	a := errClient(dead404)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": okClient("bee")}, nil,
		Config{HedgeDelay: -1, ModelLockout: LockoutConfig{Enabled: true, BaseCooldown: 50 * time.Millisecond}})
	chain := chainFor("a", "b")
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req1: %v", err)
	}
	if got := len(eng.ModelLockouts()); got != 1 {
		t.Fatalf("locks after req1 = %d, want 1", got)
	}
	// While locked, A is never dialled — the recovery request
	// proves it: A stays at 1 call while B serves.
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
		t.Fatalf("req2 (locked): %v", err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("locked: dead model called %d times, want 1 (never re-dialled while locked)", got)
	}
	// After expiry A is tried again; it recovered, so the success
	// clears the lock.
	time.Sleep(100 * time.Millisecond)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "a-ok"}, nil
	}
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "a-ok" {
		t.Fatalf("recovery req = %v, %v, want a-ok + nil", resp, err)
	}
	if got := eng.ModelLockouts(); len(got) != 0 {
		t.Fatalf("ModelLockouts() after success = %+v, want empty", got)
	}
}

// TestModelLockoutPerModel: a lock on model "m1" never blocks model
// "m2" on the same provider — the triple includes the model. The
// chains are built by hand here (not chainFor, which hardcodes
// Model "m"): production chains carry the requested model.
func TestModelLockoutPerModel(t *testing.T) {
	ctx := context.Background()
	dead404 := &provider.UpstreamError{StatusCode: 404, Retryable: true, Body: "model not found"}
	eng := NewEngine(map[string]provider.Client{"a": errClient(dead404), "b": okClient("bee")}, nil,
		Config{HedgeDelay: -1, ModelLockout: LockoutConfig{Enabled: true}})
	chainM1 := []Attempt{{ProviderID: "a", Model: "m1"}, {ProviderID: "b", Model: "m1"}}
	chainM2 := []Attempt{{ProviderID: "a", Model: "m2"}, {ProviderID: "b", Model: "m2"}}
	if _, _, err := eng.ChatWithChain(ctx, chainM1, coolReq("m1")); err != nil {
		t.Fatalf("m1 req: %v", err)
	}
	// m2 on the same provider is a different triple: A is tried.
	a := eng.clients["a"].(*fakeClient)
	before := a.chatCalls()
	if _, _, err := eng.ChatWithChain(ctx, chainM2, coolReq("m2")); err != nil {
		t.Fatalf("m2 req: %v", err)
	}
	if got := a.chatCalls(); got != before+1 {
		t.Fatalf("m2 req: dead provider called %d times (was %d), want +1 (m2 is a different triple)", got, before)
	}
}

// traceOutcome counts trace attempts with the given outcome.
func traceOutcome(trace *routetrace.RouteTrace, outcome string) int {
	if trace == nil {
		return 0
	}
	n := 0
	for _, at := range trace.Attempts {
		if at.Outcome == outcome {
			n++
		}
	}
	return n
}
