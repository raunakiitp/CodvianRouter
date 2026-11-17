// Package config — gateway keys.
//
// A gateway key is the credential a local client presents to the router
// (e.g. as "Authorization: Bearer <key>"). The router stores only the
// SHA-256 hash of the key, never the key itself; the raw key is shown
// exactly once, when it is issued.
//
// Raw key format: "crk_" followed by 32 characters drawn from
// [A-Za-z0-9] using crypto/rand. Record IDs look like "gk_<16 hex chars>".
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// GatewayKey is one stored gateway credential. KeyHash is the hex-encoded
// SHA-256 of the raw key; the raw key itself is never persisted.
type GatewayKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	KeyHash   string    `json:"key_hash"`
	Scopes    []string  `json:"scopes"`
	BudgetUSD float64   `json:"budget_usd"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	// GatewayKeyPrefix is prepended to every raw gateway key.
	GatewayKeyPrefix = "crk_"
	// gatewayKeyRandomLen is the number of random characters after the prefix.
	gatewayKeyRandomLen = 32
)

var gatewayKeyAlphabet = []rune("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")

// HashKey returns the hex-encoded SHA-256 of a raw gateway key.
func HashKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// randomString returns n characters drawn uniformly from gatewayKeyAlphabet
// using crypto/rand (rejection sampling, so no modulo bias).
func randomString(n int) (string, error) {
	out := make([]rune, n)
	max := big.NewInt(int64(len(gatewayKeyAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("config: cannot generate random key material: %w", err)
		}
		out[i] = gatewayKeyAlphabet[v.Int64()]
	}
	return string(out), nil
}

// IssueKey creates a new gateway key record and returns it together with
// the raw key. The raw key is returned exactly once — callers must show or
// store it now, because only the hash is kept in the returned record.
// name is a human label (e.g. "default"); scopes is a list of permission
// strings (e.g. "chat", "models"); budgetUSD is this key's own spend cap
// in USD (0 = no per-key cap; the global Config.BudgetUSD still applies).
func IssueKey(name string, scopes []string, budgetUSD float64) (GatewayKey, string, error) {
	if name == "" {
		return GatewayKey{}, "", errors.New("config: gateway key name must not be empty")
	}
	if budgetUSD < 0 {
		return GatewayKey{}, "", fmt.Errorf("config: gateway key budget must not be negative (got %v)", budgetUSD)
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return GatewayKey{}, "", fmt.Errorf("config: cannot generate key id: %w", err)
	}
	secret, err := randomString(gatewayKeyRandomLen)
	if err != nil {
		return GatewayKey{}, "", err
	}
	raw := GatewayKeyPrefix + secret
	key := GatewayKey{
		ID:        "gk_" + hex.EncodeToString(idBytes),
		Name:      name,
		KeyHash:   HashKey(raw),
		Scopes:    append([]string(nil), scopes...),
		BudgetUSD: budgetUSD,
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	return key, raw, nil
}

// Verify reports whether raw is the key this record was issued for, using
// a constant-time comparison of the SHA-256 hashes.
func (k GatewayKey) Verify(raw string) bool {
	if k.KeyHash == "" || raw == "" {
		return false
	}
	want, err := hex.DecodeString(k.KeyHash)
	if err != nil || len(want) != sha256.Size {
		return false
	}
	got := sha256.Sum256([]byte(raw))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// HasScope reports whether this key carries the given scope.
func (k GatewayKey) HasScope(scope string) bool {
	for _, s := range k.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// AddGatewayKey appends key to the config's gateway key list, rejecting a
// duplicate ID.
func (c *Config) AddGatewayKey(key GatewayKey) error {
	for _, existing := range c.GatewayKeys {
		if existing.ID == key.ID {
			return fmt.Errorf("config: gateway key id %q already exists", key.ID)
		}
	}
	c.GatewayKeys = append(c.GatewayKeys, key)
	return nil
}

// FindGatewayKey returns the key with the given ID, if present.
func (c *Config) FindGatewayKey(id string) (GatewayKey, bool) {
	for _, k := range c.GatewayKeys {
		if k.ID == id {
			return k, true
		}
	}
	return GatewayKey{}, false
}

// VerifyGatewayKey checks raw against every stored gateway key and returns
// the matching key, if any. This is the auth check the server calls per
// request: auth is ON whenever at least one key exists, and a request with
// no matching key is rejected.
func (c *Config) VerifyGatewayKey(raw string) (GatewayKey, bool) {
	for _, k := range c.GatewayKeys {
		if k.Verify(raw) {
			return k, true
		}
	}
	return GatewayKey{}, false
}
