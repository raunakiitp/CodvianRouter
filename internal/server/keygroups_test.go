// AK-1 — key groups (GAP-TRACK §14): team-level model access
// control. Group records + per-group glob allow/deny rules +
// membership, enforced in checkKeyPolicy and in the /v1/models
// filter helper. Fail closed throughout: a key whose groups are
// all deleted / typo'd / deactivated, or whose group store is
// unreadable, is refused for every model — never silently
// unrestricted (the sector-typical own fail-open trap, not copied).
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// ak1Groups is the shared fixture: team-a allows gpt-* but denies
// gpt-4o; team-b allows claude-*; team-c is DISABLED and allows
// everything (its grants must pause) but denies gpt-4 (its denies
// must keep biting).
func ak1Groups() []config.KeyGroup {
	return []config.KeyGroup{
		{ID: "kg-team-a", Name: "team-a", Rules: []config.GroupModelRule{
			{Effect: "allow", Pattern: "gpt-*"},
			{Effect: "deny", Pattern: "gpt-4o"},
		}},
		{ID: "kg-team-b", Name: "team-b", Rules: []config.GroupModelRule{
			{Effect: "allow", Pattern: "claude-*"},
		}},
		{ID: "kg-team-c", Name: "team-c", Disabled: true, Rules: []config.GroupModelRule{
			{Effect: "allow", Pattern: "*"},
			{Effect: "deny", Pattern: "gpt-4"},
		}},
	}
}

func ak1Cfg(t *testing.T, groups []config.KeyGroup) *config.Config {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	if err := cfg.SetKeyGroups(groups); err != nil {
		t.Fatalf("SetKeyGroups: %v", err)
	}
	return cfg
}

func ak1Key(groups ...string) config.GatewayKey {
	return config.GatewayKey{ID: "gk_test", Name: "test", KeyGroups: groups}
}

// TestAK1AllowAndNoGrant: an active group's allow glob grants;
// a model no active group allows is refused (no-grant = deny);
// a key in no groups is untouched by the layer.
func TestAK1AllowAndNoGrant(t *testing.T) {
	cfg := ak1Cfg(t, ak1Groups())
	key := ak1Key("kg-team-a")
	if d := cfg.GroupModelDecision(key, "gpt-4", ""); d.Allowed != true || d.Reason != config.GroupAllowed {
		t.Fatalf("gpt-4: decision = %+v, want allowed", d)
	}
	if d := cfg.GroupModelDecision(key, "claude-3", ""); d.Allowed != false || d.Reason != config.GroupNoGrant {
		t.Fatalf("claude-3: decision = %+v, want no_grant deny", d)
	}
	plain := config.GatewayKey{ID: "gk_plain", Name: "plain"}
	if d := cfg.GroupModelDecision(plain, "anything", ""); d.Allowed != true || d.Reason != config.GroupNoMembership {
		t.Fatalf("no-membership key: decision = %+v, want allowed/no_membership", d)
	}
}

