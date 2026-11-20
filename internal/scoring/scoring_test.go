package scoring

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// threeCandidates is the shared 3-candidate set used by the per-mode
// ordering tests. The providers differ on every axis so each mode's
// weights produce a distinct expected order (computed by hand in the
// test comments from the documented normalisations).
//
//	fast: 100ms, 99% success, $30/1M, quality 0.8, quota 100
//	slow: 2000ms, 95% success, $1/1M, quality 0.6, quota 100
//	mid:  500ms, 90% success, $5/1M, quality 0.7, quota 50
func threeCandidates() ([]Candidate, map[Candidate]Signals) {
	cands := []Candidate{
		{ProviderID: "fast", Model: "m"},
		{ProviderID: "slow", Model: "m"},
		{ProviderID: "mid", Model: "m"},
	}
	sig := map[Candidate]Signals{
		{ProviderID: "fast", Model: "m"}: {LatencyMs: 100, SuccessRate: 0.99, CostUSDPer1M: 30, QualityScore: 0.8, QuotaHint: 100},
		{ProviderID: "slow", Model: "m"}: {LatencyMs: 2000, SuccessRate: 0.95, CostUSDPer1M: 1, QualityScore: 0.6, QuotaHint: 100},
		{ProviderID: "mid", Model: "m"}:  {LatencyMs: 500, SuccessRate: 0.90, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 50},
	}
	return cands, sig
}

func orderOf(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ProviderID)
	}
	return out
}

func TestModesWeightsSumToOne(t *testing.T) {
	for mode, w := range Modes {
		sum := w.Latency + w.Success + w.Cost + w.Quality + w.Quota
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("mode %q weights sum to %v, want 1", mode, sum)
		}
	}
	if len(Modes) != 6 {
		t.Errorf("Modes has %d entries, want 6 (balanced, fast, cheap, quality, reliable, latency-aware)", len(Modes))
	}
}

