package wire

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func mustGeminiReq(t *testing.T) ChatCompletionRequest {
	t.Helper()
	return ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: mustJSON("Be helpful.")},
			{Role: "user", Content: mustJSON("Hi")},
		},
	}
}

func TestConvertGeminiRequestBasic(t *testing.T) {
	out, err := ConvertGeminiRequest(mustGeminiReq(t))
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	if out.SystemInstruction == nil || len(out.SystemInstruction.Parts) != 1 || out.SystemInstruction.Parts[0].Text != "Be helpful." {
		t.Errorf("SystemInstruction = %+v, want the system message extracted", out.SystemInstruction)
	}
	if len(out.Contents) != 1 || out.Contents[0].Role != "user" {
		t.Fatalf("Contents = %+v, want one user content", out.Contents)
	}
	if out.Contents[0].Parts[0].Text != "Hi" {
		t.Errorf("content text = %q", out.Contents[0].Parts[0].Text)
	}
	if out.GenerationConfig != nil {
		t.Errorf("GenerationConfig = %+v, want nil when nothing set", out.GenerationConfig)
	}
}

func TestConvertGeminiRequestGenerationConfig(t *testing.T) {
	temp := 0.7
	topP := 0.9
	req := mustGeminiReq(t)
	req.Temperature = &temp
	req.TopP = &topP
	req.MaxTokens = 100
	req.Stop = []string{"END"}
	out, err := ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	gen := out.GenerationConfig
	if gen == nil {
		t.Fatal("GenerationConfig is nil")
	}
	if *gen.Temperature != 0.7 || *gen.TopP != 0.9 || *gen.MaxOutputTokens != 100 {
		t.Errorf("GenerationConfig = %+v", gen)
	}
	if len(gen.StopSequences) != 1 || gen.StopSequences[0] != "END" {
		t.Errorf("StopSequences = %v", gen.StopSequences)
	}
}

func TestConvertGeminiRequestTemperatureBounds(t *testing.T) {
	req := mustGeminiReq(t)
	bad := 2.5
	req.Temperature = &bad
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("temperature 2.5 must fail on the Gemini wire")
	}
	req.Temperature = nil
	bad = -0.1
	req.Temperature = &bad
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("temperature -0.1 must fail")
	}
	// 2.0 is the Gemini maximum and is allowed.
	req.Temperature = &bad
	*req.Temperature = 2.0
	if _, err := ConvertGeminiRequest(req); err != nil {
		t.Fatalf("temperature 2.0 must be allowed: %v", err)
	}
}

func TestConvertGeminiRequestTooManyStops(t *testing.T) {
	req := mustGeminiReq(t)
	req.Stop = []string{"a", "b", "c", "d", "e", "f"}
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("6 stop sequences must fail (Gemini caps at 5)")
	}
}

func TestConvertGeminiRequestRolesAndMerging(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("one")},
			{Role: "user", Content: mustJSON("two")},
			{Role: "assistant", Content: mustJSON("answer")},
			{Role: "assistant", Content: mustJSON("more")},
		},
	}
	out, err := ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	// Consecutive same-role contents merge: user, model.
	if len(out.Contents) != 2 {
		t.Fatalf("Contents = %d, want 2 merged contents", len(out.Contents))
	}
	if out.Contents[0].Role != "user" || len(out.Contents[0].Parts) != 2 {
		t.Errorf("first content = %+v", out.Contents[0])
	}
	if out.Contents[1].Role != "model" || len(out.Contents[1].Parts) != 2 {
		t.Errorf("second content = %+v", out.Contents[1])
	}
}

func TestConvertGeminiRequestFirstMustBeUser(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{
			{Role: "assistant", Content: mustJSON("hello")},
		},
	}
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("a conversation opening with a model turn must fail")
	}
}

