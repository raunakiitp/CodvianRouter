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
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/health"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/web"
)

// DefaultTimeout is the per-request timeout applied to /v1/* handlers.
const DefaultTimeout = 120 * time.Second

// maxRequestBody caps a single chat-completions request body (10 MiB).
const maxRequestBody = 10 << 20

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
// mid-stream resume. skipChars is the number of runes of content the
// client has already received; the engine must not re-emit them. The
// server additionally trims any replayed overlap (see trimAlreadySent)
// as a safety net, so a resume that ignores skipChars still cannot
// duplicate content on the wire.
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
// mid-stream resume, chunk-aware (see server.StreamResumer for the
// text-only form).
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
func (v configVerifier) VerifyKey(_ context.Context, key string) bool {
	if v.cfg == nil || key == "" {
		return false
	}
	_, ok := v.cfg.VerifyGatewayKey(key)
	return ok
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

// ---------------------------------------------------------------------------
// Model registry.
// ---------------------------------------------------------------------------

// Model is one entry in a GET /v1/models list response.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
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
			out = append(out, Model{ID: m.ID, Object: "model", OwnedBy: p.Name})
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
	engine             Engine
	verifier           KeyVerifier
	cfg                *config.Config // nil = no config loaded (settings export answers 503)
	registry           ModelRegistry
	dashboardProviders DashboardProviderLister // nil = no provider feed
	healthStatuses     HealthStatusLister      // nil = health unknown for all providers
	autoRouter         *autorouter.Router      // nil = auto-routing off; "auto/*" then fails honestly
	models             []string
	timeout            time.Duration
	logger             *log.Logger
	recorder           *metrics.Recorder // nil = metrics disabled
	handler            http.Handler
	// Route-guard state (see guard.go). authRelaxed relaxes Tier 3
	// only — Tier 2 (ALWAYS_PROTECTED) never honours it, and Tier 1
	// (LOCAL_ONLY) always checks loopback first.
	authRelaxed      bool
	listenAddr       string
	bootstrapPending atomic.Bool
	bootstrapIssuer  BootstrapIssuer
	shutdownHook     func()
	startedAt        time.Time
	localOnlyDenials atomic.Uint64
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
	}
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
// the server decoupled from the checker's probe machinery.
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

// New builds a Server. engine must not be nil.
//
// Every route is registered through register(), which looks the route
// up in the guard-tier table (guard.go) and applies that tier's
// wrappers — LOCAL_ONLY first, then auth — so no route can land
// unclassified.
func New(engine Engine, opts ...Option) *Server {
	s := &Server{engine: engine, timeout: DefaultTimeout, logger: log.Default(), startedAt: time.Now()}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	s.register(mux, "GET", "/healthz", s.handleHealthz)
	s.register(mux, "GET", "/dashboard", s.handleDashboard)
	s.register(mux, "GET", "/dashboard/providers", s.handleDashboardProviders)
	s.register(mux, "GET", "/dashboard/auto-route", s.handleAutoRoute)
	s.register(mux, "GET", "/dashboard/auto-route/last", s.handleAutoRouteLast)
	s.register(mux, "GET", "/dashboard/coalescing", s.handleCoalescing)
	s.register(mux, "GET", "/dashboard/concurrency", s.handleConcurrency)
	s.register(mux, "GET", "/dashboard/guard", s.handleGuard)
	s.register(mux, "GET", "/api/providers", s.handleAPIProviders)
	s.register(mux, "GET", "/metrics", s.handleMetrics)
	s.register(mux, "GET", "/v1/models", s.handleModels)
	s.register(mux, "POST", "/v1/chat/completions", s.handleChatCompletions)
	s.register(mux, "POST", "/admin/shutdown", s.handleShutdown)
	s.register(mux, "POST", "/admin/service/install", s.notImplementedAdmin("service install"))
	s.register(mux, "POST", "/admin/plugin/load", s.notImplementedAdmin("plugin load"))
	s.register(mux, "POST", "/admin/update", s.notImplementedAdmin("auto-update"))
	s.register(mux, "POST", "/admin/tunnel/start", s.notImplementedAdmin("tunnel start"))
	s.register(mux, "POST", "/admin/provider/login", s.notImplementedAdmin("provider login"))
	s.register(mux, "POST", "/admin/bootstrap", s.handleBootstrap)
	s.register(mux, "POST", "/admin/settings/export", s.handleSettingsExport)
	s.register(mux, "GET", "/admin/status", s.handleAdminStatus)
	s.handler = s.withLogging(mux)
	return s
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
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "invalid gateway key")
			return
		}
		next(w, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	if s.verifier == nil {
		return false
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return false
	}
	return s.verifier.VerifyKey(r.Context(), parts[1])
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
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an OpenAI-shaped client error.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: "invalid_request_error"}})
}

