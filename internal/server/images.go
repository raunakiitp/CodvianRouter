// POST /v1/images/generations and POST /v1/images/edits —
// OpenAI-compatible images passthrough (M8.5).
//
// Design: both calls are routed through the SAME engine as POST
// /v1/chat/completions — chain resolution, budgets, guardrails,
// ledger, route trace — but each is replay-exempt
// (router.ReplayExemption): exactly ONE upstream attempt, never
// replayed onto another target. An images call is single-shot
// generation work paid for per image; a replay would
// double-apply upstream work (and double-bill), so the first
// target actually dialled owns the outcome, success or failure.
//
// The engine never sees the image bytes directly: the handler
// builds a synthetic provider.ChatRequest carrying the prompt
// text (for the shared guardrail / budget / ledger machinery —
// see internal/router/images_passthrough.go) and hands the real
// request to Engine.Images. The synthetic request is never sent
// upstream. Generations travel as raw JSON, edits as raw
// multipart/form-data — forwarded verbatim, never decoded and
// rebuilt, so binary image parts cannot be mangled by a
// re-encode. The upstream's response body is returned verbatim
// (b64_json stays b64_json); only the model and usage block are
// parsed, for the ledger and telemetry headers.
//
// OpenAI-compatible wire ONLY: a native-wire provider
// (Anthropic/Gemini/Cohere/Bedrock) is skipped at engine
// selection, and when no target can serve the endpoint the
// answer is an honest 400 naming the limitation — never a
// mistranslation onto a native image surface.
//
// Endpoint names verified against the OpenAI Images API
// (2026-11-19): generations and edits are the only two live
// endpoints. Variations is retired and /v1/images/upscale does
// not exist in the OpenAI API — neither is built here.
//
// COORDINATOR WIRING (this file only defines the handlers; the
// routes live in files this slice must not edit):
//   - internal/server/guard.go route table, next to the
//     embeddings/rerank rows (TierManagement, gateway-key auth,
//     scope "chat" — the embeddings/rerank precedent; see the
//     M8.5 report for the proposal):
//     {Method: "POST", Path: "/v1/images/generations", Tier: TierManagement, Description: "Images generations — OpenAI-compatible, routed through the same engine as chat completions (budgets, guardrails, ledger, route trace) but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
//     {Method: "POST", Path: "/v1/images/edits", Tier: TierManagement, Description: "Images edits — OpenAI-compatible multipart/form-data, forwarded verbatim (never decoded and rebuilt), routed through the same engine as chat completions but replay-exempt: exactly one upstream attempt, never replayed onto another target. Gateway-key auth required.", Scopes: []string{"chat"}},
//   - internal/server/server.go route registration, next to the
//     embeddings/rerank lines:
//     s.register(mux, "POST", "/v1/images/generations", s.handleImagesGenerations)
//     s.register(mux, "POST", "/v1/images/edits", s.handleImagesEdits)
//
// Streaming generations (stream:true) are refused 400 here:
// the passthrough is a single non-streaming POST. The inbound
// body is capped at maxRequestBody (10 MiB, the same cap as
// chat/embeddings — mirrored, not invented): a multi-image edit
// larger than that is refused before any provider is dialled.
// Both caps are open questions for large images; see the M8.5
// report.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// imagesEngine is the optional engine capability these handlers
// need (router.Engine implements it). A server whose engine
// does not implement it answers 503 — an honest "this
// deployment cannot serve this surface", never a faked image.
type imagesEngine interface {
	Images(context.Context, provider.ImageRequest) (*provider.ImageResponse, error)
}

// handleImagesGenerations serves POST /v1/images/generations.
func (s *Server) handleImagesGenerations(w http.ResponseWriter, r *http.Request) {
	s.handleImages(w, r, provider.ImageGenerations)
}

// handleImagesEdits serves POST /v1/images/edits.
func (s *Server) handleImagesEdits(w http.ResponseWriter, r *http.Request) {
	s.handleImages(w, r, provider.ImageEdits)
}

