package server

// Team cost centers (M5.12): the server side — the pre-call gate,
// the dashboard GET/POST feeds (including membership), and the
// caller's team section in me/status.
//
// The budget is measured on ESTIMATED LIST cost (the team's usage
// in its window repriced at catalog prices), exactly as the sector-typical
// getCommittedTeamEstimatedListCostUsd does; the metered cost is
// reported alongside it but never decides the budget. Any unpriced
// usage in the window fails closed (exceeded), because a team whose
// usage cannot be priced cannot prove it is under budget. An
// exceeded team refuses with 400 and the sector-typical message, in the
// policy chain right after the internal-budget gate.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/org"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// teamBudgetStatus is one team's computed budget state in its
// current window.
type teamBudgetStatus struct {
	Team                 ledger.Team
	WindowStart          time.Time
	WindowEnd            time.Time
	EstimatedListCostUSD float64
	MeteredCostUSD       float64
	HasUnpricedUsage     bool
	Exceeded             bool
}

// teamStatus advances the team's window (rolling it forward when it
// has passed, persisting the move) and reprices the window's usage
// at catalog list prices. The window is [WindowStart, WindowEnd).
func (s *Server) teamStatus(ctx context.Context, team ledger.Team, now time.Time) (teamBudgetStatus, error) {
	st := teamBudgetStatus{Team: team}
	if team.MaxBudgetUSD == nil {
		return st, nil
	}
	advanced, err := s.ledger.AdvanceTeamBudgetWindow(ctx, team, now)
	if err != nil {
		return st, err
	}
	st.Team = advanced
	st.WindowStart, st.WindowEnd = ledger.TeamWindow(advanced)
	slices, err := s.ledger.UsageSlicesForTeam(ctx, team.ID, st.WindowStart, st.WindowEnd)
	if err != nil {
		return st, err
	}
	prices := s.catalogListPrices(ctx)
	for _, sl := range slices {
		st.MeteredCostUSD += sl.CostUSD
		if !sl.PriceKnown || !sl.SplitKnown {
			st.HasUnpricedUsage = true
			continue
		}
		p, ok := prices[sl.Model]
		if !ok {
			st.HasUnpricedUsage = true
			continue
		}
		st.EstimatedListCostUSD += (float64(sl.PromptTokens)*p[0] + float64(sl.CompletionTokens)*p[1]) / 1e6
	}
	st.Exceeded = st.EstimatedListCostUSD >= *team.MaxBudgetUSD || st.HasUnpricedUsage
	return st, nil
}

// checkTeamBudget is the team-budget gate: the key's active team,
// refused with 400 when its shared budget is exceeded. It returns
// false when it already wrote a refusal. A ledger error fails
// closed (503); a key with no team, or a team with no budget,
// passes.
func (s *Server) checkTeamBudget(w http.ResponseWriter, r *http.Request) bool {
	if s.ledger == nil {
		return true
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return true
	}
	team, err := s.ledger.ActiveTeamForKey(r.Context(), keyID)
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, "team budget check failed")
		return false
	}
	if team == nil || team.MaxBudgetUSD == nil {
		return true
	}
	st, err := s.teamStatus(r.Context(), *team, time.Now())
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, "team budget check failed")
		return false
	}
	if !st.Exceeded {
		return true
	}
	writeJSON(w, http.StatusBadRequest, errorBody{Error: errorDetail{Message: fmt.Sprintf("This API key's team reached its shared usage quota. The soft committed-usage window resets at %s.", st.WindowEnd.Format(time.RFC3339)), Type: "team_budget"}})
	return false
}

// routingStrategy reports the routing strategy a chat-shaped request
// asks for (the contract's Sort field), or "" when none is declared.
// Used so every dialect's policy gate reads the same field the engine
// will honour.
func routingStrategy(req *provider.ChatRequest) string {
	if req == nil || req.Routing == nil {
		return ""
	}
	return req.Routing.Sort
}

// checkTeamPolicy enforces org/team policy on the chat path: cross-team
// isolation plus the team's model and strategy allowlists, resolved from
// the caller's gateway key through the org store. It is a thin wrapper
// over enforceTeamPolicy so every dialect shares ONE decision path.
func (s *Server) checkTeamPolicy(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) bool {
	model := ""
	if req != nil {
		model = req.Model
	}
	return s.enforceTeamPolicy(w, r, model, routingStrategy(req))
}

