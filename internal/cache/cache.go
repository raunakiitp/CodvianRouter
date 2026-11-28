// Package cache implements the router's three-layer caching:
//
//  1. ExactCache — in-memory LRU cache with TTL. Key = SHA-256 of
//     model + messages JSON, so identical requests hit byte-for-byte.
//  2. Provider prompt cache — Prepare annotates an OpenAI-compatible
//     request with cache_control markers for providers that support
//     them (best-effort; a no-op for providers that don't).
//  3. Semantic cache — an interface plus two implementations:
//     NoopSemanticCache (inert) and LocalSemanticCache. By default
//     LocalSemanticCache is a deterministic hashed bag-of-words
//     similarity cache — NOT a neural embedding model — but when a
//     NeuralEmbedder is configured it uses real provider embeddings
//     (text-embedding-3-small, 1536-dim, 3 s timeout, fail-open to
//     bag-of-words). See the LocalSemanticCache docs for the honest
//     description of what it does and does not do.
//
// MODEL SCOPING (CHECKLIST 187) — read before serving from any
// layer: every cached response is scoped by the model id the
// request named. The exact-match key already includes the model
// (ExactKey), and the semantic layer records the requesting model
// on each entry and serves it only to a lookup naming that same
// model (exact, case-sensitive string match). A cached
// claude-sonnet response is therefore never served for a gpt-4o
// request, however similar the text — the 163-164 smoke caught
// exactly that cross-model hit, because the semantic vector is
// text-only. The provider is deliberately NOT part of the scope:
// the same model id served by two different providers (gpt-4o via
// OpenAI and via OpenRouter) shares entries, because an entry
// records the model that produced it, not the pipe it came
// through. Entries stored without a recorded model (empty
// Response.Model — only the raw Set/SetText API can produce them)
// are unattributed and are never served to a request naming a
// model; only the raw model-less Get/GetText can read them back.
// Serving code must use GetForModel/GetTextForModel (and store
// with SetTextForModel naming the request model), never the raw
// Get/GetText.
//
// TENANT + TEMPERATURE SCOPING (M6.11) — the semantic layer
// records, per entry, the tenant (gateway key id, "" for
// anonymous — a real tenant, never a shared bucket) and the
// temperature bucket (cache.TemperatureBucket, the same 2-decimal
// bucketing the exact layer uses) — but ONLY for entries stored
// through the Scoped APIs (GetTextForModelScopedWithThreshold /
// SetTextForModelScoped, and the vector-level SetForModelScoped /
// GetForModelScopedWithThreshold). A scoped lookup serves an
// entry only when tenant AND temperature bucket both match, so a
// cross-tenant paraphrase and a cross-temperature paraphrase
// both miss. Entries stored through the legacy (unscoped) APIs
// record no tenant/temperature (scoped=false) and are NEVER
// served by a scoped lookup — they fail closed. The legacy
// lookup APIs themselves remain unscoped by design (they are the
// raw/diagnostic path pinned by
// TestM611SemanticLayerIsTenantBlind): they serve any entry
// naming the model, scoped or not. Serving code must use the
// Scoped APIs, never the legacy ones.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Usage is the token usage of a cached response.
//
// CachedTokens / ReasoningTokens were added for the Phase 2 carry-over
// "cache replay usage-detail preservation": a replayed response must
// not silently drop the detail splits the original report carried
// (prompt_tokens_details.cached_tokens,
// completion_tokens_details.reasoning_tokens). Both are omitempty, so
// every pre-existing serialized entry (memory, SQLite, Redis) stays
// byte-compatible and reads back as zero when absent. Negative
// values never store: they are malformed upstream output and clamp to
// zero at the store site.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens,omitempty"`
	ReasoningTokens  int `json:"reasoning_tokens,omitempty"`
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

// DefaultMaxEntryBytes is the per-entry size cap (M6.1): an entry
// whose response content exceeds 256 KiB is never stored —
// the sector-typical per-entry cap (semanticCache.ts:110). DefaultMaxTotalBytes
// is the total memory cap for the layer: 2 MiB (the sector-typical memory
// tier, semanticCache.ts:127). Entry size is measured as the stored
// response content in bytes — the key is a fixed 64-char hex string
// and struct overhead is not counted, so the measure is exact and
// auditable, not an estimate.
const DefaultMaxEntryBytes = 256 << 10
const DefaultMaxTotalBytes = 2 << 20

// exactEntrySize is the accounted size of one stored response.
func exactEntrySize(resp CachedResponse) int {
	return len(resp.Content)
}

type exactEntry struct {
	resp    CachedResponse
	expires time.Time
}

// ExactCache is a concurrency-safe in-memory cache with per-entry TTL
// and a bounded size. Eviction is LRU-ish: the least recently used
// entry is dropped when the cache is full. Expired entries are removed
// lazily on access; the size cap bounds total entries either way.
// M6.1 adds byte caps alongside the entry-count cap: a single entry
// over DefaultMaxEntryBytes is refused outright, and LRU eviction
// also runs while the layer's total bytes would exceed
// DefaultMaxTotalBytes.
type ExactCache struct {
	mu         sync.Mutex
	maxEntries int
	// maxEntryBytes refuses a single entry larger than this;
	// maxTotalBytes bounds the sum of all stored entries' sizes.
	// totalBytes is that sum, maintained under mu on every
	// insert / evict / expiry / delete / flush.
	maxEntryBytes int
	maxTotalBytes int
	totalBytes    int
	items         map[string]*exactEntry
	// order tracks recency: most recently used at the front.
	// Kept as a slice of keys for simplicity; the cache is bounded
	// (default 1000 entries) so linear scans stay cheap.
	order []string
}

// NewExactCache returns an ExactCache holding at most maxEntries
// entries (DefaultMaxEntries when maxEntries <= 0), with the
// default byte caps (256 KiB per entry, 2 MiB total).
func NewExactCache(maxEntries int) *ExactCache {
	return NewExactCacheWithCaps(maxEntries, DefaultMaxEntryBytes, DefaultMaxTotalBytes)
}

// NewExactCacheWithCaps returns an ExactCache with explicit caps;
// a cap <= 0 selects its default.
func NewExactCacheWithCaps(maxEntries, maxEntryBytes, maxTotalBytes int) *ExactCache {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxEntryBytes <= 0 {
		maxEntryBytes = DefaultMaxEntryBytes
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalBytes
	}
	return &ExactCache{
		maxEntries:    maxEntries,
		maxEntryBytes: maxEntryBytes,
		maxTotalBytes: maxTotalBytes,
		items:         make(map[string]*exactEntry, maxEntries),
		order:         make([]string, 0, maxEntries),
	}
}

// touch moves key to the front of the recency order. Caller holds mu.
// It shifts in place (copy) instead of allocating a fresh slice per
// touch as the old implementation did (append([]string{key},
// c.order...) — one allocation per cache Set/Get-hit; the memstats
// slice (CHECKLIST 235-236) measured touch at ~71 MB churn per
// ~24k requests, and the CPU profile (CHECKLIST 249-250) put it at
// 0.46 s cum). When key is already front, or absent (Set of a new
// key), the append below stays inside the preallocated capacity
// (NewExactCache caps order at maxEntries and Set evicts first),
// so the common paths allocate nothing.
func (c *ExactCache) touch(key string) {
	for i, k := range c.order {
		if k == key {
			copy(c.order[1:i+1], c.order[:i])
			c.order[0] = key
			return
		}
	}
	c.order = append(c.order, key)
	copy(c.order[1:], c.order[:len(c.order)-1])
	c.order[0] = key
}

// evict drops the least recently used entry. Caller holds mu.
// The evicted entry's bytes leave totalBytes with it.
func (c *ExactCache) evict() {
	if len(c.order) == 0 {
		return
	}
	last := c.order[len(c.order)-1]
	c.order = c.order[:len(c.order)-1]
	if e, ok := c.items[last]; ok {
		c.totalBytes -= exactEntrySize(e.resp)
	}
	delete(c.items, last)
}

// evictExcept drops the least recently used entry other than
// except. Caller holds mu. It returns false when no other entry
// exists to evict.
func (c *ExactCache) evictExcept(except string) bool {
	for i := len(c.order) - 1; i >= 0; i-- {
		if c.order[i] == except {
			continue
		}
		victim := c.order[i]
		c.order = append(c.order[:i], c.order[i+1:]...)
		if e, ok := c.items[victim]; ok {
			c.totalBytes -= exactEntrySize(e.resp)
		}
		delete(c.items, victim)
		return true
	}
	return false
}

// removeLocked deletes key's entry and its recency position,
// accounting its bytes out. Caller holds mu.
func (c *ExactCache) removeLocked(key string) {
	e, ok := c.items[key]
	if !ok {
		return
	}
	c.totalBytes -= exactEntrySize(e.resp)
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
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
		c.removeLocked(key)
		return CachedResponse{}, false
	}
	c.touch(key)
	return e.resp, true
}

