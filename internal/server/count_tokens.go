// Token-count endpoints (M8.3, Area 14 §14a).
//
// POST /v1/messages/count_tokens — the Anthropic Messages token-count
// surface: count the prompt tokens of a Messages-API request body
// locally, with the existing tokencount package (CountPromptWire,
// Anthropic wire framing). No upstream call, ever — pure local
// counting, so this endpoint works with no provider configured and
// never spends anything.
//
// GET /v1/responses/input_tokens?response_id=<id> — recount the
// input tokens of a stored Responses transcript with the same
// counter (OpenAI-rule framing, the same rule estimatePromptTokens
// uses for TurnInputTokens). 404 honestly when the id does not
// exist; never invent a count.
//
// Registration (coordinator-owned — this file must not touch
// server.go or guard.go): both routes are TierManagement,
// gateway-key auth, scope "chat" (proposed; coordinator assigns at
// registration). The handlers themselves do no auth: like every
// other /v1 handler, auth is applied by the guard at registration.
//
// Honesty rules, inherited from the tokencount package doc:
//   - A local count is an ESTIMATE, never a provider-reported
//     number. The response says so: `estimated` is true whenever
//     the count is a chars/4 over-cap estimate OR the model name
//     matched no known encoding and cl100k_base was assumed
//     (`encoding_fallback`). Only a matched encoding on an
//     exactly-encoded prompt reports estimated=false.
//   - Unknown models never error: they resolve to cl100k_base
//     with encoding_fallback=true (tokencount.encodingForModel).
//   - No prices, no caps, no model data is invented anywhere
//     here: the response carries counts and the encoding only.
//   - Image blocks count 0: the BPE counter cannot count image
//     tokens, and silently pricing them as text would be an
//     invented number. This undercounts image-bearing prompts
//     relative to what an upstream would bill — see the
//     Limitations section below.
//
// Array-form message content counts ONCE (M1.9): the translated
// provider.Message carries both Content (concatenated text) and
// Parts (the text parts), and tokencount.CountPrompt takes the
// text parts when Parts is set — never Content + Parts together.
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// ---------------------------------------------------------------------------
// Shared response shape.
// ---------------------------------------------------------------------------

// tokenCountResponse is the response body of both endpoints: the
// counted tokens plus the encoding and whether the count is exact
// or estimated. `estimated` is true when the count is a chars/4
// over-cap estimate or when the model fell back to cl100k_base
// (encoding_fallback); only a matched encoding on an exactly
// encoded prompt reports estimated=false. No cost, no price, no
// cap is ever included — this endpoint counts tokens, nothing else.
type tokenCountResponse struct {
	// InputTokens is the locally counted prompt/input token count.
	InputTokens int `json:"input_tokens"`
	// Model is the model the count was computed for (echoed).
	Model string `json:"model"`
	// Encoding is the tiktoken encoding used ("cl100k_base" or
	// "o200k_base").
	Encoding string `json:"encoding"`
	// Estimated is true when InputTokens is an estimate rather
	// than an exact BPE count for the named model.
	Estimated bool `json:"estimated"`
	// EncodingFallback is true when the model name matched no
	// known encoding and cl100k_base was assumed.
	EncodingFallback bool `json:"encoding_fallback"`
}

// responsesInputTokensResponse is the GET /v1/responses/input_tokens
// body: the shared count fields plus the response id and how many
// transcript messages were counted as input. MessagesCounted is
// the input side of the stored transcript — every message except
// the trailing assistant turn, which is this response's own
// output, not its input.
type responsesInputTokensResponse struct {
	tokenCountResponse
	ResponseID      string `json:"response_id"`
	MessagesCounted int    `json:"messages_counted"`
}

// estimatedFrom reports the response-level estimated flag for a
// TokenCount: an over-cap chars/4 estimate, or a fallback
// encoding, both make the count an estimate.
func estimatedFrom(c tokencount.TokenCount) bool {
	return c.Estimated || c.Fallback
}

