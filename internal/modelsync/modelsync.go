// Package modelsync is the opt-in live price/context sync from
// models.dev (CHECKLIST 179).
//
// It is OFF by default: nothing in this package runs, and no
// network call is made, unless the operator explicitly enables it
// in the config file ("models_dev" block). When enabled, the
// router fetches the public models.dev catalog
// (https://models.dev/api.json) and fills in prices and context
// windows ONLY for (provider, model) pairs the operator has NOT
// priced at either level (and windows the operator has NOT set).
// Operator values always win — the sync never overwrites them,
// and a pair the operator prices later is dropped from the synced
// layer on the next sync.
//
// What models.dev reports, per model: cost in USD per 1M tokens
// (input / output / cache_read / cache_write) and limit.context
// (the context window). Only the BASE tier of a model's cost is
// used: tiered prices (context_over_200k and per-tier entries)
// are ignored, because the router cannot know pre-send which tier
// a request falls in, and guessing a tier would invent a price.
// Models models.dev marks "deprecated" are skipped entirely.
//
// A failed or offline fetch keeps the last good data and records
// the failure honestly (last error + last success time, so the
// dashboard can say "stale since …", never silently). The synced
// state (prices, windows, sync times, last attempt) is persisted
// in the data dir (models-dev-sync.json, 0600, atomic write) so a
// restart keeps the last good data without re-fetching.
//
// The feed is fetched over plain HTTPS GET with no checksum,
// signature, or pinning: models.dev publishes no checksum or
// signature for api.json, so there is nothing to verify against
// yet. A checksum pin is pending models.dev publishing one — do
// not invent a checksum. Until then the truncation guard in Sync
// (an empty or implausibly shrunken catalog is refused as a
// failed sync) is the only defence against a truncated or
// corrupted fetch applying partial prices with no warning.
//
// No network call happens in tests except against a local httptest
// stand-in for models.dev (the URL is configurable).
package modelsync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// DefaultURL is the public models.dev catalog. It is the default
// for an enabled sync with no URL configured — not a fallback the
// router reaches for on its own.
const DefaultURL = config.DefaultModelsDevURL

// MaxCatalogBytes bounds one models.dev fetch (the catalog is a
// large JSON document, currently tens of MB; 64 MiB leaves ample
// headroom while refusing an unbounded read).
const MaxCatalogBytes = 64 << 20

// StateFileName is the persisted sync state in the data dir.
const StateFileName = "models-dev-sync.json"

// CatalogModel is one model in the models.dev catalog: its price
// (USD per 1M tokens, base tier only) and context window, when
// models.dev reports them. A nil price leg means models.dev did
// not report that leg — never $0. CostInvalid means a reported
// leg failed validation (negative / non-finite): the price is
// dropped for that model, the window (if any) still syncs.
// SupportsVision is true when the feed's [modalities] table
// lists "image" under input; ModalitiesReported is true when the
// feed carried a modalities table at all for the model (without
// it, SupportsVision false means "not reported", not "text-only").
// Reasoning / ToolCall are the feed's capability flags (nil = not
// reported): whether the model supports reasoning and tool
// calling. The tier derivation in intelligence.go aggregates
// them per bare model id.
type CatalogModel struct {
	ID                  string
	ContextWindow       int
	InputUSDPer1M       *float64
	OutputUSDPer1M      *float64
	CachedInputUSDPer1M *float64
	Deprecated          bool
	CostInvalid         bool
	SupportsVision      bool
	ModalitiesReported  bool
	Reasoning           *bool
	ToolCall            *bool
}

// CatalogProvider is one provider in the models.dev catalog.
type CatalogProvider struct {
	ID     string
	Name   string
	Models map[string]CatalogModel
}

// Catalog is the parsed models.dev api.json: provider ID ->
// provider, each with its models keyed by model ID.
type Catalog struct {
	Providers map[string]CatalogProvider
}

type rawCatalogModel struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Limit  *struct {
		Context *int `json:"context"`
	} `json:"limit"`
	Cost *struct {
		Input      *float64 `json:"input"`
		Output     *float64 `json:"output"`
		CacheRead  *float64 `json:"cache_read"`
		CacheWrite *float64 `json:"cache_write"`
	} `json:"cost"`
	// Reasoning / ToolCall are the feed's capability flags (nil =
	// not reported): whether the model supports reasoning and
	// tool calling. The tier derivation in intelligence.go reads
	// them; a missing flag is never treated as a false claim.
	Reasoning *bool `json:"reasoning"`
	ToolCall  *bool `json:"tool_call"`
	// Modalities is the [modalities] table: input/output modality
	// lists ("text", "image", "audio", "video"). A model that
	// accepts image input is vision-capable (G19 Area 1).
	Modalities *struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
}

// hasImageInput reports whether a modalities input list names
// image input (case-insensitive, whitespace-tolerant).
func hasImageInput(list []string) bool {
	for _, m := range list {
		if strings.EqualFold(strings.TrimSpace(m), "image") {
			return true
		}
	}
	return false
}

type rawCatalogProvider struct {
	ID     string                     `json:"id"`
	Name   string                     `json:"name"`
	Models map[string]rawCatalogModel `json:"models"`
}

