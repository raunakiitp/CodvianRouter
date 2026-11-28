// Package coalescing implements single-flight request coalescing for
// non-streaming requests.
//
// When N identical non-streaming requests arrive at the same time, only
// the first (the leader) calls the upstream: later callers (waiters)
// wait on the leader's flight and share its result — one upstream call,
// one charge, one quota hit. The whole retry loop (the router's
// ChatWithChain, including failover, 429 backoff and hedging) runs
// inside the one flight, so waiters share a single backoff sequence,
// not N parallel ones. A shared failure reaches every waiter (one
// error, told once).
//
// What is NEVER coalesced:
//   - streaming requests (the router's ChatStream* paths never touch
//     this package — a stream cannot be shared between callers
//     without duplicating or splitting its deltas);
//   - stateful / tool-call turns: any request carrying tools, a
//     tool_choice, a tool-role message, or tool calls in its history
//     (see Coalescable — the same rule the degrade wrapper uses for
//     cacheable requests, because a cached/shared answer holds content
//     only and must never silently drop tool calls).
//
// The flight is keyed on the canonical request hash (RequestKey):
// model + messages + sampling params, canonicalized by JSON encoding
// a fixed-shape struct and SHA-256 hashing it. Requests that differ
// in any of those fields get different keys and never share a flight.
// Headers and gateway keys are NOT part of the key — the key is about
// the request, and the upstream call is identical either way — but
// the leader's own context drives the flight: if it is cancelled the
// flight fails and every waiter gets that error. A waiter whose own
// context ends first leaves with its own context error and does not
// disturb the leader.
//
// Tenant isolation: RequestKeyForTenant prefixes the caller's tenant
// (the gateway key ID) into the key, so identical requests from
// different tenants never share a flight — one tenant must never be
// served another tenant's response, nor ride on another tenant's
// quota. The tenant is hashed, never stored in cleartext in the key.
// A zero tenant hashes exactly as RequestKey does, so existing
// callers that cannot supply a tenant yet keep today's behaviour.
//
// In-flight cap: at most MaxInFlight flights run at once (the sector-typical
// deduplicate() caps at 1,000 in-flight coalesced requests). Past the
// cap a new request does NOT block and does NOT queue: it runs
// uncoalesced, as its own flight, and is counted honestly as
// CapBypassed — never folded into Shared.
//
// Flight timeout: every flight runs under a FlightTimeout context
// (the sector-typical deduplicate() times out after 60 s). On timeout the
// flight fails with context.DeadlineExceeded, every waiter gets that
// error, the entry is cleaned up, and a later identical request
// starts a fresh flight. DoCtx hands the timeout context to fn, so
// a fn that respects its context actually aborts its upstream call;
// Do's legacy no-context fn is timed out at the flight level instead
// (its goroutine is abandoned, its late result discarded).
//
// This is the golang.org/x/sync/singleflight pattern implemented with
// stdlib sync only: x/sync is not a dependency of this module and the
// project does not add network dependencies for a pattern this small.
//
// This package is deliberately a leaf (it imports only provider and
// routetrace): the router owns the flight group, the server reports
// its counters, and the degrade wrapper forwards them — if the group
// lived in the router, the server could not import it without an
// import cycle (router tests import the server).
//
// Ledger honesty: the leader's upstream attempt is recorded by the
// router's normal path exactly once. Each waiter is recorded
// separately through the router's SharedLedger interface: a row
// marked shared, with zero tokens and zero cost — the waiter was
// served, but it was not billed, and the shared flag keeps it
// distinguishable from a real upstream attempt so usage totals never
// double-count.
package coalescing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// MaxInFlight is the cap on concurrent registered flights. Past it a
// new request runs uncoalesced (see Group.DoCtx) and is counted as
// CapBypassed. The number is the sector-typical: its deduplicate() caps at
// 1,000 in-flight coalesced requests (M6 parity, Area 6).
const MaxInFlight = 1000

// FlightTimeout is the per-flight timeout: the leader's fn runs under
// a context that expires this long after the flight starts, and on
// expiry the flight fails with context.DeadlineExceeded. The number
// is the sector-typical: its deduplicate() times out after 60 s (M6 parity,
// Area 6).
const FlightTimeout = 60 * time.Second

