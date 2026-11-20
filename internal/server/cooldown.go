// Soft-demotion cooldown feed (research-log entry 183).
//
// GET /dashboard/cooldown reports the live cooldown mode (global +
// per-chain overrides) and every provider currently cooling —
// soft-demoted (moved to the end of the order, still tried once)
// or hard-cooled (skipped until its cooldown ends) — with the
// whole seconds remaining on each cooldown. POST /dashboard/cooldown
// changes the mode. Both are Tier 3 MANAGEMENT, auth-exempt like
// the other dashboard feeds: they carry mode names and cooldown
// counters only, no keys, no base URLs, no message content.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// cooldownReporter is optionally implemented by engines that can
// report and change cooldown modes (router.Engine, and
// degrade.StaleEngine which forwards all four).
type cooldownReporter interface {
	CooldownTargets() []router.CoolingTarget
	CooldownState() (string, map[string]string)
	SetCooldownMode(string)
	SetChainCooldownMode(model, mode string)
}

// breakerReporter is optionally implemented by engines that can
// report live per-provider circuit-breaker states
// (router.Engine, and degrade.StaleEngine which forwards it). It
// is separate from cooldownReporter so an engine that reports
// cooldowns but no breaker state still feeds the cooldown half.
type breakerReporter interface {
	BreakerStates() []router.BreakerStatus
}

