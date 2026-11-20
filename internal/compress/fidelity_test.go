package compress

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// M7.2 fidelity gate tests: the round-trip half of the M7 exit
// gate. Every test proves the same contract two ways where it
// matters: CheckLossy on the lossy engine's real output
// (applyLossy), and Apply end-to-end (gate wired in, refusal
// returns the original byte-identical). Budgets below were
// calibrated against the real token counts (see the probe runs
// in the session log); a budget that no longer cuts the intended
// structure fails loudly instead of passing vacuously, because
// each refusal test also asserts the un-gated output really is
// damaged (CheckLossy false with a reason naming the structure).

const (
	fidelityLongURL     = "https://example.com/api/v1/users/1234567890abcdef/profile?tab=settings&view=detail&x=" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	fidelityGoFence     = "```go\npackage main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello world\")\n\tfor i := 0; i < 10; i++ {\n\t\tfmt.Println(i * i)\n\t}\n}\n```"
	fidelityPyFence     = "```python\ndef fib(n):\n    if n < 2:\n        return n\n    return fib(n-1) + fib(n-2)\n\nprint(fib(10))\n```"
	fidelityNestedJSON  = `{"user": {"id": 42, "name": "alice", "tags": ["a", "b", "c"]}, "items": [{"sku": "X1", "price": 9.99}, {"sku": "X2", "price": 19.99}], "meta": {"page": 1, "total": 2}}`
	fidelityTabularJSON = `[{"name": "alice", "age": 30, "city": "pune"}, {"name": "bob", "age": 25, "city": "mumbai"}, {"name": "carol", "age": 41, "city": "delhi"}, {"name": "dave", "age": 33, "city": "jaipur"}]`
	fidelityJSONWithURL = `{"docs": "https://docs.example.com/guides/getting-started/installation/linux/debian/bookworm", "homepage": "https://example.com/"}`
)

func fidelityProse(n int) string {
	return strings.Repeat("lorem ipsum dolor sit amet consectetur adipiscing elit ", n)
}

func fidelityLossyReq(budget int) *provider.CompressionRequest {
	return &provider.CompressionRequest{LossyMaxPromptTokens: budget}
}

func fidelitySystemUser(content string) []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: content},
	}
}

// assertGateRefusal checks the Apply-level refusal contract:
// the original messages come back byte-identical, nothing is
// reported applied, no tokens are claimed saved, and the only
// step recorded is the fidelity-gate refusal.
func assertGateRefusal(t *testing.T, orig, out []provider.Message, res *provider.CompressionResult) {
	t.Helper()
	if !reflect.DeepEqual(orig, out) {
		t.Fatalf("gate refusal must return the original messages byte-identical, got %d messages vs %d", len(out), len(orig))
	}
	if res.Applied || res.LossyApplied || res.Lossless {
		t.Fatalf("gate refusal must report nothing applied, got %+v", res)
	}
	if res.SavedTokens != 0 {
		t.Fatalf("gate refusal must save 0 tokens, got %+v", res)
	}
	if res.BeforeTokens <= 0 || res.AfterTokens != res.BeforeTokens {
		t.Fatalf("gate refusal must report after==before>0, got %+v", res)
	}
	if len(res.Steps) != 1 || res.Steps[0] != StepFidelityGateRefused {
		t.Fatalf("gate refusal must record exactly [%q], got %v", StepFidelityGateRefused, res.Steps)
	}
}

// assertReasonsContain fails unless at least one reason mentions
// substr (case-insensitive).
func assertReasonsContain(t *testing.T, reasons []string, substr string) {
	t.Helper()
	for _, r := range reasons {
		if strings.Contains(strings.ToLower(r), strings.ToLower(substr)) {
			return
		}
	}
	t.Fatalf("expected a reason mentioning %q, got %v", substr, reasons)
}

