// Package router implements the Codvian Router routing engine: fallback
// chains, quota-aware ordering, 429 / Retry-After handling with backoff,
// per-provider circuit breakers, hedged requests and streaming-safe
// failover with mid-stream resume.
//
// The engine is transport-agnostic: provider access comes through the
// provider.Client interface (internal/provider) and usage accounting
// through the Ledger interface, both injected. There is no SQLite, HTTP
// or other concrete I/O in this package, so it is unit-testable with
// fake clients.
//
// Chat types (Message, ChatRequest, ChatResponse, RateLimitError,
// QuotaError, UpstreamError) come from internal/provider. One mismatch
// remains between the two layers and is bridged here, not hidden:
// provider.Client streams by callback (ChatStream with onDelta), while
// the engine's streaming core (hedging, first-event races, resume)
// consumes a channel of StreamEvents. bridgeStream adapts one to the
// other; the engine never re-implements the upstream client.
//
// The Engine also satisfies the server.Engine contract
// (internal/server): Chat / ChatStream over provider types, with the
// fallback chain resolved per request model (Config.ChainFor, or every
// registered client by default), plus the optional StreamResumer
// contract (ChatStreamFrom) which replays a fresh internal stream and
// skips the runes the client already received.
//
// Design targets (goals, not claims): detect a failed provider in 2-3s,
// switch to the next attempt in 0.1-0.3s, route a request in under 1s.
// Nothing here hardcodes those numbers; the timeouts and delays are
// configurable in Config and the tests measure the behaviour that can
// be measured locally (hedge timing, attempt counts, resume content).
package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// Message roles.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

// StreamEvent is one event from a streaming chat response. Exactly one
// of Delta / Done / Err is meaningful per event: Delta carries text,
// Done marks clean completion, Err marks failure.
type StreamEvent struct {
	Delta string
	Done  bool
	Err   error
}

// Ledger records one provider attempt for usage accounting. It is
// defined here (not imported) so the engine depends only on this small
// interface; the concrete SQLite ledger lives in another package.
//
// Usage is the full token split reported by the upstream (see
// provider.Usage). The engine passes it through verbatim and never
// invents a split: failed attempts and streaming attempts record
// Usage{} (all zeros), because their token usage is unknown to the
// engine — the ledger must record that as unknown, not as a guessed
// prompt/completion split or a made-up cost.
type Ledger interface {
	RecordAttempt(provider, model string, ok bool, latencyMs int64, usage provider.Usage)
}

// ---------------------------------------------------------------------------
// Routing types.
// ---------------------------------------------------------------------------

// Attempt is one step of a fallback chain: a provider and a model on it.
type Attempt struct {
	ProviderID string
	Model      string
}

// ErrNoAvailableAttempt is returned when a chain has no attempt the
// engine is willing to try (empty chain, all providers zero-quota, or
// all skipped by breaker/cooldown).
var ErrNoAvailableAttempt = errors.New("router: no attempts available (all skipped or chain empty)")

// Config tunes the engine. Zero values get the documented defaults.
// Set HedgeDelay negative to disable hedging entirely.
type Config struct {
	// MaxAttempts caps the total number of provider calls for one
	// request across the whole chain, including 429 retries and
	// hedged second attempts. Default 5.
	MaxAttempts int
	// HedgeDelay is how long to wait for the first attempt's first
	// response before starting the next attempt in parallel.
	// Default 150ms; negative disables hedging.
	HedgeDelay time.Duration
	// BackoffBase is the first exponential-backoff delay used when a
	// provider 429s without a Retry-After hint. Default 200ms.
	BackoffBase time.Duration
	// BackoffMax caps exponential backoff. Default 5s.
	BackoffMax time.Duration
	// BreakerThreshold is how many consecutive failures open a
	// provider's circuit breaker. Default 5.
	BreakerThreshold int
	// BreakerOpenFor is how long an open breaker stays open before a
	// half-open probe is allowed. Default 30s.
	BreakerOpenFor time.Duration
	// RetryAfterCap caps a provider's Retry-After hint. Default 30s.
	RetryAfterCap time.Duration
	// QuotaHint, if set, reports a provider's remaining quota
	// (higher = more headroom). Providers reporting 0 are skipped.
	// If nil, every provider is treated as having ample quota.
	QuotaHint func(providerID string) int
	// ChainFor, if set, builds the fallback chain for a request model.
	// It is used by the server-facing Chat / ChatStream / ChatStreamFrom
	// methods, which take no explicit chain. If nil, the chain is every
	// registered client for the model, in sorted provider-ID order.
	// Callers that know which providers offer a model (e.g. main, from
	// the registry) should set this so providers that do not offer the
	// model are never tried.
	ChainFor func(model string) []Attempt
	// OrderHook, if set, is applied as the FINAL reordering step of
	// orderAttempts, after zero-quota filtering and quota-hint sorting.
	// It receives the quota-ordered chain and returns the order to try.
	// This is the frozen extension point for cost/latency/quality
	// scoring (internal/scoring): scorers live in their own package
	// and never edit this file. The hook must be deterministic for a
	// given input, must not drop or duplicate attempts, and must not
	// block. If nil, quota order stands.
	OrderHook func(chain []Attempt) []Attempt
}

