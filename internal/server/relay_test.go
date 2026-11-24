package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/readiness"
	"github.com/raunakiitp/codvian-router/internal/relay"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// fakeRelayResolver serves canned relay targets for the messages tests.
type fakeRelayResolver struct {
	targets map[string]*relayTarget
	err     error
}

func (f *fakeRelayResolver) ResolveRelayTarget(_ context.Context, model string) (*relayTarget, error) {
	if f.err != nil {
		return nil, f.err
	}
	t, ok := f.targets[model]
	if !ok {
		return nil, errTestNoTarget
	}
	return t, nil
}

var errTestNoTarget = errors.New("no relay target for model")

// anthropicMock records the exact bytes it receives and answers with a
// canned Anthropic response (JSON, or SSE when the request streams).
type anthropicMock struct {
	mu         sync.Mutex
	bodies     [][]byte
	headers    []http.Header
	streamBody string
	jsonBody   string
	status     int
	// hook, when set, takes over the response for the duration of one
	// request: it returns true when it handled the request itself. It
	// exists so a test can drive a stream by hand (a slow client, a
	// mid-stream disconnect, an observed upstream cancellation).
	hook func(w http.ResponseWriter, r *http.Request) bool
}

func (m *anthropicMock) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.bodies = append(m.bodies, body)
		m.headers = append(m.headers, r.Header.Clone())
		m.mu.Unlock()
		if r.URL.Path != "/v1/messages" {
			t.Errorf("mock got unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		status := m.status
		if status == 0 {
			status = http.StatusOK
		}
		var stream bool
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		stream = probe.Stream
		if m.hook != nil && m.hook(w, r) {
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(m.streamBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(m.jsonBody))
	}
}

func (m *anthropicMock) lastBody(t *testing.T) []byte {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.bodies) == 0 {
		t.Fatal("mock received no request")
	}
	return m.bodies[len(m.bodies)-1]
}

// testCallerID returns the gateway-key id the server derives for
// the static test key (W1.1): the messages path keys cache entries
// by this tenant, so a test that seeds the cache directly must seed
// under the same id the handler will look up.
func testCallerID(t *testing.T, srv *Server) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("Authorization", "Bearer test-key-123")
	id := srv.callerKeyID(r)
	if id == "" {
		t.Fatal("callerKeyID for the static test key is empty")
	}
	return id
}

func newMessagesServer(t *testing.T, eng Engine, mock *anthropicMock, opts ...Option) *Server {
	t.Helper()
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"claude-sonnet-4-5": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: srv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test", "anthropic-version": "2023-06-01"},
			UpstreamModel: "claude-sonnet-4-5", Attempts: 1,
		},
		"claude-aliased": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: srv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "claude-sonnet-4-5", Attempts: 1,
		},
		"multi-model": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: srv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "multi-model", Attempts: 2,
		},
		"local-model": {
			ProviderID: "ollama-local", ProviderName: "Ollama", WireFormat: "ollama",
			BaseURL: "http://127.0.0.1:9", AuthHeaders: map[string]string{},
			UpstreamModel: "local-model", Attempts: 1, IsLocal: true,
		},
	}}
	all := append([]Option{WithRelayResolver(resolver)}, opts...)
	return newTestServer(t, eng, all...)
}

const anthropicJSONReply = `{"id":"msg_01","type":"message","role":"assistant","content":[{"type":"text","text":"Hello from Claude"}],"model":"claude-sonnet-4-5","stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":4}}`

