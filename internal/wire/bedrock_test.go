package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"net/http"
	"strings"
	"testing"
)

// buildEventStreamFrame builds one Amazon event-stream frame for a
// ConverseStream event: headers ":event-type" / ":message-type" /
// ":content-type" plus a JSON payload, with correct prelude and
// message CRCs. It is the test-side encoder for readEventStream.
func buildEventStreamFrame(t *testing.T, eventType string, payload any) []byte {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal event payload: %v", err)
	}
	var headers bytes.Buffer
	writeHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7) // string
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(value)))
		headers.WriteString(value)
	}
	writeHeader(":event-type", eventType)
	writeHeader(":message-type", "event")
	writeHeader(":content-type", "application/json")
	totalLen := 12 + headers.Len() + len(body) + 4
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(totalLen))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	prelude := frame.Bytes()
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(prelude))
	frame.Write(headers.Bytes())
	frame.Write(body)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}

// buildEventStreamException builds one exception frame
// (:message-type "exception", :exception-type + :message headers).
func buildEventStreamException(t *testing.T, exceptionType, message string) []byte {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"message": message})
	var headers bytes.Buffer
	writeHeader := func(name, value string) {
		headers.WriteByte(byte(len(name)))
		headers.WriteString(name)
		headers.WriteByte(7)
		_ = binary.Write(&headers, binary.BigEndian, uint16(len(value)))
		headers.WriteString(value)
	}
	writeHeader(":exception-type", exceptionType)
	writeHeader(":message-type", "exception")
	writeHeader(":message", message)
	totalLen := 12 + headers.Len() + len(body) + 4
	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, uint32(totalLen))
	_ = binary.Write(&frame, binary.BigEndian, uint32(headers.Len()))
	prelude := frame.Bytes()
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(prelude))
	frame.Write(headers.Bytes())
	frame.Write(body)
	_ = binary.Write(&frame, binary.BigEndian, crc32.ChecksumIEEE(frame.Bytes()))
	return frame.Bytes()
}

func TestConvertBedrockRequestBasic(t *testing.T) {
	temp := 0.7
	req := ChatCompletionRequest{
		Model:       "anthropic.claude-3-haiku-20240307-v1:0",
		Messages:    []ChatMessage{{Role: "system", Content: json.RawMessage(`"You are helpful."`)}, {Role: "user", Content: json.RawMessage(`"Hi"`)}},
		MaxTokens:   100,
		Temperature: &temp,
		Stop:        []string{"END"},
	}
	got, err := ConvertBedrockRequest(req)
	if err != nil {
		t.Fatalf("ConvertBedrockRequest: %v", err)
	}
	if len(got.System) != 1 || got.System[0].Text != "You are helpful." {
		t.Errorf("System = %+v", got.System)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" {
		t.Fatalf("Messages = %+v", got.Messages)
	}
	if got.InferenceConfig == nil || got.InferenceConfig.MaxTokens != 100 || *got.InferenceConfig.Temperature != 0.7 {
		t.Errorf("InferenceConfig = %+v", got.InferenceConfig)
	}
	if len(got.InferenceConfig.StopSequences) != 1 || got.InferenceConfig.StopSequences[0] != "END" {
		t.Errorf("StopSequences = %v", got.InferenceConfig.StopSequences)
	}
	if got.ToolConfig != nil {
		t.Errorf("ToolConfig should be nil without tools, got %+v", got.ToolConfig)
	}
}

