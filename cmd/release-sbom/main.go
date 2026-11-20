// Command release-sbom generates a CycloneDX 1.6 SBOM for the Codvian
// Router module graph and writes it to stdout (or --out FILE).
//
// It is the dependency-free local SBOM path: it runs `go list -m -json
// all` and renders the result with internal/release. The release
// pipeline in CI prefers syft (via GoReleaser, richer: it also catalogs
// files inside the archives); this command is the fallback when syft is
// unavailable and what the SBOM smoke test exercises. See
// docs/RELEASE-SIGNING.md and scripts/release-sbom.sh.
//
// Usage:
//
//	go run ./cmd/release-sbom [--version vX.Y.Z] [--out sbom.cyclonedx.json]
//
// --version defaults to 0.0.0-dev: an unreleased tree has no version and
// this tool will not invent one.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/raunakiitp/codvian-router/internal/release"
)

func main() {
	version := flag.String("version", "0.0.0-dev", "version stamped on the root component (default 0.0.0-dev: unreleased)")
	out := flag.String("out", "", "write the SBOM to this file instead of stdout")
	flag.Parse()

	if err := run(*version, *out); err != nil {
		fmt.Fprintf(os.Stderr, "release-sbom: %v\n", err)
		os.Exit(1)
	}
}

func run(version, out string) error {
	// Run from the module root so `go list` sees this module, even when
	// invoked from elsewhere.
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	cmd := exec.Command("go", "list", "-m", "-json", "all")
	cmd.Dir = root
	raw, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list -m -json all: %w (run from the repo root with the Go toolchain on PATH)", err)
	}
	mods, err := release.ParseGoListJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	var rootMod release.Module
	var deps []release.Module
	for _, m := range mods {
		if m.Main {
			rootMod = m
		} else {
			deps = append(deps, m)
		}
	}
	if rootMod.Path == "" {
		return fmt.Errorf("go list output has no main module")
	}
	sbom, err := release.GenerateSBOM(rootMod.Path, version, deps)
	if err != nil {
		return err
	}
	if out == "" {
		_, err = os.Stdout.Write(sbom)
		return err
	}
	if err := os.WriteFile(out, sbom, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", out)
	return nil
}

// moduleRoot walks up from the working directory until it finds go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("could not find go.mod above %s", dir)
		}
		dir = parent
	}
}
