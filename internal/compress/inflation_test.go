package compress

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

const inflationTestModel = "gpt-4o"

// countTokens is the test-side exact count, mirroring what the
// guard uses, so expected numbers below are independently
// recomputed in each test rather than copied from the guard.
func countTokens(t *testing.T, msgs []provider.Message) int {
	t.Helper()
	c, err := tokencount.CountPrompt(inflationTestModel, msgs)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if c.Estimated {
		t.Fatalf("test input unexpectedly estimated: %+v", c)
	}
	return c.Tokens
}

// (a) Synthetic inflation is caught: a hand-built compressed
// output with strictly more tokens than the original.
func TestInflationSyntheticCaught(t *testing.T) {
	original := []provider.Message{{Role: "user", Content: "Hi"}}
	compressed := []provider.Message{
		{Role: "user", Content: "Hi"},
		{Role: "assistant", Content: "Hello there, I am happy to help you with whatever you need today."},
	}
	inflated, before, after := CheckNoInflation(inflationTestModel, original, compressed)
	if !inflated {
		t.Fatalf("synthetic inflation not caught: before=%d after=%d", before, after)
	}
	if before != countTokens(t, original) || after != countTokens(t, compressed) {
		t.Fatalf("counts mismatch: guard before=%d after=%d", before, after)
	}
	if after <= before {
		t.Fatalf("test bug: synthetic case is not inflated: before=%d after=%d", before, after)
	}
	t.Logf("synthetic inflation caught: before=%d after=%d delta=+%d", before, after, after-before)
}

// (b) Equal tokens pass: a zero-saving compression is NOT
// inflation and must not be reported as a failure.
func TestInflationEqualTokensPass(t *testing.T) {
	// Identical messages: zero saving, zero inflation.
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "What is the capital of France?"},
	}
	inflated, before, after := CheckNoInflation(inflationTestModel, msgs, msgs)
	if inflated {
		t.Fatalf("identical messages reported as inflation: before=%d after=%d", before, after)
	}
	if before != after || before != countTokens(t, msgs) {
		t.Fatalf("equal case counts wrong: before=%d after=%d", before, after)
	}
	t.Logf("identical messages pass: before=%d after=%d (zero-saving, not inflation)", before, after)

	// Different content, same token count: "cat" and "dog" are
	// each a single BPE token, so swapping one for the other is
	// a zero-token change and must pass.
	orig := []provider.Message{{Role: "user", Content: "cat"}}
	comp := []provider.Message{{Role: "user", Content: "dog"}}
	inflated, before, after = CheckNoInflation(inflationTestModel, orig, comp)
	if inflated {
		t.Fatalf("same-count rewrite reported as inflation: before=%d after=%d", before, after)
	}
	if before != after {
		t.Fatalf("expected equal counts for cat/dog, got before=%d after=%d", before, after)
	}
	t.Logf("same-count rewrite passes: before=%d after=%d", before, after)

	// Empty vs empty: 0 tokens both sides, not inflation.
	inflated, before, after = CheckNoInflation(inflationTestModel, nil, nil)
	if inflated || before != 0 || after != 0 {
		t.Fatalf("empty prompts should pass at 0/0, got inflated=%v before=%d after=%d", inflated, before, after)
	}
}

// (c) A real saving passes, with exact before/after numbers:
// deduping a repeated system block, 10 -> 6 tokens.
func TestInflationRealSavingPass(t *testing.T) {
	original := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "You are helpful."},
	}
	compressed := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
	}
	inflated, before, after := CheckNoInflation(inflationTestModel, original, compressed)
	if inflated {
		t.Fatalf("real saving reported as inflation: before=%d after=%d", before, after)
	}
	if before != 10 || after != 6 {
		t.Fatalf("expected exact counts before=10 after=6, got before=%d after=%d", before, after)
	}
	t.Logf("real saving passes: before=%d after=%d saved=%d", before, after, before-after)
}

