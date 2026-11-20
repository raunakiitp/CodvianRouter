package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M8.17: GET /api/analytics/summary, /by-provider, /by-model,
// /costs — read-only roll-ups over stored ledger rows.
//
// The routes are NOT registered in server.go by this slice (the
// coordinator owns registration + the guard table), so these
// tests drive the handlers directly — the same handler the guard
// will wrap — and the apitest fixture contract tests run the
// handler on an httptest server via apitest.Run. Auth is the
// guard table's job (proposed: Tier 3 MANAGEMENT + admin scope,
// like /api/usage/budget); the handlers themselves do no auth,
// exactly like handleUsageBudget.

// analyticsPrices is the price book the seeded rows are recorded
// against: "priced-model" costs $10/1M in + $30/1M out (1,000 +
// 1,000 tokens = $0.04); every other model is unpriced.
func analyticsPrices() ledger.Prices {
	return ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
}

// seedAnalyticsRows records the two mock rows every fixture in
// tests/fixtures/v1/m8.17/ assumes: one priced row
// (mock-upstream/priced-model, 1000 prompt + 1000 completion,
// $0.04) and one unpriced row (mock-upstream/mock-model, 500 +
// 500, cost unknown). Both are recorded "now", so they fall in
// the day, week and month windows.
func seedAnalyticsRows(t *testing.T, s *keyBudgetSetup) {
	t.Helper()
	prices := analyticsPrices()
	for _, u := range []ledger.Usage{
		{GatewayKeyID: s.id, Provider: "mock-upstream", Model: "priced-model",
			PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: true, LatencyMS: 10},
		{GatewayKeyID: s.id, Provider: "mock-upstream", Model: "mock-model",
			PromptTokens: 500, CompletionTokens: 500, SplitKnown: true, OK: true, LatencyMS: 20},
	} {
		if _, err := s.led.RecordUsage(context.Background(), u, prices); err != nil {
			t.Fatalf("seed analytics row: %v", err)
		}
	}
}

// seedAnalyticsPricedOnly records a single priced row: the
// cost_status "complete" case.
func seedAnalyticsPricedOnly(t *testing.T, s *keyBudgetSetup) {
	t.Helper()
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "mock-upstream", Model: "priced-model",
		PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: true, LatencyMS: 10,
	}, analyticsPrices()); err != nil {
		t.Fatalf("seed priced row: %v", err)
	}
}

// analyticsSetup builds a server with a real ledger + config.
func analyticsSetup(t *testing.T) (*keyBudgetSetup, *Server) {
	t.Helper()
	s, _ := keyStatusSetup(t)
	return s, s.server(t)
}

// callAnalytics invokes one analytics handler directly (no guard
// — the guard table is the coordinator's registration) and
// returns the recorder.
func callAnalytics(t *testing.T, handler func(http.ResponseWriter, *http.Request), method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func decodeAnalytics[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode analytics response: %v: %s", err, rec.Body.String())
	}
	return out
}

// --- Fixture contract tests (one per endpoint + empty + error) ---

func TestAnalyticsSummaryFixture(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsSummary),
		apitest.LoadFixture(t, "m8.17", "analytics-summary-200"))
}

func TestAnalyticsSummaryEmptyFixture(t *testing.T) {
	_, srv := analyticsSetup(t)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsSummary),
		apitest.LoadFixture(t, "m8.17", "analytics-summary-empty-200"))
}

func TestAnalyticsByProviderFixture(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsByProvider),
		apitest.LoadFixture(t, "m8.17", "analytics-by-provider-200"))
}

func TestAnalyticsByModelFixture(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsByModel),
		apitest.LoadFixture(t, "m8.17", "analytics-by-model-200"))
}

func TestAnalyticsCostsFixture(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsCosts),
		apitest.LoadFixture(t, "m8.17", "analytics-costs-200"))
}

func TestAnalyticsCostsEmptyFixture(t *testing.T) {
	_, srv := analyticsSetup(t)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsCosts),
		apitest.LoadFixture(t, "m8.17", "analytics-costs-empty-200"))
}

func TestAnalyticsSummaryBadWindowFixture(t *testing.T) {
	_, srv := analyticsSetup(t)
	apitest.Run(t, http.HandlerFunc(srv.handleAnalyticsSummary),
		apitest.LoadFixture(t, "m8.17", "analytics-summary-bad-window-400"))
}

// --- Value tests (exact numbers the fixtures cannot pin) ---

