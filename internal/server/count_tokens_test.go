// M8.3 token-count endpoints (Area 14 §14a): POST
// /v1/messages/count_tokens and GET /v1/responses/input_tokens.
//
// Both handlers are pure local counting (tokencount.CountPromptWire)
// — no engine call, no upstream call. The fixture contract tests
// run the real handlers through the apitest harness; the direct
// tests pin the behaviours the fixtures cannot: array-form content
// counting once (M1.9), tool_use / tool-definition counting,
// validation errors, and the input-side rule for stored
// transcripts (everything except the trailing assistant turn).
//
// Registration is the coordinator's job (guard table + mux in
// server.go/guard.go, which this slice must not touch): the
// handlers below are exercised directly, exactly as the
// unregistered handlers of sibling M8 slices are in their tests.
// Proposed guard entries (TierManagement, gateway-key auth):
//
//	POST /v1/messages/count_tokens — scope "chat"
//	GET  /v1/responses/input_tokens — scope "chat"
//
// One scope per route is proposed; the coordinator assigns at
// registration.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/responses"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// countTokensBody decodes a token-count response body.
type countTokensBody struct {
	InputTokens      int    `json:"input_tokens"`
	Model            string `json:"model"`
	Encoding         string `json:"encoding"`
	Estimated        bool   `json:"estimated"`
	EncodingFallback bool   `json:"encoding_fallback"`
	ResponseID       string `json:"response_id,omitempty"`
	MessagesCounted  int    `json:"messages_counted,omitempty"`
}

// callCountTokens calls one of the two handlers directly and
// returns the status code plus the decoded body.
func callCountTokens(t *testing.T, srv *Server, method, path, body string) (int, countTokensBody) {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	switch {
	case strings.HasPrefix(path, "/v1/messages/count_tokens"):
		srv.handleMessagesCountTokens(rec, req)
	case strings.HasPrefix(path, "/v1/responses/input_tokens"):
		srv.handleResponsesInputTokens(rec, req)
	default:
		t.Fatalf("callCountTokens: unknown path %q", path)
	}
	var out countTokensBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("callCountTokens %s %s: body is not JSON: %v (body: %.200s)", method, path, err, rec.Body.String())
	}
	return rec.Code, out
}

// seedStoredResponse stores one Responses record with the given
// transcript (user + assistant turns, in order) under id.
func seedStoredResponse(t *testing.T, srv *Server, id, model string, transcript []provider.Message) {
	t.Helper()
	raw, err := json.Marshal(transcript)
	if err != nil {
		t.Fatalf("seedStoredResponse: marshal transcript: %v", err)
	}
	srv.responses.Put(responses.Record{
		ID:             id,
		Model:          model,
		Status:         responses.StatusCompleted,
		TranscriptJSON: raw,
	})
}

// ---------------------------------------------------------------------------
// Fixture contract tests (apitest harness).
// ---------------------------------------------------------------------------

func TestM83MessagesCountTokensFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.3", "messages-count-tokens-200")
	apitest.Run(t, http.HandlerFunc(srv.handleMessagesCountTokens), f)
}

func TestM83MessagesCountTokensStringContentFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.3", "messages-count-tokens-string-content-200")
	apitest.Run(t, http.HandlerFunc(srv.handleMessagesCountTokens), f)
}

func TestM83MessagesCountTokens400Fixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.3", "messages-count-tokens-400")
	apitest.Run(t, http.HandlerFunc(srv.handleMessagesCountTokens), f)
}

func TestM83ResponsesInputTokensFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedStoredResponse(t, srv, "resp_m8_3_fixture_01", "mock-model", []provider.Message{
		{Role: "user", Content: "What is the capital of France?"},
		{Role: "assistant", Content: "Paris."},
	})
	f := apitest.LoadFixture(t, "m8.3", "responses-input-tokens-200")
	apitest.Run(t, http.HandlerFunc(srv.handleResponsesInputTokens), f)
}

func TestM83ResponsesInputTokens404Fixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.3", "responses-input-tokens-404")
	apitest.Run(t, http.HandlerFunc(srv.handleResponsesInputTokens), f)
}

