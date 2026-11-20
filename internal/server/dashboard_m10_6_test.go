// M10-6 dashboard Costs: the Usage view expanded in place into
// Costs — side-by-side Spend 1D/7D/30D + requests + active
// providers/models + avg cost/req as the KPI row, Cost Explorer
// lite (dimension filter, top-50 table, CSV of exactly the shown
// rows), Cost by key (per-key table from the same
// GET /api/analytics/costs call that loads avg cost — no second
// fetch, priced cost labelled a lower bound where unpriced
// requests exist, no combined figure shown), Monthly Forecast as
// a labelled linear projection, quota-share pools UI and teams UI.
//
// AVG-COST FINDING (verified against the feeds, not assumed): GET
// /api/analytics/costs carries priced_cost_usd, requests (TOTAL
// requests in the window), unpriced_requests and cost_status — and
// NO avg-cost field. Avg cost/req is therefore computable ONLY as
// priced_cost_usd / (requests - unpriced_requests), and only when
// unpriced_requests is present (the priced/unpriced split was
// computed). When the split is unavailable the feed omits
// unpriced_requests and the dashboard must show avg cost as
// unavailable, never priced_cost_usd / total requests (that would
// count unpriced rows as $0). The JS pure helper costsAvgPerReq
// encodes exactly that rule and is executed under node below.
//
// AUTH FINDING: GET /api/analytics/* are admin-scoped
// (guard.go), so the dashboard — which holds no stored key —
// cannot auto-load them. The KPI spend/requests/active figures
// and the explorer therefore read the auth-exempt GET
// /dashboard/usage feed, and avg cost loads only when the operator
// types a gateway key and presses "Load avg cost". The test below
// proves the 401 without a key and the feed equality with one.
package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// costsSection returns the served HTML of <section id="view-costs">.
func costsSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-costs"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-costs section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-costs section is not closed")
	}
	return rest[:end]
}

// costsInlineScript returns the served dashboard's single inline
// <script> block.
func costsInlineScript(t *testing.T, body string) string {
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

func TestDashboardM10_6CostsRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := costsSection(t, body)

	for _, want := range []string{
		// Renamed in place: Costs, not "Usage & Cost".
		"<h1>Costs</h1>",
		// KPI row: side-by-side Spend 1D/7D/30D.
		`id="costSpendDay"`, `id="costSpendWeek"`, `id="costSpendMonth"`,
		"Spend · last 24 hours", "Spend · last 7 days", "Spend · last 30 days",
		// Requests + active providers/models + avg cost/req.
		`id="costRequests"`, `id="costActive"`, `id="costAvgReq"`,
		"Requests · last 30 days", "Active providers / models", "Avg cost / request",
		// Avg cost loads only with the operator key.
		`id="costAvgLoadBtn"`, "Load avg cost",
		// Cost Explorer lite.
		`id="costExplorerDim"`, `id="costExplorerBody"`, `id="costExplorerCsvBtn"`,
		"Cost Explorer", "Export CSV", "Top 50 by cost",
		// Cost by key: the key dimension the old note said did
		// not exist. Per-key table from the same costs feed
		// that loads avg cost — no second fetch.
		`id="costByKeyBody"`, `id="costByKeyEmpty"`, `id="costByKeyLoadBtn"`,
		"Cost by key", "Load per-key costs", "No key usage in this window",
		"Priced cost (lower bound where unpriced requests exist)",
		"Per-key breakdown is in Cost by key below",
		// Monthly Forecast: labelled estimate with its basis.
		`id="costForecast"`, `id="costForecastBasis"`,
		"Monthly forecast", "estimate from the last 30 days, not a commitment",
		// Quota-share pools UI.
		`id="costPoolsBody"`, `id="costPoolSaveBtn"`, `id="costPoolDeleteBtn"`,
		"Quota-share pools", "Create / replace pool", "No quota pools yet",
		// Teams UI.
		`id="costTeamsBody"`, `id="costTeamSaveBtn"`, `id="costTeamDeleteBtn"`,
		`id="costTeamMemberAssignBtn"`, `id="costTeamMemberUnassignBtn"`,
		"Teams", "Create / replace team", "No teams yet",
		// The section still keeps the pre-existing detail cards.
		"Spend by provider", "Spend by model", "Per-key rolling budgets",
		"Per-request budgets", "token-count drift",
		// The JS reads the live feeds (usage auth-exempt; analytics
		// admin-scoped, via the operator key).
		"/dashboard/usage", "/api/analytics/costs",
		"/dashboard/quota-pools", "/dashboard/teams", "/dashboard/keys",
	} {
		if !strings.Contains(section, want) && !strings.Contains(body, want) {
			t.Errorf("dashboard Costs missing %q", want)
		}
	}

	// The view itself is Costs now, not Usage: nav, section id,
	// titles map and go() dispatch all use "costs", and no old
	// "usage" view name remains anywhere in the served page.
	// (The /dashboard/usage feed path and loadUsage() stay: those
	// are backend/feed names, not view names.)
	for _, want := range []string{
		`data-view="costs"`, `id="view-costs"`, `costs:"Costs"`,
		`data-goto="costs"`, `if(view === "costs")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard Costs view missing %q", want)
		}
	}
	for _, gone := range []string{
		`id="view-usage"`, `data-view="usage"`, `data-goto="usage"`,
		`usage:"Usage"`, `if(view === "usage")`, `#view-usage`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("dashboard still contains old Usage view name %q (renamed to Costs)", gone)
		}
	}

	// The old single-range KPI ids are gone: the KPI row is now the
	// side-by-side Costs row.
	for _, gone := range []string{
		`id="usageSpend"`, `id="usageTok"`, `id="usageFailovers"`,
		`id="usageSpendLabel"`, `id="usageSpendPill"`, `id="usageSpendNote"`,
		`id="usageTokPill"`, `id="usageFailoversPill"`,
		"Usage &amp; Cost", "Usage & Cost",
	} {
		if strings.Contains(section, gone) {
			t.Errorf("dashboard Costs still contains %q", gone)
		}
	}

	// Quota-schedules and internal-budgets get NO UI in M10.
	for _, banned := range []string{"quota-schedules", "internal-budgets", "Quota schedules", "Internal budgets"} {
		if strings.Contains(section, banned) {
			t.Errorf("Costs section must not contain %q (out of M10 scope)", banned)
		}
	}

	// No fake-data markers, no Playground.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(section, banned) {
			t.Errorf("Costs section contains %q", banned)
		}
	}
}

