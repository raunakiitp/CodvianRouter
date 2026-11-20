// B2-05 (parity tracker:501): the balanced pick must use the installed
// pool ranker — main.go's rankModels closure (16-factor ranking
// under DefaultWeights16), installed via SetAutoRanker. These tests
// install a ranker with a KNOWN order (reversal) and assert the
// dispatch serves the ranker's first model; uninstalling the ranker
// must change the outcome back to catalog order. A test that passes
// with the ranker uninstalled proves nothing about the wiring, so
// every test here FAILS if SetAutoRanker is a no-op.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func b2RankerCatalog() []autorouter.CatalogModel {
	return []autorouter.CatalogModel{
		{Provider: "provA", Model: "m1"},
		{Provider: "provB", Model: "m2"},
		{Provider: "provC", Model: "m3"},
	}
}

func b2RankerReq(id string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    id,
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

// b2DispatchBalanced installs catalog + ranker, dispatches auto/chat
// (Strategy "balanced" — the default pick that consults the ranker),
// and returns the served model. Callers uninstall via t.Cleanup.
func b2DispatchBalanced(t *testing.T, ranker autorouter.ModelRanker) string {
	t.Helper()
	eng := NewEngine(nil, nil, testConfig())
	SetAutoCatalog(func() []autorouter.CatalogModel { return b2RankerCatalog() })
	t.Cleanup(func() { SetAutoCatalog(nil) })
	SetAutoRanker(ranker)
	t.Cleanup(func() { SetAutoRanker(nil) })
	req, disp, handled := eng.dispatchAutoVariant(context.Background(), b2RankerReq("auto/chat"))
	if !handled {
		t.Fatalf("auto/chat not handled (catalog/ranker not installed?)")
	}
	if disp.Err != nil {
		t.Fatalf("auto/chat dispatch error: %v", disp.Err)
	}
	return req.Model
}

// reverseRanker is a ranker with a known, deterministic order: the
// exact reverse of its input. Any dispatch that consults it must
// serve the LAST catalog model; any dispatch that ignores it serves
// the FIRST.
func reverseRanker(tier autorouter.Tier, models []string) []string {
	out := append([]string(nil), models...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func TestB2RankerDrivesBalancedPick(t *testing.T) {
	got := b2DispatchBalanced(t, reverseRanker)
	if got != "m3" {
		t.Fatalf("auto/chat with reversing ranker served %q, want m3 (ranker's first, not catalog order)", got)
	}
}

func TestB2BalancedPickFallsBackToCatalogOrderWithoutRanker(t *testing.T) {
	got := b2DispatchBalanced(t, nil)
	if got != "m1" {
		t.Fatalf("auto/chat with no ranker served %q, want m1 (catalog order)", got)
	}
}

// TestB2RankerUninstalledAfterInstall is the closure test: the same
// engine serves the ranker's pick while installed and catalog order
// once uninstalled. If the dispatch cached the ranker or never
// consulted it, one of the two assertions fails.
func TestB2RankerUninstalledAfterInstall(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	SetAutoCatalog(func() []autorouter.CatalogModel { return b2RankerCatalog() })
	t.Cleanup(func() { SetAutoCatalog(nil) })

	SetAutoRanker(reverseRanker)
	req, disp, handled := eng.dispatchAutoVariant(context.Background(), b2RankerReq("auto/chat"))
	if !handled || disp.Err != nil {
		t.Fatalf("installed-ranker dispatch: handled=%v err=%v", handled, disp.Err)
	}
	if req.Model != "m3" {
		t.Fatalf("with ranker installed served %q, want m3", req.Model)
	}

	SetAutoRanker(nil)
	req, disp, handled = eng.dispatchAutoVariant(context.Background(), b2RankerReq("auto/chat"))
	if !handled || disp.Err != nil {
		t.Fatalf("uninstalled-ranker dispatch: handled=%v err=%v", handled, disp.Err)
	}
	if req.Model != "m1" {
		t.Fatalf("with ranker uninstalled served %q, want m1 (catalog order) — the dispatch must notice the uninstall", req.Model)
	}
}

// TestB2NonPermutationRankerIgnored pins rankedFirst's enforcement:
// a ranker that drops a model is ignored and catalog order stands.
func TestB2NonPermutationRankerIgnored(t *testing.T) {
	dropping := func(tier autorouter.Tier, models []string) []string {
		return models[:len(models)-1]
	}
	got := b2DispatchBalanced(t, dropping)
	if got != "m1" {
		t.Fatalf("auto/chat with a dropping ranker served %q, want m1 (non-permutation ranker must be ignored)", got)
	}
}
