package catalog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoBrandPatterns are built from per-character fragments so the literal
// prohibited names never appear in this file (the file itself must not trip
// the existing brand-scrub scan). CI patterns are case-insensitive; the spaced
// brand variant stays capitalized-only so lowercase prose never false-positives.
func repoBrandPatterns() []*regexp.Regexp {
	pat := func(frag ...string) *regexp.Regexp {
		return regexp.MustCompile(strings.Join(frag, ""))
	}
	ci := func(frag ...string) *regexp.Regexp {
		return regexp.MustCompile("(?i)" + strings.Join(frag, ""))
	}
	sysSpaced := pat("S", "y", "s", "t", "e", "m", " ", "O", "n", "e")
	sysContig := pat("S", "y", "s", "t", "e", "m", "O", "n", "e")
	omniPat := ci("o", "m", "n", "i", "[- _]?r", "o", "u", "t", "e")
	cheaper := ci("c", "h", "e", "a", "p", "e", "r", "[- _]?i", "n", "f", "e", "r", "e", "n", "c", "e")
	openSSE := ci("o", "p", "e", "n", "[-_]s", "s", "e")
	glyph := ci("o", "m", "n", "i", "G", "l", "y", "p", "h")
	skills := ci("o", "m", "n", "i", "[- _]?s", "k", "i", "l", "l", "s")
	omniVar := ci("O", "M", "N", "I", "_")
	refRouter := ci("reference", " ", "router")
	return []*regexp.Regexp{sysSpaced, sysContig, omniPat, cheaper, openSSE, glyph, skills, omniVar, refRouter}
}

// TestRepoBrandGateSelfTest proves the gate patterns actually match every
// seeded banned string (the gate is proven, not just widened on paper), and
// that sanctioned lowercase prose does not false-positive. Seed strings are
// concatenated at runtime so no literal prohibited name appears in source.
func TestRepoBrandGateSelfTest(t *testing.T) {
	pats := repoBrandPatterns()
	matchAny := func(s string) bool {
		for _, p := range pats {
			if p.MatchString(s) {
				return true
			}
		}
		return false
	}
	mustMatch := []string{
		"the System" + " One scoring", "System" + "One call",
		"reference" + " router",
		"open" + "-sse" + "/services/x.ts",
		"Omni" + "Glyph", "omni" + "-skills", "OMNI" + "_FOO",
		"omni" + "-route", "omni" + "_route",
		"cheaper" + "inference", "Cheaper" + " Inference",
	}
	for _, s := range mustMatch {
		if !matchAny(s) {
			t.Errorf("gate pattern did NOT match seeded banned string %q", s)
		}
	}
	for _, s := range []string{"non-system" + " one", "/v1/system" + "one", "system" + " one lowercase"} {
		if matchAny(s) {
			t.Errorf("gate pattern false-positived on sanctioned lowercase prose %q", s)
		}
	}
}

// repoScanExcludes mirrors the naming gate's disclosed exclusions plus the
// removal-history test files that cite a retired endpoint in a commit-hash
// comment (sanctioned history, not a new stain).
var repoScanExcludes = []string{
	"go.mod", "go.sum",
	"internal/catalog/providers_reference.go",
	"internal/catalog/verified.go",
	"internal/catalog/providers_additional.go",
	"docs/catalog-audit-", "web/catalog-audit.json",
	".github/", ".goreleaser.yml", "bin/codvian-router",
	"cmd/codvian-router/release_verify.go",
	"internal/release/verify.go",
	"docs/POST_P1_FINDINGS_LEDGER.md",
	"docs/POST_P1_REMEDIATION_REPORT.md",
	"scripts/check-naming.py", "scripts/check-naming.sh",
	"internal/catalog/brand_scrub_regression_test.go",
	"internal/catalog/repo_brand_gate_test.go",
}

func repoScanExcluded(path string) bool {
	for _, e := range repoScanExcludes {
		if strings.HasSuffix(path, e) || strings.Contains(path, e) {
			return true
		}
	}
	return false
}

// TestRepoWideSpacedBrandZero scans the whole repo (minus disclosed
// exclusions) and fails if the spaced/contiguous proper-noun brand variant
// appears anywhere — the stain class this run introduced and then scrubbed.
func TestRepoWideSpacedBrandZero(t *testing.T) {
	root := "../.."
	spaced := regexp.MustCompile(`S[y][s][t][e][m][ ][O][n][e]`)
	contig := regexp.MustCompile(`S[y][s][t][e][m][O][n][e]`)
	others := repoBrandPatterns()
	found := 0
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if repoScanExcluded(rel) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil || strings.ContainsRune(string(data), 0) {
			return nil
		}
		for i, ln := range strings.Split(string(data), "\n") {
			other := false
			for _, p := range others {
				if p != nil && p.MatchString(ln) {
					other = true
					break
				}
			}
			if spaced.MatchString(ln) || contig.MatchString(ln) || other {
				t.Errorf("brand variant at %s:%d: %s", rel, i+1, strings.TrimSpace(ln))
				found++
				if found > 20 {
					return filepath.SkipAll
				}
			}
		}
		return nil
	})
	if found == 0 {
		t.Log("repo-wide spaced/contiguous brand variant: ZERO hits")
	}
}
