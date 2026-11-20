// Multi-key pooling (CHECKLIST 175): one provider may hold several API
// keys — a pool — instead of a single key. The router tries the
// preferred keys first, fails over to the next key in the pool on
// 401/429/quota, and holds reserve keys back until every preferred
// key is exhausted. A dead key is cooled down per-key, never taking
// the whole provider out (see internal/router: per-key cooldowns,
// breaker-neutral 4xx per CHECKLIST 151).
//
// A ProviderKey is one credential in the pool. The ciphertext is
// stored exactly like the provider's legacy single key
// (AES-256-GCM via the registry cipher); the fingerprint is the
// first 12 hex characters of SHA-256(plaintext) — the same
// KeyFingerprint the warm-pool feed already serves — so the
// dashboard can tell keys apart without ever carrying key material.
// Role is operator-marked: "preferred" (tried first, in pool order)
// or "reserve" (held back until all preferred keys are exhausted).
package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Key roles in a provider's pool.
const (
	// KeyRolePreferred is a key the router tries first, in pool
	// order, before any reserve key.
	KeyRolePreferred = "preferred"
	// KeyRoleReserve is a key held back until every preferred key
	// of the provider is exhausted for the request.
	KeyRoleReserve = "reserve"
)

// IsKeyRole reports whether s names a known key role.
func IsKeyRole(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case KeyRolePreferred, KeyRoleReserve:
		return true
	}
	return false
}

// NormalizeKeyRole lowercases/trims s and resolves "" to preferred
// (the default role for a newly added key). Unknown names also
// resolve to preferred — the registry never errors on a role name;
// config validation rejects unknown names at load time instead.
func NormalizeKeyRole(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case KeyRoleReserve:
		return KeyRoleReserve
	default:
		return KeyRolePreferred
	}
}

// ProviderKey is one API key in a provider's pool.
type ProviderKey struct {
	// ID is the stable key identity ("key_" + 16 hex chars). It
	// is what the ledger attributes spend to and what the
	// router's per-key cooldowns are keyed on — never the key
	// itself, never the fingerprint.
	ID string `json:"id"`
	// Encrypted is the AES-256-GCM sealed key (same cipher as
	// the provider's legacy APIKeyEncrypted).
	Encrypted string `json:"api_key_encrypted"`
	// Role is "preferred" or "reserve" (see KeyRolePreferred /
	// KeyRoleReserve). "" reads as preferred for rows written
	// before roles existed.
	Role string `json:"role,omitempty"`
	// Fingerprint is KeyFingerprint(plaintext) — first 12 hex
	// of SHA-256 — computed once at add time, so the dashboard
	// can tell keys apart without key material.
	Fingerprint string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// NewProviderKeyID returns a fresh provider-key ID
// ("key_" + 16 hex chars from crypto/rand).
func NewProviderKeyID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially impossible on a
		// usable system; fall back to a time-based value rather
		// than returning an empty ID.
		return fmt.Sprintf("key_%d", time.Now().UnixNano())
	}
	return "key_" + hex.EncodeToString(b[:])
}

// Clone returns a deep copy of the key.
func (k ProviderKey) Clone() ProviderKey { return k }

// EffectiveRole returns the key's role, resolving "" (rows written
// before roles existed) to preferred.
func (k ProviderKey) EffectiveRole() string {
	if k.Role == "" {
		return KeyRolePreferred
	}
	return NormalizeKeyRole(k.Role)
}

// IsReserve reports whether the key is held back until every
// preferred key is exhausted.
func (k ProviderKey) IsReserve() bool {
	return k.EffectiveRole() == KeyRoleReserve
}

// legacyKeyID derives the stable ID for a provider's legacy
// single key (stored before pools existed) from its ciphertext:
// the ciphertext is stable across restarts for a stored value, so
// the ID is stable too, and it carries no key material. A
// re-sealed key (rotation) gets a new ID — honestly a new stored
// credential, so its ledger history starts fresh instead of
// silently merging into the old key's spend.
func legacyKeyID(ciphertext string) string {
	sum := sha256.Sum256([]byte(ciphertext))
	return "key_" + hex.EncodeToString(sum[:])[:12]
}

// EffectiveKeys returns the provider's key pool in pool order:
// the stored Keys when the provider has any, otherwise a single
// preferred key synthesized from the legacy APIKeyEncrypted (a
// provider stored before pools existed). The returned slice is a
// copy — callers cannot mutate registry state through it. A
// provider with no stored key at all (empty legacy key, e.g. a
// no-auth local provider) returns an empty pool: there is no key
// to pool, and the router's legacy single-client path still
// applies (ClientFor with an empty key).
func (p *Provider) EffectiveKeys() []ProviderKey {
	if p == nil {
		return nil
	}
	if len(p.Keys) > 0 {
		out := make([]ProviderKey, len(p.Keys))
		copy(out, p.Keys)
		return out
	}
	if p.APIKeyEncrypted == "" {
		return nil
	}
	return []ProviderKey{{
		ID:        legacyKeyID(p.APIKeyEncrypted),
		Encrypted: p.APIKeyEncrypted,
		Role:      KeyRolePreferred,
		CreatedAt: p.CreatedAt,
	}}
}

// PrimaryKey returns the key ClientFor serves by default: the
// first preferred key in pool order, or the first key when the
// pool holds only reserve keys (an all-reserve pool still has to
// serve — its keys are simply tried after an empty preferred
// pass). False when the provider holds no key at all.
func (p *Provider) PrimaryKey() (ProviderKey, bool) {
	keys := p.EffectiveKeys()
	if len(keys) == 0 {
		return ProviderKey{}, false
	}
	for _, k := range keys {
		if !k.IsReserve() {
			return k, true
		}
	}
	return keys[0], true
}

// KeyByID returns the pool key with id, or false when the provider
// holds no such key. The legacy single key is found by its derived
// ID (see legacyKeyID).
func (p *Provider) KeyByID(id string) (ProviderKey, bool) {
	for _, k := range p.EffectiveKeys() {
		if k.ID == id {
			return k, true
		}
	}
	return ProviderKey{}, false
}

// PreferredKeys returns the pool's preferred keys in pool order
// (the keys the router tries first).
func (p *Provider) PreferredKeys() []ProviderKey {
	var out []ProviderKey
	for _, k := range p.EffectiveKeys() {
		if !k.IsReserve() {
			out = append(out, k)
		}
	}
	return out
}

// ReserveKeys returns the pool's reserve keys in pool order (held
// back until every preferred key is exhausted).
func (p *Provider) ReserveKeys() []ProviderKey {
	var out []ProviderKey
	for _, k := range p.EffectiveKeys() {
		if k.IsReserve() {
			out = append(out, k)
		}
	}
	return out
}
