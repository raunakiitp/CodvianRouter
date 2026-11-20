// `codvian-router release` — release verification (CHECKLIST items
// 261–262, research-log entries 233 + 235).
//
// The signing half of the pipeline (GoReleaser + cosign keyless +
// SLSA attestations) already exists; this is the verification side
// as a single command:
//
//	codvian-router release verify v0.1.0
//	    Download the published release for this machine from GitHub
//	    and verify it: cosign keyless signature over checksums.txt
//	    → SHA-256 of the archive → cosign signature + JSON validity
//	    of the SBOM → build-provenance attestation (when `gh` is
//	    installed) → cosign signature + attestation of the published
//	    Docker image. Exits 0 and prints VERIFIED only when every
//	    applicable check passed; on ANY failure it exits non-zero
//	    naming the artifact. Do not install or run the files.
//
//	codvian-router release verify v0.1.0 --dir DIR
//	    Verify files already on disk in DIR (a dist/ directory, a
//	    downloaded release, or test fixtures) — no download. All six
//	    platform archives are expected, exactly as a complete
//	    release ships them. Attestations are skipped in local mode:
//	    they live in GitHub's attestation store, not in the
//	    directory. A snapshot dist/ (unsigned by design) FAILS
//	    here, correctly — it is not a release.
//
//	codvian-router release verify v0.1.0 --dir DIR --key cosign.pub
//	    Keypair mode: verify bundles against a local cosign public
//	    key instead of the keyless workflow identity. This is how
//	    the pipeline's own tests exercise verification locally;
//	    real releases are keyless and need no --key.
//
// The engine lives in internal/release (VerifyReleaseDir); this file
// is only argument parsing, the GitHub download, and the report
// printing. It never creates a tag or a release, and it never
// publishes anything — verification only.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/release"
)

// maxDownloadBytes bounds a single downloaded release file. Release
// archives are tens of megabytes; 256 MiB is far above that and
// stops a hostile or corrupted download from filling the disk.
const maxDownloadBytes = 256 << 20

// releaseVerifyConfig is the parsed form of `release verify` flags.
type releaseVerifyConfig struct {
	tag                string
	dir                string // local mode when non-empty
	repo               string
	keyPath            string
	skipImage          bool
	imageRef           string
	skipAttestation    bool
	requireAttestation bool
	downloadBaseURL    string // testing override (env CODVIAN_ROUTER_RELEASE_DOWNLOAD_BASE_URL)
}

// runRelease dispatches the `release` subcommand group.
func runRelease(opts options, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "codvian-router: release requires an action: verify")
		return 2
	}
	switch args[0] {
	case "verify":
		return runReleaseVerify(opts, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "codvian-router: unknown release action %q (want verify)\n", args[0])
		return 2
	}
}

func releaseVerifyUsage() {
	fmt.Fprintln(os.Stderr, "usage: codvian-router release verify <tag> [--dir DIR] [--repo OWNER/NAME] [--key PUBKEY] [--skip-image] [--image REF] [--skip-attestation] [--require-attestation]")
	fmt.Fprintln(os.Stderr, "  <tag>      release tag, e.g. v0.1.0 (a leading v is added when missing)")
	fmt.Fprintln(os.Stderr, "  --dir DIR  verify files already on disk in DIR instead of downloading (all six platform archives expected)")
	fmt.Fprintln(os.Stderr, "  --key KEY  keypair mode: verify bundles against this cosign public key (local test fixtures only)")
	fmt.Fprintln(os.Stderr, "  --skip-image  do not verify the published Docker image (remote mode verifies it by default)")
	fmt.Fprintln(os.Stderr, "  --image REF   verify this image reference instead of ghcr.io/raunakiitp/codvian-router:<tag>")
	fmt.Fprintln(os.Stderr, "  --skip-attestation  skip the `gh attestation verify` steps")
	fmt.Fprintln(os.Stderr, "  --require-attestation  fail instead of skipping when the `gh` CLI is missing")
}

// runReleaseVerify parses `release verify` flags and runs the
// verification, printing one line per check and a final verdict.
// The flags live in opts (the global parser consumes every --flag
// before the subcommand sees the args); args may also carry them
// for direct calls, and override the opts values.
func runReleaseVerify(opts options, args []string) int {
	cfg, err := parseReleaseVerifyArgs(args, opts)
	if err != nil {
		if err == errReleaseVerifyHelp {
			return 0
		}
		fmt.Fprintf(os.Stderr, "codvian-router: release verify: %v\n", err)
		releaseVerifyUsage()
		return 2
	}
	return verifyRelease(cfg, release.NewExecToolVerifier())
}

