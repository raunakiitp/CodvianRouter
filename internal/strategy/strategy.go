// Package strategy is the routing-strategy family for Codvian Router:
// the named strategies a chain (fallback chain / route) can use, how
// each one scores providers, and the per-request "auto:<axis>" alias.
//
// The strategies (each a real scoring rule over live signals — health
// probe latency, measured attempt latency/success from the engine,
// registry quota hints, and operator-configured model prices from
// the price book — never a stub), except priority, which is not a
// scoring rule at all:
//
//   - priority: the DEFAULT (M1.11): the operator's declared chain
//     order, no re-ranking. The first target in the chain is tried
//     first, exactly as configured. Unknown strategy names also
//     fall back to priority, never to a re-ranking.
//   - balanced: the existing ModeBalanced weights (latency .25,
//     success .25, cost .20, quality .20, quota .10).
//   - cheapest: cost-heavy (ModeCheap: cost .55). Cost is the real
//     blended price from the operator's price book (USD per 1M
//     tokens); a model with no configured price scores neutral on
//     cost, never cheapest.
//   - fastest: latency-heavy (ModeFast: latency .55).
//   - most-reliable: success-rate and quota heavy (ModeReliable:
//     success .45, quota .30).
//   - least-latency: pure latency with a reliability tiebreak
//     (latency .80, success .20, everything else 0) — unlike
//     fastest, cost/quality/quota never move the order.
//   - weighted: the per-target weight draw (the reference router
//     selectWeightedTarget): one target is drawn with
//     probability proportional to its weight. Per-target weights
//     do not exist in chain config yet, so every target draws
//     with weight 1 (uniform) until Phase C adds them. The old
//     operator weight-set behaviour is "custom-weights".
//   - code: cost per solved task (items 247–248, OpenRouter Auto
//     Code): the auto-router's "auto/code" mode. It is NOT a
//     weight set — it ranks by expected cost per solved task
//     (price × expected attempts, failures penalised harshly)
//     from recorded coding-task outcomes, at model-resolution
//     time in the auto-router. The chain-order hook below cannot
//     express that objective (it has no outcome evidence), so a
//     chain ordered under "code" falls back to balanced weights
//     for ordering providers serving the already-resolved model;
//     the cost-per-solved-task choice already happened.
//
// Quality, wherever a strategy weighs it, is the measured
// error-free completion rate from real attempts (see
// internal/scoring): successes / attempts, real attempts only.
// It is never a benchmark score or a claimed capability rating.
//
//   - priority: declared order, no re-ranking (the default).
//   - balanced: the existing ModeBalanced weights (latency .25,
//     success .25, cost .20, quality .20, quota .10).
//   - cheapest: cost-heavy (ModeCheap: cost .55).
//   - fastest: latency-heavy (ModeFast: latency .55).
//   - most-reliable: success-rate and quota heavy (ModeReliable:
//     success .45, quota .30).
//   - least-latency: pure latency with a reliability tiebreak
//     (latency .80, success .20, everything else 0) — unlike
//     fastest, cost/quality/quota never move the order.
//   - weighted: the per-target weight draw (see above);
//     "custom-weights" is the operator's own weight set (config
//     routing weighted_weights, normalised to sum to 1; invalid or
//     missing weights fall back to balanced, never to a broken ranking).
//   - code: cost per solved task — the auto/code mode (see the
//     first list above); not a weight set, so the chain-order
//     hook orders under balanced while the mode itself ranks by
//     expected cost per solved task at model-resolution time.
//
// Precedence for one request (highest first): an "auto:<axis>" alias
// in the requested model name, then the chain's own strategy
// override, then the global strategy. The alias and the chain
// override are resolved by the engine (internal/router); this
// package only names the strategies and builds the scoring hook
// for a resolved name.
//
// The "auto:<axis>" alias (FreeLLMAPI's per-chain-strategy rule,
// research-log entry 156): a client that asks for model
// "auto:cheapest" (or fastest / most-reliable / least-latency /
// balanced / weighted / code / priority) gets that strategy for this request,
// and the alias rewrites to the matching "auto/*" virtual model so
// the auto-router still resolves a concrete model
// (cheapest -> auto/cheap, fastest and least-latency -> auto/fast,
// most-reliable -> auto/reasoning, code -> auto/code, balanced,
// weighted and priority -> auto).
// With no auto-router wired the rewritten name fails honestly like
// any other unknown "auto/*" model.
package strategy

