// Gemini native-wire conversion (Google AI Studio generateContent).
//
//	OpenAI ChatCompletionRequest --ConvertGeminiRequest--> GeminiRequest
//	GeminiResponse               --ConvertGeminiResponse--> ChatCompletionResponse
//	Gemini SSE stream            --TranslateGeminiStream--> ChatChunk stream
//	Gemini error JSON            --MapGeminiError/MapGeminiStreamError--> GeminiError
//
// The Gemini API surface used here is POST
// {base}/v1beta/models/{model}:generateContent (and
// :streamGenerateContent?alt=sse for streaming), with the API key in
// the x-goog-api-key header (resolved per (provider, endpoint) by
// catalog.ResolveAuth). This file is a pure conversion library: no I/O,
// no credentials, no HTTP. The actual call stays in
// internal/provider (geminiClient).
//
// Mapping notes, all deliberate and loud on failure
// (*UnrepresentableError, never silent wrong output):
//   - system/developer messages become systemInstruction (Gemini has no
//     developer role); assistant messages become role "model"; tool
//     results become role "function" with a functionResponse part.
//   - Gemini requires contents to alternate user/model roles (with
//     "function" contents in the tool flow), so consecutive same-role
//     contents are merged, exactly like the Anthropic conversion.
//   - The first content must be role "user" — Gemini rejects a
//     conversation that opens with a model turn.
//   - A tool result names its function by the preceding assistant
//     message's tool call (matched on tool_call_id); when no earlier
//     call matches, the tool_call_id itself is used as the name.
//   - Images: data: URLs become inlineData parts; http(s) URLs have no
//     Gemini equivalent (fileData needs a File API URI) and are
//     unrepresentable.
//   - temperature is 0–2 on Gemini (OpenAI allows up to 2 as well, so
//     the range check is the same bound); stopSequences are capped at
//     5 by the API.
//   - finishReason SAFETY / RECITATION (content filtered) maps to
//     OpenAI "content_filter"; TOOL_CALLS maps to "tool_calls".
//   - A response with no candidates but a promptFeedback blockReason
//     is a filtered prompt: it converts to an empty reply with
//     finish_reason "content_filter", not an error.
package wire

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// ---------------------------------------------------------------------------
// Gemini side (generateContent / streamGenerateContent).
// ---------------------------------------------------------------------------

// GeminiPart is one part of a Gemini content block.
type GeminiPart struct {
	Text string `json:"text,omitempty"`
	// Thought marks a part as model thought (internal reasoning):
	// its text is never visible content, and it is captured into
	// the response's reasoning state instead.
	Thought bool `json:"thought,omitempty"`
	// InlineData carries a base64 image (data: URL on the OpenAI side).
	InlineData *GeminiInlineData `json:"inlineData,omitempty"`
	// FunctionCall is a model request to call a function.
	FunctionCall *GeminiFunctionCall `json:"functionCall,omitempty"`
	// FunctionResponse is a tool result.
	FunctionResponse *GeminiFunctionResponse `json:"functionResponse,omitempty"`
	// ThoughtSignature is internal reasoning metadata Gemini attaches
	// to parts; it is carried through on decode and captured into
	// the reasoning state (never emitted as content).
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

// GeminiInlineData is a base64-encoded blob part.
type GeminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// GeminiFunctionCall is a model's request to call a function.
type GeminiFunctionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// GeminiFunctionResponse is a tool result returned to the model.
type GeminiFunctionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response,omitempty"`
}

// GeminiContent is one turn in a Gemini request/response. Role is
// "user", "model", or "function" (tool results). systemInstruction
// uses the same shape without a role.
type GeminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []GeminiPart `json:"parts"`
}

// GeminiFunctionDeclaration is one function a Gemini request offers.
type GeminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// GeminiTool is one tools entry in a Gemini request.
type GeminiTool struct {
	FunctionDeclarations []GeminiFunctionDeclaration `json:"functionDeclarations"`
}

// GeminiFunctionCallingConfig is the toolConfig of a Gemini request.
type GeminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"` // "AUTO" | "ANY" | "NONE"
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// GeminiToolConfig is the toolConfig of a Gemini request.
type GeminiToolConfig struct {
	FunctionCallingConfig GeminiFunctionCallingConfig `json:"functionCallingConfig"`
}

// GeminiGenerationConfig is the generationConfig of a Gemini request.
type GeminiGenerationConfig struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
	// ThinkingConfig carries the numeric thinking budget
	// (CHECKLIST 285-286): the operator-declared budget for the
	// attempt's effort in config `reasoning.effort_budgets`.
	// Nil means no thinkingConfig is sent — the provider
	// default applies. Set only by the Gemini client from
	// ChatRequest.ThinkingBudget, never by the wire converter
	// (the converter must not invent a budget for an effort).
	ThinkingConfig *GeminiThinkingConfig `json:"thinkingConfig,omitempty"`
}

// GeminiThinkingConfig is the thinkingConfig block of a Gemini
// generationConfig: {"thinkingBudget": N}.
type GeminiThinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget,omitempty"`
}

// GeminiRequest is the body of POST .../models/{model}:generateContent.
type GeminiRequest struct {
	Contents          []GeminiContent         `json:"contents"`
	SystemInstruction *GeminiContent          `json:"systemInstruction,omitempty"`
	Tools             []GeminiTool            `json:"tools,omitempty"`
	ToolConfig        *GeminiToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *GeminiGenerationConfig `json:"generationConfig,omitempty"`
}

