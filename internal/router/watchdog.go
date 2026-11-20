// Throughput watchdog on stalled streams (M4.6, the reference router
// streamHealth.ts).
//
// A stream that stops delivering bytes is worse than a stream that
// fails: the client hangs with no error and no failover. The
// watchdog watches the live forwarding in forwardStream — the single
// funnel every streamed byte passes through after commit (and for
// exempt requests, directly) — and aborts the attempt when the
// throughput falls below the floor after a grace period:
//
//   - warmup 30 s: no enforcement (a slow first token is not a
//     stall; the preread phase before commit is separately bounded
//     by prereadMaxWait, 5 s).
//   - window 30 s: after warmup, every 30 s window must deliver at
//     least 4 B/s on average.
//   - a window below the floor aborts the stream with a
//     throughputStalledError. Before commit that is a failover
//     (the preread buffer held everything, so the client saw
//     nothing); after commit the stream ends with the error — the
//     client owns the partial body, so no switch.
//
// Bytes are text-delta bytes plus tool-call bytes (id, name,
// arguments): a stream delivering only tool calls is alive, and a
// stream delivering only empty heartbeats is not — empty events
// carry no bytes and never reset the window.
//
// The durations are package vars, not consts, so tests can shrink
// them without waiting 60 s; nothing else in the codebase changes
// them (same pattern as prereadMaxWait).
package router

import (
	"fmt"
	"time"
)

// watchdogWarmup is the grace period before enforcement starts.
var watchdogWarmup = 30 * time.Second

// watchdogWindow is the throughput measurement window after warmup.
var watchdogWindow = 30 * time.Second

// watchdogMinBytesPerSec is the floor: a window averaging below
// this is a stalled stream (4 B/s).
const watchdogMinBytesPerSec = 4

// throughputStalledError is the abort the watchdog raises when a
// stream's window throughput falls below the floor.
type throughputStalledError struct {
	rate   float64       // measured bytes/second over the window
	window time.Duration // the window that was measured
}

func (e *throughputStalledError) Error() string {
	return fmt.Sprintf("router: stream stalled (throughput %.2f B/s over %v, floor %d B/s)", e.rate, e.window, watchdogMinBytesPerSec)
}

// IsThroughputStalled reports whether err is a watchdog abort.
func IsThroughputStalled(err error) bool {
	_, ok := err.(*throughputStalledError)
	return ok
}

// watchdogBytes is the live byte count of one stream event: text
// deltas plus tool-call payload. Empty events count 0.
func watchdogBytes(ev StreamEvent) int {
	n := len(ev.Delta)
	for _, tc := range ev.ToolCalls {
		n += len(tc.ID) + len(tc.Name) + len(tc.Arguments)
	}
	return n
}

// streamWatchdog is the per-attempt watchdog state. It is driven by
// forwardStream's event loop: noteBytes on every forwarded event,
// and check on a timer (check returns the abort error, or nil).
type streamWatchdog struct {
	start       time.Time
	windowStart time.Time
	bytes       int
	warmed      bool
}

// newStreamWatchdog starts the watchdog at now.
func newStreamWatchdog(now time.Time) *streamWatchdog {
	return &streamWatchdog{start: now, windowStart: now}
}

// nextCheck is when the watchdog next wants to be evaluated: the
// end of warmup, then the end of each window.
func (w *streamWatchdog) nextCheck() time.Duration {
	if !w.warmed {
		return time.Until(w.start.Add(watchdogWarmup))
	}
	return time.Until(w.windowStart.Add(watchdogWindow))
}

// noteBytes adds the event's live bytes to the current window.
func (w *streamWatchdog) noteBytes(n int) {
	w.bytes += n
}

// check evaluates the current window at now. Before warmup ends it
// only flips the warmed flag and restarts the window (warmup bytes
// do not count toward the first measured window — warmup is grace,
// not credit). After warmup, a window averaging below the floor
// returns the abort error; a healthy window restarts the window.
func (w *streamWatchdog) check(now time.Time) error {
	if !w.warmed {
		if now.Before(w.start.Add(watchdogWarmup)) {
			return nil
		}
		w.warmed = true
		w.windowStart = now
		w.bytes = 0
		return nil
	}
	elapsed := now.Sub(w.windowStart)
	if elapsed <= 0 {
		return nil
	}
	rate := float64(w.bytes) / elapsed.Seconds()
	if rate < watchdogMinBytesPerSec {
		return &throughputStalledError{rate: rate, window: elapsed}
	}
	w.windowStart = now
	w.bytes = 0
	return nil
}