// TestM83FixtureWrongCountFails is the toggle proof for the
// fixture assertion path: the real handler's 200 response must
// FAIL CheckResponse when the fixture expects a different token
// count. A test that passed against any count would prove nothing.
func TestM83FixtureWrongCountFails(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	f := apitest.LoadFixture(t, "m8.3", "messages-count-tokens-200")
	raw, err := json.Marshal(f.Request.Body)
	if err != nil {
		t.Fatalf("marshal fixture request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.handleMessagesCountTokens(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("handler status = %d, want 200 (body: %.200s)", rec.Code, rec.Body.String())
	}
	resp := &http.Response{StatusCode: rec.Code, Header: http.Header{}}
	f.Assert.JSONPaths["input_tokens"] = 999999 // wrong on purpose
	if err := apitest.CheckResponse(resp, rec.Body.Bytes(), f); err == nil {
		t.Fatal("CheckResponse accepted a wrong input_tokens count")
	} else if !strings.Contains(err.Error(), "input_tokens") {
		t.Fatalf("wrong-count error should name input_tokens, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Direct behaviour tests.
// ---------------------------------------------------------------------------

// TestM83ArrayContentCountsOnce (M1.9): the same text sent as a
// one-part array must count exactly the same as string form.
// If the translator or the counter added Content + Parts, the
// array form would count ~2x.
func TestM83ArrayContentCountsOnce(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	_, array := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"Hello, world!"}]}]}`)
	_, str := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"Hello, world!"}]}`)
	if array.InputTokens != str.InputTokens {
		t.Fatalf("array form counted %d tokens, string form %d — array content must count once", array.InputTokens, str.InputTokens)
	}
	if array.InputTokens != 5 {
		t.Fatalf("input_tokens = %d, want 5 (4 text tokens + 1 Anthropic framing token)", array.InputTokens)
	}
}

// TestM83ArrayMultiPartCountsEachPartOnce: two text parts count
// as their concatenated text, not doubled.
func TestM83ArrayMultiPartCountsEachPartOnce(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	_, array := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"Hello, world!"},{"type":"text","text":" How are you?"}]}]}`)
	_, concat := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"Hello, world! How are you?"}]}`)
	if array.InputTokens != concat.InputTokens {
		t.Fatalf("multi-part array counted %d, concatenated string %d", array.InputTokens, concat.InputTokens)
	}
}

// TestM83ToolUseAndToolDefinitionsCounted: tool_use input
// (assistant tool-call arguments) and tool definitions both
// contribute to the count — the wire bills both as input tokens.
func TestM83ToolUseAndToolDefinitionsCounted(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	_, noTools := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"What is the weather in Pune?"}]}`)
	_, withCall := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"What is the weather in Pune?"},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Pune"}}]}]}`)
	if withCall.InputTokens != 15 {
		t.Fatalf("tool_use count = %d, want 15", withCall.InputTokens)
	}
	if withCall.InputTokens <= noTools.InputTokens {
		t.Fatalf("tool_use call added no tokens (%d vs %d without)", withCall.InputTokens, noTools.InputTokens)
	}
	_, withDefs := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"Hi"}],"tools":[{"name":"get_weather","description":"Get the weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}]}`)
	if withDefs.InputTokens != 38 {
		t.Fatalf("tool-definition count = %d, want 38", withDefs.InputTokens)
	}
}

// TestM83EstimatedFlag: a matched encoding on an exactly encoded
// prompt reports estimated=false; an unknown model reports
// estimated=true with encoding_fallback=true.
func TestM83EstimatedFlag(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	_, exact := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"gpt-4o","messages":[{"role":"user","content":"Hi"}]}`)
	if exact.Estimated || exact.EncodingFallback || exact.Encoding != "o200k_base" {
		t.Fatalf("gpt-4o: estimated=%v fallback=%v encoding=%q, want false/false/o200k_base", exact.Estimated, exact.EncodingFallback, exact.Encoding)
	}
	_, est := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens",
		`{"model":"mock-model","messages":[{"role":"user","content":"Hi"}]}`)
	if !est.Estimated || !est.EncodingFallback || est.Encoding != "cl100k_base" {
		t.Fatalf("mock-model: estimated=%v fallback=%v encoding=%q, want true/true/cl100k_base", est.Estimated, est.EncodingFallback, est.Encoding)
	}
}

// TestM83MessagesCountValidation pins the 400s: missing model,
// missing messages, an unknown role, and a tool_use block on a
// user message. A wrong method is a 405.
func TestM83MessagesCountValidation(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing model", `{"messages":[{"role":"user","content":"Hi"}]}`},
		{"missing messages", `{"model":"gpt-4o"}`},
		{"empty messages", `{"model":"gpt-4o","messages":[]}`},
		{"bad role", `{"model":"gpt-4o","messages":[{"role":"system","content":"Hi"}]}`},
		{"tool_use on user", `{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"tool_use","id":"x","name":"f","input":{}}]}]}`},
		{"tool_result on assistant", `{"model":"gpt-4o","messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"x","content":"ok"}]}]}`},
		{"unnamed tool", `{"model":"gpt-4o","messages":[{"role":"user","content":"Hi"}],"tools":[{"description":"no name"}]}`},
	} {
		code, _ := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens", tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/messages/count_tokens", nil)
	rec := httptest.NewRecorder()
	srv.handleMessagesCountTokens(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET on count_tokens: status = %d, want 405", rec.Code)
	}
}

