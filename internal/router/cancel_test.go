// Cancel propagation + always-terminal stream events + non-streaming
// empty-completion failover (CHECKLIST 153; LiteLLM v1.111.0-dev.4
// gap: aborted Bedrock streams kept reading after the client
// disconnected, and a Responses fallback replayed tool calls that
// had already been delivered).
//
// (a) A client abort (context cancel) must stop upstream reads
// immediately: the engine propagates the caller's ctx to the
// provider client (bridgeStream selects on ctx.Done, and every
// real client binds its HTTP request to that ctx, so the transport
// aborts the body read), and no new chunk is emitted after the
// abort. The test mock counts reads after abort: zero new reads
// past a small grace window for the one already in flight.
// (b) Every stream ends with a terminal event: the channel yields
// a Done event on success or an Err event on failure/cancel, and
// is then closed — never a hang. A cancel yields a terminal Err
// carrying context.Canceled (not a spurious "upstream closed
// stream" error) and never fails over to the next target.
// (c) A non-streaming empty completion fails over before the 200
// commits: the streaming half is the preread buffer (CHECKLIST
// 133, proven by TestStreamingPrereadFailoverGate); this file
// proves the non-streaming half, which the preread buffer never
// sees.
package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// tickChunkClient is a slow upstream stand-in: it emits one chunk
// per tick until ctx ends, counting every emission with its
// timestamp. It respects ctx exactly the way the real clients do
// (their HTTP transport aborts the body read on ctx cancel), so a
// test that cancels mid-stream and then sees no further emission
// proves the engine propagated the abort instead of letting the
// upstream run on.
type tickChunkClient struct {
	mu        sync.Mutex
	interval  time.Duration
	calls     int
	emissions []time.Time
}

func (c *tickChunkClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *tickChunkClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return c.ChatStreamChunks(ctx, req, func(chunk provider.StreamChunk) error {
		if chunk.Content == "" {
			return nil
		}
		return onDelta(chunk.Content)
	})
}

func (c *tickChunkClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	tick := time.NewTicker(c.interval)
	defer tick.Stop()
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			c.mu.Lock()
			c.emissions = append(c.emissions, time.Now())
			c.mu.Unlock()
			if err := onChunk(provider.StreamChunk{Content: fmt.Sprintf("t%d ", i)}); err != nil {
				return nil, err
			}
		}
	}
}

func (c *tickChunkClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *tickChunkClient) TestKey(ctx context.Context) error                { return nil }

func (c *tickChunkClient) streamCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// emissionsAfter counts chunks emitted strictly after t.
func (c *tickChunkClient) emissionsAfter(t time.Time) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, e := range c.emissions {
		if e.After(t) {
			n++
		}
	}
	return n
}

