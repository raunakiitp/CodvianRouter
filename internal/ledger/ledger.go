// Package ledger implements the unified credit/usage ledger for the
// Codvian Router. All usage is recorded locally in SQLite (pure-Go driver
// modernc.org/sqlite, no cgo), so the router can run fully offline.
//
// Schema:
//
//	usage_events(id, ts, gateway_key_id, provider, model,
//	             prompt_tokens, completion_tokens, total_tokens,
//	             cached_tokens, reasoning_tokens,
//	             cache_write_tokens, cache_write_tokens_5m,
//	             cache_write_tokens_1h, local_estimate,
//	             tokens_split_known, price_known,
//	             cost_usd, ok, latency_ms, shared)
//	budgets(key_id, cap_usd, spent_usd, period)
//	key_budgets(key_id, limit_usd, unpriced_policy, daily_limit_usd,
//	weekly_limit_usd, monthly_limit_usd, warning_threshold,
//	reset_interval, reset_time) — rolling 5h cap + calendar windows
//	5-hour per-key USD caps, see keybudget.go.
//	quota_pools(id, name, saturation_threshold) +
//	quota_pool_dimensions(pool_id, unit, window, limit_value) +
//	quota_pool_allocations(pool_id, key_id, weight, cap_value,
//	cap_unit, policy) — fair-share pools, see pool.go.
//
// shared marks a request served from a coalesced (single-flight)
// upstream call: 1 when the row is a waiter that shared the leader's
// call, 0 otherwise. A shared row always carries zero tokens and
// zero cost — the leader's row is the only one billed — so summing
// cost_usd or tokens over usage_events never double-counts, and
// filtering shared = 0 gives exactly the real upstream attempts.
//
// Cost is computed from caller-supplied per-(provider, model)
// prices: tokens * per-token price. Prices are never invented here
// — a pair with no supplied price records cost 0, explicitly, so
// an unknown price can never silently turn into a made-up charge.
// Likewise a usage split is never invented: when the caller does
// not know the prompt/completion split (Usage.SplitKnown false),
// the event records prompt_tokens = completion_tokens = 0, the
// reported total in total_tokens, tokens_split_known = 0, and cost
// 0 — a split is needed for honest pricing, so no cost is guessed
// from a bare total.
package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"

	_ "modernc.org/sqlite"
)

// PriceKey identifies one priced (provider, model) pair: the key
// every price in Prices is stored under. A PriceKey with an empty
// Provider is a MODEL-LEVEL price: it applies to every provider
// serving that model, unless a pair entry for the serving provider
// overrides it — it is a fallback, not a pair price. (It mirrors
// config.PriceKey; the ledger keeps its own so this leaf package
// does not import config.)
type PriceKey struct {
	Provider string
	Model    string
}

// ModelPrice is the per-token price of one model, in USD.
type ModelPrice struct {
	PromptPerToken     float64 // USD per prompt token
	CompletionPerToken float64 // USD per completion token
	// CachedPromptPerToken is the USD per prompt token served from
	// the provider's prompt cache. It applies only when CachedKnown
	// is true: a configured $0 (free cache reads) is a real price,
	// distinct from "no cached rate configured" (CachedKnown false),
	// which prices cached tokens at the full PromptPerToken rate —
	// the old behaviour, never an invented rate.
	CachedPromptPerToken float64
	CachedKnown          bool
	// CacheWrite5mPerToken / CacheWrite1hPerToken are the USD per
	// token written into the provider's prompt cache at the
	// 5-minute / 1-hour TTL tier (M6.9). Each applies only when
	// its Known flag is true: an explicit configured tier rate
	// always wins over the multiplier fallback in
	// CostWithCacheTiers. A configured $0 is a real price,
	// distinct from "no tier rate configured" (Known false).
	// Config fields ModelPrice.CacheWrite5mUSDPer1M /
	// CacheWrite1hUSDPer1M (internal/config/prices.go) carry
	// these rates; buildLedgerPrices in
	// cmd/codvian-router/main.go maps them onto these fields.
	// Until a rate is configured, the multiplier fallback below
	// is the tier pricing in effect.
	CacheWrite5mPerToken float64
	CacheWrite5mKnown    bool
	CacheWrite1hPerToken float64
	CacheWrite1hKnown    bool
	// EndpointPrices is the PER-ENDPOINT price dimension (G16,
	// Area 11 cost row): the same model costs differently per
	// API surface — chat vs embeddings vs responses vs audio —
	// because the upstream tariffs differ (e.g. OpenAI's
	// text-embedding models price per input token with no
	// completion leg at all). Keys are the endpoint strings the
	// ledger stores on usage rows ("" = chat completions,
	// "embeddings", "responses", "audio", "images", "rerank",
	// ... — provider.EndpointKind strings). A leg set (non-nil)
	// in the endpoint override replaces the base leg for that
	// endpoint; a nil leg falls back to the base leg, so an
	// override need only state the legs that actually differ.
	// An endpoint with no entry prices at the base legs. The
	// config side (config.ModelPrice.EndpointPrices,
	// internal/config/prices.go) carries the same dimension in
	// per-1M form; buildLedgerPrices in cmd/codvian-router/
	// main.go maps it onto this field.
	EndpointPrices map[string]EndpointPrice
}

// EndpointPrice is one endpoint's override legs for a ModelPrice.
// Every leg is a pointer so "not overridden" (nil = fall back to
// the base leg) stays distinct from "overridden as free" (0).
type EndpointPrice struct {
	PromptPerToken       *float64
	CompletionPerToken   *float64
	CachedPromptPerToken *float64
}

// priceForEndpoint returns the price in effect for one endpoint:
// the base price with any endpoint-override legs applied. An
// endpoint with no override entry, or an empty endpoint (""),
// prices at the base legs unchanged.
func (pr ModelPrice) priceForEndpoint(endpoint string) ModelPrice {
	if endpoint == "" || len(pr.EndpointPrices) == 0 {
		return pr
	}
	ep, ok := pr.EndpointPrices[endpoint]
	if !ok {
		return pr
	}
	out := pr
	if ep.PromptPerToken != nil {
		out.PromptPerToken = *ep.PromptPerToken
	}
	if ep.CompletionPerToken != nil {
		out.CompletionPerToken = *ep.CompletionPerToken
	}
	if ep.CachedPromptPerToken != nil {
		out.CachedPromptPerToken = *ep.CachedPromptPerToken
		out.CachedKnown = true
	}
	return out
}

// cacheWrite5mMultiplier / cacheWrite1hMultiplier are the
// Anthropic-documented cache-write price multiples of the base
// input price: 1.25x for the 5-minute TTL, 2x for the 1-hour TTL.
// Sources in-tree: internal/server/server.go:2092-2094
// ("Anthropic documents 1.25x (5-minute TTL) and 2x (1-hour TTL)
// for cache writes") and the the reference router reference estimator
// (read-only study, src/lib/usage/cacheWriteEstimate.ts:35,55-56:
// "5m = 1.25x input, 1h = 2x input"). They apply ONLY to
// Anthropic-served Claude models (see isAnthropicCacheWriteTier);
// every other provider's tier rate is unknown, so its tiers price
// at the plain input rate — never at a guessed multiple.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// isAnthropicCacheWriteTier reports whether the (provider, model)
// pair is billed at Anthropic's documented cache-write multiples.
// It mirrors the the reference router reference gate
// (cacheWriteEstimate.ts: the anthropic branch requires provider in
// {"anthropic", "claude", "cc"} AND model starting with "claude-"):
// Bedrock-served Claude models and OpenRouter-served Claude models
// are deliberately excluded — their write tariffs are their own
// (Bedrock's, OpenRouter's), not Anthropic's direct API tariff, so
// applying the Anthropic multiple there would be an invented rate.
func isAnthropicCacheWriteTier(provider, model string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude", "cc":
	default:
		return false
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
}

// IsAnthropicCacheWriteTier is the exported form of
// isAnthropicCacheWriteTier, for the server's dashboard economics
// (internal/server/server.go cacheComboEconomics), which prices the
// same tiers from config.ModelPrice and must apply the same
// provider/model gate — one source of truth for the classification,
// never a second copy of the rule.
func IsAnthropicCacheWriteTier(provider, model string) bool {
	return isAnthropicCacheWriteTier(provider, model)
}

// cacheWriteRate resolves the USD per-token rate for one cache-write
// tier: the explicit configured tier rate when one exists, else the
// Anthropic-documented multiple of the input price for
// Anthropic-served Claude models, else the plain input price (the
// tier rate is unknown for that provider — priced at 1x, never at
// a guessed multiple).
func (pr ModelPrice) cacheWriteRate(provider, model string, oneHour bool) float64 {
	if oneHour {
		if pr.CacheWrite1hKnown {
			return pr.CacheWrite1hPerToken
		}
		if isAnthropicCacheWriteTier(provider, model) {
			return pr.PromptPerToken * cacheWrite1hMultiplier
		}
		return pr.PromptPerToken
	}
	if pr.CacheWrite5mKnown {
		return pr.CacheWrite5mPerToken
	}
	if isAnthropicCacheWriteTier(provider, model) {
		return pr.PromptPerToken * cacheWrite5mMultiplier
	}
	return pr.PromptPerToken
}

// Prices maps (provider, model) -> per-token prices. A pair absent
// from the map falls back to the model-level entry (empty Provider)
// for that model; a model absent from the map at both levels has
// unknown pricing and costs 0 (see Cost).
type Prices map[PriceKey]ModelPrice

// Known reports whether a price is configured for the (provider,
// model) pair — the pair entry, or the model-level fallback. It is
// the "is this usage priced?" question: a known price may be
// genuinely $0 (the operator priced the model free); an unknown
// price is never $0, it is unknown.
func (p Prices) Known(provider, model string) bool {
	if _, ok := p[PriceKey{Provider: provider, Model: model}]; ok {
		return true
	}
	if provider != "" {
		if _, ok := p[PriceKey{Model: model}]; ok {
			return true
		}
	}
	return false
}

// KnownModel reports whether any price is configured for a model:
// a pair price at any provider, or the model-level fallback.
func (p Prices) KnownModel(model string) bool {
	if _, ok := p[PriceKey{Model: model}]; ok {
		return true
	}
	for k := range p {
		if k.Model == model {
			return true
		}
	}
	return false
}

// Cost returns the USD cost of a call, or 0 when the (provider,
// model) pair's price is unknown. The pair price wins; when the
// pair has no entry, the MODEL-LEVEL FALLBACK applies — the price
// keyed by the model alone, if one is configured. Unknown prices
// are recorded as 0, never guessed.
func (p Prices) Cost(provider, model string, promptTokens, completionTokens int) float64 {
	return p.CostWithCached(provider, model, promptTokens, completionTokens, 0)
}

// CostWithCached returns the USD cost of a call whose prompt split
// includes cachedTokens provider-cached prompt tokens (a subset of
// promptTokens, clamped — a malformed report can never claim more
// cached tokens than prompt tokens). Cached tokens price at the
// configured cached rate when one exists (CachedKnown); without a
// configured cached rate they price at the full prompt rate, never
// at an invented one. The pair price wins; when the pair has no
// entry, the MODEL-LEVEL FALLBACK applies. Unknown prices cost 0.
//
// Cache-write tokens are NOT priced here: this signature carries
// no tier split, so a caller that knows the 5m/1h split must use
// CostWithCacheTiers. CostWithCached is exactly CostWithCacheTiers
// with both write tiers 0 — the legacy path, unchanged.
func (p Prices) CostWithCached(provider, model string, promptTokens, completionTokens, cachedTokens int) float64 {
	return p.CostWithCacheTiers(provider, model, promptTokens, completionTokens, cachedTokens, 0, 0)
}

// CostWithCacheTiers returns the USD cost of a call whose prompt
// split includes cachedTokens provider-cached prompt tokens plus
// writeTokens5m / writeTokens1h tokens written into the provider's
// prompt cache at the 5-minute / 1-hour TTL tier (M6.9, Anthropic
// cache_creation split into ephemeral_5m / ephemeral_1h).
//
// The prompt is partitioned, never double-counted: the tiers are
// subsets of promptTokens, so the regular prompt leg is
// promptTokens - cachedTokens - writeTokens5m - writeTokens1h at
// the prompt rate, cached reads at the cached rate, each write
// tier at its tier rate (see cacheWriteRate), and completion at
// the completion rate. Each subset is clamped in turn (cached to
// the prompt, 5m to what is left, 1h to what is left after that),
// so a malformed report can never claim more tiered tokens than
// prompt tokens. The pair price wins; when the pair has no entry,
// the MODEL-LEVEL FALLBACK applies. Unknown prices cost 0.
func (p Prices) CostWithCacheTiers(provider, model string, promptTokens, completionTokens, cachedTokens, writeTokens5m, writeTokens1h int) float64 {
	return p.CostWithOptions(provider, model, CostOptions{}, promptTokens, completionTokens, cachedTokens, writeTokens5m, writeTokens1h)
}

// CostOptions carries the per-request dimensions CostWithOptions
// prices on: which API surface served the call, which service
// tier it ran at, and whether flat-rate providers zero out.
type CostOptions struct {
	// Endpoint is the API surface ("", "embeddings",
	// "responses", "audio", ...): the per-endpoint price
	// dimension selects the endpoint's override legs when the
	// price carries them, else the base legs. "" = chat.
	Endpoint string
	// ServiceTier is the request's service tier ("", "standard",
	// "flex", "batch", "priority", "auto"): flex and batch
	// price at 0.5x (see ServiceTierMultiplier); anything else
	// prices at 1x, never at a guessed multiplier.
	ServiceTier string
	// FlatRateAsZero, when true, prices a flat-rate
	// (subscription / coding-plan / cookie-web) provider at $0
	// instead of its per-token estimate (see IsFlatRateProvider):
	// the opt-in display/analytics treatment, matching
	// the reference router's flatRateAsZero option. Budget / quota paths
	// keep estimating (they pass false) — only callers that
	// opted in zero out.
	FlatRateAsZero bool
}

// NormalizeServiceTier lower-cases and trims a service-tier
// value; "" stays "" (standard).
func NormalizeServiceTier(tier string) string {
	return strings.ToLower(strings.TrimSpace(tier))
}

// ServiceTierMultiplier returns the cost multiplier for a service
// tier: flex 0.5x and batch 0.5x (OpenAI Flex Processing is billed
// at a 50% token discount, like Batch — the reference router
// costCalculator.ts getCodexFastCostMultiplier; the G6-verified
// batch figure is the same 50%), standard / priority / auto / ""
// and any unknown tier 1x. An unknown tier is never priced at a
// guessed multiplier.
//
// DIVERGENCE (deliberate, better than the reference router): the reference router's
// fast/priority multiplier (1.5x–2.5x) applies ONLY to Codex
// GPT-5.x/6 models on the codex/cx providers. We do not apply a
// provider+model-specific fast multiplier here: a generic tier
// multiplier would misprice every other provider's priority tier,
// and the Codex fast tariff belongs in a provider-specific price
// row, not in the shared tier rule.
func ServiceTierMultiplier(tier string) float64 {
	switch NormalizeServiceTier(tier) {
	case "flex", "batch":
		return 0.5
	default:
		return 1
	}
}