// Stats is a snapshot of the single-flight counters. All counters
// are real: they start at zero and only move when real requests flow
// through the group. UpstreamAvoided and QuotaSaved are both defined
// as the shared-call count — every waiter that shared a flight
// avoided exactly one upstream call and therefore exactly one quota
// hit — so they always equal Shared; they are separate fields only
// because the dashboard shows them as separate numbers.
type Stats struct {
	// Requests is the number of coalescable non-streaming requests
	// that entered the group (leaders + waiters).
	Requests uint64 `json:"requests"`
	// Flights is the number of leader executions — one upstream
	// attempt sequence per distinct in-flight key.
	Flights uint64 `json:"flights"`
	// Shared is the number of waiters served from a shared flight
	// (success or shared failure — either way the waiter made no
	// upstream call of its own).
	Shared uint64 `json:"shared_calls"`
	// UpstreamAvoided is the number of upstream calls avoided by
	// coalescing (== Shared).
	UpstreamAvoided uint64 `json:"upstream_avoided"`
	// QuotaSaved is the number of quota hits saved (== Shared: one
	// avoided upstream call is one quota hit not consumed). This
	// counts calls, not tokens — the tokens saved depend on the
	// response and are not guessed here.
	QuotaSaved uint64 `json:"quota_saved_calls"`
	// InFlight is the number of flights currently running.
	InFlight int `json:"in_flight"`
	// CapBypassed is the number of requests that arrived while the
	// in-flight cap was reached and therefore ran uncoalesced as
	// their own flight instead of joining or starting a registered
	// flight. It is counted separately from Shared on purpose: a
	// bypassed request made its own upstream call, so folding it
	// into Shared would overstate the calls coalescing avoided.
	// CapBypassed requests are included in Requests, but not in
	// Flights (they were never registered flights) and not in
	// InFlight while they run.
	CapBypassed uint64 `json:"cap_bypassed"`
}

// Result is the outcome of one flight: the response the leader got
// (nil on error), the route trace of the leader's attempt sequence
// (nil when no attempt was made), and the error (nil on success).
// Waiters receive the leader's result by value; the *ChatResponse
// itself is shared read-only and must not be mutated by callers.
type Result struct {
	Resp  *provider.ChatResponse
	Trace *routetrace.RouteTrace
	Err   error
}

// flightCall is one in-flight coalesced request.
type flightCall struct {
	done   chan struct{} // closed when result is set
	result Result
}

// Group is the single-flight registry: at most one flight per key at
// a time. It is safe for concurrent use. One mutex guards every
// field — the registration check, the cap check and the counter
// updates are all done under that single mu, so there is no lock
// ordering to get wrong on this hot path.
type Group struct {
	mu          sync.Mutex
	calls       map[string]*flightCall
	requests    uint64
	flights     uint64
	shared      uint64
	capBypassed uint64
	// timeout and maxInFlight are test overrides for the defaults
	// FlightTimeout and MaxInFlight. Zero means "use the default".
	// They are read under mu at flight registration, so they must
	// be set before the group is used concurrently, never while
	// flights are running.
	timeout     time.Duration
	maxInFlight int
}

// New returns an empty Group.
func New() *Group {
	return &Group{calls: make(map[string]*flightCall)}
}

// effectiveTimeout returns the per-flight timeout in force for
// this group: the test override when set, else FlightTimeout.
// Callers must hold g.mu.
func (g *Group) effectiveTimeout() time.Duration {
	if g.timeout > 0 {
		return g.timeout
	}
	return FlightTimeout
}

// effectiveMaxInFlight returns the in-flight cap in force for this
// group: the test override when set, else MaxInFlight. Callers must
// hold g.mu.
func (g *Group) effectiveMaxInFlight() int {
	if g.maxInFlight > 0 {
		return g.maxInFlight
	}
	return MaxInFlight
}

