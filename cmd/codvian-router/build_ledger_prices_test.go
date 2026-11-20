package main

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func fptr(v float64) *float64 { return &v }

// M1.6: buildLedgerPrices must carry the configured cached-input
// rate into ledger.Prices — at pair level, model level, and the
// synced (models.dev) layer — so the ledger prices cached tokens
// at the cached rate, exactly like the dashboard does. A nil
// cached rate stays unset: the ledger then falls back to the full
// input price for cached tokens, never an invented rate.
func TestBuildLedgerPricesCarriesCachedInputRate(t *testing.T) {
	book := config.NewPriceBook()
	book.Pairs[config.PriceKey{Provider: "p1", Model: "model-a"}] = config.ModelPrice{
		InputUSDPer1M: fptr(2), OutputUSDPer1M: fptr(10), CachedInputUSDPer1M: fptr(0.25),
	}
	book.Pairs[config.PriceKey{Provider: "p1", Model: "model-b"}] = config.ModelPrice{
		InputUSDPer1M: fptr(4), OutputUSDPer1M: fptr(8), // no cached rate
	}
	book.Entries["model-c"] = config.ModelPrice{
		InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(6), CachedInputUSDPer1M: fptr(0.3),
	}
	book.SetSyncedPrices(map[config.PriceKey]config.SyncedPrice{
		{Provider: "p2", Model: "model-d"}: {Price: config.ModelPrice{
			InputUSDPer1M: fptr(5), OutputUSDPer1M: fptr(15), CachedInputUSDPer1M: fptr(0.5),
		}},
	})

	prices := buildLedgerPrices(book, provider.NewRegistry(nil))

	// Pair price carries the cached rate.
	pair := prices[ledger.PriceKey{Provider: "p1", Model: "model-a"}]
	if !pair.CachedKnown || pair.CachedPromptPerToken != 0.25e-6 {
		t.Errorf("pair model-a cached = %v (known=%v), want 0.25e-6 known", pair.CachedPromptPerToken, pair.CachedKnown)
	}
	if pair.PromptPerToken != 2e-6 || pair.CompletionPerToken != 10e-6 {
		t.Errorf("pair model-a = %v/%v, want 2e-6/10e-6", pair.PromptPerToken, pair.CompletionPerToken)
	}
	// No cached rate configured: unset, not zero-as-a-rate.
	noCached := prices[ledger.PriceKey{Provider: "p1", Model: "model-b"}]
	if noCached.CachedKnown {
		t.Errorf("pair model-b CachedKnown = true (want false: no cached rate configured)")
	}
	// Model-level fallback carries the cached rate.
	fallback := prices[ledger.PriceKey{Model: "model-c"}]
	if !fallback.CachedKnown || fallback.CachedPromptPerToken != 0.3e-6 {
		t.Errorf("model-level model-c cached = %v (known=%v), want 0.3e-6 known", fallback.CachedPromptPerToken, fallback.CachedKnown)
	}
	// Synced layer carries the cached rate.
	synced := prices[ledger.PriceKey{Provider: "p2", Model: "model-d"}]
	if !synced.CachedKnown || synced.CachedPromptPerToken != 0.5e-6 {
		t.Errorf("synced model-d cached = %v (known=%v), want 0.5e-6 known", synced.CachedPromptPerToken, synced.CachedKnown)
	}

	// End to end through the ledger price: 1000 prompt (800 cached),
	// 100 completion at the pair price == dashboard formula.
	want := float64(200)*2e-6 + float64(800)*0.25e-6 + float64(100)*10e-6
	if got := prices.CostWithCached("p1", "model-a", 1000, 100, 800); got != want {
		t.Errorf("CostWithCached via buildLedgerPrices = %v (want %v)", got, want)
	}
}

