// Auto/* variant dispatch tests (M3 Phase D, builder D2).
//
// These tests prove the engine actually dispatches known auto/* ids
// through autorouter.ResolveAutoVariant over a live catalog —
// through the public interface (Engine.Chat / ChatStream /
// resolveAutoRequest + chainForModels, the dispatch seam) — and
// assert on the resolved model / strategy / chain, never on
// internals. Every test is written so it FAILS if the dispatch
// call is removed: the AutoRoute stub below always resolves to
// "fallback-coder", so a request that never reaches the variant
// resolver serves fallback-coder (or fails with a generic empty
// chain error) instead of the variant pick, and the fail-closed
// tests assert on the resolver's own error text, which only
// dispatch can produce.
package router

import (
	"context"
	"math/rand"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// autoVariantCatalog is the fake live catalog for dispatch tests:
// one model per provider, with real price / latency / quality /
// tier / billing signals, so each variant's pick rule is exercised
// by its own signal, not by catalog order.
func autoVariantCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provA", Model: "coder-best", Category: "coding", Tier: "premium", PriceUSDPerMTok: 10, HasPrice: true, LatencyMs: 500, HasLatency: true, Quality: 0.9, HasQuality: true},
		{Provider: "provB", Model: "coder-cheap", Category: "coding", Tier: "cheap", PriceUSDPerMTok: 1, HasPrice: true, LatencyMs: 100, HasLatency: true, Quality: 0.8, HasQuality: true},
		{Provider: "provC", Model: "chat-fast", Category: "chat", Tier: "premium", PriceUSDPerMTok: 5, HasPrice: true, LatencyMs: 50, HasLatency: true, Quality: 0.5, HasQuality: true},
		{Provider: "provD", Model: "chat-cheap", Category: "chat", Tier: "cheap", PriceUSDPerMTok: 0.5, HasPrice: true, LatencyMs: 800, HasLatency: true, Quality: 0.4, HasQuality: true},
		{Provider: "provE", Model: "free-model", Tier: "free", Billing: "metered", PriceUSDPerMTok: 0, HasPrice: true, LatencyMs: 200, HasLatency: true, Quality: 0.3, HasQuality: true},
		{Provider: "provF", Model: "sub-model", Tier: "premium", Billing: "subscription", OverageHardStop: true, RemainingPercent: 80, HasRemaining: true, PriceUSDPerMTok: 2, HasPrice: true, LatencyMs: 300, HasLatency: true, Quality: 0.6, HasQuality: true},
		{Provider: "provG", Model: "sub-spent", Tier: "premium", Billing: "subscription", OverageHardStop: true, RemainingPercent: 1, HasRemaining: true, PriceUSDPerMTok: 2, HasPrice: true, LatencyMs: 300, HasLatency: true, Quality: 0.6, HasQuality: true},
		{Provider: "provH", Model: "keyless-model", Tier: "premium", Billing: "keyless", RemainingPercent: 90, HasRemaining: true, PriceUSDPerMTok: 3, HasPrice: true, LatencyMs: 300, HasLatency: true, Quality: 0.5, HasQuality: true},
	}
}

// autoVariantModelProvider maps every catalog model (plus the
// AutoRoute stub's fallback) to its serving provider.
var autoVariantModelProvider = map[string]string{
	"coder-best":     "provA",
	"coder-cheap":    "provB",
	"chat-fast":      "provC",
	"chat-cheap":     "provD",
	"free-model":     "provE",
	"sub-model":      "provF",
	"sub-spent":      "provG",
	"keyless-model":  "provH",
	"fallback-coder": "provFallback",
}

// newAutoVariantTestEngine builds an engine whose AutoRoute stub
// always resolves to "fallback-coder": any test asserting a variant
// pick therefore FAILS if variant dispatch is removed (the request
// would serve fallback-coder instead).
func newAutoVariantTestEngine(t *testing.T) (*Engine, map[string]*fakeClient) {
	t.Helper()
	fakes := map[string]*fakeClient{}
	clients := map[string]provider.Client{}
	for _, p := range []string{"provA", "provB", "provC", "provD", "provE", "provF", "provG", "provH", "provFallback"} {
		fc := &fakeClient{}
		fakes[p] = fc
		clients[p] = fc
	}
	chainFor := func(model string) []Attempt {
		if p, ok := autoVariantModelProvider[model]; ok {
			return []Attempt{{ProviderID: p, Model: model}}
		}
		return nil
	}
	autoRoute := func(model, prompt string) (string, autorouter.Decision, bool) {
		return "fallback-coder", autorouter.Decision{VirtualModel: model, Tier: autorouter.TierCoding, Model: "fallback-coder"}, true
	}
	eng := NewEngine(clients, nil, Config{ChainFor: chainFor, AutoRoute: autoRoute})
	return eng, fakes
}