// ---------------------------------------------------------------------------
// POST /v1/messages/count_tokens
// ---------------------------------------------------------------------------

// messagesCountTokensRequest is the decode shape for POST
// /v1/messages/count_tokens: the Anthropic Messages request subset
// that carries countable tokens — model, system, messages, tools.
// Fields outside this subset (max_tokens, temperature, stream,
// cache_control, metadata, ...) are ignored: they carry no prompt
// tokens and this endpoint never forwards anything upstream, so
// there is nothing to honour or refuse.
type messagesCountTokensRequest struct {
	Model    string                 `json:"model"`
	System   json.RawMessage        `json:"system,omitempty"`
	Messages []messagesCountMessage `json:"messages"`
	Tools    []messagesCountTool    `json:"tools,omitempty"`
}

// messagesCountMessage is one message of a Messages request:
// role plus content as a string or an array of blocks.
type messagesCountMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// messagesCountBlock is one content block of a Messages request.
// Only the fields that carry countable text are decoded: text
// blocks, tool_use input, tool_result content. Image (and other
// non-text) blocks carry no countable text — see the package doc.
type messagesCountBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use blocks (assistant messages):
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result blocks (user messages):
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
}

// messagesCountTool is one tool definition of a Messages request
// (Anthropic shape: name, description, input_schema). The type
// field ("custom" on newer clients) is accepted and ignored —
// the counter only needs the function shape.
type messagesCountTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// messagesSystemText decodes a Messages `system` field — a string
// or an array of text blocks — into plain text plus, for array
// form, the wire parts (so the counter counts array form once,
// M1.9). An empty/missing system is "" with no parts.
func messagesSystemText(raw json.RawMessage) (string, []provider.ContentPart, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", nil, ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil, ""
	}
	var blocks []messagesCountBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", nil, "system must be a string or an array of text blocks"
	}
	var sb strings.Builder
	var parts []provider.ContentPart
	for _, b := range blocks {
		if b.Type != "" && b.Type != "text" {
			return "", nil, "system blocks must be text blocks"
		}
		if b.Text != "" {
			sb.WriteString(b.Text)
			parts = append(parts, provider.ContentPart{Type: "text", Text: b.Text})
		}
	}
	return sb.String(), parts, ""
}

// messagesBlockText decodes a tool_result block's content — a
// string or an array of text blocks — into plain text.
func messagesBlockText(raw json.RawMessage) (string, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, ""
	}
	var blocks []messagesCountBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", "tool_result content must be a string or an array of text blocks"
	}
	var sb strings.Builder
	for _, b := range blocks {
		// Any block carrying a text field contributes it;
		// blocks without text (e.g. image results) count 0.
		sb.WriteString(b.Text)
	}
	return sb.String(), ""
}

// messagesCountToChat translates a decoded Messages count request
// into the canonical chat request the counter consumes. The
// translation is counting-only: roles, text parts, tool_use input
// (as tool-call arguments) and tool_result text are carried; image
// blocks are dropped (they carry no countable text — see the
// package doc). A message that arrives in array form keeps its
// Parts, so CountPrompt counts it once (M1.9), never as
// Content + Parts.
func messagesCountToChat(req messagesCountTokensRequest) (provider.ChatRequest, string) {
	chat := provider.ChatRequest{Model: req.Model}
	if text, parts, errMsg := messagesSystemText(req.System); errMsg != "" {
		return chat, errMsg
	} else if text != "" {
		m := provider.Message{Role: "system", Content: text}
		if len(parts) > 0 {
			m.Parts = parts
		}
		chat.Messages = append(chat.Messages, m)
	}
	for i, msg := range req.Messages {
		switch msg.Role {
		case "user", "assistant":
		default:
			return chat, "messages[" + strconv.Itoa(i) + "].role must be \"user\" or \"assistant\""
		}
		m, errMsg := messagesCountMessageToChat(msg, i)
		if errMsg != "" {
			return chat, errMsg
		}
		chat.Messages = append(chat.Messages, m)
	}
	for i, t := range req.Tools {
		if strings.TrimSpace(t.Name) == "" {
			return chat, "tools[" + strconv.Itoa(i) + "].name is required"
		}
		td := provider.ToolDefinition{Type: "function"}
		td.Function.Name = t.Name
		td.Function.Description = t.Description
		td.Function.Parameters = t.InputSchema
		chat.Tools = append(chat.Tools, td)
	}
	return chat, ""
}

