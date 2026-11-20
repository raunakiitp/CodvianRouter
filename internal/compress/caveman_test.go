package compress

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ruleFixtures drives both the per-rule firing tests and the toggle
// tests: input is condensed by the single rule (expected), and by
// the full level pipeline with the rule on (remnant must be gone)
// and off (remnant must survive).
var ruleFixtures = []struct {
	rule     string
	level    Level
	input    string
	expected string // single-rule output
	remnant  string // distinctive text that only this rule removes
}{
	{"excessive_gratitude", LevelLite,
		"Thank you so much for the detailed explanation today, it really helped me.",
		"for the detailed explanation today, it really helped me.", "so much"},
	{"pleasantries", LevelLite,
		"Thanks for the update today, the deployment finished on Friday morning.",
		"for the update today, the deployment finished on Friday morning.", "Thanks"},
	{"polite_framing", LevelLite,
		"Please explain how the migration works before Friday morning standup.",
		"explain how the migration works before Friday morning standup.", "Please"},
	{"hedging", LevelLite,
		"I think that the migration will probably finish before Friday morning standup.",
		"the migration will finish before Friday morning standup.", "probably"},
	{"filler_adverbs", LevelLite,
		"The migration basically finished, it actually works now after the restart today.",
		"The migration finished, it works now after the restart today.", "basically"},
	{"filler_phrases", LevelLite,
		"I want to deploy the new version on Friday morning after the tests pass.",
		"deploy the new version on Friday morning after the tests pass.", "I want to"},
	{"redundant_openers", LevelLite,
		"Hello, I need help with the database migration that failed on Friday morning.",
		"I need help with the database migration that failed on Friday morning.", "Hello"},
	{"verbose_requests", LevelLite,
		"I was wondering if you could explain the migration steps before Friday morning.",
		"explain the migration steps before Friday morning.", "I was wondering"},
	{"self_reference", LevelLite,
		"I am trying to deploy the new version but the migration keeps failing badly.",
		"deploy the new version but the migration keeps failing badly.", "I am trying to"},
	{"softeners", LevelLite,
		"Could you review the migration if possible before Friday morning standup meeting?",
		"Could you review the migration before Friday morning standup meeting?", "if possible"},
	{"uncertainty_fillers", LevelLite,
		"I guess the migration is more or less ready for Friday morning deployment.",
		"the migration is ready for Friday morning deployment.", "I guess"},
	{"qualifier_removal", LevelLite,
		"The migration is a bit slow and somewhat unreliable on Friday morning runs.",
		"The migration is slow and unreliable on Friday morning runs.", "a bit"},
	{"verbose_instructions", LevelLite,
		"Provide a detailed explanation of the migration before Friday morning standup.",
		"Provide the migration before Friday morning standup.", "detailed explanation of"},
	{"question_to_directive", LevelLite,
		"Can you explain why the migration failed on Friday morning during deployment?",
		"Explain why the migration failed on Friday morning during deployment?", "Can you explain why"},
	{"context_setup", LevelLite,
		"Here is my code: the migration script keeps failing on Friday morning runs.",
		"the migration script keeps failing on Friday morning runs.", "Here is my code"},
	{"intent_clarification", LevelLite,
		"What I'm trying to do is deploy the new version on Friday morning.",
		"deploy the new version on Friday morning.", "What I'm trying to do is"},
	{"background_removal", LevelLite,
		"As you may know, the migration failed on Friday morning during deployment.",
		"the migration failed on Friday morning during deployment.", "As you may know"},
	{"repeated_context", LevelLite,
		"As mentioned before, the migration failed on Friday morning during deployment.",
		"the migration failed on Friday morning during deployment.", "As mentioned before"},
	{"repeated_question", LevelLite,
		"This is the same question I asked about the migration failing Friday morning.",
		"I asked about the migration failing Friday morning.", "same question"},
	{"reestablished_context", LevelLite,
		"Returning to the migration that failed on Friday morning, what went wrong there?",
		"the migration that failed on Friday morning, what went wrong there?", "Returning to"},
	{"meta_commentary", LevelLite,
		"Note that the migration failed on Friday morning during the deployment run.",
		"the migration failed on Friday morning during the deployment run.", "Note that"},
	{"purpose_statement", LevelLite,
		"I wrote the script for the purpose of automating the Friday morning migration.",
		"I wrote the script for automating the Friday morning migration.", "for the purpose of"},
	{"purpose_phrases", LevelLite,
		"I wrote the script in order to automate the Friday morning migration run.",
		"I wrote the script to automate the Friday morning migration run.", "in order to"},
	{"verbose_connectors", LevelLite,
		"Furthermore, the migration failed on Friday morning during the deployment.",
		"the migration failed on Friday morning during the deployment.", "Furthermore"},
	{"transition_removal", LevelLite,
		"However, the migration failed on Friday morning during the deployment run.",
		"the migration failed on Friday morning during the deployment run.", "However"},
	{"emphasis_removal", LevelLite,
		"The migration was very slow and really unreliable on Friday morning runs.",
		"The migration was slow and unreliable on Friday morning runs.", "very slow"},
	{"explanatory_prefix", LevelLite,
		"The function appears to be handling the migration that failed Friday morning.",
		"Function: the migration that failed Friday morning.", "appears to be handling"},
	{"redundant_phrasing", LevelFull,
		"Make sure to restart the service before Friday morning standup meeting starts.",
		"restart the service before Friday morning standup meeting starts.", "Make sure to"},
	{"redundant_directive", LevelFull,
		"You should restart the service before Friday morning standup meeting starts.",
		"restart the service before Friday morning standup meeting starts.", "You should"},
	{"list_conjunction", LevelFull,
		"Check the logs, the metrics, and also the alerts before Friday morning standup.",
		"Check the logs, the metrics, the alerts before Friday morning standup.", "and also"},
	{"redundant_quantifiers", LevelFull,
		"Check each and every log file before the Friday morning deployment starts.",
		"Check each log file before the Friday morning deployment starts.", "each and every"},
	{"all_quantifier", LevelFull,
		"Check any and all log files before Friday morning deployment starts today.",
		"Check all log files before Friday morning deployment starts today.", "any and all"},
	{"passive_voice", LevelFull,
		"The report was created on Friday morning before the deployment started today.",
		"The report created on Friday morning before the deployment started today.", "was created"},
	{"compound_collapse", LevelFull,
		"Check the logs and any potential errors before Friday morning deployment.",
		"Check the logs and errors before Friday morning deployment.", "any potential"},
	{"articles", LevelFull,
		"Check the log file for a missing entry before the Friday morning run.",
		"Check log file for missing entry before the Friday morning run.", "the log file"},
	{"leader_phrases", LevelFull,
		"I will deploy the new version on Friday morning after the tests pass.",
		"deploy the new version on Friday morning after the tests pass.", "I will deploy"},
	{"repeated_sentence_dedupe", LevelLite,
		"The deployment failed on Friday morning. The deployment failed on Friday morning. We need to investigate the logs today.",
		"The deployment failed on Friday morning. We need to investigate the logs today.", "The deployment failed on Friday morning. The deployment failed"},
	{"whitespace_normalisation", LevelLite,
		"Check   the logs  ,  and the metrics\t\tbefore Friday morning.",
		"Check the logs, and the metrics before Friday morning.", "   "},
}