// ParseCatalog parses the models.dev api.json document. The top
// level is a map of provider ID -> provider object. A provider
// with no "models" object contributes nothing; a model entry
// with no cost and no context window is kept (it still counts as
// a fetched model, it just yields no update). Malformed JSON
// fails the whole parse — a half-parsed catalog must never sync.
func ParseCatalog(data []byte) (*Catalog, error) {
	var raw map[string]rawCatalogProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("modelsync: invalid models.dev catalog JSON: %w", err)
	}
	cat := &Catalog{Providers: make(map[string]CatalogProvider, len(raw))}
	for id, rp := range raw {
		if id == "" {
			continue
		}
		p := CatalogProvider{ID: id, Name: rp.Name, Models: make(map[string]CatalogModel, len(rp.Models))}
		for mid, rm := range rp.Models {
			if mid == "" {
				continue
			}
			m := CatalogModel{ID: mid, Deprecated: rm.Status == "deprecated"}
			if rm.Limit != nil && rm.Limit.Context != nil && *rm.Limit.Context > 0 {
				m.ContextWindow = *rm.Limit.Context
			}
			// Capability flags for the tier derivation: copied
			// through as reported (nil stays nil — "not
			// reported", never a false claim either way).
			m.Reasoning = rm.Reasoning
			m.ToolCall = rm.ToolCall
			if rm.Modalities != nil {
				// The feed reported a modalities table: image
				// input means vision-capable. A missing table
				// leaves ModalitiesReported false, so "no
				// image listed" is never confused with "the
				// feed said text-only".
				m.ModalitiesReported = true
				m.SupportsVision = hasImageInput(rm.Modalities.Input)
			}
			if rm.Cost != nil {
				// A reported leg that is negative or
				// non-finite invalidates the price (never
				// sync a nonsense price); the window, if
				// any, is independent data and still syncs.
				for _, leg := range []*float64{rm.Cost.Input, rm.Cost.Output, rm.Cost.CacheRead, rm.Cost.CacheWrite} {
					if leg != nil && (math.IsNaN(*leg) || math.IsInf(*leg, 0) || *leg < 0) {
						m.CostInvalid = true
						break
					}
				}
				if !m.CostInvalid {
					m.InputUSDPer1M = rm.Cost.Input
					m.OutputUSDPer1M = rm.Cost.Output
					m.CachedInputUSDPer1M = rm.Cost.CacheRead
				}
			}
			p.Models[mid] = m
		}
		cat.Providers[id] = p
	}
	return cat, nil
}

// SourceModelsDev / SourceLiteLLM are the two price-sync sources
// a Syncer can fetch (G16, Area 11 price coverage): models.dev
// (the default) and LiteLLM's model_prices_and_context_window.json.
// Both parse into the same Catalog shape, so one Plan runs against
// either source, and both fill in prices ONLY for (provider,
// model) pairs the operator has NOT priced — never overwriting
// operator-set prices. The strings are canonical in
// config.SyncedSourceModelsDev / config.SyncedSourceLiteLLM (the
// price book stores them); these are aliases, so the two
// spellings can never drift apart. A Syncer serves ONE source:
// run a second Syncer (own state file — LiteLLMStateFileName —
// and PriceBook.SetSyncedPricesForSource, so the two layers never
// wipe each other; models.dev wins a pair both sources cover)
// for the second source. NewLiteLLMSyncer builds it; the
// schedule/loop that runs it lives in main.go (Wave 3 owns it),
// mirroring runModelsDevSyncLoop.
const (
	SourceModelsDev = config.SyncedSourceModelsDev
	SourceLiteLLM   = config.SyncedSourceLiteLLM
)

// LiteLLMStateFileName is the persisted sync state for the
// LiteLLM Syncer in the data dir. A LiteLLM Syncer MUST use its
// own state file — sharing StateFileName with the models.dev
// Syncer would corrupt both states (LoadState refuses a state
// file whose recorded source disagrees with the syncer's source).
const LiteLLMStateFileName = "litellm-sync.json"

// DefaultLiteLLMURL is the public LiteLLM model cost map: the
// second sync source (PARITY Area 3, models.dev sync row —
// OmniRoute also syncs LiteLLM). It is the default for a LiteLLM
// sync with no URL configured — not a fallback the router reaches
// for on its own. Prices are USD per token (×1e6 for per-1M).
const DefaultLiteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

type rawLiteLLMModel struct {
	Provider    string   `json:"litellm_provider"`
	MaxInput    *int     `json:"max_input_tokens"`
	InputCost   *float64 `json:"input_cost_per_token"`
	OutputCost  *float64 `json:"output_cost_per_token"`
	CacheRead   *float64 `json:"cache_read_input_token_cost"`
	Deprecation string   `json:"deprecation_date"`
}

// ParseLiteLLMCatalog parses LiteLLM's model_prices_and_context_window.json
// into the same Catalog shape ParseCatalog produces, so one Plan
// can run against either source. The LiteLLM map is flat — model
// key -> entry — with the provider in `litellm_provider`, or in
// the key prefix ("openai/gpt-4o"); a key with neither is skipped,
// never attributed by guess. Prices are USD per token and are
// converted to USD per 1M tokens (×1e6); the tiered / batch /
// above-200k variants are ignored, like models.dev's tiers: the
// router cannot know pre-send which tier a request falls in.
// cache_creation_input_token_cost is dropped, not mapped: our
// price book splits cache-write into 5m/1h legs and one LiteLLM
// value cannot be split into two without inventing the split.
// The "sample_spec" entry (the file's own field template) is
// skipped. A deprecation_date in the past marks the model
// deprecated; an unparseable one is ignored, never fatal.
//
// CT-6 (fail-closed field-vs-prefix rules, 2026-11-19):
//   - Mismatch: when the key carries a provider prefix AND the
//     `litellm_provider` field is non-empty, the two must agree
//     (exact string match after trimming the field). On
//     disagreement the entry is REJECTED — logged at warning level
//     and never lands in the parsed catalog — so a misattributed
//     pair can never reach Plan/matchProvider. A bare key (no
//     prefix) still trusts the field alone, and an empty field
//     still falls back to the key prefix; only a present
//     disagreement is rejected.
//   - Duplicate: keys sort before parsing, so a duplicate
//     (provider, model) pair resolves FIRST-WINS — the first key
//     in sorted order keeps the slot, later duplicates are skipped
//     with a warning log. Deterministic across runs.
func ParseLiteLLMCatalog(data []byte) (*Catalog, error) {
	return parseLiteLLMCatalog(data, slog.Default())
}

