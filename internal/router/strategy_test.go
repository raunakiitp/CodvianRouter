package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// recordingHookFactory returns a Config.StrategyHook that records
// every strategy name it is asked for (in call order) and orders the
// chain so the provider named after the strategy wins when present
// ("cheapest" -> "cheap", "fastest" -> "fast", anything else ->
// input order). It proves the plumbing — which strategy the engine
// resolved — while the real scoring rules are proved in
// internal/strategy (HookFactory over live signals).
type recordingHookFactory struct {
	mu       sync.Mutex
	recorded []string
}

func (f *recordingHookFactory) hook(strategy string) func(chain []Attempt) []Attempt {
	f.mu.Lock()
	f.recorded = append(f.recorded, strategy)
	f.mu.Unlock()
	return func(chain []Attempt) []Attempt {
		first := ""
		switch strategy {
		case "cheapest":
			first = "cheap"
		case "fastest":
			first = "fast"
		}
		if first == "" {
			return chain
		}
		out := make([]Attempt, 0, len(chain))
		var rest []Attempt
		for _, att := range chain {
			if att.ProviderID == first {
				out = append(out, att)
			} else {
				rest = append(rest, att)
			}
		}
		return append(out, rest...)
	}
}

func (f *recordingHookFactory) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recorded) == 0 {
		return ""
	}
	return f.recorded[len(f.recorded)-1]
}

func TestResolveAutoAlias(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	cases := map[string]struct {
		axis  string
		model string
	}{
		"auto:cheapest":      {axis: "cheapest", model: "auto/cheap"},
		"auto:fastest":       {axis: "fastest", model: "auto/fast"},
		"auto:least-latency": {axis: "least-latency", model: "auto/fast"},
		"auto:most-reliable": {axis: "most-reliable", model: "auto/reasoning"},
		"auto:balanced":      {axis: "balanced", model: "auto"},
		"auto:weighted":      {axis: "weighted", model: "auto"},
		"auto:code":          {axis: "code", model: "auto/code"},
		"auto:fusion":        {axis: "fusion", model: "auto/fusion"},
		"auto:pipeline":      {axis: "pipeline", model: "auto/pipeline"},
		"auto:jev":           {axis: "jev", model: "auto/jev"},
		"AUTO:Cheapest":      {axis: "cheapest", model: "auto/cheap"},
	}
	for in, want := range cases {
		axis, req := eng.resolveAutoAlias(provider.ChatRequest{Model: in})
		if axis != want.axis || req.Model != want.model {
			t.Errorf("resolveAutoAlias(%q) = (%q, %q) (want %q, %q)", in, axis, req.Model, want.axis, want.model)
		}
	}
	for _, notAlias := range []string{"auto", "auto/fast", "gpt-4o", "auto:", ""} {
		axis, req := eng.resolveAutoAlias(provider.ChatRequest{Model: notAlias})
		if axis != "" || req.Model != notAlias {
			t.Errorf("resolveAutoAlias(%q) = (%q, %q) (want no alias)", notAlias, axis, req.Model)
		}
	}
}

func TestPerChainOverrideBeatsGlobal(t *testing.T) {
	factory := &recordingHookFactory{}
	cfg := testConfig()
	cfg.Strategy = "balanced"
	cfg.ChainStrategy = func(model string) string {
		if model == "m-cheap" {
			return "cheapest"
		}
		return ""
	}
	cfg.StrategyHook = factory.hook
	eng := NewEngine(map[string]provider.Client{
		"cheap": okClient("from-cheap"),
		"fast":  okClient("from-fast"),
	}, nil, cfg)

	req := testReq()
	req.Model = "m-cheap"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat m-cheap: %v", err)
	}
	if resp.Content != "from-cheap" {
		t.Fatalf("m-cheap content = %q (want from-cheap: chain strategy did not order the chain)", resp.Content)
	}
	if got := factory.last(); got != "cheapest" {
		t.Errorf("m-cheap strategy = %q (want cheapest)", got)
	}

	req.Model = "m-other"
	resp, err = eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat m-other: %v", err)
	}
	if resp.Content != "from-cheap" {
		// Balanced keeps input (quota) order: cheap was registered
		// first in the chain, so it still serves — the point is
		// the strategy resolved, not the winner.
		t.Logf("m-other content = %q", resp.Content)
	}
	if got := factory.last(); got != "balanced" {
		t.Errorf("m-other strategy = %q (want balanced: no chain override, global applies)", got)
	}
}