func TestStreamCancelStopsUpstreamReads(t *testing.T) {
	// The LiteLLM gap, reproduced as a router-level proof: a
	// client that goes away mid-stream must stop the upstream
	// read loop — no wasted tokens/cost after the client is gone
	// — and must never fail over to the next target (a cancel is
	// the caller's decision, not a provider failure).
	a := &tickChunkClient{interval: 20 * time.Millisecond}
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := eng.ChatStreamWithChain(ctx, chain, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	// Drain until two deltas arrive, then abort: the client is gone.
	// The terminal Err event (context.Canceled) is expected after
	// the abort; the channel must then close promptly — the engine
	// sends its terminal event and stops, it never hangs.
	var got string
	var cancelAt time.Time
	var lastDeltaAt time.Time
	var terminalErr error
	sentCancel := false
	timeout := time.After(10 * time.Second)
drain:
	for {
		select {
		case <-timeout:
			t.Fatal("stream did not terminate within 10s of cancel")
		case ev, ok := <-ch:
			if !ok {
				break drain
			}
			if ev.Err != nil {
				if !sentCancel {
					t.Fatalf("unexpected terminal error before cancel: %v", ev.Err)
				}
				terminalErr = ev.Err
				continue
			}
			if ev.Done {
				t.Fatalf("stream finished before cancel (got %q)", got)
			}
			got += ev.Delta
			lastDeltaAt = time.Now()
			if got == "t1 t2 " && !sentCancel {
				cancelAt = time.Now()
				cancel()
				sentCancel = true
			}
		}
	}
	if terminalErr == nil {
		t.Fatal("channel closed with no terminal event after cancel")
	}
	if !errors.Is(terminalErr, context.Canceled) {
		t.Fatalf("terminal error = %v, want context.Canceled", terminalErr)
	}
	if got == "" || !strings.HasPrefix(got, "t1 t2 ") {
		t.Fatalf("stream = %q, want prefix %q (at least the two deltas that triggered the cancel)", got, "t1 t2 ")
	}
	// No new chunk delivered after the abort, past the same
	// 150 ms grace the upstream-emission assertion uses
	// (CHECKLIST 190): the old exact-equality check failed
	// whenever the one delta already in flight when the cancel
	// landed won the buffered-channel send race and arrived as
	// "t1 t2 t3 " — that delta was emitted before the abort, so
	// its arrival is not a violation, but anything arriving
	// after the grace window is. The invariant — the abort stops
	// the stream — is still asserted twice: here on delivery,
	// and below on upstream emissions.
	if !lastDeltaAt.IsZero() && lastDeltaAt.After(cancelAt.Add(150*time.Millisecond)) {
		t.Fatalf("last delta arrived %v after abort (+150ms grace), want none", lastDeltaAt.Sub(cancelAt))
	}
	// Zero new upstream reads after the abort, past a 150 ms grace
	// window for the single read that may already have been in
	// flight when the cancel landed.
	time.Sleep(300 * time.Millisecond)
	if n := a.emissionsAfter(cancelAt.Add(150 * time.Millisecond)); n != 0 {
		t.Fatalf("upstream emitted %d chunks after abort (+150ms grace), want 0", n)
	}
	if b.streamCalls() != 0 {
		t.Fatalf("fallback stream calls = %d, want 0 (a cancel never fails over)", b.streamCalls())
	}
	if trace := eng.LastTrace(); trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	} else {
		got := trace.Attempts[0]
		if got.Reason != "cancelled" {
			t.Fatalf("trace attempt reason = %q, want cancelled", got.Reason)
		}
		if got.Terminal != "cancelled" {
			t.Fatalf("trace attempt terminal = %q, want cancelled", got.Terminal)
		}
	}
}

