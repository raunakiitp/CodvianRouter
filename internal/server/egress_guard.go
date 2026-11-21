package server

import (
	"net/netip"

	"github.com/raunakiitp/codvian-router/internal/egress"
)

// egress_guard.go: server-side names for the shared SSRF-safe egress
// layer in internal/egress (validated-IP dialing, no second DNS
// resolution, proxies off, hostname TLS). POST /v1/web/fetch and
// webhook delivery both use it; there is one implementation.

type egressGuard = egress.Guard

var errEgressBlocked = egress.ErrBlocked

func egressAddrAllowed(ip netip.Addr) bool { return egress.AddrAllowed(ip) }

func defaultEgressGuard() *egressGuard { return egress.Default() }
