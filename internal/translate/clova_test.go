package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

func clovaChatReq(t *testing.T, req wire.ChatCompletionRequest) *ClovaRequest {
	t.Helper()
	out, err := ConvertClovaRequest(req)
	if err != nil {
		t.Fatalf("ConvertClovaRequest: %v", err)
	}
	return out
}

// TestConvertClovaRequestRoundTrip translates an OpenAI request to the
// CLOVA v3 shape, re-encodes it through JSON (verifying the wire tags
// produce a valid, re-decodable body), and maps a CLOVA response that
// answers that request back to OpenAI, asserting the assistant turn
// round-trips semantically.
func TestConvertClovaRequestRoundTrip(t *testing.T) {
	temp := 0.7
	topP := 0.9
	req := wire.ChatCompletionRequest{
		Model:       "HCX-005",
		Messages:    []wire.ChatMessage{{Role: "system", Content: mustJSON("be brief")}, {Role: "user", Content: mustJSON("hello world")}},
		Temperature: &temp,
		TopP:        &topP,
		MaxTokens:   1024,
		Stop:        []string{"END"},
	}
	out := clovaChatReq(t, req)

	// The wire shape must survive a JSON round trip.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal ClovaRequest: %v", err)
	}
	var decoded ClovaRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal ClovaRequest: %v\nbody: %s", err, raw)
	}
	if len(decoded.Messages) != 2 ||
		decoded.Messages[0].Role != "system" || decoded.Messages[1].Role != "user" {
		t.Fatalf("decoded messages = %s", raw)
	}
	var sys, user string
	if err := json.Unmarshal(decoded.Messages[0].Content, &sys); err != nil || sys != "be brief" {
		t.Errorf("system content = %s (err %v)", decoded.Messages[0].Content, err)
	}
	if err := json.Unmarshal(decoded.Messages[1].Content, &user); err != nil || user != "hello world" {
		t.Errorf("user content = %s (err %v)", decoded.Messages[1].Content, err)
	}
	if decoded.Temperature == nil || *decoded.Temperature != 0.7 || decoded.TopP == nil || *decoded.TopP != 0.9 {
		t.Errorf("temperature/topP = %v/%v", decoded.Temperature, decoded.TopP)
	}
	if decoded.MaxTokens != 1024 || len(decoded.Stop) != 1 || decoded.Stop[0] != "END" {
		t.Errorf("maxTokens/stop = %d/%v", decoded.MaxTokens, decoded.Stop)
	}
	// The model must not appear in the body (it is a path parameter).
	if strings.Contains(string(raw), `"HCX-005"`) {
		t.Errorf("model leaked into body: %s", raw)
	}

	// The provider answers; the response maps back to OpenAI.
	resp := &ClovaResponse{
		Status: ClovaStatus{Code: "20000", Message: "OK"},
		Result: ClovaResult{
			Created:      1791043155000,
			Usage:        ClovaUsage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13},
			Message:      ClovaResultMessage{Role: "assistant", Content: "hello!"},
			FinishReason: "stop",
		},
	}
	back, err := ConvertClovaResponse(resp)
	if err != nil {
		t.Fatalf("ConvertClovaResponse: %v", err)
	}
	if len(back.Choices) != 1 {
		t.Fatalf("choices = %d", len(back.Choices))
	}
	var reply string
	if err := json.Unmarshal(back.Choices[0].Message.Content, &reply); err != nil || reply != "hello!" {
		t.Errorf("reply = %s (err %v)", back.Choices[0].Message.Content, err)
	}
	if back.Choices[0].Message.Role != "assistant" || back.Choices[0].FinishReason != "stop" {
		t.Errorf("role/finish = %s/%s", back.Choices[0].Message.Role, back.Choices[0].FinishReason)
	}
	if back.Usage.PromptTokens != 9 || back.Usage.CompletionTokens != 4 || back.Usage.TotalTokens != 13 {
		t.Errorf("usage = %+v", back.Usage)
	}
	if back.Object != "chat.completion" {
		t.Errorf("object = %q", back.Object)
	}
	// The millisecond timestamp normalises to seconds.
	if back.Created != 1791043155 {
		t.Errorf("created = %d, want 1791043155", back.Created)
	}
}

