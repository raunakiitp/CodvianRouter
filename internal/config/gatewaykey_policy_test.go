package config

import (
	"strings"
	"testing"
)

// G15 Area 4 — per-key policy enforcement (IP, model, endpoint).

func TestGatewayKeyAllowedIP(t *testing.T) {
	// No lists at all: every peer passes, exactly as before.
	plain := GatewayKey{}
	if !plain.AllowedIP("192.168.1.5") || !plain.AllowedIP("") {
		t.Error("key with no IP lists must allow every peer")
	}

	allow := GatewayKey{IPAllowlist: []string{"10.0.0.0/8", "192.168.1.5"}}
	if !allow.AllowedIP("10.1.2.3") {
		t.Error("CIDR allowlist must admit 10.1.2.3")
	}
	if !allow.AllowedIP("192.168.1.5") {
		t.Error("exact allowlist entry must admit 192.168.1.5")
	}
	if allow.AllowedIP("192.168.1.6") {
		t.Error("non-allowlisted IP must be refused")
	}
	if allow.AllowedIP("") {
		t.Error("unknown peer must fail closed when an allowlist exists")
	}

	// Blocklist wins over the allowlist.
	both := GatewayKey{
		IPAllowlist: []string{"10.0.0.0/8"},
		IPBlocklist: []string{"10.1.2.3"},
	}
	if both.AllowedIP("10.1.2.3") {
		t.Error("blocklisted IP must be refused even when the CIDR allowlist covers it")
	}
	if !both.AllowedIP("10.9.9.9") {
		t.Error("non-blocklisted allowlisted IP must pass")
	}

	// Blocklist alone: everything except the blocked passes.
	blocked := GatewayKey{IPBlocklist: []string{"203.0.113.0/24"}}
	if blocked.AllowedIP("203.0.113.7") {
		t.Error("blocklisted CIDR must refuse 203.0.113.7")
	}
	if !blocked.AllowedIP("198.51.100.1") {
		t.Error("non-blocklisted IP must pass a blocklist-only key")
	}
	if blocked.AllowedIP("") {
		t.Error("unknown peer must fail closed when a blocklist exists")
	}

	// G15 re-review nit 12: plain-IP entries compare as parsed
	// IPs — a non-canonical IPv6 spelling must not evade a
	// blocklist entry, and must still match an allowlist entry.
	v6blocked := GatewayKey{IPBlocklist: []string{"2001:db8::1"}}
	if v6blocked.AllowedIP("2001:0db8:0000:0000:0000:0000:0000:0001") {
		t.Error("expanded-form IPv6 peer must still hit the compressed blocklist entry")
	}
	v6allowed := GatewayKey{IPAllowlist: []string{"2001:0db8:0000:0000:0000:0000:0000:0001"}}
	if !v6allowed.AllowedIP("2001:db8::1") {
		t.Error("compressed-form IPv6 peer must match the expanded allowlist entry")
	}
}

func TestGatewayKeyModelAllowed(t *testing.T) {
	// "all" mode ("" and "all"): everything unless blocked.
	all := GatewayKey{}
	if !all.ModelAllowed("gpt-4") {
		t.Error("empty mode must allow every model")
	}
	all.BlockedModels = []string{"gpt-4"}
	if all.ModelAllowed("gpt-4") {
		t.Error("blocked model must be refused in all mode")
	}
	if !all.ModelAllowed("claude-3") {
		t.Error("non-blocked model must pass in all mode")
	}
	// Wildcard block.
	all.BlockedModels = []string{"gpt-*"}
	if all.ModelAllowed("gpt-4o") {
		t.Error("wildcard block gpt-* must refuse gpt-4o")
	}
	if !all.ModelAllowed("claude-3") {
		t.Error("wildcard block gpt-* must not refuse claude-3")
	}

	// Restricted mode: only the allowlist, blocked wins.
	restr := GatewayKey{ModelAccessMode: "restricted", AllowedModels: []string{"model-a", "model-*"}, BlockedModels: []string{"model-b"}}
	if !restr.ModelAllowed("model-a") {
		t.Error("restricted mode must allow a listed model")
	}
	if !restr.ModelAllowed("model-c") {
		t.Error("restricted mode must allow a wildcard-listed model")
	}
	if restr.ModelAllowed("model-b") {
		t.Error("blocked model must win over the allowlist in restricted mode")
	}
	if restr.ModelAllowed("other") {
		t.Error("restricted mode must refuse an unlisted model")
	}
	// Restricted with an empty allowlist denies every model.
	empty := GatewayKey{ModelAccessMode: "restricted"}
	if empty.ModelAllowed("anything") {
		t.Error("restricted mode with an empty allowlist must deny every model")
	}
	// Empty model is not judged here (the caller skips it).
	if !restr.ModelAllowed("") {
		t.Error("empty model must not be refused by ModelAllowed (caller skips the check)")
	}
}

