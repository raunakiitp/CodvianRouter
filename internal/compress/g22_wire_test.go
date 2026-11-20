package compress

import (
	"fmt"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// G22 wire-only tests (PARITY.md Area 10 rows :848/:865/:866/:867/
// :868/:869). Each test is a TOGGLE proof: the asserted behaviour
// must change when the wire it proves is removed — a test that
// passes with the wire deleted is marked as such and does not
// count as wiring evidence (none here are).

// g22FreezeFixture is a prompt where the system message is the
// ONLY plain-text message the lossy truncation can cut: the last
// user message is part-carrying (never truncated, only dropped
// whole or kept — and it is pinned, so kept), and the assistant
// message is short plain text (dropped whole in the drop phase).
// With a budget a little under the total, the truncation must
// come out of the system prompt — unless it is frozen, in which
// case the request honestly does not fit.
func g22FreezeFixture() (msgs []provider.Message, sysText string) {
	sysText = strings.Repeat("The system instruction is to answer clearly and completely with citations for every claim made. ", 40)
	userText := strings.Repeat("Please keep the answer short and focused on the question asked. ", 12)
	return []provider.Message{
		{Role: "system", Content: sysText},
		{Role: "assistant", Content: "Working on it."},
		{Role: "user", Parts: []provider.ContentPart{{Type: "text", Text: userText}}, Content: userText},
	}, sysText
}

// g22FreezeBudget sizes a budget that forces a truncation out of
// the system prompt: total minus 50 tokens (the assistant drop
// saves only a few, so ~45 must come from truncation).
func g22FreezeBudget(t *testing.T, msgs []provider.Message) int {
	t.Helper()
	total := countTokens(t, msgs)
	return total - 50
}

func g22LossyReq(preserveMode string, budget int) *provider.CompressionRequest {
	return &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget, PreserveSystemPromptMode: preserveMode}
}

// TestG22PreserveModes proves row :869: Apply honours the
// preserve mode, default "always". frozen=true means the system
// prompt comes back byte-identical AND the request honestly
// reports not fitting (the frozen prefix made the budget
// unreachable — a fake fit would be worse than the miss).
// frozen=false means the system prompt IS truncated to fit.
// Either assertion fails if the freeze wire is removed: without
// it every case truncates the system prompt.
func TestG22PreserveModes(t *testing.T) {
	cases := []struct {
		name     string
		preserve string
		opts     ApplyOptions
		frozen   bool
	}{
		{"default empty mode freezes", "", ApplyOptions{}, true},
		{"always freezes", "always", ApplyOptions{}, true},
		{"whenNoCache with caching provider freezes", "whenNoCache", ApplyOptions{IsCachingProvider: true}, true},
		{"whenNoCache with cache_control freezes", "whenNoCache", ApplyOptions{HasCacheControl: true}, true},
		{"whenNoCache with no cache does not freeze", "whenNoCache", ApplyOptions{}, false},
		{"never does not freeze", "never", ApplyOptions{}, false},
		{"unknown mode defaults to always", "bogus", ApplyOptions{}, true},
		{"explicit freeze seam beats never", "never", ApplyOptions{FreezeSystemPrompt: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs, _ := g22FreezeFixture()
			// The frozen expectation is the LOSSLESS normal
			// form, not the raw input: the whitespace step
			// legitimately strips the fixture's trailing
			// space, and "frozen" means lossy left it alone
			// after that.
			wantSys := findRole(Normalise(msgs), "system").Content
			budget := g22FreezeBudget(t, msgs)
			out, res := ApplyWithOptions("gpt-4o", msgs, g22LossyReq(tc.preserve, budget), nil, nil, tc.opts)
			sys := findRole(out, "system")
			if sys == nil {
				t.Fatalf("no system message in output: %+v", out)
			}
			if tc.frozen {
				if sys.Content != wantSys {
					t.Fatalf("frozen system prompt must be byte-identical to its normal form; got truncated to %d chars, want %d", len(sys.Content), len(wantSys))
				}
				if res.AfterTokens <= budget {
					t.Fatalf("frozen request must honestly miss the budget: AfterTokens=%d budget=%d res=%+v", res.AfterTokens, budget, res)
				}
			} else {
				if sys.Content == wantSys {
					t.Fatalf("unfrozen system prompt must be truncated to fit, but came back intact")
				}
				if !strings.Contains(sys.Content, truncationMarker) {
					t.Fatalf("unfrozen system truncation must carry the marker, got %q", sys.Content)
				}
				if res.AfterTokens > budget {
					t.Fatalf("unfrozen request must fit the budget: AfterTokens=%d budget=%d res=%+v", res.AfterTokens, budget, res)
				}
			}
			if !res.LossyApplied {
				t.Fatalf("lossy must have applied, got %+v", res)
			}
		})
	}
}

