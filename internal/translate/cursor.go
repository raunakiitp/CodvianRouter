package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Pair: openai <-> cursor (Cursor Cloud Agents API).
//
// SHAPE PROVENANCE: Cursor's public API surface is documented at
// cursor.com/docs/cloud-agent/api/endpoints. It is an agent-
// orchestration REST API (POST /v1/agents, POST /v1/agents/{id}/runs,
// GET /v1/agents/{id}/runs/{runId}/stream). Cursor does NOT expose an
// OpenAI-compatible chat-completions endpoint — /v1/chat/completions
// and /v1/responses do not exist on api.cursor.com (confirmed by
// Cursor's own docs statement that the API "is not a standalone
// model-inference or chat-completions API").
//
// This pair therefore translates the chat-completions surface onto the
// documented agent surface, best-effort. RECORDED DIVERGENCES:
//   - A conversation is carried as the agent's prompt text (rendered
//     transcript); Cursor holds the real conversation state
//     server-side. Follow-up turns map onto POST /v1/agents/{id}/runs
//     with the new user prompt (prior messages must be empty — the
//     agent already has them).
//   - Sampling parameters (temperature, top_p, max_tokens, stop) have
//     no field in the documented agent shapes; they are dropped, never
//     invented.
//   - Client tool definitions have no equivalent (the cloud agent has
//     its own tools); they are dropped.
//   - Stream event shapes are the documented ones: assistant{text},
//     thinking{text}, tool_call{callId,name,status,args?,result?},
//     status, interaction_update, heartbeat, result{runId,status,
//     text?,durationMs?,git?}, error{code,message}, done. The result
//     event's text is the final assistant reply (the reference's own
//     example differs from the assistant deltas) and is used as the
//     authoritative content; tool_call "completed" results have no
//     OpenAI equivalent and are skipped; no usage payload exists, so
//     usage stays zero.
// ---------------------------------------------------------------------------

// CursorPrompt is the prompt object of the create / follow-up bodies.
type CursorPrompt struct {
	Text string `json:"text"`
}

// CursorModelSelection is the model object of the create body.
type CursorModelSelection struct {
	ID string `json:"id,omitempty"`
}

// CursorAgentCreateRequest is the body of POST /v1/agents.
type CursorAgentCreateRequest struct {
	Prompt CursorPrompt          `json:"prompt"`
	Model  *CursorModelSelection `json:"model,omitempty"`
}

// CursorAgentFollowupRequest is the body of POST /v1/agents/{id}/runs
// (the follow-up prompt continues the agent's server-side
// conversation).
type CursorAgentFollowupRequest struct {
	Prompt CursorPrompt `json:"prompt"`
}

// CursorAgent is the agent object of a create response.
type CursorAgent struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
}

// CursorRun is the run object of a create response.
type CursorRun struct {
	ID     string `json:"id,omitempty"`
	Status string `json:"status,omitempty"`
}

// CursorAgentCreateResponse is the body of a successful
// POST /v1/agents: the created agent and its first run. No assistant
// text is available here (it arrives on the run stream), so the
// response conversion works on run results instead.
type CursorAgentCreateResponse struct {
	Agent CursorAgent `json:"agent,omitempty"`
	Run   CursorRun   `json:"run,omitempty"`
}

