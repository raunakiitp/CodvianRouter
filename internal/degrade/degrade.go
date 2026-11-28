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
// exact-match cache (cache.ExactCache, keyed by the scoped key
// cache.ScopedKey(tenant, model, messages, ...), TTL DefaultStoreTTL) and falls back
// to that cache on failure. It does not invent responses, does not
// call providers itself, and does not log message content.
package degrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/router"
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
	// AllowNonZeroTemp is the per-key explicit allowance to cache
	// temperature>0 responses (M1.4). No per-key allowance exists
	// in config yet, so this stays false in serve mode until one
	// is added: temperature>0 answers are never stored and never
	// served from cache. Setting it opts this engine's tenants in.
	AllowNonZeroTemp bool
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
	// SemanticThreshold is the operator-configured semantic
	// similarity floor (M6.7; config cache.semantic_threshold,
	// wired by the coordinator from config into this field when
	// the engine is built). 0 means the default
	// (cache.DefaultSemanticThreshold, 0.85). Values below 0.85
	// are ignored — the effective floor never drops below 0.85 —
	// and values above 1 are ignored. A per-request threshold
	// header can only raise this floor for one request, never
	// lower it (see effectiveSemanticThreshold).
	SemanticThreshold float32
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
// the caches are expected to be safe for concurrent use as well
// (server.Engine implementations and cache.ExactStore /
// cache.SemanticStore implementations both are).
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

// storeRefusalMetrics is optionally implemented by a CacheMetrics sink
// (metrics.Recorder does): one call per response the store poison guard
// refused to cache, labelled with the reason. Structural, so this
// package still does not import internal/metrics.
type storeRefusalMetrics interface {
	IncCacheStoreRefused(reason string)
}

// vectorSourceMetrics is optionally implemented by a CacheMetrics sink
// (metrics.Recorder does): one call per served semantic hit, labelled
// with the vector space the hit was matched in.
type vectorSourceMetrics interface {
	IncSemanticVectorSource(source string)
}

type StaleEngine struct {
	engine   server.Engine
	exact    cache.ExactStore
	semantic cache.SemanticStore
	// reasoningCache, when set via SetReasoningCache, is the M6.6
	// reasoning replay cache (cache.ReasoningCache): the FULL
	// reasoning state captured from successful responses is
	// stored under tool-call ids (and the conversation digest),
	// and restored onto assistant messages that arrive without
	// it, before the request goes upstream. Nil disables it; the
	// router's own transcript-keyed store
	// (internal/reasoning.Store) keeps working either way. The
	// full state — not text only — is what is stored and
	// injected (M6.6b): a text-only inject used to pre-empt the
	// router's signed state for the same turn, failing
	// Anthropic/Bedrock hops whose wire requires the signature.
	// Injecting the stored state verbatim makes the pre-emption
	// harmless: the router sees exactly the state its own store
	// would have injected.
	reasoningCache *cache.ReasoningCache
	policy         Policy
	now            func() time.Time
	mu             sync.Mutex
	cacheMetrics   CacheMetrics
	lastStale      bool
	// lastCacheKind is "exact" or "semantic" when the most recent
	// call was served proactively from a cache, "" otherwise.
	// lastCacheSimilarity is the semantic similarity of that hit
	// (0 for exact hits). A stale serve is not a cache hit: it
	// sets lastStale and clears these fields.
	lastCacheKind       string
	lastCacheSimilarity float32
	// lastCacheVectorSource is the vector space the most recent
	// semantic hit was matched in ("neural" / "bag-of-words"), "" for
	// exact hits and non-hits. Read by LastCacheVectorSource so the
	// server can mark the response with X-Codvian-Cache-Embedding.
	lastCacheVectorSource string
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
	// tenantKeys indexes the exact-cache keys this wrapper stored,
	// by tenant (gateway key id), so FlushTenant can remove
	// exactly one tenant's entries (M1.5). Keys stored by other
	// writers are not indexed here; they are only removed by
	// FlushAll. Relay-stored entries join the index via
	// NoteCacheStore (the server calls it after storing).
	tenantKeys map[string]map[string]struct{}
	// semProvenance records, per (model, 3-turn window text),
	// which tenants stored a semantic entry for that exact text
	// and when (M1.4). Since M6.11 the semantic store itself
	// records the tenant on scoped entries, so this map is
	// defence in depth, not the enforcement: it still gates the
	// exact-text case for stores WITHOUT the scoped methods
	// (whose legacy lookups are tenant-blind), and it is the
	// gate the relay path reads. Same-tenant paraphrase hits
	// still work — the gate only fires on an exact-text match
	// owned by someone else. The cross-tenant paraphrase gap
	// this map could never close is closed inside the store by
	// the M6.11 scoped lookup (see semanticLookup).
	semProvenance map[semProvenanceKey]map[string]time.Time
	// savingsRecorder, when set, receives one RecordHit per
	// proactive own-cache hit (M1.5). savingsPrices supplies the
	// per-token USD prices for the hit's model; nil means unknown
	// prices, so dollars saved is $0 and only tokens are recorded.
	savingsRecorder CacheSavingsRecorder
	savingsPrices   func(model string) (promptPerToken, completionPerToken float64)
	// ledger, when set via SetLedgerRecorder, receives one ledger
	// row per proactive own-cache hit (B4-51): the measured tokens
	// the hit saved (CacheTokensSaved), recorded as a no-charge
	// row — no upstream call was made, so no provider was billed.
	// Nil disables it; until main.go wires it, hits record
	// nothing to the ledger and behaviour is unchanged.
	ledger ledgerRecorder
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
	// A typed nil must become a nil interface (see
	// WrapWithSemantic): passing it through would make the
	// wrapper believe a cache exists and panic on first use.
	var store cache.ExactStore
	if exact != nil {
		store = exact
	}
	return WrapWithSemantic(engine, store, nil, policy)
}

// WrapWithSemantic is Wrap plus the semantic cache layer: successful
// responses are stored in both the exact-match cache and the semantic
// cache, and when Policy.ServeCache is true, cacheable requests are
// served proactively from exact first, then semantic. A nil semantic
// cache disables the semantic layer (exact behaviour unchanged).
//
// Both caches are interfaces (cache.ExactStore / cache.SemanticStore,
// M6.2b): the in-memory ExactCache / LocalSemanticCache satisfy
// them, and so do the persistent wrappers (*cache.PersistentCache
// directly; *cache.PersistentSemanticCache via AsStore). Passing a
// persistent wrapper makes serve mode TRUE write-through — every
// store reaches disk synchronously instead of waiting for the
// 1-minute snapshot. Pass a nil interface, never a typed nil
// pointer, for an absent layer: a typed nil inside the interface
// would pass the nil checks and panic on use.
func WrapWithSemantic(engine server.Engine, exact cache.ExactStore, semantic cache.SemanticStore, policy Policy) server.Engine {
	if engine == nil {
		return nil
	}
	// A typed nil — (*cache.ExactCache)(nil),
	// (*cache.PersistentCache)(nil), (*cache.LocalSemanticCache)(nil)
	// — inside the interface is non-nil, so the wrapper's nil
	// checks would pass and the first cache call would panic on
	// the nil receiver. Normalize the shipped implementations to
	// a nil interface here, at construction (callers should still
	// pass a nil interface, never a typed nil, for an absent
	// layer — this only saves callers that kept the old
	// concrete-typed call).
	switch c := exact.(type) {
	case *cache.ExactCache:
		if c == nil {
			exact = nil
		}
	case *cache.PersistentCache:
		if c == nil {
			exact = nil
		}
	}
	switch c := semantic.(type) {
	case *cache.LocalSemanticCache:
		if c == nil {
			semantic = nil
		}
	}
	return &StaleEngine{
		engine:        engine,
		exact:         exact,
		semantic:      semantic,
		policy:        policy,
		now:           time.Now,
		storedAt:      make(map[string]time.Time),
		tenantKeys:    make(map[string]map[string]struct{}),
		semProvenance: make(map[semProvenanceKey]map[string]time.Time),
	}
}

// SetCacheMetrics wires hit/miss reporting (e.g. *metrics.Recorder).
// A nil value disables reporting.
func (e *StaleEngine) SetCacheMetrics(m CacheMetrics) {
	e.mu.Lock()
	e.cacheMetrics = m
	e.mu.Unlock()
}

// SetReasoningCache wires the M6.6 reasoning replay cache. A nil
// value disables reasoning capture/injection on this wrapper (the
// router's own replay store is unaffected). The cache is opt-in
// because Wrap/WrapWithSemantic must keep their signatures: the
// coordinator wires it after construction, exactly like
// SetCacheMetrics.
func (e *StaleEngine) SetReasoningCache(c *cache.ReasoningCache) {
	e.mu.Lock()
	e.reasoningCache = c
	e.mu.Unlock()
}

// ReasoningCacheStats returns the live accounting of the wired
// reasoning replay cache. With no cache wired it returns the zero
// value — no cache, no stats, never an invented count.
func (e *StaleEngine) ReasoningCacheStats() cache.ReasoningStats {
	e.mu.Lock()
	rc := e.reasoningCache
	e.mu.Unlock()
	if rc == nil {
		return cache.ReasoningStats{}
	}
	return rc.Stats()
}

// semProvenanceKey is one semantic entry's identity for
// provenance: the model the request named plus the exact 3-turn
// window text the semantic cache matched on.
type semProvenanceKey struct {
	model string
	text  string
}

// CacheSavingsRecorder receives one RecordHit per proactive own-cache
// hit (M1.5). cache.PersistentCache satisfies it; a nil value
// disables savings recording. Prices are caller-supplied per-token
// USD rates for the hit's model (0 when unknown).
type CacheSavingsRecorder interface {
	RecordHit(ctx context.Context, tenant, key string, resp cache.CachedResponse, promptPricePerToken, completionPricePerToken float64) (cache.SavingsRecord, error)
}

// SetSavingsRecorder wires savings recording for proactive cache
// hits. A nil value disables recording.
func (e *StaleEngine) SetSavingsRecorder(r CacheSavingsRecorder) {
	e.mu.Lock()
	e.savingsRecorder = r
	e.mu.Unlock()
}

// SetSavingsPrices wires the per-token USD price lookup used when
// recording a hit (model → prompt/completion price per token). A nil
// value means prices are unknown: dollars saved records as $0 and
// only tokens are recorded, never an invented price.
func (e *StaleEngine) SetSavingsPrices(fn func(model string) (promptPerToken, completionPerToken float64)) {
	e.mu.Lock()
	e.savingsPrices = fn
	e.mu.Unlock()
}

// ledgerRecorder receives one ledger row per proactive own-cache
// hit (B4-51). *ledger.AsyncWriter satisfies it (Record is async,
// so recording never blocks serving); a nil value disables ledger
// recording.
type ledgerRecorder interface {
	Record(ledger.Usage)
}

// SetLedgerRecorder wires ledger recording for proactive own-cache
// hits (B4-51). A nil value disables recording.
func (e *StaleEngine) SetLedgerRecorder(r ledgerRecorder) {
	e.mu.Lock()
	e.ledger = r
	e.mu.Unlock()
}

