package router

import (
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// Multi-region failover order (CHECKLIST 257-258, research-log
// entry 232 — Parasail multi-region routing): failover order is
// region-aware. The cross-region penalty (180-220 ms US-East ->
// APAC) is far cheaper than a model switch that rebuilds the
// prompt cache, so the order is:
//
//  1. the same model in another region (preserves the model and
//     the prompt-cache shape);
//  2. an equivalent model from another provider;
//  3. a self-hosted open-weight model.
//
// Region is a first-class routing dimension, operator-declared on
// provider/model entries (provider.Provider.Region / Model.Region)
// and never guessed: undeclared means unknown (""), never a
// default region. The ordering below only ever moves an attempt
// on facts the operator declared; an unknown region never
// promotes or demotes an attempt into the cross-region tier.
//
// The existing failover rules still apply inside each step: only
// 429/5xx/timeout fail over (the loops decide that, never this
// file), Retry-After is honoured, and pin escapes still work
// (pinFirst runs after ordering, so a pin still leads).

// Region failover step slugs. They name the step on the trace
// (AttemptTrace.FailoverStep), in the decision header
// (RoutingInfo.RegionStep) and in the live counts below — one
// vocabulary, so the three surfaces can never disagree.
const (
	// StepCrossRegion is the same model in a different declared
	// region from the attempt that just failed.
	StepCrossRegion = "cross-region"
	// StepEquivalentModel is a different model from another
	// provider (an operator-declared equivalent — see
	// config.RoutingConfig.ModelEquivalents).
	StepEquivalentModel = "equivalent-model"
	// StepSelfHosted is a self-hosted open-weight model: the
	// last resort, after every hosted equivalent.
	StepSelfHosted = "self-hosted"
)

// Failover257Stats is the engine's live account of region-aware
// failover steps: how many requests actually stepped to each
// tier after a retryable failure, and which step happened most
// recently. It is counts only — no request content, no prompt
// text. Zero until a step happens, never invented.
type Failover257Stats struct {
	CrossRegion     uint64    `json:"cross_region"`
	EquivalentModel uint64    `json:"equivalent_model"`
	SelfHosted      uint64    `json:"self_hosted"`
	LastStep        string    `json:"last_step,omitempty"`
	LastProvider    string    `json:"last_provider,omitempty"`
	LastModel       string    `json:"last_model,omitempty"`
	LastAt          time.Time `json:"last_at,omitempty"`
}

// Failover257Stats returns a snapshot of the engine's live
// region-aware failover outcomes (see Failover257Stats).
func (e *Engine) Failover257Stats() Failover257Stats {
	e.fail257Mu.Lock()
	defer e.fail257Mu.Unlock()
	return Failover257Stats{
		CrossRegion:     e.fail257CrossRegion,
		EquivalentModel: e.fail257Equivalent,
		SelfHosted:      e.fail257SelfHosted,
		LastStep:        e.fail257LastStep,
		LastProvider:    e.fail257LastProvider,
		LastModel:       e.fail257LastModel,
		LastAt:          e.fail257LastAt,
	}
}

// noteRegionFailover counts one request that actually stepped to
// a region-aware failover tier: the previous target failed
// retryably and the loop moved on to an attempt in a different
// tier. step must be one of the Step* slugs; "" counts nothing
// (a same-model same-region hop to another provider is a plain
// provider failover, not a region step).
func (e *Engine) noteRegionFailover(step, providerID, model string) {
	if step != StepCrossRegion && step != StepEquivalentModel && step != StepSelfHosted {
		return
	}
	e.fail257Mu.Lock()
	defer e.fail257Mu.Unlock()
	switch step {
	case StepCrossRegion:
		e.fail257CrossRegion++
	case StepEquivalentModel:
		e.fail257Equivalent++
	case StepSelfHosted:
		e.fail257SelfHosted++
	}
	e.fail257LastStep = step
	e.fail257LastProvider = providerID
	e.fail257LastModel = model
	e.fail257LastAt = time.Now()
}

// regionOf resolves the effective serving region for one
// (provider, model) pair through Config.RegionFor (nil-safe:
// no RegionFor means every region is unknown, so no attempt is
// ever promoted into the cross-region tier).
func (e *Engine) regionOf(providerID, model string) string {
	if e.cfg.RegionFor == nil {
		return ""
	}
	return e.cfg.RegionFor(providerID, model)
}

// isSelfHosted reports whether a provider is self-hosted
// through Config.SelfHosted (nil-safe: no hook means nothing is
// self-hosted, so nothing is ever demoted to the last tier).
func (e *Engine) isSelfHosted(providerID string) bool {
	return e.cfg.SelfHosted != nil && e.cfg.SelfHosted(providerID)
}

// regionStepBetween names the region-aware failover step from a
// failed attempt (prevProvider/prevModel) to the next attempt
// (curProvider/curModel). "" means no region step: a retry of
// the same target, or a same-model hop within the same region
// (or where either region is unknown — an unknown region can
// never prove a cross-region hop). Self-hosted wins over the
// model comparison: a self-hosted target is the last tier even
// when it serves the same model.
func (e *Engine) regionStepBetween(prevProvider, prevModel, curProvider, curModel string) string {
	if e.isSelfHosted(curProvider) {
		return StepSelfHosted
	}
	if curModel == prevModel {
		prevRegion, curRegion := e.regionOf(prevProvider, prevModel), e.regionOf(curProvider, curModel)
		if prevRegion != "" && curRegion != "" && prevRegion != curRegion {
			return StepCrossRegion
		}
		return ""
	}
	return StepEquivalentModel
}

// regionTier ranks one attempt against the chain's primary
// (ordered[0]) for the region-aware order: 0 = same model in
// another declared region, 1 = same model in the same region
// (or unknown region), 2 = a different (equivalent) model,
// 3 = self-hosted. The primary itself is never tiered —
// applyRegionTiers keeps it first whatever its tier would be.
func (e *Engine) regionTier(att, primary Attempt) int {
	if e.isSelfHosted(att.ProviderID) {
		return 3
	}
	if att.Model == primary.Model {
		primaryRegion, region := e.regionOf(primary.ProviderID, primary.Model), e.regionOf(att.ProviderID, att.Model)
		if primaryRegion != "" && region != "" && region != primaryRegion {
			return 0
		}
		return 1
	}
	return 2
}

// applyRegionTiers stably partitions an already-ordered chain
// into the region-aware failover order, keeping the primary
// (ordered[0]) first: same model in another region, then same
// model in the same region, then equivalent models, then
// self-hosted. Within each tier the input order stands, so the
// quota-hint sort and the strategy hook still rank targets
// inside a tier — the region order only decides which tier is
// tried before which. It never drops, duplicates or resurrects
// an attempt. With no RegionFor/SelfHosted configured every
// non-self-hosted same-model attempt lands in tier 1 and every
// other model in tier 2, so the order is unchanged apart from
// self-hosted demotion — and with neither hook the input order
// is returned untouched.
func (e *Engine) applyRegionTiers(ordered []Attempt) []Attempt {
	if len(ordered) < 2 {
		return ordered
	}
	primary := ordered[0]
	var tiers [4][]Attempt
	for _, att := range ordered[1:] {
		t := e.regionTier(att, primary)
		tiers[t] = append(tiers[t], att)
	}
	out := make([]Attempt, 0, len(ordered))
	out = append(out, primary)
	for _, tier := range tiers {
		out = append(out, tier...)
	}
	return out
}

// isFailoverFailureOutcome reports whether a trace outcome is a
// retryable failure that a failover stepped from: a provider
// error, a rate limit, a stream error, or an empty stream.
// Skips (never dialled), content refusals (policy routing, not
// failure failover), hedged losers (a race, not a failure) and
// successes are not failover triggers.
func isFailoverFailureOutcome(outcome string) bool {
	switch outcome {
	case "error", "rate_limited", "stream_error", "empty_stream":
		return true
	}
	return false
}

// noteRegionFailoverFromTrace counts the region step of the
// attempt about to be tried (att), when the trace shows the
// request already failed retryably on an earlier target. It
// walks the trace backwards to the most recent failure and
// names the step from that failure to att; a step of "" counts
// nothing. lastCounted is the per-request guard: the trace
// index of the last failure already counted from, so one
// failure is never counted twice against two later attempts —
// the step is counted against the attempt that actually follows
// the failure, and a later attempt counts against its own
// preceding failure instead. A same-target retry and a
// same-region hop count nothing and leave lastCounted alone,
// so a later attempt still counts against the failure that
// precedes it.
func (e *Engine) noteRegionFailoverFromTrace(trace *routetrace.RouteTrace, att Attempt, lastCounted *int) {
	if trace == nil || lastCounted == nil {
		return
	}
	for i := len(trace.Attempts) - 1; i >= 0; i-- {
		if !isFailoverFailureOutcome(trace.Attempts[i].Outcome) {
			continue
		}
		if i <= *lastCounted {
			return
		}
		failed := trace.Attempts[i]
		if failed.Provider == att.ProviderID && failed.Model == att.Model {
			return
		}
		if step := e.regionStepBetween(failed.Provider, failed.Model, att.ProviderID, att.Model); step != "" {
			e.noteRegionFailover(step, att.ProviderID, att.Model)
			*lastCounted = i
		}
		return
	}
}

// annotateRegionTrace stamps every attempt in a trace with its
// effective region and, for attempts that follow a retryable
// failure, the region-aware failover step from that failure.
// It is a recompute from the trace contents (idempotent —
// re-annotation rewrites the same values), run in setLastTrace
// so every published trace carries it, whatever path published
// it. Skipped attempts get a region but no step (never dialled);
// content refusals, hedged losers and successes are not failure
// triggers, so a hop after one of them names no step — except
// the serving attempt itself, whose step is named against the
// last failure before it (that is the failover that served the
// request).
func (e *Engine) annotateRegionTrace(t *routetrace.RouteTrace) {
	if t == nil {
		return
	}
	var lastFailure *routetrace.AttemptTrace
	for i := range t.Attempts {
		a := &t.Attempts[i]
		a.Region = e.regionOf(a.Provider, a.Model)
		if strings.HasPrefix(a.Outcome, "skipped_") || a.Outcome == "hedged_cancelled" || a.Outcome == "content_refused" {
			continue
		}
		if lastFailure != nil {
			a.FailoverStep = e.regionStepBetween(lastFailure.Provider, lastFailure.Model, a.Provider, a.Model)
		} else {
			a.FailoverStep = ""
		}
		if isFailoverFailureOutcome(a.Outcome) {
			lastFailure = a
		}
	}
}

// regionStepForWinner names the region-aware failover step of
// the attempt that served a request (providerID/model), against
// the last retryable failure in the trace before it. It walks
// backwards past skips, hedged losers, refusals and the winner's
// own success entry; a retry of the same target that failed
// (winner == failed target) names no step. "" when no failure
// precedes the winner: the first target served, so there was no
// failover step to name. Used for RoutingInfo.RegionStep — the
// decision header's region step — so the header names the step
// the trace already shows for the serving attempt.
func (e *Engine) regionStepForWinner(trace *routetrace.RouteTrace, providerID, model string) string {
	if trace == nil {
		return ""
	}
	for i := len(trace.Attempts) - 1; i >= 0; i-- {
		a := trace.Attempts[i]
		if !isFailoverFailureOutcome(a.Outcome) {
			continue
		}
		if a.Provider == providerID && a.Model == model {
			return ""
		}
		return e.regionStepBetween(a.Provider, a.Model, providerID, model)
	}
	return ""
}
