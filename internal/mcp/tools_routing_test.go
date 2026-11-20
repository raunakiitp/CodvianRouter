package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// newRoutingTestServer builds a server over a fakeSource populated
// with the M9.3 fixtures: 2 providers (p1 enabled, p2 disabled),
// 2 combos (fast = p1 only, cheapest strategy; mixed = p1+p2,
// priority), measured metrics for fast, per-model stats, and a
// prompt rate for p1/coder-model only (p2/chat-model is unpriced
// — the cost-honesty fixture).
func newRoutingTestServer() (*Server, *fakeSource) {
	sr := float64(0.9)
	f := &fakeSource{
		providers: []ProviderInfo{
			{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}, CatalogID: "groq", Verification: "verified-from-docs"},
			{ID: "p2", Name: "Chatter", WireFormat: "anthropic", Enabled: false, Models: []string{"chat-model", "chat-model-2"}},
		},
		models: []ModelInfo{
			{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"},
			{ID: "chat-model", ProviderID: "p2", ProviderName: "Chatter"},
		},
		status: StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 2, EnabledProviders: 1, Models: 3, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1},
		combos: map[string]ComboInfo{
			"fast":  {Name: "fast", Members: []string{"p1"}, Strategy: "fastest", Usable: true},
			"mixed": {Name: "mixed", Members: []string{"p1", "p2"}, Strategy: "priority", Usable: false, UnusableReason: "p2: provider disabled"},
		},
		metrics: map[string]ComboMetrics{
			"fast": {Name: "fast", Requests: 10, OK: 9, Errors: 1, SuccessRate: &sr, PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0.05, CostKnown: true, MeanLatencyMS: 120, LatencyKnown: true, LatencyEvents: 9, Window: "all_time"},
		},
		stats: []ModelStat{
			{Provider: "p1", Model: "coder-model", Requests: 10, OK: 9, MeanLatencyMS: 120, LatencyKnown: true, LatencyEvents: 9},
			{Provider: "p2", Model: "chat-model", Requests: 4, OK: 4, MeanLatencyMS: 90, LatencyKnown: true, LatencyEvents: 4},
		},
		rates: map[string]float64{"p1/coder-model": 3.0},
	}
	return New(f, "0.1.0").WithClientIdentity(LocalOperator()), f
}

// callToolErrMsg runs a tools/call and returns its JSON-RPC error
// code and message (0 / "" when the call succeeded). M9.4's
// callToolErr returns the code only; this sibling is for the
// assertions that also check the message text.
func callToolErrMsg(t *testing.T, s *Server, name string, args map[string]any) (float64, string) {
	t.Helper()
	rawArgs, _ := json.Marshal(args)
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(rawArgs)},
	})
	out := call(t, s, string(msg))
	errBody, ok := out["error"].(map[string]any)
	if !ok {
		return 0, ""
	}
	code, _ := errBody["code"].(float64)
	msgText, _ := errBody["message"].(string)
	return code, msgText
}

// withScopes builds a server over the routing fake with exactly
// the given scopes (a narrow gateway-key identity), via M9.4's
// serverFor helper.
func withScopes(t *testing.T, scopes ...string) *Server {
	t.Helper()
	_, f := newRoutingTestServer()
	return serverFor(f, "key-1", scopes)
}

func TestM93FakeStrategyNamesPinned(t *testing.T) {
	// The fake's strategy list mirrors strategy.All (+ aliases)
	// but the test package cannot import internal/strategy
	// (import cycle: strategy → router → config → mcptools →
	// mcp). So the canonical list is PINNED here, copied from
	// internal/strategy/strategy.go (All + aliases): if strategy
	// gains or renames a name, update this list AND
	// fakeStrategyNames in mcp_test.go together. The test fails
	// if the two drift apart.
	pinned := []string{
		"priority", "balanced", "cheapest", "fastest", "most-reliable",
		"least-latency", "weighted", "custom-weights", "code",
		"round-robin", "random", "strict-random", "fill-first",
		"least-used", "p2c", "quota-weighted", "headroom",
		"expiry-first", "reset-aware", "reset-window", "lkgp",
		"context-optimized", "cache-optimized", "context-relay",
		"cost-optimized", "ship-fast", "cost-saver", "quality-first",
		"offline-friendly", "reliability-first", "chaos-mode",
	}
	for _, name := range pinned {
		if _, ok := fakeStrategyNames[name]; !ok {
			t.Errorf("fake strategy list is missing pinned strategy %q", name)
		}
	}
	aliases := map[string]string{
		"cheap": "cheapest", "fast": "fastest", "reliable": "most-reliable",
		"latency": "least-latency", "eco": "cost-optimized", "custom": "custom-weights",
	}
	for alias, canon := range aliases {
		if got, ok := fakeStrategyNames[alias]; !ok || got != canon {
			t.Errorf("fake strategy alias %q = %q, want %q", alias, got, canon)
		}
	}
	if len(fakeStrategyNames) != len(pinned)+len(aliases) {
		t.Errorf("fake strategy list has %d entries, want %d (pinned + aliases) — a name was added without updating the pin", len(fakeStrategyNames), len(pinned)+len(aliases))
	}
}

