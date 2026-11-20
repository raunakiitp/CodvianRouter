package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M5.7: GET/POST /api/usage/budget + GET /api/usage/om-usage.

func usageBudgetSetup(t *testing.T) (*keyBudgetSetup, *Server) {
	t.Helper()
	s, cfgPath := keyStatusSetup(t)
	// The budget API needs the admin scope.
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	srv := s.server(t, s.persistOpt(cfgPath))
	return s, srv
}

func TestUsageBudgetGet(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	s.seedPricedSpend(t, 1000, 1000) // $0.04 today
	// A daily $1 cap at 09:30 reset: active window daily.
	if err := s.led.SetKeyBudgetConfig(context.Background(), keyBudgetForTest(s.id, 1.0)); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, "GET", "/api/usage/budget?apiKeyId="+s.id, "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var sum usageBudgetSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sum.APIKeyID != s.id {
		t.Errorf("api_key_id = %q, want %q", sum.APIKeyID, s.id)
	}
	if sum.DailyLimitUSD == nil || *sum.DailyLimitUSD != 1.0 {
		t.Errorf("daily_limit_usd = %v, want 1.0", sum.DailyLimitUSD)
	}
	if sum.WarningThreshold != 0.8 || sum.ResetInterval != "daily" || sum.ResetTime != "09:30" {
		t.Errorf("threshold/interval/time = %v/%q/%q, want 0.8/daily/09:30", sum.WarningThreshold, sum.ResetInterval, sum.ResetTime)
	}
	if sum.TotalCostToday != 0.04 || sum.TotalCostMonth != 0.04 || sum.TotalCostPeriod != 0.04 {
		t.Errorf("totals today/month/period = %v/%v/%v, want 0.04/0.04/0.04", sum.TotalCostToday, sum.TotalCostMonth, sum.TotalCostPeriod)
	}
	if sum.ActiveLimitUSD == nil || *sum.ActiveLimitUSD != 1.0 {
		t.Errorf("active_limit_usd = %v, want 1.0", sum.ActiveLimitUSD)
	}
	if sum.BudgetCheck.Status != "allow" {
		t.Errorf("budget_check = %+v, want allow", sum.BudgetCheck)
	}
	if sum.BudgetResetAt.IsZero() || sum.NextResetAt.IsZero() || sum.PeriodStartAt.IsZero() {
		t.Error("reset timestamps must all be set")
	}
	if !sum.PeriodStartAt.Equal(sum.BudgetResetAt) || !sum.PeriodStartAt.Equal(sum.LastBudgetResetAt) {
		t.Error("period_start_at, budget_reset_at and last_budget_reset_at must agree")
	}
	// The 5h rolling cap is reported alongside.
	if sum.SpentUSD != 0.04 || sum.AtLimit {
		t.Errorf("5h spent/at_limit = %v/%v, want 0.04/false", sum.SpentUSD, sum.AtLimit)
	}
}

