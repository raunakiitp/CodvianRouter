package wire

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func fptr(v float64) *float64 { return &v }
func sptr(v string) *string   { return &v }

func strMsg(role, content string) ChatMessage {
	return ChatMessage{Role: role, Content: json.RawMessage(mustJSON(content))}
}

// ---------------------------------------------------------------------------
// Request conversion.
// ---------------------------------------------------------------------------

func TestConvertRequestBasicText(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ChatMessage{strMsg("user", "Hello")},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if got.Model != "claude-sonnet-4-5" {
		t.Errorf("model = %q", got.Model)
	}
	if got.MaxTokens != DefaultMaxTokens {
		t.Errorf("max_tokens = %d, want default %d", got.MaxTokens, DefaultMaxTokens)
	}
	if got.Stream {
		t.Errorf("stream = true, want false")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("messages = %+v", got.Messages)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "Hello" {
		t.Errorf("content blocks = %+v", blocks)
	}
	if got.System != "" {
		t.Errorf("system = %q, want empty", got.System)
	}
}

func TestConvertRequestMaxTokensExplicit(t *testing.T) {
	req := ChatCompletionRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: 1024,
		Messages:  []ChatMessage{strMsg("user", "Hi")},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", got.MaxTokens)
	}
}

func TestConvertRequestSystemPrompt(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			strMsg("system", "You are helpful."),
			strMsg("system", "Be concise."),
			strMsg("user", "Hi"),
		},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if want := "You are helpful.\n\nBe concise."; got.System != want {
		t.Errorf("system = %q, want %q", got.System, want)
	}
	if len(got.Messages) != 1 {
		t.Errorf("messages = %+v, want only the user message", got.Messages)
	}
}

func TestConvertRequestDeveloperBecomesSystem(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			strMsg("developer", "Follow the style guide."),
			strMsg("user", "Hi"),
		},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if got.System != "Follow the style guide." {
		t.Errorf("system = %q", got.System)
	}
}

func TestConvertRequestMultiTurn(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			strMsg("user", "What is 2+2?"),
			strMsg("assistant", "4"),
			strMsg("user", "And 3+3?"),
		},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(got.Messages))
	}
	roles := []string{"user", "assistant", "user"}
	for i, want := range roles {
		if got.Messages[i].Role != want {
			t.Errorf("messages[%d].role = %q, want %q", i, got.Messages[i].Role, want)
		}
	}
}