// enforceTeamPolicy is the single org/team policy gate (P1.6). It runs
// BEFORE provider dispatch on every model-addressed dialect — chat
// completions, Anthropic Messages, Responses, legacy completions, Gemini
// generateContent, embeddings, images, audio, media verification, batch
// items and A2A tasks — so no alternate dialect is a policy bypass.
//
// Resolution is by the AUTHENTICATED key: the org store's binding for
// the caller's gateway key decides the team, and the team decides
// what the request may do. A key with no binding, or a deployment with
// no org store, is unrestricted (no policy was declared, so none is
// invented).
//
// Checks, in order:
//  1. model allowlist — a team with a non-empty list refuses any other
//     requested model (403 team_policy).
//  2. strategy allowlist — same, for the requested routing strategy.
//  3. org budget — the organization's declared budget compared against
//     the estimated list cost of its teams' usage in each team's
//     current window (400 org_budget). Unpriced usage in the window
//     fails closed, exactly like the team gate: usage that cannot be
//     priced cannot prove the org is under budget.
//
// Every refusal is written to the store's append-only audit log
// (action request.denied) with the key, team and reason, so a denial
// survives a restart. Returns false when it already wrote a refusal.
func (s *Server) enforceTeamPolicy(w http.ResponseWriter, r *http.Request, model, strategy string) bool {
	ref := s.evaluateTeamPolicy(r.Context(), s.callerKeyID(r), model, strategy)
	if ref == nil {
		return true
	}
	if ref.Status >= http.StatusInternalServerError {
		writeServerError(w, ref.Status, ref.Message)
		return false
	}
	writeJSON(w, ref.Status, errorBody{Error: errorDetail{Message: ref.Message, Type: ref.Type}})
	return false
}

// teamPolicyRefusal is one policy decision's refusal: the status, the
// message and the error type the caller should see. Detail is the
// audit-log line (it may name the model/strategy, which the client
// message already does).
type teamPolicyRefusal struct {
	Status  int
	Message string
	Type    string
	Detail  string
}

// evaluateTeamPolicy is the key-addressed core of the gate: it resolves
// the org store's binding for keyID and returns nil when the request is
// allowed, or the refusal to report. Callers that have no
// http.Request (batch workers, A2A task runners) use this directly so
// they enforce exactly the same policy as the HTTP handlers. Every
// refusal is audited.
func (s *Server) evaluateTeamPolicy(ctx context.Context, keyID, model, strategy string) *teamPolicyRefusal {
	st, err := s.ensureOrgStore()
	if err != nil || st == nil {
		return nil
	}
	if keyID == "" {
		return nil
	}
	team := st.TeamForKey(keyID)
	if team == nil {
		return nil
	}
	if model != "" && !st.AllowModel(team.ID, model) {
		st.RecordDenial(keyID, team.ID, "model "+model+" denied by team allowlist")
		return &teamPolicyRefusal{
			Status:  http.StatusForbidden,
			Message: fmt.Sprintf("model %q not allowed for this team", model),
			Type:    "team_policy",
		}
	}
	if strategy != "" && !st.AllowStrategy(team.ID, strategy) {
		st.RecordDenial(keyID, team.ID, "strategy "+strategy+" denied by team allowlist")
		return &teamPolicyRefusal{
			Status:  http.StatusForbidden,
			Message: fmt.Sprintf("routing strategy %q not allowed for this team", strategy),
			Type:    "team_policy",
		}
	}
	org := st.OrgOf(team.ID)
	if org == nil || org.DefaultBudgetUSD <= 0 || s.ledger == nil {
		return nil
	}
	spent, unpriced, err := s.orgWindowListCost(ctx, org)
	if err != nil {
		// Fail closed: an org whose spend cannot be read cannot
		// prove it is under budget.
		return &teamPolicyRefusal{Status: http.StatusServiceUnavailable, Message: "org budget check failed", Type: "org_budget"}
	}
	if unpriced || spent >= org.DefaultBudgetUSD {
		st.RecordDenial(keyID, team.ID, fmt.Sprintf("org %s budget: spent %.6f unpriced=%v budget %.6f", org.ID, spent, unpriced, org.DefaultBudgetUSD))
		return &teamPolicyRefusal{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("This API key's organization reached its shared usage budget (%s).", org.ID),
			Type:    "org_budget",
		}
	}
	return nil
}

