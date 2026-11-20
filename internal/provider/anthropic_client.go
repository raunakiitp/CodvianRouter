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
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
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
			Reasoning:  m.Reasoning,
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
		Usage:        Usage{PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens, TotalTokens: resp.Usage.TotalTokens, CachedTokens: resp.Usage.CachedTokens, CacheWriteTokens: resp.Usage.CacheWriteTokens, CacheWriteTokens5m: resp.Usage.CacheWriteTokens5m, CacheWriteTokens1h: resp.Usage.CacheWriteTokens1h},
		Model:        resp.Model,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
		Reasoning:    resp.Reasoning,
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
			return &RateLimitError{RetryAfter: ae.RetryAfter, RetryAfterPresent: ae.RetryAfterPresent, Body: ae.Message}
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

// applyAnthropicThinkingBudget stamps the operator-declared thinking
// budget onto a converted Anthropic request (CHECKLIST 285-286).
// The budget arrives on ChatRequest.ThinkingBudget, resolved by
// the engine from config `reasoning.effort_budgets` for the
// attempt's effective effort. A zero budget sends no thinking
// field at all — no default is invented, and the provider default
// applies.
func applyAnthropicThinkingBudget(anthReq *wire.AnthropicRequest, req ChatRequest) {
	if anthReq == nil || req.ThinkingBudget <= 0 {
		return
	}
	anthReq.Thinking = &wire.AnthropicThinking{Type: "enabled", BudgetTokens: req.ThinkingBudget}
}

// ---------------------------------------------------------------------------
// Prompt-cache marker (M6.9c): wire cache.Prepare()'s annotation into
// production on the Anthropic Messages API.
// ---------------------------------------------------------------------------

// promptCacheMarkerProviders is the LIVE subset of
// cache.promptCacheProviders (and its strategy mirror
// strategy.promptCacheMarkerProviders) that this client actually
// annotates: the native Anthropic hosts only. The two maps stay
// unchanged and in lockstep; this set is deliberately narrower,
// and the gap is a reported divergence, not drift:
//
//   - "openrouter" stays OUT of THIS client. Its default surface
//     in this router is the OpenAI-compatible wire, where the
//     marker now rides on content parts (G13:
//     openAIClient.applyOpenRouterPromptCacheMarker in
//     client.go — the OpenAI wire gained the cache_control
//     path, so OpenRouter's chat-completions endpoint is marked
//     there, gated to the Claude/Qwen/Gemini model families its
//     docs say honour explicit breakpoints). OpenRouter's own
//     docs DO say explicit cache_control breakpoints work on its
//     chat-completions API for Anthropic models
//     (openrouter.ai/docs/guides/best-practices/prompt-caching:
//     "Place cache_control directly on individual content
//     blocks", 5-minute default, 1-hour via "ttl": "1h").
//     The Anthropic endpoint variant of OpenRouter IS served by
//     this client, but the model behind it is not guaranteed to
//     be Claude and OpenRouter's marker translation for
//     non-Claude models on the Messages endpoint is not verified
//     against primary docs, so it stays out there too (fail
//     closed).
//   - Any other provider on the Anthropic wire (DeepSeek's
//     /anthropic surface, Fireworks, opencode, ...) stays out for
//     the same reason: the wire shape fits, but no evidence shows
//     the upstream honours (or even accepts) the marker, and a
//     marker on an endpoint that 400s it would break working
//     requests. Only hosts Anthropic itself runs are annotated.
//
// providerID is set by the registry only (openAIClient.setHTTPAdapter,
// called from registry.clientForKey via attachHTTPAdapter); ad-hoc
// clients built outside the registry carry "" and are NOT annotated
// — fail closed, never annotate on a guessed provider.
var promptCacheMarkerProviders = map[string]bool{
	"anthropic": true,
	"claude":    true,
}

// minPromptCacheTokens is the smallest prompt size (estimated tokens)
// worth annotating: the router's shared floor
// (strategy.MinCacheablePromptTokens = 1024 — Anthropic's minimum
// cacheable prefix is 1024 tokens for most Claude models; newer
// models need up to 4096, see the OpenRouter docs table, so this
// floor can still pay a write that never caches on those models —
// a per-model floor is follow-up work, not invented here). The
// constant is duplicated here rather than imported because
// internal/strategy imports internal/router, which imports this
// package — importing it would be a cycle.
const minPromptCacheTokens = 1024

// promptCharsPerToken is the estimator for the length gate below:
// ~4 characters per token (Anthropic's own rough rule). Byte length
// is used, so non-ASCII text underestimates (a 3-byte CJK character
// counts as 3/4 of a token against ~1.5 real ones) — the gate errs
// toward NOT annotating, never toward annotating a short prompt.
const promptCharsPerToken = 4

