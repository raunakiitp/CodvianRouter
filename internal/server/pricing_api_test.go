package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M8.15: GET / PATCH /api/pricing handler tests.
//
// The handlers are invoked directly here (srv.handleAPIPricing)
// — route registration (guard-table row + s.register call)
// landed at merge; the direct-invocation tests still exercise
// the handler in isolation, and the fixture tests below run the
// same handler through the apitest harness. Every price asserted
// is a test-seeded mock rate (2.5/10/0.25 per 1M), never a live
// provider price.

// m815FakeRegistry is a ModelRegistry offering exactly one pair:
// mock-upstream/mock-model. It implements dashboardModelLister,
// so the handler's known-pair check reads provider+model the way
// production's provider registry does.
type m815FakeRegistry struct {
	models []provider.DashboardModel
}

func (f *m815FakeRegistry) Models(_ context.Context) ([]Model, error) {
	out := []Model{}
	for _, m := range f.models {
		out = append(out, Model{ID: m.ID, ProviderID: m.ProviderID})
	}
	return out, nil
}

func (f *m815FakeRegistry) DashboardModels() []provider.DashboardModel { return f.models }

// m815PriceBook is the seeded operator book: one fully priced
// pair (mock-upstream/mock-model at 2.5 in / 10 out / 0.25 cached
// USD per 1M, source "config").
func m815PriceBook() *config.PriceBook {
	return &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "mock-upstream", Model: "mock-model"}: {
				InputUSDPer1M:       pricePtr(2.5),
				OutputUSDPer1M:      pricePtr(10),
				CachedInputUSDPer1M: pricePtr(0.25),
			},
		},
		PairSources: map[config.PriceKey]string{
			{Provider: "mock-upstream", Model: "mock-model"}: "config",
		},
	}
}

// m815LedgerPrices is the seeded ledger price map: the same pair
// in per-token rates — the table the ledger actually charges.
func m815LedgerPrices() ledger.Prices {
	return ledger.Prices{
		{Provider: "mock-upstream", Model: "mock-model"}: {
			PromptPerToken:       2.5e-6,
			CompletionPerToken:   10e-6,
			CachedPromptPerToken: 0.25e-6,
			CachedKnown:          true,
		},
	}
}

// m815PricingServer builds a test server wired like production
// for pricing: config + persist stub, the mock registry, the
// seeded book and ledger map. persistErr, when non-nil, is what
// every configPersist call returns (the persist-failure test).
// It returns the server, the config (so tests can assert the
// ProviderPrices block was written) and the save counter.
func m815PricingServer(t *testing.T, persistErr error, extra ...Option) (*Server, *config.Config, *int) {
	t.Helper()
	cfg := &config.Config{}
	saves := 0
	opts := []Option{
		WithConfig(cfg),
		WithConfigPersist(func() error { saves++; return persistErr }),
		WithRegistry(&m815FakeRegistry{models: []provider.DashboardModel{
			{ID: "mock-model", ProviderID: "mock-upstream", ProviderName: "Mock Upstream"},
		}}),
		WithPriceBook(m815PriceBook()),
	}
	opts = append(opts, extra...)
	srv := newTestServer(t, &fakeEngine{}, opts...)
	srv.SetLedgerPrices(m815LedgerPrices())
	return srv, cfg, &saves
}

// m815CallPricing invokes the pricing handler directly (the
// route is registered at merge; until then ServeHTTP would 404).
func m815CallPricing(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	rec := httptest.NewRecorder()
	srv.handleAPIPricing(rec, req)
	return rec
}

// m815GetPricing decodes a GET /api/pricing response body.
func m815GetPricing(t *testing.T, srv *Server, path string) apiPricingResponse {
	t.Helper()
	rec := m815CallPricing(t, srv, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	var resp apiPricingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET %s: decode: %v", path, err)
	}
	return resp
}