// recordCacheHitLedger records one proactive own-cache hit as a
// ledger row (B4-51), if a recorder is wired. The row carries the
// avoided call's measured usage (the cached entry's split, as
// served) and the tokens saved: prompt+completion for an exact hit
// — the identical request would have billed exactly those —
// prompt only for a semantic hit, whose own completion would have
// differed. NoCharge marks the row as a call that never happened:
// $0 by construction, never a computed or reported cost.
// Best-effort: recording must never break a served hit.
func (e *StaleEngine) recordCacheHitLedger(req provider.ChatRequest, cached cache.CachedResponse, kind string, latencyMs int64) {
	e.mu.Lock()
	rec := e.ledger
	e.mu.Unlock()
	if rec == nil {
		return
	}
	model := cached.Model
	if model == "" {
		model = req.Model
	}
	prompt := cached.Usage.PromptTokens
	if prompt < 0 {
		prompt = 0
	}
	completion := cached.Usage.CompletionTokens
	if completion < 0 {
		completion = 0
	}
	total := cached.Usage.TotalTokens
	if total < 0 {
		total = 0
	}
	saved := prompt
	if kind == "exact" {
		saved += completion
	}
	rec.Record(ledger.Usage{
		GatewayKeyID:     req.CallerID,
		Provider:         "cache",
		Model:            model,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CacheTokensSaved: saved,
		SplitKnown:       prompt != 0 || completion != 0,
		OK:               true,
		LatencyMS:        latencyMs,
		NoCharge:         true,
	})
}

// recordCacheHit records one proactive own-cache hit with the
// savings recorder, if any. Best-effort: a recorder error is
// swallowed — savings accounting must never break a served hit.
func (e *StaleEngine) recordCacheHit(ctx context.Context, tenant, key string, cached cache.CachedResponse) {
	e.mu.Lock()
	rec := e.savingsRecorder
	priceFn := e.savingsPrices
	e.mu.Unlock()
	if rec == nil {
		return
	}
	var pp, cp float64
	if priceFn != nil {
		pp, cp = priceFn(cached.Model)
	}
	_, _ = rec.RecordHit(ctx, tenant, key, cached, pp, cp)
}

// NoteCacheStore records that key was stored under tenant by a
// writer outside this wrapper (the server's messages-relay store),
// so FlushTenant can remove it too. It takes no lock on the cache
// itself — only on this wrapper's index.
func (e *StaleEngine) NoteCacheStore(tenant, key string) {
	if tenant == "" || key == "" {
		return
	}
	e.mu.Lock()
	if e.tenantKeys[tenant] == nil {
		e.tenantKeys[tenant] = make(map[string]struct{})
	}
	e.tenantKeys[tenant][key] = struct{}{}
	e.mu.Unlock()
}

// noteSemanticStore records that tenant stored a semantic entry
// for (model, text). Provenance older than the store TTL is
// dropped on read, so the map cannot serve a tenant an entry the
// cache has already aged out of; the map itself is pruned when it
// grows past semProvenanceCap entries.
func (e *StaleEngine) noteSemanticStore(tenant, model, text string) {
	if model == "" || text == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.semProvenance) > semProvenanceCap {
		now := e.now()
		for k, tenants := range e.semProvenance {
			for tenant, at := range tenants {
				if now.Sub(at) > DefaultStoreTTL {
					delete(tenants, tenant)
				}
			}
			if len(tenants) == 0 {
				delete(e.semProvenance, k)
			}
		}
	}
	k := semProvenanceKey{model: model, text: text}
	if e.semProvenance[k] == nil {
		e.semProvenance[k] = make(map[string]time.Time)
	}
	e.semProvenance[k][tenant] = e.now()
}

// NoteSemanticStore is noteSemanticStore for writers outside this
// wrapper (the server's messages-relay store), so a relay-stored
// semantic entry joins the provenance the lookup gate reads.
func (e *StaleEngine) NoteSemanticStore(tenant, model, text string) {
	e.noteSemanticStore(tenant, model, text)
}

// semProvenanceCap bounds the provenance map; entries are pruned
// by age once it is crossed.
const semProvenanceCap = 8192

// semanticBlocked reports whether a semantic lookup for
// (tenant, model, text) must be skipped: the exact text was
// stored by at least one other tenant, and not by this tenant
// (within the store TTL). Since M6.11 the scoped store enforces
// tenant isolation itself, so for scoped stores this gate is
// redundant but harmless; it still protects the fallback path —
// a store WITHOUT the scoped methods serves tenant-blind legacy
// lookups, where this gate is the only thing keeping an
// exact-text match from serving another tenant's answer at
// similarity 1.0. It returns false when nobody stored this exact
// text (a paraphrase or a new prompt — the semantic lookup runs
// as before) and when this tenant stored it (their own entry is
// the top match). Caller must not hold e.mu (it takes it).
func (e *StaleEngine) semanticBlocked(tenant, model, text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	tenants, ok := e.semProvenance[semProvenanceKey{model: model, text: text}]
	if !ok || len(tenants) == 0 {
		return false
	}
	now := e.now()
	other := false
	for t, at := range tenants {
		if now.Sub(at) > DefaultStoreTTL {
			continue
		}
		if t == tenant {
			return false
		}
		other = true
	}
	return other
}

// SemanticLookupBlocked is semanticBlocked for callers outside
// this wrapper (the server's messages-relay lookup), so the relay
// path honours the same gate as the engine path.
func (e *StaleEngine) SemanticLookupBlocked(tenant, model, text string) bool {
	return e.semanticBlocked(tenant, model, text)
}

// FlushAll removes every entry from the exact cache and clears the
// wrapper's store-time and tenant indexes. It satisfies
// cache.FlushStore, so POST /admin/cache/flush can use this engine
// as its store. A nil exact cache flushes nothing.
//
// The exact layer is an ExactStore, which has no FlushAll (the two
// implementations spell it differently: ExactCache.FlushAll() int,
// PersistentCache.FlushAll(ctx) int), so the call dispatches on
// the form the layer offers. With a *PersistentCache the flush
// deletes the disk rows immediately (M6.2b): an admin flush is
// durable at once, not only at the next snapshot tick — a
// crash/restart in that window no longer reloads flushed entries.
func (e *StaleEngine) FlushAll(ctx context.Context) int {
	if e.exact == nil {
		return 0
	}
	n := flushExactStore(ctx, e.exact)
	e.mu.Lock()
	e.storedAt = make(map[string]time.Time)
	e.tenantKeys = make(map[string]map[string]struct{})
	e.semProvenance = make(map[semProvenanceKey]map[string]time.Time)
	e.mu.Unlock()
	return n
}

// flushExactStore flushes an ExactStore through whichever FlushAll
// form it offers. A store offering neither form flushes nothing
// (both shipped implementations offer one). The server's
// exactCacheFlushStore has its own copy of this dispatch — it
// cannot call this one (internal/server cannot import
// internal/degrade: degrade imports server).
func flushExactStore(ctx context.Context, c cache.ExactStore) int {
	switch c := c.(type) {
	case interface{ FlushAll(context.Context) int }:
		return c.FlushAll(ctx)
	case interface{ FlushAll() int }:
		return c.FlushAll()
	default:
		return 0
	}
}

// SweepExpired removes every expired entry from the wrapped exact
// layer and returns how many were removed. A nil exact layer
// sweeps nothing.
func (e *StaleEngine) SweepExpired(ctx context.Context) int {
	if e == nil || e.exact == nil {
		return 0
	}
	return e.exact.SweepExpired(ctx)
}

// DeleteExactForModel removes every exact-cache entry the wrapped
// layer holds for model and returns how many keys were removed.
// Other models are untouched (A13 H6 per-model invalidation).
func (e *StaleEngine) DeleteExactForModel(ctx context.Context, model string) int {
	if e == nil || e.exact == nil {
		return 0
	}
	return len(e.exact.DeleteExactForModel(ctx, model))
}

// savingsEngine is a *StaleEngine that also reports live cache
// savings (M6.2b): it embeds the wrapper, so every method the
// server asserts on the engine (Chat, ChatStream, ChatStreamFrom,
// ChatStreamChunks, FlushAll, LastCacheHit, CoalescingStats, …)
// is promoted unchanged, and adds SavingsTotals reading the
// persistent cache the serving path records hits into.
//
// It deliberately does NOT live on *StaleEngine itself: the
// cache-stats handler emits a savings block whenever the serving
// engine implements SavingsTotals(), so a method on StaleEngine
// would make even a memory-only deployment (no savings recorder,
// no store) emit a zero savings block — indistinguishable from a
// real store with zero recorded hits, the exact fake-data trap
// pinned in internal/server/cache_stats_test.go. The reporter
// therefore exists only when a real *cache.PersistentCache is
// wired (see WithSavings); without one the handler keeps
// emitting savings_note.
type savingsEngine struct {
	*StaleEngine
	totals func() cache.SavingsTotals
}

// SavingsTotals returns the live aggregated savings from the
// persistent cache this decorator was built with: every recorded
// own-cache hit, including hits recorded before a restart. A nil
// totals source yields the zero value — never a panic, never an
// invented number.
func (e *savingsEngine) SavingsTotals() cache.SavingsTotals {
	if e == nil || e.totals == nil {
		return cache.SavingsTotals{}
	}
	return e.totals()
}

// WithSavings wraps engine so GET /api/cache/stats can read live
// savings totals from totals (normally a *cache.PersistentCache's
// SavingsTotals method value). A nil engine returns nil. Call it
// only when a real savings store is wired: an engine wrapped
// here always reports a savings block, even an all-zero one,
// and a deployment with no store must keep the bare engine so
// the endpoint honestly says savings are unavailable.
func WithSavings(engine *StaleEngine, totals func() cache.SavingsTotals) server.Engine {
	if engine == nil {
		return nil
	}
	return &savingsEngine{StaleEngine: engine, totals: totals}
}

// FlushTenant removes every exact-cache entry this wrapper (or a
// NoteCacheStore caller) stored under tenant, and nothing else. It
// satisfies cache.FlushStore. Entries whose key is indexed but
// already gone from the cache (evicted) are dropped from the index
// and not counted.
func (e *StaleEngine) FlushTenant(ctx context.Context, tenant string) int {
	if e.exact == nil || tenant == "" {
		return 0
	}
	e.mu.Lock()
	keys, ok := e.tenantKeys[tenant]
	e.mu.Unlock()
	if !ok || len(keys) == 0 {
		return 0
	}
	n := 0
	for key := range keys {
		if _, found := e.exact.Get(ctx, key); found {
			e.exact.Delete(ctx, key)
			n++
		}
		e.mu.Lock()
		delete(e.storedAt, key)
		e.mu.Unlock()
	}
	e.mu.Lock()
	delete(e.tenantKeys, tenant)
	// The tenant's semantic provenance goes too: its entries can
	// no longer be served to it (exact keys are gone), and the
	// remaining tenants' ownership of the same texts must stay
	// visible so the gate still blocks the flushed tenant.
	for k, tenants := range e.semProvenance {
		delete(tenants, tenant)
		if len(tenants) == 0 {
			delete(e.semProvenance, k)
		}
	}
	e.mu.Unlock()
	return n
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
	e.setCacheOutcomeSource(kind, similarity, "")
}

// setCacheOutcomeSource is setCacheOutcome plus the vector space a
// semantic hit was matched in (Stream D observability).
func (e *StaleEngine) setCacheOutcomeSource(kind string, similarity float32, vectorSource string) {
	e.mu.Lock()
	e.lastCacheKind = kind
	e.lastCacheSimilarity = similarity
	e.lastCacheVectorSource = vectorSource
	e.lastStale = false
	e.lastStaleAge = 0
	e.mu.Unlock()
}