// breakerJSON is one provider's live circuit-breaker state in the
// feed. Open is true while the breaker is open (tripped and inside
// its open window); ConsecutiveFailures is the live breaker-counted
// failure count; LastReason is the taxonomy reason slug of the
// failure that most recently counted toward the breaker — when
// Open, the reason it tripped ("" when no breaker-counted failure
// has happened yet); Ignored4xx is how many 4xx failures this
// provider served that never tripped the breaker — the live proof
// that a bad key or a bad request is not a provider-health signal.
// Name is the operator's display name for the provider (from the
// config file); it is omitted when the config has no name for the
// ID, and the dashboard then falls back to the provider ID.
type breakerJSON struct {
	ProviderID          string    `json:"provider"`
	Name                string    `json:"name,omitempty"`
	Open                bool      `json:"open"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Threshold           int       `json:"threshold"`
	OpenedAt            time.Time `json:"opened_at,omitempty"`
	RemainingSeconds    int64     `json:"remaining_seconds"`
	LastReason          string    `json:"last_reason,omitempty"`
	Ignored4xx          int       `json:"ignored_4xx"`
}

// coolingTargetJSON is one provider currently cooling in the feed.
// RemainingSeconds is the whole seconds left until the cooldown
// ends; Failures is the consecutive-failure count that built a
// soft cooldown (0 for a hard one); Demoted is true when the
// target is moved to the end of the order instead of skipped.
// KeyID names the cooling key inside a multi-key pool (CHECKLIST
// 175): "" means the provider's legacy single key — the whole
// provider cools. A non-empty KeyID means only that key cools;
// the provider's other keys keep serving. Name is the operator's
// display name for the provider (from the config file); it is
// omitted when the config has no name for the ID, and the
// dashboard then falls back to the provider ID.
type coolingTargetJSON struct {
	ProviderID       string    `json:"provider"`
	KeyID            string    `json:"key_id,omitempty"`
	Name             string    `json:"name,omitempty"`
	Mode             string    `json:"mode"`
	Until            time.Time `json:"until"`
	RemainingSeconds int64     `json:"remaining_seconds"`
	Failures         int       `json:"failures"`
	Demoted          bool      `json:"demoted"`
}

// chainCooldownJSON is one chain (model) in the feed: its
// configured override ("" = inherit the global mode) and the mode
// that actually applies to it right now.
type chainCooldownJSON struct {
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	Effective string `json:"effective_mode"`
}

// cooldownModeInfo is one mode in the feed: its id, display name
// and what it does to a cooling target.
type cooldownModeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Rule string `json:"rule"`
}

// cooldownResponse is the GET /dashboard/cooldown payload (and the
// POST response after an update). Mode is the effective global
// mode ("hard" when nothing is configured); Persisted reports
// whether a persist function is wired, i.e. whether a POST
// survives a restart. Breakers is every known provider's live
// circuit-breaker state (empty when the engine reports none);
// BreakerRule states the tripping rule the engine implements, so
// the dashboard never hard-codes it.
type cooldownResponse struct {
	Modes       []cooldownModeInfo  `json:"modes"`
	Mode        string              `json:"mode"`
	Chains      []chainCooldownJSON `json:"chains"`
	Cooling     []coolingTargetJSON `json:"cooling"`
	Breakers    []breakerJSON       `json:"breakers"`
	BreakerRule string              `json:"breaker_rule"`
	Persisted   bool                `json:"persisted"`
}

// breakerRule is the circuit-breaker tripping rule the engine
// implements (see router.isClientErrorFailure): only 408/5xx and
// transport failures count toward the breaker — 4xx never trips
// it. It is served in the feed so the dashboard states the live
// rule instead of hard-coding its own copy.
const breakerRule = "The breaker trips only on 408, 5xx and transport failures — 4xx (400/401/403/422) never trips it."

// chainModelIDs returns the known chain (model) IDs in sorted
// order: registry models first, then any model named in extra
// (e.g. a chain with an override but no registry model — removed
// since startup, or named ahead of its model). An empty list
// before any provider is configured is the honest empty state —
// no chains are invented. Shared by the strategies and cooldown
// feeds so both list the same chains.
func (s *Server) chainModelIDs(ctx context.Context, extra map[string]string) []string {
	modelIDs := []string{}
	seen := map[string]bool{}
	if s.registry != nil {
		if models, err := s.registry.Models(ctx); err == nil {
			for _, m := range models {
				if m.ID == "" || seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				modelIDs = append(modelIDs, m.ID)
			}
		}
	} else {
		// No registry: the configured model IDs (WithModels) are
		// the chains, exactly as GET /v1/models lists them.
		for _, id := range s.models {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			modelIDs = append(modelIDs, id)
		}
	}
	for model := range extra {
		if !seen[model] {
			seen[model] = true
			modelIDs = append(modelIDs, model)
		}
	}
	sort.Strings(modelIDs)
	return modelIDs
}

// cooldownFeed builds the live cooldown feed.
func (s *Server) cooldownFeed(r *http.Request) cooldownResponse {
	rc := s.routingConfig()
	global := router.CooldownHard
	configChains := map[string]string{}
	if rc != nil {
		if rc.CooldownMode != "" {
			global = router.NormalizeCooldownMode(rc.CooldownMode)
		}
		for model, m := range rc.ChainCooldownModes {
			configChains[model] = m
		}
	}
	// Runtime overrides (dashboard POSTs, or SetCooldownMode /
	// SetChainCooldownMode calls) win over the config file for
	// this run.
	runtimeGlobal := ""
	runtimeChains := map[string]string{}
	// Display names for the cooling targets come from the config
	// file — the engine only knows provider IDs, and the dashboard
	// must show "acme-openai", not "prov_66dc…".
	names := map[string]string{}
	if s.cfg != nil {
		for _, p := range s.cfg.Providers {
			if p.ID != "" && p.Name != "" {
				names[p.ID] = p.Name
			}
		}
	}
	var cooling []coolingTargetJSON
	if rep, ok := s.engine.(cooldownReporter); ok {
		runtimeGlobal, runtimeChains = rep.CooldownState()
		for _, t := range rep.CooldownTargets() {
			cooling = append(cooling, coolingTargetJSON{
				ProviderID:       t.ProviderID,
				KeyID:            t.KeyID,
				Name:             names[t.ProviderID],
				Mode:             t.Mode,
				Until:            t.Until,
				RemainingSeconds: t.RemainingSeconds,
				Failures:         t.Failures,
				Demoted:          t.Demoted,
			})
		}
	}
	if cooling == nil {
		cooling = []coolingTargetJSON{}
	}
	var breakers []breakerJSON
	if rep, ok := s.engine.(breakerReporter); ok {
		for _, b := range rep.BreakerStates() {
			breakers = append(breakers, breakerJSON{
				ProviderID:          b.ProviderID,
				Name:                names[b.ProviderID],
				Open:                b.Open,
				ConsecutiveFailures: b.ConsecutiveFailures,
				Threshold:           b.Threshold,
				OpenedAt:            b.OpenedAt,
				RemainingSeconds:    b.RemainingSeconds,
				LastReason:          b.LastReason,
				Ignored4xx:          b.Ignored4xx,
			})
		}
	}
	if breakers == nil {
		breakers = []breakerJSON{}
	}
	if runtimeGlobal != "" {
		global = router.NormalizeCooldownMode(runtimeGlobal)
	}
	effectiveChains := map[string]string{}
	for model, m := range configChains {
		effectiveChains[model] = m
	}
	for model, m := range runtimeChains {
		effectiveChains[model] = m
	}
	modelIDs := s.chainModelIDs(r.Context(), effectiveChains)
	chains := make([]chainCooldownJSON, 0, len(modelIDs))
	for _, model := range modelIDs {
		override := effectiveChains[model]
		effective := override
		if effective == "" {
			effective = global
		}
		chains = append(chains, chainCooldownJSON{Model: model, Mode: override, Effective: effective})
	}
	return cooldownResponse{
		Modes: []cooldownModeInfo{
			{ID: router.CooldownHard, Name: "Hard (skip)", Rule: "A cooling provider is skipped until its cooldown ends. Default — behaviour is unchanged unless the operator opts into soft-demote."},
			{ID: router.CooldownSoftDemote, Name: "Soft-demote", Rule: "A cooling provider moves to the end of the order and is still tried once, so a request fails only if every target fails. Cooldown: the upstream's Retry-After, otherwise 5 s, doubling per consecutive failure up to 5 min; a success ends it."},
		},
		Mode:        global,
		Chains:      chains,
		Cooling:     cooling,
		Breakers:    breakers,
		BreakerRule: breakerRule,
		Persisted:   s.routingPersist != nil,
	}
}

// handleCooldown serves GET /dashboard/cooldown.
func (s *Server) handleCooldown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.cooldownFeed(r))
}

// handleCooldownUpdate serves POST /dashboard/cooldown: replace
// the global cooldown mode and/or per-chain overrides with the
// posted ones and return the live feed. Unknown mode names are a
// 400 (nothing is applied); an empty global string clears back to
// the configured default, and an empty per-chain value clears that
// chain's override. The persist function, when wired, runs first
// and its failure leaves routing unchanged.
func (s *Server) handleCooldownUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update RoutingUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid cooldown update: expected {\"cooldown_mode\": string, \"chain_cooldown_modes\": {model: mode}}")
		return
	}
	if update.CooldownMode != nil && !router.IsCooldownMode(*update.CooldownMode) {
		writeError(w, http.StatusBadRequest, "unknown cooldown mode "+*update.CooldownMode)
		return
	}
	for model, m := range update.ChainCooldownModes {
		if model == "" {
			writeError(w, http.StatusBadRequest, "chain_cooldown_modes must not contain an empty model")
			return
		}
		if m != "" && !router.IsCooldownMode(m) {
			writeError(w, http.StatusBadRequest, "unknown cooldown mode "+m+" for chain "+model)
			return
		}
	}
	if s.routingPersist != nil {
		if err := s.routingPersist(update); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save cooldown update")
			return
		}
	}
	// Runtime apply: only after the persist (when wired)
	// succeeded, so a failed save can never leave runtime and
	// file disagreeing.
	if rep, ok := s.engine.(cooldownReporter); ok {
		if update.CooldownMode != nil {
			rep.SetCooldownMode(*update.CooldownMode)
		}
		for model, m := range update.ChainCooldownModes {
			rep.SetChainCooldownMode(model, m)
		}
	}
	s.logger.Printf("cooldown: mode updated (global=%v chains=%v persisted=%v)", guardrails.RedactLogLine(fmt.Sprintf("%v", update.CooldownMode)), guardrails.RedactLogLine(fmt.Sprintf("%v", update.ChainCooldownModes)), s.routingPersist != nil)
	writeJSON(w, http.StatusOK, s.cooldownFeed(r))
}
