// Package metrics provides Prometheus-style observability for Codvian Router
// using only the standard library.
//
// It implements Prometheus text exposition by hand; it does NOT depend on
// the Prometheus client library. It is NOT the OpenTelemetry SDK and emits
// no OTLP — tracing here is a local JSON trace log (see trace.go).
//
// Privacy: the recorder only ever sees provider/model/outcome/layer label
// values. It never logs API keys, request/response message content, or any
// other payload data. Label values are sanitized before exposition.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// LatencyBuckets are the upper bounds (inclusive, in milliseconds) for
// codvian_router_request_latency_ms. A final +Inf bucket is always emitted.
var LatencyBuckets = []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

type requestKey struct {
	provider string
	model    string
	outcome  string
}

type failoverKey struct {
	from string
	to   string
}

type latencyKey struct {
	provider string
	model    string
}

// usageKey is one token/cost series. The kind is drawn from a fixed
// vocabulary (prompt/completion/cached/reasoning for tokens, reported
// for cost), so a provider-controlled string can never create an
// unbounded number of series. Provider and model are sanitized through
// normalizeProviderModel — no key, prompt or payload ever reaches here.
type usageKey struct {
	provider string
	model    string
	kind     string
}

type concurrencyKey struct {
	provider string
}

// guardrailEvalKey is one (phase, action) guardrail evaluation
// bucket: phase "request" / "response" / "tool_args", action
// "allowed" / "redacted" / "blocked".
type guardrailEvalKey struct {
	phase  string
	action string
}

type histogram struct {
	buckets []uint64 // one per LatencyBuckets entry, cumulative counts
	sum     float64
	count   uint64
}

// Recorder is a concurrency-safe in-memory store for router metrics.
// The zero value is usable, but prefer NewRecorder.
type Recorder struct {
	mu          sync.Mutex
	requests    map[requestKey]uint64
	failovers   map[failoverKey]uint64
	cacheHits   map[string]uint64
	cacheMisses map[string]uint64
	// Semantic-hit vector space (Stream D observability): which vector
	// space produced each served semantic hit ("neural" /
	// "bag-of-words"), so neural-vs-BoW serving is visible per hit,
	// not just in aggregate.
	semanticVectorSource map[string]uint64
	// Cache store refusals by poison reason (Stream D poison guards):
	// one per response the store gate refused to cache ("refusal",
	// "truncated", "tool_calls", "empty", "temperature").
	cacheStoreRefused map[string]uint64
	budgetRefused     uint64
	coalesced         uint64
	inFlight          int64
	latency           map[latencyKey]*histogram
	// Adaptive concurrency limiter state, fed by the router (see
	// SetConcurrencyLimit / IncConcurrencyRejected): the current
	// cap per provider and how many calls were shed by it.
	concurrencyLimit    map[concurrencyKey]float64
	concurrencyRejected map[concurrencyKey]uint64
	// Guardrail counters, fed by the server (see IncGuardrail):
	// redactions and blocks per kind (pii/secret/injection).
	guardrailRedacted map[string]uint64
	guardrailBlocked  map[string]uint64
	// Guardrail evaluations by (phase, action), fed by the server
	// (see IncGuardrailEval): one entry per Check call — phase is
	// "request" / "response" / "tool_args", action is "allowed" /
	// "redacted" / "blocked". The per-kind split lives in the two
	// maps above.
	guardrailEvals map[guardrailEvalKey]uint64
	// Upstream failures: chat requests whose engine error was an
	// upstream failure (a 502 from an exhausted chain, or a
	// terminal upstream 4xx). Deadline stops and policy refusals
	// are NOT counted here — they are router-side, not upstream.
	upstreamFailures uint64
	// Content-policy refusal chain (CHECKLIST 161), fed by the
	// engine (see SetRefusalMetrics): refusals seen, requests
	// served by a refusal-chain hop, and requests that ended in
	// an honest refusal because the chain ran dry. A refusal is
	// NOT an upstream failure (it says nothing about provider
	// health), so these counters are separate from
	// upstreamFailures above, never folded into it.
	contentRefusals       uint64
	refusalChainServed    uint64
	refusalChainExhausted uint64
	// Panics recovered at request/stream boundaries, by location
	// (see internal/router/panic.go for the location list):
	// provider_chat / provider_stream (a provider client or chunk
	// converter panicked), engine_chat / engine_stream (the
	// engine's own machinery panicked), server_* (server-side
	// handler code panicked). A recovered panic is counted exactly
	// once, at the boundary that caught it — never silently
	// swallowed, never double-counted. Zero until a panic
	// actually happens: an empty map is the honest "no panics"
	// state, never an invented series.
	panics map[string]uint64
	// Token usage by (provider, model, kind) and upstream-REPORTED
	// cost in micro-USD by (provider, model, "reported"). Computed
	// pricing lives in the ledger (/api/analytics), never here:
	// the metrics recorder must not duplicate pricing logic.
	usageTokens     map[usageKey]uint64
	usageCostMicros map[usageKey]uint64
}

