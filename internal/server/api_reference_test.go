// M8.21 — API reference coverage tests.
//
// The guard route table (guard.go, guardRouteTable) is the single
// source of truth for the API surface. These tests enforce that the
// human doc (docs/API_REFERENCE.md), the machine summary
// (docs/llm.txt), the try-it page (web/tryit.html) and the fixture
// (tests/fixtures/v1/m8.21/routes.json) all cover every route in
// that table — a route added to the table without a doc section
// fails the gate. Regenerate the docs with:
//
//	python3 scripts/gen-api-docs.py
package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot resolves the worktree root from this test file's location,
// so the tests work regardless of the test runner's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with: python3 scripts/gen-api-docs.py)", rel, err)
	}
	return string(b)
}

// routeSection returns the doc text for one route's "### `METHOD path`"
// section (up to the next route heading), or "" if the heading is absent.
func routeSection(doc, method, path string) string {
	heading := "### `" + method + " " + path + "`"
	start := strings.Index(doc, heading)
	if start < 0 {
		return ""
	}
	rest := doc[start+len(heading):]
	if next := strings.Index(rest, "\n### `"); next >= 0 {
		return rest[:next]
	}
	return rest
}

// TestAPIReferenceCoversEveryRoute is the M8 exit-gate enforcement:
// every guard-table route must have a section in docs/API_REFERENCE.md
// stating its tier, its scopes (or that any authenticated key suffices)
// and its auth rule. Removing a section — or adding a route without
// documenting it — fails this test.
func TestAPIReferenceCoversEveryRoute(t *testing.T) {
	doc := readRepoFile(t, "docs/API_REFERENCE.md")
	if !strings.Contains(doc, "python3 scripts/gen-api-docs.py") {
		t.Error("API_REFERENCE.md lost its regeneration note — it must say how to regenerate it")
	}
	table := RouteTable()
	if len(table) == 0 {
		t.Fatal("guard route table is empty")
	}
	for _, r := range table {
		key := r.Method + " " + r.Path
		sec := routeSection(doc, r.Method, r.Path)
		if sec == "" {
			t.Errorf("API_REFERENCE.md is missing a section for %s (run: python3 scripts/gen-api-docs.py)", key)
			continue
		}
		if !strings.Contains(sec, "`"+string(r.Tier)+"`") {
			t.Errorf("API_REFERENCE.md section %s does not state tier %s", key, r.Tier)
		}
		if len(r.Scopes) > 0 {
			for _, sc := range r.Scopes {
				if !strings.Contains(sec, "`"+sc+"`") {
					t.Errorf("API_REFERENCE.md section %s does not state scope %s", key, sc)
				}
			}
		} else if !strings.Contains(sec, "any authenticated key") && !strings.Contains(sec, "None") {
			t.Errorf("API_REFERENCE.md section %s does not state its scope/auth rule", key)
		}
		if !strings.Contains(sec, r.Description) {
			t.Errorf("API_REFERENCE.md section %s does not carry the guard-table description verbatim", key)
		}
		if r.SpawnCapable && !strings.Contains(sec, "Spawn-capable:** yes") {
			t.Errorf("API_REFERENCE.md section %s does not flag spawn-capable", key)
		}
	}
}

// TestLLMTxtCoversEveryRoute requires every guard-table route to
// appear as a line in docs/llm.txt (the compact machine-readable
// summary), with its tier and auth class.
func TestLLMTxtCoversEveryRoute(t *testing.T) {
	doc := readRepoFile(t, "docs/llm.txt")
	for _, r := range RouteTable() {
		line := r.Method + " " + r.Path + " | " + string(r.Tier) + " |"
		if !strings.Contains(doc, line) {
			t.Errorf("docs/llm.txt is missing route %s %s (tier %s)", r.Method, r.Path, r.Tier)
		}
	}
	if !strings.Contains(doc, "/openapi.json carries no scopes") {
		t.Error("docs/llm.txt lost its /openapi.json gap note")
	}
}

// tryItRoutes extracts the embedded ROUTES JSON blob from web/tryit.html.
func tryItRoutes(t *testing.T) []GuardRoute {
	t.Helper()
	html := readRepoFile(t, "web/tryit.html")
	start := strings.Index(html, "const ROUTES = ")
	if start < 0 {
		t.Fatal("web/tryit.html has no embedded ROUTES blob")
	}
	blob := html[start+len("const ROUTES = "):]
	end := strings.Index(blob, "];\n// END TRYIT-ROUTES-JSON")
	if end < 0 {
		t.Fatal("web/tryit.html ROUTES blob has no END TRYIT-ROUTES-JSON marker")
	}
	var routes []GuardRoute
	if err := json.Unmarshal([]byte(blob[:end+1]), &routes); err != nil {
		t.Fatalf("web/tryit.html embedded ROUTES JSON does not parse: %v", err)
	}
	return routes
}

