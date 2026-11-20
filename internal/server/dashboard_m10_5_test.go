// M10-5 dashboard Combos view: full CRUD over the live
// GET/POST/PATCH/DELETE /v1/combos feed (combos_api.go, M8.14;
// guard.go rows 340-347 — GETs scope "models", mutations scope
// "admin", TierManagement throughout).
//
// Proves, against the served dashboard (not just the file on disk):
//   - the Combos section renders (heading, table/form ids, the
//     honest zero-combos empty state, the group-mirror note) and
//     go() reloads it on every open;
//   - the CRUD round-trip through the live test server (create ->
//     list shows it -> PATCH edit -> rename is create-then-delete
//     -> DELETE removes it), asserting the feed, not the DOM;
//   - every mutating fetch in combosViewSave() — create POST,
//     PATCH, rename POST, rename DELETE — is wired through the
//     shared adminHeaders() helper, asserted as REAL calls (the
//     exact `var headers = adminHeaders();` statement, never a
//     bare-name substring the comments would match), with one
//     fresh call + guard per branch and the Save-button disable
//     after each branch's guard (hoist review); the rename
//     DELETE reuses the rename POST's headers, because
//     adminHeaders() clears the typed key on every call, so a
//     second call would abort it;
//   - the group mirror is real: creating a combo adds its routing
//     group to the live GET /dashboard/groups feed, deleting it
//     removes the group again;
//   - the extracted inline script passes node --check.
//
// NOT proven here (honest limit): no live-browser click-through
// of the Combos form — that needs a browser agent driving the
// served page. The feed-level round-trip below is the same
// sequence the form's fetches perform, in the same order.
package server

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// m10_5Section returns the served HTML of <section id="view-combos">.
func m10_5Section(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="view-combos"`)
	if start < 0 {
		t.Fatalf("dashboard has no view-combos section")
	}
	rest := body[start:]
	end := strings.Index(rest, "</section>")
	if end < 0 {
		t.Fatalf("view-combos section is not closed")
	}
	return rest[:end]
}

// m10_5JSRegion returns this slice's own JS: from its header
// comment to loadModels(), so the fetch/method assertions below
// check only this slice's code, never a sibling's.
func m10_5JSRegion(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "Combos view (M10-5)")
	if start < 0 {
		t.Fatalf("dashboard script has no Combos view (M10-5) block")
	}
	rest := script[start:]
	end := strings.Index(rest, "function loadModels(){")
	if end < 0 {
		t.Fatalf("Combos view (M10-5) JS block has no end marker (function loadModels(){)")
	}
	return rest[:end]
}

// m10_5FuncRegion returns one named function from the script
// (from "function <name>(){" to the next "function " at column
// start, or to end of script).
func m10_5FuncRegion(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, "function "+name+"(){")
	if start < 0 {
		t.Fatalf("dashboard script has no function %s", name)
	}
	rest := script[start:]
	end := strings.Index(rest[1:], "\n  function ")
	if end < 0 {
		return rest
	}
	return rest[:end+1]
}

func TestDashboardM10_5CombosRenders(t *testing.T) {
	body := m10_0DashboardBody(t)
	section := m10_5Section(t, body)
	script := m10_0InlineScript(t, body)
	js := m10_5JSRegion(t, script)

	// Section: heading, live-feed label, table + empty/key/error
	// states, detail card, create/edit form, group-mirror note.
	for _, want := range []string{
		"<h1>Combos</h1>",
		"Named routing chains",
		`id="combosViewRefreshBtn"`, `id="combosViewNewBtn"`, "+ New combo",
		"Live — from GET /v1/combos",
		`id="combosViewCount"`, "Loading live data…",
		`id="combosViewBody"`,
		`id="combosViewEmpty"`, "No combos yet — create one",
		`id="combosViewKey"`, `id="combosViewLoadBtn"`, "Load combos",
		`id="combosViewError"`,
		// The honest group-mirror note: group:<name>/<model>.
		"group:&lt;name&gt;/&lt;model&gt;",
		"mirrors into the routing groups",
		// Detail card: view / edit / rename / delete.
		`id="combosViewDetailCard"`, `id="combosViewDetailTitle"`,
		`id="combosViewDetailSteps"`, `id="combosViewEditBtn"`,
		`id="combosViewRenameBtn"`, `id="combosViewDeleteBtn"`,
		// Form card: name + strategy + chain, save/cancel.
		`id="combosViewFormCard"`, `id="combosViewFormTitle"`,
		`id="combosViewNameInput"`, `id="combosViewStrategySel"`,
		`id="combosViewChainInput"`, `id="combosViewSaveBtn"`,
		`id="combosViewCancelBtn"`, "Save combo",
		"Live — POST /v1/combos",
		"one per line, in order: provider[/model][@effort]",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("dashboard Combos section missing %q", want)
		}
	}

	// The nav slot and the go() title exist, and opening the view
	// reloads the live list every time (never a stale snapshot).
	if !strings.Contains(body, `data-view="combos"`) {
		t.Error("dashboard nav has no data-view=\"combos\" slot")
	}
	if !strings.Contains(body, `combos:"Combos"`) {
		t.Error("dashboard go() titles map has no combos entry")
	}
	if !strings.Contains(body, `if(view === "combos"){ combosViewLoad(combosViewKeyed); }`) {
		t.Error("dashboard go() does not reload the Combos view on open")
	}

	// The view's JS reads the live feed and exposes the CRUD
	// functions the buttons call — no hard-coded combo rows.
	for _, want := range []string{
		`fetch("/v1/combos"`,
		`fetch("/v1/combos/"`,
		`method: "POST"`, `method: "PATCH"`, `method: "DELETE"`,
		"function combosViewLoad(",
		"function combosViewRender(",
		"function combosViewSave(",
		"function combosViewDelete(",
		"function combosViewOpenForm(",
		"function combosViewShowDetail(",
		// Rename is create-then-delete: the new name is created
		// first, the old record removed only after the create
		// answers 201 — a failed rename never loses the original.
		// (The comment wraps over two lines, so the halves are
		// pinned separately, never as one joined string.)
		"create the new name first, delete the old one only",
		"after the create answers 201",
		"Saving with a different name renames it",
		// The Models tab's Combos tab reads the same live feed.
		"function loadCombos(){",
		"function renderCombos(){",
		`$("#modelsTabCombos")`,
	} {
		if !strings.Contains(script, want) && !strings.Contains(js, want) {
			t.Errorf("dashboard Combos JS missing %q", want)
		}
	}
	// The rename hint lives in this slice's JS, not a sibling's.
	if !strings.Contains(js, "Saving with a different name renames it") {
		t.Error("Combos view JS missing the rename hint (create first, then delete the old one)")
	}

	// B3 is absent by design — no buttons, no disabled controls —
	// until the backend verbs are registered in server.go.
	for _, banned := range []string{
		"combosViewDuplicate", "combosViewImport", "combosViewExport",
		"combosViewTestRun", "combosViewEnable", "combosViewDisable",
		"Duplicate", "Test run",
	} {
		if strings.Contains(section, banned) || strings.Contains(js, banned) {
			t.Errorf("Combos section must have no B3 control, but contains %q (absent by design)", banned)
		}
	}

	// No fake-data markers, no Playground.
	for _, banned := range []string{"Lorem", "Sample data", "sample data", "Playground"} {
		if strings.Contains(section, banned) {
			t.Errorf("Combos section contains %q", banned)
		}
	}
}

// m10_5CombosList is the GET /v1/combos payload.
type m10_5CombosList struct {
	Combos    []Combo `json:"combos"`
	Persisted bool    `json:"persisted"`
}

// m10_5List drives GET /v1/combos against the live test server.
func m10_5List(t *testing.T, srv *Server, key string) (int, m10_5CombosList) {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/v1/combos", "", key)
	var out m10_5CombosList
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode combos list: %v: %s", err, rec.Body.String())
		}
	}
	return rec.Code, out
}

// m10_5Groups drives the auth-exempt GET /dashboard/groups feed
// and returns the group names + their member provider IDs.
func m10_5Groups(t *testing.T, srv *Server) map[string][]string {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/groups", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/groups status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Groups []struct {
			Name    string `json:"name"`
			Members []struct {
				ProviderID string `json:"provider_id"`
			} `json:"members"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode groups feed: %v: %s", err, rec.Body.String())
	}
	groups := map[string][]string{}
	for _, g := range out.Groups {
		members := []string{}
		for _, m := range g.Members {
			members = append(members, m.ProviderID)
		}
		groups[g.Name] = members
	}
	return groups
}

// m10_5Setup returns a server whose config carries the mock
// providers the chain validator accepts (comboTestConfig) plus an
// admin-scoped key (chat+models+admin: GETs need "models",
// mutations need "admin").
func m10_5Setup(t *testing.T) (*keyBudgetSetup, *Server, string) {
	t.Helper()
	s := newKeyBudgetSetup(t)
	comboCfg := comboTestConfig()
	s.cfg.Providers = append(s.cfg.Providers, comboCfg.Providers...)
	if s.cfg.Routing == nil {
		s.cfg.Routing = comboCfg.Routing
	}
	adminRaw := adminKeyFor(t, s)
	return s, s.server(t), adminRaw
}

// The feed answers, honestly, before anything is created: the list
// is an empty array (not null, not a fabricated row), and every
// combos verb refuses without a key.
func TestDashboardM10_5CombosFeedsAnswer(t *testing.T) {
	_, srv, adminRaw := m10_5Setup(t)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/combos"},
		{http.MethodPost, "/v1/combos"},
		{http.MethodPatch, "/v1/combos/nope"},
		{http.MethodDelete, "/v1/combos/nope"},
	} {
		rec := doReq(t, srv, tc.method, tc.path, `{"name":"x","chain":["mock-upstream/mock-model"],"strategy":"priority"}`, "")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without a key status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}

	if code, list := m10_5List(t, srv, adminRaw); code != http.StatusOK {
		t.Fatalf("GET /v1/combos status = %d, want 200", code)
	} else if len(list.Combos) != 0 {
		t.Fatalf("list before create = %+v, want empty", list)
	}
	// The wire body is an honest empty array, so the dashboard's
	// "No combos yet — create one" state is fed, never faked.
	rec := doReq(t, srv, http.MethodGet, "/v1/combos", "", adminRaw)
	if !strings.Contains(rec.Body.String(), `"combos":[]`) {
		t.Errorf("GET /v1/combos body = %s, want an empty \"combos\":[] array", rec.Body.String())
	}
}

