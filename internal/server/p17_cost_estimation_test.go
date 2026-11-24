// P1.7 — pre-dispatch cost estimation and reconciliation.
//
// These tests pin the two rules the block exists for: an estimate never
// calls a provider and never invents a price, and a reconciliation is
// only "reconciled" when BOTH sides are priced — token usage alone is not
// enough.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func p17usd(v float64) *float64 { return &v }

// p17Server wires the fixture server with a price book that carries
// input, output, cached-input and both cache-write tiers for
// "priced-model".
func p17Server(t *testing.T) (*keyBudgetSetup, *Server, string, string) {
	t.Helper()
	s := newKeyBudgetSetup(t)
	s.book.Entries["priced-model"] = config.ModelPrice{
		InputUSDPer1M:        p17usd(10),
		OutputUSDPer1M:       p17usd(20),
		CachedInputUSDPer1M:  p17usd(1),
		CacheWrite5mUSDPer1M: p17usd(12.5),
		CacheWrite1hUSDPer1M: p17usd(15),
	}
	// The cost endpoints are admin-scoped management routes; issue the
	// admin key before the server is built so the verifier sees it.
	adminKey, adminRaw := issueScopedKey(t, s.cfg, "p17-admin", []string{"admin", "chat"})
	return s, s.server(t), adminRaw, adminKey.ID
}

func TestP17EstimateKnownPrice(t *testing.T) {
	_, srv, admin, _ := p17Server(t)
	body := `{"model":"priced-model","messages":[{"role":"user","content":"hello world"}],"output_tokens":100}`
	rec := doReq(t, srv, http.MethodPost, "/api/cost/estimate", body, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out CostEstimateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.CostKnown {
		t.Fatal("cost_known = false for a booked price")
	}
	if out.PriceSource != "model" {
		t.Fatalf("price_source = %q, want model", out.PriceSource)
	}
	if out.EstimatedCompletionTokens != 100 || out.CompletionTokensSource != "requested" {
		t.Fatalf("completion = %d/%s, want 100/requested", out.EstimatedCompletionTokens, out.CompletionTokensSource)
	}
	if out.PromptTokens <= 0 {
		t.Fatalf("prompt_tokens = %d, want > 0", out.PromptTokens)
	}
	if out.PromptTokensSource == "" {
		t.Fatal("prompt_tokens_source must be reported")
	}
	want := float64(out.PromptTokens)*10.0/1e6 + 100*20.0/1e6
	if diff := out.EstimatedCostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Fatalf("estimated = %v, want %v", out.EstimatedCostUSD, want)
	}
	if out.Breakdown == nil || out.Breakdown.CachedInputPer1M == nil || *out.Breakdown.CachedInputPer1M != 1 {
		t.Fatal("cached-input RATE must be reported when the book has one")
	}
	if !out.CachedShareUnknown {
		t.Fatal("cached_share_unknown must be true pre-dispatch: the cached share cannot be known")
	}
	if out.Breakdown.CachedInputCostUSD != 0 {
		t.Fatalf("cached_input_cost = %v, want 0 — no cached share may be invented", out.Breakdown.CachedInputCostUSD)
	}
	if !strings.Contains(out.EstimateNote, "not an invoice") {
		t.Fatalf("estimate_note = %q, want the estimate-is-not-an-invoice wording", out.EstimateNote)
	}
}

