// M10-15 dashboard Analytics page (new view): summary KPIs,
// by-provider, by-model and costs — the four existing feeds only
// (GET /api/analytics/summary|by-provider|by-model|costs,
// server.go:1485-1488, guard.go:183-186, Tier Management +
// admin scope). Every number on the page traces to one of those
// four feeds: the smoke values asserted here are the feed
// responses, and the node driver below feeds those same live
// responses into the page's pure helpers and checks the outputs
// match field by field.
//
// CUT (recorded, per docs/plan/m10-dashboard-plan.md §M10-15):
// Combo Health is NOT built — the four G17 backend handlers
// (handleAnalyticsComboHealth, handleAnalyticsAutoRouting,
// handleAnalyticsCompression, handleAnalyticsDiversity in
// analytics_api.go) were removed as dead code in the M10-15
// cleanup: they existed as code but there was no registered/guarded endpoint for any of them.
// No s.register in server.go and no guard.go row, so they were
// unreachable over HTTP and the section must not call those
// routes. No health table is assembled from unrelated numbers.
// The auto-routing / compression / diversity tabs are likewise
// ABSENT, not empty: no registered feed exists for them, so the
// section contains no such tab strings at all. At 0 traffic the
// page shows "No data yet", never zero-filled numbers that imply
// measured zeros.
//
// Proves the section is served in the live dashboard HTML, not
// just present in the file.
package server

import (
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// analyticsSection returns the served HTML of
// <section id="view-analytics">.
func analyticsSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-analytics"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-analytics section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-analytics section is not closed")
	}
	return rest[:end]
}

// analyticsInlineScript returns the served dashboard's single
// inline <script> block.
func analyticsInlineScript(t *testing.T, body string) string {
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

// m10_15Function returns the source of the named top-level
// dashboard JS function, from its "function <name>(" line to the
// next top-level "function " line (or the end of the served HTML).
func m10_15Function(t *testing.T, body, name string) string {
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

// m10_15GetJSON fetches path with a gateway key and decodes the
// 200 JSON body into v. (The shared getJSON helper sends no key,
// and the analytics feeds are admin-scoped, so they would 401.)
func m10_15GetJSON(t *testing.T, srv *Server, path, key string, v any) {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, path, "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: decode: %v (body %q)", path, err, rec.Body.String())
	}
}