// writeServerError writes an OpenAI-shaped server/upstream error.
func writeServerError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Message: msg, Type: "server_error"}})
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
// run yet). It embeds provider.DashboardProvider, so the JSON carries
// the same safe fields (id, name, wire format, verification, auth
// kind, models) and — like /dashboard/providers — never a key or a
// base URL.
type apiProvider struct {
	provider.DashboardProvider
	HealthStatus string `json:"health_status"`
}

// apiProvidersResponse is the GET /api/providers payload.
type apiProvidersResponse struct {
	Providers []apiProvider            `json:"providers"`
	Summary   dashboardProviderSummary `json:"summary"`
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
	if s.healthStatuses != nil {
		for _, st := range s.healthStatuses.Last() {
			healthByID[st.ProviderID] = st.OK
		}
	}
	providers := make([]apiProvider, 0, len(base))
	for _, p := range base {
		status := "unknown"
		if ok, checked := healthByID[p.ID]; checked {
			if ok {
				status = "ok"
			} else {
				status = "down"
			}
		}
		providers = append(providers, apiProvider{DashboardProvider: p, HealthStatus: status})
	}
	writeJSON(w, http.StatusOK, apiProvidersResponse{Providers: providers, Summary: summarizeProviders(base)})
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
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(dec)
		return
	}
	writeJSON(w, http.StatusOK, dec)
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
	writeJSON(w, http.StatusOK, map[string]any{"has_decision": ok, "decision": dec})
}

