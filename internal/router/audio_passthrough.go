// Audio passthrough (M8.6, CHECKLIST audio slice).
//
// POST /v1/audio/speech, /v1/audio/transcriptions and
// /v1/audio/translations — OpenAI-compatible audio, routed through
// the SAME engine as chat completions and embeddings: chain
// resolution, budgets, breaker/cooldown filtering, the adaptive
// limiter, ledger and trace — but replay-exempt (exactly ONE
// upstream attempt, never replayed onto another target), via
// singleAttempt, the same helper Embed/Rerank use. Audio work is
// single-shot: re-speaking a TTS prompt or re-transcribing an
// upload would double-apply upstream work (and double-bill it),
// so the first target actually dialled owns the outcome, success
// or failure. A target skipped before dialling (no capability,
// zero quota, breaker open, cooling, limiter shed) is selection,
// not a replay.
//
// Endpoint names, verified against the OpenAI API shape
// (developers.openai.com/api/docs/guides/speech-to-text and the
// Audio API reference, checked 2026-11-19):
//   - POST /v1/audio/speech — text-to-speech, JSON body
//     {model, input, voice, response_format, ...}, returns audio
//     bytes.
//   - POST /v1/audio/transcriptions — speech-to-text, multipart
//     file + model, returns JSON {"text": ...}.
//   - POST /v1/audio/translations — translate audio to English,
//     multipart file + model, returns JSON {"text": ...}.
//
// /v1/audio/speech-to-text and /v1/audio/text-to-speech are NOT
// OpenAI endpoints: they are descriptive names (the reference router uses
// them as its own route names). This slice serves them as ALIASES
// of the canonical operations — speech-to-text is transcriptions,
// text-to-speech is speech — one handler each, no second
// operation, no invented wire shape. The engine knows only the
// three canonical operations; the server maps the aliases.
//
// Wire rule: OpenAI-compatible wire ONLY. A native-wire provider
// (Anthropic, Gemini, Cohere, Bedrock) has no /v1/audio/* surface
// in this slice, so it is skipped at selection (supportsAudio)
// and never dialled for audio. When no target in the chain can
// serve audio, the engine answers NoCapableProviderError — an
// honest 400 naming the endpoint, never a 502, and no
// upstream-failure counter moves.
//
// Transport: the engine calls the provider through
// provider.AudioPoster — PostAudio on *openAIClient, which
// POSTs a raw body (JSON or multipart, forwarded byte-for-byte,
// never decoded and rebuilt) to {base}/v1/audio/<operation>
// with the catalog-resolved auth headers and returns the
// provider's status, Content-Type and body verbatim, mapping a
// non-2xx upstream status to a typed *provider.UpstreamError.
// callAudio below is the adapter between the router and the
// provider shapes (router.AudioRequest → provider.AudioRequest
// via reqWithModelAudio, provider.AudioResponse →
// router.AudioResponse) — the same images precedent as
// reqWithModelImage. There is deliberately NO router-side
// client interface for audio: an interface taking router
// types could never be implemented by a provider client (the
// provider package cannot import the router — the import
// would cycle), which is exactly why the M8.6 slice's
// AudioPassthroughClient stayed unimplemented in production
// and audio answered an honest 400 until this adapter landed.
// The provider interface (provider.AudioPoster) is the
// contract; the router asserts it in supportsAudio/callAudio.
//
// Usage / cost: audio responses carry no token usage block
// (transcription JSON has no usage; speech returns raw audio
// bytes), so the ledger records the attempt with unknown usage
// and unknown cost — never an invented $0, never a guessed
// per-minute/per-character price. The per-request budget gate
// still runs on the bookkeeping request, but an unpriced audio
// model has no local estimate, so the strict gate cannot price
// it (same as a token-ID embeddings input).
//
// Endpoint-kind note: the audio kinds below are defined HERE, in
// the router package, because the provider taxonomy (which owns
// EndpointKind constants) is outside this slice's new-files-only
// scope. The endpoint projection in cascade.go DOES have audio
// cases (endpointForProjection maps the three audio kinds onto
// the autorouter's audio endpoints, which the combo schema
// knows), so AudioPassthrough calls singleAttempt directly — the
// non-chat strategy set ("priority", "fill-first", ...) stands,
// a chat-only combo strategy ("fusion", "pipeline", ...) fails
// closed naming the strategy and the endpoint. The earlier
// singleAudioAttempt mirror of singleAttempt was deleted once
// the cascade fold landed; the exactly-once invariant is pinned
// by the same tests Embed/Rerank carry.
//
// The error taxonomy's 405/501 rule likewise has no audio case
// and treats audio like chat (wireName ""), so a 405/501 from
// the single attempt surfaces as the upstream's own status
// instead of a fallback signal — correct under the replay
// exemption, which forbids falling back anyway.
//
// Deliberately NOT cached, NOT coalesced, NOT stale-served: a
// transcription is the caller's own audio transcribed once, and
// TTS audio is generated per request — serving a cached
// transcript or clip would hand out another request's result and
// hide the upstream call the ledger must record. Guardrails run
// on the speech input text and on the transcription prompt field
// (server side); an audio upload with no prompt field has no
// text to scan.
package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// AudioOperation is one canonical OpenAI audio operation. The
// two alias route names (speech-to-text, text-to-speech) are
// mapped to these by the server handler; the engine never sees
// an alias.
type AudioOperation string

