package translate

import (
	"encoding/json"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertClaudeRequestToGeminiRoundTrip converts an Anthropic
// request into a Gemini request, re-encodes it through JSON, and — to
// close the loop — runs the Gemini result through the existing wire's
// Gemini conversion shape expectations. The conversation round-trips
// semantically: system -> systemInstruction, turns -> contents with
// roles, tool_use -> functionCall, tool_result -> functionResponse.
func TestConvertClaudeRequestToGeminiRoundTrip(t *testing.T) {
	temp := 0.4
	req := &wire.AnthropicRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 2048,
		System:    "be brief",
		Messages: []wire.AnthropicMessage{
			{Role: "user", Content: []wire.AnthropicBlock{
				{Type: "text", Text: "weather in Seoul?"},
			}},
			{Role: "assistant", Content: []wire.AnthropicBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "get_weather", Input: json.RawMessage(`{"city":"Seoul"}`)},
			}},
			{Role: "user", Content: []wire.AnthropicBlock{
				{Type: "tool_result", ToolUseID: "toolu_1", Content: "17C clear"},
				{Type: "text", Text: "thanks"},
			}},
		},
		Temperature: &temp,
		Tools: []wire.AnthropicTool{{
			Name:        "get_weather",
			Description: "weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	out, err := ConvertClaudeRequestToGemini(req)
	if err != nil {
		t.Fatalf("ConvertClaudeRequestToGemini: %v", err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded wire.GeminiRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if decoded.SystemInstruction == nil || len(decoded.SystemInstruction.Parts) != 1 ||
		decoded.SystemInstruction.Parts[0].Text != "be brief" {
		t.Errorf("systemInstruction = %+v", decoded.SystemInstruction)
	}
	if len(decoded.Tools) != 1 || len(decoded.Tools[0].FunctionDeclarations) != 1 ||
		decoded.Tools[0].FunctionDeclarations[0].Name != "get_weather" {
		t.Errorf("tools = %+v", decoded.Tools)
	}
	if decoded.GenerationConfig == nil || decoded.GenerationConfig.Temperature == nil ||
		*decoded.GenerationConfig.Temperature != 0.4 || *decoded.GenerationConfig.MaxOutputTokens != 2048 {
		t.Errorf("generationConfig = %+v", decoded.GenerationConfig)
	}
	// Consecutive user turns merge into alternating contents: the
	// tool result and the "thanks" text share one user content.
	if len(decoded.Contents) != 3 {
		t.Fatalf("contents = %d: %s", len(decoded.Contents), raw)
	}
	if decoded.Contents[0].Role != "user" || decoded.Contents[0].Parts[0].Text != "weather in Seoul?" {
		t.Errorf("contents[0] = %+v", decoded.Contents[0])
	}
	if decoded.Contents[1].Role != "model" || decoded.Contents[1].Parts[0].FunctionCall == nil ||
		decoded.Contents[1].Parts[0].FunctionCall.Name != "get_weather" {
		t.Errorf("contents[1] = %+v", decoded.Contents[1])
	}
	var args map[string]any
	if err := json.Unmarshal(decoded.Contents[1].Parts[0].FunctionCall.Args, &args); err != nil || args["city"] != "Seoul" {
		t.Errorf("functionCall args = %s (err %v)", decoded.Contents[1].Parts[0].FunctionCall.Args, err)
	}
	if decoded.Contents[2].Role != "user" || decoded.Contents[2].Parts[0].FunctionResponse == nil ||
		decoded.Contents[2].Parts[0].FunctionResponse.Name != "get_weather" {
		t.Errorf("contents[2] = %+v", decoded.Contents[2])
	}
	var respVal map[string]any
	if err := json.Unmarshal(decoded.Contents[2].Parts[0].FunctionResponse.Response, &respVal); err != nil ||
		respVal["result"] != "17C clear" {
		t.Errorf("functionResponse = %s (err %v)", decoded.Contents[2].Parts[0].FunctionResponse.Response, err)
	}
	if decoded.Contents[2].Parts[1].Text != "thanks" {
		t.Errorf("contents[2] parts[1] = %+v", decoded.Contents[2].Parts[1])
	}
}

func TestConvertClaudeRequestToGeminiThinking(t *testing.T) {
	// A signed thinking block replays as a signed thought part; the
	// thinking budget maps onto thinkingConfig.
	req := &wire.AnthropicRequest{
		MaxTokens: 1024,
		Messages: []wire.AnthropicMessage{
			{Role: "user", Content: []wire.AnthropicBlock{{Type: "text", Text: "go"}}},
			{Role: "assistant", Content: []wire.AnthropicBlock{{
				Type: "thinking", Thinking: "hmm", Signature: "sig1",
			}, {Type: "text", Text: "hi"}}},
			{Role: "user", Content: []wire.AnthropicBlock{{Type: "text", Text: "go on"}}},
		},
		Thinking: &wire.AnthropicThinking{Type: "enabled", BudgetTokens: 4096},
	}
	out, err := ConvertClaudeRequestToGemini(req)
	if err != nil {
		t.Fatalf("ConvertClaudeRequestToGemini: %v", err)
	}
	if out.GenerationConfig == nil || out.GenerationConfig.ThinkingConfig == nil ||
		out.GenerationConfig.ThinkingConfig.ThinkingBudget != 4096 {
		t.Fatalf("thinkingConfig = %+v", out.GenerationConfig)
	}
	found := false
	for _, c := range out.Contents {
		for _, p := range c.Parts {
			if p.Thought && p.Text == "hmm" && p.ThoughtSignature == "sig1" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("signed thought part missing: %+v", out.Contents)
	}
}

func TestConvertClaudeRequestToGeminiRefusals(t *testing.T) {
	base := func(blocks ...wire.AnthropicBlock) *wire.AnthropicRequest {
		return &wire.AnthropicRequest{
			MaxTokens: 64,
			Messages: []wire.AnthropicMessage{
				{Role: "user", Content: blocks},
			},
		}
	}
	t.Run("redacted thinking", func(t *testing.T) {
		if _, err := ConvertClaudeRequestToGemini(base(
			wire.AnthropicBlock{Type: "redacted_thinking", Data: "opaque"})); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("orphan tool result", func(t *testing.T) {
		if _, err := ConvertClaudeRequestToGemini(base(
			wire.AnthropicBlock{Type: "tool_result", ToolUseID: "nope", Content: "x"})); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("url image", func(t *testing.T) {
		if _, err := ConvertClaudeRequestToGemini(base(
			wire.AnthropicBlock{Type: "image", Source: &wire.AnthropicImageSource{Type: "url", URL: "https://example.com/a.png"}})); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
}
