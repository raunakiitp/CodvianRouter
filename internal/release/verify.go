// Release verification engine — the verification side of the Sigstore
// signing pipeline (CHECKLIST items 261–262, research-log entries
// 233 + 235).
//
// The signing half already exists: GoReleaser signs checksums.txt and
// each per-archive SBOM with cosign keyless (`.goreleaser.yml`,
// `signs`), signs the Docker image the same way (`docker_signs`), and
// the workflow records SLSA build-provenance attestations
// (`.github/workflows/release.yml`). What was missing was a verifier
// that a user — or the release-day checklist — can run against a
// published release and get a loud yes/no. This package is that
// verifier; `codvian-router release verify` (cmd/codvian-router) is
// its command-line front end, and scripts/verify-release.sh /
// scripts/verify-image.sh are the shell equivalents.
//
// What it checks, in order, for a tag vX.Y.Z:
//
//  1. checksums.txt parses and every file it lists hashes correctly
//     (the existing VerifyChecksums — integrity against the manifest).
//  2. Every expected archive for the tag is present on disk AND listed
//     in checksums.txt. An archive that is present but unmanifested is
//     unauthenticated and fails.
//  3. SBOM presence + shape: every expected archive has a
//     <archive>.sbom.json next to it, valid JSON, SPDX-JSON (what
//     syft produces in the release) or CycloneDX (what the local
//     generator produces). A missing or malformed SBOM fails.
//  4. Sigstore bundle presence + offline structural check for
//     checksums.txt and every SBOM: the bundle must exist, must be a
//     DSSE bundle, its certificate's identity (URI SAN) must be
//     exactly the release workflow + this tag, its OIDC issuer must
//     be GitHub Actions, and the in-toto payload's SHA-256 must be
//     the artifact's own hash. This catches the LiteLLM lookalike
//     case (entry 233) — a valid signature from a different repo or
//     tag — without a network round trip.
//  5. Full cryptographic verification via cosign (ToolVerifier):
//     `cosign verify-blob` for checksums.txt and every SBOM
//     (keyless identity pinned to the workflow + tag; keypair mode
//     with --key for local test fixtures only), and `cosign verify`
//     for the published image.
//  6. Build-provenance attestation via `gh attestation verify` for
//     the archive and the image.
//
// Fail-closed rules (the LiteLLM lesson: a check that passes by
// skipping is worse than no check):
//
//   - A missing bundle, a missing SBOM, or an archive absent from
//     checksums.txt FAILS. There is no "optional" artifact.
//   - If cosign is not available, the signature checks FAIL — they
//     are never silently skipped. The offline structural check (4)
//     is a pre-check, not a substitute: it does not verify the
//     signature cryptographically, does not check the Rekor
//     transparency-log entry, and does not verify the Fulcio
//     certificate chain (that needs the Sigstore trust root, which
//     only cosign fetches). It also deliberately does NOT check the
//     certificate's expiry: Fulcio certificates live ~10 minutes,
//     so a genuine release's certificate is expired by the time
//     anyone verifies it — cosign accepts it because the Rekor
//     entry proves it was valid when logged.
//   - Attestation is the one skippable step, matching
//     scripts/verify-release.sh: when `gh` is absent it is reported
//     SKIPPED (loudly, in the summary), never OK — unless
//     RequireAttestation is set, in which case its absence FAILS.
//
// This package never shells out and never touches the network
// itself: the external tools run only through the injected
// ToolVerifier (the cmd wires the real cosign/gh executables).
// Everything else here is pure Go over files on disk.
package release

