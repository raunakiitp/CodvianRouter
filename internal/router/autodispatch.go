// Auto/* variant dispatch (M3 Phase D, builder D2).
//
// This file wires the Phase C variant resolver
// (autorouter.ResolveAutoVariant) into the engine's request path:
// a request naming a known auto/* id is resolved against the live
// catalog instead of the embedding-tier router, the resolution's
// strategy becomes the request's effective strategy, and a panel /
// ordered resolution builds a multi-model chain.
//
// Live catalog: the engine holds no provider registry, so the
// catalog arrives through a process-wide supplier installed by
// cmd/codvian-router's buildAutoRouter (which owns the registry +
// price book). The supplier returns the STATIC catalog facts only
// (provider, model, price, quota hint, tier-membership category);
// this file enriches each entry per request with the engine's own
// LIVE signals (measured latency EWMA + success rate from real
// attempts, open-breaker health) — the same live record the
// scoring signals use, never invented numbers. A nil supplier
// means "no live catalog": variant dispatch does not run and the
// existing AutoRoute path serves exactly as before.
//
// Last-good: auto/lkgp needs the recorded last-known-good model.
// The LKGP registry lives in internal/strategy, which this package
// cannot import (cycle), and the server records successes keyed by
// the concrete serving model — there is no global-last accessor.
// So the last-good also arrives through a process-wide supplier,
// installed by whoever owns a real last-good source. With no
// supplier (the production state today) auto/lkgp errors honestly
// via the resolver — never a silent fallback. Tests install both
// suppliers directly.
//
// Pool ranker: the balanced pick scores through the same
// ModelRanker hook the server wires into the auto-router
// (main.go's rankModels closure). It arrives through a
// process-wide supplier like the catalog (SetAutoRanker). With
// no supplier the balanced pick keeps catalog order.
package router

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/modality"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/sla"
)

// autoDispatch is the per-request outcome of auto/* variant
// dispatch, carried from resolveAuto through resolveAutoRequest to
// the Chat/ChatStream chain-build lines.
type autoDispatch struct {
	// Strategy is the variant's effective strategy
	// ("quality-first", "ship-fast", "cost-saver", "code",
	// "chaos-mode", "lkgp", ...). "" when no variant applied.
	Strategy string
	// Ordered is the panel / ordered model list for
	// multi-model resolutions (auto/chaos panel, auto/thrifty
	// rung order). Nil for single-model resolutions — those
	// keep the exact existing chainFor(req.Model) path.
	Ordered []string
	// OrderedExplicit is true when Ordered is a multi-model
	// list whose order is final (a dispatched thrifty ladder,
	// subscription rung pool, chaos panel, nadir order): the
	// chain-build lines must treat it as explicit order — no
	// quota-hint sort, no strategy hook, no hedge may re-rank
	// it — exactly like a group member order (CHECKLIST 173).
	// False for single-model resolutions (len(Ordered) <= 1),
	// where the single-model chain path applies unchanged.
	OrderedExplicit bool
	// Err is a variant-resolution error (auto/subscription rung
	// empty, auto/lkgp with nothing recorded, ...). The
	// request must fail honestly with this error — never fall
	// back to another model, never a billable model.
	Err error
}

var (
	autoCatalogMu  sync.RWMutex
	autoCatalogFn  func() []autorouter.CatalogModel
	autoLastGoodMu sync.RWMutex
	autoLastGoodFn func() string
	autoRankerMu   sync.RWMutex
	autoRankerFn   autorouter.ModelRanker
)

// SetAutoCatalog installs the live variant-catalog supplier: the
// static catalog facts from the registry + price book (see the
// file header). A nil supplier clears it: variant dispatch does
// not run. Safe for concurrent use with the request path; install
// once at startup.
func SetAutoCatalog(fn func() []autorouter.CatalogModel) {
	autoCatalogMu.Lock()
	defer autoCatalogMu.Unlock()
	autoCatalogFn = fn
}

// SetAutoLastGood installs the last-known-good supplier for
// auto/lkgp: the recorded last-good model id, "" when none is
// recorded. A nil supplier clears it. Safe for concurrent use
// with the request path; install once at startup.
func SetAutoLastGood(fn func() string) {
	autoLastGoodMu.Lock()
	defer autoLastGoodMu.Unlock()
	autoLastGoodFn = fn
}