// NewRecorder returns an initialized Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		requests:             make(map[requestKey]uint64),
		failovers:            make(map[failoverKey]uint64),
		cacheHits:            make(map[string]uint64),
		cacheMisses:          make(map[string]uint64),
		semanticVectorSource: make(map[string]uint64),
		cacheStoreRefused:    make(map[string]uint64),
		latency:              make(map[latencyKey]*histogram),
		concurrencyLimit:     make(map[concurrencyKey]float64),
		concurrencyRejected:  make(map[concurrencyKey]uint64),
		guardrailRedacted:    make(map[string]uint64),
		guardrailBlocked:     make(map[string]uint64),
		guardrailEvals:       make(map[guardrailEvalKey]uint64),
		panics:               make(map[string]uint64),
		usageTokens:          make(map[usageKey]uint64),
		usageCostMicros:      make(map[usageKey]uint64),
	}
}

// sanitizeLabel keeps only [a-zA-Z0-9_:], replacing every other byte with
// "_". This keeps label values safe for Prometheus text exposition.
func sanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == ':' {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// normalizeProviderModel sanitizes a provider/model label value, mapping
// an empty value to "unknown".
func normalizeProviderModel(s string) string {
	if s == "" {
		return "unknown"
	}
	v := sanitizeLabel(s)
	if v == "" {
		return "unknown"
	}
	return v
}

// normalizeOutcome sanitizes an outcome label. Unknown outcomes are kept
// as-is (sanitized) rather than silently remapped, so a caller bug stays
// visible in the exposition; the documented values are
// "ok", "error" and "rate_limited".
func normalizeOutcome(s string) string {
	return sanitizeLabel(s)
}

// normalizeUsageKind maps a token/cost kind to a FIXED vocabulary. An
// unexpected value never becomes a new label — it collapses to "other",
// keeping cardinality bounded even if a future caller passes junk.
func normalizeUsageKind(kind string) string {
	switch kind {
	case "prompt", "completion", "cached", "reasoning", "reported":
		return kind
	default:
		return "other"
	}
}

// IncUsageTokens adds tokens to codvian_router_usage_tokens_total for a
// provider/model and one of the fixed kinds: prompt, completion, cached
// or reasoning. Zero/negative counts are ignored (no invented series),
// and both label values pass through the same sanitizer as every other
// metric — provider-supplied model names cannot inject keys or payload.
// The engine calls this from its single recordAttempt path, so streaming
// and non-streaming requests are counted exactly once each.
func (r *Recorder) IncUsageTokens(provider, model, kind string, tokens int) {
	if tokens <= 0 {
		return
	}
	k := usageKey{normalizeProviderModel(provider), normalizeProviderModel(model), normalizeUsageKind(kind)}
	r.mu.Lock()
	if r.usageTokens == nil {
		r.usageTokens = make(map[usageKey]uint64)
	}
	r.usageTokens[k] += uint64(tokens)
	r.mu.Unlock()
}

// AddReportedCost adds an upstream-reported cost to
// codvian_router_usage_cost_usd_total{source="reported"}. Only a real,
// finite, positive USD amount is counted — a nil/NaN/Inf/negative
// report is ignored, never rounded into a fake $0 series. Hand-computed
// pricing is deliberately NOT duplicated here: it belongs to the ledger
// and its analytics API, which already reconciles reported vs priced.
func (r *Recorder) AddReportedCost(provider, model string, usd float64) {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return
	}
	micros := uint64(math.Round(usd * 1e6))
	if micros == 0 {
		return
	}
	k := usageKey{normalizeProviderModel(provider), normalizeProviderModel(model), "reported"}
	r.mu.Lock()
	if r.usageCostMicros == nil {
		r.usageCostMicros = make(map[usageKey]uint64)
	}
	r.usageCostMicros[k] += micros
	r.mu.Unlock()
}

// IncRequest increments codvian_router_requests_total for the given
// provider, model and outcome ("ok", "error" or "rate_limited").
// Empty provider/model are recorded as "unknown".
func (r *Recorder) IncRequest(provider, model, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.requests == nil {
		r.requests = make(map[requestKey]uint64)
	}
	k := requestKey{normalizeProviderModel(provider), normalizeProviderModel(model), normalizeOutcome(outcome)}
	r.requests[k]++
}