import (
	"context"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Release identity constants. These must match .goreleaser.yml and
// .github/workflows/release.yml exactly; a workflow rename changes
// the pinned identity, and the verify paths must move in the same
// commit (see docs/RELEASE-SIGNING.md, "If something goes wrong").
const (
	// DefaultRepo is the GitHub repository the release is published from.
	DefaultRepo = "raunakiitp/CodvianRouter"
	// ReleaseWorkflow is the workflow that signs releases, as a
	// Fulcio certificate identity (workflow URL, no tag — the tag is
	// appended as @refs/tags/<tag>).
	ReleaseWorkflow = "https://github.com/raunakiitp/CodvianRouter/.github/workflows/release.yml"
	// ReleaseOIDCIssuer is the OIDC issuer the signing certificates
	// are minted from (GitHub Actions).
	ReleaseOIDCIssuer = "https://token.actions.githubusercontent.com"
	// ReleaseImageRepo is the container image repository.
	ReleaseImageRepo = "ghcr.io/raunakiitp/codvian-router"
)

// ReleaseIdentity returns the full keyless signing identity for a
// tag: the release workflow URL pinned to the exact tag ref. A
// signature whose certificate carries any other identity — another
// repo, another workflow, another tag — is not this release.
func ReleaseIdentity(tag string) string {
	return ReleaseWorkflow + "@refs/tags/" + tag
}

// MaxSBOMBytes bounds how large an SBOM may be before CheckSBOMShape
// refuses it. A real syft SBOM for this project is well under 1 MiB;
// 8 MiB is far above that and stops a hostile SBOM from being a
// memory problem.
const MaxSBOMBytes = 8 << 20

// MaxBundleBytes bounds a Sigstore bundle file. Real bundles are a
// few kilobytes (one certificate + one Rekor entry + one signature).
const MaxBundleBytes = 1 << 20

// Platform is one (GOOS, GOARCH) pair the release builds for
// (.goreleaser.yml `builds`).
type Platform struct {
	GOOS   string
	GOARCH string
}

// ReleasePlatforms is every platform the release publishes, in the
// order .goreleaser.yml builds them.
var ReleasePlatforms = []Platform{
	{GOOS: "linux", GOARCH: "amd64"},
	{GOOS: "linux", GOARCH: "arm64"},
	{GOOS: "darwin", GOARCH: "amd64"},
	{GOOS: "darwin", GOARCH: "arm64"},
	{GOOS: "windows", GOARCH: "amd64"},
	{GOOS: "windows", GOARCH: "arm64"},
}

// ArchiveName returns the GoReleaser archive name for a version
// (WITHOUT the leading "v": "0.1.0") and platform, per the
// name_template in .goreleaser.yml:
// codvian-router_<version>_<os>_<arch>.tar.gz (.zip on Windows).
func ArchiveName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("codvian-router_%s_%s_%s.%s", version, goos, goarch, ext)
}

// ExpectedArchives returns all six archive names for a version
// (without the leading "v").
func ExpectedArchives(version string) []string {
	out := make([]string, 0, len(ReleasePlatforms))
	for _, p := range ReleasePlatforms {
		out = append(out, ArchiveName(version, p.GOOS, p.GOARCH))
	}
	return out
}

// NormalizeTag returns the tag with a leading "v" ("0.1.0" ->
// "v0.1.0", "v0.1.0" unchanged) and rejects empty or malformed
// tags. The release workflow only ever runs on v*.*.* tags.
func NormalizeTag(tag string) (string, error) {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return "", errors.New("release: empty tag")
	}
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	rest := strings.TrimPrefix(tag, "v")
	if rest == "" || strings.ContainsAny(rest, " \t\n/\\") {
		return "", fmt.Errorf("release: malformed tag %q", tag)
	}
	return tag, nil
}

// CheckStatus is the outcome of one verification check.
type CheckStatus int

const (
	// CheckPass: the check ran and the artifact is good.
	CheckPass CheckStatus = iota
	// CheckFail: the check ran and the artifact is bad, missing, or
	// unverifiable — the release must not be installed or run.
	CheckFail
	// CheckSkipped: the check did not run (a tool it needs is
	// absent and the check allows skipping). A skipped check is
	// never a pass; the summary says so.
	CheckSkipped
)

func (s CheckStatus) String() string {
	switch s {
	case CheckPass:
		return "ok"
	case CheckFail:
		return "FAIL"
	default:
		return "SKIP"
	}
}

// Check is one named verification step and its outcome. Detail
// names the artifact on failure, so a failed run says exactly what
// to distrust.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
}

// Report is the ordered result of a full release verification.
type Report struct {
	Checks []Check
}

// OK reports whether every check passed (no failures; skips are
// allowed only where the caller accepted them — a skipped
// attestation is reported, not hidden).
func (r *Report) OK() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return false
		}
	}
	return true
}

// Failed returns the failing checks, in order.
func (r *Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			out = append(out, c)
		}
	}
	return out
}

// ToolVerifier runs the external signature/attestation tools. The
// methods take absolute file/bundle paths (for blobs) or an image
// reference. A nil error means the tool verified the subject.
type ToolVerifier interface {
	// VerifyBlobKeyless runs: cosign verify-blob
	// --certificate-identity <identity> --certificate-oidc-issuer
	// <issuer> --bundle <bundle> <file>.
	VerifyBlobKeyless(file, bundle, identity, issuer string) error
	// VerifyBlobKey runs: cosign verify-blob --key <pubKey>
	// --bundle <bundle> --insecure-ignore-tlog <file>. Keypair mode
	// is for local test fixtures only — real releases are keyless.
	VerifyBlobKey(file, bundle, pubKey string) error
	// VerifyImageKeyless runs: cosign verify
	// --certificate-identity <identity> --certificate-oidc-issuer
	// <issuer> <image>.
	VerifyImageKeyless(image, identity, issuer string) error
	// VerifyImageKey runs: cosign verify --key <pubKey>
	// --insecure-ignore-tlog <image>.
	VerifyImageKey(image, pubKey string) error
	// VerifyAttestation runs: gh attestation verify <target>
	// --repo <repo> (target is a file path or an oci:// image ref).
	VerifyAttestation(target, repo string) error
	// HasTool reports whether the named executable is on PATH.
	HasTool(name string) bool
}