// routesEqual compares a generated route list against the guard table
// field by field — the try-it page must never show a stale route.
func routesEqual(t *testing.T, what string, got []GuardRoute) {
	t.Helper()
	want := RouteTable()
	if len(got) != len(want) {
		t.Fatalf("%s has %d routes, guard table has %d", what, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Method != w.Method || g.Path != w.Path || g.Tier != w.Tier ||
			g.SpawnCapable != w.SpawnCapable || g.AuthExempt != w.AuthExempt ||
			g.Bootstrap != w.Bootstrap || g.Description != w.Description ||
			strings.Join(g.Scopes, ",") != strings.Join(w.Scopes, ",") {
			t.Errorf("%s route %d differs from guard table:\n got %+v\nwant %+v", what, i, g, w)
		}
	}
}

// TestTryItRoutesMatchGuardTable proves the try-it page renders the
// live guard table, not a hand-written copy: remove or change a route
// in the embedded JSON and this fails.
func TestTryItRoutesMatchGuardTable(t *testing.T) {
	routesEqual(t, "web/tryit.html embedded ROUTES", tryItRoutes(t))
	html := readRepoFile(t, "web/tryit.html")
	// The page must send requests only to the router it was served
	// from, and must never persist the gateway key.
	if strings.Contains(html, "localStorage.") || strings.Contains(html, "sessionStorage.") ||
		strings.Contains(html, "document.cookie") {
		t.Error("web/tryit.html stores state in the browser — the gateway key must live in page memory only")
	}
	if !strings.Contains(html, "fetch(base + path") {
		t.Error("web/tryit.html does not send try-it requests to the configured base URL")
	}
}

// TestTryItFixtureMatchesGuardTable proves the checked-in fixture
// (tests/fixtures/v1/m8.21/routes.json) is the guard table, so other
// slices can consume it without re-parsing guard.go.
func TestTryItFixtureMatchesGuardTable(t *testing.T) {
	raw := readRepoFile(t, "tests/fixtures/v1/m8.21/routes.json")
	var routes []GuardRoute
	if err := json.Unmarshal([]byte(raw), &routes); err != nil {
		t.Fatalf("routes.json does not parse: %v", err)
	}
	routesEqual(t, "tests/fixtures/v1/m8.21/routes.json", routes)
}

// TestOpenAPIGapIsDocumented re-checks the live /openapi.json route
// coverage and proves the documented gap is real: the live spec
// carries no structured scope information (no "scopes" or "security"
// field on any operation), which is exactly why API_REFERENCE.md
// (not the spec) is the source for scopes and auth. NOTE: do NOT
// probe this with a substring search for "scope" — the spec's
// summaries are the guard-table descriptions verbatim, and a few of
// those mention the word "scope" in prose (e.g. "(admin scope)");
// prose is not a machine-readable scope, and a substring probe
// false-positives on it. Check for the field instead.
func TestOpenAPIGapIsDocumented(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, "GET", "/openapi.json", "", "")
	if rec.Code != 200 {
		t.Fatalf("GET /openapi.json: status %d, want 200", rec.Code)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Summary string `json:"summary"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	for _, r := range RouteTable() {
		methods, ok := spec.Paths[r.Path]
		if !ok {
			t.Errorf("live /openapi.json is missing path %s", r.Path)
			continue
		}
		if _, ok := methods[strings.ToLower(r.Method)]; !ok {
			t.Errorf("live /openapi.json path %s is missing method %s", r.Path, r.Method)
		}
	}
	// The gap itself, checked structurally: no operation carries a
	// scopes/security field. Re-decode generically so an added field
	// is actually seen (the typed struct above drops unknown keys).
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	paths, _ := raw["paths"].(map[string]any)
	for path, opsAny := range paths {
		ops, _ := opsAny.(map[string]any)
		for method, opAny := range ops {
			op, _ := opAny.(map[string]any)
			for _, field := range []string{"scopes", "security"} {
				if _, ok := op[field]; ok {
					t.Errorf("live /openapi.json %s %s now carries a %q field — update the gap note in API_REFERENCE.md (the gap is closed)", method, path, field)
				}
			}
		}
	}
	if _, ok := raw["security"]; ok {
		t.Error("live /openapi.json now carries a top-level security field — update the gap note in API_REFERENCE.md (the gap is closed)")
	}
	doc := readRepoFile(t, "docs/API_REFERENCE.md")
	if !strings.Contains(doc, "The `/openapi.json` gap") {
		t.Error("API_REFERENCE.md lost its /openapi.json gap section")
	}
}
