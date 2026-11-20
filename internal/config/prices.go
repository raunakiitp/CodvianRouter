// Package config — model price source (see config.go for the rest of
// the package).
//
// A model price is OPERATOR-CONFIGURED data by default, never fetched
// from the network at runtime and never invented by the router. The
// operator writes prices in the config file ("prices" block and
// "provider_prices" block, USD per 1M tokens) or in a separate local
// JSON price file ("price_file"), and the router reads them verbatim.
// No catalog entry, model-list fetch or default table populates
// prices: a model with no configured price is UNKNOWN, not free —
// the scorer treats it as neutral (never the cheapest) and the
// ledger records $0 cost for it, labelled as a lower bound wherever
// spend is shown.
//
// The ONE exception is the opt-in models.dev sync (see
// internal/modelsync, config "models_dev" block): when the operator
// explicitly enables it, the router fetches the public models.dev
// catalog and fills in prices (and context windows) ONLY for
// (provider, model) pairs the operator has NOT priced at either
// level. Synced prices live in a separate layer (PriceBook's synced
// layer) that Lookup consults LAST, so an operator price always
// wins; the layer records source + sync time per pair for the
// dashboard. The sync is OFF by default — with it off the router
// makes no network call for prices at all.
//
// Prices are keyed by (provider, model): the same model can cost
// differently at two providers, and the pair price is what the
// ledger charges and what the cheapest strategy and the auto-router
// tier ordering rank on. A price keyed by model alone still parses
// (the legacy "prices" block and flat price files) and is kept as a
// MODEL-LEVEL FALLBACK: it applies to every provider serving that
// model, but only when no pair price exists for the serving
// provider — it is a fallback, not a pair price, and the lookup
// code says so wherever the two levels meet. A pair price always
// wins over a model-level price for the same model, regardless of
// which source (config file vs price file) each came from; within
// one level, the config file wins over the price file.
//
// Price fields are pointers: an absent field is "unknown", a present
// 0 is "genuinely free". That distinction is what keeps an unpriced
// model from being ranked as free, and a free model from being
// ranked as unknown.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"sync/atomic"
	"time"
)

// ModelPrice is the operator-configured price of one model, in USD
// per 1M tokens. Every field is a pointer so "not configured"
// (nil = unknown) stays distinct from "configured as free" (0).
//
//   - InputUSDPer1M / OutputUSDPer1M: the price per 1M input /
//     output tokens. Both must be set for a blended price to exist:
//     a half-known price is unknown, never guessed from one side.
//   - CachedInputUSDPer1M: the optional price per 1M cached-input
//     tokens (prompt-cache reads, where the upstream charges less
//     for a cache hit). Nil means the operator did not configure a
//     cached price; it is never derived from the input price.
//   - CacheWrite5mUSDPer1M / CacheWrite1hUSDPer1M (M6.9b): the
//     optional price per 1M tokens written into the prompt cache
//     at the 5-minute / 1-hour TTL tier (Anthropic
//     cache_creation split). Nil means the operator did not
//     configure a tier rate; the ledger then falls back to the
//     Anthropic-documented multiples (1.25x / 2x of the input
//     price) for Anthropic-served Claude models, else the plain
//     input price — never an invented rate.
type ModelPrice struct {
	InputUSDPer1M        *float64 `json:"input_usd_per_1m,omitempty"`
	OutputUSDPer1M       *float64 `json:"output_usd_per_1m,omitempty"`
	CachedInputUSDPer1M  *float64 `json:"cached_input_usd_per_1m,omitempty"`
	CacheWrite5mUSDPer1M *float64 `json:"cache_write_5m_usd_per_1m,omitempty"`
	CacheWrite1hUSDPer1M *float64 `json:"cache_write_1h_usd_per_1m,omitempty"`
	// EndpointPrices is the PER-ENDPOINT price dimension (G16,
	// Area 11 cost row): the same model costs differently per
	// API surface — chat vs embeddings vs responses vs audio —
	// because the upstream tariffs differ (e.g. an embedding
	// model prices per input token with no completion leg at
	// all). Keys are endpoint strings ("" = chat completions,
	// "embeddings", "responses", "audio", "images", "rerank",
	// ...). A leg set (non-nil) in the endpoint override
	// replaces the base leg for that endpoint; a nil leg falls
	// back to the base leg, so an override need only state the
	// legs that actually differ. An endpoint with no entry
	// prices at the base legs. The ledger side
	// (ledger.ModelPrice.EndpointPrices) carries the same
	// dimension in per-token form; buildLedgerPrices in
	// cmd/codvian-router/main.go maps it across.
	EndpointPrices map[string]ModelPrice `json:"endpoint_prices,omitempty"`
}