// SetAutoRanker installs the live pool ranker for variant
// dispatch: the same ModelRanker hook the server wires into the
// auto-router (main.go's rankModels closure: 16-factor ranking
// under DefaultWeights16 over the live signals closure). A nil
// ranker clears it: the balanced pick keeps catalog order. Safe
// for concurrent use with the request path; install once at
// startup.
func SetAutoRanker(fn autorouter.ModelRanker) {
	autoRankerMu.Lock()
	defer autoRankerMu.Unlock()
	autoRankerFn = fn
}

// autoRanker returns the installed pool ranker, or nil when none
// is installed.
func (e *Engine) autoRanker() autorouter.ModelRanker {
	autoRankerMu.RLock()
	fn := autoRankerFn
	autoRankerMu.RUnlock()
	return fn
}

// autoLastGood returns the recorded last-good model id, "" when
// none is recorded or no supplier is installed.
func (e *Engine) autoLastGood() string {
	autoLastGoodMu.RLock()
	fn := autoLastGoodFn
	autoLastGoodMu.RUnlock()
	if fn == nil {
		return ""
	}
	return strings.TrimSpace(fn())
}

// liveAutoCatalog returns the live variant catalog: the static
// supplier's entries enriched with the engine's live per-provider
// signals. Latency is the measured EWMA from real attempts and
// quality is the measured error-free completion rate (success
// rate) over real attempts — the same live record the scoring
// signals use. Health is set only for an open breaker (0.0);
// every other provider's health is unknown, never guessed.
// Remaining quota is the static entry's measured quota hint as
// a percentage (HasQuota only); no hint leaves it unknown, never
// 100%. Nil when no supplier is installed or the supplier
// reports no candidates: variant dispatch cannot run.
func (e *Engine) liveAutoCatalog() []autorouter.CatalogModel {
	autoCatalogMu.RLock()
	fn := autoCatalogFn
	autoCatalogMu.RUnlock()
	if fn == nil {
		return nil
	}
	base := fn()
	if len(base) == 0 {
		return nil
	}
	out := make([]autorouter.CatalogModel, len(base))
	copy(out, base)
	for i := range out {
		// Remaining quota (G19 Area 1, row 486 auto/thrifty):
		// the static entry's QuotaHeadroom is the registry
		// quota hint as a fraction, when one was actually
		// measured — carry it as a percentage so the thrifty
		// ladder's cutoffs (and rungExhausted) have real data
		// to gate on. No hint (HasQuota false) leaves
		// HasRemaining false: unknown, never 100%.
		if out[i].HasQuota {
			out[i].RemainingPercent = out[i].QuotaHeadroom * 100
			out[i].HasRemaining = true
		}
		st, ok := e.ProviderStats(out[i].Provider)
		if !ok || st.Samples <= 0 {
			continue
		}
		if st.LatencyMs > 0 {
			out[i].LatencyMs = st.LatencyMs
			out[i].HasLatency = true
		}
		out[i].Quality = st.SuccessRate
		out[i].HasQuality = true
		if e.breakerOpen(out[i].Provider) {
			out[i].Health = 0
			out[i].HasHealth = true
		}
	}
	return out
}

// slaPolicy builds the operator-configured SLA policy for the
// autorouter path (M3 Area 1, PARITY row 394): the Config SLA
// fields, resolved exactly the way the chain hook's context is
// (see strategyHookContextFor in router.go) — an unset target
// means 2000 ms, an unset (or non-positive, hand-built) error
// rate means the 0.05 default, cost cap and hard constraints
// pass through as set. An explicitly configured error rate of
// 0 (SLAMaxErrorRateSet, from routing.sla.max_error_rate)
// reaches the policy as 0 — zero-tolerance — instead of being
// rewritten to 0.05. The engine dispatch path passes this
// policy so auto/sla-aware honours what the operator configured.
func (e *Engine) slaPolicy() sla.Policy {
	p := sla.Policy{
		TargetP95Ms:        e.cfg.SLATargetP95Ms,
		MaxErrorRate:       e.cfg.SLAMaxErrorRate,
		MaxCostPer1MTokens: e.cfg.SLAMaxCostPer1M,
		HardConstraints:    e.cfg.SLAHardConstraints,
	}
	if p.TargetP95Ms <= 0 {
		p.TargetP95Ms = 2000
	}
	if p.MaxErrorRate <= 0 && !e.cfg.SLAMaxErrorRateSet {
		p.MaxErrorRate = 0.05
	}
	return p
}