import (
	"log/slog"
	"math"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// Strategy is one named routing strategy.
type Strategy string

// Strategies.
const (
	// StrategyPriority is the operator's declared chain order:
	// no re-ranking, the first target in the chain is tried
	// first. This is the default when nothing is set (M1.11):
	// the reference router's default is priority, and ours was backwards
	// (balanced re-ranked every request). Balanced remains
	// selectable; priority simply honours the order the
	// operator wrote down.
	StrategyPriority Strategy = "priority"
	// StrategyBalanced weighs latency, reliability, cost and quality
	// roughly equally.
	StrategyBalanced Strategy = "balanced"
	// StrategyCheapest is cost-heavy: the cheapest provider that is
	// still reasonably reliable wins.
	StrategyCheapest Strategy = "cheapest"
	// StrategyFastest is latency-heavy: the fastest provider that is
	// still reasonably reliable wins.
	StrategyFastest Strategy = "fastest"
	// StrategyMostReliable is success-rate and quota heavy: providers
	// that succeed and still have headroom win.
	StrategyMostReliable Strategy = "most-reliable"
	// StrategyLeastLatency is pure latency with a reliability
	// tiebreak: the lowest-latency provider wins, full stop.
	StrategyLeastLatency Strategy = "least-latency"
	// StrategyLatencyAware prefers the lowest-latency candidate
	// among VIABLE ones (G10 Area 12 row 9): latency-dominant
	// weights (scoring.ModeLatencyAware: latency 0.70) with a
	// reliability guard — a candidate with measured attempts that
	// mostly fail is demoted behind viable candidates, however
	// fast it is. Unlike least-latency, a large cost gap can beat
	// a noise-level latency gap; unlike fastest, latency dominates
	// instead of merely leading. See wiringHook.
	StrategyLatencyAware Strategy = "latency-aware"
	// StrategyWeighted uses the operator's own weights (see
	// WeightsFor): every component weight is theirs to set.
	// NOTE (M3): "weighted" now means the reference router's per-target
	// weight draw (selectWeightedTarget); the operator weight-set
	// behaviour moved to StrategyCustomWeights ("custom-weights").
	// WeightsFor(StrategyWeighted) still returns the custom
	// weights for weight-only callers, so the old weight set
	// keeps working under its new name.
	StrategyWeighted Strategy = "weighted"
	// StrategyCode is cost per solved task (items 247–248):
	// the auto/code mode. It ranks models by expected cost per
	// solved task — price × expected attempts, with failures
	// penalised harshly — learned from recorded coding-task
	// outcomes. It is not a weight set: see WeightsFor.
	StrategyCode Strategy = "code"
	// StrategyCustomWeights is the operator's own weight set
	// (config routing weighted_weights, normalised to sum to 1).
	// Before M3 this behaviour was named "weighted"; M3 renames
	// it so "weighted" can mean the reference router's per-target weight
	// draw (see StrategyWeighted). Pre-1.0 rename, see LOG.md.
	StrategyCustomWeights Strategy = "custom-weights"
	// M3 ordering strategies (see wiring.go for the data each
	// one can actually see at hook time — several are PARTIAL
	// until quota/attempt/context data is plumbed in Phase C).
	StrategyRoundRobin       Strategy = "round-robin"
	StrategyRandom           Strategy = "random"
	StrategyStrictRandom     Strategy = "strict-random"
	StrategyFillFirst        Strategy = "fill-first"
	StrategyLeastUsed        Strategy = "least-used"
	StrategyP2C              Strategy = "p2c"
	StrategyQuotaWeighted    Strategy = "quota-weighted"
	StrategyHeadroom         Strategy = "headroom"
	StrategyExpiryFirst      Strategy = "expiry-first"
	StrategyResetAware       Strategy = "reset-aware"
	StrategyResetWindow      Strategy = "reset-window"
	StrategyLKGP             Strategy = "lkgp"
	StrategyContextOptimized Strategy = "context-optimized"
	StrategyCacheOptimized   Strategy = "cache-optimized"
	StrategyContextRelay     Strategy = "context-relay"
	StrategyCostOptimized    Strategy = "cost-optimized"
	StrategyShipFast         Strategy = "ship-fast"
	StrategyCostSaver        Strategy = "cost-saver"
	StrategyQualityFirst     Strategy = "quality-first"
	StrategyOfflineFriendly  Strategy = "offline-friendly"
	StrategyReliabilityFirst Strategy = "reliability-first"
	StrategyChaosMode        Strategy = "chaos-mode"
	// StrategyCascade is the 4-tier fallback cascade
	// (Subscription → API Key → Cheap → Free, B2-542). Its
	// ordering lives in the engine, not here: the router
	// intercepts it before the hook (internal/router/cascade.go
	// — orderAttemptsWithContext / rankAttemptsWithContext apply
	// applyCascadeOrder over the live auto-catalog tiers), the
	// same interception pattern as quota-share. This constant
	// exists so the name is selectable everywhere a strategy
	// string is validated — config global_strategy, the
	// dashboard picker (All), MCP strategy.Parse — and so the
	// hook factory can return the identity for it (see
	// wiringHook) instead of falling through to a balanced
	// re-rank on any path that calls the hook directly.
	StrategyCascade Strategy = "cascade"
	// StrategySLAAware ranks by SLA compliance: latency p95,
	// error rate and cost against the operator's objectives
	// (internal/sla). Alias "sla". See wiring.go.
	StrategySLAAware Strategy = "sla-aware"
	// StrategyNadir is the decision-API model-selection
	// strategy (PARITY row 396). It is NOT an ordering: the
	// model choice happens at auto/nadir dispatch time in the
	// router (which can do I/O), and the chain hook is the
	// identity — the declared (priority) order stands. It is
	// fail-CLOSED there: with no decision endpoint configured
	// no external call is ever made. See internal/router/nadir.go.
	StrategyNadir Strategy = "nadir"
)

// All lists every strategy in a stable order (the dashboard picker
// order and the config validation set). Priority leads: it is the
// default.
var All = []Strategy{
	StrategyPriority,
	StrategyBalanced,
	StrategyCheapest,
	StrategyFastest,
	StrategyMostReliable,
	StrategyLeastLatency,
	StrategyLatencyAware,
	StrategyWeighted,
	StrategyCustomWeights,
	StrategyCode,
	StrategyRoundRobin,
	StrategyRandom,
	StrategyStrictRandom,
	StrategyFillFirst,
	StrategyLeastUsed,
	StrategyP2C,
	StrategyQuotaWeighted,
	StrategyHeadroom,
	StrategyExpiryFirst,
	StrategyResetAware,
	StrategyResetWindow,
	StrategyLKGP,
	StrategyContextOptimized,
	StrategyCacheOptimized,
	StrategyContextRelay,
	StrategyCostOptimized,
	StrategyShipFast,
	StrategyCostSaver,
	StrategyQualityFirst,
	StrategyOfflineFriendly,
	StrategyReliabilityFirst,
	StrategyChaosMode,
	StrategyCascade,
	StrategySLAAware,
	StrategyNadir,
}

// aliases maps colloquial names to their canonical strategy.
var aliases = map[string]Strategy{
	"cheap":    StrategyCheapest,
	"fast":     StrategyFastest,
	"reliable": StrategyMostReliable,
	"latency":  StrategyLeastLatency,
	"eco":      StrategyCostOptimized,
	"custom":   StrategyCustomWeights,
	"sla":      StrategySLAAware,
}

// IsValid reports whether s names a known strategy (canonical name
// or alias). An empty string is not valid — it means "inherit".
func IsValid(s string) bool {
	_, ok := Parse(s)
	return ok
}

// Parse normalises a strategy name (case-insensitive, surrounding
// whitespace trimmed) to its canonical Strategy. Aliases ("cheap",
// "fast", "reliable", "latency") map to their canonical names.
// It reports false for "" and for unknown names; callers fall back
// to StrategyPriority on false, never to an error.
func Parse(s string) (Strategy, bool) {
	norm := Strategy(strings.ToLower(strings.TrimSpace(s)))
	for _, st := range All {
		if norm == st {
			return st, true
		}
	}
	if st, ok := aliases[string(norm)]; ok {
		return st, true
	}
	return "", false
}

// LatencyAwareWeights is the weight set for StrategyLatencyAware:
// the scoring.ModeLatencyAware preset (latency 0.70, success 0.15,
// cost/quality/quota 0.05 each). Kept as a named var so callers
// that build a scorer directly do not have to look the mode up.
var LatencyAwareWeights = scoring.Modes[scoring.ModeLatencyAware]

// LeastLatencyWeights is the weight set for StrategyLeastLatency:
// pure latency with a reliability tiebreak. Cost, quality and quota
// never move the order — that is what distinguishes it from
// fastest, where they still count a little.
var LeastLatencyWeights = scoring.Weights{Latency: 0.80, Success: 0.20}

// WeightsFor returns the scoring weights for a strategy name. An
// unknown name returns the balanced weights. For "custom-weights"
// the caller-supplied custom weights are normalised (see
// scoring.NewScorerWithWeights); invalid or missing custom weights
// fall back to balanced — a bad weight set can never produce a
// broken ranking. For "priority" the weights are balanced ON
// PURPOSE as a neutral fallback for weight-only callers: priority
// is not a weight set and its ordering hook never consults these
// weights — HookFactory returns the identity hook for it, so the
// chain keeps its declared order. The same holds for "weighted"
// (a draw, not a weight set), "code" (cost per solved task, not a
// weight set) and every M3 ordering strategy (the hook orders by
// its own rule, never by these weights).
func WeightsFor(s Strategy, custom scoring.Weights) scoring.Weights {
	switch s {
	case StrategyPriority:
		return scoring.Modes[scoring.ModeBalanced]
	case StrategyCheapest:
		return scoring.Modes[scoring.ModeCheap]
	case StrategyFastest:
		return scoring.Modes[scoring.ModeFast]
	case StrategyMostReliable:
		return scoring.Modes[scoring.ModeReliable]
	case StrategyLeastLatency:
		return LeastLatencyWeights
	case StrategyLatencyAware:
		return scoring.Modes[scoring.ModeLatencyAware]
	case StrategyCode:
		return scoring.Modes[scoring.ModeBalanced]
	case StrategyCustomWeights:
		if nw, ok := normalize(custom); ok {
			return nw
		}
		return scoring.Modes[scoring.ModeBalanced]
	default:
		return scoring.Modes[scoring.ModeBalanced]
	}
}

// normalize scales custom weights to sum to 1. It reports false
// when they cannot be used (any negative, NaN or infinite component,
// or a total <= 0).
func normalize(w scoring.Weights) (scoring.Weights, bool) {
	parts := []float64{w.Latency, w.Success, w.Cost, w.Quality, w.Quota}
	total := 0.0
	for _, p := range parts {
		if p < 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			return scoring.Weights{}, false
		}
		total += p
	}
	if total <= 0 {
		return scoring.Weights{}, false
	}
	return scoring.Weights{
		Latency: w.Latency / total,
		Success: w.Success / total,
		Cost:    w.Cost / total,
		Quality: w.Quality / total,
		Quota:   w.Quota / total,
	}, true
}

