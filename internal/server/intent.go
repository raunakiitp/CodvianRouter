// Intent tiers + metadata-only learning suggestions (CHECKLIST
// 259-260, research-log entry 234 — Aiport): the server feed for
// GET/POST /dashboard/intent-tiers and GET/POST
// /dashboard/suggestions, the per-request outcome hook that feeds
// the learning loop, and the unmapped-tier refusal on the live
// request path.
//
// Privacy: every payload here is counts, model names and taxonomy
// slugs only — no prompts, no answers, no message content, no
// keys, no base URLs. Suggestions are computed from real recorded
// outcomes (see internal/autorouter suggestions.go); an empty
// feed is an honest empty state, never invented evidence.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// intentTierRow is one intent tier in GET /dashboard/intent-tiers:
// the tier, its virtual-model name, whether it is mapped, and the
// (model, effort) pair when mapped. An unmapped tier is reported
// as mapped=false with no model — never a silent default.
type intentTierRow struct {
	Tier         autorouter.IntentTier `json:"tier"`
	VirtualModel string                `json:"virtual_model"`
	Mapped       bool                  `json:"mapped"`
	Model        string                `json:"model,omitempty"`
	Effort       autorouter.Effort     `json:"effort,omitempty"`
}

// intentTiersResponse is the GET /dashboard/intent-tiers payload.
// Persisted is always false: tier mappings set here (or by an
// approved suggestion) apply at runtime only — a restart restores
// the config file's auto_router.intent_tiers, and the dashboard
// says so instead of implying permanence.
type intentTiersResponse struct {
	Tiers      []intentTierRow `json:"tiers"`
	Configured bool            `json:"configured"`
	Persisted  bool            `json:"persisted"`
	Note       string          `json:"note"`
}

const intentTiersNote = "Intent tiers are operator-defined: a request naming intent/<tier> resolves to that tier's (model, effort) pair. Changes made here apply at runtime only — edit auto_router.intent_tiers in the config file to make them permanent. An unmapped tier answers with an honest error naming the tier, never a silent default model."

// intentTiersFeed builds the live intent-tiers feed from the
// auto-router. It reports Configured=false (with all tiers
// unmapped) when no auto-router is wired, so the dashboard shows
// an honest empty state instead of inventing mappings.
func (s *Server) intentTiersFeed() intentTiersResponse {
	resp := intentTiersResponse{
		Tiers:      []intentTierRow{},
		Configured: s.autoRouter != nil,
		Persisted:  false,
		Note:       intentTiersNote,
	}
	for _, tier := range autorouter.AllIntentTiers {
		row := intentTierRow{Tier: tier, VirtualModel: autorouter.IntentModelName(tier)}
		if s.autoRouter != nil {
			if m, ok := s.autoRouter.ResolveIntent(tier); ok {
				row.Mapped = true
				row.Model = m.Model
				row.Effort = m.Effort
			}
		}
		resp.Tiers = append(resp.Tiers, row)
	}
	return resp
}

// handleIntentTiers serves GET /dashboard/intent-tiers.
func (s *Server) handleIntentTiers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.intentTiersFeed())
}

// intentTierUpdate is one POST /dashboard/intent-tiers change: the
// operator's definition of what a tier means. Effort is optional
// ("" = no effort pinned).
type intentTierUpdate struct {
	Tier   string `json:"tier"`
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

// handleIntentTiersUpdate serves POST /dashboard/intent-tiers: set
// (or redefine) one intent tier's mapping. Unknown tiers, empty
// models and unknown efforts are 400s naming the problem — a bad
// definition is never stored. Returns the live feed.
func (s *Server) handleIntentTiersUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update intentTierUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, `invalid intent-tier update: expected {"tier": "fast"|"balanced"|"best", "model": "...", "effort": "low"|"medium"|"high"}`)
		return
	}
	tier := autorouter.IntentTier(strings.TrimSpace(update.Tier))
	if !autorouter.IsIntentTier(string(tier)) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown intent tier %q (want fast, balanced or best)", update.Tier))
		return
	}
	effort, err := autorouter.ParseEffort(update.Effort)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.autoRouter.SetIntentTier(tier, autorouter.IntentMapping{Model: strings.TrimSpace(update.Model), Effort: effort}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.intentTiersFeed())
}