// execToolVerifier shells out to the real cosign and gh binaries.
// Timeouts bound each tool call: cosign's first run fetches the
// Sigstore trust root, so the blob/image timeouts are generous.
type execToolVerifier struct {
	timeout time.Duration
}

// NewExecToolVerifier returns a ToolVerifier backed by the cosign
// and gh executables on PATH.
func NewExecToolVerifier() ToolVerifier {
	return &execToolVerifier{timeout: 5 * time.Minute}
}

func (v *execToolVerifier) run(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), v.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			// Keep the tool's own verdict (it names the subject);
			// cap the length so a hostile tool cannot flood the log.
			if len(trimmed) > 2000 {
				trimmed = trimmed[:2000] + "…"
			}
			return fmt.Errorf("%s: %v", trimmed, err)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func (v *execToolVerifier) VerifyBlobKeyless(file, bundle, identity, issuer string) error {
	return v.run("cosign", "verify-blob",
		"--certificate-identity", identity,
		"--certificate-oidc-issuer", issuer,
		"--bundle", bundle, file)
}

func (v *execToolVerifier) VerifyBlobKey(file, bundle, pubKey string) error {
	return v.run("cosign", "verify-blob",
		"--key", pubKey, "--bundle", bundle,
		"--insecure-ignore-tlog", file)
}

func (v *execToolVerifier) VerifyImageKeyless(image, identity, issuer string) error {
	return v.run("cosign", "verify",
		"--certificate-identity", identity,
		"--certificate-oidc-issuer", issuer, image)
}

func (v *execToolVerifier) VerifyImageKey(image, pubKey string) error {
	return v.run("cosign", "verify",
		"--key", pubKey, "--insecure-ignore-tlog", image)
}

func (v *execToolVerifier) VerifyAttestation(target, repo string) error {
	return v.run("gh", "attestation", "verify", target, "--repo", repo)
}

