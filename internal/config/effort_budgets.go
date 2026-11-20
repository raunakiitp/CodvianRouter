// Operator-declared effort→budget table (CHECKLIST 285-286,
// Vertex added 303-304).
//
// Anthropic, Gemini and Vertex take a numeric thinking budget,
// not a named effort — Anthropic `thinking.budget_tokens`,
// Gemini and Vertex `thinkingConfig.thinkingBudget` (Vertex uses
// the Gemini API shape, so the budget rides the same field) —
// so a named effort (low / medium / high, ...) can only reach
// those wires through a table the operator declares in config:
//
//	"reasoning": {
//	  "effort_budgets": {
//	    "anthropic": {"low": 1024, "medium": 4096, "high": 16384},
//	    "gemini":    {"low": 1024, "medium": 4096, "high": 16384},
//	    "vertex":    {"low": 1024, "medium": 4096, "high": 16384}
//	  }
//	}
//
// Each wire has its own entry in the table: a budget declared
// for "gemini" does NOT apply to Vertex (or vice versa) — the
// operator declares each wire's budgets explicitly, and the
// engine never copies one wire's numbers onto another. The
// table is the ONLY source of budgets: there are no defaults
// and none are invented. An effort with no entry sends no
// thinking field at all, and the trace records
// "not-forwarded:no-operator-budget-table" for that attempt, so
// the gap stays visible instead of silently filled with a
// guessed number. Cohere and Bedrock Converse have no effort
// control at all, so no table entry can ever apply to them —
// the router records "not-forwarded:no-effort-control" for
// those wires.
package config

import (
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ReasoningConfig tunes reasoning-effort forwarding (CHECKLIST
// 285-286, Vertex 303-304). Nil means defaults: no effort→budget table, so
// Anthropic/Gemini/Vertex attempts send no thinking field. A pointer so
// omitempty drops the key entirely when unset.
type ReasoningConfig struct {
	// EffortBudgets is the operator-declared effort→budget
	// table (see EffortBudgetTable). Nil means no table is
	// declared.
	EffortBudgets *EffortBudgetTable `json:"effort_budgets,omitempty"`
	// ForcedEffort is the operator-declared forced-reasoning
	// policy (CHECKLIST 311-312, see forced_effort.go): rules
	// forcing a reasoning effort on a model or route. Nil or
	// empty means no policy is declared and no effort is ever
	// forced.
	ForcedEffort []ForcedEffortPolicy `json:"forced_effort,omitempty"`
}

// EffortBudgetTable maps a named reasoning-effort level to a
// numeric thinking-token budget, per wire that takes a numeric
// budget instead of a named effort. "anthropic", "gemini" and
// "vertex" exist: no other wire accepts a numeric thinking
// budget, and adding a key for one would silently do nothing, so
// the table has exactly these three fields. Vertex uses the
// Gemini API shape but has its own entry — a "gemini" budget is
// never copied onto Vertex. Keys are effort levels in their
// normalized (lowercase, trimmed) form; values are token budgets
// and must be > 0.
type EffortBudgetTable struct {
	Anthropic map[string]int `json:"anthropic,omitempty"`
	Gemini    map[string]int `json:"gemini,omitempty"`
	Vertex    map[string]int `json:"vertex,omitempty"`
}

// Lookup returns the operator-declared budget for one (wire,
// effort) pair. It reports false when there is no table, when the
// wire takes no numeric budget (anything but "anthropic" /
// "gemini" / "vertex"), when the effort has no entry, or when the
// entry is not a positive budget — every one of those is "send nothing",
// never a guessed number. The effort is normalized before the
// lookup, so "Low" and "low" mean the same entry.
func (t *EffortBudgetTable) Lookup(wire, effort string) (int, bool) {
	if t == nil {
		return 0, false
	}
	var m map[string]int
	switch wire {
	case provider.WireFormatAnthropic:
		m = t.Anthropic
	case provider.WireFormatGemini:
		m = t.Gemini
	case provider.WireFormatVertex:
		m = t.Vertex
	default:
		return 0, false
	}
	budget, ok := m[provider.NormalizeReasoningEffort(effort)]
	if !ok || budget <= 0 {
		return 0, false
	}
	return budget, true
}

// Validate checks the table: every key must be a known effort
// level in normalized form ("none" is rejected — it means "no
// reasoning", so a budget for it is meaningless), and every
// budget must be > 0. A nil table is always valid.
func (t *EffortBudgetTable) Validate() error {
	if t == nil {
		return nil
	}
	for wire, m := range map[string]map[string]int{
		provider.WireFormatAnthropic: t.Anthropic,
		provider.WireFormatGemini:    t.Gemini,
		provider.WireFormatVertex:    t.Vertex,
	} {
		for effort, budget := range m {
			if effort != provider.NormalizeReasoningEffort(effort) || !provider.ValidReasoningEffort(effort) {
				return fmt.Errorf("config: reasoning.effort_budgets.%s key %q is not a known effort level (low, medium, high, minimal, xhigh)", wire, effort)
			}
			if effort == provider.ReasoningEffortNone {
				return fmt.Errorf("config: reasoning.effort_budgets.%s key \"none\" is meaningless (\"none\" means no reasoning, so it takes no budget)", wire)
			}
			if budget <= 0 {
				return fmt.Errorf("config: reasoning.effort_budgets.%s[%q] must be > 0 (got %d)", wire, effort, budget)
			}
		}
	}
	return nil
}