// ObserveLatency records one request latency (in milliseconds) for the
// given provider/model in codvian_router_request_latency_ms.
// Empty provider/model are recorded as "unknown". Negative values are
// clamped to 0.
func (r *Recorder) ObserveLatency(provider, model string, ms float64) {
	if ms < 0 {
		ms = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.latency == nil {
		r.latency = make(map[latencyKey]*histogram)
	}
	k := latencyKey{normalizeProviderModel(provider), normalizeProviderModel(model)}
	h, ok := r.latency[k]
	if !ok {
		h = &histogram{buckets: make([]uint64, len(LatencyBuckets))}
		r.latency[k] = h
	}
	for i, b := range LatencyBuckets {
		if ms <= b {
			h.buckets[i]++
		}
	}
	h.sum += ms
	h.count++
}

// IncFailover increments codvian_router_failovers_total for a failover
// from fromProvider to toProvider.
func (r *Recorder) IncFailover(fromProvider, toProvider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failovers == nil {
		r.failovers = make(map[failoverKey]uint64)
	}
	k := failoverKey{sanitizeLabel(fromProvider), sanitizeLabel(toProvider)}
	r.failovers[k]++
}

// IncCacheHit increments codvian_router_cache_hits_total for a cache layer
// (e.g. "exact" or "semantic").
func (r *Recorder) IncCacheHit(layer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cacheHits == nil {
		r.cacheHits = make(map[string]uint64)
	}
	r.cacheHits[sanitizeLabel(layer)]++
}

// IncCacheMiss increments codvian_router_cache_misses_total for a cache layer.
func (r *Recorder) IncCacheMiss(layer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cacheMisses == nil {
		r.cacheMisses = make(map[string]uint64)
	}
	r.cacheMisses[sanitizeLabel(layer)]++
}

// IncSemanticVectorSource increments
// codvian_router_semantic_hits_by_vector_source_total for one served
// semantic cache hit, labelled with the vector space the hit was
// matched in ("neural" or "bag-of-words"). Called by the serving
// engine/relay on every semantic hit; exact hits never reach it.
func (r *Recorder) IncSemanticVectorSource(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.semanticVectorSource == nil {
		r.semanticVectorSource = make(map[string]uint64)
	}
	r.semanticVectorSource[sanitizeLabel(source)]++
}

// IncCacheStoreRefused increments
// codvian_router_cache_store_refused_total for one response the cache
// store gate refused to store, labelled with the poison reason
// ("refusal", "truncated", "tool_calls", "empty", "temperature").
func (r *Recorder) IncCacheStoreRefused(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cacheStoreRefused == nil {
		r.cacheStoreRefused = make(map[string]uint64)
	}
	r.cacheStoreRefused[sanitizeLabel(reason)]++
}

// IncBudgetRefused increments codvian_router_budget_refused_total.
func (r *Recorder) IncBudgetRefused() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgetRefused++
}

// IncCoalescedShared increments codvian_router_coalesced_total: one
// per waiter served from a shared single-flight call. Each increment
// is one upstream call avoided (and one quota hit saved); the
// leader's own call is counted in the request counters, not here.
func (r *Recorder) IncCoalescedShared() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.coalesced++
}

// Coalesced returns the current codvian_router_coalesced_total count.
func (r *Recorder) Coalesced() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.coalesced
}

// SetConcurrencyLimit records the adaptive concurrency cap for one
// provider (codvian_router_concurrency_limit gauge). Called by the
// router whenever a completed call moves the cap.
func (r *Recorder) SetConcurrencyLimit(provider string, limit float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrencyLimit == nil {
		r.concurrencyLimit = make(map[concurrencyKey]float64)
	}
	r.concurrencyLimit[concurrencyKey{normalizeProviderModel(provider)}] = limit
}

// IncConcurrencyRejected increments
// codvian_router_concurrency_rejected_total for one provider: one
// call the adaptive limiter shed to another provider instead of
// admitting.
func (r *Recorder) IncConcurrencyRejected(provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.concurrencyRejected == nil {
		r.concurrencyRejected = make(map[concurrencyKey]uint64)
	}
	r.concurrencyRejected[concurrencyKey{normalizeProviderModel(provider)}]++
}

// IncGuardrail records one guardrail outcome for /metrics: a
// redaction of kind (pii/secret/injection), and — when blocked is
// true — a block of the same kind. Only the kind label is recorded,
// never any matched value or prompt text.
func (r *Recorder) IncGuardrail(kind string, blocked bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.guardrailRedacted == nil {
		r.guardrailRedacted = make(map[string]uint64)
	}
	if r.guardrailBlocked == nil {
		r.guardrailBlocked = make(map[string]uint64)
	}
	k := sanitizeLabel(kind)
	r.guardrailRedacted[k]++
	if blocked {
		r.guardrailBlocked[k]++
	}
}

// IncGuardrailEval records one guardrail evaluation for /metrics:
// one Check call in phase (request/response/tool_args) that ended
// with action (allowed/redacted/blocked). Called once per Check, so
// a phase's rows sum to its evaluation count. The per-kind
// redaction/block counters are kept separately by IncGuardrail.
func (r *Recorder) IncGuardrailEval(phase, action string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.guardrailEvals == nil {
		r.guardrailEvals = make(map[guardrailEvalKey]uint64)
	}
	r.guardrailEvals[guardrailEvalKey{sanitizeLabel(phase), sanitizeLabel(action)}]++
}

// IncUpstreamFailure increments codvian_router_upstream_failures_total:
// one chat request whose engine error was an upstream failure.
func (r *Recorder) IncUpstreamFailure() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upstreamFailures++
}

