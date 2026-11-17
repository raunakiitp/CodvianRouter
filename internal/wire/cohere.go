// Cohere native-wire conversion (Chat API v2, POST /v2/chat).
//
//	OpenAI ChatCompletionRequest --ConvertCohereRequest--> CohereRequest
//	CohereResponse               --ConvertCohereResponse--> ChatCompletionResponse
//	Cohere SSE stream            --TranslateCohereStream--> ChatChunk stream
//	Cohere error JSON            --MapCohereError/MapCohereStreamError--> CohereError
//
// The Cohere API surface used here is POST {base}/v2/chat (and the
// same endpoint with "stream": true for SSE), with the API key in the
// Authorization: Bearer header (resolved per (provider, endpoint) by
// catalog.ResolveAuth). This file is a pure conversion library: no I/O,
// no credentials, no HTTP. The actual call stays in internal/provider
// (cohereClient).
//
// Shapes verified against Cohere's own docs (docs.cohere.com,
// reference/chat, chat-api, streaming, tool-use-streaming,
// image-inputs; all read 2026-11-17):
//   - Request messages are typed: roles user/assistant/system/tool;
//     content is a string or a list of typed blocks (text, image_url).
//     Image blocks use the SAME {"type":"image_url","image_url":{"url"}}
//     shape as OpenAI and accept both data: URLs and http(s) URLs
//     (max 20 images / 20 MB per request).
//   - Tools are {"type":"function","function":{name, description,
//     parameters}}; tool_choice is the enum "NONE"/"REQUIRED", and
//     omitting it means the model is free to choose (there is no
//     "auto" value — OpenAI "auto" is therefore dropped, not mapped).
//   - Non-streaming response: {id, finish_reason, message:{role,
//     content:[{type:"text",text}], tool_calls:[...]},
//     usage|meta:{tokens|billed_units}}. finish_reason values are
//     COMPLETE / MAX_TOKENS / TOOL_CALL / STOP_SEQUENCE / ERROR /
//     TIMEOUT (the reference also lists lowercase spellings).
//   - Streaming events (SSE data payloads carry a "type" field):
//     message-start, content-start, content-delta
//     (delta.message.content.text), content-end, tool-plan-delta,
//     tool-call-start (delta.message.tool_calls: list of {id, name}),
//     tool-call-delta (delta.message.tool_calls[].function.arguments
//     fragments), tool-call-end, message-end
//     (delta.finish_reason, delta.usage).
//
// Mapping notes, all deliberate and loud on failure
// (*UnrepresentableError, never silent wrong output):
//   - OpenAI temperature maps straight through (Cohere: non-negative
//     float); top_p maps to Cohere's `p` (docs: 0.01–0.99), so a
//     top_p outside that range is unrepresentable rather than
//     silently clamped.
//   - stop_sequences are capped at 5 by the API; more than 5 is an
//     error, not a truncation.
//   - Cohere has a system role, so system/developer messages stay
//     system messages (unlike Gemini, which folds them into a
//     systemInstruction).
//   - Assistant tool calls ride in message.tool_calls in the same
//     {id, type:"function", function:{name, arguments}} shape as
//     OpenAI; the response may ALSO carry tool calls as
//     {"type":"tool-call"} content blocks — both are accepted and
//     deduplicated by id.
//   - Tool results are role "tool" messages carrying the tool_call_id
//     and text content blocks.
//   - Response content blocks the OpenAI side cannot express
//     (document/citation blocks, image output) return
//     *UnrepresentableError; tool-plan deltas in a stream are dropped
//     (internal planning text, no OpenAI equivalent).
//   - A finish_reason of ERROR/TIMEOUT is a failure, not a turn: it
//     returns *CohereError (timeout retryable, error not).
package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Cohere side (Chat API v2).
// ---------------------------------------------------------------------------

