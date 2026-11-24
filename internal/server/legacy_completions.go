// M8.2 — legacy completions + routing introspection (Area 14 §14a).
//
// POST /v1/completions is the legacy OpenAI completions surface:
// a prompt-style request executed through the SAME chat engine as
// POST /v1/chat/completions (the prompt is adapted to one user
// message; no second engine is invented), with the chat response
// translated back into the text_completion shape. The pre-call
// gate sequence mirrors POST /v1/embeddings (same-package helpers,
// same order): admission, intent-tier check, local readiness,
// guardrails, plugin pre-request hooks, compression, access
// schedule, request/prompt/key budgets, token limits, quota pools
// + schedules, internal budgets, team budget, key traffic,
// throttle delay, key session. Streaming is NOT supported on this
// surface (400 naming /v1/chat/completions instead): the chat
// engine's stream path is chat-shaped, and re-shaping a live
// stream into legacy SSE would invent framing the engine never
// produced.
//
// GET /v1/classify, GET /v1/explain/routing and GET /v1/segment
// are read-only introspection: they read the router's existing
// decision machinery (autorouter.Classify / ResolveWithThreshold,
// the configured routing strategy, the last served decision +
// trace) and report what the router WOULD do and why. They NEVER
// call an upstream: every response carries would_call_upstream
// false / upstream_calls 0, and no code path in these three
// handlers reaches the engine's Chat/ChatStream. A request that
// names a concrete (non-virtual) model is a 400 — classification
// only makes sense for a virtual model — and a decision with no
// configured concrete model is a 502 with the decision attached
// (same honest shape as GET /dashboard/auto-route).
//
// No prices, models, caps or capabilities are invented here:
// an unpriced model stays unpriced (enrichDecision leaves the
// cost fields absent), an unconfigured strategy is reported as
// null with the source "none configured", and /v1/segment caps
// at 100 segments, mirroring the GET /v1/me/sessions list cap —
// never a silently truncated longer list (truncated=true says so).
//
// Registration (guard table + mux) is the coordinator's: these
// handlers are methods on *Server in this file only. Proposed
// guard rows (all TierManagement, gateway-key auth):
//
//	POST /v1/completions      scope "chat"
//	GET  /v1/classify         scope "chat"
//	GET  /v1/explain/routing  scope "chat"
//	GET  /v1/segment          scope "chat"
//
// Auth itself is enforced by the register wrapper, not by these
// handlers — the same split as every other handler in this package.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// ---------------------------------------------------------------------------
// POST /v1/completions (legacy)
// ---------------------------------------------------------------------------

// legacyCompletionsRequest is the prompt-style request body. Prompt
// is decoded as any so the handler can accept exactly the shapes
// it supports (string, or a one-element []string) and reject the
// rest with a 400 naming the problem — token-id prompts and
// multi-prompt batches are refused, never silently reinterpreted.
type legacyCompletionsRequest struct {
	Model            string   `json:"model"`
	Prompt           any      `json:"prompt"`
	Suffix           string   `json:"suffix,omitempty"`
	MaxTokens        *int     `json:"max_tokens,omitempty"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	N                *int     `json:"n,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	Stop             any      `json:"stop,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Echo             bool     `json:"echo,omitempty"`
	BestOf           *int     `json:"best_of,omitempty"`
	Logprobs         *int     `json:"logprobs,omitempty"`
	// User is accepted and ignored: the gateway does not use
	// end-user ids for routing or billing, and the chat surface
	// ignores unknown fields the same way.
	User string `json:"user,omitempty"`
}

// legacyCompletionChoice is one choice of a text_completion
// response. Logprobs is always null: the chat engine reports no
// per-token logprobs, and a null is the honest "unknown", never an
// invented distribution. FinishReason is null when the engine
// reported none — again unknown, never a guessed "stop".
type legacyCompletionChoice struct {
	Text         string  `json:"text"`
	Index        int     `json:"index"`
	Logprobs     any     `json:"logprobs"`
	FinishReason *string `json:"finish_reason"`
}