// GeminiCandidate is one candidate of a Gemini response.
type GeminiCandidate struct {
	Content      *GeminiContent `json:"content,omitempty"`
	FinishReason string         `json:"finishReason,omitempty"`
	Index        int            `json:"index,omitempty"`
}

// GeminiPromptFeedback is the promptFeedback block of a Gemini response.
type GeminiPromptFeedback struct {
	BlockReason string `json:"blockReason,omitempty"`
}

// GeminiUsageMetadata is the usageMetadata block of a Gemini response.
// CachedContentTokenCount is Gemini's provider-reported count of
// prompt tokens served from its cached content.
// ThoughtsTokenCount is Gemini's provider-reported count of
// completion tokens spent on internal reasoning — a subset of
// CandidatesTokenCount.
type GeminiUsageMetadata struct {
	PromptTokenCount        int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount    int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount         int `json:"totalTokenCount,omitempty"`
	CachedContentTokenCount int `json:"cachedContentTokenCount,omitempty"`
	ThoughtsTokenCount      int `json:"thoughtsTokenCount,omitempty"`
}

// GeminiResponse is the body of a generateContent reply (streaming or not).
type GeminiResponse struct {
	Candidates     []GeminiCandidate     `json:"candidates,omitempty"`
	PromptFeedback *GeminiPromptFeedback `json:"promptFeedback,omitempty"`
	UsageMetadata  *GeminiUsageMetadata  `json:"usageMetadata,omitempty"`
	ModelVersion   string                `json:"modelVersion,omitempty"`
}

// ---------------------------------------------------------------------------
// Errors.
// ---------------------------------------------------------------------------

// GeminiError is an error returned by the Gemini API, preserving the
// HTTP status and the Gemini error status string (e.g.
// "RESOURCE_EXHAUSTED", "UNAVAILABLE"). Retryable is true for the
// transient classes the router must fail over on: 429 /
// RESOURCE_EXHAUSTED, 503 / UNAVAILABLE, and any other 5xx.
// StatusCode is 0 for errors surfaced mid-stream (the HTTP status was
// already 200 when the stream's error event arrived).
type GeminiError struct {
	StatusCode int
	Status     string // e.g. "RESOURCE_EXHAUSTED", "INVALID_ARGUMENT"
	Message    string
	RetryAfter time.Duration
	// RetryAfterPresent: see AnthropicError — true when a
	// Retry-After header was present and parsed, including "0".
	RetryAfterPresent bool
	Retryable         bool
}

func (e *GeminiError) Error() string {
	if e == nil {
		return "wire: gemini error"
	}
	if e.StatusCode != 0 {
		if e.Message != "" {
			return fmt.Sprintf("gemini error: HTTP %d (%s): %s", e.StatusCode, e.Status, e.Message)
		}
		return fmt.Sprintf("gemini error: HTTP %d (%s)", e.StatusCode, e.Status)
	}
	if e.Message != "" {
		return fmt.Sprintf("gemini error (%s): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("gemini error (%s)", e.Status)
}

// IsRateLimit reports whether this is a 429 / RESOURCE_EXHAUSTED.
func (e *GeminiError) IsRateLimit() bool {
	return e != nil && (e.StatusCode == http.StatusTooManyRequests || e.Status == "RESOURCE_EXHAUSTED")
}

// IsGeminiRetryable reports whether err is a *GeminiError marked retryable.
func IsGeminiRetryable(err error) bool {
	var ge *GeminiError
	return errors.As(err, &ge) && ge.Retryable
}

// geminiErrorEnvelope is the {"error": {...}} envelope.
type geminiErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// parseGeminiErrorBody extracts code/message/status from a Gemini error
// body, tolerating the {"error": "plain string"} shape and non-JSON
// bodies.
func parseGeminiErrorBody(body []byte) (code int, status, message string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return 0, "", ""
	}
	var env geminiErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && (env.Error.Message != "" || env.Error.Status != "" || env.Error.Code != 0) {
		return env.Error.Code, env.Error.Status, env.Error.Message
	}
	var alt struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &alt); err == nil && alt.Error != "" {
		return 0, "", alt.Error
	}
	if len(trimmed) > 500 {
		trimmed = trimmed[:500]
	}
	return 0, "", trimmed
}

// MapGeminiError converts a non-2xx Gemini HTTP response (status,
// headers, body) into a *GeminiError that preserves the status code
// and the Gemini error status. 429 stays 429-class and 5xx stay 5xx
// so the router's failover triggers correctly. 402/404 are retryable
// per the shared taxonomy (RetryableStatus): they are properties of
// that target (its billing, its model list) — the provider layer
// still maps a 402 to QuotaError by status, so the retryable flag
// here only matters for the wire-level error itself.
func MapGeminiError(status int, headers http.Header, body []byte) error {
	_, errStatus, message := parseGeminiErrorBody(body)
	retryable := false
	switch {
	case status == http.StatusTooManyRequests || errStatus == "RESOURCE_EXHAUSTED":
		retryable = true
	case errStatus == "UNAVAILABLE" || errStatus == "DEADLINE_EXCEEDED":
		retryable = true
	case status == http.StatusBadRequest && IsModelNotFoundSignal(errStatus+" "+message):
		// Entry 151: a 400 that means "model not found at this
		// target" falls back; any other 400 stops the chain.
		retryable = true
	case RetryableStatus(status):
		retryable = true
	}
	ra, raPresent := parseRetryAfter(headers)
	if !raPresent {
		// No usable header: a Gemini 429 usually carries its
		// hint in the body instead — google.rpc.RetryInfo
		// (`"retryDelay": "30s"`). The general body parser
		// lives in internal/provider (ParseRetryAfterBody,
		// M4.4); this wire-local extraction covers only the
		// RetryInfo shape, because the wire package cannot
		// import the provider package.
		ra, raPresent = geminiRetryDelay(body)
	}
	return &GeminiError{
		StatusCode:        status,
		Status:            errStatus,
		Message:           message,
		RetryAfter:        ra,
		RetryAfterPresent: raPresent,
		Retryable:         retryable,
	}
}

