package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// B12: burn-rate forecast tests for GET /api/usage/budget/forecast.
// Each case asserts an honest status — projected only when real ledger
// spend and a real budget limit exist; otherwise insufficient_data.

func TestUsageBudgetForecastProjected(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// $0.40 of spend recorded now (within the last hour).
	s.seedPricedSpend(t, 10000, 10000)
	if err := s.led.SetKeyBudgetConfig(t.Context(), keyBudgetForTest(s.id, 1.0)); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, http.MethodGet, "/api/usage/budget/forecast?apiKeyId="+s.id, "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var fc usageBudgetForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}
	if fc.Status != "projected" {
		t.Errorf("status = %q, want projected (note=%q)", fc.Status, fc.Note)
	}
	if fc.ActiveLimitUSD == nil || *fc.ActiveLimitUSD != 1.0 {
		t.Errorf("active_limit_usd = %v, want 1.0", fc.ActiveLimitUSD)
	}
	if fc.RemainingUSD == nil || *fc.RemainingUSD < 0.59 || *fc.RemainingUSD > 0.61 {
		t.Errorf("remaining_usd = %v, want ~0.60", fc.RemainingUSD)
	}
	if fc.BurnRate1hUSDh <= 0 {
		t.Errorf("burn_rate_1h = %v, want > 0 (real spend exists)", fc.BurnRate1hUSDh)
	}
	if fc.Requests1h < 1 {
		t.Errorf("requests_1h = %d, want >= 1", fc.Requests1h)
	}
	if fc.ProjectedInHours == nil || *fc.ProjectedInHours <= 0 {
		t.Errorf("projected_in_hours = %v, want > 0", fc.ProjectedInHours)
	}
	if fc.ProjectedExhaustion == nil || !fc.ProjectedExhaustion.After(time.Now()) {
		t.Errorf("projected_exhaustion = %v, want a future time", fc.ProjectedExhaustion)
	}
}

func TestUsageBudgetForecastNoSpendIsInsufficientData(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// Budget set but zero spend recorded.
	if err := s.led.SetKeyBudgetConfig(t.Context(), keyBudgetForTest(s.id, 1.0)); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, http.MethodGet, "/api/usage/budget/forecast?apiKeyId="+s.id, "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var fc usageBudgetForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fc.Status != "insufficient_data" {
		t.Errorf("status = %q, want insufficient_data (no recent spend to project from)", fc.Status)
	}
	if fc.ProjectedExhaustion != nil || fc.ProjectedInHours != nil {
		t.Errorf("projection fields set despite insufficient data: %v %v", fc.ProjectedExhaustion, fc.ProjectedInHours)
	}
}

func TestUsageBudgetForecastNoLimitIsInsufficientData(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	s.seedPricedSpend(t, 1000, 1000) // spend exists, but no budget limit set
	rec := doReq(t, srv, http.MethodGet, "/api/usage/budget/forecast?apiKeyId="+s.id, "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var fc usageBudgetForecastResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &fc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fc.ActiveLimitUSD != nil {
		t.Errorf("active_limit_usd = %v, want nil (no budget set)", *fc.ActiveLimitUSD)
	}
	if fc.Status != "insufficient_data" {
		t.Errorf("status = %q, want insufficient_data (no limit to project against)", fc.Status)
	}
	if fc.ProjectedExhaustion != nil {
		t.Error("projected_exhaustion set with no limit — must not fabricate")
	}
}

func TestUsageBudgetForecastValidation(t *testing.T) {
	s, srv := usageBudgetSetup(t)
	// Missing apiKeyId -> 400.
	if rec := doReq(t, srv, http.MethodGet, "/api/usage/budget/forecast", "", s.raw); rec.Code != http.StatusBadRequest {
		t.Errorf("no apiKeyId: status = %d, want 400", rec.Code)
	}
	// Unknown key -> 404.
	if rec := doReq(t, srv, http.MethodGet, "/api/usage/budget/forecast?apiKeyId=nope", "", s.raw); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key: status = %d, want 404", rec.Code)
	}
	// POST -> 405.
	if rec := doReq(t, srv, http.MethodPost, "/api/usage/budget/forecast?apiKeyId="+s.id, "", s.raw); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status = %d, want 405", rec.Code)
	}
}
