// Package provider — Bedrock native-wire client.
//
// bedrockClient speaks the Bedrock Converse API (POST
// /model/{modelId}/converse, and /converse-stream for streaming)
// for providers whose catalog wire format is Bedrock. It converts
// between the gateway's OpenAI-shaped ChatRequest/ChatResponse and
// the Converse wire format using internal/wire (a pure conversion
// library), and signs every request with AWS SigV4 (see sigv4.go)
// from the operator's AWS credentials — the provider key material,
// "AWS_ACCESS_KEY_ID:AWS_SECRET_ACCESS_KEY" with an optional
// ":AWS_SESSION_TOKEN". No static Authorization header is ever
// sent for this wire: the catalog's exchange refusal for Bedrock is
// satisfied by the per-request signature, not by faking a header.
//
// The base URL is the Bedrock runtime root, e.g.
// https://bedrock-runtime.us-east-1.amazonaws.com (the catalog
// entry's Converse endpoint BaseURL, with {region} substituted by a
// concrete region at add time). The model ID rides in the URL path;
// the request body carries no modelId. A provider added on a
// Bedrock Mantle endpoint (OpenAI/Anthropic-compatible, Bearer <redacted>
// is NOT this client: the catalog resolves that surface to the
// OpenAI or Anthropic wire, and those clients serve it.
//
// Error mapping (failover depends on it): a 429 /
// ThrottlingException becomes *RateLimitError (the router honours
// Retry-After and fails over immediately); quota/billing failures
// become *QuotaError; other non-2xx become *UpstreamError with
// Retryable set from the wire error (5xx / timeout / throttling
// retryable). Mid-stream exception events surface the same way via
// wire.MapBedrockStreamError.
package provider

import (
	"bytes"
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

// bedrockClient is the Client implementation for Bedrock-wire
// upstreams. It embeds *openAIClient for the shared HTTP plumbing
// (HTTP client, timeouts); auth is SigV4 per request, not the
// embedded client's authHeaders — newRequest is deliberately NOT
// used here, because the signature must cover the exact headers and
// body sent.
type bedrockClient struct {
	*openAIClient
}

// bedrockCredentials parses the stored provider key into AWS
// credentials. A key that is not "ACCESS:SECRET[:TOKEN]" is an
// exchange error, never a faked header.
func (c *bedrockClient) bedrockCredentials() (awsCredentials, error) {
	return parseAWSCredentials(c.apiKey)
}

// converseURL is POST {base}/model/{modelId}/converse. The model ID
// is used verbatim in the path (Bedrock model IDs and inference
// profile IDs contain dots and colons but no "/"); the base never
// gains a "/v1" — this is not an OpenAI-compatible path.
func (c *bedrockClient) converseURL(modelID string, stream bool) string {
	suffix := "converse"
	if stream {
		suffix = "converse-stream"
	}
	return strings.TrimRight(c.baseURL, "/") + "/model/" + modelID + "/" + suffix
}

// newSignedRequest builds a SigV4-signed request for the Bedrock
// runtime: Content-Type + Accept, X-Amz-Content-Sha256 over the
// body, then the signature. The region comes from the base URL
// host; the service is always "bedrock".
func (c *bedrockClient) newSignedRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	creds, err := c.bedrockCredentials()
	if err != nil {
		return nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	if err := signSigV4(req, body, creds, regionFromBaseURL(c.baseURL), "bedrock", time.Now()); err != nil {
		return nil, err
	}
	return req, nil
}

// mapBedrockError converts a *wire.BedrockError (or a
// *wire.UnrepresentableError) into this package's typed errors. A
// rate limit becomes *RateLimitError (router fails over on it);
// billing/quota failures become *QuotaError (not transient); anything
// else becomes *UpstreamError preserving retryability. Unknown errors
// pass through unchanged.
func mapBedrockError(err error) error {
	if err == nil {
		return nil
	}
	var be *wire.BedrockError
	if errors.As(err, &be) {
		if be.IsRateLimit() {
			return &RateLimitError{RetryAfter: be.RetryAfter, RetryAfterPresent: be.RetryAfterPresent, Body: be.Message}
		}
		if be.StatusCode == http.StatusPaymentRequired ||
			(be.StatusCode == http.StatusBadRequest && isQuotaBody(be.Message)) {
			return &QuotaError{Message: be.Message, StatusCode: be.StatusCode, Body: be.Error()}
		}
		status := be.StatusCode
		if status == 0 {
			// Mid-stream exception event: the HTTP status was
			// already 200; report it as a bad-gateway-class
			// failure.
			status = http.StatusBadGateway
		}
		return &UpstreamError{StatusCode: status, Retryable: be.Retryable, Body: be.Error()}
	}
	var ue *wire.UnrepresentableError
	if errors.As(err, &ue) {
		// The request cannot be represented on this wire at all
		// (e.g. a remote image URL, an assistant-first
		// conversation). It is not transient and not the upstream's
		// fault; failing over to another provider is the correct
		// recovery, so surface it as a non-retryable upstream
		// error carrying the reason.
		return &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: ue.Error()}
	}
	return err
}

