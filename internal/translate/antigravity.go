package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Pair: openai <-> antigravity (Google Antigravity / Cloud Code Assist).
//
// SHAPE PROVENANCE: Google Antigravity's backend is the Cloud Code
// Assist API at cloudcode-pa.googleapis.com
// (v1internal:generateContent and v1internal:streamGenerateContent?alt=sse).
// It is not publicly documented as a stable JSON contract; the shapes
// below are the protocol facts verifiable from open-source clients
// that exercise the surface (notably usamashehab/antigravity-proxy,
// whose README documents the envelope and whose source shows it
// verbatim):
//
//	{
//	  "project": "<code-assist project id>",
//	  "model": "<backend model name>",
//	  "request": { ...native Gemini generateContent body... },
//	  "requestType": "agent",
//	  "userAgent": "antigravity",
//	  "requestId": "agent-<hex>"
//	}
//
// Responses are wrapped the same way: {"response": {...Gemini
// GenerateContentResponse...}} (stream payloads tolerate a bare
// Gemini response too, as the reference client's unwrap does).
//
// Because the inner `request` IS the native Gemini shape, this pair
// REUSES the existing Gemini wire logic rather than duplicating it:
// wire.ConvertGeminiRequest builds the inner body, and
// wire.ConvertGeminiResponse / wire.TranslateGeminiStream handle the
// inner response/stream — the pair only owns the envelope.
//
// RECORDED DIVERGENCES: the surface is not officially documented;
// sampling maps through the Gemini shape (generationConfig), which is
// the documented behaviour of the inner request. Model names differ
// from the OpenAI-facing names (e.g. gemini-3.1-pro ->
// gemini-3.1-pro-low); the caller supplies the backend model name and
// this converter passes it through untouched.
// ---------------------------------------------------------------------------

// AntigravityEnvelope is the Cloud Code Assist request wrapper. The
// inner Request is a native Gemini generateContent body (built by
// wire.ConvertGeminiRequest).
type AntigravityEnvelope struct {
	Project     string              `json:"project,omitempty"`
	Model       string              `json:"model,omitempty"`
	Request     *wire.GeminiRequest `json:"request,omitempty"`
	RequestType string              `json:"requestType,omitempty"` // "agent"
	UserAgent   string              `json:"userAgent,omitempty"`   // "antigravity"
	RequestID   string              `json:"requestId,omitempty"`   // "agent-<hex>"
}

// AntigravityResponse is the Cloud Code Assist response wrapper: the
// Gemini GenerateContentResponse under "response".
type AntigravityResponse struct {
	Response *wire.GeminiResponse `json:"response,omitempty"`
}

// Antigravity request constants, verbatim from the observed surface.
const (
	AntigravityRequestType = "agent"
	AntigravityUserAgent   = "antigravity"
)

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Antigravity envelope.
// ---------------------------------------------------------------------------

// ConvertAntigravityRequest converts an OpenAI chat-completions request
// into the Antigravity Cloud Code Assist envelope, reusing
// wire.ConvertGeminiRequest for the inner Gemini body. project is the
// Cloud Code Assist project id (discovered via v1internal:loadCodeAssist
// by the transport layer); when empty a random requestId is generated
// ("agent-<hex>"), matching the observed client behaviour.
func ConvertAntigravityRequest(req wire.ChatCompletionRequest, project string) (*AntigravityEnvelope, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	inner, err := wire.ConvertGeminiRequest(req)
	if err != nil {
		return nil, err
	}
	id, err := newRequestID()
	if err != nil {
		return nil, fmt.Errorf("translate: generate requestId: %w", err)
	}
	return &AntigravityEnvelope{
		Project:     project,
		Model:       req.Model,
		Request:     inner,
		RequestType: AntigravityRequestType,
		UserAgent:   AntigravityUserAgent,
		RequestID:   id,
	}, nil
}

// newRequestID mints a requestId in the observed "agent-<hex>" form.
func newRequestID() (string, error) {
	hex, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	hex = strings.ReplaceAll(hex, "-", "")
	return "agent-" + hex, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Antigravity envelope -> OpenAI.
// ---------------------------------------------------------------------------

// ConvertAntigravityResponse unwraps the {"response": ...} envelope and
// reuses wire.ConvertGeminiResponse for the inner Gemini response.
// Responses without a "response" body return *UnrepresentableError.
func ConvertAntigravityResponse(resp *AntigravityResponse) (*wire.ChatCompletionResponse, error) {
	if resp == nil || resp.Response == nil {
		return nil, unrep("antigravity response has no inner Gemini response")
	}
	return wire.ConvertGeminiResponse(resp.Response)
}

// ---------------------------------------------------------------------------
// Streaming: Antigravity SSE -> OpenAI chat-completion chunks.
//
// The stream endpoint (v1internal:streamGenerateContent?alt=sse) sends
// one {"response": {...}} envelope per SSE data payload (a bare Gemini
// response is tolerated, mirroring the reference client's unwrap). The
// pair unwraps the envelopes and delegates to
// wire.TranslateGeminiStream, so Gemini thought parts, tool calls,
// usage and errors translate exactly as on the native Gemini wire.
// ---------------------------------------------------------------------------

// TranslateAntigravityStream reads an Antigravity SSE stream, unwraps
// the {"response": ...} envelopes, and emits OpenAI
// chat.completion.chunk values via emit, in order. It returns the
// reassembled result when the stream ends.
func TranslateAntigravityStream(r io.Reader, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	pr, pw := io.Pipe()
	go func() {
		err := readSSE(r, func(ev sseEvent) error {
			data := strings.TrimSpace(ev.data)
			if data == "" || data == "[DONE]" {
				return nil
			}
			payload := data
			// Unwrap {"response": {...}}; tolerate a bare Gemini
			// payload (the reference client's unwrap does both).
			var probe struct {
				Response json.RawMessage `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &probe); err == nil && len(probe.Response) > 0 {
				payload = string(probe.Response)
			}
			_, werr := fmt.Fprintf(pw, "data: %s\n\n", payload)
			return werr
		})
		// CloseWithError(nil) makes the reader see EOF; an error
		// propagates to the inner translator.
		pw.CloseWithError(err)
	}()
	return wire.TranslateGeminiStream(pr, emit)
}
