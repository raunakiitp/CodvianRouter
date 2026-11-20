// Reversible PII token vault (B3-19, PARITY.md:782).
//
// The vault is the opt-in counterpart to plain redaction: where Redact
// replaces a PII match with a dead placeholder ([REDACTED:EMAIL]), the
// vault mints a token ([VAULT:email:<id>]) that the gateway can swap
// back for the original value at exit — tokens minted pre-call,
// restored at exit. The provider only ever sees tokens, never the
// PII itself; the client that sent the PII gets its own values back.
//
// Design rules (deliberate, original — capability only, no copied
// implementation):
//
//   - PII kinds only. Secrets (API keys, bearer tokens, private-key
//     blocks, generic token shapes) and injection phrases are never
//     minted: a restorable secret is a leak waiting for a prompt
//     that echoes it, so they keep their dead placeholders.
//   - The sealed value never leaves the vault file. The token
//     carries only the PII kind label and a random id — no value,
//     no hash of the value. A forged token with an unknown id is
//     left untouched by Restore.
//   - Encryption is injected, not built here: this package is a
//     leaf (stdlib only, no internal imports), so the caller
//     supplies a Sealer. crypto.Vault implements Sealer exactly
//     (Seal/Unseal), so production sealing is Argon2id + AES-256-GCM
//     under the router's master key, and the vault file holds only
//     sealed values. A nil Sealer fails Mint closed.
//   - Persistence is an atomic JSON file (0600) in the data dir,
//     one entry per token id. Mint saves on every mint: a token the
//     provider echoed back must survive a restart, or the restore
//     at exit silently drops it. A failed save rolls the mint back
//     in memory, so no token is ever handed out that is not on disk.
//   - Same value minted twice in one process maps to one token
//     (in-memory dedup), so the provider sees a consistent token
//     for one entity. After a restart the same value mints a fresh
//     token — the old token still restores, so nothing breaks.
//   - Restore is best-effort per token: an unknown id or an
//     unsealable entry is left in place and counted as unrestored,
//     never dropped, so a response is never silently corrupted.
package guardrails

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Sealer encrypts one value for storage and decrypts it back.
// crypto.Vault implements this interface exactly (Seal/Unseal).
type Sealer interface {
	Seal(plaintext []byte) (string, error)
	Unseal(sealed string) ([]byte, error)
}

// TokenRegexp matches one vault token: [VAULT:<kind>:<32 hex id>].
// Kind labels are the PII pattern labels (email, phone-intl,
// ssn, ...): lowercase letters, digits and hyphens only.
var TokenRegexp = regexp.MustCompile(`\[VAULT:([a-z0-9-]+):([0-9a-f]{32})\]`)

// tokenString renders one vault token for a kind + id.
func tokenString(kind, id string) string {
	return "[VAULT:" + kind + ":" + id + "]"
}

// vaultEntry is one persisted token: the PII kind, the sealed
// original value, and when it was minted (Unix seconds, for a
// future pruning pass — nothing prunes yet).
type vaultEntry struct {
	Kind        string `json:"kind"`
	Sealed      string `json:"sealed"`
	CreatedUnix int64  `json:"created_unix"`
}

// vaultFile is the on-disk shape of the vault: versioned, one
// entry per token id.
type vaultFile struct {
	Version int                   `json:"version"`
	Entries map[string]vaultEntry `json:"entries"`
}

// Vault is the reversible PII token vault. The zero value is
// unusable (no sealer, no entries); use NewVault. A Vault is
// safe for concurrent use.
type Vault struct {
	mu      sync.Mutex
	sealer  Sealer
	path    string // "" = memory-only, nothing is persisted
	entries map[string]vaultEntry
	byValue map[string]string // kind + "\x00" + value -> token id
}

// NewVault returns a Vault sealing with sealer and persisting to
// path ("" = memory-only). Entries are not loaded: call Load
// after construction when path names an existing vault file.
func NewVault(sealer Sealer, path string) *Vault {
	return &Vault{
		sealer:  sealer,
		path:    path,
		entries: map[string]vaultEntry{},
		byValue: map[string]string{},
	}
}

