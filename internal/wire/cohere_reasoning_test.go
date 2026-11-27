package wire

// A1 (Item 5, wire slice): reasoning replay EMISSION on the Cohere
// wire. The strategy in internal/reasoning admits a text-bearing
// state for Cohere's reasoning models (command-a-reasoning family,
// per docs.cohere.com) and refuses it for any other model; these
// tests pin the other half — that an admitted state is actually
// EMITTED into the converted request as the documented
// {"type":"thinking","thinking":...} content block, and that a
// non-reasoning model still refuses the hop instead of silently
// dropping the reasoning. Every emission test fails if the
// convertCohereMessage emission is removed.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

func cohereReasoningState() *reasoning.State {
	return &reasoning.State{
		Blocks: []reasoning.Block{
			{Kind: reasoning.KindThinking, Text: "step one: consider the user's intent"},
			{Kind: reasoning.KindThinking, Text: "step two: answer plainly"},
		},
		SourceWire:  reasoning.WireAnthropic,
		SourceModel: "claude-sonnet-4",
	}
}

// cohereAssistantBlocks decodes an assistant message's content list.
func cohereAssistantBlocks(t *testing.T, out *CohereRequest, index int) []map[string]any {
	t.Helper()
	if index >= len(out.Messages) {
		t.Fatalf("messages[%d] missing (have %d)", index, len(out.Messages))
	}
	var blocks []map[string]any
	if err := json.Unmarshal(out.Messages[index].Content, &blocks); err != nil {
		t.Fatalf("assistant content is not a block list: %s (err %v)", out.Messages[index].Content, err)
	}
	return blocks
}

func TestCohereReasoningEmission(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "command-a-reasoning-08-2025",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("explain")},
			{
				Role:      "assistant",
				Content:   mustJSON("the answer is 42"),
				Reasoning: cohereReasoningState(),
			},
		},
	}
	out := mustCohereReq(t, req)

	blocks := cohereAssistantBlocks(t, out, 1)
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %d (%v), want 2 thinking blocks + 1 text", len(blocks), blocks)
	}
	for i, want := range []string{"step one: consider the user's intent", "step two: answer plainly"} {
		if blocks[i]["type"] != "thinking" || blocks[i]["thinking"] != want {
			t.Errorf("blocks[%d] = %v, want thinking %q", i, blocks[i], want)
		}
	}
	if blocks[2]["type"] != "text" || blocks[2]["text"] != "the answer is 42" {
		t.Errorf("visible content = %v, want the text block after the thinking blocks", blocks[2])
	}
}

func TestCohereReasoningEmissionToolCallTurn(t *testing.T) {
	// A tool-call turn with placeholder reasoning: the placeholder
	// must ride the thinking slot of the assistant message (the
	// documented form), never the visible text.
	state, ok := reasoning.ToolCallPlaceholder(reasoning.WireCohere, "command-a-reasoning-08-2025")
	if !ok {
		t.Fatalf("ToolCallPlaceholder refused the cohere reasoning model — the injection path is dead")
	}
	req := ChatCompletionRequest{
		Model: "command-a-reasoning-08-2025",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("search for it")},
			{
				Role:      "assistant",
				Content:   mustJSON(""),
				Reasoning: state,
				ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "search", Arguments: `{"q":"x"}`}}},
			},
		},
	}
	out := mustCohereReq(t, req)
	blocks := cohereAssistantBlocks(t, out, 1)
	if len(blocks) == 0 || blocks[0]["type"] != "thinking" {
		t.Fatalf("placeholder did not ride a thinking block: %v", blocks)
	}
	if got := blocks[0]["thinking"]; got != reasoning.PlaceholderText {
		t.Errorf("placeholder thinking = %v, want %q", got, reasoning.PlaceholderText)
	}
	if strings.Contains(string(out.Messages[1].Content), `"text":"[REASONING`) {
		t.Errorf("placeholder leaked into visible text: %s", out.Messages[1].Content)
	}
	if len(out.Messages[1].ToolCalls) != 1 {
		t.Errorf("tool call dropped: %+v", out.Messages[1])
	}
}

func TestCohereReasoningRefusedOnNonReasoningModel(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "command-r-plus",
		Messages: []ChatMessage{
			{Role: "assistant", Content: mustJSON("hi"), Reasoning: cohereReasoningState()},
		},
	}
	if _, err := ConvertCohereRequest(req); err == nil {
		t.Fatal("non-reasoning cohere model must refuse a restored reasoning state, not silently drop it")
	} else if !strings.Contains(err.Error(), "thinking content blocks") {
		t.Errorf("refusal = %v, want the thinking-blocks reason", err)
	}
}

func TestCohereNoReasoningNoThinkingBlocks(t *testing.T) {
	// Control: a message without reasoning is untouched (no thinking
	// block appears anywhere in the request).
	out := mustCohereReq(t, ChatCompletionRequest{
		Model: "command-a-reasoning-08-2025",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("hi")},
			{Role: "assistant", Content: mustJSON("hello")},
		},
	})
	for i, m := range out.Messages {
		if strings.Contains(string(m.Content), `"thinking"`) {
			t.Errorf("messages[%d] carries a thinking block without any reasoning state: %s", i, m.Content)
		}
	}
}

func TestCohereReasoningThinkingBlocksHelperShape(t *testing.T) {
	// Pins the emission form the converter consumes: the helper must
	// produce ONLY text-bearing blocks, in order, as unsigned
	// thinking blocks.
	s := cohereReasoningState()
	s.Blocks = append(s.Blocks, reasoning.Block{Kind: "signature", Data: json.RawMessage(`{"sig":"x"}`)})
	got := s.CohereThinkingBlocks()
	if len(got) != 2 {
		t.Fatalf("CohereThinkingBlocks = %d blocks, want 2 (signature must be skipped)", len(got))
	}
	for _, b := range got {
		if b.Type != "thinking" || strings.TrimSpace(b.Thinking) == "" {
			t.Errorf("bad block: %+v", b)
		}
	}
}
