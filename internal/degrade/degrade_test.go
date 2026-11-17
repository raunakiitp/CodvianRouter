package degrade

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/coalescing"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/server"
)

var errUpstreamDown = errors.New("all providers failed")

func testReq() provider.ChatRequest {
	return provider.ChatRequest{
		Model: "test-model",
		Messages: []provider.Message{
			{Role: "user", Content: "What is 2+2?"},
		},
	}
}

func freshResp() *provider.ChatResponse {
	return &provider.ChatResponse{
		Content: "4",
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6},
		Model:   "test-model",
	}
}

// fakeEngine is a controllable server.Engine for tests.
type fakeEngine struct {
	mu           sync.Mutex
	chatResp     *provider.ChatResponse
	chatErr      error
	streamResp   *provider.ChatResponse
	streamErr    error
	streamDeltas []string // deltas emitted in order before returning
	chatCalls    int
	streamCalls  int
}

func (f *fakeEngine) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatCalls++
	return f.chatResp, f.chatErr
}

func (f *fakeEngine) ChatStream(_ context.Context, _ provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.streamCalls++
	deltas := append([]string(nil), f.streamDeltas...)
	resp, err := f.streamResp, f.streamErr
	f.mu.Unlock()
	for _, d := range deltas {
		if err := onDelta(d); err != nil {
			return nil, err
		}
	}
	return resp, err
}

// wrapTest builds a StaleEngine around f with a fresh ExactCache and
// returns the concrete wrapper so tests can control its clock.
func wrapTest(f server.Engine, policy Policy) (*StaleEngine, *cache.ExactCache) {
	exact := cache.NewExactCache(100)
	eng := Wrap(f, exact, policy)
	se, ok := eng.(*StaleEngine)
	if !ok {
		panic(fmt.Sprintf("Wrap did not return *StaleEngine, got %T", eng))
	}
	return se, exact
}

// populate stores a fresh response for testReq via a successful Chat.
func populate(t *testing.T, se *StaleEngine, f *fakeEngine) {
	t.Helper()
	f.chatResp = freshResp()
	f.chatErr = nil
	ctx := context.Background()
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("populate Chat: %v", err)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("populate Chat returned %+v", resp)
	}
}

func TestChatSuccessStoresAndReturns(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, exact := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "4" || resp.Model != "test-model" {
		t.Fatalf("Chat returned %+v", resp)
	}
	if se.LastServedStale() {
		t.Fatal("fresh success must not be marked stale")
	}
	if se.LastStaleAge() != 0 {
		t.Fatalf("fresh success LastStaleAge = %v, want 0", se.LastStaleAge())
	}

	key, err := cache.ExactKey(testReq().Model, testReq().Messages)
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	cached, ok := exact.Get(ctx, key)
	if !ok {
		t.Fatal("successful Chat did not store the response in the exact cache")
	}
	if cached.Content != "4" || cached.Model != "test-model" {
		t.Fatalf("cached response = %+v", cached)
	}
	if cached.Usage.TotalTokens != 6 {
		t.Fatalf("cached usage = %+v", cached.Usage)
	}
}

func TestChatFailureWithCacheHitReturnsStale(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)

	// Now every provider fails.
	f.chatResp = nil
	f.chatErr = errUpstreamDown

	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("Chat with stale fallback should not error, got %v", err)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("stale Chat returned %+v", resp)
	}
	if resp.Model != "test-model" {
		t.Fatalf("stale Chat Model = %q, want cached model %q", resp.Model, "test-model")
	}
	if !se.LastServedStale() {
		t.Fatal("LastServedStale() = false after serving stale content")
	}
	if se.LastStaleAge() < 0 {
		t.Fatalf("LastStaleAge = %v, want >= 0", se.LastStaleAge())
	}
}

func TestChatFailureWithCacheMissReturnsOriginalError(t *testing.T) {
	f := &fakeEngine{chatErr: errUpstreamDown}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)

	_, err := se.Chat(context.Background(), testReq())
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("Chat error = %v, want the original error %v (unchanged)", err, errUpstreamDown)
	}
	if se.LastServedStale() {
		t.Fatal("cache miss must not be marked stale")
	}
}

