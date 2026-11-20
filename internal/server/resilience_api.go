// GET /api/resilience (M8.16, management promotion B).
//
// The live resilience state, read from the existing machinery at
// request time — never a snapshot from startup, never fabricated
// values:
//
//   - Circuit breakers: the engine's live BreakerStates() (the same
//     breakerReporter interface the /dashboard/cooldown feed reads).
//   - Cooldowns: the engine's live CooldownTargets() plus the
//     effective global cooldown mode (via cooldownFeed, which
//     already resolves both from live state).
//   - Hedge: there is NO live hedge counter or delay exposed to the
//     server anywhere in this repo (no hedge counter in the engine,
//     the metrics recorder, or the trace — only the configured
//     HedgeDelay inside router.Config, which the Server cannot read,
//     and the 256 KiB body cap). The hedge section therefore reports
//     status "unknown" with the reason, and the body cap mirrored
//     from the codebase constant — counts are absent, never zero,
//     because zero would claim "no hedge ever fired".
//
// B6 (M10) adds the write side on the same file:
//
//   - GET/PATCH /api/resilience/settings — the settings read (live
//     breaker/cooldown state + which settings have no runtime
//     backing) and patch. Cooldown modes (global + per-model) are
//     real: they go through cooldownReporter.SetCooldownMode /
//     SetChainCooldownMode and the routing persist function, exactly
//     like POST /dashboard/cooldown. Breaker threshold, breaker
//     open-for and cooldown-seconds have NO runtime backing —
//     router.Config.BreakerThreshold / BreakerOpenFor are read when
//     the engine is built (internal/router/router.go:
//     breakerThresholdFor, breakerOpenFor) and router.Engine
//     exposes no setter — so a PATCH naming one is validated for
//     range first (400 on a bad range) and then answers 501 naming
//     the field, and nothing is applied.
//   - POST /api/resilience/breakers/reset — reset one provider's
//     breaker, or all with {"all": true}. It goes through the
//     breakerResetter interface below. router.Engine DOES
//     implement it (ResetBreaker / ResetAllBreakers,
//     internal/router/router.go:2611/:2633), and the production
//     wrapper forwards it — degrade.StaleEngine implements
//     ResetBreaker / ResetAllBreakers by forwarding to the
//     wrapped engine (internal/degrade/degrade.go), as does the
//     savings wrapper through its embedded *StaleEngine — so
//     against the production engine reset is a real reset (200,
//     or 404 for an unknown provider id), not a 501. An engine
//     wired directly without that wrapper that does NOT
//     implement the interface still answers 501.
//   - POST /api/resilience/cooldowns + DELETE
//     /api/resilience/cooldowns/{model...} — set / clear one model's
//     (one chain's) cooldown mode, via SetChainCooldownMode and the
//     routing persist function. "Per-model cooldown" here is the
//     per-chain cooldown-mode override: cooldowns in this codebase
//     are per (provider, key) with lengths derived from the
//     upstream Retry-After hint, never per model, so there is no
//     per-model cooldown *duration* to set.
//
// An unprobed provider reports "unknown": a provider the operator
// configured (s.cfg.Providers) but the engine reports no breaker
// state for at all is listed with state "unknown". A provider the
// engine DOES report is listed with its live state — "closed" with
// zero counted failures means no breaker-counted failure has been
// observed; the breaker feed counts failures only, never successes,
// so it cannot distinguish "never used" from "healthy", and this
// endpoint does not pretend otherwise (see the response note).
//
// Tier/scope are proposed in the M8.16 slice report; the coordinator
// assigns them at registration (guard.go + register()).
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// resilienceHedgeMaxBodyBytes mirrors the largest request body that
// may be hedged (internal/router/hedge.go:16, hedgeMaxBodyBytes =
// 256 KiB). It is a mirror of a codebase constant, cited here so a
// drift is visible next to the citation — not an invented cap.
const resilienceHedgeMaxBodyBytes = 256 << 10