// geminiRetryDelayRE matches a google.rpc.RetryInfo retryDelay value
// inside a Gemini error body: `"retryDelay": "30s"` (protobuf
// Duration JSON). See geminiRetryDelay.
var geminiRetryDelayRE = regexp.MustCompile(`"retryDelay"\s*:\s*"([0-9]+(?:\.[0-9]+)?s)"`)

// geminiRetryDelay extracts a Retry-After duration from a Gemini error
// body (google.rpc.RetryInfo), or (0, false) when the body carries
// none. The general body parser is internal/provider.ParseRetryAfterBody
// (M4.4); this wire-local extraction exists because the wire package
// cannot import the provider package, and Gemini is the one wire
// whose 429s carry their hint in the body rather than a header.
func geminiRetryDelay(body []byte) (time.Duration, bool) {
	m := geminiRetryDelayRE.FindSubmatch(body)
	if m == nil {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(string(m[1]), "s"), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// MapGeminiStreamError converts a Gemini SSE error payload
// ({"error": {...}}) into a *GeminiError. The HTTP status was already
// 200 when the stream started, so StatusCode is derived from the
// error status when it implies one (429 for RESOURCE_EXHAUSTED, 503
// for UNAVAILABLE) and 0 otherwise.
func MapGeminiStreamError(body []byte) error {
	_, errStatus, message := parseGeminiErrorBody(body)
	status := 0
	retryable := false
	switch errStatus {
	case "RESOURCE_EXHAUSTED":
		status, retryable = http.StatusTooManyRequests, true
	case "UNAVAILABLE", "DEADLINE_EXCEEDED":
		status, retryable = http.StatusServiceUnavailable, true
	case "INTERNAL", "UNKNOWN":
		retryable = true
	}
	// SSE error payloads carry no headers, so a RetryInfo hint in
	// the payload is the only hint there is (see geminiRetryDelay).
	ra, raPresent := geminiRetryDelay(body)
	return &GeminiError{
		StatusCode:        status,
		Status:            errStatus,
		Message:           message,
		RetryAfter:        ra,
		RetryAfterPresent: raPresent,
		Retryable:         retryable,
	}
}

// ---------------------------------------------------------------------------
// Request conversion: OpenAI -> Gemini.
// ---------------------------------------------------------------------------

// geminiImageMediaTypes are the image media types Gemini accepts for
// inlineData parts.
var geminiImageMediaTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/gif":  true,
	"image/webp": true,
	"image/heic": true,
	"image/heif": true,
}

// ConvertGeminiRequest converts an OpenAI chat-completions request into
// a Gemini generateContent request body. It returns
// *UnrepresentableError for anything that cannot be represented
// faithfully.
func ConvertGeminiRequest(req ChatCompletionRequest) (*GeminiRequest, error) {
	if strings.TrimSpace(req.Model) == "" {
		return nil, unrep("model is empty")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return nil, unrep("temperature %v is outside Gemini's 0-2 range", *req.Temperature)
	}
	if req.TopP != nil && (*req.TopP < 0 || *req.TopP > 1) {
		return nil, unrep("top_p %v is outside the 0-1 range", *req.TopP)
	}
	if len(req.Stop) > 5 {
		return nil, unrep("Gemini accepts at most 5 stop sequences, got %d", len(req.Stop))
	}
	// Reasoning continuity (CHECKLIST 163): validate every
	// assistant message's restored reasoning state up front —
	// Gemini translates reasoning text into thought parts and
	// drops lone signatures, but a state it cannot represent at
	// all fails the hop here (the router fails over) rather
	// than silently dropping reasoning.
	for i, m := range req.Messages {
		if m.Reasoning != nil && !m.Reasoning.Empty() {
			if _, _, err := reasoning.Check(m.Reasoning, reasoning.WireGemini, req.Model); err != nil {
				return nil, fmt.Errorf("messages[%d] reasoning: %w", i, err)
			}
		}
	}

	// Map tool-call IDs to function names from assistant messages, so
	// a later tool-result message (which carries only the call ID)
	// can name its functionResponse correctly.
	callNames := make(map[string]string)
	for _, m := range req.Messages {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && tc.Function.Name != "" {
				callNames[tc.ID] = tc.Function.Name
			}
		}
	}

	out := &GeminiRequest{}

	tools, toolConfig, err := convertGeminiTools(req.Tools, req.ToolChoice)
	if err != nil {
		return nil, err
	}
	out.Tools = tools
	out.ToolConfig = toolConfig

	if req.Temperature != nil || req.TopP != nil || req.MaxTokens > 0 || len(req.Stop) > 0 {
		gen := &GeminiGenerationConfig{
			Temperature:   req.Temperature,
			TopP:          req.TopP,
			StopSequences: append([]string(nil), req.Stop...),
		}
		if req.MaxTokens > 0 {
			m := req.MaxTokens
			gen.MaxOutputTokens = &m
		}
		out.GenerationConfig = gen
	}

	var system []string
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "developer":
			// Gemini has no developer role; OpenAI's developer
			// message is a system instruction, so it joins the
			// system prompt.
			text, err := geminiMessageText(m, i)
			if err != nil {
				return nil, err
			}
			if text != "" {
				system = append(system, text)
			}
		default:
			content, err := convertGeminiMessage(m, i, callNames)
			if err != nil {
				return nil, err
			}
			out.Contents = appendGeminiContent(out.Contents, content)
		}
	}
	if len(system) > 0 {
		out.SystemInstruction = &GeminiContent{Parts: []GeminiPart{{Text: strings.Join(system, "\n\n")}}}
	}
	if len(out.Contents) == 0 {
		return nil, unrep("no representable messages (all messages were empty)")
	}
	if out.Contents[0].Role != "user" {
		return nil, unrep("first message must be from the user, got role %q", out.Contents[0].Role)
	}
	return out, nil
}

