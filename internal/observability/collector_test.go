// Acceptance tests for M2.3 — Collector: per-provider/model p50/p95/p99,
// error classes, fallback counts, cache hit ratio, tokens, cost, breaker
// state, Prometheus exposition, /metrics handler registration.
// Written FIRST (failing) per the Wave 1 plan; implementation lands after.
package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCollectorPercentilesP50P95P99(t *testing.T) {
	c := NewCollector()
	// 100 samples, 1..100 ms: nearest-rank p50=50, p95=95, p99=99.
	for i := 1; i <= 100; i++ {
		c.Record(Observation{Provider: "openai", Model: "gpt-4o", LatencyMs: float64(i), Outcome: "ok"})
	}
	p50, p95, p99, n := c.Latency("openai", "gpt-4o")
	if n != 100 {
		t.Fatalf("n = %d, want 100", n)
	}
	if p50 != 50 || p95 != 95 || p99 != 99 {
		t.Fatalf("p50/p95/p99 = %v/%v/%v, want 50/95/99", p50, p95, p99)
	}
	// No samples: honest zeros, not invented numbers.
	if p50, p95, p99, n := c.Latency("nobody", "nothing"); n != 0 || p50 != 0 || p95 != 0 || p99 != 0 {
		t.Fatalf("empty key: got %v/%v/%v n=%d, want 0/0/0 n=0", p50, p95, p99, n)
	}
}

func TestCollectorErrorClassesAndFallbacks(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "error", ErrorClass: "rate_limited", LatencyMs: 12})
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "error", ErrorClass: "rate_limited", LatencyMs: 13})
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "error", ErrorClass: "timeout", LatencyMs: 5000})
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "ok", FallbackAttempts: 2, LatencyMs: 40})
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "ok", FallbackAttempts: 0, LatencyMs: 30})

	snap := c.Snapshot()
	if snap.Requests != 5 || snap.OK != 2 || snap.Errors != 3 {
		t.Fatalf("requests/ok/errors = %d/%d/%d, want 5/2/3", snap.Requests, snap.OK, snap.Errors)
	}
	if got := snap.ErrorsByClass["rate_limited"]; got != 2 {
		t.Fatalf("rate_limited errors = %d, want 2", got)
	}
	if got := snap.ErrorsByClass["timeout"]; got != 1 {
		t.Fatalf("timeout errors = %d, want 1", got)
	}
	if snap.Fallbacks != 1 || snap.FallbackAttempts != 2 {
		t.Fatalf("fallbacks/attempts = %d/%d, want 1/2", snap.Fallbacks, snap.FallbackAttempts)
	}
	// An error with no class is counted under "unknown", never dropped.
	c.Record(Observation{Provider: "anthropic", Model: "claude-sonnet", Outcome: "error", LatencyMs: 1})
	if got := c.Snapshot().ErrorsByClass["unknown"]; got != 1 {
		t.Fatalf("unknown errors = %d, want 1", got)
	}
}

func TestCollectorCacheHitRatio(t *testing.T) {
	c := NewCollector()
	if r := c.CacheHitRatio(); r != 0 {
		t.Fatalf("ratio with no lookups = %v, want 0", r)
	}
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", CacheHit: true, CacheLayer: "exact", LatencyMs: 1})
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", CacheHit: true, CacheLayer: "semantic", LatencyMs: 1})
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", CacheHit: false, CacheLayer: "exact", LatencyMs: 10})
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", CacheHit: false, CacheLayer: "exact", LatencyMs: 10})
	if r := c.CacheHitRatio(); r != 0.5 {
		t.Fatalf("cache hit ratio = %v, want 0.5", r)
	}
	snap := c.Snapshot()
	if snap.CacheHits != 2 || snap.CacheMisses != 2 {
		t.Fatalf("hits/misses = %d/%d, want 2/2", snap.CacheHits, snap.CacheMisses)
	}
	if snap.CacheHitsByLayer["exact"] != 1 || snap.CacheHitsByLayer["semantic"] != 1 {
		t.Fatalf("hits by layer = %v, want exact:1 semantic:1", snap.CacheHitsByLayer)
	}
}

func TestCollectorTokensAndCost(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", TokensIn: 100, TokensOut: 50, TokensCached: 20, CostUSD: 0.0015, LatencyMs: 10})
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", TokensIn: 200, TokensOut: 0, TokensCached: 0, CostUSD: 0.0025, LatencyMs: 20})
	snap := c.Snapshot()
	if snap.TokensIn != 300 || snap.TokensOut != 50 || snap.TokensCached != 20 {
		t.Fatalf("tokens in/out/cached = %d/%d/%d, want 300/50/20", snap.TokensIn, snap.TokensOut, snap.TokensCached)
	}
	if snap.CostUSD != 0.004 {
		t.Fatalf("cost = %v, want 0.004", snap.CostUSD)
	}
}