func TestConvertRequestPassthroughFields(t *testing.T) {
	req := ChatCompletionRequest{
		Model:       "claude-sonnet-4-5",
		Messages:    []ChatMessage{strMsg("user", "Hi")},
		Stream:      true,
		Temperature: fptr(0.7),
		TopP:        fptr(0.9),
		Stop:        []string{"END", "STOP"},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if !got.Stream {
		t.Errorf("stream = false, want true")
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("temperature = %v", got.Temperature)
	}
	if got.TopP == nil || *got.TopP != 0.9 {
		t.Errorf("top_p = %v", got.TopP)
	}
	if !reflect.DeepEqual(got.StopSequences, []string{"END", "STOP"}) {
		t.Errorf("stop_sequences = %v", got.StopSequences)
	}
}

func TestConvertRequestTools(t *testing.T) {
	tool := ToolDefinition{Type: "function"}
	tool.Function.Name = "get_weather"
	tool.Function.Description = "Get the weather"
	tool.Function.Parameters = json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
	req := ChatCompletionRequest{
		Model:      "claude-sonnet-4-5",
		Messages:   []ChatMessage{strMsg("user", "Weather in Pune?")},
		Tools:      []ToolDefinition{tool},
		ToolChoice: "auto",
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tools = %+v", got.Tools)
	}
	if got.Tools[0].Name != "get_weather" || got.Tools[0].Description != "Get the weather" {
		t.Errorf("tool = %+v", got.Tools[0])
	}
	if string(got.Tools[0].InputSchema) != `{"type":"object","properties":{"city":{"type":"string"}}}` {
		t.Errorf("input_schema = %s", got.Tools[0].InputSchema)
	}
	if got.ToolChoice == nil || got.ToolChoice.Type != "auto" {
		t.Errorf("tool_choice = %+v, want auto", got.ToolChoice)
	}
}

func TestConvertRequestToolChoiceMapping(t *testing.T) {
	tool := ToolDefinition{Type: "function"}
	tool.Function.Name = "get_weather"
	tool.Function.Parameters = json.RawMessage(`{"type":"object"}`)

	cases := []struct {
		name       string
		toolChoice any
		tools      []ToolDefinition
		wantType   string // "" = nil tool_choice
		wantName   string
		wantDrop   bool // tools dropped
		wantErr    bool
	}{
		{"auto string", "auto", []ToolDefinition{tool}, "auto", "", false, false},
		{"required string", "required", []ToolDefinition{tool}, "any", "", false, false},
		{"required no tools", "required", nil, "", "", false, true},
		{"none drops tools", "none", []ToolDefinition{tool}, "", "", true, false},
		{"function object", map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}, []ToolDefinition{tool}, "tool", "get_weather", false, false},
		{"tool object", map[string]any{"type": "tool", "name": "get_weather"}, []ToolDefinition{tool}, "tool", "get_weather", false, false},
		{"tool object no tools", map[string]any{"type": "tool", "name": "get_weather"}, nil, "", "", false, true},
		{"unknown string", "sometimes", []ToolDefinition{tool}, "", "", false, true},
		{"nil", nil, []ToolDefinition{tool}, "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := ChatCompletionRequest{
				Model:      "claude-sonnet-4-5",
				Messages:   []ChatMessage{strMsg("user", "Hi")},
				Tools:      tc.tools,
				ToolChoice: tc.toolChoice,
			}
			got, err := ConvertRequest(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				var ue *UnrepresentableError
				if !errors.As(err, &ue) {
					t.Fatalf("error = %T, want *UnrepresentableError", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ConvertRequest: %v", err)
			}
			if tc.wantDrop && len(got.Tools) != 0 {
				t.Errorf("tools = %+v, want dropped", got.Tools)
			}
			if tc.wantType == "" {
				if got.ToolChoice != nil {
					t.Errorf("tool_choice = %+v, want nil", got.ToolChoice)
				}
			} else {
				if got.ToolChoice == nil || got.ToolChoice.Type != tc.wantType || got.ToolChoice.Name != tc.wantName {
					t.Errorf("tool_choice = %+v, want type %q name %q", got.ToolChoice, tc.wantType, tc.wantName)
				}
			}
		})
	}
}

func TestConvertRequestToolCallsInHistory(t *testing.T) {
	assistant := ChatMessage{
		Role:    "assistant",
		Content: json.RawMessage(`"Let me check."`),
		ToolCalls: []ToolCall{{
			ID:   "toolu_1",
			Type: "function",
			Function: ToolCallFunction{
				Name:      "get_weather",
				Arguments: `{"city":"Pune"}`,
			},
		}},
	}
	toolResult := ChatMessage{
		Role:       "tool",
		Content:    json.RawMessage(mustJSON("sunny, 28C")),
		ToolCallID: "toolu_1",
	}
	req := ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			strMsg("user", "Weather in Pune?"),
			assistant,
			toolResult,
			strMsg("user", "Thanks"),
		},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	// assistant + tool result + follow-up user message merge into
	// alternating roles: user, assistant, user.
	if len(got.Messages) != 3 {
		t.Fatalf("messages = %+v, want 3", got.Messages)
	}
	asst := got.Messages[1]
	if len(asst.Content) != 2 {
		t.Fatalf("assistant blocks = %+v, want text + tool_use", asst.Content)
	}
	if asst.Content[0].Type != "text" || asst.Content[0].Text != "Let me check." {
		t.Errorf("assistant block 0 = %+v", asst.Content[0])
	}
	tu := asst.Content[1]
	if tu.Type != "tool_use" || tu.ID != "toolu_1" || tu.Name != "get_weather" || string(tu.Input) != `{"city":"Pune"}` {
		t.Errorf("tool_use block = %+v", tu)
	}
	// The tool result and the follow-up user message merge into one
	// user message (Anthropic forbids consecutive same-role messages).
	user2 := got.Messages[2]
	if len(user2.Content) != 2 {
		t.Fatalf("merged user blocks = %+v, want tool_result + text", user2.Content)
	}
	tr := user2.Content[0]
	if tr.Type != "tool_result" || tr.ToolUseID != "toolu_1" || tr.Content != "sunny, 28C" {
		t.Errorf("tool_result block = %+v", tr)
	}
	if user2.Content[1].Type != "text" || user2.Content[1].Text != "Thanks" {
		t.Errorf("merged text block = %+v", user2.Content[1])
	}
}