// IncPanic increments codvian_router_panics_total{location}: one
// panic recovered at a request/stream boundary instead of killing
// the gateway (see internal/router/panic.go for the location list).
// The location is sanitized like every other label; an empty
// location is recorded as "unknown" rather than dropped, so a
// caller bug stays visible.
func (r *Recorder) IncPanic(location string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.panics == nil {
		r.panics = make(map[string]uint64)
	}
	loc := sanitizeLabel(location)
	if loc == "" {
		loc = "unknown"
	}
	r.panics[loc]++
}

// Panics returns the current codvian_router_panics_total count
// summed over every location.
func (r *Recorder) Panics() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var total uint64
	for _, n := range r.panics {
		total += n
	}
	return total
}

// UpstreamFailures returns the current
// codvian_router_upstream_failures_total count.
func (r *Recorder) UpstreamFailures() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upstreamFailures
}

// IncContentRefusal increments
// codvian_router_content_refusals_total: one upstream
// content-policy / safety refusal seen on one attempt. A refusal
// is not an upstream failure — it never touches
// upstreamFailures.
func (r *Recorder) IncContentRefusal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.contentRefusals++
}

// IncRefusalChainServed increments
// codvian_router_refusal_chain_served_total: one request served
// by a refusal-chain hop after an earlier target refused the
// content.
func (r *Recorder) IncRefusalChainServed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusalChainServed++
}

// IncRefusalChainExhausted increments
// codvian_router_refusal_chain_exhausted_total: one request that
// ended in an honest refusal (403 / -32001) because the refusal
// chain ran dry.
func (r *Recorder) IncRefusalChainExhausted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refusalChainExhausted++
}

// ContentRefusals returns the current
// codvian_router_content_refusals_total count.
func (r *Recorder) ContentRefusals() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.contentRefusals
}

// RefusalChainServed returns the current
// codvian_router_refusal_chain_served_total count.
func (r *Recorder) RefusalChainServed() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusalChainServed
}

// RefusalChainExhausted returns the current
// codvian_router_refusal_chain_exhausted_total count.
func (r *Recorder) RefusalChainExhausted() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.refusalChainExhausted
}

// SetInFlight sets the codvian_router_in_flight gauge to n.
// Negative values are clamped to 0.
func (r *Recorder) SetInFlight(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 {
		n = 0
	}
	r.inFlight = int64(n)
}

// IncInFlight increments the codvian_router_in_flight gauge by 1.
func (r *Recorder) IncInFlight() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inFlight++
}

// DecInFlight decrements the codvian_router_in_flight gauge by 1,
// clamping at 0 so a mismatched Dec cannot drive it negative.
func (r *Recorder) DecInFlight() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inFlight > 0 {
		r.inFlight--
	}
}

// escapeLabelValue escapes backslash, double-quote and newline for a
// Prometheus label value. Values are already sanitized, so this is
// defence-in-depth.
func escapeLabelValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return s
}

func formatLabels(pairs ...string) string {
	// pairs are alternating key, value (already sanitized).
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `%s="%s"`, pairs[i], escapeLabelValue(pairs[i+1]))
	}
	b.WriteByte('}')
	return b.String()
}

// formatFloat renders a float the way Prometheus exposition expects:
// integers without a decimal point, otherwise shortest repr.
func formatFloat(f float64) string {
	return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%v", f), ".0"), "")
}

