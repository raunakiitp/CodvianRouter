package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

// M8.11: me/quotas v1 surface — GET /v1/me/sessions/{id},
// POST /v1/quotas/check, and per-window total request counts in
// me/status (Area 11 row 724).
//
// The two new routes are NOT registered in server.go yet (the
// coordinator owns guard.go/server.go registration), so these
// tests drive the handlers through a test-only mux with the same
// patterns the coordinator will register. The handlers guard
// identity themselves (callerKeyID → 401), exactly like
// handleMeSessions, so the mux tests prove the auth behaviour
// the guard wrapper will front in production.
//
// Fixture tests load their fixture with apitest.LoadFixture and
// replace the placeholder "Bearer <redacted>" header with the
// test's own issued key — a fixture file must never carry a real
// key.

// quotasMux serves the M8.11 handlers (plus me/status, for the
// total-requests fixture) on the patterns the coordinator will
// register in server.go.
func quotasMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/me/sessions/{id}", srv.handleMeSessionDetail)
	mux.HandleFunc("/v1/quotas/check", srv.handleQuotasCheck)
	mux.HandleFunc("/v1/me/status", srv.handleMeStatus)
	return mux
}

func quotasDoReq(t *testing.T, mux http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
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
	mux.ServeHTTP(rec, req)
	return rec
}

// quotasMockModelServer builds a server like keyBudgetSetup.server
// but with "mock-model" priced at $10/1M in + $30/1M out in the
// price book and the ledger prices — the mock-model rates the
// quotas-check fixtures are written against. Test-only rates,
// never a real provider price.
func quotasMockModelServer(t *testing.T, s *keyBudgetSetup) *Server {
	t.Helper()
	s.book.Entries["mock-model"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	prices := ledger.Prices{
		ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
		ledger.PriceKey{Model: "mock-model"}:   ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
	}
	return newTestServer(t, s.eng, WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book), WithLedgerPrices(prices))
}

// seedQuotaSession seeds one two-turn session (root + child) for
// the fixture key, plus another key's session and a keyless
// (pre-M5.6) session that must never be visible to this caller.
// Same numbers as TestMeSessions: root 1M+1M, child 500+500 at
// cheap-model $0.5/$1.5 per 1M → list $2.001.
func seedQuotaSession(t *testing.T, srv *Server, keyID string) {
	t.Helper()
	now := time.Now().Unix()
	srv.responses.Put(responses.Record{
		ID: "resp-root1", Model: "cheap-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 3600,
		TurnInputTokens: 1_000_000, TurnOutputTokens: 1_000_000,
		SessionInputTokens: 1_000_000, SessionOutputTokens: 1_000_000,
		KeyID: keyID,
	})
	srv.responses.Put(responses.Record{
		ID: "resp-child1", Model: "cheap-model", ProviderID: "prov-a",
		Status: responses.StatusCompleted, CreatedAt: now - 1800,
		PreviousID:      "resp-root1",
		TurnInputTokens: 500, TurnOutputTokens: 500,
		SessionInputTokens: 1_000_500, SessionOutputTokens: 1_000_500,
		KeyID: keyID,
	})
	srv.responses.Put(responses.Record{
		ID: "resp-other", Model: "cheap-model",
		Status: responses.StatusCompleted, CreatedAt: now - 30,
		TurnInputTokens: 10, TurnOutputTokens: 10, KeyID: "other-key",
	})
	srv.responses.Put(responses.Record{
		ID: "resp-legacy", Model: "cheap-model",
		Status: responses.StatusCompleted, CreatedAt: now - 10,
		TurnInputTokens: 10, TurnOutputTokens: 10,
	})
}

// --- GET /v1/me/sessions/{id} ---

func TestMeSessionDetailFixture200(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedQuotaSession(t, srv, s.id)
	f := apitest.LoadFixture(t, "m8.11", "me-session-detail-200")
	f.Request.Headers["Authorization"] = "Bearer " + s.raw
	apitest.Run(t, quotasMux(srv), f)
}

func TestMeSessionDetailFixture404(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	f := apitest.LoadFixture(t, "m8.11", "me-session-detail-404")
	f.Request.Headers["Authorization"] = "Bearer " + s.raw
	apitest.Run(t, quotasMux(srv), f)
}