// CostWithOptions is the full cost computation: every leg, every
// dimension. It is CostWithCacheTiers plus (a) the per-endpoint
// price dimension (opts.Endpoint selects the endpoint's override
// legs), (b) the service-tier multiplier (flex / batch 0.5x), and
// (c) the flat-rate opt-in (flat-rate provider + FlatRateAsZero
// costs $0, not "unknown"). The cached-input leg and the
// cache-write tier legs were already here (G13-verified); this
// closes the remaining gaps in the Area 11 "Cost computation,
// all legs" row. Unknown prices cost 0, as everywhere.
func (p Prices) CostWithOptions(provider, model string, opts CostOptions, promptTokens, completionTokens, cachedTokens, writeTokens5m, writeTokens1h int) float64 {
	pr, ok := p[PriceKey{Provider: provider, Model: model}]
	if !ok && provider != "" {
		pr, ok = p[PriceKey{Model: model}]
	}
	if !ok {
		return 0
	}
	if opts.FlatRateAsZero && IsFlatRateProvider(provider) {
		return 0
	}
	pr = pr.priceForEndpoint(opts.Endpoint)
	if cachedTokens < 0 {
		cachedTokens = 0
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}
	if writeTokens5m < 0 {
		writeTokens5m = 0
	}
	if writeTokens5m > promptTokens-cachedTokens {
		writeTokens5m = promptTokens - cachedTokens
	}
	if writeTokens1h < 0 {
		writeTokens1h = 0
	}
	if writeTokens1h > promptTokens-cachedTokens-writeTokens5m {
		writeTokens1h = promptTokens - cachedTokens - writeTokens5m
	}
	cachedRate := pr.PromptPerToken
	if pr.CachedKnown {
		cachedRate = pr.CachedPromptPerToken
	}
	cost := float64(promptTokens-cachedTokens-writeTokens5m-writeTokens1h)*pr.PromptPerToken +
		float64(cachedTokens)*cachedRate +
		float64(writeTokens5m)*pr.cacheWriteRate(provider, model, false) +
		float64(writeTokens1h)*pr.cacheWriteRate(provider, model, true) +
		float64(completionTokens)*pr.CompletionPerToken
	return cost * ServiceTierMultiplier(opts.ServiceTier)
}

// CostForModel returns the USD cost of a call priced by the
// MODEL-LEVEL FALLBACK alone (no provider): the price keyed by the
// model, or 0 when the model has no model-level price. Callers
// that know the serving provider must use Cost with it, so a pair
// price is never silently replaced by the fallback.
func (p Prices) CostForModel(model string, promptTokens, completionTokens int) float64 {
	return p.Cost("", model, promptTokens, completionTokens)
}

// Usage is one recorded request against a gateway key.
//
// SplitKnown reports whether PromptTokens / CompletionTokens are the
// real split reported by the upstream. When false, the split is
// unknown: PromptTokens and CompletionTokens must be ignored (they
// are stored as 0) and only TotalTokens is meaningful — and the cost
// is 0, because a split is needed for honest pricing. Callers must
// never set SplitKnown true with a guessed split.
//
// CachedTokens is the provider-reported count of prompt tokens
// served from the provider's prompt cache (OpenAI
// prompt_tokens_details.cached_tokens, Anthropic
// cache_read_input_tokens, Gemini cachedContentTokenCount). It is a
// subset of PromptTokens, stored only alongside a known split (0
// otherwise, and 0 on shared rows) — never a guessed number.
//
// Shared reports whether the request was served from a coalesced
// (single-flight) upstream call instead of making its own: the
// leader's row carries the real tokens and cost, and a shared row
// always records zero tokens and zero cost, so it can never inflate
// spend or token totals. It exists so a served-but-not-billed
// request is still visible in the ledger, distinguishable from a
// real upstream attempt.
type Usage struct {
	GatewayKeyID string
	// ProviderKeyID is the provider-side API key that served the
	// attempt (CHECKLIST 175, multi-key pooling): a "key_…" ID
	// for one key of a pool, "" for the provider's legacy single
	// key (or a row recorded before key pooling existed). Spend
	// is attributed per key, never pooled into one number.
	ProviderKeyID string
	Provider      string
	Model         string
	// Endpoint names the API surface the attempt was made to
	// ("" = chat completions, "embeddings", "rerank", ... — the
	// provider.EndpointKind string). It is stored on the usage
	// row so the dashboard can label the call's kind honestly
	// instead of guessing from the model name.
	Endpoint         string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	// ReasoningTokens is the provider-reported count of completion
	// tokens spent on internal reasoning (OpenAI
	// completion_tokens_details.reasoning_tokens, Gemini
	// thoughtsTokenCount) — a subset of CompletionTokens, stored
	// only alongside a known split. Providers that do not report
	// a reasoning split leave it 0: unknown, never guessed.
	ReasoningTokens int
	// CacheWriteTokens is the provider-reported count of input
	// tokens written into the provider's prompt cache on this
	// call (Anthropic cache_creation_input_tokens) — the
	// cache-write side of prompt-cache economics, stored only
	// alongside a known split. 0 means not reported.
	//
	// CacheWriteTokens5m / CacheWriteTokens1h (M6.9) split that
	// total by TTL tier: tokens written at the 5-minute /
	// 1-hour ephemeral tier (Anthropic usage.cache_creation
	// ephemeral_5m_input_tokens / ephemeral_1h_input_tokens).
	// When either tier is set, CacheWriteTokens is their sum —
	// the tiers win over a separately supplied total, which is
	// then ignored. When both tiers are 0, CacheWriteTokens is
	// the legacy unsplit total: the tier is unknown (the
	// upstream reported only cache_creation_input_tokens), and
	// the row prices the writes at the plain input rate — the
	// honest fallback, never a guessed tier. 0 / 0 with a 0
	// total means not reported at all.
	CacheWriteTokens   int
	CacheWriteTokens5m int
	CacheWriteTokens1h int
	// LocalEstimate marks a row whose prompt count is a LOCAL
	// tiktoken estimate (internal/tokencount), not a
	// provider-reported count: the upstream reported no usage
	// at all. Such a row stores the estimate in prompt_tokens
	// (and total_tokens), with SplitKnown false and cost 0 —
	// the split is genuinely unknown (no completion count) and
	// no cost can be priced from a bare prompt estimate. The
	// flag is what keeps an estimate from ever being read as a
	// reported split.
	LocalEstimate bool
	// LocalEstimatePromptTokens is the local tiktoken estimate
	// of the prompt AS SENT for this row, recorded alongside
	// the provider-reported split whenever the router computed
	// one (CHECKLIST 229) — the estimate-vs-reported drift
	// column. 0 means it was never measured for this row
	// (counting failed, or the row was recorded before this
	// field existed, or it is a shared/failed row). On a
	// local-estimate row it equals PromptTokens. It is never
	// used for pricing: cost always comes from the reported
	// split, or is 0 when there is none.
	LocalEstimatePromptTokens int
	// PromptBeforeCompression / CompressionSavedTokens are the
	// lossless-first compression record for the row (CHECKLIST
	// 183): the local tiktoken estimate of the prompt BEFORE
	// compression, and how many of those tokens compression
	// saved. PromptTokens on the row is the prompt AS SENT (the
	// compressed prompt, provider-reported or estimated).
	// Both are local estimates, never provider-reported; 0 / 0
	// means "no compression applied to this request". They ride
	// on the successful attempt's row only — a failed attempt
	// billed nothing and a shared row is zeroed like every
	// other token field — so window totals count each
	// request's saving once.
	PromptBeforeCompression int
	CompressionSavedTokens  int
	// CacheTokensSaved is the tokens this request saved by being
	// served from the router's own exact/semantic cache instead
	// of an upstream call: the prompt (and, for an exact hit,
	// completion) tokens that would have been billed on a full
	// call, as measured by the caller that served the hit
	// (router/server lane sets it from the cache hit; the ledger
	// stores it verbatim, never derives it — it cannot know what
	// the upstream would have billed). 0 means no cache saving:
	// not a cache hit, or the saving was not measured. It rides
	// on the successful attempt's row only, like the compression
	// record — a failed attempt saved nothing and a shared row is
	// zeroed like every other token field — so window totals
	// count each request's saving once.
	CacheTokensSaved int
	// NoCharge marks a row whose request made no upstream call at
	// all (B4-51: a proactive own-cache hit): the cost is $0 by
	// construction — no provider was billed — never a computed or
	// reported cost. The split stays known (the avoided call's
	// measured usage rides along), so the row still says how big
	// the avoided call was. Only the caller that served the hit
	// may set it; the ledger never derives it.
	NoCharge   bool
	SplitKnown bool
	OK         bool
	LatencyMS  int64
	Shared     bool
	// Cost is the EXACT cost the upstream reported for this
	// attempt, in USD — when the upstream reports one at all.
	// Only populate from a real reported value, never invent
	// one: today xAI reports cost_in_usd_ticks (1e10 ticks per
	// USD, per the xAI cost-tracking docs) and OpenRouter
	// reports usage.cost (bare USD) — both decoded in
	// provider.usageWire (internal/provider/client.go). Nil
	// means "the upstream reported no cost": the ledger falls
	// back to the hand-computed cost. A present value wins over
	// the computed cost (see normalizeUsage): it is the
	// provider's own billing number, and the reconciliation
	// delta (reported − computed) is recorded on the row.
	// A negative / NaN / infinite reported value is ignored
	// (treated as absent) — a nonsense billing number must
	// never become the row's cost.
	Cost *float64
	// ServiceTier is the request's service tier ("", "standard",
	// "flex", "batch", ...): flex and batch price at 0.5x (see
	// ServiceTierMultiplier). It is set from the request
	// (provider.ChatRequest.ServiceTier, threaded through
	// router.Attempt by the coordinator — see the G16 report);
	// "" means standard. The tier multiplier applies to the
	// hand-computed cost only, never to a provider-reported
	// exact cost (the reference router returns the exact cost before any
	// multiplier too).
	ServiceTier string
	// AutoVariant is the auto-routing variant that served this
	// attempt ("auto", "auto/thrifty", "auto/best-code", ...):
	// the virtual model the client asked for, as resolved by
	// the auto dispatcher. "" means the request was NOT
	// auto-routed (an explicit model) or the variant was never
	// recorded — the router sets it from the auto dispatch
	// (producer wiring is a follow-up; the ledger only stores
	// what the caller measured). It is what the
	// /api/analytics/auto-routing breakdown groups by.
	AutoVariant string
	// Cancelled marks an attempt that was started and then
	// cancelled before it produced a result — today, a hedge
	// loser (the other hedge attempt won). A cancelled row is
	// NOT a failed attempt: it is stored (the attempt really
	// happened, with its latency) but excluded from failure
	// counts (Summary.Errors, Buckets, LogFailed). The router
	// marks it with Usage.AsCancelled (ledger layer); the
	// router call-site change is a one-liner the coordinator
	// applies (see the G16 report).
	Cancelled bool
	// SessionID / ProjectID / ClientID are the attribution
	// dimensions for the attempt (G16, Area 11 session /
	// project / client row): the conversation session, the
	// operator's project, and the client identity the request
	// arrived under. All three are operator/client metadata,
	// never content; "" means unattributed. They are set from
	// request metadata with Usage.WithAttribution (ledger
	// layer); the router call-site wiring is a coordinator
	// one-liner (see the G16 report). Rows round-trip them and
	// filter by them (RecentEventsByAttribution).
	SessionID string
	ProjectID string
	ClientID  string
	// GitBranch is the git branch the attempt ran under, when
	// the request carried one (AK-3 agent-session rollup): the
	// rollup records it on the session row. "" means the
	// request carried no branch — no producer sets this yet
	// (no request field carries a branch today), so it stays
	// empty until a slice wires a carrier through.
	GitBranch string
}

// CostDeltaUSD returns the reconciliation delta for this usage:
// reported cost minus hand-computed cost, when a reported cost is
// present. (0, false) when no cost was reported — there is nothing
// to reconcile against. Callers that need the computed cost pass
// it in; the ledger records the same delta on the row (see
// normalizedRow.costDelta).
func (u Usage) CostDeltaUSD(computed float64) (float64, bool) {
	if u.Cost == nil || math.IsNaN(*u.Cost) || math.IsInf(*u.Cost, 0) || *u.Cost < 0 {
		return 0, false
	}
	return *u.Cost - computed, true
}

// AsCancelled returns a copy of u marked as a cancelled attempt
// (a hedge loser: started, then cancelled because the other hedge
// attempt won). It is the ledger-layer flag the router sets on the
// usage it records for a hedge loser, so the row is stored but
// never counted as a failure. OK is forced false: a cancelled
// attempt did not succeed — its cancelled flag, not its ok flag,
// is what keeps it out of the failure counts.
func (u Usage) AsCancelled() Usage {
	u.Cancelled = true
	u.OK = false
	return u
}

// WithAttribution returns a copy of u carrying the session /
// project / client attribution for the attempt. Empty values stay
// empty (unattributed). It is the ledger-layer setter the router
// calls with the request's metadata before recording.
func (u Usage) WithAttribution(sessionID, projectID, clientID string) Usage {
	u.SessionID = sessionID
	u.ProjectID = projectID
	u.ClientID = clientID
	return u
}

// Ledger is the usage/budget store backed by a local SQLite file.
type Ledger struct {
	db   *sql.DB
	path string

	// importMu serialises ImportEvents calls in-process (see
	// import.go): the dedup check and the insert must not
	// interleave with another import.
	importMu sync.Mutex

	// reserveMu serialises the check+insert in ReserveSpend and
	// the reservation read in CheckKeyBudget (M5.8): the
	// committed-spend gate must not interleave with another
	// reserve's check, or two parallel requests could both pass
	// on the same headroom.
	reserveMu sync.Mutex

	// stmtMu guards the lazily-prepared hot-path statements
	// below. They are prepared once (CPU profile, 2026-11-18
	// bench, CHECKLIST 249-250: _sqlite3Prepare cumulated
	// 3.49 s — every QueryRowContext re-parsed its SQL — and
	// GetBudget alone cumulated 4.07 s, called twice per
	// request by CheckBudgetOrError). *sql.Stmt is safe for
	// concurrent use, and the ledger runs on a single
	// connection (SetMaxOpenConns(1) in Open), so one prepared
	// statement serves every goroutine. A prepare failure
	// leaves the field nil and the caller falls back to the
	// direct query — a cold-cache slowdown, never an error.
	// Close closes any prepared statement.
	stmtMu           sync.Mutex
	getBudgetStmt    *sql.Stmt
	getKeyBudgetStmt *sql.Stmt

	// FailClosed enables "never bill me" mode: once a key's budget is
	// exhausted (spent >= cap, including a cap of 0), CheckBudget
	// refuses the request. When false, an exhausted budget is only a
	// warning signal — requests are still allowed. See budget.go.
	FailClosed bool

	// FlatRateAsZero is the flat-rate opt-in (G16, Area 11): when
	// true, a flat-rate (subscription / coding-plan / cookie-web)
	// provider's usage costs $0 in the ledger instead of its
	// per-token estimate — the provider bills a flat fee, not per
	// token, so the estimate would overstate the bill. It mirrors
	// the reference router's flatRateAsZero cost option. The operator (or
	// main.go, from config) sets it after Open; RecordUsage reads
	// it per call. Default false: without the opt-in a flat-rate
	// provider prices like any other provider ("unknown" when
	// unpriced, never silently $0).
	FlatRateAsZero bool

	// UsageRetentionDays is the usage_events retention window in
	// days (G16, Area 11 retention): rows older than this are
	// pruned by PruneExpiredUsage and at startup by Open. 0 means
	// the default (DefaultUsageRetentionDays, 7 days, matching
	// the reference router's 7-day fallback); negative also means the
	// default. The operator (or main.go, from config) sets it
	// after Open; call PruneExpiredUsage again after setting it
	// to apply a configured window to rows Open already pruned
	// at the default.
	UsageRetentionDays int
}

