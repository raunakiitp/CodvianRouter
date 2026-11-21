// `codvian-router update` — self-update from a published GitHub
// release, with the same verification discipline as
// scripts/install.sh:
//
//  1. resolve the tag (--tag, a positional tag, or the latest
//     release from the GitHub API);
//  2. download the release tarball + checksums.txt (the manifest
//     download is retried 3 times, then the update FAILS CLOSED —
//     a release whose manifest cannot be fetched is never
//     installed unverified);
//  3. verify the Sigstore cosign keyless signature over
//     checksums.txt when cosign is installed, BEFORE trusting any
//     checksum — and refuse when the signature check fails. Without
//     cosign it warns loudly and continues with checksum-only
//     verification, unless CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is
//     set (then it refuses), exactly like install.sh;
//  4. verify the tarball's SHA-256 against checksums.txt (the
//     archive must be listed in the manifest; a mismatch or a
//     missing entry refuses the install);
//  5. extract the binary, stop the supervised gateway if one is
//     running, swap the binary atomically (old binary kept as
//     .bak until the swap succeeds, restored on failure), and
//     restart the gateway when it was running.
//
// `update --check` only reports whether a newer release exists.
// The running binary is never replaced on Windows while it is
// loaded — the rename fails there and the update refuses with the
// old binary untouched (use scripts/install.sh on Windows).
//
// The HTTP POST /admin/update endpoint is intentionally NOT wired
// to this: the handler runs inside the gateway it would replace,
// so a real implementation needs self-exec/restart semantics and
// an async job, not this CLI's stop-then-swap. It stays an honest
// 501 until that design lands.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/release"
	"github.com/raunakiitp/codvian-router/internal/tray"
)

// updateAPIBase is the GitHub API base for the latest-release
// lookup. CODVIAN_ROUTER_UPDATE_API_BASE_URL overrides it —
// testing-only (tests point it at a local server serving a
// controlled releases/latest payload).
func updateAPIBase() string {
	if base := os.Getenv("CODVIAN_ROUTER_UPDATE_API_BASE_URL"); base != "" {
		return strings.TrimSuffix(base, "/")
	}
	return "https://api.github.com"
}

// updateDownloadBase returns the base URL release files are
// downloaded from for tag. CODVIAN_ROUTER_UPDATE_DOWNLOAD_BASE_URL
// overrides it — testing-only (tests point it at a local server
// serving controlled artifacts; whatever it points at is still
// signature- + checksum-verified exactly as a GitHub download
// would be).
func updateDownloadBase(tag string) string {
	if base := os.Getenv("CODVIAN_ROUTER_UPDATE_DOWNLOAD_BASE_URL"); base != "" {
		return strings.TrimSuffix(base, "/") + "/" + tag
	}
	return "https://github.com/" + release.DefaultRepo + "/releases/download/" + tag
}

func updateHTTPClient() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute}
}

// resolveLatestTag returns the latest release tag (v-prefixed) from
// the GitHub releases API.
// The lookup itself lives in internal/release (LatestTag) so the CLI
// and POST /admin/update classify failures the same way; a confirmed
// "no published release" is a *release.LookupError of kind
// KindNoRelease, never a guessed tag.
func resolveLatestTag(apiBase string, client *http.Client) (string, error) {
	tag, err := release.LatestTag(context.Background(), client, apiBase, release.DefaultRepo)
	if release.IsNoRelease(err) {
		return "", fmt.Errorf("%s has no published release yet: nothing to update to", release.DefaultRepo)
	}
	return tag, err
}

// downloadWithRetries GETs url to dest, retrying 3 attempts with
// 2s/4s backoff (the install.sh manifest discipline). A persistently
// failing download is an error, never a silent skip.
func downloadWithRetries(client *http.Client, url, dest string) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if err := downloadFile(client, url, dest); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < 3 {
			fmt.Fprintf(os.Stderr, "codvian-router: update: download failed (attempt %d/3) — retrying in %ds...\n", attempt, attempt*2)
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
	}
	return fmt.Errorf("download failed after 3 attempts: %s (%v)", url, lastErr)
}

