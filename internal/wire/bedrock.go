// Bedrock native-wire conversion (Converse API, POST
// /model/{modelId}/converse and /model/{modelId}/converse-stream).
//
//	OpenAI ChatCompletionRequest --ConvertBedrockRequest--> BedrockRequest
//	BedrockResponse             --ConvertBedrockResponse--> ChatCompletionResponse
//	ConverseStream events       --TranslateBedrockStream--> ChatChunk stream
//	Bedrock error JSON          --MapBedrockError--> BedrockError
//
// The Converse API surface used here is the Bedrock runtime endpoint
// (bedrock-runtime.{region}.amazonaws.com). The request body carries
// messages (typed content blocks), an optional system prompt array,
// inferenceConfig and toolConfig; the model ID rides in the URL path,
// not the body. Streaming is NOT SSE: ConverseStream returns an
// Amazon event-stream (binary framing, content-type
// application/vnd.amazon.eventstream) whose events are
// messageStart / contentBlockStart / contentBlockDelta /
// contentBlockStop / messageStop / metadata, plus exception events.
// This file parses that framing directly (see readEventStream).
//
// Shapes verified against AWS docs (docs.aws.amazon.com/bedrock,
// API_reference Converse / ConverseStream and the Converse user
// guide, read 2026-11-17):
//   - Message content blocks: {text}, {image:{format,source:{bytes}}},
//     {toolUse:{toolUseId,name,input}}, {toolResult:{toolUseId,
//     content:[{text}|{json}],status}}.
//   - inferenceConfig: maxTokens, temperature, topP, stopSequences.
//   - toolConfig: tools:[{toolSpec:{name,description,inputSchema:
//     {json}}}], toolChoice {auto:{}} | {any:{}} | {tool:{name}}.
//   - Response: output.message.content (text / toolUse /
//     reasoningContent blocks), stopReason end_turn | tool_use |
//     max_tokens | stop_sequence | guardrail_intervened |
//     content_filtered, usage {inputTokens, outputTokens, totalTokens}.
//   - Errors are {"message": "..."} with the exception type in the
//     x-amzn-ErrorType header (e.g. ThrottlingException).
//
// Mapping notes, all deliberate and loud on failure
// (*UnrepresentableError, never silent wrong output):
//   - Converse requires alternating user/assistant roles starting
//     with a user message, so consecutive same-role messages are
//     merged; a conversation starting with an assistant message is
//     unrepresentable.
//   - Tool results ride in a user message as {toolResult} blocks; a
//     "tool" message with no tool_call_id is unrepresentable.
//   - Images must be data: URLs (format from the media type, bytes
//     passed through as base64); http(s) image URLs cannot be fetched
//     by Converse and are unrepresentable.
//   - reasoningContent blocks in a response are dropped (internal
//     reasoning, no OpenAI equivalent), like Anthropic thinking.
//   - stop_sequence and guardrail/content-filter stops map to "stop".
//   - ThrottlingException (and any 429) is a rate limit; 5xx / 408
//     are retryable; other 4xx are not.
package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Bedrock side (Converse API).
// ---------------------------------------------------------------------------

// BedrockImageSource is the source of a Converse image block: the
// image bytes as a base64 string.
type BedrockImageSource struct {
	Bytes string `json:"bytes"`
}

// BedrockImage is an image content block's {format, source} payload.
type BedrockImage struct {
	Format string             `json:"format"` // "png" | "jpeg" | "gif" | "webp"
	Source BedrockImageSource `json:"source"`
}

// BedrockToolUse is a toolUse content block.
type BedrockToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
}

// BedrockToolResultContent is one block inside a toolResult's content.
type BedrockToolResultContent struct {
	Text string          `json:"text,omitempty"`
	JSON json.RawMessage `json:"json,omitempty"`
}

// BedrockToolResult is a toolResult content block.
type BedrockToolResult struct {
	ToolUseID string                     `json:"toolUseId"`
	Content   []BedrockToolResultContent `json:"content,omitempty"`
	Status    string                     `json:"status,omitempty"` // "success" | "error"
}

// BedrockContentBlock is one typed block in a Converse message (or in
// the response message). Only the fields for its Type are populated;
// the wire uses single-key objects ({"text": ...}, {"toolUse": ...}),
// so the block is kept as raw JSON keyed by type.
type BedrockContentBlock struct {
	Text             *string            `json:"text,omitempty"`
	Image            *BedrockImage      `json:"image,omitempty"`
	ToolUse          *BedrockToolUse    `json:"toolUse,omitempty"`
	ToolResult       *BedrockToolResult `json:"toolResult,omitempty"`
	ReasoningContent json.RawMessage    `json:"reasoningContent,omitempty"`
}

