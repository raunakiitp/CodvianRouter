// Auto-variant resolution (M3 Phase C, builder C5).
//
// This file resolves the `auto/*` ids the reference router documents in
// docs/routing/AUTO-COMBO.md and docs/routing/SUBSCRIPTION_LADDER.md
// that the local autorouter did not yet cover — auto/offline,
// auto/lkgp, auto/chaos, auto/subscription, auto/thrifty, the
// auto/best-* and auto/pro-* families, and auto/<category>:<tier>
// composition — into a routing decision over a caller-supplied
// catalog. It is PURE: no globals, no DB, no network, no prompt
// classification. Prompt classification for plain `auto` stays in
// Router.Classify/Resolve (autorouter.go); this resolver only maps
// an id to (pool filter, scoring strategy, resolved model).
//
// Semantics, each pinned to the reference (study only, no code
// copied):
//   - Flat ids: open-sse/services/autoCombo/builtinCatalog.ts
//     (AUTO_TEMPLATE_VARIANTS) and docs/routing/AUTO-COMBO.md:13-28.
//   - Category x tier: suffixComposition.ts (AUTO_CATEGORIES,
//     AUTO_TIERS, tierToWeightVariant, buildAutoCandidateFilter) and
//     AUTO-COMBO.md:30-45. Category/tier filtering is FAIL-OPEN:
//     "if a constraint matches no connected models, the full pool is
//     used so routing never breaks" (AUTO-COMBO.md:45) — the
//     resolution says so in Notes when that happens.
//   - auto/subscription: SUBSCRIPTION_LADDER.md:71-82 — rung 0 only,
//     hard-stop overage, live-verified quota headroom; an empty pool
//     is an ERROR, never a billable fallback. keyless does NOT
//     qualify. FAIL CLOSED.
//   - auto/thrifty: SUBSCRIPTION_LADDER.md:95-106 — all rungs in
//     order, exhausted quota-bearing rungs gated out, unknown quota
//     ADMITTED (fail open: trying a plan connection costs nothing).
//     Rung order subscriptionLadder.ts RUNG_ORDER:
//     subscription, keyless, free, cheap, premium.
//   - auto/chaos: AUTO-COMBO.md:28 + candidateProjection.ts —
//     parallel fan-out, one model per provider, panel of 5 by
//     default (CHAOS_MAX_PANEL, clamped [1,10]). NOT fault
//     injection. This resolver returns the PANEL; it never fakes
//     panel results.
//   - auto/smart: quality-first + 10% exploration (AUTO-COMBO.md:26).
//     Exploration is a per-request draw at dispatch, not a property
//     of a pure resolution, so it is recorded in Notes, not applied.
//   - auto/offline: most quota/headroom first (AUTO-COMBO.md:25),
//     offline-friendly pack (quota 0.3324). A model with no quota
//     fetcher keeps the default of full quota (AUTO-COMBO.md,
//     "Unreadable quota"); a fetcher that returned nothing readable
//     scores 0 (QuotaUnreadable).
//   - Unknown auto/* ids ERROR honestly — never silently resolve to
//     plain auto (that would route a request the caller did not ask
//     for).
//   - A resolution never names a model outside the given catalog:
//     every pick is an element of the input, or Model is "" with an
//     error / an honest note.
//
// The 4-tier fallback cascade (Subscription -> API Key -> Cheap ->
// Free) and the per-endpoint strategy projection live here too:
// OrderCascade and StrategiesForEndpoint / ProjectStrategyForEndpoint.
// The cascade order is the deep-study §1.1 wording (parity tracker Area 1
// notes its exact wording was "not re-verified fresh"); it is NOT
// the 5-rung ladder order above — the two are different orderings
// for different purposes, and each function documents which one it
// implements.
package autorouter

import (
	"fmt"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/sla"
)

// ---------------------------------------------------------------------------
// Catalog input.
// ---------------------------------------------------------------------------

// CatalogModel is one candidate model as the caller knows it. Every
// field is operator- or measurement-supplied; an unset signal field
// (Has* == false) means "unknown", never a guessed value. Price and
// latency use the same convention as the cost-optimized strategy:
// an unknown price sorts LAST, never cheapest.
type CatalogModel struct {
	// Provider is the provider id ("anthropic", "openai", ...).
	Provider string
	// Model is the full model id ("provider/model") — the value a
	// resolution returns. It must already exist in the caller's
	// catalog; this resolver never invents one.
	Model string
	// Category is the model's capability class for category
	// filtering: "coding", "reasoning", "vision", "chat",
	// "multimodal". "" means unclassified.
	Category string
	// Vision reports whether the model can see images (vision /
	// multimodal category filter).
	Vision bool
	// Reasoning reports whether the model is a reasoning/thinking
	// model (reasoning category filter).
	Reasoning bool
	// Tier is the economic tier from catalog pricing, the
	// classifyTier analogue: "free", "cheap" or "premium".
	// "" means unclassified.
	Tier string
	// Billing is the CONNECTION's billing class, not the model's
	// price (SUBSCRIPTION_LADDER.md: "Billing is a connection
	// fact, not a model fact"): "subscription", "keyless",
	// "metered" or "unknown". "" means unknown.
	Billing string
	// OverageHardStop reports whether the connection's overage
	// behaviour is a documented hard-stop (required for the
	// auto/subscription pool).
	OverageHardStop bool
	// PriceUSDPerMTok is the blended price (60% input + 40%
	// output, the auto-combo blended-cost convention), valid when
	// HasPrice. Unknown prices sort last, never cheapest.
	PriceUSDPerMTok float64
	HasPrice        bool
	// LatencyMs is the p95 latency in milliseconds, valid when
	// HasLatency.
	LatencyMs  float64
	HasLatency bool
	// QuotaHeadroom is the remaining quota fraction in [0,1],
	// valid when HasQuota. A model with no quota fetcher at all
	// (HasQuota == false, QuotaUnreadable == false) counts as
	// full headroom (1.0), the reference default of 100.
	QuotaHeadroom float64
	HasQuota      bool
	// QuotaUnreadable reports that a quota fetcher exists but
	// returned nothing readable: the model scores 0 headroom
	// (soft-deprioritised, never hard-blocked).
	QuotaUnreadable bool
	// RemainingPercent is the remaining quota in percent, valid
	// when HasRemaining. It drives the ladder cutoffs
	// (exit 2%, re-entry 5%).
	RemainingPercent float64
	HasRemaining     bool
	// Health is the breaker health score (CLOSED=1.0,
	// HALF_OPEN=0.5, OPEN=0.0), valid when HasHealth.
	Health    float64
	HasHealth bool
	// Quality is the feedback-driven output-quality signal in
	// [0,1], valid when HasQuality. No observations means no
	// signal (HasQuality == false), not a neutral 0.5 — the
	// neutral default lives in the scorer, not in this resolver.
	Quality    float64
	HasQuality bool
}

// Headroom returns the model's quota headroom in [0,1] for
// offline/headroom ranking: unreadable reads score 0, no fetcher
// counts as full (1.0), a known reading is clamped to [0,1].
func (m CatalogModel) Headroom() float64 {
	if m.QuotaUnreadable {
		return 0
	}
	if !m.HasQuota {
		return 1
	}
	if m.QuotaHeadroom < 0 {
		return 0
	}
	if m.QuotaHeadroom > 1 {
		return 1
	}
	return m.QuotaHeadroom
}

