// Package server implements the OpenAI-compatible HTTP gateway for Codvian Router.
//
// Stdlib only (net/http). It also serves the static dashboard UI at
// GET /dashboard (the approved dashboard embedded in the web package);
// the dashboard carries no live data and needs no gateway key, while
// every /v1/* route and /metrics stay auth-gated.
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
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
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
	engine   Engine
	verifier KeyVerifier
	registry ModelRegistry
	models   []string
	timeout  time.Duration
	logger   *log.Logger
	recorder *metrics.Recorder // nil = metrics disabled
	handler  http.Handler
}

// Option configures a Server.
type Option func(*Server)

// WithKeyVerifier sets the gateway-key verifier.
func WithKeyVerifier(v KeyVerifier) Option {
	return func(s *Server) { s.verifier = v }
}

// WithConfig sets the verifier from a loaded *config.Config (production
// path — see NewConfigVerifier).
func WithConfig(cfg *config.Config) Option {
	return func(s *Server) { s.verifier = NewConfigVerifier(cfg) }
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
// (adapted via AdaptProviderRegistry).
func WithProviderRegistry(r *provider.Registry) Option {
	return func(s *Server) { s.registry = AdaptProviderRegistry(r) }
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

// New builds a Server. engine must not be nil.
func New(engine Engine, opts ...Option) *Server {
	s := &Server{engine: engine, timeout: DefaultTimeout, logger: log.Default()}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/dashboard", s.handleDashboard)
	mux.HandleFunc("/metrics", s.requireAuth(s.handleMetrics))
	mux.HandleFunc("/v1/models", s.requireAuth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", s.requireAuth(s.handleChatCompletions))
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
// is static sample UI with no live data, and the server binds
// localhost by default (see --listen). It must never shadow the API:
// /dashboard is an exact path, so /v1/* and /metrics are unaffected.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(web.IndexHTML)
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
