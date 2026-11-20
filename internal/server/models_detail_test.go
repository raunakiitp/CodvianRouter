// Models surface (M8.10, Area 14 §14a): GET /v1/models/{model},
// GET /v1/models/query, GET /v1/models/health — one apitest
// fixture contract test per endpoint, plus an unknown-model 404,
// an unpriced-model case, and the free-tier / capability edges.
//
// The handlers are NOT registered on the production mux yet —
// server.go / guard.go belong to the coordinator, who wires the
// registration (see the proposed entries at the bottom of this
// file). Tests therefore serve the handlers on a local mux with
// the exact patterns the coordinator will register; Go's mux
// gives the literal /v1/models/query and /v1/models/health
// patterns precedence over the /v1/models/{model} wildcard, and
// TestM810LiteralRoutesBeatDetailWildcard proves it.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m810Mux serves the three M8.10 handlers on the patterns the
// coordinator will register in server.go. Auth/scope is NOT
// exercised here: the guard enforces TierManagement + scope
// "models" at registration (as for GET /v1/models), and the
// table-driven scope test covers guard-table routes once the
// entries land.
func m810Mux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models/query", srv.handleModelsQuery)
	mux.HandleFunc("GET /v1/models/health", srv.handleModelsHealth)
	mux.HandleFunc("GET /v1/models/{model}", srv.handleModelDetail)
	return mux
}

// m810Do serves one request against the M8.10 mux and returns the
// recorder, so tests can decode the body as well as check status.
func m810Do(t *testing.T, mux *http.ServeMux, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// m810HealthChecker is the wired health lister for the health
// fixture: prov-a probed up (42 ms) with 4 recorded ring samples,
// prov-b never probed.
func m810HealthChecker() fakeHealthStatusesWithHistory {
	checkedAt := time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC)
	up1h, up24h := 100.0, 90.0
	p50, p95 := int64(120), int64(300)
	return fakeHealthStatusesWithHistory{
		fakeHealthStatuses: fakeHealthStatuses{statuses: []health.Status{
			{ProviderID: "prov-a", ProviderName: "Provider A", OK: true, LatencyMs: 42, CheckedAt: checkedAt},
		}},
		histories: map[string]health.HistorySummary{
			"prov-a": {ProviderID: "prov-a", Samples: 4, Uptime1h: &up1h, Uptime24h: &up24h, P50Ms: &p50, P95Ms: &p95},
		},
	}
}

func TestM810ModelDetailFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "model-detail-200")
	apitest.Run(t, m810Mux(srv), f)
}

func TestM810ModelDetailUnknown404Fixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "model-detail-404-unknown-model")
	apitest.Run(t, m810Mux(srv), f)
	// The 404 names the missing model id.
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/no-such-model")
	if !strings.Contains(rec.Body.String(), "no-such-model") {
		t.Errorf("404 body = %s, want it to name the missing model", rec.Body.String())
	}
}

func TestM810ModelDetailUnpricedFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "model-detail-200-unpriced")
	apitest.Run(t, m810Mux(srv), f)
	// Unknown price means null numerics, never $0: decode and
	// check the pointers are nil.
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/unknown-price-model")
	var got modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Price.InputUSDPer1M != nil || got.Price.OutputUSDPer1M != nil || got.Price.BlendedUSDPer1M != nil {
		t.Errorf("unpriced model price = %+v, want all numerics null", got.Price)
	}
}

func TestM810ModelDetailFreePairPrice(t *testing.T) {
	// A configured $0 pair price is genuinely free (Known true,
	// blended 0) — and must not leak onto the unpriced model.
	zero := 0.0
	book := &config.PriceBook{Pairs: map[config.PriceKey]config.ModelPrice{
		{Provider: "prov-b", Model: "b-model"}: {InputUSDPer1M: &zero, OutputUSDPer1M: &zero},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)), WithPriceBook(book))
	mux := m810Mux(srv)
	rec := m810Do(t, mux, http.MethodGet, "/v1/models/b-model")
	var got modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Price.Known || got.Price.Source != "pair" || got.Price.BlendedUSDPer1M == nil || *got.Price.BlendedUSDPer1M != 0 {
		t.Errorf("b-model price = %+v, want known pair price with blended 0", got.Price)
	}
	rec = m810Do(t, mux, http.MethodGet, "/v1/models/unknown-price-model")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Price.Known {
		t.Errorf("unknown-price-model price = %+v, want unknown (the $0 pair price must not leak)", got.Price)
	}
}