func (c *bedrockClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	bdReq, err := wire.ConvertBedrockRequest(toWireRequest(req))
	if err != nil {
		return nil, mapBedrockError(err)
	}
	payload, err := json.Marshal(bdReq)
	if err != nil {
		return nil, fmt.Errorf("encoding bedrock request: %w", err)
	}
	httpReq, err := c.newSignedRequest(ctx, http.MethodPost, c.converseURL(req.Model, false), payload)
	if err != nil {
		return nil, mapBedrockError(err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	signals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading bedrock response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapBedrockError(wire.MapBedrockError(resp.StatusCode, resp.Header, body))
	}
	var decoded wire.BedrockResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed bedrock response: " + err.Error()}
	}
	converted, err := wire.ConvertBedrockResponse(&decoded)
	if err != nil {
		return nil, mapBedrockError(err)
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
func (c *bedrockClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
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

// ChatStreamChunks performs a streaming ConverseStream request,
// translating the Amazon event-stream body into OpenAI-shaped chunks
// via wire.TranslateBedrockStream. The returned ChatResponse holds
// the reassembled result (content, completed tool calls, finish
// reason, usage).
func (c *bedrockClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	bdReq, err := wire.ConvertBedrockRequest(toWireRequest(req))
	if err != nil {
		return nil, mapBedrockError(err)
	}
	payload, err := json.Marshal(bdReq)
	if err != nil {
		return nil, fmt.Errorf("encoding bedrock request: %w", err)
	}
	// Per-provider abort (CHECKLIST 277, LiteLLM #51034 half (e)):
	// the stream runs under a child context the client itself can
	// cancel. ConverseStream has no abort API — closing the HTTP
	// connection IS the abort: AWS stops generating when the
	// connection drops, so billed generation stops with the stream
	// instead of running to completion with no reader. The cancel
	// fires the moment the caller stops consuming (onChunk error)
	// or the parent context is cancelled; the deferred cancel
	// also closes the connection when Translate returns for any
	// other reason, instead of leaving the body close (defer
	// below) as the only abort. Without the explicit cancel, an
	// onChunk error still closed the body eventually — but only
	// after Translate unwound — and a caller that cancelled the
	// parent context relied on the transport noticing it; the
	// child context makes the abort immediate and observable in
	// both cases (see bedrock_abort_test.go: the mock upstream
	// stops sending frames when the connection drops).
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	httpReq, err := c.newSignedRequest(streamCtx, http.MethodPost, c.converseURL(req.Model, true), payload)
	if err != nil {
		return nil, mapBedrockError(err)
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
		return nil, mapBedrockError(wire.MapBedrockError(resp.StatusCode, resp.Header, body))
	}
	emit := func(wc wire.ChatChunk) error {
		// Same cancel rule as the OpenAI/Anthropic clients
		// (CHECKLIST 190/205): wire.readEventStream now
		// delivers frames incrementally (CHECKLIST 215), so a
		// frame that arrives after the abort must never be
		// delivered. ctx.Err() passes through mapBedrockError
		// unwrapped, so the router still recognises the
		// cancellation.
		if err := streamCtx.Err(); err != nil {
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
		// A consumer that stops consuming aborts the upstream
		// NOW (see the child context above): cancel before
		// returning the error, so the connection drops while
		// Translate is still unwinding instead of after it.
		if err := onChunk(chunk); err != nil {
			cancel()
			return err
		}
		return nil
	}
	res, err := wire.TranslateBedrockStream(resp.Body, emit)
	if err != nil {
		return nil, mapBedrockError(err)
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

// bedrockFoundationModelsResponse is the ListFoundationModels shape:
// {"modelSummaries": [{"modelId": "..."}]}.
type bedrockFoundationModelsResponse struct {
	ModelSummaries []struct {
		ModelID string `json:"modelId"`
	} `json:"modelSummaries"`
}

// listModelsURL is GET {control-plane}/foundation-models. The real
// Bedrock control plane lives on bedrock.{region}.amazonaws.com,
// while the runtime base is bedrock-runtime.{region}.amazonaws.com.
// A non-AWS base (mock, proxy) has no control-plane host to derive,
// so the request goes to {base}/foundation-models instead.
func (c *bedrockClient) listModelsURL() string {
	u := strings.TrimRight(c.baseURL, "/")
	if strings.HasPrefix(u, "https://bedrock-runtime.") {
		rest := strings.TrimPrefix(u, "https://bedrock-runtime.")
		return "https://bedrock." + rest + "/foundation-models"
	}
	if strings.HasPrefix(u, "http://bedrock-runtime.") {
		rest := strings.TrimPrefix(u, "http://bedrock-runtime.")
		return "http://bedrock." + rest + "/foundation-models"
	}
	return u + "/foundation-models"
}

// ListModels returns the foundation model IDs from the Bedrock
// control plane (ListFoundationModels). The runtime API has no model
// list, so this is the honest endpoint for TestKey / model
// discovery; it is a separate SigV4-signed GET.
func (c *bedrockClient) ListModels(ctx context.Context) ([]string, error) {
	httpReq, err := c.newSignedRequest(ctx, http.MethodGet, c.listModelsURL(), nil)
	if err != nil {
		return nil, mapBedrockError(err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("reading bedrock models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, mapBedrockError(wire.MapBedrockError(resp.StatusCode, resp.Header, body))
	}
	var decoded bedrockFoundationModelsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed bedrock models response: " + err.Error()}
	}
	ids := make([]string, 0, len(decoded.ModelSummaries))
	for _, m := range decoded.ModelSummaries {
		if m.ModelID != "" {
			ids = append(ids, m.ModelID)
		}
	}
	return ids, nil
}

// TestKey verifies the AWS credentials with a ListFoundationModels call.
func (c *bedrockClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}

// Ensure bedrockClient satisfies the client contracts at compile time.
var (
	_ Client        = (*bedrockClient)(nil)
	_ ChunkStreamer = (*bedrockClient)(nil)
)
