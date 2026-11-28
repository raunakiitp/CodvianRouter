// Package wire converts between the OpenAI chat-completions wire format
// (the format the gateway speaks to clients) and native provider wire
// formats. This file implements the Anthropic Messages API
// (POST /v1/messages) side.
//
// The package is a pure conversion library: it performs no I/O, holds no
// credentials, and knows nothing about HTTP. Auth headers for Anthropic
// surfaces come from catalog.ResolveAuth (Bearer + anthropic-version),
// and the actual HTTP call stays in internal/provider.
//
// Direction of travel:
//
//	OpenAI ChatCompletionRequest --ConvertRequest--> AnthropicRequest
//	AnthropicResponse           --ConvertResponse--> ChatCompletionResponse
//	Anthropic SSE stream        --TranslateStream--> ChatChunk stream
//	Anthropic error JSON        --MapError/MapStreamError--> AnthropicError
//
// Anything that cannot be represented faithfully on the Anthropic side
// returns *UnrepresentableError instead of silently producing wrong
// output. Anything the upstream returns that cannot be represented on
// the OpenAI side does the same on the way back.
package wire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// DefaultMaxTokens is used when the OpenAI request omits max_tokens.
// Anthropic requires max_tokens on every request; there is no
// "provider default" on the wire.
const DefaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// OpenAI side (chat-completions shape, as the gateway receives it).
// These types are wire-local: provider.ChatRequest is a flat
// {role, content} shape and cannot carry tool calls, image parts, or
// tool definitions, so the conversion layer defines the full shape
// here. The integration step maps the gateway's request onto these.
// ---------------------------------------------------------------------------

// ContentPart is one element of an OpenAI multi-part message content.
type ContentPart struct {
	Type string `json:"type"` // "text" | "image_url" | ...
	Text string `json:"text,omitempty"`
	// CacheControl, when set, is a client-supplied prompt-cache
	// breakpoint for this part (G13): OpenAI-compatible upstreams
	// that honour it (OpenRouter's chat-completions API for
	// Claude/Qwen/Gemini models) pin the prefix up to this part.
	// The gateway decodes it, preserves it through every wire
	// conversion (Anthropic blocks carry it verbatim), and never
	// overwrites it with its own auto-marker — a client that
	// asked for a breakpoint keeps exactly the one it asked for.
	// It is omitted when nil, so parts without a marker encode
	// exactly as before.
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
	// ImageURL is set for "image_url" parts. Detail (low/high/auto) has
	// no Anthropic equivalent and is ignored on conversion.
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
}

// ImageURLPart is the payload of an "image_url" content part.
type ImageURLPart struct {
	URL string `json:"url"`
}

// ToolCall is one OpenAI tool call (assistant message or response).
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the function payload of a ToolCall.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-encoded object
}

// ChatMessage is one OpenAI chat message. Content is kept raw because
// it may be a plain string, an array of ContentPart, or null (assistant
// messages that only carry tool calls). Reasoning carries the
// assistant turn's captured reasoning state, when the router
// restored one for this message (or the client supplied one):
// it is json:"-" — router-internal, never serialised into a
// coalescing key, a cache key, a trace, or a client-facing body —
// and each wire converter translates it into its own shape (or
// refuses the hop honestly) via reasoning.Check.
type ChatMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	ToolCalls  []ToolCall       `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	Reasoning  *reasoning.State `json:"-"`
}

// ToolDefinition is one OpenAI "function" tool definition.
type ToolDefinition struct {
	Type string `json:"type"` // must be "function"
	// CacheControl, when set, is a client-supplied prompt-cache
	// breakpoint on this tool definition (G13): Anthropic accepts
	// cache_control on tool definitions, and OpenRouter's
	// chat-completions API honours it for Claude/Qwen/Gemini
	// models. Preserved through conversion (convertTools copies
	// it onto AnthropicTool) and never overwritten by the
	// gateway's own auto-marker.
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
	Function     struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// ChatCompletionRequest is an OpenAI /v1/chat/completions request.
// Temperature and TopP are pointers so "absent" stays distinguishable
// from an explicit 0. MaxTokens 0 means absent (Anthropic default
// DefaultMaxTokens is applied). Stop accepts a string or an array of
// strings on decode.
type ChatCompletionRequest struct {
	Model       string           `json:"model"`
	Messages    []ChatMessage    `json:"messages"`
	Stream      bool             `json:"stream,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	// ToolChoice accepts the OpenAI forms: "none" | "auto" | "required"
	// or {"type":"function","function":{"name":...}}.
	ToolChoice any `json:"tool_choice,omitempty"`
}

// chatCompletionRequestJSON is the decode shape for ChatCompletionRequest
// (Stop may be a string or a string array on the wire).
type chatCompletionRequestJSON struct {
	Model       string           `json:"model"`
	Messages    []ChatMessage    `json:"messages"`
	Stream      bool             `json:"stream,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        json.RawMessage  `json:"stop,omitempty"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	ToolChoice  any              `json:"tool_choice,omitempty"`
}

