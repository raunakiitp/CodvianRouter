package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// Pair: claude -> gemini (Anthropic Messages request -> Gemini
// generateContent request) and gemini -> claude (the reverse).
//
// Both directions reuse the wire package's target types
// (wire.GeminiRequest / wire.AnthropicRequest) so the produced bodies
// carry exactly the same shapes the native wire converters emit; the
// shared code here owns only the block-level translation the two
// native converters do not face each other across.
//
// Directional divergences are recorded on each converter below.
// ---------------------------------------------------------------------------

// ConvertClaudeRequestToGemini converts an Anthropic Messages request
// into a Gemini generateContent request.
//
// DIVERGENCES (recorded, never silent):
//   - redacted_thinking blocks have no Gemini equivalent and are
//     refused (the reasoning state cannot be replayed faithfully).
//   - a tool_result whose tool_use_id does not match an earlier
//     tool_use block in the same request cannot name its
//     functionResponse and is refused.
//   - Anthropic cache_control breakpoints have no Gemini request-side
//     equivalent; they are dropped (Gemini caches implicitly).
//   - the system prompt loses its block boundaries (Gemini takes one
//     systemInstruction); block text is joined with blank lines.
func ConvertClaudeRequestToGemini(req *wire.AnthropicRequest) (*wire.GeminiRequest, error) {
	if req == nil {
		return nil, unrep("anthropic request is nil")
	}
	if len(req.Messages) == 0 {
		return nil, unrep("messages is empty")
	}

	out := &wire.GeminiRequest{}

	// System: block form or string form both become one
	// systemInstruction.
	var systemTexts []string
	if req.System != "" {
		systemTexts = append(systemTexts, req.System)
	}
	for _, b := range req.SystemBlocks {
		if b.Type == "text" && b.Text != "" {
			systemTexts = append(systemTexts, b.Text)
		}
	}
	if len(systemTexts) > 0 {
		out.SystemInstruction = &wire.GeminiContent{
			Parts: []wire.GeminiPart{{Text: strings.Join(systemTexts, "\n\n")}},
		}
	}

	// Tools and tool_choice.
	if len(req.Tools) > 0 || req.ToolChoice != nil {
		tools, toolConfig, err := claudeToolsToGemini(req.Tools, req.ToolChoice)
		if err != nil {
			return nil, err
		}
		out.Tools = tools
		out.ToolConfig = toolConfig
	}

	// Generation parameters.
	gen := &wire.GeminiGenerationConfig{}
	if req.Temperature != nil {
		gen.Temperature = req.Temperature
	}
	if req.TopP != nil {
		gen.TopP = req.TopP
	}
	if req.MaxTokens > 0 {
		m := req.MaxTokens
		gen.MaxOutputTokens = &m
	}
	if len(req.StopSequences) > 0 {
		gen.StopSequences = append([]string(nil), req.StopSequences...)
	}
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		gen.ThinkingConfig = &wire.GeminiThinkingConfig{ThinkingBudget: req.Thinking.BudgetTokens}
	}
	if gen.Temperature != nil || gen.TopP != nil || gen.MaxOutputTokens != nil ||
		len(gen.StopSequences) > 0 || gen.ThinkingConfig != nil {
		out.GenerationConfig = gen
	}

	// toolUseNames maps tool_use IDs to names so tool_result blocks
	// can name their functionResponse (Gemini identifies results by
	// function name, Anthropic by ID).
	toolUseNames := make(map[string]string)
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.ID != "" && b.Name != "" {
				toolUseNames[b.ID] = b.Name
			}
		}
	}

	for i, m := range req.Messages {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		} else if m.Role != "user" {
			return nil, unrep("messages[%d] role %q has no Gemini equivalent (anthropic messages are user/assistant only)", i, m.Role)
		}
		content := wire.GeminiContent{Role: role}
		for j, b := range m.Content {
			parts, err := claudeBlockToGeminiParts(b, i, j, toolUseNames)
			if err != nil {
				return nil, err
			}
			content.Parts = append(content.Parts, parts...)
		}
		if len(content.Parts) == 0 {
			continue
		}
		out.Contents = appendGeminiContent(out.Contents, content)
	}
	if len(out.Contents) == 0 {
		return nil, unrep("no representable messages (all messages were empty)")
	}
	if out.Contents[0].Role != "user" {
		return nil, unrep("first message must be from the user, got role %q", out.Contents[0].Role)
	}
	return out, nil
}

// claudeToolsToGemini converts Anthropic tools and tool_choice onto
// Gemini's functionDeclarations / functionCallingConfig.
func claudeToolsToGemini(tools []wire.AnthropicTool, toolChoice *wire.AnthropicToolChoice) ([]wire.GeminiTool, *wire.GeminiToolConfig, error) {
	var out []wire.GeminiTool
	if len(tools) > 0 {
		decls := make([]wire.GeminiFunctionDeclaration, 0, len(tools))
		for i, t := range tools {
			if strings.TrimSpace(t.Name) == "" {
				return nil, nil, unrep("tools[%d].name is empty", i)
			}
			params := t.InputSchema
			if len(params) == 0 {
				params = json.RawMessage(`{"type":"object"}`)
			}
			decls = append(decls, wire.GeminiFunctionDeclaration{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			})
		}
		out = []wire.GeminiTool{{FunctionDeclarations: decls}}
	}
	if toolChoice == nil {
		return out, nil, nil
	}
	cfg := &wire.GeminiToolConfig{FunctionCallingConfig: wire.GeminiFunctionCallingConfig{Mode: "AUTO"}}
	switch toolChoice.Type {
	case "auto":
		cfg.FunctionCallingConfig.Mode = "AUTO"
	case "any":
		cfg.FunctionCallingConfig.Mode = "ANY"
	case "tool":
		if toolChoice.Name == "" {
			return nil, nil, unrep("tool_choice names no tool")
		}
		cfg.FunctionCallingConfig.Mode = "ANY"
		cfg.FunctionCallingConfig.AllowedFunctionNames = []string{toolChoice.Name}
	default:
		return nil, nil, unrep("tool_choice type %q has no Gemini equivalent", toolChoice.Type)
	}
	return out, cfg, nil
}