func (c Config) withDefaults() Config {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.HedgeDelay == 0 {
		c.HedgeDelay = 150 * time.Millisecond
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 200 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Second
	}
	if c.BreakerThreshold <= 0 {
		c.BreakerThreshold = 5
	}
	if c.BreakerOpenFor <= 0 {
		c.BreakerOpenFor = 30 * time.Second
	}
	if c.RetryAfterCap <= 0 {
		c.RetryAfterCap = 30 * time.Second
	}
	return c
}

// ---------------------------------------------------------------------------
// Engine.
// ---------------------------------------------------------------------------

type breakerState struct {
	consecutiveFailures int
	openedAt            time.Time
	probeInFlight       bool
}

// Engine routes chat requests across providers.
type Engine struct {
	clients map[string]provider.Client
	ledger  Ledger
	cfg     Config

	mu         sync.Mutex
	breakers   map[string]*breakerState
	cooldowns  map[string]time.Time
	quotaHints map[string]int
	lastTrace  *routetrace.RouteTrace

	randMu sync.Mutex
	rand   *rand.Rand
}

// NewEngine builds an Engine. clients is keyed by provider ID; the map
// is copied. ledger may be nil (no usage accounting).
func NewEngine(clients map[string]provider.Client, ledger Ledger, cfg Config) *Engine {
	cfg = cfg.withDefaults()
	cp := make(map[string]provider.Client, len(clients))
	for k, v := range clients {
		cp[k] = v
	}
	return &Engine{
		clients:    cp,
		ledger:     ledger,
		cfg:        cfg,
		breakers:   make(map[string]*breakerState),
		cooldowns:  make(map[string]time.Time),
		quotaHints: make(map[string]int),
		rand:       rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// SetQuotaHint overrides the remaining-quota hint for one provider.
// A hint of 0 means "no quota left": the provider is skipped.
// Overrides take precedence over Config.QuotaHint.
func (e *Engine) SetQuotaHint(providerID string, hint int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.quotaHints[providerID] = hint
}

// addTraceAttempt appends one attempt to a trace. It exists because
// RouteTrace.Add takes plain provider/model strings (the trace package
// cannot import router.Attempt), while call sites here already have the
// Attempt in hand.
func addTraceAttempt(t *routetrace.RouteTrace, att Attempt, latencyMs int64, outcome string, err error) {
	t.Add(att.ProviderID, att.Model, latencyMs, outcome, err)
}

// LastTrace returns the RouteTrace of the most recently finished
// request (Chat or ChatStream). It is nil before any request.
func (e *Engine) LastTrace() *routetrace.RouteTrace {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastTrace
}

func (e *Engine) setLastTrace(t *routetrace.RouteTrace) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastTrace = t
}

// quotaHint returns the remaining-quota hint for a provider; providers
// without a hint are treated as having ample quota (100).
func (e *Engine) quotaHint(providerID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if h, ok := e.quotaHints[providerID]; ok {
		return h
	}
	if e.cfg.QuotaHint != nil {
		return e.cfg.QuotaHint(providerID)
	}
	return 100
}

// QuotaHintFunc adapts a provider registry to Config.QuotaHint, so the
// engine reads live quota hints on every request: a
// Registry.SetQuotaHint update takes effect without restarting the
// engine (or the process). Mapping: an unknown provider ID, or a
// provider whose hint is QuotaUnknown (-1), reports 100 (ample quota);
// a hint f in [0, 1] reports int(f*100), so 0 means "no quota left"
// and the engine skips that provider. A nil registry reports 100 for
// every provider.
func QuotaHintFunc(reg *provider.Registry) func(string) int {
	return func(providerID string) int {
		if reg == nil {
			return 100
		}
		p, ok := reg.Get(providerID)
		if !ok {
			return 100
		}
		if p.QuotaHint == provider.QuotaUnknown {
			return 100
		}
		if p.QuotaHint < 0 || p.QuotaHint > 1 {
			return 100
		}
		return int(p.QuotaHint * 100)
	}
}

// orderAttempts filters out zero-quota attempts and stably sorts the
// rest by quota hint descending, so the most headroom is tried first
// while the caller's chain order is preserved among equal hints.
func (e *Engine) orderAttempts(chain []Attempt) (ordered, zeroSkipped []Attempt) {
	type scored struct {
		att  Attempt
		hint int
	}
	var items []scored
	for _, att := range chain {
		h := e.quotaHint(att.ProviderID)
		if h <= 0 {
			zeroSkipped = append(zeroSkipped, att)
			continue
		}
		items = append(items, scored{att: att, hint: h})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].hint > items[j].hint })
	for _, it := range items {
		ordered = append(ordered, it.att)
	}
	if e.cfg.OrderHook != nil {
		ordered = e.cfg.OrderHook(ordered)
	}
	return ordered, zeroSkipped
}

// --- circuit breaker ---

// breakerAllow reports whether a provider may be tried now. It also
// claims the single half-open probe slot when a breaker has cooled
// down: at most one probe runs at a time; further callers see the
// breaker as open until the probe resolves.
func (e *Engine) breakerAllow(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.breakers[provider]
	if st == nil || st.consecutiveFailures < e.cfg.BreakerThreshold {
		return true
	}
	if st.openedAt.IsZero() {
		return true
	}
	if time.Since(st.openedAt) >= e.cfg.BreakerOpenFor {
		if st.probeInFlight {
			return false
		}
		st.probeInFlight = true
		return true
	}
	return false
}