// The feeds the Costs view reads must answer on a live server:
// usage (auth-exempt) for every range, quota-pools and teams
// (auth-exempt reads, empty lists with nothing configured).
func TestDashboardM10_6CostsFeedsAnswer(t *testing.T) {
	// Teams 503s with no ledger wired (pre-existing handler
	// behaviour), so drive this against a ledger-wired server.
	_, srv := analyticsSetup(t)
	for _, path := range []string{
		"/dashboard/usage?range=24h",
		"/dashboard/usage?range=7d",
		"/dashboard/usage?range=30d",
		"/dashboard/quota-pools",
		"/dashboard/teams",
		"/dashboard/keys",
	} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
	}
}

// KPI values equal /api/analytics/costs for the same range: with
// rows seeded "now" they fall in both the usage rolling windows
// (24h/7d/30d) and the analytics calendar windows (day/week/
// month), so priced spend and request counts must agree exactly.
// (Window definitions differ — rolling vs calendar — so on real
// data spanning midnight UTC the two can differ; the equality
// holds for same-day data, which is what the seed proves.)
func TestDashboardM10_6KpiFeedsEqualAnalyticsCosts(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s) // 1 priced row ($0.04) + 1 unpriced row
	// The analytics GETs are admin-scoped: widen this test's own
	// fixture key to chat+admin (same pattern as the pool/team
	// POST tests).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}

	// Without a key the analytics feed refuses: this is why the
	// dashboard cannot auto-load it (see the avg-cost test).
	rec := doReq(t, srv, http.MethodGet, "/api/analytics/costs?window=day", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/analytics/costs without a key status = %d, want 401", rec.Code)
	}

	for _, tc := range []struct{ usageRange, window string }{
		{"24h", "day"}, {"7d", "week"}, {"30d", "month"},
	} {
		rec := doReq(t, srv, http.MethodGet, "/dashboard/usage?range="+tc.usageRange, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/usage?range=%s status = %d, want 200", tc.usageRange, rec.Code)
		}
		var us usageResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &us); err != nil {
			t.Fatalf("decode usage %s: %v", tc.usageRange, err)
		}
		rec = doReq(t, srv, http.MethodGet, "/api/analytics/costs?window="+tc.window, "", s.raw)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/analytics/costs?window=%s status = %d, want 200: %s", tc.window, rec.Code, rec.Body.String())
		}
		var costs analyticsCostsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &costs); err != nil {
			t.Fatalf("decode costs %s: %v", tc.window, err)
		}
		if us.Summary.CostUSD != costs.PricedCostUSD {
			t.Errorf("usage %s cost %v != analytics %s priced_cost_usd %v (want equal: $0.04 priced, unpriced row records $0)",
				tc.usageRange, us.Summary.CostUSD, tc.window, costs.PricedCostUSD)
		}
		if us.Summary.Requests != costs.Requests {
			t.Errorf("usage %s requests %d != analytics %s requests %d (want 2)",
				tc.usageRange, us.Summary.Requests, tc.window, costs.Requests)
		}
		if costs.PricedCostUSD != 0.04 || costs.Requests != 2 || costs.CostStatus != "priced_only" {
			t.Errorf("analytics costs %s = %+v, want priced 0.04 / requests 2 / priced_only", tc.window, costs)
		}
		if costs.UnpricedRequests == nil || *costs.UnpricedRequests != 1 {
			t.Errorf("analytics costs %s unpriced_requests = %v, want 1", tc.window, costs.UnpricedRequests)
		}
	}

	// The by-provider / by-model breakdowns agree with the same
	// totals, so the explorer's rows sum to the feed totals.
	rec = doReq(t, srv, http.MethodGet, "/api/analytics/by-provider?window=day", "", s.raw)
	var byProv analyticsByProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &byProv); err != nil {
		t.Fatalf("decode by-provider: %v", err)
	}
	var provCost float64
	var provReq int
	for _, p := range byProv.Providers {
		provCost += p.PricedCostUSD
		provReq += p.Requests
	}
	if provCost != 0.04 || provReq != 2 {
		t.Errorf("by-provider sums = cost %v / requests %d, want 0.04 / 2", provCost, provReq)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/analytics/by-model?window=day", "", s.raw)
	var byModel analyticsByModelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &byModel); err != nil {
		t.Fatalf("decode by-model: %v", err)
	}
	var modelCost float64
	var modelReq int
	for _, m := range byModel.Models {
		modelCost += m.PricedCostUSD
		modelReq += m.Requests
	}
	if modelCost != 0.04 || modelReq != 2 {
		t.Errorf("by-model sums = cost %v / requests %d, want 0.04 / 2", modelCost, modelReq)
	}
}

