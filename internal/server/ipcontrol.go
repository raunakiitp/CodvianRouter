// Package server — IP allowlist/blocklist (PR #53, Phase 4).
//
// IP control restricts dashboard/API access by client IP. Wired into the
// middleware chain between logging and audit:
//
//   - Logging (outermost): every request gets a correlation ID
//   - IP control: denies disallowed IPs with 403 before audit/handler
//   - Audit: records allowed requests
//
// Security model:
//   - Client IP is resolved from RemoteAddr (the real TCP peer) only.
//     X-Forwarded-For and Host are NEVER trusted.
//   - Allowlist and blocklist are mutually exclusive per entry.
//   - Blocklist takes precedence: listed IPs are always denied.
//   - Empty allowlist = allow all (subject to blocklist).
//   - Non-empty allowlist = deny all except listed IPs (deny-by-default).
//   - CIDR support for both IPv4 and IPv6 (net.ParseCIDR).
package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
)

// ipRule is a single allowlist or blocklist entry (IP or CIDR).
type ipRule struct {
	network *net.IPNet // nil for single-IP entries
	ip      net.IP     // the IP (single-IP) or network.IP (CIDR)
	cidr    string     // original string (for logging/errors)
	isCIDR  bool
}

// ipControlConfig holds the parsed IP control configuration.
type ipControlConfig struct {
	allowlist []ipRule // empty = allow all
	blocklist []ipRule // empty = block none
	denyAll   bool     // true when allowlist is non-empty
}

// ipControlStore holds the server's IP control configuration.
type ipControlStore struct {
	mu  sync.RWMutex
	cfg *ipControlConfig
}

// newIPControlStore creates an empty IP control store (allow all, block none).
func newIPControlStore() *ipControlStore {
	return &ipControlStore{
		cfg: &ipControlConfig{},
	}
}

// parseIPRule parses a single IP or CIDR string into an ipRule.
func parseIPRule(s string) (ipRule, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ipRule{}, fmt.Errorf("ip: empty rule")
	}
	if strings.Contains(s, "/") {
		_, network, err := net.ParseCIDR(s)
		if err != nil {
			return ipRule{}, fmt.Errorf("ip: invalid CIDR %q: %w", s, err)
		}
		return ipRule{network: network, ip: network.IP, cidr: s, isCIDR: true}, nil
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return ipRule{}, fmt.Errorf("ip: invalid IP %q", s)
	}
	return ipRule{network: nil, ip: ip, cidr: s, isCIDR: false}, nil
}

// match reports whether the rule matches the given IP.
func (r *ipRule) match(ip net.IP) bool {
	if r.isCIDR {
		return r.network.Contains(ip)
	}
	return r.ip.Equal(ip)
}

// matchAny reports whether ip matches any rule in the list.
func matchAny(ip net.IP, rules []ipRule) bool {
	for _, r := range rules {
		if r.match(ip) {
			return true
		}
	}
	return false
}

// clientIP resolves the client IP from the request. Returns the
// RemoteAddr host only; X-Forwarded-For and Host are never trusted.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// allowed reports whether the given client IP is permitted.
func (s *ipControlStore) allowed(ip net.IP) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	c := s.cfg
	if matchAny(ip, c.blocklist) {
		return false
	}
	if c.denyAll {
		return matchAny(ip, c.allowlist)
	}
	return true
}

// configure replaces the IP control configuration.
func (s *ipControlStore) configure(allowlist, blocklist []string) error {
	var allowRules, blockRules []ipRule
	for _, s := range allowlist {
		r, err := parseIPRule(s)
		if err != nil {
			return err
		}
		allowRules = append(allowRules, r)
	}
	for _, s := range blocklist {
		r, err := parseIPRule(s)
		if err != nil {
			return err
		}
		blockRules = append(blockRules, r)
	}

	// An IP cannot be in both lists.
	for _, ar := range allowRules {
		for _, br := range blockRules {
			if ar.ip.Equal(br.ip) || (ar.isCIDR && br.isCIDR && ar.network.String() == br.network.String()) {
				return fmt.Errorf("ip: %q is in both allowlist and blocklist", ar.cidr)
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = &ipControlConfig{
		allowlist: allowRules,
		blocklist: blockRules,
		denyAll:   len(allowRules) > 0,
	}
	return nil
}

// withIPControl is middleware that enforces IP allowlist/blocklist.
// Denied requests receive 403. Allowed requests proceed to the next
// handler (audit + route).
func (s *Server) withIPControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := net.ParseIP(s.clientIP(r))
		if ip == nil {
			writeError(w, http.StatusForbidden, "ip: client IP not permitted")
			return
		}
		store := s.ipControlStore()
		if store == nil || !store.allowed(ip) {
			writeError(w, http.StatusForbidden, "ip: client IP not permitted")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ipControlStore returns the server's IP control store, creating it on
// first use.
func (s *Server) ipControlStore() *ipControlStore {
	s.dashAuthMu.Lock()
	defer s.dashAuthMu.Unlock()
	if s.ipControl != nil {
		return s.ipControl
	}
	s.ipControl = newIPControlStore()
	return s.ipControl
}