// WriteTo renders the current metrics in Prometheus text exposition
// format to b. It never logs keys or message content — it only emits the
// label values recorded above.
func (r *Recorder) WriteTo(b *strings.Builder) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Requests counter.
	b.WriteString("# HELP codvian_router_requests_total Total router requests by provider, model and outcome.\n")
	b.WriteString("# TYPE codvian_router_requests_total counter\n")
	reqKeys := make([]requestKey, 0, len(r.requests))
	for k := range r.requests {
		reqKeys = append(reqKeys, k)
	}
	sort.Slice(reqKeys, func(i, j int) bool {
		if reqKeys[i].provider != reqKeys[j].provider {
			return reqKeys[i].provider < reqKeys[j].provider
		}
		if reqKeys[i].model != reqKeys[j].model {
			return reqKeys[i].model < reqKeys[j].model
		}
		return reqKeys[i].outcome < reqKeys[j].outcome
	})
	for _, k := range reqKeys {
		fmt.Fprintf(b, "codvian_router_requests_total%s %d\n",
			formatLabels("provider", k.provider, "model", k.model, "outcome", k.outcome), r.requests[k])
	}

	// Token usage and upstream-reported cost counters. Only real
	// values are ever recorded (see IncUsageTokens/AddReportedCost):
	// an empty map emits HELP/TYPE but no fake zero series.
	b.WriteString("# HELP codvian_router_usage_tokens_total Tokens processed by provider, model and token kind (prompt/completion/cached/reasoning) from provider-reported usage.\n")
	b.WriteString("# TYPE codvian_router_usage_tokens_total counter\n")
	writeUsageCounter(b, "codvian_router_usage_tokens_total", "kind", r.usageTokens, false)
	b.WriteString("# HELP codvian_router_usage_cost_usd_total Upstream-reported cost in USD by provider and model; excludes computed pricing, which the ledger analytics API reports.\n")
	b.WriteString("# TYPE codvian_router_usage_cost_usd_total counter\n")
	writeUsageCounter(b, "codvian_router_usage_cost_usd_total", "source", r.usageCostMicros, true)

	// Failovers counter.
	b.WriteString("# HELP codvian_router_failovers_total Total failovers between providers.\n")
	b.WriteString("# TYPE codvian_router_failovers_total counter\n")
	foKeys := make([]failoverKey, 0, len(r.failovers))
	for k := range r.failovers {
		foKeys = append(foKeys, k)
	}
	sort.Slice(foKeys, func(i, j int) bool {
		if foKeys[i].from != foKeys[j].from {
			return foKeys[i].from < foKeys[j].from
		}
		return foKeys[i].to < foKeys[j].to
	})
	for _, k := range foKeys {
		fmt.Fprintf(b, "codvian_router_failovers_total%s %d\n",
			formatLabels("from_provider", k.from, "to_provider", k.to), r.failovers[k])
	}

	// Cache hits / misses.
	b.WriteString("# HELP codvian_router_cache_hits_total Total cache hits by layer.\n")
	b.WriteString("# TYPE codvian_router_cache_hits_total counter\n")
	writeStringCounter(b, "codvian_router_cache_hits_total", "layer", r.cacheHits)
	b.WriteString("# HELP codvian_router_cache_misses_total Total cache misses by layer.\n")
	b.WriteString("# TYPE codvian_router_cache_misses_total counter\n")
	writeStringCounter(b, "codvian_router_cache_misses_total", "layer", r.cacheMisses)
	// Semantic hits by the vector space they were matched in (neural
	// embedding vs hashed bag-of-words fallback) — Stream D
	// observability: which space actually serves.
	b.WriteString("# HELP codvian_router_semantic_hits_by_vector_source_total Served semantic cache hits by vector space (neural or bag-of-words).\n")
	b.WriteString("# TYPE codvian_router_semantic_hits_by_vector_source_total counter\n")
	writeStringCounter(b, "codvian_router_semantic_hits_by_vector_source_total", "source", r.semanticVectorSource)
	// Responses the cache store gate refused to store, by poison reason.
	b.WriteString("# HELP codvian_router_cache_store_refused_total Responses refused by the cache store poison guard, by reason (refusal, truncated, tool_calls, empty, temperature).\n")
	b.WriteString("# TYPE codvian_router_cache_store_refused_total counter\n")
	writeStringCounter(b, "codvian_router_cache_store_refused_total", "reason", r.cacheStoreRefused)

	// Budget refused.
	b.WriteString("# HELP codvian_router_budget_refused_total Total requests refused by the budget guard.\n")
	b.WriteString("# TYPE codvian_router_budget_refused_total counter\n")
	fmt.Fprintf(b, "codvian_router_budget_refused_total %d\n", r.budgetRefused)

	// Guardrail redactions / blocks by kind (labels only — no prompt
	// text or matched values ever reach a metric).
	b.WriteString("# HELP codvian_router_guardrail_redactions_total Total guardrail redactions by kind.\n")
	b.WriteString("# TYPE codvian_router_guardrail_redactions_total counter\n")
	writeStringCounter(b, "codvian_router_guardrail_redactions_total", "kind", r.guardrailRedacted)
	b.WriteString("# HELP codvian_router_guardrail_blocked_total Total requests blocked by a guardrail, by kind.\n")
	b.WriteString("# TYPE codvian_router_guardrail_blocked_total counter\n")
	writeStringCounter(b, "codvian_router_guardrail_blocked_total", "kind", r.guardrailBlocked)

	// Guardrail evaluations by phase and action: one entry per Check
	// call. A policy refusal is action="blocked" (HTTP 403, code
	// -32001); an upstream failure is NOT here — it has its own
	// counter below, so the two can never be confused.
	b.WriteString("# HELP codvian_router_guardrail_evaluations_total Total guardrail evaluations by phase and action.\n")
	b.WriteString("# TYPE codvian_router_guardrail_evaluations_total counter\n")
	evKeys := make([]guardrailEvalKey, 0, len(r.guardrailEvals))
	for k := range r.guardrailEvals {
		evKeys = append(evKeys, k)
	}
	sort.Slice(evKeys, func(i, j int) bool {
		if evKeys[i].phase != evKeys[j].phase {
			return evKeys[i].phase < evKeys[j].phase
		}
		return evKeys[i].action < evKeys[j].action
	})
	for _, k := range evKeys {
		fmt.Fprintf(b, "codvian_router_guardrail_evaluations_total%s %d\n",
			formatLabels("phase", k.phase, "action", k.action), r.guardrailEvals[k])
	}

	// Upstream failures: engine errors that belong to the upstream
	// provider (exhausted-chain 502, or a terminal upstream 4xx).
	// Policy refusals (-32001) are counted in the guardrail
	// evaluations above, never here.
	b.WriteString("# HELP codvian_router_upstream_failures_total Total chat requests that failed with an upstream provider error.\n")
	b.WriteString("# TYPE codvian_router_upstream_failures_total counter\n")
	fmt.Fprintf(b, "codvian_router_upstream_failures_total %d\n", r.upstreamFailures)

	// Content-policy refusals (CHECKLIST 161): upstream refusals
	// of the content itself. These are NOT upstream failures —
	// a refusal says nothing about provider health — so they
	// have their own counters: refusals seen, requests served by
	// a refusal-chain hop, and requests that ended in an honest
	// refusal because the chain ran dry.
	b.WriteString("# HELP codvian_router_content_refusals_total Total upstream content-policy refusals seen, one per refusing attempt.\n")
	b.WriteString("# TYPE codvian_router_content_refusals_total counter\n")
	fmt.Fprintf(b, "codvian_router_content_refusals_total %d\n", r.contentRefusals)
	b.WriteString("# HELP codvian_router_refusal_chain_served_total Total requests served by a refusal-chain hop after an earlier target refused the content.\n")
	b.WriteString("# TYPE codvian_router_refusal_chain_served_total counter\n")
	fmt.Fprintf(b, "codvian_router_refusal_chain_served_total %d\n", r.refusalChainServed)
	b.WriteString("# HELP codvian_router_refusal_chain_exhausted_total Total requests that ended in an honest refusal (403 / -32001) because the refusal chain ran dry.\n")
	b.WriteString("# TYPE codvian_router_refusal_chain_exhausted_total counter\n")
	fmt.Fprintf(b, "codvian_router_refusal_chain_exhausted_total %d\n", r.refusalChainExhausted)

	// Recovered panics: a panic at a request/stream boundary that
	// was caught instead of killing the gateway (CHECKLIST 165).
	// One series per location that actually panicked; no series
	// before the first panic is the honest zero, never a fake
	// placeholder. The recovered value is never a label — it is
	// logged (redacted) by the server, never exposed here.
	b.WriteString("# HELP codvian_router_panics_total Total panics recovered at request/stream boundaries, by location.\n")
	b.WriteString("# TYPE codvian_router_panics_total counter\n")
	writeStringCounter(b, "codvian_router_panics_total", "location", r.panics)

	// Coalesced (single-flight) waiters served from a shared call.
	b.WriteString("# HELP codvian_router_coalesced_total Total requests served from a shared single-flight call (one upstream call avoided per shared request).\n")
	b.WriteString("# TYPE codvian_router_coalesced_total counter\n")
	fmt.Fprintf(b, "codvian_router_coalesced_total %d\n", r.coalesced)

	// In-flight gauge.
	b.WriteString("# HELP codvian_router_in_flight Current in-flight requests.\n")
	b.WriteString("# TYPE codvian_router_in_flight gauge\n")
	fmt.Fprintf(b, "codvian_router_in_flight %d\n", r.inFlight)

	// Adaptive concurrency limits (per provider) and rejections.
	b.WriteString("# HELP codvian_router_concurrency_limit Current adaptive concurrency cap per provider (Gradient2).\n")
	b.WriteString("# TYPE codvian_router_concurrency_limit gauge\n")
	concKeys := make([]concurrencyKey, 0, len(r.concurrencyLimit))
	for k := range r.concurrencyLimit {
		concKeys = append(concKeys, k)
	}
	sort.Slice(concKeys, func(i, j int) bool { return concKeys[i].provider < concKeys[j].provider })
	for _, k := range concKeys {
		fmt.Fprintf(b, "codvian_router_concurrency_limit%s %v\n",
			formatLabels("provider", k.provider), r.concurrencyLimit[k])
	}
	b.WriteString("# HELP codvian_router_concurrency_rejected_total Total calls shed by the adaptive concurrency limiter per provider.\n")
	b.WriteString("# TYPE codvian_router_concurrency_rejected_total counter\n")
	rejKeys := make([]concurrencyKey, 0, len(r.concurrencyRejected))
	for k := range r.concurrencyRejected {
		rejKeys = append(rejKeys, k)
	}
	sort.Slice(rejKeys, func(i, j int) bool { return rejKeys[i].provider < rejKeys[j].provider })
	for _, k := range rejKeys {
		fmt.Fprintf(b, "codvian_router_concurrency_rejected_total%s %d\n",
			formatLabels("provider", k.provider), r.concurrencyRejected[k])
	}

	// Latency histogram.
	b.WriteString("# HELP codvian_router_request_latency_ms Request latency in milliseconds.\n")
	b.WriteString("# TYPE codvian_router_request_latency_ms histogram\n")
	latKeys := make([]latencyKey, 0, len(r.latency))
	for k := range r.latency {
		latKeys = append(latKeys, k)
	}
	sort.Slice(latKeys, func(i, j int) bool {
		if latKeys[i].provider != latKeys[j].provider {
			return latKeys[i].provider < latKeys[j].provider
		}
		return latKeys[i].model < latKeys[j].model
	})
	for _, k := range latKeys {
		h := r.latency[k]
		for i, bound := range LatencyBuckets {
			fmt.Fprintf(b, "codvian_router_request_latency_ms_bucket%s %d\n",
				formatLabels("provider", k.provider, "model", k.model, "le", formatFloat(bound)), h.buckets[i])
		}
		// +Inf bucket always equals the total count.
		fmt.Fprintf(b, "codvian_router_request_latency_ms_bucket%s %d\n",
			formatLabels("provider", k.provider, "model", k.model, "le", "+Inf"), h.count)
		fmt.Fprintf(b, "codvian_router_request_latency_ms_sum%s %v\n",
			formatLabels("provider", k.provider, "model", k.model), h.sum)
		fmt.Fprintf(b, "codvian_router_request_latency_ms_count%s %d\n",
			formatLabels("provider", k.provider, "model", k.model), h.count)
	}
}