// ---------------------------------------------------------------------------
// Resolution.
// ---------------------------------------------------------------------------

// VariantResolution is one resolved auto/* id: which scoring
// strategy applies, which concrete model (if a single model is the
// answer), and why. Model is "" exactly when the id does not pick a
// single model: auto/chaos returns a Panel instead, plain "auto"
// needs a prompt classification (Router.Classify) before any model
// can be named, and an error return means no resolution at all.
type VariantResolution struct {
	// AutoID is the id that was resolved, as given.
	AutoID string
	// Strategy is the scoring strategy / mode pack that applies:
	// "balanced", "ship-fast", "cost-saver", "quality-first",
	// "offline-friendly", "reliability-first", "chaos-mode",
	// "lkgp" or "code" (auto/code: cost-per-solved-task ranking,
	// see code.go — this resolver only names the strategy).
	Strategy string
	// Tier is the autorouter Tier the id maps to, when it maps to
	// one (coding / reasoning / chat). Vision and multimodal have
	// no autorouter tier; Tier is "" and Category names the filter.
	Tier Tier
	// Category is the capability filter that applies ("coding",
	// "reasoning", "vision", "chat", "multimodal"), "" when none.
	Category string
	// TierFilter is the model-tier filter that applies ("free" or
	// "pro"), "" when none.
	TierFilter string
	// Model is the resolved concrete model id, an element of the
	// input catalog. "" when the id resolves to a panel, needs a
	// prompt, or names no single model.
	Model string
	// Panel is the auto/chaos fan-out panel: one model per
	// provider, at most ChaosPanelSize, in catalog order. Empty
	// for every other id. The panel is a dispatch list, not
	// results — no answer is faked here.
	Panel []string
	// Ordered is the ordered fallback pool for ids whose meaning
	// is an ordering, not a single pick: auto/thrifty (rung
	// order) and the cascade steps. For single-pick ids it holds
	// exactly [Model]. Empty when Model is "" and Panel is empty.
	Ordered []string
	// Notes explains the decision in plain words: which filter
	// applied, which signal decided the pick, and what was NOT
	// decided here (e.g. smart's 10% exploration, which happens
	// at dispatch). A fail-open filter that matched nothing says
	// so here.
	Notes string
}

// ChaosPanelSize is the default auto/chaos panel size: one model per
// provider, 5 by default (candidateProjection.ts,
// CHAOS_MAX_PANEL, clamped [1,10] there).
const ChaosPanelSize = 5

// Ladder cutoffs (subscriptionLadder.ts / SUBSCRIPTION_LADDER.md):
// a quota-bearing connection at or below ExitCutoffPercent counts
// as exhausted; a rung that just reset is re-admitted only above
// ReentryMinRemainingPercent.
const (
	// ExitCutoffPercent is the remaining-percent at/below which a
	// quota-bearing connection counts as exhausted (default 2,
	// matching quotaPreflight.defaultThresholdPercent).
	ExitCutoffPercent = 2.0
	// ReentryMinRemainingPercent is the remaining-percent a
	// quota-bearing connection must EXCEED to be re-admitted
	// after exhaustion (default 5; the hysteresis band).
	ReentryMinRemainingPercent = 5.0
)

// ValidAutoCategories and ValidAutoTiers are the suffixComposition
// axes (AUTO_CATEGORIES / AUTO_TIERS).
var (
	// ValidAutoCategories lists the category axis values.
	ValidAutoCategories = []string{"coding", "reasoning", "vision", "chat", "multimodal"}
	// ValidAutoTiers lists the tier axis values ("floor" is the
	// cheap alias; "subscription"/"thrifty" narrow by connection
	// billing, not model price).
	ValidAutoTiers = []string{"fast", "cheap", "floor", "free", "reliable", "pro", "subscription", "thrifty"}
)

// flatVariant maps a flat auto/* suffix to its (strategy, tier,
// category, tierFilter). It is the Go form of AUTO_TEMPLATE_VARIANTS
// (builtinCatalog.ts) plus the vision-category correction
// (VISION_CATEGORY_AUTO_IDS): auto/best-vision and auto/pro-vision
// resolve to the vision CATEGORY, not to a flat variant, so a
// vision request can never resolve to a text-only model.
var flatVariant = map[string]VariantResolution{
	"coding":  {Strategy: "quality-first", Tier: TierCoding, Category: "coding"},
	"code":    {Strategy: "code", Tier: TierCoding, Category: "coding"},
	"fast":    {Strategy: "ship-fast", Tier: TierQuickQA},
	"latency": {Strategy: "latency-aware", Tier: TierQuickQA},
	// "latency-aware" is the full name of the same variant, so
	// auto/latency-aware resolves exactly like auto/latency
	// (G10 Area 12 row 9).
	"latency-aware": {Strategy: "latency-aware", Tier: TierQuickQA},
	"cheap":         {Strategy: "cost-saver", Tier: TierQuickQA},
	"chat":          {Strategy: "balanced", Tier: TierChat, Category: "chat"},
	"offline":       {Strategy: "offline-friendly"},
	"smart":         {Strategy: "quality-first"},
	"lkgp":          {Strategy: "lkgp"},
	"chaos":         {Strategy: "chaos-mode"},
	// Combo panels (A2-2): auto/fusion, auto/pipeline and auto/jev are
	// real multi-target resolutions — the panel IS the decision (see
	// resolveComboPanel). auto:fusion / auto:pipeline / auto:jev map
	// here through the router's resolveAutoAlias, and the engine runs
	// the panel: fusion and jev fan out in parallel over the panel and
	// pick a winner with a judge; pipeline runs the panel as sequential
	// stages. The panel order is the resolved order; no answer is faked
	// in this pure resolver.
	"fusion":   {Strategy: "fusion"},
	"pipeline": {Strategy: "pipeline"},
	"jev":      {Strategy: "jev"},
	// sla-aware / sla (M3 Area 1, PARITY row 394): pick by SLA
	// compliance over the catalog's measured signals (see
	// pickByStrategy). "nadir" (row 396) is listed so
	// IsKnownAutoID recognises auto/nadir, but the pure
	// resolver refuses it — model selection needs a live
	// decision endpoint and runs at dispatch in the router
	// (fail-closed when unconfigured); see resolveFlat.
	"sla-aware":        {Strategy: "sla-aware"},
	"sla":              {Strategy: "sla-aware"},
	"nadir":            {Strategy: "nadir"},
	"subscription":     {Strategy: "balanced", TierFilter: "subscription"},
	"thrifty":          {Strategy: "cost-saver", TierFilter: "thrifty"},
	"best-coding":      {Strategy: "quality-first", Tier: TierCoding, Category: "coding"},
	"best-reasoning":   {Strategy: "quality-first", Tier: TierReasoning, Category: "reasoning"},
	"best-fast":        {Strategy: "ship-fast"},
	"best-vision":      {Strategy: "balanced", Category: "vision"},
	"best-chat":        {Strategy: "balanced", Tier: TierChat, Category: "chat"},
	"best-coding-fast": {Strategy: "ship-fast", Tier: TierCoding, Category: "coding"},
	"best-free":        {Strategy: "cost-saver", TierFilter: "free"},
	"best-chaos":       {Strategy: "chaos-mode"},
	"pro-coding":       {Strategy: "quality-first", Tier: TierCoding, Category: "coding", TierFilter: "pro"},
	"pro-reasoning":    {Strategy: "quality-first", Tier: TierReasoning, Category: "reasoning", TierFilter: "pro"},
	"pro-vision":       {Strategy: "balanced", Category: "vision", TierFilter: "pro"},
	"pro-chat":         {Strategy: "balanced", Tier: TierChat, Category: "chat", TierFilter: "pro"},
	"pro-fast":         {Strategy: "ship-fast", TierFilter: "pro"},
}

