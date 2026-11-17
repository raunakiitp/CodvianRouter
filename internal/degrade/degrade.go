// Package degrade implements graceful degradation for Codvian Router:
// when every provider fails, serve a stale exact-match cached response
// instead of a bare error — but ONLY when explicitly enabled, and ONLY
// with a visible staleness marker, never silently.
//
// Honesty warning — read before enabling this: a stale response is a
// copy of an answer that was correct (or at least, that a provider
// produced) at the time it was cached. It can be outdated, wrong, or
// misleading by the time it is served: facts change, prices change,
// code advice goes stale. Serving it is a deliberate trade-off —
// "something plausible, clearly marked stale" beats "no answer at
// all" only when the caller knows it is stale and decides that is
// acceptable. This package therefore:
//
//   - is OFF by default (DefaultPolicy has Enabled=false);
//   - never claims freshness: the server.Engine contract has no
//     staleness field, so staleness is exposed out-of-band via
//     StaleEngine.LastServedStale / LastStaleAge, which the coordinator
//     must surface to the caller (for example as a response header or
//     a log line) — the wrapper itself cannot put the marker in the
//     response body, and it does not alter the cached content to
//     pretend otherwise;
//   - never serves an entry older than Policy.MaxStaleAge when that
//     limit is set (> 0);
//   - never replays a stale response over a partially delivered
//     stream (that would duplicate content the client already has).
//
// The wrapper is backend only. It stores successful responses in the
// exact-match cache (cache.ExactCache, keyed by
// cache.ExactKey(model, messages), TTL DefaultStoreTTL) and falls back
// to that cache on failure. It does not invent responses, does not
// call providers itself, and does not log message content.
package degrade

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// DefaultStoreTTL is how long a successful response stays in the
// exact-match cache when stored by a StaleEngine (1 hour). It bounds
// how old a cached entry can ever be; Policy.MaxStaleAge can only
// narrow that window further, never widen it.
const DefaultStoreTTL = time.Hour

// Policy controls graceful degradation.
//
// To enable degradation, start from DefaultPolicy and set Enabled to
// true. DefaultPolicy already sets AllowStale to true, so flipping
// Enabled alone is sufficient when starting from it. Both flags must
// be true for a stale response to be served: Enabled is the master
// switch for the feature, AllowStale is the explicit permission to
// hand out stale content. Setting either to false disables serving
// stale responses (successful responses are still cached either way,
// so enabling later can serve entries cached earlier).
type Policy struct {
	// Enabled turns degradation on. When false, the wrapper is a
	// pass-through that still caches successes but never serves a
	// stale response on failure.
	Enabled bool
	// MaxStaleAge is the oldest a cached entry may be and still be
	// served. 0 means no age limit once cached (other than the
	// 1h store TTL in the underlying cache). When > 0, entries
	// older than this are never served.
	MaxStaleAge time.Duration
	// AllowStale is the explicit permission to serve stale content.
	// Both Enabled and AllowStale must be true for a stale
	// response to be served.
	AllowStale bool
	// ServeCache turns proactive cache serving on: for a cacheable
	// request (see cacheable) the wrapper first consults the
	// exact-match cache and then the semantic cache, and serves a
	// hit WITHOUT calling any provider. A proactive hit is marked
	// via LastCacheHit — never as stale — and is not gated by
	// Enabled/AllowStale, because a within-TTL cache hit is not
	// degradation: the answer was produced by a provider for an
	// (almost) identical prompt and is still fresh. Default false,
	// so existing callers keep the old behaviour (cache only as a
	// failure fallback) unless they opt in explicitly.
	ServeCache bool
}

// DefaultPolicy returns the safe default: degradation OFF
// (Enabled=false). AllowStale is true so that enabling the feature is
// a single-field change (set Enabled=true); MaxStaleAge is 0, i.e.
// no age limit beyond the cache's own TTL.
func DefaultPolicy() Policy {
	return Policy{Enabled: false, MaxStaleAge: 0, AllowStale: true}
}