func installAutoCatalog(t *testing.T, catalog []autorouter.CatalogModel) {
	t.Helper()
	old := autoCatalogFn
	SetAutoCatalog(func() []autorouter.CatalogModel { return catalog })
	t.Cleanup(func() { SetAutoCatalog(old) })
}

func installAutoLastGood(t *testing.T, lastGood string) {
	t.Helper()
	old := autoLastGoodFn
	SetAutoLastGood(func() string { return lastGood })
	t.Cleanup(func() { SetAutoLastGood(old) })
}

func servedModel(t *testing.T, fakes map[string]*fakeClient) string {
	t.Helper()
	for _, fc := range fakes {
		fc.mu.Lock()
		n := len(fc.reqs)
		var m string
		if n > 0 {
			m = fc.reqs[n-1].Model
		}
		fc.mu.Unlock()
		if n > 0 {
			return m
		}
	}
	return ""
}

func totalChats(fakes map[string]*fakeClient) int {
	n := 0
	for _, fc := range fakes {
		fc.mu.Lock()
		n += fc.chats
		fc.mu.Unlock()
	}
	return n
}

func autoChat(t *testing.T, eng *Engine, model, prompt string) (*provider.ChatResponse, error) {
	t.Helper()
	return eng.Chat(context.Background(), provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: prompt}},
	})
}

// TestAutoCodingVariantResolvesQualityFirst: auto/coding dispatches
// to the highest-quality coding-category model with the
// quality-first strategy — not to the cheapest coder, and not to
// the AutoRoute stub's fallback-coder.
func TestAutoCodingVariantResolvesQualityFirst(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := autoChat(t, eng, "auto/coding", "Write a Python function to reverse a string")
	if err != nil {
		t.Fatalf("Chat(auto/coding) error: %v", err)
	}
	if resp == nil {
		t.Fatal("Chat(auto/coding) returned nil response")
	}
	if got := servedModel(t, fakes); got != "coder-best" {
		t.Errorf("served model = %q; want coder-best (highest quality coding model, not cheapest)", got)
	}
	if fakes["provFallback"].chats != 0 {
		t.Errorf("fallback provider chats = %d; want 0 (dispatch must not fall through to AutoRoute)", fakes["provFallback"].chats)
	}
	routing, ok := eng.LastRouting()
	if !ok {
		t.Fatal("LastRouting: no routing recorded")
	}
	if routing.Strategy != "quality-first" {
		t.Errorf("routing strategy = %q; want quality-first", routing.Strategy)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for variant dispatch")
	}
	if dec.Model != "coder-best" {
		t.Errorf("decision model = %q; want coder-best", dec.Model)
	}
}

