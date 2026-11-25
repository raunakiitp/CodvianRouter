// Panic isolation on the request/stream path (CHECKLIST 165-166,
// Upstream-gateway behaviour): a panic in a provider client, a chunk
// converter, or a hook must never kill the gateway. These tests
// deliberately panic inside the provider client / hook and prove
// the engine survives, the request fails over or ends with a
// terminal error, the ledger row exists, and the panic counter
// moved.
package router

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakePanicMetrics counts recovered panics by location for tests
// (the real metrics.Recorder is covered by its own package tests).
type fakePanicMetrics struct {
	mu     sync.Mutex
	counts map[string]int
}

func (m *fakePanicMetrics) IncPanic(location string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = make(map[string]int)
	}
	m.counts[location]++
}

func (m *fakePanicMetrics) total() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := 0
	for _, n := range m.counts {
		total += n
	}
	return total
}

func (m *fakePanicMetrics) at(location string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[location]
}

// panicClient is a provider.Client that panics in Chat and/or
// ChatStreamChunks, simulating a bug in a provider client or in a
// wire chunk converter turning a malformed upstream event into a
// panic. The panic value is deliberately a plain string — the
// server's redaction of richer values is covered in
// internal/server (TestPanicValueRedactedInLog).
func panicClient() *fakeClient {
	return &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		panic("simulated provider client panic")
	}}
}

// panicChunkClient streams the given chunks via onChunk and then
// panics, simulating a chunk converter that dies mid-stream.
func panicChunkClient(chunks []provider.StreamChunk) *prereadChunkClient {
	return &prereadChunkClient{chunkFn: func(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
		var sb strings.Builder
		for _, ck := range chunks {
			sb.WriteString(ck.Content)
			if err := onChunk(ck); err != nil {
				return nil, err
			}
		}
		panic("simulated chunk converter panic")
	}}
}

func panicChatReq(model string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    model,
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	}
}

// panicFirstChain orders the chain with the panicking provider
// first and the fallback second: the default chain order is
// sorted provider IDs, which would try "fallback" first and never
// reach the panicker.
func panicFirstChain(model string) []Attempt {
	return []Attempt{{ProviderID: "panicker", Model: model}, {ProviderID: "fallback", Model: model}}
}

// TestCallChatPanicFailsOverToNextProvider proves the headline
// behaviour: a provider whose client panics fails the attempt like
// any other error — the chain moves on, the fallback serves the
// request, and the panicked attempt is in the ledger as a failed
// row (zero tokens, zero cost).
func TestCallChatPanicFailsOverToNextProvider(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{
		"panicker": panicClient(),
		"fallback": okClient("fallback says hi"),
	}, led, Config{MaxAttempts: 3, ChainFor: panicFirstChain})
	eng.SetPanicMetrics(pm)

	resp, err := eng.Chat(context.Background(), panicChatReq("m"))
	if err != nil {
		t.Fatalf("Chat returned error: %v (want fallback success)", err)
	}
	if resp.Content != "fallback says hi" {
		t.Fatalf("Content = %q, want %q", resp.Content, "fallback says hi")
	}
	if pm.at("provider_chat") != 1 {
		t.Fatalf("provider_chat panics = %d, want 1", pm.at("provider_chat"))
	}
	if pm.total() != 1 {
		t.Fatalf("total panics = %d, want 1 (counted exactly once)", pm.total())
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if len(led.calls) != 2 {
		t.Fatalf("ledger rows = %d, want 2 (failed panicker + ok fallback)", len(led.calls))
	}
	if led.calls[0].provider != "panicker" || led.calls[0].ok {
		t.Fatalf("ledger[0] = %+v, want panicker/failed", led.calls[0])
	}
	if led.calls[0].usage != (provider.Usage{}) {
		t.Fatalf("ledger[0].usage = %+v, want zero usage", led.calls[0].usage)
	}
	if led.calls[1].provider != "fallback" || !led.calls[1].ok {
		t.Fatalf("ledger[1] = %+v, want fallback/ok", led.calls[1])
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2", trace)
	}
	if trace.Attempts[0].Reason != provider.ReasonPanicRecovered {
		t.Fatalf("trace[0].reason = %q, want %q", trace.Attempts[0].Reason, provider.ReasonPanicRecovered)
	}
}

// TestCallChatPanicSingleProviderReturnsPanicError proves the
// exhausted case: with no fallback, the caller gets a *PanicError
// (an ordinary error — the process is alive to return it), the
// ledger still has the failed row, and the panic is counted.
func TestCallChatPanicSingleProviderReturnsPanicError(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{"panicker": panicClient()}, led, Config{MaxAttempts: 3})
	eng.SetPanicMetrics(pm)

	_, err := eng.Chat(context.Background(), panicChatReq("m"))
	if err == nil {
		t.Fatal("Chat returned nil error, want a PanicError")
	}
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want *PanicError", err, err)
	}
	if pe.Location != "provider_chat" {
		t.Fatalf("PanicError.Location = %q, want provider_chat", pe.Location)
	}
	// The fixed Error() string never carries the recovered value.
	if got := pe.Error(); got != "router: panic recovered in provider_chat" {
		t.Fatalf("PanicError.Error() = %q", got)
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1 failed row", led.count())
	}
	if pm.at("provider_chat") != 1 {
		t.Fatalf("provider_chat panics = %d, want 1", pm.at("provider_chat"))
	}
}

