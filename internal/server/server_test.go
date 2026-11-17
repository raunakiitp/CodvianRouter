package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
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

func TestDashboard(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	// No gateway key: the dashboard is static UI, served without auth.
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"Codvian Router", "Overview", "Providers", "Models", "verif-badge", "/api/providers"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	// The approved design has no Playground section anywhere.
	if strings.Contains(body, "Playground") {
		t.Fatal("dashboard must not contain Playground")
	}
}

func TestDashboardMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestDashboardDoesNotBypassAPIAuth guards the wiring: adding
// /dashboard must not open /v1/* or /metrics to unauthenticated calls.
func TestDashboardDoesNotBypassAPIAuth(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithModels("model-a"))
	for _, path := range []string{"/v1/models", "/metrics", "/v1/chat/completions"} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without key: status = %d, want 401", path, rec.Code)
		}
	}
}

// fakeDashboardProviders is a controllable DashboardProviderLister.
type fakeDashboardProviders struct {
	providers []provider.DashboardProvider
}

func (f fakeDashboardProviders) DashboardProviders() []provider.DashboardProvider {
	return f.providers
}

// TestDashboardProviders proves the live provider feed: verification
// status, wire format and auth kind per provider, plus the summary
// counts — served without a gateway key (like the dashboard page
// itself) and carrying no secrets.
func TestDashboardProviders(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", AuthKind: "bearer", AuthEndpoint: "chat", CatalogID: "groq", Models: 2},
		{ID: "p2", Name: "DeepSeek", Enabled: true, WireFormat: "anthropic", Verification: "verified-from-docs", AuthKind: "x-api-key", AuthEndpoint: "anthropic", CatalogID: "deepseek", Models: 1},
		{ID: "p3", Name: "My Custom", Enabled: false, WireFormat: "openai", Verification: "unverified", AuthKind: "bearer", Models: 0},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got dashboardProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(got.Providers))
	}
	if got.Providers[0].Verification != "verified-from-docs" || got.Providers[0].AuthKind != "bearer" || got.Providers[0].WireFormat != "openai" {
		t.Errorf("groq = %+v", got.Providers[0])
	}
	if got.Providers[1].AuthKind != "x-api-key" || got.Providers[1].WireFormat != "anthropic" {
		t.Errorf("deepseek = %+v", got.Providers[1])
	}
	if got.Providers[2].Verification != "unverified" || got.Providers[2].Enabled {
		t.Errorf("custom = %+v", got.Providers[2])
	}
	want := dashboardProviderSummary{Total: 3, Enabled: 2, VerifiedFromDocs: 2, Unverified: 1}
	if got.Summary != want {
		t.Errorf("summary = %+v, want %+v", got.Summary, want)
	}
	// No secrets anywhere in the raw payload.
	body := rec.Body.String()
	for _, secret := range []string{"sk-", "api_key", "base_url"} {
		if strings.Contains(body, secret) {
			t.Errorf("payload contains %q — provider feed must never carry secrets", secret)
		}
	}
}

// TestDashboardProvidersEmpty proves a server with no lister answers
// an honest empty list, not an error — the dashboard renders
// "no providers" instead of failing.
func TestDashboardProvidersEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got dashboardProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Providers == nil || len(got.Providers) != 0 {
		t.Fatalf("providers = %+v, want empty list", got.Providers)
	}
	if got.Summary.Total != 0 {
		t.Errorf("summary = %+v, want zero total", got.Summary)
	}
}

func TestDashboardProvidersMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(fakeDashboardProviders{}))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/providers", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// fakeHealthStatuses is a controllable HealthStatusLister.
type fakeHealthStatuses struct {
	statuses []health.Status
}

func (f fakeHealthStatuses) Last() []health.Status { return f.statuses }

