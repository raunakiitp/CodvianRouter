package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/release"
)

// fakeReleaseServer serves a controlled release: a tarball holding
// a fake codvian-router binary, a checksums.txt for it, and a
// releases/latest payload. Options corrupt one piece at a time so
// each fail-closed rule has its own test.
type fakeReleaseOpts struct {
	tag             string
	tamperArchive   bool // flip a byte after the checksum was computed
	omitFromManfest bool // checksums.txt lists a different file
	omitManifest    bool // checksums.txt 404s
	noBinaryEntry   bool // tarball holds some other file
	traversalEntry  bool // tarball also holds a ../evil entry
}

func buildFakeTarball(t *testing.T, opts fakeReleaseOpts) []byte {
	t.Helper()
	// On Windows the release archive is a .zip and
	// extractUpdateBinary dispatches on the extension, so the fake
	// must be a zip there too (a tarball named .zip fails with
	// "not a valid zip file"). The entries mirror the tarball:
	// the binary, an optional traversal entry, and "something-else"
	// when the binary is absent.
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		if opts.traversalEntry {
			w, err := zw.Create("../evil")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("evil")); err != nil {
				t.Fatal(err)
			}
		}
		name := "codvian-router.exe"
		if opts.noBinaryEntry {
			name = "something-else"
		}
		fh := &zip.FileHeader{Name: name, Method: zip.Deflate}
		fh.SetMode(0o755)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("fake-new-codvian-router-binary")); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if opts.traversalEntry {
		evil := []byte("evil")
		if err := tw.WriteHeader(&tar.Header{Name: "../evil", Mode: 0o644, Size: int64(len(evil))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(evil); err != nil {
			t.Fatal(err)
		}
	}
	name := "codvian-router"
	if opts.noBinaryEntry {
		name = "something-else"
	}
	body := []byte("fake-new-codvian-router-binary")
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fakeReleaseServer(t *testing.T, opts fakeReleaseOpts) *httptest.Server {
	t.Helper()
	if opts.tag == "" {
		opts.tag = "v9.9.9"
	}
	version := strings.TrimPrefix(opts.tag, "v")
	archiveName := release.ArchiveName(version, runtime.GOOS, runtime.GOARCH)
	archive := buildFakeTarball(t, opts)
	sum := fmt.Sprintf("%x", sha256.Sum256(archive))
	if opts.tamperArchive {
		archive[len(archive)-1] ^= 0xff
	}
	manifest := sum + "  " + archiveName + "\n"
	if opts.omitFromManfest {
		manifest = sum + "  some-other-file.tar.gz\n"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/raunakiitp/CodvianRouter/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name": %q}`, opts.tag)
	})
	mux.HandleFunc("/"+opts.tag+"/"+archiveName, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	mux.HandleFunc("/"+opts.tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		if opts.omitManifest {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, manifest)
	})
	return httptest.NewServer(mux)
}

func TestResolveLatestTag(t *testing.T) {
	srv := fakeReleaseServer(t, fakeReleaseOpts{})
	defer srv.Close()
	tag, err := resolveLatestTag(srv.URL, updateHTTPClient())
	if err != nil {
		t.Fatalf("resolveLatestTag: %v", err)
	}
	if tag != "v9.9.9" {
		t.Fatalf("tag = %q, want v9.9.9", tag)
	}
	// A server without the endpoint errors instead of inventing a tag.
	bad := httptest.NewServer(http.NotFoundHandler())
	defer bad.Close()
	if _, err := resolveLatestTag(bad.URL, updateHTTPClient()); err == nil {
		t.Fatal("expected error for a 404 latest-release lookup")
	}
}

func TestVerifyUpdateArchiveOK(t *testing.T) {
	srv := fakeReleaseServer(t, fakeReleaseOpts{})
	defer srv.Close()
	dl, err := downloadUpdateFiles("v9.9.9", srv.URL+"/v9.9.9", updateHTTPClient())
	if err != nil {
		t.Fatalf("downloadUpdateFiles: %v", err)
	}
	defer os.RemoveAll(dl.dir)
	// cosign is not installed in this VM, so this exercises the
	// warn-and-continue checksum-only path (the same path
	// install.sh takes without cosign).
	if err := verifyUpdateArchive(dl, updateHTTPClient(), release.NewExecToolVerifier()); err != nil {
		t.Fatalf("verifyUpdateArchive: %v", err)
	}
}

func TestVerifyUpdateArchiveFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		opts fakeReleaseOpts
		want string
	}{
		{"tampered archive", fakeReleaseOpts{tamperArchive: true}, "MISMATCH"},
		{"missing manifest", fakeReleaseOpts{omitManifest: true}, "checksums.txt"},
		{"archive not listed", fakeReleaseOpts{omitFromManfest: true}, "does not list"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := fakeReleaseServer(t, tc.opts)
			defer srv.Close()
			dl, err := downloadUpdateFiles("v9.9.9", srv.URL+"/v9.9.9", updateHTTPClient())
			if err != nil {
				// The missing-manifest case fails at download.
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("downloadUpdateFiles: %v, want %q", err, tc.want)
				}
				return
			}
			defer os.RemoveAll(dl.dir)
			err = verifyUpdateArchive(dl, updateHTTPClient(), release.NewExecToolVerifier())
			if err == nil {
				t.Fatalf("verifyUpdateArchive succeeded, want failure containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyUpdateArchive: %v, want %q", err, tc.want)
			}
		})
	}
}