func TestStreamCancelTerminalEvent(t *testing.T) {
	// (b) A cancelled stream ends with a terminal Err event
	// carrying context.Canceled — never a hang, and never a
	// spurious "upstream closed stream" error from the bridge
	// channel closing as the cancellation unwinds.
	a := &tickChunkClient{interval: 20 * time.Millisecond}
	b := streamClient([]string{"bee"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := eng.ChatStreamWithChain(ctx, chain, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	// Cancel as soon as the first delta arrives, then drain to
	// close: collectStream fails if the channel closes with no
	// terminal event at all.
	sentCancel := false
	sawTerminal := false
	timeout := time.After(10 * time.Second)
	for {
		select {
		case <-timeout:
			t.Fatal("stream did not terminate within 10s of cancel")
		case ev, ok := <-ch:
			if !ok {
				if !sawTerminal {
					t.Fatal("channel closed with no terminal event after cancel")
				}
				return
			}
			if ev.Done {
				t.Fatal("stream finished cleanly after cancel, want a terminal error")
			}
			if ev.Err != nil {
				if !errors.Is(ev.Err, context.Canceled) {
					t.Fatalf("terminal error = %v, want context.Canceled", ev.Err)
				}
				sawTerminal = true
				continue
			}
			if !sentCancel {
				cancel()
				sentCancel = true
			}
		}
	}
}

func TestStreamTerminalDoneRecorded(t *testing.T) {
	// (b, success side) A clean stream records terminal "done" on
	// its attempt, and a content-then-death stream records
	// terminal "error" — the dashboard's Live Routing view renders
	// the stream's ending from this real trace data.
	a := streamClient([]string{"hello"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	if out, err := collectStream(t, ch); err != nil || out != "hello" {
		t.Fatalf("stream = %q err = %v, want hello/nil", out, err)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Terminal != "done" {
		t.Fatalf("trace = %+v, want 1 attempt with terminal done", trace)
	}

	dead := streamClient([]string{"Hello "}, errors.New("boom"))
	eng2 := NewEngine(map[string]provider.Client{"a": dead}, nil, testConfig())
	ch2, err := eng2.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	if out, err := collectStream(t, ch2); err == nil || out != "Hello " {
		t.Fatalf("stream = %q err = %v, want partial Hello + error", out, err)
	}
	trace2 := eng2.LastTrace()
	if trace2 == nil || len(trace2.Attempts) != 1 || trace2.Attempts[0].Terminal != "error" {
		t.Fatalf("trace = %+v, want 1 attempt with terminal error", trace2)
	}
	if trace2.Attempts[0].Outcome != "stream_error" {
		t.Fatalf("outcome = %q, want stream_error", trace2.Attempts[0].Outcome)
	}
}

// TestNonStreamingEmptyCompletionFailsOver is the non-streaming half
// of (c): the preread buffer only ever sees streams, so a
// non-streaming provider that answers 200 with no content and no
// tool calls must fail over in the same request, before the 200
// commits — never a success with an empty body.
func TestNonStreamingEmptyCompletionFailsOver(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	newEngine := func(a, b provider.Client) *Engine {
		return NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	}

	t.Run("empty-stop-fails-over", func(t *testing.T) {
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "stop"}, nil
		}}
		b := okClient("bee")
		eng := newEngine(a, b)
		resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err != nil {
			t.Fatalf("ChatWithChain: %v", err)
		}
		if resp.Content != "bee" {
			t.Fatalf("content = %q, want bee", resp.Content)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1", a.chatCalls(), b.chatCalls())
		}
		if trace == nil || len(trace.Attempts) != 2 {
			t.Fatalf("trace = %+v, want 2 attempts", trace)
		}
		if got := trace.Attempts[0]; got.Reason != "empty_stream" {
			t.Fatalf("attempt 0 = %+v, want reason empty_stream", got)
		}
	})

	t.Run("empty-finish-only-fails-over", func(t *testing.T) {
		// A finish reason with no content is still empty.
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "length"}, nil
		}}
		b := okClient("bee")
		eng := newEngine(a, b)
		resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err != nil || resp.Content != "bee" {
			t.Fatalf("content = %q err = %v, want bee/nil", resp.Content, err)
		}
	})

	t.Run("tool-call-is-not-empty", func(t *testing.T) {
		// A reply that carries a tool call is real content: the
		// client will execute that call, so it must NOT fail over.
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{Name: "get_weather", Arguments: `{"city":"Pune"}`}}}}, nil
		}}
		b := okClient("bee")
		eng := newEngine(a, b)
		resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err != nil {
			t.Fatalf("ChatWithChain: %v", err)
		}
		if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Function.Name != "get_weather" {
			t.Fatalf("tool calls = %+v, want get_weather", resp.ToolCalls)
		}
		if b.chatCalls() != 0 {
			t.Fatalf("b chat calls = %d, want 0 (no failover on a tool-carrying reply)", b.chatCalls())
		}
	})

	t.Run("content-filter-empty-fails-over", func(t *testing.T) {
		// A content-filter refusal is still an empty completion
		// for routing (another target may serve the prompt under
		// a different filter) — only the breaker stays neutral.
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "content_filter"}, nil
		}}
		b := okClient("bee")
		eng := newEngine(a, b)
		resp, _, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err != nil || resp.Content != "bee" {
			t.Fatalf("content = %q err = %v, want bee/nil", resp.Content, err)
		}
	})

	t.Run("all-targets-empty-is-honest-error", func(t *testing.T) {
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "stop"}, nil
		}}
		b := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "stop"}, nil
		}}
		eng := newEngine(a, b)
		resp, trace, err := eng.ChatWithChain(context.Background(), chain, testReq())
		if err == nil {
			t.Fatalf("ChatWithChain: resp = %+v, want an error (never a fake empty success)", resp)
		}
		var ese *provider.EmptyStreamError
		if !errors.As(err, &ese) {
			t.Fatalf("err = %v, want *EmptyStreamError", err)
		}
		if trace == nil || len(trace.Attempts) != 2 {
			t.Fatalf("trace = %+v, want 2 attempts", trace)
		}
	})

	t.Run("exempt-request-never-replayed", func(t *testing.T) {
		// Replay-exempt requests get exactly one attempt: an
		// empty embeddings reply is returned as-is, never
		// failed over onto another target.
		a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "", FinishReason: "stop"}, nil
		}}
		b := okClient("bee")
		eng := newEngine(a, b)
		req := testReq()
		req.Endpoint = provider.EndpointEmbeddings
		resp, _, err := eng.ChatWithChain(context.Background(), chain, req)
		if err != nil {
			t.Fatalf("ChatWithChain: %v", err)
		}
		if resp.Content != "" {
			t.Fatalf("content = %q, want the empty reply as-is", resp.Content)
		}
		if b.chatCalls() != 0 {
			t.Fatalf("b chat calls = %d, want 0 (exempt requests are never replayed)", b.chatCalls())
		}
	})
}
