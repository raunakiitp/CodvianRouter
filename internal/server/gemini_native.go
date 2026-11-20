// M8.13: Gemini-native surface (POST /v1beta/models/...,
// GET /v1beta/models).
//
// The gateway speaks Gemini-native request/response shapes on this
// surface and routes through the normal engine — the same engine
// /v1/chat/completions uses — so failover, ledger, cache and
// coalescing apply, and a Gemini-wire provider is served by the
// EXISTING Gemini wire client (internal/provider geminiClient via
// internal/wire conversion). No second Gemini client is built
// here: this file only translates Gemini-native JSON into the
// canonical provider.ChatRequest and the canonical
// provider.ChatResponse back into Gemini-native JSON.
//
// Routing note (Go ServeMux): a pattern segment may not mix a
// wildcard with a literal suffix — "POST
// /v1beta/models/{model}:generateContent" panics at registration
// (bad wildcard segment). The coordinator registers ONE route,
// "POST /v1beta/models/{rest...}", and handleGeminiModelAction
// dispatches on the ":generateContent" / ":streamGenerateContent"
// suffix. GET /v1beta/models is a separate route.
//
// Field-mapping rule: a Gemini-native field with no mapping in the
// canonical form is rejected with a 400 naming the field, never
// silently dropped. Unknown top-level (or nested) fields are
// rejected the same way by DisallowUnknownFields. The full table
// lives in the slice report; the code marks each rejection with
// the field name.
//
// Honesty rules (same as M8.10 / /v1/models): unknown prices stay
// unknown (the ListModels shape carries no price at all);
// supportedGenerationMethods lists only what this gateway
// demonstrably serves for a Gemini-wire model (generateContent +
// streamGenerateContent, both backed by geminiClient); a
// capability the catalog does not record (display name,
// description, input/output token limits, version) is omitted,
// never invented. Vertex-wire providers are NOT listed here:
// their models are addressed as publishers/google/models/...,
// not as models/<id>, so listing them under models/<id> would
// invent an address.
//
// Error shape: every error this file itself writes uses the
// Gemini envelope {"error": {"code", "message", "status"}} —
// writeGeminiClientError for pre-engine rejections (400
// validation, 404 unknown action, 405, 413, 503 engine not
// configured), writeGeminiError for engine failures, and
// writeGeminiUpstreamFailure for the response-conversion 502.
// ACCEPTED EXCEPTION (shared helpers this slice does not own):
// pre-call gate refusals (preBodyAdmission, intent tier, access
// schedule, key budget/token limits, quota pools/schedules,
// internal/team budgets, traffic, throttle, session, readiness)
// and guardrail blocks (applyGuardrails /
// applyResponseGuardrails -> writePolicyRefusal, HTTP 403 with
// the JSON-RPC code -32001) still answer in the gateway's shared
// OpenAI/JSON-RPC shape, because those helpers live in
// server.go and write directly to the ResponseWriter before this
// file can intervene; the auth/scope wrappers (requireAuth /
// requireScope, guard.go) likewise. Unifying those shared paths
// is coordinator work, not this slice's — the split is recorded
// here rather than left silent.
//
// Known odd path, no fix in this slice: POST /v1beta/models
// (bare, no model) 307-redirects to /v1beta/models/, which then
// hits rest="" and answers 404 unknown action "". The single-model
// GET (/v1beta/models/{model}) has no route at all — both are
// coordinator/PARITY items (guard.go / server.go registration).
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// geminiNativeMethods are the generation methods this gateway
// serves for a Gemini-wire model: the non-streaming generate and
// the SSE stream, both backed by the existing geminiClient
// (Chat / ChatStreamChunks). This is a claim about the gateway's
// own surface, not about the upstream catalog.
var geminiNativeMethods = []string{"generateContent", "streamGenerateContent"}

// ---------------------------------------------------------------------------
// Gemini-native request shapes (generateContent).
// ---------------------------------------------------------------------------

// geminiNativePart is one part of a Gemini content block. Fields
// with no canonical mapping are decoded so the translator can
// reject them by name (fileData, executableCode,
// codeExecutionResult); fields the wire never sees (videoMetadata
// etc.) are rejected by DisallowUnknownFields at decode time.
type geminiNativePart struct {
	Text string `json:"text,omitempty"`
	// Thought marks a model thought part (internal reasoning).
	Thought          bool   `json:"thought,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
	InlineData       *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FileData *struct {
		MimeType string `json:"mimeType,omitempty"`
		FileURI  string `json:"fileUri,omitempty"`
	} `json:"fileData,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args,omitempty"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response,omitempty"`
	} `json:"functionResponse,omitempty"`
	ExecutableCode *struct {
		Language string `json:"language,omitempty"`
		Code     string `json:"code,omitempty"`
	} `json:"executableCode,omitempty"`
	CodeExecutionResult *struct {
		Outcome string `json:"outcome,omitempty"`
	} `json:"codeExecutionResult,omitempty"`
}

// geminiNativeContent is one turn of a Gemini request. Role is
// "user", "model", "function", or "" (defaults to "user", the
// Gemini default); systemInstruction uses the same shape.
type geminiNativeContent struct {
	Role  string             `json:"role,omitempty"`
	Parts []geminiNativePart `json:"parts"`
}

// geminiNativeFunctionDeclaration is one function a Gemini request offers.
type geminiNativeFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	// ParametersJSONSchema is the newer spelling of the same
	// schema; used only when Parameters is absent.
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

// geminiNativeTool is one tools entry of a Gemini request.
type geminiNativeTool struct {
	FunctionDeclarations []geminiNativeFunctionDeclaration `json:"functionDeclarations"`
}