// breakerOpen is a read-only check used when deciding whether a second
// attempt is even worth hedging to. Unlike breakerAllow it never claims
// the half-open probe slot.
func (e *Engine) breakerOpen(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.breakers[provider]
	if st == nil || st.consecutiveFailures < e.cfg.BreakerThreshold {
		return false
	}
	if st.openedAt.IsZero() {
		return false
	}
	return time.Since(st.openedAt) < e.cfg.BreakerOpenFor
}

func (e *Engine) breakerSuccess(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.breakers, provider)
}

func (e *Engine) breakerFailure(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.breakers[provider]
	if st == nil {
		st = &breakerState{}
		e.breakers[provider] = st
	}
	if st.probeInFlight {
		// Half-open probe failed: reopen immediately.
		st.probeInFlight = false
		st.consecutiveFailures = e.cfg.BreakerThreshold
		st.openedAt = time.Now()
		return
	}
	st.consecutiveFailures++
	if st.consecutiveFailures >= e.cfg.BreakerThreshold {
		st.openedAt = time.Now()
	}
}

// breakerNeutral releases a claimed half-open probe without counting a
// success or a failure. Used for 429s (throttling, not provider-down)
// and for attempts cancelled as hedge losers.
func (e *Engine) breakerNeutral(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if st := e.breakers[provider]; st != nil {
		st.probeInFlight = false
	}
}

// --- cooldowns ---

func (e *Engine) onCooldown(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	until, ok := e.cooldowns[provider]
	return ok && time.Now().Before(until)
}

func (e *Engine) setCooldown(provider string, d time.Duration) {
	if d <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cooldowns[provider] = time.Now().Add(d)
}

func (e *Engine) clearCooldown(provider string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cooldowns, provider)
}

// --- helpers ---

func (e *Engine) hedgeSecondAvailable(att Attempt) bool {
	return !e.breakerOpen(att.ProviderID) && !e.onCooldown(att.ProviderID)
}

func (e *Engine) hasClient(providerID string) bool {
	_, ok := e.clients[providerID]
	return ok
}

// sleep waits for d or until ctx is done. It reports false if ctx ended.
func (e *Engine) sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// rateLimitWait computes how long to honour a 429: the provider's
// Retry-After hint capped at RetryAfterCap, or exponential backoff
// (base * 2^(n-1), capped at BackoffMax) with jitter in [0.5x, 1x]
// when there is no hint. n is the consecutive-429 count (1-based).
func (e *Engine) rateLimitWait(retryAfter time.Duration, n int) time.Duration {
	if retryAfter > 0 {
		if retryAfter > e.cfg.RetryAfterCap {
			return e.cfg.RetryAfterCap
		}
		return retryAfter
	}
	shift := n - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 10 {
		shift = 10
	}
	d := e.cfg.BackoffBase << uint(shift)
	if d > e.cfg.BackoffMax || d <= 0 {
		d = e.cfg.BackoffMax
	}
	e.randMu.Lock()
	f := 0.5 + e.rand.Float64()*0.5
	e.randMu.Unlock()
	return time.Duration(float64(d) * f)
}

// asRateLimit extracts the Retry-After hint from a *RateLimitError
// (including wrapped ones). provider.RateLimitError only implements
// error on its pointer receiver, so only the pointer form is accepted.
func asRateLimit(err error) (time.Duration, bool) {
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		return rle.RetryAfter, true
	}
	return 0, false
}

func reqWithModel(req provider.ChatRequest, att Attempt) provider.ChatRequest {
	r := req
	r.Model = att.Model
	return r
}

// attemptResult is the outcome of one provider call.
type attemptResult struct {
	att       Attempt
	resp      *provider.ChatResponse
	err       error
	latencyMs int64
	called    bool // a real client call was made (false: unknown provider)
}