// UnmarshalJSON decodes a ChatCompletionRequest, accepting Stop as either
// a single string or an array of strings.
func (r *ChatCompletionRequest) UnmarshalJSON(data []byte) error {
	var raw chatCompletionRequestJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*r = ChatCompletionRequest{
		Model:       raw.Model,
		Messages:    raw.Messages,
		Stream:      raw.Stream,
		MaxTokens:   raw.MaxTokens,
		Temperature: raw.Temperature,
		TopP:        raw.TopP,
		Tools:       raw.Tools,
		ToolChoice:  raw.ToolChoice,
	}
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
		return fmt.Errorf("wire: invalid stop value: %w", err)
	}
	r.Stop = multi
	return nil
}

// Usage is OpenAI-shaped token usage. CachedTokens is the
// provider-reported count of prompt tokens served from the
// provider's prompt cache (Anthropic's cache_read_input_tokens,
// Gemini's cachedContentTokenCount) — 0 when the upstream did not
// report any. It is a subset of PromptTokens, never a separate
// charge; the ledger stores it only alongside a known split.
// CacheWriteTokens is Anthropic's cache_creation_input_tokens:
// tokens written into the prompt cache on this call.
// CacheWriteTokens5m / CacheWriteTokens1h (M6.9b) split that total
// by TTL tier: tokens written at the 5-minute / 1-hour ephemeral
// tier (Anthropic usage.cache_creation ephemeral_5m_input_tokens
// / ephemeral_1h_input_tokens). Both 0 with a positive
// CacheWriteTokens means the upstream reported only the legacy
// total — the tier is unknown, never guessed.
// ReasoningTokens is the provider-reported count of completion
// tokens spent on internal reasoning (Gemini's thoughtsTokenCount)
// — a subset of CompletionTokens; 0 when the wire reports none.
type Usage struct {
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	TotalTokens        int `json:"total_tokens"`
	CachedTokens       int `json:"cached_tokens,omitempty"`
	CacheWriteTokens   int `json:"cache_write_tokens,omitempty"`
	CacheWriteTokens5m int `json:"cache_write_tokens_5m,omitempty"`
	CacheWriteTokens1h int `json:"cache_write_tokens_1h,omitempty"`
	ReasoningTokens    int `json:"reasoning_tokens,omitempty"`
}

// ResponseChoice is one choice of an OpenAI chat completion.
type ResponseChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionResponse is an OpenAI /v1/chat/completions response.
// Reasoning carries the reasoning state captured from the upstream
// response (Anthropic thinking blocks, Gemini thought signatures,
// Bedrock reasoningContent), when any was present: it is json:"-"
// — router-internal, never sent to the client — and the engine
// stores it in the replay store so the next turn can restore it.
type ChatCompletionResponse struct {
	ID        string           `json:"id"`
	Object    string           `json:"object"`
	Created   int64            `json:"created"`
	Model     string           `json:"model"`
	Choices   []ResponseChoice `json:"choices"`
	Usage     Usage            `json:"usage"`
	Reasoning *reasoning.State `json:"-"`
}

// ---------------------------------------------------------------------------
// Anthropic side (Messages API, POST /v1/messages).
// ---------------------------------------------------------------------------