func TestM815PricingGetFixture(t *testing.T) {
	srv, _, _ := m815PricingServer(t, nil)
	f := apitest.LoadFixture(t, "m8.15", "pricing-get-200")
	apitest.Run(t, http.HandlerFunc(srv.handleAPIPricing), f)
}

func TestM815PricingGetUnpricedPair(t *testing.T) {
	srv, _, _ := m815PricingServer(t, nil)
	resp := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=ghost-model")
	if len(resp.Prices) != 1 {
		t.Fatalf("prices = %d rows (want 1)", len(resp.Prices))
	}
	row := resp.Prices[0]
	if row.Priced {
		t.Errorf("ghost-model priced = true (want false: unpriced, never $0)")
	}
	if row.InputUSDPer1M != nil || row.OutputUSDPer1M != nil || row.BlendedUSDPer1M != nil {
		t.Errorf("ghost-model carries rates %+v (want none)", row)
	}
	if row.Source != "" {
		t.Errorf("ghost-model source = %q (want empty)", row.Source)
	}
	// And the list mode must not contain the ghost pair at all.
	list := m815GetPricing(t, srv, "/api/pricing")
	for _, r := range list.Prices {
		if r.Model == "ghost-model" {
			t.Errorf("list contains unpriced ghost-model")
		}
	}
}

func TestM815PricingGetBookOnly(t *testing.T) {
	// No ledger map wired: GET falls back to the book itself.
	srv := newTestServer(t, &fakeEngine{}, WithPriceBook(m815PriceBook()))
	resp := m815GetPricing(t, srv, "/api/pricing")
	if len(resp.Prices) != 1 {
		t.Fatalf("prices = %d rows (want 1)", len(resp.Prices))
	}
	row := resp.Prices[0]
	if !row.Priced || row.InputUSDPer1M == nil || *row.InputUSDPer1M != 2.5 {
		t.Errorf("book-only row = %+v (want priced input 2.5)", row)
	}
}

func TestM815PricingPatchFixtureRoundTrip(t *testing.T) {
	srv, cfg, saves := m815PricingServer(t, nil)
	f := apitest.LoadFixture(t, "m8.15", "pricing-patch-200")
	apitest.Run(t, http.HandlerFunc(srv.handleAPIPricing), f)

	// PATCH -> GET round trip: GET reflects the override at once.
	resp := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=mock-model")
	row := resp.Prices[0]
	if row.InputUSDPer1M == nil || *row.InputUSDPer1M != 3 || row.OutputUSDPer1M == nil || *row.OutputUSDPer1M != 12 {
		t.Fatalf("after PATCH, GET row = %+v (want input 3, output 12)", row)
	}
	// The override persisted into the config block (the durable
	// record) and the save ran exactly once.
	if *saves != 1 {
		t.Errorf("configPersist calls = %d (want 1)", *saves)
	}
	stored, ok := cfg.ProviderPrices["mock-upstream"]["mock-model"]
	if !ok {
		t.Fatalf("cfg.ProviderPrices has no mock-upstream/mock-model entry")
	}
	if stored.InputUSDPer1M == nil || *stored.InputUSDPer1M != 3 {
		t.Errorf("cfg stored input = %v (want 3)", stored.InputUSDPer1M)
	}
	// The ledger map moved with it: the ledger would now charge
	// the new rate.
	if lp := srv.getLedgerPrices(); !lp.Known("mock-upstream", "mock-model") {
		t.Errorf("ledger lost the pair after PATCH")
	} else if got := lp.Cost("mock-upstream", "mock-model", 1_000_000, 0); got != 3 {
		t.Errorf("ledger cost for 1M input tokens = %v (want 3)", got)
	}
}