func TestM93ListCombos(t *testing.T) {
	s, _ := newRoutingTestServer()
	sc := callTool(t, s, "list_combos", map[string]any{})
	combos, _ := sc["combos"].([]any)
	if len(combos) != 2 {
		t.Fatalf("list_combos returned %d combos, want 2", len(combos))
	}
	if sc["count"] != float64(2) {
		t.Errorf("count = %v, want 2", sc["count"])
	}
	first, _ := combos[0].(map[string]any)
	if first["name"] != "fast" || first["strategy"] != "fastest" {
		t.Errorf("first combo = %v, want fast/fastest", first)
	}
}

func TestM93CreateCombo(t *testing.T) {
	s, f := newRoutingTestServer()
	sc := callTool(t, s, "create_combo", map[string]any{"name": "solo", "members": []string{"p1"}, "strategy": "cheap"})
	combo, _ := sc["combo"].(map[string]any)
	if combo["name"] != "solo" || combo["strategy"] != "cheapest" {
		t.Errorf("created combo = %v, want solo/cheapest (alias normalised)", combo)
	}
	if _, ok := f.combos["solo"]; !ok {
		t.Errorf("fake source has no combo %q after create_combo", "solo")
	}
	// Duplicate name is -32602 and nothing is mutated.
	if code := callToolErr(t, s, "create_combo", map[string]any{"name": "solo", "members": []string{"p1"}}); code != -32602 {
		t.Errorf("duplicate create_combo code = %v, want -32602", code)
	}
	if len(f.combos) != 3 {
		t.Errorf("combos after duplicate attempt = %d, want 3 (nothing mutated)", len(f.combos))
	}
}

func TestM93CreateComboInvalidInputs(t *testing.T) {
	s, f := newRoutingTestServer()
	before := len(f.combos)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty name", map[string]any{"name": "", "members": []string{"p1"}}},
		{"illegal name", map[string]any{"name": "a/b", "members": []string{"p1"}}},
		{"no members", map[string]any{"name": "x", "members": []any{}}},
		{"unknown provider", map[string]any{"name": "x", "members": []string{"nope"}}},
		{"unknown strategy", map[string]any{"name": "x", "members": []string{"p1"}, "strategy": "teleport"}},
		{"members not an array", map[string]any{"name": "x", "members": "p1"}},
		{"missing name", map[string]any{"members": []string{"p1"}}},
	}
	for _, tc := range cases {
		if code, msg := callToolErrMsg(t, s, "create_combo", tc.args); code != -32602 {
			t.Errorf("%s: code = %v, want -32602 (%s)", tc.name, code, msg)
		}
	}
	if len(f.combos) != before {
		t.Errorf("combos = %d after invalid creates, want %d (nothing mutated)", len(f.combos), before)
	}
	// A combo execution mode is a valid combo strategy.
	if code := callToolErr(t, s, "create_combo", map[string]any{"name": "fuse", "members": []string{"p1"}, "strategy": "fusion"}); code != 0 {
		t.Errorf("fusion create_combo should succeed, got error code %v", code)
	}
}

// TestM93CreateComboThreadsCallContext (CreateCombo review
// nit 1): the create_combo handler passes the tools/call
// request context — not context.Background() — to
// Source.CreateCombo. A call arriving on an already-cancelled
// context is refused with -32602 naming the cancellation, the
// fake records that cancelled ctx (proving the ctx that reached
// the Source is the call's ctx), and nothing is stored. A live
// context creates normally, and the recorded ctx is live too.
func TestM93CreateComboThreadsCallContext(t *testing.T) {
	s, f := newRoutingTestServer()
	callWithCtx := func(ctx context.Context, name string) (float64, string) {
		t.Helper()
		rawArgs, _ := json.Marshal(map[string]any{"name": name, "members": []string{"p1"}, "strategy": "priority"})
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "create_combo", "arguments": json.RawMessage(rawArgs)},
		})
		raw := s.HandleWithIdentity(ctx, msg, LocalOperator())
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, raw)
		}
		errBody, ok := out["error"].(map[string]any)
		if !ok {
			return 0, ""
		}
		code, _ := errBody["code"].(float64)
		msgText, _ := errBody["message"].(string)
		return code, msgText
	}

	// Cancelled call ctx: refused, nothing stored, and the
	// ctx the Source saw is the cancelled call ctx itself.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code, msg := callWithCtx(ctx, "cancelled"); code != -32602 || !strings.Contains(msg, "context canceled") {
		t.Errorf("cancelled create_combo code/msg = %v / %q, want -32602 naming the cancelled context", code, msg)
	}
	if f.createComboCtx == nil {
		t.Fatal("fake recorded no ctx for CreateCombo — the handler did not pass a context to the Source")
	}
	if f.createComboCtxErr != context.Canceled {
		t.Errorf("CreateCombo ctx Err() at call time = %v, want context.Canceled (the call's ctx, not Background)", f.createComboCtxErr)
	}
	if _, ok := f.combos["cancelled"]; ok {
		t.Error("cancelled create_combo stored the combo anyway")
	}

	// Live ctx: creates normally, and the ctx the Source saw
	// was live at call time.
	if code, msg := callWithCtx(context.Background(), "live"); code != 0 {
		t.Fatalf("live create_combo code/msg = %v / %q, want success", code, msg)
	}
	if f.createComboCtx == nil || f.createComboCtxErr != nil {
		t.Errorf("CreateCombo ctx Err() at call time = %v, want nil (a live call ctx)", f.createComboCtxErr)
	}
	if _, ok := f.combos["live"]; !ok {
		t.Error("live create_combo did not store the combo")
	}
}

