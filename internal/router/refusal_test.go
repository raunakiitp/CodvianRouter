// Content-policy refusal chain (CHECKLIST 161-162): a refusal
// is a verdict about the content, not about the target — so the
// refusing target is never retried, no health signal is touched,
// and the request routes on the separate refusal chain (or ends
// in an honest refusal when the chain runs dry).
package router

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// refusalErr is a 400 content-policy refusal, the shape OpenAI
// actually emits (code "content_filter").
func refusalErr() error {
	return &provider.UpstreamError{StatusCode: 400, Body: `{"error":{"message":"content filtered due to policy","type":"invalid_request_error","code":"content_filter"}}`}
}

func refusalClient() *fakeClient { return errClient(refusalErr()) }

func TestRefusalChainStatsEmpty(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("a")}, nil, testConfig())
	stats := eng.RefusalChainStats()
	if stats.Refusals != 0 || stats.ServedViaRefusal != 0 || stats.Exhausted != 0 {
		t.Fatalf("fresh stats = %+v, want all zero", stats)
	}
	if stats.LastProvider != "" || stats.LastModel != "" {
		t.Fatalf("fresh stats last = %q/%q, want empty", stats.LastProvider, stats.LastModel)
	}
}

func TestContentRefusalRoutesToNextTarget(t *testing.T) {
	a := refusalClient()
	b := okClient("bee")
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, led, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq())
	if err != nil {
		t.Fatalf("ChatWithChain: %v", err)
	}
	if resp.Content != "bee" {
		t.Fatalf("content = %q, want bee", resp.Content)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 {
		t.Fatalf("calls a=%d b=%d, want 1/1 (no same-target retry, one hop each)", a.chatCalls(), b.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	if got := trace.Attempts[0]; got.Outcome != "content_refused" || got.Reason != "content_refused" {
		t.Errorf("attempt 0 = outcome %q reason %q, want content_refused/content_refused", got.Outcome, got.Reason)
	}
	if got := trace.Attempts[1]; got.Outcome != "success" {
		t.Errorf("attempt 1 outcome = %q, want success", got.Outcome)
	}
	// The refusal is a failed ledger row (attempt happened, nothing
	// served); the serve is a success row.
	if led.count() != 2 {
		t.Fatalf("ledger rows = %d, want 2", led.count())
	}
	if led.calls[0].ok {
		t.Errorf("ledger row 0 ok = true, want false (refusal served nothing)")
	}
	if !led.calls[1].ok {
		t.Errorf("ledger row 1 ok = false, want true")
	}
	stats := eng.RefusalChainStats()
	if stats.Refusals != 1 || stats.ServedViaRefusal != 1 || stats.Exhausted != 0 {
		t.Errorf("stats = %+v, want refusals=1 served=1 exhausted=0", stats)
	}
	if stats.LastProvider != "a" || stats.LastModel != "m" {
		t.Errorf("last refusal = %q/%q, want a/m", stats.LastProvider, stats.LastModel)
	}
}

func TestContentRefusalDoesNotRetrySameTarget(t *testing.T) {
	a := refusalClient()
	cfg := testConfig()
	cfg.MaxAttempts = 5
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, cfg)
	_, trace, err := eng.ChatWithChain(context.Background(), chainFor("a"), testReq())
	if !errors.Is(err, ErrContentRefused) {
		t.Fatalf("err = %v, want ErrContentRefused", err)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("a chat calls = %d, want 1 (a refusal is never retried on the same target)", a.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "content_refused" {
		t.Fatalf("trace = %+v, want 1 content_refused attempt", trace)
	}
	if stats := eng.RefusalChainStats(); stats.Refusals != 1 || stats.Exhausted != 1 {
		t.Errorf("stats = %+v, want refusals=1 exhausted=1", stats)
	}
}

func TestContentRefusalDoesNotTripBreakerOrCountAsHealthFailure(t *testing.T) {
	a := refusalClient()
	b := okClient("bee")
	cfg := testConfig()
	cfg.BreakerThreshold = 1 // one failure would open the breaker, if a refusal counted
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	if _, _, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq()); err != nil {
		t.Fatalf("request 1: %v", err)
	}
	// No health signal was recorded for the refusing target.
	if _, ok := eng.ProviderStats("a"); ok {
		t.Errorf("ProviderStats(a) exists after a refusal, want none (a refusal is not a health failure)")
	}
	if len(eng.AllProviderStats()) != 1 || eng.AllProviderStats()[0].ProviderID != "b" {
		t.Errorf("AllProviderStats = %+v, want only b (the serving target)", eng.AllProviderStats())
	}
	if eng.onCooldown("a") {
		t.Errorf("a is on cooldown after a refusal, want no cooldown")
	}
	// The refusing target is still routable: replace its client
	// with a serving one and route to it alone. A tripped breaker
	// would skip it (skipped_breaker) and the request would fail.
	eng.AddClient("a", okClient("aye"))
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a"), testReq())
	if err != nil {
		t.Fatalf("request 2 (a now serving): %v, trace %+v", err, trace)
	}
	if resp.Content != "aye" {
		t.Fatalf("request 2 content = %q, want aye", resp.Content)
	}
}

func TestContentRefusalChainExhaustedIsHonest(t *testing.T) {
	a := refusalClient()
	b := refusalClient()
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq())
	if !errors.Is(err, ErrContentRefused) {
		t.Fatalf("err = %v, want ErrContentRefused", err)
	}
	if resp != nil {
		t.Fatalf("resp = %+v, want nil (never a fake success)", resp)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 {
		t.Fatalf("calls a=%d b=%d, want 1/1 (each target once, no retries)", a.chatCalls(), b.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	for i, att := range trace.Attempts {
		if att.Outcome != "content_refused" || att.Reason != "content_refused" {
			t.Errorf("attempt %d = outcome %q reason %q, want content_refused/content_refused", i, att.Outcome, att.Reason)
		}
	}
	if stats := eng.RefusalChainStats(); stats.Refusals != 2 || stats.ServedViaRefusal != 0 || stats.Exhausted != 1 {
		t.Errorf("stats = %+v, want refusals=2 served=0 exhausted=1", stats)
	}
}

func TestRefusalChainConfiguredSeparately(t *testing.T) {
	a := refusalClient()
	b := okClient("bee")
	c := okClient("cee")
	cfg := testConfig()
	// The failure chain order is a, b, c — but the operator's
	// refusal chain names c first: after a refuses, c is tried
	// before b (the target known to accept this content class).
	cfg.RefusalChainFor = func(model string) []Attempt {
		if model != "m" {
			return nil
		}
		return []Attempt{{ProviderID: "c", Model: "m"}, {ProviderID: "b", Model: "m"}}
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, cfg)
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b", "c"), coolReq("m"))
	if err != nil || resp.Content != "cee" {
		t.Fatalf("content = %q err = %v, want cee/nil", resp.Content, err)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b chat calls = %d, want 0 (refusal chain tried c first and it served)", b.chatCalls())
	}
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[1].Provider != "c" {
		t.Fatalf("trace = %+v, want [a refused, c served]", trace)
	}
}

func TestRefusalChainEmptyConfiguredEndsAtOnce(t *testing.T) {
	a := refusalClient()
	b := okClient("bee")
	cfg := testConfig()
	// An explicitly empty configured chain means "no refusal
	// routing for this model": the first refusal ends the
	// request at once, without dialling b.
	cfg.RefusalChainFor = func(model string) []Attempt { return []Attempt{} }
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	_, _, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq())
	if !errors.Is(err, ErrContentRefused) {
		t.Fatalf("err = %v, want ErrContentRefused", err)
	}
	if b.chatCalls() != 0 {
		t.Fatalf("b chat calls = %d, want 0 (empty refusal chain dials nothing)", b.chatCalls())
	}
	if stats := eng.RefusalChainStats(); stats.Exhausted != 1 {
		t.Errorf("stats = %+v, want exhausted=1", stats)
	}
}

func TestRefusalChainSkipsAlreadyTriedConfiguredTargets(t *testing.T) {
	a := refusalClient()
	b := refusalClient()
	c := okClient("cee")
	cfg := testConfig()
	// The configured chain names a again first: a already
	// refused, so it must be skipped, not re-asked.
	cfg.RefusalChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, cfg)
	resp, _, err := eng.ChatWithChain(context.Background(), chainFor("a", "b", "c"), testReq())
	if err != nil || resp.Content != "cee" {
		t.Fatalf("content = %q err = %v, want cee/nil", resp.Content, err)
	}
	if a.chatCalls() != 1 {
		t.Fatalf("a chat calls = %d, want 1 (never re-asked after refusing)", a.chatCalls())
	}
	if b.chatCalls() != 1 || c.chatCalls() != 1 {
		t.Fatalf("calls b=%d c=%d, want 1/1", b.chatCalls(), c.chatCalls())
	}
}

func TestContentFilterFinishIsRefusal(t *testing.T) {
	// A content_filter finish WITH partial content is still a
	// refusal: delivering the filtered fragment as an answer
	// would be a fake partial success.
	a := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "partial", FinishReason: "content_filter"}, nil
	}}
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq())
	if err != nil || resp.Content != "bee" {
		t.Fatalf("content = %q err = %v, want bee/nil", resp.Content, err)
	}
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "content_refused" {
		t.Fatalf("trace = %+v, want [a content_refused, b success]", trace)
	}
	if _, ok := eng.ProviderStats("a"); ok {
		t.Errorf("ProviderStats(a) exists after a content_filter finish, want none")
	}
}

