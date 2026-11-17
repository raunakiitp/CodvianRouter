package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeEngine is a controllable Engine for tests. It can succeed, fail,
// fail mid-stream, and (as a StreamResumer) resume from an offset.
type fakeEngine struct {
	chatResp        provider.ChatResponse
	chatErr         error
	deltas          []string
	streamErr       error
	failAfter       int // return streamErr after this many deltas; -1 = never fail
	blocked         chan struct{}
	resumeErr       error
	resumeDeltas    []string
	replayFromStart bool // ChatStreamFrom re-emits the full content (tests trim)
	resumeCalled    bool
	resumeGotSkip   int
}

func (f *fakeEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if f.blocked != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-f.blocked:
		}
	}
	if f.chatErr != nil {
		return nil, f.chatErr
	}
	resp := f.chatResp
	return &resp, nil
}

func (f *fakeEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	for i, d := range f.deltas {
		if f.streamErr != nil && f.failAfter >= 0 && i == f.failAfter {
			return nil, f.streamErr
		}
		if err := onDelta(d); err != nil {
			return nil, err
		}
	}
	if f.streamErr != nil && f.failAfter >= 0 && f.failAfter == len(f.deltas) {
		return nil, f.streamErr
	}
	resp := f.chatResp
	return &resp, nil
}

func (f *fakeEngine) ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	f.resumeCalled = true
	f.resumeGotSkip = skipChars
	if f.replayFromStart {
		// Deliberately ignore skipChars and replay everything.
		for _, d := range f.deltas {
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
	} else {
		for _, d := range f.resumeDeltas {
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
	}
	if f.resumeErr != nil {
		return nil, f.resumeErr
	}
	resp := f.chatResp
	return &resp, nil
}

func newTestServer(t *testing.T, eng Engine, opts ...Option) *Server {
	t.Helper()
	all := append([]Option{WithStaticKeys("test-key-123"), WithLogger(log.New(io.Discard, "", 0))}, opts...)
	return New(eng, all...)
}

func doReq(t *testing.T, srv *Server, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestHealthz(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/healthz", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %q, want {\"status\":\"ok\"}", got)
	}
}

func TestModelsUsesConfiguredModels(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithModels("model-a", "model-b"))
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Object string  `json:"object"`
		Data   []Model `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "list" || len(out.Data) != 2 || out.Data[0].ID != "model-a" || out.Data[1].ID != "model-b" {
		t.Fatalf("unexpected models response: %+v", out)
	}
}

type fakeRegistry struct{ models []Model }

func (f *fakeRegistry) Models(ctx context.Context) ([]Model, error) { return f.models, nil }

func TestModelsUsesRegistry(t *testing.T) {
	reg := &fakeRegistry{models: []Model{{ID: "reg-model", Object: "model", OwnedBy: "prov"}}}
	srv := newTestServer(t, &fakeEngine{}, WithRegistry(reg), WithModels("ignored"))
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reg-model") || strings.Contains(rec.Body.String(), "ignored") {
		t.Fatalf("registry not used: %s", rec.Body.String())
	}
}

// TestModelsUsesProviderRegistry exercises the real *provider.Registry
// through AdaptProviderRegistry, loaded from a JSON store file.
func TestModelsUsesProviderRegistry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	cfgs := []provider.ProviderConfig{{
		ID: "prov_1", Name: "TestProv", BaseURL: "https://example.com",
		Models:  []provider.Model{{ID: "m1", Name: "m1"}, {ID: "m2", Name: "m2"}},
		Enabled: true,
	}}
	raw, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(path))
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg), WithModels("ignored"))
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"id":"m1"`) || !strings.Contains(body, `"id":"m2"`) {
		t.Fatalf("provider models not listed: %s", body)
	}
	if !strings.Contains(body, "TestProv") || strings.Contains(body, "ignored") {
		t.Fatalf("unexpected models response: %s", body)
	}
}

func TestChatCompletionsSuccess(t *testing.T) {
	eng := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Hello there",
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
	}}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message      provider.Message `json:"message"`
			FinishReason string           `json:"finish_reason"`
		} `json:"choices"`
		Usage provider.Usage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || !strings.HasPrefix(out.ID, "chatcmpl-") {
		t.Fatalf("bad completion envelope: %+v", out)
	}
	if out.Model != "model-a" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "Hello there" {
		t.Fatalf("bad choices: %+v", out)
	}
	if out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("bad choice: %+v", out.Choices[0])
	}
	if out.Usage.TotalTokens != 7 {
		t.Fatalf("usage = %+v, want total 7", out.Usage)
	}
}