// Create -> list shows it -> GET by id -> PATCH edit -> PATCH
// rename is a 400 -> rename via create-then-delete (the UI's path)
// -> DELETE removes it -> list empty again, all through the mux,
// with the admin key. The chain bodies use the UI's own shape:
// "provider[/model][@effort]" strings.
func TestDashboardM10_5CombosCRUDRoundTrip(t *testing.T) {
	s, srv, adminRaw := m10_5Setup(t)

	// Rejected creates change nothing: unknown strategy, unknown
	// provider. (The dashboard form relies on this server-side
	// validation — it never invents its own error text.)
	for _, tc := range []struct{ name, body string }{
		{"bad strategy", `{"name":"m105-bad","chain":["mock-upstream/mock-model"],"strategy":"bogus-strategy"}`},
		{"unknown provider", `{"name":"m105-bad","chain":["no-such-provider/mock-model"],"strategy":"priority"}`},
	} {
		rec := doReq(t, srv, http.MethodPost, "/v1/combos", tc.body, adminRaw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if _, list := m10_5List(t, srv, adminRaw); len(list.Combos) != 0 {
		t.Fatalf("list after rejected creates = %+v, want empty", list)
	}

	// Create (201): one step, priority — exactly what the form posts.
	rec := doReq(t, srv, http.MethodPost, "/v1/combos",
		`{"name":"m105-cheap-first","chain":["mock-upstream/mock-model"],"strategy":"priority"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created Combo
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created combo: %v: %s", err, rec.Body.String())
	}
	if created.ID != "m105-cheap-first" || created.Name != "m105-cheap-first" {
		t.Errorf("created id/name = %q/%q, want m105-cheap-first", created.ID, created.Name)
	}
	if len(created.Chain) != 1 || created.Chain[0].Provider != "mock-upstream" || created.Chain[0].Model != "mock-model" {
		t.Errorf("created chain = %+v, want one step mock-upstream/mock-model", created.Chain)
	}
	if created.Strategy != "priority" || !created.Enabled {
		t.Errorf("created = %+v, want strategy priority / enabled", created)
	}
	if !created.Usable || !created.Chain[0].Usable || !created.Chain[0].HasKey {
		t.Errorf("created usable = %v / step usable %v / has_key %v, want all true (mock-upstream holds a key)",
			created.Usable, created.Chain[0].Usable, created.Chain[0].HasKey)
	}

	// List shows it; GET by id returns it.
	if _, list := m10_5List(t, srv, adminRaw); len(list.Combos) != 1 || list.Combos[0].Name != "m105-cheap-first" {
		t.Fatalf("list after create = %+v, want exactly m105-cheap-first", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/combos/m105-cheap-first", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/combos/m105-cheap-first status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The group mirror is real: the routing-groups feed gains the
	// combo the moment it is created (member = provider, no model
	// pin — the mirror stores provider[@effort] only), and the
	// chain-strategy mirror carries its strategy.
	groups := m10_5Groups(t, srv)
	if members, ok := groups["m105-cheap-first"]; !ok {
		t.Errorf("groups feed has no m105-cheap-first group after create (groups = %v)", groups)
	} else if len(members) != 1 || members[0] != "mock-upstream" {
		t.Errorf("mirrored group members = %v, want [mock-upstream]", members)
	}
	s.cfg.RLockRouting()
	mirroredStrategy := ""
	if s.cfg.Routing != nil {
		mirroredStrategy = s.cfg.Routing.ChainStrategies["m105-cheap-first"]
	}
	s.cfg.RUnlockRouting()
	if mirroredStrategy != "priority" {
		t.Errorf("mirrored chain strategy = %q, want priority", mirroredStrategy)
	}

	// PATCH edit (200): a second step + a new strategy.
	rec = doReq(t, srv, http.MethodPatch, "/v1/combos/m105-cheap-first",
		`{"chain":["mock-upstream/mock-model","mock-local/mock-model"],"strategy":"cheapest"}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var edited Combo
	if err := json.Unmarshal(rec.Body.Bytes(), &edited); err != nil {
		t.Fatalf("decode edited combo: %v: %s", err, rec.Body.String())
	}
	if len(edited.Chain) != 2 || edited.Chain[1].Provider != "mock-local" || edited.Chain[1].Model != "mock-model" {
		t.Errorf("edited chain = %+v, want mock-upstream + mock-local steps", edited.Chain)
	}
	if edited.Strategy != "cheapest" {
		t.Errorf("edited strategy = %q, want cheapest", edited.Strategy)
	}

	// PATCH rename is a 400 naming the rule — this is why the UI
	// renames by create-then-delete instead. Nothing changes.
	rec = doReq(t, srv, http.MethodPatch, "/v1/combos/m105-cheap-first",
		`{"name":"m105-renamed"}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH rename status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cannot rename") {
		t.Errorf("PATCH rename body = %s, want it to name the cannot-rename rule", rec.Body.String())
	}
	if _, list := m10_5List(t, srv, adminRaw); len(list.Combos) != 1 || list.Combos[0].Strategy != "cheapest" || len(list.Combos[0].Chain) != 2 {
		t.Errorf("list after rejected rename = %+v, want the edited combo unchanged", list)
	}

	// Rename the UI's way: create the new name first (201), then
	// delete the old one (200, deleted:true). The old id is 404
	// from then on, and the group mirror follows the rename.
	rec = doReq(t, srv, http.MethodPost, "/v1/combos",
		`{"name":"m105-cheap-first-v2","chain":["mock-upstream/mock-model","mock-local/mock-model"],"strategy":"cheapest"}`, adminRaw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("rename-create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodDelete, "/v1/combos/m105-cheap-first", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename-delete status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var deleted struct {
		Deleted bool   `json:"deleted"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &deleted); err != nil {
		t.Fatalf("decode delete: %v: %s", err, rec.Body.String())
	}
	if !deleted.Deleted || deleted.ID != "m105-cheap-first" {
		t.Errorf("delete = %+v, want deleted:true id m105-cheap-first", deleted)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/combos/m105-cheap-first", "", adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET old id after rename status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if _, list := m10_5List(t, srv, adminRaw); len(list.Combos) != 1 || list.Combos[0].Name != "m105-cheap-first-v2" {
		t.Fatalf("list after rename = %+v, want exactly m105-cheap-first-v2", list)
	}
	groups = m10_5Groups(t, srv)
	if _, ok := groups["m105-cheap-first"]; ok {
		t.Errorf("groups feed still has m105-cheap-first after the rename-delete (groups = %v)", groups)
	}
	if members, ok := groups["m105-cheap-first-v2"]; !ok || len(members) != 2 {
		t.Errorf("groups feed after rename = %v, want m105-cheap-first-v2 with 2 members", groups)
	}

	// DELETE without a key is refused; with the key it succeeds.
	rec = doReq(t, srv, http.MethodDelete, "/v1/combos/m105-cheap-first-v2", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("DELETE without a key status = %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, http.MethodDelete, "/v1/combos/m105-cheap-first-v2", "", adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// List is empty again; GET by id is 404; the group mirror is gone.
	if _, list := m10_5List(t, srv, adminRaw); len(list.Combos) != 0 {
		t.Errorf("list after delete = %+v, want empty", list)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/combos/m105-cheap-first-v2", "", adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET after delete status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	groups = m10_5Groups(t, srv)
	if _, ok := groups["m105-cheap-first-v2"]; ok {
		t.Errorf("groups feed still has m105-cheap-first-v2 after delete (groups = %v)", groups)
	}
	// Deleting an unknown id is an honest 404, never a silent success.
	rec = doReq(t, srv, http.MethodDelete, "/v1/combos/m105-cheap-first-v2", "", adminRaw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("DELETE unknown id status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestDashboardM10_5AdminHeadersWired mirrors the M10-0 sweep for
// the Combos region — with the hoist-review fix applied: the M10-0
// sweep is COMMENT-MATCHABLE (it checks strings.Contains(window,
// "adminHeaders()"), and the explanatory comments at
// web/index.html:5000/5002, :5213-5214 and :5244-5245 contain that literal), so a
// sweep here would pass on comment text alone even if a call were
// missing. This test therefore asserts REAL calls only: the exact
// call statement `var headers = adminHeaders();` inside the ±700
// window of every mutating fetch. The rename DELETE reuses the
// rename POST's headers (one fresh adminHeaders() read per branch:
// a second call would find the key already cleared and abort the
// delete), so the save function must read headers exactly three
// times — create, PATCH, rename — and no second call may sit
// between the rename POST and its DELETE.
func TestDashboardM10_5AdminHeadersWired(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	js := m10_5JSRegion(t, script)
	save := m10_5FuncRegion(t, script, "combosViewSave")
	del := m10_5FuncRegion(t, script, "combosViewDelete")

	if strings.Count(script, "function adminHeaders()") != 1 {
		t.Error("adminHeaders() must be defined exactly once in the dashboard script")
	}
	// One fresh read per save branch: create, PATCH, rename.
	// Count the full call+guard statement, not the bare name:
	// the comments mention adminHeaders() without calling it.
	if n := strings.Count(save, "var headers = adminHeaders(); if(!headers) return;"); n != 3 {
		t.Errorf("combosViewSave has %d call+guard statements, want 3 (create POST, PATCH, rename POST)", n)
	}
	// The standalone delete reads it once, directly above its fetch.
	if n := strings.Count(del, "var headers = adminHeaders(); if(!headers) return;"); n != 1 {
		t.Errorf("combosViewDelete has %d call+guard statements, want 1", n)
	}
	// No mutating fetch anywhere in this slice's JS may send its
	// own hand-built Authorization header — the shared helper is
	// the only auth path.
	if strings.Contains(js, `"Authorization"`) {
		t.Error("Combos JS must not build its own Authorization header (adminHeaders() is the only auth path)")
	}

	// ±700-char sweep, M10-0 style, over both functions — but
	// matching the REAL call statement, never the bare helper
	// name (the comments carry the bare name and would match a
	// substring sweep even with the call deleted).
	mutatingMethod := regexp.MustCompile(`method:\s*("(?:POST|DELETE|PATCH|PUT)"|method\b)`)
	for _, region := range []struct {
		name, src string
	}{{"combosViewSave", save}, {"combosViewDelete", del}} {
		count := 0
		for i := 0; i < len(region.src); {
			rel := strings.Index(region.src[i:], "fetch(")
			if rel < 0 {
				break
			}
			idx := i + rel
			tail := region.src[idx:]
			if len(tail) > 700 {
				tail = tail[:700]
			}
			if mutatingMethod.MatchString(tail) {
				count++
				lo := idx - 700
				if lo < 0 {
					lo = 0
				}
				hi := idx + 700
				if hi > len(region.src) {
					hi = len(region.src)
				}
				if !strings.Contains(region.src[lo:hi], "var headers = adminHeaders();") {
					snip := tail
					if len(snip) > 120 {
						snip = snip[:120]
					}
					t.Errorf("%s: mutating fetch without a real adminHeaders() call nearby: %q", region.name, snip)
				}
			}
			i = idx + len("fetch(")
		}
		if count == 0 {
			t.Errorf("%s: no mutating fetch found (sweep covered nothing)", region.name)
		}
		// The save function must hold all four mutating fetches:
		// create POST, PATCH, rename POST, rename DELETE.
		if region.name == "combosViewSave" && count != 4 {
			t.Errorf("combosViewSave has %d mutating fetches, want 4 (create POST, PATCH, rename POST, rename DELETE)", count)
		}
		if region.name == "combosViewDelete" && count != 1 {
			t.Errorf("combosViewDelete has %d mutating fetches, want 1 (DELETE)", count)
		}
	}

	// Pin the rename reuse, anchored on the rename branch (not on
	// the first POST fetch in the function, which is the create):
	// between the rename POST fetch and the rename DELETE fetch
	// there is no second adminHeaders() call at all — the DELETE
	// reuses the POST's headers variable.
	renameStart := strings.Index(save, "/* Rename: create the new name first")
	if renameStart < 0 {
		t.Fatal("combosViewSave has no rename branch comment")
	}
	renameBranch := save[renameStart:]
	renamePost := strings.Index(renameBranch, `fetch("/v1/combos", {method: "POST"`)
	if renamePost < 0 {
		t.Fatal("combosViewSave rename branch has no POST fetch")
	}
	renameDelete := strings.Index(renameBranch[renamePost:], `method: "DELETE"`)
	if renameDelete < 0 {
		t.Fatal("combosViewSave rename branch has no DELETE fetch")
	}
	between := renameBranch[renamePost : renamePost+renameDelete]
	if strings.Contains(between, "adminHeaders()") {
		t.Error("rename DELETE must reuse the rename POST's headers — a second adminHeaders() call between them would find the key already cleared and abort the delete")
	}
	if !strings.Contains(between, "headers: headers") {
		t.Error("rename DELETE must send the same headers variable as the rename POST")
	}
}

// m10_5Branch returns one branch of combosViewSave(), delimited by
// its opening marker (or, for the rename branch, its comment) and
// the next branch's marker.
func m10_5Branch(t *testing.T, save, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(save, startMarker)
	if start < 0 {
		t.Fatalf("combosViewSave has no branch starting at %q", startMarker)
	}
	if endMarker == "" {
		return save[start:]
	}
	end := strings.Index(save[start:], endMarker)
	if end < 0 {
		t.Fatalf("combosViewSave branch %q has no end marker %q", startMarker, endMarker)
	}
	return save[start : start+end]
}

// m10_5AssertOrder fails unless each needle appears in branch in
// the given order (strictly increasing positions).
func m10_5AssertOrder(t *testing.T, branch, branchName string, needles ...string) {
	t.Helper()
	pos := -1
	for _, needle := range needles {
		rel := strings.Index(branch[pos+1:], needle)
		if rel < 0 {
			t.Errorf("%s branch: missing %q after position %d", branchName, needle, pos)
			return
		}
		pos = pos + 1 + rel
	}
}

// TestDashboardM10_5AdminHeadersHoistPerBranch covers the
// per-branch hoist directly (M10-5 review): each of the three
// save branches (create / same-name PATCH / rename) has its OWN
// fresh adminHeaders() call + guard, and the Save-button disable
// sits AFTER that branch's headers check — so with no key typed
// the function returns before the button is ever disabled, and no
// fetch fires. A single hoisted call at the top of the function
// could not serve the later branches: adminHeaders() clears the
// typed key on every call, so the PATCH / rename fetches would
// send a cleared key.
func TestDashboardM10_5AdminHeadersHoistPerBranch(t *testing.T) {
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	save := m10_5FuncRegion(t, script, "combosViewSave")

	// Exactly three call+guard statements in the whole function —
	// one per branch, none hoisted to the top of the function.
	const guard = "var headers = adminHeaders(); if(!headers) return;"
	if n := strings.Count(save, guard); n != 3 {
		t.Fatalf("combosViewSave has %d %q statements, want 3 (one per branch)", n, guard)
	}
	// No hoisted call before the first branch opens: everything
	// before `if(combosViewEditing === null){` is validation only.
	createStart := strings.Index(save, "if(combosViewEditing === null){")
	if createStart < 0 {
		t.Fatal("combosViewSave has no create branch")
	}
	if strings.Contains(save[:createStart], "adminHeaders()") {
		t.Error("combosViewSave must not call adminHeaders() before the first branch (no hoisted call at the top)")
	}

	branches := []struct {
		name, start, end, fetch string
	}{
		{"create", "if(combosViewEditing === null){", "if(name === combosViewEditing){",
			`fetch("/v1/combos", {method: "POST"`},
		{"patch", "if(name === combosViewEditing){", "/* Rename: create the new name first",
			`fetch("/v1/combos/" + encodeURIComponent(combosViewEditing), {method: "PATCH"`},
		{"rename", "/* Rename: create the new name first", "",
			`fetch("/v1/combos", {method: "POST"`},
	}
	for _, br := range branches {
		branch := m10_5Branch(t, save, br.start, br.end)
		// One fresh call + guard in this branch only.
		if n := strings.Count(branch, guard); n != 1 {
			t.Errorf("%s branch has %d call+guard statements, want exactly 1 (its own fresh read)", br.name, n)
		}
		// The branch's fetch sends the headers variable it just read.
		if !strings.Contains(branch, br.fetch) {
			t.Errorf("%s branch missing its fetch %q", br.name, br.fetch)
		}
		// Order inside the branch: guard first, then the Save
		// button is disabled, then the fetch fires. With no key
		// typed the guard returns before the disable, so the
		// button is never left disabled and no fetch fires.
		m10_5AssertOrder(t, branch, br.name,
			guard,
			"if(saveBtn) saveBtn.disabled = true;",
			br.fetch,
		)
		if !strings.Contains(branch, "headers: headers") {
			t.Errorf("%s branch fetch does not send the headers variable", br.name)
		}
	}

	// The rename branch holds two fetches (POST then DELETE) on
	// the ONE guard above: exactly one call in the branch, and
	// the DELETE reuses that call's headers object.
	rename := m10_5Branch(t, save, "/* Rename: create the new name first", "")
	if n := strings.Count(rename, "var headers = adminHeaders();"); n != 1 {
		t.Errorf("rename branch has %d adminHeaders() calls, want 1 (the DELETE reuses the POST's headers)", n)
	}
	m10_5AssertOrder(t, rename, "rename",
		guard,
		"if(saveBtn) saveBtn.disabled = true;",
		`fetch("/v1/combos", {method: "POST"`,
		`fetch("/v1/combos/" + encodeURIComponent(oldId), {method: "DELETE"`,
	)
	// The standalone delete is the same shape: guard, then fetch.
	del := m10_5FuncRegion(t, script, "combosViewDelete")
	m10_5AssertOrder(t, del, "combosViewDelete",
		guard,
		`fetch("/v1/combos/" + encodeURIComponent(id), {method: "DELETE"`,
	)
}

// TestDashboardM10_5InlineScriptSyntax runs node --check over the
// extracted inline script of the served dashboard (same pattern as
// TestDashboardM10_4InlineScriptSyntax). It skips when node is not
// installed.
func TestDashboardM10_5InlineScriptSyntax(t *testing.T) {
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
