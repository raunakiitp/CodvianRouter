package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M8.18: teams + quota-pools management APIs.
//
// The routes are NOT registered on the live server mux yet —
// server.go registration and the guard.go route table are the
// coordinator's job (this slice may not edit either file). The
// tests therefore drive the handlers through a test-only mux
// with the exact patterns the coordinator will register, and
// the handlers themselves assume management auth was already
// enforced by the guard table, exactly like handleUsageBudget.
//
// Auth note: these tests send no Authorization header on the
// test mux on purpose — the guard is not under test here; the
// tier/scope proposal (Tier 3 MANAGEMENT, admin scope on all
// ten routes) is in the slice report for the coordinator to
// register.

// m818Mux wires the ten M8.18 handlers on the patterns the
// coordinator will register in server.go.
func m818Mux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/teams", srv.handleAPITeams)
	mux.HandleFunc("GET /api/teams", srv.handleAPITeams)
	mux.HandleFunc("GET /api/teams/{id}", srv.handleAPITeam)
	mux.HandleFunc("PATCH /api/teams/{id}", srv.handleAPITeam)
	mux.HandleFunc("DELETE /api/teams/{id}", srv.handleAPITeam)
	mux.HandleFunc("POST /api/quota-pools", srv.handleAPIQuotaPools)
	mux.HandleFunc("GET /api/quota-pools", srv.handleAPIQuotaPools)
	mux.HandleFunc("GET /api/quota-pools/{id}", srv.handleAPIQuotaPool)
	mux.HandleFunc("PATCH /api/quota-pools/{id}", srv.handleAPIQuotaPool)
	mux.HandleFunc("DELETE /api/quota-pools/{id}", srv.handleAPIQuotaPool)
	return mux
}

// m818Do sends one request to the test mux and returns the recorder.
func m818Do(t *testing.T, mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// m818Setup builds a server + test mux with a second gateway key
// (id2) for the two-member pool cases. cfgPath is the config file
// the persist option writes to.
func m818Setup(t *testing.T) (*keyBudgetSetup, *Server, *http.ServeMux, string, string) {
	t.Helper()
	s, cfgPath := keyStatusSetup(t)
	key2, _, err := config.IssueKey("test2", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key2); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t, s.persistOpt(cfgPath))
	return s, srv, m818Mux(srv), cfgPath, key2.ID
}

// m818RewritePlaceholders replaces placeholder key ids inside a
// loaded fixture's request body (fixtures cannot name the random
// gateway key ids issued per test run).
func m818RewritePlaceholders(t *testing.T, f *apitest.Fixture, mapping map[string]string) {
	t.Helper()
	f.Request.Body = m818RewriteValue(f.Request.Body, mapping)
}

func m818RewriteValue(v any, mapping map[string]string) any {
	switch node := v.(type) {
	case map[string]any:
		for k, val := range node {
			node[k] = m818RewriteValue(val, mapping)
		}
		return node
	case []any:
		for i, val := range node {
			node[i] = m818RewriteValue(val, mapping)
		}
		return node
	case string:
		if repl, ok := mapping[node]; ok {
			return repl
		}
		return node
	default:
		return v
	}
}

