package provider

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// M6.9c: the prompt-cache marker must reach the Anthropic wire through
// the REAL client path (anthropicClient.Chat / ChatStreamChunks), not
// through a unit test of cache.Prepare alone. These tests drive the
// mock upstream and assert on the outbound JSON the client actually
// sends: cache_control {"type": "ephemeral"} on the LAST user
// message's last text block only — and absent everywhere the M6.5
// gate refuses it.

// mustM69cAnthropicClient builds the standard mock Anthropic client
// and stamps the registry provider ID the production path would set
// (registry.clientForKey -> attachHTTPAdapter -> setHTTPAdapter).
// providerID "" is the ad-hoc client: it must NOT be annotated.
func mustM69cAnthropicClient(t *testing.T, srv *httptest.Server, providerID string) Client {
	t.Helper()
	c := mustAnthropicClient(t, srv)
	ac, ok := c.(*anthropicClient)
	if !ok {
		t.Fatalf("mustAnthropicClient did not return *anthropicClient, got %T", c)
	}
	ac.providerID = providerID
	return c
}

// m69cLongPrompt is ~4500 chars -> ~1125 estimated tokens, above the
// 1024-token floor. m69cShortPrompt is far below it.
const m69cLongPrompt = "The quick brown fox jumps over the lazy dog. " +
	"The quick brown fox jumps over the lazy dog. " +
	"The quick brown fox jumps over the lazy dog. " +
	"The quick brown fox jumps over the lazy dog. " +
	"The quick brown fox jumps over the lazy dog. "

func m69cLongText() string { return strings.Repeat(m69cLongPrompt, 20) } // 4500 chars

// m69cOutbound decodes the mock's last request body as the Anthropic
// wire shape and counts cache_control markers per message.
func m69cOutbound(t *testing.T, mock *anthropicMock) wire.AnthropicRequest {
	t.Helper()
	var body wire.AnthropicRequest
	if err := json.Unmarshal(mock.lastBody, &body); err != nil {
		t.Fatalf("outbound body is not an Anthropic request: %v\nbody: %s", err, mock.lastBody)
	}
	return body
}

// m69cMarkerCount counts content blocks carrying cache_control across
// all messages, and reports which (message index, block index) hold
// them.
func m69cMarkerCount(body wire.AnthropicRequest) (int, [][2]int) {
	n := 0
	var at [][2]int
	for i, m := range body.Messages {
		for j, b := range m.Content {
			if b.CacheControl != nil {
				n++
				at = append(at, [2]int{i, j})
			}
		}
	}
	return n, at
}

// m69cMarkerCountAll counts markers everywhere they can sit:
// message blocks (m69cMarkerCount) plus the system prompt blocks
// and the tool definitions (G13 system/tools marking).
func m69cMarkerCountAll(body wire.AnthropicRequest) int {
	n, _ := m69cMarkerCount(body)
	for _, b := range body.SystemBlocks {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, t := range body.Tools {
		if t.CacheControl != nil {
			n++
		}
	}
	return n
}

func TestM69cChatMarksLastUserMessageOnly(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "system", Content: "Be helpful."},
			{Role: "user", Content: m69cLongText()},
			{Role: "assistant", Content: "Sure, here is the summary."},
			{Role: "user", Content: m69cLongText()},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := m69cOutbound(t, mock)
	n, at := m69cMarkerCount(body)
	if n != 1 {
		t.Fatalf("cache_control markers = %d at %v, want exactly 1\nbody: %s", n, at, mock.lastBody)
	}
	// The single marker must be on the LAST user message (index 2:
	// system is extracted, so messages are user/assistant/user),
	// on its last (only) text block, with type ephemeral.
	if at[0][0] != 2 || at[0][1] != 0 {
		t.Errorf("marker at message %d block %d, want message 2 block 0", at[0][0], at[0][1])
	}
	blk := body.Messages[2].Content[0]
	if blk.CacheControl.Type != "ephemeral" {
		t.Errorf("marker type = %q, want \"ephemeral\"", blk.CacheControl.Type)
	}
	if blk.CacheControl.TTL != "" {
		t.Errorf("marker ttl = %q, want empty (provider-default 5m tier)", blk.CacheControl.TTL)
	}
	// No marker on the first user message or the assistant turn.
	if n2, _ := m69cMarkerCount(wire.AnthropicRequest{Messages: body.Messages[:2]}); n2 != 0 {
		t.Errorf("first two messages carry %d markers, want 0", n2)
	}
	// G13: the system prompt ("Be helpful.") is marked too — the
	// request carries 2 markers in total (system + last user),
	// so the raw body has 2 cache_control keys.
	if all := m69cMarkerCountAll(body); all != 2 {
		t.Errorf("total markers = %d, want 2 (system + last user)", all)
	}
	if len(body.SystemBlocks) != 1 || body.SystemBlocks[0].CacheControl == nil {
		t.Errorf("system prompt not marked: SystemBlocks = %+v", body.SystemBlocks)
	}
	if got := strings.Count(string(mock.lastBody), `"cache_control"`); got != 2 {
		t.Errorf("raw body has %d cache_control keys, want 2", got)
	}
}

