// Metadata-only learning loop (CHECKLIST 259-260, research-log
// entry 234 — Aiport): per-(task, model) latency/success stats
// drive routing *suggestions* that a human approves and can
// revert. Prompts and responses are NEVER stored: an outcome
// records only the task, the model, the effort, the latency, the
// success/failure flag and a coarse failure-reason slug (the
// route-trace error taxonomy, e.g. "rate_limited" — never a raw
// error string, never content). Suggestions are computed from the
// real recorded outcomes with their evidence counts shown, and
// applying or reverting one is an explicit operator action
// recorded in the log.
//
// A "task" is the routing task that chose the model: for an
// intent request the intent tier ("intent/fast"); for an auto/*
// request the autorouter tier that won ("coding", "chat",
// "reasoning", "quick-qa"). Plain model requests (no routing
// decision) feed nothing — there is no routing choice to learn
// from.
//
// Suggestion kinds (both need real evidence, never a guess):
//   - intent_remap: an intent tier's current model is beaten on
//     the SAME task by another model that actually served it —
//     higher success rate by a margin, or much lower latency at
//     no worse success — with both sides above the sample floor.
//     Approving remaps the tier (effort unchanged); reverting
//     restores the previous mapping.
//   - tier_demote: a model in an autorouter tier's candidate list
//     keeps failing that tier's task (success below the floor,
//     above the sample floor). Approving removes it from that
//     tier's candidates; reverting adds it back. (A demote, not a
//     reorder, because the per-tier ranker already reorders
//     candidates by live cost/latency/quality on every request —
//     a static reorder would be a no-op the moment the ranker
//     runs. Removing a failing candidate is the change the
//     ranker cannot undo.)
//
// Evidence is in-memory and resets on restart (like the auto/code
// outcome evidence): the raw outcomes are the served requests
// themselves, and the suggestion log is the operator's audit
// trail for this run. Nothing is persisted to the config file:
// an approved change applies at runtime only, and the feed says
// so — edit auto_router.intent_tiers (or tier_models) in the
// config file to make it permanent.
package autorouter

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// SuggestionMinSamples is the evidence floor: a side of a
// comparison needs at least this many recorded requests before it
// can support a suggestion. Below it the stats are shown, never
// acted on.
const SuggestionMinSamples = 10

// SuggestionSuccessFloor is the success rate below which a model
// is a demote candidate for its tier (with enough samples).
const SuggestionSuccessFloor = 0.70

// SuggestionSuccessMargin is how much better a challenger must be
// (success rate) to justify remapping an intent tier to it.
const SuggestionSuccessMargin = 0.10

// SuggestionLatencyRatio is how much faster a challenger must be
// (mean latency) to justify remapping an intent tier to it when
// its success rate is no worse.
const SuggestionLatencyRatio = 1.5

// Outcome is one served routing outcome: metadata only. Task is
// the routing task ("intent/fast", or an autorouter tier name);
// Model the concrete model that served it; Effort the resolved
// effort ("" when none was pinned); LatencyMs the measured
// request latency; OK whether the client got a successful
// response; Reason the coarse failure-reason slug ("" on
// success, e.g. "rate_limited", "model_not_found",
// "guardrail_blocked" — never a raw error string, never content).
type Outcome struct {
	Task      string
	Model     string
	Effort    Effort
	LatencyMs int64
	OK        bool
	Reason    string
}

// TaskModelStats is one (task, model) cell of recorded outcomes:
// real counts only. Effort is the most recent non-empty effort
// observed for the cell ("" when no observation pinned one).
// FailureReasons counts the failure-reason slugs (metadata only).
type TaskModelStats struct {
	Task           string         `json:"task"`
	Model          string         `json:"model"`
	Effort         Effort         `json:"effort,omitempty"`
	Requests       int            `json:"requests"`
	Successes      int            `json:"successes"`
	Failures       int            `json:"failures"`
	MeanLatencyMs  float64        `json:"mean_latency_ms"`
	SuccessRate    float64        `json:"success_rate"`
	FailureReasons map[string]int `json:"failure_reasons,omitempty"`
}

// outcomeAcc is the mutable accumulator behind one TaskModelStats
// cell.
type outcomeAcc struct {
	requests     int
	successes    int
	failures     int
	latencyTotal int64
	effort       Effort
	reasons      map[string]int
}

