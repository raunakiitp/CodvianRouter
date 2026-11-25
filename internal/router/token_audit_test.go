package router

// Token-count accuracy audit harness (CHECKLIST 229-230).
//
// The harness sends the fixed corpus (internal/tokencount.Corpus)
// through each of the five wires — OpenAI, Anthropic, Gemini,
// Cohere, Bedrock — using the REAL provider clients and the REAL
// engine, against local mocks that return KNOWN provider-reported
// usage, and reads the (local, reported) pair back out of the REAL
// ledger. The drift table it logs is the audit: local tiktoken
// estimate vs provider-reported prompt tokens, per (wire, corpus
// class).
//
// THE SIMULATED PROVIDER — read this before trusting a number:
// a mock cannot run Anthropic's, Gemini's, Cohere's or Bedrock's
// real tokenizer (no local implementation exists for them), so the
// mocks simulate a provider that bills the WIRE PAYLOAD it
// receives, tokenized with the same BPE the router used. That
// isolates the COVERAGE drift — what text the estimator ignores
// (roles, tool schemas, per-message framing) — from the
// VOCABULARY drift (a different tokenizer splitting the same text
// differently), which this audit CANNOT measure and which remains
// an open unknown for every non-OpenAI wire. The per-wire mock
// rules are:
//
//   - OpenAI: the documented cookbook rule — per message the BPE
//     count of its content (+ tool-call arguments) + 3 tokens,
//     +1 when the message carries a name, +3 reply priming, plus
//     the BPE count of the canonical JSON of the tool definitions.
//   - Anthropic / Cohere: the BPE count of the system text, plus
//     per message the BPE count of the role label + content text
//     (tool calls inside a message count as their semantic
//     content — name + arguments — see auditPartText), plus the
//     BPE count of the canonical JSON of the tool definitions.
//     (No extra framing constant: the role strings are counted
//     as the text they are.)
//   - Gemini / Bedrock: the same rule over their wire shapes —
//     system instruction / system array, per-content role +
//     parts (text parts as text, functionCall / functionResponse
//     / toolUse / toolResult blocks as their semantic content:
//     function name + arguments/response JSON, tool-result
//     content as text — see auditPartText), plus the canonical
//     JSON of the tool declarations / tool config.
//
// Completion usage is the BPE count of the fixed reply text
// "audit-ok" under the request model's encoding — the audit
// compares PROMPT tokens only; the completion side exists so the
// ledger row carries a known split.
//
// What the harness proves end to end: the request the client
// sends, the usage the mock reports, the engine's reconciliation,
// and the ledger row that stores both numbers. A sample is built
// from the LEDGER row (prompt_tokens = reported,
// local_estimate_prompt_tokens = local), never from in-memory
// values — if the plumbing dropped either side, the test fails
// instead of reporting a clean table.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// auditLedgerAdapter records engine attempts synchronously into a
// real ledger, with the same mapping as the production adapter in
// cmd/codvian-router (SplitKnown, LocalEstimate, and the local
// estimate of the prompt as sent). RecordUsage errors are kept and
// surfaced by the test — a silent drop would fake the audit.
type auditLedgerAdapter struct {
	led *ledger.Ledger

	mu      sync.Mutex
	lastErr error
}

func (a *auditLedgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	a.RecordAttemptForKey("", providerName, model, ok, latencyMs, usage)
}

func (a *auditLedgerAdapter) RecordAttemptForKey(keyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	a.record(keyID, "", providerName, model, ok, latencyMs, usage)
}

func (a *auditLedgerAdapter) RecordAttemptWithProviderKey(gatewayKeyID, providerName, providerKeyID, model string, ok bool, latencyMs int64, usage provider.Usage) {
	a.record(gatewayKeyID, providerKeyID, providerName, model, ok, latencyMs, usage)
}

