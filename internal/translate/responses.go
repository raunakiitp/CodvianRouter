package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Pair: openai-responses (the OpenAI Responses surface, POST /v1/responses).
//
// Shapes verified against OpenAI's own API reference
// (developers.openai.com/api-reference/resources/responses/methods/create
// and the streaming guide): requests carry model, input (a string or an
// array of items — messages with typed content parts, function_call
// items with call_id/name/string arguments, function_call_output items
// with call_id/string output), instructions, max_output_tokens,
// temperature, top_p, tools ({"type":"function", name, description,
// parameters}), tool_choice. Responses carry status, output items
// (message with output_text content parts, function_call, reasoning),
// usage {input_tokens, output_tokens, total_tokens, input_tokens_details
// {cached_tokens}, output_tokens_details {reasoning_tokens}}. Streaming
// uses semantic events: response.created, response.in_progress,
// response.output_item.added/done, response.output_text.delta,
// response.function_call_arguments.delta/done, response.completed,
// response.failed, error.
//
// RECORDED DIVERGENCES from an ideal full mapping:
//   - The Responses surface has no stop-sequences parameter; a request
//     with stop set is refused, never silently dropped.
//   - Reasoning output items are display summaries, not replayable
//     reasoning state; they are skipped (recorded), never surfaced as
//     content.
//   - refusal content parts are treated as the assistant's visible
//     reply (they are the message the model chose to send).
// ---------------------------------------------------------------------------

// ResponsesContentPart is one typed content part of a Responses input
// or output message (input_text/input_image on input, output_text/
// refusal on output).
type ResponsesContentPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// ImageURL is the URL of an input_image part.
	ImageURL string `json:"image_url,omitempty"`
}

// ResponsesInputItem is one item of a Responses request's input array:
// a message, a function_call (the assistant turn's tool invocation,
// with string-encoded arguments), or a function_call_output (the tool
// result).
type ResponsesInputItem struct {
	Type    string                 `json:"type,omitempty"` // "message" | "function_call" | "function_call_output"
	Role    string                 `json:"role,omitempty"` // user | assistant | system on "message"
	Content []ResponsesContentPart `json:"content,omitempty"`
	// function_call.
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // string-encoded JSON
	// function_call_output.
	Output string `json:"output,omitempty"`
}

// ResponsesTool is one function tool of a Responses request.
type ResponsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

// ResponsesRequest is the body of POST /v1/responses. Input is either
// a plain string or a []ResponsesInputItem.
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           any             `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
}

// ResponsesUsage is the usage block of a Responses response.
type ResponsesUsage struct {
	InputTokens       int `json:"input_tokens"`
	OutputTokens      int `json:"output_tokens"`
	TotalTokens       int `json:"total_tokens"`
	InputTokensDetail struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetail struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// ResponsesOutputItem is one output item of a Responses response.
type ResponsesOutputItem struct {
	Type    string `json:"type"` // "message" | "function_call" | "reasoning" | ...
	ID      string `json:"id,omitempty"`
	Status  string `json:"status,omitempty"`
	Role    string `json:"role,omitempty"`
	Content []struct {
		Type    string `json:"type"` // "output_text" | "refusal"
		Text    string `json:"text,omitempty"`
		Refusal string `json:"refusal,omitempty"`
	} `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // string-encoded JSON
}