// AnthropicImageSource is the source of an Anthropic image block.
type AnthropicImageSource struct {
	Type      string `json:"type"` // "base64" | "url"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// AnthropicCacheControl is the cache_control block Anthropic accepts on
// a content block to pin the prefix up to and including that block in
// its prompt cache (POST /v1/messages). Type is always "ephemeral"
// (the 5-minute TTL is the provider default; TTL "1h" buys the
// 1-hour tier at a 2x write cost — this router never sets it, see
// provider.applyPromptCacheMarker). The block sits on the content
// block itself, never nested inside tool_result.content (Anthropic
// rejects that with a 400), never on thinking/redacted_thinking
// blocks, and at most 4 blocks per request may carry one
// (Anthropic limit; this router sets at most one). Verified shape
// against the the sector baseline reference (study only):
// the earlier implementation markMessageCacheControl
// / markLastCacheableBlock set block.cache_control =
// {type: "ephemeral"} (with optional ttl) on the last cacheable
// block of a message's content array — markLastCacheableBlock walks
// back past thinking and blank blocks (markMessageCacheControl marks
// the last block unconditionally); and against OpenRouter's
// prompt-caching docs (openrouter.ai/docs/guides/best-practices/
// prompt-caching): `"cache_control": { "type": "ephemeral" }` on a
// content block, 5-minute default, `"ttl": "1h"` for the 1-hour tier,
// max 4 explicit breakpoints per request. This router marks text
// blocks only (never image/tool_result) and gives up if the last
// user message has no text block rather than falling back to an
// earlier user turn — conservative by design.
type AnthropicCacheControl struct {
	Type string `json:"type"`          // "ephemeral"
	TTL  string `json:"ttl,omitempty"` // "5m" | "1h"; empty = provider default (5m)
}

// AnthropicBlock is one Anthropic content block. Only the fields for
// its Type are populated. Thinking blocks carry their thinking text
// in Thinking and their cryptographic signature in Signature — the
// signature is what makes a thinking block replayable: the API
// rejects thinking blocks without the signature it issued.
// Redacted thinking carries its encrypted payload in Data.
// CacheControl, when set, is the prompt-cache breakpoint for this
// block (see AnthropicCacheControl); it is set by the Anthropic
// provider client's auto-marker (M6.9c/G13) or preserved verbatim
// from a client-supplied marker by wire.ConvertRequest (G13) —
// ConvertRequest never invents a marker, but it must not drop one
// the client sent either.
type AnthropicBlock struct {
	Type string `json:"type"` // "text" | "image" | "tool_use" | "tool_result" | "thinking" | "redacted_thinking"
	// Text blocks.
	Text string `json:"text,omitempty"`
	// Prompt-cache breakpoint for this block (M6.9c).
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
	// Thinking blocks.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Redacted-thinking blocks.
	Data string `json:"data,omitempty"`
	// Image blocks.
	Source *AnthropicImageSource `json:"source,omitempty"`
	// Tool-use blocks.
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// Tool-result blocks.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// AnthropicMessage is one message in an Anthropic request. Content is
// always sent as an array of blocks (the string form is never needed).
type AnthropicMessage struct {
	Role    string           `json:"role"` // "user" | "assistant"
	Content []AnthropicBlock `json:"content"`
}

// AnthropicTool is one tool definition for an Anthropic request.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	// CacheControl, when set, is the prompt-cache breakpoint for
	// this tool definition (G13): Anthropic pins the prefix up
	// to and including the marked tool, so a long tool list is
	// cached like the system prompt and the last user message.
	// Set only from a client-supplied marker (copied by
	// convertTools) or by the Anthropic provider client's
	// auto-marker — never by wire.ConvertRequest, which stays a
	// pure conversion.
	CacheControl *AnthropicCacheControl `json:"cache_control,omitempty"`
}

// AnthropicToolChoice is the tool_choice of an Anthropic request.
type AnthropicToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool"
	Name string `json:"name,omitempty"`
}

// AnthropicRequest is the body of POST /v1/messages.
type AnthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []AnthropicMessage `json:"messages"`
	System    string             `json:"system,omitempty"`
	// SystemBlocks is the system prompt in block form (G13): the
	// Anthropic API accepts `system` as a string OR as an array
	// of text blocks, and only the block form can carry a
	// cache_control breakpoint. ConvertRequest fills System
	// (string form) for plain prompts; it fills SystemBlocks
	// only when a system part carries a client-supplied
	// cache_control, and the provider client's auto-marker fills
	// it when it marks the system prompt. MarshalJSON emits the
	// block form when SystemBlocks is set, the string form
	// otherwise, so unmarked requests encode exactly as before;
	// UnmarshalJSON accepts both forms. The two fields always
	// agree on text: System is the joined text of SystemBlocks
	// when blocks are present.
	SystemBlocks  []AnthropicBlock     `json:"-"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	// Thinking enables extended thinking (CHECKLIST 285-286):
	// the numeric budget the operator declared for the
	// attempt's effort in config `reasoning.effort_budgets`.
	// Nil means no thinking field is sent — the provider
	// default applies. Set only by the Anthropic client from
	// ChatRequest.ThinkingBudget, never by the wire converter
	// (the converter must not invent a budget for an effort).
	Thinking *AnthropicThinking `json:"thinking,omitempty"`
}

// MarshalJSON emits `system` as an array of blocks when
// SystemBlocks is set (the only form that can carry a
// cache_control breakpoint), and as the plain string otherwise
// — unmarked requests encode byte-identically to before.
func (r AnthropicRequest) MarshalJSON() ([]byte, error) {
	if len(r.SystemBlocks) == 0 {
		type Alias AnthropicRequest
		return json.Marshal(Alias(r))
	}
	type Alias AnthropicRequest
	aux := struct {
		Alias
		System []AnthropicBlock `json:"system"`
	}{Alias: Alias(r), System: r.SystemBlocks}
	return json.Marshal(aux)
}

// UnmarshalJSON accepts `system` as a string or as an array of
// blocks. The block form populates SystemBlocks and joins its
// text into System, so readers that only look at System keep
// working; the string form populates System only.
func (r *AnthropicRequest) UnmarshalJSON(data []byte) error {
	type Alias AnthropicRequest
	aux := struct {
		Alias
		System json.RawMessage `json:"system"`
	}{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = AnthropicRequest(aux.Alias)
	if len(aux.System) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(aux.System, &s); err == nil {
		r.System = s
		return nil
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(aux.System, &blocks); err != nil {
		return err
	}
	r.SystemBlocks = blocks
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	r.System = strings.Join(texts, "\n\n")
	return nil
}

// AnthropicThinking is the `thinking` block of a Messages request:
// {"type": "enabled", "budget_tokens": N}.
type AnthropicThinking struct {
	Type         string `json:"type"` // "enabled"
	BudgetTokens int    `json:"budget_tokens"`
}

// AnthropicCacheCreation is the usage.cache_creation object of an
// Anthropic response: the cache-write split by TTL tier. It is
// present only when the upstream reports the split (the 1-hour TTL
// is in play); older responses carry only the legacy
// cache_creation_input_tokens total and no cache_creation object.
// Field names verified against the shape as parsed by the
// the sector baseline reference (the earlier implementation:
// usage.cache_creation.ephemeral_5m_input_tokens and
// ephemeral_1h_input_tokens) and corroborated by Anthropic usage
// examples in the wild.
type AnthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens,omitempty"`
}

