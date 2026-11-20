package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/raunakiitp/codvian-router/internal/release"
)

// runVerifyRelease verifies the files in a release directory against
// its checksums.txt manifest:
//
//	codvian-router verify-release [DIR]
//
// DIR defaults to the current directory. It checks that every file
// listed in checksums.txt exists in DIR and its SHA-256 matches, and
// exits non-zero naming the first file that does not. It proves
// integrity against the manifest — it does NOT prove the manifest came
// from the project. That is the cosign signature's job; run
// scripts/verify-release.sh (or the cosign commands in
// docs/RELEASE-SIGNING.md) for the full signature + checksums + SBOM
// verification before installing a release.
func runVerifyRelease(positional []string) int {
	dir := "."
	if len(positional) > 1 {
		dir = positional[1]
	}
	manifest := filepath.Join(dir, "checksums.txt")
	n, err := release.VerifyChecksumsFile(dir, manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: verify-release: %v\n", err)
		return 1
	}
	fmt.Printf("verified: %d file(s) in %s match checksums.txt\n", n, dir)
	return 0
}
