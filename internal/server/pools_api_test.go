package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M8.18: quota-pools management API. The mux/auth note in
// teams_api_test.go applies here too: the routes are not yet
// registered on the live server mux (coordinator's job), the
// tests drive the handlers through the test-only m818Mux, and
// the proposed tier is Tier 3 MANAGEMENT with the admin scope.

func m818SeedPool(t *testing.T, s *keyBudgetSetup) {
	t.Helper()
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool-api-1", Name: "API Pool",
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: 100}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 50, Policy: "hard"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIQuotaPoolsCreateGetDeleteRoundTrip(t *testing.T) {
	s, _, mux, _, id2 := m818Setup(t)
	// Create with two members; the dimension limit is
	// per-account (100) × 1 backing account = 100 effective.
	rec := m818Do(t, mux, "POST", "/api/quota-pools", `{"id":"pool-rt","name":"Round Trip","dimensions":[{"unit":"requests","window":"daily","limit":100}],"allocations":[{"key_id":"`+s.id+`","weight":50,"policy":"hard"},{"key_id":"`+id2+`","weight":50,"policy":"soft"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created quotaPoolView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v: %s", err, rec.Body.String())
	}
	if created.ID != "pool-rt" || created.Name != "Round Trip" {
		t.Errorf("created = %+v, want pool-rt / Round Trip", created)
	}
	if len(created.Dimensions) != 1 || created.Dimensions[0].Limit != 100 || created.Dimensions[0].PerAccountLimit != 100 || created.Dimensions[0].AccountCount != 1 {
		t.Errorf("created dimensions = %+v, want 1 dimension, effective limit 100 (per-account 100 × 1)", created.Dimensions)
	}
	if len(created.Allocations) != 2 || created.Allocations[0].Weight != 50 || created.Allocations[1].Weight != 50 {
		t.Errorf("created allocations = %+v, want 2 × weight 50", created.Allocations)
	}
	if created.Dimensions[0].NextReset == "" {
		t.Error("dimension must report next_reset_at")
	}
	// The config file tracks the pool too (persist was wired).
	found := false
	for _, p := range s.cfg.QuotaPools {
		if p.ID == "pool-rt" && len(p.Allocations) == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("cfg.QuotaPools = %+v, want pool-rt with 2 allocations", s.cfg.QuotaPools)
	}
	// List shows it.
	rec = m818Do(t, mux, "GET", "/api/quota-pools", "")
	var list quotaPoolsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Pools) != 1 || list.Pools[0].ID != "pool-rt" {
		t.Fatalf("list = %+v, want 1 pool pool-rt", list.Pools)
	}
	// Get one.
	rec = m818Do(t, mux, "GET", "/api/quota-pools/pool-rt", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Patch: rename + tighten the saturation threshold; the
	// dimensions and allocations keep their stored values.
	rec = m818Do(t, mux, "PATCH", "/api/quota-pools/pool-rt", `{"name":"Renamed","saturation_threshold":0.9}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var patched quotaPoolView
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if patched.Name != "Renamed" || patched.SaturationThreshold != 0.9 {
		t.Errorf("patched = %+v, want Renamed / threshold 0.9", patched)
	}
	if len(patched.Dimensions) != 1 || len(patched.Allocations) != 2 {
		t.Errorf("patched dimensions/allocations = %d/%d, want 1/2 (absent fields kept)", len(patched.Dimensions), len(patched.Allocations))
	}
	// Delete, then get 404s and the list is empty.
	rec = m818Do(t, mux, "DELETE", "/api/quota-pools/pool-rt", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("delete status = %d, want 200 ok:true: %s", rec.Code, rec.Body.String())
	}
	rec = m818Do(t, mux, "GET", "/api/quota-pools/pool-rt", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	rec = m818Do(t, mux, "GET", "/api/quota-pools", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	if len(list.Pools) != 0 {
		t.Errorf("list after delete = %d pools, want 0", len(list.Pools))
	}
}

func TestAPIQuotaPoolsShareValidation(t *testing.T) {
	s, _, mux, _, id2 := m818Setup(t)
	dim := `"dimensions":[{"unit":"requests","window":"daily","limit":100}]`
	cases := []struct {
		name, body, want string
	}{
		{"all zero shares", `{"id":"p-bad1","name":"x",` + dim + `,"allocations":[{"key_id":"` + s.id + `","weight":0,"policy":"hard"},{"key_id":"` + id2 + `","weight":0,"policy":"hard"}]}`, "allocations"},
		{"shares over 100", `{"id":"p-bad2","name":"x",` + dim + `,"allocations":[{"key_id":"` + s.id + `","weight":60,"policy":"hard"},{"key_id":"` + id2 + `","weight":60,"policy":"hard"}]}`, "allocations"},
		{"unknown key", `{"id":"p-bad3","name":"x",` + dim + `,"allocations":[{"key_id":"ghost-key","weight":50,"policy":"hard"}]}`, "ghost-key"},
		{"duplicate key", `{"id":"p-bad4","name":"x",` + dim + `,"allocations":[{"key_id":"` + s.id + `","weight":50,"policy":"hard"},{"key_id":"` + s.id + `","weight":50,"policy":"hard"}]}`, "allocated twice"},
		{"bad unit", `{"id":"p-bad5","name":"x","dimensions":[{"unit":"percent","window":"daily","limit":100}],"allocations":[]}`, "unit"},
		{"zero limit", `{"id":"p-bad6","name":"x","dimensions":[{"unit":"requests","window":"daily","limit":0}],"allocations":[]}`, "limit"},
		{"no dimensions", `{"id":"p-bad7","name":"x","allocations":[]}`, "dimension"},
		{"missing name", `{"id":"p-bad8",` + dim + `,"allocations":[]}`, "name"},
		{"bad policy", `{"id":"p-bad9","name":"x",` + dim + `,"allocations":[{"key_id":"` + s.id + `","weight":50,"policy":"greedy"}]}`, "policy"},
		{"weight over 100", `{"id":"p-bad10","name":"x",` + dim + `,"allocations":[{"key_id":"` + s.id + `","weight":150,"policy":"hard"}]}`, "weight"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := m818Do(t, mux, "POST", "/api/quota-pools", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("400 body = %s, want it to name %q", rec.Body.String(), tc.want)
			}
		})
	}
	// Duplicate id → 409, and nothing was stored for the bad ones.
	if rec := m818Do(t, mux, "POST", "/api/quota-pools", `{"id":"p-dup","name":"Dup",`+dim+`,"allocations":[]}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := m818Do(t, mux, "POST", "/api/quota-pools", `{"id":"p-dup","name":"Dup again",`+dim+`,"allocations":[]}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if rec := m818Do(t, mux, "GET", "/api/quota-pools/p-bad1", ""); rec.Code != http.StatusNotFound {
		t.Errorf("rejected pool p-bad1 must not exist, GET status = %d", rec.Code)
	}
	// A pool with NO allocations is allowed (members join later) —
	// the shares rule only fires when allocations exist.
	if rec := m818Do(t, mux, "POST", "/api/quota-pools", `{"id":"p-empty","name":"Empty",`+dim+`,"allocations":[]}`); rec.Code != http.StatusCreated {
		t.Fatalf("empty-allocations create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// TestAPIQuotaPoolsFairShareEnforcedByGate proves the API is a
// face over the existing machinery: a pool created through the
// API blocks the chat path with 429 key_pool_limit once its
// global limit is consumed — the pool is enforced, not just
// stored.
func TestAPIQuotaPoolsFairShareEnforcedByGate(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath), WithProviderRegistry(modelsTestRegistry(t)))
	mux := m818Mux(srv)
	if rec := m818Do(t, mux, "POST", "/api/quota-pools", `{"id":"pool-gate","name":"Gate","dimensions":[{"unit":"requests","window":"daily","limit":100}],"allocations":[{"key_id":"`+s.id+`","weight":50,"policy":"hard"}]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 100; i++ {
		s.seedPricedSpend(t, 1, 1) // 100 requests consumed today
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_pool_limit") {
		t.Errorf("429 body = %s, want error type key_pool_limit", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the pool must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

// TestAPIQuotaPoolsFixtureContracts runs one apitest fixture per
// quota-pool endpoint (docs/API_FIXTURES.md): each fixture is
// recorded mock data, never live provider data.
func TestAPIQuotaPoolsFixtureContracts(t *testing.T) {
	cases := []struct {
		name         string
		needSeed     bool
		placeholders map[string]string // filled per-test with real key ids
	}{
		{"pools-create-201", false, map[string]string{"key-placeholder-1": "id1"}},
		{"pools-list-200", true, nil},
		{"pools-get-200", true, nil},
		{"pools-get-404", false, nil},
		{"pools-patch-200", true, nil},
		{"pools-delete-200", true, nil},
		{"pools-create-400-all-zero-shares", false, map[string]string{"key-placeholder-1": "id1", "key-placeholder-2": "id2"}},
		{"pools-create-400-shares-over-100", false, map[string]string{"key-placeholder-1": "id1", "key-placeholder-2": "id2"}},
		{"pools-create-400-unknown-key", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, mux, _, id2 := m818Setup(t)
			if tc.needSeed {
				m818SeedPool(t, s)
			}
			f := apitest.LoadFixture(t, "m8.18", tc.name)
			if len(tc.placeholders) > 0 {
				mapping := map[string]string{}
				for ph, which := range tc.placeholders {
					if which == "id1" {
						mapping[ph] = s.id
					} else {
						mapping[ph] = id2
					}
				}
				m818RewritePlaceholders(t, &f, mapping)
			}
			apitest.Run(t, mux, f)
		})
	}
}

// TestAPIQuotaPoolsToggle proves the fixture assertions actually
// bite: a wrong expected status fails, and a mux without the
// pool routes answers 404 — so the positive contract tests pass
// because the handlers exist, not because the assertions are lax.
func TestAPIQuotaPoolsToggle(t *testing.T) {
	s, _, _, _, _ := m818Setup(t)
	m818SeedPool(t, s)
	f := apitest.LoadFixture(t, "m8.18", "pools-get-200")
	f.Assert.Status = http.StatusCreated // wrong: the handler answers 200
	resp := &http.Response{StatusCode: http.StatusOK}
	body := []byte(`{"id":"pool-api-1","name":"API Pool","dimensions":[],"allocations":[]}`)
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Error("CheckResponse with a wrong expected status must fail")
	}
	empty := http.NewServeMux()
	rec := m818Do(t, empty, "POST", "/api/quota-pools", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-route POST status = %d, want 404", rec.Code)
	}
}