// m10_15AdminSetup builds a server with a real ledger and a
// gateway key carrying the admin scope the analytics feeds
// require, and returns the server plus the raw key.
func m10_15AdminSetup(t *testing.T) (*keyBudgetSetup, *Server) {
	t.Helper()
	setup := newKeyBudgetSetup(t)
	for i := range setup.cfg.GatewayKeys {
		if setup.cfg.GatewayKeys[i].ID == setup.id {
			setup.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	return setup, setup.server(t)
}

func TestDashboardM10_15AnalyticsRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := analyticsSection(t, body)
	script := analyticsInlineScript(t, body)

	for _, want := range []string{
		"<h1>Analytics</h1>",
		"Request, token and spend roll-ups from the stored ledger",
		// Window picker: styled per the design system, day/week/month.
		`id="m10_15WindowSel"`,
		`class="sel"`,
		`value="day"`,
		`value="week"`,
		`value="month"`,
		`id="m10_15RefreshBtn"`,
		// Admin-key gate: honest needs-key card + Load button.
		`id="m10_15KeyCard"`,
		`id="m10_15LoadBtn"`,
		"Load analytics",
		"Analytics needs an admin-scoped gateway key",
		`id="m10_15ErrorCard"`,
		`id="m10_15Error"`,
		// Summary KPI row (six live ids, static "—": no fabricated numbers).
		`id="m10_15KpiRequests">—<`,
		`id="m10_15KpiTokens">—<`,
		`id="m10_15KpiLatency">—<`,
		`id="m10_15KpiSpend">—<`,
		`id="m10_15KpiErrRate">—<`,
		`id="m10_15KpiCostStatus">—<`,
		`id="m10_15KpiCostNote"`,
		`id="m10_15SummaryNote"`,
		// By-provider table + honest empty state.
		`id="m10_15ProviderBody"`,
		`id="m10_15ProviderEmpty"`,
		`id="m10_15ProviderMeta"`,
		"No data yet — no usage rows in this window.",
		// By-model table + honest empty state.
		`id="m10_15ModelBody"`,
		`id="m10_15ModelEmpty"`,
		`id="m10_15ModelMeta"`,
		// Costs card: priced spend + unpriced remainder, no total.
		`id="m10_15CostsSpend">—<`,
		`id="m10_15CostsUnpricedReq">—<`,
		`id="m10_15CostsUnpricedTok">—<`,
		`id="m10_15CostsStatus">—<`,
		`id="m10_15CostsNote"`,
		"There is deliberately no total-cost figure here",
		// Feed labels: every table names the feed it reads.
		"from GET /api/analytics/by-provider",
		"from GET /api/analytics/by-model",
		"from GET /api/analytics/costs",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Analytics section missing %q", want)
		}
	}

	// Live pills: hidden before the first load — the values are
	// still "—" / "Loading live data…" then, so no pill may claim
	// live data. Exactly six pills, one per KPI card, all hidden.
	if n := strings.Count(section, `class="pill flat m10_15LivePill" style="display:none">Live</span>`); n != 6 {
		t.Errorf("Analytics section has %d hidden Live pills, want 6 (one per KPI card)", n)
	}
	if strings.Contains(section, `<span class="pill flat">Live</span>`) {
		t.Error("Analytics section has a visible Live pill before any load (must stay hidden until the first successful load)")
	}

	// The render reveals the pills once a load succeeds.
	renderFn := m10_15Function(t, body, "m10_15RenderAnalytics")
	for _, want := range []string{
		`$$(".m10_15LivePill")`,
		`p.style.display = ""`,
	} {
		if !strings.Contains(renderFn, want) {
			t.Errorf("dashboard Analytics render missing %q (Live pills must appear only after a successful load)", want)
		}
	}

	// The section is reachable from the nav (M10-0 slot) and loads
	// on open.
	if !strings.Contains(body, `data-view="analytics"`) {
		t.Error("dashboard nav has no analytics entry")
	}
	if !strings.Contains(script, `if(view === "analytics"){ m10_15LoadAnalytics(m10_15Keyed); }`) {
		t.Error("dashboard go() does not load Analytics on open")
	}

	// The loader reads all four feeds for the same window, inside
	// its own function — a whole-script search would pass on the
	// costs feed read by the unrelated avg-cost card. (The URLs
	// live in one array fetched via fetch(u, opts), so the paths
	// are asserted as the array literals plus the fetch call.)
	fn := m10_15Function(t, body, "m10_15LoadAnalytics")
	for _, feed := range []string{
		`"/api/analytics/summary?window="`,
		`"/api/analytics/by-provider?window="`,
		`"/api/analytics/by-model?window="`,
		`"/api/analytics/costs?window="`,
		`fetch(u, opts)`,
		"adminHeaders()",
		"m10_15RenderAnalytics",
	} {
		if !strings.Contains(fn, feed) {
			t.Errorf("dashboard Analytics loader missing %q", feed)
		}
	}

	// The loader calls only the four registered feeds — never the
	// auto-routing / compression / diversity / combo-health
	// routes (their handlers were removed as dead code in the
	// M10-15 cleanup; no registered/guarded endpoint ever
	// existed, so they were unreachable over HTTP and the
	// section must not call those routes).
	for _, banned := range []string{"auto-routing", "compression", "diversity", "combo"} {
		if strings.Contains(fn, banned) {
			t.Errorf("dashboard Analytics loader must not call %q (no registered endpoint)", banned)
		}
	}

	// The cut tabs and the cut health table are ABSENT from the
	// section — not empty, not disabled, not present as labels.
	// (Scoped to the section: other sections legitimately use
	// some of these words.)
	for _, banned := range []string{
		"Auto-routing", "auto-routing",
		"compression", "Compression",
		"diversity", "Diversity",
		"Combo Health", "combo health",
		"Lorem", "Sample data", "sample data",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("Analytics section must not contain %q (cut scope)", banned)
		}
	}
	// Playground stays removed from the whole dashboard.
	if strings.Contains(body, "Playground") {
		t.Error("dashboard must not contain Playground")
	}
}