func (a *auditLedgerAdapter) record(keyID, providerKeyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.led.RecordUsage(ctx, ledger.Usage{
		GatewayKeyID:              keyID,
		ProviderKeyID:             providerKeyID,
		Provider:                  providerName,
		Model:                     model,
		Endpoint:                  string(usage.Endpoint),
		PromptTokens:              usage.PromptTokens,
		CompletionTokens:          usage.CompletionTokens,
		TotalTokens:               usage.TotalTokens,
		CachedTokens:              usage.CachedTokens,
		ReasoningTokens:           usage.ReasoningTokens,
		CacheWriteTokens:          usage.CacheWriteTokens,
		PromptBeforeCompression:   usage.PromptBeforeCompression,
		CompressionSavedTokens:    usage.CompressionSavedTokens,
		LocalEstimate:             usage.LocalEstimate,
		LocalEstimatePromptTokens: usage.LocalEstimatePrompt,
		SplitKnown:                !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
		OK:                        ok,
		LatencyMS:                 latencyMs,
	}, nil)
	if err != nil {
		a.mu.Lock()
		a.lastErr = err
		a.mu.Unlock()
	}
}

func (a *auditLedgerAdapter) err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// auditBPE counts text under the request model's encoding — the
// same counter the router used, so the mock and the estimator
// differ only in WHAT text they count, never in how. A counting
// failure reports t.Errorf and counts 0, never t.Fatalf: this runs
// in the mock's HTTP handler goroutine, where Fatal would kill the
// connection instead of the test.
func auditBPE(t *testing.T, model, text string) int {
	t.Helper()
	if text == "" {
		return 0
	}
	c, err := tokencount.Count(model, text)
	if err != nil {
		t.Errorf("audit BPE count: %v", err)
		return 0
	}
	return c.Tokens
}

// auditCanonicalJSON re-marshals a JSON value with sorted keys, so
// the mock counts tool definitions deterministically regardless of
// the key order the wire client sent. Empty / null input counts 0.
func auditCanonicalJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Errorf("audit canonical JSON: %v", err)
		return ""
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Errorf("audit canonical JSON marshal: %v", err)
		return ""
	}
	return string(out)
}

// auditTextContent extracts the plain text of a content field that
// may be a string or an array of parts. Each part contributes its
// semantic text (see auditPartText) — what a provider tokenizes —
// not the JSON transport syntax around it.
func auditTextContent(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Errorf("audit content parse: %v", err)
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString(auditPartText(t, p))
	}
	return sb.String()
}