func TestM810ModelDetailBaseURLAbsent(t *testing.T) {
	// A provider stored with no base URL reports
	// base_url_configured false (presence only — the URL itself
	// is never returned).
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	cfgs := []provider.ProviderConfig{{
		ID: "prov-c", Name: "Provider C", BaseURL: "",
		Models:  []provider.Model{{ID: "c-model"}},
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
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg))
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/c-model")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got modelDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.BaseURLConfigured == nil || *got.BaseURLConfigured {
		t.Errorf("base_url_configured = %v, want false (pointer to false)", got.BaseURLConfigured)
	}
	if strings.Contains(rec.Body.String(), "base_url\":") {
		t.Errorf("body = %s, must never carry a base URL value", rec.Body.String())
	}
}

func TestM810ModelsQueryFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "models-query-200")
	apitest.Run(t, m810Mux(srv), f)
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/query?provider=prov-b")
	var out struct {
		Data []modelDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "b-model" {
		t.Errorf("provider=prov-b query = %v, want exactly [b-model]", out.Data)
	}
}

func TestM810ModelsQueryCapability400Fixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "models-query-400-capability")
	apitest.Run(t, m810Mux(srv), f)
	// The 400 names the gap: capability, and that the catalog
	// records no capability flags.
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/query?capability=vision")
	body := rec.Body.String()
	if !strings.Contains(body, "capability") || !strings.Contains(body, "no per-model capability") {
		t.Errorf("400 body = %s, want it to name the capability gap", body)
	}
}

func TestM810ModelsQueryFreeTierOnly(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	f := apitest.LoadFixture(t, "m8.10", "models-query-200-free-tier-only")
	apitest.Run(t, m810Mux(srv), f)
	// No price book, no free-category provider: nothing is
	// known-free, so the list is empty — unknown is not $0.
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/query?free-tier-only=true")
	var out struct {
		Data []modelDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("free-tier-only with no prices = %v, want []", out.Data)
	}
	// With a $0 pair price for b-model, exactly that model is
	// free-tier — the priced models stay out.
	zero := 0.0
	book := &config.PriceBook{Pairs: map[config.PriceKey]config.ModelPrice{
		{Provider: "prov-b", Model: "b-model"}: {InputUSDPer1M: &zero, OutputUSDPer1M: &zero},
	}}
	srv2 := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)), WithPriceBook(book))
	rec = m810Do(t, m810Mux(srv2), http.MethodGet, "/v1/models/query?free-tier-only=true")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "b-model" {
		t.Errorf("free-tier-only with $0 b-model = %v, want [b-model]", out.Data)
	}
	// A malformed flag answers 400 naming the parameter.
	rec = m810Do(t, m810Mux(srv2), http.MethodGet, "/v1/models/query?free-tier-only=maybe")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "free-tier-only") {
		t.Errorf("free-tier-only=maybe: status = %d body = %s, want 400 naming free-tier-only", rec.Code, rec.Body.String())
	}
	// An unknown provider matches nothing, exactly as on
	// GET /v1/models — an empty list, not an error.
	rec = m810Do(t, m810Mux(srv2), http.MethodGet, "/v1/models/query?provider=does-not-exist")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 0 {
		t.Errorf("provider=does-not-exist query = %v, want []", out.Data)
	}
}

func TestM810ModelsHealthFixture(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)), WithHealthChecker(m810HealthChecker()))
	f := apitest.LoadFixture(t, "m8.10", "models-health-200")
	apitest.Run(t, m810Mux(srv), f)
	// The unprobed row's probe fields are null, never invented:
	// decode and check them directly.
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/health")
	var out struct {
		Data       []modelHealthRow `json:"data"`
		ProbeScope string           `json:"probe_scope"`
		Note       string           `json:"note"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ProbeScope != "provider" || !strings.Contains(out.Note, "provider-level") {
		t.Errorf("probe_scope = %q note = %q, want the provider-level probe limitation stated", out.ProbeScope, out.Note)
	}
	if len(out.Data) != 5 {
		t.Fatalf("health rows = %d, want 5", len(out.Data))
	}
	if out.Data[0].LatencyMs == nil || *out.Data[0].LatencyMs != 42 || out.Data[0].Uptime24h == nil || *out.Data[0].Uptime24h != 90 {
		t.Errorf("cheap-model health = %+v, want latency 42 and uptime_24h 90", out.Data[0])
	}
	b := out.Data[4]
	if b.ID != "b-model" || b.Status != "unknown" || b.LastProbeAt != nil || b.LatencyMs != nil || b.Uptime1h != nil || b.Uptime24h != nil || b.Samples != 0 {
		t.Errorf("b-model health = %+v, want unknown with null probe fields and 0 samples", b)
	}
}

func TestM810ModelsHealthNoChecker(t *testing.T) {
	// No health checker wired: every model is unknown, with
	// null probe fields — the endpoint still works, it never
	// invents a status.
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []modelHealthRow `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 5 {
		t.Fatalf("health rows = %d, want 5", len(out.Data))
	}
	for _, row := range out.Data {
		if row.Status != "unknown" || row.LastProbeAt != nil || row.LatencyMs != nil || row.Uptime1h != nil {
			t.Errorf("row %+v: want unknown with null probe fields", row)
		}
	}
}