func TestAnalyticsSummaryValues(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	rec := callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsSummaryResponse](t, rec)
	if out.Name != "day" || !out.HasData {
		t.Errorf("window/has_data = %q/%v, want day/true", out.Name, out.HasData)
	}
	if out.Requests != 2 || out.OK != 2 || out.Errors != 0 {
		t.Errorf("requests/ok/errors = %d/%d/%d, want 2/2/0", out.Requests, out.OK, out.Errors)
	}
	if out.PromptTokens != 1500 || out.CompletionTokens != 1500 || out.TotalTokens != 3000 {
		t.Errorf("prompt/completion/total = %d/%d/%d, want 1500/1500/3000", out.PromptTokens, out.CompletionTokens, out.TotalTokens)
	}
	if out.PricedCostUSD != 0.04 || out.CostStatus != "priced_only" {
		t.Errorf("priced_cost_usd/cost_status = %v/%q, want 0.04/priced_only", out.PricedCostUSD, out.CostStatus)
	}
	if out.UnpricedRequests == nil || *out.UnpricedRequests != 1 || out.UnpricedTokens == nil || *out.UnpricedTokens != 1000 {
		t.Errorf("unpriced requests/tokens = %v/%v, want 1/1000", out.UnpricedRequests, out.UnpricedTokens)
	}
	if out.MeanLatencyMS != 15 {
		t.Errorf("mean_latency_ms = %v, want 15 (the mean of the two seeded rows)", out.MeanLatencyMS)
	}
	// The day window is today's UTC calendar day at 00:00.
	now := time.Now().UTC()
	wantStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if !out.Start.Equal(wantStart) || !out.End.Equal(wantStart.AddDate(0, 0, 1)) {
		t.Errorf("window_start/end = %v/%v, want %v/%v", out.Start, out.End, wantStart, wantStart.AddDate(0, 0, 1))
	}
	if out.Note == "" {
		t.Error("priced_only response must carry a note saying the figure is a lower bound")
	}
}

