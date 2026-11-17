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
		"GET /dashboard/guard", "GET /api/providers", "GET /metrics",
		"GET /v1/models", "POST /v1/chat/completions",
		"POST /admin/shutdown", "POST /admin/service/install",
		"POST /admin/plugin/load", "POST /admin/update",
		"POST /admin/tunnel/start", "POST /admin/provider/login",
		"POST /admin/bootstrap", "POST /admin/settings/export",
		"GET /admin/status",
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
	counts := map[GuardTier]int{}
	for _, r := range table {
		counts[r.Tier]++
	}
	if counts[TierLocalOnly] != 7 || counts[TierAlwaysProtected] != 1 || counts[TierManagement] != 13 {
		t.Errorf("tier counts = LOCAL_ONLY %d, ALWAYS_PROTECTED %d, MANAGEMENT %d (want 7/1/13)", counts[TierLocalOnly], counts[TierAlwaysProtected], counts[TierManagement])
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
	paths := []string{"/admin/service/install", "/admin/plugin/load", "/admin/update", "/admin/tunnel/start", "/admin/provider/login"}
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
	// Tier 2 with the real key: 200, secrets redacted.
	key := issuedRaw
	rec = doReqPeer(t, srv, "POST", "/admin/settings/export", "", key, loopbackPeer)
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
	k, raw, err := config.IssueKey("test", []string{"chat"}, 0)
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
	rec := doReqPeer(t, srv, "POST", "/admin/settings/export", "", "test-key-123", loopbackPeer)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("settings export without config: status %d, want 503", rec.Code)
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
	// Loopback: 201 with the key, once.
	rec = doReqPeer(t, srv, "POST", "/admin/bootstrap", "", "", loopbackPeer)
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
	rec := doReqPeer(t, srv, "POST", "/admin/bootstrap", "", "", loopbackPeer)
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
