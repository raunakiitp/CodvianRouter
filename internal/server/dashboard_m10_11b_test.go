// M10-11b dashboard Resilience B6 writes (follow-up to M10-11).
//
// The read-only M10-11 Resilience view gains the write side the B6
// backend landed (resilience_api.go): per-row + reset-all breaker
// reset, a global cooldown-mode settings form, and a per-model
// cooldown override control — all TierManagement / admin scope
// through the shared adminHeaders() helper, every result shown
// from the live response. The three settings with no runtime
// backing (breaker_threshold, breaker_open_for_seconds,
// cooldown_seconds) are rendered as read-only text from the live
// settings feed, never as editable controls that pretend to work.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// newM1011bServer builds a B6 server (mutable breaker/cooldown
// state, real breakerResetter) plus an admin-scoped gateway key,
// so the round-trips below run through the mux — proving the
// registration and the guard, not just the handler.
func newM1011bServer(t *testing.T) (*Server, string, *b6TestEngine) {
	t.Helper()
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "cc", Name: "cc", Enabled: true},
		{ID: "glm", Name: "glm", Enabled: true},
		{ID: "openai", Name: "openai", Enabled: true},
	}}
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	eng := newB6Engine()
	return newTestServer(t, eng, WithConfig(cfg), WithModels("openai/gpt-5")), raw, eng
}

// m10_11bJSRegion returns this slice's own JS: from its header
// comment to the Health wiring line that follows it, so the
// no-second-auth-path assertions below check only this slice's
// code, never a sibling's. The start anchor includes the opening
// /* of the header comment, so the returned region is valid
// standalone JS as-is (no caller needs to re-open the comment).
func m10_11bJSRegion(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "/* ---------- Resilience B6 writes")
	if start < 0 {
		t.Fatalf("dashboard script has no M10-11b B6 block")
	}
	rest := script[start:]
	end := strings.Index(rest, "var m10_11HealthRefreshBtn")
	if end < 0 {
		t.Fatalf("M10-11b B6 JS block has no end marker")
	}
	return rest[:end]
}

