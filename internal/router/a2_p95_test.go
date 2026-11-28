// A2-6: ProviderStats.LatencyP95Ms is the TRUE histogram p95 (>= 20 samples),
// not the p50 EWMA, and the p50 fallback is bounded (p95 unavailable below 20
// samples).
package router

import "testing"

func TestProviderStatsP95FromHistogram(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	// 18 samples at 100 ms and 2 at 5000 ms: the histogram p95 (the 19th of
	// 20 values) is 5000 ms, while the p50 EWMA stays well under 3000 ms.
	for i := 0; i < 18; i++ {
		eng.noteAttemptStats("p1", "", true, 100)
	}
	for i := 0; i < 2; i++ {
		eng.noteAttemptStats("p1", "", true, 5000)
	}
	st, ok := eng.ProviderStats("p1")
	if !ok {
		t.Fatal("ProviderStats(p1) = no stats, want 20 recorded attempts")
	}
	if st.LatencyP95Ms != 5000 {
		t.Fatalf("LatencyP95Ms = %v, want 5000 (the histogram p95)", st.LatencyP95Ms)
	}
	if st.LatencyMs >= 3000 {
		t.Fatalf("p50 EWMA = %v, want it far below the p95 (proving the two signals differ)", st.LatencyMs)
	}
}

func TestProviderStatsP95UnavailableBelow20Samples(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	for i := 0; i < 10; i++ {
		eng.noteAttemptStats("p1", "", true, 100)
	}
	st, ok := eng.ProviderStats("p1")
	if !ok {
		t.Fatal("ProviderStats(p1) = no stats, want 10 recorded attempts")
	}
	if st.LatencyP95Ms != -1 {
		t.Fatalf("LatencyP95Ms = %v with 10 samples, want -1 (p95 unavailable; callers fall back to the p50 EWMA %v)", st.LatencyP95Ms, st.LatencyMs)
	}
}
