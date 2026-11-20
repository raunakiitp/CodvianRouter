package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

func mustMsg(t *testing.T, s string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func anthropicThinkingState() *reasoning.State {
	return &reasoning.State{
		Blocks: []reasoning.Block{
			{Kind: reasoning.KindThinking, Text: "consider the weather", Signature: "sig-abc"},
			{Kind: reasoning.KindRedactedThinking, Data: json.RawMessage(`"ZW5jcnlwdGVk"`)},
		},
		SourceWire: reasoning.WireAnthropic, SourceModel: "claude-sonnet-4-5",
	}
}

// --- Anthropic request: thinking blocks replay verbatim, leading ---

func TestConvertRequestAnthropicThinking(t *testing.T) {
	req, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "what is the weather?")},
			{Role: "assistant", Content: mustMsg(t, "I will check."), Reasoning: anthropicThinkingState()},
			{Role: "user", Content: mustMsg(t, "and tomorrow?")},
		},
	})
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(req.Messages))
	}
	blocks := req.Messages[1].Content
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks = %d, want 3 (thinking + redacted + text)", len(blocks))
	}
	if blocks[0].Type != "thinking" || blocks[0].Thinking != "consider the weather" || blocks[0].Signature != "sig-abc" {
		t.Fatalf("block 0 = %+v, want thinking with text+signature", blocks[0])
	}
	if blocks[1].Type != "redacted_thinking" || blocks[1].Data != "ZW5jcnlwdGVk" {
		t.Fatalf("block 1 = %+v, want redacted_thinking with the encrypted payload", blocks[1])
	}
	if blocks[2].Type != "text" || blocks[2].Text != "I will check." {
		t.Fatalf("block 2 = %+v, want the visible text last", blocks[2])
	}
}

func TestConvertRequestAnthropicMissingSignature(t *testing.T) {
	_, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "unsigned thought"}},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected an error for a thinking block without a signature")
	}
	if !reasoning.IsUnrepresentable(err) {
		t.Fatalf("error is not *reasoning.UnrepresentableError: %T %v", err, err)
	}
}

func TestConvertRequestAnthropicThoughtTextUnrepresentable(t *testing.T) {
	_, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "plain reasoning"}},
			}},
		},
	})
	if err == nil || !reasoning.IsUnrepresentable(err) {
		t.Fatalf("expected *reasoning.UnrepresentableError, got %v", err)
	}
}

// --- Anthropic response: thinking captured, never visible ---

func TestConvertResponseAnthropicThinking(t *testing.T) {
	resp, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg_1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []AnthropicBlock{
			{Type: "thinking", Thinking: "let me think", Signature: "sig-1"},
			{Type: "text", Text: "Sunny."},
			{Type: "redacted_thinking", Data: "cmVkYWN0ZWQ="},
		},
		Usage: AnthropicUsage{InputTokens: 10, OutputTokens: 5},
	})
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	choice := resp.Choices[0]
	var text string
	if err := json.Unmarshal(choice.Message.Content, &text); err != nil {
		t.Fatalf("message content: %v", err)
	}
	if text != "Sunny." {
		t.Fatalf("visible content = %q, want only the text (thinking must never become content)", text)
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 2 {
		t.Fatalf("Reasoning = %+v, want 2 blocks", resp.Reasoning)
	}
	b0, b1 := resp.Reasoning.Blocks[0], resp.Reasoning.Blocks[1]
	if b0.Kind != reasoning.KindThinking || b0.Text != "let me think" || b0.Signature != "sig-1" {
		t.Fatalf("block 0 = %+v", b0)
	}
	if b1.Kind != reasoning.KindRedactedThinking || string(b1.Data) != `"cmVkYWN0ZWQ="` {
		t.Fatalf("block 1 = %+v", b1)
	}
	if resp.Reasoning.SourceWire != reasoning.WireAnthropic {
		t.Fatalf("SourceWire = %q", resp.Reasoning.SourceWire)
	}
}

// --- Anthropic stream: thinking_delta + signature_delta captured ---

