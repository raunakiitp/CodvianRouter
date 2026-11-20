// M10-0 foundations: nav slots for the new M10 sections, the shared
// admin auth header, and the approved design tokens.
//
// Proves, against the served dashboard (not just the file on disk):
//   - the nav matches the M10 end-state (plan §5.1): one slot per new
//     section (Combos, Cache, Resilience, Health, Webhooks, Analytics,
//     Batch, Responses, Audit), and the Route-guards /
//     Guardrails entries are gone (folded into Settings by M10-14);
//   - every mutating dashboard fetch sends Authorization — the nine
//     admin POSTs that used to send none now go through adminHeaders(),
//     and the previously-wired ones still carry the header;
//   - every admin-scoped mutating route in guard.go answers 401/403
//     without a key (loopback peer, so Tier 1 routes reach the auth
//     check instead of the loopback check);
//   - the approved design tokens are present and Playground is absent.
package server

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// m10_0NewNavViews are the M10 end-state nav slots (plan §5.1, in
// M10-0's nav order). The sections themselves land with their owning
// slices; the slots are inert until then (go() returns early when the
// section does not exist yet). Activity is NOT a slot: its section was
// deferred by M10-8 and no section ever landed, so the orphaned nav
// button + titles entry were removed (M10-17 gate-prep G7) rather
// than left as a dead nav entry that shows nothing on click.
var m10_0NewNavViews = []string{
	"combos", "cache", "resilience", "health", "webhooks",
	"analytics", "batch", "responses", "audit",
}

// m10_0AdminDashboardPOSTs are the dashboard POST paths that sent no
// Authorization header before M10-0 and now go through adminHeaders().
// Each must appear in the served script next to an adminHeaders() call.
var m10_0AdminDashboardPOSTs = []string{
	"/dashboard/models/refresh",
	"/dashboard/key-budgets",
	"/dashboard/feedback",
	"/dashboard/intent-tiers",
	"/dashboard/suggestions",
	"/dashboard/strategies",
	"/dashboard/cooldown",
	"/dashboard/guardrails",
	"/api/mcp/status",
}

// m10_0PlanAdminPOSTs is the plan §5 M10-0 enumeration of the
// admin-scoped dashboard POST set (plus the /admin/keys/* set named in
// §3.1): the server-side sweep must cover every one of these.
var m10_0PlanAdminPOSTs = []string{
	"POST /dashboard/models/refresh",
	"POST /dashboard/feedback",
	"POST /dashboard/intent-tiers",
	"POST /dashboard/suggestions",
	"POST /dashboard/guardrails",
	"POST /dashboard/strategies",
	"POST /dashboard/cooldown",
	"POST /dashboard/key-budgets",
	"POST /dashboard/quota-pools",
	"POST /dashboard/quota-schedules",
	"POST /dashboard/internal-budgets",
	"POST /dashboard/teams",
	"POST /dashboard/teams/{id}/members",
	"POST /api/providers",
	"DELETE /api/providers/{id}",
	"POST /api/providers/{id}/keys",
	"DELETE /api/providers/{id}/keys/{keyId}",
	"POST /api/providers/{id}/keys/{keyId}/role",
	"POST /admin/keys/{id}",
	"DELETE /admin/keys/{id}",
	"POST /admin/keys/{id}/rotate",
	"POST /admin/keys/revoke-all",
}

func m10_0DashboardBody(t *testing.T) string {
	t.Helper()
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func m10_0InlineScript(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, "<script>")
	end := strings.Index(body, "</script>")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("served dashboard has no inline <script> block")
	}
	if strings.Count(body, "<script>") != 1 || strings.Count(body, "</script>") != 1 {
		t.Fatalf("served dashboard has %d <script> / %d </script> blocks, want 1 each",
			strings.Count(body, "<script>"), strings.Count(body, "</script>"))
	}
	return body[start+len("<script>") : end]
}

func TestDashboardM10_0NavSlots(t *testing.T) {
	body := m10_0DashboardBody(t)
	for _, view := range m10_0NewNavViews {
		if !strings.Contains(body, `data-view="`+view+`"`) {
			t.Errorf("M10-0 nav slot missing: data-view=%q", view)
		}
	}
	// Guard / Guardrails leave the nav in M10-0 (their sections are
	// folded into Settings by M10-14; the feeds stay live).
	for _, gone := range []string{`data-view="guard"`, `data-view="guardrails"`} {
		if strings.Contains(body, gone) {
			t.Errorf("M10-0 nav must not contain %s", gone)
		}
	}
	// Nav inventory pin: 20 pre-M10 entries - 2 removed + 9 new = 27
	// (Activity is not a slot — its section never landed; see above).
	if got := strings.Count(body, "data-view="); got != 27 {
		t.Errorf("nav data-view count = %d, want 27 (20 - 2 removed + 9 new)", got)
	}
	// No nav button may point at a missing section: every data-view
	// value must have a matching id="view-<name>" section, so no dead
	// nav entry can show nothing on click (M10-17 gate-prep G7: the
	// orphaned Activity button was removed for exactly this reason).
	navRe := regexp.MustCompile(`data-view="([^"]+)"`)
	for _, m := range navRe.FindAllStringSubmatch(body, -1) {
		if !strings.Contains(body, `id="view-`+m[1]+`"`) {
			t.Errorf("nav data-view=%q has no matching section id=\"view-%s\"", m[1], m[1])
		}
	}
	// Activity must be fully gone from the nav (button + titles entry).
	if strings.Contains(body, `data-view="activity"`) || strings.Contains(body, `activity:"Activity"`) {
		t.Error("orphaned Activity nav entry must not exist (no view-activity section)")
	}
	// The shared operator-key field every admin POST reads from.
	if !strings.Contains(body, `id="operatorKeyInput"`) {
		t.Error("M10-0 operator-key field missing: id=\"operatorKeyInput\"")
	}
	// Approved design tokens (plan §1): pure white light / pure black
	// dark, dark-lime palette, the three families, 26px card radius.
	for _, want := range []string{
		"--bg:#FFFFFF", "--bg:#000000",
		"Plus Jakarta Sans", "JetBrains Mono", "Source Serif 4",
		"--radius:26px",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("M10-0 design token missing: %q", want)
		}
	}
	// Playground stays removed, entirely.
	if strings.Contains(body, "Playground") {
		t.Error("dashboard must not contain Playground")
	}
}