func TestM815PricingPatchInvalidRateFixture(t *testing.T) {
	srv, _, saves := m815PricingServer(t, nil)
	f := apitest.LoadFixture(t, "m8.15", "pricing-patch-400-invalid-rate")
	apitest.Run(t, http.HandlerFunc(srv.handleAPIPricing), f)

	// The 400 names the bad field, and nothing was applied or
	// saved: GET still shows the old rates.
	rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":-1,"output_usd_per_1m":12}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d (want 400)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "input_usd_per_1m") {
		t.Errorf("400 body = %s (must name input_usd_per_1m)", rec.Body.String())
	}
	if *saves != 0 {
		t.Errorf("configPersist calls = %d (want 0 after a refused PATCH)", *saves)
	}
	row := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=mock-model").Prices[0]
	if row.InputUSDPer1M == nil || *row.InputUSDPer1M != 2.5 {
		t.Errorf("after refused PATCH, GET input = %v (want 2.5)", row.InputUSDPer1M)
	}
}

func TestM815PricingPatchUnknownIDs(t *testing.T) {
	srv, _, _ := m815PricingServer(t, nil)
	cases := []struct {
		name, body, want string
	}{
		{"unknown provider", `{"provider":"no-such-provider","model":"mock-model","input_usd_per_1m":1,"output_usd_per_1m":2}`, "no-such-provider"},
		{"unknown model for provider", `{"provider":"mock-upstream","model":"ghost-model","input_usd_per_1m":1,"output_usd_per_1m":2}`, "ghost-model"},
		{"missing provider", `{"model":"mock-model","input_usd_per_1m":1,"output_usd_per_1m":2}`, "provider"},
		{"no rates at all", `{"provider":"mock-upstream","model":"mock-model"}`, "rate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s (want 400)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("400 body = %s (must name %q)", rec.Body.String(), tc.want)
			}
		})
	}
}

func TestM815PricingPatchMergesFields(t *testing.T) {
	srv, _, _ := m815PricingServer(t, nil)
	rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":4}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	row := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=mock-model").Prices[0]
	if row.InputUSDPer1M == nil || *row.InputUSDPer1M != 4 {
		t.Errorf("input = %v (want 4)", row.InputUSDPer1M)
	}
	if row.OutputUSDPer1M == nil || *row.OutputUSDPer1M != 10 {
		t.Errorf("output = %v (want 10 kept: PATCH merges, it does not drop unnamed rates)", row.OutputUSDPer1M)
	}
	if row.CachedInputUSDPer1M == nil || *row.CachedInputUSDPer1M != 0.25 {
		t.Errorf("cached = %v (want 0.25 kept)", row.CachedInputUSDPer1M)
	}
}

func TestM815PricingPatchPersistFailureRollsBack(t *testing.T) {
	srv, cfg, _ := m815PricingServer(t, errors.New("disk full"))
	rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":9,"output_usd_per_1m":9}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s (want 500)", rec.Code, rec.Body.String())
	}
	// All three layers rolled back: GET, config block, ledger.
	row := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=mock-model").Prices[0]
	if row.InputUSDPer1M == nil || *row.InputUSDPer1M != 2.5 {
		t.Errorf("after failed save, GET input = %v (want 2.5 rolled back)", row.InputUSDPer1M)
	}
	if _, ok := cfg.ProviderPrices["mock-upstream"]; ok {
		t.Errorf("cfg.ProviderPrices was not rolled back: %v", cfg.ProviderPrices)
	}
	if got := srv.getLedgerPrices().Cost("mock-upstream", "mock-model", 1_000_000, 0); got != 2.5 {
		t.Errorf("after failed save, ledger cost = %v (want 2.5 rolled back)", got)
	}
}

func TestM815PricingPatchNoPersistWired(t *testing.T) {
	// No configPersist (and no config): overrides must not apply
	// runtime-only and pretend they persisted.
	srv := newTestServer(t, &fakeEngine{},
		WithRegistry(&m815FakeRegistry{models: []provider.DashboardModel{
			{ID: "mock-model", ProviderID: "mock-upstream"},
		}}),
		WithPriceBook(m815PriceBook()),
	)
	rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":9,"output_usd_per_1m":9}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s (want 503)", rec.Code, rec.Body.String())
	}
}

