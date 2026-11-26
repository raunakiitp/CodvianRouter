// Panic isolation on the request/stream path (CHECKLIST 165-166,
// upstream-gateway behaviour): a panic in a provider client, a chunk
// converter, a hook, or the engine itself must never kill the
// gateway process. These tests drive the real HTTP handlers with
// deliberately panicking clients/engines and prove the server
// survives, the client gets a terminal error frame (stream) or an
// honest 502 (non-stream), the ledger row exists, the panic is
// counted in /metrics and in the dashboard overview feed, and the
// panic value never leaks into logs or responses.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// panicTestLedger is an in-memory ledger for the engine under
// test: it implements router.Ledger (RecordAttempt) and
// router.KeyedLedger (RecordAttemptForKey) so tests can assert
// exactly which attempts the engine recorded, including failed
// rows for panicked attempts.
type panicTestLedger struct {
	mu   sync.Mutex
	rows []panicLedgerRow
}

type panicLedgerRow struct {
	keyID    string
	provider string
	model    string
	ok       bool
	usage    provider.Usage
}

func (l *panicTestLedger) RecordAttempt(providerID, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.RecordAttemptForKey("", providerID, model, ok, latencyMs, usage)
}

func (l *panicTestLedger) RecordAttemptForKey(keyID, providerID, model string, ok bool, latencyMs int64, usage provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, panicLedgerRow{keyID: keyID, provider: providerID, model: model, ok: ok, usage: usage})
}

func (l *panicTestLedger) snapshot() []panicLedgerRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]panicLedgerRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// panicChatClient is a provider.Client that panics in Chat and in
// ChatStreamChunks (after emitting the configured chunks),
// simulating a bug in a provider client or in a wire chunk
// converter. okClient answers normally (fallback target).
type panicChatClient struct {
	chunks []string
}

func (c *panicChatClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	panic("simulated provider client panic")
}

func (c *panicChatClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	panic("simulated provider stream panic")
}

func (c *panicChatClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	for _, ck := range c.chunks {
		if err := onChunk(provider.StreamChunk{Content: ck}); err != nil {
			return nil, err
		}
	}
	panic("simulated chunk converter panic")
}

func (c *panicChatClient) ListModels(ctx context.Context) ([]string, error) {
	return []string{"m"}, nil
}

func (c *panicChatClient) TestKey(ctx context.Context) error { return nil }

type okChatClient struct{ content string }

func (c *okChatClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: c.content, Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}, nil
}

func (c *okChatClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if err := onDelta(c.content); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: c.content, Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}, nil
}

func (c *okChatClient) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if err := onChunk(provider.StreamChunk{Content: c.content}); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: c.content, Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}, nil
}

func (c *okChatClient) ListModels(ctx context.Context) ([]string, error) {
	return []string{"m"}, nil
}

func (c *okChatClient) TestKey(ctx context.Context) error { return nil }

// panicFirstChain orders the chain with the panicking provider
// first and the fallback second (the default chain order is
// sorted provider IDs, which would try "fallback" first).
func panicFirstChain(model string) []router.Attempt {
	return []router.Attempt{{ProviderID: "panicker", Model: model}, {ProviderID: "fallback", Model: model}}
}

// panicTestServer wires a server with a real ledger (the server's
// backstop rows land there), a recorder, and the given engine.
func panicTestServer(t *testing.T, eng Engine) (*Server, *metrics.Recorder, *ledger.Ledger) {
	t.Helper()
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { led.Close() })
	rec := metrics.NewRecorder()
	srv := newTestServer(t, eng, WithLedger(led), WithMetrics(rec))
	return srv, rec, led
}

func panicPost(t *testing.T, srv *Server, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":"hello"}]}`
	if stream {
		body = `{"model":"m","messages":[{"role":"user","content":"hello"}],"stream":true}`
	}
	return doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
}

func panicOverview(t *testing.T, srv *Server) overviewResponse {
	t.Helper()
	var ov overviewResponse
	getJSON(t, srv, "/dashboard/overview?range=24h", &ov)
	return ov
}