// lastAutoDecision reads the most recent auto-routing decision from
// the engine when it reports one.
func (s *Server) lastAutoDecision() (autorouter.Decision, bool) {
	if ad, ok := s.engine.(autoDecisionReporter); ok {
		return ad.LastAutoDecision()
	}
	return autorouter.Decision{}, false
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

// setAutoRouteHeader marks a non-streaming auto-routed response with
// the real routing decision: the tier picked, its confidence, the
// resolved model, and whether it fell back. It must be called before
// the response body is written. Streaming responses cannot carry the
// header (the decision is final only after the stream ends), so
// handleChatStream logs it instead.
func (s *Server) setAutoRouteHeader(w http.ResponseWriter) {
	dec, ok := s.lastAutoDecision()
	if !ok {
		return
	}
	w.Header().Set("X-Codvian-Auto-Tier", string(dec.Tier))
	w.Header().Set("X-Codvian-Auto-Confidence", fmt.Sprintf("%.3f", dec.Confidence))
	w.Header().Set("X-Codvian-Auto-Model", dec.Model)
	if dec.Fallback {
		w.Header().Set("X-Codvian-Auto-Fallback", "true")
	}
}

// handleMetrics serves the Prometheus exposition of the recorder. It
// is auth-gated like the rest of /v1/*: a scrape config must present a
// gateway key, because the router may be bound to a non-loopback
// address with --listen. Without a recorder it answers 503.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.recorder == nil {
		writeError(w, http.StatusServiceUnavailable, "metrics not enabled")
		return
	}
	s.recorder.Handler().ServeHTTP(w, r)
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
// configured. Provider/model come from the route trace when available
// (the last attempt is the winner on success, the last tried on
// failure), falling back to the request model and "unknown" provider.
// Failovers — a non-success attempt followed by another attempt — are
// counted from the trace. The trace is the engine's most recent one,
// so under concurrency a request may occasionally be attributed to a
// neighbour's trace; these counters are approximate telemetry, not
// billing data (the ledger is the billing record).
func (s *Server) recordMetrics(req provider.ChatRequest, resp *provider.ChatResponse, err error, started time.Time) {
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
	if tp, ok := s.engine.(traceProvider); ok {
		if trace := tp.LastTrace(); trace != nil && len(trace.Attempts) > 0 {
			last := trace.Attempts[len(trace.Attempts)-1]
			providerName, model = last.Provider, last.Model
			for i := 0; i+1 < len(trace.Attempts); i++ {
				if trace.Attempts[i].Outcome != "success" {
					rec.IncFailover(trace.Attempts[i].Provider, trace.Attempts[i+1].Provider)
				}
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
			s.logger.Printf("models registry error: %v", err)
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
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	var req provider.ChatRequest
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
	if msg := validateChatRequest(&req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if lf := logFieldsFrom(ctx); lf != nil {
		lf.model = req.Model
	}

	start := time.Now()
	if s.recorder != nil {
		s.recorder.IncInFlight()
		defer s.recorder.DecInFlight()
	}

	if req.Stream {
		err := s.handleChatStream(w, ctx, req)
		s.recordMetrics(req, nil, err, start)
		return
	}

	resp, err := s.engine.Chat(ctx, req)
	s.recordMetrics(req, resp, err, start)
	if err != nil {
		s.handleEngineError(w, err)
		return
	}
	if resp == nil {
		// A nil response with a nil error is an upstream failure.
		writeServerError(w, http.StatusBadGateway, "upstream provider error")
		return
	}
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
	s.setAutoRouteHeader(w)
	writeJSON(w, http.StatusOK, chatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []chatChoiceJSON{{
			Index:        0,
			Message:      provider.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls},
			FinishReason: finishReason,
		}},
		Usage: resp.Usage,
	})
}

func validateChatRequest(req *provider.ChatRequest) string {
	if req.Model == "" {
		return "model is required"
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

// handleEngineError maps an engine failure to a status code. Upstream
// detail is not exposed to the client.
func (s *Server) handleEngineError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeServerError(w, http.StatusGatewayTimeout, "request timed out")
		return
	}
	writeServerError(w, http.StatusBadGateway, "upstream provider error")
}

type chatCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []chatChoiceJSON `json:"choices"`
	Usage   provider.Usage   `json:"usage"`
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
}

// handleChatStream streams SSE deltas to the client. It returns nil
// on a completed stream and the engine error otherwise, so the caller
// can record metrics. A stream that served a stale cached response
// cannot carry the X-Codvian-Stale header (staleness is known only
// after the stream ends), so it is logged instead.
func (s *Server) handleChatStream(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest) error {
	id := newChatID()
	created := time.Now().Unix()

	var sent strings.Builder // full content already sent to the client
	headersSent := false
	flusher, _ := w.(http.Flusher)

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
	}
	writeSSE := func(line string) error {
		ensureHeaders()
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
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
	onChunk := func(chunk provider.StreamChunk) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		d := streamDelta{Content: chunk.Content, ToolCalls: chunk.ToolCalls}
		if first {
			d.Role = "assistant"
			first = false
		}
		var fr *string
		if chunk.FinishReason != "" {
			fr = &chunk.FinishReason
			finishSent = true
		}
		if d.Content == "" && len(d.ToolCalls) == 0 && fr == nil {
			return nil
		}
		if err := writeChunk(d, fr); err != nil {
			return err
		}
		sent.WriteString(chunk.Content)
		return nil
	}
	onDelta := func(delta string) error {
		if delta == "" {
			return nil
		}
		return onChunk(provider.StreamChunk{Content: delta})
	}
	finish := func(reason string) error {
		// A chunk already carried the finish reason: just close.
		if finishSent {
			return writeSSE("data: [DONE]\n\n")
		}
		if reason == "" {
			reason = "stop"
		}
		fr := reason
		if err := writeChunk(streamDelta{}, &fr); err != nil {
			return err
		}
		return writeSSE("data: [DONE]\n\n")
	}

	var resp *provider.ChatResponse
	var err error
	if ce, ok := s.engine.(chunkEngine); ok {
		resp, err = ce.ChatStreamChunks(ctx, req, onChunk)
	} else {
		resp, err = s.engine.ChatStream(ctx, req, onDelta)
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
		if dec, ok := s.lastAutoDecision(); ok {
			s.logger.Printf("stream auto-routed to tier %s (confidence %.3f, model %s, fallback %v)", dec.Tier, dec.Confidence, dec.Model, dec.Fallback)
		}
		reason := "stop"
		if resp != nil && resp.FinishReason != "" {
			reason = resp.FinishReason
		}
		_ = finish(reason)
		return nil
	}

	// The engine failed. If nothing was sent yet, headers have not gone
	// out (they are written lazily on the first chunk), so a plain JSON
	// error with a 502/504 status is still possible.
	if sent.Len() == 0 && !headersSent {
		s.handleEngineError(w, err)
		return err
	}

	// Partial content already reached the client: it must not be re-sent.
	// Resume once from the exact offset, if the engine supports it.
	// The chunk-aware resumer is preferred (tool-call deltas survive);
	// otherwise the text-only resumer replays content only.
	skipChars := len([]rune(sent.String()))
	if resumer, ok := s.engine.(chunkResumer); ok {
		resumeChunk := func(chunk provider.StreamChunk) error {
			if chunk.Content != "" {
				// Safety net: strip any prefix the client already
				// has, in case the engine ignored skipChars and
				// replayed from start.
				chunk.Content = trimAlreadySent(sent.String(), chunk.Content)
				if chunk.Content == "" && len(chunk.ToolCalls) == 0 && chunk.FinishReason == "" {
					return nil
				}
			}
			return onChunk(chunk)
		}
		resp2, err2 := resumer.ChatStreamFromChunks(ctx, req, skipChars, resumeChunk)
		if err2 == nil {
			reason := "stop"
			if resp2 != nil && resp2.FinishReason != "" {
				reason = resp2.FinishReason
			}
			_ = finish(reason)
			return nil
		}
		err = err2
	} else if resumer, ok := s.engine.(StreamResumer); ok {
		resumeDelta := func(delta string) error {
			if delta == "" {
				return nil
			}
			// Safety net: strip any prefix the client already has, in
			// case the engine ignored skipChars and replayed from start.
			if trimmed := trimAlreadySent(sent.String(), delta); trimmed != delta {
				delta = trimmed
				if delta == "" {
					return nil
				}
			}
			return onDelta(delta)
		}
		resp2, err2 := resumer.ChatStreamFrom(ctx, req, skipChars, resumeDelta)
		if err2 == nil {
			reason := "stop"
			if resp2 != nil && resp2.FinishReason != "" {
				reason = resp2.FinishReason
			}
			_ = finish(reason)
			return nil
		}
		err = err2
	}

	// Resume unavailable or failed: end the stream with an SSE error
	// event rather than duplicating content or hanging.
	s.logger.Printf("stream failed after %d bytes sent: %v", sent.Len(), err)
	payload, _ := json.Marshal(errorBody{Error: errorDetail{Message: "upstream provider error", Type: "server_error"}})
	_ = writeSSE("data: " + string(payload) + "\n\n")
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

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lf := &logFields{}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyLogFields{}, lf))
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(rec, r)
		// Never log keys or message content — route, model, status,
		// latency only.
		s.logger.Printf("method=%s path=%s model=%s status=%d latency_ms=%d",
			r.Method, r.URL.Path, lf.model, rec.status, time.Since(start).Milliseconds())
	})
}