// TestAK1DenyWins: a deny in one group beats an allow in another,
// and beats an allow in the same group.
func TestAK1DenyWins(t *testing.T) {
	cfg := ak1Cfg(t, ak1Groups())
	// gpt-4o is allowed by team-a's gpt-* but denied by team-a itself.
	key := ak1Key("kg-team-a")
	if d := cfg.GroupModelDecision(key, "gpt-4o", ""); d.Allowed != false || d.Reason != config.GroupDenied {
		t.Fatalf("gpt-4o (same-group deny): decision = %+v, want denied", d)
	}
	// gpt-4o allowed by team-a, denied by nobody else here — but add
	// team-b? No: prove cross-group deny via team-c's deny of gpt-4
	// against team-a's allow of gpt-*.
	both := ak1Key("kg-team-a", "kg-team-c")
	if d := cfg.GroupModelDecision(both, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupDenied {
		t.Fatalf("gpt-4 (cross-group deny): decision = %+v, want denied", d)
	}
	if d := cfg.GroupModelDecision(both, "gpt-4-turbo", ""); d.Allowed != true {
		t.Fatalf("gpt-4-turbo (not denied anywhere): decision = %+v, want allowed", d)
	}
}

// TestAK1InactivePausesGrantKeepsDeny: a disabled group's allows
// grant nothing, but its denies still refuse. A key in ONLY a
// disabled group is fail-closed (all inactive).
func TestAK1InactivePausesGrantKeepsDeny(t *testing.T) {
	cfg := ak1Cfg(t, ak1Groups())
	onlyDisabled := ak1Key("kg-team-c")
	if d := cfg.GroupModelDecision(onlyDisabled, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupAllInactive || !d.FailClosed {
		t.Fatalf("disabled-only key, denied model: decision = %+v, want all_inactive fail-closed", d)
	}
	if d := cfg.GroupModelDecision(onlyDisabled, "claude-3", ""); d.Allowed != false || d.Reason != config.GroupAllInactive || !d.FailClosed {
		t.Fatalf("disabled-only key, other model: decision = %+v, want all_inactive fail-closed (grants paused)", d)
	}
	// Active allow + inactive deny: the inactive deny still wins.
	mixed := ak1Key("kg-team-a", "kg-team-c")
	if d := cfg.GroupModelDecision(mixed, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupDenied {
		t.Fatalf("active allow + inactive deny: decision = %+v, want denied", d)
	}
	// And the inactive group's "*" allow grants nothing on its own.
	if d := cfg.GroupModelDecision(mixed, "llama-3", ""); d.Allowed != false || d.Reason != config.GroupNoGrant {
		t.Fatalf("inactive '*' allow must grant nothing: decision = %+v, want no_grant", d)
	}
}

// TestAK1FailClosedMissingGroups: memberships resolving to NO
// existing group fail closed; a dangling membership next to a good
// one grants nothing but does not break the good group's rules.
func TestAK1FailClosedMissingGroups(t *testing.T) {
	cfg := ak1Cfg(t, ak1Groups())
	ghost := ak1Key("kg-nope", "kg-also-nope")
	if d := cfg.GroupModelDecision(ghost, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupMissing || !d.FailClosed {
		t.Fatalf("all-dangling key: decision = %+v, want groups_missing fail-closed", d)
	}
	partial := ak1Key("kg-team-a", "kg-typo")
	if d := cfg.GroupModelDecision(partial, "gpt-4", ""); d.Allowed != true {
		t.Fatalf("good + dangling membership, allowed model: decision = %+v, want allowed", d)
	}
	if d := cfg.GroupModelDecision(partial, "claude-3", ""); d.Allowed != false || d.Reason != config.GroupNoGrant {
		t.Fatalf("good + dangling membership, ungranted model: decision = %+v, want no_grant", d)
	}
	// A key in groups on a config with NO groups at all is missing too.
	empty := config.DefaultConfig(t.TempDir())
	if d := empty.GroupModelDecision(ghost, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupMissing || !d.FailClosed {
		t.Fatalf("no groups stored: decision = %+v, want groups_missing fail-closed", d)
	}
}

// TestAK1UnreadableGroupsFailClosed: a malformed key_groups
// extension is refused, never treated as "no groups".
func TestAK1UnreadableGroupsFailClosed(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Extensions = map[string]json.RawMessage{
		config.KeyGroupsExtensionKey: json.RawMessage(`[{bad json`),
	}
	key := ak1Key("kg-team-a")
	if d := cfg.GroupModelDecision(key, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupUnreadable || !d.FailClosed {
		t.Fatalf("malformed groups: decision = %+v, want groups_unreadable fail-closed", d)
	}
}

// TestAK1ProviderPin: a pinned rule matches only that provider.
// On the request path (provider unknown) pinned rules never match
// — a pinned allow grants nothing there, so the model is refused
// rather than widened.
func TestAK1ProviderPin(t *testing.T) {
	cfg := ak1Cfg(t, []config.KeyGroup{{
		ID: "kg-pin", Name: "pinned", Rules: []config.GroupModelRule{
			{Effect: "allow", Pattern: "gpt-*", Provider: "openai"},
			{Effect: "deny", Pattern: "gpt-4o", Provider: "azure"},
		},
	}})
	key := ak1Key("kg-pin")
	if d := cfg.GroupModelDecision(key, "gpt-4", "openai"); d.Allowed != true {
		t.Fatalf("pinned allow, matching provider: %+v, want allowed", d)
	}
	if d := cfg.GroupModelDecision(key, "gpt-4", "azure"); d.Allowed != false || d.Reason != config.GroupNoGrant {
		t.Fatalf("pinned allow, other provider: %+v, want no_grant", d)
	}
	if d := cfg.GroupModelDecision(key, "gpt-4", ""); d.Allowed != false || d.Reason != config.GroupNoGrant {
		t.Fatalf("pinned allow, unknown provider (request path): %+v, want no_grant, never widened", d)
	}
	if d := cfg.GroupModelDecision(key, "gpt-4o", "azure"); d.Allowed != false || d.Reason != config.GroupDenied {
		t.Fatalf("pinned deny, matching provider: %+v, want denied", d)
	}
	if d := cfg.GroupModelDecision(key, "gpt-4o", "openai"); d.Allowed != true {
		t.Fatalf("pinned deny, other provider: %+v, want allowed (allow rule matches, deny pinned elsewhere)", d)
	}
}

// TestAK1SetKeyGroupsValidation: bad records and duplicate IDs are
// refused at write time; a stored set survives a JSON round-trip
// of the whole config (the Extensions persistence path).
func TestAK1SetKeyGroupsValidation(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	if err := cfg.SetKeyGroups([]config.KeyGroup{{ID: "kg-x", Name: "x", Rules: []config.GroupModelRule{{Effect: "maybe", Pattern: "*"}}}}); err == nil {
		t.Fatal("bad rule effect accepted, want an error")
	}
	if err := cfg.SetKeyGroups([]config.KeyGroup{{ID: "kg-x", Name: "x", Rules: []config.GroupModelRule{{Effect: "allow", Pattern: ""}}}}); err == nil {
		t.Fatal("empty rule pattern accepted, want an error")
	}
	if err := cfg.SetKeyGroups([]config.KeyGroup{{ID: "kg-x", Name: "x"}, {ID: "kg-x", Name: "dup"}}); err == nil {
		t.Fatal("duplicate group id accepted, want an error")
	}
	if err := cfg.SetKeyGroups(ak1Groups()); err != nil {
		t.Fatalf("SetKeyGroups valid: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	var back config.Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	groups, err := back.KeyGroups()
	if err != nil {
		t.Fatalf("KeyGroups after round-trip: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("round-trip groups = %d, want 3", len(groups))
	}
	if err := back.SetKeyGroups(nil); err != nil {
		t.Fatalf("clear groups: %v", err)
	}
	if groups, _ := back.KeyGroups(); len(groups) != 0 {
		t.Fatalf("cleared groups = %d, want 0", len(groups))
	}
}

// TestKeyGroupsFailClosed: the AK-1 acceptance test at the
// request-path layer (checkKeyPolicy). A key whose groups are all
// deleted / misspelled, all deactivated, or whose group store is
// unreadable is DENIED with 403 key_group_missing — it never
// silently becomes unrestricted (the sector-typical fail-open trap, not
// copied here).
func TestKeyGroupsFailClosed(t *testing.T) {
	s, _ := keyStatusSetup(t)
	if err := s.cfg.SetKeyGroups(ak1Groups()); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	key, _ := s.cfg.FindGatewayKey(s.id)

	// All memberships misspelled (groups deleted / typo'd).
	key.KeyGroups = []string{"kg-nope", "kg-also-nope"}
	stopped, code, body := ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_missing") {
		t.Fatalf("all-misspelled groups: stopped=%v code=%d body=%s, want 403 key_group_missing", stopped, code, body)
	}
	// All memberships deactivated (grants paused, nothing left).
	key.KeyGroups = []string{"kg-team-c"}
	stopped, code, body = ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_missing") {
		t.Fatalf("all-deactivated groups: stopped=%v code=%d body=%s, want 403 key_group_missing", stopped, code, body)
	}
	// Unreadable group store (malformed key_groups extension).
	s.cfg.Extensions = map[string]json.RawMessage{
		config.KeyGroupsExtensionKey: json.RawMessage(`[{bad json`),
	}
	srv = s.server(t)
	key, _ = s.cfg.FindGatewayKey(s.id)
	key.KeyGroups = []string{"kg-team-a"}
	stopped, code, body = ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_missing") {
		t.Fatalf("unreadable groups: stopped=%v code=%d body=%s, want 403 key_group_missing", stopped, code, body)
	}
}

// TestAK1KeyPolicyCarriesKeyGroups: membership travels with the
// policy bundle (create / update / import) and validates.
func TestAK1KeyPolicyCarriesKeyGroups(t *testing.T) {
	key := ak1Key("kg-team-a")
	key.ApplyPolicy(config.KeyPolicy{ModelAccessMode: "all", KeyGroups: []string{"kg-team-a", "kg-team-b"}})
	if got := key.PolicyOf().KeyGroups; len(got) != 2 || got[0] != "kg-team-a" {
		t.Fatalf("PolicyOf KeyGroups = %v, want [kg-team-a kg-team-b]", got)
	}
	if err := (config.KeyPolicy{KeyGroups: []string{"kg-a", ""}}).Validate(); err == nil {
		t.Fatal("empty group id in policy accepted, want an error")
	}
}

// ak1CheckKeyPolicy drives checkKeyPolicy directly: it writes the
// 403 and returns true when the request must stop.
func ak1CheckKeyPolicy(t *testing.T, srv *Server, key config.GatewayKey, model string) (stopped bool, code int, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	stopped = srv.checkKeyPolicy(rec, req, key, model)
	return stopped, rec.Code, rec.Body.String()
}

// TestAK1CheckKeyPolicy: the request path enforces groups after the
// per-key policy — deny-wins and fail-closed get distinct 403s,
// and a no-membership key passes untouched.
func TestAK1CheckKeyPolicy(t *testing.T) {
	s, _ := keyStatusSetup(t)
	if err := s.cfg.SetKeyGroups(ak1Groups()); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	key, _ := s.cfg.FindGatewayKey(s.id)

	// No membership: passes (group layer imposes nothing).
	stopped, code, _ := ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if stopped || code != http.StatusOK {
		t.Fatalf("no-membership key: stopped=%v code=%d, want pass-through", stopped, code)
	}

	// Membership, allowed model: passes.
	key.KeyGroups = []string{"kg-team-a"}
	stopped, code, _ = ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if stopped || code != http.StatusOK {
		t.Fatalf("allowed model: stopped=%v code=%d, want pass-through", stopped, code)
	}
	// Membership, group-denied model: 403 key_group_model_not_allowed.
	stopped, code, body := ak1CheckKeyPolicy(t, srv, key, "gpt-4o")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_model_not_allowed") {
		t.Fatalf("denied model: stopped=%v code=%d body=%s, want 403 key_group_model_not_allowed", stopped, code, body)
	}
	// Membership, no-grant model: same 403 type.
	stopped, code, body = ak1CheckKeyPolicy(t, srv, key, "claude-3")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_model_not_allowed") {
		t.Fatalf("no-grant model: stopped=%v code=%d body=%s, want 403 key_group_model_not_allowed", stopped, code, body)
	}
	// All memberships dangling: 403 key_group_missing (fail closed).
	key.KeyGroups = []string{"kg-nope"}
	stopped, code, body = ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_group_missing") {
		t.Fatalf("dangling groups: stopped=%v code=%d body=%s, want 403 key_group_missing", stopped, code, body)
	}
	// Per-key policy still applies first: a restricted per-key
	// allowlist refusing the model answers key_model_not_allowed,
	// not the group type.
	key.KeyGroups = []string{"kg-team-a"}
	setFixturePolicy(s, config.KeyPolicy{ModelAccessMode: "restricted", AllowedModels: []string{"other-model"}})
	key, _ = s.cfg.FindGatewayKey(s.id)
	key.KeyGroups = []string{"kg-team-a"}
	stopped, code, body = ak1CheckKeyPolicy(t, srv, key, "gpt-4")
	if !stopped || code != http.StatusForbidden || !strings.Contains(body, "key_model_not_allowed") {
		t.Fatalf("per-key deny: stopped=%v code=%d body=%s, want 403 key_model_not_allowed", stopped, code, body)
	}
}

// TestAK1ChatPathGroupEnforced: a live POST /v1/chat/completions
// naming a group-denied model is refused BEFORE any provider is
// called (engine call count stays 0); a group-allowed model
// reaches the engine.
func TestAK1ChatPathGroupEnforced(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// team-a as usual, plus an allow for the fixture's priced model
	// (the only model the fixture price book prices — an unpriced
	// model could be refused by the budget gate before the engine,
	// which would prove nothing about the group layer).
	groups := ak1Groups()
	groups[0].Rules = append(groups[0].Rules, config.GroupModelRule{Effect: "allow", Pattern: "priced-model"})
	if err := s.cfg.SetKeyGroups(groups); err != nil {
		t.Fatal(err)
	}
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].KeyGroups = []string{"kg-team-a"}
		}
	}
	srv := s.server(t)

	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("gpt-4o", false), s.raw)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "key_group_model_not_allowed") {
		t.Fatalf("group-denied chat: status = %d, want 403 key_group_model_not_allowed: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the refusal must happen before any provider is called", s.eng.chatCalls.Load())
	}

	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("group-allowed chat: status = 403, want the request to reach the engine: %s", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1 — the allowed request must reach the provider", s.eng.chatCalls.Load())
	}
}