func TestConvertRequestImages(t *testing.T) {
	// data: URL -> base64 source.
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	imgMsg := ChatMessage{Role: "user"}
	parts, _ := json.Marshal([]ContentPart{
		{Type: "text", Text: "What is this?"},
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64," + pngB64}},
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/cat.jpg"}},
	})
	imgMsg.Content = parts
	req := ChatCompletionRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []ChatMessage{imgMsg},
	}
	got, err := ConvertRequest(req)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	blocks := got.Messages[0].Content
	if len(blocks) != 3 {
		t.Fatalf("blocks = %+v, want 3", blocks)
	}
	if blocks[1].Type != "image" || blocks[1].Source.Type != "base64" || blocks[1].Source.MediaType != "image/png" || blocks[1].Source.Data != pngB64 {
		t.Errorf("base64 image block = %+v", blocks[1])
	}
	if blocks[2].Type != "image" || blocks[2].Source.Type != "url" || blocks[2].Source.URL != "https://example.com/cat.jpg" {
		t.Errorf("url image block = %+v", blocks[2])
	}
}

func TestConvertRequestUnrepresentable(t *testing.T) {
	badImg := func(url string) ChatMessage {
		m := ChatMessage{Role: "user"}
		parts, _ := json.Marshal([]ContentPart{{Type: "image_url", ImageURL: &ImageURLPart{URL: url}}})
		m.Content = parts
		return m
	}
	audioMsg := ChatMessage{Role: "user"}
	audioParts, _ := json.Marshal([]ContentPart{{Type: "input_audio", Text: "x"}})
	audioMsg.Content = audioParts

	cases := []struct {
		name string
		req  ChatCompletionRequest
	}{
		{"empty model", ChatCompletionRequest{Messages: []ChatMessage{strMsg("user", "Hi")}}},
		{"no messages", ChatCompletionRequest{Model: "m"}},
		{"first message assistant", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{strMsg("assistant", "Hi")}}},
		{"unknown role", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{strMsg("narrator", "Hi")}}},
		{"temperature above 1", ChatCompletionRequest{Model: "m", Temperature: fptr(1.5), Messages: []ChatMessage{strMsg("user", "Hi")}}},
		{"temperature below 0", ChatCompletionRequest{Model: "m", Temperature: fptr(-0.1), Messages: []ChatMessage{strMsg("user", "Hi")}}},
		{"top_p above 1", ChatCompletionRequest{Model: "m", TopP: fptr(1.2), Messages: []ChatMessage{strMsg("user", "Hi")}}},
		{"tool result without id", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{strMsg("user", "Hi"), {Role: "tool", Content: json.RawMessage(mustJSON("ok"))}}}},
		{"tool call bad JSON args", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{strMsg("user", "Hi"), {
			Role:      "assistant",
			ToolCalls: []ToolCall{{ID: "t1", Type: "function", Function: ToolCallFunction{Name: "f", Arguments: "{not json"}}},
		}}}},
		{"audio part", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{audioMsg}}},
		{"ftp image", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{badImg("ftp://example.com/a.png")}}},
		{"unsupported image media type", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{badImg("data:image/bmp;base64,AAAA")}}},
		{"bad base64 image", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{badImg("data:image/png;base64,!!!not-base64!!!")}}},
		{"non-function tool type", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{strMsg("user", "Hi")}, Tools: []ToolDefinition{{Type: "custom"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ConvertRequest(tc.req)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var ue *UnrepresentableError
			if !errors.As(err, &ue) {
				t.Fatalf("error = %T (%v), want *UnrepresentableError", err, err)
			}
		})
	}
}

func TestConvertRequestStopStringForm(t *testing.T) {
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"Hi"}],"stop":"END"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(req.Stop, []string{"END"}) {
		t.Errorf("stop = %v, want [END]", req.Stop)
	}
}