// TestCavemanRuleFires: each rule, run alone, rewrites its fixture
// to exactly the expected output.
func TestCavemanRuleFires(t *testing.T) {
	for _, fx := range ruleFixtures {
		t.Run(fx.rule, func(t *testing.T) {
			rule, ok := ruleByName(fx.rule)
			if !ok {
				t.Fatalf("rule %q not registered", fx.rule)
			}
			if rule.Level != fx.level {
				t.Fatalf("rule %q level %q, want %q", fx.rule, rule.Level, fx.level)
			}
			got, applied := condenseTextWithRules(fx.input, []Rule{rule})
			if got != fx.expected {
				t.Fatalf("rule %q alone:\n got: %q\nwant: %q", fx.rule, got, fx.expected)
			}
			if len(applied) != 1 || applied[0] != fx.rule {
				t.Fatalf("rule %q alone: applied %v, want [%q]", fx.rule, applied, fx.rule)
			}
		})
	}
}

// TestCavemanRuleToggle: each rule test must FAIL with the rule
// disabled and PASS with it enabled, inside the full level
// pipeline. "Fail" here means the rule's distinctive remnant
// survives; "pass" means it is gone and the text got shorter.
func TestCavemanRuleToggle(t *testing.T) {
	for _, fx := range ruleFixtures {
		t.Run(fx.rule, func(t *testing.T) {
			with, _ := condenseTextWithRules(fx.input, rulesForLevel(fx.level))
			if strings.Contains(with, fx.remnant) {
				t.Fatalf("rule %q enabled: remnant %q still present in %q", fx.rule, fx.remnant, with)
			}
			if len(with) >= len(fx.input) {
				t.Fatalf("rule %q enabled: output not shorter: %q", fx.rule, with)
			}
			without, _ := condenseTextWithRules(fx.input, rulesForLevelExcept(fx.level, fx.rule))
			if fx.rule == "repeated_sentence_dedupe" {
				if n := strings.Count(without, "The deployment failed on Friday morning."); n != 2 {
					t.Fatalf("rule disabled: duplicated sentence occurs %d times, want 2 in %q", n, without)
				}
				return
			}
			if !strings.Contains(without, fx.remnant) {
				t.Fatalf("rule %q disabled: remnant %q gone in %q (another rule removed it?)", fx.rule, fx.remnant, without)
			}
		})
	}
}

