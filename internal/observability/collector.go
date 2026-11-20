// Package observability provides the M2.3/M2.4 telemetry surface for Codvian
// Router: a Collector that aggregates per-request observations into
// per-provider/model latency percentiles (p50/p95/p99), error classes,
// fallback counts, cache hit ratio, tokens, cost and breaker state, and
// exposes them in Prometheus text format; a structured JSON request logger;
// and an OpenTelemetry-compatible tracer that is a no-op unless the binary
// is built with -tags otel.
//
// Every number the Collector reports comes from observations fed to Record
// (or breaker states fed to SetBreakerState) by the real request path.
// Nothing is invented: a fresh Collector reports zeros, an empty latency
// key reports 0/0/0, and the cache hit ratio is 0 until a cache lookup has
// actually been recorded. Wiring Record/SetBreakerState into the request
// path is the coordinator's call-site step; this package only aggregates
// what it is given.
package observability

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// maxSamplesPerKey bounds memory: each provider+model key keeps at most
// this many recent latency samples (ring overwrite of the oldest).
const maxSamplesPerKey = 4096

// Observation is one finished request as measured by the real request path.
// All fields are measurements, never estimates: LatencyMs is the measured
// wall latency, CostUSD the ledger-computed cost, Tokens* the counted
// tokens. Zero values mean "not measured / not applicable", never a guess.
type Observation struct {
	Provider string
	Model    string
	// LatencyMs is the measured end-to-end latency in milliseconds.
	LatencyMs float64
	// Outcome is "ok", "error" or "timeout".
	Outcome string
	// ErrorClass classifies a non-ok outcome, e.g. "rate_limited",
	// "timeout", "upstream_5xx", "auth". Empty on errors counts as "unknown".
	ErrorClass string
	// FallbackAttempts is how many fallback hops this request took (0 = none).
	FallbackAttempts int
	// CacheHit reports whether a cache layer served this request. It is
	// only meaningful together with CacheLayer: a lookup is counted only
	// when CacheLayer is non-empty, so requests that never consulted the
	// cache do not pollute the hit ratio.
	CacheHit     bool
	CacheLayer   string // "exact" / "semantic" / ...
	TokensIn     int64
	TokensOut    int64
	TokensCached int64
	// CostUSD is the ledger-computed cost of this request in USD.
	CostUSD float64
}

// BreakerState is one provider's circuit-breaker state as reported by
// internal/router Engine.BreakerStates. Fed via SetBreakerState; the
// Collector never invents breaker state.
type BreakerState struct {
	Provider            string
	Open                bool
	ConsecutiveFailures int
	Threshold           int
}

type latencyKey struct {
	provider string
	model    string
}

type errorKey struct {
	provider string
	model    string
	class    string
}

// Snapshot is a point-in-time copy of the Collector's totals. Maps are
// copies; mutating them cannot corrupt the Collector.
type Snapshot struct {
	Requests           uint64            `json:"requests"`
	OK                 uint64            `json:"ok"`
	Errors             uint64            `json:"errors"`
	ErrorsByClass      map[string]uint64 `json:"errors_by_class,omitempty"`
	Fallbacks          uint64            `json:"fallbacks"`
	FallbackAttempts   uint64            `json:"fallback_attempts"`
	CacheHits          uint64            `json:"cache_hits"`
	CacheMisses        uint64            `json:"cache_misses"`
	CacheHitsByLayer   map[string]uint64 `json:"cache_hits_by_layer,omitempty"`
	CacheMissesByLayer map[string]uint64 `json:"cache_misses_by_layer,omitempty"`
	CacheHitRatio      float64           `json:"cache_hit_ratio"`
	TokensIn           int64             `json:"tokens_in"`
	TokensOut          int64             `json:"tokens_out"`
	TokensCached       int64             `json:"tokens_cached"`
	CostUSD            float64           `json:"cost_usd"`
	LatencyP50         float64           `json:"latency_p50_ms"`
	LatencyP95         float64           `json:"latency_p95_ms"`
	LatencyP99         float64           `json:"latency_p99_ms"`
	LatencySamples     uint64            `json:"latency_samples"`
	Breakers           []BreakerState    `json:"breakers,omitempty"`
}

