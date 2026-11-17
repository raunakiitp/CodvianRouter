// Package cache implements the router's three-layer caching:
//
//  1. ExactCache — in-memory LRU cache with TTL. Key = SHA-256 of
//     model + messages JSON, so identical requests hit byte-for-byte.
//  2. Provider prompt cache — Prepare annotates an OpenAI-compatible
//     request with cache_control markers for providers that support
//     them (best-effort; a no-op for providers that don't).
//  3. Semantic cache — an interface plus two implementations:
//     NoopSemanticCache (inert) and LocalSemanticCache, a
//     deterministic hashed bag-of-words similarity cache. It is NOT
//     a neural embedding model: see the LocalSemanticCache docs for
//     the honest description of what it does and does not do.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Usage is the token usage of a cached response.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CachedResponse is what a cache layer stores and returns.
type CachedResponse struct {
	Content string `json:"content"`
	Usage   Usage  `json:"usage"`
	Model   string `json:"model"`
}

// Cache is the interface every cache layer satisfies.
type Cache interface {
	// Get returns the cached response and true on a hit, or the zero
	// value and false on a miss / expired entry.
	Get(ctx context.Context, key string) (CachedResponse, bool)
	// Set stores resp under key for ttl. A ttl <= 0 means "do not
	// store" (nothing is cached).
	Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration)
}

// ---------------------------------------------------------------------------
// Layer 1: exact-match cache
// ---------------------------------------------------------------------------

// DefaultMaxEntries bounds the ExactCache when the caller passes <= 0.
const DefaultMaxEntries = 1000

type exactEntry struct {
	resp    CachedResponse
	expires time.Time
}

// ExactCache is a concurrency-safe in-memory cache with per-entry TTL
// and a bounded size. Eviction is LRU-ish: the least recently used
// entry is dropped when the cache is full. Expired entries are removed
// lazily on access; the size cap bounds total entries either way.
type ExactCache struct {
	mu         sync.Mutex
	maxEntries int
	items      map[string]*exactEntry
	// order tracks recency: most recently used at the front.
	// Kept as a slice of keys for simplicity; the cache is bounded
	// (default 1000 entries) so linear scans stay cheap.
	order []string
}

// NewExactCache returns an ExactCache holding at most maxEntries
// entries (DefaultMaxEntries when maxEntries <= 0).
func NewExactCache(maxEntries int) *ExactCache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &ExactCache{
		maxEntries: maxEntries,
		items:      make(map[string]*exactEntry, maxEntries),
		order:      make([]string, 0, maxEntries),
	}
}

// touch moves key to the front of the recency order. Caller holds mu.
func (c *ExactCache) touch(key string) {
	for i, k := range c.order {
		if k == key {
			copy(c.order[i:], c.order[i+1:])
			c.order = c.order[:len(c.order)-1]
			break
		}
	}
	c.order = append([]string{key}, c.order...)
}

// evict drops the least recently used entry. Caller holds mu.
func (c *ExactCache) evict() {
	if len(c.order) == 0 {
		return
	}
	last := c.order[len(c.order)-1]
	c.order = c.order[:len(c.order)-1]
	delete(c.items, last)
}

// Get returns a cached response on a hit. Expired entries are deleted
// and reported as misses. A hit refreshes the entry's recency.
func (c *ExactCache) Get(ctx context.Context, key string) (CachedResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return CachedResponse{}, false
	}
	if time.Now().After(e.expires) {
		delete(c.items, key)
		for i, k := range c.order {
			if k == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
		return CachedResponse{}, false
	}
	c.touch(key)
	return e.resp, true
}

// Set stores resp under key for ttl. ttl <= 0 stores nothing.
func (c *ExactCache) Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		for len(c.items) >= c.maxEntries {
			c.evict()
		}
	}
	c.items[key] = &exactEntry{resp: resp, expires: time.Now().Add(ttl)}
	c.touch(key)
}

