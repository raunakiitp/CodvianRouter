package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// ---------------------------------------------------------------------------
// Reasoning continuity across failover (CHECKLIST 163-164).
// ---------------------------------------------------------------------------

// reasoningErr builds a *reasoning.UnrepresentableError the way a
// wire converter does: Check on a state the target wire cannot
// represent.
func reasoningErr(t *testing.T) error {
	t.Helper()
	_, _, err := reasoning.Check(&reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "plain reasoning"}},
	}, reasoning.WireCohere, "command-r")
	if err == nil {
		t.Fatal("reasoning.Check did not fail for Cohere")
	}
	return err
}

func thinkingResponse(content string) *provider.ChatResponse {
	return &provider.ChatResponse{
		Content: content,
		Reasoning: &reasoning.State{
			Blocks: []reasoning.Block{
				{Kind: reasoning.KindThinking, Text: "let me think", Signature: "sig-1"},
			},
			SourceWire: reasoning.WireAnthropic, SourceModel: "claude-sonnet-4-5",
		},
	}
}

// --- Unit: same-wire capture + injection, never as user content ---

func TestReasoningSameWireCaptureAndInject(t *testing.T) {
	thinking := thinkingResponse("Sunny.")
	client := &fakeClient{wireFormat: reasoning.WireAnthropic, chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return thinking, nil
	}}
	e := NewEngine(map[string]provider.Client{"anthropic": client}, nil, Config{HedgeDelay: -1})
	ctx := context.Background()

	// Turn 1: the thinking state is captured.
	resp, _, err := e.ChatWithChain(ctx, []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "key-1",
		Messages: []provider.Message{{Role: "user", Content: "what is the weather?"}},
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if resp.Content != "Sunny." {
		t.Fatalf("turn 1 content = %q", resp.Content)
	}
	if got := e.ReasoningStats(); got.Captures != 1 || got.Entries != 1 {
		t.Fatalf("stats after turn 1 = %+v, want 1 capture / 1 entry", got)
	}

	// Turn 2: the client sends the assistant message WITHOUT any
	// reasoning — the engine restores it from the store.
	_, trace, err := e.ChatWithChain(ctx, []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "key-1",
		Messages: []provider.Message{
			{Role: "user", Content: "what is the weather?"},
			{Role: "assistant", Content: "Sunny."},
			{Role: "user", Content: "and tomorrow?"},
		},
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(client.reqs) != 2 {
		t.Fatalf("client saw %d requests, want 2", len(client.reqs))
	}
	sent := client.reqs[1]
	if len(sent.Messages) != 3 {
		t.Fatalf("turn 2 messages = %d, want 3", len(sent.Messages))
	}
	// The reasoning rides ONLY on the assistant message it came
	// from — never on a user message, never as fresh content.
	if sent.Messages[0].Reasoning != nil || sent.Messages[2].Reasoning != nil {
		t.Fatalf("reasoning attached to a user message: %+v / %+v", sent.Messages[0].Reasoning, sent.Messages[2].Reasoning)
	}
	asst := sent.Messages[1]
	if asst.Reasoning == nil || len(asst.Reasoning.Blocks) != 1 {
		t.Fatalf("assistant message reasoning = %+v, want the restored thinking block", asst.Reasoning)
	}
	b := asst.Reasoning.Blocks[0]
	if b.Kind != reasoning.KindThinking || b.Text != "let me think" || b.Signature != "sig-1" {
		t.Fatalf("restored block = %+v", b)
	}
	if sent.Messages[0].Content != "what is the weather?" || sent.Messages[2].Content != "and tomorrow?" {
		t.Fatalf("user content changed: %q / %q", sent.Messages[0].Content, sent.Messages[2].Content)
	}
	if got := e.ReasoningStats(); got.Injected != 1 {
		t.Fatalf("stats after turn 2 = %+v, want 1 injection", got)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Reasoning == "" {
		t.Fatalf("trace attempts = %+v, want a reasoning label on the hop", trace.Attempts)
	}
	if !strings.Contains(trace.Attempts[0].Reasoning, "preserved") {
		t.Fatalf("trace reasoning label = %q, want it to say preserved", trace.Attempts[0].Reasoning)
	}
}

// --- Unit: unrepresentable hop fails over, breaker-neutral ---

func TestReasoningUnrepresentableHopFailsOver(t *testing.T) {
	cohereLike := &fakeClient{wireFormat: reasoning.WireCohere, chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, reasoningErr(t)
	}}
	second := okClient("served by fallback")
	e := NewEngine(map[string]provider.Client{
		"first":  cohereLike,
		"second": second,
	}, nil, Config{HedgeDelay: -1})
	ctx := context.Background()
	resp, trace, err := e.ChatWithChain(ctx, []Attempt{
		{ProviderID: "first", Model: "command-r"},
		{ProviderID: "second", Model: "gpt-4o"},
	}, provider.ChatRequest{
		Model:    "command-r",
		CallerID: "key-1",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello", Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "plain reasoning"}},
			}},
			{Role: "user", Content: "again"},
		},
	})
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "served by fallback" {
		t.Fatalf("content = %q", resp.Content)
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(trace.Attempts))
	}
	first := trace.Attempts[0]
	if first.Reason != provider.ReasonReasoningUnrepresentable {
		t.Fatalf("first attempt reason = %q, want %q", first.Reason, provider.ReasonReasoningUnrepresentable)
	}
	if !strings.Contains(first.Reasoning, "unrepresentable") {
		t.Fatalf("first attempt reasoning label = %q, want it to say unrepresentable", first.Reasoning)
	}
	// Breaker-neutral: the failure must not count against the
	// first provider's health.
	for _, bs := range e.BreakerStates() {
		if bs.ProviderID == "first" && (bs.Open || bs.ConsecutiveFailures != 0) {
			t.Fatalf("first provider breaker state = %+v, want neutral (not open, 0 failures)", bs)
		}
	}
}

