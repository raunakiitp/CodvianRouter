package relay

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

func TestDecideVerbatimOnlyOnDialectMatchAndNoFeatures(t *testing.T) {
	if v, _ := Decide("anthropic", "anthropic", Features{}); !v {
		t.Error("matching dialect with no features should relay verbatim")
	}
	cases := []struct {
		name    string
		dialect string
		wire    string
		f       Features
	}{
		{"dialect mismatch", "anthropic", "openai", Features{}},
		{"guardrails", "anthropic", "anthropic", Features{GuardrailsBlocking: true}},
		{"cache", "anthropic", "anthropic", Features{CacheServing: true}},
		{"failover", "anthropic", "anthropic", Features{MultiAttempt: true}},
		{"forced reasoning", "anthropic", "anthropic", Features{ReasoningForced: true}},
	}
	for _, c := range cases {
		if v, reason := Decide(c.dialect, c.wire, c.f); v {
			t.Errorf("%s: got verbatim, want translate", c.name)
		} else if reason == "" {
			t.Errorf("%s: empty reason", c.name)
		}
	}
	if _, reason := Decide("anthropic", "openai", Features{}); !strings.Contains(reason, "anthropic") || !strings.Contains(reason, "openai") {
		t.Errorf("mismatch reason should name both dialects, got %q", reason)
	}
}

func TestRewriteModelBytePreserving(t *testing.T) {
	// Deliberately odd formatting, unknown fields, a nested "model"
	// key, and a cache_control block: only the top-level model value
	// may change; every other byte must be identical.
	raw := []byte("{\n  \"model\" : \"claude-old\",  \"max_tokens\": 5,\n  \"messages\": [{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"hi\", \"cache_control\": {\"type\": \"ephemeral\"}}, {\"type\": \"text\", \"text\": \"model\"}]}],\n  \"nested\": {\"model\": \"must-not-change\"},\n  \"weird\": \"model\"\n}")
	out, err := RewriteModel(raw, "claude-new")
	if err != nil {
		t.Fatalf("RewriteModel: %v", err)
	}
	want := []byte("{\n  \"model\" : \"claude-new\",  \"max_tokens\": 5,\n  \"messages\": [{\"role\": \"user\", \"content\": [{\"type\": \"text\", \"text\": \"hi\", \"cache_control\": {\"type\": \"ephemeral\"}}, {\"type\": \"text\", \"text\": \"model\"}]}],\n  \"nested\": {\"model\": \"must-not-change\"},\n  \"weird\": \"model\"\n}")
	if !bytes.Equal(out, want) {
		t.Errorf("rewritten body differs outside the model value:\n got %s\nwant %s", out, want)
	}
	// cache_control survived the rewrite untouched.
	if !bytes.Contains(out, []byte(`"cache_control": {"type": "ephemeral"}`)) {
		t.Error("cache_control was not preserved byte-for-byte")
	}
}

func TestRewriteModelEdgeCases(t *testing.T) {
	// Model key last, no trailing comma.
	out, err := RewriteModel([]byte(`{"messages":[],"model":"a"}`), "b")
	if err != nil || string(out) != `{"messages":[],"model":"b"}` {
		t.Errorf("model-last rewrite = %q, err %v", out, err)
	}
	// Escaped characters in the old model value are consumed, not copied.
	out, err = RewriteModel([]byte(`{"model":"a\"b"}`), "c")
	if err != nil || string(out) != `{"model":"c"}` {
		t.Errorf("escaped model rewrite = %q, err %v", out, err)
	}
	// Model value with characters that need JSON escaping in the replacement.
	out, err = RewriteModel([]byte(`{"model":"a"}`), `x"y`)
	if err != nil || string(out) != `{"model":"x\"y"}` {
		t.Errorf("escaping rewrite = %q, err %v", out, err)
	}
	for _, bad := range []string{
		``,
		`not json`,
		`[1,2]`,
		`{"messages":[]}`,
		`{"model":42}`,
		`{"model":null}`,
		`{"model":"a"`,
	} {
		if _, err := RewriteModel([]byte(bad), "b"); err == nil {
			t.Errorf("RewriteModel(%q) should fail", bad)
		}
	}
}

