package strategy

import (
	"math"
	"testing"
)

// Acceptance tests for internal/strategy/cache_affinity.go (M6.5).
//
// Every ordering test below uses an input order that DIFFERS from the
// expected output order, so each test FAILS if the affinity term is
// removed or the function returns its input unchanged (identity
// ordering). The toggle check is done explicitly in the M6.5 report:
// with cacheAffinityRank stubbed to (0, false, 0) the marker,
// warmth and fallback tests below all fail; with it restored they
// pass.

func cacheableReq() CacheAffinityRequest {
	return CacheAffinityRequest{PromptTokens: 2048}
}

func cacheTargets(providers ...string) []CacheAffinityTarget {
	out := make([]CacheAffinityTarget, 0, len(providers))
	for _, p := range providers {
		out = append(out, CacheAffinityTarget{AffinityTarget: AffinityTarget{Provider: p, Model: p + "/model"}})
	}
	return out
}

func cacheProviders(got []CacheAffinityTarget) []string {
	out := make([]string, 0, len(got))
	for _, t := range got {
		out = append(out, t.Provider)
	}
	return out
}

func TestCacheAffinityPrefersMarkerSupportingTarget(t *testing.T) {
	// Marker support is the only signal here (no warmth measured,
	// all fallback scores 0): anthropic must lead over openai and
	// google, which keep declared order behind it. If the marker
	// term is removed, the order stays openai, anthropic, google
	// and this test fails.
	in := cacheTargets("openai", "anthropic", "google")
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"anthropic", "openai", "google"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("OrderByCacheAffinity = %v, want %v", cacheProviders(got), want)
	}
}

func TestCacheAffinityMarkerCoversAllPrepareProviders(t *testing.T) {
	// Every provider cache.Prepare() annotates must win the marker
	// tiebreak. openrouter is deliberately out of the set: its
	// OpenAI-wire markers are applied by the provider client, not by
	// cache.Prepare, so it does not win the marker tiebreak here.
	in := cacheTargets("deepseek", "openrouter", "openai", "claude")
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"claude", "deepseek", "openrouter", "openai"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("OrderByCacheAffinity = %v, want %v", cacheProviders(got), want)
	}
}

func TestCacheAffinityWarmthBeatsMarker(t *testing.T) {
	// Measured warmth dominates marker capability: a warm
	// non-marker target (openai, warmth 0.9) leads a cold
	// marker-supporting one (anthropic, measured 0.0). If the
	// warmth term is removed, anthropic leads and this fails.
	in := []CacheAffinityTarget{
		{AffinityTarget: AffinityTarget{Provider: "anthropic", Model: "anthropic/m"}, Warmth: 0.0, WarmthKnown: true},
		{AffinityTarget: AffinityTarget{Provider: "openai", Model: "openai/m"}, Warmth: 0.9, WarmthKnown: true},
		{AffinityTarget: AffinityTarget{Provider: "google", Model: "google/m"}, Warmth: 0.5, WarmthKnown: true},
	}
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"openai", "google", "anthropic"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("OrderByCacheAffinity = %v, want %v", cacheProviders(got), want)
	}
}

func TestCacheAffinityUnknownWarmthIsNotMeasuredCold(t *testing.T) {
	// Unknown warmth (WarmthKnown false) ranks as 0 but must NOT
	// outrank a measured 0.0 on a tiebreak, and must not invent a
	// warmth: with equal warmth the marker decides. Here openai
	// (unknown) and anthropic (measured 0.0) tie on warmth 0, so
	// the marker-supporting anthropic leads — the unknown target
	// is not treated as warm, and the measured-cold target is not
	// penalised below the unknown one on warmth alone.
	in := []CacheAffinityTarget{
		{AffinityTarget: AffinityTarget{Provider: "openai", Model: "openai/m"}},
		{AffinityTarget: AffinityTarget{Provider: "anthropic", Model: "anthropic/m"}, Warmth: 0.0, WarmthKnown: true},
	}
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"anthropic", "openai"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("OrderByCacheAffinity = %v, want %v", cacheProviders(got), want)
	}
}

func TestCacheAffinityTiesFallBackToScoreThenDeclaredOrder(t *testing.T) {
	// Equal warmth (unknown) and equal marker support: the fallback
	// balanced score decides. If the score term is removed, the
	// declared order stands and this fails.
	in := []CacheAffinityTarget{
		{AffinityTarget: AffinityTarget{Provider: "openai", Model: "openai/a"}, Score: 0.2},
		{AffinityTarget: AffinityTarget{Provider: "deepseek", Model: "deepseek/b"}, Score: 0.8},
		{AffinityTarget: AffinityTarget{Provider: "google", Model: "google/c"}, Score: 0.5},
	}
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"deepseek", "google", "openai"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("OrderByCacheAffinity = %v, want %v", cacheProviders(got), want)
	}
	// All signals equal: declared order stands (stable sort).
	tied := cacheTargets("openai", "deepseek", "google")
	if got := OrderByCacheAffinity(tied, cacheableReq()); !sameStrings(cacheProviders(got), []string{"openai", "deepseek", "google"}) {
		t.Fatalf("tied targets reordered: got %v, want declared order", cacheProviders(got))
	}
}

