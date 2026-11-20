// M6.5-wiring router tests: the per-request hook context is built
// from the request and threaded through orderAttempts (and the
// strategy-tree path) to the per-request strategy hook, without
// changing the behaviour of the context-free path.
package router

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// recordingHookContextFactory records the context the per-request
// hook is built with, and returns the chain unchanged.
type recordingHookContextFactory struct {
	got []StrategyHookContext
}

func (f *recordingHookContextFactory) hook(strategy string, ctx StrategyHookContext) func(chain []Attempt) []Attempt {
	f.got = append(f.got, ctx)
	return func(chain []Attempt) []Attempt { return chain }
}

func TestM65OrderAttemptsPassesHookContext(t *testing.T) {
	factory := &recordingHookContextFactory{}
	cfg := testConfig()
	cfg.StrategyHookForRequest = factory.hook
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	want := StrategyHookContext{PromptTokens: 2000, Temperature: 0.7, TemperatureSet: true}
	ordered, _ := eng.orderAttemptsWithContext(chain, "cache-optimized", "hard", false, want)
	if len(ordered) != 2 {
		t.Fatalf("ordered %d attempts, want 2", len(ordered))
	}
	if len(factory.got) != 1 {
		t.Fatalf("per-request hook built %d times, want 1", len(factory.got))
	}
	if factory.got[0] != want {
		t.Errorf("hook context = %+v, want %+v", factory.got[0], want)
	}
}

func TestM65OrderAttemptsPrefersPerRequestHook(t *testing.T) {
	// When both hooks are installed, the per-request hook wins;
	// the context-free hook must not run.
	perRequest := &recordingHookContextFactory{}
	plainCalled := false
	cfg := testConfig()
	cfg.StrategyHookForRequest = perRequest.hook
	cfg.StrategyHook = func(strategy string) func(chain []Attempt) []Attempt {
		plainCalled = true
		return func(chain []Attempt) []Attempt { return chain }
	}
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}}
	eng.orderAttemptsWithContext(chain, "balanced", "hard", false, StrategyHookContext{PromptTokens: 2000})
	if len(perRequest.got) != 1 {
		t.Errorf("per-request hook built %d times, want 1", len(perRequest.got))
	}
	if plainCalled {
		t.Errorf("context-free StrategyHook ran while StrategyHookForRequest was set")
	}
}

func TestM65OrderAttemptsWithoutContextKeepsOldHook(t *testing.T) {
	// The context-free entry point still calls the old
	// StrategyHook (with no per-request hook installed), and a
	// per-request hook installed alone runs with a zero context
	// there — so existing callers (embeddings, tests) behave
	// exactly as before.
	plainCalled := false
	cfg := testConfig()
	cfg.StrategyHook = func(strategy string) func(chain []Attempt) []Attempt {
		plainCalled = true
		return func(chain []Attempt) []Attempt { return chain }
	}
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}}
	eng.orderAttempts(chain, "balanced", "hard", false)
	if !plainCalled {
		t.Errorf("context-free StrategyHook did not run under orderAttempts")
	}

	perRequest := &recordingHookContextFactory{}
	cfg = testConfig()
	cfg.StrategyHookForRequest = perRequest.hook
	eng = NewEngine(nil, nil, cfg)
	eng.orderAttempts(chain, "cache-optimized", "hard", false)
	if len(perRequest.got) != 1 {
		t.Fatalf("per-request hook built %d times, want 1", len(perRequest.got))
	}
	if perRequest.got[0] != (StrategyHookContext{}) {
		t.Errorf("per-request hook under orderAttempts got context %+v, want zero", perRequest.got[0])
	}
}

func TestM65PreserveOrderNeverCallsHook(t *testing.T) {
	// An explicit contract order short-circuits before any hook:
	// the per-request hook must not run at all, whatever context
	// it would have received.
	perRequest := &recordingHookContextFactory{}
	cfg := testConfig()
	cfg.StrategyHookForRequest = perRequest.hook
	eng := NewEngine(nil, nil, cfg)
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	ordered, _ := eng.orderAttemptsWithContext(chain, "cache-optimized", "hard", true,
		StrategyHookContext{PromptTokens: 2000, ContractPinned: true})
	if len(perRequest.got) != 0 {
		t.Errorf("per-request hook built %d times under preserveOrder, want 0", len(perRequest.got))
	}
	if len(ordered) != 2 || ordered[0].ProviderID != "a" || ordered[1].ProviderID != "b" {
		t.Errorf("preserveOrder order = %v, want [a, b] unchanged", ordered)
	}
}

