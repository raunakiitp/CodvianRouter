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
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/org"
)

// orgState holds the lazily-opened P3-A org store on THIS Server
// instance (not a package global), so two Servers with different data
// dirs stay isolated.
type orgState struct {
	mu       sync.Mutex
	store    *org.Store
	storeErr error
}

func (s *Server) orgStateInit() *orgState {
	s.orgStateMu.Lock()
	defer s.orgStateMu.Unlock()
	if s.orgState == nil {
		s.orgState = &orgState{}
	}
	return s.orgState
}

func (s *Server) ensureOrgStore() (*org.Store, error) {
	st := s.orgStateInit()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.storeErr != nil {
		return nil, st.storeErr
	}
	if st.store != nil {
		return st.store, nil
	}
	var path string
	if s.cfg != nil && s.cfg.DataDir != "" {
		path = filepath.Join(s.cfg.DataDir, "org.json")
	}
	store, err := org.Open(path)
	if err != nil {
		st.storeErr = err
		return nil, err
	}
	st.store = store
	return st.store, nil
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
	// AUD-SEC-003: bound the body before decoding, like every other handler.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
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
	// AUD-SEC-003: bound the body before decoding, like every other handler.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
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
	// Also create in the ledger for team budget enforcement (unifies dual Team model)
	if s.ledger != nil {
		ledgerTeam := ledger.Team{
			ID:             in.ID,
			Name:           in.Name,
			Description:    "",
			Status:         "active",
			MaxBudgetUSD:   nil, // Budget is managed via org store's BudgetUSD
			BudgetDuration: "",
		}
		if in.BudgetUSD > 0 {
			ledgerTeam.MaxBudgetUSD = &in.BudgetUSD
			ledgerTeam.BudgetDuration = "30d" // default to monthly
		}
		if err := s.ledger.SetTeam(r.Context(), ledgerTeam); err != nil {
			// Rollback org store team on ledger failure
			_ = st.DeleteTeam(in.ID)
			writeError(w, http.StatusBadRequest, "ledger team create failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusCreated, orgTeamViewOf(t))
}

// orgBindInput is the key-bind body.
type orgBindInput struct {
	KeyID  string `json:"key_id"`
	TeamID string `json:"team_id"`
}

// handleOrgTeamBind (POST /api/org/teams/{id}/bind) binds a gateway key
// to a team. Also binds in the ledger for budget enforcement.
func (s *Server) handleOrgTeamBind(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureOrgStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "org store unavailable: "+err.Error())
		return
	}
	var in orgBindInput
	// AUD-SEC-003: bound the body before decoding, like every other handler.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
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
	// Also bind in the ledger for team budget enforcement (unifies dual Team model)
	if s.ledger != nil {
		if err := s.ledger.AssignKeyToTeam(r.Context(), in.KeyID, teamID, time.Now()); err != nil {
			// Rollback org store binding on ledger failure
			_ = st.UnbindKey(in.KeyID, org.RoleOwner)
			writeError(w, http.StatusBadRequest, "ledger bind failed: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"key_id": in.KeyID, "team_id": teamID})
}

// handleOrgTeamUnbind (POST /api/org/teams/{id}/unbind?key_id=...) removes
// a key's team binding. Also unbinds from the ledger.
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
	// Also unbind from the ledger (unifies dual Team model)
	if s.ledger != nil {
		if _, err := s.ledger.UnassignKeyFromTeam(r.Context(), keyID, time.Now()); err != nil {
			// Log but don't fail - the org store binding is already removed
			s.logger.Printf("org unbind: ledger unassign failed for %s: %v", keyID, err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"key_id": keyID, "unbound": true})
}

// handleOrgAnalytics (GET /api/org/analytics) returns organization-level
// usage analytics across all teams.
func (s *Server) handleOrgAnalytics(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	ctx := r.Context()
	now := time.Now()
	// Get all teams and their budget status
	teams, err := s.ledger.ListTeams(ctx)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not list teams")
		return
	}
	type orgTeamAnalytics struct {
		TeamID               string    `json:"team_id"`
		TeamName             string    `json:"team_name"`
		MaxBudgetUSD         *float64  `json:"max_budget_usd,omitempty"`
		BudgetDuration       string    `json:"budget_duration,omitempty"`
		WindowStart          time.Time `json:"window_start"`
		WindowEnd            time.Time `json:"window_end"`
		EstimatedListCostUSD float64   `json:"estimated_list_cost_usd"`
		MeteredCostUSD       float64   `json:"metered_cost_usd"`
		HasUnpricedUsage     bool      `json:"has_unpriced_usage"`
		Exceeded             bool      `json:"exceeded"`
		RequestCount         int       `json:"request_count"`
		TotalTokens          int       `json:"total_tokens"`
	}
	var out []orgTeamAnalytics
	totalRequests := 0
	totalTokens := 0
	totalMeteredCost := 0.0
	totalListCost := 0.0
	for _, team := range teams {
		if team.Status == "archived" {
			continue
		}
		v := orgTeamAnalytics{
			TeamID:         team.ID,
			TeamName:       team.Name,
			MaxBudgetUSD:   team.MaxBudgetUSD,
			BudgetDuration: team.BudgetDuration,
		}
		if team.MaxBudgetUSD != nil {
			st, err := s.teamStatus(ctx, team, now)
			if err != nil {
				s.logger.Printf("org analytics: team status failed for %s: %v", team.ID, err)
			} else {
				v.WindowStart = st.WindowStart
				v.WindowEnd = st.WindowEnd
				v.EstimatedListCostUSD = st.EstimatedListCostUSD
				v.MeteredCostUSD = st.MeteredCostUSD
				v.HasUnpricedUsage = st.HasUnpricedUsage
				v.Exceeded = st.Exceeded
			}
		}
		// Get usage slices for request/token counts
		if team.MaxBudgetUSD != nil {
			advanced, _ := s.ledger.AdvanceTeamBudgetWindow(ctx, team, now)
			start, end := ledger.TeamWindow(advanced)
			slices, err := s.ledger.UsageSlicesForTeam(ctx, team.ID, start, end)
			if err == nil {
				for _, sl := range slices {
					v.RequestCount += sl.Rows
					v.TotalTokens += sl.TotalTokens
					totalRequests += sl.Rows
					totalTokens += sl.TotalTokens
					totalMeteredCost += sl.CostUSD
				}
			}
		}
		out = append(out, v)
	}
	response := map[string]any{
		"teams":             out,
		"total_requests":    totalRequests,
		"total_tokens":      totalTokens,
		"total_metered_usd": totalMeteredCost,
		"total_list_usd":    totalListCost,
	}
	writeJSON(w, http.StatusOK, response)
}

// handleOrgTeamAnalytics (GET /api/org/teams/{id}/analytics) returns
// team-level usage analytics for a specific team.
func (s *Server) handleOrgTeamAnalytics(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	teamID := r.PathValue("id")
	if teamID == "" {
		writeError(w, http.StatusBadRequest, "team id required")
		return
	}
	ctx := r.Context()
	now := time.Now()
	team, err := s.ledger.GetTeam(ctx, teamID)
	if err != nil || team == nil {
		writeError(w, http.StatusNotFound, "team not found")
		return
	}
	v := map[string]any{
		"team_id":   team.ID,
		"team_name": team.Name,
		"status":    team.Status,
	}
	if team.MaxBudgetUSD != nil {
		v["max_budget_usd"] = *team.MaxBudgetUSD
		v["budget_duration"] = team.BudgetDuration
		st, err := s.teamStatus(ctx, *team, now)
		if err == nil {
			v["window_start"] = st.WindowStart
			v["window_end"] = st.WindowEnd
			v["estimated_list_cost_usd"] = st.EstimatedListCostUSD
			v["metered_cost_usd"] = st.MeteredCostUSD
			v["has_unpriced_usage"] = st.HasUnpricedUsage
			v["exceeded"] = st.Exceeded
		}
		// Per-member breakdown
		members, err := s.ledger.TeamMembers(ctx, teamID)
		if err == nil {
			memberViews := []map[string]any{}
			for _, m := range members {
				mv := map[string]any{
					"key_id":      m.KeyID,
					"assigned_at": m.AssignedAt,
				}
				memberViews = append(memberViews, mv)
			}
			v["members"] = memberViews
		}
	}
	writeJSON(w, http.StatusOK, v)
}

// registerOrgRoutes wires the P3-A management API (guard rows exist).
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
	s.register(mux, "GET", "/api/org/analytics", s.handleOrgAnalytics)
	s.register(mux, "GET", "/api/org/teams/{id}/analytics", s.handleOrgTeamAnalytics)
}
