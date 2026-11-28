// Per-request "strategy" body field (A2-1) end-to-end tests.
//
// This file is package router_test (external) so it can import
// internal/strategy: strategy imports router, so an internal test in
// package router would be an import cycle. It drives the engine only
// through its exported API (router.NewEngine → Engine.Chat /
// Engine.ChatStream) and proves the request-body "strategy" field is
// wired through effectiveStrategy.
//
// FAILS-WHEN-OFF: revert the resolveAutoRequest change that reads
// req.Strategy and these tests fail — an unknown strategy then falls
// back to the configured default ("priority"), so LastRouting().Strategy
// is "priority" (not the requested name) and a combo strategy like
// fusion runs the SEQUENTIAL path (only the first provider is dialled,
// no fan-out), which every fan-out assertion below catches.
package router_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// strategyStubClient is a minimal provider.Client that records how
// many Chat / ChatStream calls it served and returns a fixed answer.
type strategyStubClient struct {
	mu      sync.Mutex
	chats   int
	streams int
	content string
}

func (c *strategyStubClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.chats++
	c.mu.Unlock()
	return &provider.ChatResponse{Content: c.content}, nil
}

func (c *strategyStubClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.streams++
	c.mu.Unlock()
	if onDelta != nil {
		if err := onDelta(c.content); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: c.content}, nil
}

func (c *strategyStubClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *strategyStubClient) TestKey(ctx context.Context) error                { return nil }

func (c *strategyStubClient) calls() (chat, stream int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats, c.streams
}

// newStrategyTestEngine builds an engine whose ChainFor returns the
// SAME TWO providers for every model — so a combo strategy fans out to
// two distinct targets and a sequential strategy walks them in order.
func newStrategyTestEngine() (eng *router.Engine, p1, p2 *strategyStubClient) {
	p1 = &strategyStubClient{content: "short"}
	p2 = &strategyStubClient{content: "this is a much longer panel answer the judge must prefer"}
	clients := map[string]provider.Client{"p1": p1, "p2": p2}
	chainFor := func(model string) []router.Attempt {
		return []router.Attempt{
			{ProviderID: "p1", Model: model},
			{ProviderID: "p2", Model: model},
		}
	}
	eng = router.NewEngine(clients, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor:         chainFor,
	})
	return eng, p1, p2
}

func strategyReq(name string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
		Strategy: name,
	}
}

// TestRequestBodyStrategyRoutesEveryNameViaChatAndStream drives the
// REAL chat and stream paths for every strategy name internal/strategy
// recognises (canonical names plus aliases) and asserts the effective
// strategy the engine recorded is that name — on both paths.
func TestRequestBodyStrategyRoutesEveryNameViaChatAndStream(t *testing.T) {
	names := make([]string, 0, len(strategy.All)+8)
	for _, s := range strategy.All {
		names = append(names, string(s))
	}
	// Aliases the server validator accepts (internal/strategy aliases).
	names = append(names, "cheap", "fast", "reliable", "latency", "eco", "custom", "sla", "budget")

	for _, name := range names {
		name := name
		t.Run("chat/"+name, func(t *testing.T) {
			eng, _, _ := newStrategyTestEngine()
			resp, err := eng.Chat(context.Background(), strategyReq(name))
			if err != nil {
				t.Fatalf("Chat(strategy=%q): %v", name, err)
			}
			if resp == nil || resp.Content == "" {
				t.Fatalf("Chat(strategy=%q): empty response", name)
			}
			routing, ok := eng.LastRouting()
			if !ok || routing.Strategy != name {
				t.Fatalf("Chat(strategy=%q): LastRouting.Strategy = %q (ok=%v), want %q", name, routing.Strategy, ok, name)
			}
		})
		t.Run("stream/"+name, func(t *testing.T) {
			eng, _, _ := newStrategyTestEngine()
			var sb strings.Builder
			resp, err := eng.ChatStream(context.Background(), strategyReq(name), func(d string) error {
				sb.WriteString(d)
				return nil
			})
			if err != nil {
				t.Fatalf("ChatStream(strategy=%q): %v", name, err)
			}
			if resp == nil {
				t.Fatalf("ChatStream(strategy=%q): nil response", name)
			}
			if sb.Len() == 0 {
				t.Fatalf("ChatStream(strategy=%q): no deltas delivered", name)
			}
			routing, ok := eng.LastRouting()
			if !ok || routing.Strategy != name {
				t.Fatalf("ChatStream(strategy=%q): LastRouting.Strategy = %q (ok=%v), want %q", name, routing.Strategy, ok, name)
			}
		})
	}
}

