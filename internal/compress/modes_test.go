package compress

import (
	"strings"
	"testing"
)

func stepEngines(t *testing.T, p ModePipeline) []string {
	t.Helper()
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.Engine)
	}
	return out
}

func equalStrings(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestParseMode(t *testing.T) {
	for _, in := range []string{"off", "lite", "standard", "aggressive", "ultra", "rtk", "stacked"} {
		m, err := ParseMode(in)
		if err != nil {
			t.Fatalf("ParseMode(%q): %v", in, err)
		}
		if string(m) != in {
			t.Fatalf("ParseMode(%q) = %q", in, m)
		}
	}
	// Case-insensitive, whitespace-tolerant.
	m, err := ParseMode("  LiTe ")
	if err != nil || m != ModeLite {
		t.Fatalf("ParseMode(\"  LiTe \") = %q, %v; want lite, nil", m, err)
	}
	for _, bad := range []string{"", "medium", "caveman", "lite\nx"} {
		if _, err := ParseMode(bad); err == nil {
			t.Errorf("ParseMode(%q) accepted, want error", bad)
		}
	}
}

// TestResolveModeTable pins the exact step list of every mode under
// today's availability. If the mapping is removed or reordered,
// this test fails — that is the toggle: the table IS the contract.
func TestResolveModeTable(t *testing.T) {
	avail := DefaultAvailability()
	lossless := []string{StepWhitespace, StepDedupeSystem, StepToolResults}
	lossy := []string{StepLossyDropped, StepLossyTruncated}
	cases := []struct {
		mode      Mode
		steps     []string
		available bool
		lossy     bool
		unavail   []string
	}{
		{ModeOff, nil, true, false, nil},
		{ModeLite, lossless, true, false, nil},
		{ModeStandard, append(append([]string{}, lossless...), EngineCaveman), true, true, nil},
		{ModeAggressive, append(append(append([]string{}, lossless...), lossy...), EngineMessageAging, EngineSummarization), false, true, []string{EngineSummarization}},
		{ModeUltra, append(append(append([]string{}, lossless...), lossy...), EngineHeuristicPrune, EngineSLMTier), false, true, []string{EngineSLMTier}},
		{ModeRTK, []string{EngineRTK}, true, true, nil},
		{ModeStacked, append(append(append([]string{}, lossless...), lossy...), EngineRTK, EngineCaveman), true, true, nil},
	}
	for _, c := range cases {
		p, err := ResolveMode(c.mode, 0, avail)
		if err != nil {
			t.Fatalf("ResolveMode(%q): %v", c.mode, err)
		}
		equalStrings(t, stepEngines(t, p), c.steps)
		if p.Available != c.available {
			t.Errorf("mode %q: Available=%v, want %v (unavailable=%v)", c.mode, p.Available, c.available, p.Unavailable)
		}
		if p.Lossy != c.lossy {
			t.Errorf("mode %q: Lossy=%v, want %v", c.mode, p.Lossy, c.lossy)
		}
		equalStrings(t, p.Unavailable, c.unavail)
		if p.Lossy && !p.RequiresLossyOptIn {
			t.Errorf("mode %q: lossy but RequiresLossyOptIn=false", c.mode)
		}
		if !p.Lossy && p.RequiresLossyOptIn {
			t.Errorf("mode %q: not lossy but RequiresLossyOptIn=true", c.mode)
		}
	}
	// Unknown mode and negative budget are errors, never a silent mode.
	if _, err := ResolveMode("medium", 0, avail); err == nil {
		t.Error("ResolveMode(\"medium\") accepted, want error")
	}
	if _, err := ResolveMode(ModeLite, -1, avail); err == nil {
		t.Error("ResolveMode(lite, -1) accepted, want error")
	}
}

// TestResolveModeBudgets pins per-step budgets: lossless steps are
// never capped; the lossy pair carries the caller's budget.
func TestResolveModeBudgets(t *testing.T) {
	avail := DefaultAvailability()
	p, err := ResolveMode(ModeAggressive, 1000, avail)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Steps {
		switch s.Engine {
		case StepWhitespace, StepDedupeSystem, StepToolResults,
			EngineMessageAging, EngineSummarization:
			if s.BudgetTokens != 0 {
				t.Errorf("step %q: BudgetTokens=%d, want 0 (uncapped)", s.Engine, s.BudgetTokens)
			}
		case StepLossyDropped, StepLossyTruncated:
			if s.BudgetTokens != 1000 {
				t.Errorf("step %q: BudgetTokens=%d, want 1000", s.Engine, s.BudgetTokens)
			}
		}
	}
	// Zero budget = no target: lossy steps uncapped.
	p, err = ResolveMode(ModeAggressive, 0, avail)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Steps {
		if (s.Engine == StepLossyDropped || s.Engine == StepLossyTruncated) && s.BudgetTokens != 0 {
			t.Errorf("step %q: BudgetTokens=%d with zero budget, want 0", s.Engine, s.BudgetTokens)
		}
	}
}

// TestResolveModeAvailabilityToggle proves the mapping keys off
// availability, not hardcoded truth: flip Caveman off and standard
// becomes unavailable (its Caveman step is the only unavailable
// one); flip lossless off and lite stops being available.
func TestResolveModeAvailabilityToggle(t *testing.T) {
	avail := DefaultAvailability()
	avail.Caveman = false
	p, err := ResolveMode(ModeStandard, 0, avail)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available {
		t.Fatalf("standard with Caveman unavailable: Available=true, want false")
	}
	if len(p.Unavailable) != 1 || p.Unavailable[0] != EngineCaveman {
		t.Fatalf("standard with Caveman unavailable: Unavailable=%v, want [%q]", p.Unavailable, EngineCaveman)
	}
	equalStrings(t, stepEngines(t, p), []string{StepWhitespace, StepDedupeSystem, StepToolResults, EngineCaveman})

	avail2 := DefaultAvailability()
	avail2.Lossless = false
	p, err = ResolveMode(ModeLite, 0, avail2)
	if err != nil {
		t.Fatal(err)
	}
	if p.Available {
		t.Fatal("lite with Lossless=false: Available=true, want false")
	}
	if len(p.Unavailable) != 3 {
		t.Fatalf("lite with Lossless=false: Unavailable=%v, want all 3 lossless steps", p.Unavailable)
	}

	// Every unavailable step carries a non-empty reason.
	avail3 := DefaultAvailability()
	for _, m := range []Mode{ModeStandard, ModeAggressive, ModeUltra, ModeRTK, ModeStacked} {
		p, err := ResolveMode(m, 0, avail3)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range p.Steps {
			if !s.Available && strings.TrimSpace(s.UnavailableReason) == "" {
				t.Errorf("mode %q step %q unavailable with empty reason", m, s.Engine)
			}
			if s.Available && s.UnavailableReason != "" {
				t.Errorf("mode %q step %q available but carries reason %q", m, s.Engine, s.UnavailableReason)
			}
		}
	}
}

func TestEscalationLadder(t *testing.T) {
	avail := DefaultAvailability()
	ladder := EscalationLadder(avail)
	wantOrder := []string{
		EngineSessionDedup, EngineRTK, EngineHeadroom,
		EngineLite,
		EngineCaveman, EngineMessageAging, EngineHeuristicPrune,
	}
	if len(ladder) != len(wantOrder) {
		t.Fatalf("ladder has %d stages, want %d: %v", len(ladder), len(wantOrder), ladder)
	}
	for i, want := range wantOrder {
		if ladder[i].Engine != want {
			t.Errorf("ladder[%d] = %q, want %q", i, ladder[i].Engine, want)
		}
	}
	// Today lite, rtk, caveman, message-aging and heuristic-pruning
	// are live (M7.4/M7.5 landed and the M7 merge wave wired them
	// in; B4-31/B4-33 wired aging and pruning in Wave 12); every
	// other stage reports unavailable WITH a reason — never
	// silently absent.
	live := map[string]bool{EngineLite: true, EngineRTK: true, EngineCaveman: true, EngineMessageAging: true, EngineHeuristicPrune: true}
	for _, s := range ladder {
		if live[s.Engine] {
			if !s.Available {
				t.Errorf("ladder stage %q unavailable, want available", s.Engine)
			}
			continue
		}
		if s.Available {
			t.Errorf("ladder stage %q available, want unavailable (engine does not exist)", s.Engine)
		}
		if strings.TrimSpace(s.UnavailableReason) == "" {
			t.Errorf("ladder stage %q unavailable with empty reason", s.Engine)
		}
	}
	// Intensities mirror the sector-typical ladder: rtk standard, caveman full.
	for _, s := range ladder {
		switch s.Engine {
		case EngineRTK:
			if s.Intensity != "standard" {
				t.Errorf("rtk intensity %q, want standard", s.Intensity)
			}
		case EngineCaveman:
			if s.Intensity != "full" {
				t.Errorf("caveman intensity %q, want full", s.Intensity)
			}
		}
	}
	// Flipping RTK off flips exactly its stage.
	avail.RTK = false
	ladder = EscalationLadder(avail)
	for _, s := range ladder {
		if s.Engine == EngineRTK {
			if s.Available {
				t.Error("rtk ladder stage still available after RTK=false")
			}
		} else if s.Engine == EngineLite || s.Engine == EngineCaveman {
			if !s.Available {
				t.Errorf("ladder stage %q flipped with RTK=false, want available", s.Engine)
			}
		}
	}
}

func TestAutoTrigger(t *testing.T) {
	cfg := DefaultAutoTriggerConfig() // threshold 8000
	cases := []struct {
		prompt, tool int
		want         Mode
	}{
		{0, 0, ModeOff},             // empty
		{-5, 0, ModeOff},            // negative
		{100, 0, ModeOff},           // small prompt -> none
		{7999, 0, ModeOff},          // just under threshold
		{8000, 0, ModeLite},         // at threshold
		{12000, 0, ModeLite},        // 1x-2x
		{15999, 0, ModeLite},        // just under 2x
		{16000, 0, ModeStandard},    // 2x
		{30000, 0, ModeStandard},    // 2x-4x
		{32000, 0, ModeAggressive},  // 4x
		{60000, 0, ModeAggressive},  // 4x-8x
		{64000, 0, ModeUltra},       // 8x
		{100000, 0, ModeUltra},      // large prompt -> strongest single mode
		{16000, 9000, ModeRTK},      // tool-heavy (>=50%) at 2x -> rtk
		{20000, 9999, ModeStandard}, // tool just under half -> size rule wins
		{64000, 40000, ModeStacked}, // tool-heavy at 8x -> stacked
		{100000, 60000, ModeStacked},
	}
	for _, c := range cases {
		got, reason := AutoTrigger(AutoTriggerInput{PromptTokens: c.prompt, ToolTokens: c.tool}, cfg)
		if got != c.want {
			t.Errorf("AutoTrigger(%d prompt, %d tool) = %q (%s), want %q", c.prompt, c.tool, got, reason, c.want)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("AutoTrigger(%d, %d): empty reason", c.prompt, c.tool)
		}
	}
	// Zero config takes the defaults (threshold 8000, not 0).
	got, _ := AutoTrigger(AutoTriggerInput{PromptTokens: 8000}, AutoTriggerConfig{})
	if got != ModeLite {
		t.Errorf("AutoTrigger with zero config at 8000 tokens = %q, want lite (default threshold)", got)
	}
	// Deterministic: same input, same output, twice.
	a, _ := AutoTrigger(AutoTriggerInput{PromptTokens: 50000, ToolTokens: 1000}, cfg)
	b, _ := AutoTrigger(AutoTriggerInput{PromptTokens: 50000, ToolTokens: 1000}, cfg)
	if a != b {
		t.Errorf("AutoTrigger not deterministic: %q vs %q", a, b)
	}
}

func TestEstimateAfterTokens(t *testing.T) {
	// Off is exact; lite uses the MEASURED M7.1 factor 0.902.
	after, src := EstimateAfterTokens(ModeOff, 10000)
	if after != 10000 || src != EstimateExact {
		t.Errorf("off estimate = (%d, %q), want (10000, exact)", after, src)
	}
	after, src = EstimateAfterTokens(ModeLite, 10000)
	if after != 9020 || src != EstimateMeasured {
		t.Errorf("lite estimate = (%d, %q), want (9020, measured-m7.1-corpus)", after, src)
	}
	// Unmeasured engines are labelled as published planning factors.
	after, src = EstimateAfterTokens(ModeStandard, 10000)
	if after != 7000 || src != EstimatePublishedPlanningFactor {
		t.Errorf("standard estimate = (%d, %q), want (7000, published-planning)", after, src)
	}
	after, src = EstimateAfterTokens(ModeStacked, 10000)
	if after != 5950 || src != EstimateDerived {
		t.Errorf("stacked estimate = (%d, %q), want (5950, derived rtk x caveman)", after, src)
	}
	after, _ = EstimateAfterTokens(ModeLite, 0)
	if after != 0 {
		t.Errorf("lite estimate at 0 tokens = %d, want 0", after)
	}
}

// TestDialCheapestFitting checks the dial under TODAY's
// availability (off, lite, standard, rtk and stacked are live
// since the M7 merge wave; aggressive and ultra are not) and under
// a hypothetical all-available build (proving the escalation logic
// itself works and will engage as the remaining engines land).
func TestDialCheapestFitting(t *testing.T) {
	avail := DefaultAvailability()
	// Already fits: cheapest is off, no escalation, exact fit.
	res, err := PickCheapestFitting(5000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeOff || !res.Fit || res.EstimatedAfterTokens != 5000 {
		t.Errorf("already-fits dial = %+v, want off/fit/5000", res)
	}
	// Needs lite: 10000 tokens, budget 9500. Lite estimates 9020
	// (fits); off estimates 10000 (does not).
	res, err = PickCheapestFitting(10000, 9500, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeLite || !res.Fit || res.EstimatedAfterTokens != 9020 {
		t.Errorf("needs-lite dial = %+v, want lite/fit/9020", res)
	}
	if res.EstimateSource != EstimateMeasured {
		t.Errorf("needs-lite estimate source = %q, want measured", res.EstimateSource)
	}
	// Needs standard: budget 8000, prompt 10000. Lite estimates
	// 9020 > 8000; standard estimates 7000 (fits) and is the
	// cheapest fitting mode. Aggressive and ultra are
	// unavailable and must be NAMED in SkippedUnavailable — the
	// dial scans past them on the way to stacked.
	res, err = PickCheapestFitting(10000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStandard || !res.Fit || res.EstimatedAfterTokens != 7000 {
		t.Errorf("needs-standard dial = %+v, want standard/fit/7000", res)
	}
	if res.EstimateSource != EstimatePublishedPlanningFactor {
		t.Errorf("needs-standard estimate source = %q, want published-planning", res.EstimateSource)
	}
	if res.StrongestAvailable != ModeStacked {
		t.Errorf("StrongestAvailable = %q, want stacked", res.StrongestAvailable)
	}
	// Nothing available fits: budget 4000, prompt 10000. Lite
	// 9020 no, standard 7000 no, stacked 5950 no; aggressive
	// and ultra are unavailable and must be NAMED in
	// SkippedUnavailable, not silently skipped.
	res, err = PickCheapestFitting(10000, 4000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStacked || res.Fit {
		t.Errorf("no-fit dial = %+v, want stacked/no-fit", res)
	}
	if res.EstimatedAfterTokens != 5950 {
		t.Errorf("no-fit dial estimate = %d, want 5950", res.EstimatedAfterTokens)
	}
	if res.StrongestAvailable != ModeStacked {
		t.Errorf("StrongestAvailable = %q, want stacked", res.StrongestAvailable)
	}
	skipped := map[Mode]bool{}
	for _, s := range res.SkippedUnavailable {
		skipped[s.Mode] = true
		if strings.TrimSpace(s.Reason) == "" {
			t.Errorf("skipped mode %q has empty reason", s.Mode)
		}
	}
	for _, m := range []Mode{ModeAggressive, ModeUltra} {
		if !skipped[m] {
			t.Errorf("no-fit dial did not record skipped unavailable mode %q: %v", m, res.SkippedUnavailable)
		}
	}
	for _, m := range []Mode{ModeLite, ModeStandard, ModeRTK, ModeStacked} {
		if skipped[m] {
			t.Errorf("no-fit dial wrongly recorded available mode %q as skipped: %v", m, res.SkippedUnavailable)
		}
	}
	// Errors: no budget, negative prompt.
	if _, err := PickCheapestFitting(10000, 0, avail); err == nil {
		t.Error("PickCheapestFitting with budget 0 accepted, want error")
	}
	if _, err := PickCheapestFitting(-1, 8000, avail); err == nil {
		t.Error("PickCheapestFitting with negative prompt accepted, want error")
	}

	// All-available build: the dial walks up EXACTLY as far as
	// needed. 10000 tokens, budget 6500: lite 9020 no, standard
	// 7000 no, aggressive 5500 yes -> aggressive.
	all := EngineAvailability{Lossless: true, LossyDropTruncate: true, RTK: true, Caveman: true, SessionDedup: true, Headroom: true, MessageAging: true, Summarization: true, HeuristicPruning: true, SLMTier: true}
	res, err = PickCheapestFitting(10000, 6500, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeAggressive || !res.Fit || res.EstimatedAfterTokens != 5500 {
		t.Errorf("all-available dial = %+v, want aggressive/fit/5500", res)
	}
	if len(res.SkippedUnavailable) != 0 {
		t.Errorf("all-available dial skipped %v, want none", res.SkippedUnavailable)
	}
	// Same build, budget 4000: ultra 4000 fits exactly -> ultra.
	res, err = PickCheapestFitting(10000, 4000, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeUltra || !res.Fit {
		t.Errorf("ultra-fit dial = %+v, want ultra/fit", res)
	}
}

func TestEscalateFrom(t *testing.T) {
	avail := DefaultAvailability()
	// Fits at start: no escalation.
	res, err := EscalateFrom(ModeLite, 9000, 10000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeLite || !res.Fit || len(res.Escalations) != 0 {
		t.Errorf("fits-at-start = %+v, want lite/fit/no escalations", res)
	}
	// From off, budget 8000, prompt 10000: off 10000 no -> lite
	// 9020 no -> standard 7000 yes. Exactly two escalations,
	// and no unavailable mode is passed over (the walk stops at
	// the first fit, before aggressive/ultra are scanned).
	res, err = EscalateFrom(ModeOff, 10000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStandard || !res.Fit || res.EstimatedAfterTokens != 7000 {
		t.Errorf("escalate-from-off = %+v, want standard/fit/7000", res)
	}
	if len(res.Escalations) != 2 || res.Escalations[0].From != ModeOff || res.Escalations[0].To != ModeLite ||
		res.Escalations[1].From != ModeLite || res.Escalations[1].To != ModeStandard {
		t.Errorf("escalations = %+v, want exactly [off->lite, lite->standard]", res.Escalations)
	}
	if len(res.SkippedUnavailable) != 0 {
		t.Errorf("escalation recorded skipped modes %v, want none (walk stopped at first fit)", res.SkippedUnavailable)
	}
	// From lite with a budget lite cannot meet: one escalation
	// to standard, which fits.
	res, err = EscalateFrom(ModeLite, 10000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStandard || !res.Fit || len(res.Escalations) != 1 ||
		res.Escalations[0].From != ModeLite || res.Escalations[0].To != ModeStandard {
		t.Errorf("escalate-from-lite = %+v, want standard/fit/[lite->standard]", res)
	}
	// Start mode unavailable: recorded as a skip, escalation begins
	// from the next stronger available mode (stacked, which fits)
	// with the fallback recorded.
	res, err = EscalateFrom(ModeUltra, 10000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStacked || !res.Fit {
		t.Errorf("escalate-from-unavailable-ultra = %+v, want stacked/fit", res)
	}
	found := false
	for _, s := range res.SkippedUnavailable {
		if s.Mode == ModeUltra {
			found = true
		}
	}
	if !found {
		t.Errorf("escalation from unavailable ultra did not record the skip: %+v", res)
	}
	if len(res.Escalations) != 1 {
		t.Errorf("escalations = %+v, want exactly one fallback record", res.Escalations)
	}
	// Already fits at an unavailable start mode: honest answer is
	// off (fits, available), with the start recorded as skipped.
	// Aggressive is the unavailable start now (standard is live).
	res, err = EscalateFrom(ModeAggressive, 5000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeOff || !res.Fit {
		t.Errorf("fits-at-unavailable-start = %+v, want off/fit", res)
	}
	found = false
	for _, s := range res.SkippedUnavailable {
		if s.Mode == ModeAggressive {
			found = true
		}
	}
	if !found {
		t.Errorf("fits-at-unavailable-start did not record the aggressive skip: %+v", res)
	}
	// Errors: unknown start, no budget, negative prompt.
	if _, err := EscalateFrom("medium", 10000, 8000, avail); err == nil {
		t.Error("EscalateFrom(\"medium\") accepted, want error")
	}
	if _, err := EscalateFrom(ModeLite, 10000, 0, avail); err == nil {
		t.Error("EscalateFrom with budget 0 accepted, want error")
	}
	if _, err := EscalateFrom(ModeLite, -1, 8000, avail); err == nil {
		t.Error("EscalateFrom with negative prompt accepted, want error")
	}

	// All-available build: escalation walks exactly one rung at a
	// time. From lite, 10000 tokens, budget 6500: lite 9020 no ->
	// standard 7000 no -> aggressive 5500 yes. Two escalations.
	all := EngineAvailability{Lossless: true, LossyDropTruncate: true, RTK: true, Caveman: true, SessionDedup: true, Headroom: true, MessageAging: true, Summarization: true, HeuristicPruning: true, SLMTier: true}
	res, err = EscalateFrom(ModeLite, 10000, 6500, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeAggressive || !res.Fit {
		t.Errorf("all-available escalation = %+v, want aggressive/fit", res)
	}
	if len(res.Escalations) != 2 ||
		res.Escalations[0].From != ModeLite || res.Escalations[0].To != ModeStandard ||
		res.Escalations[1].From != ModeStandard || res.Escalations[1].To != ModeAggressive {
		t.Errorf("escalations = %+v, want [lite->standard, standard->aggressive]", res.Escalations)
	}
	// Never loops: budget 1 forces the walk to the top and stops.
	res, err = EscalateFrom(ModeOff, 10000, 1, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStacked || res.Fit {
		t.Errorf("walk-to-top = %+v, want stacked/no-fit", res)
	}
	if len(res.Escalations) != 5 { // off->lite->standard->aggressive->ultra->stacked
		t.Errorf("walk-to-top escalations = %d, want 5: %+v", len(res.Escalations), res.Escalations)
	}
}

// TestEscalateFromRTKStart covers the RTK-start path (review nit 3):
// RTK has no rank, so its start is special-cased. RTK is available
// since the M7 merge wave, so the unavailable-start case below uses
// an explicit RTK=false availability; the default-availability cases
// cover RTK available (fits, and does-not-fit escalating to stacked).
func TestEscalateFromRTKStart(t *testing.T) {
	avail := DefaultAvailability()
	// RTK available but does not fit: 10000 tokens, budget 8000
	// (rtk 8500). RTK->stacked, stacked 5950 fits.
	res, err := EscalateFrom(ModeRTK, 10000, 8000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStacked || !res.Fit {
		t.Errorf("rtk-no-fit = %+v, want stacked/fit", res)
	}
	if len(res.Escalations) != 1 || res.Escalations[0].From != ModeRTK || res.Escalations[0].To != ModeStacked {
		t.Errorf("rtk-no-fit escalations = %+v, want exactly [rtk->stacked]", res.Escalations)
	}
	if len(res.SkippedUnavailable) != 0 {
		t.Errorf("rtk-no-fit recorded skipped modes %v, want none", res.SkippedUnavailable)
	}
	// RTK unavailable (explicit override): 10000 tokens, budget
	// 8000. The fallback records BOTH the skip and a fallback
	// escalation entry (RTK->lite), like the generic
	// unavailable-start path; the walk then continues lite ->
	// standard, which fits.
	rtkOff := DefaultAvailability()
	rtkOff.RTK = false
	res, err = EscalateFrom(ModeRTK, 10000, 8000, rtkOff)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeStandard || !res.Fit {
		t.Errorf("rtk-unavailable = %+v, want standard/fit", res)
	}
	if len(res.Escalations) != 2 ||
		res.Escalations[0].From != ModeRTK || res.Escalations[0].To != ModeLite ||
		res.Escalations[1].From != ModeLite || res.Escalations[1].To != ModeStandard {
		t.Errorf("rtk-unavailable escalations = %+v, want exactly [rtk->lite, lite->standard]", res.Escalations)
	}
	skipped := map[Mode]bool{}
	for _, s := range res.SkippedUnavailable {
		skipped[s.Mode] = true
	}
	if !skipped[ModeRTK] {
		t.Errorf("rtk-unavailable did not record the RTK skip: %+v", res)
	}
	// RTK available and fits: 10000 tokens, budget 9000 (rtk 8500).
	res, err = EscalateFrom(ModeRTK, 10000, 9000, avail)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedMode != ModeRTK || !res.Fit || len(res.Escalations) != 0 {
		t.Errorf("rtk-available-fits = %+v, want rtk/fit/no escalations", res)
	}
	if res.EstimatedAfterTokens != 8500 {
		t.Errorf("rtk-available-fits estimate = %d, want 8500", res.EstimatedAfterTokens)
	}
}
