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

type histogram struct {
	buckets []uint64 // one per LatencyBuckets entry, cumulative counts
	sum     float64
	count   uint64
}

// Recorder is a concurrency-safe in-memory store for router metrics.
// The zero value is usable, but prefer NewRecorder.
type Recorder struct {
	mu            sync.Mutex
	requests      map[requestKey]uint64
	failovers     map[failoverKey]uint64
	cacheHits     map[string]uint64
	cacheMisses   map[string]uint64
	budgetRefused uint64
	inFlight      int64
	latency       map[latencyKey]*histogram
}

// NewRecorder returns an initialized Recorder.
func NewRecorder() *Recorder {
	return &Recorder{
		requests:    make(map[requestKey]uint64),
		failovers:   make(map[failoverKey]uint64),
		cacheHits:   make(map[string]uint64),
		cacheMisses: make(map[string]uint64),
		latency:     make(map[latencyKey]*histogram),
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

// IncBudgetRefused increments codvian_router_budget_refused_total.
func (r *Recorder) IncBudgetRefused() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.budgetRefused++
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

	// Budget refused.
	b.WriteString("# HELP codvian_router_budget_refused_total Total requests refused by the budget guard.\n")
	b.WriteString("# TYPE codvian_router_budget_refused_total counter\n")
	fmt.Fprintf(b, "codvian_router_budget_refused_total %d\n", r.budgetRefused)

	// In-flight gauge.
	b.WriteString("# HELP codvian_router_in_flight Current in-flight requests.\n")
	b.WriteString("# TYPE codvian_router_in_flight gauge\n")
	fmt.Fprintf(b, "codvian_router_in_flight %d\n", r.inFlight)

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
