package metrics

import (
	"testing"
)

// G10 Area 12 row 11: Snapshot must report p99 alongside p50/p95,
// computed from real recorded samples (histogram buckets
// 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000 ms;
// percentileFromBuckets returns the upper bound of the first bucket
// whose cumulative count reaches the nearest-rank need).
//
// Distribution, hand-computed: 100 samples, 98 at 10 ms, 2 at
// 5000 ms. Cumulative counts: <=10 ms bucket holds 98, <=5000 ms
// holds 100.
//   - p50: need = ceil(0.50*100) = 50 -> first bucket >= 50 is
//     10 ms.
//   - p95: need = ceil(0.95*100) = 95 -> first bucket >= 95 is
//     10 ms.
//   - p99: need = ceil(0.99*100) = 99 -> 10 ms bucket holds only
//     98 < 99, so the first bucket >= 99 is 5000 ms.
func TestSnapshotReportsP99FromRealSamples(t *testing.T) {
	r := NewRecorder()
	for i := 0; i < 98; i++ {
		r.ObserveLatency("prov-a", "m", 10)
	}
	for i := 0; i < 2; i++ {
		r.ObserveLatency("prov-a", "m", 5000)
	}
	snap := r.Snapshot()
	if snap.LatencyN != 100 {
		t.Fatalf("LatencyN = %d, want 100", snap.LatencyN)
	}
	if snap.LatencyP50 != 10 {
		t.Errorf("LatencyP50 = %v, want 10", snap.LatencyP50)
	}
	if snap.LatencyP95 != 10 {
		t.Errorf("LatencyP95 = %v, want 10", snap.LatencyP95)
	}
	if snap.LatencyP99 != 5000 {
		t.Errorf("LatencyP99 = %v, want 5000 (99th sample sits in the 5000ms bucket)", snap.LatencyP99)
	}
}

func TestSnapshotP99ZeroWithNoSamples(t *testing.T) {
	snap := NewRecorder().Snapshot()
	if snap.LatencyP50 != 0 || snap.LatencyP95 != 0 || snap.LatencyP99 != 0 {
		t.Errorf("empty snapshot percentiles = %v/%v/%v, want 0/0/0",
			snap.LatencyP50, snap.LatencyP95, snap.LatencyP99)
	}
}