// Avg cost/req computability, at the feed level: the priced
// request count exists exactly when the feed carries
// unpriced_requests. Complete split (0 unpriced) and priced-only
// split (some unpriced) are computable; a missing split (no
// config wired, cost_status "unknown") is not — the dashboard
// must show avg cost as unavailable there, never divide priced
// spend by total requests.
func TestDashboardM10_6AvgCostComputability(t *testing.T) {
	// Complete: 1 priced row, 0 unpriced.
	s, srv := analyticsSetup(t)
	seedAnalyticsPricedOnly(t, s)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	rec := doReq(t, srv, http.MethodGet, "/api/analytics/costs?window=day", "", s.raw)
	var complete analyticsCostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &complete); err != nil {
		t.Fatalf("decode complete costs: %v", err)
	}
	if complete.UnpricedRequests == nil || *complete.UnpricedRequests != 0 || complete.Requests != 1 {
		t.Fatalf("complete costs = %+v, want requests 1 / unpriced 0", complete)
	}
	priced := complete.Requests - *complete.UnpricedRequests
	if priced != 1 || math.Abs(complete.PricedCostUSD/float64(priced)-0.04) > 1e-9 {
		t.Errorf("complete avg = priced %v / %d priced requests, want $0.04/request", complete.PricedCostUSD, priced)
	}

	// Priced-only: 2 requests, 1 unpriced — priced count is 1.
	s2, srv2 := analyticsSetup(t)
	seedAnalyticsRows(t, s2)
	for i := range s2.cfg.GatewayKeys {
		if s2.cfg.GatewayKeys[i].ID == s2.id {
			s2.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	rec = doReq(t, srv2, http.MethodGet, "/api/analytics/costs?window=day", "", s2.raw)
	var mixed analyticsCostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mixed); err != nil {
		t.Fatalf("decode mixed costs: %v", err)
	}
	if mixed.UnpricedRequests == nil || *mixed.UnpricedRequests != 1 || mixed.Requests != 2 {
		t.Fatalf("mixed costs = %+v, want requests 2 / unpriced 1", mixed)
	}
	priced = mixed.Requests - *mixed.UnpricedRequests
	if priced != 1 || math.Abs(mixed.PricedCostUSD/float64(priced)-0.04) > 1e-9 {
		t.Errorf("priced-only avg = priced %v / %d priced requests, want $0.04 per PRICED request", mixed.PricedCostUSD, priced)
	}

	// Unknown split: no config wired, so the feed omits
	// unpriced_requests entirely — the priced count is unknown.
	s3, _ := keyStatusSetup(t)
	seedAnalyticsPricedOnly(t, s3)
	srv3 := newTestServer(t, &fakeEngine{}, WithLedger(s3.led))
	rec = callAnalytics(t, srv3.handleAnalyticsCosts, http.MethodGet, "/api/analytics/costs?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("no-config costs status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var unknown analyticsCostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &unknown); err != nil {
		t.Fatalf("decode unknown costs: %v", err)
	}
	if unknown.UnpricedRequests != nil {
		t.Errorf("no-config costs unpriced_requests = %v, want absent (split unknown)", *unknown.UnpricedRequests)
	}
	if unknown.CostStatus != "unknown" {
		t.Errorf("no-config costs cost_status = %q, want unknown", unknown.CostStatus)
	}
	// And the served dashboard says the card is unavailable until
	// the operator key loads it — it never prints a guessed value.
	dashSrv := newTestServer(t, &fakeEngine{})
	dash := doReq(t, dashSrv, http.MethodGet, "/dashboard", "", "")
	if !strings.Contains(dash.Body.String(), "Avg cost needs the admin-scoped analytics feed") {
		t.Error("dashboard must explain that avg cost needs the admin-scoped analytics feed")
	}
}

