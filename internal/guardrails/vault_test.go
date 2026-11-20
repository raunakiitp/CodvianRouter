package guardrails

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
	// Windows does not enforce Unix permission bits; skip the mode check there.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
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

func TestStreamRestorerSplitToken(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	tok, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	full := "reach me at " + tok + " please"
	r := NewStreamRestorer(v)
	cut1 := strings.Index(full, tok) + len("[VAULT:ema")
	cut2 := strings.Index(full, tok) + len(tok) - 8
	var out strings.Builder
	out.WriteString(r.WriteChunk(full[:cut1]))
	out.WriteString(r.WriteChunk(full[cut1:cut2]))
	out.WriteString(r.WriteChunk(full[cut2:]))
	out.WriteString(r.Flush())
	if got := out.String(); got != "reach me at alice@example.com please" {
		t.Fatalf("streamed restore = %q, want %q", got, "reach me at alice@example.com please")
	}
	if got := r.Restored(); got != 1 {
		t.Fatalf("Restored = %d, want 1", got)
	}
}

func TestStreamRestorerSplitMarker(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	tok, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	full := "mail " + tok + "!"
	i := strings.Index(full, tok)
	r := NewStreamRestorer(v)
	var out strings.Builder
	out.WriteString(r.WriteChunk(full[:i+3]))
	out.WriteString(r.WriteChunk(full[i+3:]))
	out.WriteString(r.Flush())
	if got := out.String(); got != "mail alice@example.com!" {
		t.Fatalf("split marker restore = %q, want %q", got, "mail alice@example.com!")
	}
	if got := r.Restored(); got != 1 {
		t.Fatalf("Restored = %d, want 1", got)
	}
}

func TestStreamRestorerWholeTokenPerChunk(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	tok, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	r := NewStreamRestorer(v)
	if got := r.WriteChunk("mail " + tok); got != "mail alice@example.com" {
		t.Fatalf("WriteChunk whole token = %q, want restored", got)
	}
	if got := r.Flush(); got != "" {
		t.Fatalf("Flush after whole token = %q, want empty", got)
	}
	if got := r.Restored(); got != 1 {
		t.Fatalf("Restored = %d, want 1", got)
	}
}

func TestStreamRestorerUnknownTokenSplitUntouched(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	forged := "[VAULT:email:0123456789abcdef0123456789abcdef]"
	full := "contact " + forged + " now"
	r := NewStreamRestorer(v)
	cut := len("contact [VAULT:email:012345")
	var out strings.Builder
	out.WriteString(r.WriteChunk(full[:cut]))
	out.WriteString(r.WriteChunk(full[cut:]))
	out.WriteString(r.Flush())
	if got := out.String(); got != full {
		t.Fatalf("forged split token = %q, want unchanged %q", got, full)
	}
	if got := r.Restored(); got != 0 {
		t.Fatalf("Restored = %d, want 0", got)
	}
}

func TestStreamRestorerLiteralPrefixReleased(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	r := NewStreamRestorer(v)
	if got := r.WriteChunk("see [VAULT: note"); got != "see " {
		t.Fatalf("WriteChunk literal prefix = %q, want %q", got, "see ")
	}
	if got := r.Flush(); got != "[VAULT: note" {
		t.Fatalf("Flush literal prefix = %q, want %q", got, "[VAULT: note")
	}
	long := "[VAULT:" + strings.Repeat("x", 200)
	if got := r.WriteChunk(long); got != long {
		t.Fatalf("WriteChunk overlong prefix = %q, want released unchanged", got)
	}
	if got := r.Flush(); got != "" {
		t.Fatalf("Flush after overlong prefix = %q, want empty", got)
	}
}

func TestStreamRestorerNilVault(t *testing.T) {
	r := NewStreamRestorer(nil)
	text := "mail [VAULT:email:0123456789abcdef0123456789abcdef]!"
	if got := r.WriteChunk(text); got != text {
		t.Fatalf("nil-vault WriteChunk = %q, want pass-through", got)
	}
	if got := r.Flush(); got != "" {
		t.Fatalf("nil-vault Flush = %q, want empty", got)
	}
	if got := r.Restored(); got != 0 {
		t.Fatalf("nil-vault Restored = %d, want 0", got)
	}
	if got := (*StreamRestorer)(nil).WriteChunk("x"); got != "x" {
		t.Fatalf("nil-restorer WriteChunk = %q, want %q", got, "x")
	}
	if got := (*StreamRestorer)(nil).Flush(); got != "" {
		t.Fatalf("nil-restorer Flush = %q, want empty", got)
	}
}

func TestStreamRestorerMultipleTokens(t *testing.T) {
	v := NewVault(newTestSealer(), "")
	a, err := v.Mint("email", "alice@example.com")
	if err != nil {
		t.Fatalf("Mint a: %v", err)
	}
	b, err := v.Mint("email", "bob@example.com")
	if err != nil {
		t.Fatalf("Mint b: %v", err)
	}
	full := "a=" + a + " b=" + b + " end"
	r := NewStreamRestorer(v)
	cut := strings.Index(full, b) + 20
	var out strings.Builder
	out.WriteString(r.WriteChunk(full[:cut]))
	out.WriteString(r.WriteChunk(full[cut:]))
	out.WriteString(r.Flush())
	if got := out.String(); got != "a=alice@example.com b=bob@example.com end" {
		t.Fatalf("multiple tokens = %q, want %q", got, "a=alice@example.com b=bob@example.com end")
	}
	if got := r.Restored(); got != 2 {
		t.Fatalf("Restored = %d, want 2", got)
	}
}
