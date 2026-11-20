package provider

// Cache-write tier carry (M6.9b): the Anthropic client maps the
// wire usage split (usage.cache_creation ephemeral_5m/1h) into
// provider.Usage.CacheWriteTokens5m/1h on both the non-streaming
// and streaming paths. Only Anthropic reports a split — no other
// client in this tree populates the tiers.

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

func TestAnthropicFromWireResponseCarriesCacheWriteTiers(t *testing.T) {
	converted, err := wire.ConvertResponse(&wire.AnthropicResponse{
		ID:    "msg-1",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []wire.AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: strptr("end_turn"),
		Usage: wire.AnthropicUsage{
			InputTokens:              1000,
			OutputTokens:             20,
			CacheCreationInputTokens: 300,
			CacheCreation: &wire.AnthropicCacheCreation{
				Ephemeral5mInputTokens: 100,
				Ephemeral1hInputTokens: 200,
			},
		},
	})
	if err != nil {
		t.Fatalf("wire.ConvertResponse: %v", err)
	}
	resp, err := fromWireResponse(converted)
	if err != nil {
		t.Fatalf("fromWireResponse: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 300 || resp.Usage.CacheWriteTokens5m != 100 || resp.Usage.CacheWriteTokens1h != 200 {
		t.Fatalf("usage = %+v, want cache-write 300 / 5m 100 / 1h 200", resp.Usage)
	}
	// Legacy wire usage (no split object): total only, tiers 0.
	converted2, err := wire.ConvertResponse(&wire.AnthropicResponse{
		ID:    "msg-2",
		Type:  "message",
		Role:  "assistant",
		Model: "claude-sonnet-4-5",
		Content: []wire.AnthropicBlock{
			{Type: "text", Text: "hi"},
		},
		StopReason: strptr("end_turn"),
		Usage: wire.AnthropicUsage{
			InputTokens:              1000,
			OutputTokens:             20,
			CacheCreationInputTokens: 300,
		},
	})
	if err != nil {
		t.Fatalf("wire.ConvertResponse legacy: %v", err)
	}
	resp2, err := fromWireResponse(converted2)
	if err != nil {
		t.Fatalf("fromWireResponse legacy: %v", err)
	}
	if resp2.Usage.CacheWriteTokens != 300 || resp2.Usage.CacheWriteTokens5m != 0 || resp2.Usage.CacheWriteTokens1h != 0 {
		t.Fatalf("legacy usage = %+v, want cache-write 300 / 5m 0 / 1h 0", resp2.Usage)
	}
}

func strptr(s string) *string { return &s }

func TestAnthropicChatStreamChunksCarriesCacheWriteTiers(t *testing.T) {
	srv, mock := newAnthropicMock(t)
	defer srv.Close()
	mock.streamBody = "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":1000,\"cache_creation_input_tokens\":300,\"cache_creation\":{\"ephemeral_5m_input_tokens\":100,\"ephemeral_1h_input_tokens\":200}}}}\n\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\"}}\n\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi there\"}}\n\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	cs, ok := mustAnthropicClient(t, srv).(ChunkStreamer)
	if !ok {
		t.Fatal("anthropic client must implement ChunkStreamer")
	}
	var finalChunkUsage *Usage
	resp, err := cs.ChatStreamChunks(context.Background(), ChatRequest{
		Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Usage != nil {
			finalChunkUsage = chunk.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Usage.CacheWriteTokens != 300 || resp.Usage.CacheWriteTokens5m != 100 || resp.Usage.CacheWriteTokens1h != 200 {
		t.Fatalf("stream usage = %+v, want cache-write 300 / 5m 100 / 1h 200", resp.Usage)
	}
	if finalChunkUsage == nil || finalChunkUsage.CacheWriteTokens5m != 100 || finalChunkUsage.CacheWriteTokens1h != 200 {
		t.Fatalf("final chunk usage = %+v, want tiers 100/200", finalChunkUsage)
	}
}