// ResponsesResponse is the body of a Responses API response.
type ResponsesResponse struct {
	ID                string                `json:"id"`
	Object            string                `json:"object,omitempty"` // "response"
	CreatedAt         int64                 `json:"created_at,omitempty"`
	Status            string                `json:"status,omitempty"` // completed | failed | incomplete | in_progress
	Model             string                `json:"model,omitempty"`
	Output            []ResponsesOutputItem `json:"output,omitempty"`
	Usage             *ResponsesUsage       `json:"usage,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
	Error *struct {
		Message string `json:"message,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Responses.
// ---------------------------------------------------------------------------

// ConvertResponsesRequest converts an OpenAI chat-completions request
// into a Responses API request. System/developer messages become the
// instructions parameter; tool calls become function_call items and
// tool results function_call_output items; the string-encoded OpenAI
// arguments pass through (both surfaces encode arguments as strings).
func ConvertResponsesRequest(req wire.ChatCompletionRequest) (*ResponsesRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	if len(req.Stop) > 0 {
		return nil, unrep("stop sequences have no Responses parameter (the surface does not support stop)")
	}

	out := &ResponsesRequest{
		Model:           req.Model,
		MaxOutputTokens: req.MaxTokens,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		Stream:          req.Stream,
	}

	tools := make([]ResponsesTool, 0, len(req.Tools))
	for i, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			return nil, unrep("tools[%d].type %q is not \"function\"", i, t.Type)
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			return nil, unrep("tools[%d].function.name is empty", i)
		}
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, ResponsesTool{
			Type:        "function",
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	if len(tools) > 0 {
		out.Tools = tools
	}
	switch tc := req.ToolChoice.(type) {
	case nil:
	case string:
		switch tc {
		case "auto", "none", "required":
			out.ToolChoice = tc
		default:
			return nil, unrep("tool_choice %q is not a Responses value", tc)
		}
	case map[string]any:
		// Named-function form passes through verbatim (same shape).
		fnObj, _ := tc["function"].(map[string]any)
		if _, ok := fnObj["name"].(string); !ok {
			return nil, unrep("tool_choice object has no function.name")
		}
		out.ToolChoice = tc
	default:
		return nil, unrep("tool_choice of type %T has no Responses equivalent", req.ToolChoice)
	}

	var instructions []string
	var items []ResponsesInputItem
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := translateMessageText(m, i)
			if err != nil {
				return nil, err
			}
			if text != "" {
				instructions = append(instructions, text)
			}
		case "user", "assistant":
			item, err := convertResponsesMessageItem(m, i)
			if err != nil {
				return nil, err
			}
			// A message item with no content parts carries nothing;
			// skip it rather than emit a bare record.
			if len(item.Content) > 0 {
				items = append(items, item)
			}
			for _, tc := range m.ToolCalls {
				if tc.Type != "" && tc.Type != "function" {
					return nil, unrep("messages[%d] tool call type %q is not \"function\"", i, tc.Type)
				}
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return nil, unrep("messages[%d] tool call arguments are not valid JSON", i)
				}
				items = append(items, ResponsesInputItem{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Function.Name,
					Arguments: args,
				})
			}
		case "tool":
			if m.ToolCallID == "" {
				return nil, unrep("messages[%d] is a tool result with no tool_call_id", i)
			}
			text, err := translateMessageText(m, i)
			if err != nil {
				return nil, err
			}
			items = append(items, ResponsesInputItem{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: text,
			})
		default:
			return nil, unrep("messages[%d] role %q has no Responses equivalent", i, m.Role)
		}
	}
	if len(instructions) > 0 {
		out.Instructions = strings.Join(instructions, "\n\n")
	}
	if len(items) == 0 {
		return nil, unrep("no representable messages (all messages were empty)")
	}
	out.Input = items
	return out, nil
}

