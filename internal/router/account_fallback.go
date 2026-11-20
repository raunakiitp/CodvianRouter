// Account-level fallback strategies (M3 Area 1, PARITY row 388).
//
// the reference router's ACCOUNT_FALLBACK_STRATEGY_VALUES
// (src/shared/constants/routingStrategies.ts:56-65, studied, not
// copied) names the per-provider setting that orders the
// ACCOUNTS (connections / keys) of one provider when a request
// falls back between them: settings.fallbackStrategy, overridable
// per provider via providerStrategies[provider].fallbackStrategy
// (default "fill-first", src/sse/services/auth.ts:1930). This
// file is the local form of that concept: Config
// .AccountFallbackStrategy (global) +
// .AccountFallbackStrategies (per-provider override), applied
// inside orderAttempts — within each (provider, model) group of
// the chain, after the chain strategy hook, never under an
// explicit client order (preserveOrder).
//
// What each value does with the signals that actually exist:
//   - priority: declared order (pool order). Identity.
//   - fill-first: drains the first account before the next.
//     Ordering-wise that IS declared order; filterZeroQuota
//     filters exhausted PROVIDERS (provider-level quotaHint).
//     Individual accounts at their measured per-key cap now
//     sink to the tail via orderAccountCapCheck (B2-09), so
//     the drain skips exhausted keys instead of retrying them
//     until they fail (the reference router's own wording: "preserves
//     priority order; the drain is enforced by retrying the
//     first target until it fails" — the cap check is the
//     stricter local form).
//   - round-robin: per-(provider, model) rotation; each request
//     starts at the next account.
//   - p2c: power of two choices over the accounts — draw two
//     distinct accounts at random, the better-scoring one
//     leads, the rest keep declared order. The score is the
//     provider-level composite (success rate +
//     1/log10(latency+10), breaker OPEN scores -Inf, same
//     shape as internal/strategy's OrderP2C); per-key latency
//     / success signals do not exist, so accounts of one
//     provider share the provider score and the draw decides
//     between them.
//   - random: Fisher-Yates shuffle of the accounts.
//   - least-used: fewest measured attempts first, from the
//     engine's live per-(provider, key) attempt counter
//     (keyAttempts, real attempts only; unmeasured counts as 0).
//   - cost-optimized: accounts of one provider serving one
//     model cost the same, so every account ties and declared
//     order stands. Vacuous but correct — the price lives on
//     the (provider, model) pair, not the key.
//   - strict-random: shuffled deck per (provider, model) —
//     every account leads exactly once per cycle, then the deck
//     reshuffles with last-of-cycle != first-of-next.
//   - weighted: per-account weight draw. No per-account weight
//     source exists, so every account weighs 1 and the draw is
//     uniform (the reference router's own fall-back when the total weight
//     is <= 0); the drawn account leads, the rest keep declared
//     order.
//
// Quota-share cap (B2-09; the reference router #20561): after the strategy
// orders a group, orderAccountCapCheck moves accounts whose
// measured per-key remaining is 0 to the tail
// (LearnedKeyQuotaHint, learned from real upstream rate-limit
// headers). See the function comment for the measured-vs-
// configured divergence.
//
// expiry-first is NOT a value here: ordering accounts by their
// quota-expiry instant would need an expiry ordering signal;
// the per-key hint's expiresAt is only freshness for the cap
// check (an account is at cap while its 0-fraction entry is
// live), and scoring.Signals.ExpiresAt is per-(provider,
// model) and zero everywhere — see internal/strategy/wiring.go's
// data-honesty header — so no expiry ordering between accounts
// of one provider can be computed without inventing data.
// Parse rejects it; the engine falls back to fill-first for
// it. BLOCKED, not faked.
package router

import (
	"math"
	"math/rand"
	"sort"
	"strings"
)

// AccountFallbackStrategy is one account-level fallback strategy.
type AccountFallbackStrategy string

// Account-level fallback strategies (PARITY row 388).
const (
	// AccountFallbackPriority is declared pool order.
	AccountFallbackPriority AccountFallbackStrategy = "priority"
	// AccountFallbackFillFirst drains the first account before
	// the next (declared order; see the file header).
	AccountFallbackFillFirst AccountFallbackStrategy = "fill-first"
	// AccountFallbackRoundRobin rotates the leading account.
	AccountFallbackRoundRobin AccountFallbackStrategy = "round-robin"
	// AccountFallbackP2C is power-of-two-choices over accounts.
	AccountFallbackP2C AccountFallbackStrategy = "p2c"
	// AccountFallbackRandom shuffles the accounts.
	AccountFallbackRandom AccountFallbackStrategy = "random"
	// AccountFallbackLeastUsed orders by measured per-key
	// attempts, fewest first.
	AccountFallbackLeastUsed AccountFallbackStrategy = "least-used"
	// AccountFallbackCostOptimized is cheapest-first; within
	// one provider+model every account ties (see file header).
	AccountFallbackCostOptimized AccountFallbackStrategy = "cost-optimized"
	// AccountFallbackStrictRandom deals from a shuffled deck.
	AccountFallbackStrictRandom AccountFallbackStrategy = "strict-random"
	// AccountFallbackWeighted draws one leading account by
	// weight; weights are all 1 (no per-account weight source
	// exists), so the draw is uniform.
	AccountFallbackWeighted AccountFallbackStrategy = "weighted"
)

