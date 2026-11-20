package server

// Teams management API (M8.18): POST/GET /api/teams and
// GET/PATCH/DELETE /api/teams/{id}.
//
// This is a management face over the M5.12 team cost centers that
// already exist in the ledger (internal/ledger/team.go) and the
// config file (config.TeamConfig). No second store is invented:
// every read and write here goes through the same ledger rows the
// dashboard feeds (handleTeams/handleTeamUpdate) and the pre-call
// budget gate (checkTeamBudget) use, so a budget set here is
// enforced by that gate on the chat paths, not merely stored.
//
// Auth is NOT checked here: like handleUsageBudget, these handlers
// assume the guard table has already enforced management auth.
// Registration (guard.go route table + server.go register) is the
// coordinator's job; the proposed tier is in the file header of
// each handler below and in the slice report: Tier 3 MANAGEMENT
// with the admin scope on all ten M8.18 routes (they carry no
// credentials and cannot redirect traffic, so they are not Tier 2
// ALWAYS_PROTECTED like the provider add/delete routes).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// apiTeamView builds one team's API row: the stored fields plus
// the live budget window, list + metered cost, and the per-member
// breakdown — the same shape as the dashboard's teamView, so the
// two feeds can never disagree about what a team costs.
func (s *Server) apiTeamView(ctx context.Context, team ledger.Team, now time.Time) teamView {
	v := teamView{
		ID: team.ID, Name: team.Name, Description: team.Description,
		Status: team.Status, MaxBudgetUSD: team.MaxBudgetUSD,
		BudgetDuration: team.BudgetDuration, Members: []teamMemberView{},
	}
	members, err := s.ledger.TeamMembers(ctx, team.ID)
	if err != nil {
		s.logger.Printf("api/teams: members failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
		return v
	}
	for _, m := range members {
		v.Members = append(v.Members, teamMemberView{KeyID: m.KeyID, AssignedAt: m.AssignedAt})
	}
	if team.MaxBudgetUSD == nil {
		return v
	}
	st, err := s.teamStatus(ctx, team, now)
	if err != nil {
		s.logger.Printf("api/teams: status failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
		return v
	}
	v.WindowStart = &st.WindowStart
	v.WindowEnd = &st.WindowEnd
	v.EstimatedListCostUSD = st.EstimatedListCostUSD
	v.MeteredCostUSD = st.MeteredCostUSD
	v.HasUnpricedUsage = st.HasUnpricedUsage
	v.Exceeded = st.Exceeded
	prices := s.catalogListPrices(ctx)
	slices, err := s.ledger.TeamMemberSlices(ctx, team.ID, st.WindowStart, st.WindowEnd)
	if err != nil {
		s.logger.Printf("api/teams: member slices failed for %s: %v", team.ID, guardrails.RedactLogLine(err.Error()))
		return v
	}
	byKey := map[string]*teamMemberView{}
	for i := range v.Members {
		byKey[v.Members[i].KeyID] = &v.Members[i]
	}
	for _, sl := range slices {
		mv, ok := byKey[sl.KeyID]
		if !ok {
			continue // left the team after spending: counts in the total, no member row
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
	return v
}

// apiTeamCreate is the POST /api/teams body. id is optional —
// when absent the server generates one (team-<hex>). members are
// gateway key ids; each must exist in the config (when one is
// wired) and is assigned through the ledger's AssignKeyToTeam, so
// an archived team or a key already on another team follows the
// ledger's own rules.
type apiTeamCreate struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Status         string   `json:"status,omitempty"`
	MaxBudgetUSD   *float64 `json:"max_budget_usd,omitempty"`
	BudgetDuration string   `json:"budget_duration,omitempty"`
	Members        []string `json:"members,omitempty"`
}

// newAPIID mints a server-generated resource id (team-<8 hex> or
// pool-<8 hex>). crypto/rand failure is a 500, never a guessable
// or empty id.
func newAPIID(prefix string) (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("cannot generate id: %w", err)
	}
	return prefix + hex.EncodeToString(b), nil
}

// handleAPITeams serves POST /api/teams (create, 201) and
// GET /api/teams (list, 200). Proposed tier: Tier 3 MANAGEMENT,
// admin scope (see file header).
func (s *Server) handleAPITeams(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "teams unavailable: no ledger wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPITeamsList(w, r)
	case http.MethodPost:
		s.handleAPITeamsCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAPITeamsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	teams, err := s.ledger.ListTeams(ctx)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not list teams")
		return
	}
	out := teamsResponse{Teams: []teamView{}}
	for _, team := range teams {
		out.Teams = append(out.Teams, s.apiTeamView(ctx, team, now))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPITeamsCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var create apiTeamCreate
	if err := json.NewDecoder(r.Body).Decode(&create); err != nil {
		writeError(w, http.StatusBadRequest, "invalid team: expected {\"id\": string, \"name\": string, \"description\": string, \"status\": \"active\"|\"archived\", \"max_budget_usd\": number, \"budget_duration\": \"1d\"|\"7d\"|\"30d\", \"members\": [key_id]}")
		return
	}
	ctx := r.Context()
	if create.ID == "" {
		id, err := newAPIID("team-")
		if err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not create team")
			return
		}
		create.ID = id
	}
	if prev, err := s.ledger.GetTeam(ctx, create.ID); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read team")
		return
	} else if prev != nil {
		writeError(w, http.StatusConflict, "team "+create.ID+" already exists")
		return
	}
	if err := s.validateAPPTeamMembers(create.Members); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if create.Status == "archived" && len(create.Members) > 0 {
		writeError(w, http.StatusBadRequest, "members cannot be assigned to an archived team")
		return
	}
	next := config.TeamConfig{
		ID: create.ID, Name: create.Name, Description: create.Description,
		Status: create.Status, MaxBudgetUSD: create.MaxBudgetUSD,
		BudgetDuration: create.BudgetDuration, Members: create.Members,
	}
	if err := s.validateTeamUpdate(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	team := teamConfigToLedger(next)
	if err := s.ledger.SetTeam(ctx, team); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save team")
		return
	}
	now := time.Now()
	for _, keyID := range create.Members {
		if err := s.ledger.AssignKeyToTeam(ctx, keyID, create.ID, now); err != nil {
			// Roll the team back: a half-membered team must
			// never be left behind by a failed create.
			_ = s.ledger.DeleteTeam(ctx, create.ID)
			writeServerError(w, http.StatusInternalServerError, "could not assign team members")
			return
		}
	}
	if !s.persistAPITeamConfig(ctx, create.ID, next, nil) {
		// persistAPITeamConfig rolled the ledger back; the
		// caller answers the 500.
		writeServerError(w, http.StatusInternalServerError, "could not persist team config")
		return
	}
	stored, err := s.ledger.GetTeam(ctx, create.ID)
	if err != nil || stored == nil {
		writeServerError(w, http.StatusInternalServerError, "could not read created team")
		return
	}
	writeJSON(w, http.StatusCreated, s.apiTeamView(ctx, *stored, time.Now()))
}

