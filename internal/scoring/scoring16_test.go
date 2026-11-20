// Acceptance tests for the 16-factor auto scorer (scoring16.go).
//
// Written FIRST, before the implementation (M3 builder S3).
// Every test name matches -run 'TestSixteen|TestModePack|TestFactor'.
//
// Design: each of the 11 new factors has a direct value test AND an
// ordering test under a single-factor weight set, so the suite FAILS if
// any factor is removed or stubbed to a constant — including the two
// factors whose REFERENCE weight is 0 (cacheAffinity,
// resetWindowAffinity): under the reference tables they would never
// vote, but this router's packs give them real weights (RT-4), so
// TestModePackCacheWarmthVotes pins that they vote under every pack.
// The mode-pack test uses one discriminating fixture on which every
// pack must produce a different full ordering (hand-computed below).
package scoring

import (
	"math"
	"reflect"
	"sort"
	"testing"
)

// sixteenBase is the shared fixture: every factor sits at a fixed,
// mid-range value except the ones a test deliberately varies.
// With MaxCostPer1M=10 and CostPer1M=10 every candidate's costInv is 0;
// with MaxLatencyMs=1000 and LatencyP95Ms=1000 every latencyInv is 0;
// stddev 100/100 gives stability 0; tags ["code"] vs ["chat"] give
// taskFit 0; standard tier with no reset interval gives tierPriority
// 0.33*0.8 = 0.264; cheap/cheap gives tierAffinity 1.0; specificity 30
// on a cheap model gives 0.9; 1000/8000 context gives 0.875;
// cache 0.5; no session gives 0.5; reset in 7d gives
// 1-604800/2592000 = 0.7667; 3 connections give (3-1)/10 = 0.2.
func sixteenBase() SixteenInputs {
	return SixteenInputs{
		QuotaRemainingPct:      50,
		LatencyP95Ms:           1000,
		MaxLatencyMs:           1000,
		CostPer1M:              10,
		MaxCostPer1M:           10,
		SuccessRate:            0.9,
		QualityScore:           0.5,
		LatencyStdDevMs:        100,
		MaxStdDevMs:            100,
		Breaker:                BreakerClosed,
		TaskTags:               []string{"code"},
		ModelTags:              []string{"chat"},
		Tier:                   AccountTierStandard,
		QuotaResetIntervalSecs: 0,
		ModelTier:              ModelTierCheap,
		RecommendedMinTier:     ModelTierCheap,
		SpecificityScore:       30,
		ContextTokens:          1000,
		ModelContextTokens:     8000,
		CachedPromptFraction:   0.5,
		SessionProvider:        "",
		SessionModel:           "",
		ResetInSeconds:         604800,
		ConnectionCount:        3,
	}
}

func sixteenCand(id string) Candidate { return Candidate{ProviderID: id, Model: "m"} }

// order16 returns the provider IDs of Rank16 in order.
func order16(cands []Candidate, inputs map[Candidate]SixteenInputs, w Weights16) []string {
	ranked := Rank16(cands, inputs, w)
	out := make([]string, len(ranked))
	for i, c := range ranked {
		out[i] = c.ProviderID
	}
	return out
}

// --- weights ---------------------------------------------------------------

func TestSixteenDefaultWeightsAreOmniRouteRescaledForLiveFactors(t *testing.T) {
	// G19 Area 1, gap-track RT-4: OmniRoute's DEFAULT_WEIGHTS
	// (scoring.ts) with cacheAffinity, resetWindowAffinity and
	// reliability given real weights (0.0476 / 0.0476 / 0.03 —
	// the sibling factors' weights) and the other thirteen
	// rescaled proportionally (×0.8748), so the set still sums
	// to 1.0. The verbatim OmniRoute table is deliberately NOT
	// preserved here: the three factors are now fed from live
	// signals (ledger cache warmth, ledger reset roll-off,
	// observed success rate), and zeroing them would throw
	// real data away. ModePacks carry the same divergence (see
	// TestSixteenModePackTablesRescaledForLiveFactors).
	want := Weights16{
		Quota: 0.12500892, Health: 0.1404054, CostInv: 0.12500892, LatencyInv: 0.09998964,
		TaskFit: 0.06665976, Stability: 0.04164048, TierPriority: 0.04164048,
		TierAffinity: 0.04164048, SpecificityMatch: 0.04164048, ContextAffinity: 0.04164048,
		CacheAffinity: 0.0476, SessionAffinity: 0.04164048, ResetWindowAffinity: 0.0476,
		ConnectionDensity: 0.04164048, Quality: 0.026244, Reliability: 0.03,
	}
	if DefaultWeights16 != want {
		t.Fatalf("DefaultWeights16 = %+v, want %+v", DefaultWeights16, want)
	}
}