// assertGateRefusesRealDamage runs the lossy engine on the
// normalised messages and proves the un-gated output really is
// damaged (gate false, reason names want), so a refusal test
// cannot pass vacuously by never cutting anything.
func assertGateRefusesRealDamage(t *testing.T, msgs []provider.Message, budget int, want string) {
	t.Helper()
	norm := Normalise(msgs)
	trunc, dropped, truncated := applyLossy("gpt-4o", norm, budget)
	if dropped == 0 && truncated == 0 {
		t.Fatalf("budget %d changed nothing: test would pass vacuously", budget)
	}
	ok, reasons := CheckLossy(norm, trunc)
	if ok {
		t.Fatalf("gate must refuse the un-gated lossy output (budget %d), got ok", budget)
	}
	assertReasonsContain(t, reasons, want)
}

func TestCheckLossyIdentityAndEmpty(t *testing.T) {
	msgs := fidelitySystemUser("hello")
	if ok, reasons := CheckLossy(msgs, msgs); !ok {
		t.Fatalf("identical messages must pass, got %v", reasons)
	}
	if ok, _ := CheckLossy(nil, nil); !ok {
		t.Fatal("empty vs empty must pass")
	}
	if ok, _ := CheckLossy(nil, msgs); ok {
		t.Fatal("fabricated messages from nothing must fail")
	}
	if ok, _ := CheckLossy(msgs, nil); ok {
		t.Fatal("dropping every message must fail")
	}
}

func TestCheckLossyCleanProseTruncationPasses(t *testing.T) {
	// Prose-only: there is no structure to damage, so the gate
	// must let the lossy truncation through.
	msgs := fidelitySystemUser(fidelityProse(200))
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(50), nil, nil)
	if !res.LossyApplied {
		t.Fatalf("prose truncation must apply, got %+v", res)
	}
	if contains(res.Steps, StepFidelityGateRefused) {
		t.Fatalf("clean prose truncation must not trip the gate, got %+v", res)
	}
	if !strings.Contains(out[len(out)-1].Content, truncationMarker) {
		t.Fatalf("truncated message must carry the marker, got %q", out[len(out)-1].Content)
	}
}