func TestDashboardM10_11bSectionsRender(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	resilience := m10_11Section(t, body, "view-resilience")
	script := m10_11InlineScript(t, body)
	js := m10_11bJSRegion(t, script)

	// (a) Breaker reset: per-row Reset column in the breaker table
	// plus the reset-all control and its live status line.
	for _, want := range []string{
		"<th>Reset</th>",
		`id="m10_11bResetAllBtn"`, "Reset all breakers",
		`id="m10_11bResetStatus"`, `id="m10_11bResetNote"`,
		"Resetting a breaker clears its counted failures and open window",
		"per-key Retry-After cooldown",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section missing %q", want)
		}
	}
	// (b) Settings form: global cooldown-mode select
	// (hard/soft-demote/inherit) + save/load + the honest note
	// that the three unbacked settings answer 501, not a control.
	for _, want := range []string{
		`id="m10_11bSettingsModeSel"`,
		`value="hard"`, `value="soft-demote"`,
		"Inherit — configured default",
		`id="m10_11bSettingsSaveBtn"`, "Save cooldown mode",
		`id="m10_11bSettingsLoadBtn"`, "Load settings",
		`id="m10_11bSettingsStatus"`, `id="m10_11bSettingsPersisted"`,
		`id="m10_11bSettingsUnsupported"`,
		"breaker_threshold", "breaker_open_for_seconds", "cooldown_seconds",
		"answer 501 naming the field",
		"change them in config and restart",
		"no global cooldown-duration knob",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section missing %q", want)
		}
	}
	// (c) Per-model cooldown override: model id + mode, set via
	// POST, clear via DELETE, plus the live overrides list.
	for _, want := range []string{
		`id="m10_11bCooldownModelInput"`, "openai/gpt-5",
		`id="m10_11bCooldownModeSel"`,
		`id="m10_11bCooldownSetBtn"`, "Set override",
		`id="m10_11bCooldownClearBtn"`, "Clear override",
		`id="m10_11bCooldownStatus"`,
		`id="m10_11bCooldownChainsBody"`, `id="m10_11bCooldownChainsEmpty"`,
		"No per-model overrides loaded",
		"no per-model cooldown duration",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section missing %q", want)
		}
	}

	// The read-only M10-11 content stays as-is: KPI cards, the
	// breaker/cooling tables, the Hedge card and the Cooldown link.
	for _, want := range []string{
		`id="m10_11ResilienceOpen"`, `id="m10_11ResilienceCooling"`,
		`id="m10_11ResilienceMode"`, `id="m10_11ResilienceBreakersBody"`,
		`id="m10_11ResilienceCoolingBody"`,
		`id="m10_11ResilienceHedge"`, `id="m10_11ResilienceNote"`,
		`id="m10_11ResilienceCooldownBtn"`, "Open Cooldown",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section lost read-only M10-11 content %q", want)
		}
	}

	// The B6 JS wires every control to its real endpoint, through
	// the shared adminHeaders() helper, with a confirm gate on the
	// destructive actions and the live error text on failure.
	for _, want := range []string{
		`fetch("/api/resilience/settings"`,
		`fetch("/api/resilience/breakers/reset"`,
		`fetch("/api/resilience/cooldowns"`,
		`m10_11bCooldownPath(`,
		`method: "PATCH"`, `method: "POST"`, `method: "DELETE"`,
		"adminHeaders()",
		`window.confirm("Reset the circuit breaker for "`,
		`window.confirm("Reset all circuit breakers?`,
		`window.confirm("Clear the cooldown override for "`,
		"m10_11bResetBreaker", "m10_11bResetAllBreakers",
		"m10_11bSaveSettings", "m10_11bLoadSettings",
		"m10_11bRenderSettings", "m10_11bSetCooldown", "m10_11bClearCooldown",
		"m10_11bResetRequestBody", "m10_11bSettingsPatchBody",
		"d.error.message",
		"reset_supported",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard M10-11b JS missing %q", want)
		}
	}

	// No second auth path in this slice's JS: no literal
	// Authorization/Bearer header — the key only ever travels via
	// adminHeaders(), which reads it fresh and clears it.
	for _, banned := range []string{"Authorization", "Bearer"} {
		if strings.Contains(js, banned) {
			t.Errorf("M10-11b JS contains a second auth path %q", banned)
		}
	}

	// No fake-data markers.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(resilience, banned) {
			t.Errorf("Resilience section contains %q", banned)
		}
	}
}