// ---------------------------------------------------------------------------
// Response conversion.
// ---------------------------------------------------------------------------

func TestConvertResponseText(t *testing.T) {
	resp := &AnthropicResponse{
		ID:         "msg_1",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-5",
		StopReason: sptr("end_turn"),
		Content:    []AnthropicBlock{{Type: "text", Text: "Hello there"}},
		Usage:      AnthropicUsage{InputTokens: 10, OutputTokens: 4},
	}
	got, err := ConvertResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if got.ID != "msg_1" || got.Object != "chat.completion" || got.Model != "claude-sonnet-4-5" {
		t.Errorf("envelope = %+v", got)
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %+v", got.Choices)
	}
	var content string
	if err := json.Unmarshal(got.Choices[0].Message.Content, &content); err != nil || content != "Hello there" {
		t.Errorf("content = %s (err %v)", got.Choices[0].Message.Content, err)
	}
	if got.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", got.Choices[0].FinishReason)
	}
	if got.Usage != (Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}) {
		t.Errorf("usage = %+v", got.Usage)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":                      "stop",
		"stop_sequence":                 "stop",
		"pause_turn":                    "stop",
		"refusal":                       "stop",
		"":                              "stop",
		"max_tokens":                    "length",
		"model_context_window_exceeded": "length",
		"tool_use":                      "tool_calls",
		"something_new":                 "stop",
	}
	for in, want := range cases {
		if got := MapFinishReason(in); got != want {
			t.Errorf("MapFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertResponseToolUse(t *testing.T) {
	resp := &AnthropicResponse{
		ID:         "msg_2",
		Type:       "message",
		Role:       "assistant",
		Model:      "claude-sonnet-4-5",
		StopReason: sptr("tool_use"),
		Content: []AnthropicBlock{
			{Type: "text", Text: "Checking the weather."},
			{Type: "tool_use", ID: "toolu_9", Name: "get_weather", Input: json.RawMessage(`{"city":"Pune"}`)},
		},
		Usage: AnthropicUsage{InputTokens: 42, OutputTokens: 30},
	}
	got, err := ConvertResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	tcs := got.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %+v", tcs)
	}
	if tcs[0].ID != "toolu_9" || tcs[0].Type != "function" || tcs[0].Function.Name != "get_weather" || tcs[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("tool_call = %+v", tcs[0])
	}
}

func TestConvertResponseThinkingDropped(t *testing.T) {
	resp := &AnthropicResponse{
		ID: "msg_3", Type: "message", Role: "assistant", Model: "m",
		StopReason: sptr("end_turn"),
		Content: []AnthropicBlock{
			{Type: "thinking", Text: "let me think"},
			{Type: "redacted_thinking"},
			{Type: "text", Text: "Answer"},
		},
	}
	got, err := ConvertResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	var content string
	_ = json.Unmarshal(got.Choices[0].Message.Content, &content)
	if content != "Answer" {
		t.Errorf("content = %q, want Answer", content)
	}
}

func TestConvertResponseUnrepresentable(t *testing.T) {
	resp := &AnthropicResponse{
		ID: "msg_4", Type: "message", Role: "assistant", Model: "m",
		StopReason: sptr("end_turn"),
		Content: []AnthropicBlock{
			{Type: "text", Text: "ok"},
			{Type: "server_tool_use", Name: "web_search"},
		},
	}
	_, err := ConvertResponse(resp)
	if err == nil {
		t.Fatalf("expected error for server_tool_use block")
	}
	var ue *UnrepresentableError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %T, want *UnrepresentableError", err)
	}
}

// ---------------------------------------------------------------------------
// Streaming.
// ---------------------------------------------------------------------------