const schema = `
CREATE TABLE IF NOT EXISTS usage_events (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  ts                INTEGER NOT NULL,  -- unix milliseconds (UTC)
  gateway_key_id    TEXT NOT NULL,
  -- provider_key_id: the provider-side API key that served the
  -- attempt (CHECKLIST 175) — a "key_…" pool key ID, or "" for the
  -- provider's legacy single key / rows recorded before pooling.
  -- Spend is attributed per key, never pooled into one number.
  provider_key_id   TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL,
  model             TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  -- cache_write_tokens_5m / cache_write_tokens_1h (M6.9): the
  -- cache-write split by TTL tier (Anthropic ephemeral_5m /
  -- ephemeral_1h). 0 / 0 on a row whose cache_write_tokens is
  -- positive means the tier split was never reported — the
  -- upstream gave only the unsplit total — never "all 5m" or
  -- "all 1h" by assumption.
  cache_write_tokens_5m INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens_1h INTEGER NOT NULL DEFAULT 0,
  prompt_before_compression_tokens INTEGER NOT NULL DEFAULT 0,
  compression_saved_tokens INTEGER NOT NULL DEFAULT 0,
  -- cache_tokens_saved (B4-16, Area 11 cost-saved row): the tokens
  -- this request saved by being served from the router's own
  -- exact/semantic cache instead of an upstream call, as measured
  -- by the caller that served the hit. 0 means no cache saving.
  cache_tokens_saved INTEGER NOT NULL DEFAULT 0,
  local_estimate    INTEGER NOT NULL DEFAULT 0,
  -- local_estimate_prompt_tokens (CHECKLIST 229): the local
  -- tiktoken estimate of the prompt AS SENT, for the same request,
  -- recorded alongside the provider-reported split whenever the
  -- router computed one — 0 means it was never measured for this
  -- row (counting failed, or the row predates this column, or it
  -- is a shared/failed row). It is what makes estimate-vs-reported
  -- drift computable from live ledger data: a row with a known
  -- split AND a positive estimate here is one compared request.
  -- On a local-estimate row (upstream reported no usage) it equals
  -- prompt_tokens, which already holds the estimate.
  local_estimate_prompt_tokens INTEGER NOT NULL DEFAULT 0,
  tokens_split_known INTEGER NOT NULL DEFAULT 1,
  -- endpoint names the API surface the attempt was made to
  -- (CHECKLIST 171-172): "" for chat completions (including every
  -- row recorded before this column existed — chat was the only
  -- surface then), "embeddings" or "rerank" for those calls. It
  -- is what lets the dashboard tell an embeddings call from a
  -- chat call with the same model, instead of guessing from the
  -- model name.
  endpoint          TEXT NOT NULL DEFAULT '',
  -- price_known: 1 when the (provider, model) pair had a configured
  -- price at record time (including a genuinely-free $0 price), 0
  -- when the price was unknown. It is what separates "cost $0
  -- because the model is free" from "cost $0 because the price is
  -- unknown" — the rolling per-key budget counts only priced spend
  -- and reports the rest as unknown, never as $0 (see keybudget.go).
  price_known       INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  ok                INTEGER NOT NULL DEFAULT 1,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  shared            INTEGER NOT NULL DEFAULT 0,
  -- team_id (M5.12): the cost center the gateway key belonged to
  -- when this row was spent, resolved inside RecordUsage from
  -- the key's active binding. '' means the key belonged to no
  -- team. It is what lets a team's budget count exactly the
  -- usage its members spent while members, even after a key is
  -- reassigned or unassigned.
  team_id           TEXT NOT NULL DEFAULT '',
  -- reported_cost_usd / reported_cost_known (G16, Area 11
  -- gateway-reported cost): the EXACT cost the upstream reported
  -- for this attempt (xAI cost_in_usd_ticks, OpenRouter
  -- usage.cost), 0 / 0 when the upstream reported none. When
  -- known, cost_usd IS the reported cost (it wins over the
  -- hand-computed cost) and cost_delta_usd is reported minus
  -- computed — the reconciliation delta.
  reported_cost_usd   REAL NOT NULL DEFAULT 0,
  reported_cost_known INTEGER NOT NULL DEFAULT 0,
  cost_delta_usd      REAL NOT NULL DEFAULT 0,
  -- service_tier (G16, Area 11 batch/flex): the request's
  -- service tier ("", "standard", "flex", "batch", ...). flex
  -- and batch price at 0.5x; "" is standard. The tier
  -- multiplier applies to the hand-computed cost only, never
  -- to a reported exact cost.
  service_tier      TEXT NOT NULL DEFAULT '',
  -- cancelled (G16, Area 11 refused/fallback billing): 1 when
  -- the attempt was started then cancelled — a hedge loser.
  -- A cancelled row is stored but never counted as a failure
  -- (Summary.Errors, Buckets and LogFailed all exclude it).
  cancelled         INTEGER NOT NULL DEFAULT 0,
  -- session_id / project_id / client_id (G16, Area 11 session /
  -- project / client attribution): the conversation session,
  -- operator project and client identity the attempt belongs
  -- to. '' means unattributed. Metadata only, never content.
  session_id        TEXT NOT NULL DEFAULT '',
  project_id        TEXT NOT NULL DEFAULT '',
  client_id         TEXT NOT NULL DEFAULT '',
  -- auto_variant (Stage 1, Area 11 analytics endpoints): the
  -- auto-routing variant that served this attempt ("auto",
  -- "auto/thrifty", "auto/best-code", ...). "" means the request
  -- was not auto-routed, or the variant was never recorded.
  -- It is what the /api/analytics/auto-routing breakdown
  -- groups by.
  auto_variant      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_usage_key_ts
  ON usage_events(gateway_key_id, ts);

-- Teams (M5.12): cost centers — named groupings of gateway keys
-- with an optional shared rolling 1d/7d/30d USD budget.
CREATE TABLE IF NOT EXISTS teams (
  id                 TEXT PRIMARY KEY,
  name               TEXT NOT NULL,
  description        TEXT NOT NULL DEFAULT '',
  status             TEXT NOT NULL DEFAULT 'active',
  max_budget_usd     REAL,
  budget_duration    TEXT,
  budget_reset_at_ms INTEGER,
  created_at_ms      INTEGER NOT NULL,
  updated_at_ms      INTEGER NOT NULL,
  archived_at_ms     INTEGER
);
-- Key↔team membership history (M5.12): one row per assignment.
-- valid_to_ms NULL means the binding is active; a key has at most
-- one active binding (enforced by the partial unique index).
CREATE TABLE IF NOT EXISTS key_team_bindings (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  gateway_key_id   TEXT NOT NULL,
  team_id          TEXT NOT NULL,
  valid_from_ms    INTEGER NOT NULL,
  valid_to_ms      INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_key_team_active
  ON key_team_bindings(gateway_key_id) WHERE valid_to_ms IS NULL;
CREATE INDEX IF NOT EXISTS idx_key_team_history
  ON key_team_bindings(gateway_key_id, valid_from_ms);

-- Learned upstream rate limits (M5.13): one row per
-- (provider, provider key, kind) holding the latest limit /
-- remaining / reset the upstream reported. The router's startup
-- load paces the adaptive limiter from the fresh rows.
CREATE TABLE IF NOT EXISTS learned_rate_limits (
  provider_id     TEXT NOT NULL,
  provider_key_id TEXT NOT NULL DEFAULT '',
  kind            TEXT NOT NULL,
  limit_value     REAL NOT NULL,
  remaining       REAL NOT NULL,
  reset_at_ms     INTEGER,
  updated_ms      INTEGER NOT NULL,
  PRIMARY KEY (provider_id, provider_key_id, kind)
);

CREATE TABLE IF NOT EXISTS budgets (
  key_id   TEXT PRIMARY KEY,
  cap_usd  REAL NOT NULL,
  spent_usd REAL NOT NULL DEFAULT 0,
  period   TEXT NOT NULL DEFAULT ''
);

-- key_budgets: one rolling 5-hour USD cap per gateway key, plus
-- its unpriced-usage policy (fail_closed | count_as_zero). See
-- keybudget.go. Rows live in the ledger database, so budgets
-- survive a restart exactly like usage history.
-- M5.1: calendar-window caps (daily / weekly / monthly, NULL =
-- unset) + the active-window selector (reset_interval +
-- reset_time, the reference router costRules.ts) + warning_threshold.
CREATE TABLE IF NOT EXISTS key_budgets (
  key_id         TEXT PRIMARY KEY,
  limit_usd      REAL NOT NULL,
  unpriced_policy TEXT NOT NULL DEFAULT 'fail_closed',
  daily_limit_usd REAL,
  weekly_limit_usd REAL,
  monthly_limit_usd REAL,
  warning_threshold REAL NOT NULL DEFAULT 0.8,
  reset_interval TEXT NOT NULL DEFAULT 'daily',
  reset_time TEXT NOT NULL DEFAULT '00:00'
);

-- spend_reservations: committed (reserved, unsettled) spend
-- (M5.8). One row per in-flight request: the estimated cost
-- committed by ReserveSpend before sending, deleted by
-- SettleReservation once the actual usage row has been recorded.
-- The gate counts committed + settled, so parallel requests
-- cannot all pass on the same headroom. expires_ms bounds a
-- row's life (SpendReservationTTL); expired rows are pruned
-- lazily on read and never counted.
CREATE TABLE IF NOT EXISTS spend_reservations (
  id                  TEXT PRIMARY KEY,
  gateway_key_id      TEXT NOT NULL,
  estimated_cost_usd  REAL NOT NULL,
  created_ms          INTEGER NOT NULL,
  expires_ms          INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_spend_reservations_key ON spend_reservations(gateway_key_id);

-- quota_schedules: time-aware quota windows on a pool (M5.9,
-- the reference router lib/quota/schedules.ts). days is a comma list of JS
-- getDay() values (0 = Sunday); start/end_minute are minutes
-- from local midnight (end <= start wraps past midnight); mode
-- is allow (whitelist) or block (blackout); budget_value/unit/
-- window cap the pool's own consumption while the window is open.
CREATE TABLE IF NOT EXISTS quota_schedules (
  id            TEXT PRIMARY KEY,
  pool_id       TEXT NOT NULL REFERENCES quota_pools(id),
  label         TEXT NOT NULL DEFAULT '',
  days          TEXT NOT NULL DEFAULT '',
  start_minute  INTEGER NOT NULL DEFAULT 0,
  end_minute    INTEGER NOT NULL DEFAULT 1440,
  timezone      TEXT NOT NULL DEFAULT '',
  mode          TEXT NOT NULL DEFAULT 'allow',
  budget_value  REAL,
  budget_unit   TEXT NOT NULL DEFAULT '',
  budget_window TEXT NOT NULL DEFAULT '',
  priority      INTEGER NOT NULL DEFAULT 0,
  enabled       INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_quota_schedules_pool ON quota_schedules(pool_id);
CREATE TABLE IF NOT EXISTS quota_schedule_reserves (
  schedule_id TEXT NOT NULL REFERENCES quota_schedules(id) ON DELETE CASCADE,
  window      TEXT NOT NULL,
  percent     REAL NOT NULL
);

-- internal_budgets: operator caps on total spend (M5.11,
-- the reference router lib/usage/budgetGuard.ts). scope is global /
-- provider / model / pool (the matching *_id names the entity);
-- period is a calendar window (daily / weekly / monthly);
-- limit_type is currency (USD) / tokens / requests.
CREATE TABLE IF NOT EXISTS internal_budgets (
  id                TEXT PRIMARY KEY,
  scope             TEXT NOT NULL DEFAULT 'global',
  provider_id       TEXT NOT NULL DEFAULT '',
  model_id          TEXT NOT NULL DEFAULT '',
  pool_id           TEXT NOT NULL DEFAULT '',
  period            TEXT NOT NULL DEFAULT 'daily',
  limit_type        TEXT NOT NULL DEFAULT 'currency',
  limit_value       REAL NOT NULL DEFAULT 0,
  warning_threshold REAL NOT NULL DEFAULT 0.8,
  enabled           INTEGER NOT NULL DEFAULT 1
);

-- quota_pools: fair-share quota pools (M5.4, the reference router
-- lib/quota/fairShare.ts + dimensions.ts). A pool shares one quota
-- across its member keys: each dimension (unit x window) has a
-- global pool limit, each member key has a weight (0-100, its
-- share of the pool), an optional absolute cap, and a policy
-- (hard | soft | burst). The ledger IS the counter — consumption
-- is summed from usage_events, so no counter table exists to
-- drift (the reference router keeps separate quota_consumption buckets with
-- a sliding-window decay; ours sums the window directly, which
-- is the same "ledger is the counter" rule as the USD budgets
-- and token limits). See pool.go.
CREATE TABLE IF NOT EXISTS quota_pools (
  id                   TEXT PRIMARY KEY,
  name                 TEXT NOT NULL DEFAULT '',
  saturation_threshold REAL NOT NULL DEFAULT 0.5,
  account_count        INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS quota_pool_dimensions (
  pool_id     TEXT NOT NULL,
  unit        TEXT NOT NULL,
  window      TEXT NOT NULL,
  limit_value REAL NOT NULL,
  PRIMARY KEY (pool_id, unit, window)
);
CREATE TABLE IF NOT EXISTS quota_pool_allocations (
  pool_id   TEXT NOT NULL,
  key_id    TEXT NOT NULL,
  weight    REAL NOT NULL DEFAULT 0,
  cap_value REAL,
  cap_unit  TEXT,
  policy    TEXT NOT NULL DEFAULT 'hard',
  PRIMARY KEY (pool_id, key_id)
);
CREATE INDEX IF NOT EXISTS idx_quota_pool_allocations_key
  ON quota_pool_allocations(key_id);

-- response_tree: the durable Responses conversation-tree index
-- (CHECKLIST 237-238) — one row per served POST /v1/responses
-- create, linking each response to its parent (previous_response_id).
-- Metadata only (ids, model, provider, status, created_at, stream):
-- never input or output text. The table is created by this schema
-- on every Open (IF NOT EXISTS), so a database created by an older
-- build gains it empty on first open — the migration for old DBs,
-- with no backfill for rows recorded before this slice. See
-- response_tree.go.
CREATE TABLE IF NOT EXISTS response_tree (
  id         TEXT PRIMARY KEY,
  parent_id  TEXT NOT NULL DEFAULT '',
  model      TEXT NOT NULL DEFAULT '',
  provider   TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  stream     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_response_tree_parent
  ON response_tree(parent_id);

-- agent_sessions (AK-3, GAP-TRACK §14): the coding-agent
-- session rollup — one row per gateway key + client session id,
-- or per key + project inside a 30-minute idle window when no
-- client session id was carried (see agent_sessions.go). The
-- rollup is upserted in the same transaction as the usage row
-- (RecordUsage / AsyncWriter.commitBatch), so usage and session
-- counters move together. The table is created by this schema
-- on every Open (IF NOT EXISTS), so a database created by an
-- older build gains it empty on first open — no backfill for
-- rows recorded before this slice: history before the rollup
-- existed is honestly absent, never reconstructed.
CREATE TABLE IF NOT EXISTS agent_sessions (
  id                TEXT PRIMARY KEY,
  gateway_key_id    TEXT NOT NULL,
  client_session_id TEXT NOT NULL DEFAULT '',
  client_id         TEXT NOT NULL DEFAULT '',
  project_id        TEXT NOT NULL DEFAULT '',
  git_branch        TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL DEFAULT '',
  requests          INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  input_tokens      INTEGER NOT NULL DEFAULT 0,
  output_tokens     INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0,
  unpriced_count    INTEGER NOT NULL DEFAULT 0,
  created_at_ms     INTEGER NOT NULL,
  last_seen_ms      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_key_lastseen
  ON agent_sessions(gateway_key_id, last_seen_ms);
CREATE INDEX IF NOT EXISTS idx_agent_sessions_key_client
  ON agent_sessions(gateway_key_id, client_session_id);
`

// Open opens (creating if needed) the SQLite ledger at path, applies
// the schema, and returns the Ledger. Parent directories are created.
func Open(path string) (*Ledger, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("ledger: create directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("ledger: open sqlite: %w", err)
	}
	// Single writer connection: SQLite (even in WAL mode) serialises
	// writers, and one connection avoids "database is locked" churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ledger: apply schema: %w", err)
	}
	// Migrate databases created before total_tokens /
	// tokens_split_known existed: fresh databases already have the
	// columns from the schema above, so this is a no-op for them.
	if err := ensureUsageColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	// Migrate databases created before the M5.1 calendar-window
	// budget columns existed (same pattern, same idempotency).
	if err := ensureKeyBudgetColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	// Migrate databases created before the M5.5 per-account
	// pool column existed (same pattern, same idempotency).
	if err := ensureQuotaPoolColumns(db); err != nil {
		db.Close()
		return nil, err
	}
	l := &Ledger{db: db, path: path}
	// Startup cleanup (G16, Area 11 retention): prune usage rows
	// older than the default retention window, so the table
	// cannot grow unbounded across restarts. Best-effort: a
	// prune failure must never fail startup (the rows are still
	// pruned on the next open, or by an explicit
	// PruneExpiredUsage call once the operator's configured
	// window is set). A fresh database has no rows, so this is
	// a no-op there.
	_, _ = l.PruneExpiredUsage(context.Background(), time.Now())
	return l, nil
}

