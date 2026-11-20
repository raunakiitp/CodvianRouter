package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.tar.gz", "fake-archive-bytes")
	writeFile(t, dir, "b.zip", "other-bytes")
	content, err := GenerateChecksums(dir, []string{"b.zip", "a.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	// Entries are sorted by name regardless of input order.
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "  a.tar.gz") || !strings.HasSuffix(lines[1], "  b.zip") {
		t.Fatalf("unexpected checksums content:\n%s", content)
	}
	// Each digest matches a direct SHA-256 of the file.
	for _, name := range []string{"a.tar.gz", "b.zip"} {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		sum := sha256.Sum256(b)
		if !strings.Contains(content, hex.EncodeToString(sum[:])) {
			t.Fatalf("checksums content missing digest for %s:\n%s", name, content)
		}
	}
	if err := VerifyChecksums(dir, []byte(content)); err != nil {
		t.Fatalf("VerifyChecksums on a just-generated manifest: %v", err)
	}
}

// TestGenerateMatchesSha256sum proves the generated manifest is the
// same format `sha256sum -c` reads, by running the real tool on it.
func TestGenerateMatchesSha256sum(t *testing.T) {
	sha256sum, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Skip("sha256sum not on PATH")
	}
	dir := t.TempDir()
	writeFile(t, dir, "codvian-router_0.1.0_linux_amd64.tar.gz", "archive")
	content, err := GenerateChecksums(dir, []string{"codvian-router_0.1.0_linux_amd64.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(sha256sum, "-c", "checksums.txt")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sha256sum -c rejected our manifest: %v\n%s", err, out)
	}
}

func TestVerifyRejectsTamperedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "release.tar.gz", "genuine bytes")
	content, err := GenerateChecksums(dir, []string{"release.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	// Tamper: flip one byte, as a swapped binary would.
	writeFile(t, dir, "release.tar.gz", "genuine byteX")
	if err := VerifyChecksums(dir, []byte(content)); err == nil {
		t.Fatal("VerifyChecksums accepted a tampered file")
	} else if !strings.Contains(err.Error(), "release.tar.gz") || !strings.Contains(err.Error(), "MISMATCH") {
		t.Fatalf("tamper error does not name the file and the mismatch: %v", err)
	}
}

func TestVerifyRejectsMissingFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.tar.gz", "x")
	content, err := GenerateChecksums(dir, []string{"a.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "a.tar.gz")); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksums(dir, []byte(content)); err == nil {
		t.Fatal("VerifyChecksums accepted a manifest whose file is missing")
	} else if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing-file error does not say missing: %v", err)
	}
}

