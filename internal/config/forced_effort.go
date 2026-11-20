// Operator-declared forced-reasoning policy (CHECKLIST 311-312).
//
// A policy forces a reasoning effort on a model or route —
// e.g. always-high for a coding tier — whatever the client asked
// for. It is declared in config:
//
//	"reasoning": {
//	  "forced_effort": [
//	    {"model": "auto/coding", "effort": "high"},
//	    {"model": "model-b", "effort": "high", "allow_client_override": true}
//	  ]
//	}
//
// Model matches the model as the client named it (a concrete
// model such as "gpt-5", or a virtual model / route such as
// "auto/coding" or "intent/coding") or the model the auto-router
// resolved it to; "*" matches every model and is the fallback
// when no exact rule matches. The first exact match wins, in
// declared order; "*" is consulted only when nothing exact
// matched, so a specific rule always beats the wildcard.
//
// Precedence (see internal/router/forced_effort.go): a matched
// policy beats the intent-tier effort, a routing-group entry's
// effort, and the client's own reasoning_effort — in that order —
// UNLESS the policy sets allow_client_override, in which case a
// client-supplied reasoning_effort wins over the policy (and
// over the tier and group entry too: the flag is the only way a
// client value wins at all). With no client value, an
// override-allowed policy still forces its effort. The trace
// records which won per attempt ("forced:high:policy:auto/coding"
// or "client-override:low:policy:model-b"), so a forced effort
// is never silent.
package config

import (
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ForcedEffortPolicy is one operator-declared forced-reasoning
// rule (CHECKLIST 311-312). Model is the model or route the rule
// matches (a concrete model, a virtual model such as
// "auto/coding", or "*" for every model); Effort is the reasoning
// effort the rule forces; AllowClientOverride, when true, lets a
// client-supplied reasoning_effort win over this rule (default
// false: the policy wins).
type ForcedEffortPolicy struct {
	Model               string `json:"model"`
	Effort              string `json:"effort"`
	AllowClientOverride bool   `json:"allow_client_override,omitempty"`
}

// ForcedEffortFor returns the rule matching one model: the first
// exact match in declared order, else the first "*" rule, else
// not found. A nil receiver (no reasoning block) never matches.
func (c *ReasoningConfig) ForcedEffortFor(model string) (ForcedEffortPolicy, bool) {
	if c == nil {
		return ForcedEffortPolicy{}, false
	}
	for _, p := range c.ForcedEffort {
		if p.Model == model {
			return p, true
		}
	}
	for _, p := range c.ForcedEffort {
		if p.Model == "*" {
			return p, true
		}
	}
	return ForcedEffortPolicy{}, false
}

// ValidateForcedEffort checks the forced-effort rules: every
// model must be non-empty, every effort a known level in
// normalized form ("none" is allowed here — forcing no reasoning
// is a real policy, unlike a budget for "none", which is
// meaningless), and no model may appear twice (the first match
// wins, so a duplicate could only be a typo shadowing a rule).
// An empty list is always valid.
func (c *ReasoningConfig) ValidateForcedEffort() error {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(c.ForcedEffort))
	for _, p := range c.ForcedEffort {
		if strings.TrimSpace(p.Model) == "" {
			return fmt.Errorf("config: reasoning.forced_effort has an entry with an empty model")
		}
		if p.Effort != provider.NormalizeReasoningEffort(p.Effort) || !provider.ValidReasoningEffort(p.Effort) {
			return fmt.Errorf("config: reasoning.forced_effort[%q] effort %q is not a known effort level (none, minimal, low, medium, high, xhigh)", p.Model, p.Effort)
		}
		if _, dup := seen[p.Model]; dup {
			return fmt.Errorf("config: reasoning.forced_effort has two rules for model %q (first match wins, so the second could never apply)", p.Model)
		}
		seen[p.Model] = struct{}{}
	}
	return nil
}
