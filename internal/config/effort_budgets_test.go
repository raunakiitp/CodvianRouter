// Operator-declared effort→budget table (CHECKLIST 285-286):
// the table is the only source of thinking budgets — no defaults
// are invented — and a bad entry fails config validation with
// the field named.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEffortBudgetTableLookup(t *testing.T) {
	table := &EffortBudgetTable{
		Anthropic: map[string]int{"low": 1024, "high": 16384},
		Gemini:    map[string]int{"low": 2048},
	}
	for _, tc := range []struct {
		wire, effort string
		want         int
		ok           bool
	}{
		{"anthropic", "low", 1024, true},
		{"anthropic", "high", 16384, true},
		{"anthropic", "Low", 1024, true}, // normalized lookup
		{"gemini", "low", 2048, true},
		{"gemini", "high", 0, false},      // no entry: send nothing
		{"anthropic", "medium", 0, false}, // no entry: send nothing
		{"openai", "low", 0, false},       // named-effort wire: no budget
		{"cohere", "low", 0, false},       // no effort control at all
		{"bedrock-converse", "low", 0, false},
	} {
		got, ok := table.Lookup(tc.wire, tc.effort)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("Lookup(%q, %q) = (%d, %v), want (%d, %v)", tc.wire, tc.effort, got, ok, tc.want, tc.ok)
		}
	}
	// No table at all: nothing is ever resolved.
	var nilTable *EffortBudgetTable
	if _, ok := nilTable.Lookup("anthropic", "low"); ok {
		t.Fatalf("nil table Lookup returned ok=true, want false")
	}
	// An entry with a non-positive budget resolves to nothing —
	// a zero/negative budget must never be sent.
	zero := &EffortBudgetTable{Anthropic: map[string]int{"low": 0}}
	if _, ok := zero.Lookup("anthropic", "low"); ok {
		t.Fatalf("zero-budget Lookup returned ok=true, want false")
	}
}

func TestEffortBudgetTableValidate(t *testing.T) {
	good := &EffortBudgetTable{
		Anthropic: map[string]int{"low": 1024, "medium": 4096, "high": 16384, "minimal": 512, "xhigh": 32768},
		Gemini:    map[string]int{"low": 1024},
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("good table Validate: %v", err)
	}
	var nilTable *EffortBudgetTable
	if err := nilTable.Validate(); err != nil {
		t.Fatalf("nil table Validate: %v", err)
	}
	for _, tc := range []struct {
		name  string
		table *EffortBudgetTable
		want  string
	}{
		{"unknown effort level", &EffortBudgetTable{Anthropic: map[string]int{"turbo": 1024}}, "turbo"},
		{"unnormalized key", &EffortBudgetTable{Anthropic: map[string]int{"Low": 1024}}, "Low"},
		{"none takes no budget", &EffortBudgetTable{Anthropic: map[string]int{"none": 1024}}, "none"},
		{"zero budget", &EffortBudgetTable{Gemini: map[string]int{"low": 0}}, "low"},
		{"negative budget", &EffortBudgetTable{Gemini: map[string]int{"high": -5}}, "high"},
	} {
		if err := tc.table.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Validate = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

func TestEffortBudgetTableConfigRoundTrip(t *testing.T) {
	// The table loads from the `reasoning.effort_budgets` key,
	// survives a save/load round-trip, and validates at load.
	raw := `{"version":1,"listen":"127.0.0.1:20128","data_dir":"/tmp/x",
		"reasoning":{"effort_budgets":{"anthropic":{"low":1024,"high":16384},"gemini":{"low":2048}}}}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if c.Reasoning == nil || c.Reasoning.EffortBudgets == nil {
		t.Fatalf("Reasoning table missing after Unmarshal: %+v", c.Reasoning)
	}
	if got, ok := c.Reasoning.EffortBudgets.Lookup("anthropic", "high"); !ok || got != 16384 {
		t.Fatalf("Lookup(anthropic, high) = (%d, %v), want (16384, true)", got, ok)
	}
	if err := c.Reasoning.EffortBudgets.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := json.Marshal(&c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var c2 Config
	if err := json.Unmarshal(out, &c2); err != nil {
		t.Fatalf("re-Unmarshal: %v", err)
	}
	if got, ok := c2.Reasoning.EffortBudgets.Lookup("gemini", "low"); !ok || got != 2048 {
		t.Fatalf("round-trip Lookup(gemini, low) = (%d, %v), want (2048, true)", got, ok)
	}
	// A bad table fails Config.Validate with the field named.
	bad := `{"version":1,"listen":"127.0.0.1:20128","data_dir":"/tmp/x",
		"reasoning":{"effort_budgets":{"anthropic":{"low":0}}}}`
	var cb Config
	if err := json.Unmarshal([]byte(bad), &cb); err != nil {
		t.Fatalf("Unmarshal bad: %v", err)
	}
	if err := cb.Validate(); err == nil || !strings.Contains(err.Error(), "reasoning.effort_budgets.anthropic") {
		t.Fatalf("Validate bad table = %v, want an error naming reasoning.effort_budgets.anthropic", err)
	}
}