func TestDisabledPolicyNeverServesStale(t *testing.T) {
	f := &fakeEngine{}
	se, _ := wrapTest(f, DefaultPolicy()) // Enabled=false by default
	ctx := context.Background()

	populate(t, se, f)

	f.chatResp = nil
	f.chatErr = errUpstreamDown
	_, err := se.Chat(ctx, testReq())
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("disabled policy Chat error = %v, want original error", err)
	}
	if se.LastServedStale() {
		t.Fatal("disabled policy must never mark a call stale")
	}

	// Same for streams: zero deltas delivered, cache hit, still no replay.
	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = nil
	deltas := 0
	_, err = se.ChatStream(ctx, testReq(), func(string) error { deltas++; return nil })
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("disabled policy ChatStream error = %v, want original error", err)
	}
	if deltas != 0 {
		t.Fatalf("disabled policy replayed %d deltas, want 0", deltas)
	}
}

func TestAllowStaleFalseNeverServesStale(t *testing.T) {
	f := &fakeEngine{}
	policy := Policy{Enabled: true, AllowStale: false}
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)

	f.chatResp = nil
	f.chatErr = errUpstreamDown
	_, err := se.Chat(ctx, testReq())
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("AllowStale=false Chat error = %v, want original error", err)
	}
	if se.LastServedStale() {
		t.Fatal("AllowStale=false must never mark a call stale")
	}
}

func TestExpiredEntryNeverServed(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	policy.MaxStaleAge = time.Minute
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	now := time.Now()
	se.now = func() time.Time { return now }
	populate(t, se, f)

	// Advance the clock past MaxStaleAge.
	se.now = func() time.Time { return now.Add(2 * time.Minute) }

	f.chatResp = nil
	f.chatErr = errUpstreamDown
	_, err := se.Chat(ctx, testReq())
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("expired entry Chat error = %v, want original error", err)
	}
	if se.LastServedStale() {
		t.Fatal("expired entry must never be served as stale")
	}
}

func TestYoungEntryServedUnderMaxStaleAge(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	policy.MaxStaleAge = time.Minute
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	now := time.Now()
	se.now = func() time.Time { return now }
	populate(t, se, f)

	se.now = func() time.Time { return now.Add(30 * time.Second) }

	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("young entry should be served, got error %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("young stale entry returned %+v", resp)
	}
	if !se.LastServedStale() {
		t.Fatal("young entry serve must be marked stale")
	}
	if se.LastStaleAge() != 30*time.Second {
		t.Fatalf("LastStaleAge = %v, want 30s", se.LastStaleAge())
	}
}

func TestNoAgeLimitServesOldEntry(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true // MaxStaleAge 0: no age limit
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	now := time.Now()
	se.now = func() time.Time { return now }
	populate(t, se, f)

	// A day later, past the 1h store TTL the cache would enforce in
	// real time — but the cache entry here is still present because
	// the fake clock does not move the cache's own clock; this test
	// only proves the policy imposes no age limit of its own.
	se.now = func() time.Time { return now.Add(24 * time.Hour) }

	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("no-limit policy should serve the old entry, got %v", err)
	}
	if resp.Content != "4" {
		t.Fatalf("old entry returned %+v", resp)
	}
	if !se.LastServedStale() || se.LastStaleAge() != 24*time.Hour {
		t.Fatalf("LastServedStale=%v LastStaleAge=%v, want true/24h", se.LastServedStale(), se.LastStaleAge())
	}
}

func TestFreshSuccessResetsStaleFlag(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("stale serve: %v", err)
	}
	if !se.LastServedStale() {
		t.Fatal("expected stale flag after stale serve")
	}

	// Providers recover: a fresh success must clear the flag.
	f.chatResp = freshResp()
	f.chatErr = nil
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("fresh Chat: %v", err)
	}
	if se.LastServedStale() {
		t.Fatal("fresh success after stale serve must clear LastServedStale")
	}
}

func TestChatStreamFailureZeroDeltasReplaysCachedAsOneDelta(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)

	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = nil // zero deltas delivered before the failure

	var got []string
	resp, err := se.ChatStream(ctx, testReq(), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream with stale replay should not error, got %v", err)
	}
	if len(got) != 1 || got[0] != "4" {
		t.Fatalf("replayed deltas = %q, want exactly one delta %q", got, "4")
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("replayed ChatStream returned %+v", resp)
	}
	if !se.LastServedStale() {
		t.Fatal("stale stream replay must be marked stale")
	}
}

