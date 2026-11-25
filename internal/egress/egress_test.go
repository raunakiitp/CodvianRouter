// Package egress tests the SSRF-safe outbound policy: the
// connection-time half of the provider base-URL policy (the
// validation half is covered in internal/provider's
// ValidateBaseURL tests). These tests pin the vectors a
// regression would reopen: literal loopback / private / link-local
// / cloud-metadata / reserved addresses, a HOSTNAME that resolves
// into that space (DNS rebinding), a mixed answer, and the
// redirect policy.
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"
)

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// TestAddrAllowedMatrix pins classify() across the address space an
// outbound request must never reach, plus a few that must stay
// reachable (the public internet).
func TestAddrAllowedMatrix(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.255.255.254", "0.0.0.0", // loopback + this-network
		"10.0.0.1", "172.16.5.5", "192.168.0.1", // RFC1918
		"100.64.0.1",                     // CGNAT (RFC 6598)
		"169.254.169.254", "169.254.1.1", // link-local + cloud metadata
		"192.0.2.1", "198.51.100.9", "203.0.113.7", // TEST-NETs
		"198.18.0.1", "240.0.0.1", "255.255.255.255", // benchmarking + reserved
		"224.0.0.1", // multicast
		"::1", "::", // IPv6 loopback + unspecified
		"fc00::1", "fd12:3456::1", // unique-local
		"fe80::1", "ff02::1", // link-local + multicast
		"64:ff9b::7f00:1",  // NAT64 wrapping 127.0.0.1
		"2002:7f00:1::",    // 6to4 wrapping 127.0.0.1
		"2001:db8::1",      // documentation
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"::ffff:10.0.0.1",  // IPv4-mapped private
	}
	for _, s := range blocked {
		if AddrAllowed(mustAddr(t, s)) {
			t.Errorf("AddrAllowed(%s) = true; want false (non-public)", s)
		}
	}
	allowed := []string{"93.184.216.34", "8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if !AddrAllowed(mustAddr(t, s)) {
			t.Errorf("AddrAllowed(%s) = false; want true (public unicast)", s)
		}
	}
}

// TestResolveRejectsLiteralAndLocalhostNames covers the parts
// decidable without DNS: literal non-public IPs (v4 and v6,
// bracketed or not) and localhost names.
func TestResolveRejectsLiteralAndLocalhostNames(t *testing.T) {
	g := &Guard{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			t.Fatal("Lookup must not run for a literal or localhost name")
			return nil, nil
		},
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, nil },
	}
	for _, host := range []string{
		"127.0.0.1", "169.254.169.254", "10.1.2.3", "0.0.0.0",
		"::1", "[::1]", "fc00::1", "fe80::1", "::ffff:127.0.0.1",
		"localhost", "LOCALHOST", "svc.localhost",
	} {
		if _, err := g.Resolve(context.Background(), host); !errors.Is(err, ErrBlocked) {
			t.Errorf("Resolve(%q) err = %v; want ErrBlocked", host, err)
		}
	}
	if _, err := g.Resolve(context.Background(), ""); !errors.Is(err, ErrBlocked) {
		t.Errorf("Resolve(\"\") err = %v; want ErrBlocked", err)
	}
}

// TestResolveBlocksDNSRebinding: a name that LOOKS public but
// resolves into private space is refused — the core rebinding case.
// A MIXED answer (one public, one private) is refused as a whole, so
// a rebinding server cannot win the race by returning both.
func TestResolveBlocksDNSRebinding(t *testing.T) {
	cases := map[string][]string{
		"rebind.example": {"127.0.0.1"},
		"meta.example":   {"169.254.169.254"},
		"internal.thing": {"10.1.2.3"},
		"v6.internal":    {"fd00::1"},
		"mixed.example":  {"93.184.216.34", "10.0.0.9"},
		"mapped.example": {"::ffff:127.0.0.1"},
	}
	for host, ips := range cases {
		g := &Guard{
			Lookup: func(_ context.Context, _ string) ([]netip.Addr, error) {
				out := make([]netip.Addr, 0, len(ips))
				for _, s := range ips {
					out = append(out, mustAddr(t, s))
				}
				return out, nil
			},
			Dial: func(context.Context, string, string) (net.Conn, error) {
				t.Errorf("Dial must not run for %s", host)
				return nil, nil
			},
		}
		if _, err := g.Resolve(context.Background(), host); !errors.Is(err, ErrBlocked) {
			t.Errorf("Resolve(%q) = %v; want ErrBlocked (rebinding)", host, err)
		}
	}
}