// parseLiteLLMCatalog is ParseLiteLLMCatalog with an injectable
// logger (tests capture warnings through it; production uses
// slog.Default()). A nil logger falls back to slog.Default(), so
// warnings are still emitted (CT-6: nil does not disable them).
func parseLiteLLMCatalog(data []byte, log *slog.Logger) (*Catalog, error) {
	if log == nil {
		log = slog.Default()
	}
	var raw map[string]rawLiteLLMModel
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("modelsync: invalid LiteLLM catalog JSON: %w", err)
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic: duplicate (provider, model) pairs resolve first-wins, same way every run
	cat := &Catalog{Providers: make(map[string]CatalogProvider)}
	now := time.Now().UTC()
	for _, key := range keys {
		if key == "" || key == "sample_spec" {
			continue
		}
		rm := raw[key]
		provider := strings.TrimSpace(rm.Provider)
		modelID := key
		if provider == "" {
			if i := strings.Index(key, "/"); i > 0 {
				provider = key[:i]
				modelID = key[i+1:]
			}
		} else if i := strings.Index(key, "/"); i >= 0 {
			// CT-6: field and key prefix disagree -> reject, never
			// attribute the model to the field's provider.
			if prefix := key[:i]; prefix != provider {
				log.Warn("modelsync: LiteLLM entry provider mismatch, skipping entry",
					"key", key, "litellm_provider", provider, "key_prefix", prefix)
				continue
			}
			modelID = key[i+1:]
		}
		if provider == "" || modelID == "" {
			continue
		}
		m := CatalogModel{ID: modelID}
		if rm.MaxInput != nil && *rm.MaxInput > 0 {
			m.ContextWindow = *rm.MaxInput
		}
		for _, leg := range []*float64{rm.InputCost, rm.OutputCost, rm.CacheRead} {
			if leg != nil && (math.IsNaN(*leg) || math.IsInf(*leg, 0) || *leg < 0) {
				m.CostInvalid = true
				break
			}
		}
		if !m.CostInvalid {
			const per1M = 1e6
			if rm.InputCost != nil {
				v := *rm.InputCost * per1M
				m.InputUSDPer1M = &v
			}
			if rm.OutputCost != nil {
				v := *rm.OutputCost * per1M
				m.OutputUSDPer1M = &v
			}
			if rm.CacheRead != nil {
				v := *rm.CacheRead * per1M
				m.CachedInputUSDPer1M = &v
			}
		}
		if rm.Deprecation != "" {
			if d, err := time.Parse("2006-01-02", strings.TrimSpace(rm.Deprecation)); err == nil && !d.After(now) {
				m.Deprecated = true
			}
		}
		p, ok := cat.Providers[provider]
		if !ok {
			p = CatalogProvider{ID: provider, Models: make(map[string]CatalogModel)}
			cat.Providers[provider] = p
		}
		// CT-6: duplicate (provider, model) pair — first-wins.
		// Keys are sorted, so the winner is deterministic; later
		// duplicates are skipped with a warning, never silently
		// overwriting the first entry.
		if _, dup := p.Models[modelID]; dup {
			log.Warn("modelsync: duplicate LiteLLM (provider, model) pair, keeping first entry",
				"key", key, "provider", provider, "model", modelID)
			continue
		}
		p.Models[modelID] = m
	}
	return cat, nil
}

// LocalModel is one model the router knows a provider offers: its
// ID and its current context window (0 = unknown).
type LocalModel struct {
	ID            string
	ContextWindow int
}

// LocalProvider is one configured provider as the sync sees it:
// the registry ID, the catalog ID when it came from the catalog,
// and the models the router knows it offers. Only models listed
// here can receive synced prices/windows — the sync never invents
// a model the provider does not serve.
type LocalProvider struct {
	ID        string
	CatalogID string
	Models    []LocalModel
}

// Counts is the honest accounting of one sync run: what was
// fetched, matched, applied, and skipped (with the reason). The
// dashboard shows these, never a bare "synced".
type Counts struct {
	FetchedProviders      int `json:"fetched_providers"`
	FetchedModels         int `json:"fetched_models"`
	MatchedPairs          int `json:"matched_pairs"`
	PricesApplied         int `json:"prices_applied"`
	WindowsApplied        int `json:"windows_applied"`
	VisionsApplied        int `json:"visions_applied"`
	SkippedOperatorPrice  int `json:"skipped_operator_price"`
	SkippedOperatorWindow int `json:"skipped_operator_window"`
	SkippedDeprecated     int `json:"skipped_deprecated"`
	SkippedUnknownModel   int `json:"skipped_unknown_model"`
	SkippedInvalidPrice   int `json:"skipped_invalid_price"`
	DroppedStale          int `json:"dropped_stale"`
}

// Update is one (provider, model) pair the sync wants to change.
// HasPrice / HasWindow say whether models.dev had that datum at
// all; the SkippedOperator flags say the datum existed but the
// operator owns the pair, so it must not be applied.
type Update struct {
	Provider string
	Model    string
	// Price is the models.dev price (base tier only); nil when
	// models.dev reported no usable price for the model.
	Price *config.ModelPrice
	// ContextWindow is the models.dev context window; 0 when
	// models.dev reported none.
	ContextWindow int
	// Vision / VisionReported carry the models.dev vision
	// capability (G19 Area 1): VisionReported is true only when
	// the feed carried a modalities table for the model, and
	// Vision is then whether it lists image input. A model
	// whose feed entry has no modalities table reports
	// nothing — the sync never clears a capability on the
	// absence of data.
	Vision         bool
	VisionReported bool
	// PriceSkippedOperator / WindowSkippedOperator: models.dev
	// had the datum, but the operator already set it — apply
	// nothing for that datum.
	PriceSkippedOperator  bool
	WindowSkippedOperator bool
}

