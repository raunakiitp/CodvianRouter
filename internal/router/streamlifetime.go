// Stream-lifetime cap (CHECKLIST 255, research-log entries
// 230–231, Magpie v0.1.4406): a hard lifetime bound on held and
// relayed streams — "Held or relayed streams now automatically end
// after 30 minutes to avoid lingering connections."
//
// The rule, in one sentence: no stream lives forever. Every stream
// the engine runs — a live relay to the client, a stream held in
// the preread buffer waiting for first content (items 133–134),
// a Bedrock incremental event stream, a drained stream behind
// ChatStreamChunks — gets the same max lifetime (default 30
// minutes, operator-configurable via Config.StreamMaxLifetime).
// On expiry the upstream call is terminated (the lifetime context
// cancels it, so the provider client aborts its HTTP read and
// stops billed generation) and the client gets a clean terminal
// event carrying the typed ErrStreamLifetimeExceeded — never a
// silent close, never a generic cancel.
//
// The cap bounds one REQUEST's stream, not one attempt: it starts
// when runStream starts and it is not reset by failover. A stream
// killed at the cap never fails over to a fresh target — a fresh
// attempt would hand the client a fresh lifetime, and the bound
// would be meaningless. A stream that already completed (Done)
// is unaffected: the cap cannot un-complete it.
//
// Interaction with the other bounds, honest and explicit:
//   - The per-combo deadline budget (Config.DeadlineBudget) still
//     applies first when it is shorter — whichever fires first
//     ends the stream, with its own typed error.
//   - The server's per-request timeout (default 120 s) still
//     applies to HTTP streams first when it is shorter. The cap
//     is the engine-level bound: it is the binding one for streams
//     outside HTTP (headless/CLI, MCP, drain paths with a
//     background context) and whenever the operator configures a
//     shorter cap or raises the server timeout.
//   - A client that goes away cancels the request context, which
//     ends the stream immediately — the cap is the backstop for a
//     stream whose client never goes away and whose upstream never
//     finishes, not the mechanism for client disconnects.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultStreamMaxLifetime is the stream-lifetime cap when the
// operator configures none (Config.StreamMaxLifetime == 0):
// Magpie's 30 minutes.
const DefaultStreamMaxLifetime = 30 * time.Minute

// ErrStreamLifetimeExceeded is the sentinel for a stream killed
// at the lifetime cap. The terminal event carries the wrapped
// form (see newStreamLifetimeError), so errors.Is works and the
// message names the cap that fired.
var ErrStreamLifetimeExceeded = errors.New("router: stream lifetime exceeded")

// NewStreamLifetimeError wraps the sentinel with the cap that
// fired, so the terminal event and the trace say how long the
// stream was allowed to live.
func NewStreamLifetimeError(cap time.Duration) error {
	return fmt.Errorf("%w (cap %s)", ErrStreamLifetimeExceeded, cap)
}

// streamLifetimeKilled counts streams the cap terminated, on the
// engine (see noteStreamLifetimeExceeded). It is the counter the
// dashboard's stream-lifetime feed reports.
func (e *Engine) noteStreamLifetimeExceeded() {
	if e == nil {
		return
	}
	e.streamLifetimeKilled.Add(1)
}

// StreamLifetimeExceededCount returns how many streams the cap
// has terminated since this engine started. Safe on a nil engine
// (0).
func (e *Engine) StreamLifetimeExceededCount() uint64 {
	if e == nil {
		return 0
	}
	return e.streamLifetimeKilled.Load()
}

// streamMaxLifetime resolves the effective cap: a configured
// positive value wins; 0 means the 30-minute default; a negative
// value disables the cap entirely (no bound — the operator asked
// for it, and the server request timeout / caller context is then
// the only bound; the dashboard feed reports the cap disabled).
func (e *Engine) streamMaxLifetime() time.Duration {
	if e == nil {
		return DefaultStreamMaxLifetime
	}
	switch {
	case e.cfg.StreamMaxLifetime < 0:
		return 0
	case e.cfg.StreamMaxLifetime == 0:
		return DefaultStreamMaxLifetime
	default:
		return e.cfg.StreamMaxLifetime
	}
}

// StreamMaxLifetime returns the effective stream-lifetime cap (0
// when disabled). The server's stream-lifetime feed and the
// verbatim-relay bound read it from here, so the dashboard and
// the enforcement can never disagree about the cap.
func (e *Engine) StreamMaxLifetime() time.Duration {
	return e.streamMaxLifetime()
}

// withStreamLifetime derives the context a stream runs under: the
// parent context plus a timer that cancels it with the typed
// lifetime error when the cap fires. The returned cancel stops
// the timer (defer it); calling cancel with nil after the stream
// ends is a no-op. When the cap is disabled the parent context
// is returned unchanged with a no-op cancel, so the stream runs
// unbounded by the cap — the caller's context is still the bound.
func (e *Engine) withStreamLifetime(ctx context.Context) (context.Context, context.CancelFunc) {
	cap := e.streamMaxLifetime()
	if cap <= 0 {
		return ctx, func() {}
	}
	lifeCtx, cancel := context.WithCancelCause(ctx)
	err := NewStreamLifetimeError(cap)
	// The timer owns exactly one cancel: the first expiry wins,
	// and a later client disconnect cannot overwrite the cause
	// (WithCancelCause keeps the first cause).
	timer := time.AfterFunc(cap, func() { cancel(err) })
	return lifeCtx, func() {
		timer.Stop()
		cancel(nil)
	}
}

// streamLifetimeCause reports the lifetime error when ctx was
// cancelled by the cap firing, and nil otherwise — a client
// disconnect, a guardrail cut, or the deadline budget cancelling
// the same context is not a lifetime expiry, and must never be
// reported as one.
func streamLifetimeCause(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil && errors.Is(cause, ErrStreamLifetimeExceeded) {
		return cause
	}
	return nil
}

// lifetimeErr resolves a stream context's error to the error the
// terminal event should carry: the typed lifetime error when the
// cap fired, otherwise the context's own error. It is what the
// loop-top check and the 429-retry sleep use, so a cap that fires
// between attempts still ends the request with the lifetime
// error, never a bare context.Canceled.
func lifetimeErr(ctx context.Context) error {
	if lt := streamLifetimeCause(ctx); lt != nil {
		return lt
	}
	return ctx.Err()
}
