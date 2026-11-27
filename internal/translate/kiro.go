package translate

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Pair: openai <-> kiro (AWS Kiro, generateAssistantResponse).
//
// SHAPE PROVENANCE (recorded honestly): Kiro's model surface is the AWS
// CodeWhisperer / Q Developer "generateAssistantResponse" API. It is
// not publicly documented as a stable JSON contract; the shapes below
// are the protocol facts verifiable from Kiro CLI request captures
// published by AWS Builder Library (builder.aws.com, "kiro-cli agents"
// request JSON) and from open-source clients (KiroStudio, Kiro-Go,
// kiro2cc, ki2api) that exercise the same surface:
//
//	{
//	  "conversationState": {
//	    "conversationId": "<uuid>",
//	    "history": [ {userInputMessage, assistantResponseMessage}, ... ],
//	    "currentMessage": {
//	      "userInputMessage": {
//	        "content": "...", "modelId": "...", "origin": "...",
//	        "userInputMessageContext": {"tools": [...]}
//	      }
//	    },
//	    "chatTriggerType": "MANUAL",
//	    "agentTaskType": "vibe"
//	  },
//	  "profileArn": "arn:aws:codewhisperer:..."
//	}
//
// The upstream assistant side streams assistantResponseEvent payloads
// ({assistantResponseEvent: {content: "<fragment>"}}).
//
// RECORDED DIVERGENCES from an ideal full mapping:
//   - Sampling parameters (temperature, top_p, max_tokens, stop) have
//     no field in the captured conversationState shape. They are
//     dropped, never invented.
//   - There is no system-prompt field; the system/developer message is
//     folded into the first user message's content (the approach every
//     captured community client uses).
//   - There is no dedicated tool-result history entry in the captured
//     shape; a tool result is folded into the following user message's
//     content text (again the community-client approach).
//   - The raw transport is AWS event-stream framing; this translator
//     consumes the JSON event payloads (the transport layer handles
//     framing), re-emitted as SSE by the client adapter.
// ---------------------------------------------------------------------------

// KiroToolSpec is one entry of userInputMessageContext.tools
// (CodeWhisperer toolSpecification shape).
type KiroToolSpec struct {
	ToolSpec struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		InputSchema struct {
			JSON json.RawMessage `json:"json,omitempty"`
		} `json:"inputSchema,omitempty"`
	} `json:"toolSpec"`
}

// KiroUserInputMessageContext is the context block of a user message.
type KiroUserInputMessageContext struct {
	Tools []KiroToolSpec `json:"tools,omitempty"`
}

