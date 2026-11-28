// A2-2: auto/fusion, auto/pipeline and auto/jev resolve to real
// multi-target panels.
package autorouter

import (
	"strings"
	"testing"
)

// TestResolveAutoComboPanels: the three combo variant ids are known
// ids and resolve to a multi-target panel (one model per provider),
// with Model left "" (the panel IS the decision) and the strategy name
// matching the id. Fails-when-off: without the flatVariant entries the
// ids are unknown (IsKnownAutoID false / resolve errors).
func TestResolveAutoComboPanels(t *testing.T) {
	catalog := []CatalogModel{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
		{Provider: "p3", Model: "m3"},
		{Provider: "p1", Model: "m1-alt"}, // same provider: must not add a second panel slot
	}
	for _, id := range []string{"auto/fusion", "auto/pipeline", "auto/jev"} {
		if !IsKnownAutoID(id) {
			t.Fatalf("IsKnownAutoID(%q) = false, want true", id)
		}
		res, err := ResolveAutoVariant(id, catalog, "")
		if err != nil {
			t.Fatalf("ResolveAutoVariant(%q): %v", id, err)
		}
		if len(res.Panel) < 2 {
			t.Fatalf("%s panel = %v, want >= 2 targets", id, res.Panel)
		}
		if len(res.Panel) != 3 {
			t.Fatalf("%s panel = %v, want one model per provider (3)", id, res.Panel)
		}
		if res.Model != "" {
			t.Fatalf("%s Model = %q, want empty (the panel is the decision)", id, res.Model)
		}
		want := strings.TrimPrefix(id, "auto/")
		if res.Strategy != want {
			t.Fatalf("%s Strategy = %q, want %q", id, res.Strategy, want)
		}
		if len(res.Ordered) != len(res.Panel) {
			t.Fatalf("%s Ordered = %v, want it to match Panel %v", id, res.Ordered, res.Panel)
		}
	}
}

// TestResolveAutoComboPanelSingleProvider: a one-provider catalog yields
// a one-target panel (the engine answers directly, no judge) — the
// resolver never invents a second target.
func TestResolveAutoComboPanelSingleProvider(t *testing.T) {
	res, err := ResolveAutoVariant("auto/fusion", []CatalogModel{{Provider: "p1", Model: "m1"}}, "")
	if err != nil {
		t.Fatalf("ResolveAutoVariant(auto/fusion): %v", err)
	}
	if len(res.Panel) != 1 {
		t.Fatalf("panel = %v, want exactly one target", res.Panel)
	}
}