// convertGeminiTools converts OpenAI tool definitions to Gemini
// functionDeclarations and the matching toolConfig for the requested
// tool_choice. tool_choice "none" drops the tools entirely.
func convertGeminiTools(tools []ToolDefinition, toolChoice any) ([]GeminiTool, *GeminiToolConfig, error) {
	if s, ok := toolChoice.(string); ok && s == "none" {
		return nil, &GeminiToolConfig{FunctionCallingConfig: GeminiFunctionCallingConfig{Mode: "NONE"}}, nil
	}
	if len(tools) == 0 {
		// No tools defined: only "none" and absent tool_choice are
		// meaningful; "auto"/"required"/a named function with no
		// tools is an error, not a silent no-op.
		if toolChoice == nil {
			return nil, nil, nil
		}
		if s, ok := toolChoice.(string); ok && s == "auto" {
			return nil, nil, nil
		}
		return nil, nil, unrep("tool_choice %v with no tools defined", toolChoice)
	}
	decls := make([]GeminiFunctionDeclaration, 0, len(tools))
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
		decls = append(decls, GeminiFunctionDeclaration{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  params,
		})
	}
	toolList := []GeminiTool{{FunctionDeclarations: decls}}
	cfg, err := convertGeminiToolChoice(toolChoice, decls)
	if err != nil {
		return nil, nil, err
	}
	return toolList, cfg, nil
}

// convertGeminiToolChoice maps an OpenAI tool_choice onto Gemini's
// functionCallingConfig.
func convertGeminiToolChoice(tc any, decls []GeminiFunctionDeclaration) (*GeminiToolConfig, error) {
	mode := "AUTO"
	var allowed []string
	if tc != nil {
		if s, ok := tc.(string); ok {
			switch s {
			case "auto":
				mode = "AUTO"
			case "required":
				mode = "ANY"
			case "none":
				mode = "NONE"
			default:
				return nil, unrep("unknown tool_choice %q", s)
			}
		} else if m, ok := tc.(map[string]any); ok {
			t, _ := m["type"].(string)
			switch t {
			case "auto":
				mode = "AUTO"
			case "any":
				mode = "ANY"
			case "tool":
				name, _ := m["name"].(string)
				if name == "" {
					return nil, unrep("tool_choice of type \"tool\" needs a name")
				}
				mode = "ANY"
				allowed = []string{name}
			case "function":
				fn, _ := m["function"].(map[string]any)
				name, _ := fn["name"].(string)
				if name == "" {
					return nil, unrep("tool_choice function has no name")
				}
				mode = "ANY"
				allowed = []string{name}
			default:
				return nil, unrep("unknown tool_choice type %q", t)
			}
		} else {
			return nil, unrep("tool_choice has unsupported shape %T", tc)
		}
	}
	// A named function must actually be declared.
	for _, name := range allowed {
		found := false
		for _, d := range decls {
			if d.Name == name {
				found = true
				break
			}
		}
		if !found {
			return nil, unrep("tool_choice names tool %q but it is not defined", name)
		}
	}
	if mode == "AUTO" && allowed == nil {
		// Default: no toolConfig needed; Gemini defaults to AUTO.
		return nil, nil
	}
	return &GeminiToolConfig{FunctionCallingConfig: GeminiFunctionCallingConfig{
		Mode:                 mode,
		AllowedFunctionNames: allowed,
	}}, nil
}

// appendGeminiContent appends a content block, merging into the
// previous content when it has the same role. Gemini requires
// contents to alternate user/model roles (tool flows routinely
// produce consecutive same-role contents), so merging is required
// for correctness.
func appendGeminiContent(contents []GeminiContent, c GeminiContent) []GeminiContent {
	if len(c.Parts) == 0 {
		return contents
	}
	if n := len(contents); n > 0 && contents[n-1].Role == c.Role {
		contents[n-1].Parts = append(contents[n-1].Parts, c.Parts...)
		return contents
	}
	return append(contents, c)
}