// TestDashboardM10_15FeedValuesEqualSmoke proves every number the
// Analytics page shows traces to one of the four feeds: the
// seeded ledger's exact responses, fetched through the registered
// routes with an admin key. Seed: one priced row
// (mock-upstream/priced-model, 1000 prompt + 1000 completion =
// $0.04) and one unpriced row (mock-upstream/mock-model, 500 +
// 500), latencies 10 ms and 20 ms.
func TestDashboardM10_15FeedValuesEqualSmoke(t *testing.T) {
	setup, srv := m10_15AdminSetup(t)
	seedAnalyticsRows(t, setup)

	var sum analyticsSummaryResponse
	m10_15GetJSON(t, srv, "/api/analytics/summary?window=day", setup.raw, &sum)
	if !sum.HasData || sum.Requests != 2 || sum.OK != 2 || sum.Errors != 0 {
		t.Errorf("summary counts = has_data %v / %d / %d / %d, want true / 2 / 2 / 0", sum.HasData, sum.Requests, sum.OK, sum.Errors)
	}
	if sum.PromptTokens != 1500 || sum.CompletionTokens != 1500 || sum.TotalTokens != 3000 {
		t.Errorf("summary tokens = %d / %d / %d, want 1500 / 1500 / 3000", sum.PromptTokens, sum.CompletionTokens, sum.TotalTokens)
	}
	if math.Abs(sum.PricedCostUSD-0.04) > 1e-9 {
		t.Errorf("summary priced_cost_usd = %v, want 0.04", sum.PricedCostUSD)
	}
	if sum.CostStatus != "priced_only" {
		t.Errorf("summary cost_status = %q, want priced_only", sum.CostStatus)
	}
	if sum.UnpricedRequests == nil || *sum.UnpricedRequests != 1 {
		t.Errorf("summary unpriced_requests = %v, want 1", sum.UnpricedRequests)
	}
	if sum.UnpricedTokens == nil || *sum.UnpricedTokens != 1000 {
		t.Errorf("summary unpriced_tokens = %v, want 1000", sum.UnpricedTokens)
	}
	if math.Abs(sum.MeanLatencyMS-15) > 1e-9 {
		t.Errorf("summary mean_latency_ms = %v, want 15", sum.MeanLatencyMS)
	}
	if sum.Name != "day" {
		t.Errorf("summary window = %q, want day", sum.Name)
	}
	if !strings.Contains(sum.Note, "priced_cost_usd covers priced rows only") {
		t.Errorf("summary note = %q, want the priced-only honesty note", sum.Note)
	}

	var bp analyticsByProviderResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-provider?window=day", setup.raw, &bp)
	if !bp.HasData || len(bp.Providers) != 1 {
		t.Fatalf("by-provider = has_data %v / %d rows, want true / 1", bp.HasData, len(bp.Providers))
	}
	p := bp.Providers[0]
	if p.Provider != "mock-upstream" || p.Requests != 2 || p.OK != 2 || p.Errors != 0 {
		t.Errorf("by-provider row = %+v, want mock-upstream / 2 / 2 / 0", p)
	}
	if p.PromptTokens != 1500 || p.CompletionTokens != 1500 || p.TotalTokens != 3000 {
		t.Errorf("by-provider tokens = %d / %d / %d, want 1500 / 1500 / 3000", p.PromptTokens, p.CompletionTokens, p.TotalTokens)
	}
	if math.Abs(p.PricedCostUSD-0.04) > 1e-9 {
		t.Errorf("by-provider priced_cost_usd = %v, want 0.04", p.PricedCostUSD)
	}
	if !p.UnpricedRequestsKnown || p.UnpricedRequests != 1 {
		t.Errorf("by-provider unpriced = known %v / %d, want true / 1", p.UnpricedRequestsKnown, p.UnpricedRequests)
	}

	var bm analyticsByModelResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-model?window=day", setup.raw, &bm)
	if !bm.HasData || len(bm.Models) != 2 {
		t.Fatalf("by-model = has_data %v / %d rows, want true / 2", bm.HasData, len(bm.Models))
	}
	byName := map[string]analyticsModelRow{}
	for _, m := range bm.Models {
		byName[m.Model] = m
	}
	priced, ok := byName["priced-model"]
	if !ok {
		t.Fatal("by-model has no priced-model row")
	}
	if priced.Provider != "mock-upstream" || priced.Requests != 1 || priced.PromptTokens != 1000 || priced.CompletionTokens != 1000 || priced.TotalTokens != 2000 {
		t.Errorf("priced-model row = %+v, want mock-upstream / 1 / 1000 / 1000 / 2000", priced)
	}
	if math.Abs(priced.PricedCostUSD-0.04) > 1e-9 || priced.UnpricedRequests != 0 || !priced.UnpricedRequestsKnown {
		t.Errorf("priced-model cost = %v / unpriced %d / known %v, want 0.04 / 0 / true", priced.PricedCostUSD, priced.UnpricedRequests, priced.UnpricedRequestsKnown)
	}
	mock, ok := byName["mock-model"]
	if !ok {
		t.Fatal("by-model has no mock-model row")
	}
	if mock.Provider != "mock-upstream" || mock.Requests != 1 || mock.PromptTokens != 500 || mock.CompletionTokens != 500 || mock.TotalTokens != 1000 {
		t.Errorf("mock-model row = %+v, want mock-upstream / 1 / 500 / 500 / 1000", mock)
	}
	if math.Abs(mock.PricedCostUSD) > 1e-9 || mock.UnpricedRequests != 1 || !mock.UnpricedRequestsKnown {
		t.Errorf("mock-model cost = %v / unpriced %d / known %v, want 0 / 1 / true", mock.PricedCostUSD, mock.UnpricedRequests, mock.UnpricedRequestsKnown)
	}

	var co analyticsCostsResponse
	m10_15GetJSON(t, srv, "/api/analytics/costs?window=day", setup.raw, &co)
	if !co.HasData || co.Requests != 2 {
		t.Errorf("costs = has_data %v / requests %d, want true / 2", co.HasData, co.Requests)
	}
	if math.Abs(co.PricedCostUSD-0.04) > 1e-9 {
		t.Errorf("costs priced_cost_usd = %v, want 0.04", co.PricedCostUSD)
	}
	if co.CostStatus != "priced_only" {
		t.Errorf("costs cost_status = %q, want priced_only", co.CostStatus)
	}
	if co.UnpricedRequests == nil || *co.UnpricedRequests != 1 {
		t.Errorf("costs unpriced_requests = %v, want 1", co.UnpricedRequests)
	}
	if co.UnpricedTokens == nil || *co.UnpricedTokens != 1000 {
		t.Errorf("costs unpriced_tokens = %v, want 1000", co.UnpricedTokens)
	}

	// The feeds are admin-scoped: no key → 401/403, so the
	// dashboard's needs-key card is honest, not decorative.
	for _, path := range []string{
		"/api/analytics/summary?window=day",
		"/api/analytics/by-provider?window=day",
		"/api/analytics/by-model?window=day",
		"/api/analytics/costs?window=day",
	} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("GET %s without a key status = %d, want 401 or 403", path, rec.Code)
		}
	}
}