// TestCavemanRuleInventory pins the rule count and names: 29 lite
// + 9 full = 38 rules. A rule added or dropped without updating
// this test (and the report) fails loudly.
func TestCavemanRuleInventory(t *testing.T) {
	lite := rulesForLevel(LevelLite)
	full := rulesForLevel(LevelFull)
	if len(lite) != 29 {
		names := make([]string, len(lite))
		for i, r := range lite {
			names[i] = r.Name
		}
		t.Fatalf("lite rules = %d, want 29: %v", len(lite), names)
	}
	if len(full) != 38 {
		t.Fatalf("full rules = %d, want 38", len(full))
	}
	if len(ruleFixtures) != 38 {
		t.Fatalf("rule fixtures = %d, want 38 (one per rule)", len(ruleFixtures))
	}
}

// TestCavemanMinLengthBoundary: 49 runes passes through unchanged
// even though it contains "Please"; 50 and 51 runes condense.
func TestCavemanMinLengthBoundary(t *testing.T) {
	full := "Please " + strings.Repeat("word ", 9) // 7 + 45 = 52 runes
	if len(full) != 52 {
		t.Fatalf("test setup: len %d, want 52", len(full))
	}
	if got, _ := CondenseText(full[:49], LevelLite); got != full[:49] {
		t.Fatalf("49 runes: got %q, want unchanged", got)
	}
	for _, n := range []int{50, 51} {
		got, applied := CondenseText(full[:n], LevelLite)
		if strings.Contains(got, "Please") {
			t.Fatalf("%d runes: 'Please' survived in %q", n, got)
		}
		if len(got) >= n {
			t.Fatalf("%d runes: output not shorter: %q", n, got)
		}
		if len(applied) == 0 || applied[0] != "polite_framing" {
			t.Fatalf("%d runes: applied %v, want polite_framing first", n, applied)
		}
	}
	if got, applied := CondenseText("Please help me.", LevelLite); got != "Please help me." || len(applied) != 0 {
		t.Fatalf("short input: got %q %v, want unchanged", got, applied)
	}
}

