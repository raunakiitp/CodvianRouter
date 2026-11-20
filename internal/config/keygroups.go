// Package config — key groups (AK-1, GAP-TRACK §14).
//
// Team-level model access control (OmniRoute `key_groups` +
// `group_model_permissions` + `key_group_members`, migration 065,
// `src/lib/db/apiKeyGroups.ts`): a group is a named set of glob
// model allow/deny rules, and a gateway key belongs to zero or
// more groups via GatewayKey.KeyGroups (group IDs).
//
// Semantics (matching OmniRoute where it is sound, diverging
// where it is not):
//
//   - Deny-wins: any deny rule in ANY of the key's resolved groups
//     refuses the model, even when another group allows it.
//   - Inactive groups pause grants but keep denies: a disabled
//     group's allow rules grant nothing, but its deny rules still
//     refuse.
//   - A group grants only when it is active and one of its allow
//     rules matches. A key in groups with no matching allow rule
//     is refused (no-grant = deny).
//   - FAIL CLOSED (deliberate divergence from OmniRoute's
//     fail-open trap): a key whose memberships resolve to NO
//     existing group (all deleted / typo'd), or whose resolved
//     groups are ALL disabled, or whose group store is unreadable,
//     is refused for every model — it never silently becomes
//     unrestricted. A key in NO groups is unaffected: the group
//     layer imposes nothing and the per-key policy alone governs
//     (back-compat for every key issued before groups existed).
//   - A dangling membership alongside a good one is tolerated:
//     the typo'd entry grants nothing, the good group's rules
//     still apply. Only "all memberships dangling" fails closed.
//   - Provider pin: a rule may pin a provider (registry ID). A
//     pinned rule matches only rows/requests whose provider is
//     known and equal. On the request path the provider is unknown
//     (the request names a bare model), so pinned rules never
//     match there — pinned allows grant nothing and pinned denies
//     refuse nothing on that path. That direction is safe: a
//     model grantable only via a pinned rule is refused on the
//     request path (no-grant), never widened.
//
// Storage: groups live in the config file as the top-level
// `key_groups` JSON key. Config has no first-class KeyGroups
// field yet (a config-slice promotion, with load-time validation,
// is the follow-up); until then the key round-trips verbatim
// through Config.Extensions (preserved across Load/Save/Clone),
// and KeyGroups()/SetKeyGroups() below are the typed accessors.
// A malformed `key_groups` extension fails closed at enforcement
// time (deny + log), never silently unenforced.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// KeyGroupsExtensionKey is the top-level config-file JSON key
// holding the group records (see the package doc above).
const KeyGroupsExtensionKey = "key_groups"

// GroupModelRule is one glob model rule inside a key group.
// Effect is "allow" or "deny". Pattern is a model glob in the
// same language as the per-key policy lists: exact match, a
// trailing "*" prefix ("gpt-*"), or a bare "*" matching every
// model. Provider, when set, pins the rule to one provider
// (registry ID, case-insensitive); empty means every provider.
type GroupModelRule struct {
	Effect   string `json:"effect"`
	Pattern  string `json:"pattern"`
	Provider string `json:"provider,omitempty"`
}

// KeyGroup is one team-level model access group: a named set of
// model allow/deny rules. Disabled is the inactive flag: a
// disabled group's allows grant nothing, but its denies still
// refuse (OmniRoute's "inactive groups pause grants but keep
// denies"). Default (zero value) is active.
type KeyGroup struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Disabled    bool             `json:"disabled,omitempty"`
	Rules       []GroupModelRule `json:"rules,omitempty"`
}