func TestChatStreamFailureAfterPartialDeltasDoesNotReplay(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)

	// One delta reaches the caller, then the stream fails.
	f.streamResp = nil
	f.streamErr = errUpstreamDown
	f.streamDeltas = []string{"par"}

	var got []string
	_, err := se.ChatStream(ctx, testReq(), func(d string) error {
		got = append(got, d)
		return nil
	})
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("partial stream ChatStream error = %v, want original error", err)
	}
	if len(got) != 1 || got[0] != "par" {
		t.Fatalf("deltas delivered = %q, want only the partial %q (no stale replay)", got, "par")
	}
	if se.LastServedStale() {
		t.Fatal("partial stream failure must not be marked stale")
	}
}

func TestChatStreamSuccessStores(t *testing.T) {
	f := &fakeEngine{
		streamResp:   freshResp(),
		streamDeltas: []string{"4"},
	}
	se, exact := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	var got []string
	resp, err := se.ChatStream(ctx, testReq(), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "4" || len(got) != 1 {
		t.Fatalf("ChatStream returned %+v deltas %q", resp, got)
	}
	if se.LastServedStale() {
		t.Fatal("fresh stream success must not be marked stale")
	}
	key, _ := cache.ExactKey(testReq().Model, testReq().Messages)
	if _, ok := exact.Get(ctx, key); !ok {
		t.Fatal("successful ChatStream did not store the response")
	}
}

func TestChatStreamFailureCacheMissReturnsOriginalError(t *testing.T) {
	f := &fakeEngine{streamErr: errUpstreamDown}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)

	deltas := 0
	_, err := se.ChatStream(context.Background(), testReq(), func(string) error {
		deltas++
		return nil
	})
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("ChatStream error = %v, want original error", err)
	}
	if deltas != 0 {
		t.Fatalf("cache-miss stream delivered %d deltas, want 0", deltas)
	}
}

func TestWrapNilEngineReturnsNil(t *testing.T) {
	if got := Wrap(nil, cache.NewExactCache(10), DefaultPolicy()); got != nil {
		t.Fatalf("Wrap(nil engine) = %v, want nil", got)
	}
}

func TestNilExactCacheIsPassthrough(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	policy := DefaultPolicy()
	policy.Enabled = true
	eng := Wrap(f, nil, policy)
	if eng == nil {
		t.Fatal("Wrap with nil cache returned nil engine")
	}
	resp, err := eng.Chat(context.Background(), testReq())
	if err != nil || resp.Content != "4" {
		t.Fatalf("nil-cache Chat = %+v, %v", resp, err)
	}
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	_, err = eng.Chat(context.Background(), testReq())
	if !errors.Is(err, errUpstreamDown) {
		t.Fatalf("nil-cache failure error = %v, want original error", err)
	}
}

func TestDefaultPolicyIsOff(t *testing.T) {
	p := DefaultPolicy()
	if p.Enabled {
		t.Fatal("DefaultPolicy must have Enabled=false (degradation OFF by default)")
	}
	if !p.AllowStale {
		t.Fatal("DefaultPolicy must have AllowStale=true so enabling is a single Enabled flip")
	}
	if p.MaxStaleAge != 0 {
		t.Fatalf("DefaultPolicy MaxStaleAge = %v, want 0 (no age limit)", p.MaxStaleAge)
	}
}

func TestStaleEngineImplementsServerEngine(t *testing.T) {
	var _ server.Engine = (*StaleEngine)(nil)
	f := &fakeEngine{chatResp: freshResp()}
	var eng server.Engine = Wrap(f, cache.NewExactCache(10), DefaultPolicy())
	if eng == nil {
		t.Fatal("Wrap returned nil for a valid engine")
	}
}

