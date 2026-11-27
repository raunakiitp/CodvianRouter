package translate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// ---------------------------------------------------------------------------
// gemini -> claude.
// ---------------------------------------------------------------------------

// ConvertGeminiRequestToClaude converts a Gemini generateContent
// request into an Anthropic Messages request.
//
// DIVERGENCES (recorded, never silent):
//   - Gemini identifies tool results by function *name*; Anthropic by
//     tool_use_id. A functionResponse is mapped onto a tool_result
//     whose tool_use_id is the ID minted for the matching earlier
//     functionCall in the same request; a result with no matching
//     earlier call is refused.
//   - thought-signature-only parts (a signature with no thought text)
//     have no Anthropic block and are refused; unsigned thought text
//     has no Anthropic representation either (the API rejects unsigned
//     thinking blocks), so it is refused too.
//   - a functionCallingConfig mode NONE has no tool_choice equivalent;
//     the tools are dropped entirely (the same treatment the Anthropic
//     wire gives OpenAI tool_choice "none").
//   - MaxOutputTokens absent on the Gemini side leaves MaxTokens 0
//     (the Anthropic wire's "use the provider default" sentinel).
func ConvertGeminiRequestToClaude(req *wire.GeminiRequest) (*wire.AnthropicRequest, error) {
	if req == nil {
		return nil, unrep("gemini request is nil")
	}
	if len(req.Contents) == 0 {
		return nil, unrep("contents is empty")
	}

	out := &wire.AnthropicRequest{}

	// System instruction.
	if req.SystemInstruction != nil {
		var texts []string
		for _, p := range req.SystemInstruction.Parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		out.System = strings.Join(texts, "\n\n")
	}

	// Tools and tool_choice. Mode NONE drops the tools (no Anthropic
	// tool_choice "none").
	keepTools := true
	var allowedNames []string
	forceAny := false
	var forcedName string
	if req.ToolConfig != nil {
		mode := req.ToolConfig.FunctionCallingConfig.Mode
		switch mode {
		case "NONE":
			keepTools = false
		case "ANY":
			forceAny = true
			allowedNames = req.ToolConfig.FunctionCallingConfig.AllowedFunctionNames
		case "AUTO", "":
		default:
			return nil, unrep("functionCallingConfig mode %q has no Anthropic equivalent", mode)
		}
	}
	if len(req.Tools) > 0 && keepTools {
		for i, t := range req.Tools {
			for j, d := range t.FunctionDeclarations {
				if strings.TrimSpace(d.Name) == "" {
					return nil, unrep("tools[%d].functionDeclarations[%d].name is empty", i, j)
				}
				params := d.Parameters
				if len(params) == 0 {
					params = json.RawMessage(`{"type":"object"}`)
				}
				out.Tools = append(out.Tools, wire.AnthropicTool{
					Name:        d.Name,
					Description: d.Description,
					InputSchema: params,
				})
			}
		}
	}
	if forceAny && len(out.Tools) > 0 {
		if len(allowedNames) == 1 {
			forcedName = allowedNames[0]
			found := false
			for _, t := range out.Tools {
				if t.Name == forcedName {
					found = true
					break
				}
			}
			if !found {
				return nil, unrep("functionCallingConfig allows unknown function %q", forcedName)
			}
			out.ToolChoice = &wire.AnthropicToolChoice{Type: "tool", Name: forcedName}
		} else {
			out.ToolChoice = &wire.AnthropicToolChoice{Type: "any"}
		}
	}

	// Generation parameters.
	if req.GenerationConfig != nil {
		gen := req.GenerationConfig
		out.Temperature = gen.Temperature
		out.TopP = gen.TopP
		out.MaxTokens = 0
		if gen.MaxOutputTokens != nil {
			out.MaxTokens = *gen.MaxOutputTokens
		}
		out.StopSequences = append([]string(nil), gen.StopSequences...)
		if gen.ThinkingConfig != nil && gen.ThinkingConfig.ThinkingBudget > 0 {
			out.Thinking = &wire.AnthropicThinking{Type: "enabled", BudgetTokens: gen.ThinkingConfig.ThinkingBudget}
		}
	}

	// Contents. toolCallIDs maps minted IDs by function name so
	// functionResponse parts can reference the matching tool_use.
	toolCallIDs := make(map[string]string)
	toolSeq := 0
	var lastRole string
	for i, c := range req.Contents {
		role := "user"
		if c.Role == "model" {
			role = "assistant"
		} else if c.Role != "user" && c.Role != "function" {
			return nil, unrep("contents[%d] role %q has no Anthropic equivalent", i, c.Role)
		}
		var blocks []wire.AnthropicBlock
		for j, p := range c.Parts {
			blks, err := geminiPartToClaudeBlocks(p, i, j, toolCallIDs, &toolSeq)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, blks...)
		}
		if len(blocks) == 0 {
			continue
		}
		if role == lastRole && len(out.Messages) > 0 {
			// Anthropic rejects consecutive same-role messages;
			// merge (mirroring the wire's appendMessage).
			out.Messages[len(out.Messages)-1].Content = append(out.Messages[len(out.Messages)-1].Content, blocks...)
			continue
		}
		out.Messages = append(out.Messages, wire.AnthropicMessage{Role: role, Content: blocks})
		lastRole = role
	}
	if len(out.Messages) == 0 {
		return nil, unrep("no representable messages (all contents were empty)")
	}
	return out, nil
}

