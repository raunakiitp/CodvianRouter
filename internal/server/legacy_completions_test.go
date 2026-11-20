// M8.2 contract + unit tests: POST /v1/completions (legacy) and
// GET /v1/classify, /v1/explain/routing, /v1/segment.
//
// The four routes are NOT registered in the mux yet (registration
// is the coordinator's — server.go/guard.go are not this slice's
// files), so these tests call the handler methods directly via
// http.HandlerFunc, and the apitest fixture tests pass the method
// value as the handler under test. Auth is enforced by the
// register wrapper, not by the handlers, so the direct calls
// carry the test key for realism but do not depend on it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// legacyRecordingEngine is a test Engine that records the
// ChatRequest it received and returns a canned response (or err).
// It proves the legacy handler adapts the prompt into exactly one
// user message and calls the chat engine — if the Chat call were
// removed, the recording assertions fail.
type legacyRecordingEngine struct {
	lastReq provider.ChatRequest
	resp    provider.ChatResponse
	err     error
}

func (e *legacyRecordingEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.lastReq = req
	if e.err != nil {
		return nil, e.err
	}
	r := e.resp
	return &r, nil
}

func (e *legacyRecordingEngine) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return nil, errors.New("legacy completions never streams")
}

// callLegacyHandler invokes one M8.2 handler method directly (the
// routes are unregistered until the coordinator wires them) and
// returns the recorder. method/path/body are the request; the
// test key is sent for realism.
func callM82Handler(t *testing.T, handler func(http.ResponseWriter, *http.Request), method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key-123")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// mockAutoRouter builds the auto-router the fixture tests wire:
// DefaultModel mock-model, so every picked tier resolves to the
// mock model and no real model is ever named.
func mockAutoRouter() *autorouter.Router {
	return autorouter.New(autorouter.Config{DefaultModel: "mock-model"})
}

// chatRespFromRecorded converts a fixture's recorded chat
// completion body into the provider.ChatResponse the fake engine
// returns. The recorded body is mock data replayed by the test
// double — the handler under test never sees the HTTP form.
func chatRespFromRecorded(t *testing.T, f apitest.Fixture) provider.ChatResponse {
	t.Helper()
	raw, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
				Role    string `json:"role"`
			} `json:"message"`
		} `json:"choices"`
		Usage provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal recorded response: %v", err)
	}
	if len(body.Choices) == 0 {
		t.Fatal("recorded response has no choices")
	}
	return provider.ChatResponse{
		Content:      body.Choices[0].Message.Content,
		Model:        body.Model,
		Usage:        body.Usage,
		FinishReason: body.Choices[0].FinishReason,
	}
}

// ---------------------------------------------------------------------------
// POST /v1/completions
// ---------------------------------------------------------------------------

func TestLegacyCompletionsFixture200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "completions-200")
	eng := &fakeEngine{chatResp: chatRespFromRecorded(t, f)}
	srv := newTestServer(t, eng)
	apitest.Run(t, http.HandlerFunc(srv.handleLegacyCompletions), f)
}