// suggestionsResponse is the GET /dashboard/suggestions payload:
// the pending suggestions (computed from real recorded outcomes),
// the per-(task, model) stats behind them, and the applied-change
// log for this run. Empty slices are honest empty states.
type suggestionsResponse struct {
	Suggestions  []autorouter.Suggestion     `json:"suggestions"`
	Stats        []autorouter.TaskModelStats `json:"stats"`
	Log          []autorouter.AppliedChange  `json:"log"`
	MinSamples   int                         `json:"min_samples"`
	SuccessFloor float64                     `json:"success_floor"`
	Rules        string                      `json:"rules"`
}

const suggestionsRules = "A suggestion needs real evidence: at least 10 recorded requests on each side. An intent tier is remapped only when another model beat its current model on the same task — success rate higher by 10 points, or mean latency 1.5x lower at no worse success. A model is demoted from an autorouter tier only when its success rate on that tier's task is below 70% over at least 10 requests. Nothing is applied without approval, and every approval can be reverted. Prompts and responses are never stored — only task, model, effort, latency, success and a failure-reason slug."

// handleSuggestions serves GET /dashboard/suggestions.
func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.suggestionsFeed())
}

// writeSuggestionError maps an apply/revert failure to its honest
// status: a suggestion that is no longer pending, a stale mapping
// or an already-reverted change is a 409 (the state moved under
// the operator); an unknown id is a 404; anything else is a 500.
func writeSuggestionError(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not pending"), strings.Contains(msg, "stale"), strings.Contains(msg, "already reverted"):
		writeError(w, http.StatusConflict, msg)
	case strings.Contains(msg, "unknown"):
		writeError(w, http.StatusNotFound, msg)
	default:
		writeServerError(w, http.StatusInternalServerError, msg)
	}
}

// suggestionsFeed builds the live suggestions feed from the
// auto-router's recorder and suggestion log.
func (s *Server) suggestionsFeed() suggestionsResponse {
	suggs := s.autoRouter.Suggestions()
	if suggs == nil {
		suggs = []autorouter.Suggestion{}
	}
	stats := s.autoRouter.OutcomeStats()
	if stats == nil {
		stats = []autorouter.TaskModelStats{}
	}
	log := s.autoRouter.SuggestionLog()
	if log == nil {
		log = []autorouter.AppliedChange{}
	}
	return suggestionsResponse{
		Suggestions:  suggs,
		Stats:        stats,
		Log:          log,
		MinSamples:   autorouter.SuggestionMinSamples,
		SuccessFloor: autorouter.SuggestionSuccessFloor,
		Rules:        suggestionsRules,
	}
}

// suggestionAction is one POST /dashboard/suggestions change:
// approve applies a pending suggestion by id; revert restores a
// previously applied change by its log id.
type suggestionAction struct {
	Action       string `json:"action"`
	SuggestionID string `json:"suggestion_id,omitempty"`
	ChangeID     string `json:"change_id,omitempty"`
}

