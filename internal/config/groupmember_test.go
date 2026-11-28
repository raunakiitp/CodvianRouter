// Routing-group member parsing tests (CHECKLIST 256): a group
// member is "provider" or "provider@effort" — the same provider
// twice at different efforts is two distinct entries, the same
// (provider, effort) pair twice is an error.
package config

import (
	"strings"
	"testing"
)

func TestParseGroupMember(t *testing.T) {
	cases := []struct {
		member   string
		provider string
		effort   string
		wantErr  string // substring, "" = no error
	}{
		{member: "prov-a", provider: "prov-a", effort: ""},
		{member: "prov-a@low", provider: "prov-a", effort: "low"},
		{member: "prov-a@HIGH", provider: "prov-a", effort: "high"}, // normalised
		{member: "prov-a@xhigh", provider: "prov-a", effort: "xhigh"},
		{member: "prov-a@minimal", provider: "prov-a", effort: "minimal"},
		{member: "prov-a@none", provider: "prov-a", effort: "none"},
		{member: "  prov-a@low  ", provider: "prov-a", effort: "low"}, // trimmed
		// The split is on the LAST "@": an "@" inside the provider
		// id stays part of the id when the suffix is a known level.
		{member: "prov@x@high", provider: "prov@x", effort: "high"},
		// An unknown suffix is NOT read as an effort — the whole
		// member is rejected, so a typo can never silently become
		// part of a provider id.
		{member: "prov-a@bogus", wantErr: "unknown reasoning effort"},
		{member: "prov-a@", wantErr: "empty effort"},
		{member: "@low", wantErr: "no provider"},
		{member: "", wantErr: "is empty"},
		{member: "   ", wantErr: "is empty"},
	}
	for _, tc := range cases {
		providerID, effort, err := ParseGroupMember(tc.member)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("ParseGroupMember(%q) = error %v, want (%q, %q)", tc.member, err, tc.provider, tc.effort)
				continue
			}
			if providerID != tc.provider || effort != tc.effort {
				t.Errorf("ParseGroupMember(%q) = (%q, %q), want (%q, %q)", tc.member, providerID, effort, tc.provider, tc.effort)
			}
			continue
		}
		if err == nil {
			t.Errorf("ParseGroupMember(%q) = (%q, %q), nil, want error containing %q", tc.member, providerID, effort, tc.wantErr)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("ParseGroupMember(%q) error = %v, want it to contain %q", tc.member, err, tc.wantErr)
		}
	}
}

// TestConfigRoutingGroupsEffortValidation: the same provider twice
// at different efforts is a valid group; the same (provider,
// effort) pair twice, or an unknown effort, fails at load.
func TestConfigRoutingGroupsEffortValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.Providers = []ProviderConfig{
			{ID: "prov-a", BaseURL: "http://127.0.0.1:9001"},
			{ID: "prov-b", BaseURL: "http://127.0.0.1:9002"},
		}
		cfg.Routing = &RoutingConfig{
			Groups: map[string][]string{"fast": {"prov-a@low", "prov-a@high", "prov-b"}},
		}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Errorf("same provider at two efforts failed Validate: %v", err)
	}
	cases := []struct {
		name    string
		members []string
		want    string
	}{
		{
			name:    "same pair twice",
			members: []string{"prov-a@low", "prov-a@low"},
			want:    `lists provider "prov-a" at effort "low" twice`,
		},
		{
			name:    "same provider no effort twice",
			members: []string{"prov-a", "prov-a"},
			want:    `lists provider "prov-a" twice`,
		},
		{
			name:    "unknown effort",
			members: []string{"prov-a@bogus"},
			want:    "unknown reasoning effort",
		},
		{
			name:    "unknown provider with effort",
			members: []string{"prov-ghost@low"},
			want:    `lists unknown provider "prov-ghost"`,
		},
	}
	for _, tc := range cases {
		cfg := base()
		cfg.Routing.Groups["fast"] = tc.members
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: Validate passed, want error containing %q", tc.name, tc.want)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate error = %v, want it to contain %q", tc.name, err, tc.want)
		}
	}
}

// TestConfigStreamMaxLifetimeMs: the cap is accepted as a duration
// in milliseconds — 0 (default), positive, and negative (disabled)
// all validate; the value is what the engine enforces, so the
// config must never reject an explicit negative.
func TestConfigStreamMaxLifetimeMs(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{StreamMaxLifetimeMs: -1}
	if err := cfg.Validate(); err != nil {
		t.Errorf("negative stream_max_lifetime_ms failed Validate: %v", err)
	}
	cfg.Routing.StreamMaxLifetimeMs = 60000
	if err := cfg.Validate(); err != nil {
		t.Errorf("positive stream_max_lifetime_ms failed Validate: %v", err)
	}
}

// --- nested group refs (Item 2: recursive combo-ref expansion) ---

func TestValidateGroupGraphNoCycle(t *testing.T) {
	groups := map[string][]string{
		"outer": {"group:inner", "p1"},
		"inner": {"p2", "p3"},
	}
	if err := validateGroupGraph(groups); err != nil {
		t.Fatalf("acyclic graph rejected: %v", err)
	}
}

func TestValidateGroupGraphCycle(t *testing.T) {
	groups := map[string][]string{
		"a": {"group:b"},
		"b": {"group:a"},
	}
	err := validateGroupGraph(groups)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("2-cycle not rejected: %v", err)
	}
}

func TestValidateGroupGraphSelfRef(t *testing.T) {
	groups := map[string][]string{"a": {"group:a"}}
	if err := validateGroupGraph(groups); err == nil {
		t.Fatal("self-ref not rejected as cycle")
	}
}

func TestExpandGroupMembersNested(t *testing.T) {
	groups := map[string][]string{
		"outer": {"p1", "group:mid", "p4"},
		"mid":   {"p2", "group:inner"},
		"inner": {"p3"},
	}
	got, ok := ExpandGroupMembers(groups, "outer")
	if !ok {
		t.Fatal("ok=false for known group")
	}
	want := []string{"p1", "p2", "p3", "p4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("nested expansion = %v, want %v (depth-first, order preserved)", got, want)
	}
}

func TestExpandGroupMembersCycleTerminates(t *testing.T) {
	// Defensive runtime guard: even if a cycle slipped past load, the
	// expansion must terminate (visited set), never loop.
	groups := map[string][]string{
		"a": {"group:b", "pa"},
		"b": {"group:a", "pb"},
	}
	got, ok := ExpandGroupMembers(groups, "a")
	if !ok {
		t.Fatal("ok=false")
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "pa") || !strings.Contains(joined, "pb") {
		t.Fatalf("cycle expansion missing members: %v", got)
	}
}

func TestExpandGroupMembersUnknown(t *testing.T) {
	if _, ok := ExpandGroupMembers(map[string][]string{}, "nope"); ok {
		t.Fatal("unknown group returned ok=true")
	}
}