// Collector aggregates Observations. The zero value is usable, but prefer
// NewCollector. All methods are safe for concurrent use.
type Collector struct {
	mu sync.Mutex

	requests      map[latencyKey]uint64
	ok            map[latencyKey]uint64
	errors        map[latencyKey]uint64
	errorsByClass map[string]uint64
	// errorsByKeyClass counts errors per (provider, model, class) so the
	// Prometheus exposition can split error classes by provider/model
	// from recorded state, without inventing a split.
	errorsByKeyClass map[errorKey]uint64
	fallbacks        map[latencyKey]uint64
	fallbackAtt      map[latencyKey]uint64
	latency          map[latencyKey][]float64 // ring buffer, len <= maxSamplesPerKey
	latencyIdx       map[latencyKey]int
	latencyAll       []float64 // global samples, ring
	latencyAllIdx    int

	cacheHits    map[string]uint64
	cacheMisses  map[string]uint64
	tokensIn     int64
	tokensOut    int64
	tokensCached int64
	costUSD      float64
	breakers     map[string]BreakerState
}

// NewCollector returns an initialized Collector.
func NewCollector() *Collector {
	return &Collector{
		requests:         map[latencyKey]uint64{},
		ok:               map[latencyKey]uint64{},
		errors:           map[latencyKey]uint64{},
		errorsByClass:    map[string]uint64{},
		errorsByKeyClass: map[errorKey]uint64{},
		fallbacks:        map[latencyKey]uint64{},
		fallbackAtt:      map[latencyKey]uint64{},
		latency:          map[latencyKey][]float64{},
		latencyIdx:       map[latencyKey]int{},
		cacheHits:        map[string]uint64{},
		cacheMisses:      map[string]uint64{},
		breakers:         map[string]BreakerState{},
	}
}

// Record feeds one measured observation into the Collector.
func (c *Collector) Record(o Observation) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := latencyKey{provider: o.Provider, model: o.Model}
	c.requests[key]++
	switch o.Outcome {
	case "ok":
		c.ok[key]++
	default:
		c.errors[key]++
		class := o.ErrorClass
		if class == "" {
			class = "unknown"
		}
		c.errorsByClass[class]++
		c.errorsByKeyClass[errorKey{provider: o.Provider, model: o.Model, class: class}]++
	}
	if o.FallbackAttempts > 0 {
		c.fallbacks[key]++
		c.fallbackAtt[key] += uint64(o.FallbackAttempts)
	}
	if o.CacheLayer != "" {
		if o.CacheHit {
			c.cacheHits[o.CacheLayer]++
		} else {
			c.cacheMisses[o.CacheLayer]++
		}
	}
	c.tokensIn += o.TokensIn
	c.tokensOut += o.TokensOut
	c.tokensCached += o.TokensCached
	c.costUSD += o.CostUSD
	// Latency sample (ring overwrite once the per-key cap is reached).
	samples := c.latency[key]
	if len(samples) < maxSamplesPerKey {
		c.latency[key] = append(samples, o.LatencyMs)
	} else {
		idx := c.latencyIdx[key]
		samples[idx] = o.LatencyMs
		c.latencyIdx[key] = (idx + 1) % maxSamplesPerKey
	}
	if len(c.latencyAll) < maxSamplesPerKey {
		c.latencyAll = append(c.latencyAll, o.LatencyMs)
	} else {
		c.latencyAll[c.latencyAllIdx] = o.LatencyMs
		c.latencyAllIdx = (c.latencyAllIdx + 1) % maxSamplesPerKey
	}
}

// SetBreakerState records one provider's breaker state as reported by the
// router. Re-setting the same provider replaces its previous state.
func (c *Collector) SetBreakerState(st BreakerState) {
	if c == nil || st.Provider == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.breakers[st.Provider] = st
}