func TestCollectorBreakerState(t *testing.T) {
	c := NewCollector()
	if len(c.BreakerStates()) != 0 {
		t.Fatalf("fresh collector has breaker states, want none")
	}
	c.SetBreakerState(BreakerState{Provider: "openai", Open: true, ConsecutiveFailures: 5, Threshold: 5})
	c.SetBreakerState(BreakerState{Provider: "anthropic", Open: false, ConsecutiveFailures: 0, Threshold: 5})
	states := c.BreakerStates()
	if len(states) != 2 {
		t.Fatalf("breaker states = %d, want 2", len(states))
	}
	byProv := map[string]BreakerState{}
	for _, s := range states {
		byProv[s.Provider] = s
	}
	if !byProv["openai"].Open || byProv["openai"].ConsecutiveFailures != 5 {
		t.Fatalf("openai breaker = %+v, want open with 5 failures", byProv["openai"])
	}
	if byProv["anthropic"].Open {
		t.Fatalf("anthropic breaker open, want closed")
	}
	// Updating the same provider replaces, never duplicates.
	c.SetBreakerState(BreakerState{Provider: "openai", Open: false, ConsecutiveFailures: 0, Threshold: 5})
	if len(c.BreakerStates()) != 2 || c.BreakerStates()[0].Provider == "" {
		t.Fatalf("breaker update duplicated or lost state: %v", c.BreakerStates())
	}
}

func TestPrometheusExpositionReflectsMeasuredState(t *testing.T) {
	c := NewCollector()
	for i := 1; i <= 100; i++ {
		c.Record(Observation{Provider: "openai", Model: "gpt-4o", LatencyMs: float64(i), Outcome: "ok", TokensIn: 10, TokensOut: 5, CostUSD: 0.0001})
	}
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "error", ErrorClass: "timeout", LatencyMs: 50})
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", CacheHit: true, CacheLayer: "exact", FallbackAttempts: 1, LatencyMs: 2})
	c.SetBreakerState(BreakerState{Provider: "openai", Open: true, ConsecutiveFailures: 3, Threshold: 5})

	var sb strings.Builder
	c.WritePrometheus(&sb)
	out := sb.String()
	// p99 must be present and equal to the measured value (99), not a guess.
	if !strings.Contains(out, `codvian_router_request_latency_ms{provider="openai",model="gpt-4o",quantile="0.99"} 99`) {
		t.Fatalf("exposition lacks measured p99=99:\n%s", out)
	}
	if !strings.Contains(out, `quantile="0.5"} 50`) || !strings.Contains(out, `quantile="0.95"} 95`) {
		t.Fatalf("exposition lacks measured p50/p95:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_requests_total{provider="openai",model="gpt-4o",outcome="ok"} 101`) {
		t.Fatalf("exposition lacks ok request count:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_errors_total{provider="openai",model="gpt-4o",error_class="timeout"} 1`) {
		t.Fatalf("exposition lacks timeout error count:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_fallbacks_total{provider="openai",model="gpt-4o"} 1`) {
		t.Fatalf("exposition lacks fallback count:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_cache_hit_ratio 1`) { // 1 hit / 1 lookup
		t.Fatalf("exposition lacks cache hit ratio 1:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_breaker_open{provider="openai"} 1`) {
		t.Fatalf("exposition lacks open breaker gauge:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_cost_usd_total 0.01`) {
		t.Fatalf("exposition lacks total cost 0.01:\n%s", out)
	}
}

func TestRegisterMetricsServesPrometheus(t *testing.T) {
	c := NewCollector()
	c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", LatencyMs: 7})
	mux := http.NewServeMux()
	RegisterMetrics(mux, c)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus text format", ct)
	}
	if !strings.Contains(rec.Body.String(), "codvian_router_requests_total") {
		t.Fatalf("body lacks codvian_router_requests_total:\n%s", rec.Body.String())
	}
	// A nil collector must not panic the handler: it answers 503, like the
	// existing server /metrics does with a nil recorder.
	mux2 := http.NewServeMux()
	RegisterMetrics(mux2, nil)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil collector GET /metrics status = %d, want 503", rec2.Code)
	}
}

func TestCollectorConcurrentRecord(t *testing.T) {
	c := NewCollector()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 250; i++ {
				c.Record(Observation{Provider: "openai", Model: "gpt-4o", Outcome: "ok", LatencyMs: float64(i%100 + 1), TokensIn: 1, CostUSD: 0.00001})
				c.SetBreakerState(BreakerState{Provider: "openai", Open: g%2 == 0})
			}
		}(g)
	}
	wg.Wait()
	if got := c.Snapshot().Requests; got != 2000 {
		t.Fatalf("concurrent requests = %d, want 2000", got)
	}
}
