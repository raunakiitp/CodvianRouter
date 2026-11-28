// A13 Stream A (gap H1): a BARE router.Engine consults the cache.
//
// Before this slice, caching was a property of the serve-mode assembly: the
// degrade wrapper and the server relay consulted the caches, and a bare
// Engine consulted nothing. These tests drive `NewEngine` directly — no
// wrapper, no server — so they fail if the engine's own consultation is
// removed.
//
// FAILS-WHEN-OFF: set cfg.ServeCache = false (or delete the
// engineCacheLookup call in chatWithChainStrategy) and every "served from
// cache / provider not dialled" assertion fails, because the request always
// reaches the provider.
package router

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// cacheTestEngine builds a bare engine with one provider and both cache
// layers wired, ServeCache on.
func cacheTestEngine(t *testing.T, exact cache.ExactStore, semantic cache.SemanticStore) (*Engine, *fakeClient) {
	t.Helper()
	fc := okClient("live-answer")
	cfg := testConfig()
	cfg.ServeCache = true
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}}
	}
	eng := NewEngine(map[string]provider.Client{"a": fc}, nil, cfg)
	eng.SetCaches(exact, semantic)
	return eng, fc
}

// TestBareEngineServesExactCacheHit: the first identical request dials the
// provider and stores; the second is served from the engine's own exact
// layer with ZERO extra provider calls.
func TestBareEngineServesExactCacheHit(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "tenant-1", Temperature: 0,
		Messages: []provider.Message{{Role: "user", Content: "what is a mutex?"}}}

	first, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if first.Content != "live-answer" {
		t.Fatalf("first content = %q, want the live answer", first.Content)
	}
	if got := fc.chatCalls(); got != 1 {
		t.Fatalf("provider calls after the first request = %d, want 1", got)
	}

	second, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if second.Content != "live-answer" {
		t.Fatalf("second content = %q, want the cached answer", second.Content)
	}
	if got := fc.chatCalls(); got != 1 {
		t.Fatalf("provider calls = %d after a repeated identical request, want still 1 (the engine must serve its own cache without dialling)", got)
	}
	if hit := eng.LastCacheHit(); hit != "exact" {
		t.Errorf("LastCacheHit() = %q, want exact", hit)
	}
}

// TestBareEngineCacheTenantIsolation: another tenant's identical prompt must
// MISS the exact layer — the tenant is part of the key.
func TestBareEngineCacheTenantIsolation(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	base := provider.ChatRequest{Model: "m", Temperature: 0,
		Messages: []provider.Message{{Role: "user", Content: "same prompt"}}}

	a := base
	a.CallerID = "tenant-a"
	if _, err := eng.Chat(context.Background(), a); err != nil {
		t.Fatalf("tenant-a Chat: %v", err)
	}
	b := base
	b.CallerID = "tenant-b"
	if _, err := eng.Chat(context.Background(), b); err != nil {
		t.Fatalf("tenant-b Chat: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (tenant-b must not be served tenant-a's entry)", got)
	}
	if hit := eng.LastCacheHit(); hit != "" {
		t.Errorf("LastCacheHit() = %q for the cross-tenant request, want no hit", hit)
	}
}

// TestBareEngineCacheModelIsolation: a different model never hits another
// model's entry (a claude answer must never answer a gpt request).
func TestBareEngineCacheModelIsolation(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	one := provider.ChatRequest{Model: "model-a", CallerID: "t", Temperature: 0,
		Messages: []provider.Message{{Role: "user", Content: "same prompt"}}}
	two := one
	two.Model = "model-b"
	if _, err := eng.Chat(context.Background(), one); err != nil {
		t.Fatalf("model-a Chat: %v", err)
	}
	if _, err := eng.Chat(context.Background(), two); err != nil {
		t.Fatalf("model-b Chat: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (model-b must not hit model-a's entry)", got)
	}
}

// TestBareEngineCacheBypassHeader: a bypass request neither reads nor writes.
func TestBareEngineCacheBypassHeader(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0,
		CacheControls: &provider.CacheControls{Bypass: true},
		Messages:      []provider.Message{{Role: "user", Content: "bypass me"}}}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("bypass Chat: %v", err)
	}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("bypass Chat #2: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d for bypassed requests, want 2 (a bypass must never be served from cache)", got)
	}
}

// TestBareEngineCacheRefusesNonZeroTemperature: the temperature gate holds on
// the engine path exactly as on the wrapper path.
func TestBareEngineCacheRefusesNonZeroTemperature(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0.7,
		Messages: []provider.Message{{Role: "user", Content: "hot prompt"}}}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat #2: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (temperature>0 is never cached)", got)
	}
}

