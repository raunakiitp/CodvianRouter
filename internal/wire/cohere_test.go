package wire

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func mustCohereReq(t *testing.T, req ChatCompletionRequest) *CohereRequest {
	t.Helper()
	out, err := ConvertCohereRequest(req)
	if err != nil {
		t.Fatalf("ConvertCohereRequest: %v", err)
	}
	return out
}

func TestConvertCohereRequestBasic(t *testing.T) {
	temp := 0.7
	topP := 0.9
	out := mustCohereReq(t, ChatCompletionRequest{
		Model:       "command-a-03-2025",
		Messages:    []ChatMessage{{Role: "system", Content: mustJSON("be brief")}, {Role: "user", Content: mustJSON("hi")}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   100,
		Stop:        []string{"END"},
	})
	if out.Model != "command-a-03-2025" {
		t.Errorf("Model = %q", out.Model)
	}
	if len(out.Messages) != 2 || out.Messages[0].Role != "system" || out.Messages[1].Role != "user" {
		t.Errorf("Messages = %+v", out.Messages)
	}
	if *out.Temperature != 0.7 || *out.P != 0.9 {
		t.Errorf("Temperature/P = %v/%v", *out.Temperature, *out.P)
	}
	if out.MaxTokens != 100 || len(out.StopSequences) != 1 || out.StopSequences[0] != "END" {
		t.Errorf("MaxTokens/StopSequences = %d/%v", out.MaxTokens, out.StopSequences)
	}
	// System content is a plain string on the Cohere side.
	var sys string
	if err := json.Unmarshal(out.Messages[0].Content, &sys); err != nil || sys != "be brief" {
		t.Errorf("system content = %s (err %v)", out.Messages[0].Content, err)
	}
}

func TestConvertCohereRequestTools(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "command-a-03-2025",
		Messages: []ChatMessage{{Role: "user", Content: mustJSON("weather?")}},
		Tools: []ToolDefinition{{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		}},
		ToolChoice: "required",
	}
	out := mustCohereReq(t, req)
	if len(out.Tools) != 1 || out.Tools[0].Type != "function" || out.Tools[0].Function.Name != "get_weather" {
		t.Errorf("Tools = %+v", out.Tools)
	}
	if out.ToolChoice != "REQUIRED" {
		t.Errorf("ToolChoice = %v, want REQUIRED", out.ToolChoice)
	}

	// "auto" is omitted (free choice is the default); "none" is NONE.
	out = mustCohereReq(t, ChatCompletionRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}},
		Tools:      req.Tools,
		ToolChoice: "auto",
	})
	if out.ToolChoice != nil {
		t.Errorf("ToolChoice auto = %v, want omitted (nil)", out.ToolChoice)
	}
	out = mustCohereReq(t, ChatCompletionRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}},
		Tools:      req.Tools,
		ToolChoice: "none",
	})
	if out.ToolChoice != "NONE" {
		t.Errorf("ToolChoice none = %v, want NONE", out.ToolChoice)
	}
	// Named function passes through in Cohere's function form.
	out = mustCohereReq(t, ChatCompletionRequest{
		Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}},
		Tools:      req.Tools,
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}},
	})
	m, ok := out.ToolChoice.(map[string]any)
	if !ok || m["type"] != "function" {
		t.Errorf("ToolChoice named = %v, want function form", out.ToolChoice)
	}
}

func TestConvertCohereRequestToolMessages(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "command-a-03-2025",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("weather in Pune?")},
			{Role: "assistant", Content: mustJSON(""), ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}}}},
			{Role: "tool", Content: mustJSON("24°C"), ToolCallID: "call_1"},
		},
	}
	out := mustCohereReq(t, req)
	if len(out.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3", len(out.Messages))
	}
	if len(out.Messages[1].ToolCalls) != 1 || out.Messages[1].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("assistant ToolCalls = %+v", out.Messages[1].ToolCalls)
	}
	if out.Messages[2].Role != "tool" || out.Messages[2].ToolCallID != "call_1" {
		t.Errorf("tool message = %+v", out.Messages[2])
	}
}