// PriceForEndpoint returns the price in effect for one endpoint:
// the base price with any endpoint-override legs applied. An
// endpoint with no override entry, or an empty endpoint (""),
// prices at the base legs unchanged.
func (p ModelPrice) PriceForEndpoint(endpoint string) ModelPrice {
	if endpoint == "" || len(p.EndpointPrices) == 0 {
		return p
	}
	ep, ok := p.EndpointPrices[endpoint]
	if !ok {
		return p
	}
	out := p
	if ep.InputUSDPer1M != nil {
		out.InputUSDPer1M = ep.InputUSDPer1M
	}
	if ep.OutputUSDPer1M != nil {
		out.OutputUSDPer1M = ep.OutputUSDPer1M
	}
	if ep.CachedInputUSDPer1M != nil {
		out.CachedInputUSDPer1M = ep.CachedInputUSDPer1M
	}
	if ep.CacheWrite5mUSDPer1M != nil {
		out.CacheWrite5mUSDPer1M = ep.CacheWrite5mUSDPer1M
	}
	if ep.CacheWrite1hUSDPer1M != nil {
		out.CacheWrite1hUSDPer1M = ep.CacheWrite1hUSDPer1M
	}
	// An endpoint override never carries a nested endpoint
	// dimension of its own: one level is the whole dimension.
	out.EndpointPrices = nil
	return out
}

// Validate rejects a price with a negative, NaN or infinite field,
// naming the field. A zero price is valid: it means genuinely free.
func (p ModelPrice) Validate() error {
	for name, v := range map[string]*float64{
		"input_usd_per_1m":          p.InputUSDPer1M,
		"output_usd_per_1m":         p.OutputUSDPer1M,
		"cached_input_usd_per_1m":   p.CachedInputUSDPer1M,
		"cache_write_5m_usd_per_1m": p.CacheWrite5mUSDPer1M,
		"cache_write_1h_usd_per_1m": p.CacheWrite1hUSDPer1M,
	} {
		if v == nil {
			continue
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return fmt.Errorf("config: price %s must be a finite number (got %v)", name, *v)
		}
		if *v < 0 {
			return fmt.Errorf("config: price %s must not be negative (got %v)", name, *v)
		}
	}
	for endpoint, ep := range p.EndpointPrices {
		if err := ep.Validate(); err != nil {
			return fmt.Errorf("config: price endpoint %q: %w", endpoint, err)
		}
	}
	return nil
}

// Known reports whether the price carries any configured value at
// all (input, output, cached, either cache-write tier, or any
// endpoint-override leg). An all-nil price is an explicitly unknown
// price, not a free one.
func (p ModelPrice) Known() bool {
	if p.InputUSDPer1M != nil || p.OutputUSDPer1M != nil || p.CachedInputUSDPer1M != nil || p.CacheWrite5mUSDPer1M != nil || p.CacheWrite1hUSDPer1M != nil {
		return true
	}
	for _, ep := range p.EndpointPrices {
		if ep.Known() {
			return true
		}
	}
	return false
}

// BlendedUSDPer1M returns the blended price in USD per 1M tokens —
// the mean of the input and output prices — and whether it is known.
// Both sides must be configured: with only one side set the blend
// is unknown (false), because the missing side must not be guessed
// from the known one.
func (p ModelPrice) BlendedUSDPer1M() (float64, bool) {
	if p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
		return 0, false
	}
	return (*p.InputUSDPer1M + *p.OutputUSDPer1M) / 2, true
}

// PriceKey is one priced (provider, model) pair: the key every
// price in the book is stored under. A PriceKey with an empty
// Provider is never stored in Pairs; the model-level fallback lives
// in PriceBook.Entries instead, keyed by model ID alone.
type PriceKey struct {
	Provider string
	Model    string
}