func TestParseAnthropicRoundTrip(t *testing.T) {
	raw := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"system":"You are helpful.","messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]},{"role":"assistant","content":[{"type":"text","text":"Hi"},{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Pune"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}]} ],"tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"tool_choice":{"type":"tool","name":"get_weather"}}`)
	req, err := ParseAnthropic(raw)
	if err != nil {
		t.Fatalf("ParseAnthropic: %v", err)
	}
	if req.Model != "claude-sonnet-4-5" || req.MaxTokens != 64 {
		t.Errorf("model/max_tokens = %q/%d", req.Model, req.MaxTokens)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 (system + 3)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", req.Messages[0].Role)
	}
	if len(req.Messages[2].ToolCalls) != 1 || req.Messages[2].ToolCalls[0].Function.Name != "get_weather" {
		t.Errorf("assistant tool call not converted: %+v", req.Messages[2].ToolCalls)
	}
	if req.Messages[3].Role != "tool" || req.Messages[3].ToolCallID != "toolu_1" {
		t.Errorf("tool result not converted: role=%q id=%q", req.Messages[3].Role, req.Messages[3].ToolCallID)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tools not converted: %+v", req.Tools)
	}
}

func TestParseAnthropicCacheControlCountedNotHidden(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	res, err := ParseAnthropicDetailed(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed: %v", err)
	}
	if res.DroppedCacheControl != 2 {
		t.Errorf("DroppedCacheControl = %d, want 2", res.DroppedCacheControl)
	}
	if len(res.Request.Messages) == 0 || res.Request.Messages[0].Role != "system" {
		t.Errorf("system block lost in translation: %+v", res.Request.Messages)
	}
}

func TestParseAnthropicRejectsThinking(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"thinking":{"type":"enabled","budget_tokens":1000},"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := ParseAnthropic(raw); err == nil {
		t.Error("thinking block should be rejected on the translate path, not silently dropped")
	}
	raw2 := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}]}`)
	if _, err := ParseAnthropic(raw2); err == nil {
		t.Error("thinking content block should be rejected on the translate path")
	}
}

func TestToAnthropicResponse(t *testing.T) {
	resp := &provider.ChatResponse{
		Content: "Hello", Model: "claude-sonnet-4-5", FinishReason: "stop",
		Usage:     provider.Usage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13, CachedTokens: 4},
		ToolCalls: []provider.ToolCall{{ID: "toolu_9", Type: "function", Function: wire.ToolCallFunction{Name: "f", Arguments: `{"a":1}`}}},
	}
	b, err := ToAnthropicResponse(resp)
	if err != nil {
		t.Fatalf("ToAnthropicResponse: %v", err)
	}
	var got struct {
		Type       string `json:"type"`
		Role       string `json:"role"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
			Cached int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if got.Type != "message" || got.Role != "assistant" || got.StopReason != "end_turn" {
		t.Errorf("envelope wrong: %+v", got)
	}
	if len(got.Content) != 2 || got.Content[0].Text != "Hello" || got.Content[1].Name != "f" {
		t.Errorf("content blocks wrong: %+v", got.Content)
	}
	if got.Usage.Input != 10 || got.Usage.Output != 3 || got.Usage.Cached != 4 {
		t.Errorf("usage wrong: %+v", got.Usage)
	}
	if MapFinishReasonToAnthropic("tool_calls") != "tool_use" || MapFinishReasonToAnthropic("length") != "max_tokens" {
		t.Error("finish-reason mapping wrong")
	}
}

func TestAnthropicStreamEmitterSequence(t *testing.T) {
	var buf bytes.Buffer
	em, err := NewAnthropicStreamEmitter(&buf, "msg_test1", "claude-sonnet-4-5")
	if err != nil {
		t.Fatal(err)
	}
	if err := em.Start(); err != nil {
		t.Fatal(err)
	}
	if err := em.TextDelta("Hel"); err != nil {
		t.Fatal(err)
	}
	if err := em.TextDelta("lo"); err != nil {
		t.Fatal(err)
	}
	if err := em.ToolUseBlock("toolu_1", "get_weather", `{"city":"Pune"}`); err != nil {
		t.Fatal(err)
	}
	if err := em.Finish("tool_use", provider.Usage{PromptTokens: 20, CompletionTokens: 5, TotalTokens: 25}); err != nil {
		t.Fatal(err)
	}
	// Every data line must be valid JSON; event order must be the
	// Anthropic envelope sequence.
	var events []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			var v map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Fatalf("data line is not valid JSON: %q: %v", line, err)
			}
		}
	}
	want := []string{"message_start", "content_block_start", "content_block_delta", "content_block_delta", "content_block_stop", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
	// message_start must not invent usage: no usage key at all.
	if strings.Contains(buf.String(), `"input_tokens":0`) {
		t.Error("message_start emitted invented zero usage")
	}
	if !strings.Contains(buf.String(), `"input_tokens":20`) {
		t.Error("message_delta does not carry the true usage")
	}
	if !strings.Contains(buf.String(), `"partial_json":"{\"city\":\"Pune\"}"`) {
		t.Error("tool_use input JSON not emitted as input_json_delta")
	}
}