// PlanResult is the outcome of planning a sync against the local
// registry: the updates to apply, and the previously synced
// entries to drop because the operator has since priced/set them
// (the synced layer must never shadow an operator value).
// SyncedAt is the instant the catalog was fetched — the caller
// stamps applied prices with it, so the price book and the state
// file record the same sync time.
type PlanResult struct {
	SyncedAt time.Time
	Updates  []Update
	// DropPrices lists synced price pairs to drop: the operator
	// has since priced the pair at either level.
	DropPrices []config.PriceKey
	// DropWindows lists (provider, model) pairs whose synced
	// window to drop: the operator has since set a different
	// window for the pair.
	DropWindows []config.PriceKey
	Counts      Counts
}

// matchProvider finds the models.dev provider for a local
// provider: exact match on the catalog ID first (the canonical
// catalog identity), then on the registry ID. No fuzzy matching,
// no base-URL guessing — an unmatched provider syncs nothing.
func (c *Catalog) matchProvider(lp LocalProvider) (CatalogProvider, bool) {
	if c == nil {
		return CatalogProvider{}, false
	}
	if lp.CatalogID != "" {
		if p, ok := c.Providers[lp.CatalogID]; ok {
			return p, true
		}
	}
	if lp.ID != "" {
		if p, ok := c.Providers[lp.ID]; ok {
			return p, true
		}
	}
	return CatalogProvider{}, false
}

// hasPrice reports whether a models.dev model carries a usable
// price: at least one billable leg (input or output). A cached
// price alone does not make a price — the ledger and the scorer
// need the billable legs, and a cache-only entry would falsely
// mark the pair "priced".
func (m CatalogModel) hasPrice() bool {
	if m.CostInvalid {
		return false
	}
	return m.InputUSDPer1M != nil || m.OutputUSDPer1M != nil
}

// toModelPrice converts a models.dev model to the router's price
// shape (USD per 1M tokens, pointers so absent stays unknown).
// Call only when hasPrice is true.
func (m CatalogModel) toModelPrice() config.ModelPrice {
	return config.ModelPrice{
		InputUSDPer1M:       m.InputUSDPer1M,
		OutputUSDPer1M:      m.OutputUSDPer1M,
		CachedInputUSDPer1M: m.CachedInputUSDPer1M,
	}
}

// Plan works out what a fetched catalog would change, given the
// local providers and two operator-ownership oracles:
//   - operatorHasPrice(provider, model): the operator priced the
//     pair at either level (pair or model-level fallback) — a
//     synced price must never shadow it.
//   - syncedWindow(provider, model): the window the sync itself
//     last applied to the pair, if any. A current window that
//     differs from it was set by the operator (or by hand), so
//     the sync must not overwrite it; a current window equal to
//     it is the sync's own value and may be refreshed.
//
// A pair models.dev does not list is skipped (never invented);
// a deprecated model is skipped entirely. Plan never touches
// the network and never mutates anything: applying is the
// caller's job.
func Plan(cat *Catalog, locals []LocalProvider, operatorHasPrice func(provider, model string) bool, syncedWindow func(provider, model string) (int, bool)) PlanResult {
	var res PlanResult
	if cat != nil {
		res.Counts.FetchedProviders = len(cat.Providers)
		for _, p := range cat.Providers {
			res.Counts.FetchedModels += len(p.Models)
		}
	}
	if cat == nil {
		return res
	}
	for _, lp := range locals {
		remote, ok := cat.matchProvider(lp)
		if !ok {
			continue
		}
		for _, lm := range lp.Models {
			if lm.ID == "" {
				continue
			}
			rm, ok := remote.Models[lm.ID]
			if !ok {
				res.Counts.SkippedUnknownModel++
				continue
			}
			res.Counts.MatchedPairs++
			if rm.Deprecated {
				res.Counts.SkippedDeprecated++
				continue
			}
			u := Update{Provider: lp.ID, Model: lm.ID}
			// Price: apply only when models.dev has one and
			// the operator has not priced the pair.
			if rm.CostInvalid {
				res.Counts.SkippedInvalidPrice++
			} else if rm.hasPrice() {
				if operatorHasPrice(lp.ID, lm.ID) {
					u.PriceSkippedOperator = true
					res.Counts.SkippedOperatorPrice++
				} else {
					p := rm.toModelPrice()
					u.Price = &p
				}
			}
			// Context window: apply only when models.dev has
			// one and the operator has not set one. A current
			// window of 0 is unknown (fill it in); a current
			// window equal to the one the sync last applied is
			// the sync's own value (refresh it); anything else
			// is operator-set (leave it alone).
			if rm.ContextWindow > 0 {
				switch {
				case lm.ContextWindow == 0:
					u.ContextWindow = rm.ContextWindow
				case syncedWindow != nil:
					if w, ok := syncedWindow(lp.ID, lm.ID); ok && w == lm.ContextWindow {
						u.ContextWindow = rm.ContextWindow
					} else {
						u.WindowSkippedOperator = true
						res.Counts.SkippedOperatorWindow++
					}
				default:
					u.WindowSkippedOperator = true
					res.Counts.SkippedOperatorWindow++
				}
			}
			// Vision: apply only when the feed reported a
			// modalities table for the model (G19 Area 1). A
			// reported table without image input clears a
			// previously synced vision flag; a missing table
			// reports nothing and leaves the flag alone.
			if rm.ModalitiesReported {
				u.Vision = rm.SupportsVision
				u.VisionReported = true
			}
			if u.Price != nil || u.ContextWindow > 0 || u.VisionReported {
				res.Updates = append(res.Updates, u)
				if u.Price != nil {
					res.Counts.PricesApplied++
				}
				if u.ContextWindow > 0 {
					res.Counts.WindowsApplied++
				}
				if u.VisionReported && u.Vision {
					res.Counts.VisionsApplied++
				}
			}
		}
	}
	return res
}

