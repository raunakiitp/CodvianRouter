// RT-2 tests: LKGP TTL pinning (the G19 TTL half, pinned here
// directly in memory) and failure-clear — a hard failure against
// the pinned provider drops the pin, so the next request stops
// hoisting a dead target; a success re-records it.
package strategy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

func TestRT2LookupExpiresAfterTTL(t *testing.T) {
	// A pin older than LKGPRecordTTL is not returned by
	// Lookup (aged in memory, no Save/Load round-trip); a
	// fresh pin is returned; a zero-UpdatedAt record is
	// treated fresh (it cannot prove its age — see the
	// LKGPRecordTTL comment in affinity.go).
	s := NewLKGPStore()
	old := time.Now().Add(-LKGPRecordTTL - time.Minute)
	s.byChain["old"] = LKGPRecord{Provider: "prov-old", Model: "m-old", UpdatedAt: old}
	s.RecordSuccess("fresh", "prov-fresh", "m-fresh", "")
	s.byChain["zero"] = LKGPRecord{Provider: "prov-zero", Model: "m-zero"}

	if _, ok := s.Lookup("old"); ok {
		t.Fatalf("expired pin still returned by Lookup, want missing")
	}
	rec, ok := s.Lookup("fresh")
	if !ok || rec.Provider != "prov-fresh" {
		t.Fatalf("fresh pin Lookup = %+v, %v, want prov-fresh, true", rec, ok)
	}
	if _, ok := s.Lookup("zero"); !ok {
		t.Fatalf("zero-UpdatedAt pin missing, want treated fresh")
	}
	if _, ok := s.Lookup("missing"); ok {
		t.Fatalf("unknown chain key returned a pin, want missing")
	}
}

func TestRT2LoadDropsExpiredPins(t *testing.T) {
	// Same expiry, across a Save/Load round-trip: a stale pin
	// must not survive a restart.
	s := NewLKGPStore()
	s.RecordSuccess("chain-a", "prov-a", "model-a", "")
	s.RecordSuccess("chain-b", "prov-b", "model-b", "")
	path := filepath.Join(t.TempDir(), "lkgp.json")
	if err := s.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f lkgpFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rec := f.Records["chain-a"]
	rec.UpdatedAt = time.Now().Add(-LKGPRecordTTL - time.Minute)
	f.Records["chain-a"] = rec
	data, err = json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	dst := NewLKGPStore()
	if err := dst.Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, ok := dst.Lookup("chain-a"); ok {
		t.Fatalf("expired pin chain-a survived Load, want dropped")
	}
	if _, ok := dst.Lookup("chain-b"); !ok {
		t.Fatalf("fresh pin chain-b missing after Load, want present")
	}
}

func TestRT2LatestSkipsExpiredPins(t *testing.T) {
	// Latest()/LatestRecord() skip expired pins in memory:
	// with only a stale record there is no last-good at all,
	// and with a fresh record beside it the fresh one wins.
	r := NewLKGPRegistry()
	r.RecordSuccessFor("chain-old", "prov-old", "model-old", "")
	r.store.byChain["chain-old"] = LKGPRecord{
		Provider: "prov-old", Model: "model-old",
		UpdatedAt: time.Now().Add(-LKGPRecordTTL - time.Minute),
	}
	if _, ok := r.Latest(); ok {
		t.Fatalf("Latest() returned a stale-only pin, want none")
	}
	if _, ok := r.LatestRecord(); ok {
		t.Fatalf("LatestRecord() returned a stale-only pin, want none")
	}
	r.RecordSuccessFor("chain-new", "prov-new", "model-new", "")
	got, ok := r.Latest()
	if !ok || got != "model-new" {
		t.Fatalf("Latest() = %q, %v, want model-new, true", got, ok)
	}
	rec, ok := r.LatestRecord()
	if !ok || rec.Provider != "prov-new" || rec.Model != "model-new" {
		t.Fatalf("LatestRecord() = %+v, %v, want prov-new/model-new", rec, ok)
	}
}