func (e *Engine) callChat(ctx context.Context, att Attempt, req provider.ChatRequest) attemptResult {
	client, ok := e.clients[att.ProviderID]
	if !ok {
		return attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	start := time.Now()
	resp, err := client.Chat(ctx, reqWithModel(req, att))
	return attemptResult{att: att, resp: resp, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
}

// noteSuccess records a successful attempt in the trace, ledger and
// circuit breaker.
func (e *Engine) noteSuccess(trace *routetrace.RouteTrace, res attemptResult) {
	addTraceAttempt(trace, res.att, res.latencyMs, "success", nil)
	if e.ledger != nil && res.called {
		usage := provider.Usage{}
		if res.resp != nil {
			usage = res.resp.Usage
		}
		e.ledger.RecordAttempt(res.att.ProviderID, res.att.Model, true, res.latencyMs, usage)
	}
	e.breakerSuccess(res.att.ProviderID)
}

// noteFailure records a failed attempt. It returns whether the error
// was a rate limit and, if so, how long the provider was cooled down
// for. 429s do NOT trip the circuit breaker (throttling is not the
// provider being down); they set a per-provider cooldown instead.
// Context cancellation and unknown providers likewise never touch the
// breaker.
func (e *Engine) noteFailure(trace *routetrace.RouteTrace, res attemptResult, consecutive429 *int) (rateLimited bool, wait time.Duration) {
	if e.ledger != nil && res.called {
		// Failed attempt: no usage was reported, so record Usage{}
		// (unknown), never a guessed split.
		e.ledger.RecordAttempt(res.att.ProviderID, res.att.Model, false, res.latencyMs, provider.Usage{})
	}
	if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
		addTraceAttempt(trace, res.att, res.latencyMs, "error", res.err)
		return false, 0
	}
	if !e.hasClient(res.att.ProviderID) {
		addTraceAttempt(trace, res.att, res.latencyMs, "error", res.err)
		return false, 0
	}
	if ra, ok := asRateLimit(res.err); ok {
		*consecutive429++
		wait = e.rateLimitWait(ra, *consecutive429)
		e.setCooldown(res.att.ProviderID, wait)
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttempt(trace, res.att, res.latencyMs, "rate_limited", res.err)
		return true, wait
	}
	*consecutive429 = 0
	e.breakerFailure(res.att.ProviderID)
	addTraceAttempt(trace, res.att, res.latencyMs, "error", res.err)
	return false, 0
}

// noteHedgedLoser records an attempt that was started as a hedge and
// then cancelled because the other attempt won.
func (e *Engine) noteHedgedLoser(trace *routetrace.RouteTrace, res attemptResult) {
	addTraceAttempt(trace, res.att, res.latencyMs, "hedged_cancelled", context.Canceled)
	if e.ledger != nil && res.called {
		// Cancelled hedge loser: no usage was reported, so record
		// Usage{} (unknown), never a guessed split.
		e.ledger.RecordAttempt(res.att.ProviderID, res.att.Model, false, res.latencyMs, provider.Usage{})
	}
	e.breakerNeutral(res.att.ProviderID)
}

// ---------------------------------------------------------------------------
// Non-streaming chat.
// ---------------------------------------------------------------------------

// ChatWithChain routes one request through the fallback chain and returns
// the first successful response, the RouteTrace of every attempt, and nil;
// or a nil response, the trace, and the last error if no attempt
// succeeded. At most Config.MaxAttempts provider calls are made. The
// serving provider is recorded in the trace (RouteTrace.Attempts); the
// response itself carries only the provider's content, usage and model.
//
// The server-facing Chat (no chain argument) delegates here after
// resolving the chain for req.Model; see chainFor.
func (e *Engine) ChatWithChain(ctx context.Context, chain []Attempt, req provider.ChatRequest) (*provider.ChatResponse, *routetrace.RouteTrace, error) {
	trace := &routetrace.RouteTrace{}
	if len(chain) == 0 {
		return nil, trace, errors.New("router: empty fallback chain")
	}
	if err := ctx.Err(); err != nil {
		return nil, trace, err
	}
	ordered, zeroSkipped := e.orderAttempts(chain)
	for _, att := range zeroSkipped {
		addTraceAttempt(trace, att, 0, "skipped_zero_quota", nil)
	}
	if len(ordered) == 0 {
		return nil, trace, ErrNoAvailableAttempt
	}

	maxAttempts := e.cfg.MaxAttempts
	attemptsUsed := 0
	consecutive429 := 0
	var lastErr error
	idx := 0
	for idx < len(ordered) && attemptsUsed < maxAttempts {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break
		}
		att := ordered[idx]
		if e.onCooldown(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_cooldown", nil)
			idx++
			continue
		}
		if !e.breakerAllow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_breaker", nil)
			idx++
			continue
		}

		// Hedged first attempt: only for the first attempt, only
		// while budget remains, and only if the second attempt is
		// actually available to hedge to.
		if idx == 0 && attemptsUsed == 0 && e.cfg.HedgeDelay > 0 && len(ordered) > 1 && e.hedgeSecondAvailable(ordered[1]) {
			r1, r2, launched := e.hedgedChat(ctx, att, ordered[1], req)
			attemptsUsed++
			if launched {
				attemptsUsed++
			}
			if r1.err == nil {
				e.noteSuccess(trace, r1)
				if launched && r2 != nil {
					e.noteHedgedLoser(trace, *r2)
				}
				e.setLastTrace(trace)
				return e.finishResponse(r1), trace, nil
			}
			_, _ = e.noteFailure(trace, r1, &consecutive429)
			lastErr = r1.err
			if !launched {
				// First attempt failed before the hedge fired;
				// fall through to the next attempt normally.
				idx = 1
				continue
			}
			if r2 != nil {
				if r2.err == nil {
					e.noteSuccess(trace, *r2)
					e.setLastTrace(trace)
					return e.finishResponse(*r2), trace, nil
				}
				_, _ = e.noteFailure(trace, *r2, &consecutive429)
				lastErr = r2.err
			}
			idx = 2
			continue
		}

		res := e.callChat(ctx, att, req)
		attemptsUsed++
		if res.err == nil {
			e.noteSuccess(trace, res)
			e.setLastTrace(trace)
			return e.finishResponse(res), trace, nil
		}
		lastErr = res.err
		isRL, wait := e.noteFailure(trace, res, &consecutive429)
		if isRL {
			if idx+1 < len(ordered) && attemptsUsed < maxAttempts {
				// Fail over immediately: waiting out a 429 is
				// slower than switching providers.
				idx++
				continue
			}
			if attemptsUsed < maxAttempts {
				// No further attempts: honour the wait, then
				// retry the same provider.
				if !e.sleep(ctx, wait) {
					lastErr = ctx.Err()
					break
				}
				e.clearCooldown(att.ProviderID)
				continue
			}
			break
		}
		idx++
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAttempt
	}
	e.setLastTrace(trace)
	return nil, trace, lastErr
}

