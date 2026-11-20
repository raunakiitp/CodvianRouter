// M6.3 truncated-response drain gap: a text-only provider client
// (ChatStream only, no ChatStreamChunks) reports truncation only in
// the returned response's FinishReason — there is no per-chunk
// reason for the bridge to forward. Before the fix, the Done event
// carried no FinishReason at all (it lived only inside ev.Resp,
// which the drain never read for the reason), so the reassembled
// response came back with FinishReason "" and degrade.store()
// passed it ("unknown passes" — cache.ShouldStore,
// internal/cache/key.go:207-215). These tests prove the finish
// reason survives the drain for such a client, on both the text
// and the chunk entry points (both funnel through
// chatStreamDrainChunks), and that the bridge Done event itself
// carries it.
package router

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// m63TextOnlyLengthClient is a text-only client (ChatStream only —
// it deliberately does NOT implement provider.ChunkStreamer) whose
// stream is cut off by max_tokens: it emits one delta, then returns
// a response with FinishReason "length".
func m63TextOnlyLengthClient() *fakeClient {
	return &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("partial"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "partial", FinishReason: "length"}, nil
	}}
}

func m63LengthReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: RoleUser, Content: "hi"}},
	}
}

// TestM63TextOnlyStreamKeepsLengthFinishReason (M6.3): a truncated
// stream from a text-only client must reassemble with FinishReason
// "length" — the drain falls back to the Done event's Resp when the
// event itself carries no reason.
func TestM63TextOnlyStreamKeepsLengthFinishReason(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": m63TextOnlyLengthClient()}, nil, testConfig())
	var got []string
	resp, err := eng.ChatStream(context.Background(), m63LengthReq(), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp == nil || resp.Content != "partial" {
		t.Fatalf("ChatStream returned %+v deltas %q, want content \"partial\"", resp, got)
	}
	if resp.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want \"length\" (truncated text-only stream must not reassemble as unknown)", resp.FinishReason)
	}
}

// TestM63TextOnlyChunkStreamKeepsLengthFinishReason (M6.3): the
// chunk entry point routes through the same drain, so a truncated
// text-only stream must keep "length" there too.
func TestM63TextOnlyChunkStreamKeepsLengthFinishReason(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": m63TextOnlyLengthClient()}, nil, testConfig())
	var got []provider.StreamChunk
	resp, err := eng.ChatStreamChunks(context.Background(), m63LengthReq(), func(c provider.StreamChunk) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp == nil || resp.Content != "partial" {
		t.Fatalf("ChatStreamChunks returned %+v chunks %v, want content \"partial\"", resp, got)
	}
	if resp.FinishReason != "length" {
		t.Fatalf("FinishReason = %q, want \"length\" (truncated text-only chunk stream must not reassemble as unknown)", resp.FinishReason)
	}
}

// TestM63BridgeDoneEventCarriesLengthFinishReason (M6.3): the
// bridge's Done event itself carries the finish reason (mirroring
// replayComboResult), so producers — not just the drain fallback —
// carry "length" for a text-only client.
func TestM63BridgeDoneEventCarriesLengthFinishReason(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": m63TextOnlyLengthClient()}, nil, testConfig())
	ch := eng.bridgeStream(context.Background(), Attempt{ProviderID: "a", Model: "m"}, m63TextOnlyLengthClient(), m63LengthReq())
	var sawDelta bool
	var done *StreamEvent
	timeout := time.After(10 * time.Second)
	for done == nil {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("bridge channel closed without a Done event")
			}
			if ev.Err != nil {
				t.Fatalf("bridge event error: %v", ev.Err)
			}
			if ev.Delta != "" {
				sawDelta = true
			}
			if ev.Done {
				ev := ev
				done = &ev
			}
		case <-timeout:
			t.Fatal("timed out waiting for the bridge Done event")
		}
	}
	if !sawDelta {
		t.Fatal("bridge emitted no text delta before Done")
	}
	if done.FinishReason != "length" {
		t.Fatalf("Done event FinishReason = %q, want \"length\"", done.FinishReason)
	}
	if done.Resp == nil || done.Resp.FinishReason != "length" {
		t.Fatalf("Done event Resp = %+v, want FinishReason \"length\"", done.Resp)
	}
	// Drain to close so the bridge goroutine can exit.
	for range ch {
	}
}