// (d) Every real inflation case from the probe is caught, with
// the exact numbers the probe measured on the Apply path
// (model gpt-4o). Mechanism: the lossy truncation marker costs
// 9 tokens, so a small overage cut nets MORE tokens than the
// input — applyLossy ALONE inflates (131->140, 131->137,
// 131->132, 565->571). Since the M7 merge wave wired the M7.7
// hard-budget post-pass into Apply, these cases now FIT instead
// of refusing: the post-pass squeezes past the marker rounding
// (the squeeze aims two markers under budget) and the final
// output costs LESS than the input, so the inflation guard has
// nothing to refuse. The guard itself is still live — see
// TestInflationRefusalShortMessage, where no fit is possible
// and the guard refuses. See the doc comment in inflation.go.
func TestInflationRealCases(t *testing.T) {
	prose100 := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 9)
	proseLong := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 40)
	prose := func(n int) []provider.Message {
		s := prose100
		if n == 40 {
			s = proseLong
		}
		return []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: s},
		}
	}
	cases := []struct {
		name       string
		msgs       []provider.Message
		budget     int
		wantBefore int
		wantAfter  int
	}{
		// 131 tokens, over by 2 -> 140 (+9).
		{"single100-over2", prose(9), 129, 131, 140},
		// 131 tokens, over by 5 -> 137 (+6).
		{"single100-over5", prose(9), 126, 131, 137},
		// 131 tokens, over by 10 -> 132 (+1).
		{"single100-over10", prose(9), 121, 131, 132},
		// 565 tokens, over by 3 -> 571 (+6).
		{"singleLong-over3", prose(40), 562, 565, 571},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// The M7.7 hard-budget post-pass (wired into Apply
			// by the M7 merge wave) fits these cases: the final
			// output is under budget, costs less than the
			// input, and names the post-pass step — the
			// inflation guard has nothing to refuse.
			out, res := Apply(inflationTestModel, c.msgs,
				&provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: c.budget}, nil, nil)
			if !res.Applied || !res.LossyApplied {
				t.Fatalf("post-pass must fit the case, got applied=%v lossy=%v res=%+v", res.Applied, res.LossyApplied, res)
			}
			if res.AfterTokens > c.budget {
				t.Fatalf("post-pass fit must be <= budget %d, got after=%d res=%+v", c.budget, res.AfterTokens, res)
			}
			if res.AfterTokens >= res.BeforeTokens || res.SavedTokens != res.BeforeTokens-res.AfterTokens {
				t.Fatalf("post-pass must save tokens honestly, got %+v", res)
			}
			if res.BeforeTokens != c.wantBefore {
				t.Fatalf("before=%d, want %d", res.BeforeTokens, c.wantBefore)
			}
			found := false
			for _, s := range res.Steps {
				if s == StepHardBudgetTruncated {
					found = true
				}
			}
			if !found {
				t.Fatalf("post-pass fit must name %q in steps %v", StepHardBudgetTruncated, res.Steps)
			}
			_ = out
			// Independent proof the UNWIRED path would have
			// inflated: run the lossy step directly on the
			// lossless output and count both sides exactly.
			losslessOut, _ := applyLossless(c.msgs, 0)
			lossyOut, _, _ := applyLossy(inflationTestModel, losslessOut, c.budget)
			before := countTokens(t, c.msgs)
			after := countTokens(t, lossyOut)
			if before != c.wantBefore || after != c.wantAfter {
				t.Fatalf("counts drifted from probe: got before=%d after=%d, want before=%d after=%d",
					before, after, c.wantBefore, c.wantAfter)
			}
			if after <= before {
				t.Fatalf("unwired lossy output must inflate: before=%d after=%d", before, after)
			}
			t.Logf("real inflation fitted by post-pass: before=%d unwired-after=%d delta=+%d (budget %d, wired-after=%d)", before, after, after-before, c.budget, res.AfterTokens)
		})
	}
}

// TestInflationRefusalShortMessage pins the inflation guard's live
// refusal path after the M7.7 post-pass landed: a short message
// with a tiny overage cannot be squeezed — every truncation nets
// zero or negative (the 9-token marker costs more than the cut
// saves), the post-pass makes no progress, and the final output
// still costs MORE than the input, so the guard refuses and the
// original comes back byte-identical. (Probed on the merge wave:
// 23-token prompt, budgets 22 and 18 both refuse.)
func TestInflationRefusalShortMessage(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Please explain in a few words why the sky looks blue during the day and red at sunset."},
	}
	if total := countTokens(t, msgs); total != 23 {
		t.Fatalf("fixture drifted: total=%d, want 23", total)
	}
	for _, budget := range []int{22, 18} {
		out, res := Apply(inflationTestModel, msgs,
			&provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget}, nil, nil)
		assertInflationRefusal(t, msgs, out, res, 23)
	}
}