func TestRequestAliasWinsOverChainOverride(t *testing.T) {
	factory := &recordingHookFactory{}
	cfg := testConfig()
	cfg.Strategy = "balanced"
	cfg.ChainStrategy = func(model string) string {
		if model == "m-cheap" {
			return "cheapest"
		}
		return ""
	}
	cfg.StrategyHook = factory.hook
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "cheap", Model: model}, {ProviderID: "fast", Model: model}}
	}
	// The alias rewrites auto:fastest -> auto/fast, and the
	// auto-router resolves that to the concrete model the chain
	// override is set for — the alias strategy must still win.
	cfg.AutoRoute = func(model, prompt string) (string, autorouter.Decision, bool) {
		if model != "auto/fast" {
			t.Errorf("AutoRoute got model %q (want auto/fast: the alias rewrite)", model)
			return "", autorouter.Decision{}, false
		}
		return "m-cheap", autorouter.Decision{Model: "m-cheap"}, true
	}
	eng := NewEngine(map[string]provider.Client{
		"cheap": okClient("from-cheap"),
		"fast":  okClient("from-fast"),
	}, nil, cfg)

	req := testReq()
	req.Model = "auto:fastest"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat auto:fastest: %v", err)
	}
	if resp.Content != "from-fast" {
		t.Fatalf("auto:fastest content = %q (want from-fast: the alias strategy did not win)", resp.Content)
	}
	if got := factory.last(); got != "fastest" {
		t.Errorf("auto:fastest strategy = %q (want fastest: alias beats chain override cheapest)", got)
	}
}

func TestRuntimeStrategySetters(t *testing.T) {
	factory := &recordingHookFactory{}
	cfg := testConfig()
	cfg.Strategy = "balanced"
	cfg.StrategyHook = factory.hook
	eng := NewEngine(map[string]provider.Client{
		"cheap": okClient("from-cheap"),
		"fast":  okClient("from-fast"),
	}, nil, cfg)
	req := testReq()
	req.Model = "m"

	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := factory.last(); got != "balanced" {
		t.Fatalf("initial strategy = %q (want balanced)", got)
	}

	eng.SetGlobalStrategy("cheapest")
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := factory.last(); got != "cheapest" {
		t.Errorf("after SetGlobalStrategy strategy = %q (want cheapest)", got)
	}

	eng.SetChainStrategy("m", "fastest")
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := factory.last(); got != "fastest" {
		t.Errorf("after SetChainStrategy strategy = %q (want fastest: chain beats runtime global)", got)
	}

	global, chains := eng.StrategyState()
	if global != "cheapest" || chains["m"] != "fastest" {
		t.Errorf("StrategyState = (%q, %v) (want cheapest, {m: fastest})", global, chains)
	}

	eng.SetChainStrategy("m", "") // clear: inherits the global again
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := factory.last(); got != "cheapest" {
		t.Errorf("after clearing chain override strategy = %q (want cheapest)", got)
	}
	eng.SetGlobalStrategy("") // clear: back to the configured global
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := factory.last(); got != "balanced" {
		t.Errorf("after clearing global override strategy = %q (want balanced)", got)
	}
}

func TestLastRouting(t *testing.T) {
	if _, ok := NewEngine(nil, nil, testConfig()).LastRouting(); ok {
		t.Fatal("LastRouting before any request unexpectedly ok")
	}
	cfg := testConfig()
	cfg.Strategy = "cheapest"
	eng := NewEngine(map[string]provider.Client{"cheap": okClient("hi")}, nil, cfg)
	req := testReq()
	req.Model = "m"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	info, ok := eng.LastRouting()
	if !ok {
		t.Fatal("LastRouting after a request unexpectedly not ok")
	}
	if info.Strategy != "cheapest" || info.Model != "m" || info.Provider != "cheap" {
		t.Errorf("LastRouting = %+v (want strategy cheapest, model m, provider cheap)", info)
	}
	if info.At.IsZero() {
		t.Error("LastRouting.At is zero")
	}
}

