// M10-11 dashboard Resilience + Health views.
//
// Resilience: live breaker/cooldown state from GET /api/resilience
// (admin-scoped, so the dashboard retries with a key via the shared
// adminHeaders() helper — the Combos pattern), with a link to the
// existing Cooldown section, which owns the controls. Health: process
// health from GET /dashboard/memstats + GET /dashboard/readiness +
// GET /dashboard/benchmark (+ the history ring from GET
// /dashboard/benchmark/history, shown only when recorded).
//
// The M10-11 content itself is read-only: the KPI cards, breaker /
// cooling tables and Hedge card only read the live state. The
// Resilience SECTION as a whole is not read-only any more — the
// M10-11b (B6) follow-up added the write controls below it (breaker
// reset, resilience settings read/patch, per-model cooldowns),
// wired to the real B6 endpoints through adminHeaders(); the
// round-trips live in dashboard_m10_11b_test.go, and this file
// asserts only that the three cards are present and wired. At 0
// providers / no traffic the counters read 0 with "no traffic yet"
// labelling, never as performance claims.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/readiness"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// m10_11Section returns the served HTML of one dashboard section.
func m10_11Section(t *testing.T, body, id string) string {
	t.Helper()
	start := strings.Index(body, `id="`+id+`"`)
	if start < 0 {
		t.Fatalf("dashboard has no %s section", id)
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("%s section is not closed", id)
	}
	return rest[:end]
}

// m10_11InlineScript returns the served dashboard's single inline
// <script> block.
func m10_11InlineScript(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<script>")
	if start < 0 {
		t.Fatalf("dashboard has no inline script")
	}
	rest := body[start+len("<script>"):]
	end := strings.Index(rest, "</script>")
	if end < 0 {
		t.Fatalf("inline script is not closed")
	}
	return rest[:end]
}

// newM1011ResilienceServer builds the M8.16 fixture server (openai's
// breaker OPEN 5/5, cc and glm closed, glm's only connection cooling)
// plus an admin-scoped gateway key, so the mux-level resilience feed
// can be read exactly as the dashboard reads it.
func newM1011ResilienceServer(t *testing.T) (*Server, string) {
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
	eng := &autoComboTestEngine{
		breakers: []router.BreakerStatus{
			{ProviderID: "openai", Open: true, ConsecutiveFailures: 5, Threshold: 5},
			{ProviderID: "cc"},
			{ProviderID: "glm"},
		},
		cooling: []router.CoolingTarget{
			{ProviderID: "glm", KeyID: "conn-c", Mode: router.CooldownSoftDemote, RemainingSeconds: 30, Failures: 2, Demoted: true},
		},
	}
	return newTestServer(t, eng, WithConfig(cfg)), raw
}