// Routing contract on /v1/messages (CHECKLIST 211): the canonical
// "routing" field is parsed into ParseResult.Routing, a non-empty
// contract forces translation (the verbatim path stays
// contract-free by construction), and a malformed contract fails
// closed with the identical message chat completions gives.

func TestParseAnthropicRoutingContract(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"routing":{"order":["b","a"],"only":["a","b"],"sort":"price","max_price":{"input":1.5,"output":3},"data_collection":"deny","group":"eu"}}`)
	res, err := ParseAnthropicDetailed(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed: %v", err)
	}
	if res.Routing == nil {
		t.Fatal("Routing is nil, want the parsed contract")
	}
	r := res.Routing
	if len(r.Order) != 2 || r.Order[0] != "b" || r.Order[1] != "a" {
		t.Errorf("order = %v, want [b a]", r.Order)
	}
	if len(r.Only) != 2 || r.Only[0] != "a" {
		t.Errorf("only = %v, want [a b]", r.Only)
	}
	if r.Sort != "price" {
		t.Errorf("sort = %q, want price", r.Sort)
	}
	if r.MaxPrice == nil || r.MaxPrice.Input == nil || *r.MaxPrice.Input != 1.5 || r.MaxPrice.Output == nil || *r.MaxPrice.Output != 3 {
		t.Errorf("max_price = %+v, want input 1.5 / output 3", r.MaxPrice)
	}
	if r.DataCollection != "deny" {
		t.Errorf("data_collection = %q, want deny", r.DataCollection)
	}
	if r.Group != "eu" {
		t.Errorf("group = %q, want eu", r.Group)
	}
	if r.Empty() {
		t.Error("a full contract reports Empty, want non-empty")
	}
}

func TestParseAnthropicRoutingAbsentAndEmpty(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	res, err := ParseAnthropicDetailed(raw)
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed: %v", err)
	}
	if res.Routing != nil {
		t.Errorf("Routing = %+v, want nil when the field is absent", res.Routing)
	}
	rawEmpty := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"routing":{}}`)
	res, err = ParseAnthropicDetailed(rawEmpty)
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed (empty routing): %v", err)
	}
	if res.Routing == nil || !res.Routing.Empty() {
		t.Errorf("Routing = %+v, want a present-but-empty contract (same as no contract)", res.Routing)
	}
	rawNull := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"routing":null}`)
	res, err = ParseAnthropicDetailed(rawNull)
	if err != nil {
		t.Fatalf("ParseAnthropicDetailed (null routing): %v", err)
	}
	if res.Routing != nil {
		t.Errorf("Routing = %+v, want nil for an explicit null", res.Routing)
	}
}

func TestParseAnthropicRoutingUnknownFieldFailsClosed(t *testing.T) {
	raw := []byte(`{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"routing":{"excludes":["a"]}}`)
	_, err := ParseAnthropicDetailed(raw)
	if err == nil {
		t.Fatal("unknown routing field should fail, not be silently dropped")
	}
	if !strings.Contains(err.Error(), "invalid routing") || !strings.Contains(err.Error(), "excludes") {
		t.Errorf("error = %q, want it to name the invalid routing field", err.Error())
	}
	// The message is identical to chat completions' decode error
	// for the same mistake (provider.ChatRequest.UnmarshalJSON).
	var chatReq provider.ChatRequest
	chatErr := json.Unmarshal([]byte(`{"model":"m","messages":[],"routing":{"excludes":["a"]}}`), &chatReq)
	if chatErr == nil {
		t.Fatal("chat decode should also fail on the unknown field")
	}
	if err.Error() != chatErr.Error() {
		t.Errorf("messages routing error = %q\nchat routing error    = %q\nwant identical messages", err.Error(), chatErr.Error())
	}
}

func TestDecideRoutingPresentForcesTranslate(t *testing.T) {
	v, reason := Decide("anthropic", "anthropic", Features{RoutingPresent: true})
	if v {
		t.Error("a routing contract must force translation, never verbatim")
	}
	if !strings.Contains(reason, "routing contract") {
		t.Errorf("reason = %q, want it to name the routing contract", reason)
	}
	// An empty contract does not force anything: verbatim stays
	// available for a matching dialect.
	v, _ = Decide("anthropic", "anthropic", Features{RoutingPresent: false})
	if !v {
		t.Error("no contract: matching dialect should still relay verbatim")
	}
}