func TestM93SwitchCombos(t *testing.T) {
	s, f := newRoutingTestServer()
	sc := callTool(t, s, "switch_combos", map[string]any{"name": "fast"})
	if sc["active_combo"] != "fast" {
		t.Errorf("active_combo = %v, want fast", sc["active_combo"])
	}
	if f.active != "fast" {
		t.Errorf("fake active = %q, want fast", f.active)
	}
	if note, _ := sc["note"].(string); !strings.Contains(note, "Session-active") {
		t.Errorf("switch_combos note %q does not say the selection is session-scoped", note)
	}
	// Unknown combo: -32602, active unchanged.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "nope"}); code != -32602 {
		t.Errorf("unknown switch_combos code = %v, want -32602", code)
	}
	if f.active != "fast" {
		t.Errorf("active = %q after failed switch, want fast", f.active)
	}
	// active=false clears the selection.
	sc = callTool(t, s, "switch_combos", map[string]any{"name": "fast", "active": false})
	if sc["active_combo"] != nil {
		t.Errorf("active_combo after clear = %v, want null", sc["active_combo"])
	}
	if f.active != "" {
		t.Errorf("fake active = %q after clear, want empty", f.active)
	}
	// active=false on a non-active combo is -32602.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "mixed", "active": false}); code != -32602 {
		t.Errorf("deactivate non-active code = %v, want -32602", code)
	}
}

func TestM93GetComboMetrics(t *testing.T) {
	s, _ := newRoutingTestServer()
	sc := callTool(t, s, "get_combo_metrics", map[string]any{"name": "fast"})
	m, _ := sc["metrics"].(map[string]any)
	if m["requests"] != float64(10) || m["ok"] != float64(9) || m["cost_usd"] != 0.05 {
		t.Errorf("metrics = %v, want 10 requests / 9 ok / $0.05", m)
	}
	if m["success_rate"] != 0.9 {
		t.Errorf("success_rate = %v, want 0.9", m["success_rate"])
	}
	if code := callToolErr(t, s, "get_combo_metrics", map[string]any{"name": "nope"}); code != -32602 {
		t.Errorf("unknown combo metrics code = %v, want -32602", code)
	}
}

func TestM93SetRoutingStrategy(t *testing.T) {
	s, f := newRoutingTestServer()
	sc := callTool(t, s, "set_routing_strategy", map[string]any{"strategy": "cheapest"})
	cur, _ := sc["strategy"].(map[string]any)
	if cur["name"] != "cheapest" || cur["scope"] != "global" {
		t.Errorf("global strategy after set = %v, want cheapest/global", cur)
	}
	prev, _ := sc["previous"].(map[string]any)
	if prev["name"] != "priority" {
		t.Errorf("previous strategy = %v, want priority", prev)
	}
	sc = callTool(t, s, "set_routing_strategy", map[string]any{"strategy": "fast", "chain": "coder-model"})
	cur, _ = sc["strategy"].(map[string]any)
	if cur["name"] != "fastest" || cur["scope"] != "chain" || cur["inherited"] != false {
		t.Errorf("chain strategy after set = %v, want fastest/chain/not-inherited", cur)
	}
	if f.strategies["coder-model"] != "fastest" {
		t.Errorf("fake chain strategy = %q, want fastest", f.strategies["coder-model"])
	}
	// Unknown strategy: -32602, nothing mutated.
	if code := callToolErr(t, s, "set_routing_strategy", map[string]any{"strategy": "teleport"}); code != -32602 {
		t.Errorf("unknown strategy code = %v, want -32602", code)
	}
	if f.strategies[""] != "cheapest" {
		t.Errorf("global strategy after failed set = %q, want cheapest (unchanged)", f.strategies[""])
	}
}

func TestM93ExplainRoute(t *testing.T) {
	s, _ := newRoutingTestServer()
	sc := callTool(t, s, "explain_route", map[string]any{"model": "coder-model"})
	route, _ := sc["route"].(map[string]any)
	if route["strategy"] != "priority" {
		t.Errorf("strategy = %v, want priority", route["strategy"])
	}
	attempts, _ := route["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (only p1 offers coder-model)", len(attempts))
	}
	first, _ := attempts[0].(map[string]any)
	if first["provider"] != "p1" || first["price_known"] != true || first["prompt_usd_per_1m"] != 3.0 {
		t.Errorf("attempt = %v, want p1 / price known / $3 per 1M", first)
	}
	if note, _ := route["note"].(string); !strings.Contains(note, "declared") {
		t.Errorf("note %q does not say the order is declared order", note)
	}
	// Unpriced model: price_known false, no rate.
	sc = callTool(t, s, "explain_route", map[string]any{"model": "chat-model"})
	route, _ = sc["route"].(map[string]any)
	attempts, _ = route["attempts"].([]any)
	first, _ = attempts[0].(map[string]any)
	if first["price_known"] != false {
		t.Errorf("unpriced attempt price_known = %v, want false", first["price_known"])
	}
	if _, hasRate := first["prompt_usd_per_1m"]; hasRate {
		t.Errorf("unpriced attempt carries a rate: %v", first)
	}
	// Unknown model and virtual auto/* model: -32602.
	if code := callToolErr(t, s, "explain_route", map[string]any{"model": "nope"}); code != -32602 {
		t.Errorf("unknown model code = %v, want -32602", code)
	}
	if code := callToolErr(t, s, "explain_route", map[string]any{"model": "auto/fast"}); code != -32602 {
		t.Errorf("auto/* model code = %v, want -32602", code)
	}
}