// TestG22PlainApplyDefaultsAlways proves the same default
// through the plain Apply entry point (zero options): a bare
// lossy request freezes the system prompt.
func TestG22PlainApplyDefaultsAlways(t *testing.T) {
	msgs, _ := g22FreezeFixture()
	wantSys := findRole(Normalise(msgs), "system").Content
	budget := g22FreezeBudget(t, msgs)
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget}, nil, nil)
	sys := findRole(out, "system")
	if sys == nil {
		t.Fatalf("no system message in output: %+v", out)
	}
	if sys.Content != wantSys {
		t.Fatalf("plain Apply must freeze the system prompt by default; got truncated to %d chars", len(sys.Content))
	}
	if res.AfterTokens <= budget {
		t.Fatalf("frozen request must honestly miss the budget: AfterTokens=%d budget=%d", res.AfterTokens, budget)
	}
}

// TestG22CacheAwareDowngrade proves row :868: for a caching
// provider, aggressive/ultra are downgraded to standard — and
// the downgrade is live, not just a label: the downgraded run
// behaves as standard (Caveman runs, filler removed). The
// non-caching control keeps the requested mode.
func TestG22CacheAwareDowngrade(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello, could you please explain in detail how the caching system works in this project? I really appreciate it, thank you so much for your help with this problem."},
	}
	for _, mode := range []string{"aggressive", "ultra"} {
		out, res := ApplyWithOptions("gpt-4o", msgs, modeReq(t, mode, 10000), nil, nil, ApplyOptions{IsCachingProvider: true})
		if res.Mode != "standard" {
			t.Fatalf("mode %q with a caching provider must downgrade to standard, got %q", mode, res.Mode)
		}
		if !hasStep(res, StepCaveman) {
			t.Fatalf("downgraded %q must run Caveman as standard does, steps=%v", mode, res.Steps)
		}
		if strings.Contains(out[1].Content, "could you please") {
			t.Fatalf("downgraded %q must condense as standard does, got %q", mode, out[1].Content)
		}
		_, res = ApplyWithOptions("gpt-4o", msgs, modeReq(t, mode, 10000), nil, nil, ApplyOptions{})
		if res.Mode != mode {
			t.Fatalf("mode %q without a caching provider must stay %q, got %q", mode, mode, res.Mode)
		}
	}
	// Standard and RTK pass through unchanged for a caching provider.
	for _, mode := range []string{"standard", "rtk"} {
		_, res := ApplyWithOptions("gpt-4o", stackedFixture(), modeReq(t, mode, 10000), nil, nil, ApplyOptions{IsCachingProvider: true})
		if res.Mode != mode {
			t.Fatalf("mode %q with a caching provider must stay %q, got %q", mode, mode, res.Mode)
		}
	}
}