func TestM69cChatStreamMarksToo(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	cs, ok := mustM69cAnthropicClient(t, srv, "anthropic").(ChunkStreamer)
	if !ok {
		t.Fatal("anthropic client must implement ChunkStreamer")
	}
	_, err := cs.ChatStreamChunks(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: m69cLongText()}},
	}, func(StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	body := m69cOutbound(t, mock)
	if n, _ := m69cMarkerCount(body); n != 1 {
		t.Fatalf("streaming request markers = %d, want 1\nbody: %s", n, mock.lastBody)
	}
	if body.Messages[0].Content[0].CacheControl.Type != "ephemeral" {
		t.Errorf("streaming marker type = %q, want \"ephemeral\"", body.Messages[0].Content[0].CacheControl.Type)
	}
}

func TestM69cClaudeProviderIDMarked(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "claude").Chat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: m69cLongText()}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if n, _ := m69cMarkerCount(m69cOutbound(t, mock)); n != 1 {
		t.Fatalf("provider claude: markers = %d, want 1", n)
	}
}

func TestM69cShortPromptUnmarked(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
		t.Errorf("short prompt must not be marked, body: %s", got)
	}
}

func TestM69cToolsMarked(t *testing.T) {
	// G13: tool DEFINITIONS no longer disqualify the request —
	// the last tool definition is marked alongside the last user
	// message (2 markers; the system prompt is absent here, so
	// no third). Tool definitions are part of the cached prefix
	// on the Anthropic API, so a long tool list is cached like
	// the rest of the prompt. (In-flight tool CALLS still
	// disqualify — see TestM69cToolCallsUnmarked.)
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{{Role: "user", Content: m69cLongText()}},
		Tools: []ToolDefinition{{
			Type: "function",
			Function: struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			}{Name: "get_weather", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := m69cOutbound(t, mock)
	if all := m69cMarkerCountAll(body); all != 2 {
		t.Fatalf("total markers = %d, want 2 (last user + last tool)\nbody: %s", all, mock.lastBody)
	}
	if len(body.Tools) != 1 || body.Tools[0].CacheControl == nil || body.Tools[0].CacheControl.Type != "ephemeral" {
		t.Errorf("last tool not marked: Tools = %+v", body.Tools)
	}
	if n, _ := m69cMarkerCount(body); n != 1 {
		t.Errorf("message markers = %d, want 1 (last user)", n)
	}
}

func TestM69cToolCallsUnmarked(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "user", Content: m69cLongText()},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID:   "toolu_1",
				Type: "function",
				Function: ToolCallFunction{
					Name:      "get_weather",
					Arguments: `{"city":"Pune"}`,
				},
			}}},
			{Role: "user", Content: m69cLongText()},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
		t.Errorf("request with assistant tool calls must not be marked, body: %s", got)
	}
}

