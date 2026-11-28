package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// Per-request USD budget (CHECKLIST 155-156): header parsing,
// the pre-send 402 with zero engine calls, the strict mid-chain
// 402, honest non-strict overage recording, and the live
// dashboard feed.
//
// The price book here is model-level only ($10/1M in, $30/1M
// out for gpt-4): "hello world" counts as 3 local tokens, so
// its prompt estimate is 3 × $10/1M = $0.00003.

func budgetBook() *config.PriceBook {
	book := config.NewPriceBook()
	book.Entries["gpt-4"] = config.ModelPrice{InputUSDPer1M: floatPtr(10), OutputUSDPer1M: floatPtr(30)}
	return book
}

// doBudgetReq is doReq with extra headers: the per-request
// budget headers ride on the HTTP request, not the JSON body.
func doBudgetReq(t *testing.T, srv *Server, method, path, body, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const budgetBody = `{"model":"gpt-4","messages":[{"role":"user","content":"hello world"}]}`

func getBudgetFeed(t *testing.T, srv *Server) requestBudgetFeed {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/request-budgets", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/request-budgets status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var feed requestBudgetFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("feed decode: %v (body %q)", err, rec.Body.String())
	}
	return feed
}

// Header parsing: valid values, strict spellings, the
// reference-style alias, and every invalid shape refused.
func TestParseRequestBudget(t *testing.T) {
	cases := []struct {
		name       string
		headers    map[string]string
		wantBudget float64
		wantStrict bool
		wantErr    bool
	}{
		{"absent", nil, 0, false, false},
		{"plain", map[string]string{"X-Codvian-Budget": "0.05"}, 0.05, false, false},
		{"strict true", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "true"}, 0.05, true, false},
		{"strict 1", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "1"}, 0.05, true, false},
		{"strict word", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "strict"}, 0.05, true, false},
		{"strict false", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "false"}, 0.05, false, false},
		{"sector baseline alias", map[string]string{"X-Codvian-Budget": "0.05", "Budget-Fallback": "strict"}, 0.05, true, false},
		{"alias ignored when header says false", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "false", "Budget-Fallback": "strict"}, 0.05, false, false},
		{"not a number", map[string]string{"X-Codvian-Budget": "abc"}, 0, false, true},
		{"zero", map[string]string{"X-Codvian-Budget": "0"}, 0, false, true},
		{"negative", map[string]string{"X-Codvian-Budget": "-1"}, 0, false, true},
		{"inf", map[string]string{"X-Codvian-Budget": "Inf"}, 0, false, true},
		{"nan", map[string]string{"X-Codvian-Budget": "NaN"}, 0, false, true},
		{"bad strict", map[string]string{"X-Codvian-Budget": "0.05", "X-Codvian-Budget-Strict": "maybe"}, 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			budget, strict, err := parseRequestBudget(req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("err = nil, want an error for headers %v", tc.headers)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantBudget == 0 {
				if budget != nil {
					t.Errorf("budget = %v, want nil (no header)", *budget)
				}
			} else if budget == nil || *budget != tc.wantBudget {
				t.Errorf("budget = %v, want %v", budget, tc.wantBudget)
			}
			if strict != tc.wantStrict {
				t.Errorf("strict = %v, want %v", strict, tc.wantStrict)
			}
		})
	}
}

// Pre-send 402, both modes: the $0.00009 prompt estimate alone
// exceeds the $0.00001 budget, so the request is refused before
// any provider is dialled — the engine is never called — and the
// refusal is recorded in the live feed.
func TestRequestBudgetPreSend402(t *testing.T) {
	for _, strict := range []bool{true, false} {
		mode := "non-strict"
		if strict {
			mode = "strict"
		}
		t.Run(mode, func(t *testing.T) {
			eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
			srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
			headers := map[string]string{"X-Codvian-Budget": "0.00001"}
			if strict {
				headers["X-Codvian-Budget-Strict"] = "true"
			}
			rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123", headers)
			if rec.Code != http.StatusPaymentRequired {
				t.Fatalf("status = %d, want 402 (body %q)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "per-request budget exceeded") || !strings.Contains(rec.Body.String(), "before any provider was called") {
				t.Errorf("402 body = %q, want the per-request budget named and the pre-send refusal", rec.Body.String())
			}
			if eng.chatCalls.Load() != 0 {
				t.Errorf("engine Chat called %d times, want 0 (refused before dialling)", eng.chatCalls.Load())
			}
			feed := getBudgetFeed(t, srv)
			if feed.RequestsWithBudget != 1 || feed.RefusedPreSend != 1 {
				t.Errorf("feed = %+v, want requests_with_budget 1 / refused_pre_send 1", feed)
			}
			if len(feed.Events) != 1 || feed.Events[0].Outcome != "refused_pre_send" || feed.Events[0].BudgetUSD != 0.00001 || feed.Events[0].Strict != strict {
				t.Errorf("events = %+v, want one refused_pre_send event for $0.00001 strict=%v", feed.Events, strict)
			}
			if feed.Events[0].EstimatedCostUSD < 0.000089999 || feed.Events[0].EstimatedCostUSD > 0.000090001 {
				t.Errorf("event estimated cost = %v, want ~0.00009", feed.Events[0].EstimatedCostUSD)
			}
		})
	}
}

// An invalid budget header is a 400, never silently ignored:
// the client must know its cap was not applied.
func TestRequestBudgetInvalidHeader400(t *testing.T) {
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
	rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123",
		map[string]string{"X-Codvian-Budget": "free"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat called %d times, want 0", eng.chatCalls.Load())
	}
}

// A budget the estimate fits proceeds normally, and the
// request carries the budget into the engine (a fake engine
// records the request it saw).
func TestRequestBudgetUnderCapProceeds(t *testing.T) {
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "gpt-4"}}}
	srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
	rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123",
		map[string]string{"X-Codvian-Budget": "1.00", "X-Codvian-Budget-Strict": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat called %d times, want 1", eng.chatCalls.Load())
	}
	feed := getBudgetFeed(t, srv)
	if feed.RequestsWithBudget != 1 || feed.RefusedPreSend != 0 || feed.Overages != 0 {
		t.Errorf("feed = %+v, want 1 budgeted request, no refusals, no overages", feed)
	}
}