// TestPanicErrorClassificationIsRetryable proves a recovered panic
// classifies as a retryable failure (failover), never as a terminal
// request error: the default taxonomy branch is what the chain
// relies on, so pin it.
func TestPanicErrorClassificationIsRetryable(t *testing.T) {
	cls := provider.ClassifyError(NewPanicError("provider_chat", "boom"), provider.EndpointChatCompletions)
	if cls.Decision != provider.DecisionFallback {
		t.Fatalf("ClassifyError decision = %v, want DecisionFallback", cls.Decision)
	}
}

// TestBridgeStreamPanicBeforeCommitFailsOver proves a chunk
// converter that panics before any content fails over: the client
// sees the fallback's content, never a partial body, and both
// attempts are in the ledger.
func TestBridgeStreamPanicBeforeCommitFailsOver(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{
		"panicker": panicChunkClient(nil),
		"fallback": prereadChunks([]provider.StreamChunk{{Content: "fallback stream"}}, nil),
	}, led, Config{MaxAttempts: 3, ChainFor: panicFirstChain})
	eng.SetPanicMetrics(pm)

	resp, err := eng.ChatStreamChunks(context.Background(), panicChatReq("m"), func(provider.StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("ChatStreamChunks returned error: %v (want fallback success)", err)
	}
	if resp.Content != "fallback stream" {
		t.Fatalf("Content = %q, want %q", resp.Content, "fallback stream")
	}
	if pm.at("provider_stream") != 1 {
		t.Fatalf("provider_stream panics = %d, want 1", pm.at("provider_stream"))
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if len(led.calls) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(led.calls))
	}
	if led.calls[0].provider != "panicker" || led.calls[0].ok {
		t.Fatalf("ledger[0] = %+v, want panicker/failed", led.calls[0])
	}
	if led.calls[1].provider != "fallback" || !led.calls[1].ok {
		t.Fatalf("ledger[1] = %+v, want fallback/ok", led.calls[1])
	}
}

// TestBridgeStreamPanicAfterCommitEndsStream proves a chunk
// converter that panics after content was delivered ends the
// stream with a terminal error — never a switch to another
// provider after commit (the preread rule), and never a dead
// gateway. The delivered prefix stays delivered; the failed
// attempt is in the ledger.
func TestBridgeStreamPanicAfterCommitEndsStream(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{
		"panicker": panicChunkClient([]provider.StreamChunk{{Content: "hel"}}),
		"fallback": prereadChunks([]provider.StreamChunk{{Content: "should never serve"}}, nil),
	}, led, Config{MaxAttempts: 3, ChainFor: panicFirstChain})
	eng.SetPanicMetrics(pm)

	var got strings.Builder
	_, err := eng.ChatStreamChunks(context.Background(), panicChatReq("m"), func(ck provider.StreamChunk) error {
		got.WriteString(ck.Content)
		return nil
	})
	if err == nil {
		t.Fatal("ChatStreamChunks returned nil error, want a terminal PanicError")
	}
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want *PanicError", err, err)
	}
	if pe.Location != "provider_stream" {
		t.Fatalf("PanicError.Location = %q, want provider_stream", pe.Location)
	}
	if got.String() != "hel" {
		t.Fatalf("delivered = %q, want %q (prefix stays, nothing after)", got.String(), "hel")
	}
	if led.count() != 1 {
		t.Fatalf("ledger rows = %d, want 1 failed row", led.count())
	}
	if pm.at("provider_stream") != 1 {
		t.Fatalf("provider_stream panics = %d, want 1", pm.at("provider_stream"))
	}
	// The fallback was never tried: no switch after commit.
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace attempts = %v, want exactly 1 (no failover after commit)", trace)
	}
}

// TestChatWithChainPanicInOrderHookRecovered proves a panicking
// routing hook is recovered at the engine boundary: the request
// gets an ordinary error (the process is alive to return it),
// the panic is counted once at engine_chat, and no attempt was
// recorded — no provider was called, so there is nothing to bill.
// (The server's backstop ledger row for this case is covered in
// internal/server: TestChatCompletionsHookPanicLeavesLedgerRow.)
func TestChatWithChainPanicInOrderHookRecovered(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{
		"a": okClient("a says hi"),
		"b": okClient("b says hi"),
	}, led, Config{
		MaxAttempts: 3,
		OrderHook: func(chain []Attempt) []Attempt {
			panic("simulated order-hook panic")
		},
	})
	eng.SetPanicMetrics(pm)

	_, err := eng.Chat(context.Background(), panicChatReq("m"))
	if err == nil {
		t.Fatal("Chat returned nil error, want a PanicError")
	}
	var pe *PanicError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T (%v), want *PanicError", err, err)
	}
	if pe.Location != "engine_chat" {
		t.Fatalf("PanicError.Location = %q, want engine_chat", pe.Location)
	}
	if pm.at("engine_chat") != 1 {
		t.Fatalf("engine_chat panics = %d, want 1", pm.at("engine_chat"))
	}
	if pm.total() != 1 {
		t.Fatalf("total panics = %d, want 1", pm.total())
	}
	if led.count() != 0 {
		t.Fatalf("ledger rows = %d, want 0 (no provider was called)", led.count())
	}
}