// BedrockMessage is one message in a Converse request/response.
type BedrockMessage struct {
	Role    string                `json:"role"` // "user" | "assistant"
	Content []BedrockContentBlock `json:"content"`
}

// BedrockSystemBlock is one system-prompt block.
type BedrockSystemBlock struct {
	Text string `json:"text,omitempty"`
}

// BedrockInferenceConfig is the inferenceConfig of a Converse request.
type BedrockInferenceConfig struct {
	MaxTokens     int      `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// BedrockToolSpec is the toolSpec of one tool definition.
type BedrockToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

// BedrockTool is one tool in toolConfig.tools.
type BedrockTool struct {
	ToolSpec BedrockToolSpec `json:"toolSpec"`
}

// BedrockToolChoice is the toolChoice of a Converse request: exactly
// one of auto / any / tool is set.
type BedrockToolChoice struct {
	Auto *struct{} `json:"auto,omitempty"`
	Any  *struct{} `json:"any,omitempty"`
	Tool *struct {
		Name string `json:"name"`
	} `json:"tool,omitempty"`
}

// BedrockToolConfig is the toolConfig of a Converse request.
type BedrockToolConfig struct {
	Tools      []BedrockTool      `json:"tools"`
	ToolChoice *BedrockToolChoice `json:"toolChoice,omitempty"`
}

// BedrockRequest is the body of POST /model/{modelId}/converse (and
// converse-stream). The model ID is in the URL path, not the body.
type BedrockRequest struct {
	Messages        []BedrockMessage        `json:"messages"`
	System          []BedrockSystemBlock    `json:"system,omitempty"`
	InferenceConfig *BedrockInferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *BedrockToolConfig      `json:"toolConfig,omitempty"`
}

// BedrockUsage is the usage block of a Converse response.
type BedrockUsage struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// BedrockResponse is the body of a non-streaming Converse reply.
type BedrockResponse struct {
	Output struct {
		Message BedrockMessage `json:"message"`
	} `json:"output"`
	StopReason string       `json:"stopReason,omitempty"`
	Usage      BedrockUsage `json:"usage,omitempty"`
	Metrics    *struct {
		LatencyMs int `json:"latencyMs,omitempty"`
	} `json:"metrics,omitempty"`
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// BedrockError is an error returned by the Bedrock API, preserving the
// HTTP status and the exception type. Retryable is true for the
// transient classes the router must fail over on: 429,
// ThrottlingException, 408, and any 5xx. StatusCode is 0 for errors
// surfaced mid-stream (the HTTP status was already 200 when the
// exception event arrived).
type BedrockError struct {
	StatusCode int
	Type       string // e.g. "ThrottlingException"
	Message    string
	RetryAfter time.Duration
	Retryable  bool
}

func (e *BedrockError) Error() string {
	if e == nil {
		return "wire: bedrock error"
	}
	if e.StatusCode != 0 {
		if e.Type != "" && e.Message != "" {
			return fmt.Sprintf("bedrock error: HTTP %d (%s): %s", e.StatusCode, e.Type, e.Message)
		}
		if e.Message != "" {
			return fmt.Sprintf("bedrock error: HTTP %d: %s", e.StatusCode, e.Message)
		}
		if e.Type != "" {
			return fmt.Sprintf("bedrock error: HTTP %d (%s)", e.StatusCode, e.Type)
		}
		return fmt.Sprintf("bedrock error: HTTP %d", e.StatusCode)
	}
	if e.Type != "" && e.Message != "" {
		return fmt.Sprintf("bedrock error (%s): %s", e.Type, e.Message)
	}
	if e.Message != "" {
		return fmt.Sprintf("bedrock error: %s", e.Message)
	}
	return "wire: bedrock error"
}

// IsRateLimit reports whether this is a 429 or a throttling exception.
func (e *BedrockError) IsRateLimit() bool {
	if e == nil {
		return false
	}
	if e.StatusCode == http.StatusTooManyRequests {
		return true
	}
	return strings.Contains(e.Type, "Throttl")
}

// IsBedrockRetryable reports whether err is a *BedrockError marked retryable.
func IsBedrockRetryable(err error) bool {
	var be *BedrockError
	return errors.As(err, &be) && be.Retryable
}

// parseBedrockErrorBody extracts the message and exception type from a
// Bedrock error body. The REST shape is {"message": "..."} (with an
// optional "__type" field); a plain-text body is used as-is.
func parseBedrockErrorBody(body []byte) (string, string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", ""
	}
	var decoded struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil {
		msg := decoded.Message
		if msg == "" {
			msg = decoded.Error
		}
		return msg, decoded.Type
	}
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	return trimmed, ""
}

// bedrockErrorType returns the exception type from the
// x-amzn-ErrorType header (preferred) or the body, stripping any
// namespace prefix ("com.amazonaws.bedrock#ThrottlingException" ->
// "ThrottlingException").
func bedrockErrorType(headers http.Header, bodyType string) string {
	t := headers.Get("x-amzn-ErrorType")
	if t == "" {
		t = headers.Get("X-Amzn-ErrorType")
	}
	if t == "" {
		t = bodyType
	}
	if i := strings.Index(t, "#"); i >= 0 {
		t = t[i+1:]
	}
	if i := strings.Index(t, ":"); i >= 0 {
		t = t[:i]
	}
	return strings.TrimSpace(t)
}

// MapBedrockError converts a non-2xx Bedrock HTTP response (status,
// headers, body) into a *BedrockError that preserves the status code.
// 429 / ThrottlingException stays rate-limit class and 5xx stay 5xx
// so the router's failover triggers correctly; a 4xx whose body
// mentions quota/billing is not retryable (it is a quota problem, and
// the provider maps it to QuotaError by message).
func MapBedrockError(status int, headers http.Header, body []byte) error {
	message, bodyType := parseBedrockErrorBody(body)
	errType := bedrockErrorType(headers, bodyType)
	retryable := false
	switch {
	case status == http.StatusTooManyRequests:
		retryable = true
	case status == http.StatusRequestTimeout:
		retryable = true
	case status >= 500:
		retryable = true
	case strings.Contains(errType, "Throttl"):
		retryable = true
	case errType == "ModelTimeoutException" || errType == "ServiceUnavailableException":
		retryable = true
	}
	return &BedrockError{
		StatusCode: status,
		Type:       errType,
		Message:    message,
		RetryAfter: parseRetryAfter(headers),
		Retryable:  retryable,
	}
}

// MapBedrockStreamError converts a ConverseStream exception event
// (exception type + message) into a *BedrockError. The HTTP status
// was already 200 when the stream started, so StatusCode is 0;
// retryability is derived from the exception type.
func MapBedrockStreamError(exceptionType, message string) error {
	errType := exceptionType
	if i := strings.Index(errType, "#"); i >= 0 {
		errType = errType[i+1:]
	}
	retryable := strings.Contains(errType, "Throttl") ||
		errType == "ModelTimeoutException" ||
		errType == "ServiceUnavailableException" ||
		errType == "ModelStreamErrorException"
	return &BedrockError{
		StatusCode: 0,
		Type:       errType,
		Message:    message,
		Retryable:  retryable,
	}
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Bedrock Converse.
// ---------------------------------------------------------------------------

// ConvertBedrockRequest converts an OpenAI chat-completions request
// into a Bedrock Converse request body. It returns
// *UnrepresentableError for anything that cannot be represented
// faithfully (see the package notes).
func ConvertBedrockRequest(req ChatCompletionRequest) (*BedrockRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}

	tools, toolChoice, err := convertBedrockTools(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}

	out := &BedrockRequest{}
	if len(tools) > 0 {
		out.ToolConfig = &BedrockToolConfig{Tools: tools, ToolChoice: toolChoice}
	} else if toolChoice != nil {
		// "none" with no tools: nothing to configure; any other
		// choice without tools was already rejected above.
		_ = toolChoice
	}
	if req.MaxTokens > 0 || req.Temperature != nil || req.TopP != nil || len(req.Stop) > 0 {
		out.InferenceConfig = &BedrockInferenceConfig{
			MaxTokens:     req.MaxTokens,
			Temperature:   req.Temperature,
			TopP:          req.TopP,
			StopSequences: append([]string(nil), req.Stop...),
		}
	}

	// Merge consecutive same-role messages: Converse requires
	// strictly alternating user/assistant roles, starting with user.
	type mergedMsg struct {
		role   string
		blocks []BedrockContentBlock
	}
	var merged []mergedMsg
	for i, m := range req.Messages {
		role, blocks, err := convertBedrockMessage(m, i)
		if err != nil {
			return nil, err
		}
		if role == "system" {
			// System/developer messages become system blocks.
			for _, b := range blocks {
				if b.Text != nil {
					out.System = append(out.System, BedrockSystemBlock{Text: *b.Text})
				}
			}
			continue
		}
		if len(merged) > 0 && merged[len(merged)-1].role == role {
			merged[len(merged)-1].blocks = append(merged[len(merged)-1].blocks, blocks...)
		} else {
			merged = append(merged, mergedMsg{role: role, blocks: blocks})
		}
	}
	if len(merged) == 0 {
		return nil, unrep("no representable messages (all messages were system-only or empty)")
	}
	if merged[0].role != "user" {
		return nil, unrep("Bedrock Converse requires the conversation to start with a user message, got %q", merged[0].role)
	}
	for _, m := range merged {
		if len(m.blocks) == 0 {
			return nil, unrep("a %s message with no content cannot be represented on Bedrock Converse", m.role)
		}
		out.Messages = append(out.Messages, BedrockMessage{Role: m.role, Content: m.blocks})
	}
	return out, nil
}

// convertBedrockTools converts OpenAI tool definitions to Converse
// toolConfig tools and the matching toolChoice. OpenAI "auto" (and an
// absent tool_choice) maps to {auto:{}}; "none" maps to no toolConfig
// at all (handled by the caller omitting ToolConfig when there are no
// tools — here a "none" with tools means "do not call", which
// Converse expresses by omitting toolChoice while still declaring the
// tools, so the model sees them but is not forced); "required" maps
// to {any:{}}; a named function maps to {tool:{name}}.
func convertBedrockTools(tools []ToolDefinition, toolChoice any) ([]BedrockTool, *BedrockToolChoice, error) {
	if len(tools) == 0 {
		if toolChoice == nil {
			return nil, nil, nil
		}
		if s, ok := toolChoice.(string); ok && (s == "none" || s == "auto") {
			return nil, nil, nil
		}
		return nil, nil, unrep("tool_choice %v with no tools defined", toolChoice)
	}
	bt := make([]BedrockTool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, nil, unrep("tools[%d].type %q is not \"function\"", i, t.Type)
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, nil, unrep("tools[%d].function.name is empty", i)
		}
		schema := map[string]any{"type": "object"}
		if len(t.Function.Parameters) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(t.Function.Parameters, &decoded); err != nil {
				return nil, nil, unrep("tools[%d].function.parameters is not a valid JSON object", i)
			}
			schema = decoded
		}
		bt = append(bt, BedrockTool{ToolSpec: BedrockToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: map[string]any{"json": schema},
		}})
	}
	tc, err := convertBedrockToolChoice(toolChoice, bt)
	if err != nil {
		return nil, nil, err
	}
	return bt, tc, nil
}

// convertBedrockToolChoice maps an OpenAI tool_choice onto a Converse
// toolChoice. A nil/absent choice means "auto".
func convertBedrockToolChoice(tc any, tools []BedrockTool) (*BedrockToolChoice, error) {
	if tc == nil {
		return &BedrockToolChoice{Auto: &struct{}{}}, nil
	}
	if s, ok := tc.(string); ok {
		switch s {
		case "auto":
			return &BedrockToolChoice{Auto: &struct{}{}}, nil
		case "none":
			// No choice constraint: omit toolChoice while the tools
			// stay declared (the model may still call them, but is
			// not forced — Converse has no "never call" choice).
			return nil, nil
		case "required":
			return &BedrockToolChoice{Any: &struct{}{}}, nil
		default:
			return nil, unrep("unknown tool_choice %q", s)
		}
	}
	if m, ok := tc.(map[string]any); ok {
		t, _ := m["type"].(string)
		switch t {
		case "auto":
			return &BedrockToolChoice{Auto: &struct{}{}}, nil
		case "none":
			return nil, nil
		case "required", "any":
			return &BedrockToolChoice{Any: &struct{}{}}, nil
		case "function", "tool":
			name := ""
			if t == "tool" {
				name, _ = m["name"].(string)
			} else if fn, ok := m["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				return nil, unrep("tool_choice of type %q needs a function name", t)
			}
			for _, tool := range tools {
				if tool.ToolSpec.Name == name {
					return &BedrockToolChoice{Tool: &struct {
						Name string `json:"name"`
					}{Name: name}}, nil
				}
			}
			return nil, unrep("tool_choice names tool %q but it is not defined", name)
		default:
			return nil, unrep("unknown tool_choice type %q", t)
		}
	}
	return nil, unrep("tool_choice has unsupported shape %T", tc)
}

// convertBedrockMessage converts one OpenAI message to a Converse role
// + content blocks. System/developer messages return role "system"
// (their text is folded into the request's system array by the
// caller); every other role returns "user" or "assistant".
func convertBedrockMessage(m ChatMessage, index int) (string, []BedrockContentBlock, error) {
	switch m.Role {
	case "system", "developer":
		text, err := bedrockMessageText(m, index)
		if err != nil {
			return "", nil, err
		}
		if text == "" {
			return "system", nil, nil
		}
		return "system", []BedrockContentBlock{{Text: &text}}, nil
	case "user":
		blocks, err := convertBedrockParts(m, index)
		if err != nil {
			return "", nil, err
		}
		return "user", blocks, nil
	case "assistant":
		blocks, err := convertBedrockParts(m, index)
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
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(args), &raw); err != nil {
				return "", nil, unrep("messages[%d].tool_calls[%d].function.arguments is not valid JSON", index, i)
			}
			id := tc.ID
			if id == "" {
				return "", nil, unrep("messages[%d].tool_calls[%d] has no id (Converse toolUse requires toolUseId)", index, i)
			}
			blocks = append(blocks, BedrockContentBlock{ToolUse: &BedrockToolUse{
				ToolUseID: id,
				Name:      tc.Function.Name,
				Input:     raw,
			}})
		}
		return "assistant", blocks, nil
	case "tool":
		if strings.TrimSpace(m.ToolCallID) == "" {
			return "", nil, unrep("messages[%d] is a tool result with no tool_call_id", index)
		}
		text, err := bedrockMessageText(m, index)
		if err != nil {
			return "", nil, err
		}
		return "user", []BedrockContentBlock{{ToolResult: &BedrockToolResult{
			ToolUseID: m.ToolCallID,
			Content:   []BedrockToolResultContent{{Text: text}},
			Status:    "success",
		}}}, nil
	default:
		return "", nil, unrep("messages[%d].role %q is not supported", index, m.Role)
	}
}

// bedrockMessageText extracts the plain-text content of a message.
// Multi-part content must be text-only here; image parts in a
// system/tool message are unrepresentable.
func bedrockMessageText(m ChatMessage, index int) (string, error) {
	blocks, err := convertBedrockParts(m, index)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Text == nil {
			return "", unrep("messages[%d]: non-text content cannot appear in a %s message", index, m.Role)
		}
		sb.WriteString(*b.Text)
	}
	return sb.String(), nil
}

// convertBedrockParts converts OpenAI message content (a string, an
// array of parts, or null) into Converse content blocks. Image blocks
// must be data: URLs: the format comes from the media type and the
// base64 bytes pass through unchanged. http(s) image URLs cannot be
// fetched by Converse and are unrepresentable.
func convertBedrockParts(m ChatMessage, index int) ([]BedrockContentBlock, error) {
	trimmed := strings.TrimSpace(string(m.Content))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		if asString == "" {
			return nil, nil
		}
		return []BedrockContentBlock{{Text: &asString}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(m.Content, &parts); err != nil {
		return nil, unrep("messages[%d].content is neither a string nor a content-part array", index)
	}
	var out []BedrockContentBlock
	for i, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				text := p.Text
				out = append(out, BedrockContentBlock{Text: &text})
			}
		case "image_url":
			if p.ImageURL == nil || strings.TrimSpace(p.ImageURL.URL) == "" {
				return nil, unrep("messages[%d].content[%d] is an image_url part with no URL", index, i)
			}
			format, data, err := bedrockImageData(p.ImageURL.URL)
			if err != nil {
				return nil, unrep("messages[%d].content[%d]: %v", index, i, err)
			}
			out = append(out, BedrockContentBlock{Image: &BedrockImage{
				Format: format,
				Source: BedrockImageSource{Bytes: data},
			}})
		default:
			return nil, unrep("messages[%d].content[%d].type %q is not supported (only \"text\" and \"image_url\")", index, i, p.Type)
		}
	}
	return out, nil
}

// bedrockImageData parses a data: image URL into a Converse format
// and the base64 payload. Non-data URLs are rejected: Converse
// cannot fetch remote images.
func bedrockImageData(url string) (string, string, error) {
	if !strings.HasPrefix(url, "data:") {
		return "", "", fmt.Errorf("only data: image URLs are supported (got a remote URL)")
	}
	rest := strings.TrimPrefix(url, "data:")
	meta, data, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(meta, ";base64") {
		return "", "", fmt.Errorf("data: image URL is not base64-encoded")
	}
	mediaType := strings.TrimSuffix(meta, ";base64")
	var format string
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		format = "jpeg"
	case "image/png":
		format = "png"
	case "image/gif":
		format = "gif"
	case "image/webp":
		format = "webp"
	default:
		return "", "", fmt.Errorf("unsupported image media type %q (Converse supports png, jpeg, gif, webp)", mediaType)
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return "", "", fmt.Errorf("image data is not valid base64: %v", err)
	}
	return format, data, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Bedrock Converse -> OpenAI.
// ---------------------------------------------------------------------------

// MapBedrockStopReason maps a Converse stopReason onto an OpenAI
// finish_reason. end_turn / stop_sequence / guardrail_intervened /
// content_filtered all end the turn ("stop"); tool_use means the
// model called tools; max_tokens means truncation ("length"). An
// empty or unknown reason maps to "stop" — the turn ended, and
// inventing a more specific reason would be guessing.
func MapBedrockStopReason(stopReason string) string {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "end_turn", "stop_sequence", "guardrail_intervened", "content_filtered", "":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// ConvertBedrockResponse converts a non-streaming Converse response
// into an OpenAI chat-completion response. reasoningContent blocks
// are dropped (internal reasoning, no OpenAI equivalent); a
// document block or any other unrepresentable content returns
// *UnrepresentableError.
func ConvertBedrockResponse(resp *BedrockResponse) (*ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("bedrock response is nil")
	}
	var (
		text      strings.Builder
		toolCalls []ToolCall
	)
	for i, b := range resp.Output.Message.Content {
		switch {
		case b.Text != nil:
			text.WriteString(*b.Text)
		case b.ToolUse != nil:
			if strings.TrimSpace(b.ToolUse.Name) == "" {
				return nil, unrep("response content[%d] is a toolUse block with no name", i)
			}
			args := strings.TrimSpace(string(b.ToolUse.Input))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, unrep("response toolUse %q input is not valid JSON", b.ToolUse.Name)
			}
			toolCalls = append(toolCalls, ToolCall{
				ID:   b.ToolUse.ToolUseID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      b.ToolUse.Name,
					Arguments: args,
				},
			})
		case b.ReasoningContent != nil:
			// Internal reasoning, dropped by design.
		case b.Image != nil:
			return nil, unrep("response content[%d] is an image block with no OpenAI equivalent", i)
		default:
			return nil, unrep("response content[%d] has an unknown shape with no OpenAI equivalent", i)
		}
	}
	msg := ChatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = mustJSON(text.String())
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	return &ChatCompletionResponse{
		ID:      "",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "",
		Choices: []ResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: MapBedrockStopReason(resp.StopReason),
		}},
		Usage: Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

// ---------------------------------------------------------------------------
// Streaming: ConverseStream event stream -> OpenAI chat-completion chunks.
// ---------------------------------------------------------------------------

// bedrockStreamEvent is one parsed ConverseStream event: the
// :event-type header value and the JSON payload.
type bedrockStreamEvent struct {
	Type    string
	Payload json.RawMessage
}

// readEventStream parses Amazon event-stream framing (the binary
// framing ConverseStream returns, content-type
// application/vnd.amazon.eventstream) and calls fn once per event
// with its :event-type and JSON payload. Frames are CRC-checked
// (prelude + message CRC); a bad CRC is a loud error, never a
// silently accepted corrupt stream.
//
// Frame layout: total length (4B BE), headers length (4B BE),
// prelude CRC (4B, CRC32 of the first 8 bytes), headers, payload,
// message CRC (4B, CRC32 of everything before it). A header is:
// name length (1B), name, value type (1B), value. Value types:
// 0 bool false, 1 bool true, 2 byte, 3 short, 4 int, 5 long,
// 6 bytes, 7 string, 8 timestamp, 9 uuid.
func readEventStream(r io.Reader, fn func(bedrockStreamEvent) error) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("wire: error reading Bedrock event stream: %w", err)
	}
	pos := 0
	for pos < len(data) {
		if len(data)-pos < 12 {
			return errors.New("wire: truncated Bedrock event-stream frame (prelude)")
		}
		totalLen := int(binary.BigEndian.Uint32(data[pos : pos+4]))
		headersLen := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		preludeCRC := binary.BigEndian.Uint32(data[pos+8 : pos+12])
		if totalLen < 16 || pos+totalLen > len(data) {
			return errors.New("wire: truncated Bedrock event-stream frame (message)")
		}
		if crc32.ChecksumIEEE(data[pos:pos+8]) != preludeCRC {
			return errors.New("wire: Bedrock event-stream prelude CRC mismatch")
		}
		msgEnd := pos + totalLen
		if crc32.ChecksumIEEE(data[pos:msgEnd-4]) != binary.BigEndian.Uint32(data[msgEnd-4:msgEnd]) {
			return errors.New("wire: Bedrock event-stream message CRC mismatch")
		}
		headers, err := parseEventStreamHeaders(data[pos+12 : pos+12+headersLen])
		if err != nil {
			return err
		}
		payload := data[pos+12+headersLen : msgEnd-4]
		evType := headers[":event-type"]
		msgType := headers[":message-type"]
		if msgType == "exception" {
			exType := headers[":exception-type"]
			var decoded struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(payload, &decoded)
			if decoded.Message == "" {
				decoded.Message = headers[":message"]
			}
			return MapBedrockStreamError(exType, decoded.Message)
		}
		if evType == "" {
			return errors.New("wire: Bedrock event-stream frame has no :event-type header")
		}
		if err := fn(bedrockStreamEvent{Type: evType, Payload: bytes.Clone(payload)}); err != nil {
			return err
		}
		pos = msgEnd
	}
	return nil
}

// parseEventStreamHeaders parses the header section of one
// event-stream frame into name -> string value (only string headers
// are kept; other value types are skipped by length).
func parseEventStreamHeaders(b []byte) (map[string]string, error) {
	out := make(map[string]string)
	pos := 0
	for pos < len(b) {
		if pos+1 > len(b) {
			return nil, errors.New("wire: truncated Bedrock event-stream header")
		}
		nameLen := int(b[pos])
		pos++
		if pos+nameLen+1 > len(b) {
			return nil, errors.New("wire: truncated Bedrock event-stream header")
		}
		name := string(b[pos : pos+nameLen])
		pos += nameLen
		valueType := b[pos]
		pos++
		switch valueType {
		case 0, 1: // bool: no value bytes
			out[name] = map[bool]string{false: "false", true: "true"}[valueType == 1]
		case 2: // byte
			if pos+1 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos++
		case 3: // short
			if pos+2 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos += 2
		case 4: // int
			if pos+4 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos += 4
		case 5: // long
			if pos+8 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos += 8
		case 6: // bytes
			if pos+2 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			n := int(binary.BigEndian.Uint16(b[pos : pos+2]))
			pos += 2 + n
			if pos > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
		case 7: // string
			if pos+2 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			n := int(binary.BigEndian.Uint16(b[pos : pos+2]))
			pos += 2
			if pos+n > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			out[name] = string(b[pos : pos+n])
			pos += n
		case 8: // timestamp
			if pos+8 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos += 8
		case 9: // uuid
			if pos+16 > len(b) {
				return nil, errors.New("wire: truncated Bedrock event-stream header value")
			}
			pos += 16
		default:
			return nil, fmt.Errorf("wire: unknown Bedrock event-stream header value type %d", valueType)
		}
	}
	return out, nil
}

// bedrockStreamDelta is the delta of one contentBlockDelta event.
type bedrockStreamDelta struct {
	Text string `json:"text,omitempty"`
	// Tool-use input fragments arrive under "toolUse".
	ToolUse *struct {
		Input string `json:"input,omitempty"`
	} `json:"toolUse,omitempty"`
	ReasoningContent *struct {
		Text string `json:"text,omitempty"`
	} `json:"reasoningContent,omitempty"`
}

// bedrockStreamEventJSON is the JSON envelope of one ConverseStream
// event payload. Only the fields each event type carries are
// populated.
type bedrockStreamEventJSON struct {
	Role string `json:"role,omitempty"`
	// contentBlockStart.
	ContentBlockIndex int `json:"contentBlockIndex,omitempty"`
	Start             *struct {
		ToolUse *struct {
			ToolUseID string `json:"toolUseId,omitempty"`
			Name      string `json:"name,omitempty"`
		} `json:"toolUse,omitempty"`
	} `json:"start,omitempty"`
	// contentBlockDelta.
	Delta *bedrockStreamDelta `json:"delta,omitempty"`
	// messageStop.
	StopReason string `json:"stopReason,omitempty"`
	// metadata.
	Usage *struct {
		InputTokens  int `json:"inputTokens,omitempty"`
		OutputTokens int `json:"outputTokens,omitempty"`
		TotalTokens  int `json:"totalTokens,omitempty"`
	} `json:"usage,omitempty"`
}

// TranslateBedrockStream reads a ConverseStream event-stream body and
// emits OpenAI chat.completion.chunk values via emit, in order. It
// returns the reassembled result when messageStop arrives.
//
//   - contentBlockDelta text becomes a content delta; toolUse input
//     fragments become tool-call argument deltas, validated as JSON
//     when the block closes; reasoningContent deltas are dropped.
//   - contentBlockStart opens one tool call per toolUse block (id +
//     name, first delta for the call); contentBlockStop validates the
//     accumulated arguments as JSON (empty becomes "{}").
//   - messageStop supplies the stop reason; metadata supplies usage;
//     the final emitted chunk carries finish_reason and usage.
//   - an exception event returns the mapped *BedrockError and never a
//     success result; a stream that ends without messageStop returns
//     an error (truncated), never a partial success; a stream with no
//     events at all returns an error (empty).
func TranslateBedrockStream(r io.Reader, emit func(ChatChunk) error) (StreamResult, error) {
	if emit == nil {
		emit = func(ChatChunk) error { return nil }
	}
	var res StreamResult
	created := time.Now().Unix()
	blocks := make(map[int]*streamBlock)
	toolIndex := make(map[int]int)
	nextTool := 0
	var (
		content    strings.Builder
		sawEvent   bool
		sawStop    bool
		stopReason string
	)
	emitChunk := func(delta ChunkDelta, finishReason *string, usage *Usage) error {
		return emit(ChatChunk{
			ID:      res.ID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   res.Model,
			Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
			Usage:   usage,
		})
	}

	err := readEventStream(r, func(ev bedrockStreamEvent) error {
		sawEvent = true
		var parsed bedrockStreamEventJSON
		if err := json.Unmarshal(ev.Payload, &parsed); err != nil {
			return fmt.Errorf("wire: malformed Bedrock stream event %q: %w", ev.Type, err)
		}
		switch ev.Type {
		case "messageStart":
			return emitChunk(ChunkDelta{Role: "assistant"}, nil, nil)
		case "contentBlockStart":
			blk := &streamBlock{kind: "text"}
			if parsed.Start != nil && parsed.Start.ToolUse != nil {
				tu := parsed.Start.ToolUse
				if strings.TrimSpace(tu.Name) == "" {
					return unrep("streamed toolUse block has no name")
				}
				blk.kind = "tool_use"
				blk.toolID = tu.ToolUseID
				blk.toolName = tu.Name
				toolIndex[parsed.ContentBlockIndex] = nextTool
				nextTool++
				if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
					Index: toolIndex[parsed.ContentBlockIndex],
					ID:    blk.toolID,
					Type:  "function",
					Function: ChunkFunction{
						Name: blk.toolName,
					},
				}}}, nil, nil); err != nil {
					return err
				}
			}
			blocks[parsed.ContentBlockIndex] = blk
			return nil
		case "contentBlockDelta":
			blk, ok := blocks[parsed.ContentBlockIndex]
			if !ok {
				return fmt.Errorf("wire: contentBlockDelta for unknown block index %d", parsed.ContentBlockIndex)
			}
			if parsed.Delta == nil {
				return nil
			}
			if parsed.Delta.Text != "" {
				if blk.kind != "text" {
					return fmt.Errorf("wire: text delta for non-text block index %d", parsed.ContentBlockIndex)
				}
				content.WriteString(parsed.Delta.Text)
				return emitChunk(ChunkDelta{Content: parsed.Delta.Text}, nil, nil)
			}
			if parsed.Delta.ToolUse != nil && parsed.Delta.ToolUse.Input != "" {
				if blk.kind != "tool_use" {
					return fmt.Errorf("wire: toolUse delta for non-tool block index %d", parsed.ContentBlockIndex)
				}
				blk.jsonBuf.WriteString(parsed.Delta.ToolUse.Input)
				return emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
					Index: toolIndex[parsed.ContentBlockIndex],
					Function: ChunkFunction{
						Arguments: parsed.Delta.ToolUse.Input,
					},
				}}}, nil, nil)
			}
			// reasoningContent deltas: dropped, no OpenAI equivalent.
			return nil
		case "contentBlockStop":
			blk, ok := blocks[parsed.ContentBlockIndex]
			if !ok {
				return fmt.Errorf("wire: contentBlockStop for unknown block index %d", parsed.ContentBlockIndex)
			}
			delete(blocks, parsed.ContentBlockIndex)
			if blk.kind == "tool_use" {
				args := strings.TrimSpace(blk.jsonBuf.String())
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return fmt.Errorf("wire: streamed toolUse arguments for %q are not valid JSON", blk.toolName)
				}
				res.ToolCalls = append(res.ToolCalls, ToolCall{
					ID:   blk.toolID,
					Type: "function",
					Function: ToolCallFunction{
						Name:      blk.toolName,
						Arguments: args,
					},
				})
			}
			return nil
		case "messageStop":
			sawStop = true
			stopReason = parsed.StopReason
			return nil
		case "metadata":
			if parsed.Usage != nil {
				res.Usage = Usage{
					PromptTokens:     parsed.Usage.InputTokens,
					CompletionTokens: parsed.Usage.OutputTokens,
					TotalTokens:      parsed.Usage.TotalTokens,
				}
			}
			return nil
		default:
			// Unknown event type: ignored — a future Converse event
			// must not break streams that carry usable content.
			return nil
		}
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !sawEvent {
		return StreamResult{}, errors.New("wire: Bedrock event stream ended with no events (empty)")
	}
	if !sawStop {
		return StreamResult{}, errors.New("wire: Bedrock event stream ended without messageStop (truncated)")
	}
	res.Content = content.String()
	res.FinishReason = MapBedrockStopReason(stopReason)
	res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
	fr := res.FinishReason
	usage := res.Usage
	if err := emitChunk(ChunkDelta{}, &fr, &usage); err != nil {
		return StreamResult{}, err
	}
	return res, nil
}
