// M8.13 tests: Gemini-native surface.
//
// Two layers: pure-translation unit tests (no server), and
// handler tests. The fixture contract tests run through the real
// handler with a mock upstream speaking the Gemini wire: a
// provider.NewClient pointed at an httptest server (Gemini wire
// format) wrapped in a test engine, so the request travels
// Gemini-native HTTP -> translation -> existing geminiClient ->
// mock upstream -> Gemini-native HTTP. No live provider is ever
// involved; every fixture is labelled mock data.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// geminiTestRegistry is a ModelRegistry that also reports wire
// formats (dashboardModelLister), so GET /v1beta/models can tell
// Gemini models apart from the rest.
type geminiTestRegistry struct {
	models []provider.DashboardModel
}

func (r *geminiTestRegistry) Models(_ context.Context) ([]Model, error) {
	out := make([]Model, 0, len(r.models))
	for _, m := range r.models {
		out = append(out, Model{
			ID: m.ID, Object: "model", OwnedBy: m.ProviderName,
			ProviderID: m.ProviderID, Category: m.Category,
			ContextWindow: m.ContextWindow, InputPriceUSD: m.InputPriceUSD,
			OutputPriceUSD: m.OutputPriceUSD, Deprecated: m.Deprecated,
		})
	}
	return out, nil
}

func (r *geminiTestRegistry) DashboardModels() []provider.DashboardModel {
	return r.models
}

// geminiClientEngine adapts one provider.Client (the existing
// Gemini wire client, pointed at a mock upstream) to the server
// Engine contract, including the optional chunk-streaming half.
type geminiClientEngine struct {
	client provider.Client
}

func (e *geminiClientEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return e.client.Chat(ctx, req)
}

func (e *geminiClientEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return e.client.ChatStream(ctx, req, onDelta)
}

func (e *geminiClientEngine) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if cs, ok := e.client.(provider.ChunkStreamer); ok {
		return cs.ChatStreamChunks(ctx, req, onChunk)
	}
	return e.client.ChatStream(ctx, req, func(delta string) error {
		return onChunk(provider.StreamChunk{Content: delta})
	})
}

// geminiRecordingEngine records the canonical request the handler
// built, so tests can assert on the translation itself.
type geminiRecordingEngine struct {
	resp       provider.ChatResponse
	lastReq    provider.ChatRequest
	lastStream bool
}

func (e *geminiRecordingEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.lastReq = req
	r := e.resp
	return &r, nil
}

func (e *geminiRecordingEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.lastReq = req
	e.lastStream = true
	r := e.resp
	return &r, nil
}

// geminiChunkEngine replays canned chunks, for the chunk-aware
// streaming path.
type geminiChunkEngine struct {
	resp      provider.ChatResponse
	chunks    []provider.StreamChunk
	streamErr error
}

func (e *geminiChunkEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	r := e.resp
	return &r, nil
}

func (e *geminiChunkEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	for _, c := range e.chunks {
		if c.Content != "" {
			if err := onDelta(c.Content); err != nil {
				return nil, err
			}
		}
	}
	r := e.resp
	return &r, nil
}

func (e *geminiChunkEngine) ChatStreamChunks(_ context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	for _, c := range e.chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	if e.streamErr != nil {
		return nil, e.streamErr
	}
	r := e.resp
	return &r, nil
}

// geminiMockUpstream is an httptest server speaking the Gemini
// wire: :generateContent answers the canned JSON body,
// :streamGenerateContent?alt=sse answers the canned SSE payload,
// /models answers the canned list. It records the last request
// it saw so tests can assert on the translated upstream call.
type geminiMockUpstream struct {
	t            *testing.T
	generateBody any
	streamBody   string
	lastPath     string
	lastBody     []byte
	lastHeaders  http.Header
}

func (m *geminiMockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		if r.URL.RawQuery != "" {
			m.lastPath += "?" + r.URL.RawQuery
		}
		m.lastHeaders = r.Header
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch {
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m.generateBody)
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, m.streamBody)
		case r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newGeminiTestMux wires the M8.13 handlers the way the