// KiroUserInputMessage is a user turn (current message or history).
type KiroUserInputMessage struct {
	Content                 string                       `json:"content"`
	ModelID                 string                       `json:"modelId,omitempty"`
	Origin                  string                       `json:"origin,omitempty"`
	UserInputMessageContext *KiroUserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

// KiroToolUse is one tool invocation inside an assistant response.
// Input is an object (same string->object translation as CLOVA's
// toolCall arguments).
type KiroToolUse struct {
	ToolUseID string          `json:"toolUseId"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"` // JSON object
}

// KiroAssistantContentBlock is one block of an assistant response's
// content array: {text: "..."} or {toolUse: {...}}.
type KiroAssistantContentBlock struct {
	Text    string       `json:"text,omitempty"`
	ToolUse *KiroToolUse `json:"toolUse,omitempty"`
}

// KiroAssistantResponseMessage is an assistant turn (history or a
// complete non-streamed response).
type KiroAssistantResponseMessage struct {
	Content []KiroAssistantContentBlock `json:"content"`
}

// KiroHistoryEntry is one user/assistant pair of conversation history.
type KiroHistoryEntry struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

// KiroCurrentMessage wraps the current user message.
type KiroCurrentMessage struct {
	UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
}

// KiroConversationState is the conversationState block of a request.
type KiroConversationState struct {
	ConversationID  string             `json:"conversationId"`
	CurrentMessage  KiroCurrentMessage `json:"currentMessage"`
	History         []KiroHistoryEntry `json:"history,omitempty"`
	ChatTriggerType string             `json:"chatTriggerType"`         // "MANUAL"
	AgentTaskType   string             `json:"agentTaskType,omitempty"` // "vibe"
}

// KiroRequest is the generateAssistantResponse request body.
type KiroRequest struct {
	ConversationState KiroConversationState `json:"conversationState"`
	ProfileArn        string                `json:"profileArn,omitempty"`
}

// KiroOrigin is the value sent in userInputMessage.origin. Kiro CLI
// captures show the client's own origin ("KIRO_CLI", "AI_EDITOR", ...);
// this router speaks as the user, so it uses "USER" (the value captured
// from the KiroStudio client).
const KiroOrigin = "USER"

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Kiro generateAssistantResponse.
// ---------------------------------------------------------------------------

// ConvertKiroRequest converts an OpenAI chat-completions request into
// a Kiro generateAssistantResponse body. conversationID is the caller's
// conversation identifier; when empty a random UUID v4 is generated
// (Kiro requires one per request). The request must end with a
// non-empty user turn — that turn becomes currentMessage; everything
// before it is rendered as user/assistant history pairs.
func ConvertKiroRequest(req wire.ChatCompletionRequest, conversationID string) (*KiroRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty (it becomes userInputMessage.modelId)")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	if conversationID == "" {
		id, err := newUUIDv4()
		if err != nil {
			return nil, fmt.Errorf("translate: generate conversationId: %w", err)
		}
		conversationID = id
	}

	out := &KiroRequest{
		ConversationState: KiroConversationState{
			ConversationID:  conversationID,
			ChatTriggerType: "MANUAL",
			AgentTaskType:   "vibe",
		},
	}

	tools := convertKiroTools(req.Tools)

	// systemPrompt collects the system/developer text; there is no
	// system field on this surface, so it is folded into the first
	// user-turn content. History entries are user/assistant pairs:
	// a user turn opens an entry, the following assistant turn
	// completes it. Tool results fold into the user-turn content
	// that follows them (community-client approach), including the
	// trailing one that becomes currentMessage.
	var systemPrompt strings.Builder
	var toolBuf strings.Builder
	var pendingUser strings.Builder
	var history []KiroHistoryEntry

	// withSystem prepends the not-yet-folded system prompt to the
	// first user-turn content.
	withSystem := func(text string) string {
		if systemPrompt.Len() == 0 {
			return text
		}
		sys := systemPrompt.String()
		systemPrompt.Reset()
		return sys + "\n\n" + text
	}
	// flushAll closes the pending user turn(s) into history entries:
	// folded tool results and then the user text, in message order.
	flushAll := func() {
		if toolBuf.Len() > 0 {
			history = append(history, KiroHistoryEntry{
				UserInputMessage: &KiroUserInputMessage{Content: withSystem(toolBuf.String()), Origin: KiroOrigin},
			})
			toolBuf.Reset()
		}
		if pendingUser.Len() > 0 {
			history = append(history, KiroHistoryEntry{
				UserInputMessage: &KiroUserInputMessage{Content: withSystem(pendingUser.String()), Origin: KiroOrigin},
			})
			pendingUser.Reset()
		}
	}

	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			text, err := translateMessageText(m, i)
			if err != nil {
				return nil, err
			}
			if text != "" {
				if systemPrompt.Len() > 0 {
					systemPrompt.WriteString("\n\n")
				}
				systemPrompt.WriteString(text)
			}
		case "assistant":
			flushAll()
			if len(history) == 0 || history[len(history)-1].AssistantResponseMessage != nil {
				return nil, unrep("messages[%d] is an assistant message without a preceding user turn (Kiro history entries are user/assistant pairs)", i)
			}
			var text string
			if len(m.Content) > 0 {
				t, err := translateMessageText(m, i)
				if err != nil {
					return nil, err
				}
				text = t
			}
			blocks := make([]KiroAssistantContentBlock, 0, 1+len(m.ToolCalls))
			if text != "" {
				blocks = append(blocks, KiroAssistantContentBlock{Text: text})
			}
			for _, tc := range m.ToolCalls {
				if tc.Type != "" && tc.Type != "function" {
					return nil, unrep("messages[%d] tool call type %q is not \"function\"", i, tc.Type)
				}
				input, err := openAIArgsAsObject(tc.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("messages[%d] tool call %q: %w", i, tc.Function.Name, err)
				}
				blocks = append(blocks, KiroAssistantContentBlock{
					ToolUse: &KiroToolUse{
						ToolUseID: tc.ID,
						Name:      tc.Function.Name,
						Input:     input,
					},
				})
			}
			history[len(history)-1].AssistantResponseMessage = &KiroAssistantResponseMessage{Content: blocks}
		case "tool":
			// No dedicated tool-result history entry exists in the
			// captured shape; the result joins the following user
			// turn's content text (community-client approach).
			text, err := translateMessageText(m, i)
			if err != nil {
				return nil, err
			}
			toolBuf.WriteString("\nTool result (call " + m.ToolCallID + "):\n" + text)
		case "user":
			text, err := translateMessageText(m, i)
			if err != nil {
				return nil, err
			}
			pendingUser.WriteString(text)
		default:
			return nil, unrep("messages[%d] role %q has no Kiro equivalent", i, m.Role)
		}
	}

	// The final user turn is the current message; tool results that
	// trail it (or lead it) fold into its content, in message order.
	if toolBuf.Len() == 0 && pendingUser.Len() == 0 {
		return nil, unrep("request must end with a user message (it becomes currentMessage)")
	}
	var current strings.Builder
	if toolBuf.Len() > 0 {
		current.WriteString(strings.TrimSpace(toolBuf.String()))
	}
	if pendingUser.Len() > 0 {
		if current.Len() > 0 {
			current.WriteString("\n\n")
		}
		current.WriteString(pendingUser.String())
	}
	if systemPrompt.Len() > 0 && !anyUserInHistory(history) {
		sys := systemPrompt.String()
		systemPrompt.Reset()
		current.WriteString(sys + "\n\n" + current.String())
	}
	out.ConversationState.CurrentMessage = KiroCurrentMessage{
		UserInputMessage: KiroUserInputMessage{
			Content:                 current.String(),
			ModelID:                 req.Model,
			Origin:                  KiroOrigin,
			UserInputMessageContext: tools,
		},
	}
	out.ConversationState.History = history
	return out, nil
}

