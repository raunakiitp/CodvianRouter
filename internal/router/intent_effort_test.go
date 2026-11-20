// Intent-tier resolved effort onto the engine attempt
// (CHECKLIST 269-270): the 259-260 slice's open gap — the
// resolved intent-tier effort was reported in headers but never
// carried onto the engine attempt (the 255-256 group-effort
// override covers routing groups only). These tests prove the
// tier-resolved (model, effort) reaches the provider: the fake
// upstream records the reasoning_effort it received, and the
// trace records the effort that served each attempt.
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// stubIntentRoute is an AutoRoute hook resolving any intent
// virtual model to the given concrete model at the given effort
// ("" = no effort pinned). It mirrors what the real autorouter
// returns for a mapped tier, without the classification path.
func stubIntentRoute(model, effort string) func(string, string) (string, autorouter.Decision, bool) {
	return func(virtualModel, _ string) (string, autorouter.Decision, bool) {
		tier, _ := autorouter.ParseIntentModel(virtualModel)
		return model, autorouter.Decision{
			VirtualModel: virtualModel,
			IntentTier:   tier,
			Model:        model,
			Effort:       autorouter.Effort(effort),
		}, true
	}
}

// intentEngine builds an engine whose only client ("a") records
// the reasoning_effort every call received, and whose AutoRoute
// resolves intent/* to model at effort.
func intentEngine(t *testing.T, model, effort string, seen *[]string) *Engine {
	t.Helper()
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		*seen = append(*seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute(model, effort)
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	return NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
}

// TestIntentEffortReachesProvider: intent/fast (low) reaches the
// upstream as reasoning_effort "low", and the trace records
// "low" on the serving attempt — the effort the provider
// actually received, not just the resolved one.
func TestIntentEffortReachesProvider(t *testing.T) {
	var seen []string
	eng := intentEngine(t, "model-b", "low", &seen)
	req := testReq()
	req.Model = "intent/fast"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("upstream efforts = %q, want [low]", seen)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != "low" {
		t.Fatalf("trace attempts = %v, want one attempt at effort low", tr)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok || dec.Effort != autorouter.EffortLow {
		t.Fatalf("last decision effort = %q (ok=%v), want low", dec.Effort, ok)
	}
}

// TestIntentEffortDiffersByTier: the provider receives a
// different effort for intent/fast (low) than for intent/best
// (high) — the tier definitions, not one shared value.
func TestIntentEffortDiffersByTier(t *testing.T) {
	var fastSeen, bestSeen []string
	fast := intentEngine(t, "model-b", "low", &fastSeen)
	best := intentEngine(t, "model-b", "high", &bestSeen)
	for _, tc := range []struct {
		eng   *Engine
		model string
		seen  *[]string
		want  string
	}{
		{fast, "intent/fast", &fastSeen, "low"},
		{best, "intent/best", &bestSeen, "high"},
	} {
		req := testReq()
		req.Model = tc.model
		if _, err := tc.eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat %s: %v", tc.model, err)
		}
		if len(*tc.seen) != 1 || (*tc.seen)[0] != tc.want {
			t.Fatalf("%s upstream efforts = %q, want [%s]", tc.model, *tc.seen, tc.want)
		}
		tr := tc.eng.LastTrace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != tc.want {
			t.Fatalf("%s trace attempts = %v, want one attempt at effort %s", tc.model, tr, tc.want)
		}
	}
}

// TestIntentEffortOverridesClientEffort: a tier-pinned effort is
// the operator's routing decision, so it wins over a
// client-supplied reasoning_effort — the same precedence a
// routing-group entry's effort has (CHECKLIST 255).
func TestIntentEffortOverridesClientEffort(t *testing.T) {
	var seen []string
	eng := intentEngine(t, "model-b", "low", &seen)
	req := testReq()
	req.Model = "intent/fast"
	req.ReasoningEffort = "high"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("upstream efforts = %q, want [low] (tier overrides client high)", seen)
	}
}