// TestDashboardM10_15EmptyLedgerIsHonest proves the 0-traffic
// state: every feed answers has_data false / cost_status
// no_data with empty breakdowns — the page's "No data yet"
// comes from the feed flags, not from the dashboard inventing it.
func TestDashboardM10_15EmptyLedgerIsHonest(t *testing.T) {
	setup, srv := m10_15AdminSetup(t)

	var sum analyticsSummaryResponse
	m10_15GetJSON(t, srv, "/api/analytics/summary?window=day", setup.raw, &sum)
	if sum.HasData || sum.Requests != 0 || sum.CostStatus != "no_data" {
		t.Errorf("empty summary = has_data %v / requests %d / status %q, want false / 0 / no_data", sum.HasData, sum.Requests, sum.CostStatus)
	}
	if sum.Note != "no usage rows in this window" {
		t.Errorf("empty summary note = %q, want the no-rows note", sum.Note)
	}
	var bp analyticsByProviderResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-provider?window=day", setup.raw, &bp)
	if bp.HasData || len(bp.Providers) != 0 {
		t.Errorf("empty by-provider = has_data %v / %d rows, want false / 0", bp.HasData, len(bp.Providers))
	}
	var bm analyticsByModelResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-model?window=day", setup.raw, &bm)
	if bm.HasData || len(bm.Models) != 0 {
		t.Errorf("empty by-model = has_data %v / %d rows, want false / 0", bm.HasData, len(bm.Models))
	}
	var co analyticsCostsResponse
	m10_15GetJSON(t, srv, "/api/analytics/costs?window=day", setup.raw, &co)
	if co.HasData || co.Requests != 0 || co.CostStatus != "no_data" {
		t.Errorf("empty costs = has_data %v / requests %d / status %q, want false / 0 / no_data", co.HasData, co.Requests, co.CostStatus)
	}
}