// updateDownload is a downloaded release awaiting verification.
type updateDownload struct {
	tag         string
	dir         string // temp dir; caller removes it
	archiveName string
	archivePath string
	baseURL     string
}

// downloadUpdateFiles fetches the platform archive + checksums.txt
// for tag into a fresh temp directory.
func downloadUpdateFiles(tag, baseURL string, client *http.Client) (*updateDownload, error) {
	dir, err := os.MkdirTemp("", "codvian-router-update-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp directory: %w", err)
	}
	archive := release.ArchiveName(strings.TrimPrefix(tag, "v"), runtime.GOOS, runtime.GOARCH)
	dl := &updateDownload{tag: tag, dir: dir, archiveName: archive, archivePath: filepath.Join(dir, archive), baseURL: baseURL}
	if err := downloadWithRetries(client, baseURL+"/"+archive, dl.archivePath); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("download failed: %v — check the version exists at https://github.com/%s/releases", err, release.DefaultRepo)
	}
	if err := downloadWithRetries(client, baseURL+"/checksums.txt", filepath.Join(dir, "checksums.txt")); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("could not download checksums.txt for %s after 3 attempts — refusing to install a release whose manifest cannot be checked", tag)
	}
	return dl, nil
}

// verifyUpdateArchive verifies a downloaded release with the
// install.sh discipline: cosign keyless signature over
// checksums.txt first (when cosign is installed — fail closed on
// a bad signature or a missing bundle), then the archive's SHA-256
// against the manifest (the archive must be listed; a mismatch
// refuses the install). Without cosign it warns loudly and
// continues checksum-only, unless
// CODVIAN_ROUTER_REQUIRE_SIGNATURE=1, which refuses.
func verifyUpdateArchive(dl *updateDownload, client *http.Client, tools release.ToolVerifier) error {
	manifestPath := filepath.Join(dl.dir, "checksums.txt")
	bundlePath := filepath.Join(dl.dir, "checksums.txt.sigstore.json")
	if tools != nil && tools.HasTool("cosign") {
		if err := downloadWithRetries(client, dl.baseURL+"/checksums.txt.sigstore.json", bundlePath); err != nil {
			return fmt.Errorf("could not download checksums.txt.sigstore.json for %s — refusing to install a release whose signature cannot be checked", dl.tag)
		}
		if err := tools.VerifyBlobKeyless(manifestPath, bundlePath, release.ReleaseIdentity(dl.tag), release.ReleaseOIDCIssuer); err != nil {
			return fmt.Errorf("cosign signature verification FAILED for checksums.txt: %v — the manifest is not from the Codvian Router release workflow for %s. Refusing to install", err, dl.tag)
		}
		fmt.Println("signature verified (Sigstore cosign, keyless).")
	} else if os.Getenv("CODVIAN_ROUTER_REQUIRE_SIGNATURE") == "1" {
		return errors.New("CODVIAN_ROUTER_REQUIRE_SIGNATURE=1 is set but cosign is not installed. Install cosign (https://docs.sigstore.dev/cosign/installation/) and re-run, or unset the variable to fall back to checksum-only verification")
	} else {
		fmt.Println("WARNING: cosign not found — updating with checksum-only verification (checksums.txt itself is unverified).")
		fmt.Printf("To verify the release signature first, install cosign and run: codvian-router release verify %s\n", dl.tag)
	}

	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("checksums.txt unreadable: %w", err)
	}
	entries, err := release.ParseChecksums(manifest)
	if err != nil {
		return fmt.Errorf("checksums.txt malformed: %w", err)
	}
	want := ""
	for _, e := range entries {
		if e.Name == dl.archiveName {
			want = e.SHA256
			break
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt does not list %s — refusing to install an unverified binary", dl.archiveName)
	}
	got, err := release.HashFile(dl.archivePath)
	if err != nil {
		return fmt.Errorf("cannot hash %s: %w", dl.archiveName, err)
	}
	if got != want {
		return fmt.Errorf("SHA-256 MISMATCH for %s (expected %s, got %s). The download may be corrupt or tampered with — refusing to install", dl.archiveName, want, got)
	}
	fmt.Println("checksum verified (sha256).")
	return nil
}