func TestDashboardM10_11SectionsRender(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	resilience := m10_11Section(t, body, "view-resilience")
	health := m10_11Section(t, body, "view-health")
	script := m10_11InlineScript(t, body)

	for _, want := range []string{
		"<h1>Resilience</h1>",
		"read from the router — with breaker reset, settings and per-model cooldown controls below",
		`id="m10_11ResilienceOpen"`, `id="m10_11ResilienceOpenNote"`,
		`id="m10_11ResilienceCooling"`, `id="m10_11ResilienceCoolingNote"`,
		`id="m10_11ResilienceMode"`, `id="m10_11ResilienceMeta"`,
		`id="m10_11ResilienceBreakersBody"`, `id="m10_11ResilienceBreakersEmpty"`,
		"No breaker state yet — no traffic yet",
		`id="m10_11ResilienceKey"`, `id="m10_11ResilienceLoadBtn"`, "Load resilience",
		`id="m10_11ResilienceCoolingBody"`, `id="m10_11ResilienceCoolingEmpty"`,
		"No targets cooling — failed targets appear here with time left.",
		`id="m10_11ResilienceHedge"`, `id="m10_11ResilienceNote"`,
		// Link to the existing Cooldown section (not a rebuild).
		`id="m10_11ResilienceCooldownBtn"`, "Open Cooldown",
		"live in the Cooldown section — this view only reads the live state",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section missing %q", want)
		}
	}
	for _, want := range []string{
		"<h1>Health</h1>",
		"live values only",
		`id="m10_11HealthUptime"`, `id="m10_11HealthHeap"`, `id="m10_11HealthRSS"`,
		`id="m10_11HealthGoroutines"`, `id="m10_11HealthGCPause"`, `id="m10_11HealthGCNote"`,
		`id="m10_11HealthInFlight"`, `id="m10_11HealthRequestsNote"`,
		`id="m10_11HealthReadinessBody"`, `id="m10_11HealthReadinessEmpty"`,
		"No local capabilities registered — nothing to wait on.",
		`id="m10_11HealthBenchBody"`, `id="m10_11HealthBenchEmpty"`,
		"No benchmark run recorded yet",
		`id="m10_11HealthHistoryBody"`, `id="m10_11HealthHistoryEmpty"`,
		"No benchmark history recorded yet.",
		`id="m10_11HealthRefreshBtn"`, "Refresh",
	} {
		if !strings.Contains(health, want) {
			t.Errorf("dashboard Health section missing %q", want)
		}
	}

	// The Cooldown section itself is untouched by this slice: its
	// own ids are still there, and the Resilience link navigates to
	// it through go("cooldown") — a navigation, not a rebuild.
	cooldown := m10_11Section(t, body, "view-cooldown")
	for _, want := range []string{`id="coolRefreshBtn"`, `id="globalCooldownSel"`, `id="coolNow"`, `id="coolBreakers"`} {
		if !strings.Contains(cooldown, want) {
			t.Errorf("Cooldown section lost %q — M10-11 must not rebuild it", want)
		}
	}
	if !strings.Contains(script, `go("cooldown")`) {
		t.Error("dashboard Resilience JS has no go(\"cooldown\") navigation to the Cooldown section")
	}

	// The M10-11 content reads its feeds: the resilience read
	// (admin-scoped, keyed retry via the shared helper) and the
	// four auth-exempt health reads. (The B6 writes are asserted
	// separately below.)
	for _, want := range []string{
		`fetch("/api/resilience"`,
		"adminHeaders()",
		"m10_11LoadResilience",
		"m10_11RenderResilience",
		`fetch("/dashboard/memstats")`,
		`fetch("/dashboard/readiness")`,
		`fetch("/dashboard/benchmark")`,
		`fetch("/dashboard/benchmark/history")`,
		"m10_11LoadHealth",
		"d.breakers", "d.cooling", "d.cooldown_mode", "d.hedge", "d.note",
		"d.heap_alloc_mib", "d.capabilities", "d.recorded", "d.runs", "d.passes",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("dashboard M10-11 JS missing %q", want)
		}
	}

	// B6 writes (M10-11b, landed after this slice): the Resilience
	// section now carries the three write cards below the read-only
	// M10-11 content — breaker reset, resilience settings, and
	// per-model cooldown. Presence + wiring only here; the
	// round-trips live in dashboard_m10_11b_test.go.
	for _, want := range []string{
		"Breaker reset",
		`id="m10_11bResetAllBtn"`, "Reset all breakers",
		`id="m10_11bResetStatus"`,
		"Resilience settings",
		`id="m10_11bSettingsModeSel"`,
		`id="m10_11bSettingsSaveBtn"`, "Save cooldown mode",
		`id="m10_11bSettingsLoadBtn"`, "Load settings",
		"Per-model cooldown",
		`id="m10_11bCooldownModelInput"`,
		`id="m10_11bCooldownSetBtn"`, "Set override",
		`id="m10_11bCooldownClearBtn"`, "Clear override",
	} {
		if !strings.Contains(resilience, want) {
			t.Errorf("dashboard Resilience section missing B6 control %q", want)
		}
	}
	// The three cards are wired to the real B6 endpoints through
	// the shared adminHeaders() helper — asserted inside this
	// slice's own JS region, so a sibling's use of the same
	// strings cannot satisfy the check.
	js := m10_11bJSRegion(t, script)
	for _, want := range []string{
		`fetch("/api/resilience/settings"`,
		`fetch("/api/resilience/breakers/reset"`,
		`fetch("/api/resilience/cooldowns"`,
		// Clear override: the DELETE goes through the per-model
		// path helper, not a literal cooldowns URL.
		`m10_11bCooldownPath`,
		`{method: "DELETE"`,
		"adminHeaders()",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("dashboard M10-11b JS region missing %q", want)
		}
	}

	// No B6 controls outside the Resilience section: the Health
	// section stays read-only, with none of the write strings.
	for _, banned := range []string{
		"reset", "Reset", "settings", "Settings",
		"per-model", "Per-model",
		"/api/resilience/settings", "/api/resilience/breakers/reset",
		"/api/resilience/cooldowns",
	} {
		if strings.Contains(health, banned) {
			t.Errorf("Health section contains B6 control string %q", banned)
		}
	}

	// Static KPI values are "—": no fabricated number ships in the
	// page; every value arrives from a live feed.
	for _, id := range []string{
		`id="m10_11ResilienceOpen">—<`, `id="m10_11ResilienceCooling">—<`,
		`id="m10_11ResilienceMode">—<`,
		`id="m10_11HealthUptime">—<`, `id="m10_11HealthHeap">—<`,
		`id="m10_11HealthRSS">—<`, `id="m10_11HealthGoroutines">—<`,
		`id="m10_11HealthGCPause">—<`, `id="m10_11HealthInFlight">—<`,
	} {
		if !strings.Contains(body, id) {
			t.Errorf("dashboard M10-11 KPI %q is not the honest static —", id)
		}
	}

	// No fake-data markers, no Playground.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(resilience, banned) {
			t.Errorf("Resilience section contains %q", banned)
		}
		if strings.Contains(health, banned) {
			t.Errorf("Health section contains %q", banned)
		}
	}
}