const (
	// AudioSpeech is POST /v1/audio/speech (text-to-speech).
	// Alias route: POST /v1/audio/text-to-speech.
	AudioSpeech AudioOperation = "speech"
	// AudioTranscriptions is POST /v1/audio/transcriptions
	// (speech-to-text). Alias route: POST
	// /v1/audio/speech-to-text.
	AudioTranscriptions AudioOperation = "transcriptions"
	// AudioTranslations is POST /v1/audio/translations
	// (translate audio to English). No alias.
	AudioTranslations AudioOperation = "translations"
)

// audioEndpointKind maps a canonical operation onto the endpoint
// kind the trace, ledger and replay-exemption label carry. The
// kinds are defined here (see the package doc): the provider
// taxonomy is outside this slice's scope.
const (
	// EndpointAudioSpeech is POST /v1/audio/speech.
	EndpointAudioSpeech provider.EndpointKind = "audio_speech"
	// EndpointAudioTranscriptions is POST
	// /v1/audio/transcriptions.
	EndpointAudioTranscriptions provider.EndpointKind = "audio_transcriptions"
	// EndpointAudioTranslations is POST /v1/audio/translations.
	EndpointAudioTranslations provider.EndpointKind = "audio_translations"
)

// audioEndpointKind returns the endpoint kind for one canonical
// operation, and false when the operation is not canonical (an
// alias must already have been mapped by the caller).
func audioEndpointKind(op AudioOperation) (provider.EndpointKind, bool) {
	switch op {
	case AudioSpeech:
		return EndpointAudioSpeech, true
	case AudioTranscriptions:
		return EndpointAudioTranscriptions, true
	case AudioTranslations:
		return EndpointAudioTranslations, true
	default:
		return "", false
	}
}

// audioUpstreamPath is the upstream path segment for one
// canonical operation: {base}/v1/audio/<segment>. It is the path
// the provider transport posts to, and the segment the mock
// clients in tests assert on.
func audioUpstreamPath(op AudioOperation) (string, bool) {
	switch op {
	case AudioSpeech:
		return "audio/speech", true
	case AudioTranscriptions:
		return "audio/transcriptions", true
	case AudioTranslations:
		return "audio/translations", true
	default:
		return "", false
	}
}

// AudioRequest is one audio passthrough request. Body is the
// caller's request body byte-for-byte — JSON for speech,
// multipart/form-data for transcriptions/translations — and is
// forwarded verbatim, never decoded and rebuilt. ContentType is
// the caller's Content-Type header (including the multipart
// boundary), forwarded with the body. Text is bookkeeping only:
// the speech input text, for the guardrail scan and the local
// token estimate; empty for audio uploads, which carry no text.
// It is never sent upstream. CallerID / BudgetUSD /
// BudgetStrict are router-internal, never serialised.
type AudioRequest struct {
	Operation   AudioOperation
	Model       string
	Body        []byte
	ContentType string
	Text        string

	CallerID     string
	BudgetUSD    *float64
	BudgetStrict bool
}