// String renders the key as "provider/model", or just the model ID
// when the provider is empty (a model-level key, never a pair).
func (k PriceKey) String() string {
	if k.Provider == "" {
		return k.Model
	}
	return k.Provider + "/" + k.Model
}

// SyncedSourceModelsDev / SyncedSourceLiteLLM are the two price-sync
// sources a synced price can come from (parity tracker Area 11, price
// coverage): the models.dev catalog and LiteLLM's model cost map.
// A synced price carries its source so the dashboard can name it
// honestly, and so the two sources can share one PriceBook layer
// without one wiping the other (see SetSyncedPricesForSource).
// models.dev outranks LiteLLM for the same pair (parity tracker:1134
// resolution order: user > models.dev > LiteLLM-synced >
// hardcoded defaults). internal/modelsync aliases these as
// SourceModelsDev / SourceLiteLLM — the strings here are canonical,
// the two spellings must never drift apart.
const (
	SyncedSourceModelsDev = "models.dev"
	SyncedSourceLiteLLM   = "litellm"
)

// SyncedPrice is one price fetched by an opt-in sync (see
// internal/modelsync): the price itself plus when it was fetched,
// so the dashboard can show provenance (source + sync time)
// instead of a bare number. Source is which sync produced it
// ("models.dev" or "litellm"); empty means "models.dev" — entries
// synced before sources were tracked — and SourcePair reports
// that default, never "".
type SyncedPrice struct {
	Price    ModelPrice
	SyncedAt time.Time
	Source   string
}

// syncedLayer is the immutable opt-in models.dev price layer:
// (provider, model) -> price synced from models.dev. It is
// replaced wholesale on every sync (copy-on-write through an
// atomic pointer) so request-path lookups never take a lock and
// never see a half-applied sync.
type syncedLayer struct {
	pairs map[PriceKey]SyncedPrice
}

// PriceBook is the resolved price source: the config file's price
// blocks merged over an optional local price file. Entries are
// keyed by (provider, model) in Pairs; Entries holds the legacy
// model-level prices, which Lookup consults only when no pair
// price exists. A model absent from both maps has an unknown price
// — lookups report that, never $0.
//
// The synced layer (see SetSyncedPrices) holds the opt-in
// models.dev prices and is consulted LAST: an operator price at
// either level always wins over a synced price for the same pair,
// and the sync itself never writes a pair the operator priced
// (see OperatorHasPrice).
type PriceBook struct {
	// Pairs maps (provider, model) -> configured price: the
	// price that provider charges for that model.
	Pairs map[PriceKey]ModelPrice
	// PairSources maps (provider, model) -> where its price came
	// from: "config" (the config file's provider_prices block)
	// or "price-file" (the local price file).
	PairSources map[PriceKey]string
	// Entries maps model ID -> configured price: the MODEL-LEVEL
	// FALLBACK. It applies to every provider serving that model,
	// but only when no pair price exists for the serving
	// provider — it is a fallback, not a pair price.
	Entries map[string]ModelPrice
	// Sources maps model ID -> where its model-level price came
	// from: "config" (the config file's prices block) or
	// "price-file" (the local price file).
	Sources map[string]string
	// HasFile reports whether a price file was loaded (even an
	// empty one): the dashboard can say "price file loaded" vs
	// "no price file configured".
	HasFile bool
	// FileModels is how many entries the price file contributed
	// (pair entries plus model-level entries, before config-file
	// entries overrode any of them).
	FileModels int
	// synced is the opt-in synced price layer (models.dev and/or
	// LiteLLM — see SyncedSourceModelsDev / SyncedSourceLiteLLM).
	// It is never part of Merge (operator sources only) and is
	// not persisted with the config: the sync state files own it
	// (see internal/modelsync). It is consulted LAST (see
	// Lookup): an operator price at either level always wins
	// over a synced price for the same pair, and between the two
	// sync sources models.dev wins over LiteLLM for the same
	// pair (see SetSyncedPricesForSource).
	synced atomic.Pointer[syncedLayer]
}