// The resilience values the page renders equal GET /api/resilience:
// the fixture's OPEN breaker (5/5), the live glm cooling target,
// the honest-unknown hedge section. Without a key the feed refuses
// (admin scope) — which is why the page shows the needs-key card.
func TestDashboardM10_11ResilienceEqualsFeed(t *testing.T) {
	srv, raw := newM1011ResilienceServer(t)

	rec := doReq(t, srv, http.MethodGet, "/api/resilience", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/resilience without a key status = %d, want 401 (admin scope)", rec.Code)
	}

	rec = doReq(t, srv, http.MethodGet, "/api/resilience", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/resilience with admin key status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp resilienceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resilience feed: %v", err)
	}
	if !resp.EngineAvailable {
		t.Fatalf("engine_available = false, want true")
	}
	if len(resp.Breakers) != 3 {
		t.Fatalf("breakers = %d, want 3", len(resp.Breakers))
	}
	byID := map[string]resilienceBreakerJSON{}
	for _, b := range resp.Breakers {
		byID[b.ProviderID] = b
	}
	if openai := byID["openai"]; !openai.Open || openai.State != "OPEN" || openai.ConsecutiveFailures != 5 || openai.Threshold != 5 {
		t.Errorf("openai breaker = %+v, want OPEN with 5 consecutive failures / threshold 5", openai)
	}
	if cc := byID["cc"]; cc.Open || cc.State != "CLOSED" {
		t.Errorf("cc breaker = %+v, want CLOSED", cc)
	}
	if len(resp.Cooling) != 1 || resp.Cooling[0].ProviderID != "glm" || resp.Cooling[0].KeyID != "conn-c" || resp.Cooling[0].RemainingSeconds != 30 || !resp.Cooling[0].Demoted {
		t.Errorf("cooling = %+v, want the live glm conn-c target with 30s remaining, demoted", resp.Cooling)
	}
	if resp.Hedge.Status != "unknown" || resp.Hedge.MaxBodyBytes != 256<<10 {
		t.Errorf("hedge = %+v, want status unknown with the 256 KiB mirrored cap", resp.Hedge)
	}
	if !strings.Contains(resp.Hedge.Reason, "no hedge counter") {
		t.Errorf("hedge reason = %q, want the honest-unknown reason", resp.Hedge.Reason)
	}
	if resp.Note == "" || !strings.Contains(resp.Note, "cannot distinguish") {
		t.Errorf("resilience note = %q, want the breaker-counts caveat", resp.Note)
	}
	// The page renders exactly these values: the JS reads the
	// breaker/cooling/hedge/note blocks of this same feed (asserted
	// structurally in TestDashboardM10_11SectionsRender — the
	// values themselves come from this feed, never a second source).
}