func TestFidelityGateRefusesTruncatedGoFence(t *testing.T) {
	msgs := fidelitySystemUser("Intro " + fidelityProse(4)[:200] + "\n" + fidelityGoFence + "\nOutro " + fidelityProse(4)[:200])
	assertGateRefusesRealDamage(t, msgs, 80, "code fence")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(80), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedPythonFence(t *testing.T) {
	msgs := fidelitySystemUser("Intro " + fidelityProse(4)[:200] + "\n" + fidelityPyFence + "\nOutro " + fidelityProse(4)[:200])
	assertGateRefusesRealDamage(t, msgs, 80, "code fence")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(80), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedNestedJSON(t *testing.T) {
	msgs := fidelitySystemUser("Here is the payload:\n" + fidelityNestedJSON + "\nPlease process it.\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 60, "JSON value")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(60), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedTabularJSON(t *testing.T) {
	msgs := fidelitySystemUser("Rows:\n" + fidelityTabularJSON + "\nEnd.\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 60, "JSON value")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(60), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedURL(t *testing.T) {
	msgs := fidelitySystemUser("See the docs at " + fidelityLongURL + " for details.\n" + fidelityProse(30) + "\n" + fidelityProse(30))
	// The un-gated output splices the URL head onto the tail of
	// the prose (a novel URL that never existed) as well as
	// cutting it: both reasons must fire.
	assertGateRefusesRealDamage(t, msgs, 60, "spliced URL")
	assertGateRefusesRealDamage(t, msgs, 60, "URL cut")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(60), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesJSONStringContainingURL(t *testing.T) {
	msgs := fidelitySystemUser("Config:\n" + fidelityJSONWithURL + "\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 40, "JSON value")
	assertGateRefusesRealDamage(t, msgs, 40, "URL")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(40), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesFenceContainingJSON(t *testing.T) {
	msgs := fidelitySystemUser("Payload:\n```json\n" + fidelityNestedJSON + "\n```\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 60, "code fence")
	assertGateRefusesRealDamage(t, msgs, 60, "JSON value")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(60), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedURLInCodeFence(t *testing.T) {
	// Hand-built cut: a URL inside a code fence, truncated
	// mid-string by the same head+tail cut the engine makes.
	// The fence is ~2,000 runes: ~500 runes of code, then the
	// URL, then ~1,300 runes of code. truncateMiddle(., 0.5)
	// keeps the first 30% and the last 20%, so the cut falls
	// inside the URL (its prefix survives in the head) and
	// inside the fence (opener and closer survive, middle code
	// does not).
	headCode := strings.Repeat("fmt.Println(\"x\")\n", 28) // 504 runes
	tailCode := strings.Repeat("fmt.Println(\"y\")\n", 72) // 1,296 runes
	fence := "```go\n" + headCode + "// see " + fidelityLongURL + "\n" + tailCode + "```"
	orig := []provider.Message{{Role: "user", Content: fence}}
	cut := truncateMiddle(fence, 0.5)
	if strings.Contains(cut, fidelityLongURL) {
		t.Fatal("test setup: the hand-built cut must not leave the URL intact")
	}
	if !strings.Contains(cut, "https://example.") {
		t.Fatal("test setup: the hand-built cut must leave a URL fragment behind")
	}
	if ok, reasons := CheckLossy(orig, []provider.Message{{Role: "user", Content: cut}}); ok {
		t.Fatalf("cut URL inside a fence must fail, got ok (cut=%q)", cut)
	} else {
		assertReasonsContain(t, reasons, "URL")
		assertReasonsContain(t, reasons, "code fence")
	}
}

func TestCheckLossyURLSplitAcrossTruncationBoundary(t *testing.T) {
	// Adversarial sweep: the same long URL placed at several
	// offsets across the truncation cut point. Wherever the cut
	// actually slices the URL, the gate must refuse; the test
	// fails if the sweep never slices it at all (vacuous).
	cuts := 0
	for _, frac := range []float64{0.15, 0.25, 0.35, 0.5, 0.65, 0.75} {
		total := 2000
		urlStart := int(float64(total) * frac)
		var sb strings.Builder
		for sb.Len() < urlStart {
			sb.WriteString("word ")
		}
		sb.WriteString(fidelityLongURL)
		for sb.Len() < total {
			sb.WriteString(" word")
		}
		content := sb.String()
		cut := truncateMiddle(content, 0.5)
		if strings.Contains(cut, fidelityLongURL) {
			continue // URL survived whole (in head or tail): gate must pass, nothing to prove here.
		}
		// Did the cut leave a fragment of the URL behind?
		fragment := false
		for _, w := range urlFragments(fidelityLongURL) {
			if w != "" && strings.Contains(cut, w) {
				fragment = true
				break
			}
		}
		if !fragment {
			continue // URL dropped whole with the middle: allowed.
		}
		cuts++
		orig := []provider.Message{{Role: "user", Content: content}}
		comp := []provider.Message{{Role: "user", Content: cut}}
		if ok, reasons := CheckLossy(orig, comp); ok {
			t.Fatalf("URL split across the cut (start frac %.2f) must fail, got ok", frac)
		} else {
			assertReasonsContain(t, reasons, "URL")
		}
	}
	if cuts < 2 {
		t.Fatalf("sweep cut the URL only %d time(s): test is vacuous", cuts)
	}
}

func TestCheckLossySplicedURLRefused(t *testing.T) {
	// Head of one URL joined to the tail of another is a novel
	// URL that never existed: the gate must refuse it even
	// though each half is "intact" text.
	a := "https://alpha.example.com/first/path/segment/one"
	b := "https://beta.example.com/second/path/segment/two"
	orig := []provider.Message{{Role: "user", Content: "See " + a + " and " + b + " for details."}}
	spliced := a[:30] + b[30:]
	comp := []provider.Message{{Role: "user", Content: "See " + spliced + " for details."}}
	if ok, reasons := CheckLossy(orig, comp); ok {
		t.Fatalf("spliced URL %q must fail, got ok", spliced)
	} else {
		assertReasonsContain(t, reasons, "spliced URL")
	}
}

func TestFidelityGateOddFences(t *testing.T) {
	// An odd number of fences: the original itself ends inside
	// a fence. Any truncation of that message damages the
	// unclosed tail and must be refused...
	msgs := fidelitySystemUser("Broken fence:\n```go\npackage main\nfunc main() {}\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 80, "fence")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(80), nil, nil)
	assertGateRefusal(t, msgs, out, res)

	// ...but dropping the message whole is allowed, and an
	// unclosed fence that survives verbatim (no truncation of
	// that message) passes.
	dropped := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Broken fence:\n```go\npackage main\nfunc main() {}\n"},
		{Role: "user", Content: "What does this do?"},
	}
	kept := []provider.Message{dropped[0], dropped[2]}
	if ok, reasons := CheckLossy(dropped, kept); !ok {
		t.Fatalf("dropping an unclosed-fence message whole must pass, got %v", reasons)
	}
	if ok, reasons := CheckLossy(dropped, dropped); !ok {
		t.Fatalf("an unclosed fence surviving verbatim must pass, got %v", reasons)
	}
}

func TestFidelityGatePassesWhenStructureIntact(t *testing.T) {
	// Structure up front, long prose after: the cut lands in
	// prose only, so the URL, fence and JSON all survive intact
	// and the lossy saving is served.
	content := "See " + fidelityLongURL + "\n" + fidelityGoFence + "\n" + fidelityNestedJSON + "\nNow the long story:\n" + fidelityProse(30) + fidelityProse(30)
	msgs := fidelitySystemUser(content)
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(300), nil, nil)
	if !res.LossyApplied {
		t.Fatalf("prose-only truncation must apply, got %+v", res)
	}
	if contains(res.Steps, StepFidelityGateRefused) {
		t.Fatalf("intact structure must not trip the gate, got %+v", res)
	}
	joined := out[len(out)-1].Content
	for _, want := range []string{fidelityLongURL, fidelityGoFence, fidelityNestedJSON} {
		if !strings.Contains(joined, want) {
			t.Fatalf("surviving message must still contain %q", excerpt(want, 60))
		}
	}
}

func TestFidelityGatePreservesToolCallsAndParts(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: strings.Repeat("filler turn ", 60)},
		{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_abc123", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city": "pune"}`}}}},
		{Role: "tool", Content: `{"temp": 28, "unit": "c"}`, ToolCallID: "call_abc123"},
		{Role: "user", Parts: []provider.ContentPart{
			{Type: "text", Text: "Describe this image please"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/img/photo123.png"}},
		}, Content: "Describe this image please"},
		{Role: "user", Content: "Final question " + strings.Repeat("word ", 80)},
	}
	// Budget 120 drops the filler turn whole and keeps the
	// tool/parts messages: the gate must pass and those
	// messages must be byte-identical in the output.
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(120), nil, nil)
	if !res.Applied || !res.LossyApplied {
		t.Fatalf("expected a lossy application, got %+v", res)
	}
	if contains(res.Steps, StepFidelityGateRefused) {
		t.Fatalf("whole-message drops must not trip the gate, got %+v", res)
	}
	var toolMsg, partMsg *provider.Message
	var assistantMsg *provider.Message
	for i := range out {
		switch {
		case out[i].Role == "tool":
			toolMsg = &out[i]
		case out[i].Role == "assistant":
			assistantMsg = &out[i]
		case len(out[i].Parts) > 0:
			partMsg = &out[i]
		}
	}
	if assistantMsg == nil || len(assistantMsg.ToolCalls) != 1 || assistantMsg.ToolCalls[0].ID != "call_abc123" {
		t.Fatalf("assistant tool-call id must survive, got %+v", assistantMsg)
	}
	if toolMsg == nil || toolMsg.ToolCallID != "call_abc123" || toolMsg.Content != `{"temp": 28, "unit": "c"}` {
		t.Fatalf("tool message must survive byte-identical, got %+v", toolMsg)
	}
	if partMsg == nil || !reflect.DeepEqual(partMsg.Parts, msgs[4].Parts) || partMsg.Content != msgs[4].Content {
		t.Fatalf("part-carrying message must survive byte-identical, got %+v", partMsg)
	}

	// Direct: dropping a structured message whole passes.
	kept := []provider.Message{msgs[0], msgs[2], msgs[3], msgs[4], msgs[5]}
	if ok, reasons := CheckLossy(msgs, kept); !ok {
		t.Fatalf("dropping a whole message must pass, got %v", reasons)
	}
}

func TestCheckLossyRefusesRewrittenPartMessage(t *testing.T) {
	orig := []provider.Message{{
		Role: "user",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "Describe this image please"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/img/photo123.png"}},
		},
		Content: "Describe this image please",
	}}
	rewrittenText := []provider.Message{{
		Role: "user",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "Describe this picture please"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/img/photo123.png"}},
		},
		Content: "Describe this picture please",
	}}
	if ok, reasons := CheckLossy(orig, rewrittenText); ok {
		t.Fatal("rewriting a text part must fail")
	} else {
		assertReasonsContain(t, reasons, "part")
	}
	rewrittenImage := []provider.Message{{
		Role: "user",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "Describe this image please"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/img/other.png"}},
		},
		Content: "Describe this image please",
	}}
	if ok, reasons := CheckLossy(orig, rewrittenImage); ok {
		t.Fatal("rewriting an image part must fail")
	} else {
		assertReasonsContain(t, reasons, "part")
	}
}

func TestCheckLossyRefusesRoleAndIdentifierChanges(t *testing.T) {
	base := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "f"}}}},
		{Role: "tool", Content: "result", ToolCallID: "call_1"},
		{Role: "user", Content: "thanks"},
	}
	cases := map[string][]provider.Message{
		"role changed": {
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "f"}}}},
			{Role: "tool", Content: "result", ToolCallID: "call_1"},
			{Role: "user", Content: "thanks"},
		},
		"tool-call id changed": {
			{Role: "system", Content: "Be helpful."},
			{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_2", Type: "function", Function: provider.ToolCallFunction{Name: "f"}}}},
			{Role: "tool", Content: "result", ToolCallID: "call_1"},
			{Role: "user", Content: "thanks"},
		},
		"tool message id changed": {
			{Role: "system", Content: "Be helpful."},
			{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "f"}}}},
			{Role: "tool", Content: "result", ToolCallID: "call_9"},
			{Role: "user", Content: "thanks"},
		},
		"order swapped": {
			{Role: "system", Content: "Be helpful."},
			{Role: "tool", Content: "result", ToolCallID: "call_1"},
			{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "f"}}}},
			{Role: "user", Content: "thanks"},
		},
		"fabricated message":   append(append([]provider.Message{}, base...), provider.Message{Role: "user", Content: "injected"}),
		"last message dropped": base[:3],
	}
	for name, comp := range cases {
		if ok, reasons := CheckLossy(base, comp); ok {
			t.Fatalf("%s: must fail, got ok", name)
		} else if len(reasons) == 0 {
			t.Fatalf("%s: must fail with a reason", name)
		}
	}
}