func panicMetricsBody(t *testing.T, srv *Server) string {
	t.Helper()
	// /metrics is key-protected, like every other route in the
	// test server (static key "test-key-123").
	rec := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func panicRecentEvents(t *testing.T, led *ledger.Ledger) []ledger.UsageEvent {
	t.Helper()
	evs, err := led.RecentEvents(context.Background(), 100, ledger.LogAll)
	if err != nil {
		t.Fatalf("RecentEvents: %v", err)
	}
	return evs
}

// TestChatCompletionsSurvivesPanickingProvider is the headline
// end-to-end proof: a provider whose client panics fails over to
// the fallback, the client gets a 200 with the fallback's
// content, both attempts are in the engine ledger, the panic is
// counted in /metrics and in the overview feed, and a second
// request still works — the process never died.
func TestChatCompletionsSurvivesPanickingProvider(t *testing.T) {
	engLedger := &panicTestLedger{}
	eng := router.NewEngine(map[string]provider.Client{
		"panicker": &panicChatClient{},
		"fallback": &okChatClient{content: "fallback says hi"},
	}, engLedger, router.Config{MaxAttempts: 3, ChainFor: panicFirstChain})
	srv, rec, _ := panicTestServer(t, eng)
	eng.SetPanicMetrics(rec)

	w := panicPost(t, srv, false)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200 (fallback should serve)\nbody: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, w.Body.String())
	}
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in response: %s", w.Body.String())
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "fallback says hi" {
		t.Fatalf("content = %v, want %q", msg["content"], "fallback says hi")
	}

	rows := engLedger.snapshot()
	if len(rows) != 2 {
		t.Fatalf("engine ledger rows = %d, want 2 (failed panicker + ok fallback): %+v", len(rows), rows)
	}
	if rows[0].provider != "panicker" || rows[0].ok {
		t.Fatalf("ledger[0] = %+v, want panicker/failed", rows[0])
	}
	if rows[0].usage != (provider.Usage{}) {
		t.Fatalf("ledger[0].usage = %+v, want zero usage", rows[0].usage)
	}
	if rows[1].provider != "fallback" || !rows[1].ok {
		t.Fatalf("ledger[1] = %+v, want fallback/ok", rows[1])
	}

	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="provider_chat"} 1`) {
		t.Fatalf("metrics missing provider_chat panic series:\n%s", got)
	}
	if ov := panicOverview(t, srv); ov.Panics != 1 {
		t.Fatalf("overview panics_since_start = %d, want 1", ov.Panics)
	}

	// The process is still up: a second request works.
	w2 := panicPost(t, srv, false)
	if w2.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200", w2.Code)
	}
}

// TestChatCompletionsPanicSingleProvider502AndLedgerRow proves the
// exhausted case over HTTP: no fallback exists, so the client
// gets an honest 502 (never a hang, never a crash), the failed
// attempt is in the engine ledger, the panic is counted, and a
// second request still gets a (502) answer — the server survived.
func TestChatCompletionsPanicSingleProvider502AndLedgerRow(t *testing.T) {
	engLedger := &panicTestLedger{}
	eng := router.NewEngine(map[string]provider.Client{"panicker": &panicChatClient{}}, engLedger,
		router.Config{MaxAttempts: 3, ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "panicker", Model: model}}
		}})
	srv, rec, _ := panicTestServer(t, eng)
	eng.SetPanicMetrics(rec)

	w := panicPost(t, srv, false)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("POST status = %d, want 502\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream provider error") {
		t.Fatalf("502 body = %s, want it to say \"upstream provider error\" (the provider client faulted)", w.Body.String())
	}
	rows := engLedger.snapshot()
	if len(rows) != 1 || rows[0].provider != "panicker" || rows[0].ok {
		t.Fatalf("engine ledger rows = %+v, want exactly one failed panicker row", rows)
	}
	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="provider_chat"} 1`) {
		t.Fatalf("metrics missing provider_chat panic series:\n%s", got)
	}
	if ov := panicOverview(t, srv); ov.Panics != 1 {
		t.Fatalf("overview panics_since_start = %d, want 1", ov.Panics)
	}
	w2 := panicPost(t, srv, false)
	if w2.Code != http.StatusBadGateway {
		t.Fatalf("second POST status = %d, want 502 (still alive, still honest)", w2.Code)
	}
}