func TestConcurrentChatCallsSafe(t *testing.T) {
	f := &fakeEngine{}
	policy := DefaultPolicy()
	policy.Enabled = true
	se, _ := wrapTest(f, policy)
	ctx := context.Background()

	populate(t, se, f)

	// Half the goroutines see failures (stale fallback), half see
	// successes (fresh store). All must complete without a race or
	// a wrong error; run with -race to catch data races.
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if i%2 == 0 {
					f.mu.Lock()
					f.chatResp = nil
					f.chatErr = errUpstreamDown
					f.mu.Unlock()
					resp, err := se.Chat(ctx, testReq())
					if err != nil && !errors.Is(err, errUpstreamDown) {
						t.Errorf("concurrent failing Chat: unexpected error %v", err)
						return
					}
					if err == nil && (resp == nil || resp.Content != "4") {
						t.Errorf("concurrent stale Chat returned %+v", resp)
						return
					}
				} else {
					f.mu.Lock()
					f.chatResp = freshResp()
					f.chatErr = nil
					f.mu.Unlock()
					resp, err := se.Chat(ctx, testReq())
					if err != nil || resp == nil || resp.Content != "4" {
						t.Errorf("concurrent fresh Chat = %+v, %v", resp, err)
						return
					}
				}
				// Reading the outcome flags concurrently too.
				_ = se.LastServedStale()
				_ = se.LastStaleAge()
			}
		}(i)
	}
	wg.Wait()
}

// resumingFakeEngine is a server.Engine that also implements
// server.StreamResumer, for ChatStreamFrom delegation tests.
type resumingFakeEngine struct {
	fakeEngine
	resumeCalled bool
	resumeSkip   int
	resumeDeltas []string
	resumeErr    error
}

func (f *resumingFakeEngine) ChatStreamFrom(_ context.Context, _ provider.ChatRequest, skipChars int, onDelta func(string) error) (*provider.ChatResponse, error) {
	f.resumeCalled = true
	f.resumeSkip = skipChars
	for _, d := range f.resumeDeltas {
		if err := onDelta(d); err != nil {
			return nil, err
		}
	}
	return f.streamResp, f.resumeErr
}