func TestM815PricingMethodNotAllowed(t *testing.T) {
	srv, _, _ := m815PricingServer(t, nil)
	rec := m815CallPricing(t, srv, http.MethodDelete, "/api/pricing", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d (want 405)", rec.Code)
	}
}

// TestM815PricingPatchConcurrentRace is the F1 regression
// probe: 16 concurrent PATCHes to the same pair, run under
// -race. Before the fix, validation read the price-book maps
// unlocked while another PATCH's apply wrote them — a DATA RACE
// (fatal concurrent map read/write in a non-race build). Every
// PATCH must answer 200 and the final GET must show one of the
// applied rates.
func TestM815PricingPatchConcurrentRace(t *testing.T) {
	srv, _, saves := m815PricingServer(t, nil)
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":` + strconv.Itoa(i+1) + `,"output_usd_per_1m":10}`
			rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing", body)
			if rec.Code != http.StatusOK {
				errs <- rec.Body.String()
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("concurrent PATCH failed: %s", e)
	}
	if *saves != n {
		t.Errorf("configPersist calls = %d (want %d: one save per PATCH)", *saves, n)
	}
	row := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=mock-model").Prices[0]
	if row.InputUSDPer1M == nil || *row.InputUSDPer1M < 1 || *row.InputUSDPer1M > n {
		t.Errorf("after concurrent PATCHes, GET input = %v (want one of 1..%d)", row.InputUSDPer1M, n)
	}
}

// m815HalfKnownBook is the seeded book for the F4/F5 tests: the
// fully priced mock pair, a half-known pair (input only, no
// output — the ledger cannot charge it), and a model-level
// fallback for "shared-model".
func m815HalfKnownBook() *config.PriceBook {
	book := m815PriceBook()
	book.Pairs[config.PriceKey{Provider: "mock-upstream", Model: "half-model"}] = config.ModelPrice{
		InputUSDPer1M: pricePtr(5),
	}
	book.PairSources[config.PriceKey{Provider: "mock-upstream", Model: "half-model"}] = "config"
	if book.Entries == nil {
		book.Entries = map[string]config.ModelPrice{}
	}
	if book.Sources == nil {
		book.Sources = map[string]string{}
	}
	book.Entries["shared-model"] = config.ModelPrice{
		InputUSDPer1M:  pricePtr(1),
		OutputUSDPer1M: pricePtr(2),
	}
	book.Sources["shared-model"] = "config"
	return book
}

func m815FindRow(rows []apiPricingRow, provider, model string) (apiPricingRow, bool) {
	for _, r := range rows {
		if r.Provider == provider && r.Model == model {
			return r, true
		}
	}
	return apiPricingRow{}, false
}