// M7.2-fix (F1): paren-URL hole. The old urlPattern excluded ')',
// so extractURLs truncated a URL at its first ')'; the gate then
// protected only that prefix and everything after the paren was
// invisible to it. These tests pin the fix. The plain-URL control
// uses the identical geometry: '(' and ')' replaced by '_' so the
// URL is the same length (97 chars) and every cut lands at the
// same offset.
const (
	fidelityParenURL             = "https://en.wikipedia.org/wiki/PC_Tools_(Central_Point_Software)/extra/long/path/segment/here/more"
	fidelityPlainURLSameGeometry = "https://en.wikipedia.org/wiki/PC_Tools__Central_Point_Software_/extra/long/path/segment/here/more"
	fidelityParenURLHead         = "https://en.wikipedia.org/wiki/PC_Tools_(Central_Point_Software)"
	fidelityParenURLTailFragment = "path/segment/here"
)

// parenURLTruncateMiddleCut is the reviewer's geometry: the
// content is the paren URL plus 20 trailing chars (117 runes);
// truncateMiddle(., 0.9) keeps 105 runes, head 63 — exactly up
// to and including the ')' (index 62) — and tail 42, so the cut
// removes "/extra/long/" and the surviving tail starts at
// "path/segment/here/more" (no leading slash: that slash is cut).
func parenURLTruncateMiddleCut(url string) (content, cut string) {
	content = url + strings.Repeat("b", 20)
	return content, truncateMiddle(content, 0.9)
}

