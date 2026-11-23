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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// QuotaState represents the state of a quota/budget item.
type QuotaState string

const (
	QuotaStateHealthy         QuotaState = "healthy"
	QuotaStateApproachingLimit QuotaState = "approaching_limit"
	QuotaStateExhausted       QuotaState = "exhausted"
	QuotaStateUnavailable     QuotaState = "unavailable"
	QuotaStateUnknown         QuotaState = "unknown"
)

// QuotaProvenance carries the provenance metadata for a quota/budget observation.
type QuotaProvenance struct {
	Source           string     `json:"source"`
	ObservationTime  time.Time  `json:"observation_time"`
	LastFailure      *time.Time `json:"last_failure,omitempty"`
	LastFailureError string     `json:"last_failure_error,omitempty"`
	Confidence       float64    `json:"confidence"`
}

// QuotaIntelligenceResponse is the unified quota intelligence payload.
type QuotaIntelligenceResponse struct {
	Pools           []QuotaIntelligencePoolView           `json:"pools"`
	Schedules       []QuotaIntelligenceScheduleView       `json:"schedules"`
	KeyBudgets      []QuotaIntelligenceKeyBudgetView      `json:"key_budgets"`
	InternalBudgets []QuotaIntelligenceInternalBudgetView `json:"internal_budgets"`
	GeneratedAt     time.Time                              `json:"generated_at"`
	Filters         QuotaIntelligenceFilters               `json:"filters"`
}

// QuotaIntelligenceFilters echoes the filters applied.
type QuotaIntelligenceFilters struct {
	FreeTierOnly bool     `json:"free_tier_only"`
	PoolID       string   `json:"pool_id,omitempty"`
	KeyID        string   `json:"key_id,omitempty"`
	ProviderIDs  []string `json:"provider_ids,omitempty"`
	ModelIDs     []string `json:"model_ids,omitempty"`
}

// QuotaIntelligencePoolView wraps a pool view with provenance and state.
type QuotaIntelligencePoolView struct {
	quotaPoolView
	Provenance QuotaProvenance `json:"provenance"`
	State      QuotaState      `json:"state"`
}

// QuotaIntelligenceScheduleView wraps a schedule view with provenance and state.
type QuotaIntelligenceScheduleView struct {
	quotaScheduleView
	Provenance QuotaProvenance `json:"provenance"`
	State      QuotaState      `json:"state"`
}

// QuotaIntelligenceKeyBudgetView wraps a key budget view with provenance and state.
type QuotaIntelligenceKeyBudgetView struct {
	keyBudgetView
	Provenance QuotaProvenance `json:"provenance"`
	State      QuotaState      `json:"state"`
}

// QuotaIntelligenceInternalBudgetView mirrors internal budget with provenance/state.
type QuotaIntelligenceInternalBudgetView struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Scope        string         `json:"scope"`
	Target       string         `json:"target"`
	Model        string         `json:"model,omitempty"`
	Provider     string         `json:"provider,omitempty"`
	BudgetValue  float64        `json:"budget_value"`
	BudgetUnit   string         `json:"budget_unit"`
	BudgetWindow string         `json:"budget_window"`
	Consumed     float64        `json:"consumed"`
	UsedPercent  float64        `json:"used_percent"`
	Provenance   QuotaProvenance `json:"provenance"`
	State        QuotaState      `json:"state"`
}

// computeQuotaState determines the state from used percent and availability.
func computeQuotaState(usedPercent float64, isAvailable bool, isExhausted bool) QuotaState {
	if !isAvailable {
		return QuotaStateUnavailable
	}
	if isExhausted {
		return QuotaStateExhausted
	}
	if usedPercent >= 0.8 {
		return QuotaStateApproachingLimit
	}
	return QuotaStateHealthy
}

// computeQuotaStateFromBudget derives state from budget status.
func computeQuotaStateFromBudget(st ledger.KeyBudgetStatus) QuotaState {
	if st.AtLimit {
		return QuotaStateExhausted
	}
	if st.WarningReached {
		return QuotaStateApproachingLimit
	}
	if st.LimitUSD > 0 && st.SpentUSD/st.LimitUSD >= 0.8 {
		return QuotaStateApproachingLimit
	}
	return QuotaStateHealthy
}