// Delete removes key if present.
func (c *ExactCache) Delete(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; !ok {
		return
	}
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// Size returns the number of entries currently stored (including any
// not-yet-expired entries that have passed their TTL but not been
// accessed since).
func (c *ExactCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// ExactKey derives the layer-1 cache key for a request: SHA-256 of the
// JSON encoding of {model, messages}. Identical model + messages
// always produce the same key; any difference (including message order
// or whitespace) produces a different key. Streaming flags, temperature
// and other sampling parameters are intentionally NOT part of the key:
// the cached content is the same either way, and the caller decides
// whether a cached answer is acceptable for a given request.
func ExactKey(model string, messages any) (string, error) {
	payload := struct {
		Model    string `json:"model"`
		Messages any    `json:"messages"`
	}{Model: model, Messages: messages}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ---------------------------------------------------------------------------
// Layer 2: provider prompt-cache support
// ---------------------------------------------------------------------------

// CacheControl is the cache_control block some providers accept on a
// message to pin its prefix in their prompt cache.
type CacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// Message is one chat message in an OpenAI-compatible request. Only the
// string-content form is modelled here; content-block (array) messages
// are out of scope for prompt-cache annotation.
type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ChatRequest is the minimal OpenAI-compatible chat-completions request
// shape the router annotates for prompt caching.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream,omitempty"`
}

// promptCacheProviders lists the providers (by the router's provider
// name, lower-cased) known to honour cache_control on messages.
// Anthropic pioneered the marker; OpenRouter passes it through to the
// underlying provider. Anything not listed is a no-op.
var promptCacheProviders = map[string]bool{
	"anthropic":  true,
	"claude":     true,
	"openrouter": true,
}

// Prepare annotates req for provider-side prompt caching: it sets
// cache_control {"type": "ephemeral"} on the LAST user message, which
// marks everything up to and including that message as a reusable
// prefix. Best-effort and idempotent.
//
//   - No-op when provider is not in promptCacheProviders (OpenAI runs
//     automatic prefix caching server-side and ignores cache_control
//     on messages; Google Gemini uses a separate cachedContents API,
//     so a message marker would be meaningless there).
//   - No-op when req is nil or has no user message.
//   - Only string-content messages are annotated.
func Prepare(req *ChatRequest, provider string) {
	if req == nil || !promptCacheProviders[normalizeProvider(provider)] {
		return
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		req.Messages[i].CacheControl = &CacheControl{Type: "ephemeral"}
		return
	}
}

func normalizeProvider(p string) string {
	// Lower-case and trim; provider names are matched exactly otherwise.
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == ' ' || c == '\t' || c == '\n' {
			continue
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}

// ---------------------------------------------------------------------------
// Layer 3: semantic cache
// ---------------------------------------------------------------------------
//
// HONEST DESCRIPTION — read before calling this "semantic" or
// "embedding-based": LocalSemanticCache does NOT use a neural embedding
// model and does NOT understand meaning. It is a deterministic hashed
// bag-of-words similarity cache: it tokenizes text, hashes token
// unigrams and bigrams into a fixed 256-dimensional float32 vector,
// and compares vectors with cosine similarity. Two texts "match" when
// they share enough of the same words in the same order — token-overlap
// similarity, nothing more. Paraphrases that use different words will
// miss; texts that share words but mean different things can hit.
// That is a deliberate trade-off: it works fully offline, with no
// network call, no external model, and negligible CPU/RAM, so it runs
// on a low-end PC. A future neural-embedding backend (provider API or
// a local model) would replace the EmbedText vector builder and the
// SemanticCache plumbing stays the same; until then, do not describe
// this layer as embeddings or semantic understanding.

// SemanticEntry is a cached response found by token-overlap similarity
// rather than by exact match. Similarity is the cosine similarity
// (0..1) between the query vector and the stored vector.
type SemanticEntry struct {
	Response   CachedResponse
	Similarity float32
}

// SemanticCache finds cached responses by vector similarity. Callers
// pass a vector for the incoming request and receive the closest
// stored response when it is similar enough.
type SemanticCache interface {
	Get(ctx context.Context, embedding []float32) (SemanticEntry, bool)
	Set(ctx context.Context, embedding []float32, entry SemanticEntry)
}

// NoopSemanticCache is a SemanticCache that never hits and stores
// nothing. Use it when the caller wants the semantic layer inert.
type NoopSemanticCache struct{}

// Get always reports a miss.
func (NoopSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	return SemanticEntry{}, false
}

// Set discards the entry.
func (NoopSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) {
}

// ---------------------------------------------------------------------------
// LocalSemanticCache: deterministic hashed bag-of-words similarity
// ---------------------------------------------------------------------------

// DefaultSemanticThreshold is the cosine similarity a stored entry
// must reach to count as a hit (0.85).
const DefaultSemanticThreshold = 0.85

// DefaultSemanticMaxEntries bounds a LocalSemanticCache when the
// caller passes <= 0.
const DefaultSemanticMaxEntries = 500

// DefaultSemanticTTL is the lifetime of entries stored through the
// SemanticCache interface (which carries no TTL of its own). SetText
// takes an explicit TTL instead.
const DefaultSemanticTTL = time.Hour

// SemanticVectorDims is the fixed dimensionality of the hashed
// bag-of-words vectors built by EmbedText.
const SemanticVectorDims = 256

// LocalSemanticCacheConfig tunes a LocalSemanticCache. Zero values
// select the defaults (threshold 0.85, 500 entries, 1h TTL).
type LocalSemanticCacheConfig struct {
	// Threshold is the minimum cosine similarity for a hit.
	// 0 selects DefaultSemanticThreshold.
	Threshold float32
	// MaxEntries caps stored entries; the least recently used entry
	// is evicted when the cap is reached. <= 0 selects
	// DefaultSemanticMaxEntries.
	MaxEntries int
	// TTL is the lifetime of entries stored via the SemanticCache
	// interface Set (which takes no TTL). <= 0 selects
	// DefaultSemanticTTL.
	TTL time.Duration
}

// localSemanticEntry is one stored vector plus its response.
type localSemanticEntry struct {
	vec     []float32 // L2-normalized, SemanticVectorDims long
	entry   SemanticEntry
	expires time.Time
}

// LocalSemanticCache is a concurrency-safe, offline, stdlib-only
// similarity cache. It stores fixed 256-dimensional hashed
// bag-of-words vectors (see EmbedText) and serves the stored response
// whose vector is closest to the query vector, provided the cosine
// similarity is at least the configured threshold. Entries carry a
// per-entry TTL and the cache evicts least-recently-used entries when
// full.
//
// This is NOT a neural embedding model and does NOT understand
// meaning: similarity here is token overlap (shared words and word
// pairs), so synonyms and paraphrases generally miss while
// word-sharing but semantically different texts can hit. It is a
// cheap local approximation, suitable as a first-pass cache on a
// low-end PC — not a replacement for real embeddings. A future
// neural-embedding backend would replace EmbedText (the vector
// builder) while Get/Set and the SemanticCache interface stay as-is.
//
// It is wired into the request path through degrade.StaleEngine
// (WrapWithSemantic + Policy.ServeCache): cacheable requests are
// served proactively from exact first, then this layer, and
// successes are stored here under the last user message's text.
type LocalSemanticCache struct {
	mu         sync.Mutex
	threshold  float32
	maxEntries int
	ttl        time.Duration
	items      map[int]*localSemanticEntry
	// order tracks recency: most recently used first.
	order  []int
	nextID int
}

// NewLocalSemanticCache returns a LocalSemanticCache with the
// defaults: threshold 0.85, 500 entries, 1h TTL for interface Set.
func NewLocalSemanticCache() *LocalSemanticCache {
	return NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{})
}

// NewLocalSemanticCacheWithConfig returns a LocalSemanticCache tuned
// by cfg; zero fields select the defaults.
func NewLocalSemanticCacheWithConfig(cfg LocalSemanticCacheConfig) *LocalSemanticCache {
	threshold := cfg.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultSemanticThreshold
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultSemanticMaxEntries
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultSemanticTTL
	}
	return &LocalSemanticCache{
		threshold:  threshold,
		maxEntries: maxEntries,
		ttl:        ttl,
		items:      make(map[int]*localSemanticEntry, maxEntries),
		order:      make([]int, 0, maxEntries),
	}
}

// EmbedText returns the deterministic hashed bag-of-words vector for
// text: 256 float32s, L2-normalized, built from token unigrams +
// bigrams hashed into buckets. Identical text yields the identical
// vector. This is a token-overlap feature vector, NOT a neural
// embedding — it carries no meaning beyond which words (and word
// pairs) the text contains. Exported so other local packages (e.g.
// the auto-router) can reuse exactly this embedding approach instead
// of building a second one.
func EmbedText(text string) []float32 {
	return embedText(text)
}

// Cosine returns the cosine similarity of a and b, computed over the
// shorter of the two. Zero vectors (or empty inputs) score 0.
// Exported alongside EmbedText for the same reuse reason.
func Cosine(a, b []float32) float32 {
	return cosine(a, b)
}

// tokenize lower-cases text and splits on anything that is not a
// letter or digit, dropping empties.
func tokenize(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// hashToken maps a token to a vector bucket with FNV-1a.
func hashToken(token string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(token))
	return int(h.Sum64() % SemanticVectorDims)
}

// embedText builds the deterministic hashed bag-of-words vector for
// text: token unigrams plus consecutive-token bigrams, hashed into
// 256 buckets, count-weighted, then L2-normalized. Identical text
// always yields the identical vector; an empty (or token-less) text
// yields the zero vector. This is a token-overlap feature vector, NOT
// a neural embedding.
func embedText(text string) []float32 {
	vec := make([]float32, SemanticVectorDims)
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return vec
	}
	for _, tok := range tokens {
		vec[hashToken(tok)]++
	}
	for i := 0; i+1 < len(tokens); i++ {
		vec[hashToken(tokens[i]+"\x00"+tokens[i+1])]++
	}
	// L2-normalize.
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return vec
	}
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// EmbedText returns the deterministic hashed bag-of-words vector for
// text: 256 float32s, L2-normalized, built from token unigrams +
// bigrams hashed into buckets. Identical text yields the identical
// vector. This is a token-overlap feature vector, NOT a neural
// embedding — it carries no meaning beyond which words (and word
// pairs) the text contains.
func (c *LocalSemanticCache) EmbedText(text string) []float32 {
	return embedText(text)
}