func TestCacheAffinityNonCacheableRequestsUnaffected(t *testing.T) {
	// Each non-cacheable shape must return the declared order
	// unchanged even though anthropic would win the marker
	// tiebreak on a cacheable request.
	in := cacheTargets("openai", "anthropic", "google")
	want := []string{"openai", "anthropic", "google"}
	cases := map[string]CacheAffinityRequest{
		"tools":            {PromptTokens: 2048, HasTools: true},
		"temperature>0":    {PromptTokens: 2048, Temperature: 0.7, TemperatureSet: true},
		"short prompt":     {PromptTokens: 512},
		"at threshold-1":   {PromptTokens: MinCacheablePromptTokens - 1},
		"contract pinned":  {PromptTokens: 2048, ContractPinned: true},
		"zero prompt":      {PromptTokens: 0},
		"negative prompt":  {PromptTokens: -100},
		"tools+long+temp0": {PromptTokens: 8192, HasTools: true, Temperature: 0, TemperatureSet: true},
	}
	for name, req := range cases {
		if got := OrderByCacheAffinity(in, req); !sameStrings(cacheProviders(got), want) {
			t.Errorf("%s: OrderByCacheAffinity = %v, want declared order %v", name, cacheProviders(got), want)
		}
	}
	// Boundary: exactly the minimum prefix IS cacheable, and
	// temperature 0 explicitly set IS cacheable.
	if got := OrderByCacheAffinity(in, CacheAffinityRequest{PromptTokens: MinCacheablePromptTokens}); !sameStrings(cacheProviders(got), []string{"anthropic", "openai", "google"}) {
		t.Errorf("at-threshold prompt: got %v, want marker ordering", cacheProviders(got))
	}
	if got := OrderByCacheAffinity(in, CacheAffinityRequest{PromptTokens: 2048, Temperature: 0, TemperatureSet: true}); !sameStrings(cacheProviders(got), []string{"anthropic", "openai", "google"}) {
		t.Errorf("temp-0 prompt: got %v, want marker ordering", cacheProviders(got))
	}
}

func TestCacheAffinityPureAndDeterministic(t *testing.T) {
	in := []CacheAffinityTarget{
		{AffinityTarget: AffinityTarget{Provider: "openai", Model: "openai/a"}, Warmth: 0.9, WarmthKnown: true, Score: 0.1},
		{AffinityTarget: AffinityTarget{Provider: "anthropic", Model: "anthropic/b"}, Warmth: 0.4, WarmthKnown: true, Score: 0.9},
		{AffinityTarget: AffinityTarget{Provider: "google", Model: "google/c"}, Score: 0.5},
	}
	// The input slice must not be mutated.
	before := cacheProviders(in)
	first := cacheProviders(OrderByCacheAffinity(in, cacheableReq()))
	if !sameStrings(cacheProviders(in), before) {
		t.Fatalf("input mutated: got %v, want %v", cacheProviders(in), before)
	}
	// Same inputs, repeated runs (and interleaved with another
	// input), give the same output: no RNG, clock, I/O or global
	// state in the ordering.
	for i := 0; i < 50; i++ {
		other := OrderByCacheAffinity(cacheTargets("google", "anthropic"), cacheableReq())
		if !sameStrings(cacheProviders(other), []string{"anthropic", "google"}) {
			t.Fatalf("run %d: interleaved ordering changed: %v", i, cacheProviders(other))
		}
		if got := cacheProviders(OrderByCacheAffinity(in, cacheableReq())); !sameStrings(got, first) {
			t.Fatalf("run %d: ordering changed: got %v, want %v", i, got, first)
		}
	}
	// Empty and single-target inputs are returned unchanged.
	if got := OrderByCacheAffinity(nil, cacheableReq()); len(got) != 0 {
		t.Fatalf("nil input returned %d targets, want 0", len(got))
	}
	single := cacheTargets("anthropic")
	if got := OrderByCacheAffinity(single, cacheableReq()); len(got) != 1 || got[0].Provider != "anthropic" {
		t.Fatalf("single target: got %v", cacheProviders(got))
	}
}

func TestCacheAffinityMarkerNormalization(t *testing.T) {
	for _, p := range []string{"anthropic", "Anthropic", "ANTHROPIC", " anthropic ", "claude", "Claude"} {
		if !SupportsPromptCacheMarker(p) {
			t.Errorf("SupportsPromptCacheMarker(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"", "openai", "google", "gemini", "deepseek", "openrouter", "OpenRouter", "anthropicx", "xanthropic"} {
		if SupportsPromptCacheMarker(p) {
			t.Errorf("SupportsPromptCacheMarker(%q) = true, want false", p)
		}
	}
}

func TestCacheAffinityWarmthClamped(t *testing.T) {
	// Out-of-range warmth is clamped to 0..1, never rejected: 1.5
	// behaves as 1.0 and -0.5 as 0.0. NaN warmth/score behave as 0.
	in := []CacheAffinityTarget{
		{AffinityTarget: AffinityTarget{Provider: "google", Model: "google/m"}, Warmth: 0.5, WarmthKnown: true},
		{AffinityTarget: AffinityTarget{Provider: "openai", Model: "openai/m"}, Warmth: 1.5, WarmthKnown: true},
		{AffinityTarget: AffinityTarget{Provider: "deepseek", Model: "deepseek/m"}, Warmth: -0.5, WarmthKnown: true},
		{AffinityTarget: AffinityTarget{Provider: "anthropic", Model: "anthropic/m"}, Warmth: math.NaN(), WarmthKnown: true, Score: math.NaN()},
	}
	got := OrderByCacheAffinity(in, cacheableReq())
	if want := []string{"openai", "google", "anthropic", "deepseek"}; !sameStrings(cacheProviders(got), want) {
		t.Fatalf("clamped order = %v, want %v", cacheProviders(got), want)
	}
}