func TestClassifyPriceTierThresholds(t *testing.T) {
	// OmniRoute tierResolver.ts / tierConfig.ts defaults:
	// free <= $0/M input, cheap <= $1.00/M input, premium above.
	cases := []struct {
		input, blended float64
		hasInput       bool
		want           ModelTier
	}{
		{0, 0, true, ModelTierFree},
		{0, 5, true, ModelTierFree},      // input leg decides, not blended
		{0.5, 0.5, true, ModelTierCheap}, // $0.50 input
		{1.0, 1.0, true, ModelTierCheap}, // at the cheap threshold
		{1.0001, 1.0001, true, ModelTierPremium},
		{5, 5, true, ModelTierPremium},
		{0, 0, false, ModelTierFree}, // blended proxy when no input leg
		{0.5, 0.5, false, ModelTierCheap},
		{5, 5, false, ModelTierPremium},
		{-1, -1, false, ModelTierUnknown}, // unknown price: no guessed tier
		{-1, -1, true, ModelTierUnknown},
	}
	for _, tc := range cases {
		if got := ClassifyPriceTier(tc.input, tc.hasInput, tc.blended); got != tc.want {
			t.Fatalf("ClassifyPriceTier(%v, %v, %v) = %v, want %v", tc.input, tc.hasInput, tc.blended, got, tc.want)
		}
	}
}

func TestSixteenWeightsSumToOne(t *testing.T) {
	if s := DefaultWeights16.Sum(); math.Abs(s-1.0) > 1e-9 {
		t.Fatalf("DefaultWeights16 sums to %v, want 1.0 within 1e-9", s)
	}
	for _, name := range ModePackNames {
		raw := ModePacks[name]
		// OmniRoute's own tables sum to 0.9999 (rounding in the
		// reference); the EFFECTIVE weights Score16 applies are the
		// normalised ones and must sum to 1.0 within 1e-9.
		if s := raw.Sum(); math.Abs(s-0.9999) > 1e-9 {
			t.Fatalf("pack %q raw sum = %v, want 0.9999 (reference rounding)", name, s)
		}
		norm, ok := NormalizeWeights16(raw)
		if !ok {
			t.Fatalf("pack %q failed to normalise", name)
		}
		if s := norm.Sum(); math.Abs(s-1.0) > 1e-9 {
			t.Fatalf("pack %q normalised sum = %v, want 1.0 within 1e-9", name, s)
		}
	}
}

func TestSixteenModePackTablesRescaledForLiveFactors(t *testing.T) {
	// G19 Area 1, gap-track RT-4: the reference headline weights
	// from open-sse/services/autoCombo/modePacks.ts, rescaled ×0.9048
	// (see ModePacks) so cacheAffinity and resetWindowAffinity can
	// vote at 0.04759524 each while every pack still sums to 0.9999.
	// The verbatim reference table is deliberately NOT preserved:
	// both factors are fed from live ledger signals, and zeroing
	// them would throw real data away. Un-scale any value below
	// (÷0.9048) to check it against modePacks.ts.
	headlines := map[string]map[string]float64{
		"ship-fast":         {"LatencyInv": 0.27578304, "Health": 0.24131016},
		"cost-saver":        {"CostInv": 0.30075552},
		"quality-first":     {"TaskFit": 0.31885152, "Stability": 0.12929592},
		"offline-friendly":  {"Quota": 0.30075552},
		"reliability-first": {"Health": 0.31885152, "Stability": 0.1723644, "Reliability": 0.036192},
		"chaos-mode":        {"Health": 0.36192, "TaskFit": 0.1723644},
	}
	for name, fields := range headlines {
		w, ok := ModePacks[name]
		if !ok {
			t.Fatalf("missing mode pack %q", name)
		}
		got := map[string]float64{
			"LatencyInv": w.LatencyInv, "Health": w.Health, "CostInv": w.CostInv,
			"TaskFit": w.TaskFit, "Stability": w.Stability, "Quota": w.Quota,
			"Reliability": w.Reliability,
		}
		for f, want := range fields {
			if got[f] != want {
				t.Fatalf("pack %q %s = %v, want %v (modePacks.ts ×0.9048)", name, f, got[f], want)
			}
		}
		// The RT-4 divergence itself: both formerly-zero live
		// factors vote at the sibling-affinity weight on the
		// pack's 0.9999 base, and reliability (already nonzero
		// in the reference) stays nonzero.
		if w.CacheAffinity != 0.04759524 || w.ResetWindowAffinity != 0.04759524 {
			t.Fatalf("pack %q cache/reset weights = %v/%v, want 0.04759524/0.04759524 (RT-4)",
				name, w.CacheAffinity, w.ResetWindowAffinity)
		}
		if w.Reliability <= 0 {
			t.Fatalf("pack %q Reliability = %v, want > 0", name, w.Reliability)
		}
	}
	if len(ModePacks) != 6 {
		t.Fatalf("len(ModePacks) = %d, want 6", len(ModePacks))
	}
}

// TestModePackCacheWarmthVotes (G19 Area 1, RT-4): under EVERY
// mode pack, measured cache warmth moves the score — the two
// formerly-zero factors now vote. Two candidates identical except
// CachedPromptFraction (0 vs 1): the warm one scores higher and
// leads under each pack. Same for the reset window: an imminent
// reset (0s) beats a reset 30d out under each pack.
func TestModePackCacheWarmthVotes(t *testing.T) {
	for _, name := range ModePackNames {
		w := ModePacks[name]
		if w.CacheAffinity <= 0 || w.ResetWindowAffinity <= 0 {
			t.Fatalf("pack %q has a zero live weight: cache %v reset %v", name, w.CacheAffinity, w.ResetWindowAffinity)
		}
		cold, warm := sixteenBase(), sixteenBase()
		cold.CachedPromptFraction, warm.CachedPromptFraction = 0, 1
		cands := []Candidate{sixteenCand("cold"), sixteenCand("warm")}
		inputs := map[Candidate]SixteenInputs{cands[0]: cold, cands[1]: warm}
		if got := Score16(cands[1], warm, w); got <= Score16(cands[0], cold, w) {
			t.Fatalf("pack %q: warm score %v <= cold score %v, want warmth to raise the score",
				name, got, Score16(cands[0], cold, w))
		}
		if got := order16(cands, inputs, w); got[0] != "warm" {
			t.Fatalf("pack %q cache-warmth ordering = %v, want warm first", name, got)
		}
		soon, late := sixteenBase(), sixteenBase()
		soon.ResetInSeconds, late.ResetInSeconds = 0, 2592000
		rcands := []Candidate{sixteenCand("late"), sixteenCand("soon")}
		rinputs := map[Candidate]SixteenInputs{rcands[0]: late, rcands[1]: soon}
		if got := order16(rcands, rinputs, w); got[0] != "soon" {
			t.Fatalf("pack %q reset-window ordering = %v, want the imminent-reset candidate first", name, got)
		}
	}
}

