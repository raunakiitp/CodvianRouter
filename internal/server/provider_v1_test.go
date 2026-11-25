package server

// Per-provider v1 surfaces (M8.12): POST
// /v1/providers/{provider}/chat/completions, GET
// /v1/providers/{provider}/models, GET /v1/providers/{provider}/status.
//
// NOTE on registration: the routes are not in the mux yet —
// RegisterProviderV1Routes (provider_v1.go) is wired by the
// coordinator together with the guard-table entries (guard.go and
// server.go are outside this slice's owned files), because
// s.register panics on an unclassified route. These tests therefore
// invoke the handlers directly (http.HandlerFunc), the same way
// other handler tests in this package exercise handlers, and use
// apitest fixtures for the contract assertions. Auth/scope
// enforcement lives in the guard wrapper at registration; the
// proposed scopes are recorded in provider_v1.go and in the final
// report.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// providerV1MockUpstream is one mock upstream behind one provider
// key: it counts calls, records the Authorization header it saw
// (proving which key served the attempt), and answers chat
// completions with a canned status/body.
type providerV1MockUpstream struct {
	calls    atomic.Int32
	authSeen string
	mu       sync.Mutex
	status   int
	body     string
}

func (m *providerV1MockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		m.mu.Lock()
		m.authSeen = r.Header.Get("Authorization")
		m.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.status)
		_, _ = w.Write([]byte(m.body))
	}
}

func (m *providerV1MockUpstream) callCount() int { return int(m.calls.Load()) }

func (m *providerV1MockUpstream) lastAuth() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.authSeen
}

// providerV1FakeEngine is a controllable Engine for the
// models/status tests: it records chat requests (normally none)
// and reports scripted breaker/cooldown state.
type providerV1FakeEngine struct {
	mu       sync.Mutex
	reqs     []provider.ChatRequest
	resp     provider.ChatResponse
	breakers []router.BreakerStatus
	cooling  []router.CoolingTarget
}

func (e *providerV1FakeEngine) Chat(_ context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	out := e.resp
	e.mu.Unlock()
	return &out, nil
}

func (e *providerV1FakeEngine) ChatStream(_ context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	out := e.resp
	e.mu.Unlock()
	return &out, nil
}

func (e *providerV1FakeEngine) BreakerStates() []router.BreakerStatus { return e.breakers }
func (e *providerV1FakeEngine) CooldownTargets() []router.CoolingTarget {
	return e.cooling
}
func (e *providerV1FakeEngine) CooldownState() (string, map[string]string) {
	return "", nil
}
func (e *providerV1FakeEngine) SetCooldownMode(string)              {}
func (e *providerV1FakeEngine) SetChainCooldownMode(string, string) {}
func (e *providerV1FakeEngine) chatReqCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.reqs)
}

// providerV1FakeRegistry is a controllable ModelRegistry.
type providerV1FakeRegistry struct {
	models []Model
	err    error
}

func (f *providerV1FakeRegistry) Models(context.Context) ([]Model, error) {
	return f.models, f.err
}

// providerV1TestConfig builds a config whose gateway key the test
// can present and whose providers list holds mock-upstream with two
// keys (key-1, key-2). It returns the config and the raw gateway
// key.
func providerV1TestConfig(t *testing.T, baseURL string) (*config.Config, string) {
	t.Helper()
	k, raw, err := config.IssueKey("m8.12-test", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{
			ID:      "mock-upstream",
			Name:    "Mock Upstream",
			BaseURL: baseURL,
			Enabled: true,
			Keys: []provider.ProviderKey{
				{ID: "key-1", Role: provider.KeyRolePreferred},
				{ID: "key-2", Role: provider.KeyRolePreferred},
			},
		}},
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	return cfg, raw
}

