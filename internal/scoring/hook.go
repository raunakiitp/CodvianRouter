package scoring

import (
	"sort"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// SignalsFunc looks up the live signals for one attempt: the
// provider AND the model it would serve. Cost is per (provider,
// model) pair — the operator's price book prices pairs, not models
// alone — so the same model at a cheap provider and an expensive
// provider must score differently on cost; a model-only lookup
// would rank both by one price and misprice one of them.
// Latency, success and quality are per provider in practice (the
// engine keeps attempt stats per provider), so a signals func may
// ignore the model for those components. The func must be cheap and
// non-blocking: the hook runs on the request path and must not stall
// routing.
//
// The func is per (provider, model), not per (provider, model,
// effort): no per-effort signal source exists yet — latency,
// success and quality are measured per provider, cost is priced
// per pair — so two attempts of the same pair at different efforts
// share the pair's signals and tie on score (stable order keeps
// the config order). Effort-differentiated ranking needs the map
// form (Rank / RankWithWeights, keyed by the full candidate
// including effort) with a caller that has per-effort signals;
// see BestEffortMove for the effort-preference primitive. The
// Candidate the hook scores still carries the attempt's effort,
// so the identity is never lost — only the signal lookup is
// pair-level today.
type SignalsFunc func(providerID, model string) Signals

// Hook returns a router.Config.OrderHook that reorders a fallback chain
// by Rank under the given mode, looking each attempt's signals up via
// the signals function.
//
//   - If signals is nil, the returned hook is the identity: the chain
//     passes through untouched and quota order stands. This is the safe
//     default when no signal source is wired up yet.
//   - Otherwise each attempt is looked up once (signals funcs must be
//     cheap and non-blocking — the hook runs on the request path and
//     must not stall routing), the chain is ranked, and the ranked
//     order is returned. The hook never drops or duplicates an
//     attempt: the output is a permutation of the input.
//   - Attempts whose provider/model has no signals score by quota
//     hint only (see Scorer.Score) and rank last unless a hint lifts
//     them.
//
// Importing internal/router here is safe: router does not import
// scoring, so there is no import cycle. Wiring this hook into
// router.Config is the coordinator's job, not this package's.
func Hook(signals SignalsFunc, mode Mode) func(chain []router.Attempt) []router.Attempt {
	return HookWithWeights(signals, Modes[mode])
}

// HookWithWeights is Hook for an explicit weight set (the
// "weighted" strategy): the chain is ranked by the weighted score
// under w. The same nil-signals identity rule applies: with no
// signal source the chain passes through untouched.
func HookWithWeights(signals SignalsFunc, w Weights) func(chain []router.Attempt) []router.Attempt {
	if signals == nil {
		return func(chain []router.Attempt) []router.Attempt { return chain }
	}
	scorer := NewScorerWithWeights(w)
	return func(chain []router.Attempt) []router.Attempt {
		type scored struct {
			att   router.Attempt
			score float64
		}
		items := make([]scored, 0, len(chain))
		for _, att := range chain {
			s := signals(att.ProviderID, att.Model)
			items = append(items, scored{att: att, score: scorer.Score(Candidate{ProviderID: att.ProviderID, Model: att.Model, Effort: att.Effort}, s)})
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].score > items[j].score })
		out := make([]router.Attempt, 0, len(items))
		for _, it := range items {
			out = append(out, it.att)
		}
		return out
	}
}
