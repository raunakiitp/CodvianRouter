package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M5.4: fair-share quota pools, server side — the pre-call gate
// on the chat path and the dashboard GET/POST feeds.

func TestQuotaPoolGateBlocksChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	// Pool: 2 requests/day, k1 weight 50 (fair share 1), hard.
	// One prior request today puts total at 1/2 = 50% (strict)
	// with k1 at its fair share of 1 → the next chat is refused.
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool1", Name: "team",
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: 2}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 50, Policy: "hard"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1, 1) // one successful row today
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_pool_limit") {
		t.Errorf("429 body = %s, want error type key_pool_limit", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry a Retry-After header")
	}
}

func TestQuotaPoolGateSoftPenalizedHeader(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	// Same pool, soft policy: the request is served, with the
	// penalized header naming the pool.
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool1", Name: "team",
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: 2}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 50, Policy: "soft"}},
	}); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1, 1)
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (soft allows): %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-FairShare-Penalized"); got != "pool1" {
		t.Errorf("X-Codvian-FairShare-Penalized = %q, want pool1", got)
	}
}

func TestQuotaPoolGateNoPoolAllows(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello"}]}`
	rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (no pools): %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaPoolsDashboardRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	// The quota-pools POST is admin-scoped: widen this test's own
	// fixture key to chat+admin (same pattern as the key-budgets
	// POST tests).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	// Create a pool via POST.
	create := `{"id":"pool1","name":"team","saturation_threshold":0.5,` +
		`"dimensions":[{"unit":"requests","window":"daily","limit":100}],` +
		`"allocations":[{"key_id":"` + s.id + `","weight":50,"policy":"hard"}]}`
	rec := doReq(t, srv, "POST", "/dashboard/quota-pools", create, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// GET lists it with live consumption (0 so far).
	rec = doReq(t, srv, "GET", "/dashboard/quota-pools", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaPoolsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Pools) != 1 || out.Pools[0].ID != "pool1" || len(out.Pools[0].Dimensions) != 1 {
		t.Fatalf("GET pools = %+v, want 1 pool with 1 dimension", out.Pools)
	}
	if out.Pools[0].Dimensions[0].Consumed != 0 || out.Pools[0].Dimensions[0].Limit != 100 {
		t.Errorf("dimension = %+v, want consumed 0 / limit 100", out.Pools[0].Dimensions[0])
	}
	// One request through (the fake engine records no ledger row
	// itself — usage recording is the real engine's job — so seed
	// the row directly, the way the other budget tests do):
	// consumption shows up in GET.
	body := `{"model":"unpriced-model","messages":[{"role":"user","content":"hello"}]}`
	if rec := doReq(t, srv, "POST", "/v1/chat/completions", body, s.raw); rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	s.seedPricedSpend(t, 1, 1)
	rec = doReq(t, srv, "GET", "/dashboard/quota-pools", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Pools[0].Dimensions[0].Consumed != 1 {
		t.Errorf("dimension consumed = %v, want 1 after one chat", out.Pools[0].Dimensions[0].Consumed)
	}
	// Bad bodies are rejected, never stored.
	for _, bad := range []string{
		`{"id":"pool2","name":"x","dimensions":[{"unit":"percent","window":"daily","limit":100}],"allocations":[]}`,
		`{"id":"pool2","name":"x","dimensions":[{"unit":"requests","window":"daily","limit":0}],"allocations":[]}`,
		`{"id":"pool2","name":"x","dimensions":[{"unit":"requests","window":"daily","limit":100}],"allocations":[{"key_id":"ghost","weight":50,"policy":"hard"}]}`,
	} {
		rec := doReq(t, srv, "POST", "/dashboard/quota-pools", bad, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad pool POST status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	}
	// Delete removes it.
	rec = doReq(t, srv, "POST", "/dashboard/quota-pools", `{"id":"pool1","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/quota-pools", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Pools) != 0 {
		t.Errorf("GET after delete = %d pools, want 0", len(out.Pools))
	}
}

// M5.5: account quotas in me/status — per-account limit × backing
// accounts, own + pool-wide consumption, fair share.
func TestMeStatusAccountQuotas(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	// Pool: 100 requests/day per account × 3 accounts = 300
	// effective; k1 weight 50 → fair share 150.
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool1", Name: "team", AccountCount: 3,
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: 100}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 50, Policy: "hard"}},
	}); err != nil {
		t.Fatal(err)
	}
	// 30 requests by this key today.
	for i := 0; i < 30; i++ {
		s.seedPricedSpend(t, 1, 1)
	}
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v", err)
	}
	if len(st.AccountQuotas) != 1 {
		t.Fatalf("account_quotas = %d rows, want 1: %+v", len(st.AccountQuotas), st.AccountQuotas)
	}
	q := st.AccountQuotas[0]
	if q.PoolID != "pool1" || q.Unit != "requests" || q.Window != "daily" {
		t.Errorf("quota row = %+v, want pool1/requests/daily", q)
	}
	if q.PerAccountLimit != 100 || q.AccountCount != 3 || q.EffectiveLimit != 300 {
		t.Errorf("limits = per-account %v × %d = effective %v, want 100 × 3 = 300", q.PerAccountLimit, q.AccountCount, q.EffectiveLimit)
	}
	if q.Consumed != 30 || q.ConsumedTotal != 30 {
		t.Errorf("consumed = %v / total %v, want 30 / 30", q.Consumed, q.ConsumedTotal)
	}
	if q.Remaining != 270 || q.FairShare != 150 {
		t.Errorf("remaining = %v / fair share = %v, want 270 / 150", q.Remaining, q.FairShare)
	}
	if q.NextReset.IsZero() {
		t.Error("next_reset_at must be set")
	}
}

func TestMeStatusAccountQuotasEmpty(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var st meStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode me/status: %v", err)
	}
	if len(st.AccountQuotas) != 0 {
		t.Errorf("account_quotas = %d rows, want 0 (no pools)", len(st.AccountQuotas))
	}
}
