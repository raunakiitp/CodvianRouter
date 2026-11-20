// Media verify-first passthrough (M8.20, CHECKLIST 20x).
//
// Four media surfaces, each built ONLY because a provider already in
// the catalog documents a real API for it (verify-first, Q8):
//
//   - Search: Perplexity Search API, POST https://api.perplexity.ai/search
//     (https://docs.perplexity.ai/docs/search/overview). The catalog
//     entry "perplexity" already records "Search POST /search" and
//     carries base https://api.perplexity.ai. The upstream takes no
//     model; the gateway request takes none either.
//   - OCR: Mistral OCR API, POST https://api.mistral.ai/v1/ocr, model
//     mistral-ocr-latest
//     (https://docs.mistral.ai/capabilities/document/#tag/ocr/operation/ocr_v1_ocr_post;
//     the endpoint is also listed in Mistral's own batch-endpoint
//     reference, docs.mistral.ai/api/endpoint/batch). Catalog
//     "mistral" base is https://api.mistral.ai/v1.
//   - Video: Runway Dev API, POST https://api.dev.runwayml.com/v1/text_to_video
//   - GET /v1/tasks/{id} (https://docs.dev.runwayml.com/guides/using-the-api/;
//     task polling https://docs.dev.runwayml.com/api-details/sdks/).
//     Catalog "runwayml" base is https://api.dev.runwayml.com/v1.
//     Runway requires the X-Runway-Version: 2024-11-06 header on
//     every request; the handler supplies it (see
//     internal/server/media_verify.go).
//   - Music: MiniMax Music Generation API,
//     POST https://api.minimax.io/v1/music_generation, model music-3.0
//     (https://platform.minimax.io/docs/api-reference/music-generation).
//     Catalog "minimax" base is https://api.minimax.io/v1 (triage fill).
//     RESTRICTION (same docs page): since 2026-08-20 the paid Music
//     Generation / Lyrics Generation APIs are closed to NEW users;
//     existing paying users keep access, and the free model IDs
//     (music-3.0-free, music-2.6-free, music-cover-free) are
//     discontinued. The gateway routes a caller who holds an entitled
//     MiniMax key; it cannot conjure access for anyone else.
//
// Deliberately NOT built (verified dead or unverifiable):
//   - OpenAI Sora Videos API (POST /v1/videos): OpenAI's own docs
//     (https://developers.openai.com/api/docs/guides/video-generation)
//     say "The Sora 2 models and Videos API were shut down on
//     September 24, 2026 and are no longer available."
//   - Google Veo via the Gemini API (veo-3.1-generate-preview
//     :predictLongRunning): Google AI Studio docs say the Veo 3.1
//     preview models in the Gemini API were shut down on October 22,
//     2026; the GA models live behind Vertex / the Gemini Enterprise
//     Agent Platform, not the catalog's Gemini API-key base.
//   - Suno / Udio music: neither documents a public self-serve API
//     (Suno: invite-only partner beta at platform.suno.com, no public
//     docs; Udio: no public API, help center confirms). The catalog
//     entries "udio" (and "suno", which does not exist in the catalog
//     at all) carry no base URL, so the router could not route to
//     them anyway. Third-party Suno/Udio wrappers are not providers
//     in the catalog and are not used.
//   - ElevenLabs Music: a documented API exists, but the catalog
//     entry "elevenlabs" has BaseURLs nil, so the router cannot route
//     to it — excluded by the slice rule (provider must already be
//     routable: key + base URL in the catalog). Same for "replicate",
//     "fal-ai" and "stability-ai" (all BaseURLs nil).
//   - Tavily / Brave / Exa / Firecrawl-style search providers: NOT
//     added to the catalog (coordinator decision Q8 — they are not
//     LLM providers). Search is served by Perplexity, an LLM provider
//     already in the catalog.
//
// Design: one generic engine-side passthrough. The handler validates
// the gateway request, builds a MediaPassthroughRequest naming the
// pinned provider + the provider-native upstream path, and hands it
// to Engine.MediaPassthrough (internal/router/media_verify.go),
// which runs it through the same single-attempt machinery as
// embeddings (chain, breaker, cooldown, limiter, budget gate,
// ledger, trace — exactly one upstream attempt, never replayed: a
// video/music generation starts a paid job, so a replay would
// double-charge). The request body travels VERBATIM: the gateway
// never rebuilds it, so provider-native fields the gateway does not
// know (ratio, duration, audio_setting, include_image_base64, ...)
// reach the upstream untouched, and the upstream validates them.
// The response is returned VERBATIM too (status + body): MiniMax
// reports failures inside a 200 envelope (base_resp.status_code),
// Runway reports a task object, Perplexity reports ranked results —
// none of that is reinterpreted here.
//
// No price is recorded for any of these calls: none of the four
// models has a price-book entry, so cost stays unknown (never an
// invented number). The ledger may carry a local token estimate for
// the bookkeeping text (query / prompt), flagged LocalEstimate —
// the same fallback embeddings gets when the upstream reports no
// usage. That estimate is a token count, never a price.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Endpoint kinds for the M8.20 media surfaces. They are declared
// here (not in taxonomy.go, which this slice does not own) because
// EndpointKind is a plain string type: a new file can add kinds
// without touching the shared taxonomy. They are known to
// endpointForProjection (internal/router/cascade.go, M8.20
// mapping — the single shared-file touch of this slice, 8 lines)
// so the default "priority" strategy stands on media calls.
const (
	// EndpointSearch is POST /v1/search (Perplexity Search API).
	EndpointSearch EndpointKind = "search"
	// EndpointOCR is POST /v1/ocr (Mistral OCR API).
	EndpointOCR EndpointKind = "ocr"
	// EndpointVideo is POST /v1/videos/generations + GET /v1/videos/{id}
	// (Runway Dev text-to-video task create + task retrieve).
	EndpointVideo EndpointKind = "video"
	// EndpointMusic is POST /v1/music/generations (MiniMax Music
	// Generation API).
	EndpointMusic EndpointKind = "music"
)