func TestAnalyticsCostsComplete(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsPricedOnly(t, s)
	rec := callAnalytics(t, srv.handleAnalyticsCosts, http.MethodGet, "/api/analytics/costs?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsCostsResponse](t, rec)
	if !out.HasData || out.Requests != 1 || out.PricedCostUSD != 0.04 || out.CostStatus != "complete" {
		t.Errorf("costs = %+v, want has_data/requests 1/priced 0.04/complete", out)
	}
	if out.UnpricedRequests == nil || *out.UnpricedRequests != 0 {
		t.Errorf("unpriced_requests = %v, want 0 (all rows priced)", out.UnpricedRequests)
	}
	if out.Note == "" {
		t.Error("complete-cost response must carry a note saying the split covers gateway keys still in config only")
	} else if !strings.Contains(out.Note, "still in config") {
		t.Errorf("complete-cost note = %q, want it to say the split covers keys still in config only", out.Note)
	}
}

// TestAnalyticsCostsByKeyValues proves the by_key roll-up on
// GET /api/analytics/costs: priced spend per gateway key from
// real ledger rows, with unpriced rows counted per key under
// the fail-closed policy (priced_cost_usd is a lower bound when
// unpriced_requests > 0), and a configured key with no rows
// absent — never zero-filled. Failed rows and shared
// (coalesced-waiter) rows are excluded from the per-key
// unpriced count: both are zero-token/zero-cost by
// construction, so their unknown price is irrelevant to cost.
func TestAnalyticsCostsByKeyValues(t *testing.T) {
	s, _ := keyStatusSetup(t)
	key2, _, err := config.IssueKey("test-2", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key2); err != nil {
		t.Fatal(err)
	}
	// A third configured key that records nothing: it must not
	// appear in by_key at all.
	key3, _, err := config.IssueKey("test-3", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key3); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	prices := analyticsPrices()
	record := func(keyID, model string, prompt, completion int, ok bool) {
		t.Helper()
		if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: "mock-upstream", Model: model,
			PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: ok, LatencyMS: 10,
		}, prices); err != nil {
			t.Fatalf("seed by-key row: %v", err)
		}
	}
	// Key 1: one priced row (1000+1000 priced-model = $0.04)
	// plus one unpriced row (500+500 mock-model, cost unknown).
	record(s.id, "priced-model", 1000, 1000, true)
	record(s.id, "mock-model", 500, 500, true)
	// Key 2: one priced row (2000 prompt + 1000 completion =
	// $0.02 + $0.03 = $0.05), nothing unpriced.
	record(key2.ID, "priced-model", 2000, 1000, true)
	// A failed unpriced row under key 1: zero tokens, zero
	// cost — it counts in requests but must NOT join the
	// key's unpriced count (it spent nothing, so its unknown
	// price is irrelevant to cost).
	record(s.id, "mock-model", 0, 0, false)
	// A shared/coalesced waiter row under key 1, on the
	// unpriced model with real-looking tokens: it is OK and
	// zero-token/zero-cost by construction (the leader's row
	// carries the real usage), so it counts in requests and
	// OK but must NOT join the key's unpriced count — the
	// per-key GROUP BY carries shared = 0.
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "mock-upstream", Model: "mock-model",
		PromptTokens: 500, CompletionTokens: 500, SplitKnown: true, OK: true, LatencyMS: 10, Shared: true,
	}, prices); err != nil {
		t.Fatalf("seed shared by-key row: %v", err)
	}

	rec := callAnalytics(t, srv.handleAnalyticsCosts, http.MethodGet, "/api/analytics/costs?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsCostsResponse](t, rec)
	if !out.HasData || out.Requests != 5 {
		t.Fatalf("costs has_data/requests = %v/%d, want true/5: %s", out.HasData, out.Requests, rec.Body.String())
	}
	if out.PricedCostUSD != 0.09 || out.CostStatus != "priced_only" {
		t.Errorf("priced_cost_usd/cost_status = %v/%q, want 0.09/priced_only", out.PricedCostUSD, out.CostStatus)
	}
	if out.UnpricedRequests == nil || *out.UnpricedRequests != 1 {
		t.Errorf("unpriced_requests = %v, want 1 (only key 1's successful unpriced row)", out.UnpricedRequests)
	}
	if out.ByKey == nil {
		t.Fatal("by_key is null, want a JSON array (one row per key with ledger rows)")
	}
	if len(out.ByKey) != 2 {
		t.Fatalf("by_key rows = %d, want 2 (keys 1 and 2 only): %s", len(out.ByKey), rec.Body.String())
	}
	byKey := map[string]analyticsKeyRow{}
	for _, k := range out.ByKey {
		byKey[k.GatewayKeyID] = k
	}
	k1 := byKey[s.id]
	if k1.Requests != 4 || k1.OK != 3 || k1.PromptTokens != 1500 || k1.CompletionTokens != 1500 ||
		k1.TotalTokens != 3000 || k1.PricedCostUSD != 0.04 || k1.UnpricedRequests != 1 || !k1.UnpricedRequestsKnown {
		t.Errorf("key 1 row = %+v, want requests 4/ok 3/tokens 1500/1500/3000/priced 0.04/unpriced 1/known", k1)
	}
	k2 := byKey[key2.ID]
	if k2.Requests != 1 || k2.OK != 1 || k2.PromptTokens != 2000 || k2.CompletionTokens != 1000 ||
		k2.TotalTokens != 3000 || k2.PricedCostUSD != 0.05 || k2.UnpricedRequests != 0 || !k2.UnpricedRequestsKnown {
		t.Errorf("key 2 row = %+v, want requests 1/ok 1/tokens 2000/1000/3000/priced 0.05/unpriced 0/known", k2)
	}
	if _, ok := byKey[key3.ID]; ok {
		t.Errorf("key 3 (no rows) appears in by_key — a key with no spend must be absent, not zero-filled")
	}
	// The per-key priced spend must reconcile exactly to the
	// window's priced total: the roll-up partitions the
	// ledger, it does not re-price it.
	sum := 0.0
	for _, k := range out.ByKey {
		sum += k.PricedCostUSD
	}
	if sum != out.PricedCostUSD {
		t.Errorf("sum of by_key priced_cost_usd = %v, want the window priced_cost_usd %v", sum, out.PricedCostUSD)
	}
}

