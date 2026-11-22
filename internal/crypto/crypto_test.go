package crypto

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSealUnsealRoundTrip(t *testing.T) {
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sk-test-provider-key-12345")
	sealed, err := v.Seal(plain)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if !strings.HasPrefix(sealed, "v2:") {
		t.Fatalf("sealed value %q lacks v2: prefix", sealed)
	}
	if sealed == string(plain) {
		t.Error("sealed value must differ from plaintext")
	}
	got, err := v.Unseal(sealed)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("Unseal = %q, want %q", got, plain)
	}
}

func TestSealIsNonDeterministic(t *testing.T) {
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := v.Seal([]byte("same plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := v.Seal([]byte("same plaintext"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two seals of the same plaintext must differ (random nonce)")
	}
	// Both must still decrypt.
	for _, s := range []string{a, b} {
		if got, err := v.Unseal(s); err != nil || string(got) != "same plaintext" {
			t.Errorf("Unseal(%q) = %q, err = %v", s, got, err)
		}
	}
}

func TestUnsealRejectsTampering(t *testing.T) {
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Flip one character in the middle of the base64 payload (to a
	// different valid base64 character, so the failure is the GCM tag
	// check, not a decode error).
	mid := len(sealed) / 2
	orig := sealed[mid]
	replacement := byte('A')
	if orig == 'A' {
		replacement = 'B'
	}
	tampered := sealed[:mid] + string(replacement) + sealed[mid+1:]
	if tampered == sealed {
		t.Fatal("test setup: tampering produced the same value")
	}
	if _, err := v.Unseal(tampered); err == nil {
		t.Error("Unseal should reject a tampered value")
	}
}

func TestUnsealRejectsBadFormat(t *testing.T) {
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"",
		"not-a-sealed-value",
		"v2:AAAA", // valid prefix but too short for nonce + tag
		"v3:AAAA",
		"v1:not-base64!!!",
		"v1:AAAA", // too short for nonce + tag
	} {
		if _, err := v.Unseal(bad); err == nil {
			t.Errorf("Unseal(%q) should error", bad)
		}
	}
}

func TestVaultIsBoundToDataDir(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	v1, err := New(dir1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := New(dir2)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v1.Seal([]byte("cross-vault"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v2.Unseal(sealed); err == nil {
		t.Error("a value sealed by one vault must not unseal under another vault")
	}
	// Same data dir, new Vault instance: must unseal (master secret persists).
	v1b, err := New(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v1b.Unseal(sealed); err != nil || string(got) != "cross-vault" {
		t.Errorf("reopened vault Unseal = %q, err = %v", got, err)
	}
}

func TestMasterSecretFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "master.key")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("master.key was not created: %v", err)
	}
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("master.key perms = %o, want 0600", info.Mode().Perm())
	}
	secret, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(secret) != 32 {
		t.Errorf("master secret length = %d, want 32", len(secret))
	}
}

func TestNewFromSecret(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	v, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Unseal(sealed); err != nil || string(got) != "hello" {
		t.Errorf("same-secret vault Unseal = %q, err = %v", got, err)
	}
	if _, err := NewFromSecret([]byte("short"), ""); err == nil {
		t.Error("NewFromSecret should reject a secret shorter than 16 bytes")
	}
	if _, err := New(""); err == nil {
		t.Error("New should reject an empty data directory")
	}
}

func TestSealEmptyPlaintext(t *testing.T) {
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.Unseal(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("empty plaintext round-trip gave %q", got)
	}
}

// sealV1 builds a legacy "v1:" sealed value exactly the way the pre-
// Argon2id build did: AES-256-GCM under SHA-256(secret). Used to prove
// backward compatibility with values sealed before this change.
func sealV1(t *testing.T, secret, plaintext []byte) string {
	t.Helper()
	key := sha256.Sum256(secret)
	sealed, err := sealWithKey(key, sealPrefixV1, plaintext)
	if err != nil {
		t.Fatalf("sealV1: %v", err)
	}
	return sealed
}