// TestRequestBodyStrategyFusionFansOutRealPanel proves a single request
// that asks for strategy=fusion runs a REAL multi-target panel: BOTH
// providers are dialled (one chat call each) and the judge's winner
// (the longer answer) is returned, on both the chat and stream paths.
// Without the field wiring the strategy is "priority", which walks the
// chain sequentially — only p1 is dialled — so this fails.
func TestRequestBodyStrategyFusionFansOutRealPanel(t *testing.T) {
	eng, p1, p2 := newStrategyTestEngine()
	resp, err := eng.Chat(context.Background(), strategyReq("fusion"))
	if err != nil {
		t.Fatalf("Chat(strategy=fusion): %v", err)
	}
	c1, _ := p1.calls()
	c2, _ := p2.calls()
	// Both providers are dialled (a REAL multi-target panel). The
	// first panel member is also the default fusion judge model
	// (A2-3b), so it is dialled once for the panel and once more for
	// the judge's winner pick.
	if c1 != 2 || c2 != 1 {
		t.Fatalf("fusion panel calls p1=%d p2=%d, want p1=2 (panel member + judge pick) and p2=1 (panel member)", c1, c2)
	}
	if resp.Content != p2.content {
		t.Fatalf("fusion winner = %q, want the judge's pick (p2's longer answer) %q", resp.Content, p2.content)
	}
	if routing, ok := eng.LastRouting(); !ok || routing.Strategy != "fusion" {
		t.Fatalf("LastRouting.Strategy = %q (ok=%v), want fusion", routing.Strategy, ok)
	}

	engS, s1, s2 := newStrategyTestEngine()
	var sb strings.Builder
	sResp, err := engS.ChatStream(context.Background(), strategyReq("fusion"), func(d string) error {
		sb.WriteString(d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream(strategy=fusion): %v", err)
	}
	sc1, _ := s1.calls()
	sc2, _ := s2.calls()
	// Both providers are dialled on the STREAM path too (a REAL
	// multi-target panel); the first panel member is also the default
	// fusion judge model (A2-3b), so it is dialled once for the panel
	// and once more for the judge's winner pick.
	if sc1 != 2 || sc2 != 1 {
		t.Fatalf("fusion stream panel calls p1=%d p2=%d, want p1=2 (panel member + judge pick) and p2=1 (panel member)", sc1, sc2)
	}
	if sb.String() != s2.content || sResp == nil || sResp.Content != s2.content {
		t.Fatalf("fusion stream = %q (resp %+v), want judge winner %q", sb.String(), sResp, s2.content)
	}
}

// TestRequestBodyStrategyBeatsAutoAxisAlias proves the new body field
// wins over the "auto:<axis>" model alias: the model asks for
// auto:cheapest (a sequential alias) while the body asks for fusion, and
// the engine runs the fusion panel (both providers dialled, strategy
// recorded as fusion). The alias still rewrites the model underneath
// (auto/cheap), so a request can name both a virtual model and a
// per-request strategy.
func TestRequestBodyStrategyBeatsAutoAxisAlias(t *testing.T) {
	eng, p1, p2 := newStrategyTestEngine()
	req := strategyReq("fusion")
	req.Model = "auto:cheapest"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat(model=auto:cheapest, strategy=fusion): %v", err)
	}
	c1, _ := p1.calls()
	c2, _ := p2.calls()
	// Both providers are dialled (a REAL multi-target panel); the
	// first panel member is also the default fusion judge model
	// (A2-3b), so it is dialled once for the panel and once more for
	// the judge's winner pick.
	if c1 != 2 || c2 != 1 {
		t.Fatalf("panel calls p1=%d p2=%d, want p1=2 (panel member + judge pick) and p2=1 (panel member) (body strategy=fusion beat the auto:cheapest alias)", c1, c2)
	}
	if resp.Content != p2.content {
		t.Fatalf("winner = %q, want the fusion judge's pick %q", resp.Content, p2.content)
	}
	if routing, ok := eng.LastRouting(); !ok || routing.Strategy != "fusion" {
		t.Fatalf("LastRouting.Strategy = %q (ok=%v), want fusion", routing.Strategy, ok)
	}
}
