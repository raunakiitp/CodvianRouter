package cache

import (
	"context"
	"testing"
	"time"
)

// Model scoping (CHECKLIST 187) — the bug this slice fixes: the
// semantic vector is text-only, so the 163-164 smoke caught a
// cached claude-sonnet response being served for a gpt-4o request
// in 0ms. A cached response must be served only to a request for
// the same model, however similar the text.

const (
	modelScopeClaude = "claude-sonnet-4-5"
	modelScopeGPT    = "gpt-4o"
	modelScopeText   = "Describe the monsoon season in Mumbai."
)

// TestSemanticCacheCrossModelNeverHits is the exact 163-164 repro
// at the cache layer: identical text (similarity 1.0), stored for
// claude-sonnet-4-5, must NEVER hit for a gpt-4o lookup — neither
// through GetTextForModel nor through the raw vector GetForModel.
func TestSemanticCacheCrossModelNeverHits(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, modelScopeClaude, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "Sunny.", Model: modelScopeClaude}}, time.Hour)

	if got, ok := c.GetTextForModel(ctx, modelScopeGPT, modelScopeText); ok {
		t.Fatalf("cross-model GetTextForModel = hit (%q), want miss", got.Response.Content)
	}
	if got, ok := c.GetForModel(ctx, modelScopeGPT, c.EmbedText(modelScopeText)); ok {
		t.Fatalf("cross-model GetForModel = hit (%q), want miss", got.Response.Content)
	}
	// The same-model lookup still hits, with similarity 1.0.
	got, ok := c.GetTextForModel(ctx, modelScopeClaude, modelScopeText)
	if !ok || got.Response.Content != "Sunny." {
		t.Fatalf("same-model GetTextForModel = (%+v, %v), want Sunny. hit", got, ok)
	}
	if got.Similarity != 1 {
		t.Fatalf("same-model similarity = %v, want 1", got.Similarity)
	}
}

// TestSemanticCacheCrossModelParaphraseNeverHits: even a paraphrase
// (similarity >= threshold but < 1) must not cross models.
func TestSemanticCacheCrossModelParaphraseNeverHits(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, modelScopeClaude, "What is the capital of France?",
		SemanticEntry{Response: CachedResponse{Content: "Paris is the capital of France.", Model: modelScopeClaude}}, time.Hour)

	// Same-model paraphrase hits (the layer still works).
	if got, ok := c.GetTextForModel(ctx, modelScopeClaude, "What is the capital of France today?"); !ok || got.Response.Content != "Paris is the capital of France." {
		t.Fatalf("same-model paraphrase = (%+v, %v), want Paris hit", got, ok)
	}
	// Cross-model paraphrase misses.
	if got, ok := c.GetTextForModel(ctx, modelScopeGPT, "What is the capital of France today?"); ok {
		t.Fatalf("cross-model paraphrase = hit (%q), want miss", got.Response.Content)
	}
}

// TestSemanticCacheSameTextDifferentModelsCoexist: the same text
// stored under two models takes two slots — the per-model dedup
// must not collapse them — and each lookup gets its own model's
// content. Re-storing the same model + text replaces in place
// (size unchanged), never evicting the other model's entry.
func TestSemanticCacheSameTextDifferentModelsCoexist(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, modelScopeClaude, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "Sunny.", Model: modelScopeClaude}}, time.Hour)
	c.SetTextForModel(ctx, modelScopeGPT, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "GPT sunny.", Model: modelScopeGPT}}, time.Hour)
	if c.Size() != 2 {
		t.Fatalf("Size = %d, want 2 (one entry per model)", c.Size())
	}
	got, ok := c.GetTextForModel(ctx, modelScopeClaude, modelScopeText)
	if !ok || got.Response.Content != "Sunny." {
		t.Fatalf("claude lookup = (%+v, %v), want Sunny.", got, ok)
	}
	got, ok = c.GetTextForModel(ctx, modelScopeGPT, modelScopeText)
	if !ok || got.Response.Content != "GPT sunny." {
		t.Fatalf("gpt-4o lookup = (%+v, %v), want GPT sunny.", got, ok)
	}
	// Re-store for claude: replaces in place, gpt-4o untouched.
	c.SetTextForModel(ctx, modelScopeClaude, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "Cloudy.", Model: modelScopeClaude}}, time.Hour)
	if c.Size() != 2 {
		t.Fatalf("Size after re-store = %d, want 2", c.Size())
	}
	got, ok = c.GetTextForModel(ctx, modelScopeClaude, modelScopeText)
	if !ok || got.Response.Content != "Cloudy." {
		t.Fatalf("claude re-store lookup = (%+v, %v), want Cloudy.", got, ok)
	}
	got, ok = c.GetTextForModel(ctx, modelScopeGPT, modelScopeText)
	if !ok || got.Response.Content != "GPT sunny." {
		t.Fatalf("gpt-4o lookup after claude re-store = (%+v, %v), want GPT sunny.", got, ok)
	}
}