// Load reads persisted entries from the vault file. A missing
// file is a fresh vault (nil error). A corrupt file fails loudly:
// silently dropping sealed PII would strand tokens the provider
// already echoed back.
func (v *Vault) Load() error {
	if v == nil || v.path == "" {
		return nil
	}
	data, err := os.ReadFile(v.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("guardrails: cannot read vault file %q: %w", v.path, err)
	}
	var file vaultFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("guardrails: vault file %q is corrupt: %w", v.path, err)
	}
	if file.Version != 1 {
		return fmt.Errorf("guardrails: vault file %q has unsupported version %d (want 1)", v.path, file.Version)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for id, e := range file.Entries {
		if id == "" || e.Kind == "" || e.Sealed == "" {
			continue
		}
		v.entries[id] = e
	}
	return nil
}

// Mint seals value under kind and returns its token. The same
// (kind, value) minted twice in this process returns the same
// token. Mint fails closed: no sealer, an empty kind/value, a
// seal failure or a persist failure all return an error and no
// token — the caller must fall back to the dead placeholder, so
// a mint failure can never leak the value.
func (v *Vault) Mint(kind, value string) (string, error) {
	if v == nil {
		return "", errors.New("guardrails: nil vault cannot mint")
	}
	if v.sealer == nil {
		return "", errors.New("guardrails: vault has no sealer, cannot mint")
	}
	if kind == "" || value == "" {
		return "", errors.New("guardrails: vault mint needs a kind and a value")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	key := kind + "\x00" + value
	if id, ok := v.byValue[key]; ok {
		return tokenString(kind, id), nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("guardrails: vault token id: %w", err)
	}
	id := hex.EncodeToString(raw[:])
	sealed, err := v.sealer.Seal([]byte(value))
	if err != nil {
		return "", fmt.Errorf("guardrails: vault seal failed: %w", err)
	}
	v.entries[id] = vaultEntry{Kind: kind, Sealed: sealed, CreatedUnix: time.Now().Unix()}
	v.byValue[key] = id
	if err := v.saveLocked(); err != nil {
		delete(v.entries, id)
		delete(v.byValue, key)
		return "", err
	}
	return tokenString(kind, id), nil
}

// tokenFor mints match under pattern and returns the token and
// true. On any mint failure it returns the dead placeholder and
// false, so a mint failure degrades to plain redaction and can
// never leak the value.
func (v *Vault) tokenFor(pattern, match, placeholder string) (string, bool) {
	tok, err := v.Mint(pattern, match)
	if err != nil {
		return placeholder, false
	}
	return tok, true
}

// Restore swaps every known vault token in text back for its
// original value and reports how many tokens were restored.
// Unknown ids and unsealable entries are left in place — a
// response is never corrupted by a token that cannot be opened.
// A nil vault restores nothing.
func (v *Vault) Restore(text string) (string, int) {
	if v == nil || text == "" || !strings.Contains(text, "[VAULT:") {
		return text, 0
	}
	locs := TokenRegexp.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return text, 0
	}
	var b strings.Builder
	b.Grow(len(text))
	last := 0
	restored := 0
	for _, loc := range locs {
		id := text[loc[4]:loc[5]]
		v.mu.Lock()
		entry, ok := v.entries[id]
		v.mu.Unlock()
		if !ok {
			continue
		}
		plain, err := v.sealer.Unseal(entry.Sealed)
		if err != nil {
			continue
		}
		b.WriteString(text[last:loc[0]])
		b.Write(plain)
		last = loc[1]
		restored++
	}
	if restored == 0 {
		return text, 0
	}
	b.WriteString(text[last:])
	return b.String(), restored
}

// maxStreamTokenLen bounds the trailing fragment a
// StreamRestorer holds for the next chunk: a vault token is
// "[VAULT:" + kind + ":" + 32 hex + "]", and kind labels are
// short PII labels ("email", "phone-intl", ...), so a real token
// is under 64 bytes; 128 leaves wide headroom. A "[VAULT:"
// fragment longer than this without a closing bracket cannot
// be a token, so it is released instead of buffering the rest
// of the stream behind a literal "[VAULT:".
const maxStreamTokenLen = 128

// StreamRestorer reassembles vault tokens split across streamed
// chunks and restores them on the fly (B3-19, :782 follow-up).
//
// A token minted pre-call can arrive split across SSE chunk
// boundaries — even inside the "[VAULT:" marker itself — so
// restoring each chunk in isolation would leave the token text
// in the delivered stream. The restorer holds a trailing
// partial token (or a trailing partial "[VAULT:" marker) for
// the next chunk and restores complete tokens as they close.
// An unknown id, an unsealable entry, or a fragment that never
// completes is emitted unchanged by Flush, never dropped.
//
// One StreamRestorer serves one stream; it is not safe for
// concurrent use. A nil vault passes text through unchanged.
type StreamRestorer struct {
	v        *Vault
	buf      string // held trailing text: partial token or marker
	restored int
}

