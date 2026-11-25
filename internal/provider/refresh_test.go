package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// refreshMock is a mutable OpenAI-wire upstream for refresh tests:
// its /v1/models list and status can change between the add (which
// probes it) and the refresh, so the test can prove the refresh
// re-probes live instead of replaying the add-time list.
type refreshMock struct {
	mu     sync.Mutex
	models []string
	status int // 0 = 200
}

func newRefreshMock(t *testing.T, models ...string) (*httptest.Server, *refreshMock) {
	t.Helper()
	m := &refreshMock{models: models}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if m.status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(m.status)
			_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
			return
		}
		data := make([]map[string]string, 0, len(m.models))
		for _, id := range m.models {
			data = append(data, map[string]string{"id": id})
		}
		body, _ := json.Marshal(map[string]any{"data": data})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	return srv, m
}

func (m *refreshMock) set(models []string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.models = models
	m.status = status
}

// addRefreshProvider adds a catalog provider (groq, OpenAI wire)
// against a refreshMock and returns its registry ID.
func addRefreshProvider(t *testing.T, reg *Registry, srv *httptest.Server) string {
	t.Helper()
	p, err := reg.AddCatalogWithOptions(context.Background(), "groq", "sk-refresh-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog groq: %v", err)
	}
	return p.ID
}

func TestRegistryRefreshModelsSuccessMerges(t *testing.T) {
	srv, mock := newRefreshMock(t, "model-a", "model-b")
	defer srv.Close()
	reg, dir := newCatalogTestRegistry(t)
	id := addRefreshProvider(t, reg, srv)

	// Stored metadata the upstream list does not report: model-a
	// keeps its window, prices and deprecated flag after the
	// refresh; model-b is dropped when the upstream stops listing
	// it. (In-package test: write the stored records directly.)
	reg.mu.Lock()
	reg.providers[id].Models = []Model{
		{ID: "model-a", ContextWindow: 128000, InputPriceUSD: 1, OutputPriceUSD: 2, Deprecated: true},
		{ID: "model-b", ContextWindow: 32000},
	}
	reg.mu.Unlock()

	mock.set([]string{"model-a", "model-c"}, 0)
	results, err := reg.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if res.Status != ModelRefreshOK || res.Error != "" {
		t.Fatalf("result = %+v, want ok with no error", res)
	}
	if len(res.Models) != 2 || res.Models[0] != "model-a" || res.Models[1] != "model-c" {
		t.Fatalf("result models = %v, want [model-a model-c] (live order)", res.Models)
	}
	got, ok := reg.Get(id)
	if !ok {
		t.Fatal("provider gone after refresh")
	}
	if len(got.Models) != 2 {
		t.Fatalf("stored models = %+v, want 2", got.Models)
	}
	a := got.Models[0]
	if a.ID != "model-a" || a.ContextWindow != 128000 || a.InputPriceUSD != 1 || a.OutputPriceUSD != 2 || !a.Deprecated {
		t.Errorf("model-a after refresh = %+v, want stored metadata kept (window 128000, prices 1/2, deprecated)", a)
	}
	if c := got.Models[1]; c.ID != "model-c" || c.ContextWindow != 0 || c.InputPriceUSD != 0 || c.OutputPriceUSD != 0 || c.Deprecated {
		t.Errorf("model-c after refresh = %+v, want bare record (unknown window/prices, never guessed)", c)
	}
	// Persisted: a fresh registry on the same file sees the
	// refreshed list.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("reload vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)), WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got2, ok := reg2.Get(id)
	if !ok || len(got2.Models) != 2 || got2.Models[0].ID != "model-a" || got2.Models[1].ID != "model-c" {
		t.Fatalf("reloaded models = %+v, want the refreshed list", got2)
	}
}

func TestRegistryRefreshModelsFailureKeepsStoredList(t *testing.T) {
	srv, mock := newRefreshMock(t, "model-a", "model-b")
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)
	id := addRefreshProvider(t, reg, srv)

	mock.set(nil, http.StatusInternalServerError)
	results, err := reg.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: per-provider failures are results, not an error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	res := results[0]
	if res.Status != ModelRefreshFailed {
		t.Fatalf("result = %+v, want failed", res)
	}
	// The error is the HTTP status only — the upstream body
	// ("boom") must never be carried into the result.
	if res.Error != "upstream HTTP 500" {
		t.Errorf("error = %q, want %q", res.Error, "upstream HTTP 500")
	}
	if len(res.Models) != 2 || res.Models[0] != "model-a" || res.Models[1] != "model-b" {
		t.Errorf("result models = %v, want the stored list [model-a model-b]", res.Models)
	}
	got, _ := reg.Get(id)
	if len(got.Models) != 2 || got.Models[0].ID != "model-a" || got.Models[1].ID != "model-b" {
		t.Errorf("stored models after failed refresh = %+v, want unchanged", got.Models)
	}
}

