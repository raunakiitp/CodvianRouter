// Package provider — Gemini native-wire client.
//
// geminiClient speaks the Google AI Studio Gemini API
// (POST {base}/models/{model}:generateContent, and
// :streamGenerateContent?alt=sse for streaming) for providers whose
// catalog wire format is Gemini. It converts between the gateway's
// OpenAI-shaped ChatRequest/ChatResponse and the Gemini wire format
// using internal/wire (a pure conversion library), and resolves auth
// per (provider, endpoint) through the catalog — exactly the headers
// ResolveAuth returns (x-goog-api-key; never an Authorization header
// or a ?key= query parameter, which would leak into logs).
//
// The base URL is the API root, e.g.
// https://generativelanguage.googleapis.com/v1beta (the catalog
// entry's BaseURLs[0]). The model path is built as
// {base}/models/{model}:generateContent — no "/v1" is appended, so a
// mock base URL works the same way as the real one. A model ID with a
// "models/" prefix (as the real ListModels returns) is stripped.
//
// Error mapping (failover depends on it): a 429 / RESOURCE_EXHAUSTED
// becomes *RateLimitError (the router honours Retry-After and fails
// over immediately); quota/billing failures become *QuotaError; other
// non-2xx become *UpstreamError with Retryable set from the wire
// error (UNAVAILABLE / 5xx retryable). Mid-stream error events
// surface the same way via wire.MapGeminiStreamError.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// geminiClient is the Client implementation for Gemini-wire upstreams.
// It embeds *openAIClient for the shared HTTP plumbing (auth headers,
// request building, response reading); only the request/response
// shapes differ.
type geminiClient struct {
	*openAIClient
}

// geminiModelName strips the "models/" prefix the real ListModels
// returns ("models/gemini-2.0-flash" -> "gemini-2.0-flash").
func geminiModelName(model string) string {
	return strings.TrimPrefix(model, "models/")
}

// generateURL is POST {base}/models/{model}:generateContent.
func (c *geminiClient) generateURL(model string) string {
	return strings.TrimRight(c.baseURL, "/") + "/models/" + geminiModelName(model) + ":generateContent"
}

// streamURL is POST {base}/models/{model}:streamGenerateContent?alt=sse.
func (c *geminiClient) streamURL(model string) string {
	return strings.TrimRight(c.baseURL, "/") + "/models/" + geminiModelName(model) + ":streamGenerateContent?alt=sse"
}

// modelsListURL is GET {base}/models (the real Gemini API root is
// .../v1beta, so this is GET .../v1beta/models).
func (c *geminiClient) modelsListURL() string {
	return strings.TrimRight(c.baseURL, "/") + "/models"
}

// mapGeminiError converts a *wire.GeminiError (or a
// *wire.UnrepresentableError) into this package's typed errors. A
// rate limit becomes *RateLimitError (router fails over on it);
// billing/quota failures become *QuotaError (not transient); anything
// else becomes *UpstreamError preserving retryability. Unknown errors
// pass through unchanged.
func mapGeminiError(err error) error {
	if err == nil {
		return nil
	}
	var ge *wire.GeminiError
	if errors.As(err, &ge) {
		if ge.IsRateLimit() {
			return &RateLimitError{RetryAfter: ge.RetryAfter, Body: ge.Message}
		}
		if ge.Status == "PERMISSION_DENIED" && isQuotaBody(ge.Message) ||
			ge.StatusCode == http.StatusPaymentRequired ||
			(ge.StatusCode == http.StatusBadRequest && isQuotaBody(ge.Message)) {
			return &QuotaError{Message: ge.Message, StatusCode: ge.StatusCode, Body: ge.Error()}
		}
		status := ge.StatusCode
		if status == 0 {
			// Mid-stream error event: the HTTP status was already
			// 200; report it as a bad-gateway-class failure.
			status = http.StatusBadGateway
		}
		return &UpstreamError{StatusCode: status, Retryable: ge.Retryable, Body: ge.Error()}
	}
	var ue *wire.UnrepresentableError
	if errors.As(err, &ue) {
		// The request cannot be represented on this wire at all
		// (e.g. an http(s) image URL). It is not transient and not
		// the upstream's fault; failing over to another provider is
		// the correct recovery, so surface it as a non-retryable
		// upstream error carrying the reason.
		return &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: ue.Error()}
	}
	return err
}

