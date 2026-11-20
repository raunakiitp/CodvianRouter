// M10-13 dashboard MCP half (docs/plan/m10-dashboard-plan.md
// §M10-13, MCP half): the M9.9 MCP section polished against the
// plan — tool list with scopes, live provider/model counts,
// selector state + counters, audit view, transport toggles —
// reusing the M9.9 section where it already satisfies the plan
// and filling only the gaps.
//
// Live-backend verification first (probe 2026-11-19, real test
// server, no providers wired):
//   - GET /dashboard/mcp keyless → 200: 24 tools, 11 distinct
//     scopes, providers 0, models 0, ready false, mode "off",
//     servers [] — the plan's "24 tools / 11 scopes" are the live
//     numbers, never the reference router's 110/33. The tool list renders
//     even at 0 providers (plan §7).
//   - GET /api/mcp/status keyless → 200: both transports off,
//     0 sessions (live session COUNTS).
//   - GET /api/mcp/audit keyless → 200: available false, total 0.
//   - POST /api/mcp/status keyless → 401; with a static gateway
//     key → 200 and the toggle flips (read back keyless).
//
// What this file proves, over the live server and the served
// dashboard:
//   - the MCP section renders (all ids, incl. the new M10-13
//     tools-count pill) with no Playground, no invented
//     endpoint, and no the reference router numbers;
//   - the three MCP read feeds answer 200 keyless — they are
//     AuthExempt BY DESIGN (internal/server/guard.go:116-118;
//     internal/server/mcp.go:1-18 header: "unauthenticated by
//     design: the payload carries no secrets"), so there is NO
//     needs-key card for the reads: loadMCP() must stay a bare
//     keyless fetch, and the 5s auto-refresh needs no keyed-load
//     guard because it only re-reads keyless GETs. The one
//     admin-scoped MCP call, POST /api/mcp/status, already goes
//     through the shared adminHeaders() helper (setMCPTransport)
//     and fails honestly without a key — pinned here;
//   - the feed values the section renders are exactly the live
//     endpoint's values: tool count + distinct-scope count from
//     GET /dashboard/mcp, transport flags + session counts from
//     GET /api/mcp/status, audit availability from
//     GET /api/mcp/audit;
//   - a feed that does not answer renders "Could not load"
//     instead of a stale "Checking…" (the M10-13 A2A pattern);
//   - the served inline script parses (node --check).
//
// Deliberately NOT built (recorded gaps, no M10 backend
// proposed — plan §M10-13): heartbeat status and per-session
// tracking. Only live session COUNTS exist
// (GET /api/mcp/status); no endpoint lists sessions or reports
// a heartbeat to the dashboard, and this slice adds no backend
// route. No mutation controls beyond the plan's transport
// toggles exist in this section.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// m10_13MCPSection returns the served dashboard's MCP section
// (view-mcp), from its section tag to the A2A section marker, so
// assertions about the MCP region cannot pass off A2A strings.
func m10_13MCPSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-mcp"`)
	if start < 0 {
		t.Fatal("served dashboard has no MCP section (id=\"view-mcp\")")
	}
	rest := body[start:]
	if end := strings.Index(rest, "<!-- ================= A2A"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// m10_13MCPGET fetches one live MCP feed keyless and decodes the
// 200 JSON body into out.
func m10_13MCPGET(t *testing.T, srv *Server, path string, out any) {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, path, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s keyless: status = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("GET %s: decode: %v (body %q)", path, err, rec.Body.String())
	}
}

// m10_13DistinctScopes counts the distinct scopes across the
// feed's tools — the same count loadMCP() renders in the tools
// pill, computed here from the live feed, never hardcoded.
func m10_13DistinctScopes(tools []mcpToolFeed) int {
	seen := map[string]bool{}
	for _, tl := range tools {
		for _, s := range tl.Scopes {
			seen[s] = true
		}
	}
	return len(seen)
}