// The health values the page renders equal the memstats / readiness /
// benchmark feeds: live process numbers, an empty capability list
// (no readiness gate wired in the test server), and an unrecorded
// benchmark + history (no bench file wired in the test server).
func TestDashboardM10_11HealthEqualsFeeds(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	rec := doReq(t, srv, http.MethodGet, "/dashboard/memstats", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/memstats status = %d, want 200", rec.Code)
	}
	var mem memstatsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mem); err != nil {
		t.Fatalf("decode memstats: %v", err)
	}
	if mem.GoVersion == "" {
		t.Error("memstats go_version is empty")
	}
	if mem.UptimeSeconds < 0 {
		t.Errorf("memstats uptime_seconds = %v, want >= 0", mem.UptimeSeconds)
	}
	if mem.Goroutines <= 0 {
		t.Errorf("memstats goroutines = %d, want > 0", mem.Goroutines)
	}
	// RSS comes from /proc/self/status, which does not exist
	// on Windows (readRSSKiB is Linux-only by design and the
	// feed honestly reports unavailable there), so the RSS
	// assertion is Linux-only — the same skip pattern as
	// TestDashboardMemstatsLive in memstats_test.go. The
	// go_version/uptime/goroutines checks above still validate
	// the live snapshot on every OS.
	if runtime.GOOS != "windows" {
		if !mem.RSSAvailable || mem.RSSMiB <= 0 {
			t.Errorf("memstats rss = %v available=%v, want a live RSS on Linux", mem.RSSMiB, mem.RSSAvailable)
		}
	}

	rec = doReq(t, srv, http.MethodGet, "/dashboard/readiness", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/readiness status = %d, want 200", rec.Code)
	}
	var ready readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if len(ready.Capabilities) != 0 {
		t.Errorf("readiness capabilities = %+v, want empty (no gate wired)", ready.Capabilities)
	}

	rec = doReq(t, srv, http.MethodGet, "/dashboard/benchmark", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/benchmark status = %d, want 200", rec.Code)
	}
	var bench benchmarkResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &bench); err != nil {
		t.Fatalf("decode benchmark: %v", err)
	}
	if bench.Recorded {
		t.Error("benchmark recorded = true, want false (no bench file wired)")
	}
	if !strings.Contains(bench.Note, "No benchmark run recorded yet") {
		t.Errorf("benchmark note = %q, want the honest unrecorded note", bench.Note)
	}

	rec = doReq(t, srv, http.MethodGet, "/dashboard/benchmark/history", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/benchmark/history status = %d, want 200", rec.Code)
	}
	var hist benchmarkHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("decode benchmark history: %v", err)
	}
	if hist.Recorded {
		t.Error("benchmark history recorded = true, want false (no history dir wired)")
	}
	if len(hist.Runs) != 0 {
		t.Errorf("benchmark history runs = %d, want 0 (never synthesised)", len(hist.Runs))
	}
}