// TestM83MessagesCountBadJSON is a 400, never a 500 or a count.
func TestM83MessagesCountBadJSON(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	code, _ := callCountTokens(t, srv, "POST", "/v1/messages/count_tokens", `{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status = %d, want 400", code)
	}
}

// TestM83ResponsesInputExcludesAssistantTurn: a multi-turn
// transcript counts every turn except the trailing assistant
// turn — earlier assistant turns were sent as context, so they
// stay counted.
func TestM83ResponsesInputExcludesAssistantTurn(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedStoredResponse(t, srv, "resp_m8_3_multi", "mock-model", []provider.Message{
		{Role: "user", Content: "What is the capital of France?"},
		{Role: "assistant", Content: "Paris."},
		{Role: "user", Content: "And of Germany?"},
		{Role: "assistant", Content: "Berlin."},
	})
	code, out := callCountTokens(t, srv, "GET", "/v1/responses/input_tokens?response_id=resp_m8_3_multi", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.MessagesCounted != 3 {
		t.Fatalf("messages_counted = %d, want 3 (all but the trailing assistant turn)", out.MessagesCounted)
	}
	// The recount must equal CountPromptWire over exactly the
	// input side — the same counter, the same framing.
	want, err := tokencount.CountPromptWire("mock-model", provider.WireFormatOpenAI, provider.ChatRequest{
		Model: "mock-model",
		Messages: []provider.Message{
			{Role: "user", Content: "What is the capital of France?"},
			{Role: "assistant", Content: "Paris."},
			{Role: "user", Content: "And of Germany?"},
		},
	})
	if err != nil {
		t.Fatalf("CountPromptWire: %v", err)
	}
	if out.InputTokens != want.Tokens {
		t.Fatalf("input_tokens = %d, want %d (CountPromptWire over the input side)", out.InputTokens, want.Tokens)
	}
	// And it must NOT equal the count over the whole transcript
	// (which would include this response's own output).
	whole, err := tokencount.CountPromptWire("mock-model", provider.WireFormatOpenAI, provider.ChatRequest{
		Model: "mock-model",
		Messages: []provider.Message{
			{Role: "user", Content: "What is the capital of France?"},
			{Role: "assistant", Content: "Paris."},
			{Role: "user", Content: "And of Germany?"},
			{Role: "assistant", Content: "Berlin."},
		},
	})
	if err != nil {
		t.Fatalf("CountPromptWire (whole): %v", err)
	}
	if out.InputTokens == whole.Tokens {
		t.Fatalf("input_tokens %d equals the whole-transcript count — the trailing assistant turn was not excluded", out.InputTokens)
	}
}

// TestM83ResponsesInputMissingParamAndMethod: no id is a 400
// (both spellings missing); a POST is a 405; the `id` alias works.
func TestM83ResponsesInputMissingParamAndMethod(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	seedStoredResponse(t, srv, "resp_m8_3_alias", "mock-model", []provider.Message{
		{Role: "user", Content: "Hello."},
		{Role: "assistant", Content: "Hi."},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/input_tokens", nil)
	rec := httptest.NewRecorder()
	srv.handleResponsesInputTokens(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no response_id: status = %d, want 400", rec.Code)
	}
	code, out := callCountTokens(t, srv, "GET", "/v1/responses/input_tokens?id=resp_m8_3_alias", "")
	if code != http.StatusOK {
		t.Fatalf("id alias: status = %d, want 200", code)
	}
	if out.ResponseID != "resp_m8_3_alias" {
		t.Fatalf("id alias: response_id = %q, want resp_m8_3_alias", out.ResponseID)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens?response_id=resp_m8_3_alias", nil)
	rec = httptest.NewRecorder()
	srv.handleResponsesInputTokens(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on input_tokens: status = %d, want 405", rec.Code)
	}
}

// TestM83ResponsesInputUnknownID404: an id that was never stored
// is an honest 404, never a synthesised count.
func TestM83ResponsesInputUnknownID404(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/input_tokens?response_id=resp_never_stored", nil)
	rec := httptest.NewRecorder()
	srv.handleResponsesInputTokens(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id: status = %d, want 404 (body: %.200s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "resp_never_stored") {
		t.Fatalf("404 body should name the id, got: %.200s", rec.Body.String())
	}
}