func TestRT2ClearIfProviderOnlyClearsItsOwnProvider(t *testing.T) {
	s := NewLKGPStore()
	s.RecordSuccess("chain-a", "prov-a", "model-a", "")
	s.RecordSuccess("chain-b", "prov-b", "model-b", "")

	// A failure against another provider must not drop
	// chain-a's pin.
	if cleared := s.ClearIfProvider("chain-a", "prov-other"); cleared {
		t.Fatalf("ClearIfProvider cleared a pin for a different provider")
	}
	if _, ok := s.Lookup("chain-a"); !ok {
		t.Fatalf("pin for chain-a dropped by another provider's failure")
	}
	// A failure against the pinned provider clears it, and
	// only it.
	if cleared := s.ClearIfProvider("chain-a", "prov-a"); !cleared {
		t.Fatalf("ClearIfProvider did not clear the pinned provider's record")
	}
	if _, ok := s.Lookup("chain-a"); ok {
		t.Fatalf("pin for chain-a still present after ClearIfProvider")
	}
	if _, ok := s.Lookup("chain-b"); !ok {
		t.Fatalf("pin for chain-b dropped by chain-a's clear")
	}
	// Unknown chain key and empty arguments are no-ops.
	if cleared := s.ClearIfProvider("no-such-chain", "prov-a"); cleared {
		t.Fatalf("ClearIfProvider reported clearing an unknown chain")
	}
	if cleared := s.ClearIfProvider("", "prov-a"); cleared {
		t.Fatalf("ClearIfProvider cleared on an empty chain key")
	}
	if cleared := s.ClearIfProvider("chain-b", ""); cleared {
		t.Fatalf("ClearIfProvider cleared on an empty provider")
	}
	if _, ok := s.Lookup("chain-b"); !ok {
		t.Fatalf("pin for chain-b dropped by an empty-arg clear")
	}
}

func TestRT2ClearStopsHoistingAndSuccessReRecords(t *testing.T) {
	// The observable acceptance behaviour at the hook level:
	// a recorded pin hoists its provider first; clearing the
	// pin (what the engine does after a hard failure) stops
	// the hoist; re-recording the success restores it.
	s := NewLKGPStore()
	chain := []AffinityTarget{
		{Provider: "prov-live", Model: "m"},
		{Provider: "prov-dead", Model: "m"},
	}
	s.RecordSuccess("m", "prov-dead", "m", "")
	ordered := s.OrderByLKGP("m", chain)
	if ordered[0].Provider != "prov-dead" {
		t.Fatalf("recorded pin did not hoist: order %v, want prov-dead first", ordered)
	}
	if cleared := s.ClearIfProvider("m", "prov-dead"); !cleared {
		t.Fatalf("ClearIfProvider did not clear the dead provider's pin")
	}
	ordered = s.OrderByLKGP("m", chain)
	if ordered[0].Provider != "prov-live" {
		t.Fatalf("cleared pin still hoisted: order %v, want prov-live first", ordered)
	}
	// A success re-records the pin (in production this is
	// server.recordTelemetry on outcome ok).
	s.RecordSuccess("m", "prov-live", "m", "")
	ordered = s.OrderByLKGP("m", chain)
	if ordered[0].Provider != "prov-live" {
		t.Fatalf("re-recorded pin did not hoist: order %v, want prov-live first", ordered)
	}
}

// rt2StubClient is a minimal provider.Client for the
// end-to-end failure-clear test below.
type rt2StubClient struct {
	mu     sync.Mutex
	chats  int
	chatFn func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)
}

func (c *rt2StubClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.chats++
	fn := c.chatFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *rt2StubClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *rt2StubClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *rt2StubClient) TestKey(ctx context.Context) error                { return nil }

func (c *rt2StubClient) chatCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats
}

