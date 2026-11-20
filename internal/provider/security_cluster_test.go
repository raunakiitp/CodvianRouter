// Security-cluster tests (CHECKLIST 245, LiteLLM #47028/#51254):
// client-supplied Vertex overrides are detected at decode time,
// and admin deployment pins outrank client-supplied body fields.
package provider

import (
	"encoding/json"
	"testing"
)

func TestDetectVertexOverride(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantField string
		wantValue string
	}{
		{"vertex_location", `{"model":"m","vertex_location":"us-east1"}`, "vertex_location", "us-east1"},
		{"vertex_project", `{"model":"m","vertex_project":"my-project"}`, "vertex_project", "my-project"},
		{"vertex_ai_location", `{"model":"m","vertex_ai_location":"europe-west1"}`, "vertex_ai_location", "europe-west1"},
		{"vertex_ai_project", `{"model":"m","vertex_ai_project":"other-project"}`, "vertex_ai_project", "other-project"},
		{"non-string value is still an override", `{"model":"m","vertex_location":123}`, "vertex_location", "123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ov, ok := DetectVertexOverride([]byte(tc.body))
			if !ok {
				t.Fatalf("DetectVertexOverride(%s) = not found, want field %q", tc.body, tc.wantField)
			}
			if ov.Field != tc.wantField || ov.Value != tc.wantValue {
				t.Errorf("DetectVertexOverride = %+v, want {%q %q}", ov, tc.wantField, tc.wantValue)
			}
		})
	}
	absent := []string{
		`{"model":"m","messages":[]}`,
		`{"model":"m","vertex_location":null}`,
		`{"model":"m","vertex_location":""}`,
		`{"model":"m","vertex_project":null}`,
		`not json at all`,
	}
	for _, body := range absent {
		if ov, ok := DetectVertexOverride([]byte(body)); ok {
			t.Errorf("DetectVertexOverride(%s) = %+v, want absent", body, ov)
		}
	}
}

func TestChatRequestUnmarshalRecordsVertexOverride(t *testing.T) {
	var req ChatRequest
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"vertex_project":"my-project"}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if req.ClientVertexOverride == nil {
		t.Fatalf("ClientVertexOverride = nil, want the override recorded")
	}
	if req.ClientVertexOverride.Field != "vertex_project" || req.ClientVertexOverride.Value != "my-project" {
		t.Errorf("ClientVertexOverride = %+v, want {vertex_project my-project}", req.ClientVertexOverride)
	}
	// A clean body records no override.
	var clean ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), &clean); err != nil {
		t.Fatalf("Unmarshal clean: %v", err)
	}
	if clean.ClientVertexOverride != nil {
		t.Errorf("clean request ClientVertexOverride = %+v, want nil", clean.ClientVertexOverride)
	}
}

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }

func TestApplyFixedParams(t *testing.T) {
	// (b1) A pinned field outranks a conflicting client value.
	req := &ChatRequest{Model: "m", Temperature: 0.9, TopP: 0.8, MaxTokens: 100}
	model := Model{ID: "m", FixedParams: &ModelFixedParams{Temperature: floatPtr(0.2), MaxTokens: intPtr(50)}}
	applied := ApplyFixedParams(req, model)
	if req.Temperature != 0.2 {
		t.Errorf("temperature = %v, want 0.2 (admin pin wins over client 0.9)", req.Temperature)
	}
	if req.MaxTokens != 50 {
		t.Errorf("max_tokens = %v, want 50 (admin pin wins over client 100)", req.MaxTokens)
	}
	// top_p is not pinned: the client's value stands.
	if req.TopP != 0.8 {
		t.Errorf("top_p = %v, want 0.8 (unpinned field keeps the client value)", req.TopP)
	}
	if len(applied) != 2 || applied[0] != "temperature" || applied[1] != "max_tokens" {
		t.Errorf("applied = %v, want [temperature max_tokens]", applied)
	}

	// (b2) The other way: no pin at all — every client value stands.
	req2 := &ChatRequest{Model: "m", Temperature: 0.9, TopP: 0.8, MaxTokens: 100}
	if got := ApplyFixedParams(req2, Model{ID: "m"}); len(got) != 0 {
		t.Errorf("unpinned applied = %v, want none", got)
	}
	if req2.Temperature != 0.9 || req2.TopP != 0.8 || req2.MaxTokens != 100 {
		t.Errorf("unpinned request changed: %+v", req2)
	}

	// A pin applies even when the client sent nothing for it.
	req3 := &ChatRequest{Model: "m"}
	ApplyFixedParams(req3, Model{ID: "m", FixedParams: &ModelFixedParams{Temperature: floatPtr(0.1)}})
	if req3.Temperature != 0.1 {
		t.Errorf("temperature = %v, want 0.1 (pin is the deployment value, not just a ceiling)", req3.Temperature)
	}

	// Nil request / nil params never panic.
	if got := ApplyFixedParams(nil, model); len(got) != 0 {
		t.Errorf("nil request applied = %v, want none", got)
	}
}
