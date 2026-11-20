package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// captureEngine is a controllable Engine for Responses tests: it
// records every request it receives (so a continuation's
// transcript can be asserted) and returns scripted content or
// stream deltas. It implements chunkEngine so the Responses
// stream path takes the chunk-aware branch.
type captureEngine struct {
	mu       sync.Mutex
	reqs     []provider.ChatRequest
	resp     provider.ChatResponse
	err      error
	deltas   []string
	chunkErr error
}

func (e *captureEngine) lastReq() provider.ChatRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.reqs) == 0 {
		return provider.ChatRequest{}
	}
	return e.reqs[len(e.reqs)-1]
}

func (e *captureEngine) reqCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.reqs)
}

func (e *captureEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	resp, err := e.resp, e.err
	e.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := resp
	return &out, nil
}

func (e *captureEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	deltas, err := append([]string(nil), e.deltas...), e.chunkErr
	resp := e.resp
	e.mu.Unlock()
	for _, d := range deltas {
		if err := onDelta(d); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	out := resp
	return &out, nil
}

func (e *captureEngine) ChatStreamChunks(_ context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	deltas, err := append([]string(nil), e.deltas...), e.chunkErr
	resp := e.resp
	e.mu.Unlock()
	for _, d := range deltas {
		if err := onChunk(provider.StreamChunk{Content: d}); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, err
	}
	out := resp
	return &out, nil
}

func TestDecodeResponsesRequest(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
		wantMsg []provider.Message
	}{
		{
			name:    "string input",
			body:    `{"model":"m","input":"hello"}`,
			wantMsg: []provider.Message{{Role: "user", Content: "hello"}},
		},
		{
			name:    "array message items",
			body:    `{"model":"m","input":[{"role":"system","content":"be brief"},{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			wantMsg: []provider.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}},
		},
		{
			name:    "message item type",
			body:    `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`,
			wantMsg: []provider.Message{{Role: "user", Content: "hi"}},
		},
		{
			name: "function call items round-trip",
			body: `{"model":"m","input":[{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"sunny"}]}`,
			wantMsg: []provider.Message{
				{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: "{}"}}}},
				{Role: "tool", Content: "sunny", ToolCallID: "call_1"},
			},
		},
		{name: "missing model", body: `{"input":"hi"}`, wantErr: "model is required"},
		{name: "missing input", body: `{"model":"m"}`, wantErr: "input is required"},
		{name: "empty string input", body: `{"model":"m","input":"  "}`, wantErr: "input is required"},
		{name: "empty array input", body: `{"model":"m","input":[]}`, wantErr: "input is required"},
		{name: "number input", body: `{"model":"m","input":42}`, wantErr: "input must be a string or an array of items"},
		{name: "bad role", body: `{"model":"m","input":[{"role":"wizard","content":"hi"}]}`, wantErr: "input[0].role must be one of system, developer, user, assistant"},
		{name: "unknown item type", body: `{"model":"m","input":[{"type":"image","content":"x"}]}`, wantErr: `input[0]: unsupported item type "image"`},
		{name: "unknown content part", body: `{"model":"m","input":[{"role":"user","content":[{"type":"image_url","text":"x"}]}]}`, wantErr: `input[0].content: unsupported content part type "image_url"`},
		{name: "function call without call id", body: `{"model":"m","input":[{"type":"function_call","name":"f"}]}`, wantErr: "input[0]: function_call items need call_id and name"},
		{name: "negative max output tokens", body: `{"model":"m","input":"hi","max_output_tokens":-1}`, wantErr: "max_output_tokens must not be negative"},
		{name: "unsupported tool type", body: `{"model":"m","input":"hi","tools":[{"type":"web_search","name":"x"}]}`, wantErr: `unsupported tool type "web_search" (only function tools are supported)`},
		{name: "tool without name", body: `{"model":"m","input":"hi","tools":[{"type":"function"}]}`, wantErr: "tools[].name is required"},
		{name: "bad tool choice string", body: `{"model":"m","input":"hi","tool_choice":"sometimes"}`, wantErr: `unsupported tool_choice "sometimes" (want auto, none, required, or a function object)`},
		{name: "unknown routing key", body: `{"model":"m","input":"hi","routing":{"excludes":["a"]}}`, wantErr: `invalid routing: unknown field "excludes" (known fields: order, only, exclude, sort, max_price, data_collection)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, msgs, errMsg := decodeResponsesRequest([]byte(tc.body))
			if tc.wantErr != "" {
				if errMsg != tc.wantErr {
					t.Fatalf("err = %q, want %q", errMsg, tc.wantErr)
				}
				return
			}
			if errMsg != "" {
				t.Fatalf("err = %q, want none", errMsg)
			}
			if len(msgs) != len(tc.wantMsg) {
				t.Fatalf("messages = %+v, want %+v", msgs, tc.wantMsg)
			}
			for i := range msgs {
				if msgs[i].Role != tc.wantMsg[i].Role || msgs[i].Content != tc.wantMsg[i].Content {
					t.Fatalf("messages[%d] = %+v, want %+v", i, msgs[i], tc.wantMsg[i])
				}
			}
			_ = req
		})
	}
}

