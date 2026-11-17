// Package provider — Anthropic native-wire client.
//
// anthropicClient speaks the Anthropic Messages API (POST /v1/messages)
// for providers whose catalog wire format is Anthropic. It converts
// between the gateway's OpenAI-shaped ChatRequest/ChatResponse and the
// Messages wire format using internal/wire (a pure conversion library),
// and resolves auth per (provider, endpoint) through the catalog —
// exactly the headers ResolveAuth returns (Bearer + anthropic-version
// on the messages surface, x-api-key on DeepSeek's Anthropic surface),
// never a hard-coded Bearer.
//
// Error mapping (failover depends on it): a 429 / rate_limit_error
// becomes *RateLimitError (the router honours Retry-After and fails
// over immediately); billing/quota failures become *QuotaError; other
// non-2xx become *UpstreamError with Retryable set from the wire
// error (overloaded / 5xx retryable). Mid-stream error events surface
// the same way via wire.MapStreamError.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// anthropicClient is the Client implementation for Anthropic-wire
// upstreams. It embeds *openAIClient for the shared HTTP plumbing
// (auth headers, request building, response reading); only the
// request/response shapes differ.
type anthropicClient struct {
	*openAIClient
}

// messagesURL is POST {base}/v1/messages (with the /v1 rule from
// joinAPIPath: a base ending in "/v1" is used as-is, otherwise "/v1"
// is appended — so both https://api.anthropic.com and
// https://api.deepseek.com/anthropic resolve to .../v1/messages).
func (c *anthropicClient) messagesURL() string {
	return joinAPIPath(c.baseURL, "messages")
}

// toWireRequest converts a gateway ChatRequest into wire's OpenAI
// shape, which wire.ConvertRequest then turns into an
// AnthropicRequest. Message content is passed through raw (string or
// content-part array) so images and tool calls survive; temperature /
// top_p are pointers so "absent" stays distinct from an explicit 0.
func toWireRequest(req ChatRequest) wire.ChatCompletionRequest {
	out := wire.ChatCompletionRequest{
		Model:     req.Model,
		Messages:  make([]wire.ChatMessage, 0, len(req.Messages)),
		Stream:    req.Stream,
		MaxTokens: req.MaxTokens,
		Stop:      append([]string(nil), req.Stop...),
		Tools:     req.Tools,
	}
	if req.Temperature != 0 {
		t := req.Temperature
		out.Temperature = &t
	}
	if req.TopP != 0 {
		p := req.TopP
		out.TopP = &p
	}
	if len(req.ToolChoice) > 0 {
		var tc any
		if err := json.Unmarshal(req.ToolChoice, &tc); err == nil {
			out.ToolChoice = tc
		}
	}
	for _, m := range req.Messages {
		wm := wire.ChatMessage{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		switch {
		case len(m.Parts) > 0:
			wm.Content = mustJSONRaw(m.Parts)
		case m.Content != "":
			wm.Content = mustJSONRaw(m.Content)
		default:
			// No content (e.g. an assistant message carrying only
			// tool calls): leave Content nil so it encodes as null.
		}
		out.Messages = append(out.Messages, wm)
	}
	return out
}

// fromWireResponse converts a wire ChatCompletionResponse into the
// gateway shape, decoding the message content string.
func fromWireResponse(resp *wire.ChatCompletionResponse) (*ChatResponse, error) {
	if resp == nil || len(resp.Choices) == 0 {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "upstream returned no choices"}
	}
	choice := resp.Choices[0]
	var content string
	if len(choice.Message.Content) > 0 {
		if err := json.Unmarshal(choice.Message.Content, &content); err != nil {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed anthropic response content: " + err.Error()}
		}
	}
	return &ChatResponse{
		Content:      content,
		Usage:        Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens},
		Model:        resp.Model,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
	}, nil
}

