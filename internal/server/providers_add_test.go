package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// mockUpstream is a minimal OpenAI-compatible upstream for the
// add-provider tests: /v1/models answers the model list, and
// /v1/chat/completions answers one chat completion. badKey makes
// /v1/models answer 401, so the key test fails the way a wrong key
// fails against a real upstream.
type mockUpstream struct {
	badKey bool
}

func (m mockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			if m.badKey {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"mock-model-a"},{"id":"mock-model-b"}]}`))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"mock-model-a","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newAddTestServer(t *testing.T) (*Server, *provider.Registry) {
	t.Helper()
	reg := provider.NewRegistry(nil) // memory-only; ephemeral cipher is fine here
	return newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg)), reg
}

func postAdd(t *testing.T, srv *Server, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, "POST", "/api/providers", body, key)
}

func TestAddProviderCatalog(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, reg := newAddTestServer(t)

	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"`+mock.URL+`"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers catalog: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if created.Name != "Groq" || created.CatalogID != "groq" {
		t.Errorf("created = %+v, want Groq/groq", created)
	}
	if created.WireFormat != "openai" || created.Verification != "verified-from-docs" {
		t.Errorf("created wire/verification = %q/%q, want openai/verified-from-docs", created.WireFormat, created.Verification)
	}
	if len(created.Models) != 2 || created.Models[0] != "mock-model-a" {
		t.Errorf("created models = %v, want [mock-model-a mock-model-b]", created.Models)
	}
	if !created.Enabled || created.HealthStatus != "unknown" {
		t.Errorf("created enabled/health = %v/%q, want true/unknown", created.Enabled, created.HealthStatus)
	}
	// No engine hook is wired on this test server, so the response
	// must say honestly that the provider does not route yet.
	if created.RoutingLive || created.RoutingNote == "" {
		t.Errorf("created routing_live/note = %v/%q, want false/non-empty (no hook wired)", created.RoutingLive, created.RoutingNote)
	}
	// The response must never carry the key or the base URL.
	if strings.Contains(rec.Body.String(), "sk-groq-test") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("created response leaks key or base URL: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "api_key") || strings.Contains(rec.Body.String(), `"base_url"`) {
		t.Errorf("created response carries a key/base-url field: %s", rec.Body.String())
	}
	// Live in the registry with no restart, and visible on the
	// (auth-exempt) provider feed.
	if got := reg.List(); len(got) != 1 {
		t.Fatalf("registry has %d providers, want 1", len(got))
	}
	feed := doReq(t, srv, "GET", "/api/providers", "", "")
	var list apiProvidersResponse
	if err := json.Unmarshal(feed.Body.Bytes(), &list); err != nil {
		t.Fatalf("provider feed is not JSON: %v", err)
	}
	if len(list.Providers) != 1 || list.Providers[0].Name != "Groq" {
		t.Errorf("provider feed = %+v, want 1 Groq provider", list.Providers)
	}
}

func TestAddProviderCustom(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, reg := newAddTestServer(t)

	rec := postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers custom: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if created.Name != "MockUpstream" || created.CatalogID != "" {
		t.Errorf("created = %+v, want MockUpstream with no catalog link", created)
	}
	if created.WireFormat != "openai" {
		t.Errorf("created wire = %q, want openai", created.WireFormat)
	}
	if strings.Contains(rec.Body.String(), "custom-key-1") {
		t.Errorf("created response leaks the key: %s", rec.Body.String())
	}
	if got := reg.List(); len(got) != 1 {
		t.Fatalf("registry has %d providers, want 1", len(got))
	}
}

// TestAddProviderModelsFieldAccepted proves CHECKLIST 283 (b): a
// fully-custom add that sends a `models` field no longer 400s on
// strict decoding — it is 201, the provider's models are the
// ones the /v1/models probe returned (never the supplied list),
// and the response says the supplied list was not stored.
func TestAddProviderModelsFieldAccepted(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)

	rec := postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream","models":["client-model-x","client-model-y"]}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers with models field: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if len(created.Models) != 2 || created.Models[0] != "mock-model-a" || created.Models[1] != "mock-model-b" {
		t.Errorf("created models = %v, want the probe's [mock-model-a mock-model-b], never the supplied list", created.Models)
	}
	if created.ModelsNote == "" || !strings.Contains(created.ModelsNote, "not stored") || !strings.Contains(created.ModelsNote, "/v1/models") {
		t.Errorf("models_note = %q, want it to say the supplied list was not stored and models come from the /v1/models probe", created.ModelsNote)
	}
}

// TestAddProviderModelsFieldEmptyEntry400 proves the cheap
// validation on the accepted field: an empty-string entry is a
// client bug and 400s instead of being silently dropped.
func TestAddProviderModelsFieldEmptyEntry400(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)

	rec := postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream","models":["client-model-x",""]}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/providers with an empty models entry: status %d, body %s (want 400)", rec.Code, rec.Body.String())
	}
}

func TestAddProviderHookRoutesLive(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil)
	var hooked *provider.Provider
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg),
		WithProviderAddedHook(func(_ context.Context, p *provider.Provider) error {
			hooked = p
			return nil
		}))

	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"`+mock.URL+`"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if !created.RoutingLive || created.RoutingNote != "" {
		t.Errorf("created routing_live/note = %v/%q, want true/empty", created.RoutingLive, created.RoutingNote)
	}
	if hooked == nil || hooked.ID != created.ID {
		t.Errorf("hook got provider %+v, want the created provider %s", hooked, created.ID)
	}
}

func TestAddProviderHookFailureStill201(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil)
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg),
		WithProviderAddedHook(func(_ context.Context, _ *provider.Provider) error {
			return errors.New("engine unavailable")
		}))

	// The provider is stored even though the engine hook failed —
	// failing the request would lie (a retry would only hit a
	// duplicate), so the answer is 201 with routing_live=false.
	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"`+mock.URL+`"}`, "test-key-123")
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if created.RoutingLive || created.RoutingNote == "" {
		t.Errorf("created routing_live/note = %v/%q, want false/non-empty", created.RoutingLive, created.RoutingNote)
	}
	if got := reg.List(); len(got) != 1 {
		t.Fatalf("registry has %d providers, want 1 (hook failure must not lose the add)", len(got))
	}
}

