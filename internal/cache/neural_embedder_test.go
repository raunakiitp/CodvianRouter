package cache

// Tests for the B2-31 NeuralEmbedder seam (d933d22 + dedfe63):
// LocalSemanticCacheConfig.NeuralEmbedder supplies real neural
// embeddings for the text APIs (GetText*, SetText*). The cache
// calls the embedder exactly once per text operation, bounds it
// by NeuralTimeout (default 3 s), and fails open to the hashed
// bag-of-words vector on any error, timeout, empty vector, or
// NaN/Inf vector — so a dead or slow embedding provider can
// never break serving. One shared config feeds all three tiers
// (memory, Redis, SQLite) so every tier embeds the same way.
//
// What these tests do NOT cover (stated plainly):
//   - the main.go closure itself (neuralEmbedder inside openStack,
//     which marshals the text to JSON and calls engine.Embed with
//     model text-embedding-3-small): it is a local func inside
//     openStack with no seam to call from a test, so it can only
//     be exercised by booting the full stack. What IS tested here
//     is the contract the closure plugs into — the cache calls
//     the configured NeuralEmbedder exactly once per text
//     operation, and everything downstream of that call.
//   - the persistent tiers' STORE path: PersistentSemanticCache
//     and RedisSemanticCache SetText* use inner.EmbedText (BoW)
//     and never call the configured NeuralEmbedder — a real
//     production gap found by these tests (see
//     TestNeuralEmbedderConfigSharedAcrossTiers). The intended
//     contract is pinned as a SKIP until a fix builder changes
//     those six methods to embedTextCtx.
//   - neural wiring through the Redis tier: OpenRedisSemanticCache
//     dials Redis first, so without a live Redis the tier cannot
//     be opened at all (TestRedisSemanticCacheUnreachableReturnsError
//     pins the fail-open at open time). No fake Redis is used.

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// neuralVecAlpha / neuralVecBeta are deterministic 1536-dim
// stand-ins for provider embedding vectors (NeuralEmbeddingDims).
// Two texts mapping to the same vector are "the same meaning";
// texts mapping to different vectors are unrelated.
func neuralVecAlpha() []float32 {
	v := make([]float32, NeuralEmbeddingDims)
	v[0] = 1.0
	v[1] = 0.5
	return v
}

func neuralVecBeta() []float32 {
	v := make([]float32, NeuralEmbeddingDims)
	v[1000] = 1.0
	v[1001] = 0.5
	return v
}

// countingEmbedder is a NeuralEmbedder test double: it records
// every call (count + exact text + whether the ctx carries a
// deadline) and returns a deterministic vector by keyword:
// "alpha" -> vecAlpha, "beta" -> vecBeta, anything else -> a
// one-hot at index len(text)%1536. Set fail to make every call
// return an error; set block to make every call wait for the
// ctx to expire first.
type countingEmbedder struct {
	mu          sync.Mutex
	calls       int
	texts       []string
	sawDeadline bool
	fail        bool
	block       bool
	malformed   []float32 // when non-nil, returned instead of the keyword vector
}

func (s *countingEmbedder) embed(ctx context.Context, text string) ([]float32, error) {
	s.mu.Lock()
	s.calls++
	s.texts = append(s.texts, text)
	if _, ok := ctx.Deadline(); ok {
		s.sawDeadline = true
	}
	fail := s.fail
	block := s.block
	malformed := s.malformed
	s.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if fail {
		return nil, errTestNeuralDown
	}
	if malformed != nil {
		out := make([]float32, len(malformed))
		copy(out, malformed)
		return out, nil
	}
	switch {
	case strings.Contains(text, "alpha"):
		return neuralVecAlpha(), nil
	case strings.Contains(text, "beta"):
		return neuralVecBeta(), nil
	default:
		v := make([]float32, NeuralEmbeddingDims)
		v[len(text)%NeuralEmbeddingDims] = 1.0
		return v, nil
	}
}

