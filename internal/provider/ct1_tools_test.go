// CT-1 — per-model tool-capability flags (GAP-TRACK): tri-state,
// never guessed from the model id.
package provider

import (
	"encoding/json"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// TestModelToolsCapable: nil means unknown — never a
// false-positive capable and never a false-positive incapable.
func TestModelToolsCapable(t *testing.T) {
	cases := []struct {
		name        string
		tools       *bool
		wantCapable bool
		wantKnown   bool
	}{
		{"nil is unknown", nil, false, false},
		{"true is confirmed capable", boolPtr(true), true, true},
		{"false is confirmed incapable", boolPtr(false), false, true},
	}
	for _, tc := range cases {
		m := Model{ID: "m", Tools: tc.tools}
		if capable, known := m.ToolsCapable(); capable != tc.wantCapable || known != tc.wantKnown {
			t.Errorf("%s: ToolsCapable() = (%v, %v), want (%v, %v)",
				tc.name, capable, known, tc.wantCapable, tc.wantKnown)
		}
	}
}

// TestModelToolsJSONRoundTrip: the flag survives the provider
// store (JSON) — nil stays nil (omitted), true/false stay set.
// An operator-set flag in config must not degrade to unknown
// on reload, and an unknown model must not gain a flag.
func TestModelToolsJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools *bool
	}{
		{"unknown", nil},
		{"capable", boolPtr(true)},
		{"incapable", boolPtr(false)},
	} {
		raw, err := json.Marshal(Model{ID: "m", Tools: tc.tools})
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		var back Model
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", tc.name, err)
		}
		if capable, known := back.ToolsCapable(); capable != (tc.tools != nil && *tc.tools) || known != (tc.tools != nil) {
			t.Errorf("%s: round-trip ToolsCapable() = (%v, %v), want capable=%v known=%v (raw %s)",
				tc.name, capable, known, tc.tools != nil && *tc.tools, tc.tools != nil, raw)
		}
	}
}

// TestDashboardModelsSurfacesTools: the dashboard catalog feed
// carries each model's tri-state tool flag — confirmed capable,
// confirmed incapable, and unknown (nil) all survive, so the
// catalog never claims a capability it does not know.
func TestDashboardModelsSurfacesTools(t *testing.T) {
	reg := NewRegistry(nil)
	reg.providers["p1"] = &Provider{
		ID: "p1", Name: "P One", Enabled: true,
		Models: []Model{
			{ID: "m-capable", Tools: boolPtr(true)},
			{ID: "m-incapable", Tools: boolPtr(false)},
			{ID: "m-unknown"},
		},
	}
	reg.order = []string{"p1"}
	got := reg.DashboardModels()
	if len(got) != 3 {
		t.Fatalf("DashboardModels = %d rows, want 3", len(got))
	}
	want := map[string]*bool{
		"m-capable":   boolPtr(true),
		"m-incapable": boolPtr(false),
		"m-unknown":   nil,
	}
	for _, m := range got {
		w, ok := want[m.ID]
		if !ok {
			t.Errorf("unexpected row %q", m.ID)
			continue
		}
		if w == nil {
			if m.Tools != nil {
				t.Errorf("row %q: Tools = %v, want nil (unknown)", m.ID, *m.Tools)
			}
			continue
		}
		if m.Tools == nil || *m.Tools != *w {
			t.Errorf("row %q: Tools = %v, want %v", m.ID, m.Tools, *w)
		}
	}
}