// NewPriceBook returns an empty price book: no prices known, so
// every model is unknown until the operator configures one.
func NewPriceBook() *PriceBook {
	return &PriceBook{
		Pairs:       map[PriceKey]ModelPrice{},
		PairSources: map[PriceKey]string{},
		Entries:     map[string]ModelPrice{},
		Sources:     map[string]string{},
	}
}

// Lookup returns the configured price for a (provider, model)
// pair, and whether one exists. The pair price wins; when the
// pair has no entry, the MODEL-LEVEL FALLBACK applies — the
// model-level price for that model, if one is configured. When
// neither operator level has an entry, the opt-in models.dev
// layer applies (see LookupSyncedPair): an operator price at
// either level always wins over a synced price for the same pair.
// ok=false means "price unknown" — callers must treat the model
// as neutral, never as free. Use LookupPair when the distinction
// matters (a pair price vs a model-level fallback are different
// claims).
func (b *PriceBook) Lookup(provider, model string) (ModelPrice, bool) {
	if p, ok := b.LookupPair(provider, model); ok {
		return p, true
	}
	// Model-level fallback, not a pair price: the operator priced
	// the model, not this provider's serving of it.
	if p, ok := b.LookupModel(model); ok {
		return p, true
	}
	// models.dev layer, LAST: an operator price at either level
	// above always wins over a synced price for the same pair.
	return b.LookupSyncedPair(provider, model)
}

// LookupSyncedPair returns the opt-in models.dev price for a
// (provider, model) pair only — no operator fallback. ok=false
// means this pair has no synced price (the operator may still
// have priced it, or it may be unknown entirely).
func (b *PriceBook) LookupSyncedPair(provider, model string) (ModelPrice, bool) {
	sp, ok := b.syncedPair(provider, model)
	if !ok {
		return ModelPrice{}, false
	}
	return sp.Price, true
}

// SyncedPair returns the opt-in models.dev entry for a
// (provider, model) pair — price plus when it was synced — in one
// atomic load, so a price and its provenance timestamp always come
// from the same layer generation. ok=false means the pair has no
// synced price.
func (b *PriceBook) SyncedPair(provider, model string) (SyncedPrice, bool) {
	return b.syncedPair(provider, model)
}

// SyncedAtPair returns when a (provider, model) pair's price was
// last synced from models.dev, and whether a synced price exists
// for the pair at all.
func (b *PriceBook) SyncedAtPair(provider, model string) (time.Time, bool) {
	sp, ok := b.syncedPair(provider, model)
	if !ok {
		return time.Time{}, false
	}
	return sp.SyncedAt, true
}

// syncedPair returns the models.dev entry for a pair, if one was
// synced. The layer is loaded atomically and never mutated in
// place, so this is safe on the request path without a lock.
func (b *PriceBook) syncedPair(provider, model string) (SyncedPrice, bool) {
	if b == nil {
		return SyncedPrice{}, false
	}
	layer := b.synced.Load()
	if layer == nil {
		return SyncedPrice{}, false
	}
	sp, ok := layer.pairs[PriceKey{Provider: provider, Model: model}]
	return sp, ok
}

// SetSyncedPrices replaces the whole opt-in synced layer with
// entries (a nil or empty map clears it). The caller owns the
// merge: entries the operator has since priced must be dropped by
// the caller before this call, so the layer never carries a pair
// the operator now prices — Lookup would still prefer the
// operator price, but a stale synced row would linger in the
// dashboard feed. Replacing wholesale (copy-on-write) means a
// concurrent Lookup sees either the old layer or the new one,
// never a half-applied sync.
//
// Single-source only: with a second sync source live, callers
// MUST use SetSyncedPricesForSource instead — this wholesale
// replace would wipe the other source's entries. (Clone keeps
// using this: it copies the whole layer, every source at once.)
func (b *PriceBook) SetSyncedPrices(entries map[PriceKey]SyncedPrice) {
	if b == nil {
		return
	}
	layer := &syncedLayer{pairs: make(map[PriceKey]SyncedPrice, len(entries))}
	for k, v := range entries {
		layer.pairs[k] = v
	}
	b.synced.Store(layer)
}

