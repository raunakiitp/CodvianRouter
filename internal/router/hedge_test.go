package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// Hedge vs explicit order (CHECKLIST 191): when the chain order is
// EXPLICIT — a per-request contract order, a routing-group member
// order, or a strategy-tree order — the operator or client stated
// which provider serves first, and the default 150 ms hedge must
// not silently override it. These tests prove the rule from both
// sides: an explicit-order slow first pick is NOT beaten by the
// hedge (no hedged_cancelled, second attempt never dialled), and
// the default (engine-computed) chain still hedges at the
// configured delay. Every test uses a first pick slower than the
// hedge delay, so with the rule removed the hedge would win and
// the test would fail — the slow pick serving is the proof, not
// the absence of a race.

// hasHedgedCancelled reports whether any attempt in the trace was
// cancelled as a hedge loser.
func hasHedgedCancelled(trace *routetrace.RouteTrace) bool {
	if trace == nil {
		return false
	}
	for _, att := range trace.Attempts {
		if att.Outcome == "hedged_cancelled" {
			return true
		}
	}
	return false
}

func TestExplicitOrderContractChainNeverHedges(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	req := contractReq(&provider.RequestRouting{Order: []string{"a", "b"}})
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "slow" {
		t.Fatalf("content = %q, want slow (explicit first pick served, not the hedge winner)", resp.Content)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("a calls = %d, want 1", a.chatCalls())
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b calls = %d, want 0 (hedge never launched on an explicit-order chain)", b.chatCalls())
	}
	if hasHedgedCancelled(trace) {
		t.Fatalf("trace has a hedged_cancelled attempt, want none: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeDisabledExplicitOrder {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeDisabledExplicitOrder)
	}
	if trace.HedgeDelayMs != 0 {
		t.Fatalf("trace.HedgeDelayMs = %d, want 0 (no hedge, no delay)", trace.HedgeDelayMs)
	}
}

func TestExplicitOrderGroupChainNeverHedges(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group == "fast" {
			return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}, true
		}
		return nil, false
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast"}
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "slow" {
		t.Fatalf("content = %q, want slow (group's first member served, not the hedge winner)", resp.Content)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b calls = %d, want 0 (hedge never launched on a group-ordered chain)", b.chatCalls())
	}
	if hasHedgedCancelled(trace) {
		t.Fatalf("trace has a hedged_cancelled attempt, want none: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeDisabledExplicitOrder {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeDisabledExplicitOrder)
	}
}

func TestExplicitOrderTreeChainNeverHedges(t *testing.T) {
	// cheapest-of[a, b] with a cost 10, b cost 1: the tree picks
	// b first. b is the slow first pick (300 ms); a is instant.
	// Without the rule the 20 ms hedge would race a in and a
	// would serve — the trace would show hedged_cancelled for b.
	a := okClient("fast")
	b := delayClient(300*time.Millisecond, "slow")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	cfg.StrategyHook = treeHook(map[string]float64{"a": 10, "b": 1}, map[string]float64{"a": 5, "b": 50})
	cfg.StrategyTree = &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
		{Provider: "a", Model: "m"},
		{Provider: "b", Model: "m"},
	}}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "slow" {
		t.Fatalf("content = %q, want slow (tree's first pick served, not the hedge winner)", resp.Content)
	}
	if a.chatCalls() != 0 {
		t.Fatalf("a calls = %d, want 0 (hedge never launched on a tree-ordered chain)", a.chatCalls())
	}
	if hasHedgedCancelled(trace) {
		t.Fatalf("trace has a hedged_cancelled attempt, want none: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeDisabledExplicitOrder {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeDisabledExplicitOrder)
	}
	// The tree path still names the leaf that served.
	if len(trace.StrategyTreePath) == 0 || trace.StrategyTreePath[len(trace.StrategyTreePath)-1] != "b" {
		t.Fatalf("trace.StrategyTreePath = %v, want it to end at b", trace.StrategyTreePath)
	}
}

