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
)

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is a chat completion request. MaxTokens 0 means "provider
// default"; Temperature 0 means "omit, use provider default".
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

// Usage is token usage reported by the upstream.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse is a completed (non-streaming or reassembled streaming)
// chat completion.
type ChatResponse struct {
	Content string `json:"content"`
	Usage   Usage  `json:"usage"`
	Model   string `json:"model"`
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

// openAIClient is the Client implementation for OpenAI-compatible upstreams.
type openAIClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// authHeaders, when non-nil, are the exact auth headers to send
	// (resolved per provider + endpoint by the catalog). Nil means the
	// legacy default: Authorization: Bearer <apiKey>.
	authHeaders map[string]string
}

// NewClient builds a Client for an OpenAI-compatible upstream. baseURL is
// the API root: it may already end in "/v1" (e.g. https://api.codvian.com/v1)
// or be a bare host root (e.g. https://api.openai.com); the client appends
// "/v1/..." only when the path does not already end in "/v1".
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
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message      Message `json:"message"`
		Delta        Message `json:"delta"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

func (c *openAIClient) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	payload, err := json.Marshal(chatCompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      false,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
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
	return &ChatResponse{
		Content: decoded.Choices[0].Message.Content,
		Usage:   decoded.Usage,
		Model:   decoded.Model,
	}, nil
}

func (c *openAIClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string) error) (*ChatResponse, error) {
	if req.Model == "" {
		return nil, errors.New("chat request model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("chat request has no messages")
	}
	if onDelta == nil {
		onDelta = func(string) error { return nil }
	}
	payload, err := json.Marshal(chatCompletionRequest{
		Model:       req.Model,
		Messages:    req.Messages,
		Stream:      true,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	})
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
		full  strings.Builder
		model string
		usage Usage
		done  bool
	)
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
		var chunk chatCompletionResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "malformed SSE data: " + data}
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		// Some providers send usage in the final chunk.
		if chunk.Usage.TotalTokens != 0 || chunk.Usage.PromptTokens != 0 || chunk.Usage.CompletionTokens != 0 {
			usage = chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		delta := chunk.Choices[0].Delta.Content
		if delta == "" {
			continue
		}
		full.WriteString(delta)
		if err := onDelta(delta); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: false, Body: "error reading SSE stream: " + err.Error()}
	}
	if !done && full.Len() == 0 {
		// A stream that ends without [DONE] and without any content is
		// treated as a truncated upstream response, not a success.
		return nil, &UpstreamError{StatusCode: http.StatusOK, Retryable: true, Body: "SSE stream ended without [DONE]"}
	}
	return &ChatResponse{Content: full.String(), Usage: usage, Model: model}, nil
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