// messagesCountMessageToChat translates one Messages message into
// a canonical chat message.
func messagesCountMessageToChat(msg messagesCountMessage, index int) (provider.Message, string) {
	m := provider.Message{Role: msg.Role}
	trimmed := strings.TrimSpace(string(msg.Content))
	if trimmed == "" || trimmed == "null" {
		// An assistant message carrying only tool calls (or an
		// empty content) is legal; a user message with no
		// content counts 0 and is kept so framing stays honest.
		return m, ""
	}
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		m.Content = asString
		return m, ""
	}
	var blocks []messagesCountBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return m, "messages[" + strconv.Itoa(index) + "].content must be a string or an array of content blocks"
	}
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				sb.WriteString(b.Text)
				m.Parts = append(m.Parts, provider.ContentPart{Type: "text", Text: b.Text})
			}
		case "thinking":
			// Thinking text replayed in an assistant turn is
			// part of the prompt the upstream bills, so it
			// counts as text. redacted_thinking carries no
			// text and counts 0.
			if b.Text != "" {
				sb.WriteString(b.Text)
				m.Parts = append(m.Parts, provider.ContentPart{Type: "text", Text: b.Text})
			}
		case "tool_use":
			if msg.Role != "assistant" {
				return m, "messages[" + strconv.Itoa(index) + "]: tool_use blocks belong on assistant messages"
			}
			args := "{}"
			if len(b.Input) > 0 && string(b.Input) != "null" {
				args = string(b.Input)
			}
			m.ToolCalls = append(m.ToolCalls, provider.ToolCall{
				ID:   b.ID,
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "tool_result":
			if msg.Role != "user" {
				return m, "messages[" + strconv.Itoa(index) + "]: tool_result blocks belong on user messages"
			}
			text, errMsg := messagesBlockText(b.Content)
			if errMsg != "" {
				return m, "messages[" + strconv.Itoa(index) + "]: " + errMsg
			}
			if text != "" {
				sb.WriteString(text)
				m.Parts = append(m.Parts, provider.ContentPart{Type: "text", Text: text})
			}
			if b.ToolUseID != "" && m.ToolCallID == "" {
				m.ToolCallID = b.ToolUseID
			}
		case "image", "redacted_thinking":
			// No countable text: image tokens cannot be
			// counted by a BPE counter (see the package doc),
			// redacted thinking carries no text at all.
		default:
			// Unknown block type: count any text field it
			// carries rather than silently undercounting,
			// and never refuse a request for a block shape
			// this endpoint does not need to understand.
			if b.Text != "" {
				sb.WriteString(b.Text)
				m.Parts = append(m.Parts, provider.ContentPart{Type: "text", Text: b.Text})
			}
		}
	}
	m.Content = sb.String()
	return m, ""
}

// handleMessagesCountTokens serves POST /v1/messages/count_tokens.
// Pure local counting: the request body is decoded and counted
// with tokencount.CountPromptWire (Anthropic wire framing — the
// Messages API's own wire, +1 role-label token per message). No
// engine call, no provider call, no ledger write, no budget
// check: counting must never spend, refuse on quota, or require
// a provider to be configured. Invalid bodies are 400s naming
// the problem; a counting failure (a packaging defect in the
// vendored BPE tables, the only way CountPromptWire errors) is
// an honest 500, never a synthesised count.
func (s *Server) handleMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_messages_count_tokens", v)
			s.writePanicFailure(w)
		}
	}()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeAnthropicError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	var req messagesCountTokensRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeAnthropicError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages is required")
		return
	}
	chat, errMsg := messagesCountToChat(req)
	if errMsg != "" {
		writeAnthropicError(w, http.StatusBadRequest, errMsg)
		return
	}
	count, err := tokencount.CountPromptWire(req.Model, provider.WireFormatAnthropic, chat)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "token counting failed")
		return
	}
	writeJSON(w, http.StatusOK, tokenCountResponse{
		InputTokens:      count.Tokens,
		Model:            req.Model,
		Encoding:         count.Encoding,
		Estimated:        estimatedFrom(count),
		EncodingFallback: count.Fallback,
	})
}