// TestG22ResolveCacheAwarePreserve pins the canonical resolvers
// at their new home (internal/compress/preserve.go — moved from
// degrade in G22 so Apply can call them without an import cycle;
// degrade keeps aliases, covered by its own tests).
func TestG22ResolveCacheAwarePreserve(t *testing.T) {
	cas := ResolveCacheAwareStrategy("aggressive", true)
	if cas.Strategy != "standard" || !cas.SkipSystemPrompt || !cas.DeterministicOnly {
		t.Fatalf("caching aggressive must downgrade to standard with both guards, got %+v", cas)
	}
	if got := ResolveCacheAwareStrategy("aggressive", false); got.Strategy != "aggressive" || got.SkipSystemPrompt {
		t.Fatalf("non-caching aggressive must pass through unguarded, got %+v", got)
	}
	cases := []struct {
		mode    SystemPromptMode
		caching bool
		control bool
		want    bool
	}{
		{SystemPromptPreserveAlways, false, false, true},
		{SystemPromptPreserveNever, true, true, false},
		{SystemPromptPreserveWhenNoCache, true, false, true},
		{SystemPromptPreserveWhenNoCache, false, true, true},
		{SystemPromptPreserveWhenNoCache, false, false, false},
		{"", false, false, true},
		{"bogus", false, false, true},
	}
	for _, tc := range cases {
		if got := ResolvePreserveSystemPrompt(tc.mode, tc.caching, tc.control); got != tc.want {
			t.Errorf("ResolvePreserveSystemPrompt(%q, caching=%v, control=%v) = %v, want %v", tc.mode, tc.caching, tc.control, got, tc.want)
		}
	}
}

// g22CapFixture is a prompt with an OLD tool result (before the
// last assistant message — cap-eligible) and a CURRENT-turn tool
// result (after the last assistant message — never capped). The
// old result's lines are DISTINCT: the cap path filters with
// SmartFilterText first (B4-38), which collapses runs of
// identical consecutive lines — 100 identical lines would
// collapse to one and the cap would never engage, so the fixture
// numbers the lines to keep the bulk the cap must cut.
func g22CapFixture() ([]provider.Message, string, string) {
	var oldSb strings.Builder
	oldSb.WriteString("HEAD-ANCHOR alpha beta gamma\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&oldSb, "line %03d of old tool output with some detail here\n", i)
	}
	oldSb.WriteString("TAIL-ANCHOR omega final line\n")
	oldTool := oldSb.String()
	curTool := strings.Repeat("current turn tool output line\n", 100)
	return []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Run the tests."},
		{Role: "tool", Content: oldTool, ToolCallID: "call-old"},
		{Role: "assistant", Content: "I will check the results."},
		{Role: "tool", Content: curTool, ToolCallID: "call-new"},
		{Role: "user", Content: "What did the tests say?"},
	}, oldTool, curTool
}

// g22ToolContents returns the old and current-turn tool contents
// of an Apply output, in order.
func g22ToolContents(out []provider.Message) (oldTool, curTool string) {
	var tools []string
	for _, m := range out {
		if m.Role == "tool" {
			tools = append(tools, m.Content)
		}
	}
	if len(tools) != 2 {
		return "", ""
	}
	return tools[0], tools[1]
}