// assertInflationRefusal checks the Apply-level inflation-guard
// refusal contract, mirroring assertGateRefusal in
// fidelity_test.go: the original messages come back
// byte-identical, nothing is reported applied, no tokens are
// claimed saved, and the only step recorded is the inflation
// guard refusal, with after==before==wantBefore.
func assertInflationRefusal(t *testing.T, orig, out []provider.Message, res *provider.CompressionResult, wantBefore int) {
	t.Helper()
	if !reflect.DeepEqual(orig, out) {
		t.Fatalf("inflation refusal must return the original messages byte-identical, got %d messages vs %d", len(out), len(orig))
	}
	if res.Applied || res.LossyApplied || res.Lossless {
		t.Fatalf("inflation refusal must report nothing applied, got %+v", res)
	}
	if res.SavedTokens != 0 {
		t.Fatalf("inflation refusal must save 0 tokens, got %+v", res)
	}
	if res.BeforeTokens != wantBefore || res.AfterTokens != res.BeforeTokens {
		t.Fatalf("inflation refusal must report after==before==%d, got %+v", wantBefore, res)
	}
	if len(res.Steps) != 1 || res.Steps[0] != StepInflationGuardRefused {
		t.Fatalf("inflation refusal must record exactly [%q], got %v", StepInflationGuardRefused, res.Steps)
	}
}