func TestTranslateStreamAnthropicThinking(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_s","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"deep "}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"thought"}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-stream"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Sunny."}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":1}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	var emitted []ChatChunk
	res, err := TranslateStream(strings.NewReader(sse), func(c ChatChunk) error {
		emitted = append(emitted, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if res.Content != "Sunny." {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Reasoning == nil || len(res.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 thinking block", res.Reasoning)
	}
	b := res.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindThinking || b.Text != "deep thought" || b.Signature != "sig-stream" {
		t.Fatalf("thinking block = %+v", b)
	}
	// No emitted chunk may carry the thinking text as content.
	for _, c := range emitted {
		for _, ch := range c.Choices {
			if strings.Contains(ch.Delta.Content, "deep thought") {
				t.Fatalf("thinking text leaked into an emitted chunk: %+v", c)
			}
		}
	}
}

// --- Gemini request: thought parts + signature attach ---

func TestConvertGeminiRequestThinking(t *testing.T) {
	req, err := ConvertGeminiRequest(ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "what is the weather?")},
			{Role: "assistant", Content: mustMsg(t, "I will check."), Reasoning: anthropicThinkingState(), ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Paris"}`}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	if len(req.Contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(req.Contents))
	}
	model := req.Contents[1]
	if model.Role != "model" {
		t.Fatalf("role = %q", model.Role)
	}
	if len(model.Parts) != 3 {
		t.Fatalf("model parts = %d, want 3 (thought + text + functionCall)", len(model.Parts))
	}
	thought := model.Parts[0]
	if !thought.Thought || thought.Text != "consider the weather" {
		t.Fatalf("part 0 = %+v, want a thought part with the reasoning text", thought)
	}
	// The Anthropic signature has no Gemini equivalent on a thought
	// part: it is dropped (recorded by reasoning.Check) — the
	// thought part must NOT carry it.
	if thought.ThoughtSignature != "" {
		t.Fatalf("thought part carries a signature %q (no gemini equivalent)", thought.ThoughtSignature)
	}
	if model.Parts[1].Text != "I will check." || model.Parts[1].Thought {
		t.Fatalf("part 1 = %+v, want the visible text", model.Parts[1])
	}
	if model.Parts[2].FunctionCall == nil || model.Parts[2].FunctionCall.Name != "get_weather" {
		t.Fatalf("part 2 = %+v, want the function call", model.Parts[2])
	}
}

// A Gemini-issued thought signature DOES ride on the thought part
// (it is Gemini's own signature); only foreign signatures drop.
func TestConvertGeminiRequestGeminiSignaturePreserved(t *testing.T) {
	req, err := ConvertGeminiRequest(ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: &reasoning.State{
				Blocks:     []reasoning.Block{{Kind: reasoning.KindThought, Text: "gemini plan", Signature: "sig-gemini"}},
				SourceWire: reasoning.WireGemini, SourceModel: "gemini-2.5-pro",
			}},
		},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	parts := req.Contents[1].Parts
	if len(parts) != 2 || !parts[0].Thought || parts[0].Text != "gemini plan" {
		t.Fatalf("parts = %+v, want a leading thought part", parts)
	}
	if parts[0].ThoughtSignature != "sig-gemini" {
		t.Fatalf("thought part signature = %q, want sig-gemini", parts[0].ThoughtSignature)
	}
}

func TestConvertGeminiRequestSignatureAttachesToCall(t *testing.T) {
	req, err := ConvertGeminiRequest(ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "call the tool")},
			{Role: "assistant", Content: mustMsg(t, ""), Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThoughtSignature, Name: "get_weather", Signature: "sig-call"}},
			}, ToolCalls: []ToolCall{
				{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{}`}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	parts := req.Contents[1].Parts
	if len(parts) != 1 || parts[0].FunctionCall == nil {
		t.Fatalf("parts = %+v, want one functionCall part", parts)
	}
	if parts[0].ThoughtSignature != "sig-call" {
		t.Fatalf("functionCall signature = %q, want sig-call", parts[0].ThoughtSignature)
	}
}

// --- Gemini response: thought parts captured, never visible ---

func TestConvertGeminiResponseThinking(t *testing.T) {
	resp, err := ConvertGeminiResponse(&GeminiResponse{
		ModelVersion: "gemini-2.5-pro",
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Role: "model", Parts: []GeminiPart{
				{Text: "internal plan", Thought: true, ThoughtSignature: "sig-thought"},
				{Text: "Sunny."},
				{FunctionCall: &GeminiFunctionCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Paris"}`)}, ThoughtSignature: "sig-call"},
			}},
			FinishReason: "TOOL_CALLS",
		}},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiResponse: %v", err)
	}
	var text string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &text); err != nil {
		t.Fatalf("message content: %v", err)
	}
	if text != "Sunny." {
		t.Fatalf("visible content = %q, want only the text", text)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.Choices[0].Message.ToolCalls))
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 2 {
		t.Fatalf("Reasoning = %+v, want 2 blocks (thought + call signature)", resp.Reasoning)
	}
	b0, b1 := resp.Reasoning.Blocks[0], resp.Reasoning.Blocks[1]
	if b0.Kind != reasoning.KindThought || b0.Text != "internal plan" || b0.Signature != "sig-thought" {
		t.Fatalf("block 0 = %+v", b0)
	}
	if b1.Kind != reasoning.KindThoughtSignature || b1.Name != "get_weather" || b1.Signature != "sig-call" {
		t.Fatalf("block 1 = %+v", b1)
	}
}