// claudeBlockToGeminiParts converts one Anthropic content block to one
// or more Gemini parts.
func claudeBlockToGeminiParts(b wire.AnthropicBlock, msgIndex, blockIdx int, toolUseNames map[string]string) ([]wire.GeminiPart, error) {
	where := fmt.Sprintf("messages[%d] content[%d]", msgIndex, blockIdx)
	switch b.Type {
	case "text":
		if b.Text == "" {
			return nil, nil
		}
		return []wire.GeminiPart{{Text: b.Text}}, nil
	case "image":
		if b.Source == nil {
			return nil, unrep("%s is an image block with no source", where)
		}
		switch b.Source.Type {
		case "base64":
			return []wire.GeminiPart{{InlineData: &wire.GeminiInlineData{
				MimeType: b.Source.MediaType,
				Data:     b.Source.Data,
			}}}, nil
		case "url":
			// Gemini's generateContent takes inline base64 data;
			// fetching a URL is the transport layer's job, so a
			// URL-source image block cannot be represented here.
			return nil, unrep("%s is a URL-source image block; Gemini's generateContent request takes inline base64 data only", where)
		default:
			return nil, unrep("%s image source type %q is not supported", where, b.Source.Type)
		}
	case "tool_use":
		if strings.TrimSpace(b.Name) == "" {
			return nil, unrep("%s is a tool_use block with no name", where)
		}
		args := strings.TrimSpace(string(b.Input))
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, unrep("%s tool_use input is not valid JSON", where)
		}
		return []wire.GeminiPart{{FunctionCall: &wire.GeminiFunctionCall{
			Name: b.Name,
			Args: json.RawMessage(args),
		}}}, nil
	case "tool_result":
		name, ok := toolUseNames[b.ToolUseID]
		if !ok {
			return nil, unrep("%s is a tool_result with tool_use_id %q matching no earlier tool_use block (Gemini names functionResponses by function name)", where, b.ToolUseID)
		}
		text, err := claudeToolResultText(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", where, err)
		}
		return []wire.GeminiPart{{FunctionResponse: &wire.GeminiFunctionResponse{
			Name:     name,
			Response: geminiFunctionResponseValue(text),
		}}}, nil
	case "thinking":
		if strings.TrimSpace(b.Thinking) == "" && b.Signature == "" {
			return nil, nil
		}
		// Thought text replays as a thought part; the signature
		// rides along on the part. An unsigned thinking block
		// replays as thought text without a signature (Gemini
		// accepts this; the signature is dropped).
		return []wire.GeminiPart{{Text: b.Thinking, Thought: true, ThoughtSignature: b.Signature}}, nil
	case "redacted_thinking":
		return nil, unrep("%s is a redacted_thinking block, which has no Gemini equivalent (the reasoning state cannot be replayed faithfully)", where)
	default:
		return nil, unrep("%s block type %q has no Gemini equivalent", where, b.Type)
	}
}

// geminiFunctionResponseValue wraps a tool-result text in the
// {"result": ...} object form the Gemini functionResponse field takes
// (a JSON object body passes through verbatim). Mirrors the wire's
// geminiFunctionResponseValue, which is unexported there.
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

// claudeToolResultText extracts the text of a tool_result block
// (content is a string or an array of blocks).
func claudeToolResultText(b wire.AnthropicBlock) (string, error) {
	switch content := b.Content.(type) {
	case nil:
		return "", nil
	case string:
		return content, nil
	case []any:
		var texts []string
		for i, item := range content {
			obj, ok := item.(map[string]any)
			if !ok {
				return "", unrep("content[%d] has an unsupported shape", i)
			}
			if t, _ := obj["type"].(string); t == "text" {
				if s, _ := obj["text"].(string); s != "" {
					texts = append(texts, s)
				}
			} else {
				return "", unrep("content[%d] block type %q has no text-only Gemini functionResponse equivalent", i, t)
			}
		}
		return strings.Join(texts, ""), nil
	default:
		return "", unrep("content of type %T is not representable", b.Content)
	}
}

// appendGeminiContent merges a content block into the previous one when
// the roles match (Gemini requires alternating user/model turns after
// role normalisation; e.g. two user messages in a row).
func appendGeminiContent(contents []wire.GeminiContent, c wire.GeminiContent) []wire.GeminiContent {
	if len(contents) > 0 && contents[len(contents)-1].Role == c.Role {
		contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, c.Parts...)
		return contents
	}
	return append(contents, c)
}