// The reset round-trip, through the mux with an admin key: the
// tripped openai breaker is reset (live feed then shows it CLOSED
// with zero failures), an unknown provider 404s, and reset-all
// closes every tripped breaker. Without a key every B6 write
// refuses with 401 (admin scope).
func TestDashboardM10_11bResetRoundTrip(t *testing.T) {
	srv, raw, eng := newM1011bServer(t)

	// No key → 401 on every B6 write and on the settings read.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "openai"}`},
		{http.MethodPatch, "/api/resilience/settings", `{"cooldown_mode": "soft-demote"}`},
		{http.MethodPost, "/api/resilience/cooldowns", `{"model": "openai/gpt-5", "mode": "hard"}`},
		{http.MethodDelete, "/api/resilience/cooldowns/openai/gpt-5", ""},
		{http.MethodGet, "/api/resilience/settings", ""},
	} {
		if rec := doReq(t, srv, tc.method, tc.path, tc.body, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	// The fixture's openai breaker is OPEN before the reset.
	settings := b6GetSettings(t, srv)
	if openai := b6BreakerByID(t, settings, "openai"); !openai.Open || openai.ConsecutiveFailures != 5 {
		t.Fatalf("fixture openai breaker = %+v, want OPEN with 5 consecutive failures", openai)
	}
	if !settings.ResetSupported {
		t.Fatalf("reset_supported = false, want true for the resetter engine")
	}

	// Unknown provider → 404, nothing reset.
	rec := doReq(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "ghost"}`, raw)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset ghost status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}

	// Reset the tripped breaker through the mux.
	rec = doReq(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "openai"}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp resilienceBreakerResetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != "openai" || resp.ResetCount != 1 {
		t.Fatalf("reset response = %+v, want reset [openai] count 1", resp)
	}

	// The live feed now shows it CLOSED with zero failures.
	after := b6GetSettings(t, srv)
	if openai := b6BreakerByID(t, after, "openai"); openai.Open || openai.State != "CLOSED" || openai.ConsecutiveFailures != 0 {
		t.Fatalf("openai breaker after reset = %+v, want CLOSED with 0 consecutive failures", openai)
	}

	// Reset-all: trip glm in-test too, then reset all three.
	eng.mu.Lock()
	eng.breakers["glm"] = router.BreakerStatus{ProviderID: "glm", Open: true, State: router.BreakerOpen, ConsecutiveFailures: 12, Threshold: 12, LastReason: "upstream-5xx", Trips: 2}
	eng.breakers["openai"] = router.BreakerStatus{ProviderID: "openai", Open: true, State: router.BreakerOpen, ConsecutiveFailures: 5, Threshold: 5, LastReason: "upstream-5xx", Trips: 1}
	eng.mu.Unlock()
	rec = doReq(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"all": true}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-all status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reset-all response: %v", err)
	}
	if resp.ResetCount != 3 || len(resp.Reset) != 3 {
		t.Fatalf("reset-all = %+v, want all 3 known breakers reset", resp)
	}
	afterAll := b6GetSettings(t, srv)
	for _, id := range []string{"openai", "glm", "cc"} {
		if b := b6BreakerByID(t, afterAll, id); b.Open || b.State != "CLOSED" || b.ConsecutiveFailures != 0 {
			t.Fatalf("%s breaker after reset-all = %+v, want CLOSED with 0 failures", id, b)
		}
	}
}

// The settings PATCH round-trip, through the mux: the global mode
// changes and survives a re-read, clearing it ("") restores the
// configured default, and a valid-but-unbacked field answers 501
// naming the field with nothing applied.
func TestDashboardM10_11bSettingsPatchRoundTrip(t *testing.T) {
	srv, raw, _ := newM1011bServer(t)

	// A valid but unbacked value answers 501 naming the field.
	for _, tc := range []struct{ body, field string }{
		{`{"breaker_threshold": 12}`, "breaker_threshold"},
		{`{"breaker_open_for_seconds": 30}`, "breaker_open_for_seconds"},
		{`{"cooldown_seconds": 60}`, "cooldown_seconds"},
	} {
		rec := doReq(t, srv, http.MethodPatch, "/api/resilience/settings", tc.body, raw)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("PATCH %s status = %d, want 501 (body: %s)", tc.body, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.field) {
			t.Fatalf("501 body = %s, want it to name %q", rec.Body.String(), tc.field)
		}
	}
	// A bad range is a 400, never a 501.
	rec := doReq(t, srv, http.MethodPatch, "/api/resilience/settings", `{"breaker_threshold": 0}`, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH threshold 0 status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	// Nothing was applied by the rejected patches.
	if got := b6GetSettings(t, srv); got.CooldownMode != router.CooldownHard {
		t.Fatalf("mode after rejected patches = %q, want hard", got.CooldownMode)
	}

	// Apply the real setting through the mux.
	rec = doReq(t, srv, http.MethodPatch, "/api/resilience/settings", `{"cooldown_mode": "soft-demote"}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeB6Settings(t, rec); got.CooldownMode != router.CooldownSoftDemote {
		t.Fatalf("PATCH response cooldown_mode = %q, want soft-demote", got.CooldownMode)
	}
	if reread := b6GetSettings(t, srv); reread.CooldownMode != router.CooldownSoftDemote {
		t.Fatalf("re-read cooldown_mode = %q, want soft-demote", reread.CooldownMode)
	}

	// Clearing ("") restores the configured default (hard).
	rec = doReq(t, srv, http.MethodPatch, "/api/resilience/settings", `{"cooldown_mode": ""}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH clear status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeB6Settings(t, rec); got.CooldownMode != router.CooldownHard {
		t.Fatalf("mode after clear = %q, want hard (configured default)", got.CooldownMode)
	}
}

// The per-model cooldown round-trip, through the mux: set a mode
// for a model, see it effective on re-read, clear it (twice —
// clearing is idempotent), see the model inherit the global mode
// again. The DELETE path carries a slash in the model id, which
// only the {model...} wildcard matches.
func TestDashboardM10_11bPerModelCooldownRoundTrip(t *testing.T) {
	srv, raw, _ := newM1011bServer(t)
	model := "openai/gpt-5"

	rec := doReq(t, srv, http.MethodPost, "/api/resilience/cooldowns", `{"model": "`+model+`", "mode": "soft-demote"}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeB6Settings(t, rec); b6ChainByModel(t, got, model).Effective != router.CooldownSoftDemote {
		t.Fatalf("chain after set = %+v, want effective soft-demote", got.Chains)
	}
	if reread := b6GetSettings(t, srv); b6ChainByModel(t, reread, model).Effective != router.CooldownSoftDemote {
		t.Fatalf("re-read chain = %+v, want effective soft-demote", reread.Chains)
	}

	for i := 0; i < 2; i++ {
		rec = doReq(t, srv, http.MethodDelete, "/api/resilience/cooldowns/"+model, "", raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("clear #%d status = %d, want 200 (body: %s)", i+1, rec.Code, rec.Body.String())
		}
		if got := decodeB6Settings(t, rec); b6ChainByModel(t, got, model).Effective != router.CooldownHard {
			t.Fatalf("chain after clear #%d = %+v, want effective hard (inherit global)", i+1, got.Chains)
		}
	}
	if reread := b6GetSettings(t, srv); b6ChainByModel(t, reread, model).Effective != router.CooldownHard {
		t.Fatalf("re-read chain after clear = %+v, want effective hard", reread.Chains)
	}
}

// m10_11bFuncBody returns one top-level function from the B6 JS
// region, from its declaration to the next top-level function (or
// the end of the region), so the nit-fix assertions below check
// each write handler on its own.
func m10_11bFuncBody(t *testing.T, js, name string) string {
	t.Helper()
	start := strings.Index(js, "function "+name+"(")
	if start < 0 {
		t.Fatalf("B6 JS has no function %s", name)
	}
	rest := js[start:]
	if idx := strings.Index(rest[1:], "\n  function "); idx >= 0 {
		return rest[:idx+1]
	}
	return rest
}

// The B6 nit fixes, pinned on the served JS: every write success
// handler marks the view keyed before its keyed reload (nit 1),
// the rendered unsupported-settings line keeps the "answer 501
// naming the field" wording (nit 2), and every write button is
// disabled in flight and re-enabled on both paths (nit 3).
func TestDashboardM10_11bNitFixesStatic(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := m10_11InlineScript(t, rec.Body.String())
	js := m10_11bJSRegion(t, script)

	// Nit 1: exactly five keyed assignments in the B6 region — one
	// per write success handler, no other place sets the flag.
	if n := strings.Count(js, "m10_11ResilienceKeyed = true;"); n != 5 {
		t.Errorf("B6 JS has %d %q assignments, want 5 (one per write success handler)", n, "m10_11ResilienceKeyed = true;")
	}
	writes := []string{
		"m10_11bResetBreaker", "m10_11bResetAllBreakers", "m10_11bSaveSettings",
		"m10_11bSetCooldown", "m10_11bClearCooldown",
	}
	for _, name := range writes {
		body := m10_11bFuncBody(t, js, name)
		ki := strings.Index(body, "m10_11ResilienceKeyed = true;")
		li := strings.Index(body, "m10_11LoadResilience(m10_11ResilienceKeyed);")
		if ki < 0 {
			t.Errorf("%s never sets m10_11ResilienceKeyed = true on success", name)
		} else if li < 0 || ki > li {
			t.Errorf("%s does not set m10_11ResilienceKeyed = true before its keyed reload", name)
		}
		// Nit 3: disable on entry, re-enable on success AND on
		// failure (two false calls: the .then and the .catch).
		if n := strings.Count(body, "m10_11bSetWriteBusy(btn, true"); n != 1 {
			t.Errorf("%s has %d in-flight disables, want 1", name, n)
		}
		if n := strings.Count(body, "m10_11bSetWriteBusy(btn, false"); n != 2 {
			t.Errorf("%s has %d re-enables, want 2 (success + failure)", name, n)
		}
	}

	// Nit 2: the rendered unsupported line keeps the 501 wording
	// alongside the per-field reasons.
	render := m10_11bFuncBody(t, js, "m10_11bRenderSettings")
	if !strings.Contains(render, "answer 501 naming the field") {
		t.Errorf("m10_11bRenderSettings drops the %q wording once the feed loads", "answer 501 naming the field")
	}
	if !strings.Contains(render, `u.field + " — " + u.reason`) {
		t.Errorf("m10_11bRenderSettings no longer renders the per-field reasons")
	}

	// Nit 3: the helper itself disables, re-enables, and refuses
	// to re-enable a reset button the feed disabled.
	helper := m10_11bFuncBody(t, js, "m10_11bSetWriteBusy")
	for _, want := range []string{"btn.disabled = true", "btn.disabled = false", "m10_11bResetSupported === false"} {
		if !strings.Contains(helper, want) {
			t.Errorf("m10_11bSetWriteBusy missing %q", want)
		}
	}
	// The dynamic buttons are handed to their handlers, so the
	// clicked button — not just the static one — is the one
	// disabled: per-row reset, per-row clear, and the main clear.
	// (The per-row reset call site lives in m10_11RenderResilience,
	// ahead of the B6 region, so check the full inline script.)
	for _, want := range []string{
		"m10_11bResetBreaker(b.provider, resetBtn)",
		"m10_11bClearCooldown(c.model, clearBtn)",
		`m10_11bClearCooldown("", m10_11bCooldownClearBtn)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard JS missing button hand-off %q", want)
		}
	}
}

// The B6 nit fixes, driven under node against the exact B6 JS
// served in the dashboard: stub DOM/fetch/adminHeaders, run each
// write, and observe the button state, the keyed flag and the
// reload call — including the failure path, where the button must
// come back enabled and no keyed reload may happen.
func TestDashboardM10_11bNitFixesUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not on PATH — cannot run the inline-script check (install node or set NODE_PATH)")
	}
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := m10_11InlineScript(t, rec.Body.String())
	js := m10_11bJSRegion(t, script)
	prelude := `
var els = {};
function $(sel){
  if(!els[sel]) els[sel] = {style: {}, disabled: false, textContent: "", value: "", innerHTML: "", addEventListener: function(){}};
  return els[sel];
}
function $$(sel){ return []; }
function toast(){}
var window = {confirm: function(){ return true; }};
function adminHeaders(){ return {"X-Api-Key": "test-key"}; }
var m10_11ResilienceKeyed = false;
var m10_11bResetSupported = null;
var loadCalls = [];
function m10_11LoadResilience(k){ loadCalls.push(k); }
var fetchMode = "ok";
function fetch(url, opts){
  if(fetchMode === "fail") return Promise.resolve({ok: false, status: 500, json: function(){ return Promise.resolve({}); }, text: function(){ return Promise.resolve("boom"); }});
  return Promise.resolve({ok: true, status: 200, json: function(){ return Promise.resolve({reset: ["openai"], reset_count: 1, cooldown_mode: "hard", persisted: true, reset_supported: true, unsupported: [{field: "breaker_threshold", reason: "no runtime backing"}], chains: []}); }, text: function(){ return Promise.resolve("{}"); }});
}
function sleep(ms){ return new Promise(function(r){ setTimeout(r, ms); }); }
`
	// The write handlers call the pure helpers (reset/settings
	// bodies, cooldown path), which live in the M10-11b-PURE block
	// ahead of the B6 region — include that block in the driver.
	pureStart := strings.Index(script, "/* M10-11b-PURE-START */")
	pureEnd := strings.Index(script, "/* M10-11b-PURE-END */")
	if pureStart < 0 || pureEnd < 0 || pureEnd <= pureStart {
		t.Fatal("pure helper block markers (M10-11b-PURE-START/END) not found in the inline script")
	}
	pure := script[pureStart:pureEnd]
	// The region is valid standalone JS as-is: it starts at the
	// opening /* of the B6 header comment, so no prefix is needed.
	driver := prelude + pure + js + `
(async function(){
  var out = {};
  m10_11bRenderSettings({cooldown_mode: "hard", persisted: true, reset_supported: true, unsupported: [{field: "breaker_threshold", reason: "no runtime backing"}, {field: "cooldown_seconds", reason: "no runtime backing"}], chains: []});
  var unsup = $("#m10_11bSettingsUnsupported").textContent;
  out.unsupKeeps501 = unsup.indexOf("answer 501 naming the field") >= 0;
  out.unsupKeepsFieldReason = unsup.indexOf("breaker_threshold") >= 0 && unsup.indexOf("no runtime backing") >= 0;
  var rowBtn = {disabled: false};
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  m10_11bResetBreaker("openai", rowBtn);
  out.rowBtnDisabledDuring = rowBtn.disabled === true;
  await sleep(50);
  out.rowBtnDisabledAfter = rowBtn.disabled === false;
  out.keyedAfterRowReset = m10_11ResilienceKeyed === true;
  out.loadKeyedAfterRowReset = loadCalls.length > 0 && loadCalls[loadCalls.length - 1] === true;
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  var allBtn = $("#m10_11bResetAllBtn");
  allBtn.disabled = false;
  m10_11bResetAllBreakers();
  out.allBtnDisabledDuring = allBtn.disabled === true;
  await sleep(50);
  out.allBtnDisabledAfter = allBtn.disabled === false;
  out.keyedAfterResetAll = m10_11ResilienceKeyed === true;
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  $("#m10_11bSettingsModeSel").value = "soft-demote";
  var saveBtn = $("#m10_11bSettingsSaveBtn");
  saveBtn.disabled = false;
  m10_11bSaveSettings();
  out.saveBtnDisabledDuring = saveBtn.disabled === true;
  await sleep(50);
  out.saveBtnDisabledAfter = saveBtn.disabled === false;
  out.keyedAfterSave = m10_11ResilienceKeyed === true;
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  $("#m10_11bCooldownModelInput").value = "openai/gpt-5";
  $("#m10_11bCooldownModeSel").value = "hard";
  var setBtn = $("#m10_11bCooldownSetBtn");
  setBtn.disabled = false;
  m10_11bSetCooldown();
  out.setBtnDisabledDuring = setBtn.disabled === true;
  await sleep(50);
  out.setBtnDisabledAfter = setBtn.disabled === false;
  out.keyedAfterSet = m10_11ResilienceKeyed === true;
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  var clearBtn = $("#m10_11bCooldownClearBtn");
  clearBtn.disabled = false;
  m10_11bClearCooldown("", clearBtn);
  out.clearBtnDisabledDuring = clearBtn.disabled === true;
  await sleep(50);
  out.clearBtnDisabledAfter = clearBtn.disabled === false;
  out.keyedAfterClear = m10_11ResilienceKeyed === true;
  fetchMode = "fail";
  m10_11ResilienceKeyed = false; loadCalls.length = 0;
  allBtn.disabled = false;
  m10_11bResetAllBreakers();
  out.failBtnDisabledDuring = allBtn.disabled === true;
  await sleep(50);
  out.failBtnDisabledAfter = allBtn.disabled === false;
  out.keyedAfterFail = m10_11ResilienceKeyed === false;
  out.loadCallsAfterFail = loadCalls.length;
  m10_11bResetSupported = false;
  var guardBtn = {disabled: true};
  m10_11bSetWriteBusy(guardBtn, false, true);
  out.resetGuardKeepsDisabled = guardBtn.disabled === true;
  var plainBtn = {disabled: true};
  m10_11bSetWriteBusy(plainBtn, false, false);
  out.plainReenable = plainBtn.disabled === false;
  console.log(JSON.stringify(out));
})().catch(function(e){ console.log(JSON.stringify({driverError: String(e && e.stack || e)})); });
`
	dir := t.TempDir()
	driverPath := filepath.Join(dir, "nit-driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node nit-fix driver failed: %v: %s", err, raw)
	}
	var got struct {
		DriverError             string `json:"driverError"`
		UnsupKeeps501           bool   `json:"unsupKeeps501"`
		UnsupKeepsFieldReason   bool   `json:"unsupKeepsFieldReason"`
		RowBtnDisabledDuring    bool   `json:"rowBtnDisabledDuring"`
		RowBtnDisabledAfter     bool   `json:"rowBtnDisabledAfter"`
		KeyedAfterRowReset      bool   `json:"keyedAfterRowReset"`
		LoadKeyedAfterRowReset  bool   `json:"loadKeyedAfterRowReset"`
		AllBtnDisabledDuring    bool   `json:"allBtnDisabledDuring"`
		AllBtnDisabledAfter     bool   `json:"allBtnDisabledAfter"`
		KeyedAfterResetAll      bool   `json:"keyedAfterResetAll"`
		SaveBtnDisabledDuring   bool   `json:"saveBtnDisabledDuring"`
		SaveBtnDisabledAfter    bool   `json:"saveBtnDisabledAfter"`
		KeyedAfterSave          bool   `json:"keyedAfterSave"`
		SetBtnDisabledDuring    bool   `json:"setBtnDisabledDuring"`
		SetBtnDisabledAfter     bool   `json:"setBtnDisabledAfter"`
		KeyedAfterSet           bool   `json:"keyedAfterSet"`
		ClearBtnDisabledDuring  bool   `json:"clearBtnDisabledDuring"`
		ClearBtnDisabledAfter   bool   `json:"clearBtnDisabledAfter"`
		KeyedAfterClear         bool   `json:"keyedAfterClear"`
		FailBtnDisabledDuring   bool   `json:"failBtnDisabledDuring"`
		FailBtnDisabledAfter    bool   `json:"failBtnDisabledAfter"`
		KeyedAfterFail          bool   `json:"keyedAfterFail"`
		LoadCallsAfterFail      int    `json:"loadCallsAfterFail"`
		ResetGuardKeepsDisabled bool   `json:"resetGuardKeepsDisabled"`
		PlainReenable           bool   `json:"plainReenable"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node nit-fix driver output: %v (raw %s)", err, raw)
	}
	if got.DriverError != "" {
		t.Fatalf("node nit-fix driver error: %s", got.DriverError)
	}
	for name, ok := range map[string]bool{
		"rendered unsupported keeps 501 wording":         got.UnsupKeeps501,
		"rendered unsupported keeps field + reason":      got.UnsupKeepsFieldReason,
		"per-row reset button disabled in flight":        got.RowBtnDisabledDuring,
		"per-row reset button re-enabled after success":  got.RowBtnDisabledAfter,
		"keyed flag set after per-row reset":             got.KeyedAfterRowReset,
		"reload after per-row reset goes out keyed":      got.LoadKeyedAfterRowReset,
		"reset-all button disabled in flight":            got.AllBtnDisabledDuring,
		"reset-all button re-enabled after success":      got.AllBtnDisabledAfter,
		"keyed flag set after reset-all":                 got.KeyedAfterResetAll,
		"settings save button disabled in flight":        got.SaveBtnDisabledDuring,
		"settings save button re-enabled after success":  got.SaveBtnDisabledAfter,
		"keyed flag set after settings save":             got.KeyedAfterSave,
		"cooldown set button disabled in flight":         got.SetBtnDisabledDuring,
		"cooldown set button re-enabled after success":   got.SetBtnDisabledAfter,
		"keyed flag set after cooldown set":              got.KeyedAfterSet,
		"cooldown clear button disabled in flight":       got.ClearBtnDisabledDuring,
		"cooldown clear button re-enabled after success": got.ClearBtnDisabledAfter,
		"keyed flag set after cooldown clear":            got.KeyedAfterClear,
		"reset-all button disabled in flight (failure)":  got.FailBtnDisabledDuring,
		"reset-all button re-enabled after failure":      got.FailBtnDisabledAfter,
		"keyed flag stays false after failed write":      got.KeyedAfterFail,
		"reset guard keeps unsupported button disabled":  got.ResetGuardKeepsDisabled,
		"non-reset button re-enables":                    got.PlainReenable,
	} {
		if !ok {
			t.Errorf("node nit-fix driver: %s = false", name)
		}
	}
	if got.LoadCallsAfterFail != 0 {
		t.Errorf("node nit-fix driver: failed write triggered %d reload(s), want 0", got.LoadCallsAfterFail)
	}
}

func TestDashboardM10_11bPureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not on PATH — cannot run the inline-script check (install node or set NODE_PATH)")
	}
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := m10_11InlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-11b-PURE-START */")
	end := strings.Index(script, "/* M10-11b-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-11b-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]
	driver := pure + `
var out = {
  pathSlash: m10_11bCooldownPath("openai/gpt-5"),
  pathMulti: m10_11bCooldownPath("a/b/c"),
  pathSpace: m10_11bCooldownPath("openai/gpt 5"),
  resetOne: m10_11bResetRequestBody("openai", false),
  resetAll: m10_11bResetRequestBody(null, true),
  patchSoft: m10_11bSettingsPatchBody("soft-demote"),
  patchClear: m10_11bSettingsPatchBody("")
};
console.log(JSON.stringify(out));
`
	driverPath := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var got struct {
		PathSlash  string         `json:"pathSlash"`
		PathMulti  string         `json:"pathMulti"`
		PathSpace  string         `json:"pathSpace"`
		ResetOne   map[string]any `json:"resetOne"`
		ResetAll   map[string]any `json:"resetAll"`
		PatchSoft  map[string]any `json:"patchSoft"`
		PatchClear map[string]any `json:"patchClear"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}
	if got.PathSlash != "/api/resilience/cooldowns/openai/gpt-5" {
		t.Errorf("m10_11bCooldownPath(openai/gpt-5) = %q, want the slashed path kept (wildcard route)", got.PathSlash)
	}
	if got.PathMulti != "/api/resilience/cooldowns/a/b/c" {
		t.Errorf("m10_11bCooldownPath(a/b/c) = %q, want all segments kept", got.PathMulti)
	}
	if got.PathSpace != "/api/resilience/cooldowns/openai/gpt%205" {
		t.Errorf("m10_11bCooldownPath(openai/gpt 5) = %q, want the space encoded", got.PathSpace)
	}
	if got.ResetOne["provider"] != "openai" || len(got.ResetOne) != 1 {
		t.Errorf("m10_11bResetRequestBody(openai, false) = %v, want {provider: openai}", got.ResetOne)
	}
	if got.ResetAll["all"] != true || len(got.ResetAll) != 1 {
		t.Errorf("m10_11bResetRequestBody(null, true) = %v, want {all: true}", got.ResetAll)
	}
	if got.PatchSoft["cooldown_mode"] != "soft-demote" || len(got.PatchSoft) != 1 {
		t.Errorf("m10_11bSettingsPatchBody(soft-demote) = %v, want {cooldown_mode: soft-demote}", got.PatchSoft)
	}
	if got.PatchClear["cooldown_mode"] != "" || len(got.PatchClear) != 1 {
		t.Errorf("m10_11bSettingsPatchBody(\"\") = %v, want {cooldown_mode: \"\"} (inherit)", got.PatchClear)
	}
}