func (e *Engine) finishResponse(res attemptResult) *provider.ChatResponse {
	resp := res.resp
	if resp == nil {
		resp = &provider.ChatResponse{}
	}
	if resp.Model == "" {
		resp.Model = res.att.Model
	}
	return resp
}

// hedgedChat races the first two attempts. The first attempt starts
// immediately; if it has not returned within HedgeDelay, the second
// starts in parallel and whichever returns first wins; the loser is
// cancelled. If the first finisher errored, the already-running other
// attempt is still given its chance to succeed before giving up, since
// it was already launched. It returns the first result, the second
// result (nil when the hedge never fired), and whether the second
// attempt was launched.
func (e *Engine) hedgedChat(ctx context.Context, first, second Attempt, req provider.ChatRequest) (attemptResult, *attemptResult, bool) {
	start := time.Now()
	resCh := make(chan attemptResult, 2)
	ctxA, cancelA := context.WithCancel(ctx)
	defer cancelA()
	go func() { resCh <- e.callChat(ctxA, first, req) }()

	timer := time.NewTimer(e.cfg.HedgeDelay)
	defer timer.Stop()
	select {
	case r := <-resCh:
		return r, nil, false
	case <-timer.C:
	case <-ctx.Done():
		return attemptResult{att: first, err: ctx.Err(), latencyMs: time.Since(start).Milliseconds()}, nil, false
	}

	ctxB, cancelB := context.WithCancel(ctx)
	defer cancelB()
	go func() { resCh <- e.callChat(ctxB, second, req) }()

	r1 := <-resCh
	if r1.err == nil {
		// Winner finished first; the loser is cancelled by the
		// deferred cancels on return.
		loser := second
		if r1.att == second {
			loser = first
		}
		return r1, &attemptResult{att: loser, err: context.Canceled, latencyMs: time.Since(start).Milliseconds(), called: true}, true
	}
	// First finisher errored; wait for the other one.
	select {
	case r2 := <-resCh:
		return r1, &r2, true
	case <-ctx.Done():
		other := second
		if r1.att == second {
			other = first
		}
		return r1, &attemptResult{att: other, err: ctx.Err(), latencyMs: time.Since(start).Milliseconds()}, true
	}
}

// ---------------------------------------------------------------------------
// Streaming chat.
//
// Resume strategy (streaming-safe failover): if a stream fails after
// emitting >= 1 delta, the engine re-issues the same request to the
// next attempt with the partial content already delivered appended as
// an assistant message (Messages + {assistant, partial}). The next
// provider therefore continues from the visible prefix instead of
// restarting, so the user sees one continuous stream. Trade-offs:
// (a) the resuming provider may repeat or rephrase part of the prefix
// — the engine does not de-duplicate; (b) the partial text is billed
// twice (once per provider); (c) if the partial ends mid-token/word
// the continuation may be slightly incoherent. A failure before any
// delta is a plain failover with the original request unchanged.
// ---------------------------------------------------------------------------