// tierStrategy maps a suffix tier to its scoring strategy
// (tierToWeightVariant in suffixComposition.ts): fast -> ship-fast,
// cheap/floor/thrifty -> cost-saver, reliable -> reliability-first.
// free/pro carry no weight bias (quality-first by default) — their
// effect is the pool filter. subscription has no weight bias either
// (balanced): the pool is already restricted to plan-included
// connections, so the scorer ranks them on merit.
func tierStrategy(tier string) string {
	switch tier {
	case "fast":
		return "ship-fast"
	case "cheap", "floor", "thrifty":
		return "cost-saver"
	case "reliable":
		return "reliability-first"
	case "free", "pro":
		return "quality-first"
	case "subscription":
		return "balanced"
	default:
		return "balanced"
	}
}

// categoryTier maps a category name to its autorouter Tier, when one
// exists. Vision and multimodal have no autorouter tier.
func categoryTier(category string) Tier {
	switch category {
	case "coding":
		return TierCoding
	case "reasoning":
		return TierReasoning
	case "chat":
		return TierChat
	default:
		return ""
	}
}

func isCategory(s string) bool {
	for _, c := range ValidAutoCategories {
		if s == c {
			return true
		}
	}
	return false
}

func isTier(s string) bool {
	for _, t := range ValidAutoTiers {
		if s == t {
			return true
		}
	}
	return false
}

// parseAutoSuffix parses the suffix after "auto/" into
// (category, tier, ok), the Go form of parseAutoSuffix
// (suffixComposition.ts): "<category>" or "<category>:<tier>".
// A bare tier name ("fast", "cheap", ...) is NOT a suffix form —
// those stay with the flat-variant table.
func parseAutoSuffix(suffix string) (category, tier string, ok bool) {
	if suffix == "" {
		return "", "", false
	}
	parts := strings.Split(suffix, ":")
	if len(parts) > 2 {
		return "", "", false
	}
	if len(parts) == 2 {
		if !isCategory(parts[0]) || !isTier(parts[1]) {
			return "", "", false
		}
		return parts[0], parts[1], true
	}
	if isCategory(parts[0]) {
		return parts[0], "", true
	}
	return "", "", false
}

// IsKnownAutoID reports whether id is a recognised built-in auto/*
// id: plain "auto", a flat variant, a best-*/pro-* id, or a valid
// auto/<category>[:<tier>] composition. It is the single source of
// truth the candidate endpoint uses to decide 404 vs. a pool.
func IsKnownAutoID(id string) bool {
	if id == "auto" {
		return true
	}
	if !strings.HasPrefix(id, "auto/") {
		return false
	}
	suffix := strings.TrimPrefix(id, "auto/")
	if suffix == "" {
		return false
	}
	if _, ok := flatVariant[suffix]; ok {
		return true
	}
	_, _, ok := parseAutoSuffix(suffix)
	return ok
}

// ResolveAutoVariant resolves one auto/* id against catalog.
//
// catalog is the caller's current candidate pool (every Model in it
// must already exist — the resolver never invents one). lastGood is
// the last-known-good model id for auto/lkgp ("" when none is
// recorded — auto/lkgp then errors honestly).
//
// An unknown id, an empty catalog, or a fail-closed empty pool is an
// error: the caller must surface it, never silently fall back to
// plain auto or to a billable model.
//
// The sla-aware pick scores under the default SLA policy here;
// ResolveAutoVariantWithPolicy scores under a caller-supplied
// policy instead (the engine dispatch path passes the
// operator-configured one).
func ResolveAutoVariant(autoID string, catalog []CatalogModel, lastGood string) (VariantResolution, error) {
	return ResolveAutoVariantWithPolicy(autoID, catalog, lastGood, sla.DefaultPolicy())
}

// ResolveAutoVariantWithPolicy is ResolveAutoVariant scored
// under policy: the operator-configured SLA (see
// sla.DefaultPolicy for the zero-policy meaning). Only the
// sla-aware pick reads the policy — every other strategy
// ignores it, so behaviour for non-SLA ids is identical.
// The balanced pick uses catalog order here: no ranker is
// supplied. Callers with a pool ranker use
// ResolveAutoVariantWithPolicyAndRanker.
func ResolveAutoVariantWithPolicy(autoID string, catalog []CatalogModel, lastGood string, policy sla.Policy) (VariantResolution, error) {
	return ResolveAutoVariantWithPolicyAndRanker(autoID, catalog, lastGood, policy, nil)
}

// ResolveAutoVariantWithPolicyAndRanker is
// ResolveAutoVariantWithPolicy with a caller-supplied pool
// ranker: when non-nil, the balanced (default) pick is the
// ranker's first model instead of catalog order. The ranker is
// the same ModelRanker hook the server wires into the
// auto-router (main.go's rankModels closure: 16-factor ranking
// under DefaultWeights16 over the live signals closure) — the
// balanced pick then scores on the same live signals as the
// tier ranking instead of taking pool[0]. A nil ranker keeps
// catalog order, exactly as before. The ranker must return a
// permutation of its input; a ranker that drops, duplicates or
// panics is ignored and catalog order stands (the same
// enforcement as Router.modelFor).
func ResolveAutoVariantWithPolicyAndRanker(autoID string, catalog []CatalogModel, lastGood string, policy sla.Policy, ranker ModelRanker) (VariantResolution, error) {
	if autoID == "auto" {
		// Plain auto needs a prompt classification before any
		// model can be named (Router.Classify). This pure
		// resolver records the strategy and refuses to pick a
		// model without one.
		if len(catalog) == 0 {
			return VariantResolution{}, fmt.Errorf("auto: empty catalog: no model to resolve")
		}
		return VariantResolution{
			AutoID:   autoID,
			Strategy: "balanced",
			Notes:    "balanced default (LKGP-sticky); no model chosen without a prompt — classify via Router.Classify/Resolve",
		}, nil
	}
	if !strings.HasPrefix(autoID, "auto/") {
		return VariantResolution{}, fmt.Errorf("not an auto/* id: %q", autoID)
	}
	suffix := strings.TrimPrefix(autoID, "auto/")
	if suffix == "" {
		return VariantResolution{}, fmt.Errorf("unknown auto variant %q: empty suffix", autoID)
	}

	// Flat variants first (builtinCatalog resolveAutoVariant
	// precedence: template table, then suffix parse).
	if base, ok := flatVariant[suffix]; ok {
		res := base
		res.AutoID = autoID
		return resolveFlat(res, catalog, lastGood, policy, ranker)
	}
	// Category x tier composition.
	if category, tier, ok := parseAutoSuffix(suffix); ok {
		res := VariantResolution{
			AutoID:   autoID,
			Strategy: tierStrategy(tier),
			Tier:     categoryTier(category),
			Category: category,
		}
		if tier == "free" || tier == "pro" {
			res.TierFilter = tier
		} else if tier == "subscription" || tier == "thrifty" {
			res.TierFilter = tier
		}
		return resolveComposed(res, category, tier, catalog, policy, ranker)
	}
	return VariantResolution{}, fmt.Errorf("unknown auto variant %q: no flat variant, category or tier matches", autoID)
}

