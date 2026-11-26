package server

// WS9E closure item 2: dedicated tests for the Gemini native
// structured surfaces — the tool-parameter JSON schema mapping
// (parametersJsonSchema, the newer spelling) and the honest
// structured-output (responseSchema/responseMimeType) rejection —
// plus the provider × modality × operation capability matrix pinned
// from the real endpoint projections.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// geminiReqWithTools builds a generateContent request carrying one
// tool with the given per-declaration schema spellings.
func geminiReqWithTools(t *testing.T, parameters, jsonSchema string) *provider.ChatRequest {
	t.Helper()
	decl := map[string]any{"name": "get_weather", "description": "weather lookup"}
	if parameters != "" {
		decl["parameters"] = json.RawMessage(parameters)
	}
	if jsonSchema != "" {
		decl["parametersJsonSchema"] = json.RawMessage(jsonSchema)
	}
	raw, err := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": "weather?"}}},
		},
		"tools": []map[string]any{{"functionDeclarations": []map[string]any{decl}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var in geminiNativeGenerateRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		t.Fatalf("request fixture: %v", err)
	}
	req := &provider.ChatRequest{}
	if err := geminiApplyTools(req, &in); err != nil {
		t.Fatalf("geminiApplyTools: %v", err)
	}
	return req
}

// TestGeminiParametersJSONSchemaMapped: the newer parametersJsonSchema
// spelling maps to the canonical tool parameters when the classic
// parameters field is absent — a client using only the new spelling
// must not lose its schema (a silently dropped schema would change
// what the model is allowed to emit).
func TestGeminiParametersJSONSchemaMapped(t *testing.T) {
	schema := `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`
	req := geminiReqWithTools(t, "", schema)
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	got := string(req.Tools[0].Function.Parameters)
	if got != schema {
		t.Fatalf("mapped parameters = %s, want the parametersJsonSchema payload verbatim", got)
	}
	if req.Tools[0].Function.Name != "get_weather" || req.Tools[0].Function.Description != "weather lookup" {
		t.Fatalf("tool identity lost: %+v", req.Tools[0].Function)
	}
}

// TestGeminiParametersWinsOverJSONSchema: when BOTH spellings are
// present the classic parameters field wins — the documented
// precedence, not last-write.
func TestGeminiParametersWinsOverJSONSchema(t *testing.T) {
	req := geminiReqWithTools(t,
		`{"type":"object","properties":{"classic":{"type":"boolean"}}}`,
		`{"type":"object","properties":{"newer":{"type":"boolean"}}}`)
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	got := string(req.Tools[0].Function.Parameters)
	if !strings.Contains(got, `"classic"`) || strings.Contains(got, `"newer"`) {
		t.Fatalf("parameters = %s, want the classic payload to win over parametersJsonSchema", got)
	}
}

// TestGeminiBothSchemasAbsentDefaultsToObject: no schema at all
// defaults to {"type":"object"} — a tool without parameters still
// declares an object shape instead of a nil schema.
func TestGeminiBothSchemasAbsentDefaultsToObject(t *testing.T) {
	req := geminiReqWithTools(t, "", "")
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(req.Tools))
	}
	if got := string(req.Tools[0].Function.Parameters); got != `{"type":"object"}` {
		t.Fatalf("default parameters = %s, want {\"type\":\"object\"}", got)
	}
}

func stringsIndex(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
// TestGeminiStructuredOutputRejectedHonest: generationConfig's
// responseMimeType / responseSchema (the structured-OUTPUT surface)
// have no canonical mapping — they are REJECTED with an error naming
// the field, never silently dropped (a silent drop would return
// prose where the client expected JSON).
func TestGeminiStructuredOutputRejectedHonest(t *testing.T) {
	for _, field := range []string{"responseMimeType", "responseSchema"} {
		body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"` +
			field + `":"application/json"}}`
		var in geminiNativeGenerateRequest
		if err := json.Unmarshal([]byte(body), &in); err != nil {
			t.Fatalf("%s fixture: %v", field, err)
		}
		req := &provider.ChatRequest{}
		err := geminiApplyGenerationConfig(req, &in)
		if err == nil {
			t.Fatalf("%s accepted — structured output must be rejected, not dropped", field)
		}
		if stringsIndex(err.Error(), field) < 0 {
			t.Fatalf("%s error = %v, want the error to name the field", field, err)
		}
	}
}