func TestLegacyCompletionsPromptBecomesUserMessage(t *testing.T) {
	eng := &legacyRecordingEngine{resp: provider.ChatResponse{
		Content:      "Hello from the mock upstream.",
		Model:        "mock-model",
		Usage:        provider.Usage{PromptTokens: 4, CompletionTokens: 6, TotalTokens: 10},
		FinishReason: "stop",
	}}
	srv := newTestServer(t, eng)
	rec := callM82Handler(t, srv.handleLegacyCompletions, http.MethodPost, "/v1/completions",
		`{"model":"mock-model","prompt":"Say hello.","max_tokens":16,"temperature":0.5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	// The prompt reached the chat engine as exactly one user
	// message on the chat endpoint — the adaptation contract.
	if len(eng.lastReq.Messages) != 1 || eng.lastReq.Messages[0].Role != "user" || eng.lastReq.Messages[0].Content != "Say hello." {
		t.Fatalf("engine request messages = %+v, want one user message carrying the prompt", eng.lastReq.Messages)
	}
	if eng.lastReq.Endpoint != provider.EndpointChatCompletions {
		t.Fatalf("engine request endpoint = %q, want chat_completions (the legacy call runs on the chat engine)", eng.lastReq.Endpoint)
	}
	if eng.lastReq.Model != "mock-model" || eng.lastReq.MaxTokens != 16 || eng.lastReq.Temperature != 0.5 {
		t.Fatalf("engine request = model %q max_tokens %d temperature %v, want mock-model/16/0.5", eng.lastReq.Model, eng.lastReq.MaxTokens, eng.lastReq.Temperature)
	}
	var out legacyCompletionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	if out.Object != "text_completion" || !strings.HasPrefix(out.ID, "cmpl-") || out.Created == 0 {
		t.Fatalf("response identity = object %q id %q created %d, want text_completion / cmpl-* / nonzero", out.Object, out.ID, out.Created)
	}
	if out.Model != "mock-model" || len(out.Choices) != 1 || out.Choices[0].Text != "Hello from the mock upstream." || out.Choices[0].Index != 0 {
		t.Fatalf("response = %+v, want one choice with the engine text", out)
	}
	if out.Choices[0].FinishReason == nil || *out.Choices[0].FinishReason != "stop" {
		t.Fatalf("choices[0].finish_reason = %+v, want stop", out.Choices[0].FinishReason)
	}
	if out.Choices[0].Logprobs != nil {
		t.Fatalf("choices[0].logprobs = %v, want null (the engine reports no logprobs)", out.Choices[0].Logprobs)
	}
	if out.Usage.TotalTokens != 10 || out.Usage.PromptTokens != 4 || out.Usage.CompletionTokens != 6 {
		t.Fatalf("usage = %+v, want 4/6/10 from the engine", out.Usage)
	}
	// The surface header names the legacy surface, and the
	// decision header names the serving model.
	if got := rec.Header().Get("X-Codvian-Endpoint"); got != "completions" {
		t.Fatalf("X-Codvian-Endpoint = %q, want completions", got)
	}
}

func TestLegacyCompletionsValidation(t *testing.T) {
	srv := newTestServer(t, &legacyRecordingEngine{resp: provider.ChatResponse{Content: "x", Model: "mock-model"}})
	cases := []struct {
		name string
		body string
		want string // substring the 400 must name
	}{
		{"empty body", ``, "invalid request body"},
		{"bad json", `{not json`, "invalid request body"},
		{"missing model", `{"prompt":"hi"}`, "model is required"},
		{"missing prompt", `{"model":"mock-model"}`, "prompt is required"},
		{"empty prompt", `{"model":"mock-model","prompt":"  "}`, "prompt must not be empty"},
		{"token-id prompt", `{"model":"mock-model","prompt":[1,2,3]}`, "token-id prompts are not supported"},
		{"multi prompt", `{"model":"mock-model","prompt":["a","b"]}`, "multiple prompts"},
		{"stream", `{"model":"mock-model","prompt":"hi","stream":true}`, "streaming is not supported"},
		{"n=2", `{"model":"mock-model","prompt":"hi","n":2}`, "n=2 is not supported"},
		{"best_of", `{"model":"mock-model","prompt":"hi","best_of":3}`, "best_of=3 is not supported"},
		{"echo", `{"model":"mock-model","prompt":"hi","echo":true}`, "echo is not supported"},
		{"suffix", `{"model":"mock-model","prompt":"hi","suffix":"end"}`, "suffix (insert mode) is not supported"},
		{"logprobs", `{"model":"mock-model","prompt":"hi","logprobs":2}`, "logprobs is not supported"},
		{"presence penalty", `{"model":"mock-model","prompt":"hi","presence_penalty":0.5}`, "presence_penalty is not supported"},
		{"frequency penalty", `{"model":"mock-model","prompt":"hi","frequency_penalty":-0.5}`, "frequency_penalty is not supported"},
		{"negative max_tokens", `{"model":"mock-model","prompt":"hi","max_tokens":-1}`, "max_tokens must not be negative"},
		{"bad stop", `{"model":"mock-model","prompt":"hi","stop":42}`, "stop must be a string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := callM82Handler(t, srv.handleLegacyCompletions, http.MethodPost, "/v1/completions", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %s does not name %q", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestLegacyCompletionsSingleElementPromptArray(t *testing.T) {
	eng := &legacyRecordingEngine{resp: provider.ChatResponse{Content: "x", Model: "mock-model"}}
	srv := newTestServer(t, eng)
	rec := callM82Handler(t, srv.handleLegacyCompletions, http.MethodPost, "/v1/completions",
		`{"model":"mock-model","prompt":["Say hello."],"stop":"\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if len(eng.lastReq.Stop) != 1 || eng.lastReq.Stop[0] != "\n" {
		t.Fatalf("engine stop = %v, want [\"\\n\"]", eng.lastReq.Stop)
	}
}

func TestLegacyCompletionsMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &legacyRecordingEngine{})
	rec := callM82Handler(t, srv.handleLegacyCompletions, http.MethodGet, "/v1/completions", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestLegacyCompletionsEngineError(t *testing.T) {
	// Proves the handler really calls the engine: with the Chat
	// call removed this test would see a 200 instead of the
	// engine error mapped to 502.
	eng := &legacyRecordingEngine{err: errors.New("mock upstream failure")}
	srv := newTestServer(t, eng)
	rec := callM82Handler(t, srv.handleLegacyCompletions, http.MethodPost, "/v1/completions",
		`{"model":"mock-model","prompt":"Say hello."}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestLegacyCompletionsToggleWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "completions-200")
	f.Assert.Status = 201 // wrong on purpose: the response is 200
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(map[string]any{"object": "text_completion"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 201")
	}
}

func TestLegacyCompletionsToggleWrongFieldFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "completions-200")
	f.Assert.JSONPaths["object"] = "chat.completion" // wrong on purpose
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body, err := json.Marshal(map[string]any{"object": "text_completion", "choices": []any{}, "id": "cmpl-x", "usage": map[string]any{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted object=chat.completion against assert object=text_completion")
	}
}

// ---------------------------------------------------------------------------
// GET /v1/classify
// ---------------------------------------------------------------------------

func TestClassifyFixture200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "classify-200")
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	apitest.Run(t, http.HandlerFunc(srv.handleClassify), f)
}

func TestClassifyValidation(t *testing.T) {
	// No auto-router: honest 503, never a faked decision.
	srv := newTestServer(t, &fakeEngine{})
	rec := callM82Handler(t, srv.handleClassify, http.MethodGet, "/v1/classify?prompt=hello", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-router status = %d, want 503", rec.Code)
	}
	srv = newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	for _, tc := range []struct {
		name string
		path string
		want int
		sub  string
	}{
		{"missing prompt", "/v1/classify", http.StatusBadRequest, "prompt is required"},
		{"unknown virtual model", "/v1/classify?model=gpt-4&prompt=hello", http.StatusBadRequest, "unknown virtual model"},
		{"bad threshold", "/v1/classify?prompt=hello&threshold=2", http.StatusBadRequest, "threshold must be a number"},
		{"post not allowed", "/v1/classify?prompt=hello", http.StatusMethodNotAllowed, "method not allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := http.MethodGet
			if tc.name == "post not allowed" {
				method = http.MethodPost
			}
			rec := callM82Handler(t, srv.handleClassify, method, tc.path, "")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.want, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.sub) {
				t.Fatalf("body %s does not contain %q", rec.Body.String(), tc.sub)
			}
		})
	}
}

func TestClassifyReturnsRealDecision(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	rec := callM82Handler(t, srv.handleClassify, http.MethodGet,
		"/v1/classify?model=auto&prompt="+url.QueryEscape("Write a function that sorts a list."), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var dec struct {
		VirtualModel string             `json:"virtual_model"`
		Tier         string             `json:"tier"`
		Scores       map[string]float32 `json:"scores"`
		Model        string             `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.VirtualModel != "auto" || dec.Model != "mock-model" || len(dec.Scores) == 0 {
		t.Fatalf("decision = %+v, want virtual_model auto, model mock-model, non-empty scores", dec)
	}
}

func TestClassifyUnresolvedTierIs502WithDecision(t *testing.T) {
	// An auto-router with no DefaultModel and no tier models
	// classifies but cannot resolve: 502 with the decision
	// attached, never an invented model.
	ar := autorouter.New(autorouter.Config{})
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(ar))
	rec := callM82Handler(t, srv.handleClassify, http.MethodGet,
		"/v1/classify?model=auto&prompt="+url.QueryEscape("Write a function."), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	var dec struct {
		VirtualModel string `json:"virtual_model"`
		Model        string `json:"model"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Model != "" || !strings.Contains(dec.Reason, "no concrete model") {
		t.Fatalf("unresolved decision = %+v, want empty model and a reason naming the missing model", dec)
	}
}

func TestClassifyToggleWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "classify-200")
	f.Assert.Status = 404 // wrong on purpose
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := apitest.CheckResponse(resp, []byte(`{"virtual_model":"auto","tier":"coding","scores":{},"model":"mock-model"}`), f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 404")
	}
}

// ---------------------------------------------------------------------------
// GET /v1/explain/routing
// ---------------------------------------------------------------------------

func TestExplainRoutingFixture200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "explain-routing-200")
	srv := newTestServer(t, &fakeEngine{},
		WithAutoRouter(mockAutoRouter()),
		WithConfig(&config.Config{Routing: &config.RoutingConfig{GlobalStrategy: "balanced"}}))
	apitest.Run(t, http.HandlerFunc(srv.handleExplainRouting), f)
}

func TestExplainRoutingNeverCallsUpstream(t *testing.T) {
	// The explain contract: no upstream call is possible — the
	// handler has no engine call on any path — and the response
	// says so explicitly. No router (503) and no strategy
	// configured (null strategy) are both honest states.
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	rec := callM82Handler(t, srv.handleExplainRouting, http.MethodGet,
		"/v1/explain/routing?model=auto&prompt="+url.QueryEscape("Write a function."), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out struct {
		WouldCallUpstream bool    `json:"would_call_upstream"`
		UpstreamCalls     int     `json:"upstream_calls"`
		Strategy          *string `json:"strategy"`
		StrategySource    string  `json:"strategy_source"`
		LastServed        struct {
			HasDecision bool `json:"has_decision"`
		} `json:"last_served"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.WouldCallUpstream || out.UpstreamCalls != 0 {
		t.Fatalf("explain = %+v, want would_call_upstream false / upstream_calls 0", out)
	}
	if out.Strategy != nil || out.StrategySource != "none configured" {
		t.Fatalf("explain strategy = %v source %q, want null / none configured (no config wired)", out.Strategy, out.StrategySource)
	}
	if out.LastServed.HasDecision {
		t.Fatal("last_served.has_decision is true before any request was served")
	}
	// With a configured global strategy the explanation names
	// it and its reason, from the config — never a guess.
	srv2 := newTestServer(t, &fakeEngine{},
		WithAutoRouter(mockAutoRouter()),
		WithConfig(&config.Config{Routing: &config.RoutingConfig{GlobalStrategy: "cheapest"}}))
	rec2 := callM82Handler(t, srv2.handleExplainRouting, http.MethodGet,
		"/v1/explain/routing?model=auto&prompt="+url.QueryEscape("Write a function."), "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
	}
	var out2 struct {
		Strategy       *string `json:"strategy"`
		StrategyReason *string `json:"strategy_reason"`
		StrategySource string  `json:"strategy_source"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out2.Strategy == nil || *out2.Strategy != "cheapest" || out2.StrategyReason == nil || *out2.StrategyReason != "cheapest" {
		t.Fatalf("explain strategy = %+v, want cheapest/cheapest", out2)
	}
	if out2.StrategySource != "config routing.global_strategy" {
		t.Fatalf("strategy_source = %q, want config routing.global_strategy", out2.StrategySource)
	}
}

func TestExplainRoutingToggleWrongFieldFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "explain-routing-200")
	f.Assert.JSONPaths["would_call_upstream"] = true // wrong on purpose
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	body := []byte(`{"decision":{"virtual_model":"auto","model":"mock-model"},"would_call_upstream":false,"upstream_calls":0,"strategy":"balanced","strategy_reason":"balanced","strategy_source":"config routing.global_strategy","last_served":{"has_decision":false,"attempts":[]}}`)
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Fatal("CheckResponse accepted would_call_upstream=true against assert false")
	}
}

// ---------------------------------------------------------------------------
// GET /v1/segment
// ---------------------------------------------------------------------------

func TestSegmentFixture200(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "segment-200")
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	apitest.Run(t, http.HandlerFunc(srv.handleSegment), f)
}

func TestSegmentSplitsParagraphsAndCaps(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(mockAutoRouter()))
	// Paragraphs + sentences: 2 paragraphs, 3 sentences.
	prompt := "First sentence. Second sentence!\n\nThird sentence?"
	rec := callM82Handler(t, srv.handleSegment, http.MethodGet,
		"/v1/segment?model=auto&prompt="+url.QueryEscape(prompt), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out segmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SegmentCount != 3 || out.Truncated || len(out.Segments) != 3 {
		t.Fatalf("segments = %+v, want 3 untruncated", out)
	}
	if out.Segments[0].Text != "First sentence." || out.Segments[1].Text != "Second sentence!" || out.Segments[2].Text != "Third sentence?" {
		t.Fatalf("segment texts = %q %q %q", out.Segments[0].Text, out.Segments[1].Text, out.Segments[2].Text)
	}
	for i, seg := range out.Segments {
		if seg.Index != i || !seg.Resolved || seg.Decision.Model != "mock-model" {
			t.Fatalf("segment %d = %+v, want resolved with model mock-model", i, seg)
		}
	}
	if out.WouldCallUpstream || out.UpstreamCalls != 0 {
		t.Fatalf("segment response claims upstream use: %+v", out)
	}
	// 101 sentences cap at 100 with truncated=true.
	var parts []string
	for i := 1; i <= 101; i++ {
		parts = append(parts, fmt.Sprintf("Item %d is here.", i))
	}
	rec = callM82Handler(t, srv.handleSegment, http.MethodGet,
		"/v1/segment?model=auto&prompt="+url.QueryEscape(strings.Join(parts, " ")), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("cap status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var capped segmentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &capped); err != nil {
		t.Fatalf("decode capped: %v", err)
	}
	if capped.SegmentCount != 100 || !capped.Truncated {
		t.Fatalf("capped = count %d truncated %v, want 100/true", capped.SegmentCount, capped.Truncated)
	}
	// Validation: missing prompt 400, no router 503.
	rec = callM82Handler(t, srv.handleSegment, http.MethodGet, "/v1/segment", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-prompt status = %d, want 400", rec.Code)
	}
	srvNoRouter := newTestServer(t, &fakeEngine{})
	rec = callM82Handler(t, srvNoRouter.handleSegment, http.MethodGet, "/v1/segment?prompt=hello", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-router status = %d, want 503", rec.Code)
	}
}

func TestSplitPromptSegments(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Hello world", []string{"Hello world"}},
		{"Version 1.2 shipped today. Really!", []string{"Version 1.2 shipped today.", "Really!"}},
		{"One.\n\nTwo?\nThree!", []string{"One.", "Two?", "Three!"}},
		{"  Spaced.   Out.  ", []string{"Spaced.", "Out."}},
	}
	for _, tc := range cases {
		got := splitPromptSegments(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("splitPromptSegments(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("splitPromptSegments(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestSegmentToggleWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.2", "segment-200")
	f.Assert.Status = 500 // wrong on purpose
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := apitest.CheckResponse(resp, []byte(`{"model":"auto","segments":[],"segment_count":0,"truncated":false,"would_call_upstream":false,"upstream_calls":0}`), f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 500")
	}
}