// resolveFlat resolves a flat-variant base against the catalog.
func resolveFlat(res VariantResolution, catalog []CatalogModel, lastGood string, policy sla.Policy, ranker ModelRanker) (VariantResolution, error) {
	if len(catalog) == 0 {
		return VariantResolution{}, fmt.Errorf("%s: empty catalog: no model to resolve", res.AutoID)
	}
	// auto/nadir needs a live decision endpoint and a prompt:
	// the pure resolver has neither, so it refuses honestly
	// instead of picking a model without a decision. The engine
	// dispatch path (router.dispatchAutoVariant) intercepts
	// auto/nadir before calling this resolver.
	if res.Strategy == "nadir" {
		return VariantResolution{}, fmt.Errorf("auto/nadir: model selection needs a configured decision endpoint — resolve through the engine dispatch path, not the pure resolver")
	}
	switch res.AutoID {
	case "auto/lkgp":
		return resolveLKGP(res, catalog, lastGood)
	case "auto/chaos", "auto/best-chaos":
		return resolveChaos(res, catalog), nil
	case "auto/fusion", "auto/pipeline", "auto/jev":
		return resolveComboPanel(res, catalog), nil
	case "auto/subscription":
		return resolveSubscription(res, catalog)
	case "auto/thrifty":
		return resolveThrifty(res, catalog), nil
	}

	pool := catalog
	notes := ""
	// auto/code keeps the local tier set (autorouter.go):
	// coding + reasoning models only. An empty narrowing is an
	// error, like resolveCode's empty-pool failure — never a
	// silent fall-through to an unrelated model.
	if res.AutoID == "auto/code" {
		narrowed := []CatalogModel{}
		for _, m := range pool {
			if m.Category == "coding" || m.Reasoning {
				narrowed = append(narrowed, m)
			}
		}
		if len(narrowed) == 0 {
			return VariantResolution{}, fmt.Errorf("auto/code: pool empty after narrowing to coding/reasoning models — no model to score")
		}
		pool = narrowed
		notes = fmt.Sprintf("pool narrowed to coding/reasoning models (%d candidates)", len(pool))
	}
	// Tier filter (free/pro): free -> free-tier models only, pro ->
	// premium models only (classifyTier filter, suffixComposition).
	if res.TierFilter == "free" || res.TierFilter == "pro" {
		want := res.TierFilter
		if want == "pro" {
			want = "premium"
		}
		filtered := filterByTier(pool, want)
		if len(filtered) == 0 {
			notes = fmt.Sprintf("no %s-tier model in catalog; filter is fail-open so the full pool is used", want)
		} else {
			pool = filtered
			notes = fmt.Sprintf("pool filtered to %s-tier models (%d candidates)", want, len(pool))
		}
	}
	// Category filter (best-*/pro-* vision, reasoning...).
	// Only vision/multimodal and reasoning filter the pool
	// (buildAutoCandidateFilter); coding/chat add NO filter — the
	// category only selects the weight pack there, and the
	// vision-category ids resolve to the balanced default weights
	// (their flat "smart" variant is dropped by the vision
	// override in builtinCatalog).
	if res.Category != "" {
		filtered := filterByCategory(pool, res.Category)
		if filtered == nil {
			if notes != "" {
				notes += "; "
			}
			notes += fmt.Sprintf("category %s adds no pool filter (scoring only)", res.Category)
		} else if len(filtered) == 0 {
			if notes != "" {
				notes += "; "
			}
			notes += fmt.Sprintf("no %s-capable model in catalog; category filter is fail-open so the full pool is used", res.Category)
		} else {
			pool = filtered
			if notes != "" {
				notes += "; "
			}
			notes += fmt.Sprintf("pool filtered to %s models (%d candidates)", res.Category, len(pool))
		}
	}
	if res.AutoID == "auto/smart" {
		if notes != "" {
			notes += "; "
		}
		notes += "quality-first + 10% exploration at dispatch (exploration is a per-request draw, not applied here)"
	}
	model, pickNote := pickByStrategyWithPolicy(res.Strategy, pool, policy, ranker, res.Tier)
	res.Model = model
	res.Ordered = []string{model}
	if pickNote != "" {
		if notes != "" {
			notes += "; "
		}
		notes += pickNote
	}
	res.Notes = notes
	return res, nil
}

// resolveComposed resolves an auto/<category>[:<tier>] id.
func resolveComposed(res VariantResolution, category, tier string, catalog []CatalogModel, policy sla.Policy, ranker ModelRanker) (VariantResolution, error) {
	if len(catalog) == 0 {
		return VariantResolution{}, fmt.Errorf("%s: empty catalog: no model to resolve", res.AutoID)
	}
	// Subscription / thrifty tiers narrow by CONNECTION billing,
	// not model price (suffixComposition: applied in the factory
	// against live connection state). They compose with the
	// category filter first.
	pool := catalog
	notes := ""
	if category != "" {
		filtered := filterByCategory(pool, category)
		if filtered == nil {
			notes = fmt.Sprintf("category %s adds no pool filter (scoring only)", category)
		} else if len(filtered) == 0 {
			notes = fmt.Sprintf("no %s-capable model in catalog; category filter is fail-open so the full pool is used", category)
		} else {
			pool = filtered
			notes = fmt.Sprintf("pool filtered to %s models (%d candidates)", category, len(pool))
		}
	}
	switch tier {
	case "subscription":
		sub, err := resolveSubscription(res, pool)
		if err != nil {
			return VariantResolution{}, err
		}
		if notes != "" {
			sub.Notes = notes + "; " + sub.Notes
		}
		return sub, nil
	case "thrifty":
		thrifty := resolveThrifty(res, pool)
		if notes != "" {
			thrifty.Notes = notes + "; " + thrifty.Notes
		}
		return thrifty, nil
	case "free", "pro":
		want := tier
		if want == "pro" {
			want = "premium"
		}
		filtered := filterByTier(pool, want)
		if len(filtered) == 0 {
			if notes != "" {
				notes += "; "
			}
			notes += fmt.Sprintf("no %s-tier model in catalog; filter is fail-open so the full pool is used", want)
		} else {
			pool = filtered
			if notes != "" {
				notes += "; "
			}
			notes += fmt.Sprintf("pool filtered to %s-tier models (%d candidates)", want, len(pool))
		}
	}
	model, pickNote := pickByStrategyWithPolicy(res.Strategy, pool, policy, ranker, res.Tier)
	res.Model = model
	res.Ordered = []string{model}
	if pickNote != "" {
		if notes != "" {
			notes += "; "
		}
		notes += pickNote
	}
	res.Notes = notes
	return res, nil
}