// M3 Area 1 (PARITY row 394): the engine's configured SLA
// fields reach both consumers — the per-request hook context
// (strategyHookContextFor) and the autorouter policy
// (slaPolicy). Unset means the defaults (2000 ms / 5%); an
// explicitly configured error rate of 0 (SLAMaxErrorRateSet,
// set by main.go from routing.sla.max_error_rate) reaches both
// as 0 — zero-tolerance — and must NOT be rewritten to 0.05.
// A hand-built positive rate without the flag still works
// (back-compat for direct Config construction, e.g.
// TestDispatchSLAAwareHonoursConfiguredPolicy).
func TestSLAConfiguredPolicyReachesHookContext(t *testing.T) {
	req := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
	// Unset: both consumers see the defaults.
	eng := NewEngine(nil, nil, testConfig())
	ctx := eng.strategyHookContextFor(req, false)
	if ctx.SLATargetP95Ms != 2000 || ctx.SLAMaxErrorRate != 0.05 || ctx.SLAMaxCostPer1M != 0 || ctx.SLAHardConstraints {
		t.Errorf("unset SLA context = %+v, want 2000 / 0.05 / 0 / soft", ctx)
	}
	if p := eng.slaPolicy(); p.TargetP95Ms != 2000 || p.MaxErrorRate != 0.05 || p.MaxCostPer1MTokens != 0 || p.HardConstraints {
		t.Errorf("unset slaPolicy = %+v, want 2000 / 0.05 / 0 / soft", p)
	}
	// Explicitly configured, including zero-tolerance: the 0
	// must survive to both consumers as 0, not 0.05.
	cfg := testConfig()
	cfg.SLATargetP95Ms = 500
	cfg.SLAMaxErrorRate = 0
	cfg.SLAMaxErrorRateSet = true
	cfg.SLAMaxCostPer1M = 5
	cfg.SLAHardConstraints = true
	eng = NewEngine(nil, nil, cfg)
	ctx = eng.strategyHookContextFor(req, false)
	if ctx.SLATargetP95Ms != 500 || ctx.SLAMaxErrorRate != 0 || !ctx.SLAMaxErrorRateSet || ctx.SLAMaxCostPer1M != 5 || !ctx.SLAHardConstraints {
		t.Errorf("configured SLA context = %+v, want 500 / 0 (zero-tolerance, Set) / 5 / hard", ctx)
	}
	if p := eng.slaPolicy(); p.TargetP95Ms != 500 || p.MaxErrorRate != 0 || p.MaxCostPer1MTokens != 5 || !p.HardConstraints {
		t.Errorf("configured slaPolicy = %+v, want 500 / 0 (zero-tolerance) / 5 / hard", p)
	}
	// Hand-built positive rate without the flag: used as-is.
	cfg = testConfig()
	cfg.SLAMaxErrorRate = 0.20
	eng = NewEngine(nil, nil, cfg)
	ctx = eng.strategyHookContextFor(req, false)
	if ctx.SLAMaxErrorRate != 0.20 {
		t.Errorf("hand-built rate context = %v, want 0.20", ctx.SLAMaxErrorRate)
	}
	if p := eng.slaPolicy(); p.MaxErrorRate != 0.20 {
		t.Errorf("hand-built rate slaPolicy = %v, want 0.20", p.MaxErrorRate)
	}
}

func TestM65StrategyHookContextFor(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	long := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: RoleUser, Content: strings.Repeat("word ", 1500)}},
	}
	ctx := eng.strategyHookContextFor(long, false)
	if ctx.PromptTokens < 1024 {
		t.Errorf("long prompt counted %d tokens, want >= 1024 (gate floor)", ctx.PromptTokens)
	}
	if ctx.HasTools || ctx.TemperatureSet || ctx.ContractPinned {
		t.Errorf("long prompt context = %+v, want no tools / unset temperature / not pinned", ctx)
	}

	short := provider.ChatRequest{
		Model:    "gpt-4o",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
	if got := eng.strategyHookContextFor(short, false).PromptTokens; got >= 1024 {
		t.Errorf("short prompt counted %d tokens, want < 1024", got)
	}

	withTools := long
	withTools.Tools = []provider.ToolDefinition{{}}
	withTools.Temperature = 0.7
	ctx = eng.strategyHookContextFor(withTools, true)
	if !ctx.HasTools {
		t.Errorf("tools request HasTools = false, want true")
	}
	if !ctx.TemperatureSet || ctx.Temperature != 0.7 {
		t.Errorf("tools request temperature = (%v, set=%v), want (0.7, true)", ctx.Temperature, ctx.TemperatureSet)
	}
	if !ctx.ContractPinned {
		t.Errorf("explicit-order request ContractPinned = false, want true")
	}
}
