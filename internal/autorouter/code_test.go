package autorouter

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// stubOutcomes is a CodeOutcomeProvider backed by a fixed map,
// for tests.
type stubOutcomes map[string]TaskOutcome

func (s stubOutcomes) CodeOutcome(model string) (TaskOutcome, bool) {
	o, ok := s[model]
	return o, ok
}

func TestCostPerSolvedHarshPenalty(t *testing.T) {
	// The headline property: a cheap model that fails the task
	// must lose to a pricier model that solves it. Cheap: $0.50
	// per 1M, solves 2 of 10. Pricey: $5.00 per 1M, solves 19
	// of 20 (0.95). Cheap CPS = 0.5 × 5 × 2.6 = 6.50; pricey
	// CPS = 5 × 1.0526 × 1.10 ≈ 5.79.
	cheap, _, _ := CostPerSolved(0.50, TaskOutcome{Attempts: 10, Solved: 2})
	pricey, _, _ := CostPerSolved(5.00, TaskOutcome{Attempts: 20, Solved: 19})
	if !(pricey < cheap) {
		t.Fatalf("pricey-but-solving CPS %.2f should beat cheap-but-failing CPS %.2f", pricey, cheap)
	}
	if math.Abs(cheap-6.50) > 0.01 {
		t.Errorf("cheap CPS = %.2f, want 6.50", cheap)
	}
	if math.Abs(pricey-5.79) > 0.01 {
		t.Errorf("pricey CPS = %.2f, want ~5.79", pricey)
	}
}

func TestCostPerSolvedNoEvidenceIsNeutralDefault(t *testing.T) {
	// No evidence: both rank at the 0.5 default, so the order is
	// by price — and the rate reported is the default, flagged
	// as not evidence.
	cpsCheap, pCheap, evCheap := CostPerSolved(1.0, TaskOutcome{})
	cpsPricey, pPricey, evPricey := CostPerSolved(4.0, TaskOutcome{})
	if pCheap != DefaultSolveRate || pPricey != DefaultSolveRate {
		t.Errorf("no-evidence rate = %.2f/%.2f, want default %.2f", pCheap, pPricey, DefaultSolveRate)
	}
	if evCheap || evPricey {
		t.Error("no-evidence outcome must report evidence=false")
	}
	if !(cpsCheap < cpsPricey) {
		t.Errorf("no-evidence CPS cheap %.2f should beat pricey %.2f (price order)", cpsCheap, cpsPricey)
	}
	// Below the evidence floor the measured rate must NOT apply:
	// 9 failures out of 9 still ranks at the default.
	_, p, ev := CostPerSolved(1.0, TaskOutcome{Attempts: 9, Solved: 0})
	if ev || p != DefaultSolveRate {
		t.Errorf("below-floor outcome: rate %.2f evidence %v, want default %.2f evidence false", p, ev, DefaultSolveRate)
	}
	// At the floor the measured rate applies.
	_, p, ev = CostPerSolved(1.0, TaskOutcome{Attempts: 10, Solved: 0})
	if !ev || p != 0 {
		t.Errorf("at-floor outcome: rate %.2f evidence %v, want 0.00 evidence true", p, ev)
	}
}

func TestScoreCodeModelsCheapFailingLoses(t *testing.T) {
	prices := func(m string) (float64, bool) {
		switch m {
		case "cheap-fail":
			return 0.50, true
		case "pricey-solve":
			return 5.00, true
		default:
			return 0, false
		}
	}
	outcomes := stubOutcomes{
		"cheap-fail":   {Attempts: 10, Solved: 2},
		"pricey-solve": {Attempts: 20, Solved: 19},
	}
	scores := ScoreCodeModels([]string{"cheap-fail", "pricey-solve"}, prices, outcomes)
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want 2", len(scores))
	}
	if scores[0].Model != "pricey-solve" {
		t.Errorf("winner = %q, want pricey-solve (cheap model fails 80%% of tasks)", scores[0].Model)
	}
	if !scores[0].Evidence || !scores[1].Evidence {
		t.Error("seeded outcomes above the floor must report evidence=true")
	}
	if scores[0].Samples != 20 || scores[1].Samples != 10 {
		t.Errorf("samples = %d/%d, want 20/10", scores[0].Samples, scores[1].Samples)
	}
}

