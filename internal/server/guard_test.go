package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// doReqPeer is doReq with an explicit TCP peer address: httptest
// otherwise stamps RemoteAddr as 192.0.2.1 (non-loopback), which is
// exactly what the guard tests want to control per request.
func doReqPeer(t *testing.T, srv *Server, method, path, body, key, remoteAddr string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = remoteAddr
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const (
	loopbackPeer    = "127.0.0.1:54321"
	loopbackPeerV6  = "[::1]:54321"
	nonLoopbackPeer = "192.168.1.5:54321"
)

// TestRouteTableSpawnCapableAreLocalOnly is the build-time/test-time
// gate: it walks the route table and FAILS if any spawn-capable route
// is not classified LOCAL_ONLY. It also fails on duplicate routes and
// on any route registered in New() that is missing from the table.
func TestRouteTableSpawnCapableAreLocalOnly(t *testing.T) {
	table := RouteTable()
	if len(table) == 0 {
		t.Fatal("route table is empty")
	}
	seen := make(map[string]bool, len(table))
	for _, r := range table {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("duplicate route in table: %s", key)
		}
		seen[key] = true
		if r.Tier != TierLocalOnly && r.Tier != TierAlwaysProtected && r.Tier != TierManagement {
			t.Errorf("route %s has unknown tier %q", key, r.Tier)
		}
		if r.SpawnCapable && r.Tier != TierLocalOnly {
			t.Errorf("spawn-capable route %s is tier %s, not LOCAL_ONLY", key, r.Tier)
		}
		if r.Description == "" {
			t.Errorf("route %s has no description", key)
		}
	}
	// Every route New() registers must be classified. This list
	// mirrors New()'s register() calls; adding a route to New()
	// without a table entry breaks register() first (it panics),
	// and forgetting one here breaks this test.
	expected := []string{
		"GET /healthz", "GET /dashboard", "GET /dashboard/providers",
		"GET /dashboard/auto-route", "GET /dashboard/auto-route/last",
		"GET /dashboard/coalescing", "GET /dashboard/concurrency",
		"GET /dashboard/pools",
		"GET /dashboard/guard", "GET /dashboard/audit", "GET /dashboard/mcp", "GET /dashboard/overview",
		"GET /api/mcp/audit", "GET /api/mcp/status", "POST /api/mcp/status",
		"GET /api/mcp/sse", "POST /api/mcp/sse", "POST /api/mcp/stream", "GET /api/mcp/stream", "DELETE /api/mcp/stream/{id}",
		"GET /dashboard/models", "POST /dashboard/models/refresh", "GET /dashboard/memstats", "GET /dashboard/catalog-audit", "GET /dashboard/logs",
		"GET /dashboard/usage", "GET /dashboard/keys",
		"POST /dashboard/auth/setup", "POST /dashboard/auth/login", "POST /dashboard/auth/logout", "POST /dashboard/auth/change",
		"GET /dashboard/guardrails",
		"POST /dashboard/guardrails", "GET /dashboard/strategies",
		"POST /dashboard/strategies", "GET /dashboard/cooldown",
		"POST /dashboard/cooldown", "GET /dashboard/trace", "GET /dashboard/context-fit", "GET /dashboard/prices",
		"GET /dashboard/benchmark", "GET /dashboard/benchmark/history", "GET /dashboard/release-verify", "GET /dashboard/readiness", "GET /dashboard/streaming", "GET /dashboard/key-budgets", "POST /dashboard/key-budgets", "GET /dashboard/quota-pools", "POST /dashboard/quota-pools", "GET /dashboard/quota-schedules", "POST /dashboard/quota-schedules", "GET /dashboard/internal-budgets", "POST /dashboard/internal-budgets", "GET /dashboard/teams", "POST /dashboard/teams", "POST /dashboard/teams/{id}/members", "GET /dashboard/request-budgets", "GET /dashboard/reasoning", "GET /dashboard/responses", "GET /dashboard/responses/tree", "GET /dashboard/groups", "GET /dashboard/stream-lifetime", "GET /dashboard/headless", "GET /dashboard/plugins", "GET /dashboard/verification", "GET /openapi.json",
		"GET /dashboard/feedback", "POST /dashboard/feedback", "GET /dashboard/learned", "GET /dashboard/code-outcomes",
		"GET /dashboard/intent-tiers", "POST /dashboard/intent-tiers", "GET /dashboard/suggestions", "POST /dashboard/suggestions",
		"GET /api/providers", "POST /api/providers", "DELETE /api/providers/{id}", "POST /api/providers/{id}/keys", "DELETE /api/providers/{id}/keys/{keyId}", "POST /api/providers/{id}/keys/{keyId}/role", "POST /api/providers/{id}/keys/{keyId}/rotate", "POST /api/providers/{id}/enabled", "GET /api/catalog", "GET /api/usage/budget", "POST /api/usage/budget", "GET /api/usage/om-usage", "GET /api/cache/stats", "GET /api/telemetry/model-latency-stats", "GET /metrics",
		"GET /api/analytics/summary", "GET /api/analytics/by-provider", "GET /api/analytics/by-model", "GET /api/analytics/costs", "GET /api/analytics/compression", "GET /api/analytics/diversity", "GET /api/analytics/auto-routing", "GET /api/analytics/combo-health", "GET /api/resilience", "GET /api/resilience/settings", "PATCH /api/resilience/settings", "POST /api/resilience/breakers/reset", "POST /api/resilience/cooldowns", "DELETE /api/resilience/cooldowns/{model...}", "GET /api/logs", "POST /api/guardrails/test", "GET /api/fallback/chains", "POST /api/fallback/chains", "DELETE /api/fallback/chains",
		"GET /api/teams", "POST /api/teams", "GET /api/teams/{id}", "PATCH /api/teams/{id}", "DELETE /api/teams/{id}", "GET /api/quota-pools", "POST /api/quota-pools", "GET /api/quota-pools/{id}", "PATCH /api/quota-pools/{id}", "DELETE /api/quota-pools/{id}",
		"POST /api/webhooks", "GET /api/webhooks", "GET /api/webhooks/{id}", "PUT /api/webhooks/{id}", "DELETE /api/webhooks/{id}",
		"GET /api/pricing", "PATCH /api/pricing", "DELETE /api/cache",
		"GET /v1/models", "POST /v1/chat/completions", "POST /a2a", "GET /api/a2a/status", "GET /.well-known/agent.json", "POST /v1/embeddings", "POST /v1/multimodal-embeddings", "POST /v1/rerank", "POST /v1/messages", "POST /v1/responses", "GET /v1/responses/{id}", "GET /v1/responses", "DELETE /v1/responses/{id}", "GET /v1/me/status", "GET /v1/me/sessions", "GET /v1/auto-combo/{channel}/candidates",
		"GET /v1/models/query", "GET /v1/models/health", "GET /v1/models/{model...}", "POST /v1/providers/{provider}/chat/completions", "GET /v1/providers/{provider}/models", "GET /v1/providers/{provider}/status", "GET /v1beta/models", "POST /v1beta/models/{rest...}",
		"POST /v1/completions", "POST /v1/relay/chat/completions", "GET /v1/classify", "GET /v1/explain/routing", "GET /v1/segment", "POST /v1/images/generations", "POST /v1/images/edits",
		"POST /v1/audio/speech", "POST /v1/audio/text-to-speech", "POST /v1/audio/text-to-speech/{voiceId}", "POST /v1/audio/transcriptions", "POST /v1/audio/speech-to-text", "POST /v1/audio/translations",
		"POST /v1/moderations", "POST /v1/search", "POST /v1/web/fetch", "POST /v1/ocr", "POST /v1/videos/generations", "GET /v1/videos/{id}", "POST /v1/music/generations",
		"POST /v1/messages/count_tokens", "GET /v1/responses/{id}/input_items", "GET /v1/responses/input_tokens",
		"POST /v1/files", "GET /v1/files", "GET /v1/files/{id}", "DELETE /v1/files/{id}", "GET /v1/files/{id}/content",
		"POST /v1/batches", "GET /v1/batches", "GET /v1/batches/{id}", "POST /v1/batches/{id}/cancel", "DELETE /v1/batches/{id}", "DELETE /v1/batches/delete-completed",
		"GET /v1/me/sessions/{id}", "POST /v1/quotas/check",
		"GET /v1/combos", "POST /v1/combos", "GET /v1/combos/{id}", "PATCH /v1/combos/{id}", "DELETE /v1/combos/{id}", "GET /v1/combos/export", "GET /v1/combos/{id}/export", "POST /v1/combos/import", "POST /v1/combos/{id}/duplicate", "POST /v1/combos/{id}/test-run", "POST /v1/combos/{id}/enable", "POST /v1/combos/{id}/disable",
		"POST /admin/shutdown", "POST /admin/restart", "POST /admin/service/install",
		"POST /admin/plugin/load", "POST /admin/update",
		"POST /admin/tunnel/start", "POST /admin/tunnel/stop",
		"GET /admin/tunnel/status", "POST /admin/provider/login",
		"POST /admin/bootstrap", "POST /admin/settings/export",
		"POST /admin/db/export", "POST /admin/db/wipe/arm",
		"POST /admin/db/wipe", "POST /admin/db/import",
		"POST /admin/keys/revoke-all", "POST /admin/keys/{id}", "DELETE /admin/keys/{id}", "POST /admin/keys/{id}/rotate", "POST /admin/config/reset", "POST /admin/keys", "POST /admin/keys/import", "GET /admin/key-templates",
		"POST /oauth/clients", "GET /oauth/clients", "DELETE /oauth/clients/{id}", "GET /oauth/authorize", "POST /oauth/token", "POST /oauth/revoke",
		"POST /admin/cache/flush",
		"GET /admin/status",
		// B8 Dashboard Part 2 routes
		"GET /api/features", "PATCH /api/features",
		"GET /api/settings/cors", "PATCH /api/settings/cors",
		"GET /api/settings/blocked-providers", "PATCH /api/settings/blocked-providers",
		"GET /api/settings/ai", "PATCH /api/settings/ai",
		"GET /api/settings/modality", "PATCH /api/settings/modality",
		"POST /api/pricing/sync", "GET /api/pricing/sync/status",
		"GET /api/settings/transform", "PUT /api/settings/transform",
		"GET /api/settings/sidebar", "PUT /api/settings/sidebar",
		"GET /api/settings/advanced", "PATCH /api/settings/advanced",
		"DELETE /api/lkgp/cache", "GET /api/routing/strategies",
		"GET /api/settings/stickiness", "PATCH /api/settings/stickiness",
		"GET /api/settings/switches", "PATCH /api/settings/switches",
		"GET /api/settings/response-validation", "PUT /api/settings/response-validation",
		"DELETE /api/cache/{model}",
		"POST /admin/keys/{id}/disable", "POST /admin/keys/{id}/enable", "POST /admin/keys/{id}/revoke",
		"PATCH /admin/keys/{id}/budget", "PATCH /admin/keys/{id}/budget-warn", "PATCH /admin/keys/{id}/budget-reset",
		"GET /admin/budget-templates", "POST /admin/budget-templates", "POST /admin/budget-templates/{id}/apply",
		"GET /api/keys/{id}/devices", "POST /admin/keys/{id}/lease",
		"GET /admin/management-tokens", "POST /admin/management-tokens", "GET /admin/management-tokens/{id}", "DELETE /admin/management-tokens/{id}",
		"GET /admin/key-pools", "POST /admin/key-pools", "GET /admin/key-pools/{id}", "PATCH /admin/key-pools/{id}", "DELETE /admin/key-pools/{id}",
		"GET /dashboard/keys/{id}/routing", "GET /dashboard/keys/{id}/access",
		"GET /api/settings/ip-filter", "PATCH /api/settings/ip-filter",
		"GET /api/analytics/cost-explorer", "GET /api/analytics/spend-projection", "GET /api/analytics/budget-table",
		"GET /api/analytics/cost-by-key", "GET /api/analytics/cost-by-account", "GET /api/analytics/weekly-pattern",
		"GET /api/analytics/heatmap", "GET /api/analytics/utilization", "GET /api/analytics/search",
		"GET /api/analytics/free-rankings", "GET /api/analytics/scoring-inspector", "GET /api/analytics/candidates",
		"GET /api/logs/proxy", "GET /api/logs/console", "GET /api/logs/timeline/{id}", "GET /api/logs/activity",
		"GET /api/logs/conversations", "GET /api/logs/compression", "GET /api/logs/audit", "GET /api/logs/export",
		"GET /api/logs/destinations", "POST /api/logs/destinations", "GET /api/logs/destinations/{id}", "PATCH /api/logs/destinations/{id}", "DELETE /api/logs/destinations/{id}",
		"GET /api/combos/{id}/capabilities", "PUT /api/combos/{id}/capabilities",
		"GET /api/settings/combo-tuning", "PATCH /api/settings/combo-tuning",
		"GET /api/settings/scoring-modes", "GET /api/settings/account-fallback", "PATCH /api/settings/account-fallback",
		"GET /api/combos/{id}/studio-cascade",
	}
	if len(expected) != len(table) {
		t.Errorf("route table has %d entries, expected %d (a route was added without updating the table, or vice versa)", len(table), len(expected))
	}
	for _, want := range expected {
		if !seen[want] {
			t.Errorf("route %s is registered but missing from the route table", want)
		}
	}
	// Tier counts, pinned so a reclassification is a deliberate diff.
	// 186 MANAGEMENT = 81 previously pinned + 60 M8 routes (M8.2–M8.20
	// except the four Tier 2 ones below: legacy/completions, images,
	// audio, moderations, media, files, count_tokens, input_items,
	// models detail/query/health, provider-v1, gemini, combos,
	// analytics, resilience/logs/guardrails-test, teams, quota-pools,
	// webhooks GETs, pricing GET) + 6 M8.8 batch routes + 3 M9.7 MCP
	// routes (GET /api/mcp/audit, GET /api/mcp/status,
	// POST /api/mcp/status) + 5 M9.2 MCP transport routes (GET/POST
	// /api/mcp/sse, POST/GET /api/mcp/stream, DELETE
	// /api/mcp/stream/{id}) + 1 M9.8 A2A route (POST /a2a) + 1 M9.9
	// A2A agent card (GET /.well-known/agent.json, public) + 7
	// registration-slice routes (GET /api/a2a/status, GET
	// /dashboard/audit, GET/PATCH /api/resilience/settings, POST
	// /api/resilience/breakers/reset, POST /api/resilience/cooldowns,
	// DELETE /api/resilience/cooldowns/{model...}) + 12 M10 routes
	// (GET/POST/DELETE /api/fallback/chains, GET /admin/key-templates,
	// GET /api/telemetry/model-latency-stats, GET /v1/combos/export,
	// GET /v1/combos/{id}/export, POST /v1/combos/import, POST
	// /v1/combos/{id}/duplicate, POST /v1/combos/{id}/test-run, POST
	// /v1/combos/{id}/enable, POST /v1/combos/{id}/disable)
	// + 4 DASHAUTH routes (POST /dashboard/auth/setup, /login,
	// /logout, /change, B3-04) + 4 analytics routes (GET
	// /api/analytics/compression, /diversity, /auto-routing,
	// /combo-health, B4-17) + 2 OAuth token endpoints (POST
	// /oauth/token, POST /oauth/revoke, B3-07).
	// 29 ALWAYS_PROTECTED = 16 pinned + PATCH /api/pricing and
	// DELETE /api/cache (M8.15) + POST/DELETE /api/webhooks (M8.19)
	// + PUT /api/webhooks/{id} (registration slice) + POST
	// /admin/keys and POST /admin/keys/import (M10) + POST
	// /api/providers/{id}/enabled (critic fix 2026-11-19) + POST
	// /api/providers/{id}/keys/{keyId}/rotate (critic fix 2026-11-19)
	// + 4 OAuth routes (POST/GET/DELETE /oauth/clients + GET
	// /oauth/authorize, B3-07).
	// Phase 0 (2026-11-21): LOCAL_ONLY 9 -> 10 for POST
	// /admin/restart (spawn-capable: it starts a replacement
	// process). MANAGEMENT 250 -> 254 for POST
	// /v1/multimodal-embeddings, POST /v1/audio/text-to-speech,
	// POST /v1/audio/text-to-speech/{voiceId} and POST
	// /v1/web/fetch. The fabricated routes added with them
	// (/v1/systemone, /v1/audio/voices, /v1/voices,
	// /v1/video-bridge/drilldown x2) were removed, not counted.
	counts := map[GuardTier]int{}
	for _, r := range table {
		counts[r.Tier]++
	}
	if counts[TierLocalOnly] != 10 || counts[TierAlwaysProtected] != 49 || counts[TierManagement] != 254 {
		t.Errorf("tier counts = LOCAL_ONLY %d, ALWAYS_PROTECTED %d, MANAGEMENT %d (want 10/49/254)", counts[TierLocalOnly], counts[TierAlwaysProtected], counts[TierManagement])
	}
}