// ChatStreamWithChain routes a streaming request through the fallback
// chain and returns a channel of StreamEvents. The channel yields Deltas,
// ends with a Done event on success, or ends with an Err event if every
// attempt failed; it is then closed. The returned error is non-nil
// only for a bad request (empty chain) or an already-done ctx —
// per-attempt failures are delivered as events, so callers must drain
// the channel. Use Engine.LastTrace after the channel closes to see
// every attempt. Callers must either drain the channel or cancel ctx,
// otherwise a blocked send can stall the engine goroutine.
//
// The server-facing ChatStream (no chain argument) delegates here after
// resolving the chain for req.Model; see chainFor.
func (e *Engine) ChatStreamWithChain(ctx context.Context, chain []Attempt, req provider.ChatRequest) (<-chan StreamEvent, error) {
	if len(chain) == 0 {
		return nil, errors.New("router: empty fallback chain")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make(chan StreamEvent, 16)
	go e.runStream(ctx, chain, req, out)
	return out, nil
}

// bridgeStream adapts a provider.Client's callback-based ChatStream to
// the channel of StreamEvents the engine's streaming core consumes.
// The channel yields deltas in order, then a Done event on clean
// completion or an Err event on failure, and is then closed. If ctx is
// cancelled (e.g. a hedged loser is abandoned) the bridge stops
// forwarding and the underlying client call unwinds via ctx.
func bridgeStream(ctx context.Context, client provider.Client, req provider.ChatRequest) <-chan StreamEvent {
	ch := make(chan StreamEvent, 16)
	go func() {
		defer close(ch)
		_, err := client.ChatStream(ctx, req, func(delta string) error {
			if delta == "" {
				return nil
			}
			select {
			case ch <- StreamEvent{Delta: delta}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil {
			select {
			case ch <- StreamEvent{Err: err}:
			case <-ctx.Done():
			}
			return
		}
		select {
		case ch <- StreamEvent{Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch
}

func (e *Engine) sendTerminal(out chan<- StreamEvent, err error) {
	if err == nil {
		return
	}
	select {
	case out <- StreamEvent{Err: err}:
	default: // receiver gone; channel close below still signals the end
	}
}

type streamOutcome int

const (
	streamDone streamOutcome = iota
	streamFailed
	streamCtxDone
)

// forwardStream forwards events from ch to out, appending deltas to
// partial. first, when non-nil, is an already-consumed first event
// (hedge path) that is emitted before reading ch. It returns when the
// stream ends cleanly (Done), fails (Err event or unexpected close),
// or ctx ends.
func (e *Engine) forwardStream(ctx context.Context, out chan<- StreamEvent, ch <-chan StreamEvent, first *StreamEvent, partial *strings.Builder) (streamOutcome, error) {
	if first != nil {
		if first.Err != nil {
			return streamFailed, first.Err
		}
		if first.Delta != "" {
			partial.WriteString(first.Delta)
		}
		select {
		case out <- *first:
		case <-ctx.Done():
			return streamCtxDone, ctx.Err()
		}
		if first.Done {
			return streamDone, nil
		}
	}
	for {
		select {
		case <-ctx.Done():
			return streamCtxDone, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return streamFailed, errors.New("router: upstream closed stream without Done")
			}
			if ev.Err != nil {
				return streamFailed, ev.Err
			}
			if ev.Delta != "" {
				partial.WriteString(ev.Delta)
			}
			select {
			case out <- ev:
			case <-ctx.Done():
				return streamCtxDone, ctx.Err()
			}
			if ev.Done {
				return streamDone, nil
			}
		}
	}
}

// noteStreamStartFailure records an attempt whose ChatStream call
// itself failed. It returns the 429 wait (0 when not rate-limited) so
// the caller can honour it before retrying the same provider.
func (e *Engine) noteStreamStartFailure(trace *routetrace.RouteTrace, res attemptResult, start time.Time) time.Duration {
	lat := time.Since(start).Milliseconds()
	if !e.hasClient(res.att.ProviderID) {
		addTraceAttempt(trace, res.att, lat, "error", res.err)
		return 0
	}
	if e.ledger != nil {
		// Stream start failure: no usage was reported, so record
		// Usage{} (unknown), never a guessed split.
		e.ledger.RecordAttempt(res.att.ProviderID, res.att.Model, false, lat, provider.Usage{})
	}
	if errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded) {
		addTraceAttempt(trace, res.att, lat, "error", res.err)
		return 0
	}
	if ra, ok := asRateLimit(res.err); ok {
		wait := e.rateLimitWait(ra, 1)
		e.setCooldown(res.att.ProviderID, wait)
		e.breakerNeutral(res.att.ProviderID)
		addTraceAttempt(trace, res.att, lat, "rate_limited", res.err)
		return wait
	}
	e.breakerFailure(res.att.ProviderID)
	addTraceAttempt(trace, res.att, lat, "error", res.err)
	return 0
}

// runStream is the ChatStreamWithChain worker goroutine.
func (e *Engine) runStream(ctx context.Context, chain []Attempt, req provider.ChatRequest, out chan<- StreamEvent) {
	defer close(out)
	trace := &routetrace.RouteTrace{}
	defer e.setLastTrace(trace)

	ordered, zeroSkipped := e.orderAttempts(chain)
	for _, att := range zeroSkipped {
		addTraceAttempt(trace, att, 0, "skipped_zero_quota", nil)
	}
	if len(ordered) == 0 {
		e.sendTerminal(out, ErrNoAvailableAttempt)
		return
	}

	maxAttempts := e.cfg.MaxAttempts
	attemptsUsed := 0
	var lastErr error
	var partial strings.Builder

attemptLoop:
	for idx := 0; idx < len(ordered) && attemptsUsed < maxAttempts; {
		if err := ctx.Err(); err != nil {
			lastErr = err
			break attemptLoop
		}
		att := ordered[idx]
		if e.onCooldown(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_cooldown", nil)
			idx++
			continue attemptLoop
		}
		if !e.breakerAllow(att.ProviderID) {
			addTraceAttempt(trace, att, 0, "skipped_breaker", nil)
			idx++
			continue attemptLoop
		}

		// Resume: if a previous attempt already streamed a prefix,
		// hand it to the next provider as an assistant message.
		r := req
		r.Model = att.Model
		if partial.Len() > 0 {
			msgs := make([]provider.Message, 0, len(req.Messages)+1)
			msgs = append(msgs, req.Messages...)
			msgs = append(msgs, provider.Message{Role: RoleAssistant, Content: partial.String()})
			r.Messages = msgs
		}
		start := time.Now()

		var ch <-chan StreamEvent
		var first *StreamEvent
		streamAtt := att
		consumed := 1
		var cancelWinner context.CancelFunc

		if idx == 0 && attemptsUsed == 0 && e.cfg.HedgeDelay > 0 && len(ordered) > 1 && e.hedgeSecondAvailable(ordered[1]) {
			hs := e.hedgedStream(ctx, att, ordered[1], r)
			consumed = len(hs.started)
			attemptsUsed += consumed
			if hs.err != nil {
				for _, s := range hs.started {
					e.noteStreamStartFailure(trace, attemptResult{att: s, err: hs.err}, start)
				}
				lastErr = hs.err
				idx += consumed
				continue attemptLoop
			}
			if hs.loser != nil {
				lat := time.Since(start).Milliseconds()
				addTraceAttempt(trace, *hs.loser, lat, "hedged_cancelled", context.Canceled)
				if e.ledger != nil {
					// Cancelled hedge loser: usage unknown.
					e.ledger.RecordAttempt(hs.loser.ProviderID, hs.loser.Model, false, lat, provider.Usage{})
				}
				e.breakerNeutral(hs.loser.ProviderID)
			}
			streamAtt = hs.winner
			ch = hs.ch
			first = hs.first
			cancelWinner = hs.cancel
		} else {
			client, ok := e.clients[att.ProviderID]
			if !ok {
				e.noteStreamStartFailure(trace, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}, start)
				attemptsUsed++
				lastErr = fmt.Errorf("router: unknown provider %q", att.ProviderID)
				idx++
				continue attemptLoop
			}
			// bridgeStream never fails synchronously: a provider
			// failure (including a start failure such as a refused
			// connection) arrives as an Err event on the channel,
			// and the streamFailed branch below handles it — with
			// the same 429-retry-same-provider rule when nothing
			// was streamed yet.
			ch = bridgeStream(ctx, client, r)
			attemptsUsed++
		}

		outcome, serr := e.forwardStream(ctx, out, ch, first, &partial)
		if cancelWinner != nil {
			cancelWinner()
		}
		lat := time.Since(start).Milliseconds()
		switch outcome {
		case streamDone:
			addTraceAttempt(trace, streamAtt, lat, "success", nil)
			if e.ledger != nil {
				// Streaming usage is unknown to the engine:
				// bridgeStream discards the upstream ChatResponse
				// (which may carry usage), so record Usage{}
				// (zeros) and never invent a split or a cost.
				e.ledger.RecordAttempt(streamAtt.ProviderID, streamAtt.Model, true, lat, provider.Usage{})
			}
			e.breakerSuccess(streamAtt.ProviderID)
			return
		case streamCtxDone:
			addTraceAttempt(trace, streamAtt, lat, "error", serr)
			if e.ledger != nil {
				// Cancelled stream: usage unknown.
				e.ledger.RecordAttempt(streamAtt.ProviderID, streamAtt.Model, false, lat, provider.Usage{})
			}
			lastErr = serr
			break attemptLoop
		case streamFailed:
			var retryWait time.Duration
			if ra, ok := asRateLimit(serr); ok {
				retryWait = e.rateLimitWait(ra, 1)
				e.setCooldown(streamAtt.ProviderID, retryWait)
				e.breakerNeutral(streamAtt.ProviderID)
				oc := "rate_limited"
				if partial.Len() > 0 {
					oc = "stream_error"
				}
				addTraceAttempt(trace, streamAtt, lat, oc, serr)
			} else {
				if !errors.Is(serr, context.Canceled) && !errors.Is(serr, context.DeadlineExceeded) {
					e.breakerFailure(streamAtt.ProviderID)
				}
				oc := "error"
				if partial.Len() > 0 {
					oc = "stream_error"
				}
				addTraceAttempt(trace, streamAtt, lat, oc, serr)
			}
			if e.ledger != nil {
				// Failed stream: usage unknown to the engine.
				e.ledger.RecordAttempt(streamAtt.ProviderID, streamAtt.Model, false, lat, provider.Usage{})
			}
			lastErr = serr
			// Retry the same provider after a 429 only when
			// nothing was streamed yet (no resume needed), this
			// was a single (non-hedged) attempt, it was the last
			// attempt, and budget remains.
			if partial.Len() == 0 && retryWait > 0 && consumed == 1 && idx+consumed >= len(ordered) && attemptsUsed < maxAttempts {
				if !e.sleep(ctx, retryWait) {
					lastErr = ctx.Err()
					break attemptLoop
				}
				e.clearCooldown(streamAtt.ProviderID)
				continue attemptLoop
			}
			idx += consumed
			continue attemptLoop
		}
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAttempt
	}
	e.sendTerminal(out, lastErr)
}

// hedgedStream is the streaming analogue of hedgedChat: start the
// first stream, and if no first event arrives within HedgeDelay, start
// the second stream in parallel and take whichever yields its first
// event first; the loser is cancelled. The winner's first event is
// already consumed and returned in first so the caller can forward it
// before reading the rest of the winner's channel.
type hedgedStreamResult struct {
	winner  Attempt
	ch      <-chan StreamEvent
	first   *StreamEvent
	started []Attempt
	loser   *Attempt
	err     error // non-nil when no stream could be started
	cancel  context.CancelFunc
}

func (e *Engine) hedgedStream(ctx context.Context, first, second Attempt, req provider.ChatRequest) hedgedStreamResult {
	c1, ok := e.clients[first.ProviderID]
	if !ok {
		return hedgedStreamResult{started: []Attempt{first}, err: fmt.Errorf("router: unknown provider %q", first.ProviderID)}
	}
	ctxA, cancelA := context.WithCancel(ctx)
	// bridgeStream never fails synchronously: a provider failure
	// arrives as an Err event on the channel, so there is no start-fail
	// path here — the winner's first event may itself be an error, and
	// forwardStream turns it into a failover like any other failure.
	chA := bridgeStream(ctxA, c1, reqWithModel(req, first))
	firstA := firstEventChan(chA)
	timer := time.NewTimer(e.cfg.HedgeDelay)
	defer timer.Stop()
	select {
	case ev := <-firstA:
		return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first}, cancel: cancelA}
	case <-timer.C:
	case <-ctx.Done():
		cancelA()
		return hedgedStreamResult{started: []Attempt{first}, err: ctx.Err()}
	}

	c2, ok := e.clients[second.ProviderID]
	if !ok {
		// Second provider is not registered; keep waiting on the first.
		select {
		case ev := <-firstA:
			return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first}, cancel: cancelA}
		case <-ctx.Done():
			cancelA()
			return hedgedStreamResult{started: []Attempt{first}, err: ctx.Err()}
		}
	}
	ctxB, cancelB := context.WithCancel(ctx)
	chB := bridgeStream(ctxB, c2, reqWithModel(req, second))
	firstB := firstEventChan(chB)
	select {
	case ev := <-firstA:
		cancelB()
		return hedgedStreamResult{winner: first, ch: chA, first: &ev, started: []Attempt{first, second}, loser: &second, cancel: cancelA}
	case ev := <-firstB:
		cancelA()
		return hedgedStreamResult{winner: second, ch: chB, first: &ev, started: []Attempt{first, second}, loser: &first, cancel: cancelB}
	case <-ctx.Done():
		cancelA()
		cancelB()
		return hedgedStreamResult{started: []Attempt{first, second}, err: ctx.Err()}
	}
}