// TestCavemanLevelGating: full-only rules must not fire at lite.
func TestCavemanLevelGating(t *testing.T) {
	cases := []struct{ name, input, liteWant, fullWant string }{
		{"articles",
			"Check the log file for a missing entry before the Friday morning run.",
			"Check the log file for a missing entry before the Friday morning run.",
			"Check log file for missing entry before the Friday morning run."},
		{"leader_phrases",
			"I will deploy the new version on Friday morning after the tests pass.",
			"I will deploy the new version on Friday morning after the tests pass.",
			// Full also runs articles: "the" before "new"/"tests" goes too.
			"deploy new version on Friday morning after tests pass."},
		{"redundant_phrasing",
			"Make sure to restart the service before Friday morning standup meeting.",
			"Make sure to restart the service before Friday morning standup meeting.",
			// Full also runs articles: "the" before "service" goes too.
			"restart service before Friday morning standup meeting."},
		{"passive_voice",
			"The report was created on Friday morning before the deployment started.",
			"The report was created on Friday morning before the deployment started.",
			"The report created on Friday morning before deployment started."},
	}
	for _, c := range cases {
		if got, _ := CondenseText(c.input, LevelLite); got != c.liteWant {
			t.Fatalf("%s lite: got %q, want %q", c.name, got, c.liteWant)
		}
		if got, _ := CondenseText(c.input, LevelFull); got != c.fullWant {
			t.Fatalf("%s full: got %q, want %q", c.name, got, c.fullWant)
		}
	}
	// An unknown level behaves as lite, never as full.
	if got, _ := CondenseText(cases[0].input, Level("bogus")); got != cases[0].liteWant {
		t.Fatalf("unknown level: got %q, want lite behaviour %q", got, cases[0].liteWant)
	}
}

// TestCavemanProtection: code fences, URLs, JSON (strict and
// single-quoted) and inline code survive byte-identical, while the
// prose around them is condensed — and CheckLossy agrees.
func TestCavemanProtection(t *testing.T) {
	fence := "```go\nfunc main() {\n\tfmt.Println(\"thank you so much\")\n}\n```"
	cases := []struct {
		name     string
		input    string
		mustKeep []string // substrings that must survive byte-identical
		mustDrop []string // filler that must be gone
	}{
		{"code_fence",
			"Please explain the code below, it is basically broken today.\n" + fence + "\nThe function is very slow and really unreliable.",
			[]string{fence, "thank you so much"},
			[]string{"Please", "basically", "very slow", "really unreliable"}},
		{"url",
			"Please check https://example.com/docs/migration-guide for details, it is basically the main reference today.",
			[]string{"https://example.com/docs/migration-guide"},
			[]string{"Please", "basically"}},
		{"json",
			`Please review this config, it is basically the main one today: {"name": "thank you", "note": "please help me", "count": 42}`,
			[]string{`{"name": "thank you", "note": "please help me", "count": 42}`},
			[]string{"Please review", "basically"}},
		{"json_single_quoted",
			`Please review this config, it is basically the main one today: {'note': 'thank you so much', 'count': 42}`,
			[]string{`{'note': 'thank you so much', 'count': 42}`},
			[]string{"Please review", "basically"}},
		{"inline_code",
			"Please run `thank you so much` in the terminal, it is basically broken today now.",
			[]string{"`thank you so much`"},
			[]string{"Please", "basically"}},
		{"unclosed_fence",
			"Please explain the code below, it is basically broken today.\n```go\nfunc main() {\n\tfmt.Println(\"thank you so much\")\n}",
			[]string{"```go\nfunc main() {\n\tfmt.Println(\"thank you so much\")\n}"},
			[]string{"Please", "basically"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs := []provider.Message{{Role: "user", Content: c.input}}
			res := CondenseMessages(msgs, LevelFull)
			got := res.Messages[0].Content
			for _, keep := range c.mustKeep {
				if !strings.Contains(got, keep) {
					t.Fatalf("protected segment lost:\n got: %q\nwant it to contain: %q", got, keep)
				}
			}
			for _, drop := range c.mustDrop {
				if strings.Contains(got, drop) {
					t.Fatalf("filler %q survived in %q", drop, got)
				}
			}
			if ok, reasons := CheckLossy(msgs, res.Messages); !ok {
				t.Fatalf("CheckLossy refused the condensed output: %v\n got: %q", reasons, got)
			}
		})
	}
}