// NewStreamRestorer returns a StreamRestorer for one stream
// bound to v. A nil v (vault off) yields a pass-through.
func NewStreamRestorer(v *Vault) *StreamRestorer {
	return &StreamRestorer{v: v}
}

// WriteChunk feeds one stream delta and returns the text safe to
// emit now: complete tokens are restored, and a trailing partial
// token (or partial "[VAULT:" marker) is held for the next
// chunk.
func (r *StreamRestorer) WriteChunk(chunk string) string {
	if chunk == "" {
		return ""
	}
	if r == nil || r.v == nil {
		return chunk
	}
	text := r.buf + chunk
	r.buf = ""
	hold := streamHoldLen(text)
	if hold == 0 {
		restored, n := r.v.Restore(text)
		r.restored += n
		return restored
	}
	head, tail := text[:len(text)-hold], text[len(text)-hold:]
	restored, n := r.v.Restore(head)
	r.restored += n
	r.buf = tail
	return restored
}

// Flush ends the stream: any held tail is restored if it
// completed (or emitted unchanged if it never did — an
// unknown/incomplete token is left in place, never dropped)
// and returned. Call exactly once, after the last WriteChunk.
func (r *StreamRestorer) Flush() string {
	if r == nil || r.buf == "" {
		return ""
	}
	tail := r.buf
	r.buf = ""
	if r.v == nil {
		return tail
	}
	restored, n := r.v.Restore(tail)
	r.restored += n
	return restored
}

// Restored reports how many tokens this restorer has restored so
// far, including any restored by Flush.
func (r *StreamRestorer) Restored() int {
	if r == nil {
		return 0
	}
	return r.restored
}

// streamHoldLen returns how many trailing bytes of text must be
// held for the next chunk because they may be the start of a
// vault token: either a "[VAULT:" fragment with no closing
// bracket yet (short enough to be a real token), or a trailing
// partial "[VAULT:" marker. Zero means the whole text is safe
// to restore and emit now.
func streamHoldLen(text string) int {
	const marker = "[VAULT:"
	if i := strings.LastIndex(text, marker); i >= 0 {
		tail := text[i:]
		if !strings.Contains(tail, "]") && len(tail) <= maxStreamTokenLen {
			return len(tail)
		}
	}
	max := len(marker) - 1
	if len(text) < max {
		max = len(text)
	}
	for k := max; k > 0; k-- {
		if strings.HasPrefix(marker, text[len(text)-k:]) {
			return k
		}
	}
	return 0
}

// Count reports how many tokens the vault holds (persisted
// entries included). It never unseals anything.
func (v *Vault) Count() int {
	if v == nil {
		return 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.entries)
}

// saveLocked writes the vault file atomically (temp file in the
// same directory + rename, 0600). The caller holds v.mu. A
// memory-only vault (path "") saves nothing.
func (v *Vault) saveLocked() error {
	if v.path == "" {
		return nil
	}
	dir := filepath.Dir(v.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("guardrails: cannot create vault directory %q: %w", dir, err)
	}
	data, err := json.Marshal(vaultFile{Version: 1, Entries: v.entries})
	if err != nil {
		return fmt.Errorf("guardrails: cannot encode vault: %w", err)
	}
	tmp := filepath.Join(dir, ".pii_vault.json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("guardrails: cannot write vault file %q: %w", v.path, err)
	}
	if err := os.Rename(tmp, v.path); err != nil {
		return fmt.Errorf("guardrails: cannot publish vault file %q: %w", v.path, err)
	}
	return nil
}

// RedactWithVault is Redact with reversible PII tokens: every
// PII match is minted into the vault and replaced by its token,
// while secrets and injections keep their dead placeholders (see
// the package comment — a restorable secret is a leak). It
// returns the rewritten text and how many tokens were minted. A
// nil vault behaves exactly like Redact.
func RedactWithVault(text string, v *Vault) (string, int) {
	if v == nil {
		return Redact(text), 0
	}
	out, _, minted := scanVault(text, v)
	return out, minted
}
