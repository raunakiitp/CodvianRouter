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
// Pair: openai <-> clova (Naver Cloud CLOVA Studio Chat Completions v3).
//
// Shapes verified against NCP's own documentation
// (api.ncloud-docs.com/docs/en/clovastudio-chatcompletionsv3 and
// .../clovastudio-chatcompletionsv3-fc): POST
// /v3/chat-completions/{modelName} carries a messages array with
// roles system|user|assistant|tool, camelCase hyperparameters
// (topP/topK/maxTokens/maxCompletionTokens/temperature/
// repetitionPenalty/stop/seed), and OpenAI-like tools with camelCase
// toolCalls / toolCallId. Notably, toolCall function.arguments is a
// JSON *object* on this surface, not the string OpenAI uses — the
// conversion decodes and re-encodes it.
// ---------------------------------------------------------------------------

// ClovaToolCall is one CLOVA Studio v3 tool call (camelCase toolCalls).
// Function.Arguments is an object on this wire (unlike OpenAI's
// string-encoded arguments); the converter translates between the two.
type ClovaToolCall struct {
	ID       string                `json:"id"`
	Type     string                `json:"type"` // "function"
	Function ClovaToolCallFunction `json:"function"`
}

// ClovaToolCallFunction is the function payload of a ClovaToolCall.
type ClovaToolCallFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"` // JSON object
}

// ClovaTool is one CLOVA Studio v3 function tool definition.
type ClovaTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// ClovaMessage is one CLOVA Studio v3 conversation message. Content is
// a JSON string (or, for image input, an array of typed parts — see
// clovaContentParts); tool results carry ToolCallID.
type ClovaMessage struct {
	Role       string          `json:"role"` // system | user | assistant | tool
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ClovaToolCall `json:"toolCalls,omitempty"`
	ToolCallID string          `json:"toolCallId,omitempty"`
}

// ClovaContentPart is one element of an array-form CLOVA message
// content (image input).
type ClovaContentPart struct {
	Type     string `json:"type"` // "text" | "image_url"
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url,omitempty"`
	} `json:"imageUrl,omitempty"`
	DataURI *struct {
		Data string `json:"data,omitempty"`
	} `json:"dataUri,omitempty"`
}

// ClovaThinking is the thinking block of a CLOVA Studio v3 request.
type ClovaThinking struct {
	Effort string `json:"effort,omitempty"` // "none" | "low" | "medium" | "high"
}

