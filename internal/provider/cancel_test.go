// Cancel propagation at the provider-client layer (CHECKLIST 153;
// LiteLLM v1.111.0-dev.4 gap: aborted Bedrock streams kept reading
// after the client disconnected).
//
// Every client binds its upstream HTTP request to the caller's ctx
// (NewRequestWithContext), so cancelling ctx makes the transport
// abort the response-body read: a blocked SSE scan unblocks at
// once, the client call returns promptly, and the mock upstream
// sees the connection go away and stops writing. The mock counts
// writes after abort: zero new writes past a grace window for the
// one already in flight — the observable half of "zero new reads".
// The same mechanism covers every native wire (Anthropic, Gemini,
// Cohere read SSE through the same ctx-bound request; Bedrock reads
// its event stream off the same ctx-bound response body), so one
// test on the OpenAI wire proves the shared path; the Anthropic
// wire's reader is covered by the same assertion shape below.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// slowSSEMock streams one SSE line per tick and records every write
// with its timestamp, stopping the moment its own request ctx ends
// (the server side notices the client going away) or a write fails.
type slowSSEMock struct {
	mu     sync.Mutex
	writes []time.Time
}

func (m *slowSSEMock) writesAfter(t time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.writes {
		if w.After(t) {
			n++
		}
	}
	return n
}

// writesBefore counts lines written strictly before t.
func (m *slowSSEMock) writesBefore(t time.Time) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, w := range m.writes {
		if w.Before(t) {
			n++
		}
	}
	return n
}

func (m *slowSSEMock) handler(t *testing.T, data func(i int) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for i := 1; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(w, "data: %s\n\n", data(i)); err != nil {
					return
				}
				fl.Flush()
				m.mu.Lock()
				m.writes = append(m.writes, time.Now())
				m.mu.Unlock()
			}
		}
	}
}

func TestOpenAIStreamCancelStopsUpstreamReads(t *testing.T) {
	mock := &slowSSEMock{}
	srv := httptest.NewServer(mock.handler(t, func(i int) string {
		return fmt.Sprintf(`{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"t%d "},"finish_reason":null}]}`, i)
	}))
	defer srv.Close()
	c := mustClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	var cancelAt time.Time
	seen := 0
	start := time.Now()
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("OpenAI client does not implement ChunkStreamer")
	}
	_, err := cs.ChatStreamChunks(ctx, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		seen++
		if seen == 2 {
			// The client is gone: abort mid-stream.
			cancelAt = time.Now()
			cancel()
		}
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ChatStreamChunks returned nil error after cancel, want a prompt cancellation error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (reads must stop immediately)", elapsed)
	}
	if seen != 2 {
		t.Fatalf("client delivered %d chunks, want exactly 2", seen)
	}
	// Zero new upstream writes after the abort, past a 500 ms
	// grace window for the single write that may already have
	// been in flight when the cancel landed.
	time.Sleep(600 * time.Millisecond)
	if n := mock.writesAfter(cancelAt.Add(500 * time.Millisecond)); n != 0 {
		t.Fatalf("mock wrote %d SSE lines after abort (+500ms grace), want 0", n)
	}
	// The total-line bound is time-derived, not a fixed count
	// (CHECKLIST 190): the mock emits at most one line per
	// 30 ms tick, so the lines it could have written before the
	// client cancelled are bounded by the time the client took
	// to cancel — a fixed "<= 6" assumed the client kept up
	// with the 30 ms producer, which scheduling jitter under
	// -race does not guarantee. After the abort it must still
	// stop promptly: at most 3 more lines inside the grace
	// window (the server checks its request ctx every tick and
	// a write on a dead connection fails at once).
	maxBefore := int(cancelAt.Sub(start)/(30*time.Millisecond)) + 2
	if before := mock.writesBefore(cancelAt); before > maxBefore {
		t.Fatalf("mock wrote %d lines before abort, want <= %d (one per 30ms tick over %v)", before, maxBefore, cancelAt.Sub(start))
	}
	if after := mock.writesAfter(cancelAt); after > 3 {
		t.Fatalf("mock wrote %d lines after abort, want <= 3 (must stop promptly)", after)
	}
}

