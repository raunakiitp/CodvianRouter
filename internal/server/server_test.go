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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// fakeEngine is a controllable Engine for tests. It can succeed, fail,
// or fail mid-stream.
type fakeEngine struct {
	chatResp  provider.ChatResponse
	chatErr   error
	deltas    []string
	streamErr error
	failAfter int // return streamErr after this many deltas; -1 = never fail
	blocked   chan struct{}
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

func newTestServer(t *testing.T, eng Engine, opts ...Option) *Server {
	t.Helper()
	all := append([]Option{WithStaticKeys("test-key-123"), WithLogger(log.New(io.Discard, "", 0))}, opts...)
	srv := New(eng, all...)
	// Close the server-owned stores at test end: on Windows an
	// open ccr.db blocks t.TempDir cleanup, failing the test at
	// teardown even when every assertion passed.
	t.Cleanup(func() { _ = srv.Close() })
	return srv
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
	for _, path := range []string{"/v1/models", "/metrics"} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s without key: status = %d, want 401", path, rec.Code)
		}
	}
	// /v1/chat/completions is POST-only: an unauthenticated GET is
	// refused by the mux with 405 before auth runs. That still proves
	// the point — no data, no bypass — but it is a method rejection,
	// not an auth rejection, so it asserts 405 rather than 401.
	// (A POST without a key still answers 401 — the mux matches the
	// POST pattern first, then the auth wrapper refuses it.)
	rec := doReq(t, srv, http.MethodGet, "/v1/chat/completions", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/chat/completions without key: status = %d, want 405 (POST-only route)", rec.Code)
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
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(fakeDashboardProviders{}), WithProviderRegistry(provider.NewRegistry(nil)))
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

// TestAPIProvidersVertexTokenCache proves CHECKLIST 284: GET
// /api/providers carries the shared Vertex minted-token cache's
// live totals (mints, cache hits, keys) — the exact numbers
// provider.VertexTokenCacheStats reports, never invented ones.
func TestAPIProvidersVertexTokenCache(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "vx", Name: "Vertex", Enabled: true, WireFormat: "vertex", Verification: "verified-from-docs", AuthKind: "service-account", CatalogID: "vertex", Models: 2},
	}}))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got apiProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	mints, hits, keys := provider.VertexTokenCacheStats()
	if got.VertexTokenCache.Mints != mints || got.VertexTokenCache.Hits != hits || got.VertexTokenCache.Keys != keys {
		t.Errorf("vertex_token_cache = %+v, want the live stats mints=%d hits=%d keys=%d", got.VertexTokenCache, mints, hits, keys)
	}
}

// fakeHealthStatusesWithHistory is a HealthStatusLister that also
// keeps probe history, like *health.Checker.
type fakeHealthStatusesWithHistory struct {
	fakeHealthStatuses
	histories map[string]health.HistorySummary
}

func (f fakeHealthStatusesWithHistory) History(providerID string) health.HistorySummary {
	if h, ok := f.histories[providerID]; ok {
		return h
	}
	return health.HistorySummary{ProviderID: providerID, Buckets: make([]health.HistoryBucket, 24)}
}

// TestAPIProvidersHistory proves GET /api/providers carries each
// provider's recorded probe history: real uptime percentages and
// latency percentiles where probes ran, null windows where none did,
// and no history field at all when the wired checker keeps none.
func TestAPIProvidersHistory(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", Models: 2},
		{ID: "p2", Name: "DeepSeek", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", Models: 1},
	}}
	up1h, up24h := 50.0, 75.0
	p50, p95 := int64(40), int64(120)
	buckets := make([]health.HistoryBucket, 24)
	buckets[23] = health.HistoryBucket{Up: 1, Down: 1, P50Ms: &p50}
	healthLister := fakeHealthStatusesWithHistory{
		fakeHealthStatuses: fakeHealthStatuses{statuses: []health.Status{
			{ProviderID: "p1", ProviderName: "Groq", OK: true},
			{ProviderID: "p2", ProviderName: "DeepSeek", OK: false},
		}},
		histories: map[string]health.HistorySummary{
			"p1": {ProviderID: "p1", Samples: 4, Uptime1h: &up1h, Uptime24h: &up24h, P50Ms: &p50, P95Ms: &p95, Buckets: buckets},
			// p2 has no probes: all windows nil, 24 empty buckets.
		},
	}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister), WithHealthChecker(healthLister))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got apiProvidersResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(got.Providers))
	}
	h := got.Providers[0].History
	if h == nil {
		t.Fatal("p1 history is nil, want recorded history")
	}
	if h.Samples != 4 || h.Uptime1h == nil || *h.Uptime1h != 50 || h.Uptime24h == nil || *h.Uptime24h != 75 {
		t.Errorf("p1 history uptime = %+v, want samples 4, 1h 50, 24h 75", h)
	}
	if h.P50Ms == nil || *h.P50Ms != 40 || h.P95Ms == nil || *h.P95Ms != 120 {
		t.Errorf("p1 history latency = p50 %v p95 %v, want 40/120", h.P50Ms, h.P95Ms)
	}
	if len(h.Buckets) != 24 {
		t.Fatalf("p1 buckets = %d, want 24", len(h.Buckets))
	}
	if h.Buckets[23].Up != 1 || h.Buckets[23].Down != 1 || h.Buckets[23].P50Ms == nil || *h.Buckets[23].P50Ms != 40 {
		t.Errorf("p1 last bucket = %+v, want up 1 down 1 p50 40", h.Buckets[23])
	}
	if h.Buckets[0].Up != 0 || h.Buckets[0].Down != 0 || h.Buckets[0].P50Ms != nil {
		t.Errorf("p1 first bucket = %+v, want empty (no data)", h.Buckets[0])
	}
	h2 := got.Providers[1].History
	if h2 == nil {
		t.Fatal("p2 history is nil, want an empty summary (no probes yet)")
	}
	if h2.Samples != 0 || h2.Uptime1h != nil || h2.Uptime24h != nil || h2.P50Ms != nil || h2.P95Ms != nil {
		t.Errorf("p2 history = %+v, want samples 0 and all windows nil", h2)
	}
	if len(h2.Buckets) != 24 {
		t.Errorf("p2 buckets = %d, want 24 empty buckets", len(h2.Buckets))
	}
}

