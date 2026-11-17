// Package routetrace holds the per-request attempt record produced by
// the routing engine. It lives in its own leaf package (no internal
// imports) so both the engine (internal/router) and its consumers
// (internal/server for metrics, internal/degrade which forwards the
// trace) can use it without an import cycle: router_test imports
// server, so server cannot import router.
package routetrace

// AttemptTrace records what happened to a single attempt.
type AttemptTrace struct {
	Provider  string
	Model     string
	LatencyMs int64
	Outcome   string // success, error, rate_limited, stream_error,
	// skipped_breaker, skipped_cooldown, skipped_zero_quota, hedged_cancelled
	Error string
}

// RouteTrace records every attempt made for one request, in order.
// It is returned by Engine.ChatWithChain and available via
// Engine.LastTrace after a ChatStreamWithChain finishes.
type RouteTrace struct {
	Attempts []AttemptTrace
}

// Add appends one attempt to the trace.
func (t *RouteTrace) Add(provider, model string, latencyMs int64, outcome string, err error) {
	a := AttemptTrace{Provider: provider, Model: model, LatencyMs: latencyMs, Outcome: outcome}
	if err != nil {
		a.Error = err.Error()
	}
	t.Attempts = append(t.Attempts, a)
}
