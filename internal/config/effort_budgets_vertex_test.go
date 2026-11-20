// Vertex effort→budget table (CHECKLIST 303-304): the "vertex"
// entry of reasoning.effort_budgets — its own entry, never a
// "gemini" budget copied over — plus the config round-trip.
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEffortBudgetTableVertexLookup(t *testing.T) {
	table := &EffortBudgetTable{
		Gemini: map[string]int{"low": 2048},
		Vertex: map[string]int{"low": 1024, "high": 16384},
	}
	// Vertex reads its own entry.
	if got, ok := table.Lookup("vertex", "low"); !ok || got != 1024 {
		t.Fatalf("Lookup(vertex, low) = (%d, %v), want (1024, true)", got, ok)
	}
	if got, ok := table.Lookup("vertex", "high"); !ok || got != 16384 {
		t.Fatalf("Lookup(vertex, high) = (%d, %v), want (16384, true)", got, ok)
	}
	// A gemini entry never leaks onto Vertex, and vice versa.
	if got, ok := table.Lookup("vertex", "low"); ok && got == 2048 {
		t.Fatalf("Lookup(vertex, low) returned the gemini budget 2048")
	}
	if _, ok := table.Lookup("gemini", "high"); ok {
		t.Fatalf("Lookup(gemini, high) = true, want false (only vertex maps high)")
	}
	// Unmapped effort and unnormalized effort behave as before.
	if _, ok := table.Lookup("vertex", "medium"); ok {
		t.Fatalf("Lookup(vertex, medium) = true, want false (no entry)")
	}
	if got, ok := table.Lookup("vertex", "Low"); !ok || got != 1024 {
		t.Fatalf("Lookup(vertex, Low) = (%d, %v), want (1024, true) (normalized)", got, ok)
	}
	var nilTable *EffortBudgetTable
	if _, ok := nilTable.Lookup("vertex", "low"); ok {
		t.Fatalf("nil table Lookup(vertex, low) = true, want false")
	}
}

func TestEffortBudgetTableVertexValidate(t *testing.T) {
	good := &EffortBudgetTable{Vertex: map[string]int{"low": 1024, "high": 16384}}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(good vertex table): %v", err)
	}
	for _, tc := range []struct {
		name  string
		table *EffortBudgetTable
		want  string
	}{
		{"unknown effort level", &EffortBudgetTable{Vertex: map[string]int{"turbo": 1024}}, "turbo"},
		{"unnormalized key", &EffortBudgetTable{Vertex: map[string]int{"Low": 1024}}, "Low"},
		{"none takes no budget", &EffortBudgetTable{Vertex: map[string]int{"none": 1024}}, "none"},
		{"zero budget", &EffortBudgetTable{Vertex: map[string]int{"low": 0}}, "low"},
		{"negative budget", &EffortBudgetTable{Vertex: map[string]int{"high": -5}}, "high"},
	} {
		if err := tc.table.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: Validate err = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

func TestEffortBudgetTableVertexConfigRoundTrip(t *testing.T) {
	raw := `{"reasoning":{"effort_budgets":{"vertex":{"low":1024,"high":16384},"gemini":{"low":2048}}}}`
	var c struct {
		Reasoning *ReasoningConfig `json:"reasoning"`
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Reasoning == nil || c.Reasoning.EffortBudgets == nil {
		t.Fatalf("reasoning.effort_budgets missing after unmarshal: %+v", c)
	}
	if got, ok := c.Reasoning.EffortBudgets.Lookup("vertex", "high"); !ok || got != 16384 {
		t.Fatalf("Lookup(vertex, high) = (%d, %v), want (16384, true)", got, ok)
	}
	if err := c.Reasoning.EffortBudgets.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	out, err := json.Marshal(c.Reasoning.EffortBudgets)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), `"vertex":{"high":16384,"low":1024}`) &&
		!strings.Contains(string(out), `"vertex":{"low":1024,"high":16384}`) {
		t.Fatalf("marshalled table lost the vertex entry: %s", out)
	}
}
