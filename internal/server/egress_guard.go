package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// egress_guard.go: the SSRF-safe outbound HTTP client used by
// POST /v1/web/fetch.
//
// The earlier implementation validated a hostname with one DNS
// lookup and then let net/http resolve it AGAIN when dialling. A
// DNS-rebinding server can answer the first lookup with a public
// address and the second with 127.0.0.1 or 169.254.169.254, so the
// address actually connected to was never the one validated.
//
// Here the check lives inside the transport's DialContext: every
// connection (the first request AND every redirect hop) resolves
// the host exactly once, rejects the whole answer if ANY address is
// disallowed, and then dials one of the validated IP literals. No
// second resolution happens, so the connected IP is always a
// validated one. The URL keeps its hostname, so the Host header,
// TLS SNI and certificate verification still use the hostname.
// Environment proxies are disabled: a proxy would resolve the
// target itself and bypass this check.

// errEgressBlocked marks a destination refused by the egress policy.
var errEgressBlocked = errors.New("destination blocked by SSRF policy")

// egressLookupFunc resolves a host to its addresses. Production uses
// net.DefaultResolver.LookupNetIP; tests inject deterministic DNS.
type egressLookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// egressDialFunc opens the TCP connection to an already-validated
// IP:port. Production uses a net.Dialer; tests may redirect it.
type egressDialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// blockedEgressPrefixes are the ranges an outbound fetch may never
// reach, beyond what netip.Addr's Is* predicates already cover.
var blockedEgressPrefixes = func() []netip.Prefix {
	raw := []string{
		"0.0.0.0/8",       // "this network" (RFC 1122)
		"100.64.0.0/10",   // carrier-grade NAT (RFC 6598)
		"192.0.0.0/24",    // IETF protocol assignments (RFC 6890)
		"192.0.2.0/24",    // TEST-NET-1
		"198.18.0.0/15",   // benchmarking (RFC 2544)
		"198.51.100.0/24", // TEST-NET-2
		"203.0.113.0/24",  // TEST-NET-3
		"240.0.0.0/4",     // reserved, includes 255.255.255.255
		"64:ff9b::/96",    // NAT64: maps onto IPv4, could reach private v4
		"64:ff9b:1::/48",  // local-use NAT64 (RFC 8215)
		"100::/64",        // discard-only (RFC 6666)
		"2001:db8::/32",   // documentation
		"2002::/16",       // 6to4: embeds an IPv4 address
	}
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// egressAddrAllowed reports whether ip is a public unicast address
// an outbound fetch may connect to. IPv4-mapped IPv6 addresses are
// unmapped first, so ::ffff:127.0.0.1 is treated as 127.0.0.1.
func egressAddrAllowed(ip netip.Addr) bool {
	if !ip.IsValid() {
		return false
	}
	ip = ip.Unmap()
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, p := range blockedEgressPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// egressGuard holds the resolver and dialer the safe client uses.
type egressGuard struct {
	lookup egressLookupFunc
	dial   egressDialFunc
}

func defaultEgressGuard() *egressGuard {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &egressGuard{
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		dial: d.DialContext,
	}
}

// resolve returns the validated addresses for host, or an error
// wrapping errEgressBlocked when any answer is disallowed. A mixed
// answer (one public, one private) is refused as a whole: picking
// only the public one would still let an attacker steer which
// address a retry or another client uses.
func (g *egressGuard) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") ||
		strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".localhost") {
		return nil, fmt.Errorf("%w: %s is a loopback name", errEgressBlocked, host)
	}
	if ip, err := netip.ParseAddr(strings.Trim(host, "[]")); err == nil {
		if !egressAddrAllowed(ip) {
			return nil, fmt.Errorf("%w: %s is not a public address", errEgressBlocked, ip)
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	addrs, err := g.lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if !egressAddrAllowed(a) {
			return nil, fmt.Errorf("%w: %s resolves to %s, which is not a public address", errEgressBlocked, host, a.Unmap())
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}

// dialContext is the transport's DialContext: resolve once,
// validate, then dial only validated IP literals.
func (g *egressGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := g.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// client builds the SSRF-safe client. tlsConfig may be nil (system
// roots, full verification); tests pass a pool trusting their test
// server, never InsecureSkipVerify.
func (g *egressGuard) client(timeout time.Duration, maxRedirects int, tlsConfig *tls.Config) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil, // a proxy would resolve the host itself
		DialContext:           g.dialContext,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w: redirect to scheme %q", errEgressBlocked, req.URL.Scheme)
			}
			// The dial of the redirect target is validated by
			// dialContext; this early check gives a clearer error
			// for a literal blocked IP or localhost name.
			if _, err := g.resolve(req.Context(), req.URL.Hostname()); errors.Is(err, errEgressBlocked) {
				return err
			}
			return nil
		},
	}
}
