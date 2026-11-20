package server

import (
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// traceResponse is the GET /dashboard/trace payload: the route trace
// of the most recently finished request — every attempt in order,
// with its outcome and its error-taxonomy fallback reason. It is the
// dashboard's Live Routing data: which target failed, why
// ("model_not_found", "rate_limited", "request_invalid", ...), and
// which target served the request. Before any request the attempts
// list is empty — an honest empty state, never invented traffic.
// The attempt error strings are already redacted by the trace
// package, so no provider key can reach this feed.
// ContextFit carries the same request's context-window fit-check
// decision (CHECKLIST 159) when the request reached the fit gate:
// the prompt estimate, reserve, required tokens, per-model fit
// verdicts, and which chain served it. It is nil before any
// request reached the gate — the trace attempts alone (including
// any "skipped_context_fit" rows) are the record until then.
// Contract is the per-request routing contract (CHECKLIST 157)
// when the last request carried one: the controls as applied, the
// effective strategy, every provider excluded with its reason, and
// the rejection message when the contract itself was refused (in
// that case Attempts is empty). Nil when the request carried no
// contract.
// Endpoint names the API surface the traced request was made to
// (CHECKLIST 171-172): "" for chat completions, "messages" for a
// translated /v1/messages request (CHECKLIST 211 — the engine
// stamps it; a verbatim-relayed request never reaches the engine
// and so never publishes a trace), "embeddings" or "rerank" for
// those calls. The dashboard renders "" as "chat".
type traceResponse struct {
	Attempts   []routetrace.AttemptTrace   `json:"attempts"`
	Endpoint   string                      `json:"endpoint,omitempty"`
	ContextFit *router.ContextFitDecision  `json:"context_fit,omitempty"`
	Contract   *routetrace.RoutingContract `json:"contract,omitempty"`
	// Compression is the lossless-first compression applied to
	// the same request (CHECKLIST 183): prompt tokens before /
	// after, tokens saved (local estimates), steps, and whether
	// a lossy step ran. Nil when the request did not compress.
	Compression *routetrace.CompressionInfo `json:"compression,omitempty"`
	// StrategyTreePath is the path through the configured
	// strategy tree (CHECKLIST 177) to the leaf that served
	// this request — the strategy of every internal node on
	// the way down, ending with the serving provider ID.
	// Nil when no tree ordered the request.
	StrategyTreePath []string `json:"strategy_tree_path,omitempty"`
	// Hedge records what the engine decided about hedging for
	// this request (CHECKLIST 191): "armed" (the default hedge
	// was armed — the first attempt raced against the second
	// after HedgeDelayMs), "disabled-explicit-order" (no hedge:
	// the chain order was explicit — contract order, group
	// order, or strategy-tree order), or "disabled-exempt" (no
	// hedge: the request is replay-exempt). "" when no hedge
	// decision was recorded. HedgeDelayMs is the real
	// configured delay when armed, 0 otherwise.
	Hedge        string `json:"hedge,omitempty"`
	HedgeDelayMs int64  `json:"hedge_delay_ms,omitempty"`
}

// handleTrace serves GET /dashboard/trace.
func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	attempts := []routetrace.AttemptTrace{}
	var contract *routetrace.RoutingContract
	var endpoint string
	var compression *routetrace.CompressionInfo
	var treePath []string
	var hedge string
	var hedgeDelayMs int64
	if tp, ok := s.engine.(traceProvider); ok {
		if trace := tp.LastTrace(); trace != nil {
			attempts = trace.Attempts
			contract = trace.Contract
			endpoint = trace.Endpoint
			compression = trace.Compression
			treePath = trace.StrategyTreePath
			hedge = trace.Hedge
			hedgeDelayMs = trace.HedgeDelayMs
		}
	}
	if attempts == nil {
		attempts = []routetrace.AttemptTrace{}
	}
	resp := traceResponse{Attempts: attempts, Endpoint: endpoint, Contract: contract, Compression: compression, StrategyTreePath: treePath, Hedge: hedge, HedgeDelayMs: hedgeDelayMs}
	if dec, ok := s.lastContextFit(); ok {
		resp.ContextFit = &dec
	}
	writeJSON(w, http.StatusOK, resp)
}
