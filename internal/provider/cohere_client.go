// Package provider — Cohere native-wire client.
//
// cohereClient speaks the Cohere Chat API v2 (POST {base}/v2/chat,
// with "stream": true for SSE streaming) for providers whose catalog
// wire format is Cohere. It converts between the gateway's
// OpenAI-shaped ChatRequest/ChatResponse and the Cohere wire format
// using internal/wire (a pure conversion library), and resolves auth
// per (provider, endpoint) through the catalog — exactly the headers
// ResolveAuth returns (Authorization: Bearer on the native surface),
// never a hard-coded header.
//
// The base URL is the API root, e.g. https://api.cohere.com (the
// catalog entry's native endpoint BaseURL). The chat path is built as
// {base}/v2/chat — no "/v1" is appended, so a mock base URL works the
// same way as the real one. A provider added on Cohere's
// OpenAI-compatible shim (base .../compatibility/v1) is NOT this
// client: the catalog resolves that surface to the OpenAI wire, and
// the OpenAI client serves it.
//
// Error mapping (failover depends on it): a 429 becomes
// *RateLimitError (the router honours Retry-After and fails over
// immediately); quota/billing failures become *QuotaError; other
// non-2xx become *UpstreamError with Retryable set from the wire
// error (5xx / timeout retryable). Mid-stream error events surface
// the same way via wire.MapCohereStreamError, and a finish_reason of
// ERROR/TIMEOUT surfaces as a *CohereError from the conversion.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// cohereClient is the Client implementation for Cohere-wire upstreams.
// It embeds *openAIClient for the shared HTTP plumbing (auth headers,
// request building, response reading); only the request/response
// shapes differ.
type cohereClient struct {
	*openAIClient
}

// chatURL is POST {base}/v2/chat.
func (c *cohereClient) chatURL() string {
	return strings.TrimRight(c.baseURL, "/") + "/v2/chat"
}

// modelsListURL is GET {base}/v1/models (the real Cohere API serves
// a model list at /v1/models).
func (c *cohereClient) modelsListURL() string {
	return strings.TrimRight(c.baseURL, "/") + "/v1/models"
}

// mapCohereError converts a *wire.CohereError (or a
// *wire.UnrepresentableError) into this package's typed errors. A
// rate limit becomes *RateLimitError (router fails over on it);
// billing/quota failures become *QuotaError (not transient); anything
// else becomes *UpstreamError preserving retryability. Unknown errors
// pass through unchanged.
func mapCohereError(err error) error {
	if err == nil {
		return nil
	}
	var ce *wire.CohereError
	if errors.As(err, &ce) {
		if ce.IsRateLimit() {
			return &RateLimitError{RetryAfter: ce.RetryAfter, RetryAfterPresent: ce.RetryAfterPresent, Body: ce.Message}
		}
		if ce.StatusCode == http.StatusPaymentRequired ||
			(ce.StatusCode == http.StatusBadRequest && isQuotaBody(ce.Message)) {
			return &QuotaError{Message: ce.Message, StatusCode: ce.StatusCode, Body: ce.Error()}
		}
		status := ce.StatusCode
		if status == 0 {
			// Mid-stream error event or finish_reason failure:
			// the HTTP status was already 200; report it as a
			// bad-gateway-class failure.
			status = http.StatusBadGateway
		}
		return &UpstreamError{StatusCode: status, Retryable: ce.Retryable, Body: ce.Error()}
	}
	var ue *wire.UnrepresentableError
	if errors.As(err, &ue) {
		// The request cannot be represented on this wire at all
		// (e.g. top_p outside Cohere's 0.01–0.99 range). It is not
		// transient and not the upstream's fault; failing over to
		// another provider is the correct recovery, so surface it
		// as a non-retryable upstream error carrying the reason.
		return &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: ue.Error()}
	}
	return err
}

func (c *cohereClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	coReq, err := wire.ConvertCohereRequest(toWireRequest(req))
	if err != nil {
		return nil, mapCohereError(err)
	}
	coReq.Stream = false
	payload, err := json.Marshal(coReq)
	if err != nil {
		return nil, fmt.Errorf("encoding cohere request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	signals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading cohere response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapCohereError(wire.MapCohereError(resp.StatusCode, resp.Header, body))
	}
	var decoded wire.CohereResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed cohere response: " + err.Error()}
	}
	converted, err := wire.ConvertCohereResponse(&decoded)
	if err != nil {
		return nil, mapCohereError(err)
	}
	if converted.Model == "" {
		converted.Model = req.Model
	}
	out, err := fromWireResponse(converted)
	if err != nil {
		return nil, err
	}
	out.RateLimit = &signals
	return out, nil
}

// ChatStream is the text-only streaming entry point; see ChatStreamChunks.
func (c *cohereClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
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

// ChatStreamChunks performs a streaming /v2/chat request,
// translating the Cohere SSE stream into OpenAI-shaped chunks via
// wire.TranslateCohereStream. The returned ChatResponse holds the
// reassembled result (content, completed tool calls, finish reason,
// usage).
func (c *cohereClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	coReq, err := wire.ConvertCohereRequest(toWireRequest(req))
	if err != nil {
		return nil, mapCohereError(err)
	}
	coReq.Stream = true
	payload, err := json.Marshal(coReq)
	if err != nil {
		return nil, fmt.Errorf("encoding cohere request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
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
	streamSignals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapCohereError(wire.MapCohereError(resp.StatusCode, resp.Header, body))
	}
	emit := func(wc wire.ChatChunk) error {
		// Same cancel rule as the OpenAI/Anthropic clients
		// (CHECKLIST 190/205): readSSE buffers whole events in
		// the scanner, so an event read before the abort could
		// otherwise be translated and delivered after it.
		// ctx.Err() passes through mapCohereError unwrapped,
		// so the router still recognises the cancellation.
		if err := ctx.Err(); err != nil {
			return err
		}
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
	res, err := wire.TranslateCohereStream(resp.Body, emit)
	if err != nil {
		return nil, mapCohereError(err)
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
		Reasoning:    res.Reasoning,
		RateLimit:    &streamSignals,
	}, nil
}

// cohereModelsListResponse is the {"models": [{"name": "..."}]} shape
// the real Cohere ListModels endpoint returns. A {"data": [{"id":
// ...}]} shape is also accepted defensively.
type cohereModelsListResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// ListModels returns the model IDs from GET {base}/v1/models.
func (c *cohereClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, c.modelsListURL(), nil)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	var decoded cohereModelsListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.Models)+len(decoded.Data))
	for _, m := range decoded.Models {
		if m.Name != "" {
			ids = append(ids, m.Name)
		}
	}
	for _, m := range decoded.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// TestKey verifies the API key with a tiny ListModels call.
func (c *cohereClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

// Ensure cohereClient satisfies the client contracts at compile time.
var (
	_ Client        = (*cohereClient)(nil)
	_ ChunkStreamer = (*cohereClient)(nil)
)