// anyUserInHistory reports whether any history entry carries a user
// message (i.e. the system prompt was not the only preceding content).
func anyUserInHistory(history []KiroHistoryEntry) bool {
	for _, h := range history {
		if h.UserInputMessage != nil {
			return true
		}
	}
	return false
}

// convertKiroTools converts OpenAI tool definitions to CodeWhisperer
// toolSpec entries for userInputMessageContext.tools.
func convertKiroTools(tools []wire.ToolDefinition) *KiroUserInputMessageContext {
	if len(tools) == 0 {
		return nil
	}
	ctx := &KiroUserInputMessageContext{}
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue // non-function tools are skipped, not invented
		}
		if strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		var spec KiroToolSpec
		spec.ToolSpec.Name = t.Function.Name
		spec.ToolSpec.Description = t.Function.Description
		params := t.Function.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object"}`)
		}
		spec.ToolSpec.InputSchema.JSON = params
		ctx.Tools = append(ctx.Tools, spec)
	}
	if len(ctx.Tools) == 0 {
		return nil
	}
	return ctx
}

// newUUIDv4 generates a random RFC 4122 version-4 UUID.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ---------------------------------------------------------------------------
// Response conversion: Kiro assistant response -> OpenAI.
// ---------------------------------------------------------------------------

// ConvertKiroResponse converts a complete Kiro assistant turn (the same
// assistantResponseMessage shape the request history carries) into an
// OpenAI chat-completion response.
func ConvertKiroResponse(msg *KiroAssistantResponseMessage, model string) (*wire.ChatCompletionResponse, error) {
	if msg == nil {
		return nil, unrep("kiro assistant response is nil")
	}
	var text strings.Builder
	var toolCalls []wire.ToolCall
	for i, block := range msg.Content {
		switch {
		case block.ToolUse != nil:
			if strings.TrimSpace(block.ToolUse.Name) == "" {
				return nil, unrep("content[%d] is a toolUse with no name", i)
			}
			input := strings.TrimSpace(string(block.ToolUse.Input))
			if input == "" {
				input = "{}"
			}
			if !json.Valid([]byte(input)) {
				return nil, unrep("content[%d] toolUse input is not valid JSON", i)
			}
			id := block.ToolUse.ToolUseID
			if id == "" {
				// The surface permits an absent toolUseId; OpenAI
				// tool calls need one, so a sequential id is minted.
				id = fmt.Sprintf("toolu_kiro_%d", i)
			}
			toolCalls = append(toolCalls, wire.ToolCall{
				ID:   id,
				Type: "function",
				Function: wire.ToolCallFunction{
					Name:      block.ToolUse.Name,
					Arguments: input,
				},
			})
		case block.Text != "":
			text.WriteString(block.Text)
		default:
			return nil, unrep("content[%d] has an unrecognised shape", i)
		}
	}
	outMsg := wire.ChatMessage{Role: "assistant"}
	if text.Len() > 0 {
		outMsg.Content = mustJSON(text.String())
	}
	if len(toolCalls) > 0 {
		outMsg.ToolCalls = toolCalls
	}
	return &wire.ChatCompletionResponse{
		ID:      "",
		Object:  "chat.completion",
		Model:   model,
		Choices: []wire.ResponseChoice{{Index: 0, Message: outMsg, FinishReason: "stop"}},
		// Usage: the captured event stream carries no token counts;
		// usage stays zero rather than being estimated.
	}, nil
}