// TestM815PricingGetListProviderFilter is the F4 pin: list mode
// is the ledger-effective table on BOTH paths — a ?provider=
// filter keeps the model-level fallback rows (they apply to
// that provider) and the ledger path also lists half-known book
// pairs (priced:false, known side shown), exactly as the
// single-pair lookup shows them.
func TestM815PricingGetListProviderFilter(t *testing.T) {
	// Ledger path: ledger knows only the mock pair; the book
	// also holds the half-known pair and the model fallback.
	srv, _, _ := m815PricingServer(t, nil)
	srv.priceBook = m815HalfKnownBook()
	list := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream")
	if row, ok := m815FindRow(list.Prices, "", "shared-model"); !ok {
		t.Errorf("ledger list with ?provider=mock-upstream has no model-level shared-model row: %+v", list.Prices)
	} else if !row.ModelLevel || !row.Priced || row.InputUSDPer1M == nil || *row.InputUSDPer1M != 1 {
		t.Errorf("model-level row = %+v (want model_level, priced, input 1)", row)
	}
	if row, ok := m815FindRow(list.Prices, "mock-upstream", "half-model"); !ok {
		t.Errorf("ledger list has no half-known half-model row: %+v", list.Prices)
	} else if row.Priced || row.InputUSDPer1M == nil || *row.InputUSDPer1M != 5 || row.OutputUSDPer1M != nil {
		t.Errorf("half-known row = %+v (want priced:false, input 5, no output)", row)
	}
	// A filter for another provider keeps the fallback row
	// (it applies there too) but drops the mock pairs.
	other := m815GetPricing(t, srv, "/api/pricing?provider=other-provider")
	if _, ok := m815FindRow(other.Prices, "", "shared-model"); !ok {
		t.Errorf("ledger list with ?provider=other-provider has no model-level shared-model row: %+v", other.Prices)
	}
	if _, ok := m815FindRow(other.Prices, "mock-upstream", "mock-model"); ok {
		t.Errorf("ledger list with ?provider=other-provider still contains mock-upstream/mock-model")
	}

	// Book-only path: same rule, no ledger wired.
	bookSrv := newTestServer(t, &fakeEngine{}, WithPriceBook(m815HalfKnownBook()))
	bookList := m815GetPricing(t, bookSrv, "/api/pricing?provider=mock-upstream")
	if _, ok := m815FindRow(bookList.Prices, "", "shared-model"); !ok {
		t.Errorf("book-only list with ?provider=mock-upstream has no model-level shared-model row: %+v", bookList.Prices)
	}
	if _, ok := m815FindRow(bookList.Prices, "mock-upstream", "half-model"); !ok {
		t.Errorf("book-only list has no half-known half-model row: %+v", bookList.Prices)
	}
}

// TestM815PricingPatchModelLevelFallback is the F5 pin: a pair
// GET reports as priced via the book's model-level fallback can
// be PATCHed on a book-only server when the provider is known,
// even though the registry does not offer that model. An
// unknown provider is still refused.
func TestM815PricingPatchModelLevelFallback(t *testing.T) {
	cfg := &config.Config{}
	saves := 0
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithConfigPersist(func() error { saves++; return nil }),
		WithRegistry(&m815FakeRegistry{models: []provider.DashboardModel{
			{ID: "mock-model", ProviderID: "mock-upstream"},
		}}),
		WithPriceBook(m815HalfKnownBook()),
	)
	// GET reports the pair as priced via the fallback.
	row := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=shared-model").Prices[0]
	if !row.Priced || row.InputUSDPer1M == nil || *row.InputUSDPer1M != 1 {
		t.Fatalf("GET shared-model = %+v (want priced via fallback, input 1)", row)
	}
	// Unknown provider + fallback model is still a 400 — check
	// this BEFORE the PATCH below, which creates a ledger
	// model-level entry; once the ledger holds the fallback,
	// lp.Known reports the pair priced for any provider, so a
	// later PATCH for another provider would (consistently
	// with GET) be allowed.
	rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"no-such-provider","model":"shared-model","input_usd_per_1m":3,"output_usd_per_1m":6}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH unknown provider: status = %d, body = %s (want 400)", rec.Code, rec.Body.String())
	}
	rec = m815CallPricing(t, srv, http.MethodPatch, "/api/pricing",
		`{"provider":"mock-upstream","model":"shared-model","input_usd_per_1m":3,"output_usd_per_1m":6}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH fallback pair: status = %d, body = %s (want 200)", rec.Code, rec.Body.String())
	}
	if saves != 1 {
		t.Errorf("configPersist calls = %d (want 1)", saves)
	}
	after := m815GetPricing(t, srv, "/api/pricing?provider=mock-upstream&model=shared-model").Prices[0]
	if after.InputUSDPer1M == nil || *after.InputUSDPer1M != 3 || after.OutputUSDPer1M == nil || *after.OutputUSDPer1M != 6 {
		t.Errorf("after PATCH, GET shared-model = %+v (want input 3, output 6)", after)
	}
}
func TestM815FixtureWrongStatusFails(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.15", "pricing-get-200")
	f.Assert.Status = 201 // wrong expectation on purpose
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{"prices":[]}`), f); err == nil {
		t.Fatal("CheckResponse accepted a 200 response against a 201 expectation")
	}
}