// LastCacheVectorSource reports the vector space the most recent
// semantic cache hit was matched in: cache.VectorSourceNeural
// ("neural") or cache.VectorSourceBagOfWords ("bag-of-words"), and ""
// when the last call was not a semantic hit. The server turns this
// into the X-Codvian-Cache-Embedding response header, so a client or
// operator can tell neural-embedding hits from token-overlap hits.
func (e *StaleEngine) LastCacheVectorSource() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastCacheKind != "semantic" {
		return ""
	}
	return e.lastCacheVectorSource
}

// RefusalChainStats forwards the wrapped engine's live refusal-chain
// outcomes (CHECKLIST 161): the degrade wrapper is a decorator, so a
// refusal recorded by the engine underneath must still reach the
// dashboard guardrails feed. An inner engine that does not report
// refusal stats yields the zero value — never an error, never a
// fabricated count.
func (e *StaleEngine) RefusalChainStats() router.RefusalChainStats {
	if rp, ok := e.engine.(interface {
		RefusalChainStats() router.RefusalChainStats
	}); ok {
		return rp.RefusalChainStats()
	}
	return router.RefusalChainStats{}
}

// Failover243Stats forwards the wrapped engine's live failover-cluster
// outcomes (CHECKLIST 243): pin escapes and suppressed tool replays.
// Same decorator rule as RefusalChainStats — a count recorded by the
// engine underneath must still reach the dashboard guardrails feed.
// An inner engine that does not report failover stats yields the zero
// value — never an error, never a fabricated count.
func (e *StaleEngine) Failover243Stats() router.Failover243Stats {
	if fp, ok := e.engine.(interface {
		Failover243Stats() router.Failover243Stats
	}); ok {
		return fp.Failover243Stats()
	}
	return router.Failover243Stats{}
}

// Failover257Stats forwards the wrapped engine's live region-aware
// failover outcomes (CHECKLIST 257): same-model cross-region,
// equivalent-model and self-hosted steps. Same decorator rule as
// Failover243Stats — a count recorded by the engine underneath
// must still reach the dashboard guardrails feed.
func (e *StaleEngine) Failover257Stats() router.Failover257Stats {
	if fp, ok := e.engine.(interface {
		Failover257Stats() router.Failover257Stats
	}); ok {
		return fp.Failover257Stats()
	}
	return router.Failover257Stats{}
}

// StreamMaxLifetime forwards the wrapped engine's effective
// stream-lifetime cap (CHECKLIST 255): the decorator must not
// hide the bound from the dashboard feed or the verbatim-relay
// bound — the cap the server reports is the cap the engine
// enforces. An inner engine that does not report one yields 0
// (disabled), never a fabricated cap.
func (e *StaleEngine) StreamMaxLifetime() time.Duration {
	if fp, ok := e.engine.(interface {
		StreamMaxLifetime() time.Duration
	}); ok {
		return fp.StreamMaxLifetime()
	}
	return 0
}

// StreamLifetimeExceededCount forwards the wrapped engine's count
// of streams the lifetime cap has terminated (CHECKLIST 255).
// Same decorator rule as StreamMaxLifetime: a count recorded by
// the engine underneath must still reach the dashboard feed.
func (e *StaleEngine) StreamLifetimeExceededCount() uint64 {
	if fp, ok := e.engine.(interface {
		StreamLifetimeExceededCount() uint64
	}); ok {
		return fp.StreamLifetimeExceededCount()
	}
	return 0
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
	e.lastCacheVectorSource = ""
	e.mu.Unlock()
}

