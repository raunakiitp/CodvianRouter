package health

import (
	"testing"
	"time"
)

// G10 Area 12 row 11: History must report P99Ms alongside P50Ms /
// P95Ms, computed from real recorded probe samples (nearest-rank
// over the sorted successful latencies in the last 24 h).
//
// Distribution, hand-computed: 100 successful probes, latencies
// 1..100 ms. Sorted rank for q is ceil(q * 100):
//   - p50 -> rank 50 -> 50 ms
//   - p95 -> rank 95 -> 95 ms
//   - p99 -> rank 99 -> 99 ms
func TestHistoryReportsP99(t *testing.T) {
	c := New(nil, nil)
	now := time.Now().UTC()
	for i := 1; i <= 100; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: int64(i), CheckedAt: now.Add(-time.Duration(i) * time.Minute)}, nil)
	}
	sum := c.History("p1")
	if sum.Samples != 100 {
		t.Fatalf("Samples = %d, want 100", sum.Samples)
	}
	if sum.P50Ms == nil || *sum.P50Ms != 50 {
		t.Errorf("P50Ms = %v, want 50", sum.P50Ms)
	}
	if sum.P95Ms == nil || *sum.P95Ms != 95 {
		t.Errorf("P95Ms = %v, want 95", sum.P95Ms)
	}
	if sum.P99Ms == nil || *sum.P99Ms != 99 {
		t.Errorf("P99Ms = %v, want 99", sum.P99Ms)
	}
}

func TestHistoryP99NilWithNoSuccessfulProbes(t *testing.T) {
	c := New(nil, nil)
	c.record(Status{ProviderID: "p1", OK: false, LatencyMs: 10, CheckedAt: time.Now().UTC()}, nil)
	sum := c.History("p1")
	if sum.P50Ms != nil || sum.P95Ms != nil || sum.P99Ms != nil {
		t.Errorf("percentiles with no successful probe = %v/%v/%v, want nil/nil/nil",
			sum.P50Ms, sum.P95Ms, sum.P99Ms)
	}
}
