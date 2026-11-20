// B2-34 (PARITY.md:534): with OrderedExplicit on, dispatch order is
// preserved — no quota-hint sort, no strategy hook, no hedge may
// re-rank a dispatched multi-model chain (auto/chaos panel, thrifty
// ladder). These tests drive the real chat path with a panel whose
// order a quota-hint sort AND a reversing strategy hook would both
// invert, and assert the panel order holds only when the flag is on.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func b2ChaosCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provA", Model: "m1"},
		{Provider: "provB", Model: "m2"},
		{Provider: "provC", Model: "m3"},
	}
}

func b2ChaosChain() []Attempt {
	return []Attempt{
		{ProviderID: "provA", Model: "m1"},
		{ProviderID: "provB", Model: "m2"},
		{ProviderID: "provC", Model: "m3"},
	}
}

func b2ChaosClients() map[string]provider.Client {
	return map[string]provider.Client{
		"provA": okClient("from-a"),
		"provB": okClient("from-b"),
		"provC": okClient("from-c"),
	}
}

func b2ChaosReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m1",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

// TestB2ChaosDispatchReturnsExplicitPanel pins the dispatch half:
// auto/chaos returns the panel in catalog order with OrderedExplicit.
func TestB2ChaosDispatchReturnsExplicitPanel(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	SetAutoCatalog(func() []autorouter.CatalogModel { return b2ChaosCatalog() })
	t.Cleanup(func() { SetAutoCatalog(nil) })
	// The request must name the variant id (not a panel model) for
	// dispatch to handle it.
	chaosReq := provider.ChatRequest{
		Model:    "auto/chaos",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
	req, disp, handled := eng.dispatchAutoVariant(context.Background(), chaosReq)
	if !handled {
		t.Fatalf("auto/chaos not handled")
	}
	if disp.Err != nil {
		t.Fatalf("auto/chaos dispatch error: %v", disp.Err)
	}
	if disp.Strategy != "chaos-mode" {
		t.Fatalf("chaos strategy = %q, want chaos-mode", disp.Strategy)
	}
	want := []string{"m1", "m2", "m3"}
	if len(disp.Ordered) != len(want) {
		t.Fatalf("chaos panel = %v, want %v", disp.Ordered, want)
	}
	for i, m := range want {
		if disp.Ordered[i] != m {
			t.Fatalf("chaos panel = %v, want %v (catalog order, one per provider)", disp.Ordered, want)
		}
	}
	if !disp.OrderedExplicit {
		t.Fatalf("chaos OrderedExplicit = false, want true")
	}
	if req.Model != "m1" {
		t.Fatalf("chaos request model = %q, want m1 (first panel model)", req.Model)
	}
}

// TestB2ExplicitOrderSurvivesQuotaHintSort: quota hints that would
// sort the chain C,B,A must not move the panel when the flag is on.
func TestB2ExplicitOrderSurvivesQuotaHintSort(t *testing.T) {
	cfg := testConfig()
	cfg.QuotaHint = func(providerID string) int {
		switch providerID {
		case "provC":
			return 3
		case "provB":
			return 2
		default:
			return 1
		}
	}
	eng := NewEngine(b2ChaosClients(), nil, cfg)

	resp, _, err := eng.chatWithChainStrategy(context.Background(), b2ChaosChain(), b2ChaosReq(), "priority", true)
	if err != nil {
		t.Fatalf("explicit-order chat: %v", err)
	}
	if resp.Content != "from-a" {
		t.Fatalf("explicit-order chat served %q, want from-a (panel order preserved despite quota hints)", resp.Content)
	}

	// Control: the same chain with the flag off IS re-sorted by the
	// hints, so C serves first — proving the flag is what holds the
	// order, not the chain shape.
	resp, _, err = eng.chatWithChainStrategy(context.Background(), b2ChaosChain(), b2ChaosReq(), "priority", false)
	if err != nil {
		t.Fatalf("non-explicit chat: %v", err)
	}
	if resp.Content != "from-c" {
		t.Fatalf("non-explicit chat served %q, want from-c (quota-hint sort must win when the flag is off)", resp.Content)
	}
}

// TestB2ExplicitOrderSurvivesStrategyHook: a reversing strategy hook
// must not move the panel when the flag is on.
func TestB2ExplicitOrderSurvivesStrategyHook(t *testing.T) {
	cfg := testConfig()
	cfg.StrategyHook = func(strategy string) func([]Attempt) []Attempt {
		return func(chain []Attempt) []Attempt {
			out := append([]Attempt(nil), chain...)
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
			return out
		}
	}
	eng := NewEngine(b2ChaosClients(), nil, cfg)

	resp, _, err := eng.chatWithChainStrategy(context.Background(), b2ChaosChain(), b2ChaosReq(), "priority", true)
	if err != nil {
		t.Fatalf("explicit-order chat: %v", err)
	}
	if resp.Content != "from-a" {
		t.Fatalf("explicit-order chat served %q, want from-a (panel order preserved despite reversing hook)", resp.Content)
	}

	// Control: with the flag off the reversing hook wins, so C serves
	// first — proving the flag is what holds the order.
	resp, _, err = eng.chatWithChainStrategy(context.Background(), b2ChaosChain(), b2ChaosReq(), "priority", false)
	if err != nil {
		t.Fatalf("non-explicit chat: %v", err)
	}
	if resp.Content != "from-c" {
		t.Fatalf("non-explicit chat served %q, want from-c (strategy hook must win when the flag is off)", resp.Content)
	}
}
