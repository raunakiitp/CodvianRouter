// Stream-lifetime cap tests (CHECKLIST 255): every held or
// relayed stream gets a hard max lifetime — 30 minutes by default,
// operator-configurable, disabled only by an explicit negative
// cap. On expiry the upstream call is terminated and the client
// sees a clean terminal error naming the cap, never a hang and
// never a [DONE]. Tests use short caps (milliseconds), never a
// 30-minute wait.
package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// stallChunkClient is a chunk-streaming client that emits the
// scripted chunks, then blocks until its context ends — the shape
// of a held stream (a provider that stops emitting but keeps the
// connection open). It records when ChatStreamChunks returned, so
// a test can prove the upstream call was actually terminated on
// expiry rather than left hanging.
type stallChunkClient struct {
	mu         sync.Mutex
	streams    int
	chunksSent int
	returned   bool
	returnedAt time.Time
	startedAt  time.Time
	chunkFn    func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

func (c *stallChunkClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *stallChunkClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *stallChunkClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.streams++
	c.startedAt = time.Now()
	fn := c.chunkFn
	c.mu.Unlock()
	resp, err := fn(ctx, req, func(ck provider.StreamChunk) error {
		// Count only chunks the bridge ACCEPTED: after the cap
		// fires the bridge's send refuses (ctx done), so a
		// post-cap chunk the mock still tries to emit is counted
		// as refused, never as delivered.
		err := onChunk(ck)
		if err == nil {
			c.mu.Lock()
			c.chunksSent++
			c.mu.Unlock()
		}
		return err
	})
	c.mu.Lock()
	c.returned = true
	c.returnedAt = time.Now()
	c.mu.Unlock()
	return resp, err
}

func (c *stallChunkClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *stallChunkClient) TestKey(ctx context.Context) error                { return nil }

func (c *stallChunkClient) stats() (streams, chunks int, returned bool, startedAt, returnedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams, c.chunksSent, c.returned, c.startedAt, c.returnedAt
}

// waitReturned waits (up to timeout) for the upstream call to
// return after the cap fired — the engine closes the stream
// channel as soon as the terminal event is sent, which races the
// mock's own return, so the flag must be polled, never read once.
func (c *stallChunkClient) waitReturned(t *testing.T, timeout time.Duration) (startedAt, returnedAt time.Time) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ret, s, r := c.returned, c.startedAt, c.returnedAt
		c.mu.Unlock()
		if ret {
			return s, r
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("upstream call did not return after the cap fired")
	return time.Time{}, time.Time{}
}

// stallEngine builds an engine whose only provider is a
// stallChunkClient with the given chunkFn, and a cap of cap.
func stallEngine(t *testing.T, cap time.Duration, chunkFn func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)) (*Engine, *stallChunkClient) {
	t.Helper()
	cli := &stallChunkClient{chunkFn: chunkFn}
	cfg := testConfig()
	cfg.StreamMaxLifetime = cap
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": cli}, &fakeLedger{}, cfg)
	return eng, cli
}

// TestStreamLifetimeCapKillsHeldStream: a provider that emits two
// chunks then holds the connection open forever is cut at the cap
// — the client gets the two deltas, then a terminal error naming
// the cap, never [DONE], never a hang.
func TestStreamLifetimeCapKillsHeldStream(t *testing.T) {
	eng, cli := stallEngine(t, 150*time.Millisecond, func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: "t1"}); err != nil {
			return nil, err
		}
		if err := onChunk(provider.StreamChunk{Content: "t2"}); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	deltas, termErr := collectStream(t, ch)
	if deltas != "t1t2" {
		t.Errorf("deltas = %q, want t1t2", deltas)
	}
	if termErr == nil || !errors.Is(termErr, ErrStreamLifetimeExceeded) {
		t.Errorf("terminal error = %v, want the stream-lifetime error", termErr)
	}
	if eng.StreamLifetimeExceededCount() != 1 {
		t.Errorf("killed count = %d, want 1", eng.StreamLifetimeExceededCount())
	}
	if tr := eng.LastTrace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Outcome != "error" || tr.Attempts[0].Terminal != "error" {
		t.Errorf("trace = %+v, want one attempt, outcome error, terminal error", tr)
	}
	streams, chunks, _, _, _ := cli.stats()
	if streams != 1 || chunks != 2 {
		t.Errorf("upstream stats = streams %d, chunks %d, want 1, 2", streams, chunks)
	}
	started, returnedAt := cli.waitReturned(t, 2*time.Second)
	if returnedAt.Sub(started) > 2*time.Second {
		t.Errorf("upstream call returned after %v, want promptly at the 150ms cap", returnedAt.Sub(started))
	}
}

// TestStreamLifetimeCapTerminatesUpstream: after the cap fires the
// mock must not emit any further chunk — the upstream call is
// terminated, not just the client side. The mock tries one more
// chunk after its context ends; the bridge must refuse it.
func TestStreamLifetimeCapTerminatesUpstream(t *testing.T) {
	eng, cli := stallEngine(t, 150*time.Millisecond, func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: "x"}); err != nil {
			return nil, err
		}
		<-ctx.Done()
		// The call is dead: any further chunk must be refused
		// by the bridge (send checks ctx first). Record the
		// attempt either way — the test asserts it never lands.
		_ = onChunk(provider.StreamChunk{Content: "after-cap"})
		return nil, ctx.Err()
	})
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	deltas, termErr := collectStream(t, ch)
	if deltas != "x" {
		t.Errorf("deltas = %q, want x (nothing after the cap)", deltas)
	}
	if termErr == nil || !errors.Is(termErr, ErrStreamLifetimeExceeded) {
		t.Errorf("terminal error = %v, want the stream-lifetime error", termErr)
	}
	if _, chunks, _, _, _ := cli.stats(); chunks != 1 {
		t.Errorf("chunks sent = %d, want 1 (the post-cap chunk must never land)", chunks)
	}
}