func TestChatStreamFromDelegatesToResumer(t *testing.T) {
	inner := &resumingFakeEngine{
		resumeDeltas: []string{"world"},
	}
	inner.streamResp = freshResp()
	exact := cache.NewExactCache(100)
	se := Wrap(inner, exact, DefaultPolicy()).(*StaleEngine)
	ctx := context.Background()

	var got []string
	resp, err := se.ChatStreamFrom(ctx, testReq(), 6, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if !inner.resumeCalled || inner.resumeSkip != 6 {
		t.Fatalf("resume not delegated: called=%v skip=%d", inner.resumeCalled, inner.resumeSkip)
	}
	if len(got) != 1 || got[0] != "world" {
		t.Fatalf("deltas = %v, want [world]", got)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("resp = %+v, want content 4", resp)
	}
	if se.LastServedStale() {
		t.Fatal("delegated resume must not be marked stale")
	}
}

func TestChatStreamFromFallbackTrims(t *testing.T) {
	f := &fakeEngine{
		streamDeltas: []string{"hello ", "world"},
		streamResp:   freshResp(),
	}
	se, _ := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	var got []string
	resp, err := se.ChatStreamFrom(ctx, testReq(), 6, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if len(got) != 1 || got[0] != "world" {
		t.Fatalf("deltas = %v, want [world] (first 6 runes trimmed)", got)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("resp = %+v, want content 4", resp)
	}
	if se.LastServedStale() {
		t.Fatal("live stream must not be marked stale")
	}
}

func TestChatStreamFromFallbackStaleReplay(t *testing.T) {
	f := &fakeEngine{}
	se, _ := wrapTest(f, Policy{Enabled: true, AllowStale: true})
	ctx := context.Background()

	// Populate the cache with a longer answer.
	f.chatResp = &provider.ChatResponse{
		Content: "hello world",
		Usage:   provider.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		Model:   "test-model",
	}
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("populate Chat: %v", err)
	}

	// Now the stream fails before delivering anything.
	f.streamDeltas = nil
	f.streamErr = errUpstreamDown
	var got []string
	resp, err := se.ChatStreamFrom(ctx, testReq(), 6, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if len(got) != 1 || got[0] != "world" {
		t.Fatalf("deltas = %v, want [world] (stale replay trimmed by skipChars)", got)
	}
	if resp == nil || resp.Content != "hello world" {
		t.Fatalf("resp = %+v, want cached content", resp)
	}
	if !se.LastServedStale() {
		t.Fatal("stale replay must be marked stale")
	}
}

func TestChatStreamFromPartialDeliveryNoReplay(t *testing.T) {
	f := &fakeEngine{}
	se, _ := wrapTest(f, Policy{Enabled: true, AllowStale: true})
	ctx := context.Background()
	populate(t, se, f)

	f.streamDeltas = []string{"hel"}
	f.streamErr = errUpstreamDown
	var got []string
	_, err := se.ChatStreamFrom(ctx, testReq(), 0, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err == nil {
		t.Fatal("expected error after partial delivery, got nil")
	}
	if len(got) != 1 || got[0] != "hel" {
		t.Fatalf("deltas = %v, want [hel] only (no stale replay)", got)
	}
	if se.LastServedStale() {
		t.Fatal("partial delivery must not be marked stale")
	}
}

func TestChatStreamFromResumerFailureNoReplay(t *testing.T) {
	inner := &resumingFakeEngine{resumeErr: errUpstreamDown}
	inner.streamResp = freshResp()
	exact := cache.NewExactCache(100)
	se := Wrap(inner, exact, Policy{Enabled: true, AllowStale: true}).(*StaleEngine)
	ctx := context.Background()

	// Populate via a successful Chat through the wrapper.
	inner.chatResp = freshResp()
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("populate Chat: %v", err)
	}

	var got []string
	_, err := se.ChatStreamFrom(ctx, testReq(), 0, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err == nil {
		t.Fatal("expected resume error, got nil")
	}
	if len(got) != 0 {
		t.Fatalf("deltas = %v, want none (no stale replay after resume failure)", got)
	}
	if se.LastServedStale() {
		t.Fatal("failed resume must not be marked stale")
	}
}

// chunkFakeEngine streams chunk-aware (tool-call deltas + finish
// reason), like the Anthropic client behind the router engine.
type chunkFakeEngine struct {
	fakeEngine
	chunks     []provider.StreamChunk
	chunkResp  *provider.ChatResponse
	chunkErr   error
	chunkCalls int
}

func (f *chunkFakeEngine) ChatStreamChunks(_ context.Context, _ provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	f.mu.Lock()
	f.chunkCalls++
	chunks := append([]provider.StreamChunk(nil), f.chunks...)
	resp, err := f.chunkResp, f.chunkErr
	f.mu.Unlock()
	for _, c := range chunks {
		if err := onChunk(c); err != nil {
			return nil, err
		}
	}
	return resp, err
}

func (f *chunkFakeEngine) ChatStreamFromChunks(_ context.Context, _ provider.ChatRequest, skipChars int, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	// Delegate to the same chunk stream; the wrapper's resume path is
	// tested via the text-only fakeEngine instead.
	_ = skipChars
	return f.ChatStreamChunks(context.Background(), testReq(), onChunk)
}

func TestChatStreamChunksForwardsToolCalls(t *testing.T) {
	f := &chunkFakeEngine{
		chunks: []provider.StreamChunk{
			{Content: "hi "},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather"}}},
			{ToolCalls: []provider.ChunkToolCall{{Index: 0, Arguments: `{"city":"Pune"}`}}},
			{FinishReason: "tool_calls"},
		},
		chunkResp: &provider.ChatResponse{Content: "hi ", FinishReason: "tool_calls"},
	}
	se, _ := wrapTest(f, DefaultPolicy())
	var got []provider.StreamChunk
	resp, err := se.ChatStreamChunks(context.Background(), testReq(), func(c provider.StreamChunk) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d chunks, want 4 (text, tool-call head, args, finish)", len(got))
	}
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].Name != "get_weather" {
		t.Errorf("chunk 1 = %+v, want the tool-call head", got[1])
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
}

func TestChatStreamChunksTextOnlyEngineBridged(t *testing.T) {
	// A wrapped engine without a chunk path must still serve chunk
	// streaming (text deltas as chunks) instead of failing.
	f := &fakeEngine{streamDeltas: []string{"a", "b"}, streamResp: freshResp()}
	se, _ := wrapTest(f, DefaultPolicy())
	var text string
	resp, err := se.ChatStreamChunks(context.Background(), testReq(), func(c provider.StreamChunk) error {
		text += c.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if text != "ab" || resp.Content != "4" {
		t.Errorf("text = %q resp = %+v", text, resp)
	}
}

func TestChatStreamChunksFailureZeroChunksReplaysStale(t *testing.T) {
	f := &chunkFakeEngine{chunkErr: errUpstreamDown}
	se, _ := wrapTest(f, Policy{Enabled: true, AllowStale: true})
	populate(t, se, &f.fakeEngine)
	var text string
	resp, err := se.ChatStreamChunks(context.Background(), testReq(), func(c provider.StreamChunk) error {
		text += c.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if text != "4" || resp.Content != "4" {
		t.Errorf("stale replay text = %q resp = %+v", text, resp)
	}
	if !se.LastServedStale() {
		t.Error("LastServedStale must be true after a stale chunk replay")
	}
}

func TestChatStreamChunksFailureAfterChunkNoReplay(t *testing.T) {
	f := &chunkFakeEngine{
		chunks:   []provider.StreamChunk{{Content: "partial "}},
		chunkErr: errUpstreamDown,
	}
	se, _ := wrapTest(f, DefaultPolicy())
	populate(t, se, &f.fakeEngine)
	_, err := se.ChatStreamChunks(context.Background(), testReq(), func(provider.StreamChunk) error { return nil })
	if !errors.Is(err, errUpstreamDown) {
		t.Errorf("error = %v, want the original upstream error (no replay after a delivered chunk)", err)
	}
	if se.LastServedStale() {
		t.Error("LastServedStale must be false after a partial delivery")
	}
}

func TestChatStreamFromChunksFallbackTrims(t *testing.T) {
	// Text-only wrapped engine: replay via ChatStreamChunks, trim
	// the first 3 runes, drop tool-call chunks inside the prefix.
	f := &fakeEngine{streamDeltas: []string{"Hello ", "world"}, streamResp: freshResp()}
	se, _ := wrapTest(f, DefaultPolicy())
	var got []provider.StreamChunk
	_, err := se.ChatStreamFromChunks(context.Background(), testReq(), 3, func(c provider.StreamChunk) error {
		got = append(got, c)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFromChunks: %v", err)
	}
	var text string
	for _, c := range got {
		text += c.Content
	}
	if text != "lo world" {
		t.Errorf("trimmed text = %q, want lo world", text)
	}
}

// ---------------------------------------------------------------------------
// Proactive cache serving (Policy.ServeCache + WrapWithSemantic).
// ---------------------------------------------------------------------------

// stubCacheMetrics records cache hit/miss reports for tests.
type stubCacheMetrics struct {
	mu     sync.Mutex
	hits   map[string]int
	misses map[string]int
}

func (s *stubCacheMetrics) IncCacheHit(layer string) {
	s.mu.Lock()
	s.hits[layer]++
	s.mu.Unlock()
}

func (s *stubCacheMetrics) IncCacheMiss(layer string) {
	s.mu.Lock()
	s.misses[layer]++
	s.mu.Unlock()
}

func (s *stubCacheMetrics) get(layer string) (hits, misses int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[layer], s.misses[layer]
}

// serveCachePolicy is DefaultPolicy with proactive cache serving on.
// Degradation stays enabled too, so the stale fallback keeps working
// alongside it.
func serveCachePolicy() Policy {
	p := DefaultPolicy()
	p.Enabled = true
	p.ServeCache = true
	return p
}

// wrapSemanticTest builds a StaleEngine with both caches and a stub
// metrics sink, so tests can assert which layer served and that the
// provider was (not) called.
func wrapSemanticTest(f server.Engine, policy Policy) (*StaleEngine, *cache.ExactCache, *cache.LocalSemanticCache, *stubCacheMetrics) {
	exact := cache.NewExactCache(100)
	sem := cache.NewLocalSemanticCache()
	m := &stubCacheMetrics{hits: map[string]int{}, misses: map[string]int{}}
	eng := WrapWithSemantic(f, exact, sem, policy)
	se, ok := eng.(*StaleEngine)
	if !ok {
		panic("WrapWithSemantic did not return *StaleEngine")
	}
	se.SetCacheMetrics(m)
	return se, exact, sem, m
}

func capitalReq(text string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "test-model",
		Messages: []provider.Message{{Role: "user", Content: text}},
	}
}

func TestServeCacheExactHitServesWithoutCallingProvider(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("first Chat calls = %d, want 1", f.chatCalls)
	}

	// Providers now fail: an exact hit must still answer, without
	// calling them, and must be marked a cache hit — never stale.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, testReq())
	if err != nil {
		t.Fatalf("exact-hit Chat: %v", err)
	}
	if resp == nil || resp.Content != "4" {
		t.Fatalf("exact-hit Chat returned %+v", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("exact hit called provider %d times, want 1 (no provider call on a hit)", f.chatCalls)
	}
	if se.LastServedStale() {
		t.Fatal("exact cache hit must not be marked stale")
	}
	kind, sim, ok := se.LastCacheHit()
	if !ok || kind != "exact" || sim != 0 {
		t.Fatalf("LastCacheHit = (%q, %v, %v), want (exact, 0, true)", kind, sim, ok)
	}
	if h, _ := m.get("exact"); h != 1 {
		t.Fatalf("exact cache hits = %d, want 1", h)
	}
}

func TestServeCacheSemanticHitOnParaphrase(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, capitalReq("What is the capital of France?")); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("first Chat calls = %d, want 1", f.chatCalls)
	}

	// Paraphrase with one extra word: same words, same order.
	// Must hit semantically, without calling the provider, and the
	// similarity must be reported honestly (>= threshold 0.85).
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	resp, err := se.Chat(ctx, capitalReq("What is the capital of France today?"))
	if err != nil {
		t.Fatalf("paraphrase Chat: %v", err)
	}
	if resp == nil || resp.Content != "Paris is the capital of France." {
		t.Fatalf("paraphrase Chat returned %+v", resp)
	}
	if f.chatCalls != 1 {
		t.Fatalf("semantic hit called provider %d times, want 1 (no provider call on a hit)", f.chatCalls)
	}
	if se.LastServedStale() {
		t.Fatal("semantic cache hit must not be marked stale")
	}
	kind, sim, ok := se.LastCacheHit()
	if !ok || kind != "semantic" {
		t.Fatalf("LastCacheHit = (%q, %v, %v), want (semantic, >=0.85, true)", kind, sim, ok)
	}
	if sim < 0.85 {
		t.Fatalf("semantic similarity = %v, want >= 0.85", sim)
	}
	if h, _ := m.get("semantic"); h != 1 {
		t.Fatalf("semantic cache hits = %d, want 1", h)
	}
	// The paraphrase is now stored under its own exact key too, so
	// repeating it is an exact hit next time.
	if _, err := se.Chat(ctx, capitalReq("What is the capital of France today?")); err != nil {
		t.Fatalf("repeat paraphrase Chat: %v", err)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "exact" {
		t.Fatalf("repeat paraphrase LastCacheHit kind = %q, want exact", kind)
	}
}

func TestServeCacheUnrelatedPromptMisses(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, _, _, m := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, capitalReq("What is the capital of France?")); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	calls := f.chatCalls

	// Unrelated prompt: no shared words, must miss both layers and
	// call the provider.
	f.chatResp = &provider.ChatResponse{Content: "Preheat the oven to 220C.", Model: "test-model"}
	f.chatErr = nil
	resp, err := se.Chat(ctx, capitalReq("How do I bake sourdough bread?"))
	if err != nil {
		t.Fatalf("unrelated Chat: %v", err)
	}
	if resp == nil || resp.Content != "Preheat the oven to 220C." {
		t.Fatalf("unrelated Chat returned %+v", resp)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("unrelated prompt did not call provider (calls %d, want %d)", f.chatCalls, calls+1)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("unrelated prompt must not report a cache hit")
	}
	if _, miss := m.get("semantic"); miss < 1 {
		t.Fatalf("semantic cache misses = %d, want >= 1", miss)
	}
}

func TestServeCacheSkipsToolRequests(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, exact, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	// A tool-using success must not be stored: the cached entry
	// holds content only and would silently drop tool calls.
	toolReq := testReq()
	toolReq.Tools = []provider.ToolDefinition{{Type: "function"}}
	toolReq.Tools[0].Function.Name = "get_weather"
	if _, err := se.Chat(ctx, toolReq); err != nil {
		t.Fatalf("tool Chat: %v", err)
	}
	key, err := cache.ExactKey(toolReq.Model, toolReq.Messages)
	if err != nil {
		t.Fatalf("ExactKey: %v", err)
	}
	if _, ok := exact.Get(ctx, key); ok {
		t.Fatal("tool request must not be stored in the exact cache")
	}

	// And a tool request must not be served from the cache either,
	// even when a plain-text entry for the same messages exists.
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("plain Chat: %v", err)
	}
	calls := f.chatCalls
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	if _, err := se.Chat(ctx, toolReq); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tool Chat with failing provider: err = %v, want original error (no cache serve)", err)
	}
	if f.chatCalls != calls+1 {
		t.Fatalf("tool request did not reach provider (calls %d, want %d)", f.chatCalls, calls+1)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("tool request must not report a cache hit")
	}
}

func TestServeCacheOffByDefault(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	// DefaultPolicy: ServeCache is false. A repeat request must call
	// the provider again — proactive serving is opt-in, so the old
	// behaviour (cache only as a failure fallback) is preserved.
	se, _ := wrapTest(f, DefaultPolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("chatCalls = %d, want 2 (no proactive serving without ServeCache)", f.chatCalls)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("ServeCache off must never report a cache hit")
	}
}

func TestServeCacheStreamReplaysSemanticHit(t *testing.T) {
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Model:   "test-model",
	}}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, capitalReq("What is the capital of France?")); err != nil {
		t.Fatalf("populate Chat: %v", err)
	}
	if f.streamCalls != 0 {
		t.Fatalf("populate used stream calls = %d, want 0", f.streamCalls)
	}

	// Streaming paraphrase: no provider stream is started; the cached
	// answer is replayed as a single delta.
	f.chatResp = nil
	f.chatErr = errUpstreamDown
	f.streamErr = errUpstreamDown
	var deltas []string
	resp, err := se.ChatStream(ctx, capitalReq("What is the capital of France today?"), func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("paraphrase ChatStream: %v", err)
	}
	if f.streamCalls != 0 {
		t.Fatalf("semantic stream hit started provider stream %d times, want 0", f.streamCalls)
	}
	if len(deltas) != 1 || deltas[0] != "Paris is the capital of France." {
		t.Fatalf("stream deltas = %q, want one cached delta", deltas)
	}
	if resp == nil || resp.Content != "Paris is the capital of France." {
		t.Fatalf("stream response = %+v", resp)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "semantic" {
		t.Fatalf("stream LastCacheHit = (%q, %v), want (semantic, true)", kind, ok)
	}
}

func TestServeCacheFreshSuccessClearsCacheHit(t *testing.T) {
	f := &fakeEngine{chatResp: freshResp()}
	se, _, _, _ := wrapSemanticTest(f, serveCachePolicy())
	ctx := context.Background()

	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("exact-hit Chat: %v", err)
	}
	if _, _, ok := se.LastCacheHit(); !ok {
		t.Fatal("expected a cache hit on the repeat")
	}
	// A fresh provider success for a different prompt must clear it.
	f.chatResp = freshResp()
	if _, err := se.Chat(ctx, capitalReq("What is the capital of France?")); err != nil {
		t.Fatalf("fresh Chat: %v", err)
	}
	if _, _, ok := se.LastCacheHit(); ok {
		t.Fatal("fresh provider success must clear LastCacheHit")
	}
	if se.LastServedStale() {
		t.Fatal("fresh provider success must not be marked stale")
	}
}

// coalescingFakeEngine is a fakeEngine that also reports single-flight
// counters, so the wrapper's forwarding can be tested.
type coalescingFakeEngine struct {
	*fakeEngine
	stats coalescing.Stats
}

func (e *coalescingFakeEngine) CoalescingStats() coalescing.Stats { return e.stats }

func TestStaleEngineForwardsCoalescingStats(t *testing.T) {
	want := coalescing.Stats{Requests: 7, Flights: 2, Shared: 5, UpstreamAvoided: 5, QuotaSaved: 5, InFlight: 0}
	se, _ := wrapTest(&coalescingFakeEngine{fakeEngine: &fakeEngine{}, stats: want}, DefaultPolicy())
	if got := se.CoalescingStats(); got != want {
		t.Fatalf("CoalescingStats = %+v, want %+v", got, want)
	}
}

func TestStaleEngineCoalescingStatsEmptyWithoutReporter(t *testing.T) {
	se, _ := wrapTest(&fakeEngine{}, DefaultPolicy())
	if got := se.CoalescingStats(); got != (coalescing.Stats{}) {
		t.Fatalf("CoalescingStats = %+v, want all zero", got)
	}
}