// SetSyncedPricesForSource replaces only one sync source's entries
// in the opt-in synced layer (a nil or empty map clears that
// source only) and keeps the other source's entries untouched, so
// the models.dev and LiteLLM syncers can share one PriceBook
// without one wiping the other's prices on every run. Precedence
// for a pair both sources cover is models.dev over LiteLLM
// (parity tracker:1134): a LiteLLM entry never overwrites a pair the
// models.dev layer already holds, and a models.dev entry
// overwrites a LiteLLM entry for the same pair. An unknown source
// stores nothing — a misspelled source must never silently land
// prices under a label the dashboard cannot name. Entries are
// stamped with the source on the way in, so SourcePair reports
// the entry's source even when the caller did not stamp it.
func (b *PriceBook) SetSyncedPricesForSource(source string, entries map[PriceKey]SyncedPrice) {
	if b == nil {
		return
	}
	if source != SyncedSourceModelsDev && source != SyncedSourceLiteLLM {
		return
	}
	old := b.synced.Load()
	next := make(map[PriceKey]SyncedPrice, len(entries))
	if old != nil {
		for k, v := range old.pairs {
			src := v.Source
			if src == "" {
				src = SyncedSourceModelsDev
			}
			if src == source {
				continue // this source's entries are replaced below
			}
			next[k] = v
		}
	}
	for k, v := range entries {
		if source == SyncedSourceLiteLLM {
			if cur, ok := next[k]; ok {
				curSrc := cur.Source
				if curSrc == "" {
					curSrc = SyncedSourceModelsDev
				}
				if curSrc == SyncedSourceModelsDev {
					// models.dev already prices this pair: the
					// LiteLLM value must never shadow it.
					continue
				}
			}
		}
		v.Source = source
		next[k] = v
	}
	b.synced.Store(&syncedLayer{pairs: next})
}