func TestRegistryRefreshModelsSkipsDisabled(t *testing.T) {
	srv, mock := newRefreshMock(t, "model-a")
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)
	id := addRefreshProvider(t, reg, srv)
	if err := reg.SetEnabled(id, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	// The upstream changes while the provider is disabled — the
	// refresh must not probe it (a probe would see model-b).
	mock.set([]string{"model-b"}, 0)
	results, err := reg.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	res := results[0]
	if res.Status != ModelRefreshSkipped || res.Error != "provider disabled" {
		t.Fatalf("result = %+v, want skipped/provider disabled", res)
	}
	if len(res.Models) != 1 || res.Models[0] != "model-a" {
		t.Errorf("result models = %v, want the stored list [model-a]", res.Models)
	}
	got, _ := reg.Get(id)
	if len(got.Models) != 1 || got.Models[0].ID != "model-a" {
		t.Errorf("stored models after skipped refresh = %+v, want unchanged", got.Models)
	}
}

func TestRegistryRefreshModelsMixedProviders(t *testing.T) {
	srvOK, mockOK := newRefreshMock(t, "model-a")
	defer srvOK.Close()
	srvBad, mockBad := newRefreshMock(t, "model-x")
	defer srvBad.Close()
	reg, _ := newCatalogTestRegistry(t)
	// Two providers on different mocks: one refreshes, one fails.
	// (Catalog IDs differ so the two adds do not collide.)
	idOK := addRefreshProvider(t, reg, srvOK)
	pBad, err := reg.AddCatalogWithOptions(context.Background(), "groq", "sk-refresh-test-2",
		CatalogAddOptions{BaseURL: srvBad.URL})
	if err != nil {
		t.Fatalf("AddCatalog second groq: %v", err)
	}
	mockOK.set([]string{"model-a", "model-b"}, 0)
	mockBad.set(nil, http.StatusTooManyRequests)
	results, err := reg.RefreshModels(context.Background())
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2 (one per provider, registry order)", len(results))
	}
	if results[0].ProviderID != idOK {
		t.Errorf("result 0 provider = %q, want %q (registry order)", results[0].ProviderID, idOK)
	}
	if results[1].ProviderID != pBad.ID {
		t.Errorf("result 1 provider = %q, want %q (registry order)", results[1].ProviderID, pBad.ID)
	}
	if results[0].Status != ModelRefreshOK || len(results[0].Models) != 2 {
		t.Errorf("result 0 = %+v, want ok with 2 models", results[0])
	}
	if results[1].Status != ModelRefreshFailed || results[1].Error != "upstream HTTP 429" {
		t.Errorf("result 1 = %+v, want failed/upstream HTTP 429", results[1])
	}
	if got, _ := reg.Get(pBad.ID); len(got.Models) != 1 || got.Models[0].ID != "model-x" {
		t.Errorf("failed provider stored models = %+v, want unchanged [model-x]", got.Models)
	}
}