func TestConvertGeminiRequestToolFlow(t *testing.T) {
	req := ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON("weather in Pune?")},
			{Role: "assistant", Content: mustJSON(""), ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: ToolCallFunction{
					Name:      "get_weather",
					Arguments: `{"city":"Pune"}`,
				},
			}}},
			{Role: "tool", Content: mustJSON("sunny, 28C"), ToolCallID: "call_1"},
			{Role: "user", Content: mustJSON("thanks")},
		},
		Tools: []ToolDefinition{{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: "get_weather", Description: "Get the weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		}},
	}
	out, err := ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	if len(out.Tools) != 1 || len(out.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("Tools = %+v, want one functionDeclaration", out.Tools)
	}
	if out.Tools[0].FunctionDeclarations[0].Name != "get_weather" {
		t.Errorf("declaration name = %q", out.Tools[0].FunctionDeclarations[0].Name)
	}
	if len(out.Contents) != 4 {
		t.Fatalf("Contents = %d, want 4 (user, model, function, user)", len(out.Contents))
	}
	model := out.Contents[1]
	if model.Role != "model" || len(model.Parts) != 1 || model.Parts[0].FunctionCall == nil {
		t.Fatalf("model content = %+v, want one functionCall part", model)
	}
	if model.Parts[0].FunctionCall.Name != "get_weather" {
		t.Errorf("functionCall name = %q", model.Parts[0].FunctionCall.Name)
	}
	fn := out.Contents[2]
	if fn.Role != "function" || len(fn.Parts) != 1 || fn.Parts[0].FunctionResponse == nil {
		t.Fatalf("function content = %+v, want one functionResponse part", fn)
	}
	// The function name comes from the earlier assistant call (matched
	// on tool_call_id), not from the call ID itself.
	if fn.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Errorf("functionResponse name = %q, want get_weather", fn.Parts[0].FunctionResponse.Name)
	}
	if !strings.Contains(string(fn.Parts[0].FunctionResponse.Response), "sunny, 28C") {
		t.Errorf("functionResponse response = %s", fn.Parts[0].FunctionResponse.Response)
	}
}

func TestConvertGeminiRequestToolChoiceForms(t *testing.T) {
	tools := []ToolDefinition{{
		Type: "function",
		Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: "get_weather"},
	}}
	base := func() ChatCompletionRequest {
		return ChatCompletionRequest{
			Model:    "gemini-2.0-flash",
			Messages: []ChatMessage{{Role: "user", Content: mustJSON("hi")}},
			Tools:    tools,
		}
	}
	// "none" drops tools and sets mode NONE.
	req := base()
	req.ToolChoice = "none"
	out, err := ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("tool_choice none: %v", err)
	}
	if len(out.Tools) != 0 || out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig.Mode != "NONE" {
		t.Errorf("none: Tools=%v ToolConfig=%+v", out.Tools, out.ToolConfig)
	}
	// "required" sets mode ANY.
	req = base()
	req.ToolChoice = "required"
	out, err = ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("tool_choice required: %v", err)
	}
	if out.ToolConfig == nil || out.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Errorf("required: ToolConfig=%+v", out.ToolConfig)
	}
	// A named function sets ANY + allowedFunctionNames.
	req = base()
	req.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}
	out, err = ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("tool_choice named: %v", err)
	}
	cfg := out.ToolConfig.FunctionCallingConfig
	if cfg.Mode != "ANY" || len(cfg.AllowedFunctionNames) != 1 || cfg.AllowedFunctionNames[0] != "get_weather" {
		t.Errorf("named: %+v", cfg)
	}
	// A named function that is not defined fails.
	req = base()
	req.ToolChoice = map[string]any{"type": "function", "function": map[string]any{"name": "nope"}}
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("naming an undefined tool must fail")
	}
	// No tool_choice: no toolConfig (Gemini defaults to AUTO).
	req = base()
	out, err = ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("no tool_choice: %v", err)
	}
	if out.ToolConfig != nil {
		t.Errorf("ToolConfig = %+v, want nil (default AUTO)", out.ToolConfig)
	}
}

func TestConvertGeminiRequestImageDataURL(t *testing.T) {
	partsJSON, err := json.Marshal([]ContentPart{
		{Type: "text", Text: "what is this?"},
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64,aGVsbG8="}},
	})
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	req := ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: partsJSON,
		}},
	}
	out, err := ConvertGeminiRequest(req)
	if err != nil {
		t.Fatalf("ConvertGeminiRequest: %v", err)
	}
	parts := out.Contents[0].Parts
	if len(parts) != 2 || parts[1].InlineData == nil {
		t.Fatalf("parts = %+v, want text + inlineData", parts)
	}
	if parts[1].InlineData.MimeType != "image/png" || parts[1].InlineData.Data != "aGVsbG8=" {
		t.Errorf("inlineData = %+v", parts[1].InlineData)
	}
}

