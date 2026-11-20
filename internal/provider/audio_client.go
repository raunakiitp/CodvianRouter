// Audio passthrough client (M8.6 merge follow-up, finding 1 —
// the provider client the M8.6 slice left missing, which kept
// audio dead in production behind an honest 400).
//
// POST {base}/v1/audio/<operation> on the same OpenAI-compatible
// client that serves chat completions: same auth headers
// (catalog-resolved), same base-URL joining, same error mapping
// as PostImage — only the endpoint name in the taxonomy differs,
// and the response is raw bytes, not JSON.
//
// Endpoint names verified against the OpenAI Audio API
// (2026-11-19, M8.6 review): the live endpoints are speech,
// transcriptions and translations. /v1/audio/speech-to-text and
// /v1/audio/text-to-speech are NOT OpenAI endpoints (OmniRoute
// route names, served by this gateway as aliases — the router
// maps them onto the canonical operations before this client
// ever sees them), so they are not values of AudioOperation.
//
// The request body travels VERBATIM: speech as raw JSON,
// transcriptions/translations as raw multipart/form-data bytes
// (the server sniffs the model/prompt fields out of the
// multipart stream for routing and guardrails, but the bytes
// sent upstream are the exact bytes the client sent — never
// decoded and rebuilt, so binary audio parts cannot be mangled
// by a re-encode). The response body is likewise returned
// verbatim (raw bytes — a TTS clip is audio/mpeg, a transcript
// is JSON; neither is parsed here); only the status and the
// Content-Type are carried alongside it, because the server
// must answer with the provider's own status and media type.
//
// OpenAI-wire ONLY: this method lives on *openAIClient, so the
// native-wire clients (Anthropic, Gemini, Cohere, Bedrock)
// inherit it by embedding — but the engine never calls it there:
// selection requires an OpenAI wire format (see
// router.supportsAudio), because only an OpenAI-compatible
// surface offers /v1/audio/*. Inheriting the method is harmless:
// an accidental call would POST to the native base and fail
// honestly on the upstream 404, exactly once.
//
// DELIBERATE DIVERGENCES from the M8.6 review contract, each
// recorded, not silent:
//   - The contract says the audio client needs "a new transport
//     helper (or a doForEndpoint-variant returning status)" in
//     client.go. That file is owned by another slice in this
//     wave, so the transport lives HERE instead: PostAudio
//     drives c.httpClient directly (same package, same
//     auth/error helpers) and returns the real status +
//     Content-Type. No shared helper was changed, so chat,
//     embeddings and images keep their exact behaviour.
//   - Accept: application/json is set by newRequest and is NOT
//     overridden for the speech op. The review asked to
//     "verify live whether OpenAI ignores it for speech or
//     override it" — no live verification is available in
//     this environment, and an override to audio/* would be
//     an unverified guess either way. Left as-is, recorded
//     here; if a provider ever honours Accept strictly on
//     /v1/audio/speech, override it for that op only.
//   - Response cap: the shared maxResponseBody (10 MiB) is NOT
//     used. doForEndpoint reads via LimitReader(maxResponseBody)
//     and SILENTLY TRUNCATES — for images that fails honestly
//     as malformed JSON, but for audio bytes there is no parse
//     step, so a >10 MiB TTS clip would be delivered as a
//     CORRUPT TRUNCATED FILE with HTTP 200 (M8.6 review
//     finding 1c). PostAudio instead caps at
//     audioMaxResponseBody (64 MiB, the same media-local cap
//     as MiniMax hex in media_verify.go — a long TTS clip is
//     the same class of payload) and DETECTS truncation:
//     it reads cap+1 bytes and refuses with an honest error
//     when the body hits the cap, and fails fast when the
//     upstream Content-Length already exceeds it.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// AudioOperation names one live OpenAI Audio endpoint. There
// are exactly three: speech, transcriptions, translations.
// The OmniRoute alias names (speech-to-text, text-to-speech)
// are mapped to these by the router and are never values of
// this type.
type AudioOperation string