// estimatedPromptTokens estimates the prompt size in tokens from raw
// character count: every message's text, which is the cacheable
// prefix up to and including the last user message plus any trailing
// content. A message decoded from array-form JSON carries BOTH
// Content (concatenated text parts, set by Message.UnmarshalJSON)
// and Parts, so Parts-form messages count their text Parts ONLY and
// Content is counted only when Parts is empty — counting both would
// double-count the same text and could mark a ~600-token prompt.
// It is an ESTIMATE, not the tiktoken count the engine computes
// (internal/router localPromptCount): this package cannot import
// internal/tokencount — tokencount imports this package
// (tokencount/audit.go uses provider types), so that import would
// be a cycle. The estimate only gates the marker; a miscount costs
// a missed saving (underestimate) or a wasted 1.25x write
// (overestimate on >4-char/token text), never a wrong answer.
func estimatedPromptTokens(req ChatRequest) int {
	chars := 0
	for _, m := range req.Messages {
		if len(m.Parts) > 0 {
			for _, p := range m.Parts {
				if p.Type == "text" {
					chars += len(p.Text)
				}
			}
			continue
		}
		chars += len(m.Content)
	}
	return chars / promptCharsPerToken
}

// promptCacheEligible is the M6.5 cacheability gate, applied with the
// same rules the router's own cache uses (cache.CacheableTemperature
// with allowNonZeroTemp=false; cache.ShouldStore's tool rule;
// strategy.MinCacheablePromptTokens), plus the provider allowlist
// above:
//
//   - provider: this client must serve a native Anthropic host
//     (providerID in promptCacheMarkerProviders). The wire format
//     alone does not qualify: DeepSeek/Fireworks/opencode speak
//     the same wire to upstreams with no verified marker support.
//   - temperature: exactly 0 (unset decodes as 0, same convention
//     as cache.CacheableTemperature). Anthropic itself does not
//     require temp 0 for prefix reuse — this rule is the router's
//     own cacheability definition, applied conservatively so a
//     marker is never written for a request the router's cache
//     would refuse.
//   - tools: tool DEFINITIONS no longer disqualify (G13 —
//     tools marking, below). A request that DEFINES tools but
//     has not called any yet gets its system prompt, its last
//     tool definition and its last user message marked (3
//     markers, within Anthropic's 4-marker limit). A request
//     whose messages already carry tool calls, or a tool-role
//     message, still does not get marked: a cached prefix that
//     ends mid-tool-flow cannot be reused by the local cache
//     (ShouldStore), and the marker would pay a write for a
//     prefix the router itself would refuse to store.
//   - length: estimated prompt tokens >= minPromptCacheTokens.
//     Below Anthropic's 1024-token minimum nothing caches and the
//     1.25x cache-write charge (5-minute TTL) is pure waste.
func (c *anthropicClient) promptCacheEligible(req ChatRequest) bool {
	if c == nil || c.openAIClient == nil {
		return false
	}
	if !promptCacheMarkerProviders[strings.ToLower(strings.TrimSpace(c.providerID))] {
		return false
	}
	if req.Temperature != 0 {
		return false
	}
	for _, m := range req.Messages {
		if len(m.ToolCalls) > 0 || m.Role == "tool" {
			return false
		}
	}
	return estimatedPromptTokens(req) >= minPromptCacheTokens
}

// anthropicMarkerCount counts cache_control markers already on the
// converted request — client-supplied ones (preserved verbatim by
// wire.ConvertRequest) plus any the auto-marker placed. Anthropic
// allows at most 4 blocks with cache_control per request.
func anthropicMarkerCount(anthReq *wire.AnthropicRequest) int {
	n := 0
	for _, b := range anthReq.SystemBlocks {
		if b.CacheControl != nil {
			n++
		}
	}
	for _, m := range anthReq.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				n++
			}
		}
	}
	for _, t := range anthReq.Tools {
		if t.CacheControl != nil {
			n++
		}
	}
	return n
}