// auditPartText renders one wire content part as the text a
// provider tokenizes: the SEMANTIC content, not the JSON transport
// syntax. A text part contributes its text; a tool-call part its
// function name + its arguments/input JSON; a tool-result part its
// content text (or JSON). The wrapper keys (type, id,
// tool_use_id) are transport framing — a few tokens either way,
// inside the audit's materiality floor — and no provider documents
// billing them; counting them as canonical JSON would inflate
// every tool turn by the wrapper instead of measuring the
// estimator's real gap. An unrecognised part falls back to its
// canonical JSON, so nothing is silently dropped.
func auditPartText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	// Two block shapes exist across the wires: {"type": ...} blocks
	// (Anthropic tool_use/tool_result, Cohere text/tool-call) and
	// wrapper-keyed blocks (Bedrock {"toolUse": ...} /
	// {"toolResult": ...}, Gemini {"functionCall": ...} /
	// {"functionResponse": ...}). Both must be recognised — an
	// unrecognised block falls back to its canonical JSON, which
	// counts the wrapper keys and JSON escaping as if the
	// provider billed transport syntax (it does not).
	var probe struct {
		Type      string          `json:"type"`
		Text      string          `json:"text"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		Args      json.RawMessage `json:"args"`
		Arguments json.RawMessage `json:"arguments"`
		Content   json.RawMessage `json:"content"`
		Response  json.RawMessage `json:"response"`
		ToolUse   *struct {
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"toolUse"`
		ToolResult *struct {
			Content json.RawMessage `json:"content"`
		} `json:"toolResult"`
		FunctionCall *struct {
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		} `json:"functionCall"`
		FunctionResponse *struct {
			Name     string          `json:"name"`
			Response json.RawMessage `json:"response"`
		} `json:"functionResponse"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return auditCanonicalJSON(t, raw)
	}
	if probe.ToolUse != nil {
		return probe.ToolUse.Name + auditCanonicalJSON(t, probe.ToolUse.Input)
	}
	if probe.ToolResult != nil {
		return auditContentValue(t, probe.ToolResult.Content)
	}
	if probe.FunctionCall != nil {
		return probe.FunctionCall.Name + auditCanonicalJSON(t, probe.FunctionCall.Args)
	}
	if probe.FunctionResponse != nil {
		return probe.FunctionResponse.Name + auditContentValue(t, probe.FunctionResponse.Response)
	}
	switch probe.Type {
	case "text":
		return probe.Text
	case "tool_use", "toolUse":
		return probe.Name + auditCanonicalJSON(t, probe.Input)
	case "functionCall":
		return probe.Name + auditCanonicalJSON(t, probe.Args)
	case "tool-call":
		return probe.Name + auditCanonicalJSON(t, probe.Arguments)
	case "tool_result", "toolResult":
		return auditContentValue(t, probe.Content)
	case "functionResponse":
		return probe.Name + auditContentValue(t, probe.Response)
	default:
		if probe.Text != "" {
			return probe.Text
		}
		return auditCanonicalJSON(t, raw)
	}
}

// auditContentValue renders a tool-result content value as the text
// a provider tokenizes: a JSON string contributes the string
// itself; an object/array contributes its canonical JSON; a
// {"text": ...} or {"json": ...} wrapper is unwrapped first.
func auditContentValue(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var obj struct {
		Text string          `json:"text"`
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Text != "" {
			return obj.Text
		}
		if len(obj.JSON) > 0 {
			return auditCanonicalJSON(t, obj.JSON)
		}
	}
	return auditCanonicalJSON(t, raw)
}

// auditModelFromPath extracts the model ID from a path of the form
// prefix + model + suffix (Gemini :generateContent, Bedrock
// /converse). A mismatch is reported with t.Errorf and returns "",
// never t.Fatalf: this runs in the mock's HTTP handler goroutine,
// where Fatal would kill the connection instead of the test.
func auditModelFromPath(t *testing.T, path, prefix, suffix string) string {
	t.Helper()
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		t.Errorf("audit mock: unexpected path %q (want %s<model>%s)", path, prefix, suffix)
		return ""
	}
	model := path[len(prefix) : len(path)-len(suffix)]
	if model == "" {
		t.Errorf("audit mock: empty model in path %q", path)
	}
	return model
}

// auditReply is the fixed mock reply. Its completion count is the
// BPE count of this text under the request model.
const auditReply = "audit-ok"

func auditWriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// auditMockOpenAI returns a mock OpenAI-compatible upstream whose
// reported prompt_tokens follow the documented cookbook rule (see
// the file header): per message BPE(content) + BPE(tool-call
// arguments) + 3 (+1 with a name), +3 reply priming, plus the BPE
// count of the canonical JSON of the tool definitions.
func auditMockOpenAI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Name      string          `json:"name"`
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("openai mock: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := 3 // reply priming
		for _, m := range req.Messages {
			prompt += auditBPE(t, req.Model, auditTextContent(t, m.Content))
			for _, tc := range m.ToolCalls {
				prompt += auditBPE(t, req.Model, tc.Function.Arguments)
			}
			prompt += 3
			if m.Name != "" {
				prompt++
			}
		}
		prompt += auditBPE(t, req.Model, auditCanonicalJSON(t, req.Tools))
		completion := auditBPE(t, req.Model, auditReply)
		auditWriteJSON(w, map[string]any{
			"id": "chatcmpl-audit", "object": "chat.completion", "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": auditReply},
			}},
			"usage": map[string]any{
				"prompt_tokens": prompt, "completion_tokens": completion,
				"total_tokens": prompt + completion,
			},
		})
	}))
}

// auditMockAnthropic returns a mock Anthropic Messages upstream.
// Reported input_tokens = BPE(system) + per message BPE(role) +
// BPE(content text) + BPE(canonical JSON of the tool definitions).
func auditMockAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string          `json:"model"`
			System   json.RawMessage `json:"system"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
			Tools json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("anthropic mock: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := auditBPE(t, req.Model, auditTextContent(t, req.System))
		for _, m := range req.Messages {
			prompt += auditBPE(t, req.Model, m.Role)
			prompt += auditBPE(t, req.Model, auditTextContent(t, m.Content))
		}
		prompt += auditBPE(t, req.Model, auditCanonicalJSON(t, req.Tools))
		completion := auditBPE(t, req.Model, auditReply)
		auditWriteJSON(w, map[string]any{
			"id": "msg-audit", "type": "message", "role": "assistant", "model": req.Model,
			"content":     []any{map[string]any{"type": "text", "text": auditReply}},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": prompt, "output_tokens": completion},
		})
	}))
}

