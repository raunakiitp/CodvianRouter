// Per-provider Bedrock abort on early stream close (CHECKLIST
// 277 (a), LiteLLM #51034 half (e)): the engine-level early close
// (CHECKLIST 243) unwinds via ctx, but the proof that the abort
// actually stops billed generation upstream lives here, at the
// provider client. The mock keeps "generating" — one frame every
// 40 ms — until the client drops the connection; the test proves
// the connection drops promptly after the cancel / consumer error
// and the mock stops sending frames, instead of generating to
// completion with no reader.
package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// slowBedrockMock is a Bedrock runtime that never finishes on its
// own: it streams one contentBlockDelta frame every 40 ms until
// the client disconnects. framesWritten counts every frame sent;
// disconnected closes when the handler returns (i.e. when the
// server notices the connection drop).
type slowBedrockMock struct {
	framesWritten atomic.Int32
	disconnected  chan struct{}
	opening       [][]byte
	deltas        [][]byte
}

func (m *slowBedrockMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer close(m.disconnected)
		if !isBedrockStreamPath(r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		// The fixed opening frames, then one delta per tick.
		// Each write is followed by a flush so the client sees
		// the frame immediately; the tick select is what stops
		// generation the moment the connection drops.
		for _, f := range m.opening {
			if _, err := w.Write(f); err != nil {
				return
			}
			fl.Flush()
			m.framesWritten.Add(1)
		}
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				f := m.deltas[i%len(m.deltas)]
				if _, err := w.Write(f); err != nil {
					return
				}
				fl.Flush()
				m.framesWritten.Add(1)
			}
		}
	}
}

func isBedrockStreamPath(path string) bool {
	return len(path) > len("/converse-stream") && path[len(path)-len("/converse-stream"):] == "/converse-stream"
}

func newSlowBedrockMock(t *testing.T) (*httptest.Server, *slowBedrockMock) {
	t.Helper()
	m := &slowBedrockMock{disconnected: make(chan struct{})}
	m.opening = [][]byte{
		bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"}),
		bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
	}
	for i := 0; i < 200; i++ {
		m.deltas = append(m.deltas, bedrockEventFrame(t, "contentBlockDelta", map[string]any{
			"contentBlockIndex": 0,
			"delta":             map[string]any{"text": fmt.Sprintf("tok%d ", i)},
		}))
	}
	return httptest.NewServer(m.handler()), m
}

func newBedrockStreamClient(t *testing.T, url string) *bedrockClient {
	t.Helper()
	c, err := NewClient(url, testAWSKey, WithWireFormat(WireFormatBedrock))
	if err != nil {
		t.Fatalf("NewClient bedrock: %v", err)
	}
	bc, ok := c.(*bedrockClient)
	if !ok {
		t.Fatalf("NewClient with bedrock wire returned %T, want *bedrockClient", c)
	}
	return bc
}

// waitDisconnected fails the test unless the mock notices the
// client drop within d — i.e. unless the abort actually closed
// the connection.
func (m *slowBedrockMock) waitDisconnected(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-m.disconnected:
	case <-time.After(d):
		t.Fatalf("mock upstream did not notice the disconnect within %s (frames sent: %d) — the abort did not stop generation upstream", d, m.framesWritten.Load())
	}
}

