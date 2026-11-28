// A2-2: auto:fusion / auto:pipeline / auto:jev resolve to REAL
// multi-target panels (with a judge).
//
// These drive the engine's public Chat / ChatStream with a live catalog
// installed, so the variant resolver builds the panel and the combo
// engine path runs it. FAILS-WHEN-OFF: remove the flatVariant entries
// (or the resolveComboPanel case) and auto/fusion etc. are no longer
// known ids — dispatchAutoVariant returns handled=false, the request
// falls through to the AutoRoute stub (fallback-coder, a single
// provider) and NO fan-out happens, so every "both/many providers
// dialled" assertion fails.
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// providersDialled counts how many distinct providers served at least
// one chat call.
func providersDialled(fakes map[string]*fakeClient) int {
	n := 0
	for _, fc := range fakes {
		if fc.chatCalls() > 0 {
			n++
		}
	}
	return n
}

// hasComboOutcome reports whether the trace carries a fan-out / judge
// outcome (a hedged loser or a JEV hold) — proof the panel actually
// fanned out rather than walking the chain sequentially.
func hasComboOutcome(trace *routetrace.RouteTrace) bool {
	if trace == nil {
		return false
	}
	for _, a := range trace.Attempts {
		if a.Outcome == "hedged_cancelled" || a.Outcome == "held_jev" {
			return true
		}
	}
	return false
}

func TestAutoFusionVariantResolvesToMultiTargetPanelWithJudge(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := autoChat(t, eng, "auto/fusion", "solve this across the panel")
	if err != nil {
		t.Fatalf("Chat(auto/fusion) error: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("Chat(auto/fusion) returned an empty response")
	}
	if got := providersDialled(fakes); got < 2 {
		t.Fatalf("auto/fusion dialled %d providers, want >= 2 (a real multi-target panel)", got)
	}
	routing, ok := eng.LastRouting()
	if !ok || routing.Strategy != "fusion" {
		t.Fatalf("LastRouting().Strategy = %q (ok=%v), want fusion", routing.Strategy, ok)
	}
	if !hasComboOutcome(eng.LastTrace()) {
		t.Fatalf("auto/fusion trace has no fan-out/judge outcome (no hedged_cancelled/held_jev): the panel did not run a judge")
	}
	if totalChats(fakes) < 2 {
		t.Fatalf("auto/fusion chat calls = %d, want >= 2 (every panel member dialled)", totalChats(fakes))
	}
}

func TestAutoPipelineVariantResolvesToSequentialStages(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := autoChat(t, eng, "auto/pipeline", "run the pipeline")
	if err != nil {
		t.Fatalf("Chat(auto/pipeline) error: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("Chat(auto/pipeline) returned an empty response")
	}
	if got := providersDialled(fakes); got < 2 {
		t.Fatalf("auto/pipeline dialled %d providers, want >= 2 (a real multi-stage pipeline)", got)
	}
	routing, ok := eng.LastRouting()
	if !ok || routing.Strategy != "pipeline" {
		t.Fatalf("LastRouting().Strategy = %q (ok=%v), want pipeline", routing.Strategy, ok)
	}
}

func TestAutoJevVariantResolvesToMultiTargetPanel(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := autoChat(t, eng, "auto/jev", "score these candidates")
	if err != nil {
		t.Fatalf("Chat(auto/jev) error: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("Chat(auto/jev) returned an empty response")
	}
	if got := providersDialled(fakes); got < 2 {
		t.Fatalf("auto/jev dialled %d providers, want >= 2 (a real multi-target panel)", got)
	}
	routing, ok := eng.LastRouting()
	if !ok || routing.Strategy != "jev" {
		t.Fatalf("LastRouting().Strategy = %q (ok=%v), want jev", routing.Strategy, ok)
	}
}

// TestAutoAxisFusionAliasResolvesToPanel: the "auto:fusion" alias
// rewrites the model to auto/fusion (through resolveAutoAlias) and the
// engine still runs the fusion panel — both the alias and the direct
// auto/fusion id resolve.
func TestAutoAxisFusionAliasResolvesToPanel(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	resp, err := autoChat(t, eng, "auto:fusion", "solve this via the alias")
	if err != nil {
		t.Fatalf("Chat(auto:fusion) error: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("Chat(auto:fusion) returned an empty response")
	}
	if got := providersDialled(fakes); got < 2 {
		t.Fatalf("auto:fusion dialled %d providers, want >= 2 (alias must resolve to the panel)", got)
	}
	routing, ok := eng.LastRouting()
	if !ok || routing.Strategy != "fusion" {
		t.Fatalf("LastRouting().Strategy = %q (ok=%v), want fusion", routing.Strategy, ok)
	}
}

// TestAutoFusionVariantStreamFansOutPanel: the stream path runs the same
// panel (combo streams replay the judged result), so auto/fusion on the
// stream path dials many providers and records the fusion strategy.
func TestAutoFusionVariantStreamFansOutPanel(t *testing.T) {
	eng, fakes := newAutoVariantTestEngine(t)
	installAutoCatalog(t, autoVariantCatalog())
	var sb strings.Builder
	resp, err := eng.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "auto/fusion",
		Messages: []provider.Message{{Role: "user", Content: "stream the panel"}},
	}, func(d string) error {
		sb.WriteString(d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream(auto/fusion) error: %v", err)
	}
	if resp == nil || resp.Content == "" {
		t.Fatal("ChatStream(auto/fusion) returned an empty response")
	}
	if got := providersDialled(fakes); got < 2 {
		t.Fatalf("auto/fusion stream dialled %d providers, want >= 2", got)
	}
	routing, ok := eng.LastRouting()
	if !ok || routing.Strategy != "fusion" {
		t.Fatalf("LastRouting().Strategy = %q (ok=%v), want fusion", routing.Strategy, ok)
	}
}
