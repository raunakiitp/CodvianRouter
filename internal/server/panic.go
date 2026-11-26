// Panic isolation on the request/stream path, server side
// (CHECKLIST 165-166, upstream-gateway behaviour: a panic in a
// stream/chunk converter or a hook must never kill the gateway).
//
// The engine recovers provider/hook panics itself (see
// internal/router/panic.go) and returns them as *router.PanicError;
// this file is the server's half:
//
//   - noteServerPanic: a panic recovered in server-side handler
//     code (the onChunk closure, the MCP loop, response
//     marshalling). Counts it in codvian_router_panics_total,
//     logs the redacted value, and returns the PanicError the
//     handler answers with.
//   - logPanicError: a PanicError the engine returned (already
//     counted by the engine) — log the redacted value once, here,
//     so every recovered panic gets exactly one log line.
//   - writePanicFailure: the honest non-stream answer — HTTP 502,
//     "gateway internal error", JSON-RPC code -32603 (the same
//     internal-error code as any other exhausted-chain failure).
//     The stream answer is a terminal SSE error frame with the
//     same message, written by the handler that owns the stream
//     (handleChatStream / handleMessagesStream).
//   - recordPanicRow / recordPanicRowIfUnrecorded: the ledger
//     backstop. A panic that escaped the engine before any attempt
//     was recorded (a hook panic in orderAttempts, a panic in the
//     server's pre-engine checks) still leaves one failed row:
//     provider "unknown", the requested model, ok=false, zero
//     tokens, zero cost — an honest audit row, never a billed
//     attempt. When the engine already recorded attempts (its
//     trace has entries) there is nothing to add.
//
// Logging rule: the recovered value is formatted by
// panicValueString (a value whose String() itself panics becomes
// "unprintable panic value", never a second crash) and passed
// through guardrails.Redact — a panic value must never carry a
// request body or a key into the log. The value is never rendered
// anywhere else: PanicError.Error() is a fixed location string,
// so traces and client-facing errors stay clean.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// panicValueString formats a recovered panic value for the log. A
// value whose String() method panics is reported as unprintable
// rather than crashing the log line.
func panicValueString(v any) (out string) {
	defer func() {
		if recover() != nil {
			out = "unprintable panic value"
		}
	}()
	return fmt.Sprintf("%v", v)
}

// noteServerPanic records a panic recovered in server-side handler
// code: one codvian_router_panics_total{location} increment and one
// log line with the redacted value. It returns the PanicError the
// handler answers with (502, or a terminal SSE error frame). A nil
// recorder still logs; a nil logger would panic, so callers only
// invoke this on a constructed Server (New always sets a logger).
func (s *Server) noteServerPanic(location string, v any) *router.PanicError {
	if s.recorder != nil {
		s.recorder.IncPanic(location)
	}
	s.logger.Printf("panic recovered in %s: %s", location, guardrails.Redact(panicValueString(v)))
	return router.NewPanicError(location, v)
}

// noteBackgroundPanic records a panic recovered in a background
// goroutine (AUD-REL-006): the same codvian_router_panics_total
// increment and one redacted log line as noteServerPanic, but with
// no response to answer — the goroutine ends and the metric plus
// log line are the observable record. Background locations use the
// "bg_" prefix so logPanicError's server_* dedup rule is not
// triggered by anything that later surfaces the same value.
func (s *Server) noteBackgroundPanic(location string, v any) {
	if s.recorder != nil {
		s.recorder.IncPanic(location)
	}
	s.logger.Printf("panic recovered in background worker %s: %s", location, guardrails.Redact(panicValueString(v)))
}

// logPanicError logs a PanicError the engine returned (already
// counted by the engine — this only logs, never counts). Non-panic
// errors are ignored, and so are server-side panics (locations
// "server_*"): those were already logged once by noteServerPanic
// at the recover site, and logging them again here would double
// the line. Call it exactly once per request path that surfaces
// an engine error, so a recovered panic gets one log line, not
// one per layer it passed through.
func (s *Server) logPanicError(err error) {
	var pe *router.PanicError
	if !errors.As(err, &pe) {
		return
	}
	if strings.HasPrefix(pe.Location, "server_") {
		return
	}
	s.logger.Printf("panic recovered in %s: %s", pe.Location, guardrails.Redact(panicValueString(pe.Value())))
}

// isPanicError reports whether err is (or wraps) a recovered panic.
func isPanicError(err error) bool {
	var pe *router.PanicError
	return errors.As(err, &pe)
}

// writePanicFailure answers a request that died to a recovered
// panic before any response was committed: HTTP 502 with the
// generic message and the JSON-RPC internal-error code -32603, so
// a client can tell it from a policy refusal (-32001) without
// parsing strings. No panic detail is ever exposed to the client.
// It deliberately does NOT count an upstream failure: the gateway
// itself faulted, no provider did — the panic has its own counter
// (codvian_router_panics_total), and conflating the two would lie
// about provider health.
func (s *Server) writePanicFailure(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadGateway, errorBody{Error: errorDetail{
		Message: "gateway internal error",
		Type:    "server_error",
		Code:    guardrails.UpstreamFailureCode,
	}})
}

// traceHasNoAttempts reports whether the engine's most recent
// route trace has no attempts. A trace with entries means the
// engine recorded those attempts (trace and ledger rows are
// written together, in that order), so there is nothing for the
// server backstop to add. Under concurrency the trace may be a
// neighbour's — the same approximation recordMetrics already
// accepts — so this is a best-effort guard against a duplicate
// row, not a guarantee: a zero-cost audit row is occasionally
// skipped, never invented twice for one attempt.
func (s *Server) traceHasNoAttempts() bool {
	if s.engine == nil {
		return true
	}
	tp, ok := s.engine.(traceProvider)
	if !ok || tp == nil {
		return true
	}
	tr := tp.LastTrace()
	return tr == nil || len(tr.Attempts) == 0
}

// recordPanicRow writes one failed ledger row for a request whose
// panic left no attempt on record: provider "unknown", the
// requested model, ok=false, zero tokens, zero cost. keyID is the
// gateway-key ID the request arrived under ("" = anonymous — the
// row is still recorded, as the engine does for anonymous
// requests). Best-effort: a nil ledger, an unparsed request (no
// model to attribute), or a ledger error all skip silently — the
// panic is already counted and logged, and a missing audit row
// must never fail the request a second time.
func (s *Server) recordPanicRow(ctx context.Context, keyID string, req provider.ChatRequest, started time.Time) {
	if s.ledger == nil || req.Model == "" {
		return
	}
	_, _ = s.ledger.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID: keyID,
		Provider:     "unknown",
		Model:        req.Model,
		OK:           false,
		LatencyMS:    time.Since(started).Milliseconds(),
	}, s.getLedgerPrices())
}

// recordPanicRowIfUnrecorded is recordPanicRow guarded by the
// trace check: it records only when the engine recorded no
// attempt for this request (see traceHasNoAttempts).
func (s *Server) recordPanicRowIfUnrecorded(ctx context.Context, keyID string, req provider.ChatRequest, started time.Time) {
	if !s.traceHasNoAttempts() {
		return
	}
	s.recordPanicRow(ctx, keyID, req, started)
}
