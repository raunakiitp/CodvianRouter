// Phase D, builder D4: quota-share engine use, 4-tier cascade and
// endpoint-specific combo projection.
//
// Why this file exists instead of calling internal/strategy: the
// router package CANNOT import internal/strategy — that would be
// an import cycle (strategy/wiring.go imports router, and
// router.go's Config comment says so explicitly: "the router
// itself never imports the strategy package (that would be a
// cycle), so strategy names are opaque strings here"). Cross-package
// data therefore arrives the same way every other live signal
// does: through Config funcs (QuotaHint, RegionFor, SelfHosted)
// and process-wide suppliers (autodispatch.go). So:
//
//   - Quota-share: the engine runs its own DRR (quotaShareDRR)
//     with semantics mirroring strategy.SelectQuotaShare's
//     applyDRR exactly (quantum = share/total, winner pays a unit
//     cost of 1, ties keep input order — the 2:1 sequence A,B,A,A,
//     B,A and the return-to-zero invariant are pinned by
//     TestQuotaShareDRRMatchesReferenceSequence against the
//     strategy package's hand-computed table). The DRR state is
//     ONE pool per chain signature, stored on the Engine under
//     e.mu (see orderQuotaShare below), following the
//     DefaultChainStates precedent (one state per chain, keyed by
//     the declared provider/model list). Shares arrive through
//     Config.QuotaShareFor; nil means share 1 for every member.
//     Saturation gating and P2C mirror SelectQuotaShare steps
//     1b and 3 (G19 Area 1): members at/over their adaptive
//     concurrency cap (in-flight >= live limit) are deprioritised
//     to the tail — fail-open when all are at cap — and between
//     the top two DRR candidates the one with fewer in-flight
//     requests wins. Quota-bucket saturation itself is NOT
//     re-gated here: filterZeroQuota already drops zero-hint
//     members upstream (orderAttemptsWithContext), so a saturated
//     quota bucket never reaches the DRR at all. That is a
//     deliberate divergence from the reference, which demotes
//     saturated members to the tail instead of dropping them:
//     a provider known to have no quota left is never dialled,
//     and the drop is reported on the trace (zeroSkipped) rather
//     than silently retried last. In-flight counts are per
//     provider (ConcurrencyStats), coarser than the reference's
//     per-connection counts — two keys of one provider share one
//     count.
//   - 4-tier cascade: applyCascadeOrder implements the
//     subscription -> api-key -> cheap -> free order of
//     autorouter.OrderCascade for an attempt list. It IS wired
//     now (G19 Area 1): orderAttemptsWithContext applies it when
//     the effective strategy is "cascade" (the internal strategy
//     name, following the quota-share precedent — the engine
//     intercepts it before the StrategyHook, so no config
//     allowlist entry is needed for the wiring itself). The tier
//     for each attempt comes from the live auto catalog
//     (cascadeTierFn, via SetAutoCatalog): the connection's
//     billing class and the model's economic tier per
//     (provider, model), classified by autorouter.CascadeTierFor.
//     A pair with no catalog entry — or no supplier at all —
//     has no known tier: such attempts keep their relative order
//     AFTER every tiered attempt, never dropped, never assigned
//     an invented tier. The config-file surface landed with
//     S1-B2-542-SELECTOR: routing.global_strategy naming
//     "cascade" is accepted by the config allowlist
//     (internal/config/config.go isStrategyName) and by
//     strategy.Parse (internal/strategy StrategyCascade in All),
//     so the cascade is selectable wherever a strategy string
//     is validated — config file, dashboard picker, MCP. The
//     thrifty ladder remains the escalation path for
//     auto/thrifty. OrderCascade itself still has zero
//     callers: it projects a catalog (omitting unplaced models),
//     while the engine ordering must not drop attempts, so the
//     two keep their different contracts deliberately.
//   - Endpoint projection: projectStrategyForEndpoint enforces
//     autorouter.ProjectStrategyForEndpoint on the effective
//     strategy. The primitive's contract is VALIDATE-OR-ERROR,
//     not substitute: a disallowed strategy on a non-chat
//     endpoint is an error naming the strategy and the endpoint
//     (the reference combo schema rejects the combo config), so
//     the engine fails the request closed — it never silently
//     substitutes "priority" or any other strategy. The rule
//     governs combo strategies only (autorouter.AllComboStrategies
//     plus "chaos-mode" and the internal "quota-share"): engine
//     mode packs ("balanced", "cost-saver", ...) and unknown names
//     are not combo strategies and pass through untouched. "" (or
//     "chat_completions"/"messages"/"responses") means chat.
package router

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// quotaShareStrategy is the effective-strategy name that selects
// quota-share DRR ordering in orderAttempts.
const quotaShareStrategy = "quota-share"

