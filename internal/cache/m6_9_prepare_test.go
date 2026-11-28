package cache

import "testing"

// Prompt-cache marker boundary, pinned by the M6.9 audit
// (2026-11-18): Prepare() annotates exactly anthropic / claude and
// nothing else. openrouter's OpenAI-wire markers are applied by the
// provider client (internal/provider/client.go,
// applyOpenRouterPromptCacheMarker), not by cache.Prepare, so it is
// deliberately out of this set. The audit verified, per provider,
// that no other provider in the tree can honestly be added —
// either the upstream honours no message marker (OpenAI automatic,
// Gemini cachedContents, DeepSeek automatic, Cohere none
// documented) or the upstream honours the marker only in a form
// this router cannot yet carry (Bedrock Converse cachePoint,
// Vertex Claude rawPredict). If a provider is added to
// promptCacheProviders, this test must be updated in the same
// change, with the per-provider evidence in the map comment —
// never silently.

// TestPrepareM69ProviderBoundary pins the exact supported set.
func TestPrepareM69ProviderBoundary(t *testing.T) {
	mkReq := func() *ChatRequest {
		return &ChatRequest{
			Model: "m",
			Messages: []Message{
				{Role: "system", Content: "sys"},
				{Role: "user", Content: "first"},
				{Role: "assistant", Content: "reply"},
				{Role: "user", Content: "second"},
			},
		}
	}
	annotated := func(req *ChatRequest) []int {
		var idx []int
		for i, m := range req.Messages {
			if m.CacheControl != nil {
				idx = append(idx, i)
			}
		}
		return idx
	}
	// Supported: exactly the last user message (index 3), and
	// the marker is the ephemeral type.
	for _, provider := range []string{"anthropic", "claude", "Anthropic", " ANTHROPIC ", "Claude"} {
		req := mkReq()
		Prepare(req, provider)
		got := annotated(req)
		if len(got) != 1 || got[0] != 3 {
			t.Fatalf("provider %q: annotated %v, want [3]", provider, got)
		}
		if req.Messages[3].CacheControl.Type != "ephemeral" {
			t.Fatalf("provider %q: marker type %q, want ephemeral", provider, req.Messages[3].CacheControl.Type)
		}
	}
	// Not supported: no message annotated at all. Bedrock and
	// Vertex are the deliberate non-additions — the upstream
	// honours the marker only translated (Converse cachePoint /
	// Vertex Claude rawPredict), which this router cannot yet
	// carry (see the promptCacheProviders comment); the rest
	// honour no message marker at all. openrouter is out of
	// cache.Prepare's canonical set: its OpenAI-wire marking lives
	// in the provider client and is governed there (g13).
	for _, provider := range []string{
		"bedrock", "vertex", "vertex-partner",
		"openai", "gemini", "google",
		"deepseek", "cohere", "fireworks", "together", "groq", "xai",
		"mistral", "ollama", "openrouter", "unknown-provider", "",
	} {
		req := mkReq()
		Prepare(req, provider)
		if got := annotated(req); len(got) != 0 {
			t.Fatalf("provider %q: annotated %v, want no-op", provider, got)
		}
	}
}

// TestPrepareM69LastUserTargetingUnchanged pins that the marker
// still lands on the LAST user message only, even with trailing
// assistant/tool messages — the targeting the tier pricing and
// cache-affinity ordering assume.
func TestPrepareM69LastUserTargetingUnchanged(t *testing.T) {
	req := &ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "user", Content: "first"},
			{Role: "user", Content: "second"},
			{Role: "assistant", Content: "reply"},
		},
	}
	Prepare(req, "anthropic")
	for i, m := range req.Messages {
		if i == 1 {
			if m.CacheControl == nil || m.CacheControl.Type != "ephemeral" {
				t.Fatalf("message 1 not annotated: %+v", m)
			}
		} else if m.CacheControl != nil {
			t.Fatalf("message %d unexpectedly annotated: %+v", i, m)
		}
	}
}