// validateAPPTeamMembers rejects a member list naming a gateway
// key that does not exist (when a config is wired), naming the
// offending key id. With no config wired there is nothing to
// check against and any id passes — the ledger binding itself
// does not verify key existence.
func (s *Server) validateAPPTeamMembers(members []string) error {
	if s.cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, keyID := range members {
		if keyID == "" {
			return fmt.Errorf("members must not contain an empty key_id")
		}
		if seen[keyID] {
			return fmt.Errorf("members names key %q twice", keyID)
		}
		seen[keyID] = true
		if _, ok := s.cfg.FindGatewayKey(keyID); !ok {
			return fmt.Errorf("members names a gateway key that does not exist (%q)", keyID)
		}
	}
	return nil
}

// persistAPITeamConfig writes one team row into the config file
// (when a config + persist function are wired) and rolls the
// ledger back when the save fails. prev is the pre-change team
// (nil for a create); when non-nil the rollback restores it and
// its members. It returns false when the save failed — it does
// NOT answer the request itself; the caller writes the 500.
// With no config wired it returns true: the ledger is the durable
// store.
func (s *Server) persistAPITeamConfig(ctx context.Context, id string, next config.TeamConfig, prev *ledger.Team) bool {
	if s.cfg == nil || s.configPersist == nil {
		return true
	}
	prevCfg := append([]config.TeamConfig(nil), s.cfg.Teams...)
	found := false
	for i, t := range s.cfg.Teams {
		if t.ID == id {
			s.cfg.Teams[i] = next
			found = true
			break
		}
	}
	if !found {
		s.cfg.Teams = append(s.cfg.Teams, next)
	}
	if err := s.configPersist(); err != nil {
		s.cfg.Teams = prevCfg
		if prev == nil {
			_ = s.ledger.DeleteTeam(ctx, id)
		} else {
			_ = s.ledger.SetTeam(ctx, *prev)
			// Best-effort membership restore: the team row
			// is back, but bindings closed by the change
			// cannot be perfectly rewound — a persist
			// failure is a 500 and the operator re-reads
			// the team to see what actually stuck.
			members, _ := s.ledger.TeamMembers(ctx, id)
			now := time.Now()
			for _, m := range members {
				_, _ = s.ledger.UnassignKeyFromTeam(ctx, m.KeyID, now)
			}
			for _, m := range prevCfgMembers(prevCfg, id) {
				_ = s.ledger.AssignKeyToTeam(ctx, m, id, now)
			}
		}
		return false
	}
	return true
}