// The page's pure helpers run under node, against the exact code
// served in the dashboard: node --check on the whole extracted
// inline script, then the pure block driven with the REAL feed
// responses fetched above — the rendered numbers must equal the
// feed responses field by field, and the empty feeds must render
// nulls ("No data yet"), never zeros.
func TestDashboardM10_15PureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — cannot run the inline-script check")
	}
	setup, srv := m10_15AdminSetup(t)
	seedAnalyticsRows(t, setup)

	var sum analyticsSummaryResponse
	m10_15GetJSON(t, srv, "/api/analytics/summary?window=day", setup.raw, &sum)
	var bp analyticsByProviderResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-provider?window=day", setup.raw, &bp)
	var bm analyticsByModelResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-model?window=day", setup.raw, &bm)
	var co analyticsCostsResponse
	m10_15GetJSON(t, srv, "/api/analytics/costs?window=day", setup.raw, &co)

	srv2 := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv2, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := analyticsInlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-15-PURE-START */")
	end := strings.Index(script, "/* M10-15-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-15-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]

	mustJSON := func(t *testing.T, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal feed for node driver: %v", err)
		}
		return string(b)
	}
	driver := pure + `
var summary = ` + mustJSON(t, sum) + `;
var byProv = ` + mustJSON(t, bp) + `;
var byModel = ` + mustJSON(t, bm) + `;
var costs = ` + mustJSON(t, co) + `;
var emptySummary = {has_data: false, cost_status: "no_data", requests: 0};
var emptyByProv = {has_data: false, providers: []};
var emptyByModel = {has_data: false, models: []};
var emptyCosts = {has_data: false, cost_status: "no_data", requests: 0};
var out = {
  kpis: m10_15SummaryKpis(summary),
  provRows: m10_15ProviderRows(byProv),
  modelRows: m10_15ModelRows(byModel),
  costs: m10_15CostsKpis(costs),
  emptyKpis: m10_15SummaryKpis(emptySummary),
  emptyProv: m10_15ProviderRows(emptyByProv),
  emptyModel: m10_15ModelRows(emptyByModel),
  emptyCosts: m10_15CostsKpis(emptyCosts),
  fmtUSD: m10_15FmtUSD(0.04),
  fmtInt: m10_15FmtInt(3000)
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
		Kpis struct {
			Requests      *int     `json:"requests"`
			OK            *int     `json:"ok"`
			Errors        *int     `json:"errors"`
			TotalTokens   *int     `json:"totalTokens"`
			MeanLatencyMs *float64 `json:"meanLatencyMs"`
			PricedCost    *float64 `json:"pricedCost"`
			CostStatus    string   `json:"costStatus"`
			CostLabel     string   `json:"costLabel"`
			ErrorRate     *float64 `json:"errorRate"`
			Note          string   `json:"note"`
		} `json:"kpis"`
		ProvRows []struct {
			Provider         string  `json:"provider"`
			Requests         int     `json:"requests"`
			OK               int     `json:"ok"`
			Errors           int     `json:"errors"`
			TotalTokens      int     `json:"totalTokens"`
			PricedCost       float64 `json:"pricedCost"`
			UnpricedRequests int     `json:"unpricedRequests"`
			UnpricedKnown    bool    `json:"unpricedKnown"`
		} `json:"provRows"`
		ModelRows []struct {
			Model            string  `json:"model"`
			Provider         string  `json:"provider"`
			Requests         int     `json:"requests"`
			TotalTokens      int     `json:"totalTokens"`
			PricedCost       float64 `json:"pricedCost"`
			UnpricedRequests int     `json:"unpricedRequests"`
			UnpricedKnown    bool    `json:"unpricedKnown"`
		} `json:"modelRows"`
		Costs struct {
			Requests         *int     `json:"requests"`
			PricedCost       *float64 `json:"pricedCost"`
			UnpricedRequests *int     `json:"unpricedRequests"`
			UnpricedTokens   *int     `json:"unpricedTokens"`
			CostStatus       string   `json:"costStatus"`
			CostLabel        string   `json:"costLabel"`
			Note             string   `json:"note"`
		} `json:"costs"`
		EmptyKpis struct {
			Requests  *int   `json:"requests"`
			CostLabel string `json:"costLabel"`
			Note      string `json:"note"`
		} `json:"emptyKpis"`
		EmptyProv  []struct{} `json:"emptyProv"`
		EmptyModel []struct{} `json:"emptyModel"`
		EmptyCosts struct {
			Requests  *int   `json:"requests"`
			CostLabel string `json:"costLabel"`
			Note      string `json:"note"`
		} `json:"emptyCosts"`
		FmtUSD string `json:"fmtUSD"`
		FmtInt string `json:"fmtInt"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}

	// Summary KPIs equal the summary feed, field by field.
	if got.Kpis.Requests == nil || *got.Kpis.Requests != sum.Requests {
		t.Errorf("kpis.requests = %v, want %d (summary feed)", got.Kpis.Requests, sum.Requests)
	}
	if got.Kpis.OK == nil || *got.Kpis.OK != sum.OK {
		t.Errorf("kpis.ok = %v, want %d", got.Kpis.OK, sum.OK)
	}
	if got.Kpis.Errors == nil || *got.Kpis.Errors != sum.Errors {
		t.Errorf("kpis.errors = %v, want %d", got.Kpis.Errors, sum.Errors)
	}
	if got.Kpis.TotalTokens == nil || *got.Kpis.TotalTokens != sum.TotalTokens {
		t.Errorf("kpis.totalTokens = %v, want %d", got.Kpis.TotalTokens, sum.TotalTokens)
	}
	if got.Kpis.MeanLatencyMs == nil || math.Abs(*got.Kpis.MeanLatencyMs-sum.MeanLatencyMS) > 1e-9 {
		t.Errorf("kpis.meanLatencyMs = %v, want %v", got.Kpis.MeanLatencyMs, sum.MeanLatencyMS)
	}
	if got.Kpis.PricedCost == nil || math.Abs(*got.Kpis.PricedCost-sum.PricedCostUSD) > 1e-9 {
		t.Errorf("kpis.pricedCost = %v, want %v", got.Kpis.PricedCost, sum.PricedCostUSD)
	}
	if got.Kpis.CostStatus != sum.CostStatus || got.Kpis.CostLabel != "priced rows only" {
		t.Errorf("kpis cost = %q / %q, want priced_only / priced rows only", got.Kpis.CostStatus, got.Kpis.CostLabel)
	}
	if got.Kpis.ErrorRate == nil || *got.Kpis.ErrorRate != 0 {
		t.Errorf("kpis.errorRate = %v, want 0", got.Kpis.ErrorRate)
	}
	if !strings.Contains(got.Kpis.Note, "priced_cost_usd covers priced rows only") {
		t.Errorf("kpis.note = %q, want the feed's priced-only note", got.Kpis.Note)
	}

	// Provider rows equal the by-provider feed.
	if len(got.ProvRows) != len(bp.Providers) {
		t.Fatalf("provRows = %d rows, want %d (by-provider feed)", len(got.ProvRows), len(bp.Providers))
	}
	if got.ProvRows[0].Provider != bp.Providers[0].Provider || got.ProvRows[0].Requests != bp.Providers[0].Requests ||
		got.ProvRows[0].TotalTokens != bp.Providers[0].TotalTokens || math.Abs(got.ProvRows[0].PricedCost-bp.Providers[0].PricedCostUSD) > 1e-9 ||
		got.ProvRows[0].UnpricedRequests != bp.Providers[0].UnpricedRequests || !got.ProvRows[0].UnpricedKnown {
		t.Errorf("provRows[0] = %+v, want the by-provider feed row %+v", got.ProvRows[0], bp.Providers[0])
	}

	// Model rows equal the by-model feed (order is by requests desc;
	// both rows have 1 request, so compare as a set).
	if len(got.ModelRows) != len(bm.Models) {
		t.Fatalf("modelRows = %d rows, want %d (by-model feed)", len(got.ModelRows), len(bm.Models))
	}
	wantModels := map[string]analyticsModelRow{}
	for _, m := range bm.Models {
		wantModels[m.Model] = m
	}
	for _, r := range got.ModelRows {
		w, ok := wantModels[r.Model]
		if !ok {
			t.Errorf("modelRows has unexpected model %q", r.Model)
			continue
		}
		if r.Provider != w.Provider || r.Requests != w.Requests || r.TotalTokens != w.TotalTokens ||
			math.Abs(r.PricedCost-w.PricedCostUSD) > 1e-9 || r.UnpricedRequests != w.UnpricedRequests || !r.UnpricedKnown {
			t.Errorf("modelRows %q = %+v, want the by-model feed row %+v", r.Model, r, w)
		}
	}

	// Costs equal the costs feed.
	if got.Costs.Requests == nil || *got.Costs.Requests != co.Requests {
		t.Errorf("costs.requests = %v, want %d", got.Costs.Requests, co.Requests)
	}
	if got.Costs.PricedCost == nil || math.Abs(*got.Costs.PricedCost-co.PricedCostUSD) > 1e-9 {
		t.Errorf("costs.pricedCost = %v, want %v", got.Costs.PricedCost, co.PricedCostUSD)
	}
	if got.Costs.UnpricedRequests == nil || *got.Costs.UnpricedRequests != *co.UnpricedRequests {
		t.Errorf("costs.unpricedRequests = %v, want %v", got.Costs.UnpricedRequests, co.UnpricedRequests)
	}
	if got.Costs.UnpricedTokens == nil || *got.Costs.UnpricedTokens != *co.UnpricedTokens {
		t.Errorf("costs.unpricedTokens = %v, want %v", got.Costs.UnpricedTokens, co.UnpricedTokens)
	}
	if got.Costs.CostStatus != co.CostStatus || got.Costs.CostLabel != "priced rows only" {
		t.Errorf("costs status = %q / %q, want priced_only / priced rows only", got.Costs.CostStatus, got.Costs.CostLabel)
	}

	// Empty feeds render nulls, never zeros.
	if got.EmptyKpis.Requests != nil {
		t.Errorf("empty kpis.requests = %v, want null (No data yet, not 0)", *got.EmptyKpis.Requests)
	}
	if got.EmptyKpis.CostLabel != "no data yet" {
		t.Errorf("empty kpis.costLabel = %q, want no data yet", got.EmptyKpis.CostLabel)
	}
	if got.EmptyKpis.Note != "No data yet — no usage rows in this window." {
		t.Errorf("empty kpis.note = %q, want the No data yet note", got.EmptyKpis.Note)
	}
	if len(got.EmptyProv) != 0 || len(got.EmptyModel) != 0 {
		t.Errorf("empty rows = %d providers / %d models, want 0 / 0", len(got.EmptyProv), len(got.EmptyModel))
	}
	if got.EmptyCosts.Requests != nil || got.EmptyCosts.CostLabel != "no data yet" {
		t.Errorf("empty costs = requests %v / label %q, want null / no data yet", got.EmptyCosts.Requests, got.EmptyCosts.CostLabel)
	}

	// Formatters: the exact strings the page shows for the feed values.
	if got.FmtUSD != "$0.04" {
		t.Errorf("m10_15FmtUSD(0.04) = %q, want $0.04", got.FmtUSD)
	}
	if got.FmtInt != "3,000" {
		t.Errorf("m10_15FmtInt(3000) = %q, want 3,000", got.FmtInt)
	}
}