// TestRegisterPanicsOnUnknownRoute proves the fail-fast half of the
// gate: a route with no table entry cannot be registered.
func TestRegisterPanicsOnUnknownRoute(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	defer func() {
		if r := recover(); r == nil {
			t.Error("register() with an unclassified route did not panic")
		}
	}()
	srv.register(http.NewServeMux(), "POST", "/admin/secret-spawn", func(w http.ResponseWriter, r *http.Request) {})
}

func TestIsLoopbackPeer(t *testing.T) {
	cases := []struct {
		remote string
		xff    string
		host   string
		want   bool
	}{
		{"127.0.0.1:1234", "", "", true},
		{"[::1]:1234", "", "", true},
		{"127.0.0.2:1234", "", "", true}, // 127/8 is loopback
		{"127.0.0.1", "", "", true},      // bare IP, no port
		{"192.168.1.5:1234", "", "", false},
		{"10.0.0.1:1234", "", "", false},
		{"8.8.8.8:1234", "", "", false},
		{"", "", "", false},
		{"localhost:1234", "", "", false}, // hostname, not an IP — fail closed
		// Forged headers must never turn a non-loopback peer into one.
		{"192.168.1.5:1234", "127.0.0.1", "", false},
		{"192.168.1.5:1234", "127.0.0.1, 10.0.0.1", "", false},
		{"192.168.1.5:1234", "", "localhost", false},
		{"192.168.1.5:1234", "", "127.0.0.1", false},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = c.remote
		if c.xff != "" {
			req.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.host != "" {
			req.Host = c.host
		}
		if got := isLoopbackPeer(req); got != c.want {
			t.Errorf("isLoopbackPeer(RemoteAddr=%q, XFF=%q, Host=%q) = %v, want %v", c.remote, c.xff, c.host, got, c.want)
		}
	}
	if isLoopbackPeer(nil) {
		t.Error("isLoopbackPeer(nil) = true, want false")
	}
}

// TestLocalOnlyBeforeAuth proves the Tier 1 ordering: a spawn-capable
// route from a non-loopback address is refused 403 even WITH a valid
// key, and even without a key it is 403 (not 401) — the guard runs
// before auth. From loopback the same route works with a key and
// still needs one without it.
func TestLocalOnlyBeforeAuth(t *testing.T) {
	var hookCalls atomic.Int32
	srv := newTestServer(t, &fakeEngine{},
		WithShutdownHook(func() { hookCalls.Add(1) }))

	// Non-loopback + valid key: 403 LOCAL_ONLY, hook never runs.
	rec := doReqPeer(t, srv, "POST", "/admin/shutdown", "", "test-key-123", nonLoopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback + valid key: status %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "LOCAL_ONLY") {
		t.Errorf("non-loopback 403 body %q does not name LOCAL_ONLY", rec.Body.String())
	}
	// Non-loopback + NO key: still 403, not 401 — guard before auth.
	rec = doReqPeer(t, srv, "POST", "/admin/shutdown", "", "", nonLoopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-loopback + no key: status %d, want 403 (guard runs before auth)", rec.Code)
	}
	// Loopback + no key: 401 — loopback alone is not enough.
	rec = doReqPeer(t, srv, "POST", "/admin/shutdown", "", "", loopbackPeer)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("loopback + no key: status %d, want 401", rec.Code)
	}
	// Loopback + valid key: 202, and the shutdown hook runs.
	rec = doReqPeer(t, srv, "POST", "/admin/shutdown", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("loopback + valid key: status %d, want 202", rec.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for hookCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if hookCalls.Load() != 1 {
		t.Errorf("shutdown hook ran %d times, want 1", hookCalls.Load())
	}
	// IPv6 loopback behaves the same.
	rec = doReqPeer(t, srv, "POST", "/admin/shutdown", "", "test-key-123", loopbackPeerV6)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("::1 + valid key: status %d, want 202", rec.Code)
	}
}

// TestShutdownWithoutHook: with no hook wired the route must not
// claim a shutdown it cannot perform — 503, not a fake 202.
func TestShutdownWithoutHook(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "POST", "/admin/shutdown", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown without hook: status %d, want 503", rec.Code)
	}
}