func TestM93ExplainRouteComboRestriction(t *testing.T) {
	s, _ := newRoutingTestServer()
	// mixed = p1+p2; chat-model is offered by p2 only, so the
	// restricted order has exactly p2.
	sc := callTool(t, s, "explain_route", map[string]any{"model": "chat-model", "combo": "mixed"})
	route, _ := sc["route"].(map[string]any)
	attempts, _ := route["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("restricted attempts = %d, want 1", len(attempts))
	}
	if attempts[0].(map[string]any)["provider"] != "p2" {
		t.Errorf("restricted attempt = %v, want p2", attempts[0])
	}
	if code := callToolErr(t, s, "explain_route", map[string]any{"model": "chat-model", "combo": "nope"}); code != -32602 {
		t.Errorf("unknown combo restriction code = %v, want -32602", code)
	}
	// A combo with no member offering the model is -32602, not
	// an empty order: "fast" is p1-only and p1 does not offer
	// chat-model.
	if code := callToolErr(t, s, "explain_route", map[string]any{"model": "chat-model", "combo": "fast"}); code != -32602 {
		t.Errorf("empty restriction code = %v, want -32602", code)
	}
}

func TestM93ExplainSimulateDefaultToActiveCombo(t *testing.T) {
	// The documented default (see the HONESTY RULES header in
	// tools_routing.go): with no combo argument, explain_route
	// and simulate_route restrict to the session-active combo
	// when one is set, and to nothing when none is set.
	s, f := newRoutingTestServer()
	// No active combo: unrestricted, no combo reported.
	sc := callTool(t, s, "explain_route", map[string]any{"model": "chat-model"})
	route, _ := sc["route"].(map[string]any)
	if _, has := route["combo"]; has {
		t.Errorf("route with no active combo carries combo = %v, want none", route["combo"])
	}
	// Activate "mixed" (p1+p2): the default now restricts to it
	// and names it. chat-model is offered by p2 only, so the
	// order still has exactly p2.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "mixed"}); code != 0 {
		t.Fatalf("switch_combos mixed failed with code %v", code)
	}
	sc = callTool(t, s, "explain_route", map[string]any{"model": "chat-model"})
	route, _ = sc["route"].(map[string]any)
	if route["combo"] != "mixed" {
		t.Errorf("route combo = %v, want mixed (session-active default)", route["combo"])
	}
	attempts, _ := route["attempts"].([]any)
	if len(attempts) != 1 || attempts[0].(map[string]any)["provider"] != "p2" {
		t.Errorf("default-restricted attempts = %v, want exactly p2", attempts)
	}
	// Activate "fast" (p1-only): the default now offers no
	// provider for chat-model, so it is -32602 — the same
	// refusal as naming "fast" explicitly. Without the default
	// this call would succeed, so the error proves the default
	// was applied.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "fast"}); code != 0 {
		t.Fatalf("switch_combos fast failed with code %v", code)
	}
	if code := callToolErr(t, s, "explain_route", map[string]any{"model": "chat-model"}); code != -32602 {
		t.Errorf("explain with active fast code = %v, want -32602", code)
	}
	if code := callToolErr(t, s, "simulate_route", map[string]any{"model": "chat-model", "prompt_tokens": 1000}); code != -32602 {
		t.Errorf("simulate with active fast code = %v, want -32602", code)
	}
	// An explicit combo argument overrides the active default.
	sc = callTool(t, s, "explain_route", map[string]any{"model": "chat-model", "combo": "mixed"})
	route, _ = sc["route"].(map[string]any)
	if route["combo"] != "mixed" {
		t.Errorf("explicit combo route combo = %v, want mixed (override of active fast)", route["combo"])
	}
	sc = callTool(t, s, "simulate_route", map[string]any{"model": "chat-model", "prompt_tokens": 1000, "combo": "mixed"})
	sim, _ := sc["simulation"].(map[string]any)
	if sim["combo"] != "mixed" {
		t.Errorf("explicit combo simulation combo = %v, want mixed (override of active fast)", sim["combo"])
	}
	// simulate_route with the active default (mixed): names it.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "mixed"}); code != 0 {
		t.Fatalf("switch_combos mixed (again) failed with code %v", code)
	}
	sc = callTool(t, s, "simulate_route", map[string]any{"model": "chat-model", "prompt_tokens": 1000})
	sim, _ = sc["simulation"].(map[string]any)
	if sim["combo"] != "mixed" {
		t.Errorf("simulation combo = %v, want mixed (session-active default)", sim["combo"])
	}
	// Clearing the active combo restores the unrestricted call.
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "mixed", "active": false}); code != 0 {
		t.Fatalf("clearing the active combo failed with code %v", code)
	}
	if f.active != "" {
		t.Fatalf("fake active = %q after clear, want empty", f.active)
	}
	sc = callTool(t, s, "explain_route", map[string]any{"model": "chat-model"})
	route, _ = sc["route"].(map[string]any)
	if _, has := route["combo"]; has {
		t.Errorf("route after clear carries combo = %v, want none", route["combo"])
	}
}