func TestUsageBudgetGetErrors(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	for _, tc := range []struct {
		path, key string
		want      int
	}{
		{"/api/usage/budget", s.raw, http.StatusBadRequest},                 // no apiKeyId
		{"/api/usage/budget?apiKeyId=nope", s.raw, http.StatusNotFound},     // unknown key
		{"/api/usage/budget?apiKeyId=" + s.id, "", http.StatusUnauthorized}, // no key at all
	} {
		rec := doReq(t, srv, "GET", tc.path, "", tc.key)
		if rec.Code != tc.want {
			t.Errorf("GET %s: status %d, want %d: %s", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	// A chat-only key cannot read another key's budget.
	s2, _ := keyStatusSetup(t)
	srv2 := s2.server(t)
	rec := doReq(t, srv2, "GET", "/api/usage/budget?apiKeyId="+s2.id, "", s2.raw)
	if rec.Code != http.StatusForbidden {
		t.Errorf("chat-only key GET budget: status %d, want 403", rec.Code)
	}
}

func TestUsageBudgetPost(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	s.seedPricedSpend(t, 1000, 1000)
	// A fresh key has no budget row: the 5h cap must be named
	// (a row with limit 0 would block all spend), so a POST
	// without limitUsd is a 400.
	rec := doReq(t, srv, "POST", "/api/usage/budget", `{"apiKeyId":"`+s.id+`","dailyLimitUsd":2.5}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("fresh-key POST without limitUsd: status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/api/usage/budget", `{"apiKeyId":"`+s.id+`","limitUsd":100,"dailyLimitUsd":2.5,"weeklyLimitUsd":10,"warningThreshold":0.9,"resetInterval":"weekly","resetTime":"06:00"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var sum usageBudgetSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sum.DailyLimitUSD == nil || *sum.DailyLimitUSD != 2.5 || sum.WeeklyLimitUSD == nil || *sum.WeeklyLimitUSD != 10 {
		t.Errorf("limits daily/weekly = %v/%v, want 2.5/10", sum.DailyLimitUSD, sum.WeeklyLimitUSD)
	}
	if sum.MonthlyLimitUSD != nil {
		t.Errorf("monthly_limit_usd = %v, want absent (never set)", sum.MonthlyLimitUSD)
	}
	if sum.WarningThreshold != 0.9 || sum.ResetInterval != "weekly" || sum.ResetTime != "06:00" {
		t.Errorf("threshold/interval/time = %v/%q/%q, want 0.9/weekly/06:00", sum.WarningThreshold, sum.ResetInterval, sum.ResetTime)
	}
	if sum.ActiveLimitUSD == nil || *sum.ActiveLimitUSD != 10 {
		t.Errorf("active_limit_usd = %v, want 10 (weekly window active)", sum.ActiveLimitUSD)
	}
	// A second POST merges: only the monthly limit changes.
	rec = doReq(t, srv, "POST", "/api/usage/budget", `{"apiKeyId":"`+s.id+`","monthlyLimitUsd":50}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if sum.DailyLimitUSD == nil || *sum.DailyLimitUSD != 2.5 || sum.MonthlyLimitUSD == nil || *sum.MonthlyLimitUSD != 50 {
		t.Errorf("after merge daily/monthly = %v/%v, want 2.5/50", sum.DailyLimitUSD, sum.MonthlyLimitUSD)
	}
	// The config file kept the same merged row (persist path).
	if got := s.cfg.KeyBudgets[s.id]; got.MonthlyLimitUSD == nil || *got.MonthlyLimitUSD != 50 {
		t.Errorf("config monthly limit = %v, want 50 (persisted)", got.MonthlyLimitUSD)
	}
}

func TestUsageBudgetPostValidation(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	for _, body := range []string{
		`{"apiKeyId":"` + s.id + `","dailyLimitUsd":-1}`,
		`{"apiKeyId":"` + s.id + `","warningThreshold":1.5}`,
		`{"apiKeyId":"` + s.id + `","resetInterval":"yearly"}`,
		`{"apiKeyId":"` + s.id + `","resetTime":"25:00"}`,
		`{"dailyLimitUsd":1}`,
		`{"apiKeyId":"nope","dailyLimitUsd":1}`,
		`not json`,
	} {
		rec := doReq(t, srv, "POST", "/api/usage/budget", body, s.raw)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("POST %s: status %d, want 400 (or 404 for the unknown key)", body, rec.Code)
		}
	}
}

func TestUsageBudgetPostZeroLimit(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// B2: a zero D/W/M limit is a clean 400 ("must be positive")
	// at the handler, not a 500 from the ledger — matching
	// POST /dashboard/key-budgets (TestKeyBudgetDWMValidation).
	for _, tc := range []struct {
		name, body string
	}{
		{"zero daily", `{"apiKeyId":"` + s.id + `","limitUsd":100,"dailyLimitUsd":0}`},
		{"zero weekly", `{"apiKeyId":"` + s.id + `","limitUsd":100,"weeklyLimitUsd":0}`},
		{"zero monthly", `{"apiKeyId":"` + s.id + `","limitUsd":100,"monthlyLimitUsd":0}`},
		{"negative daily", `{"apiKeyId":"` + s.id + `","limitUsd":100,"dailyLimitUsd":-1}`},
		{"negative weekly", `{"apiKeyId":"` + s.id + `","limitUsd":100,"weeklyLimitUsd":-1}`},
		{"negative monthly", `{"apiKeyId":"` + s.id + `","limitUsd":100,"monthlyLimitUsd":-1}`},
	} {
		rec := doReq(t, srv, "POST", "/api/usage/budget", tc.body, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "must be positive") {
			t.Errorf("%s: body = %s, want it to say \"must be positive\"", tc.name, rec.Body.String())
		}
	}
	// Two or more bad D/W/M windows: the 400 always names the
	// first bad window in daily, weekly, monthly order — never
	// whichever key a map yields first.
	for _, tc := range []struct {
		name, body, want string
	}{
		{"daily+weekly bad", `{"apiKeyId":"` + s.id + `","limitUsd":100,"dailyLimitUsd":0,"weeklyLimitUsd":-1}`, "dailyLimitUsd must be positive"},
		{"weekly+monthly bad", `{"apiKeyId":"` + s.id + `","limitUsd":100,"weeklyLimitUsd":0,"monthlyLimitUsd":-1}`, "weeklyLimitUsd must be positive"},
		{"all three bad", `{"apiKeyId":"` + s.id + `","limitUsd":100,"dailyLimitUsd":-1,"weeklyLimitUsd":0,"monthlyLimitUsd":0}`, "dailyLimitUsd must be positive"},
	} {
		rec := doReq(t, srv, "POST", "/api/usage/budget", tc.body, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: body = %s, want it to say %q", tc.name, rec.Body.String(), tc.want)
		}
	}
	// No budget row was persisted by any rejected update.
	if _, err := s.led.GetKeyBudget(context.Background(), s.id); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetKeyBudget after rejected updates: err = %v, want sql.ErrNoRows (no budget persisted)", err)
	}
	// But a zero 5h cap itself is legitimate: limitUsd: 0 means
	// $0 (any spend is over it), so the POST succeeds and the
	// row persists with a 0 limit — unlike a zero D/W/M limit.
	rec := doReq(t, srv, "POST", "/api/usage/budget", `{"apiKeyId":"`+s.id+`","limitUsd":0}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST limitUsd 0: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	b, err := s.led.GetKeyBudget(context.Background(), s.id)
	if err != nil {
		t.Fatalf("GetKeyBudget after limitUsd 0 POST: err = %v", err)
	}
	if b.LimitUSD != 0 {
		t.Errorf("persisted limit_usd = %v, want 0", b.LimitUSD)
	}
}

func TestUsageBudgetWarnAndDeny(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// $0.044 spent of a $0.05 daily cap at threshold 0.8 (88%):
	// past the warning line, under the cap.
	if err := s.led.SetKeyBudgetConfig(context.Background(), keyBudgetForTest(s.id, 0.05)); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1100, 1100)
	rec := doReq(t, srv, "GET", "/api/usage/budget?apiKeyId="+s.id, "", s.raw)
	var sum usageBudgetSummary
	_ = json.Unmarshal(rec.Body.Bytes(), &sum)
	if sum.BudgetCheck.Status != "warn" {
		t.Errorf("at 88%% of cap: budget_check = %+v, want warn", sum.BudgetCheck)
	}
	// One more $0.044 row: $0.088 > $0.05 -> deny.
	s.seedPricedSpend(t, 1100, 1100)
	rec = doReq(t, srv, "GET", "/api/usage/budget?apiKeyId="+s.id, "", s.raw)
	_ = json.Unmarshal(rec.Body.Bytes(), &sum)
	if sum.BudgetCheck.Status != "deny" {
		t.Errorf("over cap: budget_check = %+v, want deny", sum.BudgetCheck)
	}
}

func TestOmUsage(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	s.seedPricedSpend(t, 1000, 1000) // $0.04
	// Disabled by default: 403 in both shapes.
	rec := doReq(t, srv, "GET", "/api/usage/om-usage", "", s.raw)
	if rec.Code != http.StatusForbidden {
		t.Errorf("om-usage without the flag: status %d, want 403: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/api/usage/om-usage?format=json", "", s.raw)
	if rec.Code != http.StatusForbidden {
		t.Errorf("om-usage json without the flag: status %d, want 403", rec.Code)
	}
	var denied map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &denied); err != nil || denied["allowed"] != false {
		t.Errorf("om-usage json denial = %s, want {allowed:false,...}", rec.Body.String())
	}
	// No key at all: 401.
	rec = doReq(t, srv, "GET", "/api/usage/om-usage", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("om-usage without a key: status %d, want 401", rec.Code)
	}
	// Enable the flag via the key-update endpoint (admin scope).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"allow_usage_command":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable flag: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Plain text (terminal) shape.
	rec = doReq(t, srv, "GET", "/api/usage/om-usage", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("om-usage text: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "today: $0.0400") || !strings.Contains(body, "month: $0.0400") || !strings.Contains(body, "next reset:") {
		t.Errorf("om-usage text = %q, want today/month/next-reset lines", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Errorf("om-usage content-type = %q, want text/plain", ct)
	}
	// JSON shape.
	rec = doReq(t, srv, "GET", "/api/usage/om-usage?format=json", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("om-usage json: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out omUsageJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode om-usage json: %v", err)
	}
	if !out.Allowed || out.KeyID != s.id || out.Today != 0.04 || out.Month != 0.04 {
		t.Errorf("om-usage json = %+v, want allowed/today 0.04/month 0.04", out)
	}
	// Disabling again restores the 403.
	rec = doReq(t, srv, "POST", "/admin/keys/"+s.id, `{"allow_usage_command":false}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable flag: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/api/usage/om-usage", "", s.raw)
	if rec.Code != http.StatusForbidden {
		t.Errorf("om-usage after disable: status %d, want 403", rec.Code)
	}
}

func keyBudgetForTest(keyID string, daily float64) ledger.KeyBudget {
	return ledger.KeyBudget{KeyID: keyID, LimitUSD: 1000, DailyLimitUSD: &daily, WarningThreshold: 0.8, ResetInterval: ledger.BudgetResetDaily, ResetTime: "09:30"}
}