// Set stores resp under key for ttl. ttl <= 0 stores nothing. An
// entry over the per-entry byte cap stores nothing. A replacement
// frees the old entry's bytes first; LRU eviction runs while the
// entry count OR the total bytes would exceed the caps.
func (c *ExactCache) Set(ctx context.Context, key string, resp CachedResponse, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	size := exactEntrySize(resp)
	if size > c.maxEntryBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, exists := c.items[key]
	if exists {
		c.totalBytes -= exactEntrySize(c.items[key].resp)
	}
	// Evict while the entry count (new keys only — a replacement
	// frees its own slot) OR the total bytes would exceed the
	// caps. Never evict the key being set: its old bytes are
	// already accounted out above, and deleting it here would
	// drop the entry the caller is storing.
	for (!exists && len(c.items) >= c.maxEntries) || c.totalBytes+size > c.maxTotalBytes {
		if !c.evictExcept(key) {
			break
		}
	}
	c.items[key] = &exactEntry{resp: resp, expires: time.Now().Add(ttl)}
	c.totalBytes += size
	c.touch(key)
}

// Delete removes key if present.
func (c *ExactCache) Delete(ctx context.Context, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

// FlushAll removes every entry and returns how many were removed
// (M1.5: backs POST /admin/cache/flush for the in-memory layer).
func (c *ExactCache) FlushAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.items)
	c.items = make(map[string]*exactEntry, c.maxEntries)
	c.order = c.order[:0]
	c.totalBytes = 0
	return n
}

// SweepExpired removes every expired entry and returns how many were
// removed. Expired entries are otherwise removed lazily on access,
// so Size/Stats can count tombstoned rows until a sweep runs.
func (c *ExactCache) SweepExpired(ctx context.Context) int {
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var expired []string
	for key, e := range c.items {
		if now.After(e.expires) {
			expired = append(expired, key)
		}
	}
	for _, key := range expired {
		c.removeLocked(key)
	}
	return len(expired)
}

// DeleteExactForModel removes every exact-cache entry whose cached
// response was produced for model and returns the keys removed. It
// never touches another model's entries (A13 H6 per-model
// invalidation). Entries stored without a recorded model (empty
// resp.Model) match only model "".
func (c *ExactCache) DeleteExactForModel(ctx context.Context, model string) []string {
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	var removed []string
	for key, e := range c.items {
		if e.resp.Model == model {
			removed = append(removed, key)
		}
	}
	for _, key := range removed {
		c.removeLocked(key)
	}
	return removed
}