// Explorer top-50 rows sum to the feed totals: the usage summary's
// by-provider / by-model roll-ups sum to the summary totals, so a
// top-50 explorer over a feed with <= 50 rows reproduces them.
func TestDashboardM10_6ExplorerSumsToFeedTotals(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/usage?range=30d", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/usage status = %d, want 200", rec.Code)
	}
	var us usageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &us); err != nil {
		t.Fatalf("decode usage: %v", err)
	}
	var provCost, modelCost float64
	var provReq, modelReq, provTok, modelTok int
	for _, p := range us.Summary.ByProvider {
		provCost += p.CostUSD
		provReq += p.Requests
		provTok += p.TotalTokens
	}
	for _, m := range us.Summary.ByModel {
		modelCost += m.CostUSD
		modelReq += m.Requests
		modelTok += m.TotalTokens
	}
	if provCost != us.Summary.CostUSD || modelCost != us.Summary.CostUSD {
		t.Errorf("by-provider cost %v / by-model cost %v != summary cost %v", provCost, modelCost, us.Summary.CostUSD)
	}
	if provReq != us.Summary.Requests || modelReq != us.Summary.Requests {
		t.Errorf("by-provider requests %d / by-model requests %d != summary requests %d", provReq, modelReq, us.Summary.Requests)
	}
	if provTok != us.Summary.TotalTokens || modelTok != us.Summary.TotalTokens {
		t.Errorf("by-provider tokens %d / by-model tokens %d != summary tokens %d", provTok, modelTok, us.Summary.TotalTokens)
	}
}