func TestConvertBedrockRequestMergeAndTools(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "meta.llama3-70b-instruct-v1:0",
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"first"`)},
			{Role: "user", Content: json.RawMessage(`"second"`)},
			{Role: "assistant", Content: json.RawMessage(`"thinking"`), ToolCalls: []ToolCall{{ID: "call-1", Type: "function", Function: ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}}}},
			{Role: "tool", ToolCallID: "call-1", Content: json.RawMessage(`"sunny"`)},
		},
		Tools: []ToolDefinition{{Type: "function", Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}}},
	}
	got, err := ConvertBedrockRequest(req)
	if err != nil {
		t.Fatalf("ConvertBedrockRequest: %v", err)
	}
	// The two user messages merge into one; the tool result lands in
	// a second user message after the assistant turn.
	if len(got.Messages) != 3 {
		t.Fatalf("Messages = %d, want 3 (merged user, assistant, tool-user)", len(got.Messages))
	}
	if len(got.Messages[0].Content) != 2 {
		t.Errorf("merged user message blocks = %d, want 2", len(got.Messages[0].Content))
	}
	asst := got.Messages[1]
	if len(asst.Content) != 2 {
		t.Fatalf("assistant blocks = %d, want 2 (text + toolUse)", len(asst.Content))
	}
	if asst.Content[1].ToolUse == nil || asst.Content[1].ToolUse.Name != "get_weather" || asst.Content[1].ToolUse.ToolUseID != "call-1" {
		t.Errorf("assistant toolUse = %+v", asst.Content[1].ToolUse)
	}
	toolMsg := got.Messages[2]
	if toolMsg.Role != "user" || toolMsg.Content[0].ToolResult == nil || toolMsg.Content[0].ToolResult.ToolUseID != "call-1" {
		t.Errorf("tool message = %+v", toolMsg)
	}
	if got.ToolConfig == nil || len(got.ToolConfig.Tools) != 1 || got.ToolConfig.Tools[0].ToolSpec.Name != "get_weather" {
		t.Errorf("ToolConfig = %+v", got.ToolConfig)
	}
	if got.ToolConfig.ToolChoice == nil || got.ToolConfig.ToolChoice.Auto == nil {
		t.Errorf("ToolChoice should default to auto, got %+v", got.ToolConfig.ToolChoice)
	}
}

func TestConvertBedrockRequestImage(t *testing.T) {
	dataURL := "data:image/png;base64,aGVsbG8="
	req := ChatCompletionRequest{
		Model: "anthropic.claude-3-5-sonnet-20241022-v2:0",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: json.RawMessage(`[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + dataURL + `"}}]`),
		}},
	}
	got, err := ConvertBedrockRequest(req)
	if err != nil {
		t.Fatalf("ConvertBedrockRequest: %v", err)
	}
	img := got.Messages[0].Content[1].Image
	if img == nil || img.Format != "png" || img.Source.Bytes != "aGVsbG8=" {
		t.Errorf("image block = %+v", got.Messages[0].Content[1].Image)
	}
	// A remote image URL is unrepresentable.
	req.Messages[0].Content = json.RawMessage(`[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]`)
	if _, err := ConvertBedrockRequest(req); err == nil {
		t.Error("remote image URL: expected UnrepresentableError, got nil")
	}
}

func TestConvertBedrockRequestAssistantFirst(t *testing.T) {
	req := ChatCompletionRequest{
		Model:    "m",
		Messages: []ChatMessage{{Role: "assistant", Content: json.RawMessage(`"hi"`)}},
	}
	if _, err := ConvertBedrockRequest(req); err == nil {
		t.Error("assistant-first conversation: expected error, got nil")
	} else if _, ok := err.(*UnrepresentableError); !ok {
		t.Errorf("assistant-first: error type = %T, want *UnrepresentableError", err)
	}
}

func TestConvertBedrockResponse(t *testing.T) {
	resp := &BedrockResponse{
		StopReason: "tool_use",
		Usage:      BedrockUsage{InputTokens: 10, OutputTokens: 4, TotalTokens: 14},
	}
	resp.Output.Message = BedrockMessage{Role: "assistant", Content: []BedrockContentBlock{
		{Text: strPtr("let me check")},
		{ToolUse: &BedrockToolUse{ToolUseID: "t1", Name: "get_weather", Input: json.RawMessage(`{"city":"Pune"}`)}},
	}}
	got, err := ConvertBedrockResponse(resp)
	if err != nil {
		t.Fatalf("ConvertBedrockResponse: %v", err)
	}
	if got.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", got.Choices[0].FinishReason)
	}
	if got.Usage.TotalTokens != 14 {
		t.Errorf("Usage = %+v", got.Usage)
	}
	if len(got.Choices[0].Message.ToolCalls) != 1 || got.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("ToolCalls = %+v", got.Choices[0].Message.ToolCalls)
	}
	for _, stop := range []struct {
		in, want string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"guardrail_intervened", "stop"},
		{"content_filtered", "stop"},
		{"weird", "stop"},
	} {
		if got := MapBedrockStopReason(stop.in); got != stop.want {
			t.Errorf("MapBedrockStopReason(%q) = %q, want %q", stop.in, got, stop.want)
		}
	}
}

