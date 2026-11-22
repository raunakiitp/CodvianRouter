// M10-14 dashboard Settings repair + fold-in of Guardrails and Route
// guards (plan §M10-14, §6).
//
// Proves, against the served dashboard (not just the file on disk):
//   - the top-level Route-guards and Guardrails sections are gone
//     (sections, nav entries, and the stale titles-map entries);
//   - Settings has zero fake controls: the 5 display-only switches,
//     the Listen-address select, the hard-coded 8080 Port input, the
//     unwired Request-timeout input, and the generic fake handler
//     (toast "· saved") are all gone; the only .switch elements left
//     are the three Guardrails toggles, which POST for real;
//   - the listen address is shown read-only, naming the documented
//     default 127.0.0.1:20128 (internal/config/config.go DefaultListen)
//     until the live guard feed reports it;
//   - Guardrails (PII & content card) and Route guards (Route
//     authorization tables, plus the tunnel + DB cards) live under
//     Settings → Security with the SAME ids, the SAME feeds, and the
//     SAME POST — moved, not removed;
//   - the status-line feeds the new read-only rows cite all answer
//     200, and a Guardrails toggle round-trips through the real POST.
//   - the extracted inline script passes node --check.
//
// B7/B8 did NOT land, so no toggle/listener backend is expected:
// the default plan removes the controls, and this test pins that.
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

// m10_14MovedIDs are the element ids that moved into Settings →
// Security with the fold-in. Each must occur exactly once in the
// served page, after the Settings section opens and before the next
// top-level section (Cache) opens.
var m10_14MovedIDs = []string{
	// Guardrails cards (PII & content + counts + events).
	"grChecked", "grRedacted", "grBlocked",
	"grPolicyRefusals", "grUpstreamFailures",
	"grRefusalChain", "grFailover243", "grFailover257",
	"grKeyScoping", "grAsync", "grAsyncEvents", "grEvaluations",
	"grByKind", "grEvents", "grEventsEmpty",
	"grTogglePII", "grToggleSecrets", "grToggleInjection",
	"grPill", "guardrailsRefreshBtn",
	// Route-guard cards (status + Tier 1/2/3 tables + tunnel + DB).
	"guardPill", "guardDetail",
	"guardTierLocal", "guardTierProtected", "guardTierMgmt",
	"guardTierLocalDesc", "guardTierProtectedDesc", "guardTierMgmtDesc",
	"guardPatterns", "guardRefreshBtn",
	"tunnelPill", "tunnelDetail", "tunnelTargetInput", "tunnelPortInput",
	"tunnelKeyInput", "tunnelStatusBtn", "tunnelStartBtn", "tunnelStopBtn",
	"dbExportKeyInput", "dbExportConfirmInput", "dbExportBtn",
	"dbWipeKeyInput", "dbWipeConfirmInput", "dbWipeArmBtn", "dbWipeBtn",
	"dbImportFileInput", "dbImportKeyInput", "dbImportConfirmInput", "dbImportBtn",
}

// m10_14StatusIDs are the read-only status lines that replaced the
// 5 fake switches, plus the read-only listen-address line.
var m10_14StatusIDs = []string{
	"stFailover", "stCachePin", "stCostAware", "stHealth", "stLogging",
	"listenAddrLine",
}

// m10_14StatusFeeds are the live feeds the status lines cite. Each
// must answer 200 on the test server (fake engine, no providers).
var m10_14StatusFeeds = []string{
	"/dashboard/guard",
	"/dashboard/guardrails",
	"/dashboard/usage",
	"/dashboard/trace",
	"/dashboard/strategies",
	"/dashboard/logs",
	"/api/providers",
}

func TestDashboardM10_14TopLevelSectionsGone(t *testing.T) {
	body := m10_0DashboardBody(t)
	for _, gone := range []string{
		`id="view-guard"`,
		`data-view="guard"`,
		// Stale titles-map entries (M10-0 review follow-up): the
		// sections are gone, so the map must not name them either.
		`guard:"Route guards"`,
		// NOTE: view-guardrails / data-view="guardrails" are now a
		// dedicated P3-D top-level view (added after M10-14), so they
		// are expected to be present and are not in this "gone" list.
	} {
		if strings.Contains(body, gone) {
			t.Errorf("M10-14 top-level section still present: %s", gone)
		}
	}
	// The P3-D dedicated guardrails view must exist.
	if !strings.Contains(body, `id="view-guardrails"`) || !strings.Contains(body, `data-view="guardrails"`) {
		t.Error("M10-14/P3-D: dedicated view-guardrails section missing")
	}
}