// TestCavemanRolesAndSkips: only user-role plain-text messages are
// condensed; everything else passes through byte-identical.
func TestCavemanRolesAndSkips(t *testing.T) {
	filler := "Please explain the migration, it is basically broken today now."
	msgs := []provider.Message{
		{Role: "system", Content: filler},
		{Role: "developer", Content: filler},
		{Role: "assistant", Content: filler},
		{Role: "tool", Content: filler, ToolCallID: "call_1"},
		{Role: "user", Content: filler},
		{Role: "user", Content: filler, Parts: []provider.ContentPart{{Type: "text", Text: filler}}},
		{Role: "assistant", Content: filler, ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function"}}},
	}
	orig := append([]provider.Message(nil), msgs...)
	res := CondenseMessages(msgs, LevelFull)
	for i, m := range res.Messages {
		if i == 4 {
			if strings.Contains(m.Content, "Please") || strings.Contains(m.Content, "basically") {
				t.Fatalf("user message not condensed: %q", m.Content)
			}
			continue
		}
		if m.Content != orig[i].Content {
			t.Fatalf("message %d (role %q) changed: %q, want unchanged", i, m.Role, m.Content)
		}
	}
	if res.Changed != 1 {
		t.Fatalf("Changed = %d, want 1", res.Changed)
	}
	// The input slice itself must not be mutated.
	if msgs[4].Content != filler {
		t.Fatalf("input slice mutated: %q", msgs[4].Content)
	}
	// Opting assistant in via Options condenses it too.
	res2 := CondenseMessagesWithOptions(
		[]provider.Message{{Role: "assistant", Content: filler}},
		LevelFull, Options{Roles: []string{"user", "assistant"}})
	if strings.Contains(res2.Messages[0].Content, "Please") {
		t.Fatalf("assistant with opted-in role not condensed: %q", res2.Messages[0].Content)
	}
}

// TestCavemanNoNewWordsNoNumberChanges: the condensed text must
// introduce no word and must preserve every number/date/identifier
// exactly (same digit runs, same counts).
func TestCavemanNoNewWordsNoNumberChanges(t *testing.T) {
	inputs := []string{
		"Please explain why version 2.4.1 failed on 2026-11-19, the cost was $1,250.50 today.",
		"I want to deploy build 4096 on Friday 2026-11-20 at 14:30, please confirm quickly.",
		"The API returned 429 errors and 500 errors, basically 99.9% of requests failed today.",
		"Can you tell me why user_42 with account id ACC-9182 saw error E_CONN_RESET on 2026-11-19?",
	}
	for _, in := range inputs {
		out, _ := CondenseText(in, LevelFull)
		if !wordsSubset(in, out) {
			t.Fatalf("new word introduced:\n in: %q\nout: %q", in, out)
		}
		if !numbersEqual(in, out) {
			t.Fatalf("numbers changed:\n in: %q\nout: %q", in, out)
		}
		if len(out) >= len(in) {
			t.Fatalf("no saving:\n in: %q\nout: %q", in, out)
		}
	}
	// The checks themselves must catch a violation (not vacuous).
	if wordsSubset("hello world", "hello brave world") {
		t.Fatal("wordsSubset missed an introduced word")
	}
	if numbersEqual("cost 42 today", "cost 43 today") {
		t.Fatal("numbersEqual missed a changed number")
	}
}

// TestCavemanIdempotence: condensing twice is the same as once.
func TestCavemanIdempotence(t *testing.T) {
	var corpus []string
	for _, fx := range ruleFixtures {
		corpus = append(corpus, fx.input)
	}
	corpus = append(corpus,
		"Please check https://example.com/docs/migration-guide for details, it is basically the main reference today.",
		"Please explain the code below, it is basically broken today.\n```go\nfunc main() {\n\tfmt.Println(\"thank you so much\")\n}\n```\nThe function is very slow.",
		`Please review this config: {"name": "thank you", "count": 42} and basically tell me more today.`,
		"Hello, I want to deploy the new version on Friday morning. I want to deploy the new version on Friday morning.",
	)
	for _, in := range corpus {
		once, _ := CondenseText(in, LevelFull)
		twice, _ := CondenseText(once, LevelFull)
		if once != twice {
			t.Fatalf("not idempotent:\n in:   %q\nonce: %q\ntwice: %q", in, once, twice)
		}
		msgs := []provider.Message{{Role: "user", Content: in}}
		r1 := CondenseMessages(msgs, LevelFull)
		r2 := CondenseMessages(r1.Messages, LevelFull)
		if r1.Messages[0].Content != r2.Messages[0].Content {
			t.Fatalf("CondenseMessages not idempotent:\n in:   %q\nonce: %q\ntwice: %q", in, r1.Messages[0].Content, r2.Messages[0].Content)
		}
	}
}

// TestCavemanEmptyGuard: a message that is nothing but filler
// condenses to nothing, so the engine reverts to the original
// instead of shipping an empty user message.
func TestCavemanEmptyGuard(t *testing.T) {
	in := "Thank you so much, I really appreciate it, thanks in advance!"
	if len(in) < MinMessageLength {
		t.Fatalf("test setup: len %d < 50", len(in))
	}
	res := CondenseMessages([]provider.Message{{Role: "user", Content: in}}, LevelFull)
	if res.Messages[0].Content != in {
		t.Fatalf("filler-only message not reverted: %q", res.Messages[0].Content)
	}
	if res.Changed != 0 {
		t.Fatalf("Changed = %d, want 0", res.Changed)
	}
}

// TestCavemanMixedConversation: a realistic multi-turn input
// condenses only the user prose, keeps order/roles, and passes
// CheckLossy end to end.
func TestCavemanMixedConversation(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are a helpful assistant. Please be concise today."},
		{Role: "user", Content: "Hello, I want to deploy the new version but it is basically broken today, please help me out here."},
		{Role: "assistant", Content: "Sure, I would be happy to help you with the deployment today."},
		{Role: "user", Content: "Please check https://example.com/status page, the migration failed on Friday morning. The migration failed on Friday morning."},
	}
	res := CondenseMessages(msgs, LevelFull)
	if res.Changed != 2 {
		t.Fatalf("Changed = %d, want 2", res.Changed)
	}
	for i, m := range res.Messages {
		if m.Role != msgs[i].Role {
			t.Fatalf("message %d role changed: %q", i, m.Role)
		}
	}
	if !strings.Contains(res.Messages[3].Content, "https://example.com/status") {
		t.Fatalf("URL lost: %q", res.Messages[3].Content)
	}
	if n := strings.Count(res.Messages[3].Content, "The migration failed on Friday morning."); n != 1 {
		t.Fatalf("duplicated sentence occurs %d times, want 1: %q", n, res.Messages[3].Content)
	}
	if ok, reasons := CheckLossy(msgs, res.Messages); !ok {
		t.Fatalf("CheckLossy refused: %v", reasons)
	}
}