// staleAllowed reports whether a stale response may be served under p.
func (p Policy) staleAllowed() bool { return p.Enabled && p.AllowStale }

// StaleEngine is a server.Engine wrapper that caches successful
// responses in an exact-match cache and, on failure, serves a stale
// cached response when the policy allows it. See the package doc for
// the honesty rules.
//
// It is safe for concurrent use: the last-outcome fields and the
// store timestamps are guarded by a mutex. The wrapped engine and
// the cache are expected to be safe for concurrent use as well
// (server.Engine implementations and cache.ExactCache both are).
//
// StaleEngine does implement server.StreamResumer: ChatStreamFrom
// forwards to the wrapped engine when it supports resume, and falls
// back to a full-stream replay with rune-accurate trimming when it
// does not. A stale replay never follows a partial resume.
// CacheMetrics receives cache hit/miss counts by layer ("exact",
// "semantic"). metrics.Recorder satisfies it; a nil value disables
// reporting. It is a separate interface so this package does not
// import internal/metrics.
type CacheMetrics interface {
	IncCacheHit(layer string)
	IncCacheMiss(layer string)
}

type StaleEngine struct {
	engine       server.Engine
	exact        *cache.ExactCache
	semantic     *cache.LocalSemanticCache
	policy       Policy
	now          func() time.Time
	mu           sync.Mutex
	cacheMetrics CacheMetrics
	lastStale    bool
	// lastCacheKind is "exact" or "semantic" when the most recent
	// call was served proactively from a cache, "" otherwise.
	// lastCacheSimilarity is the semantic similarity of that hit
	// (0 for exact hits). A stale serve is not a cache hit: it
	// sets lastStale and clears these fields.
	lastCacheKind       string
	lastCacheSimilarity float32
	// lastStaleAge is the age of the entry served by the most recent
	// call, when that call served stale content; 0 otherwise. When
	// the entry's store time is unknown (it was cached by someone
	// other than this wrapper) and MaxStaleAge is 0, the served age
	// is reported as 0, meaning "unknown", not "fresh".
	lastStaleAge time.Duration
	// storedAt records when this wrapper stored each cache key, so
	// MaxStaleAge can be enforced: cache.ExactCache does not expose
	// entry ages. Entries are dropped when the cache reports a miss
	// or when they expire, so the map stays roughly bounded by the
	// cache size.
	storedAt map[string]time.Time
}

// Wrap returns a server.Engine that caches successes and degrades to
// stale cached responses on failure, per policy. The returned value's
// dynamic type is *StaleEngine, so callers can use
// LastServedStale/LastStaleAge to learn whether the last call served
// stale content.
//
// A nil engine returns nil. A nil exact cache is tolerated: successes
// are returned as-is and failures have nothing to fall back to, so
// the original error is returned unchanged.
func Wrap(engine server.Engine, exact *cache.ExactCache, policy Policy) server.Engine {
	return WrapWithSemantic(engine, exact, nil, policy)
}

// WrapWithSemantic is Wrap plus the semantic cache layer: successful
// responses are stored in both the exact-match cache and the semantic
// cache, and when Policy.ServeCache is true, cacheable requests are
// served proactively from exact first, then semantic. A nil semantic
// cache disables the semantic layer (exact behaviour unchanged).
func WrapWithSemantic(engine server.Engine, exact *cache.ExactCache, semantic *cache.LocalSemanticCache, policy Policy) server.Engine {
	if engine == nil {
		return nil
	}
	return &StaleEngine{
		engine:   engine,
		exact:    exact,
		semantic: semantic,
		policy:   policy,
		now:      time.Now,
		storedAt: make(map[string]time.Time),
	}
}

// SetCacheMetrics wires hit/miss reporting (e.g. *metrics.Recorder).
// A nil value disables reporting.
func (e *StaleEngine) SetCacheMetrics(m CacheMetrics) {
	e.mu.Lock()
	e.cacheMetrics = m
	e.mu.Unlock()
}