const anthropicSSEReply = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":12,\"output_tokens\":4}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestMessagesVerbatimRelayByteComparison(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)
	// Odd whitespace, unknown fields, a nested "model" key and a
	// cache_control block: the mock must receive these bytes exactly
	// (only the model value rewritten in the aliased case below).
	raw := "{\n  \"model\" : \"claude-sonnet-4-5\",  \"max_tokens\": 64,\n  \"system\": \"Be brief.\",\n  \"messages\": [{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"Hi\", \"cache_control\": {\"type\": \"ephemeral\"}}]}],\n  \"nested\": {\"model\": \"must-not-change\"},\n  \"future_field\": {\"x\": 1}\n}"
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Errorf("X-Codvian-Relay = %q, want verbatim", got)
	}
	if got := mock.lastBody(t); string(got) != raw {
		t.Errorf("upstream received:\n%s\nwant byte-identical:\n%s", got, raw)
	}
	if !strings.Contains(rec.Body.String(), `"text":"Hello from Claude"`) {
		t.Errorf("response body not passed through: %s", rec.Body.String())
	}
	// The aliased model rewrites ONLY the top-level model value.
	rawAlias := `{"model":"claude-aliased","max_tokens":64,"messages":[{"role":"user","content":"Hi"}],"nested":{"model":"must-not-change"}}`
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", rawAlias, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("aliased status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hi"}],"nested":{"model":"must-not-change"}}`
	if got := string(mock.lastBody(t)); got != want {
		t.Errorf("aliased upstream body = %s, want %s", got, want)
	}
	// Auth headers are exactly the catalog-resolved ones.
	hdrs := mock.headers[len(mock.headers)-1]
	if hdrs.Get("x-api-key") != "sk-ant-test" {
		t.Errorf("upstream x-api-key = %q, want the catalog-resolved key", hdrs.Get("x-api-key"))
	}
	if hdrs.Get("Authorization") != "" {
		t.Errorf("upstream got an Authorization header %q: relay must send exactly the resolved headers", hdrs.Get("Authorization"))
	}
}

func TestMessagesVerbatimStreamEnvelopeUntouched(t *testing.T) {
	mock := &anthropicMock{streamBody: anthropicSSEReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if rec.Body.String() != anthropicSSEReply {
		t.Errorf("SSE body differs from upstream:\n got %q\nwant %q", rec.Body.String(), anthropicSSEReply)
	}
}

func TestMessagesTranslatePathWhenGuardrailsBlocking(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	eng := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Translated hello", Model: "claude-sonnet-4-5", FinishReason: "stop",
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	srv := newMessagesServer(t, eng, mock, WithGuardrails(guardrails.NewStore()))
	// Turn blocking on: the request must take the translate path
	// (guardrails need the canonical form) and the upstream mock
	// must NOT be called at all — the engine serves it.
	srv.guardrails.SetConfig(guardrails.Config{BlockPII: true})
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Hi","cache_control":{"type":"ephemeral"}}]}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); !strings.HasPrefix(got, "translated") {
		t.Errorf("X-Codvian-Relay = %q, want translated", got)
	}
	if !strings.Contains(rec.Header().Get("X-Codvian-Relay"), "dropped_cache_control=1") {
		t.Errorf("relay header should report the dropped cache_control block, got %q", rec.Header().Get("X-Codvian-Relay"))
	}
	mock.mu.Lock()
	calls := len(mock.bodies)
	mock.mu.Unlock()
	if calls != 0 {
		t.Errorf("mock was called %d times on the translate path; the engine should serve it", calls)
	}
	var got struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			Input int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("translated response is not valid Anthropic JSON: %v", err)
	}
	if got.Type != "message" || got.StopReason != "end_turn" || len(got.Content) != 1 || got.Content[0].Text != "Translated hello" {
		t.Errorf("translated response wrong: %+v", got)
	}
	if got.Usage.Input != 9 {
		t.Errorf("translated usage input_tokens = %d, want 9", got.Usage.Input)
	}
}

func TestMessagesTranslatePathOnMultiAttempt(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "via engine", Model: "multi-model", FinishReason: "stop"}}
	srv := newMessagesServer(t, eng, mock)
	raw := `{"model":"multi-model","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); !strings.HasPrefix(got, "translated") {
		t.Errorf("multi-attempt model: X-Codvian-Relay = %q, want translated (failover needs canonical form)", got)
	}
}

func TestMessagesStreamTranslateEmitsAnthropicEnvelope(t *testing.T) {
	mock := &anthropicMock{}
	eng := &fakeEngine{
		deltas: []string{"Hel", "lo"},
		chatResp: provider.ChatResponse{
			Content: "Hello", Model: "claude-sonnet-4-5", FinishReason: "stop",
			Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12},
		},
	}
	srv := newMessagesServer(t, eng, mock, WithGuardrails(guardrails.NewStore()))
	srv.guardrails.SetConfig(guardrails.Config{BlockSecrets: true})
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, ev := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(body, "event: "+ev) {
			t.Errorf("translated stream missing event %s:\n%s", ev, body)
		}
	}
	if !strings.Contains(body, `"text":"Hel"`) || !strings.Contains(body, `"text":"lo"`) {
		t.Errorf("translated stream missing text deltas:\n%s", body)
	}
	if !strings.Contains(body, `"input_tokens":10`) {
		t.Errorf("translated stream message_delta missing true usage:\n%s", body)
	}
}

func TestMessagesRequiresAuthAndModel(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":1,"messages":[{"role":"user","content":"Hi"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", rec.Code)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", `{"max_tokens":1,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("no model: status = %d, want 400", rec.Code)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/messages", ``, "test-key-123")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
}