func TestRT2EngineFailureClearsPinEndToEnd(t *testing.T) {
	// Full loop through the real engine + real registry +
	// real lkgp hook: a pin hoists its provider first; that
	// provider's 500 clears the pin (via Config.
	// LKGPInvalidator, wired here exactly as main.go wires
	// it); the next request no longer hoists it; and a
	// success re-records the pin (simulating
	// server.recordTelemetry, which records on outcome ok),
	// so the serving provider hoists again.
	model := "rt2-e2e-model"
	defer DefaultLKGPRegistry.Store().Clear(model)
	DefaultLKGPRegistry.RecordSuccessFor(model, "prov-a", model, "")

	err500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	clientA := &rt2StubClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, err500
	}}
	clientB := &rt2StubClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-b"}, nil
	}}
	signals := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: false}
	}
	eng := router.NewEngine(
		map[string]provider.Client{"prov-a": clientA, "prov-b": clientB},
		nil,
		router.Config{
			MaxAttempts:            5,
			HedgeDelay:             -1,
			Strategy:               "lkgp",
			StrategyHookForRequest: HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil),
			LKGPInvalidator: func(chainKey, provider string) {
				DefaultLKGPRegistry.Store().ClearIfProvider(chainKey, provider)
			},
		},
	)
	chain := []router.Attempt{{ProviderID: "prov-a", Model: model}, {ProviderID: "prov-b", Model: model}}
	req := provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	// Request 1: the pin hoists prov-a first; it 500s; the
	// chain fails over to prov-b, which serves.
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if resp == nil || resp.Content != "from-b" {
		t.Fatalf("request 1 served %v, want from-b", resp)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].Provider != "prov-a" || trace.Attempts[1].Provider != "prov-b" {
		t.Fatalf("request 1 trace providers = %v, want [prov-a prov-b]", traceProviders(trace))
	}
	// The pin is gone: the 500 cleared it (the engine does
	// not re-record — that is telemetry's job on success).
	if _, ok := DefaultLKGPRegistry.LookupFor(model); ok {
		t.Fatalf("pin still present after the 500, want cleared")
	}

	// Request 2: nothing hoists prov-a first any more — the
	// declared order stands, prov-a is still tried (it is a
	// live chain member, only its stickiness is gone) and
	// 500s again, prov-b serves.
	resp, trace, err = eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("request 2: %v", err)
	}
	if resp == nil || resp.Content != "from-b" {
		t.Fatalf("request 2 served %v, want from-b", resp)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].Provider != "prov-a" || trace.Attempts[1].Provider != "prov-b" {
		t.Fatalf("request 2 trace providers = %v, want [prov-a prov-b]", traceProviders(trace))
	}

	// The success re-records the pin — here simulated the way
	// server.recordTelemetry does it (outcome ok, serving
	// provider + requested model) — so prov-b now hoists.
	DefaultLKGPRegistry.RecordSuccessFor(model, "prov-b", model, "")
	// Make prov-a healthy now so the hoist is observable as
	// serving order rather than another failover.
	clientA.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-a"}, nil
	}
	resp, trace, err = eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("request 3: %v", err)
	}
	if resp == nil || resp.Content != "from-b" {
		t.Fatalf("request 3 served %v, want from-b (prov-b hoisted first)", resp)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Provider != "prov-b" {
		t.Fatalf("request 3 trace providers = %v, want [prov-b]", traceProviders(trace))
	}
}

