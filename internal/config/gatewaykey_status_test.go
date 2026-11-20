package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Gateway-key expiry + per-key traffic limits (CHECKLIST 195): an
// expired key never verifies, the expiry is named (not a generic
// auth failure), limits validate, and expiry + limits survive a
// config save/load round-trip with a deep-copied Clone.

func TestGatewayKeyExpired(t *testing.T) {
	key, _, err := IssueKey("exp", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if key.Expired(time.Now()) {
		t.Error("a key with no expiry must never be expired")
	}
	past := time.Now().Add(-time.Hour)
	key.ExpiresAt = &past
	if !key.Expired(time.Now()) {
		t.Error("a key whose expiry is in the past must be expired")
	}
	future := time.Now().Add(time.Hour)
	key.ExpiresAt = &future
	if key.Expired(time.Now()) {
		t.Error("a key whose expiry is in the future must not be expired")
	}
	// Expiry is inclusive: at exactly ExpiresAt the key is dead.
	at := time.Now()
	key.ExpiresAt = &at
	if !key.Expired(at) {
		t.Error("a key at exactly its expiry must be expired")
	}
}

func TestConfigVerifyGatewayKeyExpired(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	live, rawLive, err := IssueKey("live", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	dead, rawDead, err := IssueKey("dead", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	dead.ExpiresAt = &past
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	live.ExpiresAt = &future
	if err := cfg.AddGatewayKey(live); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(dead); err != nil {
		t.Fatal(err)
	}
	// The live key is not expired, even though it has an expiry.
	if _, expired := cfg.VerifyGatewayKeyExpired(rawLive); expired {
		t.Error("a live key must not report expired")
	}
	// The dead key names its expiry.
	exp, expired := cfg.VerifyGatewayKeyExpired(rawDead)
	if !expired {
		t.Fatal("an expired key must report expired")
	}
	if !exp.Equal(past) {
		t.Errorf("expiry = %v, want %v", exp, past)
	}
	// An unknown raw value is not an expired key.
	if _, expired := cfg.VerifyGatewayKeyExpired("crk_" + strings.Repeat("A", 32)); expired {
		t.Error("an unknown key must not report expired")
	}
	// VerifyGatewayKey itself still matches the dead key (the
	// server's verifier applies the expiry verdict on top).
	if _, ok := cfg.VerifyGatewayKey(rawDead); !ok {
		t.Error("VerifyGatewayKey must still match an expired key's hash (expiry is enforced by the verifier, not the lookup)")
	}
}

func TestGatewayKeyLimitsValidate(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	key, _, err := IssueKey("lim", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	key.RequestsPerMinute = -1
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "requests_per_minute") {
		t.Errorf("Validate must reject a negative requests_per_minute, got %v", err)
	}
	// AddGatewayKey copies the record, so mutate the stored copy.
	cfg.GatewayKeys[0].RequestsPerMinute = 0
	cfg.GatewayKeys[0].TokensPerMinute = -5
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tokens_per_minute") {
		t.Errorf("Validate must reject a negative tokens_per_minute, got %v", err)
	}
	cfg.GatewayKeys[0].TokensPerMinute = 0
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate must accept zero (unlimited) limits, got %v", err)
	}
}

func TestGatewayKeyExpiryLimitsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig(dir)
	key, _, err := IssueKey("rt", []string{"chat"}, 1.5)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(48 * time.Hour).Truncate(time.Second).UTC()
	key.ExpiresAt = &exp
	key.RequestsPerMinute = 60
	key.TokensPerMinute = 10000
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.FindGatewayKey(key.ID)
	if !ok {
		t.Fatal("reloaded config lost the key")
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, exp)
	}
	if got.RequestsPerMinute != 60 || got.TokensPerMinute != 10000 {
		t.Errorf("limits = %d/%d, want 60/10000", got.RequestsPerMinute, got.TokensPerMinute)
	}
	if got.BudgetUSD != 1.5 || got.Name != "rt" {
		t.Errorf("round-trip lost fields: %+v", got)
	}
	// A clone must not share the expiry pointer with the original.
	clone := cfg.Clone()
	orig, _ := cfg.FindGatewayKey(key.ID)
	cloneKey, _ := clone.FindGatewayKey(key.ID)
	moved := exp.Add(time.Hour)
	cloneKey.ExpiresAt = &moved
	if orig.ExpiresAt.Equal(moved) {
		t.Error("Clone shared the ExpiresAt pointer with the original — mutating the clone moved the live key's expiry")
	}
}