// writeUsageCounter renders usage series in deterministic order.
// micro=true converts the stored micro-USD integer back to a decimal
// USD value; tokens (micro=false) are emitted as exact integers.
func writeUsageCounter(b *strings.Builder, name, label string, m map[usageKey]uint64, micro bool) {
	keys := make([]usageKey, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		if keys[i].model != keys[j].model {
			return keys[i].model < keys[j].model
		}
		return keys[i].kind < keys[j].kind
	})
	for _, k := range keys {
		if micro {
			fmt.Fprintf(b, "%s%s %v\n", name,
				formatLabels("provider", k.provider, "model", k.model, label, k.kind),
				float64(m[k])/1e6)
		} else {
			fmt.Fprintf(b, "%s%s %d\n", name,
				formatLabels("provider", k.provider, "model", k.model, label, k.kind), m[k])
		}
	}
}

func writeStringCounter(b *strings.Builder, name, label string, m map[string]uint64) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s%s %d\n", name, formatLabels(label, k), m[k])
	}
}

// ---------------------------------------------------------------------------
// Snapshot (dashboard live feeds).
//
// Snapshot is the in-memory roll-up the dashboard's Overview and Usage
// sections read: totals since this process started (the recorder keeps
// no per-request timestamps, so it cannot answer "last 24 hours" —
// the ledger answers range-limited questions; see internal/ledger).
// Latency p50/p95/p99 are read off the recorded histogram buckets, so
// they are real measured percentiles, not estimates. All counts are
// zero until real traffic arrives: an empty snapshot is the honest
// "no requests yet" state, never a placeholder.
// ---------------------------------------------------------------------------