// cacheable reports whether req may be served from, or stored into,
// the caches. Tool-using requests are excluded: a cached entry stores
// content only, so serving it would silently drop the tool calls the
// caller asked for, and a tool result in the history makes a semantic
// text match meaningless (the answer depends on the tool output, not
// just the prompt words).
//
// Responses requests are excluded too (CHECKLIST 169): a Responses
// create mints a distinct stateful object per call, so serving a
// cached answer under a fresh response id would record a
// conversation the provider never had, and a continuation must
// never be answered from a cache entry keyed on prompt text alone
// (its transcript is the conversation, not the last user turn).
// Excluding them here also excludes the stale fallback (staleFor
// refuses non-cacheable requests the same way), so a failed
// Responses request fails honestly instead of serving a stale
// answer as a new conversation turn.
func cacheable(req provider.ChatRequest) bool {
	if req.Endpoint == provider.EndpointResponses || req.PreviousResponseID != "" {
		return false
	}
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

// hasRoutingContract reports whether the request carries a
// non-empty per-request routing contract (CHECKLIST 157). An
// empty contract (or none) is the same as no contract: the cache
// rules above apply unchanged.
func hasRoutingContract(req provider.ChatRequest) bool {
	return req.Routing != nil && !req.Routing.Empty()
}

// SemanticScopeTurns is how many of the most recent conversation
// turns the semantic cache matches on (M6.7): 3. It is now an alias
// of provider.SemanticWindowTurns — the canonical window lives in
// internal/provider/semantic_window.go (M6.7c), shared with the
// relay, so the two paths can never drift apart again. The full
// rationale for the window shape (last 3 non-system turns,
// system/developer excluded unconditionally) lives on
// provider.SemanticWindowText.
const SemanticScopeTurns = provider.SemanticWindowTurns

// messageText is the visible text of one message: Content, or its
// text parts joined when sent as an array. "" when the message
// carries no text. Thin wrapper over the shared
// provider.SemanticMessageText (M6.7c); kept so existing callers
// and tests keep compiling unchanged.
func messageText(m provider.Message) string {
	return provider.SemanticMessageText(m)
}

// semanticText is the text the semantic cache matches on: the last
// SemanticScopeTurns turns excluding system/developer messages,
// each rendered "role: text" (a missing role reads as "user", as
// in the sector-typical normalizer) and joined with "\n", in
// conversation order. Messages inside the window with no text
// contribute no line but still occupy their window slot. "" when
// none of the last 3 turns carries text.
//
// Thin wrapper over the shared provider.SemanticWindowText
// (M6.7c); kept so the existing M6.7 tests keep passing unchanged.
//
// Lookup AND store both call this one function, so the window is
// identical at store and lookup by construction: a stored entry
// can only ever be found by a lookup whose last 3 turns agree
// with the stored ones, and history older than the window can
// never break (or make) a match.
//
// NOTE: the 50-turn bypass (M6.7c, provider.SemanticMaxTurns) is
// NOT applied here — semanticText stays a pure window function so
// its text is computable (and seedable in tests) for any length.
// The cap is enforced at the call sites, lookup and store, which
// skip the semantic layer entirely for >50-turn conversations.
func semanticText(req provider.ChatRequest) string {
	return provider.SemanticWindowText(req.Messages)
}

// semanticThresholdStore is the optional per-call-threshold
// extension of cache.SemanticStore (M6.7): a store that offers
// GetTextForModelWithThreshold takes the request's effective
// floor into the lookup itself, so the floor is enforced at the
// layer that owns it. The in-memory LocalSemanticCache offers
// it, and since M6.7b so does the persistent adapter
// (cache.PersistentSemanticCache.AsStore). A store that does not
// offer it is served through the post-filter fallback in
// semanticLookup, which selects identically (the lookup returns
// the single best entry, so rejecting it below the floor is the
// same decision either way) but relies on this wrapper to
// re-check the reported similarity. The interface lives here,
// not in internal/cache, because SemanticStore itself is
// unchanged by this slice.
type semanticThresholdStore interface {
	GetTextForModelWithThreshold(ctx context.Context, model, text string, threshold float32) (cache.SemanticEntry, bool)
}

// semanticScopedLookup is the optional M6.11 tenant/temperature-
// scoped lookup extension of cache.SemanticStore: a store that
// offers GetTextForModelScopedWithThreshold serves an entry only
// to a lookup naming the same tenant (gateway key id, "" =
// anonymous) and the same temperature bucket (the exact layer's
// 2-decimal bucketing, applied inside the cache) — and never
// serves a legacy (unscoped) entry. The in-memory
// LocalSemanticCache offers it, and so does the persistent
// adapter (cache.PersistentSemanticCache.AsStore). A store that
// does not offer it falls back to the threshold/legacy lookup in
// semanticLookup, where only the exact-text provenance gate
// (semanticBlocked) stands between tenants — the pre-M6.11
// behaviour, kept so third-party SemanticStore implementations
// keep working. The interface lives here, not in internal/cache,
// because SemanticStore itself is unchanged by this slice —
// the same structural-assertion precedent as
// semanticThresholdStore (M6.7).
type semanticScopedLookup interface {
	GetTextForModelScopedWithThreshold(ctx context.Context, model, text, tenant string, temperature float64, threshold float32) (cache.SemanticEntry, bool)
}

// semanticScopedStorer is the optional M6.11 scoped store
// extension of cache.SemanticStore: the entry is recorded under
// the request's tenant and temperature bucket, so a later scoped
// lookup can enforce them. Same precedent and fallback rule as
// semanticScopedLookup: a store without it takes the legacy
// SetTextForModel path in semanticStore.
type semanticScopedStorer interface {
	SetTextForModelScoped(ctx context.Context, model, text, tenant string, temperature float64, entry cache.SemanticEntry, ttl time.Duration)
}

// effectiveSemanticThreshold resolves the semantic similarity
// floor for one request (M6.7): the highest of the cache's
// absolute floor (0.85), the operator-configured floor
// (Policy.SemanticThreshold), and the per-request header floor
// (CacheControls.ThresholdFloor). A per-request override can
// only narrow (raise) the floor, never widen (lower) it: a
// header of 0.50 behaves as 0.85, and a header below the
// configured floor behaves as the configured floor. The cache
// itself enforces the same rule again (see
// cache.LocalSemanticCache.GetTextForModelWithThreshold), so a
// store constructed with a lower floor still cannot serve below
// 0.85 through this path.
func (e *StaleEngine) effectiveSemanticThreshold(req provider.ChatRequest) float32 {
	eff := float32(cache.DefaultSemanticThreshold)
	if p := e.policy.SemanticThreshold; p > eff && p <= 1 {
		eff = p
	}
	if thr, ok := req.CacheControls.ThresholdFloor(); ok && thr > eff {
		eff = thr
	}
	return eff
}

// semanticLookup is the semantic-layer lookup for one request at
// its effective threshold (see effectiveSemanticThreshold). When
// the store offers GetTextForModelScopedWithThreshold (M6.11)
// the request's tenant and temperature go into the cache with
// the lookup, so a cross-tenant paraphrase and a
// cross-temperature paraphrase both miss inside the layer. When
// it offers only GetTextForModelWithThreshold the floor is
// pushed into the cache; otherwise the legacy lookup runs and
// the hit is post-filtered against the same floor (the M6.4
// behaviour, kept as the fallback so stores without the new
// methods still narrow correctly).
func (e *StaleEngine) semanticLookup(ctx context.Context, req provider.ChatRequest, text string, threshold float32) (cache.SemanticEntry, bool) {
	if s, ok := e.semantic.(semanticScopedLookup); ok {
		return s.GetTextForModelScopedWithThreshold(ctx, req.Model, text, req.CallerID, req.Temperature, threshold)
	}
	if s, ok := e.semantic.(semanticThresholdStore); ok {
		return s.GetTextForModelWithThreshold(ctx, req.Model, text, threshold)
	}
	hit, found := e.semantic.GetTextForModel(ctx, req.Model, text)
	if !found || hit.Similarity < threshold {
		return cache.SemanticEntry{}, false
	}
	return hit, true
}

// semanticStore stores one semantic entry for req (M6.11): when
// the store offers SetTextForModelScoped the request's tenant
// (req.CallerID, "" = anonymous) and temperature go into the
// store with the entry, so a later scoped lookup can enforce
// them. Otherwise the legacy SetTextForModel path stores an
// unscoped entry, which a scoped lookup would never serve —
// that fallback exists only so third-party SemanticStore
// implementations keep working; both shipped stores offer the
// scoped method.
func (e *StaleEngine) semanticStore(ctx context.Context, req provider.ChatRequest, text string, entry cache.SemanticEntry, ttl time.Duration) {
	if s, ok := e.semantic.(semanticScopedStorer); ok {
		s.SetTextForModelScoped(ctx, req.Model, text, req.CallerID, req.Temperature, entry, ttl)
		return
	}
	e.semantic.SetTextForModel(ctx, req.Model, text, entry, ttl)
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

// reportStoreRefused records one response the store poison guard
// refused to cache, with the poison reason as the label, when the sink
// supports it (metrics.Recorder does).
func (e *StaleEngine) reportStoreRefused(reason cache.StorePoison) {
	e.mu.Lock()
	m := e.cacheMetrics
	e.mu.Unlock()
	if sr, ok := m.(storeRefusalMetrics); ok && reason != cache.StorePoisonNone {
		sr.IncCacheStoreRefused(reason.String())
	}
}

// reportVectorSource records one served semantic hit's vector space
// when the sink supports it (metrics.Recorder does).
func (e *StaleEngine) reportVectorSource(source string) {
	e.mu.Lock()
	m := e.cacheMetrics
	e.mu.Unlock()
	if vs, ok := m.(vectorSourceMetrics); ok && source != "" {
		vs.IncSemanticVectorSource(source)
	}
}

// lossyCompressed reports whether req's response was (or may have
// been) produced from a LOSSY-trimmed context (CHECKLIST 193-194).
// The semantic cache is keyed on the last 3 turns' text, so
// it cannot tell a trimmed-context answer from a full-context one:
// lossy trimming drops middle turns, and the surviving window is
// the same text whether the dropped turns were ever there — a
// trimmed answer stored under its window would be served to a
// later full-context request whose last 3 turns agree. A response
// produced from a trimmed context must therefore never enter, or
// be served from, the semantic layer — it would be presented to
// a later full-context request as if it had seen the full history.
//
// Two signals, either of which excludes the request (fail-closed):
//   - the applied stamp (req.Compressed.LossyApplied): the server
//     handler compressed before this wrapper saw the request, so
//     the stamp says lossy actually ran;
//   - the per-request opt-in (req.Compression.LossyMaxPromptTokens
//     > 0): direct engine callers compress inside the engine,
//     AFTER this wrapper's lookup, so the stamp is not yet set
//     when lookup/store run — the opt-in is the only signal
//     visible. A request that asked for lossy is excluded even
//     when the trim ended up a no-op (prompt already under
//     budget): the cost is one cache miss, never a
//     trimmed-context answer served as a full-context one.
//
// The exact-match layer is NOT excluded: its key is the messages
// as sent (cache.ScopedKey), so a lossy request and a full-context
// request have different keys by construction, and an identical
// lossy prompt still hits its own exact entry. The stale fallback
// is the exact cache too, so it is unaffected for the same reason.
//
// Lossless compression is harmless and keeps sharing: the lossless
// steps are exactly the canonical normal form (compress.
// VerifyLossless), so a losslessly-compressed prompt means the
// same thing as the original — the semantic answer is the one the
// uncompressed prompt would have produced. That distinction is
// pinned by TestSemanticCacheLosslessSharingStillWorks.
//
// The /v1/messages relay path needs no equivalent check: it never
// applies compression (applyCompression runs only in
// handleChatCompletions; the Anthropic shape carries no
// compression opt-in), so its semantic store/lookup can only ever
// see uncompressed requests — and since lossy responses never
// enter the shared semantic cache from the gateway path, the
// relay can never serve one either.
func lossyCompressed(req provider.ChatRequest) bool {
	if req.Compressed != nil && req.Compressed.LossyApplied {
		return true
	}
	return req.Compression != nil && req.Compression.LossyMaxPromptTokens > 0
}

// scopedKey derives the M1.4 cache key for req: SHA-256 of
// {tenant, model, messages, tools, temperature bucket, ...} via
// cache.ScopedKey. The tenant is the gateway key id the request
// arrived under (req.CallerID, "" when anonymous) — two tenants
// with the same prompt never share an entry. A key error means
// the request cannot be cached or served from cache at all.
//
// OPTION A (LossyExactHit): the messages hashed are the
// PRE-COMPRESSION request, not the messages as sent. When the
// server ran its compression pre-pass it snapshotted the request
// as the client sent it (req.PreCompression); the key uses the
// snapshot's raw messages plus the snapshot's compression params
// (lossy budget, RTK level, ...), so two identical logical
// requests key identically regardless of how much state the
// stateful compression engines (CCR, session-dedup) accumulated
// between them, and two requests that differ only in their
// compression params never share an entry. With no snapshot —
// the engine called directly, or a path that never ran the
// pre-pass — the key falls back to the request as received plus
// its live compression opt-in, exactly as before for
// uncompressed requests (nil compression is omitted from the
// hash, so those keys are byte-identical to the old ones).
//
// M6.4 KEY OVERRIDE: when req carries a cache-key override
// (X-Codvian-Cache-Key), the message-derived key is replaced by
// SHA-256("codvian-cache-key-override" + tenant + model +
// override). The tenant and model stay in the hash, so two tenants
// — or two models — sharing an override value still never share an
// entry; the caller only chooses the bucket within their own
// tenant+model. The override is also what makes the request skip
// the semantic layer (provider.CacheControls.UseSemantic): the
// semantic layer is keyed on prompt text, not on this key.
//
// Nil slices are normalised to untyped nil first: a typed-nil
// []ToolDefinition / json.RawMessage marshals as JSON null inside
// the key payload, while an absent field is omitted — the same
// request would otherwise key differently depending on whether
// its decoder left the field nil or unset.
func (e *StaleEngine) scopedKey(req provider.ChatRequest) (string, error) {
	if cc := req.CacheControls; cc != nil && cc.KeyOverride != "" {
		sum := sha256.Sum256([]byte("codvian-cache-key-override\x00" + req.CallerID + "\x00" + req.Model + "\x00" + cc.KeyOverride))
		return hex.EncodeToString(sum[:]), nil
	}
	var tools any
	if req.Tools != nil {
		tools = req.Tools
	}
	var toolChoice any
	if req.ToolChoice != nil {
		toolChoice = req.ToolChoice
	}
	var messages any
	if req.Messages != nil {
		messages = req.Messages
	}
	var stop any
	if req.Stop != nil {
		stop = req.Stop
	}
	// OPTION A: key on the pre-compression request when the
	// server snapshotted one (see scopedKey's doc); otherwise
	// the request as received plus its live compression opt-in.
	var compression any
	if pc := req.PreCompression; pc != nil {
		if pc.Messages != nil {
			messages = pc.Messages
		}
		compression = pc.Compression
	} else if req.Compression != nil {
		compression = req.Compression
	}
	return cache.ScopedKey(cache.KeyInput{
		Tenant:          req.CallerID,
		Model:           req.Model,
		Messages:        messages,
		Tools:           tools,
		ToolChoice:      toolChoice,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		MaxTokens:       req.MaxTokens,
		Stop:            stop,
		ReasoningEffort: req.ReasoningEffort,
		Compression:     compression,
	})
}

// lookup consults the exact-match cache first, then the semantic
// cache, for a cacheable request under a ServeCache policy. It
// returns the cached response, the hit kind ("exact" or "semantic"),
// the semantic similarity (0 for exact hits), the exact-cache key
// that hit ("" on a semantic hit, where no exact key exists), and
// true on a hit.
// Misses are reported per layer consulted. It never calls a provider
// and never serves a hit for a non-cacheable request or when
// ServeCache is off.
//
// TENANT SCOPING (M1.4, M6.11): the exact key is the scoped key
// (cache.ScopedKey, tenant = gateway key id), so a cached answer
// is never served across tenants. The semantic layer is scoped
// the same way since M6.11: the store records the tenant on
// every scoped entry and the scoped lookup (semanticLookup)
// serves only the requesting tenant's entries, so a
// cross-tenant paraphrase misses inside the layer — the
// cross-tenant paraphrase gap the wrapper-level provenance gate
// could never close. The provenance gate (semanticBlocked) is
// kept as defence in depth: it still skips an exact-text lookup
// owned only by another tenant, which matters for stores
// without the scoped methods (whose legacy lookups are
// tenant-blind). Same-tenant paraphrase hits still work — the
// gate fires only on an exact-text match owned by someone else.
//
// TEMPERATURE GATING (M1.4, M6.11): a temperature>0 request never
// consults the cache unless the per-key allowance is on — a
// temperature-0 answer must never be served to a temperature-1.0
// request, and no temperature>0 entry is ever stored (see store),
// so there is nothing for it to hit anyway; the gate is explicit
// and fail-closed rather than relying on that. With the allowance
// on, the semantic layer partitions by temperature bucket
// (M6.11): the scoped lookup serves only entries stored at the
// requesting temperature's bucket, so a temp-0 request is never
// served a temp-0.7 answer, identical text or paraphrase alike.
//
// MODEL SCOPING (CHECKLIST 187): both layers are scoped by the
// model the request named. The exact key already includes it
// (ScopedKey); the semantic lookup uses semanticLookup
// (GetTextForModelScopedWithThreshold, M6.11 — tenant and
// temperature bucket in the layer, threshold floor M6.7), so a cached
// claude-sonnet response is never served for a gpt-4o request,
// however similar the text. The provider is deliberately not part
// of the scope: the same model id on another provider shares the
// entry.
//
// COMPRESSION SCOPING (CHECKLIST 193-194): a lossy-compressed
// request never consults the semantic layer (see lossyCompressed)
// — its last-3-turns key cannot distinguish the trimmed
// context it was answered from. The exact layer is still
// consulted: its key is the pre-compression request (raw messages
// + compression params, see scopedKey), so a trimmed prompt can
// never collide with a full one — and two identical lossy
// requests always share one.
//
// A request carrying a non-empty per-request routing contract
// (CHECKLIST 157) never consults the cache: a cached entry holds
// content only, with no record of which provider produced it, so
// serving it could hand a client an answer from a provider its
// own only/exclude/order/max_price/data_collection forbade. The
// request routes live instead.
//
// PER-REQUEST CACHE CONTROLS (M6.4, req.CacheControls): a bypass
// request (X-Codvian-No-Cache / Cache-Control: no-cache / Pragma:
// no-cache / cache-type none) never consults either layer — and
// store/staleFor honour the same flag, so a bypassed request is
// never served stale and never stored. cache-type selects the
// layers (exact/semantic/both); a key override replaces the
// exact key (tenant+model scoped, see scopedKey) and disables
// the semantic layer for the request.
//
// SEMANTIC THRESHOLD (M6.7): the semantic floor for this request
// is effectiveSemanticThreshold — max(0.85, the configured
// Policy.SemanticThreshold, the per-request header floor). It is
// pushed into the cache via GetTextForModelWithThreshold when
// the store offers it, and post-filtered (the M6.4 behaviour)
// when it does not (see semanticLookup). A floor below the
// cache's own 0.85 cannot widen hits either way: the cache
// already rejected those, and the effective floor never drops
// below 0.85.
func (e *StaleEngine) lookup(ctx context.Context, req provider.ChatRequest) (cache.CachedResponse, string, float32, string, string, bool) {
	if !e.policy.ServeCache || !cacheable(req) {
		return cache.CachedResponse{}, "", 0, "", "", false
	}
	if req.CacheControls.BypassCache() {
		return cache.CachedResponse{}, "", 0, "", "", false
	}
	if hasRoutingContract(req) {
		return cache.CachedResponse{}, "", 0, "", "", false
	}
	if !cache.CacheableTemperature(req.Temperature, e.policy.AllowNonZeroTemp) {
		return cache.CachedResponse{}, "", 0, "", "", false
	}
	if hit, key, ok := e.exactLookup(ctx, req); ok {
		return hit, "exact", 0, key, "", true
	}
	// 50-TURN BYPASS (M6.7c): a conversation longer than
	// provider.SemanticMaxTurns (50) never consults the semantic
	// layer — the sector baseline bypasses semantic caching past its
	// conversationHistoryThreshold, and the exact layer above is
	// unaffected (its key is the pre-compression request — see
	// scopedKey — so the bypass does not apply to it).
	if e.semantic != nil && !lossyCompressed(req) && req.CacheControls.UseSemantic() && !provider.SemanticTurnsExceeded(req.Messages) {
		if text := semanticText(req); text != "" {
			if e.semanticBlocked(req.CallerID, req.Model, text) {
				// Exact text owned by another tenant only:
				// serving it would hand this tenant the
				// other's answer at similarity 1.0 (M1.4).
				e.reportMiss("semantic")
			} else if hit, found := e.semanticLookup(ctx, req, text, e.effectiveSemanticThreshold(req)); found {
				e.reportHit("semantic")
				e.reportVectorSource(hit.VectorSource)
				e.recordCacheHit(ctx, req.CallerID, "", hit.Response)
				return hit.Response, "semantic", hit.Similarity, "", hit.VectorSource, true
			} else {
				e.reportMiss("semantic")
			}
		}
	}
	return cache.CachedResponse{}, "", 0, "", "", false
}

// exactLookup serves the exact layer only: the scoped key for this
// request, or nothing. It is lookup's exact branch, factored out
// so the resume paths can serve exact hits too (see
// ChatStreamFrom / ChatStreamFromChunks): an exact entry is the
// response to this identical request, so replaying it trimmed by
// skipChars returns exactly the suffix the client is missing. A
// semantic entry is a different request's answer — trimming it by
// rune count would splice two different texts — so the resume
// paths never consult the semantic layer; the stale fallback
// serves exact entries only for the same reason (see staleFor).
// The gates are lookup's: ServeCache, cacheable, bypass, routing
// contract, temperature, UseExact. It runs BEFORE injectReasoning
// on every path — and the scoped key hashes the pre-compression
// snapshot, taken before any injection, so an injected reasoning
// state can never change the key and the hit cannot be missed
// that way.
func (e *StaleEngine) exactLookup(ctx context.Context, req provider.ChatRequest) (cache.CachedResponse, string, bool) {
	if !e.policy.ServeCache || !cacheable(req) {
		return cache.CachedResponse{}, "", false
	}
	if req.CacheControls.BypassCache() {
		return cache.CachedResponse{}, "", false
	}
	if hasRoutingContract(req) {
		return cache.CachedResponse{}, "", false
	}
	if !cache.CacheableTemperature(req.Temperature, e.policy.AllowNonZeroTemp) {
		return cache.CachedResponse{}, "", false
	}
	if e.exact == nil || !req.CacheControls.UseExact() {
		return cache.CachedResponse{}, "", false
	}
	key, err := e.scopedKey(req)
	if err != nil {
		return cache.CachedResponse{}, "", false
	}
	hit, found := e.exact.Get(ctx, key)
	if !found {
		e.reportMiss("exact")
		return cache.CachedResponse{}, "", false
	}
	e.reportHit("exact")
	e.recordCacheHit(ctx, req.CallerID, key, hit)
	return hit, key, true
}

// reasoningTurnsForMessages maps request messages onto the
// transcript turns the reasoning cache keys on: role, visible
// text, and tool calls. Reasoning state is deliberately NOT part
// of a turn (see cache.ReasoningTurn), so a turn keys the same
// whether its reasoning was captured, injected, or stripped.
func reasoningTurnsForMessages(msgs []provider.Message) []cache.ReasoningTurn {
	turns := make([]cache.ReasoningTurn, 0, len(msgs))
	for _, m := range msgs {
		turns = append(turns, cache.ReasoningTurn{
			Role:      m.Role,
			Text:      reasoningMessageText(m),
			ToolCalls: reasoningToolCallRefs(m.ToolCalls),
		})
	}
	return turns
}

// reasoningMessageText is the visible text of one message for
// transcript keying: Content, or its text parts joined when sent
// as an array (same extraction as messageText, kept separate so
// the reasoning path does not depend on the semantic scope).
func reasoningMessageText(m provider.Message) string {
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

// reasoningToolCallRefs maps tool calls onto the key form: id,
// name and arguments as the client replayed them.
func reasoningToolCallRefs(calls []provider.ToolCall) []cache.ReasoningToolCall {
	if len(calls) == 0 {
		return nil
	}
	refs := make([]cache.ReasoningToolCall, 0, len(calls))
	for _, tc := range calls {
		refs = append(refs, cache.ReasoningToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return refs
}

// storeReasoning captures a successful response's reasoning into
// the M6.6 replay cache, under the response's tool-call ids and
// under the conversation digest (request transcript + the served
// assistant turn). It runs at the top of store(), BEFORE the
// content-cache early return, because its gates are different:
//
//   - Tool-using requests are INCLUDED, not excluded: reasoning
//     replay exists precisely for tool-call turns (the sector baseline keys
//     on the tool_call id). Storing reasoning text cannot drop
//     tool calls — the client replays the calls themselves in the
//     transcript — so cache.ShouldStore's tool-call exclusion
//     does NOT apply here. What DOES apply, from ShouldStore's
//     semantics, is the truncated guard (a finish_reason of
//     "length" means the reasoning may be cut off mid-thought)
//     and the temperature gate (temp>0 without the per-key
//     allowance is never stored as if deterministic — M6.3).
//     the sector baseline itself applies neither gate to replay (replay is
//     a protocol requirement there, not a determinism cache);
//     this wrapper applies them because this router's cache
//     rules (M1.4/M6.3) say a hot or truncated answer is never
//     stored, in any layer. The cost is stated plainly: a temp>0
//     thinking turn without the allowance gets no replay
//     protection from THIS layer (the router's own store still
//     captures it — internal/router/reasoning.go applies no
//     temperature gate).
//   - Responses requests are excluded (a Responses create mints
//     a distinct stateful object per call; its reasoning belongs
//     to that object, not to a replayable transcript).
//   - A bypass / no-store request never stores (per-request
//     cache controls, same as the content layers).
//   - A routing-contract request never stores (same rule as
//     store(): the entry would carry no record of the contract).
//
// Only the FULL STATE survives (M6.6b): the response's
// reasoning.State is stored verbatim — thinking blocks with
// their Anthropic signatures, Bedrock reasoningContent, redacted
// blocks — not its text alone. Storing text only used to shadow
// the router's own store: this wrapper runs outside the router,
// so its inject fires first, and the router's injectReasoning
// then skips any message that already carries reasoning —
// including a message this wrapper had just filled with a
// text-only KindThought block. On an Anthropic or Bedrock target
// that pre-emption was a hop failure, not a benign downgrade:
// reasoning.Check returns UnrepresentableError for signature-less
// thought (the API rejects unsigned thinking / signature-less
// reasoningText), and the wire converter fails the hop. Storing
// and injecting the full state makes the pre-emption exact: the
// router skips the message because the signed state is already
// there, which is what its own store would have attached.
//
// A state whose text is empty is still stored when it carries an
// opaque payload (a signature or a Data blob, e.g. a redacted
// block): it replays verbatim on its home wire. A state with
// neither text nor opaque payload (a vacuous empty-text thought
// block) is not stored — injecting it would hand an
// Anthropic/Bedrock target an unsigned block for no reason. The
// provider is recorded as "" (unknown): the provider is not
// known at this layer (M6.12), and recording a guess would
// mislabel stats. The model recorded is the model the REQUEST
// named (CHECKLIST 187), not the response's model.
func (e *StaleEngine) storeReasoning(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse) {
	e.mu.Lock()
	rc := e.reasoningCache
	e.mu.Unlock()
	if rc == nil || resp == nil || resp.Reasoning == nil || resp.Reasoning.Empty() {
		return
	}
	if req.Endpoint == provider.EndpointResponses || req.PreviousResponseID != "" {
		return
	}
	if !req.CacheControls.AllowStore() {
		return
	}
	if hasRoutingContract(req) {
		return
	}
	if strings.EqualFold(resp.FinishReason, "length") {
		return
	}
	if !cache.CacheableTemperature(req.Temperature, e.policy.AllowNonZeroTemp) {
		return
	}
	text := resp.Reasoning.ReasoningText()
	if text == "" && !reasoningHasOpaquePayload(resp.Reasoning) {
		return
	}
	for _, tc := range resp.ToolCalls {
		if key := cache.ToolCallKey(req.CallerID, tc.ID); key != "" {
			rc.SetStateForTenant(ctx, key, req.CallerID, "", req.Model, *resp.Reasoning)
		}
	}
	if len(req.Messages) > 0 {
		turns := reasoningTurnsForMessages(req.Messages)
		turns = append(turns, cache.ReasoningTurn{
			Role:      "assistant",
			Text:      resp.Content,
			ToolCalls: reasoningToolCallRefs(resp.ToolCalls),
		})
		if key := cache.ConversationKey(req.CallerID, turns); key != "" {
			rc.SetStateForTenant(ctx, key, req.CallerID, "", req.Model, *resp.Reasoning)
		}
	}
}

// reasoningHasOpaquePayload reports whether st carries anything
// beyond its text: a signature on any block, or a Data blob (a
// redacted block, a Bedrock reasoningContent block). Such a state
// replays verbatim on its home wire even when its text is empty,
// so storeReasoning keeps it; a state with neither text nor
// payload is vacuous and is not stored.
func reasoningHasOpaquePayload(st *reasoning.State) bool {
	if st == nil {
		return false
	}
	for _, b := range st.Blocks {
		if b.Signature != "" || len(b.Data) > 0 {
			return true
		}
	}
	return false
}

// injectReasoning restores cached reasoning onto the request's
// assistant messages that carry tool calls (or, with no tool
// calls, match a cached conversation digest) but arrive without
// reasoning state — the DeepSeek/Kimi contract: the next turn
// must pass reasoning_content back or the upstream 400s. It runs
// after a proactive-cache miss and before the upstream call, in
// Chat, ChatStream, ChatStreamChunks, ChatStreamFrom and
// ChatStreamFromChunks — all five entry points. (The text-only
// resume used to be excluded on the grounds that the router
// injects below this wrapper there; it injects only the turns
// its transcript-keyed store holds, so the tool-call-key hits
// this layer owns were missed on a text resume — now covered.)
//
// It returns a request whose Messages slice is copied ONLY when
// an injection actually happens (copy-on-write): the caller's
// slice is never mutated in place. A message that already
// carries reasoning is left alone. Injection is NOT a cache hit
// for X-Codvian-Cache purposes — no response is served from
// cache — so it touches neither LastCacheHit nor the metrics.
//
// Gates: Responses requests and routing-contract requests are
// excluded, for the same reasons as storeReasoning. A bypass
// request is NOT excluded: bypass means "no cached ANSWER", and
// injection restores protocol-required state for a call that
// still goes upstream — without it the call would 400, not
// merely miss a cache.
//
// The injected state is the FULL captured state (M6.6b), cloned
// from the reasoning cache: thinking blocks with their
// signatures, Bedrock reasoningContent, redacted blocks —
// exactly the state the router's own store holds for the same
// turn. That matters because this wrapper runs outside the
// router: its inject fires first, and the router's
// injectReasoning then skips any message that already carries
// reasoning. With a text-only inject that skip used to LOSE the
// signature — the router would have injected the signed state,
// and instead the hop carried an unsigned thought block. The
// full-state inject makes the skip exact: the signed state is
// already there, so nothing is lost.
//
// The router below re-validates the injected state per wire
// (reasoning.Check). What the target cannot carry is NOT
// silently dropped: when the reasoning TEXT itself cannot be
// carried — a signature-less thought block to Anthropic or
// Bedrock, reasoning text to an OpenAI-wire model that takes no
// reasoning_content, any reasoning to Cohere — Check returns an
// UnrepresentableError and the hop FAILS (the router fails over
// to the next target; only when no target can carry it does the
// client see an honest 400). Only opaque signatures with no
// equivalent slot are dropped, with the drop recorded in the
// route trace — the text survives in the target's own slot.
func (e *StaleEngine) injectReasoning(ctx context.Context, req provider.ChatRequest) provider.ChatRequest {
	e.mu.Lock()
	rc := e.reasoningCache
	e.mu.Unlock()
	if rc == nil || len(req.Messages) == 0 {
		return req
	}
	if req.Endpoint == provider.EndpointResponses || req.PreviousResponseID != "" {
		return req
	}
	if hasRoutingContract(req) {
		return req
	}
	msgs := req.Messages
	copied := false
	for i := range msgs {
		m := &msgs[i]
		if m.Role != "assistant" || (m.Reasoning != nil && !m.Reasoning.Empty()) {
			continue
		}
		var st reasoning.State
		var ok bool
		for _, tc := range m.ToolCalls {
			key := cache.ToolCallKey(req.CallerID, tc.ID)
			if key == "" {
				continue
			}
			if st, ok = rc.GetState(ctx, key); ok {
				break
			}
		}
		if !ok && len(m.ToolCalls) == 0 {
			turns := reasoningTurnsForMessages(msgs[:i+1])
			if key := cache.ConversationKey(req.CallerID, turns); key != "" {
				st, ok = rc.GetState(ctx, key)
			}
		}
		if !ok || st.Empty() {
			continue
		}
		if !copied {
			msgs = append([]provider.Message(nil), req.Messages...)
			req.Messages = msgs
			copied = true
			m = &msgs[i]
		}
		// Full captured state, verbatim (M6.6b): GetState
		// already returns a clone, so assigning it cannot
		// mutate the cache entry through the request.
		m.Reasoning = &st
	}
	return req
}

// store caches a successful response and records its store time.
// Failures to derive a key or to store are silently ignored: caching
// is best-effort and must never break a successful response.
// Non-cacheable requests (tool-using) are never stored: a cached
// entry holds content only, so storing one would let a later lookup
// silently drop tool calls. Cacheable successes are stored in the
// exact cache under their scoped key and, when a semantic cache is
// configured, in the semantic cache under the last 3 turns'
// text (semanticText) — same 1h TTL and LRU eviction rules as the exact cache, so
// neither layer can serve an entry the other has aged out of.
//
// STORE GATES (M1.4, cache.ShouldStore): a response is stored only
// when its finish reason is not "length" (truncated), its
// temperature is cacheable (0, or >0 with the per-key allowance),
// and it carries no tool calls. The gates run here, at the single
// store site, so no path can smuggle a truncated or hot answer
// into the cache.
//
// TENANT SCOPING (M1.4): the exact key is the scoped key
// (tenant = gateway key id), and the key is indexed under that
// tenant so FlushTenant can remove it later.
//
// MODEL SCOPING (CHECKLIST 187): the semantic entry is stored
// under the model the REQUEST named (SetTextForModelScoped with
// req.Model), not the response's model — the response can name an
// upstream alias that differs from the requested model, and the
// scope must be the requested model, so only a later request
// naming that same model can ever be served it. The provider is
// deliberately not part of the scope: the same model id on another
// provider shares the entry. Nothing is evicted or migrated by
// this scoping: entries are in-memory and carry their scope from
// the moment they are stored.
//
// TENANT + TEMPERATURE SCOPING (M6.11): the same store call
// records the requesting tenant (req.CallerID) and the
// temperature bucket (TemperatureBucket(req.Temperature), the
// exact layer's bucketing), so only a later request from the
// same tenant at the same temperature bucket can ever be served
// the entry — a cross-tenant paraphrase and a cross-temperature
// paraphrase both miss inside the layer.
//
// A response to a request carrying a non-empty per-request routing
// contract (CHECKLIST 157) is never stored either: the entry would
// carry no record of the contract that produced it, so a later
// lookup could serve it to a request whose contract forbids the
// provider that produced it.
//
// COMPRESSION SCOPING (CHECKLIST 193-194): a lossy-compressed
// response is never stored in the semantic layer (see
// lossyCompressed) — the entry would be keyed on the last 3
// turns' text, which cannot distinguish the trimmed context
// from a full one, so a later full-context request would be
// served the trimmed-context answer. The exact layer still stores
// it: the exact key is the pre-compression request (raw messages
// + compression params), so only an identical logical request —
// same raw messages, same compression params — can ever hit it. Lossless responses
// keep sharing semantically: the lossless steps are the canonical
// normal form, so the stored answer is the one the uncompressed
// prompt would have produced.
func (e *StaleEngine) store(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse) {
	// Reasoning capture runs FIRST, before the content-cache
	// early return below: it has its own gates (see
	// storeReasoning) and tool-using requests — excluded from
	// content caching — are exactly the requests it serves.
	e.storeReasoning(ctx, req, resp)
	if resp == nil || !cacheable(req) {
		return
	}
	// PER-REQUEST CACHE CONTROLS (M6.4): a bypass request
	// (X-Codvian-No-Cache / no-cache / cache-type none) and a
	// no-store request (X-Codvian-Cache-No-Store) never store —
	// a bypassed request must never poison the cache. The gate
	// runs here, at the single store site, so every entry point
	// (Chat, ChatStream, ChatStreamFrom, chunk variants) honours
	// it without a per-path check.
	if !req.CacheControls.AllowStore() {
		return
	}
	if hasRoutingContract(req) {
		return
	}
	// POISON GUARD (Stream D): the store-site gate is stricter than
	// cache.ShouldStore - it also refuses content-policy refusals (any
	// refusal finish reason), every truncation spelling a passthrough
	// wire may pass through un-normalised (not just "length"), and
	// empty-content responses. A refused/truncated/blank answer is not
	// an answer: caching it would replay it to every later request
	// that keys or embeds close to this one until the TTL. The guard
	// runs here, at the single store site, so every entry point
	// (Chat, ChatStream, ChatStreamFrom, chunk variants) honours it,
	// and the refusal is counted when the sink supports it.
	guard := cache.StoreGuard{
		FinishReason:     resp.FinishReason,
		Content:          resp.Content,
		Temperature:      req.Temperature,
		AllowNonZeroTemp: e.policy.AllowNonZeroTemp,
		HasToolCalls:     len(resp.ToolCalls) > 0,
	}
	if poison := guard.Poison(); poison != cache.StorePoisonNone {
		e.reportStoreRefused(poison)
		return
	}
	entry := cache.CachedResponse{
		Content: resp.Content,
		Usage: cache.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			// Phase 2 carry-over: preserve the provider-reported detail
			// splits through the cache round-trip (clamped; negatives
			// are malformed output and never stored).
			CachedTokens:    max(resp.Usage.CachedTokens, 0),
			ReasoningTokens: max(resp.Usage.ReasoningTokens, 0),
		},
		Model: resp.Model,
	}
	// Per-request TTL (M6.4): the request's TTL, capped at the
	// layer default (1h) by EffectiveTTL; 0 means the default.
	ttl := DefaultStoreTTL
	if t := req.CacheControls.EffectiveTTL(); t > 0 {
		ttl = t
	}
	if e.exact != nil && req.CacheControls.UseExact() {
		key, err := e.scopedKey(req)
		if err == nil {
			e.exact.Set(ctx, key, entry, ttl)
			e.mu.Lock()
			e.storedAt[key] = e.now()
			if e.tenantKeys[req.CallerID] == nil {
				e.tenantKeys[req.CallerID] = make(map[string]struct{})
			}
			e.tenantKeys[req.CallerID][key] = struct{}{}
			e.mu.Unlock()
		}
	}
	// 50-TURN BYPASS (M6.7c): a conversation longer than
	// provider.SemanticMaxTurns (50) never stores a semantic entry
	// either — deliberately stricter than the sector baseline, whose store
	// path has no threshold check (see
	// provider.SemanticTurnsExceeded), so no entry can exist that
	// a later lookup would have to bypass around. The exact layer
	// above is unaffected.
	if e.semantic != nil && !lossyCompressed(req) && req.CacheControls.UseSemantic() && !provider.SemanticTurnsExceeded(req.Messages) {
		if text := semanticText(req); text != "" {
			e.semanticStore(ctx, req, text, cache.SemanticEntry{Response: entry}, ttl)
			e.noteSemanticStore(req.CallerID, req.Model, text)
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
//
// Model scoping (CHECKLIST 187): the stale lookup is the exact
// cache under the scoped key (tenant + model + messages), so it is
// scoped by the requesting tenant and model exactly like the
// proactive lookup — a stale claude-sonnet answer is never served
// for a gpt-4o request, and never across tenants.
//
// Temperature gating (M1.4): a temperature>0 request never serves
// stale unless the per-key allowance is on — same gate as the
// proactive lookup, so a hot request cannot be answered from a
// cold entry even on the failure path.
//
// Compression scoping (CHECKLIST 193-194): the stale path needs
// no lossy exclusion — the exact key is the pre-compression
// request, so a lossy-trimmed prompt and a full-context prompt
// have different keys by construction, and a stale entry can only
// ever be served to a request whose raw messages and compression
// params are identical to the ones that produced it.
func (e *StaleEngine) staleFor(ctx context.Context, req provider.ChatRequest) (cached cache.CachedResponse, age time.Duration, ok bool) {
	if e.exact == nil || !e.policy.staleAllowed() {
		return cache.CachedResponse{}, 0, false
	}
	// PER-REQUEST CACHE CONTROLS (M6.4): a bypass request is
	// never served a stale answer either — bypass means "fresh
	// or fail", on the failure path as much as the proactive
	// one. A no-store request MAY still be served stale: it
	// only asked not to write, and the stale entry was written
	// by someone else's earlier request.
	if req.CacheControls.BypassCache() {
		return cache.CachedResponse{}, 0, false
	}
	// PER-REQUEST CACHE CONTROLS (M6.4 fix, review): layer
	// selection applies on the failure path too. Stale serves
	// exact-layer entries only, so a request that excluded the
	// exact layer (Type "semantic" or "none") must never be
	// served one — the proactive lookup already skips exact for
	// such requests, and the failure path must not undo that.
	if !req.CacheControls.UseExact() {
		return cache.CachedResponse{}, 0, false
	}
	if hasRoutingContract(req) {
		// A stale entry holds content only, with no record of
		// which provider produced it — serving it for a
		// contract request could hand back an answer from a
		// provider the contract forbade (CHECKLIST 157).
		return cache.CachedResponse{}, 0, false
	}
	if !cacheable(req) {
		// A cached entry holds content only. Serving it for a
		// tool-using request would silently drop the tool calls
		// the caller asked for — same reason proactive lookup and
		// store skip such requests.
		return cache.CachedResponse{}, 0, false
	}
	if !cache.CacheableTemperature(req.Temperature, e.policy.AllowNonZeroTemp) {
		return cache.CachedResponse{}, 0, false
	}
	key, err := e.scopedKey(req)
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
			// Phase 2 carry-over: the stored detail splits ride the
			// replay, so a cache hit reports the same usage shape as
			// the original live response (client usage block +
			// telemetry project them).
			CachedTokens:    cached.Usage.CachedTokens,
			ReasoningTokens: cached.Usage.ReasoningTokens,
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
	// without calling any provider, so no provider is billed. The
	// hit IS recorded in the ledger (B4-51, recordCacheHitLedger)
	// as a no-charge row carrying the measured tokens saved — the
	// engine itself never sees the request, so its recordAttempt
	// never runs for it. The hit is marked via LastCacheHit so the
	// server can label it honestly; it is never marked stale. The
	// hit is also stored under this request's exact key, so a
	// paraphrase that hit semantically becomes an exact hit next
	// time.
	start := time.Now()
	if cached, kind, sim, _, vecSource, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		e.store(ctx, req, resp)
		e.setCacheOutcomeSource(kind, sim, vecSource)
		e.recordCacheHitLedger(req, cached, kind, time.Since(start).Milliseconds())
		return resp, nil
	}
	// Reasoning replay (M6.6): restore any cached reasoning onto
	// the request before it goes upstream, so a thinking-model
	// turn the client replayed without reasoning does not 400.
	req = e.injectReasoning(ctx, req)
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

// Embed forwards one embeddings call to the wrapped engine
// (CHECKLIST 171-172): no cache lookup, no proactive serving, no
// stale fallback — an embeddings request is replay-exempt
// (exactly one attempt, never replayed), so serving a cached or
// stale vector would hand out another request's embedding and
// hide the upstream call the ledger must record. The wrapped
// engine must implement the embeddings entry point
// (router.Engine does); any other engine answers an honest
// error, never a faked vector.
func (e *StaleEngine) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	if ee, ok := e.engine.(interface {
		Embed(context.Context, provider.EmbeddingRequest) (*provider.EmbeddingResponse, error)
	}); ok {
		return ee.Embed(ctx, req)
	}
	return nil, errors.New("degrade: wrapped engine does not support embeddings")
}

// Rerank forwards one rerank call to the wrapped engine
// (CHECKLIST 171-172): same no-cache, no-stale rule as Embed —
// a rerank scores the caller's documents once, and replaying it
// (or serving a cached scoring) would double-apply upstream work.
func (e *StaleEngine) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	if ee, ok := e.engine.(interface {
		Rerank(context.Context, provider.RerankRequest) (*provider.RerankResponse, error)
	}); ok {
		return ee.Rerank(ctx, req)
	}
	return nil, errors.New("degrade: wrapped engine does not support rerank")
}

// AudioPassthrough, Images and MediaPassthrough forward the
// single-attempt passthrough surfaces (audio, images, search / OCR /
// video / music) to the wrapped engine, with the same no-cache,
// no-stale rule as Embed: each is replay-exempt, so a cached or stale
// answer would hand out another request's clip, image or task and
// hide the upstream call the ledger must record. Before Phase 0
// (2026-11-21) these were missing, so the server's optional-interface
// checks failed on the wrapper and every one of these routes answered
// 503 in production even though router.Engine serves them.
func (e *StaleEngine) AudioPassthrough(ctx context.Context, req router.AudioRequest) (*router.AudioResponse, error) {
	if ae, ok := e.engine.(interface {
		AudioPassthrough(context.Context, router.AudioRequest) (*router.AudioResponse, error)
	}); ok {
		return ae.AudioPassthrough(ctx, req)
	}
	return nil, errors.New("degrade: wrapped engine does not support audio")
}

func (e *StaleEngine) Images(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	if ie, ok := e.engine.(interface {
		Images(context.Context, provider.ImageRequest) (*provider.ImageResponse, error)
	}); ok {
		return ie.Images(ctx, req)
	}
	return nil, errors.New("degrade: wrapped engine does not support images")
}

func (e *StaleEngine) MediaPassthrough(ctx context.Context, req provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error) {
	if me, ok := e.engine.(interface {
		MediaPassthrough(context.Context, provider.MediaPassthroughRequest) (*provider.MediaPassthroughResponse, error)
	}); ok {
		return me.MediaPassthrough(ctx, req)
	}
	return nil, errors.New("degrade: wrapped engine does not support media passthrough")
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
	start := time.Now()
	// Proactive cache serving for streams: a hit is replayed as a
	// single delta, exactly like a stale replay, and no provider is
	// called. A stream that already delivered content never reaches
	// this path (lookup happens before any delta), so nothing can
	// be duplicated.
	if cached, kind, sim, _, vecSource, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		// Publish the outcome BEFORE delivering the replayed
		// chunk (CHECKLIST 199): a caller that reads
		// LastCacheHit from inside the delivery callback — the
		// server's first-chunk decision-header guard — must see
		// THIS request's cache hit, not the previous request's
		// outcome. A delivery error still resets it below via
		// setOutcome(false, 0).
		e.setCacheOutcomeSource(kind, sim, vecSource)
		if cached.Content != "" {
			if err := onDelta(cached.Content); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.recordCacheHitLedger(req, cached, kind, time.Since(start).Milliseconds())
		return resp, nil
	}
	// Reasoning replay (M6.6), as in Chat: restore cached
	// reasoning before the upstream stream starts.
	req = e.injectReasoning(ctx, req)
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
	// chunk. An empty cached content needs no delta at all. The
	// stale outcome is published BEFORE the delivery, for the same
	// reason as the proactive path above: a caller reading
	// LastServedStale from inside the callback must see this
	// request's stale serve.
	e.setOutcome(true, age)
	if cached.Content != "" {
		if err := onDelta(cached.Content); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	return toChatResponse(req, cached), nil
}

// ChatStreamFrom resumes a stream from skipChars runes in.
//
// PROACTIVE CACHE SERVING (exact layer only): before anything goes
// upstream, an exact hit is replayed as a single delta, trimmed by
// skipChars through the same trimming below — the entry is the
// response to this identical request, so the trimmed suffix is
// exactly what the client is missing and nothing is duplicated.
// The semantic layer is never consulted on a resume: a semantic
// hit is a different request's answer, and trimming it by rune
// count would splice two different texts. (The stale fallback
// below serves exact entries only for the same reason.)
//
// Reasoning replay (M6.6b), as in ChatStreamFromChunks: cached
// reasoning is restored before the upstream resume starts, so a
// thinking-model turn resumed without reasoning does not 400 —
// the router's transcript-keyed injection below covers turns it
// keyed, and this layer's tool-call-key hits are the extra
// coverage. Injection touches only the upstream request's
// assistant messages, never the deltas, so it cannot duplicate
// content to the client.
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
	start := time.Now()
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
	// Proactive exact-cache serving: the lookup runs BEFORE
	// injectReasoning below — the scoped key hashes the messages
	// as sent, so an injected reasoning state would key a
	// different entry and the hit would be missed (same order as
	// Chat / ChatStream / ChatStreamChunks).
	if cached, _, ok := e.exactLookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		// Publish the outcome BEFORE delivering the replayed
		// chunk (CHECKLIST 199): a caller that reads
		// LastCacheHit from inside the delivery callback must
		// see THIS request's cache hit, not the previous
		// request's outcome. A delivery error still resets it
		// below via setOutcome(false, 0).
		e.setCacheOutcome("exact", 0)
		if cached.Content != "" {
			if err := trimming(cached.Content); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.recordCacheHitLedger(req, cached, "exact", time.Since(start).Milliseconds())
		return resp, nil
	}
	req = e.injectReasoning(ctx, req)
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
	start := time.Now()
	// Proactive cache serving for chunk streams: a hit is replayed
	// as a single text chunk (tool calls cannot be reconstructed
	// from a content-only cached entry — same limitation as a stale
	// replay — and cacheable requests carry no tools anyway).
	// The outcome is published BEFORE the delivery (CHECKLIST 199):
	// a caller reading LastCacheHit from inside the chunk callback
	// must see this request's cache hit, not the previous
	// request's outcome. A delivery error still resets it below
	// via setOutcome(false, 0).
	if cached, kind, sim, _, vecSource, ok := e.lookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		e.setCacheOutcomeSource(kind, sim, vecSource)
		if cached.Content != "" {
			if err := onChunk(provider.StreamChunk{Content: cached.Content}); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.recordCacheHitLedger(req, cached, kind, time.Since(start).Milliseconds())
		return resp, nil
	}
	// Reasoning replay (M6.6b), as in ChatStream: restore cached
	// reasoning before the upstream chunk stream starts. The
	// server's streaming path uses this method, so without this
	// line a live stream never got degrade-level injection — only
	// the router's own store below did, and only for turns it
	// keyed by transcript (this layer's tool-call-key hits are
	// the extra coverage). The stream closure below captures req
	// by variable, so the injected request is what goes upstream.
	req = e.injectReasoning(ctx, req)
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
	// The stale outcome is published BEFORE the delivery, for the
	// same reason as the proactive path above (CHECKLIST 199).
	e.setOutcome(true, age)
	if cached.Content != "" {
		if err := onChunk(provider.StreamChunk{Content: cached.Content}); err != nil {
			e.setOutcome(false, 0)
			return nil, err
		}
	}
	return toChatResponse(req, cached), nil
}

// ChatStreamFromChunks is the chunk-aware twin of ChatStreamFrom.
// Proactive exact-cache serving (see ChatStreamFrom) applies here
// too: an exact hit is replayed as a single text chunk, trimmed by
// skipChars; the semantic layer is never consulted on a resume.
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
	start := time.Now()
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
	// Proactive exact-cache serving: the lookup runs BEFORE
	// injectReasoning below — the scoped key hashes the messages
	// as sent, so an injected reasoning state would key a
	// different entry and the hit would be missed (same order as
	// Chat / ChatStream / ChatStreamChunks).
	if cached, _, ok := e.exactLookup(ctx, req); ok {
		resp := toChatResponse(req, cached)
		// Publish the outcome BEFORE the delivery (CHECKLIST
		// 199), as in ChatStreamChunks: a caller reading
		// LastCacheHit from inside the chunk callback must see
		// THIS request's cache hit.
		e.setCacheOutcome("exact", 0)
		if cached.Content != "" {
			if err := trimming(provider.StreamChunk{Content: cached.Content}); err != nil {
				e.setOutcome(false, 0)
				return nil, err
			}
		}
		e.store(ctx, req, resp)
		e.recordCacheHitLedger(req, cached, "exact", time.Since(start).Milliseconds())
		return resp, nil
	}
	// Reasoning replay (M6.6b), as in ChatStreamChunks: restore
	// cached reasoning before the upstream resume starts, so both
	// the delegated chunk-resume path and the replay fallback
	// below carry it. Injection touches only the upstream
	// request's assistant messages — it cannot duplicate content
	// to the client, whose prefix is guarded by the skipChars
	// trimming above. (The text-only ChatStreamFrom injects the
	// same way now; the router's transcript-keyed injection below
	// covers the turns this layer does not.)
	req = e.injectReasoning(ctx, req)
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

// LastContextFit returns the wrapped engine's most recent
// context-window fit-check decision when it exposes one
// (router.Engine does), so the dashboard can show the fit per
// model for the last request through this wrapper. It returns
// ok=false when the wrapped engine has no decision to offer (no
// request yet, or no request reached the fit gate).
func (e *StaleEngine) LastContextFit() (router.ContextFitDecision, bool) {
	if cf, ok := e.engine.(interface {
		LastContextFit() (router.ContextFitDecision, bool)
	}); ok {
		return cf.LastContextFit()
	}
	return router.ContextFitDecision{}, false
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

// TokenCountStats returns the wrapped engine's local-token-count
// reconciliation counters when it exposes them (router.Engine
// does), so the dashboard can show real local-vs-reported counts
// through this wrapper. It returns zero counters when the wrapped
// engine has none to offer — an empty state, never invented
// figures.
func (e *StaleEngine) TokenCountStats() router.TokenCountStats {
	if ts, ok := e.engine.(interface{ TokenCountStats() router.TokenCountStats }); ok {
		return ts.TokenCountStats()
	}
	return router.TokenCountStats{}
}

// ReasoningStats returns the wrapped engine's reasoning replay
// counters when it exposes them (router.Engine does), so the
// dashboard can show real capture/restore counts through this
// wrapper. It returns zero counters when the wrapped engine has
// none to offer — an empty state, never invented figures.
func (e *StaleEngine) ReasoningStats() reasoning.Stats {
	if rs, ok := e.engine.(interface{ ReasoningStats() reasoning.Stats }); ok {
		return rs.ReasoningStats()
	}
	return reasoning.Stats{}
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

// LastRouting returns the wrapped engine's most recent routing
// record (strategy, model, serving provider) when it exposes one
// (router.Engine does), so the dashboard can show the live strategy
// through this wrapper. It returns ok=false when the wrapped engine
// has none to offer (no request yet, or no routing engine).
func (e *StaleEngine) LastRouting() (router.RoutingInfo, bool) {
	if lr, ok := e.engine.(interface {
		LastRouting() (router.RoutingInfo, bool)
	}); ok {
		return lr.LastRouting()
	}
	return router.RoutingInfo{}, false
}

// StrategyState returns the wrapped engine's runtime strategy
// overrides (global + per-chain) when it exposes them
// (router.Engine does), so the dashboard strategies feed reads live
// state through this wrapper. It returns "" and an empty map when
// the wrapped engine has none to offer.
func (e *StaleEngine) StrategyState() (string, map[string]string) {
	if ss, ok := e.engine.(interface {
		StrategyState() (string, map[string]string)
	}); ok {
		return ss.StrategyState()
	}
	return "", map[string]string{}
}

// SetGlobalStrategy forwards a runtime global-strategy override to
// the wrapped engine when it accepts one (router.Engine does). With
// any other engine it is a no-op — never an error, never a panic.
func (e *StaleEngine) SetGlobalStrategy(s string) {
	if ss, ok := e.engine.(interface{ SetGlobalStrategy(string) }); ok {
		ss.SetGlobalStrategy(s)
	}
}

// SetChainStrategy forwards a runtime per-chain strategy override
// to the wrapped engine when it accepts one (router.Engine does).
// With any other engine it is a no-op.
func (e *StaleEngine) SetChainStrategy(model, strategy string) {
	if ss, ok := e.engine.(interface{ SetChainStrategy(string, string) }); ok {
		ss.SetChainStrategy(model, strategy)
	}
}

// CooldownTargets returns the wrapped engine's currently cooling
// providers when it exposes them (router.Engine does), so the
// dashboard cooldown feed reads live state through this wrapper.
// It returns an empty list when the wrapped engine has none to
// offer — the honest empty state, never invented targets.
func (e *StaleEngine) CooldownTargets() []router.CoolingTarget {
	if ct, ok := e.engine.(interface{ CooldownTargets() []router.CoolingTarget }); ok {
		return ct.CooldownTargets()
	}
	return nil
}

// BreakerStates returns the wrapped engine's live per-provider
// circuit-breaker states when it exposes them (router.Engine
// does), so the dashboard cooldown feed reads live breaker state
// through this wrapper. It returns an empty list when the wrapped
// engine has none to offer — the honest empty state, never
// invented breakers.
func (e *StaleEngine) BreakerStates() []router.BreakerStatus {
	if bs, ok := e.engine.(interface{ BreakerStates() []router.BreakerStatus }); ok {
		return bs.BreakerStates()
	}
	return nil
}

// ModelLockouts returns the wrapped engine's live (unexpired)
// model lockouts when it exposes them (router.Engine does), so the
// candidate-pool endpoint (autoComboModelLockouts) reports locked
// models in serve mode instead of failing closed. It returns an
// empty list when the wrapped engine has none to offer — the
// honest empty state, never invented lockouts — and never panics
// on a nil receiver or a nil wrapped engine. The savingsEngine
// decorator embeds *StaleEngine, so it inherits this method and
// needs no forwarder of its own.
func (e *StaleEngine) ModelLockouts() []router.LockoutStatus {
	if e == nil {
		return nil
	}
	if ml, ok := e.engine.(interface{ ModelLockouts() []router.LockoutStatus }); ok {
		return ml.ModelLockouts()
	}
	return nil
}

// breakerResetter is the local copy of the server's breakerResetter
// interface (internal/server/resilience_api.go: POST
// /api/resilience/breakers/reset), defined locally because the
// server's is unexported. router.Engine implements it
// (ResetBreaker / ResetAllBreakers,
// internal/router/router.go:2611/:2633).
type breakerResetter interface {
	ResetBreaker(provider string) bool
	ResetAllBreakers() []string
}

// ResetBreaker forwards a single-provider circuit-breaker reset to
// the wrapped engine when it implements breakerResetter
// (router.Engine does), so POST /api/resilience/breakers/reset is
// a real reset against the production-wrapped engine instead of a
// 501. It reports false when the wrapped engine knows no breaker
// for that provider — and also when the wrapped engine exposes no
// reset at all, in which case nothing was reset: the honest empty
// result, never a faked success, mirroring BreakerStates returning
// nil for an engine with no breaker state. (The fixed bool/[]string
// signatures cannot carry a "not implemented" distinct from "no
// breaker known", so through this wrapper a resetter-less engine
// surfaces as nothing-reset — false here, nil from
// ResetAllBreakers — and the handler's 501 stays reachable only
// for an engine wired directly without this wrapper; see
// internal/server/resilience_api.go: handleResilienceBreakerReset.)
func (e *StaleEngine) ResetBreaker(provider string) bool {
	if r, ok := e.engine.(breakerResetter); ok {
		return r.ResetBreaker(provider)
	}
	return false
}

// ResetAllBreakers forwards a reset-all to the wrapped engine when
// it implements breakerResetter (router.Engine does) and reports
// the providers it actually reset. It returns nil when the wrapped
// engine exposes no reset at all — nothing reset, never a faked
// list — mirroring BreakerStates returning nil for an engine with
// no breaker state (see ResetBreaker for the 501 note).
func (e *StaleEngine) ResetAllBreakers() []string {
	if r, ok := e.engine.(breakerResetter); ok {
		return r.ResetAllBreakers()
	}
	return nil
}

// Ensure StaleEngine satisfies breakerResetter at compile time, so
// the server's s.engine.(breakerResetter) assertion in
// handleResilienceBreakerReset succeeds against the
// production-wrapped engine. *savingsEngine needs no separate
// methods: it embeds *StaleEngine, so both reset methods promote
// through the embedding (proven by the WithSavings subtest in
// internal/server/resilience_breaker_reset_wrapped_test.go).
var _ breakerResetter = (*StaleEngine)(nil)

// AllProviderStats returns the wrapped engine's live per-provider
// attempt record when it exposes it (router.Engine does), so the
// dashboard groups feed reads live member health through this
// wrapper. It returns an empty list when the wrapped engine has
// none to offer — the honest empty state, never invented signals.
func (e *StaleEngine) AllProviderStats() []router.ProviderStats {
	if ps, ok := e.engine.(interface{ AllProviderStats() []router.ProviderStats }); ok {
		return ps.AllProviderStats()
	}
	return nil
}

// CooldownState returns the wrapped engine's runtime cooldown-mode
// overrides (global + per-chain) when it exposes them
// (router.Engine does), so the dashboard cooldown feed reads live
// state through this wrapper. It returns "" and an empty map when
// the wrapped engine has none to offer.
func (e *StaleEngine) CooldownState() (string, map[string]string) {
	if cs, ok := e.engine.(interface {
		CooldownState() (string, map[string]string)
	}); ok {
		return cs.CooldownState()
	}
	return "", map[string]string{}
}

// SetCooldownMode forwards a runtime global cooldown-mode override
// to the wrapped engine when it accepts one (router.Engine does).
// With any other engine it is a no-op — never an error, never a
// panic.
func (e *StaleEngine) SetCooldownMode(mode string) {
	if cs, ok := e.engine.(interface{ SetCooldownMode(string) }); ok {
		cs.SetCooldownMode(mode)
	}
}

// SetChainCooldownMode forwards a runtime per-chain cooldown-mode
// override to the wrapped engine when it accepts one
// (router.Engine does). With any other engine it is a no-op.
func (e *StaleEngine) SetChainCooldownMode(model, mode string) {
	if cs, ok := e.engine.(interface{ SetChainCooldownMode(string, string) }); ok {
		cs.SetChainCooldownMode(model, mode)
	}
}

// Ensure StaleEngine satisfies server.StreamResumer at compile time.
var _ server.StreamResumer = (*StaleEngine)(nil)

// Ensure StaleEngine satisfies the chunk interfaces (defined locally
// because server's are unexported). Without these, the server's
// chunk-aware streaming path would silently fall back to text-only
// for every request in serve mode.
var _ chunkStreamer = (*StaleEngine)(nil)
var _ chunkResumer = (*StaleEngine)(nil)
