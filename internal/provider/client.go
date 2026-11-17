package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ContentPart, ToolCall and ToolDefinition are the OpenAI shapes from
// internal/wire, aliased here so the gateway and the conversion layer
// share one definition (there is exactly one OpenAI wire shape).
type ContentPart = wire.ContentPart
type ToolCall = wire.ToolCall
type ToolDefinition = wire.ToolDefinition

// Message is a single chat message. Content is the plain-text form
// (concatenated text parts when the wire sent an array). Parts carries
// multi-part content (text + image_url) when the client sent it as an
// array; ToolCalls carries assistant tool calls, ToolCallID the id a
// "tool" message answers. Content decodes from either a JSON string
// or an array of parts, and encodes back the same way it arrived
// (array when Parts is set, string otherwise); see MarshalJSON /
// UnmarshalJSON below.
type Message struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	Parts      []ContentPart `json:"-"`
	ToolCalls  []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
	Name       string        `json:"name,omitempty"`
}

// messageJSON is the decode/encode shape for Message.
type messageJSON struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// UnmarshalJSON decodes a Message, accepting content as a plain string,
// an array of content parts, or null (assistant messages that only
// carry tool calls).
func (m *Message) UnmarshalJSON(data []byte) error {
	var raw messageJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role
	m.ToolCalls = raw.ToolCalls
	m.ToolCallID = raw.ToolCallID
	m.Name = raw.Name
	m.Parts = nil
	m.Content = ""
	trimmed := strings.TrimSpace(string(raw.Content))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var asString string
	if err := json.Unmarshal(raw.Content, &asString); err == nil {
		m.Content = asString
		return nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw.Content, &parts); err != nil {
		return fmt.Errorf("provider: message content is neither a string nor a content-part array: %w", err)
	}
	m.Parts = parts
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	m.Content = sb.String()
	return nil
}

// MarshalJSON encodes a Message: content goes out as an array when
// Parts is set, as a string otherwise (null when there is no content
// at all, e.g. an assistant message carrying only tool calls).
func (m Message) MarshalJSON() ([]byte, error) {
	var content any
	switch {
	case len(m.Parts) > 0:
		content = m.Parts
	case m.Content != "":
		content = m.Content
	default:
		content = nil
	}
	return json.Marshal(messageJSON{
		Role:       m.Role,
		Content:    mustJSONRaw(content),
		ToolCalls:  m.ToolCalls,
		ToolCallID: m.ToolCallID,
		Name:       m.Name,
	})
}

// mustJSONRaw marshals v to its JSON encoding (infallible for the
// values used here: string, []ContentPart, nil).
func mustJSONRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

// ChatRequest is a chat completion request. MaxTokens 0 means "provider
// default"; Temperature / TopP 0 means "omit, use provider default".
// Stop accepts a string or an array of strings on decode (it always
// encodes as an array). ToolChoice is kept raw so every OpenAI form
// ("none" | "auto" | "required" | {"type":"function",...}) passes
// through to the wire conversion unchanged.
type ChatRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Stream      bool             `json:"stream"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	TopP        float64          `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
}

// chatRequestAlias is ChatRequest without its methods, so decoding
// into chatRequestJSON cannot recurse back into UnmarshalJSON.
type chatRequestAlias ChatRequest

// chatRequestJSON is the decode shape for ChatRequest (Stop may be a
// string or a string array on the wire).
type chatRequestJSON struct {
	chatRequestAlias
	Stop json.RawMessage `json:"stop,omitempty"`
}

// UnmarshalJSON decodes a ChatRequest, accepting Stop as either a
// single string or an array of strings.
func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	var raw chatRequestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ChatRequest(raw.chatRequestAlias)
	r.Stop = nil
	if len(raw.Stop) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw.Stop, &single); err == nil {
		r.Stop = []string{single}
		return nil
	}
	var multi []string
	if err := json.Unmarshal(raw.Stop, &multi); err != nil {
		return fmt.Errorf("provider: invalid stop value: %w", err)
	}
	r.Stop = multi
	return nil
}