// TestLocalOnlyNotImplementedRoutes: the not-yet-built Tier 1 features
// are guarded for real (403 off-host) and honest on-host (501, not a
// fake success).
func TestLocalOnlyNotImplementedRoutes(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	// /admin/update is a real update check now (TestAdminUpdateCheck).
	paths := []string{"/admin/service/install", "/admin/plugin/load", "/admin/provider/login"}
	for _, p := range paths {
		rec := doReqPeer(t, srv, "POST", p, "", "test-key-123", nonLoopbackPeer)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s non-loopback + key: status %d, want 403", p, rec.Code)
		}
		rec = doReqPeer(t, srv, "POST", p, "", "test-key-123", loopbackPeer)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("POST %s loopback + key: status %d, want 501 (guard passed, feature not built)", p, rec.Code)
		}
	}
}

// TestAlwaysProtectedIgnoresRelaxed: with auth relaxed, a Tier 3 route
// opens up but the Tier 2 settings export still demands a key — and
// when it answers, no secret material is in the payload.
func TestAlwaysProtectedIgnoresRelaxed(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	issuedKey, issuedRaw := mustIssueKey(t)
	if err := cfg.AddGatewayKey(issuedKey); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithAuthRelaxed(true), WithConfig(cfg))

	// Tier 3 with relaxed auth: no key still works.
	rec := doReqPeer(t, srv, "GET", "/admin/status", "", "", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("relaxed Tier 3 without key: status %d, want 200", rec.Code)
	}
	// Tier 2 with relaxed auth and NO key: still 401.
	rec = doReqPeer(t, srv, "POST", "/admin/settings/export", "", "", loopbackPeer)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("relaxed Tier 2 without key: status %d, want 401", rec.Code)
	}
	// Tier 2 with the real key + typed phrase: 200, secrets redacted.
	key := issuedRaw
	rec = doReqPeer(t, srv, "POST", "/admin/settings/export", `{"confirm":"EXPORT SETTINGS"}`, key, loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("Tier 2 with key: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "redacted") {
		t.Errorf("settings export does not say secrets are redacted: %s", body)
	}
	for _, leaked := range []string{`"base_url"`, `"api_key"`, "sha256"} {
		if strings.Contains(body, leaked) {
			t.Errorf("settings export contains %q: %s", leaked, body)
		}
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("settings export is not JSON: %v", err)
	}
	if payload["gateway_key_count"] != float64(1) {
		t.Errorf("gateway_key_count = %v, want 1", payload["gateway_key_count"])
	}
}