// (f) Fidelity first: when a lossy result BOTH damages a URL and
// inflates, Apply must refuse on the fidelity gate alone — the
// inflation step must never appear alongside it, preserving the
// len(Steps)==1 refusal invariant. Geometry: the paren URL
// straddles the ~59% head boundary of the user message, so a
// barely-over-budget truncation (budget = total-2) cuts it
// mid-URL while the marker makes the result more expensive.
func TestInflationGuardSkippedOnFidelityRefusal(t *testing.T) {
	content := fidelityProse(12) + "See " + fidelityParenURL + " for details.\n" + fidelityProse(9)
	msgs := fidelitySystemUser(content)
	total := countTokens(t, msgs)
	budget := total - 2
	// Preconditions, derived from the real lossy output (not
	// assumed): the unwired output must BOTH damage the URL
	// and inflate.
	losslessOut, _ := applyLossless(msgs, 0)
	lossyOut, _, _ := applyLossy(inflationTestModel, losslessOut, budget)
	if ok, _ := CheckLossy(msgs, lossyOut); ok {
		t.Fatalf("test setup: the lossy output must damage the paren URL (budget %d, total %d)", budget, total)
	}
	if inflated, before, after := CheckNoInflation(inflationTestModel, msgs, lossyOut); !inflated {
		t.Fatalf("test setup: the lossy output must inflate (before=%d after=%d, budget %d)", before, after, budget)
	}
	// The wired path refuses on fidelity alone.
	out, res := Apply(inflationTestModel, msgs,
		&provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget}, nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

// (e) Counting-error / estimated path fails closed. A true BPE
// load error is not triggerable through the public API (the
// tables are vendored and embedded), so the observable
// fail-closed trigger is the Estimated path: any side over the
// 4 MiB exact-count cap is counted as chars/4, and the guard
// must treat that unprovable cost as inflation — even when the
// two estimated counts are equal, and even when the estimated
// side looks like a saving.
func TestInflationEstimatedFailsClosed(t *testing.T) {
	huge := strings.Repeat("x", 4*1024*1024+100) // > 4 MiB cap
	hugeMsg := []provider.Message{{Role: "user", Content: huge}}
	smallMsg := []provider.Message{{Role: "user", Content: "Hi"}}

	// Estimated count is chars/4 of the joined text
	// (len(content)+1 for the separator, /4).
	wantHuge := (len(huge) + 1) / 4

	// Both sides huge and identical: equal estimated counts,
	// still inflated — the cost cannot be proven.
	inflated, before, after := CheckNoInflation(inflationTestModel, hugeMsg, hugeMsg)
	if !inflated {
		t.Fatalf("estimated input not failed closed: before=%d after=%d", before, after)
	}
	if before != wantHuge || after != wantHuge {
		t.Fatalf("expected estimated counts %d/%d, got before=%d after=%d", wantHuge, wantHuge, before, after)
	}
	t.Logf("both-sides estimated fails closed: before=%d after=%d (chars/4 estimate)", before, after)

	// Only the compressed side is estimated: 2 -> ~1M estimated tokens
	// is an apparent inflation, but the point is the guard must not
	// pass an unprovable count either way — it fails closed whether
	// the untrusted number looks like a saving or an inflation.
	inflated, before, after = CheckNoInflation(inflationTestModel, smallMsg, hugeMsg)
	if !inflated {
		t.Fatalf("estimated compressed side not failed closed: before=%d after=%d", before, after)
	}
	if before != 2 || after != wantHuge {
		t.Fatalf("expected before=2 after=%d, got before=%d after=%d", wantHuge, before, after)
	}
	t.Logf("estimated compressed side fails closed: before=%d after=%d", before, after)

	// Only the original side is estimated, compressed side is a
	// tiny exact count: looks like a huge saving, still fails
	// closed — the original cost was never proven.
	inflated, before, after = CheckNoInflation(inflationTestModel, hugeMsg, smallMsg)
	if !inflated {
		t.Fatalf("estimated original side not failed closed: before=%d after=%d", before, after)
	}
	if before != wantHuge || after != 2 {
		t.Fatalf("expected before=%d after=2, got before=%d after=%d", wantHuge, before, after)
	}
	t.Logf("estimated original side fails closed: before=%d after=%d", before, after)
}

// Probe, kept as a permanent test: run Apply over the varied
// inputs the M7.3 probe used, and assert the guard's verdict
// agrees with the directly measured counts on every case
// (inflated exactly when after > before). The numbers are
// logged so a future change in compression behaviour shows up
// here as a changed line, not a silent drift.
func TestInflationProbeAgreement(t *testing.T) {
	prose100 := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 9)
	proseLong := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 40)
	multi := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "First question here please."},
		{Role: "assistant", Content: "First answer here please."},
		{Role: "user", Content: "Second question here please."},
		{Role: "assistant", Content: "Second answer here please."},
		{Role: "user", Content: "Third and final question here please."},
	}
	toolOnly := []provider.Message{
		{Role: "assistant", Content: "", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city": "Paris", "units": "metric"}`}}}},
		{Role: "tool", Content: `{"temp": 21, "cond": "sunny"}`, ToolCallID: "call_1"},
		{Role: "user", Content: "Thanks."},
	}
	single100 := []provider.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: prose100}}
	singleLong := []provider.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: proseLong}}

	type tc struct {
		name  string
		msgs  []provider.Message
		lossy int // 0 = lossless only
	}
	cases := []tc{
		{"tiny-lossy1", []provider.Message{{Role: "user", Content: "Hi"}}, 1},
		{"tiny-lossless", []provider.Message{{Role: "user", Content: "Hi"}}, 0},
		{"short-over1", []provider.Message{{Role: "user", Content: "Hello, how are you today?"}}, 6},
		{"short-over2", []provider.Message{{Role: "user", Content: "Hello, how are you today?"}}, 5},
		{"single100-over2", single100, 129},
		{"single100-over5", single100, 126},
		{"single100-over10", single100, 121},
		{"single100-over20", single100, 111},
		{"single100-half", single100, 65},
		{"singleLong-over3", singleLong, 562},
		{"singleLong-half", singleLong, 282},
		{"single100-under-budget", single100, 181},
		{"single100-at-budget", single100, 131},
		{"empty-lossless", []provider.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: ""}}, 0},
		{"empty-lossy1", []provider.Message{{Role: "system", Content: "You are helpful."}, {Role: "user", Content: ""}}, 1},
		{"toolonly-lossless", toolOnly, 0},
		{"toolonly-lossy2", toolOnly, 2},
		{"multi-over3", multi, 28},
		{"wsjoin-lossless", []provider.Message{{Role: "user", Content: "hello   \nworld   \n\n\nfoo   "}}, 0},
		{"dupsys-lossless", []provider.Message{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "hi"},
			{Role: "system", Content: "You are helpful."},
		}, 0},
		{"repeat-a-lossy", []provider.Message{{Role: "user", Content: strings.Repeat("a", 400)}}, 10},
		{"repeat-word-lossy", []provider.Message{{Role: "user", Content: strings.Repeat("hello ", 100)}}, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := &provider.CompressionRequest{Lossless: boolPtr(true)}
			if c.lossy > 0 {
				req.LossyMaxPromptTokens = c.lossy
			}
			out, _ := Apply(inflationTestModel, c.msgs, req, nil, nil)
			before := countTokens(t, c.msgs)
			after := countTokens(t, out)
			inflated, gb, ga := CheckNoInflation(inflationTestModel, c.msgs, out)
			if gb != before || ga != after {
				t.Fatalf("guard counts disagree with direct count: guard %d->%d, direct %d->%d", gb, ga, before, after)
			}
			if inflated != (after > before) {
				t.Fatalf("guard verdict wrong: inflated=%v but before=%d after=%d", inflated, before, after)
			}
			t.Logf("probe %-22s lossy=%-4d before=%-4d after=%-4d delta=%+d inflated=%v",
				c.name, c.lossy, before, after, after-before, inflated)
		})
	}
}