// convertResponsesMessageItem converts one user/assistant message into
// a Responses input item: text parts become input_text (user) or
// output_text (assistant) content parts, image parts become
// input_image parts. Assistant reasoning state has no request-side
// field here and is dropped only when empty (a restored state is the
// caller's concern — the surface replays reasoning via stored items,
// not client JSON).
func convertResponsesMessageItem(m wire.ChatMessage, index int) (ResponsesInputItem, error) {
	item := ResponsesInputItem{Type: "message", Role: m.Role}
	textType := "input_text"
	if m.Role == "assistant" {
		textType = "output_text"
	}
	if len(m.Content) == 0 {
		return item, nil
	}
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		if asString != "" {
			item.Content = []ResponsesContentPart{{Type: textType, Text: asString}}
		}
		return item, nil
	}
	parts, err := openAIContentParts(m.Content)
	if err != nil {
		return ResponsesInputItem{}, fmt.Errorf("messages[%d]: %w", index, err)
	}
	for i, p := range parts {
		switch {
		case p.Text != "":
			item.Content = append(item.Content, ResponsesContentPart{Type: textType, Text: p.Text})
		case p.ImageURL != nil:
			item.Content = append(item.Content, ResponsesContentPart{Type: "input_image", ImageURL: p.ImageURL.URL})
		default:
			return ResponsesInputItem{}, unrep("messages[%d] parts[%d] has no Responses equivalent", index, i)
		}
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Responses -> OpenAI.
// ---------------------------------------------------------------------------

// ConvertResponsesResponse converts a Responses API response into an
// OpenAI chat-completion response. finish_reason maps: "stop"
// normally, "tool_calls" when the output carries function_call items,
// "length" when the response is incomplete because of
// max_output_tokens. A failed response returns *ResponsesError.
func ConvertResponsesResponse(resp *ResponsesResponse) (*wire.ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("responses response is nil")
	}
	if resp.Error != nil || resp.Status == "failed" {
		return nil, &ResponsesError{Message: responsesErrorMessage(resp)}
	}
	var (
		text      strings.Builder
		toolCalls []wire.ToolCall
		hasCalls  bool
	)
	for i, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					// A refusal is the assistant's visible reply.
					text.WriteString(part.Refusal)
				default:
					return nil, unrep("output[%d] message content part type %q has no OpenAI equivalent", i, part.Type)
				}
			}
		case "function_call":
			hasCalls = true
			args := strings.TrimSpace(item.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return nil, unrep("output[%d] function_call arguments are not valid JSON", i)
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			toolCalls = append(toolCalls, wire.ToolCall{
				ID:   id,
				Type: "function",
				Function: wire.ToolCallFunction{
					Name:      item.Name,
					Arguments: args,
				},
			})
		case "reasoning":
			// Display summaries, not replayable reasoning state;
			// skipped (recorded), never surfaced as content.
			continue
		default:
			// Built-in tool items (web_search_call etc.) have no
			// chat-completions equivalent; refuse rather than
			// silently drop model work.
			return nil, unrep("output[%d] type %q has no OpenAI chat-completions equivalent", i, item.Type)
		}
	}
	msg := wire.ChatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = mustJSON(text.String())
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	finish := "stop"
	switch {
	case resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason == "max_output_tokens":
		finish = "length"
	case hasCalls && resp.Status == "completed":
		finish = "tool_calls"
	}
	var usage wire.Usage
	if resp.Usage != nil {
		usage = wire.Usage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.TotalTokens,
			CachedTokens:     resp.Usage.InputTokensDetail.CachedTokens,
			ReasoningTokens:  resp.Usage.OutputTokensDetail.ReasoningTokens,
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	}
	return &wire.ChatCompletionResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: resp.CreatedAt,
		Model:   resp.Model,
		Choices: []wire.ResponseChoice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   usage,
	}, nil
}

// responsesErrorMessage extracts the message of a failed response.
func responsesErrorMessage(resp *ResponsesResponse) string {
	if resp.Error != nil && resp.Error.Message != "" {
		return resp.Error.Message
	}
	if resp.Error != nil && resp.Error.Code != "" {
		return resp.Error.Code
	}
	return "response status: " + resp.Status
}

// ResponsesError is an error from the Responses surface.
type ResponsesError struct {
	Message string
}

// Error implements error.
func (e *ResponsesError) Error() string {
	return "responses error: " + e.Message
}

// ---------------------------------------------------------------------------
// Streaming: Responses semantic SSE events -> OpenAI chat-completion
// chunks.
//
// Handled events: response.created (captures id/model),
// response.output_text.delta (content deltas), output_item.added with
// a function_call item (tool-call start), function_call_arguments.delta
// (argument fragments, indexed by the item's output_index), response.
// completed (usage + finish), response.failed and error (errors).
// Everything else (in_progress, output_text.done, content_part.*,
// refusal.*, output_item.done) carries no new chat content and is
// ignored.
// ---------------------------------------------------------------------------