// Stats returns the live entry count and total accounted bytes
// (M6.8: backs the cache stats surface). Expired-but-unaccessed
// entries still count — they are removed lazily on access, same as
// Size documents.
func (c *ExactCache) Stats() (entries, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.totalBytes
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
// name, lower-cased) that cache.Prepare actually annotates in
// production — the canonical marker set. Anthropic pioneered the
// marker and is the only provider this router carries live through
// cache.Prepare: the Anthropic provider client
// (internal/provider/anthropic_client.go applyPromptCacheMarker, M6.9c)
// routes providerID anthropic / claude through this function, so
// cache.Prepare is the single live marker path for this set.
//
// openrouter is deliberately NOT in this set: its OpenAI-wire
// markers are applied separately by
// internal/provider/client.go (applyOpenRouterPromptCacheMarker,
// g13) and are governed by that client's own model-family gate, not
// by cache.Prepare. Listing openrouter here while cache.Prepare
// never carries it would be half state.
//
// Anything not listed is a no-op.
//
// M6.9 audit (2026-11-18) — zero providers added, deliberately:
//   - Bedrock: ... (same as before, not added until a carry path exists)
//   - Vertex: ... (same)
//   - OpenAI: ... (no marker to set)
//   - DeepSeek: ... (no cache_control parameter)
//   - Gemini: ... (separate API)
//   - Cohere: ... (not added on speculation)
//   - Other OpenAI-compatible hosts (Fireworks, Together, Groq,
//     xAI, ...): no Anthropic-style marker honoured — not added.
//
// The strategy mirror (internal/strategy/cache_affinity.go
// promptCacheMarkerProviders) must stay in lockstep with this map.
var promptCacheProviders = map[string]bool{
	"anthropic": true,
	"claude":    true,
}

// SupportsPromptCacheMarker reports whether provider is known to
// honour a prompt-cache marker on messages (the promptCacheProviders
// set above). It is the canonical accessor for the map: callers
// that need the bool (the router's compression call site, G22/B10)
// use this instead of copying the table — a third copy would drift.
// The strategy mirror (internal/strategy/cache_affinity.go
// promptCacheMarkerProviders) must stay in lockstep with this map;
// this accessor reads the map itself, so it cannot drift. The
// router cannot import internal/strategy (strategy imports router),
// which is why the accessor lives here and not there.
func SupportsPromptCacheMarker(provider string) bool {
	return promptCacheProviders[normalizeProvider(provider)]
}

// Prepare annotates req for provider-side prompt caching: it sets
// cache_control {"type": "ephemeral"} on the LAST user message, which
// marks everything up to and including that message as a reusable
// prefix. Best-effort and idempotent.
//
//   - No-op when provider is not in promptCacheProviders (OpenAI runs
//     automatic prefix caching server-side and ignores cache_control
//     on messages; Google Gemini uses a separate cachedContents API,
//     so a message marker would be meaningless there; Bedrock and
//     Vertex honour the marker only in translated form — Converse
//     cachePoint / Vertex Claude rawPredict — which this router does
//     not yet carry, so they stay no-ops here until that wiring
//     lands, see the promptCacheProviders comment).
//   - No-op when req is nil or has no user message.
//   - Only string-content messages are annotated.
//
// NOTE: Prepare() has a production caller — the Anthropic provider
// client (internal/provider/anthropic_client.go
// applyPromptCacheMarker, M6.9c) routes native Anthropic hosts
// (providerID anthropic / claude) through it, so cache.Prepare is
// the single live marker path for the canonical set above.
// openrouter is deliberately not carried by cache.Prepare; its
// OpenAI-wire markers are applied by internal/provider/client.go
// (applyOpenRouterPromptCacheMarker) instead. Do not add a provider
// to this map without a proven carry path through cache.Prepare.
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
// on a low-end PC. A neural-embedding backend is available as an
// opt-in: set LocalSemanticCacheConfig.NeuralEmbedder to a provider
// embedding call (text-embedding-3-small, 1536-dim, 3 s timeout,
// fail-open to the hashed vector) and the text APIs embed through
// it; the SemanticCache plumbing stays the same. Until one is
// configured, do not describe this layer as embeddings or semantic
// understanding.

// SemanticEntry is a cached response found by token-overlap similarity
// rather than by exact match. Similarity is the cosine similarity
// (0..1) between the query vector and the stored vector.
type SemanticEntry struct {
	Response   CachedResponse
	Similarity float32
	// VectorSource names the vector space the hit was matched in:
	// VectorSourceNeural ("neural") when the query was embedded by the
	// configured neural embedding provider, VectorSourceBagOfWords
	// ("bag-of-words") when it fell back to the hashed token-overlap
	// vector, and "" when the entry was stored through the raw vector
	// API (Set/SetForModel) and the caller never said. It is stamped by
	// the text lookups (GetText*) and is what the serving path reports
	// as the X-Codvian-Cache-Embedding header value, so an operator can
	// see which space produced a served hit.
	VectorSource string `json:"vector_source,omitempty"`
}

// SemanticCache finds cached responses by vector similarity. Callers
// pass a vector for the incoming request and receive the closest
// stored response when it is similar enough.
//
// This is the RAW vector API: it carries no model, so Get serves
// only entries stored without a recorded model (see the package
// doc). It is retained for the interface contract and for
// non-serving callers (tests, benchmarks). Anything that serves a
// cached response to a client MUST use LocalSemanticCache's
// GetForModel / GetTextForModel instead, so a cross-model hit can
// never happen (CHECKLIST 187).
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
// must reach to count as a hit (0.85). The floor is justified by a
// measured recall/precision probe over a labelled paraphrase set —
// see semantic_floor_probe_test.go: at 0.85 the probe keeps its recall
// contract while every lower floor admits more same-words-different-
// meaning impostors, and every higher floor loses true paraphrases.
const DefaultSemanticThreshold = 0.85

// VectorSourceNeural and VectorSourceBagOfWords are the two vector
// spaces the semantic text APIs can embed a query in. They are the
// values SemanticEntry.VectorSource carries on a hit and the label
// values of the semantic-hit-by-vector-source counter.
const (
	VectorSourceNeural     = "neural"
	VectorSourceBagOfWords = "bag-of-words"
)

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

// NeuralEmbedder embeds text into a dense vector using a neural
// embedding model served by a provider (the sector baseline semanticCache.ts:
// text-embedding-3-small, 1536-dim, 3 s timeout, fail-open). It is
// implemented by the caller — the router's provider client — and
// injected through LocalSemanticCacheConfig.NeuralEmbedder. A nil
// NeuralEmbedder means neural embeddings are off and every text API
// uses the hashed bag-of-words vector (EmbedText) exactly as before.
// The embedder itself decides which provider model to call; the
// cache only bounds the call (NeuralTimeout) and falls back on any
// failure, so a dead or slow embedding provider can never break
// serving. Wiring a real provider client into this hook is a
// follow-up in the router layer (main.go / degrade) — this file
// only defines the seam.
type NeuralEmbedder func(ctx context.Context, text string) ([]float32, error)

// DefaultNeuralEmbeddingModel is the provider embedding model used
// when neural embeddings are enabled and no model is configured
// (the sector baseline semanticCache.ts: text-embedding-3-small).
const DefaultNeuralEmbeddingModel = "text-embedding-3-small"

// DefaultNeuralEmbeddingTimeout bounds one provider embedding call
// (the sector baseline semanticCache.ts: 3 s). A call that exceeds it fails
// open to the hashed bag-of-words vector.
const DefaultNeuralEmbeddingTimeout = 3 * time.Second

// NeuralEmbeddingDims is the dimensionality of
// DefaultNeuralEmbeddingModel's vectors (1536). Neural vectors are
// stored as wide entries (see localSemanticEntry.wide) and scored by
// the legacy full scan, exactly like any other >256-dim vector.
const NeuralEmbeddingDims = 1536

// LocalSemanticCacheConfig tunes a LocalSemanticCache. Zero values
// select the defaults (threshold 0.85, 500 entries, 1h TTL).
type LocalSemanticCacheConfig struct {
	// Threshold is the minimum cosine similarity for a hit.
	// 0 selects DefaultSemanticThreshold; a value below the
	// 0.85 floor is raised to it (M6.7).
	Threshold float32
	// MaxEntries caps stored entries; the least recently used entry
	// is evicted when the cap is reached. <= 0 selects
	// DefaultSemanticMaxEntries.
	MaxEntries int
	// TTL is the lifetime of entries stored via the SemanticCache
	// interface Set (which takes no TTL). <= 0 selects
	// DefaultSemanticTTL.
	TTL time.Duration
	// MaxEntryBytes refuses a single entry larger than this
	// (M6.1); MaxTotalBytes bounds the sum of all stored entries'
	// sizes. <= 0 selects DefaultMaxEntryBytes /
	// DefaultMaxTotalBytes. Entry size is the stored vector's
	// bytes (4 per float32) plus the response content's bytes —
	// both exact, auditable measures.
	MaxEntryBytes int
	MaxTotalBytes int
	// NeuralEmbedder, when non-nil, supplies real neural embeddings
	// for the text APIs (GetText*, SetText*): the semantic layer
	// then compares meaning, not token overlap. The provider call
	// is bounded by NeuralTimeout (default 3 s) and fails open to
	// the hashed bag-of-words vector on any error, timeout, or
	// malformed vector. NeuralModel names the embedding model
	// (default text-embedding-3-small); it is informational here —
	// the embedder itself decides which model to call — but it is
	// recorded so diagnostics can tell which model produced a
	// vector.
	NeuralEmbedder NeuralEmbedder
	NeuralModel    string
	NeuralTimeout  time.Duration
}

// posting is one entry of the inverted index: slot holds the
// entry, val is that entry's vector value at the indexed dimension,
// gen is the slot generation the posting was written with (see
// LocalSemanticCache).
type posting struct {
	slot int32
	val  float32
	gen  uint32
}

// localSemanticEntry is one stored vector plus its response. norm
// is the vector's L2 norm, precomputed at store time so the hot
// scan in Get/Set never re-sums it per entry (see sparseDot).
// gen is the slot generation: bumped on every (re)store into the
// slot, so postings written for an older generation are recognisably
// stale. hash is the exact-vector hash used for dedup (see set).
// model is the MODEL SCOPE of the entry (CHECKLIST 187): the model
// id the request that stored it named. A lookup serves this entry
// only when it names the same model — never across models, however
// similar the text. "" means unattributed: stored by the raw
// Set/SetText API with an empty Response.Model, retrievable only
// by the raw model-less Get/GetText, never served to a request
// naming a model.
// tenant is the TENANT SCOPE of the entry (M6.11): the gateway
// key id the storing request arrived under ("" = anonymous — a
// real tenant, never a shared bucket). tempBucket is the
// TEMPERATURE SCOPE (M6.11): TemperatureBucket of the storing
// request's temperature ("0.70", "0.00", ...), the same 2-decimal
// bucketing the exact layer uses (key.go). scoped is true exactly
// when tenant/tempBucket were recorded by a Scoped store API; a
// legacy (unscoped) store leaves scoped=false and both fields
// empty. A scoped lookup serves this entry only when scoped is
// true AND tenant AND tempBucket both match the lookup's scope —
// an unscoped entry therefore fails closed under every scoped
// lookup, never served. The legacy (unscoped) lookups ignore
// these fields entirely (see the package doc).
// nIdx is how many inverted-index postings this entry owns (0 for
// wide vectors, which the index cannot cover). wide marks a vector
// longer than SemanticVectorDims: the index only covers the first
// 256 dimensions, so wide entries are scored by the legacy full
// scan instead (see Get).
type localSemanticEntry struct {
	vec     []float32 // L2-normalized, SemanticVectorDims long
	norm    float64   // L2 norm of vec (1 for normalized vectors)
	entry   SemanticEntry
	expires time.Time
	gen     uint32
	hash    uint64
	model   string // model scope (CHECKLIST 187); "" = unattributed
	tenant  string // tenant scope (M6.11); "" = anonymous tenant, only when scoped
	// tempBucket is the temperature scope (M6.11): the
	// TemperatureBucket of the storing request's temperature.
	tempBucket string
	// scoped reports whether tenant/tempBucket were recorded
	// (M6.11 Scoped store APIs). False for legacy entries.
	scoped bool
	nIdx   int
	wide   bool
}

// semanticScope is the tenant + temperature scope a lookup
// enforces (M6.11). scoped=false is the legacy wildcard: the
// lookup ignores tenant/temperature entirely and serves any
// entry naming the model — the behaviour of the pre-M6.11
// Get/GetForModel APIs, kept for the raw/diagnostic path.
// scoped=true serves only entries recorded with exactly this
// tenant and temperature bucket.
type semanticScope struct {
	tenant     string
	tempBucket string
	scoped     bool
}

// legacySemanticScope is the unscoped lookup scope: tenant and
// temperature are ignored.
var legacySemanticScope = semanticScope{}

// scopedSemanticScope builds the serving scope for one request:
// tenant is the gateway key id ("" = anonymous, a real tenant),
// temperature is bucketed with TemperatureBucket — the exact
// layer's 2-decimal bucketing, never a second scheme.
func scopedSemanticScope(tenant string, temperature float64) semanticScope {
	return semanticScope{tenant: tenant, tempBucket: TemperatureBucket(temperature), scoped: true}
}

// matches reports whether entry e may be served to a lookup under
// scope s. The legacy wildcard matches everything (the model
// filter is applied separately by the caller); a scoped lookup
// matches only an entry that recorded the same tenant and the
// same temperature bucket — an unscoped (legacy) entry never
// matches, so it fails closed rather than leaking across tenants
// or temperatures.
func (s semanticScope) matches(e *localSemanticEntry) bool {
	if !s.scoped {
		return true
	}
	return e.scoped && e.tenant == s.tenant && e.tempBucket == s.tempBucket
}

// LocalSemanticCache is a concurrency-safe, offline, stdlib-only
// similarity cache. By default it stores fixed 256-dimensional
// hashed bag-of-words vectors (see EmbedText) and serves the stored
// response whose vector is closest to the query vector, provided
// the cosine similarity is at least the configured threshold.
// Entries carry a per-entry TTL and the cache evicts
// least-recently-used entries when full. When a NeuralEmbedder is
// configured (LocalSemanticCacheConfig), the text APIs instead
// embed through the provider model (text-embedding-3-small,
// 1536-dim, 3 s timeout, fail-open to bag-of-words) and store wide
// entries scored by the legacy full scan.
//
// By default this is NOT a neural embedding model and does NOT
// understand meaning: similarity here is token overlap (shared
// words and word pairs), so synonyms and paraphrases generally miss
// while word-sharing but semantically different texts can hit. It
// is a cheap local approximation, suitable as a first-pass cache on
// a low-end PC — not a replacement for real embeddings. With a
// NeuralEmbedder configured, the layer compares meaning instead;
// without one, do not describe it as embeddings or semantic
// understanding.
//
// It is wired into the request path through degrade.StaleEngine
// (WrapWithSemantic + Policy.ServeCache): cacheable requests are
// served proactively from exact first, then this layer, and
// successes are stored here under the last 3 turns' text
// (degrade.semanticText, M6.7).
// Every entry is scoped by the model the request named
// (CHECKLIST 187): a lookup serves only entries stored for that
// same model, so a cross-model hit can never happen; the provider
// is deliberately not part of the scope.
type LocalSemanticCache struct {
	mu         sync.Mutex
	threshold  float32
	maxEntries int
	ttl        time.Duration
	// neuralEmbedder / neuralModel / neuralTimeout are the neural
	// embedding backend (see LocalSemanticCacheConfig): a nil
	// embedder means bag-of-words everywhere.
	neuralEmbedder NeuralEmbedder
	neuralModel    string
	neuralTimeout  time.Duration
	// maxEntryBytes / maxTotalBytes are the M6.1 byte caps;
	// totalBytes is the sum of live entries' sizes, maintained
	// under mu on every store / free.
	maxEntryBytes int
	maxTotalBytes int
	totalBytes    int
	// slots is the dense entry store: index = slot, nil = free.
	// order tracks recency over slots: most recently used first.
	slots []*localSemanticEntry
	order []int
	size  int
	// byHash is the exact-vector dedup map, keyed by (model scope,
	// vector hash) — per model, so the same text stored under two
	// different models takes two slots instead of the second store
	// silently replacing the first (CHECKLIST 187).
	byHash  map[semanticDedupKey]int // (model, exact-vector hash) -> slot (dedup)
	nextGen uint32
	// postings is the inverted index: for each dimension, the
	// entries with a non-zero value there. Get scores only the
	// entries sharing at least one non-zero dimension with the
	// query instead of scanning all 500 entries (pprof,
	// 2026-11-18 bench: the full scan was ~11% of router CPU in
	// Get and ~7% in Set's near-duplicate scan). A posting is live
	// exactly when its slot holds an entry of the same generation;
	// evicted / replaced / expired entries leave stale postings
	// behind, which Get skips by the generation check. Stale
	// postings are reclaimed in bulk by maybeRebuild (see below),
	// so no per-eviction list surgery is ever needed.
	postings [SemanticVectorDims][]posting
	// postedTotal counts every posting ever appended (live +
	// stale); livePosts counts postings owned by live slots.
	// maybeRebuild runs when stale outnumbers live.
	postedTotal int
	livePosts   int
	// wideCount is how many live entries hold vectors longer than
	// SemanticVectorDims (raw-embedding callers only — EmbedText
	// always returns 256 dims). While any wide entry lives, Get
	// uses the legacy full scan so those entries score exactly.
	wideCount int
	// acc is scratch for Get's dot accumulation, indexed by slot
	// (len maxEntries, reused under mu, never retained).
	acc []float64
	// Embedding counters (Stream D observability): which vector space
	// the text APIs actually embedded with. embedTextCtx runs outside
	// mu, so these are atomics. neuralCalls counts successful neural
	// embeds, bowCalls counts hashed bag-of-words embeds (no embedder
	// configured, or the neural call failed/timed out/was malformed),
	// neuralFallbacks counts those fail-open fallbacks explicitly.
	neuralCalls     atomic.Uint64
	bowCalls        atomic.Uint64
	neuralFallbacks atomic.Uint64
}

// NewLocalSemanticCache returns a LocalSemanticCache with the
// defaults: threshold 0.85, 500 entries, 1h TTL for interface Set.
func NewLocalSemanticCache() *LocalSemanticCache {
	return NewLocalSemanticCacheWithConfig(LocalSemanticCacheConfig{})
}

// NewLocalSemanticCacheWithConfig returns a LocalSemanticCache tuned
// by cfg; zero fields select the defaults.
// semanticDedupKey is the exact-vector dedup key: the model scope
// plus the vector hash. Two models storing the same text hash the
// same vector but dedup separately — neither store may evict or
// replace the other's entry (CHECKLIST 187). M6.11 extends the
// key with the tenant + temperature scope: the same text stored
// for two tenants, or at two temperatures, takes two slots — a
// re-store under one scope must never replace (or evict) another
// scope's entry. Legacy (unscoped) entries dedup under
// scoped=false with empty tenant/tempBucket, so they never
// collide with a scoped entry — not even an anonymous tenant's
// ("", "0.00") scoped entry, which is a real scope, not a legacy
// one.
type semanticDedupKey struct {
	model      string
	tenant     string
	tempBucket string
	scoped     bool
	hash       uint64
}

// dedupKey returns e's exact-vector dedup key (M6.11: model +
// tenant + temperature bucket + scoped flag + vector hash).
func (e *localSemanticEntry) dedupKey() semanticDedupKey {
	return semanticDedupKey{model: e.model, tenant: e.tenant, tempBucket: e.tempBucket, scoped: e.scoped, hash: e.hash}
}

func NewLocalSemanticCacheWithConfig(cfg LocalSemanticCacheConfig) *LocalSemanticCache {
	threshold := cfg.Threshold
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultSemanticThreshold
	}
	// M6.7 (review N1): the floor is enforced here too, not only
	// on the WithThreshold path — a configured value below the
	// 0.85 floor is raised to it, so legacy Get/GetForModel can
	// never serve below the floor either.
	if threshold < DefaultSemanticThreshold {
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
	maxEntryBytes := cfg.MaxEntryBytes
	if maxEntryBytes <= 0 {
		maxEntryBytes = DefaultMaxEntryBytes
	}
	maxTotalBytes := cfg.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultMaxTotalBytes
	}
	neuralModel := cfg.NeuralModel
	if neuralModel == "" {
		neuralModel = DefaultNeuralEmbeddingModel
	}
	neuralTimeout := cfg.NeuralTimeout
	if neuralTimeout <= 0 {
		neuralTimeout = DefaultNeuralEmbeddingTimeout
	}
	return &LocalSemanticCache{
		threshold:      threshold,
		maxEntries:     maxEntries,
		ttl:            ttl,
		neuralEmbedder: cfg.NeuralEmbedder,
		neuralModel:    neuralModel,
		neuralTimeout:  neuralTimeout,
		maxEntryBytes:  maxEntryBytes,
		maxTotalBytes:  maxTotalBytes,
		slots:          make([]*localSemanticEntry, 0, maxEntries),
		order:          make([]int, 0, maxEntries),
		byHash:         make(map[semanticDedupKey]int, maxEntries),
		acc:            make([]float64, maxEntries),
	}
}