func TestBedrockAbortOnContextCancelStopsUpstream(t *testing.T) {
	srv, mock := newSlowBedrockMock(t)
	defer srv.Close()
	client := newBedrockStreamClient(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	var chunks int
	_, err := client.ChatStreamChunks(ctx, ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content != "" {
			chunks++
			if chunks == 1 {
				cancel()
			}
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ChatStreamChunks error = %v, want context.Canceled", err)
	}
	if chunks != 1 {
		t.Fatalf("content chunks delivered = %d, want exactly 1 (no frame after the abort)", chunks)
	}
	// The upstream must notice the drop promptly — and stop
	// generating: only the frames already in flight may have
	// been sent (2 opening + the first delta, plus at most one
	// tick that raced the cancel).
	mock.waitDisconnected(t, 3*time.Second)
	if got := mock.framesWritten.Load(); got > 5 {
		t.Fatalf("mock sent %d frames after the client cancelled — generation did not stop upstream", got)
	}
}

func TestBedrockAbortOnConsumerErrorStopsUpstream(t *testing.T) {
	srv, mock := newSlowBedrockMock(t)
	defer srv.Close()
	client := newBedrockStreamClient(t, srv.URL)

	downstreamGone := errors.New("downstream gone")
	var chunks int
	_, err := client.ChatStreamChunks(context.Background(), ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content != "" {
			chunks++
			if chunks == 1 {
				return downstreamGone
			}
		}
		return nil
	})
	if !errors.Is(err, downstreamGone) {
		t.Fatalf("ChatStreamChunks error = %v, want the consumer error", err)
	}
	if chunks != 1 {
		t.Fatalf("content chunks delivered = %d, want exactly 1", chunks)
	}
	mock.waitDisconnected(t, 3*time.Second)
	if got := mock.framesWritten.Load(); got > 5 {
		t.Fatalf("mock sent %d frames after the consumer error — generation did not stop upstream", got)
	}
}

// TestBedrockAbortOnConsumerErrorStopsUpstreamImmediately is the
// sensitivity proof for the explicit abort (CHECKLIST 305). The two
// tests above PASS on the old code too: their mock ticks at 40 ms,
// and the old code's deferred body close — after Translate unwinds —
// fits inside one tick, so the incidental transport close already
// stops generation there. This test sees the difference: the mock
// gates a 1 ms tick behind a signal the test sends the moment chunk 1
// is delivered, so every frame after the signal is purely post-abort.
// The explicit cancel() in emit drops the connection within ~1 ms —
// 0 post-signal frames in 30 runs, including 5 under -race — while
// with the 277 hunks reverted the deferred body close lets the mock
// emit 10–50 more frames first (29/30 runs fail the ≤2 bound): the
// old close is ~10–50 ms late, i.e. 10–50 ms of billed generation
// after the consumer stopped. The bound is 2, not 0, for the one
// tick that may already be in flight when the connection drops.
func TestBedrockAbortOnConsumerErrorStopsUpstreamImmediately(t *testing.T) {
	startTick := make(chan struct{})
	var postSignal atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isBedrockStreamPath(r.URL.Path) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		write := func(f []byte) bool {
			if _, err := w.Write(f); err != nil {
				return false
			}
			fl.Flush()
			return true
		}
		for _, f := range [][]byte{
			bedrockEventFrame(t, "messageStart", map[string]any{"role": "assistant"}),
			bedrockEventFrame(t, "contentBlockStart", map[string]any{"contentBlockIndex": 0, "start": map[string]any{}}),
			bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": "tok0 "}}),
		} {
			if !write(f) {
				return
			}
		}
		// Wait for the test's signal: chunk 1 has been delivered
		// and the consumer is about to error. Only then start the
		// 1 ms tick, so the tick count measures the abort delay
		// and nothing else.
		select {
		case <-startTick:
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			return
		}
		tk := time.NewTicker(time.Millisecond)
		defer tk.Stop()
		for i := 1; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tk.C:
				f := bedrockEventFrame(t, "contentBlockDelta", map[string]any{"contentBlockIndex": 0, "delta": map[string]any{"text": fmt.Sprintf("tok%d ", i)}})
				if !write(f) {
					return
				}
				postSignal.Add(1)
			}
		}
	}))
	defer srv.Close()
	client := newBedrockStreamClient(t, srv.URL)

	downstreamGone := errors.New("downstream gone")
	var chunks int
	var once sync.Once
	_, err := client.ChatStreamChunks(context.Background(), ChatRequest{
		Model:    "anthropic.claude-3-haiku-20240307-v1:0",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}, func(chunk StreamChunk) error {
		if chunk.Content != "" {
			chunks++
			if chunks == 1 {
				once.Do(func() { close(startTick) })
				return downstreamGone
			}
		}
		return nil
	})
	if !errors.Is(err, downstreamGone) {
		t.Fatalf("ChatStreamChunks error = %v, want the consumer error", err)
	}
	if chunks != 1 {
		t.Fatalf("content chunks delivered = %d, want exactly 1", chunks)
	}
	// Give the mock a moment to notice the drop — or keep ticking.
	time.Sleep(100 * time.Millisecond)
	if got := postSignal.Load(); got > 2 {
		t.Fatalf("mock sent %d frames after the abort signal, want <= 2 (the connection must drop within ~2 ms of the consumer error)", got)
	}
}
