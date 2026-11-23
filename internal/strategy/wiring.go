// M3 Phase B wiring (coordinator-owned): maps the new strategy names to
// the ordering implementations built in Phase A (orderings.go, quota.go,
// affinity.go, scoring16.go) and exposes them through HookFactory.
//
// Data honesty — what the hook can and cannot see (see internal parity tracker
// Area 1 for the per-row verdicts):
//   - Live per-(provider, model) signals: latency p50, success rate,
//     price, measured quality, scalar QuotaHint (higher = more
//     headroom, 0 = none), attempt counts (engine provider stats),
//     load (in-flight / live limit), and the pair's quota reset time
//     (ledger PairQuota roll-off). Quota utilisation (5h/7d) stays
//     unknown (-1): no per-pair limit exists anywhere to divide the
//     spend by, so headroom ranks everything as full headroom
//     (the reference router's own fail-open) and stays PARTIAL; quota expiry
//     times do not exist anywhere, so expiry-first falls back to the
//     (real) reset time and stays PARTIAL.
//   - QuotaRemaining is the measured remaining fraction when the
//     signal carries one (G19: probe/registry hint as a fraction),
//     else the booleanised QuotaHint — expiry-first scores a
//     continuous usable fraction from it; reset-aware still
//     scores from Util5h/Util7d (unknown -1: no per-pair limit),
//     so its remaining component stays the documented fail-open.
//     Quota expiry times now arrive from the engine's learned
//     hint (signalsFn overlays LearnedQuotaHint's reset instant
//     as ExpiresAt) whenever the engine has learned one from
//     real upstream rate-limit headers; with nothing learned
//     yet, expiry-first still falls back to the (real) reset
//     time.
//   - LKGP ordering is wired to the process-wide DefaultLKGPRegistry,
//     keyed by the requested model (the chain's first attempt model —
//     chains are single-model by construction, and the serving pair
//     is all telemetry can know). The server records every successful
//     inference into it (recordTelemetry) and persists it across
//     restarts (lkgp.json in the data dir, Load at startup / Save at
//     shutdown).
//   - Context-relay has no engine integration yet: its hook is the
//     identity until the engine feeds attempt outcomes (Phase D).
//   - Mode packs run the real 16-factor scorer on the live signals.
//     Factors with no hook input (taskFit, specificity, session,
//     tier) sit at their documented neutral values. LatencyP95Ms
//     is fed the p50 signal: the factor is pool-relative, so the
//     ranking reflects measured latency; true p95 arrives with
//     the latency-histogram plumbing (Phase D). ResetInSeconds is
//     now real (ledger roll-off), so the reset factor is no longer
//     neutral. Quota is the measured remaining fraction when the
//     signal carries one (G19), else the booleanised hint.
//     Health reads positively-known CLOSED as well as OPEN (G19).
//     ContextAffinity reads the request's prompt tokens against
//     the model's window, and cacheAffinity reads the measured
//     warmth func (G19); connection density reads the live
//     key-pool size (G19).
//   - "weighted" is the per-target weight draw (the reference router
//     selectWeightedTarget). Per-target weights come from the
//     process-wide table installed by SetChainWeights
//     (routing.chain_weights); no entry means weight 1 (uniform).
//     The old custom-weights behaviour is "custom-weights"
//     (pre-1.0 rename, see LOG.md).
//   - Stateful orderings (round-robin, strict-random deck) live in
//     the process-wide DefaultChainStates, keyed by chain signature,
//     so each chain rotates and deals from its own state.
package strategy

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
	"github.com/raunakiitp/codvian-router/internal/sla"
)

// modePackStrategies maps mode-pack strategy names to their
// 16-factor weight-pack name (scoring.ModePack).
var modePackStrategies = map[Strategy]string{
	StrategyShipFast:         "ship-fast",
	StrategyCostSaver:        "cost-saver",
	StrategyQualityFirst:     "quality-first",
	StrategyOfflineFriendly:  "offline-friendly",
	StrategyReliabilityFirst: "reliability-first",
	StrategyChaosMode:        "chaos-mode",
}

// hookState holds the shared draw state for one HookFactory instance
// (one per process). rand.Rand is not goroutine-safe; all draws go
// through mu. Round-robin/deck state is NOT here: it lives in the
// process-wide DefaultChainStates, keyed per chain (Phase C).
type hookState struct {
	rnd *rand.Rand
	mu  sync.Mutex
}

