// M10-1 dashboard Overview: the local-operator landing page — a 4-step
// quick-start (create key, connect provider, point client, monitor) with
// a show/hide toggle defaulting to shown, exactly 4 KPI cards, a
// range-segmented spend line (one range at a time, never side-by-side
// 1D/7D/30D and never an avg cost/req — no feed supplies one), a provider
// topology widget (health chips + 24h uptime bars + p50), live system
// metrics from /dashboard/memstats + /dashboard/benchmark, and a live
// provider-quota widget from /dashboard/quota-pools. Host CPU/memory has
// no live feed, so that line stays honestly "No data yet" — it is never
// invented. Proves the section is served in the live dashboard HTML, not
// just present in the file.
package server

import (
	"net/http"
	"strings"
	"testing"
)

// overviewSection returns the served HTML of <section id="view-overview">.
func overviewSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-overview"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-overview section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-overview section is not closed")
	}
	return rest[:end]
}

func TestDashboardM10_1OverviewRenders(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := overviewSection(t, body)

	// KPI row is exactly 4 cards.
	if n := strings.Count(section, `class="card kpi"`); n != 4 {
		t.Errorf("Overview KPI row has %d cards, want exactly 4", n)
	}

	for _, want := range []string{
		// Quick start: card, toggle (default shown), body, docs link.
		`id="quickStartCard"`,
		`id="qsToggle"`,
		`aria-expanded="true"`,
		`id="quickStartBody"`,
		`id="fullDocsLink"`,
		`href="https://github.com/raunakiitp/CodvianRouter"`,
		// The four steps, in order.
		"Create a router key",
		"Connect a provider",
		"Point your client at the router",
		"Monitor",
		// Step links point at the real sections.
		`data-goto="keys"`,
		`data-goto="providers"`,
		`data-goto="models"`,
		`data-goto="logs"`,
		// Base URL / key fields kept from the old quick-start card.
		`id="baseUrlVal"`,
		`id="routerKeyVal"`,
		`id="copyBaseUrlBtn"`,
		`id="revealKeyBtn"`,
		`id="codeBaseUrl"`,
		// Range-segmented spend (one range, from the overview feed).
		`id="overviewSpend"`,
		// Provider topology widget (health chips + 24h uptime + p50).
		`id="providerTopologyCard"`,
		"Provider topology",
		`id="healthList"`,
		// System metrics: live process feed + recorded benchmark.
		`id="systemMetricsCard"`,
		`id="sysCpuVal"`,
		`id="sysMemVal"`,
		`id="sysHostVal"`,
		`id="sysProcVal"`,
		`id="sysBenchVal"`,
		// Host line is honestly empty: no live host feed exists.
		"No data yet",
		// Provider quota widget (live feed only).
		`id="providerQuotaCard"`,
		`id="quotaPoolsBody"`,
		`id="quotaPoolsEmpty"`,
		"No quota pools yet",
		// Surviving detail cards.
		"Traffic",
		"Token mix",
		"Prompt cache",
		"Live routing",
		"Recent requests",
		// The Overview JS reads only real, registered feeds.
		"/dashboard/overview",
		"/dashboard/memstats",
		"/dashboard/benchmark",
		"/dashboard/quota-pools",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Overview missing %q", want)
		}
	}

	// Steps must appear in quick-start order.
	steps := []string{"Create a router key", "Connect a provider", "Point your client at the router", "Monitor"}
	prev := -1
	for _, step := range steps {
		at := strings.Index(section, step)
		if at < 0 {
			t.Errorf("quick-start step missing: %q", step)
			continue
		}
		if at < prev {
			t.Errorf("quick-start steps out of order at %q", step)
		}
		prev = at
	}

	// No side-by-side spend and no avg cost/req: neither is supplied by
	// any feed the Overview reads, so neither may appear here.
	for _, banned := range []string{"avg cost", "Avg cost", "cost/req", "cost per request"} {
		if strings.Contains(section, banned) {
			t.Errorf("Overview must not show %q (no feed supplies it)", banned)
		}
	}
	if strings.Contains(section, "1D/7D/30D") || strings.Contains(section, "1D · 7D · 30D") {
		t.Errorf("Overview spend must be range-segmented, not side-by-side 1D/7D/30D")
	}

	// No fake-data markers in the Overview section.
	for _, banned := range []string{"Lorem", "lorem", "Sample data", "sample data", "TODO: fake", "placeholder data"} {
		if strings.Contains(section, banned) {
			t.Errorf("Overview contains fake-data marker %q", banned)
		}
	}

	// No Playground anywhere in the dashboard.
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
}

// The feeds the Overview reads must answer on a live server: the
// overview feed (range-segmented), the memstats feed, the benchmark
// feed (recorded=false with no run file), and the quota-pools feed
// (empty list with no pools configured).
func TestDashboardM10_1OverviewFeedsAnswer(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, path := range []string{
		"/dashboard/overview?range=24h",
		"/dashboard/overview?range=7d",
		"/dashboard/overview?range=30d",
		"/dashboard/memstats",
		"/dashboard/benchmark",
		"/dashboard/quota-pools",
	} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, rec.Code)
		}
	}
}