// TestConvertClovaToolsRoundTrip covers the full tool loop: tool
// definitions, an assistant toolCalls turn, a tool result message, and
// a response that carries toolCalls back. CLOVA's toolCall arguments
// are an object; OpenAI's are a string — both directions must convert.
func TestConvertClovaToolsRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "HCX-005",
		Messages: []wire.ChatMessage{
			{Role: "user", Content: mustJSON("weather in Seoul?")},
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
		Tools: []wire.ToolDefinition{{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		}},
	}
	out := clovaChatReq(t, req)
	if len(out.Tools) != 1 || out.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	var last ClovaMessage
	last = out.Messages[len(out.Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call_1" {
		t.Fatalf("tool message = %+v", last)
	}
	assistant := out.Messages[len(out.Messages)-2]
	if len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant toolCalls = %+v", assistant.ToolCalls)
	}
	// The object form is required on this surface.
	var argsObj map[string]any
	if err := json.Unmarshal(assistant.ToolCalls[0].Function.Arguments, &argsObj); err != nil {
		t.Fatalf("toolCall arguments are not an object: %s (err %v)", assistant.ToolCalls[0].Function.Arguments, err)
	}
	if argsObj["city"] != "Seoul" {
		t.Errorf("toolCall arguments = %v", argsObj)
	}

	// The provider returns tool_calls; they map back to OpenAI strings.
	resp := &ClovaResponse{
		Status: ClovaStatus{Code: "20000"},
		Result: ClovaResult{
			Message: ClovaResultMessage{
				Role: "assistant",
				ToolCalls: []ClovaToolCall{{
					ID:   "call_9",
					Type: "function",
					Function: ClovaToolCallFunction{
						Name:      "get_weather",
						Arguments: json.RawMessage(`{"city":"Seoul"}`),
					},
				}},
			},
			FinishReason: "tool_calls",
		},
	}
	back, err := ConvertClovaResponse(resp)
	if err != nil {
		t.Fatalf("ConvertClovaResponse: %v", err)
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

// TestConvertClovaImageContent covers the typed content-array form for
// image input (URL and data URI variants) and the documented 5-image cap.
func TestConvertClovaImageContent(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "HCX-005",
		Messages: []wire.ChatMessage{{
			Role: "user",
			Content: mustJSONParts([]map[string]any{
				{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/pic.png"}},
				{"type": "text", "text": "describe"},
			}),
		}},
	}
	out := clovaChatReq(t, req)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %d", len(out.Messages))
	}
	var parts []ClovaContentPart
	if err := json.Unmarshal(out.Messages[0].Content, &parts); err != nil {
		t.Fatalf("content is not an array: %s (err %v)", out.Messages[0].Content, err)
	}
	if len(parts) != 2 || parts[0].Type != "image_url" || parts[0].ImageURL == nil ||
		parts[0].ImageURL.URL != "https://example.com/pic.png" || parts[1].Text != "describe" {
		t.Fatalf("parts = %+v", parts)
	}

	// data: URLs map onto the documented dataUri form.
	reqURI := wire.ChatCompletionRequest{
		Model: "HCX-005",
		Messages: []wire.ChatMessage{{
			Role: "user",
			Content: mustJSONParts([]map[string]any{
				{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,QUJD"}},
				{"type": "text", "text": "describe"},
			}),
		}},
	}
	outURI := clovaChatReq(t, reqURI)
	var uriParts []ClovaContentPart
	if err := json.Unmarshal(outURI.Messages[0].Content, &uriParts); err != nil {
		t.Fatalf("content is not an array: %s (err %v)", outURI.Messages[0].Content, err)
	}
	if uriParts[0].DataURI == nil || uriParts[0].DataURI.Data != "QUJD" || uriParts[0].ImageURL != nil {
		t.Fatalf("dataUri parts = %+v", uriParts[0])
	}

	// Six images exceed the documented cap of five per request.
	many := make([]map[string]any, 6)
	for i := range many {
		many[i] = map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/pic.png"}}
	}
	six := wire.ChatCompletionRequest{
		Model:    "HCX-005",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSONParts(many)}},
	}
	if _, err := ConvertClovaRequest(six); !IsUnrepresentable(err) {
		t.Errorf("6 images: err = %v, want unrepresentable", err)
	}
}

// TestConvertClovaRequestRefusals pins the honest refusals: more than
// one system message, out-of-range sampling, and tool_choice values the
// surface cannot represent.
func TestConvertClovaRequestRefusals(t *testing.T) {
	t.Run("two system messages", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:    "HCX-005",
			Messages: []wire.ChatMessage{{Role: "system", Content: mustJSON("a")}, {Role: "system", Content: mustJSON("b")}, {Role: "user", Content: mustJSON("hi")}},
		}
		if _, err := ConvertClovaRequest(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("temperature out of range", func(t *testing.T) {
		v := 1.5
		req := wire.ChatCompletionRequest{
			Model:       "HCX-005",
			Messages:    []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
			Temperature: &v,
		}
		if _, err := ConvertClovaRequest(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("tool_choice required", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:      "HCX-005",
			Messages:   []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
			Tools:      []wire.ToolDefinition{namedTool("f")},
			ToolChoice: "required",
		}
		if _, err := ConvertClovaRequest(req); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("tool_choice none", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:      "HCX-005",
			Messages:   []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
			Tools:      []wire.ToolDefinition{namedTool("f")},
			ToolChoice: "none",
		}
		out := clovaChatReq(t, req)
		if out.ToolChoice != "none" {
			t.Errorf("toolChoice = %v", out.ToolChoice)
		}
	})
	t.Run("named tool choice maps to object form", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:      "HCX-005",
			Messages:   []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
			Tools:      []wire.ToolDefinition{namedTool("f")},
			ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "f"}},
		}
		out := clovaChatReq(t, req)
		obj, ok := out.ToolChoice.(map[string]any)
		if !ok {
			t.Fatalf("toolChoice = %v", out.ToolChoice)
		}
		fn, _ := obj["function"].(map[string]any)
		if fn["name"] != "f" {
			t.Errorf("toolChoice = %v", obj)
		}
	})
}