func TestDashboardM10_14NoFakeControls(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)

	// The hard-coded 8080 literal is gone anywhere in the page.
	if strings.Contains(body, "8080") {
		t.Error("served dashboard still contains the 8080 literal")
	}
	// The Listen-address select and its LAN option are gone.
	if strings.Contains(body, "LAN (0.0.0.0)") {
		t.Error("served dashboard still contains the Listen-address select")
	}
	// The generic fake handler is gone: no $$(".switch") flipper and
	// no " · saved" toast suffix. (The trace line "prompt tokens ·
	// saved N" is a legitimate status string and is not matched by
	// the quoted suffix below.)
	if strings.Contains(script, `$$(".switch")`) {
		t.Error("served script still contains the generic fake switch handler")
	}
	if strings.Contains(script, `" · saved"`) {
		t.Error("served script still contains the fake \"· saved\" toast")
	}
	// The .switch elements are the three Guardrails kind toggles,
	// which have their own real POST handler, plus the per-type
	// detection toggle (B3-17): one template-rendered switch per
	// feed type, wired via the delegated #grTypes listener
	// (grPostTypeToggle -> POST /dashboard/guardrails with
	// disabled_types), proven by TestGuardrailsFeedListsTypesAndToggle.
	// A bare count bump would hide a fake control, so the 4th switch
	// must be the data-pattern one.
	if got := strings.Count(body, `class="switch"`); got != 4 {
		t.Errorf("served page has %d .switch elements, want 4 (3 Guardrails kind toggles + 1 per-type toggle)", got)
	}
	if !strings.Contains(body, `data-pattern=`) {
		t.Error("served page has no data-pattern per-type switch (the 4th .switch must be the wired per-type toggle)")
	}
	for _, id := range []string{"grTogglePII", "grToggleSecrets", "grToggleInjection"} {
		if got := strings.Count(body, `id="`+id+`"`); got != 1 {
			t.Errorf("served page has %d elements with id %q, want 1", got, id)
		}
	}
	// The 5 fake switches are gone as controls (their aria-labels
	// must not appear on any button; the labels survive only as
	// read-only status-row headings).
	for _, label := range []string{
		"Automatic failover", "Prompt-cache pinning", "Cost-aware routing",
		"Health checks", "Request logging",
	} {
		if strings.Contains(body, `aria-label="`+label+`"`) {
			t.Errorf("served page still has a switch button labelled %q", label)
		}
	}
	// The unwired Request-timeout input (no handler, no backend)
	// is gone too; the wired strategy picker stays.
	if strings.Contains(body, `<b>Request timeout</b>`) {
		t.Error("served page still contains the unwired Request-timeout control")
	}
	if !strings.Contains(body, `id="strategySel"`) {
		t.Error("Settings strategy picker missing: id=\"strategySel\"")
	}
	// The listen address is read-only text naming the documented
	// default (internal/config/config.go DefaultListen).
	if !strings.Contains(body, `id="listenAddrLine"`) {
		t.Error("Settings lacks the read-only listen-address line: id=\"listenAddrLine\"")
	}
	if !strings.Contains(body, "127.0.0.1:20128") {
		t.Error("served page never names the documented default listen address 127.0.0.1:20128")
	}
	// The read-only status lines exist.
	for _, id := range m10_14StatusIDs {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("Settings lacks the read-only status line: id=%q", id)
		}
	}
}

func TestDashboardM10_14FoldedIntoSettings(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)

	// The fold-in headings exist.
	for _, want := range []string{"Security", "PII &amp; content", "Route authorization"} {
		if !strings.Contains(body, want) {
			t.Errorf("Settings → Security lacks the heading %q", want)
		}
	}
	// Every moved id occurs exactly once, inside the Settings
	// section (it ends at the next top-level view section, whatever
	// its id — the Settings section's own opening tag precedes
	// settingsAt, so the first <section class="view" in this slice
	// is the next view; binding to a literal next view id would
	// silently swallow an inserted sibling section between Settings
	// and Cache). If no next view section exists the region runs to
	// end of body.
	settingsAt := strings.Index(body, `id="view-settings"`)
	if settingsAt < 0 {
		t.Fatal("served page lacks the view-settings section bound")
	}
	settingsRegion := body[settingsAt:]
	if endIdx := strings.Index(settingsRegion, `<section class="view"`); endIdx >= 0 {
		settingsRegion = settingsRegion[:endIdx]
	}
	for _, id := range m10_14MovedIDs {
		needle := `id="` + id + `"`
		if got := strings.Count(body, needle); got != 1 {
			t.Errorf("served page has %d elements with id %q, want 1", got, id)
			continue
		}
		if !strings.Contains(settingsRegion, needle) {
			t.Errorf("id %q is not inside the Settings section", id)
		}
	}

	// The feeds are still read from the moved home: the tier tables
	// read GET /dashboard/guard, the toggles GET + POST
	// /dashboard/guardrails (through adminHeaders, like every other
	// admin POST — see the M10-0 sweep), and Settings refreshes all
	// of them on open.
	for _, want := range []string{
		`fetch("/dashboard/guard")`,
		`fetch("/dashboard/guardrails")`,
		`fetch("/dashboard/guardrails", {method: "POST"`,
		`function grPostConfig()`,
		`function loadGuard()`,
		`function loadGuardrails()`,
		`function loadSettingsStatus()`,
		`if(view === "settings"){ loadStrategies(); loadHeadless(); loadGuard(); loadGuardrails(); loadSettingsStatus(); }`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("served script lacks %q", want)
		}
	}
	// The POST goes through adminHeaders(): the fetch site's
	// neighbourhood must name the shared helper.
	if idx := strings.Index(script, `fetch("/dashboard/guardrails", {method: "POST"`); idx < 0 {
		t.Error("served script has no POST fetch for /dashboard/guardrails")
	} else {
		lo := idx - 600
		if lo < 0 {
			lo = 0
		}
		if !strings.Contains(script[lo:idx], "adminHeaders()") {
			t.Error("POST /dashboard/guardrails is not wired through adminHeaders()")
		}
	}
}

