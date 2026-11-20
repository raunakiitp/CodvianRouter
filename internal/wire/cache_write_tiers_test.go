package wire

// Cache-write tier split (M6.9b): Anthropic's usage.cache_creation
// object (ephemeral_5m_input_tokens / ephemeral_1h_input_tokens)
// is parsed into the 5m/1h tiers and carried through the wire
// translations — non-streaming ConvertResponse and streaming
// TranslateStream (message_start). Legacy responses without the
// object keep the total only, tiers 0.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicUsageParsesCacheCreationSplit(t *testing.T) {
	var u AnthropicUsage
	if err := json.Unmarshal([]byte(`{"input_tokens":1000,"output_tokens":20,"cache_read_input_tokens":100,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}`), &u); err != nil {
		t.Fatalf("unmarshal AnthropicUsage: %v", err)
	}
	if u.CacheCreationInputTokens != 300 {
		t.Errorf("total = %d, want 300", u.CacheCreationInputTokens)
	}
	if got5m, got1h := u.CacheWriteTiers(); got5m != 100 || got1h != 200 {
		t.Errorf("tiers = %d/%d, want 100/200", got5m, got1h)
	}
}

func TestConvertResponseCarriesCacheWriteTiers(t *testing.T) {
	stop := "end_turn"
	resp, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg-1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: &stop,
		Usage: AnthropicUsage{
			InputTokens:              1000,
			OutputTokens:             20,
			CacheReadInputTokens:     100,
			CacheCreationInputTokens: 300,
			CacheCreation: &AnthropicCacheCreation{
				Ephemeral5mInputTokens: 100,
				Ephemeral1hInputTokens: 200,
			},
		},
	})
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 300 || resp.Usage.CacheWriteTokens5m != 100 || resp.Usage.CacheWriteTokens1h != 200 {
		t.Fatalf("usage = %+v, want cache-write 300 / 5m 100 / 1h 200", resp.Usage)
	}
}

func TestConvertResponseLegacyCacheWriteTotalOnly(t *testing.T) {
	// Old responses carry no cache_creation object: the total
	// survives, the tiers stay 0 (unknown, never guessed).
	stop := "end_turn"
	resp, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg-2",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: &stop,
		Usage: AnthropicUsage{
			InputTokens:              1000,
			OutputTokens:             20,
			CacheCreationInputTokens: 300,
		},
	})
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 300 || resp.Usage.CacheWriteTokens5m != 0 || resp.Usage.CacheWriteTokens1h != 0 {
		t.Fatalf("usage = %+v, want cache-write 300 / 5m 0 / 1h 0", resp.Usage)
	}
	// A partial split object (only one tier present) carries
	// that tier and leaves the other 0.
	resp2, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg-3",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: &stop,
		Usage: AnthropicUsage{
			InputTokens:              1000,
			OutputTokens:             20,
			CacheCreationInputTokens: 100,
			CacheCreation: &AnthropicCacheCreation{
				Ephemeral5mInputTokens: 100,
			},
		},
	})
	if err != nil {
		t.Fatalf("ConvertResponse partial: %v", err)
	}
	if resp2.Usage.CacheWriteTokens5m != 100 || resp2.Usage.CacheWriteTokens1h != 0 {
		t.Fatalf("partial usage = %+v, want 5m 100 / 1h 0", resp2.Usage)
	}
}

func TestTranslateStreamCarriesCacheWriteTiers(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":300,\"cache_creation\":{\"ephemeral_5m_input_tokens\":100,\"ephemeral_1h_input_tokens\":200}}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var finalUsage *Usage
	res, err := TranslateStream(strings.NewReader(sse), func(c ChatChunk) error {
		if c.Usage != nil {
			finalUsage = c.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if res.Usage.CacheWriteTokens != 300 || res.Usage.CacheWriteTokens5m != 100 || res.Usage.CacheWriteTokens1h != 200 {
		t.Fatalf("stream usage = %+v, want cache-write 300 / 5m 100 / 1h 200", res.Usage)
	}
	if finalUsage == nil || finalUsage.CacheWriteTokens5m != 100 || finalUsage.CacheWriteTokens1h != 200 {
		t.Fatalf("final chunk usage = %+v, want tiers 100/200", finalUsage)
	}
}

func TestTranslateStreamLegacyCacheWriteTotalOnly(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":1000,\"cache_creation_input_tokens\":300}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	res, err := TranslateStream(strings.NewReader(sse), nil)
	if err != nil {
		t.Fatalf("TranslateStream: %v", err)
	}
	if res.Usage.CacheWriteTokens != 300 || res.Usage.CacheWriteTokens5m != 0 || res.Usage.CacheWriteTokens1h != 0 {
		t.Fatalf("stream usage = %+v, want cache-write 300 / 5m 0 / 1h 0", res.Usage)
	}
}