// SortedSyncedPairs lists every synced (provider, model) pair in
// sorted order (provider, then model), for stable dashboard
// output. Empty when no sync ever ran or synced nothing.
func (b *PriceBook) SortedSyncedPairs() []PriceKey {
	if b == nil {
		return nil
	}
	layer := b.synced.Load()
	if layer == nil {
		return nil
	}
	out := make([]PriceKey, 0, len(layer.pairs))
	for key := range layer.pairs {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// SyncedPairsBySource lists the synced (provider, model) pairs
// from one source only ("models.dev" or "litellm"), in sorted
// order (provider, then model). Entries synced before sources
// were tracked carry no Source field and count as "models.dev".
// Empty when that source never synced anything.
func (b *PriceBook) SyncedPairsBySource(source string) []PriceKey {
	if b == nil {
		return nil
	}
	layer := b.synced.Load()
	if layer == nil {
		return nil
	}
	var out []PriceKey
	for key, sp := range layer.pairs {
		src := sp.Source
		if src == "" {
			src = SyncedSourceModelsDev
		}
		if src == source {
			out = append(out, key)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// OperatorHasPrice reports whether the OPERATOR configured a price
// for a (provider, model) pair — a pair entry or a model-level
// fallback. Synced prices never count: this is the "must the sync
// leave this pair alone?" question, and the answer is yes
// whenever the operator priced the pair at either level, so a
// synced pair can never shadow an operator's model-level price
// through the pair-wins lookup rule.
func (b *PriceBook) OperatorHasPrice(provider, model string) bool {
	if b == nil {
		return false
	}
	if _, ok := b.Pairs[PriceKey{Provider: provider, Model: model}]; ok {
		return true
	}
	_, ok := b.Entries[model]
	return ok
}

// LookupPair returns the configured price for a (provider, model)
// pair only — no model-level fallback. ok=false means this pair
// has no price of its own (a model-level price may still exist).
func (b *PriceBook) LookupPair(provider, model string) (ModelPrice, bool) {
	if b == nil || b.Pairs == nil {
		return ModelPrice{}, false
	}
	p, ok := b.Pairs[PriceKey{Provider: provider, Model: model}]
	return p, ok
}

// LookupModel returns the MODEL-LEVEL FALLBACK price for a model
// (the legacy "prices" block / flat price file), and whether one
// exists. It says nothing about any one provider's price for the
// model: a pair price for a provider wins over this at Lookup time.
func (b *PriceBook) LookupModel(model string) (ModelPrice, bool) {
	if b == nil || b.Entries == nil {
		return ModelPrice{}, false
	}
	p, ok := b.Entries[model]
	return p, ok
}

// BlendedUSDPer1M returns the blended price (USD per 1M tokens)
// for a (provider, model) pair, and whether it is known: pair
// price first, model-level fallback second. Unknown when neither
// level has an entry, or when the winning entry lacks either the
// input or the output price (see ModelPrice.BlendedUSDPer1M).
func (b *PriceBook) BlendedUSDPer1M(provider, model string) (float64, bool) {
	p, ok := b.Lookup(provider, model)
	if !ok {
		return 0, false
	}
	return p.BlendedUSDPer1M()
}

// BlendedPair returns the blended price for a (provider, model)
// pair only — no model-level fallback — and whether it is known.
func (b *PriceBook) BlendedPair(provider, model string) (float64, bool) {
	p, ok := b.LookupPair(provider, model)
	if !ok {
		return 0, false
	}
	return p.BlendedUSDPer1M()
}

// BlendedModel returns the blended MODEL-LEVEL FALLBACK price for
// a model, and whether one is configured. It is the legacy
// model-only lookup, kept for callers that genuinely have no
// provider (per-model dashboard rows); anything serving a request
// must use BlendedUSDPer1M with the provider.
func (b *PriceBook) BlendedModel(model string) (float64, bool) {
	p, ok := b.LookupModel(model)
	if !ok {
		return 0, false
	}
	return p.BlendedUSDPer1M()
}

// CheapestBlendedUSDPer1M returns the cheapest known blended price
// (USD per 1M tokens) for a model across every configured
// (provider, model) pair for it and the model-level fallback, and
// whether any is known. It exists for per-model displays (the
// strategies feed, the auto-route decision card) that name a model
// but no provider: the cheapest pair is the honest per-model
// number, and callers must label it as the cheapest pair, never as
// "the model's price". Unknown when the model has no entry at
// either level, or no entry with both sides known.
func (b *PriceBook) CheapestBlendedUSDPer1M(model string) (float64, bool) {
	if b == nil {
		return 0, false
	}
	best, known := 0.0, false
	consider := func(p ModelPrice) {
		if v, ok := p.BlendedUSDPer1M(); ok && (!known || v < best) {
			best, known = v, true
		}
	}
	for key, p := range b.Pairs {
		if key.Model == model {
			consider(p)
		}
	}
	if layer := b.synced.Load(); layer != nil {
		for key, sp := range layer.pairs {
			if key.Model == model {
				consider(sp.Price)
			}
		}
	}
	if p, ok := b.Entries[model]; ok {
		consider(p)
	}
	return best, known
}

// HasPriceForModel reports whether any price is configured for a
// model: a pair price at any provider, a synced models.dev price
// at any provider, or the model-level fallback.
// It is the "is this model priced?" question the per-key rolling
// budget's fail_closed gate asks: a configured $0 counts as
// priced (genuinely free), no entry anywhere means unpriced —
// never $0. It says nothing about which provider's price would
// apply to a request; anything serving a request must use Lookup
// with the provider.
func (b *PriceBook) HasPriceForModel(model string) bool {
	if b == nil {
		return false
	}
	if _, ok := b.Entries[model]; ok {
		return true
	}
	for key := range b.Pairs {
		if key.Model == model {
			return true
		}
	}
	if layer := b.synced.Load(); layer != nil {
		for key := range layer.pairs {
			if key.Model == model {
				return true
			}
		}
	}
	return false
}

// Source returns where a (provider, model) pair's price came from
// ("config", "price-file" or "models.dev"): the pair's source when
// it has one, else the model-level fallback's source, else the
// models.dev layer's. "" when the pair has no configured price at
// any level.
func (b *PriceBook) Source(provider, model string) string {
	if b == nil {
		return ""
	}
	if src := b.SourcePair(provider, model); src != "" {
		return src
	}
	// Model-level fallback source, not a pair source.
	return b.SourceModel(model)
}

// SourcePair returns where a (provider, model) pair's own price
// came from ("config", "price-file", "models.dev" or "litellm"),
// or "" when the pair has no entry of its own. A synced pair
// reports its own source — "litellm" for a LiteLLM-synced price,
// "models.dev" otherwise (including entries synced before sources
// were tracked, which carry no Source field) — the dashboard shows
// that, never "config".
func (b *PriceBook) SourcePair(provider, model string) string {
	if b == nil {
		return ""
	}
	if src := b.PairSources[PriceKey{Provider: provider, Model: model}]; src != "" {
		return src
	}
	if sp, ok := b.syncedPair(provider, model); ok {
		if sp.Source != "" {
			return sp.Source
		}
		return SyncedSourceModelsDev
	}
	return ""
}

// SourceModel returns where a model's MODEL-LEVEL FALLBACK price
// came from ("config" or "price-file"), or "" when the model has
// no model-level entry.
func (b *PriceBook) SourceModel(model string) string {
	if b == nil {
		return ""
	}
	return b.Sources[model]
}

// Merge overlays other's entries onto b; on a conflict b's existing
// entry wins (the receiver is the higher-precedence source). This is
// how the config file's price blocks override the price file: load
// the file first, then merge the config block over it. Pairs and
// model-level entries merge independently: a config pair price wins
// over the same file pair, a config model-level price over the same
// file model-level price — but a pair price at either level still
// wins over a model-level price at Lookup time (see Lookup).
func (b *PriceBook) Merge(other *PriceBook) {
	if b == nil || other == nil {
		return
	}
	if b.Pairs == nil {
		b.Pairs = map[PriceKey]ModelPrice{}
	}
	if b.PairSources == nil {
		b.PairSources = map[PriceKey]string{}
	}
	for key, p := range other.Pairs {
		if _, ok := b.Pairs[key]; ok {
			continue
		}
		b.Pairs[key] = p
		if src := other.PairSources[key]; src != "" {
			b.PairSources[key] = src
		}
	}
	if b.Entries == nil {
		b.Entries = map[string]ModelPrice{}
	}
	if b.Sources == nil {
		b.Sources = map[string]string{}
	}
	for model, p := range other.Entries {
		if _, ok := b.Entries[model]; ok {
			continue
		}
		b.Entries[model] = p
		if src := other.Sources[model]; src != "" {
			b.Sources[model] = src
		}
	}
}

// SortedPairs lists every priced (provider, model) pair in sorted
// order (provider, then model), for stable dashboard output.
func (b *PriceBook) SortedPairs() []PriceKey {
	if b == nil {
		return nil
	}
	out := make([]PriceKey, 0, len(b.Pairs))
	for key := range b.Pairs {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// SortedModels lists every model with a MODEL-LEVEL FALLBACK price
// in sorted order, for stable dashboard output. Models priced
// only per pair do not appear here; use SortedPairs for those.
func (b *PriceBook) SortedModels() []string {
	if b == nil {
		return nil
	}
	out := make([]string, 0, len(b.Entries))
	for model := range b.Entries {
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

// LoadPriceFile reads a local JSON price file. Two shapes are
// accepted, and may be mixed in one file:
//
//   - Flat (legacy): a model ID maps to a ModelPrice object — a
//     MODEL-LEVEL FALLBACK price, applying to every provider
//     serving that model unless a pair price overrides it:
//     {"gpt-4o": {"input_usd_per_1m": 2.50, "output_usd_per_1m": 10.00}}
//
//   - Nested (per-provider): a provider ID maps to a model ID ->
//     ModelPrice object — a (provider, model) PAIR price:
//     {"openai": {"gpt-4o": {"input_usd_per_1m": 2.50, ...}},
//
//     "anthropic": {"claude-sonnet-4-5": {"input_usd_per_1m": 3.00, ...}}}
//
// The two shapes are told apart per top-level key: a value whose
// fields are price objects is a provider map; a value whose fields
// are numbers is a ModelPrice. Every entry is validated (see
// ModelPrice.Validate); a bad entry fails the whole load with the
// provider and model named, so a typo can never silently drop a
// price. The file is read once, at startup, from a local path the
// operator supplied — the router never fetches prices from the
// network.
func LoadPriceFile(path string) (*PriceBook, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read price file %q: %w", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("config: invalid JSON in price file %q: %w", path, err)
	}
	book := NewPriceBook()
	book.HasFile = true
	for key, value := range top {
		// Nested shape first: provider -> {model -> price}.
		// A flat ModelPrice object cannot parse as this map
		// (its values are numbers, not price objects), so a
		// successful parse unambiguously means "provider map".
		var nested map[string]ModelPrice
		if err := json.Unmarshal(value, &nested); err == nil {
			if key == "" {
				return nil, fmt.Errorf("config: price file %q has an empty provider key", path)
			}
			for model, p := range nested {
				if model == "" {
					return nil, fmt.Errorf("config: price file %q provider %q has an empty model key", path, key)
				}
				if err := p.Validate(); err != nil {
					return nil, fmt.Errorf("config: price file %q provider %q model %q: %w", path, key, model, err)
				}
				pk := PriceKey{Provider: key, Model: model}
				book.Pairs[pk] = p
				book.PairSources[pk] = "price-file"
			}
			continue
		}
		// Flat shape: model -> price (model-level fallback).
		var p ModelPrice
		if err := json.Unmarshal(value, &p); err != nil {
			return nil, fmt.Errorf("config: invalid price entry for %q in price file %q: %w", key, path, err)
		}
		if key == "" {
			return nil, fmt.Errorf("config: price file %q has an empty model key", path)
		}
		if err := p.Validate(); err != nil {
			return nil, fmt.Errorf("config: price file %q model %q: %w", path, key, err)
		}
		book.Entries[key] = p
		book.Sources[key] = "price-file"
	}
	book.FileModels = len(book.Pairs) + len(book.Entries)
	return book, nil
}

// Clone returns a deep copy of the book.
func (b *PriceBook) Clone() *PriceBook {
	if b == nil {
		return nil
	}
	out := &PriceBook{HasFile: b.HasFile, FileModels: b.FileModels}
	if b.Pairs != nil {
		out.Pairs = make(map[PriceKey]ModelPrice, len(b.Pairs))
		for k, v := range b.Pairs {
			out.Pairs[k] = v.clone()
		}
	}
	if b.PairSources != nil {
		out.PairSources = make(map[PriceKey]string, len(b.PairSources))
		for k, v := range b.PairSources {
			out.PairSources[k] = v
		}
	}
	if b.Entries != nil {
		out.Entries = make(map[string]ModelPrice, len(b.Entries))
		for k, v := range b.Entries {
			out.Entries[k] = v.clone()
		}
	}
	if b.Sources != nil {
		out.Sources = make(map[string]string, len(b.Sources))
		for k, v := range b.Sources {
			out.Sources[k] = v
		}
	}
	// The synced layer is immutable and replaced wholesale, so
	// the clone takes its own copy of the current layer rather
	// than sharing the pointer: a later SetSyncedPrices on one
	// book must never move the other's prices.
	if layer := b.synced.Load(); layer != nil {
		entries := make(map[PriceKey]SyncedPrice, len(layer.pairs))
		for k, v := range layer.pairs {
			entries[k] = v
		}
		out.SetSyncedPrices(entries)
	}
	return out
}

// clone deep-copies one price (its pointer fields are re-pointed, so
// mutating the copy never mutates the original).
func (p ModelPrice) clone() ModelPrice {
	out := p
	if p.InputUSDPer1M != nil {
		v := *p.InputUSDPer1M
		out.InputUSDPer1M = &v
	}
	if p.OutputUSDPer1M != nil {
		v := *p.OutputUSDPer1M
		out.OutputUSDPer1M = &v
	}
	if p.CachedInputUSDPer1M != nil {
		v := *p.CachedInputUSDPer1M
		out.CachedInputUSDPer1M = &v
	}
	if p.CacheWrite5mUSDPer1M != nil {
		v := *p.CacheWrite5mUSDPer1M
		out.CacheWrite5mUSDPer1M = &v
	}
	if p.CacheWrite1hUSDPer1M != nil {
		v := *p.CacheWrite1hUSDPer1M
		out.CacheWrite1hUSDPer1M = &v
	}
	if p.EndpointPrices != nil {
		out.EndpointPrices = make(map[string]ModelPrice, len(p.EndpointPrices))
		for endpoint, ep := range p.EndpointPrices {
			out.EndpointPrices[endpoint] = ep.clone()
		}
	}
	return out
}