const (
	// AudioSpeech is POST /v1/audio/speech: text-to-speech
	// (JSON in, audio bytes out).
	AudioSpeech AudioOperation = "speech"
	// AudioTranscriptions is POST /v1/audio/transcriptions:
	// speech-to-text (multipart in, JSON {"text"} out).
	AudioTranscriptions AudioOperation = "transcriptions"
	// AudioTranslations is POST /v1/audio/translations:
	// translate audio to English (multipart in, JSON out).
	AudioTranslations AudioOperation = "translations"
)

// AudioPoster is optionally implemented by clients that can
// serve the OpenAI Audio endpoints. The OpenAI-wire client
// implements it; the native-wire clients inherit the method
// but the engine never calls it on them (see the package doc
// above). The router calls it directly: callAudio asserts
// provider.AudioPoster and converts a router AudioRequest into
// this AudioRequest (reqWithModelAudio — the images precedent,
// reqWithModelImage), then converts the response back. There
// is deliberately no router-side client interface: one taking
// router types could never be implemented by a provider client
// (the provider package cannot import the router), which is
// why the M8.6 slice's router-side interface stayed
// unimplemented until this adapter landed.
type AudioPoster interface {
	PostAudio(ctx context.Context, req AudioRequest) (*AudioResponse, error)
}

// AudioRequest is one POST /v1/audio/* request. Body is the
// client's raw request body, verbatim: JSON for speech,
// multipart/form-data bytes for transcriptions/translations.
// ContentType is the client's Content-Type header (carries
// the multipart boundary for uploads; empty means
// application/json). CallerID / BudgetUSD / BudgetStrict are
// router-internal (json:"-"): the caller identity from the
// gateway key, and the per-request budget from
// X-Codvian-Budget — never serialised, so they can never leak
// into a trace, the ledger, or an upstream request body.
type AudioRequest struct {
	Operation   AudioOperation `json:"-"`
	Model       string         `json:"-"`
	Body        []byte         `json:"-"`
	ContentType string         `json:"-"`

	CallerID     string   `json:"-"`
	BudgetUSD    *float64 `json:"-"`
	BudgetStrict bool     `json:"-"`
}

// Validate reports whether req is a well-formed audio request:
// a live operation, a model, and a non-empty body. For speech
// the body must be JSON with a non-empty "input" (the text to
// speak — the server already enforces this; the provider
// re-checks it so a foreign engine cannot smuggle a bad body
// past). For transcriptions/translations the body must be
// multipart/form-data. It does not validate the voice, the
// response format, or the audio bytes against any provider's
// catalogue — an unknown value fails honestly at the upstream,
// never here on a guessed catalogue.
func (r AudioRequest) Validate() error {
	switch r.Operation {
	case AudioSpeech, AudioTranscriptions, AudioTranslations:
	default:
		return fmt.Errorf("provider: audio request operation %q is not a live OpenAI Audio endpoint (speech, transcriptions, translations)", string(r.Operation))
	}
	if r.Model == "" {
		return fmt.Errorf("provider: audio request model is required")
	}
	if len(r.Body) == 0 {
		return fmt.Errorf("provider: audio request body is required")
	}
	if r.Operation == AudioSpeech {
		var decoded struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(r.Body, &decoded); err != nil {
			return fmt.Errorf("provider: audio speech request body is not valid JSON: %w", err)
		}
		if strings.TrimSpace(decoded.Input) == "" {
			return fmt.Errorf("provider: audio speech request input is required")
		}
		return nil
	}
	if r.ContentType == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.ContentType)), "multipart/form-data") {
		return fmt.Errorf("provider: audio %s request must be multipart/form-data", string(r.Operation))
	}
	return nil
}

// AudioResponse is one POST /v1/audio/* response. Body is the
// upstream's response verbatim (audio bytes for speech, JSON
// for transcriptions/translations); StatusCode is the status
// the gateway answers with and ContentType is the provider's
// own media type (audio/mpeg for speech, application/json for
// transcripts) — both preserved, never normalised. Model
// echoes the requested model. There is no Usage: audio
// responses carry no token usage, so cost stays unknown —
// never an estimate, never invented. RateLimit carries the
// upstream rate-limit headers parsed from this response
// (M5.13, the ChatResponse precedent) — nil when the upstream
// sent no rate-limit headers. The router learns from it
// (pacing + quota hint); it is never serialized to callers.
type AudioResponse struct {
	StatusCode  int    `json:"-"`
	ContentType string `json:"-"`
	Body        []byte `json:"-"`
	Model       string `json:"-"`

	RateLimit *adaptive.RateLimitSignals `json:"-"`
}