// --- Gemini stream: thought parts captured across events ---

func TestTranslateGeminiStreamThinking(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"plan part 1 ","thought":true}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro"}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"plan part 2","thought":true,"thoughtSignature":"sig-g"}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro"}`,
		``,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Sunny."}]},"finishReason":"STOP"}],"modelVersion":"gemini-2.5-pro"}`,
		``,
	}, "\n")
	var emitted []ChatChunk
	res, err := TranslateGeminiStream(strings.NewReader(sse), func(c ChatChunk) error {
		emitted = append(emitted, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateGeminiStream: %v", err)
	}
	if res.Content != "Sunny." {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Reasoning == nil || len(res.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 thought block", res.Reasoning)
	}
	b := res.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindThought || b.Text != "plan part 1 plan part 2" || b.Signature != "sig-g" {
		t.Fatalf("thought block = %+v", b)
	}
	for _, c := range emitted {
		for _, ch := range c.Choices {
			if strings.Contains(ch.Delta.Content, "plan part") {
				t.Fatalf("thought text leaked into an emitted chunk: %+v", c)
			}
		}
	}
}

// --- Bedrock request: reasoningContent replays verbatim (item-52 fix) ---

func bedrockState() *reasoning.State {
	return &reasoning.State{
		Blocks:     []reasoning.Block{{Kind: reasoning.KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"bedrock thought","signature":"br-sig"}}}`)}},
		SourceWire: reasoning.WireBedrock,
	}
}

func TestConvertBedrockRequestReasoning(t *testing.T) {
	req, err := ConvertBedrockRequest(ChatCompletionRequest{
		Model: "anthropic.claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "what is the weather?")},
			{Role: "assistant", Content: mustMsg(t, "I will check."), Reasoning: bedrockState()},
		},
	})
	if err != nil {
		t.Fatalf("ConvertBedrockRequest: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	blocks := req.Messages[1].Content
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %d, want 2 (reasoningContent + text)", len(blocks))
	}
	if blocks[0].ReasoningContent == nil {
		t.Fatalf("block 0 = %+v, want a reasoningContent block leading", blocks[0])
	}
	var decoded struct {
		ReasoningContent struct {
			ReasoningText struct {
				Text      string `json:"text"`
				Signature string `json:"signature"`
			} `json:"reasoningText"`
		} `json:"reasoningContent"`
	}
	if err := json.Unmarshal(blocks[0].ReasoningContent, &decoded); err != nil {
		t.Fatalf("reasoningContent is not valid JSON: %v", err)
	}
	if decoded.ReasoningContent.ReasoningText.Text != "bedrock thought" || decoded.ReasoningContent.ReasoningText.Signature != "br-sig" {
		t.Fatalf("reasoningContent = %+v, want the verbatim block", decoded)
	}
	if blocks[1].Text == nil || *blocks[1].Text != "I will check." {
		t.Fatalf("block 1 = %+v, want the visible text", blocks[1])
	}
}

func TestConvertBedrockRequestThinkingUnrepresentable(t *testing.T) {
	_, err := ConvertBedrockRequest(ChatCompletionRequest{
		Model: "anthropic.claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: anthropicThinkingState()},
		},
	})
	if err == nil || !reasoning.IsUnrepresentable(err) {
		t.Fatalf("expected *reasoning.UnrepresentableError, got %v", err)
	}
}

// --- Bedrock response: reasoningContent captured verbatim ---

func TestConvertBedrockResponseReasoning(t *testing.T) {
	resp, err := ConvertBedrockResponse(&BedrockResponse{
		StopReason: "end_turn",
		Usage:      BedrockUsage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	if err != nil {
		t.Fatalf("ConvertBedrockResponse (empty): %v", err)
	}
	_ = resp
	// A response carrying reasoningContent + text.
	br := &BedrockResponse{StopReason: "end_turn"}
	br.Output.Message = BedrockMessage{Role: "assistant", Content: []BedrockContentBlock{
		{ReasoningContent: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"br thought","signature":"br-sig-2"}}}`)},
		{Text: func() *string { s := "Sunny."; return &s }()},
	}}
	out, err := ConvertBedrockResponse(br)
	if err != nil {
		t.Fatalf("ConvertBedrockResponse: %v", err)
	}
	var text string
	if err := json.Unmarshal(out.Choices[0].Message.Content, &text); err != nil {
		t.Fatalf("message content: %v", err)
	}
	if text != "Sunny." {
		t.Fatalf("visible content = %q, want only the text", text)
	}
	if out.Reasoning == nil || len(out.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 bedrock block", out.Reasoning)
	}
	b := out.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindBedrock {
		t.Fatalf("block kind = %q", b.Kind)
	}
	if gotText, gotSig, _ := reasoning.ParseBedrockReasoning(b.Data); gotText != "br thought" || gotSig != "br-sig-2" {
		t.Fatalf("parsed bedrock block = %q / %q", gotText, gotSig)
	}
}