// resolveLKGP resolves auto/lkgp: the explicit last-known-good
// model. The LKGP model must be IN the catalog; a recorded model
// that is no longer connected, or no recorded model at all, is an
// honest error — never a silent fall-through to another model,
// which would break the stickiness the id promises.
func resolveLKGP(res VariantResolution, catalog []CatalogModel, lastGood string) (VariantResolution, error) {
	if strings.TrimSpace(lastGood) == "" {
		return VariantResolution{}, fmt.Errorf("auto/lkgp: no last-known-good model recorded")
	}
	for _, m := range catalog {
		if m.Model == lastGood {
			res.Model = m.Model
			res.Ordered = []string{m.Model}
			res.Notes = fmt.Sprintf("explicit last-known-good model %s (sticky; no re-ranking)", m.Model)
			return res, nil
		}
	}
	return VariantResolution{}, fmt.Errorf("auto/lkgp: last-known-good model %q is not in the catalog", lastGood)
}

// resolveChaos resolves auto/chaos: a parallel fan-out panel of one
// model per provider, at most ChaosPanelSize, in catalog order
// (candidateProjection: first model per provider, diverse panel).
// Model stays "" — the panel IS the decision; no single answer is
// faked and no panel result is invented here.
func resolveChaos(res VariantResolution, catalog []CatalogModel) VariantResolution {
	seen := make(map[string]struct{})
	panel := []string{}
	for _, m := range catalog {
		if len(panel) >= ChaosPanelSize {
			break
		}
		if _, dup := seen[m.Provider]; dup {
			continue
		}
		seen[m.Provider] = struct{}{}
		panel = append(panel, m.Model)
	}
	res.Panel = panel
	res.Ordered = append([]string(nil), panel...)
	res.Notes = fmt.Sprintf("parallel fan-out panel of %d (one model per provider, max %d); dispatch fans out, no judge synthesis here", len(panel), ChaosPanelSize)
	return res
}

// resolveComboPanel resolves auto/fusion, auto/pipeline and auto/jev
// (A2-2): the MULTI-TARGET panel the engine runs. fusion and jev fan
// out in parallel over the panel and pick a winner with a judge;
// pipeline runs the panel as sequential stages. The panel is one
// model per provider, at most ChaosPanelSize, in catalog order (the
// same candidateProjection diversity auto/chaos uses). Model stays ""
// — the panel IS the decision, and no panel result is faked in this
// pure resolver; the engine executes it.
func resolveComboPanel(res VariantResolution, catalog []CatalogModel) VariantResolution {
	panel := make([]string, 0, ChaosPanelSize)
	seen := make(map[string]struct{})
	for _, m := range catalog {
		if len(panel) >= ChaosPanelSize {
			break
		}
		if _, dup := seen[m.Provider]; dup {
			continue
		}
		seen[m.Provider] = struct{}{}
		panel = append(panel, m.Model)
	}
	res.Panel = panel
	res.Ordered = append([]string(nil), panel...)
	switch res.Strategy {
	case "pipeline":
		res.Notes = fmt.Sprintf("multi-target pipeline of %d (one model per provider, max %d); the engine runs it as sequential stages", len(panel), ChaosPanelSize)
	default:
		res.Notes = fmt.Sprintf("multi-target %s panel of %d (one model per provider, max %d); the engine fans out in parallel and picks a winner with a judge", res.Strategy, len(panel), ChaosPanelSize)
	}
	return res
}

// resolveSubscription resolves auto/subscription, FAIL CLOSED
// (SUBSCRIPTION_LADDER.md:71-82): pool = rung 0 only — curated
// billing "subscription", documented hard-stop overage, each
// connection verified live to have quota headroom (remaining above
// the exit cutoff, with a fresh reading). Everything ambiguous is
// excluded: uncurated providers, unverifiable or stale readings,
// overage that meters to paid. An empty pool is the intended
// answer: an error, never a billable fallback. keyless does NOT
// qualify ("the plan I pay for").
func resolveSubscription(res VariantResolution, catalog []CatalogModel) (VariantResolution, error) {
	pool := []CatalogModel{}
	for _, m := range catalog {
		if m.Billing != "subscription" {
			continue
		}
		if !m.OverageHardStop {
			continue
		}
		if !m.HasRemaining {
			continue
		}
		if m.RemainingPercent <= ExitCutoffPercent {
			continue
		}
		pool = append(pool, m)
	}
	if len(pool) == 0 {
		return VariantResolution{}, fmt.Errorf("auto/subscription: no subscription-quota target can serve (rung 0 empty): failing closed, no billable fallback")
	}
	// Within the rung the scorer ranks on merit; deterministically
	// that is most remaining quota first, catalog order on ties.
	best := pool[0]
	for _, m := range pool[1:] {
		if m.RemainingPercent > best.RemainingPercent {
			best = m
		}
	}
	res.Model = best.Model
	res.Ordered = modelsOf(pool)
	res.Notes = fmt.Sprintf("subscription rung only (%d verified candidates); most remaining quota first; fail closed — no billable fallback", len(pool))
	return res, nil
}

// resolveThrifty resolves auto/thrifty: ALL rungs in order, with
// exhausted quota-bearing rungs gated out and unknown quota
// ADMITTED (fail open — trying a plan connection costs nothing,
// and the dispatcher's fall-through reaches the next rung if it
// turns out exhausted). Ordering is stable within a rung, so the
// scorer's own ranking inside a rung is never reshuffled here:
// within a rung, most remaining quota first, catalog order on
// ties. This function never errors on an empty pool — that case is
// the caller's empty-catalog error; a thrifty pool that gates to
// nothing still returns the rung order with no survivors noted.
func resolveThrifty(res VariantResolution, catalog []CatalogModel) VariantResolution {
	ordered := orderPoolByRung(catalog)
	res.Ordered = modelsOf(ordered)
	if len(ordered) > 0 {
		res.Model = ordered[0].Model
	}
	res.Notes = fmt.Sprintf("ladder order subscription -> keyless -> free -> cheap -> premium (%d surviving candidates); exhausted quota rungs gated out, unknown quota admitted", len(ordered))
	return res
}

// ---------------------------------------------------------------------------
// Ladder rungs (subscriptionLadder.ts RUNG_ORDER).
// ---------------------------------------------------------------------------

// LadderRungs lists the subscription-ladder rungs in escalation
// order. Index is the ordering key.
var LadderRungs = []string{"subscription", "keyless", "free", "cheap", "premium"}

// rungIndex returns a rung's escalation index, or len(LadderRungs)
// for an unknown rung (sorts last, never first).
func rungIndex(rung string) int {
	for i, r := range LadderRungs {
		if r == rung {
			return i
		}
	}
	return len(LadderRungs)
}

// assignRung assigns a catalog model to its ladder rung: the
// connection's billing class decides subscription/keyless; a
// metered connection's rung comes from the model's economic tier
// (free / cheap / premium). An uncurated provider (unknown
// billing) is consumed as metered (SUBSCRIPTION_LADDER.md:
// "Uncurated is not free"), so it lands on a paid rung, never on
// the subscription rung.
func assignRung(m CatalogModel) string {
	switch m.Billing {
	case "subscription":
		return "subscription"
	case "keyless":
		return "keyless"
	default:
		// Metered or unknown billing: rung by model tier.
		switch m.Tier {
		case "free":
			return "free"
		case "cheap":
			return "cheap"
		default:
			return "premium"
		}
	}
}

// rungExhausted reports whether a quota-bearing rung member is
// exhausted: remaining at/below the exit cutoff, or — for
// auto/thrifty only — never exhausted on missing telemetry (the
// caller decides admission; see orderPoolByRung). Paid rungs
// (cheap/premium) have no quota: they are never exhausted here —
// their only exhaustion signal is a per-rung budget, which is
// reserved configuration, not enforced (SUBSCRIPTION_LADDER.md).
func rungExhausted(m CatalogModel, admitUnknown bool) bool {
	rung := assignRung(m)
	switch rung {
	case "subscription", "keyless", "free":
		if !m.HasRemaining {
			return !admitUnknown
		}
		return m.RemainingPercent <= ExitCutoffPercent
	default:
		return false
	}
}