// Describe returns the human-readable name and one-line rule for a
// strategy, for the dashboard picker. Unknown names describe as
// balanced (the fallback the engine actually applies).
func Describe(s Strategy) (name, rule string) {
	switch s {
	case StrategyPriority:
		return "Priority", "Declared order first — targets are tried in the order the operator configured them, with no re-ranking."
	case StrategyCheapest:
		return "Cheapest", "Lowest cost first — the cheapest provider that is still reasonably reliable wins."
	case StrategyFastest:
		return "Fastest", "Lowest latency first, with reliability as a guard — the fastest provider that still succeeds wins."
	case StrategyMostReliable:
		return "Most reliable", "Highest success rate and most quota headroom first — the steadiest provider wins."
	case StrategyLeastLatency:
		return "Least latency", "Pure latency: the lowest measured latency wins; cost, quality and quota never move the order."
	case StrategyLatencyAware:
		return "Latency-aware", "Lowest latency among viable targets — latency dominates, but a target with measured attempts that mostly fail is demoted, and a large cost gap can beat a noise-level latency gap."
	case StrategyWeighted:
		return "Weighted", "Per-target weight draw: one target is drawn with probability proportional to its weight (all weights 1 until chain per-target weights land)."
	case StrategyCustomWeights:
		return "Custom weights", "Your own weights: every scoring component is weighted exactly as configured."
	case StrategyCode:
		return "Code (cost per solved task)", "Lowest expected cost per solved task — price × expected attempts, failures penalised harshly, so a cheap model that fails the task loses to a pricier model that solves it. Learns from recorded coding-task outcomes; with no evidence it ranks by price at a neutral solve rate."
	case StrategyRoundRobin:
		return "Round-robin", "Cycles evenly through targets: each request starts at the next target in the rotation."
	case StrategyRandom:
		return "Random", "Targets in random order (Fisher-Yates shuffle) — spreads load with no signal at all."
	case StrategyStrictRandom:
		return "Strict random", "Shuffled deck: no target repeats until every target has led once, then the deck reshuffles."
	case StrategyFillFirst:
		return "Fill-first", "Drains the first target's quota before moving to the next — quota is used up in declared order."
	case StrategyLeastUsed:
		return "Least used", "Least-attempted target first — evens out usage counts across targets."
	case StrategyP2C:
		return "Power of two choices", "Picks two targets at random and tries the better-scoring one first."
	case StrategyQuotaWeighted:
		return "Quota-weighted", "Weighted draw favouring targets with the best score relative to their load."
	case StrategyHeadroom:
		return "Headroom", "Most quota headroom first — the target with the most free capacity wins."
	case StrategyExpiryFirst:
		return "Expiry-first", "Soonest-expiring quota first — use quota before it expires."
	case StrategyResetAware:
		return "Reset-aware", "Orders by quota remaining weighted against time until the quota resets."
	case StrategyResetWindow:
		return "Reset-window", "Soonest quota reset first — targets whose quota refreshes soonest lead."
	case StrategyLKGP:
		return "Last known good", "The last target that succeeded for this chain leads — sticky to what works."
	case StrategyContextOptimized:
		return "Context-optimized", "Largest context window first — the model that fits the most context wins."
	case StrategyCacheOptimized:
		return "Cache-optimized", "Warmest prompt cache first — the target holding the most cached prefix wins."
	case StrategyContextRelay:
		return "Context-relay", "Carries context across attempts so a fallback target continues where the last one stopped."
	case StrategyCostOptimized:
		return "Cost-optimized", "Pure cheapest-first: lowest price wins, unknown price sorts last, never cheapest."
	case StrategyShipFast:
		return "Ship-fast (mode pack)", "16-factor weights tuned for latency: fastest delivery wins."
	case StrategyCostSaver:
		return "Cost-saver (mode pack)", "16-factor weights tuned for cost: cheapest delivery wins."
	case StrategyQualityFirst:
		return "Quality-first (mode pack)", "16-factor weights tuned for task fit and stability: best answer wins."
	case StrategyOfflineFriendly:
		return "Offline-friendly (mode pack)", "16-factor weights tuned for quota: targets with the most quota headroom win."
	case StrategyReliabilityFirst:
		return "Reliability-first (mode pack)", "16-factor weights tuned for health and stability: the steadiest target wins."
	case StrategyChaosMode:
		return "Chaos-mode (mode pack)", "16-factor weights tuned for health under faults; parallel fan-out is Phase C."
	case StrategySLAAware:
		return "SLA-aware", "Most SLA-compliant first — latency, error rate and cost scored against the operator's objectives (default 2000 ms / 5% / no cost cap). Limitation, disclosed honestly: the latency fed to the scorer is the measured p50 EWMA standing in for p95, not a true p95 (no latency histogram exists yet), and stability scores neutral 0.5 (no latency-stddev signal exists yet); unknown signals score neutral, never perfect."
	case StrategyNadir:
		return "Nadir (decision API)", "Model selection by an explicitly configured external decision endpoint (auto/nadir dispatch); as a chain strategy it orders nothing — the declared priority order stands, and with no endpoint configured no external call is ever made."
	case StrategyCascade:
		return "Cascade (4-tier fallback)", "Subscription → API key → cheap → free: the highest billing tier first; targets with no known tier keep their relative order last, never dropped."
	default:
		return "Balanced", "Latency, reliability, cost and quality weighed roughly equally."
	}
}