// cosine returns the cosine similarity of a and b, computed over the
// shorter of the two. Zero vectors (or empty inputs) score 0.
func cosine(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if sim > 1 {
		sim = 1
	}
	if sim < -1 {
		sim = -1
	}
	return float32(sim)
}

func isZeroVector(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

// touch moves id to the front of the recency order. Caller holds mu.
func (c *LocalSemanticCache) touch(id int) {
	for i, k := range c.order {
		if k == id {
			copy(c.order[i:], c.order[i+1:])
			c.order = c.order[:len(c.order)-1]
			break
		}
	}
	c.order = append([]int{id}, c.order...)
}

// remove deletes id from items and the recency order. Caller holds mu.
func (c *LocalSemanticCache) remove(id int) {
	delete(c.items, id)
	for i, k := range c.order {
		if k == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// evict drops the least recently used entry. Caller holds mu.
func (c *LocalSemanticCache) evict() {
	if len(c.order) == 0 {
		return
	}
	c.remove(c.order[len(c.order)-1])
}

// Get returns the stored response whose vector is most similar to
// embedding, when that similarity is at least the configured
// threshold. Expired entries are deleted and reported as misses. A
// hit refreshes the entry's recency and reports the actual
// similarity in the returned entry. Zero or empty embeddings always
// miss.
func (c *LocalSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	if len(embedding) == 0 || isZeroVector(embedding) {
		return SemanticEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	bestID := -1
	var best float32
	for id, e := range c.items {
		if now.After(e.expires) {
			c.remove(id)
			continue
		}
		if sim := cosine(embedding, e.vec); sim > best {
			best, bestID = sim, id
		}
	}
	if bestID < 0 || best < c.threshold {
		return SemanticEntry{}, false
	}
	c.touch(bestID)
	hit := c.items[bestID].entry
	hit.Similarity = best
	return hit, true
}

// Set stores entry under embedding for the cache's default TTL. A
// zero or empty embedding stores nothing. Storing a vector that is
// essentially identical (cosine >= 0.9999) to an existing entry
// replaces that entry in place instead of duplicating it. When the
// cache is full, the least recently used entry is evicted.
func (c *LocalSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) {
	c.set(embedding, entry, c.ttl)
}

// set is the shared store path for Set and SetText. Caller passes
// the TTL; ttl <= 0 stores nothing.
func (c *LocalSemanticCache) set(embedding []float32, entry SemanticEntry, ttl time.Duration) {
	if ttl <= 0 || len(embedding) == 0 || isZeroVector(embedding) {
		return
	}
	vec := make([]float32, len(embedding))
	copy(vec, embedding)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Replace a near-identical vector in place instead of duplicating.
	for id, e := range c.items {
		if now.After(e.expires) {
			c.remove(id)
			continue
		}
		if cosine(vec, e.vec) >= 0.9999 {
			e.vec = vec
			e.entry = entry
			e.expires = now.Add(ttl)
			c.touch(id)
			return
		}
	}
	for len(c.items) >= c.maxEntries {
		c.evict()
	}
	id := c.nextID
	c.nextID++
	c.items[id] = &localSemanticEntry{vec: vec, entry: entry, expires: now.Add(ttl)}
	c.touch(id)
}

// GetText returns the cached response for text most similar to the
// given text, when the token-overlap cosine similarity is at least
// the configured threshold. It is Get over the vector EmbedText
// builds for text.
func (c *LocalSemanticCache) GetText(ctx context.Context, text string) (SemanticEntry, bool) {
	return c.Get(ctx, c.EmbedText(text))
}

// SetText stores entry for text for ttl, keyed by the hashed
// bag-of-words vector EmbedText builds for text. ttl <= 0 stores
// nothing; token-less text stores nothing.
func (c *LocalSemanticCache) SetText(ctx context.Context, text string, entry SemanticEntry, ttl time.Duration) {
	c.set(c.EmbedText(text), entry, ttl)
}

// Size returns the number of entries currently stored (including any
// that have passed their TTL but not been accessed since).
func (c *LocalSemanticCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}