// TestStreamLifetimeCapBoundsPreread: the cap bounds the preread
// buffer path (CHECKLIST 133-134) too — a stream that has emitted
// only one empty event (never content, never Done) is held in the
// preread buffer, and the cap still cuts it. Nothing was ever
// delivered to the client, so the deltas are empty.
func TestStreamLifetimeCapBoundsPreread(t *testing.T) {
	eng, _ := stallEngine(t, 150*time.Millisecond, func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: ""}); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	deltas, termErr := collectStream(t, ch)
	if deltas != "" {
		t.Errorf("deltas = %q, want empty (preread never committed)", deltas)
	}
	if termErr == nil || !errors.Is(termErr, ErrStreamLifetimeExceeded) {
		t.Errorf("terminal error = %v, want the stream-lifetime error", termErr)
	}
	if tr := eng.LastTrace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Preread != "" {
		t.Errorf("trace = %+v, want one attempt with no preread commit", tr)
	}
}

// TestStreamLifetimeCapDisabled: a negative cap disables the
// lifetime bound — a stream that finishes on its own is unaffected
// and the kill counter stays at zero.
func TestStreamLifetimeCapDisabled(t *testing.T) {
	eng, cli := stallEngine(t, -1, func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: "done"}); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "done"}, nil
	})
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	deltas, termErr := collectStream(t, ch)
	if deltas != "done" || termErr != nil {
		t.Errorf("deltas = %q, err = %v, want done, nil", deltas, termErr)
	}
	if eng.StreamLifetimeExceededCount() != 0 {
		t.Errorf("killed count = %d, want 0 (cap disabled)", eng.StreamLifetimeExceededCount())
	}
	if _, chunks, _, _, _ := cli.stats(); chunks != 1 {
		t.Errorf("chunks = %d, want 1", chunks)
	}
}

// TestStreamLifetimeCapDefault: a zero cap resolves to the 30-minute
// default, and a negative cap resolves to 0 (disabled) — the
// defaults are part of the contract, not an accident of one build.
func TestStreamLifetimeCapDefault(t *testing.T) {
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": &fakeClient{}}, &fakeLedger{}, cfg)
	if got := eng.StreamMaxLifetime(); got != DefaultStreamMaxLifetime {
		t.Errorf("default cap = %v, want %v", got, DefaultStreamMaxLifetime)
	}
	if DefaultStreamMaxLifetime != 30*time.Minute {
		t.Errorf("DefaultStreamMaxLifetime = %v, want 30m", DefaultStreamMaxLifetime)
	}
	cfg.StreamMaxLifetime = -1
	eng = NewEngine(map[string]provider.Client{"a": &fakeClient{}}, &fakeLedger{}, cfg)
	if got := eng.StreamMaxLifetime(); got != 0 {
		t.Errorf("disabled cap = %v, want 0", got)
	}
	cfg.StreamMaxLifetime = 5 * time.Minute
	eng = NewEngine(map[string]provider.Client{"a": &fakeClient{}}, &fakeLedger{}, cfg)
	if got := eng.StreamMaxLifetime(); got != 5*time.Minute {
		t.Errorf("configured cap = %v, want 5m", got)
	}
}

// TestStreamLifetimeCapDoesNotFireOnFastStream: a stream that
// finishes well inside the cap is untouched — the cap is a bound
// on a held stream, not a race against every stream.
func TestStreamLifetimeCapDoesNotFireOnFastStream(t *testing.T) {
	eng, _ := stallEngine(t, 500*time.Millisecond, func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: "fast"}); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "fast"}, nil
	})
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	deltas, termErr := collectStream(t, ch)
	if deltas != "fast" || termErr != nil {
		t.Errorf("deltas = %q, err = %v, want fast, nil", deltas, termErr)
	}
	if eng.StreamLifetimeExceededCount() != 0 {
		t.Errorf("killed count = %d, want 0", eng.StreamLifetimeExceededCount())
	}
}

// TestStreamLifetimeCapNoFailover: expiry of the cap never fails
// over to the next attempt — the stream is dead, and the next
// provider would be serving a client that has already been told
// the stream ended. A chain of two attempts ends at the cap with
// exactly one trace attempt.
func TestStreamLifetimeCapNoFailover(t *testing.T) {
	cli := &stallChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	never := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		t.Error("second attempt was tried after the cap fired — the cap must never fail over")
		return &provider.ChatResponse{Content: "never"}, nil
	}}
	cfg := testConfig()
	cfg.StreamMaxLifetime = 150 * time.Millisecond
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{"a": cli, "b": never}, &fakeLedger{}, cfg)
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}, testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	if _, termErr := collectStream(t, ch); termErr == nil || !errors.Is(termErr, ErrStreamLifetimeExceeded) {
		t.Errorf("terminal error = %v, want the stream-lifetime error", termErr)
	}
	if tr := eng.LastTrace(); tr == nil || len(tr.Attempts) != 1 {
		t.Errorf("trace attempts = %d, want exactly 1 (no failover on cap expiry)", len(tr.Attempts))
	}
}