// AutoAxis parses an "auto:<axis>" request model. It returns the
// strategy the alias asks for and true when the model is such an
// alias with a known axis; otherwise it returns "" and false. The
// alias is case-insensitive ("AUTO:Cheapest" works) and tolerates
// surrounding whitespace.
func AutoAxis(model string) (Strategy, bool) {
	trimmed := strings.TrimSpace(model)
	if len(trimmed) <= 5 || !strings.EqualFold(trimmed[:5], "auto:") {
		return "", false
	}
	return Parse(trimmed[5:])
}

// AxisVirtualModel maps an auto:<axis> strategy to the "auto/*"
// virtual model the auto-router resolves it to. The mapping is the
// closest virtual model for the axis: cheapest -> auto/cheap,
// fastest and least-latency -> auto/fast, most-reliable ->
// auto/reasoning (the strong tier), code -> auto/code (the
// cost-per-solved-task mode), priority, balanced and weighted -> auto.
func AxisVirtualModel(s Strategy) string {
	switch s {
	case StrategyCheapest:
		return "auto/cheap"
	case StrategyFastest, StrategyLeastLatency, StrategyLatencyAware:
		return "auto/fast"
	case StrategyMostReliable:
		return "auto/reasoning"
	case StrategyCode:
		return "auto/code"
	case StrategyPriority:
		return "auto"
	default:
		return "auto"
	}
}