// ensureUsageColumns adds total_tokens and tokens_split_known to
// usage_events when they are missing (databases created by an older
// build). It is idempotent: each column is added only after a PRAGMA
// table_info check shows it absent, so opening an already-migrated
// database changes nothing.
func ensureUsageColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(usage_events)`)
	if err != nil {
		return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: inspect usage_events schema: %w", err)
	}
	rows.Close()
	add := map[string]string{
		"total_tokens":       `ALTER TABLE usage_events ADD COLUMN total_tokens INTEGER NOT NULL DEFAULT 0`,
		"cached_tokens":      `ALTER TABLE usage_events ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`,
		"reasoning_tokens":   `ALTER TABLE usage_events ADD COLUMN reasoning_tokens INTEGER NOT NULL DEFAULT 0`,
		"cache_write_tokens": `ALTER TABLE usage_events ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0`,
		// cache_write_tokens_5m / _1h (M6.9): default 0 for
		// pre-existing rows — their tier split was never
		// recorded, so they are honestly "tier unknown", never
		// retroactively split from the unsplit total.
		"cache_write_tokens_5m":            `ALTER TABLE usage_events ADD COLUMN cache_write_tokens_5m INTEGER NOT NULL DEFAULT 0`,
		"cache_write_tokens_1h":            `ALTER TABLE usage_events ADD COLUMN cache_write_tokens_1h INTEGER NOT NULL DEFAULT 0`,
		"prompt_before_compression_tokens": `ALTER TABLE usage_events ADD COLUMN prompt_before_compression_tokens INTEGER NOT NULL DEFAULT 0`,
		"compression_saved_tokens":         `ALTER TABLE usage_events ADD COLUMN compression_saved_tokens INTEGER NOT NULL DEFAULT 0`,
		// cache_tokens_saved (B4-16): default 0 for pre-existing
		// rows — their cache saving was never recorded, so they
		// are honestly "not measured", never retroactively
		// filled in.
		"cache_tokens_saved": `ALTER TABLE usage_events ADD COLUMN cache_tokens_saved INTEGER NOT NULL DEFAULT 0`,
		"local_estimate":     `ALTER TABLE usage_events ADD COLUMN local_estimate INTEGER NOT NULL DEFAULT 0`,
		// local_estimate_prompt_tokens defaults to 0 for
		// pre-existing rows: their local estimate was never
		// recorded alongside the reported split, so they are
		// honestly "not measured" — never retroactively filled
		// from prompt_tokens, which on a split-known row is the
		// provider's number, not the estimate.
		"local_estimate_prompt_tokens": `ALTER TABLE usage_events ADD COLUMN local_estimate_prompt_tokens INTEGER NOT NULL DEFAULT 0`,
		"tokens_split_known":           `ALTER TABLE usage_events ADD COLUMN tokens_split_known INTEGER NOT NULL DEFAULT 1`,
		// price_known defaults to 0 for pre-existing rows: their
		// price state at record time was never stored, so they
		// are honestly unknown spend, never retroactively
		// claimed as priced (or free).
		"price_known": `ALTER TABLE usage_events ADD COLUMN price_known INTEGER NOT NULL DEFAULT 0`,
		// endpoint defaults to "" for pre-existing rows: chat
		// completions was the only surface when they were
		// recorded, so "" (chat) is the honest value, not a
		// guess.
		"endpoint": `ALTER TABLE usage_events ADD COLUMN endpoint TEXT NOT NULL DEFAULT ''`,
		"shared":   `ALTER TABLE usage_events ADD COLUMN shared INTEGER NOT NULL DEFAULT 0`,
		// provider_key_id defaults to '' for pre-existing rows:
		// they were recorded before key pooling existed, so they
		// are honestly unattributed — never retroactively assigned
		// to a key they may not have used.
		"provider_key_id": `ALTER TABLE usage_events ADD COLUMN provider_key_id TEXT NOT NULL DEFAULT ''`,
		// team_id defaults to '' for pre-existing rows: they were
		// recorded before teams existed, so they belong to no
		// team — never retroactively assigned to a team the key
		// may not have belonged to then.
		"team_id": `ALTER TABLE usage_events ADD COLUMN team_id TEXT NOT NULL DEFAULT ''`,
		// G16 (Area 11): reported-cost reconciliation, service
		// tier, hedge-cancelled flag, session/project/client
		// attribution. All default to "absent" for pre-existing
		// rows: their reported cost was never recorded, their
		// tier was standard, they were not hedge losers, and
		// they carry no attribution — never retroactively
		// filled in.
		"reported_cost_usd":   `ALTER TABLE usage_events ADD COLUMN reported_cost_usd REAL NOT NULL DEFAULT 0`,
		"reported_cost_known": `ALTER TABLE usage_events ADD COLUMN reported_cost_known INTEGER NOT NULL DEFAULT 0`,
		"cost_delta_usd":      `ALTER TABLE usage_events ADD COLUMN cost_delta_usd REAL NOT NULL DEFAULT 0`,
		"service_tier":        `ALTER TABLE usage_events ADD COLUMN service_tier TEXT NOT NULL DEFAULT ''`,
		"cancelled":           `ALTER TABLE usage_events ADD COLUMN cancelled INTEGER NOT NULL DEFAULT 0`,
		"session_id":          `ALTER TABLE usage_events ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
		"project_id":          `ALTER TABLE usage_events ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`,
		"client_id":           `ALTER TABLE usage_events ADD COLUMN client_id TEXT NOT NULL DEFAULT ''`,
		// auto_variant defaults to '' for pre-existing rows:
		// they were recorded before the variant was captured,
		// so they are honestly "not auto-routed / unknown",
		// never retroactively assigned to a variant.
		"auto_variant": `ALTER TABLE usage_events ADD COLUMN auto_variant TEXT NOT NULL DEFAULT ''`,
	}
	for col, stmt := range add {
		if present[col] {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger: add column %s: %w", col, err)
		}
	}
	// The team index lives here, not in the schema string: the
	// schema runs before this migration on an old database, and
	// the index would fail there because team_id does not exist
	// yet.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_team_ts ON usage_events(team_id, ts)`); err != nil {
		return fmt.Errorf("ledger: create team index: %w", err)
	}
	// Attribution indexes (G16): the session / project / client
	// filters in RecentEventsByAttribution scan by these.
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_session_ts ON usage_events(session_id, ts)`); err != nil {
		return fmt.Errorf("ledger: create session index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_project_ts ON usage_events(project_id, ts)`); err != nil {
		return fmt.Errorf("ledger: create project index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_client_ts ON usage_events(client_id, ts)`); err != nil {
		return fmt.Errorf("ledger: create client index: %w", err)
	}
	return nil
}