// ProviderSnapshot is one provider's request totals since start.
type ProviderSnapshot struct {
	Provider string  `json:"provider"`
	Requests uint64  `json:"requests"`
	OK       uint64  `json:"ok"`
	Errors   uint64  `json:"errors"`
	MeanMs   float64 `json:"mean_latency_ms"`
}

// Snapshot is the recorder's totals since process start.
type Snapshot struct {
	Requests  uint64 `json:"requests"`
	OK        uint64 `json:"ok"`
	Errors    uint64 `json:"errors"`
	Failovers uint64 `json:"failovers"`
	InFlight  int64  `json:"in_flight"`
	Coalesced uint64 `json:"coalesced"`
	CacheHits uint64 `json:"cache_hits"`
	CacheMiss uint64 `json:"cache_misses"`
	// CacheHitsByLayer / CacheMissesByLayer are the same gateway
	// cache counters split by layer ("exact" / "semantic"), so the
	// dashboard can show the split honestly instead of a single
	// total. Copies: mutating them cannot corrupt the recorder.
	// Empty (nil) before any cache lookup has been recorded.
	CacheHitsByLayer   map[string]uint64 `json:"cache_hits_by_layer,omitempty"`
	CacheMissesByLayer map[string]uint64 `json:"cache_misses_by_layer,omitempty"`
	// SemanticHitsByVectorSource is the same semantic-hit counter
	// split by vector space ("neural" / "bag-of-words"), and
	// CacheStoreRefusedByReason the store-poison refusals by reason.
	// Empty (nil) until the first event, like the layer maps above.
	SemanticHitsByVectorSource map[string]uint64 `json:"semantic_hits_by_vector_source,omitempty"`
	CacheStoreRefusedByReason  map[string]uint64 `json:"cache_store_refused_by_reason,omitempty"`
	// Panics is the total recovered panics since process start,
	// summed over every location (see Panics). Zero until a panic
	// actually happens — the dashboard renders it as an honest
	// zero, never as an invented baseline.
	Panics     uint64  `json:"panics"`
	LatencyN   uint64  `json:"latency_samples"`
	LatencyAvg float64 `json:"latency_mean_ms"`
	LatencyP50 float64 `json:"latency_p50_ms"`
	LatencyP95 float64 `json:"latency_p95_ms"`
	// LatencyP99 is the 99th-percentile latency in milliseconds,
	// read off the same recorded histogram buckets as p50/p95
	// (G10 Area 12 row 11): 0 until a latency sample is recorded.
	LatencyP99 float64            `json:"latency_p99_ms"`
	ByProvider []ProviderSnapshot `json:"by_provider"`
}