func TestConvertClovaResponseErrorStatus(t *testing.T) {
	resp := &ClovaResponse{Status: ClovaStatus{Code: "400100", Message: "Invalid request"}}
	_, err := ConvertClovaResponse(resp)
	var ce *ClovaError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ClovaError", err)
	}
	if ce.Status.Code != "400100" {
		t.Errorf("code = %q", ce.Status.Code)
	}
}

// TestTranslateClovaStream feeds a documented CLOVA SSE sequence
// (token fragments, then the result event with the full text and
// usage) and asserts the emitted OpenAI chunk sequence reassembles.
func TestTranslateClovaStream(t *testing.T) {
	stream := "id: 1\r\n" +
		"event: token\r\n" +
		`data: {"message": {"role": "assistant", "content": "He"}, "finishReason": null, "created": 1744710905, "usage": null}` + "\r\n\r\n" +
		"event: token\r\n" +
		`data: {"message": {"role": "assistant", "content": "llo"}, "finishReason": null, "created": 1744710905, "usage": null}` + "\r\n\r\n" +
		"event: result\r\n" +
		`data: {"message": {"role": "assistant", "content": "Hello"}, "finishReason": "stop", "created": 1744710905, "usage": {"promptTokens": 20, "completionTokens": 5, "totalTokens": 25}}` + "\r\n\r\n"

	var chunks []wire.ChatChunk
	res, err := TranslateClovaStream(strings.NewReader(stream), "HCX-005", func(c wire.ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateClovaStream: %v", err)
	}
	if res.Content != "Hello" || res.FinishReason != "stop" {
		t.Fatalf("content/finish = %q/%q", res.Content, res.FinishReason)
	}
	if res.Model != "HCX-005" {
		t.Errorf("model = %q", res.Model)
	}
	if res.Usage.PromptTokens != 20 || res.Usage.CompletionTokens != 5 || res.Usage.TotalTokens != 25 {
		t.Errorf("usage = %+v", res.Usage)
	}
	// Two content deltas + one final chunk.
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content != "He" || chunks[1].Choices[0].Delta.Content != "llo" {
		t.Errorf("deltas = %q, %q", chunks[0].Choices[0].Delta.Content, chunks[1].Choices[0].Delta.Content)
	}
	// The result event must not be re-emitted as a delta.
	if chunks[1].Choices[0].Delta.Content == "Hello" {
		t.Errorf("result content was emitted as a delta")
	}
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v", final.Choices[0].FinishReason)
	}
	if final.Usage == nil || final.Usage.TotalTokens != 25 {
		t.Errorf("final usage = %v", final.Usage)
	}
	if final.Object != "chat.completion.chunk" {
		t.Errorf("final object = %q", final.Object)
	}
}

func TestTranslateClovaStreamEmpty(t *testing.T) {
	if _, err := TranslateClovaStream(strings.NewReader(""), "HCX-005", nil); err == nil {
		t.Errorf("empty stream returned no error")
	}
}

func TestTranslateClovaStreamErrorEvent(t *testing.T) {
	stream := "event: token\r\n" +
		`data: {"message": {"role": "assistant", "content": "hi"}, "finishReason": null}` + "\r\n\r\n" +
		"event: error\r\n" +
		`data: {"status": {"code": "400100", "message": "Invalid request"}}` + "\r\n\r\n"
	_, err := TranslateClovaStream(strings.NewReader(stream), "HCX-005", nil)
	var ce *ClovaError
	if !errors.As(err, &ce) || ce.Status.Code != "400100" {
		t.Fatalf("err = %v, want *ClovaError 400100", err)
	}
}

// --- small helpers shared by the pair tests ---

func mustJSONParts(parts []map[string]any) json.RawMessage {
	b, err := json.Marshal(parts)
	if err != nil {
		panic(err)
	}
	return b
}

func namedTool(name string) wire.ToolDefinition {
	return wire.ToolDefinition{
		Type: "function",
		Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)},
	}
}
