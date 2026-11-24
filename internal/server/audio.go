// Audio passthrough handlers (M8.6).
//
// POST /v1/audio/speech, /v1/audio/transcriptions,
// /v1/audio/translations, plus the aliases POST
// /v1/audio/speech-to-text (= transcriptions) and POST
// /v1/audio/text-to-speech (= speech). Route registration lives
// in guard.go:225-229 and server.go:1397-1401 (wired at merge):
// every route is TierManagement, gateway-key auth, scope "chat"
// (the scope chat completions, embeddings and rerank already
// carry — audio is a model-invoking inference call like them,
// not a management read). Auth is enforced by the guard table
// at registration, not by these handlers, exactly like the
// embeddings handlers.
//
// Design (mirrors handleEmbedRerank): the handler validates,
// runs the shared pre-call gates (admission, guardrails on the
// speech input text, plugin pre-request hooks, key budgets,
// token limits, quota pools/schedules, internal budgets, team
// budget, traffic, throttle, key session), then hands the raw
// request body to the engine — Engine.AudioPassthrough — which
// routes it through the chain with failover selection,
// breaker/cooldown filtering, the adaptive limiter, ledger and
// trace, exactly once. The provider's response comes back
// verbatim: status, Content-Type and body bytes untouched.
//
// Wire rules:
//   - The body is forwarded byte-for-byte, never decoded and
//     rebuilt. Multipart audio bodies are forwarded with the
//     caller's original Content-Type (boundary included); the
//     handler only scans the multipart framing to extract the
//     `model` field and confirm a `file` part is present.
//   - Speech is JSON ({model, input, voice, ...}); the handler
//     parses model + input for validation and the guardrail
//     scan, and forwards the original JSON bytes.
//   - Transcriptions/translations DO carry text: OpenAI's
//     guide documents an optional `prompt` field on
//     transcriptions (plus keywords/languages) —
//     attacker-controlled text that reaches the upstream model.
//     The handler extracts it (extractAudioMultipart) and it
//     rides the bookkeeping request, so the guardrail scan and
//     the local token estimate cover it exactly like the speech
//     input text. (The M8.6 review found the old "no text to
//     scan" claim understated; whisper-1 prompts have a
//     224-token limit upstream, which the upstream enforces.)
//     The audio bytes themselves are still never scanned.
//
// Upload-size limit (M8.6 review finding 4, landed here): an
// audio upload is capped at audioMaxUploadBytes = 25 MiB,
// enforced with http.MaxBytesReader before the body is read,
// answering 413 on overflow. The value is OpenAI-derived, not
// invented: OpenAI's speech-to-text guide states "Files can be
// up to 25 MB". the reference router's transcriptions route carries no
// upload-size cap at all in-route (no size check in
// src/app/api/v1/audio/transcriptions/route.ts) — the gateway
// is stricter here, deliberately: one 100 MB upload is 100 MB
// of RSS (handler + engine + transport share the slice), and
// concurrent uploads multiply it. The engine's provider
// transport is separately bounded on the RESPONSE side by the
// provider audio client's 64 MiB cap (audioMaxResponseBody),
// which detects truncation instead of delivering a corrupt
// clip.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// audioEngine is the optional engine capability these handlers
// need (router.Engine implements it). A server whose engine does
// not implement it answers 503 — an honest "this deployment
// cannot serve this surface", never a faked clip or transcript.
type audioEngine interface {
	AudioPassthrough(context.Context, router.AudioRequest) (*router.AudioResponse, error)
}

// audioMaxUploadBytes caps one audio upload at 25 MiB (M8.6
// review finding 4). The value is OpenAI-derived: the
// speech-to-text guide states "Files can be up to 25 MB".
// Enforced with http.MaxBytesReader before the body is read;
// overflow answers 413.
const audioMaxUploadBytes = 25 << 20

// handleAudioSpeech serves POST /v1/audio/speech
// (text-to-speech, JSON in, audio bytes out).
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	s.handleAudio(w, r, router.AudioSpeech)
}

// handleAudioTextToSpeech serves POST /v1/audio/text-to-speech,
// the alias of POST /v1/audio/speech: same operation, same
// handler, no second wire shape. /v1/audio/text-to-speech is
// NOT an OpenAI endpoint (verified against the OpenAI Audio API
// reference, 2026-11-19); it exists because the reference router names
// its route that way.
func (s *Server) handleAudioTextToSpeech(w http.ResponseWriter, r *http.Request) {
	s.handleAudio(w, r, router.AudioSpeech)
}

// handleAudioTranscriptions serves POST
// /v1/audio/transcriptions (speech-to-text, multipart in, JSON
// {"text": ...} out).
func (s *Server) handleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	s.handleAudio(w, r, router.AudioTranscriptions)
}

