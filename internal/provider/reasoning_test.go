package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// --- Message decode/encode: reasoning_content in, never out ---

func TestMessageReasoningContentDecodesNeverEncodes(t *testing.T) {
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"Sunny.","reasoning_content":"because the sun is out"}`), &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if m.Content != "Sunny." {
		t.Fatalf("Content = %q", m.Content)
	}
	if m.Reasoning == nil || len(m.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 thought block", m.Reasoning)
	}
	b := m.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindThought || b.Text != "because the sun is out" {
		t.Fatalf("block = %+v", b)
	}
	if m.Reasoning.SourceWire != reasoning.WireOpenAI {
		t.Fatalf("SourceWire = %q", m.Reasoning.SourceWire)
	}
	// Re-encoding must NOT carry reasoning_content back out:
	// reasoning state is router-internal, never client-facing.
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "reasoning_content") || strings.Contains(string(out), "because the sun is out") {
		t.Fatalf("reasoning leaked into the encoded message: %s", out)
	}
}

// --- OpenAI wire: reasoning_content sent for replay models only ---

func reasoningOpenAIMock(t *testing.T, body string) (*httptest.Server, *[]byte) {
	t.Helper()
	var last *[]byte
	last = new([]byte)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*last = b
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	return srv, last
}

func TestOpenAIClientSendsReasoningContentForReplayModel(t *testing.T) {
	srv, last := reasoningOpenAIMock(t, `{"id":"chatcmpl_1","model":"deepseek-reasoner","choices":[{"message":{"role":"assistant","content":"Sunny.","reasoning_content":"the forecast says so"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	defer srv.Close()
	c, err := NewClient(srv.URL, "k", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{
			{Role: "user", Content: "what is the weather?"},
			{Role: "assistant", Content: "I will check.", Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "check the forecast first"}},
			}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var sent struct {
		Messages []struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*last, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if len(sent.Messages) != 2 || sent.Messages[1].ReasoningContent != "check the forecast first" {
		t.Fatalf("sent messages = %+v, want reasoning_content on the assistant message", sent.Messages)
	}
	if sent.Messages[0].ReasoningContent != "" {
		t.Fatalf("user message carried reasoning_content")
	}
	if resp.Content != "Sunny." {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 1 || resp.Reasoning.Blocks[0].Text != "the forecast says so" {
		t.Fatalf("Reasoning = %+v, want the response's reasoning_content captured", resp.Reasoning)
	}
}

func TestOpenAIClientRejectsReasoningForNonReplayModel(t *testing.T) {
	srv, _ := reasoningOpenAIMock(t, `{}`)
	defer srv.Close()
	c, err := NewClient(srv.URL, "k", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Chat(context.Background(), ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello", Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "think", Signature: "sig"}},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected an error for reasoning state on gpt-4o")
	}
	if !reasoning.IsUnrepresentable(err) {
		t.Fatalf("error is not *reasoning.UnrepresentableError: %T %v", err, err)
	}
	// The taxonomy must classify it as a fall-back, not a stop:
	// the next target may be a replay model.
	cl := ClassifyError(err, EndpointChatCompletions)
	if cl.Decision != DecisionFallback || cl.Reason != ReasonReasoningUnrepresentable {
		t.Fatalf("ClassifyError = %+v, want fallback/reasoning_unrepresentable", cl)
	}
}

func TestOpenAIClientStreamCapturesReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"model\":\"deepseek-reasoner\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"streamed \"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"model\":\"deepseek-reasoner\",\"choices\":[{\"delta\":{\"reasoning_content\":\"thought\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"model\":\"deepseek-reasoner\",\"choices\":[{\"delta\":{\"content\":\"Sunny.\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "k", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var chunks []StreamChunk
	streamer, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("openai client does not implement ChunkStreamer")
	}
	resp, err := streamer.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "deepseek-reasoner",
		Messages: []Message{
			{Role: "user", Content: "what is the weather?"},
		},
	}, func(ch StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "Sunny." {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 1 || resp.Reasoning.Blocks[0].Text != "streamed thought" {
		t.Fatalf("Reasoning = %+v, want the accumulated reasoning_content", resp.Reasoning)
	}
	for _, ch := range chunks {
		if strings.Contains(ch.Content, "streamed") {
			t.Fatalf("reasoning leaked into a stream chunk: %+v", ch)
		}
	}
}

// --- Native clients carry reasoning through ---

func TestAnthropicClientCarriesReasoning(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	// The mock's fixed reply has no thinking blocks; override the
	// stream body with one that does, and exercise the stream path.
	mock.streamBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":12}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"stream think\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-s\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi there\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	c := mustAnthropicClient(t, srv)
	streamer, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("anthropic client does not implement ChunkStreamer")
	}
	resp, err := streamer.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "user", Content: "Hi"},
		},
	}, func(StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "hi there" {
		t.Fatalf("content = %q", resp.Content)
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 1 {
		t.Fatalf("Reasoning = %+v, want 1 thinking block", resp.Reasoning)
	}
	b := resp.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindThinking || b.Text != "stream think" || b.Signature != "sig-s" {
		t.Fatalf("thinking block = %+v", b)
	}
	// And the request path: a restored thinking state must be
	// sent verbatim on the next call.
	_, err = c.Chat(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "hi there", Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "stream think", Signature: "sig-s"}},
			}},
			{Role: "user", Content: "And now?"},
		},
	})
	if err != nil {
		t.Fatalf("Chat with restored reasoning: %v", err)
	}
	var sent struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(mock.lastBody, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if len(sent.Messages) != 3 || len(sent.Messages[1].Content) == 0 || sent.Messages[1].Content[0].Type != "thinking" {
		t.Fatalf("assistant message did not lead with a thinking block: %+v", sent.Messages)
	}
	if sent.Messages[1].Content[0].Thinking != "stream think" || sent.Messages[1].Content[0].Signature != "sig-s" {
		t.Fatalf("thinking block sent = %+v", sent.Messages[1].Content[0])
	}
}