// TestAPIProvidersHistoryAbsent proves a checker without history
// leaves the history field off the payload entirely (omitempty),
// instead of inventing one.
func TestAPIProvidersHistoryAbsent(t *testing.T) {
	lister := fakeDashboardProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Groq", Enabled: true, WireFormat: "openai", Verification: "verified-from-docs", Models: 2},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(lister), WithHealthChecker(fakeHealthStatuses{}))
	rec := doReq(t, srv, http.MethodGet, "/api/providers", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"history"`) {
		t.Errorf("payload contains a history field with a history-less checker: %s", rec.Body.String())
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
	// POST /api/providers is a real route now (the provider-add
	// endpoint, Tier 2): without a key it is 401, with a key and an
	// empty body it is 400 — never a 405. The GET feed itself still
	// answers only GET (see TestAPIProvidersEmpty).
	srv := newTestServer(t, &fakeEngine{}, WithDashboardProviders(fakeDashboardProviders{}), WithProviderRegistry(provider.NewRegistry(nil)))
	if rec := doReq(t, srv, http.MethodPost, "/api/providers", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST without key: status = %d, want 401", rec.Code)
	}
	if rec := doReq(t, srv, http.MethodPost, "/api/providers", "", "test-key-123"); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST with key, empty body: status = %d, want 400", rec.Code)
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

// failoverGateLedger is a router.Ledger that records every attempt
// the engine makes, so the failover gate test can prove from ledger
// evidence — not just the HTTP status — which provider failed and
// which one actually served the request.
type failoverGateLedger struct {
	mu   sync.Mutex
	rows []failoverGateLedgerRow
}

type failoverGateLedgerRow struct {
	provider string
	model    string
	ok       bool
}

func (l *failoverGateLedger) RecordAttempt(providerID, model string, ok bool, _ int64, _ provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, failoverGateLedgerRow{provider: providerID, model: model, ok: ok})
}

func (l *failoverGateLedger) snapshot() []failoverGateLedgerRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]failoverGateLedgerRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// TestChatCompletionsSameRequestFailoverDeadModel is the permanent
// failover QA gate (CHECKLIST 103-104; research-log entry 149):
// agentgateway-style routers only switch from the NEXT request on,
// leaving the first one to fail. Codvian Router's rule is
// "429/limits never reach the user": a request hitting a dead model
// must succeed via fallback in THAT SAME request.
//
// ONE POST /v1/chat/completions goes through the real server, the
// real router.Engine and real provider clients against local
// httptest mocks — no fakeEngine, no fakeClient. The primary mock
// is dead in three ways (429, 500, never-responding) and the single
// request must still return 200 with the fallback provider's
// content. The test fails if the router returns the upstream error
// or only heals the next request: the dead mock must be hit exactly
// once, the fallback exactly once, the ledger must show one failed
// dead attempt followed by one successful fallback attempt, the
// route trace must show the fallback serving this request, and the
// dead provider's error text must not leak into the response body.
func TestChatCompletionsSameRequestFailoverDeadModel(t *testing.T) {
	for _, tc := range []struct {
		name         string
		deadStatus   int    // 0 = never responds (hangs until the client times out)
		deadBody     string // error text that must never leak into the response
		deadTimeout  time.Duration
		wantOutcome0 string // route-trace outcome of the dead attempt
	}{
		{"dead-429", http.StatusTooManyRequests, "rate limited by dead-model", 2 * time.Second, "rate_limited"},
		{"dead-500", http.StatusInternalServerError, "internal boom", 2 * time.Second, "error"},
		{"dead-hang", 0, "", 50 * time.Millisecond, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadHits, fallbackHits atomic.Int32
			deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deadHits.Add(1)
				if tc.deadStatus == 0 {
					// Never responds: block until the caller
					// gives up (client timeout) or the test
					// ends. The 200ms arm is a safety net so
					// the handler never outlives the test by
					// much (httptest Close waits for outstanding
					// handlers); it is never reached in practice
					// because the client's 50ms timeout aborts
					// the request first.
					select {
					case <-r.Context().Done():
						return
					case <-time.After(200 * time.Millisecond):
						w.WriteHeader(http.StatusOK)
						_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"too late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.deadStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"` + tc.deadBody + `","type":"server_error"}}`))
			}))
			defer deadSrv.Close()
			fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			}))
			defer fallbackSrv.Close()

			deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithTimeout(tc.deadTimeout))
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
				HedgeDelay:       -1, // hedging off: deterministic attempt order
				BackoffBase:      time.Millisecond,
				BackoffMax:       5 * time.Millisecond,
				BreakerThreshold: 5,
				BreakerOpenFor:   time.Minute,
				RetryAfterCap:    30 * time.Second,
				ChainFor: func(model string) []router.Attempt {
					if model != "gate-model" {
						return nil
					}
					return []router.Attempt{{ProviderID: "dead", Model: "gate-model"}, {ProviderID: "fallback", Model: "gate-model"}}
				},
			})
			rec := metrics.NewRecorder()
			// Server timeout is generous (10s) so a hang can only
			// be bounded by the dead client's own timeout (50ms);
			// the elapsed assertion below catches it if failover
			// ever waits the client out instead of switching.
			srv := newTestServer(t, eng, WithMetrics(rec), WithTimeout(10*time.Second))

			start := time.Now()
			resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
				`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
			elapsed := time.Since(start)
			if elapsed > 5*time.Second {
				t.Fatalf("single request took %v, want < 5s (a hanging dead model must fail over fast, not burn the request)", elapsed)
			}
			if resp.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: the SAME request must succeed via fallback, not return the upstream error (body %s)", resp.Code, resp.Body.String())
			}
			var out struct {
				Model   string `json:"model"`
				Choices []struct {
					Message provider.Message `json:"message"`
				} `json:"choices"`
				Usage provider.Usage `json:"usage"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode response: %v (body %s)", err, resp.Body.String())
			}
			if out.Model != "gate-model" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "fallback says hi" {
				t.Fatalf("response = %+v, want model gate-model with content %q (the fallback's content, not the dead provider's)", out, "fallback says hi")
			}
			if out.Usage.TotalTokens != 7 {
				t.Fatalf("usage = %+v, want total 7 (the fallback's usage)", out.Usage)
			}
			if tc.deadBody != "" && strings.Contains(resp.Body.String(), tc.deadBody) {
				t.Fatalf("response body leaks the dead provider's error text %q: %s", tc.deadBody, resp.Body.String())
			}
			// Same-request proof: exactly one hit on each mock.
			// A router that only heals the NEXT request would show
			// dead=1, fallback=0 here and a non-200 above.
			if got := deadHits.Load(); got != 1 {
				t.Fatalf("dead mock hits = %d, want 1", got)
			}
			if got := fallbackHits.Load(); got != 1 {
				t.Fatalf("fallback mock hits = %d, want 1 (the fallback must serve THIS request)", got)
			}
			// Ledger evidence: failed dead attempt, then the
			// successful fallback attempt — in that order.
			rows := led.snapshot()
			if len(rows) != 2 {
				t.Fatalf("ledger rows = %+v, want exactly 2", rows)
			}
			if rows[0] != (failoverGateLedgerRow{provider: "dead", model: "gate-model", ok: false}) {
				t.Fatalf("ledger row 0 = %+v, want dead failed", rows[0])
			}
			if rows[1] != (failoverGateLedgerRow{provider: "fallback", model: "gate-model", ok: true}) {
				t.Fatalf("ledger row 1 = %+v, want fallback succeeded", rows[1])
			}
			// Route-trace evidence: the fallback served this request.
			trace := eng.LastTrace()
			if trace == nil || len(trace.Attempts) != 2 {
				t.Fatalf("trace = %+v, want 2 attempts", trace)
			}
			if trace.Attempts[0].Provider != "dead" || trace.Attempts[0].Outcome != tc.wantOutcome0 {
				t.Fatalf("trace attempt 0 = %+v, want dead/%s", trace.Attempts[0], tc.wantOutcome0)
			}
			if trace.Attempts[1].Provider != "fallback" || trace.Attempts[1].Outcome != "success" {
				t.Fatalf("trace attempt 1 = %+v, want fallback/success", trace.Attempts[1])
			}
			if info, ok := eng.LastRouting(); !ok || info.Provider != "fallback" {
				t.Fatalf("LastRouting = %+v, ok=%v, want provider fallback", info, ok)
			}
			// Metrics evidence: the failover counter attributes
			// the switch dead -> fallback.
			m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
			mbody := m.Body.String()
			if !strings.Contains(mbody, "codvian_router_failovers_total{") ||
				!strings.Contains(mbody, `from_provider="dead"`) ||
				!strings.Contains(mbody, `to_provider="fallback"`) {
				t.Fatalf("metrics missing failover counter dead->fallback:\n%s", mbody)
			}
		})
	}
}

