package autorouter

import (
	"testing"
)

// G10 Area 12 row 9: auto/latency and auto/latency-aware resolve to
// the latency-aware strategy and pick the lowest-latency VIABLE
// model. On the shared fixedCatalog the fastest model overall is
// deepseek/deepseek-chat (300 ms, quality 0.60 — viable); the
// 500 ms zhipu model has quality 0.40 and would be excluded even
// if it were fastest.
func TestAutoVariantLatencyPicksLowestViableLatency(t *testing.T) {
	for _, id := range []string{"auto/latency", "auto/latency-aware"} {
		res := resolve(t, id, fixedCatalog(), "")
		if res.Strategy != "latency-aware" {
			t.Fatalf("%s: strategy = %q, want latency-aware", id, res.Strategy)
		}
		if res.Model != "deepseek/deepseek-chat" {
			t.Fatalf("%s: model = %q, want deepseek/deepseek-chat (300ms, lowest viable latency)", id, res.Model)
		}
	}
}

func TestAutoVariantLatencySkipsMostlyFailingFastest(t *testing.T) {
	cat := []CatalogModel{
		{Provider: "a", Model: "a/fast-dead", LatencyMs: 50, HasLatency: true, Quality: 0.10, HasQuality: true},
		{Provider: "b", Model: "b/slow-ok", LatencyMs: 400, HasLatency: true, Quality: 0.95, HasQuality: true},
		{Provider: "c", Model: "c/mid-unknown", LatencyMs: 200, HasLatency: true},
	}
	res := resolve(t, "auto/latency", cat, "")
	// Unknown quality fails open (viable): 200 ms beats 400 ms,
	// and the 50 ms mostly-failing model is excluded.
	if res.Model != "c/mid-unknown" {
		t.Fatalf("model = %q, want c/mid-unknown (200ms viable; 50ms model is mostly-failing)", res.Model)
	}
}
