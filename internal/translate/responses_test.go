package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertResponsesRequestRoundTrip translates an OpenAI
// chat-completions request into a Responses request, re-encodes it
// through JSON, and maps a Responses response answering it back to
// OpenAI, asserting the assistant turn round-trips semantically.
func TestConvertResponsesRequestRoundTrip(t *testing.T) {
	temp := 0.5
	req := wire.ChatCompletionRequest{
		Model:       "gpt-6.2",
		Messages:    []wire.ChatMessage{{Role: "system", Content: mustJSON("be brief")}, {Role: "user", Content: mustJSON("hello world")}},
		Temperature: &temp,
		MaxTokens:   512,
	}
	out, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("ConvertResponsesRequest: %v", err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ResponsesRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if decoded.Model != "gpt-6.2" || decoded.Instructions != "be brief" {
		t.Errorf("model/instructions = %q/%q", decoded.Model, decoded.Instructions)
	}
	if decoded.MaxOutputTokens != 512 || decoded.Temperature == nil || *decoded.Temperature != 0.5 {
		t.Errorf("max_output_tokens/temperature = %d/%v", decoded.MaxOutputTokens, decoded.Temperature)
	}
	items, ok := decoded.Input.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("input = %v", decoded.Input)
	}
	first, _ := items[0].(map[string]any)
	if first["type"] != "message" || first["role"] != "user" {
		t.Errorf("input[0] = %v", first)
	}
	content, _ := first["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "hello world" {
		t.Errorf("input[0].content[0] = %v", part)
	}

	// The provider answers; the response maps back to OpenAI.
	resp := &ResponsesResponse{
		ID:        "resp_123",
		Object:    "response",
		CreatedAt: 1752100704,
		Status:    "completed",
		Model:     "gpt-6.2",
		Output: []ResponsesOutputItem{{
			Type: "message",
			Role: "assistant",
			Content: []struct {
				Type    string `json:"type"`
				Text    string `json:"text,omitempty"`
				Refusal string `json:"refusal,omitempty"`
			}{{Type: "output_text", Text: "hi!"}},
		}},
		Usage: &ResponsesUsage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13},
	}
	resp.Usage.InputTokensDetail.CachedTokens = 4
	resp.Usage.OutputTokensDetail.ReasoningTokens = 1
	back, err := ConvertResponsesResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponsesResponse: %v", err)
	}
	var reply string
	if err := json.Unmarshal(back.Choices[0].Message.Content, &reply); err != nil || reply != "hi!" {
		t.Errorf("reply = %s (err %v)", back.Choices[0].Message.Content, err)
	}
	if back.Choices[0].Message.Role != "assistant" || back.Choices[0].FinishReason != "stop" {
		t.Errorf("role/finish = %s/%s", back.Choices[0].Message.Role, back.Choices[0].FinishReason)
	}
	if back.ID != "resp_123" || back.Model != "gpt-6.2" || back.Created != 1752100704 {
		t.Errorf("id/model/created = %q/%q/%d", back.ID, back.Model, back.Created)
	}
	if back.Usage.PromptTokens != 10 || back.Usage.CompletionTokens != 3 || back.Usage.TotalTokens != 13 ||
		back.Usage.CachedTokens != 4 || back.Usage.ReasoningTokens != 1 {
		t.Errorf("usage = %+v", back.Usage)
	}
}

// TestConvertResponsesToolsRoundTrip covers the tool loop: definitions,
// function_call input items, function_call_output items, and a response
// with function_call output items (finish_reason tool_calls).
func TestConvertResponsesToolsRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "gpt-6.2",
		Messages: []wire.ChatMessage{
			{Role: "user", Content: mustJSON("weather?")},
			{
				Role: "assistant",
				ToolCalls: []wire.ToolCall{{
					ID:   "call_1",
					Type: "function",
					Function: wire.ToolCallFunction{
						Name:      "get_weather",
						Arguments: `{"city":"Seoul"}`,
					},
				}},
			},
			{Role: "tool", ToolCallID: "call_1", Content: mustJSON("17C clear")},
		},
		Tools: []wire.ToolDefinition{namedTool("get_weather")},
	}
	out, err := ConvertResponsesRequest(req)
	if err != nil {
		t.Fatalf("ConvertResponsesRequest: %v", err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "get_weather" || out.Tools[0].Type != "function" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	items := out.Input.([]ResponsesInputItem)
	if len(items) != 3 {
		t.Fatalf("input items = %d", len(items))
	}
	if items[1].Type != "function_call" || items[1].CallID != "call_1" ||
		items[1].Name != "get_weather" || items[1].Arguments != `{"city":"Seoul"}` {
		t.Errorf("function_call item = %+v", items[1])
	}
	if items[2].Type != "function_call_output" || items[2].CallID != "call_1" || items[2].Output != "17C clear" {
		t.Errorf("function_call_output item = %+v", items[2])
	}

	// The provider answers with a function_call output item.
	resp := &ResponsesResponse{
		ID:     "resp_9",
		Status: "completed",
		Output: []ResponsesOutputItem{{
			Type:      "function_call",
			CallID:    "call_9",
			Name:      "get_weather",
			Arguments: `{"city":"Seoul"}`,
		}},
	}
	back, err := ConvertResponsesResponse(resp)
	if err != nil {
		t.Fatalf("ConvertResponsesResponse: %v", err)
	}
	tcs := back.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].ID != "call_9" || tcs[0].Function.Name != "get_weather" {
		t.Fatalf("tool calls = %+v", tcs)
	}
	if tcs[0].Function.Arguments != `{"city":"Seoul"}` {
		t.Errorf("arguments = %s", tcs[0].Function.Arguments)
	}
	if back.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q", back.Choices[0].FinishReason)
	}
}

