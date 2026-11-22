// P3-A: organization / team / RBAC management API.
//
// Exposes the org.Store through the management API so an operator can
// create teams, bind gateway keys to a team (team-key isolation) and
// read the audit log. Reads are MANAGEMENT, mutations ALWAYS_PROTECTED,
// all admin scope. The store is lazily opened from cfg.DataDir.
package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/org"
)

var (
	orgMu       sync.Mutex
	orgStore    *org.Store
	orgStoreErr error
)

func (s *Server) ensureOrgStore() (*org.Store, error) {
	orgMu.Lock()
	defer orgMu.Unlock()
	if orgStoreErr != nil {
		return nil, orgStoreErr
	}
	if orgStore != nil {
		return orgStore, nil
	}
	var path string
	if s.cfg != nil && s.cfg.DataDir != "" {
		path = filepath.Join(s.cfg.DataDir, "org.json")
	}
	st, err := org.Open(path)
	if err != nil {
		orgStoreErr = err
		return nil, err
	}
	orgStore = st
	return st, nil
}

type orgTeamView struct {
	ID                string            `json:"id"`
	OrgID             string            `json:"org_id"`
	Name              string            `json:"name"`
	BudgetUSD         float64           `json:"budget_usd"`
	AllowedModels     []string          `json:"allowed_models,omitempty"`
	AllowedStrategies []string          `json:"allowed_strategies,omitempty"`
	Roles             map[string]string `json:"roles,omitempty"`
}

func orgTeamViewOf(t *org.Team) orgTeamView {
	roles := make(map[string]string, len(t.Roles))
	for k, v := range t.Roles {
		roles[k] = string(v)
	}
	return orgTeamView{
		ID: t.ID, OrgID: t.OrgID, Name: t.Name, BudgetUSD: t.BudgetUSD,
		AllowedModels: t.AllowedModels, AllowedStrategies: t.AllowedStrategies, Roles: roles,
	}
}

type orgSummaryView struct {
	Orgs     int           `json:"orgs"`
	Teams    []orgTeamView `json:"teams"`
	Bindings []string      `json:"bindings"` // bound key ids
}

// handleOrgSummary (GET /api/org) lists teams and key bindings.
func (s *Server) handleOrgSummary(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	view := orgSummaryView{Orgs: len(st.Orgs())}
	for _, t := range st.Teams() {
		view.Teams = append(view.Teams, orgTeamViewOf(t))
	}
	// Bindings are not enumerated here (a key id is sensitive); the
	// audit log records bind/unbind actions with the key ids.
	writeJSON(w, http.StatusOK, view)
}

// orgCreateInput is the org-create body.
type orgCreateInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// handleOrgCreate (POST /api/org) creates/boootstraps an org.
func (s *Server) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	var in orgCreateInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := st.AddOrg(&org.Organization{ID: in.ID, Name: in.Name}, org.RoleOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": in.ID, "name": in.Name})
}

// orgTeamInput is the team-create body.
type orgTeamInput struct {
	ID                string   `json:"id"`
	OrgID             string   `json:"org_id"`
	Name              string   `json:"name"`
	BudgetUSD         float64  `json:"budget_usd"`
	AllowedModels     []string `json:"allowed_models"`
	AllowedStrategies []string `json:"allowed_strategies"`
}

// handleOrgTeamCreate (POST /api/org/teams).
func (s *Server) handleOrgTeamCreate(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	var in orgTeamInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	t := &org.Team{ID: in.ID, OrgID: in.OrgID, Name: in.Name, BudgetUSD: in.BudgetUSD,
		AllowedModels: in.AllowedModels, AllowedStrategies: in.AllowedStrategies}
	// The operator key's scope maps to an owner-level actor here (this
	// server has no separate identity store; the gateway key IS the
	// admin principal, so manage actions are authorized by admin scope).
	if err := st.AddTeam(t, org.RoleOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, orgTeamViewOf(t))
}

// orgBindInput is the key-bind body.
type orgBindInput struct {
	KeyID  string `json:"key_id"`
	TeamID string `json:"team_id"`
}

// handleOrgTeamBind (POST /api/org/teams/{id}/bind) binds a gateway key
// to a team.
func (s *Server) handleOrgTeamBind(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	var in orgBindInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	teamID := r.PathValue("id")
	if in.TeamID != "" {
		teamID = in.TeamID
	}
	if err := st.BindKey(in.KeyID, teamID, org.RoleOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key_id": in.KeyID, "team_id": teamID})
}

// handleOrgTeamUnbind (POST /api/org/teams/{id}/unbind?key_id=...) removes
// a key's team binding.
func (s *Server) handleOrgTeamUnbind(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	keyID := r.URL.Query().Get("key_id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "missing key_id")
		return
	}
	if err := st.UnbindKey(keyID, org.RoleOwner); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key_id": keyID, "unbound": true})
}

// handleOrgAudit (GET /api/org/audit) returns the audit log.
func (s *Server) handleOrgAudit(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, st.Audit(limit))
}

// registerOrgRoutes wires the P3-A management API (guard rows exist).
func (s *Server) registerOrgRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/org", s.handleOrgSummary)
	s.register(mux, "POST", "/api/org", s.handleOrgCreate)
	s.register(mux, "POST", "/api/org/teams", s.handleOrgTeamCreate)
	s.register(mux, "POST", "/api/org/teams/{id}/bind", s.handleOrgTeamBind)
	s.register(mux, "POST", "/api/org/teams/{id}/unbind", s.handleOrgTeamUnbind)
	s.register(mux, "GET", "/api/org/audit", s.handleOrgAudit)
}