// firstEventChan returns a channel that yields the first event from ch
// (or a synthetic error if ch closes first). The helper goroutine ends
// when ch yields, closes, or its ctx is cancelled.
func firstEventChan(ch <-chan StreamEvent) <-chan StreamEvent {
	out := make(chan StreamEvent, 1)
	go func() {
		ev, ok := <-ch
		if !ok {
			out <- StreamEvent{Err: errors.New("router: upstream closed stream without first event")}
			return
		}
		out <- ev
	}()
	return out
}

// ---------------------------------------------------------------------------
// Server-facing API (internal/server Engine + StreamResumer contracts).
//
// These methods take no explicit chain: the chain is resolved per
// request model via chainFor. The chain-based ChatWithChain /
// ChatStreamWithChain above remain the core; these are thin adapters
// over them, so all routing behaviour (fallback, 429 handling,
// breakers, hedging, mid-stream resume) is shared, not duplicated.
// ---------------------------------------------------------------------------

// chainFor resolves the fallback chain for a request model: Config.ChainFor
// when set, otherwise every registered client for the model, in sorted
// provider-ID order (deterministic, if arbitrary).
func (e *Engine) chainFor(model string) []Attempt {
	if e.cfg.ChainFor != nil {
		return e.cfg.ChainFor(model)
	}
	ids := make([]string, 0, len(e.clients))
	for id := range e.clients {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	chain := make([]Attempt, 0, len(ids))
	for _, id := range ids {
		chain = append(chain, Attempt{ProviderID: id, Model: model})
	}
	return chain
}

// Chat implements the server.Engine contract: it routes req through the
// fallback chain resolved for req.Model and returns the first successful
// response. Use LastTrace to see every attempt.
func (e *Engine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	resp, _, err := e.ChatWithChain(ctx, e.chainFor(req.Model), req)
	return resp, err
}

// ChatStream implements the server.Engine contract: it routes a streaming
// request through the fallback chain resolved for req.Model, calls
// onDelta once per content fragment (in order), and returns the
// reassembled response. Failover and mid-stream resume happen inside
// the engine, so fragments already delivered are never re-emitted.
func (e *Engine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.chatStreamDrain(ctx, e.chainFor(req.Model), req, 0, onDelta)
}

// ChatStreamFrom implements the server.StreamResumer contract: like
// ChatStream, but the first skipChars runes of the reassembled content
// are not delivered to onDelta (the client already has them). It runs a
// fresh internal stream — the engine's own failover/resume still
// applies inside it — and trims the already-sent prefix rune-wise, so
// a resume never duplicates content on the wire even if the new stream
// replays from the start. skipChars <= 0 behaves like ChatStream. The
// returned response always holds the full reassembled content.
func (e *Engine) ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.chatStreamDrain(ctx, e.chainFor(req.Model), req, skipChars, onDelta)
}

// chatStreamDrain drives a chain-based stream and forwards its deltas to
// onDelta, skipping the first skipChars runes when resuming. It returns
// the reassembled response (full content, even when resuming).
func (e *Engine) chatStreamDrain(ctx context.Context, chain []Attempt, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	if skipChars < 0 {
		skipChars = 0
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.ChatStreamWithChain(ctx, chain, req)
	if err != nil {
		return nil, err
	}
	var full strings.Builder
	skipped := 0
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil, errors.New("router: stream closed without Done")
			}
			if ev.Err != nil {
				return nil, ev.Err
			}
			if ev.Delta != "" {
				full.WriteString(ev.Delta)
				delta := ev.Delta
				if skipped < skipChars {
					runes := []rune(delta)
					drop := skipChars - skipped
					if drop > len(runes) {
						drop = len(runes)
					}
					skipped += drop
					delta = string(runes[drop:])
				}
				if delta != "" {
					if err := onDelta(delta); err != nil {
						return nil, err
					}
				}
			}
			if ev.Done {
				return &provider.ChatResponse{Content: full.String(), Model: req.Model}, nil
			}
		}
	}
}