func TestChatCompletionsUnauthorized(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, tc := range []struct {
		name string
		key  string
	}{
		{"no header", ""},
		{"wrong key", "nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
				`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, tc.key)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var out errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if out.Error.Message != "invalid gateway key" || out.Error.Type != "invalid_request_error" {
				t.Fatalf("error body = %+v", out)
			}
		})
	}
	// Models endpoint is protected too.
	if rec := doReq(t, srv, http.MethodGet, "/v1/models", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("models without key: status = %d, want 401", rec.Code)
	}
}

// TestAuthWithConfigVerifier checks the production auth path: keys issued
// via internal/config and verified through NewConfigVerifier.
func TestAuthWithConfigVerifier(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("default", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	// The static test key must NOT work once config verification is in use.
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("static key with config verifier: status = %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/models", "", raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("config-issued key: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestChatCompletionsBadRequest(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	cases := []struct {
		name string
		body string
	}{
		{"missing model", `{"messages":[{"role":"user","content":"hi"}]}`},
		{"empty messages", `{"model":"m","messages":[]}`},
		{"missing role", `{"model":"m","messages":[{"content":"hi"}]}`},
		{"bad role", `{"model":"m","messages":[{"role":"alien","content":"hi"}]}`},
		{"empty user content", `{"model":"m","messages":[{"role":"user","content":""}]}`},
		{"invalid json", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", tc.body, "test-key-123")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestChatCompletionsStreaming(t *testing.T) {
	eng := &fakeEngine{
		deltas: []string{"Hello", " world"},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" world"`) {
		t.Fatalf("chunks missing content: %s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Fatalf("chunks missing chunk object: %s", body)
	}
	if !strings.Contains(body, `"role":"assistant"`) {
		t.Fatalf("first chunk missing assistant role: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %s", body)
	}
}

func TestChatCompletionsStreamingResume(t *testing.T) {
	eng := &fakeEngine{
		deltas:       []string{"Hello ", "world"},
		failAfter:    1, // send "Hello ", then fail before "world"
		streamErr:    errors.New("upstream died mid-stream"),
		resumeDeltas: []string{"world"},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !eng.resumeCalled {
		t.Fatal("expected a resume attempt via ChatStreamFrom")
	}
	if eng.resumeGotSkip != len("Hello ") {
		t.Fatalf("resume skip = %d, want %d", eng.resumeGotSkip, len("Hello "))
	}
	if n := strings.Count(body, "Hello"); n != 1 {
		t.Fatalf("sent content %q appears %d times, want 1 (no re-send on resume): %s", "Hello", n, body)
	}
	if !strings.Contains(body, "world") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("resume did not complete stream: %s", body)
	}
}

func TestChatCompletionsStreamingResumeTrimsReplay(t *testing.T) {
	eng := &fakeEngine{
		deltas:          []string{"Hello ", "world"},
		failAfter:       1,
		streamErr:       errors.New("upstream died mid-stream"),
		replayFromStart: true, // resume ignores skipChars, replays everything
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	body := rec.Body.String()
	if n := strings.Count(body, "Hello"); n != 1 {
		t.Fatalf("replayed content appears %d times, want 1: %s", n, body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("stream did not complete after trimmed resume: %s", body)
	}
}

func TestChatCompletionsStreamingFailureBeforeAnyContent(t *testing.T) {
	eng := &fakeEngine{failAfter: 0, streamErr: errors.New("upstream down")}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no content sent yet, JSON error still possible)", rec.Code)
	}
}

func TestChatCompletionsTimeout(t *testing.T) {
	eng := &fakeEngine{blocked: make(chan struct{})}
	srv := newTestServer(t, eng, WithTimeout(50*time.Millisecond))
	start := time.Now()
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if time.Since(start) > 10*time.Second {
		t.Fatal("request did not time out promptly")
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
}

func TestTrimAlreadySent(t *testing.T) {
	cases := []struct{ sent, delta, want string }{
		{"Hello ", "Hello world", "world"},
		{"Hello ", "world", "world"},
		{"abc", "abc", ""},
		{"", "hello", "hello"},
		{"hello", "", ""},
		{"abcdef", "defghi", "ghi"},
	}
	for _, tc := range cases {
		if got := trimAlreadySent(tc.sent, tc.delta); got != tc.want {
			t.Errorf("trimAlreadySent(%q, %q) = %q, want %q", tc.sent, tc.delta, got, tc.want)
		}
	}
}