// coordinator will register them (gateway-key auth + scope),
// on a test mux. Registration itself (guard.go / server.go)
// is the coordinator's step; this mirrors it so the handler
// tests prove the auth wrapper composes.
//
// NOTE (review F2): this mux wraps with requireAuth, while
// production TierManagement registration uses
// requireAuthUnlessRelaxed (guard.go:378) — in relaxed-auth
// mode the real route serves unauthenticated requests, while
// this test mux proves a 401. The 401 case below therefore
// proves the wrapper composes, not production's relaxed-mode
// behaviour.
func newGeminiTestMux(t *testing.T, eng Engine, reg ModelRegistry) *http.ServeMux {
	t.Helper()
	srv := newTestServer(t, eng, WithRegistry(reg))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1beta/models",
		srv.requireAuth(srv.requireScope(srv.handleGeminiModels, []string{"models"})))
	mux.HandleFunc("POST /v1beta/models/{rest...}",
		srv.requireAuth(srv.requireScope(srv.handleGeminiModelAction, []string{"chat"})))
	return mux
}

func newGeminiTestRegistry() *geminiTestRegistry {
	return &geminiTestRegistry{models: []provider.DashboardModel{
		{ID: "mock-gemini-model", ProviderID: "mock-upstream", ProviderName: "mock-upstream", WireFormat: provider.WireFormatGemini},
		{ID: "mock-gemini-deprecated", ProviderID: "mock-upstream", ProviderName: "mock-upstream", WireFormat: provider.WireFormatGemini, Deprecated: true},
		{ID: "mock-openai-model", ProviderID: "mock-openai", ProviderName: "mock-openai", WireFormat: provider.WireFormatOpenAI},
	}}
}

func mustGeminiTestClient(t *testing.T, upstream *httptest.Server) provider.Client {
	t.Helper()
	c, err := provider.NewClient(upstream.URL, "mock-key",
		provider.WithAuthHeaders(map[string]string{"x-goog-api-key": "mock-key"}),
		provider.WithWireFormat(provider.WireFormatGemini), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient gemini: %v", err)
	}
	return c
}

func doGeminiReq(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// Fixture contract tests (docs/API_FIXTURES.md).
// ---------------------------------------------------------------------------

func TestM813ModelsFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.13", "models-200")
	mux := newGeminiTestMux(t, &fakeEngine{}, newGeminiTestRegistry())
	apitest.Run(t, mux, f)
}

