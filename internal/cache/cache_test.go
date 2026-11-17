package cache

import (
	"context"
	"math"
	"sync"
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

func TestLocalSemanticCacheImplementsInterface(t *testing.T) {
	var _ SemanticCache = NewLocalSemanticCache()
}

func TestEmbedTextDeterministicAndNormalized(t *testing.T) {
	c := NewLocalSemanticCache()
	v1 := c.EmbedText("explain how to install python on windows step by step")
	v2 := c.EmbedText("explain how to install python on windows step by step")
	if len(v1) != SemanticVectorDims {
		t.Fatalf("EmbedText length = %d, want %d", len(v1), SemanticVectorDims)
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("EmbedText not deterministic at dim %d: %v vs %v", i, v1[i], v2[i])
		}
	}
	var sum float64
	for _, x := range v1 {
		sum += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sum)-1) > 1e-5 {
		t.Fatalf("EmbedText L2 norm = %v, want ~1.0", math.Sqrt(sum))
	}
	// Token-less text yields the zero vector.
	for i, x := range c.EmbedText("   !!! ") {
		if x != 0 {
			t.Fatalf("empty text vector[%d] = %v, want 0", i, x)
		}
	}
}

func TestLocalSemanticCacheIdenticalTextHits(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	text := "explain how to install python on windows step by step"
	c.SetText(ctx, text, SemanticEntry{Response: CachedResponse{Content: "install guide"}}, time.Minute)
	got, ok := c.GetText(ctx, text)
	if !ok {
		t.Fatalf("GetText identical text = miss, want hit")
	}
	if got.Response.Content != "install guide" {
		t.Fatalf("GetText content = %q, want %q", got.Response.Content, "install guide")
	}
	if math.Abs(float64(got.Similarity)-1.0) > 1e-5 {
		t.Fatalf("identical text similarity = %v, want ~1.0", got.Similarity)
	}
}

func TestLocalSemanticCacheHighOverlapHits(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetText(ctx, "explain how to install python on windows step by step",
		SemanticEntry{Response: CachedResponse{Content: "install guide"}}, time.Minute)
	// One word changed out of nine: high token overlap.
	got, ok := c.GetText(ctx, "explain how to install python on mac step by step")
	if !ok {
		t.Fatalf("GetText high-overlap text = miss, want hit")
	}
	if got.Similarity < DefaultSemanticThreshold {
		t.Fatalf("high-overlap similarity = %v, want >= %v", got.Similarity, DefaultSemanticThreshold)
	}
}

func TestLocalSemanticCacheUnrelatedMisses(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetText(ctx, "explain how to install python on windows step by step",
		SemanticEntry{Response: CachedResponse{Content: "install guide"}}, time.Minute)
	if _, ok := c.GetText(ctx, "what is the capital of france"); ok {
		t.Fatalf("GetText unrelated text = hit, want miss")
	}
	if _, ok := c.GetText(ctx, ""); ok {
		t.Fatalf("GetText empty text = hit, want miss")
	}
}

func TestLocalSemanticCacheBelowThresholdMisses(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetText(ctx, "how do i reset my password in the app",
		SemanticEntry{Response: CachedResponse{Content: "reset help"}}, time.Minute)
	// Shares words ("how do i ... my ... in the app") but swaps the
	// key content words: token-overlap similarity lands well below
	// the 0.85 default threshold.
	if _, ok := c.GetText(ctx, "how do i change my username in the app"); ok {
		t.Fatalf("GetText near-miss = hit, want miss below threshold")
	}

	// A stricter threshold also rejects even a high-overlap query.
	strict := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{Threshold: 0.99})
	strict.SetText(ctx, "explain how to install python on windows step by step",
		SemanticEntry{Response: CachedResponse{Content: "install guide"}}, time.Minute)
	if _, ok := strict.GetText(ctx, "explain how to install python on mac step by step"); ok {
		t.Fatalf("threshold 0.99: high-overlap query = hit, want miss")
	}
	if _, ok := strict.GetText(ctx, "explain how to install python on windows step by step"); !ok {
		t.Fatalf("threshold 0.99: identical text = miss, want hit")
	}
}

