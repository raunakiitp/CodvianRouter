package chaos

import (
	"context"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// okClient is a healthy fake: answers "ok" at once.
type okClient struct{ chats int }

func (c *okClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.chats++
	return &provider.ChatResponse{Content: "ok"}, nil
}
func (c *okClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}
func (c *okClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *okClient) TestKey(ctx context.Context) error                { return nil }

func TestInjectorKill(t *testing.T) {
	base := &okClient{}
	inj := NewInjector(base, Fault{Kill: true})
	ctx := context.Background()
	req := provider.ChatRequest{Model: "m"}
	if _, err := inj.Chat(ctx, req); err == nil {
		t.Fatalf("killed Chat: want error, got nil")
	}
	if _, err := inj.ChatStream(ctx, req, nil); err == nil {
		t.Fatalf("killed ChatStream: want error, got nil")
	}
	if base.chats != 0 {
		t.Fatalf("wrapped client called %d times, want 0 (kill short-circuits)", base.chats)
	}
	if calls, failures := inj.Stats(); calls != 2 || failures != 2 {
		t.Fatalf("stats = (%d, %d), want (2, 2)", calls, failures)
	}
	// Revive mid-run: the same injector passes through again.
	inj.SetFault(Fault{})
	if _, err := inj.Chat(ctx, req); err != nil {
		t.Fatalf("revived Chat: %v", err)
	}
	if base.chats != 1 {
		t.Fatalf("wrapped client called %d times, want 1 after revive", base.chats)
	}
}

func TestInjectorFailTimes(t *testing.T) {
	base := &okClient{}
	inj := NewInjector(base, Fault{FailTimes: 2})
	ctx := context.Background()
	req := provider.ChatRequest{Model: "m"}
	for i := 0; i < 2; i++ {
		if _, err := inj.Chat(ctx, req); err == nil {
			t.Fatalf("call %d: want failure, got nil", i+1)
		}
	}
	if _, err := inj.Chat(ctx, req); err != nil {
		t.Fatalf("call 3 (past FailTimes): %v", err)
	}
	if calls, failures := inj.Stats(); calls != 3 || failures != 2 {
		t.Fatalf("stats = (%d, %d), want (3, 2)", calls, failures)
	}
}

func TestInjectorSlow(t *testing.T) {
	base := &okClient{}
	inj := NewInjector(base, Fault{Slow: 50 * time.Millisecond})
	ctx := context.Background()
	start := time.Now()
	if _, err := inj.Chat(ctx, provider.ChatRequest{Model: "m"}); err != nil {
		t.Fatalf("slow Chat: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("elapsed = %v, want >= 50ms", elapsed)
	}
	// A cancelled context aborts the slow wait instead of calling through.
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := inj.Chat(cancelCtx, provider.ChatRequest{Model: "m"}); err == nil {
		t.Fatalf("cancelled slow Chat: want error, got nil")
	}
	if base.chats != 1 {
		t.Fatalf("wrapped client called %d times, want 1 (cancelled wait never dials)", base.chats)
	}
}

func TestInjectorKillError(t *testing.T) {
	// A Kill fault with an explicit error returns exactly that error
	// (so a scenario can kill with a 429, a 503, or a transport error).
	base := &okClient{}
	want := &provider.RateLimitError{Body: "slow down"}
	inj := NewInjector(base, Fault{Kill: true, Err: want})
	if _, err := inj.Chat(context.Background(), provider.ChatRequest{Model: "m"}); err != want {
		t.Fatalf("Chat error = %v, want the injected error", err)
	}
	// A Kill fault with no error is a retryable 500, so the engine
	// fails over on it instead of stopping the chain.
	inj2 := NewInjector(base, Fault{Kill: true})
	_, err := inj2.Chat(context.Background(), provider.ChatRequest{Model: "m"})
	var ue *provider.UpstreamError
	if !isUpstream500(err, &ue) {
		t.Fatalf("default kill error = %T %v, want retryable *UpstreamError 500", err, err)
	}
}

func isUpstream500(err error, out **provider.UpstreamError) bool {
	if e, ok := err.(*provider.UpstreamError); ok && e.StatusCode == 500 && e.Retryable {
		*out = e
		return true
	}
	return false
}