func TestMergeModelLists(t *testing.T) {
	stored := []Model{
		{ID: "a", ContextWindow: 1000, InputPriceUSD: 1, OutputPriceUSD: 2, Deprecated: true},
		{ID: "b", ContextWindow: 2000},
		{ID: "c"},
	}
	for _, tc := range []struct {
		name string
		live []string
		want []Model
	}{
		{"keeps metadata, drops removed, adds bare", []string{"a", "d"}, []Model{
			{ID: "a", ContextWindow: 1000, InputPriceUSD: 1, OutputPriceUSD: 2, Deprecated: true},
			{ID: "d"},
		}},
		{"empty live list drops everything", []string{}, []Model{}},
		{"live order wins", []string{"c", "a"}, []Model{{ID: "c"}, {ID: "a", ContextWindow: 1000, InputPriceUSD: 1, OutputPriceUSD: 2, Deprecated: true}}},
		{"duplicate live ids deduped", []string{"a", "a", "b"}, []Model{
			{ID: "a", ContextWindow: 1000, InputPriceUSD: 1, OutputPriceUSD: 2, Deprecated: true},
			{ID: "b", ContextWindow: 2000},
		}},
		{"empty live ids ignored", []string{"", "b"}, []Model{{ID: "b", ContextWindow: 2000}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeModelLists(stored, tc.live)
			if len(got) != len(tc.want) {
				t.Fatalf("merge = %+v, want %+v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("merge[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestRefreshErrorText(t *testing.T) {
	if got := refreshErrorText(&UpstreamError{StatusCode: 429, Body: "secret-body"}); got != "upstream HTTP 429" {
		t.Errorf("UpstreamError text = %q, want upstream HTTP 429 (never the body)", got)
	}
	if got := refreshErrorText(&RateLimitError{Body: "secret-body"}); got != "upstream HTTP 429" {
		t.Errorf("RateLimitError text = %q, want upstream HTTP 429 (never the body)", got)
	}
	if got := refreshErrorText(&QuotaError{StatusCode: 402, Message: "quota", Body: "secret-body"}); got != "upstream HTTP 402" {
		t.Errorf("QuotaError text = %q, want upstream HTTP 402 (never the body)", got)
	}
	if got := refreshErrorText(errors.New("connection refused")); got != "connection refused" {
		t.Errorf("plain error text = %q, want it passed through", got)
	}
}

// TestRegistryAddStampsModelsFetchedAt (CT-7): the add-time
// ListModels call stamps ModelsFetchedAt, so a list fetched at
// add time carries its age from birth — and the stamp survives a
// reload from disk.
func TestRegistryAddStampsModelsFetchedAt(t *testing.T) {
	srv, _ := newRefreshMock(t, "model-a")
	defer srv.Close()
	reg, dir := newCatalogTestRegistry(t)
	before := time.Now().UTC()
	id := addRefreshProvider(t, reg, srv)
	got, ok := reg.Get(id)
	if !ok {
		t.Fatal("provider gone after add")
	}
	if got.ModelsFetchedAt.IsZero() {
		t.Fatal("ModelsFetchedAt is zero after add with a live list fetch")
	}
	if got.ModelsFetchedAt.Before(before) || got.ModelsFetchedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("ModelsFetchedAt = %v, want within [%v, now+1m]", got.ModelsFetchedAt, before)
	}
	// Persisted: a fresh registry on the same file sees the stamp.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("reload vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)), WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got2, ok := reg2.Get(id)
	if !ok {
		t.Fatal("provider gone after reload")
	}
	if !got2.ModelsFetchedAt.Equal(got.ModelsFetchedAt) {
		t.Errorf("reloaded ModelsFetchedAt = %v, want %v (stamp must persist)", got2.ModelsFetchedAt, got.ModelsFetchedAt)
	}
}

// TestRegistryRefreshModelsStampsFetchedAt (CT-7): a successful
// refresh re-stamps ModelsFetchedAt to the refresh time; a failed
// or skipped refresh leaves the previous stamp untouched (the
// old list — and its age — survive, honestly).
func TestRegistryRefreshModelsStampsFetchedAt(t *testing.T) {
	srv, mock := newRefreshMock(t, "model-a")
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)
	id := addRefreshProvider(t, reg, srv)

	old := time.Now().UTC().Add(-48 * time.Hour)
	reg.mu.Lock()
	reg.providers[id].ModelsFetchedAt = old
	reg.mu.Unlock()

	// Success re-stamps to ~now.
	mock.set([]string{"model-a", "model-b"}, 0)
	before := time.Now().UTC()
	if _, err := reg.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	got, _ := reg.Get(id)
	if got.ModelsFetchedAt.IsZero() || got.ModelsFetchedAt.Before(before) || got.ModelsFetchedAt.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("ModelsFetchedAt after ok refresh = %v, want within [%v, now+1m] (was %v)", got.ModelsFetchedAt, before, old)
	}

	// Failure keeps the (new) stamp: the list is unchanged, so
	// its fetch time is unchanged too.
	stamped := got.ModelsFetchedAt
	mock.set(nil, http.StatusInternalServerError)
	if _, err := reg.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels (failure is a result, not an error): %v", err)
	}
	got, _ = reg.Get(id)
	if !got.ModelsFetchedAt.Equal(stamped) {
		t.Errorf("ModelsFetchedAt after failed refresh = %v, want unchanged %v", got.ModelsFetchedAt, stamped)
	}

	// Skipped (disabled) keeps the stamp as well.
	if err := reg.SetEnabled(id, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	mock.set([]string{"model-c"}, 0)
	if _, err := reg.RefreshModels(context.Background()); err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	got, _ = reg.Get(id)
	if !got.ModelsFetchedAt.Equal(stamped) {
		t.Errorf("ModelsFetchedAt after skipped refresh = %v, want unchanged %v", got.ModelsFetchedAt, stamped)
	}
}

// TestProviderModelsListStale (CT-7): the staleness rule — never
// fetched is always stale, a fresh fetch is not, and the boundary
// is exactly ModelListRefreshInterval (24h).
func TestProviderModelsListStale(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		fetchedAt time.Time
		want      bool
	}{
		{"never fetched (zero) is stale", time.Time{}, true},
		{"fetched just now is fresh", now, false},
		{"fetched 23h ago is fresh", now.Add(-23 * time.Hour), false},
		{"fetched 24h ago is stale (boundary, inclusive)", now.Add(-ModelListRefreshInterval), true},
		{"fetched 25h ago is stale", now.Add(-25 * time.Hour), true},
		{"fetched in the future (clock skew) is fresh", now.Add(time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Provider{ModelsFetchedAt: tc.fetchedAt}
			if got := p.ModelsListStale(now); got != tc.want {
				t.Errorf("ModelsListStale(%v) = %v, want %v", tc.fetchedAt, got, tc.want)
			}
		})
	}
	if got := (*Provider)(nil).ModelsListStale(now); !got {
		t.Errorf("nil provider ModelsListStale = %v, want true", got)
	}
}
