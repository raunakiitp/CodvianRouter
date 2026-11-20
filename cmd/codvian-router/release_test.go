package main

import (
	"encoding/base64"
	"encoding/json"
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

// --- fake tools (same contract as internal/release's fake) ----------

type releaseFakeTools struct {
	blobErr   error
	imageErr  error
	attestErr error
	calls     []string
}

func (f *releaseFakeTools) VerifyBlobKeyless(file, bundle, identity, issuer string) error {
	f.calls = append(f.calls, "blob-keyless:"+filepath.Base(file))
	return f.blobErr
}
func (f *releaseFakeTools) VerifyBlobKey(file, bundle, pubKey string) error {
	f.calls = append(f.calls, "blob-key:"+filepath.Base(file))
	return f.blobErr
}
func (f *releaseFakeTools) VerifyImageKeyless(image, identity, issuer string) error {
	f.calls = append(f.calls, "image-keyless:"+image)
	return f.imageErr
}
func (f *releaseFakeTools) VerifyImageKey(image, pubKey string) error {
	f.calls = append(f.calls, "image-key:"+image)
	return f.imageErr
}
func (f *releaseFakeTools) VerifyAttestation(target, repo string) error {
	f.calls = append(f.calls, "attest:"+filepath.Base(target))
	return f.attestErr
}
func (f *releaseFakeTools) HasTool(name string) bool { return true }

// --- fixture builder --------------------------------------------------
// A complete fake release directory for the cmd tests: all six
// platform archives + SBOMs + checksums.txt (archives AND SBOMs, the
// real GoReleaser layout) + keypair bundles. Local mode expects a
// complete release — a directory missing platforms FAILS, correctly.
// The `archive`/`sbom` fields point at this machine's platform for
// the tamper tests.

type cmdFixture struct {
	dir     string
	archive string
	sbom    string
}

func buildCmdFixture(t *testing.T, withBundles bool) *cmdFixture {
	t.Helper()
	dir := t.TempDir()
	var manifestNames []string
	var localArchive, localSBOM string
	for _, p := range release.ReleasePlatforms {
		archive := release.ArchiveName("0.1.0", p.GOOS, p.GOARCH)
		sbom := archive + ".sbom.json"
		if err := os.WriteFile(filepath.Join(dir, archive), []byte("fake binary for "+archive+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sbomJSON := `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","name":"codvian-router-0.1.0","dataLicense":"CC0-1.0","packages":[]}`
		if err := os.WriteFile(filepath.Join(dir, sbom), []byte(sbomJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		manifestNames = append(manifestNames, archive, sbom)
		if p.GOOS == runtime.GOOS && p.GOARCH == runtime.GOARCH {
			localArchive, localSBOM = archive, sbom
		}
	}
	manifest, err := release.GenerateChecksums(dir, manifestNames)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if withBundles {
		signed := []string{"checksums.txt"}
		for _, name := range manifestNames {
			if strings.HasSuffix(name, ".sbom.json") {
				signed = append(signed, name)
			}
		}
		for _, file := range signed {
			sum, err := release.HashFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			bundle := keypairBundleJSON(t, sum)
			if err := os.WriteFile(filepath.Join(dir, file+".sigstore.json"), bundle, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return &cmdFixture{dir: dir, archive: localArchive, sbom: localSBOM}
}

// keypairBundleJSON builds a Sigstore bundle carrying a raw public
// key and a DSSE payload claiming digest for its subject.
func keypairBundleJSON(t *testing.T, digest string) []byte {
	t.Helper()
	stmt := map[string]any{
		"_type": "https://in-toto.io/Statement/v0.1",
		"subject": []map[string]any{
			{"name": "artifact", "digest": map[string]string{"sha256": digest}},
		},
		"predicateType": "https://sigstore.dev/cosign/blob/v1",
		"predicate":     map[string]any{},
	}
	payload, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	bundle := map[string]any{
		"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": map[string]any{
			"publicKey": map[string]any{"rawBytes": base64.StdEncoding.EncodeToString([]byte("fake-public-key"))},
		},
		"dsseEnvelope": map[string]any{
			"payloadType": "application/vnd.in-toto+json",
			"payload":     base64.StdEncoding.EncodeToString(payload),
			"signatures":  []map[string]any{{"keyid": "", "sig": base64.StdEncoding.EncodeToString([]byte("fakesig"))}},
		},
	}
	out, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- argument parsing ---------------------------------------------------

func TestParseReleaseVerifyArgs(t *testing.T) {
	cfg, err := parseReleaseVerifyArgs([]string{"v0.1.0", "--dir", "/tmp/d", "--key", "k.pub", "--skip-image", "--require-attestation"}, options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.tag != "v0.1.0" || cfg.dir != "/tmp/d" || cfg.keyPath != "k.pub" || !cfg.skipImage || !cfg.requireAttestation {
		t.Fatalf("parsed config mismatch: %+v", cfg)
	}
	// Tag without a leading v is normalised.
	cfg2, err := parseReleaseVerifyArgs([]string{"0.1.0"}, options{})
	if err != nil || cfg2.tag != "v0.1.0" {
		t.Fatalf("tag normalisation: cfg=%+v err=%v", cfg2, err)
	}
	// --flag=value form.
	cfg3, err := parseReleaseVerifyArgs([]string{"v0.1.0", "--dir=/tmp/d", "--image=example.local/img:tag"}, options{})
	if err != nil || cfg3.dir != "/tmp/d" || cfg3.imageRef != "example.local/img:tag" {
		t.Fatalf("--flag=value form: cfg=%+v err=%v", cfg3, err)
	}
	// Errors: no tag, two tags, unknown flag, missing value.
	for _, args := range [][]string{
		{},
		{"v0.1.0", "v0.2.0"},
		{"v0.1.0", "--bogus"},
		{"v0.1.0", "--dir"},
		{"v0.1.0", "--repo"},
	} {
		if _, err := parseReleaseVerifyArgs(args, options{}); err == nil {
			t.Fatalf("args %v must fail parsing", args)
		}
	}
	// --help is the sentinel, not an error.
	if _, err := parseReleaseVerifyArgs([]string{"--help"}, options{}); err != errReleaseVerifyHelp {
		t.Fatalf("--help must return the help sentinel, got %v", err)
	}
	if code := runReleaseVerify(options{}, []string{"--help"}); code != 0 {
		t.Fatalf("--help must exit 0, got %d", code)
	}
	if code := runReleaseVerify(options{}, []string{}); code != 2 {
		t.Fatalf("no args must exit 2, got %d", code)
	}
}

func TestRunReleaseDispatch(t *testing.T) {
	if code := runRelease(options{}, nil); code != 2 {
		t.Fatalf("release with no action must exit 2, got %d", code)
	}
	if code := runRelease(options{}, []string{"bogus"}); code != 2 {
		t.Fatalf("unknown release action must exit 2, got %d", code)
	}
}

// The global parser consumes every --flag before the subcommand sees
// the args, so a release flag only works when registered there (the
// provider add-catalog flags are the precedent). This test pins the
// registration: flags after the subcommand AND before it must both
// survive parseArgs and land in opts.
func TestParseArgsAcceptsReleaseFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"release", "verify", "v0.1.0", "--dir", "/tmp/d", "--key", "k.pub", "--skip-image", "--skip-attestation", "--require-attestation", "--image", "img:tag", "--repo", "o/r", "--download-base-url", "http://localhost:9"})
	if err != nil {
		t.Fatalf("parseArgs rejected release flags: %v", err)
	}
	if len(pos) != 3 || pos[0] != "release" || pos[1] != "verify" || pos[2] != "v0.1.0" {
		t.Fatalf("positional after flag parsing: %v", pos)
	}
	if opts.releaseDir != "/tmp/d" || opts.releaseKey != "k.pub" || !opts.releaseSkipImage ||
		!opts.releaseSkipAttestation || !opts.releaseRequireAttestation ||
		opts.releaseImage != "img:tag" || opts.releaseRepo != "o/r" ||
		opts.releaseDownloadBaseURL != "http://localhost:9" {
		t.Fatalf("opts mismatch: %+v", opts)
	}
	// Flags before the subcommand work too.
	opts2, pos2, err := parseArgs([]string{"--dir", "/tmp/d", "release", "verify", "v0.1.0"})
	if err != nil {
		t.Fatalf("parseArgs rejected flags before the subcommand: %v", err)
	}
	if opts2.releaseDir != "/tmp/d" || len(pos2) != 3 {
		t.Fatalf("flags-before-subcommand: opts=%+v pos=%v", opts2, pos2)
	}
	// --flag=value form.
	opts3, _, err := parseArgs([]string{"release", "verify", "v0.1.0", "--dir=/tmp/d", "--skip-image=false"})
	if err != nil {
		t.Fatalf("parseArgs rejected --flag=value form: %v", err)
	}
	if opts3.releaseDir != "/tmp/d" || opts3.releaseSkipImage {
		t.Fatalf("--flag=value form: opts=%+v", opts3)
	}
}

// The subcommand parser seeds its config from opts (where the global
// parser put the flags); explicit args override the seeded values.
func TestParseReleaseVerifyArgsSeedsFromOptions(t *testing.T) {
	opts := options{
		releaseDir: "/tmp/d", releaseKey: "k.pub", releaseSkipImage: true,
		releaseRepo: "o/r", releaseImage: "img:tag",
	}
	cfg, err := parseReleaseVerifyArgs([]string{"v0.1.0"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.dir != "/tmp/d" || cfg.keyPath != "k.pub" || !cfg.skipImage ||
		cfg.repo != "o/r" || cfg.imageRef != "img:tag" {
		t.Fatalf("opts seeding mismatch: %+v", cfg)
	}
	// An explicit --dir in args overrides the seeded one.
	cfg2, err := parseReleaseVerifyArgs([]string{"v0.1.0", "--dir", "/tmp/other"}, opts)
	if err != nil || cfg2.dir != "/tmp/other" {
		t.Fatalf("args must override opts: cfg=%+v err=%v", cfg2, err)
	}
}

// --- local mode ---------------------------------------------------------

func TestReleaseVerifyLocalKeypairHappyPath(t *testing.T) {
	fx := buildCmdFixture(t, true)
	tools := &releaseFakeTools{}
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", dir: fx.dir, keyPath: "testdata/cosign.pub",
	}, tools)
	if code != 0 {
		t.Fatalf("happy-path local verify must exit 0, got %d", code)
	}
	// checksums.txt + every SBOM went through cosign (fake): 1 + 6.
	blobs := 0
	for _, c := range tools.calls {
		if strings.HasPrefix(c, "blob-key:") {
			blobs++
		}
	}
	if blobs != 7 {
		t.Fatalf("expected 7 blob verifications (checksums + 6 SBOMs), got %d: %v", blobs, tools.calls)
	}
	// Local mode never calls the attestation check (nothing
	// published to attest) and never checks an image.
	for _, c := range tools.calls {
		if strings.HasPrefix(c, "attest:") || strings.HasPrefix(c, "image-") {
			t.Fatalf("local mode must not call %q", c)
		}
	}
}

func TestReleaseVerifyLocalTamperedArchiveFails(t *testing.T) {
	fx := buildCmdFixture(t, true)
	p := filepath.Join(fx.dir, fx.archive)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", dir: fx.dir, keyPath: "testdata/cosign.pub",
	}, &releaseFakeTools{})
	if code != 1 {
		t.Fatalf("tampered archive must exit 1, got %d", code)
	}
}

func TestReleaseVerifyLocalUnsignedSnapshotFails(t *testing.T) {
	fx := buildCmdFixture(t, false)
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", dir: fx.dir,
	}, &releaseFakeTools{})
	if code != 1 {
		t.Fatalf("unsigned snapshot directory must exit 1, got %d", code)
	}
}

// --- remote mode (local HTTP server, no real network) -------------------

func TestReleaseVerifyRemoteDownloadHappyPath(t *testing.T) {
	fx := buildCmdFixture(t, true)
	// Serve the fixture dir as the "release" (the download base URL
	// override is the testing hook, mirroring the npm wrapper's
	// CODVIAN_ROUTER_DOWNLOAD_BASE_URL).
	srv := httptest.NewServer(http.FileServer(http.Dir(fx.dir)))
	defer srv.Close()
	tools := &releaseFakeTools{}
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", keyPath: "testdata/cosign.pub", downloadBaseURL: srv.URL,
	}, tools)
	if code != 0 {
		t.Fatalf("remote happy path must exit 0, got %d", code)
	}
	// Remote mode checks the image and the attestation too.
	image := release.ReleaseImageRepo + ":v0.1.0"
	foundImage, foundAttest := false, false
	for _, c := range tools.calls {
		if c == "image-key:"+image {
			foundImage = true
		}
		if c == "attest:"+fx.archive {
			foundAttest = true
		}
	}
	if !foundImage {
		t.Fatalf("remote mode must verify the image, calls: %v", tools.calls)
	}
	if !foundAttest {
		t.Fatalf("remote mode must verify the attestation, calls: %v", tools.calls)
	}
}

func TestReleaseVerifyRemoteTamperedDownloadFails(t *testing.T) {
	fx := buildCmdFixture(t, true)
	// Serve the archive with one flipped byte: the download is
	// intact as HTTP, but the bytes no longer match checksums.txt.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, fx.archive) {
			b, _ := os.ReadFile(filepath.Join(fx.dir, fx.archive))
			b[0] ^= 0xff
			_, _ = w.Write(b)
			return
		}
		http.ServeFile(w, r, filepath.Join(fx.dir, filepath.Base(r.URL.Path)))
	}))
	defer srv.Close()
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", keyPath: "testdata/cosign.pub", downloadBaseURL: srv.URL,
	}, &releaseFakeTools{})
	if code != 1 {
		t.Fatalf("tampered download must exit 1, got %d", code)
	}
}

func TestReleaseVerifyRemoteMissingReleaseFails(t *testing.T) {
	// A server that 404s everything: the download must fail loudly,
	// not "verify" an empty directory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v9.9.9", downloadBaseURL: srv.URL,
	}, &releaseFakeTools{})
	if code != 1 {
		t.Fatalf("missing release must exit 1, got %d", code)
	}
}

func TestReleaseVerifyRemoteCosignRejectionFails(t *testing.T) {
	fx := buildCmdFixture(t, true)
	srv := httptest.NewServer(http.FileServer(http.Dir(fx.dir)))
	defer srv.Close()
	tools := &releaseFakeTools{blobErr: fmt.Errorf("signature verification failed")}
	code := verifyRelease(&releaseVerifyConfig{
		tag: "v0.1.0", keyPath: "testdata/cosign.pub", downloadBaseURL: srv.URL,
	}, tools)
	if code != 1 {
		t.Fatalf("cosign rejection must exit 1, got %d", code)
	}
}