// parseReleaseVerifyArgs parses the flags for `release verify`. The
// first positional argument is the tag; anything else positional is
// an error. Flags accept both `--flag value` and `--flag=value`.
// The global parser turns --help into a "help" positional, so that
// is accepted as a help request too.
func parseReleaseVerifyArgs(args []string, opts options) (*releaseVerifyConfig, error) {
	cfg := &releaseVerifyConfig{
		repo:               release.DefaultRepo,
		dir:                opts.releaseDir,
		keyPath:            opts.releaseKey,
		imageRef:           opts.releaseImage,
		skipImage:          opts.releaseSkipImage,
		skipAttestation:    opts.releaseSkipAttestation,
		requireAttestation: opts.releaseRequireAttestation,
		downloadBaseURL:    opts.releaseDownloadBaseURL,
	}
	if cfg.repo == "" {
		cfg.repo = release.DefaultRepo
	}
	if opts.releaseRepo != "" {
		cfg.repo = opts.releaseRepo
	}
	val := func(i *int, flag string) (string, error) {
		a := args[*i]
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"="), nil
		}
		if a == flag && *i+1 < len(args) {
			*i++
			return args[*i], nil
		}
		return "", fmt.Errorf("--%s needs a value", flag)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--skip-image":
			cfg.skipImage = true
		case a == "--skip-attestation":
			cfg.skipAttestation = true
		case a == "--require-attestation":
			cfg.requireAttestation = true
		case a == "-h" || a == "--help":
			releaseVerifyUsage()
			// Usage was requested: signal it with a sentinel the
			// caller turns into exit 0. (parse errors return a
			// non-nil error instead.)
			return nil, errReleaseVerifyHelp
		case strings.HasPrefix(a, "--dir"):
			v, err := val(&i, "--dir")
			if err != nil || v == "" {
				return nil, fmt.Errorf("--dir needs a directory")
			}
			cfg.dir = v
		case strings.HasPrefix(a, "--repo"):
			v, err := val(&i, "--repo")
			if err != nil || v == "" {
				return nil, fmt.Errorf("--repo needs OWNER/NAME")
			}
			cfg.repo = v
		case strings.HasPrefix(a, "--key"):
			v, err := val(&i, "--key")
			if err != nil || v == "" {
				return nil, fmt.Errorf("--key needs a public-key file")
			}
			cfg.keyPath = v
		case strings.HasPrefix(a, "--image"):
			v, err := val(&i, "--image")
			if err != nil || v == "" {
				return nil, fmt.Errorf("--image needs an image reference")
			}
			cfg.imageRef = v
		case strings.HasPrefix(a, "--download-base-url"):
			v, err := val(&i, "--download-base-url")
			if err != nil || v == "" {
				return nil, fmt.Errorf("--download-base-url needs a URL")
			}
			cfg.downloadBaseURL = v
		case strings.HasPrefix(a, "-"):
			return nil, fmt.Errorf("unknown flag %q", a)
		case a == "help" && cfg.tag == "":
			// The global parser turns --help into a "help"
			// positional; treat it as a help request.
			releaseVerifyUsage()
			return nil, errReleaseVerifyHelp
		default:
			if cfg.tag != "" {
				return nil, fmt.Errorf("unexpected argument %q (only one tag)", a)
			}
			cfg.tag = a
		}
	}
	if cfg.tag == "" {
		return nil, fmt.Errorf("a release tag is required (e.g. v0.1.0)")
	}
	tag, err := release.NormalizeTag(cfg.tag)
	if err != nil {
		return nil, err
	}
	cfg.tag = tag
	if cfg.downloadBaseURL == "" {
		cfg.downloadBaseURL = os.Getenv("CODVIAN_ROUTER_RELEASE_DOWNLOAD_BASE_URL")
	}
	return cfg, nil
}

// errReleaseVerifyHelp is the sentinel parseReleaseVerifyArgs returns
// when --help was requested.
var errReleaseVerifyHelp = fmt.Errorf("help requested")