// AccountFallbackStrategies lists every supported account
// fallback strategy in a stable order.
var AccountFallbackStrategies = []AccountFallbackStrategy{
	AccountFallbackPriority,
	AccountFallbackFillFirst,
	AccountFallbackRoundRobin,
	AccountFallbackP2C,
	AccountFallbackRandom,
	AccountFallbackLeastUsed,
	AccountFallbackCostOptimized,
	AccountFallbackStrictRandom,
	AccountFallbackWeighted,
}

// ParseAccountFallbackStrategy normalises a strategy name
// (case-insensitive, surrounding whitespace trimmed). It
// reports false for "" and for unknown names — including
// "expiry-first", which is BLOCKED (no per-key quota-expiry
// source exists; see the file header), never silently accepted.
func ParseAccountFallbackStrategy(s string) (AccountFallbackStrategy, bool) {
	norm := AccountFallbackStrategy(strings.ToLower(strings.TrimSpace(s)))
	for _, st := range AccountFallbackStrategies {
		if norm == st {
			return st, true
		}
	}
	return "", false
}

// accountFallbackStrategyFor resolves the account fallback
// strategy for one provider: the per-provider override, then
// the global setting, then the "fill-first" default (the reference router's
// default). An unknown or empty value at either level falls
// through to the next.
func (e *Engine) accountFallbackStrategyFor(providerID string) AccountFallbackStrategy {
	if e.cfg.AccountFallbackStrategies != nil {
		if s, ok := ParseAccountFallbackStrategy(e.cfg.AccountFallbackStrategies[providerID]); ok {
			return s
		}
	}
	if s, ok := ParseAccountFallbackStrategy(e.cfg.AccountFallbackStrategy); ok {
		return s
	}
	return AccountFallbackFillFirst
}

// accountGroupKey is the (provider, model) group key for the
// per-group rotation / deck state.
func accountGroupKey(providerID, model string) string {
	return providerID + "\x00" + model
}

// accountDeck is the per-(provider, model) strict-random deck:
// order is the current shuffled order of indices into the
// group's declared order, pos is the next leader index. One full
// cycle deals every account as leader exactly once; on wrap the
// deck reshuffles with its new first account different from the
// last leader. Indices, not KeyIDs, so two attempts sharing one
// key (e.g. the same key at different efforts) stay distinct.
type accountDeck struct {
	order []int
	pos   int
}

// orderPreferredReserve is the row-487 mechanism as a pure
// function: a stable partition with preferred (non-reserve)
// attempts first and reserve attempts last. A pool of only
// reserve keys still serves (its keys are simply all last).
// Shared by chainForDispatch (the auto-combo candidate path,
// PARITY row 425) and orderAttempts' tail partition.
func orderPreferredReserve(chain []Attempt) []Attempt {
	var preferred, reserve []Attempt
	for _, att := range chain {
		if att.Reserve {
			reserve = append(reserve, att)
		} else {
			preferred = append(preferred, att)
		}
	}
	return append(preferred, reserve...)
}

// orderAccountAttempts reorders the accounts inside each
// (provider, model) group of chain under that provider's
// account fallback strategy. Groups keep their slots in the
// chain — only the accounts inside a group move — so the
// cross-provider / cross-model order the chain strategy hook
// produced is untouched. Single-account groups are untouched.
// The input chain is never mutated.
func (e *Engine) orderAccountAttempts(chain []Attempt) []Attempt {
	if len(chain) <= 1 {
		return chain
	}
	groups := make(map[string][]int, len(chain))
	var order []string
	for i, att := range chain {
		k := accountGroupKey(att.ProviderID, att.Model)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], i)
	}
	out := append([]Attempt(nil), chain...)
	for _, k := range order {
		idxs := groups[k]
		if len(idxs) <= 1 {
			continue
		}
		group := make([]Attempt, len(idxs))
		for j, i := range idxs {
			group[j] = chain[i]
		}
		ordered := e.orderAccountGroup(group)
		// Quota-share cap (B2-09; the reference router #20561 checks
		// Quota Share caps on in-request account rotation):
		// accounts whose measured per-key remaining is 0 sink
		// to the tail of the group, so rotation prefers
		// accounts with quota left. The strategy order above
		// is otherwise untouched — see orderAccountCapCheck.
		ordered = e.orderAccountCapCheck(ordered)
		for j, i := range idxs {
			out[i] = ordered[j]
		}
	}
	return out
}