// convertGeminiMessage converts one non-system OpenAI message to a
// Gemini content block.
func convertGeminiMessage(m ChatMessage, index int, callNames map[string]string) (GeminiContent, error) {
	switch m.Role {
	case "user":
		parts, err := convertGeminiParts(m.Content, index)
		if err != nil {
			return GeminiContent{}, err
		}
		return GeminiContent{Role: "user", Parts: parts}, nil
	case "assistant":
		parts, err := convertGeminiParts(m.Content, index)
		if err != nil {
			return GeminiContent{}, err
		}
		for i, tc := range m.ToolCalls {
			if tc.Type != "" && tc.Type != "function" {
				return GeminiContent{}, unrep("messages[%d].tool_calls[%d].type %q is not \"function\"", index, i, tc.Type)
			}
			if strings.TrimSpace(tc.Function.Name) == "" {
				return GeminiContent{}, unrep("messages[%d].tool_calls[%d].function.name is empty", index, i)
			}
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return GeminiContent{}, unrep("messages[%d].tool_calls[%d].function.arguments is not valid JSON", index, i)
			}
			parts = append(parts, GeminiPart{FunctionCall: &GeminiFunctionCall{
				Name: tc.Function.Name,
				Args: json.RawMessage(args),
			}})
		}
		// Restored reasoning: thought parts lead the model
		// content; thought signatures attach to the functionCall
		// part they sign (or the first part, best-effort).
		// reasoning.Check (in ConvertGeminiRequest) already
		// proved this state representable here.
		if m.Reasoning != nil && !m.Reasoning.Empty() {
			parts = geminiReasoningParts(m.Reasoning, parts)
		}
		return GeminiContent{Role: "model", Parts: parts}, nil
	case "tool":
		if strings.TrimSpace(m.ToolCallID) == "" {
			return GeminiContent{}, unrep("messages[%d] is a tool result with no tool_call_id", index)
		}
		name := callNames[m.ToolCallID]
		if name == "" {
			// No earlier assistant call matches: use the call ID
			// as the name rather than failing the whole request.
			name = m.ToolCallID
		}
		text, err := geminiMessageText(m, index)
		if err != nil {
			return GeminiContent{}, err
		}
		resp := geminiFunctionResponseValue(text)
		return GeminiContent{Role: "function", Parts: []GeminiPart{{
			FunctionResponse: &GeminiFunctionResponse{Name: name, Response: resp},
		}}}, nil
	default:
		return GeminiContent{}, unrep("messages[%d].role %q is not supported", index, m.Role)
	}
}

// geminiReasoningParts renders a captured reasoning state as Gemini
// parts and merges them into the model content's parts. Reasoning
// text becomes thought parts (thought:true) that lead the content;
// a thought_signature attaches to the functionCall part whose name
// it carries, or — when the call is not in this message — to the
// first part, best-effort (a signature whose call is absent from
// this turn cannot be attached anywhere better; the function call
// itself is preserved in the conversation). Bedrock redacted
// content and lone signatures have no Gemini equivalent and were
// already recorded as drops by reasoning.Check.
func geminiReasoningParts(st *reasoning.State, parts []GeminiPart) []GeminiPart {
	if st == nil {
		return parts
	}
	var thought []GeminiPart
	var sigs []reasoning.Block
	for _, b := range st.Blocks {
		switch b.Kind {
		case reasoning.KindThinking, reasoning.KindThought:
			p := GeminiPart{Text: b.Text, Thought: true}
			// Only a Gemini-issued signature may ride on a
			// thought part: an Anthropic or Bedrock signature
			// is meaningless to Gemini and would fail its
			// validation, so it is dropped (recorded by Check).
			if b.Signature != "" && st.SourceWire == reasoning.WireGemini {
				p.ThoughtSignature = b.Signature
			}
			thought = append(thought, p)
		case reasoning.KindBedrock:
			if text, _, _ := reasoning.ParseBedrockReasoning(b.Data); strings.TrimSpace(text) != "" {
				thought = append(thought, GeminiPart{Text: text, Thought: true})
			}
		case reasoning.KindThoughtSignature:
			sigs = append(sigs, b)
		}
	}
	// Attach signatures to the part they sign.
	for _, sig := range sigs {
		attached := false
		if sig.Name != "" {
			for i := range parts {
				if parts[i].FunctionCall != nil && parts[i].FunctionCall.Name == sig.Name {
					parts[i].ThoughtSignature = sig.Signature
					attached = true
					break
				}
			}
		}
		if !attached {
			// No matching call in this message: attach to the
			// first part (a thought part if one was just built,
			// else the first content part). Best-effort — the
			// signature is preserved rather than dropped.
			if len(thought) > 0 {
				thought[0].ThoughtSignature = sig.Signature
			} else if len(parts) > 0 {
				parts[0].ThoughtSignature = sig.Signature
			}
		}
	}
	return append(thought, parts...)
}

// geminiFunctionResponseValue wraps a tool result's text as the
// functionResponse response object: a JSON object passes through
// unchanged, anything else becomes {"result": <text>}.
func geminiFunctionResponseValue(text string) json.RawMessage {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err == nil {
			return json.RawMessage(trimmed)
		}
	}
	b, err := json.Marshal(map[string]string{"result": text})
	if err != nil {
		return json.RawMessage(`{"result":""}`)
	}
	return b
}

// geminiMessageText extracts the plain-text content of a message.
// Multi-part content must be text-only; image parts in a
// system/developer/tool message are unrepresentable.
func geminiMessageText(m ChatMessage, index int) (string, error) {
	parts, err := convertGeminiParts(m.Content, index)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			return "", unrep("messages[%d]: non-text content cannot appear in a %s message", index, m.Role)
		}
		sb.WriteString(p.Text)
	}
	return sb.String(), nil
}

