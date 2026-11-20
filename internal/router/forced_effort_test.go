// Forced-reasoning policy (CHECKLIST 311-312): an operator
// policy forces an effort on a model or route whatever the
// client asked for, unless the policy says the client may
// override — and the trace records which won. These tests prove
// the forced effort is what the provider actually received, on
// the wire and in the trace, not just what the engine resolved.
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// forcedLookup builds a ForcedEffortFor hook from rules in
// declared order: first exact match wins, else the first "*".
func forcedLookup(policies []ForcedEffort) func(string) (ForcedEffort, bool) {
	return func(model string) (ForcedEffort, bool) {
		for _, p := range policies {
			if p.Model == model {
				return p, true
			}
		}
		for _, p := range policies {
			if p.Model == "*" {
				return p, true
			}
		}
		return ForcedEffort{}, false
	}
}

// forcedEngine builds an engine whose only client ("a") records
// the reasoning_effort every call received, and whose
// ForcedEffortFor answers from the given rules.
func forcedEngine(t *testing.T, policies []ForcedEffort, seen *[]string) *Engine {
	t.Helper()
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		*seen = append(*seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.ForcedEffortFor = forcedLookup(policies)
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	return NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
}

// checkForcedTrace asserts the trace's serving attempt carries
// the effective effort and the policy label naming the winner.
func checkForcedTrace(t *testing.T, eng *Engine, effort, policy string) {
	t.Helper()
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 {
		t.Fatalf("trace attempts = %v, want one attempt", tr)
	}
	at := tr.Attempts[0]
	if at.Effort != effort || at.EffortPolicy != policy {
		t.Fatalf("trace attempt effort = %q policy = %q, want %q / %q", at.Effort, at.EffortPolicy, effort, policy)
	}
}

// TestForcedEffortPolicyWinsOverClient: a policy on model-a
// (high) beats a client-supplied reasoning_effort (low) — the
// provider receives high, and the trace names the policy.
func TestForcedEffortPolicyWinsOverClient(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "model-a", Effort: "high"}}, &seen)
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (policy beats client low)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:model-a")
}

// TestForcedEffortAllowClientOverride: with
// allow_client_override the client's value wins — the provider
// receives the client's low, and the trace records that the
// client overrode the policy (never silently).
func TestForcedEffortAllowClientOverride(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "model-b", Effort: "high", AllowClientOverride: true}}, &seen)
	req := testReq()
	req.Model = "model-b"
	req.ReasoningEffort = "low"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("upstream efforts = %q, want [low] (client overrides policy under the flag)", seen)
	}
	checkForcedTrace(t, eng, "low", "client-override:low:policy:model-b")
}

// TestForcedEffortOverrideWithoutClientValue: an
// override-allowed policy still forces its effort when the
// client sent no value — the flag lets a client value win, it
// does not switch the policy off.
func TestForcedEffortOverrideWithoutClientValue(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "model-b", Effort: "high", AllowClientOverride: true}}, &seen)
	req := testReq()
	req.Model = "model-b"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (no client value, policy still forces)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:model-b")
}

// TestForcedEffortBeatsTier: the policy beats an intent-tier
// effort too — intent/fast pins low, the policy on the resolved
// model forces high, and the provider receives high.
func TestForcedEffortBeatsTier(t *testing.T) {
	var seen []string
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seen = append(seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute("model-b", "low")
	cfg.ForcedEffortFor = forcedLookup([]ForcedEffort{{Model: "model-b", Effort: "high"}})
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "intent/fast"
	req.ReasoningEffort = "medium"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (policy beats tier low and client medium)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:model-b")
}

// TestForcedEffortRoutePolicy: a policy on the route itself —
// the requested virtual model "intent/fast" — matches before
// resolution, so it forces high even though the resolved model
// has no rule of its own.
func TestForcedEffortRoutePolicy(t *testing.T) {
	var seen []string
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seen = append(seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute("model-b", "low")
	cfg.ForcedEffortFor = forcedLookup([]ForcedEffort{{Model: "intent/fast", Effort: "high"}})
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "intent/fast"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (route policy beats tier low)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:intent/fast")
}

// TestForcedEffortBeatsGroupEntry: a policy on the model beats a
// routing-group entry's effort — the group chain replaces the
// entry-point chain, so this proves the re-stamp in
// ChatWithChainStrategy holds the policy (without it the mock
// would see the entry's low).
func TestForcedEffortBeatsGroupEntry(t *testing.T) {
	var seen []string
	a := &fakeClient{}
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seen = append(seen, req.ReasoningEffort)
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.ForcedEffortFor = forcedLookup([]ForcedEffort{{Model: "model-b", Effort: "high"}})
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "g" {
			return nil, false
		}
		return []Attempt{{ProviderID: "a", Model: model, Effort: "low"}}, true
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "model-b"
	req.Routing = &provider.RequestRouting{Group: "g"}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("upstream efforts = %q, want [high] (policy beats group entry low)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:model-b")
}

// TestForcedEffortWildcard: a "*" rule forces every model with
// no exact rule — the client value loses to the wildcard too.
func TestForcedEffortWildcard(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "*", Effort: "medium"}}, &seen)
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "medium" {
		t.Fatalf("upstream efforts = %q, want [medium] (wildcard forces)", seen)
	}
	checkForcedTrace(t, eng, "medium", "forced:medium:policy:*")
}