func TestM93SimulateRoute(t *testing.T) {
	s, _ := newRoutingTestServer()
	sc := callTool(t, s, "simulate_route", map[string]any{"model": "coder-model", "prompt_tokens": 1000000})
	sim, _ := sc["simulation"].(map[string]any)
	if sim["estimated_prompt_cost_usd"] != 3.0 || sim["cost_known"] != true {
		t.Errorf("simulation cost = %v / known %v, want $3 / true", sim["estimated_prompt_cost_usd"], sim["cost_known"])
	}
	attempts, _ := sim["attempts"].([]any)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1", len(attempts))
	}
	last, _ := attempts[0].(map[string]any)
	if !strings.Contains(last["on_failure"].(string), "no further targets") {
		t.Errorf("last attempt on_failure = %v, want a terminal failure note", last["on_failure"])
	}
	// No prompt_tokens: no estimate, cost_known false.
	sc = callTool(t, s, "simulate_route", map[string]any{"model": "coder-model"})
	sim, _ = sc["simulation"].(map[string]any)
	if sim["cost_known"] != false {
		t.Errorf("cost_known without prompt_tokens = %v, want false", sim["cost_known"])
	}
	// Unpriced model: never $0.
	sc = callTool(t, s, "simulate_route", map[string]any{"model": "chat-model", "prompt_tokens": 1000000})
	sim, _ = sc["simulation"].(map[string]any)
	if sim["cost_known"] != false {
		t.Errorf("unpriced simulation cost_known = %v, want false", sim["cost_known"])
	}
	if _, has := sim["estimated_prompt_cost_usd"]; has {
		t.Errorf("unpriced simulation carries an estimate: %v", sim)
	}
	// Negative prompt_tokens: -32602.
	if code := callToolErr(t, s, "simulate_route", map[string]any{"model": "coder-model", "prompt_tokens": -5}); code != -32602 {
		t.Errorf("negative prompt_tokens code = %v, want -32602", code)
	}
	// Non-integer prompt_tokens: -32602.
	if code, _ := callToolErrMsg(t, s, "simulate_route", map[string]any{"model": "coder-model", "prompt_tokens": 1.5}); code != -32602 {
		t.Errorf("fractional prompt_tokens code = %v, want -32602", code)
	}
}

func TestM93BestComboForTask(t *testing.T) {
	s, _ := newRoutingTestServer()
	// "fast chat" prefers fastest → combo "fast" wins on strategy
	// match (its measured success rate is also the only one).
	sc := callTool(t, s, "best_combo_for_task", map[string]any{"task": "fast chat"})
	combo, _ := sc["combo"].(map[string]any)
	if combo["name"] != "fast" {
		t.Errorf("best combo = %v, want fast", combo["name"])
	}
	if sc["preferred_strategy"] != "fastest" {
		t.Errorf("preferred_strategy = %v, want fastest", sc["preferred_strategy"])
	}
	if sc["metrics_known"] != true {
		t.Errorf("metrics_known = %v, want true", sc["metrics_known"])
	}
	// A cost cap the measured combo satisfies keeps it.
	sc = callTool(t, s, "best_combo_for_task", map[string]any{"task": "coding", "max_cost_usd": 0.01})
	combo, _ = sc["combo"].(map[string]any)
	if combo["name"] != "fast" {
		t.Errorf("best combo under $0.01 cap = %v, want fast ($0.005/request)", combo["name"])
	}
	// A cost cap below every measured cost excludes "fast";
	// "mixed" has no metrics, so its cost is unknown and the
	// constraint is reported not-applied — it is NOT excluded.
	sc = callTool(t, s, "best_combo_for_task", map[string]any{"task": "coding", "max_cost_usd": 0.001})
	combo, _ = sc["combo"].(map[string]any)
	if combo["name"] != "mixed" {
		t.Errorf("best combo under $0.001 cap = %v, want mixed (unknown cost, not excluded)", combo["name"])
	}
	cons, _ := sc["constraints"].(map[string]any)
	if cons["max_cost_usd_applied"] != false {
		t.Errorf("constraints = %v, want max_cost_usd_applied=false for unknown-cost combo", cons)
	}
	// A zero cap is the documented no-cap sentinel ("0 or
	// omitted = no cap" in the tool schema), not a $0 / 0 ms cap:
	// "fast" measures $0.005/request and 120 ms, so a real zero
	// cap would exclude it and "mixed" would win. Both zero caps
	// together — and each alone — must behave like no cap at all.
	for _, args := range []map[string]any{
		{"task": "coding", "max_cost_usd": 0, "max_latency_ms": 0},
		{"task": "coding", "max_cost_usd": 0},
		{"task": "coding", "max_latency_ms": 0},
	} {
		sc = callTool(t, s, "best_combo_for_task", args)
		combo, _ = sc["combo"].(map[string]any)
		if combo["name"] != "fast" {
			t.Errorf("best combo with zero caps %v = %v, want fast (0 = no cap)", args, combo["name"])
		}
		cons, _ = sc["constraints"].(map[string]any)
		if cons["max_cost_usd_applied"] != nil || cons["max_latency_ms_applied"] != nil {
			t.Errorf("constraints with zero caps %v = %v, want neither cap applied", args, cons)
		}
	}
	// No combos at all: -32602.
	empty := New(&fakeSource{}, "0.1.0").WithClientIdentity(LocalOperator())
	if code := callToolErr(t, empty, "best_combo_for_task", map[string]any{"task": "x"}); code != -32602 {
		t.Errorf("no-combos best_combo code = %v, want -32602", code)
	}
}