// Do runs fn for key, or waits for the flight already running for key
// and shares its result. It returns the result and whether this
// caller was a waiter that shared a flight (false for the leader, and
// false for a waiter whose own context ended before the flight did —
// that waiter gets its context error instead and is not counted as
// shared). The leader's fn runs with the leader's ctx; waiters never
// cancel the leader. The entry is registered BEFORE fn is called, so
// a duplicate arriving while the leader is still dialing joins the
// flight instead of starting a second upstream call.
//
// The flight is bounded by FlightTimeout (see DoCtx): on timeout the
// flight fails, waiters get context.DeadlineExceeded, the entry is
// cleaned up, and a later identical request starts a fresh flight.
// Do's fn takes no context, so a fn that ignores contexts cannot be
// cancelled mid-flight — the timeout still releases the leader and
// the waiters on schedule, and the fn's late result is discarded.
//
// When the in-flight cap (MaxInFlight) is already reached, a new key
// does not block and does not queue: fn runs immediately, uncoalesced,
// as the caller's own flight, and the call is counted as CapBypassed
// (see Stats), not as a flight and not as shared.
func (g *Group) Do(ctx context.Context, key string, fn func() (*provider.ChatResponse, *routetrace.RouteTrace, error)) (Result, bool) {
	return g.DoCtx(ctx, key, func(context.Context) (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		return fn()
	})
}

// DoCtx is Do, except fn receives the leader's flight context: the
// caller's ctx with the FlightTimeout deadline applied. A fn that
// respects its context aborts its upstream call when the flight
// times out (or when the leader's own ctx ends); a fn that ignores
// it is still timed out at the flight level, as in Do. New callers
// should use DoCtx so the timeout actually cancels the upstream
// call; Do exists for the existing callers whose fn closes over
// their own ctx. Waiters behave exactly as in Do.
func (g *Group) DoCtx(ctx context.Context, key string, fn func(context.Context) (*provider.ChatResponse, *routetrace.RouteTrace, error)) (Result, bool) {
	g.mu.Lock()
	g.requests++
	if c, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return Result{Err: ctx.Err()}, false
		case <-c.done:
			g.mu.Lock()
			g.shared++
			res := c.result
			g.mu.Unlock()
			return res, true
		}
	}
	timeout := g.effectiveTimeout()
	if len(g.calls) >= g.effectiveMaxInFlight() {
		// Cap reached: run uncoalesced, right now, without
		// registering a flight and without waiting. Counted
		// honestly as a bypass, not as a flight or a share.
		// The bypass is the caller's own call, like the
		// router's uncoalesced path, so it runs under the
		// caller's ctx as passed — no flight timeout applies.
		g.capBypassed++
		g.mu.Unlock()
		resp, trace, err := fn(ctx)
		return Result{Resp: resp, Trace: trace, Err: err}, false
	}
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.flights++
	g.mu.Unlock()

	// The leader's fn runs in its own goroutine under a timeout
	// context, so the flight can be failed on schedule even if fn
	// blocks on something that ignores contexts. The result
	// channel is buffered (capacity 1) so a fn that finishes after
	// the timeout never blocks sending its discarded result.
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type outcome struct {
		res      Result
		panicked any
		hasPanic bool
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				// A panicking fn used to propagate straight to
				// the leader's caller (fn ran synchronously);
				// keep that: report the panic as a flight
				// error to the waiters, then re-panic in the
				// leader below, after the flight is closed.
				done <- outcome{res: Result{Err: fmt.Errorf("coalescing flight panicked: %v", r)}, panicked: r, hasPanic: true}
			}
		}()
		resp, trace, err := fn(lctx)
		done <- outcome{res: Result{Resp: resp, Trace: trace, Err: err}}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var res Result
	var panicked any
	var hasPanic bool
	select {
	case o := <-done:
		res, panicked, hasPanic = o.res, o.panicked, o.hasPanic
	case <-timer.C:
		// Timeout: cancel the flight context (a fn that
		// respects it aborts its upstream call now) and fail
		// the flight. The entry is cleaned up below, so a
		// later identical request starts a fresh flight; the
		// abandoned fn's late result is discarded.
		cancel()
		res = Result{Err: fmt.Errorf("coalescing flight timed out after %s: %w", timeout, context.DeadlineExceeded)}
	}
	c.result = res
	// Close first, then drop the entry: a caller arriving between
	// the close and the delete still finds the call, sees the
	// closed channel, and shares the finished result instead of
	// starting a duplicate upstream call.
	close(c.done)
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	if hasPanic {
		panic(panicked)
	}
	return res, false
}