func TestScoreCodeModelsNoEvidenceFallsBackToPrice(t *testing.T) {
	prices := func(m string) (float64, bool) {
		if m == "cheap" {
			return 1.0, true
		}
		return 4.0, true
	}
	scores := ScoreCodeModels([]string{"pricey", "cheap"}, prices, nil)
	if scores[0].Model != "cheap" {
		t.Errorf("no-evidence winner = %q, want cheap (price order at the neutral default)", scores[0].Model)
	}
	if AnyCodeEvidence(scores) {
		t.Error("no-evidence scores must report no evidence")
	}
	for _, sc := range scores {
		if sc.SolveRate != DefaultSolveRate || sc.Evidence {
			t.Errorf("%s: rate %.2f evidence %v, want default %.2f evidence false", sc.Model, sc.SolveRate, sc.Evidence, DefaultSolveRate)
		}
	}
}

func TestScoreCodeModelsUnknownPriceSortsLast(t *testing.T) {
	prices := func(m string) (float64, bool) {
		if m == "priced" {
			return 10.0, true
		}
		return 0, false
	}
	scores := ScoreCodeModels([]string{"unpriced", "priced"}, prices, nil)
	if scores[0].Model != "priced" {
		t.Errorf("winner = %q, want priced (unpriced model has no computable CPS)", scores[0].Model)
	}
	if scores[1].PriceKnown || scores[1].CostPerSolved != UnknownPriceCPS {
		t.Errorf("unpriced model: PriceKnown=%v CPS=%v, want false/UnknownPriceCPS", scores[1].PriceKnown, scores[1].CostPerSolved)
	}
	// All unpriced: input order stands, flagged honestly.
	scores = ScoreCodeModels([]string{"a", "b"}, nil, nil)
	if scores[0].Model != "a" || scores[1].Model != "b" {
		t.Errorf("all-unpriced order = %q/%q, want input order a/b", scores[0].Model, scores[1].Model)
	}
}

func TestNarrowPool(t *testing.T) {
	models := []string{"m1", "m2", "m3", "m4"}
	pool := NarrowPool(models, CodePoolFilter{
		Allow:               []string{"m1", "m2", "m3"},
		Exclude:             []string{"m2"},
		RequireCapabilities: []string{"tools"},
		Capabilities: map[string][]string{
			"m1": {"tools", "vision"},
			"m3": {"vision"},
		},
		Guardrail: func(m string) bool { return m != "m4" },
	})
	// m1 survives; m2 excluded; m3 lacks tools; m4 not on allow
	// list (and guardrail-rejected too — allowlist is checked
	// first, so that is the recorded reason).
	if len(pool.Models) != 1 || pool.Models[0] != "m1" {
		t.Fatalf("survivors = %v, want [m1]", pool.Models)
	}
	for m, want := range map[string]string{
		"m2": "exclude",
		"m3": "missing required capability",
		"m4": "allow list",
	} {
		if why, ok := pool.Dropped[m]; !ok || !strings.Contains(why, want) {
			t.Errorf("drop %s: got %q, want reason containing %q", m, why, want)
		}
	}
}

func TestNarrowPoolUnknownCapabilitiesDroppedWhenRequired(t *testing.T) {
	pool := NarrowPool([]string{"unknown"}, CodePoolFilter{
		RequireCapabilities: []string{"tools"},
	})
	if len(pool.Models) != 0 {
		t.Fatalf("survivors = %v, want none (capabilities unknown, requirement set)", pool.Models)
	}
	if why := pool.Dropped["unknown"]; !strings.Contains(why, "capabilities unknown") {
		t.Errorf("drop reason = %q, want capabilities-unknown note", why)
	}
	// With no requirement, unknown capabilities do not drop.
	pool = NarrowPool([]string{"unknown"}, CodePoolFilter{})
	if len(pool.Models) != 1 {
		t.Errorf("survivors = %v, want [unknown] with no narrowing", pool.Models)
	}
}

func TestOutcomeStoreCodingTierOnly(t *testing.T) {
	s := NewOutcomeStore()
	s.Record("m1", "coding", "accept")
	s.Record("m1", "coding", "discard")
	s.Record("m1", "coding", "edit")
	s.Record("m1", "reasoning", "accept") // not a coding outcome: ignored
	s.Record("m1", "coding", "bogus-signal")
	s.Record("", "coding", "accept")
	o, ok := s.CodeOutcome("m1")
	if !ok {
		t.Fatal("m1 should have recorded outcomes")
	}
	if o.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (coding tier only, known signals only)", o.Attempts)
	}
	if o.Solved != 1.5 {
		t.Errorf("solved = %.2f, want 1.50 (accept 1 + discard 0 + edit 0.5)", o.Solved)
	}
	if _, ok := s.CodeOutcome("nope"); ok {
		t.Error("unknown model must report no outcome")
	}
	if snap := s.Snapshot(); len(snap) != 1 {
		t.Errorf("snapshot has %d models, want 1", len(snap))
	}
}