// Validate reports whether the group record is well-formed: a
// non-empty ID and name, and every rule with effect "allow" or
// "deny" and a non-empty pattern. It does not check ID
// uniqueness — that is the set's job (see SetKeyGroups).
func (g KeyGroup) Validate() error {
	if strings.TrimSpace(g.ID) == "" {
		return errors.New("config: key group id must not be empty")
	}
	if strings.TrimSpace(g.Name) == "" {
		return fmt.Errorf("config: key group %q: name must not be empty", g.ID)
	}
	for _, r := range g.Rules {
		switch r.Effect {
		case "allow", "deny":
		default:
			return fmt.Errorf("config: key group %q: rule effect %q must be \"allow\" or \"deny\"", g.ID, r.Effect)
		}
		if strings.TrimSpace(r.Pattern) == "" {
			return fmt.Errorf("config: key group %q: rule pattern must not be empty", g.ID)
		}
	}
	return nil
}

// ruleMatches reports whether rule matches model from provider:
// the glob must match the model ID, and a pinned provider must
// equal the known provider. An unknown provider ("") never
// matches a pinned rule (see the package doc).
func (r GroupModelRule) ruleMatches(model, provider string) bool {
	if !matchPolicyPattern(r.Pattern, model) {
		return false
	}
	if r.Provider == "" {
		return true
	}
	return provider != "" && strings.EqualFold(r.Provider, provider)
}

// AllowsModel reports whether this ACTIVE group grants model
// (from provider): an allow rule matches. A disabled group
// grants nothing — its allows are paused.
func (g KeyGroup) AllowsModel(model, provider string) bool {
	if g.Disabled {
		return false
	}
	for _, r := range g.Rules {
		if r.Effect == "allow" && r.ruleMatches(model, provider) {
			return true
		}
	}
	return false
}

// DeniesModel reports whether this group refuses model (from
// provider): a deny rule matches. Disabled groups still deny —
// inactivity pauses grants, never denies.
func (g KeyGroup) DeniesModel(model, provider string) bool {
	for _, r := range g.Rules {
		if r.Effect == "deny" && r.ruleMatches(model, provider) {
			return true
		}
	}
	return false
}

// KeyGroups returns the group records stored in this config's
// `key_groups` extension (see the package doc). No extension
// means no groups. A malformed extension is an error naming the
// key — callers fail closed on it, never treat it as "no groups".
func (c *Config) KeyGroups() ([]KeyGroup, error) {
	if c == nil || len(c.Extensions) == 0 {
		return nil, nil
	}
	raw, ok := c.Extensions[KeyGroupsExtensionKey]
	if !ok {
		return nil, nil
	}
	var groups []KeyGroup
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, fmt.Errorf("config: %q is not a valid key-group list: %w", KeyGroupsExtensionKey, err)
	}
	for _, g := range groups {
		if err := g.Validate(); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

// SetKeyGroups validates groups (each record, plus duplicate
// IDs) and stores them as the config's `key_groups` extension,
// so they persist across Save/Load via Extensions. Pass nil to
// clear the stored groups.
func (c *Config) SetKeyGroups(groups []KeyGroup) error {
	if len(groups) == 0 {
		if c.Extensions != nil {
			delete(c.Extensions, KeyGroupsExtensionKey)
		}
		return nil
	}
	seen := make(map[string]bool, len(groups))
	for _, g := range groups {
		if err := g.Validate(); err != nil {
			return err
		}
		if seen[g.ID] {
			return fmt.Errorf("config: duplicate key group id %q", g.ID)
		}
		seen[g.ID] = true
	}
	raw, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("config: cannot encode key groups: %w", err)
	}
	if c.Extensions == nil {
		c.Extensions = make(map[string]json.RawMessage, 1)
	}
	c.Extensions[KeyGroupsExtensionKey] = raw
	return nil
}

// FindKeyGroup returns the group with the given ID, if present.
func FindKeyGroup(groups []KeyGroup, id string) (KeyGroup, bool) {
	for _, g := range groups {
		if g.ID == id {
			return g, true
		}
	}
	return KeyGroup{}, false
}

// GroupDecisionReason names why a group evaluation came out the
// way it did. The "fail-closed" reasons (Missing, AllInactive,
// Unreadable) mean the key must be refused for every model.
type GroupDecisionReason string