// extractUpdateBinary extracts the codvian-router binary from the
// release archive into destDir and returns its path. Only the
// single expected entry is ever written; any other entry —
// including a path-traversal entry — is skipped, never extracted.
func extractUpdateBinary(archivePath, destDir string) (string, error) {
	binaryName := "codvian-router"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	dest := filepath.Join(destDir, binaryName)
	if strings.HasSuffix(archivePath, ".tar.gz") {
		f, err := os.Open(archivePath)
		if err != nil {
			return "", err
		}
		defer f.Close()
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", archivePath, err)
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", fmt.Errorf("cannot read %s: %w", archivePath, err)
			}
			if hdr.Name != binaryName || hdr.Typeflag != tar.TypeReg {
				continue
			}
			if hdr.Size <= 0 || hdr.Size > maxDownloadBytes {
				return "", fmt.Errorf("archive entry %s has an implausible size (%d bytes)", binaryName, hdr.Size)
			}
			out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
			if err != nil {
				return "", err
			}
			_, copyErr := io.Copy(out, io.LimitReader(tr, maxDownloadBytes+1))
			closeErr := out.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			// OpenFile's perm is masked by the umask; chmod
			// explicitly so the installed binary is 0755
			// regardless of the operator's umask.
			if err := os.Chmod(dest, 0o755); err != nil {
				return "", err
			}
			return dest, nil
		}
		return "", fmt.Errorf("the archive did not contain a %s binary", binaryName)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return "", fmt.Errorf("cannot read %s: %w", archivePath, err)
		}
		defer zr.Close()
		for _, zf := range zr.File {
			if zf.Name != binaryName {
				continue
			}
			if zf.UncompressedSize64 == 0 || zf.UncompressedSize64 > maxDownloadBytes {
				return "", fmt.Errorf("archive entry %s has an implausible size (%d bytes)", binaryName, zf.UncompressedSize64)
			}
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
			if err != nil {
				rc.Close()
				return "", err
			}
			_, copyErr := io.Copy(out, io.LimitReader(rc, maxDownloadBytes+1))
			closeErr := out.Close()
			rc.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			// OpenFile's perm is masked by the umask; chmod
			// explicitly so the installed binary is 0755
			// regardless of the operator's umask.
			if err := os.Chmod(dest, 0o755); err != nil {
				return "", err
			}
			return dest, nil
		}
		return "", fmt.Errorf("the archive did not contain a %s binary", binaryName)
	}
	return "", fmt.Errorf("unsupported archive %s (want .tar.gz or .zip)", archivePath)
}

// swapBinary atomically replaces the binary at targetPath with the
// verified binary at newPath (same directory, so the rename cannot
// cross filesystems). The current binary is kept as targetPath.bak
// until the swap succeeds and restored on failure, so a failed
// swap never leaves the operator without a binary.
func swapBinary(newPath, targetPath string) error {
	if _, err := os.Stat(targetPath); err != nil {
		return fmt.Errorf("current binary %s: %w", targetPath, err)
	}
	bakPath := targetPath + ".bak"
	_ = os.Remove(bakPath)
	if err := os.Rename(targetPath, bakPath); err != nil {
		return fmt.Errorf("cannot move the current binary aside: %w (nothing was changed)", err)
	}
	if err := os.Rename(newPath, targetPath); err != nil {
		_ = os.Rename(bakPath, targetPath)
		return fmt.Errorf("cannot install the new binary: %w (previous binary restored)", err)
	}
	if err := os.Chmod(targetPath, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: warning: cannot chmod %s: %v\n", targetPath, err)
	}
	_ = os.Remove(bakPath)
	return nil
}

// currentBinaryPath returns the running binary's path, resolving
// symlinks so the swap replaces the real file, not a link.
func currentBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