// StateEntry is one synced (provider, model) pair in the persisted
// state file: the last synced price and/or window, and when.
// Vision is the last synced vision capability (G19 Area 1):
// true only when a sync affirmatively reported image input for
// the model. A false Vision means "not reported vision-capable"
// (or never reported); the registry default is false too, so
// only true entries need re-applying at startup.
type StateEntry struct {
	Provider      string             `json:"provider"`
	Model         string             `json:"model"`
	Price         *config.ModelPrice `json:"price,omitempty"`
	ContextWindow int                `json:"context_window,omitempty"`
	Vision        bool               `json:"vision,omitempty"`
	SyncedAt      time.Time          `json:"synced_at"`
}

// Status is a snapshot of the syncer's state for the dashboard:
// whether the sync is on, which source it fetches ("models.dev"
// or "litellm"), when it last tried / succeeded, the last error
// ("" = the last attempt succeeded or none ran yet), and the
// synced entries (keyed "provider\x00model" — model IDs may
// contain "/", so "/" cannot separate the pair).
type Status struct {
	Enabled       bool
	Source        string
	URL           string
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	LastError     string
	Entries       map[string]StateEntry
	Counts        Counts
}

// EntryKey is the Status.Entries map key for a pair.
func EntryKey(provider, model string) string { return provider + "\x00" + model }

// SyncerConfig is what a Syncer needs: the operator's opt-in and
// fetch bounds. HTTPClient is injectable for tests (a client
// pointed at a local httptest stand-in); nil builds a default
// client with the configured timeout.
//
// Source selects the catalog to fetch: "" or SourceModelsDev
// fetches models.dev (DefaultURL); SourceLiteLLM fetches LiteLLM's
// model_prices_and_context_window.json (DefaultLiteLLMURL). Any
// other value fails Sync closed (an error, no fetch) — a
// misspelled source must never silently fetch the wrong catalog.
type SyncerConfig struct {
	Enabled  bool
	URL      string
	Source   string
	Interval time.Duration
	Timeout  time.Duration
	// HTTPClient, when set, is used as-is (tests). It must already
	// carry a timeout when the caller cares about one.
	HTTPClient *http.Client
}

// Syncer runs the opt-in models.dev sync: fetch, plan, hand the
// plan to the caller to apply, and persist the resulting state.
// It is safe for concurrent use; Sync serialises on a mutex so a
// ticker and a manual run can never overlap.
type Syncer struct {
	cfg       SyncerConfig
	client    *http.Client
	statePath string

	mu sync.Mutex
	// status is the durable state: last attempt/success/error,
	// counts of the last run, and the synced entries.
	lastAttempt *time.Time
	lastSuccess *time.Time
	lastError   string
	counts      Counts
	entries     map[string]StateEntry // keyed by EntryKey
	// lastCatalog is the catalog of the last successful fetch
	// (nil = none yet). The tier rebuild reads it after a
	// successful Sync: Intelligence.RebuildTier(LastCatalog()).
	// A refused (truncated) fetch never replaces it, so a failed
	// sync cannot wipe the last good tiers.
	lastCatalog *Catalog
}

// New returns a Syncer. A disabled config yields a Syncer whose
// Sync is a no-op that never touches the network; Status still
// reports enabled=false so the dashboard can say so honestly.
// The default URL follows Source: models.dev for "" /
// SourceModelsDev, the LiteLLM cost map for SourceLiteLLM. An
// unknown Source leaves the URL as configured (empty stays
// empty) and Sync fails closed on it — New itself never errors,
// so callers that construct a Syncer at startup cannot crash on
// a misspelled source; the failure surfaces on the first Sync.
func New(cfg SyncerConfig, statePath string) *Syncer {
	if cfg.URL == "" {
		switch cfg.Source {
		case SourceLiteLLM:
			cfg.URL = DefaultLiteLLMURL
		default:
			cfg.URL = DefaultURL
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Duration(config.DefaultModelsDevTimeoutSeconds) * time.Second
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: cfg.Timeout}
	}
	return &Syncer{cfg: cfg, client: client, statePath: statePath, entries: map[string]StateEntry{}}
}

// Enabled reports whether the operator turned the sync on.
func (s *Syncer) Enabled() bool { return s.cfg.Enabled }

// Interval returns the configured re-sync interval (0 = once at
// startup only).
func (s *Syncer) Interval() time.Duration { return s.cfg.Interval }

// URL returns the catalog URL in force.
func (s *Syncer) URL() string { return s.cfg.URL }

// Source returns the catalog source in force ("" means
// models.dev — the default when no source was configured).
func (s *Syncer) Source() string {
	if s.cfg.Source == "" {
		return SourceModelsDev
	}
	return s.cfg.Source
}

// Status returns a copy of the current sync state for the
// dashboard. The Entries map is copied, so callers cannot mutate
// the syncer's state.
func (s *Syncer) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make(map[string]StateEntry, len(s.entries))
	for k, v := range s.entries {
		entries[k] = v
	}
	st := Status{
		Enabled: s.cfg.Enabled,
		Source:  s.Source(),
		URL:     s.cfg.URL,
		Entries: entries,
		Counts:  s.counts,
	}
	if s.lastAttempt != nil {
		t := *s.lastAttempt
		st.LastAttemptAt = &t
	}
	if s.lastSuccess != nil {
		t := *s.lastSuccess
		st.LastSuccessAt = &t
	}
	st.LastError = s.lastError
	return st
}

