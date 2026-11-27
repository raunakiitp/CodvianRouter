package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertKiroRequestRoundTrip translates an OpenAI conversation
// into the generateAssistantResponse shape, re-encodes it through JSON,
// and maps a Kiro assistant turn answering that request back to OpenAI,
// asserting the assistant turn round-trips semantically.
func TestConvertKiroRequestRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "claude-sonnet-4-6",
		Messages: []wire.ChatMessage{
			{Role: "system", Content: mustJSON("be brief")},
			{Role: "user", Content: mustJSON("hello")},
			{Role: "assistant", Content: mustJSON("hi there")},
			{Role: "user", Content: mustJSON("what is 2+2?")},
		},
	}
	out, err := ConvertKiroRequest(req, "11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("ConvertKiroRequest: %v", err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal KiroRequest: %v", err)
	}
	var decoded KiroRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal KiroRequest: %v\nbody: %s", err, raw)
	}
	cs := decoded.ConversationState
	if cs.ConversationID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("conversationId = %q", cs.ConversationID)
	}
	if cs.ChatTriggerType != "MANUAL" {
		t.Errorf("chatTriggerType = %q", cs.ChatTriggerType)
	}
	cur := cs.CurrentMessage.UserInputMessage
	if cur.Content != "what is 2+2?" || cur.ModelID != "claude-sonnet-4-6" {
		t.Errorf("currentMessage = %+v", cur)
	}
	// history = one user/assistant pair: {user: "be brief\n\nhello",
	// assistant: "hi there"} — the system prompt is folded into the
	// first user-turn content.
	if len(cs.History) != 1 {
		t.Fatalf("history = %d entries: %s", len(cs.History), raw)
	}
	first := cs.History[0].UserInputMessage
	if first == nil || !strings.Contains(first.Content, "be brief") || !strings.Contains(first.Content, "hello") {
		t.Errorf("first history entry = %+v", first)
	}
	second := cs.History[0].AssistantResponseMessage
	if second == nil || len(second.Content) != 1 || second.Content[0].Text != "hi there" {
		t.Errorf("assistant history entry = %+v", second)
	}

	// The provider answers with one text block; it maps back to OpenAI.
	respMsg := &KiroAssistantResponseMessage{
		Content: []KiroAssistantContentBlock{{Text: "4"}},
	}
	back, err := ConvertKiroResponse(respMsg, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ConvertKiroResponse: %v", err)
	}
	var reply string
	if err := json.Unmarshal(back.Choices[0].Message.Content, &reply); err != nil || reply != "4" {
		t.Errorf("reply = %s (err %v)", back.Choices[0].Message.Content, err)
	}
	if back.Choices[0].Message.Role != "assistant" || back.Choices[0].FinishReason != "stop" {
		t.Errorf("role/finish = %s/%s", back.Choices[0].Message.Role, back.Choices[0].FinishReason)
	}
	if back.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", back.Model)
	}
}

// TestConvertKiroRequestToolsRoundTrip covers tool definitions, an
// assistant toolUse turn, a folded tool result, and a toolUse response
// mapping back to OpenAI tool calls.
func TestConvertKiroRequestToolsRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "claude-sonnet-4-6",
		Messages: []wire.ChatMessage{
			{Role: "user", Content: mustJSON("read the readme")},
			{
				Role: "assistant",
				ToolCalls: []wire.ToolCall{{
					ID:   "toolu_1",
					Type: "function",
					Function: wire.ToolCallFunction{
						Name:      "read_file",
						Arguments: `{"path":"README.md"}`,
					},
				}},
			},
			{Role: "tool", ToolCallID: "toolu_1", Content: mustJSON("# Project")},
			{Role: "user", Content: mustJSON("summarise it")},
		},
		Tools: []wire.ToolDefinition{namedTool("read_file")},
	}
	out, err := ConvertKiroRequest(req, "conv-1")
	if err != nil {
		t.Fatalf("ConvertKiroRequest: %v", err)
	}
	cur := out.ConversationState.CurrentMessage.UserInputMessage
	// The trailing tool result folds into the current message's
	// content, ahead of the user text, in message order.
	if !strings.Contains(cur.Content, "Tool result") || !strings.Contains(cur.Content, "toolu_1") ||
		!strings.Contains(cur.Content, "summarise it") ||
		strings.Index(cur.Content, "Tool result") > strings.Index(cur.Content, "summarise it") {
		t.Errorf("currentMessage.content = %q", cur.Content)
	}
	// Tool definitions ride in the current message's context.
	if cur.UserInputMessageContext == nil || len(cur.UserInputMessageContext.Tools) != 1 {
		t.Fatalf("tools context = %+v", cur.UserInputMessageContext)
	}
	spec := cur.UserInputMessageContext.Tools[0].ToolSpec
	if spec.Name != "read_file" || spec.InputSchema.JSON == nil {
		t.Errorf("toolSpec = %+v", spec)
	}
	// History: one pair — user "read the readme" + assistant toolUse.
	h := out.ConversationState.History
	if len(h) != 1 {
		t.Fatalf("history = %d entries", len(h))
	}
	au := h[0].AssistantResponseMessage
	if au == nil || len(au.Content) != 1 || au.Content[0].ToolUse == nil {
		t.Fatalf("assistant history entry = %+v", au)
	}
	tu := au.Content[0].ToolUse
	if tu.ToolUseID != "toolu_1" || tu.Name != "read_file" {
		t.Errorf("toolUse = %+v", tu)
	}
	var inputObj map[string]any
	if err := json.Unmarshal(tu.Input, &inputObj); err != nil || inputObj["path"] != "README.md" {
		t.Errorf("toolUse input = %s (err %v)", tu.Input, err)
	}

	// The provider answers with a toolUse; it maps back to OpenAI.
	respMsg := &KiroAssistantResponseMessage{
		Content: []KiroAssistantContentBlock{{
			ToolUse: &KiroToolUse{ToolUseID: "toolu_9", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
		}},
	}
	back, err := ConvertKiroResponse(respMsg, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ConvertKiroResponse: %v", err)
	}
	tcs := back.Choices[0].Message.ToolCalls
	if len(tcs) != 1 || tcs[0].ID != "toolu_9" || tcs[0].Function.Name != "read_file" {
		t.Fatalf("tool calls = %+v", tcs)
	}
	if tcs[0].Function.Arguments != `{"path":"README.md"}` {
		t.Errorf("arguments = %s", tcs[0].Function.Arguments)
	}
}