// reasonsJoined renders a tool result's reasons list as one
// string, for the best_combo_for_task independence test.
func reasonsJoined(t *testing.T, out map[string]any) string {
	t.Helper()
	raw, ok := out["reasons"].([]any)
	if !ok {
		t.Fatalf("result has no reasons list: %v", out)
	}
	parts := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

func TestM93BestComboReasonsIndependentAcrossCalls(t *testing.T) {
	s, f := newRoutingTestServer()
	f.ledgerDown = true
	// Ledger down: no combo has measured history, so
	// toolBestComboForTask appends the no-history note to the
	// best combo's reasons; that append must go to a fresh copy,
	// never into the candidate's backing array. The aliasing was
	// latent (candidates are rebuilt per call and never retained),
	// so the observable invariant is pinned instead: repeated
	// calls return identical, independent reasons, the appended
	// note exactly once each, and mutating one call's decoded
	// reasons cannot change the next call's. Mirrors
	// TestSmartRoutingReasonsIndependentAcrossCalls (a2a).
	out1 := callTool(t, s, "best_combo_for_task", map[string]any{"task": "fast chat"})
	out2 := callTool(t, s, "best_combo_for_task", map[string]any{"task": "fast chat"})
	r1, r2 := reasonsJoined(t, out1), reasonsJoined(t, out2)
	if r1 != r2 {
		t.Errorf("reasons differ across calls:\nfirst:  %q\nsecond: %q", r1, r2)
	}
	if n := strings.Count(r1, "ranked on strategy match alone"); n != 1 {
		t.Errorf("no-history note appears %d times in %q, want exactly once", n, r1)
	}
	// Mutate the first call's decoded reasons, then call again:
	// the next call must be unaffected.
	raw1, _ := out1["reasons"].([]any)
	out1["reasons"] = append(raw1, "tampered")
	out3 := callTool(t, s, "best_combo_for_task", map[string]any{"task": "fast chat"})
	if got := reasonsJoined(t, out3); got != r1 {
		t.Errorf("reasons after mutating a prior result = %q, want %q", got, r1)
	}
}

func TestM93PickFastestModel(t *testing.T) {
	s, _ := newRoutingTestServer()
	sc := callTool(t, s, "pick_fastest_model", map[string]any{})
	if sc["model"] != "chat-model" || sc["provider"] != "p2" || sc["mean_latency_ms"] != 90.0 {
		t.Errorf("fastest = %v, want chat-model/p2/90ms", sc)
	}
	sc = callTool(t, s, "pick_fastest_model", map[string]any{"provider": "p1"})
	if sc["model"] != "coder-model" {
		t.Errorf("fastest for p1 = %v, want coder-model", sc["model"])
	}
	// min_requests above every model's count: honest error, not
	// a guess. The dispatcher answers -32602 with an
	// "unavailable"-style message (no measured latency).
	if code, msg := callToolErrMsg(t, s, "pick_fastest_model", map[string]any{"min_requests": 100}); code != -32602 || !strings.Contains(msg, "no measured model latency") {
		t.Errorf("no-latency pick code/msg = %v / %q, want -32602 mentioning no measured latency", code, msg)
	}
	// min_requests must be >= 1.
	if code := callToolErr(t, s, "pick_fastest_model", map[string]any{"min_requests": 0}); code != -32602 {
		t.Errorf("min_requests 0 code = %v, want -32602", code)
	}
	// Ledger down: no stats at all → same honest error.
	down := New(&fakeSource{ledgerDown: true}, "0.1.0").WithClientIdentity(LocalOperator())
	if code, msg := callToolErrMsg(t, down, "pick_fastest_model", map[string]any{}); code != -32602 || !strings.Contains(msg, "no measured model latency") {
		t.Errorf("ledger-down pick code/msg = %v / %q, want -32602 mentioning no measured latency", code, msg)
	}
}

func TestM93ScopeDenial(t *testing.T) {
	// A narrow key with only the read:routing scope can call the
	// read tools but is denied the manage tools with -32002 —
	// and the denial happens BEFORE dispatch (the combo is not
	// created).
	s := withScopes(t, ScopeReadRouting)
	sc := callTool(t, s, "list_combos", map[string]any{})
	if sc["count"] == nil {
		t.Errorf("read-scoped list_combos should succeed: %v", sc)
	}
	if code := callToolErr(t, s, "create_combo", map[string]any{"name": "x", "members": []string{"p1"}}); code != ScopeDeniedCode {
		t.Errorf("read-scoped create_combo code = %v, want -32002", code)
	}
	if code := callToolErr(t, s, "set_routing_strategy", map[string]any{"strategy": "cheapest"}); code != ScopeDeniedCode {
		t.Errorf("read-scoped set_routing_strategy code = %v, want -32002", code)
	}
	if code := callToolErr(t, s, "switch_combos", map[string]any{"name": "fast"}); code != ScopeDeniedCode {
		t.Errorf("read-scoped switch_combos code = %v, want -32002", code)
	}
	// A key with only the manage scope can mutate but cannot read.
	m := withScopes(t, ScopeManageRouting)
	if code := callToolErr(t, m, "list_combos", map[string]any{}); code != ScopeDeniedCode {
		t.Errorf("manage-scoped list_combos code = %v, want -32002", code)
	}
	// The wildcard mcp:read:* covers the read tools.
	w := withScopes(t, "mcp:read:*")
	if code := callToolErr(t, w, "explain_route", map[string]any{"model": "coder-model"}); code != 0 {
		t.Errorf("read:* explain_route should succeed, got error code %v", code)
	}
	// Anonymous (no scopes) is denied everything.
	anon := New(&fakeSource{}, "0.1.0")
	if code := callToolErr(t, anon, "list_combos", map[string]any{}); code != ScopeDeniedCode {
		t.Errorf("anonymous list_combos code = %v, want -32002", code)
	}
}

func TestM93ScopeDenialDispatchesNothing(t *testing.T) {
	// The scope check runs before dispatch: a denied create
	// leaves the Source untouched (no combo appears).
	srv, f := newRoutingTestServer()
	narrow := New(f, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadRouting}))
	_ = srv
	if code := callToolErr(t, narrow, "create_combo", map[string]any{"name": "ghost", "members": []string{"p1"}}); code != ScopeDeniedCode {
		t.Fatalf("denied create_combo code = %v, want -32002", code)
	}
	if _, ok := f.combos["ghost"]; ok {
		t.Errorf("denied create_combo still created the combo")
	}
}

