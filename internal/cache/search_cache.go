// Search result cache (M6.6, parity tracker Area 6 row "Search cache
// (60 s)"): the cache-package half of the sector-typical searchCache.ts.
//
// WHAT THE SECTOR BASELINE CACHES (the earlier implementation, read-only
// study — no code copied): search API results, keyed by SHA-256 of
// {q: normalized query, p: provider, t: searchType, n: maxResults,
// c: country, l: language, f: filters}. Query normalization is
// NFKC + lowercase + trim + collapse whitespace. Default TTL is
// 60 s (SEARCH_CACHE_TTL_MS, default 60*1000); at most 500 entries
// (MAX_CACHE_ENTRIES); eviction purges expired entries first, then
// FIFO. getOrCoalesce() returns the cached result, joins an
// in-flight identical request (coalescing — one upstream fetch for
// concurrent identical queries), or executes the fetch and stores
// it. A TTL of 0 bypasses BOTH the cache and the coalescing, so a
// caller that asked for no caching gets its own independent fetch.
// Fetch errors are never stored. Counters are real: hits (including
// waiters that joined an in-flight request), misses, size. There
// is no tenant in the key and no temperature (search results are
// not sampled outputs).
//
// WHAT THIS TYPE IS — AND IS NOT:
//   - Generic over the result type (SearchCache[T]): the search
//     seam exists now — POST /v1/search (M8.20,
//     internal/server/media_verify.go) — and this cache wires
//     into it (G13): the handler calls GetOrCoalesce around the
//     single Perplexity passthrough with the 60 s TTL, keyed by
//     the search parameters. The generic shape stays, because
//     the cached value is the verbatim upstream answer
//     (searchCachedResult), not a chat shape.
//   - Tenant scoping: the sector baseline puts NO tenant in the search key.
//     This type follows the sector baseline exactly — the key carries the
//     search parameters only — and documents the consequence:
//     search results are treated as public data (the same query
//     returns the same results for any caller), exactly as
//     the sector baseline treats them. If a future search seam serves
//     tenant-private results, the key must gain a tenant field
//     first (SearchKeyForTenant, mirroring
//     coalescing.RequestKeyForTenant) — do not wire a private
//     search source into this cache without it.
//   - Caps follow the M6.1 pattern (256 KiB per entry, 2 MiB
//     total). Entry size is the exact length of the value's JSON
//     encoding, marshalled once at store time; a value that
//     cannot marshal, or that exceeds the per-entry cap, is
//     refused, never truncated or approximated. Total-cap
//     eviction is FIFO (the sector-typical rule), after an expired-first
//     purge — not LRU: search freshness is time-ordered.
//   - Coalescing: internal/coalescing.Group does NOT fit here —
//     it is typed to provider.ChatResponse / routetrace.RouteTrace
//     (its Result carries *provider.ChatResponse), and a search
//     result is generic data; forcing search results through a
//     ChatResponse-typed flight would be a misuse, and changing
//     the Group's signature is outside this slice. The in-flight
//     map here mirrors the sector baseline searchCache.ts's own inline
//     `inflight` map (same single-flight semantics: leader
//     executes, waiters share, TTL<=0 bypasses both cache and
//     coalescing, failures are shared but never stored) — it is
//     the cache's own getOrCoalesce, not a second coalescer
//     package. If Group ever gains a generic flight, this map
//     should be replaced by it.
//   - Normalization gap, stated plainly: NFKC needs
//     golang.org/x/text/unicode/norm, which is NOT a module
//     dependency (go.mod carries no golang.org/x/text), and this
//     slice adds no new network dependency for one normalization
//     step. normalizeQuery therefore does lowercase + trim +
//     collapse-whitespace only. Two queries that differ only in
//     Unicode compatibility form (e.g. full-width vs ASCII) key
//     differently here and would have shared one entry under
//     the sector baseline — a missed-sharing case, never a wrong serve.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"
)