func TestContentRefusalSecondRefusalInChain(t *testing.T) {
	// Two refusals in a row keep routing: a refuses, b refuses,
	// c serves — each exactly once.
	a := refusalClient()
	b := refusalClient()
	c := okClient("cee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b", "c"), testReq())
	if err != nil || resp.Content != "cee" {
		t.Fatalf("content = %q err = %v, want cee/nil", resp.Content, err)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 || c.chatCalls() != 1 {
		t.Fatalf("calls a=%d b=%d c=%d, want 1/1/1", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	if stats := eng.RefusalChainStats(); stats.Refusals != 2 || stats.ServedViaRefusal != 1 {
		t.Errorf("stats = %+v, want refusals=2 served=1", stats)
	}
	if trace == nil || len(trace.Attempts) != 3 {
		t.Fatalf("trace = %+v, want 3 attempts", trace)
	}
}

func TestContentRefusalTypedError(t *testing.T) {
	// A typed refusal (a blocked prompt surfaced as an error,
	// not an HTTP body) routes the same way.
	a := errClient(&provider.ContentRefusalError{StatusCode: 400, Message: "blocked prompt"})
	b := okClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, trace, err := eng.ChatWithChain(context.Background(), chainFor("a", "b"), testReq())
	if err != nil || resp.Content != "bee" {
		t.Fatalf("content = %q err = %v, want bee/nil", resp.Content, err)
	}
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "content_refused" {
		t.Fatalf("trace = %+v, want [a content_refused, b success]", trace)
	}
}

// refusalChunkClient is a fake provider.Client that streams
// chunk-aware, so a finish reason survives the bridge (the
// text-only ChatStream path drops it — see bridgeStream).
type refusalChunkClient struct {
	mu      sync.Mutex
	streams int
	chunkFn func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error)
}

func (c *refusalChunkClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *refusalChunkClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *refusalChunkClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.streams++
	fn := c.chunkFn
	c.mu.Unlock()
	if fn != nil {
		return fn(ctx, req, onChunk)
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (c *refusalChunkClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *refusalChunkClient) TestKey(ctx context.Context) error                { return nil }

func (c *refusalChunkClient) streamCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams
}

// contentFilterChunkClient streams a single finish-reason-only
// chunk: the upstream ended the stream on its content filter
// without sending any content.
func contentFilterChunkClient() *refusalChunkClient {
	return &refusalChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{FinishReason: "content_filter"}); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "", FinishReason: "content_filter"}, nil
	}}
}