func TestAddProviderRequiresAuth(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)

	body := `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"` + mock.URL + `"}`
	if rec := postAdd(t, srv, body, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no gateway key: status %d, want 401", rec.Code)
	}
	if rec := postAdd(t, srv, body, "wrong-key"); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong gateway key: status %d, want 401", rec.Code)
	}
}

func TestAddProviderRelaxedAuthStillRequiresKey(t *testing.T) {
	// Tier 2 must never honour authRelaxed: with auth relaxed, a
	// Tier 3 route opens up but POST /api/providers still demands
	// a key.
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	reg := provider.NewRegistry(nil)
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg), WithAuthRelaxed(true))

	body := `{"catalog_id":"groq","api_key":"sk-groq-test","base_url":"` + mock.URL + `"}`
	if rec := postAdd(t, srv, body, ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("relaxed auth, no key: status %d, want 401 (Tier 2)", rec.Code)
	}
	if rec := postAdd(t, srv, body, "test-key-123"); rec.Code != http.StatusCreated {
		t.Errorf("relaxed auth, valid key: status %d, want 201", rec.Code)
	}
}

func TestAddProviderUnknownCatalog(t *testing.T) {
	srv, _ := newAddTestServer(t)
	rec := postAdd(t, srv, `{"catalog_id":"no-such-provider","api_key":"k"}`, "test-key-123")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown catalog id: status %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown catalog provider") {
		t.Errorf("unknown catalog id: body %s does not name the cause", rec.Body.String())
	}
}

func TestAddProviderToSGreyRefused(t *testing.T) {
	srv, reg := newAddTestServer(t)
	rec := postAdd(t, srv, `{"catalog_id":"chatgpt-web","api_key":"cookie-material"}`, "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Errorf("ToS-grey add: status %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ToS-grey") {
		t.Errorf("ToS-grey add: body %s does not name the refusal", rec.Body.String())
	}
	if got := reg.List(); len(got) != 0 {
		t.Errorf("ToS-grey provider was stored (%d providers), want 0", len(got))
	}
}