// dispatchAutoVariant resolves one request's auto/* model through
// the Phase C variant resolver. It returns handled=false when the
// model is not a known variant id, when it is plain "auto" (which
// needs a prompt classification via the AutoRoute path), or when
// no live catalog is installed — in all three cases the caller
// runs the existing AutoRoute path unchanged. When handled, the
// returned request carries the resolved model and the dispatch
// carries the effective strategy, the panel/ordered list, or the
// resolver's error (fail-closed: the caller must surface Err,
// never fall back).
func (e *Engine) dispatchAutoVariant(ctx context.Context, req provider.ChatRequest) (provider.ChatRequest, autoDispatch, bool) {
	id := strings.TrimSpace(req.Model)
	if id == "auto" || !autorouter.IsKnownAutoID(id) {
		return req, autoDispatch{}, false
	}
	catalog := e.liveAutoCatalog()
	if len(catalog) == 0 {
		return req, autoDispatch{}, false
	}
	// auto/nadir (M3 Area 1, PARITY row 396): model selection
	// needs a live decision endpoint, so it cannot go through
	// the pure resolver — the resolver errors for it instead.
	// The decision runs here, at dispatch, via nadirSelect:
	// fail-CLOSED when unconfigured (priority fallback, no
	// external call), bounded POST when configured.
	if id == "auto/nadir" {
		model, ordered, notes := e.nadirSelect(ctx, catalog, lastUserPrompt(req))
		dec := autorouter.Decision{VirtualModel: id, Model: model, Reason: notes}
		e.setLastAutoDecision(&dec)
		if att := AttributionFrom(ctx); att != nil {
			att.PublishDecision(&dec)
		}
		if model == "" {
			return req, autoDispatch{Err: fmt.Errorf("auto/nadir: no model to serve (empty catalog)")}, true
		}
		req.Model = model
		return req, autoDispatch{Strategy: "nadir", Ordered: ordered, OrderedExplicit: len(ordered) > 1}, true
	}
	res, err := autorouter.ResolveAutoVariantWithPolicyAndRanker(id, catalog, e.autoLastGood(), e.slaPolicy(), e.autoRanker())
	if err != nil {
		dec := autorouter.Decision{VirtualModel: id, Reason: err.Error()}
		e.setLastAutoDecision(&dec)
		if att := AttributionFrom(ctx); att != nil {
			att.PublishDecision(&dec)
		}
		return req, autoDispatch{Err: err}, true
	}
	model := res.Model
	ordered := res.Ordered
	notes := res.Notes
	if id == "auto/smart" {
		// The resolver records exploration in Notes but does
		// not draw it: the 10% exploration draw happens HERE,
		// at dispatch, once per request.
		if alt, explored := e.smartExploration(catalog, model); explored {
			notes += fmt.Sprintf("; 10%% exploration at dispatch: picked %s instead of top pick %s", alt, model)
			model = alt
			ordered = []string{model}
		}
	}
	dec := autorouter.Decision{
		VirtualModel: id,
		Tier:         res.Tier,
		Model:        model,
		Reason:       notes,
	}
	e.setLastAutoDecision(&dec)
	if att := AttributionFrom(ctx); att != nil {
		att.PublishDecision(&dec)
	}
	switch {
	case len(res.Panel) > 0:
		// auto/chaos: the panel IS the decision — no single
		// model is faked. The request model becomes the first
		// panel model (for traces / forced-effort matching);
		// the chain carries the whole panel.
		req.Model = res.Panel[0]
		ordered = append([]string(nil), res.Panel...)
		return req, autoDispatch{Strategy: res.Strategy, Ordered: ordered, OrderedExplicit: len(ordered) > 1}, true
	case model != "":
		req.Model = model
		return req, autoDispatch{Strategy: res.Strategy, Ordered: ordered, OrderedExplicit: len(ordered) > 1}, true
	default:
		// A resolution naming no model and no panel cannot
		// serve: fail honestly instead of routing blind.
		err := fmt.Errorf("%s: variant resolved to no model and no panel", id)
		dec := autorouter.Decision{VirtualModel: id, Reason: err.Error()}
		e.setLastAutoDecision(&dec)
		if att := AttributionFrom(ctx); att != nil {
			att.PublishDecision(&dec)
		}
		return req, autoDispatch{Err: err}, true
	}
}

