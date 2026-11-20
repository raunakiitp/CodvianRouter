package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M5.12: team cost centers, server side — the pre-call gate, the
// dashboard feeds (teams + membership), and the me/status section.
//
// The tests seed "pricey-model" (100k in / 100k out at $10/$20 per
// 1M in both the ledger price book and the catalog): $3.00 metered
// AND $3.00 list, so list and metered cost agree exactly. The
// ledger-only "priced-model" must NOT be used here — the catalog
// does not know it, so the list-price reprice would (correctly)
// call it unpriced.
func teamSetup(t *testing.T, max float64) (*keyBudgetSetup, string) {
	t.Helper()
	s, cfgPath := keyStatusSetup(t)
	if err := s.led.SetTeam(context.Background(), ledger.Team{
		ID: "t1", Name: "Alpha", MaxBudgetUSD: &max, BudgetDuration: "7d",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.led.AssignKeyToTeam(context.Background(), s.id, "t1", time.Now()); err != nil {
		t.Fatal(err)
	}
	return s, cfgPath
}

func TestTeamBudgetGateBlocksChat(t *testing.T) {
	s, _ := teamSetup(t, 1)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of a $1.00 team budget
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chat status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "team_budget") {
		t.Errorf("400 body = %s, want error type team_budget", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "soft committed-usage window resets at") {
		t.Errorf("400 body = %s, want the reference router's reset message", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the team budget must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestTeamBudgetGateAllowsUnderBudget(t *testing.T) {
	s, _ := teamSetup(t, 100)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of $100.00
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestTeamBudgetGateUnpricedFailsClosed(t *testing.T) {
	s, _ := teamSetup(t, 100)
	srv := s.server(t)
	// Unpriced usage in the window: the team cannot prove it is
	// under budget, so the gate refuses even though $100 is far
	// above the priced $0.00.
	seedModelSpend(t, s, "unknown-price-model", 1000, 1000, 0, 0)
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chat status = %d, want 400 (unpriced usage fails closed): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "team_budget") {
		t.Errorf("400 body = %s, want error type team_budget", rec.Body.String())
	}
}

func TestTeamBudgetGateNoTeamAllows(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (no team): %s", rec.Code, rec.Body.String())
	}
}

func TestTeamsDashboardRoundTrip(t *testing.T) {
	s, cfgPath := teamSetup(t, 5)
	srv := s.server(t, s.persistOpt(cfgPath), WithProviderRegistry(modelsTestRegistry(t)))
	// The teams POSTs are admin-scoped: widen this test's own
	// fixture key to chat+admin (same pattern as the pool tests).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of $5.00
	// GET lists the team with live costs and its member.
	rec := doReq(t, srv, "GET", "/dashboard/teams", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out teamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Teams) != 1 || out.Teams[0].ID != "t1" {
		t.Fatalf("GET teams = %+v, want 1 team t1", out.Teams)
	}
	got := out.Teams[0]
	if got.EstimatedListCostUSD != 3 || got.MeteredCostUSD != 3 || got.Exceeded {
		t.Errorf("team = %+v, want list 3 / metered 3 / not exceeded", got)
	}
	if len(got.Members) != 1 || got.Members[0].KeyID != s.id || got.Members[0].Requests != 1 || got.Members[0].Tokens != 200000 {
		t.Errorf("members = %+v, want 1 member with 1 request / 200000 tokens", got.Members)
	}
	if got.Members[0].ListCostUSD == nil || *got.Members[0].ListCostUSD != 3 {
		t.Errorf("member list cost = %+v, want 3", got.Members[0].ListCostUSD)
	}
	// Bad bodies are rejected, never stored.
	for _, bad := range []string{
		`{"id":"b2","max_budget_usd":5,"budget_duration":"7d"}`,
		`{"id":"b2","name":"B","max_budget_usd":0,"budget_duration":"7d"}`,
		`{"id":"b2","name":"B","budget_duration":"7d"}`,
		`{"id":"b2","name":"B","max_budget_usd":5}`,
		`{"id":"b2","name":"B","max_budget_usd":5,"budget_duration":"year"}`,
		`{"id":"b2","name":"B","status":"deleted"}`,
	} {
		rec := doReq(t, srv, "POST", "/dashboard/teams", bad, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad team POST status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	}
	// Create a second team via POST, then delete it.
	rec = doReq(t, srv, "POST", "/dashboard/teams", `{"id":"t2","name":"Beta"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST t2 status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/dashboard/teams", `{"id":"t2","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE t2 status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/teams", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Teams) != 1 {
		t.Errorf("GET after delete = %d teams, want 1", len(out.Teams))
	}
}

func TestTeamMembersEndpoint(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath), WithProviderRegistry(modelsTestRegistry(t)))
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	if err := s.led.SetTeam(context.Background(), ledger.Team{ID: "t1", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	// Assign.
	rec := doReq(t, srv, "POST", "/dashboard/teams/t1/members", `{"key_id":"`+s.id+`","action":"assign"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("assign status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if team, _ := s.led.ActiveTeamForKey(context.Background(), s.id); team == nil || team.ID != "t1" {
		t.Fatalf("ActiveTeamForKey = %+v, want t1", team)
	}
	// Unassign a key that is not a member of this team refuses.
	rec = doReq(t, srv, "POST", "/dashboard/teams/t1/members", `{"key_id":"other","action":"unassign"}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unassign non-member status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Assign to an unknown team refuses.
	rec = doReq(t, srv, "POST", "/dashboard/teams/nope/members", `{"key_id":"`+s.id+`","action":"assign"}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("assign unknown team status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// Unassign.
	rec = doReq(t, srv, "POST", "/dashboard/teams/t1/members", `{"key_id":"`+s.id+`","action":"unassign"}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("unassign status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if team, _ := s.led.ActiveTeamForKey(context.Background(), s.id); team != nil {
		t.Errorf("ActiveTeamForKey after unassign = %+v, want nil", team)
	}
	// Bad action refuses.
	rec = doReq(t, srv, "POST", "/dashboard/teams/t1/members", `{"key_id":"`+s.id+`","action":"move"}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad action status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestMeStatusTeamSection(t *testing.T) {
	s, _ := teamSetup(t, 5)
	srv := s.server(t, WithProviderRegistry(modelsTestRegistry(t)))
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of $5.00
	rec := doReq(t, srv, "GET", "/v1/me/status", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("me/status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	team, ok := out["team"].(map[string]any)
	if !ok {
		t.Fatalf("me/status team section missing: %s", rec.Body.String())
	}
	if team["team_id"] != "t1" || team["estimated_list_cost_usd"] != 3.0 || team["metered_cost_usd"] != 3.0 || team["exceeded"] != false {
		t.Errorf("team section = %+v, want t1 / 3 / 3 / not exceeded", team)
	}
	// A key with no team has no section.
	s2, _ := keyStatusSetup(t)
	srv2 := s2.server(t)
	rec = doReq(t, srv2, "GET", "/v1/me/status", "", s2.raw)
	out2 := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, ok := out2["team"]; ok {
		t.Errorf("me/status team section present for a key with no team: %s", rec.Body.String())
	}
}