func prevCfgMembers(cfg []config.TeamConfig, id string) []string {
	for _, t := range cfg {
		if t.ID == id {
			return t.Members
		}
	}
	return nil
}

// handleAPITeam serves GET /api/teams/{id} (200), PATCH
// /api/teams/{id} (200, partial update) and DELETE
// /api/teams/{id} (200). Proposed tier: Tier 3 MANAGEMENT,
// admin scope (see file header).
func (s *Server) handleAPITeam(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "teams unavailable: no ledger wired")
		return
	}
	teamID := strings.TrimSpace(r.PathValue("id"))
	if teamID == "" {
		writeError(w, http.StatusBadRequest, "team id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPITeamGet(w, r, teamID)
	case http.MethodPatch:
		s.handleAPITeamPatch(w, r, teamID)
	case http.MethodDelete:
		s.handleAPITeamDelete(w, r, teamID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAPITeamGet(w http.ResponseWriter, r *http.Request, teamID string) {
	team, err := s.ledger.GetTeam(r.Context(), teamID)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read team")
		return
	}
	if team == nil {
		writeError(w, http.StatusNotFound, "unknown team "+teamID)
		return
	}
	writeJSON(w, http.StatusOK, s.apiTeamView(r.Context(), *team, time.Now()))
}

// apiTeamPatchField reads one PATCH field out of the raw body map:
// it reports whether the field was present and whether it was an
// explicit null, so a null (clear the field) is distinguishable
// from an absent field (keep the stored value).
func apiTeamPatchField(raw map[string]json.RawMessage, name string, dst any) (present, isNull bool, err error) {
	v, ok := raw[name]
	if !ok {
		return false, false, nil
	}
	if string(v) == "null" {
		return true, true, nil
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return true, false, fmt.Errorf("field %q: %v", name, err)
	}
	return true, false, nil
}

func (s *Server) handleAPITeamPatch(w http.ResponseWriter, r *http.Request, teamID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "invalid team patch: expected a JSON object with any of {\"name\", \"description\", \"status\", \"max_budget_usd\", \"budget_duration\", \"members\"}")
		return
	}
	ctx := r.Context()
	prev, err := s.ledger.GetTeam(ctx, teamID)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read team")
		return
	}
	if prev == nil {
		writeError(w, http.StatusNotFound, "unknown team "+teamID)
		return
	}
	prevMembers, _ := s.ledger.TeamMembers(ctx, teamID)
	prevIDs := make([]string, 0, len(prevMembers))
	for _, m := range prevMembers {
		prevIDs = append(prevIDs, m.KeyID)
	}
	next := teamLedgerToConfig(*prev, prevIDs)

	var name, description, status, duration *string
	var budget *float64
	var members *[]string
	var budgetPresent, durationPresent, membersPresent bool
	if present, isNull, err := apiTeamPatchField(raw, "name", &name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		if isNull {
			next.Name = "" // null fails validation below: name is required
		} else {
			next.Name = *name
		}
	}
	if present, isNull, err := apiTeamPatchField(raw, "description", &description); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		if isNull {
			next.Description = ""
		} else {
			next.Description = *description
		}
	}
	if present, isNull, err := apiTeamPatchField(raw, "status", &status); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		if isNull {
			next.Status = ""
		} else {
			next.Status = *status
		}
	}
	if present, isNull, err := apiTeamPatchField(raw, "max_budget_usd", &budget); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		budgetPresent = true
		if isNull || budget == nil {
			next.MaxBudgetUSD = nil // explicit null clears the budget
		} else {
			next.MaxBudgetUSD = budget
		}
	}
	if present, isNull, err := apiTeamPatchField(raw, "budget_duration", &duration); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		durationPresent = true
		if isNull || duration == nil {
			next.BudgetDuration = ""
		} else {
			next.BudgetDuration = *duration
		}
	}
	if present, isNull, err := apiTeamPatchField(raw, "members", &members); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	} else if present {
		membersPresent = true
		if isNull || members == nil {
			next.Members = []string{} // explicit null clears the membership
		} else {
			next.Members = *members
		}
	}
	// A budget change resets the window: the new cap starts now,
	// it does not inherit the old window's spent total — the same
	// rule as the dashboard update.
	budgetChanged := (budgetPresent && (prev.MaxBudgetUSD == nil) != (next.MaxBudgetUSD == nil)) ||
		(budgetPresent && prev.MaxBudgetUSD != nil && next.MaxBudgetUSD != nil && *prev.MaxBudgetUSD != *next.MaxBudgetUSD) ||
		(durationPresent && prev.BudgetDuration != next.BudgetDuration)
	if err := s.validateTeamUpdate(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if membersPresent {
		if err := s.validateAPPTeamMembers(next.Members); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if next.Status == "archived" && len(next.Members) > 0 {
			writeError(w, http.StatusBadRequest, "members cannot be assigned to an archived team")
			return
		}
	}
	team := teamConfigToLedger(next)
	team.BudgetResetAt = prev.BudgetResetAt
	if budgetChanged {
		team.BudgetResetAt = nil
	}
	if err := s.ledger.SetTeam(ctx, team); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save team")
		return
	}
	now := time.Now()
	if membersPresent {
		want := map[string]bool{}
		for _, m := range next.Members {
			want[m] = true
		}
		for _, m := range prevIDs {
			if !want[m] {
				if _, err := s.ledger.UnassignKeyFromTeam(ctx, m, now); err != nil {
					writeServerError(w, http.StatusInternalServerError, "could not update team members")
					return
				}
			}
		}
		for _, m := range next.Members {
			if err := s.ledger.AssignKeyToTeam(ctx, m, teamID, now); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	if !s.persistAPITeamConfig(ctx, teamID, next, prev) {
		writeServerError(w, http.StatusInternalServerError, "could not persist team config")
		return
	}
	stored, err := s.ledger.GetTeam(ctx, teamID)
	if err != nil || stored == nil {
		writeServerError(w, http.StatusInternalServerError, "could not read updated team")
		return
	}
	writeJSON(w, http.StatusOK, s.apiTeamView(ctx, *stored, time.Now()))
}

func (s *Server) handleAPITeamDelete(w http.ResponseWriter, r *http.Request, teamID string) {
	ctx := r.Context()
	prev, err := s.ledger.GetTeam(ctx, teamID)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read team")
		return
	}
	if prev == nil {
		writeError(w, http.StatusNotFound, "unknown team "+teamID)
		return
	}
	if err := s.ledger.DeleteTeam(ctx, teamID); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not delete team")
		return
	}
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.TeamConfig(nil), s.cfg.Teams...)
		kept := s.cfg.Teams[:0]
		for _, t := range s.cfg.Teams {
			if t.ID != teamID {
				kept = append(kept, t)
			}
		}
		s.cfg.Teams = kept
		if err := s.configPersist(); err != nil {
			// Best-effort rollback: the team row is restored,
			// but bindings closed by DeleteTeam are gone —
			// the same limit as the dashboard delete path.
			// The operator re-reads the team to see what stuck.
			s.cfg.Teams = prevCfg
			_ = s.ledger.SetTeam(ctx, *prev)
			writeServerError(w, http.StatusInternalServerError, "could not persist team config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
