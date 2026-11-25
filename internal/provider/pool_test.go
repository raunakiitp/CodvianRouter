package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPoolSizeForRate pins the Little's Law derivation: N =
// ceil(λ × W), clamped to [min, max]. The defaults (20 req/s ×
// 1000 ms) give 20 — the default pool size.
func TestPoolSizeForRate(t *testing.T) {
	cases := []struct {
		name     string
		rps      float64
		lat      time.Duration
		min, max int
		want     int
	}{
		{"defaults 20rps x 1000ms", 20, time.Second, 2, 200, 20},
		{"exact", 10, 500 * time.Millisecond, 2, 200, 5},
		{"rounds up", 10, 501 * time.Millisecond, 2, 200, 6},
		{"floor", 1, 10 * time.Millisecond, 2, 200, 2},
		{"ceiling", 500, 2 * time.Second, 2, 200, 200},
		{"zero rate gets floor", 0, time.Second, 2, 200, 2},
		{"zero latency gets floor", 20, 0, 2, 200, 2},
		{"negative rate gets floor", -5, time.Second, 2, 200, 2},
		{"min above max clamps to min", 20, time.Second, 50, 10, 50},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PoolSizeForRate(tc.rps, tc.lat, tc.min, tc.max); got != tc.want {
				t.Errorf("PoolSizeForRate(%v, %v, %d, %d) = %d, want %d", tc.rps, tc.lat, tc.min, tc.max, got, tc.want)
			}
		})
	}
}

func TestPoolConfigResolvedAndSize(t *testing.T) {
	def := PoolConfig{}.Resolved()
	if def.ExpectedRPS != DefaultPoolExpectedRPS || def.ExpectedLatencyMs != DefaultPoolExpectedLatencyMs ||
		def.MinConns != DefaultPoolMinConns || def.MaxConns != DefaultPoolMaxConns {
		t.Errorf("zero PoolConfig resolved to %+v, want defaults 20/1000/2/200", def)
	}
	if got := (PoolConfig{}).Size(); got != 20 {
		t.Errorf("default PoolConfig.Size() = %d, want 20 (ceil(20 x 1.0s))", got)
	}
	// Explicit override wins over the derivation, still clamped.
	if got := (PoolConfig{MaxIdlePerHost: 7}).Size(); got != 7 {
		t.Errorf("override 7: Size() = %d, want 7", got)
	}
	if got := (PoolConfig{MaxIdlePerHost: 5000}).Size(); got != DefaultPoolMaxConns {
		t.Errorf("override 5000: Size() = %d, want ceiling %d", got, DefaultPoolMaxConns)
	}
	if got := (PoolConfig{MaxIdlePerHost: 1}).Size(); got != DefaultPoolMinConns {
		t.Errorf("override 1: Size() = %d, want floor %d", got, DefaultPoolMinConns)
	}
	// Derivation from custom inputs: 50 req/s x 400 ms = 20.
	if got := (PoolConfig{ExpectedRPS: 50, ExpectedLatencyMs: 400}).Size(); got != 20 {
		t.Errorf("50rps x 400ms: Size() = %d, want 20", got)
	}
}

func TestPoolConfigValidate(t *testing.T) {
	if err := (PoolConfig{}).Validate(); err != nil {
		t.Errorf("zero config must validate: %v", err)
	}
	bad := []PoolConfig{
		{ExpectedRPS: -1},
		{ExpectedLatencyMs: -100},
		{MaxIdlePerHost: -1},
		{MaxIdlePerHost: 1001},
		{MinConns: -1},
		{MaxConns: -1},
		{MinConns: 10, MaxConns: 5},
	}
	for i, cfg := range bad {
		if err := cfg.Validate(); err == nil {
			t.Errorf("config %d (%+v) must fail validation", i, cfg)
		}
	}
}

func TestKeyFingerprint(t *testing.T) {
	fp := KeyFingerprint("sk-test-key-123")
	if len(fp) != 12 {
		t.Errorf("fingerprint %q has length %d, want 12", fp, len(fp))
	}
	if strings.Contains(fp, "sk-test-key-123") {
		t.Error("fingerprint contains key material")
	}
	if fp2 := KeyFingerprint("sk-test-key-123"); fp2 != fp {
		t.Error("fingerprint is not deterministic")
	}
	if fp3 := KeyFingerprint("sk-different-key"); fp3 == fp {
		t.Error("different keys must fingerprint differently")
	}
}

