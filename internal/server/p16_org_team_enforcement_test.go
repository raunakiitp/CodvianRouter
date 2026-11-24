// P1.6 — organization/team request-path enforcement.
//
// These tests prove a DECLARED team/org policy actually changes request
// outcomes on every dialect, before any provider is dispatched, and that
// no alternate API is a bypass. They also cover the negative cases the
// block calls for: unauthorized models, unauthorized strategies,
// cross-team isolation, and an over-budget organization.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/org"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// p16FullEngine is countingEngine plus the optional engine surfaces the
// media/embeddings dialects need, so those handlers reach their policy
// gate instead of answering 503 "surface not available". chatCalls and
// streamCalls stay observable through the embedded countingEngine.
type p16FullEngine struct{ *countingEngine }

func (e *p16FullEngine) Embed(ctx context.Context, req provider.EmbeddingRequest) (*provider.EmbeddingResponse, error) {
	return &provider.EmbeddingResponse{Object: "list", Model: req.Model, Usage: provider.Usage{PromptTokens: 5, TotalTokens: 5}}, nil
}

func (e *p16FullEngine) Rerank(ctx context.Context, req provider.RerankRequest) (*provider.RerankResponse, error) {
	return &provider.RerankResponse{Model: req.Model}, nil
}

func (e *p16FullEngine) Images(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	return &provider.ImageResponse{StatusCode: 200, Model: req.Model, Body: json.RawMessage(`{"data":[]}`)}, nil
}

func (e *p16FullEngine) AudioPassthrough(ctx context.Context, req router.AudioRequest) (*router.AudioResponse, error) {
	return &router.AudioResponse{StatusCode: 200, ContentType: "application/json", Model: req.Model, Body: []byte(`{}`)}, nil
}

// p16FullServer builds the fixture server with p16FullEngine wired, so
// every P1.6 dialect is reachable and counted.
func p16FullServer(t *testing.T, s *keyBudgetSetup, extra ...Option) (*Server, *p16FullEngine) {
	t.Helper()
	eng := &p16FullEngine{countingEngine: s.eng}
	opts := []Option{WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}})}
	return newTestServer(t, eng, append(opts, extra...)...), eng
}

// p16Setup wires a server whose authenticated key (s.id) is bound to
// org "o1" / team "t1" with the given allowlists. An empty allowlist
// means unrestricted (the store's own convention).
func p16Setup(t *testing.T, allowedModels, allowedStrategies []string) (*keyBudgetSetup, *Server, *org.Store, string) {
	t.Helper()
	s, cfgPath := keyStatusSetup(t)
	srv, _ := p16FullServer(t, s, WithProviderRegistry(modelsTestRegistry(t)))
	st, err := srv.ensureOrgStore()
	if err != nil || st == nil {
		t.Fatalf("ensureOrgStore: %v", err)
	}
	if err := st.AddOrg(&org.Organization{ID: "o1", Name: "Acme"}, org.RoleOwner); err != nil {
		t.Fatalf("AddOrg: %v", err)
	}
	if err := st.AddTeam(&org.Team{
		ID: "t1", OrgID: "o1", Name: "Alpha",
		AllowedModels:     allowedModels,
		AllowedStrategies: allowedStrategies,
	}, org.RoleOwner); err != nil {
		t.Fatalf("AddTeam: %v", err)
	}
	if err := st.BindKey(s.id, "t1", org.RoleOwner); err != nil {
		t.Fatalf("BindKey: %v", err)
	}
	return s, srv, st, cfgPath
}

func p16ErrorType(t *testing.T, body []byte) string {
	t.Helper()
	var out struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode error body %q: %v", string(body), err)
	}
	return out.Error.Type
}

// TestP16TeamModelAllowlistRefusesEveryDialect is the core negative
// test: a team restricted to one model cannot reach a different model
// through ANY model-addressed dialect, and no provider is dispatched.
func TestP16TeamModelAllowlistRefusesEveryDialect(t *testing.T) {
	s, srv, _, _ := p16Setup(t, []string{"cheap-model"}, nil)
	// pricey-model exists in the registry, so only the team policy can
	// stop it.
	denied := "pricey-model"

	cases := []struct {
		name string
		path string
		body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"` + denied + `","messages":[{"role":"user","content":"hi"}]}`},
		{"messages", "/v1/messages", g20MessagesBody(denied)},
		{"responses", "/v1/responses", `{"model":"` + denied + `","input":"hi"}`},
		{"completions", "/v1/completions", `{"model":"` + denied + `","prompt":"hi"}`},
		{"gemini", "/v1beta/models/" + denied + ":generateContent", g20GeminiBody()},
		{"embeddings", "/v1/embeddings", `{"model":"` + denied + `","input":"hi"}`},
		{"images", "/v1/images/generations", `{"model":"` + denied + `","prompt":"a cat"}`},
		{"audio", "/v1/audio/speech", `{"model":"` + denied + `","input":"hi","voice":"alloy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := s.eng.chatCalls.Load()
			var rec *httptest.ResponseRecorder
			if tc.name == "gemini" {
				rec = doReqPeer(t, srv, http.MethodPost, tc.path, tc.body, s.raw, loopbackPeer)
			} else {
				rec = doReq(t, srv, http.MethodPost, tc.path, tc.body, s.raw)
			}
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s status = %d, want 403: %s", tc.name, rec.Code, rec.Body.String())
			}
			if got := p16ErrorType(t, rec.Body.Bytes()); got != "team_policy" {
				t.Fatalf("%s error type = %q, want team_policy", tc.name, got)
			}
			if after := s.eng.chatCalls.Load(); after != before {
				t.Fatalf("%s dispatched %d provider call(s) despite the denial", tc.name, after-before)
			}
		})
	}
}