// runProviderV1Fixture mirrors apitest.Run but sends the fixture's
// request with the test's real gateway key instead of the
// fixture's placeholder Authorization header. The fixture's other
// headers and body are used verbatim, and the assertions run
// through apitest.CheckResponse.
func runProviderV1Fixture(t *testing.T, handler http.Handler, f apitest.Fixture, rawKey string) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()
	var body io.Reader
	if f.Request.Body != nil {
		raw, err := json.Marshal(f.Request.Body)
		if err != nil {
			t.Fatalf("marshal fixture request body: %v", err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(f.Method, srv.URL+f.Path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for k, v := range f.Request.Headers {
		if strings.EqualFold(k, "Authorization") {
			continue // replaced below with the test's real key
		}
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+rawKey)
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := apitest.CheckResponse(resp, respBody, f); err != nil {
		t.Fatalf("apitest: fixture %s/%s: %v", f.Slice, f.Name, err)
	}
}

func providerV1ChatBody(model string) string {
	return `{"model":"` + model + `","messages":[{"role":"user","content":"Say hello."}]}`
}

// TestProviderV1ChatPinnedFailoverFixture is the M8.12 core proof:
// a pinned request goes through the REAL engine, fails over
// between the pinned provider's two keys against mock upstreams
// (key-1 answers 429, key-2 answers 200), and never calls the
// other provider — even though the chain lists the other provider
// FIRST, so without the pin it would have served. The fixture
// pins the response contract; the call counts pin the routing.
func TestProviderV1ChatPinnedFailoverFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.12", "provider-chat-completions-200")

	key1 := &providerV1MockUpstream{status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`}
	key2 := &providerV1MockUpstream{status: http.StatusOK}
	other := &providerV1MockUpstream{status: http.StatusOK, body: `{"model":"mock-model","choices":[{"message":{"role":"assistant","content":"Served by the other provider."}}]}`}
	// key-2 replays the fixture's recorded response verbatim.
	recBody, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	key2.body = string(recBody)

	srv1 := httptest.NewServer(key1.handler())
	defer srv1.Close()
	srv2 := httptest.NewServer(key2.handler())
	defer srv2.Close()
	srvOther := httptest.NewServer(other.handler())
	defer srvOther.Close()

	c1, err := provider.NewClient(srv1.URL, "mock-key-1", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient key-1: %v", err)
	}
	c2, err := provider.NewClient(srv2.URL, "mock-key-2", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient key-2: %v", err)
	}
	cOther, err := provider.NewClient(srvOther.URL, "mock-key-other", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient other: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{
		router.ClientKey("mock-upstream", "key-1"): c1,
		router.ClientKey("mock-upstream", "key-2"): c2,
		router.ClientKey("other-provider", ""):     cOther,
	}, nil, router.Config{
		DisableAdaptiveLimiter: true,
		MaxAttempts:            5,
		HedgeDelay:             -1, // no hedge: failover must be sequential
		BackoffBase:            time.Millisecond,
		BackoffMax:             time.Millisecond,
		ChainFor: func(model string) []router.Attempt {
			if model != "mock-model" {
				return nil
			}
			// Other provider FIRST: only the pin keeps it out.
			return []router.Attempt{
				{ProviderID: "other-provider", Model: model},
				{ProviderID: "mock-upstream", Model: model, KeyID: "key-1"},
				{ProviderID: "mock-upstream", Model: model, KeyID: "key-2"},
			}
		},
	})
	cfg, rawKey := providerV1TestConfig(t, srv2.URL)
	srv := newTestServer(t, eng, WithConfig(cfg))

	runProviderV1Fixture(t, http.HandlerFunc(srv.handleProviderChatCompletions), f, rawKey)

	if got := key1.callCount(); got != 1 {
		t.Fatalf("key-1 mock calls = %d, want 1 (the pinned provider's first key must be tried)", got)
	}
	if got := key2.callCount(); got != 1 {
		t.Fatalf("key-2 mock calls = %d, want 1 (failover within the pinned provider)", got)
	}
	if got := other.callCount(); got != 0 {
		t.Fatalf("other-provider mock calls = %d, want 0 (the pin must never reroute to another provider)", got)
	}
	if got := key1.lastAuth(); got != "Bearer mock-key-1" {
		t.Fatalf("key-1 mock Authorization = %q, want Bearer mock-key-1", got)
	}
	if got := key2.lastAuth(); got != "Bearer mock-key-2" {
		t.Fatalf("key-2 mock Authorization = %q, want Bearer mock-key-2 (failover switched keys)", got)
	}
}

// TestProviderV1ChatUnconfiguredFixture pins the honest failure:
// a catalog-known provider with no key configured answers 422
// naming the gap, and no upstream is called at all.
func TestProviderV1ChatUnconfiguredFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.12", "provider-chat-completions-422-unconfigured")
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "must never be served"}}
	// Empty config: no providers configured, so "openai" is
	// catalog-known only. Gateway key is the static test key.
	srv := newTestServer(t, eng)
	runProviderV1Fixture(t, http.HandlerFunc(srv.handleProviderChatCompletions), f, "test-key-123")
	if got := eng.chatReqCount(); got != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 (unconfigured provider must fail before any provider call)", got)
	}
}

// TestProviderV1ModelsFixture pins the per-provider models list:
// only the pinned provider's models appear, in the /v1/models
// list shape.
func TestProviderV1ModelsFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.12", "provider-models-200")
	eng := &providerV1FakeEngine{}
	reg := &providerV1FakeRegistry{models: []Model{
		{ID: "mock-model", Object: "model", OwnedBy: "mock-upstream", ProviderID: "mock-upstream"},
		{ID: "mock-model-2", Object: "model", OwnedBy: "mock-upstream", ProviderID: "mock-upstream"},
		{ID: "other-model", Object: "model", OwnedBy: "other-provider", ProviderID: "other-provider"},
	}}
	cfg, _ := providerV1TestConfig(t, "http://127.0.0.1:9")
	srv := newTestServer(t, eng, WithConfig(cfg), WithRegistry(reg))
	apitest.Run(t, http.HandlerFunc(srv.handleProviderModels), f)
}

// TestProviderV1StatusFixture pins the per-provider status report:
// configured facts plus the engine's live breaker state.
func TestProviderV1StatusFixture(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.12", "provider-status-200")
	eng := &providerV1FakeEngine{
		breakers: []router.BreakerStatus{{ProviderID: "mock-upstream", Open: true, ConsecutiveFailures: 3}},
	}
	cfg, _ := providerV1TestConfig(t, "http://127.0.0.1:9")
	srv := newTestServer(t, eng, WithConfig(cfg))
	apitest.Run(t, http.HandlerFunc(srv.handleProviderStatus), f)
}

// TestProviderV1ChatFixtureToggle proves the fixture assertions
// themselves fail on a wrong expectation (the toggle rule): the
// same 200 response checked against a 201 expectation must fail.
func TestProviderV1ChatFixtureToggle(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.12", "provider-chat-completions-200")
	resp := &http.Response{StatusCode: http.StatusOK}
	body, err := json.Marshal(f.RecordedResponse.Body)
	if err != nil {
		t.Fatalf("marshal recorded response: %v", err)
	}
	if err := apitest.CheckResponse(resp, body, f); err != nil {
		t.Fatalf("CheckResponse with the fixture's own expectations must pass: %v", err)
	}
	wrong := f
	wrong.Assert.Status = http.StatusCreated
	if err := apitest.CheckResponse(resp, body, wrong); err == nil {
		t.Fatal("CheckResponse with assert.status 201 against a 200 response must fail (toggle)")
	}
}

// TestProviderV1ChatUnknownProvider answers 404 and never calls
// the engine.
func TestProviderV1ChatUnknownProvider(t *testing.T) {
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "must never be served"}}
	srv := newTestServer(t, eng)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/providers/no-such-provider/chat/completions", strings.NewReader(providerV1ChatBody("mock-model")))
	req.Header.Set("Authorization", "Bearer test-key-123")
	srv.handleProviderChatCompletions(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no-such-provider") {
		t.Fatalf("body %s does not name the unknown provider", rec.Body.String())
	}
	if got := eng.chatReqCount(); got != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", got)
	}
}

// TestProviderV1ChatNoKey answers 422 naming the missing key for
// a configured provider with no keys.
func TestProviderV1ChatNoKey(t *testing.T) {
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "must never be served"}}
	k, raw, err := config.IssueKey("m8.12-test", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	cfg := &config.Config{Providers: []config.ProviderConfig{{
		ID: "keyless-provider", Name: "Keyless", BaseURL: "http://127.0.0.1:9", Enabled: true,
	}}}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	srv := newTestServer(t, eng, WithConfig(cfg))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/providers/keyless-provider/chat/completions", strings.NewReader(providerV1ChatBody("mock-model")))
	req.Header.Set("Authorization", "Bearer "+raw)
	srv.handleProviderChatCompletions(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no API key") {
		t.Fatalf("body %s does not name the missing key", rec.Body.String())
	}
	if got := eng.chatReqCount(); got != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", got)
	}
}

// TestProviderV1ChatNoBaseURL answers 422 naming the missing base
// URL for a configured provider with keys but no base URL.
func TestProviderV1ChatNoBaseURL(t *testing.T) {
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "must never be served"}}
	k, raw, err := config.IssueKey("m8.12-test", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	cfg := &config.Config{Providers: []config.ProviderConfig{{
		ID: "nourl-provider", Name: "NoURL", Enabled: true,
		Keys: []provider.ProviderKey{{ID: "key-1"}},
	}}}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	srv := newTestServer(t, eng, WithConfig(cfg))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/providers/nourl-provider/chat/completions", strings.NewReader(providerV1ChatBody("mock-model")))
	req.Header.Set("Authorization", "Bearer "+raw)
	srv.handleProviderChatCompletions(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no base URL") {
		t.Fatalf("body %s does not name the missing base URL", rec.Body.String())
	}
	if got := eng.chatReqCount(); got != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", got)
	}
}

// TestProviderV1ChatRoutingConflict proves a client routing
// contract naming a different provider is a 400, never a silent
// rewrite: the engine is never called.
func TestProviderV1ChatRoutingConflict(t *testing.T) {
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "must never be served"}}
	cfg, raw := providerV1TestConfig(t, "http://127.0.0.1:9")
	srv := newTestServer(t, eng, WithConfig(cfg))
	cases := map[string]string{
		"only other":     `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"routing":{"only":["other-provider"]}}`,
		"order other":    `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"routing":{"order":["other-provider"]}}`,
		"exclude pin":    `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"routing":{"exclude":["mock-upstream"]}}`,
		"group":          `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"routing":{"group":"some-group"}}`,
		"routing scalar": `{"model":"mock-model","messages":[{"role":"user","content":"hi"}],"routing":"only-mock"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/providers/mock-upstream/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+raw)
			srv.handleProviderChatCompletions(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if got := eng.chatReqCount(); got != 0 {
		t.Fatalf("engine Chat calls = %d, want 0", got)
	}
}

