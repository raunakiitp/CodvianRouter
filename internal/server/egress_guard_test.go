package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEgressAddrAllowed(t *testing.T) {
	cases := map[string]bool{
		"93.184.215.14":        true,  // public IPv4
		"8.8.8.8":              true,  // public IPv4
		"2606:4700:4700::1111": true,  // public IPv6
		"127.0.0.1":            false, // loopback
		"127.8.9.10":           false, // loopback /8
		"::1":                  false, // IPv6 loopback
		"10.0.0.5":             false, // RFC1918
		"172.16.3.4":           false, // RFC1918
		"192.168.1.1":          false, // RFC1918
		"169.254.169.254":      false, // link-local / cloud metadata
		"fe80::1":              false, // IPv6 link-local
		"fc00::1":              false, // IPv6 ULA
		"fd12:3456::1":         false, // IPv6 ULA
		"0.0.0.0":              false, // unspecified
		"::":                   false, // unspecified
		"100.64.1.1":           false, // CGNAT
		"224.0.0.1":            false, // multicast
		"ff02::1":              false, // IPv6 multicast
		"255.255.255.255":      false, // broadcast / reserved
		"::ffff:127.0.0.1":     false, // IPv4-mapped loopback
		"::ffff:10.0.0.1":      false, // IPv4-mapped private
		"64:ff9b::a00:1":       false, // NAT64 onto 10.0.0.1
		"2002:a00:1::1":        false, // 6to4 embedding 10.0.0.1
		"::ffff:93.184.215.14": true,  // IPv4-mapped public
	}
	for s, want := range cases {
		if got := egressAddrAllowed(netip.MustParseAddr(s)); got != want {
			t.Errorf("egressAddrAllowed(%s) = %v, want %v", s, got, want)
		}
	}
}

// fakeDNS is deterministic DNS: answers[host] is returned per call in
// order (the last answer repeats), so a rebinding resolver can give
// a public address first and a private one after.
type fakeDNS struct {
	mu      sync.Mutex
	answers map[string][][]string
	calls   map[string]int
}

func (f *fakeDNS) lookup(_ context.Context, host string) ([]netip.Addr, error) {
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
	out := []netip.Addr{}
	for _, s := range seq[i] {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

// recordingDialer records every address dialled and connects each
// one to target (a local test server standing in for the public IP).
type recordingDialer struct {
	mu     sync.Mutex
	dialed []string
	target string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.dialed = append(d.dialed, addr)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func (d *recordingDialer) addrs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.dialed...)
}

func webFetch(t *testing.T, srv *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"url": url})
	return doReqPeer(t, srv, "POST", "/v1/web/fetch", string(b), "test-key-123", loopbackPeer)
}

func newFetchServer(t *testing.T, dns *fakeDNS, dialer *recordingDialer, tlsCfg *tls.Config) *Server {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	srv.webFetchEgress = &egressGuard{lookup: dns.lookup, dial: dialer.dial}
	srv.webFetchTLS = tlsCfg
	return srv
}

// TestWebFetchDialsTheValidatedIP proves the address actually
// connected to is the validated public IP: the dialer sees only
// the IP literal from the single DNS answer, never the hostname.
func TestWebFetchDialsTheValidatedIP(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "public.test" {
			t.Errorf("upstream saw Host %q, want public.test (hostname must be preserved)", r.Host)
		}
		_, _ = w.Write([]byte("<html><title>Hi</title><body>ok page</body></html>"))
	}))
	defer up.Close()
	dns := &fakeDNS{answers: map[string][][]string{"public.test": {{"93.184.215.14"}}}}
	dialer := &recordingDialer{target: up.Listener.Addr().String()}
	srv := newFetchServer(t, dns, dialer, nil)

	rec := webFetch(t, srv, "http://public.test/page")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := dialer.addrs(); len(got) != 1 || got[0] != "93.184.215.14:80" {
		t.Fatalf("dialed %v, want exactly [93.184.215.14:80]", got)
	}
	var res map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["title"] != "Hi" || !strings.Contains(res["content"].(string), "ok page") {
		t.Errorf("unexpected body %v", res)
	}
}

// TestWebFetchIPv6Public proves a public IPv6 answer is dialled as
// a bracketed IPv6 literal.
func TestWebFetchIPv6Public(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("v6")) }))
	defer up.Close()
	dns := &fakeDNS{answers: map[string][][]string{"v6.test": {{"2606:4700:4700::1111"}}}}
	dialer := &recordingDialer{target: up.Listener.Addr().String()}
	srv := newFetchServer(t, dns, dialer, nil)
	if rec := webFetch(t, srv, "http://v6.test/"); rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := dialer.addrs(); len(got) != 1 || got[0] != "[2606:4700:4700::1111]:80" {
		t.Fatalf("dialed %v, want [[2606:4700:4700::1111]:80]", got)
	}
}