func TestPoolManagerSamePairSharesTransport(t *testing.T) {
	m := NewPoolManager(PoolConfig{}, true)
	a := m.TransportFor("prov-a", "key-one")
	b := m.TransportFor("prov-a", "key-one")
	if a != b {
		t.Error("same (provider, key) must return the same transport")
	}
	if got := m.PoolCount(); got != 1 {
		t.Errorf("PoolCount = %d, want 1", got)
	}
	// A different key gets its own pool; a different provider with
	// the same key also gets its own pool (different host).
	if c := m.TransportFor("prov-a", "key-two"); c == a {
		t.Error("different key must get a different transport")
	}
	if d := m.TransportFor("prov-b", "key-one"); d == a {
		t.Error("different provider must get a different transport")
	}
	if got := m.PoolCount(); got != 3 {
		t.Errorf("PoolCount = %d, want 3", got)
	}
	// Stats carry the fingerprint, never the key.
	for _, s := range m.Stats() {
		if strings.Contains(s.KeyFingerprint, "key-one") || strings.Contains(s.KeyFingerprint, "key-two") {
			t.Errorf("stat fingerprint %q contains key material", s.KeyFingerprint)
		}
		if s.KeyFingerprint != KeyFingerprint("key-one") && s.KeyFingerprint != KeyFingerprint("key-two") {
			t.Errorf("stat fingerprint %q is not a fingerprint of either key", s.KeyFingerprint)
		}
	}
}

func TestPoolManagerDropClosesIdle(t *testing.T) {
	m := NewPoolManager(PoolConfig{}, true)
	m.TransportFor("prov-a", "key-one")
	m.TransportFor("prov-b", "key-one")
	m.Drop("prov-a")
	if got := m.PoolCount(); got != 1 {
		t.Errorf("after Drop(prov-a) PoolCount = %d, want 1", got)
	}
	if stats := m.Stats(); len(stats) != 1 || stats[0].ProviderID != "prov-b" {
		t.Errorf("remaining pool is not prov-b: %+v", stats)
	}
}

// countingListener wraps a net.Listener and counts Accept calls:
// every accept is one client dial (+ one TLS handshake) arriving at
// the mock — the independent cross-check for the transport's own
// dial counter.
type countingListener struct {
	net.Listener
	mu      sync.Mutex
	accepts int
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.accepts++
		l.mu.Unlock()
	}
	return c, err
}

func (l *countingListener) Accepted() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepts
}

// tlsMock is a TLS upstream answering OpenAI-wire chat + models
// with a fixed delay. By default it speaks HTTP/1.1 over TLS
// (EnableHTTP2 false): one request per connection, which is the
// case where the idle-pool size decides whether the next burst
// re-dials. (Under HTTP/2 multiplexing a few connections carry any
// burst, so the pool size cannot matter there — the size is derived
// from Little's Law for the general case: HTTP/1.1 upstreams,
// stream-capped HTTP/2 upstreams, and connection churn. See
// docs/pool-sizing.md for the honest caveat.)
type tlsMock struct {
	srv   *httptest.Server
	ln    *countingListener
	mu    sync.Mutex
	proto string
}

