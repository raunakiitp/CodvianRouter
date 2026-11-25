// Panic isolation on the request/stream path (CHECKLIST 165-166,
// Upstream-gateway behaviour: a panic in a stream/chunk converter or a
// hook must never kill the gateway).
//
// The boundaries, innermost first:
//
//   - callChat (per non-streaming attempt): a panicking provider
//     client becomes an ordinary attempt error, so the chain fails
//     over and the attempt is recorded in the ledger by the normal
//     failure path.
//   - bridgeStream (per streaming attempt): a panicking provider
//     stream client — including a wire chunk converter turning a
//     malformed upstream event into a panic — becomes a terminal
//     Err event on the stream channel, so runStream fails over
//     before commit and ends the stream with an error after commit.
//   - ChatWithChainStrategy / runStream (per request): the backstop
//     for panics in the engine's own machinery (a scoring/order
//     hook, the preread buffer). The request gets an error / a
//     terminal event, never a dead process.
//   - The server's own handlers (internal/server/panic.go): the
//     backstop for panics in server-side code (the onChunk closure,
//     the MCP loop, response marshalling). The client gets an
//     honest 502 (non-stream) or a terminal SSE error frame
//     (stream), and a ledger row is recorded when the engine
//     recorded none.
//
// Every recovered panic is counted exactly once, at the boundary
// that caught it, in codvian_router_panics_total{location} (see
// PanicMetrics / metrics.Recorder.IncPanic). The recovered value is
// logged exactly once, by the server, redacted — never a request
// body or a key (see server.logPanicError / noteServerPanic). The
// engine has no logger; the value rides the PanicError to the
// server, which logs it.
//
// Locations (the label values — keep them stable; dashboards and
// alerts may key on them):
//   - provider_chat, provider_stream: a provider client panicked.
//   - engine_chat, engine_stream: the engine's own machinery
//     panicked (hook, preread, chain bookkeeping).
//   - server_chat, server_stream: server-side handler code
//     panicked on /v1/chat/completions.
//   - server_messages, server_messages_stream: the same, on
//     /v1/messages.
package router

// PanicMetrics is optionally implemented by a metrics sink that
// wants one increment per recovered panic (metrics.Recorder
// satisfies it). Wiring is best-effort: a nil sink disables
// reporting, never routing — a panic is still recovered and the
// request still answered, just not counted.
type PanicMetrics interface {
	IncPanic(location string)
}

// PanicError is the error a recovered panic becomes at a request or
// stream boundary.
//
// Error() carries only the fixed location string — never the
// recovered value — so route traces, metrics and client-facing
// error paths can print it safely: a panic value could contain
// anything (an upstream body fragment, a key-shaped string), and
// traces are rendered in the dashboard. The value itself is
// available via Value(), for the server's (redacted) log line —
// the one place it is ever rendered.
//
// Classification: provider.ClassifyError has no case for it, so it
// falls through to the default (DecisionFallback,
// ReasonTransportError) — a panicking provider is a retryable
// failure, and the chain moves on. The trace reason is overridden
// to ReasonPanicRecovered in addTraceAttemptPreread so the
// dashboard says what actually happened instead of
// "transport_error".
type PanicError struct {
	// Location is where the panic was caught (see the list at the
	// top of this file).
	Location string
	value    any
}

// NewPanicError wraps a recovered panic value at the named location.
func NewPanicError(location string, value any) *PanicError {
	return &PanicError{Location: location, value: value}
}

// Error returns the fixed location string. It never includes the
// recovered value.
func (e *PanicError) Error() string {
	if e == nil {
		return "router: recovered panic"
	}
	return "router: panic recovered in " + e.Location
}

// Value returns the recovered panic value, for logging only. Callers
// must redact it before it reaches any log line.
func (e *PanicError) Value() any {
	if e == nil {
		return nil
	}
	return e.value
}