func TestResponsesChatRequestTranslation(t *testing.T) {
	req := responsesRequest{
		Model:        "m",
		Instructions: "be brief",
		Temperature:  0.7,
		Tools:        []responsesTool{{Type: "function", Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice:   json.RawMessage(`{"type":"function","name":"get_weather"}`),
	}
	base := []provider.Message{{Role: "user", Content: "first"}, {Role: "assistant", Content: "sunny"}}
	input := []provider.Message{{Role: "user", Content: "and tomorrow?"}}
	chat := responsesChatRequest(req, input, base, "prov_9")
	if chat.Endpoint != provider.EndpointResponses {
		t.Fatalf("Endpoint = %q, want responses", chat.Endpoint)
	}
	if chat.Model != "m" || chat.Temperature != 0.7 {
		t.Fatalf("chat = %+v, want model m temperature 0.7", chat)
	}
	if len(chat.Messages) != 4 || chat.Messages[0].Role != "system" || chat.Messages[0].Content != "be brief" {
		t.Fatalf("messages = %+v, want instructions first, then base, then input", chat.Messages)
	}
	if chat.PinProviderID != "prov_9" {
		t.Fatalf("PinProviderID = %q, want prov_9", chat.PinProviderID)
	}
	if len(chat.Tools) != 1 || chat.Tools[0].Function.Name != "get_weather" {
		t.Fatalf("tools = %+v, want the function tool", chat.Tools)
	}
	if got := string(chat.ToolChoice); got != `{"type":"function","function":{"name":"get_weather"}}` {
		t.Fatalf("tool_choice = %s, want the chat function shape", got)
	}
	// A continuation carries the id; a fresh create does not.
	cont := responsesRequest{Model: "m", PreviousResponseID: "resp_1"}
	c2 := responsesChatRequest(cont, input, base, "")
	if c2.PreviousResponseID != "resp_1" {
		t.Fatalf("PreviousResponseID = %q, want resp_1", c2.PreviousResponseID)
	}
}

func TestBuildResponsesObject(t *testing.T) {
	req := responsesRequest{Model: "m", Metadata: map[string]string{"k": "v"}}
	resp := &provider.ChatResponse{
		Content:      "hi there",
		Model:        "m",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, CachedTokens: 4},
		ToolCalls: []provider.ToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`},
		}},
	}
	obj, assistant := buildResponsesObject("resp_1", "msg_1", req, resp)
	if obj.ID != "resp_1" || obj.Object != "response" || obj.Status != "completed" {
		t.Fatalf("obj = %+v, want id resp_1 object response status completed", obj)
	}
	if obj.PreviousResponseID != nil {
		t.Fatalf("PreviousResponseID = %v, want null for a fresh create", obj.PreviousResponseID)
	}
	if len(obj.Output) != 2 || obj.Output[0].Type != "message" || obj.Output[0].ID != "msg_1" {
		t.Fatalf("output = %+v, want a message item first", obj.Output)
	}
	if len(obj.Output[0].Content) != 1 || obj.Output[0].Content[0].Text != "hi there" || obj.Output[0].Content[0].Type != "output_text" {
		t.Fatalf("message content = %+v, want one output_text part", obj.Output[0].Content)
	}
	if obj.Output[1].Type != "function_call" || obj.Output[1].CallID != "call_1" || obj.Output[1].Name != "get_weather" {
		t.Fatalf("output[1] = %+v, want the function_call item", obj.Output[1])
	}
	if obj.Usage == nil || obj.Usage.InputTokens != 10 || obj.Usage.OutputTokens != 3 || obj.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v, want 10/3/13", obj.Usage)
	}
	if obj.Usage.InputDetails == nil || obj.Usage.InputDetails.CachedTokens != 4 {
		t.Fatalf("usage details = %+v, want cached 4", obj.Usage)
	}
	if obj.Metadata["k"] != "v" {
		t.Fatalf("metadata = %+v, want the echoed metadata", obj.Metadata)
	}
	if assistant.Content != "hi there" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant turn = %+v, want the content + tool call for the transcript", assistant)
	}
	// Finish-reason mapping: length → incomplete/max_output_tokens.
	resp2 := &provider.ChatResponse{Content: "cut", FinishReason: "length"}
	obj2, _ := buildResponsesObject("resp_2", "msg_2", req, resp2)
	if obj2.Status != "incomplete" || obj2.IncompleteDetails == nil || obj2.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("length obj = %+v, want incomplete/max_output_tokens", obj2)
	}
	// A continuation echoes its previous id.
	req3 := responsesRequest{Model: "m", PreviousResponseID: "resp_1"}
	obj3, _ := buildResponsesObject("resp_3", "msg_3", req3, resp)
	if obj3.PreviousResponseID == nil || *obj3.PreviousResponseID != "resp_1" {
		t.Fatalf("PreviousResponseID = %+v, want resp_1", obj3.PreviousResponseID)
	}
}

func decodeResponsesBody(t *testing.T, rec *httptest.ResponseRecorder) responsesResponse {
	t.Helper()
	var obj responsesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return obj
}

func TestResponsesCreateRetrieve(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "hello back", Model: "gpt-4o",
		Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 4, TotalTokens: 9},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))
	// Auth: no key → 401.
	if rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"gpt-4o","input":"hi"}`, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key create: status = %d, want 401", rec.Code)
	}
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"gpt-4o","input":"hi","metadata":{"k":"v"}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	obj := decodeResponsesBody(t, rec)
	if obj.Object != "response" || obj.Status != "completed" || obj.Model != "gpt-4o" {
		t.Fatalf("obj = %+v, want response/completed/gpt-4o", obj)
	}
	if !strings.HasPrefix(obj.ID, "resp_") {
		t.Fatalf("id = %q, want a resp_ id", obj.ID)
	}
	if len(obj.Output) != 1 || obj.Output[0].Content[0].Text != "hello back" {
		t.Fatalf("output = %+v, want the engine's text", obj.Output)
	}
	if obj.Usage == nil || obj.Usage.TotalTokens != 9 {
		t.Fatalf("usage = %+v, want total 9", obj.Usage)
	}
	if obj.Metadata["k"] != "v" {
		t.Fatalf("metadata = %+v, want the echoed metadata", obj.Metadata)
	}
	if obj.PreviousResponseID != nil {
		t.Fatalf("PreviousResponseID = %v, want null", obj.PreviousResponseID)
	}
	// The engine saw a Responses-marked request with one user turn.
	got := eng.lastReq()
	if got.Endpoint != provider.EndpointResponses || len(got.Messages) != 1 || got.Messages[0].Content != "hi" {
		t.Fatalf("engine req = %+v, want one user turn marked responses", got)
	}
	if got.PreviousResponseID != "" || got.PinProviderID != "" {
		t.Fatalf("fresh create carried continuation state: %+v", got)
	}
	// Retrieve: byte-identical to the create, and the feed shows
	// the id with no content.
	rec2 := doReq(t, srv, http.MethodGet, "/v1/responses/"+obj.ID, "", "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve: status = %d, want 200", rec2.Code)
	}
	var obj2 responsesResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &obj2); err != nil {
		t.Fatalf("decode retrieve: %v", err)
	}
	if obj2.ID != obj.ID || obj2.Output[0].Content[0].Text != "hello back" {
		t.Fatalf("retrieve = %+v, want the same object as the create", obj2)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/resp_nope", "", "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown retrieve: status = %d, want 404", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodGet, "/v1/responses/"+obj.ID, "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key retrieve: status = %d, want 401", rec.Code)
	}
	feed := doReq(t, srv, http.MethodGet, "/dashboard/responses", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("feed: status = %d, want 200 (auth-exempt)", feed.Code)
	}
	var f responsesFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if len(f.Responses) != 1 || f.Responses[0].ID != obj.ID || f.Responses[0].Status != "completed" {
		t.Fatalf("feed = %+v, want the one created response", f)
	}
	if f.Created != 1 || f.Continuations != 0 {
		t.Fatalf("feed counts = %+v, want created 1 continuations 0", f)
	}
	if strings.Contains(feed.Body.String(), "hello back") || strings.Contains(feed.Body.String(), `"input"`) {
		t.Fatalf("feed leaks response content: %s", feed.Body.String())
	}
	// Validation errors are 400s naming the problem.
	for _, body := range []string{
		`{"input":"hi"}`,
		`{"model":"m"}`,
		`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`,
		`{"model":"m","input":"hi","routing":{"excludes":["a"]}}`,
	} {
		if rec := doReq(t, srv, http.MethodPost, "/v1/responses", body, "test-key-123"); rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestResponsesContinuationResumesTranscript(t *testing.T) {
	eng := &captureEngine{resp: provider.ChatResponse{
		Content: "first answer", Model: "m",
		Usage: provider.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4},
	}}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"what is 2+2?"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("create 1: status = %d, want 200", rec.Code)
	}
	first := decodeResponsesBody(t, rec)
	// Unknown previous id: 404, never a fresh conversation.
	if rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"and 3+3?","previous_response_id":"resp_nope"}`, "test-key-123"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown continuation: status = %d, want 404", rec.Code)
	}
	eng.resp = provider.ChatResponse{Content: "second answer", Model: "m", Usage: provider.Usage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8}}
	rec2 := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"and 3+3?","previous_response_id":"`+first.ID+`"}`, "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("continuation: status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
	}
	second := decodeResponsesBody(t, rec2)
	if second.PreviousResponseID == nil || *second.PreviousResponseID != first.ID {
		t.Fatalf("continuation previous_response_id = %+v, want %s", second.PreviousResponseID, first.ID)
	}
	// The engine saw the full transcript: turn 1 + assistant
	// answer + turn 2 — never just the new input.
	got := eng.lastReq()
	if got.PreviousResponseID != first.ID {
		t.Fatalf("engine PreviousResponseID = %q, want %s", got.PreviousResponseID, first.ID)
	}
	if len(got.Messages) != 3 {
		t.Fatalf("engine messages = %+v, want 3 (user, assistant, user)", got.Messages)
	}
	if got.Messages[0].Content != "what is 2+2?" || got.Messages[1].Content != "first answer" || got.Messages[2].Content != "and 3+3?" {
		t.Fatalf("engine transcript = %+v, want the resumed conversation", got.Messages)
	}
	// The feed shows the chain: second continues first.
	feed := doReq(t, srv, http.MethodGet, "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if len(f.Responses) != 2 || f.Continuations != 1 {
		t.Fatalf("feed = %+v, want 2 responses, 1 continuation", f)
	}
	if f.Responses[0].PreviousResponseID != first.ID || f.Responses[1].PreviousResponseID != "" {
		t.Fatalf("feed chain = %+v, want second→first", f.Responses)
	}
	// Third turn still resumes the whole conversation.
	eng.resp = provider.ChatResponse{Content: "third answer", Model: "m"}
	rec3 := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"and 4+4?","previous_response_id":"`+second.ID+`"}`, "test-key-123")
	if rec3.Code != http.StatusOK {
		t.Fatalf("third turn: status = %d, want 200", rec3.Code)
	}
	got = eng.lastReq()
	if len(got.Messages) != 5 {
		t.Fatalf("third-turn messages = %+v, want 5 turns", got.Messages)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	eng := &captureEngine{
		deltas: []string{"Hello ", "world"},
		resp: provider.ChatResponse{
			Content: "Hello world", Model: "m",
			Usage: provider.Usage{PromptTokens: 2, CompletionTokens: 2, TotalTokens: 4},
		},
	}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()))
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"m","input":"hi","stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream create: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		`"delta":"Hello "`,
		`"delta":"world"`,
		"event: response.output_text.done",
		`"text":"Hello world"`,
		"event: response.output_item.done",
		"event: response.completed",
		`"status":"completed"`,
		"data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %q:\n%s", want, body)
		}
	}
	// The created event's id is the stored response id: retrieve
	// returns the completed object with the streamed text.
	// Parse the id out of the first response.created data line.
	var created struct {
		Response responsesResponse `json:"response"`
	}
	createdID := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: {") {
			var ev struct {
				Response *responsesResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err == nil && ev.Response != nil && ev.Response.ID != "" {
				createdID = ev.Response.ID
				created.Response = *ev.Response
				break
			}
		}
	}
	if createdID == "" || !strings.HasPrefix(createdID, "resp_") {
		t.Fatalf("no response.created id found in stream:\n%s", body)
	}
	if created.Response.Status != "in_progress" {
		t.Fatalf("created status = %q, want in_progress", created.Response.Status)
	}
	rec2 := doReq(t, srv, http.MethodGet, "/v1/responses/"+createdID, "", "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve streamed response: status = %d, want 200", rec2.Code)
	}
	got := decodeResponsesBody(t, rec2)
	if got.Status != "completed" || got.Output[0].Content[0].Text != "Hello world" {
		t.Fatalf("retrieved stream = %+v, want completed/Hello world", got)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 4 {
		t.Fatalf("retrieved usage = %+v, want total 4", got.Usage)
	}
	feed := doReq(t, srv, http.MethodGet, "/dashboard/responses", "", "")
	var f responsesFeed
	if err := json.Unmarshal(feed.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if len(f.Responses) != 1 || !f.Responses[0].Stream {
		t.Fatalf("feed = %+v, want the one streamed response marked stream", f)
	}
}

// responsesMockStack builds the real Server + real router.Engine +
// two real provider clients against local mocks, for model
// "resp-model". deadStatus 0 = never responds (hangs until the
// client times out); deadToggle flips the dead mock between
// healthy and dead at runtime.
func responsesMockStack(t *testing.T, deadStatus int, deadTimeout time.Duration) (*Server, *router.Engine, *failoverGateLedger, *int32, *int32, *atomicBool) {
	t.Helper()
	var deadHits, fallbackHits int32
	deadOn := &atomicBool{v: true}
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&deadHits, 1)
		if !deadOn.get() {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-dead","object":"chat.completion","model":"resp-model","choices":[{"message":{"role":"assistant","content":"dead says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			return
		}
		if deadStatus == 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(200 * time.Millisecond):
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"resp-model","choices":[{"message":{"role":"assistant","content":"too late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(deadStatus)
		_, _ = w.Write([]byte(`{"error":{"message":"dead model is down","type":"server_error"}}`))
	}))
	t.Cleanup(deadSrv.Close)
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fallbackHits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","model":"resp-model","choices":[{"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	t.Cleanup(fallbackSrv.Close)
	deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithTimeout(deadTimeout))
	if err != nil {
		t.Fatalf("NewClient dead: %v", err)
	}
	fallbackClient, err := provider.NewClient(fallbackSrv.URL, "fallback-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient fallback: %v", err)
	}
	led := &failoverGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"dead": deadClient, "fallback": fallbackClient}, led, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "resp-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "dead", Model: "resp-model"}, {ProviderID: "fallback", Model: "resp-model"}}
		},
	})
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))
	return srv, eng, led, &deadHits, &fallbackHits, deadOn
}

// atomicBool is a test-only atomic bool.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}

func (b *atomicBool) set(v bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.v = v
}

// TestResponsesSameRequestFailover is the CHECKLIST 169 failover
// gate for Responses: a FRESH create hitting a dead model (429)
// succeeds via fallback in THAT SAME request — dead hit once,
// fallback hit once, ledger dead-failed then fallback-ok, trace
// 2 attempts, response stored + retrievable.
func TestResponsesSameRequestFailover(t *testing.T) {
	srv, eng, led, deadHits, fallbackHits, _ := responsesMockStack(t, http.StatusTooManyRequests, 2*time.Second)
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"resp-model","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("fresh create: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	obj := decodeResponsesBody(t, rec)
	if len(obj.Output) != 1 || obj.Output[0].Content[0].Text != "fallback says hi" {
		t.Fatalf("obj = %+v, want the fallback's content", obj)
	}
	if strings.Contains(rec.Body.String(), "dead model is down") {
		t.Fatalf("response leaks the dead provider's error text: %s", rec.Body.String())
	}
	if got := atomic.LoadInt32(deadHits); got != 1 {
		t.Fatalf("dead hits = %d, want 1", got)
	}
	if got := atomic.LoadInt32(fallbackHits); got != 1 {
		t.Fatalf("fallback hits = %d, want 1", got)
	}
	rows := led.snapshot()
	if len(rows) != 2 || rows[0] != (failoverGateLedgerRow{provider: "dead", model: "resp-model", ok: false}) || rows[1] != (failoverGateLedgerRow{provider: "fallback", model: "resp-model", ok: true}) {
		t.Fatalf("ledger rows = %+v, want dead-failed then fallback-ok", rows)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[1].Provider != "fallback" || trace.Attempts[1].Outcome != "success" {
		t.Fatalf("trace = %+v, want dead then fallback/success", trace)
	}
	// Stored + retrievable under the created id.
	rec2 := doReq(t, srv, http.MethodGet, "/v1/responses/"+obj.ID, "", "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve after failover: status = %d, want 200", rec2.Code)
	}
}

// TestResponsesContinuationFailsOver is the CHECKLIST 237
// failover gate, end to end: (a) a continuation pinned to a
// provider that now 429s succeeds via the fallback in THAT SAME
// request — the pinned provider is hit exactly once, the fallback
// serves the same branched input (the parent transcript + the new
// turn); (b) a continuation pinned to the healthy fallback still
// serves even though the dead provider is first in the chain.
func TestResponsesContinuationFailsOver(t *testing.T) {
	t.Run("pinned-provider-429s", func(t *testing.T) {
		// Dead is healthy at create time, so the record pins
		// dead; then dead starts 429ing.
		srv, _, _, deadHits, fallbackHits, deadOn := responsesMockStack(t, http.StatusTooManyRequests, 2*time.Second)
		deadOn.set(false)
		rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"resp-model","input":"first"}`, "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status = %d, want 200", rec.Code)
		}
		first := decodeResponsesBody(t, rec)
		if got := atomic.LoadInt32(deadHits); got != 1 {
			t.Fatalf("create dead hits = %d, want 1 (dead served it)", got)
		}
		deadOn.set(true)
		before := atomic.LoadInt32(deadHits)
		rec2 := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"resp-model","input":"second","previous_response_id":"`+first.ID+`"}`, "test-key-123")
		if rec2.Code != http.StatusOK {
			t.Fatalf("continuation on dead provider: status = %d, want 200 via failover (body %s)", rec2.Code, rec2.Body.String())
		}
		second := decodeResponsesBody(t, rec2)
		if len(second.Output) != 1 || second.Output[0].Content[0].Text != "fallback says hi" {
			t.Fatalf("continuation obj = %+v, want the fallback's content", second)
		}
		if second.PreviousResponseID == nil || *second.PreviousResponseID != first.ID {
			t.Fatalf("continuation previous_response_id = %v, want %s", second.PreviousResponseID, first.ID)
		}
		if got := atomic.LoadInt32(deadHits) - before; got != 1 {
			t.Fatalf("continuation dead hits = %d, want exactly 1", got)
		}
		if got := atomic.LoadInt32(fallbackHits); got != 1 {
			t.Fatalf("fallback hits = %d, want 1 (the branched request failed over onto it)", got)
		}
	})
	t.Run("pin-skips-dead-provider", func(t *testing.T) {
		// Dead 429s at create time, so the record pins the
		// fallback; the continuation then serves from the
		// fallback without ever dialling dead again.
		srv, _, _, deadHits, fallbackHits, _ := responsesMockStack(t, http.StatusTooManyRequests, 2*time.Second)
		rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"resp-model","input":"first"}`, "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status = %d, want 200", rec.Code)
		}
		first := decodeResponsesBody(t, rec)
		deadBefore, fbBefore := atomic.LoadInt32(deadHits), atomic.LoadInt32(fallbackHits)
		rec2 := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"resp-model","input":"second","previous_response_id":"`+first.ID+`"}`, "test-key-123")
		if rec2.Code != http.StatusOK {
			t.Fatalf("continuation: status = %d, want 200 (body %s)", rec2.Code, rec2.Body.String())
		}
		second := decodeResponsesBody(t, rec2)
		if len(second.Output) != 1 || second.Output[0].Content[0].Text != "fallback says hi" {
			t.Fatalf("continuation obj = %+v, want the fallback's content", second)
		}
		if got := atomic.LoadInt32(deadHits) - deadBefore; got != 0 {
			t.Fatalf("continuation dead hits = %d, want 0 (pinned to fallback)", got)
		}
		if got := atomic.LoadInt32(fallbackHits) - fbBefore; got != 1 {
			t.Fatalf("continuation fallback hits = %d, want 1", got)
		}
	})
}

// TestResponsesConcurrentCreatesNeverShare proves the no-coalescing
// rule end to end: two identical concurrent creates each mint
// their own response id and each dial the upstream once — they
// never share one flight.
func TestResponsesConcurrentCreatesNeverShare(t *testing.T) {
	var hits int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(150 * time.Millisecond) // overlap the two flights
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"resp-model","choices":[{"message":{"role":"assistant","content":"same answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	t.Cleanup(mock.Close)
	client, err := provider.NewClient(mock.URL, "k", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"only": client}, &failoverGateLedger{}, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "resp-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "only", Model: "resp-model"}}
		},
	})
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))
	const n = 2
	recs := make([]*httptest.ResponseRecorder, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// No testing.T calls in here: build the request
			// directly so a failure cannot call t.Fatalf off
			// the test goroutine.
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"resp-model","input":"hi"}`))
			req.Header.Set("Authorization", "Bearer test-key-123")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			recs[i] = rec
		}(i)
	}
	wg.Wait()
	ids := make([]string, n)
	for i, rec := range recs {
		if rec.Code != http.StatusOK {
			t.Fatalf("create %d: status = %d, want 200", i, rec.Code)
		}
		ids[i] = decodeResponsesBody(t, rec).ID
	}
	if ids[0] == "" || ids[0] == ids[1] {
		t.Fatalf("ids = %v, want two distinct response ids", ids)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (no shared flight)", got)
	}
}