// TestChatCompletionsBreakerNeverTripsOn4xx is the permanent gate
// for CHECKLIST 151: the circuit breaker must never trip on 4xx,
// proven end to end through the real Server, the real
// router.Engine and real provider clients against local httptest
// mocks — no fakeEngine.
//
// (a) Five consecutive 401s from one provider leave its breaker
// CLOSED in the live GET /dashboard/cooldown feed (0 counted
// failures, 5 ignored 4xx), and the provider's live model still
// serves once the key is fixed — the breaker never took the whole
// provider out. (b) Five consecutive 500s from another provider
// trip its breaker exactly as before (OPEN, 5 counted failures,
// reason upstream_error), and the next request is skipped by the
// breaker without dialling the mock again.
func TestChatCompletionsBreakerNeverTripsOn4xx(t *testing.T) {
	var badKeyHits, flakyHits atomic.Int32
	var keyFixed atomic.Bool
	badKeySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badKeyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if keyFixed.Load() {
			_, _ = w.Write([]byte(`{"id":"chatcmpl-fixed","object":"chat.completion","model":"bad-model","choices":[{"message":{"role":"assistant","content":"live model answers"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`))
	}))
	defer badKeySrv.Close()
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flakyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"internal boom","type":"server_error"}}`))
	}))
	defer flakySrv.Close()

	badKeyClient, err := provider.NewClient(badKeySrv.URL, "bad-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient badkey: %v", err)
	}
	flakyClient, err := provider.NewClient(flakySrv.URL, "flaky-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient flaky: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"badkey": badKeyClient, "flaky": flakyClient}, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			switch model {
			case "bad-model":
				return []router.Attempt{{ProviderID: "badkey", Model: "bad-model"}}
			case "flaky-model":
				return []router.Attempt{{ProviderID: "flaky", Model: "flaky-model"}}
			}
			return nil
		},
	})
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))

	chat := func(model string) *httptest.ResponseRecorder {
		return doReq(t, srv, http.MethodPost, "/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	}
	breakersByID := func() map[string]breakerJSON {
		feed := getCooldown(t, srv)
		out := map[string]breakerJSON{}
		for _, b := range feed.Breakers {
			out[b.ProviderID] = b
		}
		return out
	}

	// (a) Five consecutive 401s: the server answers 401 (the
	// terminal upstream 4xx maps to its own status), the mock is
	// hit exactly once per request, and the breaker stays closed.
	for i := 0; i < 5; i++ {
		rec := chat("bad-model")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("401 request %d: status = %d, want 401 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := badKeyHits.Load(); got != 5 {
		t.Fatalf("bad-key mock hits = %d, want 5 (every request must still reach the provider)", got)
	}
	bs := breakersByID()
	bad := bs["badkey"]
	if bad.Open || bad.ConsecutiveFailures != 0 {
		t.Fatalf("breaker badkey = %+v, want CLOSED with 0 counted failures after 5x 401", bad)
	}
	if bad.Ignored4xx != 5 {
		t.Fatalf("breaker badkey ignored_4xx = %d, want 5", bad.Ignored4xx)
	}
	if bad.LastReason != "" {
		t.Fatalf("breaker badkey last_reason = %q, want empty (no breaker-counted failure)", bad.LastReason)
	}
	// The provider's live model still routes once the key is
	// fixed: the breaker never took the whole provider out.
	keyFixed.Store(true)
	rec := chat("bad-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("request after the key fix: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "live model answers") {
		t.Fatalf("response after the key fix = %s, want the live model's content", rec.Body.String())
	}
	if got := badKeyHits.Load(); got != 6 {
		t.Fatalf("bad-key mock hits = %d, want 6 (the fixed-key request must reach the provider, not a half-open probe wall)", got)
	}

	// (b) Five consecutive 500s trip the other provider's breaker
	// exactly as before.
	for i := 0; i < 5; i++ {
		rec := chat("flaky-model")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("500 request %d: status = %d, want 502 (exhausted chain, body %s)", i+1, rec.Code, rec.Body.String())
		}
	}
	if got := flakyHits.Load(); got != 5 {
		t.Fatalf("flaky mock hits = %d, want 5", got)
	}
	bs = breakersByID()
	flaky := bs["flaky"]
	if !flaky.Open || flaky.ConsecutiveFailures != 5 {
		t.Fatalf("breaker flaky = %+v, want OPEN with 5 counted failures after 5x 500", flaky)
	}
	if flaky.LastReason != "upstream_error" {
		t.Fatalf("breaker flaky last_reason = %q, want upstream_error", flaky.LastReason)
	}
	if flaky.RemainingSeconds <= 0 {
		t.Fatalf("breaker flaky remaining_seconds = %d, want > 0 while open", flaky.RemainingSeconds)
	}
	// The next request is skipped by the breaker: the mock is
	// not dialled again, and the trace says so.
	rec = chat("flaky-model")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("request with open breaker: status = %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if got := flakyHits.Load(); got != 5 {
		t.Fatalf("flaky mock hits = %d, want still 5 (open breaker must skip the provider)", got)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "skipped_breaker" {
		t.Fatalf("trace = %+v, want exactly one skipped_breaker attempt", trace)
	}
}

// TestChatCompletionsStreamingPrereadGate is the permanent gate for
// CHECKLIST 133 (research-log entries 97 + 111): the streaming
// preread buffer, proven end to end through the real server, the
// real router.Engine and real provider clients against local
// httptest mocks — no fakeEngine.
//
// (a) A primary that streams [DONE] with no content fails over in
// the SAME request: 200 with the fallback's content, and the body
// contains no partial body from the primary (it sent none). (b) A
// primary that errors before the first byte fails over the same
// way. (c) A primary that sends one content chunk and then dies
// mid-stream does NOT switch: the client keeps the chunk it got,
// the stream ends with an honest SSE error frame followed by
// [DONE] (CHECKLIST 153: every committed stream ends with a
// terminal event — the error frame alone used to leave a client
// waiting forever for the terminator), and the fallback mock is
// never hit.
func TestChatCompletionsStreamingPrereadGate(t *testing.T) {
	const chunkHello = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"
	const chunkFallback1 = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fallback \"},\"finish_reason\":null}]}\n\n"
	const chunkFallback2 = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"says hi\"},\"finish_reason\":null}]}\n\n"
	const chunkFinish = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	for _, tc := range []struct {
		name           string
		primary        string // empty | http500 | content-then-die
		wantBody       []string
		wantNotBody    []string
		wantOutcome0   string
		wantPreread0   string
		wantAttempts   int
		wantFallback   int
		wantDeadHits   int
		wantLedgerRows []failoverGateLedgerRow
	}{
		{
			name:         "empty stream fails over",
			primary:      "empty",
			wantBody:     []string{`"content":"fallback "`, `"content":"says hi"`, "data: [DONE]"},
			wantOutcome0: "empty_stream", wantPreread0: "empty",
			// M4.8: an empty stream is an early-retryable blip, so
			// the dead target is re-opened (initial + 3 re-opens;
			// the reserve guard leaves 1 of the 5 attempts for
			// the fallback) before the request fails over. The
			// outcome is unchanged — the fallback serves — only
			// the same-target attempt count grows 1 → 4.
			wantAttempts: 5, wantFallback: 1, wantDeadHits: 4,
			wantLedgerRows: []failoverGateLedgerRow{{provider: "dead", model: "gate-model", ok: false}, {provider: "dead", model: "gate-model", ok: false}, {provider: "dead", model: "gate-model", ok: false}, {provider: "dead", model: "gate-model", ok: false}, {provider: "fallback", model: "gate-model", ok: true}},
		},
		{
			name:         "error before first byte fails over",
			primary:      "http500",
			wantBody:     []string{`"content":"fallback "`, `"content":"says hi"`, "data: [DONE]"},
			wantOutcome0: "error", wantPreread0: "",
			wantAttempts: 2, wantFallback: 1, wantDeadHits: 1,
			wantLedgerRows: []failoverGateLedgerRow{{provider: "dead", model: "gate-model", ok: false}, {provider: "fallback", model: "gate-model", ok: true}},
		},
		{
			name:         "content then death never switches",
			primary:      "content-then-die",
			wantBody:     []string{`"content":"Hello "`, "upstream provider error", "data: [DONE]"},
			wantNotBody:  []string{"fallback"},
			wantOutcome0: "stream_error", wantPreread0: "held",
			wantAttempts: 1, wantFallback: 0, wantDeadHits: 1,
			wantLedgerRows: []failoverGateLedgerRow{{provider: "dead", model: "gate-model", ok: false}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadHits, fallbackHits atomic.Int32
			dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deadHits.Add(1)
				switch tc.primary {
				case "empty":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("data: [DONE]\n\n"))
				case "http500":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
				case "content-then-die":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(chunkHello))
					w.(http.Flusher).Flush()
					// A malformed SSE line: the real OpenAI
					// client fails the stream on it — the
					// deterministic stand-in for "upstream dies"
					// after the first content byte.
					_, _ = w.Write([]byte("this is not valid sse\n\n"))
					w.(http.Flusher).Flush()
				}
			}))
			defer dead.Close()
			fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(chunkFallback1))
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte(chunkFallback2))
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte(chunkFinish))
				w.(http.Flusher).Flush()
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				w.(http.Flusher).Flush()
			}))
			defer fallback.Close()
			led := &failoverGateLedger{}
			deadClient, err := provider.NewClient(dead.URL, "dead-key", provider.WithTimeout(2*time.Second))
			if err != nil {
				t.Fatalf("NewClient dead: %v", err)
			}
			fallbackClient, err := provider.NewClient(fallback.URL, "fallback-key", provider.WithTimeout(2*time.Second))
			if err != nil {
				t.Fatalf("NewClient fallback: %v", err)
			}
			eng := router.NewEngine(map[string]provider.Client{
				"dead":     deadClient,
				"fallback": fallbackClient,
			}, led, router.Config{
				MaxAttempts:      5,
				HedgeDelay:       -1,
				BackoffBase:      time.Millisecond,
				BackoffMax:       5 * time.Millisecond,
				BreakerThreshold: 5,
				BreakerOpenFor:   time.Minute,
				RetryAfterCap:    30 * time.Second,
				ChainFor: func(model string) []router.Attempt {
					return []router.Attempt{{ProviderID: "dead", Model: model}, {ProviderID: "fallback", Model: model}}
				},
			})
			srv := newTestServer(t, eng, WithTimeout(10*time.Second))
			rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
				`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range tc.wantBody {
				if !strings.Contains(body, want) {
					t.Fatalf("body missing %q:\n%s", want, body)
				}
			}
			for _, not := range tc.wantNotBody {
				if strings.Contains(body, not) {
					t.Fatalf("body must not contain %q:\n%s", not, body)
				}
			}
			if got := deadHits.Load(); got != int32(tc.wantDeadHits) {
				t.Fatalf("dead hits = %d, want %d", got, tc.wantDeadHits)
			}
			if got := fallbackHits.Load(); got != int32(tc.wantFallback) {
				t.Fatalf("fallback hits = %d, want %d", got, tc.wantFallback)
			}
			trace := eng.LastTrace()
			if trace == nil || len(trace.Attempts) != tc.wantAttempts {
				t.Fatalf("trace = %+v, want %d attempts", trace, tc.wantAttempts)
			}
			if trace.Attempts[0].Provider != "dead" || trace.Attempts[0].Outcome != tc.wantOutcome0 {
				t.Fatalf("trace attempt 0 = %+v, want dead/%s", trace.Attempts[0], tc.wantOutcome0)
			}
			if trace.Attempts[0].Preread != tc.wantPreread0 {
				t.Fatalf("trace attempt 0 preread = %q, want %q", trace.Attempts[0].Preread, tc.wantPreread0)
			}
			if tc.wantFallback == 1 {
				last := trace.Attempts[len(trace.Attempts)-1]
				if last.Provider != "fallback" || last.Outcome != "success" {
					t.Fatalf("trace last attempt = %+v, want fallback/success", last)
				}
			}
			if got, want := led.snapshot(), tc.wantLedgerRows; len(got) != len(want) {
				t.Fatalf("ledger rows = %+v, want %+v", got, want)
			} else {
				for i := range want {
					if got[i] != want[i] {
						t.Fatalf("ledger row %d = %+v, want %+v", i, got[i], want[i])
					}
				}
			}
		})
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