// orderAccountCapCheck is the quota-share cap on in-request
// account rotation (B2-09; the reference router #20561 checks Quota Share
// caps on in-request account rotation). It is a stable
// partition, applied AFTER the group's fallback strategy has
// ordered the accounts: accounts under their cap keep their
// strategy order first, accounts at cap sink to the tail. It
// never drops an account and never reorders within either side,
// so every strategy's behaviour is unchanged when no account
// is at cap — and when every account is at cap the strategy
// order stands (fail-open: a capped account can still serve if
// it is the only option, and its window refills at the hint's
// expiry).
//
// The cap signal is MEASURED, not configured: the per-key
// remaining fraction learnRateLimit records from real upstream
// rate-limit headers (LearnedKeyQuotaHint). An account is at
// cap when its measured remaining is 0 and the entry is live
// (unexpired). No signal for an account means "not at cap" —
// a provider that reports no rate-limit headers teaches
// nothing, and rotation must not invent a cap for it.
//
// Divergence from the reference router (deliberate, recorded): the reference router's
// cap is a configured absolute cap_value per allocation row
// (the operator sets it; unconfigured keys have no cap) and
// the capped key is blocked. Ours needs no operator
// configuration — the cap is learned — and deprioritises
// instead of blocking, matching this tree's quota-share pool
// (saturated members go to the tail, never dropped;
// all-saturated fails open). A configured per-key cap_value
// surface is the follow-up, not built here: it needs a new
// config schema (quota_shares is per (provider, model), not
// per key), and inventing one is out of scope for this slice.
func (e *Engine) orderAccountCapCheck(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	under := make([]Attempt, 0, len(group))
	atCap := make([]Attempt, 0, len(group))
	for _, att := range group {
		if e.accountAtQuotaCap(att) {
			atCap = append(atCap, att)
		} else {
			under = append(under, att)
		}
	}
	if len(atCap) == 0 || len(under) == 0 {
		return group
	}
	return append(under, atCap...)
}

// accountAtQuotaCap reports whether one account has hit its
// quota-share cap: its measured per-key remaining fraction is 0
// (and the entry is live — LearnedKeyQuotaHint drops expired
// entries, so a refilled window reads as not-at-cap). No entry
// means no signal, which means not at cap.
func (e *Engine) accountAtQuotaCap(att Attempt) bool {
	hint, _, ok := e.LearnedKeyQuotaHint(att.ProviderID, att.KeyID)
	return ok && hint <= 0
}

// orderAccountGroup orders one (provider, model) group's
// accounts under the group's provider's account fallback
// strategy. All accounts share the provider, so the strategy
// resolves once per group.
func (e *Engine) orderAccountGroup(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	strategy := e.accountFallbackStrategyFor(group[0].ProviderID)
	switch strategy {
	case AccountFallbackPriority, AccountFallbackFillFirst, AccountFallbackCostOptimized:
		// Declared order: priority is the pool order;
		// fill-first drains in declared order (exhausted
		// PROVIDERS are already filtered upstream by
		// filterZeroQuota on the provider-level quotaHint;
		// no per-key quota source exists); every
		// account of one provider+model costs the same, so
		// cost-optimized ties and keeps declared order.
		return group
	case AccountFallbackLeastUsed:
		out := append([]Attempt(nil), group...)
		e.mu.Lock()
		counts := make([]int, len(out))
		for i, att := range out {
			counts[i] = e.keyAttempts[ClientKey(att.ProviderID, att.KeyID)]
		}
		e.mu.Unlock()
		sort.SliceStable(out, func(a, b int) bool { return counts[a] < counts[b] })
		return out
	case AccountFallbackRandom:
		out := append([]Attempt(nil), group...)
		e.randMu.Lock()
		defer e.randMu.Unlock()
		if e.rand == nil {
			return out
		}
		for i := len(out) - 1; i > 0; i-- {
			j := e.rand.Intn(i + 1)
			out[i], out[j] = out[j], out[i]
		}
		return out
	case AccountFallbackP2C:
		return e.orderAccountP2C(group)
	case AccountFallbackWeighted:
		return e.orderAccountWeighted(group)
	case AccountFallbackRoundRobin:
		return e.orderAccountRoundRobin(group)
	case AccountFallbackStrictRandom:
		return e.orderAccountStrictRandom(group)
	default:
		return group
	}
}

