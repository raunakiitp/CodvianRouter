// POST /v1/embeddings and POST /v1/rerank — OpenAI-compatible
// passthrough (CHECKLIST 171-172; competitor-sweep P1 gap:
// LiteLLM/Portkey route embeddings and rerank through the same
// gateway, with the same budgets, guardrails and ledger).
//
// Design: both calls are routed through the SAME engine as POST
// /v1/chat/completions — chain resolution, budgets, guardrails,
// ledger, route trace — but each is replay-exempt
// (router.ReplayExemption): exactly ONE upstream attempt, never
// replayed onto another target. A target skipped before dialling
// (no capability, zero quota, breaker open, cooling, limiter
// shed) is selection, not a replay; the first target actually
// dialled owns the outcome, success or failure.
//
// The engine never sees the embeddings/rerank shapes directly:
// the handler builds a synthetic provider.ChatRequest carrying
// the input text (for the shared guardrail / budget / ledger
// machinery — see internal/router/embeddings.go) and hands the
// real request to Engine.Embed / Engine.Rerank. The synthetic
// request is never sent upstream.
//
// Plugin hooks (CHECKLIST 219-220) run on both surfaces through
// that same synthetic request: the pre-request pipeline after the
// built-in guardrails (deny/error fail closed, panic fails closed
// as a gateway panic — the chat semantics), and the post-response
// pipeline on a delivered response (fail-open, shape-only). The
// replay exemption is untouched: a plugin denial happens before
// the engine is called, so it can never cause a retry, and a
// delivered response is still exactly one upstream attempt.
//
// Deliberately NOT cached, coalesced or stale-served: an
// embedding is a stateful single-shot call, and a rerank scores
// the caller's own documents once — serving a cached vector or
// scoring would hand out another request's result and hide the
// upstream call the ledger must record.
//
// Shape notes: embeddings are returned verbatim (json.RawMessage
// — float arrays stay float arrays, base64 stays base64); only
// the usage block is parsed. Rerank follows the Cohere/LiteLLM
// shape (OpenAI has no /v1/rerank): model, query, documents,
// top_n, return_documents; results carry index +
// relevance_score; usage only when the upstream reports it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// embedRerankEngine is the optional engine capability these
// handlers need (router.Engine implements it; degrade.StaleEngine
// forwards it with no caching and no stale fallback). A server
// whose engine does not implement it answers 503 — an honest
// "this deployment cannot serve this surface", never a faked
// vector or scoring.
type embedRerankEngine interface {
	Embed(context.Context, provider.EmbeddingRequest) (*provider.EmbeddingResponse, error)
	Rerank(context.Context, provider.RerankRequest) (*provider.RerankResponse, error)
}

// handleEmbeddings serves POST /v1/embeddings.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.handleEmbedRerank(w, r, provider.EndpointEmbeddings)
}

// handleRerank serves POST /v1/rerank.
func (s *Server) handleRerank(w http.ResponseWriter, r *http.Request) {
	s.handleEmbedRerank(w, r, provider.EndpointRerank)
}