// applyPromptCacheMarker sets cache_control {"type": "ephemeral"} on
// up to three slots of an already-converted Anthropic request —
// the exact annotation cache.Prepare() describes for its own
// request type, placed here because this client is the only
// production caller of wire.ConvertRequest (both call sites
// below) and the only place that knows the serving provider
// (providerID), the temperature, the tools, and the prompt length
// together. The relay is NOT the site: it hands the request to
// engine.Chat/ChatStream (internal/server/relay.go:1221,1396) and
// never sees the resolved provider or the wire body.
//
// Slots, in priority order (G13 adds system + tools alongside the
// M6.9c last-user slot; Anthropic allows at most 4 markers per
// request, so the three auto-markers plus a client marker still
// fit — a slot that already carries a client-supplied marker is
// left exactly as the client sent it, and no auto-marker is added
// once 4 markers are present):
//
//  1. Last user message's last text block (M6.9c). Placement
//     mirrors cache.Prepare (LAST user message) and the reference router's
//     markLastCacheableBlock: walk back from the last user
//     message, then back through its blocks to the last
//     non-empty TEXT block — thinking/redacted_thinking, blank
//     text, image and tool_result blocks never take a marker
//     (Anthropic 400s cache_control on thinking blocks and
//     nested inside tool_result.content).
//  2. System prompt (G13): the last text block of the system
//     prompt. When the system arrived as a plain string it is
//     lifted into block form (SystemBlocks) so the marker has
//     somewhere to sit; the string form is untouched when no
//     marker is placed.
//  3. Tool definitions (G13): the LAST tool definition. Anthropic
//     pins the prefix up to and including the marked tool, so a
//     long tool list is cached like the rest of the prefix.
//
// No TTL is set: the provider default is the 5-minute tier
// (1.25x write, 0.1x read), matching cache.Prepare's
// {"type": "ephemeral"} and the M6.9b ledger tier split; the
// 1-hour tier (2x write) is not opted into. The marker is a
// no-op when the gate fails — never annotate a request the
// cache rules would refuse.
func (c *anthropicClient) applyPromptCacheMarker(anthReq *wire.AnthropicRequest, req ChatRequest) {
	if anthReq == nil || !c.promptCacheEligible(req) {
		return
	}
	const maxAnthropicMarkers = 4
	marked := anthropicMarkerCount(anthReq)
	ephemeral := &wire.AnthropicCacheControl{Type: "ephemeral"}
	// 1. Last user message's last text block.
	if marked < maxAnthropicMarkers {
		for i := len(anthReq.Messages) - 1; i >= 0; i-- {
			if anthReq.Messages[i].Role != "user" {
				continue
			}
			blocks := anthReq.Messages[i].Content
			for j := len(blocks) - 1; j >= 0; j-- {
				if blocks[j].Type != "text" || strings.TrimSpace(blocks[j].Text) == "" {
					continue
				}
				if blocks[j].CacheControl == nil {
					blocks[j].CacheControl = ephemeral
					marked++
				}
				break
			}
			break
		}
	}
	// 2. System prompt: last text block.
	if marked < maxAnthropicMarkers {
		if len(anthReq.SystemBlocks) > 0 {
			for j := len(anthReq.SystemBlocks) - 1; j >= 0; j-- {
				if anthReq.SystemBlocks[j].Type != "text" || strings.TrimSpace(anthReq.SystemBlocks[j].Text) == "" {
					continue
				}
				if anthReq.SystemBlocks[j].CacheControl == nil {
					anthReq.SystemBlocks[j].CacheControl = ephemeral
					marked++
				}
				break
			}
		} else if strings.TrimSpace(anthReq.System) != "" {
			anthReq.SystemBlocks = []wire.AnthropicBlock{{
				Type: "text", Text: anthReq.System, CacheControl: ephemeral,
			}}
			marked++
		}
	}
	// 3. Tool definitions: last tool.
	if marked < maxAnthropicMarkers && len(anthReq.Tools) > 0 {
		last := &anthReq.Tools[len(anthReq.Tools)-1]
		if last.CacheControl == nil {
			last.CacheControl = ephemeral
		}
	}
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
	applyAnthropicThinkingBudget(anthReq, req)
	c.applyPromptCacheMarker(anthReq, req)
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
	signals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
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
	out, err := fromWireResponse(converted)
	if err != nil {
		return nil, err
	}
	out.RateLimit = &signals
	return out, nil
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
	applyAnthropicThinkingBudget(anthReq, req)
	c.applyPromptCacheMarker(anthReq, req)
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
	streamSignals := adaptive.ParseRateLimitHeaders(resp.Header, time.Now())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapAnthropicError(wire.MapError(resp.StatusCode, resp.Header, body))
	}
	emit := func(wc wire.ChatChunk) error {
		// Same cancel rule as the OpenAI client (CHECKLIST
		// 190): readSSE buffers whole events in the scanner,
		// so an event read before the abort could otherwise
		// be translated and delivered after it. ctx.Err()
		// passes through mapAnthropicError unwrapped, so the
		// router still recognises the cancellation.
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
				PromptTokens:       wc.Usage.PromptTokens,
				CompletionTokens:   wc.Usage.CompletionTokens,
				TotalTokens:        wc.Usage.TotalTokens,
				CachedTokens:       wc.Usage.CachedTokens,
				CacheWriteTokens:   wc.Usage.CacheWriteTokens,
				CacheWriteTokens5m: wc.Usage.CacheWriteTokens5m,
				CacheWriteTokens1h: wc.Usage.CacheWriteTokens1h,
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
		Usage:        Usage{PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens, CachedTokens: res.Usage.CachedTokens, CacheWriteTokens: res.Usage.CacheWriteTokens, CacheWriteTokens5m: res.Usage.CacheWriteTokens5m, CacheWriteTokens1h: res.Usage.CacheWriteTokens1h},
		Model:        res.Model,
		ToolCalls:    res.ToolCalls,
		FinishReason: res.FinishReason,
		Reasoning:    res.Reasoning,
		RateLimit:    &streamSignals,
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
