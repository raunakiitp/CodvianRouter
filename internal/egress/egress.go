// Package egress is the SSRF-safe outbound HTTP layer shared by every
// server-initiated request to a user-supplied URL (POST /v1/web/fetch,
// webhook delivery).
//
// The policy check lives inside the transport's DialContext: every new
// connection (first request, every retry, every redirect hop) resolves
// the host exactly once, rejects the whole answer if ANY address is
// disallowed, and then dials one of those validated IP literals. There
// is no second DNS resolution between the check and the connect, so a
// DNS-rebinding server cannot swap in 127.0.0.1 or 169.254.169.254
// after validation. The request URL keeps its hostname, so the Host
// header, TLS SNI and certificate verification still use the hostname.
// Environment proxies are disabled: a proxy resolves the target itself
// and would bypass the check.
package egress

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

// ErrBlocked marks a destination refused by the egress policy. Callers
// test it with errors.Is (it survives *url.Error wrapping).
var ErrBlocked = errors.New("destination blocked by SSRF policy")

// LookupFunc resolves a host to its addresses.
type LookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// DialFunc opens the TCP connection to an already-validated IP:port.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// blockedPrefixes are ranges never reachable, beyond what netip.Addr's
// Is* predicates already cover.
var blockedPrefixes = func() []netip.Prefix {
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

// classify returns "" for an allowed public unicast address, or the
// reason it is blocked. IPv4-mapped IPv6 is unmapped first.
func classify(ip netip.Addr) string {
	if !ip.IsValid() {
		return "invalid"
	}
	ip = ip.Unmap()
	switch {
	case ip.IsLoopback():
		return "loopback"
	case ip.IsPrivate():
		return "private-network"
	case ip.IsUnspecified():
		return "unspecified"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "link-local"
	case ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return "multicast"
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return "reserved"
		}
	}
	return ""
}

// AddrAllowed reports whether ip is a public unicast address an
// outbound request may connect to.
func AddrAllowed(ip netip.Addr) bool { return classify(ip) == "" }

// Guard holds the resolver and dialer. The zero value is not usable;
// use Default, or set both fields (tests inject deterministic DNS and a
// dialer that records the address it was asked to connect to).
type Guard struct {
	Lookup LookupFunc
	Dial   DialFunc
}

// Default returns a Guard using the system resolver and a net.Dialer.
func Default() *Guard {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &Guard{
		Lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		Dial: d.DialContext,
	}
}

// Resolve returns the validated addresses for host, or an error
// wrapping ErrBlocked when the name or any answer is disallowed. A
// mixed answer (one public, one private) is refused as a whole.
func (g *Guard) Resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" {
		return nil, fmt.Errorf("%w: empty host", ErrBlocked)
	}
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return nil, fmt.Errorf("%w: %s is a loopback (localhost) name", ErrBlocked, host)
	}
	if ip, err := netip.ParseAddr(strings.Trim(h, "[]")); err == nil {
		if r := classify(ip); r != "" {
			return nil, fmt.Errorf("%w: %s is a %s address", ErrBlocked, ip, r)
		}
		return []netip.Addr{ip.Unmap()}, nil
	}
	addrs, err := g.Lookup(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if r := classify(a); r != "" {
			return nil, fmt.Errorf("%w: %s resolves to %s, a %s address", ErrBlocked, host, a.Unmap(), r)
		}
		out = append(out, a.Unmap())
	}
	return out, nil
}

// DialContext resolves once, validates, then dials only validated IP
// literals. It is the transport's only dial path.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.Resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := g.Dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// DialContext returns a DialContext that applies the guard's policy to
// every connection made through the given dialer. It is the
// integration point for callers that already own a transport (the
// provider client's transport, for example) and only need their
// dial path checked: the host is resolved once, every answer is
// validated, and only a validated IP literal is dialed. Because it
// wraps the transport's ONLY dial path, it also covers every
// redirect hop the client follows - there is no second, unchecked
// way out.
//
// The wrapped dialer is used for the actual connection, so the
// caller's own dial settings (timeouts, TCP_FASTOPEN, TCP_NODELAY)
// are preserved; only the ADDRESS the connection targets is
// constrained.
func DialContext(dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	g := Default()
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := g.Resolve(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: no address to dial", ErrBlocked)
		}
		return nil, lastErr
	}
}

// Client builds an SSRF-safe client. maxRedirects <= 0 disables
// redirects: the 3xx response is returned to the caller as-is. A
// followed redirect must be http(s), and its connection goes through
// DialContext like any other. tlsConfig may be nil (system roots, full
// verification); tests pass a pool trusting their server, never
// InsecureSkipVerify. timeout <= 0 leaves the client timeout to the
// request context.
func (g *Guard) Client(timeout time.Duration, maxRedirects int, tlsConfig *tls.Config) *http.Client {
	tr := &http.Transport{
		Proxy:               nil, // a proxy would resolve the host itself
		DialContext:         g.DialContext,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        4,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	if timeout > 0 {
		tr.ResponseHeaderTimeout = timeout
	}
	c := &http.Client{Transport: tr}
	if timeout > 0 {
		c.Timeout = timeout
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if maxRedirects <= 0 {
			return http.ErrUseLastResponse
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("%w: redirect to scheme %q", ErrBlocked, req.URL.Scheme)
		}
		// The redirect's connection is validated by DialContext; this
		// early check only gives a clearer error for a blocked
		// literal or localhost name.
		if _, err := g.Resolve(req.Context(), req.URL.Hostname()); errors.Is(err, ErrBlocked) {
			return err
		}
		return nil
	}
	return c
}
