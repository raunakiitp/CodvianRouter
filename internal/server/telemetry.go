// M2 telemetry wiring (W2): feed the observability Collector and
// structured request Logger from the A8 choke points.
//
// One observation per finished inference request. The record sites
// are the A8 choke points themselves — writeResponseHeaders for
// non-streaming responses and the stream-trailer sites for streams
// — plus the committed-error stream ends (which carry no trailer
// per the A8 rule, but are still finished requests). Recording is
// server-side only: it never writes to, or alters, any response
// body. GET /v1/responses/{id} (retrieve) reuses
// writeResponseHeaders but is NOT an inference request, so it is
// deliberately never recorded.
//
// A nil collector / logger means telemetry is off, never a panic:
// both observability methods are nil-safe, and recordTelemetry
// also guards the breaker lookup.
package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/observability"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// WithTelemetryCollector overrides the observability Collector
// that finished inference requests are recorded into. A nil
// collector disables collection (and the collector half of
// /metrics); the default is a fresh collector constructed in New.
func WithTelemetryCollector(c *observability.Collector) Option {
	return func(s *Server) { s.obsCollector = c }
}

// WithTelemetryLogger overrides the structured request Logger that
// one JSON line per finished inference request is written to. A nil
// logger disables request logging; the default writes JSON lines
// to os.Stderr. The Logger has no field for prompt/response
// content or keys, so nothing sensitive can ever be logged.
func WithTelemetryLogger(l *observability.Logger) Option {
	return func(s *Server) { s.obsLogger = l }
}

// WithTracer overrides the request tracer. The default is the
// build-tagged tracer constructed in New (no-op unless -tags otel);
// main passes observability.NewConfiguredTracer(...) here so an
// operator-configured OTLP endpoint exports request spans.
func WithTracer(t *observability.Tracer) Option {
	return func(s *Server) { s.tracer = t }
}

// telemetryOutcome maps a finished HTTP status onto the
// Collector/Logger outcome + error class. 2xx is "ok"; a timeout
// status is the "timeout" outcome (not "error"); 429 is
// "rate_limited"; 401/403 is "auth"; any other 5xx is
// "upstream_5xx"; anything else non-ok is "unknown".
func telemetryOutcome(status int) (outcome, class string) {
	switch {
	case status >= 200 && status < 300:
		return "ok", ""
	case status == http.StatusTooManyRequests:
		return "error", "rate_limited"
	case status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout:
		return "timeout", "timeout"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "error", "auth"
	case status >= 500:
		return "error", "upstream_5xx"
	default:
		return "error", "unknown"
	}
}

// statusForEngineError mirrors handleEngineError's status mapping
// (same checks, same order) so an error path records the outcome
// class of the status the client actually received: 403 for an
// exhausted refusal chain, the contract's own status, 400 for a
// no-fit / unrepresentable / no-capable-provider refusal, 402 for
// a strict budget stop, 504 for a deadline / stream-lifetime /
// context-timeout stop, a ClassifyError 4xx stop status, else 502.
func (s *Server) statusForEngineError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, router.ErrContentRefused) ||
		provider.ClassifyError(err, provider.EndpointChatCompletions).ContentRefusal {
		return http.StatusForbidden
	}
	var ncp *router.NoCapableProviderError
	if errors.As(err, &ncp) {
		return http.StatusBadRequest
	}
	var ce *router.ContractError
	if errors.As(err, &ce) {
		return ce.Status
	}
	var tooLarge *router.ErrContextTooLarge
	if errors.As(err, &tooLarge) {
		return http.StatusBadRequest
	}
	var runrep *reasoning.UnrepresentableError
	if errors.As(err, &runrep) {
		return http.StatusBadRequest
	}
	if router.IsRequestBudgetExceeded(err) {
		return http.StatusPaymentRequired
	}
	if errors.Is(err, router.ErrDeadlineExceeded) ||
		errors.Is(err, router.ErrStreamLifetimeExceeded) ||
		errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	// AUD-REL-001: a classified 4xx is the status the client actually
	// received (the engine may have retried first — the decision says
	// whether the CHAIN continued, not what the last upstream
	// answered). Telemetry must agree with the wire, so any 4xx
	// classification is returned, not only DecisionStop ones; a 0 or
	// 5xx classification still falls through to 502.
	if cls := provider.ClassifyError(err, provider.EndpointChatCompletions); cls.Status >= 400 && cls.Status < 500 {
		return cls.Status
	}
	return http.StatusBadGateway
}

