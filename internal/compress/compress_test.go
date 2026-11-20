package compress

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

func boolPtr(b bool) *bool { return &b }

func TestLosslessDedupeSystem(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "Be concise."},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied || !res.Verified {
		t.Fatalf("expected applied+verified, got %+v", res)
	}
	if len(out) != 5 {
		t.Fatalf("expected 5 messages (one system dup removed), got %d", len(out))
	}
	if out[0].Content != "You are helpful." || out[4].Content != "Be concise." {
		t.Fatalf("wrong survivors: %+v", out)
	}
	// The repeated USER turns are intentional until proven
	// otherwise: they must survive.
	if out[1].Content != "hi" || out[2].Content != "hi" || out[3].Content != "hi" {
		t.Fatalf("user duplicates must be kept, got %+v", out)
	}
	if !contains(res.Steps, StepDedupeSystem) {
		t.Fatalf("steps %v should name dedupe", res.Steps)
	}
}

func TestLosslessWhitespace(t *testing.T) {
	in := "Hello world   \n\n\n\nSecond para\t \n\n\nThird"
	want := "Hello world\n\nSecond para\n\nThird"
	msgs := []provider.Message{{Role: "user", Content: in}}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied {
		t.Fatalf("expected whitespace step to apply, got %+v", res)
	}
	if out[0].Content != want {
		t.Fatalf("whitespace: got %q want %q", out[0].Content, want)
	}
	if !contains(res.Steps, StepWhitespace) {
		t.Fatalf("steps %v should name whitespace", res.Steps)
	}
}

func TestLosslessWhitespaceCodeFenceUntouched(t *testing.T) {
	in := "See this:   \n```\ncode   with    spaces\n\n\nblank lines kept\n```\n\n\n\nAfter   "
	msgs := []provider.Message{{Role: "user", Content: in}}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied {
		t.Fatalf("expected applied, got %+v", res)
	}
	if !strings.Contains(out[0].Content, "code   with    spaces\n\n\nblank lines kept") {
		t.Fatalf("fence content must be byte-identical, got %q", out[0].Content)
	}
	if !strings.Contains(out[0].Content, "See this:\n```") {
		t.Fatalf("text before fence should be trimmed, got %q", out[0].Content)
	}
	if strings.HasSuffix(out[0].Content, " ") {
		t.Fatalf("trailing whitespace after fence should go, got %q", out[0].Content)
	}
}

func TestLosslessToolResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: "assistant", Content: "calling", ToolCalls: []provider.ToolCall{{ID: "c1"}}},
		{Role: "tool", Content: "stale result", ToolCallID: "c1"},
		{Role: "tool", Content: "fresh result", ToolCallID: "c1"},
		{Role: "tool", Content: "other call", ToolCallID: "c2"},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied || !res.Verified {
		t.Fatalf("expected applied+verified, got %+v", res)
	}
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(out), out)
	}
	if out[1].Content != "fresh result" || out[2].Content != "other call" {
		t.Fatalf("expected last-per-id kept, got %+v", out)
	}
	if !contains(res.Steps, StepToolResults) {
		t.Fatalf("steps %v should name tool results", res.Steps)
	}
}

func TestLosslessIdempotentAndVerified(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Rules.  \n\n\n\nMore rules."},
		{Role: "system", Content: "Rules.  \n\n\n\nMore rules."},
		{Role: "user", Content: "Hello   \n\n\nworld"},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Verified {
		t.Fatalf("expected verified, got %+v", res)
	}
	if !VerifyLossless(msgs, out) {
		t.Fatal("VerifyLossless must pass for Apply output")
	}
	// Second pass is a no-op: already the normal form.
	out2, res2 := Apply("gpt-4o", out, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if res2.Applied {
		t.Fatalf("second pass must be a no-op, got %+v", res2)
	}
	if len(out2) != len(out) {
		t.Fatalf("second pass changed length: %d vs %d", len(out2), len(out))
	}
}

func TestVerifyLosslessRejectsTampering(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "What is 2+2?"},
	}
	out, _ := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !VerifyLossless(msgs, out) {
		t.Fatal("honest output must verify")
	}
	// A changed word is not a lossless step.
	tampered := append([]provider.Message{}, out...)
	tampered[1].Content = "What is 3+3?"
	if VerifyLossless(msgs, tampered) {
		t.Fatal("changed word must fail verification")
	}
	// A dropped non-duplicate message is not a lossless step.
	if VerifyLossless(msgs, out[:1]) {
		t.Fatal("dropped user message must fail verification")
	}
	// Whitespace changed inside a code fence is not a lossless step.
	fenced := []provider.Message{{Role: "user", Content: "```\na   b\n```"}}
	fencedOut := []provider.Message{{Role: "user", Content: "```\na b\n```"}}
	if VerifyLossless(fenced, fencedOut) {
		t.Fatal("fence whitespace change must fail verification")
	}
}