func TestGeminiClientCarriesReasoning(t *testing.T) {
	srv, mock := newGeminiMock(t)
	defer srv.Close()
	c := mustGeminiClient(t, srv)
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model: "gemini-2.0-flash",
		Messages: []Message{
			{Role: "user", Content: "Hi"},
			{Role: "assistant", Content: "hi there", Reasoning: &reasoning.State{
				Blocks:     []reasoning.Block{{Kind: reasoning.KindThought, Text: "gemini plan", Signature: "sig-g"}},
				SourceWire: reasoning.WireGemini,
			}},
		},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi there" {
		t.Fatalf("content = %q", resp.Content)
	}
	// The request must carry the thought part (with its Gemini
	// signature) ahead of the visible text.
	var sent struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text             string `json:"text"`
				Thought          bool   `json:"thought"`
				ThoughtSignature string `json:"thoughtSignature"`
			} `json:"parts"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(mock.lastBody, &sent); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if len(sent.Contents) != 2 {
		t.Fatalf("contents = %d, want 2", len(sent.Contents))
	}
	parts := sent.Contents[1].Parts
	if len(parts) != 2 || !parts[0].Thought || parts[0].Text != "gemini plan" || parts[0].ThoughtSignature != "sig-g" {
		t.Fatalf("model parts = %+v, want a leading thought part with its signature", parts)
	}
}

// --- WireFormat values ---

func TestWireFormatValues(t *testing.T) {
	cases := []struct {
		format string
		want   string
	}{
		{"", WireFormatOpenAI},
		{WireFormatOpenAI, WireFormatOpenAI},
		{WireFormatAnthropic, WireFormatAnthropic},
		{WireFormatGemini, WireFormatGemini},
		{WireFormatCohere, WireFormatCohere},
		{WireFormatBedrock, WireFormatBedrock},
	}
	for _, tc := range cases {
		c, err := NewClient("https://example.com", "k", WithWireFormat(tc.format))
		if err != nil {
			t.Fatalf("NewClient(%q): %v", tc.format, err)
		}
		wf, ok := c.(interface{ WireFormat() string })
		if !ok {
			t.Fatalf("client for %q does not expose WireFormat()", tc.format)
		}
		if got := wf.WireFormat(); got != tc.want {
			t.Fatalf("WireFormat(%q) = %q, want %q", tc.format, got, tc.want)
		}
	}
}

// --- Taxonomy: reasoning errors fall back, never stop ---

func TestClassifyErrorReasoningUnrepresentable(t *testing.T) {
	_, _, err := reasoning.Check(&reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "plain"}},
	}, reasoning.WireCohere, "command-r")
	if err == nil {
		t.Fatal("expected a reasoning error from Check")
	}
	cl := ClassifyError(err, EndpointChatCompletions)
	if cl.Decision != DecisionFallback {
		t.Fatalf("decision = %v, want fallback", cl.Decision)
	}
	if cl.Reason != ReasonReasoningUnrepresentable {
		t.Fatalf("reason = %q, want %q", cl.Reason, ReasonReasoningUnrepresentable)
	}
	// And the error still unwraps to the reasoning type after
	// the taxonomy has seen it.
	if !reasoning.IsUnrepresentable(err) {
		t.Fatalf("error no longer unwraps to *reasoning.UnrepresentableError")
	}
	var runrep *reasoning.UnrepresentableError
	if !errors.As(err, &runrep) {
		t.Fatalf("errors.As did not find *reasoning.UnrepresentableError")
	}
}
