package router

// Multi-region failover order (CHECKLIST 257-258, research-log
// entry 232). Each test names the rule it proves, and the attempt
// sequence in the trace — not a comment — is the proof of the
// order. Chains are deliberately adversarial (the cross-region
// target is NOT first in the input order) so every test fails
// without the region tiering: without it the input order stands
// and the wrong target serves.

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// regionTestConfig wires RegionFor/SelfHosted from fixed maps, so
// the tests control exactly which (provider, model) is in which
// region and which provider is self-hosted — no registry, no
// guessing.
func regionTestConfig(t *testing.T, regions map[string]string, selfHosted map[string]bool) Config {
	t.Helper()
	cfg := testConfig()
	cfg.RegionFor = func(providerID, model string) string {
		return regions[providerID+"\x00"+model]
	}
	cfg.SelfHosted = func(providerID string) bool {
		return selfHosted[providerID]
	}
	return cfg
}

func regionReq(model string) provider.ChatRequest {
	req := testReq()
	req.Model = model
	return req
}

// TestRegionFailoverSameModelOtherRegionFirst proves step 1: a
// failing same-model same-region attempt goes to the same model
// in another region before any other model. FAILED without the
// fix (region tiering): the same-region target "b" served, the
// cross-region target "c" was never tried.
func TestRegionFailoverSameModelOtherRegionFirst(t *testing.T) {
	regions := map[string]string{
		"a\x00m": "us-east-1", "b\x00m": "us-east-1", "c\x00m": "eu-west-1",
		"d\x00m2": "eu-west-1",
	}
	cfg := regionTestConfig(t, regions, nil)
	a := errClient(errors.New("boom"))
	b := okClient("bee")
	c := okClient("cee")
	d := okClient("dee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c, "d": d}, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "b", Model: "m"},
		{ProviderID: "c", Model: "m"},
		{ProviderID: "d", Model: "m2"},
	}
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
	if err != nil || resp == nil || resp.Content != "cee" {
		t.Fatalf("Chat = %+v err = %v, want cee from cross-region C", resp, err)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 0 || c.chatCalls() != 1 || d.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d d=%d, want 1/0/1/0: same-region B and equivalent D must never be tried", a.chatCalls(), b.chatCalls(), c.chatCalls(), d.chatCalls())
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2 (failed A, serving C)", len(trace.Attempts))
	}
	if trace.Attempts[0].Provider != "a" || trace.Attempts[1].Provider != "c" {
		t.Fatalf("trace order = %q, %q, want a, c: the attempt sequence is the proof", trace.Attempts[0].Provider, trace.Attempts[1].Provider)
	}
	if trace.Attempts[0].Region != "us-east-1" || trace.Attempts[1].Region != "eu-west-1" {
		t.Fatalf("trace regions = %q, %q, want us-east-1, eu-west-1", trace.Attempts[0].Region, trace.Attempts[1].Region)
	}
	if trace.Attempts[1].FailoverStep != StepCrossRegion {
		t.Fatalf("serving attempt failover_step = %q, want %q", trace.Attempts[1].FailoverStep, StepCrossRegion)
	}
	if got := eng.Failover257Stats(); got.CrossRegion != 1 || got.EquivalentModel != 0 || got.SelfHosted != 0 {
		t.Fatalf("stats = %+v, want exactly 1 cross-region step", got)
	}
	if info, ok := eng.LastRouting(); !ok || info.RegionStep != StepCrossRegion {
		t.Fatalf("LastRouting region_step = %q (ok=%v), want %q", info.RegionStep, ok, StepCrossRegion)
	}
}