// TestG22ToolResultCap proves row :848: the opt-in Lite cap
// truncates an old tool result (head + tail kept, marker
// present, within the cap), leaves the current turn's tool
// result untouched, and leaves everything untouched when the
// cap is not opted in. The cap alone (no Lossless field) opts
// the request into the lossless pass.
func TestG22ToolResultCap(t *testing.T) {
	msgs, oldTool, curTool := g22CapFixture()
	// Cap as the sole opt-in: no Lossless field at all.
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{ToolResultMaxChars: 300}, nil, nil)
	if !res.Applied {
		t.Fatalf("cap-only request must apply the lossless pass, got %+v", res)
	}
	if !hasStep(res, StepToolResultCap) {
		t.Fatalf("opted-in cap must record %q, steps=%v res=%+v", StepToolResultCap, res.Steps, res)
	}
	cappedOld, outCur := g22ToolContents(out)
	if got := len([]rune(cappedOld)); got > 300 {
		t.Fatalf("capped old tool result is %d runes, over the 300 cap", got)
	}
	if !strings.Contains(cappedOld, toolCapMarker) {
		t.Fatalf("capped result must carry the marker, got %q", cappedOld)
	}
	if !strings.Contains(cappedOld, "HEAD-ANCHOR") {
		t.Fatalf("capped multi-line result must keep its head, got %q", cappedOld)
	}
	if !strings.Contains(cappedOld, "TAIL-ANCHOR") {
		t.Fatalf("capped multi-line result must keep its tail, got %q", cappedOld)
	}
	if !strings.Contains(cappedOld, "lines elided") {
		t.Fatalf("capped multi-line result must name the elision, got %q", cappedOld)
	}
	// Current-turn tool result: untouched except the lossless
	// whitespace trim (trailing newline).
	if want := strings.TrimSuffix(curTool, "\n"); outCur != want {
		t.Fatalf("current-turn tool result must be untouched, got %d chars want %d", len(outCur), len(want))
	}
	// The capped output is the cap-aware normal form: the
	// equivalence check must have passed (else Apply returns
	// the originals with Applied=false).
	if !VerifyLosslessWithCap(msgs, out, 300) {
		t.Fatalf("capped output must verify against the cap-aware normal form")
	}
	if VerifyLossless(msgs, out) {
		t.Fatalf("capped output must NOT verify against the cap-0 normal form (else the check is blind to the cap)")
	}

	// No-cap control: the same fixture, lossless only.
	out, res = Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if hasStep(res, StepToolResultCap) {
		t.Fatalf("without a cap the cap step must not appear, steps=%v", res.Steps)
	}
	uncappedOld, _ := g22ToolContents(out)
	if want := strings.TrimSuffix(oldTool, "\n"); uncappedOld != want {
		t.Fatalf("without a cap the old tool result must be untouched, got %d chars want %d", len(uncappedOld), len(want))
	}

	// Cap via ApplyOptions (request field unset) also works.
	out, res = ApplyWithOptions("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil, ApplyOptions{ToolResultMaxChars: 300})
	if !hasStep(res, StepToolResultCap) {
		t.Fatalf("options cap must record %q, steps=%v", StepToolResultCap, res.Steps)
	}
	cappedOld, _ = g22ToolContents(out)
	if got := len([]rune(cappedOld)); got > 300 {
		t.Fatalf("options-capped old tool result is %d runes, over the 300 cap", got)
	}
}

// TestG22ToolResultCapShapes pins the cap's shapes: a short
// result is untouched, a single-line result gets a head-only
// cut, and an out-of-range cap clamps to the usable floor.
func TestG22ToolResultCapShapes(t *testing.T) {
	// Short result under the cap: untouched.
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Run it."},
		{Role: "tool", Content: strings.Repeat("short result ", 8), ToolCallID: "c1"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "Thanks."},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{ToolResultMaxChars: 300}, nil, nil)
	if hasStep(res, StepToolResultCap) {
		t.Fatalf("a short tool result must not be capped, steps=%v", res.Steps)
	}
	tool := findRole(out, "tool")
	if tool == nil || !strings.Contains(tool.Content, "short result") {
		t.Fatalf("short tool result must be intact, got %+v", out)
	}

	// Single-line long result: head-only cut, no elision line.
	msgs = []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Run it."},
		{Role: "tool", Content: strings.Repeat("word ", 300), ToolCallID: "c1"},
		{Role: "assistant", Content: "Done."},
		{Role: "user", Content: "Thanks."},
	}
	out, res = Apply("gpt-4o", msgs, &provider.CompressionRequest{ToolResultMaxChars: 300}, nil, nil)
	if !hasStep(res, StepToolResultCap) {
		t.Fatalf("a long single-line tool result must be capped, steps=%v", res.Steps)
	}
	tool = findRole(out, "tool")
	if tool == nil {
		t.Fatalf("no tool message in output: %+v", out)
	}
	if got := len([]rune(tool.Content)); got > 300 {
		t.Fatalf("single-line capped result is %d runes, over the 300 cap", got)
	}
	// Head-only cut carries the count hint (B4-38:
	// "...[truncated: N chars omitted]"), not the bare marker —
	// accept either marker form, but never an elision line.
	if !strings.Contains(tool.Content, "...[truncated") || strings.Contains(tool.Content, "lines elided") {
		t.Fatalf("single-line cap must be head-only with a marker, got %q", tool.Content)
	}

	// Out-of-range cap (100) clamps to the usable floor (256).
	out, _ = Apply("gpt-4o", msgs, &provider.CompressionRequest{ToolResultMaxChars: 100}, nil, nil)
	tool = findRole(out, "tool")
	if tool == nil {
		t.Fatalf("no tool message in output: %+v", out)
	}
	if got := len([]rune(tool.Content)); got > 256 {
		t.Fatalf("clamped cap must keep the result within 256 runes, got %d", got)
	}
}