// handleImages is the shared handler for both surfaces: they
// differ only in the request shape (JSON vs multipart) and the
// operation name the engine, ledger and trace carry.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request, op provider.ImageOperation) {
	handlerStart := time.Now()
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Panic isolation, mirroring handleEmbedRerank: a panic
	// anywhere on this request's path must never kill the
	// gateway process. The client gets an honest 502, the panic
	// is counted + logged redacted, and a ledger row is recorded
	// when the engine recorded none.
	var bookReq provider.ChatRequest
	engineCalled := false
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_images", v)
			s.writePanicFailure(w)
			if bookReq.Model != "" && (!engineCalled || s.traceHasNoAttempts()) {
				rowCtx, rowCancel := context.WithTimeout(context.Background(), 5*time.Second)
				s.recordPanicRow(rowCtx, s.callerKeyID(r), bookReq, handlerStart)
				rowCancel()
			}
		}
	}()

	// Admission before I/O: a blocked or over-limit key is
	// refused BEFORE the request body is read.
	if s.preBodyAdmission(w, r) {
		return
	}

	// The request body is capped at maxRequestBody (10 MiB, the
	// same cap as chat/embeddings): an unbounded body is a
	// memory-exhaustion vector for the gateway. Multipart edits
	// carry binary image parts, so the cap bites sooner here
	// than on JSON — that is the mirrored limit, flagged open
	// in the M8.5 report, not a per-image judgement.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body too large")
		return
	}

	// Security cluster: a client-supplied Vertex
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
	// 503, not a faked image.
	ie, ok := s.engine.(imagesEngine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "images are not available on this server")
		return
	}

	// Decode + validate once. Generations is JSON (model,
	// prompt, stream); edits is multipart/form-data whose model
	// and prompt fields are sniffed out of the raw stream — the
	// bytes themselves are never rebuilt.
	var model, prompt string
	contentType := r.Header.Get("Content-Type")
	switch op {
	case provider.ImageGenerations:
		var decoded struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Stream *bool  `json:"stream"`
		}
		if err := json.Unmarshal(body, &decoded); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		if decoded.Model == "" {
			writeError(w, http.StatusBadRequest, "images generations request model is required")
			return
		}
		if decoded.Prompt == "" {
			writeError(w, http.StatusBadRequest, "images generations request prompt is required")
			return
		}
		if decoded.Stream != nil && *decoded.Stream {
			writeError(w, http.StatusBadRequest, "images generations streaming (stream:true) is not supported by this gateway")
			return
		}
		model, prompt = decoded.Model, decoded.Prompt
	case provider.ImageEdits:
		if contentType == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "multipart/form-data") {
			writeError(w, http.StatusBadRequest, "images edits request must be multipart/form-data")
			return
		}
		var sniffErr error
		model, prompt, sniffErr = sniffMultipartFields(body, contentType)
		if sniffErr != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+sniffErr.Error())
			return
		}
		if model == "" {
			writeError(w, http.StatusBadRequest, "images edits request model is required")
			return
		}
		if prompt == "" {
			writeError(w, http.StatusBadRequest, "images edits request prompt is required")
			return
		}
	default:
		writeServerError(w, http.StatusInternalServerError, "unknown images endpoint")
		return
	}

	// Readiness gate: a gateway-provided local capability that
	// is not ready answers 503 with Retry-After here, before
	// any provider is dialled.
	if !s.checkLocalReadiness(w, r, model) {
		return
	}

	// The synthetic bookkeeping request drives the shared
	// guardrail / budget / ledger / metrics machinery (see
	// internal/router/images_passthrough.go: it carries the
	// prompt text as a user message, the endpoint kind, the
	// caller and the budget — never sent upstream). The image
	// bytes are never counted: the local estimate covers the
	// prompt text only.
	bookReq = provider.ChatRequest{
		Model:    model,
		Endpoint: provider.EndpointImages,
		CallerID: s.callerKeyID(r),
	}
	if prompt != "" {
		bookReq.Messages = append(bookReq.Messages, provider.Message{Role: router.RoleUser, Content: prompt})
	}
	// Guardrails run on the prompt text BEFORE any provider is
	// dialled, exactly as on chat completions. A blocked request
	// makes zero provider calls.
	if blocked := s.applyGuardrails(ctx, w, &bookReq, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks: the same pipeline as chat
	// completions, run on the synthetic bookkeeping request
	// after the built-in guardrails and before every budget
	// gate and the engine — a denial happens before any
	// provider is dialled, so it can never cause a retry. A
	// model rewrite by a plugin is synced onto the real
	// request for generations (the JSON body's "model" field
	// is rewritten — the only rebuild on this path, and only
	// when a plugin actually rewrote the model — and the
	// local model variable follows it, so chain, budgets,
	// engine request, body and ledger all name the rewritten
	// model). For edits a
	// model rewrite is NOT applied: rewriting it would mean
	// rebuilding the multipart body and risking the binary
	// parts, so the sniffed model stands and the body travels
	// verbatim. A message-content rewrite is NOT applied on
	// either surface — the prompt travels verbatim inside the
	// body, exactly as the embeddings input does.
	if blocked, newCtx := s.applyPlugins(ctx, w, &bookReq); blocked {
		return
	} else {
		ctx = newCtx
	}
	if bookReq.Model != "" && bookReq.Model != model {
		if op == provider.ImageGenerations {
			rewritten, rewriteErr := rewriteJSONModel(body, bookReq.Model)
			if rewriteErr != nil {
				writeError(w, http.StatusBadRequest, "invalid request: "+rewriteErr.Error())
				return
			}
			body = rewritten
			// Generations: the rewrite is applied, so the
			// rewritten model names the chain, the budgets,
			// the engine request, the body and the ledger —
			// all B, never a mix of A and B.
			model = bookReq.Model
		} else {
			// Edits: the rewrite is deliberately not applied
			// (see above); the engine still routes on the
			// sniffed model. The bookkeeping model is
			// restored so the ledger and trace name the
			// model that was actually served.
			bookReq.Model = model
		}
	}
	// Per-key access schedule (M5.10): a key outside its allowed
	// hours is refused with a 403 here, before any budget is
	// touched.
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	// Per-request budget (X-Codvian-Budget), the prompt budget
	// (estimate from the prompt text) and the key budget: all
	// refused 402/429 BEFORE dialling, with zero provider hits.
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
	// Fair-share quota pools (M5.4), quota schedules (M5.9)
	// and internal budgets (M5.11): all decide before the
	// traffic check.
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	if refused := s.checkInternalBudgets(w, r, bookReq.Model); refused {
		return
	}
	// P1.6 org/team policy: the authenticated key's team allowlists
	// (model, strategy) decide before dispatch, on this dialect too —
	// no alternate API is a policy bypass.
	if !s.enforceTeamPolicy(w, r, bookReq.Model, "") {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits: same pre-call gate as chat
	// completions. The prompt tokens are estimated from the
	// prompt text via the bookkeeping request.
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

	imgReq := provider.ImageRequest{
		Operation:    op,
		Model:        model,
		Body:         body,
		ContentType:  contentType,
		PromptText:   prompt,
		CallerID:     bookReq.CallerID,
		BudgetUSD:    bookReq.BudgetUSD,
		BudgetStrict: bookReq.BudgetStrict,
	}
	start := time.Now()
	engineCalled = true
	resp, err := ie.Images(ctx, imgReq)
	if err != nil {
		s.recordImageMetrics(bookReq, "", err, start)
		// Telemetry headers: the trace still names the
		// attempted target; tokens/cost stay absent.
		meta := s.responseMeta(ctx, bookReq, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleImagesEngineError(w, err)
		return
	}

	// Plugin post-response hooks: fail-open, on the delivered
	// response only. The response is synthesised into the chat
	// shape the hooks take: model + usage are real; there is no
	// chat content, so Content stays empty and the finish reason
	// is "stop" — never invented content or tokens. A response
	// with no upstream usage block keeps zero usage, never an
	// estimate.
	pluginResp := &provider.ChatResponse{Model: resp.Model, Usage: resp.Usage, FinishReason: "stop"}
	if pluginResp.Model == "" {
		pluginResp.Model = model
	}
	s.applyPostResponsePlugins(ctx, &bookReq, pluginResp)

	// The response names the surface it came from, so a client
	// (or a log reader) never has to guess whether a 200 was
	// chat, embeddings or images.
	w.Header().Set("X-Codvian-Endpoint", string(provider.EndpointImages))
	// Decision transparency: the single attempt that served
	// this call, with the strategy that ordered its chain and
	// the reason it was chosen.
	s.setDecisionHeader(w, resp.Model, model)
	// Telemetry headers (M2.1, choke point): cost/tokens from
	// the real usage block and ledger price map — a response
	// with no upstream usage block keeps the token/cost headers
	// absent, never 0.
	meta := s.responseMeta(ctx, bookReq, pluginResp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	s.recordImageMetrics(bookReq, resp.Model, nil, start)
	// Verbatim: the upstream's response body, byte for byte
	// (b64_json stays b64_json). The status is the engine's
	// success status (200).
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(resp.Body)
}

// handleImagesEngineError maps an engine error from an images
// call onto an HTTP status. A no-capable-provider result is a
// property of the request plus the operator's chain — not a
// provider failure — so it answers an honest 400 naming the
// endpoint, never a 502. An upstream error answers with the
// upstream's OWN status and error body, verbatim (the body the
// typed error carries is redacted and truncated to 64 KiB by
// the provider client at construction — verbatim within that
// bound) — except a status below 400, which is not an error
// status at all and clamps to 502 (a malformed upstream 200).
// Everything else delegates to handleEngineError
// unchanged: a content refusal is 403/-32001, a budget breach
// is 402, a deadline is 504.
func (s *Server) handleImagesEngineError(w http.ResponseWriter, err error) {
	var ncp *router.NoCapableProviderError
	if errors.As(err, &ncp) {
		writeError(w, http.StatusBadRequest, ncp.Error())
		return
	}
	var rl *provider.RateLimitError
	if errors.As(err, &rl) {
		if rl.RetryAfterPresent {
			seconds := int(rl.RetryAfter / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, rl.Body)
		return
	}
	var qe *provider.QuotaError
	if errors.As(err, &qe) {
		status := qe.StatusCode
		if status < 100 || status > 599 {
			status = http.StatusPaymentRequired
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, qe.Body)
		return
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		// An upstream error answers with the upstream's OWN
		// status — but never below 400: a malformed upstream
		// 200 (e.g. a truncated b64_json body) is an upstream
		// failure and clamps to 502, so the wire never says
		// 200 while telemetry says 502.
		status := ue.StatusCode
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, ue.Body)
		return
	}
	s.handleEngineError(w, err)
}

// recordImageMetrics records one images call in the metrics,
// mirroring recordEmbedMetrics but WITHOUT its cache branch and
// WITHOUT failover counting: an images call is never served from
// the cache (so a stale LastCacheHit from a previous chat
// request would misattribute it to "cache"), and a single
// attempt has zero failovers by construction. The
// provider/model come from the engine's route trace (the one
// attempt that ran); the response's model wins when present.
func (s *Server) recordImageMetrics(req provider.ChatRequest, respModel string, err error, started time.Time) {
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

// sniffMultipartFields extracts the "model" and "prompt" text
// fields from a raw multipart/form-data body WITHOUT rebuilding
// it: the caller keeps the original bytes and forwards them
// verbatim. File parts are discarded as they are read. Only the
// first value of each field is kept.
func sniffMultipartFields(body []byte, contentType string) (model, prompt string, err error) {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", "", err
	}
	boundary, ok := params["boundary"]
	if !ok || boundary == "" {
		return "", "", errors.New("multipart/form-data request has no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", err
		}
		name := part.FormName()
		if part.FileName() != "" || (name != "model" && name != "prompt") {
			_, _ = io.Copy(io.Discard, part)
			continue
		}
		// Text field: bounded by the whole-body cap the caller
		// already applied, so no second cap is invented here.
		value, err := io.ReadAll(part)
		if err != nil {
			return "", "", err
		}
		switch name {
		case "model":
			if model == "" {
				model = string(value)
			}
		case "prompt":
			if prompt == "" {
				prompt = string(value)
			}
		}
	}
	return model, prompt, nil
}

// rewriteJSONModel returns body with its top-level "model"
// field replaced by model. Used only when a plugin rewrote the
// model on a generations (JSON) request; the body is otherwise
// forwarded untouched.
func rewriteJSONModel(body []byte, model string) ([]byte, error) {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	decoded["model"] = encoded
	return json.Marshal(decoded)
}