// --- Bedrock stream: reasoningContent deltas captured ---

func TestTranslateBedrockStreamReasoning(t *testing.T) {
	frames := [][]byte{
		buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}),
		buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"reasoningContent": map[string]any{"text": "br "}}}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"reasoningContent": map[string]any{"text": "thought", "signature": "br-stream-sig"}}}),
		buildEventStreamFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}),
		buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 1}),
		buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 1, "delta": map[string]any{"text": "Sunny."}}),
		buildEventStreamFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 1}),
		buildEventStreamFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}),
		buildEventStreamFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}),
	}
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	var emitted []ChatChunk
	res, err := TranslateBedrockStream(strings.NewReader(string(body)), func(c ChatChunk) error {
		emitted = append(emitted, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateBedrockStream: %v", err)
	}
	if res.Content != "Sunny." {
		t.Fatalf("content = %q", res.Content)
	}
	if res.Reasoning == nil || len(res.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 bedrock block", res.Reasoning)
	}
	b := res.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindBedrock {
		t.Fatalf("block kind = %q", b.Kind)
	}
	if gotText, gotSig, _ := reasoning.ParseBedrockReasoning(b.Data); gotText != "br thought" || gotSig != "br-stream-sig" {
		t.Fatalf("parsed bedrock block = %q / %q", gotText, gotSig)
	}
	for _, c := range emitted {
		for _, ch := range c.Choices {
			if strings.Contains(ch.Delta.Content, "br thought") {
				t.Fatalf("reasoning text leaked into an emitted chunk: %+v", c)
			}
		}
	}
}

// --- Cohere: any reasoning state fails the hop honestly ---

func TestConvertCohereRequestReasoningUnrepresentable(t *testing.T) {
	_, err := ConvertCohereRequest(ChatCompletionRequest{
		Model: "command-r",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: anthropicThinkingState()},
		},
	})
	if err == nil || !reasoning.IsUnrepresentable(err) {
		t.Fatalf("expected *reasoning.UnrepresentableError, got %v", err)
	}
}

// --- Cross-wire translation: Anthropic thinking -> Gemini thought ---

func TestAnthropicToGeminiTranslation(t *testing.T) {
	req, err := ConvertGeminiRequest(ChatCompletionRequest{
		Model: "gemini-2.5-pro",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "planned it", Signature: "sig-x"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	parts := req.Contents[1].Parts
	if len(parts) != 2 || !parts[0].Thought || parts[0].Text != "planned it" {
		t.Fatalf("parts = %+v, want a leading thought part", parts)
	}
	// And back: Gemini thought -> Anthropic is unrepresentable
	// (no Anthropic signature), so the hop fails honestly.
	_, err = ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustMsg(t, "hi")},
			{Role: "assistant", Content: mustMsg(t, "hello"), Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "planned it"}},
			}},
		},
	})
	if err == nil || !reasoning.IsUnrepresentable(err) {
		t.Fatalf("expected *reasoning.UnrepresentableError, got %v", err)
	}
}