// geminiNativeToolConfig is the toolConfig of a Gemini request.
type geminiNativeToolConfig struct {
	FunctionCallingConfig *struct {
		Mode                 string   `json:"mode,omitempty"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig,omitempty"`
}

// geminiNativeGenerationConfig is the generationConfig of a Gemini
// request. Fields with no canonical mapping are decoded so the
// translator can reject them by name.
type geminiNativeGenerationConfig struct {
	Temperature      *float64        `json:"temperature,omitempty"`
	TopP             *float64        `json:"topP,omitempty"`
	TopK             *int            `json:"topK,omitempty"`
	MaxOutputTokens  *int            `json:"maxOutputTokens,omitempty"`
	StopSequences    []string        `json:"stopSequences,omitempty"`
	CandidateCount   *int            `json:"candidateCount,omitempty"`
	ResponseMimeType string          `json:"responseMimeType,omitempty"`
	ResponseSchema   json.RawMessage `json:"responseSchema,omitempty"`
	PresencePenalty  *float64        `json:"presencePenalty,omitempty"`
	FrequencyPenalty *float64        `json:"frequencyPenalty,omitempty"`
	Seed             *int            `json:"seed,omitempty"`
	ResponseLogprobs *bool           `json:"responseLogprobs,omitempty"`
	Logprobs         *int            `json:"logprobs,omitempty"`
	ThinkingConfig   *struct {
		ThinkingBudget  *int  `json:"thinkingBudget,omitempty"`
		IncludeThoughts *bool `json:"includeThoughts,omitempty"`
	} `json:"thinkingConfig,omitempty"`
}

// geminiNativeGenerateRequest is the body of POST
// .../models/{model}:generateContent (and :streamGenerateContent).
// safetySettings / cachedContent / labels have no canonical
// mapping and are rejected by name when present.
type geminiNativeGenerateRequest struct {
	Contents          []geminiNativeContent         `json:"contents"`
	SystemInstruction *geminiNativeContent          `json:"systemInstruction,omitempty"`
	Tools             []geminiNativeTool            `json:"tools,omitempty"`
	ToolConfig        *geminiNativeToolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *geminiNativeGenerationConfig `json:"generationConfig,omitempty"`
	SafetySettings    json.RawMessage               `json:"safetySettings,omitempty"`
	CachedContent     string                        `json:"cachedContent,omitempty"`
	Labels            map[string]string             `json:"labels,omitempty"`
}

// parseGeminiGenerateRequest decodes one generateContent body.
// DisallowUnknownFields rejects any field the shape above does not
// declare, with an error naming the field; the presence checks
// below reject the declared-but-unmappable fields the same way.
func parseGeminiGenerateRequest(raw []byte) (*geminiNativeGenerateRequest, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("empty request body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var in geminiNativeGenerateRequest
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("request body must be a single JSON object")
	}
	switch {
	case len(in.SafetySettings) > 0:
		return nil, errors.New("safetySettings is not supported on this gateway: per-request safety settings have no mapping to the canonical request (guardrails are operator-configured)")
	case in.CachedContent != "":
		return nil, errors.New("cachedContent is not supported on this gateway: explicit cached content has no mapping to the canonical request")
	case len(in.Labels) > 0:
		return nil, errors.New("labels is not supported on this gateway: request labels have no mapping to the canonical request")
	case len(in.Contents) == 0:
		return nil, errors.New("contents is required")
	}
	return &in, nil
}

// geminiToChatRequest translates one parsed Gemini-native request
// into the canonical form the engine routes. Every rejection names
// the Gemini-native field that cannot be represented.
func geminiToChatRequest(model string, in *geminiNativeGenerateRequest) (provider.ChatRequest, error) {
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return provider.ChatRequest{}, errors.New("model is required")
	}
	if in == nil {
		return provider.ChatRequest{}, errors.New("empty request body")
	}
	req := provider.ChatRequest{Model: model}

	if in.SystemInstruction != nil {
		role := in.SystemInstruction.Role
		if role != "" && role != "system" {
			return provider.ChatRequest{}, fmt.Errorf("systemInstruction.role %q is not supported (want \"system\" or absent)", role)
		}
		text, err := geminiTextParts(in.SystemInstruction.Parts, "systemInstruction")
		if err != nil {
			return provider.ChatRequest{}, err
		}
		if text != "" {
			req.Messages = append(req.Messages, provider.Message{Role: "system", Content: text})
		}
	}

	for i, c := range in.Contents {
		role := c.Role
		if role == "" {
			role = "user" // Gemini default when the role is omitted.
		}
		switch role {
		case "user":
			msg, err := geminiUserContent(c.Parts, i)
			if err != nil {
				return provider.ChatRequest{}, err
			}
			req.Messages = append(req.Messages, msg)
		case "model":
			msg, err := geminiModelContent(c.Parts, i)
			if err != nil {
				return provider.ChatRequest{}, err
			}
			req.Messages = append(req.Messages, msg)
		case "function":
			msgs, err := geminiFunctionContent(c.Parts, i)
			if err != nil {
				return provider.ChatRequest{}, err
			}
			req.Messages = append(req.Messages, msgs...)
		default:
			return provider.ChatRequest{}, fmt.Errorf("contents[%d].role %q is not supported (want \"user\", \"model\" or \"function\")", i, c.Role)
		}
	}
	if len(req.Messages) == 0 {
		return provider.ChatRequest{}, errors.New("contents is required (no representable messages)")
	}
	// Gemini requires the first content to be a user turn.
	for _, m := range req.Messages {
		if m.Role == "system" {
			continue
		}
		if m.Role != "user" {
			return provider.ChatRequest{}, errors.New("contents must start with a user turn")
		}
		break
	}

	if err := geminiApplyTools(&req, in); err != nil {
		return provider.ChatRequest{}, err
	}
	if err := geminiApplyGenerationConfig(&req, in); err != nil {
		return provider.ChatRequest{}, err
	}
	return req, nil
}

// geminiPartFieldCount counts how many payload fields one part sets.
// A part setting more than one is ambiguous: reject it rather
// than silently dropping all but one.
func geminiPartFieldCount(p geminiNativePart) int {
	n := 0
	if p.Text != "" {
		n++
	}
	if p.InlineData != nil {
		n++
	}
	if p.FileData != nil {
		n++
	}
	if p.FunctionCall != nil {
		n++
	}
	if p.FunctionResponse != nil {
		n++
	}
	if p.ExecutableCode != nil {
		n++
	}
	if p.CodeExecutionResult != nil {
		n++
	}
	// A thought part is a text part with a flag, not a second
	// payload: Thought / ThoughtSignature ride on the text and
	// do not count.
	return n
}

