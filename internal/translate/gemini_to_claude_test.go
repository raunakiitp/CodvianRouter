package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertGeminiRequestToClaudeRoundTrip converts a Gemini request
// into an Anthropic request and back, asserting the conversation
// round-trips semantically. Closing the loop: the claude->gemini
// converter applied to the result reproduces an equivalent Gemini
// request.
func TestConvertGeminiRequestToClaudeRoundTrip(t *testing.T) {
	req := &wire.GeminiRequest{
		SystemInstruction: &wire.GeminiContent{Parts: []wire.GeminiPart{{Text: "be brief"}}},
		Contents: []wire.GeminiContent{
			{Role: "user", Parts: []wire.GeminiPart{{Text: "weather in Seoul?"}}},
			{Role: "model", Parts: []wire.GeminiPart{{
				FunctionCall: &wire.GeminiFunctionCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Seoul"}`)},
			}}},
			{Role: "user", Parts: []wire.GeminiPart{{
				FunctionResponse: &wire.GeminiFunctionResponse{
					Name:     "get_weather",
					Response: json.RawMessage(`{"result":"17C clear"}`),
				},
			}}},
		},
		Tools: []wire.GeminiTool{{FunctionDeclarations: []wire.GeminiFunctionDeclaration{{
			Name:        "get_weather",
			Description: "weather",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}}},
		GenerationConfig: &wire.GeminiGenerationConfig{Temperature: f64ptr(0.4)},
	}
	out, err := ConvertGeminiRequestToClaude(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToClaude: %v", err)
	}
	if out.System != "be brief" {
		t.Errorf("system = %q", out.System)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "get_weather" {
		t.Errorf("tools = %+v", out.Tools)
	}
	if out.Temperature == nil || *out.Temperature != 0.4 {
		t.Errorf("temperature = %v", out.Temperature)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	m1 := out.Messages[1]
	if m1.Role != "assistant" || len(m1.Content) != 1 || m1.Content[0].Type != "tool_use" ||
		m1.Content[0].Name != "get_weather" {
		t.Fatalf("assistant turn = %+v", m1)
	}
	var input map[string]any
	if err := json.Unmarshal(m1.Content[0].Input, &input); err != nil || input["city"] != "Seoul" {
		t.Errorf("tool_use input = %s (err %v)", m1.Content[0].Input, err)
	}
	m2 := out.Messages[2]
	if m2.Role != "user" || len(m2.Content) != 1 || m2.Content[0].Type != "tool_result" ||
		m2.Content[0].ToolUseID != m1.Content[0].ID {
		t.Fatalf("tool result turn = %+v (want tool_use_id %q)", m2, m1.Content[0].ID)
	}
	if m2.Content[0].Content != "17C clear" {
		t.Errorf("tool result content = %v", m2.Content[0].Content)
	}

	// Close the loop: claude->gemini on the result reproduces an
	// equivalent Gemini request (same roles, same function name, same
	// response payload; tool ids are minted differently).
	back, err := ConvertClaudeRequestToGemini(out)
	if err != nil {
		t.Fatalf("ConvertClaudeRequestToGemini (loop): %v", err)
	}
	raw1, _ := json.Marshal(req.Contents)
	raw2, _ := json.Marshal(back.Contents)
	if !equivalentGeminiContents(t, string(raw1), string(raw2)) {
		t.Errorf("loop contents differ:\noriginal: %s\nround trip: %s", raw1, raw2)
	}
}

// equivalentGeminiContents compares two Gemini contents JSON blobs
// semantically: same number of contents, same roles, same part kinds
// and payloads (minted tool ids are not compared).
func equivalentGeminiContents(t *testing.T, a, b string) bool {
	t.Helper()
	var ca, cb []wire.GeminiContent
	if err := json.Unmarshal([]byte(a), &ca); err != nil || json.Unmarshal([]byte(b), &cb) != nil {
		return false
	}
	if len(ca) != len(cb) {
		return false
	}
	for i := range ca {
		if ca[i].Role != cb[i].Role || len(ca[i].Parts) != len(cb[i].Parts) {
			return false
		}
		for j := range ca[i].Parts {
			pa, pb := ca[i].Parts[j], cb[i].Parts[j]
			if (pa.Text != pb.Text) || (pa.Thought != pb.Thought) {
				return false
			}
			if (pa.FunctionCall == nil) != (pb.FunctionCall == nil) ||
				(pa.FunctionResponse == nil) != (pb.FunctionResponse == nil) {
				return false
			}
			if pa.FunctionCall != nil {
				if pa.FunctionCall.Name != pb.FunctionCall.Name ||
					strings.TrimSpace(string(pa.FunctionCall.Args)) != strings.TrimSpace(string(pb.FunctionCall.Args)) {
					return false
				}
			}
			if pa.FunctionResponse != nil {
				if pa.FunctionResponse.Name != pb.FunctionResponse.Name ||
					strings.TrimSpace(string(pa.FunctionResponse.Response)) != strings.TrimSpace(string(pb.FunctionResponse.Response)) {
					return false
				}
			}
		}
	}
	return true
}

// TestClaudeGeminiImageRoundTrip covers base64 image blocks in both
// directions.
func TestClaudeGeminiImageRoundTrip(t *testing.T) {
	req := &wire.AnthropicRequest{
		MaxTokens: 64,
		Messages: []wire.AnthropicMessage{
			{Role: "user", Content: []wire.AnthropicBlock{
				{Type: "image", Source: &wire.AnthropicImageSource{Type: "base64", MediaType: "image/png", Data: "QUJD"}},
				{Type: "text", Text: "what is this?"},
			}},
		},
	}
	gem, err := ConvertClaudeRequestToGemini(req)
	if err != nil {
		t.Fatalf("ConvertClaudeRequestToGemini: %v", err)
	}
	part := gem.Contents[0].Parts[0]
	if part.InlineData == nil || part.InlineData.MimeType != "image/png" || part.InlineData.Data != "QUJD" {
		t.Fatalf("inlineData = %+v", part.InlineData)
	}
	back, err := ConvertGeminiRequestToClaude(gem)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToClaude: %v", err)
	}
	blk := back.Messages[0].Content[0]
	if blk.Type != "image" || blk.Source == nil || blk.Source.MediaType != "image/png" || blk.Source.Data != "QUJD" {
		t.Fatalf("image block = %+v", blk)
	}
}

func f64ptr(v float64) *float64 {
	return &v
}

func TestConvertGeminiRequestToClaudeRefusals(t *testing.T) {
	t.Run("unsigned thought text", func(t *testing.T) {
		req := &wire.GeminiRequest{Contents: []wire.GeminiContent{{
			Role: "model", Parts: []wire.GeminiPart{{Text: "hmm", Thought: true}},
		}}}
		if _, err := ConvertGeminiRequestToClaude(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("orphan function response", func(t *testing.T) {
		req := &wire.GeminiRequest{Contents: []wire.GeminiContent{{
			Role: "user", Parts: []wire.GeminiPart{{
				FunctionResponse: &wire.GeminiFunctionResponse{Name: "get_weather", Response: json.RawMessage(`{"result":"x"}`)},
			}},
		}}}
		if _, err := ConvertGeminiRequestToClaude(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("bare thought signature", func(t *testing.T) {
		req := &wire.GeminiRequest{Contents: []wire.GeminiContent{{
			Role: "model", Parts: []wire.GeminiPart{{ThoughtSignature: "sig"}},
		}}}
		if _, err := ConvertGeminiRequestToClaude(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
}

func TestConvertGeminiRequestToClaudeToolConfig(t *testing.T) {
	// Mode NONE drops the tools (no Anthropic tool_choice "none").
	req := &wire.GeminiRequest{
		Contents: []wire.GeminiContent{{Role: "user", Parts: []wire.GeminiPart{{Text: "hi"}}}},
		Tools: []wire.GeminiTool{{FunctionDeclarations: []wire.GeminiFunctionDeclaration{{
			Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`),
		}}}},
		ToolConfig: &wire.GeminiToolConfig{FunctionCallingConfig: wire.GeminiFunctionCallingConfig{Mode: "NONE"}},
	}
	out, err := ConvertGeminiRequestToClaude(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToClaude: %v", err)
	}
	if len(out.Tools) != 0 || out.ToolChoice != nil {
		t.Errorf("tools/toolChoice = %+v/%+v (want dropped)", out.Tools, out.ToolChoice)
	}

	// Mode ANY forces any; a single allowed name forces that tool.
	reqAny := &wire.GeminiRequest{
		Contents: []wire.GeminiContent{{Role: "user", Parts: []wire.GeminiPart{{Text: "hi"}}}},
		Tools: []wire.GeminiTool{{FunctionDeclarations: []wire.GeminiFunctionDeclaration{{
			Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`),
		}}}},
		ToolConfig: &wire.GeminiToolConfig{FunctionCallingConfig: wire.GeminiFunctionCallingConfig{
			Mode:                 "ANY",
			AllowedFunctionNames: []string{"get_weather"},
		}},
	}
	outAny, err := ConvertGeminiRequestToClaude(reqAny)
	if err != nil {
		t.Fatalf("ConvertGeminiRequestToClaude (ANY): %v", err)
	}
	if outAny.ToolChoice == nil || outAny.ToolChoice.Type != "tool" || outAny.ToolChoice.Name != "get_weather" {
		t.Errorf("toolChoice = %+v", outAny.ToolChoice)
	}
}
