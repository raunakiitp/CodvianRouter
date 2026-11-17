// Route-guard tiers for Codvian Router's admin/management routes.
//
// Every management route is classified into one of three static tiers,
// evaluated before any other auth branch runs (the same shape as
// OmniRoute's route-guard tiers in its ROUTE_GUARD_TIERS doc — the
// pattern was studied there, the code here is original):
//
//   - Tier 1 LOCAL_ONLY — routes that spawn processes, execute runtime
//     code, or change the system. Reachable ONLY from loopback, checked
//     BEFORE auth runs, by real TCP peer address (RemoteAddr) — never
//     by Host header or X-Forwarded-For. A leaked credential over a
//     non-loopback connection must still not reach a spawn.
//   - Tier 2 ALWAYS_PROTECTED — destructive routes. They always require
//     gateway-key auth, even if auth were ever relaxed for Tier 3.
//   - Tier 3 MANAGEMENT — default: auth required unless explicitly off
//     (the dashboard feeds are explicitly off: they carry no secrets).
//
// Two deliberate differences from OmniRoute, both stricter:
//   - No manage-scope bypass: there is no carve-out that lets a
//     non-loopback caller with a privileged key reach a LOCAL_ONLY
//     route. Strict loopback, no exceptions.
//   - Loopback alone is not enough for Tier 1: after the loopback
//     check passes, a valid gateway key is still required (except
//     bootstrap, where no key exists yet by definition).
//
// The classification lives in guardRouteTable, the single source of
// truth. Server.register looks every route up in the table and applies
// the wrappers for its tier; registering a route with no table entry
// panics at startup, so a new route cannot silently land unclassified.
// TestRouteTableSpawnCapableAreLocalOnly walks the table and fails if
// any spawn-capable route is not LOCAL_ONLY.
package server

import (
	"context"
	"net"
	"net/http"
	"strings"
	"time"
)

// GuardTier is one of the three route-guard tiers.
type GuardTier string

const (
	// TierLocalOnly is Tier 1: loopback-only, checked before auth.
	TierLocalOnly GuardTier = "LOCAL_ONLY"
	// TierAlwaysProtected is Tier 2: gateway-key auth always required.
	TierAlwaysProtected GuardTier = "ALWAYS_PROTECTED"
	// TierManagement is Tier 3: auth required unless explicitly off.
	TierManagement GuardTier = "MANAGEMENT"
)

// GuardRoute is one classified route: its method + path, its tier,
// whether it can spawn processes / execute runtime code, a short
// human-readable reason, and (Tier 3 only) whether it is explicitly
// exempt from auth.
type GuardRoute struct {
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Tier         GuardTier `json:"tier"`
	SpawnCapable bool      `json:"spawn_capable"`
	Description  string    `json:"description"`
	AuthExempt   bool      `json:"auth_exempt"`
	// Bootstrap marks the fresh-install first-key route: loopback-only
	// like the rest of Tier 1, but it takes no key — no key exists yet.
	Bootstrap bool `json:"bootstrap,omitempty"`
}