func (s *countingEmbedder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

var errTestNeuralDown = errors.New("test neural embedder down")

func neuralTestEntry(content string) SemanticEntry {
	return SemanticEntry{Response: CachedResponse{Content: content, Model: "m"}}
}

// TestNeuralEmbedderCalledOncePerTextCall pins the once-per-call
// contract: every text API invokes the configured NeuralEmbedder
// exactly once, with the exact text, under a ctx that carries
// the NeuralTimeout deadline — no retries, no double calls.
func TestNeuralEmbedderCalledOncePerTextCall(t *testing.T) {
	stub := &countingEmbedder{}
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	})
	ctx := context.Background()
	entry := neuralTestEntry("r")

	c.SetTextForModel(ctx, "m", "alpha one", entry, time.Hour)
	if got := stub.count(); got != 1 {
		t.Fatalf("SetTextForModel: embedder calls = %d, want 1", got)
	}
	if got := stub.texts[0]; got != "alpha one" {
		t.Fatalf("SetTextForModel: embedder got text %q, want %q", got, "alpha one")
	}

	c.GetTextForModel(ctx, "m", "alpha two")
	if got := stub.count(); got != 2 {
		t.Fatalf("GetTextForModel: embedder calls = %d, want 2", got)
	}
	c.GetText(ctx, "alpha three")
	if got := stub.count(); got != 3 {
		t.Fatalf("GetText: embedder calls = %d, want 3", got)
	}
	c.SetText(ctx, "alpha four", entry, time.Hour)
	if got := stub.count(); got != 4 {
		t.Fatalf("SetText: embedder calls = %d, want 4", got)
	}
	c.GetTextForModelScopedWithThreshold(ctx, "m", "alpha five", "", 0.7, 0)
	if got := stub.count(); got != 5 {
		t.Fatalf("GetTextForModelScopedWithThreshold: embedder calls = %d, want 5", got)
	}
	c.SetTextForModelScoped(ctx, "m", "alpha six", "", 0.7, entry, time.Hour)
	if got := stub.count(); got != 6 {
		t.Fatalf("SetTextForModelScoped: embedder calls = %d, want 6", got)
	}
	if !stub.sawDeadline {
		t.Fatal("embedder ctx carried no deadline: NeuralTimeout is not applied")
	}
}

// TestNeuralEmbedderSimilarityUsesNeuralVectors proves the text
// APIs compare the neural vectors, not token overlap: two texts
// sharing only the word "alpha" (BoW cosine ~0.14, a miss) hit
// when the embedder maps them to the same vector, while a text
// the embedder maps to an unrelated vector misses.
func TestNeuralEmbedderSimilarityUsesNeuralVectors(t *testing.T) {
	stub := &countingEmbedder{}
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	})
	ctx := context.Background()
	c.SetTextForModel(ctx, "m", "alpha one two three", neuralTestEntry("neural-hit"), time.Hour)

	got, hit := c.GetTextForModel(ctx, "m", "alpha four five six")
	if !hit {
		t.Fatal("same neural vector, different words: want hit, got miss (BoW would miss — the neural vector must drive similarity)")
	}
	if got.Response.Content != "neural-hit" {
		t.Fatalf("hit served %q, want %q", got.Response.Content, "neural-hit")
	}
	if got.Similarity < 0.99 {
		t.Fatalf("similarity = %v, want >= 0.99 for identical neural vectors", got.Similarity)
	}

	if _, hit := c.GetTextForModel(ctx, "m", "beta seven eight nine"); hit {
		t.Fatal("unrelated neural vector: want miss, got hit")
	}
}

// TestNeuralEmbedderFailureFallsBackToBoW: when the embedder
// errors, every text API still serves with bag-of-words
// similarity — identical text hits, a paraphrase misses — and
// the embedder is still attempted exactly once per call (no
// retry loop hiding the outage).
func TestNeuralEmbedderFailureFallsBackToBoW(t *testing.T) {
	stub := &countingEmbedder{fail: true}
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	})
	ctx := context.Background()
	entry := neuralTestEntry("bow-hit")

	c.SetTextForModel(ctx, "m", "hello world foo", entry, time.Hour)
	if got := stub.count(); got != 1 {
		t.Fatalf("SetTextForModel with failing embedder: calls = %d, want 1", got)
	}
	got, hit := c.GetTextForModel(ctx, "m", "hello world foo")
	if !hit || got.Response.Content != "bow-hit" {
		t.Fatalf("identical text with failing embedder: hit=%v content=%q, want hit with bow-hit", hit, got.Response.Content)
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("GetTextForModel with failing embedder: calls = %d, want 2", got)
	}
	if _, hit := c.GetTextForModel(ctx, "m", "completely different words here"); hit {
		t.Fatal("paraphrase with failing embedder: want miss (BoW behaviour), got hit")
	}
	if got := stub.count(); got != 3 {
		t.Fatalf("second GetTextForModel with failing embedder: calls = %d, want 3", got)
	}
}