func TestAnthropicStreamCancelStopsUpstreamReads(t *testing.T) {
	// Same proof for the Anthropic wire, whose SSE reader
	// (wire.readSSE) is a different loop over the same ctx-bound
	// response body: cancelling must still stop the reads. The
	// handler opens the message and a text block, then ticks
	// text deltas — the translator emits one chunk per delta.
	mock := &slowSSEMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		for _, line := range []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_x","model":"m","usage":{"input_tokens":1}}}` + "\n\n",
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n",
		} {
			if _, err := fmt.Fprint(w, line); err != nil {
				return
			}
			fl.Flush()
		}
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for i := 1; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"t%d \"}}\n\n", i); err != nil {
					return
				}
				fl.Flush()
				mock.mu.Lock()
				mock.writes = append(mock.writes, time.Now())
				mock.mu.Unlock()
			}
		}
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "test-key", WithWireFormat(WireFormatAnthropic))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var cancelAt time.Time
	seen := 0
	start := time.Now()
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("Anthropic client does not implement ChunkStreamer")
	}
	_, err = cs.ChatStreamChunks(ctx, ChatRequest{
		Model:    "m",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil // the role-only opening chunk
		}
		seen++
		if seen == 2 {
			cancelAt = time.Now()
			cancel()
		}
		return nil
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("ChatStreamChunks returned nil error after cancel, want a prompt cancellation error")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (reads must stop immediately)", elapsed)
	}
	if seen != 2 {
		t.Fatalf("client delivered %d content chunks, want exactly 2", seen)
	}
	time.Sleep(600 * time.Millisecond)
	if n := mock.writesAfter(cancelAt.Add(500 * time.Millisecond)); n != 0 {
		t.Fatalf("mock wrote %d SSE lines after abort (+500ms grace), want 0", n)
	}
}

// TestGeminiStreamCancelStopsUpstreamReads is the Gemini sibling of
// TestOpenAIStreamCancelStopsUpstreamReads (CHECKLIST 205): the
// Gemini reader shares the buffered-delivery shape (wire.readSSE
// returns already-buffered lines without touching the network), so
// without the emit-side ctx check a chunk buffered before the abort
// was delivered after it.
//
// The mock proves the buffered case deterministically, not by
// timing: it bursts 10 SSE lines in a single flush, so all 10 are
// already inside the client's scanner buffer before the first chunk
// is delivered (a slow 30 ms tick, as in the OpenAI test, leaves
// nothing buffered at the abort and passes even without the fix —
// verified by the sensitivity check below). Cancelling after 2
// content chunks must deliver exactly 2, return context.Canceled
// unwrapped (the router recognises it via errors.Is and never fails
// over on it), and stop the mock's tick writes.
func TestGeminiStreamCancelStopsUpstreamReads(t *testing.T) {
	mock := &slowSSEMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		// Burst: 10 lines in one flush, all buffered client-side
		// before the first chunk is delivered.
		for i := 1; i <= 10; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"t%d \"}]}}]}\n\n", i); err != nil {
				return
			}
			mock.mu.Lock()
			mock.writes = append(mock.writes, time.Now())
			mock.mu.Unlock()
		}
		fl.Flush()
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for i := 11; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(w, "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"t%d \"}]}}]}\n\n", i); err != nil {
					return
				}
				fl.Flush()
				mock.mu.Lock()
				mock.writes = append(mock.writes, time.Now())
				mock.mu.Unlock()
			}
		}
	}))
	defer srv.Close()
	c := mustGeminiClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	var cancelAt time.Time
	seen := 0
	start := time.Now()
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("Gemini client does not implement ChunkStreamer")
	}
	_, err := cs.ChatStreamChunks(ctx, ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		seen++
		if seen == 2 {
			cancelAt = time.Now()
			cancel()
		}
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStreamChunks error = %v, want context.Canceled (unwrapped, so the router recognises the cancel)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (reads must stop immediately)", elapsed)
	}
	if seen != 2 {
		t.Fatalf("client delivered %d chunks, want exactly 2 (10 were buffered)", seen)
	}
	time.Sleep(600 * time.Millisecond)
	if n := mock.writesAfter(cancelAt.Add(500 * time.Millisecond)); n != 0 {
		t.Fatalf("mock wrote %d SSE lines after abort (+500ms grace), want 0", n)
	}
	// The burst (10 lines) precedes the abort; at most 3 more lines
	// may slip out inside the grace window before the server
	// notices the dead connection.
	if after := mock.writesAfter(cancelAt); after > 3 {
		t.Fatalf("mock wrote %d lines after abort, want <= 3 (must stop promptly)", after)
	}
}

