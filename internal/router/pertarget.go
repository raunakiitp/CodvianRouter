// Per-target timeout (M4.7c).
//
// One hung provider must not hold a request hostage: every
// non-streaming attempt gets its own deadline — Config.
// PerTargetTimeout, default 120 s — bounding time-to-first-response
// for that target. When it fires, the attempt fails with a
// perTargetTimeoutError (a hung provider, counted by the breaker
// like any other 5xx/transport failure) and the chain fails over.
//
// The timeout is a child of the request ctx, so the request's own
// deadline still wins when it is earlier. The error deliberately
// does NOT wrap context.DeadlineExceeded: a per-target timeout is
// a provider-health signal, not a caller cancellation, and
// noteFailure's cancel guard must not swallow it.
//
// Streaming attempts need no separate timeout: time-to-first-event
// is already bounded more tightly — preread waits at most 5 s
// (prereadMaxWait) for first content, and the throughput watchdog
// (M4.6) aborts a stream that delivers nothing within warmup +
// window (30 s + 30 s). A stream that never produces a first event
// is therefore cut within ~65 s, inside the 120 s bound.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultPerTargetTimeout is the per-target timeout when
// Config.PerTargetTimeout is 0 (120 s).
const DefaultPerTargetTimeout = 120 * time.Second

// perTargetTimeoutError is the failure a per-target timeout
// produces. It is a plain error (not a context error) so the
// breaker counts it.
type perTargetTimeoutError struct {
	Timeout time.Duration
}

func (e *perTargetTimeoutError) Error() string {
	return fmt.Sprintf("router: provider timed out after %v (per-target timeout)", e.Timeout)
}

// IsPerTargetTimeout reports whether err is a per-target timeout.
func IsPerTargetTimeout(err error) bool {
	var e *perTargetTimeoutError
	return errors.As(err, &e)
}

// perTargetTimeout resolves the configured per-target timeout: an
// explicit value, the 120 s default for 0, and 0 (disabled) for a
// negative value.
func (e *Engine) perTargetTimeout() time.Duration {
	if e.cfg.PerTargetTimeout < 0 {
		return 0
	}
	if e.cfg.PerTargetTimeout == 0 {
		return DefaultPerTargetTimeout
	}
	return e.cfg.PerTargetTimeout
}

// perTargetCtx derives the attempt ctx: the request ctx with the
// per-target timeout applied. The caller must call cancel.
func (e *Engine) perTargetCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if timeout := e.perTargetTimeout(); timeout > 0 {
		return context.WithTimeout(ctx, timeout)
	}
	return ctx, func() {}
}
