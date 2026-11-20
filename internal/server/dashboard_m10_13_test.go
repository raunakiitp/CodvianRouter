// M10-13 dashboard A2A section (docs/plan/m10-dashboard-plan.md
// §M10-13, A2A half): the M9.9 section wired to the now-registered
// GET /api/a2a/status feed (server.go, guard.go Tier MANAGEMENT +
// admin scope) through the shared adminHeaders() helper.
//
// What this file proves, over the live server — not the handler,
// which a2a_status_test.go already covers (disabled all-zero,
// enabled live counts, 503 store-unavailable), and not
// registration, which registrations_m9m10_test.go covers:
//   - the served dashboard renders the A2A section (all pinned
//     ids, including the M10-13 needs-key card + button) and its
//     loadA2A() reads the status feed with adminHeaders(), never
//     with a bare unauthenticated fetch (which the guard would
//     401 in the default mode);
//   - the feed values the section renders are exactly the live
//     endpoint's values, enabled and disabled: enabled flag,
//     all five per-state counts, the six skill ids, protocol
//     version 2025-03-26 — and a keyless call is refused 401,
//     which is why the section needs the key;
//   - the served inline script parses (node --check).
//
// Forbidden, pinned here as in a2a_dashboard_test.go: no
// /api/a2a/tasks call, no per-owner task list, no task submission
// from the dashboard, no "Playground" string anywhere.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// m10_13Function returns the source of the named top-level
// dashboard JS function, from its "function <name>(" line to the
// next top-level "function " line (or the end of the served HTML).
func m10_13Function(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(")
	if start < 0 {
		t.Fatalf("dashboard JS has no function %s", name)
	}
	rest := body[start:]
	if end := strings.Index(rest, "\n  function "); end >= 0 {
		return rest[:end]
	}
	return rest
}

// m10_13StatusGET fetches the live status feed with the given key
// and decodes the 200 JSON body into an a2aStatusResponse.
func m10_13StatusGET(t *testing.T, srv *Server, key string) a2aStatusResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/a2a/status with key: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /api/a2a/status: decode: %v (body %q)", err, rec.Body.String())
	}
	return out
}

var m10_13WantSkills = []string{"smart-routing", "quota-management", "provider-discovery",
	"cost-analysis", "health-report", "list-capabilities"}

func TestDashboardM10_13A2ASectionRenders(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	loadA2A := m10_13Function(t, body, "loadA2A")

	for _, want := range []string{
		`id="view-a2a"`,
		`data-view="a2a"`,
		`id="a2aPill"`,
		`id="a2aDetail"`,
		`id="a2aTasks"`,
		`id="a2aTasksEmpty"`,
		`id="a2aSkills"`,
		`id="a2aSkillsEmpty"`,
		`id="a2aProtoVer"`,
		`id="a2aRefreshBtn"`,
		// M10-13 needs-key card + button (the admin-scoped feed
		// cannot be read keyless in the default mode).
		`id="a2aNeedsKey"`,
		`id="a2aLoadBtn"`,
		"/api/a2a/status",
		"/.well-known/agent.json",
		"a2a_enabled",
		"A2A is disabled",
		"Load with admin key",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard A2A section missing %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
	if strings.Contains(body, "/api/a2a/tasks") {
		t.Errorf("dashboard A2A section must not call a task-list endpoint")
	}

	// The wiring gap M10-13 closes: loadA2A() must read the feed
	// through adminHeaders() (the shared operator-key helper —
	// key read once, never stored), never with a bare fetch the
	// guard would 401.
	if !strings.Contains(loadA2A, "adminHeaders()") {
		t.Error("loadA2A does not read GET /api/a2a/status through adminHeaders()")
	}
	if !strings.Contains(loadA2A, `fetch("/api/a2a/status", opts)`) {
		t.Error(`loadA2A does not fetch "/api/a2a/status" with the admin headers`)
	}
	if strings.Contains(loadA2A, `fetch("/api/a2a/status")`) {
		t.Error("loadA2A still has a bare unauthenticated fetch of /api/a2a/status (401 in the default mode)")
	}
	// Auth + store-failure branches the section must render
	// honestly instead of generic "could not load".
	for _, want := range []string{"401", "403", "503", "store unavailable", "Needs key"} {
		if !strings.Contains(loadA2A, want) {
			t.Errorf("loadA2A missing the %q branch (needs-key / store-unavailable handling)", want)
		}
	}
	// Disabled rendering per the §3.2 contract: the feed's
	// all-zero counts + fixed skill list are rendered (the
	// Disabled pill + detail say they are not live activity).
	if !strings.Contains(loadA2A, "not live activity") {
		t.Error("loadA2A disabled detail does not say the rendered zeros/skills are not live activity")
	}
	// go() + Refresh re-read with the last-load key flag; the
	// 5s auto-refresh never re-reads a keyed load (the typed key
	// was consumed) and never retries a refused load.
	if !strings.Contains(script, "loadA2A(a2aKeyed)") {
		t.Error("dashboard nav/refresh does not call loadA2A(a2aKeyed)")
	}
	if !strings.Contains(script, "!a2aKeyed && !a2aNeedsKey") {
		t.Error("A2A auto-refresh is not guarded against keyed / needs-key loads")
	}
}

// TestDashboardM10_13A2AFeedDisabled proves the values the
// section renders while A2A is disabled are exactly the live
// endpoint's values: enabled:false, all-zero five-state counts,
// the fixed six skills, protocol 2025-03-26 — and that a keyless
// call is refused 401, which is why the section needs the key.
func TestDashboardM10_13A2AFeedDisabled(t *testing.T) {
	dataDir := t.TempDir()
	srv := newA2ATestServer(t, &fakeEngine{}, dataDir, false)

	// Keyless: refused — the section cannot read this feed
	// without the admin key.
	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/a2a/status keyless: status = %d, want 401 (body %q)", rec.Code, rec.Body.String())
	}

	out := m10_13StatusGET(t, srv, a2aKeyOne)
	if out.Enabled {
		t.Error("enabled = true, want false on an A2A-disabled server")
	}
	wantCounts := map[string]int{
		"submitted": 0, "working": 0, "completed": 0, "failed": 0, "cancelled": 0,
	}
	if !reflect.DeepEqual(out.TasksByState, wantCounts) {
		t.Errorf("tasks_by_state = %v, want all-zero %v", out.TasksByState, wantCounts)
	}
	if !reflect.DeepEqual(out.Skills, m10_13WantSkills) {
		t.Errorf("skills = %v, want %v", out.Skills, m10_13WantSkills)
	}
	if out.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol_version = %q, want 2025-03-26", out.ProtocolVersion)
	}
	// The disabled route never touches the store.
	if _, err := os.Stat(filepath.Join(dataDir, "a2a", "tasks.db")); !os.IsNotExist(err) {
		t.Errorf("tasks.db exists after a disabled status call (err %v) — the disabled route must never touch the store", err)
	}
}