func TestAddProviderBadKey(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{badKey: true}.handler())
	defer mock.Close()
	srv, reg := newAddTestServer(t)

	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"sk-wrong-key","base_url":"`+mock.URL+`"}`, "test-key-123")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad provider key: status %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider API key") {
		t.Errorf("bad provider key: body %s does not say the provider key was rejected", rec.Body.String())
	}
	if got := reg.List(); len(got) != 0 {
		t.Errorf("bad-key provider was stored (%d providers), want 0", len(got))
	}
}

func TestAddProviderUnreachable(t *testing.T) {
	closed := httptest.NewServer(mockUpstream{}.handler())
	closed.Close() // nothing listens on this URL any more
	srv, reg := newAddTestServer(t)

	rec := postAdd(t, srv, `{"base_url":"`+closed.URL+`","api_key":"k","name":"Gone"}`, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream: status %d, want 502 (body %s)", rec.Code, rec.Body.String())
	}
	if got := reg.List(); len(got) != 0 {
		t.Errorf("unreachable provider was stored (%d providers), want 0", len(got))
	}
}

func TestAddProviderDuplicate(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)

	body := `{"base_url":"` + mock.URL + `","api_key":"k1","name":"First"}`
	if rec := postAdd(t, srv, body, "test-key-123"); rec.Code != http.StatusCreated {
		t.Fatalf("first add: status %d, want 201", rec.Code)
	}
	rec := postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"k2","name":"Second"}`, "test-key-123")
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate base URL: status %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAddProviderValidation(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()
	srv, _ := newAddTestServer(t)

	cases := []struct {
		name string
		body string
	}{
		{"empty body", `{}`},
		{"custom without key", `{"base_url":"` + mock.URL + `"}`},
		{"custom without base url", `{"api_key":"k"}`},
		{"name with catalog id", `{"catalog_id":"groq","api_key":"k","name":"Nope"}`},
		{"unknown field", `{"catalog_id":"groq","api_key":"k","surprise":true}`},
		{"malformed json", `{"catalog_id":`},
		{"bad base url scheme", `{"base_url":"ftp://example.com/v1","api_key":"k"}`},
	}
	for _, tc := range cases {
		if rec := postAdd(t, srv, tc.body, "test-key-123"); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (body %s)", tc.name, rec.Code, rec.Body.String())
		}
	}
}

// TestAddProviderFormKeyHint pins the provider-key hint on the served
// dashboard to the real behaviour: a custom add with an empty key is
// refused (400, see TestAddProviderValidation "custom without key"), so
// the no-key path exists only for no-auth catalog entries such as
// Ollama, picked via the Catalog picker. The old wording ("Local
// no-auth providers (e.g. Ollama) take no key.") read as if leaving
// the key empty worked in Custom mode too, which it never did.
func TestAddProviderFormKeyHint(t *testing.T) {
	srv, _ := newAddTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard: status %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "No-key local providers (e.g. Ollama) work via the Catalog picker") {
		t.Error("dashboard provider-key hint does not say no-key locals work via the Catalog picker")
	}
	if !strings.Contains(body, "Custom mode always needs a key") {
		t.Error("dashboard provider-key hint does not say Custom mode always needs a key")
	}
	if strings.Contains(body, "take no key") {
		t.Error("dashboard still carries the old misleading hint \"take no key\"")
	}
}