// ensureKeyBudgetColumns adds the M5.1 calendar-window budget
// columns to key_budgets when they are missing (databases created
// by an older build). Same pattern as ensureUsageColumns: PRAGMA
// table_info first, idempotent. NULL limits mean "unset" — a
// pre-existing row gains no calendar cap from the migration, only
// the selector defaults (daily / 00:00 / 0.8).
func ensureKeyBudgetColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(key_budgets)`)
	if err != nil {
		return fmt.Errorf("ledger: inspect key_budgets schema: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("ledger: inspect key_budgets schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: inspect key_budgets schema: %w", err)
	}
	rows.Close()
	add := map[string]string{
		"daily_limit_usd":   `ALTER TABLE key_budgets ADD COLUMN daily_limit_usd REAL`,
		"weekly_limit_usd":  `ALTER TABLE key_budgets ADD COLUMN weekly_limit_usd REAL`,
		"monthly_limit_usd": `ALTER TABLE key_budgets ADD COLUMN monthly_limit_usd REAL`,
		"warning_threshold": `ALTER TABLE key_budgets ADD COLUMN warning_threshold REAL NOT NULL DEFAULT 0.8`,
		"reset_interval":    `ALTER TABLE key_budgets ADD COLUMN reset_interval TEXT NOT NULL DEFAULT 'daily'`,
		"reset_time":        `ALTER TABLE key_budgets ADD COLUMN reset_time TEXT NOT NULL DEFAULT '00:00'`,
	}
	for col, stmt := range add {
		if present[col] {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("ledger: add column %s: %w", col, err)
		}
	}
	return nil
}

// Close closes the underlying database (and any prepared
// hot-path statement, first).
func (l *Ledger) Close() error {
	if l.db == nil {
		return nil
	}
	l.stmtMu.Lock()
	if l.getBudgetStmt != nil {
		_ = l.getBudgetStmt.Close()
		l.getBudgetStmt = nil
	}
	if l.getKeyBudgetStmt != nil {
		_ = l.getKeyBudgetStmt.Close()
		l.getKeyBudgetStmt = nil
	}
	l.stmtMu.Unlock()
	return l.db.Close()
}

// preparedGetBudget returns the prepared SELECT for GetBudget,
// preparing it once on first use. ok=false means preparation
// failed (e.g. the DB is already closed): the caller must fall
// back to the direct query on l.db.
func (l *Ledger) preparedGetBudget(ctx context.Context) (*sql.Stmt, bool) {
	l.stmtMu.Lock()
	defer l.stmtMu.Unlock()
	if l.getBudgetStmt != nil {
		return l.getBudgetStmt, true
	}
	stmt, err := l.db.PrepareContext(ctx, `
		SELECT key_id, cap_usd, spent_usd, period FROM budgets
		WHERE key_id = ?`)
	if err != nil {
		return nil, false
	}
	l.getBudgetStmt = stmt
	return stmt, true
}

// preparedGetKeyBudget is preparedGetBudget for GetKeyBudget's
// SELECT. Same contract: ok=false falls back to the direct query.
func (l *Ledger) preparedGetKeyBudget(ctx context.Context) (*sql.Stmt, bool) {
	l.stmtMu.Lock()
	defer l.stmtMu.Unlock()
	if l.getKeyBudgetStmt != nil {
		return l.getKeyBudgetStmt, true
	}
	stmt, err := l.db.PrepareContext(ctx, `
		SELECT key_id, limit_usd, unpriced_policy,
		       daily_limit_usd, weekly_limit_usd, monthly_limit_usd,
		       warning_threshold, reset_interval, reset_time
		FROM key_budgets
		WHERE key_id = ?`)
	if err != nil {
		return nil, false
	}
	l.getKeyBudgetStmt = stmt
	return stmt, true
}

// Path returns the SQLite file path backing this ledger.
func (l *Ledger) Path() string { return l.path }

// RecordUsage records one request and returns its computed USD cost.
// The cost comes from prices; an unknown (provider, model) pair
// costs 0 and is recorded as 0. When a budget row exists for the key, budgets.spent_usd is
// incremented in the same transaction, so spend and budget stay in sync.
//
// Split handling (honest accounting, never a guessed split):
//   - SplitKnown true: prompt/completion are stored as given;
//     TotalTokens is stored as given, or prompt+completion when it is 0;
//     cost comes from the split prices.
//   - SplitKnown false: prompt and completion are stored as 0, the
//     reported total is stored in total_tokens, tokens_split_known is
//     0, and cost is 0 — a split is needed for honest pricing, so no
//     cost is guessed from a bare total.
//   - Shared true: the row is a waiter served from a coalesced
//     upstream call — tokens, split and cost are all forced to 0
//     (the leader's row carries the real usage), so a shared row can
//     never inflate spend or token totals.
//   - ReasoningTokens / CacheWriteTokens are provider-reported
//     subsets (of completion / prompt): stored only with a known
//     split, clamped to their parent count, 0 otherwise.
//     CacheWriteTokens5m / CacheWriteTokens1h (M6.9) further
//     split the cache writes by TTL tier; when either is set
//     the total is their sum and the row prices per tier
//     (CostWithCacheTiers), and when both are 0 the unsplit
//     total stands with its tier unknown.
//   - LocalEstimate true with SplitKnown false: the upstream
//     reported no usage, so the prompt side is a local tiktoken
//     estimate — stored in prompt_tokens/total_tokens with
//     tokens_split_known = 0, local_estimate = 1, cost 0. A known
//     split always supersedes an estimate (the flag is cleared).
//
// normalizedRow is one usage row ready to store: sanitized strings,
// honest token split, computed cost. normalizeUsage is the single
// implementation shared by the synchronous RecordUsage and the async
// batch writer (async.go), so the two paths can never drift apart
// on what a row means or costs.
type normalizedRow struct {
	gatewayKeyID string
	// providerKeyID is the provider-side key that served the
	// attempt (CHECKLIST 175), carried verbatim from Usage —
	// it is an ID, never key material, and it is never pooled:
	// each key's rows keep their own ID.
	providerKeyID    string
	provider         string
	model            string
	endpoint         string
	promptTokens     int
	completionTokens int
	totalTokens      int
	cachedTokens     int
	reasoningTokens  int
	cacheWriteTokens int
	// cacheWriteTokens5m / cacheWriteTokens1h (M6.9) are the
	// cache-write split by TTL tier. Both 0 with a positive
	// cacheWriteTokens means the legacy unsplit total: the tier
	// is unknown, and the row prices at the plain input rate.
	cacheWriteTokens5m int
	cacheWriteTokens1h int
	// promptBeforeCompression / compressionSavedTokens are
	// the compression record (CHECKLIST 183): the local
	// estimate of the prompt before compression, and the
	// tokens it saved. Stored verbatim from Usage (already
	// local estimates), clamped below — never derived here.
	promptBeforeCompression int
	compressionSavedTokens  int
	cacheTokensSaved        int
	localEstimate           int
	// localEstimatePrompt is the local tiktoken estimate of the
	// prompt as sent, stored alongside the reported split
	// (CHECKLIST 229) — 0 when the router never computed one.
	// It is clamped to >= 0 and zeroed on shared rows, like
	// every other token field; it never feeds cost.
	localEstimatePrompt int
	splitKnown          int
	// priceKnown is 1 when the (provider, model) pair had a
	// configured price at record time (including a genuinely-free
	// $0 price) AND the split is known — a split is needed for
	// honest pricing, so a split-unknown row is never marked
	// priced even when the model has a price. 0 means the row's
	// cost is unknown spend, not $0 (see keybudget.go).
	priceKnown int
	// reportedCost is the exact cost the upstream reported for
	// this attempt (0 when none was reported); reportedKnown is
	// 1 when one was reported and valid. When known, cost IS
	// the reported cost (it wins over the hand-computed cost)
	// and costDelta is reported − computed — the reconciliation
	// delta (0 when nothing was reported).
	reportedCost  float64
	reportedKnown int
	costDelta     float64
	cost          float64
	// serviceTier is the request's service tier, stored verbatim
	// ("" = standard). The tier multiplier is already folded
	// into cost above (hand-computed cost only).
	serviceTier string
	// autoVariant is the auto-routing variant that served this
	// attempt ("auto", "auto/thrifty", "auto/best-code", ...),
	// stored verbatim. "" means the request was not
	// auto-routed, or the variant was never recorded. It is
	// what the /api/analytics/auto-routing breakdown groups
	// by. It survives shared-row zeroing: a coalesced waiter
	// was still auto-routed, and the breakdown counts requests
	// served, not upstream calls.
	autoVariant string
	// cancelled is 1 when the attempt was started then
	// cancelled (a hedge loser): stored, never counted as a
	// failure.
	cancelled int
	// sessionID / projectID / clientID are the attribution
	// dimensions, stored verbatim ("" = unattributed).
	sessionID string
	projectID string
	clientID  string
	// gitBranch is the git branch the attempt ran under, when
	// the caller carried one (AK-3 agent-session rollup):
	// stored on the session row, never on the usage row (the
	// usage_events table has no branch column). "" means the
	// request carried no branch.
	gitBranch string
	ok        int
	latencyMs int64
	shared    int
}

// normalizeUsage applies the ledger's storage rules to u: spend-log
// redaction of the string fields (LiteLLM entry 152, ON by default —
// a misconfigured identifier carrying a key-shaped value must not
// land in the spend log verbatim; ordinary IDs are unchanged), the
// honest split/cost rules (see RecordUsage), and the shared-row
// zeroing (a waiter served from a coalesced call is never billed).
// flatRateAsZero is the ledger's FlatRateAsZero opt-in: a flat-rate
// provider's computed cost is $0 when it is on (see CostOptions).
func normalizeUsage(u Usage, prices Prices, flatRateAsZero bool) normalizedRow {
	// Cost is computed from the redacted model name, so a redacted
	// model prices at 0 rather than at a guessed price.
	u.GatewayKeyID = guardrails.SanitizeSpendString(u.GatewayKeyID)
	u.ProviderKeyID = guardrails.SanitizeSpendString(u.ProviderKeyID)
	u.Provider = guardrails.SanitizeSpendString(u.Provider)
	u.Model = guardrails.SanitizeSpendString(u.Model)
	u.Endpoint = guardrails.SanitizeSpendString(u.Endpoint)
	u.ServiceTier = guardrails.SanitizeSpendString(u.ServiceTier)
	u.AutoVariant = guardrails.SanitizeSpendString(u.AutoVariant)
	u.SessionID = guardrails.SanitizeSpendString(u.SessionID)
	u.ProjectID = guardrails.SanitizeSpendString(u.ProjectID)
	u.ClientID = guardrails.SanitizeSpendString(u.ClientID)
	u.GitBranch = guardrails.SanitizeSpendString(u.GitBranch)
	var r normalizedRow
	r.gatewayKeyID = u.GatewayKeyID
	r.providerKeyID = u.ProviderKeyID
	r.provider = u.Provider
	r.model = u.Model
	r.endpoint = u.Endpoint
	r.serviceTier = u.ServiceTier
	r.autoVariant = u.AutoVariant
	r.sessionID = u.SessionID
	r.projectID = u.ProjectID
	r.clientID = u.ClientID
	r.gitBranch = u.GitBranch
	if u.Cancelled {
		r.cancelled = 1
	}
	r.latencyMs = u.LatencyMS
	if u.SplitKnown {
		r.promptTokens = u.PromptTokens
		r.completionTokens = u.CompletionTokens
		r.totalTokens = u.TotalTokens
		if r.totalTokens == 0 {
			r.totalTokens = r.promptTokens + r.completionTokens
		}
		r.splitKnown = 1
		// Cached tokens are a subset of the prompt split: stored
		// only with a known split, clamped to the prompt count so
		// a malformed upstream report can never claim more cached
		// tokens than prompt tokens.
		r.cachedTokens = u.CachedTokens
		if r.cachedTokens < 0 {
			r.cachedTokens = 0
		}
		if r.cachedTokens > r.promptTokens {
			r.cachedTokens = r.promptTokens
		}
		// Reasoning tokens are a subset of the completion split,
		// cache-write tokens a subset of the prompt split: same
		// store-only-with-a-known-split rule, same clamping, so
		// a malformed report can never claim more reasoning than
		// completion or more cache writes than prompt tokens.
		// A known split supersedes any local estimate: the
		// provider's own numbers win, the estimate flag is off.
		r.reasoningTokens = u.ReasoningTokens
		if r.reasoningTokens < 0 {
			r.reasoningTokens = 0
		}
		if r.reasoningTokens > r.completionTokens {
			r.reasoningTokens = r.completionTokens
		}
		r.cacheWriteTokens = u.CacheWriteTokens
		if r.cacheWriteTokens < 0 {
			r.cacheWriteTokens = 0
		}
		if r.cacheWriteTokens > r.promptTokens {
			r.cacheWriteTokens = r.promptTokens
		}
		// Cache-write tiers (M6.9): subsets of the prompt after
		// cached reads, clamped in turn so tiers + cached reads
		// can never exceed the prompt. When either tier is set
		// the tiers win: the total becomes their sum (a
		// separately supplied total is ignored, never added on
		// top) and the row prices via CostWithCacheTiers. When
		// both tiers are 0 the legacy unsplit total above
		// stands, the tier is unknown, and the cost is the
		// legacy CostWithCached arithmetic — the writes already
		// price at the plain input rate inside the prompt leg.
		r.cacheWriteTokens5m = u.CacheWriteTokens5m
		if r.cacheWriteTokens5m < 0 {
			r.cacheWriteTokens5m = 0
		}
		if r.cacheWriteTokens5m > r.promptTokens-r.cachedTokens {
			r.cacheWriteTokens5m = r.promptTokens - r.cachedTokens
		}
		r.cacheWriteTokens1h = u.CacheWriteTokens1h
		if r.cacheWriteTokens1h < 0 {
			r.cacheWriteTokens1h = 0
		}
		if r.cacheWriteTokens1h > r.promptTokens-r.cachedTokens-r.cacheWriteTokens5m {
			r.cacheWriteTokens1h = r.promptTokens - r.cachedTokens - r.cacheWriteTokens5m
		}
		if r.cacheWriteTokens5m+r.cacheWriteTokens1h > 0 {
			r.cacheWriteTokens = r.cacheWriteTokens5m + r.cacheWriteTokens1h
		}
		r.localEstimate = 0
		// Hand-computed cost: every leg, every dimension —
		// cached-input leg, cache-write tiers, per-endpoint
		// legs, service-tier multiplier, flat-rate opt-in
		// (see CostWithOptions). A reported exact cost, when
		// present and valid, wins over this number below.
		computed := prices.CostWithOptions(u.Provider, u.Model, CostOptions{
			Endpoint:       u.Endpoint,
			ServiceTier:    u.ServiceTier,
			FlatRateAsZero: flatRateAsZero,
		}, r.promptTokens, r.completionTokens, r.cachedTokens, r.cacheWriteTokens5m, r.cacheWriteTokens1h)
		r.cost = computed
		// Priced only when a price is configured for this pair
		// (or its model-level fallback) — a configured $0 is
		// genuinely free and counts as priced; no entry means
		// the cost is unknown, never $0. A flat-rate provider
		// with the opt-in on is priced as $0 too: the $0 is a
		// real classification (subscription, not per-token),
		// not an unknown price.
		if prices.Known(u.Provider, u.Model) || (flatRateAsZero && IsFlatRateProvider(u.Provider)) {
			r.priceKnown = 1
		}
		// Gateway-reported exact cost (G16, Area 11): when the
		// upstream reported a valid cost it wins over the
		// hand-computed number — it is the provider's own
		// billing figure — and the reconciliation delta
		// (reported − computed) is recorded on the row. The
		// tier multiplier never applies to a reported cost.
		// An invalid reported cost (negative, NaN, infinite)
		// is ignored: a nonsense billing number must never
		// become the row's cost.
		if u.Cost != nil && !math.IsNaN(*u.Cost) && !math.IsInf(*u.Cost, 0) && *u.Cost >= 0 {
			r.reportedCost = *u.Cost
			r.reportedKnown = 1
			r.costDelta = *u.Cost - computed
			r.cost = *u.Cost
			r.priceKnown = 1
		}
	} else {
		r.totalTokens = u.TotalTokens
		r.splitKnown = 0
		r.cost = 0
		// A reported exact cost needs no split: it is the
		// upstream's own billing number, so it stands even
		// when the token split is unknown (e.g. a refusal the
		// upstream billed for). The delta is reported − 0:
		// there was no hand-computed cost to reconcile
		// against.
		if u.Cost != nil && !math.IsNaN(*u.Cost) && !math.IsInf(*u.Cost, 0) && *u.Cost >= 0 {
			r.reportedCost = *u.Cost
			r.reportedKnown = 1
			r.costDelta = *u.Cost
			r.cost = *u.Cost
			r.priceKnown = 1
		}
		if u.LocalEstimate {
			// The upstream reported no usage, so the prompt side
			// is a local tiktoken estimate: store it in
			// prompt_tokens (and total_tokens) so the row still
			// says how big the prompt was, but keep the split
			// unknown (no completion count exists) and the cost
			// 0 — an estimate can never be priced. The flag is
			// what distinguishes this row from a reported split.
			r.promptTokens = u.PromptTokens
			if r.promptTokens < 0 {
				r.promptTokens = 0
			}
			r.totalTokens = r.promptTokens
			r.localEstimate = 1
		}
	}
	// No-charge row (B4-51): the request made no upstream call at
	// all (proactive own-cache hit), so there is no cost to price
	// — neither a hand-computed cost (the model-level fallback
	// would otherwise price the avoided call) nor a reported
	// one. The $0 is known, not unknown: the row counts as
	// priced, and a reported cost on such a row is contradictory
	// and ignored.
	if u.NoCharge {
		r.cost = 0
		r.priceKnown = 1
		r.reportedCost = 0
		r.reportedKnown = 0
		r.costDelta = 0
	}
	// Compression record (CHECKLIST 183): stored on every row
	// that carries it, not only alongside a known split — a
	// local-estimate row (upstream reported no usage, or a
	// successful stream whose usage the engine counted locally)
	// still stores its prompt count, and the saving is measured
	// against that same local count, so dropping it here would
	// silently lose the saving for every stream and every
	// no-usage upstream. Clamped so a malformed record can never
	// claim more saved than the prompt had before compression,
	// or a negative saving. A before-count of 0 with a positive
	// saving is contradictory — the saving is zeroed, not
	// trusted. Shared rows are zeroed below, like every other
	// token field.
	r.promptBeforeCompression = u.PromptBeforeCompression
	if r.promptBeforeCompression < 0 {
		r.promptBeforeCompression = 0
	}
	r.compressionSavedTokens = u.CompressionSavedTokens
	if r.compressionSavedTokens < 0 {
		r.compressionSavedTokens = 0
	}
	if r.promptBeforeCompression == 0 {
		r.compressionSavedTokens = 0
	} else if r.compressionSavedTokens > r.promptBeforeCompression {
		r.compressionSavedTokens = r.promptBeforeCompression
	}
	// Cache-hit saving (B4-16, Area 11 cost-saved row): the tokens
	// this request saved by being served from the router's own
	// exact/semantic cache instead of an upstream call, as
	// measured by the caller that served the hit. Stored verbatim
	// from Usage (already measured), never derived here — the
	// ledger cannot know what the upstream would have billed.
	// Clamped like the compression record: never negative, and a
	// saving larger than the call's own token counts is
	// contradictory — the saving can never exceed the call it
	// avoided. A row with no token counts at all and a positive
	// saving is likewise contradictory — zeroed, not trusted.
	// Shared rows are zeroed below, like every other token field.
	r.cacheTokensSaved = u.CacheTokensSaved
	if r.cacheTokensSaved < 0 {
		r.cacheTokensSaved = 0
	}
	if r.splitKnown == 1 {
		if total := r.promptTokens + r.completionTokens; r.cacheTokensSaved > total {
			r.cacheTokensSaved = total
		}
	} else if r.totalTokens == 0 {
		r.cacheTokensSaved = 0
	}
	if u.OK {
		r.ok = 1
	}
	// Local-estimate prompt (CHECKLIST 229): the router's local
	// estimate of the prompt as sent, stored on every row that
	// carries one — alongside a known split AND on a
	// local-estimate row (where it equals prompt_tokens, which
	// already holds the estimate). A row recorded directly,
	// without going through the router, may carry LocalEstimate
	// with no LocalEstimatePromptTokens: fall back to the stored
	// prompt count, which on such a row IS the estimate.
	// Clamped to >= 0; a negative value is a caller bug, never
	// a count. Shared rows are zeroed below.
	r.localEstimatePrompt = u.LocalEstimatePromptTokens
	if r.localEstimatePrompt < 0 {
		r.localEstimatePrompt = 0
	}
	if u.LocalEstimate && r.localEstimatePrompt == 0 {
		r.localEstimatePrompt = r.promptTokens
	}
	if u.Shared {
		r.shared = 1
		// A shared row is served, never billed: zero tokens and
		// zero cost, whatever the caller passed. The leader's row
		// carries the real usage for the one upstream call.
		r.promptTokens, r.completionTokens, r.totalTokens, r.cachedTokens, r.reasoningTokens, r.cacheWriteTokens, r.cacheWriteTokens5m, r.cacheWriteTokens1h, r.promptBeforeCompression, r.compressionSavedTokens, r.cacheTokensSaved, r.localEstimate, r.localEstimatePrompt, r.splitKnown, r.priceKnown, r.cost = 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
		r.reportedCost, r.reportedKnown, r.costDelta = 0, 0, 0
	}
	return r
}

func (l *Ledger) RecordUsage(ctx context.Context, u Usage, prices Prices) (float64, error) {
	r := normalizeUsage(u, prices, l.FlatRateAsZero)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ledger: begin tx: %w", err)
	}
	defer tx.Rollback()
	// M5.12: attribute the row to the cost center the key belongs
	// to right now, atomically with the insert — the binding is
	// what the team budget counts, and a later reassignment must
	// not rewrite history.
	teamID, err := activeTeamIDForKeyTx(ctx, tx, r.gatewayKeyID)
	if err != nil {
		return 0, fmt.Errorf("ledger: resolve team for usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events
		  (ts, gateway_key_id, provider_key_id, provider, model, endpoint, prompt_tokens,
		   completion_tokens, total_tokens, cached_tokens,
		   reasoning_tokens, cache_write_tokens,
		   cache_write_tokens_5m, cache_write_tokens_1h,
		   prompt_before_compression_tokens, compression_saved_tokens, cache_tokens_saved,
		   local_estimate, local_estimate_prompt_tokens,
		   tokens_split_known, price_known,
		   cost_usd, ok, latency_ms, shared, team_id,
		   reported_cost_usd, reported_cost_known, cost_delta_usd,
		   service_tier, cancelled, session_id, project_id, client_id, auto_variant)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), r.gatewayKeyID, r.providerKeyID, r.provider, r.model, r.endpoint,
		r.promptTokens, r.completionTokens, r.totalTokens, r.cachedTokens,
		r.reasoningTokens, r.cacheWriteTokens,
		r.cacheWriteTokens5m, r.cacheWriteTokens1h,
		r.promptBeforeCompression, r.compressionSavedTokens, r.cacheTokensSaved, r.localEstimate, r.localEstimatePrompt, r.splitKnown,
		r.priceKnown,
		r.cost, r.ok, r.latencyMs, r.shared, teamID,
		r.reportedCost, r.reportedKnown, r.costDelta,
		r.serviceTier, r.cancelled, r.sessionID, r.projectID, r.clientID, r.autoVariant); err != nil {
		return 0, fmt.Errorf("ledger: insert usage event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE budgets SET spent_usd = spent_usd + ? WHERE key_id = ?`,
		r.cost, r.gatewayKeyID); err != nil {
		return 0, fmt.Errorf("ledger: update budget spend: %w", err)
	}
	// AK-3: fold this row into the agent-session rollup in the
	// SAME transaction as the usage row + budget update, so
	// usage and session counters move together or neither does.
	if err := upsertAgentSessionTx(ctx, tx, r, time.Now()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("ledger: commit: %w", err)
	}
	// The commit moved this key's spent_usd: a later GetBudget
	// on the same request context must re-read, not serve the
	// memoised pre-spend row (CHECKLIST 299-300).
	invalidateBudgetMemo(ctx, r.gatewayKeyID)
	return r.cost, nil
}

// RecordShared records one request served from a coalesced
// (single-flight) upstream call: a row marked shared with zero tokens
// and zero cost (see Usage.Shared). ok is whether the shared result
// was a success; latencyMs is how long the waiter waited for the
// leader's flight. It never moves a budget (cost is always 0).
func (l *Ledger) RecordShared(ctx context.Context, provider, model string, ok bool, latencyMs int64) (float64, error) {
	return l.RecordUsage(ctx, Usage{
		Provider:  provider,
		Model:     model,
		OK:        ok,
		LatencyMS: latencyMs,
		Shared:    true,
	}, nil)
}

// ---------------------------------------------------------------------------
// Dashboard live feeds: recent events, range summaries, time buckets.
//
// These are the read side for GET /dashboard/overview, /dashboard/logs
// and /dashboard/usage. Every figure comes from usage_events rows
// recorded by RecordUsage — real attempts only. A fresh router with
// no traffic returns empty slices and zero totals: the dashboard
// renders that as "No requests yet", never as invented numbers.
// A model with no configured price records cost 0 (see Cost), so
// spend totals are honest lower bounds — the usage endpoint says so
// instead of presenting them as exact billing.
// ---------------------------------------------------------------------------