// TestP16TeamModelAllowlistAllowsListedModel: the same team still serves
// the model it does allow — the gate refuses, it does not disable.
func TestP16TeamModelAllowlistAllowsListedModel(t *testing.T) {
	s, srv, _, _ := p16Setup(t, []string{"cheap-model"}, nil)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"cheap-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 for an allowed model: %s", rec.Code, rec.Body.String())
	}
}

// TestP16TeamStrategyAllowlistRefusesChat: a team restricted to one
// routing strategy refuses a different contract strategy.
func TestP16TeamStrategyAllowlistRefusesChat(t *testing.T) {
	s, srv, _, _ := p16Setup(t, nil, []string{"priority"})
	body := `{"model":"cheap-model","messages":[{"role":"user","content":"hi"}],"routing":{"sort":"cheapest"}}`
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not allowed for this team") {
		t.Fatalf("body = %s, want the strategy refusal", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatal("a strategy denial must not dispatch a provider call")
	}
}

// TestP16StreamingAndNonStreamingAgree: the policy decides before the
// streaming branch, so a streaming request is refused identically.
func TestP16StreamingAndNonStreamingAgree(t *testing.T) {
	s, srv, _, _ := p16Setup(t, []string{"cheap-model"}, nil)
	for _, stream := range []string{"false", "true"} {
		body := `{"model":"pricey-model","stream":` + stream + `,"messages":[{"role":"user","content":"hi"}]}`
		rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, s.raw)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("stream=%s status = %d, want 403 (streaming must not bypass the policy)", stream, rec.Code)
		}
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatal("no provider call may be dispatched for a refused request")
	}
}

// TestP16DenialIsAuditedAndPersists: a refusal writes an audit record
// that survives reopening the store (restart persistence).
func TestP16DenialIsAuditedAndPersists(t *testing.T) {
	s, srv, _, cfgPath := p16Setup(t, []string{"cheap-model"}, nil)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"pricey-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	audit := callOrgHandler(t, srv, http.MethodGet, "/api/org/audit", "")
	if audit.Code != http.StatusOK {
		t.Fatalf("audit status = %d: %s", audit.Code, audit.Body.String())
	}
	if !strings.Contains(audit.Body.String(), "request.denied") {
		t.Fatalf("audit log has no request.denied entry: %s", audit.Body.String())
	}

	// Restart persistence: the same data dir's org store still holds it.
	dir := s.cfg.DataDir
	if dir == "" {
		dir = filepath.Dir(cfgPath)
	}
	reopened, err := org.Open(filepath.Join(dir, "org.json"))
	if err != nil {
		t.Fatalf("reopen org store: %v", err)
	}
	entries := reopened.Audit(50)
	found := false
	for _, e := range entries {
		if e.Action == "request.denied" && strings.Contains(e.Detail, "pricey-model") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reopened store lost the denial audit entry: %+v", entries)
	}
	// The policy itself survives the restart too: the team's allowlist is
	// still in force after reopening.
	if team := reopened.TeamForKey(s.id); team == nil {
		t.Fatal("reopened store lost the key→team binding")
	} else if !reopened.AllowModel(team.ID, "cheap-model") || reopened.AllowModel(team.ID, "pricey-model") {
		t.Fatalf("reopened allowlist changed: %+v", team.AllowedModels)
	}
}

// TestP16DenialConsumesNoTrafficSlot: a policy refusal happens before the
// per-key traffic limiter admits the request, so it neither dispatches nor
// spends a request slot (error/cancellation accounting).
func TestP16DenialConsumesNoTrafficSlot(t *testing.T) {
	s, srv, _, _ := p16Setup(t, []string{"cheap-model"}, nil)
	g20SetRPM(s, 1)

	// Denied: refused, and must not consume the single slot.
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"pricey-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Allowed: the slot is still there, so this succeeds.
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"cheap-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed status = %d, want 200 (a denial must not spend a traffic slot): %s", rec.Code, rec.Body.String())
	}
}