// TestAnalyticsCostsByKeyEmpty: an empty window reads
// "by_key":[] — an honest empty state, never null and never a
// zero-filled row for a configured key.
func TestAnalyticsCostsByKeyEmpty(t *testing.T) {
	_, srv := analyticsSetup(t)
	rec := callAnalytics(t, srv.handleAnalyticsCosts, http.MethodGet, "/api/analytics/costs?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsCostsResponse](t, rec)
	if out.ByKey == nil {
		t.Error("by_key is null in an empty window, want []")
	}
	if len(out.ByKey) != 0 {
		t.Errorf("by_key rows = %d, want 0 in an empty window", len(out.ByKey))
	}
}

// TestLedgerSummaryOrEmptyByKey: with no ledger wired,
// ledgerSummaryOrEmpty returns an empty summary whose by_key
// roll-up marshals as [] (not null), like the other roll-ups.
func TestLedgerSummaryOrEmptyByKey(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	sum := srv.ledgerSummaryOrEmpty(context.Background(), time.Now().Add(-time.Hour), time.Now())
	if sum.ByKey == nil {
		t.Error("ledgerSummaryOrEmpty: ByKey is nil, want empty non-nil slice")
	}
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal summary: %v", err)
	}
	if !strings.Contains(string(b), `"by_key":[]`) {
		t.Errorf("ledgerSummaryOrEmpty marshals as %s, want \"by_key\":[]", b)
	}
	if strings.Contains(string(b), `"by_key":null`) {
		t.Errorf("ledgerSummaryOrEmpty marshals by_key as null: %s", b)
	}
}

func TestAnalyticsByModelValues(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	rec := callAnalytics(t, srv.handleAnalyticsByModel, http.MethodGet, "/api/analytics/by-model?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsByModelResponse](t, rec)
	if !out.HasData || len(out.Models) != 2 {
		t.Fatalf("models = %d, want 2: %s", len(out.Models), rec.Body.String())
	}
	byModel := map[string]analyticsModelRow{}
	for _, m := range out.Models {
		byModel[m.Model] = m
	}
	priced := byModel["priced-model"]
	if priced.Requests != 1 || priced.PricedCostUSD != 0.04 || priced.UnpricedRequests != 0 || !priced.UnpricedRequestsKnown {
		t.Errorf("priced-model row = %+v, want requests 1/priced 0.04/unpriced 0/known", priced)
	}
	unpriced := byModel["mock-model"]
	if unpriced.Requests != 1 || unpriced.PricedCostUSD != 0 || unpriced.UnpricedRequests != 1 || !unpriced.UnpricedRequestsKnown {
		t.Errorf("mock-model row = %+v, want requests 1/priced 0/unpriced 1/known", unpriced)
	}
}

func TestAnalyticsByProviderValues(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	rec := callAnalytics(t, srv.handleAnalyticsByProvider, http.MethodGet, "/api/analytics/by-provider?window=day")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	out := decodeAnalytics[analyticsByProviderResponse](t, rec)
	if len(out.Providers) != 1 {
		t.Fatalf("providers = %d, want 1: %s", len(out.Providers), rec.Body.String())
	}
	p := out.Providers[0]
	if p.Provider != "mock-upstream" || p.Requests != 2 || p.PricedCostUSD != 0.04 || p.UnpricedRequests != 1 {
		t.Errorf("provider row = %+v, want mock-upstream/2/0.04/unpriced 1", p)
	}
	if p.PromptTokens != 1500 || p.TotalTokens != 3000 {
		t.Errorf("provider prompt/total = %d/%d, want 1500/3000", p.PromptTokens, p.TotalTokens)
	}
}

// A failed row (recorded the way the router records one: zero
// tokens, ok=false) counts in requests/errors but never in the
// priced split — it spent nothing, so its unknown price is
// irrelevant to cost.
func TestAnalyticsFailedRowExcludedFromSplit(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "mock-upstream", Model: "mock-model",
		SplitKnown: true, OK: false, LatencyMS: 5,
	}, analyticsPrices()); err != nil {
		t.Fatalf("seed failed row: %v", err)
	}
	rec := callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary?window=day")
	out := decodeAnalytics[analyticsSummaryResponse](t, rec)
	if out.Requests != 3 || out.OK != 2 || out.Errors != 1 {
		t.Errorf("requests/ok/errors = %d/%d/%d, want 3/2/1", out.Requests, out.OK, out.Errors)
	}
	if out.UnpricedRequests == nil || *out.UnpricedRequests != 1 {
		t.Errorf("unpriced_requests = %v, want 1 (the failed row must not join the split)", out.UnpricedRequests)
	}
	if out.PricedCostUSD != 0.04 || out.CostStatus != "priced_only" {
		t.Errorf("priced/cost_status = %v/%q, want 0.04/priced_only", out.PricedCostUSD, out.CostStatus)
	}
}

// --- Window / method / no-ledger behaviour ---