// TestNeuralEmbedderTimeoutFallsBackToBoW: an embedder that never
// returns inside NeuralTimeout must not stall serving — the text
// APIs fall back to BoW and return promptly.
func TestNeuralEmbedderTimeoutFallsBackToBoW(t *testing.T) {
	stub := &countingEmbedder{block: true}
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
		NeuralTimeout:  50 * time.Millisecond,
	})
	ctx := context.Background()
	entry := neuralTestEntry("bow-hit")

	start := time.Now()
	c.SetTextForModel(ctx, "m", "hello world foo", entry, time.Hour)
	got, hit := c.GetTextForModel(ctx, "m", "hello world foo")
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("blocking embedder: text APIs took %v, want < 2s (NeuralTimeout must bound the call)", elapsed)
	}
	if !hit || got.Response.Content != "bow-hit" {
		t.Fatalf("blocking embedder: hit=%v content=%q, want hit with bow-hit via BoW fallback", hit, got.Response.Content)
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("blocking embedder: calls = %d, want 2 (one attempt per call, then fallback)", got)
	}
}

// TestNeuralEmbedderMalformedVectorFallsBackToBoW: an empty,
// NaN, or infinite vector is not a usable embedding — the text
// APIs fall back to BoW instead of storing or scoring garbage.
func TestNeuralEmbedderMalformedVectorFallsBackToBoW(t *testing.T) {
	cases := map[string][]float32{
		"empty": {},
		"NaN":   {float32(math.NaN())},
		"+Inf":  {float32(math.Inf(1))},
		"-Inf":  {float32(math.Inf(-1))},
	}
	for name, vec := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &countingEmbedder{malformed: vec}
			c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
				NeuralEmbedder: stub.embed,
				NeuralModel:    DefaultNeuralEmbeddingModel,
			})
			ctx := context.Background()
			c.SetTextForModel(ctx, "m", "hello world foo", neuralTestEntry("bow-hit"), time.Hour)
			got, hit := c.GetTextForModel(ctx, "m", "hello world foo")
			if !hit || got.Response.Content != "bow-hit" {
				t.Fatalf("malformed %s vector: hit=%v content=%q, want hit with bow-hit via BoW fallback", name, hit, got.Response.Content)
			}
		})
	}
}

// TestNeuralEmbedderNilMeansBoW pins the default: with no
// embedder configured, the text APIs are exactly the hashed
// bag-of-words cache (identical text hits, paraphrase misses).
func TestNeuralEmbedderNilMeansBoW(t *testing.T) {
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{})
	ctx := context.Background()
	c.SetTextForModel(ctx, "m", "hello world foo", neuralTestEntry("bow-hit"), time.Hour)
	if got, hit := c.GetTextForModel(ctx, "m", "hello world foo"); !hit || got.Response.Content != "bow-hit" {
		t.Fatalf("no embedder, identical text: hit=%v content=%q, want hit with bow-hit", hit, got.Response.Content)
	}
	if _, hit := c.GetTextForModel(ctx, "m", "completely different words here"); hit {
		t.Fatal("no embedder, paraphrase: want miss, got hit")
	}
}

// TestNeuralEmbedderOutageNeverServesNeuralEntry pins the
// isolation the main.go comment promises: a neural vector
// (1536 dims) and a bag-of-words vector (256 dims) never match
// each other. An entry stored while the embedder was live is
// NOT served to a BoW query during an outage — and is served
// again once the embedder returns.
func TestNeuralEmbedderOutageNeverServesNeuralEntry(t *testing.T) {
	stub := &countingEmbedder{}
	c := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	})
	ctx := context.Background()
	c.SetTextForModel(ctx, "m", "alpha one two three", neuralTestEntry("neural-hit"), time.Hour)

	// Simulate the outage: the embedder is gone.
	c.neuralEmbedder = nil
	if _, hit := c.GetTextForModel(ctx, "m", "alpha one two three"); hit {
		t.Fatal("outage: BoW query served a neural entry, want miss (1536-dim vs 256-dim vectors must never match)")
	}
	// Embedder back: the same lookup hits again.
	c.neuralEmbedder = stub.embed
	got, hit := c.GetTextForModel(ctx, "m", "alpha one two three")
	if !hit || got.Response.Content != "neural-hit" {
		t.Fatalf("embedder restored: hit=%v content=%q, want hit with neural-hit", hit, got.Response.Content)
	}
}