func TestGatewayKeyEndpointAllowed(t *testing.T) {
	plain := GatewayKey{}
	if !plain.EndpointAllowed("/v1/chat/completions") {
		t.Error("key with no endpoint list must allow every endpoint")
	}
	ep := GatewayKey{AllowedEndpoints: []string{"/v1/models", "/v1/me/*"}}
	if !ep.EndpointAllowed("/v1/models") {
		t.Error("listed endpoint must pass")
	}
	if !ep.EndpointAllowed("/v1/me/status") {
		t.Error("wildcard endpoint /v1/me/* must pass /v1/me/status")
	}
	if ep.EndpointAllowed("/v1/chat/completions") {
		t.Error("unlisted endpoint must be refused")
	}
}

func TestKeyPolicyValidate(t *testing.T) {
	good := KeyPolicy{
		IPAllowlist:     []string{"10.0.0.0/8", "192.168.1.5"},
		IPBlocklist:     []string{"203.0.113.1"},
		ModelAccessMode: "restricted", AllowedModels: []string{"model-a"},
		AllowedEndpoints: []string{"/v1/*"}, ForcedModel: "model-a",
	}
	if err := good.Validate(); err != nil {
		t.Errorf("valid policy must validate: %v", err)
	}
	bad := []KeyPolicy{
		{IPAllowlist: []string{"not-an-ip"}},
		{IPAllowlist: []string{"10.0.0.0/33"}},
		{IPBlocklist: []string{"10.0.0.0/8/extra"}},
		{IPAllowlist: []string{""}},
		{ModelAccessMode: "sometimes"},
		{AllowedModels: []string{""}},
		{AllowedEndpoints: []string{" "}},
		// G15 re-review nit 10: a forced model must be servable
		// under the policy — not blocklisted, and in the
		// allowlist under "restricted".
		{BlockedModels: []string{"model-a"}, ForcedModel: "model-a"},
		{ModelAccessMode: "restricted", AllowedModels: []string{"model-a"}, ForcedModel: "model-b"},
	}
	for i, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("policy %d must fail validation: %+v", i, p)
		}
	}
}

func TestKeyPolicyApplyToDeepCopies(t *testing.T) {
	p := KeyPolicy{IPAllowlist: []string{"10.0.0.0/8"}, AllowedModels: []string{"m1"}, ForcedModel: "m1"}
	var k GatewayKey
	k.ApplyPolicy(p)
	p.IPAllowlist[0] = "9.9.9.9"
	p.AllowedModels[0] = "m2"
	if k.IPAllowlist[0] != "10.0.0.0/8" || k.AllowedModels[0] != "m1" {
		t.Error("ApplyPolicy must deep-copy: mutating the source must not change the key")
	}
	if got := k.PolicyOf(); got.ForcedModel != "m1" || len(got.IPAllowlist) != 1 {
		t.Errorf("PolicyOf round-trip = %+v, want the applied policy", got)
	}
	// PolicyOf must also deep-copy.
	got := k.PolicyOf()
	got.IPAllowlist[0] = "8.8.8.8"
	if k.IPAllowlist[0] != "10.0.0.0/8" {
		t.Error("PolicyOf must deep-copy: mutating the result must not change the key")
	}
}

func TestValidateIPEntry(t *testing.T) {
	for _, e := range []string{"10.0.0.5", "2001:db8::1", "10.0.0.0/8", "2001:db8::/32"} {
		if err := ValidateIPEntry(e); err != nil {
			t.Errorf("ValidateIPEntry(%q) = %v, want nil", e, err)
		}
	}
	for _, e := range []string{"", " ", "example.com", "10.0.0.0/33", "10.0.0.256", "10.0.0.0/-1"} {
		if err := ValidateIPEntry(e); err == nil {
			t.Errorf("ValidateIPEntry(%q) = nil, want an error", e)
		}
	}
}

func TestNewKeyIDShape(t *testing.T) {
	id, err := NewKeyID()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id, "gk_") || len(id) != 19 {
		t.Errorf("NewKeyID() = %q, want gk_ + 16 hex chars", id)
	}
	id2, err := NewKeyID()
	if err != nil {
		t.Fatal(err)
	}
	if id == id2 {
		t.Error("two NewKeyID calls must differ")
	}
}