// geminiPartToClaudeBlocks converts one Gemini part to Anthropic
// content blocks.
func geminiPartToClaudeBlocks(p wire.GeminiPart, contentIndex, partIndex int, toolCallIDs map[string]string, toolSeq *int) ([]wire.AnthropicBlock, error) {
	where := fmt.Sprintf("contents[%d] parts[%d]", contentIndex, partIndex)
	switch {
	case p.FunctionCall != nil:
		if strings.TrimSpace(p.FunctionCall.Name) == "" {
			return nil, unrep("%s is a functionCall with no name", where)
		}
		args := strings.TrimSpace(string(p.FunctionCall.Args))
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			return nil, unrep("%s functionCall args are not valid JSON", where)
		}
		*toolSeq++
		id := fmt.Sprintf("toolu_gemini_%d", *toolSeq)
		toolCallIDs[p.FunctionCall.Name] = id
		return []wire.AnthropicBlock{{
			Type:  "tool_use",
			ID:    id,
			Name:  p.FunctionCall.Name,
			Input: json.RawMessage(args),
		}}, nil
	case p.FunctionResponse != nil:
		if strings.TrimSpace(p.FunctionResponse.Name) == "" {
			return nil, unrep("%s is a functionResponse with no name", where)
		}
		id, ok := toolCallIDs[p.FunctionResponse.Name]
		if !ok {
			return nil, unrep("%s is a functionResponse for function %q with no earlier functionCall in this request (Anthropic tool_results reference a tool_use id)", where, p.FunctionResponse.Name)
		}
		return []wire.AnthropicBlock{{
			Type:      "tool_result",
			ToolUseID: id,
			Content:   claudeToolResultContentFromGemini(p.FunctionResponse.Response),
		}}, nil
	case p.InlineData != nil:
		if p.InlineData.Data == "" {
			return nil, unrep("%s is an inlineData part with no data", where)
		}
		return []wire.AnthropicBlock{{
			Type: "image",
			Source: &wire.AnthropicImageSource{
				Type:      "base64",
				MediaType: p.InlineData.MimeType,
				Data:      p.InlineData.Data,
			},
		}}, nil
	case p.Thought:
		if p.ThoughtSignature != "" {
			// Signed thought text replays as a thinking block.
			return []wire.AnthropicBlock{{
				Type:      "thinking",
				Thinking:  p.Text,
				Signature: p.ThoughtSignature,
			}}, nil
		}
		if strings.TrimSpace(p.Text) != "" {
			// Unsigned thought text: the Anthropic API rejects
			// unsigned thinking blocks, so this cannot be
			// represented faithfully.
			return nil, unrep("%s is unsigned thought text, which has no Anthropic representation (the api rejects unsigned thinking blocks)", where)
		}
		return nil, nil
	case p.Text != "":
		return []wire.AnthropicBlock{{Type: "text", Text: p.Text}}, nil
	default:
		// Signature-only parts and unrecognised shapes.
		if p.ThoughtSignature != "" {
			return nil, unrep("%s is a bare thought signature, which has no Anthropic block equivalent", where)
		}
		return nil, unrep("%s has an unrecognised shape", where)
	}
}

// claudeToolResultContentFromGemini converts a functionResponse's
// response value into a tool_result content. The canonical {"result":
// "<text>"} form unwraps to its text so the mapping inverts the
// claude->gemini direction exactly; any other JSON object passes
// through rendered compactly.
func claudeToolResultContentFromGemini(resp json.RawMessage) string {
	if len(resp) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(resp, &s); err == nil {
		return s
	}
	var obj map[string]any
	if err := json.Unmarshal(resp, &obj); err == nil && len(obj) == 1 {
		if only, ok := obj["result"].(string); ok {
			return only
		}
	}
	// The Gemini response value is an object; render it compactly
	// as the tool result text.
	return strings.TrimSpace(string(resp))
}

// TranslateGeminiToClaudeStream is the streaming half of the
// gemini->claude pair: a Gemini-format client served by a Claude
// provider. The provider's response is the documented Anthropic SSE
// event stream (message_start / content_block_* / message_delta /
// message_stop), and the router reassembles it into the canonical
// ChatChunk stream + StreamResult exactly as the other translated
// pairs do for their providers.
//
// Both wires involved here are native wires, so the SSE parser is
// already owned by internal/wire (wire.TranslateStream) — this
// function is the PAIR's contract surface and delegates rather than
// duplicating a parser, so the pair can never drift from the wire's
// own streaming behavior.
func TranslateGeminiToClaudeStream(r io.Reader, emit func(wire.ChatChunk) error) (wire.StreamResult, error) {
	return wire.TranslateStream(r, emit)
}
