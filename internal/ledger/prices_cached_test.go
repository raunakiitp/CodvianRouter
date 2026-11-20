package ledger

import (
	"context"
	"math"
	"testing"
	"time"
)

// M1.6 golden test: a request with provider-cached prompt tokens must
// cost the same in the ledger as on the dashboard. The dashboard
// (internal/server cacheComboEconomics) prices cached tokens at the
// configured cached-input rate; the ledger used to charge them at the
// full input rate, so the two surfaces disagreed on spend for the
// same request.
//
// Dashboard formula for one request (all rates USD per token):
//
//	(prompt - cached) * input + cached * cachedRate + completion * output
func dashboardCost(prompt, completion, cached int, input, output, cachedRate float64) float64 {
	return float64(prompt-cached)*input + float64(cached)*cachedRate + float64(completion)*output
}

func approxEqual(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("cost = %v (want %v)", got, want)
	}
}

func TestPricesCostWithCached(t *testing.T) {
	p := Prices{
		{Provider: "p1", Model: "m"}:              {PromptPerToken: 2e-6, CompletionPerToken: 10e-6, CachedPromptPerToken: 0.25e-6, CachedKnown: true},
		{Model: "m-fallback"}:                     {PromptPerToken: 2e-6, CompletionPerToken: 10e-6, CachedPromptPerToken: 0.25e-6, CachedKnown: true},
		{Provider: "p1", Model: "no-cached-rate"}: {PromptPerToken: 2e-6, CompletionPerToken: 10e-6},
		{Provider: "p1", Model: "free-cache"}:     {PromptPerToken: 2e-6, CompletionPerToken: 10e-6, CachedPromptPerToken: 0, CachedKnown: true},
	}
	// 1000 prompt (800 cached), 100 completion: dashboard formula.
	want := dashboardCost(1000, 100, 800, 2e-6, 10e-6, 0.25e-6)
	approxEqual(t, p.CostWithCached("p1", "m", 1000, 100, 800), want)
	// Model-level fallback carries its cached rate too.
	approxEqual(t, p.CostWithCached("other", "m-fallback", 1000, 100, 800), want)
	// No cached rate configured: cached tokens fall back to the full
	// input price (the old behaviour — no rate is ever invented).
	approxEqual(t, p.CostWithCached("p1", "no-cached-rate", 1000, 100, 800),
		dashboardCost(1000, 100, 800, 2e-6, 10e-6, 2e-6))
	// Explicitly free cache reads (rate 0, known) charge $0 for the
	// cached leg — distinct from "no cached rate configured".
	approxEqual(t, p.CostWithCached("p1", "free-cache", 1000, 100, 800),
		dashboardCost(1000, 100, 800, 2e-6, 10e-6, 0))
	// Cached tokens can never exceed the prompt split: clamp.
	approxEqual(t, p.CostWithCached("p1", "m", 1000, 100, 5000), dashboardCost(1000, 100, 1000, 2e-6, 10e-6, 0.25e-6))
	// Zero cached tokens == the old Cost path.
	if got := p.CostWithCached("p1", "m", 1000, 100, 0); got != p.Cost("p1", "m", 1000, 100) {
		t.Errorf("CostWithCached(cached=0) = %v, Cost = %v (want equal)", got, p.Cost("p1", "m", 1000, 100))
	}
	// Unknown pair: 0, never guessed.
	if got := p.CostWithCached("p1", "missing", 1000, 100, 800); got != 0 {
		t.Errorf("unknown pair CostWithCached = %v (want 0)", got)
	}
}

func TestRecordUsageCachedTokensGolden(t *testing.T) {
	ctx := context.Background()
	l := openTestLedger(t)
	prices := Prices{
		{Provider: "p1", Model: "model-a"}: {PromptPerToken: 2e-6, CompletionPerToken: 10e-6, CachedPromptPerToken: 0.25e-6, CachedKnown: true},
	}
	// 1000 prompt tokens, 800 of them served from the provider cache,
	// 100 completion tokens. Ledger cost must equal the dashboard
	// cost for the same request.
	want := dashboardCost(1000, 100, 800, 2e-6, 10e-6, 0.25e-6)
	cost, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "key-1", Provider: "p1", Model: "model-a",
		PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 800,
		SplitKnown: true, OK: true,
	}, prices)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	approxEqual(t, cost, want)
	spent, err := l.SpendFor(ctx, "key-1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("SpendFor: %v", err)
	}
	approxEqual(t, spent, want)
}