// convertGeminiParts converts OpenAI message content (a string, an
// array of parts, or null) into Gemini content parts.
func convertGeminiParts(raw json.RawMessage, index int) ([]GeminiPart, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString == "" {
			return nil, nil
		}
		return []GeminiPart{{Text: asString}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, unrep("messages[%d].content is neither a string nor a content-part array", index)
	}
	var out []GeminiPart
	for i, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				out = append(out, GeminiPart{Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || strings.TrimSpace(p.ImageURL.URL) == "" {
				return nil, unrep("messages[%d].content[%d] is an image_url part with no URL", index, i)
			}
			part, err := convertGeminiImagePart(p.ImageURL.URL, index, i)
			if err != nil {
				return nil, err
			}
			out = append(out, part)
		default:
			return nil, unrep("messages[%d].content[%d].type %q is not supported (only \"text\" and \"image_url\")", index, i, p.Type)
		}
	}
	return out, nil
}

// convertGeminiImagePart converts one OpenAI image_url value to a
// Gemini part. Data URLs become inlineData (media type must be one
// Gemini accepts); http(s) URLs have no Gemini equivalent — fileData
// needs a File API URI, not an arbitrary URL — so they are
// unrepresentable.
func convertGeminiImagePart(url string, msgIndex, partIndex int) (GeminiPart, error) {
	if !strings.HasPrefix(url, "data:") {
		return GeminiPart{}, unrep("messages[%d].content[%d] image URL is not a data: URL (Gemini inlineData needs base64 image data; arbitrary http(s) URLs have no Gemini equivalent)", msgIndex, partIndex)
	}
	rest := strings.TrimPrefix(url, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return GeminiPart{}, unrep("messages[%d].content[%d] data URL is not a base64 image (want data:<media-type>;base64,...)", msgIndex, partIndex)
	}
	mediaType := rest[:semi]
	data := rest[semi+len(";base64,"):]
	if !geminiImageMediaTypes[mediaType] {
		return GeminiPart{}, unrep("messages[%d].content[%d] image media type %q is not supported by Gemini (jpeg, png, gif, webp, heic, heif)", msgIndex, partIndex, mediaType)
	}
	if strings.TrimSpace(data) == "" {
		return GeminiPart{}, unrep("messages[%d].content[%d] data URL has empty image data", msgIndex, partIndex)
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return GeminiPart{}, unrep("messages[%d].content[%d] image data is not valid base64", msgIndex, partIndex)
	}
	return GeminiPart{InlineData: &GeminiInlineData{MimeType: mediaType, Data: data}}, nil
}

// ---------------------------------------------------------------------------
// Response conversion: Gemini -> OpenAI.
// ---------------------------------------------------------------------------

// MapGeminiFinishReason maps a Gemini finishReason onto an OpenAI
// finish_reason. STOP and OTHER end the turn normally ("stop");
// MAX_TOKENS ran out of room ("length"); SAFETY and RECITATION mean
// the content was filtered ("content_filter"); TOOL_CALLS means the
// model wants tools run ("tool_calls"). An empty or unknown reason
// maps to "stop" — the turn ended, and inventing a more specific
// reason would be guessing.
func MapGeminiFinishReason(finishReason string) string {
	switch finishReason {
	case "STOP", "OTHER", "":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	case "TOOL_CALLS":
		return "tool_calls"
	case "MALFORMED_FUNCTION_CALL":
		return "stop"
	default:
		return "stop"
	}
}