// MediaPassthroughClient is optionally implemented by clients that
// can serve one raw media passthrough call: POST or GET an
// upstream-native path with a verbatim body, returning the upstream
// status + body verbatim. The OpenAI-wire client implements it; the
// native-wire clients inherit it by embedding *openAIClient, but the
// engine never calls it on them: media chains are provider-pinned
// (see internal/router/media_verify.go), and every pinned provider
// is OpenAI-wire in the catalog.
type MediaPassthroughClient interface {
	MediaPassthrough(ctx context.Context, req MediaPassthroughRequest) (*MediaPassthroughResponse, error)
}

// MediaPassthroughRequest is one raw upstream media call. ProviderID
// is the catalog provider the handler pinned for the endpoint
// (perplexity / mistral / runwayml / minimax); Model is the
// caller-supplied model for the surfaces that take one (OCR, video,
// music) or the provider id itself for the surfaces that take none
// (search: "perplexity"; video task retrieve: "runwayml") — it is
// the routing / ledger label and is NEVER sent upstream on its own:
// the upstream body is Body, verbatim. Path is the provider-native
// path RELATIVE TO THE CLIENT'S BASE URL — "search" (Perplexity,
// whose base has no /v1), "ocr" (Mistral), "text_to_video" /
// "tasks/{id}" (Runway), "music_generation" (MiniMax). The base
// already ends in /v1 for every provider except Perplexity, so a
// path with a leading /v1 would double it; joinRawPath joins
// without inserting anything. Method is "POST" or "GET". ExtraHeaders are per-call upstream headers the
// handler supplies from the provider's own docs (Runway's
// X-Runway-Version); Authorization / Content-Type are never
// overridable through them. CallerID / BudgetUSD / BudgetStrict /
// BookkeepingTexts are router-internal (json:"-"): the caller
// identity, the per-request budget, and the text the guardrail scan
// and token estimate run on (search query, video promptText, music
// prompt + lyrics; empty for OCR, whose document is not text).
type MediaPassthroughRequest struct {
	ProviderID   string
	Model        string
	Endpoint     EndpointKind
	Method       string
	Path         string
	Body         json.RawMessage
	ExtraHeaders map[string]string

	CallerID         string   `json:"-"`
	BudgetUSD        *float64 `json:"-"`
	BudgetStrict     bool     `json:"-"`
	BookkeepingTexts []string `json:"-"`
}

// Validate reports whether req is a well-formed passthrough request:
// a pinned provider, an endpoint, a path, an explicit POST/GET
// method, and a body on POST. It does not validate the body against
// any provider's schema — the upstream validates it honestly.
func (r MediaPassthroughRequest) Validate() error {
	if r.ProviderID == "" {
		return fmt.Errorf("provider: media passthrough provider is required")
	}
	if r.Endpoint == "" {
		return fmt.Errorf("provider: media passthrough endpoint is required")
	}
	if r.Path == "" {
		return fmt.Errorf("provider: media passthrough path is required")
	}
	switch r.Method {
	case http.MethodPost:
		if len(r.Body) == 0 || string(r.Body) == "null" {
			return fmt.Errorf("provider: media passthrough POST body is required")
		}
	case http.MethodGet:
	default:
		return fmt.Errorf("provider: media passthrough method must be POST or GET, got %q", r.Method)
	}
	return nil
}