func TestFidelityGateRefusesTruncatedParenURL(t *testing.T) {
	// (a1) Real engine cut: truncateMiddle is the cut applyLossy
	// makes (compress.go). The head ends at ')' and the tail
	// fragment "path/segment/here" survives.
	content, cut := parenURLTruncateMiddleCut(fidelityParenURL)
	if strings.Contains(cut, fidelityParenURL) {
		t.Fatal("test setup: the cut must damage the paren URL")
	}
	if !strings.Contains(cut, fidelityParenURLHead) {
		t.Fatalf("test setup: the cut must leave the URL head up to ')', got %q", cut)
	}
	if !strings.Contains(cut, fidelityParenURLTailFragment) {
		t.Fatalf("test setup: the cut must leave the tail fragment %q, got %q", fidelityParenURLTailFragment, cut)
	}
	orig := []provider.Message{{Role: "user", Content: content}}
	comp := []provider.Message{{Role: "user", Content: cut}}
	if ok, reasons := CheckLossy(orig, comp); ok {
		t.Fatalf("paren URL cut just after ')' must fail, got ok (cut=%q)", cut)
	} else {
		assertReasonsContain(t, reasons, "spliced URL")
		assertReasonsContain(t, reasons, "URL cut")
	}

	// (a2) Real applyLossy output: budget 40 truncates the
	// message four times and leaves the paren URL damaged
	// (prefix survives, full URL gone) — the gate must refuse
	// it, and Apply end-to-end must return the original.
	msgs := fidelitySystemUser("See " + fidelityParenURL + " for details.\n" + fidelityProse(30))
	assertGateRefusesRealDamage(t, msgs, 40, "URL")
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(40), nil, nil)
	assertGateRefusal(t, msgs, out, res)
}