// LastCacheHit reports whether the most recent Chat or ChatStream call
// on this wrapper was served proactively from a cache without calling
// any provider: kind is "exact" or "semantic", similarity is the
// semantic cosine similarity (0 for exact hits), and ok is true.
// It is false after a fresh provider success, after a failure, after
// a stale serve, and before any call. Callers (the coordinator /
// server) must surface this to the client — e.g. as an
// X-Codvian-Cache header — because the response itself carries no
// marker and must never be mistaken for a fresh provider response.
func (e *StaleEngine) LastCacheHit() (kind string, similarity float32, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastCacheKind == "" {
		return "", 0, false
	}
	return e.lastCacheKind, e.lastCacheSimilarity, true
}

// setCacheOutcome records a proactive cache hit. Caller must not hold
// e.mu (it takes it). It also clears the stale flag: a cache hit is
// fresh, not stale.
func (e *StaleEngine) setCacheOutcome(kind string, similarity float32) {
	e.mu.Lock()
	e.lastCacheKind = kind
	e.lastCacheSimilarity = similarity
	e.lastStale = false
	e.lastStaleAge = 0
	e.mu.Unlock()
}

// LastServedStale reports whether the most recent Chat or ChatStream
// call on this wrapper served a stale cached response. It is false
// after a fresh success, after a failure with no stale fallback, and
// before any call. Callers (the coordinator / server) must surface
// this to the client — the response itself carries no staleness
// marker, because server.Engine has no field for one.
func (e *StaleEngine) LastServedStale() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastStale
}

// LastStaleAge returns the age of the entry served by the most recent
// call, when that call served stale content; 0 otherwise. An age of 0
// alongside LastServedStale()==true means the store time was unknown
// (the entry was cached by someone other than this wrapper and
// MaxStaleAge is 0) — treat it as "unknown age", never as "fresh".
func (e *StaleEngine) LastStaleAge() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastStaleAge
}

// setOutcome records the outcome of a call. Caller must not hold e.mu
// (setOutcome takes it). It also clears any previous cache-hit record:
// a fresh success, a failure, or a stale serve is not a proactive
// cache hit.
func (e *StaleEngine) setOutcome(stale bool, age time.Duration) {
	e.mu.Lock()
	e.lastStale = stale
	e.lastStaleAge = age
	e.lastCacheKind = ""
	e.lastCacheSimilarity = 0
	e.mu.Unlock()
}

// cacheable reports whether req may be served from, or stored into,
// the caches. Tool-using requests are excluded: a cached entry stores
// content only, so serving it would silently drop the tool calls the
// caller asked for, and a tool result in the history makes a semantic
// text match meaningless (the answer depends on the tool output, not
// just the prompt words).
func cacheable(req provider.ChatRequest) bool {
	if len(req.Tools) > 0 || len(req.ToolChoice) > 0 {
		return false
	}
	for _, m := range req.Messages {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			return false
		}
	}
	return true
}

// semanticText is the text the semantic cache matches on: the last
// user message's text (Content, or its text parts when sent as an
// array). "" when there is no user message with text.
func semanticText(req provider.ChatRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		if m.Content != "" {
			return m.Content
		}
		var sb strings.Builder
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteString(" ")
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// reportHit records a cache hit with the metrics sink, if any.
func (e *StaleEngine) reportHit(layer string) {
	e.mu.Lock()
	m := e.cacheMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncCacheHit(layer)
	}
}

// reportMiss records a cache miss with the metrics sink, if any.
func (e *StaleEngine) reportMiss(layer string) {
	e.mu.Lock()
	m := e.cacheMetrics
	e.mu.Unlock()
	if m != nil {
		m.IncCacheMiss(layer)
	}
}