// M6.9b: buildLedgerPrices must carry the configured cache-write
// tier rates into ledger.Prices — at pair level, model level, and
// the synced (models.dev) layer — so the ledger prices each tier
// at the configured rate, which beats the multiplier fallback. A
// nil tier rate stays unset: the ledger then falls back to the
// Anthropic multiples (1.25x / 2x) for Anthropic-served Claude
// models, else the plain input price.
func TestBuildLedgerPricesCarriesCacheWriteTierRates(t *testing.T) {
	book := config.NewPriceBook()
	// Anthropic pair with tier rates deliberately NOT equal to
	// the 1.25x/2x multiples of the $3 input price (3.75/6.0),
	// so the cost proves the configured rates win over the
	// fallback instead of coinciding with it.
	book.Pairs[config.PriceKey{Provider: "anthropic", Model: "claude-sonnet-4-5"}] = config.ModelPrice{
		InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(15),
		CacheWrite5mUSDPer1M: fptr(4), CacheWrite1hUSDPer1M: fptr(7),
	}
	// Anthropic pair with NO tier rates: the fallback must apply.
	book.Pairs[config.PriceKey{Provider: "anthropic", Model: "claude-haiku-4-5"}] = config.ModelPrice{
		InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(15),
	}
	// Model-level fallback carries the tier rates.
	book.Entries["model-c"] = config.ModelPrice{
		InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(6),
		CacheWrite5mUSDPer1M: fptr(4), CacheWrite1hUSDPer1M: fptr(7),
	}
	// Synced layer carries the tier rates.
	book.SetSyncedPrices(map[config.PriceKey]config.SyncedPrice{
		{Provider: "p2", Model: "model-d"}: {Price: config.ModelPrice{
			InputUSDPer1M: fptr(5), OutputUSDPer1M: fptr(15),
			CacheWrite5mUSDPer1M: fptr(6), CacheWrite1hUSDPer1M: fptr(9),
		}},
	})

	prices := buildLedgerPrices(book, provider.NewRegistry(nil))

	// Pair price carries the tier rates.
	pair := prices[ledger.PriceKey{Provider: "anthropic", Model: "claude-sonnet-4-5"}]
	if !pair.CacheWrite5mKnown || pair.CacheWrite5mPerToken != 4e-6 {
		t.Errorf("pair 5m = %v (known=%v), want 4e-6 known", pair.CacheWrite5mPerToken, pair.CacheWrite5mKnown)
	}
	if !pair.CacheWrite1hKnown || pair.CacheWrite1hPerToken != 7e-6 {
		t.Errorf("pair 1h = %v (known=%v), want 7e-6 known", pair.CacheWrite1hPerToken, pair.CacheWrite1hKnown)
	}
	// Configured rates beat the fallback: 1000 prompt, 100 5m +
	// 200 1h writes, 0 completion. Configured: 700*3 + 100*4 +
	// 200*7 = 3900e-6. Fallback multiples would give 700*3 +
	// 100*3.75 + 200*6 = 3675e-6.
	want := float64(700)*3e-6 + float64(100)*4e-6 + float64(200)*7e-6
	if got := prices.CostWithCacheTiers("anthropic", "claude-sonnet-4-5", 1000, 0, 0, 100, 200); got != want {
		t.Errorf("CostWithCacheTiers configured = %v (want %v)", got, want)
	}
	// No tier rates configured: unset, and the Anthropic
	// fallback multiples price the tiers (1.25x / 2x of $3).
	noTier := prices[ledger.PriceKey{Provider: "anthropic", Model: "claude-haiku-4-5"}]
	if noTier.CacheWrite5mKnown || noTier.CacheWrite1hKnown {
		t.Errorf("pair haiku tier Known = %v/%v (want false/false)", noTier.CacheWrite5mKnown, noTier.CacheWrite1hKnown)
	}
	wantFallback := float64(700)*3e-6 + float64(100)*3.75e-6 + float64(200)*6e-6
	if got := prices.CostWithCacheTiers("anthropic", "claude-haiku-4-5", 1000, 0, 0, 100, 200); got != wantFallback {
		t.Errorf("CostWithCacheTiers fallback = %v (want %v)", got, wantFallback)
	}
	// Non-Anthropic provider, no tier rates: tiers price at the
	// plain input rate (1x), never a guessed multiple.
	book2 := config.NewPriceBook()
	book2.Pairs[config.PriceKey{Provider: "other", Model: "m"}] = config.ModelPrice{
		InputUSDPer1M: fptr(3), OutputUSDPer1M: fptr(6),
	}
	prices2 := buildLedgerPrices(book2, provider.NewRegistry(nil))
	if got := prices2.CostWithCacheTiers("other", "m", 1000, 0, 0, 100, 200); got != float64(1000)*3e-6 {
		t.Errorf("CostWithCacheTiers non-anthropic fallback = %v (want %v)", got, float64(1000)*3e-6)
	}
	// Model-level fallback carries the tier rates.
	fallback := prices[ledger.PriceKey{Model: "model-c"}]
	if !fallback.CacheWrite5mKnown || fallback.CacheWrite5mPerToken != 4e-6 || !fallback.CacheWrite1hKnown || fallback.CacheWrite1hPerToken != 7e-6 {
		t.Errorf("model-level tiers = %v/%v (known=%v/%v), want 4e-6/7e-6 known", fallback.CacheWrite5mPerToken, fallback.CacheWrite1hPerToken, fallback.CacheWrite5mKnown, fallback.CacheWrite1hKnown)
	}
	// Synced layer carries the tier rates.
	synced := prices[ledger.PriceKey{Provider: "p2", Model: "model-d"}]
	if !synced.CacheWrite5mKnown || synced.CacheWrite5mPerToken != 6e-6 || !synced.CacheWrite1hKnown || synced.CacheWrite1hPerToken != 9e-6 {
		t.Errorf("synced tiers = %v/%v (known=%v/%v), want 6e-6/9e-6 known", synced.CacheWrite5mPerToken, synced.CacheWrite1hPerToken, synced.CacheWrite5mKnown, synced.CacheWrite1hKnown)
	}
}
