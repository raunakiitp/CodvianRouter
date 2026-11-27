// Package server implements the OpenAI-compatible HTTP gateway for Codvian Router.
//
// Stdlib only (net/http). It also serves the static dashboard UI at
// GET /dashboard (the approved dashboard embedded in the web package);
// the dashboard page itself needs no gateway key, and neither do its
// live provider feeds (GET /dashboard/providers and GET /api/providers,
// which carry only operator-visible provider facts — verification,
// wire, auth kind, health — and never a key), while every /v1/* route
// and /metrics stay auth-gated.
//
// CONTRACT NOTES — read before wiring in the real packages:
//
//   - Chat types come from internal/provider (Message, ChatRequest,
//     ChatResponse, Usage). The Engine interface here is the contract the
//     router must satisfy: Chat + ChatStream over provider types. The
//     router package (internal/router) does not exist yet; when it lands
//     it should implement this Engine interface and main.go can pass it to
//     New. No local copies of the chat types are kept.
//   - Auth goes through the KeyVerifier interface. The production
//     implementation is NewConfigVerifier, which adapts
//     *config.Config.VerifyGatewayKey (config stores only SHA-256 hashes
//     of gateway keys). StaticKeyVerifier is a test/fallback helper only.
//     Auth is ON by default: with no verifier configured every /v1/*
//     request is rejected, and there is no bypass flag.
//   - /v1/models reads from the ModelRegistry interface when one is
//     supplied, and otherwise falls back to the model IDs given via
//     WithModels (the configured models). AdaptProviderRegistry adapts
//     the real *provider.Registry (which lists providers, each with its
//     own models) to that interface.
//
// Logging policy: only route, model, status and latency are logged. API
// keys and message content are never logged.
package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/compress"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/learned"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/mcptools"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/modelsync"
	"github.com/raunakiitp/codvian-router/internal/oauth"
	"github.com/raunakiitp/codvian-router/internal/observability"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/providerlogin"
	"github.com/raunakiitp/codvian-router/internal/readiness"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/responses"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/scoring"
	"github.com/raunakiitp/codvian-router/internal/strategy"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
	"github.com/raunakiitp/codvian-router/web"
)

// DefaultTimeout is the per-request timeout applied to /v1/* handlers.
const DefaultTimeout = 120 * time.Second

// maxRequestBody caps a single chat-completions request body (10 MiB).
const maxRequestBody = 10 << 20

// maxAdminJSONBody bounds a small JSON control payload (org/team, eval
// schedule, batch job, fallback chain, MCP call, ...). These handlers
// used to decode r.Body with no limit at all (AUD-SEC-003), so a client
// could stream an unbounded body into memory. 1 MiB is far above any
// legitimate payload of this shape and far below a memory risk.
const maxAdminJSONBody = 1 << 20

// ---------------------------------------------------------------------------
// Engine contract (router to implement; see package doc).
// ---------------------------------------------------------------------------

// Engine is the routing engine contract, over provider types. ChatStream
// calls onDelta once per content fragment (in order) and returns the
// completed response; the engine owns provider failover, so a ChatStream
// that fails over internally must not re-emit fragments it already
// delivered.
type Engine interface {
	Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)
	ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error)
}

// StreamResumer is optionally implemented by engines that support
// an explicit mid-stream resume (a caller asking, outright, to
// resume from an offset — e.g. a client reconnect). skipChars is
// the number of runes of content the client has already received;
// the engine must not re-emit them. The server additionally trims
// any replayed overlap (see trimAlreadySent) as a safety net, so a
// resume that ignores skipChars still cannot duplicate content on
// the wire. The server does NOT invoke resume automatically after
// an in-request stream failure: once the first content byte is
// sent, the stream ends with an honest SSE error frame instead
// (research-log entries 97 + 111 — no switch after commit).
type StreamResumer interface {
	ChatStreamFrom(ctx context.Context, req provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error)
}

// chunkEngine is optionally implemented by engines that stream
// chunk-aware: onChunk is called once per chunk (text delta,
// tool-call deltas, finish reason) and the returned response holds
// the reassembled result. The server prefers it over the text-only
// ChatStream so tool calls and the real finish reason reach the
// client instead of a hard-coded "stop".
type chunkEngine interface {
	ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

// chunkResumer is optionally implemented by chunk engines that support
// an explicit mid-stream resume, chunk-aware (see server.StreamResumer
// for the text-only form). Like StreamResumer, it is an explicit
// entry point only — never invoked automatically after a failure.
type chunkResumer interface {
	ChatStreamFromChunks(ctx context.Context, req provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

// ---------------------------------------------------------------------------
// Gateway-key verification.
// ---------------------------------------------------------------------------

// KeyVerifier verifies a gateway key. Implementations must not log keys.
type KeyVerifier interface {
	VerifyKey(ctx context.Context, key string) bool
}

// KeyIDVerifier is optionally implemented by KeyVerifiers that can
// name the gateway key a presented key belongs to. The server uses
// it only to derive the caller identity for prompt-cache affinity
// (the auto prompt_cache_key); a verifier without it simply yields
// no caller identity and no auto key is generated.
type KeyIDVerifier interface {
	KeyID(ctx context.Context, key string) (string, bool)
}

// ExpiryVerifier is optionally implemented by KeyVerifiers that can
// name an expired gateway key (CHECKLIST 195). The server asks it
// when VerifyKey fails, so the 401 can say the key expired (and
// when) instead of a generic "invalid gateway key". A verifier
// without it simply yields the generic failure.
type ExpiryVerifier interface {
	ExpiredKey(ctx context.Context, key string) (time.Time, bool)
}

// configVerifier adapts *config.Config to KeyVerifier via
// Config.VerifyGatewayKey (hash comparison, scopes/budgets live in config).
type configVerifier struct {
	cfg *config.Config
}

// NewConfigVerifier returns a KeyVerifier backed by cfg. This is the
// production verifier: main.go should pass the loaded *config.Config.
func NewConfigVerifier(cfg *config.Config) KeyVerifier {
	return configVerifier{cfg: cfg}
}

// VerifyKey reports whether key matches any gateway key stored in cfg.
// An expired key does NOT verify (CHECKLIST 195): expiry is enforced
// here, at the one auth check every route funnels through, so an
// expired key cannot reach any /v1/* or management route. The
// failure path names the expiry via ExpiredKey, below — never a
// generic "invalid gateway key".
func (v configVerifier) VerifyKey(_ context.Context, key string) bool {
	if v.cfg == nil || key == "" {
		return false
	}
	k, ok := v.cfg.VerifyGatewayKey(key)
	if !ok {
		return false
	}
	return !k.Expired(time.Now())
}

// ExpiredKey reports whether key matches a stored gateway key whose
// expiry has passed, and the expiry time when it has. It is the
// server-side companion to config.VerifyGatewayKeyExpired: the auth
// wrappers ask it when VerifyKey fails, so the 401 can say "expired
// <time>" instead of a generic auth failure.
func (v configVerifier) ExpiredKey(_ context.Context, key string) (time.Time, bool) {
	if v.cfg == nil || key == "" {
		return time.Time{}, false
	}
	return v.cfg.VerifyGatewayKeyExpired(key)
}

// KeyID returns the stored gateway-key ID for a presented key, so
// the request path can name the caller (prompt-cache affinity keys
// on it) without ever handling the raw key again after auth. The ID
// is an identifier, not a secret, and is never logged or returned
// to any client.
func (v configVerifier) KeyID(_ context.Context, key string) (string, bool) {
	if v.cfg == nil || key == "" {
		return "", false
	}
	k, ok := v.cfg.VerifyGatewayKey(key)
	if !ok {
		return "", false
	}
	return k.ID, true
}

// ScopeVerifier is optionally implemented by KeyVerifiers that can
// name the scopes a presented key carries (M1.1). The server's
// scope wrapper asks it after auth passes; a verifier without it
// (StaticKeyVerifier: tests / no-config fallback) carries no scope
// metadata, so the scope gate cannot apply to its keys and they
// pass through. Production always uses NewConfigVerifier, so every
// real key is scope-checked.
type ScopeVerifier interface {
	KeyScopes(ctx context.Context, key string) ([]string, bool)
}

// KeyScopes returns the stored scopes of the key raw belongs to.
// A disabled key yields ok=false: it never verifies, so it has no
// usable scopes — auth has already rejected it by then.
func (v configVerifier) KeyScopes(_ context.Context, key string) ([]string, bool) {
	if v.cfg == nil || key == "" {
		return nil, false
	}
	k, ok := v.cfg.VerifyGatewayKey(key)
	if !ok {
		return nil, false
	}
	return k.Scopes, true
}

// DisabledKeyVerifier is optionally implemented by KeyVerifiers
// that can name a disabled gateway key (M1.3), so the 401 can say
// the key is disabled instead of a generic "invalid gateway key".
type DisabledKeyVerifier interface {
	DisabledKey(ctx context.Context, key string) bool
}

// DisabledKey reports whether key matches a stored gateway key
// that is disabled (M1.3).
func (v configVerifier) DisabledKey(_ context.Context, key string) bool {
	if v.cfg == nil || key == "" {
		return false
	}
	return v.cfg.VerifyGatewayKeyDisabled(key)
}

// StaticKeyVerifier is an in-memory KeyVerifier for tests and for use
// when no config is loaded. It is NOT the production path — use
// NewConfigVerifier with the loaded config instead. Keys are stored only
// as SHA-256 hashes and compared in constant time.
type StaticKeyVerifier struct {
	hashes map[[32]byte]struct{}
}

// NewStaticKeyVerifier builds a verifier for the given keys. Empty keys
// are ignored.
func NewStaticKeyVerifier(keys ...string) *StaticKeyVerifier {
	v := &StaticKeyVerifier{hashes: make(map[[32]byte]struct{}, len(keys))}
	for _, k := range keys {
		if k == "" {
			continue
		}
		v.hashes[sha256.Sum256([]byte(k))] = struct{}{}
	}
	return v
}

// VerifyKey reports whether key is one of the configured keys.
func (v *StaticKeyVerifier) VerifyKey(_ context.Context, key string) bool {
	if v == nil || key == "" {
		return false
	}
	sum := sha256.Sum256([]byte(key))
	for h := range v.hashes {
		if subtle.ConstantTimeCompare(h[:], sum[:]) == 1 {
			return true
		}
	}
	return false
}

// KeyID names a verified key for the static verifier: there is no
// stored ID (keys are hashes only), so the ID is a stable derived
// identifier — "static-" plus the first 12 hex characters of the
// key's SHA-256. It is never the key itself, never logged, and only
// used as the caller identity for prompt-cache affinity. Two
// different keys get different IDs; the same key always gets the
// same one.
func (v *StaticKeyVerifier) KeyID(_ context.Context, key string) (string, bool) {
	if !v.VerifyKey(context.Background(), key) {
		return "", false
	}
	sum := sha256.Sum256([]byte(key))
	return "static-" + hex.EncodeToString(sum[:])[:12], true
}

// KeyScopes reports the scopes carried by a static key. StaticKeyVerifier
// is a test/fallback helper only, so static keys carry the full scope set
// (admin, chat, models) used by the guard table. This keeps requireScope
// enforced (P2-IDENT-003) while allowing test keys to pass scoped routes;
// production deployments use configVerifier with real per-key scopes.
func (v *StaticKeyVerifier) KeyScopes(_ context.Context, key string) ([]string, bool) {
	if !v.VerifyKey(context.Background(), key) {
		return nil, false
	}
	return []string{"admin", "chat", "models"}, true
}

// ---------------------------------------------------------------------------
// Model registry.
// ---------------------------------------------------------------------------

// Model is one entry in a GET /v1/models list response.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// Deprecated marks a model the provider has retired (operator
	// flag or the catalog's retired-model table). Deprecated rows
	// are hidden from the default list; ?include=deprecated lists
	// them again, with this field set. Requests naming a deprecated
	// model are still forwarded — the field is information, never a
	// block.
	Deprecated bool `json:"deprecated,omitempty"`
	// ProviderID is the registry ID of the provider this row is
	// listed under (the first provider offering the model, in
	// registry order — /v1/models lists each model ID once).
	// An extension beyond the OpenAI shape, so the `provider`
	// filter can match it without guessing from OwnedBy.
	ProviderID string `json:"provider_id,omitempty"`
	// Category is how the user connects to this row's provider
	// (the catalog category: api-key, free, local, no-auth,
	// oauth, ...; "custom" for a hand-added provider). It is the
	// `category` filter of GET /v1/models.
	Category string `json:"category,omitempty"`
	// ContextWindow is the model's context window in tokens, when
	// the provider reports one; 0 = unknown. It is the
	// `min_context` filter of GET /v1/models.
	ContextWindow int `json:"context_window,omitempty"`
	// InputPriceUSD / OutputPriceUSD are the provider-reported
	// prices (USD per 1M tokens) when the provider reports them;
	// 0 = unknown (the provider does not report pricing via
	// /v1/models). They are the fallback for the `max_price`
	// filter when the operator price book has no blended price
	// for the model — the book wins when it has one.
	InputPriceUSD  float64 `json:"input_price_usd,omitempty"`
	OutputPriceUSD float64 `json:"output_price_usd,omitempty"`
	// Shortlisted is true when the operator pinned this model in
	// the config `shortlist` (CHECKLIST 199-200). Shortlisted
	// rows sort first in the list.
	Shortlisted bool `json:"shortlisted,omitempty"`
	// ModelsFetchedAt is when this row's provider's model list
	// was last fetched live from the upstream (CT-7): the
	// add-time fetch or the latest refresh (manual or
	// scheduled). Nil (omitted) means never fetched — a provider
	// loaded from a config written before the field existed, or
	// one added through the chat-only path with no list — so the
	// row's age is unknown, never assumed fresh. A pointer (not
	// a bare time.Time) so the zero value omits from JSON instead
	// of emitting a year-1 timestamp.
	ModelsFetchedAt *time.Time `json:"models_fetched_at,omitempty"`
	// ModelsStale is the staleness badge (CT-7): true when the
	// provider's stored model list is older than the scheduled
	// refresh interval (24h, see
	// provider.ModelListRefreshInterval) or was never fetched.
	// A stale list may be missing new models or still naming
	// delisted ones. False is omitted.
	ModelsStale bool `json:"models_stale,omitempty"`
	// Capabilities records the structured capability tags for the model
	// (e.g. "tools", "vision", "audio", "video", "reasoning", "code", "json_schema").
	Capabilities []string `json:"capabilities,omitempty"`
	// Tools is the tri-state tool/function-calling capability flag (CT-1).
	Tools *bool `json:"tools,omitempty"`
	// Vision reports whether the model accepts visual/image input.
	Vision bool `json:"vision,omitempty"`
	// Audio reports whether the model accepts audio input.
	Audio bool `json:"audio,omitempty"`
	// Video reports whether the model accepts video input.
	Video bool `json:"video,omitempty"`
	// Reasoning reports whether the model has deep reasoning/thinking capability.
	Reasoning bool `json:"reasoning,omitempty"`
}

// ModelRegistry lists the models the gateway may serve. When no registry
// is configured the server falls back to the model IDs from WithModels.
type ModelRegistry interface {
	Models(ctx context.Context) ([]Model, error)
}

// providerRegistryAdapter adapts *provider.Registry — which lists
// providers, each carrying its own models — to ModelRegistry by
// flattening all enabled providers' models (deduplicated by model ID).
type providerRegistryAdapter struct {
	reg *provider.Registry
}

// AdaptProviderRegistry adapts a *provider.Registry to ModelRegistry.
func AdaptProviderRegistry(reg *provider.Registry) ModelRegistry {
	return providerRegistryAdapter{reg: reg}
}

// DashboardProviders returns the live per-provider summaries for the
// dashboard feed (see DashboardProviderLister).
func (a providerRegistryAdapter) DashboardProviders() []provider.DashboardProvider {
	if a.reg == nil {
		return []provider.DashboardProvider{}
	}
	return a.reg.DashboardProviders()
}

// DashboardModels returns the live model catalog for the dashboard
// feed (see DashboardModelLister): every model of every provider in
// the registry, with its best-effort metadata.
func (a providerRegistryAdapter) DashboardModels() []provider.DashboardModel {
	if a.reg == nil {
		return []provider.DashboardModel{}
	}
	return a.reg.DashboardModels()
}

// isCatalogDeprecatedModel reports whether the catalog's
// retired-model table lists id (research-log entry 179's deprecated
// rule: the catalog marks it, the list hides it by default).
func isCatalogDeprecatedModel(id string) bool {
	_, ok := catalog.IsDeprecated(id)
	return ok
}

// fetchedAtPtr returns nil for a never-fetched model list (zero
// time), so the models_fetched_at field is omitted from the JSON
// row instead of emitting a year-1 timestamp; otherwise a pointer
// to the fetch time.
func fetchedAtPtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Models returns every model of every provider in the registry.
func (a providerRegistryAdapter) Models(_ context.Context) ([]Model, error) {
	if a.reg == nil {
		return []Model{}, nil
	}
	seen := make(map[string]struct{})
	var out []Model
	for _, p := range a.reg.List() {
		if p == nil {
			continue
		}
		for _, m := range p.Models {
			if m.ID == "" {
				continue
			}
			if _, dup := seen[m.ID]; dup {
				continue
			}
			seen[m.ID] = struct{}{}
			var caps []string
			if m.Tools != nil && *m.Tools {
				caps = append(caps, "tools")
			}
			if m.Vision {
				caps = append(caps, "vision")
			}
			if m.Audio {
				caps = append(caps, "audio")
			}
			if m.Video {
				caps = append(caps, "video")
			}
			if m.Reasoning {
				caps = append(caps, "reasoning")
			}
			out = append(out, Model{
				ID: m.ID, Object: "model", OwnedBy: p.Name,
				Deprecated:      m.Deprecated || isCatalogDeprecatedModel(m.ID),
				ProviderID:      p.ID,
				Category:        provider.CategoryForProvider(p),
				ContextWindow:   m.ContextWindow,
				InputPriceUSD:   m.InputPriceUSD,
				OutputPriceUSD:  m.OutputPriceUSD,
				ModelsFetchedAt: fetchedAtPtr(p.ModelsFetchedAt),
				ModelsStale:     p.ModelsListStale(time.Now()),
				Capabilities:    caps,
				Tools:           m.Tools,
				Vision:          m.Vision,
				Audio:           m.Audio,
				Video:           m.Video,
				Reasoning:       m.Reasoning,
			})
		}
	}
	if out == nil {
		out = []Model{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Server.
// ---------------------------------------------------------------------------

// Server is the OpenAI-compatible HTTP gateway.
type Server struct {
	engine   Engine
	verifier KeyVerifier
	cfg      *config.Config // nil = no config loaded (settings export answers 503)
	// oauth is the lazy OAuth 2.0 issuer (B3-07, oauth_api.go):
	// created on first OAuth use from cfg.DataDir (in-memory
	// only when cfg is nil or has no data dir). oauthMu guards
	// oauth/oauthErr; a failed init is remembered so the OAuth
	// endpoints answer 503 instead of retrying a corrupt state.
	oauthMu  sync.Mutex
	oauth    *oauth.Issuer
	oauthErr error
	// dashAuth is the lazy dashboard password/session store
	// (B3-04, dashauth.go): created on first dashboard-auth use
	// from cfg.DataDir (in-memory only when cfg is nil or has no
	// data dir). dashAuthMu guards dashAuth/dashAuthErr; a
	// corrupt persisted state is remembered so the auth
	// endpoints answer 503 instead of silently opening the
	// dashboard.
	dashAuthMu  sync.Mutex
	dashAuth    *dashAuthStore
	dashAuthErr error
	// totpAuth is the lazy dashboard TOTP store (PR #51): created on first
	// TOTP use from cfg.DataDir (in-memory only when cfg is nil or has no
	// data dir). Guarded by dashAuthMu.
	totpAuth    *totpStore
	totpAuthErr error
	// oauthStore is the lazy dashboard OAuth connection store (PR #52):
	// created on first OAuth use from cfg.DataDir. Guarded by dashAuthMu.
	oauthStore *oauthConnStore
	// tosGate is the lazy ToS risk-acknowledgement store (A3 item 6,
	// tos_gate.go): WHO acknowledged WHICH ToS-grey provider WHEN.
	// The provider registry's gate reads it through ToSAcknowledged.
	tosGate   *tosGateStore
	tosGateMu sync.Mutex
	// ipControl is the lazy IP allowlist/blocklist store (PR #53):
	// created on first IP-control use. Guarded by dashAuthMu.
	ipControl *ipControlStore
	// evalStateMu guards the lazily-opened P3-B eval store/scheduler
	// (eval_schedules_api.go). It lives on THIS Server instance (not a
	// package global) so two Servers with different data dirs are
	// isolated.
	evalStateMu sync.Mutex
	evalState   *evalState
	// evalEngine is the engine used by the scheduled evaluation scheduler.
	// If nil, the Server's routing engine is used (real provider-backed).
	// Set via WithEvalEngine for testing or mock scenarios.
	evalEngine Engine
	// orgStateMu guards the lazily-opened P3-A org store (org_api.go).
	// It lives on THIS Server instance (not a package global) so two
	// Servers with different data dirs are isolated.
	orgStateMu sync.Mutex
	orgState   *orgState
	// dedupStore is the cross-turn session-dedup store (B4-23):
	// process memory, created eagerly in New — Seen/Mark are
	// mutex-safe, and an empty store is exactly "no session
	// yet". A nil store (a hand-built &Server{} in tests)
	// skips dedup, never an error.
	dedupStore *compress.SessionDedupStore
	// ccrStore is the lazy CCR retrieval-marker store (B4-24):
	// opened on first use from <cfg.DataDir>/ccr.db (SQLite,
	// 24 h retention; see compress.OpenCCRStore). ccrMu guards
	// ccrStore/ccrErr; a failed open is remembered so a corrupt
	// state skips CCR instead of retrying every request. A nil
	// store — no data dir, or a failed open — skips the CCR
	// engine, never an error, matching the engine contract.
	ccrMu    sync.Mutex
	ccrStore *compress.CCRStore
	ccrErr   error
	// autoOverrides is the lazy per-key candidate-exclusion store
	// (B2-15, autocombo_route.go): operator-managed JSON in
	// cfg.DataDir (in-memory empty when cfg is nil or has no data
	// dir). autoOverridesMu guards its lazy creation.
	autoOverridesMu        sync.Mutex
	autoOverrides          *autoCandidateOverrideStore
	registry               ModelRegistry
	dashboardProviders     DashboardProviderLister // nil = no provider feed
	providerAdder          ProviderAdder           // nil = POST /api/providers answers 503; set by WithProviderRegistry
	providerAddedHook      ProviderAddedHook       // nil = added providers do not route until restart (honest routing_live=false)
	providerRemover        ProviderRemover         // nil = DELETE /api/providers/{id} answers 503; set by WithProviderRegistry
	providerRemovedHook    ProviderRemovedHook     // nil = removed providers stay live in the engine until restart (honest live_removed=false)
	providerKeyManager     ProviderKeyManager      // nil = provider-key endpoints answer 503; set by WithProviderRegistry
	providerKeyChangedHook ProviderKeyChangedHook  // nil = key changes do not route until restart (honest routing_live=false)
	// capabilitySource serves the P1.5 capability matrix endpoints
	// (provider_capabilities.go). Nil = both answer 503; set by
	// WithCapabilityRegistry.
	capabilitySource capabilitiesSource
	// providerLogin drives provider account login flows (Phase 1,
	// provider_login.go): OpenRouter OAuth PKCE today. Created in
	// New; WithProviderLoginFlow overrides it for tests.
	providerLogin *providerlogin.OpenRouterFlow
	// shortlist is the operator's curated model shortlist (config
	// `shortlist`, CHECKLIST 199-200): entries pin models to the
	// top of GET /v1/models and GET /dashboard/models, marked
	// shortlisted. Nil = no shortlist. Set by WithShortlist.
	shortlist []string
	// modelRefresher re-probes provider model lists live (see
	// provider.Registry.RefreshModels). Nil = POST
	// /dashboard/models/refresh answers 503; set by
	// WithProviderRegistry (the same registry) or WithModelRefresher.
	modelRefresher      modelRefresher
	benchmarkFile       string // "" = GET /dashboard/benchmark answers recorded=false; set by WithBenchmarkFile
	benchmarkHistoryDir string // "" = GET /dashboard/benchmark/history answers recorded=false; set by WithBenchmarkHistoryDir
	streamingFile       string // "" = GET /dashboard/streaming answers recorded=false; set by WithStreamingFile
	// responsesFile (CHECKLIST 315-316): "" = Responses
	// transcripts live in memory only (tests, servers without a
	// data dir); set by WithResponsesFile to
	// <data-dir>/responses.json, the file the store persists to
	// on every mutation and restores from at startup.
	// responsesRestored is how many records the last restore
	// loaded (the dashboard feed reports it, so a restart is
	// visible). responsesPersistMu serialises snapshot writes:
	// two concurrent creates must not let the older snapshot
	// win the rename and drop the newer record.
	responsesFile      string
	responsesRestored  int
	responsesPersistMu sync.Mutex
	// sseBatching is the hybrid SSE batching switch (items
	// 135-136): flush at most every 8 ms or every 3 tokens.
	// Enabled by default; WithSSEBatching(false) turns it off
	// (measurement only — scripts/bench-sse.sh).
	sseBatching sseBatchConfig
	// headless (CHECKLIST 197-198): serve --headless runs the
	// gateway as a pure API process — no dashboard server. When
	// true, register() answers every /dashboard* route with 404
	// (see guard.go) and GET /dashboard/headless is unreachable
	// for the same reason; the feed below reports headless=false
	// on a normal (dashboard) server so Settings can say so
	// honestly. The flag is set only by WithHeadless, only from
	// serve --headless.
	headless       bool
	healthStatuses HealthStatusLister // nil = health unknown for all providers
	poolStats      poolStatsLister    // nil = GET /dashboard/pools answers an empty pool list with the default sizing
	autoRouter     *autorouter.Router // nil = auto-routing off; "auto/*" then fails honestly
	feedback       *feedback.Store    // nil = no feedback store; /dashboard/feedback answers 503
	// learned is the learned auto-router's refresher (nil = no
	// feedback store or no auto-router; GET /dashboard/learned
	// answers 503). It trains from the feedback store's recorded
	// (scores, tier, outcome) pairs and feeds the router's
	// learned-win-probability predictor — bounded, and only
	// while active (see internal/learned).
	learned *learned.Refresher
	// learnedEnabled is the operator flag (config
	// auto_router.learned_router, default OFF) applied to the
	// refresher at New time.
	learnedEnabled bool
	// codeOutcomes is the auto/code mode's live per-model
	// coding-task evidence (items 247–248, persisted at startup
	// by item 265): the tally of recorded accept / edit / discard
	// / regenerate signals on decisions served to the coding tier,
	// fed from the feedback POST handler and rebuilt from the
	// persisted feedback store at startup (see New), so a model
	// keeps its measured solve rate across restarts. Always
	// non-nil on a constructed server, so the evidence feed can
	// answer an honest empty state instead of a 503.
	codeOutcomes *autorouter.OutcomeStore
	// codeOutcomesPersisted reports whether the OutcomeStore was
	// rebuilt from the feedback store at startup (item 265):
	// true means the evidence feed's rates survive a restart,
	// false (no feedback store, or the restore failed) means the
	// tally is this run only. Set once in New, read by the
	// evidence feed.
	codeOutcomesPersisted bool
	priceBook             *config.PriceBook // nil = no operator prices configured; every model unknown
	// webhooks is the outbound webhook dispatcher (M5.9). Nil =
	// no webhooks configured: schedule blocks are refused
	// without any delivery. Set by WithWebhooks.
	webhooks *webhooks.Dispatcher
	// modelsDevStatus, when set, reports the opt-in models.dev
	// sync state (CHECKLIST 179) for the dashboard feeds: whether
	// the sync is on, when it last tried/succeeded, the last
	// error, and the synced entries (price/context provenance per
	// pair). Nil = no sync configured; the feeds omit the block.
	modelsDevStatus func() modelsync.Status
	// ledgerPrices is the ledger's per-token price map (the same
	// one the async writer charges with, including
	// provider-reported prices): the per-key budget's "is this
	// model priced?" question consults it alongside the book, so
	// the gate and the ledger can never disagree. Nil = no
	// ledger prices; the book alone decides. The map reference
	// is replaced wholesale by SetLedgerPrices when the opt-in
	// models.dev sync changes a price (CHECKLIST 179), so reads
	// go through getLedgerPrices under ledgerPricesMu — never
	// the field directly.
	ledgerPrices   ledger.Prices
	ledgerPricesMu sync.RWMutex
	guardrails     *guardrails.Store // never nil after New: log redaction is always on
	// requestRewrite (B3-20, Stage 1): when true, applyGuardrails
	// rewrites the request in place — PII/secrets the check found
	// are replaced by placeholders before the request reaches
	// the provider (the reference router's modifiedPayload). Opt-in; off by
	// default, so with blocking off a secret still goes to the
	// provider verbatim exactly as before. Set only by
	// WithRequestRewrite.
	requestRewrite bool
	// reversibleVault (B3-19, parity tracker:782): the reversible PII
	// token vault. When non-nil, the request rewrite mints vault
	// tokens for PII matches instead of dead placeholders, and
	// non-streaming responses have their tokens restored at exit.
	// Nil = the vault is off and the rewrite uses plain
	// placeholders exactly as before. Set only by
	// WithReversibleVault, which main.go calls only when the
	// config flag guardrails.reversible_vault is on.
	reversibleVault *guardrails.Vault
	// responseSanitization (B3-21/B3-22, Stage 1): when true, the
	// response guardrail runs in the store's mode (redact/warn/
	// block, default redact, the reference router's PII_RESPONSE_SANITIZATION)
	// — non-streaming responses are redacted in place before
	// delivery, and streaming responses run through the
	// store-wired ResponseStreamRedactor, with tool-call
	// arguments accumulated per call and checked once complete.
	// Off by default: responses keep the existing
	// block-or-deliver behaviour. Set only by
	// WithResponseSanitization; the mode itself is the store's,
	// set by WithResponseGuardMode.
	responseSanitization bool
	// plugins is the in-process plugin registry (CHECKLIST 201):
	// pre-request hooks run after the built-in guardrails,
	// post-response hooks run on delivered responses. Nil = no
	// plugins configured; the request path is exactly unchanged
	// and GET /dashboard/plugins reports zero plugins.
	plugins        *plugins.Registry
	routingPersist RoutingPersistFunc // nil = strategy updates apply at runtime only, never saved
	// configPersist saves the config file after a danger-zone
	// action mutates it (production: cfg.Save(path)). Nil means
	// the danger endpoints answer 503 instead of pretending to
	// act. dangerResetHook, when wired, drops the providers from
	// the in-memory registry after a config reset is saved; nil
	// means they stay loaded until restart (reported honestly).
	// dangerMu serialises the two danger actions against each other.
	configPersist   func() error
	dangerResetHook func() error
	dangerMu        sync.Mutex
	// combos is the shared M8.14 combos/chains store: one
	// instance for all five /v1/combos routes, built lazily
	// over the same *config.Config pointer the engine closures
	// capture, persisted via configPersist. combosMu guards
	// the lazy construction only; the store has its own mu.
	combos   *ComboStore
	combosMu sync.Mutex
	// Local TCP tunnel (see tunnel.go): one tunnel at a time, owned
	// by this manager; never nil after New.
	tunnel  *tunnelManager
	models  []string
	timeout time.Duration
	// mediaTimeouts is the per-endpoint media timeout override
	// (M8.20 F3), keyed by mediaSpec.name ("search" | "ocr" |
	// "video" | "music"). Set by WithMediaTimeouts; a positive
	// entry wins over the config value and the defaults in
	// mediaTimeout. Nil (the default) means no override.
	mediaTimeouts map[string]time.Duration
	logger        *log.Logger
	// apiLogRing is this server's /api/logs capture ring (M8.16).
	// Nil = no capture attached and /api/logs answers
	// captured:false. Set once by WithAPILogCapture; attaching
	// twice keeps the first ring. Stored on the Server, not in a
	// package-level pointer-keyed map: the old sync.Map keyed by
	// *Server was never cleaned, so a freed server's address could
	// be reused by a new server (TestM816LogsSince flake) which
	// then silently inherited a stale ring and never tee'd its own
	// logger to the new one.
	apiLogRing *APILogRing
	recorder   *metrics.Recorder // nil = metrics disabled
	// obsCollector / obsLogger (M2.3/M2.4 telemetry): the
	// per-request observation collector and the structured
	// request logger fed from the A8 choke points (see
	// telemetry.go). Nil = telemetry off, never a panic.
	// Constructed in New (collector + os.Stderr logger);
	// overridden in tests via WithTelemetryCollector /
	// WithTelemetryLogger.
	obsCollector *observability.Collector
	obsLogger    *observability.Logger
	// tracer records one root span per request when built with
	// -tags otel (Phase 2 observability); no-op in the default build.
	// Overridden in tests via WithTracer.
	tracer *observability.Tracer
	ledger *ledger.Ledger // nil = no request history yet; feeds answer empty
	// Responses store (CHECKLIST 169-170): the served Responses
	// objects (transcript + served object) keyed by gateway
	// response id, for retrieve + continuation. Never nil after
	// New; in-memory only, so a restart loses stored responses
	// (a later continuation then answers 404, never a wrong
	// replay). Set by WithResponsesStore in tests.
	responses *responses.Store
	// Per-request budgets (CHECKLIST 155-156): in-memory
	// recorder for budget refusals + overages, served by GET
	// /dashboard/request-budgets. Never nil after New; the
	// ledger stays the billing record.
	reqBudgets *requestBudgetStore
	// Per-key traffic limits + key status (CHECKLIST 195-196):
	// traffic is the in-memory sliding-window limiter (never nil
	// after New; counters live since server start, not across
	// restarts). keyMu serialises the key-mutation endpoints
	// (update + rotate) against each other, so two concurrent
	// rotations can never issue from the same stale key list —
	// and it guards the limiter's lazy backstop. It does NOT
	// guard reads: auth and the traffic check read s.cfg live.
	traffic *keyTrafficLimiter
	keyMu   sync.Mutex
	// keySessions is the in-memory concurrent-session tracker
	// (M5.10, maxSessions): never nil after New; counts live
	// since server start, not across restarts.
	keySessions *keySessionTracker
	// DB wipe (see dbwipe.go): healthHistory clears the probe-history
	// ring + files; ledgerFlushHook drains the async ledger writer
	// before the wipe so queued rows cannot resurrect afterwards;
	// wipeTokens holds SHA-256 hashes of outstanding single-use wipe
	// confirmation tokens (never the tokens themselves).
	// MCP selector mode (see internal/mcptools): the registry of
	// operator-configured MCP servers. Nil = no MCP servers are
	// configured — requests flow exactly as before, with no tools
	// injected and no agent loop. Set by WithMCPTools.
	mcpTools *mcptools.Registry
	// mcpAudit is the append-only MCP tool-call audit log (see
	// internal/mcp/audit.go). Nil = the audit feed answers
	// available:false with an empty list — an honest empty state,
	// not an error. Set by WithMCPAuditLog; production stdio
	// wires the same log the MCP server records into, so the
	// dashboard reads exactly what the server wrote.
	mcpAudit *mcp.AuditLog
	// mcpTransportMu guards the MCP remote-transport toggles
	// below (M9.7 dashboard; the transports themselves are
	// M9.2's). Both default OFF (fail-closed: a remote transport
	// is only reachable once the operator enables it here AND
	// M9.2 serves it). Session counts are live: mcpTransportStatus
	// reads them from the transport state (mcpHTTPState.counts in
	// mcp_http.go), so GET /api/mcp/status reports real sessions.
	mcpTransportMu   sync.Mutex
	mcpSSEEnabled    bool
	mcpStreamEnabled bool
	healthHistory    HealthHistoryClearer
	ledgerFlushHook  func()
	wipeMu           sync.Mutex
	wipeTokens       map[[32]byte]time.Time
	handler          http.Handler
	// Route-guard state (see guard.go). authRelaxed relaxes Tier 3
	// only — Tier 2 (ALWAYS_PROTECTED) never honours it, and Tier 1
	// (LOCAL_ONLY) always checks loopback first.
	authRelaxed bool
	listenAddr  string
	// version is the release version reported by GET /openapi.json.
	// Set by WithVersion; "" means the handler reports "0.1.0".
	version string
	// relayResolver resolves a model's single provider for the
	// byte-for-byte native relay (POST /v1/messages) and for the
	// readiness gate. Nil = no relay target can be resolved: every
	// /v1/messages request takes the translate path.
	relayResolver relayResolver
	// readinessGate holds the gateway-provided local capabilities
	// (see internal/readiness). Nil = no local capability is
	// registered, so nothing is readiness-gated.
	readinessGate *readiness.Gate
	// cacheServing is whether the engine serves requests proactively
	// from the gateway cache (production serve mode). The relay
	// decision needs it: only a request the cache could serve is
	// forced through the canonical form on the cache's account.
	cacheServing bool
	// exactCache / semanticCache are the gateway caches the native
	// relay consults (same instances the engine wraps, so a key
	// stored by either dialect hits for the other). Nil = the relay
	// never serves or stores cache entries; the engine path is
	// unaffected. They are interfaces (M6.2b) so serve mode can
	// pass the persistent wrappers and the relay writes through
	// to disk; the in-memory caches satisfy them too.
	exactCache       cache.ExactStore
	semanticCache    cache.SemanticStore
	bootstrapPending atomic.Bool
	bootstrapIssuer  BootstrapIssuer
	// bootstrapToken is the one-shot first-boot token (AK-2):
	// in-memory only, minted by resetBootstrapToken while
	// bootstrap is pending, spent by handleBootstrap. Never
	// persisted, never logged, never in a response. bootstrapMu
	// guards the check-issue-consume sequence (and the token
	// itself); no other mutex is taken under it.
	bootstrapToken string
	bootstrapMu    sync.Mutex
	shutdownHook   func()
	restartHook    func()
	// updateCheck backs POST /admin/update (nil = 503, see
	// admin_update.go). webFetchEgress / webFetchTLS are nil in
	// production (real DNS, system roots); tests inject them.
	updateCheck    UpdateCheckFunc
	updateCurrent  string
	webFetchEgress *egressGuard
	webFetchTLS    *tls.Config
	// webhookClient is nil in production (webhooks.New then uses the
	// SSRF-safe egress client); tests set it for loopback receivers.
	webhookClient    *http.Client
	startedAt        time.Time
	localOnlyDenials atomic.Uint64
	// CHECKLIST 243 server-side counters (see failover_243.go):
	// pre-body admission refusals (g), stripped dangling tool
	// calls (b), session-token admission refusals (f), and SSE
	// stream errors that named their deployment (h). Counts only.
	fail243EarlyRefusals           atomic.Uint64
	fail243StrippedToolCalls       atomic.Uint64
	fail243SessionAdmissionRefused atomic.Uint64
	fail243StreamErrorsNamed       atomic.Uint64
	// scopedGuardrailEvals counts request-stage guardrail
	// evaluations that ran under a per-key scope (CHECKLIST 277
	// (d)): a key with a non-empty guardrails list. Counts only.
	scopedGuardrailEvals atomic.Uint64
	// responsesSessionTokenCap is the per-session token cap for
	// Responses continuations (CHECKLIST 243, LiteLLM #50730):
	// a session whose counted tokens plus the next turn's prompt
	// estimate would exceed it refuses the turn with 429. 0 =
	// no cap: turns are counted and reported, never refused.
	// Set by WithResponsesSessionTokenCap.
	responsesSessionTokenCap int
	// A2A (M9.8, see internal/server/a2a.go): the lazily-opened
	// task service plus the in-flight processor cancels. a2aSvc
	// is nil until the first enabled /a2a call opens it — a
	// disabled endpoint never creates <data-dir>/a2a/tasks.db.
	// a2aMu guards all of these fields (and the lazy open).
	// a2aPruneLoop is the retention prune loop a2aService starts
	// exactly once alongside the store open (see a2a.go);
	// a2aPruneCancel cancels its context, and a2aStopPruneLoop
	// stops it. a2aPruneOpts overrides the prune options
	// (retention, cap, sweep interval) — nil means the design
	// defaults; set only by tests to shorten the sweep interval,
	// production leaves it nil (the interval is not an operator
	// knob — see a2a_prune.go).
	a2aMu          sync.Mutex
	a2aSvc         *a2a.Service
	a2aCancels     map[string]context.CancelFunc
	a2aPruneLoop   *A2APruneLoop
	a2aPruneCancel context.CancelFunc
	a2aPruneOpts   *A2APruneOptions
	// auditStore is the append-only request-level compliance audit
	// store (nil = audit logging off). Set by WithAuditStore.
	// auditMu guards it so Close() can release the handle while a
	// request in flight still reads it (Record on a closed store is
	// a logged no-op, never a failed request).
	auditMu    sync.Mutex
	auditStore *auditStore
	// wsAllowedOrigins lists the browser origins allowed to open a
	// WebSocket connection, in addition to the same-origin default
	// (see wsOriginAllowed). Set by WithWSAllowedOrigins; empty
	// means same-origin only.
	wsAllowedOrigins []string
	// bgWorkers is a registry of long-lived background workers (health
	// watcher, eval scheduler, A2A prune loop, etc.) keyed by name, with
	// their cancel functions. All workers started after P2-BG-003 are
	// registered here so Server.Close can stop them all at once instead
	// of relying on every caller to invoke a matching Stop/Close.
	// P2-BG-003: previously each goroutine was started and stopped ad hoc,
	// so a missed Stop call leaked a goroutine until process exit.
	bgWorkers   map[string]context.CancelFunc
	bgWorkersMu sync.Mutex
}

// Option configures a Server.
type Option func(*Server)

// WithKeyVerifier sets the gateway-key verifier.
func WithKeyVerifier(v KeyVerifier) Option {
	return func(s *Server) { s.verifier = v }
}

// WithConfig sets the verifier from a loaded *config.Config (production
// path — see NewConfigVerifier). The config is also kept for the Tier 2
// settings export (redacted snapshot) — it is read live, so keys issued
// after startup (e.g. via bootstrap) are visible immediately.
func WithConfig(cfg *config.Config) Option {
	return func(s *Server) {
		s.verifier = NewConfigVerifier(cfg)
		s.cfg = cfg
	}
}

// WithStaticKeys configures the in-memory key list (tests / fallback
// only — see StaticKeyVerifier).
func WithStaticKeys(keys ...string) Option {
	return func(s *Server) { s.verifier = NewStaticKeyVerifier(keys...) }
}

// WithRegistry sets the model registry used by GET /v1/models.
func WithRegistry(r ModelRegistry) Option {
	return func(s *Server) { s.registry = r }
}

// WithProviderRegistry sets the model registry from a *provider.Registry
// (adapted via AdaptProviderRegistry). It also wires the same registry
// as the dashboard provider feed, so GET /dashboard/providers reports
// the live providers.
func WithProviderRegistry(r *provider.Registry) Option {
	return func(s *Server) {
		s.registry = AdaptProviderRegistry(r)
		s.dashboardProviders = providerRegistryAdapter{reg: r}
		// The same registry backs POST /api/providers: Add/AddCatalog
		// mutate this live registry and persist through its Store,
		// so a provider added via the API routes immediately with
		// no restart (see providers_add.go). It also backs DELETE
		// /api/providers/{id}: Remove deletes from the live
		// registry and persists (see providers_delete.go). And it
		// backs the native relay's target resolution (see relay.go):
		// the adapter resolves a model to its single provider.
		s.providerAdder = r
		s.providerRemover = r
		s.providerKeyManager = r
		s.relayResolver = providerRegistryAdapter{reg: r}
		// The same registry backs the live model-list refresh
		// (POST /dashboard/models/refresh, CHECKLIST 200):
		// re-probing each provider's ListModels and persisting
		// the result.
		s.modelRefresher = r
		// The same registry backs the warm-pool feed
		// (GET /dashboard/pools): live per-(provider, key) pool
		// state plus the sizing config in force.
		s.poolStats = r
		// A3 item 6: wire the ToS risk-acknowledgement store into
		// the registry's gate, so the add-time, config-load and
		// request-time checks pass only for providers whose risk
		// was acknowledged via POST
		// /api/providers/{id}/risk-acknowledgement (tos_gate.go).
		r.SetToSAcknowledgement(s.ToSAcknowledged)
	}
}

// poolStatsLister reports the live warm-pool state. *provider.Registry
// satisfies it (see provider.Registry.PoolStats / PoolConfig).
type poolStatsLister interface {
	PoolStats() []provider.PoolStat
	PoolConfig() provider.PoolConfig
}

// WithPoolStats wires the warm-pool lister used by
// GET /dashboard/pools. It is separate from WithProviderRegistry so
// tests can feed the pools dashboard without a full registry.
func WithPoolStats(l poolStatsLister) Option {
	return func(s *Server) { s.poolStats = l }
}

// DashboardProviderLister lists the configured providers for the
// dashboard feed. Implementations must return live data from the
// registry, never a hard-coded list; *provider.Registry satisfies it
// (see provider.Registry.DashboardProviders).
type DashboardProviderLister interface {
	DashboardProviders() []provider.DashboardProvider
}

// WithDashboardProviders sets the provider lister used by
// GET /dashboard/providers and GET /api/providers. It is separate from
// WithRegistry so tests can feed the dashboard without a full provider
// registry.
func WithDashboardProviders(l DashboardProviderLister) Option {
	return func(s *Server) { s.dashboardProviders = l }
}

// HealthStatusLister reports the most recent health-check result per
// provider. *health.Checker satisfies it via Last; the interface keeps
// the server decoupled from the checker's probe machinery. When the
// wired lister also implements History (see historyLister),
// GET /api/providers additionally reports each provider's recorded
// probe history.
type HealthStatusLister interface {
	Last() []health.Status
}

// WithHealthChecker wires the last health-check results into
// GET /api/providers (health_status per provider). With no checker the
// endpoint still works and reports every provider as "unknown".
func WithHealthChecker(l HealthStatusLister) Option {
	return func(s *Server) { s.healthStatuses = l }
}

// WithAutoRouter wires the local auto-router into the server: its
// virtual models ("auto", "auto/fast", ...) are listed by
// GET /v1/models, its decisions are reported on auto-routed
// responses (X-Codvian-Auto-* headers), and the dashboard's
// Auto-router section classifies prompts through
// GET /dashboard/auto-route. With no router, "auto/*" requests fail
// honestly (the engine has nothing to resolve them with) and the
// dashboard endpoints answer 503.
func WithAutoRouter(r *autorouter.Router) Option {
	return func(s *Server) { s.autoRouter = r }
}

// WithPriceBook wires the operator-configured price source into the
// server: GET /dashboard/prices lists it, GET /dashboard/strategies
// shows each chain's real cost, and the auto-route endpoints report
// the resolved model's cost. With no book every model is price
// unknown — the feeds say so, never $0.
func WithPriceBook(b *config.PriceBook) Option {
	return func(s *Server) { s.priceBook = b }
}

// WithWebhooks wires the outbound webhook dispatcher (M5.9)
// into the server: quota-schedule blocks fire the
// "quota.exceeded" event through it, best-effort. A nil
// dispatcher means no webhooks are configured.
func WithWebhooks(d *webhooks.Dispatcher) Option {
	return func(s *Server) { s.webhooks = d }
}

// WithModelsDevStatus wires the opt-in models.dev sync state
// (CHECKLIST 179) into the server: GET /dashboard/models and
// GET /dashboard/prices carry the sync status (enabled, last
// attempt/success, last error, stale flag) and the per-pair
// provenance (which prices/windows came from models.dev, and
// when). With no func wired the feeds omit the block entirely —
// the sync was never configured, not "unknown".
func WithModelsDevStatus(f func() modelsync.Status) Option {
	return func(s *Server) { s.modelsDevStatus = f }
}

// SetLedgerPrices replaces the ledger's per-token price map the
// server consults (the per-key budget's "is this model priced?"
// question). It is used by the opt-in models.dev sync when a
// periodic re-sync changes a price; the map is the same object
// the async ledger writer was given via its own SetPrices, so
// the gate and the ledger can never disagree about whether a
// model is priced. A nil map means "no ledger prices".
func (s *Server) SetLedgerPrices(p ledger.Prices) {
	s.ledgerPricesMu.Lock()
	s.ledgerPrices = p
	s.ledgerPricesMu.Unlock()
}

// getLedgerPrices returns the ledger price map in force. The map
// itself is never mutated in place — SetLedgerPrices replaces the
// reference — so the snapshot stays valid for the whole call.
func (s *Server) getLedgerPrices() ledger.Prices {
	s.ledgerPricesMu.RLock()
	defer s.ledgerPricesMu.RUnlock()
	return s.ledgerPrices
}

// modelsDevSnapshot returns the current models.dev sync state, or
// nil when no sync is wired. Callers take one snapshot per feed
// and derive every provenance field from it, so one response
// never mixes two sync states.
func (s *Server) modelsDevSnapshot() *modelsync.Status {
	if s.modelsDevStatus == nil {
		return nil
	}
	st := s.modelsDevStatus()
	return &st
}

// newModelsDevStatusJSON renders a sync snapshot for the dashboard
// NOTE (CHECKLIST 177 slice, minimal fix for a concurrent-slice
// build break): this constructor was named modelsDevStatusJSON,
// the same name as the struct type below — Go forbids a func and
// a type sharing a name in one package, so the build failed.
// Renamed the func only; the type and the call site are unchanged.
// feeds: enabled, last attempt/success, last error, and whether
// the served data is stale (last attempt failed but an earlier
// sync succeeded — the prices/windows are the last good data,
// stale since LastSuccessAt). Before the first success there is
// no data at all, so Stale is never true then.
func newModelsDevStatusJSON(st *modelsync.Status) *modelsDevStatusJSON {
	if st == nil {
		return nil
	}
	out := &modelsDevStatusJSON{
		Enabled:       st.Enabled,
		URL:           st.URL,
		LastAttemptAt: st.LastAttemptAt,
		LastSuccessAt: st.LastSuccessAt,
		LastError:     st.LastError,
		Counts:        st.Counts,
	}
	if st.LastError != "" && st.LastSuccessAt != nil {
		out.Stale = true
	}
	for _, e := range st.Entries {
		if e.Price != nil {
			out.PricesSynced++
		}
		if e.ContextWindow > 0 {
			out.WindowsSynced++
		}
	}
	return out
}

// modelsDevStatusJSON is the models.dev sync status block carried
// by GET /dashboard/models and GET /dashboard/prices (CHECKLIST
// 179). Absent (nil) when no sync is configured.
type modelsDevStatusJSON struct {
	Enabled       bool             `json:"enabled"`
	URL           string           `json:"url"`
	LastAttemptAt *time.Time       `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time       `json:"last_success_at,omitempty"`
	LastError     string           `json:"last_error,omitempty"`
	Stale         bool             `json:"stale"`
	PricesSynced  int              `json:"prices_synced"`
	WindowsSynced int              `json:"windows_synced"`
	Counts        modelsync.Counts `json:"counts"`
}

// WithMCPTools wires the MCP selector-mode registry into the
// server. With no registry (nil) or a registry with no servers
// configured, chat completions flow exactly as before: no tools
// are injected and no agent loop runs. With servers configured,
// non-streaming chat completions run through the registry's agent
// loop (in-process follow-up model calls, never a second HTTP
// request); streaming requests are passed through unchanged — the
// loop is non-streaming only, documented in docs/mcp-selector.md.
func WithMCPTools(r *mcptools.Registry) Option {
	return func(s *Server) { s.mcpTools = r }
}

// WithMCPAuditLog wires the MCP tool-call audit log into the
// server so GET /api/mcp/audit can read it (M9.7). With no log
// the audit feed answers available:false with an empty list —
// the server still serves, the audit view just stays empty.
// Production wires the same *mcp.AuditLog the stdio server
// records into, so the dashboard reads exactly what was logged
// (inputs hashed, outputs truncated — never raw arguments).
func WithMCPAuditLog(l *mcp.AuditLog) Option {
	return func(s *Server) { s.mcpAudit = l }
}

// WithFeedback wires the feedback signal store into the server: its
// signals feed GET/POST /dashboard/feedback, and — when the store
// is learning-enabled — its learned weights are applied to the
// auto-router. With no store the feedback endpoints answer 503 and
// routing is exactly unchanged. The wiring to the auto-router
// happens in New (after all options), so option order does not
// matter.
func WithFeedback(f *feedback.Store) Option {
	return func(s *Server) { s.feedback = f }
}

// WithResponsesStore replaces the Responses store (CHECKLIST
// 169-170). New() already creates one, so this is only for tests
// that need a store with a shrunk TTL/cap.
func WithResponsesStore(st *responses.Store) Option {
	return func(s *Server) { s.responses = st }
}

// WithLearnedRouter sets the operator flag for the learned
// auto-router (config auto_router.learned_router, default OFF):
// when true AND the learned model is active on real evidence,
// its per-tier win probabilities compose into routing (bounded
// to +/-0.05); otherwise routing is exactly unchanged. The flag
// is applied to the refresher in New (after all options), so
// option order does not matter. With no feedback store or no
// auto-router there is no learned model at all and GET
// /dashboard/learned answers 503.
func WithLearnedRouter(enabled bool) Option {
	return func(s *Server) { s.learnedEnabled = enabled }
}

// learnedPredictorAdapter adapts a *learned.Refresher to the
// autorouter.LearnedPredictor interface (string-keyed scores in,
// tier-keyed probabilities out). It never blocks routing on a
// slow store: the refresher serves its cached model when fresh.
type learnedPredictorAdapter struct{ r *learned.Refresher }

func (a learnedPredictorAdapter) LearnedWinProbs(scores map[string]float32) (map[autorouter.Tier]float32, bool) {
	f := make(map[string]float64, len(scores))
	for k, v := range scores {
		f[k] = float64(v)
	}
	probs, active := a.r.Predict(context.Background(), f)
	out := make(map[autorouter.Tier]float32, len(probs))
	for k, v := range probs {
		out[autorouter.Tier(k)] = float32(v)
	}
	return out, active
}

// WithGuardrails wires the guardrail store into the server: its
// blocking config decides whether a matching request is refused, and
// its counters/events feed GET /dashboard/guardrails. With no store
// New builds a default one (blocking all off, log redaction always
// on), so a server can never run without the redaction stage.
func WithGuardrails(g *guardrails.Store) Option {
	return func(s *Server) {
		if g != nil {
			s.guardrails = g
		}
	}
}

// WithRequestRewrite enables request rewriting (B3-20, Stage 1):
// WithEvalEngine sets a custom engine for the scheduled evaluation
// scheduler. If not set, the Server's routing engine is used (real
// provider-backed evaluations). Use this for testing or to provide a
// mock engine for no-provider scenarios.
func WithEvalEngine(eng Engine) Option {
	return func(s *Server) { s.evalEngine = eng }
}

// WithRequestRewrite when true, applyGuardrails strips PII/secrets from the request
// before it reaches the provider. Opt-in (the reference router's
// modifiedPayload); off by default. Wiring only — main.go and
// tests choose when to turn it on.
func WithRequestRewrite(enabled bool) Option {
	return func(s *Server) { s.requestRewrite = enabled }
}

// WithReversibleVault wires the reversible PII token vault
// (B3-19, parity tracker:782): when set, the request rewrite mints
// vault tokens for PII matches instead of dead placeholders, and
// non-streaming responses have their tokens restored at exit. A
// nil vault (or no call) leaves the plain-placeholder rewrite
// exactly as before. Wiring only — main.go passes the vault only
// when the config flag guardrails.reversible_vault is on.
func WithReversibleVault(v *guardrails.Vault) Option {
	return func(s *Server) { s.reversibleVault = v }
}

// WithResponseSanitization enables response sanitization (B3-21/
// B3-22, Stage 1): when true, responses are sanitized in the
// store's mode — non-streaming in applyResponseGuardrails,
// streaming through the store-wired ResponseStreamRedactor in
// handleChatStream, with tool-call arguments accumulated per
// call and checked once complete. Opt-in (the reference router's
// PII_RESPONSE_SANITIZATION); off by default. Wiring only —
// main.go and tests choose when to turn it on.
func WithResponseSanitization(enabled bool) Option {
	return func(s *Server) { s.responseSanitization = enabled }
}

// WithResponseGuardMode sets the response-sanitization mode on the
// server's guardrail store (redact, warn or block; default redact).
// An invalid mode is ignored and the store keeps its current mode.
// The store is never nil after New, so this option is safe in any
// order relative to WithGuardrails.
func WithResponseGuardMode(mode guardrails.ResponseGuardMode) Option {
	return func(s *Server) {
		if s.guardrails != nil {
			_ = s.guardrails.SetResponseMode(mode)
		}
	}
}

// WithPlugins wires the in-process plugin registry into the server
// (CHECKLIST 201): its pre-request hooks run in
// handleChatCompletions after the built-in guardrails, and its
// post-response hooks run on delivered responses (non-streaming
// before delivery, streaming after the stream completes). With no
// registry the request path is exactly unchanged and GET
// /dashboard/plugins reports zero plugins.
func WithPlugins(r *plugins.Registry) Option {
	return func(s *Server) {
		if r != nil {
			s.plugins = r
		}
	}
}

// WithModels sets the configured model IDs returned by GET /v1/models
// when no registry is configured.
func WithModels(ids ...string) Option {
	return func(s *Server) { s.models = append([]string(nil), ids...) }
}

// WithTimeout overrides the per-request timeout (default 120s).
func WithTimeout(d time.Duration) Option {
	return func(s *Server) {
		if d > 0 {
			s.timeout = d
		}
	}
}

// WithMediaTimeouts overrides the per-endpoint media timeouts
// (M8.20 F3), keyed by media endpoint name ("search" | "ocr" |
// "video" | "music"). A positive entry wins over the
// routing.media_timeouts_ms config value and over the defaults
// in mediaTimeout; a zero or negative entry is ignored. The map
// is copied, so the caller keeps its own.
func WithMediaTimeouts(timeouts map[string]time.Duration) Option {
	return func(s *Server) {
		if len(timeouts) == 0 {
			return
		}
		if s.mediaTimeouts == nil {
			s.mediaTimeouts = make(map[string]time.Duration, len(timeouts))
		}
		for k, v := range timeouts {
			if v > 0 {
				s.mediaTimeouts[k] = v
			}
		}
	}
}

// WithLogger overrides the logger (default log.Default()).
func WithLogger(l *log.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithMetrics enables the /metrics endpoint and per-request recording
// (requests, latency, in-flight, failovers) with the given recorder.
// A nil recorder disables metrics: /metrics answers 503.
func WithMetrics(rec *metrics.Recorder) Option {
	return func(s *Server) { s.recorder = rec }
}

// WithLedger wires the usage ledger into the dashboard live feeds
// (GET /dashboard/overview, /dashboard/logs, /dashboard/usage and
// the per-model request counts on /dashboard/models). With no ledger
// the feeds still answer — with zero totals and empty lists, the
// honest "no requests yet" state — because a ledger may genuinely
// not exist yet on a fresh install. The ledger holds metadata only
// (provider, model, tokens, cost, latency, outcome); no prompt or
// response content is ever stored in it, so nothing the feeds return
// can leak message content.
func WithLedger(l *ledger.Ledger) Option {
	return func(s *Server) { s.ledger = l }
}

// WithHealthHistory wires the probe-history clearer used by POST
// /admin/db/wipe (health.Checker satisfies it). With none wired
// the wipe reports the probe history as not configured and wipes
// the other stores.
func WithHealthHistory(c HealthHistoryClearer) Option {
	return func(s *Server) { s.healthHistory = c }
}

// WithLedgerFlushHook wires the function POST /admin/db/wipe calls
// before deleting anything (production: the async ledger writer's
// Flush, so rows queued off the hot path cannot land after the
// wipe). With none wired the wipe proceeds without a flush —
// correct when the ledger is written synchronously.
func WithLedgerFlushHook(fn func()) Option {
	return func(s *Server) { s.ledgerFlushHook = fn }
}

// WithAuthRelaxed relaxes Tier 3 (MANAGEMENT) auth: Tier 3 routes
// answer without a gateway key. It never relaxes Tier 2
// (ALWAYS_PROTECTED — destructive routes always need a key) and never
// touches Tier 1 (LOCAL_ONLY — loopback is checked before auth
// regardless). Default false; production serve never sets it — it
// exists to model a future login-relaxed mode and for tests.
func WithAuthRelaxed(relaxed bool) Option {
	return func(s *Server) { s.authRelaxed = relaxed }
}

// WithListenAddr records the address the server listens on, reported
// verbatim by GET /dashboard/guard and GET /admin/status. Empty means
// "unknown" (honest, never guessed).
// WithVersion sets the release version reported by GET /openapi.json.
func WithVersion(v string) Option {
	return func(s *Server) { s.version = v }
}

func WithListenAddr(addr string) Option {
	return func(s *Server) { s.listenAddr = addr }
}

// WithBootstrapPending marks fresh-install bootstrap as pending (no
// gateway key exists yet) or complete. POST /admin/bootstrap only
// issues a key while pending.
func WithBootstrapPending(pending bool) Option {
	return func(s *Server) { s.bootstrapPending.Store(pending) }
}

// WithBootstrapIssuer wires the function that issues the first gateway
// key (production: issue + persist via the config file). With no
// issuer, POST /admin/bootstrap answers 503 while pending.
func WithBootstrapIssuer(issuer BootstrapIssuer) Option {
	return func(s *Server) { s.bootstrapIssuer = issuer }
}

// WithShutdownHook wires the function POST /admin/shutdown invokes
// after answering 202 (production: gracefully stop the HTTP server).
// With no hook the route still answers 202 and shuts nothing down —
// the response is honest about what happened only when a hook is set,
// so production must always set one.
func WithShutdownHook(hook func()) Option {
	return func(s *Server) { s.shutdownHook = hook }
}

// WithRestartHook wires the function POST /admin/restart invokes
// after answering 202 (production: gracefully restart the HTTP server).
func WithRestartHook(hook func()) Option {
	return func(s *Server) { s.restartHook = hook }
}

// RoutingUpdate is one POST /dashboard/strategies change: a new
// global strategy (nil = unchanged, "" = clear back to the
// configured default) and/or per-chain overrides ("" value =
// clear that chain's override so it inherits the global again).
// It also carries the cooldown-mode change of POST
// /dashboard/cooldown (nil CooldownMode = unchanged, "" = clear
// back to the configured default; "" per-chain value = clear that
// chain's override): the same persist function applies both, so a
// dashboard cooldown change is saved to the config file exactly
// like a strategy change.
type RoutingUpdate struct {
	Global             *string           `json:"global_strategy"`
	Chains             map[string]string `json:"chains"`
	CooldownMode       *string           `json:"cooldown_mode,omitempty"`
	ChainCooldownModes map[string]string `json:"chain_cooldown_modes,omitempty"`
}

// RoutingPersistFunc persists a routing update to the config file
// (production: mutate cfg.Routing and Save). It runs BEFORE the
// runtime is touched, so a persist failure leaves routing
// unchanged and the handler can answer 500 honestly. With no
// persist function wired, updates apply at runtime only and the
// feed reports persisted=false.
type RoutingPersistFunc func(update RoutingUpdate) error

// WithRoutingPersist wires the function that saves a routing update
// to the config file. Without it, POST /dashboard/strategies still
// works but only for this run — a restart restores the config
// file's values, and the feed says so (persisted=false).
func WithRoutingPersist(fn RoutingPersistFunc) Option {
	return func(s *Server) { s.routingPersist = fn }
}

// WithConfigPersist wires the function that saves the config file
// after a danger-zone action (POST /admin/keys/revoke-all,
// POST /admin/config/reset) mutates it in place. Without it those
// endpoints answer 503 instead of pretending to act.
func WithConfigPersist(fn func() error) Option {
	return func(s *Server) { s.configPersist = fn }
}

// WithDangerResetHook wires the function that drops the providers
// from the in-memory registry after a config reset has been saved
// (production: remove every provider from the registry, which also
// re-saves the already-reset config). Without it, reset providers
// stay loaded in the running router until restart, and the reset
// response says so.
func WithDangerResetHook(fn func() error) Option {
	return func(s *Server) { s.dangerResetHook = fn }
}

// New builds a Server. engine must not be nil.
//
// Every route is registered through register(), which looks the route
// up in the guard-tier table (guard.go) and applies that tier's
// wrappers — LOCAL_ONLY first, then auth — so no route can land
// unclassified.
// combosStore returns the shared M8.14 combos store, building it
// once over s.cfg with s.configPersist as the persist callback.
// With no config it is an empty in-memory-only store; with no
// persist wired, mutations apply in-memory and report
// persisted=false, exactly like the strategies/cooldown feeds.
func (s *Server) combosStore() *ComboStore {
	s.combosMu.Lock()
	defer s.combosMu.Unlock()
	if s.combos == nil {
		s.combos = NewComboStore(s.cfg, s.configPersist)
	}
	return s.combos
}

// agentCardBaseURL returns the router's own public base URL for
// the A2A agent card: derived from s.listenAddr — the
// operator-configured listen address (config `listen`, default
// 127.0.0.1:20128; main.go passes it via WithListenAddr) — with
// the same ":port" → "127.0.0.1:port" expansion handleOpenAPI
// uses for its servers URL, and any trailing slash trimmed.
// There is NO configured public/external URL anywhere in config:
// the router is local-only by design (config validates Listen as
// loopback), so the listen address IS the address clients reach
// it on. It is never derived from a request: the Host header and
// X-Forwarded-For can be forged by a caller, and the card's url
// must name the real endpoint (the reference router derives its base URL
// from the request; we deliberately do not). Empty when no
// listen address is configured: the card route is then not
// registered at all (see New), rather than serving a card whose
// url names an address the router does not listen on.
func (s *Server) agentCardBaseURL() string {
	if s.listenAddr == "" {
		return ""
	}
	host := s.listenAddr
	if strings.HasPrefix(host, ":") {
		host = "127.0.0.1" + host
	}
	return "http://" + strings.TrimRight(host, "/")
}

func New(engine Engine, opts ...Option) *Server {
	s := &Server{engine: engine, timeout: DefaultTimeout, logger: log.Default(), startedAt: time.Now(), guardrails: guardrails.NewStore(), tunnel: newTunnelManager(), wipeTokens: map[[32]byte]time.Time{}, reqBudgets: newRequestBudgetStore(), responses: responses.New(), traffic: newKeyTrafficLimiter(), keySessions: newKeySessionTracker(), codeOutcomes: autorouter.NewOutcomeStore(), dedupStore: compress.NewSessionDedupStore(), providerLogin: providerlogin.NewOpenRouterFlow()}
	// M2 telemetry defaults (overridden by WithTelemetryCollector /
	// WithTelemetryLogger below): a live collector, and a JSON
	// request logger on os.Stderr — never a new file.
	s.obsCollector = observability.NewCollector()
	s.obsLogger = observability.NewLogger(os.Stderr)
	// Tracing default: the build-tagged tracer (no-op unless -tags
	// otel). main wires a configured one (OTLP endpoint) via
	// WithTracer when the operator sets one.
	s.tracer = observability.NewTracer()
	// Hybrid SSE batching (items 135-136) is on by default:
	// flush at most every 8 ms or every 3 tokens. Options
	// applied below may turn it off (WithSSEBatching(false),
	// the measurement switch).
	s.sseBatching = sseBatchConfig{enabled: true, maxTokens: sseBatchMaxTokens, maxDelay: sseBatchMaxDelay}
	for _, o := range opts {
		o(s)
	}
	// AK-2: mint the one-shot bootstrap token when bootstrap is
	// pending (fresh install, or flipped back by revoke-all /
	// config reset). main.go prints it to stdout at first boot.
	s.resetBootstrapToken()
	// Default-on log redaction (entries 150/152): every line this
	// logger writes is redacted first, whatever the call sites do.
	// There is no flag or env var that turns this off.
	s.logger = wrapRedactingLogger(s.logger)
	if s.guardrails == nil {
		s.guardrails = guardrails.NewStore()
	}
	// Async guardrail verdicts (CHECKLIST 185) feed /metrics as
	// phase "async" evaluations, the same way the sync Check feeds
	// phase "request"/"response"/"tool_args" — one row per verdict,
	// so the phase's rows sum to the async verdict count. The
	// hook runs in the verdict goroutine; the recorder is
	// mutex-safe, and a nil recorder means metrics are off.
	if s.recorder != nil {
		rec := s.recorder
		s.guardrails.SetVerdictHook(func(phase string, res guardrails.AsyncResult, outcome string) {
			rec.IncGuardrailEval(phase, outcome)
		})
	}
	// Feedback-learned weights (Phase 2 prototype): when both a
	// feedback store and an auto-router are wired, the store is
	// the router's weights provider — the same *Router the engine
	// resolves "auto/*" through, so learning applies to live
	// routing, not just to the dashboard view. A nil store, or a
	// store with learning off, leaves routing exactly unchanged.
	// Auto Code mode (items 247–248): the router's
	// cost-per-solved-task evidence is this server's
	// OutcomeStore, fed from the feedback POST handler below.
	// Wired whenever an auto-router exists (not only with a
	// feedback store — without one the store simply stays
	// empty and the mode ranks by price at the documented
	// neutral default).
	if s.autoRouter != nil {
		s.autoRouter.SetCodeOutcomeProvider(s.codeOutcomes)
	}
	// Auto Code persistence (item 265): when a feedback store is
	// wired, rebuild the OutcomeStore tally from the persisted
	// signals before serving, so a model keeps its measured solve
	// rate across restarts. A restore failure is logged loudly
	// and never fatal: the evidence starts empty (the documented
	// no-evidence default) instead of taking the server down,
	// and the feed reports persisted=false so the dashboard
	// says so honestly.
	if s.feedback != nil {
		if outcomes, err := s.feedback.CodeOutcomes(context.Background()); err != nil {
			s.logger.Printf("server: auto/code outcome restore failed (%v); evidence starts empty", err)
		} else {
			s.codeOutcomes.Restore(outcomes)
			s.codeOutcomesPersisted = true
		}
	}
	if s.autoRouter != nil && s.feedback != nil {
		s.autoRouter.SetWeightsProvider(s.feedback)
		// Learned auto-router (items 181-182, RouteLLM
		// pattern): the refresher trains from the feedback
		// store's recorded pairs and feeds the router's
		// learned-win-probability predictor. The predictor
		// reports inactive while the model is off / learning /
		// uncalibrated or the flag is off, so wiring it here
		// changes nothing until real evidence activates it.
		s.learned = learned.NewRefresher(s.feedback.TrainingPairs, s.learnedEnabled)
		s.autoRouter.SetLearnedPredictor(learnedPredictorAdapter{r: s.learned})
	}
	mux := http.NewServeMux()
	s.register(mux, "GET", "/healthz", s.handleHealthz)
	// AUD-CFG-004: readiness is a distinct endpoint from liveness —
	// /healthz says "process up", /readyz says "dependencies ready".
	// Both unauthenticated and loopback-safe like /healthz: neither
	// returns anything but a status word and check names.
	s.register(mux, "GET", "/readyz", s.handleReadyz)
	s.register(mux, "GET", "/dashboard", s.handleDashboard)
	s.register(mux, "POST", "/dashboard/auth/setup", s.handleDashAuthSetup)
	s.register(mux, "POST", "/dashboard/auth/login", s.handleDashAuthLogin)
	s.register(mux, "POST", "/dashboard/auth/logout", s.handleDashAuthLogout)
	s.register(mux, "POST", "/dashboard/auth/change", s.handleDashAuthChange)
	// PR #51 TOTP endpoints:
	s.register(mux, "GET", "/dashboard/auth/totp/status", s.handleTotpStatus)
	s.register(mux, "POST", "/dashboard/auth/totp/enroll", s.handleTotpEnroll)
	s.register(mux, "POST", "/dashboard/auth/totp/confirm", s.handleTotpConfirm)
	s.register(mux, "POST", "/dashboard/auth/totp/verify", s.handleTotpVerify)
	s.register(mux, "POST", "/dashboard/auth/totp/disable", s.handleTotpDisable)
	s.register(mux, "POST", "/dashboard/auth/totp/backup-codes", s.handleTotpBackupCodes)
	// PR #52 OAuth provider connections:
	s.register(mux, "GET", "/dashboard/oauth/connect/{provider}", s.handleOAuthConnect)
	s.register(mux, "GET", "/dashboard/oauth/callback/{provider}", s.handleOAuthCallback)
	s.register(mux, "GET", "/dashboard/oauth/connections", s.handleOAuthConnections)
	s.register(mux, "POST", "/dashboard/oauth/disconnect/{provider}", s.handleOAuthDisconnect)
	s.register(mux, "GET", "/dashboard/providers", s.handleDashboardProviders)
	s.register(mux, "GET", "/dashboard/overview", s.handleOverview)
	s.register(mux, "GET", "/dashboard/models", s.handleDashboardModels)
	s.register(mux, "GET", "/dashboard/catalog-audit", s.handleCatalogAudit)
	s.register(mux, "GET", "/dashboard/release-verify", s.handleReleaseVerify)
	s.register(mux, "POST", "/dashboard/models/refresh", s.handleModelsRefresh)
	s.register(mux, "GET", "/dashboard/logs", s.handleLogs)
	s.register(mux, "GET", "/dashboard/usage", s.handleUsage)
	s.register(mux, "GET", "/dashboard/keys", s.handleKeys)
	s.register(mux, "GET", "/dashboard/auto-route", s.handleAutoRoute)
	s.register(mux, "GET", "/dashboard/auto-route/last", s.handleAutoRouteLast)
	s.register(mux, "GET", "/dashboard/feedback", s.handleFeedback)
	s.register(mux, "POST", "/dashboard/feedback", s.handleFeedback)
	s.register(mux, "GET", "/dashboard/code-outcomes", s.handleCodeOutcomes)
	s.register(mux, "GET", "/dashboard/learned", s.handleLearned)
	s.register(mux, "GET", "/dashboard/intent-tiers", s.handleIntentTiers)
	s.register(mux, "POST", "/dashboard/intent-tiers", s.handleIntentTiersUpdate)
	s.register(mux, "GET", "/dashboard/suggestions", s.handleSuggestions)
	s.register(mux, "POST", "/dashboard/suggestions", s.handleSuggestionsAction)
	s.register(mux, "GET", "/dashboard/coalescing", s.handleCoalescing)
	s.register(mux, "GET", "/dashboard/reasoning", s.handleReasoning)
	s.register(mux, "GET", "/dashboard/concurrency", s.handleConcurrency)
	s.register(mux, "GET", "/dashboard/pools", s.handlePools)
	s.register(mux, "GET", "/dashboard/guard", s.handleGuard)
	s.register(mux, "GET", "/dashboard/audit", s.handleDashboardAudit)
	s.register(mux, "GET", "/dashboard/mcp", s.handleMCP)
	s.register(mux, "GET", "/api/mcp/audit", s.handleMCPAudit)
	s.register(mux, "GET", "/api/mcp/status", s.handleMCPStatus)
	s.register(mux, "POST", "/api/mcp/status", s.handleMCPStatus)
	// M9.2 MCP remote transports (SSE + Streamable HTTP).
	s.register(mux, "GET", "/api/mcp/sse", s.handleMCPSSE)
	s.register(mux, "POST", "/api/mcp/sse", s.handleMCPSSE)
	s.register(mux, "POST", "/api/mcp/stream", s.handleMCPStream)
	s.register(mux, "GET", "/api/mcp/stream", s.handleMCPStream)
	s.register(mux, "DELETE", "/api/mcp/stream/{id}", s.handleMCPStreamDelete)
	s.register(mux, "GET", "/dashboard/guardrails", s.handleGuardrails)
	s.register(mux, "POST", "/dashboard/guardrails", s.handleGuardrailsUpdate)
	s.register(mux, "GET", "/dashboard/plugins", s.handlePlugins)
	s.register(mux, "GET", "/dashboard/verification", s.handleVerification)
	s.register(mux, "GET", "/dashboard/strategies", s.handleStrategies)
	s.register(mux, "POST", "/dashboard/strategies", s.handleStrategiesUpdate)
	s.register(mux, "GET", "/dashboard/cooldown", s.handleCooldown)
	s.register(mux, "POST", "/dashboard/cooldown", s.handleCooldownUpdate)
	s.register(mux, "GET", "/dashboard/groups", s.handleGroups)
	s.register(mux, "GET", "/dashboard/stream-lifetime", s.handleStreamLifetime)
	s.register(mux, "GET", "/dashboard/key-budgets", s.handleKeyBudgets)
	s.register(mux, "POST", "/dashboard/key-budgets", s.handleKeyBudgetUpdate)
	s.register(mux, "GET", "/dashboard/quota-pools", s.handleQuotaPools)
	s.register(mux, "POST", "/dashboard/quota-pools", s.handleQuotaPoolUpdate)
	s.register(mux, "GET", "/dashboard/quota-schedules", s.handleQuotaSchedules)
	s.register(mux, "POST", "/dashboard/quota-schedules", s.handleQuotaScheduleUpdate)
	s.register(mux, "GET", "/dashboard/internal-budgets", s.handleInternalBudgets)
	s.register(mux, "POST", "/dashboard/internal-budgets", s.handleInternalBudgetUpdate)
	s.register(mux, "GET", "/dashboard/teams", s.handleTeams)
	s.register(mux, "POST", "/dashboard/teams", s.handleTeamUpdate)
	s.register(mux, "POST", "/dashboard/teams/{id}/members", s.handleTeamMembers)
	s.register(mux, "GET", "/dashboard/request-budgets", s.handleRequestBudgets)
	s.register(mux, "GET", "/dashboard/trace", s.handleTrace)
	s.register(mux, "GET", "/dashboard/context-fit", s.handleContextFit)
	s.register(mux, "GET", "/dashboard/prices", s.handlePrices)
	s.register(mux, "GET", "/dashboard/benchmark", s.handleDashboardBenchmark)
	s.register(mux, "GET", "/dashboard/benchmark/history", s.handleDashboardBenchmarkHistory)
	s.register(mux, "GET", "/dashboard/memstats", s.handleDashboardMemstats)
	s.register(mux, "GET", "/dashboard/streaming", s.handleDashboardStreaming)
	s.register(mux, "GET", "/openapi.json", s.handleOpenAPI)
	s.register(mux, "GET", "/api/providers", s.handleAPIProviders)
	s.register(mux, "POST", "/api/providers", s.handleAddProvider)
	// A3 item 6: record the operator's explicit acknowledgement of
	// the ToS risk of a ToS-grey catalog provider (web-cookie /
	// subscription auth). Tier 2 always-protected + admin scope, so
	// an unauthenticated caller can never open the gate.
	s.register(mux, "POST", "/api/providers/{id}/risk-acknowledgement", s.handleRiskAcknowledgement)
	s.register(mux, "DELETE", "/api/providers/{id}", s.handleDeleteProvider)
	s.register(mux, "POST", "/api/providers/{id}/keys", s.handleAddProviderKey)
	s.register(mux, "GET", "/api/capabilities", s.handleCapabilities)
	s.register(mux, "POST", "/api/capabilities/refresh", s.handleCapabilitiesRefresh)
	s.register(mux, "GET", "/api/providers/{id}/capabilities", s.handleProviderCapabilities)
	s.register(mux, "DELETE", "/api/providers/{id}/keys/{keyId}", s.handleDeleteProviderKey)
	s.register(mux, "POST", "/api/providers/{id}/keys/{keyId}/role", s.handleSetProviderKeyRole)
	s.register(mux, "POST", "/api/providers/{id}/keys/{keyId}/rotate", s.handleRotateProviderKey)
	s.register(mux, "POST", "/api/providers/{id}/enabled", s.handleSetProviderEnabled)
	s.register(mux, "GET", "/api/catalog", s.handleCatalog)
	s.register(mux, "GET", "/api/usage/budget", s.handleUsageBudget)
	s.register(mux, "POST", "/api/usage/budget", s.handleUsageBudget)
	// B12: burn-rate forecast for one key. Same management tier as
	// /api/usage/budget; computed only from real ledger spend + the
	// stored budget limit — never invented when data is missing.
	s.register(mux, "GET", "/api/usage/budget/forecast", s.handleUsageBudgetForecast)
	// M8.17 analytics.
	s.register(mux, "GET", "/api/analytics/summary", s.handleAnalyticsSummary)
	s.register(mux, "GET", "/api/analytics/by-provider", s.handleAnalyticsByProvider)
	s.register(mux, "GET", "/api/analytics/by-model", s.handleAnalyticsByModel)
	s.register(mux, "GET", "/api/analytics/costs", s.handleAnalyticsCosts)
	s.register(mux, "GET", "/api/analytics/compression", s.handleAnalyticsCompression)
	s.register(mux, "GET", "/api/analytics/diversity", s.handleAnalyticsDiversity)
	s.register(mux, "GET", "/api/analytics/auto-routing", s.handleAnalyticsAutoRouting)
	s.register(mux, "GET", "/api/analytics/combo-health", s.handleAnalyticsComboHealth)
	// M8.16 resilience / logs / guardrails-test.
	s.register(mux, "GET", "/api/resilience", s.handleResilience)
	// M10 B6 resilience settings read/patch + breaker reset +
	// per-model cooldowns (guard rows in guard.go).
	s.register(mux, "GET", "/api/resilience/settings", s.handleResilienceSettings)
	s.register(mux, "PATCH", "/api/resilience/settings", s.handleResilienceSettingsPatch)
	s.register(mux, "POST", "/api/resilience/breakers/reset", s.handleResilienceBreakerReset)
	s.register(mux, "POST", "/api/resilience/cooldowns", s.handleResilienceCooldownSet)
	s.register(mux, "DELETE", "/api/resilience/cooldowns/{model...}", s.handleResilienceCooldownClear)
	// G11 fallback chains (guard rows in guard.go).
	s.register(mux, "GET", "/api/fallback/chains", s.handleFallbackChains)
	s.register(mux, "POST", "/api/fallback/chains", s.handleFallbackChains)
	s.register(mux, "DELETE", "/api/fallback/chains", s.handleFallbackChain)
	s.register(mux, "GET", "/api/logs", s.handleAPILogs)
	s.register(mux, "POST", "/api/guardrails/test", s.handleGuardrailsTest)
	// M8.18 teams + quota-pools.
	s.register(mux, "GET", "/api/teams", s.handleAPITeams)
	s.register(mux, "POST", "/api/teams", s.handleAPITeams)
	s.register(mux, "GET", "/api/teams/{id}", s.handleAPITeam)
	s.register(mux, "PATCH", "/api/teams/{id}", s.handleAPITeam)
	s.register(mux, "DELETE", "/api/teams/{id}", s.handleAPITeam)
	s.register(mux, "GET", "/api/quota-pools", s.handleAPIQuotaPools)
	s.register(mux, "POST", "/api/quota-pools", s.handleAPIQuotaPools)
	s.register(mux, "GET", "/api/quota-pools/{id}", s.handleAPIQuotaPool)
	s.register(mux, "PATCH", "/api/quota-pools/{id}", s.handleAPIQuotaPool)
	s.register(mux, "DELETE", "/api/quota-pools/{id}", s.handleAPIQuotaPool)
	s.register(mux, "GET", "/api/quota/intelligence", s.handleQuotaIntelligence)
	// M8.19 webhooks (guard rows first — registerWebhooksRoutes
	// panics on a missing table entry, like s.register).
	s.registerWebhooksRoutes(mux)
	s.registerEvalRoutes(mux)
	s.registerOrgRoutes(mux)
	s.registerCostRoutes(mux)
	s.register(mux, "GET", "/api/usage/om-usage", s.handleOmUsage)
	s.register(mux, "GET", "/api/cache/stats", s.handleCacheStats)
	s.register(mux, "GET", "/api/telemetry/model-latency-stats", s.handleModelLatencyStats)
	// M8.15 pricing + cache management.
	s.register(mux, "GET", "/api/pricing", s.handleAPIPricing)
	s.register(mux, "PATCH", "/api/pricing", s.handleAPIPricing)
	s.register(mux, "DELETE", "/api/cache", s.handleCacheDelete)
	s.register(mux, "GET", "/metrics", s.handleMetrics)
	s.register(mux, "GET", "/v1/models", s.handleModels)
	// M8.10 model detail / query / health (literals first for
	// readability; Go's mux prefers them over the wildcard).
	s.register(mux, "GET", "/v1/models/query", s.handleModelsQuery)
	s.register(mux, "GET", "/v1/models/health", s.handleModelsHealth)
	s.register(mux, "GET", "/v1/models/{model...}", s.handleModelDetail)
	// M8.12 per-provider routes (three explicit lines — NOT the
	// RegisterProviderV1Routes one-liner as well; registering
	// both would double-register and panic on duplicate patterns).
	s.register(mux, "POST", "/v1/providers/{provider}/chat/completions", s.handleProviderChatCompletions)
	s.register(mux, "GET", "/v1/providers/{provider}/models", s.handleProviderModels)
	s.register(mux, "GET", "/v1/providers/{provider}/status", s.handleProviderStatus)
	// M8.13 Gemini-native surface.
	s.register(mux, "GET", "/v1beta/models", s.handleGeminiModels)
	s.register(mux, "POST", "/v1beta/models/{rest...}", s.handleGeminiModelAction)
	s.register(mux, "POST", "/v1/chat/completions", s.handleChatCompletions)
	// C2: WebSocket streaming endpoint.
	s.register(mux, "GET", "/v1/ws", s.handleWebSocketChat)
	// M9.8 A2A JSON-RPC endpoint (guard row POST /a2a).
	s.register(mux, "POST", "/a2a", s.handleA2A)
	// M9.9 A2A operator status (guard row GET /api/a2a/status):
	// the opener reuses the lazily-opened a2aService — no second
	// store — and the handler's "never called while disabled"
	// contract holds: with A2A off, enabled=false is passed and
	// the opener is never invoked, so no tasks.db is created.
	s.register(mux, "GET", "/api/a2a/status", A2AStatusHandler(s.cfg != nil && s.cfg.A2AEnabled, func() (*a2a.Store, error) {
		svc, err := s.a2aService()
		if err != nil {
			return nil, err
		}
		return svc.Store(), nil
	}))
	// M9.9 A2A agent card (guard row GET /.well-known/agent.json):
	// the public discovery document — no auth (see the guard-row
	// comment in guard.go). Registered ONLY when the server has
	// a trusted base URL (agentCardBaseURL, from the configured
	// listen address — never from a request): a card with an
	// empty or guessed url would misdirect every agent that
	// fetches it, so no base URL means no card route. Production
	// always sets WithListenAddr (main.go, from config listen),
	// so the card is always served there; a bare test server
	// with no listen address simply has no card route. Version
	// is s.version with the OpenAPI "" → "0.1.0" fallback
	// (openapi.go). GET /api/a2a/status is registered above
	// (next to POST /a2a); its opener reuses a2aService via the
	// Service.Store() accessor, so no second store is opened.
	if baseURL := s.agentCardBaseURL(); baseURL != "" {
		version := s.version
		if version == "" {
			version = "0.1.0"
		}
		s.register(mux, "GET", "/.well-known/agent.json", AgentCardHandler(a2a.SkillDescriptors(), baseURL, version))
	}
	s.register(mux, "POST", "/v1/embeddings", s.handleEmbeddings)
	s.register(mux, "POST", "/v1/multimodal-embeddings", s.handleMultimodalEmbeddings)
	s.register(mux, "POST", "/v1/rerank", s.handleRerank)
	// M8.2 legacy completions + routing introspection.
	s.register(mux, "POST", "/v1/completions", s.handleLegacyCompletions)
	s.register(mux, "POST", "/v1/relay/chat/completions", s.handleChatCompletions)
	s.register(mux, "GET", "/v1/classify", s.handleClassify)
	s.register(mux, "GET", "/v1/explain/routing", s.handleExplainRouting)
	s.register(mux, "GET", "/v1/segment", s.handleSegment)
	// M8.5 images.
	s.register(mux, "POST", "/v1/images/generations", s.handleImagesGenerations)
	s.register(mux, "POST", "/v1/images/edits", s.handleImagesEdits)
	// M8.6 audio (canonical ops + the reference router aliases).
	s.register(mux, "POST", "/v1/audio/speech", s.handleAudioSpeech)
	s.register(mux, "POST", "/v1/audio/text-to-speech", s.handleAudioTextToSpeech)
	s.register(mux, "POST", "/v1/audio/text-to-speech/{voiceId}", s.handleAudioTextToSpeechWithVoice)
	s.register(mux, "POST", "/v1/audio/transcriptions", s.handleAudioTranscriptions)
	s.register(mux, "POST", "/v1/audio/speech-to-text", s.handleAudioSpeechToText)
	s.register(mux, "POST", "/v1/audio/translations", s.handleAudioTranslations)
	// M8.9 moderations.
	s.register(mux, "POST", "/v1/moderations", s.handleModerations)
	// M8.20 media verify.
	s.register(mux, "POST", "/v1/search", s.handleSearch)
	s.register(mux, "POST", "/v1/web/fetch", s.handleWebFetch)
	s.register(mux, "POST", "/v1/ocr", s.handleOCR)
	s.register(mux, "POST", "/v1/videos/generations", s.handleVideoGenerations)
	s.register(mux, "GET", "/v1/videos/{id}", s.handleVideoTask)
	s.register(mux, "POST", "/v1/music/generations", s.handleMusic)
	s.register(mux, "POST", "/v1/messages", s.handleMessages)
	// M8.3 token counting (Messages body).
	s.register(mux, "POST", "/v1/messages/count_tokens", s.handleMessagesCountTokens)
	s.register(mux, "POST", "/v1/responses", s.handleResponses)
	s.register(mux, "GET", "/v1/responses/{id}", s.handleGetResponse)
	s.register(mux, "GET", "/v1/responses", s.handleListResponses)
	s.register(mux, "DELETE", "/v1/responses/{id}", s.handleDeleteResponse)
	// M8.4 responses sub-action + M8.3 token counting (stored).
	s.register(mux, "GET", "/v1/responses/{id}/input_items", s.handleListResponseInputItems)
	s.register(mux, "GET", "/v1/responses/input_tokens", s.handleResponsesInputTokens)
	// M8.7 files (single dispatcher; 405 for wrong methods).
	s.register(mux, "POST", "/v1/files", s.handleFiles)
	s.register(mux, "GET", "/v1/files", s.handleFiles)
	s.register(mux, "GET", "/v1/files/{id}", s.handleFiles)
	s.register(mux, "DELETE", "/v1/files/{id}", s.handleFiles)
	s.register(mux, "GET", "/admin/files/legacy", s.handleFilesLegacyList)
	s.register(mux, "POST", "/admin/files/reclaim", s.handleFilesReclaim)
	s.register(mux, "GET", "/v1/files/{id}/content", s.handleFiles)
	// M8.8 batches (single dispatcher; 405 for wrong methods).
	s.register(mux, "POST", "/v1/batches", s.handleBatches)
	s.register(mux, "GET", "/v1/batches", s.handleBatches)
	s.register(mux, "GET", "/v1/batches/{id}", s.handleBatches)
	s.register(mux, "POST", "/v1/batches/{id}/cancel", s.handleBatches)
	s.register(mux, "DELETE", "/v1/batches/{id}", s.handleBatches)
	s.register(mux, "DELETE", "/v1/batches/delete-completed", s.handleBatches)
	s.register(mux, "GET", "/v1/me/status", s.handleMeStatus)
	s.register(mux, "GET", "/v1/me/sessions", s.handleMeSessions)
	// M8.11 session detail + pre-flight quota check.
	s.register(mux, "GET", "/v1/me/sessions/{id}", s.handleMeSessionDetail)
	s.register(mux, "POST", "/v1/quotas/check", s.handleQuotasCheck)
	s.register(mux, "GET", "/v1/auto-combo/{channel}/candidates", s.handleAutoComboCandidates)
	// M8.14 combos / chains CRUD (shared store; the register
	// wrapper enforces tier + scope, the store does no auth).
	s.register(mux, "GET", "/v1/combos", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleListCombos(w, r) })
	s.register(mux, "POST", "/v1/combos", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleCreateCombo(w, r) })
	s.register(mux, "GET", "/v1/combos/{id}", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleGetCombo(w, r) })
	s.register(mux, "PATCH", "/v1/combos/{id}", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handlePatchCombo(w, r) })
	s.register(mux, "DELETE", "/v1/combos/{id}", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleDeleteCombo(w, r) })
	// B3 combos verbs — registered individually through s.register
	// (same closure pattern as the CRUD lines above). Do NOT mount
	// ComboStore.Handler() here: it bypasses guard/auth and would
	// double-register the 5 CRUD routes.
	s.register(mux, "POST", "/v1/combos/import", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleImportCombo(w, r) })
	s.register(mux, "GET", "/v1/combos/export", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleExportAllCombos(w, r) })
	s.register(mux, "GET", "/v1/combos/{id}/export", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleExportCombo(w, r) })
	s.register(mux, "POST", "/v1/combos/{id}/duplicate", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleDuplicateCombo(w, r) })
	s.register(mux, "POST", "/v1/combos/{id}/test-run", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleTestRunCombo(w, r) })
	s.register(mux, "POST", "/v1/combos/{id}/enable", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleEnableCombo(w, r) })
	s.register(mux, "POST", "/v1/combos/{id}/disable", func(w http.ResponseWriter, r *http.Request) { s.combosStore().handleDisableCombo(w, r) })
	s.register(mux, "GET", "/dashboard/responses", s.handleDashboardResponses)
	s.register(mux, "GET", "/dashboard/responses/tree", s.handleDashboardResponsesTree)
	s.register(mux, "GET", "/dashboard/readiness", s.handleReadiness)
	s.register(mux, "GET", "/dashboard/headless", s.handleHeadlessStatus)
	s.register(mux, "POST", "/admin/shutdown", s.handleShutdown)
	s.register(mux, "POST", "/admin/restart", s.handleRestart)
	s.register(mux, "POST", "/admin/update", s.handleAdminUpdate)
	s.register(mux, "POST", "/admin/tunnel/start", s.handleTunnelStart)
	s.register(mux, "POST", "/admin/tunnel/stop", s.handleTunnelStop)
	s.register(mux, "GET", "/admin/tunnel/status", s.handleTunnelStatus)
	s.register(mux, "POST", "/admin/bootstrap", s.handleBootstrap)
	// Phase 1 provider account login (provider_login.go): start is
	// Tier 1 + admin key; the browser callback is Tier 1 loopback-only
	// without a key (one-time PKCE state is its credential).
	s.register(mux, "POST", "/admin/provider/login", s.handleProviderLogin)
	s.register(mux, "GET", "/admin/provider/login/callback", s.handleProviderLoginCallback)
	s.register(mux, "POST", "/admin/settings/export", s.handleSettingsExport)
	s.register(mux, "POST", "/admin/db/export", s.handleDBExport)
	s.register(mux, "POST", "/admin/db/wipe/arm", s.handleDBWipeArm)
	s.register(mux, "POST", "/admin/db/wipe", s.handleDBWipe)
	s.register(mux, "POST", "/admin/db/import", s.handleDBImport)
	s.register(mux, "POST", "/admin/keys/revoke-all", s.handleRevokeAllKeys)
	s.register(mux, "POST", "/admin/keys/{id}", s.handleKeyUpdate)
	s.register(mux, "DELETE", "/admin/keys/{id}", s.handleKeyDelete)
	s.register(mux, "POST", "/admin/keys/{id}/rotate", s.handleKeyRotate)
	s.register(mux, "POST", "/admin/keys", s.handleKeyCreate)
	s.register(mux, "POST", "/admin/keys/import", s.handleKeyImport)
	// B3-07 OAuth 2.0 gateway auth (oauth_api.go).
	s.register(mux, "POST", "/oauth/clients", s.handleOAuthCreateClient)
	s.register(mux, "GET", "/oauth/clients", s.handleOAuthListClients)
	s.register(mux, "DELETE", "/oauth/clients/{id}", s.handleOAuthDeleteClient)
	s.register(mux, "GET", "/oauth/authorize", s.handleOAuthAuthorize)
	s.register(mux, "POST", "/oauth/token", s.handleOAuthToken)
	s.register(mux, "POST", "/oauth/revoke", s.handleOAuthRevoke)
	s.register(mux, "GET", "/admin/key-templates", s.handleKeyTemplates)
	s.register(mux, "POST", "/admin/config/reset", s.handleResetConfig)
	s.register(mux, "POST", "/admin/cache/flush", s.handleCacheFlush)
	s.register(mux, "GET", "/admin/status", s.handleAdminStatus)

	// B8 Dashboard Part 2 routes
	s.registerB8SettingsRoutes(mux)
	s.registerB8KeyRoutes(mux)
	s.registerB8AnalyticsRoutes(mux)
	s.registerB8LogsRoutes(mux)
	s.registerB8CombosRoutes(mux)
	// Phase 0: every documented unavailable feature (501
	// feature_unavailable), including the Tier 1 admin routes.
	s.registerUnavailable(mux)

	// P3-B: start the scheduled-evaluation loop (no-op on a store
	// failure — the store is lazy and re-opens on first API use).
	s.startEvalScheduler()

	s.handler = s.withLogging(s.withIPControl(s.withAudit(mux)))
	return s
}

// handleCacheFlush serves POST /admin/cache/flush (M1.5, Tier 2):
// the cache.NewFlushHandler for this server's flush store. An
// empty body flushes every exact-cache entry; {"key_id"} flushes
// only that gateway key's entries. The store is the serving
// engine when it implements cache.FlushStore (the degrade wrapper,
// with its per-tenant index); otherwise the relay's exact cache
// (flush-all only — see exactCacheFlushStore).
func (s *Server) handleCacheFlush(w http.ResponseWriter, r *http.Request) {
	cache.NewFlushHandler(s.cacheFlushStore()).ServeHTTP(w, r)
}

// cacheFlushStore returns the flush store for this server.
func (s *Server) cacheFlushStore() cache.FlushStore {
	if fs, ok := s.engine.(cache.FlushStore); ok {
		return fs
	}
	return exactCacheFlushStore{c: s.exactCache}
}

// exactCacheFlushStore adapts an ExactStore to cache.FlushStore
// for servers whose engine keeps no per-tenant index. FlushAll
// delegates through whichever FlushAll form the store offers
// (ExactCache.FlushAll() int, PersistentCache.FlushAll(ctx) int);
// FlushTenant always removes nothing and reports 0 — an
// ExactStore records no tenant per entry, so no per-key removal
// is possible there, and the honest count is 0 rather than a
// flush-all disguised as a per-key flush. A nil cache flushes
// nothing.
type exactCacheFlushStore struct{ c cache.ExactStore }

func (s exactCacheFlushStore) FlushAll(ctx context.Context) int {
	if s.c == nil {
		return 0
	}
	switch c := s.c.(type) {
	case interface{ FlushAll(context.Context) int }:
		return c.FlushAll(ctx)
	case interface{ FlushAll() int }:
		return c.FlushAll()
	default:
		return 0
	}
}

func (s exactCacheFlushStore) FlushTenant(ctx context.Context, tenant string) int {
	return 0
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

// Handler returns the full handler (logging + routes), for embedding in
// another mux or for httptest.
func (s *Server) Handler() http.Handler { return s.handler }

// ---------------------------------------------------------------------------
// Auth.
// ---------------------------------------------------------------------------

// requireAuth enforces Authorization: Bearer <gateway key> on /v1/*.
// There is no bypass flag: a missing verifier rejects everything.
// keyPolicyAllows enforces the caller's per-key IP / endpoint
// policy on an already-authenticated request (G21, Wave 3
// VerifyKey hook): bare reads such as GET /v1/models and the
// non-spend batch routes never reach checkKeyTrafficModel, so
// without this hook an IP-restricted key could list models from a
// blocked IP and an endpoint-allowlisted key could hit read routes
// outside its allowlist. The model axis is vacuous here — a bare
// read names no model — so the hook passes "". It writes the 403
// and returns false when the request must stop. A key with no
// stored policy passes untouched, exactly as before; a request
// with no verifiable key (relaxed mode, static verifier) has no
// key to check and passes untouched. Spend paths re-check the
// same policy in checkKeyTrafficModel — the re-check is
// side-effect-free, so the funnel check costs a doubly-gated
// request one extra lookup and nothing else.
//
// The hook runs AFTER scope (as a wrapper between scope and
// handler in register()), so a key refused for lacking a scope
// still answers with the scope 403 — policy never masks a scope
// refusal.
func (s *Server) keyPolicyAllows(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg == nil {
		return true
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		// AUD-SEC-001: a credential that authenticated but could not be
		// resolved to a key identity must FAIL CLOSED. It must never
		// inherit the unrestricted "no key presented" path, which is
		// how a vendor-header caller used to escape its own IP /
		// endpoint policy. With no credential at all (auth-relaxed) or
		// a verifier that cannot name identities by design, the
		// previous no-op behaviour stands.
		if credentialPresented(r) && s.identityResolvable() {
			// P2-IDENT-001 / P2-PROTO-001: Anthropic and Gemini
			// surfaces answer 403 in their own error envelopes.
			msg := "credential could not be resolved to a gateway key identity"
			if isAnthropicEndpointPath(r.URL.Path) {
				writeAnthropicError(w, http.StatusForbidden, msg)
			} else if strings.HasPrefix(r.URL.Path, "/v1beta/") {
				writeGeminiClientError(w, http.StatusForbidden, msg)
			} else {
				writeError(w, http.StatusForbidden, msg)
			}
			return false
		}
		return true
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		return true
	}
	return !s.checkKeyPolicy(w, s.policyEndpointRequest(r), key, "")
}

// policyEndpointRequest returns the request whose path the endpoint
// axis is checked against. One mapping, and one only (G21): a
// batch create's lines execute as /v1/chat/completions, so POST
// /v1/batches is checked as a chat request — the same clone
// enforceBatchModelPolicy makes (batches_api.go). Every other
// route is checked against its literal path, including the
// non-spend batch routes (list/get/cancel/delete), which execute
// no lines.
func (s *Server) policyEndpointRequest(r *http.Request) *http.Request {
	if r.Method == http.MethodPost && r.URL != nil && r.URL.Path == "/v1/batches" {
		out := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/v1/chat/completions"
		out.URL = &u
		return out
	}
	return r
}

// requireKeyPolicy is the G21 VerifyKey hook as a wrapper (B9):
// after auth — and after scope — it enforces the per-key IP /
// endpoint policy for the request, writing the 403 and stopping
// the chain on a refusal. Applied in register() between scope and
// handler, so it covers every authenticated route (spend and bare
// read alike) and never an AuthExempt one (those never reach the
// wrapper: register() leaves them unwrapped).
func (s *Server) requireKeyPolicy(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.keyPolicyAllows(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			s.unauthorized(w, r)
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	// WS2 unified identity: the canonical principal resolution —
	// the same parse, validation and precedence every other
	// identity-dependent check uses.
	p := s.principalFor(r)
	return p.Authenticated
}

// requestKey extracts the gateway credential from a request, or ""
// when there is none. Three presentations carry the SAME gateway key
// (P1.3 client compatibility):
//
//   - `Authorization: Bearer ***` — the canonical form, and the one
//     `codvian-router keygen` prints. Wins when present.
//   - `X-Api-Key: ***` — what Anthropic-style clients send (Cline in
//     Anthropic mode, Zed, Claude Code) against an Anthropic dialect.
//   - `X-Goog-Api-Key: ***` — what Google's Gemini SDKs send against
//     the Gemini native surface.
//
// A non-Bearer Authorization scheme ("Basic ...") is NOT a credential
// for us; the API-key headers are still consulted rather than treating
// the scheme's payload as a key. Every caller funnels through here, so
// auth, scope and the 401 diagnostics all read the credential one way.
func requestKey(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		if parts := strings.SplitN(auth, " ", 2); len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") && parts[1] != "" {
			return parts[1]
		}
	}
	if k := strings.TrimSpace(r.Header.Get("X-Api-Key")); k != "" {
		return k
	}
	if k := strings.TrimSpace(r.Header.Get("X-Goog-Api-Key")); k != "" {
		return k
	}
	// P2-PROTO-002: Gemini clients may present the API key as a
	// ?key= query parameter. Scope tightly to /v1beta/ so query
	// keys never leak into non-Gemini surfaces (query keys appear
	// in server logs and proxy headers).
	if strings.HasPrefix(r.URL.Path, "/v1beta/") {
		if k := strings.TrimSpace(r.URL.Query().Get("key")); k != "" {
			return k
		}
	}
	return ""
}

// bearerKey is requestKey under its historical name: the scope
// wrapper, the disabled-key 401 path and the caller-key accessor all
// parse the credential the same way (P1.3 widened it to the
// vendor-style API-key headers).
func bearerKey(r *http.Request) string { return requestKey(r) }

// principal is the canonical identity and authorization context of
// one request (WS2 unified identity — the single resolution path
// behind AUD-SEC-001). Every identity-dependent check (authentication,
// scope enforcement, caller attribution, ownership) resolves the same
// principal instead of independently parsing or validating the
// credential.
//
// Fields:
//   - Raw: the presented credential, verbatim. NEVER logged and
//     never returned to a client.
//   - Presented: any credential in a form the server accepts.
//   - Authenticated: the verifier (or the OAuth issuer) accepted it.
//   - KeyID: the caller identity — the gateway key's stored ID, or
//     "oauth:<client_id>" for a gateway JWT (B3-07). "" when the
//     deployment cannot name identities or the credential is absent.
//   - Scopes / ScopesKnown: the resolved scope set. ScopesKnown=false
//     means the deployment cannot name scopes for this credential
//     (shared-key verifier without KeyScopes) and scope checks pass
//     through as before — the pre-existing no-metadata semantics,
//     NOT a grant.
type principal struct {
	Raw           string
	Presented     bool
	Authenticated bool
	KeyID         string
	Scopes        []string
	ScopesKnown   bool
	IsJWT         bool
}

// principalFor resolves the ONE canonical principal for a request.
// Precedence is deterministic and documented:
//
//  1. The credential is extracted once by requestKey: Authorization
//     Bearer wins, then X-Api-Key, then X-Goog-Api-Key, then ?key=
//     on /v1beta/ only. Only one credential is ever consulted — a
//     request carrying several gets the highest-precedence one, so
//     conflicting credentials cannot mix identities mid-request.
//  2. A gateway key the verifier accepts authenticates; its stored
//     key ID (when the verifier can name IDs) and stored scopes
//     (when it can name scopes) ride the same resolution.
//  3. Otherwise a gateway JWT access token authenticates exactly
//     like a key (B3-07): identity "oauth:<client_id>", scopes from
//     the token.
//  4. Anything presented but accepted by neither is unauthenticated
//     (Authenticated=false) — callers answer 401/fail closed; no
//     identity or scopes are manufactured for it.
//
// A verifier that cannot validate (nil) yields an unauthenticated
// principal with the credential still marked presented, preserving
// auth-relaxed diagnostics.
func (s *Server) principalFor(r *http.Request) *principal {
	p := &principal{}
	raw := requestKey(r)
	p.Raw = raw
	p.Presented = raw != ""
	if raw == "" || s.verifier == nil {
		return p
	}
	if s.verifier.VerifyKey(r.Context(), raw) {
		p.Authenticated = true
		if idv, ok := s.verifier.(KeyIDVerifier); ok {
			if id, ok := idv.KeyID(r.Context(), raw); ok {
				p.KeyID = id
			}
		}
		if sv, ok := s.verifier.(ScopeVerifier); ok {
			got, known := sv.KeyScopes(r.Context(), raw)
			if known {
				p.Scopes, p.ScopesKnown = got, true
			}
		}
		return p
	}
	// B3-07: a gateway JWT access token authenticates exactly like
	// a gateway key — the same funnel.
	if s.oauthTokenValid(raw) {
		p.Authenticated = true
		p.IsJWT = true
		if iss, err := s.oauthIssuer(); err == nil && iss != nil {
			if claims, err := iss.VerifyAccessToken(raw); err == nil && claims.ClientID != "" {
				p.KeyID = "oauth:" + claims.ClientID
			}
		}
		if got, known := s.oauthTokenScopes(raw); known {
			p.Scopes, p.ScopesKnown = got, true
		}
	}
	return p
}

// requireScope enforces the guard table's scope list for one route
// (M1.1): the caller's key must carry at least one listed scope,
// and scope "admin" implies every scope (the operator scope). It
// runs AFTER auth, so a missing/invalid key still answers 401 from
// the auth wrapper — this wrapper only ever answers 403, for a key
// that authenticated but is not permitted on this route. Two
// pass-throughs, both deliberate: with no Bearer token there is no
// identity to scope-check (auth-relaxed mode already decided the
// request is allowed), and a verifier that cannot name scopes
// (StaticKeyVerifier) carries no scope metadata to enforce.
func (s *Server) requireScope(next http.HandlerFunc, scopes []string) http.HandlerFunc {
	if len(scopes) == 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// WS2 unified identity: the scopes come from the same
		// canonical principal resolution auth and caller
		// attribution use — no independent scope lookup here.
		p := s.principalFor(r)
		if !p.Presented {
			next(w, r)
			return
		}
		// B3-07: a gateway JWT carries its scopes in the token,
		// not in the key store — enforce them the same way
		// (admin implies all, below). A verifier that cannot
		// name scopes and a token that is not a gateway JWT
		// still pass through, as before (ScopesKnown=false is
		// the deployment's no-metadata property, not a grant).
		if !p.ScopesKnown {
			next(w, r)
			return
		}
		key := config.GatewayKey{Scopes: p.Scopes}
		for _, want := range scopes {
			if key.HasScope(want) || key.HasScope("admin") {
				next(w, r)
				return
			}
		}
		// P2-IDENT-001 / P2-PROTO-001: Anthropic and Gemini surfaces
		// answer 403 in their own error envelopes, not the OpenAI one.
		msg := "gateway key lacks required scope " + strings.Join(scopes, " or ")
		if isAnthropicEndpointPath(r.URL.Path) {
			writeAnthropicError(w, http.StatusForbidden, msg)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1beta/") {
			writeGeminiClientError(w, http.StatusForbidden, msg)
			return
		}
		writeError(w, http.StatusForbidden, msg)
	}
}

// unauthorized writes the 401 for a failed auth check (CHECKLIST
// 195): when the presented key matches a stored gateway key whose
// expiry has passed, the failure names the expiry — an expired key
// is never reported as a generic "invalid gateway key", so the
// operator (and the client) can tell a rotated/expired credential
// from a wrong one. The raw key is never echoed, and the expiry
// time is the caller's own key metadata. Every auth wrapper in
// this package funnels through here.
func (s *Server) unauthorized(w http.ResponseWriter, r *http.Request) {
	// AUD-API-001: the Anthropic surfaces answer with the Anthropic
	// error envelope for EVERY error, auth failures included — an
	// Anthropic SDK parses that shape and an OpenAI-shaped body is not
	// a body its error model describes.
	deny := func(status int, msg string) {
		if isAnthropicEndpointPath(r.URL.Path) {
			writeAnthropicError(w, status, msg)
			return
		}
		// P2-PROTO-001: Gemini surfaces answer 401/403 in the
		// Gemini error envelope {error:{code,message,status}},
		// not the OpenAI envelope.
		if strings.HasPrefix(r.URL.Path, "/v1beta/") {
			writeGeminiClientError(w, status, msg)
			return
		}
		writeError(w, status, msg)
	}
	if s.verifier != nil {
		if key := requestKey(r); key != "" {
			{
				if ev, ok := s.verifier.(ExpiryVerifier); ok {
					if exp, expired := ev.ExpiredKey(r.Context(), key); expired {
						deny(http.StatusUnauthorized, "gateway key expired on "+exp.UTC().Format(time.RFC3339))
						return
					}
				}
				if dv, ok := s.verifier.(DisabledKeyVerifier); ok {
					if dv.DisabledKey(r.Context(), key) {
						deny(http.StatusUnauthorized, "gateway key disabled")
						return
					}
				}
				// B3-07: an expired gateway JWT names its expiry,
				// like an expired gateway key does.
				if iss, err := s.oauthIssuer(); err == nil && iss != nil {
					if exp, expired := iss.TokenExpired(key); expired {
						deny(http.StatusUnauthorized, "oauth token expired on "+exp.UTC().Format(time.RFC3339))
						return
					}
				}
			}
		}
	}
	deny(http.StatusUnauthorized, "invalid gateway key")
}

// callerKeyID returns the gateway-key ID the request arrived under,
// or "" when there is none or the verifier cannot name it. It is
// the caller identity for prompt-cache affinity: the auto
// prompt_cache_key is keyed on it, so requests under different
// gateway keys never share a cache key. The ID is never logged or
// returned to any client.
func (s *Server) callerKeyID(r *http.Request) string {
	// WS2 unified identity: resolve the canonical principal once.
	// KeyID semantics preserved exactly: no verifier, no presented
	// credential, or a verifier that cannot name identities → ""
	// (a verifier WITHOUT KeyIDVerifier never resolved a JWT
	// identity here before, and principalFor preserves that); a
	// verifier WITH KeyIDVerifier whose lookup misses resolves the
	// JWT caller identity "oauth:<client_id>" (B3-07), which
	// principalFor runs when key validation did not succeed. An
	// unauthenticated credential resolves to "" either way — the
	// old code's JWT lookup on a non-JWT string always missed too.
	return s.principalFor(r).KeyID
}

// callerForOwnership resolves the caller identity for owner-scoped
// resources (files, batches) and FAILS CLOSED when a credential was
// presented, the verifier can name identities, and none resolved
// (AUD-SEC-001/AUD-SEC-010): an unresolvable credential must never fall
// back to the shared "no identity" bucket, where unowned resources are
// visible. An absent credential keeps auth-relaxed semantics.
func (s *Server) callerForOwnership(w http.ResponseWriter, r *http.Request) (string, bool) {
	keyID := s.callerKeyID(r)
	if keyID == "" && credentialPresented(r) && s.identityResolvable() {
		// P2-IDENT-001: Anthropic surfaces answer 403 in the
		// Anthropic error envelope.
		msg := "credential could not be resolved to a gateway key identity"
		if isAnthropicEndpointPath(r.URL.Path) {
			writeAnthropicError(w, http.StatusForbidden, msg)
		} else {
			writeError(w, http.StatusForbidden, msg)
		}
		return "", false
	}
	return keyID, true
}

// credentialPresented reports whether the request carried ANY credential
// in a form this server accepts (the same surfaces as requestKey).
func credentialPresented(r *http.Request) bool { return requestKey(r) != "" }

// identityResolvable reports whether the configured verifier can name a
// caller identity at all. When it cannot (a shared-key verifier with no
// key IDs), an unresolved identity is a property of the deployment, not
// an anomaly, so per-key checks keep their no-op semantics. When it can,
// a presented credential that resolves to nothing must fail closed
// (AUD-SEC-001).
func (s *Server) identityResolvable() bool {
	if s.verifier == nil {
		return false
	}
	_, ok := s.verifier.(KeyIDVerifier)
	return ok
}

// ---------------------------------------------------------------------------
// JSON helpers.
// ---------------------------------------------------------------------------

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	// Code is a JSON-RPC-style numeric error code, present only
	// where the contract defines one (see guardrails.Code docs):
	// -32001 is a Codvian policy refusal (guardrail block, HTTP
	// 403) and -32603 is an upstream internal error (HTTP 502).
	// It is omitted everywhere else, so no other error can ever
	// be mistaken for either. Reason is the machine-readable
	// refusal reason (the guardrail kind: pii/secret/injection),
	// present only on policy refusals — never prompt text.
	Code   int    `json:"code,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Deployment names the provider deployment that failed a
	// stream (CHECKLIST 243, LiteLLM #51034): present only on
	// stream error alerts, so the alert names what to triage.
	// It is an operator-configured provider identifier, never
	// prompt or response text.
	Deployment string `json:"deployment,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// Encode BEFORE writing any header: a value that cannot be marshalled
	// (NaN/Inf floats, a failing custom marshaller) must never turn into a
	// silent empty 200 — the client would see success with no body. On an
	// encoding failure answer a 500 with a generic message.
	buf, err := json.Marshal(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"response could not be encoded","type":"server_error"}}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(buf, '\n'))
}

// writeError writes an OpenAI-shaped client error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: "invalid_request_error"}})
}

// writeServerError writes an OpenAI-shaped server/upstream error.
func writeServerError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: "server_error"}})
}

// writeUpstreamError writes an OpenAI-shaped upstream error: the
// status is the upstream's own 4xx (a terminal taxonomy verdict —
// the request itself was refused), the type names the source, and
// the message stays generic. No upstream detail is ever exposed to
// the client.
func writeUpstreamError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: "upstream_error"}})
}

// writePolicyRefusal writes a guardrail policy refusal (CHECKLIST
// 147, research-log entry 179): HTTP 403 with the Codvian
// policy-refusal code -32001 and a machine-readable reason naming
// the guardrail kind — so a client can tell a policy refusal from
// an upstream failure (-32603) without parsing message strings.
// what names what was refused ("request", "response"). The kind
// label is the only detail: no prompt or response text is ever
// echoed back.
func writePolicyRefusal(w http.ResponseWriter, kind, what string) {
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
		Message: "guardrail blocked " + what + ": " + kind,
		Type:    "policy_refusal",
		Code:    guardrails.PolicyRefusalCode,
		Reason:  kind,
	}})
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz serves GET /readyz — READINESS, not liveness
// (AUD-CFG-004). /healthz answers "the process is up" and is the
// liveness probe; /readyz answers "this process can serve real
// requests" and is the readiness probe (and the container
// healthcheck target): every critical dependency is checked with a
// bounded timeout and a failed dependency answers 503 with the
// failing check named — never a bare 503, never an internal secret
// or connection string.
//
// Critical dependencies (a failed one means requests cannot be
// served honestly):
//   - engine: the routing engine must be wired. New always sets it,
//     so a nil engine means a misconstructed server — fail closed.
//   - ledger: when one is wired, its SQLite connection must answer
//     a ping within readyzTimeout. NOT wired is legitimate on a
//     fresh install (the dashboard feeds answer honestly with empty
//     data, see WithLedger), so an absent ledger is not unready.
//
// Optional providers (the readiness capability gate at
// /dashboard/readiness) stay out of this check: a provider being
// unready degrades specific models, it does not take the gateway
// down.
const readyzTimeout = 2 * time.Second

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var failed []string
	if s.engine == nil {
		failed = append(failed, "engine not wired")
	}
	if s.ledger != nil {
		ctx, cancel := context.WithTimeout(r.Context(), readyzTimeout)
		if err := s.ledger.Ping(ctx); err != nil {
			failed = append(failed, "ledger unavailable: "+err.Error())
		}
		cancel()
	}
	if len(failed) > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unready", "reasons": failed})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// handleDashboard serves the approved static dashboard UI (embedded
// from the web package). It is intentionally NOT auth-gated: the page
// is static UI whose only live data comes from GET /dashboard/providers
// (provider facts, no keys — see handleDashboardProviders), and the
// server binds localhost by default (see --listen). It must never
// shadow the API: /dashboard is an exact path, so /v1/* and /metrics
// are unaffected.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(web.IndexHTML)
}

// dashboardProvidersResponse is the GET /dashboard/providers payload:
// the live configured providers plus a per-verification-status count
// summary (over all configured providers, enabled or not — the
// dashboard shows the same list, so the counts always add up to
// len(providers)).
type dashboardProvidersResponse struct {
	Providers []provider.DashboardProvider `json:"providers"`
	Summary   dashboardProviderSummary     `json:"summary"`
}

type dashboardProviderSummary struct {
	Total                     int `json:"total"`
	Enabled                   int `json:"enabled"`
	VerifiedFromDocs          int `json:"verified_from_docs"`
	FromReferenceMetadataOnly int `json:"from_reference_metadata_only"`
	Unverified                int `json:"unverified"`
}

// handleDashboardProviders serves the live provider feed for the
// dashboard: one entry per configured provider with its verification
// status, wire format and auth kind, straight from the registry —
// never a hard-coded list. It is intentionally NOT auth-gated, like
// /dashboard itself: the payload carries no secrets (no API keys, no
// base URLs), and the dashboard page that consumes it is served
// without a key. With no lister configured it answers an empty list
// (honest "no providers"), not an error.
func (s *Server) handleDashboardProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providers := []provider.DashboardProvider{}
	if s.dashboardProviders != nil {
		providers = s.dashboardProviders.DashboardProviders()
	}
	if providers == nil {
		providers = []provider.DashboardProvider{}
	}
	writeJSON(w, http.StatusOK, dashboardProvidersResponse{Providers: providers, Summary: summarizeProviders(providers)})
}

// summarizeProviders counts providers per verification status (over
// all configured providers, enabled or not — the dashboard shows the
// same list, so the counts always add up to len(providers)).
func summarizeProviders(providers []provider.DashboardProvider) dashboardProviderSummary {
	summary := dashboardProviderSummary{Total: len(providers)}
	for _, p := range providers {
		if p.Enabled {
			summary.Enabled++
		}
		switch p.Verification {
		case "verified-from-docs":
			summary.VerifiedFromDocs++
		case "from-reference-metadata-only":
			summary.FromReferenceMetadataOnly++
		default:
			summary.Unverified++
		}
	}
	return summary
}

// apiProvider is one entry in the GET /api/providers payload: the
// dashboard summary plus the last health-check outcome for that
// provider ("ok", "down", or "unknown" when it was never checked —
// e.g. no checker wired, a disabled provider, or a check that has not
// run yet) plus its recorded probe history (nil when the wired checker
// does not keep history, or the provider has no probes yet — see
// historyLister). It embeds provider.DashboardProvider, so the JSON
// carries the same safe fields (id, name, wire format, verification,
// auth kind, models) and — like /dashboard/providers — never a key or
// a base URL.
type apiProvider struct {
	provider.DashboardProvider
	HealthStatus string `json:"health_status"`
	// LastError is the error string of the last failed health probe,
	// taken from the health checker's last status at feed-build time
	// — no new probe is run for this feed. "" when the provider was
	// never probed or the last probe succeeded: honest empty, never
	// a fabricated message. The dashboard's error breakdown (M10-2
	// Q18) reads this field.
	LastError string              `json:"last_error"`
	History   *apiProviderHistory `json:"history,omitempty"`
	// Keys overrides the embedded DashboardProvider.Keys (the
	// shallower field wins in JSON): the same pool keys, enriched
	// with live status and honest per-key spend (CHECKLIST
	// 175-176). See apiProviderKey.
	Keys []apiProviderKey `json:"keys,omitempty"`
}

// apiProviderKey is one pool key in the GET /api/providers feed:
// the registry's identity/role/fingerprint facts (embedded), plus
// live status and honest spend from the engine + ledger.
// Status is "active" (routing now), "cooling" (inside a cooldown
// — CoolingUntil names when it ends), "reserve" (held back until
// every preferred key is exhausted), or "cooling-reserve" (a
// reserve key that is also cooling). CoolingMode is the cooldown's
// mode ("hard"/"soft-demote") while cooling, "" otherwise.
// Spend is the ledger's honest totals for this key alone:
// Requests counts every attempt row (ok and failed); Tokens and
// CostUSD sum every row's tokens/cost — the same definition the
// ledger's SpendFor uses (a failed row that still carries tokens
// still cost them; in practice failed attempts arrive with zero
// tokens, so they contribute 0). HasSpend is false when the key has
// no rows yet: the dashboard then shows "no usage yet", never a
// $0 that would claim the key served for free. LastUsed is the
// newest row's timestamp (nil while HasSpend is false).
type apiProviderKey struct {
	provider.DashboardProviderKey
	Status       string     `json:"status"`
	CoolingUntil *time.Time `json:"cooling_until,omitempty"`
	CoolingMode  string     `json:"cooling_mode,omitempty"`
	Requests     int        `json:"requests"`
	Tokens       int        `json:"tokens"`
	CostUSD      float64    `json:"cost_usd"`
	HasSpend     bool       `json:"has_spend"`
	LastUsed     *time.Time `json:"last_used,omitempty"`
}

// historyLister reports the recorded probe history for one provider.
// *health.Checker satisfies it via History; the server asks for it
// with a type assertion on the wired HealthStatusLister so checkers
// without history (and existing test fakes) keep working with
// History nil instead of breaking.
type historyLister interface {
	History(providerID string) health.HistorySummary
}

// apiHistoryBucket is one hourly bucket of recorded probes in the
// last 24 h (oldest first). up/down count the recorded probes in
// that hour; both zero means no probe ran in that hour ("no data",
// never up). p50_ms is the median latency of the successful probes
// in the hour, or null when the hour had no successful probe.
type apiHistoryBucket struct {
	Start time.Time `json:"start"`
	Up    int       `json:"up"`
	Down  int       `json:"down"`
	P50Ms *int64    `json:"p50_ms"`
}

// apiProviderHistory is the recorded probe history for one provider,
// computed from real health probes only. uptime_1h / uptime_24h are
// the share of successful probes in that window, in percent, or null
// when no probe ran in the window — never an invented 100%.
// p50_ms / p95_ms are probe-latency percentiles over successful
// probes in the last 24 h, or null when there was no successful
// probe. samples is how many probe results are currently retained
// (bounded in-memory ring, resets on restart).
type apiProviderHistory struct {
	Samples   int                `json:"samples"`
	Uptime1h  *float64           `json:"uptime_1h"`
	Uptime24h *float64           `json:"uptime_24h"`
	P50Ms     *int64             `json:"p50_ms"`
	P95Ms     *int64             `json:"p95_ms"`
	P99Ms     *int64             `json:"p99_ms"`
	Buckets   []apiHistoryBucket `json:"buckets"`
}

// historyResponse converts a health.HistorySummary into its API
// shape. A provider with no probes yet reports samples 0, null
// windows/percentiles and 24 empty buckets.
func historyResponse(sum health.HistorySummary) *apiProviderHistory {
	buckets := make([]apiHistoryBucket, 0, len(sum.Buckets))
	for _, b := range sum.Buckets {
		buckets = append(buckets, apiHistoryBucket{Start: b.Start, Up: b.Up, Down: b.Down, P50Ms: b.P50Ms})
	}
	return &apiProviderHistory{
		Samples:   sum.Samples,
		Uptime1h:  sum.Uptime1h,
		Uptime24h: sum.Uptime24h,
		P50Ms:     sum.P50Ms,
		P95Ms:     sum.P95Ms,
		P99Ms:     sum.P99Ms,
		Buckets:   buckets,
	}
}

// apiProvidersResponse is the GET /api/providers payload.
// VertexTokenCache (CHECKLIST 284) is the shared Vertex
// minted-token cache's honest totals since process start, from
// provider.VertexTokenCacheStats: exchanges run, cached-token
// reuses, and distinct service-account keys cached. Zeroes with
// no Vertex provider are the honest empty state.
type apiProvidersResponse struct {
	Providers        []apiProvider            `json:"providers"`
	Summary          dashboardProviderSummary `json:"summary"`
	VertexTokenCache vertexTokenCacheStatus   `json:"vertex_token_cache"`
}

// vertexTokenCacheStatus is one shared-cache snapshot for the
// dashboard's Providers view: Mints counts token exchanges run,
// Hits counts requests that reused a cached token, Keys counts
// distinct service-account keys holding a cache entry.
type vertexTokenCacheStatus struct {
	Mints int64 `json:"mints"`
	Hits  int64 `json:"hits"`
	Keys  int   `json:"keys"`
}

// handleAPIProviders serves GET /api/providers, the dashboard's
// provider feed: one entry per configured provider with its
// verification status, wire format and health status, straight from
// the registry (plus the health checker's last results) — never a
// hard-coded list.
//
// AUTH: intentionally NOT auth-gated, like /dashboard and
// /dashboard/providers. The payload carries no secrets (no API keys,
// no base URLs, no credential material) — only operator-visible
// provider facts — and the dashboard page that consumes it is served
// without a key; the server binds localhost by default (see --listen).
// /v1/* and /metrics keep their gateway-key auth unchanged.
func (s *Server) handleAPIProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base := []provider.DashboardProvider{}
	if s.dashboardProviders != nil {
		base = s.dashboardProviders.DashboardProviders()
	}
	if base == nil {
		base = []provider.DashboardProvider{}
	}
	healthByID := map[string]bool{}
	lastErrorByID := map[string]string{}
	var hist historyLister
	if s.healthStatuses != nil {
		for _, st := range s.healthStatuses.Last() {
			healthByID[st.ProviderID] = st.OK
			lastErrorByID[st.ProviderID] = st.LastError
		}
		// Only checkers that keep probe history report it; the rest
		// leave History nil (honest "no history", not invented).
		hist, _ = s.healthStatuses.(historyLister)
	}
	providers := make([]apiProvider, 0, len(base))
	// Per-key live status + spend (CHECKLIST 175-176), keyed by
	// provider + "\x00" + key, the same composite identity the
	// engine uses. A cooling/spend row with an empty key ID
	// belongs to the provider's legacy single key (its attempts
	// carry no key ID — the legacy key is the one with no
	// fingerprint; pool keys always have one). A ledger error
	// leaves spend empty (keys report "no usage yet") instead of
	// failing the whole feed.
	coolingByKey := map[string]router.CoolingTarget{}
	if rep, ok := s.engine.(cooldownReporter); ok {
		for _, t := range rep.CooldownTargets() {
			coolingByKey[t.ProviderID+"\x00"+t.KeyID] = t
		}
	}
	spendByKey := map[string]ledger.ProviderKeySpend{}
	if s.ledger != nil {
		if totals, err := s.ledger.ProviderKeySpendTotals(r.Context()); err != nil {
			s.logger.Printf("providers feed: per-key spend unavailable: %v", err)
		} else {
			for _, sp := range totals {
				spendByKey[sp.Provider+"\x00"+sp.ProviderKeyID] = sp
			}
		}
	}
	enrichKeys := func(p provider.DashboardProvider) []apiProviderKey {
		if len(p.Keys) == 0 {
			return nil
		}
		// The legacy key is the one with no fingerprint
		// (synthesized from the pre-pooling single key, or
		// materialized from it — see
		// provider.EffectiveKeys). When exactly one key has
		// none, "" rows apply to it; otherwise "" rows are
		// unattributed history and match no key (e.g. the
		// legacy key was removed after pooling began).
		legacyKeyID := ""
		legacyKeys := 0
		for _, k := range p.Keys {
			if k.Fingerprint == "" {
				legacyKeys++
				legacyKeyID = k.ID
			}
		}
		if legacyKeys != 1 {
			legacyKeyID = ""
		}
		// Exact key match first; the legacy "" fallback only
		// for the legacy key.
		coolingFor := func(keyID string) (router.CoolingTarget, bool) {
			if t, ok := coolingByKey[p.ID+"\x00"+keyID]; ok {
				return t, true
			}
			if legacyKeyID != "" && keyID == legacyKeyID {
				if t, ok := coolingByKey[p.ID+"\x00"]; ok {
					return t, true
				}
			}
			return router.CoolingTarget{}, false
		}
		spendFor := func(keyID string) (ledger.ProviderKeySpend, bool) {
			if sp, ok := spendByKey[p.ID+"\x00"+keyID]; ok {
				return sp, true
			}
			if legacyKeyID != "" && keyID == legacyKeyID {
				if sp, ok := spendByKey[p.ID+"\x00"]; ok {
					return sp, true
				}
			}
			return ledger.ProviderKeySpend{}, false
		}
		out := make([]apiProviderKey, 0, len(p.Keys))
		for _, k := range p.Keys {
			ak := apiProviderKey{DashboardProviderKey: k}
			if sp, ok := spendFor(k.ID); ok {
				ak.Requests, ak.Tokens, ak.CostUSD = sp.Requests, sp.Tokens, sp.CostUSD
				ak.HasSpend = true
				ak.LastUsed = sp.LastUsed
			}
			if t, ok := coolingFor(k.ID); ok {
				until := t.Until
				ak.CoolingUntil = &until
				ak.CoolingMode = t.Mode
				if k.Role == provider.KeyRoleReserve {
					ak.Status = "cooling-reserve"
				} else {
					ak.Status = "cooling"
				}
			} else if k.Role == provider.KeyRoleReserve {
				ak.Status = "reserve"
			} else {
				ak.Status = "active"
			}
			out = append(out, ak)
		}
		return out
	}
	for _, p := range base {
		status := "unknown"
		if ok, checked := healthByID[p.ID]; checked {
			if ok {
				status = "ok"
			} else {
				status = "down"
			}
		}
		entry := apiProvider{DashboardProvider: p, HealthStatus: status, LastError: lastErrorByID[p.ID], Keys: enrichKeys(p)}
		if hist != nil {
			entry.History = historyResponse(hist.History(p.ID))
		}
		providers = append(providers, entry)
	}
	writeJSON(w, http.StatusOK, func() apiProvidersResponse {
		mints, hits, keys := provider.VertexTokenCacheStats()
		return apiProvidersResponse{
			Providers: providers, Summary: summarizeProviders(base),
			VertexTokenCache: vertexTokenCacheStatus{Mints: mints, Hits: hits, Keys: keys},
		}
	}())
}

// ---------------------------------------------------------------------------
// Dashboard live feeds: Overview, Models, Logs, Usage, Keys (Slice 3).
//
// All five endpoints are intentionally NOT auth-gated, like /dashboard
// and /dashboard/providers: they report only operator-visible
// metadata — request counts, token totals, latency percentiles,
// per-model/per-provider spend, key names and scopes — and never a
// secret (no raw gateway key — only its hash is stored, and the feed
// shows a fixed mask instead; no provider key, no base URL) and
// never message content (the ledger stores none). With no traffic
// yet they answer zero totals and empty lists: the dashboard renders
// that as an honest "No requests yet" empty state, never invented
// numbers. Spend is an honest lower bound: a model with no
// configured price records cost 0 (see ledger.Cost), so the payload
// says so instead of presenting the total as exact billing.
// ---------------------------------------------------------------------------

// dashboardModelLister lists the live model catalog for the dashboard
// feed. *provider.Registry satisfies it (see provider.Registry.
// DashboardModels), adapted here through providerRegistryAdapter.
type dashboardModelLister interface {
	DashboardModels() []provider.DashboardModel
}

// parseDashboardRange maps a ?range= value to its window: "24h" (24
// one-hour buckets), "7d" (7 one-day buckets), "30d" (30 one-day
// buckets). Anything else falls back to "24h" — an unknown range is
// a client typo, not a reason to fail the feed.
func parseDashboardRange(raw string) (label string, since time.Time, width time.Duration) {
	now := time.Now()
	switch raw {
	case "7d":
		return "7d", now.AddDate(0, 0, -7), 24 * time.Hour
	case "30d":
		return "30d", now.AddDate(0, 0, -30), 24 * time.Hour
	default:
		return "24h", now.Add(-24 * time.Hour), time.Hour
	}
}

// ledgerSummaryOrEmpty returns the ledger summary for [since, until),
// or an empty summary when no ledger is wired or the query fails
// (the failure is logged; the feed stays up with honest zeros
// rather than failing the whole dashboard).
func (s *Server) ledgerSummaryOrEmpty(ctx context.Context, since, until time.Time) ledger.UsageSummary {
	empty := ledger.UsageSummary{ByProvider: []ledger.ProviderUsage{}, ByModel: []ledger.ModelUsage{}, ByKey: []ledger.KeyUsage{}}
	if s.ledger == nil {
		return empty
	}
	sum, err := s.ledger.Summary(ctx, since, until)
	if err != nil {
		s.logger.Printf("dashboard overview: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		return empty
	}
	return sum
}

// cacheCombo is one (provider, model) combo's provider-reported
// cache usage inside the Overview window: how many of its requests
// reported cached tokens, and how many cached input tokens they
// reported. Shares are derived by the dashboard from these counts.
//
// The economics fields are the combo's prompt-cache economics at
// the operator-configured (provider, model) price — all *float64,
// nil meaning unpriced/unknown, never 0-as-unknown:
//   - CachedReadCostUSD: what the cached reads cost at the
//     configured cached-input price (nil when no cached price is
//     configured — the input price is never substituted).
//   - FullInputCostUSD: what the same cached tokens would have
//     cost at the full input price (the reference the read cost
//     is judged against; nil when no input price is configured).
//   - CacheWriteCostLowUSD / CacheWriteCostHighUSD: the estimated
//     cost of writing the combo's cache-write tokens, at
//     1.25x–2.5x the input price (see cacheWriteMultiplierLow /
//     High). Nil when no cache-write tokens were reported or no
//     input price is configured — the write cost is unknown, not
//     zero, and it is never derived from the cached-read count:
//     writes happen once per cache fill, reads many times after.
//     The band is now the fallback only: whenever the ledger
//     recorded the TTL split, CacheWriteCostUSD below carries the
//     exact tiered cost instead.
//   - CacheWriteCostUSD: the exact cost of writing the combo's
//     cache-write tokens, priced per TTL tier from the ledger's
//     recorded split (CacheWriteTokens5m / CacheWriteTokens1h, see
//     cacheComboEconomics): explicit tier rate when configured,
//     else the Anthropic-documented 1.25x (5m) / 2x (1h) multiple
//     for Anthropic-served Claude models, else the plain input
//     rate. Nil when no input price is configured, or when write
//     tokens were reported but the tier split never was.
type cacheCombo struct {
	Provider                 string   `json:"provider"`
	Model                    string   `json:"model"`
	Requests                 int      `json:"requests"`
	RequestsWithCachedTokens int      `json:"requests_with_cached_tokens"`
	CachedTokens             int      `json:"cached_tokens"`
	PromptTokens             int      `json:"prompt_tokens"`
	CacheWriteTokens         int      `json:"cache_write_tokens"`
	CacheWriteTokens5m       int      `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h       int      `json:"cache_write_tokens_1h"`
	CachedReadCostUSD        *float64 `json:"cached_read_cost_usd,omitempty"`
	FullInputCostUSD         *float64 `json:"full_input_cost_usd,omitempty"`
	CacheWriteCostLowUSD     *float64 `json:"cache_write_cost_low_usd,omitempty"`
	CacheWriteCostHighUSD    *float64 `json:"cache_write_cost_high_usd,omitempty"`
	CacheWriteCostUSD        *float64 `json:"cache_write_cost_usd,omitempty"`
}

// cacheWriteMultiplierLow / cacheWriteMultiplierHigh bound the
// estimated cache-write price as a multiple of the base input
// price. Anthropic documents 1.25x (5-minute TTL) and 2x (1-hour
// TTL) for cache writes; other providers' write surcharges run up
// to ~2.5x. The band is an estimate range, not a quote, and it is
// now the fallback only: whenever the ledger recorded the TTL
// split for a combo, cacheComboEconomics prices the exact tiered
// cost (CacheWriteCostUSD) instead, and the band survives solely
// for combos whose split was never reported. (An earlier version
// of this comment said the ledger records neither provider nor
// TTL — that is stale: the ledger has recorded the per-tier split
// since M6.9, see ledger.go CacheWriteTokens5m/1h.)
const (
	cacheWriteMultiplierLow  = 1.25
	cacheWriteMultiplierHigh = 2.5
)

// cacheWriteTier5mMultiplier / cacheWriteTier1hMultiplier are the
// Anthropic-documented cache-write price multiples used for the
// exact tiered cost (CacheWriteCostUSD): 1.25x for the 5-minute
// TTL, 2x for the 1-hour TTL. They mirror internal/ledger's
// cacheWrite5mMultiplier / cacheWrite1hMultiplier (unexported
// there); the values are Anthropic-documented, not chosen here,
// and they apply ONLY to Anthropic-served Claude models (see
// ledger.IsAnthropicCacheWriteTier).
const (
	cacheWriteTier5mMultiplier = 1.25
	cacheWriteTier1hMultiplier = 2.0
)

// cacheSummary is the Overview's prompt-cache block: window totals
// plus the ten busiest combos. Every number counts only
// provider-reported cached tokens (OpenAI
// prompt_tokens_details.cached_tokens, Anthropic
// cache_read_input_tokens, Gemini cachedContentTokenCount). Cache
// affinity steers OpenAI's prompt-cache placement; it is not
// response caching, not a cache-hit prediction, and not a bill
// guarantee — the dashboard says so next to these numbers.
// CacheWriteTokens counts provider-reported cache-write tokens
// (Anthropic cache_creation_input_tokens). The economics totals
// are the sums of the priced combos' economics (nil when no combo
// in the window is priced — unknown, never $0).
type cacheSummary struct {
	RequestsWithCachedTokens int          `json:"requests_with_cached_tokens"`
	CachedTokens             int          `json:"cached_tokens"`
	PromptTokens             int          `json:"prompt_tokens"`
	CacheWriteTokens         int          `json:"cache_write_tokens"`
	CachedReadCostUSD        *float64     `json:"cached_read_cost_usd,omitempty"`
	FullInputCostUSD         *float64     `json:"full_input_cost_usd,omitempty"`
	CacheWriteCostLowUSD     *float64     `json:"cache_write_cost_low_usd,omitempty"`
	CacheWriteCostHighUSD    *float64     `json:"cache_write_cost_high_usd,omitempty"`
	CacheWriteCostUSD        *float64     `json:"cache_write_cost_usd,omitempty"`
	Combos                   []cacheCombo `json:"combos"`
}

// tokenDivergenceJSON is one material local-vs-reported token
// count disagreement, as the Overview renders it: the local
// tiktoken estimate and the provider-reported prompt count for
// the same request, with when it happened.
type tokenDivergenceJSON struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Local    int       `json:"local_tokens"`
	Reported int       `json:"reported_tokens"`
	At       time.Time `json:"at"`
}

// tokenCountsSummary is the Overview's local-vs-reported token
// reconciliation block: how many successful attempts carried both
// a local estimate and a provider-reported prompt count
// (Compared), how many diverged materially (Diverged), how many
// local counts failed outright (CountErrors — a failure never
// fails the request, it only skips the comparison), and the most
// recent divergence (Last, absent before any). Zero counters with
// no traffic are an honest empty state, never invented agreement.
type tokenCountsSummary struct {
	Compared    uint64               `json:"compared"`
	Diverged    uint64               `json:"diverged"`
	CountErrors uint64               `json:"count_errors"`
	Last        *tokenDivergenceJSON `json:"last_divergence,omitempty"`
}

// tokenCountStatsReporter is optionally implemented by engines
// that reconcile local token counts against provider-reported
// ones (router.Engine does, via degrade.StaleEngine).
type tokenCountStatsReporter interface {
	TokenCountStats() router.TokenCountStats
}

// overviewResponse is the GET /dashboard/overview payload: range-
// limited KPIs from the ledger (requests, tokens, spend, buckets)
// plus since-start latency percentiles and cache counters from the
// recorder (the recorder keeps no timestamps, so it cannot answer a
// range — the payload labels those fields as since-start).
type overviewResponse struct {
	Range                string    `json:"range"`
	Since                time.Time `json:"since"`
	Requests             int       `json:"requests"`
	OK                   int       `json:"ok"`
	Errors               int       `json:"errors"`
	PrevRequests         int       `json:"prev_requests"`
	PrevOK               int       `json:"prev_ok"`
	PrevErrors           int       `json:"prev_errors"`
	PrevPromptTokens     int       `json:"prev_prompt_tokens"`
	PrevCompletionTokens int       `json:"prev_completion_tokens"`
	PrevTotalTokens      int       `json:"prev_total_tokens"`
	PrevCostUSD          float64   `json:"prev_cost_usd"`
	PromptTokens         int       `json:"prompt_tokens"`
	CompletionTokens     int       `json:"completion_tokens"`
	TotalTokens          int       `json:"total_tokens"`
	// ReasoningTokens / CacheWriteTokens are the window totals
	// of the provider-reported token-type splits (subsets of
	// completion / prompt tokens). LocalEstimateTokens is how
	// much of PromptTokens is a local tiktoken estimate rather
	// than a provider-reported count — subtract it for the
	// reported prompt total; RequestsWithLocalEstimate how many
	// requests carry one. UnknownSplitTokens still includes
	// estimate rows (their split is unknown).
	ReasoningTokens           int     `json:"reasoning_tokens"`
	CacheWriteTokens          int     `json:"cache_write_tokens"`
	LocalEstimateTokens       int     `json:"local_estimate_tokens"`
	RequestsWithLocalEstimate int     `json:"requests_with_local_estimate"`
	UnknownSplitTokens        int     `json:"unknown_split_tokens"`
	CostUSD                   float64 `json:"cost_usd"`
	MeanLatencyMS             float64 `json:"mean_latency_ms"`
	LatencyP50MS              float64 `json:"latency_p50_ms_since_start"`
	LatencyP95MS              float64 `json:"latency_p95_ms_since_start"`
	LatencyP99MS              float64 `json:"latency_p99_ms_since_start"`
	LatencySamples            uint64  `json:"latency_samples_since_start"`
	CacheHits                 uint64  `json:"cache_hits"`
	CacheMisses               uint64  `json:"cache_misses"`
	// CacheHitsExact / CacheHitsSemantic / CacheMissesExact /
	// CacheMissesSemantic split the gateway cache counters by
	// layer (CHECKLIST 187-188), read live from the recorder
	// snapshot — since-start, like CacheHits/CacheMisses. The
	// dashboard renders the split next to the hit rate; a layer
	// with no lookups yet reads 0, never an invented split.
	CacheHitsExact      uint64 `json:"cache_hits_exact"`
	CacheHitsSemantic   uint64 `json:"cache_hits_semantic"`
	CacheMissesExact    uint64 `json:"cache_misses_exact"`
	CacheMissesSemantic uint64 `json:"cache_misses_semantic"`
	Failovers           uint64 `json:"failovers_since_start"`
	// Panics is the total recovered panics since server start
	// (codvian_router_panics_total summed over locations — see
	// metrics.Snapshot.Panics). Zero until a panic actually
	// happens: the dashboard renders it as an honest zero.
	Panics uint64       `json:"panics_since_start"`
	Cache  cacheSummary `json:"cache"`
	// Compression is the window's lossless-first compression
	// totals from the ledger summary (CHECKLIST 183): how many
	// successful attempts carried a saving, and how many
	// local-estimate prompt tokens were saved. Honest zeros
	// before any compressed request.
	Compression compressionSummary     `json:"compression"`
	TokenCounts tokenCountsSummary     `json:"token_counts"`
	Buckets     []ledger.TimeBucket    `json:"buckets"`
	ByProvider  []ledger.ProviderUsage `json:"by_provider"`
}

// handleOverview serves GET /dashboard/overview?range=24h|7d|30d.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	label, since, width := parseDashboardRange(r.URL.Query().Get("range"))
	ctx := r.Context()
	now := time.Now()
	sum := s.ledgerSummaryOrEmpty(ctx, since, time.Time{})
	// Previous equal-length window, for the "vs previous period"
	// comparison: with no traffic in either window both are zero
	// and the dashboard says so instead of inventing a trend.
	window := now.Sub(since)
	prev := s.ledgerSummaryOrEmpty(ctx, since.Add(-window), since)
	buckets := []ledger.TimeBucket{}
	if s.ledger != nil {
		if b, err := s.ledger.Buckets(ctx, since, width); err != nil {
			s.logger.Printf("dashboard overview: ledger buckets failed: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			buckets = b
		}
	}
	var snap metrics.Snapshot
	if s.recorder != nil {
		snap = s.recorder.Snapshot()
	}
	// Prompt-cache aggregate (CHECKLIST 132 + 146): window totals
	// plus the ten busiest (provider, model) combos. ByModel
	// already arrives ordered by request count, so the first ten
	// are the ten busiest; a combo with no cached tokens still
	// appears when it is busy, with zeros — the aggregate never
	// hides a busy combo for having no cache hits. Each combo
	// also carries its prompt-cache economics at the
	// operator-configured (provider, model) price (see
	// cacheComboEconomics); the window economics totals sum only
	// the priced combos, and stay nil when none is priced.
	cache := cacheSummary{
		RequestsWithCachedTokens: sum.RequestsWithCachedTokens,
		CachedTokens:             sum.CachedTokens,
		PromptTokens:             sum.PromptTokens,
		CacheWriteTokens:         sum.CacheWriteTokens,
		Combos:                   []cacheCombo{},
	}
	for i, m := range sum.ByModel {
		if i >= 10 {
			break
		}
		cb := cacheCombo{
			Provider:                 m.Provider,
			Model:                    m.Model,
			Requests:                 m.Requests,
			RequestsWithCachedTokens: m.RequestsWithCachedTokens,
			CachedTokens:             m.CachedTokens,
			PromptTokens:             m.PromptTokens,
			CacheWriteTokens:         m.CacheWriteTokens,
			CacheWriteTokens5m:       m.CacheWriteTokens5m,
			CacheWriteTokens1h:       m.CacheWriteTokens1h,
		}
		cb.CachedReadCostUSD, cb.FullInputCostUSD, cb.CacheWriteCostLowUSD, cb.CacheWriteCostHighUSD, cb.CacheWriteCostUSD = s.cacheComboEconomics(m.Provider, m.Model, m.CachedTokens, m.CacheWriteTokens, m.CacheWriteTokens5m, m.CacheWriteTokens1h)
		cache.Combos = append(cache.Combos, cb)
	}
	cache.CachedReadCostUSD, cache.FullInputCostUSD, cache.CacheWriteCostLowUSD, cache.CacheWriteCostHighUSD, cache.CacheWriteCostUSD = sumCacheEconomics(cache.Combos)
	// Local-vs-reported token reconciliation (CHECKLIST 145):
	// counters + the most recent material divergence from the
	// engine, when it reports them. An engine that does not
	// reconcile leaves honest zeros.
	var tokCounts tokenCountsSummary
	if rep, ok := s.engine.(tokenCountStatsReporter); ok && rep != nil {
		st := rep.TokenCountStats()
		tokCounts.Compared = st.Compared
		tokCounts.Diverged = st.Diverged
		tokCounts.CountErrors = st.CountErrors
		if st.HasLast {
			tokCounts.Last = &tokenDivergenceJSON{
				Provider: st.Last.Provider,
				Model:    st.Last.Model,
				Local:    st.Last.Local,
				Reported: st.Last.Reported,
				At:       st.Last.At,
			}
		}
	}
	writeJSON(w, http.StatusOK, overviewResponse{
		Range: label, Since: since,
		Requests: sum.Requests, OK: sum.OK, Errors: sum.Errors,
		PrevRequests: prev.Requests, PrevOK: prev.OK, PrevErrors: prev.Errors,
		PrevPromptTokens: prev.PromptTokens, PrevCompletionTokens: prev.CompletionTokens,
		PrevTotalTokens: prev.TotalTokens, PrevCostUSD: prev.CostUSD,
		PromptTokens: sum.PromptTokens, CompletionTokens: sum.CompletionTokens,
		TotalTokens: sum.TotalTokens, UnknownSplitTokens: sum.UnknownSplitTokens,
		ReasoningTokens: sum.ReasoningTokens, CacheWriteTokens: sum.CacheWriteTokens,
		LocalEstimateTokens: sum.LocalEstimateTokens, RequestsWithLocalEstimate: sum.RequestsWithLocalEstimate,
		CostUSD: sum.CostUSD, MeanLatencyMS: sum.MeanLatencyMS,
		LatencyP50MS: snap.LatencyP50, LatencyP95MS: snap.LatencyP95, LatencyP99MS: snap.LatencyP99,
		LatencySamples: snap.LatencyN,
		CacheHits:      snap.CacheHits, CacheMisses: snap.CacheMiss,
		CacheHitsExact: snap.CacheHitsByLayer["exact"], CacheHitsSemantic: snap.CacheHitsByLayer["semantic"],
		CacheMissesExact: snap.CacheMissesByLayer["exact"], CacheMissesSemantic: snap.CacheMissesByLayer["semantic"],
		Failovers: snap.Failovers,
		Panics:    snap.Panics,
		Cache:     cache,
		Compression: compressionSummary{
			Requests:    sum.CompressionRequests,
			TokensSaved: sum.CompressionTokensSaved,
		},
		TokenCounts: tokCounts,
		Buckets:     buckets, ByProvider: sum.ByProvider,
	})
}

// cacheWriteTierRate resolves the USD per-token rate for one
// cache-write tier from the pair price: the explicit configured
// tier rate when one exists, else the Anthropic-documented
// multiple of the input price for Anthropic-served Claude models,
// else the plain input price (the tier rate is unknown for that
// provider — priced at 1x, never at a guessed multiple). It
// mirrors ledger.ModelPrice.cacheWriteRate, which prices the same
// tiers from ledger.ModelPrice; the server prices from
// config.ModelPrice, so the logic lives here, not there. Callers
// must only call it when price.InputUSDPer1M is non-nil — the
// fallback paths dereference it.
func cacheWriteTierRate(price config.ModelPrice, providerID, model string, oneHour bool) float64 {
	perToken := *price.InputUSDPer1M / 1e6
	if oneHour {
		if price.CacheWrite1hUSDPer1M != nil {
			return *price.CacheWrite1hUSDPer1M / 1e6
		}
		if ledger.IsAnthropicCacheWriteTier(providerID, model) {
			return perToken * cacheWriteTier1hMultiplier
		}
		return perToken
	}
	if price.CacheWrite5mUSDPer1M != nil {
		return *price.CacheWrite5mUSDPer1M / 1e6
	}
	if ledger.IsAnthropicCacheWriteTier(providerID, model) {
		return perToken * cacheWriteTier5mMultiplier
	}
	return perToken
}

// cacheComboEconomics computes one (provider, model) combo's
// prompt-cache economics at the operator-configured price (pair
// first, then the model-level fallback, via pairPrice). It returns
// (cached-read cost, full-input cost, cache-write low, cache-write
// high, cache-write exact); each is nil when its inputs are
// unknown, never 0 as a stand-in for unknown:
//   - read cost needs a configured cached-input price — the input
//     price is never substituted, because a cache hit genuinely
//     costs less and guessing it at full price would overstate it.
//   - full-input cost needs a configured input price; it is what
//     the same cached tokens would have cost uncached.
//   - the write-cost range needs reported cache-write tokens AND
//     a configured input price: write cost is the write tokens
//     times the input price times the 1.25x–2.5x band. Zero
//     reported write tokens means unknown, not free — the write
//     may simply not have been reported.
//   - the exact write cost needs the same, plus the ledger's
//     recorded TTL split (cacheWriteTokens5m / 1h): each tier is
//     priced at its own rate (see cacheWriteTierRate), and any
//     un-tiered remainder (total minus the split, from rows
//     recorded before the split existed) is priced at the plain
//     input rate — the ledger's unknown-tier fallback. When write
//     tokens were reported but the split never was (both tiers 0),
//     the exact cost is nil and the band is all that can be said.
func (s *Server) cacheComboEconomics(providerID, model string, cachedTokens, cacheWriteTokens, cacheWriteTokens5m, cacheWriteTokens1h int) (readCost, fullCost, writeLow, writeHigh, writeExact *float64) {
	price, _ := s.pairPrice(providerID, model)
	if price.InputUSDPer1M != nil {
		perToken := *price.InputUSDPer1M / 1e6
		f := float64(cachedTokens) * perToken
		fullCost = &f
		if cacheWriteTokens > 0 {
			lo := float64(cacheWriteTokens) * perToken * cacheWriteMultiplierLow
			hi := float64(cacheWriteTokens) * perToken * cacheWriteMultiplierHigh
			writeLow, writeHigh = &lo, &hi
			if tiered := cacheWriteTokens5m + cacheWriteTokens1h; tiered > 0 {
				exact := float64(cacheWriteTokens5m)*cacheWriteTierRate(price, providerID, model, false) +
					float64(cacheWriteTokens1h)*cacheWriteTierRate(price, providerID, model, true)
				if remainder := cacheWriteTokens - tiered; remainder > 0 {
					exact += float64(remainder) * perToken
				}
				writeExact = &exact
			}
		}
	}
	if price.CachedInputUSDPer1M != nil {
		r := float64(cachedTokens) * (*price.CachedInputUSDPer1M / 1e6)
		readCost = &r
	}
	return readCost, fullCost, writeLow, writeHigh, writeExact
}

// sumCacheEconomics sums the priced combos' economics into window
// totals. A total stays nil when no combo in the window priced
// that leg — unknown, never $0. (A priced combo with zero tokens
// contributes a genuine $0, which is a real number, not unknown.)
func sumCacheEconomics(combos []cacheCombo) (readCost, fullCost, writeLow, writeHigh, writeExact *float64) {
	sumPtr := func(dst **float64, v *float64) {
		if v == nil {
			return
		}
		if *dst == nil {
			zero := 0.0
			*dst = &zero
		}
		**dst += *v
	}
	for _, cb := range combos {
		sumPtr(&readCost, cb.CachedReadCostUSD)
		sumPtr(&fullCost, cb.FullInputCostUSD)
		sumPtr(&writeLow, cb.CacheWriteCostLowUSD)
		sumPtr(&writeHigh, cb.CacheWriteCostHighUSD)
		sumPtr(&writeExact, cb.CacheWriteCostUSD)
	}
	return readCost, fullCost, writeLow, writeHigh, writeExact
}

// dashboardModel is one row of the GET /dashboard/models payload:
// the registry's live metadata plus how many real requests this
// router has served with the model (all time; 0 until it is used).
type dashboardModel struct {
	provider.DashboardModel
	Requests int `json:"requests"`
	// PriceSource names where this row's prices come from: "pair"
	// (operator-configured price for exactly this provider+model),
	// "model" (operator-configured model-level fallback price),
	// "models.dev" (opt-in sync from models.dev — the price was
	// NOT set by the operator; see PriceSyncedAt for when),
	// "provider" (the provider-reported model price — no operator
	// price configured), or "unknown" (no price anywhere). The
	// row's Input/Output prices are the resolved pair price when
	// one exists, never the cheapest pair and never $0-as-unknown.
	PriceSource         string  `json:"price_source"`
	CachedInputPriceUSD float64 `json:"cached_input_price_usd,omitempty"`
	// PriceSyncedAt is when the row's price was last synced from
	// models.dev; set only when PriceSource is "models.dev".
	PriceSyncedAt *time.Time `json:"price_synced_at,omitempty"`
	// ContextWindowSource names where the row's context window
	// came from: "operator" (the stored provider model carries
	// it — set by the operator or the provider's own metadata),
	// "models.dev" (the opt-in sync applied it; see
	// ContextWindowSyncedAt for when), or "" when the window is
	// unknown (0). A window the sync applied and the operator
	// later changed reports "operator" — the sync never claims a
	// value it did not set.
	ContextWindowSource   string     `json:"context_window_source,omitempty"`
	ContextWindowSyncedAt *time.Time `json:"context_window_synced_at,omitempty"`
}

type dashboardModelsResponse struct {
	Models []dashboardModel `json:"models"`
	Total  int              `json:"total"`
	// DeprecatedHidden counts the deprecated rows hidden from this
	// response by the default filter (0 when ?include=deprecated
	// was passed, or when nothing is deprecated).
	DeprecatedHidden int `json:"deprecated_hidden"`
	// ContextFit is the fit-check decision of the most recent
	// request that reached the pre-call fit gate (CHECKLIST
	// 159): the prompt estimate, reserve, required tokens, and
	// the per-model fit verdicts the Models view renders as its
	// "Fit" column. Nil before any request reached the gate —
	// the column then reads "—", never an invented verdict.
	ContextFit *router.ContextFitDecision `json:"context_fit,omitempty"`
	// ModelsDev is the opt-in models.dev sync status (CHECKLIST
	// 179): enabled, last attempt/success, last error, stale
	// flag. Nil when no sync is configured — the Models view
	// then says the sync is off, never "unknown".
	ModelsDev *modelsDevStatusJSON `json:"models_dev,omitempty"`
}

// pairPrice resolves the operator-configured price for one
// (provider, model) pair: the pair price first, then the
// model-level fallback, then the opt-in models.dev synced price.
// The returned source is "pair", "model" or "models.dev";
// "" means no price exists for this pair at any level (the caller
// falls back to the provider-reported price, then unknown). A
// configured-but-free pair ($0) is a real price, not unknown —
// ModelPrice pointers keep free distinct from unset. The synced
// branch is LAST on purpose: an operator price at either level
// always wins over a synced price for the same pair.
func (s *Server) pairPrice(providerID, model string) (config.ModelPrice, string) {
	if s.priceBook == nil {
		return config.ModelPrice{}, ""
	}
	if p, ok := s.priceBook.LookupPair(providerID, model); ok && p.Known() {
		return p, "pair"
	}
	if p, ok := s.priceBook.LookupModel(model); ok && p.Known() {
		return p, "model"
	}
	if p, ok := s.priceBook.LookupSyncedPair(providerID, model); ok && p.Known() {
		return p, "models.dev"
	}
	return config.ModelPrice{}, ""
}

func priceOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// handleDashboardModels serves GET /dashboard/models: the live model
// catalog from the registry (identity + best-effort context/prices,
// 0/unknown shown as "—" by the dashboard, never guessed), joined
// with real per-model request counts from the ledger. Prices are the
// TRUE per-(provider, model) price: the operator-configured pair
// price when one exists, else the model-level fallback, else the
// provider-reported price, else unknown — with the source named per
// row, never the cheapest pair standing in for this pair. With no
// registry it answers an empty list, not an error.
func (s *Server) handleDashboardModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	models := []dashboardModel{}
	if ml, ok := s.registry.(dashboardModelLister); ok {
		for _, m := range ml.DashboardModels() {
			models = append(models, dashboardModel{DashboardModel: m})
		}
	} else if s.registry != nil {
		// Registry without the catalog feed: fall back to the plain
		// model list (IDs + owning provider name only).
		if list, err := s.registry.Models(r.Context()); err == nil {
			for _, m := range list {
				models = append(models, dashboardModel{DashboardModel: provider.DashboardModel{
					ID: m.ID, ProviderName: m.OwnedBy, Deprecated: m.Deprecated,
				}})
			}
		} else {
			s.logger.Printf("dashboard models: registry error: %v", guardrails.RedactLogLine(err.Error()))
		}
	}
	// Deprecated models are hidden by default, exactly as on
	// /v1/models (research-log entry 179): ?include=deprecated
	// lists them again. DeprecatedHidden counts the rows hidden
	// from this response, so the dashboard can say so honestly
	// instead of the models silently disappearing.
	deprecatedHidden := 0
	if r.URL.Query().Get("include") != "deprecated" {
		kept := models[:0]
		for _, m := range models {
			if m.Deprecated {
				deprecatedHidden++
				continue
			}
			kept = append(kept, m)
		}
		for i := len(kept); i < len(models); i++ {
			models[i] = dashboardModel{}
		}
		models = kept
	}
	// Real per-model request counts from the ledger (all time).
	if s.ledger != nil {
		sum := s.ledgerSummaryOrEmpty(r.Context(), time.Time{}, time.Time{})
		byModel := map[string]int{}
		for _, m := range sum.ByModel {
			byModel[m.Model] += m.Requests
		}
		for i := range models {
			models[i].Requests = byModel[models[i].ID]
		}
	}
	// True per-(provider, model) prices (item 116): the
	// operator-configured pair price wins over the model-level
	// fallback, which wins over the provider-reported price.
	// A row with no price anywhere stays unknown (source
	// "unknown", prices 0 → "—" on the dashboard). The opt-in
	// models.dev sync (CHECKLIST 179) is the third price level:
	// source "models.dev" with the sync time, so a synced price
	// is never presented as an operator-set one.
	mdStatus := s.modelsDevSnapshot()
	for i := range models {
		if p, src := s.pairPrice(models[i].ProviderID, models[i].ID); src != "" {
			models[i].InputPriceUSD = priceOrZero(p.InputUSDPer1M)
			models[i].OutputPriceUSD = priceOrZero(p.OutputUSDPer1M)
			models[i].CachedInputPriceUSD = priceOrZero(p.CachedInputUSDPer1M)
			models[i].PriceSource = src
			if src == "models.dev" {
				if sp, ok := s.priceBook.SyncedPair(models[i].ProviderID, models[i].ID); ok {
					t := sp.SyncedAt
					models[i].PriceSyncedAt = &t
				}
			}
		} else if models[i].InputPriceUSD > 0 || models[i].OutputPriceUSD > 0 {
			models[i].PriceSource = "provider"
		} else {
			models[i].PriceSource = "unknown"
		}
		// Context-window provenance (CHECKLIST 179): "models.dev"
		// only when the sync's recorded window is exactly the
		// window the row carries — a window the operator later
		// changed reports "operator", never a stale sync claim.
		if models[i].ContextWindow > 0 {
			if mdStatus != nil {
				if e, ok := mdStatus.Entries[modelsync.EntryKey(models[i].ProviderID, models[i].ID)]; ok && e.ContextWindow > 0 && e.ContextWindow == models[i].ContextWindow {
					models[i].ContextWindowSource = "models.dev"
					t := e.SyncedAt
					models[i].ContextWindowSyncedAt = &t
				} else {
					models[i].ContextWindowSource = "operator"
				}
			} else {
				models[i].ContextWindowSource = "operator"
			}
		}
	}
	// Catalog filters + shortlist (CHECKLIST 200): applied after
	// the prices are resolved (the max_price filter reads them),
	// so a filtered response still carries resolved prices on
	// every row it returns. A malformed filter answers 400 naming
	// the parameter, never a silently unfiltered list.
	mf, ferr := parseModelFilters(r)
	if ferr != nil {
		writeError(w, http.StatusBadRequest, ferr.Error())
		return
	}
	models = s.filterDashboardModels(models, mf)
	resp := dashboardModelsResponse{Models: models, Total: len(models), DeprecatedHidden: deprecatedHidden, ModelsDev: newModelsDevStatusJSON(mdStatus)}
	if dec, ok := s.lastContextFit(); ok {
		resp.ContextFit = &dec
	}
	writeJSON(w, http.StatusOK, resp)
}

// logsResponse is the GET /dashboard/logs payload: the most recent
// recorded attempts, newest first. Each row is one attempt (a
// request that failed over appears as its failed attempt row plus
// its successful row); shared rows are waiters served from a
// coalesced flight and always carry zero tokens and zero cost.
type logsResponse struct {
	Events []ledger.UsageEvent `json:"events"`
	Filter string              `json:"filter"`
	Limit  int                 `json:"limit"`
}

// handleLogs serves GET /dashboard/logs?limit=50&filter=all|ok|failed.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	limit := 50
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	filter := ledger.LogAll
	switch q.Get("filter") {
	case "ok":
		filter = ledger.LogOK
	case "failed":
		filter = ledger.LogFailed
	}
	events := []ledger.UsageEvent{}
	if s.ledger != nil {
		if ev, err := s.ledger.RecentEvents(r.Context(), limit, filter); err != nil {
			s.logger.Printf("dashboard logs: ledger query failed: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			events = ev
		}
	}
	writeJSON(w, http.StatusOK, logsResponse{Events: events, Filter: string(filter), Limit: limit})
}

// usageResponse is the GET /dashboard/usage payload: the range-limited
// spend/token roll-up from the ledger, failovers since start from
// the recorder, and the configured global budget cap (0 = none).
// CostEstimated is always true and the dashboard says so: spend is a
// lower bound, because a model with no configured price records $0.
type usageResponse struct {
	Range         string              `json:"range"`
	Since         time.Time           `json:"since"`
	Summary       ledger.UsageSummary `json:"summary"`
	Failovers     uint64              `json:"failovers_since_start"`
	BudgetUSD     float64             `json:"budget_usd"`
	CostEstimated bool                `json:"cost_estimated"`
}

// handleUsage serves GET /dashboard/usage?range=24h|7d|30d (default 30d:
// spend is a monthly question; the Overview keeps the 24h default).
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	raw := r.URL.Query().Get("range")
	if raw == "" {
		raw = "30d"
	}
	label, since, _ := parseDashboardRange(raw)
	sum := s.ledgerSummaryOrEmpty(r.Context(), since, time.Time{})
	var failovers uint64
	if s.recorder != nil {
		failovers = s.recorder.Snapshot().Failovers
	}
	budget := 0.0
	if s.cfg != nil {
		budget = s.cfg.BudgetUSD
	}
	writeJSON(w, http.StatusOK, usageResponse{
		Range: label, Since: since, Summary: sum,
		Failovers: failovers, BudgetUSD: budget, CostEstimated: true,
	})
}

// routerKeyView is one row of the GET /dashboard/keys payload: the
// stored gateway key's operator-visible facts. Masked is a fixed
// mask, never a prefix of the real key and never its hash: the raw
// key is shown exactly once when issued and only its SHA-256 hash is
// stored, so the dashboard cannot display (or recover) it — and
// must not pretend otherwise. Last-used is not tracked by the
// router, so the dashboard says so instead of inventing a time.
type routerKeyView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Scopes    []string  `json:"scopes"`
	BudgetUSD float64   `json:"budget_usd"`
	CreatedAt time.Time `json:"created_at"`
	Masked    string    `json:"masked"`
	// Key expiry + traffic limits (CHECKLIST 195): ExpiresAt is
	// nil when the key never expires; Expired is the live verdict
	// at feed time. RPM/TPM are 0 when unlimited on that axis.
	// The fingerprint is the first 12 hex chars of the stored
	// SHA-256 hash — enough to match a key to its record, never
	// enough to authenticate with, and never the raw key.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Expired   bool       `json:"expired"`
	// Disabled is the M1.3 kill switch: a disabled key is
	// refused at auth time, and the dashboard shows that state
	// instead of a key that merely looks unused.
	Disabled          bool   `json:"disabled"`
	Fingerprint       string `json:"fingerprint"`
	RequestsPerMinute int    `json:"requests_per_minute"`
	TokensPerMinute   int    `json:"tokens_per_minute"`
	// Guardrails names the guardrails that must guard this
	// key's requests (CHECKLIST 245): every name is registered
	// (a built-in kind or a configured external guardrail).
	// Empty means no per-key list — the global blocking config
	// guards the key exactly like a key without the field.
	Guardrails []string `json:"guardrails,omitempty"`
	// M5.10: the allowed-hours window, per-request throttle
	// delay and concurrent-session cap (0 = none set).
	AccessSchedule  *config.AccessScheduleConfig `json:"access_schedule,omitempty"`
	ThrottleDelayMs int                          `json:"throttle_delay_ms,omitempty"`
	MaxSessions     int                          `json:"max_sessions,omitempty"`
	// DeviceCount is the number of distinct devices seen under
	// this key on admitted /v1 requests (B3-02 follow-up): the
	// in-memory device tracker (keydevices.go) counts one
	// fingerprint per distinct peer IP + User-Agent, newest
	// first. 0 means no device seen yet — or the router
	// restarted, which forgets devices. Always present in the
	// keys feed so every key card can show "{count} device(s)";
	// single-key create/update responses leave it 0 (those are
	// built in keystatus.go, not from this feed).
	DeviceCount int `json:"device_count"`
}

type routerKeysResponse struct {
	Keys  []routerKeyView `json:"keys"`
	Total int             `json:"total"`
}

// handleKeys serves GET /dashboard/keys: the gateway keys from the
// loaded config. With no config loaded it answers an empty list.
func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keys := []routerKeyView{}
	if s.cfg != nil {
		now := time.Now()
		for _, k := range s.cfg.GatewayKeys {
			scopes := k.Scopes
			if scopes == nil {
				scopes = []string{}
			}
			keys = append(keys, routerKeyView{
				ID: k.ID, Name: k.Name, Scopes: scopes,
				BudgetUSD: k.BudgetUSD, CreatedAt: k.CreatedAt,
				Masked:    "crk_••••••••",
				ExpiresAt: k.ExpiresAt, Expired: k.Expired(now),
				Disabled:          k.Disabled,
				Fingerprint:       keyFingerprint(k),
				RequestsPerMinute: k.RequestsPerMinute,
				TokensPerMinute:   k.TokensPerMinute,
				Guardrails:        k.Guardrails,
				AccessSchedule:    k.AccessSchedule,
				ThrottleDelayMs:   k.ThrottleDelayMs,
				MaxSessions:       k.MaxSessions,
				DeviceCount:       len(s.keyDevicesFor(k.ID)),
			})
		}
	}
	writeJSON(w, http.StatusOK, routerKeysResponse{Keys: keys, Total: len(keys)})
}

// ---------------------------------------------------------------------------
// Auto-router (GET /dashboard/auto-route, /dashboard/auto-route/last).
//
// Both endpoints are intentionally NOT auth-gated, like /dashboard
// and /dashboard/providers: they run the local auto-router only —
// no provider is called, no key is involved, and no prompt is
// stored. The classify endpoint answers a live routing decision for
// the prompt the dashboard operator typed; the "last" endpoint
// reports the decision of the most recent real "auto/*" request
// through /v1/* (or an honest empty state when there has not been
// one yet). Neither invents traffic: with no auto-router configured
// they answer 503, and with no live decision yet "last" answers
// {"has_decision": false}.
// ---------------------------------------------------------------------------

// autoDecisionReporter is optionally implemented by engines that can
// report the routing decision of their most recent auto-routed
// request (router.Engine, and degrade.StaleEngine which forwards it).
type autoDecisionReporter interface {
	LastAutoDecision() (autorouter.Decision, bool)
}

// autoRouteEnriched is the GET /dashboard/auto-route payload: the
// live Decision plus the resolved model's real cost and measured
// quality. ModelCostUSDPer1M is the cheapest configured
// (provider, model) pair price for the resolved model — the
// decision names a model, not a provider — and is nil (with
// ModelPriceKnown false) when the model has no configured price at
// either level: unknown, never $0. ModelQuality is the measured
// error-free completion rate over real attempts (see modelQuality),
// nil with 0 samples when the model has no recorded attempts yet.
// modelPairPrice is one provider's true price for the resolved
// model: the operator-configured pair price when one exists, else
// the model-level fallback, else the provider-reported price, else
// unknown (Known=false, prices 0). Blended is the mean of input
// and output when both are configured. Cheapest marks the lowest
// known blended price — the pair the cost ranking would actually
// serve first. Pairs sort cheapest-known first, unknown last.
type modelPairPrice struct {
	Provider            string   `json:"provider"`
	ProviderName        string   `json:"provider_name"`
	InputUSDPer1M       float64  `json:"input_usd_per_1m"`
	OutputUSDPer1M      float64  `json:"output_usd_per_1m"`
	CachedInputUSDPer1M float64  `json:"cached_input_usd_per_1m,omitempty"`
	BlendedUSDPer1M     *float64 `json:"blended_usd_per_1m,omitempty"`
	Known               bool     `json:"known"`
	Cheapest            bool     `json:"cheapest"`
	Source              string   `json:"source"`
}

type autoRouteEnriched struct {
	autorouter.Decision
	ModelCostUSDPer1M   *float64         `json:"model_cost_usd_per_1m,omitempty"`
	ModelPriceKnown     bool             `json:"model_price_known"`
	ModelPrices         []modelPairPrice `json:"model_prices,omitempty"`
	ModelQuality        *float64         `json:"model_quality,omitempty"`
	ModelQualitySamples int              `json:"model_quality_samples"`
}

// enrichDecision attaches the resolved model's real cost (price
// book) and measured quality (ledger aggregate) to a decision. A
// decision with no resolved model carries unknown cost/quality.
// ModelPrices lists the TRUE price of the resolved model at EVERY
// provider offering it — never just the cheapest pair: the
// cheapest is marked, the rest are shown with their own prices
// (or as unknown), so the explain view can say what each serving
// of the model actually costs.
func (s *Server) enrichDecision(r *http.Request, dec autorouter.Decision) autoRouteEnriched {
	out := autoRouteEnriched{Decision: dec}
	if dec.Model == "" {
		return out
	}
	if cost, known := s.modelCostUSDPer1M(dec.Model); known {
		out.ModelCostUSDPer1M = cost
		out.ModelPriceKnown = true
	}
	out.ModelPrices = s.modelPairPrices(dec.Model)
	if q := s.modelQualityMap(r.Context()); q != nil {
		if mq, ok := q[dec.Model]; ok {
			out.ModelQuality = mq.Rate
			out.ModelQualitySamples = mq.Samples
		}
	}
	return out
}

// modelPairPrices lists every provider offering model, with that
// provider's true price for it (pair price, model-level fallback,
// provider-reported, or unknown — same resolution as the Models
// feed). Known blended prices sort ascending (cheapest first, the
// first marked Cheapest); unknown pairs sort last by provider
// name. With no registry lister the result is nil and the explain
// view falls back to the single cheapest cost.
func (s *Server) modelPairPrices(model string) []modelPairPrice {
	if s.registry == nil {
		return nil
	}
	ml, ok := s.registry.(dashboardModelLister)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var pairs []modelPairPrice
	for _, m := range ml.DashboardModels() {
		if m.ID != model || seen[m.ProviderID] {
			continue
		}
		seen[m.ProviderID] = true
		row := modelPairPrice{Provider: m.ProviderID, ProviderName: m.ProviderName}
		if p, src := s.pairPrice(m.ProviderID, model); src != "" {
			row.InputUSDPer1M = priceOrZero(p.InputUSDPer1M)
			row.OutputUSDPer1M = priceOrZero(p.OutputUSDPer1M)
			row.CachedInputUSDPer1M = priceOrZero(p.CachedInputUSDPer1M)
			row.Source = src
			if b, ok := p.BlendedUSDPer1M(); ok {
				row.BlendedUSDPer1M = &b
				row.Known = true
			} else if p.Known() {
				// Half-known pair price (one side only): the
				// sides are shown, but no blended price is
				// invented from one side.
				row.Known = true
			}
		} else if m.InputPriceUSD > 0 || m.OutputPriceUSD > 0 {
			row.InputUSDPer1M = m.InputPriceUSD
			row.OutputUSDPer1M = m.OutputPriceUSD
			row.Source = "provider"
			row.Known = true
			b := (m.InputPriceUSD + m.OutputPriceUSD) / 2
			row.BlendedUSDPer1M = &b
		} else {
			row.Source = "unknown"
		}
		pairs = append(pairs, row)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Known != pairs[j].Known {
			return pairs[i].Known
		}
		if pairs[i].BlendedUSDPer1M != nil && pairs[j].BlendedUSDPer1M != nil {
			return *pairs[i].BlendedUSDPer1M < *pairs[j].BlendedUSDPer1M
		}
		return pairs[i].ProviderName < pairs[j].ProviderName
	})
	// Sorted cheapest-known first, so the first pair with a
	// blended price is the cheapest.
	for i := range pairs {
		if pairs[i].BlendedUSDPer1M != nil {
			pairs[i].Cheapest = true
			break
		}
	}
	return pairs
}

// handleAutoRoute serves GET /dashboard/auto-route?model=auto&prompt=...
// [&threshold=0.5]: it classifies the supplied prompt with the
// server's auto-router and returns the live Decision JSON (tier,
// confidence, per-tier scores, threshold, fallback + reason,
// resolved concrete model, measured decision latency). The threshold
// query parameter is a one-off preview override for this decision
// only; it never changes the router's configured threshold. A
// missing or empty prompt is a 400; an unknown virtual model is a
// 400; no concrete model configured for the picked tier is a 502
// with the decision attached, so the dashboard can show exactly why.
func (s *Server) handleAutoRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	q := r.URL.Query()
	model := q.Get("model")
	if model == "" {
		model = "auto"
	}
	prompt := strings.TrimSpace(q.Get("prompt"))
	if prompt == "" && !autorouter.IsIntentModel(model) {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	// An intent request names no prompt to classify — the tier
	// IS the routing instruction — so an empty prompt still
	// resolves (or fails honestly naming the tier) below.
	var threshold float32
	if raw := q.Get("threshold"); raw != "" {
		v, err := strconv.ParseFloat(raw, 32)
		if err != nil || v <= 0 || v > 1 {
			writeError(w, http.StatusBadRequest, "threshold must be a number in (0, 1]")
			return
		}
		threshold = float32(v)
	}
	_, dec, ok := s.autoRouter.ResolveWithThreshold(model, prompt, threshold)
	if !ok && dec.Model == "" && !autorouter.IsVirtualModel(model) {
		writeError(w, http.StatusBadRequest, "unknown virtual model "+model)
		return
	}
	if !ok {
		// No concrete model configured for the picked tier: the
		// decision itself is the honest answer — report it with a
		// 502 so the dashboard shows why instead of a bare error.
		// AUD-CODE-002: encode before committing the 502, so an
		// unmarshalable decision cannot answer with an empty body.
		w.Header().Set("Content-Type", "application/json")
		buf, err := json.Marshal(s.enrichDecision(r, dec))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "decision could not be encoded")
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write(buf)
		return
	}
	writeJSON(w, http.StatusOK, s.enrichDecision(r, dec))
}

// handleAutoRouteLast serves GET /dashboard/auto-route/last: the
// routing decision of the most recent real "auto/*" request through
// this server (or {"has_decision": false} when there has not been
// one yet — an empty state, never invented traffic).
func (s *Server) handleAutoRouteLast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dec, ok := s.lastAutoDecision()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"has_decision": false, "decision": dec})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_decision": true, "decision": s.enrichDecision(r, dec)})
}

// handleAutoComboCandidates serves GET
// /v1/auto-combo/{channel}/candidates (Phase D, builder D1): the
// read-only candidate-pool listing for one auto/* channel. The
// pool comes from buildAutoComboPool (autocombo_route.go) — live
// tier models, per-key connections, breaker and cooldown state —
// and the pure listing from ListAutoComboCandidates. The caller's
// per-key exclusions come from the overrides store
// (autoComboExcludedIDs); a corrupt overrides file answers 503
// rather than listing rows with wrong `excluded` flags. Unknown
// channel -> 404, malformed channel -> 400, no auto-router -> 503.
func (s *Server) handleAutoComboCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return
	}
	channel := r.PathValue("channel")
	pool, err := s.buildAutoComboPool(channel)
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, "cannot build candidate pool: "+err.Error())
		return
	}
	fullID := channel
	if channel != "auto" {
		fullID = "auto/" + channel
	}
	var excluded map[string]bool
	if keyID := s.callerKeyID(r); keyID != "" {
		excluded, err = s.autoComboExcludedIDs(keyID, fullID)
		if err != nil {
			writeServerError(w, http.StatusServiceUnavailable, "cannot load candidate overrides: "+err.Error())
			return
		}
	}
	res, err := ListAutoComboCandidates(AutoComboCandidatesInput{Channel: channel, Pool: pool, ExcludedConnectionIDs: excluded})
	if err != nil {
		switch {
		case errors.Is(err, ErrUnknownAutoChannel):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, ErrInvalidAutoChannel):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeServerError(w, http.StatusInternalServerError, "could not list candidates")
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// lastAutoDecision reads the most recent auto-routing decision from
// the engine when it reports one.
func (s *Server) lastAutoDecision() (autorouter.Decision, bool) {
	if ad, ok := s.engine.(autoDecisionReporter); ok {
		return ad.LastAutoDecision()
	}
	return autorouter.Decision{}, false
}

// requestTrace returns this request's route trace when the engine
// published one into the request's attribution sink (CHECKLIST
// 317-318): the sink holds exactly this request's trace, so the
// outcome is attributed to its own attempts even when a neighbour
// finishes first. Without a sink (older engines, test doubles,
// cache replays that never called the engine) it falls back to
// the engine-global most-recent trace — the old approximate read,
// where a neighbour's trace may leak in. Callers that use the
// fallback must treat it as telemetry, never billing data (the
// ledger is the billing record).
func (s *Server) requestTrace(ctx context.Context) *routetrace.RouteTrace {
	if att := router.AttributionFrom(ctx); att != nil {
		if tr := att.Trace(); tr != nil {
			return tr
		}
	}
	if tp, ok := s.engine.(traceProvider); ok {
		return tp.LastTrace()
	}
	return nil
}

// requestDecision returns this request's auto-routing decision
// when the engine published one into the request's attribution
// sink (CHECKLIST 317-318); otherwise the engine-global
// most-recent decision (the old approximate read — a neighbour's
// decision may leak in).
func (s *Server) requestDecision(ctx context.Context) (autorouter.Decision, bool) {
	if att := router.AttributionFrom(ctx); att != nil {
		if dec, ok := att.Decision(); ok {
			return dec, true
		}
	}
	return s.lastAutoDecision()
}

// registerAutoDecision records a served auto-routing decision in
// the feedback store (if wired), so a later POST /dashboard/feedback
// can attach a signal to it. Best-effort and silent: feedback must
// never fail a served request. Registration is also what makes a
// decision id meaningful — an id that was never registered is
// rejected by the feedback endpoint, so dashboard preview
// classifications (classified but never served, never registered)
// can never train the router.
func (s *Server) registerAutoDecision(dec autorouter.Decision) {
	if s.feedback == nil || dec.ID == "" {
		return
	}
	scores := make(map[string]float64, len(dec.Scores))
	for tier, v := range dec.Scores {
		scores[tier] = float64(v)
	}
	_ = s.feedback.RegisterDecision(context.Background(), feedback.DecisionRef{
		ID: dec.ID, Tier: string(dec.Tier), Model: dec.Model, LatencyMicros: dec.LatencyMicros,
		// Raw per-tier scores (metadata only): the A/B evaluation
		// replays these under both policies. A decision without
		// scores can never be evaluated.
		TierScores: scores,
	})
}

// ---------------------------------------------------------------------------
// Feedback-learned routing weights (GET /dashboard/feedback,
// POST /dashboard/feedback).
//
// Both endpoints are intentionally NOT auth-gated, like /dashboard
// and the other dashboard feeds: GET reports only counts (per-tier
// signal tallies, nudges, recent decision ids with their signal —
// never prompts, answers or message content, because none is ever
// stored), and POST records one signal (decision id + signal only;
// the tier/model always come from the registered decision row, so
// a client cannot invent routing facts). With no feedback store
// configured they answer 503, and a POST naming a decision this
// router never served answers 404 — a dashboard preview decision
// is classified, not served, so it is never registered and its id
// is unknown here by design.
// ---------------------------------------------------------------------------

// feedbackRequest is the POST /dashboard/feedback body: a decision
// id and a signal. Nothing else is accepted — in particular there
// is no field for a prompt, an answer, or a tier/model.
type feedbackRequest struct {
	DecisionID string `json:"decision_id"`
	Signal     string `json:"signal"`
}

// maxFeedbackBody caps a feedback POST body (4 KiB): a decision id
// plus a signal is a few dozen bytes; anything larger is not
// feedback.
const maxFeedbackBody = 4 << 10

// handleFeedback serves GET /dashboard/feedback (the learned-weights
// summary) and POST /dashboard/feedback (record one signal).
func (s *Server) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if s.feedback == nil {
		writeError(w, http.StatusServiceUnavailable, "feedback store is not configured")
		return
	}
	switch r.Method {
	case http.MethodGet:
		sum, err := s.feedback.Summary(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "feedback summary unavailable")
			return
		}
		writeJSON(w, http.StatusOK, sum)
	case http.MethodPost:
		s.handleFeedbackPost(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleFeedbackPost records one accept / edit / discard /
// regenerate signal against a served decision id. A new signal
// answers 201; repeating the same signal for the same decision
// answers 200 with the stored event (idempotent retry); a
// different signal for the same decision answers 409 (one
// decision, one outcome); an unknown decision id answers 404; a
// bad signal or decision id answers 400.
func (s *Server) handleFeedbackPost(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFeedbackBody)
	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "empty request body")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	req.DecisionID = strings.TrimSpace(req.DecisionID)
	if req.DecisionID == "" || len(req.DecisionID) > 128 {
		writeError(w, http.StatusBadRequest, "decision_id is required (at most 128 characters)")
		return
	}
	sig, err := feedback.ParseSignal(req.Signal)
	if err != nil {
		// The error echoes the caller's signal value; redact it
		// before it reaches the client — a client must never get
		// its own input bounced back unredacted.
		writeError(w, http.StatusBadRequest, guardrails.RedactLogLine(err.Error()))
		return
	}
	ev, dup, err := s.feedback.RecordSignal(r.Context(), req.DecisionID, sig)
	if err != nil {
		switch {
		case errors.Is(err, feedback.ErrUnknownDecision):
			writeError(w, http.StatusNotFound, "unknown decision id: only decisions served by this router on a real /v1 request accept feedback")
		case errors.Is(err, feedback.ErrSignalConflict):
			writeError(w, http.StatusConflict, "decision already has a different signal recorded")
		default:
			writeError(w, http.StatusInternalServerError, "feedback could not be recorded")
		}
		return
	}
	// A new signal is new evidence for the learned router: the
	// refresher retrains on the next read (a duplicate signal
	// changes nothing, but invalidating on it too is harmless —
	// the retrain is cached for 30 s either way).
	if s.learned != nil {
		s.learned.Invalidate()
	}
	if dup {
		writeJSON(w, http.StatusOK, ev)
		return
	}
	// A new signal is also new evidence for the auto/code mode:
	// the OutcomeStore tallies it per model (coding-tier
	// decisions only — Record enforces that; a duplicate never
	// reaches here, so one decision contributes exactly one
	// outcome). No content is involved: the event carries only
	// decision id, tier, model and signal.
	if s.codeOutcomes != nil {
		s.codeOutcomes.Record(ev.Model, ev.Tier, string(ev.Signal))
	}
	writeJSON(w, http.StatusCreated, ev)
}

// ---------------------------------------------------------------------------
// Learned auto-router status (GET /dashboard/learned, items 181-182).
//
// Auth-exempt like the other dashboard feeds: it reports only
// counts and the calibration measurement — status, evidence,
// per-tier sample counts, calibration error, log-loss — never
// prompts, answers or message content, because none is ever
// stored. With no feedback store (or no auto-router) it answers
// 503. A new signal invalidates the refresher (see
// handleFeedbackPost), so the next read retrains; reads are
// otherwise served from the 30 s cache, so the dashboard poll
// cannot hammer SQLite.
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Auto Code evidence (GET /dashboard/code-outcomes, items 247–248,
// persisted by item 265).
//
// Auth-exempt like the other dashboard feeds: it reports only the
// auto/code mode's recorded per-model coding-task evidence —
// model, labelled outcome count, solved credit, measured solve
// rate (or the documented neutral default when below the
// evidence floor) — counts only, never prompts, answers or
// message content, because none is ever stored. With nothing
// recorded it answers an empty list: an honest empty state,
// never invented traffic. The tally is rebuilt from the
// persisted feedback store at startup (item 265) and the
// response says so (persisted=true); without a feedback store
// it is this run only and persisted=false.
// ---------------------------------------------------------------------------

// codeOutcomeRow is one model's row in the code-outcomes feed.
type codeOutcomeRow struct {
	Model string `json:"model"`
	// Attempts is the labelled coding-outcome count for the model.
	Attempts int `json:"attempts"`
	// Solved is the summed solved credit (accept 1, edit 0.5,
	// discard/regenerate 0).
	Solved float64 `json:"solved"`
	// SolveRate is the measured rate when Evidence is true, the
	// documented neutral default otherwise.
	SolveRate float64 `json:"solve_rate"`
	// Evidence reports whether SolveRate is measured (at least
	// MinCodeSamples labelled outcomes).
	Evidence bool `json:"evidence"`
	// MinSamples is the evidence floor (MinCodeSamples), so the
	// dashboard can say how far a model is from a measured rate.
	MinSamples int `json:"min_samples"`
}

// codeOutcomesResponse is the GET /dashboard/code-outcomes payload.
type codeOutcomesResponse struct {
	Models []codeOutcomeRow `json:"models"`
	// DefaultSolveRate is the neutral rate unevidenced models rank
	// at (documented default, not a measured rate).
	DefaultSolveRate float64 `json:"default_solve_rate"`
	// MinSamples is the per-model evidence floor.
	MinSamples int `json:"min_samples"`
	// Persisted reports whether the tally was rebuilt from the
	// persisted feedback store at startup (item 265): true
	// means the rates survive a restart, false means the tally
	// is this run only.
	Persisted bool `json:"persisted"`
}

// handleCodeOutcomes serves GET /dashboard/code-outcomes: the
// auto/code mode's live per-model coding-task evidence, best
// solve rate first (models at the default sort by attempts, so
// the closest to evidence leads). A model absent from the list
// has no recorded outcomes — it ranks at the documented default
// when it competes, never at an invented rate.
func (s *Server) handleCodeOutcomes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resp := codeOutcomesResponse{Models: []codeOutcomeRow{}, DefaultSolveRate: autorouter.DefaultSolveRate, MinSamples: autorouter.MinCodeSamples, Persisted: s.codeOutcomesPersisted}
	if s.codeOutcomes != nil {
		for model, o := range s.codeOutcomes.Snapshot() {
			rate, evidence := o.SolveRate()
			resp.Models = append(resp.Models, codeOutcomeRow{
				Model: model, Attempts: o.Attempts, Solved: o.Solved,
				SolveRate: rate, Evidence: evidence, MinSamples: autorouter.MinCodeSamples,
			})
		}
		sort.Slice(resp.Models, func(i, j int) bool {
			if resp.Models[i].Evidence != resp.Models[j].Evidence {
				return resp.Models[i].Evidence
			}
			if resp.Models[i].SolveRate != resp.Models[j].SolveRate {
				return resp.Models[i].SolveRate > resp.Models[j].SolveRate
			}
			return resp.Models[i].Attempts > resp.Models[j].Attempts
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleLearned serves GET /dashboard/learned: the learned
// model's status (off / learning / active), its evidence count,
// its held-out calibration error and log-loss, and the
// per-tier sample counts — all from live data, never a fake
// accuracy. While the operator flag is off the status reads
// "learning" with a note saying so, even when the model is
// evidence-ready (see learned.Refresher.Summary).
func (s *Server) handleLearned(w http.ResponseWriter, r *http.Request) {
	if s.learned == nil {
		writeError(w, http.StatusServiceUnavailable, "learned auto-router is not configured (no feedback store or no auto-router)")
		return
	}
	writeJSON(w, http.StatusOK, s.learned.Summary(r.Context()))
}

// ---------------------------------------------------------------------------
// Single-flight coalescing (GET /dashboard/coalescing).
//
// The endpoint is intentionally NOT auth-gated, like /dashboard and
// /dashboard/providers: it reports only counters from the engine's
// single-flight group (how many requests shared a flight, how many
// upstream calls were avoided) — no keys, no message content, no
// per-request data. With an engine that reports nothing it answers
// zero counters: an honest empty state, never invented figures. The
// same shared count is also exposed as
// codvian_router_coalesced_total on the auth-gated /metrics.
// ---------------------------------------------------------------------------

// coalescingReporter is optionally implemented by engines that can
// report single-flight counters (router.Engine, and
// degrade.StaleEngine which forwards them).
type coalescingReporter interface {
	CoalescingStats() coalescing.Stats
}

// handleCoalescing serves GET /dashboard/coalescing: the live
// single-flight counters (see coalescing.Stats).
func (s *Server) handleCoalescing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var stats coalescing.Stats
	if cr, ok := s.engine.(coalescingReporter); ok {
		stats = cr.CoalescingStats()
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---------------------------------------------------------------------------
// Reasoning replay (GET /dashboard/reasoning).
//
// The endpoint is intentionally NOT auth-gated, like /dashboard and
// /dashboard/providers: it reports only counters from the engine's
// reasoning replay store (how many states were captured, how many
// turns restored one, how many lookups hit or missed, how many
// entries were evicted) — counts only, never reasoning content, no
// keys, no message content, no per-request data. With an engine
// that reports nothing it answers zero counters: an honest empty
// state, never invented figures.
// ---------------------------------------------------------------------------

// reasoningReporter is optionally implemented by engines that can
// report reasoning replay counters (router.Engine, and
// degrade.StaleEngine which forwards them).
type reasoningReporter interface {
	ReasoningStats() reasoning.Stats
}

// handleReasoning serves GET /dashboard/reasoning: the live
// reasoning replay counters (see reasoning.Stats).
func (s *Server) handleReasoning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var stats reasoning.Stats
	if rr, ok := s.engine.(reasoningReporter); ok {
		stats = rr.ReasoningStats()
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---------------------------------------------------------------------------
// Adaptive concurrency + TCP Fast Open (GET /dashboard/concurrency).
//
// The endpoint is intentionally NOT auth-gated, like /dashboard and
// /dashboard/providers: it reports only the adaptive limiter's live
// state per provider (cap, in-flight, smoothed RTT vs baseline,
// samples, admitted/shed counts) and the probed TFO state for this
// platform — no keys, no message content, no base URLs. With an
// engine that reports nothing it answers an empty provider list
// with the TFO probe: an honest empty state, never invented limits.
// The same cap is also exposed as
// codvian_router_concurrency_limit{provider} and the shed count as
// codvian_router_concurrency_rejected_total{provider} on the
// auth-gated /metrics.
// ---------------------------------------------------------------------------

// concurrencyReporter is optionally implemented by engines that can
// report adaptive limiter state (router.Engine, and
// degrade.StaleEngine which forwards it).
type concurrencyReporter interface {
	ConcurrencyStats() []adaptive.State
}

// concurrencyProvider is one row of the GET /dashboard/concurrency
// payload: the provider's live limiter state, joined with its
// registry name. Samples==0 means the provider has had no completed
// call yet — its limit is the initial cap and its RTT fields are 0,
// i.e. "no traffic yet", not a measured value.
type concurrencyProvider struct {
	ProviderID string  `json:"provider_id"`
	Name       string  `json:"name"`
	Limit      float64 `json:"limit"`
	InFlight   int     `json:"in_flight"`
	RTTMs      float64 `json:"rtt_ms"`
	BaselineMs float64 `json:"baseline_ms"`
	Samples    uint64  `json:"samples"`
	Admitted   uint64  `json:"admitted"`
	Rejected   uint64  `json:"rejected"`
}

type concurrencyResponse struct {
	Providers []concurrencyProvider `json:"providers"`
	TFO       provider.TFOState     `json:"tfo"`
}

// handleConcurrency serves GET /dashboard/concurrency: the live
// per-provider adaptive concurrency state plus the probed TFO
// state. Providers come from the dashboard provider feed (so every
// configured provider has a row, with its registry name); limiter
// state is attached by provider ID, and limiter keys with no
// registry entry (e.g. a provider removed since the engine started)
// are appended under their ID.
func (s *Server) handleConcurrency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	states := []adaptive.State{}
	if cr, ok := s.engine.(concurrencyReporter); ok {
		states = cr.ConcurrencyStats()
	}
	byID := make(map[string]adaptive.State, len(states))
	for _, st := range states {
		byID[st.Provider] = st
	}
	providers := []concurrencyProvider{}
	if s.dashboardProviders != nil {
		for _, p := range s.dashboardProviders.DashboardProviders() {
			row := concurrencyProvider{ProviderID: p.ID, Name: p.Name}
			if st, ok := byID[p.ID]; ok {
				row.Limit, row.InFlight, row.RTTMs, row.BaselineMs = st.Limit, st.InFlight, st.RTTMs, st.BaselineMs
				row.Samples, row.Admitted, row.Rejected = st.Samples, st.Admitted, st.Rejected
				delete(byID, p.ID)
			} else {
				// No limiter state yet: no traffic. The limit
				// shown is the initial cap the limiter would
				// admit under — never a measured value.
				row.Limit = adaptive.DefaultInitialLimit
			}
			providers = append(providers, row)
		}
	}
	// Limiter state for providers the registry no longer lists.
	for id, st := range byID {
		providers = append(providers, concurrencyProvider{
			ProviderID: id, Name: id,
			Limit: st.Limit, InFlight: st.InFlight, RTTMs: st.RTTMs, BaselineMs: st.BaselineMs,
			Samples: st.Samples, Admitted: st.Admitted, Rejected: st.Rejected,
		})
	}
	writeJSON(w, http.StatusOK, concurrencyResponse{Providers: providers, TFO: provider.TFOStatus()})
}

// Warm HTTP/2 connection pools (GET /dashboard/pools).
//
// The endpoint is intentionally NOT auth-gated, like /dashboard and
// /dashboard/concurrency: it reports only live pool counters per
// (provider, key) — idle/total connections, requests, dials, reused
// (requests that found a warm idle connection instead of dialing) —
// plus the key fingerprint (first 12 hex of SHA-256 of the key,
// never the key) and the Little's Law sizing in force (formula +
// inputs + derived size). No keys, no message content, no base
// URLs. With no registry wired it answers an empty pool list with
// the default sizing: an honest empty state, never invented pools.
// ---------------------------------------------------------------------------

// poolRow is one row of the GET /dashboard/pools payload: the live
// pool counters joined with the provider's registry name.
type poolRow struct {
	ProviderID     string `json:"provider_id"`
	Name           string `json:"name"`
	KeyFingerprint string `json:"key_fingerprint"`
	MaxIdle        int    `json:"max_idle"`
	Idle           int    `json:"idle"`
	Total          int    `json:"total"`
	Requests       uint64 `json:"requests"`
	Dials          uint64 `json:"dials"`
	Reused         uint64 `json:"reused"`
	Warmed         bool   `json:"warmed"`
}

// poolSizing is the Little's Law derivation behind the pool size,
// served alongside the rows so the operator can see the formula,
// its inputs, and the derived size — never a magic constant.
type poolSizing struct {
	Formula           string  `json:"formula"`
	ExpectedRPS       float64 `json:"expected_rps"`
	ExpectedLatencyMs float64 `json:"expected_latency_ms"`
	DerivedSize       int     `json:"derived_size"`
	MinConns          int     `json:"min_conns"`
	MaxConns          int     `json:"max_conns"`
	PerHostOverride   int     `json:"per_host_override"`
}

type poolsResponse struct {
	Pools  []poolRow  `json:"pools"`
	Sizing poolSizing `json:"sizing"`
}

// handlePools serves GET /dashboard/pools: the live per-(provider,
// key) warm-pool state plus the sizing derivation in force.
// Providers come from the dashboard provider feed (so every pool
// has its registry name); pools for providers the registry no
// longer lists are appended under their ID.
func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	cfg := provider.PoolConfig{}
	stats := []provider.PoolStat{}
	if s.poolStats != nil {
		cfg = s.poolStats.PoolConfig()
		stats = s.poolStats.PoolStats()
	} else {
		cfg = cfg.Resolved()
	}
	names := map[string]string{}
	if s.dashboardProviders != nil {
		for _, p := range s.dashboardProviders.DashboardProviders() {
			names[p.ID] = p.Name
		}
	}
	rows := []poolRow{}
	seen := map[string]bool{}
	for _, st := range stats {
		name := names[st.ProviderID]
		if name == "" {
			name = st.ProviderID
		}
		rows = append(rows, poolRow{
			ProviderID: st.ProviderID, Name: name,
			KeyFingerprint: st.KeyFingerprint, MaxIdle: st.MaxIdle,
			Idle: st.Idle, Total: st.Total,
			Requests: st.Requests, Dials: st.Dials,
			Reused: st.Reused, Warmed: st.Warmed,
		})
		seen[st.ProviderID] = true
	}
	// Providers with no pool yet (no client built for them — e.g.
	// added but never dialed, or a registry with no traffic) still
	// get a row: the pool is created at first ClientFor, so a
	// provider with no row has genuinely never been dialed. Its
	// counters are honest zeros, not a missing pool.
	if s.dashboardProviders != nil {
		for _, p := range s.dashboardProviders.DashboardProviders() {
			if seen[p.ID] {
				continue
			}
			rows = append(rows, poolRow{
				ProviderID: p.ID, Name: p.Name,
				KeyFingerprint: "", MaxIdle: cfg.Size(),
				Idle: 0, Total: 0,
				Requests: 0, Dials: 0, Reused: 0, Warmed: false,
			})
		}
	}
	writeJSON(w, http.StatusOK, poolsResponse{
		Pools: rows,
		Sizing: poolSizing{
			Formula:           "N = ceil(expected_rps * expected_latency_ms / 1000)",
			ExpectedRPS:       cfg.ExpectedRPS,
			ExpectedLatencyMs: cfg.ExpectedLatencyMs,
			DerivedSize:       cfg.Size(),
			MinConns:          cfg.MinConns,
			MaxConns:          cfg.MaxConns,
			PerHostOverride:   cfg.MaxIdlePerHost,
		},
	})
}

// setAutoRouteHeader marks a non-streaming auto-routed response with
// the real routing decision: the tier picked, its confidence, the
// resolved model, and whether it fell back. It must be called before
// the response body is written. Streaming responses cannot carry the
// header (the decision is final only after the stream ends), so
// handleChatStream logs it instead.
//
// The decision comes from this request's attribution sink
// (CHECKLIST 317-318) — the engine published this request's own
// decision into it — so the header (and the feedback registration
// below) names this request's decision id even when a neighbour
// finished first. Without a sink it falls back to the
// engine-global most-recent decision.
func (s *Server) setAutoRouteHeader(ctx context.Context, w http.ResponseWriter) {
	dec, ok := s.requestDecision(ctx)
	if !ok {
		return
	}
	// Register the served decision so feedback can attach to it,
	// and expose its id: the dashboard's feedback buttons (and any
	// client) post this id back with their signal.
	s.registerAutoDecision(dec)
	w.Header().Set("X-Codvian-Auto-Decision-Id", dec.ID)
	w.Header().Set("X-Codvian-Auto-Tier", string(dec.Tier))
	w.Header().Set("X-Codvian-Auto-Confidence", fmt.Sprintf("%.3f", dec.Confidence))
	w.Header().Set("X-Codvian-Auto-Model", dec.Model)
	if dec.IntentTier != "" {
		// An intent decision names its tier (and its pinned
		// effort, when one is pinned) so the client can see
		// what the intent resolved to without guessing.
		w.Header().Set("X-Codvian-Intent-Tier", string(dec.IntentTier))
		if dec.Effort != "" {
			w.Header().Set("X-Codvian-Intent-Effort", string(dec.Effort))
		}
	}
	if dec.Fallback {
		w.Header().Set("X-Codvian-Auto-Fallback", "true")
	}
}

// handleMetrics serves the Prometheus exposition of the recorder
// plus the observability collector, in one response body (both
// are Prometheus text, so concatenation is valid). It is
// auth-gated like the rest of /v1/*: a scrape config must present
// a gateway key, because the router may be bound to a non-loopback
// address with --listen. 503 only when BOTH recorder and collector
// are nil; when only one is present, that one's exposition is
// served. Breaker state is refreshed from the engine before the
// exposition is written (no background goroutine).
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.recorder == nil && s.obsCollector == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not enabled")
		return
	}
	s.refreshBreakerStates()
	if s.recorder != nil {
		s.recorder.Handler().ServeHTTP(w, r)
	} else {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
	}
	if s.obsCollector != nil {
		s.obsCollector.WritePrometheus(w)
	}
}

// ---------------------------------------------------------------------------
// Metrics recording (best-effort telemetry; never fails a request).
// ---------------------------------------------------------------------------

// traceProvider is optionally implemented by engines that expose the
// RouteTrace of the most recently finished request (router.Engine,
// and degrade.StaleEngine which forwards it).
type traceProvider interface {
	LastTrace() *routetrace.RouteTrace
}

// refusalChainStatsProvider is optionally implemented by engines
// that expose live content-policy refusal-chain outcomes (see
// router.RefusalChainStats, CHECKLIST 161): refusals seen,
// requests served by a refusal-chain hop, requests that ended in
// an honest refusal, and the most recent refusing attempt.
type refusalChainStatsProvider interface {
	RefusalChainStats() router.RefusalChainStats
}

// failover243StatsProvider is optionally implemented by engines
// that expose live CHECKLIST 243 failover outcomes (see
// router.Failover243Stats): pin escapes after a retryable failure
// on the pinned deployment, and tool-call replays suppressed on
// resumed streams.
type failover243StatsProvider interface {
	Failover243Stats() router.Failover243Stats
}

// failover257StatsProvider is optionally implemented by engines
// that expose live CHECKLIST 257 region-aware failover outcomes
// (see router.Failover257Stats): requests that stepped to each
// region tier — same model in another region, equivalent model,
// self-hosted — after a retryable failure.
type failover257StatsProvider interface {
	Failover257Stats() router.Failover257Stats
}

// contextFitProvider is optionally implemented by engines that
// expose the context-window fit-check decision of the most recent
// request (router.Engine, and degrade.StaleEngine which forwards
// it). The decision is the last request's fit verdict — prompt
// estimate, reserve, required tokens, per-model fit — never a
// summary or a sample.
type contextFitProvider interface {
	LastContextFit() (router.ContextFitDecision, bool)
}

// lastContextFit returns the engine's most recent fit-check
// decision, and whether there is one (false before any request
// reached the fit gate, or when the engine does not expose it).
func (s *Server) lastContextFit() (router.ContextFitDecision, bool) {
	if cf, ok := s.engine.(contextFitProvider); ok {
		return cf.LastContextFit()
	}
	return router.ContextFitDecision{}, false
}

// staleReporter is optionally implemented by engines that can report
// whether the most recent call served a stale cached response
// (degrade.StaleEngine).
type staleReporter interface {
	LastServedStale() bool
	LastStaleAge() time.Duration
}

// cacheHitReporter is optionally implemented by engines that can
// report whether the most recent call was served proactively from a
// cache without calling any provider (degrade.StaleEngine with
// Policy.ServeCache). kind is "exact" or "semantic"; similarity is
// the semantic cosine similarity (0 for exact hits).
type cacheHitReporter interface {
	LastCacheHit() (kind string, similarity float32, ok bool)
}

// recordMetrics records one finished chat request when a recorder is
// configured. Provider/model come from the request's own route trace
// when available (the last attempt is the winner on success, the last
// tried on failure), falling back to the request model and "unknown"
// provider. Failovers — a non-success attempt followed by another
// attempt — are counted from the trace. The trace is this request's
// own (CHECKLIST 317-318: read from the attribution sink on ctx,
// falling back to the engine-global most-recent trace only when no
// sink was attached), so under concurrency a request is attributed
// to its own attempts, not a neighbour's; the fallback counters are
// approximate telemetry, not billing data (the ledger is the
// billing record).
func (s *Server) recordMetrics(ctx context.Context, req provider.ChatRequest, resp *provider.ChatResponse, err error, started time.Time) {
	// Metadata-only learning loop (CHECKLIST 259-260): the same
	// finished request feeds the per-(task, model) outcome
	// stats. It runs first, before the recorder's nil check —
	// the loop must not depend on metrics being enabled.
	s.recordIntentOutcome(ctx, req, resp, err, started)
	rec := s.recorder
	if rec == nil {
		return
	}
	// A proactive cache hit called no provider: attribute the
	// request to the cache, not to whatever trace the previous
	// request left behind (which would misattribute it to a
	// neighbour's provider and invent a failover that never
	// happened). The ledger is untouched — it only ever sees real
	// provider attempts, so a cache hit bills nobody.
	if chr, ok := s.engine.(cacheHitReporter); ok {
		if _, _, hit := chr.LastCacheHit(); hit {
			// Hit/miss counters live with the wrapper that owns
			// the lookup (degrade.StaleEngine via SetCacheMetrics),
			// not here — counting here too would double-count
			// every hit. The server only attributes the request.
			rec.IncRequest("cache", req.Model, "ok")
			rec.ObserveLatency("cache", req.Model, float64(time.Since(started).Milliseconds()))
			return
		}
	}
	providerName, model := "unknown", req.Model
	if trace := s.requestTrace(ctx); trace != nil && len(trace.Attempts) > 0 {
		last := trace.Attempts[len(trace.Attempts)-1]
		providerName, model = last.Provider, last.Model
		for i := 0; i+1 < len(trace.Attempts); i++ {
			if trace.Attempts[i].Outcome != "success" {
				rec.IncFailover(trace.Attempts[i].Provider, trace.Attempts[i+1].Provider)
			}
		}
	}
	if resp != nil && resp.Model != "" {
		model = resp.Model
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
		if errors.Is(err, context.DeadlineExceeded) {
			outcome = "timeout"
		}
	}
	rec.IncRequest(providerName, model, outcome)
	rec.ObserveLatency(providerName, model, float64(time.Since(started).Milliseconds()))

	// Webhook events: request.completed / request.failed / provider.exhausted.
	// Best-effort, async — a webhook failure never fails the request.
	if s.webhooks != nil {
		if errors.Is(err, router.ErrNoAvailableAttempt) {
			s.webhooks.Notify(webhooks.ProviderExhaustedEvent, map[string]any{
				"model":    model,
				"provider": providerName,
			})
		} else if err == nil {
			s.webhooks.Notify(webhooks.RequestCompletedEvent, map[string]any{
				"model":      model,
				"provider":   providerName,
				"latency_ms": time.Since(started).Milliseconds(),
			})
		} else {
			s.webhooks.Notify(webhooks.RequestFailedEvent, map[string]any{
				"model":    model,
				"provider": providerName,
				"error":    err.Error(),
				"outcome":  outcome,
			})
		}
	}
}

// setStaleHeader marks a non-streaming response as stale when the
// engine reports it served a stale cached response. It must be called
// before the response body is written. Streaming responses cannot
// carry the header (staleness is known only after the stream ends),
// so handleChatStream logs it instead.
func (s *Server) setStaleHeader(w http.ResponseWriter) {
	if sr, ok := s.engine.(staleReporter); ok && sr.LastServedStale() {
		w.Header().Set("X-Codvian-Stale", "true")
		w.Header().Set("X-Codvian-Stale-Age", sr.LastStaleAge().String())
	}
}

// setCacheHeader marks a non-streaming response that was served
// proactively from a cache, so the client never mistakes it for a
// fresh provider response: X-Codvian-Cache is "exact" or "semantic",
// and a semantic hit also carries its cosine similarity. It must be
// called before the response body is written. Streaming responses
// cannot carry the header (the hit is known only after the stream
// ends), so handleChatStream logs it instead.
func (s *Server) setCacheHeader(w http.ResponseWriter) {
	chr, ok := s.engine.(cacheHitReporter)
	if !ok {
		return
	}
	kind, sim, hit := chr.LastCacheHit()
	if !hit {
		return
	}
	w.Header().Set("X-Codvian-Cache", kind)
	if kind == "semantic" {
		w.Header().Set("X-Codvian-Cache-Similarity", fmt.Sprintf("%.3f", sim))
	}
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var models []Model
	if s.registry != nil {
		var err error
		models, err = s.registry.Models(ctx)
		if err != nil {
			s.logger.Printf("models registry error: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusBadGateway, "upstream provider error")
			return
		}
	} else {
		for _, id := range s.models {
			models = append(models, Model{ID: id, Object: "model", OwnedBy: "codvian-router"})
		}
	}
	if models == nil {
		models = []Model{}
	}
	// The auto-router's virtual models are real request targets
	// (the engine resolves them to concrete models), so list them
	// for discovery — owned by "codvian-router", never by a
	// provider that does not offer them.
	if s.autoRouter != nil {
		for _, name := range autorouter.VirtualModels {
			models = append(models, Model{ID: name, Object: "model", OwnedBy: "codvian-router"})
		}
		// Intent tiers are real request targets too (the engine
		// resolves them to their operator-defined pair), so list
		// them for discovery alongside the auto/* names.
		for _, name := range autorouter.IntentVirtualModelNames() {
			models = append(models, Model{ID: name, Object: "model", OwnedBy: "codvian-router"})
		}
	}
	// Deprecated models are hidden by default (research-log entry
	// 179): ?include=deprecated lists them again, with
	// deprecated:true on the row. Hiding is display-only — a request
	// naming a deprecated model is still forwarded, never blocked.
	if r.URL.Query().Get("include") != "deprecated" {
		kept := models[:0]
		for _, m := range models {
			if !m.Deprecated {
				kept = append(kept, m)
			}
		}
		// Clear the tail so the hidden rows are not reachable
		// through the shared backing array.
		for i := len(kept); i < len(models); i++ {
			models[i] = Model{}
		}
		models = kept
	}
	// Catalog filters + shortlist (CHECKLIST 200): provider /
	// category / max_price / min_context / shortlist. A malformed
	// filter answers 400 naming the parameter, never a silently
	// unfiltered list. Shortlisted rows sort first, marked
	// shortlisted.
	mf, ferr := parseModelFilters(r)
	if ferr != nil {
		writeError(w, http.StatusBadRequest, ferr.Error())
		return
	}
	models = s.filterV1Models(models, mf)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

// getCCRStore returns the CCR retrieval-marker store, opening
// it on first use from <cfg.DataDir>/ccr.db (see
// compress.OpenCCRStore). Nil when cfg is nil or has no data
// dir, or when the open failed (remembered in ccrErr) — a nil
// store skips the CCR engine, never an error.
func (s *Server) getCCRStore() *compress.CCRStore {
	s.ccrMu.Lock()
	defer s.ccrMu.Unlock()
	if s.ccrStore != nil || s.ccrErr != nil {
		return s.ccrStore
	}
	if s.cfg == nil || s.cfg.DataDir == "" {
		return nil
	}
	store, err := compress.OpenCCRStore(filepath.Join(s.cfg.DataDir, "ccr.db"))
	if err != nil {
		s.ccrErr = err
		return nil
	}
	s.ccrStore = store
	return store
}

// Close releases the server-owned file handles: the lazy CCR
// store (ccr.db, opened on first use by getCCRStore). The
// ledger is injected via WithLedger and owned by its caller, so
// it is NOT closed here. Close is idempotent and safe to call
// on a server that never opened the store. Tests must call it
// (newTestServer registers it via t.Cleanup): on Windows an
// open ccr.db blocks t.TempDir cleanup, failing the test at
// teardown even when every assertion passed.

// RegisterBackgroundWorker registers a long-lived background worker by name
// with its cancel function, so StopAllBackgroundWorkers can stop all workers
// at once. Registering the same name twice replaces the previous entry
// (the old cancel is NOT called — callers must not double-register).
// P2-BG-003: a centralized shutdown registry replaces ad-hoc Stop/Close calls,
// so no goroutine can leak because a caller forgot to stop it.
func (s *Server) RegisterBackgroundWorker(name string, cancel context.CancelFunc) {
	s.bgWorkersMu.Lock()
	defer s.bgWorkersMu.Unlock()
	if s.bgWorkers == nil {
		s.bgWorkers = make(map[string]context.CancelFunc)
	}
	s.bgWorkers[name] = cancel
}

// StopAllBackgroundWorkers cancels every registered background worker
// and clears the registry. It is called by Close to ensure no goroutine
// outlives the server. Workers that were already stopped are no-ops.
func (s *Server) StopAllBackgroundWorkers() {
	s.bgWorkersMu.Lock()
	workers := make(map[string]context.CancelFunc, len(s.bgWorkers))
	for name, cancel := range s.bgWorkers {
		workers[name] = cancel
	}
	s.bgWorkers = make(map[string]context.CancelFunc)
	s.bgWorkersMu.Unlock()
	for name, cancel := range workers {
		cancel()
		s.logger.Printf("background worker stopped: %s", name)
	}
}

func (s *Server) Close() error {
	var errs []error
	// P2-BG-003: stop all registered background workers first so no
	// goroutine touches stores or other state after Close begins.
	s.StopAllBackgroundWorkers()
	s.ccrMu.Lock()
	store := s.ccrStore
	s.ccrStore = nil
	s.ccrMu.Unlock()
	if store != nil {
		if err := store.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	// P3-B: stop the scheduled-evaluation loop before anything
	// that closes persisted state (the loop only reads the store).
	s.stopEvalScheduler()
	// A2A: stop the prune loop first — Stop waits for its
	// goroutine to exit, so no prune touches the store after
	// it closes — then close the lazily-opened task store. A
	// store left open keeps <data-dir>/a2a/tasks.db locked,
	// failing TempDir cleanup on Windows even when every
	// assertion passed (same pattern as the CCR store above).
	// Close is idempotent: a later a2aService call re-opens
	// a fresh store and loop.
	s.a2aStopPruneLoop()
	s.a2aMu.Lock()
	svc := s.a2aSvc
	s.a2aSvc = nil
	s.a2aMu.Unlock()
	if svc != nil {
		if st := svc.Store(); st != nil {
			if err := st.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// A1: close the request-level audit store too — otherwise the
	// SQLite handle stays open past shutdown (a resource leak in
	// production, and on Windows it keeps <data-dir>/audit.db
	// locked so the process never fully releases the file).
	s.auditMu.Lock()
	audit := s.auditStore
	s.auditStore = nil
	s.auditMu.Unlock()
	if audit != nil {
		if err := audit.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// applyCompressionPrePass runs the registry pre-pass engines the
// server path owns (B4-21…26 server wiring), at the same point
// as applyCompression — after guardrails/plugins, before every
// budget gate — and in stack-priority order (session-dedup 3,
// CCR 4, Codex 12), the same order ApplyWithEngines uses, so
// nothing runs twice: Apply never runs these three engines
// itself. Session-dedup (B4-23) and CCR (B4-24) are lossless
// and run whenever their input exists: a session id from the
// X-Codvian-Session header (absent = no session, dedup
// skipped — a session id must come from the client, one id per
// conversation across turns; deriving one per request would
// make every request a new session, and deriving one from the
// key would conflate users), and the CCR store whenever it is
// open. Codex (B4-26) is lossy: it runs only under the lossy
// opt-in (LossyMaxPromptTokens > 0) and only when the client
// names a budget via X-Codvian-Codex-Max-Chars (<= 0 = ANSI
// strip + duplicate-line collapse, no truncation; unparseable
// = no Codex pass, never a failed request). A nil store or an
// absent header skips that engine, never an error.
//
// NOT wired here: nothing — the RTK level HTTP field is wired
// since S1-RTK-FIELD: X-Codvian-RTK-Level names the RTK filter
// level for this request (minimal / standard / aggressive); a
// valid value is stashed on req.Compression (body field
// rtk_level) when the request already opts into compression,
// and applyCompression carries it into ApplyWithOptions as
// opts.RTKLevel. An absent header changes nothing; an
// unparseable value is ignored, never a failed request —
// exactly like the Codex budget header above. A request with
// no compression opt-in gets no RTK step at any level: the
// level only selects how hard RTK filters, it never enables
// lossy by itself.
func (s *Server) applyCompressionPrePass(r *http.Request, req *provider.ChatRequest) {
	// OPTION A (LossyExactHit): snapshot the request as the client
	// sent it BEFORE any pre-pass engine mutates Messages. The
	// exact-cache key is derived from this snapshot (see
	// provider.ChatRequest.PreCompression and
	// degrade.scopedKey): the CCR store is stateful, so two
	// identical requests reach the engine with different
	// messages, and keying on the messages as sent makes the
	// repeat miss. Taken once — a second call leaves the first
	// snapshot in place, so the key still reflects the original
	// request, never an already-mutated one.
	if req.PreCompression == nil {
		req.PreCompression = req.SnapshotPreCompression()
	}
	if sessionID := strings.TrimSpace(r.Header.Get("X-Codvian-Session")); sessionID != "" {
		req.Messages = compress.DeduplicateSession(req.Messages, s.dedupStore, sessionID)
	}
	if store := s.getCCRStore(); store != nil {
		req.Messages = compress.ApplyCCR(req.Messages, store)
	}
	if req.Compression != nil {
		if v := strings.TrimSpace(r.Header.Get("X-Codvian-RTK-Level")); v != "" {
			if level, ok := compress.ParseRTKLevel(v); ok && level != "" {
				req.Compression.RTKLevel = string(level)
			}
		}
	}
	if req.Compression != nil && req.Compression.LossyMaxPromptTokens > 0 {
		if v := strings.TrimSpace(r.Header.Get("X-Codvian-Codex-Max-Chars")); v != "" {
			if maxChars, err := strconv.Atoi(v); err == nil {
				req.Messages, _ = compress.CompressCodexResponses(req.Messages, maxChars)
			}
		}
	}
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	handlerStart := time.Now()
	var req provider.ChatRequest
	engineCalled := false
	responded := false
	// Panic isolation (CHECKLIST 165-166): a panic anywhere on
	// this request's path — a provider client, a chunk converter,
	// a hook, the MCP loop, this handler's own code — must never
	// kill the gateway process. The engine recovers its own
	// panics first (provider/hook → *router.PanicError, counted
	// there); this is the backstop for panics that escape it.
	// The client gets an honest 502 (a terminal SSE error frame
	// belongs to handleChatStream, which has its own recover —
	// this one writes a 502 only while no stream response could
	// have been committed), the panic is counted + logged
	// redacted (noteServerPanic), and a ledger row is recorded
	// when the engine recorded none: a panic before any provider
	// was called (!engineCalled), or a panic the engine could
	// not record (its trace is empty). A panic after the engine
	// recorded attempts adds no row — the attempt rows are
	// already there. The ledger write uses a fresh background
	// context: this recover runs during unwind, after the
	// request context's cancel has already fired.
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_chat", v)
			if !responded && (!req.Stream || !engineCalled) {
				s.writePanicFailure(w)
			}
			if req.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), req, handlerStart)
				rowCancel()
			}
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// Per-request routing contract (CHECKLIST 157): the canonical
	// spelling is the "routing" request-body field. A client that
	// sends the contract as an X-Codvian-Routing header instead
	// gets a 400 naming the right spelling — the header form is
	// rejected outright, never silently ignored (a silently
	// ignored header would make the client believe its only/
	// exclude/max_price applied when nothing did).
	if r.Header.Get("X-Codvian-Routing") != "" {
		writeError(w, http.StatusBadRequest, "X-Codvian-Routing header is not supported; send the routing contract in the \"routing\" request-body field")
		return
	}

	// Admission before I/O (CHECKLIST 243, LiteLLM #51428): a
	// blocked or over-limit key is refused BEFORE the request
	// body is read — the budget cap and the requests-per-minute
	// ceiling need only the key, never the body. A key admitted
	// here is still re-checked in full after decode (the token
	// axis and the unpriced-model check need the body); this
	// only ever refuses early.
	if s.preBodyAdmission(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	// B4-19 Idempotency-Key: the raw body is the request identity
	// (idempotencyRequestHash covers method + path + raw bytes),
	// so it is read once here and decoded from the buffer — the
	// Decoder keeps the old single-value semantics (trailing data
	// after the JSON value is ignored, exactly as before).
	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	if len(rawBody) == 0 {
		writeError(w, http.StatusBadRequest, "empty request body")
		return
	}
	if err := json.NewDecoder(bytes.NewReader(rawBody)).Decode(&req); err != nil {
		if strings.Contains(err.Error(), "invalid routing") {
			// The routing contract's own decode error already
			// names the offending field (see
			// provider.ChatRequest.UnmarshalJSON); surface it
			// verbatim so the client can fix the spelling.
			writeError(w, http.StatusBadRequest, err.Error())
		} else if strings.Contains(err.Error(), "invalid compression") {
			// Same rule for the compression opt-in: its decode
			// error names the offending field — surface it
			// verbatim so the client can fix the spelling.
			writeError(w, http.StatusBadRequest, err.Error())
		} else {
			writeError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	if msg := validateChatRequest(&req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Intent tiers (CHECKLIST 259): a request naming an
	// unmapped intent tier is refused here — before guardrails,
	// budgets and any provider call — with a 400 naming the
	// tier, never a silent default model. A mapped tier flows
	// through to the engine, which resolves it via AutoRoute.
	if refused := s.checkIntentTier(w, r, req.Model); refused {
		return
	}
	// Security cluster (CHECKLIST 245): a client-supplied Vertex
	// location/project is rejected unless the admin opted in,
	// and the deployment's pinned parameters outrank the
	// client's body fields — both before any provider is
	// dialled.
	if refused := s.checkClientVertexOverride(w, req.ClientVertexOverride); refused {
		return
	}
	s.applyDeploymentParams(w, &req)
	// Readiness gate (research-log entry 179): a gateway-provided
	// local capability that is not ready answers 503 with
	// Retry-After here, before any provider is dialled.
	if !s.checkLocalReadiness(w, r, req.Model) {
		return
	}
	// Prefixed-model pin (B2-29, parity tracker Area 2 row :600): a
	// "provider/model" model string narrows the request to that
	// provider via routing.only, so a prefixed model can never
	// fail over to another provider on the plain chat path. A
	// conflicting client-supplied routing contract is a 400
	// naming the conflict. The model is final here (after
	// applyDeploymentParams), and every gate below — guardrails,
	// budgets, compression, the engine — sees the narrowed
	// request.
	if err := s.applyPrefixedModelRouting(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Caller identity for prompt-cache affinity (CHECKLIST 131):
	// the gateway-key ID the request arrived under. The provider
	// client keys the auto prompt_cache_key on it; "" means no
	// caller could be named (auth-relaxed mode, or a verifier
	// without IDs) and no auto key is generated. The ID itself is
	// never logged, stored, or returned.
	req.CallerID = s.callerKeyID(r)
	// B4-19 Idempotency-Key (billing safety): a client that
	// retries after a timeout must not double-charge. The claim
	// runs here — after decode/validation, before every gate —
	// so a replay never touches budgets, traffic, sessions or
	// any provider. Streaming requests skip it: a stream's
	// chunks are delivered incrementally and cannot be replayed,
	// so the header is ignored on streams. A claimed key is
	// released by the deferred release on every return that
	// does not complete it (gate refusal, engine error,
	// guardrail block) — only a served 200 completes it.
	var idem idempotencyBegin
	var idemDone bool
	if !req.Stream {
		var err error
		idem, err = s.idempotencyBegin(ctx, req.CallerID, r, idempotencyRequestHash(r.Method, r.URL.Path, rawBody))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		switch idem.State {
		case IdemReplay:
			if idempotencyReplay(w, idem.Record) {
				responded = true
				return
			}
			// BodyTooLarge record: the stored response cannot
			// be replayed — the client must retry with a new key.
			writeError(w, http.StatusConflict, "idempotency key cannot be replayed (stored response too large); retry with a new Idempotency-Key")
			return
		case IdemInflight:
			writeError(w, http.StatusConflict, "idempotency key already in flight; retry with the same key once the first request completes")
			return
		case IdemConflict:
			writeError(w, http.StatusConflict, "idempotency key already used with a different request; retry with a new Idempotency-Key")
			return
		}
		// IdemProceed (or IdemNone, when no key was sent, the
		// caller is anonymous, or the store is unavailable):
		// fall through and execute. The deferred release is a
		// no-op unless this is a live IdemProceed claim.
		defer func() {
			if !idemDone {
				s.idempotencyRelease(ctx, idem)
			}
		}()
	}
	// Per-request cache controls (M6.4): bypass headers and
	// per-request cache headers, parsed fail-open (invalid values
	// ignored, never a 400) and carried on the request for the
	// engine's cache wrapper (degrade) and the relay cache. A nil
	// result means no cache header was sent: default caching.
	req.CacheControls = parseCacheControls(r)
	// The API surface this request was made to: chat completions.
	// The replay exemption (router.ReplayExemption) keys off it —
	// a chat request is replay-safe pre-commit; other surfaces
	// (embeddings, images, rerank) are not. A Responses create is
	// replay-safe pre-commit like chat, fresh or branched
	// (CHECKLIST 237); only embeddings/images/rerank/warm-ups
	// are exempt.
	req.Endpoint = provider.EndpointChatCompletions
	// Guardrail stage (internal/guardrails): the check runs on the
	// full inbound request BEFORE any provider is dialled, so a
	// denial can never leak into a fallback chain, and so streaming
	// is safe (a stream already in flight cannot be blocked). Log
	// redaction is always on; blocking is opt-in per guardrail, so
	// with every toggle off the request still flows unchanged and
	// only the log/spend copies are redacted. A blocked request is
	// refused with 403 and the policy-refusal code -32001 (see
	// writePolicyRefusal) — never with a 400, and never with an
	// upstream code: the prompt is never echoed back, and no
	// provider call, ledger row or fallback attempt is made for it.
	if blocked := s.applyGuardrails(ctx, w, &req, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks (CHECKLIST 201): after the
	// built-in guardrails and before compression, budgets, and
	// the engine — a denial or fail-closed refusal happens
	// before any provider is dialled. The ctx below carries
	// any plugin-added outbound headers to the provider client.
	if blocked, newCtx := s.applyPlugins(ctx, w, &req); blocked {
		return
	} else {
		ctx = newCtx
	}
	// Lossless-first compression (CHECKLIST 183): after
	// guardrails (which saw the original prompt) and before
	// every budget gate, so the pre-send prompt-cost checks
	// price the prompt as it will actually be sent. The engine
	// keeps this stamp for the same model; a lossy step needs
	// the client's explicit per-call opt-in and is reported on
	// the response headers, never silent. The registry pre-pass
	// (session-dedup, CCR, Codex) runs first, at the same point.
	s.applyCompressionPrePass(r, &req)
	s.applyCompression(&req)
	// Per-key access schedule (M5.10, the reference router's
	// validateKeyAccessSchedule): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched — the policy chain runs the schedule check before
	// the usage-limit check.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-request budget (CHECKLIST 155): the X-Codvian-Budget
	// header, parsed + pre-send enforced here (402 before any
	// provider is dialled when the prompt estimate alone
	// exceeds the cap), and attached to the request for the
	// engine's strict mid-chain gate. Runs after guardrails
	// (a blocked request never reaches here) and before the
	// key-budget pre-send guard below, so both caps are
	// enforced before the stream / non-stream branch.
	if refused := s.enforceRequestBudget(w, r, &req); refused {
		return
	}
	// Pre-send budget enforcement (CHECKLIST 145): count the
	// prompt locally and refuse BEFORE any provider is dialled
	// when the estimated prompt cost alone would blow a
	// fail-closed budget. It runs after guardrails (a blocked
	// request never reaches here) and before the stream /
	// non-stream branch, so it covers both. See
	// enforcePromptBudget: unpriced pairs and anonymous callers
	// skip the estimate and only the already-exhausted check
	// applies; a counting failure never fails the request.
	if refused := s.enforcePromptBudget(w, r, &req); refused {
		return
	}
	// Per-key rolling 5-hour USD budget (CHECKLIST 139-140):
	// the window cap + unpriced-usage policy, enforced BEFORE
	// any provider is called. It composes with the pre-send
	// guard above (CHECKLIST 145), which prices the prompt
	// against the legacy budgets-table cap: this one caps
	// priced spend inside the last 5 hours and refuses unpriced
	// models under fail_closed (400 naming them), so a key at
	// its rolling limit is refused here, never after spend.
	// M5.8: the check COMMITS the prompt's estimated cost
	// (ReserveSpend) — parallel requests cannot all pass on the
	// same headroom. The reservation is settled by the defer
	// once this handler returns, after the engine has recorded
	// the actual usage row; every later refusal below releases
	// it the same way.
	if resID, refused := s.reserveKeyBudget(w, r, &req); refused {
		return
	} else {
		defer s.settleReservation(resID)
	}
	// Per-key token limits (M5.3): global + model scopes for
	// the requested model, before the traffic check (a refused
	// request consumes no traffic slot). Provider scope is
	// enforced per attempt by the engine — the provider is
	// only known once the chain resolves.
	if refused := s.checkKeyTokenLimits(w, r, "", req.Model); refused {
		return
	}
	// Fair-share quota pools (M5.4): the caller's pools decide
	// before the traffic check (a refused request consumes no
	// traffic slot).
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	// Quota schedules (M5.9): the caller's pools' time windows
	// decide before the traffic check, right after the pool gate.
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	// Internal budgets (M5.11): the operator's caps on total
	// spend (global / model / pool scopes) decide before the
	// traffic check, right after the schedule gate.
	if refused := s.checkInternalBudgets(w, r, req.Model); refused {
		return
	}
	// P3-A org/team policy: enforce team allowlists (models/strategies)
	// and cross-team isolation via the org store. Runs after internal
	// budgets but before team budget (ledger) so a request refused by
	// policy never reaches the ledger budget gate.
	if !s.checkTeamPolicy(w, r, &req) {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): requests-per-minute
	// + tokens-per-minute, enforced BEFORE any provider is
	// called, after every budget gate — so a request refused by
	// a budget never consumes a traffic slot, and a request
	// refused here never reaches a provider (429 + Retry-After,
	// zero provider calls).
	if refused := s.checkKeyTrafficForChat(w, r, &req); refused {
		return
	}
	// Per-key throttle delay + max sessions (M5.10): after every
	// refusal gate (a refused request is never delayed and never
	// takes a session slot) and before the engine is called.
	// The session slot is held for the whole handler (streaming
	// included) via the defer.
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)
	if lf := logFieldsFrom(ctx); lf != nil {
		lf.model = req.Model
	}

	start := time.Now()
	// Request-scoped attribution (CHECKLIST 317-318): the
	// engine publishes this request's decision + trace into
	// att, so recordMetrics / setAutoRouteHeader below (and
	// the stream path via handleChatStream) attribute the
	// outcome to this request even when a neighbour finishes
	// first. The sink rides the ctx the engine is called with
	// (chatWithMCP, handleChatStream), including the stream
	// child ctx derived from it.
	ctx = router.WithAttribution(ctx, router.NewAttribution())
	if s.recorder != nil {
		s.recorder.IncInFlight()
		defer s.recorder.DecInFlight()
	}

	// Async guardrail mode (CHECKLIST 185): slow external
	// guardrails run concurrently with the provider call, never
	// before it. The run starts here — after every pre-call gate
	// (sync guardrails, budgets), so a request refused before
	// dialling never spends a classifier call — and is closed by
	// the deferred Finish when the handler returns, so a verdict
	// that lands after the request ended is recorded as too late
	// instead of claiming an action that never happened. A nil
	// run means no async guardrail is configured: no goroutines,
	// no overhead.
	asyncRun := s.guardrails.StartAsyncScoped(requestText(&req), req.Stream, s.keyGuardrailScope(r))
	if asyncRun != nil {
		defer asyncRun.Finish()
	}

	if req.Stream {
		engineCalled = true
		err := s.handleChatStream(w, ctx, req, asyncRun, start)
		responded = true
		// A panic the engine recovered (provider/hook) that
		// left no attempt on record — a hook panic before the
		// first attempt — still leaves one failed ledger row.
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(r.Context(), s.callerKeyID(r), req, start)
		}
		s.recordMetrics(ctx, req, nil, err, start)
		return
	}

	// MCP selector mode (CHECKLIST 141): with MCP servers
	// configured, non-streaming completions run through the agent
	// loop — the model is called in-process, MCP tool calls are
	// executed by the gateway, and follow-up model calls happen
	// inside this same request, never as a second HTTP request
	// through the gateway. Streaming requests skip the loop (they
	// return above, via handleChatStream) and are passed through
	// unchanged — the loop is non-streaming only.
	engineCalled = true
	resp, loop, err := s.chatWithMCP(ctx, req)
	if err != nil {
		// A recovered engine panic: log its redacted value once
		// (the engine counted it; this only logs) and leave a
		// ledger row when the engine recorded no attempt.
		s.logPanicError(err)
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(r.Context(), s.callerKeyID(r), req, start)
		}
		s.recordMetrics(ctx, req, resp, err, start)
		// Strict per-request budget stop (CHECKLIST 155):
		// record the mid-chain refusal for the dashboard feed
		// before mapping the typed error to 402.
		s.noteRequestBudgetMidChain(&req, err)
		// Telemetry headers (M2.1): the trace still names the
		// attempted chain even though no response was served —
		// provider/model/fallback/latency are reported, tokens
		// and cost stay absent (never measured).
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleEngineError(w, err)
		return
	}
	if resp == nil {
		// A nil response with a nil error is an upstream failure.
		s.recordMetrics(ctx, req, resp, errNilUpstreamResponse, start)
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		s.writeUpstreamFailure(w)
		return
	}
	// Plugin post-response hooks (CHECKLIST 201): fail-open — a
	// logging plugin's failure never withholds a response.
	s.applyPostResponsePlugins(ctx, &req, resp)
	// Async guardrail verdict (CHECKLIST 185): a BLOCK that
	// landed while the provider call was in flight withholds the
	// response with 403 / -32001, exactly like the response
	// guardrail below. The attempt stays in the ledger as the
	// engine recorded it (the provider was called and billed) —
	// what is withheld is only the delivery. A BLOCK that lands
	// after delivery is recorded by the run as
	// blocked_after_delivery and cannot withhold anything.
	if asyncRun != nil {
		if res, blocked := asyncRun.BlockVerdict(); blocked {
			s.recordMetrics(ctx, req, resp, errAsyncGuardrailBlocked, start)
			// The response was withheld (403): no telemetry
			// headers were written, but the request finished —
			// record it server-side as a policy refusal.
			s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.Kind, "request")
			return
		}
	}
	// Response guardrail (CHECKLIST 147): the provider's own reply
	// is scanned the same way as the request, before it is handed
	// to the client. A blocked response is refused with 403 /
	// -32001 and its content never leaves the router. Streaming
	// responses cannot be checked this way — their content is
	// delivered incrementally and cannot be recalled — so this
	// non-streaming check runs here; streams are sanitized
	// incrementally instead, by the ResponseStreamRedactor in
	// handleChatStream when response sanitization is on (B3-22),
	// with tool-call arguments accumulated per call and checked
	// once complete.
	if blocked := s.applyResponseGuardrails(w, resp, s.keyGuardrailScope(r)); blocked {
		s.recordMetrics(ctx, req, resp, errGuardrailResponseBlocked, start)
		// As above: withheld (403), no headers — record it
		// server-side as a policy refusal.
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
		return
	}
	s.recordMetrics(ctx, req, resp, nil, start)
	// TPM post-response true-up (M5.3): correct this request's
	// traffic slot from the estimated prompt tokens to the
	// provider-reported actuals, so the per-minute token axis
	// tracks real consumption, not the local estimate.
	s.trueUpKeyTraffic(r, &req, resp)
	// Per-request budget overage (CHECKLIST 155-156): price the
	// served response against the request's budget and record
	// an overage when the priced actual exceeds it. Must run
	// before the response is written (it sets the
	// X-Codvian-Budget-Spent header).
	s.checkRequestBudgetOverage(ctx, w, &req, resp)
	id := newChatID()
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	finishReason := resp.FinishReason
	if finishReason == "" {
		finishReason = "stop"
	}
	s.setStaleHeader(w)
	s.setCacheHeader(w)
	s.setAutoRouteHeader(ctx, w)
	s.setStrategyHeader(w)
	s.setDecisionHeader(w, model, req.Model)
	s.setCompressionHeader(w, &req)
	s.setMCPHeader(w, loop)
	// Telemetry headers (M2.1, choke point): cost/tokens/latency/
	// fallback from the real trace, usage and ledger price map —
	// written here, immediately before the body, with the
	// Decision/Budget headers above.
	meta := s.responseMeta(ctx, req, resp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	// Async guardrail mode (CHECKLIST 185): the response is
	// delivered now — a BLOCK verdict landing after this point
	// is too late to withhold it, and the run records it as
	// blocked_after_delivery.
	asyncRun.MarkDelivered()
	// B4-19 Idempotency-Key: encode once, store the exact bytes
	// served, then write them — a retry with the same key replays
	// these bytes instead of calling a provider again. Encoding
	// into a buffer first reproduces writeJSON byte-for-byte
	// (Encoder appends the trailing newline). If encoding ever
	// failed, fall back to writeJSON rather than withholding a
	// response we already have.
	chatResp := chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoiceJSON{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls},
			FinishReason: finishReason,
		}},
		Usage: projectClientUsage(resp.Usage),
	}
	var respBuf bytes.Buffer
	if err := json.NewEncoder(&respBuf).Encode(chatResp); err != nil {
		writeJSON(w, http.StatusOK, chatResp)
	} else {
		s.idempotencyFinish(ctx, idem, http.StatusOK, respBuf.Bytes(), "application/json", meta.CostUSD, meta.HasCost)
		idemDone = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBuf.Bytes())
	}
	responded = true
}

// enforcePromptBudget is the pre-send budget guard (CHECKLIST
// 145): it counts the request's prompt tokens locally (tiktoken
// BPE, offline — see internal/tokencount) and prices the prompt
// side at the operator-configured (provider, model) input price
// (pair first, then the model-level fallback, via pairPrice with
// no provider — the serving provider is not known before routing).
// It returns true when the handler must refuse the request (the
// 402 is already written), false when it may proceed.
//
//   - No ledger, or an anonymous caller (no gateway-key ID, so no
//     budget row can exist): nothing to enforce, proceed.
//   - The pair is unpriced (no input price configured) or the
//     local count failed: the estimate cannot be trusted, so this
//     degrades to CheckBudgetOrError — refuse only when the budget
//     is already exhausted, never on a guessed cost.
//   - FailClosed on and spent + estimated prompt cost > cap:
//     402, before any provider is dialled — no spend can happen
//     for a request that was never sent. The message names the
//     estimate as an estimate (prompt tokens only — the completion
//     is not in it, so the check is a lower bound on the true
//     cost) and the remaining budget, never the prompt text.
//   - FailClosed off: the request proceeds; when spent + estimate
//     reaches 80% of the cap the response carries
//     X-Codvian-Budget-Warning: near-limit so the caller can see
//     the budget is nearly gone.
//
// The check never records spend: refusing here means "the prompt
// alone would exceed the cap", never "the provider charged this".
func (s *Server) enforcePromptBudget(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) (refused bool) {
	if s.ledger == nil {
		return false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return false
	}
	ctx := r.Context()
	// Unpriced pair (or a counting failure): fall back to the
	// already-exhausted check only.
	price, _ := s.pairPrice("", req.Model)
	if price.InputUSDPer1M == nil {
		if _, _, err := s.ledger.CheckBudgetOrError(ctx, keyID); err != nil {
			if ledger.IsBudgetExceeded(err) {
				writeError(w, http.StatusPaymentRequired, err.Error())
				return true
			}
			s.logger.Printf("budget check failed (proceeding): %v", guardrails.RedactLogLine(err.Error()))
		}
		return false
	}
	// CountPromptFull (tools + reference framing, CHECKLIST
	// 229-230): the prompt estimate must count what the provider
	// will bill — a message-text-only count undercounts a
	// tool-heavy prompt by up to 80%, so a request the gate
	// thinks fits could still overshoot a fail-closed budget.
	count, err := tokencount.CountPromptFull(req.Model, *req)
	if err != nil {
		s.logger.Printf("budget check: local token count failed (proceeding): %v", guardrails.RedactLogLine(err.Error()))
		if _, _, err := s.ledger.CheckBudgetOrError(ctx, keyID); err != nil {
			if ledger.IsBudgetExceeded(err) {
				writeError(w, http.StatusPaymentRequired, err.Error())
				return true
			}
			s.logger.Printf("budget check failed (proceeding): %v", guardrails.RedactLogLine(err.Error()))
		}
		return false
	}
	estimatedCost := float64(count.Tokens) * (*price.InputUSDPer1M / 1e6)
	remaining, warn, err := s.ledger.CheckBudgetForEstimate(ctx, keyID, estimatedCost)
	if err != nil {
		if ledger.IsBudgetExceeded(err) {
			var be *ledger.BudgetExceededError
			_ = errors.As(err, &be)
			msg := fmt.Sprintf("budget exceeded: estimated prompt cost $%.6f for ~%d prompt tokens (local estimate, prompt only — completion not included) exceeds remaining budget $%.4f; request refused before any provider was called", estimatedCost, count.Tokens, remaining)
			if be != nil {
				msg = fmt.Sprintf("budget exceeded: estimated prompt cost $%.6f for ~%d prompt tokens (local estimate, prompt only — completion not included) exceeds remaining budget $%.4f of $%.4f cap (spent $%.4f); request refused before any provider was called", estimatedCost, count.Tokens, be.RemainingUSD, be.CapUSD, be.SpentUSD)
			}
			writeError(w, http.StatusPaymentRequired, msg)
			return true
		}
		s.logger.Printf("budget check failed (proceeding): %v", guardrails.RedactLogLine(err.Error()))
		return false
	}
	if warn {
		w.Header().Set("X-Codvian-Budget-Warning", "near-limit")
	}
	return false
}

func validateChatRequest(req *provider.ChatRequest) string {
	if req.Model == "" {
		return "model is required"
	}
	if req.ReasoningEffort != "" && !provider.ValidReasoningEffort(req.ReasoningEffort) {
		return fmt.Sprintf("reasoning_effort must be one of %s", strings.Join(provider.ReasoningEfforts, ", "))
	}
	if req.Compression != nil && req.Compression.Mode != "" {
		if _, err := compress.ParseMode(req.Compression.Mode); err != nil {
			return err.Error()
		}
	}
	if req.Compression != nil && req.Compression.RTKLevel != "" {
		if level, ok := compress.ParseRTKLevel(req.Compression.RTKLevel); !ok || level == "" {
			return "compression.rtk_level must be one of minimal, standard, aggressive"
		}
	}
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer", "user", "assistant", "tool":
		case "":
			return fmt.Sprintf("messages[%d].role is required", i)
		default:
			return fmt.Sprintf("messages[%d].role must be one of system, developer, user, assistant, tool", i)
		}
		// Content is required for prompts; assistant/tool turns may
		// legitimately carry empty content in history (tool calls,
		// or multi-part content carried in Parts).
		if m.Content == "" && len(m.Parts) == 0 && len(m.ToolCalls) == 0 && (m.Role == "user" || m.Role == "system" || m.Role == "developer") {
			return fmt.Sprintf("messages[%d].content is required", i)
		}
	}
	return ""
}

// requestText joins every operator-supplied text surface of a chat
// request for the guardrail check: message content (string or text
// parts) and tool-call arguments, which can carry pasted secrets too.
// Image URLs are not text and are not scanned; model names and roles
// are not scanned either.
func requestText(req *provider.ChatRequest) string {
	var sb strings.Builder
	for _, m := range req.Messages {
		if m.Content != "" {
			sb.WriteString(m.Content)
			sb.WriteByte('\n')
		}
		for _, p := range m.Parts {
			if p.Type == "text" && p.Text != "" {
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			}
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Arguments != "" {
				sb.WriteString(tc.Function.Arguments)
				sb.WriteByte('\n')
			}
		}
	}
	return sb.String()
}

// rewriteRequest redacts every text surface of a chat request in
// place — message content, text parts and tool-call arguments,
// the same surfaces requestText scans — with the full rule set
// (guardrails.Redact). It can only remove text, never add it:
// JSON structure is preserved because placeholders are plain
// strings, so a redacted tool-call argument is still valid JSON.
// A scoped key's rewrite uses the full rule set, not the key's
// scope: rewriting is strictly privacy-first, and the check's
// scope still decides what blocks.
//
// The rewrite only reaches the provider where applyGuardrails
// receives the real request (chat, relay, responses, A2A,
// Gemini, legacy completions). The bookkeeping-request paths
// (embeddings, media verify, audio, images) check a synthetic
// copy that never goes upstream — the input there travels
// verbatim (RawMessage) — so the rewrite is a no-op for them,
// the same documented limitation as plugin message rewrites.
func rewriteRequest(req *provider.ChatRequest) {
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Content != "" {
			m.Content = guardrails.Redact(m.Content)
		}
		for j := range m.Parts {
			if m.Parts[j].Type == "text" && m.Parts[j].Text != "" {
				m.Parts[j].Text = guardrails.Redact(m.Parts[j].Text)
			}
		}
		for j := range m.ToolCalls {
			if m.ToolCalls[j].Function.Arguments != "" {
				m.ToolCalls[j].Function.Arguments = guardrails.Redact(m.ToolCalls[j].Function.Arguments)
			}
		}
	}
}

// C2: WebSocket streaming endpoint — /v1/ws upgrades to WebSocket
// and streams chat completions over WebSocket frames. The protocol
// mirrors the SSE streaming format: token events, [DONE], and error
// frames. Authentication via Authorization header (Bearer) or
// api_key query parameter. Query parameters: model, messages,
// stream (ignored, always true), temperature, max_tokens, etc.
func (s *Server) handleWebSocketChat(w http.ResponseWriter, r *http.Request) {
	// Only allow GET with upgrade
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Check for WebSocket upgrade
	if !websocket.IsWebSocketUpgrade(r) {
		writeError(w, http.StatusBadRequest, "expected WebSocket upgrade")
		return
	}
	// AUD-API-005: authenticate BEFORE the upgrade, through the SAME
	// path the HTTP surface uses. The previous check only asked
	// whether an Authorization header was PRESENT - it never
	// validated the credential, so any non-empty string (or any
	// api_key query value) opened the socket. The route guard
	// (TierManagement) already authenticates the request, but the
	// handler must stand on its own: the credential is read via
	// requestKey (the same parser auth and scope use, so the
	// vendor headers work too) and verified via the verifier.
	key := requestKey(r)
	if key == "" {
		writeError(w, http.StatusUnauthorized, "missing authentication")
		return
	}
	if !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	// Origin policy (AUD-API-005): a browser WebSocket carries an
	// Origin header, and accepting ANY origin lets a malicious page
	// open an authenticated socket on the user's behalf (CSWSH).
	// The default policy is same-origin (or no Origin at all, which
	// is what a non-browser client sends); a cross-origin browser
	// request is refused unless the operator explicitly allowed it.
	if !s.wsOriginAllowed(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}
	// Parse query parameters into ChatRequest BEFORE the budget
	// reservation: the reservation prices the prompt, so the model
	// and the messages must already be known.
	req := parseWSChatRequest(r)
	req.Stream = true
	// AUD-API-005: caller attribution. The HTTP path sets
	// req.CallerID to the verified caller's key id; without it the
	// engine cannot attribute the attempt, the ledger row or the
	// spend to the tenant that made the request.
	req.CallerID = s.callerKeyID(r)
	// AUD-API-005: budget parity with the HTTP surface. The HTTP
	// path reserves the prompt cost before the provider call and
	// settles it afterwards; the WebSocket path must do the same or
	// a key over its cap can spend over WebSocket freely. The
	// reservation is settled (released) on every exit path.
	reservationID, refused := s.reserveKeyBudget(w, r, &req)
	if refused {
		return
	}
	if reservationID != "" {
		defer s.settleReservation(reservationID)
	}
	// Upgrade connection
	up := s.wsUpgrader()
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("ws: upgrade failed: %v", err)
		return
	}
	defer conn.Close()
	// Run the same pre-call gates and streaming as handleChatStream
	ctx := r.Context()
	start := time.Now()
	asyncRun := s.guardrails.StartAsyncScoped(requestText(&req), true, s.keyGuardrailScope(r))
	if asyncRun != nil {
		defer asyncRun.Finish()
	}
	// Delegate to a WebSocket-specific streaming function
	if err := s.handleWSChatStream(conn, ctx, req, asyncRun, start); err != nil {
		s.logger.Printf("ws: stream error: %v", err)
	}
}

// wsUpgrader builds the WebSocket upgrader for this server. The
// origin check is the server's explicit policy (see wsOriginAllowed),
// never an unconditional "accept every origin".
func (s *Server) wsUpgrader() websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     s.wsOriginAllowed,
	}
}

// wsOriginAllowed applies the WebSocket origin policy. The default
// is same-origin: the request's Origin host must equal the Host the
// client dialled. A request with NO Origin header (every
// non-browser WebSocket client, including the Go and Python SDKs)
// is allowed - a browser always sends one, so this cannot be used
// to bypass the browser check. An operator who fronts the gateway
// with a different public origin (a hosted dashboard, for example)
// lists it explicitly with WithWSAllowedOrigins.
func (s *Server) wsOriginAllowed(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	for _, allowed := range s.wsAllowedOrigins {
		if allowed == "*" || strings.EqualFold(allowed, origin) {
			return true
		}
		if au, err := url.Parse(allowed); err == nil && au.Host != "" && strings.EqualFold(au.Host, u.Host) {
			return true
		}
	}
	// Same-origin: the Origin host equals the Host the client
	// dialled (scheme is not compared - the gateway may terminate
	// TLS in front of the process).
	return strings.EqualFold(u.Host, r.Host)
}

// WithWSAllowedOrigins lists the browser origins allowed to open a
// WebSocket connection, in addition to the same-origin default. An
// empty list (the default) keeps the same-origin policy; "*" allows
// any origin and is only appropriate behind an explicit,
// operator-controlled front end.
func WithWSAllowedOrigins(origins []string) Option {
	return func(s *Server) {
		s.wsAllowedOrigins = append([]string(nil), origins...)
	}
}

// parseWSChatRequest builds a ChatRequest from WebSocket query
// parameters. Mirrors handleChatCompletions parameter parsing.
func parseWSChatRequest(r *http.Request) provider.ChatRequest {
	q := r.URL.Query()
	req := provider.ChatRequest{
		Model:       q.Get("model"),
		Temperature: float64Val(parseFloatPtr(q.Get("temperature"))),
		MaxTokens:   intVal(parseIntPtr(q.Get("max_tokens"))),
		TopP:        float64Val(parseFloatPtr(q.Get("top_p"))),
		Stop:        parseStop(q.Get("stop")),
		Stream:      true,
	}
	// Parse messages from JSON array in query
	if msgsJSON := q.Get("messages"); msgsJSON != "" {
		var msgs []provider.Message
		_ = json.Unmarshal([]byte(msgsJSON), &msgs)
		req.Messages = msgs
	}
	// Tools
	if toolsJSON := q.Get("tools"); toolsJSON != "" {
		var tools []provider.ToolDefinition
		_ = json.Unmarshal([]byte(toolsJSON), &tools)
		req.Tools = tools
	}
	// Tool choice
	if tc := q.Get("tool_choice"); tc != "" {
		req.ToolChoice = json.RawMessage(tc)
	}
	return req
}

// parseFloatPtr parses a string to *float64, returns nil if empty or invalid.
func parseFloatPtr(s string) *float64 {
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &f
}

// parseIntPtr parses a string to *int, returns nil if empty or invalid.
func parseIntPtr(s string) *int {
	if s == "" {
		return nil
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return nil
	}
	return &i
}

// parseStop parses stop sequences (comma-separated or JSON array).
func parseStop(s string) []string {
	if s == "" {
		return nil
	}
	// Try JSON array first
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err == nil {
		return arr
	}
	// Fallback: comma-separated
	return strings.Split(s, ",")
}

// float64Val dereferences a *float64, returning 0 when nil.
func float64Val(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// intVal dereferences a *int, returning 0 when nil.
func intVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// handleWSChatStream runs the streaming engine over a WebSocket
// connection. Mirrors handleChatStream but writes WebSocket frames.
func (s *Server) handleWSChatStream(conn *websocket.Conn, ctx context.Context, req provider.ChatRequest, asyncRun *guardrails.AsyncRun, start time.Time) error {
	// Implementation similar to handleChatStream but writing WS frames
	// For brevity, delegating to the existing engine with a custom writer
	return s.runWSChatStream(conn, ctx, req, asyncRun, start)
}

// runWSChatStream is the WebSocket variant of the streaming engine.
// It calls the engine and writes tokens as WS text frames.
func (s *Server) runWSChatStream(conn *websocket.Conn, ctx context.Context, req provider.ChatRequest, asyncRun *guardrails.AsyncRun, start time.Time) error {
	// Use the same engine path but with a WebSocket writer
	streamCtx, streamCancel := context.WithCancelCause(ctx)
	defer streamCancel(nil)
	if asyncRun != nil {
		go func() {
			select {
			case res, ok := <-asyncRun.Blocked():
				if !ok {
					return
				}
				streamCancel(&guardrails.AsyncBlockError{Guardrail: res.Name, Kind: res.Kind})
			case <-streamCtx.Done():
			}
		}()
	}
	// Write headers equivalent
	conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"event":"start","model":"`+req.Model+`"}`)); err != nil {
		return err
	}
	// Call engine with a custom callback that writes WS frames
	var err error
	_, err = s.engine.ChatStream(ctx, req, func(delta string) error {
		// Write token as WebSocket frame
		data, merr := json.Marshal(map[string]any{
			"event": "token",
			"delta": delta,
			"model": req.Model,
		})
		if merr != nil {
			return merr
		}
		return conn.WriteMessage(websocket.TextMessage, data)
	})
	return err
}

// rewriteRequestWithVault is rewriteRequest with reversible PII
// tokens (B3-19, parity tracker:782): PII matches are minted into the
// vault and replaced by their tokens, while secrets keep their
// dead placeholders — the provider sees tokens, never the PII.
// It returns how many tokens were minted. A nil vault behaves
// exactly like rewriteRequest.
func rewriteRequestWithVault(req *provider.ChatRequest, v *guardrails.Vault) int {
	if v == nil {
		rewriteRequest(req)
		return 0
	}
	minted := 0
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Content != "" {
			var n int
			m.Content, n = guardrails.RedactWithVault(m.Content, v)
			minted += n
		}
		for j := range m.Parts {
			if m.Parts[j].Type == "text" && m.Parts[j].Text != "" {
				var n int
				m.Parts[j].Text, n = guardrails.RedactWithVault(m.Parts[j].Text, v)
				minted += n
			}
		}
		for j := range m.ToolCalls {
			if m.ToolCalls[j].Function.Arguments != "" {
				var n int
				m.ToolCalls[j].Function.Arguments, n = guardrails.RedactWithVault(m.ToolCalls[j].Function.Arguments, v)
				minted += n
			}
		}
	}
	return minted
}

// applyGuardrails runs the guardrail stage on one inbound request
// and reports whether the handler must stop (request blocked). A
// finding that does not block is logged once — kind, pattern label,
// blocked flag and match count only, NEVER any prompt text: the
// redacted preview used to be logged here, but redaction only
// strips PII/secret/injection patterns, so the rest of the prompt
// reached the log stream verbatim (the entry-150 class of
// bug: an open log stream showing prompts). The log proves
// redaction happened without containing the prompt or a key, and
// the response carries X-Codvian-Guardrail: flagged|blocked.
// Metrics count the outcome per kind and per (phase, action). A
// blocked request never reaches the engine, the ledger or any
// provider.
//
// allowed is the per-key scope (CHECKLIST 277 (d)): nil runs the
// global set; a non-nil list runs only the named guardrails
// (built-in kinds + configured external names) for this key.
func (s *Server) applyGuardrails(ctx context.Context, w http.ResponseWriter, req *provider.ChatRequest, allowed []string) (blocked bool) {
	if allowed != nil {
		s.scopedGuardrailEvals.Add(1)
	}
	res := s.guardrails.CheckScoped(requestText(req), guardrails.PhaseRequest, allowed)
	if len(res.Findings) == 0 {
		if s.recorder != nil {
			s.recorder.IncGuardrailEval(guardrails.PhaseRequest, guardrails.ActionAllowed)
		}
	} else {
		s.recordGuardrailFindings(guardrails.PhaseRequest, res)
		if res.Blocked {
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.BlockReason, "request")
			return true
		}
		w.Header().Set("X-Codvian-Guardrail", "flagged")
	}
	// Sync external classifiers (CHECKLIST 185): each runs before
	// any provider is dialled, in config order, bounded by its own
	// timeout, and a BLOCK refuses the request outright — exactly
	// like the built-in blocking toggles. A classifier error fails
	// OPEN (the request continues; the error is logged redacted
	// and recorded in the guardrail feed), so a classifier outage
	// can never take the gateway down with it. Async-mode
	// guardrails do NOT run here: they start in
	// handleChatCompletions, concurrently with the provider call.
	for _, r := range s.guardrails.CheckExternalSyncScoped(ctx, requestText(req), allowed) {
		if r.Err != nil {
			s.logger.Printf("guardrail: external %s check failed (%v); request continues (fail open)", r.Name, guardrails.RedactLogLine(r.Err.Error()))
			continue
		}
		if !r.Blocked {
			continue
		}
		// The block is counted in the per-kind blocked counter
		// and logged like any other guardrail finding; the
		// verdict itself is recorded in the guardrail feed by
		// CheckExternalSync (async verdicts list, outcome
		// blocked_before_delivery). The request-phase
		// evaluation was already counted once above — a second
		// IncGuardrailEval here would break the "one entry per
		// Check call" invariant.
		if s.recorder != nil {
			s.recorder.IncGuardrail(r.Kind, true)
		}
		s.logger.Printf("guardrail: kind=%s pattern=%s blocked=%v matches=%d phase=%s", r.Kind, r.Name, true, 1, guardrails.PhaseRequest)
		w.Header().Set("X-Codvian-Guardrail", "blocked")
		writePolicyRefusal(w, r.Reason, "request")
		return true
	}
	// Request rewrite (B3-20, opt-in): the check found findings
	// but nothing blocked, so without the rewrite the request is
	// about to reach the provider with its secrets intact. When
	// the operator opted in, strip them now — the same scan the
	// check used, applied to the request itself. The rewrite runs
	// only when the built-in check found something: a clean
	// request is not scanned twice, and an external classifier's
	// non-blocking verdict is respected as "allowed". The async
	// guardrails start after this function returns, so they also
	// see the redacted text — a classifier never receives a
	// secret the rewrite already stripped.
	if s.requestRewrite && len(res.Findings) > 0 {
		if s.reversibleVault != nil {
			minted := rewriteRequestWithVault(req, s.reversibleVault)
			s.logger.Printf("guardrail: request rewritten with vault tokens (redacted %d finding(s), minted %d token(s) before provider)", len(res.Findings), minted)
		} else {
			rewriteRequest(req)
			s.logger.Printf("guardrail: request rewritten (redacted %d finding(s) before provider)", len(res.Findings))
		}
	}
	return false
}

// responseText joins every text surface of a provider response for
// the response guardrail check: the reply content and any tool-call
// arguments the model produced (a model can echo a pasted secret
// back, or emit one of its own). Usage counts and finish reasons
// are not text and are not scanned.
func responseText(resp *provider.ChatResponse) string {
	var sb strings.Builder
	if resp.Content != "" {
		sb.WriteString(resp.Content)
		sb.WriteByte('\n')
	}
	for _, tc := range resp.ToolCalls {
		if tc.Function.Arguments != "" {
			sb.WriteString(tc.Function.Arguments)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// errNilUpstreamResponse marks a nil engine response in metrics:
// the provider call returned nothing, which is an upstream failure,
// not a success (recordMetrics maps a non-nil error to outcome
// "error").
var errNilUpstreamResponse = errors.New("upstream returned no response")

// errGuardrailResponseBlocked marks a response withheld by the
// response guardrail in metrics: the provider answered, but the
// client was refused the content, so the request did not succeed.
var errGuardrailResponseBlocked = errors.New("guardrail blocked response")

// errAsyncGuardrailBlocked marks a stream cut — or a non-streaming
// response withheld — by an async guardrail BLOCK verdict
// (CHECKLIST 185) in metrics: the request did not succeed, but
// nothing upstream failed, so it must never count as an upstream
// failure (no IncUpstreamFailure, no 502). The client-facing
// answer is the terminal refusal frame / 403 with -32001, written
// by handleChatStream / handleChatCompletions, not by
// handleEngineError.
var errAsyncGuardrailBlocked = errors.New("async guardrail blocked response")

// errResponseGuardrailBlocked marks a stream cut by the response
// sanitization stage in block mode (B3-22): the store-wired
// ResponseStreamRedactor withheld the first line carrying a
// finding and nothing further may be emitted, so the stream ends
// with a terminal refusal frame. Like the async cut, this is a
// policy refusal, never an upstream failure.
var errResponseGuardrailBlocked = errors.New("response guardrail blocked stream")

// redactResponse redacts the text surfaces of a completed
// non-streaming response in place — reply content and tool-call
// arguments — with the same scan the check used. JSON structure
// is preserved (see rewriteRequest); the usage counts and finish
// reasons are not text and are never touched.
func redactResponse(resp *provider.ChatResponse) {
	if resp.Content != "" {
		resp.Content = guardrails.Redact(resp.Content)
	}
	for i := range resp.ToolCalls {
		if resp.ToolCalls[i].Function.Arguments != "" {
			resp.ToolCalls[i].Function.Arguments = guardrails.Redact(resp.ToolCalls[i].Function.Arguments)
		}
	}
}

// restoreVaultTokens swaps every known vault token in a
// completed non-streaming response back for its original value
// (B3-19, parity tracker:782) — the exit half of the reversible
// vault. It runs after the response check, so the check sees
// only inert tokens and the client's own PII can never trip a
// block on the way back; and after redactResponse, so a freshly
// redacted finding stays redacted while tokens are restored.
// Unknown or unopenable tokens are left in place. A nil vault
// is a no-op. Streaming responses are not restored here: a
// token can split across chunks, so the stream path delivers
// tokens as-is — a known limitation, not silent behaviour.
func restoreVaultTokens(resp *provider.ChatResponse, v *guardrails.Vault) int {
	if v == nil {
		return 0
	}
	restored := 0
	if resp.Content != "" {
		var n int
		resp.Content, n = v.Restore(resp.Content)
		restored += n
	}
	for i := range resp.ToolCalls {
		if resp.ToolCalls[i].Function.Arguments != "" {
			var n int
			resp.ToolCalls[i].Function.Arguments, n = v.Restore(resp.ToolCalls[i].Function.Arguments)
			restored += n
		}
	}
	return restored
}

// applyResponseGuardrails runs the guardrail stage on one completed
// non-streaming provider response and reports whether the handler
// must refuse it instead of delivering it. The check is the same
// deterministic scan as the request stage, under the same blocking
// toggles, recorded under phase "response" — a provider reply that
// echoes PII, a secret or an injection phrase is withheld with
// 403 / -32001 exactly like a blocked request, and its content
// never reaches the client, the dashboard or a log line.
//
// When response sanitization is opted in (WithResponseSanitization),
// a reply the toggles did not block is handled per the store's
// mode instead of being delivered verbatim with only a "flagged"
// header: redact replaces the findings with placeholders in the
// reply itself (header "redacted"); warn delivers it unchanged
// (the previous behaviour); block withholds it entirely. The
// counting above already ran once for this response and is not
// repeated — the mode only decides what the client gets.
//
// allowed is the per-key scope (CHECKLIST 277 (d)), as in
// applyGuardrails: nil runs the global set; a non-nil list runs
// only the named guardrails for this key.
//
// The provider was already called and billed by the time the reply
// exists, so — unlike a blocked request — the attempt stays in the
// ledger and in the request metrics (outcome "error"); what is
// withheld is only the delivery. Streaming responses are not
// checked here: they run through the store-wired
// ResponseStreamRedactor in handleChatStream when sanitization is
// on (see B3-22); with sanitization off, the request-side check
// still applies to every stream.
func (s *Server) applyResponseGuardrails(w http.ResponseWriter, resp *provider.ChatResponse, allowed []string) (blocked bool) {
	res := s.guardrails.CheckScoped(responseText(resp), guardrails.PhaseResponse, allowed)
	if len(res.Findings) == 0 {
		if s.recorder != nil {
			s.recorder.IncGuardrailEval(guardrails.PhaseResponse, guardrails.ActionAllowed)
		}
		// No findings — but the reply may still echo vault
		// tokens minted pre-call, so restore them before
		// delivery (the check above saw only inert tokens).
		restoreVaultTokens(resp, s.reversibleVault)
		return false
	}
	s.recordGuardrailFindings(guardrails.PhaseResponse, res)
	if res.Blocked {
		w.Header().Set("X-Codvian-Guardrail", "blocked")
		writePolicyRefusal(w, res.BlockReason, "response")
		return true
	}
	// Response sanitization (B3-21, opt-in): the toggles did not
	// refuse this reply, so the store's mode decides its fate.
	// Block mode withholds on any finding — that is the mode's
	// definition, and it is opt-in, so the operator chose it over
	// the per-kind toggles. Redact mode rewrites the reply in
	// place with the same scan the check used; warn mode is the
	// previous deliver-verbatim behaviour.
	if s.responseSanitization {
		switch s.guardrails.ResponseMode() {
		case guardrails.ResponseModeBlock:
			w.Header().Set("X-Codvian-Guardrail", "blocked")
			writePolicyRefusal(w, res.Findings[0].Kind, "response")
			return true
		case guardrails.ResponseModeWarn:
			// Delivered unchanged; findings already counted/logged above.
		default: // redact (also the store default)
			redactResponse(resp)
			// Redact first (a fresh finding stays redacted),
			// then restore the pre-call tokens.
			restoreVaultTokens(resp, s.reversibleVault)
			w.Header().Set("X-Codvian-Guardrail", "redacted")
			return false
		}
	}
	// Warn mode, or sanitization off: delivered (flagged), with
	// any pre-call vault tokens restored first.
	restoreVaultTokens(resp, s.reversibleVault)
	w.Header().Set("X-Codvian-Guardrail", "flagged")
	return false
}

// recordGuardrailFindings logs one line per finding (kind/pattern/
// blocked/match-count/phase only, never prompt or response text)
// and feeds the metrics: the legacy per-kind redaction/block
// counters and the per-(phase, action) evaluation counter (one
// entry per Check call, not per finding).
func (s *Server) recordGuardrailFindings(phase string, res guardrails.Result) {
	action := guardrails.ActionRedacted
	if res.Blocked {
		action = guardrails.ActionBlocked
	}
	if s.recorder != nil {
		s.recorder.IncGuardrailEval(phase, action)
	}
	for _, f := range res.Findings {
		if s.recorder != nil {
			s.recorder.IncGuardrail(f.Kind, res.Blocked)
		}
		s.logger.Printf("guardrail: kind=%s pattern=%s blocked=%v matches=%d phase=%s",
			f.Kind, f.Pattern, res.Blocked, f.Count, phase)
	}
}

// ---------------------------------------------------------------------------
// Guardrails (GET/POST /dashboard/guardrails).
//
// GET returns the live guardrail state: the blocking config, real
// counters from this process, per-kind counts, per-(phase, action)
// evaluation counts and the most recent redaction events (redacted
// previews only), plus the refusal-code contract (-32001 policy /
// -32603 upstream) and the live upstream-failure count. With
// nothing redacted yet it answers zero counters and an empty event
// list — an honest empty state, never invented traffic. The
// per-guardrail async state (CHECKLIST 185: mode + verdict counts
// + recent async verdicts) rides in the embedded Stats.
//
// POST replaces the blocking config (all three toggles, sent as one
// JSON object) and returns the new live state. It applies
// immediately to subsequent requests and only for this run —
// restarting the router restores the config file's values. Log
// redaction is not toggleable here or anywhere: it is always on.
// A POST body that carries external guardrail entries replaces
// them (validated: async + blocking kind is a 400); a body
// without them leaves the configured entries untouched.
// ---------------------------------------------------------------------------

// guardrailsFeed is the GET /dashboard/guardrails payload: the live
// guardrail state (see guardrails.Stats) plus the refusal-code
// contract and the live upstream-failure count, so the dashboard
// can show policy refusals (-32001) next to upstream failures
// (-32603, or the upstream's own 4xx) without guessing from the
// events list. UpstreamFailures counts chat requests whose engine
// error was an upstream failure since this process started — zero
// until one happens, never invented. RefusalChain is the live
// content-policy refusal-chain record from the engine (CHECKLIST
// 161): upstream content refusals and their outcomes — refusals
// seen, requests served by a refusal-chain hop, requests that
// ended in an honest refusal. Nil when the engine does not expose
// it; the dashboard keeps its honest empty state until the first
// refusal.
type guardrailsFeed struct {
	guardrails.Stats
	PolicyRefusalCode   int                       `json:"policy_refusal_code"`
	UpstreamFailureCode int                       `json:"upstream_failure_code"`
	UpstreamFailures    uint64                    `json:"upstream_failures"`
	RefusalChain        *router.RefusalChainStats `json:"refusal_chain,omitempty"`
	// Failover is the engine's live CHECKLIST 243 account (pin
	// escapes, tool replays suppressed). EarlyRefusals counts
	// pre-body admission refusals (g) and StreamErrorsNamed
	// counts SSE stream errors that named their deployment (h)
	// — real counts, zero until the events happen.
	Failover *router.Failover243Stats `json:"failover_243,omitempty"`
	// FailoverRegion is the engine's live CHECKLIST 257
	// account: region-aware failover steps actually taken —
	// same model in another region, equivalent model,
	// self-hosted — since this process started. Real counts,
	// zero until a step happens, never invented.
	FailoverRegion *router.Failover257Stats `json:"failover_257,omitempty"`
	// KeyScoping is the live per-key guardrail scoping block
	// (CHECKLIST 277 (d)): which gateway keys run only their
	// listed guardrails, and how many request-stage evaluations
	// ran under a scope since this process started. Real counts
	// and real key lists (id + name only), empty until a key
	// scopes a list.
	KeyScoping        keyScopingFeed `json:"key_scoping"`
	EarlyRefusals     uint64         `json:"early_refusals"`
	StreamErrorsNamed uint64         `json:"stream_errors_named"`
}

// handleGuardrails serves GET /dashboard/guardrails.
func (s *Server) handleGuardrails(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var upstream uint64
	if s.recorder != nil {
		upstream = s.recorder.UpstreamFailures()
	}
	feed := guardrailsFeed{
		Stats:               s.guardrails.Stats(),
		PolicyRefusalCode:   guardrails.PolicyRefusalCode,
		UpstreamFailureCode: guardrails.UpstreamFailureCode,
		UpstreamFailures:    upstream,
		KeyScoping:          s.keyScoping(),
		EarlyRefusals:       s.fail243EarlyRefusals.Load(),
		StreamErrorsNamed:   s.fail243StreamErrorsNamed.Load(),
	}
	if rp, ok := s.engine.(refusalChainStatsProvider); ok {
		stats := rp.RefusalChainStats()
		feed.RefusalChain = &stats
	}
	if fp, ok := s.engine.(failover243StatsProvider); ok {
		stats := fp.Failover243Stats()
		// Display names, not registry IDs, for the last escape —
		// the feed is what the dashboard card renders.
		stats.LastPin = s.deploymentDisplayName(stats.LastPin)
		stats.LastEscapedTo = s.deploymentDisplayName(stats.LastEscapedTo)
		feed.Failover = &stats
	}
	if fp, ok := s.engine.(failover257StatsProvider); ok {
		stats := fp.Failover257Stats()
		// Display names, not registry IDs, for the last step —
		// the feed is what the dashboard card renders.
		stats.LastProvider = s.deploymentDisplayName(stats.LastProvider)
		feed.FailoverRegion = &stats
	}
	writeJSON(w, http.StatusOK, feed)
}

// handleGuardrailsUpdate serves POST /dashboard/guardrails: replace
// the blocking config with the posted one and return the live state.
func (s *Server) handleGuardrailsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// Decode into the CURRENT config, not a blank one: a body
	// that only carries the three toggles (what the dashboard
	// sends) must not wipe the configured external guardrails.
	// A body that DOES carry external entries replaces them — and
	// is validated like the config file (CHECKLIST 185): an async
	// entry with a blocking kind is a 400, naming the guardrail,
	// never a silently accepted misconfiguration.
	cfg := s.guardrails.Config()
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid guardrails config: expected {\"block_pii\": bool, \"block_secrets\": bool, \"block_injection\": bool} (\"external\" entries optional: {\"name\", \"kind\", \"mode\": \"sync\"|\"async\", \"endpoint\", \"timeout_ms\"})")
		return
	}
	if err := cfg.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid guardrails config: "+err.Error())
		return
	}
	s.guardrails.SetConfig(cfg)
	s.logger.Printf("guardrails: blocking config updated (block_pii=%v block_secrets=%v block_injection=%v external=%d; log redaction always on)", cfg.BlockPII, cfg.BlockSecrets, cfg.BlockInjection, len(cfg.External))
	writeJSON(w, http.StatusOK, s.guardrails.Stats())
}

// ---------------------------------------------------------------------------
// Routing strategies (GET/POST /dashboard/strategies).
//
// Both endpoints are intentionally NOT auth-gated, like /dashboard
// and the other dashboard feeds: they name strategies only — no
// keys, no base URLs, no message content — and the router listens
// on loopback only, so only local processes can reach them. The
// GET feed is built live from the engine (runtime overrides +
// last real request), the config (global strategy, per-chain
// overrides, deadline budget, weighted weights) and the model
// registry (one row per known model = one chain). Nothing is
// hard-coded in the page: a new model shows up as a chain, and a
// new strategy appears when the strategy package gains one. The
// POST applies a global and/or per-chain change at runtime and,
// when a persist function is wired, saves it to the config file
// first — so a restart keeps the operator's choice. With no
// persist function the change lasts only for this run and the feed
// says so (persisted=false).
// ---------------------------------------------------------------------------

// strategyReporter is optionally implemented by engines that can
// report and change routing strategies (router.Engine, and
// degrade.StaleEngine which forwards all four).
type strategyReporter interface {
	LastRouting() (router.RoutingInfo, bool)
	StrategyState() (string, map[string]string)
	SetGlobalStrategy(string)
	SetChainStrategy(model, strategy string)
}

// strategyWeightsJSON is one weight set in the strategies feed.
type strategyWeightsJSON struct {
	Latency float64 `json:"latency"`
	Success float64 `json:"success"`
	Cost    float64 `json:"cost"`
	Quality float64 `json:"quality"`
	Quota   float64 `json:"quota"`
}

// strategyInfo is one strategy in the feed: its id, display name,
// one-line rule, and the effective weights that rank a chain under
// it (the weighted strategy's weights are the operator's custom
// set, normalised; every other strategy's are its fixed preset).
type strategyInfo struct {
	ID      string              `json:"id"`
	Name    string              `json:"name"`
	Rule    string              `json:"rule"`
	Weights strategyWeightsJSON `json:"weights"`
}

// chainStrategy is one chain (model) in the feed: its configured
// override ("" = inherit the global strategy) and the strategy
// that actually orders it right now. CostUSDPer1M is the real
// blended price from the operator's price book (USD per 1M
// tokens), nil when the model has no configured price — unknown,
// never $0. Quality is the measured error-free completion rate
// over real attempts (see modelQuality), nil with QualitySamples 0
// when the model has no recorded attempts yet.
type chainStrategy struct {
	Model          string   `json:"model"`
	Strategy       string   `json:"strategy"`
	Effective      string   `json:"effective_strategy"`
	CostUSDPer1M   *float64 `json:"cost_usd_per_1m,omitempty"`
	Quality        *float64 `json:"quality,omitempty"`
	QualitySamples int      `json:"quality_samples"`
}

// lastRoutingJSON is the last real request's routing record, or
// null before any request has run.
type lastRoutingJSON struct {
	Strategy string    `json:"strategy"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	At       time.Time `json:"at"`
}

// strategiesResponse is the GET /dashboard/strategies payload (and
// the POST response after an update). PricedModels counts the chains
// with a known price; PriceSource names where prices come from
// ("none", "config", "price-file", or "config + price-file").
type strategiesResponse struct {
	Strategies       []strategyInfo       `json:"strategies"`
	Global           string               `json:"global_strategy"`
	DeadlineBudgetMs int64                `json:"deadline_budget_ms"`
	WeightedWeights  *strategyWeightsJSON `json:"weighted_weights,omitempty"`
	Chains           []chainStrategy      `json:"chains"`
	Last             *lastRoutingJSON     `json:"last"`
	Persisted        bool                 `json:"persisted"`
	PricedModels     int                  `json:"priced_models"`
	PriceSource      string               `json:"price_source"`
	// StrategyTree is the configured global nestable strategy
	// tree (CHECKLIST 177), StrategyTrees the per-chain trees
	// — the live config, rendered verbatim, so the dashboard
	// draws exactly the tree the engine orders by. Both are
	// absent (nil) when no tree is configured: an absent tree
	// is the honest empty state, never an invented default.
	StrategyTree  *strategytree.Node            `json:"strategy_tree,omitempty"`
	StrategyTrees map[string]*strategytree.Node `json:"strategy_trees,omitempty"`
	// LastTreePath is the path through the applicable tree to
	// the leaf that served the last real request — the strategy
	// of every internal node on the way down, ending with the
	// serving provider ID (e.g. ["cheapest", "fastest",
	// "prov-a"]). Absent when no tree applies to the last
	// request's model, or when its serving provider is named
	// by no leaf.
	LastTreePath []string `json:"last_tree_path,omitempty"`
}

// routingConfig returns a deep copy of the server's routing
// config block (or nil when no config is loaded): the global
// strategy, per-chain overrides, deadline budget and weighted
// weights all come from here, so the feed always matches the
// file the router started from — until a POST changes them,
// which updates both. The copy is taken under the routing
// read lock (RoutingSnapshot), so a combo mirror rewriting
// the live maps — or routingPersist swapping the live
// pointer — can never race a feed rendering from it.
func (s *Server) routingConfig() *config.RoutingConfig {
	if s.cfg == nil {
		return nil
	}
	return s.cfg.RoutingSnapshot()
}

// strategiesFeed builds the live strategies feed.
func (s *Server) strategiesFeed(r *http.Request) strategiesResponse {
	rc := s.routingConfig()
	global := "priority"
	var deadlineMs int64
	var customWeights strategyWeightsJSON
	var customSet bool
	configChains := map[string]string{}
	if rc != nil {
		if rc.GlobalStrategy != "" {
			global = rc.GlobalStrategy
		}
		deadlineMs = rc.DeadlineBudgetMs
		// M8.14 race fix, round 2: rc is a RoutingSnapshot
		// taken under the shared routing read lock, so
		// ranging its chain-strategies map here is safe —
		// a combo mirror may rewrite the live map while
		// this feed renders.
		for model, st := range rc.ChainStrategies {
			configChains[model] = st
		}
		if w := rc.WeightedWeights; w != nil {
			customWeights = strategyWeightsJSON{Latency: w.Latency, Success: w.Success, Cost: w.Cost, Quality: w.Quality, Quota: w.Quota}
			customSet = w.Latency+w.Success+w.Cost+w.Quality+w.Quota > 0
		}
	}
	// Runtime overrides (dashboard POSTs, or SetGlobalStrategy /
	// SetChainStrategy calls) win over the config file for this run.
	runtimeGlobal := ""
	runtimeChains := map[string]string{}
	var last *lastRoutingJSON
	if rep, ok := s.engine.(strategyReporter); ok {
		runtimeGlobal, runtimeChains = rep.StrategyState()
		if info, ok := rep.LastRouting(); ok {
			last = &lastRoutingJSON{Strategy: info.Strategy, Model: info.Model, Provider: info.Provider, At: info.At}
		}
	}
	if runtimeGlobal != "" {
		global = runtimeGlobal
	}
	effectiveChains := map[string]string{}
	for model, st := range configChains {
		effectiveChains[model] = st
	}
	for model, st := range runtimeChains {
		effectiveChains[model] = st
	}
	// One row per known model (chain): registry models first, then
	// any chain that has an override but no registry model (a model
	// removed since startup, or a chain named ahead of its model).
	// An empty list before any provider is configured is the honest
	// empty state — no chains are invented.
	modelIDs := s.chainModelIDs(r.Context(), effectiveChains)
	// Real cost + measured quality per chain, from the price book
	// and the ledger (see modelQualityMap). Unknown stays unknown:
	// a nil cost is "price unknown", a nil quality with 0 samples
	// is "no attempts yet" — the dashboard shows those words,
	// never $0 or 0%.
	qualityByModel := s.modelQualityMap(r.Context())
	chains := make([]chainStrategy, 0, len(modelIDs))
	priced := 0
	for _, model := range modelIDs {
		override := effectiveChains[model]
		effective := override
		if effective == "" {
			effective = global
		}
		cost, known := s.modelCostUSDPer1M(model)
		if known {
			priced++
		}
		q := qualityByModel[model]
		chains = append(chains, chainStrategy{
			Model: model, Strategy: override, Effective: effective,
			CostUSDPer1M: cost, Quality: q.Rate, QualitySamples: q.Samples,
		})
	}
	priceSource := "none"
	if s.priceBook != nil {
		hasConfig, hasFile := false, s.priceBook.HasFile
		for _, model := range s.priceBook.SortedModels() {
			if s.priceBook.SourceModel(model) == "config" {
				hasConfig = true
				break
			}
		}
		if !hasConfig {
			for _, key := range s.priceBook.SortedPairs() {
				if s.priceBook.SourcePair(key.Provider, key.Model) == "config" {
					hasConfig = true
					break
				}
			}
		}
		switch {
		case hasConfig && hasFile:
			priceSource = "config + price-file"
		case hasConfig:
			priceSource = "config"
		case hasFile:
			priceSource = "price-file"
		}
	}
	// The strategy list itself is fixed vocabulary from the
	// strategy package, with live weights: the weighted entry shows
	// the operator's custom set (or the balanced fallback when none
	// is configured), so the dashboard never shows a weight set the
	// engine is not actually using.
	strategies := make([]strategyInfo, 0, len(strategy.All))
	for _, st := range strategy.All {
		name, rule := strategy.Describe(st)
		w := strategy.WeightsFor(st, scoringWeights(customWeights))
		strategies = append(strategies, strategyInfo{
			ID:   string(st),
			Name: name,
			Rule: rule,
			Weights: strategyWeightsJSON{
				Latency: w.Latency, Success: w.Success, Cost: w.Cost, Quality: w.Quality, Quota: w.Quota,
			},
		})
	}
	var weightedOut *strategyWeightsJSON
	if customSet {
		weightedOut = &customWeights
	}
	// Nestable strategy trees (CHECKLIST 177): the live config
	// trees, verbatim. LastTreePath is the path to the leaf that
	// served the last real request, through the tree that
	// applies to its model (the per-chain tree wins over the
	// global tree, exactly like the engine resolves it). The
	// last request's own trace is preferred when it carries a
	// path: it names the strategy actually applied at the root
	// (a per-request override included), while recomputing from
	// the config can only name the tree's own root strategy.
	var lastTreePath []string
	if tp, ok := s.engine.(traceProvider); ok {
		if trace := tp.LastTrace(); trace != nil && len(trace.StrategyTreePath) > 0 && last != nil {
			// The trace path belongs to the last request only
			// when the trace's serving provider is the one
			// LastRouting names — under concurrency the two
			// "most recent" records can briefly disagree, and
			// a path from a neighbour's request would be a lie
			// about this one.
			for _, att := range trace.Attempts {
				if att.Outcome == "success" && att.Provider == last.Provider && att.Model == last.Model {
					lastTreePath = trace.StrategyTreePath
					break
				}
			}
		}
	}
	if lastTreePath == nil && last != nil && last.Provider != "" {
		tree := (*strategytree.Node)(nil)
		if rc != nil {
			tree = rc.StrategyTrees[last.Model]
			if tree == nil {
				tree = rc.StrategyTree
			}
		}
		lastTreePath = strategytree.LeafPath(tree, last.Provider, last.Model)
	}
	var treeOut *strategytree.Node
	var treesOut map[string]*strategytree.Node
	if rc != nil {
		treeOut = rc.StrategyTree
		treesOut = rc.StrategyTrees
	}
	return strategiesResponse{
		Strategies:       strategies,
		Global:           global,
		DeadlineBudgetMs: deadlineMs,
		WeightedWeights:  weightedOut,
		Chains:           chains,
		Last:             last,
		Persisted:        s.routingPersist != nil,
		PricedModels:     priced,
		PriceSource:      priceSource,
		StrategyTree:     treeOut,
		StrategyTrees:    treesOut,
		LastTreePath:     lastTreePath,
	}
}

// scoringWeights converts the feed's weight JSON to the scoring
// package's Weights (the strategy package's WeightsFor takes the
// scoring type).
func scoringWeights(w strategyWeightsJSON) scoring.Weights {
	return scoring.Weights{Latency: w.Latency, Success: w.Success, Cost: w.Cost, Quality: w.Quality, Quota: w.Quota}
}

// handleStrategies serves GET /dashboard/strategies.
func (s *Server) handleStrategies(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.strategiesFeed(r))
}

// handleStrategiesUpdate serves POST /dashboard/strategies: replace
// the global strategy and/or per-chain overrides with the posted
// ones and return the live feed. Unknown strategy names are a 400
// (nothing is applied); an empty global string clears back to the
// configured default, and an empty per-chain value clears that
// chain's override. The persist function, when wired, runs first
// and its failure leaves routing unchanged.
func (s *Server) handleStrategiesUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update RoutingUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid strategies update: expected {\"global_strategy\": string, \"chains\": {model: strategy}}")
		return
	}
	if update.Global != nil && *update.Global != "" && !strategy.IsValid(*update.Global) {
		writeError(w, http.StatusBadRequest, "unknown strategy "+*update.Global)
		return
	}
	for model, st := range update.Chains {
		if model == "" {
			writeError(w, http.StatusBadRequest, "chains must not contain an empty model")
			return
		}
		if st != "" && !strategy.IsValid(st) {
			writeError(w, http.StatusBadRequest, "unknown strategy "+st+" for chain "+model)
			return
		}
	}
	if s.routingPersist != nil {
		if err := s.routingPersist(update); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save routing update")
			return
		}
	}
	// Runtime apply: only after the persist (when wired) succeeded,
	// so a failed save can never leave runtime and file disagreeing.
	// With no persist function this is the whole update — runtime
	// only, and the feed reports persisted=false.
	if rep, ok := s.engine.(strategyReporter); ok {
		if update.Global != nil {
			rep.SetGlobalStrategy(*update.Global)
		}
		for model, st := range update.Chains {
			rep.SetChainStrategy(model, st)
		}
	}
	s.logger.Printf("strategies: routing updated (global=%v chains=%v persisted=%v)", guardrails.RedactLogLine(fmt.Sprintf("%v", update.Global)), guardrails.RedactLogLine(fmt.Sprintf("%v", update.Chains)), s.routingPersist != nil)
	writeJSON(w, http.StatusOK, s.strategiesFeed(r))
}

// ---------------------------------------------------------------------------
// Model prices (GET /dashboard/prices) + the measured quality signal.
//
// The price feed lists the operator-configured price source verbatim:
// every priced (provider, model) pair plus every model-level
// fallback price, with its input/output/cached prices in USD per 1M
// tokens, its blended price when both sides are known, and where it
// came from ("config" or "price-file"). A pair with no configured
// price is never listed as $0 — it is absent from the book, and
// the strategies feed reports its cost as unknown.
//
// Quality, wherever this server reports it, is the MEASURED
// error-free completion rate from real attempts: successes /
// attempts counted from the ledger's all-time per-provider roll-up
// (the same attempts the Usage section totals), aggregated
// sample-weighted across the providers offering a model. It is NOT
// a benchmark score, NOT an eval judgement and NOT a claimed
// capability rating: it says nothing about answer quality, only
// about how often real attempts completed without error. A model
// with no recorded attempts has unknown quality (nil), never 0%.
// ---------------------------------------------------------------------------

// priceRow is one price in the GET /dashboard/prices payload. The
// pointer fields are nil when that price is unknown (absent in the
// operator's configuration); a present 0 means genuinely free.
// Pair rows carry the provider; model-level rows (ModelLevel true)
// carry no provider and are the FALLBACK price for every provider
// serving that model unless a pair row overrides it for a
// provider — they are not that provider's price.
type priceRow struct {
	Provider            string   `json:"provider,omitempty"`
	Model               string   `json:"model"`
	ModelLevel          bool     `json:"model_level,omitempty"`
	InputUSDPer1M       *float64 `json:"input_usd_per_1m,omitempty"`
	OutputUSDPer1M      *float64 `json:"output_usd_per_1m,omitempty"`
	CachedInputUSDPer1M *float64 `json:"cached_input_usd_per_1m,omitempty"`
	BlendedUSDPer1M     *float64 `json:"blended_usd_per_1m,omitempty"`
	Source              string   `json:"source"`
	// SyncedAt is when the price was last synced from models.dev;
	// set only when Source is "models.dev".
	SyncedAt *time.Time `json:"synced_at,omitempty"`
}

// pricesResponse is the GET /dashboard/prices payload. HasFile
// reports whether a price file was loaded; an empty Prices list
// with HasFile false and no config entries is the honest empty
// state — no prices configured anywhere.
type pricesResponse struct {
	Prices     []priceRow `json:"prices"`
	HasFile    bool       `json:"has_price_file"`
	FileModels int        `json:"price_file_models"`
	// ModelsDev is the opt-in models.dev sync status (CHECKLIST
	// 179). Nil when no sync is configured.
	ModelsDev *modelsDevStatusJSON `json:"models_dev,omitempty"`
}

// handlePrices serves GET /dashboard/prices: the operator's price
// book, verbatim. With no price book wired it answers an empty
// list, not an error.
func (s *Server) handlePrices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resp := pricesResponse{Prices: []priceRow{}}
	if s.priceBook != nil {
		resp.HasFile = s.priceBook.HasFile
		resp.FileModels = s.priceBook.FileModels
		// Pair rows first (one per priced (provider, model)),
		// then model-level fallback rows (one per model with a
		// legacy model-only price). A model priced only per pair
		// has no model-level row; a model with both has both.
		// Synced models.dev rows (CHECKLIST 179) follow the
		// operator pair rows: they are pair prices too, but
		// their source is "models.dev" with a sync time, so
		// they are never mistaken for operator-set prices.
		// A pair the operator priced is never in the synced
		// layer at all (the sync drops it), so no pair appears
		// twice here.
		for _, key := range s.priceBook.SortedPairs() {
			p, _ := s.priceBook.LookupPair(key.Provider, key.Model)
			row := priceRow{Provider: key.Provider, Model: key.Model, Source: s.priceBook.SourcePair(key.Provider, key.Model)}
			row.InputUSDPer1M = p.InputUSDPer1M
			row.OutputUSDPer1M = p.OutputUSDPer1M
			row.CachedInputUSDPer1M = p.CachedInputUSDPer1M
			if blended, ok := p.BlendedUSDPer1M(); ok {
				row.BlendedUSDPer1M = &blended
			}
			resp.Prices = append(resp.Prices, row)
		}
		for _, key := range s.priceBook.SortedSyncedPairs() {
			sp, _ := s.priceBook.SyncedPair(key.Provider, key.Model)
			row := priceRow{Provider: key.Provider, Model: key.Model, Source: "models.dev", SyncedAt: &sp.SyncedAt}
			row.InputUSDPer1M = sp.Price.InputUSDPer1M
			row.OutputUSDPer1M = sp.Price.OutputUSDPer1M
			row.CachedInputUSDPer1M = sp.Price.CachedInputUSDPer1M
			if blended, ok := sp.Price.BlendedUSDPer1M(); ok {
				row.BlendedUSDPer1M = &blended
			}
			resp.Prices = append(resp.Prices, row)
		}
		for _, model := range s.priceBook.SortedModels() {
			p, _ := s.priceBook.LookupModel(model)
			row := priceRow{Model: model, ModelLevel: true, Source: s.priceBook.SourceModel(model)}
			row.InputUSDPer1M = p.InputUSDPer1M
			row.OutputUSDPer1M = p.OutputUSDPer1M
			row.CachedInputUSDPer1M = p.CachedInputUSDPer1M
			if blended, ok := p.BlendedUSDPer1M(); ok {
				row.BlendedUSDPer1M = &blended
			}
			resp.Prices = append(resp.Prices, row)
		}
	}
	resp.ModelsDev = newModelsDevStatusJSON(s.modelsDevSnapshot())
	writeJSON(w, http.StatusOK, resp)
}

// modelQuality is the measured quality of one model: the
// error-free completion rate over real attempts, or unknown.
// Quality is a *float64 so "no attempts yet" (nil) stays distinct
// from a measured 0% (every attempt failed).
type modelQuality struct {
	Rate    *float64 `json:"quality,omitempty"`
	Samples int      `json:"quality_samples"`
}

// modelQualityMap aggregates the ledger's all-time per-provider
// success counts into a per-model quality map. Providers offering a
// model contribute sample-weighted: quality(model) = sum(ok) /
// sum(ok+errors) across those providers. A model is reported only
// when it has recorded attempts of its own (the ledger's per-model
// rows): without that gate, a model with no attempts would inherit
// the rate of its provider's attempts for OTHER models, which
// would misattribute those attempts to it. Models with no recorded
// attempts get no entry — unknown, never invented.
//
// The aggregation is per provider because the engine keeps attempt
// stats per provider: a model offered by two providers blends their
// records, and a provider's rate covers its attempts across the
// models it serves. That is documented wherever the rate is shown,
// and the sample count travels with it so a 100% rate over 2
// attempts is never mistaken for one over 2,000.
func (s *Server) modelQualityMap(ctx context.Context) map[string]modelQuality {
	out := map[string]modelQuality{}
	if s.ledger == nil || s.registry == nil {
		return out
	}
	ml, ok := s.registry.(dashboardModelLister)
	if !ok {
		return out
	}
	sum := s.ledgerSummaryOrEmpty(ctx, time.Time{}, time.Time{})
	byProvider := map[string]struct{ ok, total int }{}
	for _, p := range sum.ByProvider {
		byProvider[p.Provider] = struct{ ok, total int }{p.OK, p.Requests}
	}
	// The gate: only models with recorded attempts of their own
	// are reported, so one model's attempts are never attributed
	// to another model from the same provider.
	ownRequests := map[string]int{}
	for _, m := range sum.ByModel {
		ownRequests[m.Model] += m.Requests
	}
	// Accumulate per model: successes and totals across the
	// providers offering it.
	type acc struct {
		ok, total int
	}
	byModel := map[string]*acc{}
	for _, m := range ml.DashboardModels() {
		st, ok := byProvider[m.ProviderID]
		if !ok || st.total == 0 {
			continue
		}
		a := byModel[m.ID]
		if a == nil {
			a = &acc{}
			byModel[m.ID] = a
		}
		a.ok += st.ok
		a.total += st.total
	}
	for model, a := range byModel {
		if ownRequests[model] == 0 {
			continue
		}
		rate := float64(a.ok) / float64(a.total)
		out[model] = modelQuality{Rate: &rate, Samples: a.total}
	}
	return out
}

// modelCostUSDPer1M returns the cheapest known blended price (USD
// per 1M tokens) for a model from the price book, and whether any
// is known: the cheapest configured (provider, model) pair for the
// model, or the model-level fallback when that is cheaper or the
// only price (see PriceBook.CheapestBlendedUSDPer1M). Per-model
// callers (the strategies feed, the auto-route decision card) name
// no provider, so the cheapest pair is the honest per-model
// number; the per-pair prices are listed by GET /dashboard/prices.
// Unknown when no book is wired, the model has no entry at either
// level, or no entry has both the input and the output price.
func (s *Server) modelCostUSDPer1M(model string) (*float64, bool) {
	if s.priceBook == nil {
		return nil, false
	}
	blended, ok := s.priceBook.CheapestBlendedUSDPer1M(model)
	if !ok {
		return nil, false
	}
	return &blended, true
}

// setStrategyHeader marks a non-streaming response with the strategy
// that ordered its chain, so a client can see why this provider
// served it. It must be called before the response body is written.
// Streaming responses cannot carry it (the strategy is final only
// after the stream ends), so handleChatStream logs it instead.
func (s *Server) setStrategyHeader(w http.ResponseWriter) {
	if rep, ok := s.engine.(strategyReporter); ok {
		if info, ok := rep.LastRouting(); ok && info.Strategy != "" {
			w.Header().Set("X-Codvian-Strategy", info.Strategy)
		}
	}
}

// chatWithMCP runs one non-streaming chat completion, through the
// MCP agent loop when MCP servers are configured. The loop's
// follow-up model calls are s.engine.Chat itself, called
// in-process — the loop never makes an HTTP request, so the
// middleware chain (auth, guardrails, ledger, cache, coalescing)
// is entered exactly once for the client's request. With no MCP
// registry, or a registry with no servers, this is a plain
// engine.Chat call and the loop result is empty.
func (s *Server) chatWithMCP(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, mcptools.LoopResult, error) {
	if s.mcpTools == nil || s.mcpTools.ServerCount() == 0 {
		resp, err := s.engine.Chat(ctx, req)
		return resp, mcptools.LoopResult{}, err
	}
	return s.mcpTools.RunAgentLoop(ctx, s.engine.Chat, req)
}

// setMCPHeader reports the MCP loop on the response, only when the
// loop actually ran (a registry is wired): the mode, how many
// follow-up model calls it made, and whether it stopped on the
// iteration cap. A request that never called a tool carries no
// iteration header — zero iterations is not a loop.
func (s *Server) setMCPHeader(w http.ResponseWriter, loop mcptools.LoopResult) {
	if s.mcpTools == nil {
		return
	}
	w.Header().Set("X-Codvian-MCP-Mode", s.mcpTools.Mode())
	if loop.Iterations > 0 {
		w.Header().Set("X-Codvian-MCP-Iterations", fmt.Sprintf("%d", loop.Iterations))
	}
	if loop.Capped {
		w.Header().Set("X-Codvian-MCP-Capped", "true")
	}
}

// handleEngineError maps an engine failure to a status code. Upstream
// detail is not exposed to the client. A spent deadline budget is a
// clean 504 (the combo stopped instead of burning more attempts),
// distinct from a plain upstream timeout. A terminal upstream
// failure (entries 151/183 — the request itself was refused as
// malformed, forbidden or unrepresentable) maps to the upstream's
// own 4xx, not a 502: a 502 would claim the gateway failed, when
// the request was the problem. Both upstream branches count as
// upstream failures (see writeUpstreamFailure); the 504 deadline
// stop does not — it is a router-side stop, not an upstream
// failure. A local guardrail policy refusal never reaches this
// function: guardrail blocks are answered in applyGuardrails /
// applyResponseGuardrails with 403 / -32001, so the two can never
// collide here. An UPSTREAM content refusal DOES reach it
// (CHECKLIST 161): every target in the refusal chain refused the
// content, so the answer is the same 403 / -32001 — with reason
// "upstream_content_refusal" — and it is never counted as an
// upstream failure (a refusal says nothing about provider health).
func (s *Server) handleEngineError(w http.ResponseWriter, err error) {
	// Upstream content refusal (CHECKLIST 161): the refusal
	// chain ran dry (ErrContentRefused) or a refusal stopped the
	// request mid-chain (a bare content-refusal error, e.g. a
	// replay-exempt request). Either way the answer is an honest
	// refusal — HTTP 403 with the policy-refusal code -32001 and
	// reason "upstream_content_refusal" — never a 502 (nothing
	// failed upstream: the targets answered, they refused the
	// content) and never an upstream-failure count.
	if errors.Is(err, router.ErrContentRefused) ||
		provider.ClassifyError(err, provider.EndpointChatCompletions).ContentRefusal {
		s.writeUpstreamContentRefusal(w)
		return
	}
	// Per-request routing-contract rejection (CHECKLIST 157): the
	// request was refused before any provider was dialled, so it
	// is a client error with the contract's own status (400 for a
	// bad control or an empty result set, 402 when the price
	// ceiling left nothing) — never a 502, and never counted as
	// an upstream failure.
	var ce *router.ContractError
	if errors.As(err, &ce) {
		writeError(w, ce.Status, ce.Msg)
		return
	}
	// Per-key token limit (M5.3): every target in the chain was
	// already at its window cap, so the engine refused BEFORE
	// any provider was dialled. A 429 with Retry-After to the
	// window reset and the error type "key_token_limit" — the
	// same shape as the pre-call gate — never a 502, and never
	// counted as an upstream failure.
	var tb *router.TokenLimitBreach
	if errors.As(err, &tb) {
		seconds := int(time.Until(tb.NextReset) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: tb.Error(), Type: "key_token_limit"}})
		return
	}
	// Internal budget, provider scope (M5.11): every target in
	// the chain was over its provider's internal budget, so the
	// engine refused BEFORE any provider was dialled. A 402,
	// the same status as the pre-call internal-budget gate and
	// the key/global budget refusals, error type
	// "internal_budget".
	var ib *router.InternalBudgetBreach
	if errors.As(err, &ib) {
		writeJSON(w, http.StatusPaymentRequired, errorBody{Error: errorDetail{Message: ib.Error(), Type: "internal_budget"}})
		return
	}
	// Context-window no-fit (CHECKLIST 159): the prompt fits no
	// configured model, so the engine refused BEFORE any provider
	// was called. That is a property of the request + the
	// operator's configuration, not an upstream failure: a 400
	// with the honest message (required vs largest window), and
	// no upstream-failure counter — nothing upstream failed.
	var tooLarge *router.ErrContextTooLarge
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusBadRequest, tooLarge.Error())
		return
	}
	// Embeddings/rerank context no-fit (P1.8): the input fits no
	// configured model, so the engine refused BEFORE any provider was
	// called — a 400, no upstream-failure counter.
	var embTooLarge *router.EmbeddingContextTooLarge
	if errors.As(err, &embTooLarge) {
		writeError(w, http.StatusBadRequest, embTooLarge.Error())
		return
	}
	// Reasoning state no target could represent (CHECKLIST 163):
	// every hop failed because the target wire cannot carry the
	// restored reasoning state (e.g. Anthropic thinking without
	// a signature, or any reasoning state on Cohere). That is a
	// property of the request's reasoning state + the configured
	// targets, not an upstream failure: an honest 400 naming the
	// wires, and no upstream-failure counter — nothing upstream
	// failed. The detail names wires and models only, never
	// reasoning or prompt text.
	var runrep *reasoning.UnrepresentableError
	if errors.As(err, &runrep) {
		writeError(w, http.StatusBadRequest, "reasoning state cannot be represented by the selected provider(s): "+runrep.Detail)
		return
	}
	// Per-request budget, strict mode (CHECKLIST 155): the
	// engine stopped the chain before dialling a provider whose
	// priced prompt estimate would have crossed the client's
	// cap. 402, like the pre-send refusal — never a 502
	// (nothing failed upstream) and never -32603, and no
	// upstream-failure counter for the same reason.
	if router.IsRequestBudgetExceeded(err) {
		writeError(w, http.StatusPaymentRequired, err.Error())
		return
	}
	if errors.Is(err, router.ErrDeadlineExceeded) {
		writeServerError(w, http.StatusGatewayTimeout, "deadline budget exceeded")
		return
	}
	// Stream-lifetime cap (CHECKLIST 255): the engine terminated
	// the stream at its max lifetime — a gateway-side time bound,
	// like the deadline budget, so the answer is a 504 naming
	// it, never a 502 (nothing failed upstream: the provider was
	// still streaming when the gateway ended it).
	if errors.Is(err, router.ErrStreamLifetimeExceeded) {
		writeServerError(w, http.StatusGatewayTimeout, "stream lifetime exceeded")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeServerError(w, http.StatusGatewayTimeout, "request timed out")
		return
	}
	// Upstream rate limit (AUD-REL-001): the provider answered 429.
	// That is the upstream's own answer, not a gateway failure, and it
	// used to collapse into the generic 502 with Retry-After dropped —
	// so a client (or its SDK) could not back off and instead retried
	// straight back into the same wall. Propagate the true status and
	// the upstream's Retry-After, exactly as the images/embeddings
	// paths already do. Not counted as an upstream failure: nothing in
	// the gateway failed.
	//
	// This is the UPSTREAM's limit. Internal quota exhaustion (our own
	// key/global budgets, per-key token windows) is refused earlier in
	// this function with its own 402 / 429 + key_token_limit shape, so
	// the two are never confused.
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		if rle.RetryAfterPresent {
			seconds := int(rle.RetryAfter / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
		}
		writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{
			Message: "upstream provider rate limited the request",
			Type:    "rate_limit_error",
		}})
		return
	}
	// Upstream quota exhaustion (AUD-REL-001): the provider answered
	// its own quota status (402/429 per vendor). As above, the
	// upstream's status is the truthful answer; a 502 would hide it.
	// The upstream body is NOT echoed into the chat envelope (it can
	// carry vendor account detail); the status and a stable type are.
	var qe *provider.QuotaError
	if errors.As(err, &qe) {
		status := qe.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusPaymentRequired
		}
		writeJSON(w, status, errorBody{Error: errorDetail{
			Message: "upstream provider quota exhausted",
			Type:    "quota_exceeded",
		}})
		return
	}
	// AUD-REL-001: a classified 4xx keeps the upstream's own status with
	// the documented "upstream_error" shape (no JSON-RPC code — the
	// status IS the code); everything else falls through to
	// writeUpstreamFailure's 502 + -32603 + "server_error" signature.
	// This is the pre-existing contract, only widened from
	// DecisionStop-4xx to any 4xx, so a retryable classification (429)
	// no longer collapses into 502.
	if cls := provider.ClassifyError(err, provider.EndpointChatCompletions); cls.Status >= 400 && cls.Status < 500 {
		if s.recorder != nil {
			s.recorder.IncUpstreamFailure()
		}
		writeUpstreamError(w, cls.Status, "upstream rejected the request")
		return
	}
	s.writeUpstreamFailure(w)
}

// writeUpstreamContentRefusal answers an exhausted refusal
// chain (CHECKLIST 161): HTTP 403 with the Codvian
// policy-refusal code -32001 and the machine-readable reason
// "upstream_content_refusal", so a client can tell an upstream
// content refusal from a local guardrail block (reason pii /
// secret / injection) and from an upstream failure (-32603)
// without parsing message strings. It is NOT counted as an
// upstream failure: the targets answered, they refused the
// content — nothing failed upstream.
func (s *Server) writeUpstreamContentRefusal(w http.ResponseWriter) {
	writeJSON(w, http.StatusForbidden, errorBody{Error: errorDetail{
		Message: "upstream providers refused the content",
		Type:    "policy_refusal",
		Code:    guardrails.PolicyRefusalCode,
		Reason:  "upstream_content_refusal",
	}})
}

// writeUpstreamFailure answers an exhausted-chain upstream failure:
// HTTP 502 with the JSON-RPC internal-error code -32603 (see
// guardrails.UpstreamFailureCode), so a client can tell it from a
// policy refusal (-32001) without parsing message strings. It also
// counts the failure for /metrics and the guardrails feed.
func (s *Server) writeUpstreamFailure(w http.ResponseWriter) {
	if s.recorder != nil {
		s.recorder.IncUpstreamFailure()
	}
	writeJSON(w, http.StatusBadGateway, errorBody{Error: errorDetail{
		Message: "upstream provider error",
		Type:    "server_error",
		Code:    guardrails.UpstreamFailureCode,
	}})
}

type chatCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chatChoiceJSON `json:"choices"`
	Usage   clientUsageBlock `json:"usage"`
}

// clientUsageBlock is the client-facing OpenAI usage projection
// (G1): the flat counts plus the nested detail blocks OpenAI SDKs
// read (prompt_tokens_details.cached_tokens,
// completion_tokens_details.reasoning_tokens). The flat
// cached_tokens field is kept for existing gateway consumers.
// Detail blocks appear only when the upstream actually reported a
// nonzero value — never invented. Router-internal bookkeeping
// (cache-write tiers, reported cost, local estimates, service tier)
// never leaks into this block: it carries only what the client is
// meant to see.
type clientUsageBlock struct {
	PromptTokens            int                            `json:"prompt_tokens"`
	CompletionTokens        int                            `json:"completion_tokens"`
	TotalTokens             int                            `json:"total_tokens"`
	CachedTokens            int                            `json:"cached_tokens,omitempty"`
	PromptTokensDetails     *clientPromptTokensDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *clientCompletionTokensDetails `json:"completion_tokens_details,omitempty"`
}

type clientPromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type clientCompletionTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// projectClientUsage projects a provider-reported usage block into
// the client-facing shape. A zero usage block projects to zero
// counts (the caller decides whether to emit it at all). Negative
// counts are malformed upstream output and clamp to 0 — the client
// never sees negative tokens (P1-09; the ledger clamps identically).
func projectClientUsage(u provider.Usage) clientUsageBlock {
	u = u.NonNegative()
	b := clientUsageBlock{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CachedTokens:     u.CachedTokens,
	}
	if b.CachedTokens > 0 {
		b.PromptTokensDetails = &clientPromptTokensDetails{CachedTokens: b.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		b.CompletionTokensDetails = &clientCompletionTokensDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return b
}

type chatChoiceJSON struct {
	Index        int              `json:"index"`
	Message      provider.Message `json:"message"`
	FinishReason string           `json:"finish_reason"`
}

// ---------------------------------------------------------------------------
// Streaming (SSE) with failover-safe resume.
// ---------------------------------------------------------------------------

type streamDelta struct {
	Role      string                   `json:"role,omitempty"`
	Content   string                   `json:"content,omitempty"`
	ToolCalls []provider.ChunkToolCall `json:"tool_calls,omitempty"`
}

type streamChoice struct {
	Index        int         `json:"index"`
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	// Usage is set only on the final usage chunk (G1): emitted
	// when the client asked for stream_options.include_usage AND
	// the upstream reported usage. Choices is an empty array on
	// that chunk, exactly like OpenAI's wire shape.
	Usage *clientUsageBlock `json:"usage,omitempty"`
}

// handleChatStream streams SSE deltas to the client. It returns nil
// on a completed stream and the engine error otherwise, so the caller
// can record metrics. A stream that served a stale cached response
// cannot carry the X-Codvian-Stale header (staleness is known only
// after the stream ends), so it is logged instead.
//
// asyncRun is the concurrent async guardrail check for this request
// (CHECKLIST 185; nil when no async guardrail is configured): the
// stream runs under a child context the async verdict can cancel,
// and a BLOCK verdict cuts it with the terminal refusal frame
// (403 / -32001) instead of an upstream error.
// streamChunkRedactor is the per-stream content filter in
// handleChatStream: the PEM-only redactor, or — when response
// sanitization is on — the store-wired ResponseStreamRedactor,
// which runs the PEM redactor first and then redacts PII/secrets
// line by line. Both types share the WriteChunk/Flush shape, so
// the stream code below does not branch on which one is live.
type streamChunkRedactor interface {
	WriteChunk(chunk string) string
	Flush() string
}

func (s *Server) handleChatStream(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest, asyncRun *guardrails.AsyncRun, start time.Time) (err error) {
	id := newChatID()
	created := time.Now().Unix()

	// Async guardrail mode (CHECKLIST 185): the stream runs under
	// a child context the async verdict can cancel. When a BLOCK
	// verdict lands, the cut goroutine cancels with the block as
	// the cause; the engine records the cut (trace +
	// prompt-estimate ledger row, no health signal) and the
	// handler below writes the terminal refusal frame. The parent
	// timeout still applies — cancelling the parent cancels this
	// child too. The deferred cancel is a no-op once the stream
	// ends (the context is abandoned either way).
	streamCtx, streamCancel := context.WithCancelCause(ctx)
	defer streamCancel(nil)
	if asyncRun != nil {
		go func() {
			select {
			case res, ok := <-asyncRun.Blocked():
				if !ok {
					return // run finished with no block: no cut
				}
				streamCancel(&guardrails.AsyncBlockError{Guardrail: res.Name, Kind: res.Kind})
			case <-streamCtx.Done():
			}
		}()
	}

	// Lossless-first compression (CHECKLIST 183): the headers
	// reporting it are set before any SSE header can be sent, so
	// a lossy stream is disclosed from the first byte, like a
	// non-streaming response. req is already compressed by
	// handleChatCompletions at this point.
	s.setCompressionHeader(w, &req)
	// Telemetry stream-start headers (M2.1): request id, version
	// and the requested model are known before the first event;
	// cost/tokens/fallback ride the final trailer instead.
	s.writeStreamStartHeaders(w, ctx, req.Model)

	// AUD-PERF-001: the full text already sent was retained in a
	// strings.Builder that was only ever read for its length, so every
	// in-flight stream held its entire completion in memory to compute a
	// boolean — memory grew with concurrency x response size, uncapped.
	// A byte counter carries exactly the same information (whether any
	// content has been delivered) at O(1) memory.
	var sentBytes int // bytes of content already sent to the client
	headersSent := false
	flusher, _ := w.(http.Flusher)

	// SSE delivery batching (items 135-136): token events go
	// through the batcher — one Flush per 3 tokens or 8 ms,
	// whichever comes first — so a fast stream pays ~1/3 the
	// flushes. [DONE] and error events use writeSSENow below:
	// they flush any pending batch first and then flush
	// immediately themselves, so a stream close is never held
	// up to 8 ms. With batching disabled (measurement only)
	// writeSSE keeps the old flush-per-token behaviour.
	var batch *sseBatcher
	if s.sseBatching.enabled {
		batch = newSSEBatcher(
			func(line string) error { _, err := io.WriteString(w, line); return err },
			func() {
				if flusher != nil {
					flusher.Flush()
				}
			},
			s.sseBatching.maxTokens, s.sseBatching.maxDelay, nil,
		)
		defer batch.Close() // never leave a token buffered past the handler
	}

	ensureHeaders := func() {
		if headersSent {
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		headersSent = true
		// Async guardrail mode (CHECKLIST 185): the first byte is
		// out — a BLOCK verdict landing after this point can no
		// longer withhold the response, only cut the stream.
		asyncRun.MarkDelivered()
	}
	writeSSE := func(line string) error {
		ensureHeaders()
		if batch != nil {
			return batch.Write(line)
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	// writeSSENow is for [DONE] and error events: flush any
	// pending batch, then write this line and flush at once.
	writeSSENow := func(line string) error {
		ensureHeaders()
		if batch != nil {
			if err := batch.Flush(); err != nil {
				return err
			}
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	// Panic isolation (CHECKLIST 165-166): a panic in the server's
	// own stream code — the onChunk closure (JSON marshal, SSE
	// write), the batcher, finish() — must end this stream with a
	// terminal error frame, never kill the gateway. Provider and
	// hook panics are already converted to errors by the engine;
	// this guards the server side of the same stream. It is
	// registered after writeSSENow exists so the terminal frame
	// can use it (which flushes any pending batch first, so tokens
	// already queued still precede the error frame, in order);
	// panics before this point are caught by the outer recover in
	// handleChatCompletions — headers have not been sent yet
	// there, so its 502 is still possible. The deferred
	// batch.Close() (registered earlier) runs after this recover,
	// flushing nothing — the frame already flushed the batch.
	defer func() {
		if v := recover(); v != nil {
			pe := s.noteServerPanic("server_stream", v)
			err = pe
			if !headersSent {
				s.writePanicFailure(w)
				return
			}
			payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "gateway internal error", Type: "server_error"}})
			_ = writeSSENow("data: " + string(payload) + "\n\n")
			_ = writeSSENow("data: [DONE]\n\n")
		}
	}()
	writeChunk := func(delta streamDelta, finishReason *string) error {
		b, err := json.Marshal(streamChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
			Choices: []streamChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
		})
		if err != nil {
			return err
		}
		return writeSSE("data: " + string(b) + "\n\n")
	}

	first := true
	finishSent := false // a chunk already carried finish_reason
	decisionSet := false
	// Stream redaction (M1.7 + B3-22): PEM private-key blocks split
	// across SSE chunks are withheld by the redactor and replaced
	// with one placeholder; emitTail below emits the withheld tail
	// before [DONE] / the error frame. One redactor per stream
	// (not safe for concurrent use). With response sanitization
	// opted in, the redactor is the store-wired
	// ResponseStreamRedactor instead: it runs the same PEM
	// redactor first, then redacts PII/secrets line by line in
	// the store's mode — and in block mode it cuts the stream at
	// the first line carrying a finding (see the cut check in
	// onChunk and after the engine call). With sanitization off,
	// the PEM-only redactor is exactly as before.
	var respRedactor *guardrails.ResponseStreamRedactor
	var redactor streamChunkRedactor = guardrails.NewStreamRedactor()
	if s.responseSanitization {
		respRedactor = s.guardrails.NewResponseStreamRedactor("")
		redactor = respRedactor
	}
	vaultRestorer := guardrails.NewStreamRestorer(s.reversibleVault)
	// Stream tool-call guardrail (B3-22, Stage 1): tool-call
	// argument fragments arrive split across chunks, so a
	// secret split across two fragments matches neither — the
	// check must run on the accumulated arguments, not per
	// fragment. The non-streaming check scans tool-call
	// arguments (responseText/redactResponse); the stream
	// never did — onChunk emitted chunk.ToolCalls verbatim.
	// With response sanitization on, each tool call's
	// arguments are accumulated per index and checked once,
	// when the call is complete (a finish reason in the
	// chunk, or stream end), with the store's CheckResponse:
	// the same check as the non-streaming path, in the
	// store's mode (redact/warn/block), recorded under phase
	// "response" with per-type toggles and custom patterns.
	// The checked call is emitted as one delta carrying the
	// full arguments, before the finish chunk. With
	// sanitization off, tool calls flow verbatim exactly as
	// before. A blocked tool call cuts the stream like a
	// blocked content line (see the cut check after the
	// engine call).
	type streamToolCall struct {
		index int
		id    string
		name  string
		args  strings.Builder
	}
	var pendingToolCalls []*streamToolCall
	toolCallBlocked := false
	toolCallBlockKind := ""
	// emitToolCall checks one accumulated tool call and emits
	// it as a single delta. It returns
	// errResponseGuardrailBlocked when the store's block mode
	// withholds the call; the cut check after the engine
	// call then ends the stream with the terminal refusal
	// frame.
	emitToolCall := func(tc *streamToolCall) error {
		checked := s.guardrails.CheckResponse(tc.args.String(), "")
		if checked.Blocked {
			toolCallBlocked = true
			if len(checked.Findings) > 0 {
				toolCallBlockKind = checked.Findings[0].Kind
			}
			return errResponseGuardrailBlocked
		}
		d := streamDelta{ToolCalls: []provider.ChunkToolCall{{
			Index: tc.index, ID: tc.id, Name: tc.name, Arguments: checked.Body,
		}}}
		if first {
			d.Role = "assistant"
			first = false
		}
		if err := writeChunk(d, nil); err != nil {
			return err
		}
		sentBytes += len(checked.Body)
		return nil
	}
	// flushToolCalls emits every accumulated tool call, in
	// first-seen order. A blocked call stops the flush: the
	// remaining calls are never emitted and the stream is cut.
	flushToolCalls := func() error {
		for _, tc := range pendingToolCalls {
			if err := emitToolCall(tc); err != nil {
				return err
			}
		}
		pendingToolCalls = nil
		return nil
	}
	// emitTail writes the redactor's withheld tail as one final
	// content chunk. It bypasses WriteChunk on purpose: the tail
	// already passed through Flush — feeding it back into
	// WriteChunk would re-withhold a trailing partial header
	// (any tail ending in "-" is held for the next chunk) and the
	// bytes would be lost. It mirrors onChunk's first-chunk role
	// and decision-header handling, so a stream whose entire
	// content was withheld still opens with a proper chunk.
	emitTail := func() {
		tail := redactor.Flush()
		if tail != "" {
			tail = vaultRestorer.WriteChunk(tail)
		}
		tail += vaultRestorer.Flush()
		if tail == "" {
			return
		}
		if !decisionSet {
			decisionSet = true
			s.setStreamDecisionHeader(w, req.Model)
		}
		d := streamDelta{Content: tail}
		if first {
			d.Role = "assistant"
			first = false
		}
		if err := writeChunk(d, nil); err != nil {
			return
		}
		sentBytes += len(tail)
	}
	onChunk := func(chunk provider.StreamChunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Decision transparency (CHECKLIST 199): the header goes
		// on the first chunk, before the first SSE write commits
		// the headers. The engine published the decision at
		// stream commit, immediately before this first event was
		// sent, so the record read here is this request's. A
		// cache-replayed stream has no provider decision: the
		// setter suppresses it (see decision.go).
		if !decisionSet {
			decisionSet = true
			s.setStreamDecisionHeader(w, req.Model)
		}
		content := redactor.WriteChunk(chunk.Content)
		content = vaultRestorer.WriteChunk(content)
		// Response sanitization cut (B3-22, block mode): the
		// redactor withheld the first line carrying a finding,
		// so nothing further may be emitted. Returning the
		// sentinel stops the engine loop; the terminal refusal
		// frame is written by the cut check after the engine
		// call below. Without this return the stream would keep
		// running silently and close with a normal [DONE], as if
		// the response had been delivered in full.
		if respRedactor != nil && respRedactor.Blocked() {
			return errResponseGuardrailBlocked
		}
		// Stream tool-call guardrail (B3-22, Stage 1): with
		// sanitization on, accumulate argument fragments per
		// tool-call index instead of emitting them verbatim.
		// The calls are checked and emitted by flushToolCalls
		// once complete (finish reason below, or stream end).
		// With sanitization off this block is skipped and the
		// delta below carries the tool calls exactly as before.
		if respRedactor != nil {
			for _, tc := range chunk.ToolCalls {
				var acc *streamToolCall
				for _, p := range pendingToolCalls {
					if p.index == tc.Index {
						acc = p
						break
					}
				}
				if acc == nil {
					acc = &streamToolCall{index: tc.Index}
					pendingToolCalls = append(pendingToolCalls, acc)
				}
				if acc.id == "" {
					acc.id = tc.ID
				}
				if acc.name == "" {
					acc.name = tc.Name
				}
				acc.args.WriteString(tc.Arguments)
			}
		}
		d := streamDelta{Content: content}
		if respRedactor == nil {
			d.ToolCalls = chunk.ToolCalls
		}
		if first {
			d.Role = "assistant"
			first = false
		}
		var fr *string
		if chunk.FinishReason != "" {
			fr = &chunk.FinishReason
			finishSent = true
		}
		// A finish reason completes every pending tool call:
		// check and emit them before the finish chunk, so the
		// client sees the calls before the turn ends.
		if fr != nil && respRedactor != nil {
			if err := flushToolCalls(); err != nil {
				return err
			}
		}
		if d.Content == "" && len(d.ToolCalls) == 0 && fr == nil {
			return nil
		}
		if err := writeChunk(d, fr); err != nil {
			return err
		}
		sentBytes += len(content)
		return nil
	}
	onDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		return onChunk(provider.StreamChunk{Content: delta})
	}
	// finalUsage (G1): set on the success path only, when the
	// client requested stream_options.include_usage and the
	// upstream actually reported usage. finish emits it as the
	// final usage chunk (empty choices array) after the finish
	// chunk and before [DONE]. A local estimate is NEVER emitted
	// here — the client asked for the provider's numbers.
	var finalUsage *clientUsageBlock
	finish := func(reason string) error {
		// A chunk already carried the finish reason: skip the
		// finish chunk, but the usage chunk (if any) still goes
		// out before [DONE].
		if !finishSent {
			if reason == "" {
				reason = "stop"
			}
			fr := reason
			if err := writeChunk(streamDelta{}, &fr); err != nil {
				return err
			}
		}
		if finalUsage != nil {
			b, err := json.Marshal(streamChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: req.Model,
				Choices: []streamChoice{}, Usage: finalUsage,
			})
			if err != nil {
				return err
			}
			if err := writeSSENow("data: " + string(b) + "\n\n"); err != nil {
				return err
			}
		}
		return writeSSENow("data: [DONE]\n\n")
	}

	var resp *provider.ChatResponse
	if ce, ok := s.engine.(chunkEngine); ok {
		resp, err = ce.ChatStreamChunks(streamCtx, req, onChunk)
	} else {
		resp, err = s.engine.ChatStream(streamCtx, req, onDelta)
	}
	// A panic the engine recovered (provider/hook): log its
	// redacted value once, here — the stream path owns this log
	// line, so handleChatCompletions does not log it again.
	s.logPanicError(err)
	// Stream tool-call guardrail (B3-22, Stage 1): a stream
	// whose finish reason never arrived in a chunk still holds
	// accumulated tool calls — check and emit them now, before
	// the cut checks below, so no call is silently dropped. A
	// blocked call sets toolCallBlocked and the sanitization
	// cut check below ends the stream with the terminal
	// refusal frame instead of [DONE]; a write error is a
	// failed stream and takes the error path below.
	if err == nil && respRedactor != nil && len(pendingToolCalls) > 0 {
		if ferr := flushToolCalls(); ferr != nil && ferr != errResponseGuardrailBlocked {
			err = ferr
		}
	}
	// Async guardrail cut (CHECKLIST 185): a BLOCK verdict landed
	// while the stream was in flight, and the cut goroutine
	// cancelled the stream context with it. The engine already
	// recorded the cut attempt (trace + prompt-estimate ledger
	// row, no health signal); the client gets the terminal refusal
	// frame — the same 403 / -32001 policy-refusal shape as the
	// sync path. When nothing was sent yet, headers have not gone
	// out, so the plain 403 JSON is still possible (and matches
	// the sync refusal exactly); once content was committed, the
	// stream ends with the SSE refusal frame + [DONE]. Either way
	// this is a policy refusal, never an upstream failure: no
	// upstream-failure count, no failover, and the returned
	// sentinel (errAsyncGuardrailBlocked) records the request as
	// an error in metrics without implicating any provider. Only
	// a failed stream can have been cut: a stream that completed
	// (err == nil) delivered its full response, and a BLOCK that
	// landed after that is recorded by the run as
	// blocked_after_delivery — too late to act, and said so.
	if err != nil && asyncRun != nil {
		if res, blocked := asyncRun.BlockVerdict(); blocked {
			if sentBytes == 0 && !headersSent {
				writePolicyRefusal(w, res.Kind, "request")
			} else {
				payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "guardrail blocked stream: " + res.Kind, Type: "policy_refusal", Code: guardrails.PolicyRefusalCode, Reason: res.Kind}})
				_ = writeSSENow("data: " + string(payload) + "\n\n")
				_ = writeSSENow("data: [DONE]\n\n")
			}
			s.logger.Printf("guardrail: async %s blocked stream after start (kind=%s); stream cut, terminal refusal -32001", res.Name, res.Kind)
			// No trailer on a cut stream — record it
			// server-side as a policy refusal.
			s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
			return errAsyncGuardrailBlocked
		}
	}
	// Response sanitization cut (B3-22, block mode): the
	// ResponseStreamRedactor cut the stream at the first line
	// carrying a finding, or a streamed tool call blocked at
	// the accumulated check (toolCallBlocked). This check does
	// NOT require err != nil: an engine that ignores the
	// onChunk error returns nil, and the stream must still
	// end with a refusal frame, never a normal [DONE]. The
	// decision uses the redactor's flag (or the tool-call
	// flag), not the error value, so it holds either way. The
	// withheld tail is never emitted (Flush returns "" once
	// cut). Like the async cut, this is a policy refusal,
	// never an upstream failure: no upstream-failure count,
	// no failover.
	if respRedactor != nil && (respRedactor.Blocked() || toolCallBlocked) {
		kind := "secret"
		if toolCallBlocked && toolCallBlockKind != "" {
			kind = toolCallBlockKind
		} else if f := respRedactor.Findings(); len(f) > 0 {
			kind = f[0].Kind
		}
		if sentBytes == 0 && !headersSent {
			writePolicyRefusal(w, kind, "response")
		} else {
			payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "guardrail blocked stream: " + kind, Type: "policy_refusal", Code: guardrails.PolicyRefusalCode, Reason: kind}})
			_ = writeSSENow("data: " + string(payload) + "\n\n")
			_ = writeSSENow("data: [DONE]\n\n")
		}
		s.logger.Printf("guardrail: response sanitization blocked stream (kind=%s); stream cut, terminal refusal -32001", kind)
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
		return errResponseGuardrailBlocked
	}
	if err == nil {
		if sr, ok := s.engine.(staleReporter); ok && sr.LastServedStale() {
			s.logger.Printf("stream served stale response (age %s)", sr.LastStaleAge())
		}
		if chr, ok := s.engine.(cacheHitReporter); ok {
			if kind, sim, hit := chr.LastCacheHit(); hit {
				s.logger.Printf("stream served from %s cache (similarity %.3f)", kind, sim)
			}
		}
		if dec, ok := s.requestDecision(ctx); ok {
			s.logger.Printf("stream auto-routed to tier %s (confidence %.3f, model %s, fallback %v)", dec.Tier, dec.Confidence, guardrails.Redact(dec.Model), dec.Fallback)
			// Streaming responses cannot carry the decision id as
			// a header (the decision is final only after the
			// stream ends), but the decision is still registered
			// so feedback can attach to it via /dashboard/feedback.
			s.registerAutoDecision(dec)
		}
		reason := "stop"
		if resp != nil && resp.FinishReason != "" {
			reason = resp.FinishReason
		}
		// Plugin post-response hooks (CHECKLIST 201): the
		// stream is committed by now, so this is observation
		// only (fail-open, same as the non-stream path).
		s.applyPostResponsePlugins(ctx, &req, resp)
		// Stream redaction (M1.7): emit the redactor's withheld
		// tail — an unclosed PEM block as one placeholder — as a
		// final chunk before [DONE], so no secret reaches the
		// client in any chunking. Withheld text was never sent,
		// so a stream that errored needs no flush: the error
		// frame ends it instead.
		emitTail()
		// G1: the client asked for stream_options.include_usage —
		// emit the upstream's reported usage as the final usage
		// chunk (finish writes it before [DONE]). Only a real
		// report qualifies: a zero usage block means the upstream
		// reported nothing, and a local estimate is never presented
		// as provider-reported.
		if req.StreamIncludeUsage && resp != nil && resp.Usage != (provider.Usage{}) {
			u := projectClientUsage(resp.Usage)
			finalUsage = &u
		}
		// G1: a stream whose upstream reported usage is priced for
		// the per-request budget exactly like a non-streaming
		// response — the overage feed records it; the spend rides
		// the telemetry trailer below (headers are already out).
		s.checkRequestBudgetOverageStream(ctx, &req, resp)
		// Telemetry trailer (M2.1): the values that were not
		// known when the stream's headers were sent — cost,
		// tokens, fallback — as a final SSE comment before
		// [DONE], and in the log row. writeSSENow flushes any
		// batched token events first, so the trailer cannot
		// overtake them.
		meta := s.responseMeta(ctx, req, resp, start)
		if line := s.streamTrailerString(ctx, meta); line != "" {
			_ = writeSSENow(line)
		}
		s.logResponseMeta(meta)
		s.recordTelemetry(ctx, meta, http.StatusOK)
		_ = finish(reason)
		return nil
	}

	// The engine failed. If nothing was sent yet, headers have not gone
	// out (they are written lazily on the first chunk), so a plain JSON
	// error with a 502/504 status is still possible.
	if sentBytes == 0 && !headersSent {
		// Strict per-request budget stop (CHECKLIST 155):
		// record the mid-chain refusal for the dashboard feed
		// before mapping the typed error to 402.
		s.noteRequestBudgetMidChain(&req, err)
		// Telemetry headers (M2.1): the trace still names the
		// attempted chain; tokens and cost stay absent.
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleEngineError(w, err)
		return err
	}

	// Partial content already reached the client: the stream is
	// committed, so it NEVER switches to another provider
	// (research-log entries 97 + 111 — no switch after the first
	// content byte). There is no resume here: a resume would
	// re-issue the request on a new target after the client
	// already owns the failure. End the stream with an SSE error
	// event rather than duplicating content or hanging.
	// Upstream error bodies can echo prompt/response text, which no
	// pattern redactor can strip: log the status only, never the
	// body (see logSafeError). The alert names the deployment
	// that failed the stream (CHECKLIST 243, LiteLLM #51034).
	//
	// Stream redaction (M1.7): the redactor may be holding a
	// trailing partial header (any chunk ending in "-" holds it
	// for the next chunk) — emit the withheld tail as a final
	// chunk before the error frame, or those bytes are lost.
	// Withheld text was never sent unredacted, so this cannot
	// leak; dropping it would corrupt the delivered prefix.
	emitTail()
	errDetail := s.streamErrorDetail()
	// Stream-lifetime cap (CHECKLIST 255): the error frame names
	// the cap, not a generic upstream failure — the message is
	// gateway-generated (the cap that fired), so unlike an
	// upstream body it cannot echo prompt or response text.
	if errors.Is(err, router.ErrStreamLifetimeExceeded) {
		errDetail = errorDetail{Message: err.Error(), Type: "server_error", Deployment: errDetail.Deployment}
	}
	s.logger.Printf("stream failed after %d bytes sent: deployment=%s %v", sentBytes, errDetail.Deployment, logSafeError(err))
	payload, _ := json.Marshal(errorBody{Error: errDetail})
	_ = writeSSENow("data: " + string(payload) + "\n\n")
	// Always-terminal (CHECKLIST 153): a committed stream ends
	// with [DONE] even after an error frame, so a client waiting
	// on the terminator never hangs. Before this slice the error
	// frame was the last line and a client that had already
	// received content waited forever for [DONE]; the error event
	// still comes first, so a client that stops at the error
	// frame sees the same bytes as before, plus the terminator.
	// Best-effort when the client already went away (a client
	// abort): the writes fail silently and the handler returns.
	_ = writeSSENow("data: [DONE]\n\n")
	// Committed-error stream: no trailer (A8 rule) — record it
	// server-side with the engine-error status.
	s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), s.statusForEngineError(err))
	return err
}

// trimAlreadySent removes the portion of delta that the client has already
// received: the longest suffix of sent that is also a prefix of delta.
// Returns delta unchanged when there is no overlap.
func trimAlreadySent(sent, delta string) string {
	max := len(sent)
	if len(delta) < max {
		max = len(delta)
	}
	for n := max; n > 0; n-- {
		if sent[len(sent)-n:] == delta[:n] {
			return delta[n:]
		}
	}
	return delta
}

func newChatID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Logging middleware (route, model, status, latency only).
// ---------------------------------------------------------------------------

type ctxKeyLogFields struct{}

type logFields struct {
	model string
}

func logFieldsFrom(ctx context.Context) *logFields {
	lf, _ := ctx.Value(ctxKeyLogFields{}).(*logFields)
	return lf
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Flush passes through so SSE streaming works behind the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes through so protocol upgrades (the WebSocket
// endpoint, CONNECT-style tunnels) work behind the recorder. The
// recorder embeds http.ResponseWriter, which does NOT carry the
// Hijacker method, so without this the type assertion inside
// gorilla/websocket's Upgrader fails and every /v1/ws handshake
// answers a bare 500 ("response does not implement http.Hijacker").
// On a successful hijack the response becomes a 101 Switching
// Protocols, so the recorded status reflects what the client saw.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("statusRecorder: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		r.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lf := &logFields{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyLogFields{}, lf))
		// Caller-key carrier (P1.7): a lazy holder so a handler (or a
		// follow-up on a derived context) can recover the authenticated
		// key ID without the request and without paying a verifier
		// lookup unless it asks.
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyCallerKey{}, &callerKeyHolder{s: s, r: r}))
		// Correlation id (M2.2): the sanitized inbound
		// X-Request-Id when the client sent one, else a fresh
		// id. It rides the request context (so every handler
		// and the response-header writer can read it) and is
		// echoed on the response before dispatch — headers set
		// here reach the client whatever status the handler
		// writes, including auth and validation errors. The
		// version rides along for the same reason: a client can
		// always tell which gateway build answered it.
		reqID := requestIDFor(r)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyRequestID{}, reqID))
		// Root span per request (otel build only; no-op otherwise):
		// the span ends when this wrapper returns, i.e. when the
		// handler has written the full response (SSE included).
		spanCtx, span := s.tracer.StartRequestSpan(r.Context(), reqID)
		span.SetAttribute("http.route", r.URL.Path)
		defer span.End()
		r = r.WithContext(spanCtx)
		w.Header().Set(headerCodvianRequestID, reqID)
		w.Header().Set("X-Request-Id", reqID)
		if s.version != "" {
			w.Header().Set(headerCodvianVersion, s.version)
		}
		// Per-request key-budget memo (CHECKLIST 279-280): the
		// key-budget guard runs twice on one request (pre-body
		// admission, then the post-decode check) and the 273-274
		// profile put those two GetKeyBudget reads at 11.54% cum.
		// One memo per request — attached here, the outermost
		// wrapper every request passes through — turns the second
		// read into a map lookup. It lives only as long as this
		// request; a budget set or deleted mid-request is visible
		// to the next request, never to this one.
		r = r.WithContext(ledger.WithKeyBudgetMemo(r.Context()))
		// Per-request global-budget memo (CHECKLIST 299-300):
		// the same shape for the GetBudget read behind
		// CheckBudgetOrError / CheckBudgetForEstimate. Honest
		// scope (see internal/ledger/budget_memo.go): on today's
		// hot paths the global-budget guard reads once per
		// request, so this memo's saving materialises only when
		// a request hits the guard twice — the memo is the dedup
		// mechanism, not a claimed win on its own.
		r = r.WithContext(ledger.WithBudgetMemo(r.Context()))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		// Never log keys or message content — route, model, status,
		// latency only. The model name is redacted first as a safety
		// net, so a model string carrying key-shaped material can
		// never reach the log verbatim (research-log entry 150). The
		// correlation id rides the row (M2.2), so the access log
		// joins to the response headers and the stream trailer.
		s.logger.Printf("method=%s path=%s model=%s status=%d latency_ms=%d request_id=%s",
			r.Method, r.URL.Path, guardrails.Redact(lf.model), rec.status, time.Since(start).Milliseconds(), reqID)
	})
}