func mustIssueKey(t *testing.T) (config.GatewayKey, string) {
	t.Helper()
	// The helper key is an operator key: it carries the admin
	// scope (M1.1), so tests that exercise Tier 2 admin routes
	// with it keep passing. Tests that prove scope refusal issue
	// their own narrowly-scoped keys instead.
	k, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	return k, raw
}

// TestManagementRequiresAuthWhenNotRelaxed: the default Tier 3
// behaviour — no key, no access.
func TestManagementRequiresAuthWhenNotRelaxed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "GET", "/admin/status", "", "", loopbackPeer)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Tier 3 without key: status %d, want 401", rec.Code)
	}
	rec = doReqPeer(t, srv, "GET", "/admin/status", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("Tier 3 with key: status %d, want 200", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("admin status is not JSON: %v", err)
	}
	if payload["status"] != "ok" || payload["auth_relaxed"] != false {
		t.Errorf("admin status payload = %v", payload)
	}
}

// TestSettingsExportWithoutConfig answers 503, not an empty export.
func TestSettingsExportWithoutConfig(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReqPeer(t, srv, "POST", "/admin/settings/export", `{"confirm":"EXPORT SETTINGS"}`, "test-key-123", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings export without config: status %d, want 503", rec.Code)
	}
}

// TestSettingsExportRequiresTypedConfirm proves the operator-
// credential second check on the settings export: a valid gateway
// key alone is not enough — the typed phrase EXPORT SETTINGS must
// be present, and a wrong phrase is refused. The phrase is distinct
// from the DB export's EXPORT, so a body captured for one export
// cannot authorise the other.
func TestSettingsExportRequiresTypedConfirm(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	issuedKey, issuedRaw := mustIssueKey(t)
	if err := cfg.AddGatewayKey(issuedKey); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	cases := []struct {
		name string
		body string
		want int
	}{
		{"no body", "", http.StatusBadRequest},
		{"wrong phrase", `{"confirm":"EXPORT"}`, http.StatusBadRequest},
		{"db-export phrase is not the settings phrase", `{"confirm":"EXPORT SETTINGS "}`, http.StatusBadRequest},
		{"correct phrase", `{"confirm":"EXPORT SETTINGS"}`, http.StatusOK},
	}
	for _, tc := range cases {
		rec := doReqPeer(t, srv, "POST", "/admin/settings/export", tc.body, issuedRaw, loopbackPeer)
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// TestBootstrapOneShotToken (AK-2): the first-key claim needs the
// single-use bootstrap token printed to stdout at first boot.
// Without it, or with a wrong one, the claim is refused 403 and
// the issuer never runs; a wrong token does not consume the real
// one. With the right token the key is issued once, the response
// never contains the token, and the token is then dead (reuse is
// 409, and BootstrapToken() is empty).
func TestBootstrapOneShotToken(t *testing.T) {
	issued := 0
	srv := newTestServer(t, &fakeEngine{},
		WithBootstrapPending(true),
		WithBootstrapIssuer(func(ctx context.Context) (string, error) {
			issued++
			return "cr_bootstrap_key_1", nil
		}))
	token := srv.BootstrapToken()
	if token == "" {
		t.Fatal("pending server issued no bootstrap token")
	}
	// No token: 403, issuer not run.
	rec := doReqPeer(t, srv, "POST", "/admin/bootstrap", `{}`, "", loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap without token: status %d, want 403", rec.Code)
	}
	if issued != 0 {
		t.Fatalf("bootstrap issuer ran %d times for a tokenless request, want 0", issued)
	}
	// Wrong token: 403, issuer not run, real token still alive.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"not-the-token"}`, "", loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with wrong token: status %d, want 403", rec.Code)
	}
	if issued != 0 {
		t.Fatalf("bootstrap issuer ran %d times for a wrong-token request, want 0", issued)
	}
	// Right token: 201 with the key, once; the response must
	// never contain the one-shot token itself.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+token+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap with token: status %d, want 201", rec.Code)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Errorf("bootstrap response leaks the one-shot token")
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("bootstrap response is not JSON: %v", err)
	}
	if payload["gateway_key"] != "cr_bootstrap_key_1" {
		t.Errorf("bootstrap gateway_key = %v, want the issued key", payload["gateway_key"])
	}
	if issued != 1 {
		t.Fatalf("bootstrap issuer ran %d times, want 1", issued)
	}
	// Single-use: the token is consumed — the accessor is empty
	// and reusing the same token is refused (bootstrap is
	// complete now, so 409, and the issuer stays at 1).
	if got := srv.BootstrapToken(); got != "" {
		t.Errorf("BootstrapToken() after use = %q, want empty (single-use)", got)
	}
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+token+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusConflict {
		t.Fatalf("bootstrap reuse of spent token: status %d, want 409", rec.Code)
	}
	if issued != 1 {
		t.Errorf("bootstrap issuer ran %d times after completion, want 1", issued)
	}
}