// orgBudgetWindow is the window an organization budget is measured
// over: the current calendar month, UTC. An organization budget is a
// monthly cap (see config docs for org budgets), so it does not borrow
// a member team's rolling window — mixing per-team windows into one org
// figure would compare spend from different periods.
func orgBudgetWindow(now time.Time) (start, end time.Time) {
	start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	end = start.AddDate(0, 1, 0)
	return start, end
}

// orgWindowListCost sums the estimated LIST cost of every team under an
// org over the org's budget window (see orgBudgetWindow), from the
// ledger. It is a read: nothing is written, and a team the ledger does
// not know simply contributes nothing. unpriced is true when any usage
// in the window cannot be repriced at catalog prices — the caller fails
// closed on it, because a list figure that omits unpriced usage is a lie
// by omission.
func (s *Server) orgWindowListCost(ctx context.Context, org *org.Organization) (spentUSD float64, unpriced bool, err error) {
	if s.ledger == nil {
		return 0, false, nil
	}
	st, err := s.ensureOrgStore()
	if err != nil || st == nil {
		return 0, false, err
	}
	prices := s.catalogListPrices(ctx)
	start, end := orgBudgetWindow(time.Now())
	for _, team := range st.TeamsForOrg(org.ID) {
		slices, err := s.ledger.UsageSlicesForTeam(ctx, team.ID, start, end)
		if err != nil {
			return 0, false, err
		}
		for _, sl := range slices {
			if !sl.PriceKnown || !sl.SplitKnown {
				unpriced = true
				continue
			}
			p, ok := prices[sl.Model]
			if !ok {
				unpriced = true
				continue
			}
			spentUSD += (float64(sl.PromptTokens)*p[0] + float64(sl.CompletionTokens)*p[1]) / 1e6
		}
	}
	return spentUSD, unpriced, nil
}

// teamMemberView is one member row of the cost-center report:
// the member's usage inside the team's current window, repriced
// at catalog prices. ListCostUSD is nil when any of the member's
// usage is unpriced (the list figure would be a lie by omission).
type teamMemberView struct {
	KeyID            string    `json:"key_id"`
	AssignedAt       time.Time `json:"assigned_at"`
	Requests         int       `json:"requests"`
	Tokens           int       `json:"tokens"`
	MeteredCostUSD   float64   `json:"metered_cost_usd"`
	ListCostUSD      *float64  `json:"list_cost_usd,omitempty"`
	HasUnpricedUsage bool      `json:"has_unpriced_usage"`
}

// teamView is one team row of GET /dashboard/teams.
type teamView struct {
	ID                   string           `json:"id"`
	Name                 string           `json:"name"`
	Description          string           `json:"description,omitempty"`
	Status               string           `json:"status"`
	MaxBudgetUSD         *float64         `json:"max_budget_usd,omitempty"`
	BudgetDuration       string           `json:"budget_duration,omitempty"`
	WindowStart          *time.Time       `json:"window_start_at,omitempty"`
	WindowEnd            *time.Time       `json:"window_end_at,omitempty"`
	EstimatedListCostUSD float64          `json:"estimated_list_cost_usd"`
	MeteredCostUSD       float64          `json:"metered_cost_usd"`
	HasUnpricedUsage     bool             `json:"has_unpriced_usage"`
	Exceeded             bool             `json:"exceeded"`
	Members              []teamMemberView `json:"members"`
}

type teamsResponse struct {
	Teams []teamView `json:"teams"`
}