// TestProviderV1PinBodyUnit pins the body rewrite itself: the pin
// is injected, a same-provider only/order is normalised to the
// pin, and non-conflicting controls survive.
func TestProviderV1PinBodyUnit(t *testing.T) {
	out, err := pinProviderV1Body([]byte(`{"model":"m","messages":[],"routing":{"only":["mock-upstream"],"sort":"price"}}`), "mock-upstream")
	if err != nil {
		t.Fatalf("pin body: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode pinned body: %v", err)
	}
	routing := decoded["routing"].(map[string]any)
	if only, _ := routing["only"].([]any); len(only) != 1 || only[0] != "mock-upstream" {
		t.Fatalf("routing.only = %v, want [mock-upstream]", routing["only"])
	}
	if routing["sort"] != "price" {
		t.Fatalf("routing.sort = %v, want price (non-conflicting controls survive)", routing["sort"])
	}
	out, err = pinProviderV1Body([]byte(`{"model":"m","messages":[]}`), "mock-upstream")
	if err != nil {
		t.Fatalf("pin body without routing: %v", err)
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode pinned body: %v", err)
	}
	if routing := decoded["routing"].(map[string]any); routing["only"].([]any)[0] != "mock-upstream" {
		t.Fatalf("routing.only = %v, want [mock-upstream]", routing["only"])
	}
	if _, err := pinProviderV1Body([]byte(`{invalid json`), "mock-upstream"); err == nil {
		t.Fatal("invalid JSON body must fail the pin, not pass through")
	}
}

