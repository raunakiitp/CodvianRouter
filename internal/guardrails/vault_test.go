package guardrails

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testSealer is a stdlib-only AES-256-GCM sealer for vault
// tests: same shape as crypto.Vault (Seal/Unseal) without the
// Argon2id key derivation, which lives in internal/crypto.
type testSealer struct{ key [32]byte }

func newTestSealer() *testSealer {
	var s testSealer
	copy(s.key[:], []byte("vault-test-sealer-key-32-bytes-long!!"))
	return &s
}

func (s *testSealer) Seal(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return "test:" + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil)), nil
}

func (s *testSealer) Unseal(sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, "test:"))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(s.key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("test sealer: sealed value too short")
	}
	return gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
}

func TestVaultMintRestoreRoundTrip(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	tok, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if !TokenRegexp.MatchString(tok) {
		t.Fatalf("token %q does not match TokenRegexp", tok)
	}
	if got := v.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
	restored, n := v.Restore("reach me at " + tok + " please")
	if n != 1 || restored != "reach me at alice@example.com please" {
		t.Fatalf("Restore = (%q, %d), want (%q, 1)", restored, n, "reach me at alice@example.com please")
	}
}

func TestVaultMintDedupsSameValue(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	a, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint 1: %v", err)
	}
	b, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint 2: %v", err)
	}
	if a != b {
		t.Fatalf("same value minted two tokens: %q vs %q", a, b)
	}
	if got := v.Count(); got != 1 {
		t.Fatalf("Count = %d, want 1", got)
	}
}

func TestVaultMintDistinctValues(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	a, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint email: %v", err)
	}
	b, err := v.Mint("email", "bob@example.com")
	if err != nil {
		t.Fatalf("Mint email 2: %v", err)
	}
	c, err := v.Mint("phone", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint phone: %v", err)
	}
	if a == b || a == c || b == c {
		t.Fatalf("distinct mints collided: %q %q %q", a, b, c)
	}
	if got := v.Count(); got != 3 {
		t.Fatalf("Count = %d, want 3", got)
	}
}

func TestVaultMintEmptyErrors(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	if _, err := v.Mint("", "x"); err == nil {
		t.Error("Mint with empty kind: want error, got nil")
	}
	if _, err := v.Mint("email", ""); err == nil {
		t.Error("Mint with empty value: want error, got nil")
	}
	if _, err := NewVault(nil, "").Mint("email", "x"); err == nil {
		t.Error("Mint with nil sealer: want error, got nil")
	}
	if _, err := (*Vault)(nil).Mint("email", "x"); err == nil {
		t.Error("Mint on nil vault: want error, got nil")
	}
}

func TestVaultRestoreUnknownTokenUntouched(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	forged := "[VAULT:email:0123456789abcdef0123456789abcdef]"
	restored, n := v.Restore("contact " + forged + " now")
	if n != 0 || restored != "contact "+forged+" now" {
		t.Fatalf("Restore forged = (%q, %d), want unchanged, 0", restored, n)
	}
	restored, n = v.Restore("nothing secret here")
	if n != 0 || restored != "nothing secret here" {
		t.Fatalf("Restore clean = (%q, %d), want unchanged, 0", restored, n)
	}
	if restored, n := (*Vault)(nil).Restore("x"); restored != "x" || n != 0 {
		t.Fatalf("Restore nil vault = (%q, %d), want (\"x\", 0)", restored, n)
	}
}

func TestVaultRestoreTamperedEntryUntouched(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	tok, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	m := TokenRegexp.FindStringSubmatch(tok)
	if m == nil {
		t.Fatalf("token %q does not parse", tok)
	}
	// Corrupt the sealed entry in place: the token must survive
	// Restore untouched, never dropped or replaced with garbage.
	v.entries[m[2]] = vaultEntry{Kind: "email", Sealed: "v2:not-base64!!", CreatedUnix: 0}
	restored, n := v.Restore("mail " + tok)
	if n != 0 || restored != "mail "+tok {
		t.Fatalf("Restore tampered = (%q, %d), want token left in place, 0", restored, n)
	}
}

func TestVaultPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pii_vault.json")
	v := NewVault(newTestSealer(), path)
	tok, err := v.Mint("email", "bob@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("vault file missing after Mint: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("vault file mode = %o, want 0600", info.Mode().Perm())
	}
	// A fresh instance over the same file restores the token:
	// persistence across restart.
	v2 := NewVault(newTestSealer(), path)
	if err := v2.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := v2.Count(); got != 1 {
		t.Fatalf("Count after Load = %d, want 1", got)
	}
	restored, n := v2.Restore("echo " + tok)
	if n != 1 || restored != "echo bob@example.com" {
		t.Fatalf("Restore after Load = (%q, %d), want (%q, 1)", restored, n, "echo bob@example.com")
	}
}

func TestVaultLoadMissingFileIsFresh(t *testing.T) {
	v := NewVault(newTestSealer(), filepath.Join(t.TempDir(), "nope.json"))
	if err := v.Load(); err != nil {
		t.Fatalf("Load missing file: %v, want nil", err)
	}
	if got := v.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
}

func TestVaultLoadCorruptFileFails(t *testing.T) {
	dir := t.TempDir()
	garbage := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(garbage, []byte("this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewVault(newTestSealer(), garbage).Load(); err == nil {
		t.Error("Load corrupt file: want error, got nil")
	}
	wrongVersion := filepath.Join(dir, "v2.json")
	if err := os.WriteFile(wrongVersion, []byte(`{"version":2,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewVault(newTestSealer(), wrongVersion).Load(); err == nil {
		t.Error("Load version 2 file: want error, got nil")
	}
}

func TestRedactWithVault(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	text := "Reach alice@example.com or +1 415-555-0132, key is api_key=sk-abcdefgh12345678"
	redacted, minted := RedactWithVault(text, v)
	if minted != 2 {
		t.Fatalf("minted = %d, want 2 (email + phone)", minted)
	}
	if strings.Contains(redacted, "alice@example.com") || strings.Contains(redacted, "+1 415-555-0132") {
		t.Fatalf("redacted still contains PII: %q", redacted)
	}
	if !strings.Contains(redacted, "[VAULT:email:") || !strings.Contains(redacted, "[VAULT:phone:") {
		t.Fatalf("redacted has no vault tokens: %q", redacted)
	}
	// Secrets are never minted: the key keeps its dead placeholder.
	if !strings.Contains(redacted, "[REDACTED:SECRET]") {
		t.Fatalf("secret lost its placeholder: %q", redacted)
	}
	if strings.Contains(redacted, "sk-abcdefgh12345678") {
		t.Fatalf("secret value leaked into redacted text: %q", redacted)
	}
	restored, n := v.Restore(redacted)
	if n != 2 || restored != "Reach alice@example.com or +1 415-555-0132, key is [REDACTED:SECRET]" {
		t.Fatalf("Restore = (%q, %d), want originals back with the secret still redacted", restored, n)
	}
}

func TestRedactWithVaultValidatedCard(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	// Valid Luhn card mints; a Luhn-failing run is an order
	// number, not a card, and survives untouched.
	text := "Card 4242424242424242 and order 4242424242424241"
	redacted, minted := RedactWithVault(text, v)
	if minted != 1 {
		t.Fatalf("minted = %d, want 1 (only the valid card)", minted)
	}
	if !strings.Contains(redacted, "[VAULT:card:") {
		t.Fatalf("valid card not minted: %q", redacted)
	}
	if !strings.Contains(redacted, "4242424242424241") {
		t.Fatalf("invalid card was rewritten: %q", redacted)
	}
	restored, n := v.Restore(redacted)
	if n != 1 || restored != text {
		t.Fatalf("Restore = (%q, %d), want original text, 1", restored, n)
	}
}

func TestRedactWithVaultNilVaultEqualsRedact(t *testing.T) {
	text := "Reach alice@example.com or +1 415-555-0132"
	redacted, minted := RedactWithVault(text, nil)
	if minted != 0 {
		t.Fatalf("minted = %d with nil vault, want 0", minted)
	}
	if redacted != Redact(text) {
		t.Fatalf("RedactWithVault(nil) = %q, Redact = %q", redacted, Redact(text))
	}
}

func TestRedactWithVaultCleanText(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	redacted, minted := RedactWithVault("hello, no secrets here", v)
	if minted != 0 || redacted != "hello, no secrets here" {
		t.Fatalf("clean text: (%q, %d), want unchanged, 0", redacted, minted)
	}
	if got := v.Count(); got != 0 {
		t.Fatalf("Count = %d after clean text, want 0", got)
	}
}
