// P1.3 — the compatibility matrix must not overclaim.
//
// Every row marked "verified" must name a Go test that actually exists
// in this tree, so a claim cannot drift away from its evidence. A row
// with no evidence is rejected outright; the credential must be one of
// the three forms the server accepts.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// p13RepoRoot walks up from the package dir to the module root.
func p13RepoRoot(t *testing.T) string {
	t.Helper()
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
			t.Fatalf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// p13TestExists reports whether a function named name is declared in any
// *_test.go file under the repo root.
func p13TestExists(t *testing.T, root, name string) bool {
	t.Helper()
	pattern := regexp.MustCompile(`func\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	found := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || found {
			return nil
		}
		if info.IsDir() {
			if base := info.Name(); base == ".git" || base == "node_modules" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if pattern.Match(b) {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestP13ClientMatrixClaimsMapToRealTests(t *testing.T) {
	root := p13RepoRoot(t)
	rows := ClientMatrix()
	if len(rows) == 0 {
		t.Fatal("the client matrix is empty")
	}
	allowedCred := map[string]bool{"bearer": true, "x-api-key": true, "x-goog-api-key": true, "bearer or x-api-key": true}
	for _, r := range rows {
		if r.Name == "" || r.Dialect == "" || r.Endpoint == "" {
			t.Fatalf("incomplete row: %+v", r)
		}
		if !allowedCred[r.Credential] {
			t.Fatalf("%s: credential %q is not one the server accepts", r.Name, r.Credential)
		}
		switch r.Status {
		case "verified":
			if r.Test == "" {
				t.Fatalf("%s: verified without naming a test", r.Name)
			}
			if !p13TestExists(t, root, r.Test) {
				t.Fatalf("%s: claims test %q, which does not exist in this tree", r.Name, r.Test)
			}
		case "partial":
			if r.Test != "" && !p13TestExists(t, root, r.Test) {
				t.Fatalf("%s: cites test %q, which does not exist", r.Name, r.Test)
			}
			if r.Note == "" {
				t.Fatalf("%s: partial without an honest caveat", r.Name)
			}
		case "unverified":
			if r.Test != "" {
				t.Fatalf("%s: unverified rows must not cite a test", r.Name)
			}
		default:
			t.Fatalf("%s: unknown status %q", r.Name, r.Status)
		}
	}
}

func TestP13ClientMatrixJSONIsWellFormed(t *testing.T) {
	rows := ClientMatrix()
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var back []clientSupport
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back) != len(rows) {
		t.Fatalf("round trip lost rows: %d -> %d", len(rows), len(back))
	}
}

func TestP13ClientMatrixIsSorted(t *testing.T) {
	rows := ClientMatrix()
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Name > rows[i].Name {
			t.Fatalf("rows not sorted: %q before %q", rows[i-1].Name, rows[i].Name)
		}
	}
}