// quotaSharePool is one pool's DRR deficit state: accumulated
// deficit per member ID (higher = this member is "owed" more
// service). Mirrors strategy.QuotaShareState; the engine owns one
// per chain signature, stored on the Engine under e.mu.
type quotaSharePool struct {
	deficits map[string]float64
}

// quotaSharePoolKey is the pool key for a chain: the declared
// provider/model list, so different chains never share DRR
// deficits. Same shape as strategy.chainSignature (which also
// ignores KeyID: two chains naming the same provider/model list
// are the same pool, and their members are still distinct DRR
// entries because the member ID carries the key).
func quotaSharePoolKey(chain []Attempt) string {
	parts := make([]string, 0, len(chain))
	for _, att := range chain {
		parts = append(parts, att.ProviderID+"/"+att.Model)
	}
	return strings.Join(parts, ",")
}

// quotaShareMemberID is one pool member's DRR key: provider +
// connection + model (the QuotaShareMember.ID contract in
// strategy/quotashare.go).
func quotaShareMemberID(att Attempt) string {
	return ClientKey(att.ProviderID, att.KeyID) + "/" + att.Model
}

// normalizeQuotaShare mirrors strategy.normalizeShare: an explicit
// 0 DISABLES the member relative to weighted siblings; anything
// missing, non-finite or negative defaults to 1.
func normalizeQuotaShare(share float64) float64 {
	if share == 0 {
		return 0
	}
	if share > 0 && !math.IsNaN(share) && !math.IsInf(share, 0) {
		return share
	}
	return 1
}

// quotaShareDRR reorders chain by deficit round-robin and mutates
// deficits: every member gains a quantum equal to its normalized
// share of the total (or 1/n when all shares are 0 — an unweighted
// pool shares evenly); the member with the largest deficit wins
// (ties keep input order) and pays a unit cost of 1, so leftover
// fractional credit carries into the next round. It returns a NEW
// slice with the winner first and the rest in input order — the
// same shape as strategy.applyDRR. shareFn is
// Config.QuotaShareFor (nil means share 1 for every member).
// Saturation and P2C are deliberately absent: the engine has no
// per-member saturation or in-flight signal on this path (see the
// file header).
func quotaShareDRR(chain []Attempt, deficits map[string]float64, shareFn func(providerID, model string) float64) []Attempt {
	out := append([]Attempt(nil), chain...)
	if len(out) <= 1 {
		return out
	}
	shares := make([]float64, len(out))
	total := 0.0
	for i, att := range out {
		share := 1.0
		if shareFn != nil {
			share = shareFn(att.ProviderID, att.Model)
		}
		shares[i] = normalizeQuotaShare(share)
		total += shares[i]
	}
	unweighted := total <= 0
	denom := total
	if unweighted {
		denom = float64(len(out))
	}
	for i, att := range out {
		quantum := 1.0 / denom
		if !unweighted {
			quantum = shares[i] / denom
		}
		deficits[quotaShareMemberID(att)] += quantum
	}
	winner := 0
	for i := 1; i < len(out); i++ {
		if deficits[quotaShareMemberID(out[i])] > deficits[quotaShareMemberID(out[winner])] {
			winner = i
		}
	}
	deficits[quotaShareMemberID(out[winner])] -= 1
	ordered := make([]Attempt, 0, len(out))
	ordered = append(ordered, out[winner])
	for i, att := range out {
		if i != winner {
			ordered = append(ordered, att)
		}
	}
	return ordered
}