func TestConvertCohereRequestImages(t *testing.T) {
	// image_url blocks pass through unchanged (Cohere v2 accepts the
	// same shape as OpenAI, data: and http(s) URLs alike).
	req := ChatCompletionRequest{
		Model: "command-a-vision-07-2025",
		Messages: []ChatMessage{{Role: "user", Content: mustJSONValue([]ContentPart{
			{Type: "text", Text: "what is this?"},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,iVBORw0KGgo="}},
			{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/cat.png"}},
		})}},
	}
	out := mustCohereReq(t, req)
	var blocks []CohereContentBlock
	if err := json.Unmarshal(out.Messages[0].Content, &blocks); err != nil {
		t.Fatalf("content decode: %v", err)
	}
	if len(blocks) != 3 || blocks[1].Type != "image_url" || blocks[1].ImageURL.URL != "data:image/png;base64,iVBORw0KGgo=" || blocks[2].ImageURL.URL != "https://example.com/cat.png" {
		t.Errorf("blocks = %+v", blocks)
	}
}

func TestConvertCohereRequestErrors(t *testing.T) {
	cases := []struct {
		name string
		req  ChatCompletionRequest
	}{
		{"empty model", ChatCompletionRequest{Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}}}},
		{"no messages", ChatCompletionRequest{Model: "m"}},
		{"negative temperature", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}}, Temperature: fptr(-0.5)}},
		{"top_p 1.0", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}}, TopP: fptr(1.0)}},
		{"six stops", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}}, Stop: []string{"a", "b", "c", "d", "e", "f"}}},
		{"tool result without id", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "tool", Content: mustJSON("x")}}}},
		{"required with no tools", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSON("x")}}, ToolChoice: "required"}},
		{"unknown role", ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "admin", Content: mustJSON("x")}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ConvertCohereRequest(tc.req); err == nil {
				t.Errorf("expected error")
			} else {
				var ue *UnrepresentableError
				if !errors.As(err, &ue) {
					t.Errorf("err = %T, want *UnrepresentableError", err)
				}
			}
		})
	}
	// 21 images exceed the documented 20-image cap.
	parts := make([]ContentPart, 0, 21)
	for i := 0; i < 21; i++ {
		parts = append(parts, ContentPart{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/i.png"}})
	}
	if _, err := ConvertCohereRequest(ChatCompletionRequest{Model: "m", Messages: []ChatMessage{{Role: "user", Content: mustJSONValue(parts)}}}); err == nil {
		t.Errorf("21 images: expected error")
	}
}

func TestConvertCohereResponse(t *testing.T) {
	resp := &CohereResponse{
		ID:           "resp-1",
		FinishReason: "COMPLETE",
		Message: CohereMessage{
			Role:    "assistant",
			Content: mustJSONValue([]CohereContentBlock{{Type: "text", Text: "hi there"}}),
		},
		Meta: &CohereMeta{Tokens: &CohereUsage{InputTokens: 12, OutputTokens: 8}},
	}
	out, err := ConvertCohereResponse(resp)
	if err != nil {
		t.Fatalf("ConvertCohereResponse: %v", err)
	}
	if out.ID != "resp-1" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("ID/FinishReason = %q/%q", out.ID, out.Choices[0].FinishReason)
	}
	var content string
	if err := json.Unmarshal(out.Choices[0].Message.Content, &content); err != nil || content != "hi there" {
		t.Errorf("content = %s (err %v)", out.Choices[0].Message.Content, err)
	}
	if out.Usage.PromptTokens != 12 || out.Usage.CompletionTokens != 8 || out.Usage.TotalTokens != 20 {
		t.Errorf("Usage = %+v", out.Usage)
	}
}

func TestConvertCohereResponseToolCalls(t *testing.T) {
	// Tool calls arrive both as {"type":"tool-call"} content blocks
	// and as a message.tool_calls list; the same call reported twice
	// must appear once.
	resp := &CohereResponse{
		FinishReason: "TOOL_CALL",
		Message: CohereMessage{
			Role: "assistant",
			Content: mustJSONValue([]CohereContentBlock{
				{Type: "tool-call", ID: "call_1", Name: "get_weather", Parameters: json.RawMessage(`{"city":"Pune"}`)},
			}),
			ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}}},
		},
		Usage: &CohereUsage{InputTokens: 20, OutputTokens: 5},
	}
	out, err := ConvertCohereResponse(resp)
	if err != nil {
		t.Fatalf("ConvertCohereResponse: %v", err)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", out.Choices[0].FinishReason)
	}
	if len(out.Choices[0].Message.ToolCalls) != 1 || out.Choices[0].Message.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCalls = %+v", out.Choices[0].Message.ToolCalls)
	}
}

