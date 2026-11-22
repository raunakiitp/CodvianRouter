// P3-A: org/team/RBAC management API.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callOrgHandler(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	// Set {id} path value for bind/unbind routes.
	if i := strings.Index(path, "/api/org/teams/"); i >= 0 {
		rest := path[len("/api/org/teams/"):]
		id := rest
		if j := strings.IndexByte(id, '/'); j >= 0 {
			id = id[:j]
		}
		if id != "" {
			req.SetPathValue("id", id)
		}
	}
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && path == "/api/org":
		srv.handleOrgCreate(rec, req)
	case method == http.MethodPost && path == "/api/org/teams":
		srv.handleOrgTeamCreate(rec, req)
	case strings.HasSuffix(path, "/bind"):
		srv.handleOrgTeamBind(rec, req)
	case strings.HasSuffix(path, "/unbind"):
		srv.handleOrgTeamUnbind(rec, req)
	case path == "/api/org/audit":
		srv.handleOrgAudit(rec, req)
	default:
		srv.handleOrgSummary(rec, req)
	}
	return rec
}

func TestP3AOrgAPI(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	// The org store is empty -> summary lists no teams.
	rec := callOrgHandler(t, srv, "GET", "/api/org", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("summary status = %d", rec.Code)
	}

	// Bootstrap an org.
	rec = callOrgHandler(t, srv, "POST", "/api/org", `{"id":"acme","name":"Acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org status = %d: %s", rec.Code, rec.Body.String())
	}

	// Create a team under the bootstrapped org.
	rec = callOrgHandler(t, srv, "POST", "/api/org/teams",
		`{"id":"core","org_id":"acme","name":"Core","budget_usd":50,"allowed_models":["gpt-5-mini"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create team status = %d: %s", rec.Code, rec.Body.String())
	}
	var created orgTeamView
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.ID != "core" || created.BudgetUSD != 50 {
		t.Fatalf("created team = %+v", created)
	}

	// Bind a key to the team.
	rec = callOrgHandler(t, srv, "POST", "/api/org/teams/core/bind", `{"key_id":"key-x","team_id":"core"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status = %d: %s", rec.Code, rec.Body.String())
	}

	// Audit log reflects create + bind.
	rec = callOrgHandler(t, srv, "GET", "/api/org/audit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit status = %d", rec.Code)
	}
	var audit []struct{ Action string }
	if err := json.NewDecoder(rec.Body).Decode(&audit); err != nil {
		t.Fatal(err)
	}
	if len(audit) < 2 {
		t.Fatalf("audit = %+v, want >= 2 entries", audit)
	}

	// Unbind.
	rec = callOrgHandler(t, srv, "POST", "/api/org/teams/core/unbind?key_id=key-x", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind status = %d: %s", rec.Code, rec.Body.String())
	}
}
