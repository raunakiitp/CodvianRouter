// Intent-tier effort forwarding on the Anthropic/Gemini wires
// (CHECKLIST 285-286): the operator-declared thinking budget
// resolved by the engine rides ChatRequest.ThinkingBudget onto
// the wire as thinking.budget_tokens (Anthropic) or
// thinkingConfig.thinkingBudget (Gemini); a zero budget sends no
// thinking field at all.
package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAnthropicThinkingBudgetForwarded: a resolved budget of 1024
// is sent as {"type":"enabled","budget_tokens":1024} on both the
// non-streaming and streaming paths.
func TestAnthropicThinkingBudgetForwarded(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	c := mustAnthropicClient(t, srv)
	req := ChatRequest{
		Model:          "claude-sonnet-4-5",
		Messages:       []Message{{Role: "user", Content: "hi"}},
		ThinkingBudget: 1024,
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var body struct {
		Thinking *struct {
			Type         string `json:"type"`
			BudgetTokens int    `json:"budget_tokens"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode anthropic body: %v", err)
	}
	if body.Thinking == nil || body.Thinking.Type != "enabled" || body.Thinking.BudgetTokens != 1024 {
		t.Fatalf("thinking = %+v, want {enabled 1024} (body %s)", body.Thinking, mock.lastBody)
	}
	if _, err := c.(ChunkStreamer).ChatStreamChunks(context.Background(), req, func(StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode anthropic stream body: %v", err)
	}
	if body.Thinking == nil || body.Thinking.BudgetTokens != 1024 {
		t.Fatalf("stream thinking = %+v, want budget 1024 (body %s)", body.Thinking, mock.lastBody)
	}
}

// TestAnthropicNoThinkingBudgetSendsNothing: with no resolved
// budget the request carries no "thinking" key at all — the
// provider default applies, and no budget is invented.
func TestAnthropicNoThinkingBudgetSendsNothing(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	if _, err := mustAnthropicClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(string(mock.lastBody), `"thinking"`) {
		t.Fatalf("body carries a thinking field with no budget: %s", mock.lastBody)
	}
}

// TestGeminiThinkingBudgetForwarded: a resolved budget of 2048 is
// sent as generationConfig.thinkingConfig.thinkingBudget on both
// the non-streaming and streaming paths — including when no
// sampling parameter is set, so the generationConfig block has to
// be created for the thinking budget alone.
func TestGeminiThinkingBudgetForwarded(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	c := mustGeminiClient(t, srv)
	req := ChatRequest{
		Model:          "gemini-2.0-flash",
		Messages:       []Message{{Role: "user", Content: "hi"}},
		ThinkingBudget: 2048,
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var body struct {
		GenerationConfig *struct {
			ThinkingConfig *struct {
				ThinkingBudget int `json:"thinkingBudget"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode gemini body: %v", err)
	}
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil ||
		body.GenerationConfig.ThinkingConfig.ThinkingBudget != 2048 {
		t.Fatalf("thinkingBudget = %+v, want 2048 (body %s)", body.GenerationConfig, mock.lastBody)
	}
	if _, err := c.(ChunkStreamer).ChatStreamChunks(context.Background(), req, func(StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode gemini stream body: %v", err)
	}
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil ||
		body.GenerationConfig.ThinkingConfig.ThinkingBudget != 2048 {
		t.Fatalf("stream thinkingBudget = %+v, want 2048 (body %s)", body.GenerationConfig, mock.lastBody)
	}
}

// TestGeminiNoThinkingBudgetSendsNothing: with no resolved budget
// the request carries no thinkingConfig at all.
func TestGeminiNoThinkingBudgetSendsNothing(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	if _, err := mustGeminiClient(t, srv).Chat(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(string(mock.lastBody), "thinkingConfig") {
		t.Fatalf("body carries a thinkingConfig with no budget: %s", mock.lastBody)
	}
}

// TestVertexThinkingBudgetForwarded (CHECKLIST 303-304): a
// resolved budget of 1024 is sent as
// generationConfig.thinkingConfig.thinkingBudget on both the
// non-streaming and streaming paths — the same field as Gemini,
// on the Vertex :generateContent / :streamGenerateContent URLs,
// behind the minted OAuth2 token.
func TestVertexThinkingBudgetForwarded(t *testing.T) {
	_, mock, _, client := vertexTestServers(t)
	req := ChatRequest{
		Model:          "gemini-2.0-flash",
		Messages:       []Message{{Role: "user", Content: "hi"}},
		ThinkingBudget: 1024,
	}
	if _, err := client.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !strings.HasSuffix(mock.lastPath, ":generateContent") {
		t.Fatalf("lastPath = %q, want :generateContent", mock.lastPath)
	}
	var body struct {
		GenerationConfig *struct {
			ThinkingConfig *struct {
				ThinkingBudget int `json:"thinkingBudget"`
			} `json:"thinkingConfig"`
		} `json:"generationConfig"`
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode vertex body: %v", err)
	}
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil ||
		body.GenerationConfig.ThinkingConfig.ThinkingBudget != 1024 {
		t.Fatalf("thinkingBudget = %+v, want 1024 (body %s)", body.GenerationConfig, mock.lastBody)
	}
	if _, err := client.ChatStreamChunks(context.Background(), req, func(StreamChunk) error { return nil }); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if !strings.HasSuffix(mock.lastPath, ":streamGenerateContent") {
		t.Fatalf("stream lastPath = %q, want :streamGenerateContent", mock.lastPath)
	}
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("decode vertex stream body: %v", err)
	}
	if body.GenerationConfig == nil || body.GenerationConfig.ThinkingConfig == nil ||
		body.GenerationConfig.ThinkingConfig.ThinkingBudget != 1024 {
		t.Fatalf("stream thinkingBudget = %+v, want 1024 (body %s)", body.GenerationConfig, mock.lastBody)
	}
}

// TestVertexNoThinkingBudgetSendsNothing (CHECKLIST 303-304):
// with no resolved budget the Vertex request carries no
// thinkingConfig at all — the provider default applies, and no
// budget is invented.
func TestVertexNoThinkingBudgetSendsNothing(t *testing.T) {
	_, mock, _, client := vertexTestServers(t)
	if _, err := client.Chat(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(string(mock.lastBody), "thinkingConfig") {
		t.Fatalf("body carries a thinkingConfig with no budget: %s", mock.lastBody)
	}
}
