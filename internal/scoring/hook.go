package scoring

import (
	"github.com/raunakiitp/codvian-router/internal/router"
)

// Hook returns a router.Config.OrderHook that reorders a fallback chain
// by Rank under the given mode, looking each provider's signals up via
// the signals function.
//
//   - If signals is nil, the returned hook is the identity: the chain
//     passes through untouched and quota order stands. This is the safe
//     default when no signal source is wired up yet.
//   - Otherwise each attempt's provider is looked up once (signals
//     funcs must be cheap and non-blocking — the hook runs on the
//     request path and must not stall routing), the chain is ranked,
//     and the ranked order is returned. The hook never drops or
//     duplicates an attempt: the output is a permutation of the input.
//   - Attempts whose provider has no signals score by quota hint only
//     (see Scorer.Score) and rank last unless a hint lifts them.
//
// Importing internal/router here is safe: router does not import
// scoring, so there is no import cycle. Wiring this hook into
// router.Config is the coordinator's job, not this package's.
func Hook(signals func(providerID string) Signals, mode Mode) func(chain []router.Attempt) []router.Attempt {
	if signals == nil {
		return func(chain []router.Attempt) []router.Attempt { return chain }
	}
	return func(chain []router.Attempt) []router.Attempt {
		candidates := make([]Candidate, 0, len(chain))
		sigMap := make(map[string]Signals, len(chain))
		for _, att := range chain {
			candidates = append(candidates, Candidate{ProviderID: att.ProviderID, Model: att.Model})
			if _, ok := sigMap[att.ProviderID]; !ok {
				sigMap[att.ProviderID] = signals(att.ProviderID)
			}
		}
		ranked := Rank(candidates, sigMap, mode)
		out := make([]router.Attempt, 0, len(ranked))
		for _, c := range ranked {
			out = append(out, router.Attempt{ProviderID: c.ProviderID, Model: c.Model})
		}
		return out
	}
}