func TestM813GenerateContentFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.13", "generate-content-200")
	mock := &geminiMockUpstream{t: t, generateBody: f.RecordedResponse.Body}
	upstream := httptest.NewServer(mock.handler())
	defer upstream.Close()
	eng := &geminiClientEngine{client: mustGeminiTestClient(t, upstream)}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	apitest.Run(t, mux, f)

	// The mock upstream proves the translation reached the
	// existing Gemini wire client, speaking the Gemini wire.
	if mock.lastPath != "/models/mock-gemini-model:generateContent" {
		t.Errorf("upstream path = %q, want /models/mock-gemini-model:generateContent", mock.lastPath)
	}
	if got := mock.lastHeaders.Get("x-goog-api-key"); got != "mock-key" {
		t.Errorf("x-goog-api-key = %q, want mock-key", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(mock.lastBody, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	contents, _ := sent["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("upstream contents = %v, want 1 turn", sent["contents"])
	}
	turn, _ := contents[0].(map[string]any)
	parts, _ := turn["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "Hello, mock Gemini." {
		t.Errorf("upstream parts = %v, want the user text", turn["parts"])
	}
	gen, _ := sent["generationConfig"].(map[string]any)
	if gen["temperature"] != 0.5 || gen["maxOutputTokens"] != float64(64) {
		t.Errorf("upstream generationConfig = %v, want temperature 0.5 + maxOutputTokens 64", gen)
	}
}

func TestM813StreamGenerateContentFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.13", "stream-generate-content-200")
	body, ok := f.RecordedResponse.Body.(string)
	if !ok {
		t.Fatalf("recorded_response.body is %T, want the SSE string", f.RecordedResponse.Body)
	}
	mock := &geminiMockUpstream{t: t, streamBody: body}
	upstream := httptest.NewServer(mock.handler())
	defer upstream.Close()
	eng := &geminiClientEngine{client: mustGeminiTestClient(t, upstream)}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	apitest.Run(t, mux, f)

	if mock.lastPath != "/models/mock-gemini-model:streamGenerateContent?alt=sse" {
		t.Errorf("upstream path = %q, want /models/mock-gemini-model:streamGenerateContent?alt=sse", mock.lastPath)
	}
}

// TestM813FixtureToggleNegative proves the contract assertions
// fail on a wrong expectation: the same fixture with a tampered
// expected status must not pass.
func TestM813FixtureToggleNegative(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.13", "generate-content-200")
	f.Assert.Status = http.StatusCreated // the handler answers 200
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{}`), f); err == nil {
		t.Fatal("CheckResponse passed with a tampered expected status (201 vs actual 200); the assertion path is dead")
	}
}

// ---------------------------------------------------------------------------
// Translation unit tests.
// ---------------------------------------------------------------------------

func mustTranslate(t *testing.T, model, body string) provider.ChatRequest {
	t.Helper()
	in, err := parseGeminiGenerateRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req, err := geminiToChatRequest(model, in)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return req
}

func TestM813TranslateBasic(t *testing.T) {
	req := mustTranslate(t, "mock-gemini-model", `{
		"contents": [
			{"role": "user", "parts": [{"text": "What is the weather?"}]},
			{"role": "model", "parts": [{"text": "Checking."}, {"functionCall": {"name": "get_weather", "args": {"city": "Pune"}}}]},
			{"role": "function", "parts": [{"functionResponse": {"name": "get_weather", "response": {"result": "sunny"}}}]}
		],
		"systemInstruction": {"parts": [{"text": "Be helpful."}]},
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "Weather", "parameters": {"type": "object"}}]}],
		"toolConfig": {"functionCallingConfig": {"mode": "ANY", "allowedFunctionNames": ["get_weather"]}},
		"generationConfig": {"temperature": 0.5, "topP": 0.9, "maxOutputTokens": 64, "stopSequences": ["END"], "thinkingConfig": {"thinkingBudget": 128}}
	}`)
	if req.Model != "mock-gemini-model" {
		t.Errorf("Model = %q", req.Model)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system + user + model + tool)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "Be helpful." {
		t.Errorf("system message = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "What is the weather?" {
		t.Errorf("user message = %+v", req.Messages[1])
	}
	if got := req.Messages[2].ToolCalls; len(got) != 1 || got[0].Function.Name != "get_weather" || got[0].Function.Arguments != `{"city": "Pune"}` {
		t.Errorf("model tool calls = %+v", got)
	}
	if tool := req.Messages[3]; tool.Role != "tool" || tool.ToolCallID != "get_weather" || tool.Content != `{"result":"sunny"}` {
		t.Errorf("tool message = %+v", tool)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools = %+v", req.Tools)
	}
	var tc map[string]any
	if err := json.Unmarshal(req.ToolChoice, &tc); err != nil {
		t.Fatalf("tool choice is not JSON: %v", err)
	}
	fn, _ := tc["function"].(map[string]any)
	if tc["type"] != "function" || fn["name"] != "get_weather" {
		t.Errorf("tool choice = %s, want the named get_weather function", req.ToolChoice)
	}
	if req.Temperature != 0.5 || req.TopP != 0.9 || req.MaxTokens != 64 {
		t.Errorf("sampling = temp %v topP %v maxTokens %v", req.Temperature, req.TopP, req.MaxTokens)
	}
	if len(req.Stop) != 1 || req.Stop[0] != "END" {
		t.Errorf("stop = %v", req.Stop)
	}
	if req.ThinkingBudget != 128 {
		t.Errorf("ThinkingBudget = %d, want 128", req.ThinkingBudget)
	}
}

func TestM813TranslateModelsPrefixStripped(t *testing.T) {
	req := mustTranslate(t, "models/mock-gemini-model", `{"contents": [{"role": "user", "parts": [{"text": "hi"}]}]}`)
	if req.Model != "mock-gemini-model" {
		t.Errorf("Model = %q, want the models/ prefix stripped", req.Model)
	}
}

func TestM813TranslateImageAndThought(t *testing.T) {
	req := mustTranslate(t, "mock-gemini-model", `{
		"contents": [
			{"role": "user", "parts": [{"text": "What is this?"}, {"inlineData": {"mimeType": "image/png", "data": "aGVsbG8="}}]},
			{"role": "model", "parts": [{"text": "A cat.", "thought": true, "thoughtSignature": "sig-1"}, {"text": "It is a cat."}]}
		]
	}`)
	user := req.Messages[0]
	if len(user.Parts) != 2 || user.Parts[1].Type != "image_url" || user.Parts[1].ImageURL.URL != "data:image/png;base64,aGVsbG8=" {
		t.Errorf("user parts = %+v, want text + image_url data URL", user.Parts)
	}
	model := req.Messages[1]
	if model.Content != "It is a cat." {
		t.Errorf("model content = %q, want only the visible text (thought text must not leak into content)", model.Content)
	}
	if model.Reasoning == nil || model.Reasoning.SourceWire != reasoning.WireGemini {
		t.Fatalf("model reasoning = %+v, want a Gemini-sourced state", model.Reasoning)
	}
	foundThought := false
	for _, b := range model.Reasoning.Blocks {
		if b.Kind == reasoning.KindThought && b.Text == "A cat." && b.Signature == "sig-1" {
			foundThought = true
		}
	}
	if !foundThought {
		t.Errorf("reasoning blocks = %+v, want the thought text with its signature", model.Reasoning.Blocks)
	}
}

// TestM813TranslateUnmappedFields: every field with no canonical
// mapping must fail with a 400 naming the field, never pass
// silently. 26 subtests = 25 rejections + 1 positive control
// (badArgsJSON: valid object args must translate, not reject).
// These are pure-translation rejections; the HTTP envelope they
// are served in is pinned by TestM813PreEngineErrorsGeminiEnvelope.
func TestM813TranslateUnmappedFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string // substring the error must name
	}{
		{"topK", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":40}}`, "generationConfig.topK"},
		{"candidateCount", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":2}}`, "generationConfig.candidateCount"},
		{"responseMimeType", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json"}}`, "generationConfig.responseMimeType"},
		{"responseSchema", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseSchema":{"type":"object"}}}`, "generationConfig.responseSchema"},
		{"presencePenalty", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"presencePenalty":0.5}}`, "generationConfig.presencePenalty"},
		{"frequencyPenalty", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"frequencyPenalty":0.5}}`, "generationConfig.frequencyPenalty"},
		{"seed", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"seed":7}}`, "generationConfig.seed"},
		{"includeThoughts", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true}}}`, "generationConfig.thinkingConfig.includeThoughts"},
		{"safetySettings", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"safetySettings":[{"category":"HARM_CATEGORY_HATE_SPEECH","threshold":"BLOCK_NONE"}]}`, "safetySettings"},
		{"cachedContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"cachedContent":"cachedContents/abc"}`, "cachedContent"},
		{"labels", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"labels":{"env":"test"}}`, "labels"},
		{"fileData", `{"contents":[{"role":"user","parts":[{"fileData":{"mimeType":"application/pdf","fileUri":"files/abc"}}]}]}`, "fileData"},
		{"executableCode", `{"contents":[{"role":"user","parts":[{"executableCode":{"language":"PYTHON","code":"print(1)"}}]}]}`, "executableCode"},
		{"unknownTopLevel", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"model":"mock-gemini-model"}`, "model"},
		{"unknownRole", `{"contents":[{"role":"system","parts":[{"text":"hi"}]}]}`, "contents[0].role"},
		{"modelFirst", `{"contents":[{"role":"model","parts":[{"text":"hi"}]}]}`, "must start with a user turn"},
		{"emptyContents", `{"contents":[]}`, "contents is required"},
		{"functionResponseInUser", `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"f","response":{}}}]}]}`, "functionResponse"},
		{"functionCallInUser", `{"contents":[{"role":"user","parts":[{"functionCall":{"name":"f","args":{}}}]}]}`, "functionCall"},
		{"badArgsJSON", `{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"name":"f","args":{"x":1}}}]}]}`, ""}, // positive control (the 1 non-rejection of the 26): valid JSON object args must NOT fail (control case below)
		{"autoWithAllowed", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"f"}]}],"toolConfig":{"functionCallingConfig":{"mode":"AUTO","allowedFunctionNames":["f"]}}}`, "allowedFunctionNames"},
		{"badMode", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"toolConfig":{"functionCallingConfig":{"mode":"VALIDATE"}}}`, "toolConfig.functionCallingConfig.mode"},
		{"temperatureRange", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"temperature":2.5}}`, "generationConfig.temperature"},
		{"tooManyStops", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"stopSequences":["a","b","c","d","e","f"]}}`, "generationConfig.stopSequences"},
		{"emptyPart", `{"contents":[{"role":"user","parts":[{}]}]}`, "is empty"},
		{"toolNoName", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"description":"x"}]}]}`, "functionDeclarations[0].name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in, err := parseGeminiGenerateRequest([]byte(tc.body))
			if err != nil {
				if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("parse error = %q, want it to name %q", err, tc.want)
				}
				return
			}
			_, err = geminiToChatRequest("mock-gemini-model", in)
			if tc.name == "badArgsJSON" {
				// Control case: args as a JSON object is valid
				// and must translate (the wire accepts objects).
				if err != nil {
					t.Fatalf("valid object args must translate, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected a rejection naming %q, got none", tc.want)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

func TestM813TranslateCandidateCountOneOK(t *testing.T) {
	req := mustTranslate(t, "mock-gemini-model", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"candidateCount":1}}`)
	if len(req.Messages) != 1 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
}