// TestWebFetchBlocksPrivateTargets covers literal and resolved
// private/loopback/link-local/IPv6-local targets and a mixed answer.
// None of them may reach the dialer.
func TestWebFetchBlocksPrivateTargets(t *testing.T) {
	dns := &fakeDNS{answers: map[string][][]string{
		"loop.test":    {{"127.0.0.1"}},
		"private.test": {{"10.1.2.3"}},
		"meta.test":    {{"169.254.169.254"}},
		"ula.test":     {{"fd00::1"}},
		"mapped.test":  {{"::ffff:127.0.0.1"}},
		"mixed.test":   {{"93.184.215.14", "192.168.0.10"}},
		"v6mixed.test": {{"2606:4700:4700::1111", "::1"}},
	}}
	dialer := &recordingDialer{target: "127.0.0.1:1"}
	srv := newFetchServer(t, dns, dialer, nil)
	urls := []string{
		"http://127.0.0.1:8080/secret",
		"http://[::1]/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://[fe80::1]/",
		"http://localhost/",
		"http://api.localhost/",
		"http://loop.test/",
		"http://private.test/",
		"http://meta.test/",
		"http://ula.test/",
		"http://mapped.test/",
		"http://mixed.test/",
		"http://v6mixed.test/",
	}
	for _, u := range urls {
		if rec := webFetch(t, srv, u); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403, body %s", u, rec.Code, rec.Body.String())
		}
	}
	if got := dialer.addrs(); len(got) != 0 {
		t.Fatalf("blocked targets reached the dialer: %v", got)
	}
}

// TestWebFetchDNSRebinding: the first resolution is public, every
// later one is 127.0.0.1. Whatever number of resolutions the client
// makes, every dialled address must be the validated public IP, or
// the request must be refused - never a loopback dial.
func TestWebFetchDNSRebinding(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer up.Close()
	dns := &fakeDNS{answers: map[string][][]string{"rebind.test": {{"93.184.215.14"}, {"127.0.0.1"}}}}
	dialer := &recordingDialer{target: up.Listener.Addr().String()}
	srv := newFetchServer(t, dns, dialer, nil)

	rec := webFetch(t, srv, "http://rebind.test/")
	for _, a := range dialer.addrs() {
		if a != "93.184.215.14:80" {
			t.Fatalf("dialled %s after rebinding, want only the validated 93.184.215.14:80", a)
		}
	}
	if rec.Code != http.StatusOK && rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 200 (validated IP used) or 403", rec.Code)
	}
	// A second request resolves again and now gets 127.0.0.1: refused.
	rec = webFetch(t, srv, "http://rebind.test/")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebound request: status %d, want 403", rec.Code)
	}
	for _, a := range dialer.addrs() {
		if strings.HasPrefix(a, "127.") {
			t.Fatalf("a loopback address was dialled: %v", dialer.addrs())
		}
	}
}

// TestWebFetchRedirectToPrivateBlocked: a public page redirecting to
// a host that resolves privately, and to a literal metadata IP, is
// refused; the private address is never dialled.
func TestWebFetchRedirectToPrivateBlocked(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/to-internal":
			http.Redirect(w, r, "http://internal.test/admin", http.StatusFound)
		case "/to-meta":
			http.Redirect(w, r, "http://169.254.169.254/latest/", http.StatusFound)
		case "/to-file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		default:
			_, _ = w.Write([]byte("fine"))
		}
	}))
	defer up.Close()
	dns := &fakeDNS{answers: map[string][][]string{
		"public.test":   {{"93.184.215.14"}},
		"internal.test": {{"10.0.0.5"}},
	}}
	dialer := &recordingDialer{target: up.Listener.Addr().String()}
	srv := newFetchServer(t, dns, dialer, nil)
	for _, p := range []string{"/to-internal", "/to-meta", "/to-file"} {
		if rec := webFetch(t, srv, "http://public.test"+p); rec.Code != http.StatusForbidden {
			t.Errorf("redirect %s: status %d, want 403, body %s", p, rec.Code, rec.Body.String())
		}
	}
	for _, a := range dialer.addrs() {
		if a != "93.184.215.14:80" {
			t.Fatalf("redirect reached a non-validated address: %v", dialer.addrs())
		}
	}
}

// TestWebFetchTLSKeepsHostname proves HTTPS still verifies the
// certificate against the HOSTNAME (SNI + verification) even though
// the dial goes to an IP literal: a cert valid for example.com
// succeeds, and a hostname the cert does not cover fails with 502.
func TestWebFetchTLSKeepsHostname(t *testing.T) {
	var sni atomic.Value
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("secure")) }))
	up.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		sni.Store(h.ServerName)
		return nil, nil
	}}
	up.StartTLS()
	defer up.Close()
	pool := x509.NewCertPool()
	pool.AddCert(up.Certificate())

	dns := &fakeDNS{answers: map[string][][]string{
		"example.com": {{"93.184.215.14"}}, // httptest cert covers example.com
		"wrong.test":  {{"93.184.215.14"}},
	}}
	dialer := &recordingDialer{target: up.Listener.Addr().String()}
	srv := newFetchServer(t, dns, dialer, &tls.Config{RootCAs: pool})

	rec := webFetch(t, srv, "https://example.com/")
	if rec.Code != http.StatusOK {
		t.Fatalf("https://example.com: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got, _ := sni.Load().(string); got != "example.com" {
		t.Errorf("TLS SNI = %q, want example.com", got)
	}
	if rec := webFetch(t, srv, "https://wrong.test/"); rec.Code != http.StatusBadGateway {
		t.Errorf("cert/hostname mismatch: status %d, want 502 (verification must stay on)", rec.Code)
	}
}

func TestWebFetchRejectsBadInput(t *testing.T) {
	srv := newFetchServer(t, &fakeDNS{}, &recordingDialer{}, nil)
	for _, u := range []string{"", "ftp://example.com/", "file:///etc/passwd", "http://user:pw@example.com/", "/relative"} {
		if rec := webFetch(t, srv, u); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: status %d, want 400", u, rec.Code)
		}
	}
}
