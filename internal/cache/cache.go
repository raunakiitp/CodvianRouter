// Package cache implements the router's three-layer caching:
//
//  1. ExactCache — in-memory LRU cache with TTL. Key = SHA-256 of
//     model + messages JSON, so identical requests hit byte-for-byte.
//  2. Provider prompt cache — Prepare annotates an OpenAI-compatible
//     request with cache_control markers for providers that support
//     them (best-effort; a no-op for providers that don't).
//  3. Semantic cache — an interface plus a no-op implementation.
//     Real embeddings/vector search are a later phase (see the TODO on
//     SemanticCache).
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
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
// Layer 3: semantic cache (interface + no-op; real work is later)
// ---------------------------------------------------------------------------

// SemanticEntry is a cached response found by meaning rather than by
// exact match. Similarity is the match score (e.g. cosine similarity)
// reported by the underlying store, 0..1.
type SemanticEntry struct {
	Response   CachedResponse
	Similarity float32
}

// SemanticCache finds cached responses by embedding similarity.
// Callers pass an embedding of the incoming request and receive the
// closest stored response when it is similar enough.
//
// TODO(phase-2): implement a real SemanticCache once an embedding
// provider and a vector store are chosen. That needs (a) an embeddings
// call (provider API or a local model), (b) storage for vectors with a
// similarity threshold and TTL, and (c) a policy for when a semantic
// hit is safe to serve (same model? same intent? user opt-in?). Until
// then, NoopSemanticCache keeps the layer wired but inert.
type SemanticCache interface {
	Get(ctx context.Context, embedding []float32) (SemanticEntry, bool)
	Set(ctx context.Context, embedding []float32, entry SemanticEntry)
}

// NoopSemanticCache is a SemanticCache that never hits and stores
// nothing. Use it until a real embedding-backed implementation lands.
type NoopSemanticCache struct{}

// Get always reports a miss.
func (NoopSemanticCache) Get(ctx context.Context, embedding []float32) (SemanticEntry, bool) {
	return SemanticEntry{}, false
}

// Set discards the entry.
func (NoopSemanticCache) Set(ctx context.Context, embedding []float32, entry SemanticEntry) {
}