// TestDashboardM10_15CutCommentIsAccurate pins the wording of the
// CUT comment at the top of this file: the four G17 handlers were
// removed as dead code in the M10-15 cleanup — they existed as
// code but had no registered/guarded endpoint, so they were
// unreachable over HTTP. The test reads its own file header
// (up to "package server", so the test's own code below cannot
// satisfy the check) so a future edit cannot silently reintroduce
// the overclaim that no backend combo-metrics endpoint exists
// at all, nor claim the handlers still exist as code.
func TestDashboardM10_15CutCommentIsAccurate(t *testing.T) {
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not locate this test file")
	}
	src, err := os.ReadFile(here)
	if err != nil {
		t.Fatalf("read this test file: %v", err)
	}
	head := string(src)
	if i := strings.Index(head, "package server"); i >= 0 {
		head = head[:i]
	}
	// The old overclaim, built by concatenation so this test's own
	// source does not itself contain the banned phrase.
	oldClaim := "no backend combo-metrics endpoint " + "exists"
	if strings.Contains(head, oldClaim) {
		t.Error("CUT comment still claims no backend combo-metrics endpoint exists — the handlers were removed as dead code; what was missing was the registered/guarded endpoint")
	}
	// The stale claim that the handlers still exist as code,
	// built by concatenation for the same reason.
	staleClaim := "exist as code " + "— but there is"
	if strings.Contains(head, staleClaim) {
		t.Error("CUT comment still claims the four handlers exist as code — they were removed as dead code in the M10-15 cleanup")
	}
	if !strings.Contains(head, "no registered/guarded endpoint") {
		t.Error("CUT comment must say there was \"no registered/guarded endpoint\" for the four handlers")
	}
	if !strings.Contains(head, "removed as dead code") {
		t.Error("CUT comment must record that the four handlers were \"removed as dead code\" in the M10-15 cleanup")
	}
	for _, want := range []string{
		"handleAnalyticsComboHealth",
		"handleAnalyticsAutoRouting",
		"handleAnalyticsCompression",
		"handleAnalyticsDiversity",
		"s.register in server.go",
		"guard.go row",
		"unreachable over HTTP",
	} {
		if !strings.Contains(head, want) {
			t.Errorf("CUT comment must name %q (the precise reason the tabs stay cut)", want)
		}
	}
}