// TestMeSessionDetailFixtureWrongStatusFails is the toggle proof
// for the fixture assertions: the same 200 response must FAIL
// the check when the expected status is wrong (201).
func TestMeSessionDetailFixtureWrongStatusFails(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedQuotaSession(t, srv, s.id)
	mux := quotasMux(srv)
	f := apitest.LoadFixture(t, "m8.11", "me-session-detail-200")
	f.Assert.Status = 201
	rec := quotasDoReq(t, mux, "GET", "/v1/me/sessions/resp-root1", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := apitest.CheckResponse(&http.Response{StatusCode: rec.Code}, rec.Body.Bytes(), f); err == nil {
		t.Fatal("CheckResponse passed with expected status 201 against a 200 response; want failure")
	}
}

func TestMeSessionDetail(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedQuotaSession(t, srv, s.id)
	mux := quotasMux(srv)

	// The root id returns the whole session, list row + turns.
	rec := quotasDoReq(t, mux, "GET", "/v1/me/sessions/resp-root1", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out meSessionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if out.Object != "session" || out.ID != "resp-root1" {
		t.Errorf("detail = object %q id %q, want session/resp-root1", out.Object, out.ID)
	}
	if out.Turns != 2 || out.InputTokens != 1_000_500 || out.OutputTokens != 1_000_500 {
		t.Errorf("detail totals = turns %d in %d out %d, want 2/1000500/1000500", out.Turns, out.InputTokens, out.OutputTokens)
	}
	if out.ListPriceUSD == nil || *out.ListPriceUSD != 2.001 {
		t.Errorf("detail list_price_usd = %v, want 2.001", out.ListPriceUSD)
	}
	if len(out.TurnsDetail) != 2 || out.TurnsDetailTruncated {
		t.Fatalf("turns_detail = %d rows truncated=%v, want 2/false", len(out.TurnsDetail), out.TurnsDetailTruncated)
	}
	if out.TurnsDetail[0].ID != "resp-root1" || out.TurnsDetail[1].ID != "resp-child1" {
		t.Errorf("turns_detail order = %q, %q, want resp-root1 (oldest) then resp-child1", out.TurnsDetail[0].ID, out.TurnsDetail[1].ID)
	}
	if out.TurnsDetail[0].ListPriceUSD == nil || *out.TurnsDetail[0].ListPriceUSD != 2.0 {
		t.Errorf("root turn list_price_usd = %v, want 2.0", out.TurnsDetail[0].ListPriceUSD)
	}

	// A TURN id resolves to its session, not a 404.
	rec = quotasDoReq(t, mux, "GET", "/v1/me/sessions/resp-child1", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("turn id: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var viaTurn meSessionDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &viaTurn); err != nil {
		t.Fatalf("decode turn-id detail: %v", err)
	}
	if viaTurn.ID != "resp-root1" || viaTurn.Turns != 2 {
		t.Errorf("turn id detail = id %q turns %d, want resp-root1/2", viaTurn.ID, viaTurn.Turns)
	}

	// Another key's session, a keyless session, and an unknown
	// id are all the same honest 404.
	for _, id := range []string{"resp-other", "resp-legacy", "no-such-session"} {
		rec = quotasDoReq(t, mux, "GET", "/v1/me/sessions/"+id, "", s.raw)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET /v1/me/sessions/%s: status = %d, want 404", id, rec.Code)
		}
	}
}

func TestMeSessionDetailRequiresKey(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := quotasDoReq(t, quotasMux(srv), "GET", "/v1/me/sessions/resp-root1", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 without a key", rec.Code)
	}
}

func TestMeSessionDetailMethodNotAllowed(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/me/sessions/resp-root1", "{}", s.raw)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405 for POST", rec.Code)
	}
}

// --- POST /v1/quotas/check ---

func TestQuotasCheckFixtureAllowed(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	f := apitest.LoadFixture(t, "m8.11", "quotas-check-200-allowed")
	f.Request.Headers["Authorization"] = "Bearer " + s.raw
	apitest.Run(t, quotasMux(srv), f)
}

func TestQuotasCheckFixtureDenied(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of spend
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	f := apitest.LoadFixture(t, "m8.11", "quotas-check-200-denied")
	f.Request.Headers["Authorization"] = "Bearer " + s.raw
	apitest.Run(t, quotasMux(srv), f)
}