// TestAPIProviders proves the dashboard's provider feed: id, name,
// wire format, verification status and health status per provider,
// plus the summary counts — served without a gateway key (like the
// dashboard page itself) and carrying no secrets. Health comes from
// the checker's last results: ok, down, and unknown for a provider
// never probed (here the disabled one).
func TestAPIProviders(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", AuthKind: "bearer", CatalogID: "groq", Models: 2},
		{ID: "p2", Name: "DeepSeek", Enabled: true, WireFormat: "anthropic", Verification: "from-reference-metadata-only", AuthKind: "x-api-key", CatalogID: "deepseek", Models: 1},
		{ID: "p3", Name: "My Custom", Enabled: false, WireFormat: "openai", Verification: "unverified", AuthKind: "bearer", Models: 0},
	}}
	healthLister := fakeHealthStatuses{statuses: []health.Status{
		{ProviderID: "p1", ProviderName: "Groq", OK: true},
		{ProviderID: "p2", ProviderName: "DeepSeek", OK: false, LastError: "boom"},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister), WithHealthChecker(healthLister))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got apiProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 3 {
		t.Fatalf("providers = %d, want 3", len(got.Providers))
	}
	if got.Providers[0].ID != "p1" || got.Providers[0].Name != "Groq" || got.Providers[0].WireFormat != "openai" || got.Providers[0].Verification != "verified-from-docs" || got.Providers[0].HealthStatus != "ok" {
		t.Errorf("groq = %+v", got.Providers[0])
	}
	if got.Providers[1].Verification != "from-reference-metadata-only" || got.Providers[1].HealthStatus != "down" {
		t.Errorf("deepseek = %+v", got.Providers[1])
	}
	if got.Providers[2].Verification != "unverified" || got.Providers[2].HealthStatus != "unknown" || got.Providers[2].Enabled {
		t.Errorf("custom = %+v", got.Providers[2])
	}
	want := dashboardProviderSummary{Total: 3, Enabled: 2, VerifiedFromDocs: 1, FromReferenceMetadataOnly: 1, Unverified: 1}
	if got.Summary != want {
		t.Errorf("summary = %+v, want %+v", got.Summary, want)
	}
	// No secrets anywhere in the raw payload.
	body := rec.Body.String()
	for _, secret := range []string{"sk-", "api_key", "base_url"} {
		if strings.Contains(body, secret) {
			t.Errorf("payload contains %q — provider feed must never carry secrets", secret)
		}
	}
}

// TestAPIProvidersNoHealthChecker proves the endpoint still works
// with no checker wired: every provider reports health "unknown"
// rather than the endpoint failing.
func TestAPIProvidersNoHealthChecker(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", Models: 2},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got apiProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 1 || got.Providers[0].HealthStatus != "unknown" {
		t.Fatalf("providers = %+v, want one provider with health unknown", got.Providers)
	}
}

// TestAPIProvidersEmpty proves a server with no lister answers an
// honest empty list, not an error — the dashboard renders its
// "no providers connected yet" empty state.
func TestAPIProvidersEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got apiProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Providers == nil || len(got.Providers) != 0 {
		t.Fatalf("providers = %+v, want empty list", got.Providers)
	}
	if got.Summary.Total != 0 {
		t.Errorf("summary = %+v, want zero total", got.Summary)
	}
}

func TestAPIProvidersMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(fakeDashboardProviders{}))
	rec := doReq(t, srv, http.MethodPost, "/api/providers", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
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

// staleFakeEngine wraps a fakeEngine and reports that its last call
// served a stale cached response.
type staleFakeEngine struct {
	*fakeEngine
	age time.Duration
}

func (f *staleFakeEngine) LastServedStale() bool       { return true }
func (f *staleFakeEngine) LastStaleAge() time.Duration { return f.age }

// tracedFakeEngine wraps a fakeEngine and exposes a fixed RouteTrace.
type tracedFakeEngine struct {
	*fakeEngine
	trace *routetrace.RouteTrace
}

func (f *tracedFakeEngine) LastTrace() *routetrace.RouteTrace { return f.trace }

func chatBody() string {
	return `{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`
}

func TestMetricsEndpointRecordsRequest(t *testing.T) {
	rec := metrics.NewRecorder()
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Model: "model-a"}}
	srv := newTestServer(t, eng, WithMetrics(rec))

	got := doReq(t, srv, http.MethodPost, "/v1/chat/completions", chatBody(), "test-key-123")
	if got.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", got.Code)
	}
	m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if m.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", m.Code)
	}
	body := m.Body.String()
	if !strings.Contains(body, "codvian_router_requests_total{") ||
		!strings.Contains(body, `outcome="ok"`) ||
		!strings.Contains(body, `model="model_a"`) {
		t.Fatalf("metrics body missing request counter:\n%s", body)
	}
	if !strings.Contains(body, "codvian_router_request_latency_ms") {
		t.Fatalf("metrics body missing latency histogram:\n%s", body)
	}
}

func TestMetricsRequiresAuth(t *testing.T) {
	rec := metrics.NewRecorder()
	srv := newTestServer(t, &fakeEngine{}, WithMetrics(rec))
	got := doReq(t, srv, http.MethodGet, "/metrics", "", "")
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("metrics without key: status = %d, want 401", got.Code)
	}
}

func TestMetricsDisabledWithoutRecorder(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	got := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("metrics without recorder: status = %d, want 503", got.Code)
	}
}

func TestMetricsCountsFailoverFromTrace(t *testing.T) {
	rec := metrics.NewRecorder()
	eng := &tracedFakeEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Model: "model-a"}},
		trace: &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
			{Provider: "provider-a", Model: "model-a", Outcome: "error"},
			{Provider: "provider-b", Model: "model-a", Outcome: "success"},
		}},
	}
	srv := newTestServer(t, eng, WithMetrics(rec))
	got := doReq(t, srv, http.MethodPost, "/v1/chat/completions", chatBody(), "test-key-123")
	if got.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", got.Code)
	}
	m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	body := m.Body.String()
	if !strings.Contains(body, "codvian_router_failovers_total{") ||
		!strings.Contains(body, `from_provider="provider_a"`) ||
		!strings.Contains(body, `to_provider="provider_b"`) {
		t.Fatalf("metrics body missing failover counter:\n%s", body)
	}
	if !strings.Contains(body, `provider="provider_b"`) {
		t.Fatalf("request counter should attribute the winning provider:\n%s", body)
	}
}

func TestMetricsRecordsErrorOutcome(t *testing.T) {
	rec := metrics.NewRecorder()
	eng := &fakeEngine{chatErr: errors.New("boom")}
	srv := newTestServer(t, eng, WithMetrics(rec))
	got := doReq(t, srv, http.MethodPost, "/v1/chat/completions", chatBody(), "test-key-123")
	if got.Code != http.StatusBadGateway {
		t.Fatalf("chat status = %d, want 502", got.Code)
	}
	m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if !strings.Contains(m.Body.String(), `outcome="error"`) {
		t.Fatalf("metrics body missing error outcome:\n%s", m.Body.String())
	}
}

func TestStaleHeaderSet(t *testing.T) {
	eng := &staleFakeEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "old answer", Model: "model-a"}},
		age:        5 * time.Minute,
	}
	srv := newTestServer(t, eng)
	got := doReq(t, srv, http.MethodPost, "/v1/chat/completions", chatBody(), "test-key-123")
	if got.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", got.Code)
	}
	if got.Header().Get("X-Codvian-Stale") != "true" {
		t.Fatalf("X-Codvian-Stale = %q, want true", got.Header().Get("X-Codvian-Stale"))
	}
	if got.Header().Get("X-Codvian-Stale-Age") != "5m0s" {
		t.Fatalf("X-Codvian-Stale-Age = %q, want 5m0s", got.Header().Get("X-Codvian-Stale-Age"))
	}
}