func TestSixteenUnknownPackFallsBackToDefault(t *testing.T) {
	if got := ModePack("no-such-pack"); got != DefaultWeights16 {
		t.Fatalf("ModePack(unknown) = %+v, want DefaultWeights16", got)
	}
}

// --- combined score ----------------------------------------------------------

func TestSixteenScoreIsWeightedSum(t *testing.T) {
	// Hand-computed: base fixture factors are
	// quota .5, health 1, costInv 0, latencyInv 0, taskFit 0,
	// stability 0, tierPriority .264, tierAffinity 1, specificity .9,
	// context .875, cache .5, session .5, reset .7666.., conn .2,
	// quality .5, reliability .9. Under DefaultWeights16 (G19/RT-4
	// rescaled: quota .12500892, health .1404054, tierPriority /
	// tierAffinity / specificity / context / session / connection
	// .04164048, cache .0476, reset .0476, quality .026244,
	// reliability .03):
	// .12500892*.5 + .1404054*1 + .04164048*.264 + .04164048*1
	// + .04164048*.9 + .04164048*.875 + .0476*.5 + .04164048*.5
	// + .0476*.7666667 + .04164048*.2 + .026244*.5 + .03*.9
	// = 0.4590189480533334 (Score16 normalises the weights first;
	// the table sums to 1.0 within 1e-9, so normalisation moves
	// the result only in the last ulps).
	in := sixteenBase()
	want := 0.4590189480533334
	if got := Score16(sixteenCand("x"), in, DefaultWeights16); math.Abs(got-want) > 1e-9 {
		t.Fatalf("Score16 = %v, want %v", got, want)
	}
}

func TestSixteenScoreInvalidWeightsFallBackToDefault(t *testing.T) {
	in := sixteenBase()
	bad := Weights16{LatencyInv: -1, Health: 1}
	// Normalisation re-scales the default weights, so compare with a
	// tolerance: float64 associativity shifts the last ulp.
	def := Score16(sixteenCand("x"), in, DefaultWeights16)
	if got := Score16(sixteenCand("x"), in, bad); math.Abs(got-def) > 1e-9 {
		t.Fatalf("Score16(bad weights) = %v, want default %v", got, def)
	}
	zero := Weights16{}
	if got := Score16(sixteenCand("x"), in, zero); math.Abs(got-def) > 1e-9 {
		t.Fatalf("Score16(zero weights) = %v, want default %v", got, def)
	}
}

// --- per-factor value + ordering tests ---------------------------------------
// Each test asserts the factor's VALUE at the extremes and that varying
// only that factor's inputs moves the ORDERING under a weight set where
// the factor votes. If the factor function is deleted or stubbed to a
// constant, its test fails.