// OutcomeRecorder accumulates metadata-only routing outcomes in
// memory, keyed by (task, model). Safe for concurrent use.
type OutcomeRecorder struct {
	mu    sync.Mutex
	cells map[string]*outcomeAcc
}

// NewOutcomeRecorder returns an empty recorder.
func NewOutcomeRecorder() *OutcomeRecorder {
	return &OutcomeRecorder{cells: make(map[string]*outcomeAcc)}
}

// Record adds one outcome. Outcomes with an empty task or model
// are ignored: a routing outcome without a task or a model names
// nothing to learn about.
func (rec *OutcomeRecorder) Record(o Outcome) {
	if rec == nil || o.Task == "" || o.Model == "" {
		return
	}
	if o.LatencyMs < 0 {
		o.LatencyMs = 0
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	key := o.Task + "\x00" + o.Model
	acc, ok := rec.cells[key]
	if !ok {
		acc = &outcomeAcc{reasons: make(map[string]int)}
		rec.cells[key] = acc
	}
	acc.requests++
	if o.OK {
		acc.successes++
	} else {
		acc.failures++
		if o.Reason != "" {
			acc.reasons[o.Reason]++
		}
	}
	acc.latencyTotal += o.LatencyMs
	if o.Effort != "" {
		acc.effort = o.Effort
	}
}

// Stats returns one row per observed (task, model) cell, sorted by
// task then model. Rows are real recorded counts only — a cell
// with no observations never appears.
func (rec *OutcomeRecorder) Stats() []TaskModelStats {
	if rec == nil {
		return []TaskModelStats{}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]TaskModelStats, 0, len(rec.cells))
	for key, acc := range rec.cells {
		// The key is task + "\x00" + model; split on the separator.
		task, model := key, ""
		for i := 0; i < len(key); i++ {
			if key[i] == 0 {
				task, model = key[:i], key[i+1:]
				break
			}
		}
		row := TaskModelStats{
			Task:      task,
			Model:     model,
			Effort:    acc.effort,
			Requests:  acc.requests,
			Successes: acc.successes,
			Failures:  acc.failures,
		}
		if acc.requests > 0 {
			row.MeanLatencyMs = float64(acc.latencyTotal) / float64(acc.requests)
			row.SuccessRate = float64(acc.successes) / float64(acc.requests)
		}
		if len(acc.reasons) > 0 {
			row.FailureReasons = make(map[string]int, len(acc.reasons))
			for r, n := range acc.reasons {
				row.FailureReasons[r] = n
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// Clear drops all recorded outcomes (tests, and a fresh start).
func (rec *OutcomeRecorder) Clear() {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.cells = make(map[string]*outcomeAcc)
}

// ---------------------------------------------------------------------------
// Suggestions.
// ---------------------------------------------------------------------------

// SuggestionKind is the kind of a routing suggestion.
type SuggestionKind string

// Suggestion kinds.
const (
	// SuggestionIntentRemap proposes remapping an intent tier to
	// a different model that beat the current one on the same task.
	SuggestionIntentRemap SuggestionKind = "intent_remap"
	// SuggestionTierDemote proposes removing a failing model
	// from one autorouter tier's candidate list.
	SuggestionTierDemote SuggestionKind = "tier_demote"
)

// SuggestionEvidence is the real recorded evidence behind a
// suggestion: the incumbent's stats, and the challenger's when
// the suggestion compares two models (nil for a demote, which
// judges one model against the floor).
type SuggestionEvidence struct {
	Current    TaskModelStats  `json:"current"`
	Challenger *TaskModelStats `json:"challenger,omitempty"`
}

// Suggestion is one human-approvable routing suggestion, computed
// from real recorded outcomes. ID is stable for its content
// (kind + task + current + suggested), so the dashboard can
// approve or revert it by id and a recompute finds the same
// suggestion again while the evidence still supports it.
type Suggestion struct {
	ID             string             `json:"id"`
	Kind           SuggestionKind     `json:"kind"`
	Task           string             `json:"task"`
	CurrentModel   string             `json:"current_model"`
	SuggestedModel string             `json:"suggested_model,omitempty"`
	Effort         Effort             `json:"effort,omitempty"`
	Reason         string             `json:"reason"`
	Evidence       SuggestionEvidence `json:"evidence"`
}

// SuggestionID builds the stable id for a suggestion's content.
func SuggestionID(kind SuggestionKind, task, current, suggested string) string {
	return string(kind) + ":" + task + ":" + current + ":" + suggested
}

// ComputeSuggestions computes the pending suggestions from real
// recorded stats. intent maps each mapped intent tier to its
// current (model, effort); tiers maps each autorouter tier to its
// current candidate model list. No stats, no suggestions —
// nothing is ever suggested from invented or zero evidence.
func ComputeSuggestions(stats []TaskModelStats, intent map[IntentTier]IntentMapping, tiers map[Tier][]string) []Suggestion {
	byTask := make(map[string]map[string]TaskModelStats)
	for _, s := range stats {
		if byTask[s.Task] == nil {
			byTask[s.Task] = make(map[string]TaskModelStats)
		}
		byTask[s.Task][s.Model] = s
	}
	out := []Suggestion{}
	// Intent remaps: the incumbent must have enough evidence,
	// and a challenger on the SAME task must beat it.
	for _, tier := range AllIntentTiers {
		mapping, ok := intent[tier]
		if !ok || mapping.Model == "" {
			continue
		}
		task := IntentModelName(tier)
		incumbent, ok := byTask[task][mapping.Model]
		if !ok || incumbent.Requests < SuggestionMinSamples {
			continue
		}
		var best *TaskModelStats
		for model, cand := range byTask[task] {
			if model == mapping.Model || cand.Requests < SuggestionMinSamples {
				continue
			}
			c := cand
			if best == nil || c.SuccessRate > best.SuccessRate ||
				(c.SuccessRate == best.SuccessRate && c.MeanLatencyMs < best.MeanLatencyMs) {
				best = &c
			}
		}
		if best == nil {
			continue
		}
		betterRate := best.SuccessRate >= incumbent.SuccessRate+SuggestionSuccessMargin
		faster := best.MeanLatencyMs > 0 && incumbent.MeanLatencyMs > 0 &&
			best.MeanLatencyMs*SuggestionLatencyRatio < incumbent.MeanLatencyMs &&
			best.SuccessRate >= incumbent.SuccessRate
		if !betterRate && !faster {
			continue
		}
		why := fmt.Sprintf("%s beat %s on %s: success %.0f%% (%d/%d requests) vs %.0f%% (%d/%d), mean latency %.0f ms vs %.0f ms",
			best.Model, incumbent.Model, task,
			100*best.SuccessRate, best.Successes, best.Requests,
			100*incumbent.SuccessRate, incumbent.Successes, incumbent.Requests,
			best.MeanLatencyMs, incumbent.MeanLatencyMs)
		out = append(out, Suggestion{
			ID:             SuggestionID(SuggestionIntentRemap, task, incumbent.Model, best.Model),
			Kind:           SuggestionIntentRemap,
			Task:           task,
			CurrentModel:   incumbent.Model,
			SuggestedModel: best.Model,
			Effort:         mapping.Effort,
			Reason:         why + " — approve to remap the tier (effort unchanged), revert restores the previous mapping.",
			Evidence:       SuggestionEvidence{Current: incumbent, Challenger: best},
		})
	}
	// Tier demotes: a candidate that keeps failing its tier's task.
	for _, tier := range AllTiers {
		for _, model := range tiers[tier] {
			cell, ok := byTask[string(tier)][model]
			if !ok || cell.Requests < SuggestionMinSamples {
				continue
			}
			if cell.SuccessRate >= SuggestionSuccessFloor {
				continue
			}
			out = append(out, Suggestion{
				ID:           SuggestionID(SuggestionTierDemote, string(tier), model, ""),
				Kind:         SuggestionTierDemote,
				Task:         string(tier),
				CurrentModel: model,
				Reason: fmt.Sprintf("%s failed %s too often: success %.0f%% (%d/%d requests, floor %.0f%%) — approve to remove it from the %s tier's candidates, revert adds it back.",
					model, string(tier), 100*cell.SuccessRate, cell.Successes, cell.Requests,
					100*SuggestionSuccessFloor, tier),
				Evidence: SuggestionEvidence{Current: cell},
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].CurrentModel < out[j].CurrentModel
	})
	return out
}

// ---------------------------------------------------------------------------
// Suggestion log: the operator's audit trail for this run.
// ---------------------------------------------------------------------------

// AppliedChange is one approved (and possibly reverted) suggestion
// application: what changed, from what to what, and when. Previous
// / New are human-readable ("model=X effort=Y" for an intent
// remap, "tier_models=[...]" for a demote); the structured
// previous values live in the unexported fields, which is what a
// revert restores.
type AppliedChange struct {
	ID           string         `json:"id"`
	SuggestionID string         `json:"suggestion_id"`
	Kind         SuggestionKind `json:"kind"`
	Task         string         `json:"task"`
	Previous     string         `json:"previous"`
	New          string         `json:"new"`
	AppliedAt    time.Time      `json:"applied_at"`
	Reverted     bool           `json:"reverted"`
	RevertedAt   *time.Time     `json:"reverted_at,omitempty"`
	prevMapping  *IntentMapping
	prevModel    string // demote: the model that was removed
}

// Suggestions returns the currently pending suggestions, computed
// from the real recorded outcomes and the current mappings.
func (r *Router) Suggestions() []Suggestion {
	tiers := make(map[Tier][]string, len(AllTiers))
	for _, tier := range AllTiers {
		tiers[tier] = r.ModelsForTier(tier)
	}
	return ComputeSuggestions(r.OutcomeStats(), r.IntentTiers(), tiers)
}

// OutcomeStats returns the recorded per-(task, model) outcome
// stats (real counts only).
func (r *Router) OutcomeStats() []TaskModelStats {
	if r.outcomes == nil {
		return []TaskModelStats{}
	}
	return r.outcomes.Stats()
}

// RecordOutcome records one served routing outcome (metadata
// only) into the learning loop. Safe for concurrent use.
func (r *Router) RecordOutcome(o Outcome) {
	if r.outcomes == nil {
		return
	}
	r.outcomes.Record(o)
}

// SuggestionLog returns a copy of this run's applied-change log,
// newest first.
func (r *Router) SuggestionLog() []AppliedChange {
	r.suggMu.Lock()
	defer r.suggMu.Unlock()
	out := make([]AppliedChange, 0, len(r.suggLog))
	for i := len(r.suggLog) - 1; i >= 0; i-- {
		out = append(out, r.suggLog[i])
	}
	return out
}

// ApplySuggestion applies one pending suggestion by id: an
// explicit operator action, recorded in the log with the previous
// value so it can be reverted. It recomputes the pending list
// first — an id that is not currently pending (already applied,
// evidence changed, mapping changed) is an error, never a silent
// no-op.
func (r *Router) ApplySuggestion(id string) (AppliedChange, error) {
	r.suggMu.Lock()
	defer r.suggMu.Unlock()
	var found *Suggestion
	for _, s := range r.suggestionsLocked() {
		if s.ID == id {
			c := s
			found = &c
			break
		}
	}
	if found == nil {
		return AppliedChange{}, fmt.Errorf("suggestion %q is not pending (already applied, or the evidence changed)", id)
	}
	r.suggSeq++
	change := AppliedChange{
		ID:           fmt.Sprintf("chg-%d", r.suggSeq),
		SuggestionID: found.ID,
		Kind:         found.Kind,
		Task:         found.Task,
		AppliedAt:    time.Now(),
	}
	switch found.Kind {
	case SuggestionIntentRemap:
		tier, ok := ParseIntentModel(found.Task)
		if !ok {
			return AppliedChange{}, fmt.Errorf("suggestion %q names a task %q that is not an intent tier", id, found.Task)
		}
		prev, ok := r.ResolveIntent(tier)
		if !ok || prev.Model != found.CurrentModel {
			return AppliedChange{}, fmt.Errorf("suggestion %q is stale: intent tier %q no longer maps to %s", id, tier, found.CurrentModel)
		}
		next := IntentMapping{Model: found.SuggestedModel, Effort: found.Effort}
		if err := r.SetIntentTier(tier, next); err != nil {
			return AppliedChange{}, err
		}
		change.Previous = prev.String()
		change.New = next.String()
		cp := prev
		change.prevMapping = &cp
	case SuggestionTierDemote:
		tier := Tier(found.Task)
		prev := r.ModelsForTier(tier)
		if _, removed := r.removeTierModel(tier, found.CurrentModel); !removed {
			return AppliedChange{}, fmt.Errorf("suggestion %q is stale: model %s is no longer a %s candidate", id, found.CurrentModel, tier)
		}
		change.Previous = "tier_models=" + fmt.Sprintf("%v", prev)
		change.New = "tier_models=" + fmt.Sprintf("%v", r.ModelsForTier(tier))
		change.prevModel = found.CurrentModel
	default:
		return AppliedChange{}, fmt.Errorf("suggestion %q has unknown kind %q", id, found.Kind)
	}
	r.suggLog = append(r.suggLog, change)
	return change, nil
}

// RevertChange reverts one applied change by its log id,
// restoring the previous mapping (intent remap) or adding the
// demoted model back to its tier (demote). A change that was
// already reverted is an error, never a double revert.
func (r *Router) RevertChange(changeID string) (AppliedChange, error) {
	r.suggMu.Lock()
	defer r.suggMu.Unlock()
	for i := range r.suggLog {
		if r.suggLog[i].ID != changeID {
			continue
		}
		if r.suggLog[i].Reverted {
			return AppliedChange{}, fmt.Errorf("change %q was already reverted", changeID)
		}
		ch := &r.suggLog[i]
		switch ch.Kind {
		case SuggestionIntentRemap:
			tier, ok := ParseIntentModel(ch.Task)
			if !ok || ch.prevMapping == nil {
				return AppliedChange{}, fmt.Errorf("change %q cannot be reverted (missing previous mapping)", changeID)
			}
			if err := r.SetIntentTier(tier, *ch.prevMapping); err != nil {
				return AppliedChange{}, err
			}
		case SuggestionTierDemote:
			tier := Tier(ch.Task)
			r.addTierModel(tier, ch.prevModel)
		default:
			return AppliedChange{}, fmt.Errorf("change %q has unknown kind %q", changeID, ch.Kind)
		}
		now := time.Now()
		ch.Reverted = true
		ch.RevertedAt = &now
		return *ch, nil
	}
	return AppliedChange{}, fmt.Errorf("unknown change %q", changeID)
}

// suggestionsLocked recomputes the pending suggestions; the caller
// must hold suggMu (it guards the log the apply path appends to).
func (r *Router) suggestionsLocked() []Suggestion {
	tiers := make(map[Tier][]string, len(AllTiers))
	for _, tier := range AllTiers {
		tiers[tier] = r.ModelsForTier(tier)
	}
	return ComputeSuggestions(r.OutcomeStats(), r.IntentTiers(), tiers)
}

// removeTierModel removes one model from one tier's candidate list
// (the demote apply). It reports the previous list and whether
// anything was removed. Safe for concurrent use with Resolve.
func (r *Router) removeTierModel(tier Tier, model string) ([]string, bool) {
	r.tierMu.Lock()
	defer r.tierMu.Unlock()
	cur := r.tierModels[tier]
	prev := append([]string(nil), cur...)
	kept := cur[:0]
	removed := false
	for _, m := range cur {
		if m == model {
			removed = true
			continue
		}
		kept = append(kept, m)
	}
	if !removed {
		return prev, false
	}
	if len(kept) == 0 {
		kept = []string{}
	}
	r.tierModels[tier] = kept
	return prev, true
}

// addTierModel appends one model to one tier's candidate list when
// it is not already there (the demote revert). Safe for concurrent
// use with Resolve.
func (r *Router) addTierModel(tier Tier, model string) {
	if model == "" {
		return
	}
	r.tierMu.Lock()
	defer r.tierMu.Unlock()
	for _, m := range r.tierModels[tier] {
		if m == model {
			return
		}
	}
	r.tierModels[tier] = append(r.tierModels[tier], model)
}

// RemoveTierModel removes one model from one tier's candidate list
// and reports whether it was present. It is the exported form of
// the demote apply, for tests and for callers that manage tiers
// directly.
func (r *Router) RemoveTierModel(tier Tier, model string) bool {
	_, removed := r.removeTierModel(tier, model)
	return removed
}

// SetTierModels replaces one tier's candidate model list wholesale
// (the operator's definition of the tier). An unknown tier is an
// error; an empty list is allowed (the tier then has no candidates
// and falls back to DefaultModel). Safe for concurrent use.
func (r *Router) SetTierModels(tier Tier, models []string) error {
	if !IsTier(string(tier)) {
		return fmt.Errorf("unknown tier %q", tier)
	}
	cp := append([]string(nil), models...)
	r.tierMu.Lock()
	defer r.tierMu.Unlock()
	r.tierModels[tier] = cp
	return nil
}