// ConvertGeminiResponse converts a non-streaming Gemini generateContent
// response into an OpenAI chat-completion response. A response with no
// candidates but a promptFeedback blockReason is a filtered prompt:
// it converts to an empty reply with finish_reason
// "content_filter", not an error. Parts Gemini cannot express on the
// OpenAI side (e.g. inlineData image output) return
// *UnrepresentableError.
func ConvertGeminiResponse(resp *GeminiResponse) (*ChatCompletionResponse, error) {
	if resp == nil {
		return nil, unrep("gemini response is nil")
	}
	if len(resp.Candidates) == 0 {
		if resp.PromptFeedback != nil && resp.PromptFeedback.BlockReason != "" {
			return &ChatCompletionResponse{
				ID:      "",
				Object:  "chat.completion",
				Created: time.Now().Unix(),
				Model:   resp.ModelVersion,
				Choices: []ResponseChoice{{
					Index:        0,
					Message:      ChatMessage{Role: "assistant", Content: mustJSON("")},
					FinishReason: "content_filter",
				}},
			}, nil
		}
		return nil, unrep("gemini response has no candidates")
	}
	cand := resp.Candidates[0]
	var (
		text      strings.Builder
		toolCalls []ToolCall
		state     *reasoning.State
	)
	capture := func(b reasoning.Block) {
		if state == nil {
			state = &reasoning.State{SourceWire: reasoning.WireGemini, SourceModel: resp.ModelVersion}
		}
		state.Add(b)
	}
	if cand.Content != nil {
		for i, p := range cand.Content.Parts {
			switch {
			case p.Thought:
				// Thought text is internal reasoning, never
				// visible content — captured, not written to
				// the message. A thought part with no text
				// but a signature carries the signature only.
				if strings.TrimSpace(p.Text) != "" {
					capture(reasoning.Block{Kind: reasoning.KindThought, Text: p.Text, Signature: p.ThoughtSignature})
				} else if p.ThoughtSignature != "" {
					capture(reasoning.Block{Kind: reasoning.KindThoughtSignature, Signature: p.ThoughtSignature})
				}
			case p.Text != "":
				text.WriteString(p.Text)
				if p.ThoughtSignature != "" {
					capture(reasoning.Block{Kind: reasoning.KindThoughtSignature, Signature: p.ThoughtSignature})
				}
			case p.FunctionCall != nil:
				if strings.TrimSpace(p.FunctionCall.Name) == "" {
					return nil, unrep("response candidate parts[%d] is a functionCall with no name", i)
				}
				args := strings.TrimSpace(string(p.FunctionCall.Args))
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					return nil, unrep("response candidate parts[%d] functionCall args are not valid JSON", i)
				}
				toolCalls = append(toolCalls, ToolCall{
					ID:   fmt.Sprintf("call_%d", len(toolCalls)),
					Type: "function",
					Function: ToolCallFunction{
						Name:      p.FunctionCall.Name,
						Arguments: args,
					},
				})
				if p.ThoughtSignature != "" {
					capture(reasoning.Block{Kind: reasoning.KindThoughtSignature, Name: p.FunctionCall.Name, Signature: p.ThoughtSignature})
				}
			case p.InlineData != nil:
				return nil, unrep("response candidate parts[%d] is image output (inlineData) with no OpenAI equivalent", i)
			case p.FunctionResponse != nil:
				return nil, unrep("response candidate parts[%d] is a functionResponse with no OpenAI equivalent", i)
			default:
				// A part carrying only a thoughtSignature
				// (internal reasoning metadata) is captured;
				// anything else unrecognised is an error.
				if p.ThoughtSignature != "" {
					capture(reasoning.Block{Kind: reasoning.KindThoughtSignature, Signature: p.ThoughtSignature})
					continue
				}
				return nil, unrep("response candidate parts[%d] has an unrecognised shape", i)
			}
		}
	}
	msg := ChatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = mustJSON(text.String())
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	var usage Usage
	if resp.UsageMetadata != nil {
		usage = Usage{
			PromptTokens:     resp.UsageMetadata.PromptTokenCount,
			CompletionTokens: resp.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      resp.UsageMetadata.TotalTokenCount,
			CachedTokens:     resp.UsageMetadata.CachedContentTokenCount,
			ReasoningTokens:  resp.UsageMetadata.ThoughtsTokenCount,
		}
		if usage.TotalTokens == 0 {
			usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
		}
	}
	return &ChatCompletionResponse{
		ID:      "",
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   resp.ModelVersion,
		Choices: []ResponseChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: MapGeminiFinishReason(cand.FinishReason),
		}},
		Usage:     usage,
		Reasoning: state,
	}, nil
}

// ---------------------------------------------------------------------------
// Streaming: Gemini SSE -> OpenAI chat-completion chunks.
// ---------------------------------------------------------------------------

