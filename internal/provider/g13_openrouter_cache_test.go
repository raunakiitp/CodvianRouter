// G13 — OpenRouter prompt-cache markers via the OpenAI wire
// (parity tracker Area 6 row "Provider prompt-cache markers").
//
// Mock-only verification: no live upstream verification exists
// in this repo for any prompt-cache marker (M6.9c). These tests
// drive the mock upstream and assert on the outbound JSON the
// OpenAI client actually sends: cache_control
// {"type": "ephemeral"} on the last user message's last text
// part, the last system message, and the last tool definition —
// and absent everywhere the gate refuses it.
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// g13OpenRouterMock serves the OpenAI chat-completions API and
// records the last request body.
type g13OpenRouterMock struct {
	lastBody []byte
	calls    int
}

func (m *g13OpenRouterMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls++
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","model":"anthropic/claude-sonnet-4-5","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}
}

// mustG13OpenRouterClient builds an OpenAI-wire client stamped
// with the registry provider ID the production path would set
// (registry.clientForKey -> attachHTTPAdapter -> setHTTPAdapter).
func mustG13OpenRouterClient(t *testing.T, srv *httptest.Server, providerID string) Client {
	t.Helper()
	c := mustClient(t, srv)
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("mustClient did not return *openAIClient, got %T", c)
	}
	oc.providerID = providerID
	return c
}

// g13OpenRouterOutbound decodes the mock's last request body as
// the OpenAI wire shape.
func g13OpenRouterOutbound(t *testing.T, mock *g13OpenRouterMock) chatCompletionRequest {
	t.Helper()
	var body chatCompletionRequest
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("outbound body is not an OpenAI request: %v\nbody: %s", err, mock.lastBody)
	}
	return body
}

// g13OpenRouterMarkerCount counts cache_control markers on message
// content parts and tool definitions of an OpenAI wire request.
func g13OpenRouterMarkerCount(body chatCompletionRequest) int {
	n := 0
	for _, m := range body.Messages {
		var parts []ContentPart
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if p.CacheControl != nil {
					n++
				}
			}
		}
	}
	for _, tl := range body.Tools {
		if tl.CacheControl != nil {
			n++
		}
	}
	return n
}

func g13ToolDef(name string) ToolDefinition {
	return ToolDefinition{
		Type: "function",
		Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: name, Parameters: json.RawMessage(`{"type":"object"}`)},
	}
}

func TestG13OpenRouterClaudeMarked(t *testing.T) {
	mock := &g13OpenRouterMock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), ChatRequest{
		Model: "anthropic/claude-sonnet-4.5",
		Messages: []Message{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: m69cLongText()},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := g13OpenRouterOutbound(t, mock)
	if n := g13OpenRouterMarkerCount(body); n != 2 {
		t.Fatalf("markers = %d, want 2 (last user + system)\nbody: %s", n, mock.lastBody)
	}
	// The user message travelled string-form in and part-form out
	// (the marker needs a part to sit on); the marker is
	// ephemeral with no TTL (provider-default 5m tier).
	var userParts []ContentPart
	if err := json.Unmarshal(body.Messages[1].Content, &userParts); err != nil {
		t.Fatalf("user content is not a part array: %v", err)
	}
	if len(userParts) != 1 || userParts[0].CacheControl == nil || userParts[0].CacheControl.Type != "ephemeral" || userParts[0].CacheControl.TTL != "" {
		t.Errorf("user marker = %+v, want ephemeral with empty ttl", userParts)
	}
}

func TestG13OpenRouterToolsMarked(t *testing.T) {
	mock := &g13OpenRouterMock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), ChatRequest{
		Model:    "anthropic/claude-sonnet-4.5",
		Messages: []Message{{Role: "user", Content: m69cLongText()}},
		Tools:    []ToolDefinition{g13ToolDef("get_weather")},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := g13OpenRouterOutbound(t, mock)
	if n := g13OpenRouterMarkerCount(body); n != 2 {
		t.Fatalf("markers = %d, want 2 (last user + last tool)\nbody: %s", n, mock.lastBody)
	}
	if len(body.Tools) != 1 || body.Tools[0].CacheControl == nil || body.Tools[0].CacheControl.Type != "ephemeral" {
		t.Errorf("last tool not marked: Tools = %+v", body.Tools)
	}
}