// Validate reports whether req is a well-formed audio request:
// a canonical operation, a model, and a non-empty body. It does
// not validate the model against any provider's catalogue, the
// voice, the response format, or the audio bytes — an unknown
// value fails honestly at the upstream, never here on a guessed
// catalogue.
func (r AudioRequest) Validate() error {
	if _, ok := audioEndpointKind(r.Operation); !ok {
		return fmt.Errorf("router: unknown audio operation %q", string(r.Operation))
	}
	if r.Model == "" {
		return fmt.Errorf("router: audio request model is required")
	}
	if len(r.Body) == 0 {
		return fmt.Errorf("router: audio request body is required")
	}
	return nil
}

// AudioResponse is one audio passthrough response, verbatim from
// the provider: the status, the Content-Type (audio/mpeg for
// speech, application/json for transcriptions/translations) and
// the body bytes, untouched. Model echoes the serving model when
// the transport knows it. There is no Usage: audio responses
// carry no token usage, so cost stays unknown — never invented.
type AudioResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
	Model       string
}

// supportsAudio reports whether client can serve an audio call
// in this slice: it implements provider.AudioPoster AND speaks
// the OpenAI wire. Native-wire clients have no /v1/audio/*
// transport, so the wire check is what keeps them out of the
// selection: only an OpenAI-compatible surface offers these
// endpoints. A client that does not expose its wire format is
// treated as OpenAI wire (the Client contract is
// OpenAI-compatible by definition).
func supportsAudio(client provider.Client) bool {
	if _, ok := client.(provider.AudioPoster); !ok {
		return false
	}
	if wf, ok := client.(wireFormatClient); ok {
		format := wf.WireFormat()
		return format == "" || format == provider.WireFormatOpenAI
	}
	return true
}