// The readiness "Retry after" column, feed-verified with a
// non-empty fixture (M10-11 review nit 4): one ready capability and
// two not-ready capabilities, each with a distinct non-zero
// retry_after_seconds — including a large one (1200) past the served
// fmtNum's thousands-separator threshold, so the expectation cannot
// assume small values. The live feed values are driven through the
// page's own rendering expression — the exact ternary extracted
// from the served inline script, evaluated under node with the
// served fmtNum — and the rendered cell must equal the feed value
// as rendered by the served fmtNum ("<n>s", derived from the feed,
// never "—" and never a hardcoded guess). The ready row still shows
// its retry-after: the column renders the field whenever it is set,
// regardless of ready state.
func TestDashboardM10_11ReadinessRetryAfterEqualsFeed(t *testing.T) {
	gate := &readiness.Gate{}
	gate.Register("local:ollama", 45*time.Second, nil)
	gate.SetReady("local:ollama", true)
	gate.Register("local:lmstudio", 120*time.Second, nil)
	// Large retry-after: past the served fmtNum's thousands-separator
	// threshold, so the rendered value is "1,200s", not "1200s".
	gate.Register("local:qwen", 1200*time.Second, nil)
	srv := newTestServer(t, &fakeEngine{}, WithReadinessGate(gate))

	rec := doReq(t, srv, http.MethodGet, "/dashboard/readiness", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/readiness status = %d, want 200", rec.Code)
	}
	var ready readinessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ready); err != nil {
		t.Fatalf("decode readiness: %v", err)
	}
	if len(ready.Capabilities) != 3 {
		t.Fatalf("readiness capabilities = %+v, want the 3 seeded capabilities", ready.Capabilities)
	}
	byName := map[string]readiness.Capability{}
	for _, c := range ready.Capabilities {
		byName[c.Name] = c
	}
	ollama, ok := byName["local:ollama"]
	if !ok {
		t.Fatal("readiness feed has no local:ollama capability")
	}
	if !ollama.Ready || !ollama.Known || ollama.RetryAfterSeconds != 45 {
		t.Errorf("local:ollama = %+v, want ready, known, retry_after_seconds 45", ollama)
	}
	lmstudio, ok := byName["local:lmstudio"]
	if !ok {
		t.Fatal("readiness feed has no local:lmstudio capability")
	}
	if lmstudio.Ready || !lmstudio.Known || lmstudio.RetryAfterSeconds != 120 {
		t.Errorf("local:lmstudio = %+v, want not ready, known, retry_after_seconds 120", lmstudio)
	}
	qwen, ok := byName["local:qwen"]
	if !ok {
		t.Fatal("readiness feed has no local:qwen capability")
	}
	if qwen.Ready || !qwen.Known || qwen.RetryAfterSeconds != 1200 {
		t.Errorf("local:qwen = %+v, want not ready, known, retry_after_seconds 1200", qwen)
	}

	// The served page carries the "Retry after" column and renders
	// each row's cell from c.retry_after_seconds, falling back to
	// "—" only when the field is absent.
	rec = doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	health := m10_11Section(t, body, "view-health")
	if !strings.Contains(health, "Retry after") {
		t.Error("dashboard Health section has no \"Retry after\" column header")
	}
	script := m10_11InlineScript(t, body)

	// The exact retry-after cell expression, as served — not a
	// copy pasted into this test: the ternary the page evaluates
	// for every readiness row.
	const exprStart = "((c && c.retry_after_seconds)"
	start := strings.Index(script, exprStart)
	if start < 0 {
		t.Fatal("served dashboard JS has no retry_after_seconds rendering expression")
	}
	rest := script[start:]
	endRel := strings.Index(rest, `+ '</td>'`)
	if endRel < 0 {
		t.Fatal("served retry_after_seconds expression is not followed by the row close — dashboard JS shape changed")
	}
	expr := strings.TrimSpace(rest[:endRel])
	if !strings.Contains(expr, `fmtNum(c.retry_after_seconds) + "s"`) || !strings.Contains(expr, `"—"`) {
		t.Fatalf("served retry-after expression = %q, want the fmtNum … + \"s\" rendering with a — fallback", expr)
	}

	if _, err := exec.LookPath("node"); err != nil {
		// Without node the rendering cannot be executed here, so
		// skip explicitly: the feed + expression-shape checks
		// above ran, but the rendered-value check did not, and
		// the run must say so instead of passing silently weaker.
		t.Fatal("node not on PATH — rendered-value check skipped, feed + expression-shape checks ran (install node or set NODE_PATH)")
	}

	// The served fmtNum, as served — the rendering expression
	// calls it, so the driver must use the page's own definition.
	const fmtNumStart = "function fmtNum(n){"
	fnStart := strings.Index(script, fmtNumStart)
	if fnStart < 0 {
		t.Fatal("served dashboard JS has no fmtNum helper")
	}
	fnRest := script[fnStart:]
	fnEnd := strings.Index(fnRest, "}")
	if fnEnd < 0 {
		t.Fatal("served fmtNum helper is not closed")
	}
	fmtNumDef := fnRest[:fnEnd+1]

	feedJSON, err := json.Marshal(ready.Capabilities)
	if err != nil {
		t.Fatalf("marshal live readiness feed: %v", err)
	}
	// The driver renders each row twice: once through the page's
	// own extracted expression, and once as the expectation —
	// fmtNum(c.retry_after_seconds) + "s" computed by the served
	// fmtNum itself, so the expectation tracks the page's
	// thousands-separator formatting (1200 renders "1,200s") for
	// any fixture size instead of assuming small values.
	driver := fmtNumDef + "\nvar caps = " + string(feedJSON) + ";\n" +
		"var out = caps.map(function(c){ return {name: c.name, rendered: " + expr + ", expected: fmtNum(c.retry_after_seconds) + \"s\"}; });\n" +
		"console.log(JSON.stringify(out));\n"
	dir := t.TempDir()
	driverPath := filepath.Join(dir, "retry-after-driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var rendered []struct {
		Name     string `json:"name"`
		Rendered string `json:"rendered"`
		Expected string `json:"expected"`
	}
	if err := json.Unmarshal(raw, &rendered); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}
	if len(rendered) != len(ready.Capabilities) {
		t.Fatalf("node driver rendered %d rows, want %d (one per feed capability)", len(rendered), len(ready.Capabilities))
	}
	byRendered := map[string]struct{ rendered, expected string }{}
	for _, r := range rendered {
		byRendered[r.Name] = struct{ rendered, expected string }{r.Rendered, r.Expected}
	}
	for _, c := range ready.Capabilities {
		// Expected value from the served fmtNum itself (see the
		// driver above), so a large fixture is covered exactly
		// the way the page renders it — no small-value assumption.
		r, ok := byRendered[c.Name]
		if !ok {
			t.Errorf("node driver rendered no row for %q", c.Name)
			continue
		}
		if r.rendered == "—" {
			t.Errorf("rendered Retry after for %q is —, want %q from the feed value %d", c.Name, r.expected, c.RetryAfterSeconds)
		}
		if r.rendered != r.expected {
			t.Errorf("rendered Retry after for %q = %q, want %q (feed retry_after_seconds %d)", c.Name, r.rendered, r.expected, c.RetryAfterSeconds)
		}
		// Concrete anchor for the large fixture: the served fmtNum
		// groups thousands, so 1200 must render "1,200s".
		if c.RetryAfterSeconds == 1200 && r.rendered != "1,200s" {
			t.Errorf("rendered Retry after for %q = %q, want \"1,200s\" (served fmtNum thousands separator)", c.Name, r.rendered)
		}
	}
}