// TestG22AutoTrigger proves row :866 (auto-trigger half): with
// AutoTrigger opted in, a tool-heavy prompt over 2x threshold
// selects rtk and the filter actually runs; without the option
// the same prompt stays bare-extractive (no mode, no RTK step).
// The explicit-mode control proves precedence: an explicit mode
// wins over the trigger.
func TestG22AutoTrigger(t *testing.T) {
	// 12 repeats ≈ 300 tool tokens: over 2x the 100-token
	// threshold (rtk) but under 8x (stacked) — the tool-heavy
	// rtk rule, not the stacked rule.
	toolOut := strings.Repeat("=== RUN TestAlpha\n--- PASS: TestAlpha (0.01s)\n=== RUN TestBeta\n--- PASS: TestBeta (0.01s)\n", 12)
	msgs := []provider.Message{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "tool", Content: toolOut, ToolCallID: "c1"},
		{Role: "user", Content: "What failed?"},
	}
	req := &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: 10000}
	opts := ApplyOptions{AutoTrigger: true, AutoTriggerConfig: AutoTriggerConfig{ThresholdTokens: 100, RTKToolShare: 0.5}}
	out, res := ApplyWithOptions("gpt-4o", msgs, req, nil, nil, opts)
	if res.Mode != "rtk" {
		t.Fatalf("auto-trigger on a tool-heavy prompt must select rtk, got %q res=%+v", res.Mode, res)
	}
	if !hasStep(res, StepRTK) {
		t.Fatalf("auto-triggered rtk must run the filter, steps=%v", res.Steps)
	}
	tool := findRole(out, "tool")
	if tool == nil || strings.Contains(tool.Content, "=== RUN") {
		t.Fatalf("auto-triggered rtk must strip routine lines, got %q", tool.Content)
	}

	// Control: the same prompt through plain Apply stays bare.
	_, res = Apply("gpt-4o", msgs, req, nil, nil)
	if res.Mode != "" {
		t.Fatalf("without the auto-trigger option the prompt must stay bare, got mode %q", res.Mode)
	}
	if hasStep(res, StepRTK) {
		t.Fatalf("without the auto-trigger option RTK must not run, steps=%v", res.Steps)
	}

	// Explicit mode wins over the trigger: a small prompt the
	// trigger would leave off still runs its named mode.
	small := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "tool", Content: toolOut, ToolCallID: "c1"},
		{Role: "user", Content: "What failed?"},
	}
	_, res = ApplyWithOptions("gpt-4o", small, modeReq(t, "rtk", 10000), nil, nil, opts)
	if res.Mode != "rtk" {
		t.Fatalf("explicit mode must win over the auto-trigger, got %q", res.Mode)
	}
}