// geminiTextParts joins the text parts of a systemInstruction (or
// any text-only content). A non-text part has no place in a
// system instruction and is rejected by name.
func geminiTextParts(parts []geminiNativePart, where string) (string, error) {
	var sb strings.Builder
	for j, p := range parts {
		if n := geminiPartFieldCount(p); n > 1 {
			return "", fmt.Errorf("%s.parts[%d] sets more than one payload field (only one of text, inlineData, functionCall, functionResponse is allowed)", where, j)
		}
		switch {
		case p.Text != "":
			sb.WriteString(p.Text)
		case p.InlineData != nil:
			return "", fmt.Errorf("%s.parts[%d].inlineData is not supported in a system instruction (images cannot appear in a system message)", where, j)
		case p.FileData != nil:
			return "", fmt.Errorf("%s.parts[%d].fileData is not supported on this gateway: File API URIs have no mapping to the canonical request", where, j)
		case p.FunctionCall != nil:
			return "", fmt.Errorf("%s.parts[%d].functionCall is not supported in a system instruction", where, j)
		case p.FunctionResponse != nil:
			return "", fmt.Errorf("%s.parts[%d].functionResponse is not supported in a system instruction", where, j)
		case p.ExecutableCode != nil:
			return "", fmt.Errorf("%s.parts[%d].executableCode is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		case p.CodeExecutionResult != nil:
			return "", fmt.Errorf("%s.parts[%d].codeExecutionResult is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		default:
			return "", fmt.Errorf("%s.parts[%d] is empty (a part must carry text, inlineData, functionCall or functionResponse)", where, j)
		}
	}
	return sb.String(), nil
}

// geminiUserContent converts one user content: text parts join
// into Content (and into Parts, so images survive); inlineData
// becomes an image_url data: URL, which the existing Gemini wire
// converter accepts back as inlineData.
func geminiUserContent(parts []geminiNativePart, index int) (provider.Message, error) {
	where := fmt.Sprintf("contents[%d]", index)
	var text strings.Builder
	var cparts []provider.ContentPart
	for j, p := range parts {
		if n := geminiPartFieldCount(p); n > 1 {
			return provider.Message{}, fmt.Errorf("%s.parts[%d] sets more than one payload field (only one of text, inlineData, functionCall, functionResponse is allowed)", where, j)
		}
		switch {
		case p.Text != "":
			text.WriteString(p.Text)
			cparts = append(cparts, provider.ContentPart{Type: "text", Text: p.Text})
		case p.InlineData != nil:
			url := "data:" + p.InlineData.MimeType + ";base64," + p.InlineData.Data
			cparts = append(cparts, provider.ContentPart{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: url}})
		case p.FileData != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].fileData is not supported on this gateway: File API URIs have no mapping to the canonical request", where, j)
		case p.FunctionCall != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].functionCall is not supported in a user turn (function calls belong in a model turn)", where, j)
		case p.FunctionResponse != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].functionResponse is not supported in a user turn (tool results belong in a function turn)", where, j)
		case p.ExecutableCode != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].executableCode is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		case p.CodeExecutionResult != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].codeExecutionResult is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		default:
			return provider.Message{}, fmt.Errorf("%s.parts[%d] is empty (a part must carry text, inlineData, functionCall or functionResponse)", where, j)
		}
	}
	msg := provider.Message{Role: "user", Content: text.String()}
	if len(cparts) > 0 {
		msg.Parts = cparts
	}
	return msg, nil
}

// geminiModelContent converts one model content: text plus
// functionCall parts become an assistant message with tool calls;
// thought parts become the message's captured reasoning state
// (the same shape the Gemini wire converter renders back into
// thought parts on the next hop).
func geminiModelContent(parts []geminiNativePart, index int) (provider.Message, error) {
	where := fmt.Sprintf("contents[%d]", index)
	var text strings.Builder
	var toolCalls []provider.ToolCall
	var thought strings.Builder
	var thoughtSig string
	var bareSigs []string
	for j, p := range parts {
		if n := geminiPartFieldCount(p); n > 1 {
			return provider.Message{}, fmt.Errorf("%s.parts[%d] sets more than one payload field (only one of text, inlineData, functionCall, functionResponse is allowed)", where, j)
		}
		switch {
		case p.Thought:
			// Thought text is internal reasoning, never visible
			// content; a thought part with no text but a
			// signature carries the signature only.
			if p.Text != "" {
				thought.WriteString(p.Text)
			}
			if p.ThoughtSignature != "" {
				thoughtSig = p.ThoughtSignature
			}
			if p.Text == "" && p.ThoughtSignature == "" {
				return provider.Message{}, fmt.Errorf("%s.parts[%d] is an empty thought part (a thought part must carry text or a thoughtSignature)", where, j)
			}
		case p.Text != "":
			text.WriteString(p.Text)
			if p.ThoughtSignature != "" {
				bareSigs = append(bareSigs, p.ThoughtSignature)
			}
		case p.FunctionCall != nil:
			name := strings.TrimSpace(p.FunctionCall.Name)
			if name == "" {
				return provider.Message{}, fmt.Errorf("%s.parts[%d].functionCall.name is required", where, j)
			}
			args := strings.TrimSpace(string(p.FunctionCall.Args))
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return provider.Message{}, fmt.Errorf("%s.parts[%d].functionCall.args is not valid JSON", where, j)
			}
			toolCalls = append(toolCalls, provider.ToolCall{
				ID:   fmt.Sprintf("call_%d", len(toolCalls)),
				Type: "function",
				Function: provider.ToolCallFunction{
					Name:      name,
					Arguments: args,
				},
			})
			if p.ThoughtSignature != "" {
				bareSigs = append(bareSigs, p.ThoughtSignature)
			}
		case p.InlineData != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].inlineData is not supported in a model turn (image output has no canonical representation)", where, j)
		case p.FileData != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].fileData is not supported on this gateway: File API URIs have no mapping to the canonical request", where, j)
		case p.FunctionResponse != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].functionResponse is not supported in a model turn (tool results belong in a function turn)", where, j)
		case p.ExecutableCode != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].executableCode is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		case p.CodeExecutionResult != nil:
			return provider.Message{}, fmt.Errorf("%s.parts[%d].codeExecutionResult is not supported on this gateway: code execution has no mapping to the canonical request", where, j)
		default:
			// A part carrying only a thoughtSignature is
			// captured, never dropped.
			if p.ThoughtSignature != "" {
				bareSigs = append(bareSigs, p.ThoughtSignature)
				continue
			}
			return provider.Message{}, fmt.Errorf("%s.parts[%d] is empty (a part must carry text, inlineData, functionCall or functionResponse)", where, j)
		}
	}
	msg := provider.Message{Role: "assistant", Content: text.String(), ToolCalls: toolCalls}
	if thought.Len() > 0 || thoughtSig != "" || len(bareSigs) > 0 {
		st := &reasoning.State{SourceWire: reasoning.WireGemini}
		if thought.Len() > 0 {
			st.Add(reasoning.Block{Kind: reasoning.KindThought, Text: thought.String(), Signature: thoughtSig})
		} else if thoughtSig != "" {
			bareSigs = append([]string{thoughtSig}, bareSigs...)
		}
		for _, sig := range bareSigs {
			st.Add(reasoning.Block{Kind: reasoning.KindThoughtSignature, Signature: sig})
		}
		msg.Reasoning = st
	}
	return msg, nil
}