// DefaultSearchTTL is the lifetime of a search entry: 60 s,
// the sector-typical SEARCH_CACHE_DEFAULT_TTL_MS.
const DefaultSearchTTL = 60 * time.Second

// DefaultSearchMaxEntries caps the search cache: 500, the sector-typical
// MAX_CACHE_ENTRIES.
const DefaultSearchMaxEntries = 500

// DefaultSearchMaxEntryBytes is the per-entry cap (M6.1): a
// result whose JSON encoding exceeds 256 KiB is refused.
const DefaultSearchMaxEntryBytes = DefaultMaxEntryBytes

// DefaultSearchMaxTotalBytes is the total memory cap (M6.1):
// 2 MiB across all entries.
const DefaultSearchMaxTotalBytes = DefaultMaxTotalBytes

// normalizeQuery normalizes a search query for keying: lowercase,
// trim, collapse every whitespace run to one space. NFKC is NOT
// applied (see the package doc — no x/text dependency); two
// queries differing only in compatibility form key differently.
func normalizeQuery(query string) string {
	fields := strings.FieldsFunc(query, unicode.IsSpace)
	return strings.ToLower(strings.Join(fields, " "))
}

// SearchKey derives the cache key for a search: SHA-256 hex of
// the JSON encoding of {q: normalized query, p: provider,
// t: searchType, n: maxResults, c: country|null,
// l: language|null, f: filters|null}. Identical parameters always
// produce the same key; any difference produces a different key.
// There is deliberately NO tenant in the key (see the package
// doc): search results are treated as public data, exactly as
// the sector baseline treats them.
func SearchKey(query, provider, searchType string, maxResults int, country, language string, filters any) string {
	payload := struct {
		Q string `json:"q"`
		P string `json:"p"`
		T string `json:"t"`
		N int    `json:"n"`
		C any    `json:"c"`
		L any    `json:"l"`
		F any    `json:"f"`
	}{
		Q: normalizeQuery(query),
		P: provider,
		T: searchType,
		N: maxResults,
	}
	if country != "" {
		payload.C = country
	}
	if language != "" {
		payload.L = language
	}
	if filters != nil {
		payload.F = filters
	}
	b, err := json.Marshal(payload)
	if err != nil {
		// The payload shape cannot fail to marshal (filters is
		// the only caller-controlled any, and encoding/json
		// fails on it only for unencodable values such as a
		// channel or NaN); a failure must never yield a key
		// that could collide with a real entry, so return "".
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SearchStats is the cache's accounting: counts only, no result
// content, no keys, no queries. Size is live entries; Hits
// counts cache hits AND waiters that joined an in-flight request
// (the sector baseline counts both as hits); Misses counts lookups that
// found nothing in flight or in cache; Evicted counts entries
// dropped by the caps. InFlight is the number of fetches running
// right now. All counters start at zero and move only when real
// searches flow through the cache — never zero-filled.
type SearchStats struct {
	Size     int    `json:"size"`
	Bytes    int    `json:"bytes"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
	Evicted  uint64 `json:"evicted"`
	InFlight int    `json:"in_flight"`
}

// SearchCacheConfig tunes a SearchCache[T]. Zero values select
// the defaults (60 s TTL, 500 entries, 256 KiB per entry, 2 MiB
// total).
type SearchCacheConfig struct {
	TTL           time.Duration
	MaxEntries    int
	MaxEntryBytes int
	MaxTotalBytes int
}

type searchEntry[T any] struct {
	value   T
	size    int // len(JSON(value)), exact
	expires time.Time
	created time.Time
}

type searchFlight[T any] struct {
	done  chan struct{}
	value T
	err   error
}

// SearchCache is a concurrency-safe in-memory TTL cache for search
// results with request coalescing: concurrent identical queries
// share one upstream fetch. See the package doc for the semantics,
// the caps, and the honest limits (no tenant in the key, no NFKC,
// unwired until a search seam exists).
type SearchCache[T any] struct {
	mu            sync.Mutex
	ttl           time.Duration
	maxEntries    int
	maxEntryBytes int
	maxTotalBytes int
	totalBytes    int
	items         map[string]*searchEntry[T]
	order         []string // insertion order, oldest first (FIFO eviction)
	inflight      map[string]*searchFlight[T]
	hits          uint64
	misses        uint64
	evicted       uint64
}

// NewSearchCache returns a SearchCache[T] with the defaults:
// 60 s TTL, 500 entries, 256 KiB per entry, 2 MiB total.
func NewSearchCache[T any]() *SearchCache[T] {
	return NewSearchCacheWithConfig[T](SearchCacheConfig{})
}

// NewSearchCacheWithConfig returns a SearchCache[T] tuned by cfg;
// zero fields select the defaults.
func NewSearchCacheWithConfig[T any](cfg SearchCacheConfig) *SearchCache[T] {
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultSearchTTL
	}
	maxEntries := cfg.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultSearchMaxEntries
	}
	maxEntryBytes := cfg.MaxEntryBytes
	if maxEntryBytes <= 0 {
		maxEntryBytes = DefaultSearchMaxEntryBytes
	}
	maxTotalBytes := cfg.MaxTotalBytes
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultSearchMaxTotalBytes
	}
	return &SearchCache[T]{
		ttl:           ttl,
		maxEntries:    maxEntries,
		maxEntryBytes: maxEntryBytes,
		maxTotalBytes: maxTotalBytes,
		items:         make(map[string]*searchEntry[T], maxEntries),
		order:         make([]string, 0, maxEntries),
		inflight:      make(map[string]*searchFlight[T]),
	}
}

// Get returns the cached result for key, and whether it was
// found. An expired entry counts as a miss and is deleted. An
// empty key always misses.
func (c *SearchCache[T]) Get(ctx context.Context, key string) (T, bool) {
	var zero T
	if key == "" {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		c.misses++
		return zero, false
	}
	if time.Now().After(e.expires) {
		c.removeLocked(key)
		c.misses++
		return zero, false
	}
	c.hits++
	return e.value, true
}

// Set stores value under key for the cache's TTL. It stores
// nothing when the key is empty, when the value cannot be JSON
// encoded (its size is then unmeasurable — refusing is honest,
// guessing is not), or when the encoding exceeds the per-entry
// byte cap. A repeat Set for the same key replaces the entry in
// place (refreshed expiry; insertion order unchanged — FIFO
// eviction keys on first insertion, so a refresh must not
// promote an entry). Expired entries are purged first, then FIFO
// eviction runs while the entry count or the total bytes would
// exceed the caps.
func (c *SearchCache[T]) Set(ctx context.Context, key string, value T) {
	c.SetWithTTL(ctx, key, value, c.ttl)
}

// SetWithTTL is Set with an explicit TTL. A ttl <= 0 stores
// nothing.
func (c *SearchCache[T]) SetWithTTL(ctx context.Context, key string, value T, ttl time.Duration) {
	if key == "" || ttl <= 0 {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	size := len(b)
	if size > c.maxEntryBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if old, ok := c.items[key]; ok {
		c.totalBytes -= old.size
		c.items[key] = &searchEntry[T]{value: value, size: size, expires: now.Add(ttl), created: old.created}
		c.totalBytes += size
		return
	}
	c.purgeExpiredLocked(now)
	for len(c.items) >= c.maxEntries || c.totalBytes+size > c.maxTotalBytes {
		if !c.evictFIFOLocked() {
			break
		}
	}
	c.items[key] = &searchEntry[T]{value: value, size: size, expires: now.Add(ttl), created: now}
	c.order = append(c.order, key)
	c.totalBytes += size
}

// GetOrCoalesce returns the cached result for key, joins an
// in-flight identical fetch, or executes fetch and stores its
// result. The second return value reports whether the result
// came from the cache or a shared flight (true) or from this
// caller's own fresh fetch (false).
//
//   - ttl <= 0 bypasses BOTH the cache and the coalescing
//     (the sector-typical rule): every concurrent call gets its own
//     independent fetch, and the result is never stored.
//   - A cache hit returns (value, true, nil) and counts as a hit.
//   - A waiter that joins an in-flight fetch shares the leader's
//     outcome — success or failure — counts as a hit, and returns
//     cached=true. A waiter whose own context ends first leaves
//     with its context error and does not disturb the leader.
//   - The leader's fetch runs outside the lock. On success the
//     result is stored (subject to the caps) and returned as
//     (value, false, nil). On failure nothing is stored — a
//     failed search is never cached as an answer — and the error
//     reaches the leader and every waiter unchanged.
func (c *SearchCache[T]) GetOrCoalesce(ctx context.Context, key string, ttl time.Duration, fetch func(context.Context) (T, error)) (T, bool, error) {
	var zero T
	if fetch == nil {
		return zero, false, nil
	}
	if ttl <= 0 {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		v, err := fetch(ctx)
		return v, false, err
	}
	if key == "" {
		c.mu.Lock()
		c.misses++
		c.mu.Unlock()
		v, err := fetch(ctx)
		return v, false, err
	}
	c.mu.Lock()
	if e, ok := c.items[key]; ok {
		if time.Now().After(e.expires) {
			c.removeLocked(key)
		} else {
			c.hits++
			v := e.value
			c.mu.Unlock()
			return v, true, nil
		}
	}
	if f, ok := c.inflight[key]; ok {
		c.hits++
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return zero, false, ctx.Err()
		case <-f.done:
			return f.value, true, f.err
		}
	}
	f := &searchFlight[T]{done: make(chan struct{})}
	c.inflight[key] = f
	c.misses++
	c.mu.Unlock()

	v, err := fetch(ctx)
	c.mu.Lock()
	f.value, f.err = v, err
	close(f.done)
	delete(c.inflight, key)
	c.mu.Unlock()
	if err != nil {
		return zero, false, err
	}
	c.SetWithTTL(ctx, key, v, ttl)
	return v, false, nil
}

// Delete removes key if present.
func (c *SearchCache[T]) Delete(ctx context.Context, key string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeLocked(key)
}

// FlushAll removes every entry and returns how many were removed.
// In-flight fetches are untouched: a waiter still receives its
// leader's result, which is then stored under the (now empty)
// cache as usual.
func (c *SearchCache[T]) FlushAll() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(c.items)
	c.items = make(map[string]*searchEntry[T], c.maxEntries)
	c.order = c.order[:0]
	c.totalBytes = 0
	return n
}

// Stats returns the live accounting snapshot.
func (c *SearchCache[T]) Stats() SearchStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return SearchStats{
		Size:     len(c.items),
		Bytes:    c.totalBytes,
		Hits:     c.hits,
		Misses:   c.misses,
		Evicted:  c.evicted,
		InFlight: len(c.inflight),
	}
}

// Size returns the number of entries currently stored (including
// any not-yet-expired entries that have passed their TTL but not
// been accessed since).
func (c *SearchCache[T]) Size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items)
}

// purgeExpiredLocked deletes every entry past its TTL. Caller
// holds mu. It runs on writes only (the sector-typical evictIfNeeded
// shape): reads delete lazily on access.
func (c *SearchCache[T]) purgeExpiredLocked(now time.Time) {
	for key, e := range c.items {
		if now.After(e.expires) {
			c.removeLocked(key)
		}
	}
}

// evictFIFOLocked drops the oldest-inserted entry. Caller holds
// mu. It returns false when there is nothing to evict.
func (c *SearchCache[T]) evictFIFOLocked() bool {
	for len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if e, ok := c.items[oldest]; ok {
			c.totalBytes -= e.size
			delete(c.items, oldest)
			c.evicted++
			return true
		}
	}
	return false
}

// removeLocked deletes key's entry and its insertion-order
// position, accounting its bytes out. Caller holds mu.
func (c *SearchCache[T]) removeLocked(key string) {
	e, ok := c.items[key]
	if !ok {
		return
	}
	c.totalBytes -= e.size
	delete(c.items, key)
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