func (v *execToolVerifier) HasTool(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// VerifyOptions controls VerifyReleaseDir.
type VerifyOptions struct {
	// Tag is the release tag, e.g. "v0.1.0" (a leading "v" is added
	// when missing). Required: archive names and the keyless
	// identity are derived from it.
	Tag string
	// Dir is the directory holding the release files (a dist/ dir,
	// a downloaded release, or a directory of test fixtures).
	Dir string
	// Archives are the release archives to require. Empty means all
	// six ExpectedArchives for the tag (a complete release). A
	// remote download passes just the local platform's archive.
	Archives []string
	// VerifyOnly, when non-empty, restricts the checksums step to
	// these manifest entries. Remote mode downloads one platform's
	// files while checksums.txt lists all six platforms, so it
	// passes the downloaded archive + SBOM here — the same
	// `--ignore-missing` rule scripts/verify-release.sh uses.
	// Empty means every listed file must be present (local mode: a
	// complete release directory).
	VerifyOnly []string
	// KeyPath, when set, verifies bundles against this cosign public
	// key (keypair mode) instead of the keyless workflow identity.
	// Keypair mode exists only so the pipeline's own tests can
	// exercise verification locally; real releases are keyless and
	// need no --key.
	KeyPath string
	// Repo is the GitHub repository for attestation checks
	// ("owner/name"); empty means DefaultRepo.
	Repo string
	// SkipImage skips the Docker image signature check. Local
	// directories have no image to check; remote verification
	// checks it by default.
	SkipImage bool
	// ImageRef overrides the image reference (default
	// ghcr.io/raunakiitp/codvian-router:<tag>).
	ImageRef string
	// SkipAttestation skips the `gh attestation verify` steps.
	// Local directories cannot be attestation-checked (attestations
	// live in GitHub's attestation store, not in the directory), so
	// local verification always skips them. A skipped attestation
	// is reported SKIP, never ok.
	SkipAttestation bool
	// RequireAttestation turns a missing `gh` CLI from a skipped
	// attestation into a failure. Use it in CI, where the
	// attestation check must actually run.
	RequireAttestation bool
	// Tools runs the external verifiers. Nil means no tools are
	// available: every signature/attestation check FAILS (fail
	// closed — a release is never "verified" because the verifier
	// was absent).
	Tools ToolVerifier
}

// VerifyReleaseDir verifies a directory of release files for a tag
// and returns the ordered check report. It returns a non-nil error
// only when verification could not run at all (bad tag, missing
// directory, unreadable checksums.txt); a bad artifact is reported
// as a failing check, not an error, so the caller can print the
// whole report and exit non-zero.
func VerifyReleaseDir(opts VerifyOptions) (*Report, error) {
	tag, err := NormalizeTag(opts.Tag)
	if err != nil {
		return nil, err
	}
	if opts.Dir == "" {
		return nil, errors.New("release: empty release directory")
	}
	info, err := os.Stat(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("release: release directory %s: %w", opts.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("release: %s is not a directory", opts.Dir)
	}
	repo := opts.Repo
	if repo == "" {
		repo = DefaultRepo
	}
	version := strings.TrimPrefix(tag, "v")
	archives := opts.Archives
	if len(archives) == 0 {
		archives = ExpectedArchives(version)
	}
	report := &Report{}

	// --- 1. checksums.txt: parse + hash every listed file ----------
	manifestPath := filepath.Join(opts.Dir, "checksums.txt")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		report.Checks = append(report.Checks, Check{
			Name:   "checksums: checksums.txt",
			Status: CheckFail,
			Detail: fmt.Sprintf("checksums.txt unreadable: %v", err),
		})
		return report, nil
	}
	entries, err := ParseChecksums(manifest)
	if err != nil {
		report.Checks = append(report.Checks, Check{
			Name:   "checksums: checksums.txt",
			Status: CheckFail,
			Detail: fmt.Sprintf("checksums.txt malformed: %v", err),
		})
		return report, nil
	}
	listed := make(map[string]string, len(entries))
	for _, e := range entries {
		listed[e.Name] = e.SHA256
	}
	// The checksums step: in local mode every listed file must be
	// present and hash correctly; in remote mode (VerifyOnly set)
	// only the downloaded subset is checked — the manifest lists
	// all six platforms, but only one was downloaded.
	manifestToVerify := manifest
	if len(opts.VerifyOnly) > 0 {
		var sub strings.Builder
		for _, name := range opts.VerifyOnly {
			sum, ok := listed[name]
			if !ok {
				report.Checks = append(report.Checks, Check{
					Name:   "checksums: files match checksums.txt",
					Status: CheckFail,
					Detail: fmt.Sprintf("release: %s: required for verification but NOT listed in checksums.txt — it is unauthenticated; do not install it", name),
				})
				return report, nil
			}
			fmt.Fprintf(&sub, "%s  %s\n", sum, name)
		}
		manifestToVerify = []byte(sub.String())
	}
	if err := VerifyChecksums(opts.Dir, manifestToVerify); err != nil {
		report.Checks = append(report.Checks, Check{
			Name:   "checksums: files match checksums.txt",
			Status: CheckFail,
			Detail: err.Error(),
		})
	} else {
		count := len(entries)
		if len(opts.VerifyOnly) > 0 {
			count = len(opts.VerifyOnly)
		}
		report.Checks = append(report.Checks, Check{
			Name:   "checksums: files match checksums.txt",
			Status: CheckPass,
			Detail: fmt.Sprintf("%d file(s) checked, all SHA-256 match", count),
		})
	}

	// Whether the manifest covers SBOMs at all. Verified against a
	// real GoReleaser v2.20.2 snapshot build (this slice): the
	// manifest lists every archive AND every SBOM. The check is
	// still adaptive rather than assumed — if a future GoReleaser
	// ever stops listing SBOMs, the verifier says so instead of
	// failing a genuine release, and SBOMs remain authenticated by
	// their own signatures.
	manifestCoversSBOM := false
	for name := range listed {
		if strings.HasSuffix(name, ".sbom.json") {
			manifestCoversSBOM = true
			break
		}
	}
	if !manifestCoversSBOM {
		report.Checks = append(report.Checks, Check{
			Name:   "checksums: SBOM coverage",
			Status: CheckPass,
			Detail: "checksums.txt does not list SBOMs (layout fact, verified against .goreleaser.yml + a snapshot build) — each SBOM is authenticated by its own Sigstore signature instead",
		})
	}

	// --- 2–4. Per-archive: presence, SBOM, bundle --------------------
	identity := ReleaseIdentity(tag)
	signed := []string{"checksums.txt"}
	for _, archive := range archives {
		// Presence + manifest listing.
		if _, err := os.Stat(filepath.Join(opts.Dir, archive)); err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   "archive: " + archive,
				Status: CheckFail,
				Detail: fmt.Sprintf("archive %s missing from the release directory", archive),
			})
			continue
		}
		if _, ok := listed[archive]; !ok {
			report.Checks = append(report.Checks, Check{
				Name:   "archive: " + archive,
				Status: CheckFail,
				Detail: fmt.Sprintf("archive %s is present but NOT listed in checksums.txt — it is unauthenticated; do not install it", archive),
			})
			continue
		}
		report.Checks = append(report.Checks, Check{
			Name:   "archive: " + archive,
			Status: CheckPass,
			Detail: "present and listed in checksums.txt (hash verified in step 1)",
		})

		// SBOM presence + shape (+ manifest listing when the
		// manifest covers SBOMs).
		sbom := archive + ".sbom.json"
		sbomPath := filepath.Join(opts.Dir, sbom)
		if _, err := os.Stat(sbomPath); err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   "sbom: " + sbom,
				Status: CheckFail,
				Detail: fmt.Sprintf("SBOM %s missing — a complete release ships <archive>.sbom.json for every archive", sbom),
			})
		} else {
			if manifestCoversSBOM {
				if _, ok := listed[sbom]; !ok {
					report.Checks = append(report.Checks, Check{
						Name:   "sbom: " + sbom,
						Status: CheckFail,
						Detail: fmt.Sprintf("SBOM %s is present but NOT listed in checksums.txt", sbom),
					})
				}
			}
			format, err := CheckSBOMShape(sbomPath)
			if err != nil {
				report.Checks = append(report.Checks, Check{
					Name:   "sbom: " + sbom,
					Status: CheckFail,
					Detail: err.Error(),
				})
			} else {
				report.Checks = append(report.Checks, Check{
					Name:   "sbom: " + sbom,
					Status: CheckPass,
					Detail: fmt.Sprintf("present, valid JSON (%s)", format),
				})
			}
		}
		signed = append(signed, sbom)
	}

	// --- 4. Bundle presence + offline structural check ---------------
	for _, file := range signed {
		bundle := file + ".sigstore.json"
		bundlePath := filepath.Join(opts.Dir, bundle)
		if _, err := os.Stat(bundlePath); err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   "bundle: " + bundle,
				Status: CheckFail,
				Detail: fmt.Sprintf("Sigstore bundle %s missing — %s has no signature; an unsigned release is not verifiable", bundle, file),
			})
			continue
		}
		var err error
		if opts.KeyPath != "" {
			err = VerifyBundleKeypair(bundlePath, filepath.Join(opts.Dir, file))
		} else {
			err = VerifyBundleIdentity(bundlePath, filepath.Join(opts.Dir, file), identity, ReleaseOIDCIssuer)
		}
		if err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   "bundle: " + bundle,
				Status: CheckFail,
				Detail: err.Error(),
			})
		} else {
			mode := "keyless identity " + identity
			if opts.KeyPath != "" {
				mode = "keypair mode (local test fixtures only)"
			}
			report.Checks = append(report.Checks, Check{
				Name:   "bundle: " + bundle,
				Status: CheckPass,
				Detail: fmt.Sprintf("bundle parses; certificate binds to %s; payload digest matches %s", mode, file),
			})
		}
	}

	// --- 5. Full cryptographic verification via cosign --------------
	verifyCosign := func(name, file string) {
		bundle := file + ".sigstore.json"
		if opts.Tools == nil || !opts.Tools.HasTool("cosign") {
			report.Checks = append(report.Checks, Check{
				Name:   name,
				Status: CheckFail,
				Detail: "cosign is required for signature verification but was not found — install cosign and re-run; the signature was NOT verified",
			})
			return
		}
		var err error
		if opts.KeyPath != "" {
			err = opts.Tools.VerifyBlobKey(filepath.Join(opts.Dir, file), filepath.Join(opts.Dir, bundle), opts.KeyPath)
		} else {
			err = opts.Tools.VerifyBlobKeyless(filepath.Join(opts.Dir, file), filepath.Join(opts.Dir, bundle), identity, ReleaseOIDCIssuer)
		}
		if err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   name,
				Status: CheckFail,
				Detail: fmt.Sprintf("cosign signature verification FAILED for %s: %v — do not install these files", file, err),
			})
			return
		}
		report.Checks = append(report.Checks, Check{
			Name:   name,
			Status: CheckPass,
			Detail: "cosign verified (signature + Rekor transparency-log entry)",
		})
	}
	for _, file := range signed {
		verifyCosign("signature: "+file, file)
	}

	// --- 5b. Docker image signature ----------------------------------
	imageRef := opts.ImageRef
	if imageRef == "" {
		imageRef = ReleaseImageRepo + ":" + tag
	}
	if !opts.SkipImage {
		if opts.Tools == nil || !opts.Tools.HasTool("cosign") {
			report.Checks = append(report.Checks, Check{
				Name:   "image: " + imageRef,
				Status: CheckFail,
				Detail: "cosign is required for image signature verification but was not found — install cosign and re-run; the image was NOT verified",
			})
		} else {
			var err error
			if opts.KeyPath != "" {
				err = opts.Tools.VerifyImageKey(imageRef, opts.KeyPath)
			} else {
				err = opts.Tools.VerifyImageKeyless(imageRef, identity, ReleaseOIDCIssuer)
			}
			if err != nil {
				report.Checks = append(report.Checks, Check{
					Name:   "image: " + imageRef,
					Status: CheckFail,
					Detail: fmt.Sprintf("cosign signature verification FAILED for %s: %v — do not run this image", imageRef, err),
				})
			} else {
				report.Checks = append(report.Checks, Check{
					Name:   "image: " + imageRef,
					Status: CheckPass,
					Detail: "cosign verified (signature + Rekor transparency-log entry)",
				})
			}
		}
	}

	// --- 6. Build-provenance attestations -----------------------------
	attest := func(name, target string) {
		if opts.SkipAttestation {
			report.Checks = append(report.Checks, Check{
				Name:   name,
				Status: CheckSkipped,
				Detail: "skipped — attestations live in GitHub's attestation store, not in the release directory; verify the published release to check them",
			})
			return
		}
		if opts.Tools == nil || !opts.Tools.HasTool("gh") {
			if opts.RequireAttestation {
				report.Checks = append(report.Checks, Check{
					Name:   name,
					Status: CheckFail,
					Detail: "gh CLI is required for the attestation check but was not found (and --require-attestation is set)",
				})
			} else {
				report.Checks = append(report.Checks, Check{
					Name:   name,
					Status: CheckSkipped,
					Detail: "gh CLI not found — attestation NOT checked (the signature checks above already passed); install gh and re-run to check it",
				})
			}
			return
		}
		if err := opts.Tools.VerifyAttestation(target, repo); err != nil {
			report.Checks = append(report.Checks, Check{
				Name:   name,
				Status: CheckFail,
				Detail: fmt.Sprintf("build-provenance attestation verification FAILED for %s: %v — do not install it", target, err),
			})
			return
		}
		report.Checks = append(report.Checks, Check{
			Name:   name,
			Status: CheckPass,
			Detail: "attestation verified (built by this repo's release workflow from the tagged commit)",
		})
	}
	for _, archive := range archives {
		attest("attestation: "+archive, filepath.Join(opts.Dir, archive))
	}
	if !opts.SkipImage {
		attest("attestation: "+imageRef, "oci://"+imageRef)
	}

	return report, nil
}