// geminiFunctionContent converts one function content into one
// tool message per functionResponse part. Gemini carries no
// call IDs, so the function name is the ToolCallID — the same
// convention the existing Gemini wire converter uses in reverse
// (it names a functionResponse from the preceding call, falling
// back to the call ID).
func geminiFunctionContent(parts []geminiNativePart, index int) ([]provider.Message, error) {
	where := fmt.Sprintf("contents[%d]", index)
	if len(parts) == 0 {
		return nil, fmt.Errorf("%s.parts is required (a function turn must carry at least one functionResponse)", where)
	}
	var out []provider.Message
	for j, p := range parts {
		if n := geminiPartFieldCount(p); n > 1 {
			return nil, fmt.Errorf("%s.parts[%d] sets more than one payload field (only one of text, inlineData, functionCall, functionResponse is allowed)", where, j)
		}
		if p.FunctionResponse == nil {
			return nil, fmt.Errorf("%s.parts[%d] is not a functionResponse (a function turn carries only functionResponse parts)", where, j)
		}
		name := strings.TrimSpace(p.FunctionResponse.Name)
		if name == "" {
			return nil, fmt.Errorf("%s.parts[%d].functionResponse.name is required", where, j)
		}
		out = append(out, provider.Message{
			Role:       "tool",
			ToolCallID: name,
			Content:    geminiFunctionResultText(p.FunctionResponse.Response),
		})
	}
	return out, nil
}

// geminiFunctionResultText renders a functionResponse response
// value as the tool message's text: a JSON string unwraps to the
// string, anything else compacts to its JSON. An absent response
// is "{}" (the wire converter wraps a missing value the same way
// on the way out).
func geminiFunctionResultText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "{}"
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err == nil {
		return compact.String()
	}
	return trimmed
}

// geminiApplyTools maps tools + toolConfig onto the canonical
// Tools / ToolChoice. Mode ANY with one allowed name becomes a
// named function choice; ANY with several becomes the "any" form
// (the wire converter accepts both); NONE becomes "none"; AUTO
// (or absent) leaves the choice unset, which the wire reads as
// AUTO. An allowedFunctionNames list under AUTO is a 400: the
// names would be silently ignored.
func geminiApplyTools(req *provider.ChatRequest, in *geminiNativeGenerateRequest) error {
	for ti, t := range in.Tools {
		if len(t.FunctionDeclarations) == 0 {
			return fmt.Errorf("tools[%d] has no functionDeclarations", ti)
		}
		for di, d := range t.FunctionDeclarations {
			if strings.TrimSpace(d.Name) == "" {
				return fmt.Errorf("tools[%d].functionDeclarations[%d].name is required", ti, di)
			}
			params := d.Parameters
			if len(params) == 0 {
				params = d.ParametersJSONSchema
			}
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			var td provider.ToolDefinition
			td.Type = "function"
			td.Function.Name = d.Name
			td.Function.Description = d.Description
			td.Function.Parameters = params
			req.Tools = append(req.Tools, td)
		}
	}
	if in.ToolConfig == nil || in.ToolConfig.FunctionCallingConfig == nil {
		return nil
	}
	fcc := in.ToolConfig.FunctionCallingConfig
	switch fcc.Mode {
	case "", "AUTO":
		if len(fcc.AllowedFunctionNames) > 0 {
			return errors.New("toolConfig.functionCallingConfig.allowedFunctionNames requires mode ANY (it is ignored under AUTO)")
		}
	case "ANY":
		switch len(fcc.AllowedFunctionNames) {
		case 0:
			req.ToolChoice = json.RawMessage(`"required"`)
		case 1:
			choice, err := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": fcc.AllowedFunctionNames[0]},
			})
			if err != nil {
				return fmt.Errorf("toolConfig.functionCallingConfig.allowedFunctionNames: %v", err)
			}
			req.ToolChoice = choice
		default:
			req.ToolChoice = json.RawMessage(`{"type":"any"}`)
		}
	case "NONE":
		req.ToolChoice = json.RawMessage(`"none"`)
	default:
		return fmt.Errorf("toolConfig.functionCallingConfig.mode %q is not supported (want \"AUTO\", \"ANY\" or \"NONE\")", fcc.Mode)
	}
	return nil
}