// guardRouteTable is the authoritative classification of every route
// the server registers. Add a route here first, then register it;
// register() panics if a route has no entry.
var guardRouteTable = []GuardRoute{
	{Method: "GET", Path: "/healthz", Tier: TierManagement, Description: "Liveness probe — no data, no auth.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard", Tier: TierManagement, Description: "Static dashboard UI — no live data of its own.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/providers", Tier: TierManagement, Description: "Live provider facts (verification, wire, health) — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/auto-route", Tier: TierManagement, Description: "Live auto-router classification for a typed prompt — no provider is called.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/auto-route/last", Tier: TierManagement, Description: "Last real auto/* routing decision, or an honest empty state.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/coalescing", Tier: TierManagement, Description: "Single-flight counters only — no keys, no message content.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/concurrency", Tier: TierManagement, Description: "Adaptive limiter state + TFO probe — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/dashboard/guard", Tier: TierManagement, Description: "Guard-tier table + live guard status — no keys, no secrets.", AuthExempt: true},
	{Method: "GET", Path: "/api/providers", Tier: TierManagement, Description: "Live provider feed — no keys, no base URLs.", AuthExempt: true},
	{Method: "GET", Path: "/metrics", Tier: TierManagement, Description: "Prometheus exposition — gateway-key auth required."},
	{Method: "GET", Path: "/v1/models", Tier: TierManagement, Description: "Model list — gateway-key auth required."},
	{Method: "POST", Path: "/v1/chat/completions", Tier: TierManagement, Description: "Chat completions — gateway-key auth required."},

	// Tier 1 — LOCAL_ONLY (spawn-capable / system-changing).
	{Method: "POST", Path: "/admin/shutdown", Tier: TierLocalOnly, SpawnCapable: true, Description: "Terminates the server process."},
	{Method: "POST", Path: "/admin/service/install", Tier: TierLocalOnly, SpawnCapable: true, Description: "Installs a system service — not implemented yet (501 behind the guard)."},
	{Method: "POST", Path: "/admin/plugin/load", Tier: TierLocalOnly, SpawnCapable: true, Description: "Loads and executes a plugin — not implemented yet (501 behind the guard)."},
	{Method: "POST", Path: "/admin/update", Tier: TierLocalOnly, SpawnCapable: true, Description: "Auto-update (checkout + install) — not implemented yet (501 behind the guard)."},
	{Method: "POST", Path: "/admin/tunnel/start", Tier: TierLocalOnly, SpawnCapable: true, Description: "Installs/spawns a tunnel binary — not implemented yet (501 behind the guard)."},
	{Method: "POST", Path: "/admin/provider/login", Tier: TierLocalOnly, SpawnCapable: true, Description: "Launches a browser for a provider login flow — not implemented yet (501 behind the guard)."},
	{Method: "POST", Path: "/admin/bootstrap", Tier: TierLocalOnly, SpawnCapable: true, Description: "Fresh-install first-key setup — only while no gateway key exists; takes no key because none exists yet.", Bootstrap: true},

	// Tier 2 — ALWAYS_PROTECTED (destructive; auth always required).
	{Method: "POST", Path: "/admin/settings/export", Tier: TierAlwaysProtected, Description: "Exports the full settings snapshot. Secrets are redacted and never exported — provider keys and gateway-key hashes stay on disk."},

	// Tier 3 — MANAGEMENT (auth required unless explicitly off).
	{Method: "GET", Path: "/admin/status", Tier: TierManagement, Description: "Live server status (uptime, provider count, guard state) — gateway-key auth required."},
}

// RouteTable returns a copy of the guard classification table: every
// route the server registers, with its tier. The gate test walks this
// table and fails if any spawn-capable route is not LOCAL_ONLY.
func RouteTable() []GuardRoute {
	out := make([]GuardRoute, len(guardRouteTable))
	copy(out, guardRouteTable)
	return out
}

// findGuardRoute looks a (method, path) pair up in the table.
func findGuardRoute(method, path string) (GuardRoute, bool) {
	for _, r := range guardRouteTable {
		if r.Method == method && r.Path == path {
			return r, true
		}
	}
	return GuardRoute{}, false
}

// isLoopbackPeer reports whether the request arrived over a real
// loopback TCP connection, judged from RemoteAddr — the actual socket
// peer — only. The Host header and X-Forwarded-For are never
// consulted: either can be forged by a caller, and trusting one
// would re-open exactly the remote-code-execution class this tier
// closes. An unparseable peer fails closed (not loopback).
func isLoopbackPeer(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port (e.g. a bare IP in tests): try the whole string.
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// requireLocalOnly is the Tier 1 guard: it runs BEFORE auth. A
// non-loopback peer is refused with 403 LOCAL_ONLY even when it
// presents a valid gateway key — a leaked credential over a tunnel
// must still not reach a spawn. The refusal is logged as the guard
// working as designed, not as an error.
func (s *Server) requireLocalOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackPeer(r) {
			s.localOnlyDenials.Add(1)
			s.logger.Printf("route guard: LOCAL_ONLY route %s %s refused for non-loopback peer %s (guard working as designed)", r.Method, r.URL.Path, r.RemoteAddr)
			writeError(w, http.StatusForbidden, "LOCAL_ONLY: this route is reachable only from loopback")
			return
		}
		next(w, r)
	}
}

// requireAuthAlways is the Tier 2 guard: gateway-key auth is required
// unconditionally. Unlike Tier 3 it does not honour authRelaxed —
// relaxing login must never open a destructive route.
func (s *Server) requireAuthAlways(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid gateway key")
			return
		}
		next(w, r)
	}
}

// requireAuthUnlessRelaxed is the Tier 3 guard: gateway-key auth is
// required unless auth was explicitly relaxed (WithAuthRelaxed).
func (s *Server) requireAuthUnlessRelaxed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authRelaxed {
			next(w, r)
			return
		}
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid gateway key")
			return
		}
		next(w, r)
	}
}