func TestConvertGeminiRequestImageHTTPURLUnrepresentable(t *testing.T) {
	partsJSON, err := json.Marshal([]ContentPart{
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/pic.png"}},
	})
	if err != nil {
		t.Fatalf("marshal parts: %v", err)
	}
	req := ChatCompletionRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{{
			Role:    "user",
			Content: partsJSON,
		}},
	}
	if _, err := ConvertGeminiRequest(req); err == nil {
		t.Fatal("an http(s) image URL must be unrepresentable on Gemini (no fileData equivalent)")
	}
}

func TestConvertGeminiResponseBasic(t *testing.T) {
	resp, err := ConvertGeminiResponse(&GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Role: "model", Parts: []GeminiPart{
				{Text: "hi there"},
			}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &GeminiUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 4, TotalTokenCount: 14},
		ModelVersion:  "gemini-2.0-flash-001",
	})
	if err != nil {
		t.Fatalf("ConvertGeminiResponse: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d", len(resp.Choices))
	}
	var content string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &content); err != nil || content != "hi there" {
		t.Errorf("content = %s (%v)", resp.Choices[0].Message.Content, err)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.Choices[0].FinishReason)
	}
	if resp.Usage != (Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14}) {
		t.Errorf("Usage = %+v", resp.Usage)
	}
	if resp.Model != "gemini-2.0-flash-001" {
		t.Errorf("Model = %q", resp.Model)
	}
}

func TestConvertGeminiResponseFunctionCall(t *testing.T) {
	resp, err := ConvertGeminiResponse(&GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content: &GeminiContent{Role: "model", Parts: []GeminiPart{
				{FunctionCall: &GeminiFunctionCall{Name: "get_weather", Args: json.RawMessage(`{"city":"Pune"}`)}},
			}},
			FinishReason: "TOOL_CALLS",
		}},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiResponse: %v", err)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.Choices[0].FinishReason)
	}
	tc := resp.Choices[0].Message.ToolCalls
	if len(tc) != 1 || tc[0].Function.Name != "get_weather" || tc[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("ToolCalls = %+v", tc)
	}
}