func TestModelsDeprecatedFiltering(t *testing.T) {
	reg := provider.NewRegistry(&memModelStore{models: []provider.ProviderConfig{
		{ID: "oai", Name: "OpenAI", BaseURL: "https://api.openai.com", APIKeyEncrypted: "", WireFormat: provider.WireFormatOpenAI, Enabled: true, Models: []provider.Model{
			{ID: "gpt-4o"},
			{ID: "gpt-4-0314"},
			{ID: "gpt-3.5-turbo", Deprecated: true},
		}},
	}})
	if err := reg.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg))
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var list struct {
		Data []struct {
			ID         string `json:"id"`
			Deprecated bool   `json:"deprecated"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0].ID != "gpt-4o" {
		t.Errorf("default list = %v, want only gpt-4o", list.Data)
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/models?include=deprecated", "", "test-key-123")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list.Data) != 3 {
		t.Fatalf("include=deprecated list = %d models, want 3", len(list.Data))
	}
	dep := map[string]bool{}
	for _, m := range list.Data {
		dep[m.ID] = m.Deprecated
	}
	if !dep["gpt-4-0314"] || !dep["gpt-3.5-turbo"] || dep["gpt-4o"] {
		t.Errorf("deprecated flags = %v", dep)
	}
	// Dashboard feed: same rule, plus the hidden count.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/models", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard models status = %d", rec.Code)
	}
	var feed struct {
		Models []struct {
			ID         string `json:"id"`
			Deprecated bool   `json:"deprecated"`
		} `json:"models"`
		Total            int `json:"total"`
		DeprecatedHidden int `json:"deprecated_hidden"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if feed.Total != 1 || feed.DeprecatedHidden != 2 {
		t.Errorf("feed total=%d hidden=%d, want 1/2", feed.Total, feed.DeprecatedHidden)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/models?include=deprecated", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if feed.Total != 3 || feed.DeprecatedHidden != 0 {
		t.Errorf("feed with include: total=%d hidden=%d, want 3/0", feed.Total, feed.DeprecatedHidden)
	}
}

// memModelStore is a minimal provider.Store for the models tests.
type memModelStore struct {
	models []provider.ProviderConfig
}

func (m *memModelStore) Load() ([]provider.ProviderConfig, error) { return m.models, nil }
func (m *memModelStore) Save(ps []provider.ProviderConfig) error  { m.models = ps; return nil }

func TestDeprecatedModelStillForwards(t *testing.T) {
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "still served", Model: "gpt-4-0314", FinishReason: "stop"}}
	srv := newTestServer(t, eng, WithModels("gpt-4-0314"))
	body := `{"model":"gpt-4-0314","messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("deprecated model chat: status = %d, body %s — a deprecated model must still forward, never hard-block", rec.Code, rec.Body.String())
	}
}

func TestReadinessGate503WithRetryAfter(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	gate := &readiness.Gate{}
	// The local Ollama runtime is not ready yet: probe says no,
	// Retry-After 30 s.
	gate.Register("local:ollama-local", 30*time.Second, func(context.Context) bool { return false })
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "served locally", Model: "local-model", FinishReason: "stop"}}
	srv := newMessagesServer(t, eng, mock, WithReadinessGate(gate))

	// Chat completions to the local model: honest 503 + Retry-After,
	// no provider dialled, no silent failure.
	body := `{"model":"local-model","messages":[{"role":"user","content":"Hi"}]}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready chat: status = %d, want 503, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After = %q, want 30", got)
	}
	if !strings.Contains(rec.Body.String(), "capability_not_ready") {
		t.Errorf("503 body should name capability_not_ready: %s", rec.Body.String())
	}
	// The same gate applies on /v1/messages.
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", `{"model":"local-model","max_tokens":8,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "30" {
		t.Errorf("not-ready messages: status = %d Retry-After = %q, want 503/30", rec.Code, rec.Header().Get("Retry-After"))
	}
	// The readiness feed reports the state live.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/readiness", "", "")
	var feed struct {
		Capabilities []struct {
			Name       string `json:"name"`
			Ready      bool   `json:"ready"`
			RetryAfter int    `json:"retry_after_seconds"`
			Known      bool   `json:"known"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("readiness feed decode: %v", err)
	}
	if len(feed.Capabilities) != 1 || feed.Capabilities[0].Name != "local:ollama-local" || feed.Capabilities[0].Ready || feed.Capabilities[0].RetryAfter != 30 || !feed.Capabilities[0].Known {
		t.Errorf("readiness feed = %+v, want one not-ready local:ollama-local/30/known", feed.Capabilities)
	}
	// Assets arrive: the same request now serves.
	gate.SetReady("local:ollama-local", true)
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("ready chat: status = %d, want 200, body %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/readiness", "", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("readiness feed decode: %v", err)
	}
	if len(feed.Capabilities) != 1 || !feed.Capabilities[0].Ready {
		t.Errorf("readiness feed after ready = %+v, want ready", feed.Capabilities)
	}
}

func TestReadinessFeedEmptyWithoutGate(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/readiness", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var feed struct {
		Capabilities []any `json:"capabilities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if feed.Capabilities == nil || len(feed.Capabilities) != 0 {
		t.Errorf("capabilities = %v, want empty non-nil list", feed.Capabilities)
	}
}