func TestChatCompletionsStreamingMidFailureErrorFrame(t *testing.T) {
	// CHECKLIST 133 (research-log entries 97 + 111): once the first
	// content byte is sent, the stream NEVER switches candidates —
	// no resume. A failure after "Hello " ends the stream with an
	// honest SSE error frame, not a resumed "world" — and, since
	// CHECKLIST 153, with [DONE] after the error frame, so a client
	// waiting on the terminator never hangs.
	eng := &fakeEngine{
		deltas:    []string{"Hello ", "world"},
		failAfter: 1, // send "Hello ", then fail before "world"
		streamErr: errors.New("upstream died mid-stream"),
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed with the first chunk): %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if n := strings.Count(body, "Hello"); n != 1 {
		t.Fatalf("sent content %q appears %d times, want 1: %s", "Hello", n, body)
	}
	if strings.Contains(body, "world") {
		t.Fatalf("resumed content leaked into the stream (no switch after commit): %s", body)
	}
	if !strings.Contains(body, `"upstream provider error"`) {
		t.Fatalf("stream does not end with the honest SSE error frame: %s", body)
	}
	// Always-terminal (CHECKLIST 153): the error frame is followed
	// by [DONE], in that order, and the stream ends with it.
	errIdx := strings.Index(body, `"upstream provider error"`)
	doneIdx := strings.Index(body, "data: [DONE]")
	if doneIdx < 0 {
		t.Fatalf("stream does not end with [DONE] after the mid-stream error frame: %s", body)
	}
	if doneIdx < errIdx {
		t.Fatalf("[DONE] at %d comes before the error frame at %d: %s", doneIdx, errIdx, body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %s", body)
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
	// Metrics are disabled only when BOTH the recorder and the
	// observability collector are nil (M2): a default collector
	// is constructed in New, so disabling the recorder alone
	// still serves the collector's exposition.
	srv := newTestServer(t, &fakeEngine{}, WithTelemetryCollector(nil))
	got := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if got.Code != http.StatusServiceUnavailable {
		t.Fatalf("metrics without recorder or collector: status = %d, want 503", got.Code)
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
	// server's legacy recorder — the recorder must not count them
	// too, or every hit would be double-counted. The M2
	// observability collector DOES count the hit once (its
	// per-layer cache-hit series is the M2 cache-ratio source),
	// so the invariant is exactly one such series in the combined
	// exposition: one owner, never two.
	if n := strings.Count(body, `codvian_router_cache_hits_total{layer="semantic"} 1`); n != 1 {
		t.Fatalf("cache-hit series counted %d times, want exactly 1 (collector only, recorder must not double-count):\n%s", n, body)
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
	// LatencyMicros is a measured value, but on Windows the
	// clock can return identical timestamps for a
	// sub-microsecond classify (time.Since is 0 over short
	// spans on this PC), so 0 is a valid measurement here —
	// only a negative value would mean the decision carries
	// no timing at all.
	if dec.LatencyMicros < 0 {
		t.Errorf("decision latency_micros = %d; want a measured non-negative value", dec.LatencyMicros)
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

// TestChatCompletionsDeadlineBudgetIs504 proves the deadline
// sub-budget's server half (research-log entry 147): when the
// engine stops a combo with ErrDeadlineExceeded, the client gets
// a clean 504 "deadline budget exceeded" — not a 502, not a
// generic upstream error, and no upstream detail leaked.
func TestChatCompletionsDeadlineBudgetIs504(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatErr: router.ErrDeadlineExceeded})
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline-exceeded chat: status %d, want 504", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "deadline budget exceeded") {
		t.Errorf("504 body = %s, want the clean deadline message", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "context deadline") {
		t.Errorf("504 body leaks engine internals: %s", rec.Body.String())
	}
}

// TestChatCompletionsModelNotFoundFallback is the permanent gate for
// CHECKLIST 121-122 (research-log entries 151 + 183): the 400/404
// model-not-found split, proven in BOTH directions through one POST
// /v1/chat/completions over the real Server + real router.Engine +
// real provider clients against local httptest mocks.
//
// (a) dead-model 404: the primary mock answers 404 "model not
// found" and the SAME request must return 200 with the fallback's
// content — dead hit exactly once, fallback hit exactly once,
// ledger dead/failed then fallback/ok, trace reason
// "model_not_found" on the dead attempt, and the same reason
// visible from GET /dashboard/trace.
//
// (b) malformed 400: the primary mock answers 400 "temperature must
// be <= 1" (no model signal) and the request must return 400
// immediately — the fallback mock is never hit, the ledger shows
// exactly one failed row, the trace shows exactly one attempt with
// reason "request_invalid", and the upstream's error text never
// leaks into the response body.
//
// Sensitivity: the test FAILS if the router returns the upstream
// error for (a) (no fallback on a dead model), or dials the
// fallback for (b) (a malformed request burning a second provider
// call), or maps (b) to a 502 instead of the honest 400.
func TestChatCompletionsModelNotFoundFallback(t *testing.T) {
	for _, tc := range []struct {
		name       string
		deadStatus int
		deadBody   string
		wantStatus int
		wantReason string
	}{
		{
			"dead-model-404",
			http.StatusNotFound,
			`{"error":{"message":"The model ` + "`gate-model`" + ` does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			http.StatusOK,
			"model_not_found",
		},
		{
			"malformed-400",
			http.StatusBadRequest,
			`{"error":{"message":"temperature must be <= 1","type":"invalid_request_error"}}`,
			http.StatusBadRequest,
			"request_invalid",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deadHits, fallbackHits atomic.Int32
			deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				deadHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.deadStatus)
				_, _ = w.Write([]byte(tc.deadBody))
			}))
			defer deadSrv.Close()
			fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fallbackHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
			}))
			defer fallbackSrv.Close()

			deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithTimeout(2*time.Second))
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
				HedgeDelay:       -1, // hedging off: deterministic attempt order
				BackoffBase:      time.Millisecond,
				BackoffMax:       5 * time.Millisecond,
				BreakerThreshold: 5,
				BreakerOpenFor:   time.Minute,
				RetryAfterCap:    30 * time.Second,
				ChainFor: func(model string) []router.Attempt {
					if model != "gate-model" {
						return nil
					}
					return []router.Attempt{{ProviderID: "dead", Model: "gate-model"}, {ProviderID: "fallback", Model: "gate-model"}}
				},
			})
			rec := metrics.NewRecorder()
			srv := newTestServer(t, eng, WithMetrics(rec), WithTimeout(10*time.Second))

			resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
				`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
			if resp.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", resp.Code, tc.wantStatus, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), "does not exist") || strings.Contains(resp.Body.String(), "temperature must be") {
				t.Fatalf("response body leaks the dead provider's error text: %s", resp.Body.String())
			}
			if got := deadHits.Load(); got != 1 {
				t.Fatalf("dead mock hits = %d, want 1", got)
			}

			// The dashboard trace feed must show the same
			// attempts the engine recorded, with the fallback
			// reason on the failed attempt.
			traceResp := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
			if traceResp.Code != http.StatusOK {
				t.Fatalf("GET /dashboard/trace status = %d, want 200", traceResp.Code)
			}
			var traceBody struct {
				Attempts []struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
					Outcome  string `json:"outcome"`
					Reason   string `json:"reason"`
				} `json:"attempts"`
			}
			if err := json.Unmarshal(traceResp.Body.Bytes(), &traceBody); err != nil {
				t.Fatalf("decode /dashboard/trace: %v (body %s)", err, traceResp.Body.String())
			}

			if tc.wantStatus == http.StatusOK {
				// (a) dead-model 404: same request succeeds via fallback.
				var out struct {
					Choices []struct {
						Message provider.Message `json:"message"`
					} `json:"choices"`
					Usage provider.Usage `json:"usage"`
				}
				if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode response: %v (body %s)", err, resp.Body.String())
				}
				if len(out.Choices) != 1 || out.Choices[0].Message.Content != "fallback says hi" {
					t.Fatalf("response = %+v, want the fallback's content", out)
				}
				if out.Usage.TotalTokens != 7 {
					t.Fatalf("usage = %+v, want total 7 (the fallback's usage)", out.Usage)
				}
				if got := fallbackHits.Load(); got != 1 {
					t.Fatalf("fallback mock hits = %d, want 1 (the fallback must serve THIS request)", got)
				}
				rows := led.snapshot()
				if len(rows) != 2 {
					t.Fatalf("ledger rows = %+v, want exactly 2", rows)
				}
				if rows[0] != (failoverGateLedgerRow{provider: "dead", model: "gate-model", ok: false}) {
					t.Fatalf("ledger row 0 = %+v, want dead failed", rows[0])
				}
				if rows[1] != (failoverGateLedgerRow{provider: "fallback", model: "gate-model", ok: true}) {
					t.Fatalf("ledger row 1 = %+v, want fallback succeeded", rows[1])
				}
				if len(traceBody.Attempts) != 2 {
					t.Fatalf("trace attempts = %+v, want 2", traceBody.Attempts)
				}
				if traceBody.Attempts[0].Provider != "dead" || traceBody.Attempts[0].Outcome != "error" || traceBody.Attempts[0].Reason != tc.wantReason {
					t.Fatalf("trace attempt 0 = %+v, want dead/error/%s", traceBody.Attempts[0], tc.wantReason)
				}
				if traceBody.Attempts[1].Provider != "fallback" || traceBody.Attempts[1].Outcome != "success" {
					t.Fatalf("trace attempt 1 = %+v, want fallback/success", traceBody.Attempts[1])
				}
				m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
				if !strings.Contains(m.Body.String(), `from_provider="dead"`) || !strings.Contains(m.Body.String(), `to_provider="fallback"`) {
					t.Fatalf("metrics missing failover counter dead->fallback:\n%s", m.Body.String())
				}
			} else {
				// (b) malformed 400: NO fallback attempt at all.
				if got := fallbackHits.Load(); got != 0 {
					t.Fatalf("fallback mock hits = %d, want 0 — a malformed request must never be dialled on a second target", got)
				}
				rows := led.snapshot()
				if len(rows) != 1 {
					t.Fatalf("ledger rows = %+v, want exactly 1 (no fallback row)", rows)
				}
				if rows[0] != (failoverGateLedgerRow{provider: "dead", model: "gate-model", ok: false}) {
					t.Fatalf("ledger row 0 = %+v, want dead failed", rows[0])
				}
				if len(traceBody.Attempts) != 1 {
					t.Fatalf("trace attempts = %+v, want exactly 1", traceBody.Attempts)
				}
				if traceBody.Attempts[0].Provider != "dead" || traceBody.Attempts[0].Outcome != "error" || traceBody.Attempts[0].Reason != tc.wantReason {
					t.Fatalf("trace attempt 0 = %+v, want dead/error/%s", traceBody.Attempts[0], tc.wantReason)
				}
			}
		})
	}
}