// auditMockGemini returns a mock Gemini generateContent upstream.
// Reported promptTokenCount = BPE(systemInstruction) + per content
// BPE(role) + BPE(parts: text as text, function calls/responses as
// canonical JSON) + BPE(canonical JSON of the function declarations).
func auditMockGemini(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := auditModelFromPath(t, r.URL.Path, "/models/", ":generateContent")
		if model == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"systemInstruction"`
			Contents []struct {
				Role  string            `json:"role"`
				Parts []json.RawMessage `json:"parts"`
			} `json:"contents"`
			Tools json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("gemini mock: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := 0
		if req.SystemInstruction != nil {
			for _, p := range req.SystemInstruction.Parts {
				prompt += auditBPE(t, model, p.Text)
			}
		}
		for _, c := range req.Contents {
			prompt += auditBPE(t, model, c.Role)
			for _, part := range c.Parts {
				prompt += auditBPE(t, model, auditPartText(t, part))
			}
		}
		prompt += auditBPE(t, model, auditCanonicalJSON(t, req.Tools))
		completion := auditBPE(t, model, auditReply)
		auditWriteJSON(w, map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{map[string]any{"text": auditReply}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount": prompt, "candidatesTokenCount": completion,
				"totalTokenCount": prompt + completion,
			},
		})
	}))
}

// auditMockCohere returns a mock Cohere Chat v2 upstream. Reported
// usage = per message BPE(role) + BPE(content text) + BPE(canonical
// JSON of the tool definitions). Content parts without a "text"
// field (tool calls) count as their canonical JSON.
func auditMockCohere(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("cohere mock: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := 0
		for _, m := range req.Messages {
			prompt += auditBPE(t, req.Model, m.Role)
			prompt += auditBPE(t, req.Model, auditTextContent(t, m.Content))
			for _, tc := range m.ToolCalls {
				prompt += auditBPE(t, req.Model, tc.Function.Name)
				prompt += auditBPE(t, req.Model, tc.Function.Arguments)
			}
		}
		prompt += auditBPE(t, req.Model, auditCanonicalJSON(t, req.Tools))
		completion := auditBPE(t, req.Model, auditReply)
		auditWriteJSON(w, map[string]any{
			"id": "chat-audit", "finish_reason": "COMPLETE",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": auditReply}}},
			"usage":   map[string]any{"input_tokens": prompt, "output_tokens": completion},
		})
	}))
}

// auditMockBedrock returns a mock Bedrock Converse upstream.
// Reported inputTokens = BPE(system) + per message BPE(role) +
// BPE(content blocks: text as text, toolUse/toolResult as
// canonical JSON) + BPE(canonical JSON of the tool config).
func auditMockBedrock(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		model := auditModelFromPath(t, r.URL.Path, "/model/", "/converse")
		if model == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
			Messages []struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
			ToolConfig json.RawMessage `json:"toolConfig"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("bedrock mock: decode: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		prompt := 0
		for _, s := range req.System {
			prompt += auditBPE(t, model, s.Text)
		}
		for _, m := range req.Messages {
			prompt += auditBPE(t, model, m.Role)
			for _, block := range m.Content {
				prompt += auditBPE(t, model, auditPartText(t, block))
			}
		}
		prompt += auditBPE(t, model, auditCanonicalJSON(t, req.ToolConfig))
		completion := auditBPE(t, model, auditReply)
		auditWriteJSON(w, map[string]any{
			"output": map[string]any{"message": map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"text": auditReply}},
			}},
			"stopReason": "end_turn",
			"usage":      map[string]any{"inputTokens": prompt, "outputTokens": completion, "totalTokens": prompt + completion},
		})
	}))
}

// auditWire is one wire under audit: the label the drift table
// uses, the model the harness sends, and how to build a real
// client for the mock.
type auditWire struct {
	name      string
	model     string
	newClient func(url string) (provider.Client, error)
}

