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
	"sync"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

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
// a time. It is safe for concurrent use.
type Group struct {
	mu       sync.Mutex
	calls    map[string]*flightCall
	requests uint64
	flights  uint64
	shared   uint64
}

// New returns an empty Group.
func New() *Group {
	return &Group{calls: make(map[string]*flightCall)}
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
func (g *Group) Do(ctx context.Context, key string, fn func() (*provider.ChatResponse, *routetrace.RouteTrace, error)) (Result, bool) {
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
	c := &flightCall{done: make(chan struct{})}
	g.calls[key] = c
	g.flights++
	g.mu.Unlock()

	resp, trace, err := fn()
	c.result = Result{Resp: resp, Trace: trace, Err: err}
	// Close first, then drop the entry: a caller arriving between
	// the close and the delete still finds the call, sees the
	// closed channel, and shares the finished result instead of
	// starting a duplicate upstream call.
	close(c.done)
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return c.result, false
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
	}
}

// requestKeyPayload is the canonical form of a request for the
// flight key: model + messages + the sampling params that change the
// answer. Tools and tool_choice are deliberately absent —
// Coalescable excludes tool-using requests before a key is ever
// derived, so they are always empty here. Stream is absent too:
// only non-streaming requests reach the group. JSON encoding of a
// struct is deterministic (fixed field order, no maps), and
// provider.Message's MarshalJSON encodes content parts, so two
// requests that differ in any of these fields hash differently,
// including message order, whitespace, and part structure.
type requestKeyPayload struct {
	Model       string             `json:"model"`
	Messages    []provider.Message `json:"messages"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	TopP        float64            `json:"top_p,omitempty"`
	Stop        []string           `json:"stop,omitempty"`
}

// RequestKey derives the canonical flight key for req: SHA-256 of
// the JSON encoding of requestKeyPayload. It only fails when a float
// param cannot be encoded (NaN/Inf) — callers must run such a
// request uncoalesced rather than failing it.
func RequestKey(req provider.ChatRequest) (string, error) {
	b, err := json.Marshal(requestKeyPayload{
		Model:       req.Model,
		Messages:    req.Messages,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Coalescable reports whether req may share a flight. Tool-using
// requests are excluded: a shared answer holds content only, so
// coalescing one would silently drop the tool calls the caller asked
// for, and a tool result in the history makes the answer depend on
// the tool output, not just the prompt. This is the same rule the
// degrade wrapper uses for cacheable requests.
func Coalescable(req provider.ChatRequest) bool {
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
