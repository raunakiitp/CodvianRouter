// Auto/lkgp production-wiring end-to-end tests (M3 Phase D, builder D2X).
//
// autodispatch_test.go is package router (internal) and cannot import
// internal/strategy — strategy imports router, so that would be an
// import cycle (the same cycle the engine's supplier design works
// around). This file is package router_test (external), the standard
// cycle-breaker, and drives the engine only through its exported API
// to prove the real registry -> SetAutoLastGood supplier ->
// Engine.Chat path for auto/lkgp. It duplicates a minimal engine
// setup (stub clients, catalog, AutoRoute stub) because the
// internal test helpers in package router are not visible here.
package router_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// lkgpStubClient is a minimal provider.Client that records which
// model each Chat/ChatStream served.
type lkgpStubClient struct {
	mu        sync.Mutex
	chats     int
	lastModel string
}

func (c *lkgpStubClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chats++
	c.lastModel = req.Model
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *lkgpStubClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chats++
	c.lastModel = req.Model
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *lkgpStubClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *lkgpStubClient) TestKey(ctx context.Context) error                { return nil }

// lkgpModelProvider maps every catalog model (plus the AutoRoute
// stub's fallback) to its serving provider.
var lkgpModelProvider = map[string]string{
	"coder-best":     "provA",
	"coder-cheap":    "provB",
	"chat-fast":      "provC",
	"fallback-coder": "provFallback",
}

// lkgpCatalog is a minimal live catalog: the models the LKGP
// records below name, plus one more candidate.
func lkgpCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provA", Model: "coder-best", Category: "coding", Tier: "premium", PriceUSDPerMTok: 10, HasPrice: true, LatencyMs: 500, HasLatency: true, Quality: 0.9, HasQuality: true},
		{Provider: "provB", Model: "coder-cheap", Category: "coding", Tier: "cheap", PriceUSDPerMTok: 1, HasPrice: true, LatencyMs: 100, HasLatency: true, Quality: 0.8, HasQuality: true},
		{Provider: "provC", Model: "chat-fast", Category: "chat", Tier: "premium", PriceUSDPerMTok: 5, HasPrice: true, LatencyMs: 50, HasLatency: true, Quality: 0.5, HasQuality: true},
	}
}

// newLKGPTestEngine builds an engine whose AutoRoute stub always
// resolves to "fallback-coder": any test asserting an LKGP pick
// therefore FAILS if the supplier/dispatch path is removed (the
// request would serve fallback-coder instead).
func newLKGPTestEngine(t *testing.T) (*router.Engine, map[string]*lkgpStubClient) {
	t.Helper()
	stubs := map[string]*lkgpStubClient{}
	clients := map[string]provider.Client{}
	for _, p := range []string{"provA", "provB", "provC", "provFallback"} {
		s := &lkgpStubClient{}
		stubs[p] = s
		clients[p] = s
	}
	chainFor := func(model string) []router.Attempt {
		if p, ok := lkgpModelProvider[model]; ok {
			return []router.Attempt{{ProviderID: p, Model: model}}
		}
		return nil
	}
	autoRoute := func(model, prompt string) (string, autorouter.Decision, bool) {
		return "fallback-coder", autorouter.Decision{VirtualModel: model, Tier: autorouter.TierCoding, Model: "fallback-coder"}, true
	}
	eng := router.NewEngine(clients, nil, router.Config{ChainFor: chainFor, AutoRoute: autoRoute})
	return eng, stubs
}

func installLKGPCatalog(t *testing.T) {
	t.Helper()
	router.SetAutoCatalog(func() []autorouter.CatalogModel { return lkgpCatalog() })
	t.Cleanup(func() { router.SetAutoCatalog(nil) })
}

// installLKGPRegistrySupplier installs the production-shape
// supplier: it reads the registry's Latest(), exactly as
// cmd/codvian-router's buildAutoRouter does, returning "" when
// nothing was ever recorded.
func installLKGPRegistrySupplier(t *testing.T, reg *strategy.LKGPRegistry) {
	t.Helper()
	router.SetAutoLastGood(func() string {
		model, ok := reg.Latest()
		if !ok {
			return ""
		}
		return model
	})
	t.Cleanup(func() { router.SetAutoLastGood(nil) })
}

