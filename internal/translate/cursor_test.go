package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// TestConvertCursorCreateRequestRoundTrip translates an OpenAI
// conversation into a create-agent body, re-encodes it through JSON,
// and maps the run's terminal result back to OpenAI, asserting the
// assistant turn round-trips semantically.
func TestConvertCursorCreateRequestRoundTrip(t *testing.T) {
	req := wire.ChatCompletionRequest{
		Model: "composer-2",
		Messages: []wire.ChatMessage{
			{Role: "system", Content: mustJSON("be brief")},
			{Role: "user", Content: mustJSON("add a readme")},
			{Role: "assistant", Content: mustJSON("done, added README.md")},
			{Role: "user", Content: mustJSON("now list the sections")},
		},
	}
	out, err := ConvertCursorCreateRequest(req)
	if err != nil {
		t.Fatalf("ConvertCursorCreateRequest: %v", err)
	}

	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded CursorAgentCreateRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, raw)
	}
	if decoded.Model == nil || decoded.Model.ID != "composer-2" {
		t.Errorf("model = %+v", decoded.Model)
	}
	if decoded.Prompt.Text == "" {
		t.Fatalf("prompt.text is empty")
	}
	// The whole conversation is rendered into the prompt, in order.
	want := []string{"[System]", "be brief", "User:", "add a readme", "Assistant:", "done, added README.md", "User:", "now list the sections"}
	pos := -1
	for _, w := range want {
		idx := strings.Index(decoded.Prompt.Text[pos+1:], w)
		if idx == -1 {
			t.Fatalf("prompt text missing %q in order: %q", w, decoded.Prompt.Text)
		}
		pos += idx + 1
	}
	// Sampling parameters have no field on this surface and must not
	// be invented.
	if strings.Contains(string(raw), "temperature") || strings.Contains(string(raw), "max_tokens") {
		t.Errorf("sampling params leaked: %s", raw)
	}

	// The run finishes; the result maps back to OpenAI.
	result := &CursorResultEvent{
		RunID:  "run-00000000-0000-0000-0000-000000000001",
		Status: "FINISHED",
		Text:   "README.md has Installation, Usage, and License.",
	}
	back, err := ConvertCursorRunResult(result, "composer-2")
	if err != nil {
		t.Fatalf("ConvertCursorRunResult: %v", err)
	}
	var reply string
	if err := json.Unmarshal(back.Choices[0].Message.Content, &reply); err != nil || reply != "README.md has Installation, Usage, and License." {
		t.Errorf("reply = %s (err %v)", back.Choices[0].Message.Content, err)
	}
	if back.Choices[0].Message.Role != "assistant" || back.Choices[0].FinishReason != "stop" {
		t.Errorf("role/finish = %s/%s", back.Choices[0].Message.Role, back.Choices[0].FinishReason)
	}
	if back.ID != "run-00000000-0000-0000-0000-000000000001" {
		t.Errorf("id = %q", back.ID)
	}
	if back.Object != "chat.completion" {
		t.Errorf("object = %q", back.Object)
	}
}

func TestConvertCursorFollowupRequest(t *testing.T) {
	// A follow-up carries only the new user turn.
	req := wire.ChatCompletionRequest{
		Model:    "composer-2",
		Messages: []wire.ChatMessage{{Role: "user", Content: mustJSON("also add a license section")}},
	}
	out, err := ConvertCursorFollowupRequest(req)
	if err != nil {
		t.Fatalf("ConvertCursorFollowupRequest: %v", err)
	}
	if out.Prompt.Text != "also add a license section" {
		t.Errorf("prompt.text = %q", out.Prompt.Text)
	}
	raw, _ := json.Marshal(out)
	var decoded CursorAgentFollowupRequest
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Prompt.Text != out.Prompt.Text {
		t.Errorf("json round trip failed: %s", raw)
	}

	// Prior messages are refused: the agent already holds them.
	withHistory := wire.ChatCompletionRequest{
		Model:    "composer-2",
		Messages: []wire.ChatMessage{{Role: "assistant", Content: mustJSON("hi")}, {Role: "user", Content: mustJSON("go on")}},
	}
	if _, err := ConvertCursorFollowupRequest(withHistory); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
	// A non-user final message is refused.
	notUser := wire.ChatCompletionRequest{
		Model:    "composer-2",
		Messages: []wire.ChatMessage{{Role: "assistant", Content: mustJSON("hi")}},
	}
	if _, err := ConvertCursorFollowupRequest(notUser); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
}