func TestStaleHeaderAbsentForFresh(t *testing.T) {
	eng := &fakeEngine{chatResp: provider.ChatResponse{Content: "fresh", Model: "model-a"}}
	srv := newTestServer(t, eng)
	got := doReq(t, srv, http.MethodPost, "/v1/chat/completions", chatBody(), "test-key-123")
	if got.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", got.Code)
	}
	if got.Header().Get("X-Codvian-Stale") != "" {
		t.Fatalf("X-Codvian-Stale = %q, want absent for fresh responses", got.Header().Get("X-Codvian-Stale"))
	}
}

// fakeChunkEngine streams chunk-aware (tool-call deltas + finish
// reason), like the real router engine after the wire integration.
type fakeChunkEngine struct {
	fakeEngine
	chunks []provider.StreamChunk
}

func (f *fakeChunkEngine) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	for _, c := range f.chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	resp := f.chatResp
	return &resp, nil
}

func (f *fakeChunkEngine) ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	return f.ChatStreamChunks(ctx, req, onChunk)
}

func TestChatCompletionsToolCallsAndFinishReason(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{
		Content:      "checking",
		Model:        "claude-sonnet-4-5",
		FinishReason: "tool_calls",
		ToolCalls:    mustToolCalls(t),
	}})
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if len(body.Choices) != 1 {
		t.Fatalf("choices = %v", body.Choices)
	}
	if body.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls (not a hard-coded stop)", body.Choices[0].FinishReason)
	}
	if len(body.Choices[0].Message.ToolCalls) != 1 || body.Choices[0].Message.ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("tool_calls = %+v", body.Choices[0].Message.ToolCalls)
	}
}

func mustToolCalls(t *testing.T) []provider.ToolCall {
	t.Helper()
	var calls []provider.ToolCall
	if err := json.Unmarshal([]byte(`[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Pune\"}"}}]`), &calls); err != nil {
		t.Fatalf("tool calls fixture: %v", err)
	}
	return calls
}