// TestDashboardTraceEmpty proves the trace feed's honest empty
// state: before any request it answers 200 with an empty attempts
// list, never invented traffic. (The fake engine has no trace, so
// the feed must still answer — it must not 500 or 404.)
func TestDashboardTraceEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	resp := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if resp.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/trace status = %d, want 200", resp.Code)
	}
	var body struct {
		Attempts []any `json:"attempts"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /dashboard/trace: %v (body %s)", err, resp.Body.String())
	}
	if body.Attempts == nil || len(body.Attempts) != 0 {
		t.Fatalf("attempts = %v, want an empty list before any request", body.Attempts)
	}
}

// TestDashboardTraceMethodNotAllowed proves GET-only: POST answers 405.
func TestDashboardTraceMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	resp := doReq(t, srv, http.MethodPost, "/dashboard/trace", "", "")
	if resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /dashboard/trace status = %d, want 405", resp.Code)
	}
}

// TestChatCompletionsRetryAfterAbandonTrace is the permanent gate for
// CHECKLIST 167-168: a real Retry-After: 120 header on a 429 must
// abandon that target for the request — one POST /v1/chat/completions
// through the real Server + real router.Engine + real provider
// clients against local httptest mocks succeeds via the fallback in
// the SAME request (dead hit exactly once, fallback hit exactly
// once, fast), and GET /dashboard/trace shows the Retry-After record
// (120s, abandoned, 0s waited) on the dead attempt, so the
// dashboard's Live Routing view can render the decision honestly.
func TestChatCompletionsRetryAfterAbandonTrace(t *testing.T) {
	var deadHits, fallbackHits atomic.Int32
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited by dead-model","type":"server_error"}}`))
	}))
	defer deadSrv.Close()
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}))
	defer fallbackSrv.Close()

	deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithTimeout(2*time.Second))
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
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "dead", Model: "gate-model"}, {ProviderID: "fallback", Model: "gate-model"}}
		},
	})
	rec := metrics.NewRecorder()
	srv := newTestServer(t, eng, WithMetrics(rec), WithTimeout(10*time.Second))

	start := time.Now()
	resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("single request took %v, want < 5s (an abandoned target must not park the request)", elapsed)
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
	if got := deadHits.Load(); got != 1 {
		t.Fatalf("dead mock hits = %d, want 1 (abandoned, never retried)", got)
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback mock hits = %d, want 1", got)
	}

	traceResp := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if traceResp.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/trace status = %d, want 200", traceResp.Code)
	}
	var traceBody struct {
		Attempts []struct {
			Provider   string `json:"provider"`
			Outcome    string `json:"outcome"`
			RetryAfter *struct {
				Seconds     float64 `json:"seconds"`
				Decision    string  `json:"decision"`
				WaitSeconds float64 `json:"wait_seconds"`
			} `json:"retry_after"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(traceResp.Body.Bytes(), &traceBody); err != nil {
		t.Fatalf("decode /dashboard/trace: %v (body %s)", err, traceResp.Body.String())
	}
	if len(traceBody.Attempts) != 2 {
		t.Fatalf("trace attempts = %d, want 2", len(traceBody.Attempts))
	}
	ra := traceBody.Attempts[0].RetryAfter
	if ra == nil {
		t.Fatalf("attempt 0 has no retry_after record, want the 120s hint + abandoned decision")
	}
	if ra.Seconds != 120 || ra.Decision != "abandoned" || ra.WaitSeconds != 0 {
		t.Fatalf("attempt 0 retry_after = %+v, want {120 abandoned 0}", ra)
	}
	if traceBody.Attempts[0].Outcome != "rate_limited" {
		t.Fatalf("attempt 0 outcome = %q, want rate_limited", traceBody.Attempts[0].Outcome)
	}
	if traceBody.Attempts[1].RetryAfter != nil {
		t.Fatalf("attempt 1 retry_after = %+v, want nil (the healthy target carried no hint)", traceBody.Attempts[1].RetryAfter)
	}
}