// Pool create/delete round-trip through the endpoints the pools UI
// posts to (GET/POST /dashboard/quota-pools).
func TestDashboardM10_6PoolCreateDeleteRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	create := `{"id":"pool-m106","name":"Costs pool","saturation_threshold":0.5,` +
		`"dimensions":[{"unit":"requests","window":"daily","limit":100}],` +
		`"allocations":[{"key_id":"` + s.id + `","weight":50,"policy":"hard"}]}`
	rec := doReq(t, srv, "POST", "/dashboard/quota-pools", create, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST pool status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Replace (same id, new limit) — the UI's create/replace form.
	replace := `{"id":"pool-m106","name":"Costs pool","saturation_threshold":0.8,` +
		`"dimensions":[{"unit":"requests","window":"daily","limit":200}],` +
		`"allocations":[{"key_id":"` + s.id + `","weight":50,"policy":"hard"}]}`
	rec = doReq(t, srv, "POST", "/dashboard/quota-pools", replace, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST pool replace status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/quota-pools", "", s.raw)
	var out quotaPoolsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pools: %v", err)
	}
	if len(out.Pools) != 1 || out.Pools[0].ID != "pool-m106" {
		t.Fatalf("GET pools = %+v, want 1 pool pool-m106", out.Pools)
	}
	if out.Pools[0].Dimensions[0].Limit != 200 || out.Pools[0].SaturationThreshold != 0.8 {
		t.Errorf("replaced pool = %+v, want limit 200 / saturation 0.8", out.Pools[0])
	}
	if len(out.Pools[0].Allocations) != 1 || out.Pools[0].Allocations[0].KeyID != s.id {
		t.Errorf("pool allocations = %+v, want 1 member %s", out.Pools[0].Allocations, s.id)
	}
	rec = doReq(t, srv, "POST", "/dashboard/quota-pools", `{"id":"pool-m106","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE pool status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/quota-pools", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode pools after delete: %v", err)
	}
	if len(out.Pools) != 0 {
		t.Errorf("GET after delete = %d pools, want 0", len(out.Pools))
	}
}

// Team create + member assign round-trip through the endpoints the
// teams UI posts to (GET/POST /dashboard/teams + members).
func TestDashboardM10_6TeamCreateMemberRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	rec := doReq(t, srv, "POST", "/dashboard/teams", `{"id":"tm106","name":"Costs team","max_budget_usd":5,"budget_duration":"7d"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST team status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/dashboard/teams/tm106/members", `{"key_id":"`+s.id+`","action":"assign"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign member status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/teams", "", s.raw)
	var out teamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode teams: %v", err)
	}
	if len(out.Teams) != 1 || out.Teams[0].ID != "tm106" {
		t.Fatalf("GET teams = %+v, want 1 team tm106", out.Teams)
	}
	if len(out.Teams[0].Members) != 1 || out.Teams[0].Members[0].KeyID != s.id {
		t.Errorf("team members = %+v, want 1 member %s", out.Teams[0].Members, s.id)
	}
	rec = doReq(t, srv, "POST", "/dashboard/teams/tm106/members", `{"key_id":"`+s.id+`","action":"unassign"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign member status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/dashboard/teams", `{"id":"tm106","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE team status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/teams", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode teams after delete: %v", err)
	}
	if len(out.Teams) != 0 {
		t.Errorf("GET after delete = %d teams, want 0", len(out.Teams))
	}
}

// The Costs pure helpers run under node, against the exact code
// served in the dashboard: node --check on the whole extracted
// inline script, then the pure block (avg-cost rule, explorer
// top-50 + totals, CSV of exactly the shown rows, forecast) driven
// with fixture data.
func TestDashboardM10_6PureHelpersUnderNode(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — cannot run the inline-script check")
	}
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	script := costsInlineScript(t, rec.Body.String())
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "inline.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("node", "--check", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("node --check on the extracted inline script failed: %v: %s", err, out)
	}
	start := strings.Index(script, "/* M10-6-PURE-START */")
	end := strings.Index(script, "/* M10-6-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-6-PURE-START/END) not found in the inline script")
	}
	pure := script[start:end]
	driver := pure + `
var summary = {by_provider: [{provider: "p", requests: 2, total_tokens: 3000, cost_usd: 0.04}, {provider: "q", requests: 1, total_tokens: 500, cost_usd: 0.01}], by_model: [{provider: "p", model: "priced-model", requests: 1, total_tokens: 2000, cost_usd: 0.04}, {provider: "p", model: "mock-model", requests: 1, total_tokens: 1000, cost_usd: 0}]};
var many = [];
for (var i = 0; i < 60; i++) many.push({provider: "prov" + i, requests: 1, total_tokens: 10, cost_usd: 60 - i});
var big = {by_provider: many, by_model: []};
var out = {
  avgComplete: costsAvgPerReq({requests: 1, priced_cost_usd: 0.04, unpriced_requests: 0}),
  avgPricedOnly: costsAvgPerReq({requests: 2, priced_cost_usd: 0.04, unpriced_requests: 1}),
  avgUnknown: costsAvgPerReq({requests: 2, priced_cost_usd: 0.04}),
  avgEmpty: costsAvgPerReq({requests: 0, priced_cost_usd: 0, unpriced_requests: 0}),
  explorerProvider: costsExplorerRows(summary, "provider"),
  explorerModel: costsExplorerRows(summary, "model"),
  explorerTop50: costsExplorerRows(big, "provider"),
  csvProvider: costsCsv(costsExplorerRows(summary, "provider").rows, "provider"),
  csvModel: costsCsv(costsExplorerRows(summary, "model").rows, "model"),
  forecast: costsForecast(3.00, "2026-01-15T00:00:00Z"),
  forecastEmpty: costsForecast(0)
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
		AvgComplete struct {
			Value          *float64 `json:"value"`
			PricedRequests *int     `json:"pricedRequests"`
		} `json:"avgComplete"`
		AvgPricedOnly struct {
			Value          *float64 `json:"value"`
			PricedRequests *int     `json:"pricedRequests"`
		} `json:"avgPricedOnly"`
		AvgUnknown struct {
			Value *float64 `json:"value"`
		} `json:"avgUnknown"`
		AvgEmpty struct {
			Value *float64 `json:"value"`
		} `json:"avgEmpty"`
		ExplorerProvider struct {
			Rows []struct {
				Label    string  `json:"label"`
				Requests int     `json:"requests"`
				Tokens   int     `json:"tokens"`
				Cost     float64 `json:"cost"`
			} `json:"rows"`
			TotalRows int `json:"totalRows"`
			Totals    struct {
				Requests int     `json:"requests"`
				Tokens   int     `json:"tokens"`
				Cost     float64 `json:"cost"`
			} `json:"totals"`
		} `json:"explorerProvider"`
		ExplorerModel struct {
			Rows []struct {
				Label string `json:"label"`
			} `json:"rows"`
			Totals struct {
				Requests int     `json:"requests"`
				Tokens   int     `json:"tokens"`
				Cost     float64 `json:"cost"`
			} `json:"totals"`
		} `json:"explorerModel"`
		ExplorerTop50 struct {
			Rows []struct {
				Cost float64 `json:"cost"`
			} `json:"rows"`
			TotalRows int `json:"totalRows"`
			Totals    struct {
				Cost float64 `json:"cost"`
			} `json:"totals"`
		} `json:"explorerTop50"`
		CSVProvider string `json:"csvProvider"`
		CSVModel    string `json:"csvModel"`
		Forecast    struct {
			Value         *float64 `json:"value"`
			DailyRate     float64  `json:"dailyRate"`
			DaysNextMonth int      `json:"daysNextMonth"`
		} `json:"forecast"`
		ForecastEmpty struct {
			Value *float64 `json:"value"`
		} `json:"forecastEmpty"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node driver output: %v: %s", err, raw)
	}
	// Avg cost: complete and priced-only both give $0.04 over 1
	// priced request; unknown split and empty window give null.
	if got.AvgComplete.Value == nil || *got.AvgComplete.Value != 0.04 || got.AvgComplete.PricedRequests == nil || *got.AvgComplete.PricedRequests != 1 {
		t.Errorf("avg complete = %+v, want value 0.04 / pricedRequests 1", got.AvgComplete)
	}
	if got.AvgPricedOnly.Value == nil || *got.AvgPricedOnly.Value != 0.04 || got.AvgPricedOnly.PricedRequests == nil || *got.AvgPricedOnly.PricedRequests != 1 {
		t.Errorf("avg priced-only = %+v, want value 0.04 / pricedRequests 1 (unpriced row excluded, not $0)", got.AvgPricedOnly)
	}
	if got.AvgUnknown.Value != nil {
		t.Errorf("avg unknown-split = %v, want null (no priced count without unpriced_requests)", *got.AvgUnknown.Value)
	}
	if got.AvgEmpty.Value != nil {
		t.Errorf("avg empty = %v, want null", *got.AvgEmpty.Value)
	}
	// Explorer: provider rows sorted by cost desc, totals sum to
	// the feed totals (3 requests / 3500 tokens / $0.05).
	if len(got.ExplorerProvider.Rows) != 2 || got.ExplorerProvider.Rows[0].Label != "p" || got.ExplorerProvider.Rows[1].Label != "q" {
		t.Errorf("explorer provider rows = %+v, want [p, q] by cost desc", got.ExplorerProvider.Rows)
	}
	if got.ExplorerProvider.Totals.Requests != 3 || got.ExplorerProvider.Totals.Tokens != 3500 || got.ExplorerProvider.Totals.Cost != 0.05 {
		t.Errorf("explorer provider totals = %+v, want 3 / 3500 / 0.05", got.ExplorerProvider.Totals)
	}
	if len(got.ExplorerModel.Rows) != 2 || got.ExplorerModel.Rows[0].Label != "priced-model" {
		t.Errorf("explorer model rows = %+v, want priced-model first", got.ExplorerModel.Rows)
	}
	if got.ExplorerModel.Totals.Requests != 2 || got.ExplorerModel.Totals.Cost != 0.04 {
		t.Errorf("explorer model totals = %+v, want 2 / 0.04", got.ExplorerModel.Totals)
	}
	// Top-50 cap: 60 rows in, 50 shown, totals cover shown only.
	if len(got.ExplorerTop50.Rows) != 50 || got.ExplorerTop50.TotalRows != 60 {
		t.Errorf("explorer top-50 = %d shown of %d, want 50 of 60", len(got.ExplorerTop50.Rows), got.ExplorerTop50.TotalRows)
	}
	if got.ExplorerTop50.Rows[0].Cost != 60 || got.ExplorerTop50.Rows[49].Cost != 11 {
		t.Errorf("explorer top-50 cost range = %v..%v, want 60..11", got.ExplorerTop50.Rows[0].Cost, got.ExplorerTop50.Rows[49].Cost)
	}
	// CSV contains exactly the shown rows, nothing else.
	wantCSVProvider := "dimension,provider,model,requests,tokens,cost_usd\nprovider,p,,2,3000,0.0400\nprovider,q,,1,500,0.0100\n"
	if got.CSVProvider != wantCSVProvider {
		t.Errorf("CSV provider = %q, want %q", got.CSVProvider, wantCSVProvider)
	}
	wantCSVModel := "dimension,provider,model,requests,tokens,cost_usd\nmodel,p,priced-model,1,2000,0.0400\nmodel,p,mock-model,1,1000,0.0000\n"
	if got.CSVModel != wantCSVModel {
		t.Errorf("CSV model = %q, want %q", got.CSVModel, wantCSVModel)
	}
	// Forecast: $3.00 over 30 days = $0.10/day; Feb 2026 has 28
	// days, so the estimate is $2.80 — a labelled projection.
	if got.Forecast.Value == nil || math.Abs(*got.Forecast.Value-2.8) > 1e-9 || got.Forecast.DailyRate != 0.1 || got.Forecast.DaysNextMonth != 28 {
		t.Errorf("forecast = %+v, want value 2.80 / daily 0.10 / 28 days", got.Forecast)
	}
	if got.ForecastEmpty.Value != nil {
		t.Errorf("forecast empty = %v, want null", *got.ForecastEmpty.Value)
	}
}

