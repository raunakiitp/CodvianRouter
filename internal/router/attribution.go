// Request-scoped outcome attribution (CHECKLIST 317-318).
//
// The engine used to publish each request's routing decision and
// route trace only into engine-global "last" slots
// (Engine.LastTrace, Engine.LastAutoDecision), and the server read
// those slots after the engine call returned to attribute the
// outcome (feedback decision registration, the metadata-only
// learning loop, metrics, budget overage). Under concurrency that
// read is a race: a neighbour request finishing between this
// request's engine return and the server's read leaves the
// neighbour's decision/trace in the slots, so the outcome is
// attributed to the wrong decision — a wrong feedback decision id
// on the response header, a wrong (task, model) cell in the
// learning loop, a wrong provider in metrics.
//
// An Attribution is one request's own routing record, carried on
// the request context (WithAttribution) from the handler to the
// engine call. The engine publishes into it at the same points it
// publishes the globals — when resolveAuto records the decision,
// when a chat/stream call finishes its trace — so the sink always
// holds exactly this request's decision and trace, whatever
// neighbours do. The server reads the sink first and falls back
// to the globals only when no sink was attached (older engines,
// test doubles, cache replays that never called the engine).
//
// The globals stay: GET /dashboard/trace and
// GET /dashboard/auto-route/last are "most recent" dashboard
// feeds by design, and they keep reading them.
package router

import (
	"context"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// Attribution is one request's routing record: the auto-routing
// decision that resolved its model (when the request named an
// auto/* or intent/* virtual model) and the route trace of the
// attempts that served it. Safe for concurrent use: the engine
// publishes into it from the request's own goroutine (and, for a
// coalesced flight, from the shared flight), while the server
// reads it after the engine call returns.
//
// A nil decision means the request named no virtual model (or the
// decision was cleared by a later non-auto resolution); a nil
// trace means the engine published none (e.g. the request was
// refused before any attempt ran — the early trace publishes
// still run, so this is rare). Callers must treat a nil trace
// as "no trace for this request", never as the previous
// request's trace.
type Attribution struct {
	mu       sync.Mutex
	decision *autorouter.Decision
	trace    *routetrace.RouteTrace
}

// NewAttribution returns an empty per-request attribution sink.
func NewAttribution() *Attribution { return &Attribution{} }

// Decision returns a copy of the request's auto-routing decision
// and whether one was published. The copy is safe to mutate;
// mutating it does not affect the sink.
func (a *Attribution) Decision() (autorouter.Decision, bool) {
	if a == nil {
		return autorouter.Decision{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.decision == nil {
		return autorouter.Decision{}, false
	}
	return *a.decision, true
}

// Trace returns the request's route trace, or nil when the engine
// published none for this request.
func (a *Attribution) Trace() *routetrace.RouteTrace {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.trace
}

// PublishDecision records dec as this request's auto-routing
// decision. A nil dec clears a previously published decision
// (a non-auto request after an auto one). The decision is
// copied, so the caller keeps owning its value. Called by the
// engine (resolveAuto) and by test doubles that stand in for it.
func (a *Attribution) PublishDecision(dec *autorouter.Decision) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if dec == nil {
		a.decision = nil
		return
	}
	cp := *dec
	a.decision = &cp
}

// PublishTrace records t as this request's route trace. A nil t
// clears a previously published trace. Called by the engine when
// a chat/stream call finishes (and, for a coalesced waiter, with
// the shared flight's trace — the waiter was served by that
// flight, so the shared trace IS its trace).
func (a *Attribution) PublishTrace(t *routetrace.RouteTrace) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.trace = t
}

type attributionKey struct{}

// WithAttribution returns ctx carrying a as this request's
// attribution sink. A nil a leaves ctx unchanged.
func WithAttribution(ctx context.Context, a *Attribution) context.Context {
	if a == nil {
		return ctx
	}
	return context.WithValue(ctx, attributionKey{}, a)
}

// AttributionFrom returns the attribution sink carried by ctx, or
// nil when the request attached none.
func AttributionFrom(ctx context.Context) *Attribution {
	if ctx == nil {
		return nil
	}
	if a, ok := ctx.Value(attributionKey{}).(*Attribution); ok {
		return a
	}
	return nil
}