// TestAutoCodeNamesItsStrategy: a request for "auto/code" — direct
// or via the "auto:code" alias — records strategy "code" with
// reason "cost-per-solved-task" in the last routing decision, so
// the X-Codvian-Decision header and the route trace name the mode
// and its reasoning. A direct "auto/cheap" request keeps the
// global strategy: only auto/code implies a strategy.
func TestAutoCodeNamesItsStrategy(t *testing.T) {
	cfg := testConfig()
	cfg.AutoRoute = func(model, prompt string) (string, autorouter.Decision, bool) {
		return "m-code", autorouter.Decision{VirtualModel: model, Tier: autorouter.TierCoding, Model: "m-code"}, true
	}
	eng := NewEngine(map[string]provider.Client{"cheap": okClient("hi")}, nil, cfg)
	for _, model := range []string{"auto/code", "auto:code"} {
		req := testReq()
		req.Model = model
		if _, err := eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat(%s): %v", model, err)
		}
		info, ok := eng.LastRouting()
		if !ok {
			t.Fatalf("LastRouting after %s unexpectedly not ok", model)
		}
		if info.Strategy != "code" || info.Reason != "cost-per-solved-task" || info.Model != "m-code" {
			t.Errorf("LastRouting(%s) = %+v (want strategy code, reason cost-per-solved-task, model m-code)", model, info)
		}
	}
	// Control: auto/cheap resolves through the same path but
	// implies no strategy — the global default stands.
	cfg.AutoRoute = func(model, prompt string) (string, autorouter.Decision, bool) {
		return "m-cheap", autorouter.Decision{VirtualModel: model, Tier: autorouter.TierQuickQA, Model: "m-cheap"}, true
	}
	eng = NewEngine(map[string]provider.Client{"cheap": okClient("hi")}, nil, cfg)
	req := testReq()
	req.Model = "auto/cheap"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat(auto/cheap): %v", err)
	}
	info, _ := eng.LastRouting()
	if info.Strategy != "priority" {
		t.Errorf("LastRouting(auto/cheap) strategy = %q, want priority (only auto/code implies a strategy; the global default is priority since M1.11)", info.Strategy)
	}
}

func TestDeadlineBudgetReturnsDeadlineExceeded(t *testing.T) {
	cfg := testConfig()
	cfg.DeadlineBudget = 50 * time.Millisecond
	eng := NewEngine(map[string]provider.Client{
		"slow": delayClient(500*time.Millisecond, "too-late"),
	}, nil, cfg)
	chain := []Attempt{{ProviderID: "slow", Model: "m"}}
	start := time.Now()
	_, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("ChatWithChain err = %v (want ErrDeadlineExceeded)", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ErrDeadlineExceeded does not wrap context.DeadlineExceeded (server 504 mapping depends on it)")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("deadline request took %v (want ~50ms: the budget must stop the combo, not wait out the provider)", elapsed)
	}
}

func TestDeadlineBudgetDoesNotMaskCallerCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.DeadlineBudget = 5 * time.Second
	eng := NewEngine(map[string]provider.Client{
		"slow": delayClient(500*time.Millisecond, "too-late"),
	}, nil, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := eng.ChatWithChain(ctx, []Attempt{{ProviderID: "slow", Model: "m"}}, testReq())
	if errors.Is(err, ErrDeadlineExceeded) {
		t.Errorf("cancelled request returned ErrDeadlineExceeded (want the caller's cancellation, not the budget error)")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled request err = %v (want context.Canceled)", err)
	}
}

func TestDeadlineBudgetStream(t *testing.T) {
	cfg := testConfig()
	cfg.DeadlineBudget = 50 * time.Millisecond
	slowStream := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		select {
		case <-time.After(500 * time.Millisecond):
			return &provider.ChatResponse{Content: "too-late"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	eng := NewEngine(map[string]provider.Client{"slow": slowStream}, nil, cfg)
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "slow", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	_, err = collectStream(t, ch)
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("stream terminal err = %v (want ErrDeadlineExceeded)", err)
	}
}

func TestProviderStatsAreLive(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{
		"a": delayClient(5*time.Millisecond, "ok"),
		"b": errClient(errors.New("boom")),
	}, nil, testConfig())
	if _, ok := eng.ProviderStats("a"); ok {
		t.Fatal("ProviderStats(a) ok before any attempt")
	}
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	// a succeeds; b fails on its own request.
	if _, _, err := eng.ChatWithChain(context.Background(), chain, testReq()); err != nil {
		t.Fatalf("Chat a: %v", err)
	}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "b", Model: "m"}}, testReq()); err == nil {
		t.Fatal("Chat b unexpectedly succeeded")
	}
	statsA, ok := eng.ProviderStats("a")
	if !ok || statsA.Attempts != 1 || statsA.Successes != 1 || statsA.SuccessRate != 1 {
		t.Errorf("ProviderStats(a) = %+v, ok=%v (want 1 attempt, 1 success, rate 1)", statsA, ok)
	}
	if statsA.LatencyMs <= 0 {
		t.Errorf("ProviderStats(a).LatencyMs = %v (want > 0: a real measured latency)", statsA.LatencyMs)
	}
	statsB, ok := eng.ProviderStats("b")
	if !ok || statsB.Attempts != 1 || statsB.Failures != 1 || statsB.SuccessRate != 0 {
		t.Errorf("ProviderStats(b) = %+v, ok=%v (want 1 attempt, 1 failure, rate 0)", statsB, ok)
	}
	all := eng.AllProviderStats()
	if len(all) != 2 {
		t.Errorf("AllProviderStats = %d providers (want 2)", len(all))
	}
}