// handleSuggestionsAction serves POST /dashboard/suggestions.
// Unknown ids are 404s; approving a spent suggestion or
// reverting an already-reverted change is a 409 — the log, not a
// silent success, is the record of what happened.
func (s *Server) handleSuggestionsAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var action suggestionAction
	if err := json.NewDecoder(r.Body).Decode(&action); err != nil {
		writeError(w, http.StatusBadRequest, `invalid suggestion action: expected {"action": "approve"|"revert", "suggestion_id": "..."} or {"action": "revert", "change_id": "..."}`)
		return
	}
	switch strings.ToLower(strings.TrimSpace(action.Action)) {
	case "approve":
		if strings.TrimSpace(action.SuggestionID) == "" {
			writeError(w, http.StatusBadRequest, "approve needs a suggestion_id")
			return
		}
		if _, err := s.autoRouter.ApplySuggestion(strings.TrimSpace(action.SuggestionID)); err != nil {
			writeSuggestionError(w, err)
			return
		}
	case "revert":
		if strings.TrimSpace(action.ChangeID) == "" {
			writeError(w, http.StatusBadRequest, "revert needs a change_id")
			return
		}
		if _, err := s.autoRouter.RevertChange(strings.TrimSpace(action.ChangeID)); err != nil {
			writeSuggestionError(w, err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown suggestion action %q (want approve or revert)", action.Action))
		return
	}
	writeJSON(w, http.StatusOK, s.suggestionsFeed())
}

// ---------------------------------------------------------------------------
// Outcome hook: feeds the metadata-only learning loop.
// ---------------------------------------------------------------------------

// recordIntentOutcome records one finished chat request as a
// metadata-only routing outcome (task, model, effort, latency,
// success, failure-reason slug — never content). It is called
// from recordMetrics, so it runs at every chat-completion exit
// point (success, engine error, guardrail block) and on the
// messages/responses paths that share recordMetrics. Best-effort
// and silent: outcome recording must never fail a served request.
//
// Attribution follows the same honesty rules as recordMetrics:
// a proactive cache hit called no provider, so it records
// nothing; the task comes from the request's own routing decision
// (the intent tier, or the auto-router tier that won) — a plain
// model request has no routing task and records nothing; the
// model is the concrete model that served (upstream echo, else
// the trace's last attempt, else the requested model). The
// decision and trace come from this request's attribution sink
// (CHECKLIST 317-318) — the engine published this request's own
// decision/trace into it — never from the engine-global
// last-decision/last-trace read, where a neighbour's decision
// could land first under concurrency and the outcome would be
// recorded against the wrong (task, model) cell. The global read
// is only the fallback when no sink was attached (older engines,
// test doubles): those recordings are approximate telemetry,
// never billing data.
func (s *Server) recordIntentOutcome(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse, err error, started time.Time) {
	if s.autoRouter == nil {
		return
	}
	// A proactive cache hit called no provider: no routing
	// outcome to learn from (and the trace would be stale).
	if chr, ok := s.engine.(cacheHitReporter); ok {
		if _, _, hit := chr.LastCacheHit(); hit {
			return
		}
	}
	var task string
	var effort autorouter.Effort
	if tier, ok := autorouter.ParseIntentModel(req.Model); ok {
		task = req.Model
		if dec, ok := s.requestDecision(ctx); ok && dec.IntentTier == tier {
			effort = dec.Effort
		}
	} else if autorouter.IsVirtualModel(req.Model) {
		dec, ok := s.requestDecision(ctx)
		if !ok || dec.Tier == "" {
			return
		}
		task = string(dec.Tier)
	} else {
		return
	}
	var last *routetrace.AttemptTrace
	if trace := s.requestTrace(ctx); trace != nil && len(trace.Attempts) > 0 {
		last = &trace.Attempts[len(trace.Attempts)-1]
	}
	model := req.Model
	if last != nil && last.Model != "" {
		model = last.Model
	}
	if resp != nil && resp.Model != "" {
		model = resp.Model
	}
	// The model must be concrete by now: a still-virtual name
	// means the request never reached a resolved provider (a
	// path that bypassed the engine), and recording it would
	// attribute outcomes to a model that never served anything.
	if autorouter.IsVirtualModel(model) {
		return
	}
	ok := err == nil && resp != nil
	reason := ""
	if !ok {
		reason = outcomeFailureReason(err, last)
	}
	s.autoRouter.RecordOutcome(autorouter.Outcome{
		Task:      task,
		Model:     model,
		Effort:    effort,
		LatencyMs: time.Since(started).Milliseconds(),
		OK:        ok,
		Reason:    reason,
	})
}

// outcomeFailureReason classifies a failed request into a coarse
// metadata slug for the learning loop. Guardrail blocks and
// nil-response failures get their own slugs (nothing upstream
// failed there); otherwise the trace's error-taxonomy reason is
// used when present, and "upstream_error" is the fallback —
// never a raw error string, which could carry content.
func outcomeFailureReason(err error, last *routetrace.AttemptTrace) string {
	if errors.Is(err, errAsyncGuardrailBlocked) || errors.Is(err, errGuardrailResponseBlocked) {
		return "guardrail_blocked"
	}
	if errors.Is(err, errNilUpstreamResponse) {
		return "no_response"
	}
	if last != nil {
		if last.Reason != "" {
			return last.Reason
		}
		if last.Outcome != "" && last.Outcome != "success" {
			return last.Outcome
		}
	}
	return "upstream_error"
}

// checkIntentTier refuses a chat-completions request naming an
// intent tier that has no mapping (or no auto-router at all)
// before any provider is dialled. It returns true when it refused.
// A mapped tier passes through untouched: the engine resolves it
// via AutoRoute, so the decision, headers and trace all carry
// the intent.
func (s *Server) checkIntentTier(w http.ResponseWriter, r *http.Request, model string) bool {
	tier, ok := autorouter.ParseIntentModel(model)
	if !ok {
		return false
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return true
	}
	if _, mapped := s.autoRouter.ResolveIntent(tier); !mapped {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("intent tier %q is not mapped: set auto_router.intent_tiers.%s in the config file (or POST /dashboard/intent-tiers) — no silent default model is used", tier, tier))
		return true
	}
	return false
}