func TestParseChecksumsRejectsMalformed(t *testing.T) {
	good := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  empty.txt\n"
	cases := []struct {
		name    string
		content string
	}{
		{"empty manifest", ""},
		{"blank line", good + "\n"},
		{"short digest", "abcd  empty.txt\n"},
		{"non-hex digest", "g3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  empty.txt\n"},
		{"uppercase digest", "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855  empty.txt\n"},
		{"no separator", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855empty.txt\n"},
		{"duplicate name", good + good},
		{"parent traversal", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ../escape.txt\n"},
		{"absolute path", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  /etc/passwd\n"},
		{"non-canonical name", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ./empty.txt\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseChecksums([]byte(tc.content)); err == nil {
				t.Fatalf("ParseChecksums accepted %q", tc.content)
			}
		})
	}
}

func TestParseChecksumsAcceptsBinaryModeSeparator(t *testing.T) {
	// `sha256sum -b` separates digest and name with " *".
	content := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 *empty.txt\n"
	entries, err := ParseChecksums([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "empty.txt" {
		t.Fatalf("unexpected entries: %+v", entries)
	}
}

func TestGenerateChecksumsRejectsNoFiles(t *testing.T) {
	if _, err := GenerateChecksums(t.TempDir(), nil); err == nil {
		t.Fatal("GenerateChecksums accepted an empty file list")
	}
}

func TestVerifyChecksumsFileCountsEntries(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.tar.gz", "x")
	writeFile(t, dir, "a.tar.gz.sbom.json", "{}")
	content, err := GenerateChecksums(dir, []string{"a.tar.gz", "a.tar.gz.sbom.json"})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(manifest, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := VerifyChecksumsFile(dir, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("verified %d entries, want 2", n)
	}
}

func TestGenerateSBOMShape(t *testing.T) {
	deps := []Module{
		{Path: "github.com/raunakiitp/codvian-router", Main: true},
		{Path: "golang.org/x/crypto", Version: "v0.24.0"},
		{Path: "modernc.org/sqlite", Version: "v1.34.0"},
		// A replaced module ships under the replacement's identity.
		{Path: "example.com/old", Version: "v1.0.0", Replace: &Module{Path: "example.com/new", Version: "v1.1.0"}},
		// A local-path replace has no version: listed, never invented.
		{Path: "example.com/local", Version: "v1.0.0", Replace: &Module{Path: "example.com/local", Version: ""}},
		// Duplicate path (same module seen twice) collapses to one.
		{Path: "golang.org/x/crypto", Version: "v0.24.0"},
	}
	out, err := GenerateSBOM("github.com/raunakiitp/codvian-router", "v0.1.0", deps)
	if err != nil {
		t.Fatal(err)
	}
	var sbom map[string]any
	if err := json.Unmarshal(out, &sbom); err != nil {
		t.Fatalf("SBOM is not valid JSON: %v\n%s", err, out)
	}
	if sbom["bomFormat"] != "CycloneDX" || sbom["specVersion"] != "1.6" {
		t.Fatalf("unexpected SBOM format header: %v", sbom)
	}
	serial, _ := sbom["serialNumber"].(string)
	if !strings.HasPrefix(serial, "urn:uuid:") {
		t.Fatalf("SBOM serialNumber is not a urn:uuid: %q", serial)
	}
	meta, _ := sbom["metadata"].(map[string]any)
	comp, _ := meta["component"].(map[string]any)
	if comp["name"] != "github.com/raunakiitp/codvian-router" || comp["version"] != "v0.1.0" {
		t.Fatalf("metadata component is not the root module: %v", comp)
	}
	if comp["purl"] != "pkg:golang/github.com/raunakiitp/codvian-router@v0.1.0" {
		t.Fatalf("root purl wrong: %v", comp["purl"])
	}
	components, _ := sbom["components"].([]any)
	if len(components) != 4 {
		t.Fatalf("got %d components, want 4 (crypto, sqlite, replaced, local)", len(components))
	}
	// Sorted by purl, so the first component is the alphabetically first.
	first, _ := components[0].(map[string]any)
	if first["purl"] != "pkg:golang/example.com/local" {
		t.Fatalf("components are not sorted by purl; first is %v", first["purl"])
	}
	byName := map[string]map[string]any{}
	for _, c := range components {
		cm, _ := c.(map[string]any)
		byName[cm["name"].(string)] = cm
	}
	if byName["example.com/new"]["version"] != "v1.1.0" {
		t.Fatalf("replaced module did not ship under the replacement version: %v", byName["example.com/new"])
	}
	if _, hasVersion := byName["example.com/local"]["version"]; hasVersion {
		t.Fatalf("local-path replace got an invented version: %v", byName["example.com/local"])
	}
	if byName["golang.org/x/crypto"]["purl"] != "pkg:golang/golang.org/x/crypto@v0.24.0" {
		t.Fatalf("crypto purl wrong: %v", byName["golang.org/x/crypto"]["purl"])
	}
}

func TestGenerateSBOMEmptyVersionIsHonest(t *testing.T) {
	out, err := GenerateSBOM("github.com/raunakiitp/codvian-router", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var sbom map[string]any
	if err := json.Unmarshal(out, &sbom); err != nil {
		t.Fatal(err)
	}
	meta, _ := sbom["metadata"].(map[string]any)
	comp, _ := meta["component"].(map[string]any)
	if comp["version"] != "0.0.0-dev" {
		t.Fatalf("unversioned root got version %v, want 0.0.0-dev", comp["version"])
	}
}

func TestParseGoListJSON(t *testing.T) {
	in := `{"Path":"github.com/raunakiitp/codvian-router","Main":true}` + "\n" +
		`{"Path":"golang.org/x/crypto","Version":"v0.24.0"}` + "\n"
	mods, err := ParseGoListJSON(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(mods) != 2 || !mods[0].Main || mods[1].Version != "v0.24.0" {
		t.Fatalf("unexpected modules: %+v", mods)
	}
	if _, err := ParseGoListJSON(strings.NewReader(`{"Path":"x"}{garbage`)); err == nil {
		t.Fatal("ParseGoListJSON accepted trailing garbage")
	}
	if _, err := ParseGoListJSON(strings.NewReader("")); err == nil {
		t.Fatal("ParseGoListJSON accepted empty input")
	}
}

// TestSBOMFromRealModuleGraph is the SBOM smoke test: it runs `go list
// -m -json all` on this very module and asserts the generated SBOM
// describes the real graph (root module + the direct dependencies).
func TestSBOMFromRealModuleGraph(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not on PATH")
	}
	// The test runs from inside the module (package dir is under it).
	cmd := exec.Command(goBin, "list", "-m", "-json", "all")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -m -json all failed: %v", err)
	}
	mods, err := ParseGoListJSON(strings.NewReader(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	var root Module
	var deps []Module
	for _, m := range mods {
		if m.Main {
			root = m
		} else {
			deps = append(deps, m)
		}
	}
	if root.Path != "github.com/raunakiitp/codvian-router" {
		t.Fatalf("main module is %q, want github.com/raunakiitp/codvian-router", root.Path)
	}
	sbomBytes, err := GenerateSBOM(root.Path, "v0.1.0", deps)
	if err != nil {
		t.Fatal(err)
	}
	var sbom map[string]any
	if err := json.Unmarshal(sbomBytes, &sbom); err != nil {
		t.Fatal(err)
	}
	components, _ := sbom["components"].([]any)
	names := map[string]bool{}
	for _, c := range components {
		cm, _ := c.(map[string]any)
		names[cm["name"].(string)] = true
	}
	for _, want := range []string{"golang.org/x/crypto", "modernc.org/sqlite"} {
		if !names[want] {
			t.Fatalf("real-graph SBOM is missing direct dependency %s (got %d components)", want, len(components))
		}
	}
	if len(components) < 3 {
		t.Fatalf("real-graph SBOM has only %d components, want at least 3", len(components))
	}
}