// Strict mid-chain stop: the engine refuses with the typed
// budget error (the fallback it would have dialled was too
// expensive) and the server answers 402 and records a
// refused_mid_chain event — never a 502.
func TestRequestBudgetMidChain402(t *testing.T) {
	be := &router.RequestBudgetExceededError{BudgetUSD: 0.001, SpentUSD: 0, NextEstimateUSD: 0.003, Provider: "pricey-prov", Model: "gpt-4"}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatErr: be}}
	srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
	rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123",
		map[string]string{"X-Codvian-Budget": "0.001", "X-Codvian-Budget-Strict": "true"})
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402 (body %q)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "-32603") {
		t.Errorf("402 body = %q, want no upstream-failure code -32603", rec.Body.String())
	}
	feed := getBudgetFeed(t, srv)
	if feed.RefusedMidChain != 1 {
		t.Errorf("feed = %+v, want refused_mid_chain 1", feed)
	}
	if len(feed.Events) != 1 || feed.Events[0].Outcome != "refused_mid_chain" || feed.Events[0].Provider != "pricey-prov" {
		t.Errorf("events = %+v, want one refused_mid_chain event for pricey-prov", feed.Events)
	}
}

// Non-strict overage, recorded honestly: the prompt estimate
// fits the $0.01 budget, but the served response's priced
// actual cost (1000 prompt + 1000 completion at $10/$30 per
// 1M = $0.04) exceeds it. The client still gets its 200, the
// response carries X-Codvian-Budget-Spent, and the feed records
// the overage with the real actual cost.
func TestRequestBudgetNonStrictOverage(t *testing.T) {
	resp := provider.ChatResponse{Content: "ok", Model: "gpt-4",
		Usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: resp}}
	srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
	rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123",
		map[string]string{"X-Codvian-Budget": "0.01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (non-strict serves; body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Budget-Spent"); got != "$0.040000" {
		t.Errorf("X-Codvian-Budget-Spent = %q, want $0.040000", got)
	}
	feed := getBudgetFeed(t, srv)
	if feed.Overages != 1 {
		t.Errorf("feed = %+v, want overages 1", feed)
	}
	if len(feed.Events) != 1 || feed.Events[0].Outcome != "overage" {
		t.Fatalf("events = %+v, want one overage event", feed.Events)
	}
	if feed.Events[0].ActualCostUSD == nil || *feed.Events[0].ActualCostUSD != 0.04 {
		t.Errorf("event actual cost = %v, want 0.04", feed.Events[0].ActualCostUSD)
	}
	if feed.Events[0].BudgetUSD != 0.01 || feed.Events[0].Strict {
		t.Errorf("event = %+v, want budget 0.01 non-strict", feed.Events[0])
	}
}

// Under budget, no overage: the same priced response against a
// $1 budget serves with the spent header and no event.
func TestRequestBudgetUnderCapNoOverage(t *testing.T) {
	resp := provider.ChatResponse{Content: "ok", Model: "gpt-4",
		Usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000}}
	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: resp}}
	srv := newTestServer(t, eng, WithPriceBook(budgetBook()))
	rec := doBudgetReq(t, srv, http.MethodPost, "/v1/chat/completions", budgetBody, "test-key-123",
		map[string]string{"X-Codvian-Budget": "1.00"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Budget-Spent"); got != "$0.040000" {
		t.Errorf("X-Codvian-Budget-Spent = %q, want $0.040000", got)
	}
	feed := getBudgetFeed(t, srv)
	if feed.Overages != 0 || len(feed.Events) != 0 {
		t.Errorf("feed = %+v, want no overages and no events", feed)
	}
}

// The feed's honest empty state: a fresh server reports zeros
// and an empty event list, never invented counts.
func TestRequestBudgetsFeedEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	feed := getBudgetFeed(t, srv)
	if feed.RequestsWithBudget != 0 || feed.RefusedPreSend != 0 || feed.RefusedMidChain != 0 || feed.Overages != 0 {
		t.Errorf("feed = %+v, want all zeros on a fresh server", feed)
	}
	if feed.Events == nil || len(feed.Events) != 0 {
		t.Errorf("events = %v, want an empty (non-nil) list", feed.Events)
	}
	rec := doReq(t, srv, http.MethodPost, "/dashboard/request-budgets", "", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /dashboard/request-budgets status = %d, want 405", rec.Code)
	}
}

// The dashboard Usage view serves the per-request budgets card
// (CHECKLIST 156): the section + its live loader are present in
// the page the real binary serves.
func TestDashboardUsageHasRequestBudgetsCard(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Per-request budgets", "loadRequestBudgets", "/dashboard/request-budgets", "refused_pre_send"} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard page lacks %q", want)
		}
	}
}