// Stats returns a snapshot of the group's counters.
func (g *Group) Stats() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{
		Requests:        g.requests,
		Flights:         g.flights,
		Shared:          g.shared,
		UpstreamAvoided: g.shared,
		QuotaSaved:      g.shared,
		InFlight:        len(g.calls),
		CapBypassed:     g.capBypassed,
	}
}

// requestKeyPayload is the canonical form of a request for the
// flight key: model + messages + the sampling params that change the
// answer + the per-request routing contract. Tools and tool_choice
// are deliberately absent — Coalescable excludes tool-using
// requests before a key is ever derived, so they are always empty
// here. Stream is absent too: only non-streaming requests reach
// the group. Routing is present because two requests that differ
// only in their contract (different only/exclude/order) must never
// share one flight: the waiter would otherwise be served by a
// provider its own contract forbade. A nil contract marshals as
// null, so uncontracted requests keep hashing exactly as before.
// JSON encoding of a struct is deterministic (fixed field order,
// no maps), and provider.Message's MarshalJSON encodes content
// parts, so two requests that differ in any of these fields hash
// differently, including message order, whitespace, and part
// structure.
type requestKeyPayload struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	TopP        float64            `json:"top_p,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
	// Strategy is the per-request "strategy" body field (A2-1).
	// It is part of the key for the same reason Routing is: two
	// requests that differ only in their strategy execute
	// differently (a different chain order, or a combo fan-out vs.
	// a sequential walk), so they must never share one flight.
	Strategy string                   `json:"strategy,omitempty"`
	Routing  *provider.RequestRouting `json:"routing,omitempty"`
}

// RequestKey derives the canonical flight key for req: SHA-256 of
// the JSON encoding of requestKeyPayload. It only fails when a float
// param cannot be encoded (NaN/Inf) — callers must run such a
// request uncoalesced rather than failing it. It carries no tenant:
// use RequestKeyForTenant when the caller's tenant is known, so
// different tenants never share a flight.
func RequestKey(req provider.ChatRequest) (string, error) {
	b, err := json.Marshal(requestKeyPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Routing:     req.Routing,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// RequestKeyForTenant derives the canonical flight key for req
// scoped to one tenant: the tenant is prefixed into the key (a
// 0x00-separated SHA-256 over tenant + base key), so identical
// requests from different tenants hash differently and never share
// a flight, while identical requests from the same tenant still
// coalesce. The tenant is the caller's identity at the coalescing
// call site — the gateway key ID. The router's Engine.Chat wires
// it (router.go: Engine.Chat calls RequestKeyForTenant(req,
// req.CallerID)); callers that cannot supply a tenant pass "" and
// keep RequestKey's behaviour bit-for-bit. The tenant is hashed,
func RequestKeyForTenant(req provider.ChatRequest, tenant string) (string, error) {
	base, err := RequestKey(req)
	if err != nil {
		return "", err
	}
	if tenant == "" {
		return base, nil
	}
	sum := sha256.Sum256([]byte(tenant + "\x00" + base))
	return hex.EncodeToString(sum[:]), nil
}

// Coalescable reports whether req may share a flight. Tool-using
// requests are excluded: a shared answer holds content only, so
// coalescing one would silently drop the tool calls the caller asked
// for, and a tool result in the history makes the answer depend on
// the tool output, not just the prompt. This is the same rule the
// degrade wrapper uses for cacheable requests.
//
// Responses requests are excluded too (CHECKLIST 169): a Responses
// create mints a distinct stateful object per call (its own
// response id, its own conversation record), so two identical
// concurrent creates must never share one flight and hand both
// callers the same id — and the flight key does not carry the
// endpoint or the continuation id, so a Responses create could
// otherwise collide with an identical chat completion. A
// continuation (PreviousResponseID set) is excluded for the same
// reason: it is replay-exempt, exactly one attempt, never shared.
func Coalescable(req provider.ChatRequest) bool {
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