// confirmUpdate asks the operator to confirm the update on a
// terminal. A non-terminal stdin (piped, cron, CI) refuses without
// --yes, so an update never runs unattended by accident.
func confirmUpdate(tag string) bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: update: refusing without --yes: stdin is not a terminal (non-interactive run)\n")
		return false
	}
	fmt.Printf("Update codvian-router %s -> %s? [y/N] ", Version, tag)
	var line string
	if _, err := fmt.Scanln(&line); err != nil && line == "" {
		return false
	}
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func runUpdate(opts options, args []string) int {
	tag := opts.updateTag
	if tag == "" && len(args) > 0 {
		tag = args[0]
	}
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "codvian-router: update takes at most one tag argument\n")
		return 2
	}
	client := updateHTTPClient()
	if tag == "" {
		latest, err := resolveLatestTag(updateAPIBase(), client)
		if err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
			return 1
		}
		tag = latest
	} else if normalized, err := release.NormalizeTag(tag); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		return 2
	} else {
		tag = normalized
	}

	if opts.updateCheck {
		current := strings.TrimPrefix(Version, "v")
		if current == strings.TrimPrefix(tag, "v") {
			fmt.Printf("codvian-router is up to date (%s)\n", tag)
		} else {
			fmt.Printf("update available: %s (current %s)\n  run `codvian-router update` to install it\n", tag, Version)
		}
		return 0
	}

	if strings.TrimPrefix(tag, "v") == strings.TrimPrefix(Version, "v") {
		fmt.Printf("codvian-router is already on %s\n", tag)
		return 0
	}
	if !opts.updateYes && !confirmUpdate(tag) {
		fmt.Fprintf(os.Stderr, "codvian-router: update: aborted (re-run with --yes to update without prompting)\n")
		return 2
	}

	targetPath, err := currentBinaryPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		return 1
	}
	// Prove the target directory is writable (and stage the new
	// binary there) BEFORE stopping the gateway: a failed swap
	// must not take the router down.
	extractDir, err := os.MkdirTemp(filepath.Dir(targetPath), ".codvian-update-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: cannot write to %s: %v\n  re-run as a user who can write that directory, or update with scripts/install.sh\n", filepath.Dir(targetPath), err)
		return 1
	}
	defer os.RemoveAll(extractDir)

	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := resolveLifecyclePort(opts, configPath)

	// Download + verify while the gateway keeps running.
	fmt.Printf("downloading %s ...\n", tag)
	dl, err := downloadUpdateFiles(tag, updateDownloadBase(tag), client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dl.dir)
	if err := verifyUpdateArchive(dl, client, release.NewExecToolVerifier()); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		return 1
	}
	newBin, err := extractUpdateBinary(dl.archivePath, extractDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		return 1
	}

	stopped, unmanaged, err := stopSupervisedGateway(configPath, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: cannot stop the running gateway: %v\n", err)
		return 1
	}
	if unmanaged {
		fmt.Fprintf(os.Stderr, "codvian-router: update: gateway answers /healthz on port %d but has no supervisor PID file — stop that process directly, then re-run `codvian-router update`\n", port)
		return 1
	}
	if err := swapBinary(newBin, targetPath); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: %v\n", err)
		if stopped {
			// Best effort: bring the old gateway back up.
			if serr := spawnSupervisedGateway(configPath, port); serr != nil {
				fmt.Fprintf(os.Stderr, "codvian-router: update: warning: could not restart the gateway after a failed swap: %v\n", serr)
			}
		}
		return 1
	}
	fmt.Printf("updated codvian-router %s -> %s (%s)\n", Version, tag, targetPath)
	if !stopped {
		fmt.Println("gateway was not running; start it with `codvian-router restart`")
		return 0
	}
	if err := spawnSupervisedGateway(configPath, port); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: update: installed, but could not restart the gateway: %v\n  start it with `codvian-router restart`\n", err)
		return 1
	}
	pid, healthy := waitForGateway(configPath, port, 15*time.Second)
	if pid == 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: update: installed, but no gateway PID file appeared at %s; see the gateway log at %s\n", tray.PIDFilePath(configPath), tray.LogFilePath(configPath))
		return 1
	}
	fmt.Printf("gateway: restarted on the new binary (pid %d, port %d)\n", pid, port)
	if !healthy {
		fmt.Fprintf(os.Stderr, "codvian-router: update: warning: gateway pid %d is up but /healthz did not answer within 15s; see the gateway log at %s\n", pid, tray.LogFilePath(configPath))
		return 1
	}
	fmt.Printf("  health: ok (GET /healthz -> 200)\n")
	return 0
}