func TestSolveCredit(t *testing.T) {
	for sig, want := range map[string]float64{"accept": 1, "edit": 0.5, "discard": 0, "regenerate": 0} {
		got, ok := SolveCredit(sig)
		if !ok || got != want {
			t.Errorf("SolveCredit(%q) = %.1f/%v, want %.1f/true", sig, got, ok, want)
		}
	}
	if _, ok := SolveCredit("thumbs-up"); ok {
		t.Error("unknown signal must not map to a credit")
	}
}

func TestResolveAutoCodeEndToEnd(t *testing.T) {
	r := New(Config{
		TierModels: map[Tier][]string{
			TierCoding: {"cheap-fail", "pricey-solve"},
		},
		CodePrices: func(m string) (float64, bool) {
			switch m {
			case "cheap-fail":
				return 0.50, true
			case "pricey-solve":
				return 5.00, true
			}
			return 0, false
		},
		CodeOutcomes: stubOutcomes{
			"cheap-fail":   {Attempts: 10, Solved: 2},
			"pricey-solve": {Attempts: 20, Solved: 19},
		},
	})
	model, dec, ok := r.Resolve(CodeVirtualModel, "Write a Python function that sorts a list of numbers")
	if !ok {
		t.Fatalf("Resolve(auto/code) failed: %s", dec.Reason)
	}
	if model != "pricey-solve" {
		t.Errorf("resolved model = %q, want pricey-solve", model)
	}
	if dec.Tier != TierCoding {
		t.Errorf("tier = %q, want coding", dec.Tier)
	}
	if len(dec.CodeScores) != 2 || dec.CodeScores[0].Model != "pricey-solve" {
		t.Errorf("code scores = %+v, want pricey-solve first", dec.CodeScores)
	}
	if dec.CodeNoEvidence {
		t.Error("seeded evidence must not report CodeNoEvidence")
	}
	if !strings.Contains(dec.Reason, "auto/code") || !strings.Contains(dec.Reason, "cost per solved task") {
		t.Errorf("reason %q must name the mode and its objective", dec.Reason)
	}
	if !strings.Contains(dec.Reason, "measured solve rate") {
		t.Errorf("reason %q must name the measured solve rate", dec.Reason)
	}
}

func TestResolveAutoCodeNoEvidenceHonest(t *testing.T) {
	r := New(Config{
		TierModels: map[Tier][]string{
			TierCoding: {"pricey", "cheap"},
		},
		CodePrices: func(m string) (float64, bool) {
			if m == "cheap" {
				return 1.0, true
			}
			return 4.0, true
		},
	})
	model, dec, ok := r.Resolve(CodeVirtualModel, "Write a Python function that sorts a list of numbers")
	if !ok {
		t.Fatalf("Resolve(auto/code) failed: %s", dec.Reason)
	}
	if model != "cheap" {
		t.Errorf("no-evidence model = %q, want cheap (price order at the neutral default)", model)
	}
	if !dec.CodeNoEvidence {
		t.Error("no-evidence decision must set CodeNoEvidence")
	}
	if !strings.Contains(dec.Reason, "no task-outcome evidence yet") {
		t.Errorf("reason %q must state the no-evidence default", dec.Reason)
	}
}

func TestResolveAutoCodeEmptyPoolFailsHonestly(t *testing.T) {
	r := New(Config{
		TierModels: map[Tier][]string{
			TierCoding: {"m1"},
		},
		CodePool: CodePoolFilter{Exclude: []string{"m1"}},
	})
	_, dec, ok := r.Resolve(CodeVirtualModel, "Write a Python function that sorts a list of numbers")
	if ok {
		t.Fatal("Resolve should fail when the pool narrows to nothing")
	}
	if dec.Model != "" {
		t.Errorf("model = %q, want empty (no model invented)", dec.Model)
	}
	if len(dec.CodePoolDropped) != 1 {
		t.Errorf("dropped = %v, want the exclusion recorded", dec.CodePoolDropped)
	}
}