// orderPoolByRung orders a pool by ladder rung, gating out
// exhausted quota-bearing members (unknown quota admitted: the
// thrifty rule). Stable within a rung: most remaining quota first,
// catalog order on ties.
func orderPoolByRung(catalog []CatalogModel) []CatalogModel {
	kept := []CatalogModel{}
	for _, m := range catalog {
		if rungExhausted(m, true) {
			continue
		}
		kept = append(kept, m)
	}
	sort.SliceStable(kept, func(i, j int) bool {
		ri, rj := rungIndex(assignRung(kept[i])), rungIndex(assignRung(kept[j]))
		if ri != rj {
			return ri < rj
		}
		// Within a rung: most remaining quota first when both
		// report it; otherwise catalog order (stable sort).
		if kept[i].HasRemaining && kept[j].HasRemaining && kept[i].RemainingPercent != kept[j].RemainingPercent {
			return kept[i].RemainingPercent > kept[j].RemainingPercent
		}
		return false
	})
	return kept
}

// ---------------------------------------------------------------------------
// Pool filters and picks.
// ---------------------------------------------------------------------------

// filterByCategory keeps models matching a category capability
// and returns nil when the category adds no filter, mirroring
// buildAutoCandidateFilter (suffixComposition.ts), which returns
// null when no filtering is needed: vision/multimodal keep
// vision-capable models, reasoning keeps reasoning/thinking
// models. Coding and chat add NO filter — their effect is the
// scoring weights, not pool membership.
func filterByCategory(pool []CatalogModel, category string) []CatalogModel {
	switch category {
	case "vision", "multimodal":
		out := []CatalogModel{}
		for _, m := range pool {
			if m.Vision {
				out = append(out, m)
			}
		}
		return out
	case "reasoning":
		out := []CatalogModel{}
		for _, m := range pool {
			if m.Reasoning {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

// filterByTier keeps models whose economic tier matches want
// ("free" or "premium").
func filterByTier(pool []CatalogModel, want string) []CatalogModel {
	out := []CatalogModel{}
	for _, m := range pool {
		if m.Tier == want {
			out = append(out, m)
		}
	}
	return out
}

// rankedFirst orders pool with ranker and returns the first model
// id, best first — or ("", false) when ranker is nil or its result
// is not a permutation of the pool. A ranker that drops, duplicates
// or panics is ignored and catalog order stands: the same
// enforcement as Router.modelFor, because a routing decision must
// never crash the server or invent a model. The tier is passed
// through for rankers that use it; the server's 16-factor ranker
// ranks any model list by best-pair score and ignores it.
func rankedFirst(ranker ModelRanker, tier Tier, pool []CatalogModel) (string, bool) {
	if ranker == nil {
		return "", false
	}
	models := modelsOf(pool)
	ranked := func() (out []string) {
		defer func() {
			if recover() != nil {
				out = nil
			}
		}()
		return ranker(tier, append([]string(nil), models...))
	}()
	if !isPermutation(ranked, models) {
		return "", false
	}
	return ranked[0], true
}

// pickByStrategy picks one model from pool by scoring strategy.
// Every pick is an element of pool (never invented); the note
// names the deciding signal, or that no signal existed and catalog
// order decided. The sla-aware pick scores under the default SLA
// policy; pickByStrategyWithPolicy scores under a caller-supplied
// policy instead. The balanced pick uses catalog order here: no
// ranker is supplied.
func pickByStrategy(strategy string, pool []CatalogModel) (string, string) {
	return pickByStrategyWithPolicy(strategy, pool, sla.DefaultPolicy(), nil, "")
}

// pickByStrategyWithPolicy is pickByStrategy scored under
// policy. Only the sla-aware case reads the policy — every other
// strategy ignores it. The balanced (default) case reads ranker:
// when non-nil, the pick is the ranker's first model for tier
// instead of pool[0] (see rankedFirst); a nil ranker keeps
// catalog order.
func pickByStrategyWithPolicy(strategy string, pool []CatalogModel, policy sla.Policy, ranker ModelRanker, tier Tier) (string, string) {
	if len(pool) == 0 {
		return "", "empty pool: no model to pick"
	}
	switch strategy {
	case "cost-saver":
		best := -1
		for i, m := range pool {
			if !m.HasPrice {
				continue // unknown price sorts last, never cheapest
			}
			if best < 0 || m.PriceUSDPerMTok < pool[best].PriceUSDPerMTok {
				best = i
			}
		}
		if best < 0 {
			return pool[0].Model, "no prices known in pool; catalog order decides (cheapest unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("cheapest in pool ($%.4f/MTok blended)", pool[best].PriceUSDPerMTok)
	case "ship-fast":
		best := -1
		for i, m := range pool {
			if !m.HasLatency {
				continue
			}
			if best < 0 || m.LatencyMs < pool[best].LatencyMs {
				best = i
			}
		}
		if best < 0 {
			return pool[0].Model, "no latency signal in pool; catalog order decides (fastest unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("lowest p95 latency in pool (%.0f ms)", pool[best].LatencyMs)
	case "latency-aware":
		// Latency-aware pick (G10 Area 12 row 9): lowest latency
		// among VIABLE models — a model with a measured quality
		// (the live catalog sets Quality to the measured success
		// rate) below 0.5 is non-viable and demoted, mirroring
		// the strategy hook's viability rule (strategy wiring.go)
		// with the signals this pure resolver can see. Unknown
		// quality fails open (viable). When no model has a
		// latency signal, or no viable model has one, catalog
		// order decides and the note says so.
		best := -1
		for i, m := range pool {
			if !m.HasLatency {
				continue
			}
			if m.HasQuality && m.Quality < 0.5 {
				continue // measured mostly-failing: non-viable
			}
			if best < 0 || m.LatencyMs < pool[best].LatencyMs {
				best = i
			}
		}
		if best >= 0 {
			return pool[best].Model, fmt.Sprintf("lowest p95 latency among viable models in pool (%.0f ms)", pool[best].LatencyMs)
		}
		// No viable model with a latency signal: fall back to the
		// lowest latency overall rather than dropping the pick,
		// and say so — the engine re-ranks at dispatch with live
		// signals.
		for i, m := range pool {
			if !m.HasLatency {
				continue
			}
			if best < 0 || m.LatencyMs < pool[best].LatencyMs {
				best = i
			}
		}
		if best < 0 {
			return pool[0].Model, "no latency signal in pool; catalog order decides (fastest unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("lowest p95 latency in pool (%.0f ms; no viable model had a latency signal, so viability could not filter)", pool[best].LatencyMs)
	case "sla-aware":
		// SLA-aware pick (M3 Area 1, PARITY row 394): the model
		// whose measured signals best meet the caller's SLA
		// policy (the operator-configured one on the engine
		// dispatch path, the default — 2000 ms p95, 5%
		// errors, no cost cap, soft constraints — through the
		// plain resolver) under internal/sla scoring. LatencyMs
		// is the catalog p95; the error rate is 1 - Quality
		// when measured — the live catalog sets Quality to the
		// measured success rate, so that IS the error rate
		// there; Health is the breaker health (0 = open,
		// counts as a violation). Unknown signals score
		// neutral, never perfect.
		var maxCost float64 = -1
		for _, m := range pool {
			if m.HasPrice && m.PriceUSDPerMTok > maxCost {
				maxCost = m.PriceUSDPerMTok
			}
		}
		best := -1
		var bestViol, bestScore float64
		for i, m := range pool {
			c := sla.Candidate{}
			if m.HasLatency {
				c.LatencyMs = m.LatencyMs
				c.HasLatency = true
			}
			if m.HasQuality {
				c.ErrorRate = 1 - m.Quality
				if c.ErrorRate < 0 {
					c.ErrorRate = 0
				}
				if c.ErrorRate > 1 {
					c.ErrorRate = 1
				}
				c.HasErrorRate = true
			}
			if m.HasPrice {
				c.CostPer1MTokens = m.PriceUSDPerMTok
				c.HasCost = true
			}
			if m.HasHealth {
				c.Health = m.Health
				c.HasHealth = true
			}
			viol, score := sla.Score(c, policy, maxCost)
			if best < 0 || sla.Better(viol, score, bestViol, bestScore, false) {
				best, bestViol, bestScore = i, viol, score
			}
		}
		if best < 0 {
			return pool[0].Model, "no SLA signal in pool; catalog order decides (compliance unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("most SLA-compliant in pool (violations %.2f, score %.2f)", bestViol, bestScore)
	case "offline-friendly":
		best := 0
		for i, m := range pool[1:] {
			if m.Headroom() > pool[best].Headroom() {
				best = i + 1
			}
		}
		return pool[best].Model, fmt.Sprintf("most quota headroom in pool (%.0f%%)", pool[best].Headroom()*100)
	case "reliability-first":
		best := -1
		for i, m := range pool {
			if !m.HasHealth {
				continue
			}
			if best < 0 || m.Health > pool[best].Health {
				best = i
			}
		}
		if best < 0 {
			return pool[0].Model, "no health signal in pool; catalog order decides (reliability unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("healthiest in pool (breaker health %.2f)", pool[best].Health)
	case "quality-first":
		best := -1
		for i, m := range pool {
			if !m.HasQuality {
				continue
			}
			if best < 0 || m.Quality > pool[best].Quality {
				best = i
			}
		}
		if best < 0 {
			return pool[0].Model, "no quality signal in pool; catalog order decides (quality unverifiable)"
		}
		return pool[best].Model, fmt.Sprintf("highest quality signal in pool (%.2f)", pool[best].Quality)
	case "code":
		// auto/code ranks by cost per solved task (code.go); with
		// no outcome evidence the documented neutral default is
		// price order, so cheapest-priced wins here as well.
		model, note := pickByStrategyWithPolicy("cost-saver", pool, policy, ranker, tier)
		return model, "cost-per-solved-task ranking lives in code.go; no outcome evidence here, so price order (documented neutral default): " + note
	default: // "balanced", "lkgp" (already resolved), anything else
		if first, ok := rankedFirst(ranker, tier, pool); ok {
			return first, "balanced 16-factor ranking (caller-supplied ranker, best first)"
		}
		return pool[0].Model, "balanced weights; no live 16-factor scorer input in this pure resolver — catalog order decides, engine re-ranks at dispatch"
	}
}

func modelsOf(pool []CatalogModel) []string {
	out := make([]string, 0, len(pool))
	for _, m := range pool {
		out = append(out, m.Model)
	}
	return out
}

// ---------------------------------------------------------------------------
// 4-tier fallback cascade (deep-study §1.1).
// ---------------------------------------------------------------------------

// CascadeTier is one rung of the 4-tier fallback cascade.
type CascadeTier string

// Cascade tiers in escalation order.
const (
	// CascadeSubscription is Tier 1: quota already paid for.
	CascadeSubscription CascadeTier = "subscription"
	// CascadeAPIKey is Tier 2: metered API-key connections.
	CascadeAPIKey CascadeTier = "api-key"
	// CascadeCheap is Tier 3: cheap-tier models.
	CascadeCheap CascadeTier = "cheap"
	// CascadeFree is Tier 4: free-tier models.
	CascadeFree CascadeTier = "free"
)

// CascadeOrder is the 4-tier fallback order: Subscription -> API
// Key -> Cheap -> Free. NOTE: this is the deep-study §1.1 wording
// (parity tracker Area 1 records its exact wording as "not re-verified
// fresh") and is NOT the 5-rung subscription ladder order
// (subscription, keyless, free, cheap, premium) that auto/thrifty
// uses. The cascade is a coarse operator-facing escalation; the
// ladder is the precise per-connection ordering. Do not merge them.
var CascadeOrder = []CascadeTier{
	CascadeSubscription,
	CascadeAPIKey,
	CascadeCheap,
	CascadeFree,
}

// CascadeStep is one tier of the cascade with its surviving
// candidates in catalog order.
type CascadeStep struct {
	// Tier is the cascade tier.
	Tier CascadeTier `json:"tier"`
	// Models are the tier's surviving candidate model ids, in
	// catalog order.
	Models []string `json:"models"`
	// Exhausted reports that the tier had members but every one
	// was gated out (quota at/below the exit cutoff). A tier
	// with no members at all is omitted from the cascade
	// instead of appearing exhausted.
	Exhausted bool `json:"exhausted,omitempty"`
}

// OrderCascade projects a catalog onto the 4-tier cascade.
// Membership (first match wins, no model in two tiers):
//   - subscription: connection billing "subscription"
//   - api-key: metered connection (any model tier)
//   - cheap: cheap-tier model (any other connection)
//   - free: free-tier model (any other connection)
//
// A model matching no tier (e.g. an uncurated premium model on an
// unknown connection) is not placed anywhere — it is not a
// fallback candidate this cascade can describe, and inventing a
// tier for it would be a lie. Quota-bearing members at/below the
// exit cutoff are gated out of their tier; a tier whose members
// are all gated out is returned with Exhausted=true and no models,
// so the caller can see the tier existed but could not serve —
// except auto/subscription, which must NOT use this cascade at
// all: it fails closed instead of stepping to the next tier.
func OrderCascade(catalog []CatalogModel) []CascadeStep {
	buckets := map[CascadeTier][]CatalogModel{}
	hadMembers := map[CascadeTier]bool{}
	for _, m := range catalog {
		tier, ok := cascadeTierOf(m)
		if !ok {
			continue
		}
		hadMembers[tier] = true
		if m.HasRemaining && m.RemainingPercent <= ExitCutoffPercent {
			continue
		}
		buckets[tier] = append(buckets[tier], m)
	}
	steps := []CascadeStep{}
	for _, tier := range CascadeOrder {
		if !hadMembers[tier] {
			continue
		}
		step := CascadeStep{Tier: tier, Models: modelsOf(buckets[tier])}
		if len(step.Models) == 0 {
			step.Exhausted = true
		}
		steps = append(steps, step)
	}
	return steps
}

// cascadeTierOf assigns a model to its first matching cascade
// tier, or false when it matches none.
func cascadeTierOf(m CatalogModel) (CascadeTier, bool) {
	if m.Billing == "subscription" {
		return CascadeSubscription, true
	}
	if m.Billing == "metered" {
		return CascadeAPIKey, true
	}
	switch m.Tier {
	case "cheap":
		return CascadeCheap, true
	case "free":
		return CascadeFree, true
	default:
		return "", false
	}
}

// CascadeTierFor is the ENGINE-side cascade classifier (G19 Area
// 1): the tier of one catalog model for applyCascadeOrder, where
// an attempt must never be dropped for having no tier. It starts
// from cascadeTierOf and adds exactly one rule — the
// subscription ladder's documented default (SUBSCRIPTION_LADDER.md:
// "Uncurated is not free"): a model on an unknown connection
// (Billing "") with an unclassified tier is consumed as metered,
// so it lands on the api-key rung, never on the free rung and
// never unplaced. A keyless connection is the one case with no
// cascade rung at all (the cascade has no keyless tier): it
// reports false, and the engine sorts it last without inventing
// a tier. OrderCascade keeps using cascadeTierOf (projection:
// unplaced models are omitted there, by design); the two
// classifiers differ deliberately, and only in this default.
func CascadeTierFor(m CatalogModel) (CascadeTier, bool) {
	if tier, ok := cascadeTierOf(m); ok {
		return tier, true
	}
	if m.Billing == "keyless" {
		return "", false
	}
	if m.Billing == "" {
		return CascadeAPIKey, true
	}
	return "", false
}

// ModelHasCapability reports whether model declares capability in
// the operator-declared per-model capability map (G19 Area 1):
// the same map the auto/code pool reads (config
// auto_router.code_capabilities). Keys are model ids as the
// operator wrote them — bare ("gpt-4o") or "provider/model" —
// so a bare key matches a prefixed lookup and vice versa.
// Comparison is case-insensitive on the capability name ("vision",
// "Vision" and "VISION" all match). A model absent from the map
// declares nothing: false, never a guessed capability.
func ModelHasCapability(caps map[string][]string, model, capability string) bool {
	if len(caps) == 0 || capability == "" {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(capability))
	check := func(list []string) bool {
		for _, c := range list {
			if strings.ToLower(strings.TrimSpace(c)) == want {
				return true
			}
		}
		return false
	}
	if check(caps[model]) {
		return true
	}
	// Bare-vs-prefixed key forms: "openai/gpt-4o" matches a
	// bare "gpt-4o" key, and a bare lookup matches a
	// "provider/model" key naming this model.
	if _, bare, ok := strings.Cut(model, "/"); ok {
		if check(caps[bare]) {
			return true
		}
	} else {
		for key, list := range caps {
			if _, b, ok := strings.Cut(key, "/"); ok && b == model {
				if check(list) {
					return true
				}
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Per-endpoint combo projection (endpointCapabilities.ts,
// comboEndpoints.ts).
// ---------------------------------------------------------------------------

// Endpoint is one dispatch surface a combo can be projected onto.
type Endpoint string

// Endpoints (COMBO_ENDPOINTS in comboEndpoints.ts).
const (
	EndpointChat                Endpoint = "chat"
	EndpointSearch              Endpoint = "search"
	EndpointWebFetch            Endpoint = "web-fetch"
	EndpointEmbeddings          Endpoint = "embeddings"
	EndpointRerank              Endpoint = "rerank"
	EndpointImages              Endpoint = "images"
	EndpointImageEdits          Endpoint = "image-edits"
	EndpointVideos              Endpoint = "videos"
	EndpointMusic               Endpoint = "music"
	EndpointOCR                 Endpoint = "ocr"
	EndpointAudioSpeech         Endpoint = "audio-speech"
	EndpointAudioTranscriptions Endpoint = "audio-transcriptions"
	EndpointAudioTranslations   Endpoint = "audio-translations"
	EndpointAudioGeneration     Endpoint = "audio-generation"
)

// AllEndpoints lists every dispatch surface in a stable order.
var AllEndpoints = []Endpoint{
	EndpointChat, EndpointSearch, EndpointWebFetch, EndpointEmbeddings,
	EndpointRerank, EndpointImages, EndpointImageEdits, EndpointVideos,
	EndpointMusic, EndpointOCR, EndpointAudioSpeech,
	EndpointAudioTranscriptions, EndpointAudioTranslations,
	EndpointAudioGeneration,
}

// IsEndpoint reports whether s names a known dispatch surface.
func IsEndpoint(s string) bool {
	for _, e := range AllEndpoints {
		if Endpoint(s) == e {
			return true
		}
	}
	return false
}

// NonChatStrategies are the only strategies a non-chat endpoint
// route may name (COMBO_NON_CHAT_STRATEGIES in comboEndpoints.ts;
// enforced by the combo schema's superRefine: any other strategy
// on a non-chat route is a validation error, not a silent
// substitution).
var NonChatStrategies = []string{
	"priority", "fill-first", "round-robin", "weighted",
	"random", "strict-random", "least-used", "p2c",
}

// AllComboStrategies is the full strategy vocabulary a chat route
// may name (the 22 strategies in AUTO-COMBO.md, plus the internal
// "quota-share" which is never a chat-route strategy either — it
// is listed here only so callers can recognise it as internal).
var AllComboStrategies = []string{
	"priority", "weighted", "round-robin", "context-relay",
	"fill-first", "p2c", "random", "least-used", "cost-optimized",
	"reset-aware", "reset-window", "expiry-first", "headroom",
	"quota-weighted", "strict-random", "auto", "lkgp",
	"context-optimized", "cache-optimized", "fusion", "pipeline",
	"jev",
}

// StrategiesForEndpoint returns the strategies that may be used on
// an endpoint: every strategy on chat, the non-chat set everywhere
// else. An unknown endpoint is an error — never silently treated
// as chat.
func StrategiesForEndpoint(endpoint Endpoint) ([]string, error) {
	if !IsEndpoint(string(endpoint)) {
		return nil, fmt.Errorf("unknown endpoint %q", endpoint)
	}
	if endpoint == EndpointChat {
		return append([]string(nil), AllComboStrategies...), nil
	}
	return append([]string(nil), NonChatStrategies...), nil
}

// ProjectStrategyForEndpoint validates a combo's strategy for an
// endpoint (comboForEndpoint + the schema's non-chat rule):
//   - chat: any strategy stands (comboForEndpoint returns the combo
//     unchanged — the chat plan is canonical).
//   - non-chat: the strategy must be in NonChatStrategies, else an
//     error naming it (the reference rejects the combo config;
//     this pure function does not silently substitute "priority").
//   - unknown endpoint or empty strategy: error.
//
// The route-override half of comboForEndpoint — a non-chat route
// replacing models/strategy/config — is the coordinator's job at
// registration: this function only answers "is this strategy
// allowed here".
func ProjectStrategyForEndpoint(strategy string, endpoint Endpoint) (string, error) {
	if strings.TrimSpace(strategy) == "" {
		return "", fmt.Errorf("empty strategy for endpoint %q", endpoint)
	}
	if !IsEndpoint(string(endpoint)) {
		return "", fmt.Errorf("unknown endpoint %q", endpoint)
	}
	if endpoint == EndpointChat {
		return strategy, nil
	}
	for _, s := range NonChatStrategies {
		if s == strategy {
			return strategy, nil
		}
	}
	return "", fmt.Errorf("strategy %q is not supported for endpoint %q (want one of %s)", strategy, endpoint, strings.Join(NonChatStrategies, ", "))
}