const (
	// GroupNoMembership: the key belongs to no group — the group
	// layer imposes nothing (Allowed is true; the per-key policy
	// still governs).
	GroupNoMembership GroupDecisionReason = "no_membership"
	// GroupAllowed: an active resolved group allows the model.
	GroupAllowed GroupDecisionReason = "allowed"
	// GroupDenied: a deny rule in a resolved group (active or
	// inactive) matches the model — deny-wins.
	GroupDenied GroupDecisionReason = "denied"
	// GroupNoGrant: resolved groups exist and at least one is
	// active, but no active group allows the model.
	GroupNoGrant GroupDecisionReason = "no_grant"
	// GroupMissing: none of the key's memberships resolves to an
	// existing group (all deleted / typo'd) — FAIL CLOSED.
	GroupMissing GroupDecisionReason = "groups_missing"
	// GroupAllInactive: every resolved group is disabled, so no
	// grant is possible — FAIL CLOSED.
	GroupAllInactive GroupDecisionReason = "groups_all_inactive"
	// GroupUnreadable: the stored group list is malformed —
	// FAIL CLOSED.
	GroupUnreadable GroupDecisionReason = "groups_unreadable"
)

// GroupDecision is the outcome of evaluating one key's groups
// for one model. Allowed false always means refuse, whatever
// the reason; FailClosed reports whether the refusal is a
// fail-closed refusal (missing / all-inactive / unreadable
// groups) rather than a plain policy refusal, so the server can
// name the cause in the 403.
type GroupDecision struct {
	Allowed    bool
	Reason     GroupDecisionReason
	FailClosed bool
}

// GroupModelDecision evaluates key's groups for model (from
// provider, "" when unknown — the request path). The per-key
// policy (ModelAllowed) is NOT consulted here: the caller
// applies both layers, and a request must pass both. An empty
// model is allowed here, matching ModelAllowed's convention:
// the caller skips the check when the request names no model.
func (c *Config) GroupModelDecision(key GatewayKey, model, provider string) GroupDecision {
	if model == "" {
		return GroupDecision{Allowed: true, Reason: GroupAllowed}
	}
	if len(key.KeyGroups) == 0 {
		return GroupDecision{Allowed: true, Reason: GroupNoMembership}
	}
	groups, err := c.KeyGroups()
	if err != nil {
		return GroupDecision{Allowed: false, Reason: GroupUnreadable, FailClosed: true}
	}
	var resolved []KeyGroup
	for _, id := range key.KeyGroups {
		if g, ok := FindKeyGroup(groups, id); ok {
			resolved = append(resolved, g)
		}
	}
	if len(resolved) == 0 {
		return GroupDecision{Allowed: false, Reason: GroupMissing, FailClosed: true}
	}
	// All-inactive FIRST: when every resolved group is disabled no
	// grant is possible, so the key fails closed as a whole — even
	// for a model a disabled group would deny. The fail-closed
	// verdict names the broken setup (all groups inactive) instead
	// of looking like a per-model policy denial. Deny-wins below
	// only applies once at least one group is active.
	anyActive := false
	for _, g := range resolved {
		if !g.Disabled {
			anyActive = true
			break
		}
	}
	if !anyActive {
		return GroupDecision{Allowed: false, Reason: GroupAllInactive, FailClosed: true}
	}
	// Deny-wins, over active AND inactive groups: an inactive
	// group's denies still refuse.
	for _, g := range resolved {
		if g.DeniesModel(model, provider) {
			return GroupDecision{Allowed: false, Reason: GroupDenied}
		}
	}
	for _, g := range resolved {
		if g.AllowsModel(model, provider) {
			return GroupDecision{Allowed: true, Reason: GroupAllowed}
		}
	}
	return GroupDecision{Allowed: false, Reason: GroupNoGrant}
}

// GroupModelAllowed reports whether key's groups admit model
// (from provider, "" when unknown). Convenience wrapper over
// GroupModelDecision for callers that need only the verdict.
func (c *Config) GroupModelAllowed(key GatewayKey, model, provider string) bool {
	return c.GroupModelDecision(key, model, provider).Allowed
}