// register wires one route into mux with the wrappers its tier
// requires. The route must be classified in guardRouteTable; an
// unclassified route panics at startup instead of landing unguarded.
func (s *Server) register(mux *http.ServeMux, method, path string, handler http.HandlerFunc) {
	route, ok := findGuardRoute(method, path)
	if !ok {
		panic("server: route " + method + " " + path + " has no guard-tier classification (add it to guardRouteTable first)")
	}
	h := handler
	switch route.Tier {
	case TierLocalOnly:
		// Loopback first, then auth — except bootstrap, which takes
		// no key because no key exists yet.
		if route.Bootstrap {
			h = s.requireLocalOnly(h)
		} else {
			h = s.requireLocalOnly(s.requireAuth(h))
		}
	case TierAlwaysProtected:
		h = s.requireAuthAlways(h)
	case TierManagement:
		if !route.AuthExempt {
			h = s.requireAuthUnlessRelaxed(h)
		}
	}
	mux.HandleFunc(path, h)
}

// ---------------------------------------------------------------------------
// Guard status feed (GET /dashboard/guard).
// ---------------------------------------------------------------------------

// guardTierView is one tier as the dashboard renders it.
type guardTierView struct {
	Tier        GuardTier    `json:"tier"`
	Description string       `json:"description"`
	Routes      []GuardRoute `json:"routes"`
}

// guardStatus is the live guard state reported by GET /dashboard/guard.
type guardStatus struct {
	LoopbackOnlyEnforced bool   `json:"loopback_only_enforced"`
	BootstrapPending     bool   `json:"bootstrap_pending"`
	AuthRelaxed          bool   `json:"auth_relaxed"`
	LocalOnlyDenials     uint64 `json:"local_only_denials"`
	Listen               string `json:"listen"`
}

// guardResponse is the GET /dashboard/guard payload: the three tiers
// with the routes in each, plus the live guard status. It carries no
// secrets — only route paths, tiers and counters.
type guardResponse struct {
	Tiers  []guardTierView `json:"tiers"`
	Status guardStatus     `json:"status"`
}

var guardTierDescriptions = map[GuardTier]string{
	TierLocalOnly:       "Routes that spawn processes, execute runtime code, or change the system. Loopback-only, checked before auth — a leaked key over a non-loopback connection still cannot reach a spawn.",
	TierAlwaysProtected: "Destructive routes. Gateway-key auth is always required, even if auth were ever relaxed for Tier 3.",
	TierManagement:      "Default tier. Auth required unless explicitly off (the dashboard feeds are explicitly off — they carry no secrets).",
}

// handleGuard serves GET /dashboard/guard: the guard-tier table plus
// live status (loopback enforcement, bootstrap state, denial count).
// It is intentionally NOT auth-gated, like the other dashboard feeds:
// the payload carries no secrets.
func (s *Server) handleGuard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	tiers := []guardTierView{
		{Tier: TierLocalOnly, Description: guardTierDescriptions[TierLocalOnly], Routes: []GuardRoute{}},
		{Tier: TierAlwaysProtected, Description: guardTierDescriptions[TierAlwaysProtected], Routes: []GuardRoute{}},
		{Tier: TierManagement, Description: guardTierDescriptions[TierManagement], Routes: []GuardRoute{}},
	}
	byTier := map[GuardTier]*[]GuardRoute{
		TierLocalOnly:       &tiers[0].Routes,
		TierAlwaysProtected: &tiers[1].Routes,
		TierManagement:      &tiers[2].Routes,
	}
	for _, route := range guardRouteTable {
		*byTier[route.Tier] = append(*byTier[route.Tier], route)
	}
	listen := s.listenAddr
	if listen == "" {
		listen = "unknown"
	}
	writeJSON(w, http.StatusOK, guardResponse{Tiers: tiers, Status: guardStatus{
		LoopbackOnlyEnforced: true,
		BootstrapPending:     s.bootstrapPending.Load(),
		AuthRelaxed:          s.authRelaxed,
		LocalOnlyDenials:     s.localOnlyDenials.Load(),
		Listen:               listen,
	}})
}

// ---------------------------------------------------------------------------
// Admin routes.
// ---------------------------------------------------------------------------

// BootstrapIssuer issues the first gateway key during fresh-install
// bootstrap. It returns the raw key (shown to the operator exactly
// once); the stored form is only ever a hash (see config.IssueKey /
// AddGatewayKey). The production issuer (main.go) persists the key to
// the config file.
type BootstrapIssuer func(ctx context.Context) (rawKey string, err error)