// UsageEvent is one recorded request attempt, as the request log shows
// it. It carries metadata only: provider, model, token counts, cost,
// latency, outcome — never prompt or response content (none is stored).
type UsageEvent struct {
	ID           int64     `json:"id"`
	TS           time.Time `json:"ts"`
	GatewayKeyID string    `json:"gateway_key_id"`
	Provider     string    `json:"provider"`
	Model        string    `json:"model"`
	// Endpoint names the API surface the attempt was made to:
	// "" (chat completions — including every row recorded
	// before this field existed), "embeddings", "rerank", or
	// another provider.EndpointKind string. The dashboard
	// renders "" as "chat".
	Endpoint         string `json:"endpoint,omitempty"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	// CachedTokens is the provider-reported count of prompt tokens
	// served from the provider's prompt cache for this attempt
	// (0 when the upstream reported none, or the split is unknown).
	CachedTokens int `json:"cached_tokens"`
	// ReasoningTokens is the provider-reported count of completion
	// tokens spent on internal reasoning for this attempt (0 when
	// the upstream reported no reasoning split).
	ReasoningTokens int `json:"reasoning_tokens"`
	// CacheWriteTokens is the provider-reported count of input
	// tokens written into the provider's prompt cache on this
	// attempt (0 when not reported).
	CacheWriteTokens int `json:"cache_write_tokens"`
	// CacheWriteTokens5m / CacheWriteTokens1h (M6.9) split that
	// total by TTL tier (Anthropic ephemeral_5m / ephemeral_1h).
	// Both 0 with a positive CacheWriteTokens means the tier
	// split was never reported — tier unknown, never guessed.
	CacheWriteTokens5m int `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h int `json:"cache_write_tokens_1h"`
	// PromptBeforeCompressionTokens / CompressionSavedTokens
	// are the lossless-first compression record for this
	// attempt (CHECKLIST 183): the local tiktoken estimate of
	// the prompt BEFORE compression, and how many of those
	// tokens compression saved. PromptTokens is the prompt AS
	// SENT. Both are local estimates, never provider-reported;
	// 0 / 0 means no compression applied.
	PromptBeforeCompressionTokens int `json:"prompt_before_compression_tokens"`
	CompressionSavedTokens        int `json:"compression_saved_tokens"`
	// CacheTokensSaved is the tokens this request saved by being
	// served from the router's own exact/semantic cache instead
	// of an upstream call (0 when no cache saving was recorded
	// for this attempt). See Usage.CacheTokensSaved.
	CacheTokensSaved int `json:"cache_tokens_saved"`
	// LocalEstimate is true when PromptTokens on this row is a
	// local tiktoken estimate, not a provider-reported count
	// (the upstream reported no usage at all). SplitKnown is
	// false on such a row.
	LocalEstimate bool `json:"local_estimate"`
	// LocalEstimatePromptTokens is the local tiktoken estimate
	// of the prompt as sent for this attempt, recorded
	// alongside the provider-reported split when the router
	// computed one (CHECKLIST 229). 0 means it was never
	// measured for this row. On a local-estimate row it equals
	// PromptTokens.
	LocalEstimatePromptTokens int     `json:"local_estimate_prompt_tokens"`
	SplitKnown                bool    `json:"split_known"`
	CostUSD                   float64 `json:"cost_usd"`
	// ReportedCostUSD is the exact cost the upstream reported
	// for this attempt, when it reported one (nil otherwise);
	// CostUSD is that reported cost when present, else the
	// hand-computed cost. CostDeltaUSD is reported minus
	// computed (0 when nothing was reported) — the
	// reconciliation delta.
	ReportedCostUSD *float64 `json:"reported_cost_usd,omitempty"`
	CostDeltaUSD    float64  `json:"cost_delta_usd"`
	// ServiceTier is the request's service tier ("" = standard).
	ServiceTier string `json:"service_tier,omitempty"`
	// Cancelled is true when the attempt was started then
	// cancelled (a hedge loser): stored, never counted as a
	// failure.
	Cancelled bool `json:"cancelled,omitempty"`
	// SessionID / ProjectID / ClientID are the attribution
	// dimensions for the attempt ("" = unattributed).
	SessionID string `json:"session_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	ClientID  string `json:"client_id,omitempty"`
	// AutoVariant is the auto-routing variant that served this
	// attempt ("" = not auto-routed, or variant never recorded).
	AutoVariant string `json:"auto_variant,omitempty"`
	OK          bool   `json:"ok"`
	LatencyMS   int64  `json:"latency_ms"`
	Shared      bool   `json:"shared"`
}

// LogFilter selects which rows RecentEvents returns: "all" (every
// attempt), "ok" (successful attempts only) or "failed" (failed
// attempts only — including attempts that later failed over, since
// each attempt is its own row).
type LogFilter string

const (
	// LogAll returns every recorded attempt.
	LogAll LogFilter = "all"
	// LogOK returns successful attempts only.
	LogOK LogFilter = "ok"
	// LogFailed returns failed attempts only.
	LogFailed LogFilter = "failed"
)

// RecentEvents returns the most recent usage events, newest first.
// limit <= 0 means 10; limit is capped at 500. An unknown filter
// behaves like "all".
func (l *Ledger) RecentEvents(ctx context.Context, limit int, filter LogFilter) ([]UsageEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 500 {
		limit = 500
	}
	where := ""
	switch filter {
	case LogOK:
		where = "WHERE ok = 1"
	case LogFailed:
		// Cancelled attempts (hedge losers) are not failures:
		// they were started, then cancelled because the other
		// hedge attempt won. They are stored, but the failed
		// filter excludes them.
		where = "WHERE ok = 0 AND cancelled = 0"
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, ts, gateway_key_id, provider, model, endpoint,
		       prompt_tokens,
		       completion_tokens, total_tokens, cached_tokens,
		       reasoning_tokens, cache_write_tokens,
		       cache_write_tokens_5m, cache_write_tokens_1h,
		       prompt_before_compression_tokens, compression_saved_tokens, cache_tokens_saved,
		       local_estimate, local_estimate_prompt_tokens,
		       tokens_split_known,
		       cost_usd, ok, latency_ms, shared,
		       reported_cost_usd, reported_cost_known, cost_delta_usd,
		       service_tier, cancelled, session_id, project_id, client_id, auto_variant
		FROM usage_events `+where+`
		ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: recent events: %w", err)
	}
	defer rows.Close()
	events := []UsageEvent{}
	for rows.Next() {
		var e UsageEvent
		var tsMs int64
		var splitKnown, ok, shared, localEstimate int
		var reportedKnown, cancelled int
		var reportedCost float64
		if err := rows.Scan(&e.ID, &tsMs, &e.GatewayKeyID, &e.Provider, &e.Model, &e.Endpoint,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CachedTokens,
			&e.ReasoningTokens, &e.CacheWriteTokens,
			&e.CacheWriteTokens5m, &e.CacheWriteTokens1h,
			&e.PromptBeforeCompressionTokens, &e.CompressionSavedTokens, &e.CacheTokensSaved,
			&localEstimate, &e.LocalEstimatePromptTokens, &splitKnown,
			&e.CostUSD, &ok, &e.LatencyMS, &shared,
			&reportedCost, &reportedKnown, &e.CostDeltaUSD,
			&e.ServiceTier, &cancelled, &e.SessionID, &e.ProjectID, &e.ClientID, &e.AutoVariant); err != nil {
			return nil, fmt.Errorf("ledger: recent events: %w", err)
		}
		e.TS = time.UnixMilli(tsMs).UTC()
		e.SplitKnown = splitKnown != 0
		e.OK = ok != 0
		e.Shared = shared != 0
		e.LocalEstimate = localEstimate != 0
		e.Cancelled = cancelled != 0
		if reportedKnown != 0 {
			v := reportedCost
			e.ReportedCostUSD = &v
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: recent events: %w", err)
	}
	return events, nil
}

// AttributionFilter selects usage rows by session / project /
// client attribution. An empty field matches any value (no filter
// on that dimension); all non-empty fields must match.
type AttributionFilter struct {
	SessionID string
	ProjectID string
	ClientID  string
}

// RecentEventsByAttribution returns the most recent usage events
// matching f, newest first. limit <= 0 means 10; limit is capped
// at 500. A filter with every field empty behaves like
// RecentEvents with LogAll.
func (l *Ledger) RecentEventsByAttribution(ctx context.Context, f AttributionFilter, limit int) ([]UsageEvent, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 500 {
		limit = 500
	}
	where := ""
	args := []any{}
	if f.SessionID != "" {
		where += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.ProjectID != "" {
		where += " AND project_id = ?"
		args = append(args, f.ProjectID)
	}
	if f.ClientID != "" {
		where += " AND client_id = ?"
		args = append(args, f.ClientID)
	}
	if where != "" {
		where = "WHERE 1=1" + where
	}
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, `
		SELECT id, ts, gateway_key_id, provider, model, endpoint,
		       prompt_tokens,
		       completion_tokens, total_tokens, cached_tokens,
		       reasoning_tokens, cache_write_tokens,
		       cache_write_tokens_5m, cache_write_tokens_1h,
		       prompt_before_compression_tokens, compression_saved_tokens, cache_tokens_saved,
		       local_estimate, local_estimate_prompt_tokens,
		       tokens_split_known,
		       cost_usd, ok, latency_ms, shared,
		       reported_cost_usd, reported_cost_known, cost_delta_usd,
		       service_tier, cancelled, session_id, project_id, client_id, auto_variant
		FROM usage_events `+where+`
		ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("ledger: recent events by attribution: %w", err)
	}
	defer rows.Close()
	events := []UsageEvent{}
	for rows.Next() {
		var e UsageEvent
		var tsMs int64
		var splitKnown, ok, shared, localEstimate int
		var reportedKnown, cancelled int
		var reportedCost float64
		if err := rows.Scan(&e.ID, &tsMs, &e.GatewayKeyID, &e.Provider, &e.Model, &e.Endpoint,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens, &e.CachedTokens,
			&e.ReasoningTokens, &e.CacheWriteTokens,
			&e.CacheWriteTokens5m, &e.CacheWriteTokens1h,
			&e.PromptBeforeCompressionTokens, &e.CompressionSavedTokens, &e.CacheTokensSaved,
			&localEstimate, &e.LocalEstimatePromptTokens, &splitKnown,
			&e.CostUSD, &ok, &e.LatencyMS, &shared,
			&reportedCost, &reportedKnown, &e.CostDeltaUSD,
			&e.ServiceTier, &cancelled, &e.SessionID, &e.ProjectID, &e.ClientID, &e.AutoVariant); err != nil {
			return nil, fmt.Errorf("ledger: recent events by attribution: %w", err)
		}
		e.TS = time.UnixMilli(tsMs).UTC()
		e.SplitKnown = splitKnown != 0
		e.OK = ok != 0
		e.Shared = shared != 0
		e.LocalEstimate = localEstimate != 0
		e.Cancelled = cancelled != 0
		if reportedKnown != 0 {
			v := reportedCost
			e.ReportedCostUSD = &v
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: recent events by attribution: %w", err)
	}
	return events, nil
}

// CompressionProviderRow is one provider's compression totals in
// a window: the requests whose prompt was compressed, the prompt
// tokens before compression, and the tokens compression saved.
// SavingRate is saved / prompt-before (0 when nothing was
// compressed): the share of the prompt compression removed.
type CompressionProviderRow struct {
	Provider           string  `json:"provider"`
	Requests           int     `json:"requests"`
	PromptBeforeTokens int     `json:"prompt_before_tokens"`
	TokensSaved        int     `json:"tokens_saved"`
	SavingRate         float64 `json:"saving_rate"`
}

// CompressionByProvider returns per-provider compression totals
// for [start, end): one row per provider with at least one
// compressed request in the window, most tokens saved first. A
// provider with no compressed request in the window is absent,
// never zero-filled. The figures come from the compression record
// stored on each usage row (prompt before compression + tokens
// saved, local estimates — see Usage), never from a separate
// compression log.
func (l *Ledger) CompressionByProvider(ctx context.Context, start, end time.Time) ([]CompressionProviderRow, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT provider,
		       COUNT(*) AS requests,
		       SUM(prompt_before_compression_tokens) AS prompt_before,
		       SUM(compression_saved_tokens) AS saved
		FROM usage_events
		WHERE ts >= ? AND ts < ? AND compression_saved_tokens > 0
		GROUP BY provider
		ORDER BY saved DESC`,
		start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: compression by provider: %w", err)
	}
	defer rows.Close()
	out := []CompressionProviderRow{}
	for rows.Next() {
		var r CompressionProviderRow
		if err := rows.Scan(&r.Provider, &r.Requests, &r.PromptBeforeTokens, &r.TokensSaved); err != nil {
			return nil, fmt.Errorf("ledger: compression by provider: %w", err)
		}
		if r.PromptBeforeTokens > 0 {
			r.SavingRate = float64(r.TokensSaved) / float64(r.PromptBeforeTokens)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: compression by provider: %w", err)
	}
	return out, nil
}

// AutoRoutingProviderRow is one provider's share of auto-routed
// requests in a window.
type AutoRoutingProviderRow struct {
	Provider string `json:"provider"`
	Requests int    `json:"requests"`
}

// AutoRoutingBreakdown is the auto-routing analytics for a window:
// the requests the auto dispatcher served (rows carrying an auto
// variant), grouped by variant, plus the providers that served
// them. Variants is never nil: an empty window reads as an empty
// map, never null. TotalRequests counts rows, so a coalesced
// waiter and its leader both count — both were requests served.
type AutoRoutingBreakdown struct {
	TotalRequests int                      `json:"total_requests"`
	Variants      map[string]int           `json:"variant_breakdown"`
	TopProviders  []AutoRoutingProviderRow `json:"top_providers"`
}

// AutoRoutingBreakdown returns the auto-routing breakdown for
// [start, end). Rows with no variant (explicit-model requests, or
// rows recorded before the variant was captured) are excluded:
// the breakdown is auto-routed traffic only.
func (l *Ledger) AutoRoutingBreakdown(ctx context.Context, start, end time.Time) (AutoRoutingBreakdown, error) {
	out := AutoRoutingBreakdown{Variants: map[string]int{}, TopProviders: []AutoRoutingProviderRow{}}
	vrows, err := l.db.QueryContext(ctx, `
		SELECT auto_variant, COUNT(*) AS n
		FROM usage_events
		WHERE ts >= ? AND ts < ? AND auto_variant != ''
		GROUP BY auto_variant
		ORDER BY n DESC`,
		start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return out, fmt.Errorf("ledger: auto-routing variants: %w", err)
	}
	for vrows.Next() {
		var variant string
		var n int
		if err := vrows.Scan(&variant, &n); err != nil {
			vrows.Close()
			return out, fmt.Errorf("ledger: auto-routing variants: %w", err)
		}
		out.Variants[variant] = n
		out.TotalRequests += n
	}
	if err := vrows.Err(); err != nil {
		vrows.Close()
		return out, fmt.Errorf("ledger: auto-routing variants: %w", err)
	}
	vrows.Close()
	prows, err := l.db.QueryContext(ctx, `
		SELECT provider, COUNT(*) AS n
		FROM usage_events
		WHERE ts >= ? AND ts < ? AND auto_variant != ''
		GROUP BY provider
		ORDER BY n DESC
		LIMIT 10`,
		start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return out, fmt.Errorf("ledger: auto-routing providers: %w", err)
	}
	defer prows.Close()
	for prows.Next() {
		var p AutoRoutingProviderRow
		if err := prows.Scan(&p.Provider, &p.Requests); err != nil {
			return out, fmt.Errorf("ledger: auto-routing providers: %w", err)
		}
		out.TopProviders = append(out.TopProviders, p)
	}
	if err := prows.Err(); err != nil {
		return out, fmt.Errorf("ledger: auto-routing providers: %w", err)
	}
	return out, nil
}

// ProviderUsage is one provider's totals inside a UsageSummary.
type ProviderUsage struct {
	Provider string `json:"provider"`
	Requests int    `json:"requests"`
	OK       int    `json:"ok"`
	// Errors counts failed attempts EXCLUDING cancelled ones
	// (hedge losers); Cancelled counts the cancelled attempts.
	Errors           int `json:"errors"`
	Cancelled        int `json:"cancelled"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// CachedTokens is the provider-reported cached input tokens in
	// this group; RequestsWithCachedTokens how many of its requests
	// reported any cached tokens at all.
	CachedTokens             int `json:"cached_tokens"`
	RequestsWithCachedTokens int `json:"requests_with_cached_tokens"`
	// ReasoningTokens is the provider-reported reasoning tokens
	// in this group (a subset of CompletionTokens); CacheWriteTokens
	// the provider-reported cache-write tokens (a subset of
	// PromptTokens); CacheWriteTokens5m / CacheWriteTokens1h
	// (M6.9) split those writes by TTL tier — both 0 with a
	// positive CacheWriteTokens means the tier split was never
	// reported for the group. LocalEstimateTokens the prompt tokens that
	// are local tiktoken estimates, not provider-reported counts,
	// and RequestsWithLocalEstimate how many requests carry one.
	// PromptTokens includes those estimates — subtract
	// LocalEstimateTokens to get the provider-reported prompt
	// total for this group.
	ReasoningTokens           int     `json:"reasoning_tokens"`
	CacheWriteTokens          int     `json:"cache_write_tokens"`
	CacheWriteTokens5m        int     `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h        int     `json:"cache_write_tokens_1h"`
	LocalEstimateTokens       int     `json:"local_estimate_tokens"`
	RequestsWithLocalEstimate int     `json:"requests_with_local_estimate"`
	CostUSD                   float64 `json:"cost_usd"`
}

// ModelUsage is one model's totals inside a UsageSummary.
type ModelUsage struct {
	Model            string `json:"model"`
	Provider         string `json:"provider"`
	Requests         int    `json:"requests"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	// CachedTokens / RequestsWithCachedTokens: same meaning as on
	// ProviderUsage, for this (provider, model) combo — the
	// per-combo cache aggregate the Overview renders.
	CachedTokens             int `json:"cached_tokens"`
	RequestsWithCachedTokens int `json:"requests_with_cached_tokens"`
	// ReasoningTokens / CacheWriteTokens / LocalEstimateTokens:
	// same meaning as on ProviderUsage, for this combo.
	// CacheWriteTokens5m / CacheWriteTokens1h (M6.9) split the
	// combo's cache writes by TTL tier.
	ReasoningTokens           int     `json:"reasoning_tokens"`
	CacheWriteTokens          int     `json:"cache_write_tokens"`
	CacheWriteTokens5m        int     `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h        int     `json:"cache_write_tokens_1h"`
	LocalEstimateTokens       int     `json:"local_estimate_tokens"`
	RequestsWithLocalEstimate int     `json:"requests_with_local_estimate"`
	CostUSD                   float64 `json:"cost_usd"`
}

// KeyUsage is one (gateway key, provider) pair's totals inside
// a UsageSummary (B12: GROUP BY gateway_key_id, provider —
// never merged across providers sharing a key).
//
// CostUSD is the pair's PRICED spend only: SUM(cost_usd) over the
// key's rows in the window. A row with no metered price records
// cost 0, so it contributes $0 here — priced spend is a LOWER
// BOUND on the key's true spend whenever UnpricedRequests > 0,
// never a total. UnpricedRequests counts exactly the rows whose
// cost is unknown: successful, non-shared rows with
// price_known = 0, counted per row from the stored flag. Failed
// rows spent nothing (zero tokens, zero cost), so their unknown
// price is irrelevant to cost and they are not counted;
// shared (coalesced-waiter) rows likewise record zero, the
// leader's row carrying the real usage. Because this count is
// per row, it is exact for the (key, provider) pair — unlike the global
// priced/unpriced split in internal/server (analyticsSplitFor),
// which taints a whole (provider, model) group on one unpriced
// row and is therefore an upper bound. A (key, provider) pair with
// no rows in the window has no KeyUsage row at all: it is absent, never
// zero-filled — a zero row would invent spend data for a pair
// that spent nothing.
type KeyUsage struct {
	GatewayKeyID string `json:"gateway_key_id"`
	// Provider splits one key's spend per provider (B12): a key
	// routing to two providers yields two KeyUsage rows, never
	// one merged row — usage/cost are per (key, provider).
	Provider         string  `json:"provider"`
	Requests         int     `json:"requests"`
	OK               int     `json:"ok"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	UnpricedRequests int     `json:"unpriced_requests"`
}