func TestG13OpenRouterCacheableFamilies(t *testing.T) {
	// Only the families OpenRouter documents as honouring
	// explicit breakpoints are marked: Claude, Qwen, Gemini.
	for _, model := range []string{
		"anthropic/claude-sonnet-4.5",
		"qwen/qwen3-235b-a22b",
		"google/gemini-2.5-flash",
	} {
		t.Run(model, func(t *testing.T) {
			mock := &g13OpenRouterMock{}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), ChatRequest{
				Model:    model,
				Messages: []Message{{Role: "user", Content: m69cLongText()}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if n := g13OpenRouterMarkerCount(g13OpenRouterOutbound(t, mock)); n != 1 {
				t.Errorf("model %q: markers = %d, want 1", model, n)
			}
		})
	}
}

func TestG13OpenRouterNonCacheableModelUnmarked(t *testing.T) {
	// OpenAI-family models cache implicitly on OpenRouter and do
	// not take explicit breakpoints — annotating them would be a
	// fake promise, so they stay unmarked. Same for an unknown
	// family.
	for _, model := range []string{"openai/gpt-5", "meta-llama/llama-4-maverick", "deepseek/deepseek-chat-v3"} {
		t.Run(model, func(t *testing.T) {
			mock := &g13OpenRouterMock{}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), ChatRequest{
				Model:    model,
				Messages: []Message{{Role: "user", Content: m69cLongText()}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
				t.Errorf("model %q must not be marked, body: %s", model, got)
			}
		})
	}
}

func TestG13OpenRouterNonOpenRouterProviderUnmarked(t *testing.T) {
	// The marker is gated to providerID "openrouter": any other
	// OpenAI-wire provider (or an ad-hoc client with "") must
	// never be annotated, even for a Claude model.
	for _, providerID := range []string{"", "openai", "groq"} {
		t.Run("provider="+providerID, func(t *testing.T) {
			mock := &g13OpenRouterMock{}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			_, err := mustG13OpenRouterClient(t, srv, providerID).Chat(context.Background(), ChatRequest{
				Model:    "anthropic/claude-sonnet-4.5",
				Messages: []Message{{Role: "user", Content: m69cLongText()}},
			})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
				t.Errorf("provider %q must not be marked, body: %s", providerID, got)
			}
		})
	}
}

func TestG13OpenRouterClientMarkerPreserved(t *testing.T) {
	// A client-supplied cache_control (here with ttl 1h) on a
	// user part survives to the OpenRouter wire verbatim — the
	// auto-marker must not overwrite it — and the system message
	// still gets its own auto marker.
	mock := &g13OpenRouterMock{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), ChatRequest{
		Model: "anthropic/claude-sonnet-4.5",
		Messages: []Message{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Parts: []ContentPart{{
				Type: "text", Text: m69cLongText(),
				CacheControl: &wire.AnthropicCacheControl{Type: "ephemeral", TTL: "1h"},
			}}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := g13OpenRouterOutbound(t, mock)
	if n := g13OpenRouterMarkerCount(body); n != 2 {
		t.Fatalf("markers = %d, want 2 (client user marker + system auto marker)\nbody: %s", n, mock.lastBody)
	}
	var userParts []ContentPart
	if err := json.Unmarshal(body.Messages[1].Content, &userParts); err != nil {
		t.Fatalf("user content is not a part array: %v", err)
	}
	if len(userParts) != 1 || userParts[0].CacheControl == nil || userParts[0].CacheControl.TTL != "1h" {
		t.Errorf("user marker = %+v, want the client's ephemeral/1h preserved", userParts)
	}
}

func TestG13OpenRouterShortPromptAndTempUnmarked(t *testing.T) {
	// The length and temperature gates apply on the OpenRouter
	// path exactly as on the Anthropic path.
	for _, tc := range []struct {
		name string
		req  ChatRequest
	}{
		{"short", ChatRequest{Model: "anthropic/claude-sonnet-4.5", Messages: []Message{{Role: "user", Content: "Hi"}}}},
		{"temp", ChatRequest{Model: "anthropic/claude-sonnet-4.5", Temperature: 0.7, Messages: []Message{{Role: "user", Content: m69cLongText()}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := &g13OpenRouterMock{}
			srv := httptest.NewServer(mock.handler())
			defer srv.Close()
			_, err := mustG13OpenRouterClient(t, srv, "openrouter").Chat(context.Background(), tc.req)
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
				t.Errorf("%s request must not be marked, body: %s", tc.name, got)
			}
		})
	}
}