// TestRegionFailoverSameModelOtherRegionFirstStreaming is the
// streaming half of TestRegionFailoverSameModelOtherRegionFirst:
// the same order must hold on the stream path, proven by the
// same trace attempt sequence.
func TestRegionFailoverSameModelOtherRegionFirstStreaming(t *testing.T) {
	regions := map[string]string{
		"a\x00m": "us-east-1", "b\x00m": "us-east-1", "c\x00m": "eu-west-1",
	}
	cfg := regionTestConfig(t, regions, nil)
	a := streamClient(nil, errors.New("boom"))
	b := streamClient([]string{"bee"}, nil)
	c := streamClient([]string{"cee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "b", Model: "m"},
		{ProviderID: "c", Model: "m"},
	}
	ch, err := eng.ChatStreamWithChain(context.Background(), chain, regionReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	out, err := collectStream(t, ch)
	if err != nil || out != "cee" {
		t.Fatalf("stream = %q err = %v, want cee from cross-region C", out, err)
	}
	// M4.8 early-retry (see failover_243_test.go): the truncating
	// stream is re-opened 3 times on A before the cross-region
	// failover to C — B (same region as A) is still skipped.
	if a.streamCalls() != 4 || b.streamCalls() != 0 || c.streamCalls() != 1 {
		t.Fatalf("calls a=%d b=%d c=%d, want 4/0/1", a.streamCalls(), b.streamCalls(), c.streamCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 5 || trace.Attempts[0].Provider != "a" || trace.Attempts[4].Provider != "c" {
		t.Fatalf("trace = %+v, want [a, a, a, a, c]", trace)
	}
	if trace.Attempts[4].FailoverStep != StepCrossRegion {
		t.Fatalf("serving attempt failover_step = %q, want %q", trace.Attempts[4].FailoverStep, StepCrossRegion)
	}
	if got := eng.Failover257Stats(); got.CrossRegion != 1 {
		t.Fatalf("stats cross_region = %d, want 1", got.CrossRegion)
	}
}

// TestRegionFailoverFallsToEquivalentModel proves step 2: with
// no other region available, a failing same-model chain falls
// to an equivalent model from another provider — and the
// self-hosted model, listed first in the input chain, is NOT
// tried before it. FAILED without the fix: the self-hosted
// target "d" served first.
func TestRegionFailoverFallsToEquivalentModel(t *testing.T) {
	regions := map[string]string{
		"a\x00m": "us-east-1", "b\x00m": "us-east-1",
		"c\x00m2": "us-east-1", "d\x00m3": "us-east-1",
	}
	cfg := regionTestConfig(t, regions, map[string]bool{"d": true})
	a := errClient(errors.New("boom"))
	b := errClient(errors.New("boom"))
	c := okClient("cee")
	d := okClient("dee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c, "d": d}, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "d", Model: "m3"},
		{ProviderID: "c", Model: "m2"},
		{ProviderID: "b", Model: "m"},
	}
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
	if err != nil || resp == nil || resp.Content != "cee" {
		t.Fatalf("Chat = %+v err = %v, want cee from equivalent C", resp, err)
	}
	if d.chatCalls() != 0 {
		t.Fatalf("self-hosted calls = %d, want 0: the equivalent model must win over self-hosted", d.chatCalls())
	}
	if len(trace.Attempts) != 3 || trace.Attempts[2].Provider != "c" {
		t.Fatalf("trace = %+v, want [a, b, c]", trace.Attempts)
	}
	if trace.Attempts[2].FailoverStep != StepEquivalentModel {
		t.Fatalf("serving attempt failover_step = %q, want %q", trace.Attempts[2].FailoverStep, StepEquivalentModel)
	}
	// The decision header names the same step: the winner serves
	// m2 while m was requested, so the step must be computed
	// against the SERVING model — naming it against the requested
	// model compared a same-model failure with the same model and
	// reported no step at all (caught live by the smoke, R2c).
	if info, ok := eng.LastRouting(); !ok || info.RegionStep != StepEquivalentModel {
		t.Fatalf("LastRouting region_step = %q (ok=%v), want %q", info.RegionStep, ok, StepEquivalentModel)
	}
	if got := eng.Failover257Stats(); got.EquivalentModel != 1 || got.SelfHosted != 0 || got.CrossRegion != 0 {
		t.Fatalf("stats = %+v, want exactly 1 equivalent-model step", got)
	}
}

// TestRegionFailoverSelfHostedLast proves step 3: the
// self-hosted open-weight model is the last resort — it serves
// only after the same model in another region has also failed.
// FAILED without the fix: the self-hosted target "c", second in
// the input chain, served before the cross-region target "b".
func TestRegionFailoverSelfHostedLast(t *testing.T) {
	regions := map[string]string{
		"a\x00m": "us-east-1", "b\x00m": "eu-west-1", "c\x00m3": "us-east-1",
	}
	cfg := regionTestConfig(t, regions, map[string]bool{"c": true})
	a := errClient(errors.New("boom"))
	b := errClient(errors.New("boom"))
	c := okClient("cee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "c", Model: "m3"},
		{ProviderID: "b", Model: "m"},
	}
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
	if err != nil || resp == nil || resp.Content != "cee" {
		t.Fatalf("Chat = %+v err = %v, want cee from self-hosted C", resp, err)
	}
	if len(trace.Attempts) != 3 || trace.Attempts[1].Provider != "b" || trace.Attempts[2].Provider != "c" {
		t.Fatalf("trace = %+v, want [a, b, c]: cross-region before self-hosted", trace.Attempts)
	}
	if trace.Attempts[1].FailoverStep != StepCrossRegion {
		t.Fatalf("cross-region attempt failover_step = %q, want %q", trace.Attempts[1].FailoverStep, StepCrossRegion)
	}
	if trace.Attempts[2].FailoverStep != StepSelfHosted {
		t.Fatalf("serving attempt failover_step = %q, want %q", trace.Attempts[2].FailoverStep, StepSelfHosted)
	}
	// Same serving-model rule as the equivalent-model test: the
	// winner serves m3 while m was requested, so the header step
	// must be computed against m3 (caught live by the smoke, R3b
	// — before the fix the header named no step at all).
	if info, ok := eng.LastRouting(); !ok || info.RegionStep != StepSelfHosted {
		t.Fatalf("LastRouting region_step = %q (ok=%v), want %q", info.RegionStep, ok, StepSelfHosted)
	}
	if got := eng.Failover257Stats(); got.CrossRegion != 1 || got.SelfHosted != 1 || got.EquivalentModel != 0 {
		t.Fatalf("stats = %+v, want 1 cross-region + 1 self-hosted", got)
	}
}

// TestRegionFailoverUnknownRegionNeverCrossRegion proves an
// unknown region never claims a cross-region step: a same-model
// target with no declared region is tried AFTER a declared
// other-region target, and when the primary's own region is
// unknown, no hop from it can be a cross-region step at all.
func TestRegionFailoverUnknownRegionNeverCrossRegion(t *testing.T) {
	t.Run("unknown target after declared other region", func(t *testing.T) {
		regions := map[string]string{"a\x00m": "us-east-1", "c\x00m": "eu-west-1"}
		cfg := regionTestConfig(t, regions, nil)
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		c := okClient("cee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, cfg)
		chain := []Attempt{
			{ProviderID: "a", Model: "m"},
			{ProviderID: "b", Model: "m"},
			{ProviderID: "c", Model: "m"},
		}
		resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
		if err != nil || resp == nil || resp.Content != "cee" {
			t.Fatalf("Chat = %+v err = %v, want cee from declared other-region C", resp, err)
		}
		if b.chatCalls() != 0 {
			t.Fatalf("unknown-region calls = %d, want 0: it must sort after a declared other region", b.chatCalls())
		}
		if trace.Attempts[1].FailoverStep != StepCrossRegion {
			t.Fatalf("failover_step = %q, want %q", trace.Attempts[1].FailoverStep, StepCrossRegion)
		}
	})
	t.Run("unknown primary region names no step", func(t *testing.T) {
		regions := map[string]string{"b\x00m": "eu-west-1"}
		cfg := regionTestConfig(t, regions, nil)
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
		chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
		resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee", resp, err)
		}
		if trace.Attempts[1].FailoverStep != "" {
			t.Fatalf("failover_step = %q, want \"\": an unknown primary region cannot prove a cross-region hop", trace.Attempts[1].FailoverStep)
		}
		if got := eng.Failover257Stats(); got.CrossRegion != 0 {
			t.Fatalf("stats cross_region = %d, want 0", got.CrossRegion)
		}
	})
}

// TestRegionFailoverSameRegionHopIsNotARegionStep proves a
// same-model same-region hop to another provider is a plain
// provider failover: it names no region step and counts nothing.
func TestRegionFailoverSameRegionHopIsNotARegionStep(t *testing.T) {
	regions := map[string]string{"a\x00m": "us-east-1", "b\x00m": "us-east-1"}
	cfg := regionTestConfig(t, regions, nil)
	a := errClient(errors.New("boom"))
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	resp, trace, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
	if err != nil || resp == nil || resp.Content != "bee" {
		t.Fatalf("Chat = %+v err = %v, want bee", resp, err)
	}
	if trace.Attempts[1].FailoverStep != "" {
		t.Fatalf("failover_step = %q, want \"\"", trace.Attempts[1].FailoverStep)
	}
	if got := eng.Failover257Stats(); got != (Failover257Stats{}) {
		t.Fatalf("stats = %+v, want all zero", got)
	}
	if info, ok := eng.LastRouting(); !ok || info.RegionStep != "" {
		t.Fatalf("LastRouting region_step = %q (ok=%v), want \"\"", info.RegionStep, ok)
	}
}

// TestRegionFailoverNonRetryableDoesNotStep proves the existing
// failover rules still bound the region order: a terminal 400
// stops the chain — no region step is taken, nothing is counted,
// and the next target is never dialled.
func TestRegionFailoverNonRetryableDoesNotStep(t *testing.T) {
	regions := map[string]string{"a\x00m": "us-east-1", "b\x00m": "eu-west-1"}
	cfg := regionTestConfig(t, regions, nil)
	a := errClient(&provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`})
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	_, _, err := eng.ChatWithChain(context.Background(), chain, regionReq("m"))
	if err == nil {
		t.Fatalf("Chat err = nil, want the terminal 400 to stop the chain")
	}
	if b.chatCalls() != 0 {
		t.Fatalf("cross-region calls = %d, want 0: a terminal failure never fails over", b.chatCalls())
	}
	if got := eng.Failover257Stats(); got != (Failover257Stats{}) {
		t.Fatalf("stats = %+v, want all zero", got)
	}
}

// TestRegionTieringPreservesExplicitOrder proves an explicit
// client order stays final: region tiering never re-ranks a
// contract-ordered chain (the safety partitions still apply,
// the preference does not).
func TestRegionTieringPreservesExplicitOrder(t *testing.T) {
	regions := map[string]string{
		"a\x00m": "us-east-1", "b\x00m": "us-east-1", "c\x00m": "eu-west-1",
	}
	cfg := regionTestConfig(t, regions, nil)
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "b", Model: "m"},
		{ProviderID: "c", Model: "m"},
	}
	ordered, _ := eng.orderAttempts(chain, "balanced", "hard", true)
	if len(ordered) != 3 || ordered[0].ProviderID != "a" || ordered[1].ProviderID != "b" || ordered[2].ProviderID != "c" {
		t.Fatalf("explicit order = %v, want [a, b, c] unchanged", ordered)
	}
	// Control: the same chain without the explicit-order flag IS
	// region-tiered (c moves ahead of b).
	ordered, _ = eng.orderAttempts(chain, "balanced", "hard", false)
	if len(ordered) != 3 || ordered[0].ProviderID != "a" || ordered[1].ProviderID != "c" || ordered[2].ProviderID != "b" {
		t.Fatalf("engine order = %v, want [a, c, b]", ordered)
	}
}

// TestRegionTieringKeepsPrimaryFirst proves the primary pick is
// never demoted by the tiering: the first attempt after scoring
// stays first, and the tiers order only what follows it —
// including when the primary is itself self-hosted (the
// operator's scoring picked it; the region order governs the
// failover after it, not the pick).
func TestRegionTieringKeepsPrimaryFirst(t *testing.T) {
	regions := map[string]string{"a\x00m": "us-east-1", "b\x00m": "eu-west-1"}
	cfg := regionTestConfig(t, regions, map[string]bool{"a": true})
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	ordered, _ := eng.orderAttempts(chain, "balanced", "hard", false)
	if len(ordered) != 2 || ordered[0].ProviderID != "a" || ordered[1].ProviderID != "b" {
		t.Fatalf("order = %v, want [a, b]: the primary stays first", ordered)
	}
}
