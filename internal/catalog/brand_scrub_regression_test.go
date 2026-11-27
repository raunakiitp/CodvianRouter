package catalog

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// W5 stain regression (A3): the retired brand strings scrubbed from the
// catalog code in v2 S1 (commit d5ef5d4) must never come back. This test
// scans every Go source file in the catalog packages (internal/catalog,
// internal/provider) for the retired brand substrings. Each pattern is
// assembled here from fragments so this test file does not itself contain
// the literals it forbids.
//
// Documented exceptions — a hit is tolerated only for these exact
// substrings; any other retired string in the same files still fails:
//   - the retired catalog row's technical ID (owner decision D2, commit
//     e78afac): the row is deliberately kept with Retired=true and a
//     written reason so an add-by-ID attempt is explained instead of
//     reporting an unknown provider, and the retirement tests name the ID;
//   - that same row's display name, in providers_reference.go only (the
//     Name field of the retained retired row).
//
// Out of scope on purpose: the repo-wide comment convention that cites the
// upstream reference source paths (e.g. internal/compress, internal/router)
// and the pre-existing operator-facing env-var knobs in internal/compress —
// renaming env vars is a breaking change, not a scrub. The prose phrase
// naming the upstream parity target is the tree's sanctioned post-scrub
// terminology and is not a pattern here.
func TestCatalogPackagesFreeOfRetiredBrandStrings(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	pkgDir := filepath.Dir(thisFile)
	repoRoot := filepath.Clean(filepath.Join(pkgDir, "..", ".."))

	cheaperDisplay := strings.Join([]string{"cheaper", " inference"}, "")
	cheaperID := strings.Join([]string{"cheaper", "inference"}, "")
	patterns := []string{
		strings.Join([]string{"omni", "route"}, ""),
		strings.Join([]string{"omni", "-", "route"}, ""),
		strings.Join([]string{"omni", "_"}, ""),
		strings.Join([]string{"omni", "glyph"}, ""),
		strings.Join([]string{"omni", "-", "skills"}, ""),
		strings.Join([]string{"system", "one"}, ""),
		strings.Join([]string{"open", "-", "sse"}, ""),
		cheaperDisplay,
		cheaperID,
	}
	perFileAllowed := map[string][]string{
		"internal/catalog/providers_reference.go": {cheaperDisplay},
	}
	globalAllowed := []string{cheaperID}

	dirs := []string{pkgDir, filepath.Join(repoRoot, "internal", "provider")}
	var hits []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			low := strings.ToLower(string(data))
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			for _, p := range patterns {
				if !strings.Contains(low, p) {
					continue
				}
				tolerated := false
				for _, g := range globalAllowed {
					if p == g {
						tolerated = true
					}
				}
				for _, a := range perFileAllowed[rel] {
					if p == a {
						tolerated = true
					}
				}
				if !tolerated {
					hits = append(hits, rel+" contains retired brand substring ("+p+")")
				}
			}
		}
	}
	if len(hits) > 0 {
		t.Fatalf("retired brand strings are back in the catalog packages:\n  %s",
			strings.Join(hits, "\n  "))
	}
}