// ---------------------------------------------------------------------------
// Response conversion + error mapping.
// ---------------------------------------------------------------------------

func TestM813ResponseMapping(t *testing.T) {
	resp := &provider.ChatResponse{
		Content: "sunny",
		Model:   "mock-gemini-model",
		Usage:   provider.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12, CachedTokens: 2, ReasoningTokens: 3},
		ToolCalls: []provider.ToolCall{
			{ID: "call_0", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}},
		},
		FinishReason: "tool_calls",
	}
	gem, err := chatResponseToGemini(resp)
	if err != nil {
		t.Fatalf("chatResponseToGemini: %v", err)
	}
	if gem.ModelVersion != "mock-gemini-model" {
		t.Errorf("ModelVersion = %q", gem.ModelVersion)
	}
	if len(gem.Candidates) != 1 || gem.Candidates[0].FinishReason != "TOOL_CALLS" {
		t.Fatalf("candidates = %+v, want one TOOL_CALLS candidate", gem.Candidates)
	}
	parts := gem.Candidates[0].Content.Parts
	if len(parts) != 2 || parts[0].Text != "sunny" || parts[1].FunctionCall == nil || parts[1].FunctionCall.Name != "get_weather" {
		t.Fatalf("parts = %+v, want text + functionCall", parts)
	}
	if gem.UsageMetadata == nil || gem.UsageMetadata.PromptTokenCount != 7 || gem.UsageMetadata.TotalTokenCount != 12 ||
		gem.UsageMetadata.CachedContentTokenCount != 2 || gem.UsageMetadata.ThoughtsTokenCount != 3 {
		t.Errorf("usageMetadata = %+v", gem.UsageMetadata)
	}
}