func TestLocalSemanticCacheTTLExpiry(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCache()
	c.SetText(ctx, "hello world", SemanticEntry{Response: CachedResponse{Content: "hi"}}, 20*time.Millisecond)
	if _, ok := c.GetText(ctx, "hello world"); !ok {
		t.Fatalf("GetText before expiry = miss, want hit")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.GetText(ctx, "hello world"); ok {
		t.Fatalf("GetText after expiry = hit, want miss")
	}
	// ttl <= 0 stores nothing.
	c.SetText(ctx, "never stored", SemanticEntry{Response: CachedResponse{Content: "x"}}, 0)
	if _, ok := c.GetText(ctx, "never stored"); ok {
		t.Fatalf("ttl=0 entry was stored, want nothing cached")
	}
}

func TestLocalSemanticCacheLRUEviction(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntries: 3})
	c.SetText(ctx, "alpha one two three", SemanticEntry{Response: CachedResponse{Content: "a"}}, time.Minute)
	c.SetText(ctx, "bravo four five six", SemanticEntry{Response: CachedResponse{Content: "b"}}, time.Minute)
	c.SetText(ctx, "charlie seven eight nine", SemanticEntry{Response: CachedResponse{Content: "c"}}, time.Minute)
	// Touch "alpha" so "bravo" is least recently used.
	if _, ok := c.GetText(ctx, "alpha one two three"); !ok {
		t.Fatalf("GetText alpha = miss, want hit")
	}
	c.SetText(ctx, "delta ten eleven twelve", SemanticEntry{Response: CachedResponse{Content: "d"}}, time.Minute)
	if c.Size() != 3 {
		t.Fatalf("Size = %d, want 3", c.Size())
	}
	if _, ok := c.GetText(ctx, "bravo four five six"); ok {
		t.Fatalf("LRU victim bravo still present, want evicted")
	}
	if _, ok := c.GetText(ctx, "alpha one two three"); !ok {
		t.Fatalf("recently used alpha evicted, want present")
	}
	if _, ok := c.GetText(ctx, "delta ten eleven twelve"); !ok {
		t.Fatalf("new entry delta missing, want present")
	}
}

func TestLocalSemanticCacheRawEmbeddingInterface(t *testing.T) {
	ctx := context.Background()
	var sc SemanticCache = NewLocalSemanticCache()
	// The interface path works with vectors produced by EmbedText.
	lc := sc.(*LocalSemanticCache)
	vec := lc.EmbedText("explain how to install python on windows step by step")
	sc.Set(ctx, vec, SemanticEntry{Response: CachedResponse{Content: "install guide"}})
	got, ok := sc.Get(ctx, lc.EmbedText("explain how to install python on windows step by step"))
	if !ok || got.Response.Content != "install guide" {
		t.Fatalf("interface Get = (%+v, %v), want install guide hit", got, ok)
	}
	// Zero / empty embeddings always miss and store nothing.
	if _, ok := sc.Get(ctx, []float32{}); ok {
		t.Fatalf("empty embedding Get = hit, want miss")
	}
	if _, ok := sc.Get(ctx, make([]float32, SemanticVectorDims)); ok {
		t.Fatalf("zero embedding Get = hit, want miss")
	}
}

func TestLocalSemanticCacheConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{MaxEntries: 50})
	texts := []string{
		"explain how to install python on windows step by step",
		"what is the capital of france",
		"how do i reset my password in the app",
		"best way to learn golang concurrency patterns",
		"why is the sky blue on a clear day",
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				text := texts[(g+i)%len(texts)]
				c.SetText(ctx, text, SemanticEntry{Response: CachedResponse{Content: text}}, time.Minute)
				c.GetText(ctx, text)
				c.GetText(ctx, "some unrelated query text here")
			}
		}(g)
	}
	wg.Wait()
	if c.Size() == 0 {
		t.Fatalf("Size after concurrent use = 0, want > 0")
	}
	if c.Size() > 50 {
		t.Fatalf("Size = %d, want <= 50 (max entries)", c.Size())
	}
}