// TestP16OtherTeamAndUnboundKeyAreUnaffected: isolation is per key
// binding — another team's allowlist cannot restrict this key, and an
// unbound key keeps working (no policy was declared for it).
func TestP16OtherTeamAndUnboundKeyAreUnaffected(t *testing.T) {
	s, srv, st, _ := p16Setup(t, []string{"cheap-model"}, nil)

	// A second team with a different allowlist: binding a DIFFERENT key
	// to it must not change this key's outcome.
	if err := st.AddTeam(&org.Team{ID: "t2", OrgID: "o1", Name: "Beta", AllowedModels: []string{"pricey-model"}}, org.RoleOwner); err != nil {
		t.Fatalf("AddTeam t2: %v", err)
	}
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"pricey-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for team t1's restriction: %s", rec.Code, rec.Body.String())
	}

	// Unbind: with no team binding the key is unrestricted again.
	if err := st.UnbindKey(s.id, org.RoleOwner); err != nil {
		t.Fatalf("UnbindKey: %v", err)
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"pricey-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbound key status = %d, want 200 (no declared policy): %s", rec.Code, rec.Body.String())
	}
}

// p16BindOrgTeamWithLedger creates an org team AND its ledger twin and
// binds the fixture key to both, so seeded usage is attributed to the
// team the org budget is computed over (the two stores share the team
// id, exactly as POST /api/org/teams does).
func p16BindOrgTeamWithLedger(t *testing.T, s *keyBudgetSetup, st *org.Store, orgID, teamID string, orgBudget float64) {
	t.Helper()
	if err := st.AddOrg(&org.Organization{ID: orgID, Name: orgID, DefaultBudgetUSD: orgBudget}, org.RoleOwner); err != nil {
		t.Fatalf("AddOrg %s: %v", orgID, err)
	}
	if err := st.AddTeam(&org.Team{ID: teamID, OrgID: orgID, Name: teamID}, org.RoleOwner); err != nil {
		t.Fatalf("AddTeam %s: %v", teamID, err)
	}
	if err := st.BindKey(s.id, teamID, org.RoleOwner); err != nil {
		t.Fatalf("BindKey %s: %v", teamID, err)
	}
	// Ledger twin with NO team budget, so only the org budget can
	// refuse — the point of the test.
	if err := s.led.SetTeam(context.Background(), ledger.Team{ID: teamID, Name: teamID}); err != nil {
		t.Fatalf("ledger SetTeam %s: %v", teamID, err)
	}
	if err := s.led.AssignKeyToTeam(context.Background(), s.id, teamID, time.Now()); err != nil {
		t.Fatalf("AssignKeyToTeam %s: %v", teamID, err)
	}
}

// TestP16OrgBudgetRefused: the organization's declared budget refuses
// before dispatch, with its own error type — distinct from team_budget.
func TestP16OrgBudgetRefused(t *testing.T) {
	s, srv, st, _ := p16Setup(t, nil, nil)
	p16BindOrgTeamWithLedger(t, s, st, "ob", "tb", 0.5)
	seedModelSpend(t, s, "pricey-model", 100000, 100000, 10.0/1e6, 20.0/1e6) // $3.00 of a $0.50 org budget

	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"pricey-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (org budget): %s", rec.Code, rec.Body.String())
	}
	if got := p16ErrorType(t, rec.Body.Bytes()); got != "org_budget" {
		t.Fatalf("error type = %q, want org_budget", got)
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatal("an org-budget refusal must not dispatch a provider call")
	}
}

// TestP16OrgBudgetUnpricedFailsClosed: usage that cannot be priced
// cannot prove the org is under budget, so it fails closed — the same
// honesty rule the team gate follows.
func TestP16OrgBudgetUnpricedFailsClosed(t *testing.T) {
	s, srv, st, _ := p16Setup(t, nil, nil)
	p16BindOrgTeamWithLedger(t, s, st, "oz", "tz", 1000)
	seedModelSpend(t, s, "unknown-price-model", 1000, 1000, 0, 0) // unpriced usage

	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"cheap-model","messages":[{"role":"user","content":"hi"}]}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unpriced usage fails closed): %s", rec.Code, rec.Body.String())
	}
	if got := p16ErrorType(t, rec.Body.Bytes()); got != "org_budget" {
		t.Fatalf("error type = %q, want org_budget", got)
	}
}

// TestP16BatchItemPolicyDenied: a batch cannot smuggle a model the
// owner's key is not allowed to call.
func TestP16BatchItemPolicyDenied(t *testing.T) {
	s, srv, _, _ := p16Setup(t, []string{"cheap-model"}, nil)
	ref := srv.evaluateTeamPolicy(context.Background(), s.id, "pricey-model", "")
	if ref == nil {
		t.Fatal("evaluateTeamPolicy allowed a model the team forbids")
	}
	if ref.Status != http.StatusForbidden || ref.Type != "team_policy" {
		t.Fatalf("refusal = %+v, want 403 team_policy", ref)
	}
	allowed := srv.evaluateTeamPolicy(context.Background(), s.id, "cheap-model", "")
	if allowed != nil {
		t.Fatalf("evaluateTeamPolicy refused an allowed model: %+v", allowed)
	}
}