func TestM813FinishReasonMapping(t *testing.T) {
	for in, want := range map[string]string{
		"stop": "STOP", "length": "MAX_TOKENS", "tool_calls": "TOOL_CALLS",
		"content_filter": "SAFETY", "": "", "weird": "",
	} {
		if got := geminiFinishReason(in); got != want {
			t.Errorf("geminiFinishReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestM813ErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantGemini string
	}{
		{"rateLimit", &provider.RateLimitError{RetryAfter: 30 * time.Second}, 429, "RESOURCE_EXHAUSTED"},
		{"quota", &provider.QuotaError{StatusCode: 402, Message: "billing"}, 402, "PERMISSION_DENIED"},
		{"unrepresentable", &wire.UnrepresentableError{Reason: "generationConfig.topK has no mapping"}, 400, "INVALID_ARGUMENT"},
		{"upstream404", &provider.UpstreamError{StatusCode: 404, Body: "not found"}, 404, "NOT_FOUND"},
		{"upstream502", &provider.UpstreamError{StatusCode: 502, Body: "bad gateway"}, 502, "UNAVAILABLE"},
		{"upstreamNoStatus", &provider.UpstreamError{Body: "boom"}, 502, "UNAVAILABLE"},
		{"generic", errors.New("boom"), 502, "UNAVAILABLE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeGeminiError(rec, tc.err)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var env geminiErrorEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not a Gemini envelope: %v", err)
			}
			if env.Error.Status != tc.wantGemini || env.Error.Code != tc.wantStatus {
				t.Errorf("envelope = %+v, want status %q code %d", env.Error, tc.wantGemini, tc.wantStatus)
			}
		})
	}
	rec := httptest.NewRecorder()
	writeGeminiError(rec, &provider.RateLimitError{RetryAfter: 30 * time.Second})
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
}