// legacyCompletionsResponse is the text_completion shape. No cost
// fields: cost is unknown here unless the caller reads the
// per-response headers (which carry the real priced cost when the
// price book knows the pair, and stay absent otherwise).
type legacyCompletionsResponse struct {
	ID      string                   `json:"id"`
	Object  string                   `json:"object"`
	Created int64                    `json:"created"`
	Model   string                   `json:"model"`
	Choices []legacyCompletionChoice `json:"choices"`
	Usage   provider.Usage           `json:"usage"`
}

// decodeLegacyPrompt extracts the single prompt string from the
// request's prompt field: a string, or a one-element []string.
// Anything else is an error naming the problem.
func decodeLegacyPrompt(raw any) (string, error) {
	switch p := raw.(type) {
	case nil:
		return "", fmt.Errorf("prompt is required")
	case string:
		if strings.TrimSpace(p) == "" {
			return "", fmt.Errorf("prompt must not be empty")
		}
		return p, nil
	case []any:
		if len(p) == 0 {
			return "", fmt.Errorf("prompt must not be empty")
		}
		if _, ok := p[0].(string); !ok {
			return "", fmt.Errorf("token-id prompts are not supported: prompt must be a string")
		}
		if len(p) > 1 {
			return "", fmt.Errorf("multiple prompts in one request are not supported: send one prompt per request")
		}
		s := p[0].(string)
		if strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("prompt must not be empty")
		}
		return s, nil
	default:
		return "", fmt.Errorf("prompt must be a string")
	}
}

// decodeLegacyStop extracts the stop sequences: absent, a string,
// or a []string. Anything else is an error.
func decodeLegacyStop(raw any) ([]string, error) {
	switch s := raw.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{s}, nil
	case []any:
		out := make([]string, 0, len(s))
		for _, v := range s {
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("stop must be a string or an array of strings")
			}
			out = append(out, str)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("stop must be a string or an array of strings")
	}
}

// toChatRequest validates a legacy request and adapts it to the
// chat engine's request: one user message carrying the prompt.
// Fields the chat engine cannot honour are refused with a 400
// naming the field — never silently dropped, so a client can never
// believe a penalty, echo, suffix or logprobs request was applied
// when it was not.
func toChatRequest(lr *legacyCompletionsRequest) (provider.ChatRequest, string, error) {
	if strings.TrimSpace(lr.Model) == "" {
		return provider.ChatRequest{}, "", fmt.Errorf("model is required")
	}
	prompt, err := decodeLegacyPrompt(lr.Prompt)
	if err != nil {
		return provider.ChatRequest{}, "", err
	}
	if lr.Stream {
		return provider.ChatRequest{}, "", fmt.Errorf("streaming is not supported on POST /v1/completions: use POST /v1/chat/completions with stream=true")
	}
	if lr.N != nil && *lr.N != 1 {
		return provider.ChatRequest{}, "", fmt.Errorf("n=%d is not supported on POST /v1/completions: the chat engine returns a single completion (n=1)", *lr.N)
	}
	if lr.BestOf != nil && *lr.BestOf > 1 {
		return provider.ChatRequest{}, "", fmt.Errorf("best_of=%d is not supported on POST /v1/completions: the chat engine returns a single completion", *lr.BestOf)
	}
	if lr.Echo {
		return provider.ChatRequest{}, "", fmt.Errorf("echo is not supported on POST /v1/completions")
	}
	if lr.Suffix != "" {
		return provider.ChatRequest{}, "", fmt.Errorf("suffix (insert mode) is not supported on POST /v1/completions")
	}
	if lr.Logprobs != nil && *lr.Logprobs > 0 {
		return provider.ChatRequest{}, "", fmt.Errorf("logprobs is not supported on POST /v1/completions: the chat engine reports no per-token logprobs")
	}
	if lr.PresencePenalty != nil && *lr.PresencePenalty != 0 {
		return provider.ChatRequest{}, "", fmt.Errorf("presence_penalty is not supported on POST /v1/completions: the chat engine has no penalty field")
	}
	if lr.FrequencyPenalty != nil && *lr.FrequencyPenalty != 0 {
		return provider.ChatRequest{}, "", fmt.Errorf("frequency_penalty is not supported on POST /v1/completions: the chat engine has no penalty field")
	}
	if lr.MaxTokens != nil && *lr.MaxTokens < 0 {
		return provider.ChatRequest{}, "", fmt.Errorf("max_tokens must not be negative")
	}
	stop, err := decodeLegacyStop(lr.Stop)
	if err != nil {
		return provider.ChatRequest{}, "", err
	}
	req := provider.ChatRequest{
		Model:    strings.TrimSpace(lr.Model),
		Messages: []provider.Message{{Role: router.RoleUser, Content: prompt}},
		Stop:     stop,
		// The legacy call is executed by the chat engine, so it
		// carries the chat endpoint kind: same failover, same
		// replay, same trace/ledger surface as a chat request.
		// There is no EndpointLegacyCompletions kind; the
		// completions shape is a translation of a chat call,
		// not a separate engine path.
		Endpoint: provider.EndpointChatCompletions,
	}
	if lr.MaxTokens != nil {
		req.MaxTokens = *lr.MaxTokens
	}
	if lr.Temperature != nil {
		req.Temperature = *lr.Temperature
	}
	if lr.TopP != nil {
		req.TopP = *lr.TopP
	}
	return req, prompt, nil
}