// TestDashboardM10_13A2AFeedEnabledLiveCounts proves the values
// the section renders while A2A is enabled are the live endpoint's
// live counts: tasks seeded through the real service (two owners,
// so the counts are the operator aggregates across all owners)
// read back through the registered route, with the same six skills
// and protocol version — and no owner id leaks in the response.
func TestDashboardM10_13A2AFeedEnabledLiveCounts(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if svc.Store() == nil {
		t.Fatal("Service.Store() = nil, want the live store")
	}
	ownerA := "owner-m10-13-a"
	if _, err := svc.CreateTask(ownerA, json.RawMessage(`{"role":"user","content":"hi"}`)); err != nil {
		t.Fatalf("seed submitted task: %v", err)
	}
	ownerB := a2a.OwnerIDForKey("m10-13-key-b")
	a2aStatusSeedTask(t, svc.Store(), ownerB, a2a.StateCompleted)

	rec := doReq(t, srv, http.MethodGet, "/api/a2a/status", "", a2aKeyOne)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/a2a/status admin: status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var out a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("status response is not JSON: %v: %s", err, rec.Body.String())
	}
	if !out.Enabled {
		t.Error("enabled = false, want true on an A2A-enabled server")
	}
	wantCounts := map[string]int{
		"submitted": 1, "working": 0, "completed": 1, "failed": 0, "cancelled": 0,
	}
	if !reflect.DeepEqual(out.TasksByState, wantCounts) {
		t.Errorf("tasks_by_state = %v, want %v (one submitted + one completed across two owners)", out.TasksByState, wantCounts)
	}
	if !reflect.DeepEqual(out.Skills, m10_13WantSkills) {
		t.Errorf("skills = %v, want %v", out.Skills, m10_13WantSkills)
	}
	if out.ProtocolVersion != "2025-03-26" {
		t.Errorf("protocol_version = %q, want 2025-03-26", out.ProtocolVersion)
	}
	if strings.Contains(rec.Body.String(), ownerA) {
		t.Errorf("status response leaks the seed owner id: %s", rec.Body.String())
	}
}

// TestDashboardM10_13A2AInlineScriptSyntax runs node --check over
// the extracted inline script of the served dashboard (same
// pattern as TestDashboardM10_12InlineScriptSyntax). It skips
// when node is not installed.
func TestDashboardM10_13A2AInlineScriptSyntax(t *testing.T) {
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