// recordTelemetry records one finished inference request into the
// Collector and the structured request log, from the ResponseMeta
// the A8 choke point already computed. status is the HTTP status
// the client received (or will receive) for this request. A nil
// collector/logger records nothing and never panics. Only measured
// values are recorded: tokens/cost are zero when the meta says
// they were never measured (HasUsage/HasCost false), and a cache
// hit is counted only when a cache layer actually served the
// request (Cache non-empty). No prompt/response content, key, or
// key id is passed anywhere near the log entry.
func (s *Server) recordTelemetry(ctx context.Context, meta ResponseMeta, status int) {
	outcome, class := telemetryOutcome(status)
	// LKGP (M3 Phase C): a successful inference through this
	// provider for this model is a last-known-good observation.
	// The registry key is the requested model (see chainModel in
	// internal/strategy/wiring.go); the key/account id is not in
	// ResponseMeta, so it records as "" (unknown). In-memory
	// only, never I/O, never a panic — and only on success: a
	// failed attempt must never become "last known good".
	if outcome == "ok" && meta.Provider != "" && meta.Model != "" {
		strategy.DefaultLKGPRegistry.RecordSuccessFor(meta.Model, meta.Provider, meta.Model, "")
	}
	var latencyMs, upstreamMs float64
	if d := meta.latencyOf(); d > 0 {
		latencyMs = float64(d) / float64(time.Millisecond)
	}
	if meta.HasUpstreamLatency {
		upstreamMs = float64(meta.UpstreamLatency) / float64(time.Millisecond)
	}
	var tokensIn, tokensOut, tokensCached int64
	var costUSD float64
	if meta.HasUsage {
		tokensIn = int64(meta.Usage.PromptTokens)
		tokensOut = int64(meta.Usage.CompletionTokens)
		tokensCached = int64(meta.Usage.CachedTokens)
	}
	if meta.HasCost {
		costUSD = meta.CostUSD
	}
	requestID := meta.RequestID
	if requestID == "" {
		// Verbatim relay metas carry no id of their own; the
		// logging middleware's id is on the request context.
		requestID = requestIDFromContext(ctx)
	}
	cache := ""
	if meta.Cache != "" {
		cache = "hit"
	}
	s.obsCollector.Record(observability.Observation{
		Provider:         meta.Provider,
		Model:            meta.Model,
		LatencyMs:        latencyMs,
		Outcome:          outcome,
		ErrorClass:       class,
		FallbackAttempts: meta.FallbackAttempts,
		CacheHit:         meta.Cache != "",
		CacheLayer:       meta.Cache,
		TokensIn:         tokensIn,
		TokensOut:        tokensOut,
		TokensCached:     tokensCached,
		CostUSD:          costUSD,
	})
	s.obsLogger.LogRequest(observability.RequestLogEntry{
		RequestID:         requestID,
		Provider:          meta.Provider,
		Model:             meta.Model,
		Outcome:           outcome,
		ErrorClass:        class,
		LatencyMs:         latencyMs,
		UpstreamLatencyMs: upstreamMs,
		TokensIn:          tokensIn,
		TokensOut:         tokensOut,
		TokensCached:      tokensCached,
		CostUSD:           costUSD,
		Cache:             cache,
		CacheLayer:        meta.Cache,
		FallbackAttempts:  meta.FallbackAttempts,
		FallbackPath:      meta.FallbackPath,
		BreakerOpen:       s.breakerOpenFor(meta.Provider),
	})
}

// breakerOpenFor reports whether the collector's most recent
// breaker state for this provider is open. False when no state
// was ever reported for the provider (honest unknown, never
// guessed) or when telemetry is off.
func (s *Server) breakerOpenFor(provider string) bool {
	if provider == "" || s.obsCollector == nil {
		return false
	}
	for _, st := range s.obsCollector.BreakerStates() {
		if st.Provider == provider {
			return st.Open
		}
	}
	return false
}

// refreshBreakerStates probes the engine the way the existing code
// probes optional engine interfaces (see traceProvider and
// breakerReporter) and refreshes the collector before a /metrics
// scrape. No background goroutine, no invented state: an engine
// that does not expose breaker state leaves the collector empty.
func (s *Server) refreshBreakerStates() {
	if s.obsCollector == nil {
		return
	}
	rep, ok := s.engine.(breakerReporter)
	if !ok {
		return
	}
	for _, st := range rep.BreakerStates() {
		s.obsCollector.SetBreakerState(observability.BreakerState{
			Provider:            st.ProviderID,
			Open:                st.Open,
			ConsecutiveFailures: st.ConsecutiveFailures,
			Threshold:           st.Threshold,
		})
	}
}