// --- Unit: all targets unrepresentable -> honest error, no 502 shape ---

func TestReasoningAllUnrepresentableHonestError(t *testing.T) {
	bad := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, reasoningErr(t)
	}}
	e := NewEngine(map[string]provider.Client{"a": bad, "b": bad}, nil, Config{HedgeDelay: -1})
	_, _, err := e.ChatWithChain(context.Background(), []Attempt{
		{ProviderID: "a", Model: "command-r"},
		{ProviderID: "b", Model: "command-r"},
	}, provider.ChatRequest{
		Model:    "command-r",
		CallerID: "key-1",
		Messages: []provider.Message{
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "hello", Reasoning: &reasoning.State{
				Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "plain reasoning"}},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected an error when no target can represent the reasoning")
	}
	if !reasoning.IsUnrepresentable(err) {
		t.Fatalf("error is not *reasoning.UnrepresentableError: %T %v", err, err)
	}
	// The taxonomy must NOT call it terminal: it is a
	// per-target failure, and the server maps the surviving
	// error to an honest 400 (not a 502).
	if cls := provider.ClassifyError(err, provider.EndpointChatCompletions); cls.Decision != provider.DecisionFallback {
		t.Fatalf("ClassifyError decision = %v, want fallback", cls.Decision)
	}
}

// --- Unit: stream capture via the drain ---

func TestReasoningStreamCaptureViaDrain(t *testing.T) {
	thinking := thinkingResponse("Streamed sunny.")
	client := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("Streamed sunny."); err != nil {
			return nil, err
		}
		return thinking, nil
	}}
	e := NewEngine(map[string]provider.Client{"anthropic": client}, nil, Config{HedgeDelay: -1})
	ctx := context.Background()
	resp, err := e.chatStreamDrainChunks(ctx, []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "key-1",
		Messages: []provider.Message{{Role: "user", Content: "what is the weather?"}},
	}, 0, func(provider.StreamChunk) error { return nil }, "", false)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if resp.Content != "Streamed sunny." {
		t.Fatalf("drained content = %q", resp.Content)
	}
	if resp.Reasoning == nil || len(resp.Reasoning.Blocks) != 1 || resp.Reasoning.Blocks[0].Signature != "sig-1" {
		t.Fatalf("drained response reasoning = %+v, want the thinking block", resp.Reasoning)
	}
	if got := e.ReasoningStats(); got.Captures != 1 {
		t.Fatalf("stats after stream = %+v, want 1 capture", got)
	}
	// The next non-streaming turn restores it.
	_, _, err = e.ChatWithChain(ctx, []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "key-1",
		Messages: []provider.Message{
			{Role: "user", Content: "what is the weather?"},
			{Role: "assistant", Content: "Streamed sunny."},
			{Role: "user", Content: "and tomorrow?"},
		},
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if got := e.ReasoningStats(); got.Injected != 1 {
		t.Fatalf("stats after turn 2 = %+v, want 1 injection", got)
	}
}