func TestFidelityGateRefusesTruncatedPlainURLSameGeometry(t *testing.T) {
	// (b) Plain-URL control in the identical geometry: the same
	// truncateMiddle cut, the same offsets. Must be refused
	// with both reasons, before and after the paren fix.
	content, cut := parenURLTruncateMiddleCut(fidelityPlainURLSameGeometry)
	if strings.Contains(cut, fidelityPlainURLSameGeometry) {
		t.Fatal("test setup: the cut must damage the plain URL")
	}
	if !strings.Contains(cut, fidelityParenURLTailFragment) {
		t.Fatalf("test setup: the cut must leave the tail fragment %q, got %q", fidelityParenURLTailFragment, cut)
	}
	orig := []provider.Message{{Role: "user", Content: content}}
	comp := []provider.Message{{Role: "user", Content: cut}}
	if ok, reasons := CheckLossy(orig, comp); ok {
		t.Fatalf("plain URL cut in the identical geometry must fail, got ok (cut=%q)", cut)
	} else {
		assertReasonsContain(t, reasons, "spliced URL")
		assertReasonsContain(t, reasons, "URL cut")
	}
}

func TestCheckLossyRefusesParenURLCutWithSurvivingTail(t *testing.T) {
	// (c) Hand-built cut: the URL head up to ')' survives, the
	// marker sits where "/extra/long/" was removed, and the tail
	// fragment "path/segment/here" survives. Before the fix
	// the gate saw the original URL itself as only the head
	// (truncated at ')'), so neither check could fire.
	orig := []provider.Message{{Role: "user", Content: "See " + fidelityParenURL + " for details."}}
	comp := []provider.Message{{Role: "user", Content: "See " + fidelityParenURLHead + "\n" + truncationMarker + "\n" + fidelityParenURLTailFragment + " for details."}}
	if strings.Contains(comp[0].Content, fidelityParenURL) {
		t.Fatal("test setup: the hand-built cut must not leave the URL intact")
	}
	if !strings.Contains(comp[0].Content, fidelityParenURLHead) || !strings.Contains(comp[0].Content, fidelityParenURLTailFragment) {
		t.Fatal("test setup: the hand-built cut must leave the head and the tail fragment behind")
	}
	if ok, reasons := CheckLossy(orig, comp); ok {
		t.Fatalf("paren URL cut with a surviving tail fragment must fail, got ok (comp=%q)", comp[0].Content)
	} else {
		assertReasonsContain(t, reasons, "spliced URL")
		assertReasonsContain(t, reasons, "URL cut")
	}
}

