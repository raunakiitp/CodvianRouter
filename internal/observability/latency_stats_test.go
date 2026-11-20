package observability

import (
	"testing"
)

// G10 Area 12 row 9: LatencyStats is the per-(provider, model)
// p50/p95/p99 source for GET /api/telemetry/model-latency-stats.
// Nearest-rank percentiles, hand-computed: samples 1..100 ms give
// p50 = 50, p95 = 95, p99 = 99.
func TestLatencyStatsPerKeyP50P95P99(t *testing.T) {
	c := NewCollector()
	for i := 1; i <= 100; i++ {
		c.Record(Observation{Provider: "prov-a", Model: "m1", LatencyMs: float64(i), Outcome: "ok"})
	}
	for i := 1; i <= 10; i++ {
		c.Record(Observation{Provider: "prov-b", Model: "m2", LatencyMs: float64(i * 10), Outcome: "ok"})
	}
	stats := c.LatencyStats()
	if len(stats) != 2 {
		t.Fatalf("LatencyStats() returned %d rows, want 2", len(stats))
	}
	// Sorted by provider, then model.
	a := stats[0]
	if a.Provider != "prov-a" || a.Model != "m1" || a.Samples != 100 {
		t.Fatalf("first row = %+v, want prov-a/m1 with 100 samples", a)
	}
	if a.P50Ms != 50 || a.P95Ms != 95 || a.P99Ms != 99 {
		t.Errorf("prov-a/m1 percentiles = %v/%v/%v, want 50/95/99", a.P50Ms, a.P95Ms, a.P99Ms)
	}
	b := stats[1]
	if b.Provider != "prov-b" || b.Model != "m2" || b.Samples != 10 {
		t.Fatalf("second row = %+v, want prov-b/m2 with 10 samples", b)
	}
	// 10 samples at 10..100 ms: p50 rank 5 -> 50, p95 rank 10 -> 100,
	// p99 rank 10 -> 100.
	if b.P50Ms != 50 || b.P95Ms != 100 || b.P99Ms != 100 {
		t.Errorf("prov-b/m2 percentiles = %v/%v/%v, want 50/100/100", b.P50Ms, b.P95Ms, b.P99Ms)
	}
}

func TestLatencyStatsEmptyWithNoSamples(t *testing.T) {
	if got := NewCollector().LatencyStats(); len(got) != 0 {
		t.Errorf("LatencyStats() on a fresh collector = %v, want empty", got)
	}
	if got := (*Collector)(nil).LatencyStats(); got != nil {
		t.Errorf("LatencyStats() on nil collector = %v, want nil", got)
	}
}