func collectChunks(t *testing.T, sse string) (chunks []ChatChunk, res StreamResult, err error) {
	t.Helper()
	res, err = TranslateStream(strings.NewReader(sse), func(c ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	return chunks, res, err
}

const textStreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_stream1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: ping
data: {"type":"ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}
`

func TestTranslateStreamText(t *testing.T) {
	chunks, res, err := collectChunks(t, textStreamSSE)
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if res.Content != "Hello world" {
		t.Errorf("content = %q", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish_reason = %q", res.FinishReason)
	}
	if res.Usage != (Usage{PromptTokens: 12, CompletionTokens: 5, TotalTokens: 17}) {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.ID != "msg_stream1" || res.Model != "claude-sonnet-4-5" {
		t.Errorf("result id/model = %q/%q", res.ID, res.Model)
	}
	// Chunks: role-only open, two text deltas, final chunk.
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d, want 4: %+v", len(chunks), chunks)
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk delta = %+v, want role assistant", chunks[0].Choices[0].Delta)
	}
	if chunks[1].Choices[0].Delta.Content != "Hello " {
		t.Errorf("chunk 1 content = %q", chunks[1].Choices[0].Delta.Content)
	}
	if chunks[2].Choices[0].Delta.Content != "world" {
		t.Errorf("chunk 2 content = %q", chunks[2].Choices[0].Delta.Content)
	}
	last := chunks[3]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("final chunk finish_reason = %v", last.Choices[0].FinishReason)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 17 {
		t.Errorf("final chunk usage = %+v", last.Usage)
	}
	for _, c := range chunks {
		if c.Object != "chat.completion.chunk" {
			t.Errorf("chunk object = %q", c.Object)
		}
	}
}

const toolStreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_stream2","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":50,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll check."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_7","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" \"Pune\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":25}}

event: message_stop
data: {"type":"message_stop"}
`

func TestTranslateStreamToolUse(t *testing.T) {
	chunks, res, err := collectChunks(t, toolStreamSSE)
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if res.Content != "I'll check." {
		t.Errorf("content = %q", res.Content)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", res.FinishReason)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v", res.ToolCalls)
	}
	tc := res.ToolCalls[0]
	if tc.ID != "toolu_7" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city": "Pune"}` {
		t.Errorf("tool_call = %+v", tc)
	}
	// The tool_use open chunk carries id + name; the two argument
	// fragments arrive as separate deltas.
	var opens, argDeltas int
	for _, c := range chunks {
		for _, td := range c.Choices[0].Delta.ToolCalls {
			if td.ID == "toolu_7" && td.Function.Name == "get_weather" {
				opens++
			}
			if td.Function.Arguments != "" {
				argDeltas++
			}
		}
	}
	if opens != 1 {
		t.Errorf("tool open deltas = %d, want 1", opens)
	}
	if argDeltas != 2 {
		t.Errorf("argument deltas = %d, want 2", argDeltas)
	}
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("final finish_reason = %v", last.Choices[0].FinishReason)
	}
}

func TestTranslateStreamErrorEvent(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_e1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Partial"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}

`
	_, _, err := collectChunks(t, sse)
	if err == nil {
		t.Fatalf("expected error from error event")
	}
	var ae *AnthropicError
	if !errors.As(err, &ae) {
		t.Fatalf("error = %T, want *AnthropicError", err)
	}
	if !ae.IsOverloaded() || !ae.Retryable {
		t.Errorf("error = %+v, want overloaded + retryable", ae)
	}
}

func TestTranslateStreamTruncated(t *testing.T) {
	cases := map[string]string{
		"no message_stop": `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}
`,
		"no message_delta": `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: message_stop
data: {"type":"message_stop"}
`,
		"empty stream": ``,
	}
	for name, sse := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := collectChunks(t, sse)
			if err == nil {
				t.Fatalf("expected truncated-stream error")
			}
		})
	}
}

func TestTranslateStreamBadToolJSON(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{oops"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}

event: message_stop
data: {"type":"message_stop"}
`
	_, _, err := collectChunks(t, sse)
	if err == nil {
		t.Fatalf("expected error for invalid tool JSON")
	}
}