// AnthropicUsage is the usage block of an Anthropic response.
// CacheReadInputTokens is Anthropic's provider-reported count of
// input tokens served from its prompt cache;
// CacheCreationInputTokens the tokens written into the cache on
// this call (the cache-write side of prompt-cache economics).
// CacheCreation, when present, splits that total by TTL tier
// (see AnthropicCacheCreation); nil means the upstream reported
// only the legacy total.
type AnthropicUsage struct {
	InputTokens              int                     `json:"input_tokens"`
	OutputTokens             int                     `json:"output_tokens"`
	CacheReadInputTokens     int                     `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int                     `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *AnthropicCacheCreation `json:"cache_creation,omitempty"`
}

// CacheWriteTiers returns the 5-minute / 1-hour cache-write split
// from usage.cache_creation, or 0/0 when the upstream did not
// report the split object — the legacy total-only case.
func (u AnthropicUsage) CacheWriteTiers() (int, int) {
	if u.CacheCreation == nil {
		return 0, 0
	}
	return u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens
}

// AnthropicResponse is the body of a non-streaming /v1/messages reply.
type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Content      []AnthropicBlock `json:"content"`
	Model        string           `json:"model"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// UnrepresentableError is returned when an input cannot be represented
// faithfully on the other wire format. It is a loud failure by design:
// the caller must not fall back to a lossy translation.
type UnrepresentableError struct {
	Reason string
}

func (e *UnrepresentableError) Error() string {
	if e == nil {
		return "wire: unrepresentable input"
	}
	return "wire: cannot represent in target format: " + e.Reason
}

// unrep builds an UnrepresentableError.
func unrep(format string, args ...any) error {
	return &UnrepresentableError{Reason: fmt.Sprintf(format, args...)}
}

// AnthropicError is an error returned by the Anthropic API, preserving
// the HTTP status and the Anthropic error type. Retryable is true for
// the transient classes the router must fail over on: 429
// (rate_limit_error), 529/503 overloaded_error, timeout_error, and any
// other 5xx. StatusCode is 0 for errors surfaced mid-stream (the HTTP
// status was already 200 when the stream's error event arrived).
type AnthropicError struct {
	StatusCode int
	Type       string
	Message    string
	RetryAfter time.Duration
	// RetryAfterPresent is true when a Retry-After header was
	// present and parsed (including an explicit "0") — see
	// provider.RateLimitError: a present 0 is a poisoned hint
	// the router floors, an absent hint means backoff.
	RetryAfterPresent bool
	Retryable         bool
}

func (e *AnthropicError) Error() string {
	if e == nil {
		return "wire: anthropic error"
	}
	if e.StatusCode != 0 {
		if e.Message != "" {
			return fmt.Sprintf("anthropic error: HTTP %d (%s): %s", e.StatusCode, e.Type, e.Message)
		}
		return fmt.Sprintf("anthropic error: HTTP %d (%s)", e.StatusCode, e.Type)
	}
	if e.Message != "" {
		return fmt.Sprintf("anthropic error (%s): %s", e.Type, e.Message)
	}
	return fmt.Sprintf("anthropic error (%s)", e.Type)
}

// IsRateLimit reports whether this is a 429 / rate_limit_error.
func (e *AnthropicError) IsRateLimit() bool {
	return e != nil && (e.StatusCode == http.StatusTooManyRequests || e.Type == "rate_limit_error")
}

// IsOverloaded reports whether this is an overloaded_error (529, or a
// 503 carrying that type, as some Anthropic-compatible providers use).
func (e *AnthropicError) IsOverloaded() bool {
	return e != nil && e.Type == "overloaded_error"
}

// IsRetryable reports whether err is a *AnthropicError or
// *GeminiError marked retryable.
func IsRetryable(err error) bool {
	var ae *AnthropicError
	if errors.As(err, &ae) {
		return ae.Retryable
	}
	var ge *GeminiError
	return errors.As(err, &ge) && ge.Retryable
}

// HTTPStatus returns the preserved HTTP status of an *AnthropicError
// or *GeminiError, or 0 when err is neither (e.g. a mid-stream error
// event, where the HTTP status was already 200).
func HTTPStatus(err error) int {
	var ae *AnthropicError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	var ge *GeminiError
	if errors.As(err, &ge) {
		return ge.StatusCode
	}
	return 0
}

// anthropicErrorEnvelope is the {"type":"error","error":{...}} envelope.
type anthropicErrorEnvelope struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseAnthropicErrorBody extracts type/message from an Anthropic error
// body, tolerating the {"error": "plain string"} shape and non-JSON
// bodies.
func parseAnthropicErrorBody(body []byte) (errType, message string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", ""
	}
	var env anthropicErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Type != "" {
		return env.Error.Type, env.Error.Message
	}
	// {"error": "plain string"} shape.
	var alt struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &alt); err == nil && alt.Error != "" {
		return "", alt.Error
	}
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	return "", trimmed
}

// parseRetryAfter parses a Retry-After header: integer seconds, a
// fractional-second value, or an HTTP date. It reports whether the
// header was present and parsed: (0, true) for an explicit "0",
// (0, false) when absent or unparsable.
func parseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second, true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
		return time.Duration(f * float64(time.Second)), true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d, true
		}
	}
	return 0, false
}

// MapError converts a non-2xx Anthropic HTTP response (status, headers,
// body) into an *AnthropicError that preserves the status code and the
// Anthropic error type. 429 stays 429-class and 5xx stay 5xx so the
// router's failover triggers correctly; overloaded_error is retryable
// whether it arrives as 529 (Anthropic) or 503 (some compatible
// providers). 402/404 are retryable per the shared taxonomy
// (RetryableStatus): they are properties of that target, not of the
// request. A billing_error on any other 4xx (e.g. a 400) is not
// retryable — except a 400 whose type/message signals
// model-not-found (entry 151), which falls back like a 404.
func MapError(status int, headers http.Header, body []byte) error {
	errType, message := parseAnthropicErrorBody(body)
	retryable := false
	switch {
	case status == http.StatusTooManyRequests || errType == "rate_limit_error":
		retryable = true
	case errType == "overloaded_error" || errType == "timeout_error":
		retryable = true
	case status == http.StatusBadRequest && IsModelNotFoundSignal(errType+" "+message):
		// Entry 151: a 400 that means "model not found at this
		// target" falls back; any other 400 stops the chain.
		retryable = true
	case RetryableStatus(status):
		retryable = true
	}
	ra, raPresent := parseRetryAfter(headers)
	return &AnthropicError{
		StatusCode:        status,
		Type:              errType,
		Message:           message,
		RetryAfter:        ra,
		RetryAfterPresent: raPresent,
		Retryable:         retryable,
	}
}

// MapStreamError converts an Anthropic SSE "error" event payload
// ({"type":"error","error":{...}}) into an *AnthropicError. The HTTP
// status was already 200 when the stream started, so StatusCode is
// derived from the error type when it implies one (429 for
// rate_limit_error, 529 for overloaded_error) and 0 otherwise.
// Retryable follows the same transient classes as MapError.
func MapStreamError(body []byte) error {
	errType, message := parseAnthropicErrorBody(body)
	status := 0
	retryable := false
	switch errType {
	case "rate_limit_error":
		status, retryable = http.StatusTooManyRequests, true
	case "overloaded_error":
		status, retryable = 529, true
	case "timeout_error", "api_error":
		retryable = true
	}
	return &AnthropicError{
		StatusCode: status,
		Type:       errType,
		Message:    message,
		Retryable:  retryable,
	}
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Anthropic.
// ---------------------------------------------------------------------------

// imageMediaTypes are the image media types Anthropic accepts for
// base64 image sources.
var imageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
}

// ConvertRequest converts an OpenAI chat-completions request into an
// Anthropic /v1/messages request body. It returns *UnrepresentableError
// for anything that cannot be represented faithfully.
func ConvertRequest(req ChatCompletionRequest) (*AnthropicRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 1) {
		return nil, unrep("temperature %v is outside Anthropic's 0-1 range", *req.Temperature)
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return nil, unrep("top_p %v is outside the 0-1 range", *req.TopP)
	}
	// Reasoning continuity (CHECKLIST 163): every assistant
	// message's restored reasoning state must be representable
	// on this wire before anything is sent — a thinking block
	// without its Anthropic signature cannot be replayed, so
	// the hop fails here with *reasoning.UnrepresentableError
	// (the router fails over to a target that can carry it)
	// instead of silently dropping the reasoning.
	for i, m := range req.Messages {
		if m.Reasoning != nil && !m.Reasoning.Empty() {
			if _, _, err := reasoning.Check(m.Reasoning, reasoning.WireAnthropic, req.Model); err != nil {
				return nil, fmt.Errorf("messages[%d] reasoning: %w", i, err)
			}
		}
	}

	out := &AnthropicRequest{
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		Temperature:   req.Temperature,
		TopP:          req.TopP,
		StopSequences: append([]string(nil), req.Stop...),
		Stream:        req.Stream,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = DefaultMaxTokens
	}

	tools, dropTools, err := convertTools(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	if !dropTools {
		out.Tools = tools
		out.ToolChoice, err = convertToolChoice(req.ToolChoice, len(tools) > 0)
		if err != nil {
			return nil, err
		}
	}

	var system []string
	var systemBlocks []AnthropicBlock
	systemMarked := false
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			// Anthropic has no developer role; OpenAI's developer
			// message is a system instruction, so it joins the
			// system prompt.
			//
			// G13: a client-supplied cache_control on a system
			// part is preserved, not dropped — the system prompt
			// then travels in block form (out.SystemBlocks) so
			// the marker has somewhere to sit; without a client
			// marker the prompt stays the plain string form, so
			// unmarked requests convert exactly as before.
			blocks, err := convertContentParts(m.Content, i)
			if err != nil {
				return nil, err
			}
			for _, b := range blocks {
				if b.Type != "text" {
					return nil, unrep("messages[%d]: non-text content cannot appear in a %s message", i, m.Role)
				}
				if b.CacheControl != nil {
					systemMarked = true
				}
				if b.Text != "" {
					system = append(system, b.Text)
					systemBlocks = append(systemBlocks, b)
				}
			}
		default:
			role, blocks, err := convertMessage(m, i)
			if err != nil {
				return nil, err
			}
			out.Messages = appendMessage(out.Messages, role, blocks)
		}
	}
	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}
	if systemMarked {
		out.SystemBlocks = systemBlocks
	}
	if len(out.Messages) == 0 {
		return nil, unrep("no representable messages (all messages were empty)")
	}
	if out.Messages[0].Role != "user" {
		return nil, unrep("first message must be from the user, got role %q", out.Messages[0].Role)
	}
	return out, nil
}

// convertTools converts OpenAI tool definitions to Anthropic tools and
// reports whether tool_choice "none" asked for the tools to be dropped.
func convertTools(tools []ToolDefinition, toolChoice any) ([]AnthropicTool, bool, error) {
	if s, ok := toolChoice.(string); ok && s == "none" {
		return nil, true, nil
	}
	out := make([]AnthropicTool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, false, unrep("tools[%d].type %q is not \"function\"", i, t.Type)
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, false, unrep("tools[%d].function.name is empty", i)
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, AnthropicTool{
			Name:         t.Function.Name,
			Description:  t.Function.Description,
			InputSchema:  schema,
			CacheControl: t.CacheControl,
		})
	}
	return out, false, nil
}

// convertToolChoice maps an OpenAI tool_choice onto Anthropic's.
// hasTools reports whether any tools were defined.
func convertToolChoice(tc any, hasTools bool) (*AnthropicToolChoice, error) {
	if tc == nil {
		return nil, nil
	}
	if s, ok := tc.(string); ok {
		switch s {
		case "auto":
			return &AnthropicToolChoice{Type: "auto"}, nil
		case "required":
			if !hasTools {
				return nil, unrep("tool_choice \"required\" with no tools defined")
			}
			return &AnthropicToolChoice{Type: "any"}, nil
		case "none":
			// Handled in convertTools (tools are dropped); nothing to send.
			return nil, nil
		default:
			return nil, unrep("unknown tool_choice %q", s)
		}
	}
	m, ok := tc.(map[string]any)
	if !ok {
		return nil, unrep("tool_choice has unsupported shape %T", tc)
	}
	t, _ := m["type"].(string)
	switch t {
	case "auto", "any":
		if t == "any" && !hasTools {
			return nil, unrep("tool_choice \"any\" with no tools defined")
		}
		return &AnthropicToolChoice{Type: t}, nil
	case "tool":
		name, _ := m["name"].(string)
		if name == "" {
			return nil, unrep("tool_choice of type \"tool\" needs a name")
		}
		if !hasTools {
			return nil, unrep("tool_choice names tool %q but no tools are defined", name)
		}
		return &AnthropicToolChoice{Type: "tool", Name: name}, nil
	case "function":
		// OpenAI object form: {"type":"function","function":{"name":...}}.
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if name == "" {
			return nil, unrep("tool_choice function has no name")
		}
		if !hasTools {
			return nil, unrep("tool_choice names tool %q but no tools are defined", name)
		}
		return &AnthropicToolChoice{Type: "tool", Name: name}, nil
	default:
		return nil, unrep("unknown tool_choice type %q", t)
	}
}

// appendMessage appends blocks as a message, merging into the previous
// message when it has the same role. Anthropic rejects consecutive
// messages with the same role, and tool flows routinely produce them
// (several tool results in a row, or a tool result followed by a plain
// user turn), so merging is required for correctness.
func appendMessage(msgs []AnthropicMessage, role string, blocks []AnthropicBlock) []AnthropicMessage {
	if len(blocks) == 0 {
		return msgs
	}
	if n := len(msgs); n > 0 && msgs[n-1].Role == role {
		msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
		return msgs
	}
	return append(msgs, AnthropicMessage{Role: role, Content: blocks})
}

// convertMessage converts one non-system OpenAI message to an Anthropic
// role plus content blocks.
func convertMessage(m ChatMessage, index int) (string, []AnthropicBlock, error) {
	switch m.Role {
	case "user":
		blocks, err := convertContentParts(m.Content, index)
		if err != nil {
			return "", nil, err
		}
		return "user", blocks, nil
	case "assistant":
		blocks, err := convertContentParts(m.Content, index)
		if err != nil {
			return "", nil, err
		}
		for i, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return "", nil, unrep("messages[%d].tool_calls[%d].type %q is not \"function\"", index, i, tc.Type)
			}
			if strings.TrimSpace(tc.Function.Name) == "" {
				return "", nil, unrep("messages[%d].tool_calls[%d].function.name is empty", index, i)
			}
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return "", nil, unrep("messages[%d].tool_calls[%d].function.arguments is not valid JSON", index, i)
			}
			blocks = append(blocks, AnthropicBlock{
				Type:  "tool_use",
				ID:    tc.ID,
				Name:  tc.Function.Name,
				Input: json.RawMessage(args),
			})
		}
		// Restored reasoning leads the assistant message:
		// Anthropic requires thinking blocks before the text
		// and tool_use blocks that follow them, and each one
		// carries the signature the API issued with it.
		// reasoning.Check (in ConvertRequest) already proved
		// this state representable here.
		if m.Reasoning != nil && !m.Reasoning.Empty() {
			blocks = append(anthropicThinkingBlocks(m.Reasoning), blocks...)
		}
		return "assistant", blocks, nil
	case "tool":
		if strings.TrimSpace(m.ToolCallID) == "" {
			return "", nil, unrep("messages[%d] is a tool result with no tool_call_id", index)
		}
		text, err := messageText(m, index)
		if err != nil {
			return "", nil, err
		}
		return "user", []AnthropicBlock{{
			Type:      "tool_result",
			ToolUseID: m.ToolCallID,
			Content:   text,
		}}, nil
	default:
		return "", nil, unrep("messages[%d].role %q is not supported", index, m.Role)
	}
}

// anthropicThinkingBlocks renders a captured reasoning state as
// Anthropic thinking blocks, in order. Only the block kinds
// reasoning.Check admits on this wire reach here (thinking with a
// signature, redacted thinking); anything else was already refused
// by ConvertRequest's Check.
func anthropicThinkingBlocks(st *reasoning.State) []AnthropicBlock {
	if st == nil {
		return nil
	}
	out := make([]AnthropicBlock, 0, len(st.Blocks))
	for _, b := range st.Blocks {
		switch b.Kind {
		case reasoning.KindThinking:
			out = append(out, AnthropicBlock{Type: "thinking", Thinking: b.Text, Signature: b.Signature})
		case reasoning.KindRedactedThinking:
			var data string
			if len(b.Data) > 0 {
				// Data is the JSON-encoded redacted string.
				if err := json.Unmarshal(b.Data, &data); err != nil {
					data = string(b.Data)
				}
			}
			out = append(out, AnthropicBlock{Type: "redacted_thinking", Data: data})
		}
	}
	return out
}

// messageText extracts the plain-text content of a message. Multi-part
// content must be text-only; image parts in a system/tool message are
// unrepresentable.
func messageText(m ChatMessage, index int) (string, error) {
	blocks, err := convertContentParts(m.Content, index)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" {
			return "", unrep("messages[%d]: non-text content cannot appear in a %s message", index, m.Role)
		}
		sb.WriteString(b.Text)
	}
	return sb.String(), nil
}

// convertContentParts converts OpenAI message content (a string, an
// array of parts, or null) into Anthropic content blocks.
func convertContentParts(raw json.RawMessage, index int) ([]AnthropicBlock, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return nil, nil
		}
		return []AnthropicBlock{{Type: "text", Text: asString}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, unrep("messages[%d].content is neither a string nor a content-part array", index)
	}
	var blocks []AnthropicBlock
	for i, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				// G13: a client-supplied cache_control rides
				// along onto the Anthropic block verbatim — the
				// converter never invents a marker, but it must
				// not drop one the client sent either.
				blocks = append(blocks, AnthropicBlock{Type: "text", Text: p.Text, CacheControl: p.CacheControl})
			}
		case "image_url":
			if p.ImageURL == nil || strings.TrimSpace(p.ImageURL.URL) == "" {
				return nil, unrep("messages[%d].content[%d] is an image_url part with no URL", index, i)
			}
			// G13 (B2-35): a client-supplied cache_control rides
			// along onto the image block verbatim, exactly as on
			// text parts above — Anthropic accepts cache_control
			// on image blocks, and dropping it here would silently
			// lose a breakpoint the client asked for.
			b, err := convertImagePart(p.ImageURL.URL, index, i, p.CacheControl)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b)
		default:
			return nil, unrep("messages[%d].content[%d].type %q is not supported (only \"text\" and \"image_url\")", index, i, p.Type)
		}
	}
	return blocks, nil
}

// convertImagePart converts one OpenAI image_url value to an Anthropic
// image block. Data URLs become base64 sources (media type must be one
// Anthropic accepts); http(s) URLs become url sources. A client-supplied
// cache_control (nil when absent) rides along onto the block verbatim
// (G13, B2-35) — Anthropic accepts cache_control on image blocks.
func convertImagePart(url string, msgIndex, partIndex int, cacheControl *AnthropicCacheControl) (AnthropicBlock, error) {
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		semi := strings.Index(rest, ";base64,")
		if semi < 0 {
			return AnthropicBlock{}, unrep("messages[%d].content[%d] data URL is not a base64 image (want data:<media-type>;base64,...)", msgIndex, partIndex)
		}
		mediaType := rest[:semi]
		data := rest[semi+len(";base64,"):]
		if !imageMediaTypes[mediaType] {
			return AnthropicBlock{}, unrep("messages[%d].content[%d] image media type %q is not supported by Anthropic (jpeg, png, gif, webp)", msgIndex, partIndex, mediaType)
		}
		if strings.TrimSpace(data) == "" {
			return AnthropicBlock{}, unrep("messages[%d].content[%d] data URL has empty image data", msgIndex, partIndex)
		}
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return AnthropicBlock{}, unrep("messages[%d].content[%d] image data is not valid base64", msgIndex, partIndex)
		}
		return AnthropicBlock{
			Type:         "image",
			CacheControl: cacheControl,
			Source:       &AnthropicImageSource{Type: "base64", MediaType: mediaType, Data: data},
		}, nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return AnthropicBlock{
			Type:         "image",
			CacheControl: cacheControl,
			Source:       &AnthropicImageSource{Type: "url", URL: url},
		}, nil
	}
	return AnthropicBlock{}, unrep("messages[%d].content[%d] image URL scheme is not supported (want data: or http(s):)", msgIndex, partIndex)
}

// ---------------------------------------------------------------------------
// Response conversion: Anthropic -> OpenAI.
// ---------------------------------------------------------------------------

// MapFinishReason maps an Anthropic stop_reason onto an OpenAI
// finish_reason. end_turn, stop_sequence, pause_turn and refusal all
// end the turn normally ("stop"); max_tokens and
// model_context_window_exceeded ran out of room ("length"); tool_use
// means the model wants tools run ("tool_calls"). An empty or unknown
// reason maps to "stop" — the turn ended, and inventing a more
// specific reason would be guessing.
func MapFinishReason(stopReason string) string {
	switch stopReason {
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "":
		return "stop"
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// ConvertResponse converts a non-streaming Anthropic message response
// into an OpenAI chat-completion response. Thinking blocks
// ("thinking", "redacted_thinking") are internal reasoning with no
// OpenAI equivalent and are dropped by design; any other block type
// the OpenAI shape cannot carry (e.g. server_tool_use from
// Anthropic's built-in tools) returns *UnrepresentableError.
func ConvertResponse(resp *AnthropicResponse) (*ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("anthropic response is nil")
	}
	if resp.Type != "" && resp.Type != "message" {
		return nil, unrep("anthropic response type %q is not \"message\"", resp.Type)
	}
	if resp.Role != "" && resp.Role != "assistant" {
		return nil, unrep("anthropic response role %q is not \"assistant\"", resp.Role)
	}
	var (
		text      strings.Builder
		toolCalls []ToolCall
		state     *reasoning.State
	)
	for i, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			if strings.TrimSpace(b.Name) == "" {
				return nil, unrep("response content[%d] is a tool_use block with no name", i)
			}
			args := strings.TrimSpace(string(b.Input))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, unrep("response content[%d] tool_use input is not valid JSON", i)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "thinking", "redacted_thinking":
			// Reasoning capture (CHECKLIST 163): thinking blocks
			// are internal reasoning — they must NOT be copied
			// into the visible message — but they are captured
			// verbatim (text + signature) on the response so the
			// engine can store them and replay them on the next
			// turn or after a failover. A thinking block without
			// a signature is not replayable; it is still captured
			// (the next hop's Check refuses it honestly rather
			// than silently dropping it here).
			if state == nil {
				state = &reasoning.State{SourceWire: reasoning.WireAnthropic, SourceModel: resp.Model}
			}
			if b.Type == "thinking" {
				state.Add(reasoning.Block{Kind: reasoning.KindThinking, Text: b.Thinking, Signature: b.Signature})
			} else {
				data, _ := json.Marshal(b.Data)
				state.Add(reasoning.Block{Kind: reasoning.KindRedactedThinking, Data: data})
			}
		default:
			return nil, unrep("response content[%d].type %q has no OpenAI equivalent", i, b.Type)
		}
	}
	stopReason := ""
	if resp.StopReason != nil {
		stopReason = *resp.StopReason
	}
	write5m, write1h := resp.Usage.CacheWriteTiers()
	msg := ChatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = mustJSON(text.String())
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return &ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.Model,
		Choices: []ResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: MapFinishReason(stopReason),
		}},
		Usage: Usage{
			PromptTokens:       resp.Usage.InputTokens,
			CompletionTokens:   resp.Usage.OutputTokens,
			TotalTokens:        resp.Usage.InputTokens + resp.Usage.OutputTokens,
			CachedTokens:       resp.Usage.CacheReadInputTokens,
			CacheWriteTokens:   resp.Usage.CacheCreationInputTokens,
			CacheWriteTokens5m: write5m,
			CacheWriteTokens1h: write1h,
		},
		Reasoning: state,
	}, nil
}

// mustJSON marshals a string to its JSON encoding (infallible for a string).
func mustJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail.
		return json.RawMessage(`""`)
	}
	return b
}
