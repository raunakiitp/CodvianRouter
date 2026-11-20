package metrics

import "testing"

// TestSnapshot proves the dashboard Overview/Usage feeds read real
// measured totals: request counts by outcome, failovers, cache
// counters, per-provider breakdowns, and latency percentiles read
// off the recorded histogram buckets — with zero values (not
// placeholders) before any traffic arrives.
func TestSnapshotEmpty(t *testing.T) {
	snap := NewRecorder().Snapshot()
	if snap.Requests != 0 || snap.OK != 0 || snap.Errors != 0 {
		t.Errorf("empty snapshot counts = %+v, want all zero", snap)
	}
	if snap.LatencyP50 != 0 || snap.LatencyP95 != 0 || snap.LatencyAvg != 0 {
		t.Errorf("empty snapshot latency = p50 %v p95 %v avg %v, want 0", snap.LatencyP50, snap.LatencyP95, snap.LatencyAvg)
	}
	if len(snap.ByProvider) != 0 {
		t.Errorf("empty snapshot by_provider = %d rows, want 0", len(snap.ByProvider))
	}
}

func TestSnapshotTotalsAndPercentiles(t *testing.T) {
	r := NewRecorder()
	// 10 requests at 10 ms, 10 at 100 ms: p50 lands on the 10 ms
	// bucket bound, p95 on the 100 ms bucket bound.
	for i := 0; i < 10; i++ {
		r.IncRequest("openai", "gpt-4o", "ok")
		r.ObserveLatency("openai", "gpt-4o", 10)
	}
	for i := 0; i < 10; i++ {
		r.IncRequest("openai", "gpt-4o", "ok")
		r.ObserveLatency("openai", "gpt-4o", 100)
	}
	r.IncRequest("anthropic", "claude-3", "error")
	r.ObserveLatency("anthropic", "claude-3", 500)
	r.IncFailover("openai", "anthropic")
	r.IncCacheHit("exact")
	r.IncCacheHit("exact")
	r.IncCacheMiss("exact")
	r.IncCoalescedShared()

	snap := r.Snapshot()
	if snap.Requests != 21 || snap.OK != 20 || snap.Errors != 1 {
		t.Errorf("snapshot counts = %d/%d/%d, want 21/20/1", snap.Requests, snap.OK, snap.Errors)
	}
	if snap.Failovers != 1 {
		t.Errorf("failovers = %d, want 1", snap.Failovers)
	}
	if snap.CacheHits != 2 || snap.CacheMiss != 1 {
		t.Errorf("cache hits/misses = %d/%d, want 2/1", snap.CacheHits, snap.CacheMiss)
	}
	if snap.Coalesced != 1 {
		t.Errorf("coalesced = %d, want 1", snap.Coalesced)
	}
	if snap.LatencyN != 21 {
		t.Errorf("latency samples = %d, want 21", snap.LatencyN)
	}
	// Mean = (10*10 + 10*100 + 500) / 21 = 1600/21 ≈ 76.19.
	if snap.LatencyAvg < 76 || snap.LatencyAvg > 77 {
		t.Errorf("latency mean = %v, want ~76.19", snap.LatencyAvg)
	}
	// p50: 11th of 21 samples is a 100 ms sample → 100 ms bucket.
	if snap.LatencyP50 != 100 {
		t.Errorf("latency p50 = %v, want 100", snap.LatencyP50)
	}
	// p95: 20th of 21 samples is a 100 ms sample → 100 ms bucket.
	if snap.LatencyP95 != 100 {
		t.Errorf("latency p95 = %v, want 100", snap.LatencyP95)
	}
	if len(snap.ByProvider) != 2 {
		t.Fatalf("by_provider = %d rows, want 2", len(snap.ByProvider))
	}
	byProv := map[string]ProviderSnapshot{}
	for _, p := range snap.ByProvider {
		byProv[p.Provider] = p
	}
	openai := byProv["openai"]
	if openai.Requests != 20 || openai.OK != 20 || openai.Errors != 0 {
		t.Errorf("openai = %+v, want 20/20/0", openai)
	}
	if openai.MeanMs != 55 {
		t.Errorf("openai mean latency = %v, want 55", openai.MeanMs)
	}
	anthropic := byProv["anthropic"]
	if anthropic.Requests != 1 || anthropic.Errors != 1 || anthropic.MeanMs != 500 {
		t.Errorf("anthropic = %+v, want 1/1 error, mean 500", anthropic)
	}
}

// TestSnapshotCacheHitsByLayer (CHECKLIST 187-188): the snapshot
// splits the gateway cache counters by layer, so the dashboard
// Overview can show exact vs semantic hits honestly from live
// data. The maps are copies — mutating them must not corrupt the
// recorder — and they stay nil before any cache lookup.
func TestSnapshotCacheHitsByLayer(t *testing.T) {
	if snap := NewRecorder().Snapshot(); snap.CacheHitsByLayer != nil || snap.CacheMissesByLayer != nil {
		t.Fatalf("empty snapshot layer maps = %v/%v, want nil/nil", snap.CacheHitsByLayer, snap.CacheMissesByLayer)
	}
	r := NewRecorder()
	r.IncCacheHit("exact")
	r.IncCacheHit("exact")
	r.IncCacheHit("semantic")
	r.IncCacheMiss("semantic")
	snap := r.Snapshot()
	if snap.CacheHits != 3 || snap.CacheMiss != 1 {
		t.Fatalf("totals = %d/%d, want 3/1", snap.CacheHits, snap.CacheMiss)
	}
	if snap.CacheHitsByLayer["exact"] != 2 || snap.CacheHitsByLayer["semantic"] != 1 {
		t.Fatalf("hits by layer = %v, want map[exact:2 semantic:1]", snap.CacheHitsByLayer)
	}
	if snap.CacheMissesByLayer["semantic"] != 1 || len(snap.CacheMissesByLayer) != 1 {
		t.Fatalf("misses by layer = %v, want map[semantic:1]", snap.CacheMissesByLayer)
	}
	// Mutating the returned maps must not corrupt the recorder.
	snap.CacheHitsByLayer["exact"] = 999
	if snap2 := r.Snapshot(); snap2.CacheHitsByLayer["exact"] != 2 {
		t.Fatalf("recorder corrupted by snapshot mutation: exact = %d, want 2", snap2.CacheHitsByLayer["exact"])
	}
}