// TestAutoCheapVariantResolvesCheapestFirst: auto/cheap dispatches
// to the cheapest-priced model in the pool with the cost-saver
// strategy.
func TestAutoCheapVariantResolvesCheapestFirst(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := autoChat(t, eng, "auto/cheap", "What is two plus two")
	if err != nil {
		t.Fatalf("Chat(auto/cheap) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "free-model" {
		t.Errorf("served model = %q; want free-model (cheapest in pool at $0/MTok)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "cost-saver" {
		t.Errorf("routing strategy = %q; want cost-saver", routing.Strategy)
	}
}

// TestAutoFastVariantResolvesFastestFirst: auto/fast dispatches to
// the lowest-latency model with the ship-fast strategy.
func TestAutoFastVariantResolvesFastestFirst(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := autoChat(t, eng, "auto/fast", "What is two plus two")
	if err != nil {
		t.Fatalf("Chat(auto/fast) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "chat-fast" {
		t.Errorf("served model = %q; want chat-fast (lowest p95 latency in pool)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "ship-fast" {
		t.Errorf("routing strategy = %q; want ship-fast", routing.Strategy)
	}
}

// TestAutoCodeVariantNamesCodeStrategy: auto/code dispatches with
// the "code" strategy (cost per solved task) over the
// coding/reasoning pool — cheapest there wins on the documented
// neutral default.
func TestAutoCodeVariantNamesCodeStrategy(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := autoChat(t, eng, "auto/code", "Write a Python function to reverse a string")
	if err != nil {
		t.Fatalf("Chat(auto/code) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "coder-cheap" {
		t.Errorf("served model = %q; want coder-cheap (cheapest coding/reasoning model)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "code" {
		t.Errorf("routing strategy = %q; want code", routing.Strategy)
	}
}

// TestAutoSmartExplorationDraw: the auto/smart 10% exploration draw
// fires under a fixed seed and does not fire under another —
// deterministically, not statistically. Seed 9 makes the engine's
// first Float64() 0.0036 (< 0.10: explore); seed 0 makes it 0.9452
// (>= 0.10: exploit the top quality pick). The strategy stays
// quality-first either way.
func TestAutoSmartExplorationDraw(t *testing.T) {
	catalog := autoVariantCatalog()
	top := "coder-best" // highest quality in the catalog

	t.Run("explores under seed 9", func(t *testing.T) {
		eng, fakes := newAutoVariantTestEngine(t)
		installAutoCatalog(t, catalog)
		eng.rand = rand.New(rand.NewSource(9))
		_, err := autoChat(t, eng, "auto/smart", "Write a Python function to reverse a string")
		if err != nil {
			t.Fatalf("Chat(auto/smart) error: %v", err)
		}
		got := servedModel(t, fakes)
		if got == top {
			t.Errorf("served model = %q; want a non-top exploration pick under seed 9, not the top pick", got)
		}
		if got == "" {
			t.Error("no model served")
		}
		for _, m := range catalog {
			if m.Model == got {
				return // explored pick is a real catalog model
			}
		}
		t.Errorf("explored model %q is not in the catalog", got)
		routing, _ := eng.LastRouting()
		if routing.Strategy != "quality-first" {
			t.Errorf("routing strategy = %q; want quality-first even when exploring", routing.Strategy)
		}
	})

	t.Run("exploits under seed 0", func(t *testing.T) {
		eng, fakes := newAutoVariantTestEngine(t)
		installAutoCatalog(t, catalog)
		eng.rand = rand.New(rand.NewSource(0))
		_, err := autoChat(t, eng, "auto/smart", "Write a Python function to reverse a string")
		if err != nil {
			t.Fatalf("Chat(auto/smart) error: %v", err)
		}
		if got := servedModel(t, fakes); got != top {
			t.Errorf("served model = %q; want %q (top quality pick, no exploration under seed 0)", got, top)
		}
	})
}

// TestAutoChaosVariantPanelChain: auto/chaos resolves to a panel
// chain (one model per provider, catalog order) with strategy
// chaos-mode. The panel order is asserted at the dispatch seam
// (resolveAutoRequest + chainForModels); Chat proves the strategy
// threads through and the first panel attempt serves.
func TestAutoChaosVariantPanelChain(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())

	strategy, req, _, disp := eng.resolveAutoRequest(context.Background(), provider.ChatRequest{
		Model:    "auto/chaos",
		Messages: []provider.Message{{Role: "user", Content: "Write a Python function"}},
	})
	if disp.Err != nil {
		t.Fatalf("resolveAutoRequest(auto/chaos) error: %v", disp.Err)
	}
	if strategy != "chaos-mode" {
		t.Errorf("strategy = %q; want chaos-mode", strategy)
	}
	wantPanel := []string{"coder-best", "coder-cheap", "chat-fast", "chat-cheap", "free-model"}
	if len(disp.Ordered) != len(wantPanel) {
		t.Fatalf("panel = %v; want %v", disp.Ordered, wantPanel)
	}
	for i, m := range wantPanel {
		if disp.Ordered[i] != m {
			t.Errorf("panel[%d] = %q; want %q (panel = %v)", i, disp.Ordered[i], m, disp.Ordered)
		}
	}
	if req.Model != "coder-best" {
		t.Errorf("request model = %q; want coder-best (first panel model)", req.Model)
	}
	chain := eng.chainForModels(disp.Ordered)
	if len(chain) != len(wantPanel) {
		t.Fatalf("chainForModels(panel) = %d attempts; want %d", len(chain), len(wantPanel))
	}
	for i, m := range wantPanel {
		if chain[i].Model != m {
			t.Errorf("chain[%d].Model = %q; want %q", i, chain[i].Model, m)
		}
		if chain[i].ProviderID != autoVariantModelProvider[m] {
			t.Errorf("chain[%d].ProviderID = %q; want %q", i, chain[i].ProviderID, autoVariantModelProvider[m])
		}
	}

	// Chat level: the panel fans out (D3's combo engine owns
	// winner selection, so the served model is any panel
	// member, not necessarily the first) with strategy
	// chaos-mode threaded through.
	_, err := autoChat(t, eng, "auto/chaos", "Write a Python function")
	if err != nil {
		t.Fatalf("Chat(auto/chaos) error: %v", err)
	}
	got := servedModel(t, fakes)
	inPanel := false
	for _, m := range wantPanel {
		if got == m {
			inPanel = true
		}
	}
	if !inPanel {
		t.Errorf("served model = %q; want one of the panel %v (fan-out winner is D3's combo engine)", got, wantPanel)
	}
	if n := totalChats(fakes); n < 2 {
		t.Errorf("upstream chats = %d; want >= 2 (the panel must fan out, not serve a single attempt)", n)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "chaos-mode" {
		t.Errorf("routing strategy = %q; want chaos-mode", routing.Strategy)
	}
}

// TestAutoChaosVariantStreamsPanelStrategy: the chaos-mode strategy
// threads through the streaming entry point too. The panel fans
// out through D3's combo engine (which serves the fused result
// via non-streaming panel calls), so the assertion is the strategy
// + a panel attempt being served — not which provider streamed.
func TestAutoChaosVariantStreamsPanelStrategy(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := eng.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "auto/chaos",
		Messages: []provider.Message{{Role: "user", Content: "Write a Python function"}},
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream(auto/chaos) error: %v", err)
	}
	if resp == nil {
		t.Fatal("ChatStream(auto/chaos) returned nil response")
	}
	if n := totalChats(fakes); n < 1 {
		t.Errorf("upstream chats = %d; want >= 1 (the panel must be attempted through the stream entry point)", n)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "chaos-mode" {
		t.Errorf("routing strategy = %q; want chaos-mode", routing.Strategy)
	}
}

// TestAutoThriftyVariantRungOrder: auto/thrifty resolves in ladder
// rung order (subscription -> keyless -> free -> cheap -> premium),
// exhausted quota rungs gated out (sub-spent at 1% remaining is
// gone), and serves the rung-0 model first.
func TestAutoThriftyVariantRungOrder(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())

	_, _, _, disp := eng.resolveAutoRequest(context.Background(), provider.ChatRequest{
		Model:    "auto/thrifty",
		Messages: []provider.Message{{Role: "user", Content: "What is two plus two"}},
	})
	if disp.Err != nil {
		t.Fatalf("resolveAutoRequest(auto/thrifty) error: %v", disp.Err)
	}
	want := []string{"sub-model", "keyless-model", "free-model", "coder-cheap", "chat-cheap", "coder-best", "chat-fast"}
	if len(disp.Ordered) != len(want) {
		t.Fatalf("ordered = %v; want %v", disp.Ordered, want)
	}
	for i, m := range want {
		if disp.Ordered[i] != m {
			t.Errorf("ordered[%d] = %q; want %q (ordered = %v)", i, disp.Ordered[i], m, disp.Ordered)
		}
	}
	chain := eng.chainForModels(disp.Ordered)
	if len(chain) != len(want) {
		t.Fatalf("chainForModels(ordered) = %d attempts; want %d", len(chain), len(want))
	}
	for i, m := range want {
		if chain[i].Model != m {
			t.Errorf("chain[%d].Model = %q; want %q", i, chain[i].Model, m)
		}
	}

	_, err := autoChat(t, eng, "auto/thrifty", "What is two plus two")
	if err != nil {
		t.Fatalf("Chat(auto/thrifty) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "sub-model" {
		t.Errorf("served model = %q; want sub-model (rung 0 first)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "cost-saver" {
		t.Errorf("routing strategy = %q; want cost-saver", routing.Strategy)
	}
}

// TestAutoSubscriptionNoRungFailsClosed: with no usable
// subscription rung (the only subscription models are exhausted),
// auto/subscription errors with the resolver's fail-closed reason
// and NO provider is ever called — never a billable fallback. The
// error text is asserted so the test FAILS if dispatch is removed
// (without it the error would be a generic empty-chain error).
func TestAutoSubscriptionNoRungFailsClosed(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	catalog := autoVariantCatalog()
	// Drop the usable subscription model; sub-spent (1% left) stays
	// and must be gated out, leaving rung 0 empty.
	var kept []autorouter.CatalogModel
	for _, m := range catalog {
		if m.Model != "sub-model" {
			kept = append(kept, m)
		}
	}
	installAutoCatalog(t, kept)
	_, err := autoChat(t, eng, "auto/subscription", "What is two plus two")
	if err == nil {
		t.Fatal("Chat(auto/subscription) with an empty subscription rung: got nil error; want an honest fail-closed error, never a billable fallback")
	}
	if !strings.Contains(err.Error(), "no subscription-quota target") {
		t.Errorf("error = %q; want it to name the empty subscription rung (fail-closed reason)", err.Error())
	}
	if n := totalChats(fakes); n != 0 {
		t.Errorf("upstream chats = %d; want 0 (no provider may be called on fail-closed)", n)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for the failed subscription request")
	}
	if !strings.Contains(dec.Reason, "subscription") {
		t.Errorf("decision reason = %q; want it to name the subscription failure", dec.Reason)
	}
}

// TestAutoLKGPNoRecordErrors: with no last-good model recorded,
// auto/lkgp errors honestly (no silent fallback to another model)
// and no provider is called. With a recorded last-good it serves
// exactly that model.
func TestAutoLKGPNoRecordErrors(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	// No LastGood hook installed: nothing recorded.
	_, err := autoChat(t, eng, "auto/lkgp", "What is two plus two")
	if err == nil {
		t.Fatal("Chat(auto/lkgp) with no recorded last-good: got nil error; want an honest error, never a silent fallback")
	}
	if !strings.Contains(err.Error(), "no last-known-good") {
		t.Errorf("error = %q; want it to name the missing last-known-good model", err.Error())
	}
	if n := totalChats(fakes); n != 0 {
		t.Errorf("upstream chats = %d; want 0 (no provider may be called when lkgp is unrecorded)", n)
	}

	// Recorded last-good: serves exactly that model, sticky.
	installAutoLastGood(t, "coder-cheap")
	_, err = autoChat(t, eng, "auto/lkgp", "What is two plus two")
	if err != nil {
		t.Fatalf("Chat(auto/lkgp) with recorded last-good error: %v", err)
	}
	if got := servedModel(t, fakes); got != "coder-cheap" {
		t.Errorf("served model = %q; want coder-cheap (the recorded last-good model)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "lkgp" {
		t.Errorf("routing strategy = %q; want lkgp", routing.Strategy)
	}
}

// TestAutoVariantWithoutCatalogFallsBackToAutoRoute: with no live
// catalog installed, variant dispatch cannot run and the existing
// AutoRoute path serves exactly as before.
func TestAutoVariantWithoutCatalogFallsBackToAutoRoute(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	// No catalog installed.
	_, err := autoChat(t, eng, "auto/coding", "Write a Python function to reverse a string")
	if err != nil {
		t.Fatalf("Chat(auto/coding) without catalog error: %v", err)
	}
	if got := servedModel(t, fakes); got != "fallback-coder" {
		t.Errorf("served model = %q; want fallback-coder (AutoRoute path when no catalog is installed)", got)
	}
}

// TestAutoPlainStillClassifiesWithCatalog: plain "auto" is NOT
// hijacked by variant dispatch even with a catalog installed — it
// still goes through the embedding-tier AutoRoute path.
func TestAutoPlainStillClassifiesWithCatalog(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := autoChat(t, eng, "auto", "Write a Python function to reverse a string")
	if err != nil {
		t.Fatalf("Chat(auto) error: %v", err)
	}
	if got := servedModel(t, fakes); got != "fallback-coder" {
		t.Errorf("served model = %q; want fallback-coder (plain auto still classifies via AutoRoute)", got)
	}
}

// TestAutoCheapStreamsToo: the variant dispatch runs on the
// streaming entry point as well.
func TestAutoCheapStreamsToo(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	_, err := eng.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "auto/cheap",
		Messages: []provider.Message{{Role: "user", Content: "What is two plus two"}},
	}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream(auto/cheap) error: %v", err)
	}
	if fakes["provE"].streams != 1 {
		t.Errorf("provE streams = %d; want 1 (cheapest model streams)", fakes["provE"].streams)
	}
}

// TestChainForModelsDedupAndOrder: chainForModels concatenates the
// per-model chains in order and dedupes by (provider, key, model).
func TestChainForModelsDedupAndOrder(t *testing.T) {
	eng, _ := newAutoVariantTestEngine(t)
	chain := eng.chainForModels([]string{"coder-best", "coder-cheap", "coder-best", "chat-fast"})
	if len(chain) != 3 {
		t.Fatalf("chainForModels = %v; want 3 attempts (duplicate coder-best removed)", chain)
	}
	want := []Attempt{
		{ProviderID: "provA", Model: "coder-best"},
		{ProviderID: "provB", Model: "coder-cheap"},
		{ProviderID: "provC", Model: "chat-fast"},
	}
	for i, w := range want {
		if chain[i] != w {
			t.Errorf("chain[%d] = %+v; want %+v", i, chain[i], w)
		}
	}
	if got := eng.chainForModels(nil); len(got) != 0 {
		t.Errorf("chainForModels(nil) = %v; want empty", got)
	}
	if got := eng.chainForModels([]string{"coder-best"}); len(got) != 1 || got[0].Model != "coder-best" {
		t.Errorf("chainForModels([coder-best]) = %v; want one coder-best attempt", got)
	}
}

// TestAutoDispatchSmokeRemainingIDs (M3 Phase D, D2X): dispatch-level
// smoke coverage for the remaining auto/* ids D2's tests did not
// drive through Engine.Chat — auto/offline, auto/chat, the
// auto/best-* family and auto/<category>:<tier> composition.
// Each case asserts the resolved model actually served plus
// LastRouting().Strategy, so it FAILS if dispatch is removed
// (the AutoRoute stub would serve fallback-coder instead).
// Expected picks are derived from the resolver's documented rules
// (autovariants.go), not from catalog order alone — see the
// per-case comments.
func TestAutoDispatchSmokeRemainingIDs(t *testing.T) {
	cases := []struct {
		id           string
		wantModel    string
		wantStrategy string
		wantFailOpen bool // decision Reason must record the fail-open
	}{
		// auto/offline: most quota headroom first; no model
		// reports quota, so every headroom is the full 1.0
		// default and the first catalog model wins.
		{"auto/offline", "coder-best", "offline-friendly", false},
		// auto/chat: balanced over the full pool (chat adds no
		// pool filter — scoring only), so catalog order decides.
		{"auto/chat", "coder-best", "balanced", false},
		// auto/best-free: free-tier filter leaves only free-model.
		{"auto/best-free", "free-model", "cost-saver", false},
		// auto/best-coding: quality-first over the full pool
		// (coding adds no pool filter); highest quality wins.
		{"auto/best-coding", "coder-best", "quality-first", false},
		// auto/best-fast: ship-fast; lowest latency wins.
		{"auto/best-fast", "chat-fast", "ship-fast", false},
		// auto/best-chat: balanced over the full pool.
		{"auto/best-chat", "coder-best", "balanced", false},
		// auto/best-vision: no vision-capable model exists in
		// the catalog, so the vision filter FAILS OPEN to the
		// full pool (balanced -> first model). The fail-open is
		// the documented gap, not a vision pick.
		{"auto/best-vision", "coder-best", "balanced", true},
		// auto/best-reasoning: no reasoning model exists in the
		// catalog, so the reasoning filter FAILS OPEN likewise.
		{"auto/best-reasoning", "coder-best", "quality-first", true},
		// auto/best-coding-fast: ship-fast; coding adds no pool
		// filter, so the fastest model overall wins, not the
		// fastest coder.
		{"auto/best-coding-fast", "chat-fast", "ship-fast", false},
		// auto/coding:free: free-tier filter leaves only
		// free-model; the free tier carries no weight bias, so
		// quality-first applies.
		{"auto/coding:free", "free-model", "quality-first", false},
		// auto/chat:reliable: reliability-first; no model
		// reports breaker health, so catalog order decides.
		{"auto/chat:reliable", "coder-best", "reliability-first", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			eng, fakes := newAutoVariantTestEngine(t)
			installAutoCatalog(t, autoVariantCatalog())
			_, err := autoChat(t, eng, tc.id, "Write a Python function to reverse a string")
			if err != nil {
				t.Fatalf("Chat(%s) error: %v", tc.id, err)
			}
			if got := servedModel(t, fakes); got != tc.wantModel {
				t.Errorf("served model = %q; want %q", got, tc.wantModel)
			}
			if fakes["provFallback"].chats != 0 {
				t.Errorf("fallback provider chats = %d; want 0 (dispatch must not fall through to AutoRoute)", fakes["provFallback"].chats)
			}
			routing, ok := eng.LastRouting()
			if !ok {
				t.Fatal("LastRouting: no routing recorded")
			}
			if routing.Strategy != tc.wantStrategy {
				t.Errorf("routing strategy = %q; want %q", routing.Strategy, tc.wantStrategy)
			}
			dec, ok := eng.LastAutoDecision()
			if !ok {
				t.Fatal("LastAutoDecision: no decision recorded for variant dispatch")
			}
			if dec.Model != tc.wantModel {
				t.Errorf("decision model = %q; want %q", dec.Model, tc.wantModel)
			}
			if tc.wantFailOpen && !strings.Contains(dec.Reason, "fail-open") {
				t.Errorf("decision reason = %q; want it to record the fail-open (no capable model in catalog)", dec.Reason)
			}
		})
	}
}

// TestAutoProCodingFailOpenPinned (M3 Phase D, D2X): auto/pro-coding
// FAILS OPEN in production — main.go's SetAutoCatalog supplier only
// ever sets Tier "free" (for a $0 blended price) and leaves every
// other model unclassified, so no Tier "premium" model exists there
// and the pro filter matches nothing. This test mirrors production
// by clearing the "premium" tiers out of the shared test catalog
// (which DOES carry Tier "premium" entries — that catalog does not
// mirror production on this point) and pins the fail-open: the
// full-pool quality-first pick serves, and the decision records
// the fail-open. This is the DOCUMENTED GAP, not a pass of the pro
// filter: do NOT read it as proof that pro-coding restricts to
// premium models — it cannot, because no premium data exists.
func TestAutoProCodingFailOpenPinned(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	catalog := autoVariantCatalog()
	for i := range catalog {
		if catalog[i].Tier == "premium" {
			catalog[i].Tier = ""
		}
	}
	installAutoCatalog(t, catalog)
	_, err := autoChat(t, eng, "auto/pro-coding", "Write a Python function to reverse a string")
	if err != nil {
		t.Fatalf("Chat(auto/pro-coding) error: %v", err)
	}
	// Fail-open to the full-pool pick: highest quality overall.
	if got := servedModel(t, fakes); got != "coder-best" {
		t.Errorf("served model = %q; want coder-best (full-pool quality-first pick — the pro filter matched nothing)", got)
	}
	routing, _ := eng.LastRouting()
	if routing.Strategy != "quality-first" {
		t.Errorf("routing strategy = %q; want quality-first", routing.Strategy)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for variant dispatch")
	}
	if !strings.Contains(dec.Reason, "no premium-tier model in catalog; filter is fail-open so the full pool is used") {
		t.Errorf("decision reason = %q; want it to record the premium fail-open", dec.Reason)
	}
	if strings.Contains(dec.Reason, "pool filtered to premium-tier") {
		t.Errorf("decision reason = %q; must NOT claim a premium-tier filter applied", dec.Reason)
	}
}

// TestChainForModelsKeepsDistinctKeys: two attempts for the same
// (provider, model) under different keys are NOT duplicates.
func TestChainForModelsKeepsDistinctKeys(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"p": &fakeClient{}}, nil, Config{
		ChainFor: func(model string) []Attempt {
			if model == "m" {
				return []Attempt{
					{ProviderID: "p", Model: "m", KeyID: "k1"},
					{ProviderID: "p", Model: "m", KeyID: "k2"},
					{ProviderID: "p", Model: "m", KeyID: "k1"},
				}
			}
			return nil
		},
	})
	chain := eng.chainForModels([]string{"m", "m"})
	if len(chain) != 2 {
		t.Fatalf("chainForModels = %v; want 2 attempts (k1, k2 — the repeated k1 deduped)", chain)
	}
	if chain[0].KeyID != "k1" || chain[1].KeyID != "k2" {
		t.Errorf("chain keys = %q, %q; want k1, k2", chain[0].KeyID, chain[1].KeyID)
	}
}
