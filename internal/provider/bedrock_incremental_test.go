package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestBedrockStreamIncrementalDelivery proves CHECKLIST 215 through
// the real client: a Bedrock mock that trickles frames 400 ms apart
// must deliver the first content chunk long before the body
// completes — the old ReadAll reader delivered nothing until the
// last byte arrived.
func TestBedrockStreamIncrementalDelivery(t *testing.T) {
	frames := [][]byte{
		bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"}),
		bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
		bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "first "}}),
		bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "second"}}),
		bedrockEventFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0}),
		bedrockEventFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 12, "outputTokens": 8, "totalTokens": 20}}),
		bedrockEventFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"}),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		for i, f := range frames {
			if i > 0 {
				time.Sleep(400 * time.Millisecond)
			}
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		}
	}))
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	start := time.Now()
	var firstAt time.Time
	var mu sync.Mutex
	var content string
	res, err := c.(ChunkStreamer).ChatStreamChunks(context.Background(), ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content != "" {
			mu.Lock()
			if firstAt.IsZero() {
				firstAt = time.Now()
			}
			mu.Unlock()
			content += chunk.Content
		}
		return nil
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if res.Content != "first second" {
		t.Fatalf("reassembled content = %q, want \"first second\"", res.Content)
	}
	if content != "first second" {
		t.Fatalf("streamed content = %q, want \"first second\"", content)
	}
	if res.Usage.TotalTokens != 20 {
		t.Fatalf("usage total = %d, want 20", res.Usage.TotalTokens)
	}
	mu.Lock()
	defer mu.Unlock()
	if firstAt.IsZero() {
		t.Fatal("no content chunk was delivered")
	}
	// 7 frames x 400 ms gaps = ~2.4 s; the first content frame is
	// available immediately.
	if firstAt.Sub(start) > elapsed-500*time.Millisecond {
		t.Fatalf("first content chunk arrived at %s into a %s stream, want >= 500 ms before the end (incremental delivery)", firstAt.Sub(start), elapsed)
	}
}

// TestBedrockStreamNeverEndingDeliversChunks proves the fixed
// CHECKLIST 205 limitation end to end: a Bedrock stream that never
// ends still streams the chunks it has already sent. The mock ticks
// forever; at least one content chunk must arrive while the stream
// is still open, and cancelling the ctx must then return
// context.Canceled promptly.
func TestBedrockStreamNeverEndingDeliversChunks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		write := func(frame []byte) bool {
			if _, err := w.Write(frame); err != nil {
				return false
			}
			fl.Flush()
			return true
		}
		if !write(bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"})) {
			return
		}
		if !write(bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}})) {
			return
		}
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for i := 1; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if !write(bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "t"}})) {
					return
				}
			}
		}
	}))
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		_, err := c.(ChunkStreamer).ChatStreamChunks(ctx, ChatRequest{
			Model:    "anthropic.claude-3-haiku-20240307-v1:0",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}, func(chunk StreamChunk) error {
			if chunk.Content != "" {
				select {
				case first <- time.Now():
				default:
				}
			}
			return nil
		})
		done <- err
	}()
	select {
	case <-first:
		// First chunk arrived while the stream is still open —
		// the old reader delivered nothing here.
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("no content chunk arrived from a never-ending Bedrock stream in 5 s")
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ChatStreamChunks error = %v, want context.Canceled", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("cancel took %s, want < 2s", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatStreamChunks did not return after cancel")
	}
}

// TestBedrockStreamAbortsOnLifetimeCancel (CHECKLIST 255): the
// stream-lifetime cap cancels the request context with a cause —
// the Bedrock incremental stream must abort promptly on that
// cancellation, mid event-stream, so a stalled Bedrock stream
// cannot outlive its cap. The mock stalls after one delta, exactly
// the held-stream shape the cap exists for.
func TestBedrockStreamAbortsOnLifetimeCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		for _, f := range [][]byte{
			bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"}),
			bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
			bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "x"}}),
		} {
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
		}
		// Stall forever: no more frames, no close — the client
		// must return on the context cancellation alone.
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	// The cap's shape: a WithCancelCause child whose cause names
	// the expiry (the router cancels its lifetime ctx this way).
	ctx, cancelCause := context.WithCancelCause(context.Background())
	first := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		_, err := c.(ChunkStreamer).ChatStreamChunks(ctx, ChatRequest{
			Model:    "anthropic.claude-3-haiku-20240307-v1:0",
			Messages: []Message{{Role: "user", Content: "hi"}},
		}, func(chunk StreamChunk) error {
			if chunk.Content != "" {
				select {
				case first <- struct{}{}:
				default:
				}
			}
			return nil
		})
		done <- err
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		cancelCause(context.Canceled)
		t.Fatal("no content chunk arrived from the stalled Bedrock stream in 5 s")
	}
	start := time.Now()
	cancelCause(errors.New("router: stream lifetime exceeded (cap 150ms)"))
	select {
	case err := <-done:
		// The Bedrock reader wraps the cancellation cause in its
		// read error — the error must name the lifetime expiry
		// (a plain cancel() with no cause surfaces as
		// context.Canceled instead, see
		// TestBedrockStreamNeverEndingDeliversChunks).
		if err == nil || !strings.Contains(err.Error(), "stream lifetime exceeded") {
			t.Fatalf("ChatStreamChunks error = %v, want an error naming the stream lifetime expiry", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("cancel took %s, want < 2s (the cap must kill a stalled Bedrock stream promptly)", time.Since(start))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChatStreamChunks did not return after the lifetime cancellation")
	}
}
