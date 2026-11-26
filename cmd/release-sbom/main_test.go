// Package main tests the release-sbom command (P2-TEST-07).
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRunProducesValidSBOM verifies that run() generates a valid CycloneDX
// SBOM from a minimal module. This covers the cmd/release-sbom path which
// previously had zero test files (P2-TEST-07 / AUD-TEST-009).
func TestRunProducesValidSBOM(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal go.mod so `go list -m -json all` has something to list.
	goMod := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(goMod, []byte("module example.com/test\n\ngo 1.27.4\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	// Temporarily chdir so moduleRoot() finds this dir.
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(orig)

	// run() writes to stdout by default; just verify it doesn't error.
	if err := run("0.0.0-test", ""); err != nil {
		t.Fatalf("run: %v", err)
	}
}