// lookup consults the exact-match cache first, then the semantic
// cache, for a cacheable request under a ServeCache policy. It
// returns the cached response, the hit kind ("exact" or "semantic"),
// the semantic similarity (0 for exact hits), and true on a hit.
// Misses are reported per layer consulted. It never calls a provider
// and never serves a hit for a non-cacheable request or when
// ServeCache is off.
func (e *StaleEngine) lookup(ctx context.Context, req provider.ChatRequest) (cache.CachedResponse, string, float32, bool) {
	if !e.policy.ServeCache || !cacheable(req) {
		return cache.CachedResponse{}, "", 0, false
	}
	if e.exact != nil {
		key, err := cache.ExactKey(req.Model, req.Messages)
		if err == nil {
			if hit, found := e.exact.Get(ctx, key); found {
				e.reportHit("exact")
				return hit, "exact", 0, true
			}
			e.reportMiss("exact")
		}
	}
	if e.semantic != nil {
		if text := semanticText(req); text != "" {
			if hit, found := e.semantic.GetText(ctx, text); found {
				e.reportHit("semantic")
				return hit.Response, "semantic", hit.Similarity, true
			}
			e.reportMiss("semantic")
		}
	}
	return cache.CachedResponse{}, "", 0, false
}

// store caches a successful response and records its store time.
// Failures to derive a key or to store are silently ignored: caching
// is best-effort and must never break a successful response.
// Non-cacheable requests (tool-using) are never stored: a cached
// entry holds content only, so storing one would let a later lookup
// silently drop tool calls. Cacheable successes are stored in the
// exact cache under their exact key and, when a semantic cache is
// configured, in the semantic cache under the last user message's
// text — same 1h TTL and LRU eviction rules as the exact cache, so
// neither layer can serve an entry the other has aged out of.
func (e *StaleEngine) store(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse) {
	if resp == nil || !cacheable(req) {
		return
	}
	entry := cache.CachedResponse{
		Content: resp.Content,
		Usage: cache.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		Model: resp.Model,
	}
	if e.exact != nil {
		key, err := cache.ExactKey(req.Model, req.Messages)
		if err == nil {
			e.exact.Set(ctx, key, entry, DefaultStoreTTL)
			e.mu.Lock()
			e.storedAt[key] = e.now()
			e.mu.Unlock()
		}
	}
	if e.semantic != nil {
		if text := semanticText(req); text != "" {
			e.semantic.SetText(ctx, text, cache.SemanticEntry{Response: entry}, DefaultStoreTTL)
		}
	}
}

// staleFor returns the cached response for req if one exists and is
// young enough under the policy, with its age. The boolean is false
// on a miss, on an entry too old for MaxStaleAge, and on an entry
// whose age cannot be verified when MaxStaleAge > 0. A nil exact
// cache always misses.
//
// Note: an entry cached by someone other than this wrapper has no
// recorded store time. With MaxStaleAge == 0 it is still served
// (age reported as 0 = unknown); with MaxStaleAge > 0 it is never
// served, because its age cannot be proven to be within the limit.
func (e *StaleEngine) staleFor(ctx context.Context, req provider.ChatRequest) (cached cache.CachedResponse, age time.Duration, ok bool) {
	if e.exact == nil || !e.policy.staleAllowed() {
		return cache.CachedResponse{}, 0, false
	}
	if !cacheable(req) {
		// A cached entry holds content only. Serving it for a
		// tool-using request would silently drop the tool calls
		// the caller asked for — same reason proactive lookup and
		// store skip such requests.
		return cache.CachedResponse{}, 0, false
	}
	key, err := cache.ExactKey(req.Model, req.Messages)
	if err != nil {
		return cache.CachedResponse{}, 0, false
	}
	hit, found := e.exact.Get(ctx, key)
	if !found {
		e.mu.Lock()
		delete(e.storedAt, key)
		e.mu.Unlock()
		return cache.CachedResponse{}, 0, false
	}
	e.mu.Lock()
	stored, known := e.storedAt[key]
	e.mu.Unlock()
	now := e.now()
	if !known {
		if e.policy.MaxStaleAge > 0 {
			// Age unverifiable: never serve under an age limit.
			return cache.CachedResponse{}, 0, false
		}
		return hit, 0, true
	}
	age = now.Sub(stored)
	if age < 0 {
		age = 0
	}
	if e.policy.MaxStaleAge > 0 && age > e.policy.MaxStaleAge {
		e.mu.Lock()
		delete(e.storedAt, key)
		e.mu.Unlock()
		return cache.CachedResponse{}, 0, false
	}
	return hit, age, true
}