// TestForcedEffortNoMatchPassesThrough: with rules for other
// models only, the client's value passes through untouched and
// the trace records no policy.
func TestForcedEffortNoMatchPassesThrough(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "model-b", Effort: "high"}}, &seen)
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("upstream efforts = %q, want [low] (no rule matches)", seen)
	}
	checkForcedTrace(t, eng, "", "")
}

// TestForcedEffortForgedLabelDropped: a client-supplied
// EffortPolicy is dropped when no rule matches — the trace
// never carries a forged policy label.
func TestForcedEffortForgedLabelDropped(t *testing.T) {
	var seen []string
	eng := forcedEngine(t, []ForcedEffort{{Model: "model-b", Effort: "high"}}, &seen)
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	req.EffortPolicy = "forced:high:policy:forged"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(seen) != 1 || seen[0] != "low" {
		t.Fatalf("upstream efforts = %q, want [low]", seen)
	}
	checkForcedTrace(t, eng, "", "")
}

// TestForcedEffortStream: the streaming path carries the forced
// effort too — the stream upstream and the trace both see it.
func TestForcedEffortStream(t *testing.T) {
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
	cfg.ForcedEffortFor = forcedLookup([]ForcedEffort{{Model: "model-a", Effort: "high"}})
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	var sb strings.Builder
	if _, err := eng.ChatStream(context.Background(), req, func(d string) error { sb.WriteString(d); return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.String() != "ok" {
		t.Fatalf("stream deltas = %q, want ok", sb.String())
	}
	if len(seen) != 1 || seen[0] != "high" {
		t.Fatalf("stream upstream efforts = %q, want [high] (policy beats client low)", seen)
	}
	checkForcedTrace(t, eng, "high", "forced:high:policy:model-a")
}

// TestWithForcedEffort: the stamping helper stamps a
// request-level policy onto every attempt (over a tier or group
// effort), falls back to per-attempt rules when no request-level
// rule matched, never mutates the input chain, and returns the
// chain and request untouched when nothing matches.
func TestWithForcedEffort(t *testing.T) {
	cfg := testConfig()
	cfg.ForcedEffortFor = forcedLookup([]ForcedEffort{
		{Model: "model-a", Effort: "high"},
		{Model: "model-c", Effort: "low", AllowClientOverride: true},
	})
	eng := NewEngine(map[string]provider.Client{}, &fakeLedger{}, cfg)
	chain := []Attempt{
		{ProviderID: "a", Model: "model-a", Effort: "low"}, // tier/group effort: policy wins
		{ProviderID: "b", Model: "model-b"},
	}
	req := testReq()
	req.Model = "model-a"
	req.ReasoningEffort = "low"
	stamped, outReq := eng.withForcedEffort(chain, req, "model-a", "low")
	if len(stamped) != 2 || stamped[0].Effort != "high" || stamped[0].EffortPolicy != "forced:high:policy:model-a" {
		t.Fatalf("stamped[0] = %+v, want effort high + forced label", stamped[0])
	}
	if stamped[1].Effort != "high" || stamped[1].EffortPolicy != "forced:high:policy:model-a" {
		t.Fatalf("stamped[1] = %+v, want request-level policy on every attempt", stamped[1])
	}
	if outReq.ReasoningEffort != "high" || outReq.EffortPolicy != "forced:high:policy:model-a" {
		t.Fatalf("request = %+v, want effort high + forced label", outReq)
	}
	if chain[0].Effort != "low" || req.ReasoningEffort != "low" {
		t.Fatalf("input mutated: chain=%v req=%+v", chain, req)
	}
	// No request-level rule: per-attempt rule applies to its own
	// attempt only, and the client wins under allow-override.
	chain2 := []Attempt{
		{ProviderID: "a", Model: "model-c"},
		{ProviderID: "b", Model: "model-b"},
	}
	req2 := testReq()
	req2.Model = "model-b"
	req2.ReasoningEffort = "medium"
	stamped2, outReq2 := eng.withForcedEffort(chain2, req2, "model-b", "medium")
	if stamped2[0].Effort != "medium" || stamped2[0].EffortPolicy != "client-override:medium:policy:model-c" {
		t.Fatalf("stamped2[0] = %+v, want client-override medium", stamped2[0])
	}
	if stamped2[1].Effort != "" || stamped2[1].EffortPolicy != "" {
		t.Fatalf("stamped2[1] = %+v, want untouched", stamped2[1])
	}
	if outReq2.ReasoningEffort != "medium" || outReq2.EffortPolicy != "" {
		t.Fatalf("outReq2 = %+v, want request untouched (no request-level rule)", outReq2)
	}
	// Nothing matches at all: chain and request come back unchanged.
	chain3 := []Attempt{{ProviderID: "a", Model: "model-z"}}
	req3 := testReq()
	req3.Model = "model-z"
	req3.ReasoningEffort = "low"
	stamped3, outReq3 := eng.withForcedEffort(chain3, req3, "model-z", "low")
	if stamped3[0].Effort != "" || stamped3[0].EffortPolicy != "" || outReq3.ReasoningEffort != "low" || outReq3.EffortPolicy != "" {
		t.Fatalf("no-match changed something: %v %+v", stamped3, outReq3)
	}
}

// TestApplyRequestForcedEffort: the group re-stamp copies the
// request-level decision onto a fresh chain, never mutates the
// input, and leaves a chain unchanged when the request carries
// no decision.
func TestApplyRequestForcedEffort(t *testing.T) {
	chain := []Attempt{
		{ProviderID: "a", Model: "model-b", Effort: "low"},
		{ProviderID: "b", Model: "model-b"},
	}
	req := testReq()
	req.Model = "model-b"
	req.ReasoningEffort = "high"
	req.EffortPolicy = "forced:high:policy:model-b"
	stamped := applyRequestForcedEffort(chain, req)
	if len(stamped) != 2 || stamped[0].Effort != "high" || stamped[0].EffortPolicy != "forced:high:policy:model-b" || stamped[1].Effort != "high" {
		t.Fatalf("stamped = %v, want both attempts at forced high", stamped)
	}
	if chain[0].Effort != "low" {
		t.Fatalf("input chain mutated: %v", chain)
	}
	req2 := testReq()
	if got := applyRequestForcedEffort(chain, req2); got[0].Effort != "low" {
		t.Fatalf("no-decision request changed the chain: %v", got)
	}
}

// TestResolveForcedWinner: the winner rule — client wins only
// under allow-override with a value present; otherwise the
// policy wins; values are normalized.
func TestResolveForcedWinner(t *testing.T) {
	for _, tc := range []struct {
		pol        ForcedEffort
		client     string
		wantEffort string
		wantLabel  string
	}{
		{ForcedEffort{Model: "m", Effort: "high"}, "low", "high", "forced:high:policy:m"},
		{ForcedEffort{Model: "m", Effort: "high"}, "", "high", "forced:high:policy:m"},
		{ForcedEffort{Model: "m", Effort: "high", AllowClientOverride: true}, "low", "low", "client-override:low:policy:m"},
		{ForcedEffort{Model: "m", Effort: "high", AllowClientOverride: true}, "", "high", "forced:high:policy:m"},
		{ForcedEffort{Model: "m", Effort: "High", AllowClientOverride: true}, "Low", "low", "client-override:low:policy:m"},
	} {
		eff, label := resolveForcedWinner(tc.pol, tc.client)
		if eff != tc.wantEffort || label != tc.wantLabel {
			t.Fatalf("resolveForcedWinner(%+v, %q) = %q, %q; want %q, %q", tc.pol, tc.client, eff, label, tc.wantEffort, tc.wantLabel)
		}
	}
}