// UsageSummary is the range-limited roll-up for one window [since, now).
// UnknownSplitTokens counts tokens from rows whose prompt/completion
// split was never reported (SplitKnown false): only their total is
// meaningful, and their cost is 0 — they are reported separately so
// a reader never mistakes a total for a split.
// CachedTokens is the provider-reported cached input tokens in the
// window; RequestsWithCachedTokens how many requests reported any.
// Both count only what providers reported — never an estimate.
type UsageSummary struct {
	Requests int `json:"requests"`
	OK       int `json:"ok"`
	// Errors counts failed attempts EXCLUDING cancelled ones
	// (hedge losers): a cancelled attempt is not a failure.
	// Cancelled counts them separately, so Requests ==
	// OK + Errors + Cancelled holds for every window.
	Errors                   int `json:"errors"`
	Cancelled                int `json:"cancelled"`
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	TotalTokens              int `json:"total_tokens"`
	CachedTokens             int `json:"cached_tokens"`
	RequestsWithCachedTokens int `json:"requests_with_cached_tokens"`
	// ReasoningTokens / CacheWriteTokens: window totals of the
	// provider-reported splits (subsets of completion / prompt).
	// CacheWriteTokens5m / CacheWriteTokens1h (M6.9) split the
	// window's cache writes by TTL tier; both 0 with a positive
	// CacheWriteTokens means the tier split was never reported
	// for the window — tier unknown, never guessed.
	// LocalEstimateTokens / RequestsWithLocalEstimate: how much
	// of PromptTokens is a local tiktoken estimate rather than a
	// provider-reported count — subtract it for the reported
	// prompt total. UnknownSplitTokens still includes estimate
	// rows (their split is unknown); the estimate fields say how
	// much of it is estimated.
	ReasoningTokens           int `json:"reasoning_tokens"`
	CacheWriteTokens          int `json:"cache_write_tokens"`
	CacheWriteTokens5m        int `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h        int `json:"cache_write_tokens_1h"`
	LocalEstimateTokens       int `json:"local_estimate_tokens"`
	RequestsWithLocalEstimate int `json:"requests_with_local_estimate"`
	// CompressionRequests / CompressionTokensSaved are the
	// window totals of the lossless-first compression record
	// (CHECKLIST 183): how many successful attempts carried a
	// compression saving, and how many local-estimate prompt
	// tokens compression saved across them. Saved tokens are
	// local estimates, never provider-reported; failed and
	// shared rows carry none, so each request is counted once.
	CompressionRequests    int `json:"compression_requests"`
	CompressionTokensSaved int `json:"compression_tokens_saved"`
	// CacheHitRequests / CacheTokensSaved are the window totals
	// of the own-cache record (B4-16, Area 11 cost-saved row):
	// how many successful attempts were served from the
	// router's own exact/semantic cache, and how many tokens
	// those hits saved vs. a full upstream call. Saved tokens
	// are caller-measured, never derived; failed and shared
	// rows carry none, so each request is counted once.
	CacheHitRequests   int             `json:"cache_hit_requests"`
	CacheTokensSaved   int             `json:"cache_tokens_saved"`
	UnknownSplitTokens int             `json:"unknown_split_tokens"`
	CostUSD            float64         `json:"cost_usd"`
	MeanLatencyMS      float64         `json:"mean_latency_ms"`
	ByProvider         []ProviderUsage `json:"by_provider"`
	ByModel            []ModelUsage    `json:"by_model"`
	// ByKey is the per-gateway-key roll-up (see KeyUsage): one
	// row per key with at least one usage row in the window,
	// priced spend plus the exact per-row unpriced count. Keys
	// with no rows are absent, never zero-filled.
	ByKey []KeyUsage `json:"by_key"`
	// Drift is the window's estimate-vs-reported token-count
	// drift per (provider, model) (CHECKLIST 229-230), from
	// live ledger rows that carry BOTH a provider-reported
	// prompt count and a local estimate (see DriftSummary).
	// Empty before any compared request — never invented.
	Drift []DriftRow `json:"drift"`
}

// DriftRow is one (provider, model) cell of estimate-vs-reported
// token-count drift (CHECKLIST 229-230): how many requests in the
// window carried BOTH a provider-reported prompt count and a local
// estimate (Compared), and how far the local estimate sat from the
// reported count, in percent of the reported count. MeanErrPct is
// the signed mean — negative means the local estimate
// systematically UNDERCOUNTS what the provider bills (the
// dangerous direction for a fail-closed budget); MeanAbsErrPct and
// MaxAbsErrPct are magnitude and worst case. A cell appears only
// when at least one compared request exists in the window.
type DriftRow struct {
	Provider      string  `json:"provider"`
	Model         string  `json:"model"`
	Compared      int     `json:"compared"`
	MeanErrPct    float64 `json:"mean_error_pct"`
	MeanAbsErrPct float64 `json:"mean_abs_error_pct"`
	MaxAbsErrPct  float64 `json:"max_abs_error_pct"`
}

// DriftSummary aggregates estimate-vs-reported prompt-token drift
// per (provider, model) over [since, until) (until zero means
// "now"). Only rows that carry both sides of the comparison are
// counted: a known split (tokens_split_known = 1), a positive
// provider-reported prompt count, a positive local estimate, and
// not a shared row (a shared row's tokens are zero by design —
// the leader's row carries the real usage). Rows recorded before
// the estimate column existed have a 0 estimate and are excluded
// honestly, never treated as perfect agreement. An empty window
// returns an empty slice, never nil, so the dashboard renders an
// honest empty state.
func (l *Ledger) DriftSummary(ctx context.Context, since, until time.Time) ([]DriftRow, error) {
	out := []DriftRow{}
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	untilClause := ""
	args := []any{sinceMs}
	if !until.IsZero() {
		untilClause = " AND ts < ?"
		args = append(args, untilMs)
	}
	rows, err := l.db.QueryContext(ctx, `
		SELECT provider, model, COUNT(*),
		       COALESCE(AVG(100.0 * (local_estimate_prompt_tokens - prompt_tokens) / prompt_tokens), 0),
		       COALESCE(AVG(ABS(100.0 * (local_estimate_prompt_tokens - prompt_tokens) / prompt_tokens)), 0),
		       COALESCE(MAX(ABS(100.0 * (local_estimate_prompt_tokens - prompt_tokens) / prompt_tokens)), 0)
		FROM usage_events
		WHERE ts >= ?`+untilClause+`
		  AND tokens_split_known = 1
		  AND prompt_tokens > 0
		  AND local_estimate_prompt_tokens > 0
		  AND shared = 0
		GROUP BY provider, model ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return out, fmt.Errorf("ledger: drift summary: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d DriftRow
		if err := rows.Scan(&d.Provider, &d.Model, &d.Compared,
			&d.MeanErrPct, &d.MeanAbsErrPct, &d.MaxAbsErrPct); err != nil {
			return out, fmt.Errorf("ledger: drift summary: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("ledger: drift summary: %w", err)
	}
	return out, nil
}

// Summary aggregates usage_events in [since, until): totals plus
// per-provider and per-model breakdowns. until zero means "now".
// Empty window, empty summary.
func (l *Ledger) Summary(ctx context.Context, since, until time.Time) (UsageSummary, error) {
	var s UsageSummary
	s.ByProvider = []ProviderUsage{}
	s.ByModel = []ModelUsage{}
	s.ByKey = []KeyUsage{}
	s.Drift = []DriftRow{}
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()
	untilClause := ""
	args := []any{sinceMs}
	if !until.IsZero() {
		untilClause = " AND ts < ?"
		args = append(args, untilMs)
	}
	err := l.db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cancelled = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cached_tokens), 0),
		       COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(reasoning_tokens), 0),
		       COALESCE(SUM(cache_write_tokens), 0),
		       COALESCE(SUM(cache_write_tokens_5m), 0),
		       COALESCE(SUM(cache_write_tokens_1h), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN prompt_tokens ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 1 AND compression_saved_tokens > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN compression_saved_tokens ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 1 AND cache_tokens_saved > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN cache_tokens_saved ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN tokens_split_known = 0 THEN total_tokens ELSE 0 END), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(AVG(latency_ms), 0)
		FROM usage_events WHERE ts >= ?`+untilClause, args...).Scan(
		&s.Requests, &s.OK, &s.Errors, &s.Cancelled, &s.PromptTokens, &s.CompletionTokens,
		&s.TotalTokens, &s.CachedTokens, &s.RequestsWithCachedTokens,
		&s.ReasoningTokens, &s.CacheWriteTokens, &s.CacheWriteTokens5m, &s.CacheWriteTokens1h, &s.LocalEstimateTokens,
		&s.RequestsWithLocalEstimate,
		&s.CompressionRequests, &s.CompressionTokensSaved,
		&s.CacheHitRequests, &s.CacheTokensSaved,
		&s.UnknownSplitTokens, &s.CostUSD, &s.MeanLatencyMS)
	if err != nil {
		return s, fmt.Errorf("ledger: summary: %w", err)
	}
	provRows, err := l.db.QueryContext(ctx, `
		SELECT provider, COUNT(*),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN ok = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN cancelled = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cached_tokens), 0),
		       COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(reasoning_tokens), 0),
		       COALESCE(SUM(cache_write_tokens), 0),
		       COALESCE(SUM(cache_write_tokens_5m), 0),
		       COALESCE(SUM(cache_write_tokens_1h), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN prompt_tokens ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM usage_events WHERE ts >= ?`+untilClause+`
		GROUP BY provider ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return s, fmt.Errorf("ledger: summary by provider: %w", err)
	}
	for provRows.Next() {
		var p ProviderUsage
		if err := provRows.Scan(&p.Provider, &p.Requests, &p.OK, &p.Errors, &p.Cancelled,
			&p.PromptTokens, &p.CompletionTokens, &p.TotalTokens,
			&p.CachedTokens, &p.RequestsWithCachedTokens,
			&p.ReasoningTokens, &p.CacheWriteTokens, &p.CacheWriteTokens5m, &p.CacheWriteTokens1h, &p.LocalEstimateTokens,
			&p.RequestsWithLocalEstimate, &p.CostUSD); err != nil {
			provRows.Close()
			return s, fmt.Errorf("ledger: summary by provider: %w", err)
		}
		s.ByProvider = append(s.ByProvider, p)
	}
	if err := provRows.Err(); err != nil {
		provRows.Close()
		return s, fmt.Errorf("ledger: summary by provider: %w", err)
	}
	provRows.Close()
	modelRows, err := l.db.QueryContext(ctx, `
		SELECT model, provider, COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cached_tokens), 0),
		       COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(reasoning_tokens), 0),
		       COALESCE(SUM(cache_write_tokens), 0),
		       COALESCE(SUM(cache_write_tokens_5m), 0),
		       COALESCE(SUM(cache_write_tokens_1h), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN prompt_tokens ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN local_estimate = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(cost_usd), 0)
		FROM usage_events WHERE ts >= ?`+untilClause+`
		GROUP BY model, provider ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return s, fmt.Errorf("ledger: summary by model: %w", err)
	}
	for modelRows.Next() {
		var m ModelUsage
		if err := modelRows.Scan(&m.Model, &m.Provider, &m.Requests,
			&m.PromptTokens, &m.CompletionTokens, &m.TotalTokens,
			&m.CachedTokens, &m.RequestsWithCachedTokens,
			&m.ReasoningTokens, &m.CacheWriteTokens, &m.CacheWriteTokens5m, &m.CacheWriteTokens1h, &m.LocalEstimateTokens,
			&m.RequestsWithLocalEstimate, &m.CostUSD); err != nil {
			modelRows.Close()
			return s, fmt.Errorf("ledger: summary by model: %w", err)
		}
		s.ByModel = append(s.ByModel, m)
	}
	if err := modelRows.Err(); err != nil {
		modelRows.Close()
		return s, fmt.Errorf("ledger: summary by model: %w", err)
	}
	modelRows.Close()
	// Per-(gateway-key, provider) roll-up (Cost Explorer key
	// dimension, B12): GROUP BY gateway_key_id, provider — one
	// key routing to two providers yields two rows with
	// per-provider totals, never one merged row. A (key,
	// provider) pair appears only if it has at least one row
	// here, so a pair with no spend in the window is absent,
	// never zero-filled. CostUSD is SUM(cost_usd): priced spend
	// only (see KeyUsage for the fail-closed unpriced policy).
	keyRows, err := l.db.QueryContext(ctx, `
		SELECT gateway_key_id, provider, COUNT(*),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(SUM(CASE WHEN ok = 1 AND shared = 0 AND price_known = 0 THEN 1 ELSE 0 END), 0)
		FROM usage_events WHERE ts >= ?`+untilClause+`
		GROUP BY gateway_key_id, provider ORDER BY COUNT(*) DESC`, args...)
	if err != nil {
		return s, fmt.Errorf("ledger: summary by key: %w", err)
	}
	for keyRows.Next() {
		var k KeyUsage
		if err := keyRows.Scan(&k.GatewayKeyID, &k.Provider, &k.Requests, &k.OK,
			&k.PromptTokens, &k.CompletionTokens, &k.TotalTokens,
			&k.CostUSD, &k.UnpricedRequests); err != nil {
			keyRows.Close()
			return s, fmt.Errorf("ledger: summary by key: %w", err)
		}
		s.ByKey = append(s.ByKey, k)
	}
	if err := keyRows.Err(); err != nil {
		keyRows.Close()
		return s, fmt.Errorf("ledger: summary by key: %w", err)
	}
	keyRows.Close()
	// Estimate-vs-reported drift (CHECKLIST 229-230): the same
	// window, per (provider, model), from rows that carry both
	// sides of the comparison.
	drift, err := l.DriftSummary(ctx, since, until)
	if err != nil {
		return s, err
	}
	s.Drift = drift
	return s, nil
}

// CachedTokenFraction returns the measured cached-token fraction
// for one (provider, model) pair over [since, now):
// provider-reported cached prompt tokens over provider-reported
// prompt tokens, from successful rows with a known token split.
// ok=false when the pair has no such rows in the window — no
// measurement, never a guessed 0 (a pair that never cached reads
// as "unknown", and a pair whose rows all failed or carry no
// split likewise). The fraction is clamped to 0..1; the ledger
// already clamps stored cached tokens to the prompt count, so a
// value above 1 would only come from corrupt rows. Read-only: a
// single aggregate SELECT, no schema change, no writes. A zero
// since means "all time". This is the warmth signal the
// cache-optimized strategy orders on (M6.5-wiring): the caller
// (production factory install) closes over it with a recency
// window — cache warmth decays with the provider TTLs, so a
// stale all-time fraction would rank a long-cold pair as warm.
func (l *Ledger) CachedTokenFraction(ctx context.Context, provider, model string, since time.Time) (fraction float64, ok bool, err error) {
	var cached, prompt int64
	err = l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cached_tokens), 0), COALESCE(SUM(prompt_tokens), 0)
		FROM usage_events
		WHERE provider = ? AND model = ? AND ts >= ? AND ok = 1 AND tokens_split_known = 1`,
		provider, model, since.UnixMilli()).Scan(&cached, &prompt)
	if err != nil {
		return 0, false, fmt.Errorf("ledger: cached token fraction: %w", err)
	}
	if prompt <= 0 {
		return 0, false, nil
	}
	fraction = float64(cached) / float64(prompt)
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	return fraction, true, nil
}