func TestTokenCountAudit(t *testing.T) {
	ctx := context.Background()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "audit-ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	adapter := &auditLedgerAdapter{led: led}

	wires := []auditWire{
		{name: "openai", model: "gpt-4o-mini", newClient: func(url string) (provider.Client, error) {
			return provider.NewClient(url, "audit-key", provider.WithAllowPrivateDestinations(true))
		}},
		{name: "anthropic", model: "claude-sonnet-4-5", newClient: func(url string) (provider.Client, error) {
			return provider.NewClient(url, "audit-key", provider.WithWireFormat(provider.WireFormatAnthropic), provider.WithAllowPrivateDestinations(true))
		}},
		{name: "gemini", model: "gemini-2.5-flash", newClient: func(url string) (provider.Client, error) {
			return provider.NewClient(url, "audit-key",
				provider.WithAuthHeaders(map[string]string{"x-goog-api-key": "audit-key"}),
				provider.WithWireFormat(provider.WireFormatGemini), provider.WithAllowPrivateDestinations(true))
		}},
		{name: "cohere", model: "command-a-03-2025", newClient: func(url string) (provider.Client, error) {
			return provider.NewClient(url, "audit-key",
				provider.WithAuthHeaders(map[string]string{"Authorization": "Bearer audit-key"}),
				provider.WithWireFormat(provider.WireFormatCohere), provider.WithAllowPrivateDestinations(true))
		}},
		{name: "bedrock", model: "anthropic.claude-sonnet-4-5-20250929-v1:0", newClient: func(url string) (provider.Client, error) {
			return provider.NewClient(url, "AKIDEXAMPLE:SECRET", provider.WithWireFormat(provider.WireFormatBedrock), provider.WithAllowPrivateDestinations(true))
		}},
	}
	mocks := map[string]func(t *testing.T) *httptest.Server{
		"openai": auditMockOpenAI, "anthropic": auditMockAnthropic,
		"gemini": auditMockGemini, "cohere": auditMockCohere, "bedrock": auditMockBedrock,
	}

	var samples []tokencount.Sample
	totalCompared := 0
	for _, w := range wires {
		srv := mocks[w.name](t)
		t.Cleanup(srv.Close)
		client, err := w.newClient(srv.URL)
		if err != nil {
			t.Fatalf("wire %s: NewClient: %v", w.name, err)
		}
		eng := NewEngine(map[string]provider.Client{"audit": client}, adapter, testConfig())
		for _, c := range tokencount.Corpus() {
			req := provider.ChatRequest{
				Model: w.model, Messages: c.Messages, Tools: c.Tools,
				MaxTokens: 64, CallerID: "audit-key",
			}
			resp, _, err := eng.ChatWithChain(ctx, []Attempt{{ProviderID: "audit", Model: w.model}}, req)
			if err != nil {
				t.Errorf("wire %s case %s: ChatWithChain: %v", w.name, c.ID, err)
				continue
			}
			if resp.Content != auditReply {
				t.Errorf("wire %s case %s: content = %q, want %q", w.name, c.ID, resp.Content, auditReply)
			}
			// The sample comes from the LEDGER row, never from
			// in-memory values: the newest row must be this
			// attempt, its reported prompt count must match the
			// response usage, and it must carry the local
			// estimate alongside it.
			events, err := led.RecentEvents(ctx, 1, ledger.LogAll)
			if err != nil {
				t.Fatalf("wire %s case %s: RecentEvents: %v", w.name, c.ID, err)
			}
			if len(events) != 1 {
				t.Fatalf("wire %s case %s: RecentEvents = %d rows, want 1", w.name, c.ID, len(events))
			}
			row := events[0]
			if row.Provider != "audit" || row.Model != w.model {
				t.Errorf("wire %s case %s: ledger row provider/model = %s/%s, want audit/%s", w.name, c.ID, row.Provider, row.Model, w.model)
			}
			if !row.SplitKnown {
				t.Errorf("wire %s case %s: ledger row split unknown — the mock reported usage, the row must carry it", w.name, c.ID)
			}
			if row.PromptTokens != resp.Usage.PromptTokens {
				t.Errorf("wire %s case %s: ledger prompt_tokens %d != response usage %d", w.name, c.ID, row.PromptTokens, resp.Usage.PromptTokens)
			}
			if row.LocalEstimatePromptTokens <= 0 {
				t.Errorf("wire %s case %s: ledger row carries no local estimate", w.name, c.ID)
				continue
			}
			if row.PromptTokens <= 0 {
				t.Errorf("wire %s case %s: mock reported no prompt tokens", w.name, c.ID)
				continue
			}
			samples = append(samples, tokencount.Sample{
				Wire: w.name, Class: c.Class, CaseID: c.ID,
				Local: row.LocalEstimatePromptTokens, Reported: row.PromptTokens,
			})
		}
		if err := adapter.err(); err != nil {
			t.Fatalf("wire %s: ledger record failed: %v", w.name, err)
		}
		st := eng.TokenCountStats()
		if st.Compared != 18 {
			t.Errorf("wire %s: engine compared %d attempts, want 18 (one per corpus case)", w.name, st.Compared)
		}
		totalCompared += int(st.Compared)
		// The engine's own divergence counter must agree with
		// the ledger-derived samples for this wire: a material
		// divergence is >20 tokens AND >10% of reported.
		var material int
		for _, s := range samples {
			if s.Wire != w.name {
				continue
			}
			diff := s.Local - s.Reported
			if diff < 0 {
				diff = -diff
			}
			threshold := s.Reported / 10
			if threshold < 20 {
				threshold = 20
			}
			if diff > threshold {
				material++
			}
		}
		if int(st.Diverged) != material {
			t.Errorf("wire %s: engine diverged %d, ledger-derived material divergences %d — the two must agree", w.name, st.Diverged, material)
		}
	}

	// Coverage: every (wire, class) cell has exactly its 3 cases.
	if len(samples) != 90 {
		t.Fatalf("samples = %d, want 90 (5 wires x 18 cases)", len(samples))
	}
	if totalCompared != 90 {
		t.Errorf("engine compared %d attempts in total, want 90", totalCompared)
	}
	stats := tokencount.Summarize(samples)
	if len(stats) != 30 {
		t.Fatalf("drift cells = %d, want 30 (5 wires x 6 classes)", len(stats))
	}
	for _, st := range stats {
		if st.N != 3 {
			t.Errorf("cell %s/%s: N = %d, want 3", st.Wire, st.Class, st.N)
		}
	}

	// The audit table, logged verbatim in the form the doc uses —
	// docs/token-count-audit.md copies this output from a real run.
	t.Logf("token-count audit (local estimate vs provider-reported prompt tokens; error %% = 100*(local-reported)/reported):\n%s", tokencount.MarkdownTable(stats))

	// Drift bounds the fixed estimator must hold (CHECKLIST 230):
	// after the estimator counts tool definitions and per-message
	// framing, no (wire, class) cell may undercount systematically
	// by more than 5% on average, and no single case may be off
	// by more than 20% in either direction. The bounds are set
	// from the measured post-fix numbers with headroom, and the
	// doc records the actuals — a wire that regresses past them
	// fails here instead of silently endangering a budget.
	for _, st := range stats {
		if st.MeanErrPct < -5 {
			t.Errorf("cell %s/%s: mean error %+.2f%% — the estimator undercounts systematically (bound: >= -5%%)", st.Wire, st.Class, st.MeanErrPct)
		}
		if st.MaxAbsPct > 20 {
			t.Errorf("cell %s/%s: max |error| %.2f%% — a single case is too far off (bound: <= 20%%)", st.Wire, st.Class, st.MaxAbsPct)
		}
	}

	// The ledger itself must reproduce the same drift the harness
	// measured: the dashboard reads DriftSummary, not this test's
	// in-memory samples.
	drift, err := led.DriftSummary(ctx, time.Now().Add(-time.Hour), time.Time{})
	if err != nil {
		t.Fatalf("DriftSummary: %v", err)
	}
	if len(drift) != 5 {
		t.Fatalf("DriftSummary = %d rows, want 5 (one per wire model)", len(drift))
	}
	for _, d := range drift {
		// 7D: a row whose local estimate equals the recorded prompt
		// count is estimate-backed (a conservative match) and is
		// excluded from Compared — so Compared + EstimateBacked is
		// the population that carried both sides of the comparison.
		// Every one of the 6 classes x 3 samples per wire model must
		// be accounted for in exactly one bucket.
		if d.Compared+d.EstimateBacked != 18 {
			t.Errorf("drift row %s/%s: compared %d + estimate_backed %d = %d, want 18 total samples",
				d.Provider, d.Model, d.Compared, d.EstimateBacked, d.Compared+d.EstimateBacked)
		}
	}
}
