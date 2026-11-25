// AUD-PERF-003 plumbing: the native relay's target must carry the warm
// pool's key fingerprint so Registry.PooledTransport returns the SAME
// transport the engine clients use for that (provider, key) pair.
package provider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRelayTargetCarriesPoolKeyFingerprint(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "anth", Name: "Anthropic", BaseURL: "https://api.anthropic.com", APIKeyEncrypted: "sk-ant-test", WireFormat: WireFormatAnthropic, Models: []Model{{ID: "claude-sonnet-4-5"}}, Enabled: true, CatalogID: "anthropic"},
	})
	tgt, err := reg.RelayTargetForModel("claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("RelayTargetForModel: %v", err)
	}
	if want := KeyFingerprint("sk-ant-test"); tgt.KeyFingerprint != want {
		t.Errorf("RelayTarget.KeyFingerprint = %q, want %q (must be the pool key, never the key itself)", tgt.KeyFingerprint, want)
	}
	if tgt.KeyFingerprint == "sk-ant-test" {
		t.Fatal("RelayTarget leaked key material")
	}
	// PooledTransport must hand back the engine's own pool transport
	// for the same (provider, key) pair — the same pointer — so a
	// relayed request reuses the engine's idle connections.
	enginePool := reg.pools.TransportFor("anth", "sk-ant-test")
	relayPool := reg.PooledTransport(tgt.ProviderID, tgt.KeyFingerprint)
	if relayPool != http.RoundTripper(enginePool) {
		t.Errorf("Registry.PooledTransport did not return the engine's warm pool transport for (%s, %s)", tgt.ProviderID, tgt.KeyFingerprint)
	}
	// A fingerprint from a DIFFERENT key must resolve to a different pool.
	other := reg.PooledTransport("anth", KeyFingerprint("sk-ant-other"))
	if other == relayPool {
		t.Error("two distinct key fingerprints resolved to the same pool")
	}
	// No provider id, no pool.
	if reg.PooledTransport("", "x") != nil {
		t.Error("PooledTransport(\"\", …) = non-nil, want nil")
	}
}

// BenchmarkPoolTransportConcurrentDialReuse reports how many TCP dials
// the pooled transport needs per request under concurrency: a warm pool
// keeps idle connections, so dials/op stays well below 1 across a burst.
// Contrast the old behaviour (http.DefaultTransport, MaxIdleConnsPerHost
// =2), which re-dials for every request beyond the two idle.
func BenchmarkPoolTransportConcurrentDialReuse(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	mgr := NewPoolManager(PoolConfig{}, false)
	client := &http.Client{Transport: mgr.TransportFor("bench", "k")}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(srv.URL)
			if err != nil {
				b.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	})
	if stats := mgr.Stats(); len(stats) == 1 {
		b.ReportMetric(float64(stats[0].Dials)/float64(b.N), "dials/op")
	}
}
