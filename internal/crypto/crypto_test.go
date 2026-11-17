package crypto

import (
	"os"
	"path/filepath"
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
	if !strings.HasPrefix(sealed, "v1:") {
		t.Fatalf("sealed value %q lacks v1: prefix", sealed)
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
		"v2:AAAA",
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
	if info.Mode().Perm() != 0o600 {
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