// TestBareEngineCacheRefusesToolRequests: a tool-carrying request is not
// cacheable at all.
func TestBareEngineCacheRefusesToolRequests(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0,
		Tools:    []provider.ToolDefinition{{Type: "function"}},
		Messages: []provider.Message{{Role: "user", Content: "weather?"}}}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat #2: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (tool-carrying requests are not cacheable)", got)
	}
}

// TestBareEngineCacheRefusesRoutingContract: a contracted request must
// actually route, never be served from cache.
func TestBareEngineCacheRefusesRoutingContract(t *testing.T) {
	exact := cache.NewExactCache(16)
	eng, fc := cacheTestEngine(t, exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0,
		Routing:  &provider.RequestRouting{Group: ""},
		Messages: []provider.Message{{Role: "user", Content: "contracted"}}}
	req.Routing.Order = []string{"a"}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat #2: %v", err)
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 (a routing contract must never be served from cache)", got)
	}
}

// TestBareEngineCacheStreamHitServesWithoutDialling: the streaming path
// consults the same engine cache and serves a hit as a normal stream.
func TestBareEngineCacheStreamHitServesWithoutDialling(t *testing.T) {
	exact := cache.NewExactCache(16)
	fc := okClient("streamed-answer")
	fc.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("streamed-answer"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "streamed-answer", FinishReason: "stop"}, nil
	}
	cfg := testConfig()
	cfg.ServeCache = true
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": fc}, nil, cfg)
	eng.SetCaches(exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0,
		Messages: []provider.Message{{Role: "user", Content: "stream me"}}}

	// Prime the cache through the non-streaming path.
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("prime Chat: %v", err)
	}
	before := fc.chatCalls()

	streamReq := req
	streamReq.Stream = true
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, streamReq)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var text string
	sawDone := false
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("stream event error: %v", ev.Err)
		}
		text += ev.Delta
		if ev.Done {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatal("cache-hit stream never emitted Done")
	}
	if text != "streamed-answer" {
		t.Fatalf("streamed text = %q, want the cached answer", text)
	}
	if got := fc.chatCalls(); got != before {
		t.Fatalf("provider calls = %d (was %d): a stream cache hit must not dial", got, before)
	}
}

// TestBareEngineCacheStoresStreamedResponse: a completed engine stream is
// stored, so the next request hits it.
func TestBareEngineCacheStoresStreamedResponse(t *testing.T) {
	exact := cache.NewExactCache(16)
	fc := okClient("streamed-once")
	fc.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if onDelta != nil {
			if err := onDelta("streamed-once"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "streamed-once", FinishReason: "stop"}, nil
	}
	cfg := testConfig()
	cfg.ServeCache = true
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": fc}, nil, cfg)
	eng.SetCaches(exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0, Stream: true,
		Messages: []provider.Message{{Role: "user", Content: "stream and cache"}}}
	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("first stream error: %v", ev.Err)
		}
	}
	if got := fc.chatCalls() + fc.streamCalls(); got != 1 {
		t.Fatalf("provider calls = %d, want 1 for the first stream", got)
	}

	// The same request, non-streaming: must now be served from cache.
	chatReq := req
	chatReq.Stream = false
	got, err := eng.Chat(context.Background(), chatReq)
	if err != nil {
		t.Fatalf("Chat after stream: %v", err)
	}
	if got.Content != "streamed-once" {
		t.Fatalf("content = %q, want the streamed answer served from cache", got.Content)
	}
	if calls := fc.chatCalls() + fc.streamCalls(); calls != 1 {
		t.Fatalf("provider calls = %d, want still 1 (the completed stream must have been stored)", calls)
	}
}

// TestBareEngineNoCacheWhenServeOff: with ServeCache off (the default) the
// engine behaves exactly as before — no lookup, no store.
func TestBareEngineNoCacheWhenServeOff(t *testing.T) {
	exact := cache.NewExactCache(16)
	fc := okClient("answer")
	cfg := testConfig()
	cfg.ServeCache = false
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": fc}, nil, cfg)
	eng.SetCaches(exact, nil)

	req := provider.ChatRequest{Model: "m", CallerID: "t", Temperature: 0,
		Messages: []provider.Message{{Role: "user", Content: "x"}}}
	for i := 0; i < 2; i++ {
		if _, err := eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat #%d: %v", i, err)
		}
	}
	if got := fc.chatCalls(); got != 2 {
		t.Fatalf("provider calls = %d, want 2 with ServeCache off", got)
	}
	if n := exact.Size(); n != 0 {
		t.Fatalf("exact cache entries = %d, want 0 with ServeCache off", n)
	}
}

// keep the errors import honest
var _ = errors.New