// verifyRelease runs the verification for a parsed config: downloads
// the release in remote mode (or uses --dir in local mode), runs the
// engine, prints the report, and returns the exit code (0 verified,
// 1 failed, 2 usage error).
func verifyRelease(cfg *releaseVerifyConfig, tools release.ToolVerifier) int {
	if cfg == nil {
		return 2
	}
	opts := release.VerifyOptions{
		Tag:                cfg.tag,
		Repo:               cfg.repo,
		KeyPath:            cfg.keyPath,
		SkipImage:          cfg.skipImage,
		ImageRef:           cfg.imageRef,
		SkipAttestation:    cfg.skipAttestation,
		RequireAttestation: cfg.requireAttestation,
		Tools:              tools,
	}
	if cfg.dir != "" {
		// Local mode: all six platform archives, no image (unless
		// --image was given), no attestations (they live in
		// GitHub's attestation store, not in the directory).
		fmt.Printf("verifying local release directory %s for %s ...\n", cfg.dir, cfg.tag)
		opts.Dir = cfg.dir
		if cfg.imageRef == "" {
			opts.SkipImage = true
		}
		opts.SkipAttestation = true
	} else {
		// Remote mode: download this machine's platform archive +
		// checksums + SBOM + bundles, then verify exactly those.
		dir, err := downloadRelease(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: release verify: %v\n", err)
			return 1
		}
		defer os.RemoveAll(dir)
		fmt.Printf("verifying %s for %s/%s ...\n", cfg.tag, runtime.GOOS, runtime.GOARCH)
		opts.Dir = dir
		archive := release.ArchiveName(strings.TrimPrefix(cfg.tag, "v"), runtime.GOOS, runtime.GOARCH)
		opts.Archives = []string{archive}
		// checksums.txt lists all six platforms; only this
		// platform's files were downloaded — verify that subset
		// (the --ignore-missing rule of scripts/verify-release.sh).
		opts.VerifyOnly = []string{archive, archive + ".sbom.json"}
	}
	report, err := release.VerifyReleaseDir(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: release verify: %v\n", err)
		return 1
	}
	printVerifyReport(report)
	if report.OK() {
		fmt.Printf("\nVERIFIED: %s\n", cfg.tag)
		fmt.Println("Safe to install. (Install with scripts/install.sh, which re-checks the signature itself.)")
		return 0
	}
	failed := report.Failed()
	fmt.Printf("\nFAILED: %s — %d check(s) failed; do not install or run these files.\n", cfg.tag, len(failed))
	fmt.Println("Report it at https://github.com/raunakiitp/CodvianRouter/issues.")
	return 1
}

// printVerifyReport prints one line per check: ok / FAIL / SKIP with
// the check name and its detail (which names the artifact).
func printVerifyReport(report *release.Report) {
	for _, c := range report.Checks {
		fmt.Printf("%-4s %s — %s\n", c.Status.String(), c.Name, c.Detail)
	}
}

// downloadRelease fetches the release files for this machine into a
// fresh temporary directory and returns its path. The base URL is
// https://github.com/<repo>/releases/download/<tag> unless the
// testing override is set (flag --download-base-url, or the
// CODVIAN_ROUTER_RELEASE_DOWNLOAD_BASE_URL environment variable —
// used by tests to point at a local server; whatever it points at is
// still signature- + checksum-verified exactly as a GitHub download
// would be).
func downloadRelease(cfg *releaseVerifyConfig) (string, error) {
	base := cfg.downloadBaseURL
	if base == "" {
		base = "https://github.com/" + cfg.repo + "/releases/download/" + cfg.tag
	}
	base = strings.TrimSuffix(base, "/")
	version := strings.TrimPrefix(cfg.tag, "v")
	archive := release.ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	files := []string{
		archive,
		"checksums.txt",
		"checksums.txt.sigstore.json",
		archive + ".sbom.json",
		archive + ".sbom.json.sigstore.json",
	}
	dir, err := os.MkdirTemp("", "codvian-router-release-verify-*")
	if err != nil {
		return "", fmt.Errorf("creating temp directory: %w", err)
	}
	client := &http.Client{Timeout: 2 * time.Minute}
	for _, file := range files {
		fileURL := base + "/" + file
		if err := downloadFile(client, fileURL, filepath.Join(dir, file)); err != nil {
			os.RemoveAll(dir)
			return "", fmt.Errorf("download failed: %s (%v) — check the version exists at https://github.com/%s/releases", fileURL, err, cfg.repo)
		}
	}
	return dir, nil
}

// downloadFile GETs fileURL and writes it to dest (0600: release
// files are not secrets, but the temp dir holds the to-be-verified
// set and nothing else needs to read it). A non-200 status or an
// over-large body is an error.
func downloadFile(client *http.Client, fileURL, dest string) error {
	if _, err := url.Parse(fileURL); err != nil {
		return fmt.Errorf("bad URL %q: %w", fileURL, err)
	}
	resp, err := client.Get(fileURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if info, err := os.Stat(dest); err != nil {
		return err
	} else if info.Size() > maxDownloadBytes {
		return fmt.Errorf("downloaded file exceeds %d bytes", maxDownloadBytes)
	}
	return nil
}