// TestNeuralEmbedderModelRecorded pins that the configured model
// is recorded (defaulting to text-embedding-3-small when unset),
// so diagnostics can tell which model produced a vector.
func TestNeuralEmbedderModelRecorded(t *testing.T) {
	def := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{})
	if def.neuralModel != DefaultNeuralEmbeddingModel {
		t.Fatalf("default neuralModel = %q, want %q", def.neuralModel, DefaultNeuralEmbeddingModel)
	}
	if DefaultNeuralEmbeddingModel != "text-embedding-3-small" {
		t.Fatalf("DefaultNeuralEmbeddingModel = %q, want text-embedding-3-small", DefaultNeuralEmbeddingModel)
	}
	custom := NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{NeuralModel: "custom-model"})
	if custom.neuralModel != "custom-model" {
		t.Fatalf("explicit neuralModel = %q, want custom-model", custom.neuralModel)
	}
	if got := DefaultNeuralEmbeddingTimeout; got != 3*time.Second {
		t.Fatalf("DefaultNeuralEmbeddingTimeout = %v, want 3s", got)
	}
}

// TestNeuralEmbedderConfigSharedAcrossTiers proves one shared
// config feeds the memory and SQLite tiers: the SAME counting
// embedder is invoked through both, so every tier embeds the
// same way. The neural vector is also proved to survive a
// SQLite restart (inner store + Sync + reopen + lookup hits).
//
// KNOWN GAP (found by this test, production fix needed —
// tests-only lane, no production file touched): the persistent
// tiers' STORE path bypasses the embedder. PersistentSemantic-
// Cache.SetText* (semantic_persist.go:532/540/549) and
// RedisSemanticCache.SetText* (semantic_redis.go:534/542/551)
// all use p.inner.EmbedText(text) — plain bag-of-words —
// while every GetText* delegates to the inner cache's
// embedTextCtx (neural when configured). In serve mode with a
// persistent tier, stores write 256-dim BoW vectors but lookups
// embed 1536-dim neural vectors, and the length-mismatch rule
// means a neural lookup can NEVER hit an entry stored through
// the persistent tier's SetText path. The store half of the
// "one shared config feeds all three tiers" promise is
// unimplemented. TestPersistentSemanticCacheSetTextUsesNeural-
// Embedder below pins the intended contract as a SKIP until a
// fix builder changes those six methods to embedTextCtx.
// The Redis tier cannot be opened without a live Redis — its
// store-path bypass is verified by reading semantic_redis.go,
// not by a running test; the live-Redis neural wiring remains
// untested here (no fake Redis is used).
func TestNeuralEmbedderConfigSharedAcrossTiers(t *testing.T) {
	stub := &countingEmbedder{}
	cfg := LocalSemanticCacheConfig{
		Threshold:      DefaultSemanticThreshold,
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	}
	ctx := context.Background()

	// Memory tier: the shared config's embedder is invoked.
	mem := NewLocalSemanticCacheWithConfig(cfg)
	mem.SetTextForModel(ctx, "m", "alpha one", neuralTestEntry("mem-hit"), time.Hour)
	if got := stub.count(); got != 1 {
		t.Fatalf("memory tier SetTextForModel: embedder calls = %d, want 1", got)
	}
	if got, hit := mem.GetTextForModel(ctx, "m", "alpha one"); !hit || got.Response.Content != "mem-hit" {
		t.Fatalf("memory tier lookup: hit=%v content=%q, want hit with mem-hit", hit, got.Response.Content)
	}
	if got := stub.count(); got != 2 {
		t.Fatalf("memory tier GetTextForModel: embedder calls = %d, want 2", got)
	}

	// SQLite tier, LOOKUP half: the SAME config (same embedder
	// instance) feeds OpenPersistentSemanticCache; a neural
	// entry stored through the inner cache is served by the
	// AsStore adapter's lookup, which invokes the same stub.
	path := filepath.Join(t.TempDir(), "semantic.db")
	psc, err := OpenPersistentSemanticCache(path, cfg)
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	psc.Inner().SetTextForModel(ctx, "m", "alpha two", neuralTestEntry("sqlite-hit"), time.Hour)
	if got := stub.count(); got != 3 {
		t.Fatalf("sqlite tier inner SetTextForModel: embedder calls = %d, want 3 (same stub as memory tier)", got)
	}
	store := psc.AsStore()
	if got, hit := store.GetTextForModel(ctx, "m", "alpha two"); !hit || got.Response.Content != "sqlite-hit" {
		t.Fatalf("sqlite tier lookup: hit=%v content=%q, want hit with sqlite-hit", hit, got.Response.Content)
	}
	if got := stub.count(); got != 4 {
		t.Fatalf("sqlite tier GetTextForModel: embedder calls = %d, want 4", got)
	}
	// Persist the neural vector to disk, then reopen: the
	// neural vector survived the restart.
	if n, err := psc.Sync(ctx); err != nil || n < 1 {
		t.Fatalf("persistent cache Sync: n=%d err=%v, want >=1 nil", n, err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("persistent cache Close: %v", err)
	}
	psc2, err := OpenPersistentSemanticCache(path, cfg)
	if err != nil {
		t.Fatalf("reopen OpenPersistentSemanticCache: %v", err)
	}
	defer func() { _ = psc2.Close() }()
	if got, hit := psc2.AsStore().GetTextForModel(ctx, "m", "alpha two"); !hit || got.Response.Content != "sqlite-hit" {
		t.Fatalf("reopened sqlite tier lookup: hit=%v content=%q, want hit with sqlite-hit", hit, got.Response.Content)
	}
	if got := stub.count(); got != 5 {
		t.Fatalf("reopened sqlite tier GetTextForModel: embedder calls = %d, want 5", got)
	}
}

// TestPersistentSemanticCacheSetTextUsesNeuralEmbedder pins the
// intended store-path contract — SKIPPED until the production
// gap is fixed (see TestNeuralEmbedderConfigSharedAcrossTiers):
// PersistentSemanticCache.SetTextForModel must embed through the
// shared config's NeuralEmbedder, exactly like the memory tier,
// so an entry stored through the SQLite tier is retrievable by
// a neural lookup. Today it stores a BoW vector instead and the
// lookup below misses. A fix builder changes SetText* in
// semantic_persist.go (and the Redis twin) to embedTextCtx and
// un-skips this test.
func TestPersistentSemanticCacheSetTextUsesNeuralEmbedder(t *testing.T) {
	stub := &countingEmbedder{}
	cfg := LocalSemanticCacheConfig{
		NeuralEmbedder: stub.embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "semantic.db")
	psc, err := OpenPersistentSemanticCache(path, cfg)
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	defer func() { _ = psc.Close() }()
	store := psc.AsStore()
	store.SetTextForModel(ctx, "m", "alpha one", neuralTestEntry("sqlite-hit"), time.Hour)
	if got := stub.count(); got != 1 {
		t.Fatalf("sqlite tier SetTextForModel: embedder calls = %d, want 1", got)
	}
	if got, hit := store.GetTextForModel(ctx, "m", "alpha one"); !hit || got.Response.Content != "sqlite-hit" {
		t.Fatalf("sqlite tier lookup after store: hit=%v content=%q, want hit with sqlite-hit", hit, got.Response.Content)
	}
}

// TestRedisSemanticCacheUnreachableReturnsError pins the Redis
// tier's fail-open at open time: a Redis that cannot be reached
// is an error (the caller keeps serving memory-only), never a
// hang or a panic. It does NOT prove the neural config reaches
// the Redis tier — that needs a live Redis, which no test here
// has. See the file header.
func TestRedisSemanticCacheUnreachableReturnsError(t *testing.T) {
	cfg := LocalSemanticCacheConfig{
		NeuralEmbedder: (&countingEmbedder{}).embed,
		NeuralModel:    DefaultNeuralEmbeddingModel,
	}
	// Port 1 on loopback refuses immediately on Linux and
	// Windows; DialTimeout is 5 s but a refused dial returns
	// at once, so this must not hang.
	rc, err := OpenRedisSemanticCache(RedisOptions{Addr: "127.0.0.1:1"}, "", cfg)
	if err == nil {
		_ = rc.Close()
		t.Fatal("unreachable Redis: want error, got nil")
	}
	if rc != nil {
		_ = rc.Close()
		t.Fatal("unreachable Redis: want nil cache with the error, got non-nil")
	}
}