// smartExploration draws the auto/smart 10% exploration: with
// probability 0.10 the request picks a non-top model from the
// catalog instead of the top pick. The draw uses the engine's
// rand (time-seeded in NewEngine; tests override the seed for a
// deterministic draw — seed 9 explores, seed 0 exploits). One
// Float64 per auto/smart request, plus one Intn when exploring.
func (e *Engine) smartExploration(catalog []autorouter.CatalogModel, top string) (string, bool) {
	e.randMu.Lock()
	defer e.randMu.Unlock()
	if e.rand == nil {
		e.rand = rand.New(rand.NewSource(time.Now().UnixNano()))
	}
	if e.rand.Float64() >= 0.10 {
		return "", false
	}
	var pool []string
	for _, m := range catalog {
		if m.Model != top {
			pool = append(pool, m.Model)
		}
	}
	if len(pool) == 0 {
		return "", false
	}
	return pool[e.rand.Intn(len(pool))], true
}

// chainForModels builds the fallback chain for a list of models:
// the per-model chains concatenated in order, deduplicated by
// (provider, key, model), preserving first-seen order. Two
// attempts for the same (provider, model) under different keys
// are NOT duplicates — each key is its own target.
func (e *Engine) chainForModels(models []string) []Attempt {
	var out []Attempt
	seen := make(map[[3]string]struct{})
	for _, m := range models {
		for _, att := range e.chainFor(m) {
			key := [3]string{att.ProviderID, att.KeyID, att.Model}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, att)
		}
	}
	return out
}

// chainForDispatch builds the chain for a request after variant
// dispatch: the multi-model chain when a panel/ordered list is
// present, otherwise the exact existing single-model path. A
// one-element ordered list is still the single-model path — the
// chain for a lone model must not change shape.
//
// Preferred / reserve accounts for auto combos (M3 Area 1,
// PARITY row 425): the chain carries the row-487 mechanism at
// construction — preferred (non-reserve) attempts first,
// reserve attempts last — so the auto-combo candidate path
// orders accounts the same way the flat path does (orderAttempts
// re-applies the same partition later; the two are idempotent).
// The partition is stable within each side, so the resolver's
// model order is untouched.
func (e *Engine) chainForDispatch(model string, ordered []string) []Attempt {
	if len(ordered) > 1 || (len(ordered) > 0 && model == "") {
		return orderPreferredReserve(e.chainForModels(ordered))
	}
	return orderPreferredReserve(e.chainFor(model))
}

// maybeBridgeModality implements the modality bridges (B3-24):
// an image-bearing request whose chain head is CONFIRMED
// non-vision is rerouted to the first pool model confirmed
// vision-capable, rebuilding the chain for it (same ordered /
// effort treatment as the original chain, so auto resolutions
// and intent efforts survive the reroute). The target model for
// the capability check is the chain head — the concrete
// (provider, model) the request would actually dial — and its
// capability comes from the live VisionCapability hook, never
// from a guessed model id. Every fail-open applies: no media in
// the request, an empty chain, a missing pool hook, an unknown
// capability, or no capable alternative all return the chain and
// request untouched (the upstream then refuses honestly, exactly
// as before the bridge existed). A rebuilt chain that comes back
// empty (the alternative's provider has no client) also falls
// back to the original chain. Only vision can trigger a reroute
// today: audio/video detection exists, but no capability source
// reports them, so they never block.
func (e *Engine) maybeBridgeModality(chain []Attempt, req provider.ChatRequest, ordered []string, intentEffort string) ([]Attempt, provider.ChatRequest) {
	if len(chain) == 0 || e.cfg.VisionPool == nil {
		return chain, req
	}
	required := modality.Detect(req.Messages)
	if len(required) == 0 {
		return chain, req
	}
	first := chain[0]
	capable, known := e.modelVisionCapable(first.ProviderID, first.Model)
	// The decision needs the target as a provider.Model; the
	// struct is populated from the live hook above, never
	// invented — ID included so the row is traceable.
	target := provider.Model{ID: first.Model, Vision: capable, VisionKnown: known}
	decision, alt := modality.Decide(target, required, e.cfg.VisionPool())
	if decision != modality.Reroute {
		return chain, req
	}
	newChain := withResolvedEffort(e.chainForDispatch(alt.ModelID, ordered), intentEffort)
	if len(newChain) == 0 {
		return chain, req
	}
	req.Model = alt.ModelID
	return newChain, req
}