func TestAPITeamsCreateGetDeleteRoundTrip(t *testing.T) {
	s, _, mux, _, _ := m818Setup(t)
	// Create with a member + budget, through the API.
	rec := m818Do(t, mux, "POST", "/api/teams", `{"id":"team-rt","name":"Round Trip","description":"rt","max_budget_usd":5,"budget_duration":"7d","members":["`+s.id+`"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created teamView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v: %s", err, rec.Body.String())
	}
	if created.ID != "team-rt" || created.Name != "Round Trip" || created.MaxBudgetUSD == nil || *created.MaxBudgetUSD != 5 {
		t.Errorf("created = %+v, want team-rt / Round Trip / budget 5", created)
	}
	if len(created.Members) != 1 || created.Members[0].KeyID != s.id {
		t.Errorf("created members = %+v, want 1 member %s", created.Members, s.id)
	}
	if created.WindowStart == nil || created.WindowEnd == nil {
		t.Error("a budgeted team must report its window")
	}
	// The config file tracks the team too (persist was wired).
	found := false
	for _, tc := range s.cfg.Teams {
		if tc.ID == "team-rt" && len(tc.Members) == 1 && tc.Members[0] == s.id {
			found = true
		}
	}
	if !found {
		t.Errorf("cfg.Teams = %+v, want team-rt with member %s", s.cfg.Teams, s.id)
	}
	// List shows it.
	rec = m818Do(t, mux, "GET", "/api/teams", "")
	var list teamsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Teams) != 1 || list.Teams[0].ID != "team-rt" {
		t.Fatalf("list = %+v, want 1 team team-rt", list.Teams)
	}
	// Get one.
	rec = m818Do(t, mux, "GET", "/api/teams/team-rt", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Patch: rename, raise the budget, drop the member.
	rec = m818Do(t, mux, "PATCH", "/api/teams/team-rt", `{"name":"Renamed","max_budget_usd":20,"members":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var patched teamView
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode patched: %v", err)
	}
	if patched.Name != "Renamed" || patched.MaxBudgetUSD == nil || *patched.MaxBudgetUSD != 20 {
		t.Errorf("patched = %+v, want Renamed / budget 20", patched)
	}
	if len(patched.Members) != 0 {
		t.Errorf("patched members = %+v, want none after members:[]", patched.Members)
	}
	// Delete, then get 404s and the list is empty.
	rec = m818Do(t, mux, "DELETE", "/api/teams/team-rt", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("delete status = %d, want 200 ok:true: %s", rec.Code, rec.Body.String())
	}
	rec = m818Do(t, mux, "GET", "/api/teams/team-rt", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	rec = m818Do(t, mux, "GET", "/api/teams", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	if len(list.Teams) != 0 {
		t.Errorf("list after delete = %d teams, want 0", len(list.Teams))
	}
}

func TestAPITeamsCreateValidation(t *testing.T) {
	s, _, mux, _, _ := m818Setup(t)
	cases := []struct {
		name, body, want string
	}{
		{"missing name", `{"id":"t-bad1"}`, "name is required"},
		{"unknown member", `{"id":"t-bad2","name":"x","members":["ghost-key"]}`, "ghost-key"},
		{"archived with members", `{"id":"t-bad3","name":"x","status":"archived","members":["` + s.id + `"]}`, "archived team"},
		{"bad duration", `{"id":"t-bad4","name":"x","max_budget_usd":5,"budget_duration":"90d"}`, "budget_duration"},
		{"budget without duration", `{"id":"t-bad5","name":"x","max_budget_usd":5}`, "budget_duration"},
		{"duration without budget", `{"id":"t-bad6","name":"x","budget_duration":"7d"}`, "budget_duration"},
		{"negative budget", `{"id":"t-bad7","name":"x","max_budget_usd":-1,"budget_duration":"7d"}`, "max_budget_usd"},
		{"bad status", `{"id":"t-bad8","name":"x","status":"frozen"}`, "status"},
		{"duplicate member", `{"id":"t-bad9","name":"x","members":["` + s.id + `","` + s.id + `"]}`, "twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := m818Do(t, mux, "POST", "/api/teams", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("400 body = %s, want it to name %q", rec.Body.String(), tc.want)
			}
		})
	}
	// Duplicate id → 409, and nothing was stored for the bad ones.
	if rec := m818Do(t, mux, "POST", "/api/teams", `{"id":"t-dup","name":"Dup"}`); rec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if rec := m818Do(t, mux, "POST", "/api/teams", `{"id":"t-dup","name":"Dup again"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate create status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if rec := m818Do(t, mux, "GET", "/api/teams/t-bad2", ""); rec.Code != http.StatusNotFound {
		t.Errorf("rejected team t-bad2 must not exist, GET status = %d", rec.Code)
	}
}

func TestAPITeamsCreateAutoID(t *testing.T) {
	_, _, mux, _, _ := m818Setup(t)
	rec := m818Do(t, mux, "POST", "/api/teams", `{"name":"No ID Given"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created teamView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(created.ID, "team-") || created.ID == "team-" {
		t.Errorf("id = %q, want a generated team-<hex> id", created.ID)
	}
}

func TestAPITeamsPatchBudgetClear(t *testing.T) {
	_, _, mux, _, _ := m818Setup(t)
	if rec := m818Do(t, mux, "POST", "/api/teams", `{"id":"t-clear","name":"Clear","max_budget_usd":5,"budget_duration":"7d"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	rec := m818Do(t, mux, "PATCH", "/api/teams/t-clear", `{"max_budget_usd":null,"budget_duration":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var patched teamView
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if patched.MaxBudgetUSD != nil || patched.BudgetDuration != "" {
		t.Errorf("patched = %+v, want budget cleared", patched)
	}
	if patched.WindowStart != nil || patched.WindowEnd != nil {
		t.Error("a budget-less team must not report a window")
	}
}

// TestAPITeamsBudgetEnforcedByGate proves the API is a face over
// the existing machinery: a team + budget created through the API
// is refused by the pre-call team-budget gate on the chat path
// once its spend exceeds the cap — the budget is enforced, not
// just stored.
func TestAPITeamsBudgetEnforcedByGate(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath), WithProviderRegistry(modelsTestRegistry(t)))
	mux := m818Mux(srv)
	if rec := m818Do(t, mux, "POST", "/api/teams", `{"id":"t-gate","name":"Gate","max_budget_usd":1,"budget_duration":"7d","members":["`+s.id+`"]}`); rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of a $1.00 team budget
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("chat status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "team_budget") {
		t.Errorf("400 body = %s, want error type team_budget", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the team budget must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

// TestAPITeamsFixtureContracts runs one apitest fixture per teams
// endpoint (docs/API_FIXTURES.md): each fixture is recorded mock
// data, never live provider data.
func TestAPITeamsFixtureContracts(t *testing.T) {
	seed := func(t *testing.T, s *keyBudgetSetup) {
		t.Helper()
		max := 5.0
		if err := s.led.SetTeam(context.Background(), ledger.Team{
			ID: "team-api-1", Name: "API Team", Description: "created by fixture",
			MaxBudgetUSD: &max, BudgetDuration: "7d",
		}); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name     string
		needSeed bool
	}{
		{"teams-create-201", false},
		{"teams-list-200", true},
		{"teams-get-200", true},
		{"teams-get-404", false},
		{"teams-patch-200", true},
		{"teams-delete-200", true},
		{"teams-create-400-unknown-member", false},
		{"teams-create-400-missing-name", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, mux, _, _ := m818Setup(t)
			if tc.needSeed {
				seed(t, s)
			}
			f := apitest.LoadFixture(t, "m8.18", tc.name)
			apitest.Run(t, mux, f)
		})
	}
}

// TestAPITeamsToggle proves the fixture assertions actually bite:
// a wrong expected status fails, and a mux without the teams
// routes answers 404 — so the positive contract tests pass
// because the handlers exist, not because the assertions are lax.
func TestAPITeamsToggle(t *testing.T) {
	s, _, _, _, _ := m818Setup(t)
	max := 5.0
	if err := s.led.SetTeam(context.Background(), ledger.Team{
		ID: "team-api-1", Name: "API Team", MaxBudgetUSD: &max, BudgetDuration: "7d",
	}); err != nil {
		t.Fatal(err)
	}
	f := apitest.LoadFixture(t, "m8.18", "teams-get-200")
	f.Assert.Status = http.StatusCreated // wrong: the handler answers 200
	resp := &http.Response{StatusCode: http.StatusOK}
	body := []byte(`{"id":"team-api-1","name":"API Team","members":[]}`)
	if err := apitest.CheckResponse(resp, body, f); err == nil {
		t.Error("CheckResponse with a wrong expected status must fail")
	}
	// Feature removed: no teams routes at all → 404.
	empty := http.NewServeMux()
	rec := m818Do(t, empty, "POST", "/api/teams", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-route POST status = %d, want 404", rec.Code)
	}
}