// newHookState builds fresh draw state.
func newHookState() *hookState {
	return &hookState{
		rnd: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// draw runs an ordering func with the shared rand under the mutex.
func (s *hookState) draw(order func(targets []OrderingTarget, rnd *rand.Rand) []OrderingTarget, targets []OrderingTarget) []OrderingTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	return order(targets, s.rnd)
}

// attemptKey identifies one chain element for remapping an ordering
// back onto []router.Attempt. key is the attempt KeyID (only used
// for LKGP, where the connection matters); empty matches any KeyID.
type attemptKey struct {
	provider string
	model    string
	key      string
}

// reorderAttempts returns a permutation of chain in the order given
// by keys (first unused chain element matching each key). Any chain
// element not covered by keys is appended in declared order, so the
// result is always a permutation — the hook never drops or
// duplicates an attempt.
func reorderAttempts(chain []router.Attempt, keys []attemptKey) []router.Attempt {
	used := make([]bool, len(chain))
	out := make([]router.Attempt, 0, len(chain))
	for _, k := range keys {
		for i, att := range chain {
			if used[i] || att.ProviderID != k.provider || att.Model != k.model {
				continue
			}
			if k.key != "" && att.KeyID != k.key {
				continue
			}
			used[i] = true
			out = append(out, att)
			break
		}
	}
	for i, att := range chain {
		if !used[i] {
			out = append(out, att)
		}
	}
	return out
}

// orderingAttemptKeys converts ordered OrderingTargets back to attempt keys.
func orderingAttemptKeys(ordered []OrderingTarget) []attemptKey {
	keys := make([]attemptKey, 0, len(ordered))
	for _, t := range ordered {
		keys = append(keys, attemptKey{provider: t.Provider, model: t.Model})
	}
	return keys
}

// balancedScorer ranks one candidate under the 5-factor balanced
// weights; used as the composite Score for p2c / quota-weighted.
func balancedScorer() scoring.Scorer {
	return scoring.NewScorerWithWeights(scoring.Modes[scoring.ModeBalanced])
}

// toOrderingTargets builds the S1 input from a chain and live
// signals. Score is the 5-factor balanced score. LatencyMs is the
// measured latency the p2c draw's latency term reads (G12); it is
// the same live signal the Score's latency component already uses,
// carried separately so OrderP2C can apply the G10 term on top of
// the Score. QuotaRemaining is
// a faithful booleanisation of the scalar QuotaHint (hint > 0 means
// quota remains, 0 means none) — fill-first only needs the
// exhausted/not-exhausted distinction. Attempts is the measured
// attempt count from the engine's live provider stats (Phase C);
// unknown (-1) sorts as 0 — least-used treats an unmeasured target
// as never used (exploration fail-open), matching the reference router's
// sortTargetsByUsage for unknown usage.
func toOrderingTargets(chain []router.Attempt, signals scoring.SignalsFunc, scorer scoring.Scorer) []OrderingTarget {
	out := make([]OrderingTarget, 0, len(chain))
	for _, att := range chain {
		var score float64
		var latencyMs float64
		var quotaRemaining float64
		var attempts int
		if signals != nil {
			s := signals(att.ProviderID, att.Model)
			score = scorer.Score(scoring.Candidate{ProviderID: att.ProviderID, Model: att.Model}, s)
			latencyMs = s.LatencyMs
			if s.QuotaHint > 0 {
				quotaRemaining = 1
			}
			if s.Attempts > 0 {
				attempts = s.Attempts
			}
		}
		out = append(out, OrderingTarget{
			Provider:       att.ProviderID,
			Model:          att.Model,
			Score:          score,
			LatencyMs:      latencyMs,
			Attempts:       attempts,
			QuotaRemaining: quotaRemaining,
			Weight:         1,
		})
	}
	return out
}

// toQuotaSnapshots builds the S2 input from a chain and live
// signals. Score and the QuotaRemaining booleanisation come from
// real data, and so do the Phase C quota inputs: utilisation
// (unknown -1 when no per-pair limit exists — the ordering's
// documented fail-open), the reset time (ledger roll-off), and the
// live load (in-flight / limit). Expiry is the engine's learned
// window-reset instant (signalsFn.ExpiresAt) when the engine has
// learned one from real upstream rate-limit headers; otherwise
// it stays zero and expiry-first falls back to the reset time
// as its deadline.
//
// QuotaRemaining is the measured remaining fraction when the
// signal carries one (G19 Area 1: QuotaRemainingPct > 0, the
// probe/registry hint as a fraction — so expiry-first scores a
// continuous usable fraction instead of the booleanised
// exhausted/not distinction); otherwise it is the booleanised
// scalar QuotaHint, exactly as before. Reset-aware itself
// scores from Util5h/Util7d (still unknown -1: no per-pair
// limit), not from QuotaRemaining — the fraction does not
// change its score, only expiry-first's usable and the
// no-data floor check.
func toQuotaSnapshots(chain []router.Attempt, signals scoring.SignalsFunc, scorer scoring.Scorer) []QuotaSnapshot {
	out := make([]QuotaSnapshot, 0, len(chain))
	for _, att := range chain {
		var score float64
		var quotaRemaining float64
		var snap QuotaSnapshot
		if signals != nil {
			s := signals(att.ProviderID, att.Model)
			score = scorer.Score(scoring.Candidate{ProviderID: att.ProviderID, Model: att.Model}, s)
			if s.QuotaRemainingPct > 0 {
				quotaRemaining = s.QuotaRemainingPct / 100
			} else if s.QuotaHint > 0 {
				quotaRemaining = 1
			}
			snap = QuotaSnapshot{
				Util5h:    s.Util5h,
				Util7d:    s.Util7d,
				ResetAt:   s.ResetAt,
				ExpiresAt: s.ExpiresAt,
				Load:      s.Load,
			}
		}
		snap.Provider = att.ProviderID
		snap.Model = att.Model
		snap.Score = score
		snap.QuotaRemaining = quotaRemaining
		out = append(out, snap)
	}
	return out
}

// snapshotKeys converts ordered QuotaSnapshots back to attempt keys.
func snapshotKeys(ordered []QuotaSnapshot) []attemptKey {
	keys := make([]attemptKey, 0, len(ordered))
	for _, s := range ordered {
		keys = append(keys, attemptKey{provider: s.Provider, model: s.Model})
	}
	return keys
}

// toAffinityTargets builds the S4 input from a chain; ConnectionID
// is the attempt KeyID (the account the chain element runs on).
func toAffinityTargets(chain []router.Attempt) []AffinityTarget {
	out := make([]AffinityTarget, 0, len(chain))
	for _, att := range chain {
		out = append(out, AffinityTarget{Provider: att.ProviderID, Model: att.Model, ConnectionID: att.KeyID})
	}
	return out
}

// affinityKeys converts ordered AffinityTargets back to attempt keys,
// matching on provider + model + connection (KeyID).
func affinityKeys(ordered []AffinityTarget) []attemptKey {
	keys := make([]attemptKey, 0, len(ordered))
	for _, t := range ordered {
		keys = append(keys, attemptKey{provider: t.Provider, model: t.Model, key: t.ConnectionID})
	}
	return keys
}

// chainSignature is the chain-state key for one chain: the declared
// provider/model list, so different chains do not share round-robin
// rotation or deck state.
func chainSignature(chain []router.Attempt) string {
	parts := make([]string, 0, len(chain))
	for _, att := range chain {
		parts = append(parts, att.ProviderID+"/"+att.Model)
	}
	return strings.Join(parts, ",")
}

// chainModel is the requested model for one chain: the first
// attempt's model. Chains are single-model by construction, so every
// attempt names the same model; it is the LKGP registry key and the
// chain-weights key. Empty for an empty chain.
func chainModel(chain []router.Attempt) string {
	if len(chain) == 0 {
		return ""
	}
	return chain[0].Model
}

// SixteenInputsFromSignals builds the signal-derived 16-factor
// inputs for one candidate from live scoring signals (G19 Area 1).
// Every factor with a live source is filled from real data; every
// factor without one sits at its documented neutral value (see
// scoring16.go). Pool maxima are left unset (<= 0): the caller
// fills them from the pool it ranks against (sixteenHookCtx does).
//
//   - Quota: the measured remaining fraction when the signal
//     carries one (QuotaRemainingPct > 0 — a real probe/registry
//     measurement); otherwise the booleanised scalar QuotaHint
//     (<= 0 means exhausted, factor 0; > 0 means headroom
//     remains but the fraction is unknown, factor neutral).
//     A QuotaRemainingPct of exactly 0 with QuotaHint > 0 is
//     contradictory input and reads as unknown, never as
//     exhausted: the scalar hint is the older, authoritative
//     source and it says headroom remains.
//   - Latency: the measured p50 EWMA, fed as the latency value
//     (pool-relative, so the ranking reflects measured latency;
//     true p95 arrives with the latency-histogram plumbing).
//   - Cost / success / quality: straight from the signals.
//   - Health: OPEN when positively open, CLOSED when positively
//     closed (BreakerClosedForProvider), unknown otherwise —
//     never claimed CLOSED on no evidence.
//   - Reset: the real ledger roll-off time when known.
//   - Connection density: the live key-pool size when known.
//   - Context window: the registry/model window when known.
//   - Still neutral (no live source anywhere): taskFit tags,
//     tierPriority (no account-tier source), tierAffinity /
//     specificityMatch (the model's own tier IS now classified
//     from its real price — see below — but the routing hint
//     that names a recommended tier / specificity score does
//     not exist anywhere, so the factors stay neutral without
//     one), stability (no latency-stddev source), sessionAffinity (no
//     session store — see the file header), cacheAffinity (needs
//     the per-request warmth func, layered by sixteenInputs).
func SixteenInputsFromSignals(s scoring.Signals) scoring.SixteenInputs {
	in := scoring.SixteenInputs{
		QuotaRemainingPct:    -1, // unknown fraction -> neutral 0.5
		LatencyP95Ms:         -1, // p50 signal fed below; -1 when none
		MaxLatencyMs:         -1,
		CostPer1M:            -1, // unknown price -> neutral 0.5
		MaxCostPer1M:         -1,
		SuccessRate:          -1, // no observation -> neutral 0.5
		QualityScore:         0,  // unknown -> package neutral 0.45
		LatencyStdDevMs:      -1,
		MaxStdDevMs:          -1,
		Breaker:              scoring.BreakerUnknown,
		Tier:                 scoring.AccountTierUnknown,
		ModelTier:            scoring.ModelTierUnknown,
		SpecificityScore:     -1,
		ContextTokens:        -1,
		ModelContextTokens:   0,
		CachedPromptFraction: -1,
		ResetInSeconds:       -1,
		ConnectionCount:      0, // unknown pool size -> neutral 0.5
	}
	if s.QuotaRemainingPct > 0 {
		// Real measured fraction (0..100).
		in.QuotaRemainingPct = s.QuotaRemainingPct
	} else if s.QuotaHint <= 0 {
		in.QuotaRemainingPct = 0 // no headroom: exhausted, factor 0
	}
	if s.LatencyMs > 0 {
		// Pool-relative latency factor: the p50 signal is fed as
		// the latency value (see file header); true p95 is Phase C.
		in.LatencyP95Ms = s.LatencyMs
	}
	if s.CostUSDPer1M >= 0 {
		in.CostPer1M = s.CostUSDPer1M
		// Economic tier from the real price (G19 Area 1):
		// ClassifyPriceTier on the blended price (no input leg
		// reaches this scorer) — free at $0, cheap at <= $1/M,
		// premium above — so tierAffinity/specificityMatch vote
		// on measured prices instead of staying neutral. The
		// factors still need a routing hint (recommended tier /
		// specificity score) to vote: without one they stay
		// neutral, as documented in scoring16.go.
		in.ModelTier = scoring.ClassifyPriceTier(0, false, s.CostUSDPer1M)
	}
	if s.Attempts > 0 {
		// Measured rate, even 0: every attempt failed is a
		// real 0, not unknown. (G19: the old `SuccessRate > 0`
		// test left a measured-0 success rate at neutral 0.5,
		// ranking a provider that fails everything as if it
		// had never been tried.)
		in.SuccessRate = s.SuccessRate
	}
	// Breaker state (M3 Area 1, row 394; G19: CLOSED is now
	// positively known too): a positively OPEN breaker reads
	// as OPEN for the health factor; a positively CLOSED one
	// reads as CLOSED; false on both means "not open OR
	// unknown" and stays Unknown — never claimed CLOSED,
	// which would score perfect health on no evidence. A
	// contradictory OPEN + CLOSED input reads OPEN
	// (fail-closed).
	if s.BreakerOpen {
		in.Breaker = scoring.BreakerOpen
	} else if s.BreakerClosed {
		in.Breaker = scoring.BreakerClosed
	}
	if !s.ResetAt.IsZero() {
		// Real reset time (ledger roll-off, Phase C): the
		// reset factor is no longer neutral. A past reset
		// yields a negative value, which the scorer treats
		// as neutral (see scoring16.go resetFactor).
		in.ResetInSeconds = time.Until(s.ResetAt).Seconds()
	}
	if s.ConnectionCount > 0 {
		in.ConnectionCount = s.ConnectionCount
	}
	if s.ContextWindow > 0 {
		in.ModelContextTokens = float64(s.ContextWindow)
	}
	in.QualityScore = s.QualityScore
	return in
}

// sixteenInputsCtx is the 16-factor input builder: the
// signal-derived factors (SixteenInputsFromSignals) plus the
// per-request context and warmth source (G19 Area 1): the
// request's prompt-token count feeds the contextAffinity factor
// against the model's window (1 - promptTokens/window, neutral
// when either is unknown), and the measured per-pair cached-token
// fraction feeds cacheAffinity (neutral when the warmth func is
// nil or the pair is unmeasured).
func sixteenInputsCtx(att router.Attempt, signals scoring.SignalsFunc, maxLatency, maxCost float64, ctx router.StrategyHookContext, warmth CacheWarmthFunc) scoring.SixteenInputs {
	if signals == nil {
		return scoring.SixteenInputs{
			QuotaRemainingPct: -1, LatencyP95Ms: -1, MaxLatencyMs: maxLatency,
			CostPer1M: -1, MaxCostPer1M: maxCost, SuccessRate: -1,
			QualityScore: 0, LatencyStdDevMs: -1, MaxStdDevMs: -1,
			Breaker: scoring.BreakerUnknown, Tier: scoring.AccountTierUnknown,
			ModelTier: scoring.ModelTierUnknown, SpecificityScore: -1,
			ContextTokens: -1, ModelContextTokens: 0,
			CachedPromptFraction: -1, ResetInSeconds: -1, ConnectionCount: 0,
		}
	}
	s := signals(att.ProviderID, att.Model)
	in := SixteenInputsFromSignals(s)
	in.MaxLatencyMs = maxLatency
	in.MaxCostPer1M = maxCost
	if ctx.PromptTokens > 0 {
		in.ContextTokens = float64(ctx.PromptTokens)
	}
	if warmth != nil {
		if w, known := warmth(att.ProviderID, att.Model); known {
			in.CachedPromptFraction = w
		}
	}
	return in
}

// sixteenHookCtx ranks a chain with the 16-factor scorer under the
// named mode pack. The per-request context and warmth source flow
// into the inputs (prompt tokens for contextAffinity, measured
// cache warmth for cacheAffinity); a context-free call passes the
// zero context and a nil warmth func, so those two factors stay
// neutral exactly as before.
func sixteenHookCtx(signals scoring.SignalsFunc, packName string, ctx router.StrategyHookContext, warmth CacheWarmthFunc) func(chain []router.Attempt) []router.Attempt {
	return func(chain []router.Attempt) []router.Attempt {
		if len(chain) == 0 || signals == nil {
			return chain
		}
		var maxLatency, maxCost float64 = -1, -1
		for _, att := range chain {
			s := signals(att.ProviderID, att.Model)
			if s.LatencyMs > 0 && s.LatencyMs > maxLatency {
				maxLatency = s.LatencyMs
			}
			if s.CostUSDPer1M >= 0 && s.CostUSDPer1M > maxCost {
				maxCost = s.CostUSDPer1M
			}
		}
		candidates := make([]scoring.Candidate, 0, len(chain))
		inputs := make(map[scoring.Candidate]scoring.SixteenInputs, len(chain))
		for _, att := range chain {
			c := scoring.Candidate{ProviderID: att.ProviderID, Model: att.Model}
			candidates = append(candidates, c)
			inputs[c] = sixteenInputsCtx(att, signals, maxLatency, maxCost, ctx, warmth)
		}
		ranked := scoring.Rank16(candidates, inputs, scoring.ModePack(packName))
		keys := make([]attemptKey, 0, len(ranked))
		for _, c := range ranked {
			keys = append(keys, attemptKey{provider: c.ProviderID, model: c.Model})
		}
		return reorderAttempts(chain, keys)
	}
}

// costOptimizedHook is pure cheapest-first: lowest price wins,
// unknown price (negative) sorts last, ties keep declared order.
func costOptimizedHook(signals scoring.SignalsFunc) func(chain []router.Attempt) []router.Attempt {
	return func(chain []router.Attempt) []router.Attempt {
		if len(chain) == 0 || signals == nil {
			return chain
		}
		idx := make([]int, len(chain))
		for i := range idx {
			idx[i] = i
		}
		price := func(att router.Attempt) float64 {
			p := signals(att.ProviderID, att.Model).CostUSDPer1M
			if p < 0 {
				return math.Inf(1) // unknown price: last, never cheapest
			}
			return p
		}
		sort.SliceStable(idx, func(a, b int) bool {
			return price(chain[idx[a]]) < price(chain[idx[b]])
		})
		out := make([]router.Attempt, 0, len(chain))
		for _, i := range idx {
			out = append(out, chain[i])
		}
		return out
	}
}

// estimateCostUSD estimates the cost in USD for one request to a
// provider/model pair given the prompt token count. It uses the
// signals function to get the blended price (CostUSDPer1M) and
// returns the estimated cost for the given token count. If the
// price is unknown (negative), it returns math.Inf(1) so the
// caller can treat it as "cannot estimate" and sort it last.
func estimateCostUSD(providerID, model string, promptTokens int, signals scoring.SignalsFunc) float64 {
	if signals == nil || promptTokens <= 0 {
		return math.Inf(1)
	}
	p := signals(providerID, model).CostUSDPer1M
	if p < 0 {
		return math.Inf(1)
	}
	// Cost per 1M tokens, so cost = price * (promptTokens / 1e6).
	return p * float64(promptTokens) / 1_000_000.0
}

// wiringHook returns the OrderHook for one of the M3 strategies, or
// nil when st is not an M3 wired strategy. Stateful orderings use
// the shared state s. ctx is the per-request hook context (zero
// when the caller has none — the cacheability gate then stays
// closed); warmth, when non-nil, reports the measured
// per-(provider, model) cached-token fraction for the current
// prompt prefix, and is consulted only by the cache-optimized
// case.
func wiringHook(st Strategy, signals scoring.SignalsFunc, custom scoring.Weights, s *hookState, ctx router.StrategyHookContext, warmth CacheWarmthFunc) func(chain []router.Attempt) []router.Attempt {
	scorer := balancedScorer()
	switch st {
	case StrategyBalanced:
		// S1-SCORER16 follow-up: the default ("balanced") chain
		// now scores 16-factor under the reference router's DEFAULT_WEIGHTS
		// (scoring.Hook16Default), replacing the 5-factor
		// HookWithWeights fallback in strategy.go — the 11
		// factors with no 5-factor vote now vote on the default
		// path. Hook16Default is nil-safe (identity when
		// signals is nil), matching HookWithWeights' nil rule.
		// Custom weights do not apply to this hook: the default
		// strategy is defined by DEFAULT_WEIGHTS (B2-10/B2-11/
		// B2-12); a custom-weighted 16-factor hook is a later
		// follow-up if operators need one.
		return scoring.Hook16Default(signals)
	case StrategyRoundRobin:
		// Per-chain rotation (Phase C): each chain advances
		// its own RoundRobin in DefaultChainStates, so chain
		// A's rotation never moves chain B's next leader.
		return func(chain []router.Attempt) []router.Attempt {
			st := DefaultChainStates.GetOrCreate(chainSignature(chain))
			return reorderAttempts(chain, orderingAttemptKeys(st.RR.Order(toOrderingTargets(chain, signals, scorer))))
		}
	case StrategyRandom:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, orderingAttemptKeys(s.draw(ShuffleTargets, toOrderingTargets(chain, signals, scorer))))
		}
	case StrategyStrictRandom:
		// Per-chain deck (Phase C): each chain deals from its
		// own StrictRandomDeck in DefaultChainStates. The deck
		// draw is serialised through the shared rand (mu), as
		// the deck does not own the rng.
		return func(chain []router.Attempt) []router.Attempt {
			targets := toOrderingTargets(chain, signals, scorer)
			st := DefaultChainStates.GetOrCreate(chainSignature(chain))
			s.mu.Lock()
			ordered := st.Deck.Order(targets, s.rnd)
			s.mu.Unlock()
			return reorderAttempts(chain, orderingAttemptKeys(ordered))
		}
	case StrategyFillFirst:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, orderingAttemptKeys(OrderFillFirst(toOrderingTargets(chain, signals, scorer))))
		}
	case StrategyLeastUsed:
		// Attempts are live (Phase C): fewest measured attempts
		// first; unmeasured targets sort as never used.
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, orderingAttemptKeys(OrderLeastUsed(toOrderingTargets(chain, signals, scorer))))
		}
	case StrategyP2C:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, orderingAttemptKeys(s.draw(OrderP2C, toOrderingTargets(chain, signals, scorer))))
		}
	case StrategyLatencyAware:
		// Latency-aware (G10 Area 12 row 9): lowest latency among
		// VIABLE targets, scored under ModeLatencyAware (latency
		// 0.70). Viable means no measured failure history: a
		// candidate is non-viable only when it has measured
		// attempts (Attempts > 0) with a success rate below 0.5
		// — it mostly fails, so it is demoted behind every viable
		// candidate however fast it is. Unknown (Attempts <= 0)
		// fails OPEN: an unmeasured target is viable, matching
		// the scorer's treatment of unknown signals. Non-viable
		// targets keep their latency-aware score order among
		// themselves, so the hook never drops or reorders them
		// arbitrarily — it only demotes them as a group. The
		// latency signal is the hook's measured p50 (health-probe
		// latency history feeds the same signal upstream); cost
		// still counts at weight 0.05, so a large cost gap beats
		// a noise-level latency gap (see TestLatencyAware*).
		return func(chain []router.Attempt) []router.Attempt {
			if len(chain) == 0 || signals == nil {
				return chain
			}
			scorer := scoring.NewScorer(scoring.ModeLatencyAware)
			type scored struct {
				att    router.Attempt
				score  float64
				viable bool
			}
			items := make([]scored, 0, len(chain))
			for _, att := range chain {
				s := signals(att.ProviderID, att.Model)
				viable := s.Attempts <= 0 || s.SuccessRate >= 0.5
				items = append(items, scored{
					att:    att,
					score:  scorer.Score(scoring.Candidate{ProviderID: att.ProviderID, Model: att.Model}, s),
					viable: viable,
				})
			}
			sort.SliceStable(items, func(i, j int) bool {
				if items[i].viable != items[j].viable {
					return items[i].viable
				}
				return items[i].score > items[j].score
			})
			keys := make([]attemptKey, 0, len(items))
			for _, it := range items {
				keys = append(keys, attemptKey{provider: it.att.ProviderID, model: it.att.Model})
			}
			return reorderAttempts(chain, keys)
		}
	case StrategyWeighted:
		// Per-target weight draw over the operator-configured
		// chain weights (Phase C): the chain key is the
		// requested model, weights come from the process-wide
		// table installed by SetChainWeights, and an unconfigured
		// chain draws uniformly (every target weight 1).
		return func(chain []router.Attempt) []router.Attempt {
			targets := Resolve(chainModel(chain), toOrderingTargets(chain, signals, scorer), chainWeightsFor())
			selected, ok := func() (OrderingTarget, bool) {
				s.mu.Lock()
				defer s.mu.Unlock()
				return WeightedDraw(targets, s.rnd)
			}()
			if !ok {
				return chain
			}
			return reorderAttempts(chain, orderingAttemptKeys(OrderWeightedFallback(targets, selected, true)))
		}
	case StrategyQuotaWeighted:
		return func(chain []router.Attempt) []router.Attempt {
			targets := toQuotaSnapshots(chain, signals, scorer)
			s.mu.Lock()
			ordered := OrderQuotaWeighted(targets, s.rnd)
			s.mu.Unlock()
			return reorderAttempts(chain, snapshotKeys(ordered))
		}
	case StrategyHeadroom:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, snapshotKeys(OrderHeadroom(toQuotaSnapshots(chain, signals, scorer))))
		}
	case StrategyExpiryFirst:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, snapshotKeys(OrderExpiryFirst(toQuotaSnapshots(chain, signals, scorer), time.Now())))
		}
	case StrategyResetAware:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, snapshotKeys(OrderResetAware(toQuotaSnapshots(chain, signals, scorer), time.Now())))
		}
	case StrategyResetWindow:
		return func(chain []router.Attempt) []router.Attempt {
			return reorderAttempts(chain, snapshotKeys(OrderResetWindow(toQuotaSnapshots(chain, signals, scorer), time.Now())))
		}
	case StrategyLKGP:
		// Last-known-good first (Phase C): the registry is the
		// process-wide DefaultLKGPRegistry, keyed by the
		// requested model (the chain's first attempt model).
		// Chains are single-model by construction, and the
		// serving pair is all telemetry can know, so the model
		// is the key — not the full chain signature. Two
		// overrides keep the pin honest (G19 Area 1): the pin
		// expires after LKGPRecordTTL (Lookup already reports
		// a stale pin missing), and a pin whose provider is
		// positively breaker-OPEN — or breaker-DEGRADED
		// (RT-2 follow-up: slowing/failing, not yet OPEN) —
		// is never sticky — the declared order stands
		// instead of hoisting a dead or degrading provider
		// first. OPEN and DEGRADED are mutually exclusive
		// states, so the two checks never double-count; a
		// nil signals func cannot check the breaker, so the
		// pin stands (the strategy's own contract); the TTL
		// still applies, because Lookup enforces it without
		// any signal.
		return func(chain []router.Attempt) []router.Attempt {
			if signals != nil {
				if rec, ok := DefaultLKGPRegistry.LookupFor(chainModel(chain)); ok {
					if sig := signals(rec.Provider, rec.Model); sig.BreakerOpen || sig.BreakerDegraded {
						return chain
					}
				}
			}
			targets := toAffinityTargets(chain)
			ordered := DefaultLKGPRegistry.OrderByLKGPFor(chainModel(chain), targets)
			return reorderAttempts(chain, affinityKeys(ordered))
		}
	case StrategyContextOptimized:
		// Context windows are a live hook input (G19 Area 1):
		// the registry/model window from the signals
		// (operator-set or models.dev synced; 0 = unknown).
		// Unknown windows sort after every known window, and
		// when NO target has a known window the declared order
		// stands (OrderByContextOptimized's own rule).
		return func(chain []router.Attempt) []router.Attempt {
			targets := toAffinityTargets(chain)
			ctx := make([]ContextTarget, 0, len(targets))
			for _, t := range targets {
				window := 0
				if signals != nil {
					window = signals(t.Provider, t.Model).ContextWindow
				}
				ctx = append(ctx, ContextTarget{AffinityTarget: t, ContextWindow: window})
			}
			ordered := OrderByContextOptimized(ctx)
			keys := make([]attemptKey, 0, len(ordered))
			for _, t := range ordered {
				keys = append(keys, attemptKey{provider: t.Provider, model: t.Model})
			}
			return reorderAttempts(chain, keys)
		}
	case StrategyCacheOptimized:
		// M6.5-wiring: the pure OrderByCacheAffinity ordering,
		// fed with live inputs — measured per-pair warmth from
		// the ledger accessor (via the factory's warmth func)
		// and the request's cacheability gate from the
		// per-request hook context. The gate (prompt >= 1,024
		// tokens, no tools, deterministic temperature, no
		// pinned contract) runs first: a non-cacheable request
		// keeps declared order. Warmth is never invented: a
		// target with no ledger data has WarmthKnown false and
		// ranks as unmeasured (0). When NO target in the chain
		// has any measured warmth above 0, the hook returns the
		// chain unchanged — affinity has nothing to say, and
		// re-ranking on marker support / the balanced score
		// alone would be a re-rank on no measured cache signal
		// at all (the old identity behaviour is preserved).
		// ContractPinned is dead in the wired path — a pinned
		// request short-circuits in orderAttempts before any
		// hook runs — but the gate still checks it, so a hook
		// called directly with a pinned context cannot re-rank.
		// Scores come from the balanced scorer (the fallback
		// tiebreak); ConnectionIDs come from toAffinityTargets
		// so the remap uses affinityKeys (provider + model +
		// KeyID), never orderingAttemptKeys.
		return func(chain []router.Attempt) []router.Attempt {
			if len(chain) == 0 {
				return chain
			}
			req := CacheAffinityRequest{
				PromptTokens:   ctx.PromptTokens,
				HasTools:       ctx.HasTools,
				Temperature:    ctx.Temperature,
				TemperatureSet: ctx.TemperatureSet,
				ContractPinned: ctx.ContractPinned,
			}
			if !IsCacheableRequest(req) {
				return chain
			}
			scored := toOrderingTargets(chain, signals, scorer)
			affinity := toAffinityTargets(chain)
			targets := make([]CacheAffinityTarget, 0, len(chain))
			anyWarm := false
			for i := range scored {
				var w float64
				var known bool
				if warmth != nil {
					w, known = warmth(scored[i].Provider, scored[i].Model)
				}
				if known && w > 0 {
					anyWarm = true
				}
				targets = append(targets, CacheAffinityTarget{
					AffinityTarget: affinity[i],
					Warmth:         w,
					WarmthKnown:    known,
					Score:          scored[i].Score,
				})
			}
			if !anyWarm {
				return chain
			}
			ordered := OrderByCacheAffinity(targets, req)
			aff := make([]AffinityTarget, 0, len(ordered))
			for _, t := range ordered {
				aff = append(aff, t.AffinityTarget)
			}
			return reorderAttempts(chain, affinityKeys(aff))
		}
	case StrategyContextRelay:
		// No engine integration yet: identity until the engine
		// feeds attempt outcomes into RelayState (Phase C).
		return func(chain []router.Attempt) []router.Attempt { return chain }
	case StrategyCostOptimized:
		return costOptimizedHook(signals)
	case StrategySLAAware:
		// SLA-aware (M3 Area 1, PARITY row 394): rank by SLA
		// compliance over the live signals — p95 latency (the
		// hook's latency signal is the measured p50 EWMA, fed
		// as the latency value; true p95 arrives with the
		// latency-histogram plumbing, same caveat as
		// sixteenInputs), error rate (1 - success rate, only
		// when attempts were measured), cost, and observed
		// serving health. Unknown signals score neutral and
		// add no violation (see internal/sla). The policy comes
		// from the per-request hook context (engine Config SLA
		// fields); a zero context means the defaults, except
		// an explicitly configured error rate of 0
		// (SLAMaxErrorRateSet) which is zero-tolerance.
		// Breaker-OPEN pre-filter (the reference router's rule):
		// candidates positively known OPEN are removed
		// before scoring; only if every candidate is OPEN
		// is the full pool scored. A false BreakerOpen means
		// "not open OR unknown" and scores normally.
		return func(chain []router.Attempt) []router.Attempt {
			if len(chain) == 0 || signals == nil {
				return chain
			}
			policy := sla.Policy{
				TargetP95Ms:        ctx.SLATargetP95Ms,
				MaxErrorRate:       ctx.SLAMaxErrorRate,
				MaxCostPer1MTokens: ctx.SLAMaxCostPer1M,
				HardConstraints:    ctx.SLAHardConstraints,
			}
			// The context carries the resolved error rate
			// (the engine maps unset to the 0.05 default);
			// a negative rate still means "default" to the
			// scorer. A 0 rate is zero-tolerance ONLY when
			// the context says it was explicitly configured
			// (SLAMaxErrorRateSet); a context-free 0 still
			// means "default", as before.
			if !ctx.SLAMaxErrorRateSet && policy.MaxErrorRate == 0 {
				policy.MaxErrorRate = -1
			}
			type scored struct {
				att       router.Attempt
				violation float64
				score     float64
			}
			// One signals read per attempt, then the OPEN
			// pre-filter: the scored pool is the non-OPEN
			// candidates, or the full chain when all are
			// OPEN. reorderAttempts below appends any
			// unscored (OPEN) attempts after the scored
			// ones, in declared order — so an OPEN provider
			// sorts last but is never dropped.
			sigs := make([]scoring.Signals, len(chain))
			pool := make([]int, 0, len(chain))
			for i, att := range chain {
				sigs[i] = signals(att.ProviderID, att.Model)
				if !sigs[i].BreakerOpen {
					pool = append(pool, i)
				}
			}
			if len(pool) == 0 {
				for i := range chain {
					pool = append(pool, i)
				}
			}
			var maxCost float64 = -1
					cands := make([]sla.Candidate, len(pool))
					for j, i := range pool {
						s := sigs[i]
						c := sla.Candidate{}
						// Use true p95 from histogram when available (B9);
						// fall back to p50 EWMA. The SLA policy's target is p95.
						if s.LatencyP95Ms > 0 {
							c.LatencyMs = s.LatencyP95Ms
							c.HasLatency = true
						} else if s.LatencyMs > 0 {
							c.LatencyMs = s.LatencyMs
							c.HasLatency = true
						}
						if s.Attempts > 0 {
					c.ErrorRate = 1 - s.SuccessRate
					if c.ErrorRate < 0 {
						c.ErrorRate = 0
					}
					if c.ErrorRate > 1 {
						c.ErrorRate = 1
					}
					c.HasErrorRate = true
					c.Health = 0
					if s.SuccessRate > 0 {
						c.Health = 1
					}
					c.HasHealth = true
				}
				if s.CostUSDPer1M >= 0 {
					c.CostPer1MTokens = s.CostUSDPer1M
					c.HasCost = true
					if s.CostUSDPer1M > maxCost {
						maxCost = s.CostUSDPer1M
					}
				}
				cands[j] = c
			}
			items := make([]scored, len(pool))
			for j, i := range pool {
				v, sc := sla.Score(cands[j], policy, maxCost)
				items[j] = scored{att: chain[i], violation: v, score: sc}
			}
			sort.SliceStable(items, func(a, b int) bool {
				return sla.Better(items[a].violation, items[a].score, items[b].violation, items[b].score, policy.HardConstraints)
			})
			keys := make([]attemptKey, 0, len(items))
			for _, it := range items {
				keys = append(keys, attemptKey{provider: it.att.ProviderID, model: it.att.Model})
			}
			return reorderAttempts(chain, keys)
		}
	case StrategyNadir:
			// Nadir is a model-selection strategy, not an ordering:
			// the decision happens at auto/nadir dispatch time in
			// the router (internal/router/nadir.go), which can do
			// I/O. As a chain hook it is the identity — the
			// declared (priority) order stands, and no external
			// call is ever made from here. Returning nil would
			// fall through to the balanced weight hook and
			// re-rank, which would NOT be the priority fallback.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		case StrategyCascade:
			// Cascade is an engine-intercepted ordering, not a
			// weight set: the router applies applyCascadeOrder
			// over the live auto-catalog tiers before the hook
			// (internal/router/cascade.go). As a chain hook it
			// is the identity — the declared order stands. Returning
			// nil would fall through to the balanced weight hook
			// and re-rank, which would NOT be the cascade order.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		case StrategyAuto:
			// Auto is the auto-routing entry point: the auto-router
			// (internal/router/autodispatch.go) resolves a concrete
			// model and effective strategy from the live catalog before
			// the chain is built. As a chain hook it is the identity —
			// the real dispatch happened at model-resolution time.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		case StrategyFusion:
			// Fusion is an engine-intercepted composition (panel + judge):
			// the router constructs the auto/fusion panel and invokes the
			// judge model before the hook. As a chain hook it is the
			// identity — the engine intercepted it. Returning nil would
			// fall through to a balanced re-rank, which would NOT be the
			// fusion panel order.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		case StrategyPipeline:
			// Pipeline is an engine-intercepted sequential composition:
			// the router tries models in auto/pipeline order, feeding each
			// model's output to the next; the first success terminates.
			// As a chain hook it is the identity. Returning nil would
			// fall through to a balanced re-rank, which would NOT be the
			// pipeline order.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		case StrategyJEV:
				// JEV (Judge-Evaluate-Vote) is an engine-intercepted ensemble:
				// the router constructs the auto/jev panel, invokes the judge
				// model, and selects the best candidate. As a chain hook it is
				// the identity. Returning nil would fall through to a balanced
				// re-rank, which would NOT be the JEV selection.
				return func(chain []router.Attempt) []router.Attempt { return chain }
			case StrategyBudgetAware:
					// Budget-aware (B11): ranks targets by remaining budget headroom
					// for the gateway key. The hook needs access to the ledger to
					// read KeyBudgetStatus and the budget config, and to the cost
					// estimates for each target. Since the hook factory closes over
					// the engine's signals function (which has no budget data), we
					// need to inject the budget signals via the StrategyHookContext
					// (which carries the caller's apiKeyId and the engine's ledger
					// accessor). The context's Ledger field provides the ledger
					// accessor; we type-assert it to the concrete ledger to access
					// the budget methods.
					//
					// If no budget is configured for the key, the hook falls back
					// to the balanced order (no re-ranking on no data).
					return func(chain []router.Attempt) []router.Attempt {
						if len(chain) == 0 || ctx.ApiKeyId == "" {
							return chain
						}
						// Type-assert the ledger to access budget methods.
						// The engine's ledger is the concrete *ledger.Ledger which
						// implements GetKeyBudget and GetKeyBudgetStatus.
						l, ok := ctx.Ledger.(*ledger.Ledger)
									if !ok || l == nil {
										return chain
									}
									// Read budget status and config from the ledger.
									// KeyBudgetStatus requires a time.Time for the rolling
									// window calculation; use the current time.
									budgetStatus, err := l.KeyBudgetStatus(context.Background(), ctx.ApiKeyId, time.Now())
									if err != nil || budgetStatus.LimitUSD <= 0 {
										return chain // no budget configured
									}
									budgetConfig, err := l.GetKeyBudget(context.Background(), ctx.ApiKeyId)
									if err != nil || budgetConfig.LimitUSD <= 0 {
										return chain
									}
									remaining := budgetStatus.LimitUSD - budgetStatus.SpentUSD
									if remaining <= 0 {
										return chain // budget exhausted
									}
						// Score each target by remaining budget after estimated cost.
						// Targets that fit within remaining budget score higher.
						type scored struct {
							att       router.Attempt
							remaining float64 // remaining budget after this target's estimated cost
						}
						items := make([]scored, 0, len(chain))
						for _, att := range chain {
							cost := estimateCostUSD(att.ProviderID, att.Model, ctx.PromptTokens, signals)
							rem := remaining - cost
							items = append(items, scored{att: att, remaining: rem})
						}
						sort.SliceStable(items, func(i, j int) bool {
							// Targets that fit in budget (remaining >= 0) lead.
							fitI := items[i].remaining >= 0
							fitJ := items[j].remaining >= 0
							if fitI != fitJ {
								return fitI
							}
							// Among fitting targets, more remaining budget is better.
							// Among non-fitting, less overage is better.
							if fitI {
								return items[i].remaining > items[j].remaining
							}
							return items[i].remaining > items[j].remaining // less negative = less overage
						})
						keys := make([]attemptKey, 0, len(items))
						for _, it := range items {
							keys = append(keys, attemptKey{provider: it.att.ProviderID, model: it.att.Model})
						}
						return reorderAttempts(chain, keys)
					}
				}
				if pack, ok := modePackStrategies[st]; ok {
		return sixteenHookCtx(signals, pack, ctx, warmth)
	}
	return nil
}