// toChatResponse converts a cached entry to a provider response.
// The Model is the cached model when present, otherwise the request
// model (a cached entry with no model recorded is still the answer
// for this request's model). Content is returned unchanged: the
// wrapper must not edit cached content to add a staleness marker,
// because callers may hash or compare it.
func toChatResponse(req provider.ChatRequest, cached cache.CachedResponse) *provider.ChatResponse {
	model := cached.Model
	if model == "" {
		model = req.Model
	}
	return &provider.ChatResponse{
		Content: cached.Content,
		Usage: provider.Usage{
			PromptTokens:     cached.Usage.PromptTokens,
			CompletionTokens: cached.Usage.CompletionTokens,
			TotalTokens:      cached.Usage.TotalTokens,
		},
		Model: model,
	}
}

// Chat calls the wrapped engine. On success it stores the response
// in the exact cache and returns it. On error, if the policy allows
// stale serving and an exact-match cached entry exists and is young
// enough, it returns that cached response instead (with
// LastServedStale()==true) — the content may be outdated, see the
// package doc. Otherwise the original error is returned unchanged:
// this wrapper never wraps, replaces, or annotates the upstream
// error, so callers see exactly what the engine reported.
func (e *StaleEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	// Proactive cache serving: a hit returns the cached response
	// without calling any provider, so no provider is billed and
	// the ledger (which the engine feeds) records nothing for this
	// request. The hit is marked via LastCacheHit so the server can
	// label it honestly; it is never marked stale. The hit is also
	// stored under this request's exact key, so a paraphrase that
	// hit semantically becomes an exact hit next time.
	if cached, kind, sim, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		e.store(ctx, req, resp)
		e.setCacheOutcome(kind, sim)
		return resp, nil
	}
	resp, err := e.engine.Chat(ctx, req)
	if err == nil {
		e.store(ctx, req, resp)
		e.setOutcome(false, 0)
		return resp, nil
	}
	cached, age, ok := e.staleFor(ctx, req)
	if !ok {
		e.setOutcome(false, 0)
		return nil, err
	}
	e.setOutcome(true, age)
	return toChatResponse(req, cached), nil
}

