// M9.9 dashboard A2A section: nav entry, section, status capsule,
// task-count table, skills table, protocol version and agent-card
// link. Proves the section is served in the live dashboard HTML, not
// just present in the file. The /api/a2a/status feed itself is covered
// by the status builder's tests; this test asserts the section reads
// that exact feed and links the exact agent-card path.
package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestDashboardA2ASectionRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
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
		"/api/a2a/status",
		"/.well-known/agent.json",
		"a2a_enabled",
		"A2A is disabled",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard A2A section missing %q", want)
		}
	}
	// No Playground anywhere in the dashboard.
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
	// The section must not invent a per-owner task list endpoint or
	// submit tasks from the dashboard.
	if strings.Contains(body, "/api/a2a/tasks") {
		t.Errorf("dashboard A2A section must not call a task-list endpoint")
	}
}
