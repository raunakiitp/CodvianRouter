package cache

import (
	"context"
	"testing"
	"time"
)

func TestExactCacheHitMiss(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	resp := CachedResponse{Content: "hello", Model: "m", Usage: Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}

	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatalf("Get on empty cache = hit, want miss")
	}
	c.Set(ctx, "k1", resp, time.Minute)
	got, ok := c.Get(ctx, "k1")
	if !ok {
		t.Fatalf("Get after Set = miss, want hit")
	}
	if got != resp {
		t.Fatalf("Get = %+v, want %+v", got, resp)
	}
	if _, ok := c.Get(ctx, "other"); ok {
		t.Fatalf("Get unknown key = hit, want miss")
	}
}

func TestExactCacheExpiry(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	c.Set(ctx, "k1", CachedResponse{Content: "x"}, 20*time.Millisecond)
	if _, ok := c.Get(ctx, "k1"); !ok {
		t.Fatalf("Get before expiry = miss, want hit")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatalf("Get after expiry = hit, want miss")
	}
	if c.Size() != 0 {
		t.Fatalf("Size after expired access = %d, want 0", c.Size())
	}
}

func TestExactCacheZeroTTLStoresNothing(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	c.Set(ctx, "k1", CachedResponse{Content: "x"}, 0)
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatalf("ttl=0 entry was stored, want nothing cached")
	}
	c.Set(ctx, "k1", CachedResponse{Content: "x"}, -time.Second)
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatalf("negative-ttl entry was stored, want nothing cached")
	}
}

func TestExactCacheLRUEviction(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(2)
	c.Set(ctx, "a", CachedResponse{Content: "a"}, time.Minute)
	c.Set(ctx, "b", CachedResponse{Content: "b"}, time.Minute)
	// Touch "a" so "b" is least recently used.
	if _, ok := c.Get(ctx, "a"); !ok {
		t.Fatalf("Get a = miss, want hit")
	}
	c.Set(ctx, "c", CachedResponse{Content: "c"}, time.Minute)
	if c.Size() != 2 {
		t.Fatalf("Size = %d, want 2", c.Size())
	}
	if _, ok := c.Get(ctx, "b"); ok {
		t.Fatalf("LRU victim b still present, want evicted")
	}
	if _, ok := c.Get(ctx, "a"); !ok {
		t.Fatalf("recently used a evicted, want present")
	}
	if _, ok := c.Get(ctx, "c"); !ok {
		t.Fatalf("new entry c missing, want present")
	}
}

func TestExactCacheDelete(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	c.Set(ctx, "k1", CachedResponse{Content: "x"}, time.Minute)
	c.Delete(ctx, "k1")
	if _, ok := c.Get(ctx, "k1"); ok {
		t.Fatalf("Get after Delete = hit, want miss")
	}
	c.Delete(ctx, "missing") // deleting a missing key must not panic
}

func TestExactKeyDeterministic(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "be helpful"},
		{Role: "user", Content: "hello"},
	}
	k1, err := ExactKey("model-x", msgs)
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	k2, err := ExactKey("model-x", msgs)
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	if k1 != k2 {
		t.Fatalf("same input gave different keys: %q vs %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("key length = %d, want 64 hex chars (SHA-256)", len(k1))
	}
	k3, err := ExactKey("model-y", msgs)
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	if k3 == k1 {
		t.Fatalf("different model gave same key")
	}
	k4, err := ExactKey("model-x", []Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	if k4 == k1 {
		t.Fatalf("different messages gave same key")
	}
}

func TestPrepareAnnotatesLastUserMessage(t *testing.T) {
	req := &ChatRequest{
		Model: "claude-sonnet-4",
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "first"},
			{Role: "assistant", Content: "reply"},
			{Role: "user", Content: "second"},
		},
	}
	Prepare(req, "anthropic")
	for i, m := range req.Messages {
		if i == 3 {
			if m.CacheControl == nil || m.CacheControl.Type != "ephemeral" {
				t.Fatalf("last user message not annotated: %+v", m)
			}
		} else if m.CacheControl != nil {
			t.Fatalf("message %d unexpectedly annotated: %+v", i, m)
		}
	}
	// Idempotent: a second Prepare leaves exactly one marker.
	Prepare(req, "anthropic")
	n := 0
	for _, m := range req.Messages {
		if m.CacheControl != nil {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("after second Prepare, %d messages annotated, want 1", n)
	}
}

func TestPrepareNoopForUnsupportedProvider(t *testing.T) {
	req := &ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	// OpenAI does automatic prefix caching and ignores cache_control
	// on messages; Gemini uses a separate cachedContents API.
	for _, provider := range []string{"openai", "gemini", "google", "codvian", "unknown-provider", ""} {
		Prepare(req, provider)
		if req.Messages[0].CacheControl != nil {
			t.Fatalf("provider %q: message annotated, want no-op", provider)
		}
	}
}

func TestPrepareNilAndNoUserMessage(t *testing.T) {
	Prepare(nil, "anthropic") // must not panic
	req := &ChatRequest{Model: "m", Messages: []Message{{Role: "system", Content: "s"}}}
	Prepare(req, "anthropic")
	if req.Messages[0].CacheControl != nil {
		t.Fatalf("system-only request annotated, want no-op")
	}
}

func TestNoopSemanticCache(t *testing.T) {
	ctx := context.Background()
	var sc SemanticCache = NoopSemanticCache{}
	if _, ok := sc.Get(ctx, []float32{0.1, 0.2}); ok {
		t.Fatalf("NoopSemanticCache Get = hit, want miss")
	}
	// Set must not panic.
	sc.Set(ctx, []float32{0.1, 0.2}, SemanticEntry{Response: CachedResponse{Content: "x"}})
	if _, ok := sc.Get(ctx, []float32{0.1, 0.2}); ok {
		t.Fatalf("NoopSemanticCache Get after Set = hit, want miss")
	}
}

func TestExactCacheImplementsCache(t *testing.T) {
	var _ Cache = NewExactCache(10)
}