// accountP2CScore is the provider-level P2C composite for one
// provider: success rate + 1/log10(latency+10), an open breaker
// scoring -Inf (never wins a draw). Unknown (no attempts) scores
// 0 — unmeasured does not beat measured.
func (e *Engine) accountP2CScore(providerID string) float64 {
	if e.breakerOpen(providerID) {
		return math.Inf(-1)
	}
	st, ok := e.ProviderStats(providerID)
	if !ok || st.Samples <= 0 {
		return 0
	}
	lat := st.LatencyMs
	if lat < 0 {
		lat = 0
	}
	return st.SuccessRate + 1/math.Log10(lat+10)
}

// orderAccountP2C draws two distinct accounts uniformly at
// random (the reference router's draw: first = Intn(n), second =
// Intn(n-1) bumped past first), puts the better-scoring one
// first, and keeps every other account in declared order
// behind it. A tie keeps the first-drawn account.
func (e *Engine) orderAccountP2C(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	e.randMu.Lock()
	defer e.randMu.Unlock()
	if e.rand == nil {
		return group
	}
	n := len(group)
	first := e.rand.Intn(n)
	second := e.rand.Intn(n - 1)
	if second >= first {
		second++
	}
	winner := first
	if e.accountP2CScore(group[second].ProviderID) > e.accountP2CScore(group[first].ProviderID) {
		winner = second
	}
	// Both accounts share the provider, so their scores tie
	// unless the breaker state changed between the two reads;
	// the draw itself is the ordering work here.
	out := make([]Attempt, 0, n)
	out = append(out, group[winner])
	for i, att := range group {
		if i != winner {
			out = append(out, att)
		}
	}
	return out
}

// orderAccountWeighted draws one leading account uniformly
// (all weights 1 — no per-account weight source exists) and
// keeps the rest in declared order behind it.
func (e *Engine) orderAccountWeighted(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	e.randMu.Lock()
	defer e.randMu.Unlock()
	if e.rand == nil {
		return group
	}
	lead := e.rand.Intn(len(group))
	out := make([]Attempt, 0, len(group))
	out = append(out, group[lead])
	for i, att := range group {
		if i != lead {
			out = append(out, att)
		}
	}
	return out
}

// orderAccountRoundRobin rotates the group so the next account
// in the per-group rotation leads; the rest keep declared order
// behind it (stable rotation, not a reshuffle).
func (e *Engine) orderAccountRoundRobin(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	gk := accountGroupKey(group[0].ProviderID, group[0].Model)
	e.mu.Lock()
	start := e.accountRR[gk] % len(group)
	e.accountRR[gk] = start + 1
	e.mu.Unlock()
	out := make([]Attempt, 0, len(group))
	out = append(out, group[start:]...)
	out = append(out, group[:start]...)
	return out
}

// orderAccountStrictRandom deals the next leader from the
// per-group deck: the deck order decides the full order for
// this request (leader first, then the deck order), and the
// position advances one per request. When a cycle completes
// the deck reshuffles with its new first account different
// from the last leader, so no account leads twice in a row
// across the reshuffle boundary.
//
// Lock order is randMu then e.mu (the same order the P2C /
// weighted draws use): the deck state and the rand draws are
// both guarded here, so no other path can interleave a draw.
func (e *Engine) orderAccountStrictRandom(group []Attempt) []Attempt {
	if len(group) <= 1 {
		return group
	}
	gk := accountGroupKey(group[0].ProviderID, group[0].Model)
	e.randMu.Lock()
	defer e.randMu.Unlock()
	if e.rand == nil {
		return group
	}
	e.mu.Lock()
	deck := e.accountDecks[gk]
	if deck == nil || len(deck.order) != len(group) {
		order := make([]int, len(group))
		for i := range order {
			order[i] = i
		}
		deck = &accountDeck{order: order}
		e.accountDecks[gk] = deck
		shuffleInts(e.rand, deck.order)
		deck.pos = 0
	}
	leader := deck.order[deck.pos]
	lastLeader := leader
	deck.pos++
	if deck.pos >= len(deck.order) {
		// Cycle complete: reshuffle, new first != last leader.
		shuffleInts(e.rand, deck.order)
		for len(deck.order) > 1 && deck.order[0] == lastLeader {
			shuffleInts(e.rand, deck.order)
		}
		deck.pos = 0
	}
	order := append([]int(nil), deck.order...)
	e.mu.Unlock()
	// Rotate the deck order so the leader is first.
	out := make([]Attempt, 0, len(group))
	start := 0
	for i, idx := range order {
		if idx == leader {
			start = i
			break
		}
	}
	for i := 0; i < len(order); i++ {
		out = append(out, group[order[(start+i)%len(order)]])
	}
	return out
}

// shuffleInts shuffles idx in place with rnd (Fisher-Yates).
func shuffleInts(rnd *rand.Rand, idx []int) {
	for i := len(idx) - 1; i > 0; i-- {
		j := rnd.Intn(i + 1)
		idx[i], idx[j] = idx[j], idx[i]
	}
}