func TestRankPerMode(t *testing.T) {
	// Expected orders, computed by hand from the weights in Modes and
	// the normalisations in scoring.go (latency 1/(1+ms/1000), cost
	// 1/(1+$/10), quota hint/100):
	//
	// Balanced scores: fast 0.785, slow 0.723, mid 0.715
	// Fast scores:     fast 0.891, mid 0.700, slow 0.579
	// Cheap scores:    slow 0.836, mid 0.688, fast 0.557
	// Quality scores:  fast 0.841, mid 0.715, slow 0.699
	// Reliable scores: fast 0.929, slow 0.866, mid 0.725
	cases := []struct {
		mode Mode
		want []string
	}{
		{ModeBalanced, []string{"fast", "slow", "mid"}},
		{ModeFast, []string{"fast", "mid", "slow"}},
		{ModeCheap, []string{"slow", "mid", "fast"}},
		{ModeQuality, []string{"fast", "mid", "slow"}},
		{ModeReliable, []string{"fast", "slow", "mid"}},
	}
	for _, tc := range cases {
		cands, sig := threeCandidates()
		got := orderOf(Rank(cands, sig, tc.mode))
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("Rank mode %q = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestRankReliablePrefersSteadyOverFlakyFast(t *testing.T) {
	cands := []Candidate{
		{ProviderID: "flaky-fast", Model: "m"},
		{ProviderID: "steady-slow", Model: "m"},
		{ProviderID: "mid", Model: "m"},
	}
	sig := map[Candidate]Signals{
		// Fast and cheap but fails 40% of the time.
		{ProviderID: "flaky-fast", Model: "m"}: {LatencyMs: 50, SuccessRate: 0.60, CostUSDPer1M: 1, QualityScore: 0.9, QuotaHint: 100},
		// Slow but succeeds 99% of the time.
		{ProviderID: "steady-slow", Model: "m"}: {LatencyMs: 3000, SuccessRate: 0.99, CostUSDPer1M: 1, QualityScore: 0.9, QuotaHint: 100},
		{ProviderID: "mid", Model: "m"}:         {LatencyMs: 500, SuccessRate: 0.90, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 50},
	}
	got := orderOf(Rank(cands, sig, ModeReliable))
	want := []string{"steady-slow", "flaky-fast", "mid"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank ModeReliable = %v, want %v", got, want)
	}
	// The same set under ModeFast must prefer the flaky-fast one:
	// the modes genuinely disagree, they are not all one ranking.
	gotFast := orderOf(Rank(cands, sig, ModeFast))
	if gotFast[0] != "flaky-fast" {
		t.Errorf("Rank ModeFast = %v, want flaky-fast first", gotFast)
	}
}

func TestRankUnknownQualityNeverWinsModeQuality(t *testing.T) {
	// All else equal, a candidate with unknown quality (0) must rank
	// below known-good quality, and below known-mediocre quality too.
	cands := []Candidate{
		{ProviderID: "unknown", Model: "m"},
		{ProviderID: "known-good", Model: "m"},
		{ProviderID: "known-mid", Model: "m"},
	}
	base := Signals{LatencyMs: 500, SuccessRate: 0.95, CostUSDPer1M: 5, QuotaHint: 100}
	sig := map[Candidate]Signals{
		{ProviderID: "unknown", Model: "m"}:    {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: base.CostUSDPer1M, QualityScore: 0, QuotaHint: base.QuotaHint},
		{ProviderID: "known-good", Model: "m"}: {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: base.CostUSDPer1M, QualityScore: 0.85, QuotaHint: base.QuotaHint},
		{ProviderID: "known-mid", Model: "m"}:  {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: base.CostUSDPer1M, QualityScore: 0.6, QuotaHint: base.QuotaHint},
	}
	got := orderOf(Rank(cands, sig, ModeQuality))
	want := []string{"known-good", "known-mid", "unknown"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank ModeQuality with unknown quality = %v, want %v", got, want)
	}

	// Directly: the unknown-quality component must be below any
	// known-good quality value and equal to the documented constant.
	scorer := NewScorer(ModeQuality)
	unknownScore := scorer.Score(Candidate{ProviderID: "unknown"}, sig[Candidate{ProviderID: "unknown", Model: "m"}])
	goodScore := scorer.Score(Candidate{ProviderID: "known-good"}, sig[Candidate{ProviderID: "known-good", Model: "m"}])
	if !(unknownScore < goodScore) {
		t.Errorf("unknown-quality score %v should be below known-good %v", unknownScore, goodScore)
	}
	if got := qualityComponent(0); got != UnknownQualityScore {
		t.Errorf("qualityComponent(0) = %v, want UnknownQualityScore %v", got, UnknownQualityScore)
	}
	if UnknownQualityScore >= 0.5 {
		t.Errorf("UnknownQualityScore %v must be below neutral 0.5", UnknownQualityScore)
	}
}

func TestRankPreservesAllCandidates(t *testing.T) {
	cands := []Candidate{
		{ProviderID: "a", Model: "m1"},
		{ProviderID: "b", Model: "m2"},
		{ProviderID: "c", Model: "m3"},
		{ProviderID: "ghost", Model: "m4"}, // no signals entry at all
	}
	sig := map[Candidate]Signals{
		{ProviderID: "a", Model: "m1"}: {LatencyMs: 100, SuccessRate: 0.99, CostUSDPer1M: 1, QualityScore: 0.9, QuotaHint: 100},
		{ProviderID: "b", Model: "m2"}: {LatencyMs: 200, SuccessRate: 0.98, CostUSDPer1M: 2, QualityScore: 0.8, QuotaHint: 90},
		{ProviderID: "c", Model: "m3"}: {LatencyMs: 300, SuccessRate: 0.97, CostUSDPer1M: 3, QualityScore: 0.7, QuotaHint: 80},
		// "ghost" deliberately absent: no signals, no quota hint.
	}
	for _, mode := range []Mode{ModeBalanced, ModeFast, ModeCheap, ModeQuality, ModeReliable} {
		got := Rank(cands, sig, mode)
		if len(got) != len(cands) {
			t.Fatalf("mode %q: Rank returned %d candidates, want %d", mode, len(got), len(cands))
		}
		seen := map[string]int{}
		for _, c := range got {
			seen[c.ProviderID]++
		}
		for _, c := range cands {
			if seen[c.ProviderID] != 1 {
				t.Errorf("mode %q: provider %q appears %d times, want exactly once", mode, c.ProviderID, seen[c.ProviderID])
			}
		}
		// The entirely unknown candidate scores 0 and ranks last.
		if got[len(got)-1].ProviderID != "ghost" {
			t.Errorf("mode %q: last ranked = %q, want ghost (no signals) last", mode, got[len(got)-1].ProviderID)
		}
	}

	// Empty input stays empty; nil signals map is safe.
	if got := Rank(nil, nil, ModeBalanced); len(got) != 0 {
		t.Errorf("Rank(nil, nil) = %v, want empty", got)
	}
}

func TestRankStableOnTies(t *testing.T) {
	cands := []Candidate{
		{ProviderID: "one", Model: "m"},
		{ProviderID: "two", Model: "m"},
		{ProviderID: "three", Model: "m"},
	}
	same := Signals{LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 50}
	sig := map[Candidate]Signals{
		{ProviderID: "one", Model: "m"}:   same,
		{ProviderID: "two", Model: "m"}:   same,
		{ProviderID: "three", Model: "m"}: same,
	}
	got := orderOf(Rank(cands, sig, ModeBalanced))
	want := []string{"one", "two", "three"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tied Rank = %v, want input order preserved %v", got, want)
	}
}

func TestRankQuotaHintOnlyForUnknown(t *testing.T) {
	// A candidate with only a quota hint (no measured signals) scores
	// on quota alone: a full hint beats a known-but-terrible candidate
	// only via the small quota weight, and never via invented
	// latency/cost (an unknown candidate must not look instant/free).
	scorer := NewScorer(ModeBalanced)
	hintOnly := scorer.Score(Candidate{ProviderID: "h"}, Signals{QuotaHint: 100})
	// Quota weight in Balanced is 0.10, so the score is exactly 0.10.
	if math.Abs(hintOnly-0.10) > 1e-9 {
		t.Errorf("quota-hint-only score = %v, want 0.10 (quota weight only)", hintOnly)
	}
	empty := scorer.Score(Candidate{ProviderID: "e"}, Signals{})
	if empty != 0 {
		t.Errorf("empty signals score = %v, want 0", empty)
	}
}

func TestNewScorerUnknownModeDefaultsBalanced(t *testing.T) {
	a := NewScorer(Mode("nope"))
	b := NewScorer(ModeBalanced)
	c := Candidate{ProviderID: "x", Model: "m"}
	s := Signals{LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 50}
	if a == nil || b == nil {
		t.Fatal("NewScorer returned nil")
	}
	if a.Score(c, s) != b.Score(c, s) {
		t.Errorf("unknown mode score %v, want balanced score %v", a.Score(c, s), b.Score(c, s))
	}
}

func TestHookNilSignalsIsIdentity(t *testing.T) {
	chain := []router.Attempt{
		{ProviderID: "slow", Model: "m"},
		{ProviderID: "fast", Model: "m"},
		{ProviderID: "mid", Model: "m"},
	}
	hook := Hook(nil, ModeFast)
	got := hook(chain)
	if !reflect.DeepEqual(got, chain) {
		t.Errorf("Hook(nil) = %v, want chain unchanged %v", got, chain)
	}
}

func TestHookReorders(t *testing.T) {
	_, sig := threeCandidates()
	hook := Hook(func(providerID, model string) Signals { return sig[Candidate{ProviderID: providerID, Model: model}] }, ModeCheap)
	chain := []router.Attempt{
		{ProviderID: "fast", Model: "m"},
		{ProviderID: "mid", Model: "m"},
		{ProviderID: "slow", Model: "m"},
	}
	got := hook(chain)
	want := []router.Attempt{
		{ProviderID: "slow", Model: "m"},
		{ProviderID: "mid", Model: "m"},
		{ProviderID: "fast", Model: "m"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Hook ModeCheap = %v, want %v", got, want)
	}
	// The hook never drops or duplicates attempts, even with a
	// provider the signals func knows nothing about.
	hook2 := Hook(func(providerID, model string) Signals { return Signals{} }, ModeBalanced)
	chain2 := []router.Attempt{
		{ProviderID: "x", Model: "m1"},
		{ProviderID: "x", Model: "m2"}, // same provider, two models
		{ProviderID: "y", Model: "m1"},
	}
	got2 := hook2(chain2)
	if len(got2) != len(chain2) {
		t.Fatalf("Hook with empty signals returned %d attempts, want %d", len(got2), len(chain2))
	}
}

func TestHookRanksPerModelCost(t *testing.T) {
	// Cost is per (provider, model), not per provider: the same
	// provider serving a cheap model and an expensive model must
	// rank differently under cheapest. A provider-only lookup
	// would rank both by the provider's cheapest model.
	byModel := map[string]Signals{
		"cheap-model":     {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.5, QuotaHint: 100},
		"expensive-model": {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: 100, QualityScore: 0.5, QuotaHint: 100},
		"unknown-model":   {LatencyMs: 500, SuccessRate: 0.9, CostUSDPer1M: -1, QualityScore: 0.5, QuotaHint: 100},
	}
	hook := Hook(func(providerID, model string) Signals { return byModel[model] }, ModeCheap)
	chain := []router.Attempt{
		{ProviderID: "p", Model: "expensive-model"},
		{ProviderID: "p", Model: "unknown-model"},
		{ProviderID: "p", Model: "cheap-model"},
	}
	got := hook(chain)
	if len(got) != 3 {
		t.Fatalf("hook returned %d attempts, want 3", len(got))
	}
	// Cheapest first, unknown-price neutral in the middle (never
	// cheapest, never last on cost alone), expensive last.
	if got[0].Model != "cheap-model" || got[1].Model != "unknown-model" || got[2].Model != "expensive-model" {
		t.Errorf("cheapest per-model order = %v (want cheap, unknown, expensive)", got)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: the hook reorders a real router.Engine chain.
// ---------------------------------------------------------------------------

// failClient is a provider.Client that always fails Chat, so the engine
// walks the whole chain and the trace records the attempt order.
type failClient struct{}

func (failClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("failClient: always fails")
}

func (failClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return nil, errors.New("failClient: always fails")
}

func (failClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (failClient) TestKey(ctx context.Context) error                { return nil }

func TestHookReordersEngineEndToEnd(t *testing.T) {
	_, sig := threeCandidates()
	hook := Hook(func(providerID, model string) Signals { return sig[Candidate{ProviderID: providerID, Model: model}] }, ModeCheap)

	clients := map[string]provider.Client{
		"fast": failClient{},
		"slow": failClient{},
		"mid":  failClient{},
	}
	eng := router.NewEngine(clients, nil, router.Config{
		MaxAttempts: 5,
		HedgeDelay:  -1, // disable hedging: strictly sequential attempts
		OrderHook:   hook,
	})
	// Input chain order is deliberately the opposite of the cheap
	// ranking (fast first); the hook must reorder it.
	chain := []router.Attempt{
		{ProviderID: "fast", Model: "m"},
		{ProviderID: "mid", Model: "m"},
		{ProviderID: "slow", Model: "m"},
	}
	req := provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
	_, trace, err := eng.ChatWithChain(context.Background(), chain, req)
	if err == nil {
		t.Fatal("expected ChatWithChain to fail (all clients fail), got nil error")
	}
	if trace == nil {
		t.Fatal("expected a trace, got nil")
	}
	got := make([]string, 0, len(trace.Attempts))
	for _, att := range trace.Attempts {
		got = append(got, att.Provider)
	}
	// ModeCheap ranking is slow, mid, fast.
	want := []string{"slow", "mid", "fast"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("engine attempt order = %v, want %v (hook did not reorder)", got, want)
	}
	// LastTrace must agree with the returned trace.
	lt := eng.LastTrace()
	if lt == nil || len(lt.Attempts) != len(trace.Attempts) {
		t.Errorf("LastTrace = %v, want same attempts as returned trace", lt)
	}
}

func TestRankSameModelDifferentProvider(t *testing.T) {
	// The same model at two providers with different pair prices:
	// cheapest must rank on the (provider, model) pair, not the
	// model alone — and an unknown pair must never rank as free.
	cands := []Candidate{
		{ProviderID: "expensive-provider", Model: "shared"},
		{ProviderID: "unknown-provider", Model: "shared"},
		{ProviderID: "cheap-provider", Model: "shared"},
		{ProviderID: "free-provider", Model: "shared"},
	}
	base := Signals{LatencyMs: 100, SuccessRate: 0.9, QualityScore: 0.7, QuotaHint: 100}
	sig := map[Candidate]Signals{
		{ProviderID: "expensive-provider", Model: "shared"}: {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: 60, QualityScore: base.QualityScore, QuotaHint: base.QuotaHint},
		{ProviderID: "unknown-provider", Model: "shared"}:   {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: -1, QualityScore: base.QualityScore, QuotaHint: base.QuotaHint},
		{ProviderID: "cheap-provider", Model: "shared"}:     {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: 1, QualityScore: base.QualityScore, QuotaHint: base.QuotaHint},
		{ProviderID: "free-provider", Model: "shared"}:      {LatencyMs: base.LatencyMs, SuccessRate: base.SuccessRate, CostUSDPer1M: 0, QualityScore: base.QualityScore, QuotaHint: base.QuotaHint},
	}
	got := orderOf(Rank(cands, sig, ModeCheap))
	// Free ($0, cost component 1.0) first, cheap ($1) second,
	// unknown (neutral 0.5) third — never cheapest — expensive
	// ($60) last.
	want := []string{"free-provider", "cheap-provider", "unknown-provider", "expensive-provider"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Rank ModeCheap same model = %v, want %v", got, want)
	}
}

func TestRankModelsByBestPairCheapest(t *testing.T) {
	// Auto-router tier ordering: each model ranks by its
	// best-scoring (provider, model) pair. "shared" is $60 at one
	// provider but $1 at another, so it ranks by its $1 pair and
	// beats "mid" ($5) — a model-only price would have ranked it
	// by $60 (or by a blend) and put it last.
	price := map[Candidate]Signals{
		{ProviderID: "cheap-provider", Model: "shared"}:     {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 1, QualityScore: 0.7, QuotaHint: 100},
		{ProviderID: "expensive-provider", Model: "shared"}: {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 60, QualityScore: 0.7, QuotaHint: 100},
		{ProviderID: "only-provider", Model: "mid"}:         {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 5, QualityScore: 0.7, QuotaHint: 100},
		{ProviderID: "only-provider", Model: "pricey"}:      {LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 60, QualityScore: 0.7, QuotaHint: 100},
	}
	offers := func(model string) []Candidate {
		switch model {
		case "shared":
			return []Candidate{{ProviderID: "expensive-provider", Model: "shared"}, {ProviderID: "cheap-provider", Model: "shared"}}
		case "mid":
			return []Candidate{{ProviderID: "only-provider", Model: "mid"}}
		case "pricey":
			return []Candidate{{ProviderID: "only-provider", Model: "pricey"}}
		default:
			return nil
		}
	}
	signals := func(c Candidate) Signals { return price[c] }
	// Input order is deliberately worst-first: ranking must beat
	// list order.
	got := RankModelsByBestPair([]string{"pricey", "mid", "shared"}, offers, signals, ModeCheap)
	want := []string{"shared", "mid", "pricey"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RankModelsByBestPair ModeCheap = %v, want %v (shared must rank by its $1 pair)", got, want)
	}
	// A model with no offers is scored on its own signals (the
	// model-level fallback price) and kept, never dropped.
	got = RankModelsByBestPair([]string{"pricey", "unlisted"}, offers, func(c Candidate) Signals {
		if c.Model == "unlisted" {
			return Signals{LatencyMs: 100, SuccessRate: 0.9, CostUSDPer1M: 2, QualityScore: 0.7, QuotaHint: 100}
		}
		return price[c]
	}, ModeCheap)
	if len(got) != 2 || got[0] != "unlisted" || got[1] != "pricey" {
		t.Errorf("RankModelsByBestPair no-offers = %v, want [unlisted pricey]", got)
	}
	// Empty input stays empty; the result is a permutation.
	if got := RankModelsByBestPair(nil, offers, signals, ModeCheap); len(got) != 0 {
		t.Errorf("RankModelsByBestPair(nil) = %v, want empty", got)
	}
}
