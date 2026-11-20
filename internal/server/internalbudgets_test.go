package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// M5.11: internal scoped budgets, server side — the pre-call
// gate on the chat path, the engine-error mapping, and the
// dashboard GET/POST feeds.
//
// seedPricedSpend(100000, 100000) records exactly $4.00 of
// spend (100k in × $10/1M + 100k out × $30/1M); the tests use
// integer dollars so the warning threshold is exact.
func TestInternalBudgetGateDenyBlocksChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	if err := s.led.SetInternalBudget(context.Background(), ledger.InternalBudget{
		ID: "g1", Scope: ledger.InternalBudgetScopeGlobal, Period: "daily",
		LimitType: ledger.InternalBudgetLimitCurrency, LimitValue: 1, WarningThreshold: 0.8, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 100000, 100000) // $4.00 of $1.00
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("chat status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_budget") {
		t.Errorf("402 body = %s, want error type internal_budget", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the budget must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestInternalBudgetGateWarnServesWithHeader(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	if err := s.led.SetInternalBudget(context.Background(), ledger.InternalBudget{
		ID: "g1", Scope: ledger.InternalBudgetScopeGlobal, Period: "daily",
		LimitType: ledger.InternalBudgetLimitCurrency, LimitValue: 5, WarningThreshold: 0.8, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 100000, 100000) // $4.00 of $5.00 = 80%
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (warn allows): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Budget-Warning"); got != "g1" {
		t.Errorf("X-Codvian-Budget-Warning = %q, want g1", got)
	}
}

func TestInternalBudgetGateModelScope(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	if err := s.led.SetInternalBudget(context.Background(), ledger.InternalBudget{
		ID: "m1", Scope: ledger.InternalBudgetScopeModel, ModelID: "priced-model", Period: "daily",
		LimitType: ledger.InternalBudgetLimitRequests, LimitValue: 1, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1, 1) // 1 request on priced-model
	// The priced model is at its cap → 402.
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("priced-model chat status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	// Any other model is untouched by this budget → 200.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("unpriced-model chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestInternalBudgetGatePoolScope(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool1", Name: "team",
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: 10000}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 100, Policy: "hard"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.led.SetInternalBudget(context.Background(), ledger.InternalBudget{
		ID: "pool1-b", Scope: ledger.InternalBudgetScopePool, PoolID: "pool1", Period: "daily",
		LimitType: ledger.InternalBudgetLimitCurrency, LimitValue: 1, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 100000, 100000) // $4.00 of $1.00, pool-wide
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("chat status = %d, want 402 (pool member): %s", rec.Code, rec.Body.String())
	}
	// Leaving the pool lifts the cap.
	if err := s.led.DeleteQuotaPool(context.Background(), "pool1"); err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (no longer a pool member): %s", rec.Code, rec.Body.String())
	}
}

func TestInternalBudgetEngineBreachMapsTo402(t *testing.T) {
	s, _ := keyStatusSetup(t)
	// The fake engine returns the breach the real engine would
	// produce when every target is over its provider budget.
	eng := &fakeEngine{chatErr: &router.InternalBudgetBreach{BudgetID: "p1", Provider: "openai", LimitType: "currency", Limit: 3, Used: 4, Remaining: 0, Period: "daily"}}
	srv := newTestServer(t, eng, WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book))
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("chat status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "internal_budget") {
		t.Errorf("402 body = %s, want error type internal_budget", rec.Body.String())
	}
}

func TestInternalBudgetsDashboardRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	// The internal-budgets POST is admin-scoped: widen this
	// test's own fixture key to chat+admin (same pattern as the
	// pool tests).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	s.seedPricedSpend(t, 100000, 100000) // $4.00 today
	// Create a budget via POST.
	create := `{"id":"g1","scope":"global","period":"daily","limit_type":"currency","limit_value":5,"warning_threshold":0.8,"enabled":true}`
	rec := doReq(t, srv, "POST", "/dashboard/internal-budgets", create, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// GET lists it with live usage ($4 of $5 → warn).
	rec = doReq(t, srv, "GET", "/dashboard/internal-budgets", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out internalBudgetsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Budgets) != 1 || out.Budgets[0].ID != "g1" {
		t.Fatalf("GET budgets = %+v, want 1 budget g1", out.Budgets)
	}
	got := out.Budgets[0]
	if got.Used != 4 || got.Remaining != 1 || got.Decision != "warn" {
		t.Errorf("budget = %+v, want used 4 / remaining 1 / warn", got)
	}
	// Bad bodies are rejected, never stored.
	for _, bad := range []string{
		`{"id":"b2","scope":"key","period":"daily","limit_type":"currency","limit_value":5}`,
		`{"id":"b2","scope":"global","period":"hourly","limit_type":"currency","limit_value":5}`,
		`{"id":"b2","scope":"global","period":"daily","limit_type":"usd","limit_value":5}`,
		`{"id":"b2","scope":"global","period":"daily","limit_type":"currency","limit_value":0}`,
		`{"id":"b2","scope":"provider","period":"daily","limit_type":"currency","limit_value":5}`,
		`{"id":"b2","scope":"pool","pool_id":"nope","period":"daily","limit_type":"currency","limit_value":5}`,
	} {
		rec := doReq(t, srv, "POST", "/dashboard/internal-budgets", bad, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad budget POST status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	}
	// Delete removes it.
	rec = doReq(t, srv, "POST", "/dashboard/internal-budgets", `{"id":"g1","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/internal-budgets", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Budgets) != 0 {
		t.Errorf("GET after delete = %d budgets, want 0", len(out.Budgets))
	}
}