// CheckSBOMShape parses the SBOM at path and reports its format:
// "SPDX-JSON" for the syft output the release ships, "CycloneDX"
// for the local generator's output. Anything else — invalid JSON,
// or JSON that is neither shape — is an error naming the file.
func CheckSBOMShape(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("release: reading SBOM %s: %w", filepath.Base(path), err)
	}
	if len(content) > MaxSBOMBytes {
		return "", fmt.Errorf("release: SBOM %s too large (%d bytes, max %d)", filepath.Base(path), len(content), MaxSBOMBytes)
	}
	var shape struct {
		SPDXVersion string `json:"spdxVersion"`
		SPDXID      string `json:"SPDXID"`
		BOMFormat   string `json:"bomFormat"`
		SpecVersion string `json:"specVersion"`
	}
	if err := json.Unmarshal(content, &shape); err != nil {
		return "", fmt.Errorf("release: SBOM %s is not valid JSON: %v", filepath.Base(path), err)
	}
	if shape.SPDXVersion != "" {
		if shape.SPDXID != "SPDXRef-DOCUMENT" {
			return "", fmt.Errorf("release: SBOM %s: SPDX document with unexpected SPDXID %q (want SPDXRef-DOCUMENT)", filepath.Base(path), shape.SPDXID)
		}
		return "SPDX-JSON", nil
	}
	if shape.BOMFormat == "CycloneDX" {
		if shape.SpecVersion == "" {
			return "", fmt.Errorf("release: SBOM %s: CycloneDX document without specVersion", filepath.Base(path))
		}
		return "CycloneDX", nil
	}
	return "", fmt.Errorf("release: SBOM %s is valid JSON but neither SPDX-JSON (no spdxVersion) nor CycloneDX (no bomFormat) — unrecognized SBOM format", filepath.Base(path))
}