func TestConvertCursorRequestRefusals(t *testing.T) {
	// Tool calls and tool results have no replay on this surface.
	req := wire.ChatCompletionRequest{
		Model: "composer-2",
		Messages: []wire.ChatMessage{
			{Role: "user", Content: mustJSON("go on")},
			{Role: "assistant", ToolCalls: []wire.ToolCall{{
				ID: "t", Type: "function",
				Function: wire.ToolCallFunction{Name: "f", Arguments: "{}"},
			}}},
			{Role: "user", Content: mustJSON("thanks")},
		},
	}
	if _, err := ConvertCursorCreateRequest(req); !IsUnrepresentable(err) {
		t.Errorf("err = %v, want unrepresentable", err)
	}
	// A non-FINISHED run result is refused.
	result := &CursorResultEvent{Status: "ERROR", Text: ""}
	if _, err := ConvertCursorRunResult(result, "composer-2"); err == nil {
		t.Errorf("non-FINISHED result returned no error")
	}
}

// TestTranslateCursorStream feeds the documented example stream
// (status, assistant deltas, tool_call running/completed, result,
// done) and asserts the emitted OpenAI chunk sequence reassembles.
func TestTranslateCursorStream(t *testing.T) {
	stream := "event: status\n" +
		`data: {"runId":"run-1","status":"RUNNING"}` + "\n\n" +
		"id: 1713033000000-0\n" +
		"event: assistant\n" +
		`data: {"text":"Working on it."}` + "\n\n" +
		"event: tool_call\n" +
		`data: {"callId":"call-1","name":"read_file","status":"running","args":{"path":"README.md"}}` + "\n\n" +
		"event: tool_call\n" +
		`data: {"callId":"call-1","name":"read_file","status":"completed","args":{"path":"README.md"},"result":{"success":{"content":"# Project"}}}` + "\n\n" +
		"event: thinking\n" +
		`data: {"text":"hidden reasoning"}` + "\n\n" +
		"event: heartbeat\n" +
		"data: {}" + "\n\n" +
		"event: result\n" +
		`data: {"runId":"run-1","status":"FINISHED","text":"Added README.md with installation instructions.","durationMs":12357}` + "\n\n" +
		"event: done\n" +
		"data: {}" + "\n\n"

	var chunks []wire.ChatChunk
	res, err := TranslateCursorStream(strings.NewReader(stream), "composer-2", func(c wire.ChatChunk) error {
		chunks = append(chunks, c)
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateCursorStream: %v", err)
	}
	// The result event's text is the authoritative final reply.
	if res.Content != "Added README.md with installation instructions." {
		t.Fatalf("content = %q", res.Content)
	}
	if res.FinishReason != "stop" || res.Model != "composer-2" {
		t.Fatalf("finish/model = %q/%q", res.FinishReason, res.Model)
	}
	if res.ID != "run-1" {
		t.Errorf("id = %q", res.ID)
	}
	// assistant delta + tool_call delta + final chunk; status,
	// thinking, completed tool_call, heartbeat and done emit nothing.
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	if chunks[0].Choices[0].Delta.Content != "Working on it." {
		t.Errorf("content delta = %q", chunks[0].Choices[0].Delta.Content)
	}
	tc := chunks[1].Choices[0].Delta.ToolCalls
	if len(tc) != 1 || tc[0].ID != "call-1" || tc[0].Function.Name != "read_file" {
		t.Fatalf("tool call delta = %+v", tc)
	}
	var argsObj map[string]any
	if err := json.Unmarshal([]byte(tc[0].Function.Arguments), &argsObj); err != nil || argsObj["path"] != "README.md" {
		t.Errorf("tool call args = %q (err %v)", tc[0].Function.Arguments, err)
	}
	final := chunks[len(chunks)-1]
	if final.Choices[0].FinishReason == nil || *final.Choices[0].FinishReason != "stop" {
		t.Errorf("final finish_reason = %v", final.Choices[0].FinishReason)
	}
}

func TestTranslateCursorStreamEmpty(t *testing.T) {
	if _, err := TranslateCursorStream(strings.NewReader(""), "composer-2", nil); err == nil {
		t.Errorf("empty stream returned no error")
	}
}

func TestTranslateCursorStreamErrorEvent(t *testing.T) {
	stream := "event: error\n" +
		`data: {"code":"stream_expired","message":"retention elapsed"}` + "\n\n"
	_, err := TranslateCursorStream(strings.NewReader(stream), "composer-2", nil)
	var ce *CursorError
	if !errors.As(err, &ce) || ce.Code != "stream_expired" {
		t.Fatalf("err = %v, want *CursorError stream_expired", err)
	}
}