// TestBootstrapTokenRotatesWhenPendingAgain: when bootstrap flips
// back to pending (revoke-all / config reset), a fresh token is
// issued and the old one is dead — otherwise the first key could
// never be claimed again after a wipe.
func TestBootstrapTokenRotatesWhenPendingAgain(t *testing.T) {
	issued := 0
	srv := newTestServer(t, &fakeEngine{},
		WithBootstrapPending(true),
		WithBootstrapIssuer(func(ctx context.Context) (string, error) {
			issued++
			return "cr_bootstrap_key_1", nil
		}))
	old := srv.BootstrapToken()
	if old == "" {
		t.Fatal("pending server issued no bootstrap token")
	}
	srv.resetBootstrapToken()
	next := srv.BootstrapToken()
	if next == "" || next == old {
		t.Fatalf("rotated token = %q, want a fresh non-empty token unlike %q", next, old)
	}
	// Old token is dead.
	rec := doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+old+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap with rotated-out token: status %d, want 403", rec.Code)
	}
	if issued != 0 {
		t.Fatalf("bootstrap issuer ran %d times for a dead-token request, want 0", issued)
	}
	// New token claims the key.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+next+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap with rotated token: status %d, want 201", rec.Code)
	}
	if issued != 1 {
		t.Fatalf("bootstrap issuer ran %d times, want 1", issued)
	}
}

// TestBootstrapLoopbackOnly: first-key setup is loopback-only by real
// TCP peer, works once from loopback, then refuses with 409. A forged
// X-Forwarded-For does not help a non-loopback caller.
func TestBootstrapLoopbackOnly(t *testing.T) {
	issued := 0
	srv := newTestServer(t, &fakeEngine{},
		WithBootstrapPending(true),
		WithBootstrapIssuer(func(ctx context.Context) (string, error) {
			issued++
			return "cr_bootstrap_key_1", nil
		}))

	// Non-loopback, even with a forged X-Forwarded-For: 403.
	req := httptest.NewRequest("POST", "/admin/bootstrap", nil)
	req.RemoteAddr = nonLoopbackPeer
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("bootstrap non-loopback + forged XFF: status %d, want 403", rec.Code)
	}
	if issued != 0 {
		t.Fatalf("bootstrap issuer ran %d times for a refused request, want 0", issued)
	}
	// Loopback, with the one-shot token: 201 with the key, once.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+srv.BootstrapToken()+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bootstrap loopback: status %d, want 201", rec.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("bootstrap response is not JSON: %v", err)
	}
	if payload["gateway_key"] != "cr_bootstrap_key_1" {
		t.Errorf("bootstrap gateway_key = %v, want the issued key", payload["gateway_key"])
	}
	if issued != 1 {
		t.Fatalf("bootstrap issuer ran %d times, want 1", issued)
	}
	// Second call: 409, issuer not run again.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", "", "", loopbackPeer)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second bootstrap: status %d, want 409", rec.Code)
	}
	if issued != 1 {
		t.Errorf("bootstrap issuer ran %d times after completion, want 1", issued)
	}
}