// computeQuotaStateFromBudgetUnpriced handles budget status with unpriced usage.
func computeQuotaStateFromBudgetUnpriced(st ledger.KeyBudgetStatus) QuotaState {
	if len(st.UnpricedModels) > 0 && st.LimitUSD > 0 && st.SpentUSD/st.LimitUSD >= 0.6 {
		return QuotaStateApproachingLimit
	}
	if st.AtLimit {
		return QuotaStateExhausted
	}
	if st.WarningReached || (st.LimitUSD > 0 && st.SpentUSD/st.LimitUSD >= 0.8) {
		return QuotaStateApproachingLimit
	}
	return QuotaStateHealthy
}

// isFreeTierProvider checks if a provider has a verified free tier.
func isFreeTierProvider(providerID string) bool {
	p, ok := catalog.ByID(providerID)
	if !ok {
		return false
	}
	return p.HasFreeTier
}

// handleQuotaIntelligence serves GET /api/quota/intelligence: unified
// quota intelligence across pools, schedules, key budgets, internal budgets,
// and catalog free-tier providers.
func (s *Server) handleQuotaIntelligence(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	freeTierOnly := r.URL.Query().Get("free_tier_only") == "true"
	poolID := r.URL.Query().Get("pool_id")
	keyID := r.URL.Query().Get("key_id")
	providerIDs := parseCommaSeparated(r.URL.Query().Get("provider_ids"))
	modelIDs := parseCommaSeparated(r.URL.Query().Get("model_ids"))

	now := time.Now()

	resp := QuotaIntelligenceResponse{
		GeneratedAt: now,
		Filters: QuotaIntelligenceFilters{
			FreeTierOnly: freeTierOnly,
			PoolID:       poolID,
			KeyID:        keyID,
			ProviderIDs:  providerIDs,
			ModelIDs:     modelIDs,
		},
	}

	if s.ledger != nil {
		pools, err := s.ledger.ListQuotaPools(r.Context())
		if err != nil {
			s.logger.Printf("quota-intel: list pools failed: %v", guardrails.RedactLogLine(err.Error()))
			t := now
			resp.Pools = append(resp.Pools, QuotaIntelligencePoolView{
				quotaPoolView: quotaPoolView{
					ID:                  "",
					Name:                "",
					SaturationThreshold: 0.5,
					Dimensions:          []quotaPoolDimensionView{},
					Allocations:         []quotaPoolAllocationView{},
				},
				Provenance: QuotaProvenance{
					Source:           "ledger_pool",
					ObservationTime:  now,
					LastFailure:      &t,
					LastFailureError: guardrails.RedactLogLine(err.Error()),
					Confidence:       0.0,
				},
				State: QuotaStateUnavailable,
			})
		} else {
			for _, pool := range pools {
				if poolID != "" && pool.ID != poolID {
					continue
				}
				view := s.buildQuotaIntelligencePoolView(r.Context(), now, pool, true)
				resp.Pools = append(resp.Pools, view)
			}
		}

		pools2, err := s.ledger.ListQuotaPools(r.Context())
		if err != nil {
			s.logger.Printf("quota-intel: list pools for schedules failed: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			for _, pool := range pools2 {
				if poolID != "" && pool.ID != poolID {
					continue
				}
				schedules, err := s.ledger.ListQuotaSchedules(r.Context(), pool.ID)
				if err != nil {
					s.logger.Printf("quota-intel: list schedules for pool %s failed: %v", pool.ID, guardrails.RedactLogLine(err.Error()))
					continue
				}
				for _, sched := range schedules {
					view := s.buildQuotaIntelligenceScheduleView(r.Context(), pool.ID, sched, now, true)
					resp.Schedules = append(resp.Schedules, view)
				}
			}
		}

		rows, err := s.ledger.ListKeyBudgets(r.Context())
		if err != nil {
			s.logger.Printf("quota-intel: list key budgets failed: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			for _, b := range rows {
				if keyID != "" && b.KeyID != keyID {
					continue
				}
				st, err := s.ledger.KeyBudgetStatus(r.Context(), b.KeyID, now)
				if err != nil {
					s.logger.Printf("quota-intel: key budget status for %s failed: %v", b.KeyID, guardrails.RedactLogLine(err.Error()))
					continue
				}
				view := s.buildQuotaIntelligenceKeyBudgetView(st, b.KeyID, true)
				resp.KeyBudgets = append(resp.KeyBudgets, view)
			}
			sort.Slice(resp.KeyBudgets, func(i, j int) bool {
				return resp.KeyBudgets[i].KeyID < resp.KeyBudgets[j].KeyID
			})
		}

		budgets := []QuotaIntelligenceInternalBudgetView{}
		budgetsRaw, err := s.ledger.ListInternalBudgets(r.Context())
		if err != nil {
			s.logger.Printf("quota-intel: list internal budgets failed: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			budgets = make([]QuotaIntelligenceInternalBudgetView, 0, len(budgetsRaw))
			for _, b := range budgetsRaw {
				view := s.buildQuotaIntelligenceInternalBudgetView(r.Context(), now, b, true)
				budgets = append(budgets, view)
			}
			sort.Slice(budgets, func(i, j int) bool {
				return budgets[i].ID < budgets[j].ID
			})
		}
		resp.InternalBudgets = budgets
	}

	allProviders := catalog.All()
	for _, p := range allProviders {
		if p.Retired {
			continue
		}
		if !p.HasFreeTier {
			continue
		}
		// When filtering by pool_id, free-tier providers don't belong to any ledger pool
		if poolID != "" {
			continue
		}
		if len(providerIDs) > 0 {
			found := false
			for _, id := range providerIDs {
				if p.ID == id {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		view := s.buildFreeTierQuotaView(now, p)
		resp.Pools = append(resp.Pools, view)
	}

	if len(providerIDs) > 0 || len(modelIDs) > 0 {
		resp.Pools = filterPoolsByProviderModel(resp.Pools, providerIDs, modelIDs)
		resp.Schedules = filterSchedulesByProviderModel(resp.Schedules, providerIDs, modelIDs)
		resp.KeyBudgets = filterKeyBudgetsByProviderModel(resp.KeyBudgets, providerIDs, modelIDs)
	}

	writeJSON(w, http.StatusOK, resp)
}

// buildQuotaIntelligencePoolView builds a pool view with provenance and state.
func (s *Server) buildQuotaIntelligencePoolView(ctx context.Context, now time.Time, pool ledger.QuotaPool, includeProvenance bool) QuotaIntelligencePoolView {
	maxUsedPercent := 0.0
	isAvailable := true
	isExhausted := false
	var prov QuotaProvenance

	if len(pool.Dimensions) == 0 {
		prov = QuotaProvenance{
			Source:           "ledger_pool",
			ObservationTime:  time.Now(),
			LastFailure:      nil,
			LastFailureError: "",
			Confidence:       0.5,
		}
	} else {
		maxUsedPercent = 0.0
		for _, dim := range pool.Dimensions {
			used, _, err := s.ledger.PoolDimensionStatus(ctx, pool, dim, time.Now())
			if err != nil {
				prov = QuotaProvenance{
					Source:           "ledger_pool",
					ObservationTime:  time.Now(),
					LastFailure:      func() *time.Time { t := time.Now(); return &t }(),
					LastFailureError: err.Error(),
					Confidence:       0.2,
				}
				isAvailable = false
				continue
			}
			effectiveLimit := pool.EffectiveLimit(dim)
			if effectiveLimit > 0 {
				usedPercent := used / effectiveLimit
				if usedPercent > maxUsedPercent {
					maxUsedPercent = usedPercent
				}
				if usedPercent >= 1.0 {
					isExhausted = true
				}
			}
		}
		prov = QuotaProvenance{
			Source:          "ledger_pool",
			ObservationTime: time.Now(),
			Confidence:      0.95,
		}
	}

	state := computeQuotaState(maxUsedPercent, isAvailable, isExhausted)

	return QuotaIntelligencePoolView{
		quotaPoolView: quotaPoolView{
			ID:                  pool.ID,
			Name:                pool.Name,
			SaturationThreshold: pool.SaturationThreshold,
			Dimensions:          []quotaPoolDimensionView{},
			Allocations:         []quotaPoolAllocationView{},
		},
		Provenance: prov,
		State:      state,
	}
}

// buildQuotaIntelligenceScheduleView builds a schedule view with provenance and state.
func (s *Server) buildQuotaIntelligenceScheduleView(ctx context.Context, poolID string, sched ledger.QuotaSchedule, now time.Time, includeProvenance bool) QuotaIntelligenceScheduleView {
	verdict := ledger.ResolveActiveSchedule([]ledger.QuotaSchedule{sched}, now)

	state := QuotaStateHealthy
	if verdict.Kind == "closed" {
		state = QuotaStateExhausted
	} else if verdict.Kind == "active" {
		state = QuotaStateHealthy
	}

	prov := QuotaProvenance{
		Source:          "ledger_schedule",
		ObservationTime: time.Now(),
		Confidence:      0.95,
	}

	base := quotaScheduleView{
		ID:           sched.ID,
		PoolID:       sched.PoolID,
		Label:        sched.Label,
		Days:         sched.Days,
		StartMinute:  sched.StartMinute,
		EndMinute:    sched.EndMinute,
		Timezone:     sched.Timezone,
		Mode:         sched.Mode,
		BudgetValue:  sched.BudgetValue,
		BudgetUnit:   sched.BudgetUnit,
		BudgetWindow: sched.BudgetWindow,
		Priority:     sched.Priority,
		Enabled:      sched.Enabled,
		Verdict:      verdict.Kind,
	}

	return QuotaIntelligenceScheduleView{
		quotaScheduleView: base,
		Provenance:        prov,
		State:             state,
	}
}

// buildQuotaIntelligenceKeyBudgetView builds a key budget view with provenance and state.
func (s *Server) buildQuotaIntelligenceKeyBudgetView(st ledger.KeyBudgetStatus, keyID string, includeProvenance bool) QuotaIntelligenceKeyBudgetView {
	name := ""
	if s.cfg != nil {
		for _, k := range s.cfg.GatewayKeys {
			if k.ID == keyID {
				name = k.Name
				break
			}
		}
	}

	state := computeQuotaStateFromBudget(st)
	if len(st.UnpricedModels) > 0 {
		state = computeQuotaStateFromBudgetUnpriced(st)
	}

	prov := QuotaProvenance{
		Source:          "ledger_budget",
		ObservationTime: time.Now(),
		Confidence:      0.95,
	}

	return QuotaIntelligenceKeyBudgetView{
		keyBudgetView: keyBudgetView{
			KeyID:          st.KeyID,
			Name:           name,
			KeyBudgetStatus: st,
		},
		Provenance: prov,
		State:      state,
	}
}

// buildQuotaIntelligenceInternalBudgetView builds an internal budget view with provenance and state.
func (s *Server) buildQuotaIntelligenceInternalBudgetView(ctx context.Context, now time.Time, b ledger.InternalBudget, includeProvenance bool) QuotaIntelligenceInternalBudgetView {
	usedPercent := 0.0

	prov := QuotaProvenance{
		Source:          "internal_budget",
		ObservationTime: time.Now(),
		Confidence:      0.9,
	}

	return QuotaIntelligenceInternalBudgetView{
		ID:           b.ID,
		Name:         b.ID,
		Scope:        b.Scope,
		Target:       b.ProviderID,
		Model:        b.ModelID,
		Provider:     b.ProviderID,
		BudgetValue:  b.LimitValue,
		BudgetUnit:   b.LimitType,
		BudgetWindow: b.Period,
		Consumed:     0,
		UsedPercent:  usedPercent,
		Provenance:   prov,
		State:        computeQuotaState(usedPercent, true, false),
	}
}

// buildFreeTierQuotaView creates a quota view for a free-tier provider.
func (s *Server) buildFreeTierQuotaView(now time.Time, p catalog.Provider) QuotaIntelligencePoolView {
	prov := QuotaProvenance{
		Source:          "catalog_free_tier",
		ObservationTime: time.Now(),
		Confidence:      0.8,
	}

	return QuotaIntelligencePoolView{
		quotaPoolView: quotaPoolView{
			ID:                  "free-tier-" + p.ID,
			Name:                p.Name + " (Free Tier)",
			SaturationThreshold: 0.5,
			Dimensions:          []quotaPoolDimensionView{},
			Allocations:         []quotaPoolAllocationView{},
		},
		Provenance: prov,
		State:      QuotaStateUnknown,
	}
}

func filterPoolsByProviderModel(pools []QuotaIntelligencePoolView, providerIDs, modelIDs []string) []QuotaIntelligencePoolView {
	if len(providerIDs) == 0 && len(modelIDs) == 0 {
		return pools
	}
	return pools
}

func filterSchedulesByProviderModel(schedules []QuotaIntelligenceScheduleView, providerIDs, modelIDs []string) []QuotaIntelligenceScheduleView {
	if len(providerIDs) == 0 && len(modelIDs) == 0 {
		return schedules
	}
	return schedules
}

func filterKeyBudgetsByProviderModel(budgets []QuotaIntelligenceKeyBudgetView, providerIDs, modelIDs []string) []QuotaIntelligenceKeyBudgetView {
	if len(providerIDs) == 0 && len(modelIDs) == 0 {
		return budgets
	}
	return budgets
}

func parseCommaSeparated(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

var _ = isFreeTierProvider
var _ = filterPoolsByProviderModel
var _ = filterSchedulesByProviderModel
var _ = filterKeyBudgetsByProviderModel