func newTLSMock(t testing.TB, delay time.Duration) *tlsMock {
	t.Helper()
	m := &tlsMock{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		m.mu.Lock()
		m.proto = r.Proto
		m.mu.Unlock()
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"mock-model","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-mock","object":"chat.completion","model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"mock says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m.ln = &countingListener{Listener: ln}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = m.ln
	// EnableHTTP2 stays false: this mock is HTTPS + HTTP/1.1 on
	// purpose (see tlsMock). newH2Mock below is the HTTP/2 twin.
	srv.StartTLS()
	t.Cleanup(srv.Close)
	m.srv = srv
	return m
}

// newHTTPMock is the plain-HTTP twin of newTLSMock, for registry
// tests that only need connection reuse (not a TLS handshake): the
// registry's default factory builds its own transport, which would
// not trust the httptest self-signed certificate.
func newHTTPMock(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"mock-model","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-mock","object":"chat.completion","model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"mock says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newH2Mock is the HTTP/2 twin of newTLSMock: the same handlers,
// but EnableHTTP2 set before StartTLS so the server offers "h2"
// via ALPN. It proves the pooled transport really negotiates
// HTTP/2 (not just HTTP/1.1 over TLS) and reuses one connection.
func newH2Mock(t testing.TB, delay time.Duration) *tlsMock {
	t.Helper()
	m := &tlsMock{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		m.mu.Lock()
		m.proto = r.Proto
		m.mu.Unlock()
	}
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"mock-model","object":"model"}]}`)
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-mock","object":"chat.completion","model":"mock-model","choices":[{"index":0,"message":{"role":"assistant","content":"mock says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	m.ln = &countingListener{Listener: ln}
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = m.ln
	// EnableHTTP2 must be set before StartTLS: otherwise the
	// server's TLS config offers only "http/1.1" and no client can
	// negotiate HTTP/2 against it.
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	m.srv = srv
	return m
}

func (m *tlsMock) Proto() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.proto
}

// burst runs n concurrent Chat requests through client and returns
// the number that failed.
func burst(t testing.TB, client Client, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	failed := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := client.Chat(context.Background(), ChatRequest{
				Model:    "mock-model",
				Messages: []Message{{Role: "user", Content: fmt.Sprintf("burst request %d", i)}},
			})
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	return failed
}

// TestWarmPoolAvoidsRedials is the proof for the Little's Law sizing:
// two bursts of 10 concurrent requests (50 ms each, so all 10 are
// in flight at once) against the same TLS mock, which speaks
// HTTPS + HTTP/1.1 — one connection per concurrent request, so the
// idle pool is the only thing standing between burst 2 and a fresh
// dial.
//
//   - With a pool sized by the formula (16 idle: 10 concurrent
//     requests need 10), burst 2 reuses the warm connections: the
//     mock accepts NO new TCP connection and the transport counts
//     NO new dial.
//   - With Go's default MaxIdleConnsPerHost = 2, burst 1 leaves
//     only 2 idle connections behind, so burst 2 must re-dial for
//     the other 8: the mock accepts new connections and the
//     transport counts new dials.
//
// The dial count is the assertion; the mock's accept count is the
// independent cross-check (a dial the transport counts must arrive
// as an accept at the mock). The latency lines are logged, not
// asserted: on localhost a TLS handshake is ~1 ms, so the wall
// difference is small here — the dial count is the proof, and on a
// real network each avoided dial saves 2+ RTTs.
//
// The HTTP/2 half of the claim is proven separately by
// TestWarmPoolSpeaksHTTP2 (same transport negotiates h2 and reuses
// one connection); the multiplexing caveat — under h2 with generous
// stream limits a small pool suffices for concurrency — is
// documented in docs/pool-sizing.md, not hidden.
func TestWarmPoolAvoidsRedials(t *testing.T) {
	const n = 10
	const delay = 50 * time.Millisecond

	t.Run("sized pool", func(t *testing.T) {
		mock := newTLSMock(t, delay)
		mgr := NewPoolManager(PoolConfig{MaxIdlePerHost: 16}, true)
		pooled := mgr.TransportFor("mock", "key-one")
		// Test-only: trust the httptest self-signed certificate.
		pooled.Transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // test-only: trust the httptest cert; NextProtos keeps the automatic HTTP/2 ALPN a nil TLS config would get
		client, err := NewClient(mock.srv.URL, "key-one", WithHTTPClient(&http.Client{Transport: pooled}), WithAllowPrivateDestinations(true))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if failed := burst(t, client, n); failed != 0 {
			t.Fatalf("burst 1: %d/%d requests failed", failed, n)
		}
		if got := mock.Proto(); got != "HTTP/1.1" {
			t.Errorf("upstream saw %q, want HTTP/1.1 (this mock is the HTTP/1.1 proof; HTTP/2 is proven by TestWarmPoolSpeaksHTTP2)", got)
		}
		dialsAfterBurst1 := pooled.dials.Load()
		acceptsAfterBurst1 := mock.ln.Accepted()
		start := time.Now()
		if failed := burst(t, client, n); failed != 0 {
			t.Fatalf("burst 2: %d/%d requests failed", failed, n)
		}
		burst2Dur := time.Since(start)
		if extra := pooled.dials.Load() - dialsAfterBurst1; extra != 0 {
			t.Errorf("sized pool: burst 2 dialled %d new connection(s), want 0 (all 10 reused)", extra)
		}
		if extra := mock.ln.Accepted() - acceptsAfterBurst1; extra != 0 {
			t.Errorf("sized pool: mock accepted %d new connection(s) in burst 2, want 0", extra)
		}
		st := mgr.Stats()
		if len(st) != 1 {
			t.Fatalf("Stats = %d pools, want 1", len(st))
		}
		if st[0].Requests != 2*n {
			t.Errorf("Requests = %d, want %d", st[0].Requests, 2*n)
		}
		if st[0].Reused < n {
			t.Errorf("Reused = %d, want >= %d (burst 2 fully reused)", st[0].Reused, n)
		}
		if !st[0].Warmed || st[0].Idle == 0 {
			t.Errorf("pool not warm after bursts: %+v", st[0])
		}
		t.Logf("sized pool: burst 2 took %s for 10x50ms requests (no re-dial); dials total %d, reused %d, idle %d",
			burst2Dur, st[0].Dials, st[0].Reused, st[0].Idle)
	})

	t.Run("default MaxIdleConnsPerHost 2", func(t *testing.T) {
		mock := newTLSMock(t, delay)
		mgr := NewPoolManager(PoolConfig{MaxIdlePerHost: 2}, true)
		pooled := mgr.TransportFor("mock", "key-one")
		// Test-only: trust the httptest self-signed certificate.
		pooled.Transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // test-only: trust the httptest cert; NextProtos keeps the automatic HTTP/2 ALPN a nil TLS config would get
		client, err := NewClient(mock.srv.URL, "key-one", WithHTTPClient(&http.Client{Transport: pooled}), WithAllowPrivateDestinations(true))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		if failed := burst(t, client, n); failed != 0 {
			t.Fatalf("burst 1: %d/%d requests failed", failed, n)
		}
		dialsAfterBurst1 := pooled.dials.Load()
		acceptsAfterBurst1 := mock.ln.Accepted()
		start := time.Now()
		if failed := burst(t, client, n); failed != 0 {
			t.Fatalf("burst 2: %d/%d requests failed", failed, n)
		}
		burst2Dur := time.Since(start)
		if extra := pooled.dials.Load() - dialsAfterBurst1; extra < 5 {
			t.Errorf("pool of 2: burst 2 dialled only %d new connection(s), want >= 5 (only 2 idle survive burst 1)", extra)
		}
		if extra := mock.ln.Accepted() - acceptsAfterBurst1; extra < 5 {
			t.Errorf("pool of 2: mock accepted only %d new connection(s) in burst 2, want >= 5", extra)
		}
		t.Logf("pool of 2: burst 2 took %s; burst-2 dials %d, mock accepts %d (the re-dial cost the sized pool avoids)",
			burst2Dur, pooled.dials.Load()-dialsAfterBurst1, mock.ln.Accepted()-acceptsAfterBurst1)
	})
}

// TestWarmPoolSpeaksHTTP2 proves the pooled transport really
// negotiates HTTP/2 (not just HTTP/1.1 over TLS) and reuses one
// connection for sequential requests: 5 sequential chats dial
// exactly once.
func TestWarmPoolSpeaksHTTP2(t *testing.T) {
	mock := newH2Mock(t, 5*time.Millisecond)
	mgr := NewPoolManager(PoolConfig{MaxIdlePerHost: 16}, true)
	pooled := mgr.TransportFor("mock", "key-one")
	// Test-only: trust the httptest self-signed certificate.
	pooled.Transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // test-only: trust the httptest cert; NextProtos keeps the automatic HTTP/2 ALPN a nil TLS config would get
	client, err := NewClient(mock.srv.URL, "key-one", WithHTTPClient(&http.Client{Transport: pooled}), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := client.Chat(context.Background(), ChatRequest{Model: "mock-model", Messages: []Message{{Role: "user", Content: fmt.Sprintf("h2 request %d", i)}}}); err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	if got := mock.Proto(); got != "HTTP/2.0" {
		t.Errorf("upstream saw %q, want HTTP/2.0", got)
	}
	st := mgr.Stats()
	if len(st) != 1 {
		t.Fatalf("Stats = %d pools, want 1", len(st))
	}
	if st[0].Dials != 1 {
		t.Errorf("5 sequential HTTP/2 chats dialled %d time(s), want 1 (one warm connection reused)", st[0].Dials)
	}
	if st[0].Requests != 5 || st[0].Reused != 4 {
		t.Errorf("Requests=%d Reused=%d, want 5/4", st[0].Requests, st[0].Reused)
	}
	if !st[0].Warmed {
		t.Errorf("pool not warm after 5 chats: %+v", st[0])
	}
}

// TestRegistryClientForSharesPool proves the registry path: two
// ClientFor calls for one provider share one pooled transport — a
// request through the second client reuses the first client's warm
// connection (dials stay at 1 for a sequential second request).
func TestRegistryClientForSharesPool(t *testing.T) {
	srv := newHTTPMock(t, 5*time.Millisecond)
	reg := NewRegistry(nil, WithPoolConfig(PoolConfig{MaxIdlePerHost: 16}), WithRegistryAllowPrivateDestinations(true))
	p, err := reg.Add(srv.URL, "pool-test-key")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	c1, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor 1: %v", err)
	}
	c2, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor 2: %v", err)
	}
	if _, err := c1.Chat(context.Background(), ChatRequest{Model: "mock-model", Messages: []Message{{Role: "user", Content: "one"}}}); err != nil {
		t.Fatalf("chat 1: %v", err)
	}
	stats := reg.PoolStats()
	if len(stats) != 1 {
		t.Fatalf("PoolStats = %d pools, want 1", len(stats))
	}
	if stats[0].Dials != 1 {
		t.Fatalf("after chat 1: Dials = %d, want 1", stats[0].Dials)
	}
	if _, err := c2.Chat(context.Background(), ChatRequest{Model: "mock-model", Messages: []Message{{Role: "user", Content: "two"}}}); err != nil {
		t.Fatalf("chat 2: %v", err)
	}
	stats = reg.PoolStats()
	if stats[0].Dials != 1 {
		t.Errorf("after chat 2 through a second client: Dials = %d, want 1 (shared warm pool)", stats[0].Dials)
	}
	if stats[0].Requests != 2 || stats[0].Reused != 1 {
		t.Errorf("after 2 chats: Requests=%d Reused=%d, want 2/1", stats[0].Requests, stats[0].Reused)
	}
	// Removing the provider drops the pool.
	if err := reg.Remove(p.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := len(reg.PoolStats()); got != 0 {
		t.Errorf("after Remove: %d pools remain, want 0", got)
	}
}

// TestWarmPools warms a registry's pools with a real TestKey per
// provider and reports failures honestly (none here — the mock
// answers).
func TestWarmPools(t *testing.T) {
	srv := newHTTPMock(t, 5*time.Millisecond)
	reg := NewRegistry(nil, WithRegistryAllowPrivateDestinations(true))
	if _, err := reg.Add(srv.URL, "warm-key"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	warmed, failures := reg.WarmPools(context.Background())
	if warmed != 1 {
		t.Errorf("Warmed = %d, want 1", warmed)
	}
	if len(failures) != 0 {
		t.Errorf("failures = %v, want none", failures)
	}
	stats := reg.PoolStats()
	if len(stats) != 1 || stats[0].Dials != 1 || !stats[0].Warmed {
		t.Errorf("after WarmPools: stats = %+v, want 1 warm pool with 1 dial", stats)
	}
	// A second warm is a no-op dial-wise: the pool is already warm.
	warmed, failures = reg.WarmPools(context.Background())
	if warmed != 1 || len(failures) != 0 {
		t.Errorf("second WarmPools: warmed=%d failures=%v, want 1/none", warmed, failures)
	}
	if stats := reg.PoolStats(); stats[0].Dials != 1 {
		t.Errorf("second WarmPools re-dialled: Dials = %d, want 1", stats[0].Dials)
	}
}

// BenchmarkWarmPoolBurst measures a 10-request burst over the sized
// warm pool (50 ms upstream delay): the burst should take ~50 ms
// (one delay, all concurrent, no re-dial) once warm. Dials are
// reported per burst so a reuse regression shows up as dials, not
// just time.
func BenchmarkWarmPoolBurst(b *testing.B) {
	mock := newTLSMock(b, 50*time.Millisecond)
	mgr := NewPoolManager(PoolConfig{MaxIdlePerHost: 16}, true)
	pooled := mgr.TransportFor("mock", "key-one")
	// Test-only: trust the httptest self-signed certificate.
	pooled.Transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // test-only: trust the httptest cert; NextProtos keeps the automatic HTTP/2 ALPN a nil TLS config would get
	client, err := NewClient(mock.srv.URL, "key-one", WithHTTPClient(&http.Client{Transport: pooled}), WithAllowPrivateDestinations(true))
	if err != nil {
		b.Fatalf("NewClient: %v", err)
	}
	// Warm the pool once (outside the timed section).
	if failed := burst(b, client, 10); failed != 0 {
		b.Fatalf("warm-up burst: %d/10 failed", failed)
	}
	dialsBefore := pooled.dials.Load()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if failed := burst(b, client, 10); failed != 0 {
			b.Fatalf("burst: %d/10 failed", failed)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(pooled.dials.Load()-dialsBefore)/float64(b.N), "dials/burst")
}