// The forecast carries the word "estimate" and its basis in the
// UI: the served Costs section says so in plain text, next to the
// value it projects from.
func TestDashboardM10_6ForecastLabelledEstimate(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	section := costsSection(t, rec.Body.String())
	for _, want := range []string{
		"estimate from the last 30 days, not a commitment",
		"Monthly forecast",
		`id="costForecastBasis"`,
	} {
		if !strings.Contains(section, want) {
			t.Errorf("forecast UI missing %q", want)
		}
	}
	if strings.Contains(section, "guaranteed") || strings.Contains(section, "will cost") {
		t.Error("forecast must never be worded as a commitment")
	}
}

// byKeySection returns the served HTML of the "Cost by key" card:
// from its heading to the next card's heading (Monthly forecast).
func byKeySection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<h3>Cost by key</h3>`)
	if start < 0 {
		t.Fatalf("dashboard has no Cost by key card")
	}
	rest := body[start:]
	end := strings.Index(rest, `<h3>Monthly forecast</h3>`)
	if end < 0 {
		t.Fatalf("Cost by key card is not followed by Monthly forecast")
	}
	return rest[:end]
}

// costsPureBlock returns the M10-6 pure-helper block of the served
// inline script (between the M10-6-PURE markers).
func costsPureBlock(t *testing.T, body string) string {
	t.Helper()
	script := costsInlineScript(t, body)
	start := strings.Index(script, "/* M10-6-PURE-START */")
	end := strings.Index(script, "/* M10-6-PURE-END */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("pure helper block markers (M10-6-PURE-START/END) not found in the inline script")
	}
	return script[start:end]
}

// TestDashboardM10_6ByKeyRenders proves the Cost Explorer key
// dimension in the served page: the per-key table and its ids,
// the lower-bound label on the priced-cost column, the honest
// empty state, and — the point of the card — that the new block
// shows NO combined/total cost figure anywhere (priced cost is
// a lower bound where unpriced requests exist, never a total).
// It also proves the wiring the task requires: the by-key table
// reads the same GET /api/analytics/costs call that loads avg
// cost (one literal window=month fetch in the page; both load
// buttons run loadCostsAvg, which renders the table from the
// same payload) — no second fetch, no second auth path.
func TestDashboardM10_6ByKeyRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := byKeySection(t, body)

	for _, want := range []string{
		`id="costByKeyBody"`, `id="costByKeyEmpty"`, `id="costByKeyLoadBtn"`,
		`id="costByKeyPeriod"`, `id="costByKeyNote"`,
		"Cost by key", "Load per-key costs",
		"Priced cost (lower bound where unpriced requests exist)",
		"lower bound where unpriced requests exist",
		"No key usage in this window",
		"Unpriced requests",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("Cost by key card missing %q", want)
		}
	}
	// The old claim is gone: the feed now reports cost by key.
	if strings.Contains(body, "Per-key breakdown is not available") {
		t.Error("dashboard still claims the per-key breakdown is not available (the costs feed now carries by_key)")
	}
	if strings.Contains(body, "no feed reports cost by gateway key") {
		t.Error("dashboard still claims no feed reports cost by gateway key")
	}
	// No combined/total cost figure in the new block: no
	// "total" in any casing, and no total-cost wording.
	if strings.Contains(strings.ToLower(section), "total") {
		t.Errorf("Cost by key block must not show a total-cost figure, but it contains %q", "total")
	}
	for _, banned := range []string{"combined cost", "Total cost", "total_cost", "grand total"} {
		if strings.Contains(section, banned) {
			t.Errorf("Cost by key block must not contain %q", banned)
		}
	}
	// One costs fetch: the literal window=month call appears
	// exactly once — the by-key button reuses loadCostsAvg
	// instead of adding a second fetch.
	if n := strings.Count(body, "/api/analytics/costs?window=month"); n != 1 {
		t.Errorf("/api/analytics/costs?window=month appears %d times, want exactly 1 (by-key must reuse the avg-cost fetch)", n)
	}
	if !strings.Contains(body, `if(costByKeyLoadBtn) costByKeyLoadBtn.addEventListener("click", loadCostsAvg);`) {
		t.Error("Load per-key costs button must run loadCostsAvg (the same fetch as avg cost)")
	}
	if !strings.Contains(body, "applyCostsByKey(d);") {
		t.Error("applyCostsAvg must render the by-key table from the same payload (applyCostsByKey call missing)")
	}
	if !strings.Contains(body, "function costsByKeyRows(costs)") {
		t.Error("pure helper costsByKeyRows is missing from the served script")
	}
}

// TestDashboardM10_6ByKeyFeedValues proves the Explorer's by-key
// values equal the live feed's by_key rows: two keys seeded
// through the real ledger, the feed fetched through the live
// endpoint with an admin key, and the served JS pure helper
// (costsByKeyRows) driven under node with that exact feed
// payload — its rows must match the feed's rows field for field,
// sorted by priced cost, with the lower-bound flag set exactly
// where the key has unpriced requests, and no totals figure.
func TestDashboardM10_6ByKeyFeedValues(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH — cannot drive the by-key helper")
	}
	s, _ := keyStatusSetup(t)
	key2, _, err := config.IssueKey("test-2", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key2); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	prices := analyticsPrices()
	record := func(keyID, model string, prompt, completion int, ok bool) {
		t.Helper()
		if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: "mock-upstream", Model: model,
			PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: ok, LatencyMS: 10,
		}, prices); err != nil {
			t.Fatalf("seed by-key row: %v", err)
		}
	}
	// Key 1: one priced row (1000+1000 priced-model = $0.04)
	// plus one unpriced row (500+500 mock-model, cost unknown).
	record(s.id, "priced-model", 1000, 1000, true)
	record(s.id, "mock-model", 500, 500, true)
	// Key 2: one priced row (2000 prompt + 1000 completion =
	// $0.02 + $0.03 = $0.05), nothing unpriced.
	record(key2.ID, "priced-model", 2000, 1000, true)
	// A failed unpriced row under key 1: zero tokens, zero
	// cost — it counts in requests but must NOT join the
	// key's unpriced count.
	record(s.id, "mock-model", 0, 0, false)

	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	rec := doReq(t, srv, http.MethodGet, "/api/analytics/costs?window=day", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/analytics/costs?window=day status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var costs analyticsCostsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &costs); err != nil {
		t.Fatalf("decode costs: %v", err)
	}
	if costs.ByKey == nil || len(costs.ByKey) != 2 {
		t.Fatalf("feed by_key = %+v, want 2 rows (keys 1 and 2)", costs.ByKey)
	}
	byKey := map[string]analyticsKeyRow{}
	for _, k := range costs.ByKey {
		byKey[k.GatewayKeyID] = k
	}
	k1 := byKey[s.id]
	if k1.Requests != 3 || k1.OK != 2 || k1.PromptTokens != 1500 || k1.CompletionTokens != 1500 ||
		k1.TotalTokens != 3000 || k1.PricedCostUSD != 0.04 || k1.UnpricedRequests != 1 || !k1.UnpricedRequestsKnown {
		t.Errorf("feed key 1 row = %+v, want requests 3/ok 2/tokens 1500/1500/3000/priced 0.04/unpriced 1/known", k1)
	}
	k2 := byKey[key2.ID]
	if k2.Requests != 1 || k2.OK != 1 || k2.PromptTokens != 2000 || k2.CompletionTokens != 1000 ||
		k2.TotalTokens != 3000 || k2.PricedCostUSD != 0.05 || k2.UnpricedRequests != 0 || !k2.UnpricedRequestsKnown {
		t.Errorf("feed key 2 row = %+v, want requests 1/ok 1/tokens 2000/1000/3000/priced 0.05/unpriced 0/known", k2)
	}

	// The Explorer's JS mapping of that exact feed payload.
	dashSrv := newTestServer(t, &fakeEngine{})
	dash := doReq(t, dashSrv, http.MethodGet, "/dashboard", "", "")
	if dash.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", dash.Code)
	}
	feedJSON, err := json.Marshal(costs)
	if err != nil {
		t.Fatal(err)
	}
	driver := costsPureBlock(t, dash.Body.String()) + `