// The M10-11 pure helpers run under node, against the exact code
// served in the dashboard: node --check on the whole extracted
// inline script, then the pure block (uptime, breaker counts,
// readiness counts, breaker state label) driven with fixture data.
func TestDashboardM10_11PureHelpersUnderNode(t *testing.T) {
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
	start := strings.Index(script, "/* M10-11-PURE-START */")
	end := strings.Index(script, "/* M10-11-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-11-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]
	driver := pure + `
var out = {
  up3661: m10_11FormatUptime(3661),
  up90: m10_11FormatUptime(90),
  up45: m10_11FormatUptime(45),
  upNull: m10_11FormatUptime(null),
  upNeg: m10_11FormatUptime(-5),
  counts: m10_11BreakerCounts([{state: "OPEN", open: true}, {state: "CLOSED"}, {state: "unknown"}, {state: "DEGRADED", open: true}, {}]),
  countsEmpty: m10_11BreakerCounts([]),
  ready: m10_11ReadinessCounts([{name: "a", ready: true, known: true}, {name: "b", ready: false, known: true}, {name: "c", known: false}]),
  labelOpen: m10_11BreakerStateLabel({state: "OPEN"}),
  labelMissing: m10_11BreakerStateLabel({}),
  labelNull: m10_11BreakerStateLabel(null)
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
		Up3661 *string `json:"up3661"`
		Up90   *string `json:"up90"`
		Up45   *string `json:"up45"`
		UpNull *string `json:"upNull"`
		UpNeg  *string `json:"upNeg"`
		Counts struct {
			Total   int `json:"total"`
			Open    int `json:"open"`
			Closed  int `json:"closed"`
			Unknown int `json:"unknown"`
		} `json:"counts"`
		CountsEmpty struct {
			Total int `json:"total"`
		} `json:"countsEmpty"`
		Ready struct {
			Total    int `json:"total"`
			Ready    int `json:"ready"`
			NotReady int `json:"notReady"`
			Unknown  int `json:"unknown"`
		} `json:"ready"`
		LabelOpen    string `json:"labelOpen"`
		LabelMissing string `json:"labelMissing"`
		LabelNull    string `json:"labelNull"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}
	if got.Up3661 == nil || *got.Up3661 != "1h 1m" {
		t.Errorf("m10_11FormatUptime(3661) = %v, want 1h 1m", got.Up3661)
	}
	if got.Up90 == nil || *got.Up90 != "1m 30s" {
		t.Errorf("m10_11FormatUptime(90) = %v, want 1m 30s", got.Up90)
	}
	if got.Up45 == nil || *got.Up45 != "45s" {
		t.Errorf("m10_11FormatUptime(45) = %v, want 45s", got.Up45)
	}
	if got.UpNull != nil {
		t.Errorf("m10_11FormatUptime(null) = %v, want null (missing uptime is not 0s)", *got.UpNull)
	}
	if got.UpNeg != nil {
		t.Errorf("m10_11FormatUptime(-5) = %v, want null", *got.UpNeg)
	}
	if got.Counts.Total != 5 || got.Counts.Open != 2 || got.Counts.Closed != 1 || got.Counts.Unknown != 2 {
		t.Errorf("m10_11BreakerCounts = %+v, want total 5 / open 2 / closed 1 / unknown 2 (bare row counts as unknown)", got.Counts)
	}
	if got.CountsEmpty.Total != 0 {
		t.Errorf("m10_11BreakerCounts([]) total = %d, want 0", got.CountsEmpty.Total)
	}
	if got.Ready.Total != 3 || got.Ready.Ready != 1 || got.Ready.NotReady != 1 || got.Ready.Unknown != 1 {
		t.Errorf("m10_11ReadinessCounts = %+v, want total 3 / ready 1 / notReady 1 / unknown 1", got.Ready)
	}
	if got.LabelOpen != "open" {
		t.Errorf("m10_11BreakerStateLabel(OPEN) = %q, want open", got.LabelOpen)
	}
	if got.LabelMissing != "unknown" {
		t.Errorf("m10_11BreakerStateLabel({}) = %q, want unknown", got.LabelMissing)
	}
	if got.LabelNull != "unknown" {
		t.Errorf("m10_11BreakerStateLabel(null) = %q, want unknown", got.LabelNull)
	}
}