// TestResolveAcceptsPublicName: a name that resolves only to public
// addresses is allowed and returns the validated, unmapped IPs.
func TestResolveAcceptsPublicName(t *testing.T) {
	g := &Guard{
		Lookup: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host != "api.example.com" {
				t.Fatalf("unexpected host %q", host)
			}
			return []netip.Addr{mustAddr(t, "93.184.216.34"), mustAddr(t, "::ffff:8.8.8.8")}, nil
		},
		Dial: func(context.Context, string, string) (net.Conn, error) { return nil, nil },
	}
	ips, err := g.Resolve(context.Background(), "api.example.com.")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("ips = %d, want 2", len(ips))
	}
	if ips[1].Is4() == false || ips[1].String() != "8.8.8.8" {
		t.Errorf("IPv4-mapped address not unmapped: %v", ips[1])
	}
}

// recordingDialer records the addresses DialContext is asked to
// connect to, so a test can prove only validated IP LITERALS are
// dialed (never the hostname).
type recordingDialer struct {
	dialed []string
}

func (d *recordingDialer) dial(_ context.Context, _ string, addr string) (net.Conn, error) {
	d.dialed = append(d.dialed, addr)
	return nil, errors.New("stop after recording")
}

// TestDialDialsValidatedLiteralOnly: the dialer receives a validated
// IP literal, never the hostname — so there is no second DNS
// resolution a rebinding server could race.
func TestDialDialsValidatedLiteralOnly(t *testing.T) {
	d := &recordingDialer{}
	g := &Guard{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{mustAddr(t, "93.184.216.34")}, nil
		},
		Dial: d.dial,
	}
	_, err := g.DialContext(context.Background(), "tcp", "api.example.com:443")
	if err == nil {
		t.Fatal("expected the dialer's sentinel error")
	}
	if len(d.dialed) != 1 || d.dialed[0] != "93.184.216.34:443" {
		t.Fatalf("dialed = %v, want [93.184.216.34:443]", d.dialed)
	}
}

// TestDialRefusesRebindBeforeDialing: a rebinding host never reaches
// the dialer.
func TestDialRefusesRebindBeforeDialing(t *testing.T) {
	d := &recordingDialer{}
	g := &Guard{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{mustAddr(t, "169.254.169.254")}, nil
		},
		Dial: d.dial,
	}
	if _, err := g.DialContext(context.Background(), "tcp", "meta.example:80"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	if len(d.dialed) != 0 {
		t.Fatalf("dialed %v; want nothing dialed for a rebinding host", d.dialed)
	}
}

// TestClientRedirectPolicy pins the CheckRedirect rules: maxRedirects
// <= 0 stops redirects entirely; a non-http(s) scheme is refused; a
// redirect to a blocked literal/localhost is refused before the
// connection; the hop limit is enforced.
func TestClientRedirectPolicy(t *testing.T) {
	g := Default()

	// maxRedirects <= 0 => do not follow.
	c0 := g.Client(time.Second, 0, nil)
	req, _ := http.NewRequest(http.MethodGet, "https://api.example.com/x", nil)
	if err := c0.CheckRedirect(req, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("maxRedirects=0 CheckRedirect = %v, want ErrUseLastResponse", err)
	}

	c := g.Client(time.Second, 3, nil) // system resolver; only literals are exercised

	// Scheme downgrade (file:) refused.
	req, _ = http.NewRequest(http.MethodGet, "file:///etc/passwd", nil)
	if err := c.CheckRedirect(req, nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("file: redirect = %v, want ErrBlocked", err)
	}
	// Redirect to a loopback literal refused.
	req, _ = http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/x", nil)
	if err := c.CheckRedirect(req, nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("loopback redirect = %v, want ErrBlocked", err)
	}
	// Redirect to the cloud metadata endpoint refused.
	req, _ = http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data", nil)
	if err := c.CheckRedirect(req, nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("metadata redirect = %v, want ErrBlocked", err)
	}
	// Redirect to localhost by name refused.
	req, _ = http.NewRequest(http.MethodGet, "http://localhost:8080/x", nil)
	if err := c.CheckRedirect(req, nil); !errors.Is(err, ErrBlocked) {
		t.Errorf("localhost redirect = %v, want ErrBlocked", err)
	}
	// Hop limit enforced.
	req, _ = http.NewRequest(http.MethodGet, "https://93.184.216.34/x", nil)
	via := make([]*http.Request, 3)
	if err := c.CheckRedirect(req, via); err == nil {
		t.Errorf("redirect past the hop limit was allowed")
	}
}