func strPtr(s string) *string { return &s }

func TestMapBedrockError(t *testing.T) {
	h := make(http.Header)
	h.Set("x-amzn-ErrorType", "com.amazonaws.bedrock#ThrottlingException")
	err := MapBedrockError(400, h, []byte(`{"message":"slow down"}`))
	be, ok := err.(*BedrockError)
	if !ok {
		t.Fatalf("error type = %T", err)
	}
	if be.Type != "ThrottlingException" || !be.IsRateLimit() || !be.Retryable {
		t.Errorf("BedrockError = %+v", be)
	}
	// Plain 429 without a type header.
	err = MapBedrockError(429, http.Header{}, []byte(`{"message":"too many"}`))
	if be, ok := err.(*BedrockError); !ok || !be.IsRateLimit() {
		t.Errorf("429 error = %+v", err)
	}
	// 500 is retryable, 400 validation is not.
	if be, _ := MapBedrockError(500, http.Header{}, []byte(`{"message":"boom"}`)).(*BedrockError); !be.Retryable {
		t.Error("500 should be retryable")
	}
	if be, _ := MapBedrockError(400, http.Header{}, []byte(`{"message":"bad input"}`)).(*BedrockError); be.Retryable {
		t.Error("400 validation should not be retryable")
	}
}

func TestTranslateBedrockStreamText(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}))
	buf.Write(buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "hel"}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "lo"}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}))
	buf.Write(buildEventStreamFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 5, "outputTokens": 2, "totalTokens": 7}}))
	buf.Write(buildEventStreamFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}))

	var chunks []ChatChunk
	res, err := TranslateBedrockStream(&buf, func(c ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateBedrockStream: %v", err)
	}
	if res.Content != "hello" || res.FinishReason != "stop" || res.Usage.TotalTokens != 7 {
		t.Errorf("result = %+v", res)
	}
	var text string
	for _, c := range chunks {
		if len(c.Choices) > 0 {
			text += c.Choices[0].Delta.Content
		}
	}
	if text != "hello" {
		t.Errorf("streamed text = %q, want hello", text)
	}
	if len(chunks) == 0 || chunks[len(chunks)-1].Choices[0].FinishReason == nil {
		t.Error("final chunk must carry finish_reason")
	}
}

func TestTranslateBedrockStreamToolUse(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}))
	buf.Write(buildEventStreamFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{"toolUse": map[string]any{"toolUseId": "t9", "name": "get_weather"}}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"toolUse": map[string]any{"input": `{"city":`}}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"toolUse": map[string]any{"input": `"Pune"}`}}}))
	buf.Write(buildEventStreamFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}))
	buf.Write(buildEventStreamFrame(t, "messageStop", map[string]any{"stopReason": "tool_use"}))

	res, err := TranslateBedrockStream(&buf, nil)
	if err != nil {
		t.Fatalf("TranslateBedrockStream: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", res.FinishReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCalls = %+v", res.ToolCalls)
	}
}

func TestTranslateBedrockStreamException(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}))
	buf.Write(buildEventStreamException(t, "com.amazonaws.bedrock#ThrottlingException", "slow down"))
	_, err := TranslateBedrockStream(&buf, nil)
	if err == nil {
		t.Fatal("expected error from exception event, got nil")
	}
	be, ok := err.(*BedrockError)
	if !ok {
		t.Fatalf("error type = %T, want *BedrockError", err)
	}
	if !be.IsRateLimit() {
		t.Errorf("exception error should be a rate limit, got %+v", be)
	}
}

func TestTranslateBedrockStreamTruncatedAndEmpty(t *testing.T) {
	// Truncated: no messageStop.
	var buf bytes.Buffer
	buf.Write(buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"}))
	if _, err := TranslateBedrockStream(&buf, nil); err == nil {
		t.Error("truncated stream: expected error, got nil")
	}
	// Empty.
	if _, err := TranslateBedrockStream(strings.NewReader(""), nil); err == nil {
		t.Error("empty stream: expected error, got nil")
	}
	// Corrupt CRC.
	bad := buildEventStreamFrame(t, "messageStart", map[string]any{"role": "assistant"})
	bad[len(bad)-1] ^= 0xff
	if _, err := TranslateBedrockStream(bytes.NewReader(bad), nil); err == nil {
		t.Error("corrupt CRC: expected error, got nil")
	}
}