func TestLosslessNoOp(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Hello"},
	}
	_, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if res.Applied {
		t.Fatalf("nothing to compress: must report not applied, got %+v", res)
	}
	if res.SavedTokens != 0 {
		t.Fatalf("no-op must save 0, got %d", res.SavedTokens)
	}
}

func TestDecide(t *testing.T) {
	if ok, _, src := Decide(nil, "gpt-4o", []string{"gpt-4o"}); !ok || src != "model-config" {
		t.Fatalf("model list should enable lossless, got %v %q", ok, src)
	}
	if ok, _, _ := Decide(nil, "other", []string{"gpt-4o"}); ok {
		t.Fatal("unlisted model must not compress")
	}
	// Explicit per-request false opts out even for a listed model.
	if ok, _, _ := Decide(&provider.CompressionRequest{Lossless: boolPtr(false)}, "gpt-4o", []string{"gpt-4o"}); ok {
		t.Fatal("explicit lossless:false must opt out")
	}
	// Lossy implies the lossless steps first, and is request-only.
	ok, max, src := Decide(&provider.CompressionRequest{LossyMaxPromptTokens: 100}, "gpt-4o", nil)
	if !ok || max != 100 || src != "request" {
		t.Fatalf("lossy must imply lossless, got %v %d %q", ok, max, src)
	}
	// A bare lossless:true with no lossy field requests no lossy.
	_, max, _ = Decide(&provider.CompressionRequest{Lossless: boolPtr(true)}, "gpt-4o", nil)
	if max != 0 {
		t.Fatalf("lossless alone must not request lossy, got max %d", max)
	}
}

func TestLossyRequiresOptIn(t *testing.T) {
	// A long prompt with no opt-in and no model listing: untouched.
	var msgs []provider.Message
	msgs = append(msgs, provider.Message{Role: "system", Content: "You are helpful."})
	for i := 0; i < 20; i++ {
		msgs = append(msgs, provider.Message{Role: "user", Content: "filler turn number " + strings.Repeat("word ", 40)})
		msgs = append(msgs, provider.Message{Role: "assistant", Content: "filler reply " + strings.Repeat("word ", 40)})
	}
	out, res := Apply("gpt-4o", msgs, nil, nil, nil)
	if res.Applied || res.LossyApplied {
		t.Fatalf("no opt-in must mean no compression, got %+v", res)
	}
	if len(out) != len(msgs) {
		t.Fatal("no opt-in must leave messages untouched")
	}
	// The same prompt WITH the per-call lossy opt-in: trimmed.
	out, res = Apply("gpt-4o", msgs, &provider.CompressionRequest{LossyMaxPromptTokens: 100}, nil, nil)
	if !res.Applied || !res.LossyApplied || !res.LossyRequested {
		t.Fatalf("lossy opt-in must apply, got %+v", res)
	}
	if res.AfterTokens > 100 {
		t.Fatalf("lossy must reach the budget when droppable turns exist: after=%d", res.AfterTokens)
	}
	if res.SavedTokens != res.BeforeTokens-res.AfterTokens {
		t.Fatalf("saved must equal before-after: %+v", res)
	}
	// Pinned: system first, last user message present, last
	// message (an assistant reply here) present.
	if out[0].Role != "system" || out[0].Content != "You are helpful." {
		t.Fatalf("lossy must keep the system prompt, got %+v", out[0])
	}
	lastUser := -1
	for i, m := range out {
		if m.Role == "user" {
			lastUser = i
		}
	}
	if lastUser < 0 || !strings.HasPrefix(out[lastUser].Content, "filler turn number") {
		t.Fatalf("lossy must keep a last user message, got %+v", out)
	}
	if out[len(out)-1].Role != "assistant" {
		t.Fatalf("lossy must keep the last message, got %+v", out[len(out)-1])
	}
	if !contains(res.Steps, StepLossyDropped) {
		t.Fatalf("steps %v should name lossy drop", res.Steps)
	}
}