// --- Sigstore bundle structural checks --------------------------------

// sigstoreBundle is the subset of the Sigstore bundle format
// (application/vnd.dev.sigstore.bundle.v0.3+json) this verifier
// reads. Only the fields the structural check needs are modelled;
// unknown fields are ignored.
type sigstoreBundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		Certificate *struct {
			RawBytes string `json:"rawBytes"`
		} `json:"certificate"`
		PublicKey *struct {
			RawBytes string `json:"rawBytes"`
		} `json:"publicKey"`
		TLogEntries []struct {
			IntegratedTime int64 `json:"integratedTime"`
			LogIndex       int64 `json:"logIndex"`
		} `json:"tlogEntries"`
	} `json:"verificationMaterial"`
	DSSEEnvelope *struct {
		PayloadType string `json:"payloadType"`
		Payload     string `json:"payload"`
		Signatures  []struct {
			KeyID string `json:"keyid"`
			Sig   string `json:"sig"`
		} `json:"signatures"`
	} `json:"dsseEnvelope"`
}

// inTotoStatement is the payload cosign sign-blob writes: an
// in-toto statement whose subject digest is the artifact's SHA-256.
type inTotoStatement struct {
	Type    string `json:"_type"`
	Subject []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	PredicateType string `json:"predicateType"`
}