// TestIntentEffortUnpinnedPassesClientThrough: a tier with no
// effort pinned changes nothing — the client's own effort passes
// through to the wire, and the trace records no effort (the
// attempt ran at no explicit effort).
func TestIntentEffortUnpinnedPassesClientThrough(t *testing.T) {
	var seen []string
	eng := intentEngine(t, "model-b", "", &seen)
	req := testReq()
	req.Model = "intent/fast"
	req.ReasoningEffort = "high"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (client value passed through)", seen)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != "" {
		t.Fatalf("trace attempts = %v, want one attempt with no effort", tr)
	}
}

// TestIntentEffortGroupEntryWins: when the request is also
// group-addressed, the group entry's own effort wins over the
// tier's on that entry — the group entry is the operator's
// routing decision for that entry (CHECKLIST 255). The tier
// effort still rides the request, so an entry with no effort of
// its own would pass it through.
func TestIntentEffortGroupEntryWins(t *testing.T) {
	var seen []string
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seen = append(seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute("model-b", "low")
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "g" {
			return nil, false
		}
		return []Attempt{{ProviderID: "a", Model: model, Effort: "high"}}, true
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "intent/fast"
	req.Routing = &provider.RequestRouting{Group: "g"}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (group entry wins over tier low)", seen)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != "high" {
		t.Fatalf("trace attempts = %v, want one attempt at effort high", tr)
	}
}

// TestIntentEffortStream: the streaming path carries the tier's
// effort too — the stream upstream and the trace both see "low".
func TestIntentEffortStream(t *testing.T) {
	var seen []string
	a := &fakeClient{}
	a.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		seen = append(seen, req.ReasoningEffort)
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute("model-b", "low")
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "intent/fast"
	var sb strings.Builder
	if _, err := eng.ChatStream(context.Background(), req, func(d string) error { sb.WriteString(d); return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.String() != "ok" {
		t.Fatalf("stream deltas = %q, want ok", sb.String())
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("stream upstream efforts = %q, want [low]", seen)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != "low" {
		t.Fatalf("stream trace attempts = %v, want one attempt at effort low", tr)
	}
}

// TestWithResolvedEffort: the stamping helper stamps attempts
// with no effort, leaves an entry's own effort alone, never
// mutates the input chain, and returns the chain untouched when
// no effort resolved.
func TestWithResolvedEffort(t *testing.T) {
	chain := []Attempt{
		{ProviderID: "a", Model: "m"},
		{ProviderID: "b", Model: "m", Effort: "high"},
	}
	stamped := withResolvedEffort(chain, "low")
	if len(stamped) != 2 || stamped[0].Effort != "low" || stamped[1].Effort != "high" {
		t.Fatalf("stamped = %v, want [low high]", stamped)
	}
	if chain[0].Effort != "" {
		t.Fatalf("input chain mutated: %v", chain)
	}
	if got := withResolvedEffort(chain, ""); len(got) != 2 || got[0].Effort != "" {
		t.Fatalf("empty effort changed the chain: %v", got)
	}
}

// TestResolveAutoSetsRequestEffort: resolveAuto itself carries
// the tier's effort onto the request, so a chain the caller did
// not stamp (a group chain that replaced the stamped one) still
// sends the tier's effort via the request pass-through.
func TestResolveAutoSetsRequestEffort(t *testing.T) {
	eng := intentEngine(t, "model-b", "low", new([]string))
	req := testReq()
	req.Model = "intent/fast"
	_, out, effort, _ := eng.resolveAutoRequest(context.Background(), req)
	if out.Model != "model-b" {
		t.Fatalf("resolved model = %q, want model-b", out.Model)
	}
	if out.ReasoningEffort != "low" || effort != "low" {
		t.Fatalf("request effort = %q, returned effort = %q, want low", out.ReasoningEffort, effort)
	}
	plain := testReq()
	plain.Model = "model-b"
	_, out2, effort2, _ := eng.resolveAutoRequest(context.Background(), plain)
	if out2.ReasoningEffort != "" || effort2 != "" {
		t.Fatalf("non-intent request effort = %q / %q, want empty", out2.ReasoningEffort, effort2)
	}
}
