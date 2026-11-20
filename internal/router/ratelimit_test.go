package router

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/adaptive"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M5.13: the engine learns upstream rate-limit headers from
// responses (and 429s) and paces the adaptive limiter from them.
func TestEngineLearnsRateLimitFromChatResponse(t *testing.T) {
	now := time.Now()
	sig := &adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 10, ObservedAt: now,
	}
	eng := NewEngine(map[string]provider.Client{
		"a": &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "hi", RateLimit: sig}, nil
		}},
	}, nil, testConfig())
	if _, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), coolReq("m")); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	states := eng.ConcurrencyStats()
	if len(states) != 1 || states[0].Provider != "a" {
		t.Fatalf("ConcurrencyStats = %+v, want 1 entry for a", states)
	}
	// 10% remaining paces the default-20 cap to MaxLimit*0.1 = 10,
	// and the successful sample's Gradient2 queue term (+1) then
	// grows it to 11 — the pacing is visible in PacedLimit.
	if states[0].Limit != 11 {
		t.Errorf("limiter cap after learning = %v, want 11 (10 paced + 1 queue growth)", states[0].Limit)
	}
	if states[0].PacedLimit != 10 {
		t.Errorf("PacedLimit = %v, want 10", states[0].PacedLimit)
	}
}

func TestEngineLearnsRateLimitFrom429(t *testing.T) {
	now := time.Now()
	sig := &adaptive.RateLimitSignals{
		HasRequests: true, RequestsLimit: 100, RequestsRemaining: 0, ObservedAt: now,
	}
	eng := NewEngine(map[string]provider.Client{
		"a": errClient(&provider.RateLimitError{RetryAfter: time.Second, Body: "slow", Signals: sig}),
		"b": okClient("hi"),
	}, nil, testConfig())
	resp, _, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi (failover still serves)", resp.Content)
	}
	// 0% remaining paces a's cap to the floor (2) — the 429 taught
	// the router before the next request.
	for _, st := range eng.ConcurrencyStats() {
		if st.Provider == "a" && st.Limit != adaptive.DefaultMinLimit {
			t.Errorf("a's cap after 429 learning = %v, want floor %v", st.Limit, adaptive.DefaultMinLimit)
		}
	}
}

func TestEngineLearnsRateLimitFromStream(t *testing.T) {
	now := time.Now()
	sig := &adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 5, ObservedAt: now,
	}
	eng := NewEngine(map[string]provider.Client{
		"a": &rateLimitStreamClient{sig: sig},
	}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	for range ch {
	}
	// 5% remaining paces the cap to MaxLimit*0.05 = 5.
	for _, st := range eng.ConcurrencyStats() {
		if st.Provider == "a" && st.Limit != 5 {
			t.Errorf("a's cap after stream learning = %v, want 5", st.Limit)
		}
	}
}

// rateLimitStreamClient is a fake streaming client whose
// reassembled response carries rate-limit signals, the way the
// wire clients attach the stream-open headers.
type rateLimitStreamClient struct {
	sig *adaptive.RateLimitSignals
}

func (c *rateLimitStreamClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi", RateLimit: c.sig}, nil
}

func (c *rateLimitStreamClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if err := onDelta("hello"); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: "hello", RateLimit: c.sig}, nil
}

func (c *rateLimitStreamClient) ListModels(ctx context.Context) ([]string, error) {
	return nil, nil
}

func (c *rateLimitStreamClient) TestKey(ctx context.Context) error { return nil }

// recordingRateLimitStore records LearnRateLimit calls for tests.
// The engine persists in a goroutine, so the test waits for the
// call with a deadline instead of assuming it already happened.
type recordingRateLimitStore struct {
	mu    sync.Mutex
	calls []recordedLearn
	done  chan struct{}
}

type recordedLearn struct {
	providerID, keyID string
	sig               adaptive.RateLimitSignals
}

func (s *recordingRateLimitStore) LearnRateLimit(ctx context.Context, providerID, keyID string, sig adaptive.RateLimitSignals, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedLearn{providerID: providerID, keyID: keyID, sig: sig})
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return nil
}

func TestEngineLearnRateLimitPersistsPerKey(t *testing.T) {
	now := time.Now()
	sig := &adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 10, ObservedAt: now,
	}
	store := &recordingRateLimitStore{done: make(chan struct{})}
	cfg := testConfig()
	cfg.RateLimitStore = store
	eng := NewEngine(map[string]provider.Client{
		"a": &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "hi", RateLimit: sig}, nil
		}},
	}, nil, cfg)
	if _, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), coolReq("m")); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	select {
	case <-store.done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the async persist")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.calls) != 1 {
		t.Fatalf("persist calls = %d, want 1", len(store.calls))
	}
	call := store.calls[0]
	if call.providerID != "a" || call.keyID != "" {
		t.Errorf("persist call = %+v, want provider a with the attempt's key id", call)
	}
	if !call.sig.HasTokens || call.sig.TokensRemaining != 10 {
		t.Errorf("persisted signal = %+v, want tokens 100/10", call.sig)
	}
}

func TestEngineLearnRateLimitNilStoreNoPanic(t *testing.T) {
	// No store configured: learning still paces the limiter, it
	// just is not persisted.
	now := time.Now()
	sig := &adaptive.RateLimitSignals{
		HasTokens: true, TokensLimit: 100, TokensRemaining: 10, ObservedAt: now,
	}
	eng := NewEngine(map[string]provider.Client{
		"a": &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "hi", RateLimit: sig}, nil
		}},
	}, nil, testConfig())
	if _, _, err := eng.ChatWithChain(context.Background(), chainFor("a"), coolReq("m")); err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	// Paced to 10, then +1 queue growth from the successful sample.
	if states := eng.ConcurrencyStats(); len(states) != 1 || states[0].Limit != 11 || states[0].PacedLimit != 10 {
		t.Errorf("ConcurrencyStats = %+v, want a capped at 11 with PacedLimit 10", states)
	}
}
