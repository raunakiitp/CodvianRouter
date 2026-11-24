// AUD-PERF-003 regression: the verbatim /v1/messages relay must send its
// upstream request over the SAME warm pooled transport the engine uses
// for a (provider, key) pair, not a bare http.Client{Timeout: …} that
// falls back to http.DefaultTransport (MaxIdleConnsPerHost=2). The
// observable defect is connection churn: a bursty relay to one provider
// re-dials TCP after the two idle connections DefaultTransport keeps.
//
// The test drives two sequential bursts of concurrent relay requests
// against a real HTTP backend and counts the backend's accepted TCP
// connections. A warm pool keeps the burst's connections idle and the
// second burst reuses them; DefaultTransport keeps only two and the
// second burst re-dials the rest. The assertion is a deterministic
// connection count, not a timing measurement.
package server

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// relayReuseRegistry builds a real registry with one Anthropic-wire
// provider whose base URL is baseURL (the test backend), so the native
// relay resolves it verbatim and dials the backend. The API key is
// sealed through the registry cipher so RelayTargetForModel can decrypt
// it and fingerprint it for the warm pool.
func relayReuseRegistry(t *testing.T, baseURL string) *provider.Registry {
	t.Helper()
	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("aud-perf-003 relay reuse secret"), dir)
	if err != nil {
		t.Fatalf("vault: %v", err)
	}
	cipher := provider.NewVaultCipher(vault)
	sealed, err := cipher.Encrypt("sk-ant-reuse-test")
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	cfgs := []provider.ProviderConfig{{
		ID:              "reuseprov",
		Name:            "Reuse",
		BaseURL:         baseURL,
		CatalogID:       "anthropic",
		WireFormat:      provider.WireFormatAnthropic,
		APIKeyEncrypted: sealed,
		Models:          []provider.Model{{ID: "claude-reuse"}},
		Enabled:         true,
	}}
	raw, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(path), provider.WithCipher(cipher))
	if err := reg.Load(); err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

func TestRelayVerbatimReusesWarmPoolConnections(t *testing.T) {
	const burst = 8

	var mu sync.Mutex
	newConns := 0
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicJSONReply))
	}))
	backend.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			mu.Lock()
			newConns++
			mu.Unlock()
		}
	}
	backend.Start()
	defer backend.Close()

	reg := relayReuseRegistry(t, backend.URL)
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg))

	body := `{"model":"claude-reuse","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	fireBurst := func() {
		var wg sync.WaitGroup
		for i := 0; i < burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := doReq(t, srv, http.MethodPost, "/v1/messages", body, "test-key-123")
				if rec.Code != http.StatusOK {
					t.Errorf("relay status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
				}
				if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
					t.Errorf("X-Codvian-Relay = %q, want verbatim (relay must not translate)", got)
				}
			}()
		}
		wg.Wait()
	}

	fireBurst()
	// Let the transport return the burst's connections to the idle pool
	// before the second burst; otherwise the second burst could dial
	// even with the fix and the count would not isolate reuse.
	time.Sleep(200 * time.Millisecond)
	fireBurst()

	mu.Lock()
	got := newConns
	mu.Unlock()

	// With the warm pool (default size 20 ≥ burst) the second burst
	// reuses the first burst's idle connections: ~burst total dials.
	// With http.DefaultTransport (MaxIdleConnsPerHost=2) the second
	// burst re-dials burst-2 of them: ~2*burst-2 total. The threshold
	// sits between the two.
	want := burst + 1
	if got > want {
		t.Errorf("backend accepted %d new TCP connections across two bursts of %d relayed requests, want <= %d: the verbatim relay is not reusing the warm pool transport (AUD-PERF-003)", got, burst, want)
	}
}
