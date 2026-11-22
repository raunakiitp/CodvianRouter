package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseIPRule(t *testing.T) {
	// Single IP
	r, err := parseIPRule("10.0.0.1")
	if err != nil {
		t.Fatalf("parseIPRule: %v", err)
	}
	if r.isCIDR {
		t.Error("expected single IP, got CIDR")
	}
	if !r.ip.Equal(net.ParseIP("10.0.0.1")) {
		t.Errorf("expected IP 10.0.0.1, got %v", r.ip)
	}

	// CIDR
	r, err = parseIPRule("10.0.0.0/24")
	if err != nil {
		t.Fatalf("parseIPRule: %v", err)
	}
	if !r.isCIDR {
		t.Error("expected CIDR, got single IP")
	}
	if !r.network.Contains(net.ParseIP("10.0.0.5")) {
		t.Error("expected CIDR to contain 10.0.0.5")
	}

	// Invalid
	_, err = parseIPRule("not-an-ip")
	if err == nil {
		t.Error("expected error for invalid IP")
	}
	_, err = parseIPRule("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}

func TestIPRuleMatch(t *testing.T) {
	r, _ := parseIPRule("10.0.0.1")
	if !r.match(net.ParseIP("10.0.0.1")) {
		t.Error("single IP should match itself")
	}
	if r.match(net.ParseIP("10.0.0.2")) {
		t.Error("single IP should not match different IP")
	}

	cidr, _ := parseIPRule("10.0.0.0/24")
	if !cidr.match(net.ParseIP("10.0.0.5")) {
		t.Error("CIDR should contain 10.0.0.5")
	}
	if !cidr.match(net.ParseIP("10.0.0.0")) {
		t.Error("CIDR should contain network address")
	}
	if !cidr.match(net.ParseIP("10.0.0.255")) {
		t.Error("CIDR should contain broadcast address")
	}
	if cidr.match(net.ParseIP("10.0.1.1")) {
		t.Error("CIDR should not contain 10.0.1.1")
	}

	// IPv6
	v6, _ := parseIPRule("::1")
	if !v6.match(net.ParseIP("::1")) {
		t.Error("IPv6 loopback should match itself")
	}
	v6cidr, _ := parseIPRule("fd00::/8")
	if !v6cidr.match(net.ParseIP("fd00::1")) {
		t.Error("IPv6 CIDR should contain fd00::1")
	}
}

func TestMatchAny(t *testing.T) {
	rules := []ipRule{
		mustParseRule(t, "10.0.0.1"),
		mustParseRule(t, "192.168.0.0/16"),
	}
	if !matchAny(net.ParseIP("10.0.0.1"), rules) {
		t.Error("should match 10.0.0.1")
	}
	if !matchAny(net.ParseIP("192.168.1.1"), rules) {
		t.Error("should match 192.168.1.1 (in CIDR)")
	}
	if matchAny(net.ParseIP("172.16.0.1"), rules) {
		t.Error("should not match 172.16.0.1")
	}
}

func mustParseRule(t *testing.T, s string) ipRule {
	t.Helper()
	r, err := parseIPRule(s)
	if err != nil {
		t.Fatalf("parseIPRule(%q): %v", s, err)
	}
	return r
}

func TestIPControlConfigure(t *testing.T) {
	store := newIPControlStore()

	// Empty config: allow all
	if !store.allowed(net.ParseIP("1.2.3.4")) {
		t.Error("empty config should allow all")
	}

	// Blocklist only
	if err := store.configure(nil, []string{"10.0.0.1"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if store.allowed(net.ParseIP("10.0.0.1")) {
		t.Error("blocklisted IP should be denied")
	}
	if !store.allowed(net.ParseIP("10.0.0.2")) {
		t.Error("non-blocklisted IP should be allowed")
	}

	// Allowlist only (deny-by-default)
	if err := store.configure([]string{"10.0.0.1"}, nil); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if !store.allowed(net.ParseIP("10.0.0.1")) {
		t.Error("allowlisted IP should be allowed")
	}
	if store.allowed(net.ParseIP("10.0.0.2")) {
		t.Error("non-allowlisted IP should be denied")
	}

	// Blocklist takes precedence over allowlist
	if err := store.configure([]string{"10.0.0.0/8"}, []string{"10.0.0.1"}); err != nil {
		t.Fatalf("configure: %v", err)
	}
	if store.allowed(net.ParseIP("10.0.0.1")) {
		t.Error("blocklisted IP should be denied even if in allowlist CIDR")
	}
	if !store.allowed(net.ParseIP("10.0.0.2")) {
		t.Error("allowlisted CIDR IP should be allowed (not blocklisted)")
	}

	// IP in both lists should error
	if err := store.configure([]string{"10.0.0.1"}, []string{"10.0.0.1"}); err == nil {
		t.Error("expected error for IP in both lists")
	}
}

func TestWithIPControlMiddleware(t *testing.T) {
	s := &Server{}

	// No IP control configured: allow all
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	h := s.withIPControl(next)

	// Loopback peer: allowed
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Unparseable RemoteAddr: denied
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "not-an-ip"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
}

func TestClientIP(t *testing.T) {
	s := &Server{}

	// Normal RemoteAddr
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.100:44444"
	if ip := s.clientIP(req); ip != "192.168.1.100" {
		t.Errorf("expected 192.168.1.100, got %q", ip)
	}

	// RemoteAddr without port
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "10.0.0.1"
	if ip := s.clientIP(req); ip != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %q", ip)
	}

	// IPv6
	req = httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "[::1]:54321"
	if ip := s.clientIP(req); ip != "::1" {
		t.Errorf("expected ::1, got %q", ip)
	}
}

func TestXFFNotTrusted(t *testing.T) {
	s := &Server{}
	// Even with X-Forwarded-For, clientIP returns RemoteAddr (no trusted proxies)
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.100:44444"
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	if ip := s.clientIP(req); ip != "192.168.1.100" {
		t.Errorf("expected 192.168.1.100 (XFF not trusted), got %q", ip)
	}
}