func TestLossyNotNeededReportsHonestly(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "Hi"},
	}
	_, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{LossyMaxPromptTokens: 1000}, nil, nil)
	if res.LossyApplied {
		t.Fatalf("under-budget prompt must not claim lossy applied, got %+v", res)
	}
	if !res.LossyRequested {
		t.Fatalf("lossy was requested, must be recorded, got %+v", res)
	}
	if res.Applied {
		t.Fatalf("nothing changed: must report not applied, got %+v", res)
	}
}

func TestLossyTruncatesSingleLongMessage(t *testing.T) {
	long := strings.Repeat("lorem ipsum dolor sit amet ", 200)
	msgs := []provider.Message{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: long},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{LossyMaxPromptTokens: 50}, nil, nil)
	if !res.LossyApplied {
		t.Fatalf("single long message must be truncated, got %+v", res)
	}
	if !contains(res.Steps, StepLossyTruncated) {
		t.Fatalf("steps %v should name truncation", res.Steps)
	}
	if !strings.Contains(out[len(out)-1].Content, truncationMarker) {
		t.Fatalf("truncated message must carry the marker, got %q", out[len(out)-1].Content)
	}
	if res.AfterTokens > 200 {
		t.Fatalf("truncation must shrink hard: after=%d", res.AfterTokens)
	}
}

func TestSavingsAccounting(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: strings.Repeat("System instruction text. ", 20)},
		{Role: "system", Content: strings.Repeat("System instruction text. ", 20)},
		{Role: "user", Content: "Hello   \n\n\n\nworld"},
	}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied || res.BeforeTokens <= 0 || res.AfterTokens <= 0 {
		t.Fatalf("counts must be positive, got %+v", res)
	}
	if res.BeforeTokens <= res.AfterTokens {
		t.Fatalf("before must exceed after, got %+v", res)
	}
	if res.SavedTokens != res.BeforeTokens-res.AfterTokens {
		t.Fatalf("saved mismatch: %+v", res)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 messages after dedupe, got %d", len(out))
	}
}

func TestPriorStampKeepsOriginalBefore(t *testing.T) {
	// Server pass (literal model) then engine pass (resolved
	// model): the saving must still be measured against the
	// ORIGINAL prompt, not the already-compressed one.
	msgs := []provider.Message{
		{Role: "system", Content: strings.Repeat("System instruction text. ", 20)},
		{Role: "system", Content: strings.Repeat("System instruction text. ", 20)},
		{Role: "user", Content: "Hello"},
	}
	out1, res1 := Apply("auto/cheap", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res1.Applied {
		t.Fatalf("first pass must apply, got %+v", res1)
	}
	out2, res2 := Apply("gpt-4o", out1, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, res1)
	if len(out2) != len(out1) {
		t.Fatalf("second pass must not change messages: %d vs %d", len(out2), len(out1))
	}
	if res2.BeforeTokens != res1.BeforeTokens {
		t.Fatalf("before must survive the re-pass: %d vs %d", res2.BeforeTokens, res1.BeforeTokens)
	}
	if res2.SavedTokens != res1.SavedTokens {
		t.Fatalf("saved must survive the re-pass: %d vs %d", res2.SavedTokens, res1.SavedTokens)
	}
}

func TestPartsRewrittenConsistently(t *testing.T) {
	msgs := []provider.Message{{
		Role: "user",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "Hello   \n\n\n\nworld"},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "http://example.invalid/x.png"}},
		},
		Content: "Hello   \n\n\n\nworld",
	}}
	out, res := Apply("gpt-4o", msgs, &provider.CompressionRequest{Lossless: boolPtr(true)}, nil, nil)
	if !res.Applied {
		t.Fatalf("expected applied, got %+v", res)
	}
	if out[0].Parts[0].Text != "Hello\n\nworld" {
		t.Fatalf("text part must be rewritten, got %q", out[0].Parts[0].Text)
	}
	if out[0].Content != "Hello\n\nworld" {
		t.Fatalf("Content must be rebuilt from parts, got %q", out[0].Content)
	}
	if out[0].Parts[1].ImageURL == nil || out[0].Parts[1].ImageURL.URL != "http://example.invalid/x.png" {
		t.Fatalf("image part must survive untouched, got %+v", out[0].Parts)
	}
}