func TestDashboardM10_0AdminHeadersWired(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)

	// The shared helper builds the header; every wired POST goes
	// through it, so the header text appears exactly once in the
	// helper and the call sites reference the helper by name.
	if !strings.Contains(script, `"Authorization": "Bearer " + key`) {
		t.Error("adminHeaders() must build an Authorization: Bearer header")
	}
	if strings.Count(script, "function adminHeaders()") != 1 {
		t.Error("adminHeaders() must be defined exactly once in the dashboard script")
	}
	for _, path := range m10_0AdminDashboardPOSTs {
		// A path can have both a GET fetch (the feed loader) and the
		// POST fetch: find the POST one, not the first occurrence.
		needle := `fetch("` + path + `"`
		foundPOST := false
		for off := 0; ; {
			rel := strings.Index(script[off:], needle)
			if rel < 0 {
				break
			}
			idx := off + rel
			off = idx + len(needle)
			tail := script[idx:]
			if len(tail) > 400 {
				tail = tail[:400]
			}
			if !strings.Contains(tail, `"POST"`) {
				continue
			}
			foundPOST = true
			lo := idx - 600
			if lo < 0 {
				lo = 0
			}
			hi := idx + 600
			if hi > len(script) {
				hi = len(script)
			}
			if !strings.Contains(script[lo:hi], "adminHeaders()") {
				t.Errorf("POST %s is not wired through adminHeaders()", path)
			}
			break
		}
		if !foundPOST {
			t.Errorf("dashboard script has no POST fetch for %s", path)
		}
	}

	// Sweep: EVERY mutating fetch in the script must carry auth —
	// either the literal header or a call to adminHeaders(). A
	// mutating fetch is one whose options name POST/DELETE/PATCH/PUT
	// (or pass the method through a variable, as the two generic
	// helpers keyReq/tunnelCall do). GET fetches carry no method and
	// are skipped.
	mutatingMethod := regexp.MustCompile(`method:\s*("(?:POST|DELETE|PATCH|PUT)"|method\b)`)
	for i := 0; i < len(script); {
		rel := strings.Index(script[i:], "fetch(")
		if rel < 0 {
			break
		}
		idx := i + rel
		tail := script[idx:]
		if len(tail) > 700 {
			tail = tail[:700]
		}
		// The dashboard-auth endpoints authenticate by the
		// HttpOnly session cookie (sent automatically by a
		// same-origin fetch) — login by the password in the
		// body, change by the session plus the current password
		// in the body, logout by the session alone. There is no
		// Bearer token for the script to attach: the session
		// cookie is HttpOnly by design, so no Authorization
		// header can exist here. /dashboard/auth/setup is NOT
		// exempt — it must go through adminHeaders(), as it
		// does today.
		if strings.HasPrefix(tail, `fetch("/dashboard/auth/login"`) || strings.HasPrefix(tail, `fetch("/dashboard/auth/change"`) || strings.HasPrefix(tail, `fetch("/dashboard/auth/logout"`) {
			i = idx + len("fetch(")
			continue
		}
		if mutatingMethod.MatchString(tail) {
			lo := idx - 700
			if lo < 0 {
				lo = 0
			}
			hi := idx + 700
			if hi > len(script) {
				hi = len(script)
			}
			window := script[lo:hi]
			if !strings.Contains(window, "Authorization") && !strings.Contains(window, "adminHeaders()") {
				snip := tail
				if len(snip) > 120 {
					snip = snip[:120]
				}
				t.Errorf("mutating fetch without Authorization near %q", snip)
			}
		}
		i = idx + len("fetch(")
	}
}

func TestDashboardM10_0AdminPostsRequireAuth(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	param := regexp.MustCompile(`\{[^}]+\}`)
	swept := map[string]bool{}
	count := 0
	for _, r := range RouteTable() {
		if r.Bootstrap {
			continue
		}
		isAdmin := false
		for _, s := range r.Scopes {
			if s == "admin" {
				isAdmin = true
				break
			}
		}
		if !isAdmin || r.Method == http.MethodGet {
			continue
		}
		// Substitute path params so the request reaches the guard
		// instead of the mux's 404.
		path := param.ReplaceAllString(r.Path, "x")
		rec := doReqPeer(t, srv, r.Method, path, "{}", "", loopbackPeer)
		swept[r.Method+" "+r.Path] = true
		count++
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Errorf("%s %s without auth: status = %d, want 401 or 403",
				r.Method, r.Path, rec.Code)
		}
	}
	if count == 0 {
		t.Fatal("no admin-scoped mutating routes found in RouteTable()")
	}
	t.Logf("M10-0 admin auth sweep: %d mutating admin routes, all 401/403 without a key", count)
	for _, want := range m10_0PlanAdminPOSTs {
		if !swept[want] {
			t.Errorf("plan §5 M10-0 enumeration %q is not in the admin route table", want)
		}
	}
}