// TestSemanticCacheRawGetNeverServesModelScopedEntry: the raw
// model-less Get/GetText can only read entries stored without a
// recorded model. A model-scoped entry is invisible to it —
// fail-closed, so no caller can reintroduce a cross-model hit
// through the raw API — and an unattributed entry is invisible to
// a model-scoped lookup.
func TestSemanticCacheRawGetNeverServesModelScopedEntry(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, modelScopeClaude, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "Sunny.", Model: modelScopeClaude}}, time.Hour)
	if got, ok := c.GetText(ctx, modelScopeText); ok {
		t.Fatalf("raw GetText served a model-scoped entry (%q), want miss", got.Response.Content)
	}
	if got, ok := c.Get(ctx, c.EmbedText(modelScopeText)); ok {
		t.Fatalf("raw Get served a model-scoped entry (%q), want miss", got.Response.Content)
	}
	// An entry stored without a model is still readable raw...
	c.SetText(ctx, "unattributed text here", SemanticEntry{Response: CachedResponse{Content: "plain"}}, time.Hour)
	if got, ok := c.GetText(ctx, "unattributed text here"); !ok || got.Response.Content != "plain" {
		t.Fatalf("raw GetText for unattributed entry = (%+v, %v), want plain hit", got, ok)
	}
	// ...but never served to a request naming a model.
	if got, ok := c.GetTextForModel(ctx, modelScopeGPT, "unattributed text here"); ok {
		t.Fatalf("model lookup served an unattributed entry (%q), want miss", got.Response.Content)
	}
}

// TestSemanticCacheSameModelAcrossProvidersSharesEntry pins the
// provider rule explicitly: the scope key contains the model id
// and nothing else. There is no provider input to the cache at
// all, so an entry stored for gpt-4o is served for a gpt-4o
// lookup regardless of which provider produced it or would serve
// it — the same model id on another provider shares the entry,
// by design, because the entry records the model that produced
// it, not the pipe it came through.
func TestSemanticCacheSameModelAcrossProvidersSharesEntry(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	// Stored "via OpenAI" (the cache has no provider field — the
	// test names the two providers only in comments), looked up
	// "via OpenRouter": same model id, so the entry applies.
	c.SetTextForModel(ctx, modelScopeGPT, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "GPT sunny.", Model: modelScopeGPT}}, time.Hour)
	got, ok := c.GetTextForModel(ctx, modelScopeGPT, modelScopeText)
	if !ok || got.Response.Content != "GPT sunny." {
		t.Fatalf("same-model lookup across providers = (%+v, %v), want GPT sunny. hit", got, ok)
	}
}

// TestSemanticCacheModelMatchIsExact: the model match is an exact,
// case-sensitive string match — the same rule the exact-match
// cache applies through ExactKey. "gpt-4o" and "GPT-4o" are
// different scopes.
func TestSemanticCacheModelMatchIsExact(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetTextForModel(ctx, modelScopeGPT, modelScopeText,
		SemanticEntry{Response: CachedResponse{Content: "GPT sunny.", Model: modelScopeGPT}}, time.Hour)
	if got, ok := c.GetTextForModel(ctx, "GPT-4o", modelScopeText); ok {
		t.Fatalf("case-differing model lookup = hit (%q), want miss", got.Response.Content)
	}
	if got, ok := c.GetTextForModel(ctx, "gpt-4o-mini", modelScopeText); ok {
		t.Fatalf("different model lookup = hit (%q), want miss", got.Response.Content)
	}
}

// TestSemanticCacheWideVectorCrossModelMiss covers the legacy full
// scan: vectors longer than SemanticVectorDims bypass the inverted
// index, and the model filter must apply there too.
func TestSemanticCacheWideVectorCrossModelMiss(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	wide := make([]float32, SemanticVectorDims+44)
	for i := range wide {
		wide[i] = 0.01
	}
	wide[0] = 1
	c.SetForModel(ctx, modelScopeClaude, wide, SemanticEntry{Response: CachedResponse{Content: "wide claude", Model: modelScopeClaude}})
	if got, ok := c.GetForModel(ctx, modelScopeGPT, wide); ok {
		t.Fatalf("cross-model wide lookup = hit (%q), want miss", got.Response.Content)
	}
	got, ok := c.GetForModel(ctx, modelScopeClaude, wide)
	if !ok || got.Response.Content != "wide claude" {
		t.Fatalf("same-model wide lookup = (%+v, %v), want wide claude hit", got, ok)
	}
}