func TestAddProviderNoRegistry(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}) // no WithProviderRegistry
	rec := postAdd(t, srv, `{"catalog_id":"groq","api_key":"k"}`, "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no registry wired: status %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestCatalogFeed(t *testing.T) {
	srv, _ := newAddTestServer(t)
	// No gateway key: the catalog feed is auth-exempt, like the
	// other dashboard feeds (catalog facts only, no secrets).
	rec := doReq(t, srv, "GET", "/api/catalog", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/catalog: status %d, want 200", rec.Code)
	}
	var feed catalogResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("catalog feed is not JSON: %v", err)
	}
	if feed.Total != catalog.Count() || len(feed.Providers) != catalog.Count() {
		t.Errorf("catalog feed total = %d/%d, want %d", feed.Total, len(feed.Providers), catalog.Count())
	}
	byID := map[string]catalogEntryView{}
	for _, e := range feed.Providers {
		byID[e.ID] = e
	}
	groq, ok := byID["groq"]
	if !ok {
		t.Fatal("catalog feed has no groq entry")
	}
	if groq.Name != "Groq" || groq.Verification != "verified-from-docs" || !groq.HasBaseURL || groq.ToSGrey {
		t.Errorf("groq entry = %+v, want Groq/verified-from-docs/has-base-url/not-grey", groq)
	}
	web, ok := byID["chatgpt-web"]
	if !ok {
		t.Fatal("catalog feed has no chatgpt-web entry")
	}
	if !web.ToSGrey {
		t.Errorf("chatgpt-web entry = %+v, want tos_grey=true", web)
	}
	// No base URLs and no key material anywhere in the feed.
	body := rec.Body.String()
	if strings.Contains(body, "https://") || strings.Contains(body, "http://") {
		t.Errorf("catalog feed contains a URL: %s", body[:min(400, len(body))])
	}
	if strings.Contains(body, `"base_url"`) || strings.Contains(body, "api_key") {
		t.Errorf("catalog feed contains a base-url/key field: %s", body[:min(400, len(body))])
	}
}

// TestAddProviderRegistersTierModels proves the live-add wiring:
// when POST /api/providers succeeds AND the engine hook reports
// the provider routes live, its models join every auto-router
// tier (so tier ordering can pick them with no restart); when the
// hook fails (routing_live=false) nothing is registered, so
// auto/* can never resolve to a model with no client.
func TestAddProviderRegistersTierModels(t *testing.T) {
	mock := httptest.NewServer(mockUpstream{}.handler())
	defer mock.Close()

	newRouter := func() *autorouter.Router {
		return autorouter.New(autorouter.Config{
			DefaultModel: "strong-model",
			TierModels: map[autorouter.Tier][]string{
				autorouter.TierQuickQA: {"qa-model"},
			},
		})
	}
	addCustom := func(srv *Server) *httptest.ResponseRecorder {
		return postAdd(t, srv, `{"base_url":"`+mock.URL+`","api_key":"custom-key-1","name":"MockUpstream"}`, "test-key-123")
	}
	contains := func(models []string, want string) bool {
		for _, m := range models {
			if m == want {
				return true
			}
		}
		return false
	}

	// Hook succeeds: routing_live=true, models registered.
	router := newRouter()
	reg := provider.NewRegistry(nil)
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg), WithAutoRouter(router),
		WithProviderAddedHook(func(_ context.Context, _ *provider.Provider) error { return nil }))
	rec := addCustom(srv)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers: status %d, body %s (want 201)", rec.Code, rec.Body.String())
	}
	var created addedProviderResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if !created.RoutingLive {
		t.Fatalf("created routing_live = false, want true (hook succeeded)")
	}
	// The mock upstream lists mock-model-a and mock-model-b.
	for _, tier := range autorouter.AllTiers {
		models := router.ModelsForTier(tier)
		if !contains(models, "mock-model-a") || !contains(models, "mock-model-b") {
			t.Errorf("tier %s models = %v, want mock-model-a and mock-model-b registered", tier, models)
		}
	}
	if got := router.ModelsForTier(autorouter.TierQuickQA); len(got) != 3 || got[0] != "qa-model" {
		t.Errorf("quick-qa models = %v, want [qa-model mock-model-a mock-model-b]", got)
	}

	// Hook fails: routing_live=false, nothing registered.
	router2 := newRouter()
	reg2 := provider.NewRegistry(nil)
	srv2 := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg2), WithAutoRouter(router2),
		WithProviderAddedHook(func(_ context.Context, _ *provider.Provider) error { return errors.New("engine unavailable") }))
	rec2 := addCustom(srv2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("POST /api/providers (hook failure): status %d, body %s (want 201)", rec2.Code, rec2.Body.String())
	}
	var created2 addedProviderResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &created2); err != nil {
		t.Fatalf("created provider is not JSON: %v", err)
	}
	if created2.RoutingLive {
		t.Fatalf("created routing_live = true, want false (hook failed)")
	}
	for _, tier := range autorouter.AllTiers {
		if models := router2.ModelsForTier(tier); contains(models, "mock-model-a") || contains(models, "mock-model-b") {
			t.Errorf("tier %s models = %v, want no live models registered when routing_live=false", tier, models)
		}
	}
}