func TestRT2EngineTimeoutClearsPinEndToEnd(t *testing.T) {
	// Timeout class (408, ReasonTimeout in the taxonomy):
	// the same loop as the 500 test — a pin hoists prov-a
	// first; its 408 clears the pin via Config.
	// LKGPInvalidator; prov-b serves; the pin stays gone.
	model := "rt2-timeout-model"
	defer DefaultLKGPRegistry.Store().Clear(model)
	DefaultLKGPRegistry.RecordSuccessFor(model, "prov-a", model, "")

	err408 := &provider.UpstreamError{StatusCode: 408, Retryable: true, Body: "request timeout"}
	clientA := &rt2StubClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, err408
	}}
	clientB := &rt2StubClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-b"}, nil
	}}
	signals := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: false}
	}
	eng := router.NewEngine(
		map[string]provider.Client{"prov-a": clientA, "prov-b": clientB},
		nil,
		router.Config{
			MaxAttempts:            5,
			HedgeDelay:             -1,
			Strategy:               "lkgp",
			StrategyHookForRequest: HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil),
			LKGPInvalidator: func(chainKey, provider string) {
				DefaultLKGPRegistry.Store().ClearIfProvider(chainKey, provider)
			},
		},
	)
	chain := []router.Attempt{{ProviderID: "prov-a", Model: model}, {ProviderID: "prov-b", Model: model}}
	req := provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}

	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if resp == nil || resp.Content != "from-b" {
		t.Fatalf("request 1 served %v, want from-b", resp)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].Provider != "prov-a" || trace.Attempts[1].Provider != "prov-b" {
		t.Fatalf("request 1 trace providers = %v, want [prov-a prov-b]", traceProviders(trace))
	}
	if _, ok := DefaultLKGPRegistry.LookupFor(model); ok {
		t.Fatalf("pin still present after the 408 timeout, want cleared")
	}
}

func TestRT2LKGPDegradedDisablesStickiness(t *testing.T) {
	// RT-2 follow-up: a last-good provider whose breaker is
	// DEGRADED (slowing/failing, not yet OPEN) is never
	// hoisted first either — the declared order stands, exactly
	// as for an OPEN pin. The same pin with a healthy breaker
	// still hoists, and an OPEN pin is still excluded exactly
	// as before (no regression on the G19 behaviour).
	model := "rt2-lkgp-degraded-model"
	defer DefaultLKGPRegistry.Store().Clear(model)
	DefaultLKGPRegistry.RecordSuccessFor(model, "slow-prov", model, "")
	chain := []router.Attempt{
		{ProviderID: "live-prov", Model: model},
		{ProviderID: "slow-prov", Model: model},
	}
	hookFor := func(signals scoring.SignalsFunc) func([]router.Attempt) []router.Attempt {
		return HookFactoryWithCacheAffinity(signals, scoring.Weights{}, nil)("lkgp", router.StrategyHookContext{})
	}
	// Degraded: no hoist.
	degraded := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerDegraded: provider == "slow-prov"}
	}
	if ordered := hookFor(degraded)(chain); ordered[0].ProviderID != "live-prov" {
		t.Fatalf("DEGRADED pin hoisted first: order %v, want live-prov first", ordered)
	}
	// OPEN: still no hoist (regression check — the G19 rule
	// is unchanged, and OPEN no longer also reports degraded,
	// so the OPEN check alone must carry it).
	open := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: provider == "slow-prov"}
	}
	if ordered := hookFor(open)(chain); ordered[0].ProviderID != "live-prov" {
		t.Fatalf("OPEN pin hoisted first: order %v, want live-prov first", ordered)
	}
	// Both flags at once (a contradictory/overlapping input):
	// still no hoist — the checks are OR-ed, never
	// double-counted into a hoist.
	both := func(provider, m string) scoring.Signals {
		return scoring.Signals{BreakerOpen: provider == "slow-prov", BreakerDegraded: provider == "slow-prov"}
	}
	if ordered := hookFor(both)(chain); ordered[0].ProviderID != "live-prov" {
		t.Fatalf("OPEN+DEGRADED pin hoisted first: order %v, want live-prov first", ordered)
	}
	// Healthy: the pin hoists as before.
	healthy := func(provider, m string) scoring.Signals {
		return scoring.Signals{}
	}
	if ordered := hookFor(healthy)(chain); ordered[0].ProviderID != "slow-prov" {
		t.Fatalf("healthy pin did not hoist: order %v, want slow-prov first", ordered)
	}
}

func traceProviders(trace *routetrace.RouteTrace) []string {
	out := make([]string, 0, len(trace.Attempts))
	for _, a := range trace.Attempts {
		out = append(out, a.Provider)
	}
	return out
}