// audioMaxResponseBody caps one audio upstream response body
// at 64 MiB — the same media-local cap as MiniMax hex in
// media_verify.go, not the shared 10 MiB maxResponseBody (see
// the package doc: the shared cap silently truncates, which
// would corrupt a long TTS clip). 64 MiB holds roughly an
// hour of MP3 at 128 kbps; anything beyond it is refused with
// an honest error instead of being silently truncated into a
// corrupt clip.
const audioMaxResponseBody = mediaMaxResponseBody

// audioURL is POST {base}/v1/audio/<operation> (with the /v1
// rule from joinAPIPath).
func (c *openAIClient) audioURL(op AudioOperation) string {
	return joinAPIPath(c.baseURL, "audio/"+string(op))
}

// PostAudio performs one non-streaming audio call: the raw body
// is POSTed verbatim to the upstream's /v1/audio/<operation>
// and the upstream's status, Content-Type and body are
// returned verbatim. There is no retry inside this method: a
// failed call is the engine's exactly-once attempt. A non-2xx
// upstream status comes back as the typed error from
// mapStatusErrorForEndpoint (UpstreamError / RateLimitError /
// QuotaError), carrying the upstream's status and (redacted,
// truncated) body so the server can answer with the upstream's
// own status instead of a gateway-invented 502. A response
// that hits the 64 MiB cap — or whose Content-Length already
// exceeds it — is refused outright, never delivered truncated.
func (c *openAIClient) PostAudio(ctx context.Context, req AudioRequest) (*AudioResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.audioURL(req.Operation), req.Body)
	if err != nil {
		return nil, err
	}
	// newRequest defaults Content-Type to application/json;
	// uploads travel as multipart/form-data and the boundary
	// in the client's Content-Type must survive verbatim, or
	// the upstream cannot parse the parts.
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	// The transport lives here, not in doForEndpoint: that
	// helper returns (body, header, err) but NOT the status,
	// and it silently truncates past 10 MiB — both fatal for
	// audio (see the package doc). This does the same
	// authenticated Do with the status, the Content-Type and
	// a truncation-detecting read.
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Fail fast when the upstream already declares more than
	// the cap: no point reading 64 MiB to learn the same.
	if resp.ContentLength > audioMaxResponseBody {
		return nil, fmt.Errorf("audio upstream response exceeds %d bytes (Content-Length %d); refusing to return a truncated response", audioMaxResponseBody, resp.ContentLength)
	}
	// Read cap+1 so truncation is DETECTED, not silent:
	// io.LimitReader truncates without an error, so a body
	// that hits the cap and keeps going is refused outright.
	body, err := io.ReadAll(io.LimitReader(resp.Body, audioMaxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading upstream response: %w", err)
	}
	if len(body) > audioMaxResponseBody {
		return nil, fmt.Errorf("audio upstream response exceeds %d bytes; refusing to return a truncated response", audioMaxResponseBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapStatusErrorForEndpoint(resp.StatusCode, resp.Header, body, "audio")
	}
	// Rate-limit headers (M5.13, the chat precedent): the
	// response headers pace the adaptive limiter and teach the
	// measured quota fraction — an upstream that sends no
	// headers teaches nothing (the signal is empty and the
	// router's learn call is a no-op). PostAudio drives the
	// transport directly (not doForEndpoint), so the header
	// surface is resp.Header itself.
	signals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	return &AudioResponse{
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
		Model:       req.Model,

		RateLimit: &signals,
	}, nil
}

// PostAudio on *openAIClient is what the router's audio path
// selects on (via the AudioPoster interface): keep the
// assertion here so a signature drift fails at compile time
// instead of silently unselecting every audio target.
var _ AudioPoster = (*openAIClient)(nil)