func TestMapGeminiFinishReason(t *testing.T) {
	cases := map[string]string{
		"STOP": "stop", "MAX_TOKENS": "length", "SAFETY": "content_filter",
		"RECITATION": "content_filter", "TOOL_CALLS": "tool_calls",
		"OTHER": "stop", "": "stop", "BOGUS": "stop",
	}
	for in, want := range cases {
		if got := MapGeminiFinishReason(in); got != want {
			t.Errorf("MapGeminiFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertGeminiResponseBlockedPrompt(t *testing.T) {
	resp, err := ConvertGeminiResponse(&GeminiResponse{
		PromptFeedback: &GeminiPromptFeedback{BlockReason: "SAFETY"},
	})
	if err != nil {
		t.Fatalf("a blocked prompt must convert, not error: %v", err)
	}
	if resp.Choices[0].FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", resp.Choices[0].FinishReason)
	}
}

func TestConvertGeminiResponseNoCandidates(t *testing.T) {
	if _, err := ConvertGeminiResponse(&GeminiResponse{}); err == nil {
		t.Fatal("no candidates and no block reason must fail")
	}
}

func TestConvertGeminiResponseImageOutputUnrepresentable(t *testing.T) {
	_, err := ConvertGeminiResponse(&GeminiResponse{
		Candidates: []GeminiCandidate{{
			Content:      &GeminiContent{Role: "model", Parts: []GeminiPart{{InlineData: &GeminiInlineData{MimeType: "image/png", Data: "x"}}}},
			FinishReason: "STOP",
		}},
	})
	if err == nil {
		t.Fatal("image output must be unrepresentable")
	}
}

func TestMapGeminiError(t *testing.T) {
	body := []byte(`{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`)
	err := MapGeminiError(http.StatusTooManyRequests, nil, body)
	var ge *GeminiError
	if !errors.As(err, &ge) {
		t.Fatalf("error is %T, want *GeminiError", err)
	}
	if !ge.Retryable || !ge.IsRateLimit() {
		t.Errorf("429 RESOURCE_EXHAUSTED must be retryable + rate limit: %+v", ge)
	}
	if ge.Status != "RESOURCE_EXHAUSTED" || ge.Message != "quota exceeded" {
		t.Errorf("parsed = %+v", ge)
	}
	// 503 UNAVAILABLE is retryable.
	err = MapGeminiError(http.StatusServiceUnavailable, nil, []byte(`{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`))
	if !IsGeminiRetryable(err) {
		t.Errorf("503 UNAVAILABLE must be retryable: %v", err)
	}
	// A 400 INVALID_ARGUMENT is not retryable.
	err = MapGeminiError(http.StatusBadRequest, nil, []byte(`{"error":{"code":400,"message":"bad model","status":"INVALID_ARGUMENT"}}`))
	if IsGeminiRetryable(err) {
		t.Errorf("400 INVALID_ARGUMENT must not be retryable: %v", err)
	}
	// Retry-After is parsed.
	h := http.Header{}
	h.Set("Retry-After", "3")
	err = MapGeminiError(http.StatusTooManyRequests, h, body)
	if !errors.As(err, &ge) || ge.RetryAfter.Seconds() != 3 {
		t.Errorf("RetryAfter = %v", ge)
	}
	// The generalized IsRetryable covers Gemini errors too.
	if !IsRetryable(MapGeminiError(http.StatusTooManyRequests, nil, body)) {
		t.Error("IsRetryable must cover *GeminiError")
	}
	if HTTPStatus(MapGeminiError(http.StatusTooManyRequests, nil, body)) != http.StatusTooManyRequests {
		t.Error("HTTPStatus must cover *GeminiError")
	}
}

func TestTranslateGeminiStreamText(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi \"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":2,\"totalTokenCount\":12}}\n\ndata: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":4,\"totalTokenCount\":14}}\n\n"
	var chunks []ChatChunk
	res, err := TranslateGeminiStream(strings.NewReader(body), func(c ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateGeminiStream: %v", err)
	}
	if res.Content != "hi there" {
		t.Errorf("Content = %q", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
	if res.Usage.TotalTokens != 14 || res.Usage.PromptTokens != 10 || res.Usage.CompletionTokens != 4 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	var text string
	for _, c := range chunks {
		if len(c.Choices) > 0 {
			text += c.Choices[0].Delta.Content
		}
	}
	if text != "hi there" {
		t.Errorf("chunk text = %q", text)
	}
	// The final chunk carries the finish reason and usage.
	last := chunks[len(chunks)-1]
	if last.Choices[0].FinishReason == nil || *last.Choices[0].FinishReason != "stop" {
		t.Errorf("final chunk finish reason = %+v", last.Choices[0].FinishReason)
	}
	if last.Usage == nil || last.Usage.TotalTokens != 14 {
		t.Errorf("final chunk usage = %+v", last.Usage)
	}
}

func TestTranslateGeminiStreamFunctionCall(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"get_weather\",\"args\":{\"city\":\"Pune\"}}}]},\"finishReason\":\"TOOL_CALLS\"}]}\n\n"
	res, err := TranslateGeminiStream(strings.NewReader(body), func(ChatChunk) error { return nil })
	if err != nil {
		t.Fatalf("TranslateGeminiStream: %v", err)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", res.FinishReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "get_weather" {
		t.Fatalf("ToolCalls = %+v", res.ToolCalls)
	}
	if res.ToolCalls[0].Function.Arguments != `{"city":"Pune"}` {
		t.Errorf("arguments = %q", res.ToolCalls[0].Function.Arguments)
	}
}

func TestTranslateGeminiStreamEmpty(t *testing.T) {
	if _, err := TranslateGeminiStream(strings.NewReader(""), func(ChatChunk) error { return nil }); err == nil {
		t.Fatal("an empty stream must fail, not succeed with nothing")
	}
}

func TestTranslateGeminiStreamError(t *testing.T) {
	body := "data: {\"error\":{\"code\":429,\"message\":\"slow down\",\"status\":\"RESOURCE_EXHAUSTED\"}}\n\n"
	_, err := TranslateGeminiStream(strings.NewReader(body), func(ChatChunk) error { return nil })
	var ge *GeminiError
	if !errors.As(err, &ge) || !ge.IsRateLimit() {
		t.Fatalf("error = %T %v, want rate-limit *GeminiError", err, err)
	}
}

func TestTranslateGeminiStreamBlocked(t *testing.T) {
	body := "data: {\"promptFeedback\":{\"blockReason\":\"SAFETY\"}}\n\n"
	res, err := TranslateGeminiStream(strings.NewReader(body), func(ChatChunk) error { return nil })
	if err != nil {
		t.Fatalf("TranslateGeminiStream: %v", err)
	}
	if res.FinishReason != "content_filter" {
		t.Errorf("FinishReason = %q, want content_filter", res.FinishReason)
	}
}