func TestV1BackwardCompat(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	v, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("legacy-provider-key")
	v1Sealed := sealV1(t, secret, plain)
	if !strings.HasPrefix(v1Sealed, "v1:") {
		t.Fatalf("test setup: v1 value %q lacks v1: prefix", v1Sealed)
	}
	got, err := v.Unseal(v1Sealed)
	if err != nil {
		t.Fatalf("Unseal of legacy v1 value: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("Unseal(v1) = %q, want %q", got, plain)
	}
	// New seals must be v2, not v1.
	sealed, err := v.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "v2:") {
		t.Errorf("Seal = %q, want v2: prefix", sealed)
	}
	// A vault opened from a real data dir must also unseal v1 values.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "master.key"), secret, 0o600); err != nil {
		t.Fatal(err)
	}
	vDir, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := vDir.Unseal(v1Sealed); err != nil || string(got) != string(plain) {
		t.Errorf("data-dir vault Unseal(v1) = %q, err = %v", got, err)
	}
}

func TestPassphraseVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	v, err := NewWithPassphrase(dir, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("sk-passphrase-provider-key")
	sealed, err := v.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "v2:") {
		t.Fatalf("passphrase Seal = %q, want v2: prefix", sealed)
	}
	got, err := v.Unseal(sealed)
	if err != nil {
		t.Fatalf("Unseal: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("Unseal = %q, want %q", got, plain)
	}
	// Salt file: 16 bytes, 0600.
	salt, err := os.ReadFile(filepath.Join(dir, "kdf.salt"))
	if err != nil {
		t.Fatalf("kdf.salt was not created: %v", err)
	}
	if len(salt) != 16 {
		t.Errorf("kdf.salt length = %d, want 16", len(salt))
	}
	info, err := os.Stat(filepath.Join(dir, "kdf.salt"))
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("kdf.salt perms = %o, want 0600", info.Mode().Perm())
	}
	// No master.key in passphrase mode.
	if _, err := os.Stat(filepath.Join(dir, "master.key")); !os.IsNotExist(err) {
		t.Error("passphrase vault must not create master.key")
	}
	if _, err := NewWithPassphrase(dir, ""); err == nil {
		t.Error("NewWithPassphrase should reject an empty passphrase")
	}
	if _, err := NewWithPassphrase("", "x"); err == nil {
		t.Error("NewWithPassphrase should reject an empty data directory")
	}
}

func TestPassphraseVaultWrongPassphraseFails(t *testing.T) {
	dir := t.TempDir()
	v, err := NewWithPassphrase(dir, "first passphrase")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v.Seal([]byte("top-secret"))
	if err != nil {
		t.Fatal(err)
	}
	// Same dir (same salt), different passphrase: must fail.
	other, err := NewWithPassphrase(dir, "second passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Unseal(sealed); err == nil {
		t.Error("Unseal with the wrong passphrase must fail")
	}
	// Same passphrase again: must succeed.
	again, err := NewWithPassphrase(dir, "first passphrase")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Unseal(sealed); err != nil || string(got) != "top-secret" {
		t.Errorf("same-passphrase vault Unseal = %q, err = %v", got, err)
	}
}