// geminiApplyGenerationConfig maps generationConfig onto the
// canonical sampling fields. Fields with no canonical mapping
// are rejected by name; an explicit temperature/topP of 0 is
// indistinguishable from absent in the canonical form (the same
// pre-existing limitation as /v1/chat/completions) and is not
// forwarded. A client-supplied thinkingBudget rides
// ThinkingBudget: with no named effort on this path the engine's
// effort forwarding leaves it untouched, and the Gemini client
// sends it as thinkingConfig.thinkingBudget. CAVEAT: if a
// tier/group pins a named effort AND the operator effort table
// has a Gemini budget, the engine overwrites the client's value
// with the pinned budget — the passthrough holds only when no
// named effort is pinned on this request.
func geminiApplyGenerationConfig(req *provider.ChatRequest, in *geminiNativeGenerateRequest) error {
	gc := in.GenerationConfig
	if gc == nil {
		return nil
	}
	switch {
	case gc.TopK != nil:
		return errors.New("generationConfig.topK is not supported on this gateway: top-K sampling has no mapping to the canonical request")
	case gc.CandidateCount != nil && *gc.CandidateCount != 1:
		return fmt.Errorf("generationConfig.candidateCount %d is not supported on this gateway: only 1 candidate is served", *gc.CandidateCount)
	case gc.ResponseMimeType != "":
		return errors.New("generationConfig.responseMimeType is not supported on this gateway: structured output has no mapping to the canonical request")
	case len(gc.ResponseSchema) > 0:
		return errors.New("generationConfig.responseSchema is not supported on this gateway: structured output has no mapping to the canonical request")
	case gc.PresencePenalty != nil:
		return errors.New("generationConfig.presencePenalty is not supported on this gateway: presence penalty has no mapping to the canonical request")
	case gc.FrequencyPenalty != nil:
		return errors.New("generationConfig.frequencyPenalty is not supported on this gateway: frequency penalty has no mapping to the canonical request")
	case gc.Seed != nil:
		return errors.New("generationConfig.seed is not supported on this gateway: seeded sampling has no mapping to the canonical request")
	case gc.ResponseLogprobs != nil || gc.Logprobs != nil:
		return errors.New("generationConfig.responseLogprobs/logprobs is not supported on this gateway: logprobs have no mapping to the canonical request")
	}
	if gc.Temperature != nil {
		if *gc.Temperature < 0 || *gc.Temperature > 2 {
			return fmt.Errorf("generationConfig.temperature %v is outside the 0-2 range", *gc.Temperature)
		}
		req.Temperature = *gc.Temperature
	}
	if gc.TopP != nil {
		if *gc.TopP < 0 || *gc.TopP > 1 {
			return fmt.Errorf("generationConfig.topP %v is outside the 0-1 range", *gc.TopP)
		}
		req.TopP = *gc.TopP
	}
	if gc.MaxOutputTokens != nil {
		if *gc.MaxOutputTokens < 0 {
			return fmt.Errorf("generationConfig.maxOutputTokens %d must not be negative", *gc.MaxOutputTokens)
		}
		req.MaxTokens = *gc.MaxOutputTokens
	}
	if len(gc.StopSequences) > 5 {
		return fmt.Errorf("generationConfig.stopSequences has %d entries, at most 5 are supported", len(gc.StopSequences))
	}
	req.Stop = append(req.Stop, gc.StopSequences...)
	if gc.ThinkingConfig != nil {
		if gc.ThinkingConfig.IncludeThoughts != nil {
			return errors.New("generationConfig.thinkingConfig.includeThoughts is not supported on this gateway: thought streaming has no mapping to the canonical request")
		}
		if gc.ThinkingConfig.ThinkingBudget != nil {
			if *gc.ThinkingConfig.ThinkingBudget < 0 {
				return fmt.Errorf("generationConfig.thinkingConfig.thinkingBudget %d must not be negative", *gc.ThinkingConfig.ThinkingBudget)
			}
			req.ThinkingBudget = *gc.ThinkingConfig.ThinkingBudget
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Response conversion: canonical -> Gemini-native.
// ---------------------------------------------------------------------------

// geminiFinishReason maps a canonical finish reason onto the
// Gemini finishReason. An empty or unknown reason is omitted
// (""), never invented.
func geminiFinishReason(canonical string) string {
	switch canonical {
	case "stop":
		return "STOP"
	case "length":
		return "MAX_TOKENS"
	case "tool_calls":
		return "TOOL_CALLS"
	case "content_filter":
		return "SAFETY"
	default:
		return ""
	}
}

// geminiUsageMetadata maps canonical usage onto Gemini
// usageMetadata, or nil when nothing is known. A zero total is
// derived from the legs, exactly as the wire converter does on
// the way in. promptFeedback is deliberately absent: the wire
// layer (ConvertGeminiResponse) maps a block reason to a
// content_filter finish and drops the reason itself, so by the
// time the handler converts, the canonical response carries no
// block reason — and inventing one (SAFETY vs another cause)
// would be guessing.
func geminiUsageMetadata(u provider.Usage) *wire.GeminiUsageMetadata {
	if u == (provider.Usage{}) {
		return nil
	}
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CompletionTokens
	}
	return &wire.GeminiUsageMetadata{
		PromptTokenCount:        u.PromptTokens,
		CandidatesTokenCount:    u.CompletionTokens,
		TotalTokenCount:         total,
		CachedContentTokenCount: u.CachedTokens,
		ThoughtsTokenCount:      u.ReasoningTokens,
	}
}

// chatResponseToGemini renders a completed canonical response as
// a Gemini GenerateContentResponse: text as one text part,
// tool calls as functionCall parts (arguments validated as JSON),
// finish reason mapped, usage mapped, modelVersion from the
// engine-reported model.
func chatResponseToGemini(resp *provider.ChatResponse) (*wire.GeminiResponse, error) {
	if resp == nil {
		return nil, errors.New("nil chat response")
	}
	var parts []wire.GeminiPart
	if resp.Content != "" {
		parts = append(parts, wire.GeminiPart{Text: resp.Content})
	}
	for i, tc := range resp.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("tool call %d (%q) arguments are not valid JSON", i, tc.Function.Name)
		}
		parts = append(parts, wire.GeminiPart{FunctionCall: &wire.GeminiFunctionCall{
			Name: tc.Function.Name,
			Args: json.RawMessage(args),
		}})
	}
	cand := wire.GeminiCandidate{FinishReason: geminiFinishReason(resp.FinishReason)}
	if len(parts) > 0 {
		cand.Content = &wire.GeminiContent{Role: "model", Parts: parts}
	}
	return &wire.GeminiResponse{
		Candidates:    []wire.GeminiCandidate{cand},
		UsageMetadata: geminiUsageMetadata(resp.Usage),
		ModelVersion:  resp.Model,
	}, nil
}

// geminiTerminalEvent renders the terminal event of a streamed
// generateContent: the completed tool calls (from the reassembled
// response — chunk tool-call deltas carry only fragments, and
// Gemini sends functionCall args complete), the finish reason and
// the usage. The text is NOT repeated here: it already streamed
// as deltas.
func geminiTerminalEvent(resp *provider.ChatResponse, model string) (*wire.GeminiResponse, error) {
	if resp == nil {
		return nil, errors.New("nil chat response")
	}
	var parts []wire.GeminiPart
	for i, tc := range resp.ToolCalls {
		args := strings.TrimSpace(tc.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, fmt.Errorf("tool call %d (%q) arguments are not valid JSON", i, tc.Function.Name)
		}
		parts = append(parts, wire.GeminiPart{FunctionCall: &wire.GeminiFunctionCall{
			Name: tc.Function.Name,
			Args: json.RawMessage(args),
		}})
	}
	cand := wire.GeminiCandidate{FinishReason: geminiFinishReason(resp.FinishReason)}
	if len(parts) > 0 {
		cand.Content = &wire.GeminiContent{Role: "model", Parts: parts}
	}
	version := resp.Model
	if version == "" {
		version = model
	}
	return &wire.GeminiResponse{
		Candidates:    []wire.GeminiCandidate{cand},
		UsageMetadata: geminiUsageMetadata(resp.Usage),
		ModelVersion:  version,
	}, nil
}

// ---------------------------------------------------------------------------
// Gemini-shaped errors.
// ---------------------------------------------------------------------------

// geminiErrorEnvelope is the {"error": {code, message, status}}
// envelope the real Gemini API returns.
type geminiErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func newGeminiErrorEnvelope(code int, status, message string) geminiErrorEnvelope {
	var e geminiErrorEnvelope
	e.Error.Code = code
	e.Error.Message = message
	e.Error.Status = status
	return e
}