// ---------------------------------------------------------------------------
// GET /v1/responses/input_tokens
// ---------------------------------------------------------------------------

// handleResponsesInputTokens serves GET
// /v1/responses/input_tokens?response_id=<id> (the `id` query
// parameter is accepted as an alias). It recounts the stored
// transcript's input side with the same counter the prompt gates
// use — CountPromptWire with the OpenAI-rule framing, the same
// rule estimatePromptTokens uses for TurnInputTokens — so the
// recount and the stored turn count agree except for the
// per-response instructions (not stored in the transcript) and
// any dangling tool calls stripped from the engine request (the
// stored transcript keeps the full history). The input side is
// every transcript message except the trailing assistant turn,
// which is this response's own output, not its input; earlier
// assistant turns stay counted, they were sent as context.
//
// An unknown id is an honest 404, never a synthesised count. A
// transcript that cannot decode is a server-side corruption: an
// honest 500, never the client's fault to work around. This
// recount is a LOCAL estimate of the prompt side — it is not the
// provider-reported usage the create served (when the upstream
// reported one), and it must never be presented as such.
func (s *Server) handleResponsesInputTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	defer func() {
		if v := recover(); v != nil {
			s.noteServerPanic("server_responses_input_tokens", v)
			s.writePanicFailure(w)
		}
	}()
	id := r.URL.Query().Get("response_id")
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" {
		writeError(w, http.StatusBadRequest, "response_id query parameter is required")
		return
	}
	if s.responses == nil {
		writeError(w, http.StatusNotFound, "response not found: "+id)
		return
	}
	rec, ok := s.responses.Get(id, s.callerKeyID(r))
	if !ok {
		writeError(w, http.StatusNotFound, "response not found: "+id)
		return
	}
	var transcript []provider.Message
	if err := json.Unmarshal(rec.TranscriptJSON, &transcript); err != nil {
		writeServerError(w, http.StatusInternalServerError, "stored response transcript is unreadable")
		return
	}
	input := transcript
	if len(input) > 0 && input[len(input)-1].Role == "assistant" {
		input = input[:len(input)-1]
	}
	var count tokencount.TokenCount
	if len(input) == 0 {
		// No input side (a transcript that is only an
		// assistant turn): 0 tokens, with the encoding
		// resolved so the response still names it honestly.
		// CountPromptWire would add reply-priming framing
		// for an empty prompt — a count for nothing.
		c, err := tokencount.Count(rec.Model, "")
		if err != nil {
			writeServerError(w, http.StatusInternalServerError, "token counting failed")
			return
		}
		count = c
	} else {
		c, err := tokencount.CountPromptWire(rec.Model, provider.WireFormatOpenAI, provider.ChatRequest{Model: rec.Model, Messages: input})
		if err != nil {
			writeServerError(w, http.StatusInternalServerError, "token counting failed")
			return
		}
		count = c
	}
	writeJSON(w, http.StatusOK, responsesInputTokensResponse{
		tokenCountResponse: tokenCountResponse{
			InputTokens:      count.Tokens,
			Model:            rec.Model,
			Encoding:         count.Encoding,
			Estimated:        estimatedFrom(count),
			EncodingFallback: count.Fallback,
		},
		ResponseID:      id,
		MessagesCounted: len(input),
	})
}
