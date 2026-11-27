package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertAntigravityRequestRoundTrip translates an OpenAI request
// through the Antigravity envelope (reusing the Gemini wire for the
// inner body), re-encodes it through JSON, unwraps a Gemini response
// that answers the request, and maps it back to OpenAI, asserting the
// assistant turn round-trips semantically.
func TestConvertAntigravityRequestRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "gemini-3.1-pro-low",
		Messages: []wire.ChatMessage{
			{Role: "system", Content: mustJSON("be brief")},
			{Role: "user", Content: mustJSON("hello")},
		},
	}
	env, err := ConvertAntigravityRequest(req, "proj-1")
	if err != nil {
		t.Fatalf("ConvertAntigravityRequest: %v", err)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded AntigravityEnvelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if decoded.Project != "proj-1" || decoded.Model != "gemini-3.1-pro-low" {
		t.Errorf("project/model = %q/%q", decoded.Project, decoded.Model)
	}
	if decoded.RequestType != "agent" || decoded.UserAgent != "antigravity" {
		t.Errorf("requestType/userAgent = %q/%q", decoded.RequestType, decoded.UserAgent)
	}
	if !strings.HasPrefix(decoded.RequestID, "agent-") || len(decoded.RequestID) != len("agent-")+32 {
		t.Errorf("requestId = %q", decoded.RequestID)
	}
	if decoded.Request == nil {
		t.Fatalf("inner Gemini request is nil")
	}
	// The inner body is the native Gemini shape: system instruction
	// and user content translated by the reused wire logic.
	if decoded.Request.SystemInstruction == nil || len(decoded.Request.SystemInstruction.Parts) != 1 ||
		decoded.Request.SystemInstruction.Parts[0].Text != "be brief" {
		t.Errorf("systemInstruction = %+v", decoded.Request.SystemInstruction)
	}
	if len(decoded.Request.Contents) != 1 || decoded.Request.Contents[0].Role != "user" ||
		decoded.Request.Contents[0].Parts[0].Text != "hello" {
		t.Errorf("contents = %+v", decoded.Request.Contents)
	}

	// The provider answers with a wrapped Gemini response.
	resp := &AntigravityResponse{Response: &wire.GeminiResponse{
		ModelVersion: "gemini-3.1-pro-low",
		Candidates: []wire.GeminiCandidate{{
			Content:      &wire.GeminiContent{Role: "model", Parts: []wire.GeminiPart{{Text: "hello!"}}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &wire.GeminiUsageMetadata{
			PromptTokenCount: 5, CandidatesTokenCount: 2, TotalTokenCount: 7,
		},
	}}
	back, err := ConvertAntigravityResponse(resp)
	if err != nil {
		t.Fatalf("ConvertAntigravityResponse: %v", err)
	}
	var reply string
	if err := json.Unmarshal(back.Choices[0].Message.Content, &reply); err != nil || reply != "hello!" {
		t.Errorf("reply = %s (err %v)", back.Choices[0].Message.Content, err)
	}
	if back.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", back.Choices[0].FinishReason)
	}
	if back.Usage.PromptTokens != 5 || back.Usage.CompletionTokens != 2 || back.Usage.TotalTokens != 7 {
		t.Errorf("usage = %+v", back.Usage)
	}
	if back.Object != "chat.completion" {
		t.Errorf("object = %q", back.Object)
	}
}

func TestConvertAntigravityRequestRefusals(t *testing.T) {
	// Empty model.
	if _, err := ConvertAntigravityRequest(wire.ChatCompletionRequest{
		Model:    "",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
	}, "p"); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
	// The inner conversion refuses what the Gemini wire refuses
	// (e.g. more than five stop sequences).
	req := wire.ChatCompletionRequest{
		Model:    "gemini-3.1-pro-low",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
		Stop:     []string{"a", "b", "c", "d", "e", "f"},
	}
	if _, err := ConvertAntigravityRequest(req, "p"); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
	// An envelope without an inner response.
	if _, err := ConvertAntigravityResponse(&AntigravityResponse{}); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
}

// TestTranslateAntigravityStream feeds wrapped Gemini SSE payloads and
// asserts the delegated stream translation reassembles, including a
// bare (unwrapped) payload tolerated mid-stream.
func TestTranslateAntigravityStream(t *testing.T) {
	stream := "data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"Hel\"}]}}]}}\n\n" +
		"data: {\"response\": {\"candidates\": [{\"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"lo!\"}]}}]}}\n\n" +
		"data: {\"candidates\": [{\"finishReason\": \"STOP\", \"content\": {\"role\": \"model\", \"parts\": [{\"text\": \"\"}]}}], " +
		"\"usageMetadata\": {\"promptTokenCount\": 4, \"candidatesTokenCount\": 4, \"totalTokenCount\": 8}}\n\n"

	var chunks []wire.ChatChunk
	res, err := TranslateAntigravityStream(strings.NewReader(stream), func(c wire.ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateAntigravityStream: %v", err)
	}
	if res.Content != "Hello!" || res.FinishReason != "stop" {
		t.Fatalf("content/finish = %q/%q", res.Content, res.FinishReason)
	}
	if res.Usage.PromptTokens != 4 || res.Usage.CompletionTokens != 4 || res.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content != "Hel" || chunks[1].Choices[0].Delta.Content != "lo!" {
		t.Errorf("deltas = %q, %q", chunks[0].Choices[0].Delta.Content, chunks[1].Choices[0].Delta.Content)
	}
}

func TestTranslateAntigravityStreamEmpty(t *testing.T) {
	if _, err := TranslateAntigravityStream(strings.NewReader(""), nil); err == nil {
		t.Errorf("empty stream returned no error")
	}
}

func TestTranslateAntigravityStreamError(t *testing.T) {
	// A Gemini-shaped error envelope passes through to the wire's
	// Gemini stream error mapping.
	stream := "data: {\"response\": {\"error\": {\"code\": 429, \"message\": \"Resource exhausted\", \"status\": \"RESOURCE_EXHAUSTED\"}}}\n\n"
	_, err := TranslateAntigravityStream(strings.NewReader(stream), nil)
	if err == nil {
		t.Fatalf("error envelope returned no error")
	}
	var ge *wire.GeminiError
	if !asGeminiError(err, &ge) || ge.StatusCode != 429 {
		t.Fatalf("err = %v, want *wire.GeminiError 429", err)
	}
}

func asGeminiError(err error, target **wire.GeminiError) bool {
	for err != nil {
		if ge, ok := err.(*wire.GeminiError); ok {
			*target = ge
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