func TestP17EstimateUnknownPriceIsNotZeroCost(t *testing.T) {
	_, srv, admin, _ := p17Server(t)
	body := `{"model":"no-such-priced-model","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`
	rec := doReq(t, srv, http.MethodPost, "/api/cost/estimate", body, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out CostEstimateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.CostKnown {
		t.Fatal("cost_known = true with no price")
	}
	if out.EstimatedCostUSD != 0 {
		t.Fatalf("estimated = %v, want 0 with unknown price", out.EstimatedCostUSD)
	}
	if out.PriceSource != "unknown" {
		t.Fatalf("price_source = %q, want unknown", out.PriceSource)
	}
	if out.Warning == "" {
		t.Fatal("an unknown price must carry a warning")
	}
	if out.CompletionTokensSource != "max_tokens" {
		t.Fatalf("completion source = %q, want max_tokens", out.CompletionTokensSource)
	}
}

func TestP17EstimateCountsTools(t *testing.T) {
	_, srv, admin, _ := p17Server(t)
	base := `{"model":"priced-model","provider":"prov-a","wire":"openai","messages":[{"role":"user","content":"hi"}]}`
	withTools := `{"model":"priced-model","provider":"prov-a","wire":"openai","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Look up the weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`

	decode := func(body string) CostEstimateResponse {
		rec := doReq(t, srv, http.MethodPost, "/api/cost/estimate", body, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out CostEstimateResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	without := decode(base)
	with := decode(withTools)
	if with.PromptTokens <= without.PromptTokens {
		t.Fatalf("prompt tokens with tools = %d, without = %d — tool definitions must be counted",
			with.PromptTokens, without.PromptTokens)
	}
	if with.TokenCountWire != "openai" {
		t.Fatalf("token_count_wire = %q, want openai", with.TokenCountWire)
	}
	if with.PromptTokensSource != "tiktoken_wire" {
		t.Fatalf("prompt_tokens_source = %q, want tiktoken_wire when a wire is supplied", with.PromptTokensSource)
	}
}

func TestP17EstimateBudgetCheck(t *testing.T) {
	s, srv, admin, adminID := p17Server(t)
	// A cap far above the estimate: within budget, with the remaining
	// figure reported.
	if err := s.led.SetKeyBudget(context.Background(), adminID, 100, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"priced-model","budget_check":true,"messages":[{"role":"user","content":"hi"}],"output_tokens":10}`
	rec := doReq(t, srv, http.MethodPost, "/api/cost/estimate", body, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var out CostEstimateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (status=%d body=%q)", err, rec.Code, rec.Body.String())
	}
	if out.Budget == nil {
		t.Fatal("budget_check requested but absent")
	}
	if out.Budget.KeyID != adminID {
		t.Fatalf("budget key_id = %q, want the authenticated key %q", out.Budget.KeyID, adminID)
	}
	if !out.Budget.WithinBudget {
		t.Fatalf("within_budget = false under a $100 cap: %+v", out.Budget)
	}
	if out.Budget.RemainingUSD <= 0 {
		t.Fatalf("remaining = %v, want > 0", out.Budget.RemainingUSD)
	}

	// Spend the cap out: the same estimate now reports over-budget, and
	// the endpoint still answers 200 (it never refuses an estimate).
	// Spend the admin key out: the same estimate now reports over-budget.
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: adminID, Provider: "p", Model: "priced-model",
		PromptTokens: 2000, CompletionTokens: 2000, SplitKnown: true, OK: true,
	}, ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 20.0 / 1e6}}); err != nil {
		t.Fatal(err)
	}
	if err := s.led.SetKeyBudget(context.Background(), adminID, 0.001, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv, http.MethodPost, "/api/cost/estimate", body, admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when over budget: %s", rec.Code, rec.Body.String())
	}
	out = CostEstimateResponse{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Budget == nil || out.Budget.WithinBudget {
		t.Fatalf("budget = %+v, want within_budget=false over a spent cap", out.Budget)
	}
}

func TestP17ReconcileStatuses(t *testing.T) {
	_, srv, admin, _ := p17Server(t)
	post := func(body string) CostReconciliation {
		rec := doReq(t, srv, http.MethodPost, "/api/cost/reconcile", body, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		var out CostReconciliation
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// usage_missing: no provider usage at all.
	if got := post(`{"model":"priced-model","estimated_cost_usd":0.01,"estimate_known":true,"usage":{},"split_known":true}`); got.Status != "usage_missing" {
		t.Fatalf("status = %q, want usage_missing", got.Status)
	}
	// actual_unknown: usage without a reported split.
	if got := post(`{"model":"priced-model","estimated_cost_usd":0.01,"estimate_known":true,"usage":{"prompt_tokens":100,"completion_tokens":10},"split_known":false}`); got.Status != "actual_unknown" {
		t.Fatalf("status = %q, want actual_unknown (split unknown)", got.Status)
	}
	// actual_unknown: no price for the model.
	if got := post(`{"model":"unpriced-x","estimated_cost_usd":0.01,"estimate_known":true,"usage":{"prompt_tokens":100,"completion_tokens":10},"split_known":true}`); got.Status != "actual_unknown" {
		t.Fatalf("status = %q, want actual_unknown (no price)", got.Status)
	}
	// estimate_unknown: priced usage, unpriced estimate — NOT reconciled.
	if got := post(`{"model":"priced-model","estimated_cost_usd":0,"estimate_known":false,"usage":{"prompt_tokens":100,"completion_tokens":10},"split_known":true}`); got.Status != "estimate_unknown" {
		t.Fatalf("status = %q, want estimate_unknown (usage alone is not reconciliation)", got.Status)
	}

	// reconciled: both sides known, with the cached leg priced at the
	// cached rate. 600 uncached @ $10/1M + 400 cached @ $1/1M + 100 out
	// @ $20/1M = 0.006 + 0.0004 + 0.002 = 0.0084.
	got := post(`{"model":"priced-model","estimated_cost_usd":0.01,"estimate_known":true,
		"usage":{"prompt_tokens":1000,"completion_tokens":100,"cached_tokens":400},"split_known":true}`)
	if got.Status != "reconciled" {
		t.Fatalf("status = %q, want reconciled: %+v", got.Status, got)
	}
	if diff := got.ActualCostUSD - 0.0084; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("actual = %v, want 0.0084", got.ActualCostUSD)
	}
	if diff := got.VarianceUSD - (0.0084 - 0.01); diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("variance = %v, want %v", got.VarianceUSD, 0.0084-0.01)
	}
	if got.VariancePct >= 0 {
		t.Fatalf("variance_pct = %v, want negative (actual below estimate)", got.VariancePct)
	}
}

func TestP17ReconcileCacheWriteTiers(t *testing.T) {
	_, srv, _, _ := p17Server(t)
	// The cache-write tier counters are router-internal (json:"-"), so
	// this drives the reconciliation directly rather than over JSON.
	// 1000 prompt, 500 writes at the 5m tier ($12.5/1M), 500 at the 1h
	// tier ($15/1M), 100 completion ($20/1M):
	// 0.01 + 0.00625 + 0.0075 + 0.002 = 0.02575
	got := srv.reconcile(CostReconcileRequest{
		Model:            "priced-model",
		EstimatedCostUSD: 0.02,
		EstimateKnown:    true,
		SplitKnown:       true,
		Usage: provider.Usage{
			PromptTokens:       1000,
			CompletionTokens:   100,
			CacheWriteTokens:   1000,
			CacheWriteTokens5m: 500,
			CacheWriteTokens1h: 500,
		},
	})
	if got.Status != "reconciled" {
		t.Fatalf("status = %q: %+v", got.Status, got)
	}
	if diff := got.ActualCostUSD - 0.02575; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("actual = %v, want 0.02575 (tier rates applied)", got.ActualCostUSD)
	}
}

func TestP17CallerKeyIDFromContext(t *testing.T) {
	s, srv, _, _ := p17Server(t)
	// A request served by this server carries the lazy caller-key holder;
	// resolving it inside the handler yields the authenticated key.
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = srv.callerKeyIDFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
	wrapped := srv.withLogging(inner)
	req := httptest.NewRequest(http.MethodPost, "/probe", nil)
	req.Header.Set("Authorization", "Bearer "+s.raw)
	wrapped.ServeHTTP(httptest.NewRecorder(), req)
	if got != s.id {
		t.Fatalf("callerKeyIDFromContext = %q, want %q", got, s.id)
	}
	// A bare context carries no identity — never a guess.
	if id := srv.callerKeyIDFromContext(context.Background()); id != "" {
		t.Fatalf("bare context returned %q, want empty", id)
	}
}