// geminiStatusForHTTP maps an HTTP status onto a Gemini error
// status string. 5xx is UNAVAILABLE (the upstream failed);
// anything unmapped is UNKNOWN rather than a guessed status.
func geminiStatusForHTTP(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case status == http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case status == http.StatusForbidden:
		return "PERMISSION_DENIED"
	case status == http.StatusNotFound:
		return "NOT_FOUND"
	case status == http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case status >= 500:
		return "UNAVAILABLE"
	default:
		return "UNKNOWN"
	}
}

// geminiErrorDetails maps an engine failure onto (HTTP status,
// Gemini status, client-safe message, Retry-After). Upstream
// detail is never exposed: messages are gateway-generated, except
// the unrepresentable reason (which names the client's own field)
// and the gateway's own contract / budget errors.
func geminiErrorDetails(err error) (status int, geminiStatus, message string, retryAfter time.Duration) {
	var rle *provider.RateLimitError
	if errors.As(err, &rle) {
		return http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "upstream rate limited", rle.RetryAfter
	}
	var qe *provider.QuotaError
	if errors.As(err, &qe) {
		st := qe.StatusCode
		if st < 400 || st > 599 {
			st = http.StatusPaymentRequired
		}
		return st, "PERMISSION_DENIED", "quota exhausted", 0
	}
	if errors.Is(err, router.ErrContentRefused) ||
		provider.ClassifyError(err, provider.EndpointChatCompletions).ContentRefusal {
		return http.StatusForbidden, "PERMISSION_DENIED", "content refused by upstream", 0
	}
	var ce *router.ContractError
	if errors.As(err, &ce) {
		return ce.Status, "INVALID_ARGUMENT", ce.Msg, 0
	}
	var tb *router.TokenLimitBreach
	if errors.As(err, &tb) {
		seconds := int(time.Until(tb.NextReset) / time.Second)
		if seconds < 1 {
			seconds = 1
		}
		return http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", tb.Error(), time.Duration(seconds) * time.Second
	}
	var ib *router.InternalBudgetBreach
	if errors.As(err, &ib) {
		return http.StatusPaymentRequired, "FAILED_PRECONDITION", ib.Error(), 0
	}
	var unrep *wire.UnrepresentableError
	if errors.As(err, &unrep) {
		return http.StatusBadRequest, "INVALID_ARGUMENT", unrep.Reason, 0
	}
	var ue *provider.UpstreamError
	if errors.As(err, &ue) {
		st := ue.StatusCode
		if st < 400 || st > 599 {
			st = http.StatusBadGateway
		}
		return st, geminiStatusForHTTP(st), "upstream provider error", 0
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "DEADLINE_EXCEEDED", "request deadline exceeded", 0
	}
	return http.StatusBadGateway, "UNAVAILABLE", "upstream provider error", 0
}

// writeGeminiError answers a pre-commit failure with the Gemini
// error envelope at the mapped status. Retry-After rides the
// header when the error carries one.
func writeGeminiError(w http.ResponseWriter, err error) {
	status, geminiStatus, message, retryAfter := geminiErrorDetails(err)
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter/time.Second)))
	}
	writeJSON(w, status, newGeminiErrorEnvelope(status, geminiStatus, message))
}

// writeGeminiClientError answers a pre-engine client rejection
// (validation, unknown action, wrong method, oversize body,
// engine not configured) with the Gemini error envelope at the
// given HTTP status — never the shared OpenAI-shaped writeError
// / writeServerError, which belong to OpenAI-native surfaces.
// geminiStatusForHTTP maps the status; unmapped statuses (405,
// 413) honestly report UNKNOWN rather than a guessed status.
func writeGeminiClientError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, newGeminiErrorEnvelope(status, geminiStatusForHTTP(status), msg))
}

// writeGeminiUpstreamFailure answers a response-conversion
// failure the way writeUpstreamFailure does for OpenAI-native
// surfaces — HTTP 502, counted in the upstream-failure metric —
// but in the Gemini envelope: writeGeminiError maps the generic
// upstream error to 502 / UNAVAILABLE with the generic message
// "upstream provider error" and no upstream detail.
func (s *Server) writeGeminiUpstreamFailure(w http.ResponseWriter) {
	if s.recorder != nil {
		s.recorder.IncUpstreamFailure()
	}
	writeGeminiError(w, errNilUpstreamResponse)
}

// geminiStreamErrorEvent renders a mid-stream failure as the SSE
// error payload Gemini uses: a data event carrying the same
// envelope. The HTTP status was already committed as 200 when
// the stream started, so the code inside the envelope is the
// mapped status.
func geminiStreamErrorEvent(err error) []byte {
	status, geminiStatus, message, _ := geminiErrorDetails(err)
	payload, _ := json.Marshal(newGeminiErrorEnvelope(status, geminiStatus, message))
	return []byte("data: " + string(payload) + "\n\n")
}

// ---------------------------------------------------------------------------
// Handlers.
// ---------------------------------------------------------------------------

// geminiModelEntry is one row of a GET /v1beta/models response:
// the Gemini resource name plus the generation methods this
// gateway serves for it. No price, no display name, no token
// limits: the catalog records none of those, and the honesty
// rule is to omit what is unknown rather than invent it.
type geminiModelEntry struct {
	Name                       string   `json:"name"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods,omitempty"`
}

// geminiModelsListResponse is the GET /v1beta/models payload.
type geminiModelsListResponse struct {
	Models []geminiModelEntry `json:"models"`
}

// handleGeminiModels serves GET /v1beta/models: the Gemini models
// in the catalog — models of providers whose wire format is
// Gemini — under their models/<id> resource names. A registry
// that cannot report wire formats yields an honest empty list
// (no wire format, no Gemini claim), never a guessed one.
// Deprecated models are hidden by default (?include=deprecated),
// exactly as on GET /v1/models.
func (s *Server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeGeminiClientError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var rows []provider.DashboardModel
	if ml, ok := s.registry.(dashboardModelLister); ok {
		rows = ml.DashboardModels()
	}
	seen := make(map[string]struct{})
	out := make([]geminiModelEntry, 0)
	includeDeprecated := r.URL.Query().Get("include") == "deprecated"
	for _, m := range rows {
		if m.WireFormat != provider.WireFormatGemini {
			continue
		}
		if m.ID == "" {
			continue
		}
		if _, dup := seen[m.ID]; dup {
			continue
		}
		seen[m.ID] = struct{}{}
		if m.Deprecated && !includeDeprecated {
			continue
		}
		out = append(out, geminiModelEntry{
			Name:                       "models/" + m.ID,
			SupportedGenerationMethods: append([]string(nil), geminiNativeMethods...),
		})
	}
	writeJSON(w, http.StatusOK, geminiModelsListResponse{Models: out})
}