// handleBootstrap serves POST /admin/bootstrap: fresh-install first-key
// setup. It is loopback-only by real TCP peer (the Tier 1 wrapper
// enforces that before this handler runs) and only while no gateway
// key exists yet. A second call answers 409; with no issuer wired it
// answers 503. On success the key is returned once and bootstrap
// flips to complete.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.bootstrapPending.Load() {
		writeError(w, http.StatusConflict, "bootstrap already complete: a gateway key already exists")
		return
	}
	if s.bootstrapIssuer == nil {
		writeError(w, http.StatusServiceUnavailable, "bootstrap issuer is not configured")
		return
	}
	raw, err := s.bootstrapIssuer(r.Context())
	if err != nil {
		s.logger.Printf("route guard: bootstrap key issue failed: %v", err)
		writeServerError(w, http.StatusInternalServerError, "could not issue gateway key")
		return
	}
	s.bootstrapPending.Store(false)
	writeJSON(w, http.StatusCreated, map[string]any{
		"gateway_key": raw,
		"note":        "Save this key now — it is shown once and cannot be recovered. Use it as: Authorization: Bearer <key>.",
	})
}

// handleShutdown serves POST /admin/shutdown (Tier 1): it answers 202
// and then invokes the shutdown hook (wired by main.go to gracefully
// stop the HTTP server). The hook runs after the response is written,
// from its own goroutine, so the 202 always reaches the caller.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.shutdownHook == nil {
		writeError(w, http.StatusServiceUnavailable, "shutdown hook is not configured")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	go s.shutdownHook()
}

// notImplementedAdmin is the handler for Tier 1 routes whose feature
// does not exist yet (service install, plugin load, auto-update,
// tunnel start, browser login flow). The guard in front of them is
// real and enforced; the 501 is honest — the route is classified
// local-only today so the feature lands guarded, not exposed.
func (s *Server) notImplementedAdmin(feature string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		writeError(w, http.StatusNotImplemented, feature+" is not implemented yet")
	}
}

// settingsExportProvider is one provider row in the settings export:
// identity and wiring facts only — never a base URL (it can carry
// embedded credentials) and never a key.
type settingsExportProvider struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	WireFormat   string   `json:"wire_format"`
	CatalogID    string   `json:"catalog_id,omitempty"`
	Verification string   `json:"verification,omitempty"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
}

// handleSettingsExport serves POST /admin/settings/export (Tier 2):
// the full settings snapshot with secrets redacted. Provider keys
// and gateway-key hashes are never exported — the response says so
// explicitly instead of silently omitting them. With no config loaded
// it answers 503 (honest "unavailable", not an empty export).
func (s *Server) handleSettingsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg := s.cfg
	if cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "settings export unavailable: no config loaded")
		return
	}
	providers := []settingsExportProvider{}
	for _, p := range cfg.Providers {
		models := []string{}
		for _, m := range p.Models {
			models = append(models, m.ID)
		}
		providers = append(providers, settingsExportProvider{
			ID: p.ID, Name: p.Name, WireFormat: p.WireFormat,
			CatalogID: p.CatalogID, Verification: p.Verification,
			Models: models, Enabled: p.Enabled,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":           cfg.Version,
		"listen":            cfg.Listen,
		"budget_usd":        cfg.BudgetUSD,
		"fail_closed":       cfg.FailClosed,
		"telemetry_enabled": cfg.TelemetryEnabled,
		"gateway_key_count": len(cfg.GatewayKeys),
		"providers":         providers,
		"secrets":           "redacted — provider keys and gateway-key hashes are never exported",
		"exported_at":       time.Now().UTC().Format(time.RFC3339),
	})
}

// handleAdminStatus serves GET /admin/status (Tier 3): live server
// status — uptime, provider count, guard state. No keys, no secrets.
func (s *Server) handleAdminStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers := 0
	if s.dashboardProviders != nil {
		providers = len(s.dashboardProviders.DashboardProviders())
	}
	listen := s.listenAddr
	if listen == "" {
		listen = "unknown"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":             "ok",
		"uptime_seconds":     int64(time.Since(s.startedAt).Seconds()),
		"providers":          providers,
		"auth_relaxed":       s.authRelaxed,
		"bootstrap_pending":  s.bootstrapPending.Load(),
		"local_only_denials": s.localOnlyDenials.Load(),
		"listen":             listen,
	})
}
