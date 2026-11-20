// Catalog filters, shortlist and live refresh (CHECKLIST 200):
// GET /v1/models and GET /dashboard/models filter by provider /
// category / max_price / min_context / shortlist; the operator's
// shortlist pins first, marked shortlisted; POST
// /dashboard/models/refresh re-probes provider model lists live
// and reports per-provider success or failure.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// modelsTestRegistry builds a real *provider.Registry from a JSON
// store file (the TestModelsUsesProviderRegistry pattern) with two
// providers: prov-a (catalog groq → category api-key) and prov-b
// (hand-added → category custom). Prices are USD per 1M tokens,
// the row unit (see TestDashboardModelsPairPrices): cheap-model
// blends to 1.0, b-model to 3.0, pricey-model to 15.0,
// unknown-price-model has no price, no-window-model has no window.
func modelsTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	cfgs := []provider.ProviderConfig{
		{
			ID: "prov-a", Name: "Provider A", BaseURL: "https://example.com/a", CatalogID: "groq",
			Models: []provider.Model{
				{ID: "cheap-model", ContextWindow: 128000, InputPriceUSD: 0.5, OutputPriceUSD: 1.5},
				{ID: "pricey-model", ContextWindow: 32000, InputPriceUSD: 10, OutputPriceUSD: 20},
				{ID: "unknown-price-model", ContextWindow: 64000},
				{ID: "no-window-model", InputPriceUSD: 0.5, OutputPriceUSD: 0.5},
			},
			Enabled: true,
		},
		{
			ID: "prov-b", Name: "Provider B", BaseURL: "https://example.com/b",
			Models: []provider.Model{
				{ID: "cheap-model", ContextWindow: 128000, InputPriceUSD: 0.5, OutputPriceUSD: 1.5},
				{ID: "b-model", ContextWindow: 200000, InputPriceUSD: 2, OutputPriceUSD: 4},
			},
			Enabled: true,
		},
	}
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
	return reg
}