func TestFidelityGateServesIntactParenURL(t *testing.T) {
	// (d) A paren URL that survives intact through lossy
	// truncation must still be served: no over-refusal. Same
	// shape as TestFidelityGatePassesWhenStructureIntact, with
	// the paren URL in place of the plain long URL.
	content := "See " + fidelityParenURL + "\n" + fidelityGoFence + "\n" + fidelityNestedJSON + "\nNow the long story:\n" + fidelityProse(30) + fidelityProse(30)
	msgs := fidelitySystemUser(content)
	out, res := Apply("gpt-4o", msgs, fidelityLossyReq(300), nil, nil)
	if !res.LossyApplied {
		t.Fatalf("prose-only truncation must apply, got %+v", res)
	}
	if contains(res.Steps, StepFidelityGateRefused) {
		t.Fatalf("an intact paren URL must not trip the gate, got %+v", res)
	}
	if !strings.Contains(out[len(out)-1].Content, fidelityParenURL) {
		t.Fatal("surviving message must still contain the intact paren URL")
	}
}

func TestCheckLossyDroppedWholeParenURLPasses(t *testing.T) {
	// (e) A paren URL dropped whole with its message must still
	// be served.
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "See " + fidelityParenURL + " for details."},
		{Role: "user", Content: "Keep me " + strings.Repeat("word ", 50)},
	}
	for _, kept := range [][]provider.Message{
		{msgs[0], msgs[2]},
		{msgs[0], msgs[1], msgs[2]},
	} {
		if ok, reasons := CheckLossy(msgs, kept); !ok {
			t.Fatalf("dropping a paren-URL message whole (or keeping it intact) must pass, got %v", reasons)
		}
	}
}

func TestExtractURLsBalancedDelimiters(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"paren URL intact", fidelityParenURL, []string{fidelityParenURL}},
		{"nested parens", "https://example.com/a_(b_(c))/d", []string{"https://example.com/a_(b_(c))/d"}},
		{"URL in surrounding parens", "(see https://example.com/foo/bar)", []string{"https://example.com/foo/bar"}},
		{"markdown link with paren URL", "[text](https://example.com/wiki/A_(B)/c)", []string{"https://example.com/wiki/A_(B)/c"}},
		{"paren URL then sentence period", "See https://example.com/wiki/A_(B)/c.", []string{"https://example.com/wiki/A_(B)/c"}},
		{"plain URL unchanged", "See " + fidelityPlainURLSameGeometry + " end", []string{fidelityPlainURLSameGeometry}},
		{"plain URL in surrounding parens", "(see https://example.com/foo)", []string{"https://example.com/foo"}},
		{"bracket URL", "https://example.com/path[foo]/bar", []string{"https://example.com/path[foo]/bar"}},
		{"IPv6 URL", "https://[::1]/path", []string{"https://[::1]/path"}},
		{"brace URL", "https://example.com/{id}/detail", []string{"https://example.com/{id}/detail"}},
		{"URL in braces prose", "{see https://example.com/foo}", []string{"https://example.com/foo"}},
	}
	for _, tc := range cases {
		if got := extractURLs(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: extractURLs(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestCheckLossyDroppedWholeStructurePasses(t *testing.T) {
	// Whole-message drops are the lossy contract: a fence, a
	// JSON value and a URL may all vanish with their message.
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Code:\n" + fidelityGoFence},
		{Role: "user", Content: "Data:\n" + fidelityNestedJSON + "\nSee " + fidelityLongURL},
		{Role: "user", Content: "Keep me " + strings.Repeat("word ", 50)},
	}
	for _, kept := range [][]provider.Message{
		{msgs[0], msgs[2], msgs[3]},
		{msgs[0], msgs[1], msgs[3]},
		{msgs[0], msgs[3]},
	} {
		if ok, reasons := CheckLossy(msgs, kept); !ok {
			t.Fatalf("dropping whole messages must pass, got %v", reasons)
		}
	}
}