// TestBootstrapWithoutIssuer: pending but no issuer wired — 503, and
// bootstrap stays pending (a later retry can still succeed).
func TestBootstrapWithoutIssuer(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithBootstrapPending(true))
	rec := doReqPeer(t, srv, "POST", "/admin/bootstrap", `{"bootstrap_token":"`+srv.BootstrapToken()+`"}`, "", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("bootstrap without issuer: status %d, want 503", rec.Code)
	}
	// Not pending at all: 409 even from loopback.
	srv2 := newTestServer(t, &fakeEngine{}, WithBootstrapPending(false))
	rec = doReqPeer(t, srv2, "POST", "/admin/bootstrap", "", "", loopbackPeer)
	if rec.Code != http.StatusConflict {
		t.Fatalf("bootstrap when complete: status %d, want 409", rec.Code)
	}
}

// TestGuardFeed: the dashboard feed lists all three tiers with their
// routes and reports live status, including the denial counter.
func TestGuardFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{},
		WithListenAddr("127.0.0.1:20128"), WithBootstrapPending(true))
	// One denied LOCAL_ONLY attempt, so the counter is a real 1.
	rec := doReqPeer(t, srv, "POST", "/admin/shutdown", "", "test-key-123", nonLoopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup denial: status %d, want 403", rec.Code)
	}
	rec = doReqPeer(t, srv, "GET", "/dashboard/guard", "", "", loopbackPeer)
	if rec.Code != http.StatusOK {
		t.Fatalf("guard feed: status %d, want 200", rec.Code)
	}
	var resp guardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("guard feed is not JSON: %v", err)
	}
	if len(resp.Tiers) != 3 {
		t.Fatalf("guard feed has %d tiers, want 3", len(resp.Tiers))
	}
	wantTiers := []GuardTier{TierLocalOnly, TierAlwaysProtected, TierManagement}
	for i, want := range wantTiers {
		if resp.Tiers[i].Tier != want {
			t.Errorf("tier[%d] = %s, want %s", i, resp.Tiers[i].Tier, want)
		}
		if resp.Tiers[i].Description == "" {
			t.Errorf("tier %s has no description", want)
		}
		if len(resp.Tiers[i].Routes) == 0 {
			t.Errorf("tier %s lists no routes", want)
		}
	}
	total := len(resp.Tiers[0].Routes) + len(resp.Tiers[1].Routes) + len(resp.Tiers[2].Routes)
	if total != len(RouteTable()) {
		t.Errorf("guard feed lists %d routes, table has %d", total, len(RouteTable()))
	}
	st := resp.Status
	if !st.LoopbackOnlyEnforced {
		t.Error("guard status loopback_only_enforced = false, want true")
	}
	if !st.BootstrapPending {
		t.Error("guard status bootstrap_pending = false, want true")
	}
	if st.AuthRelaxed {
		t.Error("guard status auth_relaxed = true, want false")
	}
	if st.LocalOnlyDenials != 1 {
		t.Errorf("guard status local_only_denials = %d, want 1", st.LocalOnlyDenials)
	}
	if st.Listen != "127.0.0.1:20128" {
		t.Errorf("guard status listen = %q, want 127.0.0.1:20128", st.Listen)
	}
	// No listen address configured: honest "unknown".
	srv2 := newTestServer(t, &fakeEngine{})
	rec = doReqPeer(t, srv2, "GET", "/dashboard/guard", "", "", loopbackPeer)
	var resp2 guardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("guard feed (no listen) is not JSON: %v", err)
	}
	if resp2.Status.Listen != "unknown" {
		t.Errorf("guard status listen without config = %q, want \"unknown\"", resp2.Status.Listen)
	}
}

// TestSpawnCapablePatternsMatchTable checks the regex companion
// against the table in BOTH directions, so neither can drift:
//
//  1. Every table route whose path matches a spawn-capable pattern
//     must be TierLocalOnly (the pattern, not the table entry,
//     has the last word on spawn-capable paths).
//  2. Every TierLocalOnly route must match a pattern — if a new
//     spawn-capable route lands in the table without extending the
//     patterns, its dynamic-segment variants would slip past the
//     companion, so the test forces the patterns to stay complete.
func TestSpawnCapablePatternsMatchTable(t *testing.T) {
	for _, r := range RouteTable() {
		matched := matchesSpawnCapablePattern(r.Path)
		if matched && r.Tier != TierLocalOnly {
			t.Errorf("route %s %s matches a spawn-capable pattern but is tier %s, not LOCAL_ONLY", r.Method, r.Path, r.Tier)
		}
		if r.Tier == TierLocalOnly && !matched {
			t.Errorf("LOCAL_ONLY route %s %s matches no spawn-capable pattern — extend spawnCapablePatterns", r.Method, r.Path)
		}
	}
	// The companion must actually catch dynamic-segment variants
	// the table never lists: a future /admin/plugin/load/{id} is
	// spawn-capable even though only /admin/plugin/load exists.
	for _, path := range []string{
		"/admin/plugin/load/abc123",
		"/admin/service/remove/my-service",
		"/admin/tunnel/start",
		"/admin/update",
		"/admin/shutdown",
		"/admin/bootstrap",
		"/admin/provider/login",
	} {
		if !matchesSpawnCapablePattern(path) {
			t.Errorf("spawn-capable path %s matches no pattern", path)
		}
	}
	// And it must NOT swallow ordinary routes: a management route
	// under /admin must stay reachable from wherever its tier
	// allows (a too-broad pattern would silently local-only it).
	for _, path := range []string{
		"/admin/settings/export",
		"/admin/db/export",
		"/admin/gc/profile",
		"/api/providers",
		"/dashboard/guard",
	} {
		if matchesSpawnCapablePattern(path) {
			t.Errorf("non-spawn path %s wrongly matches a spawn-capable pattern", path)
		}
	}
}

// TestRegisterPanicsOnSpawnPatternMisclassified proves register()
// panics when a route under a spawn-capable prefix is classified
// anything but LOCAL_ONLY — the exact misclassification the
// regex companion exists to catch (a dynamic-segment route added
// with a table entry but the wrong tier).
func TestRegisterPanicsOnSpawnPatternMisclassified(t *testing.T) {
	srv := newTestServer(t, nil)
	orig := guardRouteTable
	guardRouteTable = append(append([]GuardRoute{}, orig...),
		GuardRoute{Method: "POST", Path: "/admin/plugin/load/{id}", Tier: TierManagement, Description: "test-only misclassified spawn route"})
	defer func() { guardRouteTable = orig }()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("register() did not panic for a spawn-pattern route classified MANAGEMENT")
		}
	}()
	srv.register(http.NewServeMux(), "POST", "/admin/plugin/load/{id}", func(w http.ResponseWriter, r *http.Request) {})
}

