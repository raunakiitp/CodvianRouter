package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- fixture builders -------------------------------------------------

// fixtureRelease is a fake release directory: archives + checksums.txt
// (+ SBOMs + Sigstore bundles when asked) for a version. It mirrors
// the real GoReleaser layout in .goreleaser.yml as verified by a
// real snapshot build (GoReleaser v2.20.2, this slice): checksums.txt
// lists BOTH the archives AND the SBOMs (one .sbom.json per archive),
// and each SBOM is SPDX-2.3 with SPDXID SPDXRef-DOCUMENT.
type fixtureRelease struct {
	dir      string
	version  string
	archives []string
	sboms    []string
}

func buildFixture(t *testing.T, version string, platforms []Platform, withSBOM, withBundles bool, bundleMode string) *fixtureRelease {
	t.Helper()
	dir := t.TempDir()
	fx := &fixtureRelease{dir: dir, version: version}
	var manifestNames []string
	for _, p := range platforms {
		archive := ArchiveName(version, p.GOOS, p.GOARCH)
		content := []byte("fake codvian-router binary for " + archive + "\n")
		if err := os.WriteFile(filepath.Join(dir, archive), content, 0o644); err != nil {
			t.Fatal(err)
		}
		fx.archives = append(fx.archives, archive)
		manifestNames = append(manifestNames, archive)
		if withSBOM {
			sbom := archive + ".sbom.json"
			sbomJSON := `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","name":"codvian-router-` + version + `","dataLicense":"CC0-1.0","packages":[]}`
			if err := os.WriteFile(filepath.Join(dir, sbom), []byte(sbomJSON), 0o644); err != nil {
				t.Fatal(err)
			}
			fx.sboms = append(fx.sboms, sbom)
			// Real layout fact (snapshot build): the SBOM is listed
			// in checksums.txt alongside its archive.
			manifestNames = append(manifestNames, sbom)
		}
	}
	manifest, err := GenerateChecksums(dir, manifestNames)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if withBundles {
		signed := append([]string{"checksums.txt"}, fx.sboms...)
		for _, file := range signed {
			sum, err := HashFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			var bundle []byte
			switch bundleMode {
			case "keyless":
				bundle = craftKeylessBundle(t, sum, ReleaseIdentity("v"+version), ReleaseOIDCIssuer)
			case "keypair":
				bundle = craftKeypairBundle(t, sum)
			case "lookalike":
				// The LiteLLM scenario: a bundle whose certificate
				// names a lookalike repo (one hyphen off).
				bundle = craftKeylessBundle(t, sum, "https://github.com/raunakiitp/Codvian-Router/.github/workflows/release.yml@refs/tags/v"+version, ReleaseOIDCIssuer)
			case "wrong-digest":
				bundle = craftKeylessBundle(t, strings.Repeat("0", 64), ReleaseIdentity("v"+version), ReleaseOIDCIssuer)
			default:
				t.Fatalf("unknown bundle mode %q", bundleMode)
			}
			if err := os.WriteFile(filepath.Join(dir, file+".sigstore.json"), bundle, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fx
}

// craftKeylessBundle builds a Sigstore bundle (DSSE envelope + a
// self-signed Fulcio-shaped certificate) claiming digest for its
// subject. The certificate is NOT trusted by anyone — the structural
// check only parses it; the cryptographic proof is cosign's job.
func craftKeylessBundle(t *testing.T, digest, identity, issuer string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuerDER, err := asn1.Marshal(issuer)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := certTemplate(t, identity, issuerDER)
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return bundleJSON(t, digest, der, nil)
}

// certTemplate builds a self-signed certificate template with the
// identity as a URI SAN and the Fulcio issuer extension.
func certTemplate(t *testing.T, identity string, issuerDER []byte) *x509.Certificate {
	t.Helper()
	u := mustParseURL(t, identity)
	return &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		URIs:         []*url.URL{u},
		ExtraExtensions: []pkix.Extension{
			{Id: fulcioIssuerOID, Value: issuerDER},
		},
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// craftKeypairBundle builds a bundle carrying a raw public key
// instead of a certificate (the shape cosign sign-blob --key emits).
func craftKeypairBundle(t *testing.T, digest string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return bundleJSON(t, digest, nil, der)
}

func bundleJSON(t *testing.T, digest string, certDER, pubDER []byte) []byte {
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
	vm := map[string]any{}
	if certDER != nil {
		vm["certificate"] = map[string]any{"rawBytes": base64.StdEncoding.EncodeToString(certDER)}
		vm["tlogEntries"] = []map[string]any{{"integratedTime": time.Now().Unix(), "logIndex": 42}}
	}
	if pubDER != nil {
		vm["publicKey"] = map[string]any{"rawBytes": base64.StdEncoding.EncodeToString(pubDER)}
	}
	bundle := map[string]any{
		"mediaType":            "application/vnd.dev.sigstore.bundle.v0.3+json",
		"verificationMaterial": vm,
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

// --- fake tools ---------------------------------------------------------

type fakeTools struct {
	hasCosign bool
	hasGH     bool
	blobErr   error // returned by both blob verifies
	imageErr  error
	attestErr error
	calls     []string
}

func (f *fakeTools) record(call string) { f.calls = append(f.calls, call) }
func (f *fakeTools) VerifyBlobKeyless(file, bundle, identity, issuer string) error {
	f.record("blob-keyless:" + filepath.Base(file) + ":" + identity)
	return f.blobErr
}
func (f *fakeTools) VerifyBlobKey(file, bundle, pubKey string) error {
	f.record("blob-key:" + filepath.Base(file))
	return f.blobErr
}
func (f *fakeTools) VerifyImageKeyless(image, identity, issuer string) error {
	f.record("image-keyless:" + image)
	return f.imageErr
}
func (f *fakeTools) VerifyImageKey(image, pubKey string) error {
	f.record("image-key:" + image)
	return f.imageErr
}
func (f *fakeTools) VerifyAttestation(target, repo string) error {
	f.record("attest:" + filepath.Base(target))
	return f.attestErr
}
func (f *fakeTools) HasTool(name string) bool {
	switch name {
	case "cosign":
		return f.hasCosign
	case "gh":
		return f.hasGH
	}
	return false
}

func newFakeTools() *fakeTools { return &fakeTools{hasCosign: true, hasGH: true} }

func checkNames(r *Report) map[string]CheckStatus {
	out := make(map[string]CheckStatus, len(r.Checks))
	for _, c := range r.Checks {
		out[c.Name] = c.Status
	}
	return out
}

func failDetails(t *testing.T, r *Report) string {
	t.Helper()
	var b strings.Builder
	for _, c := range r.Failed() {
		fmt.Fprintf(&b, "  [%s] %s\n", c.Name, c.Detail)
	}
	return b.String()
}

// --- unit tests ---------------------------------------------------------

func TestArchiveName(t *testing.T) {
	if got := ArchiveName("0.1.0", "linux", "amd64"); got != "codvian-router_0.1.0_linux_amd64.tar.gz" {
		t.Fatalf("linux archive: %q", got)
	}
	if got := ArchiveName("0.1.0", "windows", "arm64"); got != "codvian-router_0.1.0_windows_arm64.zip" {
		t.Fatalf("windows archive: %q", got)
	}
	if got := ExpectedArchives("0.1.0"); len(got) != 6 {
		t.Fatalf("expected 6 archives, got %d: %v", len(got), got)
	}
}

func TestNormalizeTag(t *testing.T) {
	for in, want := range map[string]string{"v0.1.0": "v0.1.0", "0.1.0": "v0.1.0", "  v2.0.0  ": "v2.0.0"} {
		got, err := NormalizeTag(in)
		if err != nil || got != want {
			t.Fatalf("NormalizeTag(%q) = %q, %v (want %q)", in, got, err, want)
		}
	}
	if _, err := NormalizeTag(""); err == nil {
		t.Fatal("empty tag must fail")
	}
	if _, err := NormalizeTag("v"); err == nil {
		t.Fatal("bare v must fail")
	}
}

func TestVerifyReleaseDirKeypairHappyPath(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}, {GOOS: "darwin", GOARCH: "arm64"}}, true, true, "keypair")
	tools := newFakeTools()
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("happy-path report not OK:\n%s", failDetails(t, report))
	}
	// Every signed file went through cosign (fake): checksums.txt + 2 SBOMs.
	blobs := 0
	for _, c := range tools.calls {
		if strings.HasPrefix(c, "blob-key:") {
			blobs++
		}
	}
	if blobs != 3 {
		t.Fatalf("expected 3 blob verifications (checksums + 2 SBOMs), got %d: %v", blobs, tools.calls)
	}
	// Attestation was skipped (local mode) and reported as SKIP, not OK.
	names := checkNames(report)
	if names["attestation: "+fx.archives[0]] != CheckSkipped {
		t.Fatalf("local-mode attestation must be SKIP, got %v", names["attestation: "+fx.archives[0]])
	}
}

func TestVerifyReleaseDirVerifyOnlySubset(t *testing.T) {
	// Remote mode: checksums.txt lists all six platforms, but only
	// this machine's archive + SBOM were downloaded. With
	// VerifyOnly set to the downloaded subset the report passes;
	// without it, the missing platforms fail (the full manifest
	// cannot be satisfied from a partial download).
	fx := buildFixture(t, "0.1.0", ReleasePlatforms, true, true, "keypair")
	// Delete every archive except the first (simulating a
	// single-platform download), keeping checksums.txt as-is.
	for _, a := range fx.archives[1:] {
		if err := os.Remove(filepath.Join(fx.dir, a)); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(fx.dir, a+".sbom.json")); err != nil {
			t.Fatal(err)
		}
	}
	archive := fx.archives[0]
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: []string{archive},
		VerifyOnly: []string{archive, archive + ".sbom.json"},
		KeyPath:    "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("remote subset must pass with VerifyOnly:\n%s", failDetails(t, report))
	}
	// The same partial directory without VerifyOnly must fail: the
	// manifest lists files that are not there.
	report2, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: []string{archive},
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report2.OK() {
		t.Fatal("partial directory without VerifyOnly must fail on the missing manifest entries")
	}
}

