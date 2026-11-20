package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

func boolPtr(b bool) *bool { return &b }

// Lossless-first compression in the engine (CHECKLIST 183): the
// provider receives the compressed prompt, the trace carries the
// before/after counts, and the ledger row carries the same
// figures — all measured with the local counter.
func TestEngineCompressionLossless(t *testing.T) {
	led := &fakeLedger{}
	client := okClient("hi")
	eng := NewEngine(map[string]provider.Client{"p1": client}, led, Config{})
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful.   \n\n\n\nBe concise."},
		{Role: "system", Content: "You are helpful.   \n\n\n\nBe concise."},
		{Role: "user", Content: "What is 2+2?"},
	}
	req := provider.ChatRequest{
		Model:       "gpt-4o",
		Messages:    msgs,
		Compression: &provider.CompressionRequest{Lossless: boolPtr(true)},
	}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	// The provider saw one system message, whitespace-cleaned.
	if len(client.reqs) != 1 {
		t.Fatalf("client chats = %d, want 1", len(client.reqs))
	}
	sent := client.reqs[0].Messages
	if len(sent) != 2 {
		t.Fatalf("provider saw %d messages, want 2 (deduped system + user)", len(sent))
	}
	if sent[0].Content != "You are helpful.\n\nBe concise." {
		t.Fatalf("provider system = %q, want whitespace-cleaned single copy", sent[0].Content)
	}
	// The trace carries the compression honestly.
	tr := eng.LastTrace()
	if tr == nil || tr.Compression == nil {
		t.Fatalf("trace compression = nil, want the applied block")
	}
	before, err := tokencount.CountPrompt("gpt-4o", msgs)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	after, err := tokencount.CountPrompt("gpt-4o", sent)
	if err != nil {
		t.Fatalf("tokencount: %v", err)
	}
	c := tr.Compression
	if !c.Applied || !c.Verified || c.LossyApplied {
		t.Fatalf("trace compression = %+v, want applied+verified, no lossy", c)
	}
	if c.BeforeTokens != before.Tokens || c.AfterTokens != after.Tokens || c.SavedTokens != before.Tokens-after.Tokens {
		t.Fatalf("trace compression = %+v, want before=%d after=%d saved=%d", c, before.Tokens, after.Tokens, before.Tokens-after.Tokens)
	}
	// The ledger row carries the same figures, on the prompt as sent.
	if led.count() != 1 {
		t.Fatalf("ledger calls = %d, want 1", led.count())
	}
	u := led.calls[0].usage
	if u.PromptBeforeCompression != before.Tokens || u.CompressionSavedTokens != before.Tokens-after.Tokens {
		t.Fatalf("ledger usage = %+v, want before=%d saved=%d", u, before.Tokens, before.Tokens-after.Tokens)
	}
}

// A request with no opt-in and no model listing is untouched —
// and its trace carries no compression block.
func TestEngineCompressionOffByDefault(t *testing.T) {
	led := &fakeLedger{}
	client := okClient("hi")
	eng := NewEngine(map[string]provider.Client{"p1": client}, led, Config{})
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
	}
	req := provider.ChatRequest{Model: "gpt-4o", Messages: msgs}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if len(client.reqs[0].Messages) != 3 {
		t.Fatalf("provider saw %d messages, want 3 (untouched)", len(client.reqs[0].Messages))
	}
	if tr := eng.LastTrace(); tr == nil || tr.Compression != nil {
		t.Fatalf("trace compression = %+v, want nil (nothing applied)", tr)
	}
	if u := led.calls[0].usage; u.PromptBeforeCompression != 0 || u.CompressionSavedTokens != 0 {
		t.Fatalf("ledger usage = %+v, want zero compression fields", u)
	}
}