func TestChatCompletionsStreamingChunkAware(t *testing.T) {
	eng := &fakeChunkEngine{
		fakeEngine: fakeEngine{chatResp: provider.ChatResponse{
			Content:      "hi ",
			FinishReason: "tool_calls",
			ToolCalls:    mustToolCalls(t),
		}},
		chunks: []provider.StreamChunk{
			{Content: "hi "},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather"}}},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, Arguments: `{"city":"Pune"}`}}},
			{FinishReason: "tool_calls"},
		},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"tool_calls"`) {
		t.Errorf("SSE body has no tool_calls chunk:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Errorf("SSE body has no tool_calls finish_reason:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("SSE body has no [DONE]:\n%s", body)
	}
	// Exactly one tool_calls finish_reason: the chunk-aware path must
	// not also emit the old hard-coded "stop" finish chunk.
	if n := strings.Count(body, `"finish_reason":"tool_calls"`); n != 1 {
		t.Errorf(`"finish_reason":"tool_calls" appears %d times, want exactly 1:\n%s`, n, body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("SSE body must not contain a hard-coded stop finish chunk:\n%s", body)
	}
}

// cacheHitEngine wraps a fakeEngine and reports a proactive cache hit,
// like degrade.StaleEngine with Policy.ServeCache.
type cacheHitEngine struct {
	*fakeEngine
	kind string
	sim  float32
}

func (e *cacheHitEngine) LastCacheHit() (string, float32, bool) {
	if e.kind == "" {
		return "", 0, false
	}
	return e.kind, e.sim, true
}

func TestChatCompletionsSemanticCacheHeader(t *testing.T) {
	eng := &cacheHitEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "Paris.", Model: "model-a"}},
		kind:       "semantic",
		sim:        0.923,
	}
	srv := newTestServer(t, eng, WithModels("model-a"))
	rec := doReq(t, srv, "POST", "/v1/chat/completions", `{"model":"model-a","messages":[{"role":"user","content":"What is the capital of France today?"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "semantic" {
		t.Fatalf("X-Codvian-Cache = %q, want semantic", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache-Similarity"); got != "0.923" {
		t.Fatalf("X-Codvian-Cache-Similarity = %q, want 0.923", got)
	}
	if got := rec.Header().Get("X-Codvian-Stale"); got != "" {
		t.Fatalf("X-Codvian-Stale = %q, want empty (a cache hit is not stale)", got)
	}
}

func TestChatCompletionsExactCacheHeaderHasNoSimilarity(t *testing.T) {
	eng := &cacheHitEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "4", Model: "model-a"}},
		kind:       "exact",
	}
	srv := newTestServer(t, eng, WithModels("model-a"))
	rec := doReq(t, srv, "POST", "/v1/chat/completions", `{"model":"model-a","messages":[{"role":"user","content":"What is 2+2?"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "exact" {
		t.Fatalf("X-Codvian-Cache = %q, want exact", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache-Similarity"); got != "" {
		t.Fatalf("X-Codvian-Cache-Similarity = %q, want empty for exact hits", got)
	}
}

func TestChatCompletionsCacheHitMetricsAttributedToCache(t *testing.T) {
	eng := &cacheHitEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "Paris.", Model: "model-a"}},
		kind:       "semantic",
		sim:        0.91,
	}
	recorder := metrics.NewRecorder()
	srv := newTestServer(t, eng, WithModels("model-a"), WithMetrics(recorder))
	rec := doReq(t, srv, "POST", "/v1/chat/completions", `{"model":"model-a","messages":[{"role":"user","content":"What is the capital of France today?"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The request must be attributed to the cache, not to any
	// provider — and the semantic hit must be counted. Fetch the
	// exposition via the metrics endpoint (auth-gated, same key).
	mrec := doReq(t, srv, "GET", "/metrics", "", "test-key-123")
	if mrec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", mrec.Code)
	}
	body := mrec.Body.String()
	if !strings.Contains(body, `codvian_router_requests_total{provider="cache",model="model_a",outcome="ok"} 1`) {
		t.Fatalf("metrics do not attribute the request to provider=cache:\n%s", body)
	}
	// Hit/miss counters are owned by the wrapper (degrade), not the
	// server — the server must not count them too, or every hit
	// would be double-counted. Assert the server counted no hit.
	if strings.Contains(body, `codvian_router_cache_hits_total{layer="semantic"}`) {
		t.Fatalf("server must not count cache hits (wrapper owns them):\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// Auto-router endpoints (GET /dashboard/auto-route, /last).
// ---------------------------------------------------------------------------

func newAutoTestRouter() *autorouter.Router {
	return autorouter.New(autorouter.Config{
		DefaultModel: "strong-model",
		TierModels: map[autorouter.Tier][]string{
			autorouter.TierCoding:    {"coder-model"},
			autorouter.TierChat:      {"chat-model"},
			autorouter.TierReasoning: {"reasoner-model"},
			autorouter.TierQuickQA:   {"qa-model"},
		},
	})
}

func TestAutoRouteClassify(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(newAutoTestRouter()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto%2Fcoding&prompt="+url.QueryEscape("Write a Python function to reverse a string"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("classify status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("classify body is not a Decision: %v", err)
	}
	if dec.Tier != autorouter.TierCoding {
		t.Errorf("decision tier = %s; want coding", dec.Tier)
	}
	if dec.Model != "coder-model" {
		t.Errorf("decision model = %q; want coder-model", dec.Model)
	}
	if dec.Confidence < 0.5 {
		t.Errorf("decision confidence = %.3f; want >= 0.5", dec.Confidence)
	}
	if dec.LatencyMicros <= 0 {
		t.Errorf("decision latency_micros = %d; want a measured positive value", dec.LatencyMicros)
	}
	if dec.Scores["coding"] == 0 {
		t.Error("decision carries no per-tier scores")
	}
}

func TestAutoRouteClassifyFallback(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(newAutoTestRouter()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt="+url.QueryEscape("zxqv blorp wumpus flibberty"), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("classify status = %d; want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("classify body is not a Decision: %v", err)
	}
	if !dec.Fallback || dec.Tier != autorouter.TierReasoning {
		t.Errorf("decision = %+v; want fallback to reasoning", dec)
	}
	if dec.Reason == "" {
		t.Error("fallback decision carries no reason")
	}
}

func TestAutoRouteClassifyErrors(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(newAutoTestRouter()))
	// No prompt.
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("missing prompt: status = %d; want 400", rec.Code)
	}
	// Unknown virtual model.
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=gpt-4o&prompt=hello", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown model: status = %d; want 400", rec.Code)
	}
	// Bad threshold.
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=hello&threshold=2", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("threshold 2: status = %d; want 400", rec.Code)
	}
	// Threshold override honoured: 0.99 forces fallback for a partial match.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt="+url.QueryEscape("What is the boiling point of water at sea level")+"&threshold=0.99", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("threshold override: status = %d; want 200", rec.Code)
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("threshold override body: %v", err)
	}
	if !dec.Fallback || dec.Threshold != 0.99 {
		t.Errorf("threshold override decision = %+v; want fallback at threshold 0.99", dec)
	}
	// POST is not allowed.
	if rec := doReq(t, srv, http.MethodPost, "/dashboard/auto-route?model=auto&prompt=hello", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d; want 405", rec.Code)
	}
}

func TestAutoRouteNoRouterConfigured(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=hello", "", ""); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no router: status = %d; want 503, never an invented decision", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route/last", "", ""); rec.Code != http.StatusOK {
		t.Errorf("last with no engine decision: status = %d; want 200 with has_decision=false", rec.Code)
	} else {
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("last body: %v", err)
		}
		if body["has_decision"] != false {
			t.Errorf("last body = %v; want has_decision=false", body)
		}
	}
}