// BreakerStates returns the recorded breaker states in provider order.
func (c *Collector) BreakerStates() []BreakerState {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]BreakerState, 0, len(c.breakers))
	for _, st := range c.breakers {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// percentile computes the nearest-rank percentile of sorted samples.
// q is 0..1. Empty input returns 0 — an honest "no data", never a guess.
func percentile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(q * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func sortedCopy(samples []float64) []float64 {
	out := make([]float64, len(samples))
	copy(out, samples)
	sort.Float64s(out)
	return out
}

// Latency returns p50/p95/p99 (ms) and the sample count for one
// provider+model key. p99 is the gap this package closes vs
// internal/metrics, which only exposes p50/p95.
func (c *Collector) Latency(provider, model string) (p50, p95, p99 float64, n uint64) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	samples := sortedCopy(c.latency[latencyKey{provider, model}])
	return percentile(samples, 0.5), percentile(samples, 0.95), percentile(samples, 0.99), uint64(len(samples))
}

// ModelLatencyStat is one (provider, model) key's measured latency
// percentiles, computed from the samples actually recorded for that
// key (nearest-rank over the sorted samples — the same rule as
// Latency). A key with no samples never appears here, so an empty
// result is the honest "no latency data yet" state.
type ModelLatencyStat struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	P50Ms    float64 `json:"p50_ms"`
	P95Ms    float64 `json:"p95_ms"`
	P99Ms    float64 `json:"p99_ms"`
	Samples  uint64  `json:"samples"`
}

// LatencyStats returns the per-(provider, model) latency percentiles
// for every key with at least one recorded sample, in provider then
// model order. It is the data source for GET
// /api/telemetry/model-latency-stats (internal/server
// latency_stats.go, G10 Area 12 row 9): every p50/p95/p99 here
// comes from real recorded samples, never from an estimate.
func (c *Collector) LatencyStats() []ModelLatencyStat {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]latencyKey, 0, len(c.latency))
	for k, samples := range c.latency {
		if len(samples) == 0 {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})
	out := make([]ModelLatencyStat, 0, len(keys))
	for _, k := range keys {
		samples := sortedCopy(c.latency[k])
		out = append(out, ModelLatencyStat{
			Provider: k.provider,
			Model:    k.model,
			P50Ms:    percentile(samples, 0.5),
			P95Ms:    percentile(samples, 0.95),
			P99Ms:    percentile(samples, 0.99),
			Samples:  uint64(len(samples)),
		})
	}
	return out
}

// CacheHitRatio returns cache hits / (hits + misses) over actually recorded
// lookups. Zero when no lookup has been recorded.
func (c *Collector) CacheHitRatio() float64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var hits, misses uint64
	for _, n := range c.cacheHits {
		hits += n
	}
	for _, n := range c.cacheMisses {
		misses += n
	}
	if hits+misses == 0 {
		return 0
	}
	return float64(hits) / float64(hits+misses)
}

// Snapshot returns a copy of the Collector's totals.
func (c *Collector) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{ErrorsByClass: map[string]uint64{}}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := Snapshot{
		ErrorsByClass:      map[string]uint64{},
		CacheHitsByLayer:   map[string]uint64{},
		CacheMissesByLayer: map[string]uint64{},
		TokensIn:           c.tokensIn,
		TokensOut:          c.tokensOut,
		TokensCached:       c.tokensCached,
		CostUSD:            c.costUSD,
	}
	for _, n := range c.requests {
		snap.Requests += n
	}
	for _, n := range c.ok {
		snap.OK += n
	}
	for _, n := range c.errors {
		snap.Errors += n
	}
	for class, n := range c.errorsByClass {
		snap.ErrorsByClass[class] = n
	}
	for _, n := range c.fallbacks {
		snap.Fallbacks += n
	}
	for _, n := range c.fallbackAtt {
		snap.FallbackAttempts += n
	}
	var hits, misses uint64
	for layer, n := range c.cacheHits {
		snap.CacheHitsByLayer[layer] = n
		hits += n
	}
	for layer, n := range c.cacheMisses {
		snap.CacheMissesByLayer[layer] = n
		misses += n
	}
	snap.CacheHits = hits
	snap.CacheMisses = misses
	if hits+misses > 0 {
		snap.CacheHitRatio = float64(hits) / float64(hits+misses)
	}
	all := sortedCopy(c.latencyAll)
	snap.LatencyP50 = percentile(all, 0.5)
	snap.LatencyP95 = percentile(all, 0.95)
	snap.LatencyP99 = percentile(all, 0.99)
	snap.LatencySamples = uint64(len(all))
	for _, st := range c.breakers {
		snap.Breakers = append(snap.Breakers, st)
	}
	sort.Slice(snap.Breakers, func(i, j int) bool { return snap.Breakers[i].Provider < snap.Breakers[j].Provider })
	return snap
}

// --- Prometheus exposition -------------------------------------------------