// servingChunkClient streams one content chunk with the given text.
func servingChunkClient(text string) *refusalChunkClient {
	return &refusalChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		if err := onChunk(provider.StreamChunk{Content: text}); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: text, FinishReason: "stop"}, nil
	}}
}

func TestContentRefusalStreamEmptyRoutesOn(t *testing.T) {
	// Streaming: a stream that ends empty on a content_filter
	// finish is a refusal-chain hop, not a failure-chain one —
	// the next target serves, and no health signal is touched.
	a := contentFilterChunkClient()
	b := servingChunkClient("bee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), chainFor("a", "b"), testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got string
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
		got += ev.Delta
	}
	if streamErr != nil {
		t.Fatalf("stream err = %v", streamErr)
	}
	if got != "bee" {
		t.Fatalf("stream content = %q, want bee", got)
	}
	if a.streamCalls() != 1 || b.streamCalls() != 1 {
		t.Fatalf("stream calls a=%d b=%d, want 1/1", a.streamCalls(), b.streamCalls())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "content_refused" {
		t.Fatalf("trace = %+v, want [a content_refused, b success]", trace)
	}
	if _, ok := eng.ProviderStats("a"); ok {
		t.Errorf("ProviderStats(a) exists after a refused stream, want none")
	}
	if stats := eng.RefusalChainStats(); stats.Refusals != 1 || stats.ServedViaRefusal != 1 {
		t.Errorf("stats = %+v, want refusals=1 served=1", stats)
	}
}

func TestContentRefusalStreamExhaustedIsHonest(t *testing.T) {
	a := contentFilterChunkClient()
	b := contentFilterChunkClient()
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	ch, err := eng.ChatStreamWithChain(context.Background(), chainFor("a", "b"), testReq())
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if !errors.Is(streamErr, ErrContentRefused) {
		t.Fatalf("stream err = %v, want ErrContentRefused", streamErr)
	}
	if stats := eng.RefusalChainStats(); stats.Refusals != 2 || stats.Exhausted != 1 {
		t.Errorf("stats = %+v, want refusals=2 exhausted=1", stats)
	}
}

func TestIsContentRefusalErr(t *testing.T) {
	if !isContentRefusalErr(refusalErr()) {
		t.Errorf("400 content_filter error not recognised as a refusal")
	}
	if !isContentRefusalErr(&provider.ContentRefusalError{Message: "blocked"}) {
		t.Errorf("typed ContentRefusalError not recognised as a refusal")
	}
	if !isContentRefusalErr(&provider.EmptyStreamError{FinishReason: "content_filter"}) {
		t.Errorf("empty stream with content_filter finish not recognised as a refusal")
	}
	if isContentRefusalErr(&provider.UpstreamError{StatusCode: 400, Body: "temperature must be <= 1"}) {
		t.Errorf("malformed 400 recognised as a refusal, want false")
	}
	if isContentRefusalErr(&provider.UpstreamError{StatusCode: 500, Body: "boom"}) {
		t.Errorf("500 recognised as a refusal, want false")
	}
	if isContentRefusalErr(&provider.EmptyStreamError{FinishReason: "stop"}) {
		t.Errorf("empty stream with stop finish recognised as a refusal, want false")
	}
	if isContentRefusalErr(nil) {
		t.Errorf("nil recognised as a refusal, want false")
	}
}