// TestCavemanAppliedRules: the result names the rules that fired,
// in first-seen order, with no duplicates.
func TestCavemanAppliedRules(t *testing.T) {
	res := CondenseMessages([]provider.Message{{
		Role:    "user",
		Content: "Hello, please explain basically why the migration failed on Friday morning today.",
	}}, LevelLite)
	// AppliedRules is in rule-application (list) order, not text order:
	// polite_framing and filler_adverbs precede redundant_openers
	// in the inventory even though "Hello," comes first in the text.
	want := []string{"polite_framing", "filler_adverbs", "redundant_openers"}
	if len(res.AppliedRules) != len(want) {
		t.Fatalf("AppliedRules = %v, want %v", res.AppliedRules, want)
	}
	for i, w := range want {
		if res.AppliedRules[i] != w {
			t.Fatalf("AppliedRules = %v, want %v", res.AppliedRules, want)
		}
	}
	seen := map[string]bool{}
	for _, n := range res.AppliedRules {
		if seen[n] {
			t.Fatalf("duplicate rule %q in %v", n, res.AppliedRules)
		}
		seen[n] = true
	}
}

func TestCavemanCountRegexTokens(t *testing.T) {
	// Sorted digit-run comparison helper sanity: order must not matter.
	a := countRegexTokens(numberRe, "v2.4.1 on 2026-11-19")
	b := countRegexTokens(numberRe, "2026-11-19 v2.4.1")
	if len(a) != len(b) {
		t.Fatalf("countRegexTokens mismatch: %v vs %v", a, b)
	}
	for k, n := range a {
		if b[k] != n {
			t.Fatalf("countRegexTokens[%q] = %d vs %d", k, n, b[k])
		}
	}
}