// TestChatStreamSurvivesMidStreamPanic proves the streaming
// headline: the chunk converter panics after emitting "hel", so
// the client gets the "hel" chunk, then a terminal error event
// and [DONE] — never a switch to another provider after commit,
// never a dead connection. The panic is counted at
// provider_stream, and a second stream request still answers.
func TestChatStreamSurvivesMidStreamPanic(t *testing.T) {
	engLedger := &panicTestLedger{}
	eng := router.NewEngine(map[string]provider.Client{
		"panicker": &panicChatClient{chunks: []string{"hel"}},
		"fallback": &okChatClient{content: "should never serve"},
	}, engLedger, router.Config{MaxAttempts: 3, ChainFor: panicFirstChain})
	srv, rec, _ := panicTestServer(t, eng)
	eng.SetPanicMetrics(rec)

	w := panicPost(t, srv, true)
	if w.Code != http.StatusOK {
		t.Fatalf("stream POST status = %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "hel") {
		t.Fatalf("SSE body missing the committed \"hel\" chunk:\n%s", body)
	}
	if !strings.Contains(body, "upstream provider error") {
		t.Fatalf("SSE body missing the terminal error frame (want \"upstream provider error\", the provider client faulted):\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE body missing [DONE] after the error frame:\n%s", body)
	}
	if strings.Contains(body, "should never serve") {
		t.Fatalf("SSE body contains fallback content — failover after commit must never happen:\n%s", body)
	}
	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="provider_stream"} 1`) {
		t.Fatalf("metrics missing provider_stream panic series:\n%s", got)
	}
	if ov := panicOverview(t, srv); ov.Panics != 1 {
		t.Fatalf("overview panics_since_start = %d, want 1", ov.Panics)
	}
	rows := engLedger.snapshot()
	if len(rows) != 1 || rows[0].provider != "panicker" || rows[0].ok {
		t.Fatalf("engine ledger rows = %+v, want exactly one failed panicker row", rows)
	}

	// Second stream request still answers (terminal error again,
	// not a crash or a hang).
	w2 := panicPost(t, srv, true)
	if w2.Code != http.StatusOK {
		t.Fatalf("second stream POST status = %d, want 200", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "[DONE]") {
		t.Fatalf("second stream body missing [DONE]:\n%s", w2.Body.String())
	}
}

// panicOnceEngine is a fake Engine whose Chat panics on the first
// call and succeeds afterwards: it proves the server's backstop
// recover (server_chat) and that the process stays up across an
// engine panic. Its panic value is key-shaped on purpose, so the
// redaction test can reuse it.
type panicOnceEngine struct {
	calls   atomic.Int32
	chatRes string
}

func (e *panicOnceEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if e.calls.Add(1) == 1 {
		panic("simulated engine panic sk-live-FAKESECRET1234567890")
	}
	return &provider.ChatResponse{Content: e.chatRes, Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 3, TotalTokens: 8}}, nil
}

func (e *panicOnceEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if err := onDelta(e.chatRes); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: e.chatRes}, nil
}

func (e *panicOnceEngine) LastTrace() *routetrace.RouteTrace { return nil }

// TestServerRecoversWhenEnginePanics proves the server backstop:
// the engine panics, the client gets an honest 502, the panic is
// counted at server_chat, a backstop ledger row (provider
// "unknown", failed) exists even though the engine recorded
// nothing, and the very next request succeeds.
func TestServerRecoversWhenEnginePanics(t *testing.T) {
	srv, _, led := panicTestServer(t, &panicOnceEngine{chatRes: "ok after panic"})

	w := panicPost(t, srv, false)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("first POST status = %d, want 502\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "gateway internal error") {
		t.Fatalf("502 body = %s, want \"gateway internal error\"", w.Body.String())
	}
	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="server_chat"} 1`) {
		t.Fatalf("metrics missing server_chat panic series:\n%s", got)
	}
	if ov := panicOverview(t, srv); ov.Panics != 1 {
		t.Fatalf("overview panics_since_start = %d, want 1", ov.Panics)
	}
	evs := panicRecentEvents(t, led)
	found := false
	for _, ev := range evs {
		if ev.Model == "m" && ev.Provider == "unknown" && !ev.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("no backstop ledger row (provider unknown, failed, model m) in %d events: %+v", len(evs), evs)
	}

	w2 := panicPost(t, srv, false)
	if w2.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, want 200 (server survived the engine panic)\nbody: %s", w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), "ok after panic") {
		t.Fatalf("second response = %s, want the success content", w2.Body.String())
	}
}