// TranslateGeminiStream reads a Gemini streamGenerateContent SSE stream
// (alt=sse) and emits OpenAI chat.completion.chunk values via emit, in
// order. It returns the reassembled result when the stream ends.
//
//   - Each SSE data payload is one GenerateContentResponse; text parts
//     become content deltas, functionCall parts become tool-call
//     deltas (arguments accumulated across events, validated as JSON
//     when the stream closes).
//   - Tool-call indices are renumbered sequentially from 0 in order of
//     first appearance.
//   - usageMetadata (usually in the last event) supplies the usage; the
//     final emitted chunk carries finish_reason and usage.
//   - A promptFeedback blockReason mid-stream ends the turn as
//     content_filter.
//   - An error payload returns the mapped *GeminiError and never a
//     success result; a stream with no events at all returns an error
//     (empty), never a partial success. Gemini sends no [DONE]
//     sentinel — the stream simply ends — but a literal "[DONE]" line
//     is tolerated defensively.
func TranslateGeminiStream(r io.Reader, emit func(ChatChunk) error) (StreamResult, error) {
	if emit == nil {
		emit = func(ChatChunk) error { return nil }
	}
	var res StreamResult
	created := time.Now().Unix()
	// toolIndex maps a function-call name occurrence order to the
	// sequential OpenAI tool-call index; args accumulate per index.
	toolIndex := make(map[string]int)
	toolArgs := make(map[int]*strings.Builder)
	toolIDs := make(map[int]string)
	toolNames := make(map[int]string)
	nextTool := 0
	var (
		content  strings.Builder
		sawEvent bool
		finish   string
		blocked  bool
	)
	// Reasoning capture (CHECKLIST 163): thought parts accumulate
	// their text across events (thoughtBuf) and keep the latest
	// signature (thoughtSig); functionCall signatures accumulate
	// per call name (callSigs); signature-only parts accumulate in
	// order (bareSigs). Assembled into StreamResult.Reasoning at
	// the end — never emitted as content.
	var (
		thoughtBuf strings.Builder
		thoughtSig string
		callSigs   = make(map[string]string)
		callOrder  []string
		bareSigs   []string
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

	err := readSSE(r, func(ev sseEvent) error {
		data := strings.TrimSpace(ev.data)
		if data == "" || data == "[DONE]" {
			return nil
		}
		// An error payload is {"error": {...}}.
		var errProbe struct {
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &errProbe); err == nil && len(errProbe.Error) > 0 {
			return MapGeminiStreamError([]byte(data))
		}
		var parsed GeminiResponse
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			return fmt.Errorf("wire: malformed Gemini SSE data: %w", err)
		}
		sawEvent = true
		if parsed.ModelVersion != "" {
			res.Model = parsed.ModelVersion
		}
		if parsed.UsageMetadata != nil {
			res.Usage.PromptTokens = parsed.UsageMetadata.PromptTokenCount
			res.Usage.CompletionTokens = parsed.UsageMetadata.CandidatesTokenCount
			res.Usage.TotalTokens = parsed.UsageMetadata.TotalTokenCount
			res.Usage.CachedTokens = parsed.UsageMetadata.CachedContentTokenCount
			res.Usage.ReasoningTokens = parsed.UsageMetadata.ThoughtsTokenCount
			if res.Usage.TotalTokens == 0 {
				res.Usage.TotalTokens = res.Usage.PromptTokens + res.Usage.CompletionTokens
			}
		}
		if parsed.PromptFeedback != nil && parsed.PromptFeedback.BlockReason != "" {
			blocked = true
			finish = "content_filter"
			return nil
		}
		if len(parsed.Candidates) == 0 {
			return nil
		}
		cand := parsed.Candidates[0]
		if cand.FinishReason != "" {
			finish = MapGeminiFinishReason(cand.FinishReason)
		}
		if cand.Content == nil {
			return nil
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.Thought:
				// Thought text is internal reasoning, never
				// visible content — accumulated, not emitted.
				if p.Text != "" {
					thoughtBuf.WriteString(p.Text)
				}
				if p.ThoughtSignature != "" {
					thoughtSig = p.ThoughtSignature
				}
			case p.Text != "":
				content.WriteString(p.Text)
				if err := emitChunk(ChunkDelta{Role: "assistant", Content: p.Text}, nil, nil); err != nil {
					return err
				}
				if p.ThoughtSignature != "" {
					bareSigs = append(bareSigs, p.ThoughtSignature)
				}
			case p.FunctionCall != nil:
				name := p.FunctionCall.Name
				idx, ok := toolIndex[name]
				if !ok {
					idx = nextTool
					nextTool++
					toolIndex[name] = idx
					toolArgs[idx] = &strings.Builder{}
					toolIDs[idx] = fmt.Sprintf("call_%d", idx)
					toolNames[idx] = name
					if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
						Index: idx,
						ID:    toolIDs[idx],
						Type:  "function",
						Function: ChunkFunction{
							Name: name,
						},
					}}}, nil, nil); err != nil {
						return err
					}
				}
				if p.ThoughtSignature != "" {
					if _, seen := callSigs[name]; !seen {
						callOrder = append(callOrder, name)
					}
					callSigs[name] = p.ThoughtSignature
				}
				args := strings.TrimSpace(string(p.FunctionCall.Args))
				if args != "" {
					toolArgs[idx].WriteString(args)
					if err := emitChunk(ChunkDelta{ToolCalls: []ChunkToolCall{{
						Index:    idx,
						Function: ChunkFunction{Arguments: args},
					}}}, nil, nil); err != nil {
						return err
					}
				}
			case p.InlineData != nil:
				return unrep("streamed part is image output (inlineData) with no OpenAI equivalent")
			case p.FunctionResponse != nil:
				return unrep("streamed part is a functionResponse with no OpenAI equivalent")
			default:
				// Thought-signature-only parts are captured.
				if p.ThoughtSignature != "" {
					bareSigs = append(bareSigs, p.ThoughtSignature)
				}
			}
		}
		return nil
	})
	if err != nil {
		return StreamResult{}, err
	}
	if !sawEvent {
		return StreamResult{}, errors.New("wire: Gemini SSE stream ended with no events (empty)")
	}
	if finish == "" {
		finish = "stop"
	}
	if blocked {
		finish = "content_filter"
	}
	for idx := 0; idx < nextTool; idx++ {
		args := strings.TrimSpace(toolArgs[idx].String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return StreamResult{}, fmt.Errorf("wire: streamed functionCall arguments for %q are not valid JSON", toolNames[idx])
		}
		res.ToolCalls = append(res.ToolCalls, ToolCall{
			ID:   toolIDs[idx],
			Type: "function",
			Function: ToolCallFunction{
				Name:      toolNames[idx],
				Arguments: args,
			},
		})
	}
	res.Content = content.String()
	res.FinishReason = finish
	if thoughtBuf.Len() > 0 || thoughtSig != "" || len(callOrder) > 0 || len(bareSigs) > 0 {
		state := &reasoning.State{SourceWire: reasoning.WireGemini, SourceModel: res.Model}
		if thoughtBuf.Len() > 0 {
			state.Add(reasoning.Block{Kind: reasoning.KindThought, Text: thoughtBuf.String(), Signature: thoughtSig})
		} else if thoughtSig != "" {
			bareSigs = append([]string{thoughtSig}, bareSigs...)
		}
		for _, name := range callOrder {
			state.Add(reasoning.Block{Kind: reasoning.KindThoughtSignature, Name: name, Signature: callSigs[name]})
		}
		for _, sig := range bareSigs {
			state.Add(reasoning.Block{Kind: reasoning.KindThoughtSignature, Signature: sig})
		}
		res.Reasoning = state
	}
	fr := res.FinishReason
	usage := res.Usage
	if err := emitChunk(ChunkDelta{}, &fr, &usage); err != nil {
		return StreamResult{}, err
	}
	return res, nil
}
