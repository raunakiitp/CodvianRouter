// M10-8 dashboard Logs polish (Logs region only): the Logs table stays
// as-is and gains three filtered views of the SAME feed — Proxy
// (attempts with provider/model), Console (server-side event lines
// from GET /api/logs), Timeline (chronological attempts, with the
// latest-route-trace card) — each labelled as a view of the request
// log, never as a separate data source. The Activity timeline section
// is EXPLICITLY DEFERRED (it needs an M10-0-assigned nav slot that has
// not landed): this slice adds no nav entry, no Activity section, and
// no "coming soon" placeholder. Proves the section is served in the
// live dashboard HTML, not just present in the file.
package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// logsSection returns the served HTML of <section id="view-logs">.
func logsSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-logs"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-logs section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-logs section is not closed")
	}
	return rest[:end]
}

// jsFunction returns the source of the named top-level dashboard JS
// function, from its "function <name>(){" line to the next top-level
// "function " line (or the end of the served HTML).
func jsFunction(t *testing.T, body, name string) string {
	t.Helper()
	start := strings.Index(body, "function "+name+"(){")
	if start < 0 {
		t.Fatalf("dashboard JS has no function %s", name)
	}
	rest := body[start:]
	if end := strings.Index(rest, "\n  function "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestDashboardM10_8LogsViewsRender(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	section := logsSection(t, body)

	// The existing Logs table stays as-is.
	for _, want := range []string{
		`id="logBody"`,
		`id="logEmpty"`,
		`id="logSeg"`,
		`id="exportLogsBtn"`,
		// The view switcher + the three filtered-view panels.
		`id="logViewSeg"`,
		`data-logview="all"`,
		`data-logview="proxy"`,
		`data-logview="console"`,
		`data-logview="timeline"`,
		`id="logViewAll"`,
		`id="logViewProxy"`,
		`id="logViewConsole"`,
		`id="logViewTimeline"`,
		`id="logProxyBody"`,
		`id="logProxyEmpty"`,
		`id="logConsoleLines"`,
		`id="logConsoleEmpty"`,
		`id="logTimelineList"`,
		`id="logTimelineEmpty"`,
		// Console needs an admin-scoped key, typed fresh, never stored.
		`id="consoleKey"`,
		`id="consoleLoadBtn"`,
		// The latest-route-trace card lives in the Timeline view.
		`id="logTrace"`,
		// Each view is labelled as a view of the one request log.
		"Proxy — a view of the request log",
		"Console — a view of the request log",
		"Timeline — a view of the request log",
		"filtered views of the same request log",
		"not separate data sources",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Logs section missing %q", want)
		}
	}

	// The views read only the existing feeds — no new backend. Each
	// feed path must appear inside the Logs JS function that actually
	// fetches it: a whole-body search would pass on labels and
	// comments elsewhere in the file that merely mention the paths.
	// Match the fetch(" call itself, not the bare path: the
	// loadConsoleLogs function also mentions /api/logs in a comment
	// and in an error string, so a bare-path match would still pass
	// with the fetch line deleted.
	for _, tc := range []struct{ fn, feed string }{
		{"loadLogs", `fetch("/dashboard/logs`},
		{"loadConsoleLogs", `fetch("/api/logs`},
		{"loadTrace", `fetch("/dashboard/trace`},
	} {
		if !strings.Contains(jsFunction(t, body, tc.fn), tc.feed) {
			t.Errorf("dashboard Logs JS %s must fetch %q", tc.fn, tc.feed)
		}
	}

	// Activity is DEFERRED pending M10-0 nav slots: this slice adds no
	// Activity section and no placeholder inside the Logs region.
	// (The nav itself is M10-0-owned and may already carry an
	// Activity entry in the working tree; what must not exist is the
	// section it would point at.)
	for _, banned := range []string{
		`id="view-activity"`,
		`data-logview="activity"`,
		"Activity timeline",
		"coming soon",
		"Coming soon",
	} {
		if strings.Contains(section, banned) {
			t.Errorf("Logs section must not contain deferred Activity marker %q", banned)
		}
	}
	if strings.Contains(body, `id="view-activity"`) {
		t.Errorf("dashboard must not contain an Activity section (deferred pending M10-0)")
	}
}

// TestDashboardM10_8FilteredViewsAreSubsetsOfOneFeed proves the
// filtered views are subsets of the same GET /dashboard/logs response:
// the ok and failed filters partition the all feed (same rows, no
// orphans, no invented rows), and the Console + Timeline feeds answer.
func TestDashboardM10_8FilteredViewsAreSubsetsOfOneFeed(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	seedLedger(t, led)

	cfg := config.DefaultConfig(t.TempDir())
	adminKey, adminRaw, err := config.IssueKey("test-admin", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(adminKey); err != nil {
		t.Fatalf("AddGatewayKey: %v", err)
	}

	srv := newTestServer(t, &fakeEngine{}, WithLedger(led), WithConfig(cfg))

	var lgAll, lgOK, lgFailed logsResponse
	getJSON(t, srv, "/dashboard/logs?limit=100&filter=all", &lgAll)
	getJSON(t, srv, "/dashboard/logs?limit=100&filter=ok", &lgOK)
	getJSON(t, srv, "/dashboard/logs?limit=100&filter=failed", &lgFailed)
	if len(lgAll.Events) != 3 {
		t.Fatalf("logs all = %d rows, want 3", len(lgAll.Events))
	}
	if len(lgOK.Events) != 2 {
		t.Errorf("logs ok = %d rows, want 2", len(lgOK.Events))
	}
	if len(lgFailed.Events) != 1 {
		t.Errorf("logs failed = %d rows, want 1", len(lgFailed.Events))
	}
	// Subset property: every filtered row is one of the all-feed rows,
	// by ledger id; ok and failed partition the all feed.
	allIDs := map[int64]bool{}
	for _, ev := range lgAll.Events {
		allIDs[ev.ID] = true
	}
	seen := map[int64]bool{}
	for _, ev := range append(append([]ledger.UsageEvent{}, lgOK.Events...), lgFailed.Events...) {
		if !allIDs[ev.ID] {
			t.Errorf("filtered view row id %d is not in the all feed — views must be subsets of one feed", ev.ID)
		}
		if seen[ev.ID] {
			t.Errorf("row id %d appears in both the ok and failed views", ev.ID)
		}
		seen[ev.ID] = true
	}
	if len(seen) != len(lgAll.Events) {
		t.Errorf("ok + failed views cover %d rows, want all %d", len(seen), len(lgAll.Events))
	}

	// Timeline's feed: the latest-route-trace endpoint answers.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /dashboard/trace status = %d, want 200", rec.Code)
	}

	// Console's feed: GET /api/logs answers with an admin key, and
	// refuses without one — the dashboard's typed-key input exists
	// because this feed is admin-scoped, not because it is public.
	rec = doReq(t, srv, http.MethodGet, "/api/logs?limit=100", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/logs with admin key status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var apiLogs apiLogsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &apiLogs); err != nil {
		t.Fatalf("GET /api/logs decode: %v (body %q)", err, rec.Body.String())
	}
	if apiLogs.Redaction != "always-on" {
		t.Errorf("api/logs redaction = %q, want always-on", apiLogs.Redaction)
	}
	rec = doReq(t, srv, http.MethodGet, "/api/logs?limit=100", "", "")
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/logs without a key status = %d, want 401 or 403", rec.Code)
	}
}
