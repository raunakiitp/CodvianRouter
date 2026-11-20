package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/release"
)

// TestBlockB9PackagingCLIAndReleaseGate validates the end-to-end integration
// of Block B9 (Area 13: Packaging, CLI, and Release Safety Proofs).
func TestBlockB9PackagingCLIAndReleaseGate(t *testing.T) {
	// ---------------------------------------------------------
	// 1. CLI Subcommands & Flags Execution
	// ---------------------------------------------------------
	t.Run("CLISubcommandsAndFlags", func(t *testing.T) {
		// 1.1 Fast-path version flags: version, --version, -v
		for _, arg := range []string{"version", "--version", "-v"} {
			cmd := exec.Command("go", "run", "./cmd/codvian-router", arg)
			cmd.Dir = filepath.Join("..", "..")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go run ./cmd/codvian-router %s failed: %v\nOutput: %s", arg, err, string(out))
			}
			outStr := strings.TrimSpace(string(out))
			if !strings.Contains(outStr, "codvian-router v0.1.0") {
				t.Errorf("arg %q output = %q, want 'codvian-router v0.1.0'", arg, outStr)
			}
		}

		// 1.2 Keygen subcommand generates valid formatted gateway key
		cmd := exec.Command("go", "run", "./cmd/codvian-router", "keygen")
		cmd.Dir = filepath.Join("..", "..")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go run ./cmd/codvian-router keygen failed: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) == 0 || !strings.HasPrefix(strings.TrimSpace(lines[0]), "crk_") {
			t.Errorf("keygen output first line = %q, want 'crk_...' key prefix", lines[0])
		}

		// 1.3 Help subcommand outputs usage instructions
		cmd = exec.Command("go", "run", "./cmd/codvian-router", "help")
		cmd.Dir = filepath.Join("..", "..")
		out, err = cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go run ./cmd/codvian-router help failed: %v", err)
		}
		helpText := string(out)
		if !strings.Contains(helpText, "Usage:") || !strings.Contains(helpText, "serve") {
			t.Errorf("help output missing standard usage headers: %s", helpText)
		}

		// 1.4 Name Rule check across CLI output
		prohibited := []string{"omni" + "route"}
		for _, p := range prohibited {
			if strings.Contains(strings.ToLower(helpText), p) {
				t.Fatalf("CRITICAL: Prohibited brand name %q found in CLI help text", p)
			}
		}
	})

	// ---------------------------------------------------------
	// 2. Release Safety Proofs: Checksums, Tamper Detection & SBOM
	// ---------------------------------------------------------
	t.Run("ReleaseSafetyProofsAndSBOM", func(t *testing.T) {
		tmpDir := t.TempDir()

		// 2.1 Create sample release artifacts
		fileA := filepath.Join(tmpDir, "artifact-a.tar.gz")
		fileB := filepath.Join(tmpDir, "artifact-b.tar.gz")
		if err := os.WriteFile(fileA, []byte("release payload binary a"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fileB, []byte("release payload binary b"), 0644); err != nil {
			t.Fatal(err)
		}

		// 2.2 Generate checksums
		manifestStr, err := release.GenerateChecksums(tmpDir, []string{"artifact-a.tar.gz", "artifact-b.tar.gz"})
		if err != nil {
			t.Fatalf("GenerateChecksums failed: %v", err)
		}
		manifestBytes := []byte(manifestStr)

		manifestPath := filepath.Join(tmpDir, "checksums.txt")
		if err := os.WriteFile(manifestPath, manifestBytes, 0644); err != nil {
			t.Fatal(err)
		}

		// 2.3 Verify unmodified release artifacts
		if err := release.VerifyChecksums(tmpDir, manifestBytes); err != nil {
			t.Fatalf("VerifyChecksums failed on valid artifacts: %v", err)
		}

		// 2.4 Tamper detection: modifying 1 byte causes verification failure
		if err := os.WriteFile(fileA, []byte("tampered payload binary a"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := release.VerifyChecksums(tmpDir, manifestBytes); err == nil {
			t.Fatalf("VerifyChecksums passed on tampered artifact, want error")
		}

		// 2.5 CycloneDX 1.6 SBOM generation
		deps := []release.Module{
			{Path: "github.com/dustin/go-humanize", Version: "v1.0.1"},
			{Path: "github.com/google/uuid", Version: "v1.6.0"},
		}

		sbomBytes, err := release.GenerateSBOM("github.com/raunakiitp/codvian-router", "v0.1.0", deps)
		if err != nil {
			t.Fatalf("GenerateSBOM failed: %v", err)
		}

		var sbom map[string]any
		if err := json.Unmarshal(sbomBytes, &sbom); err != nil {
			t.Fatalf("decode SBOM json: %v", err)
		}
		if sbom["bomFormat"] != "CycloneDX" {
			t.Errorf("bomFormat = %v, want 'CycloneDX'", sbom["bomFormat"])
		}
		if sbom["specVersion"] != "1.6" {
			t.Errorf("specVersion = %v, want '1.6'", sbom["specVersion"])
		}
	})
}