// handleAudioSpeechToText serves POST /v1/audio/speech-to-text,
// the alias of POST /v1/audio/transcriptions: same operation,
// same handler, no second wire shape. /v1/audio/speech-to-text
// is NOT an OpenAI endpoint (verified against the OpenAI Audio
// API reference, 2026-11-19); it exists because the reference router names
// its route that way.
func (s *Server) handleAudioSpeechToText(w http.ResponseWriter, r *http.Request) {
	s.handleAudio(w, r, router.AudioTranscriptions)
}

// handleAudioTranslations serves POST /v1/audio/translations
// (translate audio to English, multipart in, JSON
// {"text": ...} out). No alias.
func (s *Server) handleAudioTranslations(w http.ResponseWriter, r *http.Request) {
	s.handleAudio(w, r, router.AudioTranslations)
}

// No voice-listing route is served: the audio surface is an
// OpenAI-wire passthrough, and the OpenAI audio API has no voice
// discovery endpoint. A fixed list here would present static data
// as live provider capability, so voice discovery is deferred until
// a provider with a real voices API (e.g. ElevenLabs) is wired.

// audioVoiceIDRe bounds the {voiceId} path segment to the
// characters real provider voice ids use.
var audioVoiceIDRe = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,128}$`)

// handleAudioTextToSpeechWithVoice serves POST
// /v1/audio/text-to-speech/{voiceId}: the path's voice id is written
// into the JSON body's "voice" field and the request then follows
// the normal speech path. A body that already names a DIFFERENT
// voice is refused 400 rather than silently overridden.
func (s *Server) handleAudioTextToSpeechWithVoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	voice := r.PathValue("voiceId")
	if !audioVoiceIDRe.MatchString(voice) {
		writeError(w, http.StatusBadRequest, "invalid voice id in path")
		return
	}
	// Admission before I/O, as in handleAudio (the check only
	// peeks, so handleAudio repeating it records nothing twice).
	if s.preBodyAdmission(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, audioMaxUploadBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large: one audio upload is capped at 25 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "invalid request: body must be a JSON object")
		return
	}
	if raw, ok := fields["voice"]; ok {
		var existing string
		if err := json.Unmarshal(raw, &existing); err != nil || existing != voice {
			writeError(w, http.StatusBadRequest, "body voice conflicts with the voice id in the path")
			return
		}
	}
	vb, _ := json.Marshal(voice)
	fields["voice"] = vb
	rewritten, err := json.Marshal(fields)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not encode request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(rewritten))
	r.ContentLength = int64(len(rewritten))
	s.handleAudio(w, r, router.AudioSpeech)
}

// audioSpeechWire is the JSON shape parsed from a speech
// request: only model + input are needed (validation, the
// guardrail scan, chain resolution). Every other field (voice,
// response_format, speed, ...) travels inside the forwarded raw
// body, never parsed or re-encoded here.
type audioSpeechWire struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// handleAudio is the shared handler for all five audio routes:
// they differ only in the canonical operation (aliases already
// mapped by the callers above) and in the request shape.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request, op router.AudioOperation) {
	handlerStart := time.Now()
	endpoint, ok := audioEndpointKind(op)
	if !ok {
		// Unreachable: the five handlers above pass only
		// canonical operations. A 500 here is honest if it
		// ever is.
		writeServerError(w, http.StatusInternalServerError, "unknown audio operation")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Panic isolation, mirroring handleChatCompletions: a panic
	// anywhere on this request's path must never kill the
	// gateway process. The client gets an honest 502, the panic
	// is counted + logged redacted, and a ledger row is recorded
	// when the engine recorded none. The ledger write uses a
	// fresh background context: this recover runs during unwind,
	// after the request context's cancel has already fired.
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

	// Admission before I/O: a blocked or over-limit key is
	// refused BEFORE the request body is read.
	if s.preBodyAdmission(w, r) {
		return
	}

	// The body is capped at 25 MiB (audioMaxUploadBytes): a
	// larger upload is refused 413 before a single byte is
	// buffered, so concurrent uploads cannot multiply into
	// unbounded RSS. Any other read failure keeps the 400.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, audioMaxUploadBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large: one audio upload is capped at 25 MiB")
			return
		}
		writeError(w, http.StatusBadRequest, "could not read request body")
		return
	}

	// Security cluster: a client-supplied Vertex
	// location/project is rejected unless the admin opted in.
	// Only the JSON speech body can carry one; a multipart
	// upload cannot be a Vertex JSON override.
	if op == router.AudioSpeech {
		if ov, ok := provider.DetectVertexOverride(body); ok {
			if refused := s.checkClientVertexOverride(w, &ov); refused {
				return
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	// A deployment whose engine cannot serve this surface says
	// so up front, before any guardrail or budget is evaluated:
	// 503, not a faked clip or transcript.
	ae, ok := s.engine.(audioEngine)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "audio is not available on this server")
		return
	}

	// Decode + validate once; the same raw body is handed to
	// the engine below, verbatim. The model (and, for speech,
	// the input text) is kept alongside for the bookkeeping
	// request.
	var model, text string
	switch op {
	case router.AudioSpeech:
		var speech audioSpeechWire
		if err := json.Unmarshal(body, &speech); err != nil {
			writeError(w, http.StatusBadRequest, "invalid request: "+err.Error())
			return
		}
		if speech.Model == "" {
			writeError(w, http.StatusBadRequest, "audio request model is required")
			return
		}
		if speech.Input == "" {
			writeError(w, http.StatusBadRequest, "audio speech request input is required")
			return
		}
		model, text = speech.Model, speech.Input
	case router.AudioTranscriptions, router.AudioTranslations:
		m, prompt, filePresent, err := extractAudioMultipart(body, r.Header.Get("Content-Type"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if m == "" {
			writeError(w, http.StatusBadRequest, "audio request model is required")
			return
		}
		if !filePresent {
			writeError(w, http.StatusBadRequest, "audio request file is required")
			return
		}
		model, text = m, prompt
	default:
		// Unreachable: endpoint was resolved above.
		writeServerError(w, http.StatusInternalServerError, "unknown audio operation")
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
	// internal/router/audio_passthrough.go: it carries the
	// speech input text — or the transcription prompt field,
	// when one was supplied — as a user message, the endpoint
	// kind, the caller ID and the per-request budget — never
	// sent upstream). An audio upload with no prompt field
	// has no messages: no local estimate and no strict
	// per-request-budget gate (there is nothing to count),
	// exactly like a token-ID embeddings input.
	bookReq = provider.ChatRequest{
		Model:    model,
		Endpoint: endpoint,
		CallerID: s.callerKeyID(r),
	}
	if text != "" {
		bookReq.Messages = append(bookReq.Messages, provider.Message{Role: router.RoleUser, Content: text})
	}
	// Guardrails run on the speech input text — or the
	// transcription prompt field — BEFORE any provider is
	// dialled, exactly as on chat completions. A blocked
	// request makes zero provider calls. For audio uploads
	// without a prompt field there is no text to scan (see
	// the package doc).
	if blocked := s.applyGuardrails(ctx, w, &bookReq, s.keyGuardrailScope(r)); blocked {
		return
	}
	// Plugin pre-request hooks: the same pipeline as chat
	// completions, run on the synthetic bookkeeping request
	// after the built-in guardrails and before every budget
	// gate and the engine. A model rewrite by a plugin is
	// synced onto the real request; a message-content rewrite
	// is NOT — the audio body travels verbatim and is never
	// rebuilt from the bookkeeping messages.
	if blocked, newCtx := s.applyPlugins(ctx, w, &bookReq); blocked {
		return
	} else {
		ctx = newCtx
	}
	if bookReq.Model != "" && bookReq.Model != model {
		model = bookReq.Model
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
	// P1.6 org/team policy: the authenticated key's team allowlists
	// (model, strategy) decide before dispatch, on this dialect too —
	// no alternate API is a policy bypass.
	if !s.enforceTeamPolicy(w, r, bookReq.Model, "") {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// Per-key traffic limits (CHECKLIST 195): same pre-call
	// gate as chat completions. The prompt tokens are estimated
	// from the speech input text via the bookkeeping request;
	// an audio upload has no text, so the token axis is empty
	// there and only the request-count axis applies.
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

	start := time.Now()
	audioReq := router.AudioRequest{
		Operation:    op,
		Model:        model,
		Body:         body,
		ContentType:  r.Header.Get("Content-Type"),
		Text:         text,
		CallerID:     bookReq.CallerID,
		BudgetUSD:    bookReq.BudgetUSD,
		BudgetStrict: bookReq.BudgetStrict,
	}
	engineCalled = true
	resp, err := ae.AudioPassthrough(ctx, audioReq)
	if err != nil {
		s.recordAudioMetrics(bookReq, "", err, start)
		// Telemetry headers (M2.1): the trace still names the
		// attempted target; tokens/cost stay absent.
		meta := s.responseMeta(ctx, bookReq, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		s.handleAudioEngineError(w, err)
		return
	}
	respModel := resp.Model

	// Plugin post-response hooks: fail-open, on the delivered
	// response only — a logging plugin's failure never
	// withholds it. The response is synthesised into the chat
	// shape the hooks take: model + usage are real; there is
	// no chat content (speech returns audio bytes, a
	// transcript is JSON, not a chat message), so Content
	// stays empty and the finish reason is "stop" — never
	// invented content or tokens. Audio usage is always zero
	// (no token usage block exists on this surface), so the
	// token/cost headers stay absent, never 0.
	pluginResp := &provider.ChatResponse{Model: respModel, FinishReason: "stop"}
	if pluginResp.Model == "" {
		pluginResp.Model = model
	}
	s.applyPostResponsePlugins(ctx, &bookReq, pluginResp)

	// The response names the surface it came from, so a client
	// (or a log reader) never has to guess whether a 200 was
	// chat, embeddings or audio.
	w.Header().Set("X-Codvian-Endpoint", string(endpoint))
	// Decision transparency (CHECKLIST 199): the single attempt
	// that served this call, with the strategy that ordered its
	// chain and the reason it was chosen.
	s.setDecisionHeader(w, respModel, model)
	// Telemetry headers (M2.1, choke point): cost/tokens from
	// the real usage block and ledger price map — an audio
	// response carries no usage block, so the token/cost
	// headers stay absent, never 0.
	meta := s.responseMeta(ctx, bookReq, pluginResp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	s.recordAudioMetrics(bookReq, respModel, nil, start)
	// Verbatim delivery: the provider's status, Content-Type
	// and body bytes, untouched. Only the Content-Type the
	// provider sent is forwarded (no other upstream headers);
	// when the provider sent none, Go's content sniffing
	// applies — no format is guessed here.
	status := resp.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	if resp.ContentType != "" {
		w.Header().Set("Content-Type", resp.ContentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.Body)
}

// audioEndpointKind maps a canonical audio operation onto the
// endpoint kind the bookkeeping request, trace and ledger carry.
func audioEndpointKind(op router.AudioOperation) (provider.EndpointKind, bool) {
	switch op {
	case router.AudioSpeech:
		return router.EndpointAudioSpeech, true
	case router.AudioTranscriptions:
		return router.EndpointAudioTranscriptions, true
	case router.AudioTranslations:
		return router.EndpointAudioTranslations, true
	default:
		return "", false
	}
}

// extractAudioMultipart scans a multipart/form-data body for
// the `model` field, the optional `prompt` field (OpenAI's
// transcriptions prompt — free text that reaches the upstream
// model, so the handler scans it with the guardrails; see the
// package doc) and the presence of a `file` part. It reads
// only the framing: the audio bytes themselves are never
// decoded, copied out, or rebuilt — the caller forwards the
// original body. A part named "model" after the file part is
// still found: every part is visited in order. The prompt is
// returned verbatim (trimmed); it is short text, never audio.
func extractAudioMultipart(body []byte, contentType string) (model, prompt string, filePresent bool, err error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", "", false, errors.New("audio request must be multipart/form-data")
	}
	if mediaType != "multipart/form-data" {
		return "", "", false, errors.New("audio request must be multipart/form-data")
	}
	boundary, ok := params["boundary"]
	if !ok || boundary == "" {
		return "", "", false, errors.New("audio request multipart body has no boundary")
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", false, errors.New("audio request multipart body is malformed: " + err.Error())
		}
		switch part.FormName() {
		case "model":
			// The model value is a short string; the audio
			// part is never read here (NextPart skips unread
			// data), so this cannot pull audio bytes into a
			// second copy.
			raw, err := io.ReadAll(part)
			if err != nil {
				return "", "", false, errors.New("audio request model field could not be read")
			}
			model = strings.TrimSpace(string(raw))
		case "prompt":
			// Same short-text rule as "model": a prompt is
			// text (whisper prompts are capped at 224 tokens
			// upstream), never audio bytes.
			raw, err := io.ReadAll(part)
			if err != nil {
				return "", "", false, errors.New("audio request prompt field could not be read")
			}
			prompt = strings.TrimSpace(string(raw))
		case "file":
			if part.FileName() != "" {
				filePresent = true
			}
		}
	}
	return model, prompt, filePresent, nil
}

// handleAudioEngineError maps an engine error from an audio call
// onto an HTTP status, mirroring handleEmbedEngineError: a
// no-capable-provider result is a property of the request plus
// the operator's chain — not a provider failure — so it answers
// an honest 400 naming the endpoint, never a 502. Everything
// else delegates to handleEngineError unchanged.
func (s *Server) handleAudioEngineError(w http.ResponseWriter, err error) {
	var ncp *router.NoCapableProviderError
	if errors.As(err, &ncp) {
		writeError(w, http.StatusBadRequest, ncp.Error())
		return
	}
	s.handleEngineError(w, err)
}

// recordAudioMetrics records one audio call in the metrics,
// mirroring recordEmbedMetrics but WITHOUT its cache branch and
// WITHOUT failover counting: an audio call is never served from
// the cache, and a single attempt has zero failovers by
// construction. The provider/model come from the engine's route
// trace (the one attempt that ran); the response's model wins
// when present.
func (s *Server) recordAudioMetrics(req provider.ChatRequest, respModel string, err error, started time.Time) {
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