// ChatStream calls the wrapped engine, forwarding deltas. On success
// it stores the completed response and returns it. On error:
//   - if any delta was already delivered, the original error is
//     returned unchanged — replaying the full cached response would
//     duplicate content the client already has;
//   - if zero deltas were delivered and a young enough cached entry
//     exists under an allowing policy, the cached full response is
//     delivered as a single delta (a single-chunk replay) and
//     returned as the completed response, with
//     LastServedStale()==true. The replayed content is stale and may
//     be outdated, see the package doc.
//   - otherwise the original error is returned unchanged.
//
// If onDelta is nil it is treated as a no-op. If onDelta returns an
// error while replaying the stale chunk, that error is returned (the
// client stopped the stream; the original engine error is not
// recoverable in that case either).
func (e *StaleEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	// Proactive cache serving for streams: a hit is replayed as a
	// single delta, exactly like a stale replay, and no provider is
	// called. A stream that already delivered content never reaches
	// this path (lookup happens before any delta), so nothing can
	// be duplicated.
	if cached, kind, sim, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		if cached.Content != "" {
			if err := onDelta(cached.Content); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.setCacheOutcome(kind, sim)
		return resp, nil
	}
	delivered := 0
	counting := func(delta string) error {
		if err := onDelta(delta); err != nil {
			return err
		}
		if delta != "" {
			delivered++
		}
		return nil
	}
	resp, err := e.engine.ChatStream(ctx, req, counting)
	if err == nil {
		e.store(ctx, req, resp)
		e.setOutcome(false, 0)
		return resp, nil
	}
	if delivered > 0 {
		// Partial content already reached the caller: never replay.
		e.setOutcome(false, 0)
		return nil, err
	}
	cached, age, ok := e.staleFor(ctx, req)
	if !ok {
		e.setOutcome(false, 0)
		return nil, err
	}
	// Zero deltas delivered: replay the cached full response as one
	// chunk. An empty cached content needs no delta at all.
	if cached.Content != "" {
		if err := onDelta(cached.Content); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	e.setOutcome(true, age)
	return toChatResponse(req, cached), nil
}

// ChatStreamFrom resumes a stream from skipChars runes in.
//
// Proactive cache lookup is deliberately skipped here: a resume means
// the client already holds a prefix, so serving a cached full
// response would duplicate content. The stale fallback below is the
// only cache path on a resume, exactly as before.
//
// If the wrapped engine implements server.StreamResumer, the resume
// is delegated to it (a success is stored like any other; a failure
// is returned unchanged — a stale replay must never follow a resume,
// because content may already have been delivered to the client).
//
// Otherwise the wrapper replays via ChatStream and trims the first
// skipChars runes itself (the server also trims overlap as a safety
// net, so a resume that ignores skipChars still cannot duplicate
// content on the wire). On failure after zero deltas were delivered,
// a young enough cached entry is replayed as a single delta, trimmed
// the same way; after any delivered delta the original error is
// returned unchanged, exactly like ChatStream.
func (e *StaleEngine) ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	if resumer, ok := e.engine.(server.StreamResumer); ok {
		resp, err := resumer.ChatStreamFrom(ctx, req, skipChars, onDelta)
		if err == nil {
			e.store(ctx, req, resp)
			e.setOutcome(false, 0)
			return resp, nil
		}
		e.setOutcome(false, 0)
		return nil, err
	}
	remaining := skipChars
	if remaining < 0 {
		remaining = 0
	}
	delivered := 0
	trimming := func(delta string) error {
		if delta == "" {
			return onDelta(delta)
		}
		if remaining > 0 {
			runes := []rune(delta)
			if len(runes) <= remaining {
				remaining -= len(runes)
				return nil
			}
			delta = string(runes[remaining:])
			remaining = 0
		}
		if err := onDelta(delta); err != nil {
			return err
		}
		delivered++
		return nil
	}
	resp, err := e.engine.ChatStream(ctx, req, trimming)
	if err == nil {
		e.store(ctx, req, resp)
		e.setOutcome(false, 0)
		return resp, nil
	}
	if delivered > 0 {
		e.setOutcome(false, 0)
		return nil, err
	}
	cached, age, ok := e.staleFor(ctx, req)
	if !ok {
		e.setOutcome(false, 0)
		return nil, err
	}
	if cached.Content != "" {
		if err := trimming(cached.Content); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	e.setOutcome(true, age)
	return toChatResponse(req, cached), nil
}

// chunkStreamer / chunkResumer mirror server's chunk interfaces
// (unexported there). Any engine that implements them streams
// tool-call deltas and finish reasons, not just text.
type chunkStreamer interface {
	ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}
type chunkResumer interface {
	ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

// ChatStreamChunks is the chunk-aware twin of ChatStream, with the
// same stale rules: a success is stored; on error after any chunk
// was delivered the error is returned unchanged; on error before
// any chunk, a young enough cached entry is replayed as a single
// text chunk. The wrapped engine's chunk path is preferred; a
// text-only wrapped engine is bridged by converting its deltas to
// chunks. Without this method the server's chunk-aware path would
// silently fall back to text-only streaming for every request that
// passes through this wrapper — which is every request in serve
// mode — and tool-call deltas would never reach the client.
//
// Limitation: a stale replay carries text only. The exact-match
// cache stores content, not tool calls, so a stale stream cannot
// reconstruct the tool calls of the original response.
func (e *StaleEngine) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if onChunk == nil {
		onChunk = func(provider.StreamChunk) error { return nil }
	}
	// Proactive cache serving for chunk streams: a hit is replayed
	// as a single text chunk (tool calls cannot be reconstructed
	// from a content-only cached entry — same limitation as a stale
	// replay — and cacheable requests carry no tools anyway).
	if cached, kind, sim, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		if cached.Content != "" {
			if err := onChunk(provider.StreamChunk{Content: cached.Content}); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.setCacheOutcome(kind, sim)
		return resp, nil
	}
	delivered := 0
	counting := func(c provider.StreamChunk) error {
		if err := onChunk(c); err != nil {
			return err
		}
		if c.Content != "" || len(c.ToolCalls) > 0 || c.FinishReason != "" {
			delivered++
		}
		return nil
	}
	var stream func(func(provider.StreamChunk) error) (*provider.ChatResponse, error)
	if cs, ok := e.engine.(chunkStreamer); ok {
		stream = func(onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
			return cs.ChatStreamChunks(ctx, req, onChunk)
		}
	} else {
		stream = func(onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
			return e.engine.ChatStream(ctx, req, func(delta string) error {
				return onChunk(provider.StreamChunk{Content: delta})
			})
		}
	}
	resp, err := stream(counting)
	if err == nil {
		e.store(ctx, req, resp)
		e.setOutcome(false, 0)
		return resp, nil
	}
	if delivered > 0 {
		e.setOutcome(false, 0)
		return nil, err
	}
	cached, age, ok := e.staleFor(ctx, req)
	if !ok {
		e.setOutcome(false, 0)
		return nil, err
	}
	if cached.Content != "" {
		if err := onChunk(provider.StreamChunk{Content: cached.Content}); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	e.setOutcome(true, age)
	return toChatResponse(req, cached), nil
}

// ChatStreamFromChunks is the chunk-aware twin of ChatStreamFrom.
// If the wrapped engine implements chunk resume, the resume is
// delegated to it (a failure is returned unchanged — a stale replay
// must never follow a resume). Otherwise the wrapper replays via
// ChatStreamChunks and trims the first skipChars runes itself.
// Tool-call chunks that fall inside the skipped prefix are dropped,
// matching the router's documented resume limitation (only the text
// prefix is continued); the returned response still carries the
// full reassembled tool calls.
func (e *StaleEngine) ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if onChunk == nil {
		onChunk = func(provider.StreamChunk) error { return nil }
	}
	if cr, ok := e.engine.(chunkResumer); ok {
		resp, err := cr.ChatStreamFromChunks(ctx, req, skipChars, onChunk)
		if err == nil {
			e.store(ctx, req, resp)
			e.setOutcome(false, 0)
			return resp, nil
		}
		e.setOutcome(false, 0)
		return nil, err
	}
	remaining := skipChars
	if remaining < 0 {
		remaining = 0
	}
	delivered := 0
	trimming := func(c provider.StreamChunk) error {
		if remaining > 0 {
			if c.Content != "" {
				runes := []rune(c.Content)
				if len(runes) <= remaining {
					remaining -= len(runes)
					// Whole chunk was already sent; any
					// tool-call fragments in it belong to
					// the prefix too.
					return nil
				}
				c.Content = string(runes[remaining:])
				remaining = 0
			} else if len(c.ToolCalls) > 0 || c.FinishReason != "" {
				// No text to measure against the skip point:
				// drop, like the router's resume does.
				return nil
			}
		}
		if c.Content == "" && len(c.ToolCalls) == 0 && c.FinishReason == "" {
			return nil
		}
		if err := onChunk(c); err != nil {
			return err
		}
		delivered++
		return nil
	}
	resp, err := e.chatStreamChunksInner(ctx, req, trimming)
	if err == nil {
		e.store(ctx, req, resp)
		e.setOutcome(false, 0)
		return resp, nil
	}
	if delivered > 0 {
		e.setOutcome(false, 0)
		return nil, err
	}
	cached, age, ok := e.staleFor(ctx, req)
	if !ok {
		e.setOutcome(false, 0)
		return nil, err
	}
	if cached.Content != "" {
		if err := trimming(provider.StreamChunk{Content: cached.Content}); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	e.setOutcome(true, age)
	return toChatResponse(req, cached), nil
}

// chatStreamChunksInner runs one chunk stream against the wrapped
// engine (chunk path when it has one, text deltas bridged to chunks
// otherwise), without any stale handling — shared by
// ChatStreamChunks and the ChatStreamFromChunks replay fallback, so
// stale logic is applied exactly once per call.
func (e *StaleEngine) chatStreamChunksInner(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if cs, ok := e.engine.(chunkStreamer); ok {
		return cs.ChatStreamChunks(ctx, req, onChunk)
	}
	return e.engine.ChatStream(ctx, req, func(delta string) error {
		return onChunk(provider.StreamChunk{Content: delta})
	})
}

// LastTrace returns the wrapped engine's route trace when it exposes
// one (router.Engine does), so callers can attribute requests and
// count failovers through this wrapper. It returns nil when the
// wrapped engine has no trace to offer.
func (e *StaleEngine) LastTrace() *routetrace.RouteTrace {
	if tp, ok := e.engine.(interface{ LastTrace() *routetrace.RouteTrace }); ok {
		return tp.LastTrace()
	}
	return nil
}

// LastAutoDecision returns the wrapped engine's most recent
// auto-routing decision when it exposes one (router.Engine does),
// so callers can show which tier a real "auto/*" request picked,
// with what confidence and why. It returns ok=false when the
// wrapped engine has no decision to offer (no auto engine, or the
// most recent request was not an auto-routed one).
func (e *StaleEngine) LastAutoDecision() (autorouter.Decision, bool) {
	if ad, ok := e.engine.(interface {
		LastAutoDecision() (autorouter.Decision, bool)
	}); ok {
		return ad.LastAutoDecision()
	}
	return autorouter.Decision{}, false
}

// CoalescingStats returns the wrapped engine's single-flight
// counters when it exposes them (router.Engine does), so the
// dashboard can show real coalescing numbers through this wrapper.
// It returns zero counters when the wrapped engine has none to
// offer — an empty state, never invented figures.
func (e *StaleEngine) CoalescingStats() coalescing.Stats {
	if cs, ok := e.engine.(interface{ CoalescingStats() coalescing.Stats }); ok {
		return cs.CoalescingStats()
	}
	return coalescing.Stats{}
}

// ConcurrencyStats returns the wrapped engine's per-(provider, key)
// adaptive limiter state when it exposes it (router.Engine does),
// so the dashboard can show live caps, in-flight counts and RTT vs
// baseline through this wrapper. It returns an empty list when the
// wrapped engine has none to offer — an empty state, never invented
// limits.
func (e *StaleEngine) ConcurrencyStats() []adaptive.State {
	if cs, ok := e.engine.(interface{ ConcurrencyStats() []adaptive.State }); ok {
		return cs.ConcurrencyStats()
	}
	return []adaptive.State{}
}

// Ensure StaleEngine satisfies server.Engine at compile time.
var _ server.Engine = (*StaleEngine)(nil)

// Ensure StaleEngine satisfies server.StreamResumer at compile time.
var _ server.StreamResumer = (*StaleEngine)(nil)

// Ensure StaleEngine satisfies the chunk interfaces (defined locally
// because server's are unexported). Without these, the server's
// chunk-aware streaming path would silently fall back to text-only
// for every request in serve mode.
var _ chunkStreamer = (*StaleEngine)(nil)
var _ chunkResumer = (*StaleEngine)(nil)