// TestRunStreamPanicInOrderHookRecovered is the streaming half of
// the hook test: a panicking order hook ends the stream with a
// terminal PanicError event, counted once at engine_stream.
func TestRunStreamPanicInOrderHookRecovered(t *testing.T) {
	led := &fakeLedger{}
	pm := &fakePanicMetrics{}
	eng := NewEngine(map[string]provider.Client{
		"a": prereadChunks([]provider.StreamChunk{{Content: "a stream"}}, nil),
	}, led, Config{
		MaxAttempts: 3,
		OrderHook: func(chain []Attempt) []Attempt {
			panic("simulated stream order-hook panic")
		},
	})
	eng.SetPanicMetrics(pm)

	ch, err := eng.ChatStreamWithChain(context.Background(), []Attempt{{ProviderID: "a", Model: "m"}}, panicChatReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain returned error: %v", err)
	}
	got, termErr := collectStream(t, ch)
	if termErr == nil {
		t.Fatalf("stream ended without error (got %q), want a terminal PanicError", got)
	}
	var pe *PanicError
	if !errors.As(termErr, &pe) {
		t.Fatalf("terminal err = %T (%v), want *PanicError", termErr, termErr)
	}
	if pe.Location != "engine_stream" {
		t.Fatalf("PanicError.Location = %q, want engine_stream", pe.Location)
	}
	if pm.at("engine_stream") != 1 {
		t.Fatalf("engine_stream panics = %d, want 1", pm.at("engine_stream"))
	}
	if led.count() != 0 {
		t.Fatalf("ledger rows = %d, want 0 (no provider was called)", led.count())
	}
}

// TestPanicMetricsNilSinkIsSafe proves the engine still recovers
// (and serves the fallback) when no panic-metrics sink is wired:
// counting is best-effort, never routing.
func TestPanicMetricsNilSinkIsSafe(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{
		"panicker": panicClient(),
		"fallback": okClient("fallback says hi"),
	}, &fakeLedger{}, Config{MaxAttempts: 3})
	resp, err := eng.Chat(context.Background(), panicChatReq("m"))
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.Content != "fallback says hi" {
		t.Fatalf("Content = %q, want fallback content", resp.Content)
	}
}

// TestEngineSurvivesRepeatedPanics proves the process stays up
// across panicking requests: ten panicking requests in a row all
// return errors (never a crash), and every one is counted. Each
// request targets a different provider (the model name IS the
// provider ID here) so the circuit breaker — which correctly
// opens after 5 consecutive failures on one provider — never
// masks a panic: every request genuinely reaches its panicking
// client.
func TestEngineSurvivesRepeatedPanics(t *testing.T) {
	pm := &fakePanicMetrics{}
	clients := make(map[string]provider.Client, 10)
	for i := 0; i < 10; i++ {
		clients["p"+strconv.Itoa(i)] = panicClient()
	}
	eng := NewEngine(clients, &fakeLedger{}, Config{
		MaxAttempts: 3,
		ChainFor: func(model string) []Attempt {
			return []Attempt{{ProviderID: model, Model: model}}
		},
	})
	eng.SetPanicMetrics(pm)
	for i := 0; i < 10; i++ {
		_, err := eng.Chat(context.Background(), panicChatReq("p"+strconv.Itoa(i)))
		if err == nil {
			t.Fatalf("request %d: nil error, want PanicError", i)
		}
		var pe *PanicError
		if !errors.As(err, &pe) {
			t.Fatalf("request %d: err = %T (%v), want *PanicError", i, err, err)
		}
	}
	if pm.at("provider_chat") != 10 {
		t.Fatalf("provider_chat panics = %d, want 10", pm.at("provider_chat"))
	}
}

// TestPanicErrorValueNeverLeaksIntoTrace proves the recovered
// value stays out of the route trace: the trace stores the
// PanicError's fixed Error() string, never the value — so a
// panic value carrying sensitive material can never reach the
// dashboard through the trace.
func TestPanicErrorValueNeverLeaksIntoTrace(t *testing.T) {
	secret := "sk-live-SECRET-VALUE-1234"
	led := &fakeLedger{}
	eng := NewEngine(map[string]provider.Client{
		"panicker": &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
			panic(secret)
		}},
	}, led, Config{MaxAttempts: 3})
	_, err := eng.Chat(context.Background(), panicChatReq("m"))
	if err == nil {
		t.Fatal("nil error, want PanicError")
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %v, want 1 attempt", trace)
	}
	if got := trace.Attempts[0].Error; got == "" {
		t.Fatal("trace attempt has no error recorded")
	} else if strings.Contains(got, secret) {
		t.Fatalf("trace error %q contains the panic value", got)
	}
}