func lkgpChat(t *testing.T, eng *router.Engine) (*provider.ChatResponse, error) {
	t.Helper()
	return eng.Chat(context.Background(), provider.ChatRequest{
		Model:    "auto/lkgp",
		Messages: []provider.Message{{Role: "user", Content: "What is two plus two"}},
	})
}

// TestAutoLKGPResolvesRegistryLatestEndToEnd: a last-good recorded
// in the real LKGP registry resolves through the supplier into
// Engine.Chat for auto/lkgp — and a newer record on another chain
// moves the pick, proving Latest() (most recently updated) is
// what the engine reads, not the first record or a fixed chain.
func TestAutoLKGPResolvesRegistryLatestEndToEnd(t *testing.T) {
	eng, stubs := newLKGPTestEngine(t)
	installLKGPCatalog(t)
	reg := strategy.NewLKGPRegistry()
	installLKGPRegistrySupplier(t, reg)

	reg.RecordSuccessFor("chain-a", "provB", "coder-cheap", "")
	time.Sleep(2 * time.Millisecond)
	reg.RecordSuccessFor("chain-b", "provC", "chat-fast", "")

	if _, err := lkgpChat(t, eng); err != nil {
		t.Fatalf("Chat(auto/lkgp) error: %v", err)
	}
	if got := stubs["provC"].lastModel; got != "chat-fast" {
		t.Errorf("served model = %q; want chat-fast (the registry's latest record, chain-b)", got)
	}
	if stubs["provFallback"].chats != 0 {
		t.Errorf("fallback provider chats = %d; want 0 (dispatch must not fall through to AutoRoute)", stubs["provFallback"].chats)
	}
	routing, ok := eng.LastRouting()
	if !ok {
		t.Fatal("LastRouting: no routing recorded")
	}
	if routing.Strategy != "lkgp" {
		t.Errorf("routing strategy = %q; want lkgp", routing.Strategy)
	}
	dec, ok := eng.LastAutoDecision()
	if !ok {
		t.Fatal("LastAutoDecision: no decision recorded for variant dispatch")
	}
	if dec.Model != "chat-fast" {
		t.Errorf("decision model = %q; want chat-fast", dec.Model)
	}

	// A newer record on another chain moves the global last-good.
	time.Sleep(2 * time.Millisecond)
	reg.RecordSuccessFor("chain-a", "provA", "coder-best", "")
	if _, err := lkgpChat(t, eng); err != nil {
		t.Fatalf("Chat(auto/lkgp) after re-record error: %v", err)
	}
	if got := stubs["provA"].lastModel; got != "coder-best" {
		t.Errorf("served model after re-record = %q; want coder-best (chain-a re-recorded last)", got)
	}
}

// TestAutoLKGPEmptyRegistrySupplierErrorsEndToEnd: with the
// registry-backed supplier installed but NOTHING recorded, the
// supplier returns "" and auto/lkgp errors honestly with no
// provider called. This is distinct from D2's no-supplier test:
// it proves the wired (production-shape) supplier fails closed
// on an empty registry instead of inventing a last-good.
func TestAutoLKGPEmptyRegistrySupplierErrorsEndToEnd(t *testing.T) {
	eng, stubs := newLKGPTestEngine(t)
	installLKGPCatalog(t)
	reg := strategy.NewLKGPRegistry()
	installLKGPRegistrySupplier(t, reg)

	_, err := lkgpChat(t, eng)
	if err == nil {
		t.Fatal("Chat(auto/lkgp) with an empty registry: got nil error; want an honest error, never a silent fallback")
	}
	if !strings.Contains(err.Error(), "no last-known-good") {
		t.Errorf("error = %q; want it to name the missing last-known-good model", err.Error())
	}
	for p, s := range stubs {
		if s.chats != 0 {
			t.Errorf("provider %s chats = %d; want 0 (no provider may be called when lkgp is unrecorded)", p, s.chats)
		}
	}
}