func TestDefaultChainStillHedges(t *testing.T) {
	a := delayClient(300*time.Millisecond, "slow")
	b := okClient("fast")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	start := time.Now()
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "fast" {
		t.Fatalf("content = %q, want fast (hedge winner on the default chain)", resp.Content)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("elapsed = %v, hedge should have returned in well under 250ms", elapsed)
	}
	if !hasHedgedCancelled(trace) {
		t.Fatalf("trace has no hedged_cancelled attempt, want one for a: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeArmed {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeArmed)
	}
	if trace.HedgeDelayMs != 20 {
		t.Fatalf("trace.HedgeDelayMs = %d, want 20 (the real configured delay)", trace.HedgeDelayMs)
	}
}

func TestFlatCheapestChainStillHedges(t *testing.T) {
	// The boundary of the rule: a FLAT cheapest chain is ordered
	// by the engine's own scoring, not by an explicit statement —
	// so the hedge still applies even though the cheapest-first
	// pick is slow. An operator who wants cheapest-first
	// guaranteed uses a contract order, a group, or a tree.
	a := okClient("fast")
	b := delayClient(300*time.Millisecond, "slow")
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	cfg.Strategy = "cheapest"
	cfg.StrategyHook = treeHook(map[string]float64{"a": 10, "b": 1}, map[string]float64{"a": 5, "b": 50})
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "fast" {
		t.Fatalf("content = %q, want fast (hedge winner on the flat cheapest chain)", resp.Content)
	}
	if !hasHedgedCancelled(trace) {
		t.Fatalf("trace has no hedged_cancelled attempt, want one for b: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeArmed {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeArmed)
	}
}

// slowStreamClient streams one delta after d (or ctx end) — the
// streaming analogue of delayClient.
func slowStreamClient(d time.Duration, content string) *fakeClient {
	return &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		select {
		case <-time.After(d):
			if err := onDelta(content); err != nil {
				return nil, err
			}
			return &provider.ChatResponse{Content: content}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}

func TestExplicitOrderStreamNeverHedges(t *testing.T) {
	a := slowStreamClient(300*time.Millisecond, "slow")
	b := streamClient([]string{"fast"}, nil)
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	req := contractReq(&provider.RequestRouting{Order: []string{"a", "b"}})
	ch, err := eng.ChatStreamWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	out, err := collectStream(t, ch)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if out != "slow" {
		t.Fatalf("stream = %q, want slow (explicit first pick streamed, not the hedge winner)", out)
	}
	if b.streamCalls() != 0 {
		t.Fatalf("b stream calls = %d, want 0 (hedge never launched on an explicit-order stream)", b.streamCalls())
	}
	trace := eng.LastTrace()
	if hasHedgedCancelled(trace) {
		t.Fatalf("trace has a hedged_cancelled attempt, want none: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeDisabledExplicitOrder {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeDisabledExplicitOrder)
	}
}

func TestDefaultStreamStillHedges(t *testing.T) {
	a := slowStreamClient(300*time.Millisecond, "slow")
	b := streamClient([]string{"fast"}, nil)
	cfg := testConfig()
	cfg.HedgeDelay = 20 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	ch, err := eng.ChatStreamWithChain(context.Background(),
		[]Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	start := time.Now()
	out, err := collectStream(t, ch)
	if err != nil {
		t.Fatalf("stream err: %v", err)
	}
	if out != "fast" {
		t.Fatalf("stream = %q, want fast (hedge winner on the default stream)", out)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Fatalf("stream took %v, hedge should have won fast", time.Since(start))
	}
	trace := eng.LastTrace()
	if !hasHedgedCancelled(trace) {
		t.Fatalf("trace has no hedged_cancelled attempt, want one for a: %v", trace.Attempts)
	}
	if trace.Hedge != routetrace.HedgeModeArmed {
		t.Fatalf("trace.Hedge = %q, want %q", trace.Hedge, routetrace.HedgeModeArmed)
	}
	if trace.HedgeDelayMs != 20 {
		t.Fatalf("trace.HedgeDelayMs = %d, want 20", trace.HedgeDelayMs)
	}
}