// LastCatalog returns the catalog of the last successful fetch
// (nil = no successful fetch yet). Callers use it to rebuild the
// models_dev_tier intelligence after a successful Sync.
func (s *Syncer) LastCatalog() *Catalog {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastCatalog
}

// NewLiteLLMSyncer returns a Syncer for the LiteLLM cost map
// (PARITY Area 3 / Area 11, price coverage): the second sync
// source, alongside the models.dev Syncer. The source is forced
// to SourceLiteLLM — the caller cannot misspell it into fetching
// the wrong catalog — and the URL defaults to DefaultLiteLLMURL
// when cfg.URL is empty. The caller passes its own state path
// (LiteLLMStateFileName in the data dir): the two syncers must
// never share a state file. The caller installs the prices with
// PriceBook.SetSyncedPricesForSource(SourceLiteLLM, ...) so the
// LiteLLM layer never wipes the models.dev layer (and vice
// versa); the models.dev price wins for a pair both sources
// cover. Construction is all this does — the schedule/loop that
// runs the syncer lives in main.go (Wave 3 owns it), mirroring
// runModelsDevSyncLoop.
func NewLiteLLMSyncer(cfg SyncerConfig, statePath string) *Syncer {
	cfg.Source = SourceLiteLLM
	return New(cfg, statePath)
}

// SyncedPrices returns the synced price layer for the price book:
// every state entry that carries a price, keyed by (provider,
// model). Each entry is stamped with this syncer's source, so
// the price book can tell a LiteLLM price from a models.dev one
// (SourcePair) and the two sources can share one layer (see
// SetSyncedPricesForSource). The caller (main.go) installs it
// with PriceBook.SetSyncedPricesForSource at startup, so a
// restart keeps the last good prices without re-fetching.
func (s *Syncer) SyncedPrices() map[config.PriceKey]config.SyncedPrice {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[config.PriceKey]config.SyncedPrice, len(s.entries))
	for _, e := range s.entries {
		if e.Price == nil {
			continue
		}
		out[config.PriceKey{Provider: e.Provider, Model: e.Model}] = config.SyncedPrice{Price: *e.Price, SyncedAt: e.SyncedAt, Source: s.Source()}
	}
	return out
}

// SyncedWindow returns the window the sync last applied to a pair,
// if any — the oracle Plan uses to tell a synced window from an
// operator-set one.
func (s *Syncer) SyncedWindow(provider, model string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[EntryKey(provider, model)]
	if !ok || e.ContextWindow <= 0 {
		return 0, false
	}
	return e.ContextWindow, true
}

// stateFile is the on-disk shape of the persisted sync state.
// Source records which syncer wrote the file ("models.dev" or
// "litellm"); LoadState refuses a file whose source disagrees
// with the syncer's source, so the two syncers can never load
// each other's state. Files written before sources were recorded
// carry no Source and load into either syncer (the operator's
// only syncer then was models.dev).
type stateFile struct {
	Version       int          `json:"version"`
	Source        string       `json:"source,omitempty"`
	URL           string       `json:"url,omitempty"`
	LastAttemptAt *time.Time   `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time   `json:"last_success_at,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
	Counts        Counts       `json:"counts"`
	Entries       []StateEntry `json:"entries,omitempty"`
}