func TestVerifyReleaseDirKeylessHappyPath(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keyless")
	tools := newFakeTools()
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		SkipImage: true, SkipAttestation: false, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("keyless happy-path report not OK:\n%s", failDetails(t, report))
	}
	// The keyless identity passed to cosign must pin the workflow + tag.
	want := ReleaseIdentity("v0.1.0")
	for _, c := range tools.calls {
		if strings.HasPrefix(c, "blob-keyless:") && !strings.HasSuffix(c, ":"+want) {
			t.Fatalf("keyless call used wrong identity: %q (want suffix %q)", c, want)
		}
	}
	// Attestation ran for the archive (remote-style mode).
	found := false
	for _, c := range tools.calls {
		if c == "attest:"+fx.archives[0] {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an attestation call for %s, calls: %v", fx.archives[0], tools.calls)
	}
}

func TestVerifyReleaseDirTamperedArchive(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	// Flip one byte in the archive.
	p := filepath.Join(fx.dir, fx.archives[0])
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("tampered archive must fail verification")
	}
	found := false
	for _, c := range report.Failed() {
		if strings.Contains(c.Detail, fx.archives[0]) && strings.Contains(c.Detail, "MISMATCH") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure names the tampered archive with a mismatch:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirTamperedSBOM(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	// Flip one byte in the SBOM: its own bundle's payload digest no
	// longer matches, so the bundle check must fail naming the SBOM.
	p := filepath.Join(fx.dir, fx.sboms[0])
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-2] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("tampered SBOM must fail verification")
	}
	found := false
	for _, c := range report.Failed() {
		if strings.Contains(c.Detail, fx.sboms[0]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure names the tampered SBOM:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirMissingSBOM(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	if err := os.Remove(filepath.Join(fx.dir, fx.sboms[0])); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("missing SBOM must fail verification")
	}
	names := checkNames(report)
	if names["sbom: "+fx.sboms[0]] != CheckFail {
		t.Fatalf("missing SBOM check must FAIL:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirMissingBundle(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	if err := os.Remove(filepath.Join(fx.dir, "checksums.txt.sigstore.json")); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("missing bundle must fail verification")
	}
	names := checkNames(report)
	if names["bundle: checksums.txt.sigstore.json"] != CheckFail {
		t.Fatalf("missing bundle check must FAIL:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirUnsignedSnapshot(t *testing.T) {
	// A snapshot build's dist/ has archives + checksums + SBOMs but
	// no bundles at all. That is correct for a snapshot — and it
	// must NOT verify as a release.
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, false, "")
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("unsigned snapshot directory must fail verification")
	}
	names := checkNames(report)
	if names["bundle: checksums.txt.sigstore.json"] != CheckFail {
		t.Fatalf("unsigned snapshot must fail on the missing bundle:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirLookalikeIdentity(t *testing.T) {
	// The LiteLLM scenario (entry 233): a bundle whose certificate
	// names a lookalike repo. The offline identity check must
	// reject it, naming the bundle and the wrong identity.
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "lookalike")
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("lookalike-identity bundle must fail verification")
	}
	found := false
	for _, c := range report.Failed() {
		if strings.Contains(c.Name, "bundle:") && strings.Contains(c.Detail, "Codvian-Router") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure names the lookalike identity:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirBundleDigestMismatch(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "wrong-digest")
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("bundle bound to different bytes must fail verification")
	}
	found := false
	for _, c := range report.Failed() {
		if strings.Contains(c.Detail, "does not match") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure reports the digest mismatch:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirBadSBOMJSON(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	if err := os.WriteFile(filepath.Join(fx.dir, fx.sboms[0]), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: newFakeTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("malformed SBOM must fail verification")
	}
	names := checkNames(report)
	if names["sbom: "+fx.sboms[0]] != CheckFail {
		t.Fatalf("malformed SBOM check must FAIL:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirMissingCosignFailsClosed(t *testing.T) {
	// No tools at all: the signature checks must FAIL, never pass
	// by skipping. This is the fail-closed rule.
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("verification without cosign must fail closed")
	}
	names := checkNames(report)
	if names["signature: checksums.txt"] != CheckFail {
		t.Fatalf("signature check without cosign must FAIL:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirCosignRejection(t *testing.T) {
	// cosign says the signature is bad (e.g. tampered bundle): the
	// check fails naming the file, even though the offline
	// structural checks passed.
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	tools := newFakeTools()
	tools.blobErr = errors.New("signature verification failed: no matching signature")
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: true, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.OK() {
		t.Fatal("cosign rejection must fail verification")
	}
	found := false
	for _, c := range report.Failed() {
		if c.Name == "signature: checksums.txt" && strings.Contains(c.Detail, "cosign signature verification FAILED") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure reports the cosign rejection:\n%s", failDetails(t, report))
	}
}

func TestVerifyReleaseDirImageCheck(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	tools := newFakeTools()
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: false, SkipAttestation: false, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("image-check report not OK:\n%s", failDetails(t, report))
	}
	image := ReleaseImageRepo + ":v0.1.0"
	names := checkNames(report)
	if names["image: "+image] != CheckPass {
		t.Fatalf("image check must pass with a healthy fake, calls: %v", tools.calls)
	}
	if names["attestation: "+image] != CheckPass {
		// gh is present in the fake, so the image attestation runs
		// (oci:// target) and passes.
		t.Fatalf("image attestation must pass with a healthy fake, got %v", names["attestation: "+image])
	}
	// And with a failing image signature, the report fails naming it.
	tools2 := newFakeTools()
	tools2.imageErr = errors.New("no signatures found")
	report2, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: false, SkipAttestation: true, Tools: tools2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report2.OK() {
		t.Fatal("failing image signature must fail verification")
	}
	names2 := checkNames(report2)
	if names2["image: "+image] != CheckFail {
		t.Fatalf("image check must FAIL on cosign rejection:\n%s", failDetails(t, report2))
	}
}

func TestVerifyReleaseDirAttestationWithoutGH(t *testing.T) {
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, true, "keypair")
	tools := newFakeTools()
	tools.hasGH = false
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: false, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Attestation skipped (loudly) is not a failure — but it must
	// be visible as SKIP, never silently OK.
	if !report.OK() {
		t.Fatalf("missing gh must not fail the report (attestation is skippable by default):\n%s", failDetails(t, report))
	}
	names := checkNames(report)
	if names["attestation: "+fx.archives[0]] != CheckSkipped {
		t.Fatalf("attestation without gh must be SKIP, got %v", names["attestation: "+fx.archives[0]])
	}

	// With --require-attestation, the same absence FAILS.
	report2, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: "testdata/cosign.pub", SkipImage: true, SkipAttestation: false,
		RequireAttestation: true, Tools: tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report2.OK() {
		t.Fatal("missing gh with RequireAttestation must fail verification")
	}
	names2 := checkNames(report2)
	if names2["attestation: "+fx.archives[0]] != CheckFail {
		t.Fatalf("attestation without gh + RequireAttestation must FAIL:\n%s", failDetails(t, report2))
	}
}

func TestCheckSBOMShape(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		format  string
		wantErr bool
	}{
		{"spdx", `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-DOCUMENT","packages":[]}`, "SPDX-JSON", false},
		{"cyclonedx", `{"bomFormat":"CycloneDX","specVersion":"1.6","components":[]}`, "CycloneDX", false},
		{"garbage", `{not json`, "", true},
		{"neither", `{"foo":1}`, "", true},
		{"spdx-wrong-id", `{"spdxVersion":"SPDX-2.3","SPDXID":"SPDXRef-OTHER"}`, "", true},
		{"cyclonedx-no-version", `{"bomFormat":"CycloneDX"}`, "", true},
	}
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name+".json")
		if err := os.WriteFile(p, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		format, err := CheckSBOMShape(p)
		if tc.wantErr && err == nil {
			t.Fatalf("%s: expected error, got format %q", tc.name, format)
		}
		if !tc.wantErr && (err != nil || format != tc.format) {
			t.Fatalf("%s: format=%q err=%v (want %q)", tc.name, format, err, tc.format)
		}
	}
}

func TestVerifyBundleIdentityDirect(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(artifact, []byte("hello release\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := HashFile(artifact)
	if err != nil {
		t.Fatal(err)
	}
	identity := ReleaseIdentity("v0.1.0")

	// Happy path.
	bundle := filepath.Join(dir, "good.sigstore.json")
	if err := os.WriteFile(bundle, craftKeylessBundle(t, sum, identity, ReleaseOIDCIssuer), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundleIdentity(bundle, artifact, identity, ReleaseOIDCIssuer); err != nil {
		t.Fatalf("good bundle must pass: %v", err)
	}

	// Wrong issuer.
	bundle2 := filepath.Join(dir, "bad-issuer.sigstore.json")
	if err := os.WriteFile(bundle2, craftKeylessBundle(t, sum, identity, "https://evil.example.com"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundleIdentity(bundle2, artifact, identity, ReleaseOIDCIssuer); err == nil {
		t.Fatal("wrong-issuer bundle must fail")
	}

	// No DSSE envelope.
	bundle3 := filepath.Join(dir, "no-dsse.sigstore.json")
	if err := os.WriteFile(bundle3, []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundleIdentity(bundle3, artifact, identity, ReleaseOIDCIssuer); err == nil {
		t.Fatal("bundle without a certificate/envelope must fail")
	}

	// Keypair bundle passed to the keyless check must fail (no cert).
	bundle4 := filepath.Join(dir, "keypair.sigstore.json")
	if err := os.WriteFile(bundle4, craftKeypairBundle(t, sum), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := VerifyBundleIdentity(bundle4, artifact, identity, ReleaseOIDCIssuer); err == nil {
		t.Fatal("keypair bundle must fail the keyless identity check")
	}
	// ...but pass the keypair check.
	if err := VerifyBundleKeypair(bundle4, artifact); err != nil {
		t.Fatalf("keypair bundle must pass the keypair check: %v", err)
	}
}

// --- real-layout test -------------------------------------------------
// Runs the engine against a REAL GoReleaser snapshot dist/ directory
// (produced by `goreleaser release --snapshot --skip=docker,sign`
// with GoReleaser v2.20.2). Gated on CODVIAN_ROUTER_SNAPSHOT_DIST:
// set it to the dist/ dir to run, unset to skip. A snapshot is
// unsigned, so the expected outcome is: checksums + archives +
// SBOMs all PASS (proving the engine reads the real layout), bundles
// FAIL naming the missing .sigstore.json files (proving an unsigned
// snapshot does not verify as a release).

func TestVerifyReleaseDirRealSnapshotLayout(t *testing.T) {
	dist := os.Getenv("CODVIAN_ROUTER_SNAPSHOT_DIST")
	if dist == "" {
		t.Skip("CODVIAN_ROUTER_SNAPSHOT_DIST not set — real-layout test skipped (set it to a goreleaser --snapshot dist/ dir)")
	}
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatalf("reading snapshot dist %s: %v", dist, err)
	}
	var archives []string
	for _, e := range entries {
		name := e.Name()
		if (strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip")) && !strings.HasSuffix(name, ".sbom.json") {
			archives = append(archives, name)
		}
	}
	if len(archives) == 0 {
		t.Fatalf("no archives in snapshot dist %s", dist)
	}
	// The snapshot version is embedded in the archive names
	// (codvian-router_<version>_<os>_<arch>.tar.gz); the tag is
	// only used to derive expected names when Archives is empty,
	// and here Archives is explicit, so any well-formed tag works.
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.0.0-SNAPSHOT", Dir: dist, Archives: archives,
		SkipImage: true, SkipAttestation: true, Tools: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := checkNames(report)
	if names["checksums: files match checksums.txt"] != CheckPass {
		t.Fatalf("real snapshot checksums must pass:\n%s", failDetails(t, report))
	}
	for _, a := range archives {
		if names["archive: "+a] != CheckPass {
			t.Fatalf("real snapshot archive %s must pass:\n%s", a, failDetails(t, report))
		}
		if names["sbom: "+a+".sbom.json"] != CheckPass {
			t.Fatalf("real snapshot SBOM %s must pass (SPDX-JSON):\n%s", a+".sbom.json", failDetails(t, report))
		}
		if names["bundle: "+a+".sbom.json.sigstore.json"] != CheckFail {
			t.Fatalf("unsigned snapshot SBOM bundle %s must FAIL:\n%s", a+".sbom.json.sigstore.json", failDetails(t, report))
		}
	}
	if names["bundle: checksums.txt.sigstore.json"] != CheckFail {
		t.Fatalf("unsigned snapshot checksums bundle must FAIL:\n%s", failDetails(t, report))
	}
	if report.OK() {
		t.Fatal("unsigned snapshot directory must not verify as a release")
	}
}

// --- exec-verifier CLI contract --------------------------------------
// Proves the real ToolVerifier invokes cosign/gh with exactly the
// arguments the release docs promise (pinned identity, issuer,
// --bundle, --insecure-ignore-tlog for keypair mode, oci:// for
// image attestations) — deterministically, offline, via stub
// executables that log their argv. This is the proof the
// real-cosign integration test cannot give in this VM.

func writeStub(t *testing.T, dir, name string) string {
	t.Helper()
	// The stub appends its argv (one line per invocation) to
	// $STUB_LOG and exits $STUB_EXIT (default 0).
	// Windows cannot execute a #!/bin/sh script and has no sh:
	// the stub must be a .bat file (found via PATHEXT, run via
	// cmd.exe by CreateProcess). %* is the argv, space-joined —
	// the same one-line-per-invocation contract as the sh stub.
	// Batch files need CRLF line endings.
	var script, file string
	if runtime.GOOS == "windows" {
		file = name + ".bat"
		script = "@echo off\r\n" +
			// Redirection first: `echo %* >> file` would echo a
			// trailing space (cmd includes the space before >>).
			">> \"%STUB_LOG%\" echo %*\r\n" +
			"exit /b %STUB_EXIT%\r\n"
	} else {
		file = name
		script = "#!/bin/sh\n" +
			"echo \"$@\" >> \"$STUB_LOG\"\n" +
			"exit ${STUB_EXIT:-0}\n"
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExecToolVerifierArgv(t *testing.T) {
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "argv.log")
	writeStub(t, binDir, "cosign")
	writeStub(t, binDir, "gh")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_LOG", logPath)

	v := NewExecToolVerifier()
	identity := ReleaseIdentity("v0.1.0")
	if err := v.VerifyBlobKeyless("/d/checksums.txt", "/d/checksums.txt.sigstore.json", identity, ReleaseOIDCIssuer); err != nil {
		t.Fatalf("VerifyBlobKeyless: %v", err)
	}
	if err := v.VerifyBlobKey("/d/a.sbom.json", "/d/a.sbom.json.sigstore.json", "/d/cosign.pub"); err != nil {
		t.Fatalf("VerifyBlobKey: %v", err)
	}
	if err := v.VerifyImageKeyless("ghcr.io/raunakiitp/codvian-router:v0.1.0", identity, ReleaseOIDCIssuer); err != nil {
		t.Fatalf("VerifyImageKeyless: %v", err)
	}
	if err := v.VerifyImageKey("ghcr.io/raunakiitp/codvian-router:v0.1.0", "/d/cosign.pub"); err != nil {
		t.Fatalf("VerifyImageKey: %v", err)
	}
	if err := v.VerifyAttestation("/d/a.tar.gz", DefaultRepo); err != nil {
		t.Fatalf("VerifyAttestation: %v", err)
	}
	if err := v.VerifyAttestation("oci://ghcr.io/raunakiitp/codvian-router:v0.1.0", DefaultRepo); err != nil {
		t.Fatalf("VerifyAttestation oci: %v", err)
	}
	if !v.HasTool("cosign") || !v.HasTool("gh") {
		t.Fatal("HasTool must find the stubs on PATH")
	}
	if v.HasTool("definitely-not-a-tool") {
		t.Fatal("HasTool must not find a nonexistent tool")
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// The Windows batch stub writes CRLF line endings; normalize
	// them so the argv comparison is line-ending agnostic.
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 tool invocations, got %d:\n%s", len(lines), string(raw))
	}
	want := []string{
		"verify-blob --certificate-identity " + identity + " --certificate-oidc-issuer " + ReleaseOIDCIssuer + " --bundle /d/checksums.txt.sigstore.json /d/checksums.txt",
		"verify-blob --key /d/cosign.pub --bundle /d/a.sbom.json.sigstore.json --insecure-ignore-tlog /d/a.sbom.json",
		"verify --certificate-identity " + identity + " --certificate-oidc-issuer " + ReleaseOIDCIssuer + " ghcr.io/raunakiitp/codvian-router:v0.1.0",
		"verify --key /d/cosign.pub --insecure-ignore-tlog ghcr.io/raunakiitp/codvian-router:v0.1.0",
		"attestation verify /d/a.tar.gz --repo " + DefaultRepo,
		"attestation verify oci://ghcr.io/raunakiitp/codvian-router:v0.1.0 --repo " + DefaultRepo,
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("invocation %d:\n got: %q\nwant: %q", i, lines[i], w)
		}
	}

	// A failing tool surfaces its stderr in the error (naming the
	// subject), and the error is not swallowed.
	t.Setenv("STUB_EXIT", "1")
	if err := v.VerifyBlobKeyless("/d/checksums.txt", "/d/checksums.txt.sigstore.json", identity, ReleaseOIDCIssuer); err == nil {
		t.Fatal("failing cosign must return an error")
	}
}

// --- real-cosign integration test --------------------------------------
// Exercises the actual cosign binary (keypair mode) end to end: sign
// fixtures, verify them through VerifyReleaseDir with the real exec
// verifier, then tamper and watch it fail. Skipped when cosign is not
// on PATH — the unit tests above cover the logic without it.

func TestVerifyReleaseDirRealCosignKeypair(t *testing.T) {
	cosign := lookPath(t, "cosign")
	if cosign == "" {
		t.Skip("cosign not on PATH — real-cosign integration test skipped (unit tests cover the logic)")
	}
	fx := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, false, "")
	// Generate a throwaway keypair (never leaves the test temp dir).
	// cosign v3 writes <prefix>.key + <prefix>.pub.
	prefix := filepath.Join(fx.dir, "test-cosign")
	priv := prefix + ".key"
	pub := prefix + ".pub"
	if err := runTool(t, cosign, "generate-key-pair", "--output-key-prefix", prefix); err != nil {
		t.Skipf("cosign generate-key-pair failed (%v) — skipping real-cosign test", err)
	}
	if _, err := os.Stat(pub); err != nil {
		t.Fatalf("cosign did not write a public key at %s: %v", pub, err)
	}
	// Sign checksums.txt + the SBOM with the throwaway key. The
	// bundle is requested with --tlog-upload=false: a keypair
	// signature needs no Rekor entry. (cosign v3.1.4 rejects the
	// flag — see signBlob — so this skips here; the live path runs
	// where cosign can sign.)
	for _, file := range append([]string{"checksums.txt"}, fx.sboms...) {
		signBlob(t, cosign, priv, filepath.Join(fx.dir, file))
	}
	// The real exec verifier must now pass the whole directory.
	report, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: pub, SkipImage: true, SkipAttestation: true, Tools: NewExecToolVerifier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("real-cosign happy path must pass:\n%s", failDetails(t, report))
	}
	// Tamper with the archive: the signature over checksums.txt
	// still verifies (the manifest is untouched), but the checksum
	// step must fail naming the archive.
	p := filepath.Join(fx.dir, fx.archives[0])
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	report2, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx.dir, Archives: fx.archives,
		KeyPath: pub, SkipImage: true, SkipAttestation: true, Tools: NewExecToolVerifier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report2.OK() {
		t.Fatal("tampered archive must fail real-cosign verification")
	}
	// Tamper with the SBOM instead (fresh fixture): its bundle's
	// payload digest no longer matches, so cosign must reject it.
	fx2 := buildFixture(t, "0.1.0", []Platform{{GOOS: "linux", GOARCH: "amd64"}}, true, false, "")
	for _, file := range append([]string{"checksums.txt"}, fx2.sboms...) {
		signBlob(t, cosign, priv, filepath.Join(fx2.dir, file))
	}
	sbomPath := filepath.Join(fx2.dir, fx2.sboms[0])
	sb, err := os.ReadFile(sbomPath)
	if err != nil {
		t.Fatal(err)
	}
	sb[len(sb)-2] ^= 0xff
	if err := os.WriteFile(sbomPath, sb, 0o644); err != nil {
		t.Fatal(err)
	}
	report3, err := VerifyReleaseDir(VerifyOptions{
		Tag: "v0.1.0", Dir: fx2.dir, Archives: fx2.archives,
		KeyPath: pub, SkipImage: true, SkipAttestation: true, Tools: NewExecToolVerifier(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report3.OK() {
		t.Fatal("tampered SBOM must fail real-cosign verification")
	}
	found := false
	for _, c := range report3.Failed() {
		if strings.Contains(c.Detail, fx2.sboms[0]) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no failure names the tampered SBOM:\n%s", failDetails(t, report3))
	}
}

// --- helpers ------------------------------------------------------------

func lookPath(t *testing.T, name string) string {
	t.Helper()
	p, _ := exec.LookPath(name)
	return p
}

func runTool(t *testing.T, name string, args ...string) error {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "COSIGN_PASSWORD=test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w (output: %s)", name, args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// signBlob signs one file with a throwaway cosign keypair and writes
// the bundle next to it. If signing fails for any reason the test is
// SKIPPED, not failed: cosign v3.1.4's sign-blob requires a signing
// config for offline use (--tlog-upload is deprecated and rejected),
// and reaching Sigstore's online services is not possible from this
// VM (its timestamp endpoint times out). The deterministic proof is
// the unit tests + the fake-tool tests + TestExecToolVerifierArgv
// below; this integration test is the live bonus that runs where
// cosign can actually sign. The skip message records the reason.
func signBlob(t *testing.T, cosign, priv, file string) {
	t.Helper()
	err := runTool(t, cosign, "sign-blob", "--key", priv,
		"--bundle", file+".sigstore.json", "--tlog-upload=false", "--yes", file)
	if err != nil {
		t.Skipf("cosign sign-blob failed (%v) — real-cosign integration test skipped; see the unit tests for the deterministic proof", err)
	}
}