// Snapshot returns the recorder's totals since process start. The
// returned maps are copies; mutating them cannot corrupt the recorder.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	snap := Snapshot{InFlight: r.inFlight, Coalesced: r.coalesced}
	byProv := map[string]*ProviderSnapshot{}
	latByProv := map[string]*histogram{}
	for k, n := range r.requests {
		snap.Requests += n
		switch k.outcome {
		case "ok":
			snap.OK += n
		case "error", "timeout":
			snap.Errors += n
		}
		p := byProv[k.provider]
		if p == nil {
			p = &ProviderSnapshot{Provider: k.provider}
			byProv[k.provider] = p
		}
		p.Requests += n
		if k.outcome == "ok" {
			p.OK += n
		} else {
			p.Errors += n
		}
	}
	for _, n := range r.failovers {
		snap.Failovers += n
	}
	for _, n := range r.cacheHits {
		snap.CacheHits += n
	}
	for _, n := range r.cacheMisses {
		snap.CacheMiss += n
	}
	if len(r.cacheHits) > 0 {
		snap.CacheHitsByLayer = make(map[string]uint64, len(r.cacheHits))
		for k, n := range r.cacheHits {
			snap.CacheHitsByLayer[k] = n
		}
	}
	if len(r.cacheMisses) > 0 {
		snap.CacheMissesByLayer = make(map[string]uint64, len(r.cacheMisses))
		for k, n := range r.cacheMisses {
			snap.CacheMissesByLayer[k] = n
		}
	}
	if len(r.semanticVectorSource) > 0 {
		snap.SemanticHitsByVectorSource = make(map[string]uint64, len(r.semanticVectorSource))
		for k, n := range r.semanticVectorSource {
			snap.SemanticHitsByVectorSource[k] = n
		}
	}
	if len(r.cacheStoreRefused) > 0 {
		snap.CacheStoreRefusedByReason = make(map[string]uint64, len(r.cacheStoreRefused))
		for k, n := range r.cacheStoreRefused {
			snap.CacheStoreRefusedByReason[k] = n
		}
	}
	for _, n := range r.panics {
		snap.Panics += n
	}
	// Latency: merge every per-(provider,model) histogram into one
	// overall distribution (for p50/p95/p99) and a per-provider mean.
	merged := &histogram{buckets: make([]uint64, len(LatencyBuckets))}
	for k, h := range r.latency {
		if h == nil {
			continue
		}
		for i, c := range h.buckets {
			merged.buckets[i] += c
		}
		merged.sum += h.sum
		merged.count += h.count
		p := byProv[k.provider]
		if p == nil {
			p = &ProviderSnapshot{Provider: k.provider}
			byProv[k.provider] = p
		}
		acc := latByProv[k.provider]
		if acc == nil {
			acc = &histogram{buckets: make([]uint64, len(LatencyBuckets))}
			latByProv[k.provider] = acc
		}
		acc.sum += h.sum
		acc.count += h.count
	}
	snap.LatencyN = merged.count
	if merged.count > 0 {
		snap.LatencyAvg = merged.sum / float64(merged.count)
		snap.LatencyP50 = percentileFromBuckets(merged, 0.50)
		snap.LatencyP95 = percentileFromBuckets(merged, 0.95)
		snap.LatencyP99 = percentileFromBuckets(merged, 0.99)
	}
	for _, p := range byProv {
		if h := latByProv[p.Provider]; h != nil && h.count > 0 {
			p.MeanMs = h.sum / float64(h.count)
		}
		snap.ByProvider = append(snap.ByProvider, *p)
	}
	sort.Slice(snap.ByProvider, func(i, j int) bool { return snap.ByProvider[i].Provider < snap.ByProvider[j].Provider })
	if snap.ByProvider == nil {
		snap.ByProvider = []ProviderSnapshot{}
	}
	return snap
}

// percentileFromBuckets reads a percentile off a cumulative histogram:
// the upper bound of the first bucket whose (already cumulative)
// count reaches q * total. The +Inf bucket guarantees a result
// whenever count > 0.
func percentileFromBuckets(h *histogram, q float64) float64 {
	if h == nil || h.count == 0 {
		return 0
	}
	need := uint64(float64(h.count)*q + 0.5)
	if need == 0 {
		need = 1
	}
	for i, cum := range h.buckets {
		if cum >= need {
			return LatencyBuckets[i]
		}
	}
	// All samples sit above the last finite bucket: report the last
	// bucket bound (the honest lower bound of the percentile).
	return LatencyBuckets[len(LatencyBuckets)-1]
}

// Handler returns an http.Handler serving this recorder's metrics in
// Prometheus text format at whatever path it is mounted on. It responds
// 200 with Content-Type "text/plain; version=0.0.4; charset=utf-8".
// It never logs keys or message content.
func (r *Recorder) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b strings.Builder
		r.WriteTo(&b)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	})
}