// TestMessagesCacheHitServedWithoutUpstream proves lookup-then-relay:
// with cache serving on, an exact-cache hit is served in the Anthropic
// shape and no provider is called at all.
func TestMessagesCacheHitServedWithoutUpstream(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	srv := newMessagesServer(t, &fakeEngine{}, mock, WithCacheServing(true), WithCaches(exact, semantic))
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`
	parsed, err := relay.ParseAnthropicDetailed([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := anthropicToChatRequest(parsed.Request)
	req.CallerID = testCallerID(t, srv)
	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	exact.Set(context.Background(), key, cache.CachedResponse{
		Content: "Cached hello", Usage: cache.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, Model: "claude-sonnet-4-5",
	}, time.Hour)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "exact" {
		t.Errorf("X-Codvian-Cache = %q, want exact", got)
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "cache-hit;layer=exact" {
		t.Errorf("X-Codvian-Relay = %q, want cache-hit;layer=exact", got)
	}
	if !strings.Contains(rec.Body.String(), `"text":"Cached hello"`) {
		t.Errorf("cache-hit body = %s, want the cached text", rec.Body.String())
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	if len(mock.bodies) != 0 {
		t.Errorf("upstream called %d times on a cache hit, want 0", len(mock.bodies))
	}
}

// TestMessagesCacheMissRelaysVerbatim proves the other half: with
// cache serving on and an empty cache, the same cacheable request
// relays verbatim (byte-identical, cache_control intact) — the cache
// never forces a translation it cannot serve. It also proves the
// store-back: after the relay, the response is in the exact cache.
func TestMessagesCacheMissRelaysVerbatim(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	srv := newMessagesServer(t, &fakeEngine{}, mock, WithCacheServing(true), WithCaches(exact, semantic))
	raw := "{\n  \"model\" : \"claude-sonnet-4-5\",  \"max_tokens\": 64,\n  \"messages\": [{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"Hi\", \"cache_control\": {\"type\": \"ephemeral\"}}]}]\n}"
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); got != "verbatim" {
		t.Errorf("X-Codvian-Relay = %q, want verbatim on a cache miss", got)
	}
	if got := string(mock.lastBody(t)); got != raw {
		t.Errorf("upstream received:\n%s\nwant byte-identical:\n%s", got, raw)
	}
	parsed, err := relay.ParseAnthropicDetailed([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := anthropicToChatRequest(parsed.Request)
	req.CallerID = testCallerID(t, srv) // store-back is scoped under this tenant (M6.11)
	if hit, _, found := srv.lookupMessagesCache(context.Background(), req); !found {
		t.Errorf("verbatim response was not stored back into the cache")
	} else if hit.Content != "Hello from Claude" {
		t.Errorf("stored cache content = %q, want %q", hit.Content, "Hello from Claude")
	}
}

// TestMessagesCacheDoesNotBypassGuardrails proves the ordering:
// with blocking guardrails on, an exact-cache hit for the same
// request is NOT served — the prompt must be inspected first, exactly
// as on /v1/chat/completions. The request takes the translate path.
func TestMessagesCacheDoesNotBypassGuardrails(t *testing.T) {
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	eng := &fakeEngine{chatResp: provider.ChatResponse{
		Content: "Translated hello", Model: "claude-sonnet-4-5", FinishReason: "stop",
		Usage: provider.Usage{PromptTokens: 9, CompletionTokens: 2, TotalTokens: 11},
	}}
	exact := cache.NewExactCache(500)
	semantic := cache.NewLocalSemanticCache()
	srv := newMessagesServer(t, eng, mock,
		WithGuardrails(guardrails.NewStore()), WithCacheServing(true), WithCaches(exact, semantic))
	srv.guardrails.SetConfig(guardrails.Config{BlockPII: true})
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`
	parsed, err := relay.ParseAnthropicDetailed([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	req := anthropicToChatRequest(parsed.Request)
	req.CallerID = testCallerID(t, srv)
	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	exact.Set(context.Background(), key, cache.CachedResponse{
		Content: "Cached hello", Usage: cache.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, Model: "claude-sonnet-4-5",
	}, time.Hour)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Relay"); !strings.HasPrefix(got, "translated") {
		t.Errorf("X-Codvian-Relay = %q, want translated (guardrails run before the cache)", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "" {
		t.Errorf("X-Codvian-Cache = %q, want empty: the cache must not serve under blocking guardrails", got)
	}
	if !strings.Contains(rec.Body.String(), `"text":"Translated hello"`) {
		t.Errorf("body = %s, want the engine's answer, not the cached one", rec.Body.String())
	}
}

// TestMessagesVerbatimRelayStreamLifetimeCap (CHECKLIST 255): a
// relayed Anthropic stream that stalls after its first event is
// killed at the engine's stream-lifetime cap — the upstream call
// is terminated and the client sees the streamed event, then an
// Anthropic terminal error event naming the cap, never a
// message_stop that never arrived and never a hang.
func TestMessagesVerbatimRelayStreamLifetimeCap(t *testing.T) {
	// Stalling upstream: one message_start event, then silence
	// forever — the held-stream shape the cap exists for.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":12,\"output_tokens\":0}}}\n\n"))
		fl.Flush()
		<-r.Context().Done() // hold until the relay terminates us
	}))
	defer upstream.Close()
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"claude-sonnet-4-5": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: upstream.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test", "anthropic-version": "2023-06-01"},
			UpstreamModel: "claude-sonnet-4-5", Attempts: 1,
		},
	}}
	// The real engine, reporting a 200 ms cap — the verbatim
	// relay reads its bound from the engine via
	// streamLifetimeCap, never from a hard-coded value.
	eng := router.NewEngine(map[string]provider.Client{"a": &fakeEngineClient{}}, nil, router.Config{
		StreamMaxLifetime: 200 * time.Millisecond,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: model}}
		},
	})
	srv := newTestServer(t, eng, WithRelayResolver(resolver), WithTimeout(10*time.Second))
	raw := `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/messages", raw, "test-key-123")
	}()
	select {
	case rec := <-done:
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
		}
		if !strings.Contains(body, "message_start") {
			t.Errorf("body = %s, want the streamed message_start before the cap", body)
		}
		if !strings.Contains(body, "stream lifetime exceeded") {
			t.Errorf("body = %s, want the terminal error event naming the cap", body)
		}
		if strings.Contains(body, "message_stop") {
			t.Errorf("body = %s, want no message_stop — the upstream never sent one and the relay must not invent it", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("verbatim relay did not return in 5 s — the cap did not kill the held relay")
	}
}