func TestCostPerSolvedZeroSolveRateStaysFinite(t *testing.T) {
	// A model that never solved (10 outcomes, 0 solved) has a
	// measured rate of 0: its CPS must stay finite (the attempts
	// term caps at MaxExpectedAttempts) so the decision still
	// JSON-encodes, and it must rank last among priced models.
	cps, p, ev := CostPerSolved(0.50, TaskOutcome{Attempts: 10, Solved: 0})
	if !ev || p != 0 {
		t.Fatalf("rate = %.2f evidence %v, want 0.00/true", p, ev)
	}
	if math.IsInf(cps, 0) || math.IsNaN(cps) {
		t.Fatalf("CPS = %v, want finite (capped attempts)", cps)
	}
	if want := 0.50 * MaxExpectedAttempts * (1 + CodeFailurePenaltyK); cps != want {
		t.Errorf("CPS = %v, want %.0f (price × capped attempts × full penalty)", cps, want)
	}
	prices := func(m string) (float64, bool) { return 1.0, true }
	scores := ScoreCodeModels([]string{"never-solves", "solves"}, prices, stubOutcomes{
		"never-solves": {Attempts: 10, Solved: 0},
		"solves":       {Attempts: 10, Solved: 10},
	})
	if scores[1].Model != "never-solves" {
		t.Errorf("last = %q, want never-solves", scores[1].Model)
	}
	// And the whole decision JSON-encodes (no +Inf/NaN anywhere).
	r := New(Config{
		TierModels: map[Tier][]string{TierCoding: {"never-solves", "solves"}},
		CodePrices: prices,
		CodeOutcomes: stubOutcomes{
			"never-solves": {Attempts: 10, Solved: 0},
			"solves":       {Attempts: 10, Solved: 10},
		},
	})
	_, dec, ok := r.Resolve(CodeVirtualModel, "Write a Python function that sorts a list of numbers")
	if !ok {
		t.Fatalf("Resolve failed: %s", dec.Reason)
	}
	if _, err := json.Marshal(dec); err != nil {
		t.Errorf("decision with a zero-solve-rate model does not JSON-encode: %v", err)
	}
}

func TestIsVirtualModelCode(t *testing.T) {
	if !IsVirtualModel(CodeVirtualModel) {
		t.Error("auto/code must be a known virtual model")
	}
	if !IsVirtualModel("auto/coding") {
		t.Error("auto/coding must still be a known virtual model")
	}
}

// TestOutcomeStoreRestore (item 265): a tally recorded in one
// store, snapshotted, restored into a fresh store — the measured
// rate survives the rebuild. Invalid entries never land, a nil
// map is a no-op, and a restore replaces (never merges) the old
// tally.
func TestOutcomeStoreRestore(t *testing.T) {
	src := NewOutcomeStore()
	for i := 0; i < 10; i++ {
		src.Record("model-code", "coding", "accept")
	}
	src.Record("model-code", "coding", "edit")
	src.Record("model-partial", "coding", "accept")

	dst := NewOutcomeStore()
	dst.Record("stale-model", "coding", "accept") // replaced by the restore
	dst.Restore(src.Snapshot())
	if _, ok := dst.CodeOutcome("stale-model"); ok {
		t.Error("restore must replace the old tally, not merge into it")
	}
	o, ok := dst.CodeOutcome("model-code")
	if !ok {
		t.Fatal("model-code missing after restore")
	}
	if o.Attempts != 11 || o.Solved != 10.5 {
		t.Errorf("model-code = %+v, want 11 attempts / 10.5 solved", o)
	}
	if p, evidence := o.SolveRate(); !evidence || p != 10.5/11 {
		t.Errorf("model-code SolveRate = %.4f/%v, want %.4f/true", p, evidence, 10.5/11)
	}

	// Invalid entries are skipped, never stored.
	dst.Restore(map[string]TaskOutcome{
		"":            {Attempts: 10, Solved: 10},
		"neg":         {Attempts: -1, Solved: 0},
		"neg-solved":  {Attempts: 10, Solved: -1},
		"over-solved": {Attempts: 10, Solved: 11},
		"good":        {Attempts: 10, Solved: 8},
	})
	if _, ok := dst.CodeOutcome("model-code"); ok {
		t.Error("restore replaced the tally: model-code must be gone")
	}
	for _, bad := range []string{"", "neg", "neg-solved", "over-solved"} {
		if _, ok := dst.CodeOutcome(bad); ok {
			t.Errorf("invalid outcome %q must not be stored", bad)
		}
	}
	if o, ok := dst.CodeOutcome("good"); !ok || o.Attempts != 10 || o.Solved != 8 {
		t.Errorf("good = %+v/%v, want 10 attempts / 8 solved", o, ok)
	}

	// Nil restore is a no-op; nil store never panics.
	dst.Restore(nil)
	if o, ok := dst.CodeOutcome("good"); !ok || o.Attempts != 10 {
		t.Errorf("nil restore changed the tally: %+v/%v", o, ok)
	}
	var nilStore *OutcomeStore
	nilStore.Restore(map[string]TaskOutcome{"x": {Attempts: 1, Solved: 1}})
}