var feed = ` + string(feedJSON) + `;
console.log(JSON.stringify(costsByKeyRows(feed)));
`
	dir := t.TempDir()
	driverPath := filepath.Join(dir, "bykey-driver.js")
	if err := os.WriteFile(driverPath, []byte(driver), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command("node", driverPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node by-key driver failed: %v: %s", err, raw)
	}
	if strings.Contains(string(raw), "totals") {
		t.Errorf("by-key helper output must carry no totals figure, got %s", raw)
	}
	var got struct {
		Rows []struct {
			KeyID            string  `json:"keyId"`
			Requests         int     `json:"requests"`
			OK               int     `json:"ok"`
			Tokens           int     `json:"tokens"`
			PricedCost       float64 `json:"pricedCost"`
			UnpricedRequests int     `json:"unpricedRequests"`
			LowerBound       bool    `json:"lowerBound"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode node by-key output: %v: %s", err, raw)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("Explorer by-key rows = %d, want 2 (the feed's by_key rows): %s", len(got.Rows), raw)
	}
	// Sorted by priced cost descending: key 2 ($0.05) first.
	if got.Rows[0].KeyID != key2.ID || got.Rows[0].PricedCost != 0.05 || got.Rows[0].Requests != 1 ||
		got.Rows[0].OK != 1 || got.Rows[0].Tokens != 3000 || got.Rows[0].UnpricedRequests != 0 || got.Rows[0].LowerBound {
		t.Errorf("Explorer by-key row 0 = %+v, want key 2 / $0.05 / 1 req / 1 ok / 3000 tokens / 0 unpriced / lowerBound false", got.Rows[0])
	}
	if got.Rows[1].KeyID != s.id || got.Rows[1].PricedCost != 0.04 || got.Rows[1].Requests != 3 ||
		got.Rows[1].OK != 2 || got.Rows[1].Tokens != 3000 || got.Rows[1].UnpricedRequests != 1 || !got.Rows[1].LowerBound {
		t.Errorf("Explorer by-key row 1 = %+v, want key 1 / $0.04 / 3 req / 2 ok / 3000 tokens / 1 unpriced / lowerBound true", got.Rows[1])
	}
	// The Explorer's rows reconcile to the feed: same keys, same
	// values, no invented or dropped row.
	for _, r := range got.Rows {
		want := byKey[r.KeyID]
		if r.Requests != want.Requests || r.Tokens != want.TotalTokens || r.PricedCost != want.PricedCostUSD || r.UnpricedRequests != want.UnpricedRequests {
			t.Errorf("Explorer row %s = %+v, feed row = %+v (must be equal)", r.KeyID, r, want)
		}
	}
}