func TestM810ModelsHealthProviderFilter(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)), WithHealthChecker(m810HealthChecker()))
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/health?provider=prov-b")
	var out struct {
		Data []modelHealthRow `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "b-model" || out.Data[0].Status != "unknown" {
		t.Errorf("provider=prov-b health = %+v, want [b-model unknown]", out.Data)
	}
}

func TestM810LiteralRoutesBeatDetailWildcard(t *testing.T) {
	// /v1/models/query and /v1/models/health must resolve to
	// their own handlers, not to a detail lookup for a model
	// literally named "query" or "health".
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	mux := m810Mux(srv)
	if rec := m810Do(t, mux, http.MethodGet, "/v1/models/query"); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/models/query: status = %d, body = %s, want 200 (not a 404 detail lookup)", rec.Code, rec.Body.String())
	}
	if rec := m810Do(t, mux, http.MethodGet, "/v1/models/health"); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/models/health: status = %d, body = %s, want 200 (not a 404 detail lookup)", rec.Code, rec.Body.String())
	}
}

func TestM810DetailMethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	rec := m810Do(t, m810Mux(srv), http.MethodPost, "/v1/models/cheap-model")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST detail: status = %d, want 405", rec.Code)
	}
}

func TestM810ToggleCheckResponseWrongStatusFails(t *testing.T) {
	// Toggle proof for the fixture assertions (API_FIXTURES.md
	// checklist item 4): the same 200 response must FAIL when
	// the fixture expects a different status, and must fail when
	// a JSON path is wrong — so the positive fixture tests above
	// fail if a handler regresses to the wrong status or shape.
	f := apitest.LoadFixture(t, "m8.10", "model-detail-200")
	f.Assert.Status = 201 // wrong on purpose: the response is 200
	resp := &http.Response{StatusCode: 200, Header: http.Header{}}
	// The recorded body is nominal (no upstream); check against
	// a real detail payload instead: build one from the handler.
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	rec := m810Do(t, m810Mux(srv), http.MethodGet, "/v1/models/cheap-model")
	if err := apitest.CheckResponse(resp, rec.Body.Bytes(), f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against assert.status 201")
	} else if !strings.Contains(err.Error(), "status") {
		t.Fatalf("wrong-status error should name status, got: %v", err)
	}
	f2 := apitest.LoadFixture(t, "m8.10", "model-detail-200")
	f2.Assert.JSONPaths["price.known"] = false // wrong on purpose: it is true
	if err := apitest.CheckResponse(resp, rec.Body.Bytes(), f2); err == nil {
		t.Fatal("CheckResponse accepted price.known=false against a true price")
	}
}

// Proposed registration for the coordinator (NOT wired here —
// server.go and guard.go are coordinator-owned):
//
//	// server.go register():
//	s.register(mux, "GET", "/v1/models/query", s.handleModelsQuery)
//	s.register(mux, "GET", "/v1/models/health", s.handleModelsHealth)
//	s.register(mux, "GET", "/v1/models/{model}", s.handleModelDetail)
//	// NOTE: register the two literal patterns before the {model}
//	// wildcard (Go's mux prefers literals over wildcards, but
//	// keep the order explicit), and confirm the table-driven
//	// scope test covers the new guard rows.
//
//	// guard.go guardRouteTable (TierManagement, gateway-key auth,
//	// scope "models" — the same scope as GET /v1/models):
//	{Method: "GET", Path: "/v1/models/{model}", Tier: TierManagement, Description: "Single model detail — catalog entry for one model (provider, base-URL presence, price when known, else unknown); unknown models answer 404. Gateway-key auth required.", Scopes: []string{"models"}},
//	{Method: "GET", Path: "/v1/models/query", Tier: TierManagement, Description: "Model catalog query — filter by provider and free-tier-only; a capability filter answers 400 (the catalog records no per-model capability flags). Gateway-key auth required.", Scopes: []string{"models"}},
//	{Method: "GET", Path: "/v1/models/health", Tier: TierManagement, Description: "Per-model health from the health-probe ring — each model's status is its provider's last probe result (probes are provider-level key tests); unprobed models report unknown. Gateway-key auth required.", Scopes: []string{"models"}},