// TimeBucket is one traffic-chart bucket: requests, errors and mean
// latency inside [Start, Start+Width). MeanLatencyMS is 0 when the
// bucket holds no requests.
type TimeBucket struct {
	Start         time.Time `json:"start"`
	Requests      int       `json:"requests"`
	Errors        int       `json:"errors"`
	MeanLatencyMS float64   `json:"mean_latency_ms"`
	CostUSD       float64   `json:"cost_usd"`
	Tokens        int       `json:"tokens"`
}

// Buckets aggregates usage_events in [since, now) into consecutive
// buckets of width. The returned slice always covers the whole
// window (empty buckets included, so the chart never invents a
// gap-fill — a bucket with zero requests is really zero).
func (l *Ledger) Buckets(ctx context.Context, since time.Time, width time.Duration) ([]TimeBucket, error) {
	if width <= 0 {
		return nil, fmt.Errorf("ledger: buckets: width must be positive")
	}
	now := time.Now()
	// Whole buckets covering [since, now): the count is the floor
	// of span/width, so a 24h window with 1h buckets is exactly 24
	// buckets ending at now — never a spurious near-empty future
	// bucket. (A row recorded after `now` here falls past the last
	// bucket and is dropped by the bounds check below; in practice
	// that window is sub-millisecond.)
	n := int(now.Sub(since) / width)
	if n < 1 {
		n = 1
	}
	if n > 366 {
		n = 366
	}
	out := make([]TimeBucket, n)
	for i := range out {
		out[i].Start = since.Add(time.Duration(i) * width).UTC()
	}
	widthMs := width.Milliseconds()
	sinceMs := since.UnixMilli()
	rows, err := l.db.QueryContext(ctx, `
		SELECT CAST((ts - ?) / ? AS INTEGER) AS b, COUNT(*),
		       COALESCE(SUM(CASE WHEN ok = 0 AND cancelled = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(AVG(latency_ms), 0),
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(SUM(total_tokens), 0)
		FROM usage_events WHERE ts >= ?
		GROUP BY b ORDER BY b`, sinceMs, widthMs, sinceMs)
	if err != nil {
		return nil, fmt.Errorf("ledger: buckets: %w", err)
	}
	defer rows.Close()
	// Group results are ACCUMULATED into the bucket, never assigned:
	// the clamp above can map an out-of-range group onto the last
	// bucket, which may already hold its own native group — assigning
	// would silently discard the native group's rows. (Found live:
	// rows recorded in the same millisecond as `now` get b == n and
	// clamp onto the last bucket, overwriting the rows recorded a
	// millisecond earlier; TestDashboardLiveFeeds failed ~25% of runs
	// with the first seeded row missing from the last bucket.)
	// MeanLatencyMS is a request-weighted mean across merged groups.
	latSum := make([]float64, n)
	for rows.Next() {
		var b, count, errors, tokens int
		var meanLat, cost float64
		if err := rows.Scan(&b, &count, &errors, &meanLat, &cost, &tokens); err != nil {
			return nil, fmt.Errorf("ledger: buckets: %w", err)
		}
		if b < 0 || b >= n {
			// A row a millisecond past the nominal window end
			// (same-millisecond truncation at the `now` edge)
			// belongs to the last bucket for display — the row is
			// real, it only straddles the edge. Clamp instead of
			// dropping, so a request served a millisecond ago can
			// never vanish from the chart. A row before `since`
			// cannot occur (the query filters ts >= since).
			if b < 0 {
				continue
			}
			b = n - 1
		}
		out[b].Requests += count
		out[b].Errors += errors
		latSum[b] += meanLat * float64(count)
		out[b].CostUSD += cost
		out[b].Tokens += tokens
	}
	for i := range out {
		if out[i].Requests > 0 {
			out[i].MeanLatencyMS = latSum[i] / float64(out[i].Requests)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: buckets: %w", err)
	}
	return out, nil
}

// WipeResult is what Ledger.Wipe removed: usage-event rows deleted
// and budget rows whose running spend was reset to zero.
type WipeResult struct {
	UsageEvents  int64
	BudgetsReset int64
}

// Wipe deletes every usage event and resets every budget's running
// spend to zero. Budget CAPS and periods are preserved — they are
// operator configuration, not history — and the tables themselves
// (and the database file) stay in place, empty. There is no undo:
// callers must gate this behind the operator-credential boundary
// (POST /admin/db/wipe) and say so in the response. A caller with
// queued rows in an AsyncWriter must Flush() first, or the flush
// will resurrect rows after the wipe.
func (l *Ledger) Wipe(ctx context.Context) (WipeResult, error) {
	var out WipeResult
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("ledger: wipe begin tx: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM usage_events`)
	if err != nil {
		return out, fmt.Errorf("ledger: wipe usage events: %w", err)
	}
	if out.UsageEvents, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("ledger: wipe usage events count: %w", err)
	}
	res, err = tx.ExecContext(ctx, `UPDATE budgets SET spent_usd = 0`)
	if err != nil {
		return out, fmt.Errorf("ledger: wipe budget spend: %w", err)
	}
	if out.BudgetsReset, err = res.RowsAffected(); err != nil {
		return out, fmt.Errorf("ledger: wipe budget spend count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return out, fmt.Errorf("ledger: wipe commit: %w", err)
	}
	return out, nil
}

// ProviderKeySpend is one (provider, provider-key) pair's honest
// ledger totals (CHECKLIST 175): every row the key served,
// attributed to that key alone — never pooled into the provider's
// total. Requests counts every attempt row (ok and failed);
// Successes counts the ok rows; Tokens/CostUSD sum every row's
// tokens/cost — the same definition SpendFor uses (a failed row
// that still carries tokens still cost them; in practice failed
// attempts arrive as Usage{} with zero tokens, so they contribute
// 0). LastUsed is the newest
// row's timestamp (nil when the key has no rows yet — it exists
// in the pool but has never served). CostUSD is the priced spend
// where prices were configured, $0 where unknown — a lower bound,
// never exact billing (see ledger.Cost). Rows recorded before key
// pooling (provider_key_id ”) are NOT attributed to any key:
// they group under the empty key ID, and the dashboard shows them
// as unattributed history, never as one key's spend.
type ProviderKeySpend struct {
	Provider      string     `json:"provider"`
	ProviderKeyID string     `json:"provider_key_id"`
	Requests      int        `json:"requests"`
	Successes     int        `json:"successes"`
	Tokens        int        `json:"tokens"`
	CostUSD       float64    `json:"cost_usd"`
	LastUsed      *time.Time `json:"last_used"`
}

// ProviderKeySpendTotals returns the per-(provider, provider-key)
// ledger totals for every pair with at least one row, ordered by
// provider then key ID. A key with no rows yet is absent — the
// dashboard reports "no usage yet" for it from the pool list,
// never from this feed.
func (l *Ledger) ProviderKeySpendTotals(ctx context.Context) ([]ProviderKeySpend, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT provider, provider_key_id,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN ok = 1 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       MAX(ts)
		FROM usage_events
		GROUP BY provider, provider_key_id
		ORDER BY provider, provider_key_id`)
	if err != nil {
		return nil, fmt.Errorf("ledger: provider key spend: %w", err)
	}
	defer rows.Close()
	out := []ProviderKeySpend{}
	for rows.Next() {
		var sp ProviderKeySpend
		var lastMs sql.NullInt64
		if err := rows.Scan(&sp.Provider, &sp.ProviderKeyID, &sp.Requests, &sp.Successes, &sp.Tokens, &sp.CostUSD, &lastMs); err != nil {
			return nil, fmt.Errorf("ledger: provider key spend scan: %w", err)
		}
		if lastMs.Valid {
			t := time.UnixMilli(lastMs.Int64).UTC()
			sp.LastUsed = &t
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: provider key spend rows: %w", err)
	}
	return out, nil
}

// SpendFor returns the total USD recorded for keyID since the given
// time (inclusive). It sums usage_events, the source of truth;
// budgets.spent_usd is a cached running total kept in sync by
// RecordUsage.
func (l *Ledger) SpendFor(ctx context.Context, keyID string, since time.Time) (float64, error) {
	var total sql.NullFloat64
	err := l.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cost_usd), 0) FROM usage_events
		WHERE gateway_key_id = ? AND ts >= ?`,
		keyID, since.UnixMilli()).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("ledger: spend for key: %w", err)
	}
	return total.Float64, nil
}

// Budget is one key's cap and running spend.
type Budget struct {
	KeyID    string
	CapUSD   float64
	SpentUSD float64
	Period   string
}

// SetBudget creates or updates the budget cap for a key. An existing
// spent_usd total is preserved when the cap changes; period is an
// opaque label (e.g. "monthly", "2026-11") used by callers to decide
// when to reset.
func (l *Ledger) SetBudget(ctx context.Context, keyID string, capUSD float64, period string) error {
	_, err := l.db.ExecContext(ctx, `
		INSERT INTO budgets (key_id, cap_usd, spent_usd, period)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(key_id) DO UPDATE SET cap_usd = excluded.cap_usd,
		                                   period = excluded.period`,
		keyID, capUSD, period)
	if err != nil {
		return fmt.Errorf("ledger: set budget: %w", err)
	}
	invalidateBudgetMemo(ctx, keyID)
	return nil
}

// GetBudget returns the budget row for a key, or sql.ErrNoRows when
// the key has no budget (unlimited). It runs on the prepared
// statement (see preparedGetBudget): this is the hottest read in
// the router — twice per request before the fix in budget.go —
// and re-preparing its SQL on every call was 3.49 s of cumulated
// _sqlite3Prepare in the 2026-11-18 CPU profile.
//
// When ctx carries a per-request memo (WithBudgetMemo, CHECKLIST
// 299-300), the row — or its absence — is read from the database
// once per request per key and served from the memo after that.
// Only a successful read or a sql.ErrNoRows miss is memoised;
// any other database error is returned uncached. See
// budget_memo.go for the honest scope note: on today's hot paths
// the guard reads once per request, so the memo's saving
// materialises only when a request hits the guard twice.
func (l *Ledger) GetBudget(ctx context.Context, keyID string) (Budget, error) {
	return l.getBudgetMemoised(ctx, keyID)
}

// getBudgetUncached is the database read behind GetBudget: the
// prepared statement when one is cached for this connection,
// else a direct query. It knows nothing about the memo.
func (l *Ledger) getBudgetUncached(ctx context.Context, keyID string) (Budget, error) {
	var b Budget
	if stmt, ok := l.preparedGetBudget(ctx); ok {
		err := stmt.QueryRowContext(ctx, keyID).Scan(&b.KeyID, &b.CapUSD, &b.SpentUSD, &b.Period)
		if err != nil {
			return Budget{}, err
		}
		return b, nil
	}
	err := l.db.QueryRowContext(ctx, `
		SELECT key_id, cap_usd, spent_usd, period FROM budgets
		WHERE key_id = ?`, keyID).Scan(&b.KeyID, &b.CapUSD, &b.SpentUSD, &b.Period)
	if err != nil {
		return Budget{}, err
	}
	return b, nil
}

// ResetSpent zeroes the running spend for a key (e.g. at a period
// rollover). It does not touch usage_events history.
func (l *Ledger) ResetSpent(ctx context.Context, keyID string) error {
	_, err := l.db.ExecContext(ctx, `
		UPDATE budgets SET spent_usd = 0 WHERE key_id = ?`, keyID)
	if err != nil {
		return fmt.Errorf("ledger: reset spend: %w", err)
	}
	invalidateBudgetMemo(ctx, keyID)
	return nil
}

// CheckBudget reports whether keyID may spend more, and how much
// budget remains (cap - spent).
//
//   - No budget row: unlimited. allowed=true, remaining=+Inf.
//   - spent < cap: allowed=true, remaining = cap - spent.
//   - spent >= cap: allowed depends on FailClosed. With FailClosed on
//     ("never bill me"), allowed=false — including a cap of 0, where
//     any spend would exceed the cap. With FailClosed off, the request
//     is still allowed (soft cap) but remaining is <= 0, signalling
//     the caller to warn. Use CheckBudgetOrError (budget.go) for the
//     typed refusal plus the 80% soft warning.
func (l *Ledger) CheckBudget(ctx context.Context, keyID string) (allowed bool, remaining float64, err error) {
	allowed, remaining, _, _, err = l.checkBudgetDetailed(ctx, keyID)
	return allowed, remaining, err
}

// ensureQuotaPoolColumns adds the M5.5 per-account pool column to
// quota_pools when it is missing (databases created by an older
// build). Same pattern as ensureKeyBudgetColumns: PRAGMA table_info
// first, idempotent. A pre-existing row gains account_count 1 —
// the M5.4 behaviour (the dimension limit was already the pool
// limit) is preserved exactly.
func ensureQuotaPoolColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(quota_pools)`)
	if err != nil {
		return fmt.Errorf("ledger: inspect quota_pools schema: %w", err)
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("ledger: inspect quota_pools schema: %w", err)
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: inspect quota_pools schema: %w", err)
	}
	rows.Close()
	if present["account_count"] {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE quota_pools ADD COLUMN account_count INTEGER NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("ledger: add column account_count: %w", err)
	}
	return nil
}