// newLegacyCompletionID mints a text_completion id. Random hex,
// never derived from the prompt or the model.
func newLegacyCompletionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("cmpl-%d", time.Now().UnixNano())
	}
	return "cmpl-" + hex.EncodeToString(b[:])
}

// toLegacyResponse translates a chat response into the
// text_completion shape. The model names the upstream echo when
// present, else the requested model; the finish reason is the
// engine's verbatim value, null when the engine reported none.
func toLegacyResponse(req provider.ChatRequest, resp *provider.ChatResponse) legacyCompletionsResponse {
	model := resp.Model
	if model == "" {
		model = req.Model
	}
	var finish *string
	if resp.FinishReason != "" {
		f := resp.FinishReason
		finish = &f
	}
	return legacyCompletionsResponse{
		ID:      newLegacyCompletionID(),
		Object:  "text_completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []legacyCompletionChoice{{Text: resp.Content, Index: 0, Logprobs: nil, FinishReason: finish}},
		Usage:   resp.Usage,
	}
}

// handleLegacyCompletions serves POST /v1/completions.
func (s *Server) handleLegacyCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	handlerStart := time.Now()
	var req provider.ChatRequest
	engineCalled := false
	// Panic isolation, mirroring handleEmbedRerank: a panic
	// anywhere on this path must never kill the gateway. The
	// client gets an honest 502, the panic is counted + logged
	// redacted, and a ledger row is recorded when the engine
	// recorded none.
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_completions", v)
			s.writePanicFailure(w)
			if req.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), req, handlerStart)
				rowCancel()
			}
		}
	}()

	// Admission before I/O: a blocked or over-limit key is
	// refused before the body is read (same as chat/embeddings).
	if s.preBodyAdmission(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large")
		return
	}
	// Security cluster: a client-supplied Vertex override in the
	// raw body is rejected unless the admin opted in — the same
	// scan embeddings runs on its body.
	if ov, ok := provider.DetectVertexOverride(body); ok {
		if refused := s.checkClientVertexOverride(w, &ov); refused {
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	var legacy legacyCompletionsRequest
	if err := json.Unmarshal(body, &legacy); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req, _, err = toChatRequest(&legacy)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if msg := validateChatRequest(&req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// Intent tiers: an unmapped intent tier is refused before
	// any provider is dialled, never silently defaulted.
	if refused := s.checkIntentTier(w, r, req.Model); refused {
		return
	}
	// Deployment-pinned parameters outrank the client's fields.
	s.applyDeploymentParams(w, &req)
	// Readiness gate: a not-ready local capability answers 503
	// with Retry-After here, before any provider is dialled.
	if !s.checkLocalReadiness(w, r, req.Model) {
		return
	}
	req.CallerID = s.callerKeyID(r)
	req.CacheControls = parseCacheControls(r)
	// Guardrails run on the prompt BEFORE any provider is
	// dialled; a blocked request makes zero provider calls.
	if blocked := s.applyGuardrails(ctx, w, &req, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks, after the built-in guardrails
	// and before every budget gate and the engine.
	if blocked, newCtx := s.applyPlugins(ctx, w, &req); blocked {
		return
	} else {
		ctx = newCtx
	}
	// Lossless-first compression, as on chat: guardrails saw
	// the original prompt, budgets price the prompt as sent.
	// The registry pre-pass (session-dedup, CCR, Codex) runs
	// first, at the same point.
	s.applyCompressionPrePass(r, &req)
	s.applyCompression(&req)
	// Per-key access schedule, before any budget is touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-request budget, prompt budget, and the rolling key
	// budget (which commits the prompt's estimated cost; the
	// defer settles it once this handler returns).
	if refused := s.enforceRequestBudget(w, r, &req); refused {
		return
	}
	if refused := s.enforcePromptBudget(w, r, &req); refused {
		return
	}
	if resID, refused := s.reserveKeyBudget(w, r, &req); refused {
		return
	} else {
		defer s.settleReservation(resID)
	}
	if refused := s.checkKeyTokenLimits(w, r, "", req.Model); refused {
		return
	}
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	if refused := s.checkInternalBudgets(w, r, req.Model); refused {
		return
	}
	// P1.6 org/team policy: the authenticated key's team allowlists
	// (model, strategy) decide before dispatch, on this dialect too —
	// no alternate API is a policy bypass.
	if !s.enforceTeamPolicy(w, r, req.Model, routingStrategy(&req)) {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	if refused := s.checkKeyTrafficForChat(w, r, &req); refused {
		return
	}
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)

	start := time.Now()
	// Request-scoped attribution, as on chat: the engine
	// publishes this request's decision + trace into the sink,
	// so recordMetrics attributes the outcome to this request
	// even when a neighbour finishes first.
	ctx = router.WithAttribution(ctx, router.NewAttribution())
	if s.recorder != nil {
		s.recorder.IncInFlight()
		defer s.recorder.DecInFlight()
	}

	// The chat engine serves the request — via the MCP agent
	// loop when MCP servers are configured, else a plain engine
	// Chat, exactly as handleChatCompletions does.
	engineCalled = true
	resp, loop, err := s.chatWithMCP(ctx, req)
	if err != nil {
		s.logPanicError(err)
		if isPanicError(err) {
			s.recordPanicRowIfUnrecorded(r.Context(), s.callerKeyID(r), req, start)
		}
		s.recordMetrics(ctx, req, resp, err, start)
		s.noteRequestBudgetMidChain(&req, err)
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleEngineError(w, err)
		return
	}
	if resp == nil {
		// A nil response with a nil error is an upstream
		// failure — same branch as chat.
		s.recordMetrics(ctx, req, resp, errNilUpstreamResponse, start)
		meta := s.responseMeta(ctx, req, resp, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		s.writeUpstreamFailure(w)
		return
	}
	// Plugin post-response hooks (fail-open) and the response
	// guardrail: the provider's own reply is scanned before
	// delivery, as on chat. A blocked response is refused with
	// 403 / -32001 and its content never leaves the router.
	s.applyPostResponsePlugins(ctx, &req, resp)
	if blocked := s.applyResponseGuardrails(w, resp, s.keyGuardrailScope(r)); blocked {
		s.recordMetrics(ctx, req, resp, errGuardrailResponseBlocked, start)
		s.recordTelemetry(ctx, s.responseMeta(ctx, req, resp, start), http.StatusForbidden)
		return
	}
	s.recordMetrics(ctx, req, resp, nil, start)
	// The response names the surface it came from, so a client
	// (or a log reader) never has to guess whether a 200 was
	// chat, embeddings or legacy completions.
	w.Header().Set("X-Codvian-Endpoint", "completions")
	s.setDecisionHeader(w, resp.Model, req.Model)
	s.setMCPHeader(w, loop)
	meta := s.responseMeta(ctx, req, resp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	writeJSON(w, http.StatusOK, toLegacyResponse(req, resp))
}

// ---------------------------------------------------------------------------
// Routing introspection: GET /v1/classify, /v1/explain/routing, /v1/segment
// ---------------------------------------------------------------------------

// introspectParams are the shared query parameters of the three
// introspection endpoints. model defaults to "auto"; threshold is
// a one-off preview override for this decision only, in (0, 1].
type introspectParams struct {
	Model     string
	Prompt    string
	Threshold float32
}

// parseIntrospectParams parses the shared query parameters. An
// empty prompt is a 400 unless the model is an intent model —
// there the tier IS the routing instruction and no prompt is
// needed to classify (same rule as GET /dashboard/auto-route).
func parseIntrospectParams(r *http.Request) (introspectParams, error) {
	q := r.URL.Query()
	p := introspectParams{Model: q.Get("model")}
	if p.Model == "" {
		p.Model = "auto"
	}
	p.Prompt = strings.TrimSpace(q.Get("prompt"))
	if raw := q.Get("threshold"); raw != "" {
		v, err := strconv.ParseFloat(raw, 32)
		if err != nil || v <= 0 || v > 1 {
			return p, fmt.Errorf("threshold must be a number in (0, 1]")
		}
		p.Threshold = float32(v)
	}
	if p.Prompt == "" && !autorouter.IsIntentModel(p.Model) {
		return p, fmt.Errorf("prompt is required")
	}
	return p, nil
}

// classifyPrompt runs the auto-router's ResolveWithThreshold for
// an introspection request and reports whether the decision
// resolved to a concrete model. ok=false with an unknown virtual
// model is a caller error (400); ok=false with a known virtual
// model is an honest 502 with the decision attached — the router
// classified the prompt but has no concrete model configured for
// the picked tier, and the decision itself says why.
func (s *Server) classifyPrompt(p introspectParams) (string, autorouter.Decision, bool, int, error) {
	_, dec, ok := s.autoRouter.ResolveWithThreshold(p.Model, p.Prompt, p.Threshold)
	if !ok && dec.Model == "" && !autorouter.IsVirtualModel(p.Model) {
		return "", dec, false, http.StatusBadRequest, fmt.Errorf("unknown virtual model %q", p.Model)
	}
	if !ok {
		return "", dec, false, http.StatusBadGateway, nil
	}
	return dec.Model, dec, true, http.StatusOK, nil
}

// requireAutoRouter answers 503 when no auto-router is wired —
// an honest "this deployment cannot classify", never a faked
// decision.
func (s *Server) requireAutoRouter(w http.ResponseWriter) bool {
	if s.autoRouter == nil {
		writeError(w, http.StatusServiceUnavailable, "auto-router is not configured")
		return false
	}
	return true
}

// handleClassify serves GET /v1/classify?model=auto&prompt=...
// [&threshold=0.5]: the live auto-router decision for the prompt
// (tier, confidence, per-tier scores, threshold, fallback +
// reason, resolved concrete model, measured decision latency,
// plus the resolved model's real price/quality when known). No
// upstream is called.
func (s *Server) handleClassify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireAutoRouter(w) {
		return
	}
	p, err := parseIntrospectParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, dec, _, status, err := s.classifyPrompt(p)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	if status != http.StatusOK {
		// AUD-CODE-002: encode BEFORE committing the status, so an
		// unmarshalable payload cannot answer a committed non-200
		// with an empty or truncated body.
		w.Header().Set("Content-Type", "application/json")
		buf, err := json.Marshal(s.enrichDecision(r, dec))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "response could not be encoded")
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(buf)
		return
	}
	writeJSON(w, http.StatusOK, s.enrichDecision(r, dec))
}

// strategyForModel returns the routing strategy that would order
// the chain for the given model, from the operator's config: a
// per-chain entry wins over the global strategy, exactly like the
// engine's own precedence. ("", "none configured") when no
// strategy is configured anywhere — never a guessed default.
// The returned reason is router.StrategyReason: the documented
// mapping of strategy name to what it optimises for.
// M8.14 race fix, round 2: the whole routing block is read
// from one RoutingSnapshot taken under the shared routing
// read lock — the old code derefed s.cfg.Routing directly
// for GlobalStrategy, racing routingPersist's pointer swap.
func (s *Server) strategyForModel(model string) (strategy, reason, source string) {
	if s.cfg == nil {
		return "", "", "none configured"
	}
	rc := s.cfg.RoutingSnapshot()
	if rc == nil {
		return "", "", "none configured"
	}
	if model != "" {
		if st, ok := rc.ChainStrategies[model]; ok && st != "" {
			return st, router.StrategyReason(st), "config routing.chain_strategies[" + model + "]"
		}
	}
	if st := rc.GlobalStrategy; st != "" {
		return st, router.StrategyReason(st), "config routing.global_strategy"
	}
	return "", "", "none configured"
}

// lastServedRouting is the last REAL request's routing, clearly
// labelled as such: it is context for the explanation, never the
// answer for this prompt. HasDecision is false before any
// request was served.
type lastServedRouting struct {
	HasDecision bool                      `json:"has_decision"`
	Decision    *router.RoutingInfo       `json:"decision,omitempty"`
	Attempts    []routetrace.AttemptTrace `json:"attempts"`
}

// explainRoutingResponse is the GET /v1/explain/routing payload:
// the classification decision for this prompt, the strategy that
// would order its chain and why, and the last served routing for
// context. WouldCallUpstream is always false and UpstreamCalls
// always 0: explaining never dials a provider.
type explainRoutingResponse struct {
	Decision          autoRouteEnriched `json:"decision"`
	WouldCallUpstream bool              `json:"would_call_upstream"`
	UpstreamCalls     int               `json:"upstream_calls"`
	Strategy          *string           `json:"strategy"`
	StrategyReason    *string           `json:"strategy_reason,omitempty"`
	StrategySource    string            `json:"strategy_source"`
	LastServed        lastServedRouting `json:"last_served"`
}

// handleExplainRouting serves GET
// /v1/explain/routing?model=auto&prompt=... [&threshold=0.5]:
// what the router WOULD do with this prompt and why — the
// classification, the chain-ordering strategy and its reason,
// the last served decision + trace as context — without calling
// any upstream.
func (s *Server) handleExplainRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireAutoRouter(w) {
		return
	}
	p, err := parseIntrospectParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	model, dec, _, status, err := s.classifyPrompt(p)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	strategy, reason, source := s.strategyForModel(model)
	resp := explainRoutingResponse{
		Decision:          s.enrichDecision(r, dec),
		WouldCallUpstream: false,
		UpstreamCalls:     0,
		StrategySource:    source,
		LastServed:        s.lastServedRouting(r),
	}
	if strategy != "" {
		resp.Strategy = &strategy
		resp.StrategyReason = &reason
	}
	if status != http.StatusOK {
		// Unresolved decision: the explanation itself is the
		// honest answer — report it with a 502, as the
		// dashboard auto-route endpoint does.
		// AUD-CODE-002: encode before committing the status.
		w.Header().Set("Content-Type", "application/json")
		buf, err := json.Marshal(resp)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "response could not be encoded")
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write(buf)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// lastServedRouting reads the engine's most recent routing record
// and route trace. Both are labelled last-served: under
// concurrency they may belong to a neighbour request, and they
// are never presented as this prompt's decision.
func (s *Server) lastServedRouting(r *http.Request) lastServedRouting {
	out := lastServedRouting{Attempts: []routetrace.AttemptTrace{}}
	if info, ok := s.lastDecision(); ok {
		out.HasDecision = true
		out.Decision = &info
	}
	if trace := s.requestTrace(r.Context()); trace != nil && len(trace.Attempts) > 0 {
		out.Attempts = trace.Attempts
	}
	return out
}

// maxIntrospectSegments caps /v1/segment at 100 segments,
// mirroring the GET /v1/me/sessions list cap (M5.6). Longer
// prompts still classify their first 100 segments; truncated=true
// says so instead of silently dropping the rest.
const maxIntrospectSegments = 100

// splitPromptSegments splits a prompt into routing segments:
// blank-line paragraphs, then sentences (a run ending in . ! or ?
// followed by whitespace or end of input). Empty pieces are
// dropped; the text of each segment is kept verbatim (trimmed).
func splitPromptSegments(prompt string) []string {
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	var out []string
	for _, para := range strings.Split(prompt, "\n\n") {
		for _, seg := range splitSentences(para) {
			if seg != "" {
				out = append(out, seg)
			}
		}
	}
	return out
}

// splitSentences splits one paragraph into sentences. A sentence
// ends at '.', '!' or '?' when the next rune is whitespace or the
// input ends; anything else (abbreviations mid-sentence, decimal
// points) stays inside the segment.
func splitSentences(para string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(para)
	for i, ch := range runes {
		cur.WriteRune(ch)
		if ch != '.' && ch != '!' && ch != '?' {
			continue
		}
		if i+1 < len(runes) && !isSegmentSpace(runes[i+1]) {
			continue
		}
		seg := strings.TrimSpace(cur.String())
		cur.Reset()
		if seg != "" {
			out = append(out, seg)
		}
	}
	if rest := strings.TrimSpace(cur.String()); rest != "" {
		out = append(out, rest)
	}
	return out
}

// isSegmentSpace reports whether r separates sentences: Unicode
// whitespace, or a newline (paragraphs were already split, but a
// lone newline inside a paragraph separates segments too).
func isSegmentSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}

// segmentResult is one classified segment of a /v1/segment
// request. Resolved is false when the router classified the
// segment but has no concrete model configured for its tier —
// the decision attached still says why.
type segmentResult struct {
	Index    int               `json:"index"`
	Text     string            `json:"text"`
	Resolved bool              `json:"resolved"`
	Decision autoRouteEnriched `json:"decision"`
}

// segmentResponse is the GET /v1/segment payload: every segment
// of the prompt with its own routing decision. No upstream is
// called for any segment.
type segmentResponse struct {
	Model             string          `json:"model"`
	Segments          []segmentResult `json:"segments"`
	SegmentCount      int             `json:"segment_count"`
	Truncated         bool            `json:"truncated"`
	WouldCallUpstream bool            `json:"would_call_upstream"`
	UpstreamCalls     int             `json:"upstream_calls"`
}

// handleSegment serves GET /v1/segment?model=auto&prompt=...
// [&threshold=0.5]: split the prompt into segments and classify
// each one with the auto-router, so a caller can see which part
// of a long prompt would route where. No upstream is called.
func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireAutoRouter(w) {
		return
	}
	p, err := parseIntrospectParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	segments := splitPromptSegments(p.Prompt)
	truncated := false
	if len(segments) > maxIntrospectSegments {
		segments = segments[:maxIntrospectSegments]
		truncated = true
	}
	results := make([]segmentResult, 0, len(segments))
	for i, text := range segments {
		_, dec, ok := s.autoRouter.ResolveWithThreshold(p.Model, text, p.Threshold)
		results = append(results, segmentResult{
			Index:    i,
			Text:     text,
			Resolved: ok,
			Decision: s.enrichDecision(r, dec),
		})
	}
	writeJSON(w, http.StatusOK, segmentResponse{
		Model:             p.Model,
		Segments:          results,
		SegmentCount:      len(results),
		Truncated:         truncated,
		WouldCallUpstream: false,
		UpstreamCalls:     0,
	})
}