func TestConvertCohereResponseFinishReasons(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		err  bool
	}{
		{"COMPLETE", "stop", false},
		{"complete", "stop", false},
		{"MAX_TOKENS", "length", false},
		{"max_tokens", "length", false},
		{"TOOL_CALL", "tool_calls", false},
		{"STOP_SEQUENCE", "stop", false},
		{"ERROR", "", true},
		{"TIMEOUT", "", true},
	} {
		out, err := ConvertCohereResponse(&CohereResponse{
			FinishReason: tc.in,
			Message:      CohereMessage{Role: "assistant", Content: mustJSON("x")},
		})
		if tc.err {
			if err == nil {
				t.Errorf("%q: expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if out.Choices[0].FinishReason != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, out.Choices[0].FinishReason, tc.want)
		}
	}
	// A TIMEOUT finish is retryable, ERROR is not.
	if _, err := ConvertCohereResponse(&CohereResponse{FinishReason: "TIMEOUT", Message: CohereMessage{Role: "assistant"}}); err == nil || !IsCohereRetryable(err) {
		t.Errorf("TIMEOUT err = %v, want retryable CohereError", err)
	}
	if _, err := ConvertCohereResponse(&CohereResponse{FinishReason: "ERROR", Message: CohereMessage{Role: "assistant"}}); err == nil || IsCohereRetryable(err) {
		t.Errorf("ERROR err = %v, want non-retryable CohereError", err)
	}
}

func TestMapCohereError(t *testing.T) {
	err := MapCohereError(http.StatusTooManyRequests, http.Header{"Retry-After": []string{"30"}}, []byte(`{"message":"Too many requests"}`))
	var ce *CohereError
	if !errors.As(err, &ce) || !ce.IsRateLimit() || !ce.Retryable || ce.RetryAfter != 30e9 {
		t.Errorf("429 err = %+v", err)
	}
	err = MapCohereError(http.StatusInternalServerError, nil, []byte(`{"message":"boom"}`))
	if !errors.As(err, &ce) || !ce.Retryable || ce.StatusCode != 500 {
		t.Errorf("500 err = %+v", err)
	}
	err = MapCohereError(http.StatusBadRequest, nil, []byte(`{"message":"bad model"}`))
	if !errors.As(err, &ce) || ce.Retryable {
		t.Errorf("400 err = %+v, want non-retryable", err)
	}
	// Non-JSON body survives as the message.
	err = MapCohereError(http.StatusServiceUnavailable, nil, []byte(`plain text`))
	if !errors.As(err, &ce) || ce.Message != "plain text" {
		t.Errorf("plain body err = %+v", err)
	}
}

func TestTranslateCohereStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"type":"message-start","id":"chat-1","delta":{"message":{"role":"assistant"}}}`,
		``,
		`data: {"type":"content-start","index":0,"delta":{"message":{"content":{"type":"text"}}}}`,
		``,
		`data: {"type":"content-delta","index":0,"delta":{"message":{"content":{"type":"text","text":"hi "}}}}`,
		``,
		`data: {"type":"content-delta","index":0,"delta":{"message":{"content":{"type":"text","text":"there"}}}}`,
		``,
		`data: {"type":"tool-call-start","index":1,"delta":{"message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather"}}]}}}`,
		``,
		`data: {"type":"tool-call-delta","index":1,"delta":{"message":{"tool_calls":[{"id":"call_1","function":{"arguments":"{\"city\":"}}]}}}`,
		``,
		`data: {"type":"tool-call-delta","index":1,"delta":{"message":{"tool_calls":[{"id":"call_1","function":{"arguments":"\"Pune\"}"}}]}}}`,
		``,
		`data: {"type":"tool-call-end","index":1}`,
		``,
		`data: {"type":"message-end","delta":{"finish_reason":"TOOL_CALL","usage":{"input_tokens":12,"output_tokens":8}}}`,
		``,
	}, "\n")
	var chunks []ChatChunk
	res, err := TranslateCohereStream(strings.NewReader(body), func(c ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateCohereStream: %v", err)
	}
	if res.ID != "chat-1" || res.Content != "hi there" || res.FinishReason != "tool_calls" {
		t.Errorf("result = %+v", res)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCalls = %+v", res.ToolCalls)
	}
	if res.Usage.PromptTokens != 12 || res.Usage.TotalTokens != 20 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	if len(chunks) == 0 || chunks[len(chunks)-1].Choices[0].FinishReason == nil || *chunks[len(chunks)-1].Choices[0].FinishReason != "tool_calls" {
		t.Errorf("final chunk missing finish_reason, chunks = %d", len(chunks))
	}
}

func TestTranslateCohereStreamErrors(t *testing.T) {
	// Error event mid-stream.
	body := "data: {\"type\":\"message-start\",\"id\":\"x\",\"delta\":{\"message\":{\"role\":\"assistant\"}}}\n\ndata: {\"type\":\"error\",\"message\":\"Too many requests\"}\n\n"
	if _, err := TranslateCohereStream(strings.NewReader(body), nil); err == nil {
		t.Errorf("error event: expected error")
	} else {
		var ce *CohereError
		if !errors.As(err, &ce) || !ce.Retryable {
			t.Errorf("error event err = %v, want retryable CohereError", err)
		}
	}
	// Empty stream.
	if _, err := TranslateCohereStream(strings.NewReader(""), nil); err == nil {
		t.Errorf("empty stream: expected error")
	}
	// Truncated stream (no message-end).
	body = "data: {\"type\":\"message-start\",\"id\":\"x\",\"delta\":{\"message\":{\"role\":\"assistant\"}}}\n\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"type\":\"text\",\"text\":\"hi\"}}}}\n\n"
	if _, err := TranslateCohereStream(strings.NewReader(body), nil); err == nil {
		t.Errorf("truncated stream: expected error")
	}
	// Malformed JSON.
	if _, err := TranslateCohereStream(strings.NewReader("data: {not json}\n\n"), nil); err == nil {
		t.Errorf("malformed JSON: expected error")
	}
}