// ClovaRequest is the body of POST /v3/chat-completions/{modelName}.
// The model is a path parameter on this surface, so it is not a body
// field. Generation parameters default to the documented provider
// defaults when omitted (topP 0.8, topK 0, temperature 0.5,
// repetitionPenalty 1.1), so every field is omitted when the OpenAI
// request did not set it.
type ClovaRequest struct {
	Messages            []ClovaMessage `json:"messages"`
	TopP                *float64       `json:"topP,omitempty"`
	TopK                *int           `json:"topK,omitempty"`
	MaxTokens           int            `json:"maxTokens,omitempty"`
	MaxCompletionTokens int            `json:"maxCompletionTokens,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	RepetitionPenalty   *float64       `json:"repetitionPenalty,omitempty"`
	Stop                []string       `json:"stop,omitempty"`
	Seed                *int           `json:"seed,omitempty"`
	IncludeAiFilters    bool           `json:"includeAiFilters,omitempty"`
	Thinking            *ClovaThinking `json:"thinking,omitempty"`
	Tools               []ClovaTool    `json:"tools,omitempty"`
	ToolChoice          any            `json:"toolChoice,omitempty"` // "auto" | "none" | {type,function:{name}}
}

// ClovaStatus is the status block of a CLOVA Studio response; "20000"
// is the documented success code.
type ClovaStatus struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// ClovaUsage is the usage block of a CLOVA Studio response.
type ClovaUsage struct {
	PromptTokens     int `json:"promptTokens,omitempty"`
	CompletionTokens int `json:"completionTokens,omitempty"`
	TotalTokens      int `json:"totalTokens,omitempty"`
}

// ClovaResultMessage is the result.message block of a CLOVA response.
type ClovaResultMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []ClovaToolCall `json:"toolCalls,omitempty"`
}

// ClovaResult is the result block of a CLOVA response.
type ClovaResult struct {
	// Created is a Unix timestamp; the reference documents it as
	// milliseconds while the worked example shows seconds, so the
	// converter normalises (see ConvertClovaResponse).
	Created      int64              `json:"created,omitempty"`
	Usage        ClovaUsage         `json:"usage,omitempty"`
	Message      ClovaResultMessage `json:"message,omitempty"`
	FinishReason string             `json:"finishReason,omitempty"` // length | stop | tool_calls
	Seed         int                `json:"seed,omitempty"`
}

// ClovaResponse is the body of a successful CLOVA Studio v3 call.
type ClovaResponse struct {
	Status ClovaStatus `json:"status,omitempty"`
	Result ClovaResult `json:"result,omitempty"`
}

// ClovaError is a CLOVA Studio error: a non-2xx HTTP body or an SSE
// error event, both shaped {status: {code, message}}.
type ClovaError struct {
	StatusCode int         // 0 for stream errors (HTTP already 200)
	Status     ClovaStatus `json:"status,omitempty"`
}

// Error implements error.
func (e *ClovaError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("clova error: HTTP %d: %s %s", e.StatusCode, e.Status.Code, e.Status.Message)
	}
	return fmt.Sprintf("clova error: %s %s", e.Status.Code, e.Status.Message)
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> CLOVA Studio v3.
// ---------------------------------------------------------------------------

// clovaMaxImages is the documented cap: up to five images per request,
// one per turn (HCX-005).
const clovaMaxImages = 5

// ConvertClovaRequest converts an OpenAI chat-completions request into
// a CLOVA Studio Chat Completions v3 request body. It returns
// *UnrepresentableError for anything the surface cannot represent
// faithfully.
func ConvertClovaRequest(req wire.ChatCompletionRequest) (*ClovaRequest, error) {
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty (the model name is a CLOVA path parameter, sent by the transport)")
	}
	// Documented sampling ranges: temperature 0.00-1.00, topP
	// 0.00 < topP <= 1.00. Values outside are refused, never clamped.
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 1) {
		return nil, unrep("temperature %v is outside CLOVA's 0-1 range", *req.Temperature)
	}
	if req.TopP != nil && (*req.TopP <= 0 || *req.TopP > 1) {
		return nil, unrep("top_p %v is outside CLOVA's 0-1 range for topP", *req.TopP)
	}

	out := &ClovaRequest{
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Stop:              append([]string(nil), req.Stop...),
		MaxTokens:         req.MaxTokens,
		RepetitionPenalty: nil, // no OpenAI equivalent; left at the provider default
	}

	tools, toolChoice, err := convertClovaTools(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.Tools = tools
	out.ToolChoice = toolChoice

	imageCount := 0
	systemCount := 0
	for i, m := range req.Messages {
		cm, n, err := convertClovaMessage(m, i)
		if err != nil {
			return nil, err
		}
		imageCount += n
		if m.Role == "system" || m.Role == "developer" {
			systemCount++
			// Documented constraint: at most one system message
			// per request. "developer" is a system instruction
			// in OpenAI terms, so it counts against the same limit.
			if systemCount > 1 {
				return nil, unrep("CLOVA accepts at most one system message per request, got %d", systemCount)
			}
			cm.Role = "system"
		}
		if imageCount > clovaMaxImages {
			return nil, unrep("CLOVA accepts at most %d images per request, got more", clovaMaxImages)
		}
		out.Messages = append(out.Messages, cm)
	}
	return out, nil
}

// convertClovaTools converts OpenAI tool definitions and tool_choice
// to the CLOVA v3 forms. tool_choice "required" has no documented
// CLOVA equivalent (the surface documents auto / none / a named
// function), so it is refused rather than silently downgraded.
func convertClovaTools(tools []wire.ToolDefinition, toolChoice any) ([]ClovaTool, any, error) {
	if s, ok := toolChoice.(string); ok && s == "none" {
		return nil, "none", nil
	}
	if len(tools) == 0 {
		if toolChoice == nil {
			return nil, nil, nil
		}
		if s, ok := toolChoice.(string); ok && s == "auto" {
			return nil, nil, nil
		}
		return nil, nil, unrep("tool_choice %v with no tools defined", toolChoice)
	}
	ct := make([]ClovaTool, 0, len(tools))
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
		fn := struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: t.Function.Name, Description: t.Function.Description, Parameters: params}
		ct = append(ct, ClovaTool{Type: "function", Function: fn})
	}
	switch tc := toolChoice.(type) {
	case nil:
		return ct, nil, nil
	case string:
		switch tc {
		case "auto":
			return ct, "auto", nil
		case "required":
			return nil, nil, unrep("tool_choice \"required\" has no CLOVA equivalent (auto/none/named function only)")
		default:
			return nil, nil, unrep("tool_choice %q is not a CLOVA value", tc)
		}
	default:
		// OpenAI named-function form {"type":"function","function":{"name":...}}
		// maps onto CLOVA's {"type":"function","function":{"name":...}} verbatim.
		m, ok := tc.(map[string]any)
		if !ok {
			return nil, nil, unrep("tool_choice of type %T has no CLOVA equivalent", toolChoice)
		}
		fnObj, _ := m["function"].(map[string]any)
		name, _ := fnObj["name"].(string)
		if name == "" {
			return nil, nil, unrep("tool_choice object has no function.name")
		}
		return ct, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": name},
		}, nil
	}
}

// convertClovaMessage converts one OpenAI message to the CLOVA v3
// form. Text-only messages encode content as a JSON string; messages
// with image parts encode the documented array form. Assistant tool
// calls re-encode the string-encoded OpenAI arguments as the object
// form this surface uses; tool results carry toolCallId.
func convertClovaMessage(m wire.ChatMessage, index int) (ClovaMessage, int, error) {
	cm := ClovaMessage{Role: m.Role}
	switch m.Role {
	case "system", "developer":
		text, err := translateMessageText(m, index)
		if err != nil {
			return ClovaMessage{}, 0, err
		}
		cm.Content = mustJSON(text)
		return cm, 0, nil
	case "tool":
		text, err := translateMessageText(m, index)
		if err != nil {
			return ClovaMessage{}, 0, err
		}
		cm.Role = "tool"
		cm.Content = mustJSON(text)
		cm.ToolCallID = m.ToolCallID
		if cm.ToolCallID == "" {
			return ClovaMessage{}, 0, unrep("messages[%d] is a tool result with no tool_call_id", index)
		}
		return cm, 0, nil
	case "assistant":
		cm.Role = "assistant"
		text, err := translateMessageText(m, index)
		if err != nil {
			return ClovaMessage{}, 0, err
		}
		cm.Content = mustJSON(text)
		for _, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return ClovaMessage{}, 0, unrep("messages[%d] tool call type %q is not \"function\"", index, tc.Type)
			}
			args, err := openAIArgsAsObject(tc.Function.Arguments)
			if err != nil {
				return ClovaMessage{}, 0, fmt.Errorf("messages[%d] tool call %q: %w", index, tc.Function.Name, err)
			}
			cm.ToolCalls = append(cm.ToolCalls, ClovaToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: ClovaToolCallFunction{
					Name:      tc.Function.Name,
					Arguments: args,
				},
			})
		}
		return cm, 0, nil
	case "user":
		cm.Role = "user"
		return convertClovaUserContent(m, index)
	default:
		return ClovaMessage{}, 0, unrep("messages[%d] role %q has no CLOVA equivalent", index, m.Role)
	}
}

// convertClovaUserContent decodes the user message content: a plain
// string or an array of text / image_url parts (the array form becomes
// CLOVA's typed content array; a data: URL becomes the documented
// dataUri form, an http(s) URL the imageUrl form).
func convertClovaUserContent(m wire.ChatMessage, index int) (ClovaMessage, int, error) {
	if len(m.Content) == 0 {
		return ClovaMessage{Role: "user", Content: mustJSON("")}, 0, nil
	}
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		return ClovaMessage{Role: "user", Content: m.Content}, 0, nil
	}
	parts, err := openAIContentParts(m.Content)
	if err != nil {
		return ClovaMessage{}, 0, fmt.Errorf("messages[%d]: %w", index, err)
	}
	var clovaParts []ClovaContentPart
	images := 0
	for i, p := range parts {
		switch {
		case p.Text != "":
			clovaParts = append(clovaParts, ClovaContentPart{Type: "text", Text: p.Text})
		case p.ImageURL != nil:
			images++
			part := ClovaContentPart{Type: "image_url"}
			u := p.ImageURL.URL
			if rest, ok := strings.CutPrefix(u, "data:"); ok {
				// data:[<mime>][;base64],<data> -> dataUri.data
				_, b64, found := strings.Cut(rest, ",")
				if !found {
					return ClovaMessage{}, 0, unrep("messages[%d] parts[%d] image data URL is malformed", index, i)
				}
				part.DataURI = &struct {
					Data string `json:"data,omitempty"`
				}{Data: b64}
			} else {
				part.ImageURL = &struct {
					URL string `json:"url,omitempty"`
				}{URL: u}
			}
			clovaParts = append(clovaParts, part)
		default:
			return ClovaMessage{}, 0, unrep("messages[%d] parts[%d] has no CLOVA equivalent", index, i)
		}
	}
	raw, err := json.Marshal(clovaParts)
	if err != nil {
		return ClovaMessage{}, 0, err
	}
	return ClovaMessage{Role: "user", Content: raw}, images, nil
}

// translateMessageText extracts the plain-text content of an OpenAI
// message (string content; array content that is all text is joined).
func translateMessageText(m wire.ChatMessage, index int) (string, error) {
	if len(m.Content) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s, nil
	}
	parts, err := openAIContentParts(m.Content)
	if err != nil {
		return "", fmt.Errorf("messages[%d]: %w", index, err)
	}
	var texts []string
	for i, p := range parts {
		if p.Text == "" {
			return "", unrep("messages[%d] parts[%d] is not text and this context requires text-only content", index, i)
		}
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, ""), nil
}

// openAIContentPart is one decoded OpenAI content part.
type openAIContentPart struct {
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// openAIContentParts decodes OpenAI multi-part message content
// (array of {type, text, image_url}).
func openAIContentParts(raw json.RawMessage) ([]openAIContentPart, error) {
	var parts []openAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, unrep("content is neither a string nor a part array")
	}
	return parts, nil
}

// openAIArgsAsObject converts OpenAI's string-encoded tool-call
// arguments into the JSON object form the CLOVA v3 surface uses.
func openAIArgsAsObject(args string) (json.RawMessage, error) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		trimmed = "{}"
	}
	if !json.Valid([]byte(trimmed)) {
		return nil, unrep("tool-call arguments are not valid JSON")
	}
	return json.RawMessage(trimmed), nil
}

// ---------------------------------------------------------------------------
// Response conversion: CLOVA Studio v3 -> OpenAI.
// ---------------------------------------------------------------------------

// ConvertClovaResponse converts a CLOVA Studio Chat Completions v3
// response into an OpenAI chat-completion response. A response whose
// status block reports a non-success code returns *ClovaError.
func ConvertClovaResponse(resp *ClovaResponse) (*wire.ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("clova response is nil")
	}
	if resp.Status.Code != "" && resp.Status.Code != "20000" {
		return nil, &ClovaError{Status: resp.Status}
	}
	msg := wire.ChatMessage{Role: "assistant"}
	if resp.Result.Message.Content != "" {
		msg.Content = mustJSON(resp.Result.Message.Content)
	}
	for i, tc := range resp.Result.Message.ToolCalls {
		args, err := clovaArgsAsString(tc.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("result.message.toolCalls[%d]: %w", i, err)
		}
		msg.ToolCalls = append(msg.ToolCalls, wire.ToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: wire.ToolCallFunction{
				Name:      tc.Function.Name,
				Arguments: args,
			},
		})
	}
	usage := wire.Usage{
		PromptTokens:     resp.Result.Usage.PromptTokens,
		CompletionTokens: resp.Result.Usage.CompletionTokens,
		TotalTokens:      resp.Result.Usage.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	return &wire.ChatCompletionResponse{
		ID:      "",
		Object:  "chat.completion",
		Created: clovaCreatedToUnix(resp.Result.Created),
		Model:   "", // not reported on this surface; the caller knows the requested model
		Choices: []wire.ResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: mapClovaFinishReason(resp.Result.FinishReason),
		}},
		Usage: usage,
	}, nil
}

// clovaCreatedToUnix normalises the response timestamp: the reference
// documents milliseconds while the worked examples show seconds, so a
// value in the millisecond range (> 1e12) is divided down and the
// documented-seconds range passes through.
func clovaCreatedToUnix(created int64) int64 {
	if created > 1_000_000_000_000 {
		return created / 1000
	}
	return created
}

// clovaArgsAsString converts the CLOVA object-form tool-call arguments
// into OpenAI's string-encoded form (compact JSON).
func clovaArgsAsString(args json.RawMessage) (string, error) {
	trimmed := strings.TrimSpace(string(args))
	if trimmed == "" {
		return "{}", nil
	}
	if !json.Valid([]byte(trimmed)) {
		return "", unrep("tool-call arguments are not valid JSON")
	}
	return trimmed, nil
}

// mapClovaFinishReason maps a CLOVA finishReason (length | stop |
// tool_calls) onto the OpenAI vocabulary. The values coincide; an
// absent value maps to "stop" (the natural end of the turn).
func mapClovaFinishReason(finishReason string) string {
	if finishReason == "" {
		return "stop"
	}
	return finishReason
}

// ---------------------------------------------------------------------------
// Streaming: CLOVA Studio v3 SSE -> OpenAI chat-completion chunks.
//
// Documented event types: "token" (one content fragment, finishReason
// null until the end), "result" (the final event: full message,
// finishReason, usage), "error" ({status:{code,message}}) and
// "signal". The result event's message.content is the authoritative
// full text (the reference's own example shows it as the
// concatenation of the token fragments), so it is used for the
// reassembled result instead of a blind accumulation — never emitted
// again as a delta.
// ---------------------------------------------------------------------------

// TranslateClovaStream reads a CLOVA Studio v3 SSE stream and emits
// OpenAI chat.completion.chunk values via emit, in order. It returns
// the reassembled result when the stream ends. model is the requested
// model name (this surface does not echo a model field in stream
// events).
func TranslateClovaStream(r io.Reader, model string, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	if emit == nil {
		emit = func(wire.ChatChunk) error { return nil }
	}
	var res wire.StreamResult
	res.Model = model
	var content strings.Builder
	var resultContent string
	var finish string
	sawEvent := false
	created := time.Now().Unix()
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
		switch ev.name {
		case "token", "result", "":
			// The event name is the discriminator; "token" and
			// "result" share one payload shape (fields absent on
			// token events decode as zero values). Nameless
			// events are tolerated defensively.
		case "error":
			var ce ClovaError
			if err := json.Unmarshal([]byte(data), &ce); err != nil {
				return fmt.Errorf("translate: malformed CLOVA SSE error event: %w", err)
			}
			return &ce
		case "signal":
			// Keep-alive / signal data; carries no conversation content.
			return nil
		default:
			// An unknown named event must not kill the stream.
			return nil
		}
		var parsed struct {
			Message      ClovaResultMessage `json:"message"`
			FinishReason *string            `json:"finishReason"`
			Created      int64              `json:"created"`
			Usage        *ClovaUsage        `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("translate: malformed CLOVA SSE data: %w", err)
		}
		sawEvent = true
		isResult := ev.name == "result"
		if parsed.Created > 0 {
			created = clovaCreatedToUnix(parsed.Created)
		}
		if parsed.Usage != nil {
			res.Usage.PromptTokens = parsed.Usage.PromptTokens
			res.Usage.CompletionTokens = parsed.Usage.CompletionTokens
			res.Usage.TotalTokens = parsed.Usage.TotalTokens
			if res.Usage.TotalTokens == 0 {
				res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
			}
		}
		if parsed.FinishReason != nil && *parsed.FinishReason != "" {
			finish = mapClovaFinishReason(*parsed.FinishReason)
		}
		if isResult {
			// The final event carries the full text and usage.
			resultContent = parsed.Message.Content
			return nil
		}
		if parsed.Message.Content != "" {
			content.WriteString(parsed.Message.Content)
			if err := emitChunk(wire.ChunkDelta{Role: "assistant", Content: parsed.Message.Content}, nil, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return wire.StreamResult{}, err
	}
	if !sawEvent {
		return wire.StreamResult{}, fmt.Errorf("translate: CLOVA SSE stream ended with no events (empty)")
	}
	if resultContent != "" {
		res.Content = resultContent
	} else {
		res.Content = content.String()
	}
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