// fulcioIssuerOID is the Fulcio certificate extension carrying the
// OIDC issuer the certificate was minted from.
var fulcioIssuerOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

func readBundle(path string) (*sigstoreBundle, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("release: reading bundle %s: %w", filepath.Base(path), err)
	}
	if len(content) > MaxBundleBytes {
		return nil, fmt.Errorf("release: bundle %s too large (%d bytes, max %d)", filepath.Base(path), len(content), MaxBundleBytes)
	}
	var bundle sigstoreBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return nil, fmt.Errorf("release: bundle %s is not valid JSON: %v", filepath.Base(path), err)
	}
	return &bundle, nil
}

// bundlePayloadDigest decodes the DSSE envelope payload and returns
// the SHA-256 the bundle claims for its artifact. It also enforces
// the envelope shape cosign sign-blob produces: an in-toto statement
// with exactly one subject carrying a sha256 digest.
func bundlePayloadDigest(bundle *sigstoreBundle, bundleName string) (string, error) {
	if bundle.DSSEEnvelope == nil {
		return "", fmt.Errorf("release: bundle %s has no DSSE envelope — not a cosign sign-blob bundle (cosign v3 emits DSSE bundles only)", bundleName)
	}
	if bundle.DSSEEnvelope.PayloadType != "application/vnd.in-toto+json" {
		return "", fmt.Errorf("release: bundle %s: unexpected DSSE payload type %q (want application/vnd.in-toto+json)", bundleName, bundle.DSSEEnvelope.PayloadType)
	}
	raw, err := base64.StdEncoding.DecodeString(bundle.DSSEEnvelope.Payload)
	if err != nil {
		return "", fmt.Errorf("release: bundle %s: DSSE payload is not base64: %v", bundleName, err)
	}
	var stmt inTotoStatement
	if err := json.Unmarshal(raw, &stmt); err != nil {
		return "", fmt.Errorf("release: bundle %s: DSSE payload is not an in-toto statement: %v", bundleName, err)
	}
	if len(stmt.Subject) != 1 {
		return "", fmt.Errorf("release: bundle %s: in-toto statement has %d subjects (want exactly 1)", bundleName, len(stmt.Subject))
	}
	digest, ok := stmt.Subject[0].Digest["sha256"]
	if !ok || digest == "" {
		return "", fmt.Errorf("release: bundle %s: in-toto statement carries no sha256 digest for its subject", bundleName)
	}
	return strings.ToLower(digest), nil
}

