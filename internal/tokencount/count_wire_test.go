package tokencount_test

import (
	"encoding/json"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Regression tests for the token-count accuracy audit fix
// (CHECKLIST 229-230): CountPromptWire must count tool definitions
// and per-message framing, not just message text. The exact
// small-case numbers below are hand-verifiable against the OpenAI
// cookbook rule (3 tokens per message + 3 reply priming) and the
// audit corpus; the end-to-end drift bounds live in
// internal/router/token_audit_test.go.

func wireReq(messages []provider.Message, tools []provider.ToolDefinition) provider.ChatRequest {
	return provider.ChatRequest{Model: "gpt-4o-mini", Messages: messages, Tools: tools}
}

func TestCountPromptWireOpenAIFraming(t *testing.T) {
	// eng-short: system + user, message text counts 13 tokens
	// (CountPrompt). OpenAI framing adds 3 per message + 3
	// priming = 9, so the wire count is 22 exactly.
	corpus := tokencount.Corpus()
	var engShort *tokencount.CorpusCase
	for i := range corpus {
		if corpus[i].ID == "eng-short" {
			engShort = &corpus[i]
		}
	}
	if engShort == nil {
		t.Fatal("no eng-short case")
	}
	base, err := tokencount.CountPrompt("gpt-4o-mini", engShort.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if base.Tokens != 13 {
		t.Fatalf("CountPrompt eng-short = %d, want 13 (corpus changed?)", base.Tokens)
	}
	got, err := tokencount.CountPromptWire("gpt-4o-mini", "openai", wireReq(engShort.Messages, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens != 22 {
		t.Errorf("CountPromptWire openai eng-short = %d, want 22 (13 text + 3x2 framing + 3 priming)", got.Tokens)
	}
	if got.Encoding != base.Encoding || got.Fallback != base.Fallback {
		t.Errorf("wire count changed encoding/fallback: %+v vs %+v", got, base)
	}
	// A single-message prompt: 3 + 3 = 6 framing tokens.
	one := []provider.Message{{Role: "user", Content: "hi"}}
	oneBase, _ := tokencount.CountPrompt("gpt-4o-mini", one)
	oneWire, err := tokencount.CountPromptWire("gpt-4o-mini", "openai", wireReq(one, nil))
	if err != nil {
		t.Fatal(err)
	}
	if oneWire.Tokens != oneBase.Tokens+6 {
		t.Errorf("single message: wire %d, want base %d + 6", oneWire.Tokens, oneBase.Tokens)
	}
	// Unknown wire ("") takes the OpenAI reference framing — the
	// conservative choice for a gate that does not know the wire.
	unknown, err := tokencount.CountPromptWire("gpt-4o-mini", "", wireReq(engShort.Messages, nil))
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Tokens != 22 {
		t.Errorf("unknown wire eng-short = %d, want 22 (OpenAI reference)", unknown.Tokens)
	}
}

func TestCountPromptWireNativeFraming(t *testing.T) {
	// Native wires carry one role-label token per message, not
	// the OpenAI 3-per-message rule: eng-short (2 messages) is
	// base 13 + 2 = 15 on Anthropic/Gemini/Cohere/Bedrock.
	corpus := tokencount.Corpus()
	var engShort *tokencount.CorpusCase
	for i := range corpus {
		if corpus[i].ID == "eng-short" {
			engShort = &corpus[i]
		}
	}
	for _, wire := range []string{provider.WireFormatAnthropic, provider.WireFormatGemini, provider.WireFormatCohere, provider.WireFormatBedrock} {
		got, err := tokencount.CountPromptWire("claude-sonnet-4-5", wire, provider.ChatRequest{Model: "claude-sonnet-4-5", Messages: engShort.Messages})
		if err != nil {
			t.Fatal(err)
		}
		if got.Tokens != 15 {
			t.Errorf("wire %s eng-short = %d, want 15 (13 text + 1 per message)", wire, got.Tokens)
		}
	}
}

func TestCountPromptWireCountsTools(t *testing.T) {
	// tool-single: the tool definition must be counted. The wire
	// count is the message-text count + the BPE count of the
	// tools-array JSON + framing — never the message text alone.
	corpus := tokencount.Corpus()
	var toolSingle *tokencount.CorpusCase
	for i := range corpus {
		if corpus[i].ID == "tool-single" {
			toolSingle = &corpus[i]
		}
	}
	if toolSingle == nil {
		t.Fatal("no tool-single case")
	}
	base, err := tokencount.CountPrompt("gpt-4o-mini", toolSingle.Messages)
	if err != nil {
		t.Fatal(err)
	}
	toolsJSON, err := json.Marshal(toolSingle.Tools)
	if err != nil {
		t.Fatal(err)
	}
	toolsCount, err := tokencount.Count("gpt-4o-mini", string(toolsJSON))
	if err != nil {
		t.Fatal(err)
	}
	if toolsCount.Tokens <= 0 {
		t.Fatal("tools JSON counts 0 — corpus broken")
	}
	got, err := tokencount.CountPromptWire("gpt-4o-mini", "openai", wireReq(toolSingle.Messages, toolSingle.Tools))
	if err != nil {
		t.Fatal(err)
	}
	// 1 message: framing 3 + 3 = 6.
	want := base.Tokens + toolsCount.Tokens + 6
	if got.Tokens != want {
		t.Errorf("CountPromptWire openai tool-single = %d, want %d (base %d + tools %d + framing 6)", got.Tokens, want, base.Tokens, toolsCount.Tokens)
	}
	// The old behaviour (message text only) would have returned
	// base.Tokens — assert the fix actually moved the number.
	if got.Tokens == base.Tokens {
		t.Errorf("wire count equals message-text count — tools not counted")
	}
}

func TestCountPromptWireBedrockReshaping(t *testing.T) {
	// Bedrock wraps tools in toolSpec + inputSchema.json and adds
	// toolChoice {"auto":{}}: the Bedrock count of a tool-heavy
	// request must exceed the OpenAI-shape tools count, not equal
	// it — and an explicit tool_choice "none" (which omits
	// toolChoice on the wire) must count less than the auto shape.
	corpus := tokencount.Corpus()
	var toolSingle *tokencount.CorpusCase
	for i := range corpus {
		if corpus[i].ID == "tool-single" {
			toolSingle = &corpus[i]
		}
	}
	// Same model (hence same encoding) on both sides, so the
	// comparison isolates the tools reshaping from the encoding.
	// The message text counts 10 tokens under this model.
	model := "anthropic.claude-sonnet-4-5-20250929-v1:0"
	openai, err := tokencount.CountPromptWire(model, provider.WireFormatOpenAI, provider.ChatRequest{
		Model: model, Messages: toolSingle.Messages, Tools: toolSingle.Tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	bedrock, err := tokencount.CountPromptWire(model, provider.WireFormatBedrock, provider.ChatRequest{
		Model: model, Messages: toolSingle.Messages, Tools: toolSingle.Tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bedrock framing is +1/message vs OpenAI's +6, so compare
	// the tools terms, not the totals: strip text + framing.
	openaiTools := openai.Tokens - 10 - 6   // text 10, framing 6
	bedrockTools := bedrock.Tokens - 10 - 1 // text 10, framing 1
	if bedrockTools <= openaiTools {
		t.Errorf("bedrock tools term %d, want > openai tools term %d (toolSpec/inputSchema/toolChoice wrappers)", bedrockTools, openaiTools)
	}
	noneReq := provider.ChatRequest{
		Model: model, Messages: toolSingle.Messages,
		Tools: toolSingle.Tools, ToolChoice: json.RawMessage(`"none"`),
	}
	noneCount, err := tokencount.CountPromptWire("anthropic.claude-sonnet-4-5-20250929-v1:0", provider.WireFormatBedrock, noneReq)
	if err != nil {
		t.Fatal(err)
	}
	if noneCount.Tokens >= bedrock.Tokens {
		t.Errorf("tool_choice none = %d, want < auto shape %d (toolChoice omitted on the wire)", noneCount.Tokens, bedrock.Tokens)
	}
}

func TestCountPromptFullIsOpenAIReference(t *testing.T) {
	corpus := tokencount.Corpus()
	for _, c := range corpus {
		full, err := tokencount.CountPromptFull("gpt-4o-mini", wireReq(c.Messages, c.Tools))
		if err != nil {
			t.Fatal(err)
		}
		wire, err := tokencount.CountPromptWire("gpt-4o-mini", "openai", wireReq(c.Messages, c.Tools))
		if err != nil {
			t.Fatal(err)
		}
		if full.Tokens != wire.Tokens {
			t.Errorf("case %s: CountPromptFull %d != CountPromptWire(openai) %d", c.ID, full.Tokens, wire.Tokens)
		}
		// The full count never undercounts the message text.
		base, _ := tokencount.CountPrompt("gpt-4o-mini", c.Messages)
		if full.Tokens < base.Tokens {
			t.Errorf("case %s: full count %d < message-text count %d", c.ID, full.Tokens, base.Tokens)
		}
	}
}