func escapeLabelValue(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(s)
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// formatCost renders a USD total rounded to 10 decimal places (the same
// precision as the X-Codvian-Cost-USD response header). Summing many small
// float64 costs otherwise prints artefacts like 0.009999999999999995.
func formatCost(f float64) string {
	return strconv.FormatFloat(math.Round(f*1e10)/1e10, 'g', -1, 64)
}

func formatLabels(pairs ...string) string {
	var sb strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(pairs[i])
		sb.WriteString(`="`)
		sb.WriteString(escapeLabelValue(pairs[i+1]))
		sb.WriteByte('"')
	}
	return sb.String()
}

// WritePrometheus writes the Collector's current state in Prometheus text
// exposition format. Every series value is computed from recorded state.
func (c *Collector) WritePrometheus(w io.Writer) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder

	keys := make([]latencyKey, 0, len(c.requests))
	for k := range c.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].provider != keys[j].provider {
			return keys[i].provider < keys[j].provider
		}
		return keys[i].model < keys[j].model
	})

	b.WriteString("# HELP codvian_router_requests_total Requests by provider, model and outcome.\n")
	b.WriteString("# TYPE codvian_router_requests_total counter\n")
	for _, k := range keys {
		for _, outcome := range []string{"ok", "error"} {
			var n uint64
			if outcome == "ok" {
				n = c.ok[k]
			} else {
				n = c.errors[k]
			}
			fmt.Fprintf(&b, "codvian_router_requests_total{%s} %d\n",
				formatLabels("provider", k.provider, "model", k.model, "outcome", outcome), n)
		}
	}

	// Error classes, split by provider+model from recorded state.
	b.WriteString("# HELP codvian_router_errors_total Errors by provider, model and error class.\n")
	b.WriteString("# TYPE codvian_router_errors_total counter\n")
	errKeys := make([]errorKey, 0, len(c.errorsByKeyClass))
	for k := range c.errorsByKeyClass {
		errKeys = append(errKeys, k)
	}
	sort.Slice(errKeys, func(i, j int) bool {
		if errKeys[i].provider != errKeys[j].provider {
			return errKeys[i].provider < errKeys[j].provider
		}
		if errKeys[i].model != errKeys[j].model {
			return errKeys[i].model < errKeys[j].model
		}
		return errKeys[i].class < errKeys[j].class
	})
	for _, k := range errKeys {
		fmt.Fprintf(&b, "codvian_router_errors_total{%s} %d\n",
			formatLabels("provider", k.provider, "model", k.model, "error_class", k.class), c.errorsByKeyClass[k])
	}

	b.WriteString("# HELP codvian_router_request_latency_ms Request latency in milliseconds.\n")
	b.WriteString("# TYPE codvian_router_request_latency_ms summary\n")
	for _, k := range keys {
		samples := sortedCopy(c.latency[k])
		if len(samples) == 0 {
			continue
		}
		var sum float64
		for _, v := range samples {
			sum += v
		}
		labels := formatLabels("provider", k.provider, "model", k.model)
		fmt.Fprintf(&b, "codvian_router_request_latency_ms{%s,quantile=\"0.5\"} %s\n", labels, formatFloat(percentile(samples, 0.5)))
		fmt.Fprintf(&b, "codvian_router_request_latency_ms{%s,quantile=\"0.95\"} %s\n", labels, formatFloat(percentile(samples, 0.95)))
		fmt.Fprintf(&b, "codvian_router_request_latency_ms{%s,quantile=\"0.99\"} %s\n", labels, formatFloat(percentile(samples, 0.99)))
		fmt.Fprintf(&b, "codvian_router_request_latency_ms_count{%s} %d\n", labels, len(samples))
		fmt.Fprintf(&b, "codvian_router_request_latency_ms_sum{%s} %s\n", labels, formatFloat(sum))
	}

	b.WriteString("# HELP codvian_router_fallbacks_total Requests that took at least one fallback hop.\n")
	b.WriteString("# TYPE codvian_router_fallbacks_total counter\n")
	b.WriteString("# HELP codvian_router_fallback_attempts_total Total fallback hops taken.\n")
	b.WriteString("# TYPE codvian_router_fallback_attempts_total counter\n")
	for _, k := range keys {
		labels := formatLabels("provider", k.provider, "model", k.model)
		fmt.Fprintf(&b, "codvian_router_fallbacks_total{%s} %d\n", labels, c.fallbacks[k])
		fmt.Fprintf(&b, "codvian_router_fallback_attempts_total{%s} %d\n", labels, c.fallbackAtt[k])
	}

	b.WriteString("# HELP codvian_router_cache_hits_total Cache hits by layer.\n")
	b.WriteString("# TYPE codvian_router_cache_hits_total counter\n")
	b.WriteString("# HELP codvian_router_cache_misses_total Cache misses by layer.\n")
	b.WriteString("# TYPE codvian_router_cache_misses_total counter\n")
	b.WriteString("# HELP codvian_router_cache_hit_ratio Cache hits / (hits + misses).\n")
	b.WriteString("# TYPE codvian_router_cache_hit_ratio gauge\n")
	layers := map[string]bool{}
	for l := range c.cacheHits {
		layers[l] = true
	}
	for l := range c.cacheMisses {
		layers[l] = true
	}
	sortedLayers := make([]string, 0, len(layers))
	for l := range layers {
		sortedLayers = append(sortedLayers, l)
	}
	sort.Strings(sortedLayers)
	var hits, misses uint64
	for _, l := range sortedLayers {
		fmt.Fprintf(&b, "codvian_router_cache_hits_total{%s} %d\n", formatLabels("layer", l), c.cacheHits[l])
		fmt.Fprintf(&b, "codvian_router_cache_misses_total{%s} %d\n", formatLabels("layer", l), c.cacheMisses[l])
		hits += c.cacheHits[l]
		misses += c.cacheMisses[l]
	}
	ratio := 0.0
	if hits+misses > 0 {
		ratio = float64(hits) / float64(hits+misses)
	}
	fmt.Fprintf(&b, "codvian_router_cache_hit_ratio %s\n", formatFloat(ratio))

	b.WriteString("# HELP codvian_router_tokens_total Tokens by type.\n")
	b.WriteString("# TYPE codvian_router_tokens_total counter\n")
	fmt.Fprintf(&b, "codvian_router_tokens_total{%s} %d\n", formatLabels("type", "in"), c.tokensIn)
	fmt.Fprintf(&b, "codvian_router_tokens_total{%s} %d\n", formatLabels("type", "out"), c.tokensOut)
	fmt.Fprintf(&b, "codvian_router_tokens_total{%s} %d\n", formatLabels("type", "cached"), c.tokensCached)

	b.WriteString("# HELP codvian_router_cost_usd_total Total ledger-computed cost in USD.\n")
	b.WriteString("# TYPE codvian_router_cost_usd_total counter\n")
	fmt.Fprintf(&b, "codvian_router_cost_usd_total %s\n", formatCost(c.costUSD))

	b.WriteString("# HELP codvian_router_breaker_open 1 when the provider circuit breaker is open.\n")
	b.WriteString("# TYPE codvian_router_breaker_open gauge\n")
	b.WriteString("# HELP codvian_router_breaker_consecutive_failures Consecutive failures counted by the breaker.\n")
	b.WriteString("# TYPE codvian_router_breaker_consecutive_failures gauge\n")
	provs := make([]string, 0, len(c.breakers))
	for p := range c.breakers {
		provs = append(provs, p)
	}
	sort.Strings(provs)
	for _, p := range provs {
		st := c.breakers[p]
		open := 0
		if st.Open {
			open = 1
		}
		fmt.Fprintf(&b, "codvian_router_breaker_open{%s} %d\n", formatLabels("provider", p), open)
		fmt.Fprintf(&b, "codvian_router_breaker_consecutive_failures{%s} %d\n", formatLabels("provider", p), st.ConsecutiveFailures)
	}

	_, _ = io.WriteString(w, b.String())
}

// Handler returns an http.Handler serving this Collector's Prometheus
// exposition at whatever path it is mounted on. A nil Collector answers
// 503 (metrics not enabled), mirroring internal/server handleMetrics.
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if c == nil {
			http.Error(w, "metrics not enabled", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		c.WritePrometheus(w)
	})
}

// RegisterMetrics mounts the Collector's Prometheus handler at /metrics on
// the given mux. It performs no auth itself: the caller must mount it
// behind the gateway-key guard (see the coordinator wiring note — in
// internal/server this means registering through the guarded route table,
// the same Tier 3 auth that guards the existing /metrics endpoint).
func RegisterMetrics(mux *http.ServeMux, c *Collector) {
	if mux == nil {
		return
	}
	mux.Handle("/metrics", c.Handler())
}
