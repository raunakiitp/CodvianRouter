package config

import (
	"strings"
	"testing"
)

func TestIssueKeyFormat(t *testing.T) {
	key, raw, err := IssueKey("test", []string{"chat", "models"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, GatewayKeyPrefix) {
		t.Fatalf("raw key %q lacks prefix %q", raw, GatewayKeyPrefix)
	}
	secret := strings.TrimPrefix(raw, GatewayKeyPrefix)
	if len(secret) != 32 {
		t.Fatalf("random part length = %d, want 32", len(secret))
	}
	for _, r := range secret {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			t.Fatalf("raw key contains non-alphanumeric character %q", r)
		}
	}
	if !strings.HasPrefix(key.ID, "gk_") {
		t.Fatalf("key ID %q lacks gk_ prefix", key.ID)
	}
	if key.Name != "test" {
		t.Errorf("Name = %q", key.Name)
	}
	if key.BudgetUSD != 5 {
		t.Errorf("BudgetUSD = %v", key.BudgetUSD)
	}
	if key.KeyHash == raw {
		t.Error("KeyHash must not be the raw key")
	}
	if key.KeyHash != HashKey(raw) {
		t.Error("KeyHash must be the SHA-256 hex of the raw key")
	}
	if len(key.KeyHash) != 64 {
		t.Errorf("KeyHash length = %d, want 64 (SHA-256 hex)", len(key.KeyHash))
	}
	if key.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set")
	}
	if !key.HasScope("chat") || !key.HasScope("models") || key.HasScope("admin") {
		t.Errorf("scopes wrong: %v", key.Scopes)
	}
}

func TestIssueKeyUniqueness(t *testing.T) {
	_, raw1, err := IssueKey("a", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, raw2, err := IssueKey("a", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if raw1 == raw2 {
		t.Error("two issued keys must differ")
	}
}

func TestIssueKeyValidation(t *testing.T) {
	if _, _, err := IssueKey("", nil, 0); err == nil {
		t.Error("IssueKey should reject an empty name")
	}
	if _, _, err := IssueKey("x", nil, -1); err == nil {
		t.Error("IssueKey should reject a negative budget")
	}
}

func TestGatewayKeyVerify(t *testing.T) {
	key, raw, err := IssueKey("v", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !key.Verify(raw) {
		t.Error("Verify should accept the issued raw key")
	}
	if key.Verify(raw + "x") {
		t.Error("Verify should reject a tampered key")
	}
	if key.Verify("") {
		t.Error("Verify should reject an empty key")
	}
	if key.Verify("crk_" + strings.Repeat("A", 32)) {
		t.Error("Verify should reject an unrelated key")
	}
	// A key with a corrupt stored hash never verifies.
	bad := key
	bad.KeyHash = "not-hex"
	if bad.Verify(raw) {
		t.Error("Verify should reject a corrupt stored hash")
	}
}

func TestConfigVerifyGatewayKey(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	if _, ok := cfg.VerifyGatewayKey("crk_" + strings.Repeat("A", 32)); ok {
		t.Error("empty config should not verify any key")
	}
	k1, raw1, err := IssueKey("one", []string{"chat"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	k2, raw2, err := IssueKey("two", []string{"models"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k1); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k2); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k1); err == nil {
		t.Error("AddGatewayKey should reject a duplicate ID")
	}
	got, ok := cfg.VerifyGatewayKey(raw2)
	if !ok || got.ID != k2.ID || got.Name != "two" {
		t.Errorf("VerifyGatewayKey returned %+v, ok=%v", got, ok)
	}
	if _, ok := cfg.VerifyGatewayKey(raw1 + "tampered"); ok {
		t.Error("VerifyGatewayKey should reject a tampered key")
	}
	found, ok := cfg.FindGatewayKey(k1.ID)
	if !ok || found.Name != "one" {
		t.Errorf("FindGatewayKey: %+v, ok=%v", found, ok)
	}
	if _, ok := cfg.FindGatewayKey("gk_doesnotexist"); ok {
		t.Error("FindGatewayKey should miss unknown IDs")
	}
}