func TestConvertResponsesRefusals(t *testing.T) {
	// Stop sequences have no Responses parameter: refused, never dropped.
	req := wire.ChatCompletionRequest{
		Model:    "gpt-6.2",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
		Stop:     []string{"END"},
	}
	if _, err := ConvertResponsesRequest(req); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
	// Incomplete because of max_output_tokens maps to finish "length".
	resp := &ResponsesResponse{
		Status: "incomplete",
		IncompleteDetails: &struct {
			Reason string `json:"reason"`
		}{Reason: "max_output_tokens"},
	}
	back, err := ConvertResponsesResponse(resp)
	if err != nil {
		t.Fatalf("incomplete response: %v", err)
	}
	if back.Choices[0].FinishReason != "length" {
		t.Errorf("finish_reason = %q", back.Choices[0].FinishReason)
	}
	// A failed response is an error.
	resp.Status = "failed"
	resp.Error = &struct {
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	}{Message: "boom"}
	if _, err := ConvertResponsesResponse(resp); err == nil {
		t.Errorf("failed response returned no error")
	} else {
		var re *ResponsesError
		if !errors.As(err, &re) {
			t.Errorf("err = %v, want *ResponsesError", err)
		}
	}
}

// TestTranslateResponsesStream feeds a documented event sequence —
// response.created, output_text deltas, a function_call item with
// argument deltas, response.completed with usage — and asserts the
// emitted OpenAI chunk sequence reassembles.
func TestTranslateResponsesStream(t *testing.T) {
	stream := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-6.2"}}` + "\n\n" +
		"event: response.in_progress\n" +
		`data: {"type":"response.in_progress","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"Hel"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"lo"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather"}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\""}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":":\"Seoul\"}"}` + "\n\n" +
		"event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","text":"Hello"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":8,"output_tokens":6,"total_tokens":14,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}` + "\n\n"

	var chunks []wire.ChatChunk
	res, err := TranslateResponsesStream(strings.NewReader(stream), func(c wire.ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateResponsesStream: %v", err)
	}
	if res.ID != "resp_1" || res.Model != "gpt-6.2" {
		t.Errorf("id/model = %q/%q", res.ID, res.Model)
	}
	if res.Content != "Hello" || res.FinishReason != "stop" {
		t.Fatalf("content/finish = %q/%q", res.Content, res.FinishReason)
	}
	if res.Usage.PromptTokens != 8 || res.Usage.CompletionTokens != 6 || res.Usage.TotalTokens != 14 ||
		res.Usage.CachedTokens != 2 || res.Usage.ReasoningTokens != 1 {
		t.Errorf("usage = %+v", res.Usage)
	}
	// 2 content deltas + tool-call start + 2 arg deltas + final.
	if len(chunks) != 6 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content != "Hel" || chunks[1].Choices[0].Delta.Content != "lo" {
		t.Errorf("content deltas = %q,%q", chunks[0].Choices[0].Delta.Content, chunks[1].Choices[0].Delta.Content)
	}
	start := chunks[2].Choices[0].Delta.ToolCalls
	if len(start) != 1 || start[0].Index != 1 || start[0].ID != "call_1" || start[0].Function.Name != "get_weather" {
		t.Fatalf("tool-call start = %+v", start)
	}
	args := chunks[3].Choices[0].Delta.ToolCalls[0].Function.Arguments + chunks[4].Choices[0].Delta.ToolCalls[0].Function.Arguments
	if args != `{"city":"Seoul"}` {
		t.Errorf("reassembled arguments = %q", args)
	}
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v", final.Choices[0].FinishReason)
	}
	if final.Usage == nil || final.Usage.TotalTokens != 14 {
		t.Errorf("final usage = %v", final.Usage)
	}
}

func TestTranslateResponsesStreamEmpty(t *testing.T) {
	if _, err := TranslateResponsesStream(strings.NewReader(""), nil); err == nil {
		t.Errorf("empty stream returned no error")
	}
}

func TestTranslateResponsesStreamFailed(t *testing.T) {
	stream := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"status":"failed","error":{"message":"quota exceeded"}}}` + "\n\n"
	_, err := TranslateResponsesStream(strings.NewReader(stream), nil)
	var re *ResponsesError
	if !errors.As(err, &re) || re.Message != "quota exceeded" {
		t.Fatalf("err = %v, want *ResponsesError quota exceeded", err)
	}
}
