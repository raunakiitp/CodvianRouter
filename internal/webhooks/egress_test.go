package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/egress"
)

// These tests drive the real delivery transport (egress.Guard.Client,
// the same constructor New(nil) uses) with deterministic DNS and a
// dialer that records the address it is asked to connect to and then
// connects to a local test server standing in for that IP. They prove
// what is actually dialled, not just what a validator returns.

type seqDNS struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func (f *seqDNS) lookup(_ context.Context, host string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	seq, ok := f.answers[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	i := f.calls[host]
	f.calls[host]++
	if i >= len(seq) {
		i = len(seq) - 1
	}
	var out []netip.Addr
	for _, s := range seq[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

type recDialer struct {
	mu     sync.Mutex
	dialed []string
	target string
}

func (d *recDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func (d *recDialer) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func guardedDispatcher(dns *seqDNS, d *recDialer, tlsCfg *tls.Config, url string, lb *logBuf) *Dispatcher {
	g := &egress.Guard{Lookup: dns.lookup, Dial: d.dial}
	return New([]Endpoint{{ID: "ep", URL: url, Events: []string{QuotaExceededEvent}, Secret: "s3cr3t"}},
		g.Client(0, 0, tlsCfg), log.New(lb, "", 0))
}

func onlyAddr(t *testing.T, got []string, want string) {
	t.Helper()
	for _, a := range got {
		if a != want {
			t.Fatalf("dialled %v, want only %s", got, want)
		}
	}
}

// TestDefaultClientRefusesLoopback: the production default (nil client)
// refuses a loopback receiver at delivery; it is never contacted and
// the refusal is terminal (no retries).
func TestDefaultClientRefusesLoopback(t *testing.T) {
	noRetryBackoff(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	lb := &logBuf{}
	d := New([]Endpoint{{ID: "lo", URL: srv.URL, Events: []string{QuotaExceededEvent}}}, nil, log.New(lb, "", 0))
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	if hits.Load() != 0 {
		t.Fatalf("loopback receiver got %d requests, want 0", hits.Load())
	}
	if !strings.Contains(lb.String(), "blocked by egress policy") || strings.Contains(lb.String(), "retrying") {
		t.Fatalf("log = %q, want one terminal 'blocked by egress policy' line", lb.String())
	}
}

// TestDefaultClientIgnoresProxyEnv: HTTP(S)_PROXY cannot route a
// delivery around the policy - the default client never uses a proxy.
func TestDefaultClientIgnoresProxyEnv(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { proxyHits.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	dns := &seqDNS{answers: map[string][][]string{"public.test": {{"93.184.215.14"}}}}
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer up.Close()
	rd := &recDialer{target: up.Listener.Addr().String()}
	d := guardedDispatcher(dns, rd, nil, "http://public.test/hook", &logBuf{})
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	if proxyHits.Load() != 0 {
		t.Fatalf("proxy got %d requests, want 0", proxyHits.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("receiver hits = %d, want 1 (direct, validated dial)", hits.Load())
	}
	onlyAddr(t, rd.addrs(), "93.184.215.14:80")
	if tr, ok := New(nil, nil, nil).client.Transport.(*http.Transport); !ok || tr.Proxy != nil {
		t.Fatal("default webhook transport has a Proxy function; it must be nil")
	}
}

// TestDeliveryDialsValidatedIP: a public IPv4 and IPv6 target is
// dialled as the validated IP literal, with the hostname kept in Host
// and the payload/HMAC unchanged.
func TestDeliveryDialsValidatedIP(t *testing.T) {
	body := []byte(`{"event":"quota.exceeded"}`)
	mac := hmac.New(sha256.New, []byte("s3cr3t"))
	mac.Write(body)
	wantSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	for _, tc := range []struct{ host, ip, dial string }{
		{"v4.test", "93.184.215.14", "93.184.215.14:80"},
		{"v6.test", "2606:4700:4700::1111", "[2606:4700:4700::1111]:80"},
	} {
		var gotHost, gotSig string
		var gotBody []byte
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHost, gotSig = r.Host, r.Header.Get("X-Codvian-Signature-256")
			gotBody, _ = io.ReadAll(r.Body)
		}))
		dns := &seqDNS{answers: map[string][][]string{tc.host: {{tc.ip}}}}
		rd := &recDialer{target: up.Listener.Addr().String()}
		d := guardedDispatcher(dns, rd, nil, "http://"+tc.host+"/hook", &logBuf{})
		d.deliver(d.endpoints[0], QuotaExceededEvent, body)
		up.Close()
		if got := rd.addrs(); len(got) != 1 || got[0] != tc.dial {
			t.Fatalf("%s: dialled %v, want [%s]", tc.host, got, tc.dial)
		}
		if gotHost != tc.host || gotSig != wantSig || !bytes.Equal(gotBody, body) {
			t.Fatalf("%s: host=%q sig=%q body=%q", tc.host, gotHost, gotSig, gotBody)
		}
	}
}

// TestDeliveryBlocksDisallowedTargets: literal and resolved loopback,
// private, link-local/metadata, IPv6-local, IPv4-mapped and mixed
// answers never reach the dialer.
func TestDeliveryBlocksDisallowedTargets(t *testing.T) {
	noRetryBackoff(t)
	dns := &seqDNS{answers: map[string][][]string{
		"lo.test":     {{"127.0.0.1"}},
		"priv.test":   {{"10.0.0.7"}},
		"meta.test":   {{"169.254.169.254"}},
		"ula.test":    {{"fd00::5"}},
		"mapped.test": {{"::ffff:192.168.1.1"}},
		"mixed.test":  {{"93.184.215.14", "172.16.0.9"}},
	}}
	rd := &recDialer{target: "127.0.0.1:1"}
	for _, u := range []string{
		"http://127.0.0.1:9/h", "http://[::1]/h", "http://169.254.169.254/latest/meta-data/",
		"http://10.1.1.1/h", "http://[fe80::1]/h", "http://[::ffff:127.0.0.1]/h", "http://0.0.0.0/h",
		"http://localhost/h", "http://lo.test/h", "http://priv.test/h", "http://meta.test/h",
		"http://ula.test/h", "http://mapped.test/h", "http://mixed.test/h",
	} {
		lb := &logBuf{}
		d := guardedDispatcher(dns, rd, nil, u, lb)
		d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
		if !strings.Contains(lb.String(), "blocked by egress policy") {
			t.Errorf("%s: log %q, want a blocked line", u, lb.String())
		}
	}
	if got := rd.addrs(); len(got) != 0 {
		t.Fatalf("blocked targets were dialled: %v", got)
	}
}

// TestDeliveryRebindingAfterRegistration: the host was public when it
// was validated at registration; by delivery it resolves to loopback.
// Delivery resolves again inside the dial and refuses - no loopback
// address is ever dialled.
func TestDeliveryRebindingAfterRegistration(t *testing.T) {
	noRetryBackoff(t)
	dns := &seqDNS{answers: map[string][][]string{"rebind.test": {{"93.184.215.14"}, {"127.0.0.1"}}}}
	g := &egress.Guard{Lookup: dns.lookup, Dial: (&recDialer{}).dial}
	if _, err := g.Resolve(context.Background(), "rebind.test"); err != nil { // registration-time check
		t.Fatalf("registration resolve: %v", err)
	}
	rd := &recDialer{target: "127.0.0.1:1"}
	lb := &logBuf{}
	d := guardedDispatcher(dns, rd, nil, "http://rebind.test/hook", lb)
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	if got := rd.addrs(); len(got) != 0 {
		t.Fatalf("dialled %v after rebinding, want nothing", got)
	}
	if !strings.Contains(lb.String(), "blocked by egress policy") {
		t.Fatalf("log %q, want blocked", lb.String())
	}
}

// TestDeliveryRebindingOnRetry: the first attempt reaches the public
// IP and gets a 5xx; before the retry the name flips to a private IP.
// The retry's new connection is validated again and refused.
func TestDeliveryRebindingOnRetry(t *testing.T) {
	noRetryBackoff(t)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Connection", "close") // force a new connection per attempt
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer up.Close()
	dns := &seqDNS{answers: map[string][][]string{"flip.test": {{"93.184.215.14"}, {"10.0.0.1"}}}}
	rd := &recDialer{target: up.Listener.Addr().String()}
	lb := &logBuf{}
	d := guardedDispatcher(dns, rd, nil, "http://flip.test/hook", lb)
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	onlyAddr(t, rd.addrs(), "93.184.215.14:80")
	if hits.Load() != 1 || !strings.Contains(lb.String(), "blocked by egress policy") {
		t.Fatalf("hits=%d log=%q, want 1 hit then a blocked retry", hits.Load(), lb.String())
	}
}

// TestDeliveryDoesNotFollowRedirects: a 3xx to an internal host is a
// terminal answer; the redirect target is never resolved or dialled.
func TestDeliveryDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer up.Close()
	dns := &seqDNS{answers: map[string][][]string{"public.test": {{"93.184.215.14"}}}}
	rd := &recDialer{target: up.Listener.Addr().String()}
	lb := &logBuf{}
	d := guardedDispatcher(dns, rd, nil, "http://public.test/hook", lb)
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	onlyAddr(t, rd.addrs(), "93.184.215.14:80")
	if hits.Load() != 1 || !strings.Contains(lb.String(), "302") {
		t.Fatalf("hits=%d log=%q, want one request and a terminal 302", hits.Load(), lb.String())
	}
}

// TestDeliveryTLSVerifiesHostname: HTTPS delivery to a validated IP
// still verifies the certificate against the hostname and sends it as
// SNI; a hostname the certificate does not cover is refused.
func TestDeliveryTLSVerifiesHostname(t *testing.T) {
	noRetryBackoff(t)
	var sni atomic.Value
	var hits atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	up.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.Store(h.ServerName)
		return nil, nil
	}}
	up.StartTLS()
	defer up.Close()
	pool := x509.NewCertPool()
	pool.AddCert(up.Certificate())
	dns := &seqDNS{answers: map[string][][]string{"example.com": {{"93.184.215.14"}}, "wrong.test": {{"93.184.215.14"}}}}
	rd := &recDialer{target: up.Listener.Addr().String()}
	cfg := &tls.Config{RootCAs: pool}

	d := guardedDispatcher(dns, rd, cfg, "https://example.com/hook", &logBuf{})
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	if hits.Load() != 1 {
		t.Fatalf("https://example.com hits = %d, want 1", hits.Load())
	}
	if got, _ := sni.Load().(string); got != "example.com" {
		t.Fatalf("SNI = %q, want example.com", got)
	}
	lb := &logBuf{}
	d = guardedDispatcher(dns, rd, cfg, "https://wrong.test/hook", lb)
	d.deliver(d.endpoints[0], QuotaExceededEvent, []byte(`{}`))
	if hits.Load() != 1 || !strings.Contains(lb.String(), "certificate") {
		t.Fatalf("hostname mismatch: hits=%d log=%q, want refused on certificate", hits.Load(), lb.String())
	}
	onlyAddr(t, rd.addrs(), "93.184.215.14:443")
}