func TestPassphraseVaultRejectsV1(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	v1Sealed := sealV1(t, secret, []byte("legacy-key"))
	salt := make([]byte, 16)
	v, err := NewFromPassphrase("some passphrase", salt, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.Unseal(v1Sealed)
	if err == nil {
		t.Fatal("passphrase vault must reject a v1 value")
	}
	if !strings.Contains(err.Error(), "non-passphrase") {
		t.Errorf("v1 rejection error = %q, want it to mention non-passphrase vault", err)
	}
	if _, err := NewFromPassphrase("", salt, ""); err == nil {
		t.Error("NewFromPassphrase should reject an empty passphrase")
	}
	if _, err := NewFromPassphrase("x", []byte("short"), ""); err == nil {
		t.Error("NewFromPassphrase should reject a salt that is not 16 bytes")
	}
}

func TestReSealUpgradesV1ToV2(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	v, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("migrate-me")
	v1Sealed := sealV1(t, secret, plain)
	upgraded, err := v.ReSeal(v1Sealed)
	if err != nil {
		t.Fatalf("ReSeal(v1): %v", err)
	}
	if !strings.HasPrefix(upgraded, "v2:") {
		t.Errorf("ReSeal(v1) = %q, want v2: prefix", upgraded)
	}
	if upgraded == v1Sealed {
		t.Error("ReSeal must produce a different value")
	}
	got, err := v.Unseal(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(plain) {
		t.Errorf("Unseal(ReSeal(v1)) = %q, want %q", got, plain)
	}
	// ReSeal of a v2 value is a no-op in format (still v2, still decrypts).
	v2Sealed, err := v.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	again, err := v.ReSeal(v2Sealed)
	if err != nil {
		t.Fatalf("ReSeal(v2): %v", err)
	}
	if !strings.HasPrefix(again, "v2:") {
		t.Errorf("ReSeal(v2) = %q, want v2: prefix", again)
	}
	if got, err := v.Unseal(again); err != nil || string(got) != string(plain) {
		t.Errorf("Unseal(ReSeal(v2)) = %q, err = %v", got, err)
	}
	// ReSeal of garbage fails.
	if _, err := v.ReSeal("not-a-sealed-value"); err == nil {
		t.Error("ReSeal should fail on a malformed value")
	}
}

func TestSaltPersistsAcrossNew(t *testing.T) {
	dir := t.TempDir()
	v1, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := v1.Seal([]byte("stable-key"))
	if err != nil {
		t.Fatal(err)
	}
	salt1, err := os.ReadFile(filepath.Join(dir, "kdf.salt"))
	if err != nil {
		t.Fatal(err)
	}
	// Second New() on the same dir must derive the same key: it must
	// unseal the first vault's value, and the salt file must be unchanged.
	v2, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	salt2, err := os.ReadFile(filepath.Join(dir, "kdf.salt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(salt1, salt2) {
		t.Error("kdf.salt changed between two New() calls on the same dir")
	}
	if got, err := v2.Unseal(sealed); err != nil || string(got) != "stable-key" {
		t.Errorf("reopened vault Unseal = %q, err = %v", got, err)
	}
}

func TestNewFromSecretDeterministicSalt(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	a, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewFromSecret(secret, "")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Seal([]byte("deterministic"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := b.Unseal(sealed); err != nil || string(got) != "deterministic" {
		t.Errorf("same-secret vault Unseal = %q, err = %v", got, err)
	}
}

// TestCryptoDependencyAtSecureVersion is a regression test for the
// golang.org/x/crypto security remediation (Dependabot alerts #1-#17,
// CVE-2024-45337 through CVE-2026-46598). It verifies the go.mod requires
// golang.org/x/crypto at or above the version that patches all reported
// vulnerabilities (v0.59.0). If this test fails, the dependency was
// accidentally downgraded and must be re-upgraded.
func TestCryptoDependencyAtSecureVersion(t *testing.T) {
	const minVersion = "v0.59.0"
	// Locate go.mod by walking up from this test file's directory.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	modPath := ""
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			modPath = filepath.Join(d, "go.mod")
			break
		}
		if d == filepath.Dir(d) {
			break // reached filesystem root
		}
	}
	if modPath == "" {
		t.Fatal("could not locate go.mod by walking up from test directory")
	}
	modBytes, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatalf("cannot read go.mod: %v", err)
	}
	var found string
	for _, line := range strings.Split(string(modBytes), "\n") {
		if strings.Contains(line, "golang.org/x/crypto") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				found = fields[1]
			}
			break
		}
	}
	if found == "" {
		t.Fatal("golang.org/x/crypto not found in go.mod require block")
	}
	if found < minVersion {
		t.Fatalf("golang.org/x/crypto version %q is below the secure minimum %q (Dependabot alerts #1-#17, CVE-2024-45337 through CVE-2026-46598)", found, minVersion)
	}
}