// M1.1 acceptance: scopes are enforced on every guarded route,
// driven by the guard table itself. A key that authenticates but
// lacks the route's scope gets 403 (never 401 — auth passed, the
// scope gate refused); a key with the scope passes the gate. The
// table test walks RouteTable() so a new route is covered the
// moment it is classified.
//
// Scope vocabulary (the only scopes the codebase issues today):
// "chat" = all /v1 inference endpoints (chat, embeddings, rerank,
// messages, responses, legacy completions, images, audio, files,
// moderations, media search/OCR/video/music, count_tokens,
// input_items/input_tokens, provider chat, gemini actions,
// session detail, quotas check), "models" = GET /v1/models and
// the model-listing surfaces (detail/query/health, provider
// models/status, gemini list, combo GETs), "admin" = every
// management / destructive route. "admin" implies every scope.
func issueScopedKey(t *testing.T, cfg *config.Config, name string, scopes []string) (config.GatewayKey, string) {
	t.Helper()
	k, raw, err := config.IssueKey(name, scopes, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatal(err)
	}
	return k, raw
}

func hasScopeName(scopes []string, want string) bool {
	for _, s := range scopes {
		if s == want {
			return true
		}
	}
	return false
}

func TestScopeEnforcedTableDriven(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	_, chatRaw := issueScopedKey(t, cfg, "chat-user", []string{"chat"})
	_, modelsRaw := issueScopedKey(t, cfg, "models-user", []string{"models"})
	_, adminRaw := issueScopedKey(t, cfg, "admin-user", []string{"chat", "models", "admin"})
	eng := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "hi", Model: "model-a",
		Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2},
	}}
	srv := newTestServer(t, eng, WithConfig(cfg))
	for _, route := range RouteTable() {
		route := route
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			path := strings.NewReplacer("{id}", "gk_test123", "{keyId}", "key_test123").Replace(route.Path)
			body := ""
			if route.Method == http.MethodPost || route.Method == http.MethodDelete {
				body = "{}"
			}
			if route.Path == "/v1/chat/completions" {
				body = kbChatBody("model-a", false)
			}
			// Bootstrap takes no key by design: from loopback it
			// is 409 once a key exists (never 401).
			if route.Bootstrap {
				rec := doReqPeer(t, srv, route.Method, path, body, "", loopbackPeer)
				if rec.Code != http.StatusConflict {
					t.Errorf("bootstrap without key: status %d, want 409 (already complete)", rec.Code)
				}
				return
			}
			// Exempt routes are public by classification: no key
			// must ever answer 401 on them.
			if route.AuthExempt {
				rec := doReqPeer(t, srv, route.Method, path, body, "", loopbackPeer)
				if rec.Code == http.StatusUnauthorized {
					t.Errorf("exempt route answered 401 without a key")
				}
				return
			}
			// No key at all: 401 (guard before auth only changes
			// this for Tier 1 off-loopback, tested elsewhere).
			rec := doReqPeer(t, srv, route.Method, path, body, "", loopbackPeer)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("no key: status %d, want 401", rec.Code)
			}
			// A valid key lacking the route's scope: 403 naming
			// the scope.
			if len(route.Scopes) > 0 {
				wrong := chatRaw
				if hasScopeName(route.Scopes, "chat") {
					wrong = modelsRaw // lacks "chat"
				}
				rec = doReqPeer(t, srv, route.Method, path, body, wrong, loopbackPeer)
				if rec.Code != http.StatusForbidden {
					t.Errorf("key without scope %v: status %d, want 403", route.Scopes, rec.Code)
				} else if !strings.Contains(strings.ToLower(rec.Body.String()), "scope") {
					t.Errorf("403 body %q must name the missing scope", rec.Body.String())
				}
			}
			// M5.7: GET /api/usage/om-usage answers 403 for a key
			// without allow_usage_command even when the scope
			// gate passes — the flag is a per-key feature gate,
			// not a scope. The test's admin key has no flag, so
			// 403 naming the disabled command is the expected
			// handler verdict, not a scope failure.
			if route.Path == "/api/usage/om-usage" {
				rec = doReqPeer(t, srv, route.Method, path, body, adminRaw, loopbackPeer)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "usage command is disabled") {
					t.Errorf("admin key without allow_usage_command: status %d, want 403 naming the disabled usage command: %s", rec.Code, rec.Body.String())
				}
				return
			}
			// M9.2: MCP remote transports are deliberately
			// scope-free in the guard table — authorisation lives
			// inside the transport, in the disjoint mcp: scope
			// namespace. An admin key carries no valid mcp:
			// scope, so the handler refuses it — but the toggle
			// is checked first, and both transports default OFF,
			// so the four toggle-checked routes answer 503
			// (disabled) and DELETE (which skips the toggle so
			// sessions stay terminable) answers 403 naming the
			// invalid scope. Either is the handler's own verdict,
			// not a scope-gate failure: passing "admin" through
			// would escalate a chat/admin key into MCP tools
			// (M9.1 SECURITY REVIEW N4).
			if route.Path == "/api/mcp/sse" || route.Path == "/api/mcp/stream" {
				rec = doReqPeer(t, srv, route.Method, path, body, adminRaw, loopbackPeer)
				if rec.Code != http.StatusServiceUnavailable {
					t.Errorf("admin key on disabled MCP transport %s %s: status %d, want 503: %s", route.Method, route.Path, rec.Code, rec.Body.String())
				}
				return
			}
			if route.Path == "/api/mcp/stream/{id}" {
				rec = doReqPeer(t, srv, route.Method, path, body, adminRaw, loopbackPeer)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not a valid MCP scope") {
					t.Errorf("admin key on MCP transport DELETE: status %d, want 403 naming the invalid MCP scope: %s", rec.Code, rec.Body.String())
				}
				return
			}
			// Dashboard login authenticates by password, not by
			// gateway key: the scope gate passes an admin key
			// through, and the handler itself answers 401 "wrong
			// dashboard password" because the table-driven body
			// carries no password. That 401 is the handler's own
			// verdict, not a scope-gate failure — same shape as
			// the om-usage carve-out above.
			if route.Path == "/dashboard/auth/login" {
				rec = doReqPeer(t, srv, route.Method, path, body, adminRaw, loopbackPeer)
				if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "wrong dashboard password") {
					t.Errorf("admin key on dashboard login: status %d, want 401 naming the wrong dashboard password: %s", rec.Code, rec.Body.String())
				}
				return
			}
			// A key with the scope (admin implies all): the scope
			// gate passes — the status is the handler's own
			// verdict, anything but 401/403.
			rec = doReqPeer(t, srv, route.Method, path, body, adminRaw, loopbackPeer)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("admin key: status %d, want the scope gate to pass (not 401/403): %s", rec.Code, rec.Body.String())
			}
		})
	}
	// Exact-200 spot checks: scope present means the real handler
	// result, not just a passed gate.
	exact := []struct {
		method, path, body, key string
	}{
		{"GET", "/v1/models", "", modelsRaw},
		{"GET", "/v1/me/status", "", chatRaw},
		{"POST", "/v1/chat/completions", kbChatBody("model-a", false), chatRaw},
		{"GET", "/admin/status", "", adminRaw},
		{"GET", "/healthz", "", ""},
	}
	for _, c := range exact {
		rec := doReqPeer(t, srv, c.method, c.path, c.body, c.key, loopbackPeer)
		if rec.Code != http.StatusOK {
			t.Errorf("%s %s: status %d, want 200: %s", c.method, c.path, rec.Code, rec.Body.String())
		}
	}
	// A models-only key cannot call chat at all.
	rec := doReqPeer(t, srv, "POST", "/v1/chat/completions", kbChatBody("model-a", false), modelsRaw, loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("models-only key on chat: status %d, want 403", rec.Code)
	}
	// A chat-only key cannot call admin routes.
	rec = doReqPeer(t, srv, "GET", "/admin/status", "", chatRaw, loopbackPeer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("chat-only key on admin/status: status %d, want 403", rec.Code)
	}
}