func TestM93ToolListIncludesRoutingTools(t *testing.T) {
	s, _ := newRoutingTestServer()
	out := call(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		names[m["name"].(string)] = true
		if m["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema", m["name"])
		}
	}
	for _, want := range []string{"list_combos", "create_combo", "switch_combos", "get_combo_metrics", "set_routing_strategy", "best_combo_for_task", "explain_route", "simulate_route", "pick_fastest_model"} {
		if !names[want] {
			t.Errorf("tools/list is missing %s", want)
		}
	}
	// Built-ins stay first, in order (M9.0 fixtures pin this).
	if len(tools) >= 3 {
		if tools[0].(map[string]any)["name"] != "router_status" || tools[1].(map[string]any)["name"] != "provider_list" || tools[2].(map[string]any)["name"] != "model_list" {
			t.Errorf("built-in tools are not first in tools/list")
		}
	}
}

func TestM93ToggleCreateComboHandler(t *testing.T) {
	// Toggle verification for the create_combo handler: with the
	// handler's dispatch neutralised (a Source that refuses
	// every create), the test FAILS; with the real fake it passes.
	// This test asserts the passing half; the failing half is
	// shown by TestM93ToggleCreateComboHandlerNeutralised below
	// (same assertions, refusing Source).
	s, f := newRoutingTestServer()
	sc := callTool(t, s, "create_combo", map[string]any{"name": "toggle", "members": []string{"p1"}})
	if sc["created"] != true {
		t.Fatalf("create_combo created = %v, want true", sc["created"])
	}
	if _, ok := f.combos["toggle"]; !ok {
		t.Fatalf("combo toggle was not stored")
	}
}

// refusingSource is the toggle-neutralised Source: every mutation
// refuses, so any test asserting a mutation happened must fail.
type refusingSource struct{ *fakeSource }

func (r *refusingSource) CreateCombo(ctx context.Context, name string, members []string, strategy string) error {
	return ErrInvalidInput
}

// assertCreateComboHandlerOutcome runs the SAME assertions
// against either branch of the create_combo handler toggle:
// wantCreated selects the toggle-ON (live fake Source) outcome
// versus the toggle-OFF (refusing Source) outcome. The test only
// discriminates if both branches are asserted here — a test that
// checked only one branch would pass against a handler that
// always succeeded, or always refused (the weak shape the M9.3
// review flagged: this test used to document rather than perform
// its toggle). Mirrors assertCreateComboOutcome in
// internal/apitest/mcp_fixture_test.go, but through the real MCP
// handler (tools/call) instead of the Source method directly.
func assertCreateComboHandlerOutcome(t *testing.T, s *Server, f *fakeSource, wantCreated bool) {
	t.Helper()
	before := len(f.Combos())
	args := map[string]any{"name": "toggle", "members": []string{"p1"}, "strategy": "fastest"}
	if wantCreated {
		sc := callTool(t, s, "create_combo", args)
		if sc["created"] != true {
			t.Fatalf("toggle-on create_combo created = %v, want true", sc["created"])
		}
		// The handler re-reads the stored combo, so the reply
		// itself must carry the expected members/strategy.
		combo, _ := sc["combo"].(map[string]any)
		if combo == nil || combo["name"] != "toggle" || combo["strategy"] != "fastest" || combo["usable"] != true {
			t.Fatalf("toggle-on create_combo reply combo = %v, want name toggle, strategy fastest, usable", combo)
		}
		members, _ := combo["members"].([]any)
		if len(members) != 1 || members[0] != "p1" {
			t.Fatalf("toggle-on create_combo reply combo members = %v, want [p1]", members)
		}
		after := f.Combos()
		if len(after) != before+1 {
			t.Fatalf("toggle-on create_combo: combo count = %d, want %d (one combo stored)", len(after), before+1)
		}
		stored, ok := f.combos["toggle"]
		if !ok {
			t.Fatal("toggle-on create_combo: combo toggle not stored")
		}
		if len(stored.Members) != 1 || stored.Members[0] != "p1" || stored.Strategy != "fastest" || !stored.Usable {
			t.Fatalf("toggle-on create_combo: stored combo = %+v, want members [p1], strategy fastest, usable", stored)
		}
		return
	}
	if code, msg := callToolErrMsg(t, s, "create_combo", args); code != -32602 || !strings.Contains(msg, "invalid input") {
		t.Fatalf("toggle-off create_combo code/msg = %v / %q, want -32602 mentioning invalid input (handler must fail when the Source refuses)", code, msg)
	}
	after := f.Combos()
	if len(after) != before {
		t.Fatalf("toggle-off create_combo: combo count = %d, want %d (nothing stored)", len(after), before)
	}
	if _, ok := f.combos["toggle"]; ok {
		t.Fatal("toggle-off create_combo: combo toggle was stored despite the refusal")
	}
}

