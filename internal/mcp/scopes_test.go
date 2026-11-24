package mcp

import (
	"strings"
	"testing"
)

func TestScopeMatches(t *testing.T) {
	cases := []struct {
		granted, required string
		want              bool
	}{
		// Exact match.
		{"mcp:read:status", "mcp:read:status", true},
		{"mcp:read:status", "mcp:read:providers", false},
		// Full wildcard.
		{"*", "mcp:read:status", true},
		{"*", "mcp:destructive", true},
		// Trailing-* wildcard.
		{"mcp:read:*", "mcp:read:providers", true},
		{"mcp:read:*", "mcp:read:status", true},
		{"mcp:read:*", "mcp:manage:cost", false},
		{"mcp:*", "mcp:destructive", true},
		{"mcp:manage:*", "mcp:manage:cache", true},
		{"mcp:manage:*", "mcp:read:status", false},
		// Parent scope does NOT cover children (no implicit hierarchy).
		{"mcp:read", "mcp:read:status", false},
		{"mcp", "mcp:read:status", false},
		// A * anywhere but the end is literal, never a wildcard.
		{"mcp:*:status", "mcp:read:status", false},
		{"mcp:read:stat*", "mcp:read:status", true}, // trailing * with a longer prefix still matches
		{"mcp:read:stat*", "mcp:read:providers", false},
		// Empty never matches.
		{"", "mcp:read:status", false},
		{"mcp:read:*", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		if got := ScopeMatches(c.granted, c.required); got != c.want {
			t.Errorf("ScopeMatches(%q, %q) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
}

func TestCheckScopes(t *testing.T) {
	// All required covered (exact + wildcard mix) → allow, no missing.
	res := CheckScopes([]string{"mcp:read:*", "mcp:destructive"}, []string{ScopeReadStatus, ScopeDestructive})
	if !res.Allowed || len(res.Missing) != 0 || res.Reason != "" {
		t.Errorf("covered check = %+v, want allowed with no missing", res)
	}
	// Partial coverage → deny, missing names exactly the uncovered.
	res = CheckScopes([]string{ScopeReadProviders}, []string{ScopeReadProviders, ScopeReadStatus})
	if res.Allowed {
		t.Errorf("partial check allowed, want deny: %+v", res)
	}
	if len(res.Missing) != 1 || res.Missing[0] != ScopeReadStatus {
		t.Errorf("missing = %v, want [%s]", res.Missing, ScopeReadStatus)
	}
	if !strings.Contains(res.Reason, ScopeReadStatus) {
		t.Errorf("reason %q does not name the missing scope", res.Reason)
	}
	// Empty grants deny any non-empty requirement.
	res = CheckScopes(nil, []string{ScopeReadStatus})
	if res.Allowed {
		t.Errorf("empty grants allowed, want deny")
	}
	res = CheckScopes([]string{}, []string{ScopeReadStatus})
	if res.Allowed {
		t.Errorf("empty grants allowed, want deny")
	}
	// Empty requirement allows (no tool has one today; the rule is total).
	res = CheckScopes(nil, nil)
	if !res.Allowed {
		t.Errorf("empty requirement denied, want allow")
	}
	// AND semantics for destructive tools: family scope alone is
	// NOT enough; both family and mcp:destructive are required.
	res = CheckScopes([]string{ScopeManageCache}, []string{ScopeManageCache, ScopeDestructive})
	if res.Allowed || len(res.Missing) != 1 || res.Missing[0] != ScopeDestructive {
		t.Errorf("family-only destructive check = %+v, want deny missing mcp:destructive", res)
	}
	res = CheckScopes([]string{ScopeManageCache, ScopeDestructive}, []string{ScopeManageCache, ScopeDestructive})
	if !res.Allowed {
		t.Errorf("family+destructive check denied, want allow: %+v", res)
	}
}

func TestToolScopeAssignment(t *testing.T) {
	want := map[string][]string{
		"router_status": {ScopeReadStatus},
		"provider_list": {ScopeReadProviders},
		"model_list":    {ScopeReadModels},
	}
	for tool, scopes := range want {
		got, known := RequiredScopes(tool)
		if !known {
			t.Fatalf("RequiredScopes(%q): unknown tool, want known", tool)
		}
		if len(got) != len(scopes) || got[0] != scopes[0] {
			t.Errorf("RequiredScopes(%q) = %v, want %v", tool, got, scopes)
		}
	}
	// Unknown tool: no scopes, not known (dispatcher answers -32602).
	if _, known := RequiredScopes("chat"); known {
		t.Errorf("RequiredScopes(chat) known, want unknown")
	}
	// The returned slice is a copy: mutating it must not change
	// the assignment table.
	got, _ := RequiredScopes("router_status")
	got[0] = "mcp:read:providers"
	again, _ := RequiredScopes("router_status")
	if again[0] != ScopeReadStatus {
		t.Errorf("mutating RequiredScopes result changed the table: %v", again)
	}
}

func TestAllScopesKnown(t *testing.T) {
	all := AllScopes()
	// 21 scopes: the M9.1 eleven plus ten P1.2 ecosystem-expansion
	// scopes (read: capabilities, quota, budget, cost_est,
	// routing_plan, compression, eval, org; manage: eval, org).
	if len(all) != 21 {
		t.Fatalf("AllScopes() has %d scopes, want 21: %v", len(all), all)
	}
	seen := map[string]bool{}
	for _, s := range all {
		if !strings.HasPrefix(s, "mcp:") {
			t.Errorf("scope %q lacks the mcp: prefix", s)
		}
		if seen[s] {
			t.Errorf("duplicate scope %q", s)
		}
		seen[s] = true
		if !IsKnownScope(s) {
			t.Errorf("IsKnownScope(%q) = false, want true", s)
		}
	}
	if IsKnownScope("*") || IsKnownScope("mcp:read:*") {
		t.Errorf("wildcards must not be known scopes")
	}
	if IsKnownScope("chat") {
		t.Errorf("an HTTP gateway scope must not be a known MCP scope")
	}
}

func TestRegisterToolScopes(t *testing.T) {
	// M9.3/M9.4 seam: a future tool registers its scopes, then
	// the check honours them (including the destructive AND rule).
	RegisterToolScopes("test_future_tool", ScopeManageRouting, ScopeDestructive)
	got, known := RequiredScopes("test_future_tool")
	if !known || len(got) != 2 {
		t.Fatalf("registered tool scopes = %v, known=%v, want 2 scopes", got, known)
	}
	res := CheckScopes([]string{ScopeManageRouting}, got)
	if res.Allowed {
		t.Errorf("family-only grant allowed a destructive tool, want deny")
	}
	// Registering an unknown scope panics: a typo must never
	// silently grant or deny a tool.
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("RegisterToolScopes with an unknown scope did not panic")
		}
	}()
	RegisterToolScopes("test_bad_tool", "mcp:read:nope")
}