// handleTeams serves GET /dashboard/teams: every team with its
// current budget window, list + metered cost, and the per-member
// breakdown. A team whose status read fails is listed with its
// stored fields and zeros, and the failure is logged — one bad
// team must not hide the rest.
func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "teams unavailable: no ledger wired")
		return
	}
	ctx := r.Context()
	now := time.Now()
	teams, err := s.ledger.ListTeams(ctx)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not list teams")
		return
	}
	prices := s.catalogListPrices(ctx)
	out := teamsResponse{Teams: []teamView{}}
	for _, team := range teams {
		v := teamView{
			ID: team.ID, Name: team.Name, Description: team.Description,
			Status: team.Status, MaxBudgetUSD: team.MaxBudgetUSD,
			BudgetDuration: team.BudgetDuration, Members: []teamMemberView{},
		}
		members, err := s.ledger.TeamMembers(ctx, team.ID)
		if err != nil {
			s.logger.Printf("teams: members failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
		}
		for _, m := range members {
			v.Members = append(v.Members, teamMemberView{KeyID: m.KeyID, AssignedAt: m.AssignedAt})
		}
		if team.MaxBudgetUSD == nil {
			out.Teams = append(out.Teams, v)
			continue
		}
		st, err := s.teamStatus(ctx, team, now)
		if err != nil {
			s.logger.Printf("teams: status failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
			out.Teams = append(out.Teams, v)
			continue
		}
		v.WindowStart = &st.WindowStart
		v.WindowEnd = &st.WindowEnd
		v.EstimatedListCostUSD = st.EstimatedListCostUSD
		v.MeteredCostUSD = st.MeteredCostUSD
		v.HasUnpricedUsage = st.HasUnpricedUsage
		v.Exceeded = st.Exceeded
		// Per-member breakdown, repriced at the same catalog
		// prices as the team total.
		slices, err := s.ledger.TeamMemberSlices(ctx, team.ID, st.WindowStart, st.WindowEnd)
		if err != nil {
			s.logger.Printf("teams: member slices failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
			out.Teams = append(out.Teams, v)
			continue
		}
		byKey := map[string]*teamMemberView{}
		for i := range v.Members {
			byKey[v.Members[i].KeyID] = &v.Members[i]
		}
		for _, sl := range slices {
			mv, ok := byKey[sl.KeyID]
			if !ok {
				// The key left the team after spending: its
				// rows still count toward the team total, but
				// it is not a current member, so it has no row
				// in the breakdown.
				continue
			}
			mv.Requests += sl.Rows
			mv.Tokens += sl.TotalTokens
			mv.MeteredCostUSD += sl.CostUSD
			if !sl.PriceKnown || !sl.SplitKnown {
				mv.HasUnpricedUsage = true
				continue
			}
			p, ok := prices[sl.Model]
			if !ok {
				mv.HasUnpricedUsage = true
				continue
			}
			list := (float64(sl.PromptTokens)*p[0] + float64(sl.CompletionTokens)*p[1]) / 1e6
			if mv.ListCostUSD == nil {
				zero := 0.0
				mv.ListCostUSD = &zero
			}
			*mv.ListCostUSD += list
		}
		out.Teams = append(out.Teams, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// teamUpdate is the POST /dashboard/teams body. Absent fields keep
// their stored values (a new team starts from the defaults);
// max_budget_usd and budget_duration must be set or cleared
// together. Membership is NOT changed here — use
// POST /dashboard/teams/{id}/members.
type teamUpdate struct {
	ID             string   `json:"id"`
	Name           *string  `json:"name"`
	Description    *string  `json:"description"`
	Status         *string  `json:"status"`
	MaxBudgetUSD   *float64 `json:"max_budget_usd"`
	BudgetDuration *string  `json:"budget_duration"`
	Delete         bool     `json:"delete"`
}

func teamConfigToLedger(c config.TeamConfig) ledger.Team {
	return ledger.Team{
		ID: c.ID, Name: c.Name, Description: c.Description, Status: c.Status,
		MaxBudgetUSD: c.MaxBudgetUSD, BudgetDuration: c.BudgetDuration,
	}
}

func teamLedgerToConfig(t ledger.Team, members []string) config.TeamConfig {
	return config.TeamConfig{
		ID: t.ID, Name: t.Name, Description: t.Description, Status: t.Status,
		MaxBudgetUSD: t.MaxBudgetUSD, BudgetDuration: t.BudgetDuration,
		Members: members,
	}
}

func (s *Server) validateTeamUpdate(next config.TeamConfig) error {
	if next.Name == "" {
		return fmt.Errorf("name is required")
	}
	if next.MaxBudgetUSD != nil && *next.MaxBudgetUSD <= 0 {
		return fmt.Errorf("max_budget_usd must be positive (got %v)", *next.MaxBudgetUSD)
	}
	if next.MaxBudgetUSD == nil && next.BudgetDuration != "" {
		return fmt.Errorf("budget_duration requires a max_budget_usd")
	}
	if next.MaxBudgetUSD != nil && next.BudgetDuration == "" {
		return fmt.Errorf("max_budget_usd requires a budget_duration")
	}
	switch next.BudgetDuration {
	case "", "1d", "7d", "30d":
	default:
		return fmt.Errorf("budget_duration %q is not a known duration (1d, 7d, 30d)", next.BudgetDuration)
	}
	if next.Status != "" && next.Status != "active" && next.Status != "archived" {
		return fmt.Errorf("status %q must be active or archived", next.Status)
	}
	return nil
}

// handleTeamUpdate serves POST /dashboard/teams: create, update
// (full-row merge over the stored team) or delete a team. The
// ledger is written first, then the config file; a persist failure
// rolls the ledger back, so runtime and file never disagree.
// Membership is preserved across updates: the config entry keeps
// the ledger's current members.
func (s *Server) handleTeamUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "teams unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update teamUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid team update: expected {\"id\": string, \"name\": string, \"description\": string, \"status\": string, \"max_budget_usd\": number, \"budget_duration\": string, \"delete\": bool}")
		return
	}
	if update.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	prev, prevErr := s.ledger.GetTeam(ctx, update.ID)
	if prevErr != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read team")
		return
	}
	hadPrev := prev != nil
	if update.Delete {
		if err := s.ledger.DeleteTeam(ctx, update.ID); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not delete team")
			return
		}
	} else {
		next := config.TeamConfig{ID: update.ID}
		if hadPrev {
			members, _ := s.ledger.TeamMembers(ctx, update.ID)
			ids := make([]string, 0, len(members))
			for _, m := range members {
				ids = append(ids, m.KeyID)
			}
			next = teamLedgerToConfig(*prev, ids)
		}
		if update.Name != nil {
			next.Name = *update.Name
		}
		if update.Description != nil {
			next.Description = *update.Description
		}
		if update.MaxBudgetUSD != nil {
			next.MaxBudgetUSD = update.MaxBudgetUSD
		}
		if update.BudgetDuration != nil {
			next.BudgetDuration = *update.BudgetDuration
		}
		if update.Status != nil {
			next.Status = *update.Status
		}
		if err := s.validateTeamUpdate(next); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		team := teamConfigToLedger(next)
		if update.Status != nil {
			team.Status = *update.Status
		} else if hadPrev {
			team.Status = prev.Status
		}
		// A budget change resets the window: the new cap starts
		// now, it does not inherit the old window's spent total.
		if hadPrev && (prev.MaxBudgetUSD == nil) != (team.MaxBudgetUSD == nil) ||
			hadPrev && prev.MaxBudgetUSD != nil && team.MaxBudgetUSD != nil && *prev.MaxBudgetUSD != *team.MaxBudgetUSD ||
			hadPrev && prev.BudgetDuration != team.BudgetDuration {
			team.BudgetResetAt = nil
		} else if hadPrev {
			team.BudgetResetAt = prev.BudgetResetAt
		}
		if err := s.ledger.SetTeam(ctx, team); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save team")
			return
		}
	}
	// Config block: keep the file in step with the ledger, or
	// roll the ledger back when the save fails.
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.TeamConfig(nil), s.cfg.Teams...)
		if update.Delete {
			kept := s.cfg.Teams[:0]
			for _, t := range s.cfg.Teams {
				if t.ID != update.ID {
					kept = append(kept, t)
				}
			}
			s.cfg.Teams = kept
		} else {
			next := config.TeamConfig{ID: update.ID}
			for _, t := range s.cfg.Teams {
				if t.ID == update.ID {
					next = t
					break
				}
			}
			if update.Name != nil {
				next.Name = *update.Name
			}
			if update.Description != nil {
				next.Description = *update.Description
			}
			if update.MaxBudgetUSD != nil {
				next.MaxBudgetUSD = update.MaxBudgetUSD
			}
			if update.BudgetDuration != nil {
				next.BudgetDuration = *update.BudgetDuration
			}
			if update.Status != nil {
				next.Status = *update.Status
			}
			found := false
			for i, t := range s.cfg.Teams {
				if t.ID == update.ID {
					s.cfg.Teams[i] = next
					found = true
					break
				}
			}
			if !found {
				s.cfg.Teams = append(s.cfg.Teams, next)
			}
		}
		if err := s.configPersist(); err != nil {
			s.cfg.Teams = prevCfg
			if hadPrev {
				_ = s.ledger.SetTeam(ctx, *prev)
			} else {
				_ = s.ledger.DeleteTeam(ctx, update.ID)
			}
			writeServerError(w, http.StatusInternalServerError, "could not persist team config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// teamMemberUpdate is the POST /dashboard/teams/{id}/members body.
type teamMemberUpdate struct {
	KeyID  string `json:"key_id"`
	Action string `json:"action"` // "assign" | "unassign"
}

// handleTeamMembers serves POST /dashboard/teams/{id}/members:
// assign a key to the team, or unassign it (the key must currently
// belong to this team). The ledger is written first, then the
// config file; a persist failure rolls the ledger binding back.
func (s *Server) handleTeamMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "teams unavailable: no ledger wired")
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeError(w, http.StatusBadRequest, "team id is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update teamMemberUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid team member update: expected {\"key_id\": string, \"action\": \"assign\"|\"unassign\"}")
		return
	}
	if update.KeyID == "" {
		writeError(w, http.StatusBadRequest, "key_id is required")
		return
	}
	ctx := r.Context()
	now := time.Now()
	prevTeam, _ := s.ledger.ActiveTeamForKey(ctx, update.KeyID)
	switch update.Action {
	case "assign":
		if err := s.ledger.AssignKeyToTeam(ctx, update.KeyID, teamID, now); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	case "unassign":
		if prevTeam == nil || prevTeam.ID != teamID {
			writeError(w, http.StatusBadRequest, "key is not a member of this team")
			return
		}
		if _, err := s.ledger.UnassignKeyFromTeam(ctx, update.KeyID, now); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not unassign key from team")
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "action must be \"assign\" or \"unassign\"")
		return
	}
	// Config block: the team's member list in the file follows
	// the ledger; a persist failure rolls the binding back.
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.TeamConfig(nil), s.cfg.Teams...)
		for i, t := range s.cfg.Teams {
			if t.ID != teamID {
				continue
			}
			kept := t.Members[:0]
			for _, m := range t.Members {
				if m != update.KeyID {
					kept = append(kept, m)
				}
			}
			t.Members = kept
			if update.Action == "assign" {
				t.Members = append(t.Members, update.KeyID)
			}
			s.cfg.Teams[i] = t
		}
		if err := s.configPersist(); err != nil {
			s.cfg.Teams = prevCfg
			if prevTeam == nil {
				_, _ = s.ledger.UnassignKeyFromTeam(ctx, update.KeyID, now)
			} else {
				_ = s.ledger.AssignKeyToTeam(ctx, update.KeyID, prevTeam.ID, now)
			}
			writeServerError(w, http.StatusInternalServerError, "could not persist team config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// meStatusTeam is the caller's team section of GET /v1/me/status.
type meStatusTeam struct {
	TeamID               string    `json:"team_id"`
	TeamName             string    `json:"team_name"`
	MaxBudgetUSD         *float64  `json:"max_budget_usd,omitempty"`
	BudgetDuration       string    `json:"budget_duration,omitempty"`
	WindowStart          time.Time `json:"window_start_at"`
	WindowEnd            time.Time `json:"window_end_at"`
	EstimatedListCostUSD float64   `json:"estimated_list_cost_usd"`
	MeteredCostUSD       float64   `json:"metered_cost_usd"`
	HasUnpricedUsage     bool      `json:"has_unpriced_usage"`
	Exceeded             bool      `json:"exceeded"`
}