// LoadState reads the persisted state file into the syncer. A
// missing file means "never synced" and is not an error. A
// corrupt file returns an error and leaves the syncer empty —
// the caller logs the warning and carries on with no synced data
// (startup must never fail on a corrupt sync cache), and the
// next successful sync overwrites the file.
func (s *Syncer) LoadState() error {
	if s.statePath == "" {
		return nil
	}
	raw, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("modelsync: cannot read state file %q: %w", s.statePath, err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return fmt.Errorf("modelsync: invalid state file %q: %w", s.statePath, err)
	}
	// Fail closed on a foreign state file: a models.dev state
	// file loaded into a LiteLLM syncer (or vice versa) would
	// stamp the wrong source on every price it restores. Files
	// written before sources were recorded carry no Source and
	// still load (the only syncer then was models.dev).
	if sf.Source != "" && sf.Source != s.Source() {
		return fmt.Errorf("modelsync: state file %q belongs to source %q, this syncer is %q", s.statePath, sf.Source, s.Source())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAttempt = sf.LastAttemptAt
	s.lastSuccess = sf.LastSuccessAt
	s.lastError = sf.LastError
	s.counts = sf.Counts
	s.entries = make(map[string]StateEntry, len(sf.Entries))
	for _, e := range sf.Entries {
		if e.Provider == "" || e.Model == "" {
			continue
		}
		s.entries[EntryKey(e.Provider, e.Model)] = e
	}
	return nil
}

// maxCatalogShrink is the largest share of the previous catalog
// a fresh fetch may lose (providers or models) before Sync
// refuses it as truncated. models.dev legitimately adds and
// removes providers and models over time, but a real catalog edit
// removes a handful, not a fifth of the catalog in one fetch — a
// drop past 20% is truncation or corruption until proven
// otherwise, and refusing it keeps the last good prices instead
// of applying a partial catalog with no warning.
//
// minShrinkBaseline is the smallest previous catalog the shrink
// check applies to. Below it a single provider or model wobbling
// in or out is a large fraction, and the guard would false-positive
// on tiny catalogs; with no meaningful baseline the check stays
// silent (the empty-catalog check below still applies).
const (
	maxCatalogShrink  = 0.20
	minShrinkBaseline = 5
)

// catalogSize counts the providers and models in a parsed
// catalog (nil counts as empty).
func catalogSize(cat *Catalog) (providers, models int) {
	if cat == nil {
		return 0, 0
	}
	providers = len(cat.Providers)
	for _, p := range cat.Providers {
		models += len(p.Models)
	}
	return providers, models
}

// checkCatalogTruncation refuses a fetched catalog that is empty
// or has shrunk implausibly against the last good sync (prev is
// the counts of the last successful sync; zero means "never
// synced", so no baseline exists and only the empty check
// applies). A refused catalog is a failed sync: the caller keeps
// the last good data and records the error, never applying the
// partial catalog.
func checkCatalogTruncation(prev Counts, cat *Catalog) error {
	providers, models := catalogSize(cat)
	if providers == 0 {
		return fmt.Errorf("modelsync: fetched catalog has 0 providers — refusing empty fetch, keeping last good data")
	}
	if models == 0 {
		return fmt.Errorf("modelsync: fetched catalog has 0 models — refusing empty fetch, keeping last good data")
	}
	if prev.FetchedProviders >= minShrinkBaseline && float64(providers) < float64(prev.FetchedProviders)*(1-maxCatalogShrink) {
		return fmt.Errorf("modelsync: fetched catalog shrank from %d to %d providers (over %.0f%%) — refusing truncated fetch, keeping last good data", prev.FetchedProviders, providers, maxCatalogShrink*100)
	}
	if prev.FetchedModels >= minShrinkBaseline && float64(models) < float64(prev.FetchedModels)*(1-maxCatalogShrink) {
		return fmt.Errorf("modelsync: fetched catalog shrank from %d to %d models (over %.0f%%) — refusing truncated fetch, keeping last good data", prev.FetchedModels, models, maxCatalogShrink*100)
	}
	return nil
}

// fetch downloads and parses the catalog for the Syncer's source
// (models.dev or LiteLLM). The request carries a bounded timeout
// (client or ctx) and a bounded body.
//
// The feed is unsigned: a plain HTTPS GET with no checksum,
// signature, or pinning (models.dev publishes none — a checksum
// pin is pending them publishing one). fetch therefore cannot
// prove the body is complete or authentic; the truncation guard
// in Sync (checkCatalogTruncation) is what stops an empty or
// implausibly shrunken catalog from applying.
func (s *Syncer) fetch(ctx context.Context) (*Catalog, error) {
	switch s.cfg.Source {
	case "", SourceModelsDev, SourceLiteLLM:
	default:
		return nil, fmt.Errorf("modelsync: unknown sync source %q (want %q or %q)", s.cfg.Source, SourceModelsDev, SourceLiteLLM)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("modelsync: cannot build request for %q: %w", s.cfg.URL, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codvian-router price sync")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("modelsync: fetch %q failed: %w", s.cfg.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("modelsync: fetch %q: unexpected HTTP status %d", s.cfg.URL, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("modelsync: fetch %q: cannot read body: %w", s.cfg.URL, err)
	}
	if len(data) > MaxCatalogBytes {
		return nil, fmt.Errorf("modelsync: fetch %q: catalog exceeds %d bytes", s.cfg.URL, MaxCatalogBytes)
	}
	if s.cfg.Source == SourceLiteLLM {
		return ParseLiteLLMCatalog(data)
	}
	return ParseCatalog(data)
}

// Sync runs one sync: fetch the catalog, plan against the local
// providers, hand the plan to apply (the caller installs prices
// into the price book, windows into the registry, and rebuilds
// ledger prices), then record the new state and persist it.
//
// When disabled it is a no-op that returns zero counts and never
// touches the network. On a fetch/parse failure it records the
// error + attempt time, persists that failure (so the dashboard
// can show it after a restart), and returns the error — the last
// good entries stay in place, in the syncer and in the price
// book, and nothing is applied. On an apply failure it records
// the error the same way and returns it without updating the
// entries, so a retry re-plans the same updates.
//
// operatorHasPrice is the price-book oracle
// (PriceBook.OperatorHasPrice). locals is a snapshot of the
// registry taken by the caller just before the call.
func (s *Syncer) Sync(ctx context.Context, locals []LocalProvider, operatorHasPrice func(provider, model string) bool, apply func(PlanResult) error) (Counts, error) {
	if !s.cfg.Enabled {
		return Counts{}, nil
	}
	now := time.Now().UTC()
	// Snapshot the previous entries under the lock, then release
	// it: the fetch and the apply callback run UNLOCKED. The
	// apply (modelsDevApplyPlan) reads the syncer's current
	// prices via SyncedPrices(), and holding s.mu across apply
	// would deadlock on that re-entrant lock — Go mutexes are
	// not re-entrant. The plan is folded back under the lock at
	// the end, so Status() and the dashboard only ever see a
	// consistent snapshot. (Single caller in practice: the one
	// background loop; concurrent Sync calls would interleave
	// their applies.)
	s.mu.Lock()
	s.lastAttempt = &now
	prevCounts := s.counts
	prev := make(map[string]StateEntry, len(s.entries))
	for k, e := range s.entries {
		prev[k] = e
	}
	s.mu.Unlock()

	fail := func(err error) (Counts, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastError = err.Error()
		// Persist the failure honestly; a persist error must not
		// mask the sync error itself.
		_ = s.persistLocked()
		return Counts{}, err
	}

	cat, err := s.fetch(ctx)
	if err != nil {
		return fail(err)
	}
	// CT-5 truncation guard: an empty or implausibly shrunken
	// catalog is a failed sync, not a successful one — the last
	// good entries stay in place, the failure is recorded and
	// persisted, and apply is never called, so a truncated
	// fetch can never silently clear or halve the synced prices.
	if err := checkCatalogTruncation(prevCounts, cat); err != nil {
		return fail(err)
	}
	// The fetch passed the truncation guard: it is the new last
	// good catalog, and the tier rebuild reads it after Sync
	// succeeds. (Set here, not after apply: apply can fail while
	// the catalog itself is good — the tiers rebuild from the
	// fetched data regardless of the price apply outcome.)
	s.mu.Lock()
	s.lastCatalog = cat
	s.mu.Unlock()
	syncedWindow := func(provider, model string) (int, bool) {
		e, ok := prev[EntryKey(provider, model)]
		if !ok || e.ContextWindow <= 0 {
			return 0, false
		}
		return e.ContextWindow, true
	}
	plan := Plan(cat, locals, operatorHasPrice, syncedWindow)
	plan.SyncedAt = now
	// Drops: a pair the operator has since priced (or a window
	// the operator has since set) leaves the synced layer, so
	// the operator value is the only one left standing.
	for _, lp := range locals {
		for _, lm := range lp.Models {
			key := EntryKey(lp.ID, lm.ID)
			e, ok := prev[key]
			if !ok {
				continue
			}
			if operatorHasPrice(lp.ID, lm.ID) && e.Price != nil {
				// The operator has since priced a pair the
				// sync previously filled in: the synced price
				// leaves the layer, so the operator value is
				// the only one left standing.
				plan.DropPrices = append(plan.DropPrices, config.PriceKey{Provider: lp.ID, Model: lm.ID})
				plan.Counts.DroppedStale++
			}
			if e.ContextWindow > 0 && lm.ContextWindow > 0 && lm.ContextWindow != e.ContextWindow {
				// The current window differs from the one the
				// sync last applied: the operator (or a hand
				// edit) set it, so the synced window is
				// dropped and never refreshed over it.
				plan.DropWindows = append(plan.DropWindows, config.PriceKey{Provider: lp.ID, Model: lm.ID})
				plan.Counts.DroppedStale++
			}
		}
	}
	// A provider that no longer exists in the registry had its
	// synced entries removed with it: its prices and windows
	// are dead (nothing can look them up), so they leave the
	// state. Entries for a provider that still exists but whose
	// model is absent from its list are KEPT: a model list can
	// be transiently incomplete (a failed model-list fetch at
	// add time), and dropping good data on a transient gap
	// would be worse than keeping a stale row.
	localProviders := make(map[string]bool, len(locals))
	for _, lp := range locals {
		localProviders[lp.ID] = true
	}
	for _, e := range prev {
		if localProviders[e.Provider] {
			continue
		}
		if e.Price != nil {
			plan.DropPrices = append(plan.DropPrices, config.PriceKey{Provider: e.Provider, Model: e.Model})
		}
		if e.ContextWindow > 0 {
			plan.DropWindows = append(plan.DropWindows, config.PriceKey{Provider: e.Provider, Model: e.Model})
		}
		plan.Counts.DroppedStale++
	}
	if apply != nil {
		if err := apply(plan); err != nil {
			return fail(fmt.Errorf("modelsync: apply failed: %w", err))
		}
	}
	// Apply succeeded: fold the plan into the state under the
	// lock. Drops first, then the new values (a
	// dropped-then-re-added pair ends up with the fresh value,
	// never the stale one).
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range plan.DropPrices {
		if e, ok := s.entries[EntryKey(k.Provider, k.Model)]; ok {
			e.Price = nil
			if e.ContextWindow <= 0 {
				delete(s.entries, EntryKey(k.Provider, k.Model))
			} else {
				s.entries[EntryKey(k.Provider, k.Model)] = e
			}
		}
	}
	for _, k := range plan.DropWindows {
		if e, ok := s.entries[EntryKey(k.Provider, k.Model)]; ok {
			e.ContextWindow = 0
			if e.Price == nil {
				delete(s.entries, EntryKey(k.Provider, k.Model))
			} else {
				s.entries[EntryKey(k.Provider, k.Model)] = e
			}
		}
	}
	for _, u := range plan.Updates {
		key := EntryKey(u.Provider, u.Model)
		e := s.entries[key]
		e.Provider = u.Provider
		e.Model = u.Model
		e.SyncedAt = now
		if u.Price != nil {
			p := *u.Price
			e.Price = &p
		}
		if u.ContextWindow > 0 {
			e.ContextWindow = u.ContextWindow
		}
		if u.VisionReported {
			e.Vision = u.Vision
		}
		s.entries[key] = e
	}
	s.lastSuccess = &now
	s.lastError = ""
	s.counts = plan.Counts
	if err := s.persistLocked(); err != nil {
		// The sync itself succeeded and the in-memory state is
		// correct; only the restart durability is lost. Record
		// it, but do not report the sync as failed.
		s.lastError = fmt.Sprintf("sync applied, but state could not be persisted: %v", err)
	}
	return plan.Counts, nil
}

// persistLocked persists while the caller already holds s.mu.
func (s *Syncer) persistLocked() error {
	// persist reads s.entries etc. directly (already locked) —
	// factor the write out so it does not re-lock.
	if s.statePath == "" {
		return nil
	}
	sf := stateFile{Version: 1, Source: s.Source(), URL: s.cfg.URL, LastAttemptAt: s.lastAttempt, LastSuccessAt: s.lastSuccess, LastError: s.lastError, Counts: s.counts}
	keys := make([]string, 0, len(s.entries))
	for k := range s.entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sf.Entries = append(sf.Entries, s.entries[k])
	}
	data, err := json.Marshal(sf)
	if err != nil {
		return fmt.Errorf("modelsync: cannot encode state: %w", err)
	}
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("modelsync: cannot create state dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".models-dev-sync-*.tmp")
	if err != nil {
		return fmt.Errorf("modelsync: cannot create state temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot write state temp file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot chmod state temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("modelsync: cannot fsync state temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("modelsync: cannot close state temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.statePath); err != nil {
		return fmt.Errorf("modelsync: cannot rename state file: %w", err)
	}
	return nil
}
