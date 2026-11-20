// Images passthrough client (M8.5).
//
// POST {base}/v1/images/generations and POST {base}/v1/images/edits
// on the same OpenAI-compatible client that serves chat
// completions: same auth headers (catalog-resolved), same
// base-URL joining, same error mapping — only the endpoint name
// in the taxonomy differs.
//
// Endpoint names verified against the OpenAI Images API (2026-11-19):
// the Images API has exactly TWO live endpoints, generations and
// edits. POST /v1/images/variations is retired (edits with a prompt
// replaces it) and POST /v1/images/upscale does NOT exist in the
// OpenAI API — it is not built here, and any client asking for it
// gets the gateway's 404, not a faked endpoint.
//
// The request body travels VERBATIM: generations as raw JSON,
// edits as raw multipart/form-data bytes (the server sniffs the
// model/prompt fields out of the multipart stream for routing
// and guardrails, but the bytes sent upstream are the exact bytes
// the client sent — never decoded and rebuilt, so binary image
// parts cannot be mangled by a re-encode). The response body is
// likewise returned verbatim (json.RawMessage — b64_json stays
// b64_json); only the model and usage block are parsed, for the
// ledger and the telemetry headers.
//
// OpenAI-wire ONLY: this method lives on *openAIClient, so the
// native-wire clients (Anthropic, Gemini, Cohere, Bedrock)
// inherit it by embedding — but the engine never calls it there:
// selection requires an OpenAI wire format (see
// router.supportsImages), because only an OpenAI-compatible
// surface offers /v1/images/*. Inheriting the method is harmless:
// an accidental call would POST to the native base and fail
// honestly on the upstream 404, exactly once.
//
// Limits mirrored from existing code, not invented here:
//   - upstream response bodies are capped at maxResponseBody
//     (10 MiB, provider/client.go): a b64_json image larger than
//     that is truncated by the shared cap and fails honestly as a
//     malformed response. Large-image responses need that cap
//     revisited; see the M8.5 report.
//   - the server caps the inbound body at maxRequestBody (10 MiB):
//     a multi-image edit larger than that is refused 400 before
//     any provider is dialled. Same open question as above.
//
// Streaming (stream:true on generations) is NOT supported: the
// passthrough is a single non-streaming POST and the server
// refuses stream:true with an honest 400.
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
)

// ImageOperation names one live OpenAI Images endpoint. There
// are exactly two: generations and edits. Variations is retired
// and upscale does not exist in the OpenAI API — neither is a
// value of this type.
type ImageOperation string

const (
	// ImageGenerations is POST /v1/images/generations: generate
	// images from scratch from a text prompt (JSON body).
	ImageGenerations ImageOperation = "generations"
	// ImageEdits is POST /v1/images/edits: modify existing images
	// with a new prompt (multipart/form-data body).
	ImageEdits ImageOperation = "edits"
)

// ImagePoster is optionally implemented by clients that can serve
// the OpenAI Images endpoints. The OpenAI-wire client implements
// it; the native-wire clients inherit the method but the engine
// never calls it on them (see the package doc above).
type ImagePoster interface {
	PostImage(ctx context.Context, req ImageRequest) (*ImageResponse, error)
}

// ImageRequest is one POST /v1/images/* request. Body is the
// client's raw request body, verbatim: JSON for generations,
// multipart/form-data bytes for edits. ContentType is the
// client's Content-Type header (carries the multipart boundary
// for edits; empty means application/json). PromptText is the
// prompt the server extracted for guardrails / the local token
// estimate (router-internal, json:"-"): it is never sent
// upstream, the prompt already travels inside Body.
// CallerID / BudgetUSD / BudgetStrict are router-internal
// (json:"-"): the caller identity from the gateway key, and the
// per-request budget from X-Codvian-Budget — never serialised,
// so they can never leak into a trace, the ledger, or an
// upstream request body.
type ImageRequest struct {
	Operation   ImageOperation `json:"-"`
	Model       string         `json:"-"`
	Body        []byte         `json:"-"`
	ContentType string         `json:"-"`

	PromptText string `json:"-"`

	CallerID     string   `json:"-"`
	BudgetUSD    *float64 `json:"-"`
	BudgetStrict bool     `json:"-"`
}

// Validate reports whether req is a well-formed images request:
// a live operation, a model, and a non-empty body. For
// generations the body must be JSON with a non-empty "prompt"
// and must not ask for streaming. For edits the body must be
// multipart/form-data. It does not validate sizes, quality,
// output formats or any other field against any provider's
// catalogue — an unknown value fails honestly at the upstream,
// never here on a guessed catalogue.
func (r ImageRequest) Validate() error {
	switch r.Operation {
	case ImageGenerations, ImageEdits:
	default:
		return fmt.Errorf("provider: images request operation %q is not a live OpenAI Images endpoint (generations, edits)", string(r.Operation))
	}
	if r.Model == "" {
		return fmt.Errorf("provider: images request model is required")
	}
	if len(r.Body) == 0 {
		return fmt.Errorf("provider: images request body is required")
	}
	if r.Operation == ImageGenerations {
		var decoded struct {
			Prompt json.RawMessage `json:"prompt"`
			Stream *bool           `json:"stream"`
		}
		if err := json.Unmarshal(r.Body, &decoded); err != nil {
			return fmt.Errorf("provider: images generations request body is not valid JSON: %w", err)
		}
		var prompt string
		if len(decoded.Prompt) > 0 {
			_ = json.Unmarshal(decoded.Prompt, &prompt)
		}
		if prompt == "" {
			return fmt.Errorf("provider: images generations request prompt is required")
		}
		if decoded.Stream != nil && *decoded.Stream {
			return fmt.Errorf("provider: images generations streaming (stream:true) is not supported by this gateway")
		}
		return nil
	}
	if r.ContentType == "" || !strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.ContentType)), "multipart/form-data") {
		return fmt.Errorf("provider: images edits request must be multipart/form-data")
	}
	return nil
}