func getV1Models(t *testing.T, srv *Server, query string) []Model {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/v1/models"+query, "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models%s: status = %d, body = %s", query, rec.Code, rec.Body.String())
	}
	var out struct {
		Data []Model `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /v1/models: %v (body %s)", err, rec.Body.String())
	}
	return out.Data
}

func modelIDs(models []Model) []string {
	ids := make([]string, len(models))
	for i, m := range models {
		ids[i] = m.ID
	}
	return ids
}

func TestModelsFilterByProvider(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	// /v1/models lists each model ID once, under the first provider
	// offering it in registry order: cheap-model belongs to prov-a.
	got := modelIDs(getV1Models(t, srv, "?provider=prov-a"))
	if len(got) != 4 || got[0] != "cheap-model" || got[1] != "pricey-model" || got[2] != "unknown-price-model" || got[3] != "no-window-model" {
		t.Errorf("provider=prov-a: %v, want prov-a's 4 models in registry order", got)
	}
	// Name matches too (case-insensitive), and prov-b lists only
	// the model prov-a does not already claim.
	got = modelIDs(getV1Models(t, srv, "?provider=Provider%20A"))
	if len(got) != 4 {
		t.Errorf("provider=Provider A (name): %v, want prov-a's 4 models", got)
	}
	got = modelIDs(getV1Models(t, srv, "?provider=prov-b"))
	if len(got) != 1 || got[0] != "b-model" {
		t.Errorf("provider=prov-b: %v, want [b-model] (cheap-model is listed under prov-a)", got)
	}
	got = modelIDs(getV1Models(t, srv, "?provider=does-not-exist"))
	if len(got) != 0 {
		t.Errorf("provider=does-not-exist: %v, want []", got)
	}
}

func TestModelsFilterByCategory(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	got := modelIDs(getV1Models(t, srv, "?category=api-key"))
	if len(got) != 4 {
		t.Errorf("category=api-key: %v, want prov-a's 4 models (groq is api-key)", got)
	}
	got = modelIDs(getV1Models(t, srv, "?category=custom"))
	if len(got) != 1 || got[0] != "b-model" {
		t.Errorf("category=custom: %v, want [b-model] (hand-added provider)", got)
	}
	got = modelIDs(getV1Models(t, srv, "?category=API-KEY"))
	if len(got) != 4 {
		t.Errorf("category=API-KEY (case-insensitive): %v, want prov-a's 4 models", got)
	}
}

func TestModelsFilterByMaxPrice(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	// Blends: cheap 1.0, no-window 0.5, b-model 3.0, pricey 15.0,
	// unknown-price has no blend at all.
	got := modelIDs(getV1Models(t, srv, "?max_price=2"))
	if len(got) != 2 || got[0] != "cheap-model" || got[1] != "no-window-model" {
		t.Errorf("max_price=2: %v, want [cheap-model no-window-model]", got)
	}
	got = modelIDs(getV1Models(t, srv, "?max_price=1"))
	if len(got) != 2 || got[0] != "cheap-model" || got[1] != "no-window-model" {
		t.Errorf("max_price=1 (boundary, inclusive): %v, want [cheap-model no-window-model]", got)
	}
	got = modelIDs(getV1Models(t, srv, "?max_price=0.1"))
	if len(got) != 0 {
		t.Errorf("max_price=0.1: %v, want [] (nothing is that cheap, and unknown is never assumed cheap)", got)
	}
	// A malformed filter answers 400 naming the parameter, never
	// a silently unfiltered list.
	rec := doReq(t, srv, http.MethodGet, "/v1/models?max_price=abc", "", "test-key-123")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "max_price") {
		t.Errorf("max_price=abc: status = %d body = %s, want 400 naming max_price", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/v1/models?max_price=-1", "", "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("max_price=-1: status = %d, want 400", rec.Code)
	}
}

func TestModelsFilterByMinContext(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)))
	got := modelIDs(getV1Models(t, srv, "?min_context=100000"))
	if len(got) != 2 || got[0] != "cheap-model" || got[1] != "b-model" {
		t.Errorf("min_context=100000: %v, want [cheap-model b-model]", got)
	}
	got = modelIDs(getV1Models(t, srv, "?min_context=128000"))
	if len(got) != 2 {
		t.Errorf("min_context=128000 (boundary, inclusive): %v, want [cheap-model b-model]", got)
	}
	// Unknown window is excluded, never assumed large: with no
	// other filter the list has 5 rows, so min_context=1 must
	// drop exactly the one windowless row.
	got = modelIDs(getV1Models(t, srv, "?min_context=1"))
	if len(got) != 4 {
		t.Errorf("min_context=1: %v, want 4 rows (no-window-model excluded)", got)
	}
	rec := doReq(t, srv, http.MethodGet, "/v1/models?min_context=abc", "", "test-key-123")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "min_context") {
		t.Errorf("min_context=abc: status = %d body = %s, want 400 naming min_context", rec.Code, rec.Body.String())
	}
}

func TestModelsShortlistOrderingAndMarking(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)),
		WithShortlist([]string{"pricey-model", "b-model"}))
	models := getV1Models(t, srv, "")
	if len(models) != 5 {
		t.Fatalf("models = %d, want 5", len(models))
	}
	// Pinned rows sort first, in shortlist order, marked
	// shortlisted; the rest keep registry order.
	if models[0].ID != "pricey-model" || !models[0].Shortlisted {
		t.Errorf("models[0] = %v (shortlisted=%v), want pricey-model pinned first", models[0].ID, models[0].Shortlisted)
	}
	if models[1].ID != "b-model" || !models[1].Shortlisted {
		t.Errorf("models[1] = %v (shortlisted=%v), want b-model pinned second", models[1].ID, models[1].Shortlisted)
	}
	for _, m := range models[2:] {
		if m.Shortlisted {
			t.Errorf("model %q is shortlisted, want only the two pins marked", m.ID)
		}
	}
	if got := modelIDs(models[2:]); len(got) != 3 || got[0] != "cheap-model" || got[1] != "unknown-price-model" || got[2] != "no-window-model" {
		t.Errorf("unpinned tail = %v, want registry order [cheap-model unknown-price-model no-window-model]", got)
	}
	// shortlist=true lists only the pins.
	got := modelIDs(getV1Models(t, srv, "?shortlist=true"))
	if len(got) != 2 || got[0] != "pricey-model" || got[1] != "b-model" {
		t.Errorf("shortlist=true: %v, want [pricey-model b-model]", got)
	}
}

func TestModelsShortlistPairEntry(t *testing.T) {
	// A "providerID/modelID" entry pins only that pair. On
	// /v1/models cheap-model is listed under prov-a (dedupe), so
	// "prov-b/cheap-model" does NOT pin it there — but the plain
	// model entry "cheap-model" pins it wherever it is listed.
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)),
		WithShortlist([]string{"prov-b/cheap-model"}))
	models := getV1Models(t, srv, "")
	if models[0].ID != "cheap-model" || models[0].Shortlisted {
		t.Errorf("models[0] = %v (shortlisted=%v), want cheap-model first by registry order, NOT pinned (its listed provider is prov-a)", models[0].ID, models[0].Shortlisted)
	}
	if got := modelIDs(getV1Models(t, srv, "?shortlist=true")); len(got) != 0 {
		t.Errorf("shortlist=true with only a prov-b pin: %v, want [] on /v1/models", got)
	}
	srv2 := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)),
		WithShortlist([]string{"cheap-model"}))
	models2 := getV1Models(t, srv2, "")
	if models2[0].ID != "cheap-model" || !models2[0].Shortlisted {
		t.Errorf("plain entry: models[0] = %v (shortlisted=%v), want cheap-model pinned", models2[0].ID, models2[0].Shortlisted)
	}
}

func TestDashboardModelsFiltersAndShortlist(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsTestRegistry(t)),
		WithShortlist([]string{"prov-b/cheap-model"}))
	get := func(query string) dashboardModelsResponse {
		var resp dashboardModelsResponse
		rec := doReq(t, srv, http.MethodGet, "/dashboard/models"+query, "", "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /dashboard/models%s: status = %d, body = %s", query, rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode dashboard models: %v", err)
		}
		return resp
	}
	// The dashboard feed is per-(provider, model): 6 rows.
	all := get("")
	if len(all.Models) != 6 {
		t.Fatalf("dashboard models = %d, want 6 (per-pair feed)", len(all.Models))
	}
	// The prov-b/cheap-model pair IS pinned here (unlike /v1),
	// and sorts first.
	if all.Models[0].ID != "cheap-model" || all.Models[0].ProviderID != "prov-b" || !all.Models[0].Shortlisted {
		t.Errorf("models[0] = %s/%s (shortlisted=%v), want prov-b/cheap-model pinned first", all.Models[0].ProviderID, all.Models[0].ID, all.Models[0].Shortlisted)
	}
	// The prov-a row of the same model is not pinned by the pair
	// entry.
	for _, m := range all.Models[1:] {
		if m.ID == "cheap-model" && m.ProviderID == "prov-a" && m.Shortlisted {
			t.Errorf("prov-a/cheap-model is shortlisted, want only the prov-b pair pinned")
		}
	}
	// Filters compose, server-side: provider + min_context.
	rows := get("?provider=prov-b&min_context=150000")
	if len(rows.Models) != 1 || rows.Models[0].ID != "b-model" {
		t.Errorf("provider=prov-b&min_context=150000: %d rows, want [b-model] (cheap-model's 128K window is below)", len(rows.Models))
	}
	rows = get("?category=api-key&max_price=2")
	if len(rows.Models) != 2 {
		t.Errorf("category=api-key&max_price=2: %d rows, want 2 (cheap-model + no-window-model)", len(rows.Models))
	}
	rows = get("?shortlist=true")
	if len(rows.Models) != 1 || rows.Models[0].ProviderID != "prov-b" {
		t.Errorf("shortlist=true: %d rows, want [prov-b/cheap-model]", len(rows.Models))
	}
	// Categories ride on the rows.
	if all.Models[0].Category != "api-key" && all.Models[0].Category != "custom" {
		t.Errorf("category missing on dashboard rows: %q", all.Models[0].Category)
	}
}

// fakeRefresher is a controllable modelRefresher for the refresh
// endpoint tests: it returns scripted per-provider results (and an
// optional persist-style error) without touching any upstream.
type fakeRefresher struct {
	results []provider.ModelRefreshResult
	err     error
	calls   int
}

func (f *fakeRefresher) RefreshModels(ctx context.Context) ([]provider.ModelRefreshResult, error) {
	f.calls++
	return f.results, f.err
}

func TestModelsRefreshEndpoint(t *testing.T) {
	ref := &fakeRefresher{results: []provider.ModelRefreshResult{
		{ProviderID: "prov-a", ProviderName: "Provider A", Status: provider.ModelRefreshOK, Models: []string{"cheap-model", "new-model"}},
		{ProviderID: "prov-b", ProviderName: "Provider B", Status: provider.ModelRefreshFailed, Models: []string{"b-model"}, Error: "upstream HTTP 500"},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithModelRefresher(ref))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/models/refresh", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ref.calls != 1 {
		t.Fatalf("refresher calls = %d, want 1", ref.calls)
	}
	var out modelsRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode refresh response: %v (body %s)", err, rec.Body.String())
	}
	if out.Total != 2 || out.OK != 1 || out.Failed != 1 || out.Skipped != 0 {
		t.Errorf("totals = %+v, want total 2 ok 1 failed 1 skipped 0", out)
	}
	if len(out.Providers) != 2 || out.Providers[0].Status != provider.ModelRefreshOK || out.Providers[1].Status != provider.ModelRefreshFailed {
		t.Errorf("providers = %+v, want [ok failed]", out.Providers)
	}
	if out.Providers[1].Error != "upstream HTTP 500" {
		t.Errorf("failed provider error = %q, want the honest reason", out.Providers[1].Error)
	}
	if out.RefreshedAt.IsZero() {
		t.Errorf("refreshed_at is zero")
	}
	// GET is not a refresh.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/models/refresh", "", "test-key-123")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET refresh: status = %d, want 405", rec.Code)
	}
}

func TestModelsRefreshEndpointUnavailable(t *testing.T) {
	// No refresher wired (no registry, no explicit refresher):
	// the endpoint answers 503, never a faked refresh.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodPost, "/dashboard/models/refresh", "", "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s, want 503", rec.Code, rec.Body.String())
	}
}

func TestModelsRefreshEndpointPersistError(t *testing.T) {
	ref := &fakeRefresher{
		results: []provider.ModelRefreshResult{
			{ProviderID: "prov-a", ProviderName: "Provider A", Status: provider.ModelRefreshOK, Models: []string{"cheap-model"}},
		},
		err: errors.New("fake persist failure"),
	}
	srv := newTestServer(t, &fakeEngine{}, WithModelRefresher(ref))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/models/refresh", "", "test-key-123")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 500 for a persist failure", rec.Code, rec.Body.String())
	}
	// The 500 still carries the per-provider results: the caller
	// sees what was probed, not just "error".
	if !strings.Contains(rec.Body.String(), "prov-a") || !strings.Contains(rec.Body.String(), "fake persist failure") {
		t.Errorf("body = %s, want the refresh results AND the persist error", rec.Body.String())
	}
}

// TestModelsRefreshLiveRegistry exercises the real wiring:
// WithProviderRegistry wires the registry as the refresher, so a
// POST re-probes the provider's live /v1/models and the catalog
// then lists the refreshed models. The provider is added through
// the real catalog-add path (so its key is sealed properly), then
// the mock's list changes: the refresh must pick up the new list
// and drop the model the upstream no longer lists.
func TestModelsRefreshLiveRegistry(t *testing.T) {
	var mu sync.Mutex
	live := []string{"stale-model", "cheap-model"}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		ids := append([]string(nil), live...)
		mu.Unlock()
		data := make([]map[string]string, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]string{"id": id})
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer mock.Close()
	reg, _ := newLiveRefreshRegistry(t)
	if _, err := reg.AddCatalogWithOptions(context.Background(), "groq", "sk-live-test",
		provider.CatalogAddOptions{BaseURL: mock.URL}); err != nil {
		t.Fatalf("AddCatalog groq: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(reg))
	if got := modelIDs(getV1Models(t, srv, "")); len(got) != 2 || got[0] != "stale-model" || got[1] != "cheap-model" {
		t.Fatalf("models after add = %v, want [stale-model cheap-model]", got)
	}
	mu.Lock()
	live = []string{"fresh-model", "cheap-model"}
	mu.Unlock()
	rec := doReq(t, srv, http.MethodPost, "/dashboard/models/refresh", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out modelsRefreshResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode refresh: %v", err)
	}
	if out.OK != 1 || len(out.Providers) != 1 || out.Providers[0].Status != provider.ModelRefreshOK {
		t.Fatalf("refresh = %+v, want 1 ok provider", out)
	}
	if len(out.Providers[0].Models) != 2 || out.Providers[0].Models[0] != "fresh-model" {
		t.Errorf("refreshed models = %v, want [fresh-model cheap-model] (live order)", out.Providers[0].Models)
	}
	// The catalog now lists the refreshed models; the stale one
	// is gone (the upstream no longer lists it).
	if got := modelIDs(getV1Models(t, srv, "")); len(got) != 2 || got[0] != "fresh-model" || got[1] != "cheap-model" {
		t.Errorf("models after refresh = %v, want [fresh-model cheap-model]", got)
	}
}

// newLiveRefreshRegistry builds a file-backed registry whose keys
// are sealed with a vault cipher (the catalog-add path needs a
// working cipher, and a JSON store without one cannot build
// clients: ClientFor fails decrypting the empty seal).
func newLiveRefreshRegistry(t *testing.T) (*provider.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-refresh!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := provider.NewFileRegistry(filepath.Join(dir, "providers.json"), provider.WithCipher(provider.NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return reg, dir
}

// modelsFetchedAtTestRegistry builds a *provider.Registry from a
// JSON store file with three providers whose stored model lists
// differ in fetch age (CT-7): prov-a fetched 1h ago (fresh),
// prov-b fetched 25h ago (stale — past the 24h interval),
// prov-c never fetched (zero stamp, always stale).
func modelsFetchedAtTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	now := time.Now().UTC()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	cfgs := []provider.ProviderConfig{
		{
			ID: "prov-a", Name: "Provider A", BaseURL: "https://example.com/a", CatalogID: "groq",
			Models:          []provider.Model{{ID: "fresh-model"}},
			ModelsFetchedAt: now.Add(-time.Hour),
			Enabled:         true,
		},
		{
			ID: "prov-b", Name: "Provider B", BaseURL: "https://example.com/b",
			Models:          []provider.Model{{ID: "stale-model"}},
			ModelsFetchedAt: now.Add(-25 * time.Hour),
			Enabled:         true,
		},
		{
			ID: "prov-c", Name: "Provider C", BaseURL: "https://example.com/c",
			Models:  []provider.Model{{ID: "never-model"}},
			Enabled: true,
		},
	}
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
	return reg
}

// TestV1ModelsFetchedAtAndStale (CT-7): GET /v1/models surfaces
// each row's list age — models_fetched_at carries the fetch time
// and models_stale is the badge when the list is older than the
// 24h refresh interval (or was never fetched). A never-fetched
// row omits the timestamp (zero) and is still marked stale: the
// age is unknown, never faked as fresh.
func TestV1ModelsFetchedAtAndStale(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(modelsFetchedAtTestRegistry(t)))
	models := getV1Models(t, srv, "")
	if len(models) != 3 {
		t.Fatalf("models = %v, want [fresh-model stale-model never-model]", modelIDs(models))
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}
	fresh := byID["fresh-model"]
	if fresh.ModelsFetchedAt == nil {
		t.Errorf("fresh-model: models_fetched_at is nil, want the stored fetch time")
	} else if age := time.Since(*fresh.ModelsFetchedAt); age < 30*time.Minute || age > 2*time.Hour {
		t.Errorf("fresh-model: models_fetched_at age = %v, want ~1h", age)
	}
	if fresh.ModelsStale {
		t.Errorf("fresh-model: models_stale = true, want false (fetched 1h ago)")
	}
	stale := byID["stale-model"]
	if stale.ModelsFetchedAt == nil {
		t.Errorf("stale-model: models_fetched_at is nil, want the stored fetch time")
	}
	if !stale.ModelsStale {
		t.Errorf("stale-model: models_stale = false, want true (fetched 25h ago, past the 24h interval)")
	}
	never := byID["never-model"]
	if never.ModelsFetchedAt != nil {
		t.Errorf("never-model: models_fetched_at = %v, want nil/omitted (never fetched)", never.ModelsFetchedAt)
	}
	if !never.ModelsStale {
		t.Errorf("never-model: models_stale = false, want true (never fetched is always stale)")
	}
	// The raw JSON carries the badge only where it applies: no
	// models_stale key on the fresh row, and no models_fetched_at
	// key on the never-fetched row.
	rec := doReq(t, srv, http.MethodGet, "/v1/models", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models: status = %d", rec.Code)
	}
	var raw struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw /v1/models: %v", err)
	}
	rawByID := map[string]map[string]any{}
	for _, row := range raw.Data {
		rawByID[row["id"].(string)] = row
	}
	if _, ok := rawByID["fresh-model"]["models_stale"]; ok {
		t.Errorf("fresh-model raw JSON carries models_stale, want it omitted when false")
	}
	if _, ok := rawByID["fresh-model"]["models_fetched_at"]; !ok {
		t.Errorf("fresh-model raw JSON lacks models_fetched_at, want the fetch timestamp")
	}
	if _, ok := rawByID["never-model"]["models_fetched_at"]; ok {
		t.Errorf("never-model raw JSON carries models_fetched_at, want it omitted when never fetched")
	}
	if _, ok := rawByID["stale-model"]["models_stale"]; !ok {
		t.Errorf("stale-model raw JSON lacks models_stale, want the badge")
	}
}