// --- Unit: replay-exempt requests never touch the store ---

func TestReasoningExemptRequestsUntouched(t *testing.T) {
	thinking := thinkingResponse("embedded")
	client := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return thinking, nil
	}}
	e := NewEngine(map[string]provider.Client{"anthropic": client}, nil, Config{HedgeDelay: -1})
	_, _, err := e.ChatWithChain(context.Background(), []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "key-1",
		Endpoint: provider.EndpointEmbeddings,
		Messages: []provider.Message{{Role: "user", Content: "embed this"}},
	})
	if err != nil {
		t.Fatalf("exempt request: %v", err)
	}
	if got := e.ReasoningStats(); got.Captures != 0 || got.Entries != 0 {
		t.Fatalf("stats after exempt request = %+v, want nothing captured", got)
	}
}

// --- GATE TEST: multi-turn conversation with thinking blocks
// survives a provider switch (real Anthropic + Gemini clients
// against httptest mocks; the Gemini mock asserts the reasoning
// state arrived as a thought part). ---

type gateAnthropicMock struct {
	t     *testing.T
	mu    sync.Mutex
	calls int
}

func (m *gateAnthropicMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.calls++
		call := m.calls
		m.mu.Unlock()
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			// Turn 1: thinking blocks + text.
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"let me think","signature":"sig-gate"},{"type":"text","text":"Sunny."}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":8}}`))
			return
		}
		// Turn 2: the primary is rate-limited — the chain must
		// fail over to Gemini.
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}
}

type gateGeminiMock struct {
	t           *testing.T
	mu          sync.Mutex
	sawThought  bool
	thoughtText string
	thoughtSig  string
	calls       int
}

func (m *gateGeminiMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":generateContent") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var decoded struct {
			Contents []struct {
				Role  string `json:"role"`
				Parts []struct {
					Text             string `json:"text"`
					Thought          bool   `json:"thought"`
					ThoughtSignature string `json:"thoughtSignature"`
				} `json:"parts"`
			} `json:"contents"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			m.t.Errorf("gemini request body is not JSON: %v", err)
		}
		m.mu.Lock()
		m.calls++
		for _, c := range decoded.Contents {
			if c.Role != "model" {
				continue
			}
			for _, p := range c.Parts {
				if p.Thought {
					m.sawThought = true
					m.thoughtText = p.Text
					m.thoughtSig = p.ThoughtSignature
				}
			}
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Rainy."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":20,"candidatesTokenCount":4,"totalTokenCount":24},"modelVersion":"gemini-2.5-pro"}`))
	}
}

