// Package config owns the versioned, JSON-backed configuration for
// codvian-router: loading, validation, atomic saving, and defaults.
//
// Config file format (version 1):
//
//	{
//	  "version": 1,
//	  "listen": "127.0.0.1:20128",
//	  "data_dir": "/home/user/.local/share/codvian-router",
//	  "gateway_keys": [ ... ],   // see gatewaykey.go; only hashes are stored
//	  "providers": [ ... ],      // see ProviderConfig below
//	  "budget_usd": 0,           // 0 = no global cap set
//	  "fail_closed": true,       // refuse work instead of bypassing limits
//	  "telemetry_enabled": false // no telemetry ever unless explicitly true
//	  "auto_router": { ... }     // see AutoRouterConfig; omitted = defaults
//	  "routing": { ... }         // see RoutingConfig; omitted = defaults
//	}
//
// The router is local-only by design: Listen must be a loopback address
// (127.0.0.1 or ::1). Validate enforces this.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/capability"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// CurrentVersion is the only config version this build understands.
const CurrentVersion = 1

// DefaultListen is the default bind address. Localhost only, never 0.0.0.0.
const DefaultListen = "127.0.0.1:20128"

// DefaultPort is the port portion of DefaultListen.
const DefaultPort = 20128

// Config is the on-disk router configuration (version 1).
//
// Extensions holds top-level JSON fields this build does not recognise,
// keyed by their JSON name. They are preserved verbatim across Load/Save
// round-trips (see MarshalJSON/UnmarshalJSON) so a config written by a
// newer version does not silently lose fields it does not understand.
// Extensions is tagged json:"-" so it never appears as a field of its
// own in the normal field layout; its entries are re-emitted as
// top-level keys. Callers must not put a known field name in Extensions:
// known fields always win on marshal.
type Config struct {
	Version     int              `json:"version"`
	Listen      string           `json:"listen"`
	DataDir     string           `json:"data_dir"`
	GatewayKeys []GatewayKey     `json:"gateway_keys"`
	Providers   []ProviderConfig `json:"providers"`
	BudgetUSD   float64          `json:"budget_usd"`
	FailClosed  bool             `json:"fail_closed"`
	// AllowClientVertexOverride is the admin opt-in for
	// client-supplied Vertex location/project overrides
	// (CHECKLIST 245, LiteLLM #47028). Default false: a client
	// that sends vertex_location / vertex_project (or the
	// vertex_ai_* spellings) in a request body gets a 400
	// naming the field, because the cloud project/location a
	// request lands in is the admin's choice, never the
	// caller's. True admits the field; it still has no routing
	// effect in this build — no Vertex provider consumes it —
	// the location/project always come from the admin's
	// provider config.
	AllowClientVertexOverride bool `json:"allow_client_vertex_override,omitempty"`
	TelemetryEnabled          bool `json:"telemetry_enabled"`
	// TracingOTLPEndpoint is the operator-configured OTLP/HTTP
	// collector for request traces, e.g.
	// http://localhost:4318/v1/traces. Empty (the default) keeps
	// tracing in-process only: spans still record to the bounded
	// ring behind -tags otel, but nothing leaves the gateway. Only
	// meaningful in an otel-tagged build; the default build ignores
	// it. Headers carry static collector auth (e.g. an
	// Authorization or api-key header). Timeout bounds one export
	// POST; a failed POST never blocks a request (bounded queue,
	// dropped spans counted via the /api/observability metrics).
	TracingOTLPEndpoint  string            `json:"tracing_otlp_endpoint,omitempty"`
	TracingOTLPHeaders   map[string]string `json:"tracing_otlp_headers,omitempty"`
	TracingOTLPTimeoutMs int               `json:"tracing_otlp_timeout_ms,omitempty"`
	// A2AEnabled is the operator opt-in for the Agent-to-Agent
	// endpoint (internal/a2a, M9.8): the /a2a JSON-RPC surface and
	// its task store are OFF unless this is true. Default false:
	// the reference router's A2A is keyless by default (GHSA-v54m-6rm3-p565)
	// and that default must never be copied. No validation is
	// needed beyond the bool itself — false and true are both
	// valid; the default is what matters.
	A2AEnabled bool `json:"a2a_enabled,omitempty"`
	// A2A tunes A2A task retention (M9.8 follow-up,
	// docs/design/a2a-retention-design.md §4). Nil means
	// defaults: 7-day retention, 1,000 tasks per key, pruning
	// on. A pointer so omitempty drops the key entirely when
	// unset. The prune interval is NOT a config key (§2): the
	// sweep runs hourly, fixed in code.
	A2A *A2AConfig `json:"a2a,omitempty"`
	// AutoRouter tunes the local auto-router ("auto/*" virtual
	// models). Omitted entirely means defaults: threshold 0.5,
	// fallback tier reasoning, no tier models (so auto-routing
	// falls back to DefaultModel, or fails honestly when that is
	// empty too).
	AutoRouter AutoRouterConfig `json:"auto_router,omitempty"`

	// Reasoning tunes reasoning-effort forwarding (CHECKLIST
	// 285-286): the operator-declared effort→budget table that
	// lets a named effort reach the Anthropic/Gemini wires as a
	// numeric thinking budget. Nil means no table is declared,
	// so those wires send no thinking field. A pointer so
	// omitempty drops the key entirely when unset.
	Reasoning *ReasoningConfig `json:"reasoning,omitempty"`

	// Guardrails tunes the guardrail stage (internal/guardrails).
	// Only the blocking toggles live here — log redaction has no
	// off switch and is not configurable: prompts and provider keys
	// are always redacted before anything reaches a log or the
	// spend ledger. All three default to false (blocking opt-in).
	// A pointer so omitempty drops the key entirely when unset.
	Guardrails *GuardrailsConfig `json:"guardrails,omitempty"`

	// Responses tunes the Responses API (/v1/responses, see
	// docs/responses-api.md). Nil means defaults: sessions are
	// counted, never refused. A pointer so omitempty drops the
	// key entirely when unset.
	Responses *ResponsesConfig `json:"responses,omitempty"`

	// Cache tunes cache persistence (M6.2, see internal/cache):
	// the SQLite files the exact-match and semantic layers
	// snapshot to so entries survive a restart. Nil, or an
	// empty path, means memory-only for that layer — the
	// previous default, unchanged. A path is used as given when
	// absolute and resolved against DataDir when relative. A
	// path that cannot be opened logs a warning at startup and
	// that layer keeps running memory-only; a bad cache path
	// never fails startup. A pointer so omitempty drops the
	// key entirely when unset.
	Cache *CacheConfig `json:"cache,omitempty"`

	// Plugins names the built-in plugins to run, in order (see
	// internal/plugins, CHECKLIST 201): the list IS the pipeline
	// order — pre-request and post-response hooks run in this
	// order. Only built-in names are accepted ("header-add",
	// "response-log"); anything else fails config load naming
	// it. There is deliberately no path / URL / .so field here:
	// plugins are compiled-in Go code only, never arbitrary code
	// loaded from config (see docs/plugins.md). Nil means no
	// plugins run — the gateway works exactly as before, and the
	// dashboard reports zero plugins honestly.
	Plugins *PluginsConfig `json:"plugins,omitempty"`

	// Routing tunes the routing-strategy family (see
	// internal/strategy): the global strategy, the per-combo
	// deadline budget, per-chain strategy overrides, and the
	// custom weights for the "custom-weights" strategy. Nil means
	// defaults: global "priority" (declared chain order, no
	// re-ranking), no deadline budget, no overrides, no custom
	// weights (custom-weights then behaves as balanced). A pointer so
	// omitempty drops the key entirely when unset.
	//
	// ChainWeights (M3 Phase C) is the per-target weight table for the
	// "weighted" strategy: chain (model ID) -> target ("provider/model")
	// -> weight, operator intent, not a measured signal. A missing map
	// means every target draws with weight 1.
	Routing *RoutingConfig `json:"routing,omitempty"`

	// routingMu is the ONE mutex guarding Routing.Groups and
	// Routing.ChainStrategies — the two maps the M8.14 combos
	// API mirrors into live while group traffic reads them
	// (M8.14 race fix). Every accessor of either map takes it:
	// writers (ComboStore mirrorGroup / mirrorStrategy /
	// unmirror) take the write lock, readers (groupChainFor,
	// the groups feed, strategyForModel, Clone, the
	// routingPersist copy, the strategies feed) take the read
	// lock or use the snapshot helpers below. Whole-block
	// readers use RoutingSnapshot (a deep copy under the read
	// lock) and never deref cfg.Routing directly: the pointer
	// itself is only ever swapped under the write lock by
	// routingPersist, so an unlocked load of it races the swap.
	// A per-site mutex would not fix the race; this shared one does.
	//
	// LOCK ORDER (M8.14 race fix, round 3 — the real order, now
	// documented in one place; the earlier "pricingMu and
	// routingMu are never nested" phrasing was false and is
	// gone). One total order, never reversed anywhere:
	//   server pricingMu -> pricesMu -> routingMu,
	//   ComboStore.mu (internal/server) -> routingMu,
	//   dangerMu / keyMu (internal/server) -> pricesMu -> routingMu.
	// The pricing PATCH holds pricingMu across configPersist,
	// and Save takes pricesMu then routingMu inside it, so
	// pricingMu -> pricesMu -> routingMu nests in that one
	// direction only. Clone snapshots ProviderPrices and
	// Prices under the prices read lock BEFORE taking the
	// routing lock, and holds the prices read lock across
	// the whole Clone, and RestoreFrom
	// takes pricesMu then routingMu — nobody ever takes
	// pricesMu or routingMu and then acquires pricingMu,
	// ComboStore.mu, dangerMu or keyMu. Never take
	// ComboStore.mu while holding routingMu, and never hold
	// routingMu across disk I/O or a network call: Clone, the
	// snapshot helpers and the mirror functions copy under
	// the lock and release it before any persist or save runs.
	// Save takes the read locks only for Validate + Marshal
	// (CPU work), never for the file write/rename. routingMu
	// also guards the cfg.Routing pointer swap in
	// routingPersist (cmd/codvian-router): the swap itself
	// takes the write lock, briefly, with no I/O under it.
	//
	// It is a *sync.RWMutex, not a value, so Config.Clone's
	// `out := *c` shares the one mutex instead of copying
	// it (copying a mutex is a vet error and a race). Nil
	// means "not initialised yet — no concurrent access":
	// DefaultConfig and Load initialise it, and
	// EnsureRoutingMu initialises it for literal configs
	// (tests, NewComboStore) before serving starts. The
	// lock/unlock methods below are nil-safe no-ops so a
	// literal config used single-threaded never panics.
	routingMu *sync.RWMutex `json:"-"`

	// pricesMu is the ONE mutex guarding ProviderPrices — the
	// per-(provider, model) price map the pricing PATCH
	// writes live (internal/server/pricing_api.go) while Clone
	// and Save read it. It is a *sync.RWMutex, not a value,
	// for the same reason as routingMu: Clone's `out := *c`
	// shares the one mutex instead of copying it. Nil means
	// "not initialised yet" (same rule: DefaultConfig and
	// Load initialise it, EnsurePricesMu covers literal
	// configs); the lock/unlock methods below are nil-safe
	// no-ops. Lock order is pricesMu -> routingMu (see the
	// routingMu comment above): Clone snapshots ProviderPrices
	// and Prices under the prices read lock BEFORE taking the
	// routing read lock, and holds the prices read lock across
	// the whole Clone; Save holds the prices read lock across
	// Validate + Marshal and takes the routing read lock
	// inside it, and the pricing PATCH takes the prices
	// write lock for its ProviderPrices snapshot + apply
	// (released before configPersist, because Save takes the
	// prices read lock — holding the write lock across the
	// persist would deadlock). Prices (the model-level map)
	// has exactly one live writer — RestoreFrom, the
	// whole-struct rollback, which writes its header under
	// the prices write lock — and is guarded by this mutex
	// the same way: PricesSnapshot and Clone read it only
	// under the prices read lock.
	pricesMu *sync.RWMutex `json:"-"`

	// Prices is the operator-configured MODEL-LEVEL price source
	// (see prices.go): model ID -> USD per 1M tokens (input,
	// output, optional cached-input). This is the legacy shape,
	// kept working: a model-level price applies to every provider
	// serving that model, but only when no (provider, model) pair
	// price exists for the serving provider — it is a fallback,
	// not a pair price. Absent entirely means no model has a
	// configured model-level price. Prices here override the same
	// model in PriceFile.
	Prices map[string]ModelPrice `json:"prices,omitempty"`

	// ProviderPrices is the operator-configured PER-PAIR price
	// source (see prices.go): provider ID -> model ID -> USD per
	// 1M tokens (input, output, optional cached-input). A pair
	// price is what the ledger charges and what the cheapest
	// strategy and the auto-router tier ordering rank on, because
	// the same model can cost differently at two providers.
	// ProviderPrices here override the same pair in PriceFile,
	// and a pair price always wins over a model-level Prices
	// entry for the same model at lookup time.
	ProviderPrices map[string]map[string]ModelPrice `json:"provider_prices,omitempty"`

	// PriceFile is the optional path to a local JSON price file:
	// flat model ID -> price object (model-level fallback) and/or
	// provider ID -> model ID -> price object (pair price), the
	// same two shapes as Prices / ProviderPrices. It is read once
	// at startup; a missing or invalid file fails startup. Empty
	// means no price file. The router never fetches prices from
	// the network for this file — the only network price source
	// is the separate opt-in models_dev sync (see ModelsDev).
	PriceFile string `json:"price_file,omitempty"`

	// ModelsDev is the opt-in live price/context sync from
	// models.dev (see internal/modelsync). Nil means the sync is
	// OFF: the router makes no network call to models.dev, and
	// every price/context window comes from the operator's own
	// config, price file, or provider metadata. When Enabled is
	// true the router fetches the public models.dev catalog at
	// startup (and every IntervalMinutes thereafter when > 0)
	// and fills in prices + context windows ONLY for
	// (provider, model) pairs the operator has NOT priced (and
	// windows the operator has NOT set) — operator values always
	// win. A pointer so omitempty drops the key entirely when
	// unset.
	ModelsDev *ModelsDevConfig `json:"models_dev,omitempty"`

	// LiteLLM is the opt-in live price/context sync from
	// LiteLLM's public model cost map (B4-03/12, Stage 1):
	// the same shape as ModelsDevConfig — Enabled/URL/
	// IntervalMinutes/TimeoutSeconds. Nil (or Enabled
	// false) means the sync is off and nothing touches the
	// network. An empty URL means the public LiteLLM cost
	// map (modelsync.DefaultLiteLLMURL, applied in main.go
	// — the config package cannot name it, modelsync
	// imports config). Prices land in the price book's
	// synced layer stamped with the "litellm" source, so
	// they never wipe the models.dev layer (and vice versa);
	// models.dev wins for a pair both sources cover.
	LiteLLM *LiteLLMConfig `json:"litellm,omitempty"`

	// ArenaElo is the operator's opt-in Arena ELO sync block
	// (B2-14): the leaderboard sync filling the model-
	// intelligence store's arena_elo source. Nil (or
	// Enabled false) means the sync is off and nothing
	// touches the network. A pointer so omitempty drops the
	// key entirely when unset.
	ArenaElo *ArenaEloConfig `json:"arena_elo,omitempty"`

	// Ledger tunes the spend ledger's cost options. Nil
	// means defaults: flat-rate providers price per token
	// like any other provider (never silently $0).
	Ledger *LedgerConfig `json:"ledger,omitempty"`

	// MCP tunes the MCP selector mode (see internal/mcptools):
	// which external MCP servers the gateway exposes to models,
	// and how their tools reach the prompt. Nil means no MCP
	// servers are configured — nothing is injected and the
	// dashboard's MCP section reports the honest empty state. A
	// pointer so omitempty drops the key entirely when unset.
	MCP *MCPConfig `json:"mcp,omitempty"`

	// Compression tunes lossless-first prompt compression (see
	// internal/compress, CHECKLIST 183): LosslessModels names
	// the models whose prompts get the lossless steps by default
	// (dedupe repeated system blocks, strip redundant whitespace,
	// canonicalise repeated tool results) — no per-request
	// opt-in needed for those models. A per-request
	// "compression.lossless": false still opts a single request
	// OUT, and a per-request opt-in applies to any model. Lossy
	// compression is NEVER configured here: it needs an explicit
	// per-call "compression.lossy_max_prompt_tokens" from the
	// client, every time. Nil means no model compresses by
	// default. A pointer so omitempty drops the key entirely
	// when unset.
	Compression *CompressionConfig `json:"compression,omitempty"`

	// Pools tunes the warm HTTP/2 connection pools (see
	// internal/provider/pool.go): one transport per (provider, key),
	// created at startup and warmed with a real request, with
	// MaxIdleConnsPerHost sized by Little's Law — N = ceil(λ × W)
	// from expected_rps × expected_latency_ms, clamped to
	// [min_conns, max_conns] (defaults 20 req/s × 1000 ms → 20 idle
	// connections, min 2, max 200). max_idle_per_host overrides the
	// derivation when > 0. Nil means defaults. A pointer so
	// omitempty drops the key entirely when unset.
	Pools *provider.PoolConfig `json:"pools,omitempty"`

	// KeyBudgets is the operator-configured rolling 5-hour USD cap
	// per gateway key (see internal/ledger/keybudget.go): key ID
	// -> cap + unpriced-usage policy. Nil means no key has a
	// rolling budget (unlimited). At startup the entries are
	// applied into the ledger (the durable store — a restart
	// keeps them even if the config file is later edited), and the
	// dashboard's POST /dashboard/key-budgets writes both the
	// ledger row and this block, so the two never drift apart.
	KeyBudgets map[string]KeyBudgetConfig `json:"key_budgets,omitempty"`

	// QuotaPools is the operator-configured fair-share pools
	// (M5.4, see quotapool.go): each pool shares one quota
	// across its member keys. At startup the entries are applied
	// into the ledger (the durable store), and the dashboard's
	// POST /dashboard/quota-pools writes both the ledger rows
	// and this block, so the two never drift apart.
	QuotaPools []QuotaPoolConfig `json:"quota_pools,omitempty"`

	// QuotaSchedules is the operator-configured time-aware quota
	// windows on the pools (M5.9, see quotaschedule.go): allow
	// (whitelist) or block (blackout) windows, keep-free reserve
	// floors, and per-window own-consumption budgets. At startup
	// the entries are applied into the ledger (the durable
	// store), and the dashboard's POST /dashboard/quota-
	// schedules writes both the ledger rows and this block.
	QuotaSchedules []QuotaScheduleConfig `json:"quota_schedules,omitempty"`

	// WebhooksDurableQueue enables the persistent outbound webhook delivery
	// queue (A4 item 5): pending/failed deliveries persist under the data
	// dir and are redelivered once at startup, and a delivery history is
	// retained. Off by default (in-memory, matching prior behaviour).
	WebhooksDurableQueue bool `json:"webhooks_durable_queue,omitempty"`

	// Webhooks is the operator-configured outbound webhook
	// endpoints (M5.9, see quotaschedule.go). Today only the
	// "quota.exceeded" event is emitted, best-effort, when a
	// quota schedule blocks a request.
	Webhooks []WebhookConfig `json:"webhooks,omitempty"`

	// Teams is the operator-configured cost centers (M5.12, see
	// team.go): named groupings of gateway keys with an optional
	// shared rolling 1d/7d/30d USD budget. At startup the entries
	// are applied into the ledger (the durable store), and the
	// dashboard's POST /dashboard/teams writes both the ledger
	// rows and this block.
	Teams []TeamConfig `json:"teams,omitempty"`

	// InternalBudgets is the operator-configured caps on total
	// spend (M5.11, see internalbudget.go): scope global /
	// provider / model / pool × period daily / weekly / monthly
	// × limit type currency / tokens / requests. At startup the
	// entries are applied into the ledger (the durable store),
	// and the dashboard's POST /dashboard/internal-budgets
	// writes both the ledger rows and this block.
	InternalBudgets []InternalBudgetConfig `json:"internal_budgets,omitempty"`

	// Shortlist is the operator's curated model shortlist
	// (CHECKLIST 199-200): model IDs pinned to the top of the
	// catalog, in list order, clearly marked in GET /v1/models
	// and GET /dashboard/models (shortlisted: true) and first in
	// both lists. An entry pins any row whose model ID equals the
	// entry ("gpt-4o", matched wherever it is offered) and any
	// row whose "providerID/modelID" equals the entry (matched
	// only for that provider) — the two readings of an entry
	// containing "/" are both applied, so a model ID that itself
	// contains "/" keeps working as a plain pin. Entries naming
	// models no provider offers are ignored, never an error — the
	// shortlist is a display pin, not a routing rule, and a
	// pinned model that disappears is simply not shown.
	Shortlist []string `json:"shortlist,omitempty"`

	Extensions map[string]json.RawMessage `json:"-"`
}

// KeyBudgetConfig is one key's rolling 5-hour USD budget in the
// config file: the cap in USD, and what happens to usage whose
// price is unknown — "fail_closed" (default: block the request
// with a 400 naming the unpriced models) or "count_as_zero"
// (let it through counting $0, still enforcing priced spend).
// A limit of 0 means no spend is allowed under enforcement.
//
// M5.1: calendar-window caps (nil = unset, unlimited for that
// window) + the active-window selector. ResetInterval selects the
// enforced calendar window (daily / weekly / monthly; "" =
// daily); ResetTime ("HH:MM", "" = "00:00") is the UTC time of
// day the window rolls over. WarningThreshold (0–1, 0 = default
// 0.8) is the fraction of the active limit at which the key
// reports as warning.
type KeyBudgetConfig struct {
	LimitUSD         float64  `json:"limit_usd"`
	UnpricedPolicy   string   `json:"unpriced_policy,omitempty"`
	DailyLimitUSD    *float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD   *float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD  *float64 `json:"monthly_limit_usd,omitempty"`
	WarningThreshold float64  `json:"warning_threshold,omitempty"`
	ResetInterval    string   `json:"reset_interval,omitempty"`
	ResetTime        string   `json:"reset_time,omitempty"`
}

// ModelsDevConfig is the persisted shape of the opt-in live
// price/context sync from models.dev (see internal/modelsync).
// The sync is OFF unless Enabled is true: with it off the router
// never contacts models.dev. When on, the router fetches the
// public models.dev catalog (URL, default
// https://models.dev/api.json) at startup and every
// IntervalMinutes thereafter (0 = once at startup only), with
// each fetch bounded by TimeoutSeconds (0 = default 15s).
// Only the BASE tier of a model's models.dev cost is used
// (input/output/cached-input USD per 1M tokens); tiered prices
// (context_over_200k and per-tier entries) are ignored, because
// the router cannot know pre-send which tier a request falls
// in, and guessing would invent a price.
type ModelsDevConfig struct {
	Enabled         bool   `json:"enabled,omitempty"`
	URL             string `json:"url,omitempty"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
}

// DefaultModelsDevURL is the public models.dev catalog. It is a
// default, not a secret and not a fallback: an operator who
// enables the sync with an empty URL gets this one.
const DefaultModelsDevURL = "https://models.dev/api.json"

// DefaultModelsDevTimeoutSeconds bounds one models.dev fetch when
// TimeoutSeconds is unset (0).
const DefaultModelsDevTimeoutSeconds = 15

// EffectiveURL returns the catalog URL in force: the configured
// URL, or the models.dev default when empty.
func (c *ModelsDevConfig) EffectiveURL() string {
	if c != nil && strings.TrimSpace(c.URL) != "" {
		return strings.TrimSpace(c.URL)
	}
	return DefaultModelsDevURL
}

// EffectiveTimeoutSeconds returns the fetch timeout in force: the
// configured value, or the default 15s when unset (0).
func (c *ModelsDevConfig) EffectiveTimeoutSeconds() int {
	if c != nil && c.TimeoutSeconds > 0 {
		return c.TimeoutSeconds
	}
	return DefaultModelsDevTimeoutSeconds
}

// ArenaEloConfig is the persisted shape of the operator's
// opt-in Arena ELO sync block (B2-14, "arena_elo" in the
// config file, next to models_dev). The sync itself is off
// unless Enabled is true — with it off nothing touches the
// network, exactly like the models_dev sync.
type ArenaEloConfig struct {
	Enabled         bool   `json:"enabled,omitempty"`
	URL             string `json:"url,omitempty"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
}