func TestM93ToggleCreateComboHandlerNeutralised(t *testing.T) {
	// Toggle verification for the create_combo handler, both
	// branches through the real handler (tools/call): toggle ON
	// (the live fake Source) the create succeeds and the combo
	// is stored with its members, strategy and usability;
	// toggle OFF (the refusing Source) the same call is refused
	// with -32602 and stores nothing. Both branches run the same
	// assertions via assertCreateComboHandlerOutcome, so flipping
	// the behaviour flips the test.
	on, onFake := newRoutingTestServer()
	assertCreateComboHandlerOutcome(t, on, onFake, true)
	_, offFake := newRoutingTestServer()
	neutralised := New(&refusingSource{offFake}, "0.1.0").WithClientIdentity(LocalOperator())
	assertCreateComboHandlerOutcome(t, neutralised, offFake, false)
}

// TestM93LedgerDownSurfacesAsUnavailable checks the dispatcher
// wart honestly: a ledger failure is still -32602 (the only
// handler-error code), but the message says "unavailable".
func TestM93LedgerDownSurfacesAsUnavailable(t *testing.T) {
	s, _ := newRoutingTestServer()
	down := New(&fakeSource{
		providers:  []ProviderInfo{{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}}},
		combos:     map[string]ComboInfo{"fast": {Name: "fast", Members: []string{"p1"}, Strategy: "fastest"}},
		ledgerDown: true,
	}, "0.1.0").WithClientIdentity(LocalOperator())
	_ = s
	if code, msg := callToolErrMsg(t, down, "get_combo_metrics", map[string]any{"name": "fast"}); code != -32602 || !strings.Contains(msg, "unavailable:") {
		t.Errorf("ledger-down metrics code/msg = %v / %q, want -32602 with an 'unavailable:' message", code, msg)
	}
}

// statsBackendSource wraps the fake to implement the optional
// modelStatsAvailable interface with a chosen availability: it
// is how the tests drive the N4 branch without a live ledger.
type statsBackendSource struct {
	*fakeSource
	available bool
}

func (s *statsBackendSource) ModelStatsAvailable() bool { return s.available }

func TestM93PickFastestModelBackendUnavailable(t *testing.T) {
	// N4: when the latency backend itself is unavailable, the
	// error must say the backend is unavailable — never "no
	// measured model latency yet", which claims the ledger was
	// consulted and found no events. The provider-filtered call
	// says the same: with no backend there is no per-provider
	// latency either.
	down := &statsBackendSource{fakeSource: &fakeSource{ledgerDown: true}, available: false}
	s := New(down, "0.1.0").WithClientIdentity(LocalOperator())
	if code, msg := callToolErrMsg(t, s, "pick_fastest_model", map[string]any{}); code != -32602 || !strings.Contains(msg, "backend is not available") {
		t.Errorf("backend-down pick code/msg = %v / %q, want -32602 saying the backend is not available", code, msg)
	}
	if code, msg := callToolErrMsg(t, s, "pick_fastest_model", map[string]any{"provider": "p1"}); code != -32602 || !strings.Contains(msg, "backend is not available") {
		t.Errorf("backend-down pick (provider) code/msg = %v / %q, want -32602 saying the backend is not available", code, msg)
	}
	// Control: the same empty stats with the backend available
	// keep the old "no measured latency" message — the branch is
	// selected by availability, not by emptiness.
	up := &statsBackendSource{fakeSource: &fakeSource{ledgerDown: true}, available: true}
	sUp := New(up, "0.1.0").WithClientIdentity(LocalOperator())
	if code, msg := callToolErrMsg(t, sUp, "pick_fastest_model", map[string]any{}); code != -32602 || !strings.Contains(msg, "no measured model latency") || strings.Contains(msg, "backend is not available") {
		t.Errorf("backend-up pick code/msg = %v / %q, want -32602 mentioning no measured latency (not backend unavailable)", code, msg)
	}
}