// Usage is token usage reported by the upstream.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a completed (non-streaming or reassembled streaming)
// chat completion. ToolCalls carries the model's tool calls (empty
// for a plain text turn) and FinishReason the upstream finish reason
// ("stop", "length", "tool_calls", ...); both are "" when the upstream
// did not report them.
type ChatResponse struct {
	Content      string     `json:"content"`
	Usage        Usage      `json:"usage"`
	Model        string     `json:"model"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

// ChunkToolCall is one tool-call delta inside a streaming chunk:
// Index identifies the call, ID/Name arrive on its first delta,
// Arguments carries a fragment (streaming) or the complete arguments
// (reassembled response).
type ChunkToolCall struct {
	Index     int    `json:"index"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// StreamChunk is one chunk of a streaming chat completion, in
// provider-neutral form: Content is a text delta ("" when the chunk
// carries only tool-call deltas), ToolCalls the tool-call deltas of
// this chunk, FinishReason the upstream finish reason ("" until the
// final chunk), Usage the usage block when the upstream sent one
// (nil otherwise).
type StreamChunk struct {
	Content      string          `json:"content,omitempty"`
	ToolCalls    []ChunkToolCall `json:"tool_calls,omitempty"`
	FinishReason string          `json:"finish_reason,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
}

// ChunkStreamer is optionally implemented by Clients that can stream
// chunk-aware: onChunk is called once per upstream chunk, in order,
// with the full chunk (text delta, tool-call deltas, finish reason,
// usage) — not just a text string. The returned ChatResponse holds
// the reassembled result (content, completed tool calls, finish
// reason, usage). Clients that do not implement it stream text-only
// through ChatStream; the router and server prefer this interface
// when present and fall back otherwise.
type ChunkStreamer interface {
	ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error)
}

// RateLimitError is returned when the upstream answers 429. RetryAfter is
// parsed from the Retry-After header (seconds, or an HTTP date); it is 0
// when the header is absent or unparsable — callers should then apply
// their own backoff.
type RateLimitError struct {
	RetryAfter time.Duration
	Body       string
}

func (e *RateLimitError) Error() string {
	if e == nil {
		return "rate limited"
	}
	if e.RetryAfter > 0 {
		return fmt.Sprintf("upstream rate limited, retry after %s", e.RetryAfter)
	}
	return "upstream rate limited"
}

// QuotaError is returned when the upstream reports exhausted quota /
// billing problems (HTTP 402, or a 4xx whose body mentions quota,
// insufficient_quota, billing, or "exceeded"). Unlike a rate limit this is
// not transient: retrying the same provider will not help.
type QuotaError struct {
	Message    string
	StatusCode int
	Body       string
}

func (e *QuotaError) Error() string {
	if e == nil {
		return "upstream quota exhausted"
	}
	if e.Message != "" {
		return fmt.Sprintf("upstream quota error: %s", e.Message)
	}
	return "upstream quota exhausted"
}

// UpstreamError is any other non-2xx upstream response. Retryable is true
// for 408, 429 (only when not already mapped to RateLimitError — it always
// is, so this is defensive) and 5xx.
type UpstreamError struct {
	StatusCode int
	Retryable  bool
	Body       string
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "upstream error"
	}
	return fmt.Sprintf("upstream error: HTTP %d%s", e.StatusCode, truncateForError(e.Body))
}

func truncateForError(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if len(body) > 200 {
		body = body[:200] + "…"
	}
	return ": " + body
}

// Client is an OpenAI-compatible upstream client. Implementations must be
// safe for concurrent use.
type Client interface {
	// Chat performs a single non-streaming chat completion.
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream performs a streaming chat completion, calling onDelta for
	// each content delta in order. The returned ChatResponse holds the
	// reassembled full content. If onDelta returns an error, streaming
	// stops and that error is returned.
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error)
	// ListModels returns the model IDs from GET {base}/models.
	ListModels(ctx context.Context) ([]string, error)
	// TestKey verifies the API key with a tiny ListModels call.
	TestKey(ctx context.Context) error
}

// ClientOption configures an openAIClient.
type ClientOption func(*openAIClient)

// WithHTTPClient overrides the HTTP client (tests, custom transports).
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *openAIClient) { c.httpClient = hc }
}

// WithTimeout sets the timeout for non-streaming requests. Streaming
// requests always run with no client-level timeout (context controls them).
func WithTimeout(d time.Duration) ClientOption {
	return func(c *openAIClient) { c.httpClient.Timeout = d }
}

// WithAuthHeaders sets the exact auth headers sent on every upstream
// request, as resolved by the catalog for one (provider, endpoint)
// pair (see catalog.ResolveAuth). A nil map means "legacy": send
// `Authorization: Bearer <key>`. A non-nil map — possibly empty, for
// no-auth surfaces — replaces the Bearer default entirely, so a
// provider that needs `x-api-key` (DeepSeek's Anthropic surface),
// `api-key` (Azure), `X-Fireworks-Api-Key`, or extra headers like
// `anthropic-version` gets exactly those headers and nothing else.
// Exchange surfaces never reach this option: the resolver returns an
// error instead of headers, so no static header is ever faked for them.
func WithAuthHeaders(headers map[string]string) ClientOption {
	return func(c *openAIClient) { c.authHeaders = headers }
}

// WithWireFormat selects the wire format the client speaks.
// "anthropic" returns a Messages client (internal/wire), "gemini" a
// generateContent client (internal/wire); "" and "openai" return the
// OpenAI-compatible client. Any other format (cohere,
// bedrock-converse, ollama, unknown) also returns the OpenAI client
// for now — those native transports are not built yet, and the
// previous behaviour was to speak OpenAI wire to them and fail over
// on the upstream error, so falling back preserves it instead of
// breaking ClientFor for providers added before this option existed.
func WithWireFormat(format string) ClientOption {
	return func(c *openAIClient) { c.wireFormat = format }
}

// openAIClient is the Client implementation for OpenAI-compatible upstreams.
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// authHeaders, when non-nil, are the exact auth headers to send
	// (resolved per provider + endpoint by the catalog). Nil means the
	// legacy default: Authorization: Bearer <apiKey>.
	authHeaders map[string]string
	// wireFormat selects the wire spoken by NewClient: "" / "openai"
	// stays here, "anthropic" builds an anthropicClient and "gemini"
	// a geminiClient wrapping this one, anything else falls back to
	// OpenAI wire (see WithWireFormat).
	wireFormat string
}

// NewClient builds a Client for an upstream. baseURL is the API root: it
// may already end in "/v1" (e.g. https://api.codvian.com/v1) or be a
// bare host root (e.g. https://api.openai.com); the client appends
// "/v1/..." only when the path does not already end in "/v1". The wire
// format comes from WithWireFormat: "anthropic" returns a Messages
// client, "gemini" a generateContent client, everything else the
// OpenAI-compatible client.
func NewClient(baseURL, apiKey string, opts ...ClientOption) (Client, error) {
	normalized, err := ValidateBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	c := &openAIClient{
		baseURL:    normalized,
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	if c.wireFormat == WireFormatAnthropic {
		return &anthropicClient{openAIClient: c}, nil
	}
	if c.wireFormat == WireFormatGemini {
		return &geminiClient{openAIClient: c}, nil
	}
	return c, nil
}

// joinAPIPath joins base with an API segment, appending "/v1" first when
// the base path does not already end in "/v1".
func joinAPIPath(base, segment string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/" + strings.TrimPrefix(segment, "/")
	}
	return base + "/v1/" + strings.TrimPrefix(segment, "/")
}

// modelsURL is GET {base}/models (with the /v1 rule above).
func (c *openAIClient) modelsURL() string { return joinAPIPath(c.baseURL, "models") }

// chatURL is POST {base}/chat/completions (with the /v1 rule above).
func (c *openAIClient) chatURL() string {
	return joinAPIPath(c.baseURL, "chat/completions")
}

// maxErrorBody is how much of an error body we keep / read.
const maxErrorBody = 64 * 1024

// maxResponseBody caps non-streaming response bodies.
const maxResponseBody = 10 * 1024 * 1024

func (c *openAIClient) newRequest(ctx context.Context, method, url string, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	if c.authHeaders != nil {
		// Catalog-resolved auth: exactly these headers, no Bearer
		// default. An empty map means the surface takes no credential
		// (local runtimes); the stored key is simply not sent.
		for k, v := range c.authHeaders {
			req.Header.Set(k, v)
		}
	} else if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

// parseRetryAfter parses a Retry-After header value: integer seconds, a
// fractional-second value, or an HTTP date. Returns 0 when unparsable.
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second))
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

func isQuotaBody(body string) bool {
	lower := strings.ToLower(body)
	for _, marker := range []string{
		"insufficient_quota", "insufficient quota", "quota", "billing",
		"exceeded", "out of credits", "no credits",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// mapStatusError converts a non-2xx status + headers + body into the
// typed error for this package.
func mapStatusError(status int, h http.Header, body []byte) error {
	trimmed := string(body)
	if len(trimmed) > maxErrorBody {
		trimmed = trimmed[:maxErrorBody]
	}
	switch {
	case status == http.StatusTooManyRequests:
		return &RateLimitError{RetryAfter: parseRetryAfter(h), Body: trimmed}
	case status == http.StatusPaymentRequired ||
		(status >= 400 && status < 500 && isQuotaBody(trimmed)):
		msg := parseAPIErrorMessage(body)
		if msg == "" {
			msg = strings.TrimSpace(trimmed)
		}
		return &QuotaError{Message: msg, StatusCode: status, Body: trimmed}
	default:
		return &UpstreamError{
			StatusCode: status,
			Retryable:  status == http.StatusRequestTimeout || status >= 500,
			Body:       trimmed,
		}
	}
}

// apiErrorEnvelope handles both {"error": {"message": ...}} and
// {"error": "plain string"} shapes.
type apiErrorEnvelope struct {
	Error json.RawMessage `json:"error"`
}

type apiErrorObject struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// parseAPIErrorMessage extracts the human message from an upstream error
// body, or "" when there is none.
func parseAPIErrorMessage(body []byte) string {
	var env apiErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil || len(env.Error) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(env.Error, &asString); err == nil {
		return asString
	}
	var obj apiErrorObject
	if err := json.Unmarshal(env.Error, &obj); err == nil {
		return obj.Message
	}
	return ""
}

// do performs the request and returns the body bytes for a 2xx response,
// or a typed error otherwise. Network errors are returned as-is (the
// caller / router treats them as retryable transport failures).
func (c *openAIClient) do(req *http.Request) ([]byte, http.Header, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, nil, fmt.Errorf("reading upstream response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.Header, mapStatusError(resp.StatusCode, resp.Header, body)
	}
	return body, resp.Header, nil
}

// chatCompletionRequest is the wire shape for /v1/chat/completions.
type chatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Stream      bool             `json:"stream"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	TopP        float64          `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
}

// openAIToolCallDelta is one tool-call delta inside a streaming chunk.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// chatDelta is the delta of one streaming choice.
type chatDelta struct {
	Role      string                `json:"role,omitempty"`
	Content   string                `json:"content,omitempty"`
	ToolCalls []openAIToolCallDelta `json:"tool_calls,omitempty"`
}

// chatCompletionChoice is one choice of a chat completion, streaming
// or not: Message for the full reply, Delta for a chunk.
type chatCompletionChoice struct {
	Message      Message   `json:"message"`
	Delta        chatDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type chatCompletionResponse struct {
	ID      string                 `json:"id"`
	Model   string                 `json:"model"`
	Choices []chatCompletionChoice `json:"choices"`
	Usage   Usage                  `json:"usage"`
}

// toChatCompletionRequest converts a provider ChatRequest to the wire shape.
func toChatCompletionRequest(req ChatRequest) chatCompletionRequest {
	return chatCompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      req.Stream,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}
}

// MergeToolCallDeltas folds streaming tool-call deltas into the
// accumulated calls (keyed by index): the first delta sets ID/Name,
// later deltas append argument fragments.
func MergeToolCallDeltas(calls []ToolCall, deltas []ChunkToolCall) []ToolCall {
	for _, d := range deltas {
		for len(calls) <= d.Index {
			calls = append(calls, ToolCall{})
		}
		call := &calls[d.Index]
		if d.ID != "" {
			call.ID = d.ID
		}
		if call.Type == "" {
			call.Type = "function"
		}
		if d.Name != "" {
			call.Function.Name = d.Name
		}
		call.Function.Arguments += d.Arguments
	}
	return calls
}

func (c *openAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	r := req
	r.Stream = false // Chat is the non-streaming call, whatever the caller set.
	payload, err := json.Marshal(toChatCompletionRequest(r))
	if err != nil {
		return nil, fmt.Errorf("encoding chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
	if err != nil {
		return nil, err
	}
	body, _, err := c.do(httpReq)
	if err != nil {
		return nil, err
	}
	var decoded chatCompletionResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed chat response: " + err.Error()}
	}
	if len(decoded.Choices) == 0 {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "upstream returned no choices"}
	}
	choice := decoded.Choices[0]
	return &ChatResponse{
		Content:      choice.Message.Content,
		Usage:        decoded.Usage,
		Model:        decoded.Model,
		ToolCalls:    choice.Message.ToolCalls,
		FinishReason: choice.FinishReason,
	}, nil
}

// ChatStream is the text-only streaming entry point: it drains
// ChatStreamChunks and forwards only the content deltas. Chunk-aware
// callers (router, server) use ChatStreamChunks directly.
func (c *openAIClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
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

// ChatStreamChunks performs a streaming chat completion, calling
// onChunk once per SSE chunk in order with the full chunk (text delta,
// tool-call deltas, finish reason, usage). The returned ChatResponse
// holds the reassembled result. If onChunk returns an error,
// streaming stops and that error is returned.
func (c *openAIClient) ChatStreamChunks(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onChunk == nil {
		onChunk = func(StreamChunk) error { return nil }
	}
	payload, err := json.Marshal(toChatCompletionRequest(ChatRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      true,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.Stop,
		Tools:       req.Tools,
		ToolChoice:  req.ToolChoice,
	}))
	if err != nil {
		return nil, fmt.Errorf("encoding chat request: %w", err)
	}
	httpReq, err := c.newRequest(ctx, http.MethodPost, c.chatURL(), payload)
	if err != nil {
		return nil, err
	}
	// No client-level timeout for streaming: a long stream must not be cut
	// off by http.Client.Timeout. The context still bounds the request.
	streamClient := *c.httpClient
	streamClient.Timeout = 0
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, mapStatusError(resp.StatusCode, resp.Header, body)
	}
	var (
		full         strings.Builder
		model        string
		usage        Usage
		toolCalls    []ToolCall
		finishReason string
		done         bool
	)
	emit := func(chunk StreamChunk) error {
		if chunk.Content != "" {
			full.WriteString(chunk.Content)
		}
		if len(chunk.ToolCalls) > 0 {
			toolCalls = MergeToolCallDeltas(toolCalls, chunk.ToolCalls)
		}
		if chunk.FinishReason != "" {
			finishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		return onChunk(chunk)
	}
	scanner := bufio.NewScanner(resp.Body)
	// Deltas can be large; allow up to 1 MiB per SSE line.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // blank line or SSE comment
		}
		if strings.HasPrefix(line, "event:") {
			continue // event names carry no content for chat completions
		}
		if !strings.HasPrefix(line, "data:") {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed SSE line: " + line}
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			done = true
			break
		}
		var chunkResp chatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunkResp); err != nil {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed SSE data: " + data}
		}
		if chunkResp.Model != "" {
			model = chunkResp.Model
		}
		// Some providers send usage in the final chunk.
		if chunkResp.Usage.TotalTokens != 0 || chunkResp.Usage.PromptTokens != 0 || chunkResp.Usage.CompletionTokens != 0 {
			usage = chunkResp.Usage
		}
		if len(chunkResp.Choices) == 0 {
			continue
		}
		choice := chunkResp.Choices[0]
		chunk := StreamChunk{Content: choice.Delta.Content, FinishReason: choice.FinishReason}
		for _, tc := range choice.Delta.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, ChunkToolCall{
				Index:     tc.Index,
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
			})
		}
		if chunkResp.Usage.TotalTokens != 0 || chunkResp.Usage.PromptTokens != 0 || chunkResp.Usage.CompletionTokens != 0 {
			u := chunkResp.Usage
			chunk.Usage = &u
		}
		if chunk.Content == "" && len(chunk.ToolCalls) == 0 && chunk.FinishReason == "" && chunk.Usage == nil {
			continue
		}
		if err := emit(chunk); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "error reading SSE stream: " + err.Error()}
	}
	if !done && full.Len() == 0 && len(toolCalls) == 0 {
		// A stream that ends without [DONE] and without any content is
		// treated as a truncated upstream response, not a success.
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: true, Body: "SSE stream ended without [DONE]"}
	}
	return &ChatResponse{Content: full.String(), Usage: usage, Model: model, ToolCalls: toolCalls, FinishReason: finishReason}, nil
}

type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (c *openAIClient) ListModels(ctx context.Context) ([]string, error) {
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

func (c *openAIClient) TestKey(ctx context.Context) error {
	_, err := c.ListModels(ctx)
	return err
}