// LiteLLMConfig is the persisted shape of the opt-in LiteLLM
// price/context sync block (B4-03/12, "litellm" in the config
// file, next to models_dev). Same shape as ModelsDevConfig:
// Enabled off means the sync is off and nothing touches the
// network; URL empty means the public LiteLLM cost map;
// IntervalMinutes 0 means once at startup only; TimeoutSeconds
// 0 means the default 15s.
type LiteLLMConfig struct {
	Enabled         bool   `json:"enabled,omitempty"`
	URL             string `json:"url,omitempty"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
	TimeoutSeconds  int    `json:"timeout_seconds,omitempty"`
}

// LedgerConfig is the persisted shape of the ledger cost
// options. FlatRateAsZero is the flat-rate opt-in (B4-13):
// when true, a flat-rate (subscription / coding-plan /
// cookie-web) provider's usage costs $0 in the ledger
// instead of its per-token estimate. Default false.
type LedgerConfig struct {
	FlatRateAsZero bool `json:"flat_rate_as_zero,omitempty"`
}

// MCPServerConfig is one operator-configured MCP server in the
// config file: a local MCP server process the gateway spawns on
// stdio (command + args). The command is the operator's own
// choice from their own config file — it never arrives over HTTP
// and is never taken from a request.
type MCPServerConfig struct {
	// Name identifies the server: it prefixes every tool it
	// exposes (server__tool) and is what the dashboard shows.
	// Must be unique across servers.
	Name string `json:"name"`
	// Command is the executable to spawn; Args its arguments.
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// MCPConfig is the persisted shape of the MCP selector settings.
// Mode is "selector" (default — inject only mcp_tools_get and
// mcp_tools_execute, no matter how many servers or tools are
// configured) or "direct" (opt-in — inject every discovered tool
// schema). MaxIterations caps the agent loop's follow-up model
// calls per request (default 10, highest accepted 50); hitting
// the cap stops the loop honestly instead of looping forever.
//
// Controls (CHECKLIST 392, M9.6 follow-up) is the operator's
// opt-in to the client-side MCP hardening in internal/mcptools
// (allowlist + MaxCalls/MaxDuration spend budget, enforced in the
// single ExecuteDetailed choke point). Presence is the switch:
//
//   - Controls == nil (no "controls" section in the config file):
//     legacy behaviour — the production registry is built with
//     mcptools.New, allow-all, exactly as before M9.6. The
//     operator has not opted into restrictions, so none are
//     imposed. This is the operator's explicit choice (absence),
//     NOT a fail-open hole: it does not touch the server-side
//     M9.1 scopes, which are enforced on the router-as-server
//     side regardless.
//   - Controls != nil (section present, even empty): enforced
//     per Controls from construction via NewWithControls —
//     fail-closed, so an empty Allow list denies every tool.
//     Present-but-empty ("controls": {}) is a deliberate
//     deny-all, not a mistake, and Validate does not reject it.
//
// Failed dispatches consume budget (M9.6 review nit 1 —
// DOCUMENTED, not refunded): the budget reservation is made
// before dispatch inside ExecuteDetailed and is NOT refunded
// when the server call fails, so a failed dispatch counts as one
// spent call against MaxCalls. A refund would need a change to
// the mcptools choke point itself (outside this wiring slice);
// counting attempts rather than successes is the honest,
// predictable rule, and it is pinned by a test in
// cmd/codvian-router (TestNewMCPRegistryFailedDispatchConsumesBudget).
type MCPConfig struct {
	Mode          string             `json:"mode,omitempty"`
	MaxIterations int                `json:"max_iterations,omitempty"`
	Servers       []MCPServerConfig  `json:"servers,omitempty"`
	Controls      *mcptools.Controls `json:"controls,omitempty"`
}

// mcpControlsJSON is the config-file wire shape of
// mcptools.Controls: identical except max_duration accepts a Go
// duration string ("5m", "30s", "1h30m") as well as a JSON number
// (nanoseconds — the encoding/json shape of time.Duration, which
// is what a verbatim embedding would read). A duration string is
// the form operators write; the number form stays accepted so a
// file written from a marshalled mcptools.Controls still loads.
type mcpControlsJSON struct {
	Allow       []string        `json:"allow,omitempty"`
	Deny        []string        `json:"deny,omitempty"`
	MaxCalls    int             `json:"max_calls,omitempty"`
	MaxDuration json.RawMessage `json:"max_duration,omitempty"`
}

// parseMCPControlsDuration decodes one max_duration value: a
// string parsed with time.ParseDuration, or a JSON number read as
// nanoseconds. Anything else (boolean, object, malformed string,
// non-integer number) is a load error naming the field.
func parseMCPControlsDuration(raw json.RawMessage) (time.Duration, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, fmt.Errorf("config: mcp.controls.max_duration must be a duration string (e.g. \"5m\") or a number of nanoseconds: %w", err)
		}
		d, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("config: mcp.controls.max_duration %q is not a valid duration (e.g. \"5m\", \"30s\", \"1h30m\"): %v", s, err)
		}
		return d, nil
	}
	var nanos int64
	if err := json.Unmarshal(raw, &nanos); err != nil {
		return 0, fmt.Errorf("config: mcp.controls.max_duration must be a duration string (e.g. \"5m\") or a number of nanoseconds: %w", err)
	}
	return time.Duration(nanos), nil
}

// UnmarshalJSON decodes an MCPConfig, accepting max_duration as a
// duration string or a number of nanoseconds (see
// mcpControlsJSON). Any other field decodes exactly as before;
// an invalid duration is a load error, never silently ignored.
func (c *MCPConfig) UnmarshalJSON(data []byte) error {
	type mcpConfigAlias MCPConfig
	var aux struct {
		mcpConfigAlias
		Controls *mcpControlsJSON `json:"controls,omitempty"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*c = MCPConfig(aux.mcpConfigAlias)
	if aux.Controls == nil {
		c.Controls = nil
		return nil
	}
	d, err := parseMCPControlsDuration(aux.Controls.MaxDuration)
	if err != nil {
		return err
	}
	c.Controls = &mcptools.Controls{
		Allow:       aux.Controls.Allow,
		Deny:        aux.Controls.Deny,
		MaxCalls:    aux.Controls.MaxCalls,
		MaxDuration: d,
	}
	return nil
}

// MarshalJSON encodes an MCPConfig, writing max_duration as a
// duration string ("5m0s") when set so a saved config stays
// human-readable and re-loads to the same duration. A zero
// max_duration is omitted (0 = unlimited).
func (c MCPConfig) MarshalJSON() ([]byte, error) {
	type mcpConfigAlias MCPConfig
	var aux struct {
		mcpConfigAlias
		Controls *mcpControlsJSON `json:"controls,omitempty"`
	}
	aux.mcpConfigAlias = mcpConfigAlias(c)
	if c.Controls != nil {
		ctl := &mcpControlsJSON{
			Allow:    c.Controls.Allow,
			Deny:     c.Controls.Deny,
			MaxCalls: c.Controls.MaxCalls,
		}
		if c.Controls.MaxDuration != 0 {
			raw, err := json.Marshal(c.Controls.MaxDuration.String())
			if err != nil {
				return nil, err
			}
			ctl.MaxDuration = raw
		}
		aux.Controls = ctl
	}
	return json.Marshal(aux)
}

// ControlsEngaged reports whether the operator opted into the
// M9.6 client-side controls: true only when the "controls"
// section is present in the config. A nil receiver or a nil
// Controls pointer means legacy allow-all.
func (c *MCPConfig) ControlsEngaged() bool {
	return c != nil && c.Controls != nil
}

// CompressionConfig is the persisted shape of the lossless-first
// compression settings (CHECKLIST 183). LosslessModels names the
// models whose prompts get the lossless steps by default. There is
// deliberately no lossy field here: lossy compression is a
// per-call client opt-in only ("compression.lossy_max_prompt_tokens"
// in the request body), never an operator default.
type CompressionConfig struct {
	LosslessModels []string `json:"lossless_models,omitempty"`
}

// EffectiveMode returns the mode in force: "direct" only when
// explicitly configured (case-insensitive), "selector" otherwise.
func (c *MCPConfig) EffectiveMode() string {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.Mode), "direct") {
		return "direct"
	}
	return "selector"
}

// EffectiveMaxIterations returns the loop cap in force: the
// configured value, or the default 10 when unset (0) or negative.
// The 10 matches mcptools.DefaultMaxIterations; it is spelled out
// here (not imported) because config stays a low-level package.
func (c *MCPConfig) EffectiveMaxIterations() int {
	if c != nil && c.MaxIterations > 0 {
		return c.MaxIterations
	}
	return 10
}

// AutoRouterConfig is the persisted shape of the auto-router
// settings in the config file. Thresholds are confidence dials:
// a tier match below the threshold falls back to the default tier
// instead of guessing (see internal/autorouter). TierModels maps a
// tier name ("coding", "chat", "reasoning", "quick-qa") to concrete
// model IDs in preference order; a tier with no entry uses
// DefaultModel. Keys are never stored here — only model names.
type AutoRouterConfig struct {
	// Threshold is the default confidence dial (0.5 when 0).
	// Must be in (0, 1] when set.
	Threshold float64 `json:"threshold,omitempty"`
	// Thresholds overrides Threshold per virtual model name
	// ("auto/fast", ...). Each entry must be in (0, 1].
	Thresholds map[string]float64 `json:"thresholds,omitempty"`
	// DefaultTier is the fallback tier ("reasoning" when empty).
	DefaultTier string `json:"default_tier,omitempty"`
	// DefaultModel is the concrete model used when the picked
	// tier has no TierModels entry. Empty means "no model" —
	// auto-routing then fails honestly instead of inventing one.
	DefaultModel string `json:"default_model,omitempty"`
	// TierModels maps tier names to concrete model IDs.
	TierModels map[string][]string `json:"tier_models,omitempty"`
	// IntentTiers maps intent tier names ("fast", "balanced",
	// "best") to their operator-defined (model, effort) pairs
	// (see internal/autorouter intent.go): a request naming
	// "intent/<tier>" resolves to that pair. A tier with no
	// entry is unmapped — a request naming it fails honestly
	// naming the tier, never a silent default model. Empty by
	// default: no tier is mapped until the operator says what
	// it means (no invented default mapping).
	IntentTiers map[string]IntentTierConfig `json:"intent_tiers,omitempty"`
	// FeedbackLearnedWeights turns the Phase 2 feedback prototype
	// on: per-tier score nudges learned from accept / edit /
	// discard / regenerate signals (see internal/feedback) apply
	// to routing. Default false (OFF): with the flag off the
	// feedback store still records signals, but the nudges are
	// exactly zero, so routing is unchanged. Even with the flag
	// on, a tier gets no nudge until it has 10 signals, and the
	// nudge is bounded to +/-0.05.
	FeedbackLearnedWeights bool `json:"feedback_learned_weights,omitempty"`
	// LearnedRouter turns the learned auto-router on (see
	// internal/learned, RouteLLM pattern): per-tier win
	// probabilities predicted from the recorded heuristic scores
	// compose into routing as a bounded (+/-0.05) additive
	// signal. Default false (OFF). Even with the flag on, the
	// model stays OFF until real evidence exists: at least 50
	// labelled outcomes, and a held-out calibration error at
	// most 0.15 with log-loss beating the base rate — otherwise
	// routing is exactly unchanged (item-115 no-evidence rule).
	// With the flag off the model still trains and reports its
	// status honestly on GET /dashboard/learned, but its
	// predictions never apply.
	LearnedRouter bool `json:"learned_router,omitempty"`
	// CodeAllow / CodeExclude are the auto/code mode's pool
	// allowlist / denylist (see internal/autorouter code.go):
	// when CodeAllow is non-empty only listed models compete;
	// listed CodeExclude models never compete. Empty means no
	// list narrowing.
	CodeAllow   []string `json:"code_allow,omitempty"`
	CodeExclude []string `json:"code_exclude,omitempty"`
	// CodeRequireCapabilities lists the capabilities every
	// auto/code candidate must have (e.g. "tools"). A model
	// whose declared capabilities (CodeCapabilities) miss any
	// required one is dropped — and a model with NO declared
	// capabilities is dropped too when this list is non-empty,
	// because an unverified capability must not be assumed.
	CodeRequireCapabilities []string `json:"code_require_capabilities,omitempty"`
	// CodeCapabilities is the operator-declared per-model
	// capability set for the auto/code pool narrowing. These are
	// the OPERATOR's declarations, not measured properties: a
	// model absent from the map declares nothing. The router
	// never guesses a capability from a model name.
	CodeCapabilities map[string][]string `json:"code_capabilities,omitempty"`
}

// IntentTierConfig is one intent tier's operator-defined meaning
// in the config file: the concrete model it maps to, and the
// reasoning effort pinned for it ("" = no effort pinned; "low",
// "medium" or "high" otherwise).
type IntentTierConfig struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
}

// PluginsConfig is the persisted shape of the plugin list in the
// config file (see internal/plugins, CHECKLIST 201). Enabled names
// built-in plugins in pipeline order. There is no code path here
// by design: plugins are compiled-in Go only.
//
// CostCapUSD is the cost-cap plugin's per-request cap in USD
// (CHECKLIST 239): a request whose estimated cost exceeds it is
// refused 402 before any provider is dialled. Nil (absent) means
// unlimited — the plugin counts its calls and never denies. A
// negative cap is rejected at load. A cap set without "cost-cap"
// in Enabled is also rejected: an inert cap would silently do
// nothing, and the operator plainly meant to cap.
type PluginsConfig struct {
	Enabled    []string `json:"enabled,omitempty"`
	CostCapUSD *float64 `json:"cost_cap_usd_per_request,omitempty"`
}

// ResponsesConfig tunes the Responses API (/v1/responses).
// SessionTokenCap is the per-session token cap for Responses
// continuations (CHECKLIST 263–264, follow-up to 243–244):
// a session whose counted tokens plus the next turn's prompt
// estimate would exceed it refuses the turn with 429
// (session_token_limit) before any provider is dialled. 0 (the
// default, and the value when the whole responses block is
// absent) means counting-only: turns are counted and reported
// on the dashboard, never refused. Negative is rejected at
// config load — a cap that refuses everything (or nothing, by
// clamping) would be a silent misconfiguration.
type ResponsesConfig struct {
	SessionTokenCap int `json:"session_token_cap,omitempty"`
}

// CacheConfig is the persisted shape of cache persistence (M6.2):
// one SQLite path per cache layer. Empty means that layer stays
// memory-only. The two layers keep separate files so either can
// be wiped independently; the exact file also holds the savings
// rows (see internal/cache/persist.go). The semantic layer has
// a second, Redis, persistence tier (see
// internal/cache/semantic_redis.go): when RedisAddr is set the
// semantic layer persists to Redis instead of SQLite.
type CacheConfig struct {
	// ExactPath is the SQLite file for the exact-match layer
	// (layer 1). Empty means memory-only.
	ExactPath string `json:"exact_persist_path,omitempty"`
	// SemanticPath is the SQLite file for the semantic layer
	// (layer 3). Empty means memory-only. Ignored when
	// RedisAddr is set: Redis replaces SQLite for this layer.
	SemanticPath string `json:"semantic_persist_path,omitempty"`
	// RedisAddr is the Redis server address, host:port, for the
	// semantic layer's Redis persistence tier. Empty means the
	// semantic layer keeps its SQLite tier (or memory-only
	// when no semantic_persist_path is set either). When set,
	// the semantic layer persists to Redis instead of SQLite —
	// the SQLite file is not opened at all.
	RedisAddr string `json:"redis_addr,omitempty"`
	// RedisPassword is the AUTH password for the Redis server.
	// Empty means no AUTH.
	RedisPassword string `json:"redis_password,omitempty"`
	// RedisDB is the Redis database index. 0 is the default.
	RedisDB int `json:"redis_db,omitempty"`
	// RedisTLS enables TLS to the Redis server (system roots,
	// TLS 1.2+, ServerName from the address host).
	RedisTLS bool `json:"redis_tls,omitempty"`
	// SemanticThreshold is the operator-configured semantic
	// similarity floor (M6.7): a stored entry must score at
	// least this to count as a hit. 0 means unset (the default
	// 0.85 — see EffectiveSemanticThreshold). A configured value
	// must be within [0.85, 1.0]; anything else is rejected by
	// Validate, never silently clamped — a floor below 0.85
	// would widen hits past what the cache was built to serve,
	// and a floor above 1.0 could never hit. A per-request
	// threshold header can only raise this floor for one
	// request, never lower it.
	SemanticThreshold float64 `json:"semantic_threshold,omitempty"`
}

// EffectiveSemanticThreshold returns the semantic similarity
// floor this config selects: SemanticThreshold when set, else
// the 0.85 default. A nil receiver is the default too. It does
// NOT validate: call Validate first — an out-of-range value
// here is a load-time error, not a value to clamp at use.
func (c *CacheConfig) EffectiveSemanticThreshold() float64 {
	if c == nil || c.SemanticThreshold == 0 {
		return 0.85
	}
	return c.SemanticThreshold
}

// A2AConfig tunes A2A task retention (M9.8 follow-up,
// docs/design/a2a-retention-design.md §4): the terminal-task
// retention window, the per-key task cap, and the prune on/off
// switch. Retention and pruning are ON by default whenever A2A
// is enabled; the operator turns pruning off only by an explicit
// false. There is no way to disable the cap: 0 means the 1,000
// default, and eviction is the safety net that bounds disk when
// the prune job fails.
type A2AConfig struct {
	// RetentionDays is the terminal-task retention window in
	// days. 0/absent = 7. Negative is rejected by Validate.
	RetentionDays int `json:"retention_days,omitempty"`
	// MaxTasksPerKey is the per-key task cap. 0/absent = 1000.
	// Negative is rejected by Validate. Any positive value is
	// accepted (an operator who sets 1 gets exactly the
	// all-non-terminal reject behaviour of design §3).
	MaxTasksPerKey int `json:"max_tasks_per_key,omitempty"`
	// PruneEnabled turns the prune job on/off. Nil/absent =
	// true (pruning is ON whenever A2A is enabled); explicit
	// false skips pruning entirely. A pointer so "absent" is
	// distinguishable from an explicit false.
	PruneEnabled *bool `json:"prune_enabled,omitempty"`
}

// DefaultA2ARetentionDays is the default terminal-task retention
// window in days (design §1/§4).
const DefaultA2ARetentionDays = 7

// DefaultA2AMaxTasksPerKey is the default per-key task cap
// (design §3/§4).
const DefaultA2AMaxTasksPerKey = 1000

// EffectiveRetentionDays returns the retention window in days:
// the configured value, or 7 when unset (0) or nil. It does NOT
// validate: call Validate first — a negative value here is a
// load-time error, not a value to clamp at use.
func (c *A2AConfig) EffectiveRetentionDays() int {
	if c == nil || c.RetentionDays == 0 {
		return DefaultA2ARetentionDays
	}
	return c.RetentionDays
}

// EffectiveMaxTasksPerKey returns the per-key task cap: the
// configured value, or 1000 when unset (0) or nil. It does NOT
// validate (see EffectiveRetentionDays).
func (c *A2AConfig) EffectiveMaxTasksPerKey() int {
	if c == nil || c.MaxTasksPerKey == 0 {
		return DefaultA2AMaxTasksPerKey
	}
	return c.MaxTasksPerKey
}

// EffectivePruneEnabled returns whether pruning runs: true
// unless the operator explicitly set false. Nil receiver or nil
// PruneEnabled both mean true (pruning ON by default).
func (c *A2AConfig) EffectivePruneEnabled() bool {
	if c == nil || c.PruneEnabled == nil {
		return true
	}
	return *c.PruneEnabled
}

// GuardrailsConfig is the persisted shape of the guardrail blocking
// policy in the config file. It mirrors guardrails.Config exactly
// (same JSON names) so main.go can seed the runtime store from it.
// Log redaction is deliberately absent: it is always on and is not
// a setting.
//
// DisabledTypes is the per-type toggle selection (G18 Area 7):
// built-in pattern labels (see guardrails ListTypes) whose findings
// are dropped from Check results. An unknown label fails config
// load, naming the label.
//
// External mirrors guardrails.ExternalConfig: one entry per slow
// external classifier (CHECKLIST 185), each in sync or async mode.
// A blocking kind (pii/secret/injection) in async mode is rejected
// by Validate — a secret must never reach the provider while a
// verdict is in flight.
type GuardrailsConfig struct {
	BlockPII       bool                      `json:"block_pii,omitempty"`
	BlockSecrets   bool                      `json:"block_secrets,omitempty"`
	BlockInjection bool                      `json:"block_injection,omitempty"`
	DisabledTypes  []string                  `json:"disabled_types,omitempty"`
	External       []ExternalGuardrailConfig `json:"external,omitempty"`
	// CustomPatterns is the operator-configured custom regex
	// patterns (B3-18): one entry per pattern, applied by the
	// store at startup via AddCustomPattern (see
	// buildGuardrailStore). Kind is "pii" or "injection"
	// (empty means "pii"); Severity is advisory (0-10).
	// A bad pattern fails startup, naming the label.
	CustomPatterns []CustomPatternConfig `json:"custom_patterns,omitempty"`
	// RequestRewrite redacts PII/secrets in the request in
	// place before the provider is dialled (B3-20). Off by
	// default: the check still runs, only the rewrite is
	// opt-in.
	RequestRewrite bool `json:"request_rewrite,omitempty"`
	// ResponseSanitization applies the store's response
	// guard to delivered responses (B3-21/B3-22). Off by
	// default.
	ResponseSanitization bool `json:"response_sanitization,omitempty"`
	// ResponseGuardMode is the response-sanitization mode:
	// "redact" (default), "warn" or "block". Empty means
	// "redact". Any other value fails config load.
	ResponseGuardMode string `json:"response_guard_mode,omitempty"`
	// ReversibleVault mints reversible PII tokens in the
	// request rewrite instead of dead placeholders (B3-19,
	// parity tracker:782 — the capability behind the reference router's
	// PII_REVERSIBLE_VAULT_ENABLED). Off by default. It
	// only takes effect when RequestRewrite is also on:
	// the tokens are minted by the rewrite, pre-call, and
	// restored from the vault at exit; a response that
	// echoes a token gets its original value back. The
	// vault file lives in the data dir (pii_vault.json),
	// sealed under the router's master key; secrets are
	// never minted, only PII.
	ReversibleVault bool `json:"reversible_vault,omitempty"`
}

// CustomPatternConfig is the persisted shape of one operator
// custom regex pattern (B3-18): the store's CustomPattern,
// minus the compiled form (built at startup by
// AddCustomPattern, which validates the regex). ID and Label
// must both be non-empty; Kind is "pii" or "injection"
// (empty means "pii"); Severity is advisory (0-10).
type CustomPatternConfig struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Pattern  string `json:"pattern"`
	Kind     string `json:"kind,omitempty"`
	Severity int    `json:"severity,omitempty"`
}