func TestQuotasCheckValidation(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	mux := quotasMux(srv)
	for name, body := range map[string]string{
		"missing model":   `{"provider":"mock-upstream","estimated_tokens":100}`,
		"negative tokens": `{"model":"mock-model","estimated_tokens":-1}`,
		"split mismatch":  `{"model":"mock-model","estimated_tokens":100,"estimated_prompt_tokens":60,"estimated_completion_tokens":50}`,
		"bad json":        `{not json`,
	} {
		rec := quotasDoReq(t, mux, "POST", "/v1/quotas/check", body, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", name, rec.Code, rec.Body.String())
		}
	}
	rec := quotasDoReq(t, mux, "POST", "/v1/quotas/check", `{"model":"mock-model","estimated_tokens":10}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: status = %d, want 401", rec.Code)
	}
	rec = quotasDoReq(t, mux, "GET", "/v1/quotas/check", "", s.raw)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}
}

func TestQuotasCheckNoLedger503(t *testing.T) {
	s, _ := keyStatusSetup(t)
	// Ledger deliberately unwired: the check must fail closed,
	// never wave a verdict through blind.
	srv := newTestServer(t, s.eng, WithConfig(s.cfg), WithPriceBook(s.book))
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"mock-model","estimated_tokens":10}`, s.raw)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 with no ledger: %s", rec.Code, rec.Body.String())
	}
}

func TestQuotasCheckUnpricedDenied(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10.0, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"unpriced-model","estimated_tokens":100}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (a refusal is a verdict): %s", rec.Code, rec.Body.String())
	}
	var out quotaCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Allowed || out.Reason != "unpriced_model" {
		t.Errorf("verdict = allowed=%v reason %q, want false/unpriced_model: %+v", out.Allowed, out.Reason, out)
	}
	if len(out.UnpricedModels) != 1 || out.UnpricedModels[0] != "unpriced-model" {
		t.Errorf("unpriced_models = %v, want [unpriced-model]", out.UnpricedModels)
	}
	// Unknown price is unknown, never $0.
	if out.PriceKnown || out.EstimatedCostUSD != nil {
		t.Errorf("price_known=%v estimated_cost_usd=%v, want false/nil (unknown, never $0)", out.PriceKnown, out.EstimatedCostUSD)
	}
}

func TestQuotasCheckProjectedBudgetDenied(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	s.seedPricedSpend(t, 1000, 1000) // $0.04 spent
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.05, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	// Current spend fits ($0.04 < $0.05), but the estimate
	// (2000 tokens × $10/1M input = $0.02) projects to $0.06 —
	// the same refusal ReserveSpend would make for a real
	// request with this prompt.
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"priced-model","estimated_tokens":2000}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Allowed || out.Reason != "budget_exceeded" {
		t.Errorf("verdict = allowed=%v reason %q, want false/budget_exceeded: %+v", out.Allowed, out.Reason, out)
	}
	if out.Budget.ProjectedSpent == nil || *out.Budget.ProjectedSpent < 0.05 {
		t.Errorf("projected_spent = %v, want >= 0.05", out.Budget.ProjectedSpent)
	}
	if out.Budget.LimitUSD == nil || *out.Budget.LimitUSD != 0.05 {
		t.Errorf("budget limit = %v, want 0.05", out.Budget.LimitUSD)
	}
}

func TestQuotasCheckSplitEstimatePricing(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	// Split given: 1000 prompt × $10 + 1000 completion × $30,
	// per 1M = $0.04, priced at the split basis.
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"mock-model","estimated_prompt_tokens":1000,"estimated_completion_tokens":1000}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Allowed || out.Reason != "ok" {
		t.Errorf("verdict = allowed=%v reason %q, want true/ok", out.Allowed, out.Reason)
	}
	if out.EstimatedTokens != 2000 || out.EstimatedCostBasis != "split" {
		t.Errorf("estimate = tokens %d basis %q, want 2000/split", out.EstimatedTokens, out.EstimatedCostBasis)
	}
	if out.EstimatedCostUSD == nil || *out.EstimatedCostUSD != 0.04 {
		t.Errorf("estimated_cost_usd = %v, want 0.04", out.EstimatedCostUSD)
	}
}

func TestQuotasCheckTokenLimitDenied(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].TokenLimits = []config.TokenLimit{
				{Scope: "global", Limit: 100, ResetInterval: "daily", ResetTime: "00:00"},
			}
		}
	}
	s.seedPricedSpend(t, 100, 50) // 150 tokens used, over the 100 cap
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"priced-model","estimated_tokens":10}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Allowed || out.Reason != "token_limit_exceeded" {
		t.Errorf("verdict = allowed=%v reason %q, want false/token_limit_exceeded: %+v", out.Allowed, out.Reason, out)
	}
	if len(out.TokenLimits) != 1 {
		t.Fatalf("token_limits = %d rows, want 1: %+v", len(out.TokenLimits), out.TokenLimits)
	}
	row := out.TokenLimits[0]
	if row.Scope != "global" || row.Limit != 100 || row.Used != 150 || row.Remaining != 0 {
		t.Errorf("token limit row = %+v, want global/100/used 150/remaining 0", row)
	}
}