// TestCohereStreamCancelStopsUpstreamReads is the Cohere sibling of
// TestOpenAIStreamCancelStopsUpstreamReads (CHECKLIST 205): the
// Cohere reader shares the buffered-delivery shape (wire.readSSE),
// so without the emit-side ctx check a chunk buffered before the
// abort was delivered after it. Same proof as the Gemini sibling:
// the mock bursts 10 content-delta lines in a single flush (after
// the message-start line), so all 10 are already inside the
// client's scanner buffer before the first chunk is delivered —
// cancelling after 2 must deliver exactly 2, return
// context.Canceled unwrapped, and stop the mock's tick writes.
func TestCohereStreamCancelStopsUpstreamReads(t *testing.T) {
	mock := &slowSSEMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		if _, err := fmt.Fprint(w, "data: {\"type\":\"message-start\",\"id\":\"chat-1\"}\n\n"); err != nil {
			return
		}
		// Burst: 10 deltas in one flush, all buffered client-side
		// before the first chunk is delivered.
		for i := 1; i <= 10; i++ {
			if _, err := fmt.Fprintf(w, "data: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"type\":\"text\",\"text\":\"t%d \"}}}}\n\n", i); err != nil {
				return
			}
			mock.mu.Lock()
			mock.writes = append(mock.writes, time.Now())
			mock.mu.Unlock()
		}
		fl.Flush()
		tick := time.NewTicker(30 * time.Millisecond)
		defer tick.Stop()
		for i := 11; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(w, "data: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"type\":\"text\",\"text\":\"t%d \"}}}}\n\n", i); err != nil {
					return
				}
				fl.Flush()
				mock.mu.Lock()
				mock.writes = append(mock.writes, time.Now())
				mock.mu.Unlock()
			}
		}
	}))
	defer srv.Close()
	c := mustCohereClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	var cancelAt time.Time
	seen := 0
	start := time.Now()
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("Cohere client does not implement ChunkStreamer")
	}
	_, err := cs.ChatStreamChunks(ctx, ChatRequest{
		Model:    "command-a-03-2025",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		seen++
		if seen == 2 {
			cancelAt = time.Now()
			cancel()
		}
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStreamChunks error = %v, want context.Canceled (unwrapped, so the router recognises the cancel)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (reads must stop immediately)", elapsed)
	}
	if seen != 2 {
		t.Fatalf("client delivered %d chunks, want exactly 2 (10 were buffered)", seen)
	}
	time.Sleep(600 * time.Millisecond)
	if n := mock.writesAfter(cancelAt.Add(500 * time.Millisecond)); n != 0 {
		t.Fatalf("mock wrote %d SSE lines after abort (+500ms grace), want 0", n)
	}
	if after := mock.writesAfter(cancelAt); after > 3 {
		t.Fatalf("mock wrote %d lines after abort, want <= 3 (must stop promptly)", after)
	}
}