// panicChunkEngine is a fake chunkEngine whose ChatStreamChunks
// panics after emitting one chunk on the first call and streams
// normally afterwards: it proves the server_stream backstop.
type panicChunkEngine struct {
	calls atomic.Int32
}

func (e *panicChunkEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (e *panicChunkEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if err := onDelta("ok"); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (e *panicChunkEngine) ChatStreamChunks(ctx context.Context, req provider.ChatRequest, onChunk func(provider.StreamChunk) error) (*provider.ChatResponse, error) {
	if e.calls.Add(1) == 1 {
		if err := onChunk(provider.StreamChunk{Content: "hel"}); err != nil {
			return nil, err
		}
		panic("simulated chunk-engine panic")
	}
	if err := onChunk(provider.StreamChunk{Content: "ok stream"}); err != nil {
		return nil, err
	}
	return &provider.ChatResponse{Content: "ok stream"}, nil
}

func (e *panicChunkEngine) LastTrace() *routetrace.RouteTrace { return nil }

// TestServerRecoversWhenChunkEnginePanics proves the
// server_stream backstop: the chunk engine panics mid-stream, the
// client still gets the committed chunk, then a terminal SSE
// error frame and [DONE]; the panic is counted at server_stream;
// the next stream request succeeds.
func TestServerRecoversWhenChunkEnginePanics(t *testing.T) {
	srv, _, _ := panicTestServer(t, &panicChunkEngine{})

	w := panicPost(t, srv, true)
	if w.Code != http.StatusOK {
		t.Fatalf("first stream POST status = %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "hel") {
		t.Fatalf("SSE body missing the committed \"hel\" chunk:\n%s", body)
	}
	if !strings.Contains(body, "gateway internal error") {
		t.Fatalf("SSE body missing the terminal error frame:\n%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE body missing [DONE]:\n%s", body)
	}
	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="server_stream"} 1`) {
		t.Fatalf("metrics missing server_stream panic series:\n%s", got)
	}

	w2 := panicPost(t, srv, true)
	if w2.Code != http.StatusOK {
		t.Fatalf("second stream POST status = %d, want 200", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), "ok stream") {
		t.Fatalf("second stream body = %s, want the success content", w2.Body.String())
	}
}

// TestChatCompletionsHookPanicLeavesLedgerRow proves the engine
// hook case over HTTP: a panicking order hook means no provider
// was ever called, so the engine records nothing — but the
// server's backstop still records a failed row (provider
// "unknown"), the client gets an honest 502, and the panic is
// counted at engine_chat.
func TestChatCompletionsHookPanicLeavesLedgerRow(t *testing.T) {
	engLedger := &panicTestLedger{}
	eng := router.NewEngine(map[string]provider.Client{
		"a": &okChatClient{content: "a says hi"},
	}, engLedger, router.Config{
		MaxAttempts: 3,
		OrderHook: func(chain []router.Attempt) []router.Attempt {
			panic("simulated order-hook panic")
		},
	})
	srv, rec, led := panicTestServer(t, eng)
	eng.SetPanicMetrics(rec)

	w := panicPost(t, srv, false)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("POST status = %d, want 502\nbody: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "upstream provider error") {
		t.Fatalf("502 body = %s, want \"upstream provider error\" (the provider client faulted)", w.Body.String())
	}
	if len(engLedger.snapshot()) != 0 {
		t.Fatalf("engine ledger rows = %+v, want none (no provider was called)", engLedger.snapshot())
	}
	evs := panicRecentEvents(t, led)
	found := false
	for _, ev := range evs {
		if ev.Model == "m" && ev.Provider == "unknown" && !ev.OK {
			found = true
		}
	}
	if !found {
		t.Fatalf("no backstop ledger row (provider unknown, failed, model m) in %d events: %+v", len(evs), evs)
	}
	if got := panicMetricsBody(t, srv); !strings.Contains(got, `codvian_router_panics_total{location="engine_chat"} 1`) {
		t.Fatalf("metrics missing engine_chat panic series:\n%s", got)
	}
	if ov := panicOverview(t, srv); ov.Panics != 1 {
		t.Fatalf("overview panics_since_start = %d, want 1", ov.Panics)
	}
}

// TestOverviewPanicsZeroBeforeAnyPanic proves the honest zero:
// a fresh server with no panics reports panics_since_start 0 and
// renders no panics series in /metrics.
func TestOverviewPanicsZeroBeforeAnyPanic(t *testing.T) {
	srv, _, _ := panicTestServer(t, &fakeEngine{})
	if ov := panicOverview(t, srv); ov.Panics != 0 {
		t.Fatalf("overview panics_since_start = %d, want 0 before any panic", ov.Panics)
	}
	if got := panicMetricsBody(t, srv); strings.Contains(got, "codvian_router_panics_total{") {
		t.Fatalf("metrics render a panics series before any panic:\n%s", got)
	}
}

// TestPanicValueRedactedInLog proves the panic value — which here
// is key-shaped on purpose — is redacted in the server log line
// and never appears in the client response.
func TestPanicValueRedactedInLog(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	var logBuf bytes.Buffer
	srv := newTestServer(t, &panicOnceEngine{chatRes: "ok"},
		WithLedger(led), WithMetrics(metrics.NewRecorder()),
		WithLogger(log.New(&logBuf, "", 0)))

	w := panicPost(t, srv, false)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("POST status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "FAKESECRET") {
		t.Fatalf("502 body leaks the panic value: %s", w.Body.String())
	}
	logged := logBuf.String()
	if strings.Contains(logged, "FAKESECRET") {
		t.Fatalf("server log leaks the panic value:\n%s", logged)
	}
	if !strings.Contains(logged, "panic recovered in server_chat") {
		t.Fatalf("server log has no panic-recovered line:\n%s", logged)
	}
}

// TestNoteBackgroundPanicCountsAndRedacts (AUD-REL-006): the
// background-worker recovery path increments
// codvian_router_panics_total under its bg_ location and logs the
// redacted value — never the raw panic payload (a key-shaped value
// here on purpose, mirroring TestPanicValueRedactedInLog).
func TestNoteBackgroundPanicCountsAndRedacts(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	var logBuf bytes.Buffer
	rec := metrics.NewRecorder()
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led), WithMetrics(rec),
		WithLogger(log.New(&logBuf, "", 0)))

	srv.noteBackgroundPanic("bg_a2a_stream_task", "sk-ant-FAKESECRET-explosion")
	if got := rec.Panics(); got != 1 {
		t.Fatalf("panics counter = %d, want 1", got)
	}
	logged := logBuf.String()
	if strings.Contains(logged, "FAKESECRET") {
		t.Fatalf("background panic log leaks the panic value:\n%s", logged)
	}
	if !strings.Contains(logged, "panic recovered in background worker bg_a2a_stream_task") {
		t.Fatalf("background panic log line missing:\n%s", logged)
	}
	// A second background panic in the same location counts again
	// (per-occurrence, not deduped).
	srv.noteBackgroundPanic("bg_a2a_stream_task", errors.New("again"))
	if got := rec.Panics(); got != 2 {
		t.Fatalf("panics counter after second panic = %d, want 2", got)
	}
}