// ExternalGuardrailConfig is the persisted shape of one external
// guardrail: a named check backed by a remote classifier endpoint.
// Mode is "sync" (check before dialling; a BLOCK refuses the
// request) or "async" (check concurrently with the provider call;
// a BLOCK cuts the stream or withholds an undelivered response).
// TimeoutMs caps one check in milliseconds (0 = 5 s default).
type ExternalGuardrailConfig struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Mode      string `json:"mode"`
	Endpoint  string `json:"endpoint"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// RoutingConfig is the persisted shape of the routing-strategy
// settings in the config file. Strategy names are the canonical
// names of internal/strategy (see strategy.All); an empty global
// strategy means "priority".
//
// MIGRATION (M1.11, 2026-11-18): the default global strategy
// changed from "balanced" (re-rank every request by latency,
// success, cost, quality and quota) to "priority" (try targets
// in the operator's declared chain order, no re-ranking).
// Configs that set global_strategy explicitly are unaffected;
// configs that relied on the unset default now route in declared
// order. To keep the old behaviour, set
// routing.global_strategy to "balanced" explicitly. "balanced"
// remains a valid, selectable strategy, and "code" (cost per
// solved task) is now accepted by config validation as declared.
//
// MIGRATION (M3, 2026-11-18): "weighted" now means the reference router's
// per-target weight draw (uniform until chain per-target weights
// land in Phase C). The old operator weight-set behaviour moved
// to "custom-weights" (alias "custom"); configs that set
// global_strategy or a chain strategy to "weighted" with
// weighted_weights must rename it to "custom-weights" to keep
// the old ranking. "weighted_weights" keeps its JSON field name.
//
// ChainStrategies maps a model ID (the chain) to its strategy; a chain with no entry inherits the
// global strategy, and a per-request "auto:<axis>" alias still
// wins over both. DeadlineBudgetMs is the per-combo time budget in
// milliseconds (0 = disabled). WeightedWeights holds the custom
// weights for the "custom-weights" strategy (latency, success, cost,
// quality, quota); all five are optional and normalised to sum to
// 1 at use, so only their ratios matter.
//
// CooldownMode is the global cooldown mode for retryable failures
// (research-log entry 183): "hard" (the default — a cooling
// provider is skipped until its cooldown ends) or "soft-demote"
// (it moves to the end of the order instead and is still tried
// once). Empty means "hard". ChainCooldownModes maps a model ID to
// its cooldown mode; a chain with no entry inherits the global
// mode. An unknown mode is rejected at load, naming the field.
//
// ContextFallbackModels is the dedicated context-window fallback
// chain (CHECKLIST 159): model IDs, in preference order, that a
// prompt jumps to when it does not fit any model in the requested
// chain. Each entry is resolved against the providers that offer
// it, exactly like a normal chain; an entry no provider offers
// contributes nothing (it is not an error — the model may arrive
// later via a live provider add). Empty means no dedicated chain:
// a prompt that fits nothing fails honestly pre-call with
// ErrContextTooLarge. ContextReserveTokens is the response reserve
// (in tokens) held back when the caller did not set max_tokens:
// the prompt must fit in window minus this reserve. 0 means the
// engine default (4096); a request max_tokens, when set, replaces
// it for that request. Negative is rejected at load.
//
// RefusalChains (CHECKLIST 161) maps a model ID to its
// content-policy refusal chain: the ordered provider IDs tried —
// once each, never the refusing target again — when a target
// refuses the request on content-policy / safety grounds. It is
// configured separately from the failure chain: the operator names
// the targets known to accept that content class. A model with no
// entry inherits the default (the failure chain's remaining
// targets, in order, each once); an explicitly empty list means
// "no refusal routing for this model" (the first refusal ends the
// request in an honest 403 / -32001 at once). Provider IDs are
// validated for shape only at load (non-empty, no duplicates — a
// name with no live client is skipped at request time, never
// dialled).
//
// ModelEquivalents (CHECKLIST 257) maps a model ID to the
// equivalent models, in preference order, that failover may try
// AFTER the same model in another region and BEFORE any
// self-hosted model: the operator declares which models are
// equivalent — the router never guesses equivalence from a name,
// a price, or a benchmark. An entry no provider offers
// contributes nothing (it is not an error — the model may arrive
// later via a live provider add). A model with no entry has no
// equivalents: failover stays on the same model across regions
// and providers, then self-hosted. A model must never list
// itself as its own equivalent, and an entry must not repeat a
// model (both are load errors naming the field).
// ModelLockoutConfig is the operator-facing form of the model
// lockout (M4.3, router.LockoutConfig): per-(provider, key, model)
// lockout after repeated failures. Off unless Enabled is true
// (the reference router's default). Zero sub-values mean the router defaults
// (403/404/429/502/503/504; base 120 s; max 30 min; max 10 steps).
type ModelLockoutConfig struct {
	Enabled        bool  `json:"enabled,omitempty"`
	ErrorCodes     []int `json:"error_codes,omitempty"`
	BaseCooldownMs int64 `json:"base_cooldown_ms,omitempty"`
	MaxCooldownMs  int64 `json:"max_cooldown_ms,omitempty"`
	MaxSteps       int   `json:"max_steps,omitempty"`
}

// WaitBudgetFileConfig is the operator-facing form of the wait
// budgets (M4.8, router.WaitBudgetConfig). Off unless Enabled is
// true (the default is fail-fast). Zero sub-values mean the
// the reference router defaults (90 s / 5 retries / 300 s total).
type WaitBudgetFileConfig struct {
	Enabled           bool  `json:"enabled,omitempty"`
	MaxWaitPerRetryMs int64 `json:"max_wait_per_retry_ms,omitempty"`
	MaxRetries        int   `json:"max_retries,omitempty"`
	TotalBudgetMs     int64 `json:"total_budget_ms,omitempty"`
}

// maxMediaTimeoutMs is the largest accepted routing.
// media_timeouts_ms value (M8.20 F3): 1 hour, in milliseconds.
// See the MediaTimeoutsMs field comment for the rationale.
const maxMediaTimeoutMs = 3600000

// SLAConfig is the operator-facing form of the SLA objectives
// for the "sla-aware" strategy (M3 Area 1, PARITY row 394;
// router.Config SLA* fields). Every field is a pointer so that
// "unset" (nil — the engine default) is distinguishable from an
// explicitly configured value: in particular an explicit
// max_error_rate of 0 is a real zero-tolerance policy (any error
// violates), not "unset" — a plain float64 0 could never say
// which one the operator meant. The latency actually fed to the
// scorer is the measured p50 EWMA, not a true p95 (no latency
// histogram exists yet; true p95 / latency stddev is deferred —
// see internal/sla and the strategy's Describe text), so
// target_p95_ms names the objective the p50 stand-in is scored
// against. A cost cap of 0 means "no cap", the same as unset:
// the scorer treats 0 as no cap, so only the error rate has a
// meaningful explicit 0.
type SLAConfig struct {
	// TargetP95Ms is the latency objective in milliseconds.
	// Nil means the 2000 ms default (the reference router
	// DEFAULT_SLA_TARGET_P95_MS). When set it must be > 0.
	TargetP95Ms *float64 `json:"target_p95_ms,omitempty"`
	// MaxErrorRate is the tolerated error fraction in [0, 1].
	// Nil means the 0.05 default (the reference router
	// DEFAULT_SLA_MAX_ERROR_RATE). An explicit 0 is
	// zero-tolerance: any measured error violates the SLO.
	MaxErrorRate *float64 `json:"max_error_rate,omitempty"`
	// MaxCostPer1M caps the blended cost in USD per 1M tokens.
	// Nil (or 0) means "no cap": cost then scores relative
	// to the pool maximum instead of against a threshold.
	MaxCostPer1M *float64 `json:"max_cost_per_1m_tokens,omitempty"`
	// HardConstraints sorts by violation count first (fewest
	// SLO breaches wins, score breaks ties) instead of by
	// score alone. Nil means soft (the default).
	HardConstraints *bool `json:"hard_constraints,omitempty"`
}

// DefaultNadirCooldownMs is the failure-cooldown window
// (G5) a nadir block carries when it omits cooldown_ms, in
// milliseconds: 30000. It mirrors
// router.NadirFailureCooldownMs (same number, own const —
// the config package must not import the router package).
const DefaultNadirCooldownMs = 30000

// NadirConfig is the operator-facing form of the Nadir
// decision-endpoint settings for the "nadir" strategy (M3
// Area 1, PARITY row 396; router.Config Nadir* fields). The
// block itself is a pointer, so an absent block means
// unconfigured (fail-closed: auto/nadir never calls any
// external API). A present block sets only what it declares:
// Endpoint is the decision API origin (empty = unconfigured),
// TimeoutMs bounds one decision call in milliseconds
// (<= 0 = the 2000 ms default, clamped to 30000 max by the
// engine). There is deliberately NO api-key field here: the
// key comes from the CODVIAN_ROUTER_NADIR_API_KEY environment
// variable only (see main.go) — a key in a config file gets
// committed, logged, or copied. There is also deliberately
// NO default endpoint and NO env fallback for the endpoint:
// the egress destination must be an explicit, reviewed
// config-file choice, never a silent default that would send
// the prompt's last user turn to a third party the operator
// never chose (a deliberate divergence from the reference router's
// fail-open hardcoded https://api.getnadir.com).
type NadirConfig struct {
	// Endpoint is the origin of a Nadir-compatible decision
	// API (e.g. "https://api.getnadir.com"). Empty means
	// unconfigured. A trailing "/v1" is stripped by the
	// engine (nadirSelect posts to <origin>/v1/bucket), so
	// a pasted base URL cannot double up to /v1/v1/bucket.
	Endpoint string `json:"endpoint,omitempty"`
	// TimeoutMs bounds one decision call in milliseconds.
	// 0 (or absent) means the 2000 ms default; the engine
	// clamps values above 30000 to 30000. Validate rejects
	// negatives and values above 30000 at load.
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// CooldownMs is the failure-cooldown window for the
	// decision endpoint in milliseconds (G5): after a failed
	// decision call the endpoint is skipped for this long
	// before it is tried again. 0 (or absent) means the
	// 30000 ms default (DefaultNadirCooldownMs); Load fills
	// the default in when the nadir block omits it. Unlike
	// TimeoutMs there is no upper cap — an operator may cool
	// a flaky endpoint down longer. Validate rejects
	// negatives at load.
	CooldownMs int `json:"cooldown_ms,omitempty"`
}

type RoutingConfig struct {
	GlobalStrategy   string `json:"global_strategy,omitempty"`
	DeadlineBudgetMs int64  `json:"deadline_budget_ms,omitempty"`
	// StreamMaxLifetimeMs is the hard lifetime bound on a held or
	// relayed stream in milliseconds (CHECKLIST 255, Magpie
	// v0.1.4406): every stream the engine runs is terminated at
	// this age, with a clean terminal event carrying
	// ErrStreamLifetimeExceeded. 0 (default) means the 30-minute
	// default; a negative value disables the cap entirely (the
	// server request timeout / caller context is then the only
	// bound). Unlike deadline_budget_ms, a negative value here is
	// a deliberate opt-out, not a misconfiguration — so it is
	// accepted, never rejected.
	StreamMaxLifetimeMs int64 `json:"stream_max_lifetime_ms,omitempty"`
	// MaxAttempts caps the attempts per request (M4.8, the reference router
	// maxGlobalAttempts clamped [1, 200]). 0 means the engine
	// default (5); the engine clamps any value above 200 to 200,
	// and Validate rejects values outside [1, 200] when set.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// RequireCapabilities (P1.5) lists capability names that every
	// routing attempt must hold with VERIFIED support before it is
	// dialled: e.g. ["vision"], ["tool_calling"]. A model whose
	// capability is unknown or merely declared (unverified) does
	// NOT satisfy a requirement — absence of evidence is never
	// treated as support. Empty (the default) applies no filter.
	// Unknown names are rejected at load, with the full vocabulary
	// in the error, never silently ignored.
	RequireCapabilities []string `json:"require_capabilities,omitempty"`
	// PerTargetTimeoutMs bounds one non-streaming attempt's
	// time-to-first-response in milliseconds (M4.7c). 0 means the
	// 120 s default; -1 disables the timeout. Streaming attempts
	// are bounded instead by preread + the throughput watchdog.
	PerTargetTimeoutMs int64 `json:"per_target_timeout_ms,omitempty"`
	// MediaTimeoutsMs is the opt-in per-endpoint request timeout
	// for the media verify-first surfaces (M8.20 F3), in
	// milliseconds, keyed by endpoint name: "search", "ocr",
	// "video" (covers both the video-generations POST and the
	// video-task GET) or "music". 0 — or an absent key — means
	// the default: 10 minutes for music (MiniMax generation is
	// synchronous and takes tens of seconds to several minutes,
	// with no provider-documented upper bound in the docs the
	// code cites, so the bound is a chosen operator-overridable
	// default, not a provider promise) and the server-wide 120 s
	// default for the other endpoints. Validate rejects unknown
	// keys, negative values, and values above 3,600,000 (1 h) —
	// a hold longer than an hour is not a credible synchronous
	// generation, and the cap catches a milliseconds/seconds
	// unit slip (e.g. 600000000) before it becomes an
	// effectively unbounded request.
	MediaTimeoutsMs map[string]int64 `json:"media_timeouts_ms,omitempty"`
	// PredictiveTtftMs is the predictive TTFT breaker threshold
	// in milliseconds (M4.7a): a target whose mean time-to-first-
	// byte exceeds this (min 5 samples) is skipped while another
	// target remains. 0 disables the prediction (default).
	PredictiveTtftMs int `json:"predictive_ttft_ms,omitempty"`
	// ModelLockout is the opt-in model-lockout block (M4.3).
	// Nil means the lockout stays off (the reference router's default).
	ModelLockout *ModelLockoutConfig `json:"model_lockout,omitempty"`
	// WaitBudgets is the opt-in wait-budget block (M4.8). Nil
	// means fail-fast (the default): 429s fail over at once.
	WaitBudgets *WaitBudgetFileConfig `json:"wait_budgets,omitempty"`
	// PreferredFailoverAfterMs is the opt-in preferred-account
	// response-start deadline in milliseconds (M4.8): the first
	// streaming attempt that produces no first byte within this
	// long rotates to ONE sibling account. 0 disables it
	// (default); 20000 is the the reference router value.
	PreferredFailoverAfterMs int64 `json:"preferred_failover_after_ms,omitempty"`
	// AccountFallbackStrategy is the global account-level
	// fallback strategy (M3 Area 1, PARITY row 388): how the
	// ACCOUNTS (keys) of one provider serving one model are
	// ordered when a request falls back between them. Valid
	// values are the 9 account fallback names (priority,
	// fill-first, round-robin, p2c, random, least-used,
	// cost-optimized, strict-random, weighted); empty means
	// the "fill-first" default (the reference router's default).
	// "expiry-first" is NOT valid: no per-key quota-expiry
	// source exists, so it is rejected at load (see Validate).
	// "weighted" draws uniformly (all weights 1) until a
	// per-account weight source exists; "p2c" at account
	// level is a random draw with a breaker guard (both
	// accounts of one provider share the provider-level
	// score) — both honest fallbacks, no invented signals.
	AccountFallbackStrategy string `json:"account_fallback_strategy,omitempty"`
	// AccountFallbackStrategies overrides
	// AccountFallbackStrategy per provider ID; a provider
	// with no entry inherits the global value, and an empty
	// per-provider value inherits too.
	AccountFallbackStrategies map[string]string `json:"account_fallback_strategies,omitempty"`
	// SLA is the opt-in SLA-objective block (M3 Area 1,
	// PARITY row 394) for the "sla-aware" strategy. Nil
	// means the engine defaults (2000 ms / 5% / no cost
	// cap / soft constraints); a present block overrides
	// only the fields it sets — each field is a pointer,
	// so an explicit 0 max_error_rate is zero-tolerance,
	// not "unset" (see SLAConfig).
	SLA *SLAConfig `json:"sla,omitempty"`
	// Nadir is the opt-in decision-endpoint block (M3 Area
	// 1, PARITY row 396) for the "nadir" strategy. Nil means
	// unconfigured: auto/nadir falls back to priority order
	// and never calls any external API (fail-closed). A
	// present block sets the endpoint and/or the decision
	// timeout; the API key stays out of the file entirely
	// (CODVIAN_ROUTER_NADIR_API_KEY env var, see
	// NadirConfig).
	Nadir           *NadirConfig      `json:"nadir,omitempty"`
	ChainStrategies map[string]string `json:"chain_strategies,omitempty"`
	WeightedWeights *RoutingWeights   `json:"weighted_weights,omitempty"`
	// ChainWeights maps a model ID (the chain, same key as
	// ChainStrategies) to the per-target weights for the
	// "weighted" strategy (M3 Phase C): target key
	// ("provider/model", see strategy.OrderingTarget.Key) ->
	// weight. Weights are operator intent, not measured
	// signals: they say how much of the draw each target
	// should get relative to its peers. A missing map, a
	// missing chain entry, or a missing target entry all mean
	// weight 1 (uniform). A weight of 0 excludes that target
	// from the draw while any positive weight exists (see
	// strategy.WeightedDraw). Unknown chain keys are allowed:
	// chains are operator-defined, and a chain may arrive
	// later via a live provider add. A chain with all-zero
	// weights is rejected at load (see Validate).
	ChainWeights          map[string]map[string]float64 `json:"chain_weights,omitempty"`
	CooldownMode          string                        `json:"cooldown_mode,omitempty"`
	ChainCooldownModes    map[string]string             `json:"chain_cooldown_modes,omitempty"`
	ContextFallbackModels []string                      `json:"context_fallback_models,omitempty"`
	ContextReserveTokens  int                           `json:"context_reserve_tokens,omitempty"`
	RefusalChains         map[string][]string           `json:"refusal_chains,omitempty"`
	ModelEquivalents      map[string][]string           `json:"model_equivalents,omitempty"`

	// Groups is the operator-defined named routing groups
	// (CHECKLIST 173): group name -> ordered provider IDs that
	// make up one named routing target. A group is addressed as
	// one target — by name in a request (routing.group, or the
	// "group:<name>/<model>" model alias) or as a "group:<name>"
	// entry in a refusal chain — and resolves to its members in
	// order, participating in failover, budgets and the ledger
	// exactly like a chain. A group name must be non-empty and
	// must not contain ":" or "/" (both are alias syntax);
	// every member must be a provider ID from this config file
	// (an unknown member is a startup error naming it), with no
	// duplicates in one group, and a group must not be empty.
	// A member may carry a reasoning effort (CHECKLIST 255):
	// "provider@low" runs that provider at effort "low", so a
	// group entry is (model, effort) — the same provider may
	// appear twice at different efforts, and each entry is a
	// distinct attempt. The duplicate rule is on
	// (provider, effort), not provider alone. At request time a
	// member with no live client, a disabled member, or a member
	// that does not offer the requested model is skipped (the
	// same rules as the fallback chain); a group whose members
	// all skip for a model fails the request honestly with a 400
	// naming the group and the model.
	Groups map[string][]string `json:"groups,omitempty"`

	// StrategyTree is the nestable strategy tree for every chain
	// (CHECKLIST 177): a node is a strategy plus children, or a
	// leaf naming a provider (see internal/strategytree) — e.g.
	// cheapest-of[fastest-of[A, B], C]. A nil tree means no tree:
	// chains order by the flat strategy (global / per-chain /
	// per-request alias) exactly as before.
	StrategyTree *strategytree.Node `json:"strategy_tree,omitempty"`
	// StrategyTrees is the per-chain override: the tree for one
	// model (the chain) wins over StrategyTree for that model,
	// exactly like ChainStrategies wins over GlobalStrategy.
	// A model with no entry uses StrategyTree (or the flat
	// strategy when that is nil too).
	StrategyTrees map[string]*strategytree.Node `json:"strategy_trees,omitempty"`
	// ResponseValidation is the 200-OK body validation
	// predicate (:523, G11 Area 2): the config-file surface
	// for router.ResponseValidationConfig. Nil means the
	// built-in error-payload predicate, enabled.
	ResponseValidation *ResponseValidationFileConfig `json:"response_validation,omitempty"`
	// DeadlineFailureMax is the readiness-deadline failure
	// sub-budget (B2-28): this many per-target deadline
	// failures in one request end the combo with a clean 504.
	// 0 means the engine default (3); negative disables the
	// sub-budget.
	DeadlineFailureMax int `json:"deadline_failure_max,omitempty"`
	// MaxProvidersPerRequest caps the DISTINCT providers
	// attempted in one request (B2-29): 0 means the engine
	// default (3). Same-provider retries do not consume it.
	MaxProvidersPerRequest int `json:"max_providers_per_request,omitempty"`
	// MaxRetries caps same-target retries after the first
	// failure, per target (B2-30, the reference router maxRetries):
	// 0 means the engine default (3).
	MaxRetries int `json:"max_retries,omitempty"`
	// RetryDelayMs is the base delay between same-target
	// retries in milliseconds (B2-30, the reference router
	// retryDelayMs): 0 means the engine default (200 ms).
	RetryDelayMs int64 `json:"retry_delay_ms,omitempty"`
	// MaxSetRetries caps same-target retries across the
	// whole set — the whole chain for this request (B2-30,
	// the reference router maxSetRetries): 0 means no set cap.
	MaxSetRetries int `json:"max_set_retries,omitempty"`
	// FailoverBeforeRetry is the explicit failover-before-
	// retry switch (B2-30, the reference router failoverBeforeRetry):
	// when true AND a next target exists, a retryable
	// transient failure (502/503/504) fails over immediately
	// instead of retrying the same target. Default false:
	// the same target is retried first.
	FailoverBeforeRetry bool `json:"failover_before_retry,omitempty"`
	// HedgeDelayMs is how long to wait for the first
	// attempt's first response before starting the next
	// attempt in parallel, in milliseconds (B2-26): 0
	// means the engine default (150 ms); negative disables
	// hedging. The 150 ms default is deliberate and is NOT
	// changed by this wiring (the reference router defaults to off;
	// see the Stage 1 queue for the open decision).
	HedgeDelayMs int64 `json:"hedge_delay_ms,omitempty"`
	// RelayHandoffThreshold is the quota-use fraction at
	// which a successful turn on a handoff provider starts
	// relaying context (B2-01, the reference router
	// HANDOFF_WARNING_THRESHOLD): 0 means the engine
	// default (0.85).
	RelayHandoffThreshold float64 `json:"relay_handoff_threshold,omitempty"`
	// FusionJudgeModel names the model the fusion strategy
	// uses as its LLM judge (A2-3b). Empty = the judge is the
	// FIRST PANEL MEMBER (the fusion default: no operator
	// configuration is needed - the panel's own first target
	// judges). When set, the model must be servable; an
	// unresolvable judge model falls back to the first panel
	// member too, never to a guess.
	FusionJudgeModel string `json:"fusion_judge_model,omitempty"`
	// FusionMinPanel is the minimum number of successful panel
	// answers before the judge synthesizes (A2-3b). 0 means the
	// engine default (2). A panel below it is served directly
	// (the best survivor's verbatim answer, no judge call).
	FusionMinPanel int `json:"fusion_min_panel,omitempty"`
	// FusionMaxPanel caps the panel size (A2-3b). 0 means the
	// engine default (40).
	FusionMaxPanel int `json:"fusion_max_panel,omitempty"`
	// FusionStragglerGraceMs is how long a slow panel member may
	// keep the panel open after the others answered (A2-3b).
	// 0 means the engine default (8000).
	FusionStragglerGraceMs int64 `json:"fusion_straggler_grace_ms,omitempty"`
	// FusionPanelHardTimeoutMs is the hard wall-clock cap on the
	// whole panel (A2-3b). 0 means the engine default (90000).
	FusionPanelHardTimeoutMs int64 `json:"fusion_panel_hard_timeout_ms,omitempty"`
	// RelayExhaustionThreshold is the quota-use fraction
	// at or above which relaying is pointless (B2-01,
	// the reference router HANDOFF_EXHAUSTION_THRESHOLD): 0 means
	// the engine default (0.95); it must be above
	// RelayHandoffThreshold.
	RelayExhaustionThreshold float64 `json:"relay_exhaustion_threshold,omitempty"`
	// RelayTTLMs is how long a generated handoff stays
	// usable, in milliseconds (B2-01, the reference router
	// DEFAULT_TTL_MS): 0 means the engine default (5h).
	RelayTTLMs int64 `json:"relay_ttl_ms,omitempty"`
	// RelayMaxMessages caps the source messages fed to
	// the summariser (B2-01, the reference router
	// DEFAULT_MAX_MESSAGES_FOR_SUMMARY): 0 means the
	// engine default (30).
	RelayMaxMessages int `json:"relay_max_messages,omitempty"`
	// QuotaShares is the operator-configured quota-share
	// table (B2-08): provider ID -> model -> share weight
	// for the quota-share ordering. A missing provider,
	// model or entry means share 1 (equal). Shares must
	// be positive.
	QuotaShares map[string]map[string]float64 `json:"quota_shares,omitempty"`
	// Pipeline knobs (A2-7), read by the engine's "pipeline" strategy.
	// PipelineTaskDetection (string, empty = "off"): "auto" runs the
	// multi-stage pipeline only for complex (code/planning) tasks and
	// serves a single provider for simple ones. PipelineSkipTokensUnder
	// (integer, 0 = disabled): skip the pipeline and serve a single
	// provider when the prompt's local token estimate (≈4 chars/token)
	// is below the threshold. PipelineFallback (string, empty = fail):
	// "single-provider" serves a single provider when the pipeline
	// fails, instead of failing the request. PipelineMaxReflectionLoops
	// (integer, 0 = off): when > 0, appends that many real reflection
	// stages after the main pipeline stages — each re-examines the
	// original prompt plus the previous stage's output and produces a
	// revised output (threaded forward, Optional so it never aborts).
	// Each is validated at load (pipeline_task_detection
	// must be ""/"auto"; pipeline_fallback must be ""/"single-provider";
	// the integer fields must be >= 0).
	PipelineTaskDetection      string `json:"pipeline_task_detection,omitempty"`
	PipelineMaxReflectionLoops int    `json:"pipeline_max_reflection_loops,omitempty"`
	PipelineSkipTokensUnder    int    `json:"pipeline_skip_for_tokens_under,omitempty"`
	PipelineFallback           string `json:"pipeline_fallback,omitempty"`
	// JevJudgeEnabled, when true, turns on the JEV strategy's LLM
	// judge (A2-4): the LLM-judge scoring call runs against the
	// configured judge model (JevJudgeModel) instead of the
	// deterministic heuristic scorer. It is OFF by default — the
	// preference order is unchanged (heuristic scorer, no extra
	// judge call). A judge failure still falls back to the
	// heuristic scorer, so the request never fails because the
	// judge did.
	JevJudgeEnabled bool `json:"jev_judge_enabled,omitempty"`
	// JevJudgeModel names the model the JEV strategy's LLM judge
	// (A2-4) sends its LLM-judge scoring request to. Empty means
	// no judge model: even with jev_judge_enabled on, the judge
	// call is skipped and the heuristic scorer is used. Configured
	// exactly like a routing-group model, resolved through the
	// engine's chain map.
	JevJudgeModel string `json:"jev_judge_model,omitempty"`
}

// ResponseValidationFileConfig is the persisted shape of the
// 200-OK body validation predicate (:523, G11 Area 2). It
// mirrors router.ResponseValidationConfig exactly (same JSON
// names) so main.go can seed the engine from it. The zero
// value is the built-in error-payload predicate, enabled.
type ResponseValidationFileConfig struct {
	// Disabled turns the whole predicate off.
	Disabled bool `json:"disabled,omitempty"`
	// ForbiddenSubstrings fails the body when any of them
	// appears in the response content.
	ForbiddenSubstrings []string `json:"forbidden_substrings,omitempty"`
	// RequiredSubstrings fails the body when any of them is
	// missing from the response content.
	RequiredSubstrings []string `json:"required_substrings,omitempty"`
	// MinContentLength fails the body when the response
	// content is shorter than this. 0 disables the check.
	MinContentLength int `json:"min_content_length,omitempty"`
}

// RoutingWeights is the persisted custom weight set for the
// "custom-weights" strategy. Each field is a component weight in [0, 1]
// (ratios, not percentages); the set is normalised to sum to 1
// when used. An all-zero set means "no custom weights" and the
// custom-weights strategy behaves as balanced.
type RoutingWeights struct {
	Latency float64 `json:"latency,omitempty"`
	Success float64 `json:"success,omitempty"`
	Cost    float64 `json:"cost,omitempty"`
	Quality float64 `json:"quality,omitempty"`
	Quota   float64 `json:"quota,omitempty"`
}

// knownConfigFields is the set of top-level JSON keys owned by Config v1.
// Anything else in the file is an unknown field and goes to Extensions.
var knownConfigFields = map[string]bool{
	"version":           true,
	"listen":            true,
	"data_dir":          true,
	"gateway_keys":      true,
	"providers":         true,
	"budget_usd":        true,
	"fail_closed":       true,
	"telemetry_enabled": true,
	"auto_router":       true,
	"reasoning":         true,
	"guardrails":        true,
	"responses":         true,
	"plugins":           true,
	"routing":           true,
	"prices":            true,
	"provider_prices":   true,
	"price_file":        true,
	"mcp":               true,
	"pools":             true,
	"key_budgets":       true,
}

// configAlias is Config without its MarshalJSON/UnmarshalJSON methods, so
// the custom methods can delegate the known fields to encoding/json
// without recursing.
type configAlias Config

// MarshalJSON encodes the known v1 fields exactly as before, then
// re-emits every Extensions entry as a top-level key with its raw JSON
// bytes verbatim. Extension keys are sorted so output is deterministic;
// a key that collides with a known field is dropped (known fields win).
// A nil *Config encodes as JSON null.
func (c *Config) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	known, err := json.Marshal((*configAlias)(c))
	if err != nil {
		return nil, err
	}
	if len(c.Extensions) == 0 {
		return known, nil
	}
	keys := make([]string, 0, len(c.Extensions))
	for k, v := range c.Extensions {
		if knownConfigFields[k] || v == nil {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return known, nil
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	// known is a JSON object; splice the extension entries in before
	// the closing brace so each raw value is emitted byte-for-byte.
	buf.Write(known[:len(known)-1])
	for _, k := range keys {
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(c.Extensions[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes the known v1 fields as before and stores every
// other top-level field, raw bytes included, in Extensions. Unknown
// fields are never interpreted or validated, only preserved. When the
// input has no unknown fields, Extensions is left nil.
func (c *Config) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var alias configAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*c = Config(alias)
	c.Extensions = nil
	for k, v := range raw {
		if knownConfigFields[k] {
			continue
		}
		if c.Extensions == nil {
			c.Extensions = make(map[string]json.RawMessage, len(raw))
		}
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		c.Extensions[k] = cp
	}
	return nil
}

// ProviderConfig is the persisted shape of one upstream provider entry in
// the config file. It is an alias for provider.ProviderConfig — the single
// source of truth defined in internal/provider/provider.go — so the config
// file and the provider registry can never drift apart (same field names
// and JSON tags, same Models element type).
//
// The API key is never stored in plaintext: APIKeyEncrypted holds the
// sealed value produced by internal/crypto (Vault.Seal). The provider
// package unseals it at startup; see internal/crypto/README.md for the
// exact at-rest encryption method.
type ProviderConfig = provider.ProviderConfig

// DefaultDataDir returns the default data directory
// (~/.local/share/codvian-router).
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "codvian-router"), nil
}

// DefaultConfigPath returns the default config file path
// (~/.config/codvian-router/config.json).
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate user config directory: %w", err)
	}
	return filepath.Join(dir, "codvian-router", "config.json"), nil
}

// DefaultDeadlineBudgetMs is the per-combo time budget (RT-1,
// GAP-TRACK) a fresh config carries in routing.deadline_budget_ms,
// in milliseconds: 300000 (5 minutes). Before this default a fresh
// config had no budget at all (nil Routing → 0 → withDeadline
// returns the caller's context unchanged), so one request could
// run 5 attempts × the 120 s per-target timeout = 600 s. 300 s
// caps a request at 5 minutes while still allowing slow legitimate
// calls; the operator can raise/lower it, and an explicit 0 in a
// config file still disables the budget (see DeadlineBudgetMs).
const DefaultDeadlineBudgetMs = 300000

// DefaultConfig returns a fresh version-1 config with safe defaults:
// localhost-only listen, auth-relevant fail-closed mode on, telemetry off,
// no global budget cap (0), a 300 s per-combo deadline budget
// (DefaultDeadlineBudgetMs), and no providers or gateway keys yet.
func DefaultConfig(dataDir string) *Config {
	if dataDir == "" {
		if d, err := DefaultDataDir(); err == nil {
			dataDir = d
		}
	}
	return &Config{
		Version:          CurrentVersion,
		Listen:           DefaultListen,
		DataDir:          dataDir,
		GatewayKeys:      []GatewayKey{},
		Providers:        []ProviderConfig{},
		BudgetUSD:        0,
		FailClosed:       true,
		TelemetryEnabled: false,
		A2AEnabled:       false,
		routingMu:        &sync.RWMutex{},
		pricesMu:         &sync.RWMutex{},
		AutoRouter: AutoRouterConfig{
			Threshold:   autorouter.DefaultThreshold,
			DefaultTier: string(autorouter.DefaultTier),
		},
		Routing: &RoutingConfig{
			DeadlineBudgetMs: DefaultDeadlineBudgetMs,
		},
	}
}

// Validate checks the config for values this build can safely run with.
// It enforces: known version, parseable loopback-only listen address,
// non-negative budget, and a non-empty data directory.
// validResetTime reports whether s is an HH:MM clock time (00:00–
// 23:59, two digits each side). Shared by the token-limit
// validation; the key-budget validation predates it and inlines
// the same check.
func validResetTime(s string) bool {
	parts := strings.Split(s, ":")
	if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

// Validate reads Routing.Groups and Routing.ChainStrategies
// (M8.14 race fix) and ProviderPrices (round 3): call it
// single-threaded (Load) or under the routing read lock AND
// the prices read lock — Save takes both itself. Never call
// it while holding the routing or prices write lock.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil config")
	}
	if c.Version != CurrentVersion {
		return fmt.Errorf("config: unsupported version %d (want %d)", c.Version, CurrentVersion)
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("config: invalid listen address %q: %w", c.Listen, err)
	}
	if port == "" {
		return fmt.Errorf("config: listen address %q has no port", c.Listen)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: listen host %q must be a loopback address (127.0.0.1 or ::1); the router is local-only", host)
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if c.BudgetUSD < 0 {
		return fmt.Errorf("config: budget_usd must not be negative (got %v)", c.BudgetUSD)
	}
	// Responses session cap (CHECKLIST 263–264): negative is a
	// misconfiguration, not a clamp — 0 already means
	// counting-only, so a negative value can only be a typo.
	if c.Responses != nil && c.Responses.SessionTokenCap < 0 {
		return fmt.Errorf("config: responses.session_token_cap must not be negative (got %d)", c.Responses.SessionTokenCap)
	}
	// A2A retention (M9.8 follow-up, design §4): negative is a
	// misconfiguration, not a clamp — 0 already means the
	// default (7 days / 1,000 tasks), so a negative value can
	// only be a typo.
	if c.A2A != nil && c.A2A.RetentionDays < 0 {
		return fmt.Errorf("config: a2a.retention_days must not be negative (got %d)", c.A2A.RetentionDays)
	}
	if c.A2A != nil && c.A2A.MaxTasksPerKey < 0 {
		return fmt.Errorf("config: a2a.max_tasks_per_key must not be negative (got %d)", c.A2A.MaxTasksPerKey)
	}
	// Semantic threshold (M6.7): 0 means unset (default 0.85);
	// a configured value must be within [0.85, 1.0]. Rejected
	// at load with the field named, like auto_router.threshold
	// — never silently clamped, so a typo cannot quietly widen
	// (or close) the semantic layer.
	if c.Cache != nil && c.Cache.SemanticThreshold != 0 && (c.Cache.SemanticThreshold < 0.85 || c.Cache.SemanticThreshold > 1.0) {
		return fmt.Errorf("config: cache.semantic_threshold must be in [0.85, 1.0] (0 = default 0.85, got %v)", c.Cache.SemanticThreshold)
	}
	// Redis tier (B2-32): the address must be host:port when
	// set, and the database index must not be negative. Both
	// are load-time errors, never silently clamped or ignored
	// — a bad address must fail loudly instead of starting a
	// router that silently serves memory-only.
	if c.Cache != nil && c.Cache.RedisAddr != "" {
		if _, _, err := net.SplitHostPort(c.Cache.RedisAddr); err != nil {
			return fmt.Errorf("config: cache.redis_addr must be host:port (got %q)", c.Cache.RedisAddr)
		}
	}
	if c.Cache != nil && c.Cache.RedisDB < 0 {
		return fmt.Errorf("config: cache.redis_db must not be negative (got %d)", c.Cache.RedisDB)
	}
	if c.AutoRouter.Threshold < 0 || c.AutoRouter.Threshold > 1 {
		return fmt.Errorf("config: auto_router.threshold must be in [0, 1] (0 = default 0.5, got %v)", c.AutoRouter.Threshold)
	}
	for name, th := range c.AutoRouter.Thresholds {
		if th <= 0 || th > 1 {
			return fmt.Errorf("config: auto_router.thresholds[%q] must be in (0, 1] (got %v)", name, th)
		}
		if !autorouter.IsVirtualModel(name) {
			return fmt.Errorf("config: auto_router.thresholds[%q] is not a known virtual model (auto, auto/fast, auto/cheap, auto/coding, auto/code, auto/reasoning)", name)
		}
	}
	if c.AutoRouter.DefaultTier != "" && !autorouter.IsTier(c.AutoRouter.DefaultTier) {
		return fmt.Errorf("config: auto_router.default_tier %q is not a known tier (coding, chat, reasoning, quick-qa)", c.AutoRouter.DefaultTier)
	}
	// Reasoning effort→budget table (CHECKLIST 285-286): a bad
	// entry fails at load with the field named, never at request
	// time with a silently dropped thinking field.
	if c.Reasoning != nil {
		if err := c.Reasoning.EffortBudgets.Validate(); err != nil {
			return err
		}
		// Forced-reasoning policy (CHECKLIST 311-312): a bad
		// rule fails at load with the field named, never at
		// request time with a silently unforced effort.
		if err := c.Reasoning.ValidateForcedEffort(); err != nil {
			return err
		}
	}
	// Routing strategies: names must be known (or empty = inherit /
	// balanced), the deadline budget must not be negative, and the
	// custom weighted weights must each be in [0, 1] with a
	// positive total when any is set. Strategy-name validation
	// lives here, not in internal/strategy, so a bad config file
	// fails at load with the field named instead of silently
	// ranking as balanced at request time. A nil Routing block
	// means defaults and is always valid.
	if rc := c.Routing; rc != nil {
		if rc.GlobalStrategy != "" && !isStrategyName(rc.GlobalStrategy) {
			return fmt.Errorf("config: routing.global_strategy %q is not a known strategy (priority, balanced, cheapest, fastest, most-reliable, least-latency, weighted, custom-weights, code, round-robin, random, strict-random, fill-first, least-used, p2c, quota-weighted, headroom, expiry-first, reset-aware, reset-window, lkgp, context-optimized, cache-optimized, context-relay, cost-optimized, ship-fast, cost-saver, quality-first, offline-friendly, reliability-first, chaos-mode, cascade)", rc.GlobalStrategy)
		}
		// Cooldown modes: "" (default = hard), "hard" or
		// "soft-demote". The engine itself resolves unknown
		// names to hard, so a bad value must fail here, at
		// load, with the field named — never silently.
		if !isCooldownMode(rc.CooldownMode) {
			return fmt.Errorf("config: routing.cooldown_mode %q is not a known cooldown mode (hard, soft-demote)", rc.CooldownMode)
		}
		// Capability requirements (P1.5): each name must be one the
		// capability registry can actually record, otherwise the
		// filter would silently skip every attempt forever.
		for _, name := range rc.RequireCapabilities {
			c, ok := capability.ParseCapability(name)
			if !ok {
				return fmt.Errorf("config: routing.require_capabilities %q is not a known capability (known: %s)", name, strings.Join(capability.KnownCapabilities(), ", "))
			}
			// P2-PROV-002: reject a capability nothing can ever record,
			// instead of letting the filter silently skip every attempt.
			if reason, usable := capability.Configurable(c); !usable {
				return fmt.Errorf("config: routing.require_capabilities %q cannot be satisfied: %s", name, reason)
			}
		}
		for model, m := range rc.ChainCooldownModes {
			if model == "" {
				return errors.New("config: routing.chain_cooldown_modes has an empty model key")
			}
			if !isCooldownMode(m) || m == "" {
				return fmt.Errorf("config: routing.chain_cooldown_modes[%q] = %q is not a known cooldown mode (hard, soft-demote)", model, m)
			}
		}
		// Account-level fallback strategies (M3 Area 1,
		// PARITY row 388): unknown names fail at load with
		// the field and value named, never silently
		// degrading to fill-first at request time.
		// "expiry-first" is rejected here too — no per-key
		// quota-expiry source exists (design §D2), so it
		// can never order accounts honestly.
		if !isAccountFallbackStrategy(rc.AccountFallbackStrategy) {
			return fmt.Errorf("config: routing.account_fallback_strategy %q is not a known account fallback strategy (priority, fill-first, round-robin, p2c, random, least-used, cost-optimized, strict-random, weighted)", rc.AccountFallbackStrategy)
		}
		for provider, s := range rc.AccountFallbackStrategies {
			if provider == "" {
				return errors.New("config: routing.account_fallback_strategies has an empty provider key")
			}
			if !isAccountFallbackStrategy(s) {
				return fmt.Errorf("config: routing.account_fallback_strategies[%q] = %q is not a known account fallback strategy (priority, fill-first, round-robin, p2c, random, least-used, cost-optimized, strict-random, weighted)", provider, s)
			}
		}
		// SLA objectives (M3 Area 1, PARITY row 394): every
		// field is a pointer, so nil means "default" and only
		// a set value is validated. An explicit 0 error rate
		// is valid — it is zero-tolerance, not "unset" — so
		// the lower bound is inclusive; a 0 target is not a
		// real objective (everything would violate) and is
		// rejected, as are negative / non-finite values and
		// an error rate outside [0, 1]. A 0 cost cap means
		// "no cap" (the scorer's convention), so it is valid.
		// Fusion knobs (A2-3b): negative values are a config
		// error, never a silent "use the default"; the panel
		// size must also be coherent (min <= max).
		if rc.FusionMinPanel < 0 {
			return fmt.Errorf("config: routing.fusion_min_panel must not be negative (got %d)", rc.FusionMinPanel)
		}
		if rc.FusionMaxPanel < 0 {
			return fmt.Errorf("config: routing.fusion_max_panel must not be negative (got %d)", rc.FusionMaxPanel)
		}
		if rc.FusionMinPanel > 0 && rc.FusionMaxPanel > 0 && rc.FusionMinPanel > rc.FusionMaxPanel {
			return fmt.Errorf("config: routing.fusion_min_panel (%d) must not exceed routing.fusion_max_panel (%d)", rc.FusionMinPanel, rc.FusionMaxPanel)
		}
		if rc.FusionStragglerGraceMs < 0 {
			return fmt.Errorf("config: routing.fusion_straggler_grace_ms must not be negative (got %d)", rc.FusionStragglerGraceMs)
		}
		if rc.FusionPanelHardTimeoutMs < 0 {
			return fmt.Errorf("config: routing.fusion_panel_hard_timeout_ms must not be negative (got %d)", rc.FusionPanelHardTimeoutMs)
		}
		if sla := rc.SLA; sla != nil {
			if v := sla.TargetP95Ms; v != nil {
				if math.IsNaN(*v) || math.IsInf(*v, 0) || *v <= 0 {
					return fmt.Errorf("config: routing.sla.target_p95_ms must be > 0 (got %v)", *v)
				}
			}
			if v := sla.MaxErrorRate; v != nil {
				if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 1 {
					return fmt.Errorf("config: routing.sla.max_error_rate must be between 0 and 1 (got %v)", *v)
				}
			}
			if v := sla.MaxCostPer1M; v != nil {
				if math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 {
					return fmt.Errorf("config: routing.sla.max_cost_per_1m_tokens must not be negative (got %v)", *v)
				}
			}
		}
		// Nadir decision endpoint (M3 Area 1, PARITY row
		// 396): an absent block means unconfigured, and an
		// empty endpoint inside a present block means the
		// same — both are allowed. A non-empty endpoint must
		// parse as an absolute http(s) URL after the same
		// normalisation the engine applies (trim, strip a
		// trailing "/v1"); anything else is an operator
		// error at load, naming the field and value. The
		// timeout is 0 = default, and is rejected when
		// negative or above the 30000 ms engine clamp. The
		// cooldown is 0 = default (30000 ms) and is rejected
		// when negative; it has no upper cap.
		if nadir := rc.Nadir; nadir != nil {
			if nadir.Endpoint != "" && !validNadirEndpoint(nadir.Endpoint) {
				return fmt.Errorf("config: routing.nadir.endpoint %q is not an absolute http(s) URL", nadir.Endpoint)
			}
			if nadir.TimeoutMs < 0 || nadir.TimeoutMs > 30000 {
				return fmt.Errorf("config: routing.nadir.timeout_ms must be between 0 and 30000 (got %v)", nadir.TimeoutMs)
			}
			if nadir.CooldownMs < 0 {
				return fmt.Errorf("config: routing.nadir.cooldown_ms must not be negative (got %v)", nadir.CooldownMs)
			}
		}
		if rc.DeadlineBudgetMs < 0 {
			return fmt.Errorf("config: routing.deadline_budget_ms must not be negative (got %v)", rc.DeadlineBudgetMs)
		}
		// M4.9 failover knobs: max_attempts is clamped [1, 200]
		// by the engine, but a config value outside that range
		// is an operator error, not a value to silently fix —
		// it fails at load with the field named. The timeouts
		// are milliseconds, 0 = engine default; per-target
		// timeout also accepts -1 (disabled).
		if rc.MaxAttempts < 0 || rc.MaxAttempts > 200 {
			return fmt.Errorf("config: routing.max_attempts must be between 1 and 200 (got %v)", rc.MaxAttempts)
		}
		// Stage 1 wiring sweep: the failover/hedge/relay
		// knobs. 0 means the engine default everywhere
		// except where a negative value is a deliberate
		// opt-out (deadline sub-budget off, hedging off).
		if rc.MaxProvidersPerRequest < 0 {
			return fmt.Errorf("config: routing.max_providers_per_request must not be negative (got %v)", rc.MaxProvidersPerRequest)
		}
		if rc.MaxRetries < 0 {
			return fmt.Errorf("config: routing.max_retries must not be negative (got %v)", rc.MaxRetries)
		}
		if rc.RetryDelayMs < 0 {
			return fmt.Errorf("config: routing.retry_delay_ms must not be negative (got %v)", rc.RetryDelayMs)
		}
		if rc.MaxSetRetries < 0 {
			return fmt.Errorf("config: routing.max_set_retries must not be negative (got %v)", rc.MaxSetRetries)
		}
		// Context-relay thresholds/TTL (B2-01): each side
		// is a fraction in [0, 1), 0 meaning the engine
		// default — so the window ordering check runs on
		// the effective values, not the raw ones (a lone
		// exhaustion of 0.8 with an unset threshold would
		// otherwise silently invert the [0.85, 0.8) window
		// and never trigger). The 0.85 / 0.95 literals are
		// the engine defaults and must match
		// defaultRelayHandoffThreshold /
		// defaultRelayExhaustionThreshold in
		// internal/router/relay.go.
		if rc.RelayHandoffThreshold < 0 || rc.RelayHandoffThreshold >= 1 {
			return fmt.Errorf("config: routing.relay_handoff_threshold must be between 0 and 1 (got %v)", rc.RelayHandoffThreshold)
		}
		if rc.RelayExhaustionThreshold < 0 || rc.RelayExhaustionThreshold > 1 {
			return fmt.Errorf("config: routing.relay_exhaustion_threshold must be between 0 and 1 (got %v)", rc.RelayExhaustionThreshold)
		}
		effRelayHandoff := rc.RelayHandoffThreshold
		if effRelayHandoff == 0 {
			effRelayHandoff = 0.85
		}
		effRelayExhaustion := rc.RelayExhaustionThreshold
		if effRelayExhaustion == 0 {
			effRelayExhaustion = 0.95
		}
		if effRelayExhaustion <= effRelayHandoff {
			return fmt.Errorf("config: routing.relay_exhaustion_threshold (%v) must be above routing.relay_handoff_threshold (%v)", effRelayExhaustion, effRelayHandoff)
		}
		if rc.RelayTTLMs < 0 {
			return fmt.Errorf("config: routing.relay_ttl_ms must not be negative (got %v)", rc.RelayTTLMs)
		}
		if rc.RelayMaxMessages < 0 {
			return fmt.Errorf("config: routing.relay_max_messages must not be negative (got %v)", rc.RelayMaxMessages)
		}
		if rv := rc.ResponseValidation; rv != nil {
			if rv.MinContentLength < 0 {
				return fmt.Errorf("config: routing.response_validation.min_content_length must not be negative (got %v)", rv.MinContentLength)
			}
		}
		for provider, models := range rc.QuotaShares {
			if provider == "" {
				return fmt.Errorf("config: routing.quota_shares has an empty provider key")
			}
			for model, share := range models {
				if model == "" {
					return fmt.Errorf("config: routing.quota_shares[%q] has an empty model key", provider)
				}
				if share <= 0 {
					return fmt.Errorf("config: routing.quota_shares[%q][%q] must be positive (got %v)", provider, model, share)
				}
			}
		}
		// A2-7 pipeline knobs: validated so a typo fails at load, never
		// routes at a silently ignored setting. pipeline_task_detection is
		// "" (off) or "auto"; pipeline_fallback is "" (fail) or
		// "single-provider"; the integer fields must not be negative.
		switch rc.PipelineTaskDetection {
		case "", "auto":
		default:
			return fmt.Errorf("config: routing.pipeline_task_detection %q is not a known mode (want \"\" or \"auto\")", rc.PipelineTaskDetection)
		}
		switch rc.PipelineFallback {
		case "", "single-provider":
		default:
			return fmt.Errorf("config: routing.pipeline_fallback %q is not a known value (want \"\" or \"single-provider\")", rc.PipelineFallback)
		}
		if rc.PipelineMaxReflectionLoops < 0 {
			return fmt.Errorf("config: routing.pipeline_max_reflection_loops must not be negative (got %v)", rc.PipelineMaxReflectionLoops)
		}
		if rc.PipelineSkipTokensUnder < 0 {
			return fmt.Errorf("config: routing.pipeline_skip_for_tokens_under must not be negative (got %v)", rc.PipelineSkipTokensUnder)
		}
		if rc.PerTargetTimeoutMs < -1 {
			return fmt.Errorf("config: routing.per_target_timeout_ms must be -1 (disabled) or >= 0 (got %v)", rc.PerTargetTimeoutMs)
		}
		// M8.20 F3: per-endpoint media timeouts. Keys are the
		// media endpoint names; 0 means the per-endpoint
		// default (see MediaTimeoutsMs), so only negative and
		// absurdly large values are errors — plus unknown keys,
		// which would otherwise silently do nothing.
		for endpoint, ms := range rc.MediaTimeoutsMs {
			switch endpoint {
			case "search", "ocr", "video", "music":
			default:
				return fmt.Errorf("config: routing.media_timeouts_ms has unknown endpoint %q (want one of search, ocr, video, music)", endpoint)
			}
			if ms < 0 {
				return fmt.Errorf("config: routing.media_timeouts_ms[%q] must not be negative (got %v)", endpoint, ms)
			}
			if ms > maxMediaTimeoutMs {
				return fmt.Errorf("config: routing.media_timeouts_ms[%q] must be <= %v (1h, got %v)", endpoint, maxMediaTimeoutMs, ms)
			}
		}
		if rc.PredictiveTtftMs < 0 {
			return fmt.Errorf("config: routing.predictive_ttft_ms must not be negative (got %v)", rc.PredictiveTtftMs)
		}
		if rc.PreferredFailoverAfterMs < 0 {
			return fmt.Errorf("config: routing.preferred_failover_after_ms must not be negative (got %v)", rc.PreferredFailoverAfterMs)
		}
		if ml := rc.ModelLockout; ml != nil {
			if ml.BaseCooldownMs < 0 {
				return fmt.Errorf("config: routing.model_lockout.base_cooldown_ms must not be negative (got %v)", ml.BaseCooldownMs)
			}
			if ml.MaxCooldownMs < 0 {
				return fmt.Errorf("config: routing.model_lockout.max_cooldown_ms must not be negative (got %v)", ml.MaxCooldownMs)
			}
			if ml.MaxSteps < 0 {
				return fmt.Errorf("config: routing.model_lockout.max_steps must not be negative (got %v)", ml.MaxSteps)
			}
			for _, code := range ml.ErrorCodes {
				if code < 100 || code > 599 {
					return fmt.Errorf("config: routing.model_lockout.error_codes has non-HTTP status %v", code)
				}
			}
		}
		if wb := rc.WaitBudgets; wb != nil {
			if wb.MaxWaitPerRetryMs < 0 {
				return fmt.Errorf("config: routing.wait_budgets.max_wait_per_retry_ms must not be negative (got %v)", wb.MaxWaitPerRetryMs)
			}
			if wb.MaxRetries < 0 {
				return fmt.Errorf("config: routing.wait_budgets.max_retries must not be negative (got %v)", wb.MaxRetries)
			}
			if wb.TotalBudgetMs < 0 {
				return fmt.Errorf("config: routing.wait_budgets.total_budget_ms must not be negative (got %v)", wb.TotalBudgetMs)
			}
		}
		// Context-window fallback chain (CHECKLIST 159): the
		// reserve must not be negative (0 = engine default
		// 4096) and every fallback model must be a non-empty
		// ID. An entry no provider offers is not an error here
		// — it simply contributes nothing to the chain.
		if rc.ContextReserveTokens < 0 {
			return fmt.Errorf("config: routing.context_reserve_tokens must not be negative (got %v)", rc.ContextReserveTokens)
		}
		for i, m := range rc.ContextFallbackModels {
			if m == "" {
				return fmt.Errorf("config: routing.context_fallback_models[%d] is empty", i)
			}
		}
		// Content-policy refusal chains (CHECKLIST 161): a
		// model key must be non-empty and every provider ID
		// non-empty with no duplicates in one chain (a
		// duplicate would re-ask a target whose verdict is
		// already given). An entry with no live client is not
		// an error here — it is skipped at request time. One
		// exception: an entry starting with "group:" names a
		// routing group (see Groups below) and expands to that
		// group's members — and that name IS validated here
		// (an unknown group is a startup error naming it,
		// because groups are config-internal, unlike provider
		// IDs that may arrive later via a live provider add).
		for model, ids := range rc.RefusalChains {
			if model == "" {
				return errors.New("config: routing.refusal_chains has an empty model key")
			}
			seen := make(map[string]bool, len(ids))
			for i, id := range ids {
				if id == "" {
					return fmt.Errorf("config: routing.refusal_chains[%q][%d] is empty", model, i)
				}
				if seen[id] {
					return fmt.Errorf("config: routing.refusal_chains[%q] lists provider %q twice", model, id)
				}
				seen[id] = true
				if strings.HasPrefix(id, "group:") {
					name := strings.TrimSpace(id[len("group:"):])
					if name == "" {
						return fmt.Errorf("config: routing.refusal_chains[%q][%d] names a group but the group name is empty", model, i)
					}
					if _, ok := rc.Groups[name]; !ok {
						return fmt.Errorf("config: routing.refusal_chains[%q] names unknown routing group %q", model, name)
					}
				}
			}
		}
		// Model equivalents (CHECKLIST 257): a model key must
		// be non-empty, every equivalent must be a non-empty
		// model ID, a model must never list itself, and an
		// entry must not repeat a model (a duplicate is a
		// typo, not a second preference). An equivalent no
		// provider offers is not an error — it simply
		// contributes nothing to the chain.
		for model, equivs := range rc.ModelEquivalents {
			if model == "" {
				return errors.New("config: routing.model_equivalents has an empty model key")
			}
			seen := make(map[string]bool, len(equivs))
			for i, eq := range equivs {
				if eq == "" {
					return fmt.Errorf("config: routing.model_equivalents[%q][%d] is empty", model, i)
				}
				if eq == model {
					return fmt.Errorf("config: routing.model_equivalents[%q] lists itself as its own equivalent", model)
				}
				if seen[eq] {
					return fmt.Errorf("config: routing.model_equivalents[%q] lists model %q twice", model, eq)
				}
				seen[eq] = true
			}
		}
		// User-named routing groups (CHECKLIST 173): a group
		// name must be non-empty and must not contain ":" or
		// "/" (both are alias syntax — the "group:<name>/<model>"
		// model alias splits on them); a group must list at
		// least one provider; every member must be a provider
		// ID from this config file, with no duplicates in one
		// group. An unknown member fails at load naming it —
		// a group that names a provider that does not exist
		// must never silently route to a smaller pool.
		// (CHECKLIST 255): a member may carry a reasoning
		// effort — "provider@low" — so the duplicate rule is on
		// the (provider, effort) pair, not the provider alone:
		// the same provider twice at different efforts is two
		// distinct entries, while the same pair twice is still
		// an error. The effort is validated here (see
		// ParseGroupMember): an unknown level fails at load
		// naming it.
		providerIDs := make(map[string]bool, len(c.Providers))
		for _, p := range c.Providers {
			providerIDs[p.ID] = true
		}
		for name, members := range rc.Groups {
			if name == "" {
				return errors.New("config: routing.groups has an empty group name")
			}
			if strings.ContainsAny(name, ":/") {
				return fmt.Errorf("config: routing.groups[%q]: group names must not contain \":\" or \"/\" (alias syntax)", name)
			}
			if len(members) == 0 {
				return fmt.Errorf("config: routing.groups[%q] is empty: a group needs at least one provider", name)
			}
			seen := make(map[string]bool, len(members))
			for i, id := range members {
				if id == "" {
					return fmt.Errorf("config: routing.groups[%q][%d] is empty", name, i)
				}
				// A member starting with "group:" names another
				// routing group (a nested combo-ref): it expands to
				// that group's members recursively at request time.
				// The referenced name is validated here (unknown
				// group = startup error) and cycles are rejected by
				// validateGroupGraph after the loop.
				if strings.HasPrefix(id, "group:") {
					inner := strings.TrimSpace(id[len("group:"):])
					if inner == "" {
						return fmt.Errorf("config: routing.groups[%q][%d] names a group but the group name is empty", name, i)
					}
					if _, ok := rc.Groups[inner]; !ok {
						return fmt.Errorf("config: routing.groups[%q][%d] names unknown routing group %q", name, i, inner)
					}
					if inner == name {
						return fmt.Errorf("config: routing.groups[%q][%d] references itself (cycle)", name, i)
					}
					continue
				}
				providerID, effort, err := ParseGroupMember(id)
				if err != nil {
					return fmt.Errorf("config: routing.groups[%q][%d]: %w", name, i, err)
				}
				key := providerID + "\x00" + effort
				if seen[key] {
					if effort == "" {
						return fmt.Errorf("config: routing.groups[%q] lists provider %q twice", name, providerID)
					}
					return fmt.Errorf("config: routing.groups[%q] lists provider %q at effort %q twice", name, providerID, effort)
				}
				seen[key] = true
				if !providerIDs[providerID] {
					return fmt.Errorf("config: routing.groups[%q] lists unknown provider %q", name, providerID)
				}
			}
		}
		// Nested group refs form a directed graph (group -> inner
		// groups); a cycle would infinite-loop at request time, so
		// it is rejected here at load, naming the cycle path.
		if err := validateGroupGraph(rc.Groups); err != nil {
			return err
		}
		for model, s := range rc.ChainStrategies {
			if model == "" {
				return errors.New("config: routing.chain_strategies has an empty model key")
			}
			if !isStrategyName(s) {
				return fmt.Errorf("config: routing.chain_strategies[%q] = %q is not a known strategy (priority, balanced, cheapest, fastest, most-reliable, least-latency, weighted, custom-weights, code, round-robin, random, strict-random, fill-first, least-used, p2c, quota-weighted, headroom, expiry-first, reset-aware, reset-window, lkgp, context-optimized, cache-optimized, context-relay, cost-optimized, ship-fast, cost-saver, quality-first, offline-friendly, reliability-first, chaos-mode, cascade, fusion)", model, s)
			}
		}
		// Nestable strategy trees (CHECKLIST 177): each tree is
		// validated whole — unknown strategy, leaf without a
		// provider, a node that is both picker and leaf, a cycle
		// or shared node, or nesting past the depth cap all fail
		// at load with the node path named, never at request
		// time and never as a silent flatten.
		if rc.StrategyTree != nil {
			if err := strategytree.Validate(rc.StrategyTree); err != nil {
				return fmt.Errorf("config: routing.strategy_tree: %w", err)
			}
		}
		for model, tree := range rc.StrategyTrees {
			if model == "" {
				return errors.New("config: routing.strategy_trees has an empty model key")
			}
			if tree == nil {
				return fmt.Errorf("config: routing.strategy_trees[%q] is null (omit the entry instead)", model)
			}
			if err := strategytree.Validate(tree); err != nil {
				return fmt.Errorf("config: routing.strategy_trees[%q]: %w", model, err)
			}
		}
		if w := rc.WeightedWeights; w != nil {
			for name, v := range map[string]float64{"latency": w.Latency, "success": w.Success, "cost": w.Cost, "quality": w.Quality, "quota": w.Quota} {
				if v < 0 || v > 1 {
					return fmt.Errorf("config: routing.weighted_weights.%s must be in [0, 1] (got %v)", name, v)
				}
			}
			if total := w.Latency + w.Success + w.Cost + w.Quality + w.Quota; total > 0 && total < 1e-9 {
				return fmt.Errorf("config: routing.weighted_weights total (%v) is too small to normalise; use 0 for all (balanced) or weights that sum to a usable total", total)
			}
		}
		// Chain per-target weights (M3 Phase C): each weight
		// must be finite and >= 0 (negative, NaN or Inf is a
		// load error naming the field), and a chain with
		// all-zero weights is rejected — a draw over all-zero
		// weights is meaningless (omit the chain, or give a
		// target a positive weight; a single zero among
		// positives is fine — it excludes that target from
		// the draw). Unknown chain keys are allowed: chains
		// are operator-defined. A missing map, a missing
		// chain entry, or an empty inner map all mean weight
		// 1 for every target. Same rules as
		// strategy.ValidateWeights, spelled out here (with
		// config: field names) because config must not import
		// internal/strategy.
		for chain, targets := range rc.ChainWeights {
			if chain == "" {
				return errors.New("config: routing.chain_weights has an empty chain key")
			}
			allZero := true
			for target, w := range targets {
				if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
					return fmt.Errorf("config: routing.chain_weights[%q][%q] must be finite and >= 0 (got %v)", chain, target, w)
				}
				if w > 0 {
					allZero = false
				}
			}
			if len(targets) > 0 && allZero {
				return fmt.Errorf("config: routing.chain_weights[%q] has all-zero weights: a draw over all-zero weights is meaningless (omit the chain or give a target a positive weight)", chain)
			}
		}
	}
	// External guardrails (CHECKLIST 185) and per-type toggles
	// (G18 Area 7): validated whole by the guardrails package —
	// an async entry with a blocking kind (pii/secret/injection)
	// fails at load, naming the guardrail, instead of letting a
	// secret reach a provider while a verdict is in flight; an
	// unknown disabled type fails at load, naming the label. A
	// nil Guardrails block means defaults and is always valid.
	// Custom patterns (B3-18) and the response-guard mode
	// (B3-21/B3-22) are validated here too: a bad regex, an
	// unknown kind, a duplicate id or an unknown mode fails at
	// load, naming the field, instead of failing at startup or
	// silently doing nothing.
	if c.Guardrails != nil && (len(c.Guardrails.External) > 0 || len(c.Guardrails.DisabledTypes) > 0 || len(c.Guardrails.CustomPatterns) > 0 || c.Guardrails.ResponseGuardMode != "") {
		gc := guardrails.Config{}
		gc.DisabledTypes = append([]string(nil), c.Guardrails.DisabledTypes...)
		for _, e := range c.Guardrails.External {
			gc.External = append(gc.External, guardrails.ExternalConfig{
				Name: e.Name, Kind: e.Kind, Mode: guardrails.Mode(e.Mode),
				Endpoint: e.Endpoint, TimeoutMs: e.TimeoutMs,
			})
		}
		if err := gc.Validate(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		switch c.Guardrails.ResponseGuardMode {
		case "", "redact", "warn", "block":
		default:
			return fmt.Errorf("config: guardrails.response_guard_mode must be \"redact\", \"warn\" or \"block\" (got %q)", c.Guardrails.ResponseGuardMode)
		}
		seenIDs := map[string]bool{}
		for i, p := range c.Guardrails.CustomPatterns {
			if p.ID == "" {
				return fmt.Errorf("config: guardrails.custom_patterns[%d].id must not be empty", i)
			}
			if seenIDs[p.ID] {
				return fmt.Errorf("config: guardrails.custom_patterns[%d].id %q is duplicated", i, p.ID)
			}
			seenIDs[p.ID] = true
			if p.Label == "" {
				return fmt.Errorf("config: guardrails.custom_patterns[%d].label must not be empty", i)
			}
			switch p.Kind {
			case "", "pii", "injection":
			default:
				return fmt.Errorf("config: guardrails.custom_patterns[%d].kind must be \"pii\" or \"injection\" (got %q)", i, p.Kind)
			}
			if p.Severity < 0 || p.Severity > 10 {
				return fmt.Errorf("config: guardrails.custom_patterns[%d].severity must be between 0 and 10 (got %v)", i, p.Severity)
			}
			if err := guardrails.ValidateCustomPattern(p.Pattern); err != nil {
				return fmt.Errorf("config: guardrails.custom_patterns[%d] (%q): %w", i, p.Label, err)
			}
		}
	}
	// Gateway-key guardrail names (CHECKLIST 245, LiteLLM
	// #51080): every name a key lists must be registered — a
	// built-in kind (pii, secret, injection) or a configured
	// external guardrail name. A typo'd name fails at load,
	// naming the key and the name, instead of silently leaving
	// the key believing it is guarded by a guardrail that does
	// not exist. The same check runs at key-update time, so a
	// key can never acquire an unknown name at runtime either.
	if len(c.GatewayKeys) > 0 {
		gc := guardrails.Config{}
		if c.Guardrails != nil {
			for _, e := range c.Guardrails.External {
				gc.External = append(gc.External, guardrails.ExternalConfig{Name: e.Name})
			}
		}
		for _, k := range c.GatewayKeys {
			if err := gc.ValidateGuardrailNames(k.Guardrails); err != nil {
				return fmt.Errorf("config: gateway key %q: %w", k.ID, err)
			}
		}
	}
	// Plugins (CHECKLIST 201): every named plugin must be a
	// known built-in, and the list must not repeat a name — a
	// duplicate would run the same hook twice and double-count.
	// Unknown names fail at load, naming the entry, instead of
	// silently running a shorter pipeline than the operator
	// configured.
	if c.Plugins != nil {
		seen := map[string]bool{}
		for _, name := range c.Plugins.Enabled {
			if name == "" {
				return errors.New("config: plugins.enabled has an empty entry")
			}
			if seen[name] {
				return fmt.Errorf("config: plugins.enabled repeats %q", name)
			}
			seen[name] = true
			if !plugins.IsBuiltinName(name) {
				return fmt.Errorf("config: plugins.enabled[%q] is not a known built-in plugin (known: header-add, response-log, cost-cap); plugins are compiled-in only — no plugin binary path is accepted", name)
			}
		}
		// Cost-cap cap (CHECKLIST 239): non-negative, and only
		// meaningful with the plugin enabled.
		if c.Plugins.CostCapUSD != nil {
			if *c.Plugins.CostCapUSD < 0 {
				return fmt.Errorf("config: plugins.cost_cap_usd_per_request must not be negative, got %v", *c.Plugins.CostCapUSD)
			}
			if !seen[plugins.CostCapName] {
				return fmt.Errorf("config: plugins.cost_cap_usd_per_request is set but \"cost-cap\" is not in plugins.enabled — the cap would silently do nothing")
			}
		}
	}
	for tier := range c.AutoRouter.TierModels {
		if !autorouter.IsTier(tier) {
			return fmt.Errorf("config: auto_router.tier_models[%q] is not a known tier (coding, chat, reasoning, quick-qa)", tier)
		}
	}
	for tier, m := range c.AutoRouter.IntentTiers {
		if !autorouter.IsIntentTier(tier) {
			return fmt.Errorf("config: auto_router.intent_tiers[%q] is not a known intent tier (fast, balanced, best)", tier)
		}
		if strings.TrimSpace(m.Model) == "" {
			return fmt.Errorf("config: auto_router.intent_tiers[%q] needs a model (empty model is not a mapping)", tier)
		}
		if _, err := autorouter.ParseEffort(m.Effort); err != nil {
			return fmt.Errorf("config: auto_router.intent_tiers[%q].effort: %v", tier, err)
		}
	}
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("config: providers[%d] has an empty id", i)
		}
		// A catalog-linked provider may leave base_url empty: the
		// registry resolves it from the catalog entry at load. A
		// fully-custom provider must name its own base URL.
		if p.BaseURL == "" && p.CatalogID == "" {
			return fmt.Errorf("config: provider %q has an empty base_url", p.ID)
		}
		// Region (CHECKLIST 257): same single-token rule as
		// model regions — "" (unknown) is always valid.
		if err := provider.ValidateRegion(p.Region); err != nil {
			return fmt.Errorf("config: provider %q region: %w", p.ID, err)
		}
		// Legacy per-model prices on the provider entry itself
		// (USD per token) must not be negative: a negative price
		// is never a valid operator entry, and the scorer would
		// otherwise read it as a real (impossible) cost.
		for _, m := range p.Models {
			if m.InputPriceUSD < 0 {
				return fmt.Errorf("config: provider %q model %q input_price_usd must not be negative (got %v)", p.ID, m.ID, m.InputPriceUSD)
			}
			if m.OutputPriceUSD < 0 {
				return fmt.Errorf("config: provider %q model %q output_price_usd must not be negative (got %v)", p.ID, m.ID, m.OutputPriceUSD)
			}
			// Regions (CHECKLIST 257): a declared region must
			// be a single token — "" (unknown) is always
			// valid, and the value is never verified against
			// a vendor's region list (the router must not
			// guess which regions a provider offers).
			if err := provider.ValidateRegion(m.Region); err != nil {
				return fmt.Errorf("config: provider %q model %q region: %w", p.ID, m.ID, err)
			}
			// Deployment pins (CHECKLIST 245): a pinned value
			// is applied to every request for this model, so
			// an out-of-range pin must fail at load, naming
			// the field, instead of sending a nonsense value
			// upstream on every request.
			if fp := m.FixedParams; fp != nil {
				if fp.Temperature != nil && (*fp.Temperature < 0 || *fp.Temperature > 2) {
					return fmt.Errorf("config: provider %q model %q fixed_params.temperature must be in [0, 2] (got %v)", p.ID, m.ID, *fp.Temperature)
				}
				if fp.TopP != nil && (*fp.TopP <= 0 || *fp.TopP > 1) {
					return fmt.Errorf("config: provider %q model %q fixed_params.top_p must be in (0, 1] (got %v)", p.ID, m.ID, *fp.TopP)
				}
				if fp.MaxTokens != nil && *fp.MaxTokens < 1 {
					return fmt.Errorf("config: provider %q model %q fixed_params.max_tokens must be >= 1 (got %v)", p.ID, m.ID, *fp.MaxTokens)
				}
			}
		}
	}
	// Operator-configured prices (USD per 1M tokens): every entry
	// is validated with the model named, so a bad price fails at
	// load instead of ranking as a real cost at request time. A
	// nil Prices map means "no model-level prices configured" and
	// is valid; a nil ProviderPrices map means "no pair prices
	// configured" and is valid.
	// MCP selector mode: the mode must be selector/direct (or
	// empty = selector), the loop cap must be in range, and every
	// server needs a unique non-empty name and a command — a
	// server with no command cannot be spawned, so it fails at
	// load with the field named instead of failing silently at
	// request time. A nil MCP block means no servers configured
	// and is always valid.
	if mc := c.MCP; mc != nil {
		m := strings.ToLower(strings.TrimSpace(mc.Mode))
		if m != "" && m != "selector" && m != "direct" {
			return fmt.Errorf("config: mcp.mode %q is not a known mode (selector, direct)", mc.Mode)
		}
		if mc.MaxIterations < 0 || mc.MaxIterations > 50 {
			return fmt.Errorf("config: mcp.max_iterations must be in [0, 50] (0 = default 10, got %d)", mc.MaxIterations)
		}
		// M9.6 controls (CHECKLIST 392): present means enforced,
		// so the values must be usable — a negative cap or a
		// blank pattern is a load error naming the field, never
		// silently ignored. An empty Allow list is NOT an error:
		// with controls present that is the fail-closed
		// deny-all, by design (see MCPConfig).
		if mc.Controls != nil {
			for i, p := range mc.Controls.Allow {
				if strings.TrimSpace(p) == "" {
					return fmt.Errorf("config: mcp.controls.allow[%d] is empty", i)
				}
			}
			for i, p := range mc.Controls.Deny {
				if strings.TrimSpace(p) == "" {
					return fmt.Errorf("config: mcp.controls.deny[%d] is empty", i)
				}
			}
			if mc.Controls.MaxCalls < 0 {
				return fmt.Errorf("config: mcp.controls.max_calls must be >= 0 (0 = unlimited, got %d)", mc.Controls.MaxCalls)
			}
			if mc.Controls.MaxDuration < 0 {
				return fmt.Errorf("config: mcp.controls.max_duration must be >= 0 (0 = unlimited, got %s)", mc.Controls.MaxDuration)
			}
		}
		seen := map[string]bool{}
		for i, s := range mc.Servers {
			if s.Name == "" {
				return fmt.Errorf("config: mcp.servers[%d] has an empty name", i)
			}
			if seen[s.Name] {
				return fmt.Errorf("config: mcp.servers[%d] duplicates server name %q", i, s.Name)
			}
			seen[s.Name] = true
			if s.Command == "" {
				return fmt.Errorf("config: mcp.servers[%d] (%q) has an empty command", i, s.Name)
			}
		}
	}
	// Compression (CHECKLIST 183): the per-model lossless list
	// must name real (non-empty) models, with no duplicates — a
	// duplicate entry is a typo, not a second opt-in. There is
	// no lossy setting to validate: lossy is per-call only and
	// lives in the request body, never in this file.
	if cc := c.Compression; cc != nil {
		seen := map[string]bool{}
		for i, m := range cc.LosslessModels {
			if m == "" {
				return fmt.Errorf("config: compression.lossless_models[%d] is empty", i)
			}
			if seen[m] {
				return fmt.Errorf("config: compression.lossless_models duplicates model %q", m)
			}
			seen[m] = true
		}
	}
	// Shortlist (CHECKLIST 199-200): entries must be non-empty
	// and unique — a duplicate is a typo, not a second pin.
	// Entries naming models no provider offers are NOT rejected
	// here (the shortlist is a display pin, and a model may be
	// added later); they are simply never shown.
	seenShort := map[string]bool{}
	for i, m := range c.Shortlist {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("config: shortlist[%d] is empty", i)
		}
		if seenShort[m] {
			return fmt.Errorf("config: shortlist duplicates model %q", m)
		}
		seenShort[m] = true
	}
	for model, p := range c.Prices {
		if model == "" {
			return errors.New("config: prices has an empty model key")
		}
		if err := p.Validate(); err != nil {
			return fmt.Errorf("config: prices[%q]: %w", model, err)
		}
	}
	for provider, models := range c.ProviderPrices {
		if provider == "" {
			return errors.New("config: provider_prices has an empty provider key")
		}
		for model, p := range models {
			if model == "" {
				return fmt.Errorf("config: provider_prices[%q] has an empty model key", provider)
			}
			if err := p.Validate(); err != nil {
				return fmt.Errorf("config: provider_prices[%q][%q]: %w", provider, model, err)
			}
		}
	}
	// models.dev sync (opt-in, off by default): a set URL must be
	// a valid http(s) URL with a host, the interval must not be
	// negative, and the timeout must be unset (0 = default 15s)
	// or in [1, 120]s. Validation runs whether or not the sync is
	// enabled, so a typo in a disabled block fails at load
	// instead of surprising the operator the day they enable it.
	if md := c.ModelsDev; md != nil {
		if md.URL != "" {
			u, err := url.Parse(md.URL)
			if err != nil {
				return fmt.Errorf("config: models_dev.url %q is not a valid URL: %w", md.URL, err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("config: models_dev.url %q must use http or https (got %q)", md.URL, u.Scheme)
			}
			if u.Host == "" {
				return fmt.Errorf("config: models_dev.url %q has no host", md.URL)
			}
		}
		if md.IntervalMinutes < 0 {
			return fmt.Errorf("config: models_dev.interval_minutes must not be negative (got %d)", md.IntervalMinutes)
		}
		if md.TimeoutSeconds < 0 || md.TimeoutSeconds > 120 {
			return fmt.Errorf("config: models_dev.timeout_seconds must be in [0, 120] (0 = default 15s, got %d)", md.TimeoutSeconds)
		}
	}
	// Warm-pool sizing: the Little's Law inputs must be sane (a
	// negative rate or latency is nonsense, a ceiling below the
	// floor contradictory). A nil Pools block means defaults and
	// is always valid.
	if c.Pools != nil {
		if err := c.Pools.Validate(); err != nil {
			return err
		}
	}
	// Gateway-key traffic limits (CHECKLIST 195): a negative
	// requests-per-minute or tokens-per-minute is nonsense — 0
	// means unlimited on that axis, never "no requests at all".
	// Expiry needs no constraint: a past expiry is a valid state
	// (an already-expired key), it simply never authenticates.
	for _, k := range c.GatewayKeys {
		if k.RequestsPerMinute < 0 {
			return fmt.Errorf("config: gateway key %q requests_per_minute must not be negative (got %d)", k.ID, k.RequestsPerMinute)
		}
		if k.TokensPerMinute < 0 {
			return fmt.Errorf("config: gateway key %q tokens_per_minute must not be negative (got %d)", k.ID, k.TokensPerMinute)
		}
		if k.RequestsPerDay < 0 {
			return fmt.Errorf("config: gateway key %q requests_per_day must not be negative (got %d)", k.ID, k.RequestsPerDay)
		}
		if k.ThrottleDelayMs < 0 {
			return fmt.Errorf("config: gateway key %q throttle_delay_ms must not be negative (got %d)", k.ID, k.ThrottleDelayMs)
		}
		if k.MaxSessions < 0 {
			return fmt.Errorf("config: gateway key %q max_sessions must not be negative (got %d)", k.ID, k.MaxSessions)
		}
		// M5.10 access schedule: from/until are HH:MM (required
		// when enabled), days are weekdays 0-6, tz must load.
		if s := k.AccessSchedule; s != nil {
			if s.Enabled {
				if s.From == "" || !validResetTime(s.From) {
					return fmt.Errorf("config: gateway key %q access_schedule.from is required (HH:MM) when the schedule is enabled", k.ID)
				}
				if s.Until == "" || !validResetTime(s.Until) {
					return fmt.Errorf("config: gateway key %q access_schedule.until is required (HH:MM) when the schedule is enabled", k.ID)
				}
			} else {
				if s.From != "" && !validResetTime(s.From) {
					return fmt.Errorf("config: gateway key %q access_schedule.from %q is not HH:MM", k.ID, s.From)
				}
				if s.Until != "" && !validResetTime(s.Until) {
					return fmt.Errorf("config: gateway key %q access_schedule.until %q is not HH:MM", k.ID, s.Until)
				}
			}
			for _, d := range s.Days {
				if d < 0 || d > 6 {
					return fmt.Errorf("config: gateway key %q access_schedule.days has %d, which is not a weekday (0-6, 0 = Sunday)", k.ID, d)
				}
			}
			if s.TZ != "" {
				if _, err := time.LoadLocation(s.TZ); err != nil {
					return fmt.Errorf("config: gateway key %q access_schedule.tz %q is not a known IANA zone: %v", k.ID, s.TZ, err)
				}
			}
		}
		for i, r := range k.RateLimits {
			if r.Limit <= 0 {
				return fmt.Errorf("config: gateway key %q rate_limits[%d].limit must be positive (got %d)", k.ID, i, r.Limit)
			}
			if r.WindowSeconds <= 0 {
				return fmt.Errorf("config: gateway key %q rate_limits[%d].window_seconds must be positive (got %d)", k.ID, i, r.WindowSeconds)
			}
		}
		// M5.3 token limits: scope must name its kind, the
		// value must match the scope (required for model /
		// provider, empty for global), the limit must be
		// positive, and the window must be a known interval
		// with an HH:MM reset time.
		for i, tl := range k.TokenLimits {
			switch tl.Scope {
			case "model", "provider":
				if tl.ScopeValue == "" {
					return fmt.Errorf("config: gateway key %q token_limits[%d].scope_value must name the %s for a %q scope", k.ID, i, tl.Scope, tl.Scope)
				}
			case "global":
				if tl.ScopeValue != "" {
					return fmt.Errorf("config: gateway key %q token_limits[%d].scope_value must be empty for a global scope (got %q)", k.ID, i, tl.ScopeValue)
				}
			default:
				return fmt.Errorf("config: gateway key %q token_limits[%d].scope %q is not a known scope (model, provider, global)", k.ID, i, tl.Scope)
			}
			if tl.Limit <= 0 {
				return fmt.Errorf("config: gateway key %q token_limits[%d].limit must be positive (got %d)", k.ID, i, tl.Limit)
			}
			switch tl.ResetInterval {
			case "", "daily", "weekly", "monthly":
			default:
				return fmt.Errorf("config: gateway key %q token_limits[%d].reset_interval %q is not a known interval (daily, weekly, monthly)", k.ID, i, tl.ResetInterval)
			}
			if tl.ResetTime != "" && !validResetTime(tl.ResetTime) {
				return fmt.Errorf("config: gateway key %q token_limits[%d].reset_time %q is not HH:MM", k.ID, i, tl.ResetTime)
			}
		}
	}
	// Rolling per-key 5-hour budgets: the limit must not be
	// negative, the policy must be a known name ("" = default
	// fail_closed), and the key must exist — a budget for a key
	// that was never issued caps nothing and would silently
	// never fire.
	for keyID, b := range c.KeyBudgets {
		if keyID == "" {
			return errors.New("config: key_budgets has an empty key id")
		}
		if b.LimitUSD < 0 {
			return fmt.Errorf("config: key_budgets[%q].limit_usd must not be negative (got %v)", keyID, b.LimitUSD)
		}
		// M5.1 calendar windows: each limit must not be negative
		// (nil = unset); the interval must be a known name and
		// the reset time HH:MM.
		for name, v := range map[string]*float64{"daily_limit_usd": b.DailyLimitUSD, "weekly_limit_usd": b.WeeklyLimitUSD, "monthly_limit_usd": b.MonthlyLimitUSD} {
			if v != nil && *v < 0 {
				return fmt.Errorf("config: key_budgets[%q].%s must not be negative (got %v)", keyID, name, *v)
			}
		}
		switch b.ResetInterval {
		case "", "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("config: key_budgets[%q].reset_interval %q is not a known interval (daily, weekly, monthly)", keyID, b.ResetInterval)
		}
		if b.ResetTime != "" {
			parts := strings.Split(b.ResetTime, ":")
			if len(parts) != 2 || len(parts[0]) != 2 || len(parts[1]) != 2 {
				return fmt.Errorf("config: key_budgets[%q].reset_time %q is not HH:MM", keyID, b.ResetTime)
			}
			h, err1 := strconv.Atoi(parts[0])
			m, err2 := strconv.Atoi(parts[1])
			if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
				return fmt.Errorf("config: key_budgets[%q].reset_time %q is not a valid HH:MM time", keyID, b.ResetTime)
			}
		}
		if b.WarningThreshold < 0 || b.WarningThreshold > 1 {
			return fmt.Errorf("config: key_budgets[%q].warning_threshold must be between 0 and 1 (got %v)", keyID, b.WarningThreshold)
		}
		switch b.UnpricedPolicy {
		case "", "fail_closed", "count_as_zero":
		default:
			return fmt.Errorf("config: key_budgets[%q].unpriced_policy %q is not a known policy (fail_closed, count_as_zero)", keyID, b.UnpricedPolicy)
		}
		if _, ok := c.FindGatewayKey(keyID); !ok {
			return fmt.Errorf("config: key_budgets[%q] names a gateway key that does not exist", keyID)
		}
	}
	// M5.4 fair-share quota pools: ids unique, threshold 0-1,
	// at least one dimension per pool (a dimension-less pool
	// allows everything — pointless), units/windows from the
	// known sets ("percent" is rejected: it has no meaning
	// without an external quota), limits positive, allocations
	// naming existing keys with weights 0-100, policies from
	// hard/soft/burst ("" = hard), caps positive with a known
	// unit when set.
	seenPools := map[string]bool{}
	for i, p := range c.QuotaPools {
		if p.ID == "" {
			return fmt.Errorf("config: quota_pools[%d] has an empty id", i)
		}
		if seenPools[p.ID] {
			return fmt.Errorf("config: quota_pools has a duplicate id %q", p.ID)
		}
		seenPools[p.ID] = true
		if p.Name == "" {
			return fmt.Errorf("config: quota_pools[%q].name must not be empty", p.ID)
		}
		if p.SaturationThreshold < 0 || p.SaturationThreshold > 1 {
			return fmt.Errorf("config: quota_pools[%q].saturation_threshold must be between 0 and 1 (got %v)", p.ID, p.SaturationThreshold)
		}
		if p.AccountCount < 0 {
			return fmt.Errorf("config: quota_pools[%q].account_count must not be negative (got %d)", p.ID, p.AccountCount)
		}
		if len(p.Dimensions) == 0 {
			return fmt.Errorf("config: quota_pools[%q] must have at least one dimension", p.ID)
		}
		seenDims := map[string]bool{}
		for j, d := range p.Dimensions {
			switch d.Unit {
			case "requests", "tokens", "usd":
			default:
				return fmt.Errorf("config: quota_pools[%q].dimensions[%d].unit %q is not a known unit (requests, tokens, usd)", p.ID, j, d.Unit)
			}
			switch d.Window {
			case "5h", "hourly", "daily", "weekly", "monthly":
			default:
				return fmt.Errorf("config: quota_pools[%q].dimensions[%d].window %q is not a known window (5h, hourly, daily, weekly, monthly)", p.ID, j, d.Window)
			}
			if d.Limit <= 0 {
				return fmt.Errorf("config: quota_pools[%q].dimensions[%d].limit must be positive (got %v)", p.ID, j, d.Limit)
			}
			key := d.Unit + ":" + d.Window
			if seenDims[key] {
				return fmt.Errorf("config: quota_pools[%q] has a duplicate dimension %q", p.ID, key)
			}
			seenDims[key] = true
		}
		seenKeys := map[string]bool{}
		for j, a := range p.Allocations {
			if a.KeyID == "" {
				return fmt.Errorf("config: quota_pools[%q].allocations[%d] has an empty key_id", p.ID, j)
			}
			if seenKeys[a.KeyID] {
				return fmt.Errorf("config: quota_pools[%q] allocates key %q twice", p.ID, a.KeyID)
			}
			seenKeys[a.KeyID] = true
			if _, ok := c.FindGatewayKey(a.KeyID); !ok {
				return fmt.Errorf("config: quota_pools[%q].allocations[%d] names a gateway key that does not exist (%q)", p.ID, j, a.KeyID)
			}
			if a.Weight < 0 || a.Weight > 100 {
				return fmt.Errorf("config: quota_pools[%q].allocations[%d].weight must be between 0 and 100 (got %v)", p.ID, j, a.Weight)
			}
			switch a.Policy {
			case "", "hard", "soft", "burst":
			default:
				return fmt.Errorf("config: quota_pools[%q].allocations[%d].policy %q is not a known policy (hard, soft, burst)", p.ID, j, a.Policy)
			}
			if a.CapValue != nil {
				if *a.CapValue <= 0 {
					return fmt.Errorf("config: quota_pools[%q].allocations[%d].cap_value must be positive (got %v)", p.ID, j, *a.CapValue)
				}
				switch a.CapUnit {
				case "requests", "tokens", "usd":
				default:
					return fmt.Errorf("config: quota_pools[%q].allocations[%d].cap_unit %q is not a known unit (requests, tokens, usd)", p.ID, j, a.CapUnit)
				}
			} else if a.CapUnit != "" {
				return fmt.Errorf("config: quota_pools[%q].allocations[%d].cap_unit must be empty without a cap_value", p.ID, j)
			}
		}
	}
	// Quota schedules (M5.9): ids unique, pool must exist, days
	// are JS getDay() 0-6, minutes in range, mode allow/block,
	// timezone loadable, reserves name a known window with a
	// 0-100 percent, budget value and unit together with a known
	// unit/window.
	seenSchedules := map[string]bool{}
	for i, s := range c.QuotaSchedules {
		if s.ID == "" {
			return fmt.Errorf("config: quota_schedules[%d] has an empty id", i)
		}
		if seenSchedules[s.ID] {
			return fmt.Errorf("config: quota_schedules has a duplicate id %q", s.ID)
		}
		seenSchedules[s.ID] = true
		if s.PoolID == "" {
			return fmt.Errorf("config: quota_schedules[%q].pool_id must not be empty", s.ID)
		}
		if !seenPools[s.PoolID] {
			return fmt.Errorf("config: quota_schedules[%q].pool_id %q does not name a quota pool", s.ID, s.PoolID)
		}
		for _, d := range s.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("config: quota_schedules[%q].days has %d, which is not a weekday (0-6, 0 = Sunday)", s.ID, d)
			}
		}
		if s.StartMinute < 0 || s.StartMinute > 1439 {
			return fmt.Errorf("config: quota_schedules[%q].start_minute must be between 0 and 1439 (got %d)", s.ID, s.StartMinute)
		}
		if s.EndMinute < 1 || s.EndMinute > 1440 {
			return fmt.Errorf("config: quota_schedules[%q].end_minute must be between 1 and 1440 (got %d)", s.ID, s.EndMinute)
		}
		switch s.Mode {
		case "allow", "block":
		default:
			return fmt.Errorf("config: quota_schedules[%q].mode %q is not a known mode (allow, block)", s.ID, s.Mode)
		}
		if s.Timezone != "" {
			if _, err := time.LoadLocation(s.Timezone); err != nil {
				return fmt.Errorf("config: quota_schedules[%q].timezone %q is not a known IANA zone: %v", s.ID, s.Timezone, err)
			}
		}
		for j, r := range s.Reserves {
			switch r.Window {
			case "5h", "hourly", "daily", "weekly", "monthly", "any":
			default:
				return fmt.Errorf("config: quota_schedules[%q].reserves[%d].window %q is not a known window (5h, hourly, daily, weekly, monthly, any)", s.ID, j, r.Window)
			}
			if r.Percent < 0 || r.Percent > 100 {
				return fmt.Errorf("config: quota_schedules[%q].reserves[%d].percent must be between 0 and 100 (got %v)", s.ID, j, r.Percent)
			}
		}
		if s.BudgetValue != nil {
			if *s.BudgetValue <= 0 {
				return fmt.Errorf("config: quota_schedules[%q].budget_value must be positive (got %v)", s.ID, *s.BudgetValue)
			}
			switch s.BudgetUnit {
			case "requests", "tokens", "usd":
			default:
				return fmt.Errorf("config: quota_schedules[%q].budget_unit %q is not a known unit (requests, tokens, usd)", s.ID, s.BudgetUnit)
			}
			switch s.BudgetWindow {
			case "5h", "hourly", "daily", "weekly", "monthly":
			default:
				return fmt.Errorf("config: quota_schedules[%q].budget_window %q is not a known window (5h, hourly, daily, weekly, monthly)", s.ID, s.BudgetWindow)
			}
		} else if s.BudgetUnit != "" || s.BudgetWindow != "" {
			return fmt.Errorf("config: quota_schedules[%q].budget_unit/budget_window must be empty without a budget_value", s.ID)
		}
	}
	// Webhooks (M5.9, M8.19): ids unique, URL http/https, events from
	// the emitted set ("quota.exceeded" and "budget.warning").
	seenWebhooks := map[string]bool{}
	for i, w := range c.Webhooks {
		if w.ID == "" {
			return fmt.Errorf("config: webhooks[%d] has an empty id", i)
		}
		if seenWebhooks[w.ID] {
			return fmt.Errorf("config: webhooks has a duplicate id %q", w.ID)
		}
		seenWebhooks[w.ID] = true
		u, err := url.Parse(w.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: webhooks[%q].url %q is not an http(s) URL", w.ID, w.URL)
		}
		if err := ValidateWebhookKind(w); err != nil {
			return err
		}
		for _, ev := range w.Events {
			if !knownWebhookEvent(ev) {
				return fmt.Errorf("config: webhooks[%q] subscribes to unknown event %q (known: %s)", w.ID, ev, strings.Join(KnownWebhookEvents(), ", "))
			}
		}
	}
	// Internal budgets (M5.11): ids unique, scope names its
	// kind, the matching entity id is required for provider /
	// model / pool (and must name an existing pool for pool
	// scope), period daily/weekly/monthly, limit type
	// currency/tokens/requests, limit positive, warning
	// threshold 0-1 (0 = default 0.8).
	seenBudgets := map[string]bool{}
	for i, b := range c.InternalBudgets {
		if b.ID == "" {
			return fmt.Errorf("config: internal_budgets[%d] has an empty id", i)
		}
		if seenBudgets[b.ID] {
			return fmt.Errorf("config: internal_budgets has a duplicate id %q", b.ID)
		}
		seenBudgets[b.ID] = true
		switch b.Scope {
		case "global":
		case "provider":
			if b.ProviderID == "" {
				return fmt.Errorf("config: internal_budgets[%q].provider_id is required for a provider scope", b.ID)
			}
		case "model":
			if b.ModelID == "" {
				return fmt.Errorf("config: internal_budgets[%q].model_id is required for a model scope", b.ID)
			}
		case "pool":
			if b.PoolID == "" {
				return fmt.Errorf("config: internal_budgets[%q].pool_id is required for a pool scope", b.ID)
			}
			if !seenPools[b.PoolID] {
				return fmt.Errorf("config: internal_budgets[%q].pool_id %q does not name a quota pool", b.ID, b.PoolID)
			}
		default:
			return fmt.Errorf("config: internal_budgets[%q].scope %q is not a known scope (global, provider, model, pool)", b.ID, b.Scope)
		}
		switch b.Period {
		case "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("config: internal_budgets[%q].period %q is not a known period (daily, weekly, monthly)", b.ID, b.Period)
		}
		switch b.LimitType {
		case "currency", "tokens", "requests":
		default:
			return fmt.Errorf("config: internal_budgets[%q].limit_type %q is not a known limit type (currency, tokens, requests)", b.ID, b.LimitType)
		}
		if b.LimitValue <= 0 {
			return fmt.Errorf("config: internal_budgets[%q].limit_value must be positive (got %v)", b.ID, b.LimitValue)
		}
		if b.WarningThreshold < 0 || b.WarningThreshold > 1 {
			return fmt.Errorf("config: internal_budgets[%q].warning_threshold must be between 0 and 1 (got %v)", b.ID, b.WarningThreshold)
		}
	}
	// Teams (M5.12): ids unique, name required, budget duration
	// 1d/7d/30d (or empty for no budget), max budget positive
	// and paired with a duration, members name existing gateway
	// keys, and a key belongs to at most one team.
	seenTeams := map[string]bool{}
	seenTeamMembers := map[string]string{}
	for i, t := range c.Teams {
		if t.ID == "" {
			return fmt.Errorf("config: teams[%d] has an empty id", i)
		}
		if seenTeams[t.ID] {
			return fmt.Errorf("config: teams has a duplicate id %q", t.ID)
		}
		seenTeams[t.ID] = true
		if t.Name == "" {
			return fmt.Errorf("config: teams[%q].name is required", t.ID)
		}
		switch t.Status {
		case "", "active", "archived":
		default:
			return fmt.Errorf("config: teams[%q].status %q must be active or archived", t.ID, t.Status)
		}
		if t.MaxBudgetUSD != nil && *t.MaxBudgetUSD <= 0 {
			return fmt.Errorf("config: teams[%q].max_budget_usd must be positive (got %v)", t.ID, *t.MaxBudgetUSD)
		}
		if t.MaxBudgetUSD == nil && t.BudgetDuration != "" {
			return fmt.Errorf("config: teams[%q].budget_duration requires a max_budget_usd", t.ID)
		}
		if t.MaxBudgetUSD != nil && t.BudgetDuration == "" {
			return fmt.Errorf("config: teams[%q].max_budget_usd requires a budget_duration", t.ID)
		}
		switch t.BudgetDuration {
		case "", "1d", "7d", "30d":
		default:
			return fmt.Errorf("config: teams[%q].budget_duration %q is not a known duration (1d, 7d, 30d)", t.ID, t.BudgetDuration)
		}
		for _, m := range t.Members {
			if _, ok := c.FindGatewayKey(m); !ok {
				return fmt.Errorf("config: teams[%q].members names unknown gateway key %q", t.ID, m)
			}
			if other, ok := seenTeamMembers[m]; ok {
				return fmt.Errorf("config: gateway key %q belongs to two teams (%q and %q)", m, other, t.ID)
			}
			seenTeamMembers[m] = t.ID
		}
	}
	// Arena ELO sync block (B2-14): intervals and timeouts
	// are durations, never negative. 0 means the default.
	if ae := c.ArenaElo; ae != nil {
		if ae.IntervalMinutes < 0 {
			return fmt.Errorf("config: arena_elo.interval_minutes must not be negative (got %v)", ae.IntervalMinutes)
		}
		if ae.TimeoutSeconds < 0 {
			return fmt.Errorf("config: arena_elo.timeout_seconds must not be negative (got %v)", ae.TimeoutSeconds)
		}
	}
	return nil
}

// isStrategyName reports whether s names a known routing strategy
// (canonical name or alias, case-insensitive). It is a local
// allowlist mirroring internal/strategy — config must not import
// the strategy package (that would pull the router into config).
func isStrategyName(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "priority", "balanced", "cheapest", "cheap", "fastest", "fast",
		"most-reliable", "reliable", "least-latency", "latency", "weighted", "custom-weights", "custom", "code",
		"round-robin", "random", "strict-random", "fill-first", "least-used", "p2c",
		"quota-weighted", "headroom", "expiry-first", "reset-aware", "reset-window",
		"lkgp", "context-optimized", "cache-optimized", "context-relay",
		"cost-optimized", "eco", "ship-fast", "cost-saver", "quality-first",
		"offline-friendly", "reliability-first", "chaos-mode", "cascade", "fusion":
		return true
	default:
		return false
	}
}

// isAccountFallbackStrategy reports whether s names a known
// account-level fallback strategy (PARITY row 388). "" counts
// (it means "inherit the fill-first default"). The allowlist
// duplicates the 9 names in internal/router
// (ParseAccountFallbackStrategy) because config must not import
// router; "expiry-first" is deliberately absent — no per-key
// quota-expiry source exists, so it can never order accounts.
func isAccountFallbackStrategy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "priority", "fill-first", "round-robin", "p2c", "random", "least-used", "cost-optimized", "strict-random", "weighted":
		return true
	default:
		return false
	}
}

// isCooldownMode reports whether s names a known cooldown mode.
// "" counts (it means "inherit the default", i.e. hard) — except
// inside chain_cooldown_modes, where an entry must name a mode
// explicitly (the caller rejects "" there).
func isCooldownMode(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "hard", "soft-demote":
		return true
	default:
		return false
	}
}

// EnsureRoutingMu initialises the routing mutex for a config
// built literally (no DefaultConfig / Load). Call it before the
// config is shared across goroutines; it is not itself safe to
// call concurrently with a first locked access — construction
// is single-threaded (NewComboStore calls it under Server.combosMu,
// so the lazy first-combos-request construction is serialised).
func (c *Config) EnsureRoutingMu() {
	if c == nil || c.routingMu != nil {
		return
	}
	c.routingMu = &sync.RWMutex{}
}

// EnsurePricesMu initialises the prices mutex for a config
// built literally (no DefaultConfig / Load). Same rule as
// EnsureRoutingMu: call it before the config is shared across
// goroutines; not itself safe to call concurrently with a first
// locked access.
func (c *Config) EnsurePricesMu() {
	if c == nil || c.pricesMu != nil {
		return
	}
	c.pricesMu = &sync.RWMutex{}
}

// RLockRouting takes the routing read lock (nil-safe no-op when
// the mutex was never initialised). Every read of
// Routing.Groups or Routing.ChainStrategies holds it.
func (c *Config) RLockRouting() {
	if c != nil && c.routingMu != nil {
		c.routingMu.RLock()
	}
}

// RUnlockRouting releases the routing read lock.
func (c *Config) RUnlockRouting() {
	if c != nil && c.routingMu != nil {
		c.routingMu.RUnlock()
	}
}

// LockRouting takes the routing write lock (nil-safe no-op when
// the mutex was never initialised). Every write to
// Routing.Groups or Routing.ChainStrategies holds it.
func (c *Config) LockRouting() {
	if c != nil && c.routingMu != nil {
		c.routingMu.Lock()
	}
}

// UnlockRouting releases the routing write lock.
func (c *Config) UnlockRouting() {
	if c != nil && c.routingMu != nil {
		c.routingMu.Unlock()
	}
}

// RLockPrices takes the prices read lock (nil-safe no-op when
// the mutex was never initialised). Every read of
// ProviderPrices that can run concurrently with a pricing
// PATCH holds it: Clone, Save (Validate + Marshal), and
// ProviderPricesSnapshot.
func (c *Config) RLockPrices() {
	if c != nil && c.pricesMu != nil {
		c.pricesMu.RLock()
	}
}

// RUnlockPrices releases the prices read lock.
func (c *Config) RUnlockPrices() {
	if c != nil && c.pricesMu != nil {
		c.pricesMu.RUnlock()
	}
}

// LockPrices takes the prices write lock (nil-safe no-op when
// the mutex was never initialised). The pricing PATCH holds it
// for its ProviderPrices snapshot + apply, and RestoreFrom
// holds it across a whole-struct restore.
func (c *Config) LockPrices() {
	if c != nil && c.pricesMu != nil {
		c.pricesMu.Lock()
	}
}

// UnlockPrices releases the prices write lock.
func (c *Config) UnlockPrices() {
	if c != nil && c.pricesMu != nil {
		c.pricesMu.Unlock()
	}
}

// providerPricesSnapshotLocked returns a deep copy of
// ProviderPrices (nil when no pair prices are configured).
// The caller must hold the prices read lock: Clone holds it
// across the whole Clone, so it snapshots through this helper
// instead of ProviderPricesSnapshot below — calling that here
// would take the read lock a second time, which deadlocks
// when a writer (RestoreFrom, the pricing PATCH) is already
// waiting for it.
func (c *Config) providerPricesSnapshotLocked() map[string]map[string]ModelPrice {
	if c.ProviderPrices == nil {
		return nil
	}
	out := make(map[string]map[string]ModelPrice, len(c.ProviderPrices))
	for provider, models := range c.ProviderPrices {
		cp := make(map[string]ModelPrice, len(models))
		for k, v := range models {
			cp[k] = v.clone()
		}
		out[provider] = cp
	}
	return out
}

// pricesSnapshotLocked returns a deep copy of Prices (nil when
// no model-level prices are configured). Same lock rule as
// providerPricesSnapshotLocked: the caller must hold the
// prices read lock.
func (c *Config) pricesSnapshotLocked() map[string]ModelPrice {
	if c.Prices == nil {
		return nil
	}
	out := make(map[string]ModelPrice, len(c.Prices))
	for k, v := range c.Prices {
		out[k] = v.clone()
	}
	return out
}

// ProviderPricesSnapshot returns a deep copy of
// ProviderPrices (nil when no pair prices are configured),
// taken under the prices read lock. The copy is detached:
// the caller may range it after the lock is released while
// a pricing PATCH rewrites the live maps.
func (c *Config) ProviderPricesSnapshot() map[string]map[string]ModelPrice {
	if c == nil {
		return nil
	}
	c.RLockPrices()
	defer c.RUnlockPrices()
	return c.providerPricesSnapshotLocked()
}

// PricesSnapshot returns a deep copy of Prices (nil when no
// model-level prices are configured), taken under the prices
// read lock. The copy is detached the same way as
// ProviderPricesSnapshot: the caller may range it after the
// lock is released while RestoreFrom — the only live writer
// of Prices, under the prices write lock — swaps the live
// header. (The pricing PATCH never writes Prices; only the
// whole-struct rollback does.)
func (c *Config) PricesSnapshot() map[string]ModelPrice {
	if c == nil {
		return nil
	}
	c.RLockPrices()
	defer c.RUnlockPrices()
	return c.pricesSnapshotLocked()
}

// RestoreFrom restores every field of c from src — the
// whole-struct rollback the danger-zone and key-mutation
// handlers used to do as `*c = *src` (M8.14 race fix, round
// 3). The plain assignment was a race on two counts: it
// swapped the cfg.Routing pointer without the routing write
// lock (racing routingPersist's swap and the combo mirrors),
// and on the reset path src came from DefaultConfig, whose
// routingMu/pricesMu are FRESH mutex objects — the assignment
// swapped the live mutexes out from under concurrent holders,
// breaking mutual exclusion across the swap.
//
// This method fixes both: the Routing pointer is written
// while holding the routing write lock, and the LIVE mutex
// objects are preserved — the copy is field-by-field via
// reflection, skipping every unexported field, so a fresh
// mutex from DefaultConfig can never replace a live one
// (and the mutex pointer fields are never written at all —
// see RestoreFrom's own comment for why even writing the
// same pointer value is a race). src's Routing pointer is
// transferred as-is (a Clone snapshot's Routing is a fresh
// deep copy; DefaultConfig's is nil, which is the reset
// behaviour); src must be caller-private and not
// concurrently mutated — every call site passes a Clone
// snapshot or a fresh DefaultConfig owned by that handler.
// pointer is transferred as-is (a Clone snapshot's Routing
// is a fresh deep copy; DefaultConfig's is nil, which is
// the reset behaviour); src must be caller-private and not
// concurrently mutated — every call site passes a Clone
// snapshot or a fresh DefaultConfig owned by that handler.
//
// Lock order: pricesMu -> routingMu, taken while the caller
// holds dangerMu (danger.go) or keyMu (keystatus.go) —
// dangerMu / keyMu -> pricesMu -> routingMu, the same
// direction Save nests them in, so no cycle: no path ever
// takes pricesMu or routingMu and then acquires dangerMu,
// keyMu, pricingMu or ComboStore.mu (mirrors release
// routingMu before any persist runs; routingPersist
// releases it before Save). The copy is CPU-only, no I/O
// under either lock.
func (c *Config) RestoreFrom(src *Config) {
	if c == nil || src == nil || src == c {
		return
	}
	// The copy runs field-by-field via reflection, skipping
	// every unexported field (today: routingMu, pricesMu) —
	// NOT as a struct assignment `*c = *src`. A struct
	// assignment would WRITE the two mutex pointer fields,
	// and that write races the unlocked pointer reads in
	// RLockRouting / LockRouting / RLockPrices (a reader
	// must read the pointer before it can take the mutex,
	// so no lock ordering can make the write safe) — even
	// when the value written is the same live pointer, the
	// detector flags write-vs-read, and with a DefaultConfig
	// source the value is a FRESH mutex (the original bug).
	// TestDangerKeystatusRestoreRace caught both forms.
	// Reflection keeps the copy future-proof: a field added
	// to Config later is restored automatically; only
	// unexported (mutex) fields are ever skipped. src is
	// caller-private (a Clone snapshot or a fresh
	// DefaultConfig), so reading it needs no lock.
	dst := reflect.ValueOf(c).Elem()
	srcVal := reflect.ValueOf(src).Elem()
	c.LockPrices()
	c.LockRouting()
	for i := 0; i < dst.NumField(); i++ {
		if dst.Type().Field(i).PkgPath != "" {
			continue // unexported: a live mutex, never restored
		}
		dst.Field(i).Set(srcVal.Field(i))
	}
	c.UnlockRouting()
	c.UnlockPrices()
}

// GroupMembers returns a COPY of one group's members ("", false
// when the group does not exist). The copy is made under the
// routing read lock, so the caller may range it after the lock
// is released — this is the groupChainFor read path. The nil
// checks run under the lock: a routingPersist swap may change
// the pointer concurrently.
func (c *Config) GroupMembers(name string) ([]string, bool) {
	if c == nil {
		return nil, false
	}
	c.RLockRouting()
	defer c.RUnlockRouting()
	if c.Routing == nil || c.Routing.Groups == nil {
		return nil, false
	}
	members, ok := c.Routing.Groups[name]
	if !ok {
		return nil, false
	}
	return append([]string(nil), members...), true
}

// GroupsSnapshot returns a deep copy of Routing.Groups (nil
// when no routing block exists), taken under the routing read
// lock. Feeds that range every group (GET /dashboard/groups,
// the combos list) snapshot first and render from the copy.
// The nil checks run under the lock: a routingPersist swap may
// change the pointer concurrently.
func (c *Config) GroupsSnapshot() map[string][]string {
	if c == nil {
		return nil
	}
	c.RLockRouting()
	defer c.RUnlockRouting()
	if c.Routing == nil || c.Routing.Groups == nil {
		return nil
	}
	out := make(map[string][]string, len(c.Routing.Groups))
	for k, v := range c.Routing.Groups {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// ChainStrategy returns one chain's strategy override ("",
// false when none is configured), read under the routing read
// lock. This is the engine ChainStrategy closure's read path
// and strategyForModel's. The nil checks run under the lock:
// a routingPersist swap may change the pointer concurrently.
func (c *Config) ChainStrategy(model string) (string, bool) {
	if c == nil {
		return "", false
	}
	c.RLockRouting()
	defer c.RUnlockRouting()
	if c.Routing == nil || c.Routing.ChainStrategies == nil {
		return "", false
	}
	st, ok := c.Routing.ChainStrategies[model]
	return st, ok
}

// ChainStrategiesSnapshot returns a copy of
// Routing.ChainStrategies (nil when none is configured),
// taken under the routing read lock. The nil checks run
// under the lock: a routingPersist swap may change the
// pointer concurrently.
func (c *Config) ChainStrategiesSnapshot() map[string]string {
	if c == nil {
		return nil
	}
	c.RLockRouting()
	defer c.RUnlockRouting()
	if c.Routing == nil || c.Routing.ChainStrategies == nil {
		return nil
	}
	out := make(map[string]string, len(c.Routing.ChainStrategies))
	for k, v := range c.Routing.ChainStrategies {
		out[k] = v
	}
	return out
}

// RoutingSnapshot returns a deep copy of the whole routing block
// (nil when no routing block exists), taken under the routing
// read lock. Every map, nested map, slice and pointer inside
// the block is copied — Groups, ChainStrategies, ChainWeights
// (inner maps too), ChainCooldownModes, RefusalChains,
// ModelEquivalents, StrategyTree / StrategyTrees (via
// strategytree.Clone), ContextFallbackModels, WeightedWeights,
// ModelLockout (with its error-code slice) and WaitBudgets —
// so the caller may range, read and encode the copy after the
// lock is released, while a combo mirror rewrites the live
// maps or routingPersist swaps the live pointer. This is the
// read path for whole-block readers: the strategies and
// cooldown feeds, the db export encoder, strategyForModel,
// and the engine's per-request chain builders in
// cmd/codvian-router. The nil check runs under the lock: a
// routingPersist swap may change the pointer concurrently.
func (c *Config) RoutingSnapshot() *RoutingConfig {
	if c == nil {
		return nil
	}
	c.RLockRouting()
	defer c.RUnlockRouting()
	return c.cloneRoutingLocked()
}

// cloneRoutingLocked deep-copies c.Routing (nil when absent).
// The caller holds the routing lock (read or write); the copy
// reads c.Routing and every map, slice and pointer inside it.
// Clone and RoutingSnapshot share this one implementation so
// the two copies can never drift apart.
func (c *Config) cloneRoutingLocked() *RoutingConfig {
	if c.Routing == nil {
		return nil
	}
	rc := *c.Routing
	out := &rc
	if c.Routing.ChainStrategies != nil {
		out.ChainStrategies = make(map[string]string, len(c.Routing.ChainStrategies))
		for k, v := range c.Routing.ChainStrategies {
			out.ChainStrategies[k] = v
		}
	}
	if c.Routing.WeightedWeights != nil {
		w := *c.Routing.WeightedWeights
		out.WeightedWeights = &w
	}
	if c.Routing.ChainWeights != nil {
		out.ChainWeights = make(map[string]map[string]float64, len(c.Routing.ChainWeights))
		for k, v := range c.Routing.ChainWeights {
			inner := make(map[string]float64, len(v))
			for tk, tv := range v {
				inner[tk] = tv
			}
			out.ChainWeights[k] = inner
		}
	}
	if c.Routing.ChainCooldownModes != nil {
		out.ChainCooldownModes = make(map[string]string, len(c.Routing.ChainCooldownModes))
		for k, v := range c.Routing.ChainCooldownModes {
			out.ChainCooldownModes[k] = v
		}
	}
	if c.Routing.RequireCapabilities != nil {
		out.RequireCapabilities = append([]string(nil), c.Routing.RequireCapabilities...)
	}
	if c.Routing.AccountFallbackStrategies != nil {
		out.AccountFallbackStrategies = make(map[string]string, len(c.Routing.AccountFallbackStrategies))
		for k, v := range c.Routing.AccountFallbackStrategies {
			out.AccountFallbackStrategies[k] = v
		}
	}
	if c.Routing.MediaTimeoutsMs != nil {
		out.MediaTimeoutsMs = make(map[string]int64, len(c.Routing.MediaTimeoutsMs))
		for k, v := range c.Routing.MediaTimeoutsMs {
			out.MediaTimeoutsMs[k] = v
		}
	}
	if c.Routing.RefusalChains != nil {
		out.RefusalChains = make(map[string][]string, len(c.Routing.RefusalChains))
		for k, v := range c.Routing.RefusalChains {
			out.RefusalChains[k] = append([]string(nil), v...)
		}
	}
	if c.Routing.ModelEquivalents != nil {
		out.ModelEquivalents = make(map[string][]string, len(c.Routing.ModelEquivalents))
		for k, v := range c.Routing.ModelEquivalents {
			out.ModelEquivalents[k] = append([]string(nil), v...)
		}
	}
	if c.Routing.Groups != nil {
		out.Groups = make(map[string][]string, len(c.Routing.Groups))
		for k, v := range c.Routing.Groups {
			out.Groups[k] = append([]string(nil), v...)
		}
	}
	if c.Routing.StrategyTree != nil {
		out.StrategyTree = strategytree.Clone(c.Routing.StrategyTree)
	}
	if c.Routing.StrategyTrees != nil {
		out.StrategyTrees = make(map[string]*strategytree.Node, len(c.Routing.StrategyTrees))
		for k, v := range c.Routing.StrategyTrees {
			out.StrategyTrees[k] = strategytree.Clone(v)
		}
	}
	if c.Routing.ContextFallbackModels != nil {
		out.ContextFallbackModels = append([]string(nil), c.Routing.ContextFallbackModels...)
	}
	// Stage 1 wiring sweep: deep-copy the response-
	// validation pointer (with its substring slices) and
	// the quota-share table so a watcher mutating a clone
	// never moves the live config's values in place.
	if c.Routing.ResponseValidation != nil {
		rv := *c.Routing.ResponseValidation
		if c.Routing.ResponseValidation.ForbiddenSubstrings != nil {
			rv.ForbiddenSubstrings = append([]string(nil), c.Routing.ResponseValidation.ForbiddenSubstrings...)
		}
		if c.Routing.ResponseValidation.RequiredSubstrings != nil {
			rv.RequiredSubstrings = append([]string(nil), c.Routing.ResponseValidation.RequiredSubstrings...)
		}
		out.ResponseValidation = &rv
	}
	if c.Routing.QuotaShares != nil {
		out.QuotaShares = make(map[string]map[string]float64, len(c.Routing.QuotaShares))
		for k, v := range c.Routing.QuotaShares {
			inner := make(map[string]float64, len(v))
			for mk, mv := range v {
				inner[mk] = mv
			}
			out.QuotaShares[k] = inner
		}
	}
	// M4.9 failover blocks: deep-copy the pointers (and the
	// lockout's error-code slice) so a watcher mutating a
	// clone never moves the live config's blocks in place.
	if c.Routing.ModelLockout != nil {
		ml := *c.Routing.ModelLockout
		if c.Routing.ModelLockout.ErrorCodes != nil {
			ml.ErrorCodes = append([]int(nil), c.Routing.ModelLockout.ErrorCodes...)
		}
		out.ModelLockout = &ml
	}
	if c.Routing.WaitBudgets != nil {
		wb := *c.Routing.WaitBudgets
		out.WaitBudgets = &wb
	}
	// SLA block: deep-copy the pointer fields too, so a
	// watcher mutating a clone's SLA never moves the live
	// config's objectives in place.
	if c.Routing.SLA != nil {
		cp := *c.Routing.SLA
		if c.Routing.SLA.TargetP95Ms != nil {
			v := *c.Routing.SLA.TargetP95Ms
			cp.TargetP95Ms = &v
		}
		if c.Routing.SLA.MaxErrorRate != nil {
			v := *c.Routing.SLA.MaxErrorRate
			cp.MaxErrorRate = &v
		}
		if c.Routing.SLA.MaxCostPer1M != nil {
			v := *c.Routing.SLA.MaxCostPer1M
			cp.MaxCostPer1M = &v
		}
		if c.Routing.SLA.HardConstraints != nil {
			v := *c.Routing.SLA.HardConstraints
			cp.HardConstraints = &v
		}
		out.SLA = &cp
	}
	// Nadir block: value fields only, so a struct copy is a
	// full deep copy — a watcher mutating a clone's nadir
	// block never moves the live config's in place.
	if c.Routing.Nadir != nil {
		cp := *c.Routing.Nadir
		out.Nadir = &cp
	}
	return out
}

// Clone returns a deep copy of the config, including Extensions and all
// slices (gateway-key scopes and provider models included), so a watcher
// or other goroutine can hold and mutate its own copy without affecting
// the original. A nil *Config clones to nil.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	// M8.14 race fix, round 3: ProviderPrices is written by
	// the pricing PATCH under the prices write lock while
	// Clone ranges it — snapshot it under the prices read
	// lock BEFORE taking the routing read lock below (lock
	// order pricesMu -> routingMu, never the reverse), so a
	// concurrent PATCH can never race the map range. The
	// routing copy below still runs under the routing read
	// lock, as in round 2.
	// Nit closure (round-3 re-review): Prices has the same
	// shape of hazard — RestoreFrom, the only live writer
	// of the model-level map, swaps its header under the
	// prices write lock while Clone used to range it with no
	// lock held — so Clone now snapshots BOTH price maps
	// under the prices read lock and HOLDS that lock across
	// the whole Clone, down to the Prices assignment below.
	// Holding it is safe: Clone takes no other lock except
	// the routing read lock inside it (pricesMu ->
	// routingMu, the documented order), calls nothing that
	// takes pricesMu again (the snapshots go through the
	// Locked helpers, not the locking Snapshot methods —
	// a nested RLock would deadlock when a writer is
	// waiting), does no I/O under it, and no Clone caller
	// holds routingMu, pricesMu or pricingMu (danger.go and
	// keystatus.go hold at most dangerMu / keyMu; tests hold
	// nothing), so no cycle is introduced. The hold briefly
	// delays a RestoreFrom or pricing PATCH writer — CPU-only
	// copying, the same class as Save holding the prices read
	// lock across Validate + Marshal.
	c.RLockPrices()
	defer c.RUnlockPrices()
	providerPricesSnapshot := c.providerPricesSnapshotLocked()
	pricesSnapshot := c.pricesSnapshotLocked()
	c.RLockRouting()
	out := *c
	out.Routing = c.cloneRoutingLocked()
	c.RUnlockRouting()
	out.ProviderPrices = providerPricesSnapshot
	if c.GatewayKeys != nil {
		out.GatewayKeys = make([]GatewayKey, len(c.GatewayKeys))
		for i, k := range c.GatewayKeys {
			if k.Scopes != nil {
				k.Scopes = append([]string(nil), k.Scopes...)
			}
			if k.Guardrails != nil {
				k.Guardrails = append([]string(nil), k.Guardrails...)
			}
			// Deep-copy the expiry pointer (CHECKLIST 195):
			// a clone that shared it would let a watcher move
			// the live key's expiry in place.
			if k.ExpiresAt != nil {
				exp := *k.ExpiresAt
				k.ExpiresAt = &exp
			}
			// Deep-copy the M5.2/M5.3 limit slices (same rule:
			// a shared slice would let a watcher mutate the
			// live key's limits in place).
			if k.RateLimits != nil {
				k.RateLimits = append([]RateLimitRule(nil), k.RateLimits...)
			}
			if k.TokenLimits != nil {
				k.TokenLimits = append([]TokenLimit(nil), k.TokenLimits...)
				for j := range k.TokenLimits {
					if k.TokenLimits[j].Enabled != nil {
						en := *k.TokenLimits[j].Enabled
						k.TokenLimits[j].Enabled = &en
					}
				}
			}
			// Deep-copy the M5.10 access schedule: the days
			// slice (a shared slice would let a watcher mutate
			// the live key's allowed days in place).
			if k.AccessSchedule != nil {
				sch := *k.AccessSchedule
				if sch.Days != nil {
					sch.Days = append([]int(nil), sch.Days...)
				}
				k.AccessSchedule = &sch
			}
			out.GatewayKeys[i] = k
		}
	}
	// Deep-copy the M5.4 quota pools (same rule): the dimension
	// and allocation slices plus each allocation's cap pointer.
	if c.QuotaPools != nil {
		out.QuotaPools = make([]QuotaPoolConfig, len(c.QuotaPools))
		for i, p := range c.QuotaPools {
			if p.Dimensions != nil {
				p.Dimensions = append([]QuotaPoolDimensionConfig(nil), p.Dimensions...)
			}
			if p.Allocations != nil {
				p.Allocations = append([]QuotaPoolAllocationConfig(nil), p.Allocations...)
				for j := range p.Allocations {
					if p.Allocations[j].CapValue != nil {
						cap := *p.Allocations[j].CapValue
						p.Allocations[j].CapValue = &cap
					}
				}
			}
			out.QuotaPools[i] = p
		}
	}
	// Deep-copy the M5.9 quota schedules: the days and reserves
	// slices plus the budget pointer.
	if c.QuotaSchedules != nil {
		out.QuotaSchedules = make([]QuotaScheduleConfig, len(c.QuotaSchedules))
		for i, s := range c.QuotaSchedules {
			if s.Days != nil {
				s.Days = append([]int(nil), s.Days...)
			}
			if s.Reserves != nil {
				s.Reserves = append([]ScheduleReserve(nil), s.Reserves...)
			}
			if s.BudgetValue != nil {
				bv := *s.BudgetValue
				s.BudgetValue = &bv
			}
			out.QuotaSchedules[i] = s
		}
	}
	// Deep-copy the M5.9 webhooks: the events slice per endpoint.
	if c.Webhooks != nil {
		out.Webhooks = make([]WebhookConfig, len(c.Webhooks))
		for i, w := range c.Webhooks {
			if w.Events != nil {
				w.Events = append([]string(nil), w.Events...)
			}
			if w.Metadata != nil {
				m := make(map[string]string, len(w.Metadata))
				for k, v := range w.Metadata {
					m[k] = v
				}
				w.Metadata = m
			}
			out.Webhooks[i] = w
		}
	}
	// Deep-copy the M5.11 internal budgets (plain structs — a
	// slice copy is enough; there are no pointers or slices
	// inside).
	if c.InternalBudgets != nil {
		out.InternalBudgets = append([]InternalBudgetConfig(nil), c.InternalBudgets...)
	}
	// Deep-copy the M5.12 teams: the members slice plus each
	// team's max-budget pointer.
	if c.Teams != nil {
		out.Teams = make([]TeamConfig, len(c.Teams))
		for i, t := range c.Teams {
			if t.Members != nil {
				t.Members = append([]string(nil), t.Members...)
			}
			if t.MaxBudgetUSD != nil {
				max := *t.MaxBudgetUSD
				t.MaxBudgetUSD = &max
			}
			out.Teams[i] = t
		}
	}
	if c.Providers != nil {
		out.Providers = make([]ProviderConfig, len(c.Providers))
		for i, p := range c.Providers {
			if p.Models != nil {
				p.Models = append([]provider.Model(nil), p.Models...)
				// Deep-copy the deployment pins (CHECKLIST
				// 245): a clone that shared a FixedParams
				// pointer would let a watcher move the live
				// deployment's pin in place.
				for j := range p.Models {
					p.Models[j].FixedParams = p.Models[j].FixedParams.Clone()
				}
			}
			// Deep-copy the multi-key pool (CHECKLIST 175):
			// a clone that shared the Keys slice would let a
			// watcher mutate the live pool's keys in place.
			if p.Keys != nil {
				p.Keys = append([]provider.ProviderKey(nil), p.Keys...)
			}
			out.Providers[i] = p
		}
	}
	if c.AutoRouter.Thresholds != nil {
		out.AutoRouter.Thresholds = make(map[string]float64, len(c.AutoRouter.Thresholds))
		for k, v := range c.AutoRouter.Thresholds {
			out.AutoRouter.Thresholds[k] = v
		}
	}
	if c.AutoRouter.TierModels != nil {
		out.AutoRouter.TierModels = make(map[string][]string, len(c.AutoRouter.TierModels))
		for k, v := range c.AutoRouter.TierModels {
			out.AutoRouter.TierModels[k] = append([]string(nil), v...)
		}
	}
	if c.AutoRouter.IntentTiers != nil {
		out.AutoRouter.IntentTiers = make(map[string]IntentTierConfig, len(c.AutoRouter.IntentTiers))
		for k, v := range c.AutoRouter.IntentTiers {
			out.AutoRouter.IntentTiers[k] = v
		}
	}
	if c.AutoRouter.CodeAllow != nil {
		out.AutoRouter.CodeAllow = append([]string(nil), c.AutoRouter.CodeAllow...)
	}
	if c.AutoRouter.CodeExclude != nil {
		out.AutoRouter.CodeExclude = append([]string(nil), c.AutoRouter.CodeExclude...)
	}
	if c.AutoRouter.CodeRequireCapabilities != nil {
		out.AutoRouter.CodeRequireCapabilities = append([]string(nil), c.AutoRouter.CodeRequireCapabilities...)
	}
	if c.AutoRouter.CodeCapabilities != nil {
		out.AutoRouter.CodeCapabilities = make(map[string][]string, len(c.AutoRouter.CodeCapabilities))
		for k, v := range c.AutoRouter.CodeCapabilities {
			out.AutoRouter.CodeCapabilities[k] = append([]string(nil), v...)
		}
	}
	// The routing block was already deep-copied under the
	// routing read lock at the top of Clone (out.Routing
	// above); nothing to redo here.
	if c.Guardrails != nil {
		g := *c.Guardrails
		if c.Guardrails.External != nil {
			g.External = append([]ExternalGuardrailConfig(nil), c.Guardrails.External...)
		}
		if c.Guardrails.DisabledTypes != nil {
			g.DisabledTypes = append([]string(nil), c.Guardrails.DisabledTypes...)
		}
		if c.Guardrails.CustomPatterns != nil {
			g.CustomPatterns = append([]CustomPatternConfig(nil), c.Guardrails.CustomPatterns...)
		}
		out.Guardrails = &g
	}
	// Responses session cap (CHECKLIST 263–264): a plain value
	// copy — the struct holds only the cap int.
	if c.Responses != nil {
		r := *c.Responses
		out.Responses = &r
	}
	// A2A retention: copy the struct and its PruneEnabled
	// pointer, so a clone that shared it would let a watcher
	// move the live prune switch in place.
	if c.A2A != nil {
		a := *c.A2A
		if c.A2A.PruneEnabled != nil {
			pe := *c.A2A.PruneEnabled
			a.PruneEnabled = &pe
		}
		out.A2A = &a
	}
	if c.Pools != nil {
		pc := *c.Pools
		out.Pools = &pc
	}
	if c.ModelsDev != nil {
		md := *c.ModelsDev
		out.ModelsDev = &md
	}
	if c.LiteLLM != nil {
		ll := *c.LiteLLM
		out.LiteLLM = &ll
	}
	if c.ArenaElo != nil {
		ae := *c.ArenaElo
		out.ArenaElo = &ae
	}
	if c.Ledger != nil {
		l := *c.Ledger
		out.Ledger = &l
	}
	if c.MCP != nil {
		mc := *c.MCP
		out.MCP = &mc
		if c.MCP.Servers != nil {
			mc.Servers = make([]MCPServerConfig, len(c.MCP.Servers))
			for i, s := range c.MCP.Servers {
				if s.Args != nil {
					s.Args = append([]string(nil), s.Args...)
				}
				mc.Servers[i] = s
			}
		}
		if c.MCP.Controls != nil {
			ctl := *c.MCP.Controls
			ctl.Allow = append([]string(nil), c.MCP.Controls.Allow...)
			ctl.Deny = append([]string(nil), c.MCP.Controls.Deny...)
			mc.Controls = &ctl
		}
	}
	if c.Compression != nil {
		cc := *c.Compression
		out.Compression = &cc
		if c.Compression.LosslessModels != nil {
			cc.LosslessModels = append([]string(nil), c.Compression.LosslessModels...)
		}
	}
	// Prices was already deep-copied under the prices read
	// lock at the top of Clone (pricesSnapshot above, held
	// across the whole Clone); nothing to redo here.
	out.Prices = pricesSnapshot
	// ProviderPrices was already deep-copied under the
	// prices read lock at the top of Clone (providerPricesSnapshot
	// above); nothing to redo here.
	if c.KeyBudgets != nil {
		out.KeyBudgets = make(map[string]KeyBudgetConfig, len(c.KeyBudgets))
		for k, v := range c.KeyBudgets {
			// M5.1: deep-copy the window-limit pointers so a
			// watcher mutating a clone never moves the live
			// config's limits in place.
			if v.DailyLimitUSD != nil {
				d := *v.DailyLimitUSD
				v.DailyLimitUSD = &d
			}
			if v.WeeklyLimitUSD != nil {
				w := *v.WeeklyLimitUSD
				v.WeeklyLimitUSD = &w
			}
			if v.MonthlyLimitUSD != nil {
				m := *v.MonthlyLimitUSD
				v.MonthlyLimitUSD = &m
			}
			out.KeyBudgets[k] = v
		}
	}
	if c.Shortlist != nil {
		out.Shortlist = append([]string(nil), c.Shortlist...)
	}
	if c.Extensions != nil {
		out.Extensions = make(map[string]json.RawMessage, len(c.Extensions))
		for k, v := range c.Extensions {
			cp := make(json.RawMessage, len(v))
			copy(cp, v)
			out.Extensions[k] = cp
		}
	}
	return &out
}

// LoadPriceBook resolves the config's price sources into one
// PriceBook: the optional local price file first, then the config
// file's price blocks over it (config wins on a conflict WITHIN
// each level: a config pair price wins over the same file pair, a
// config model-level price over the same file model-level price).
// Across levels the pair always wins over the model-level fallback
// at Lookup time, regardless of source — so a price-file pair price
// for a model beats a config-file model-level price for that model.
// That is the documented specificity rule, not a precedence bug:
// the pair is the more specific claim about what serving that
// model through that provider costs. With neither source set it
// returns an empty book — every model unknown — and a nil error.
// A set PriceFile that cannot be read or fails validation is an
// error: the operator explicitly asked for those prices, so
// startup must fail loudly instead of routing on silently missing
// prices.
func (c *Config) LoadPriceBook() (*PriceBook, error) {
	book := NewPriceBook()
	if c.PriceFile != "" {
		fileBook, err := LoadPriceFile(c.PriceFile)
		if err != nil {
			return nil, err
		}
		book = fileBook
	}
	// M8.14 race fix, round 3: ProviderPrices is written
	// live by the pricing PATCH — range a snapshot taken
	// under the prices read lock, never the live map (the
	// len check reads the snapshot too, for the same reason).
	// (LoadPriceBook's only live caller is startup in
	// cmd/codvian-router, but the snapshot costs nothing
	// and keeps the read correct if that ever changes.)
	providerPrices := c.ProviderPricesSnapshot()
	if len(c.Prices) != 0 || len(providerPrices) != 0 {
		inline := NewPriceBook()
		for provider, models := range providerPrices {
			for model, p := range models {
				pk := PriceKey{Provider: provider, Model: model}
				inline.Pairs[pk] = p
				inline.PairSources[pk] = "config"
			}
		}
		for model, p := range c.Prices {
			inline.Entries[model] = p
			inline.Sources[model] = "config"
		}
		// Config-file entries override the price file: merge the
		// file book under the inline book (receiver wins).
		merged := inline
		merged.Merge(book)
		merged.HasFile = book.HasFile
		merged.FileModels = book.FileModels
		book = merged
	}
	return book, nil
}

// ValidateFile loads the config file at path, runs Config.Validate, and
// additionally rejects duplicate gateway key IDs, duplicate provider IDs,
// and duplicate provider base URLs (after normalization: surrounding
// whitespace and trailing slashes removed, scheme and host lowercased,
// default ports stripped). The checks inherited from Validate cover:
// version != 1, a listen address that is not loopback, a negative
// budget_usd, and any provider with an empty id or base_url. Every error
// names the offending field and value.
//
// Limitation: base-URL normalization is syntactic only. URLs that differ
// in ways normalization cannot see (different hostnames resolving to the
// same server, percent-encoding, query strings) are not treated as
// duplicates, and an unparseable base URL is compared as a lowercased,
// trimmed string.
func ValidateFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: cannot read %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("config: invalid JSON in %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	seenKeys := make(map[string]bool, len(cfg.GatewayKeys))
	for _, k := range cfg.GatewayKeys {
		if seenKeys[k.ID] {
			return fmt.Errorf("config: duplicate gateway key id %q", k.ID)
		}
		seenKeys[k.ID] = true
	}
	seenProviders := make(map[string]bool, len(cfg.Providers))
	seenURLs := make(map[string]string, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if seenProviders[p.ID] {
			return fmt.Errorf("config: duplicate provider id %q", p.ID)
		}
		seenProviders[p.ID] = true
		if p.BaseURL == "" {
			continue // catalog-resolved at load; nothing to compare yet
		}
		norm := normalizeBaseURL(p.BaseURL)
		if first, ok := seenURLs[norm]; ok {
			return fmt.Errorf("config: duplicate provider base_url %q (provider %q): same normalized URL as provider %q", p.BaseURL, p.ID, first)
		}
		seenURLs[norm] = p.ID
	}
	return nil
}

// validNadirEndpoint mirrors the engine's
// normalizeNadirBaseURL (internal/router/nadir.go) for
// config validation: trims whitespace and trailing slashes,
// strips a trailing "/v1", and requires an absolute http(s)
// URL with a host. The config package cannot import the
// router (the router imports config), so the check is
// duplicated here — keep the two in sync. An empty value is
// NOT valid here; callers treat "" as "unconfigured" before
// calling this.
func validNadirEndpoint(raw string) bool {
	value := strings.TrimSpace(raw)
	value = strings.TrimRight(value, "/")
	value = strings.TrimSuffix(value, "/v1")
	value = strings.TrimRight(value, "/")
	if value == "" {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// normalizeBaseURL canonicalizes a provider base URL for duplicate
// detection: trims surrounding whitespace and trailing slashes,
// lowercases the scheme and host, and strips a default port (:80 for
// http, :443 for https). Anything it cannot parse as a URL with a scheme
// and host is returned lowercased and trimmed instead.
func normalizeBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) ||
		(u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	u.Host = host
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// EnsureDataDir creates the data directory (0700) if it does not exist.
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("config: cannot create data_dir %q: %w", c.DataDir, err)
	}
	return nil
}

// Load reads and validates the config file at path.
// It returns an error (wrapping os.ErrNotExist) when the file is missing;
// callers that want creation should use LoadOrCreate.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: invalid JSON in %q: %w", path, err)
	}
	// The routing and prices mutexes are unexported and
	// untagged, so Unmarshal leaves them nil — initialise
	// them here so the loaded config is safe to share across
	// goroutines (M8.14 race fix).
	cfg.EnsureRoutingMu()
	cfg.EnsurePricesMu()
	// Fill in zero-value slices so callers never see nil.
	if cfg.GatewayKeys == nil {
		cfg.GatewayKeys = []GatewayKey{}
	}
	if cfg.Providers == nil {
		cfg.Providers = []ProviderConfig{}
	}
	// RT-1: fill in the per-combo deadline budget when the
	// config file omits it. A fresh config carries 300 s
	// (DefaultDeadlineBudgetMs); a pre-existing config that
	// never set the field loaded with 0, which means no
	// budget at all. Presence is checked against the raw
	// JSON — an explicit 0 still disables the budget and
	// must not be overridden (see DeadlineBudgetMs), and a
	// missing routing block is created with the default.
	var probe struct {
		Routing *struct {
			DeadlineBudgetMs *int64 `json:"deadline_budget_ms"`
			Nadir            *struct {
				CooldownMs *int `json:"cooldown_ms"`
			} `json:"nadir"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		if cfg.Routing == nil {
			cfg.Routing = &RoutingConfig{DeadlineBudgetMs: DefaultDeadlineBudgetMs}
		} else if probe.Routing == nil || probe.Routing.DeadlineBudgetMs == nil {
			cfg.Routing.DeadlineBudgetMs = DefaultDeadlineBudgetMs
		}
		// G5: fill in the nadir failure cooldown when the
		// nadir block exists but omits it. A fresh block
		// carries 30 s (DefaultNadirCooldownMs); a
		// pre-existing block that never set the field loaded
		// with 0, which the engine reads as the default
		// anyway — the fill-in makes the effective value
		// visible in the loaded config. Presence is checked
		// against the raw JSON, so an explicit value is
		// never overridden; an absent nadir block stays nil
		// (fail-closed) and is not created here.
		if cfg.Routing != nil && cfg.Routing.Nadir != nil &&
			(probe.Routing == nil || probe.Routing.Nadir == nil || probe.Routing.Nadir.CooldownMs == nil) {
			cfg.Routing.Nadir.CooldownMs = DefaultNadirCooldownMs
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadOrCreate loads the config at path, creating a default one (and its
// parent directory) when the file does not exist yet. The created file is
// written with 0600 permissions via Save.
func LoadOrCreate(path string) (*Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dataDir, derr := DefaultDataDir()
	if derr != nil {
		return nil, derr
	}
	cfg = DefaultConfig(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("config: cannot create config directory for %q: %w", path, err)
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config to path atomically: it writes a temp file in the
// same directory (0600), fsyncs it, then renames over path, so a crash
// mid-write can never leave a half-written config behind. It also creates
// the parent directory when needed.
func (c *Config) Save(path string) error {
	// M8.14 race fix, round 3: Validate reads Routing.Groups
	// (the refusal-chain group check) and MarshalJSON reads
	// both routing maps AND ProviderPrices, while a combo
	// mirror may be writing the routing maps and a pricing
	// PATCH may be writing ProviderPrices. Hold the prices
	// read lock first, then the routing read lock inside it
	// (lock order pricesMu -> routingMu, never reversed —
	// Clone snapshots prices before routing, and the pricing
	// PATCH releases its prices write lock before calling
	// configPersist, so this read lock can never deadlock
	// against a held write lock), for Validate + Marshal only
	// (CPU work), and release both before any file I/O below,
	// so neither lock is ever held across a disk write.
	// Callers must not call Save while holding the routing
	// or prices write lock.
	c.RLockPrices()
	c.RLockRouting()
	vErr := c.Validate()
	var raw []byte
	var mErr error
	if vErr == nil {
		raw, mErr = json.MarshalIndent(c, "", "  ")
	}
	c.RUnlockRouting()
	c.RUnlockPrices()
	if vErr != nil {
		return vErr
	}
	if mErr != nil {
		return fmt.Errorf("config: cannot encode config: %w", mErr)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: cannot create directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: cannot create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any failure path.
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot write temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot set temp config permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: cannot close temp config: %w", err)
	}
	if err := renameFile(tmpName, path); err != nil {
		return fmt.Errorf("config: cannot replace %q: %w", path, err)
	}
	return nil
}

// renameFile moves tmp over path, retrying briefly: on Windows the
// rename can fail transiently with "Access is denied" when a scanner
// (Defender/indexer) briefly holds the existing file open, even
// though the temp file is closed and nothing else in-process holds
// the destination (TestPricingCloneSaveRace hit this under a 2s
// concurrent-save soak). The retry is harmless on Unix, where the
// first rename succeeds. Mirrors the tray PID file's renamePIDFile.
func renameFile(tmp, path string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(tmp, path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// SetListenPort returns a copy of the listen address with the port replaced
// and the host forced to 127.0.0.1, honouring the --port CLI flag without
// ever opening the router to the network.
func (c *Config) SetListenPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("config: invalid port %d", port)
	}
	c.Listen = net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	return nil
}

// ListenPort parses and returns the port from the listen address.
func (c *Config) ListenPort() (int, error) {
	_, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return 0, fmt.Errorf("config: invalid listen address %q: %w", c.Listen, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, fmt.Errorf("config: invalid listen port %q: %w", portStr, err)
	}
	return port, nil
}