// MediaPassthroughResponse is one upstream media response, verbatim:
// the upstream HTTP status and the raw body. Status is preserved
// (not normalised to 200) because a 201 create and a 200 retrieve
// are different answers.
type MediaPassthroughResponse struct {
	Status int
	Body   json.RawMessage
}

// joinRawPath joins a client base URL and a provider-native path
// WITHOUT the /v1 rule of joinAPIPath: Perplexity's search path is
// "search" on a base with no /v1 (inserting one would 404), while
// every other media path is already relative to a base that ends in
// /v1 (inserting one would double it). Callers pass the path
// relative to the base: "search", "ocr", "text_to_video",
// "tasks/{id}", "music_generation".
func joinRawPath(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimPrefix(path, "/")
}

// mediaMaxResponseBody caps one media upstream response body at
// 64 MiB. It is deliberately media-local and larger than the
// shared maxResponseBody (10 MiB, internal/provider/client.go),
// which this slice must not raise: MiniMax music defaults to
// output_format "hex" (platform.minimax.io/docs/api-reference/
// music-generation), so the default response carries the whole
// song as hex-encoded audio — the docs' own example is 25,364 ms
// with music_size 813,651 bytes (~0.8 MB binary, ~1.6 MB hex).
// Scaling linearly, a 5-minute song at the default 256 kbps is
// ~9.6 MB binary, ~19 MB hex: under the shared 10 MiB cap a song
// longer than ~2.7 minutes would be cut mid-JSON and handed to
// the client as a valid 200. 64 MiB holds the hex of roughly a
// 17-minute song at 256 kbps, and anything beyond it is refused
// with an honest error (see MediaPassthrough) instead of being
// silently truncated into corrupt JSON.
const mediaMaxResponseBody = 64 * 1024 * 1024

// MediaPassthrough performs one raw media call: the verbatim body
// POSTed (or a GET with no body) to base+Path, with the client's
// catalog-resolved auth, returning the upstream status + body
// verbatim. It never retries internally: the engine's single
// attempt is the only call. Non-2xx statuses become the same
// mapped UpstreamError Embed/Rerank get (mapStatusErrorForEndpoint,
// with the same key-material redaction); the endpoint name is the
// media kind, so a 405/501 is NOT retryable here — the taxonomy
// only treats responses/embeddings/rerank 405/501 as "target lacks
// the endpoint", and a single-attempt surface never retries anyway.
func (c *openAIClient) MediaPassthrough(ctx context.Context, req MediaPassthroughRequest) (*MediaPassthroughResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	var payload []byte
	if req.Body != nil {
		payload = []byte(req.Body)
	}
	httpReq, err := c.newRequest(ctx, req.Method, joinRawPath(c.baseURL, req.Path), payload)
	if err != nil {
		return nil, err
	}
	// Per-call upstream headers from the provider's own docs
	// (Runway X-Runway-Version). Auth and body headers are never
	// overridable here: newRequest already set them.
	for k, v := range req.ExtraHeaders {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "content-type" || lk == "accept" {
			continue
		}
		httpReq.Header.Set(k, v)
	}
	// No client-level timeout for media (M8.20 F3): a long
	// generation — notably MiniMax music, whose per-endpoint
	// server timeout (10 minutes by default) exceeds this
	// client's 120 s http.Client.Timeout — must not be cut off
	// by the client while the server still allows it. The
	// request context — bounded by the server's per-endpoint
	// timeout — still bounds the call, exactly like the
	// streaming path (client.go: the stream client runs with
	// Timeout 0 for the same reason). Non-music surfaces keep
	// their 120 s bound through that context, so their
	// behaviour is unchanged.
	mediaClient := *c.httpClient
	mediaClient.Timeout = 0
	resp, err := mediaClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Read with the media-local cap (mediaMaxResponseBody), one
	// byte past it so truncation is DETECTED, not silent:
	// io.LimitReader truncates without an error, so a body that
	// hits the cap and keeps going is refused outright — a
	// truncated MiniMax hex payload would otherwise be returned
	// verbatim as a corrupt 200 (see mediaMaxResponseBody).
	body, err := io.ReadAll(io.LimitReader(resp.Body, mediaMaxResponseBody+1))
	if err != nil {
		return nil, fmt.Errorf("reading upstream response: %w", err)
	}
	if len(body) > mediaMaxResponseBody {
		return nil, fmt.Errorf("media upstream response exceeds %d bytes; refusing to return a truncated response", mediaMaxResponseBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapStatusErrorForEndpoint(resp.StatusCode, resp.Header, body, string(req.Endpoint))
	}
	return &MediaPassthroughResponse{Status: resp.StatusCode, Body: json.RawMessage(body)}, nil
}