// ImageResponse is one POST /v1/images/* response. Body is the
// upstream's response verbatim; StatusCode is the status the
// gateway answers with (200: doForEndpoint only returns on 2xx).
// Model and Usage are parsed opportunistically from the body
// for the ledger and telemetry headers; when the upstream
// reports no usage block they stay zero — unknown, never an
// estimate, never invented. RateLimit carries the upstream
// rate-limit headers parsed from this response (M5.13, the
// ChatResponse precedent) — nil when the upstream sent no
// rate-limit headers. The router learns from it (pacing +
// quota hint); it is never serialized to callers.
type ImageResponse struct {
	StatusCode int             `json:"-"`
	Body       json.RawMessage `json:"-"`
	Model      string          `json:"-"`
	Usage      Usage           `json:"-"`

	RateLimit *adaptive.RateLimitSignals `json:"-"`
}

// imageWireResponse is the decode shape for an images response:
// the model plus the usage block. OpenAI image usage reports
// input_tokens / output_tokens / total_tokens (image tokens),
// not the prompt_tokens / completion_tokens of chat usage; the
// mapping below folds input_tokens onto PromptTokens and
// output_tokens onto CompletionTokens so the shared ledger
// machinery records the reported counts unchanged. Some
// OpenAI-compatible providers report prompt_tokens instead —
// both spellings are accepted, input_tokens wins when both are
// present. input_tokens_details (text/image split) is not
// carried: provider.Usage has no field for it, and inventing
// one would be a guessed shape.
type imageWireResponse struct {
	Model string `json:"model"`
	Usage struct {
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		TotalTokens      int `json:"total_tokens"`
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (w imageWireResponse) usage() Usage {
	u := Usage{Endpoint: EndpointImages}
	// The input_tokens spelling wins only when it actually
	// carries counts: total_tokens alone must not select it,
	// or a prompt_tokens-only report would map to zeros.
	if w.Usage.InputTokens != 0 || w.Usage.OutputTokens != 0 {
		u.PromptTokens = w.Usage.InputTokens
		u.CompletionTokens = w.Usage.OutputTokens
		u.TotalTokens = w.Usage.TotalTokens
	} else {
		u.PromptTokens = w.Usage.PromptTokens
		u.CompletionTokens = w.Usage.CompletionTokens
		u.TotalTokens = w.Usage.TotalTokens
	}
	return u
}

// imagesURL is POST {base}/v1/images/<operation> (with the /v1
// rule from joinAPIPath).
func (c *openAIClient) imagesURL(op ImageOperation) string {
	return joinAPIPath(c.baseURL, "images/"+string(op))
}

// PostImage performs one non-streaming images call: the raw body
// is POSTed verbatim to the upstream's /v1/images/<operation>
// and the upstream's response body is returned verbatim. There
// is no retry inside this method: a failed call is the engine's
// exactly-once attempt. A non-2xx upstream status comes back as
// the typed error from doForEndpoint (UpstreamError /
// RateLimitError / QuotaError), carrying the upstream's status
// and (redacted, truncated) body so the server can answer with
// the upstream's own status and error body instead of a
// gateway-invented 502.
func (c *openAIClient) PostImage(ctx context.Context, req ImageRequest) (*ImageResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.imagesURL(req.Operation), req.Body)
	if err != nil {
		return nil, err
	}
	// newRequest defaults Content-Type to application/json;
	// edits travel as multipart/form-data and the boundary in
	// the client's Content-Type must survive verbatim, or the
	// upstream cannot parse the parts.
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	body, header, err := c.doForEndpoint(httpReq, "images")
	if err != nil {
		return nil, err
	}
	var decoded imageWireResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		// A 200 whose body cannot be parsed is an upstream
		// failure, never a success: it must carry 502 (a
		// truncated b64_json body hitting the 10 MiB
		// maxResponseBody cap takes exactly this path), so
		// the server answers 502 instead of echoing a 200
		// with a plain-text error body.
		return nil, &UpstreamError{StatusCode: http.StatusBadGateway, Retryable: false, Body: "malformed images response: " + err.Error()}
	}
	model := decoded.Model
	if model == "" {
		model = req.Model
	}
	// Rate-limit headers (M5.13, the chat precedent): the
	// response headers pace the adaptive limiter and teach the
	// measured quota fraction — an upstream that sends no
	// headers teaches nothing (the signal is empty and the
	// router's learn call is a no-op).
	signals := adaptive.ParseRateLimitHeaders(header, time.Now())
	return &ImageResponse{
		StatusCode: http.StatusOK,
		Body:       json.RawMessage(body),
		Model:      model,
		Usage:      decoded.usage(),

		RateLimit: &signals,
	}, nil
}
