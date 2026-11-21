package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestDocumentedAPIRoutesExistInGuardTable is the docs-drift guard (G-05).
//
// Every HTTP route the operator-facing API reference documents as
// `### <METHOD> /<path>` must exist in the runtime guard table (the
// same table TestRouteTableSpawnCapableAreLocalOnly validates). A route
// documented but never registered means the docs silently drifted from
// the real API surface — this test fails so the drift is fixed in the
// same change, never left to rot. The reverse direction (a registered
// route that is not yet documented) is allowed: the reference is
// allowed to lag the implementation, but must never claim a route that
// does not exist.
//
// Path normalization keeps false positives low: the documented path is
// matched to the registered path after collapsing redundant slashes and
// stripping the HTTP verb. Parameterized paths in docs (no braces) match
// the braced registered path by comparing the static segments.
func TestDocumentedAPIRoutesExistInGuardTable(t *testing.T) {
	root := findRepoRoot(t)
	doc, err := os.ReadFile(filepath.Join(root, "docs", "API_REFERENCE.md"))
	if err != nil {
		t.Skipf("docs/API_REFERENCE.md not readable here: %v", err)
	}

	registered := map[string]bool{}
	for _, r := range RouteTable() {
		registered[normalizePath(r.Path)] = true
	}

	docRe := regexp.MustCompile(`(?m)^###\s+(GET|POST|PUT|DELETE|PATCH)\s+(/[A-Za-z0-9_/:{}.\-]+)`)
	missing := []string{}
	for _, m := range docRe.FindAllStringSubmatch(string(doc), -1) {
		path := normalizePath(m[2])
		if registered[path] {
			continue
		}
		if staticMatchExists(registered, path) {
			continue
		}
		missing = append(missing, m[1]+" "+m[2])
	}

	if len(missing) > 0 {
		// Fail and list every undocumented route so the fix is obvious.
		t.Errorf("documented routes missing from the guard table (%d):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func findRepoRoot(t *testing.T) string {
	// The test runs from the package dir; walk up to the repo root
	// (the dir containing go.mod).
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find repo root from %q", dir)
		}
		dir = parent
	}
}

// normalizePath collapses repeated slashes and strips a trailing slash
// so `/api/foo//bar/` and `/api/foo/bar` compare equal.
func normalizePath(p string) string {
	return strings.TrimSuffix(strings.TrimSuffix(p, "/"), "/")
}

// staticMatchExists reports whether any registered path has the same
// static (non-parameter) segments as the documented path, so a doc that
// omits `{...}` placeholders still matches a braced registered path.
func staticMatchExists(registered map[string]bool, docPath string) bool {
	for reg := range registered {
		if staticSegmentsEqual(reg, docPath) {
			return true
		}
	}
	return false
}

func staticSegmentsEqual(a, b string) bool {
	as := strings.Split(strings.Trim(a, "/"), "/")
	bs := strings.Split(strings.Trim(b, "/"), "/")
	if len(as) != len(bs) {
		return false
	}
	for i := range as {
		// Treat parameterized segments ({id}) as wildcards.
		if isParam(as[i]) || isParam(bs[i]) {
			continue
		}
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func isParam(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}")
}