func TestM69cNonZeroTemperatureUnmarked(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model:       "claude-sonnet-4-5",
		Temperature: 0.7,
		Messages:    []Message{{Role: "user", Content: m69cLongText()}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
		t.Errorf("temperature 0.7 request must not be marked, body: %s", got)
	}
}

func TestM69cNonMarkerProvidersUnmarked(t *testing.T) {
	// OpenRouter is deliberately unwired (see promptCacheMarkerProviders):
	// its default surface is the OpenAI wire, which has no
	// cache_control path, and its Anthropic-endpoint variant is not
	// proven to forward the marker for arbitrary models. DeepSeek's
	// Anthropic surface and ad-hoc ("") clients are out for the same
	// fail-closed reason.
	for _, providerID := range []string{"", "openrouter", "deepseek", "fireworks"} {
		t.Run("provider="+providerID, func(t *testing.T) {
			srv, mock := newAnthropicMock(t)
			defer srv.Close()

			_, err := mustM69cAnthropicClient(t, srv, providerID).Chat(context.Background(), ChatRequest{
				Model:    "claude-sonnet-4-5",
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

func TestM69cLengthGateBoundary(t *testing.T) {
	// 4096 chars / 4 = 1024 estimated tokens: exactly at the floor,
	// marked. 4092 chars / 4 = 1023: below the floor, unmarked.
	for _, tc := range []struct {
		chars int
		want  bool
	}{
		{chars: 4096, want: true},
		{chars: 4092, want: false},
	} {
		srv, mock := newAnthropicMock(t)
		defer srv.Close()

		_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
			Model:    "claude-sonnet-4-5",
			Messages: []Message{{Role: "user", Content: strings.Repeat("a", tc.chars)}},
		})
		if err != nil {
			t.Fatalf("Chat: %v", err)
		}
		got := strings.Contains(string(mock.lastBody), "cache_control")
		if got != tc.want {
			t.Errorf("%d chars: marked = %v, want %v", tc.chars, got, tc.want)
		}
	}
}

func TestM69cImageOnlyLastUserUnmarked(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{{
			Role: "user",
			Parts: []ContentPart{{
				Type:     "image_url",
				ImageURL: &wire.ImageURLPart{URL: "https://example.com/x.png"},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	// No text block to mark (and no long text to pass the length
	// gate either): the request must carry no marker.
	if got := string(mock.lastBody); strings.Contains(got, "cache_control") {
		t.Errorf("image-only user message must not be marked, body: %s", got)
	}
}

func TestM69cEstimatedPromptTokens(t *testing.T) {
	// Unit-check the estimator itself: Content + text Parts count,
	// 4 chars per token, image parts do not count.
	got := estimatedPromptTokens(ChatRequest{Messages: []Message{
		{Role: "system", Content: strings.Repeat("s", 400)},
		{Role: "user", Content: strings.Repeat("u", 3600)},
		{Role: "user", Parts: []ContentPart{
			{Type: "text", Text: strings.Repeat("p", 96)},
			{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: "https://example.com/x.png"}},
		}},
	}})
	// (400 + 3600 + 96) / 4 = 1024.
	if got != 1024 {
		t.Errorf("estimatedPromptTokens = %d, want 1024", got)
	}
	// M6.9c review: a message decoded from array-form JSON carries
	// BOTH Content (concatenated text parts) and Parts — the same
	// text must be counted once, not twice.
	got = estimatedPromptTokens(ChatRequest{Messages: []Message{
		{Role: "user",
			Content: strings.Repeat("u", 400),
			Parts:   []ContentPart{{Type: "text", Text: strings.Repeat("u", 400)}},
		},
	}})
	// 400 / 4 = 100, not 200.
	if got != 100 {
		t.Errorf("estimatedPromptTokens (Content+Parts) = %d, want 100 (count once)", got)
	}
}

func TestM69cClientMarkerPreserved(t *testing.T) {
	// G13: a client-supplied cache_control survives decode AND
	// conversion AND the auto-marker — verbatim, including its
	// 1h TTL — and the auto-marker still fills the slots the
	// client did not mark (last user message). Total markers: 3
	// (system 1h + first user + last user auto).
	srv, mock := newAnthropicMock(t)
	defer srv.Close()

	_, err := mustM69cAnthropicClient(t, srv, "anthropic").Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "system", Parts: []ContentPart{{
				Type: "text", Text: strings.Repeat("s", 200),
				CacheControl: &wire.AnthropicCacheControl{Type: "ephemeral", TTL: "1h"},
			}}},
			{Role: "user", Parts: []ContentPart{{
				Type: "text", Text: "first turn",
				CacheControl: &wire.AnthropicCacheControl{Type: "ephemeral"},
			}}},
			{Role: "user", Content: m69cLongText()},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := m69cOutbound(t, mock)
	if all := m69cMarkerCountAll(body); all != 3 {
		t.Fatalf("total markers = %d, want 3 (system + first user + last user)\nbody: %s", all, mock.lastBody)
	}
	// System marker keeps the client's 1h TTL — not overwritten
	// with the provider-default ephemeral.
	if len(body.SystemBlocks) != 1 || body.SystemBlocks[0].CacheControl == nil || body.SystemBlocks[0].CacheControl.TTL != "1h" {
		t.Errorf("system marker = %+v, want ephemeral with ttl 1h", body.SystemBlocks)
	}
	// First user message keeps its client marker.
	if len(body.Messages) < 1 || len(body.Messages[0].Content) < 1 || body.Messages[0].Content[0].CacheControl == nil {
		t.Errorf("first user message lost its client marker: %+v", body.Messages)
	}
	// Last user message got the auto marker (ephemeral, no TTL).
	last := body.Messages[len(body.Messages)-1]
	if len(last.Content) < 1 || last.Content[0].CacheControl == nil || last.Content[0].CacheControl.Type != "ephemeral" || last.Content[0].CacheControl.TTL != "" {
		t.Errorf("last user marker = %+v, want ephemeral with empty ttl", last.Content)
	}
}

func TestM69cClientCacheControlSurvivesDecode(t *testing.T) {
	// G13: provider.Message decode preserves a client-supplied
	// cache_control on a content part (it used to be dropped:
	// ContentPart had no CacheControl field), and re-encoding
	// the message keeps it — so the marker reaches the wire
	// clients instead of dying at the gateway door.
	var req ChatRequest
	in := `{"model":"anthropic/claude-sonnet-4.5","messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		t.Fatalf("Unmarshal ChatRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Parts) != 1 {
		t.Fatalf("decoded parts = %+v, want 1 part", req.Messages)
	}
	cc := req.Messages[0].Parts[0].CacheControl
	if cc == nil || cc.Type != "ephemeral" || cc.TTL != "1h" {
		t.Fatalf("decoded CacheControl = %+v, want {ephemeral 1h}", cc)
	}
	out, err := json.Marshal(req.Messages[0])
	if err != nil {
		t.Fatalf("Marshal Message: %v", err)
	}
	if !strings.Contains(string(out), `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Errorf("re-encoded message lost the marker: %s", out)
	}
	// A client-supplied marker on a tool definition survives
	// decode too.
	var req2 ChatRequest
	in2 := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"},"cache_control":{"type":"ephemeral"}}]}`
	if err := json.Unmarshal([]byte(in2), &req2); err != nil {
		t.Fatalf("Unmarshal ChatRequest tools: %v", err)
	}
	if len(req2.Tools) != 1 || req2.Tools[0].CacheControl == nil || req2.Tools[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("decoded tool CacheControl = %+v, want ephemeral", req2.Tools)
	}
}