// CursorResultEvent is the payload of the stream's terminal `result`
// event (and mirrors the documented Get A Run shape).
type CursorResultEvent struct {
	RunID      string `json:"runId,omitempty"`
	Status     string `json:"status,omitempty"` // FINISHED | ...
	Text       string `json:"text,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Cursor Cloud Agents.
// ---------------------------------------------------------------------------

// ConvertCursorCreateRequest converts an OpenAI chat-completions
// request into a create-agent body: the conversation is rendered into
// the prompt text (system messages marked, then the alternating turns)
// and the requested model becomes model.id.
func ConvertCursorCreateRequest(req wire.ChatCompletionRequest) (*CursorAgentCreateRequest, error) {
	text, err := renderCursorConversation(req)
	if err != nil {
		return nil, err
	}
	out := &CursorAgentCreateRequest{Prompt: CursorPrompt{Text: text}}
	if strings.TrimSpace(req.Model) != "" {
		out.Model = &CursorModelSelection{ID: req.Model}
	}
	return out, nil
}

// ConvertCursorFollowupRequest converts a follow-up OpenAI request
// into a POST /v1/agents/{id}/runs body. The agent already holds the
// conversation server-side, so the request must carry only the new
// user turn; anything else is refused rather than re-sent.
func ConvertCursorFollowupRequest(req wire.ChatCompletionRequest) (*CursorAgentFollowupRequest, error) {
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	for i, m := range req.Messages[:len(req.Messages)-1] {
		switch m.Role {
		case "system", "developer", "assistant", "tool":
			return nil, unrep("messages[%d] is a %s message; a follow-up carries only the new user turn (the agent holds the conversation server-side)", i, m.Role)
		case "user":
			return nil, unrep("messages[%d] is an extra user message; a follow-up carries only the new user turn", i)
		default:
			return nil, unrep("messages[%d] role %q has no Cursor equivalent", i, m.Role)
		}
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" {
		return nil, unrep("the request must end with a user message, got role %q", last.Role)
	}
	text, err := translateMessageText(last, len(req.Messages)-1)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(text) == "" {
		return nil, unrep("the follow-up user message is empty")
	}
	return &CursorAgentFollowupRequest{Prompt: CursorPrompt{Text: text}}, nil
}

// renderCursorConversation renders the whole conversation into the
// prompt text: system messages marked as [System], then the
// alternating User:/Assistant: turns, ending with the user turn
// (the create body's prompt is what the agent acts on).
func renderCursorConversation(req wire.ChatCompletionRequest) (string, error) {
	if len(req.Messages) == 0 {
		return "", unrep("messages is empty")
	}
	var b strings.Builder
	for i, m := range req.Messages {
		text, err := translateMessageText(m, i)
		if err != nil {
			return "", err
		}
		if len(m.ToolCalls) > 0 {
			return "", unrep("messages[%d] carries tool calls; the Cursor agent surface has no client tool-call replay", i)
		}
		switch m.Role {
		case "system", "developer":
			if text != "" {
				fmt.Fprintf(&b, "[System]\n%s\n\n", text)
			}
		case "user":
			if text != "" {
				fmt.Fprintf(&b, "User:\n%s\n\n", text)
			}
		case "assistant":
			if text != "" {
				fmt.Fprintf(&b, "Assistant:\n%s\n\n", text)
			}
		case "tool":
			return "", unrep("messages[%d] is a tool result; the Cursor agent surface has no client tool-result replay", i)
		default:
			return "", unrep("messages[%d] role %q has no Cursor equivalent", i, m.Role)
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if strings.TrimSpace(out) == "" {
		return "", unrep("no representable messages (all messages were empty)")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Cursor run result -> OpenAI.
// ---------------------------------------------------------------------------

// ConvertCursorRunResult converts a terminal run result (the `result`
// stream event payload, or the Get A Run response body) into an OpenAI
// chat-completion response. Only FINISHED runs carry an assistant
// reply; any other status is refused.
func ConvertCursorRunResult(result *CursorResultEvent, model string) (*wire.ChatCompletionResponse, error) {
	if result == nil {
		return nil, unrep("cursor run result is nil")
	}
	if result.Status != "" && result.Status != "FINISHED" {
		return nil, &CursorError{Code: "run_status", Message: "run status is " + result.Status + ", not FINISHED"}
	}
	msg := wire.ChatMessage{Role: "assistant"}
	if result.Text != "" {
		msg.Content = mustJSON(result.Text)
	}
	return &wire.ChatCompletionResponse{
		ID:      result.RunID,
		Object:  "chat.completion",
		Model:   model,
		Choices: []wire.ResponseChoice{{Index: 0, Message: msg, FinishReason: "stop"}},
		// Usage: the documented run result carries no token counts;
		// usage stays zero rather than being estimated.
	}, nil
}

// CursorError is an error from the Cursor surface (the `error` stream
// event payload is {code, message}).
type CursorError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Error implements error.
func (e *CursorError) Error() string {
	return fmt.Sprintf("cursor error: %s: %s", e.Code, e.Message)
}

// ---------------------------------------------------------------------------
// Streaming: Cursor run SSE -> OpenAI chat-completion chunks.
//
// Documented event types: status, assistant{text}, thinking{text},
// tool_call{callId,name,status,args?,result?,truncated?},
// interaction_update, heartbeat, result{runId,status,text?,...},
// error{code,message}, done. The result event's text is the final
// assistant reply and is used as the authoritative content (the
// assistant deltas are progress text; Cursor's own example stream
// shows different text in the two). No usage payload exists; usage
// stays zero.
// ---------------------------------------------------------------------------

// TranslateCursorStream reads a Cursor run SSE stream and emits OpenAI
// chat.completion.chunk values via emit, in order. It returns the
// reassembled result when the stream ends. model is the requested
// model name (the stream events carry no model field).
func TranslateCursorStream(r io.Reader, model string, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	if emit == nil {
		emit = func(wire.ChatChunk) error { return nil }
	}
	var res wire.StreamResult
	res.Model = model
	var content strings.Builder
	var resultText string
	sawEvent := false
	emitChunk := func(delta wire.ChunkDelta, finishReason *string, usage *wire.Usage) error {
		return emit(wire.ChatChunk{
			ID:      res.ID,
			Object:  "chat.completion.chunk",
			Model:   res.Model,
			Choices: []wire.ChunkChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
			Usage:   usage,
		})
	}

	err := readSSE(r, func(ev sseEvent) error {
		data := strings.TrimSpace(ev.data)
		if data == "" {
			return nil
		}
		switch ev.name {
		case "status", "interaction_update", "heartbeat", "":
			// Sticky framing event, the richer SDK-shape duplicate
			// of the simplified events, and keep-alives: no
			// conversation content; ignored.
			return nil
		case "assistant":
			var payload struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed Cursor assistant event: %w", err)
			}
			if payload.Text == "" {
				return nil
			}
			sawEvent = true
			content.WriteString(payload.Text)
			return emitChunk(wire.ChunkDelta{Role: "assistant", Content: payload.Text}, nil, nil)
		case "thinking":
			// Internal reasoning deltas are not visible content;
			// the documented simplified events carry no replayable
			// reasoning state, so they are dropped (recorded).
			return nil
		case "tool_call":
			var payload struct {
				CallID string          `json:"callId"`
				Name   string          `json:"name"`
				Status string          `json:"status"` // running | completed
				Args   json.RawMessage `json:"args,omitempty"`
			}
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed Cursor tool_call event: %w", err)
			}
			// The tool invocation is emitted once, on the running
			// event; the completed event's result has no OpenAI
			// equivalent (tool results feed back into the agent
			// loop server-side) and is skipped (recorded).
			if payload.Status != "running" {
				return nil
			}
			if payload.Name == "" {
				return unrep("stream tool_call event has no name")
			}
			args := strings.TrimSpace(string(payload.Args))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return unrep("stream tool_call args are not valid JSON")
			}
			sawEvent = true
			return emitChunk(wire.ChunkDelta{ToolCalls: []wire.ChunkToolCall{{
				Index: 0,
				ID:    payload.CallID,
				Type:  "function",
				Function: wire.ChunkFunction{
					Name:      payload.Name,
					Arguments: args,
				},
			}}}, nil, nil)
		case "result":
			var payload CursorResultEvent
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				return fmt.Errorf("translate: malformed Cursor result event: %w", err)
			}
			sawEvent = true
			if payload.Status != "" && payload.Status != "FINISHED" {
				return &CursorError{Code: "run_status", Message: "run status is " + payload.Status + ", not FINISHED"}
			}
			// The result event's text is the final assistant reply.
			resultText = payload.Text
			res.ID = payload.RunID
			return nil
		case "error":
			var ce CursorError
			if err := json.Unmarshal([]byte(data), &ce); err != nil {
				return fmt.Errorf("translate: malformed Cursor error event: %w", err)
			}
			return &ce
		case "done":
			// Stream-complete sentinel; nothing to translate.
			return nil
		default:
			// An unknown named event must not kill the stream.
			return nil
		}
	})
	if err != nil {
		return wire.StreamResult{}, err
	}
	if !sawEvent {
		return wire.StreamResult{}, fmt.Errorf("translate: Cursor SSE stream ended with no translatable events (empty)")
	}
	if resultText != "" {
		res.Content = resultText
	} else {
		res.Content = content.String()
	}
	res.FinishReason = "stop"
	fr := res.FinishReason
	if err := emitChunk(wire.ChunkDelta{}, &fr, nil); err != nil {
		return wire.StreamResult{}, err
	}
	return res, nil
}