func TestFactorQuota(t *testing.T) {
	if got := FactorQuota(SixteenInputs{QuotaRemainingPct: 100}); got != 1.0 {
		t.Fatalf("quota 100%% = %v, want 1", got)
	}
	if got := FactorQuota(SixteenInputs{QuotaRemainingPct: 0}); got != 0.0 {
		t.Fatalf("quota 0%% = %v, want 0", got)
	}
	if got := FactorQuota(SixteenInputs{QuotaRemainingPct: -1}); got != 0.5 {
		t.Fatalf("quota unknown = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.QuotaRemainingPct, hi.QuotaRemainingPct = 0, 100
	w := Weights16{Quota: 1}
	if got := Score16(sixteenCand("a"), lo, w); got >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("quota did not move ordering: lo=%v hi=%v", got, Score16(sixteenCand("a"), hi, w))
	}
}

func TestFactorHealth(t *testing.T) {
	if got := FactorHealth(SixteenInputs{Breaker: BreakerClosed}); got != 1.0 {
		t.Fatalf("closed = %v, want 1", got)
	}
	if got := FactorHealth(SixteenInputs{Breaker: BreakerHalfOpen}); got != 0.5 {
		t.Fatalf("half-open = %v, want 0.5", got)
	}
	if got := FactorHealth(SixteenInputs{Breaker: BreakerOpen}); got != 0.0 {
		t.Fatalf("open = %v, want 0", got)
	}
	if got := FactorHealth(SixteenInputs{Breaker: BreakerUnknown}); got != 0.5 {
		t.Fatalf("unknown breaker = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.Breaker, hi.Breaker = BreakerOpen, BreakerClosed
	w := Weights16{Health: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("health did not move ordering")
	}
}

func TestFactorCostInv(t *testing.T) {
	// Pool-relative, OmniRoute style: 1 - cost/maxCost.
	in := sixteenBase()
	in.CostPer1M = 0
	if got := FactorCostInv(in); got != 1.0 {
		t.Fatalf("free cost = %v, want 1", got)
	}
	in.CostPer1M = 10
	if got := FactorCostInv(in); got != 0.0 {
		t.Fatalf("max cost = %v, want 0", got)
	}
	in.CostPer1M = -1
	if got := FactorCostInv(in); got != 0.5 {
		t.Fatalf("unknown cost = %v, want neutral 0.5", got)
	}
	in.CostPer1M, in.MaxCostPer1M = 10, 0
	if got := FactorCostInv(in); got != 0.5 {
		t.Fatalf("no pool max = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.CostPer1M, hi.CostPer1M = 10, 0
	w := Weights16{CostInv: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("costInv did not move ordering")
	}
}

func TestFactorLatencyInv(t *testing.T) {
	in := sixteenBase()
	in.LatencyP95Ms = 1
	if got := FactorLatencyInv(in); math.Abs(got-0.999) > 1e-9 {
		t.Fatalf("1ms/1000ms = %v, want 0.999", got)
	}
	in.LatencyP95Ms = 1000
	if got := FactorLatencyInv(in); got != 0.0 {
		t.Fatalf("max latency = %v, want 0", got)
	}
	in.LatencyP95Ms = 0
	if got := FactorLatencyInv(in); got != 0.5 {
		t.Fatalf("unknown latency = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.LatencyP95Ms, hi.LatencyP95Ms = 1000, 1
	w := Weights16{LatencyInv: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("latencyInv did not move ordering")
	}
}

func TestFactorTaskFit(t *testing.T) {
	// Jaccard similarity of task tags vs model tags.
	in := sixteenBase()
	in.ModelTags = []string{"code"}
	if got := FactorTaskFit(in); got != 1.0 {
		t.Fatalf("identical tags = %v, want 1", got)
	}
	in.ModelTags = []string{"chat"}
	if got := FactorTaskFit(in); got != 0.0 {
		t.Fatalf("disjoint tags = %v, want 0", got)
	}
	in.TaskTags, in.ModelTags = nil, nil
	if got := FactorTaskFit(in); got != 0.5 {
		t.Fatalf("no tags = %v, want neutral 0.5", got)
	}
	in.TaskTags, in.ModelTags = []string{"code", "review"}, []string{"code", "chat"}
	if got := FactorTaskFit(in); math.Abs(got-1.0/3.0) > 1e-9 {
		t.Fatalf("partial overlap = %v, want 1/3", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ModelTags = []string{"chat"}
	hi.ModelTags = []string{"code"}
	w := Weights16{TaskFit: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("taskFit did not move ordering")
	}
}

func TestFactorStability(t *testing.T) {
	in := sixteenBase()
	in.LatencyStdDevMs = 0
	if got := FactorStability(in); got != 1.0 {
		t.Fatalf("zero variance = %v, want 1", got)
	}
	in.LatencyStdDevMs = 100
	if got := FactorStability(in); got != 0.0 {
		t.Fatalf("max variance = %v, want 0", got)
	}
	in.LatencyStdDevMs, in.MaxStdDevMs = 50, 100
	if got := FactorStability(in); got != 0.5 {
		t.Fatalf("half variance = %v, want 0.5", got)
	}
	in.LatencyStdDevMs, in.MaxStdDevMs = -1, 100
	if got := FactorStability(in); got != 0.5 {
		t.Fatalf("unknown variance = %v, want neutral 0.5", got)
	}
	in.LatencyStdDevMs, in.MaxStdDevMs = 50, 0
	if got := FactorStability(in); got != 0.5 {
		t.Fatalf("no pool max = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.LatencyStdDevMs, hi.LatencyStdDevMs = 100, 0
	w := Weights16{Stability: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("stability did not move ordering")
	}
}

func TestFactorTierPriority(t *testing.T) {
	// OmniRoute calculateTierScore: ultra 1.0, pro 0.67, standard 0.33,
	// free 0.0, unknown 0.33; base*0.8 + resetBonus*0.2, resetBonus =
	// max(0, 1 - secs/2592000).
	cases := []struct {
		tier AccountTier
		secs float64
		want float64
	}{
		{AccountTierUltra, 0, 0.8},
		{AccountTierPro, 0, 0.536},
		{AccountTierStandard, 0, 0.264},
		{AccountTierFree, 0, 0.0},
		{AccountTierUnknown, 0, 0.264},
		{AccountTierUltra, 1296000, 0.9}, // 15d reset: bonus 0.5*0.2
		{AccountTierFree, 0, 0.0},
	}
	for _, tc := range cases {
		in := sixteenBase()
		in.Tier, in.QuotaResetIntervalSecs = tc.tier, tc.secs
		if got := FactorTierPriority(in); math.Abs(got-tc.want) > 1e-9 {
			t.Fatalf("tier %q reset %v = %v, want %v", tc.tier, tc.secs, got, tc.want)
		}
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.Tier, hi.Tier = AccountTierFree, AccountTierUltra
	w := Weights16{TierPriority: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("tierPriority did not move ordering")
	}
}

func TestFactorTierAffinity(t *testing.T) {
	// OmniRoute calculateTierAffinity: same tier 1.0, adjacent 0.7,
	// two apart 0.3, no hint 0.5.
	cases := []struct {
		model, min ModelTier
		want       float64
	}{
		{ModelTierCheap, ModelTierCheap, 1.0},
		{ModelTierFree, ModelTierCheap, 0.7},
		{ModelTierPremium, ModelTierCheap, 0.7},
		{ModelTierFree, ModelTierPremium, 0.3},
		{ModelTierPremium, ModelTierFree, 0.3},
	}
	for _, tc := range cases {
		in := sixteenBase()
		in.ModelTier, in.RecommendedMinTier = tc.model, tc.min
		if got := FactorTierAffinity(in); got != tc.want {
			t.Fatalf("tier %q vs min %q = %v, want %v", tc.model, tc.min, got, tc.want)
		}
	}
	in := sixteenBase()
	in.RecommendedMinTier = ModelTierUnknown
	if got := FactorTierAffinity(in); got != 0.5 {
		t.Fatalf("no hint = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ModelTier, hi.ModelTier = ModelTierFree, ModelTierCheap // min is cheap
	w := Weights16{TierAffinity: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("tierAffinity did not move ordering")
	}
}

func TestFactorSpecificityMatch(t *testing.T) {
	// OmniRoute calculateSpecificityMatch: free <=15 -> 0.9 else 0.2;
	// cheap 15<s<=50 -> 0.9 else 0.4; premium >50 -> 0.9 else 0.3.
	cases := []struct {
		tier ModelTier
		spec float64
		want float64
	}{
		{ModelTierFree, 10, 0.9},
		{ModelTierFree, 50, 0.2},
		{ModelTierCheap, 30, 0.9},
		{ModelTierCheap, 80, 0.4},
		{ModelTierPremium, 80, 0.9},
		{ModelTierPremium, 30, 0.3},
	}
	for _, tc := range cases {
		in := sixteenBase()
		in.ModelTier, in.SpecificityScore = tc.tier, tc.spec
		if got := FactorSpecificityMatch(in); got != tc.want {
			t.Fatalf("tier %q spec %v = %v, want %v", tc.tier, tc.spec, got, tc.want)
		}
	}
	in := sixteenBase()
	in.SpecificityScore = -1
	if got := FactorSpecificityMatch(in); got != 0.5 {
		t.Fatalf("unknown specificity = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ModelTier, lo.SpecificityScore = ModelTierCheap, 80
	hi.ModelTier, hi.SpecificityScore = ModelTierCheap, 30
	w := Weights16{SpecificityMatch: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("specificityMatch did not move ordering")
	}
}

func TestFactorContextAffinity(t *testing.T) {
	// Context-window headroom: 1 - used/window; over-window 0.
	in := sixteenBase()
	in.ContextTokens, in.ModelContextTokens = 0, 8000
	if got := FactorContextAffinity(in); got != 1.0 {
		t.Fatalf("empty context = %v, want 1", got)
	}
	in.ContextTokens = 4000
	if got := FactorContextAffinity(in); got != 0.5 {
		t.Fatalf("half window = %v, want 0.5", got)
	}
	in.ContextTokens = 8000
	if got := FactorContextAffinity(in); got != 0.0 {
		t.Fatalf("full window = %v, want 0", got)
	}
	in.ContextTokens = 16000
	if got := FactorContextAffinity(in); got != 0.0 {
		t.Fatalf("over window = %v, want 0", got)
	}
	in.ContextTokens, in.ModelContextTokens = 1000, 0
	if got := FactorContextAffinity(in); got != 0.5 {
		t.Fatalf("unknown window = %v, want neutral 0.5", got)
	}
	in.ContextTokens, in.ModelContextTokens = -1, 8000
	if got := FactorContextAffinity(in); got != 0.5 {
		t.Fatalf("unknown context = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ContextTokens, hi.ContextTokens = 8000, 1000
	w := Weights16{ContextAffinity: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("contextAffinity did not move ordering")
	}
}

func TestFactorCacheAffinity(t *testing.T) {
	// Fraction of the prompt already cached on this candidate.
	in := sixteenBase()
	in.CachedPromptFraction = 1
	if got := FactorCacheAffinity(in); got != 1.0 {
		t.Fatalf("fully cached = %v, want 1", got)
	}
	in.CachedPromptFraction = 0
	if got := FactorCacheAffinity(in); got != 0.0 {
		t.Fatalf("nothing cached = %v, want 0", got)
	}
	in.CachedPromptFraction = -1
	if got := FactorCacheAffinity(in); got != 0.5 {
		t.Fatalf("unknown cache = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.CachedPromptFraction, hi.CachedPromptFraction = 0, 1
	w := Weights16{CacheAffinity: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("cacheAffinity did not move ordering")
	}
}

func TestFactorSessionAffinity(t *testing.T) {
	c := sixteenCand("p")
	in := sixteenBase()
	// No session at all: neutral.
	if got := FactorSessionAffinity(c, in); got != 0.5 {
		t.Fatalf("no session = %v, want neutral 0.5", got)
	}
	in.SessionProvider, in.SessionModel = "p", "m"
	if got := FactorSessionAffinity(c, in); got != 1.0 {
		t.Fatalf("same provider+model = %v, want 1", got)
	}
	in.SessionModel = "other"
	if got := FactorSessionAffinity(c, in); got != 0.7 {
		t.Fatalf("same provider, other model = %v, want 0.7", got)
	}
	in.SessionProvider, in.SessionModel = "q", "m"
	if got := FactorSessionAffinity(c, in); got != 0.3 {
		t.Fatalf("other provider = %v, want 0.3", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.SessionProvider, lo.SessionModel = "q", "m"
	hi.SessionProvider, hi.SessionModel = "p", "m"
	w := Weights16{SessionAffinity: 1}
	if Score16(c, lo, w) >= Score16(c, hi, w) {
		t.Fatalf("sessionAffinity did not move ordering")
	}
}

func TestFactorResetWindowAffinity(t *testing.T) {
	// Sooner reset = higher affinity: 1 - resetSecs/2592000 (30d).
	in := sixteenBase()
	in.ResetInSeconds = 0
	if got := FactorResetWindowAffinity(in); got != 1.0 {
		t.Fatalf("resetting now = %v, want 1", got)
	}
	in.ResetInSeconds = 1296000 // 15d
	if got := FactorResetWindowAffinity(in); got != 0.5 {
		t.Fatalf("15d reset = %v, want 0.5", got)
	}
	in.ResetInSeconds = 2592000 // 30d
	if got := FactorResetWindowAffinity(in); got != 0.0 {
		t.Fatalf("30d reset = %v, want 0", got)
	}
	in.ResetInSeconds = -1
	if got := FactorResetWindowAffinity(in); got != 0.5 {
		t.Fatalf("unknown reset = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ResetInSeconds, hi.ResetInSeconds = 2592000, 0
	w := Weights16{ResetWindowAffinity: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("resetWindowAffinity did not move ordering")
	}
}

func TestFactorConnectionDensity(t *testing.T) {
	// OmniRoute: (poolSize-1)/10 clamped; unknown pool -> neutral 0.5.
	in := sixteenBase()
	in.ConnectionCount = 1
	if got := FactorConnectionDensity(in); got != 0.0 {
		t.Fatalf("1 connection = %v, want 0", got)
	}
	in.ConnectionCount = 11
	if got := FactorConnectionDensity(in); got != 1.0 {
		t.Fatalf("11 connections = %v, want 1", got)
	}
	in.ConnectionCount = 6
	if got := FactorConnectionDensity(in); got != 0.5 {
		t.Fatalf("6 connections = %v, want 0.5", got)
	}
	in.ConnectionCount = 0
	if got := FactorConnectionDensity(in); got != 0.5 {
		t.Fatalf("unknown pool = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.ConnectionCount, hi.ConnectionCount = 1, 11
	w := Weights16{ConnectionDensity: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("connectionDensity did not move ordering")
	}
}

func TestFactorQuality(t *testing.T) {
	// Reuses the package's qualityComponent: 0 (unknown) -> 0.45.
	in := sixteenBase()
	in.QualityScore = 0
	if got := FactorQuality(in); got != UnknownQualityScore {
		t.Fatalf("unknown quality = %v, want %v", got, UnknownQualityScore)
	}
	in.QualityScore = 0.8
	if got := FactorQuality(in); got != 0.8 {
		t.Fatalf("quality 0.8 = %v, want 0.8", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.QualityScore, hi.QualityScore = 0.2, 0.9
	w := Weights16{Quality: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("quality did not move ordering")
	}
}

func TestFactorReliability(t *testing.T) {
	in := sixteenBase()
	in.SuccessRate = 1
	if got := FactorReliability(in); got != 1.0 {
		t.Fatalf("success 1 = %v, want 1", got)
	}
	in.SuccessRate = 0
	if got := FactorReliability(in); got != 0.0 {
		t.Fatalf("success 0 = %v, want 0", got)
	}
	in.SuccessRate = -1
	if got := FactorReliability(in); got != 0.5 {
		t.Fatalf("unknown success = %v, want neutral 0.5", got)
	}
	lo, hi := sixteenBase(), sixteenBase()
	lo.SuccessRate, hi.SuccessRate = 0.2, 0.9
	w := Weights16{Reliability: 1}
	if Score16(sixteenCand("a"), lo, w) >= Score16(sixteenCand("a"), hi, w) {
		t.Fatalf("reliability did not move ordering")
	}
}

// TestSixteenMissingInputsAreNeutral: every factor with all of its
// inputs missing degrades to neutral (0.5), except quality, which
// reuses the package's existing unknown-quality convention (0.45),
// and tierPriority, whose OmniRoute default for an unknown tier is
// the standard-tier base (0.264).
func TestSixteenMissingInputsAreNeutral(t *testing.T) {
	empty := SixteenInputs{
		QuotaRemainingPct: -1, LatencyP95Ms: 0, MaxLatencyMs: 0,
		CostPer1M: -1, MaxCostPer1M: 0, SuccessRate: -1, QualityScore: 0,
		LatencyStdDevMs: -1, MaxStdDevMs: 0, Breaker: BreakerUnknown,
		Tier: AccountTierUnknown, QuotaResetIntervalSecs: 0,
		ModelTier: ModelTierUnknown, RecommendedMinTier: ModelTierUnknown,
		SpecificityScore: -1, ContextTokens: -1, ModelContextTokens: 0,
		CachedPromptFraction: -1, SessionProvider: "", SessionModel: "",
		ResetInSeconds: -1, ConnectionCount: 0,
	}
	f := ComputeFactors16(sixteenCand("x"), empty)
	neutral := map[string]float64{
		"Quota": f.Quota, "Health": f.Health, "CostInv": f.CostInv,
		"LatencyInv": f.LatencyInv, "TaskFit": f.TaskFit,
		"Stability": f.Stability, "TierAffinity": f.TierAffinity,
		"SpecificityMatch": f.SpecificityMatch,
		"ContextAffinity":  f.ContextAffinity,
		"CacheAffinity":    f.CacheAffinity, "SessionAffinity": f.SessionAffinity,
		"ResetWindowAffinity": f.ResetWindowAffinity,
		"ConnectionDensity":   f.ConnectionDensity,
		"Reliability":         f.Reliability,
	}
	for name, got := range neutral {
		if got != 0.5 {
			t.Fatalf("factor %s with missing inputs = %v, want neutral 0.5", name, got)
		}
	}
	if f.Quality != UnknownQualityScore {
		t.Fatalf("quality with missing inputs = %v, want %v (package convention)", f.Quality, UnknownQualityScore)
	}
	if math.Abs(f.TierPriority-0.264) > 1e-9 {
		t.Fatalf("tierPriority with unknown tier = %v, want 0.264 (OmniRoute unknown-tier base)", f.TierPriority)
	}
	if got := Score16(sixteenCand("x"), empty, DefaultWeights16); got < 0 || got > 1 {
		t.Fatalf("Score16(all unknown) = %v, want within [0,1]", got)
	}
}

// --- mode packs ---------------------------------------------------------------

// sixPackFixture builds the discriminating fixture: six candidates
// that each excel on exactly one pack's headline axis. All other
// factors are equal across candidates (see sixteenBase), so only the
// varying factors decide the order. Expected orders below were
// hand-computed from the modePacks.ts tables (see test comment).
//
//	speed:    latency 1ms (0.999), health closed, taskFit 0, stability 0
//	cheap:    cost $0 (costInv 1), latency 500ms (0.5), health closed
//	quality:  taskFit 1, stability 1, health HALF-OPEN (0.5)
//	quota:    quota 100%, health closed
//	reliable: health closed, stability 1, taskFit 0
//	chaos:    health closed, taskFit 1, stability 0.3 (stddev 70/100)
func sixPackFixture() ([]Candidate, map[Candidate]SixteenInputs) {
	ids := []string{"speed", "cheap", "quality", "quota", "reliable", "chaos"}
	cands := make([]Candidate, 0, len(ids))
	inputs := make(map[Candidate]SixteenInputs, len(ids))
	for _, id := range ids {
		c := sixteenCand(id)
		cands = append(cands, c)
		in := sixteenBase()
		switch id {
		case "speed":
			in.LatencyP95Ms = 1
		case "cheap":
			in.CostPer1M = 0
			in.LatencyP95Ms = 500
		case "quality":
			in.ModelTags = []string{"code"}
			in.LatencyStdDevMs = 0
			in.Breaker = BreakerHalfOpen
		case "quota":
			in.QuotaRemainingPct = 100
		case "reliable":
			in.LatencyStdDevMs = 0
		case "chaos":
			in.ModelTags = []string{"code"}
			in.LatencyStdDevMs = 70
		}
		inputs[c] = in
	}
	return cands, inputs
}

func TestModePackOrderingsDiffer(t *testing.T) {
	cands, inputs := sixPackFixture()
	want := map[string][]string{
		"ship-fast":         {"speed", "cheap", "chaos", "quota", "reliable", "quality"},
		"cost-saver":        {"cheap", "chaos", "quota", "quality", "reliable", "speed"},
		"quality-first":     {"quality", "chaos", "reliable", "cheap", "speed", "quota"},
		"offline-friendly":  {"quota", "cheap", "reliable", "speed", "chaos", "quality"},
		"reliability-first": {"reliable", "chaos", "quality", "quota", "speed", "cheap"},
		"chaos-mode":        {"chaos", "reliable", "quality", "cheap", "quota", "speed"},
	}
	seen := map[string]bool{}
	for _, name := range ModePackNames {
		got := order16(cands, inputs, ModePacks[name])
		if !reflect.DeepEqual(got, want[name]) {
			t.Fatalf("pack %q ordering = %v, want %v", name, got, want[name])
		}
		joined := ""
		for _, id := range got {
			joined += id + ","
		}
		if seen[joined] {
			t.Fatalf("pack %q ordering %v duplicates another pack's ordering", name, got)
		}
		seen[joined] = true
		// Each pack's winner must be the candidate named for its axis.
		if got[0] != map[string]string{
			"ship-fast": "speed", "cost-saver": "cheap",
			"quality-first": "quality", "offline-friendly": "quota",
			"reliability-first": "reliable", "chaos-mode": "chaos",
		}[name] {
			t.Fatalf("pack %q winner = %q, want its headline candidate", name, got[0])
		}
	}
}

func TestModePackRankPreservesCandidates(t *testing.T) {
	cands, inputs := sixPackFixture()
	for _, name := range ModePackNames {
		ranked := Rank16(cands, inputs, ModePacks[name])
		if len(ranked) != len(cands) {
			t.Fatalf("pack %q ranked %d candidates, want %d", name, len(ranked), len(cands))
		}
		sorted := append([]Candidate(nil), ranked...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].ProviderID < sorted[j].ProviderID })
		want := append([]Candidate(nil), cands...)
		sort.Slice(want, func(i, j int) bool { return want[i].ProviderID < want[j].ProviderID })
		if !reflect.DeepEqual(sorted, want) {
			t.Fatalf("pack %q ranking is not a permutation of the input", name)
		}
	}
}

func TestSixteenRankStableOnTies(t *testing.T) {
	a, b := sixteenCand("a"), sixteenCand("b")
	in := sixteenBase()
	inputs := map[Candidate]SixteenInputs{a: in, b: in}
	got := Rank16([]Candidate{a, b}, inputs, DefaultWeights16)
	if got[0] != a || got[1] != b {
		t.Fatalf("tied ranking = %v, want input order [a b]", got)
	}
}

func TestSixteenFactorsCoverAllSixteenWeights(t *testing.T) {
	// Every weight field must have a matching factor field, so no
	// weight can silently vote for nothing.
	in := sixteenBase()
	f := ComputeFactors16(sixteenCand("x"), in)
	wv := reflect.ValueOf(DefaultWeights16)
	fv := reflect.ValueOf(f)
	wt, ft := wv.Type(), fv.Type()
	if wt.NumField() != ft.NumField() {
		t.Fatalf("Weights16 has %d fields, Factors16 has %d", wt.NumField(), ft.NumField())
	}
	for i := 0; i < wt.NumField(); i++ {
		if wt.Field(i).Name != ft.Field(i).Name {
			t.Fatalf("field %d: Weights16.%s != Factors16.%s", i, wt.Field(i).Name, ft.Field(i).Name)
		}
	}
}

// TestRankModelsByBestPair16 (G19 Area 1): the auto pool is ranked
// by the 16-factor scorer, best pair per model. The cheap model
// wins on cost under the default weights even though its latency
// is worse; a model no provider offers is scored on its own inputs
// instead of being dropped; the result is a permutation.
func TestRankModelsByBestPair16(t *testing.T) {
	inputsFor := func(cost, latency float64) SixteenInputs {
		in := sixteenBase()
		in.CostPer1M = cost
		in.LatencyP95Ms = latency
		in.MaxCostPer1M = -1 // filled from pool maxima by the ranker
		in.MaxLatencyMs = -1
		return in
	}
	byPair := map[Candidate]SixteenInputs{
		{ProviderID: "cheap-p", Model: "cheap-model"}: inputsFor(1, 900),
		{ProviderID: "fast-p", Model: "fast-model"}:   inputsFor(20, 100),
		{ProviderID: "mid-p", Model: "cheap-model"}:   inputsFor(15, 500),
		{ProviderID: "slow-p", Model: "lonely-model"}: inputsFor(5, 2000),
	}
	offers := func(model string) []Candidate {
		switch model {
		case "cheap-model":
			return []Candidate{{ProviderID: "cheap-p", Model: model}, {ProviderID: "mid-p", Model: model}}
		case "fast-model":
			return []Candidate{{ProviderID: "fast-p", Model: model}}
		default:
			return nil
		}
	}
	inputs := func(c Candidate) SixteenInputs {
		if in, ok := byPair[c]; ok {
			return in
		}
		return sixteenBase()
	}
	ranked := RankModelsByBestPair16(
		[]string{"fast-model", "cheap-model", "lonely-model"},
		offers, inputs, DefaultWeights16)
	if len(ranked) != 3 {
		t.Fatalf("ranked %v, want 3 models", ranked)
	}
	// cheap-model's best pair ($1) beats fast-model ($20) under
	// default weights despite the latency gap; fast-model's
	// 100ms latency still beats lonely-model's 1000ms at equal
	// other factors (costInv 0 vs 0.5 does not close a 0.9
	// latencyInv gap at weight 0.1143). Hand-checked:
	// cheap ≈ 0.492 > fast ≈ 0.448 > lonely ≈ 0.416.
	if ranked[0] != "cheap-model" || ranked[1] != "fast-model" || ranked[2] != "lonely-model" {
		t.Errorf("ranked %v, want [cheap-model fast-model lonely-model]", ranked)
	}
}

// TestRankModelsByBestPair16PoolMaxima (G19 Area 1): the
// pool-relative factors use the maxima over every pair of every
// model in the pool — not per-model maxima. Two models with the
// same absolute latency rank by cost when the pool max latency
// is set by a third, slower model.
func TestRankModelsByBestPair16PoolMaxima(t *testing.T) {
	mk := func(cost, latency float64) SixteenInputs {
		in := sixteenBase()
		in.CostPer1M = cost
		in.LatencyP95Ms = latency
		in.MaxCostPer1M = -1
		in.MaxLatencyMs = -1
		return in
	}
	byPair := map[Candidate]SixteenInputs{
		{ProviderID: "p", Model: "a"}: mk(10, 100),
		{ProviderID: "p", Model: "b"}: mk(1, 100),
		{ProviderID: "p", Model: "c"}: mk(10, 1000),
	}
	offers := func(model string) []Candidate {
		return []Candidate{{ProviderID: "p", Model: model}}
	}
	ranked := RankModelsByBestPair16([]string{"a", "b", "c"}, offers,
		func(c Candidate) SixteenInputs { return byPair[c] }, DefaultWeights16)
	if len(ranked) != 3 || ranked[0] != "b" {
		t.Errorf("ranked %v, want b first (cheapest at equal latency)", ranked)
	}
	if ranked[2] != "c" {
		t.Errorf("ranked %v, want c last (slowest in the pool)", ranked)
	}
}