// callAudio makes the single upstream audio call for one attempt,
// mirroring callEmbed: adaptive-limiter admission, panic
// isolation (a panicking client fails this attempt, never the
// gateway), and a synthetic ChatResponse carrying the (zero)
// usage so noteSuccess can record the attempt in the ledger with
// unknown cost. A non-2xx response from the transport is an
// upstream error, never a delivered response.
func (e *Engine) callAudio(ctx context.Context, att Attempt, req AudioRequest, endpoint provider.EndpointKind) (aud *AudioResponse, res attemptResult) {
	client, ok := e.clientForAttempt(att)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: unknown provider %q", att.ProviderID)}
	}
	audioClient, ok := client.(provider.AudioPoster)
	if !ok {
		return nil, attemptResult{att: att, err: fmt.Errorf("router: provider %q does not support audio", att.ProviderID)}
	}
	if !e.limiter.TryAcquire(att.ProviderID) {
		e.noteConcurrencyRejected(att.ProviderID)
		return nil, attemptResult{att: att, err: adaptive.ErrLimited}
	}
	defer e.limiter.Release(att.ProviderID)
	start := time.Now()
	defer func() {
		if v := recover(); v != nil {
			e.notePanic("provider_audio")
			aud = nil
			res = attemptResult{att: att, err: NewPanicError("provider_audio", v), latencyMs: time.Since(start).Milliseconds(), called: true}
		}
	}()
	// The adapter: a router AudioRequest becomes the provider
	// AudioRequest (same operation name, same verbatim body —
	// the model is retargeted to this attempt's model, exactly
	// like reqWithModelImage), and the provider response is
	// converted back. Text is router bookkeeping only (the
	// guardrail scan / local estimate input); it never travels
	// upstream, so the provider request carries no Text field.
	presp, err := audioClient.PostAudio(ctx, reqWithModelAudio(req, att))
	if err != nil {
		// Rate-limit learning (M5.13, the callChat precedent):
		// a 429's parsed headers pace the limiter and teach
		// the measured quota fraction (the error already
		// carries the signals — see learnRateLimitError).
		e.learnRateLimitError(att, err)
		return nil, attemptResult{att: att, err: err, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	if presp == nil {
		return nil, attemptResult{att: att, err: errors.New("router: audio provider returned no response"), latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	resp := &AudioResponse{
		StatusCode:  presp.StatusCode,
		ContentType: presp.ContentType,
		Body:        presp.Body,
		Model:       presp.Model,
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Unreachable through the real client (PostAudio maps
		// a non-2xx upstream status to a typed error, never a
		// success) — kept for a foreign AudioPoster that
		// returns one as a response. The error names the
		// status AND carries the upstream body (truncated to
		// 1 KiB and redacted, so key-shaped material in it
		// cannot reach a log line), instead of discarding it.
		body := string(resp.Body)
		if len(body) > 1024 {
			body = body[:1024]
		}
		return nil, attemptResult{att: att, err: &provider.UpstreamError{
			StatusCode: resp.StatusCode,
			Body:       fmt.Sprintf("audio upstream returned status %d: %s", resp.StatusCode, guardrails.Redact(body)),
		}, latencyMs: time.Since(start).Milliseconds(), called: true}
	}
	// The usage is zero by construction: audio responses carry
	// no token usage, so the ledger records the attempt with
	// unknown cost (never an invented $0, never a guessed
	// price). The endpoint is stamped so the ledger row names
	// the audio surface it came from.
	usage := provider.Usage{Endpoint: endpoint}
	// Rate-limit learning (M5.13, the callChat precedent):
	// the response's parsed headers pace the adaptive limiter
	// and record the measured remaining fraction as the
	// engine's quota hint. The synthetic ChatResponse carries
	// the signal so noteSuccess's ledger path sees the same
	// shape chat attempts carry.
	e.learnRateLimit(att, presp.RateLimit)
	return resp, attemptResult{
		att:       att,
		resp:      &provider.ChatResponse{Usage: usage, Model: resp.Model, RateLimit: presp.RateLimit},
		latencyMs: time.Since(start).Milliseconds(),
		called:    true,
	}
}

// reqWithModelAudio returns req addressed to att's model (the
// chain resolves the model per provider; the request itself
// names the model the caller asked for). The body is untouched:
// retargeting the model never rewrites the payload. Text is
// router bookkeeping only and is dropped: the provider request
// carries no Text field (the text never travels upstream).
func reqWithModelAudio(req AudioRequest, att Attempt) provider.AudioRequest {
	return provider.AudioRequest{
		Operation:    provider.AudioOperation(req.Operation),
		Model:        att.Model,
		Body:         req.Body,
		ContentType:  req.ContentType,
		CallerID:     req.CallerID,
		BudgetUSD:    req.BudgetUSD,
		BudgetStrict: req.BudgetStrict,
	}
}

// AudioPassthrough routes one audio request: exactly one upstream
// attempt (see the package doc). The chain is resolved for
// req.Model exactly like Chat resolves its chain, and the
// bookkeeping request carries the speech input text (when any)
// as a user message, so the shared guardrail / budget / ledger
// machinery works unchanged. An audio upload carries no text, so
// its bookkeeping request has no messages: the local estimate
// and the strict gate cannot price it, and the ledger records
// whatever the upstream reported (unknown).
func (e *Engine) AudioPassthrough(ctx context.Context, req AudioRequest) (*AudioResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	endpoint, ok := audioEndpointKind(req.Operation)
	if !ok {
		// Unreachable: Validate already rejected a
		// non-canonical operation. A 500 here is honest if
		// it ever is.
		return nil, fmt.Errorf("router: unknown audio operation %q", string(req.Operation))
	}
	var texts []string
	if req.Text != "" {
		texts = []string{req.Text}
	}
	bookReq := bookkeepingRequest(req.Model, texts, endpoint, req.CallerID, req.BudgetUSD, req.BudgetStrict)
	chain := e.chainFor(req.Model)
	var aud *AudioResponse
	_, _, err := e.singleAttempt(ctx, chain, endpoint, bookReq, supportsAudio,
		func(ctx context.Context, att Attempt) attemptResult {
			var res attemptResult
			aud, res = e.callAudio(ctx, att, req, endpoint)
			return res
		})
	if err != nil {
		return nil, err
	}
	return aud, nil
}