// TestMutatingDashboardPostsRequireAuth (M1.2 acceptance): every
// mutating /dashboard/* POST answers 401 with no key. These eight
// are the complete set of AuthExempt POSTs the guard-table review
// found (verified by grep: 8 POST rows with AuthExempt: true, all
// under /dashboard/).
func TestMutatingDashboardPostsRequireAuth(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	paths := []string{
		"/dashboard/models/refresh",
		"/dashboard/feedback",
		"/dashboard/intent-tiers",
		"/dashboard/suggestions",
		"/dashboard/guardrails",
		"/dashboard/strategies",
		"/dashboard/cooldown",
		"/dashboard/key-budgets",
		"/dashboard/quota-pools",
		"/dashboard/quota-schedules",
		"/dashboard/internal-budgets",
		"/dashboard/teams",
		"/dashboard/teams/{id}/members",
	}
	for _, p := range paths {
		rec := doReqPeer(t, srv, "POST", p, "{}", "", loopbackPeer)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST %s without key: status %d, want 401", p, rec.Code)
		}
	}
}

// TestExemptRoutesAreReadOnlyGet (M1.2 review, encoded): after the
// review, every AuthExempt route is a GET — a read-only feed. A
// future mutating route that lands exempt fails this test until
// it is deliberately reclassified.
func TestExemptRoutesAreReadOnlyGet(t *testing.T) {
	// The only exempt POSTs, by design: the OAuth token endpoints
	// are public because client auth is the code_verifier / client
	// secret, not a gateway key (guard.go, B3-07). Any other
	// mutating exempt route still fails this test.
	exemptMutating := map[string]bool{
		"POST /oauth/token":  true,
		"POST /oauth/revoke": true,
	}
	exempt := 0
	for _, r := range RouteTable() {
		if !r.AuthExempt {
			continue
		}
		exempt++
		if r.Method != http.MethodGet && !exemptMutating[r.Method+" "+r.Path] {
			t.Errorf("exempt route %s %s mutates state (%s) — exempt routes must be read-only GETs", r.Method, r.Path, r.Description)
		}
	}
	if exempt == 0 {
		t.Error("no AuthExempt routes at all — /healthz, /openapi.json and the dashboard feeds must stay public")
	}
	// The review outcome, pinned: only the genuinely public
	// surface stays exempt (health, openapi, static dashboard,
	// read-only feeds, public catalog, GET /api/pricing in M8.15,
	// GET /.well-known/agent.json in M9.9 — the A2A agent card,
	// public by spec: a discovery document clients must fetch
	// before they can present a credential — and GET
	// /api/telemetry/model-latency-stats in M10: read-only counts
	// and latencies, no keys, no message content,
	// guard.go:223 — plus POST /oauth/token and POST /oauth/revoke
	// in B3-07: public by design, client auth is the code_verifier
	// / client secret, not a gateway key).
	if exempt != 58 {
		t.Errorf("exempt routes = %d, want 58 (51 before M8 + GET /api/pricing in M8.15 + GET /api/mcp/audit and GET /api/mcp/status in M9.7 + GET /.well-known/agent.json in M9.9 + GET /api/telemetry/model-latency-stats in M10 + POST /oauth/token and POST /oauth/revoke in B3-07)", exempt)
	}
}

// TestGuardTableScopesComplete (M1.1): every non-exempt route that
// needs a key also names the scope that key must carry — except
// the deliberately scope-free ones (self-introspection and
// /metrics, where any valid key suffices). An exempt route must
// not name a scope it never enforces.
func TestGuardTableScopesComplete(t *testing.T) {
	scopeFree := map[string]bool{
		"GET /v1/me/status":       true,
		"GET /v1/me/sessions":     true,
		"GET /api/usage/om-usage": true,
		"GET /metrics":            true,
		// M9.2 MCP remote transports: deliberately scope-free in
		// the guard table (see the guard-row comment in guard.go).
		// Authorisation lives inside the transport, in the
		// disjoint mcp: scope namespace; no HTTP scope fits.
		"GET /api/mcp/sse":            true,
		"POST /api/mcp/sse":           true,
		"POST /api/mcp/stream":        true,
		"GET /api/mcp/stream":         true,
		"DELETE /api/mcp/stream/{id}": true,
	}
	for _, r := range RouteTable() {
		key := r.Method + " " + r.Path
		if r.AuthExempt {
			if len(r.Scopes) > 0 {
				t.Errorf("exempt route %s names scopes %v it never enforces", key, r.Scopes)
			}
			continue
		}
		if r.Bootstrap {
			if len(r.Scopes) > 0 {
				t.Errorf("bootstrap route %s takes no key, but names scopes %v", key, r.Scopes)
			}
			continue
		}
		if scopeFree[key] {
			continue
		}
		if len(r.Scopes) == 0 {
			t.Errorf("guarded route %s names no scope — every guarded route must declare one", key)
		}
	}
}
