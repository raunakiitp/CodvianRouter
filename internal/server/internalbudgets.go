// Internal scoped budgets (M5.11): the server side — the
// pre-call gate on the chat paths, and the GET/POST
// /dashboard/internal-budgets feeds for the operator.
//
// The gate runs with the other budget gates, after the quota
// schedules and before the traffic check (a refused request
// consumes no traffic slot): it evaluates the budgets that apply
// to this request — global always, model for the requested
// model, pool for every pool the caller's key belongs to —
// against the ledger's spend in each budget's period window.
// Provider scope is NOT evaluated here (the provider is only
// known per attempt inside the engine — see
// internal/router/internalbudget.go). A deny is a 402, the same
// status as the key and global budget refusals, error type
// "internal_budget". A warn is served, with the
// X-Codvian-Budget-Warning header naming the budget so the
// operator can see the pressure. A ledger read failure fails
// closed with a 503: a limit that cannot be read must not be
// waived. Skipped entirely with no ledger.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// checkInternalBudgets enforces the internal budgets that apply
// to this request. It writes the refusal and returns true when
// the request must stop here, before any provider is called.
func (s *Server) checkInternalBudgets(w http.ResponseWriter, r *http.Request, model string) bool {
	if s.ledger == nil {
		return false
	}
	ctx := r.Context()
	now := time.Now()
	var poolIDs []string
	if keyID := s.callerKeyID(r); keyID != "" {
		pools, err := s.ledger.PoolsForKey(ctx, keyID)
		if err != nil {
			s.logger.Printf("internal-budget: pool lookup failed: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusServiceUnavailable, "internal budget check unavailable")
			return true
		}
		for _, p := range pools {
			poolIDs = append(poolIDs, p.ID)
		}
	}
	ev, err := s.ledger.EvaluateInternalBudgets(ctx, "", model, poolIDs, now)
	if err != nil {
		s.logger.Printf("internal-budget: check failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusServiceUnavailable, "internal budget check unavailable")
		return true
	}
	if ev == nil {
		return false
	}
	switch ev.Decision {
	case ledger.InternalBudgetDeny:
		writeJSON(w, http.StatusPaymentRequired, errorBody{Error: errorDetail{Message: ev.Error(), Type: "internal_budget"}})
		return true
	case ledger.InternalBudgetWarn:
		// M8.19: crossing the warning threshold also fires the
		// "budget.warning" webhook (edge-triggered inside
		// notifyBudgetWarning — a repeat warn while the budget
		// stays above the threshold does not re-deliver).
		s.notifyBudgetWarning(ev)
		w.Header().Set("X-Codvian-Budget-Warning", ev.Budget.ID)
		return false
	default:
		return false
	}
}

// ---------------------------------------------------------------------------
// GET/POST /dashboard/internal-budgets.
// ---------------------------------------------------------------------------

// internalBudgetView is one row of the GET
// /dashboard/internal-budgets payload, with its live usage in
// the current period window.
type internalBudgetView struct {
	ID               string  `json:"id"`
	Scope            string  `json:"scope"`
	ProviderID       string  `json:"provider_id,omitempty"`
	ModelID          string  `json:"model_id,omitempty"`
	PoolID           string  `json:"pool_id,omitempty"`
	Period           string  `json:"period"`
	LimitType        string  `json:"limit_type"`
	LimitValue       float64 `json:"limit_value"`
	WarningThreshold float64 `json:"warning_threshold"`
	Enabled          bool    `json:"enabled"`
	Used             float64 `json:"used"`
	Remaining        float64 `json:"remaining"`
	NextReset        string  `json:"next_reset_at"`
	Decision         string  `json:"decision"`
}

// internalBudgetsResponse is the GET /dashboard/internal-budgets payload.
type internalBudgetsResponse struct {
	Budgets []internalBudgetView `json:"budgets"`
}

// handleInternalBudgets serves GET /dashboard/internal-budgets:
// every internal budget with its live usage, remaining and
// decision in the current period window. With no ledger wired
// it answers an empty list.
func (s *Server) handleInternalBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := internalBudgetsResponse{Budgets: []internalBudgetView{}}
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	now := time.Now()
	budgets, err := s.ledger.ListInternalBudgets(ctx)
	if err != nil {
		s.logger.Printf("internal-budget: list failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not list internal budgets")
		return
	}
	for _, b := range budgets {
		used, nextReset, err := s.ledger.InternalBudgetUsage(ctx, b, now)
		if err != nil {
			s.logger.Printf("internal-budget: usage failed for budget %s: %v", b.ID, guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not read internal budget usage")
			return
		}
		remaining := b.LimitValue - used
		if remaining < 0 {
			remaining = 0
		}
		decision := string(ledger.InternalBudgetAllow)
		if b.Enabled {
			if used >= b.LimitValue {
				decision = string(ledger.InternalBudgetDeny)
			} else {
				threshold := b.WarningThreshold
				if threshold == 0 {
					threshold = 0.8
				}
				if used/b.LimitValue >= threshold {
					decision = string(ledger.InternalBudgetWarn)
				}
			}
		}
		out.Budgets = append(out.Budgets, internalBudgetView{
			ID: b.ID, Scope: b.Scope, ProviderID: b.ProviderID, ModelID: b.ModelID, PoolID: b.PoolID,
			Period: b.Period, LimitType: b.LimitType, LimitValue: b.LimitValue,
			WarningThreshold: b.WarningThreshold, Enabled: b.Enabled,
			Used: used, Remaining: remaining, NextReset: nextReset.UTC().Format(time.RFC3339), Decision: decision,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// internalBudgetUpdate is the POST /dashboard/internal-budgets
// body: a full budget upsert (absent fields keep their stored
// values), or a delete.
type internalBudgetUpdate struct {
	ID               string   `json:"id"`
	Scope            *string  `json:"scope,omitempty"`
	ProviderID       *string  `json:"provider_id,omitempty"`
	ModelID          *string  `json:"model_id,omitempty"`
	PoolID           *string  `json:"pool_id,omitempty"`
	Period           *string  `json:"period,omitempty"`
	LimitType        *string  `json:"limit_type,omitempty"`
	LimitValue       *float64 `json:"limit_value,omitempty"`
	WarningThreshold *float64 `json:"warning_threshold,omitempty"`
	Enabled          *bool    `json:"enabled,omitempty"`
	Delete           bool     `json:"delete"`
}

// internalBudgetConfigToLedger maps a config budget to the ledger shape.
func internalBudgetConfigToLedger(b config.InternalBudgetConfig) ledger.InternalBudget {
	return ledger.InternalBudget{
		ID: b.ID, Scope: b.Scope, ProviderID: b.ProviderID, ModelID: b.ModelID, PoolID: b.PoolID,
		Period: b.Period, LimitType: b.LimitType, LimitValue: b.LimitValue,
		WarningThreshold: b.WarningThreshold, Enabled: b.Enabled,
	}
}

// internalBudgetLedgerToConfig maps a ledger budget to the config shape.
func internalBudgetLedgerToConfig(b ledger.InternalBudget) config.InternalBudgetConfig {
	return config.InternalBudgetConfig{
		ID: b.ID, Scope: b.Scope, ProviderID: b.ProviderID, ModelID: b.ModelID, PoolID: b.PoolID,
		Period: b.Period, LimitType: b.LimitType, LimitValue: b.LimitValue,
		WarningThreshold: b.WarningThreshold, Enabled: b.Enabled,
	}
}

// validateInternalBudgetUpdate checks one budget body the same
// way the config validation does: scope names its kind, the
// matching entity id is required (and must name an existing
// pool for pool scope), period/limit type from the known sets,
// limit positive, warning threshold 0-1.
func (s *Server) validateInternalBudgetUpdate(ctx context.Context, b config.InternalBudgetConfig) error {
	if b.ID == "" {
		return errors.New("id is required")
	}
	switch b.Scope {
	case "global":
	case "provider":
		if b.ProviderID == "" {
			return errors.New("provider_id is required for a provider scope")
		}
	case "model":
		if b.ModelID == "" {
			return errors.New("model_id is required for a model scope")
		}
	case "pool":
		if b.PoolID == "" {
			return errors.New("pool_id is required for a pool scope")
		}
		if s.ledger != nil {
			if _, err := s.ledger.GetQuotaPool(ctx, b.PoolID); err != nil {
				return fmt.Errorf("pool_id %q does not name a quota pool", b.PoolID)
			}
		}
	default:
		return fmt.Errorf("scope %q is not a known scope (global, provider, model, pool)", b.Scope)
	}
	switch b.Period {
	case "daily", "weekly", "monthly":
	default:
		return fmt.Errorf("period %q is not a known period (daily, weekly, monthly)", b.Period)
	}
	switch b.LimitType {
	case "currency", "tokens", "requests":
	default:
		return fmt.Errorf("limit_type %q is not a known limit type (currency, tokens, requests)", b.LimitType)
	}
	if b.LimitValue <= 0 {
		return fmt.Errorf("limit_value must be positive (got %v)", b.LimitValue)
	}
	if b.WarningThreshold < 0 || b.WarningThreshold > 1 {
		return fmt.Errorf("warning_threshold must be between 0 and 1 (got %v)", b.WarningThreshold)
	}
	return nil
}

// handleInternalBudgetUpdate serves POST
// /dashboard/internal-budgets. The ledger row is written first,
// then the config file's internal_budgets block (when a config +
// persist function are wired) so the two never disagree — a
// config save failure rolls the ledger change back and answers
// 500, exactly like the key budgets and the quota pools.
func (s *Server) handleInternalBudgetUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "internal budgets unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update internalBudgetUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid internal budget update: expected {\"id\": string, \"scope\": \"global\"|\"provider\"|\"model\"|\"pool\", \"period\": \"daily\"|\"weekly\"|\"monthly\", \"limit_type\": \"currency\"|\"tokens\"|\"requests\", \"limit_value\": number, \"delete\": bool}")
		return
	}
	if update.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	prev, prevErr := s.ledger.GetInternalBudget(ctx, update.ID)
	hadPrev := prevErr == nil
	if update.Delete {
		if err := s.ledger.DeleteInternalBudget(ctx, update.ID); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not delete internal budget")
			return
		}
	} else {
		next := config.InternalBudgetConfig{ID: update.ID, Scope: "global", Period: "daily", LimitType: "currency", LimitValue: 1, WarningThreshold: 0.8, Enabled: true}
		if hadPrev {
			next = internalBudgetLedgerToConfig(prev)
		}
		applyInternalBudgetUpdate(&next, &update)
		if err := s.validateInternalBudgetUpdate(ctx, next); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.ledger.SetInternalBudget(ctx, internalBudgetConfigToLedger(next)); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save internal budget")
			return
		}
	}
	// Config block: keep the file in step with the ledger, or
	// roll the ledger back when the save fails.
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.InternalBudgetConfig(nil), s.cfg.InternalBudgets...)
		if update.Delete {
			kept := s.cfg.InternalBudgets[:0]
			for _, b := range s.cfg.InternalBudgets {
				if b.ID != update.ID {
					kept = append(kept, b)
				}
			}
			s.cfg.InternalBudgets = kept
		} else {
			next := config.InternalBudgetConfig{ID: update.ID}
			for _, b := range s.cfg.InternalBudgets {
				if b.ID == update.ID {
					next = b
					break
				}
			}
			applyInternalBudgetUpdate(&next, &update)
			found := false
			for i, b := range s.cfg.InternalBudgets {
				if b.ID == update.ID {
					s.cfg.InternalBudgets[i] = next
					found = true
					break
				}
			}
			if !found {
				s.cfg.InternalBudgets = append(s.cfg.InternalBudgets, next)
			}
		}
		if err := s.configPersist(); err != nil {
			// Roll the ledger back: runtime and file must never
			// disagree about an internal budget.
			s.cfg.InternalBudgets = prevCfg
			if hadPrev {
				_ = s.ledger.SetInternalBudget(ctx, prev)
			} else {
				_ = s.ledger.DeleteInternalBudget(ctx, update.ID)
			}
			writeServerError(w, http.StatusInternalServerError, "could not persist internal budget config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyInternalBudgetUpdate merges a POST body over a config
// budget (shared by the ledger-next and config-next builds so
// the two never disagree about which fields a partial update
// sets).
func applyInternalBudgetUpdate(next *config.InternalBudgetConfig, update *internalBudgetUpdate) {
	if update.Scope != nil {
		next.Scope = *update.Scope
	}
	if update.ProviderID != nil {
		next.ProviderID = *update.ProviderID
	}
	if update.ModelID != nil {
		next.ModelID = *update.ModelID
	}
	if update.PoolID != nil {
		next.PoolID = *update.PoolID
	}
	if update.Period != nil {
		next.Period = *update.Period
	}
	if update.LimitType != nil {
		next.LimitType = *update.LimitType
	}
	if update.LimitValue != nil {
		next.LimitValue = *update.LimitValue
	}
	if update.WarningThreshold != nil {
		next.WarningThreshold = *update.WarningThreshold
	}
	if update.Enabled != nil {
		next.Enabled = *update.Enabled
	}
}
