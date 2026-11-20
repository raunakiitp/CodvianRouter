package wire

// Cached-token mapping (CHECKLIST 132): provider-reported cached
// input tokens are carried through the wire translations — Anthropic
// cache_read_input_tokens, Gemini cachedContentTokenCount — so the
// dashboard counts only what the provider reported.

import (
	"strings"
	"testing"
)

func TestConvertResponseCarriesAnthropicCachedTokens(t *testing.T) {
	stop := "end_turn"
	resp, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg-1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-x",
		Content: []AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: &stop,
		Usage: AnthropicUsage{
			InputTokens:          1000,
			OutputTokens:         20,
			CacheReadInputTokens: 700,
		},
	})
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if resp.Usage.CachedTokens != 700 || resp.Usage.PromptTokens != 1000 {
		t.Fatalf("usage = %+v, want prompt 1000 cached 700", resp.Usage)
	}
	// No cache_read field -> 0, never a guess.
	resp2, err := ConvertResponse(&AnthropicResponse{
		ID:    "msg-2",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-x",
		Content: []AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: &stop,
		Usage:      AnthropicUsage{InputTokens: 50, OutputTokens: 5},
	})
	if err != nil {
		t.Fatalf("ConvertResponse 2: %v", err)
	}
	if resp2.Usage.CachedTokens != 0 {
		t.Fatalf("usage = %+v, want cached 0", resp2.Usage)
	}
}

func TestConvertGeminiResponseCarriesCachedTokens(t *testing.T) {
	resp, err := ConvertGeminiResponse(&GeminiResponse{
		ModelVersion: "gemini-x",
		Candidates: []GeminiCandidate{{
			Content:      &GeminiContent{Parts: []GeminiPart{{Text: "hi"}}},
			FinishReason: "STOP",
		}},
		UsageMetadata: &GeminiUsageMetadata{
			PromptTokenCount:        400,
			CandidatesTokenCount:    10,
			TotalTokenCount:         410,
			CachedContentTokenCount: 250,
		},
	})
	if err != nil {
		t.Fatalf("ConvertGeminiResponse: %v", err)
	}
	if resp.Usage.CachedTokens != 250 || resp.Usage.PromptTokens != 400 {
		t.Fatalf("usage = %+v, want prompt 400 cached 250", resp.Usage)
	}
}

func TestTranslateStreamCarriesAnthropicCachedTokens(t *testing.T) {
	sse := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":900,\"cache_read_input_tokens\":600}}}\n\n" +
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
	if res.Usage.CachedTokens != 600 || res.Usage.PromptTokens != 900 || res.Usage.CompletionTokens != 3 {
		t.Fatalf("stream usage = %+v, want prompt 900 cached 600 completion 3", res.Usage)
	}
}