// semanticEntrySize is the accounted size of one stored semantic
// entry: the vector's bytes (4 per float32 — the stored copy is
// exactly this long) plus the response content's bytes.
func semanticEntrySize(vec []float32, entry SemanticEntry) int {
	return len(vec)*4 + len(entry.Response.Content)
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
//
// Hot-path note: the old version allocated a fnv.Hash64 object (and
// a []byte copy of the token) per token. This inlines the FNV-1a
// loop over the token's bytes — byte-identical hash values, so
// bucket assignments (and therefore every similarity ever computed)
// are unchanged, with zero allocation.
func hashToken(token string) int {
	return int(fnv1a(token) % SemanticVectorDims)
}

// fnv1a is FNV-1a 64 over s: offset basis 14695981039346656037,
// prime 1099511628211 — exactly what hash/fnv.New64a computes.
func fnv1a(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// hashBigram maps a consecutive token pair to a vector bucket: the
// FNV-1a stream of a, one 0 byte, then b — byte-identical to
// hashToken(a + "\x00" + b), without the concatenating allocation.
func hashBigram(a, b string) int {
	h := uint64(14695981039346656037)
	for i := 0; i < len(a); i++ {
		h ^= uint64(a[i])
		h *= 1099511628211
	}
	h ^= 0
	h *= 1099511628211
	for i := 0; i < len(b); i++ {
		h ^= uint64(b[i])
		h *= 1099511628211
	}
	return int(h % SemanticVectorDims)
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
		vec[hashBigram(tokens[i], tokens[i+1])]++
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

// embedTextCtx embeds text for the text APIs (GetText*,
// SetText*): the neural provider embedding when a NeuralEmbedder
// is configured, else the hashed bag-of-words vector. A neural
// call runs under the configured timeout (default 3 s) and fails
// open — any error, timeout, empty vector, or vector containing
// NaN/Inf falls back to EmbedText, so the semantic layer keeps
// serving (with token-overlap similarity) when the embedding
// provider is down or slow. A neural vector is returned as-is:
// cosine divides by the stored norm at scan time, so a
// non-normalized provider vector still scores correctly.
// A neural vector (1536 dims) and a bag-of-words fallback vector
// (256 dims) never match each other: the legacy scan skips
// entries whose vector length differs from the query's (see
// getLegacyScopedLocked), so a fallback entry stored during an
// embedding outage can only be served to a fallback query, never
// to a neural one — and it expires within the entry TTL.
func (c *LocalSemanticCache) embedTextCtx(ctx context.Context, text string) []float32 {
	vec, _ := c.embedTextSource(ctx, text)
	return vec
}

// embedTextSource is embedTextCtx plus the vector space it embedded
// in: VectorSourceNeural when the configured neural embedder produced
// the vector, VectorSourceBagOfWords when the call was skipped or
// failed open. The source is what a hit reports as its
// VectorSource, so neural-vs-bag-of-words serving is observable.
func (c *LocalSemanticCache) embedTextSource(ctx context.Context, text string) ([]float32, string) {
	if c.neuralEmbedder == nil {
		c.bowCalls.Add(1)
		return embedText(text), VectorSourceBagOfWords
	}
	timeout := c.neuralTimeout
	if timeout <= 0 {
		timeout = DefaultNeuralEmbeddingTimeout
	}
	ectx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	vec, err := c.neuralEmbedder(ectx, text)
	if err != nil || len(vec) == 0 {
		c.neuralFallbacks.Add(1)
		c.bowCalls.Add(1)
		return embedText(text), VectorSourceBagOfWords
	}
	for _, v := range vec {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			c.neuralFallbacks.Add(1)
			c.bowCalls.Add(1)
			return embedText(text), VectorSourceBagOfWords
		}
	}
	c.neuralCalls.Add(1)
	return vec, VectorSourceNeural
}

// sparseIndex lists v's non-zero indices and returns its L2 norm.
// buf is scratch (a stack array from the caller when v fits, heap
// otherwise); the returned slice aliases buf and must not be
// retained past the call. Zeros contribute nothing to a dot product
// or a norm, so the index list is all a similarity scan needs.
//
// Hot-path note (pprof, 2026-11-17 bench: cosine was 30.6% of all
// router CPU — and a first sparse rewrite that still looped over
// all 256 slots twice per pair measured SLOWER, 52%): the win is
// not in cosine itself but in the cache scan — Get/Set compute the
// query's index list and norm ONCE per call and reuse the entries'
// precomputed norms, so each stored entry costs ~20 multiplies
// (one per non-zero query bucket) instead of 256 iterations. The
// generic cosine below keeps the same sparse maths for arbitrary
// vectors (its per-pair cost is roughly the old one); the cache
// fast path is what carries the improvement.
func sparseIndex(v []float32, buf []int) ([]int, float64) {
	idx := buf[:0]
	if cap(idx) < len(v) {
		idx = make([]int, 0, len(v))
	}
	var sum float64
	for i, x := range v {
		if x != 0 {
			idx = append(idx, i)
			sum += float64(x) * float64(x)
		}
	}
	return idx, math.Sqrt(sum)
}

// sparseDot is the dot product of q and e over q's non-zero indices
// only. Indices past the end of e (vectors of different lengths)
// contribute nothing, matching cosine's shorter-of-the-two rule.
func sparseDot(q []float32, qIdx []int, e []float32) float64 {
	var dot float64
	for _, i := range qIdx {
		if i < len(e) {
			dot += float64(q[i]) * float64(e[i])
		}
	}
	return dot
}

// cosine returns the cosine similarity of a and b, computed over the
// shorter of the two. Zero vectors (or empty inputs) score 0. The
// result is the same cosine as the dense sum, up to float64
// summation-order rounding (~1e-16 relative).
func cosine(a, b []float32) float32 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var bufA, bufB [SemanticVectorDims]int
	aIdx, aNorm := sparseIndex(a, bufA[:0])
	if aNorm == 0 {
		return 0
	}
	_, bNorm := sparseIndex(b, bufB[:0])
	if bNorm == 0 {
		return 0
	}
	sim := sparseDot(a, aIdx, b) / (aNorm * bNorm)
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

// touch moves slot to the front of the recency order. Caller holds
// mu. It shifts in place (append + copy) instead of allocating a
// fresh slice per touch as the old implementation did.
func (c *LocalSemanticCache) touch(slot int) {
	for i, s := range c.order {
		if s == slot {
			copy(c.order[i:], c.order[i+1:])
			c.order = c.order[:len(c.order)-1]
			break
		}
	}
	c.order = append(c.order, 0)
	copy(c.order[1:], c.order[:len(c.order)-1])
	c.order[0] = slot
}

// freeSlotLocked frees slot: the entry leaves the store, its hash
// mapping and its recency position, and its postings go stale (they
// are recognised by the generation check in Get and reclaimed in
// bulk by maybeRebuild — no per-eviction list surgery). Its bytes
// leave totalBytes with it (M6.1). Caller holds mu.
func (c *LocalSemanticCache) freeSlotLocked(slot int) {
	e := c.slots[slot]
	if e == nil {
		return
	}
	if h, ok := c.byHash[e.dedupKey()]; ok && h == slot {
		delete(c.byHash, e.dedupKey())
	}
	c.livePosts -= e.nIdx
	c.totalBytes -= semanticEntrySize(e.vec, e.entry)
	if e.wide {
		c.wideCount--
	}
	c.slots[slot] = nil
	c.size--
	for i, s := range c.order {
		if s == slot {
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
	c.freeSlotLocked(c.order[len(c.order)-1])
}

// makeRoomLocked evicts least-recently-used entries other than
// keepSlot until size bytes fit under the total cap (M6.1). The
// entry keepSlot already holds is freed by the caller right after,
// so its bytes do not count against the projection; keepSlot -1
// means no slot is kept. It stops when nothing else can be evicted
// — in that case only keepSlot's entry remains and the new entry
// fits, because a single entry is always under the total cap (the
// per-entry cap is smaller than the total cap). Caller holds mu.
func (c *LocalSemanticCache) makeRoomLocked(size, keepSlot int) {
	oldSize := 0
	if keepSlot >= 0 && keepSlot < len(c.slots) {
		if old := c.slots[keepSlot]; old != nil {
			oldSize = semanticEntrySize(old.vec, old.entry)
		}
	}
	for c.totalBytes-oldSize+size > c.maxTotalBytes {
		evicted := false
		for i := len(c.order) - 1; i >= 0; i-- {
			if c.order[i] == keepSlot {
				continue
			}
			c.freeSlotLocked(c.order[i])
			evicted = true
			break
		}
		if !evicted {
			break
		}
	}
}

// hashSparse hashes a vector's non-zero entries (dimension + value
// bits) with FNV-1a: two vectors hash equal exactly when their
// sparse contents agree, which for equal-length vectors means the
// vectors are identical. Used for exact-vector dedup in set.
func hashSparse(vec []float32, idx []int) uint64 {
	h := uint64(14695981039346656037)
	for _, i := range idx {
		h ^= uint64(i)
		h *= 1099511628211
		h ^= uint64(math.Float32bits(vec[i]))
		h *= 1099511628211
	}
	return h
}

// storeInSlotLocked writes a fresh entry into slot (which must be
// free — callers free it first when reusing) and indexes it: one
// posting per non-zero dimension, carrying the slot's new
// generation. Wide vectors (longer than SemanticVectorDims) are
// stored without postings and counted in wideCount, so Get falls
// back to the legacy full scan while any wide entry lives. Caller
// holds mu.
func (c *LocalSemanticCache) storeInSlotLocked(slot int, vec []float32, qIdx []int, qNorm float64, hash uint64, model string, scope semanticScope, entry SemanticEntry, expires time.Time) {
	c.nextGen++
	wide := len(vec) > SemanticVectorDims
	e := &localSemanticEntry{
		vec: vec, norm: qNorm, entry: entry, expires: expires,
		gen: c.nextGen, hash: hash, model: model, wide: wide,
		tenant: scope.tenant, tempBucket: scope.tempBucket, scoped: scope.scoped,
	}
	c.slots[slot] = e
	c.size++
	c.totalBytes += semanticEntrySize(vec, entry)
	if wide {
		c.wideCount++
		return
	}
	e.nIdx = len(qIdx)
	for _, dim := range qIdx {
		c.postings[dim] = append(c.postings[dim], posting{slot: int32(slot), val: vec[dim], gen: e.gen})
	}
	c.livePosts += len(qIdx)
	c.postedTotal += len(qIdx)
}

// maybeRebuild reclaims stale postings in bulk: when stale postings
// outnumber live ones (and the index is non-trivial), every posting
// list is rebuilt from the live slots alone. The rebuild costs one
// sparse pass per live entry and is amortised over the evictions
// that created the staleness, so the inverted index stays bounded
// no matter how many entries churn through the cache. Caller holds
// mu.
func (c *LocalSemanticCache) maybeRebuild() {
	stale := c.postedTotal - c.livePosts
	if c.postedTotal < 1024 || stale <= c.livePosts {
		return
	}
	for d := range c.postings {
		c.postings[d] = c.postings[d][:0]
	}
	c.postedTotal = 0
	for slot, e := range c.slots {
		if e == nil || e.wide {
			continue
		}
		var buf [SemanticVectorDims]int
		idx, _ := sparseIndex(e.vec, buf[:0])
		for _, dim := range idx {
			c.postings[dim] = append(c.postings[dim], posting{slot: int32(slot), val: e.vec[dim], gen: e.gen})
			c.postedTotal++
		}
	}
}

// Get returns the stored response whose vector is most similar to
// embedding, when that similarity is at least the configured
// threshold. Expired entries are deleted and reported as misses. A
// hit refreshes the entry's recency and reports the actual
// similarity in the returned entry. Zero or empty embeddings always
// miss.
//
// RAW model-less lookup (see the SemanticCache interface doc): Get
// serves only entries stored WITHOUT a recorded model (scope "").
// A request naming a model must use GetForModel instead — this
// method can never serve a model-scoped entry, so a cross-model
// hit through it is impossible by construction (CHECKLIST 187).
//
// Hot path (pprof, 2026-11-18 bench): the dot products are
// accumulated through the inverted index — one partial product per
// (query dimension, entry sharing it) instead of one dot per stored
// entry — and only the winning similarity is ever divided by its
// norm. The accumulation is exact (same float64 products as the old
// sparseDot, in a different summation order — the difference is
// ~1e-16, far below the 0.85 threshold), so Get returns the same
// best entry as the old full scan; see TestSemanticGetMatchesBruteForce.
func (c *LocalSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	return c.getScoped(ctx, "", embedding, legacySemanticScope)
}

// GetForModel is Get scoped by model (CHECKLIST 187): the stored
// response whose vector is most similar to embedding AND whose
// recorded model scope equals model, when that similarity is at
// least the configured threshold. A cached claude-sonnet response
// is never served for a gpt-4o lookup, however similar the text —
// the model is matched exactly (case-sensitive), the same rule the
// exact-match cache applies through ExactKey. The provider is
// deliberately not part of the scope: the same model id served by
// two providers shares entries. Expired entries are deleted and
// reported as misses; a hit refreshes recency and reports the
// actual similarity. Zero or empty embeddings always miss.
//
// LEGACY (M6.11): this lookup is TENANT- and TEMPERATURE-BLIND —
// it serves any entry naming the model, scoped or not. Serving
// code must use GetForModelScopedWithThreshold instead, which
// enforces the requesting tenant and temperature bucket.
// This method is the raw/diagnostic path (and the pinned
// behaviour of TestM611SemanticLayerIsTenantBlind).
func (c *LocalSemanticCache) GetForModel(ctx context.Context, model string, embedding []float32) (SemanticEntry, bool) {
	return c.getScoped(ctx, model, embedding, legacySemanticScope)
}

// GetForModelScopedWithThreshold is GetForModel with the M6.11
// serving scope: the stored response whose vector is most similar
// to embedding AND whose recorded model scope equals model AND
// whose recorded tenant equals tenant AND whose recorded
// temperature bucket equals TemperatureBucket(temperature), when
// that similarity is at least the effective threshold (see
// effectiveSemanticThreshold: the floor never drops below
// DefaultSemanticThreshold, 0.85). tenant "" is the anonymous
// tenant — a real tenant, never a shared bucket. An entry stored
// through a legacy (unscoped) API is never served here, whatever
// its similarity: it recorded no tenant/temperature, so it fails
// closed. A threshold <= 0 means "no override" (the cache's
// configured floor applies). Serving code (degrade, relay) must
// use this, never GetForModel.
func (c *LocalSemanticCache) GetForModelScopedWithThreshold(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	return c.getScopedWithThreshold(ctx, model, embedding, c.effectiveSemanticThreshold(threshold), scopedSemanticScope(tenant, temperature))
}

// effectiveSemanticThreshold resolves a per-call threshold
// request (M6.7) against the cache's configured floor. The
// effective threshold NEVER goes below DefaultSemanticThreshold
// (0.85): a per-call override can only narrow (raise) the bar,
// never widen it. A request below the floor behaves as the floor
// — the override is not honoured as given, and the caller must
// not pretend it was (the degrade wrapper documents the same
// rule for the header/config values it passes in). Rules:
//   - requested <= 0: no override, the configured threshold
//     applies (floored at DefaultSemanticThreshold).
//   - requested in (0, 1]: max(configured threshold,
//     DefaultSemanticThreshold, requested).
//   - requested > 1: clamped to 1 (exact matches only); the
//     header parser already clamps to [0, 1], this is the
//     backstop for directly-constructed callers.
func (c *LocalSemanticCache) effectiveSemanticThreshold(requested float32) float32 {
	floor := c.threshold
	if floor < DefaultSemanticThreshold {
		floor = DefaultSemanticThreshold
	}
	if requested <= 0 {
		return floor
	}
	if requested > 1 {
		requested = 1
	}
	if requested < floor {
		return floor
	}
	return requested
}

// GetForModelWithThreshold is GetForModel with a per-call
// similarity floor (M6.7): the stored response whose vector is
// most similar to embedding AND whose recorded model scope
// equals model, when that similarity is at least the effective
// threshold — see effectiveSemanticThreshold: the effective
// floor never drops below DefaultSemanticThreshold (0.85), so a
// per-request override can only narrow hits, never widen them.
// A threshold <= 0 means "no override" (the cache's configured
// floor applies). Serving code passes the request's effective
// threshold here instead of post-filtering the hit afterwards:
// the floor is then enforced at the layer that owns it, in one
// place, rather than relying on every caller to re-check the
// reported similarity. (Selection is identical either way —
// the lookup returns the single best entry, so a post-filter
// and this method agree on hit vs miss; the difference is
// where the rule lives, not the outcome.)
func (c *LocalSemanticCache) GetForModelWithThreshold(ctx context.Context, model string, embedding []float32, threshold float32) (SemanticEntry, bool) {
	return c.getScopedWithThreshold(ctx, model, embedding, c.effectiveSemanticThreshold(threshold), legacySemanticScope)
}

// getScoped is the shared lookup for Get (model "") and GetForModel.
// The model filter applies in the best-slot scan of both the
// indexed and the legacy path: an entry whose recorded scope
// differs from model is never a candidate, whatever its
// similarity. The tenant/temperature filter (M6.11) applies the
// same way, in BOTH paths: scope.matches is checked in the
// indexed scan below and in getLegacyScopedLocked (the wide-vector
// path), so no lookup path can serve an entry outside the
// requesting tenant/temperature. Expired entries are swept
// regardless of scope.
func (c *LocalSemanticCache) getScoped(ctx context.Context, model string, embedding []float32, scope semanticScope) (SemanticEntry, bool) {
	return c.getScopedWithThreshold(ctx, model, embedding, 0, scope)
}

// getScopedWithThreshold is getScoped with an explicit effective
// threshold: threshold <= 0 means the cache's configured
// threshold (c.threshold, exactly as Get/GetForModel use it —
// the legacy behaviour is preserved verbatim for existing
// callers); threshold > 0 is used as-is, so the caller must
// already have resolved it through effectiveSemanticThreshold.
// Both the indexed and the legacy path apply the same floor.
func (c *LocalSemanticCache) getScopedWithThreshold(ctx context.Context, model string, embedding []float32, threshold float32, scope semanticScope) (SemanticEntry, bool) {
	if len(embedding) == 0 || isZeroVector(embedding) {
		return SemanticEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var buf [SemanticVectorDims]int
	qIdx, qNorm := sparseIndex(embedding, buf[:0])
	if qNorm == 0 {
		return SemanticEntry{}, false
	}
	effThreshold := c.threshold
	if threshold > 0 {
		effThreshold = threshold
	}
	if c.wideCount > 0 || len(embedding) > SemanticVectorDims {
		return c.getLegacyScopedLocked(model, embedding, qIdx, qNorm, now, effThreshold, scope)
	}
	// Accumulate partial dots: only entries sharing a non-zero
	// dimension with the query are ever touched.
	acc := c.acc[:len(c.slots)]
	for i := range acc {
		acc[i] = 0
	}
	for _, dim := range qIdx {
		qv := float64(embedding[dim])
		for _, p := range c.postings[dim] {
			slot := int(p.slot)
			e := c.slots[slot]
			if e == nil || e.gen != p.gen {
				continue // stale posting: slot reused or freed
			}
			acc[slot] += qv * float64(p.val)
		}
	}
	// Max scan over slots, sweeping expired entries as we go.
	// The model filter (CHECKLIST 187): an entry scoped to another
	// model is never a candidate, whatever its similarity. The
	// tenant/temperature filter (M6.11) is the same rule one
	// dimension further: scope.matches rejects an entry recorded
	// for another tenant, another temperature bucket, or no
	// scope at all (legacy entry under a scoped lookup).
	bestSlot := -1
	var best float32
	var expired []int
	for slot, e := range c.slots {
		if e == nil {
			continue
		}
		if now.After(e.expires) {
			expired = append(expired, slot)
			continue
		}
		if e.model != model {
			continue
		}
		if !scope.matches(e) {
			continue
		}
		if e.norm == 0 {
			continue
		}
		if sim := float32(acc[slot] / (qNorm * e.norm)); sim > best {
			best, bestSlot = sim, slot
		}
	}
	for _, slot := range expired {
		c.freeSlotLocked(slot)
	}
	if len(expired) > 0 {
		c.maybeRebuild()
	}
	if bestSlot < 0 || best < effThreshold {
		return SemanticEntry{}, false
	}
	c.touch(bestSlot)
	hit := c.slots[bestSlot].entry
	hit.Similarity = best
	return hit, true
}

// getLegacyScopedLocked is the pre-index full scan, kept for correctness
// with vectors longer than SemanticVectorDims (raw-embedding
// callers only): every live entry is scored with sparseDot, exactly
// as Get did before the inverted index. The model filter applies
// here too: an entry scoped to another model is never a candidate.
// The tenant/temperature filter (M6.11) applies here too — this is
// the wide-vector path, and a scoped lookup that skipped it would
// leak across tenants/temperatures for exactly the entries the
// index cannot cover. Caller holds mu.
func (c *LocalSemanticCache) getLegacyScopedLocked(model string, embedding []float32, qIdx []int, qNorm float64, now time.Time, threshold float32, scope semanticScope) (SemanticEntry, bool) {
	bestSlot := -1
	var best float32
	var expired []int
	for slot, e := range c.slots {
		if e == nil {
			continue
		}
		if now.After(e.expires) {
			expired = append(expired, slot)
			continue
		}
		if e.model != model {
			continue
		}
		if !scope.matches(e) {
			continue
		}
		if e.norm == 0 {
			continue
		}
		// Vectors from different embedding spaces are
		// incomparable: a neural query (1536 dims) must never
		// match a bag-of-words fallback entry (256 dims) stored
		// during an embedding outage, or vice versa — cosine
		// over the shared prefix would be meaningless. Entries
		// are therefore scored only against a query of the same
		// length; a fallback entry can only serve a fallback
		// query, and it expires within the entry TTL.
		if len(e.vec) != len(embedding) {
			continue
		}
		if sim := float32(sparseDot(embedding, qIdx, e.vec) / (qNorm * e.norm)); sim > best {
			best, bestSlot = sim, slot
		}
	}
	for _, slot := range expired {
		c.freeSlotLocked(slot)
	}
	if bestSlot < 0 || best < threshold {
		return SemanticEntry{}, false
	}
	c.touch(bestSlot)
	hit := c.slots[bestSlot].entry
	hit.Similarity = best
	return hit, true
}

// Set stores entry under embedding for the cache's default TTL. A
// zero or empty embedding stores nothing. The entry's model scope
// is entry.Response.Model: an entry stored with a model is
// retrievable only by a lookup naming that model (GetForModel);
// an entry stored without one is retrievable only by the raw
// model-less Get. Storing a vector identical to an existing entry
// replaces that entry in place instead of duplicating it
// (exact-vector dedup by (model, tenant, temperature bucket,
// scoped, hash) — the old cosine-≥0.9999
// near-duplicate scan cost one full dot pass per store, ~7% of
// router CPU in the 2026-11-18 profile, for a distinction that only
// ever mattered for re-stores of the same text; a merely
// near-identical vector now takes its own slot, bounded by the
// LRU). When the cache is full, the least recently used entry is
// evicted.
//
// Serving code must use SetTextForModelScoped naming the REQUEST model,
// never Set/SetText directly: the response's own Model field can
// name an upstream alias that differs from the model the client
// asked for, and the scope must be the requested model
// (CHECKLIST 187). A legacy Set stores an unscoped entry a scoped
// lookup never serves (M6.11).
func (c *LocalSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) {
	c.setScoped(embedding, entry.Response.Model, legacySemanticScope, entry, c.ttl)
}

// SetForModel is Set with an explicit model scope (CHECKLIST 187):
// the entry is stored under embedding for the cache's default TTL
// and is retrievable only by a lookup naming model, whatever
// entry.Response.Model says. Serving code passes the model the
// request named (req.Model), not the response's model.
//
// LEGACY (M6.11): this stores an UNSCOPED entry (no tenant /
// temperature recorded). A scoped lookup never serves it — it
// fails closed. Serving code must use SetForModelScoped instead.
func (c *LocalSemanticCache) SetForModel(ctx context.Context, model string, embedding []float32, entry SemanticEntry) {
	c.setScoped(embedding, model, legacySemanticScope, entry, c.ttl)
}

// SetForModelScoped is SetForModel with the M6.11 serving scope:
// the entry is stored under embedding for the cache's default TTL,
// recorded for tenant (gateway key id, "" = anonymous) and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket. Serving code passes the model the request named
// (req.Model), the request's tenant (req.CallerID) and its
// temperature. The provider is deliberately not part of the scope.
func (c *LocalSemanticCache) SetForModelScoped(ctx context.Context, model string, embedding []float32, tenant string, temperature float64, entry SemanticEntry) {
	c.setScoped(embedding, model, scopedSemanticScope(tenant, temperature), entry, c.ttl)
}

// set is the shared store path for Set, SetForModel, SetText and
// SetTextForModel. Caller passes the model scope and the TTL;
// ttl <= 0 stores nothing. The scope is recorded on the slot and
// is part of the dedup key, so the same text under two models
// coexists as two entries.
//
// LEGACY (M6.11): set stores an UNSCOPED entry (no tenant /
// temperature). Use setScoped for serving stores.
func (c *LocalSemanticCache) set(embedding []float32, model string, entry SemanticEntry, ttl time.Duration) {
	c.setScoped(embedding, model, legacySemanticScope, entry, ttl)
}

// setScoped is set with an explicit tenant/temperature scope
// (M6.11): scope carries the tenant and temperature bucket the
// entry is recorded under (legacySemanticScope for the legacy
// store path — no scope recorded). The scope is part of the dedup
// key, so the same text under two tenants, two temperature
// buckets, or two models coexists as separate entries, and a
// re-store under one scope never replaces another scope's entry.
func (c *LocalSemanticCache) setScoped(embedding []float32, model string, scope semanticScope, entry SemanticEntry, ttl time.Duration) {
	if ttl <= 0 || len(embedding) == 0 || isZeroVector(embedding) {
		return
	}
	// Byte caps (M6.1): an entry over the per-entry cap stores
	// nothing. The check runs BEFORE any free, so a refused
	// oversize entry never deletes the entry it would replace.
	size := semanticEntrySize(embedding, entry)
	if size > c.maxEntryBytes {
		return
	}
	vec := make([]float32, len(embedding))
	copy(vec, embedding)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	var buf [SemanticVectorDims]int
	qIdx, qNorm := sparseIndex(vec, buf[:0])
	hash := hashSparse(vec, qIdx)
	// Exact-vector dedup, per model scope: an identical vector
	// stored for the SAME model replaces its entry in place
	// (refreshed expiry + response, recency refreshed), so
	// re-storing a cache hit never grows the cache. The same
	// vector under a DIFFERENT model takes its own slot — the
	// dedup key is (model, hash), never the hash alone. A hash
	// collision (different vector, same hash) falls through to a
	// fresh insert — the map keeps the first vector's slot, and
	// the colliding vector simply takes its own slot.
	// M6.11: the dedup key is (model, tenant, temperature
	// bucket, scoped, hash) — the same vector under a different
	// tenant or temperature takes its own slot too, and a legacy
	// (unscoped) store never replaces a scoped entry or vice
	// versa.
	dedup := semanticDedupKey{model: model, tenant: scope.tenant, tempBucket: scope.tempBucket, scoped: scope.scoped, hash: hash}
	if slot, ok := c.byHash[dedup]; ok && slot < len(c.slots) {
		if e := c.slots[slot]; e != nil && !e.wide && len(e.vec) == len(vec) && slices.Equal(e.vec, vec) {
			c.makeRoomLocked(size, slot)
			c.freeSlotLocked(slot)
			c.storeInSlotLocked(slot, vec, qIdx, qNorm, hash, model, scope, entry, now.Add(ttl))
			c.byHash[dedup] = slot
			c.touch(slot)
			c.maybeRebuild()
			return
		}
	}
	// Find a slot: a free slot first, then an expired one (an
	// expired entry is dead weight — reusing it beats evicting a
	// live one), then a fresh slot while there is room, else the
	// least recently used entry.
	slot := -1
	for i, e := range c.slots {
		if e == nil {
			slot = i
			break
		}
	}
	if slot < 0 {
		for i, e := range c.slots {
			if e != nil && now.After(e.expires) {
				slot = i
				break
			}
		}
	}
	if slot < 0 && len(c.slots) < c.maxEntries {
		slot = len(c.slots)
		c.slots = append(c.slots, nil)
	}
	if slot < 0 {
		slot = c.order[len(c.order)-1]
	}
	// Byte cap (M6.1): make room before storing — the chosen
	// slot's old entry is freed right below, so its bytes do not
	// count against the projection (see makeRoomLocked).
	c.makeRoomLocked(size, slot)
	if old := c.slots[slot]; old != nil {
		c.freeSlotLocked(slot)
	}
	c.storeInSlotLocked(slot, vec, qIdx, qNorm, hash, model, scope, entry, now.Add(ttl))
	c.byHash[dedup] = slot
	c.touch(slot)
	c.maybeRebuild()
}

// GetText returns the cached response for text most similar to the
// given text, when the cosine similarity is at least the configured
// threshold. It is Get over the vector embedTextCtx builds for text
// (neural when a NeuralEmbedder is configured, else the hashed
// bag-of-words vector) — the RAW model-less lookup, so it serves only
// entries stored without a recorded model. Serving code must use
// GetTextForModelScopedWithThreshold instead (CHECKLIST 187,
// M6.11).
func (c *LocalSemanticCache) GetText(ctx context.Context, text string) (SemanticEntry, bool) {
	vec, source := c.embedTextSource(ctx, text)
	hit, ok := c.Get(ctx, vec)
	if ok {
		hit.VectorSource = source
	}
	return hit, ok
}

// GetTextForModel is GetText scoped by model (CHECKLIST 187): the
// cached response for text most similar to the given text, stored
// for the same model. A cached claude-sonnet response is never
// served for a gpt-4o lookup, however similar the text.
//
// LEGACY (M6.11): this lookup is TENANT- and TEMPERATURE-BLIND —
// it serves any entry naming the model, scoped or not. Serving
// code must use GetTextForModelScopedWithThreshold instead. This
// method is the raw/diagnostic path (pinned by
// TestM611SemanticLayerIsTenantBlind).
func (c *LocalSemanticCache) GetTextForModel(ctx context.Context, model, text string) (SemanticEntry, bool) {
	vec, source := c.embedTextSource(ctx, text)
	hit, ok := c.GetForModel(ctx, model, vec)
	if ok {
		hit.VectorSource = source
	}
	return hit, ok
}

// GetTextForModelWithThreshold is GetTextForModel with a per-call
// similarity floor (M6.7): the cached response for text most
// similar to the given text, stored for the same model, when the
// similarity is at least the effective threshold — see
// effectiveSemanticThreshold: the floor never drops below
// DefaultSemanticThreshold (0.85), so a per-request override can
// only narrow hits, never widen them. A threshold <= 0 means "no
// override".
//
// LEGACY (M6.11): tenant- and temperature-blind, like
// GetTextForModel. Serving code must use
// GetTextForModelScopedWithThreshold instead.
func (c *LocalSemanticCache) GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (SemanticEntry, bool) {
	vec, source := c.embedTextSource(ctx, text)
	hit, ok := c.GetForModelWithThreshold(ctx, model, vec, threshold)
	if ok {
		hit.VectorSource = source
	}
	return hit, ok
}

// GetTextForModelScopedWithThreshold is GetTextForModelWithThreshold
// with the M6.11 serving scope: the cached response for text most
// similar to the given text, stored for the same model AND the
// same tenant AND the same temperature bucket
// (TemperatureBucket(temperature)), when the similarity is at
// least the effective threshold — see effectiveSemanticThreshold:
// the floor never drops below DefaultSemanticThreshold (0.85).
// tenant "" is the anonymous tenant. An entry stored through a
// legacy (unscoped) API is never served here: it fails closed.
// A threshold <= 0 means "no override". Serving code (degrade,
// relay) must use this, never GetTextForModel.
func (c *LocalSemanticCache) GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (SemanticEntry, bool) {
	vec, source := c.embedTextSource(ctx, text)
	hit, ok := c.GetForModelScopedWithThreshold(ctx, model, vec, tenant, temperature, threshold)
	if ok {
		hit.VectorSource = source
	}
	return hit, ok
}

// SetText stores entry for text for ttl, keyed by the hashed
// bag-of-words vector EmbedText builds for text. ttl <= 0 stores
// nothing; token-less text stores nothing. The entry's model scope
// is entry.Response.Model. Serving code must use SetTextForModelScoped
// naming the REQUEST model instead (CHECKLIST 187, M6.11).
func (c *LocalSemanticCache) SetText(ctx context.Context, text string, entry SemanticEntry, ttl time.Duration) {
	c.set(c.embedTextCtx(ctx, text), entry.Response.Model, entry, ttl)
}

// SetTextForModel is SetText with an explicit model scope
// (CHECKLIST 187): entry is stored for text for ttl and is
// retrievable only by a lookup naming model. Serving code passes
// the model the request named (req.Model): the scope is the
// requested model, not the response's model (which can name an
// upstream alias). The provider is deliberately not part of the
// scope — the same model id on another provider shares the entry.
//
// LEGACY (M6.11): this stores an UNSCOPED entry (no tenant /
// temperature recorded). A scoped lookup never serves it — it
// fails closed. Serving code must use SetTextForModelScoped instead.
func (c *LocalSemanticCache) SetTextForModel(ctx context.Context, model, text string, entry SemanticEntry, ttl time.Duration) {
	c.set(c.embedTextCtx(ctx, text), model, entry, ttl)
}

// SetTextForModelScoped is SetTextForModel with the M6.11 serving
// scope: entry is stored for text for ttl, recorded for tenant
// (gateway key id, "" = anonymous) and
// TemperatureBucket(temperature), and is retrievable only by a
// scoped lookup naming the same model, tenant and temperature
// bucket. Serving code passes the model the request named
// (req.Model), the request's tenant (req.CallerID) and its
// temperature. The provider is deliberately not part of the scope.
func (c *LocalSemanticCache) SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry SemanticEntry, ttl time.Duration) {
	c.setScoped(c.embedTextCtx(ctx, text), model, scopedSemanticScope(tenant, temperature), entry, ttl)
}

// Size returns the number of entries currently stored (including any
// that have passed their TTL but not been accessed since).
func (c *LocalSemanticCache) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size
}

// Stats returns the live entry count and total accounted bytes
// (M6.8: backs the cache stats surface). Expired-but-unaccessed
// entries still count — they are removed lazily, same as Size.
func (c *LocalSemanticCache) Stats() (entries, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.size, c.totalBytes
}

// EmbeddingStats is the live counters of which vector space the
// semantic text APIs embedded with (Stream D observability). Read via
// EmbeddingStatsReporter by the stats/metrics surfaces; the counts are
// since the cache was built, like every other counter here.
type EmbeddingStats struct {
	// NeuralConfigured is true when a NeuralEmbedder is wired, i.e. the
	// cache is CAPABLE of neural vectors (it may still fall back per
	// call — see Fallbacks).
	NeuralConfigured bool `json:"neural_configured"`
	// NeuralModel is the configured embedding model (informational).
	NeuralModel string `json:"neural_model,omitempty"`
	// Neural counts text embeddings produced by the neural provider.
	Neural uint64 `json:"neural_embeds"`
	// BagOfWords counts text embeddings produced by the hashed
	// token-overlap vector (no embedder configured, or a fail-open
	// fallback — see Fallbacks).
	BagOfWords uint64 `json:"bag_of_words_embeds"`
	// Fallbacks counts neural calls that failed open to the hashed
	// vector (error, timeout, empty or NaN/Inf vector).
	Fallbacks uint64 `json:"neural_fallbacks"`
}

// EmbeddingStatsReporter is optionally implemented by the semantic
// stores (LocalSemanticCache and the persistent/Redis wrappers and
// their SemanticStore adapters): the server's stats and metrics
// surfaces read it structurally, so this package does not need to know
// who serves.
type EmbeddingStatsReporter interface {
	EmbeddingStats() EmbeddingStats
}

// EmbeddingStats returns the live embedding counters (see
// EmbeddingStats). It satisfies EmbeddingStatsReporter.
func (c *LocalSemanticCache) EmbeddingStats() EmbeddingStats {
	return EmbeddingStats{
		NeuralConfigured: c.neuralEmbedder != nil,
		NeuralModel:      c.neuralModel,
		Neural:           c.neuralCalls.Load(),
		BagOfWords:       c.bowCalls.Load(),
		Fallbacks:        c.neuralFallbacks.Load(),
	}
}