// CacheWarmthFunc reports the measured per-(provider, model)
// cached-token fraction — a historical per-pair average from the
// ledger, not a prefix-specific measurement: the provider-reported
// cached input tokens over provider-reported prompt tokens for
// that pair (see internal/ledger CachedTokenFraction), 0..1.
// ok=false means the pair has no measurement — no ledger data,
// never a guessed 0. A nil func means no warmth source is
// installed: every target is unmeasured. It is consulted on the
// request path, so it must be cheap and non-blocking — a caller
// that reads the ledger must have done the read before the request
// (or serve it from a snapshot), never a fresh query inside the hook.
type CacheWarmthFunc func(provider, model string) (fraction float64, ok bool)

// HookFactoryWithCacheAffinity is HookFactory for one request:
// the same hooks, but the returned factory also receives the
// request's StrategyHookContext, so the cache-optimized hook can
// apply the cacheability gate and order by measured warmth
// (wiringHook's StrategyCacheOptimized case). Every other
// strategy ignores the context and behaves exactly as under
// HookFactory. Unknown strategy names rank as priority (declared
// order, no re-ranking) and log one warning naming the unknown name
// (RT-5: the fallback is silent no longer), matching HookFactory.
// A nil warmth func means no warmth source: the
// cache-optimized hook then returns declared order (no measured
// warmth anywhere — see wiringHook). A nil signals func returns
// the identity hook for every strategy, matching HookFactory's
// nil rule.
func HookFactoryWithCacheAffinity(signals scoring.SignalsFunc, custom scoring.Weights, warmth CacheWarmthFunc) func(strategy string, ctx router.StrategyHookContext) func(chain []router.Attempt) []router.Attempt {
	state := newHookState()
	return func(strategyName string, ctx router.StrategyHookContext) func(chain []router.Attempt) []router.Attempt {
		if signals == nil {
			// No signal source: identity for every strategy,
			// including the signal-free orderings — the caller
			// opted out of live signals entirely.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		}
		st, ok := Parse(strategyName)
		if !ok {
			// RT-5: the fallback itself is unchanged (unknown ->
			// priority, matching the reference router), but it must not be
			// silent: one warning naming the unknown string and
			// the fallback, so a future surface that forgets to
			// validate leaves a trace.
			slog.Warn("strategy: unknown strategy name, falling back to priority", "strategy", strategyName, "fallback", "priority")
			st = StrategyPriority
		}
		if st == StrategyPriority {
			return func(chain []router.Attempt) []router.Attempt { return chain }
		}
		if hook := wiringHook(st, signals, custom, state, ctx, warmth); hook != nil {
			return hook
		}
		return scoring.HookWithWeights(signals, WeightsFor(st, custom))
	}
}

// HookFactory returns the router.Config.StrategyHook for a signal
// source and custom (custom-weights) weights: given a resolved
// strategy name it returns the OrderHook that ranks a chain under
// that strategy. Unknown strategy names rank as priority (declared
// order, no re-ranking) and log one warning naming the unknown name
// (RT-5: the fallback is silent no longer). "priority" itself returns the identity
// hook: the chain keeps the operator's declared order whatever the
// live signals say. A nil signals func returns the identity hook
// for every strategy (quota order stands), matching scoring.Hook's
// nil rule. The signals func is per (provider, model): cost comes
// from the operator's price book for the (provider, model) pair
// being served, so the same model at a cheap provider and an
// expensive provider ranks differently under "cheapest" — the pair
// price, never a model-only price. M3 strategies are dispatched
// to wiringHook (see wiring.go); stateful orderings (round-robin,
// strict-random deck, LKGP store) live in one shared state object
// for this factory instance.
//
// This factory carries no per-request context and no warmth
// source, so "cache-optimized" returns declared order under it
// (the cacheability gate has no prompt to measure and no target
// has measured warmth). The live path uses
// HookFactoryWithCacheAffinity, which threads the request context
// and the ledger warmth through to the same wiringHook case.
func HookFactory(signals scoring.SignalsFunc, custom scoring.Weights) func(strategy string) func(chain []router.Attempt) []router.Attempt {
	state := newHookState()
	return func(strategyName string) func(chain []router.Attempt) []router.Attempt {
		if signals == nil {
			// No signal source: identity for every strategy,
			// including the signal-free orderings — the caller
			// opted out of live signals entirely.
			return func(chain []router.Attempt) []router.Attempt { return chain }
		}
		st, ok := Parse(strategyName)
		if !ok {
			// RT-5: see HookFactoryWithCacheAffinity — same
			// warning, same fallback (unknown -> priority).
			slog.Warn("strategy: unknown strategy name, falling back to priority", "strategy", strategyName, "fallback", "priority")
			st = StrategyPriority
		}
		if st == StrategyPriority {
			return func(chain []router.Attempt) []router.Attempt { return chain }
		}
		if hook := wiringHook(st, signals, custom, state, router.StrategyHookContext{}, nil); hook != nil {
			return hook
		}
		return scoring.HookWithWeights(signals, WeightsFor(st, custom))
	}
}