func TestTranslateStreamUnknownBlockType(t *testing.T) {
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","name":"web_search"}}

event: message_stop
data: {"type":"message_stop"}
`
	_, _, err := collectChunks(t, sse)
	if err == nil {
		t.Fatalf("expected error for server_tool_use block")
	}
	var ue *UnrepresentableError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %T, want *UnrepresentableError", err)
	}
}

// ---------------------------------------------------------------------------
// Error mapping.
// ---------------------------------------------------------------------------

func TestMapError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		headers   http.Header
		body      string
		wantType  string
		wantRetry bool
		wantAfter string // Retry-After header value
	}{
		{"400 invalid", 400, nil, `{"type":"error","error":{"type":"invalid_request_error","message":"bad max_tokens"}}`, "invalid_request_error", false, ""},
		{"401 auth", 401, nil, `{"type":"error","error":{"type":"authentication_error","message":"invalid key"}}`, "authentication_error", false, ""},
		{"403 billing", 403, nil, `{"type":"error","error":{"type":"billing_error","message":"no credits"}}`, "billing_error", false, ""},
		{"429 rate limit", 429, http.Header{"Retry-After": []string{"30"}}, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, "rate_limit_error", true, "30"},
		{"500 api", 500, nil, `{"type":"error","error":{"type":"api_error","message":"boom"}}`, "api_error", true, ""},
		{"529 overloaded", 529, nil, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, "overloaded_error", true, ""},
		{"503 overloaded type", 503, nil, `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, "overloaded_error", true, ""},
		{"non-JSON body", 500, nil, `upstream exploded`, "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := MapError(tc.status, tc.headers, []byte(tc.body))
			var ae *AnthropicError
			if !errors.As(err, &ae) {
				t.Fatalf("error = %T, want *AnthropicError", err)
			}
			if ae.StatusCode != tc.status {
				t.Errorf("status = %d, want %d", ae.StatusCode, tc.status)
			}
			if ae.Type != tc.wantType {
				t.Errorf("type = %q, want %q", ae.Type, tc.wantType)
			}
			if ae.Retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", ae.Retryable, tc.wantRetry)
			}
			if tc.wantAfter != "" && ae.RetryAfter.String() != tc.wantAfter+"s" {
				t.Errorf("retry-after = %v, want %ss", ae.RetryAfter, tc.wantAfter)
			}
			if !IsRetryable(err) != !tc.wantRetry {
				t.Errorf("IsRetryable = %v, want %v", IsRetryable(err), tc.wantRetry)
			}
		})
	}
}

func TestMapErrorRateLimitHelpers(t *testing.T) {
	err := MapError(429, nil, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"x"}}`))
	var ae *AnthropicError
	if !errors.As(err, &ae) || !ae.IsRateLimit() {
		t.Errorf("expected IsRateLimit, got %v", err)
	}
	if HTTPStatus(err) != 429 {
		t.Errorf("HTTPStatus = %d, want 429", HTTPStatus(err))
	}
	over := MapError(529, nil, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"x"}}`))
	var oe *AnthropicError
	if !errors.As(over, &oe) || !oe.IsOverloaded() {
		t.Errorf("expected IsOverloaded, got %v", over)
	}
}

func TestMapStreamError(t *testing.T) {
	cases := []struct {
		body      string
		wantCode  int
		wantRetry bool
	}{
		{`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, 529, true},
		{`{"type":"error","error":{"type":"rate_limit_error","message":"slow"}}`, 429, true},
		{`{"type":"error","error":{"type":"api_error","message":"boom"}}`, 0, true},
		{`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, 0, false},
	}
	for _, tc := range cases {
		err := MapStreamError([]byte(tc.body))
		var ae *AnthropicError
		if !errors.As(err, &ae) {
			t.Fatalf("error = %T, want *AnthropicError", err)
		}
		if ae.StatusCode != tc.wantCode {
			t.Errorf("status = %d, want %d (%s)", ae.StatusCode, tc.wantCode, tc.body)
		}
		if ae.Retryable != tc.wantRetry {
			t.Errorf("retryable = %v, want %v (%s)", ae.Retryable, tc.wantRetry, tc.body)
		}
	}
}

func TestTranslateStreamToolCallIndexRenumbered(t *testing.T) {
	// The tool_use block is Anthropic content-block index 1 (after
	// the text block), but the OpenAI tool-call index must be 0:
	// block indices count text blocks too and are not valid
	// OpenAI tool-call indices.
	chunks, _, err := collectChunks(t, toolStreamSSE)
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	for _, c := range chunks {
		for _, td := range c.Choices[0].Delta.ToolCalls {
			if td.Index != 0 {
				t.Errorf("tool-call delta index = %d, want 0 (renumbered from block 1)", td.Index)
			}
		}
	}
}