// TestBedrockStreamCancelStopsBufferedFrames is the Bedrock sibling
// of TestOpenAIStreamCancelStopsUpstreamReads (CHECKLIST 205). Since
// CHECKLIST 215 the reader delivers frames incrementally, so the
// mock's 10 text deltas arrive one at a time rather than all
// buffered — the proof is the same delivery-side rule: the callback
// cancels after the 2nd content chunk, and exactly 2 must be
// delivered — no later frame may leak through after the abort —
// with context.Canceled returned unwrapped.
func TestBedrockStreamCancelStopsBufferedFrames(t *testing.T) {
	m := &bedrockMock{t: t}
	var stream []byte
	stream = append(stream, bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"})...)
	stream = append(stream, bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}})...)
	for i := 1; i <= 10; i++ {
		stream = append(stream, bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": fmt.Sprintf("t%d ", i)}})...)
	}
	stream = append(stream, bedrockEventFrame(t, "contentBlockStop", map[string]any{"contentBlockIndex": 0})...)
	stream = append(stream, bedrockEventFrame(t, "metadata", map[string]any{"usage": map[string]any{"inputTokens": 12, "outputTokens": 8, "totalTokens": 20}})...)
	stream = append(stream, bedrockEventFrame(t, "messageStop", map[string]any{"stopReason": "end_turn"})...)
	m.streamBody = stream
	srv := httptest.NewServer(m.handler())
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	seen := 0
	start := time.Now()
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("Bedrock client does not implement ChunkStreamer")
	}
	_, err := cs.ChatStreamChunks(ctx, ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content == "" {
			return nil // the role-only opening chunk
		}
		seen++
		if seen == 2 {
			cancel()
		}
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStreamChunks error = %v, want context.Canceled (unwrapped, so the router recognises the cancel)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (buffered frames must stop at once)", elapsed)
	}
	if seen != 2 {
		t.Fatalf("client delivered %d content chunks, want exactly 2 (10 frames were buffered)", seen)
	}
}

// TestBedrockStreamCancelAbortsInflightRead proves the other half of
// the Bedrock cancel path (CHECKLIST 205, updated by 215): a stream
// still in flight — the mock ticks frames forever and never closes —
// must abort the moment the caller's ctx ends. Since CHECKLIST 215
// the reader delivers frames incrementally, so chunks DO arrive
// before the abort (the opening role chunk + the deltas ticked in
// the first ~150 ms); the proof is that the call returns promptly
// with context.Canceled, delivery stops at the abort, and the mock
// stops writing once the client goes away.
func TestBedrockStreamCancelAbortsInflightRead(t *testing.T) {
	mock := &slowSSEMock{}
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
			mock.mu.Lock()
			mock.writes = append(mock.writes, time.Now())
			mock.mu.Unlock()
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
				if !write(bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": fmt.Sprintf("t%d ", i)}})) {
					return
				}
			}
		}
	}))
	defer srv.Close()
	c := mustBedrockClient(t, srv)

	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	// No chunk can arrive before the read finishes, so the abort
	// comes from a timer, not from a delivered chunk.
	time.AfterFunc(150*time.Millisecond, cancel)
	seen := 0
	_, err := c.(ChunkStreamer).ChatStreamChunks(ctx, ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		seen++
		return nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStreamChunks error = %v, want context.Canceled", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ChatStreamChunks took %s after cancel, want < 2s (the in-flight read must abort at once)", elapsed)
	}
	if seen == 0 {
		t.Fatalf("client delivered 0 chunks, want >= 1 (incremental delivery since CHECKLIST 215: the opening frames arrive before the 150 ms abort)")
	}
	cancelAt := start.Add(150 * time.Millisecond)
	time.Sleep(600 * time.Millisecond)
	if n := mock.writesAfter(cancelAt.Add(500 * time.Millisecond)); n != 0 {
		t.Fatalf("mock wrote %d frames after abort (+500ms grace), want 0", n)
	}
}