// orderQuotaShare applies quota-share DRR ordering to an already
// quota-filtered chain, using the Engine's persistent per-pool
// state (one pool per chain signature, created on first use).
// The state lives on the Engine under e.mu and is mutated under
// that lock: the computation is pure (no provider calls, no hooks),
// so holding e.mu across it cannot deadlock — orderAttempts is
// never called with e.mu held. A chain of 0-1 attempts is
// returned untouched.
//
// Gating mirrors strategy.SelectQuotaShare (G19 Area 1), in the
// same order: (1b) at-cap members — providers whose adaptive
// limiter is at/over its live limit (in-flight >= limit, limit
// > 0) — are deprioritised behind members with headroom, and
// the DRR runs over the with-room set only, so at-cap members
// accrue no deficit while capped; fail-open when every member
// is at cap (or no limiter state exists: no state means no cap,
// never a penalty). (3) P2C between the top two DRR candidates:
// the one with fewer in-flight requests wins; ties keep the DRR
// order. Note the DRR winner is first in the returned order, but
// orderAttempts applies applyTailPartitions afterwards (limiter
// headroom / soft-demote / reserve-last), which may demote it —
// the same treatment every strategy gets, not a quota-share
// special case.
func (e *Engine) orderQuotaShare(chain []Attempt) []Attempt {
	if len(chain) <= 1 {
		return chain
	}
	// In-flight counts are read BEFORE taking e.mu (the limiter
	// owns its own lock; orderAttempts never holds e.mu here).
	inFlight := make(map[string]int, len(chain))
	for _, st := range e.ConcurrencyStats() {
		if st.Limit > 0 {
			inFlight[st.Provider] = st.InFlight
		}
	}
	limits := make(map[string]float64, len(chain))
	for _, st := range e.ConcurrencyStats() {
		if st.Limit > 0 {
			limits[st.Provider] = st.Limit
		}
	}
	atCap := func(att Attempt) bool {
		limit, ok := limits[att.ProviderID]
		return ok && float64(inFlight[att.ProviderID]) >= limit
	}
	withRoom := make([]Attempt, 0, len(chain))
	capped := make([]Attempt, 0, len(chain))
	for _, att := range chain {
		if atCap(att) {
			capped = append(capped, att)
		} else {
			withRoom = append(withRoom, att)
		}
	}
	if len(withRoom) == 0 {
		// Fail-open: every member at cap — the whole pool is
		// eligible again, exactly like SelectQuotaShare.
		withRoom, capped = chain, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	pool := e.quotaShare[quotaSharePoolKey(chain)]
	if pool == nil {
		pool = &quotaSharePool{deficits: make(map[string]float64)}
		e.quotaShare[quotaSharePoolKey(chain)] = pool
	}
	ordered := quotaShareDRR(withRoom, pool.deficits, e.cfg.QuotaShareFor)
	// P2C: fewer in-flight wins between the top two.
	if len(ordered) >= 2 && inFlight[ordered[1].ProviderID] < inFlight[ordered[0].ProviderID] {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	return append(ordered, capped...)
}

// cascadeStrategy is the effective-strategy name that selects
// 4-tier cascade ordering in orderAttempts (G19 Area 1,
// following the quota-share precedent: an internal strategy
// name the engine intercepts before the StrategyHook).
const cascadeStrategy = "cascade"

// cascadeTierFn reads each attempt's cascade tier from the live
// auto catalog (the SetAutoCatalog supplier): the connection's
// billing class and the model's economic tier per (provider,
// model), classified by autorouter.CascadeTierFor. A pair with
// no catalog entry — or no supplier at all — reports unknown,
// so the attempt sorts last without an invented tier. The
// catalog is read once per call, not per attempt. Attempt
// models may carry a "provider/" prefix while catalog models
// are bare ids; the prefix is stripped for the lookup.
func (e *Engine) cascadeTierFn() func(Attempt) (autorouter.CascadeTier, bool) {
	catalog := e.liveAutoCatalog()
	byPair := make(map[string]autorouter.CatalogModel, len(catalog))
	for _, m := range catalog {
		byPair[m.Provider+"\x00"+m.Model] = m
	}
	return func(att Attempt) (autorouter.CascadeTier, bool) {
		model := att.Model
		if prefix := att.ProviderID + "/"; len(model) > len(prefix) && model[:len(prefix)] == prefix {
			model = model[len(prefix):]
		}
		m, ok := byPair[att.ProviderID+"\x00"+model]
		if !ok {
			return "", false
		}
		return autorouter.CascadeTierFor(m)
	}
}

// cascadeTierIndex is the escalation index of a cascade tier, or
// len(autorouter.CascadeOrder) for an unplaced attempt (sorts
// last, never first).
func cascadeTierIndex(tier autorouter.CascadeTier) int {
	for i, t := range autorouter.CascadeOrder {
		if t == tier {
			return i
		}
	}
	return len(autorouter.CascadeOrder)
}

// applyCascadeOrder stably reorders chain by 4-tier cascade tier
// (subscription -> api-key -> cheap -> free), using tierFn to read
// each attempt's tier. tierFn reports false for an attempt with no
// known tier: such attempts keep their relative order AFTER every
// tiered attempt — they are never dropped and never assigned an
// invented tier (autorouter.OrderCascade omits unplaced models
// entirely; an engine ordering must not drop attempts, so they
// sort last instead). A nil tierFn means "no tier data at all":
// the chain is returned unchanged. Wired into the live path by
// orderAttemptsWithContext (router.go): it calls
// applyCascadeOrder over the live auto-catalog tiers
// (cascadeTierFn) after the strategy hook reorders the chain.
// The tier signal is the auto-catalog entry's billing/tier
// classification — subscription/keyless/free from the catalog,
// api-key otherwise — never a guessed tier.
func applyCascadeOrder(chain []Attempt, tierFn func(Attempt) (autorouter.CascadeTier, bool)) []Attempt {
	if tierFn == nil || len(chain) <= 1 {
		return chain
	}
	idx := make([]int, len(chain))
	for i := range idx {
		idx[i] = i
	}
	tierOf := make([]int, len(chain))
	for i, att := range chain {
		tierOf[i] = len(autorouter.CascadeOrder)
		if tier, ok := tierFn(att); ok {
			tierOf[i] = cascadeTierIndex(tier)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return tierOf[idx[a]] < tierOf[idx[b]] })
	out := make([]Attempt, 0, len(chain))
	for _, i := range idx {
		out = append(out, chain[i])
	}
	return out
}

// comboStrategyNames is the strategy vocabulary the endpoint
// projection governs: every combo strategy autorouter knows, plus
// the two combo strategies its AllComboStrategies list does not
// name — "chaos-mode" (a combo execution path, see combo.go) and
// the internal "quota-share" (never a chat-route strategy either,
// per autovariants.go, so it is disallowed off chat like every
// other non-listed combo strategy).
var comboStrategyNames = func() map[string]bool {
	names := make(map[string]bool, len(autorouter.AllComboStrategies)+2)
	for _, s := range autorouter.AllComboStrategies {
		names[s] = true
	}
	names["chaos-mode"] = true
	names["quota-share"] = true
	return names
}()

// endpointForProjection maps a provider endpoint kind onto the
// autorouter endpoint surface: "" (unset), "chat_completions",
// "messages" and "responses" are all the chat surface — a zero
// Endpoint means chat, never an error — while embeddings, rerank,
// images and audio map to their own surfaces. The M8.20 media kinds map
// to theirs too (search, OCR, video, music — the autorouter
// already defines all four surfaces; the mapping is what lets the
// default "priority" strategy, which is a NonChatStrategy, stand
// on a media call instead of failing it closed as unknown). Any
// other kind is a fail-closed error: an unknown surface must never
// silently collapse to chat and let a combo strategy through.
func endpointForProjection(kind provider.EndpointKind) (autorouter.Endpoint, error) {
	switch kind {
	case "", provider.EndpointChatCompletions, provider.EndpointMessages, provider.EndpointResponses, provider.EndpointGeminiGenerateContent:
		return autorouter.EndpointChat, nil
	case provider.EndpointEmbeddings:
		return autorouter.EndpointEmbeddings, nil
	case provider.EndpointRerank:
		return autorouter.EndpointRerank, nil
	case provider.EndpointImages:
		return autorouter.EndpointImages, nil
	case EndpointAudioSpeech:
		return autorouter.EndpointAudioSpeech, nil
	case EndpointAudioTranscriptions:
		return autorouter.EndpointAudioTranscriptions, nil
	case EndpointAudioTranslations:
		return autorouter.EndpointAudioTranslations, nil
	case provider.EndpointSearch:
		return autorouter.EndpointSearch, nil
	case provider.EndpointOCR:
		return autorouter.EndpointOCR, nil
	case provider.EndpointVideo:
		return autorouter.EndpointVideos, nil
	case provider.EndpointMusic:
		return autorouter.EndpointMusic, nil
	default:
		return autorouter.EndpointChat, fmt.Errorf("unknown endpoint kind %q", string(kind))
	}
}

// checkEndpointStrategy is projectStrategyForEndpoint for the
// call sites that only need the verdict: nil when the strategy
// stands on the endpoint, the projection error when it does not.
func checkEndpointStrategy(strategy string, kind provider.EndpointKind) error {
	_, err := projectStrategyForEndpoint(strategy, kind)
	return err
}

// projectStrategyForEndpoint enforces the per-endpoint combo rule
// on an effective strategy: chat allows every strategy; a non-chat
// endpoint allows only autorouter.NonChatStrategies. It returns
// the strategy unchanged when it stands, and the
// ProjectStrategyForEndpoint error when it does not — the
// primitive validates-or-errors, it never substitutes, so a
// disallowed combo strategy fails the request closed. Strategies
// that are not combo strategies (mode packs, unknown names) and
// an empty strategy pass through untouched: the combo schema has
// no opinion on them.
func projectStrategyForEndpoint(strategy string, kind provider.EndpointKind) (string, error) {
	if strings.TrimSpace(strategy) == "" || !comboStrategyNames[strategy] {
		return strategy, nil
	}
	endpoint, err := endpointForProjection(kind)
	if err != nil {
		return "", err
	}
	return autorouter.ProjectStrategyForEndpoint(strategy, endpoint)
}
