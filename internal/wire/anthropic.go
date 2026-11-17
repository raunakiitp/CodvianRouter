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
// messages that only carry tool calls).
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// ToolDefinition is one OpenAI "function" tool definition.
type ToolDefinition struct {
	Type     string `json:"type"` // must be "function"
	Function struct {
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

// Usage is OpenAI-shaped token usage.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ResponseChoice is one choice of an OpenAI chat completion.
type ResponseChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatCompletionResponse is an OpenAI /v1/chat/completions response.
type ChatCompletionResponse struct {
	ID      string           `json:"id"`
	Object  string           `json:"object"`
	Created int64            `json:"created"`
	Model   string           `json:"model"`
	Choices []ResponseChoice `json:"choices"`
	Usage   Usage            `json:"usage"`
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

// AnthropicBlock is one Anthropic content block. Only the fields for
// its Type are populated.
type AnthropicBlock struct {
	Type string `json:"type"` // "text" | "image" | "tool_use" | "tool_result" | "thinking" | "redacted_thinking"
	// Text blocks.
	Text string `json:"text,omitempty"`
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
}

// AnthropicToolChoice is the tool_choice of an Anthropic request.
type AnthropicToolChoice struct {
	Type string `json:"type"` // "auto" | "any" | "tool"
	Name string `json:"name,omitempty"`
}

// AnthropicRequest is the body of POST /v1/messages.
type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        string               `json:"system,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
}

// AnthropicUsage is the usage block of an Anthropic response.
type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
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
	Retryable  bool
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
// fractional-second value, or an HTTP date. Returns 0 when unparsable.
func parseRetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
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

// MapError converts a non-2xx Anthropic HTTP response (status, headers,
// body) into an *AnthropicError that preserves the status code and the
// Anthropic error type. 429 stays 429-class and 5xx stay 5xx so the
// router's failover triggers correctly; overloaded_error is retryable
// whether it arrives as 529 (Anthropic) or 503 (some compatible
// providers). A 4xx whose type is billing_error is not retryable.
func MapError(status int, headers http.Header, body []byte) error {
	errType, message := parseAnthropicErrorBody(body)
	retryable := false
	switch {
	case status == http.StatusTooManyRequests || errType == "rate_limit_error":
		retryable = true
	case errType == "overloaded_error" || errType == "timeout_error":
		retryable = true
	case status >= 500 || status == http.StatusRequestTimeout:
		retryable = true
	}
	return &AnthropicError{
		StatusCode: status,
		Type:       errType,
		Message:    message,
		RetryAfter: parseRetryAfter(headers),
		Retryable:  retryable,
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
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			// Anthropic has no developer role; OpenAI's developer
			// message is a system instruction, so it joins the
			// system prompt.
			text, err := messageText(m, i)
			if err != nil {
				return nil, err
			}
			if text != "" {
				system = append(system, text)
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
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
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
				blocks = append(blocks, AnthropicBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || strings.TrimSpace(p.ImageURL.URL) == "" {
				return nil, unrep("messages[%d].content[%d] is an image_url part with no URL", index, i)
			}
			b, err := convertImagePart(p.ImageURL.URL, index, i)
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
// Anthropic accepts); http(s) URLs become url sources.
func convertImagePart(url string, msgIndex, partIndex int) (AnthropicBlock, error) {
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
			Type:   "image",
			Source: &AnthropicImageSource{Type: "base64", MediaType: mediaType, Data: data},
		}, nil
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return AnthropicBlock{
			Type:   "image",
			Source: &AnthropicImageSource{Type: "url", URL: url},
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
			// Internal reasoning: no OpenAI equivalent, dropped.
		default:
			return nil, unrep("response content[%d].type %q has no OpenAI equivalent", i, b.Type)
		}
	}
	stopReason := ""
	if resp.StopReason != nil {
		stopReason = *resp.StopReason
	}
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
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
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
