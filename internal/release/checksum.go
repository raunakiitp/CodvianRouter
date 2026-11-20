// Package release provides release-artifact integrity helpers for
// Codvian Router: SHA-256 checksum manifests (the checksums.txt format
// GoReleaser publishes and `sha256sum -c` reads) and CycloneDX SBOM
// generation from the Go module graph.
//
// This package never touches the network and never shells out: it is the
// offline, dependency-free half of the release-signing pipeline. The
// online half — Sigstore cosign keyless signatures over checksums.txt and
// each SBOM, plus SLSA build-provenance attestations — lives in CI
// (.github/workflows/release.yml) and is documented in
// docs/RELEASE-SIGNING.md. A signature is what binds a checksums.txt to
// the project; this package only proves that files match a manifest it
// was given, never where that manifest came from.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxChecksumsBytes bounds how large a checksums manifest may be before
// VerifyChecksums refuses it. A real checksums.txt for this project is a
// few kilobytes (one line per archive + SBOM); 1 MiB is far above that
// and stops a hostile manifest from being a memory problem.
const MaxChecksumsBytes = 1 << 20

// Entry is one line of a checksums manifest: the expected SHA-256 of one
// file, named relative to the directory the manifest sits in.
type Entry struct {
	// SHA256 is the lowercase hex digest (64 characters).
	SHA256 string
	// Name is the file name exactly as it appears in the manifest.
	Name string
}

// HashFile returns the lowercase hex SHA-256 of the file at path,
// streaming it so large release archives never sit whole in memory.
func HashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// GenerateChecksums returns checksums.txt content (the `sha256sum`
// two-space format, entries sorted by file name) for the named files in
// dir. The result is byte-identical to what `sha256sum` would print for
// the same files, so `sha256sum -c` accepts it.
func GenerateChecksums(dir string, names []string) (string, error) {
	if len(names) == 0 {
		return "", errors.New("release: no files to checksum")
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b strings.Builder
	for _, name := range sorted {
		if err := checkManifestName(name); err != nil {
			return "", err
		}
		sum, err := HashFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("release: hashing %s: %w", name, err)
		}
		fmt.Fprintf(&b, "%s  %s\n", sum, name)
	}
	return b.String(), nil
}

// ParseChecksums parses checksums.txt content into entries. It accepts
// both separators `sha256sum` emits (`  ` for text mode, ` *` for binary
// mode). It rejects: empty content, malformed lines, non-hex or
// short/long digests, duplicate names, and names that escape the
// directory (absolute paths, `..` segments, backslashes, NUL bytes).
func ParseChecksums(content []byte) ([]Entry, error) {
	if len(content) == 0 {
		return nil, errors.New("release: empty checksums manifest")
	}
	if len(content) > MaxChecksumsBytes {
		return nil, fmt.Errorf("release: checksums manifest too large (%d bytes, max %d)", len(content), MaxChecksumsBytes)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	seen := make(map[string]struct{}, len(lines))
	entries := make([]Entry, 0, len(lines))
	for i, line := range lines {
		if line == "" {
			return nil, fmt.Errorf("release: checksums line %d: blank line", i+1)
		}
		sum, name, ok := splitChecksumLine(line)
		if !ok {
			return nil, fmt.Errorf("release: checksums line %d: malformed (want '<64 hex>  <name>')", i+1)
		}
		if err := checkManifestName(name); err != nil {
			return nil, fmt.Errorf("release: checksums line %d: %w", i+1, err)
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("release: checksums line %d: duplicate entry for %q", i+1, name)
		}
		seen[name] = struct{}{}
		entries = append(entries, Entry{SHA256: sum, Name: name})
	}
	return entries, nil
}

// splitChecksumLine splits one manifest line into (digest, name). The
// digest must be exactly 64 lowercase hex characters; the separator is
// two spaces or space-asterisk, as `sha256sum` emits.
func splitChecksumLine(line string) (sum, name string, ok bool) {
	if len(line) < 67 {
		return "", "", false
	}
	sum = line[:64]
	for _, c := range sum {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	sep := line[64:66]
	if sep != "  " && sep != " *" {
		return "", "", false
	}
	name = line[66:]
	if name == "" {
		return "", "", false
	}
	return sum, name, true
}

// checkManifestName rejects names that would let a manifest reach
// outside its directory when joined with it.
func checkManifestName(name string) error {
	if name == "" {
		return errors.New("empty file name")
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("file name %q contains a NUL byte", name)
	}
	if strings.Contains(name, "\\") {
		return fmt.Errorf("file name %q contains a backslash", name)
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("file name %q is absolute", name)
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file name %q escapes its directory", name)
	}
	if clean != name {
		// Reject "./x" and "a//b" spellings: the manifest must name files
		// exactly as they sit on disk, so a tampered manifest cannot hide
		// behind a spelling the verifier normalises away.
		return fmt.Errorf("file name %q is not in canonical form", name)
	}
	return nil
}

// VerifyChecksums verifies every file listed in the manifest against
// its directory: each listed file must exist and its SHA-256 must match.
// It returns nil only when every entry verifies. Files present in the
// directory but absent from the manifest are NOT checked — the manifest
// is an allowlist for the files it names, and the cosign signature over
// the manifest (not this function) is what proves the manifest itself
// came from the project. See docs/RELEASE-SIGNING.md.
func VerifyChecksums(dir string, content []byte) error {
	entries, err := ParseChecksums(content)
	if err != nil {
		return err
	}
	for _, e := range entries {
		actual, err := HashFile(filepath.Join(dir, e.Name))
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("release: %s: listed in checksums but missing from the release directory", e.Name)
			}
			return fmt.Errorf("release: %s: %w", e.Name, err)
		}
		if actual != e.SHA256 {
			return fmt.Errorf("release: %s: SHA-256 MISMATCH (expected %s, got %s) — the file is corrupt or tampered with; do not install it", e.Name, e.SHA256, actual)
		}
	}
	return nil
}

// VerifyChecksumsFile reads the manifest at manifestPath and verifies
// the files it lists under dir (usually the manifest's own directory).
func VerifyChecksumsFile(dir, manifestPath string) (int, error) {
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return 0, fmt.Errorf("release: reading checksums manifest: %w", err)
	}
	if err := VerifyChecksums(dir, content); err != nil {
		return 0, err
	}
	entries, err := ParseChecksums(content)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}