func TestConvertKiroRequestGeneratedConversationID(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
	}
	out, err := ConvertKiroRequest(req, "")
	if err != nil {
		t.Fatalf("ConvertKiroRequest: %v", err)
	}
	id := out.ConversationState.ConversationID
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("generated conversationId = %q", id)
	}
}

func TestConvertKiroRequestRefusals(t *testing.T) {
	t.Run("no trailing user turn", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:    "claude-sonnet-4-6",
			Messages: []wire.ChatMessage{{Role: "assistant", Content: mustJSON("hi")}},
		}
		if _, err := ConvertKiroRequest(req, "c"); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("empty model", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model:    "",
			Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("hi")}},
		}
		if _, err := ConvertKiroRequest(req, "c"); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
	t.Run("invalid tool arguments", func(t *testing.T) {
		req := wire.ChatCompletionRequest{
			Model: "claude-sonnet-4-6",
			Messages: []wire.ChatMessage{
				{Role: "user", Content: mustJSON("hi")},
				{Role: "assistant", ToolCalls: []wire.ToolCall{{
					ID: "t", Type: "function",
					Function: wire.ToolCallFunction{Name: "f", Arguments: `{not json`},
				}}},
				{Role: "user", Content: mustJSON("go on")},
			},
		}
		if _, err := ConvertKiroRequest(req, "c"); !IsUnrepresentable(err) {
			t.Errorf("err = %v, want unrepresentable", err)
		}
	})
}

// TestTranslateKiroStream feeds assistantResponseEvent fragments and
// asserts the emitted OpenAI chunk sequence reassembles.
func TestTranslateKiroStream(t *testing.T) {
	stream := "data: {\"assistantResponseEvent\": {\"content\": \"Hel\"}}\n\n" +
		"data: {\"assistantResponseEvent\": {\"content\": \"lo\"}}\n\n" +
		": keep-alive comment\n\n" +
		"data: {\"assistantResponseEvent\": {\"content\": \" world\"}}\n\n"

	var chunks []wire.ChatChunk
	res, err := TranslateKiroStream(strings.NewReader(stream), "claude-sonnet-4-6", func(c wire.ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateKiroStream: %v", err)
	}
	if res.Content != "Hello world" || res.FinishReason != "stop" {
		t.Fatalf("content/finish = %q/%q", res.Content, res.FinishReason)
	}
	if res.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", res.Model)
	}
	// Three content deltas + one final chunk; SSE comments skipped.
	if len(chunks) != 4 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	got := chunks[0].Choices[0].Delta.Content + chunks[1].Choices[0].Delta.Content + chunks[2].Choices[0].Delta.Content
	if got != "Hello world" {
		t.Errorf("reassembled deltas = %q", got)
	}
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v", final.Choices[0].FinishReason)
	}
}

func TestTranslateKiroStreamEmpty(t *testing.T) {
	if _, err := TranslateKiroStream(strings.NewReader(""), "m", nil); err == nil {
		t.Errorf("empty stream returned no error")
	}
}

func TestTranslateKiroStreamErrorEvent(t *testing.T) {
	stream := "data: {\"assistantResponseEvent\": {\"content\": \"hi\"}}\n\n" +
		"data: {\"error\": {\"code\": \"ThrottlingException\", \"message\": \"rate exceeded\"}}\n\n"
	_, err := TranslateKiroStream(strings.NewReader(stream), "m", nil)
	var ke *KiroError
	if err == nil || !strings.Contains(err.Error(), "rate exceeded") {
		t.Fatalf("err = %v, want KiroError", err)
	}
	if _, ok := err.(*KiroError); ok {
		ke = err.(*KiroError)
		if ke.Message != "rate exceeded" {
			t.Errorf("message = %q", ke.Message)
		}
	}
}