// ---------------------------------------------------------------------------
// Streaming: Kiro assistantResponseEvent payloads -> OpenAI
// chat-completion chunks.
//
// The upstream sends JSON events whose payload is
// {assistantResponseEvent: {content: "<fragment>"}} (word/phrase-level
// fragments per the AWS Builder Library capture) or an error payload
// ({error: {code, message}}). There is no finish marker or usage
// payload in the captured shape: the stream ends as "stop" when the
// payloads stop, and usage stays zero.
// ---------------------------------------------------------------------------

// TranslateKiroStream reads a Kiro event stream (SSE-framed JSON event
// payloads) and emits OpenAI chat.completion.chunk values via emit, in
// order. It returns the reassembled result when the stream ends.
func TranslateKiroStream(r io.Reader, model string, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	if emit == nil {
		emit = func(wire.ChatChunk) error { return nil }
	}
	var res wire.StreamResult
	res.Model = model
	var content strings.Builder
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
		if data == "" || data == "[DONE]" {
			return nil
		}
		var probe struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &probe); err == nil && len(probe.Error) > 0 {
			return &KiroError{Message: kiroErrorMessage(probe.Error)}
		}
		var parsed struct {
			AssistantResponseEvent *struct {
				Content string `json:"content"`
			} `json:"assistantResponseEvent"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("translate: malformed Kiro event payload: %w", err)
		}
		if parsed.AssistantResponseEvent == nil {
			// Unrecognised event payloads (metadata, keep-alives)
			// are tolerated, never fatal.
			return nil
		}
		fragment := parsed.AssistantResponseEvent.Content
		if fragment == "" {
			return nil
		}
		sawEvent = true
		content.WriteString(fragment)
		return emitChunk(wire.ChunkDelta{Role: "assistant", Content: fragment}, nil, nil)
	})
	if err != nil {
		return wire.StreamResult{}, err
	}
	if !sawEvent {
		return wire.StreamResult{}, fmt.Errorf("translate: Kiro event stream ended with no content events (empty)")
	}
	res.Content = content.String()
	res.FinishReason = "stop"
	fr := res.FinishReason
	if err := emitChunk(wire.ChunkDelta{}, &fr, nil); err != nil {
		return wire.StreamResult{}, err
	}
	return res, nil
}

// KiroError is an error event from the Kiro surface.
type KiroError struct {
	Message string
}

// Error implements error.
func (e *KiroError) Error() string {
	return "kiro error: " + e.Message
}

// kiroErrorMessage extracts the message from an error payload
// ({error: "..."} or {error: {message: "..."}}).
func kiroErrorMessage(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && s != "" {
		return s
	}
	var obj struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Message != "" {
			return obj.Message
		}
		return obj.Code
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	return trimmed
}