// TestProviderV1ModelsUnknown answers 404 for an unknown provider.
func TestProviderV1ModelsUnknown(t *testing.T) {
	srv := newTestServer(t, &providerV1FakeEngine{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/no-such-provider/models", nil)
	srv.handleProviderModels(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestProviderV1StatusUnknown answers 404 for an unknown provider.
func TestProviderV1StatusUnknown(t *testing.T) {
	srv := newTestServer(t, &providerV1FakeEngine{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/no-such-provider/status", nil)
	srv.handleProviderStatus(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestProviderV1StatusUnconfigured answers 200 with
// configured:false for a catalog-known but unconfigured provider:
// status is a report, so the honest report is the answer.
func TestProviderV1StatusUnconfigured(t *testing.T) {
	srv := newTestServer(t, &providerV1FakeEngine{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/openai/status", nil)
	srv.handleProviderStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st providerV1Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if st.Configured || st.KeyCount != 0 || st.Breaker != "unknown" {
		t.Fatalf("status = %+v, want configured=false key_count=0 breaker=unknown", st)
	}
	if !st.BaseURLKnown {
		t.Fatalf("status = %+v, want base_url_configured=true for the catalog entry", st)
	}
	if st.UnconfiguredNote == "" {
		t.Fatalf("status = %+v, want a note naming the unconfigured state", st)
	}
}

// TestProviderV1ModelsUnconfigured answers 422: the catalog
// carries no model list, so there is nothing honest to list.
func TestProviderV1ModelsUnconfigured(t *testing.T) {
	srv := newTestServer(t, &providerV1FakeEngine{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/providers/openai/models", nil)
	srv.handleProviderModels(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

// TestProviderV1ChatCaseInsensitive proves "Mock-Upstream" names
// the same provider as "mock-upstream".
func TestProviderV1ChatCaseInsensitive(t *testing.T) {
	eng := &providerV1FakeEngine{resp: provider.ChatResponse{Content: "ok", Model: "mock-model"}}
	cfg, raw := providerV1TestConfig(t, "http://127.0.0.1:9")
	srv := newTestServer(t, eng, WithConfig(cfg))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/providers/Mock-Upstream/chat/completions", strings.NewReader(providerV1ChatBody("mock-model")))
	req.Header.Set("Authorization", "Bearer "+raw)
	srv.handleProviderChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := eng.chatReqCount(); got != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", got)
	}
	// The engine must see the canonical pin, not the typed case.
	eng.mu.Lock()
	only := eng.reqs[0].Routing.Only
	eng.mu.Unlock()
	if len(only) != 1 || only[0] != "mock-upstream" {
		t.Fatalf("engine Routing.Only = %v, want [mock-upstream]", only)
	}
}