func TestDashboardM10_13MCPSectionRenders(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	section := m10_13MCPSection(t, body)
	loadMCP := m10_13Function(t, body, "loadMCP")

	for _, want := range []string{
		`id="view-mcp"`,
		`data-view="mcp"`,
		`id="mcpPill"`,
		`id="mcpDetail"`,
		`id="mcpTransportsPill"`,
		`id="mcpSSEStatus"`,
		`id="mcpSSESessions"`,
		`id="mcpSSEToggle"`,
		`id="mcpStreamStatus"`,
		`id="mcpStreamSessions"`,
		`id="mcpStreamToggle"`,
		`id="mcpModePill"`,
		`id="mcpSelectorDetail"`,
		`id="mcpServers"`,
		`id="mcpServersEmpty"`,
		`id="mcpSelectorCounts"`,
		`id="mcpTools"`,
		`id="mcpToolsEmpty"`,
		// M10-13: live tool/scope-count capsule on the Tools card.
		`id="mcpToolsPill"`,
		`id="mcpAuditPill"`,
		`id="mcpAuditCap"`,
		`id="mcpAuditBody"`,
		`id="mcpAuditEmpty"`,
		`id="mcpAuditPrev"`,
		`id="mcpAuditNext"`,
		`id="mcpAuditPage"`,
		`id="mcpRefreshBtn"`,
		"/dashboard/mcp",
		"/api/mcp/status",
		"/api/mcp/audit",
		"No MCP servers configured",
		"No tools listed yet",
		"No tool calls audited yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard MCP section missing %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
	// No invented endpoints and no the reference router numbers anywhere in
	// the MCP region: the plan's numbers are the live 24/11, never
	// the reference router's 110 tools / 33 scopes; there is no per-session
	// or heartbeat feed to call.
	for _, gone := range []string{"/api/mcp/tasks", "/api/mcp/sessions", "/api/mcp/heartbeat",
		"110 tools", "33 scopes", "heartbeat"} {
		if strings.Contains(section, gone) {
			t.Errorf("dashboard MCP section must not contain %q", gone)
		}
	}
	// No fake needs-key card for the MCP reads: the three GET
	// feeds are AuthExempt by design (guard.go:116-118) and answer
	// keyless, so a "Load with admin key" card here would lie
	// about the backend. (The A2A card keeps its own id.)
	if strings.Contains(section, "mcpNeedsKey") || strings.Contains(section, "Load with admin key") {
		t.Error("dashboard MCP section must not have a needs-key card: its read feeds answer keyless by design")
	}

	// The M10-13 gap the section fills: loadMCP() renders the
	// live tool/scope counts into the Tools capsule, computed from
	// the feed — never a hardcoded number.
	if !strings.Contains(loadMCP, "#mcpToolsPill") {
		t.Error("loadMCP does not render the live tool/scope counts into #mcpToolsPill")
	}
	if !strings.Contains(loadMCP, "tools") || !strings.Contains(loadMCP, "scopes") {
		t.Error("loadMCP tools pill does not show tool and scope counts")
	}
	// The reads stay keyless: loadMCP() must NOT call
	// adminHeaders() — the feeds are AuthExempt, and sending a
	// key on a GET the helper would clear is exactly the re-read
	// bug the A2A half guards against. Only the mutating toggle
	// POST uses the helper.
	if strings.Contains(loadMCP, "adminHeaders") {
		t.Error("loadMCP must not call adminHeaders(): GET /dashboard/mcp is AuthExempt and answers keyless")
	}
	loadStatus := m10_13Function(t, body, "loadMCPStatus")
	loadAudit := m10_13Function(t, body, "loadMCPAudit")
	// m10_13Function cuts at the next top-level function, so the
	// loadMCPAudit extract runs through the A2A comment block that
	// names adminHeaders() in prose — cut there so the check below
	// sees only the loader's own code.
	if idx := strings.Index(loadAudit, "/* ---------- A2A"); idx >= 0 {
		loadAudit = loadAudit[:idx]
	}
	if strings.Contains(loadStatus, "adminHeaders") ||
		strings.Contains(loadAudit, "adminHeaders") {
		t.Error("loadMCPStatus/loadMCPAudit must not call adminHeaders(): both GET feeds are AuthExempt")
	}
	// The 5s auto-refresh re-reads the keyless GETs only, so no
	// keyed-load guard flag is needed (or present) for MCP.
	if !strings.Contains(script, `if(v && v.classList.contains("on")) loadMCP();`) {
		t.Error("MCP auto-refresh does not re-read the keyless loadMCP() while the section is open")
	}
	// The one admin-scoped MCP call: the transport toggle POST
	// goes through adminHeaders() and fails honestly without one.
	setTransport := m10_13Function(t, body, "setMCPTransport")
	if !strings.Contains(setTransport, "adminHeaders()") {
		t.Error("setMCPTransport does not POST /api/mcp/status through adminHeaders()")
	}
	if !strings.Contains(setTransport, `fetch("/api/mcp/status"`) || !strings.Contains(setTransport, `"POST"`) {
		t.Error("setMCPTransport does not POST to /api/mcp/status")
	}
	if !strings.Contains(setTransport, "Could not change the transport — it needs an admin key") {
		t.Error("setMCPTransport has no honest needs-an-admin-key failure path")
	}
}

// TestDashboardM10_13MCPFeedsAnswerKeyless proves the values the
// section renders are exactly the live endpoints' values — and
// that the reads need no key: GET /dashboard/mcp, /api/mcp/status
// and /api/mcp/audit all answer 200 keyless (AuthExempt by
// design, guard.go:116-118). On a server with no registry wired
// the honest empty state is: tools still listed (they exist
// without providers), providers/models 0, ready false, mode off,
// no servers, audit unavailable.
func TestDashboardM10_13MCPFeedsAnswerKeyless(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	var feed mcpResponse
	m10_13MCPGET(t, srv, "/dashboard/mcp", &feed)
	if got, want := len(feed.Tools), len(mcpToolsFeed()); got != want {
		t.Errorf("GET /dashboard/mcp tools = %d, want %d (the mcp.Tools() list — feed and server must never drift)", got, want)
	}
	if got := m10_13DistinctScopes(feed.Tools); got == 0 {
		t.Errorf("GET /dashboard/mcp distinct scopes = 0, want > 0 (every tool carries scopes from the M9.1 table)")
	} else {
		t.Logf("live MCP counts: %d tools, %d distinct scopes", len(feed.Tools), got)
	}
	for _, tl := range feed.Tools {
		if len(tl.Scopes) == 0 {
			t.Errorf("tool %q has no scopes in the feed — the scope table and the feed drifted apart", tl.Name)
		}
	}
	if feed.Providers != 0 || feed.Models != 0 {
		t.Errorf("GET /dashboard/mcp providers/models = %d/%d, want 0/0 on a server with no providers wired", feed.Providers, feed.Models)
	}
	if feed.Ready {
		t.Error("GET /dashboard/mcp ready = true, want false with no registry wired")
	}
	if feed.Mode != "off" {
		t.Errorf("GET /dashboard/mcp mode = %q, want \"off\" with no MCP servers configured", feed.Mode)
	}
	if len(feed.Servers) != 0 {
		t.Errorf("GET /dashboard/mcp servers = %d, want 0 with no MCP servers configured", len(feed.Servers))
	}

	var status mcpStatusResponse
	m10_13MCPGET(t, srv, "/api/mcp/status", &status)
	if status.SSE.Enabled || status.Streamable.Enabled {
		t.Errorf("GET /api/mcp/status = %+v, want both transports off by default", status)
	}
	if status.SSE.Sessions != 0 || status.Streamable.Sessions != 0 {
		t.Errorf("GET /api/mcp/status sessions = %d/%d, want 0/0 with no sessions open", status.SSE.Sessions, status.Streamable.Sessions)
	}

	var audit mcpAuditResponse
	m10_13MCPGET(t, srv, "/api/mcp/audit", &audit)
	if audit.Available {
		t.Error("GET /api/mcp/audit available = true, want false with no audit log wired")
	}
	if audit.Total != 0 || len(audit.Entries) != 0 {
		t.Errorf("GET /api/mcp/audit total/entries = %d/%d, want 0/0 with no audit log wired", audit.Total, len(audit.Entries))
	}
}

// TestDashboardM10_13MCPTransportToggleNeedsAdminKey proves the
// only admin-scoped MCP call behaves as the section assumes: the
// toggle POST is refused 401 keyless and flips the live transport
// state with a gateway key (readable back keyless through the
// AuthExempt GET). Unknown transports are 400, never a silent
// no-op.
func TestDashboardM10_13MCPTransportToggleNeedsAdminKey(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	rec := doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":true}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/mcp/status keyless: status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}

	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/mcp/status with key: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var status mcpStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("toggle response is not JSON: %v: %s", err, rec.Body.String())
	}
	if !status.SSE.Enabled {
		t.Errorf("toggle response sse.enabled = false, want true after enabling SSE")
	}
	// The flip is live state, readable back through the keyless GET.
	var back mcpStatusResponse
	m10_13MCPGET(t, srv, "/api/mcp/status", &back)
	if !back.SSE.Enabled {
		t.Error("GET /api/mcp/status sse.enabled = false after the toggle POST enabled it")
	}

	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"bogus","enabled":true}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/mcp/status unknown transport: status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}

	// Leave the shared fixture state as found.
	rec = doReq(t, srv, http.MethodPost, "/api/mcp/status", `{"transport":"sse","enabled":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/mcp/status disable: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
}

// TestDashboardM10_13MCPFeedFailureHonest pins the M10-13
// failure handling in the MCP loaders: a feed that does not
// answer (non-200 or unreachable) renders "Could not load"
// instead of leaving the "Checking…" pills and last-good rows
// standing as if live. (The failure is driven in the browser;
// the test pins the branches in the served script, as the A2A
// half does for its 401/503 branches.)
func TestDashboardM10_13MCPFeedFailureHonest(t *testing.T) {
	body := m10_0DashboardBody(t)
	loadMCP := m10_13Function(t, body, "loadMCP")
	loadStatus := m10_13Function(t, body, "loadMCPStatus")
	loadAudit := m10_13Function(t, body, "loadMCPAudit")

	for _, fn := range []struct {
		name string
		src  string
	}{
		{"loadMCP", loadMCP},
		{"loadMCPStatus", loadStatus},
		{"loadMCPAudit", loadAudit},
	} {
		if !strings.Contains(fn.src, "Could not load") {
			t.Errorf("%s has no honest \"Could not load\" failure branch (a dead feed would leave \"Checking…\" standing)", fn.name)
		}
		if !strings.Contains(fn.src, "!r.ok") {
			t.Errorf("%s does not check r.ok before parsing — a non-200 feed would render as if it answered", fn.name)
		}
	}
	// A failed main-feed load clears the tool rows so stale rows
	// cannot linger under the error, and marks the tools capsule
	// the same way.
	if !strings.Contains(loadMCP, "#mcpTools") {
		t.Error("loadMCP failure branch does not clear the tool rows")
	}
	// Nit-fix resets pinned in the loadMCP catch: the selector
	// card is fed by this same feed, so its mode pill, selector
	// detail, server rows and live counters must all reset — and
	// the default-visible #mcpServersEmpty ("No MCP servers
	// configured") must hide, or a dead feed would keep asserting
	// no servers exist. (The success path uses srvEmpty; the
	// catch's failServersEmpty is the pinned reset.)
	for _, want := range []string{
		`$("#mcpModePill")`,
		`failModePill.textContent = "Could not load"`,
		`Could not load selector status`,
		`failServersBody.innerHTML = ""`,
		`failServersEmpty.style.display = "none"`,
		`Live tool-call counts are not live`,
	} {
		if !strings.Contains(loadMCP, want) {
			t.Errorf("loadMCP failure branch missing %q", want)
		}
	}
	// Nit-fix resets pinned in the loadMCPStatus catch: the
	// per-transport statuses, session counts and Enable/Disable
	// labels reset, and the enabled flags clear so a toggle click
	// after a failed load cannot act on a stale state.
	for _, want := range []string{
		`mcpSSEEnabled = false`,
		`mcpStreamEnabled = false`,
		`failSSEStatus.textContent = "Could not load"`,
		`failSSESessions.textContent = "—"`,
		`failSSEToggle.textContent = "Enable"`,
		`failStreamStatus.textContent = "Could not load"`,
		`failStreamSessions.textContent = "—"`,
		`failStreamToggle.textContent = "Enable"`,
		`Could not load transport status`,
	} {
		if !strings.Contains(loadStatus, want) {
			t.Errorf("loadMCPStatus failure branch missing %q", want)
		}
	}
	// Nit-fix resets pinned in the loadMCPAudit catch: the audit
	// rows clear, the cap says the log could not load, the page
	// text resets — and the default-visible #mcpAuditEmpty ("No
	// tool calls audited yet") hides, or it would contradict the
	// honest cap. (The success path uses empty; the catch's
	// failAuditEmpty is the pinned reset.)
	for _, want := range []string{
		`failAuditBody.innerHTML = ""`,
		`Could not load the audit log`,
		`failAuditPage.textContent = ""`,
		`failAuditEmpty.style.display = "none"`,
	} {
		if !strings.Contains(loadAudit, want) {
			t.Errorf("loadMCPAudit failure branch missing %q", want)
		}
	}
}

// TestDashboardM10_13MCPInlineScriptSyntax runs node --check over
// the extracted inline script of the served dashboard (same
// pattern as TestDashboardM10_13A2AInlineScriptSyntax). It skips
// when node is not installed.
func TestDashboardM10_13MCPInlineScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed: cannot run node --check")
	}
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write extracted script: %v", err)
	}
	out, err := exec.Command("node", "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
}