// TestDashboardM10_15LivePillAfterLoad proves the "Live" pills are
// absent before the first load and present after a successful load:
// the served page hides all six pills (display:none), and driving
// the served m10_15RenderAnalytics with the REAL feed responses
// under node reveals them and fills the cards with the feed values.
// A second render with the empty feeds keeps the pills visible —
// the feeds answered, so "Live" is honest even at 0 traffic — and
// the cards read "No data yet".
func TestDashboardM10_15LivePillAfterLoad(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — cannot run the render driver")
	}
	setup, srv := m10_15AdminSetup(t)
	seedAnalyticsRows(t, setup)

	var sum analyticsSummaryResponse
	m10_15GetJSON(t, srv, "/api/analytics/summary?window=day", setup.raw, &sum)
	var bp analyticsByProviderResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-provider?window=day", setup.raw, &bp)
	var bm analyticsByModelResponse
	m10_15GetJSON(t, srv, "/api/analytics/by-model?window=day", setup.raw, &bm)
	var co analyticsCostsResponse
	m10_15GetJSON(t, srv, "/api/analytics/costs?window=day", setup.raw, &co)

	srv2 := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv2, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := analyticsSection(t, body)
	if n := strings.Count(section, `class="pill flat m10_15LivePill" style="display:none">Live</span>`); n != 6 {
		t.Fatalf("served Analytics section has %d hidden Live pills, want 6 (one per KPI card)", n)
	}
	script := analyticsInlineScript(t, body)
	start := strings.Index(script, "/* M10-15-PURE-START */")
	end := strings.Index(script, "/* M10-15-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-15-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]

	mustJSON := func(t *testing.T, v any) string {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal feed for node driver: %v", err)
		}
		return string(b)
	}
	// The render and its helpers, extracted from the served script
	// itself — not a copy — driven against a minimal DOM stub: six
	// pills hidden before the load, element stubs for the cards.
	driver := `
var pills = [];
for (var i = 0; i < 6; i++) pills.push({style: {display: "none"}});
var els = {};
function $(id){ if(!els[id]) els[id] = {textContent: "", style: {display: ""}, innerHTML: "", appendChild: function(){}}; return els[id]; }
function $$(sel){ return sel === ".m10_15LivePill" ? pills : []; }
var document = {createElement: function(){ return {innerHTML: "", appendChild: function(){}}; }};
function escHtml(s){ return String(s); }
` + pure + "\n" +
		m10_15Function(t, body, "m10_15SetText") + "\n" +
		m10_15Function(t, body, "m10_15ShowError") + "\n" +
		m10_15Function(t, body, "m10_15RenderAnalytics") + "\n" +
		`var summary = ` + mustJSON(t, sum) + `;
var byProv = ` + mustJSON(t, bp) + `;
var byModel = ` + mustJSON(t, bm) + `;
var costs = ` + mustJSON(t, co) + `;
var emptySummary = {has_data: false, cost_status: "no_data", requests: 0};
var emptyByProv = {has_data: false, providers: []};
var emptyByModel = {has_data: false, models: []};
var emptyCosts = {has_data: false, cost_status: "no_data", requests: 0};
var pillsBefore = pills.map(function(p){ return p.style.display; });
m10_15RenderAnalytics(summary, byProv, byModel, costs);
var pillsAfter = pills.map(function(p){ return p.style.display; });
var out = {
  pillsBefore: pillsBefore,
  pillsAfter: pillsAfter,
  kpiRequests: els["#m10_15KpiRequests"].textContent,
  kpiTokens: els["#m10_15KpiTokens"].textContent,
  kpiSpend: els["#m10_15KpiSpend"].textContent,
  kpiCostStatus: els["#m10_15KpiCostStatus"].textContent,
  costsSpend: els["#m10_15CostsSpend"].textContent
};
m10_15RenderAnalytics(emptySummary, emptyByProv, emptyByModel, emptyCosts);
out.pillsAfterEmpty = pills.map(function(p){ return p.style.display; });
out.kpiRequestsEmpty = els["#m10_15KpiRequests"].textContent;
console.log(JSON.stringify(out));
`
	dir := t.TempDir()
	driverPath := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v: %s", err, raw)
	}
	var got struct {
		PillsBefore      []string `json:"pillsBefore"`
		PillsAfter       []string `json:"pillsAfter"`
		KpiRequests      string   `json:"kpiRequests"`
		KpiTokens        string   `json:"kpiTokens"`
		KpiSpend         string   `json:"kpiSpend"`
		KpiCostStatus    string   `json:"kpiCostStatus"`
		CostsSpend       string   `json:"costsSpend"`
		PillsAfterEmpty  []string `json:"pillsAfterEmpty"`
		KpiRequestsEmpty string   `json:"kpiRequestsEmpty"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v (raw %s)", err, raw)
	}

	// Before the first load every pill is hidden.
	if len(got.PillsBefore) != 6 {
		t.Fatalf("pills before load = %d, want 6", len(got.PillsBefore))
	}
	for i, d := range got.PillsBefore {
		if d != "none" {
			t.Errorf("pill %d display before load = %q, want \"none\"", i, d)
		}
	}
	// After a successful load every pill is shown, and the cards
	// hold the feed values — nothing fabricated.
	if len(got.PillsAfter) != 6 {
		t.Fatalf("pills after load = %d, want 6", len(got.PillsAfter))
	}
	for i, d := range got.PillsAfter {
		if d != "" {
			t.Errorf("pill %d display after load = %q, want \"\" (visible)", i, d)
		}
	}
	if got.KpiRequests != "2" {
		t.Errorf("kpiRequests after load = %q, want \"2\" (summary feed)", got.KpiRequests)
	}
	if got.KpiTokens != "3,000" {
		t.Errorf("kpiTokens after load = %q, want \"3,000\" (summary feed)", got.KpiTokens)
	}
	if got.KpiSpend != "$0.04" {
		t.Errorf("kpiSpend after load = %q, want \"$0.04\" (summary feed)", got.KpiSpend)
	}
	if got.KpiCostStatus != "priced rows only" {
		t.Errorf("kpiCostStatus after load = %q, want \"priced rows only\"", got.KpiCostStatus)
	}
	if got.CostsSpend != "$0.04" {
		t.Errorf("costsSpend after load = %q, want \"$0.04\" (costs feed)", got.CostsSpend)
	}
	// Empty feeds: pills stay visible (the feeds answered) and the
	// cards read "No data yet", never zeros.
	for i, d := range got.PillsAfterEmpty {
		if d != "" {
			t.Errorf("pill %d display after empty load = %q, want \"\" (visible)", i, d)
		}
	}
	if got.KpiRequestsEmpty != "No data yet" {
		t.Errorf("kpiRequests after empty load = %q, want \"No data yet\"", got.KpiRequestsEmpty)
	}
}