func TestAutoRouteLastHonestEmptyState(t *testing.T) {
	// An engine that reports no decision: "last" must say so, not
	// invent one.
	srv := newTestServer(t, &fakeEngine{}, WithAutoRouter(newAutoTestRouter()))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route/last", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("last status = %d; want 200", rec.Code)
	}
	var body struct {
		HasDecision bool                `json:"has_decision"`
		Decision    autorouter.Decision `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("last body: %v", err)
	}
	if body.HasDecision {
		t.Errorf("last body = %+v; want has_decision=false before any auto request", body)
	}
}

func TestModelsListsAutoVirtualModels(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "ok"}}, WithAutoRouter(newAutoTestRouter()), WithModels("plain-model"))
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("models status = %d; want 200", rec.Code)
	}
	var body struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("models body: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range body.Data {
		seen[m.ID] = true
		if m.ID == "auto" && m.OwnedBy != "codvian-router" {
			t.Errorf("auto model owned_by = %q; want codvian-router", m.OwnedBy)
		}
	}
	for _, name := range autorouter.VirtualModels {
		if !seen[name] {
			t.Errorf("GET /v1/models missing virtual model %q", name)
		}
	}
	if !seen["plain-model"] {
		t.Error("GET /v1/models missing plain-model")
	}
	// Without an auto-router configured, no virtual models appear.
	srv2 := newTestServer(t, &fakeEngine{}, WithModels("plain-model"))
	rec2 := doReq(t, srv2, http.MethodGet, "/v1/models", "", "test-key-123")
	var body2 struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("models body (no router): %v", err)
	}
	for _, m := range body2.Data {
		if m.ID == "auto" {
			t.Error("GET /v1/models listed auto with no auto-router configured")
		}
	}
}

// coalEngine is a fakeEngine that also reports single-flight counters.
type coalEngine struct {
	*fakeEngine
	stats coalescing.Stats
}

func (e *coalEngine) CoalescingStats() coalescing.Stats { return e.stats }

func TestDashboardCoalescing(t *testing.T) {
	srv := newTestServer(t, &coalEngine{fakeEngine: &fakeEngine{}, stats: coalescing.Stats{
		Requests: 12, Flights: 3, Shared: 9, UpstreamAvoided: 9, QuotaSaved: 9, InFlight: 1,
	}})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/coalescing", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got coalescing.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Requests != 12 || got.Flights != 3 || got.Shared != 9 || got.UpstreamAvoided != 9 || got.QuotaSaved != 9 || got.InFlight != 1 {
		t.Fatalf("stats = %+v, want the engine's real counters", got)
	}
}

func TestDashboardCoalescingEmpty(t *testing.T) {
	// An engine that reports nothing answers zero counters — an
	// honest empty state, never invented figures.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/coalescing", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got coalescing.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != (coalescing.Stats{}) {
		t.Fatalf("stats = %+v, want all zero", got)
	}
}

func TestDashboardCoalescingMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard/coalescing", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// concEngine is a fakeEngine that also reports adaptive limiter state.
type concEngine struct {
	*fakeEngine
	states []adaptive.State
}

func (e *concEngine) ConcurrencyStats() []adaptive.State { return e.states }

func TestDashboardConcurrency(t *testing.T) {
	srv := newTestServer(t, &concEngine{fakeEngine: &fakeEngine{}, states: []adaptive.State{
		{Provider: "prov-slow", Limit: 2.5, InFlight: 2, RTTMs: 320, BaselineMs: 5, Samples: 12, Admitted: 20, Rejected: 4},
	}}, WithDashboardProviders(fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "prov-slow", Name: "Slow Provider", Enabled: true},
		{ID: "prov-fast", Name: "Fast Provider", Enabled: true},
	}}))
	rec := doReq(t, srv, http.MethodGet, "/dashboard/concurrency", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Providers []struct {
			ProviderID string  `json:"provider_id"`
			Name       string  `json:"name"`
			Limit      float64 `json:"limit"`
			InFlight   int     `json:"in_flight"`
			RTTMs      float64 `json:"rtt_ms"`
			BaselineMs float64 `json:"baseline_ms"`
			Samples    uint64  `json:"samples"`
			Rejected   uint64  `json:"rejected"`
		} `json:"providers"`
		TFO provider.TFOState `json:"tfo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %+v, want 2 rows (one per configured provider)", got.Providers)
	}
	row := got.Providers[0]
	if row.ProviderID != "prov-slow" || row.Name != "Slow Provider" || row.Limit != 2.5 || row.InFlight != 2 || row.RTTMs != 320 || row.BaselineMs != 5 || row.Samples != 12 || row.Rejected != 4 {
		t.Fatalf("slow row = %+v, want the engine's real limiter state", row)
	}
	// A provider with no traffic yet: the initial cap, zero samples
	// — an honest empty state, never an invented RTT.
	row2 := got.Providers[1]
	if row2.ProviderID != "prov-fast" || row2.Samples != 0 || row2.RTTMs != 0 || row2.BaselineMs != 0 {
		t.Fatalf("fast row = %+v, want zero samples and zero RTT", row2)
	}
	if row2.Limit != adaptive.DefaultInitialLimit {
		t.Fatalf("fast row limit = %v, want the initial cap %v", row2.Limit, adaptive.DefaultInitialLimit)
	}
	// TFO state is the real probe, with a reason — never empty.
	if got.TFO.Platform == "" || got.TFO.Reason == "" {
		t.Fatalf("tfo = %+v, want platform + reason from the probe", got.TFO)
	}
}

func TestDashboardConcurrencyEmpty(t *testing.T) {
	// An engine that reports nothing answers an empty provider list
	// with the TFO probe — an honest empty state, never invented limits.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/concurrency", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Providers []any             `json:"providers"`
		TFO       provider.TFOState `json:"tfo"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 0 {
		t.Fatalf("providers = %v, want empty", got.Providers)
	}
	if got.TFO.Platform == "" {
		t.Fatal("tfo has no platform")
	}
}

func TestDashboardConcurrencyMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard/concurrency", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}