// CohereContentBlock is one typed block in a Cohere v2 message
// content list. Only the fields for its Type are populated.
type CohereContentBlock struct {
	Type string `json:"type"` // "text" | "image_url" | "tool-call" | ...
	// Text blocks.
	Text string `json:"text,omitempty"`
	// Image blocks: same shape as OpenAI's image_url part.
	ImageURL *ImageURLPart `json:"image_url,omitempty"`
	// Tool-call blocks (assistant content).
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// CohereMessage is one message in a Cohere v2 request/response.
// Content is kept raw because it may be a plain string or a list of
// CohereContentBlock.
type CohereMessage struct {
	Role       string          `json:"role"` // "user" | "assistant" | "system" | "tool"
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

// CohereToolFunction is the function payload of a Cohere tool.
type CohereToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// CohereTool is one tool definition for a Cohere v2 request.
type CohereTool struct {
	Type     string             `json:"type"` // "function"
	Function CohereToolFunction `json:"function"`
}

// CohereRequest is the body of POST /v2/chat.
type CohereRequest struct {
	Model         string          `json:"model"`
	Messages      []CohereMessage `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	P             *float64        `json:"p,omitempty"` // top_p
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         []CohereTool    `json:"tools,omitempty"`
	ToolChoice    any             `json:"tool_choice,omitempty"`
}

// CohereUsage is the token usage of a Cohere response, as reported
// either top-level ("usage") or under "meta" ("tokens" /
// "billed_units").
type CohereUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CohereMeta is the "meta" block of a Cohere response.
type CohereMeta struct {
	Tokens      *CohereUsage `json:"tokens,omitempty"`
	BilledUnits *CohereUsage `json:"billed_units,omitempty"`
	APIVersion  *struct {
		Version string `json:"version,omitempty"`
	} `json:"api_version,omitempty"`
}

// CohereResponse is the body of a non-streaming /v2/chat reply.
type CohereResponse struct {
	ID           string        `json:"id,omitempty"`
	FinishReason string        `json:"finish_reason,omitempty"`
	Message      CohereMessage `json:"message"`
	Usage        *CohereUsage  `json:"usage,omitempty"`
	Meta         *CohereMeta   `json:"meta,omitempty"`
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// CohereError is an error returned by the Cohere API, preserving the
// HTTP status and the message. Retryable is true for the transient
// classes the router must fail over on: 429, 408, and any 5xx.
// StatusCode is 0 for errors surfaced mid-stream (the HTTP status was
// already 200 when the stream's error event arrived) and for
// finish_reason failures (ERROR/TIMEOUT), which arrive inside a 200.
type CohereError struct {
	StatusCode int
	Message    string
	RetryAfter time.Duration
	Retryable  bool
}

// mustJSONValue marshals v to its JSON encoding (infallible for the
// values used here: strings, content-block slices).
func mustJSONValue(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}

func (e *CohereError) Error() string {
	if e == nil {
		return "wire: cohere error"
	}
	if e.StatusCode != 0 {
		if e.Message != "" {
			return fmt.Sprintf("cohere error: HTTP %d: %s", e.StatusCode, e.Message)
		}
		return fmt.Sprintf("cohere error: HTTP %d", e.StatusCode)
	}
	if e.Message != "" {
		return fmt.Sprintf("cohere error: %s", e.Message)
	}
	return "wire: cohere error"
}

// IsRateLimit reports whether this is a 429.
func (e *CohereError) IsRateLimit() bool {
	return e != nil && e.StatusCode == http.StatusTooManyRequests
}

// IsCohereRetryable reports whether err is a *CohereError marked retryable.
func IsCohereRetryable(err error) bool {
	var ce *CohereError
	return errors.As(err, &ce) && ce.Retryable
}

// parseCohereErrorBody extracts the message from a Cohere error body.
// Cohere's documented shape is {"message": "...", "id": "..."}; the
// {"error": "..."} and {"error": {"message": "..."}} shapes are also
// tolerated, as is a plain-text body.
func parseCohereErrorBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var msg struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &msg); err == nil {
		if msg.Message != "" {
			return msg.Message
		}
		if len(msg.Error) > 0 {
			var asString string
			if err := json.Unmarshal(msg.Error, &asString); err == nil {
				return asString
			}
			var asObj struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(msg.Error, &asObj); err == nil && asObj.Message != "" {
				return asObj.Message
			}
		}
	}
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	return trimmed
}

// MapCohereError converts a non-2xx Cohere HTTP response (status,
// headers, body) into a *CohereError that preserves the status code.
// 429 stays 429-class and 5xx stay 5xx so the router's failover
// triggers correctly; a 4xx whose body mentions quota/billing is not
// retryable (it is a quota problem, and the provider maps it to
// QuotaError by message).
func MapCohereError(status int, headers http.Header, body []byte) error {
	message := parseCohereErrorBody(body)
	retryable := false
	switch {
	case status == http.StatusTooManyRequests:
		retryable = true
	case status == http.StatusRequestTimeout:
		retryable = true
	case status >= 500:
		retryable = true
	}
	return &CohereError{
		StatusCode: status,
		Message:    message,
		RetryAfter: parseRetryAfter(headers),
		Retryable:  retryable,
	}
}

// MapCohereStreamError converts a Cohere SSE error payload into a
// *CohereError. The HTTP status was already 200 when the stream
// started, so StatusCode is 0; retryability is derived from the
// message (rate-limit wording) because the event carries no status.
func MapCohereStreamError(body []byte) error {
	message := parseCohereErrorBody(body)
	// Error events may nest the message under delta.
	if message == "" {
		var env struct {
			Delta json.RawMessage `json:"delta"`
		}
		if err := json.Unmarshal(body, &env); err == nil && len(env.Delta) > 0 {
			message = parseCohereErrorBody(env.Delta)
		}
	}
	lower := strings.ToLower(message)
	retryable := strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "overloaded") ||
		strings.Contains(lower, "try again")
	return &CohereError{
		StatusCode: 0,
		Message:    message,
		Retryable:  retryable,
	}
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Cohere.
// ---------------------------------------------------------------------------

// cohereMaxImages is the API's documented cap: at most 20 images per
// request (and 20 MB total).
const cohereMaxImages = 20

// ConvertCohereRequest converts an OpenAI chat-completions request
// into a Cohere Chat API v2 request body. It returns
// *UnrepresentableError for anything that cannot be represented
// faithfully.
func ConvertCohereRequest(req ChatCompletionRequest) (*CohereRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	if req.Temperature != nil && *req.Temperature < 0 {
		return nil, unrep("temperature %v is negative (Cohere requires a non-negative temperature)", *req.Temperature)
	}
	if req.TopP != nil && (*req.TopP < 0.01 || *req.TopP > 0.99) {
		return nil, unrep("top_p %v is outside Cohere's 0.01-0.99 range for p", *req.TopP)
	}
	if len(req.Stop) > 5 {
		return nil, unrep("Cohere accepts at most 5 stop sequences, got %d", len(req.Stop))
	}

	tools, toolChoice, err := convertCohereTools(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}

	out := &CohereRequest{
		Model:         req.Model,
		Stream:        req.Stream,
		Temperature:   req.Temperature,
		P:             req.TopP,
		StopSequences: append([]string(nil), req.Stop...),
		Tools:         tools,
		ToolChoice:    toolChoice,
	}
	if req.MaxTokens > 0 {
		out.MaxTokens = req.MaxTokens
	}

	imageCount := 0
	for i, m := range req.Messages {
		cm, n, err := convertCohereMessage(m, i)
		if err != nil {
			return nil, err
		}
		imageCount += n
		if imageCount > cohereMaxImages {
			return nil, unrep("Cohere accepts at most %d images per request, got more", cohereMaxImages)
		}
		out.Messages = append(out.Messages, cm)
	}
	if len(out.Messages) == 0 {
		return nil, unrep("no representable messages (all messages were empty)")
	}
	return out, nil
}

// convertCohereTools converts OpenAI tool definitions to Cohere
// function tools and the matching tool_choice. Cohere's tool_choice
// is the enum "NONE"/"REQUIRED"; omitting it means the model is free
// to choose, so OpenAI "auto" (and an absent tool_choice) maps to
// "omit", not to a value.
func convertCohereTools(tools []ToolDefinition, toolChoice any) ([]CohereTool, any, error) {
	if len(tools) == 0 {
		// No tools defined: only "none", "auto" and absent
		// tool_choice are meaningful; "required" or a named
		// function with no tools is an error, not a silent no-op.
		if toolChoice == nil {
			return nil, nil, nil
		}
		if s, ok := toolChoice.(string); ok {
			switch s {
			case "none":
				return nil, "NONE", nil
			case "auto":
				return nil, nil, nil
			}
		}
		return nil, nil, unrep("tool_choice %v with no tools defined", toolChoice)
	}
	ct := make([]CohereTool, 0, len(tools))
	for i, t := range tools {
		if t.Type != "" && t.Type != "function" {
			return nil, nil, unrep("tools[%d].type %q is not \"function\"", i, t.Type)
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, nil, unrep("tools[%d].function.name is empty", i)
		}
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		ct = append(ct, CohereTool{
			Type: "function",
			Function: CohereToolFunction{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  params,
			},
		})
	}
	tc, err := convertCohereToolChoice(toolChoice, ct)
	if err != nil {
		return nil, nil, err
	}
	return ct, tc, nil
}

// convertCohereToolChoice maps an OpenAI tool_choice onto Cohere's
// tool_choice value.
func convertCohereToolChoice(tc any, tools []CohereTool) (any, error) {
	if tc == nil {
		return nil, nil
	}
	if s, ok := tc.(string); ok {
		switch s {
		case "auto":
			// Unspecified = free choice; omit the field.
			return nil, nil
		case "none":
			return "NONE", nil
		case "required":
			return "REQUIRED", nil
		default:
			return nil, unrep("unknown tool_choice %q", s)
		}
	}
	if m, ok := tc.(map[string]any); ok {
		t, _ := m["type"].(string)
		switch t {
		case "auto":
			return nil, nil
		case "none":
			return "NONE", nil
		case "required", "any":
			return "REQUIRED", nil
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
				if tool.Function.Name == name {
					return map[string]any{
						"type":     "function",
						"function": map[string]any{"name": name},
					}, nil
				}
			}
			return nil, unrep("tool_choice names tool %q but it is not defined", name)
		default:
			return nil, unrep("unknown tool_choice type %q", t)
		}
	}
	return nil, unrep("tool_choice has unsupported shape %T", tc)
}

// convertCohereMessage converts one OpenAI message to a Cohere v2
// message. It returns the message and the number of image blocks it
// carries (for the 20-image cap).
func convertCohereMessage(m ChatMessage, index int) (CohereMessage, int, error) {
	switch m.Role {
	case "system", "developer":
		// Cohere has a system role; developer folds into it.
		text, n, err := cohereMessageText(m, index)
		if err != nil {
			return CohereMessage{}, 0, err
		}
		if n > 0 {
			return CohereMessage{}, 0, unrep("messages[%d]: image content cannot appear in a %s message", index, m.Role)
		}
		return CohereMessage{Role: "system", Content: mustJSON(text)}, 0, nil
	case "user":
		blocks, n, err := convertCohereParts(m.Content, index)
		if err != nil {
			return CohereMessage{}, 0, err
		}
		return CohereMessage{Role: "user", Content: mustJSONValue(blocks)}, n, nil
	case "assistant":
		blocks, n, err := convertCohereParts(m.Content, index)
		if err != nil {
			return CohereMessage{}, 0, err
		}
		if n > 0 {
			return CohereMessage{}, 0, unrep("messages[%d]: image content cannot appear in an assistant message", index)
		}
		out := CohereMessage{Role: "assistant", Content: mustJSONValue(blocks)}
		for i, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return CohereMessage{}, 0, unrep("messages[%d].tool_calls[%d].type %q is not \"function\"", index, i, tc.Type)
			}
			if strings.TrimSpace(tc.Function.Name) == "" {
				return CohereMessage{}, 0, unrep("messages[%d].tool_calls[%d].function.name is empty", index, i)
			}
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return CohereMessage{}, 0, unrep("messages[%d].tool_calls[%d].function.arguments is not valid JSON", index, i)
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: ToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}
		return out, 0, nil
	case "tool":
		if strings.TrimSpace(m.ToolCallID) == "" {
			return CohereMessage{}, 0, unrep("messages[%d] is a tool result with no tool_call_id", index)
		}
		text, n, err := cohereMessageText(m, index)
		if err != nil {
			return CohereMessage{}, 0, err
		}
		if n > 0 {
			return CohereMessage{}, 0, unrep("messages[%d]: image content cannot appear in a tool message", index)
		}
		return CohereMessage{
			Role:       "tool",
			Content:    mustJSONValue([]CohereContentBlock{{Type: "text", Text: text}}),
			ToolCallID: m.ToolCallID,
		}, 0, nil
	default:
		return CohereMessage{}, 0, unrep("messages[%d].role %q is not supported", index, m.Role)
	}
}

// cohereMessageText extracts the plain-text content of a message.
// Multi-part content must be text-only; image parts in a
// system/developer/tool message are unrepresentable.
func cohereMessageText(m ChatMessage, index int) (string, int, error) {
	blocks, n, err := convertCohereParts(m.Content, index)
	if err != nil {
		return "", 0, err
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" {
			return "", 0, unrep("messages[%d]: non-text content cannot appear in a %s message", index, m.Role)
		}
		sb.WriteString(b.Text)
	}
	return sb.String(), n, nil
}

// convertCohereParts converts OpenAI message content (a string, an
// array of parts, or null) into Cohere content blocks. Image blocks
// pass through unchanged: Cohere v2 accepts the same
// {"type":"image_url","image_url":{"url"}} shape as OpenAI, for both
// data: URLs and http(s) URLs.
func convertCohereParts(raw json.RawMessage, index int) ([]CohereContentBlock, int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, 0, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return nil, 0, nil
		}
		return []CohereContentBlock{{Type: "text", Text: asString}}, 0, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, 0, unrep("messages[%d].content is neither a string nor a content-part array", index)
	}
	var out []CohereContentBlock
	images := 0
	for i, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, CohereContentBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || strings.TrimSpace(p.ImageURL.URL) == "" {
				return nil, 0, unrep("messages[%d].content[%d] is an image_url part with no URL", index, i)
			}
			images++
			out = append(out, CohereContentBlock{Type: "image_url", ImageURL: p.ImageURL})
		default:
			return nil, 0, unrep("messages[%d].content[%d].type %q is not supported (only \"text\" and \"image_url\")", index, i, p.Type)
		}
	}
	return out, images, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Cohere -> OpenAI.
// ---------------------------------------------------------------------------

// MapCohereFinishReason maps a Cohere finish_reason onto an OpenAI
// finish_reason. Both the documented uppercase values (COMPLETE,
// MAX_TOKENS, TOOL_CALL, STOP_SEQUENCE) and the lowercase spellings
// the reference lists are accepted. ERROR and TIMEOUT are failures,
// not turn endings: ConvertCohereResponse rejects them via
// cohereFinishFailure before this mapping runs, so this function
// mapping them to "stop" is only a fallback for a bare mapping. An
// empty or unknown reason maps to "stop" — the turn ended, and
// inventing a more specific reason would be guessing.
func MapCohereFinishReason(finishReason string) string {
	switch strings.ToUpper(strings.TrimSpace(finishReason)) {
	case "COMPLETE", "STOP_SEQUENCE", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "TOOL_CALL":
		return "tool_calls"
	default:
		return "stop"
	}
}

// cohereFinishFailure returns a *CohereError when the finish reason
// is a failure (ERROR: not retryable; TIMEOUT: retryable), else nil.
func cohereFinishFailure(finishReason string) error {
	switch strings.ToUpper(strings.TrimSpace(finishReason)) {
	case "ERROR":
		return &CohereError{Message: "cohere generation failed (finish_reason ERROR)", Retryable: false}
	case "TIMEOUT":
		return &CohereError{Message: "cohere generation timed out (finish_reason TIMEOUT)", Retryable: true}
	default:
		return nil
	}
}

// cohereResponseUsage extracts usage from a response, preferring the
// top-level "usage" block, then "meta.tokens", then
// "meta.billed_units".
func cohereResponseUsage(resp *CohereResponse) Usage {
	var u *CohereUsage
	switch {
	case resp.Usage != nil:
		u = resp.Usage
	case resp.Meta != nil && resp.Meta.Tokens != nil:
		u = resp.Meta.Tokens
	case resp.Meta != nil && resp.Meta.BilledUnits != nil:
		u = resp.Meta.BilledUnits
	}
	if u == nil {
		return Usage{}
	}
	total := u.InputTokens + u.OutputTokens
	return Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: total}
}

// ConvertCohereResponse converts a non-streaming Cohere /v2/chat
// response into an OpenAI chat-completion response. Tool calls are
// read from message.tool_calls and from {"type":"tool-call"} content
// blocks (deduplicated by id); a finish_reason of ERROR/TIMEOUT
// returns *CohereError, not a success.
func ConvertCohereResponse(resp *CohereResponse) (*ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("cohere response is nil")
	}
	if err := cohereFinishFailure(resp.FinishReason); err != nil {
		return nil, err
	}
	var (
		text      strings.Builder
		toolCalls []ToolCall
		seenIDs   = make(map[string]bool)
	)
	addToolCall := func(id, name, args string) error {
		if strings.TrimSpace(name) == "" {
			return unrep("response tool call has no name")
		}
		args = strings.TrimSpace(args)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return unrep("response tool call %q arguments are not valid JSON", name)
		}
		if id != "" {
			if seenIDs[id] {
				return nil // same call reported twice (block + list)
			}
			seenIDs[id] = true
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:   id,
			Type: "function",
			Function: ToolCallFunction{
				Name:      name,
				Arguments: args,
			},
		})
		return nil
	}
	// Content blocks first (text + tool-call blocks).
	if len(resp.Message.Content) > 0 {
		var blocks []CohereContentBlock
		if err := json.Unmarshal(resp.Message.Content, &blocks); err == nil {
			for i, b := range blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool-call":
					if err := addToolCall(b.ID, b.Name, string(b.Parameters)); err != nil {
						return nil, err
					}
				case "":
					// A block with no type but text is treated as text.
					if b.Text != "" {
						text.WriteString(b.Text)
					}
				default:
					return nil, unrep("response message content[%d] is a %q block with no OpenAI equivalent", i, b.Type)
				}
			}
		} else {
			// Content as a plain string.
			var asString string
			if err := json.Unmarshal(resp.Message.Content, &asString); err != nil {
				return nil, unrep("response message content is neither a string nor a content-block array")
			}
			text.WriteString(asString)
		}
	}
	// message.tool_calls list (same shape as OpenAI's).
	for _, tc := range resp.Message.ToolCalls {
		if tc.Type != "" && tc.Type != "function" {
			return nil, unrep("response tool_calls entry type %q is not \"function\"", tc.Type)
		}
		if err := addToolCall(tc.ID, tc.Function.Name, tc.Function.Arguments); err != nil {
			return nil, err
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
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   "",
		Choices: []ResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: MapCohereFinishReason(resp.FinishReason),
		}},
		Usage: cohereResponseUsage(resp),
	}, nil
}

// ---------------------------------------------------------------------------
// Streaming: Cohere SSE -> OpenAI chat-completion chunks.
// ---------------------------------------------------------------------------

// cohereStreamToolCall is one tool call in a streaming delta
// (tool-call-start / tool-call-delta).
type cohereStreamToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

// cohereStreamEvent is the JSON envelope of one Cohere SSE data
// payload. Only the fields each event type carries are populated;
// the dispatch is on Type, not on the SSE event name.
type cohereStreamEvent struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	// message-start.
	Delta *struct {
		Message *struct {
			Role      string                 `json:"role,omitempty"`
			ToolPlan  string                 `json:"tool_plan,omitempty"`
			ToolCalls []cohereStreamToolCall `json:"tool_calls,omitempty"`
			Content   *struct {
				Type string `json:"type,omitempty"`
				Text string `json:"text,omitempty"`
			} `json:"content,omitempty"`
		} `json:"message,omitempty"`
		FinishReason string       `json:"finish_reason,omitempty"`
		Usage        *CohereUsage `json:"usage,omitempty"`
		Meta         *CohereMeta  `json:"meta,omitempty"`
	} `json:"delta,omitempty"`
}

// TranslateCohereStream reads a Cohere /v2/chat SSE stream
// (stream:true) and emits OpenAI chat.completion.chunk values via
// emit, in order. It returns the reassembled result when message-end
// arrives.
//
//   - content-delta becomes a content delta (delta.message.content
//     .text); tool-plan-delta is dropped (internal planning text, no
//     OpenAI equivalent).
//   - tool-call-start opens one tool call per listed entry (id +
//     name, first delta for the call); tool-call-delta appends
//     argument fragments, matched by id when present, else to the
//     most recently opened call; tool-call-end validates the
//     accumulated arguments as JSON (empty becomes "{}").
//   - message-end supplies the finish reason and usage; the final
//     emitted chunk carries finish_reason and usage.
//   - citation/search/document events (RAG) are ignored — they carry
//     no text the OpenAI side can express.
//   - an error event returns the mapped *CohereError and never a
//     success result; a stream that ends without message-end returns
//     an error (truncated), never a partial success; a stream with no
//     events at all returns an error (empty).
func TranslateCohereStream(r io.Reader, emit func(ChatChunk) error) (StreamResult, error) {
	if emit == nil {
		emit = func(ChatChunk) error { return nil }
	}
	var res StreamResult
	created := time.Now().Unix()
	// openCalls maps a Cohere tool-call id to the sequential OpenAI
	// tool-call index; args accumulate per index.
	callIndex := make(map[string]int)
	callArgs := make(map[int]*strings.Builder)
	callIDs := make(map[int]string)
	callNames := make(map[int]string)
	nextTool := 0
	lastOpen := -1
	var (
		content  strings.Builder
		sawEvent bool
		sawEnd   bool
		finish   string
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
	openCall := func(id, name string) (int, error) {
		idx := nextTool
		nextTool++
		callIndex[id] = idx
		callArgs[idx] = &strings.Builder{}
		callIDs[idx] = id
		callNames[idx] = name
		lastOpen = idx
		if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
			Index: idx,
			ID:    id,
			Type:  "function",
			Function: ChunkFunction{
				Name: name,
			},
		}}}, nil, nil); err != nil {
			return 0, err
		}
		return idx, nil
	}

	err := readSSE(r, func(ev sseEvent) error {
		data := strings.TrimSpace(ev.data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		var parsed cohereStreamEvent
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("wire: malformed Cohere SSE data: %w", err)
		}
		sawEvent = true
		if parsed.Type == "error" {
			return MapCohereStreamError([]byte(data))
		}
		switch parsed.Type {
		case "message-start":
			if parsed.ID != "" {
				res.ID = parsed.ID
			}
		case "content-delta":
			if parsed.Delta == nil || parsed.Delta.Message == nil || parsed.Delta.Message.Content == nil {
				return nil
			}
			text := parsed.Delta.Message.Content.Text
			if text == "" {
				return nil
			}
			content.WriteString(text)
			return emitChunk(ChunkDelta{Role: "assistant", Content: text}, nil, nil)
		case "tool-call-start":
			if parsed.Delta == nil || parsed.Delta.Message == nil {
				return nil
			}
			for _, tc := range parsed.Delta.Message.ToolCalls {
				if strings.TrimSpace(tc.Function.Name) == "" {
					continue
				}
				if _, err := openCall(tc.ID, tc.Function.Name); err != nil {
					return err
				}
			}
			return nil
		case "tool-call-delta":
			if parsed.Delta == nil || parsed.Delta.Message == nil {
				return nil
			}
			for _, tc := range parsed.Delta.Message.ToolCalls {
				idx := -1
				if tc.ID != "" {
					if i, ok := callIndex[tc.ID]; ok {
						idx = i
					}
				}
				if idx < 0 {
					idx = lastOpen
				}
				if idx < 0 {
					return unrep("streamed tool-call-delta names no open tool call")
				}
				args := tc.Function.Arguments
				if args != "" {
					callArgs[idx].WriteString(args)
					if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
						Index:    idx,
						Function: ChunkFunction{Arguments: args},
					}}}, nil, nil); err != nil {
						return err
					}
				}
			}
			return nil
		case "tool-call-end":
			// Arguments are validated when the stream closes; a
			// tool-call-end only confirms this call is complete.
			return nil
		case "message-end":
			sawEnd = true
			if parsed.Delta != nil {
				if parsed.Delta.FinishReason != "" {
					finish = parsed.Delta.FinishReason
				}
				if parsed.Delta.Usage != nil {
					res.Usage = Usage{
						PromptTokens:     parsed.Delta.Usage.InputTokens,
						CompletionTokens: parsed.Delta.Usage.OutputTokens,
					}
					res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
				} else if parsed.Delta.Meta != nil {
					if parsed.Delta.Meta.Tokens != nil {
						res.Usage = Usage{
							PromptTokens:     parsed.Delta.Meta.Tokens.InputTokens,
							CompletionTokens: parsed.Delta.Meta.Tokens.OutputTokens,
						}
						res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
					}
				}
			}
			return nil
		case "content-start", "content-end", "tool-plan-delta":
			// No text to emit; tool-plan deltas are dropped.
			return nil
		default:
			// Citation / search / document events and any future
			// event type: ignored, they carry no OpenAI text.
			return nil
		}
		return nil
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !sawEvent {
		return StreamResult{}, errors.New("wire: Cohere SSE stream ended with no events (empty)")
	}
	if !sawEnd {
		return StreamResult{}, errors.New("wire: Cohere SSE stream ended without message-end (truncated)")
	}
	if finish == "" {
		finish = "COMPLETE"
	}
	if err := cohereFinishFailure(finish); err != nil {
		return StreamResult{}, err
	}
	for idx := 0; idx < nextTool; idx++ {
		args := strings.TrimSpace(callArgs[idx].String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return StreamResult{}, fmt.Errorf("wire: streamed tool-call arguments for %q are not valid JSON", callNames[idx])
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:   callIDs[idx],
			Type: "function",
			Function: ToolCallFunction{
				Name:      callNames[idx],
				Arguments: args,
			},
		})
	}
	res.Content = content.String()
	res.FinishReason = MapCohereFinishReason(finish)
	fr := res.FinishReason
	usage := res.Usage
	if err := emitChunk(ChunkDelta{}, &fr, &usage); err != nil {
		return StreamResult{}, err
	}
	return res, nil
}
