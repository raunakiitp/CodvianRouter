// Decision transparency (CHECKLIST 199): the routing decision behind
// every served response — which provider served it, under which
// strategy, and why. The reason is computed by the engine at the
// moment a provider is chosen to serve (non-streaming success, stream
// commit, or the single embeddings/rerank attempt) and recorded on
// RoutingInfo, so the server can report it on the response as the
// X-Codvian-Decision header without re-deriving anything — the header
// and the engine's own record can never disagree.
//
// Reason vocabulary (stable slugs; see docs/decision-header.md):
//
//   - fallback-after-N: N earlier targets in the chain did not serve
//     this request (failed, or skipped before dialling) before this
//     provider did. A hedged loser is NOT counted: it ran in
//     parallel and lost the race, so the winner did not fall back
//     from it. N counts trace attempts, so a zero-quota skip and a
//     breaker skip count exactly like a 429 — all three are targets
//     that did not serve.
//   - group: the request named a routing group, and the first member
//     served it (no fallback).
//   - contract: a per-request routing contract (order / only /
//     exclude / sort / max_price / data_collection) shaped the chain
//     and the first target served it (no fallback).
//   - tree: a nestable strategy tree ordered the chain and the first
//     target served it (no fallback).
//   - cheapest / latency / reliability / balanced / weighted / ...:
//     no more specific reason applies — the strategy itself is the
//     reason (see StrategyReason for the mapping).
//
// Precedence is deliberate: a fallback is the most specific fact
// about why THIS provider served (the first choice did not), so it
// wins over group/contract/tree; group wins over contract (a group
// IS the contract's chain); tree is last before the strategy, because
// a tree orders the chain but does not choose the serving target.
package router

import (
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// StrategyReason maps a strategy name to the decision reason it
// implies when no more specific reason (fallback, group, contract,
// tree) applies: the strategy itself is the reason. Cheapest picks
// the cheapest target; fastest and least-latency pick on latency;
// most-reliable picks on measured reliability; code picks on
// expected cost per solved task; balanced and weighted (and any
// future strategy name) report their own name — inventing a
// narrower reason for them would be a lie about what the scorer
// optimised for.
func StrategyReason(strategy string) string {
	switch strategy {
	case "cheapest":
		return "cheapest"
	case "fastest", "least-latency":
		return "latency"
	case "most-reliable":
		return "reliability"
	case "code":
		return "cost-per-solved-task"
	default:
		return strategy
	}
}

// decisionReason derives the decision reason for a provider that is
// about to serve (or just served) a request. trace holds every
// attempt recorded so far for this request — including the failures
// and skips before the winner, but NOT the winner itself when called
// at stream-commit time (the success entry is added when the stream
// ends). contract is the request's routing contract, if any;
// treeOrdered reports whether a strategy tree ordered the chain.
// It never returns "" for a non-empty strategy.
func decisionReason(strategy string, trace *routetrace.RouteTrace, contract *routetrace.RoutingContract, treeOrdered bool) string {
	if trace != nil {
		failedBefore := 0
		for _, a := range trace.Attempts {
			// The winner's own "success" entry (added before the
			// non-streaming record call) is not a fallback step,
			// and neither is a hedged loser: it ran in parallel
			// and lost the race. Everything else — a failure, or
			// a skip before dialling — is a target that did not
			// serve, so it counts.
			if a.Outcome == "success" || a.Outcome == "hedged_cancelled" {
				continue
			}
			failedBefore++
		}
		if failedBefore > 0 {
			return fmt.Sprintf("fallback-after-%d", failedBefore)
		}
	}
	if contract != nil {
		if contract.Group != "" {
			return "group"
		}
		if len(contract.Order) > 0 || len(contract.Only) > 0 || len(contract.Exclude) > 0 ||
			contract.Sort != "" || contract.MaxPrice != nil || contract.DataCollection != "" {
			return "contract"
		}
	}
	if treeOrdered {
		return "tree"
	}
	return StrategyReason(strategy)
}

// recordDecision records the serving provider AND the reason it was
// chosen, for the most recent request. It is recordRoutingProvider
// with the reason attached: same guard (the record must belong to
// this strategy+model pair, so a concurrent request cannot
// misattribute the winner), plus the reason derived from this
// request's trace, contract and tree state. Callers pass the trace
// as it stands at the decision moment — for a stream commit that is
// before the winner's success entry is added, which is exactly
// right: the winner must not count as its own fallback step.
// model is the REQUESTED model (the record guard keys on it);
// servingModel is the model the winner actually served, which may
// differ (an equivalent-model or self-hosted winner serves a
// different model) — the region step is named against the serving
// model, never the requested one: naming it against the requested
// model compared a same-model failure with the same model and
// reported no step at all for every equivalent-model winner.
func (e *Engine) recordDecision(trace *routetrace.RouteTrace, contract *routetrace.RoutingContract, treeOrdered bool, strategy, model, servingModel, providerID string) {
	e.recordRoutingDecision(strategy, model, providerID, decisionReason(strategy, trace, contract, treeOrdered), e.regionStepForWinner(trace, providerID, servingModel))
}

// recordRoutingDecision fills in the serving provider of the most
// recent request (called on success, and at stream commit) together
// with the reason it was chosen. It only touches the record when it
// belongs to the same strategy+model pair, so a concurrent request
// cannot misattribute the winner. An empty reason leaves any
// previously recorded reason untouched — a decision recorded at
// stream commit keeps its reason when the stream's success path
// records the same provider again without one. The same rule holds
// for regionStep (CHECKLIST 257): an empty step leaves a
// previously recorded step untouched.
func (e *Engine) recordRoutingDecision(strategy, model, providerID, reason, regionStep string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.lastRoutingOK || e.lastRouting.Strategy != strategy || e.lastRouting.Model != model {
		return
	}
	e.lastRouting.Provider = providerID
	if reason != "" {
		e.lastRouting.Reason = reason
	}
	if regionStep != "" {
		e.lastRouting.RegionStep = regionStep
	}
}