// TranslateResponsesStream reads a Responses SSE stream and emits
// OpenAI chat.completion.chunk values via emit, in order. It returns
// the reassembled result when the stream ends.
func TranslateResponsesStream(r io.Reader, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	if emit == nil {
		emit = func(wire.ChatChunk) error { return nil }
	}
	var res wire.StreamResult
	created := time.Now().Unix()
	var content strings.Builder
	var finish string
	sawEvent := false
	// openCalls tracks function_call items by output_index so their
	// argument deltas map onto the right OpenAI tool-call index.
	type openCall struct{ id, name string }
	openCalls := make(map[int]openCall)
	emitChunk := func(delta wire.ChunkDelta, finishReason *string, usage *wire.Usage) error {
		return emit(wire.ChatChunk{
			ID:      res.ID,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   res.Model,
			Choices: []wire.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
			Usage:   usage,
		})
	}

	err := readSSE(r, func(ev sseEvent) error {
		data := strings.TrimSpace(ev.data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		var probe struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &probe); err != nil {
			return fmt.Errorf("translate: malformed Responses SSE data: %w", err)
		}
		if probe.Type == "error" {
			msg := probe.Message
			if msg == "" {
				msg = strings.TrimSpace(string(probe.Error))
			}
			return &ResponsesError{Message: msg}
		}
		switch probe.Type {
		case "response.created", "response.in_progress":
			var payload struct {
				Response ResponsesResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed %s event: %w", probe.Type, err)
			}
			sawEvent = true
			if payload.Response.ID != "" {
				res.ID = payload.Response.ID
			}
			if payload.Response.Model != "" {
				res.Model = payload.Response.Model
			}
			return nil
		case "response.output_text.delta":
			var payload struct {
				Delta string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed output_text.delta event: %w", err)
			}
			if payload.Delta == "" {
				return nil
			}
			sawEvent = true
			content.WriteString(payload.Delta)
			return emitChunk(wire.ChunkDelta{Role: "assistant", Content: payload.Delta}, nil, nil)
		case "response.output_item.added":
			var payload struct {
				OutputIndex int                 `json:"output_index"`
				Item        ResponsesOutputItem `json:"item"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed output_item.added event: %w", err)
			}
			if payload.Item.Type != "function_call" {
				return nil
			}
			sawEvent = true
			callID := payload.Item.CallID
			if callID == "" {
				callID = payload.Item.ID
			}
			openCalls[payload.OutputIndex] = openCall{id: callID, name: payload.Item.Name}
			return emitChunk(wire.ChunkDelta{ToolCalls: []wire.ChunkToolCall{{
				Index: payload.OutputIndex,
				ID:    callID,
				Type:  "function",
				Function: wire.ChunkFunction{
					Name: payload.Item.Name,
				},
			}}}, nil, nil)
		case "response.function_call_arguments.delta":
			var payload struct {
				OutputIndex int    `json:"output_index"`
				Delta       string `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed function_call_arguments.delta event: %w", err)
			}
			if payload.Delta == "" {
				return nil
			}
			sawEvent = true
			return emitChunk(wire.ChunkDelta{ToolCalls: []wire.ChunkToolCall{{
				Index:    payload.OutputIndex,
				Function: wire.ChunkFunction{Arguments: payload.Delta},
			}}}, nil, nil)
		case "response.completed":
			var payload struct {
				Response ResponsesResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed response.completed event: %w", err)
			}
			sawEvent = true
			if payload.Response.Usage != nil {
				u := payload.Response.Usage
				res.Usage.PromptTokens = u.InputTokens
				res.Usage.CompletionTokens = u.OutputTokens
				res.Usage.TotalTokens = u.TotalTokens
				res.Usage.CachedTokens = u.InputTokensDetail.CachedTokens
				res.Usage.ReasoningTokens = u.OutputTokensDetail.ReasoningTokens
				if res.Usage.TotalTokens == 0 {
					res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
				}
			}
			if payload.Response.IncompleteDetails != nil && payload.Response.IncompleteDetails.Reason == "max_output_tokens" {
				finish = "length"
			} else {
				finish = "stop"
			}
			return nil
		case "response.failed":
			var payload struct {
				Response ResponsesResponse `json:"response"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed response.failed event: %w", err)
			}
			return &ResponsesError{Message: responsesErrorMessage(&payload.Response)}
		case
			// Completion markers and done-notifications for parts and
			// items already emitted: no new chat content.
			"response.output_item.done",
			"response.content_part.added", "response.content_part.done",
			"response.output_text.done", "response.output_text.annotation.added",
			"response.refusal.delta", "response.refusal.done",
			"response.function_call_arguments.done",
			"response.queued":
			return nil
		case "":
			// Nameless events are tolerated defensively; the type
			// field inside the payload is the discriminator.
			return nil
		default:
			// Built-in tool progress events and unknown types must
			// not kill the stream.
			return nil
		}
	})
	if err != nil {
		return wire.StreamResult{}, err
	}
	if !sawEvent {
		return wire.StreamResult{}, fmt.Errorf("translate: Responses SSE stream ended with no events (empty)")
	}
	res.Content = content.String()
	if finish == "" {
		finish = "stop"
	}
	res.FinishReason = finish
	fr := res.FinishReason
	usage := res.Usage
	if err := emitChunk(wire.ChunkDelta{}, &fr, &usage); err != nil {
		return wire.StreamResult{}, err
	}
	return res, nil
}