func TestAnalyticsWindows(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	// Default window ("") is day; week and month both contain
	// today's rows; each labels itself.
	for _, tc := range []struct{ query, want string }{
		{"", "day"},
		{"?window=day", "day"},
		{"?window=week", "week"},
		{"?window=month", "month"},
	} {
		rec := callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary"+tc.query)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET summary%s: status %d, want 200: %s", tc.query, rec.Code, rec.Body.String())
		}
		out := decodeAnalytics[analyticsSummaryResponse](t, rec)
		if out.Name != tc.want || out.Requests != 2 {
			t.Errorf("GET summary%s: window/requests = %q/%d, want %q/2", tc.query, out.Name, out.Requests, tc.want)
		}
	}
	// The week window starts on a Monday, the month window on
	// the 1st, both at 00:00 UTC.
	rec := callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary?window=week")
	week := decodeAnalytics[analyticsSummaryResponse](t, rec)
	if week.Start.Weekday() != time.Monday || week.Start.Hour() != 0 {
		t.Errorf("week window_start = %v, want a Monday 00:00 UTC", week.Start)
	}
	rec = callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary?window=month")
	month := decodeAnalytics[analyticsSummaryResponse](t, rec)
	if month.Start.Day() != 1 || month.Start.Hour() != 0 {
		t.Errorf("month window_start = %v, want the 1st 00:00 UTC", month.Start)
	}
	// Unknown window on every endpoint: 400, not a silent
	// fallback to another period.
	for _, h := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"summary", srv.handleAnalyticsSummary, "/api/analytics/summary?window=yearly"},
		{"by-provider", srv.handleAnalyticsByProvider, "/api/analytics/by-provider?window=yearly"},
		{"by-model", srv.handleAnalyticsByModel, "/api/analytics/by-model?window=yearly"},
		{"costs", srv.handleAnalyticsCosts, "/api/analytics/costs?window=yearly"},
	} {
		if rec := callAnalytics(t, h.fn, http.MethodGet, h.path); rec.Code != http.StatusBadRequest {
			t.Errorf("%s bad window: status %d, want 400: %s", h.name, rec.Code, rec.Body.String())
		}
	}
}

func TestAnalyticsMethodNotAllowed(t *testing.T) {
	_, srv := analyticsSetup(t)
	for _, h := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"summary", srv.handleAnalyticsSummary, "/api/analytics/summary"},
		{"by-provider", srv.handleAnalyticsByProvider, "/api/analytics/by-provider"},
		{"by-model", srv.handleAnalyticsByModel, "/api/analytics/by-model"},
		{"costs", srv.handleAnalyticsCosts, "/api/analytics/costs"},
	} {
		if rec := callAnalytics(t, h.fn, http.MethodPost, h.path); rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s POST: status %d, want 405", h.name, rec.Code)
		}
	}
}

func TestAnalyticsNoLedger(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	for _, h := range []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
		path string
	}{
		{"summary", srv.handleAnalyticsSummary, "/api/analytics/summary"},
		{"by-provider", srv.handleAnalyticsByProvider, "/api/analytics/by-provider"},
		{"by-model", srv.handleAnalyticsByModel, "/api/analytics/by-model"},
		{"costs", srv.handleAnalyticsCosts, "/api/analytics/costs"},
	} {
		if rec := callAnalytics(t, h.fn, http.MethodGet, h.path); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without ledger: status %d, want 503: %s", h.name, rec.Code, rec.Body.String())
		}
	}
}

// --- Toggle verification: the contract tests must fail when the
// feature (or the contract) is removed. ---

// A fixture asserting the wrong status must fail CheckResponse
// against the real 200 response.
func TestAnalyticsFixtureWrongStatusFails(t *testing.T) {
	s, srv := analyticsSetup(t)
	seedAnalyticsRows(t, s)
	f := apitest.LoadFixture(t, "m8.17", "analytics-summary-200")
	f.Assert.Status = 201 // wrong: the handler really answers 200
	rec := callAnalytics(t, srv.handleAnalyticsSummary, http.MethodGet, "/api/analytics/summary?window=day")
	resp := &http.Response{StatusCode: rec.Code}
	if err := apitest.CheckResponse(resp, rec.Body.Bytes(), f); err == nil {
		t.Error("CheckResponse with assert.status 201 passed against a real 200 response — the toggle is broken")
	}
}

// The same fixture must fail against a handler that returns an
// empty object: the required fields are the toggle.
func TestAnalyticsFixtureFailsAgainstEmptyHandler(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.17", "analytics-summary-200")
	empty := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	})
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/analytics/summary?window=day", nil))
	fakeResp := &http.Response{StatusCode: rec.Code}
	if err := apitest.CheckResponse(fakeResp, rec.Body.Bytes(), f); err == nil {
		t.Error("CheckResponse passed against an empty-object handler — the required-fields toggle is broken")
	}
}