// ---------------------------------------------------------------------------
// Handler tests.
// ---------------------------------------------------------------------------

func TestM813ModelsHandler(t *testing.T) {
	mux := newGeminiTestMux(t, &fakeEngine{}, newGeminiTestRegistry())
	rec := doGeminiReq(t, mux, http.MethodGet, "/v1beta/models", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out geminiModelsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(out.Models) != 1 || out.Models[0].Name != "models/mock-gemini-model" {
		t.Fatalf("models = %+v, want only models/mock-gemini-model (deprecated hidden, non-Gemini excluded)", out.Models)
	}
	if len(out.Models[0].SupportedGenerationMethods) != 2 {
		t.Errorf("methods = %v, want generateContent + streamGenerateContent", out.Models[0].SupportedGenerationMethods)
	}
	// Deprecated rows come back with ?include=deprecated.
	rec = doGeminiReq(t, mux, http.MethodGet, "/v1beta/models?include=deprecated", "")
	var out2 geminiModelsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out2); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(out2.Models) != 2 {
		t.Errorf("models with include=deprecated = %d, want 2", len(out2.Models))
	}
	// No Gemini models in the catalog: an honest empty list.
	empty := newGeminiTestMux(t, &fakeEngine{}, &geminiTestRegistry{models: []provider.DashboardModel{
		{ID: "mock-openai-model", ProviderID: "mock-openai", ProviderName: "mock-openai", WireFormat: provider.WireFormatOpenAI},
	}})
	rec = doGeminiReq(t, empty, http.MethodGet, "/v1beta/models", "")
	var out3 geminiModelsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out3); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(out3.Models) != 0 {
		t.Errorf("models = %+v, want an empty list when the catalog has no Gemini models", out3.Models)
	}
	// No key: 401 (the auth wrapper composes).
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated status = %d, want 401", rec2.Code)
	}
}

func TestM813Dispatch(t *testing.T) {
	eng := &geminiRecordingEngine{resp: provider.ChatResponse{Content: "hi", Model: "mock-gemini-model", FinishReason: "stop"}}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

	rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:bogus", body)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown action status = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	rec = doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/:generateContent", body)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty model status = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	// The models/ prefix form ListModels returns is accepted.
	rec = doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/models/mock-gemini-model:generateContent", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("models/-prefixed status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if eng.lastReq.Model != "mock-gemini-model" {
		t.Errorf("engine model = %q, want the models/ prefix stripped", eng.lastReq.Model)
	}
	// Unmapped field: 400 naming the field, in the Gemini error
	// envelope (see TestM813PreEngineErrorsGeminiEnvelope).
	rec = doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":40}}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "generationConfig.topK") {
		t.Errorf("topK status = %d body = %s, want a 400 naming generationConfig.topK", rec.Code, rec.Body.String())
	}
	assertGeminiErrorEnvelope(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
	// thinkingBudget rides through to the canonical request.
	// (Caveat, review F7: a tier/group-pinned named effort plus
	// an operator Gemini budget overwrites the client's value
	// downstream — the passthrough holds only with no named
	// effort pinned; see geminiApplyGenerationConfig.)
	rec = doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"thinkingBudget":256}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("thinkingBudget status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if eng.lastReq.ThinkingBudget != 256 {
		t.Errorf("ThinkingBudget = %d, want 256", eng.lastReq.ThinkingBudget)
	}
}

func TestM813GenerateEngineRateLimit(t *testing.T) {
	eng := &fakeEngine{chatErr: &provider.RateLimitError{RetryAfter: 30 * time.Second}}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	var env geminiErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not a Gemini envelope: %v", err)
	}
	if env.Error.Status != "RESOURCE_EXHAUSTED" {
		t.Errorf("error status = %q, want RESOURCE_EXHAUSTED", env.Error.Status)
	}
}

