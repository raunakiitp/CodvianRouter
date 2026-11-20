// Package routetrace holds the per-request attempt record produced by
// the routing engine. It lives in its own leaf package (it imports
// only internal/guardrails, itself a leaf with no internal imports)
// so both the engine (internal/router) and its consumers
// (internal/server for metrics, internal/degrade which forwards the
// trace) can use it without an import cycle: router_test imports
// server, so server cannot import router.
//
// Redaction: an attempt error can carry an upstream error body, and
// an upstream body is untrusted text — it can quote the request,
// including a key. Add redacts the error string before storing it,
// so the trace can never hold a key even in memory. (Prompt text an
// upstream echoes verbatim is also kept out of logs downstream: the
// server's stream-failure log line carries the status code only.)
package routetrace

import "github.com/raunakiitp/codvian-router/internal/guardrails"

// AttemptTrace records what happened to a single attempt.
type AttemptTrace struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Effort is the reasoning effort that served this attempt
	// (CHECKLIST 255): "" when the attempt ran at no explicit
	// effort (the client's own reasoning_effort, or none),
	// otherwise one of the effort levels the operator configured
	// on the group entry or the client asked for ("low",
	// "medium", "high", ...). The dashboard's Live Routing view
	// renders it so the operator can see which effort served —
	// or failed — each hop. Stamped by the router's trace
	// funnels (stampTraceEffort), never by the trace package
	// itself.
	Effort string `json:"effort,omitempty"`
	// EffortForwarding records whether the attempt's effort was
	// forwarded to the upstream, and with what value (CHECKLIST
	// 285-286, Vertex 303-304): "forwarded:reasoning_effort=low" (OpenAI wire),
	// "forwarded:thinking.budget_tokens=1024" (Anthropic, the
	// operator-declared budget for the effort),
	// "forwarded:thinkingConfig.thinkingBudget=2048" (Gemini and
	// Vertex — Vertex reads the table's own "vertex" entry),
	// "not-forwarded:no-operator-budget-table" (Anthropic /
	// Gemini / Vertex with no table or no entry for the effort),
	// "not-forwarded:no-effort-control" (Cohere, Bedrock
	// Converse — the wire has no effort control at all), or
	// "not-forwarded:unknown-wire" (the client reports no wire
	// format). (Before 303-304 Vertex recorded
	// "not-forwarded:vertex-out-of-scope"; that label is no
	// longer produced, but old traces may still carry it and
	// the dashboard still explains it.) "" when the attempt ran at no explicit effort —
	// there was nothing to forward. The dashboard's Live
	// Routing view and the decision view render it, so the
	// operator can see per wire whether the tier's effort
	// reached the provider. Stamped by the router's trace
	// funnels (stampTraceEffortForwarding), never by the trace
	// package itself.
	EffortForwarding string `json:"effort_forwarding,omitempty"`
	// EffortPolicy records the forced-reasoning policy decision
	// for this attempt (CHECKLIST 311-312):
	// "forced:high:policy:auto/coding" (the operator's policy
	// forced high on the coding route) or
	// "client-override:low:policy:model-b" (the client's value
	// won because that policy allows override). "" when no
	// policy matched this attempt's request or model. The
	// dashboard's Live Routing view and the decision view render
	// it, so the operator can see per attempt which effort was
	// forced and by which policy — never silently. Stamped by
	// the router's trace funnels (stampTraceEffortPolicy), never
	// by the trace package itself.
	EffortPolicy string `json:"effort_policy,omitempty"`
	// KeyID names the provider-side API key that served the
	// attempt (CHECKLIST 175): "" for the provider's legacy
	// single key, a "key_…" ID for one key of a pool. The
	// dashboard's Live Routing view shows it so the operator can
	// see which key served — or failed — each hop.
	KeyID     string `json:"key_id,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Outcome   string `json:"outcome"` // success, error, rate_limited, stream_error, empty_stream,
	// skipped_breaker, skipped_cooldown, skipped_zero_quota, hedged_cancelled
	Error string `json:"error,omitempty"`
	// Preread records what the streaming preread buffer did for
	// this attempt ("" when it did not run — non-streaming
	// requests, or an attempt that committed on its first event
	// without holding anything): "held" (leading events buffered,
	// then committed on first content), "empty" (the stream ended
	// with no content before commit — failed over),
	// "exempt:<slug>" (preread skipped: the request must never be
	// replayed — see router.ReplayExemption). PrereadEvents /
	// PrereadBytes are the buffered counts for "held" (0 otherwise).
	Preread       string `json:"preread,omitempty"`
	PrereadEvents int    `json:"preread_events,omitempty"`
	PrereadBytes  int    `json:"preread_bytes,omitempty"`
	// Reason is the error-taxonomy slug for a failed attempt
	// (e.g. "model_not_found", "rate_limited", "request_invalid"
	// — see internal/provider.ClassifyError). It is "" for
	// successes and skips, which need no reason: the outcome
	// already says what happened. The dashboard's Live Routing
	// view renders it as the fallback reason.
	Reason string `json:"reason,omitempty"`
	// Terminal records how this attempt's stream ended for the
	// client (CHECKLIST 153): "done" (the terminal Done event
	// — the server renders it as [DONE]), "error" (the terminal
	// Err event after content was committed — the server renders
	// it as an SSE error frame followed by [DONE]), "cancelled"
	// (the client went away before the stream finished — the
	// terminal Err event was attempted best-effort; delivery to
	// a gone client is impossible). It is "" when the attempt
	// never reached a terminal state for the client: a
	// pre-commit failover (the next attempt owns the terminal)
	// or a non-streaming attempt (there is no SSE stream to
	// terminate). The dashboard renders it as the stream's
	// ending, from this real trace data — never inferred.
	Terminal string `json:"terminal,omitempty"`
	// RetryAfter records what a rate-limited attempt saw in the
	// upstream's Retry-After hint and what the router did with
	// it (CHECKLIST 167-168). Nil when the attempt carried no
	// Retry-After hint — an absent hint is not a zero hint, so
	// it is omitted rather than recorded as 0s.
	RetryAfter *RetryAfterInfo `json:"retry_after,omitempty"`
	// Reasoning records what happened to the restored reasoning
	// state on this hop (CHECKLIST 163-164): "preserved" (the
	// state replayed verbatim), "translated: ..." (replayed in
	// the target wire's shape, with the detail naming what
	// changed), "dropped: ..." (a part the target cannot carry
	// was dropped, with the reason), or "unrepresentable: ..."
	// (the hop failed because the target wire cannot represent
	// the state). It is "" when the request carried no reasoning
	// state. The label names only block kinds, wires and counts
	// — never reasoning or prompt text — and comes from the same
	// reasoning.Check verdict the wire converter enforced, so
	// the label and the behaviour cannot drift.
	Reasoning string `json:"reasoning,omitempty"`
	// Region is the effective serving region of this attempt
	// (CHECKLIST 257): the operator-declared region of the
	// model, else of the provider, else "" (unknown — never a
	// default). The engine stamps it when the trace is
	// published, from the same RegionFor the ordering used, so
	// the trace and the order cannot disagree about where an
	// attempt ran.
	Region string `json:"region,omitempty"`
	// FailoverStep names the region-aware failover step this
	// attempt represents (CHECKLIST 257-258), relative to the
	// previous tried attempt in the same request: "cross-region"
	// (same model, a different declared region), "equivalent-model"
	// (a different model from another provider), "self-hosted"
	// (a self-hosted open-weight model). "" on the first attempt
	// (no failover yet), on a retry of the same target, and on a
	// same-model same-region hop to another provider — none of
	// those is a region step. The dashboard's Live Routing view
	// renders it, and the decision header carries it for the
	// serving attempt (see router.RoutingInfo.RegionStep).
	FailoverStep string `json:"failover_step,omitempty"`
}

// RetryAfterInfo is the Retry-After record for one rate-limited
// attempt: the hint as the upstream sent it (Seconds — 0 for an
// explicit "0"), the decision the router took (Decision —
// "abandoned" | "floored" | "capped" | "honoured", see
// internal/router), and the wait the retry path actually used
// (WaitSeconds — 0 for an abandoned target, which is never
// retried in the request; 10 for a floored hint).
type RetryAfterInfo struct {
	Seconds     float64 `json:"seconds"`
	Decision    string  `json:"decision"`
	WaitSeconds float64 `json:"wait_seconds"`
}

// CompressionInfo is the lossless-first compression applied to
// one request (CHECKLIST 183): the local tiktoken estimate of the
// prompt before and after compression, how many tokens that saved,
// which steps ran, and whether a lossy step ran. All counts are
// local estimates, never provider-reported numbers. Nil when the
// request did not compress — an absent block is more honest than
// a zeroed one. The dashboard's Live Routing view renders it from
// the real trace, and the response carries the same facts as
// X-Codvian-Compression headers.
type CompressionInfo struct {
	Applied        bool     `json:"applied"`
	Lossless       bool     `json:"lossless,omitempty"`
	LossyApplied   bool     `json:"lossy_applied,omitempty"`
	Steps          []string `json:"steps,omitempty"`
	BeforeTokens   int      `json:"before_tokens"`
	AfterTokens    int      `json:"after_tokens"`
	SavedTokens    int      `json:"saved_tokens"`
	LossyMaxTokens int      `json:"lossy_max_tokens,omitempty"`
	Source         string   `json:"source,omitempty"`
	Verified       bool     `json:"verified,omitempty"`
}

// HedgeModeArmed is the RouteTrace.Hedge value when the engine
// armed the default hedge for the request: the first attempt was
// raced against the second after HedgeDelay. Whether the hedge
// actually fired is visible in the attempts — a "hedged_cancelled"
// row means it fired and the first attempt lost; no such row
// means the first attempt answered before the delay.
const HedgeModeArmed = "armed"

// HedgeModeDisabledExplicitOrder is the RouteTrace.Hedge value
// when the engine launched NO hedge because the chain order was
// explicit — a per-request contract order, a routing-group member
// order, or a strategy-tree order (CHECKLIST 191): hedging must
// not silently override a stated try order, so an explicit-order
// chain never races its first pick.
const HedgeModeDisabledExplicitOrder = "disabled-explicit-order"

// HedgeModeDisabledExempt is the RouteTrace.Hedge value when the
// engine launched no hedge because the request is replay-exempt
// (a stateful surface or a Responses continuation): exactly one
// attempt by design, so there is nothing to race.
const HedgeModeDisabledExempt = "disabled-exempt"

// HedgeModeDisabledLargeBody is the RouteTrace.Hedge value when
// the engine launched no hedge because the request body exceeds
// the 256 KiB hedge guard (M4.8): hedging a large body doubles
// the upstream traffic for no benefit, so the request runs on a
// single attempt (and still fails over).
const HedgeModeDisabledLargeBody = "disabled-large-body"

// RouteTrace records every attempt made for one request, in order.
// It is returned by Engine.ChatWithChain and available via
// Engine.LastTrace after a ChatStreamWithChain finishes.
//
// Contract is the per-request routing contract (CHECKLIST 157)
// when the request carried one: the controls as applied, the
// effective strategy they produced, and every provider excluded
// with its reason. Nil when the request carried no (non-empty)
// contract — an absent block is more honest than an empty one.
type RouteTrace struct {
	Attempts []AttemptTrace
	Contract *RoutingContract `json:"contract,omitempty"`
	// Hedge records what the engine did with hedged requests for
	// this request (CHECKLIST 191): HedgeModeArmed (the default
	// hedge was armed — the first attempt raced against the
	// second after HedgeDelayMs), HedgeModeDisabledExplicitOrder
	// (no hedge: the chain order was explicit — contract order,
	// group order, or strategy-tree order), or
	// HedgeModeDisabledExempt (no hedge: the request is
	// replay-exempt, exactly one attempt). "" when no hedge
	// decision was recorded — a rejected contract, an empty
	// chain, or a single-attempt chain with nothing to race.
	// The dashboard's Live Routing view renders it so the
	// operator can tell a hedged request from an
	// explicit-order one honestly, instead of inferring it
	// from the attempts.
	Hedge string `json:"hedge,omitempty"`
	// HedgeDelayMs is the hedge delay in milliseconds when
	// Hedge is HedgeModeArmed — the real configured delay,
	// never a hard-coded 150. 0 otherwise.
	HedgeDelayMs int64 `json:"hedge_delay_ms,omitempty"`
	// Endpoint names the API surface the traced request was made
	// to ("" when unset — the chat completions path does not set
	// it — otherwise the provider.EndpointKind string, e.g.
	// "embeddings", "responses"). The embeddings/rerank single-
	// attempt paths set it so the dashboard can tell a stateful
	// surface's trace from a chat trace.
	Endpoint string `json:"endpoint,omitempty"`
	// Compression is the compression applied to this request
	// (CHECKLIST 183), when any was. Nil when none applied.
	Compression *CompressionInfo `json:"compression,omitempty"`
	// StrategyTreePath is the path through the configured
	// strategy tree (CHECKLIST 177) to the leaf that served
	// this request: the strategy of every internal node on the
	// way down, ending with the serving provider ID — e.g.
	// ["cheapest", "fastest", "prov-a"]. Nil when no tree was
	// configured for the chain, or when the serving provider
	// is named by no leaf (an unmatched attempt serving the
	// request has no tree path — an absent path is more honest
	// than an invented one).
	StrategyTreePath []string `json:"strategy_tree_path,omitempty"`
}

// RoutingExclusion records one provider the per-request contract
// kept out of the chain, and why. Reason is a slug the dashboard
// renders as a human label:
//
//   - not_in_only: not listed in routing.only
//   - excluded: listed in routing.exclude
//   - not_in_order: not listed in routing.order (order defines
//     the complete try order)
//   - over_max_price: a known price above routing.max_price
//   - price_unknown: no operator-configured price to check the
//     ceiling against — excluded, never assumed cheap
//   - data_collection_deny: not verified as excluding
//     training-data collection under routing.data_collection=deny
type RoutingExclusion struct {
	Provider string `json:"provider"`
	Reason   string `json:"reason"`
}

// RoutingMaxPrice is the max_price ceiling as applied, in USD per
// 1M tokens per leg (a leg is present only when the client set it).
type RoutingMaxPrice struct {
	Input  *float64 `json:"input,omitempty"`
	Output *float64 `json:"output,omitempty"`
}

// RoutingContract is the applied per-request routing contract for
// one request: the controls as supplied (Order/Only/Exclude/Sort/
// MaxPrice/DataCollection/Group), the effective strategy the contract
// produced (Strategy — the sort mapping, or the strategy that
// would have applied without it), every provider excluded with
// its reason, and Error when the contract itself was rejected
// (unknown provider, empty result set) — in that case Attempts is
// empty and the dashboard shows the rejection, not a fake trace.
type RoutingContract struct {
	Order          []string         `json:"order,omitempty"`
	Only           []string         `json:"only,omitempty"`
	Exclude        []string         `json:"exclude,omitempty"`
	Sort           string           `json:"sort,omitempty"`
	MaxPrice       *RoutingMaxPrice `json:"max_price,omitempty"`
	DataCollection string           `json:"data_collection,omitempty"`
	// Group is the operator-named routing group the request
	// addressed (CHECKLIST 173): the group's members, in config
	// order, became the fallback chain for the request's model.
	// "" when the request named no group.
	Group    string             `json:"group,omitempty"`
	Strategy string             `json:"strategy,omitempty"`
	Excluded []RoutingExclusion `json:"excluded,omitempty"`
	Error    string             `json:"error,omitempty"`
}

// Add appends one attempt to the trace. The error string is
// redacted before it is stored (see the package doc): a trace must
// never hold a provider key, even in memory. The reason is "" —
// use AddWithReason when the caller classified the failure.
func (t *RouteTrace) Add(provider, model string, latencyMs int64, outcome string, err error) {
	t.AddWithReason(provider, model, latencyMs, outcome, err, "")
}

// AddWithReason is Add with the error-taxonomy reason slug for a
// failed attempt ("" for successes and skips).
func (t *RouteTrace) AddWithReason(provider, model string, latencyMs int64, outcome string, err error, reason string) {
	t.AddWithPreread(provider, model, latencyMs, outcome, err, reason, "", 0, 0)
}

// AddWithPreread is AddWithReason plus the preread-buffer record
// for the attempt ("" / 0 / 0 when the preread buffer did not run).
func (t *RouteTrace) AddWithPreread(provider, model string, latencyMs int64, outcome string, err error, reason, preread string, prereadEvents, prereadBytes int) {
	t.AddWithPrereadRetry(provider, model, latencyMs, outcome, err, reason, preread, prereadEvents, prereadBytes, nil)
}

// AddWithPrereadRetry is AddWithPreread plus the Retry-After record
// for the attempt (nil when the attempt carried no Retry-After
// hint — the field is omitted from the JSON feed then, never
// recorded as a zero hint).
func (t *RouteTrace) AddWithPrereadRetry(provider, model string, latencyMs int64, outcome string, err error, reason, preread string, prereadEvents, prereadBytes int, retry *RetryAfterInfo) {
	t.AddWithPrereadTerminal(provider, model, latencyMs, outcome, err, reason, preread, prereadEvents, prereadBytes, retry, "")
}

// AddWithReasoning is AddWithReason plus the reasoning-handoff
// label for the attempt ("" when the request carried no reasoning
// state — see AttemptTrace.Reasoning).
func (t *RouteTrace) AddWithReasoning(provider, model string, latencyMs int64, outcome string, err error, reason, reasoning string) {
	t.AddWithPrereadTerminalReasoning(provider, model, latencyMs, outcome, err, reason, "", 0, 0, nil, "", reasoning)
}

// AddWithPrereadTerminal is AddWithPrereadRetry plus the stream
// terminal record for the attempt ("done" | "error" | "cancelled",
// "" when the attempt never reached a terminal state for the
// client — see AttemptTrace.Terminal).
func (t *RouteTrace) AddWithPrereadTerminal(provider, model string, latencyMs int64, outcome string, err error, reason, preread string, prereadEvents, prereadBytes int, retry *RetryAfterInfo, terminal string) {
	t.AddWithPrereadTerminalReasoning(provider, model, latencyMs, outcome, err, reason, preread, prereadEvents, prereadBytes, retry, terminal, "")
}

// AddWithPrereadTerminalReasoning is AddWithPrereadTerminal plus
// the reasoning-handoff label for the attempt ("" when the request
// carried no reasoning state — see AttemptTrace.Reasoning). It is
// the funnel every Add variant reaches.
func (t *RouteTrace) AddWithPrereadTerminalReasoning(provider, model string, latencyMs int64, outcome string, err error, reason, preread string, prereadEvents, prereadBytes int, retry *RetryAfterInfo, terminal, reasoning string) {
	a := AttemptTrace{Provider: provider, Model: model, LatencyMs: latencyMs, Outcome: outcome, Reason: reason, Preread: preread, PrereadEvents: prereadEvents, PrereadBytes: prereadBytes, RetryAfter: retry, Terminal: terminal, Reasoning: reasoning}
	if err != nil {
		a.Error = guardrails.Redact(err.Error())
	}
	t.Attempts = append(t.Attempts, a)
}