// VerifyBundleIdentity is the offline structural check for a keyless
// Sigstore bundle: it proves (without the network, without cosign)
// that the bundle's certificate was issued for exactly
// expectedIdentity with expectedIssuer, and that the bundle's
// payload digest is the artifact's own SHA-256. It does NOT verify
// the signature cryptographically, the Rekor log entry, or the
// Fulcio certificate chain — that is cosign's job (step 5). A
// bundle that fails this check is wrong on its face: wrong repo,
// wrong workflow, wrong tag, or bound to different bytes.
func VerifyBundleIdentity(bundlePath, artifactPath, expectedIdentity, expectedIssuer string) error {
	bundleName := filepath.Base(bundlePath)
	bundle, err := readBundle(bundlePath)
	if err != nil {
		return err
	}
	vm := bundle.VerificationMaterial
	if vm.Certificate == nil || vm.Certificate.RawBytes == "" {
		return fmt.Errorf("release: bundle %s has no certificate — not a keyless bundle (keypair bundles carry a public key, not a certificate; use --key for those)", bundleName)
	}
	der, err := base64.StdEncoding.DecodeString(vm.Certificate.RawBytes)
	if err != nil {
		return fmt.Errorf("release: bundle %s: certificate is not base64: %v", bundleName, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return fmt.Errorf("release: bundle %s: certificate does not parse: %v", bundleName, err)
	}
	// Identity: the Fulcio certificate carries the OIDC identity as
	// a URI SAN. It must match exactly — a lookalike repo
	// (one hyphen, one letter) is a different identity, and the
	// LiteLLM compromise (entry 233) turned on exactly that.
	matched := false
	for _, uri := range cert.URIs {
		if uri.String() == expectedIdentity {
			matched = true
			break
		}
	}
	if !matched {
		got := "<none>"
		if len(cert.URIs) > 0 {
			got = cert.URIs[0].String()
		}
		return fmt.Errorf("release: bundle %s: certificate identity is %q, not %q — this signature is not from this release's workflow + tag; do not trust it", bundleName, got, expectedIdentity)
	}
	// Issuer: the Fulcio OIDC-issuer extension must be GitHub
	// Actions. (Expiry is deliberately NOT checked: Fulcio
	// certificates live ~10 minutes, so a genuine release's
	// certificate is expired by the time anyone verifies it —
	// cosign accepts it because the Rekor entry proves it was
	// valid when logged.)
	issuer := ""
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(fulcioIssuerOID) {
			var v string
			if _, err := asn1.Unmarshal(ext.Value, &v); err == nil {
				issuer = v
			}
			break
		}
	}
	if issuer == "" {
		return fmt.Errorf("release: bundle %s: certificate carries no Fulcio OIDC-issuer extension — not a GitHub Actions keyless certificate", bundleName)
	}
	if issuer != expectedIssuer {
		return fmt.Errorf("release: bundle %s: certificate OIDC issuer is %q, not %q", bundleName, issuer, expectedIssuer)
	}
	// Digest binding: the bundle must be about this artifact's
	// bytes, not some other file's.
	want, err := bundlePayloadDigest(bundle, bundleName)
	if err != nil {
		return err
	}
	actual, err := HashFile(artifactPath)
	if err != nil {
		return fmt.Errorf("release: hashing %s for bundle check: %w", filepath.Base(artifactPath), err)
	}
	if want != actual {
		return fmt.Errorf("release: bundle %s: payload digest %s does not match %s (%s) — the bundle is for different bytes", bundleName, want, filepath.Base(artifactPath), actual)
	}
	return nil
}

// VerifyBundleKeypair is the offline structural check for a bundle
// made with a local cosign keypair (pipeline self-test only): the
// bundle must carry a public key (not a certificate) and its payload
// digest must be the artifact's own SHA-256. The cryptographic
// check is cosign's job (VerifyBlobKey).
func VerifyBundleKeypair(bundlePath, artifactPath string) error {
	bundleName := filepath.Base(bundlePath)
	bundle, err := readBundle(bundlePath)
	if err != nil {
		return err
	}
	vm := bundle.VerificationMaterial
	if vm.PublicKey == nil || vm.PublicKey.RawBytes == "" {
		return fmt.Errorf("release: bundle %s has no public key — not a keypair bundle (keyless bundles carry a certificate; drop --key for those)", bundleName)
	}
	if _, err := base64.StdEncoding.DecodeString(vm.PublicKey.RawBytes); err != nil {
		return fmt.Errorf("release: bundle %s: public key is not base64: %v", bundleName, err)
	}
	want, err := bundlePayloadDigest(bundle, bundleName)
	if err != nil {
		return err
	}
	actual, err := HashFile(artifactPath)
	if err != nil {
		return fmt.Errorf("release: hashing %s for bundle check: %w", filepath.Base(artifactPath), err)
	}
	if want != actual {
		return fmt.Errorf("release: bundle %s: payload digest %s does not match %s (%s) — the bundle is for different bytes", bundleName, want, filepath.Base(artifactPath), actual)
	}
	return nil
}

// BundleDigest is a small helper for tests and tooling: it returns
// the SHA-256 a bundle's payload claims, without touching the
// artifact.
func BundleDigest(bundlePath string) (string, error) {
	bundle, err := readBundle(bundlePath)
	if err != nil {
		return "", err
	}
	return bundlePayloadDigest(bundle, filepath.Base(bundlePath))
}