// handleEmbedRerank is the shared handler for both surfaces:
// they differ only in the request/response shapes and the
// endpoint name the engine, ledger and trace carry.
func (s *Server) handleEmbedRerank(w http.ResponseWriter, r *http.Request, endpoint provider.EndpointKind) {
	handlerStart := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Panic isolation (CHECKLIST 165-166), mirroring
	// handleChatCompletions: a panic anywhere on this request's
	// path must never kill the gateway process. The client gets
	// an honest 502, the panic is counted + logged redacted, and
	// a ledger row is recorded when the engine recorded none.
	// The ledger write uses a fresh background context: this
	// recover runs during unwind, after the request context's
	// cancel has already fired.
	var bookReq provider.ChatRequest
	engineCalled := false
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_"+string(endpoint), v)
			s.writePanicFailure(w)
			if bookReq.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), bookReq, handlerStart)
				rowCancel()
			}
		}
	}()

	// Admission before I/O (CHECKLIST 277 (b), extending 243 (g)
	// beyond chat + Responses): a blocked or over-limit key is
	// refused BEFORE the request body is read — the budget cap and
	// the requests-per-minute ceiling need only the key, never the
	// body. A key admitted here is still re-checked in full after
	// decode; this only ever refuses early.
	if s.preBodyAdmission(w, r) {
		return
	}

	// The request body is capped at 10 MiB, like chat: an
	// embeddings batch is legitimately large, but an unbounded
	// body is a memory-exhaustion vector for the gateway.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large")
		return
	}

	// Security cluster (CHECKLIST 245): a client-supplied Vertex
	// location/project is rejected unless the admin opted in.
	if ov, ok := provider.DetectVertexOverride(body); ok {
		if refused := s.checkClientVertexOverride(w, &ov); refused {
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// A deployment whose engine cannot serve this surface says
	// so up front, before any guardrail or budget is evaluated:
	// 503, not a faked vector or scoring.
	ee, ok := s.engine.(embedRerankEngine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "embeddings and rerank are not available on this server")
		return
	}

	// Decode + validate once; the same request is handed to the
	// engine below. The input texts are kept alongside for the
	// bookkeeping request.
	var texts []string
	var embedReq provider.EmbeddingRequest
	var rerankReq provider.RerankRequest
	switch endpoint {
	case provider.EndpointEmbeddings:
		if err := json.Unmarshal(body, &embedReq); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		if err := embedReq.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		texts = embedReq.TextInputs()
	case provider.EndpointRerank:
		if err := json.Unmarshal(body, &rerankReq); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		if err := rerankReq.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		texts = rerankReq.TextDocuments()
		// Object documents (e.g. {"text": ...}) are skipped by
		// TextDocuments — their text lives under
		// provider-specific keys the gateway does not parse.
		// But their content still travels upstream verbatim, so
		// the guardrail must see it: the raw JSON of each
		// non-string document joins the scan as-is. No key is
		// guessed and nothing is invented — the scan sees
		// exactly what the provider will receive. Without this,
		// a rerank document shaped as an object bypassed the
		// request guardrail entirely (only the query was
		// scanned). The raw JSON also joins the token estimate,
		// so object documents stop counting as zero tokens.
		for _, d := range rerankReq.Documents {
			var s string
			if json.Unmarshal(d, &s) == nil {
				continue // plain string: already in texts
			}
			texts = append(texts, string(d))
		}
		texts = append([]string{rerankReq.Query}, texts...)
	default:
		// Unreachable: the two handlers above pass only the
		// two endpoints. A 500 here is honest if it ever is.
		writeServerError(w, http.StatusInternalServerError, "unknown embeddings endpoint")
		return
	}
	model := embedReq.Model
	if endpoint == provider.EndpointRerank {
		model = rerankReq.Model
	}

	// Readiness gate: a gateway-provided local capability that
	// is not ready answers 503 with Retry-After here, before
	// any provider is dialled.
	if !s.checkLocalReadiness(w, r, model) {
		return
	}

	// The synthetic bookkeeping request drives the shared
	// guardrail / budget / ledger / metrics machinery (see
	// internal/router/embeddings.go: it carries the input text
	// as user messages, the endpoint kind, the caller and the
	// budget — never sent upstream). Token-ID embeddings inputs
	// carry no text: they get no local estimate and no strict
	// per-request-budget gate (there is nothing to count).
	bookReq = provider.ChatRequest{
		Model:    model,
		Endpoint: endpoint,
		CallerID: s.callerKeyID(r),
	}
	for _, t := range texts {
		bookReq.Messages = append(bookReq.Messages, provider.Message{Role: router.RoleUser, Content: t})
	}
	// Guardrails run on the input text BEFORE any provider is
	// dialled, exactly as on chat completions. A blocked request
	// makes zero provider calls.
	if blocked := s.applyGuardrails(ctx, w, &bookReq, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks (CHECKLIST 219-220): the same
	// pipeline as chat completions, run on the synthetic
	// bookkeeping request after the built-in guardrails and
	// before every budget gate and the engine — a denial or
	// fail-closed refusal happens before any provider is
	// dialled, so it can never cause a retry (the replay
	// exemption is preserved: the engine is simply never
	// called). The ctx below carries any plugin-added outbound
	// headers to the provider client, exactly as on chat.
	// A model rewrite by a plugin is synced onto the real
	// request; a message-content rewrite is NOT — the
	// embeddings/rerank input travels verbatim (RawMessage)
	// and is never rebuilt from the bookkeeping messages.
	if blocked, newCtx := s.applyPlugins(ctx, w, &bookReq); blocked {
		return
	} else {
		ctx = newCtx
	}
	if bookReq.Model != "" && bookReq.Model != model {
		model = bookReq.Model
		switch endpoint {
		case provider.EndpointEmbeddings:
			embedReq.Model = model
		case provider.EndpointRerank:
			rerankReq.Model = model
		}
	}
	// Per-key access schedule (M5.10): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-request budget (X-Codvian-Budget), the prompt budget
	// (estimate from the input text) and the key budget: all
	// refused 402/429 BEFORE dialling, with zero provider hits.
	// The budget the caller attached is copied onto the real
	// request below, so the engine's strict per-request gate
	// sees it.
	if refused := s.enforceRequestBudget(w, r, &bookReq); refused {
		return
	}
	if refused := s.enforcePromptBudget(w, r, &bookReq); refused {
		return
	}
	// M5.8: the budget check commits the prompt's estimated
	// cost; settled by the defer once this handler returns.
	if resID, refused := s.reserveKeyBudget(w, r, &bookReq); refused {
		return
	} else {
		defer s.settleReservation(resID)
	}
	// Per-key token limits (M5.3): global + model scopes for
	// the requested model, before the traffic check.
	if refused := s.checkKeyTokenLimits(w, r, "", bookReq.Model); refused {
		return
	}
	// Fair-share quota pools (M5.4): the caller's pools decide
	// before the traffic check.
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
	if refused := s.checkInternalBudgets(w, r, bookReq.Model); refused {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): same pre-call
	// gate as chat completions. The prompt tokens are estimated
	// from the input text via the bookkeeping request, so the
	// tokens-per-minute axis applies here too.
	if refused := s.checkKeyTrafficForChat(w, r, &bookReq); refused {
		return
	}
	// Per-key throttle delay + max sessions (M5.10): after every
	// refusal gate and before the engine is called; the session
	// slot is held for the whole handler via the defer.
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)

	var respBody any
	var respModel string
	start := time.Now()
	switch endpoint {
	case provider.EndpointEmbeddings:
		embedReq.CallerID = bookReq.CallerID
		embedReq.BudgetUSD = bookReq.BudgetUSD
		embedReq.BudgetStrict = bookReq.BudgetStrict
		engineCalled = true
		resp, err := ee.Embed(ctx, embedReq)
		if err != nil {
			s.recordEmbedMetrics(bookReq, "", err, start)
			// Telemetry headers (M2.1): the trace still names
			// the attempted target; tokens/cost stay absent.
			meta := s.responseMeta(ctx, bookReq, nil, start)
			s.writeResponseHeaders(w, ctx, meta)
			s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
			s.handleEmbedEngineError(w, err)
			return
		}
		respBody = resp
		respModel = resp.Model
	case provider.EndpointRerank:
		rerankReq.CallerID = bookReq.CallerID
		rerankReq.BudgetUSD = bookReq.BudgetUSD
		rerankReq.BudgetStrict = bookReq.BudgetStrict
		engineCalled = true
		resp, err := ee.Rerank(ctx, rerankReq)
		if err != nil {
			s.recordEmbedMetrics(bookReq, "", err, start)
			// Telemetry headers (M2.1): as on the Embed branch.
			meta := s.responseMeta(ctx, bookReq, nil, start)
			s.writeResponseHeaders(w, ctx, meta)
			s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
			s.handleEmbedEngineError(w, err)
			return
		}
		respBody = resp
		respModel = resp.Model
	}

	// Plugin post-response hooks (CHECKLIST 219-220): fail-open,
	// on the delivered response only — a logging plugin's
	// failure never withholds it. The response is synthesised
	// into the chat shape the hooks take: model + usage are
	// real; there is no chat content, so Content stays empty
	// (ContentChars 0) and the finish reason is "stop" — never
	// invented content or tokens. A rerank response with no
	// upstream usage block keeps zero usage, never an estimate.
	var pluginResp *provider.ChatResponse
	switch endpoint {
	case provider.EndpointEmbeddings:
		if r, ok := respBody.(*provider.EmbeddingResponse); ok && r != nil {
			pluginResp = &provider.ChatResponse{Model: r.Model, Usage: r.Usage, FinishReason: "stop"}
		}
	case provider.EndpointRerank:
		if r, ok := respBody.(*provider.RerankResponse); ok && r != nil {
			pluginResp = &provider.ChatResponse{Model: r.Model, FinishReason: "stop"}
			if r.Usage != nil {
				pluginResp.Usage = *r.Usage
			}
		}
	}
	if pluginResp != nil {
		if pluginResp.Model == "" {
			pluginResp.Model = model
		}
		s.applyPostResponsePlugins(ctx, &bookReq, pluginResp)
	}

	// The response names the surface it came from, so a client
	// (or a log reader) never has to guess whether a 200 was
	// chat, embeddings or rerank.
	w.Header().Set("X-Codvian-Endpoint", string(endpoint))
	// Decision transparency (CHECKLIST 199): the single attempt
	// that served this call, with the strategy that ordered its
	// chain and the reason it was chosen. Embeddings/rerank are
	// never cached or stale-served, so the suppression guard
	// inside never fires here — but a failed or skipped chain
	// still yields no header (no serving provider to name).
	s.setDecisionHeader(w, respModel, model)
	// Telemetry headers (M2.1, choke point): cost/tokens from the
	// real usage block and ledger price map — a rerank response
	// with no upstream usage block keeps the token/cost headers
	// absent, never 0.
	meta := s.responseMeta(ctx, bookReq, pluginResp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	s.recordEmbedMetrics(bookReq, respModel, nil, start)
	writeJSON(w, http.StatusOK, respBody)
}

// handleEmbedEngineError maps an engine error from an
// embeddings/rerank call onto an HTTP status. It is
// handleEngineError (the chat path) with one addition: a
// no-capable-provider result is a property of the request plus
// the operator's chain — not a provider failure — so it answers
// an honest 400 naming the endpoint, never a 502. Everything
// else delegates to handleEngineError unchanged: a 405 from the
// single attempt surfaces as the upstream's own 405 (the
// taxonomy would fall back to another target, but the replay
// exemption forbids it — the one attempt that ran is the whole
// story), a content refusal is 403/-32001, a budget breach is
// 402, a deadline is 504.
func (s *Server) handleEmbedEngineError(w http.ResponseWriter, err error) {
	var ncp *router.NoCapableProviderError
	// NOTE (CHECKLIST 177 slice, minimal fix for a
	// concurrent-slice build break): the target was a
	// NoCapableProviderError value, whose Error() is on the
	// pointer receiver — errors.As needs a pointer to a type
	// that implements error, so the target must be
	// *NoCapableProviderError.
	if errors.As(err, &ncp) {
		writeError(w, http.StatusBadRequest, ncp.Error())
		return
	}
	s.handleEngineError(w, err)
}

// recordEmbedMetrics records one embeddings/rerank call in the
// metrics, mirroring recordMetrics but WITHOUT its cache branch
// and WITHOUT failover counting: an embeddings/rerank call is
// never served from the cache (so a stale LastCacheHit from a
// previous chat request would misattribute it to "cache"), and
// a single attempt has zero failovers by construction. The
// provider/model come from the engine's route trace (the one
// attempt that ran); the response's model wins when present.
func (s *Server) recordEmbedMetrics(req provider.ChatRequest, respModel string, err error, started time.Time) {
	rec := s.recorder
	if rec == nil {
		return
	}
	providerName, model := "unknown", req.Model
	if tp, ok := s.engine.(traceProvider); ok {
		if trace := tp.LastTrace(); trace != nil && len(trace.Attempts) > 0 {
			last := trace.Attempts[len(trace.Attempts)-1]
			providerName, model = last.Provider, last.Model
		}
	}
	if respModel != "" {
		model = respModel
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