// resilienceBreakerJSON is one provider's live circuit-breaker state
// in the /api/resilience feed. It carries the full BreakerStatus the
// engine reports (state name, profile, trips, window state) — the
// dashboard feed's breakerJSON drops several of these, so this feed
// maps BreakerStatus directly instead of reusing it. State is the
// engine's state name (CLOSED / DEGRADED / OPEN / HALF_OPEN); for a
// provider the engine does not report at all it is "unknown" (see
// resilienceFeed). Name is the operator's display name from the
// config file, omitted when the config has no name for the ID.
type resilienceBreakerJSON struct {
	ProviderID          string `json:"provider"`
	Name                string `json:"name,omitempty"`
	State               string `json:"state"`
	Open                bool   `json:"open"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	Threshold           int    `json:"threshold"`
	OpenedAt            string `json:"opened_at,omitempty"`
	RemainingSeconds    int64  `json:"remaining_seconds"`
	LastReason          string `json:"last_reason,omitempty"`
	Ignored4xx          int    `json:"ignored_4xx"`
	Profile             string `json:"profile,omitempty"`
	OpenForMs           int64  `json:"open_for_ms,omitempty"`
	Trips               int    `json:"trips"`
	WindowOpen          bool   `json:"window_open"`
	WindowFailures      int    `json:"window_failures"`
}

// resilienceHedgeJSON is the hedge section of the /api/resilience
// feed. Status is "unknown": no hedge counter or delay is exposed
// to the server (see the file doc). Launched/Won/Lost counts are
// absent on purpose — a zero would claim no hedge ever fired, which
// this feed cannot know. MaxBodyBytes is the mirrored codebase cap
// (256 KiB); Reason says why the rest is unknown.
type resilienceHedgeJSON struct {
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	MaxBodyBytes int    `json:"max_body_bytes"`
}

// resilienceResponse is the GET /api/resilience payload. Cooling and
// CooldownMode come from the live cooldown feed; Breakers is every
// known provider's live breaker state plus an "unknown" row for each
// configured provider the engine does not report; Hedge is the
// honest-unknown hedge section; EngineAvailable is false only when
// no engine is wired at all (then Breakers/Cooling are empty, not
// invented).
type resilienceResponse struct {
	CooldownMode    string                  `json:"cooldown_mode"`
	Cooling         []coolingTargetJSON     `json:"cooling"`
	Breakers        []resilienceBreakerJSON `json:"breakers"`
	BreakerRule     string                  `json:"breaker_rule"`
	Hedge           resilienceHedgeJSON     `json:"hedge"`
	EngineAvailable bool                    `json:"engine_available"`
	Note            string                  `json:"note"`
}

// resilienceNote states what the breaker rows can and cannot prove,
// so a reader never mistakes "closed, zero failures" for "never
// used" or for "healthy".
const resilienceNote = "Breaker rows count breaker-counted failures only (408/5xx/transport; 4xx never trips). A closed breaker with zero counted failures means no breaker-counted failure has been observed — the feed does not count successes, so it cannot distinguish 'never used' from 'healthy'. A provider the engine reports no breaker state for at all is listed as unknown."

// resilienceFeed builds the live resilience feed from the engine
// and the config at request time.
func (s *Server) resilienceFeed(r *http.Request) resilienceResponse {
	feed := s.cooldownFeed(r)
	names := map[string]string{}
	if s.cfg != nil {
		for _, p := range s.cfg.Providers {
			if p.ID != "" && p.Name != "" {
				names[p.ID] = p.Name
			}
		}
	}
	breakers := []resilienceBreakerJSON{}
	reported := map[string]bool{}
	if rep, ok := s.engine.(breakerReporter); ok && rep != nil {
		for _, b := range rep.BreakerStates() {
			reported[b.ProviderID] = true
			state := b.State
			if state == "" {
				// A BreakerStatus with no state name (a minimal
				// reporter): derive it from Open only — never
				// invent HALF_OPEN/DEGRADED.
				state = router.BreakerClosed
				if b.Open {
					state = router.BreakerOpen
				}
			}
			row := resilienceBreakerJSON{
				ProviderID:          b.ProviderID,
				Name:                names[b.ProviderID],
				State:               state,
				Open:                b.Open,
				ConsecutiveFailures: b.ConsecutiveFailures,
				Threshold:           b.Threshold,
				RemainingSeconds:    b.RemainingSeconds,
				LastReason:          b.LastReason,
				Ignored4xx:          b.Ignored4xx,
				Profile:             string(b.Profile),
				Trips:               b.Trips,
				WindowOpen:          b.WindowOpen,
				WindowFailures:      b.WindowFailures,
			}
			if !b.OpenedAt.IsZero() {
				row.OpenedAt = b.OpenedAt.Format("2006-01-02T15:04:05Z07:00")
			}
			if b.OpenFor > 0 {
				row.OpenForMs = b.OpenFor.Milliseconds()
			}
			breakers = append(breakers, row)
		}
	}
	// Configured providers the engine reports nothing for are
	// unknown — never silently absent, never defaulted to closed.
	if s.cfg != nil {
		unknown := []string{}
		for _, p := range s.cfg.Providers {
			if p.ID == "" || reported[p.ID] {
				continue
			}
			unknown = append(unknown, p.ID)
		}
		sort.Strings(unknown)
		for _, id := range unknown {
			breakers = append(breakers, resilienceBreakerJSON{
				ProviderID: id,
				Name:       names[id],
				State:      "unknown",
			})
		}
	}
	sort.Slice(breakers, func(i, j int) bool { return breakers[i].ProviderID < breakers[j].ProviderID })
	if breakers == nil {
		breakers = []resilienceBreakerJSON{}
	}
	cooling := feed.Cooling
	if cooling == nil {
		cooling = []coolingTargetJSON{}
	}
	return resilienceResponse{
		CooldownMode:    feed.Mode,
		Cooling:         cooling,
		Breakers:        breakers,
		BreakerRule:     feed.BreakerRule,
		Hedge:           resilienceHedgeJSON{Status: "unknown", Reason: "no hedge counter or delay is exposed to the server: the engine, the metrics recorder and the trace carry no hedge counts, and HedgeDelay lives in router.Config, which the server cannot read", MaxBodyBytes: resilienceHedgeMaxBodyBytes},
		EngineAvailable: s.engine != nil,
		Note:            resilienceNote,
	}
}

// handleResilience serves GET /api/resilience.
func (s *Server) handleResilience(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.resilienceFeed(r))
}

// ---------------------------------------------------------------------------
// B6 (M10): resilience settings read/patch + breaker reset +
// per-model cooldowns.
// ---------------------------------------------------------------------------

// breakerResetter is optionally implemented by engines that can
// reset circuit breakers at runtime. ResetBreaker clears one
// provider's breaker — consecutive-failure count, open window and
// failure-window record — so the provider is tried again at once;
// it reports false when the engine knows no breaker for that
// provider at all (unknown provider id). ResetAllBreakers resets
// every known breaker and reports the providers it actually reset.
// router.Engine DOES implement this interface (ResetBreaker /
// ResetAllBreakers, internal/router/router.go:2611/:2633), so the
// production engine answers a real reset; 501 is only for an
// engine without breakerResetter (see the file doc).
type breakerResetter interface {
	ResetBreaker(provider string) bool
	ResetAllBreakers() []string
}

// resilienceUnsupportedSetting is one setting the settings API
// accepts for validation but cannot apply: Field names it, Reason
// says why there is no backing state and what to do instead. The
// dashboard must render these as read-only / unavailable, never
// as editable controls that pretend to work.
type resilienceUnsupportedSetting struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// resilienceUnsupportedSettings is the fixed list of settings with
// no runtime backing (see the file doc for the backing-state
// citations). A PATCH naming any of these answers 501 naming the
// field; the GET settings feed carries the same list so the
// dashboard never has to hard-code it.
var resilienceUnsupportedSettings = []resilienceUnsupportedSetting{
	{
		Field:  "breaker_threshold",
		Reason: "no runtime backing: router.Config.BreakerThreshold is read when the engine is built (internal/router/router.go: breakerThresholdFor) and router.Engine exposes no setter; change it in config and restart",
	},
	{
		Field:  "breaker_open_for_seconds",
		Reason: "no runtime backing: router.Config.BreakerOpenFor is read when the engine is built (internal/router/router.go: breakerOpenFor) and router.Engine exposes no setter; change it in config and restart",
	},
	{
		Field:  "cooldown_seconds",
		Reason: "no runtime backing: the engine derives cooldown lengths from the upstream Retry-After hint (5s base, doubling per consecutive failure, 5min cap — internal/router/cooldown.go) and exposes no global cooldown-duration knob",
	},
}

// resilienceSettingsResponse is the GET /api/resilience/settings
// payload (and the PATCH / POST / DELETE response after an
// update). CooldownMode/Chains/Cooling/Breakers/BreakerRule are
// the live state from the same feeds GET /api/resilience and GET
// /dashboard/cooldown read; Persisted reports whether a persist
// function is wired (a mode change survives a restart only then);
// ResetSupported reports whether the wired engine implements
// breakerResetter (true for router.Engine — it implements
// ResetBreaker / ResetAllBreakers, so reset is a real reset);
// Unsupported lists the settings with no runtime backing.
type resilienceSettingsResponse struct {
	CooldownMode   string                         `json:"cooldown_mode"`
	Chains         []chainCooldownJSON            `json:"chains"`
	Cooling        []coolingTargetJSON            `json:"cooling"`
	Breakers       []resilienceBreakerJSON        `json:"breakers"`
	BreakerRule    string                         `json:"breaker_rule"`
	Persisted      bool                           `json:"persisted"`
	ResetSupported bool                           `json:"reset_supported"`
	Unsupported    []resilienceUnsupportedSetting `json:"unsupported"`
}

// resilienceSettings builds the live settings feed at request
// time: the cooldown feed for modes/chains/cooling, the
// resilience feed for breaker rows, plus the capability flags.
func (s *Server) resilienceSettings(r *http.Request) resilienceSettingsResponse {
	feed := s.cooldownFeed(r)
	res := s.resilienceFeed(r)
	_, resetSupported := s.engine.(breakerResetter)
	return resilienceSettingsResponse{
		CooldownMode:   feed.Mode,
		Chains:         feed.Chains,
		Cooling:        res.Cooling,
		Breakers:       res.Breakers,
		BreakerRule:    res.BreakerRule,
		Persisted:      feed.Persisted,
		ResetSupported: resetSupported,
		Unsupported:    resilienceUnsupportedSettings,
	}
}

// handleResilienceSettings serves GET /api/resilience/settings.
func (s *Server) handleResilienceSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.resilienceSettings(r))
}

// resilienceSettingsPatch is one PATCH /api/resilience/settings
// body. CooldownMode / ChainCooldownModes are real and applied
// ("" clears back to the configured default, exactly like POST
// /dashboard/cooldown). BreakerThreshold / BreakerOpenForSeconds /
// CooldownSeconds are validated for range and then answered 501 —
// they have no runtime backing (see
// resilienceUnsupportedSettings); they are accepted in the body
// only so the dashboard can bind them and get an honest answer
// instead of an "unknown field" rejection.
type resilienceSettingsPatch struct {
	CooldownMode          *string           `json:"cooldown_mode"`
	ChainCooldownModes    map[string]string `json:"chain_cooldown_modes"`
	BreakerThreshold      *int              `json:"breaker_threshold"`
	BreakerOpenForSeconds *int              `json:"breaker_open_for_seconds"`
	CooldownSeconds       *int              `json:"cooldown_seconds"`
}

// handleResilienceSettingsPatch serves PATCH
// /api/resilience/settings. Order matters and is deliberate:
// (1) range validation of every field — a breaker threshold of 0
// or a negative cooldown is a 400, never a 501; (2) unsupported
// fields — a valid but unbacked value is a 501 naming the field,
// and nothing is applied, so a mixed patch can never half-apply;
// (3) persist first, then runtime apply, mirroring
// handleCooldownUpdate, so a failed save leaves routing
// unchanged. The response is the fresh settings feed.
func (s *Server) handleResilienceSettingsPatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var patch resilienceSettingsPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid resilience settings patch: expected {\"cooldown_mode\": string, \"chain_cooldown_modes\": {model: mode}, \"breaker_threshold\": int, \"breaker_open_for_seconds\": int, \"cooldown_seconds\": int}")
		return
	}
	if patch.CooldownMode != nil && !router.IsCooldownMode(*patch.CooldownMode) {
		writeError(w, http.StatusBadRequest, "unknown cooldown mode "+*patch.CooldownMode)
		return
	}
	for model, m := range patch.ChainCooldownModes {
		if model == "" {
			writeError(w, http.StatusBadRequest, "chain_cooldown_modes must not contain an empty model")
			return
		}
		if m != "" && !router.IsCooldownMode(m) {
			writeError(w, http.StatusBadRequest, "unknown cooldown mode "+m+" for chain "+model)
			return
		}
	}
	if patch.BreakerThreshold != nil && *patch.BreakerThreshold < 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("breaker_threshold must be at least 1, got %d", *patch.BreakerThreshold))
		return
	}
	if patch.BreakerOpenForSeconds != nil && *patch.BreakerOpenForSeconds < 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("breaker_open_for_seconds must be at least 1, got %d", *patch.BreakerOpenForSeconds))
		return
	}
	if patch.CooldownSeconds != nil && *patch.CooldownSeconds < 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("cooldown_seconds must be at least 1, got %d", *patch.CooldownSeconds))
		return
	}
	// Valid but unbacked: 501 for that field only, nothing applied.
	if patch.BreakerThreshold != nil {
		writeError(w, http.StatusNotImplemented, "breaker_threshold: "+resilienceUnsupportedSettings[0].Reason)
		return
	}
	if patch.BreakerOpenForSeconds != nil {
		writeError(w, http.StatusNotImplemented, "breaker_open_for_seconds: "+resilienceUnsupportedSettings[1].Reason)
		return
	}
	if patch.CooldownSeconds != nil {
		writeError(w, http.StatusNotImplemented, "cooldown_seconds: "+resilienceUnsupportedSettings[2].Reason)
		return
	}
	if patch.CooldownMode == nil && len(patch.ChainCooldownModes) == 0 {
		writeError(w, http.StatusBadRequest, "empty resilience settings patch: nothing to change")
		return
	}
	update := RoutingUpdate{CooldownMode: patch.CooldownMode, ChainCooldownModes: patch.ChainCooldownModes}
	if s.routingPersist != nil {
		if err := s.routingPersist(update); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save resilience settings")
			return
		}
	}
	if rep, ok := s.engine.(cooldownReporter); ok {
		if patch.CooldownMode != nil {
			rep.SetCooldownMode(*patch.CooldownMode)
		}
		for model, m := range patch.ChainCooldownModes {
			rep.SetChainCooldownMode(model, m)
		}
	}
	writeJSON(w, http.StatusOK, s.resilienceSettings(r))
}

// resilienceBreakerResetRequest is one POST
// /api/resilience/breakers/reset body: exactly one of Provider
// (reset that provider's breaker) or All (reset every known
// breaker).
type resilienceBreakerResetRequest struct {
	Provider string `json:"provider"`
	All      bool   `json:"all"`
}

// resilienceBreakerResetResponse is the POST
// /api/resilience/breakers/reset payload: the providers actually
// reset, how many, and the fresh live breaker rows so the caller
// can see the reset took effect without a second round-trip.
type resilienceBreakerResetResponse struct {
	Reset      []string                `json:"reset"`
	ResetCount int                     `json:"reset_count"`
	Breakers   []resilienceBreakerJSON `json:"breakers"`
}

// handleResilienceBreakerReset serves POST
// /api/resilience/breakers/reset. Unknown provider ids answer
// 404 (nothing was reset); an engine without breakerResetter
// answers 501 — router.Engine is NOT such an engine (it
// implements ResetBreaker / ResetAllBreakers), so the
// production answer is a real reset. Caveat: reset clears
// breaker state only, so a provider that also holds a
// short-lived per-key Retry-After/dead-key cooldown can still
// be skipped until that cooldown expires (seconds, per-key,
// not provider-wide).
func (s *Server) handleResilienceBreakerReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req resilienceBreakerResetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid breaker reset: expected {\"provider\": string} or {\"all\": true}")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	if !req.All && req.Provider == "" {
		writeError(w, http.StatusBadRequest, "breaker reset needs a provider or {\"all\": true}")
		return
	}
	resetter, ok := s.engine.(breakerResetter)
	if !ok || resetter == nil {
		writeError(w, http.StatusNotImplemented, "breaker reset has no backing: engine backing this request exposes no breaker-reset method (interface not implemented)")
		return
	}
	if req.All {
		reset := resetter.ResetAllBreakers()
		if reset == nil {
			reset = []string{}
		}
		sort.Strings(reset)
		writeJSON(w, http.StatusOK, resilienceBreakerResetResponse{Reset: reset, ResetCount: len(reset), Breakers: s.resilienceFeed(r).Breakers})
		return
	}
	if !resetter.ResetBreaker(req.Provider) {
		writeError(w, http.StatusNotFound, "no breaker known for provider "+req.Provider)
		return
	}
	writeJSON(w, http.StatusOK, resilienceBreakerResetResponse{Reset: []string{req.Provider}, ResetCount: 1, Breakers: s.resilienceFeed(r).Breakers})
}

// resilienceCooldownSetRequest is one POST /api/resilience/cooldowns
// body: the model (chain) whose cooldown mode is set, and the mode.
// An empty mode is a 400 here — clearing an override is DELETE
// /api/resilience/cooldowns/{model...}.
type resilienceCooldownSetRequest struct {
	Model string `json:"model"`
	Mode  string `json:"mode"`
}

// handleResilienceCooldownSet serves POST /api/resilience/cooldowns:
// set one model's cooldown mode (hard / soft-demote). Persist runs
// first and its failure leaves routing unchanged, mirroring
// handleCooldownUpdate. An engine without cooldownReporter answers
// 501.
func (s *Server) handleResilienceCooldownSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req resilienceCooldownSetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid per-model cooldown: expected {\"model\": string, \"mode\": \"hard\"|\"soft-demote\"}")
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "per-model cooldown needs a model")
		return
	}
	if req.Mode == "" {
		writeError(w, http.StatusBadRequest, "per-model cooldown needs a mode; use DELETE to clear the override")
		return
	}
	if !router.IsCooldownMode(req.Mode) {
		writeError(w, http.StatusBadRequest, "unknown cooldown mode "+req.Mode)
		return
	}
	if s.routingPersist != nil {
		if err := s.routingPersist(RoutingUpdate{ChainCooldownModes: map[string]string{req.Model: req.Mode}}); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save per-model cooldown")
			return
		}
	}
	rep, ok := s.engine.(cooldownReporter)
	if !ok || rep == nil {
		writeError(w, http.StatusNotImplemented, "per-model cooldown has no backing: the engine does not report cooldown state")
		return
	}
	rep.SetChainCooldownMode(req.Model, req.Mode)
	writeJSON(w, http.StatusOK, s.resilienceSettings(r))
}

// handleResilienceCooldownClear serves DELETE
// /api/resilience/cooldowns/{model...}: clear one model's
// cooldown-mode override so it inherits the global mode again.
// The pattern uses the multi-segment wildcard {model...} because
// model ids contain a slash ("openai/gpt-5") — a single-segment
// {model} would never match. Clearing is idempotent — a model
// with no override still answers 200 with the fresh settings feed.
func (s *Server) handleResilienceCooldownClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	model := strings.TrimSpace(r.PathValue("model"))
	if model == "" {
		writeError(w, http.StatusBadRequest, "per-model cooldown clear needs a model")
		return
	}
	if s.routingPersist != nil {
		if err := s.routingPersist(RoutingUpdate{ChainCooldownModes: map[string]string{model: ""}}); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save per-model cooldown clear")
			return
		}
	}
	rep, ok := s.engine.(cooldownReporter)
	if !ok || rep == nil {
		writeError(w, http.StatusNotImplemented, "per-model cooldown has no backing: the engine does not report cooldown state")
		return
	}
	rep.SetChainCooldownMode(model, "")
	writeJSON(w, http.StatusOK, s.resilienceSettings(r))
}