// TestCavemanDedupeSkipsNumberedSentences: a repeated sentence that
// contains a digit run is never deduped — dropping one copy would
// change the digit-run multiset and trip the numbersEqual self-check,
// reverting the WHOLE message (the re-review defect). The numbered
// sentence survives twice and the other filler is still condensed.
func TestCavemanDedupeSkipsNumberedSentences(t *testing.T) {
	in := "The build 4096 failed on Friday morning. The build 4096 failed on Friday morning. We need to investigate today, please help us basically now."
	out, _ := CondenseText(in, LevelFull)
	if n := strings.Count(out, "The build 4096 failed on Friday morning."); n != 2 {
		t.Fatalf("numbered sentence occurs %d times, want 2 (both copies kept):\n out: %q", n, out)
	}
	if strings.Contains(out, "please") || strings.Contains(out, "basically") {
		t.Fatalf("other filler survived:\n out: %q", out)
	}
	if ok, reasons := CheckLossy(
		[]provider.Message{{Role: "user", Content: in}},
		[]provider.Message{{Role: "user", Content: out}}); !ok {
		t.Fatalf("CheckLossy refused the condensed output: %v\n out: %q", reasons, out)
	}
}

// TestCavemanLoneBacktickMisPair: a lone backtick pairs with the next
// opener, over-protecting the text between them (safe direction —
// the content survives, only savings are lost). Pinned by re-review.
func TestCavemanLoneBacktickMisPair(t *testing.T) {
	in := "run `ls` now and check ` the basically broken thing `ok` please help."
	out, _ := CondenseText(in, LevelFull)
	if !strings.Contains(out, "the basically broken thing") {
		t.Fatalf("mis-paired span content lost:\n out: %q", out)
	}
	if strings.Contains(out, "please help") {
		t.Fatalf("filler outside the mis-paired span survived:\n out: %q", out)
	}
}

// TestCavemanBraceHeavyProseOverProtection: lenient JSON detection
// can over-protect brace-heavy prose (safe direction — filler inside
// the braces survives, only savings are lost). Pinned by re-review.
func TestCavemanBraceHeavyProseOverProtection(t *testing.T) {
	in := "Please explain if (x) { y: basically broken } today, it is basically the main issue now."
	out, _ := CondenseText(in, LevelFull)
	if !strings.Contains(out, "{ y: basically broken }") {
		t.Fatalf("brace span not preserved:\n out: %q", out)
	}
	if n := strings.Count(out, "basically"); n != 1 {
		t.Fatalf("basically occurs %d times, want 1 (only the protected inner one):\n out: %q", n, out)
	}
	if strings.Contains(out, "Please") {
		t.Fatalf("filler outside the brace span survived:\n out: %q", out)
	}
}