func TestReasoningSurvivesProviderSwitch(t *testing.T) {
	anthropicMock := &gateAnthropicMock{t: t}
	anthropicSrv := httptest.NewServer(anthropicMock.handler())
	defer anthropicSrv.Close()
	geminiMock := &gateGeminiMock{t: t}
	geminiSrv := httptest.NewServer(geminiMock.handler())
	defer geminiSrv.Close()

	anthropicClient, err := provider.NewClient(anthropicSrv.URL, "sk-ant-test", provider.WithAuthHeaders(map[string]string{
		"Authorization":     "Bearer sk-ant-test",
		"anthropic-version": "2026-10-21",
	}), provider.WithWireFormat(provider.WireFormatAnthropic))
	if err != nil {
		t.Fatalf("anthropic client: %v", err)
	}
	geminiClient, err := provider.NewClient(geminiSrv.URL, "gem-key-test", provider.WithAuthHeaders(map[string]string{
		"x-goog-api-key": "gem-key-test",
	}), provider.WithWireFormat(provider.WireFormatGemini))
	if err != nil {
		t.Fatalf("gemini client: %v", err)
	}
	e := NewEngine(map[string]provider.Client{
		"anthropic": anthropicClient,
		"gemini":    geminiClient,
	}, nil, Config{HedgeDelay: -1})
	ctx := context.Background()

	// Turn 1: Anthropic answers with thinking blocks.
	resp, _, err := e.ChatWithChain(ctx, []Attempt{{ProviderID: "anthropic", Model: "claude-sonnet-4-5"}}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "gate-key",
		Messages: []provider.Message{{Role: "user", Content: "what is the weather?"}},
	})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if resp.Content != "Sunny." {
		t.Fatalf("turn 1 content = %q", resp.Content)
	}
	if got := e.ReasoningStats(); got.Captures != 1 {
		t.Fatalf("stats after turn 1 = %+v, want 1 capture", got)
	}

	// Turn 2: the client sends the assistant message with NO
	// thinking blocks. Anthropic 429s; Gemini must receive the
	// thinking as a thought part.
	resp, trace, err := e.ChatWithChain(ctx, []Attempt{
		{ProviderID: "anthropic", Model: "claude-sonnet-4-5"},
		{ProviderID: "gemini", Model: "gemini-2.5-pro"},
	}, provider.ChatRequest{
		Model:    "claude-sonnet-4-5",
		CallerID: "gate-key",
		Messages: []provider.Message{
			{Role: "user", Content: "what is the weather?"},
			{Role: "assistant", Content: "Sunny."},
			{Role: "user", Content: "and tomorrow?"},
		},
	})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if resp.Content != "Rainy." {
		t.Fatalf("turn 2 content = %q, want the Gemini fallback's answer", resp.Content)
	}
	geminiMock.mu.Lock()
	sawThought, thoughtText, thoughtSig, geminiCalls := geminiMock.sawThought, geminiMock.thoughtText, geminiMock.thoughtSig, geminiMock.calls
	geminiMock.mu.Unlock()
	if geminiCalls != 1 {
		t.Fatalf("gemini was called %d times, want 1", geminiCalls)
	}
	if !sawThought {
		t.Fatal("gemini never received a thought part — the reasoning state did not survive the provider switch")
	}
	if thoughtText != "let me think" {
		t.Fatalf("gemini thought text = %q, want %q", thoughtText, "let me think")
	}
	// The Anthropic signature must NOT ride the Gemini thought
	// part (a foreign signature would fail Gemini's validation).
	if thoughtSig != "" {
		t.Fatalf("gemini thought part carries signature %q (anthropic signatures must drop on translation)", thoughtSig)
	}
	if len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2", len(trace.Attempts))
	}
	if trace.Attempts[0].Outcome != "rate_limited" {
		t.Fatalf("attempt 0 outcome = %q, want rate_limited", trace.Attempts[0].Outcome)
	}
	if !strings.Contains(trace.Attempts[0].Reasoning, "preserved") {
		t.Fatalf("attempt 0 reasoning label = %q, want it to say preserved (the thinking went out verbatim before the 429)", trace.Attempts[0].Reasoning)
	}
	if trace.Attempts[1].Outcome != "success" {
		t.Fatalf("attempt 1 outcome = %q, want success", trace.Attempts[1].Outcome)
	}
	if !strings.Contains(trace.Attempts[1].Reasoning, "translated") {
		t.Fatalf("attempt 1 reasoning label = %q, want it to say translated", trace.Attempts[1].Reasoning)
	}
	if got := e.ReasoningStats(); got.Injected != 1 {
		t.Fatalf("stats after turn 2 = %+v, want 1 injection", got)
	}
	// The reasoning content never reached the client as text.
	if strings.Contains(resp.Content, "let me think") {
		t.Fatalf("reasoning text leaked into the client-visible content: %q", resp.Content)
	}
}

func TestReasoningStatsEmpty(t *testing.T) {
	e := NewEngine(map[string]provider.Client{}, nil, Config{})
	if got := e.ReasoningStats(); got != (reasoning.Stats{}) {
		t.Fatalf("fresh engine stats = %+v, want zero", got)
	}
}