// TestG22BudgetDial proves row :867 (dial half): with
// UseBudgetDial opted in and no explicit mode, the dial picks
// the cheapest mode whose estimate fits — lite here — and falls
// back to the strongest available mode (stacked) when nothing
// fits. Without the option the same prompt stays bare.
func TestG22BudgetDial(t *testing.T) {
	prose := strings.Repeat("The quick brown fox jumps over the lazy dog near the river bank. ", 80)
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: prose},
	}
	promptTokens := countTokens(t, msgs)
	budget := promptTokens - 50 // lite (0.902x) fits; off does not.
	req := &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: budget}
	_, res := ApplyWithOptions("gpt-4o", msgs, req, nil, nil, ApplyOptions{UseBudgetDial: true})
	if res.Mode != "lite" {
		t.Fatalf("budget dial must pick lite (cheapest fitting), got %q res=%+v", res.Mode, res)
	}
	// Control: plain Apply stays bare.
	_, res = Apply("gpt-4o", msgs, req, nil, nil)
	if res.Mode != "" {
		t.Fatalf("without the dial option the prompt must stay bare, got mode %q", res.Mode)
	}
	// Nothing fits: the dial falls back to the strongest
	// available mode and reports it honestly.
	req = &provider.CompressionRequest{Lossless: boolPtr(true), LossyMaxPromptTokens: 100}
	_, res = ApplyWithOptions("gpt-4o", msgs, req, nil, nil, ApplyOptions{UseBudgetDial: true})
	if res.Mode != "stacked" {
		t.Fatalf("budget dial with no fitting mode must fall back to stacked, got %q", res.Mode)
	}
}

// TestG22StackedStillLive re-pins row :865's cold verification:
// mode=stacked still runs the RTK → Caveman pipeline through
// Apply (RunStacked), with per-stage accounting. The deeper
// pipeline behaviour is covered by TestRunStacked* in
// pipeline_test.go; this test pins only that the Apply wire is
// live.
func TestG22StackedStillLive(t *testing.T) {
	out, res := Apply("gpt-4o", stackedFixture(), modeReq(t, "stacked", 10000), nil, nil)
	if res.Mode != "stacked" {
		t.Fatalf("res.Mode = %q, want stacked", res.Mode)
	}
	if !hasStep(res, StepRTK) || !hasStep(res, StepCaveman) {
		t.Fatalf("stacked must record both %q and %q, steps=%v", StepRTK, StepCaveman, res.Steps)
	}
	tool := findRole(out, "tool")
	if tool == nil || strings.Contains(tool.Content, "--- PASS: TestBeta") {
		t.Fatalf("stacked RTK stage must strip routine lines, got %+v", out)
	}
}

// TestG22DecideCapOptsIn pins the Decide change: a request
// carrying only ToolResultMaxChars opts into the lossless pass,
// while an explicit lossless:false still wins.
func TestG22DecideCapOptsIn(t *testing.T) {
	lossless, _, src := Decide(&provider.CompressionRequest{ToolResultMaxChars: 300}, "gpt-4o", nil)
	if !lossless || src != "request" {
		t.Fatalf("cap-only request must opt into lossless, got lossless=%v src=%q", lossless, src)
	}
	lossless, _, _ = Decide(&provider.CompressionRequest{Lossless: boolPtr(false), ToolResultMaxChars: 300}, "gpt-4o", nil)
	if lossless {
		t.Fatalf("explicit lossless:false must win over the cap opt-in")
	}
}

// TestG22EffectiveToolCap pins the clamp: out-of-range caps land
// in [256, 1,000,000]; 0 stays 0 (no cap).
func TestG22EffectiveToolCap(t *testing.T) {
	if got := effectiveToolCap(nil, ApplyOptions{}); got != 0 {
		t.Fatalf("zero options must mean no cap, got %d", got)
	}
	if got := effectiveToolCap(&provider.CompressionRequest{ToolResultMaxChars: 100}, ApplyOptions{}); got != 256 {
		t.Fatalf("cap 100 must clamp to 256, got %d", got)
	}
	if got := effectiveToolCap(&provider.CompressionRequest{ToolResultMaxChars: 2000000}, ApplyOptions{}); got != 1000000 {
		t.Fatalf("cap 2000000 must clamp to 1000000, got %d", got)
	}
	if got := effectiveToolCap(nil, ApplyOptions{ToolResultMaxChars: 500}); got != 500 {
		t.Fatalf("options cap 500 must pass through, got %d", got)
	}
	if got := effectiveToolCap(&provider.CompressionRequest{ToolResultMaxChars: 400}, ApplyOptions{ToolResultMaxChars: 500}); got != 400 {
		t.Fatalf("request cap must win over the option, got %d", got)
	}
}