func TestExtractUpdateBinary(t *testing.T) {
	srv := fakeReleaseServer(t, fakeReleaseOpts{})
	defer srv.Close()
	dl, err := downloadUpdateFiles("v9.9.9", srv.URL+"/v9.9.9", updateHTTPClient())
	if err != nil {
		t.Fatalf("downloadUpdateFiles: %v", err)
	}
	defer os.RemoveAll(dl.dir)
	destDir := t.TempDir()
	got, err := extractUpdateBinary(dl.archivePath, destDir)
	if err != nil {
		t.Fatalf("extractUpdateBinary: %v", err)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "fake-new-codvian-router-binary" {
		t.Fatalf("extracted binary = %q", body)
	}
	// Windows has no Unix permission bits (files report 0666),
	// so the exact mode is only asserted off Windows.
	if info, err := os.Stat(got); err != nil {
		t.Fatalf("Stat extracted binary: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Fatalf("extracted binary mode = %v, want 0755", info.Mode())
	}

	// A traversal entry in the same archive is never written.
	srv2 := fakeReleaseServer(t, fakeReleaseOpts{traversalEntry: true})
	defer srv2.Close()
	dl2, err := downloadUpdateFiles("v9.9.9", srv2.URL+"/v9.9.9", updateHTTPClient())
	if err != nil {
		t.Fatalf("downloadUpdateFiles: %v", err)
	}
	defer os.RemoveAll(dl2.dir)
	destDir2 := t.TempDir()
	if _, err := extractUpdateBinary(dl2.archivePath, destDir2); err != nil {
		t.Fatalf("extractUpdateBinary with traversal entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destDir2, "..", "evil")); !os.IsNotExist(err) {
		t.Fatalf("traversal entry was extracted outside the destination (err = %v)", err)
	}

	// An archive without the binary is an error, not a silent skip.
	srv3 := fakeReleaseServer(t, fakeReleaseOpts{noBinaryEntry: true})
	defer srv3.Close()
	// The manifest still lists the archive, so download + verify
	// pass; extraction must refuse.
	dl3, err := downloadUpdateFiles("v9.9.9", srv3.URL+"/v9.9.9", updateHTTPClient())
	if err != nil {
		t.Fatalf("downloadUpdateFiles: %v", err)
	}
	defer os.RemoveAll(dl3.dir)
	if _, err := extractUpdateBinary(dl3.archivePath, t.TempDir()); err == nil {
		t.Fatal("expected error extracting an archive with no codvian-router binary")
	}
}

func TestSwapBinary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "codvian-router")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	newBin := filepath.Join(dir, "new")
	if err := os.WriteFile(newBin, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := swapBinary(newBin, target); err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("target after swap = %q, err = %v", got, err)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatalf(".bak left behind after a successful swap (err = %v)", err)
	}

	// A failed swap (new binary missing) leaves the old binary
	// untouched.
	if err := swapBinary(filepath.Join(dir, "does-not-exist"), target); err == nil {
		t.Fatal("expected error swapping in a missing binary")
	}
	got, err = os.ReadFile(target)
	if err != nil || string(got) != "new" {
		t.Fatalf("target after failed swap = %q, err = %v (old binary must survive)", got, err)
	}
	if _, err := os.Stat(target + ".bak"); !os.IsNotExist(err) {
		t.Fatalf(".bak left behind after a failed swap (err = %v)", err)
	}
}

func TestRunUpdateCheck(t *testing.T) {
	srv := fakeReleaseServer(t, fakeReleaseOpts{tag: "v9.9.9"})
	defer srv.Close()
	t.Setenv("CODVIAN_ROUTER_UPDATE_API_BASE_URL", srv.URL)
	if code := run([]string{"update", "--check"}); code != 0 {
		t.Fatalf("update --check (newer available): exit = %d, want 0", code)
	}

	srv2 := fakeReleaseServer(t, fakeReleaseOpts{tag: "v0.1.0"})
	defer srv2.Close()
	t.Setenv("CODVIAN_ROUTER_UPDATE_API_BASE_URL", srv2.URL)
	if code := run([]string{"update", "--check"}); code != 0 {
		t.Fatalf("update --check (up to date): exit = %d, want 0", code)
	}
}

func TestRunUpdateRefusesNonInteractiveWithoutYes(t *testing.T) {
	srv := fakeReleaseServer(t, fakeReleaseOpts{tag: "v9.9.9"})
	defer srv.Close()
	t.Setenv("CODVIAN_ROUTER_UPDATE_API_BASE_URL", srv.URL)
	// stdin is a pipe, not a terminal: without --yes the update
	// must refuse before downloading anything.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	if code := run([]string{"update"}); code != 2 {
		t.Fatalf("update without --yes on non-terminal stdin: exit = %d, want 2", code)
	}
}