func (c *geminiClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	gemReq, err := wire.ConvertGeminiRequest(toWireRequest(req))
	if err != nil {
		return nil, mapGeminiError(err)
	}
	payload, err := json.Marshal(gemReq)
	if err != nil {
		return nil, fmt.Errorf("encoding gemini request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.generateURL(req.Model), payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading gemini response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapGeminiError(wire.MapGeminiError(resp.StatusCode, resp.Header, body))
	}
	var decoded wire.GeminiResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed gemini response: " + err.Error()}
	}
	converted, err := wire.ConvertGeminiResponse(&decoded)
	if err != nil {
		return nil, mapGeminiError(err)
	}
	if converted.Model == "" {
		converted.Model = req.Model
	}
	return fromWireResponse(converted)
}

// ChatStream is the text-only streaming entry point; see ChatStreamChunks.
func (c *geminiClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	return c.ChatStreamChunks(ctx, req, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		return onDelta(chunk.Content)
	})
}

// ChatStreamChunks performs a streaming generateContent request,
// translating the Gemini SSE stream into OpenAI-shaped chunks via
// wire.TranslateGeminiStream. The returned ChatResponse holds the
// reassembled result (content, completed tool calls, finish reason,
// usage).
func (c *geminiClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	gemReq, err := wire.ConvertGeminiRequest(toWireRequest(req))
	if err != nil {
		return nil, mapGeminiError(err)
	}
	payload, err := json.Marshal(gemReq)
	if err != nil {
		return nil, fmt.Errorf("encoding gemini request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.streamURL(req.Model), payload)
	if err != nil {
		return nil, err
	}
	// No client-level timeout for streaming (context bounds it).
	streamClient := *c.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapGeminiError(wire.MapGeminiError(resp.StatusCode, resp.Header, body))
	}
	emit := func(wc wire.ChatChunk) error {
		chunk := StreamChunk{}
		if len(wc.Choices) > 0 {
			d := wc.Choices[0].Delta
			chunk.Content = d.Content
			if wc.Choices[0].FinishReason != nil {
				chunk.FinishReason = *wc.Choices[0].FinishReason
			}
			for _, tc := range d.ToolCalls {
				chunk.ToolCalls = append(chunk.ToolCalls, ChunkToolCall{
					Index:     tc.Index,
					ID:        tc.ID,
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				})
			}
		}
		if wc.Usage != nil {
			chunk.Usage = &Usage{
				PromptTokens:     wc.Usage.PromptTokens,
				CompletionTokens: wc.Usage.CompletionTokens,
				TotalTokens:      wc.Usage.TotalTokens,
			}
		}
		return onChunk(chunk)
	}
	res, err := wire.TranslateGeminiStream(resp.Body, emit)
	if err != nil {
		return nil, mapGeminiError(err)
	}
	if res.Model == "" {
		res.Model = req.Model
	}
	return &ChatResponse{
		Content:      res.Content,
		Usage:        Usage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens},
		Model:        res.Model,
		ToolCalls:    res.ToolCalls,
		FinishReason: res.FinishReason,
	}, nil
}

// geminiModelsListResponse is the {"models": [{"name": "models/..."}]}
// shape the real Gemini ListModels endpoint returns.
type geminiModelsListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// ListModels returns the model IDs from GET {base}/models. The real
// API returns names as "models/gemini-2.0-flash"; the "models/"
// prefix is stripped so IDs match what Chat takes.
func (c *geminiClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, c.modelsListURL(), nil)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	var decoded geminiModelsListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.Models))
	for _, m := range decoded.Models {
		if m.Name != "" {
			ids = append(ids, geminiModelName(m.Name))
		}
	}
	return ids, nil
}

// TestKey verifies the API key with a tiny ListModels call.
func (c *geminiClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

// Ensure geminiClient satisfies the client contracts at compile time.
var (
	_ Client        = (*geminiClient)(nil)
	_ ChunkStreamer = (*geminiClient)(nil)
)