// TestAK1ModelsFilter: the /v1/models filter helper lists only
// group-allowed models (provider-aware), lists everything for a
// no-membership key, and lists NOTHING for a fail-closed key.
func TestAK1ModelsFilter(t *testing.T) {
	s, _ := keyStatusSetup(t)
	if err := s.cfg.SetKeyGroups(ak1Groups()); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	models := []Model{
		{ID: "gpt-4", Object: "model", ProviderID: "openai"},
		{ID: "gpt-4o", Object: "model", ProviderID: "openai"},
		{ID: "gpt-4-turbo", Object: "model", ProviderID: "azure"},
		{ID: "claude-3", Object: "model", ProviderID: "anthropic"},
		{ID: "llama-3", Object: "model", ProviderID: "meta"},
	}
	key, _ := s.cfg.FindGatewayKey(s.id)

	// No membership: unchanged.
	if got := srv.filterModelsByKeyGroups(models, key); len(got) != len(models) {
		t.Fatalf("no-membership filter: %d rows, want %d (unchanged)", len(got), len(models))
	}
	// team-a: gpt-4 + gpt-4-turbo (gpt-4o denied, rest no-grant).
	key.KeyGroups = []string{"kg-team-a"}
	got := srv.filterModelsByKeyGroups(models, key)
	ids := make([]string, 0, len(got))
	for _, m := range got {
		ids = append(ids, m.ID)
	}
	if len(got) != 2 || ids[0] != "gpt-4" || ids[1] != "gpt-4-turbo" {
		t.Fatalf("team-a filter: %v, want [gpt-4 gpt-4-turbo]", ids)
	}
	// team-a + team-b: claude-3 joins.
	key.KeyGroups = []string{"kg-team-a", "kg-team-b"}
	got = srv.filterModelsByKeyGroups(models, key)
	if len(got) != 3 {
		t.Fatalf("team-a+b filter: %d rows, want 3", len(got))
	}
	// All memberships dangling: fail closed, empty list.
	key.KeyGroups = []string{"kg-nope"}
	if got := srv.filterModelsByKeyGroups(models, key); len(got) != 0 {
		t.Fatalf("dangling groups filter: %d rows, want 0 (fail closed)", len(got))
	}
	// Disabled-only group: fail closed, empty list (grants paused).
	key.KeyGroups = []string{"kg-team-c"}
	if got := srv.filterModelsByKeyGroups(models, key); len(got) != 0 {
		t.Fatalf("disabled-only filter: %d rows, want 0 (fail closed)", len(got))
	}
}

// TestAK1ModelsFilterProviderPin: a provider-pinned rule filters
// per (provider, model) pair, not per bare model ID.
func TestAK1ModelsFilterProviderPin(t *testing.T) {
	s, _ := keyStatusSetup(t)
	if err := s.cfg.SetKeyGroups([]config.KeyGroup{{
		ID: "kg-pin", Name: "pinned", Rules: []config.GroupModelRule{
			{Effect: "allow", Pattern: "gpt-*", Provider: "openai"},
		},
	}}); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	models := []Model{
		{ID: "gpt-4", Object: "model", ProviderID: "openai"},
		{ID: "gpt-4", Object: "model", ProviderID: "azure"},
	}
	key, _ := s.cfg.FindGatewayKey(s.id)
	key.KeyGroups = []string{"kg-pin"}
	got := srv.filterModelsByKeyGroups(models, key)
	if len(got) != 1 || got[0].ProviderID != "openai" {
		t.Fatalf("pinned filter: %d rows, want only the openai gpt-4", len(got))
	}
}