// handleGeminiModelAction serves POST /v1beta/models/{rest...}:
// the :generateContent and :streamGenerateContent actions. The
// rest segment is "<model>:generateContent" or
// "<model>:streamGenerateContent"; a leading "models/" prefix on
// the model is accepted (the form ListModels returns) and
// stripped. Anything else is a 404 naming the action.
func (s *Server) handleGeminiModelAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeGeminiClientError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rest := r.PathValue("rest")
	rest = strings.TrimPrefix(rest, "models/")
	var model, action string
	if i := strings.LastIndex(rest, ":"); i >= 0 {
		model, action = rest[:i], rest[i+1:]
	}
	switch action {
	case "generateContent":
		s.geminiGenerate(w, r, model, false)
	case "streamGenerateContent":
		s.geminiGenerate(w, r, model, true)
	default:
		writeGeminiClientError(w, http.StatusNotFound, fmt.Sprintf("unknown Gemini action %q (want :generateContent or :streamGenerateContent)", action))
	}
}

// geminiGenerate is the shared core of :generateContent and
// :streamGenerateContent. It runs the same pre-call gates as
// POST /v1/messages (pre-body admission, intent tier, access
// schedule, key budget, token limits, quota pools/schedules,
// internal budgets, team budget, traffic, throttle, session,
// readiness), then translates the Gemini-native body, applies
// the sync guardrails, and routes through the engine — whose
// Gemini-wire attempt uses the existing geminiClient. Tool calls
// are returned to the client, never auto-executed (no MCP loop
// on this surface: the Gemini client owns tool execution).
// Async guardrails, per-request compression and the plugin
// pre/post hooks are NOT wired here yet (see the slice report);
// the route is TierManagement + scope "chat" at registration,
// so only scoped gateway keys can call it.
//
// EndpointKind: this surface deliberately leaves req.Endpoint
// unset, so a Gemini request replays exactly like a chat
// completion — which is right, as there is no upstream-held
// state to exempt. (A distinct kind would need more than a
// constant — EndpointKind is a plain string type, so a constant
// could be declared here, but endpointForProjection in
// internal/router/cascade.go FAIL-CLOSES on unknown kinds, so
// naming the surface distinctly needs a cascade.go case as
// well; and ReplayExemption in internal/router/preread.go gives
// this surface no exemption, as for chat.)
func (s *Server) geminiGenerate(w http.ResponseWriter, r *http.Request, model string, stream bool) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()

	if strings.TrimSpace(model) == "" {
		writeGeminiClientError(w, http.StatusBadRequest, "model is required")
		return
	}
	if s.preBodyAdmission(w, r) {
		return
	}
	if refused := s.checkIntentTier(w, r, model); refused {
		return
	}
	if refused := s.checkKeyAccessSchedule(w, r); refused {
		return
	}
	if refused := s.checkKeyBudgetForModel(w, r, model); refused {
		return
	}
	if refused := s.checkKeyTokenLimits(w, r, "", model); refused {
		return
	}
	if refused := s.checkQuotaPools(w, r); refused {
		return
	}
	if refused := s.checkQuotaSchedules(w, r); refused {
		return
	}
	if refused := s.checkInternalBudgets(w, r, model); refused {
		return
	}
	if !s.checkTeamBudget(w, r) {
		return
	}
	// G20: the model is the URL path model, known before the body
	// is read — so the gate runs as checkKeyTrafficModel, not
	// checkKeyTrafficRequestsOnly: the ModelAllowed / ForcedModel
	// policy (checkKeyPolicy) runs BEFORE the limiter admits the
	// request, so a model refusal consumes no traffic slot and no
	// provider is ever called. Same 403 key_forced_model /
	// key_model_not_allowed semantics as the chat path. The token
	// axis still does not apply (no local prompt-token estimate
	// exists on this path), so 0 tokens are admitted.
	if refused := s.checkKeyTrafficModel(w, r, 0, model); refused {
		return
	}
	if !s.throttleKeyDelay(r) {
		return
	}
	sessionKey, admitted := s.tryEnterKeySession(w, r)
	if !admitted {
		return
	}
	defer s.exitKeySession(sessionKey)
	if !s.checkLocalReadiness(w, r, model) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeGeminiClientError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeGeminiClientError(w, http.StatusBadRequest, "invalid request body")
		}
		return
	}
	in, err := parseGeminiGenerateRequest(raw)
	if err != nil {
		writeGeminiClientError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, err := geminiToChatRequest(model, in)
	if err != nil {
		writeGeminiClientError(w, http.StatusBadRequest, err.Error())
		return
	}
	req.CallerID = s.callerKeyID(r)
	req.CacheControls = parseCacheControls(r)
	req.Stream = stream
	if blocked := s.applyGuardrails(ctx, w, &req, s.keyGuardrailScope(r)); blocked {
		return
	}
	if s.engine == nil {
		writeGeminiClientError(w, http.StatusServiceUnavailable, "chat engine not configured")
		return
	}
	if stream {
		s.geminiGenerateStream(w, ctx, req, start)
		return
	}
	resp, err := s.engine.Chat(ctx, req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.recordMetrics(ctx, req, nil, err, start)
		meta := s.responseMeta(ctx, req, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, s.statusForEngineError(err))
		writeGeminiError(w, err)
		return
	}
	if resp == nil {
		s.recordMetrics(ctx, req, nil, errNilUpstreamResponse, start)
		meta := s.responseMeta(ctx, req, nil, start)
		s.writeResponseHeaders(w, ctx, meta)
		s.recordTelemetry(ctx, meta, http.StatusBadGateway)
		writeGeminiError(w, errNilUpstreamResponse)
		return
	}
	if blocked := s.applyResponseGuardrails(w, resp, s.keyGuardrailScope(r)); blocked {
		return
	}
	s.recordMetrics(ctx, req, resp, nil, start)
	gem, err := chatResponseToGemini(resp)
	if err != nil {
		s.writeGeminiUpstreamFailure(w)
		return
	}
	meta := s.responseMeta(ctx, req, resp, start)
	s.writeResponseHeaders(w, ctx, meta)
	s.recordTelemetry(ctx, meta, http.StatusOK)
	s.setStrategyHeader(w)
	writeJSON(w, http.StatusOK, gem)
}