// assertGeminiErrorEnvelope pins the error shape of one
// pre-engine / conversion failure: the body must parse as a
// Gemini envelope {"error": {"code", "message", "status"}} with
// the expected HTTP code and Gemini status, and must NOT carry
// the OpenAI-shaped "type" field. (Review F1: the slice's tests
// used to assert only status + message substring, so an
// OpenAI-shaped body shipped silently; this helper exists so
// that class of defect cannot recur.)
func assertGeminiErrorEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantGeminiStatus string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var env geminiErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not a Gemini envelope: %v (%s)", err, rec.Body.String())
	}
	if env.Error.Code != wantStatus {
		t.Errorf("envelope code = %d, want %d", env.Error.Code, wantStatus)
	}
	if env.Error.Status != wantGeminiStatus {
		t.Errorf("envelope status = %q, want %q", env.Error.Status, wantGeminiStatus)
	}
	if env.Error.Message == "" {
		t.Errorf("envelope message is empty (%s)", rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	errObj, _ := raw["error"].(map[string]any)
	if errObj == nil {
		t.Fatalf("body has no \"error\" object: %s", rec.Body.String())
	}
	if _, hasType := errObj["type"]; hasType {
		t.Errorf("error object carries an OpenAI-style \"type\" field, not a Gemini envelope: %s", rec.Body.String())
	}
}

// TestM813PreEngineErrorsGeminiEnvelope (review F1): every
// pre-engine rejection on this surface — validation 400s,
// unknown-action 404, engine-not-configured 503, and the
// response-conversion 502 — must answer in the Gemini error
// envelope, exactly like post-translation engine errors, not
// in the OpenAI envelope {"error": {"message", "type"}}.
func TestM813PreEngineErrorsGeminiEnvelope(t *testing.T) {
	validBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	topKBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"topK":40}}`

	t.Run("topK400", func(t *testing.T) {
		eng := &geminiRecordingEngine{resp: provider.ChatResponse{Content: "hi", Model: "mock-gemini-model", FinishReason: "stop"}}
		mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent", topKBody)
		assertGeminiErrorEnvelope(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
		if !strings.Contains(rec.Body.String(), "generationConfig.topK") {
			t.Errorf("topK body = %s, want it to name generationConfig.topK", rec.Body.String())
		}
	})
	t.Run("unknownAction404", func(t *testing.T) {
		eng := &geminiRecordingEngine{resp: provider.ChatResponse{Content: "hi", Model: "mock-gemini-model", FinishReason: "stop"}}
		mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:bogus", validBody)
		assertGeminiErrorEnvelope(t, rec, http.StatusNotFound, "NOT_FOUND")
	})
	t.Run("emptyModel400", func(t *testing.T) {
		eng := &geminiRecordingEngine{resp: provider.ChatResponse{Content: "hi", Model: "mock-gemini-model", FinishReason: "stop"}}
		mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/:generateContent", validBody)
		assertGeminiErrorEnvelope(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
	})
	t.Run("invalidJSON400", func(t *testing.T) {
		eng := &geminiRecordingEngine{resp: provider.ChatResponse{Content: "hi", Model: "mock-gemini-model", FinishReason: "stop"}}
		mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent", `{not json`)
		assertGeminiErrorEnvelope(t, rec, http.StatusBadRequest, "INVALID_ARGUMENT")
	})
	t.Run("engineNotConfigured503", func(t *testing.T) {
		mux := newGeminiTestMux(t, nil, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent", validBody)
		assertGeminiErrorEnvelope(t, rec, http.StatusServiceUnavailable, "UNAVAILABLE")
	})
	t.Run("responseConversion502", func(t *testing.T) {
		eng := &geminiRecordingEngine{resp: provider.ChatResponse{
			Content: "hi", Model: "mock-gemini-model", FinishReason: "tool_calls",
			ToolCalls: []provider.ToolCall{{ID: "call_0", Type: "function", Function: provider.ToolCallFunction{Name: "f", Arguments: "{invalid"}}},
		}}
		mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
		rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:generateContent", validBody)
		assertGeminiErrorEnvelope(t, rec, http.StatusBadGateway, "UNAVAILABLE")
	})
}

// parseGeminiSSEEvents splits a Gemini SSE body into its data
// payloads (one JSON object per event). No [DONE] sentinel exists
// on this surface: the stream simply ends.
func parseGeminiSSEEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	if strings.Contains(body, "[DONE]") {
		t.Errorf("SSE body contains a [DONE] sentinel, which Gemini streams never carry:\n%s", body)
	}
	var out []map[string]any
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		if !strings.HasPrefix(block, "data: ") {
			t.Fatalf("SSE block %q does not start with \"data: \"", block)
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(block, "data: ")), &ev); err != nil {
			t.Fatalf("SSE event is not JSON: %v (%q)", err, block)
		}
		out = append(out, ev)
	}
	return out
}

func TestM813StreamSSEShape(t *testing.T) {
	eng := &geminiChunkEngine{
		resp: provider.ChatResponse{
			Content: "Hello, mock human.", Model: "mock-gemini-model", FinishReason: "tool_calls",
			Usage:     provider.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12},
			ToolCalls: []provider.ToolCall{{ID: "call_0", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}}},
		},
		chunks: []provider.StreamChunk{
			{Content: "Hello, "},
			{Content: "mock human."},
		},
	}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	events := parseGeminiSSEEvents(t, rec.Body.String())
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (2 text deltas + terminal)", len(events))
	}
	textOf := func(ev map[string]any) string {
		cands, _ := ev["candidates"].([]any)
		if len(cands) == 0 {
			return ""
		}
		content, _ := cands[0].(map[string]any)["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 0 {
			return ""
		}
		s, _ := parts[0].(map[string]any)["text"].(string)
		return s
	}
	if got := textOf(events[0]); got != "Hello, " {
		t.Errorf("event 0 text = %q, want \"Hello, \"", got)
	}
	if got := textOf(events[1]); got != "mock human." {
		t.Errorf("event 1 text = %q, want \"mock human.\"", got)
	}
	terminal := events[2]
	if fr, _ := terminal["candidates"].([]any)[0].(map[string]any)["finishReason"].(string); fr != "TOOL_CALLS" {
		t.Errorf("terminal finishReason = %q, want TOOL_CALLS", fr)
	}
	usage, _ := terminal["usageMetadata"].(map[string]any)
	if usage["totalTokenCount"] != float64(12) {
		t.Errorf("terminal usage = %v, want totalTokenCount 12", usage)
	}
	cands, _ := terminal["candidates"].([]any)
	content, _ := cands[0].(map[string]any)["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	if len(parts) != 1 {
		t.Fatalf("terminal parts = %v, want the single completed functionCall", parts)
	}
	fc, _ := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"] != "get_weather" {
		t.Errorf("terminal functionCall = %v, want get_weather", fc)
	}
	// The terminal event must not repeat the streamed text.
	if got := textOf(terminal); got != "" {
		t.Errorf("terminal text = %q, want empty (text already streamed)", got)
	}
}

// TestM813StreamTextOnlyFallback covers engines without the
// chunk-aware half: text still streams, and the terminal event
// still carries the reassembled finish reason + usage.
func TestM813StreamTextOnlyFallback(t *testing.T) {
	eng := &fakeEngine{
		deltas:   []string{"Hello, ", "mock human."},
		chatResp: provider.ChatResponse{Content: "Hello, mock human.", Model: "mock-gemini-model", FinishReason: "stop", Usage: provider.Usage{PromptTokens: 7, CompletionTokens: 5, TotalTokens: 12}},
	}
	mux := newGeminiTestMux(t, eng, newGeminiTestRegistry())
	rec := doGeminiReq(t, mux, http.MethodPost, "/v1beta/models/mock-gemini-model:streamGenerateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	events := parseGeminiSSEEvents(t, rec.Body.String())
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3 (2 text deltas + terminal)", len(events))
	}
	terminal := events[2]
	if fr, _ := terminal["candidates"].([]any)[0].(map[string]any)["finishReason"].(string); fr != "STOP" {
		t.Errorf("terminal finishReason = %q, want STOP", fr)
	}
}
