// Copyright (c) 2024 CODVIAN ROUTER AUTHORS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

func newQuotaIntelSetup(t *testing.T) *keyBudgetSetup {
	t.Helper()
	s := newKeyBudgetSetup(t)
	// Re-issue the key with admin scope so management routes are accessible
	adminKey, raw, err := config.IssueKey("test-admin", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(adminKey); err != nil {
		t.Fatal(err)
	}
	s.raw = raw
	s.id = adminKey.ID
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID:          "pool-alpha",
		Name:        "Alpha Pool",
		Dimensions: []ledger.QuotaDimension{
			{Unit: "requests", Window: "daily", Limit: 1000},
		},
		Allocations: []ledger.PoolAllocation{
			{KeyID: s.id, Weight: 100, Policy: "hard"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestQuotaIntelligenceEndpoint(t *testing.T) {
	s := newQuotaIntelSetup(t)
	srv := s.server(t)

	rec := doReq(t, srv, "GET", "/api/quota/intelligence", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp QuotaIntelligenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body.String())
	}

	// Should have at least one pool (pool-alpha)
	if len(resp.Pools) == 0 {
		t.Error("expected at least one pool, got none")
	}

	// Free-tier providers from catalog should appear as pools
	hasFreeTier := false
	for _, p := range resp.Pools {
		if p.ID == "pool-alpha" || p.Name == "Alpha Pool" {
			hasFreeTier = true
		}
	}
	if !hasFreeTier {
		t.Error("expected pool-alpha in response pools")
	}

	// GeneratedAt should be set
	if resp.GeneratedAt.IsZero() {
		t.Error("generated_at should be set")
	}

	// Filters should be echoed
	if resp.Filters.PoolID != "" {
		t.Errorf("filters.pool_id = %q, want empty", resp.Filters.PoolID)
	}
}

func TestQuotaIntelligenceFreeTierProviders(t *testing.T) {
	s := newQuotaIntelSetup(t)
	srv := s.server(t)

	rec := doReq(t, srv, "GET", "/api/quota/intelligence?free_tier_only=true", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp QuotaIntelligenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !resp.Filters.FreeTierOnly {
		t.Errorf("filters.free_tier_only = %v, want true", resp.Filters.FreeTierOnly)
	}
}

func TestQuotaIntelligenceInternalBudgets(t *testing.T) {
	s := newQuotaIntelSetup(t)
	srv := s.server(t)

	rec := doReq(t, srv, "GET", "/api/quota/intelligence", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp QuotaIntelligenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Internal budgets may be empty (no internal budgets seeded), but the field
	// should be present (empty array, not null).
	if resp.InternalBudgets == nil {
		t.Error("internal_budgets should be an array, not null")
	}
	if len(resp.InternalBudgets) != 0 {
		t.Errorf("expected 0 internal budgets, got %d", len(resp.InternalBudgets))
	}
}

func TestQuotaIntelligencePoolFilter(t *testing.T) {
	s := newQuotaIntelSetup(t)
	srv := s.server(t)

	rec := doReq(t, srv, "GET", "/api/quota/intelligence?pool_id=pool-alpha", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp QuotaIntelligenceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Filters.PoolID != "pool-alpha" {
		t.Errorf("filters.pool_id = %q, want pool-alpha", resp.Filters.PoolID)
	}

	for _, p := range resp.Pools {
		if p.ID != "pool-alpha" {
			t.Errorf("pool %q returned with pool_id filter=pool-alpha", p.ID)
		}
	}
}

func TestQuotaIntelligenceAuthRequired(t *testing.T) {
	s := newQuotaIntelSetup(t)
	srv := s.server(t)

	// No auth header should be rejected (TierManagement requires admin scope)
	rec := doReq(t, srv, "GET", "/api/quota/intelligence", "", "")
	if rec.Code == http.StatusOK {
		t.Error("expected auth rejection without credentials")
	}
}

func TestQuotaStateConstants(t *testing.T) {
	// Verify all quota states are distinct
	states := map[QuotaState]bool{
		QuotaStateHealthy:          true,
		QuotaStateApproachingLimit: true,
		QuotaStateExhausted:        true,
		QuotaStateUnavailable:      true,
		QuotaStateUnknown:          true,
	}
	if len(states) != 5 {
		t.Error("expected 5 distinct quota states")
	}
	if QuotaStateUnknown == QuotaStateExhausted {
		t.Error("unknown must not equal exhausted")
	}
	if QuotaStateHealthy == QuotaStateApproachingLimit {
		t.Error("healthy must not equal approaching_limit")
	}
}