// mapAnthropicError converts a *wire.AnthropicError (or a
// *wire.UnrepresentableError) into this package's typed errors. A
// rate limit becomes *RateLimitError (router fails over on it);
// billing/quota failures become *QuotaError (not transient); anything
// else becomes *UpstreamError preserving retryability. Unknown errors
// pass through unchanged.
func mapAnthropicError(err error) error {
	if err == nil {
		return nil
	}
	var ae *wire.AnthropicError
	if errors.As(err, &ae) {
		if ae.IsRateLimit() {
			return &RateLimitError{RetryAfter: ae.RetryAfter, Body: ae.Message}
		}
		if ae.Type == "billing_error" || ae.StatusCode == http.StatusPaymentRequired ||
			(ae.StatusCode == http.StatusBadRequest && isQuotaBody(ae.Message)) {
			return &QuotaError{Message: ae.Message, StatusCode: ae.StatusCode, Body: ae.Error()}
		}
		status := ae.StatusCode
		if status == 0 {
			// Mid-stream error event: the HTTP status was already
			// 200; report it as a bad-gateway-class failure.
			status = http.StatusBadGateway
		}
		return &UpstreamError{StatusCode: status, Retryable: ae.Retryable, Body: ae.Error()}
	}
	var ue *wire.UnrepresentableError
	if errors.As(err, &ue) {
		// The request cannot be represented on this wire at all
		// (e.g. temperature > 1). It is not transient and not the
		// upstream's fault; failing over to an OpenAI-wire provider
		// is the correct recovery, so surface it as a
		// non-retryable upstream error carrying the reason.
		return &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: ue.Error()}
	}
	return err
}

func (c *anthropicClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	anthReq, err := wire.ConvertRequest(toWireRequest(req))
	if err != nil {
		return nil, mapAnthropicError(err)
	}
	anthReq.Stream = false
	payload, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("encoding anthropic request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.messagesURL(), payload)
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
		return nil, fmt.Errorf("reading anthropic response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapAnthropicError(wire.MapError(resp.StatusCode, resp.Header, body))
	}
	var decoded wire.AnthropicResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed anthropic response: " + err.Error()}
	}
	converted, err := wire.ConvertResponse(&decoded)
	if err != nil {
		return nil, mapAnthropicError(err)
	}
	return fromWireResponse(converted)
}

// ChatStream is the text-only streaming entry point; see ChatStreamChunks.
func (c *anthropicClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
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

// ChatStreamChunks performs a streaming Messages request, translating
// the Anthropic SSE stream into OpenAI-shaped chunks via
// wire.TranslateStream. The returned ChatResponse holds the
// reassembled result (content, completed tool calls, finish reason,
// usage).
func (c *anthropicClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	anthReq, err := wire.ConvertRequest(toWireRequest(req))
	if err != nil {
		return nil, mapAnthropicError(err)
	}
	anthReq.Stream = true
	payload, err := json.Marshal(anthReq)
	if err != nil {
		return nil, fmt.Errorf("encoding anthropic request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.messagesURL(), payload)
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
		return nil, mapAnthropicError(wire.MapError(resp.StatusCode, resp.Header, body))
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
	res, err := wire.TranslateStream(resp.Body, emit)
	if err != nil {
		return nil, mapAnthropicError(err)
	}
	return &ChatResponse{
		Content:      res.Content,
		Usage:        Usage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens},
		Model:        res.Model,
		ToolCalls:    res.ToolCalls,
		FinishReason: res.FinishReason,
	}, nil
}

// ListModels returns the model IDs from GET {base}/v1/models.
// Anthropic's real API serves this endpoint (Bearer or x-api-key,
// plus the anthropic-version header from the catalog-resolved auth).
func (c *anthropicClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newRequest(ctx, http.MethodGet, c.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	var decoded modelsListResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.Data))
	for _, m := range decoded.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// TestKey verifies the API key with a tiny ListModels call.
func (c *anthropicClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

// Ensure anthropicClient satisfies the client contracts at compile time.
var (
	_ Client        = (*anthropicClient)(nil)
	_ ChunkStreamer = (*anthropicClient)(nil)
)

// Ensure openAIClient satisfies ChunkStreamer too.
var _ ChunkStreamer = (*openAIClient)(nil)