// geminiGenerateStream serves :streamGenerateContent as Gemini
// SSE (alt=sse shape): one `data: {GenerateContentResponse}`
// event per text delta, a single terminal event carrying the
// finish reason, the completed tool calls and the usage, and no
// [DONE] sentinel — Gemini streams simply end. A chunk-aware
// engine streams tool calls and the real finish reason; a
// text-only engine streams text deltas and the terminal event
// still carries the reassembled tool calls and finish reason. A
// failure after the first byte is a terminal SSE error event
// (the Gemini error envelope), never a second HTTP status.
func (s *Server) geminiGenerateStream(w http.ResponseWriter, ctx context.Context, req provider.ChatRequest, start time.Time) {
	s.writeStreamStartHeaders(w, ctx, req.Model)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(ev *wire.GeminiResponse) error {
		raw, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	emitText := func(delta string) error {
		return emit(&wire.GeminiResponse{
			Candidates: []wire.GeminiCandidate{{
				Content: &wire.GeminiContent{
					Role:  "model",
					Parts: []wire.GeminiPart{{Text: delta}},
				},
			}},
			ModelVersion: req.Model,
		})
	}

	// Stream redaction (B3-26 / :798): the Gemini stream calls
	// the engine directly, not handleChatStream, so the response
	// checks never ran on its deltas. Same per-stream redactor
	// as handleChatStream: PEM-only by default, the store-wired
	// ResponseStreamRedactor when response sanitization is on —
	// and in block mode it cuts the stream at the first line
	// carrying a finding (see the cut checks after the engine
	// call). One redactor per stream (not safe for concurrent
	// use).
	var respRedactor *guardrails.ResponseStreamRedactor
	var redactor streamChunkRedactor = guardrails.NewStreamRedactor()
	if s.responseSanitization {
		respRedactor = s.guardrails.NewResponseStreamRedactor("")
		redactor = respRedactor
	}
	// Streaming vault restore (:782): same per-stream restorer as
	// handleChatStream — redactor first, then restorer, so a token
	// split across chunks still restores.
	vaultRestorer := guardrails.NewStreamRestorer(s.reversibleVault)
	// cutBySanitization ends a stream cut by the response
	// sanitization stage in block mode (B3-22): the Gemini
	// error envelope at 403 / PERMISSION_DENIED — a policy
	// refusal, never an upstream failure. Called from two
	// sites: inside the streamErr block (the engine
	// propagated the callback sentinel) and after it (the
	// engine ignored the callback error and returned nil) —
	// the decision uses the redactor's flag, not the error
	// value, so it holds either way.
	cutBySanitization := func() {
		kind := "secret"
		if f := respRedactor.Findings(); len(f) > 0 {
			kind = f[0].Kind
		}
		s.logger.Printf("guardrail: response sanitization blocked gemini stream (kind=%s); stream cut, terminal refusal", kind)
		payload, _ := json.Marshal(newGeminiErrorEnvelope(http.StatusForbidden, "PERMISSION_DENIED", "guardrail blocked stream: "+kind))
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	var finalResp *provider.ChatResponse
	var streamErr error
	if ce, ok := s.engine.(chunkEngine); ok {
		finalResp, streamErr = ce.ChatStreamChunks(ctx, req, func(chunk provider.StreamChunk) error {
			if chunk.Content == "" {
				return nil
			}
			content := redactor.WriteChunk(chunk.Content)
			content = vaultRestorer.WriteChunk(content)
			// Response sanitization cut (B3-22, block mode):
			// the redactor withheld the first line carrying
			// a finding, so nothing further may be emitted.
			// Returning the sentinel stops the engine loop;
			// the terminal refusal is written by the cut
			// check after the engine call below.
			if respRedactor != nil && respRedactor.Blocked() {
				return errResponseGuardrailBlocked
			}
			if content == "" {
				return nil
			}
			return emitText(content)
		})
	} else {
		finalResp, streamErr = s.engine.ChatStream(ctx, req, func(delta string) error {
			if delta == "" {
				return nil
			}
			content := redactor.WriteChunk(delta)
			content = vaultRestorer.WriteChunk(content)
			if respRedactor != nil && respRedactor.Blocked() {
				return errResponseGuardrailBlocked
			}
			if content == "" {
				return nil
			}
			return emitText(content)
		})
	}
	if streamErr != nil {
		if respRedactor != nil && respRedactor.Blocked() {
			cutBySanitization()
			s.recordMetrics(ctx, req, nil, errResponseGuardrailBlocked, start)
			return
		}
		_, _ = io.WriteString(w, string(geminiStreamErrorEvent(streamErr)))
		if flusher != nil {
			flusher.Flush()
		}
		s.recordMetrics(ctx, req, nil, streamErr, start)
		return
	}
	if finalResp == nil {
		_, _ = io.WriteString(w, string(geminiStreamErrorEvent(errNilUpstreamResponse)))
		if flusher != nil {
			flusher.Flush()
		}
		s.recordMetrics(ctx, req, nil, errNilUpstreamResponse, start)
		return
	}
	// Response sanitization cut (B3-22, block mode): the engine
	// ignored the callback sentinel and returned nil, so the
	// stream must still end with a refusal, never the terminal
	// event. The decision uses the redactor's flag, not the
	// error value, so it holds either way. The withheld tail
	// is never emitted (Flush returns "" once cut).
	if respRedactor != nil && respRedactor.Blocked() {
		cutBySanitization()
		s.recordMetrics(ctx, req, finalResp, errResponseGuardrailBlocked, start)
		return
	}
	// Stream redaction (M1.7): emit the redactor's withheld
	// tail — an unclosed PEM block as one placeholder — as a
	// final text delta before the terminal event, so no secret
	// reaches the client in any chunking. The terminal event
	// does not repeat the text, so the tail must go out here.
	tail := redactor.Flush()
	if tail != "" {
		tail = vaultRestorer.WriteChunk(tail)
	}
	tail += vaultRestorer.Flush()
	if tail != "" {
		if err := emitText(tail); err != nil {
			s.recordMetrics(ctx, req, finalResp, err, start)
			return
		}
	}
	terminal, err := geminiTerminalEvent(finalResp, req.Model)
	if err != nil {
		_, _ = io.WriteString(w, string(geminiStreamErrorEvent(err)))
		if flusher != nil {
			flusher.Flush()
		}
		s.recordMetrics(ctx, req, finalResp, err, start)
		return
	}
	if err := emit(terminal); err != nil {
		s.recordMetrics(ctx, req, finalResp, err, start)
		return
	}
	s.recordMetrics(ctx, req, finalResp, nil, start)
}