func TestQuotasCheckTokenLimitProjectionDenied(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := quotasMockModelServer(t, s)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].TokenLimits = []config.TokenLimit{
				{Scope: "global", Limit: 100, ResetInterval: "daily", ResetTime: "00:00"},
			}
		}
	}
	s.seedPricedSpend(t, 60, 30) // 90 used: under the cap, so the live gate admits…
	rec := quotasDoReq(t, quotasMux(srv), "POST", "/v1/quotas/check", `{"model":"priced-model","estimated_tokens":50}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaCheckResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// …but 90 + 50 = 140 would not fit in the remaining 10, so
	// the check refuses the over-pushing request itself (the
	// documented conservative projection).
	if out.Allowed || out.Reason != "token_limit_exceeded" {
		t.Errorf("verdict = allowed=%v reason %q, want false/token_limit_exceeded: %+v", out.Allowed, out.Reason, out)
	}
	if len(out.TokenLimits) != 1 || !out.TokenLimits[0].WouldExceed || out.TokenLimits[0].Used != 90 || out.TokenLimits[0].Remaining != 10 {
		t.Errorf("token limit row = %+v, want used 90/remaining 10/would_exceed true", out.TokenLimits)
	}
}

// --- Per-window total request counts (Area 11 row 724) ---

// TestTotalRequestsInSlices proves the counting definition
// against real ledger rows: 3 seeded rows → 3 total requests;
// a key with no rows → 0. The me/status field itself is wired
// by the coordinator's two-line keystatus.go patch (see
// quotas.go); TestMeStatusTotalRequestsFixture below asserts
// the field end to end and stays red until that patch lands.
func TestTotalRequestsInSlices(t *testing.T) {
	s, _ := keyStatusSetup(t)
	seedModelSpend(t, s, "cheap-model", 1_000_000, 1_000_000, 10.0/1e6, 30.0/1e6)
	seedModelSpend(t, s, "unknown-price-model", 100, 100, 10.0/1e6, 30.0/1e6)
	seedModelSpend(t, s, "unpriced-model", 50, 50, 0, 0)
	now := time.Now()
	slices, err := s.led.UsageSlices(context.Background(), s.id, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := totalRequestsInSlices(slices); got != 3 {
		t.Errorf("totalRequestsInSlices = %d, want 3 (one per seeded ledger row)", got)
	}
	if got := totalRequestsInSlices(nil); got != 0 {
		t.Errorf("totalRequestsInSlices(nil) = %d, want 0", got)
	}
	// A failed attempt (ok=0) is NOT in the total: the count is
	// the population the window's spend/tokens describe.
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "cheap-model",
		PromptTokens: 10, CompletionTokens: 10, SplitKnown: true, OK: false,
	}, nil); err != nil {
		t.Fatalf("seed failed row: %v", err)
	}
	slices, err = s.led.UsageSlices(context.Background(), s.id, now.Add(-24*time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := totalRequestsInSlices(slices); got != 3 {
		t.Errorf("totalRequestsInSlices after a failed row = %d, want 3 (failed attempts excluded)", got)
	}
}

// TestMeStatusTotalRequestsFixture asserts the per-window
// total_requests field end to end via the apitest fixture. It is
// EXPECTED TO FAIL until the coordinator applies the two-line
// keystatus.go patch documented in quotas.go (TotalRequests
// field on meStatusUsageWindow + `w.TotalRequests += sl.Rows`
// in the usageWindow loop) — the fixture is the toggle proof
// for that patch: it fails on the missing field before, and
// passes after (verified in a scratch copy with exactly that
// patch applied).
func TestMeStatusTotalRequestsFixture(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedModelSpend(t, s, "cheap-model", 1_000_000, 1_000_000, 10.0/1e6, 30.0/1e6)
	seedModelSpend(t, s, "unknown-price-model", 100, 100, 10.0/1e6, 30.0/1e6)
	seedModelSpend(t, s, "unpriced-model", 50, 50, 0, 0)
	f := apitest.LoadFixture(t, "m8.11", "me-status-total-requests-200")
	f.Request.Headers["Authorization"] = "Bearer " + s.raw
	apitest.Run(t, quotasMux(srv), f)
}
