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
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
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
type StaleEngine struct {
	engine    server.Engine
	exact     *cache.ExactCache
	policy    Policy
	now       func() time.Time
	mu        sync.Mutex
	lastStale bool
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
	if engine == nil {
		return nil
	}
	return &StaleEngine{
		engine:   engine,
		exact:    exact,
		policy:   policy,
		now:      time.Now,
		storedAt: make(map[string]time.Time),
	}
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
// (setOutcome takes it).
func (e *StaleEngine) setOutcome(stale bool, age time.Duration) {
	e.mu.Lock()
	e.lastStale = stale
	e.lastStaleAge = age
	e.mu.Unlock()
}

// store caches a successful response and records its store time.
// Failures to derive a key or to store are silently ignored: caching
// is best-effort and must never break a successful response.
func (e *StaleEngine) store(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse) {
	if e.exact == nil || resp == nil {
		return
	}
	key, err := cache.ExactKey(req.Model, req.Messages)
	if err != nil {
		return
	}
	e.exact.Set(ctx, key, cache.CachedResponse{
		Content: resp.Content,
		Usage: cache.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
		Model: resp.Model,
	}, DefaultStoreTTL)
	e.mu.Lock()
	e.storedAt[key] = e.now()
	e.mu.Unlock()
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

// Ensure StaleEngine satisfies server.Engine at compile time.
var _ server.Engine = (*StaleEngine)(nil)

// Ensure StaleEngine satisfies server.StreamResumer at compile time.
var _ server.StreamResumer = (*StaleEngine)(nil)