func TestDashboardM10_14StatusFeedsLive(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, path := range m10_14StatusFeeds {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
	}
	// The guard feed carries the live listen address the Settings
	// line reports (server.go handleGuard: status.listen).
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guard", "", "")
	var guard struct {
		Status struct {
			Listen string `json:"listen"`
		} `json:"status"`
		Tiers []struct {
			Tier string `json:"tier"`
		} `json:"tiers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &guard); err != nil {
		t.Fatalf("GET /dashboard/guard is not JSON: %v", err)
	}
	if guard.Status.Listen == "" {
		t.Error("GET /dashboard/guard status.listen is empty")
	}
	if len(guard.Tiers) == 0 {
		t.Error("GET /dashboard/guard carries no tiers for the Route authorization tables")
	}
	// The guardrails feed carries the config the toggles render.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	var gr struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gr); err != nil {
		t.Fatalf("GET /dashboard/guardrails is not JSON: %v", err)
	}
	for _, key := range []string{"block_pii", "block_secrets", "block_injection"} {
		if _, ok := gr.Config[key]; !ok {
			t.Errorf("GET /dashboard/guardrails config lacks %q", key)
		}
	}
}

// TestDashboardM10_14GuardrailsToggleRoundTrip proves the moved
// toggles still POST to /dashboard/guardrails and the change sticks
// for the run: flip PII on, re-read, flip it back.
func TestDashboardM10_14GuardrailsToggleRoundTrip(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	read := func(t *testing.T) map[string]any {
		t.Helper()
		rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/guardrails status = %d, want 200", rec.Code)
		}
		var gr struct {
			Config map[string]any `json:"config"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &gr); err != nil {
			t.Fatalf("GET /dashboard/guardrails is not JSON: %v", err)
		}
		return gr.Config
	}
	post := func(t *testing.T, body string) {
		t.Helper()
		rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails", body, "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("POST /dashboard/guardrails status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
		}
	}
	before := read(t)
	post(t, `{"block_pii":true,"block_secrets":false,"block_injection":false}`)
	after := read(t)
	if after["block_pii"] != true {
		t.Errorf("POST did not apply: block_pii = %v, want true", after["block_pii"])
	}
	// Restore the pre-test state so the toggle does not leak into
	// sibling tests sharing nothing (each test builds its own
	// server, but the restore keeps the round-trip honest).
	restore, _ := json.Marshal(map[string]any{
		"block_pii":       before["block_pii"],
		"block_secrets":   before["block_secrets"],
		"block_injection": before["block_injection"],
	})
	post(t, string(restore))
	if got := read(t); got["block_pii"] != before["block_pii"] {
		t.Errorf("restore did not apply: block_pii = %v, want %v", got["block_pii"], before["block_pii"])
	}
	// Without a key the POST is admin-guarded, like every mutating
	// dashboard POST (M10-0 sweep).
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"block_pii":true,"block_secrets":false,"block_injection":false}`, "")
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("POST /dashboard/guardrails without a key: status = %d, want 401 or 403", rec.Code)
	}
}

// TestDashboardM10_14InlineScriptSyntax runs node --check over the
// extracted inline script of the served dashboard (plan §M10-14
// tests, same pattern as TestDashboardM10_4InlineScriptSyntax).
func TestDashboardM10_14InlineScriptSyntax(t *testing.T) {
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