// Per-model config: a listed model compresses with no per-request
// opt-in; an explicit per-request false opts a single request out.
func TestEngineCompressionModelConfig(t *testing.T) {
	led := &fakeLedger{}
	client := okClient("hi")
	eng := NewEngine(map[string]provider.Client{"p1": client}, led, Config{CompressionLosslessModels: []string{"gpt-4o"}})
	msgs := []provider.Message{
		{Role: "system", Content: "Repeat.  \n\n\n\nRepeat."},
		{Role: "system", Content: "Repeat.  \n\n\n\nRepeat."},
		{Role: "user", Content: "hi"},
	}
	// Listed model, no opt-in: compressed.
	req := provider.ChatRequest{Model: "gpt-4o", Messages: msgs}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if len(client.reqs[0].Messages) != 2 {
		t.Fatalf("listed model: provider saw %d messages, want 2", len(client.reqs[0].Messages))
	}
	if tr := eng.LastTrace(); tr == nil || tr.Compression == nil || tr.Compression.Source != "model-config" {
		t.Fatalf("trace compression = %+v, want source model-config", tr)
	}
	// Same model, explicit opt-out: untouched.
	client2 := okClient("hi")
	eng2 := NewEngine(map[string]provider.Client{"p1": client2}, &fakeLedger{}, Config{CompressionLosslessModels: []string{"gpt-4o"}})
	req2 := provider.ChatRequest{Model: "gpt-4o", Messages: msgs, Compression: &provider.CompressionRequest{Lossless: boolPtr(false)}}
	if _, _, err := eng2.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req2); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if len(client2.reqs[0].Messages) != 3 {
		t.Fatalf("opt-out: provider saw %d messages, want 3 (untouched)", len(client2.reqs[0].Messages))
	}
}

// Lossy needs the explicit per-call opt-in: the same long prompt
// is untouched without it, trimmed with it — and the trace says
// lossy.
func TestEngineCompressionLossyOptIn(t *testing.T) {
	var msgs []provider.Message
	msgs = append(msgs, provider.Message{Role: "system", Content: "You are helpful."})
	for i := 0; i < 10; i++ {
		msgs = append(msgs, provider.Message{Role: "user", Content: "filler " + strings.Repeat("word ", 30)})
		msgs = append(msgs, provider.Message{Role: "assistant", Content: "reply " + strings.Repeat("word ", 30)})
	}
	// No opt-in: untouched.
	client := okClient("hi")
	eng := NewEngine(map[string]provider.Client{"p1": client}, &fakeLedger{}, Config{})
	req := provider.ChatRequest{Model: "gpt-4o", Messages: msgs}
	if _, _, err := eng.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if len(client.reqs[0].Messages) != len(msgs) {
		t.Fatalf("no opt-in: provider saw %d messages, want %d", len(client.reqs[0].Messages), len(msgs))
	}
	// Lossy opt-in: trimmed, trace says lossy.
	client2 := okClient("hi")
	eng2 := NewEngine(map[string]provider.Client{"p1": client2}, &fakeLedger{}, Config{})
	req2 := provider.ChatRequest{Model: "gpt-4o", Messages: msgs, Compression: &provider.CompressionRequest{LossyMaxPromptTokens: 80}}
	if _, _, err := eng2.ChatWithChain(context.Background(), []Attempt{{ProviderID: "p1", Model: "gpt-4o"}}, req2); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if len(client2.reqs[0].Messages) >= len(msgs) {
		t.Fatalf("lossy: provider saw %d messages, want fewer than %d", len(client2.reqs[0].Messages), len(msgs))
	}
	tr := eng2.LastTrace()
	if tr == nil || tr.Compression == nil || !tr.Compression.LossyApplied {
		t.Fatalf("trace compression = %+v, want lossy_applied", tr)
	}
	if tr.Compression.AfterTokens > 80 {
		t.Fatalf("trace after = %d, want <= 80", tr.Compression.AfterTokens)
	}
}

// Streaming: the compressed prompt reaches the provider, and the
// trace published after the drain carries the compression block.
func TestEngineCompressionStreaming(t *testing.T) {
	client := okClient("hi")
	eng := NewEngine(map[string]provider.Client{"p1": client}, &fakeLedger{}, Config{})
	msgs := []provider.Message{
		{Role: "system", Content: "Dup.  \n\n\n\nDup."},
		{Role: "system", Content: "Dup.  \n\n\n\nDup."},
		{Role: "user", Content: "hi"},
	}
	req := provider.ChatRequest{
		Model:       "gpt-4o",
		Messages:    msgs,
		Stream:      true,
		Compression: &provider.CompressionRequest{Lossless: boolPtr(true)},
	}
	if _, err := eng.ChatStreamChunks(context.Background(), req, func(provider.StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	// The fake text client serves streams from its stream path;
	// the chunk path uses ChatStreamChunks on the client — the
	// fakeClient has no ChatStreamChunks, so the engine bridges
	// via ChatStream: check the recorded stream request.
	sreq, ok := client.lastStreamReq()
	if !ok {
		t.Fatalf("no stream request recorded")
	}
	if len(sreq.Messages) != 2 {
		t.Fatalf("stream provider saw %d messages, want 2", len(sreq.Messages))
	}
	tr := eng.LastTrace()
	if tr == nil || tr.Compression == nil || !tr.Compression.Applied {
		t.Fatalf("trace compression = %+v, want applied", tr)
	}
}
