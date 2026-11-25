package server_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// Lossy-compression × semantic-cache interaction through the real
// HTTP path (CHECKLIST 193-194): the 183-184 smoke found that a
// response produced from a lossy-trimmed context could be served
// from the semantic cache to a later full-context request with
// the same last user message. The fix excludes lossy requests
// from the semantic layer (degrade.lossyCompressed); these tests
// prove the exclusion end to end — real server, real engine,
// real StaleEngine wrapper with ServeCache on, mock upstream —
// and prove lossless sharing still works through the same path.
//
// This file is package server_test (external), not package server:
// the server package's tests cannot import internal/degrade —
// degrade imports server, so that would be an import cycle. The
// fixture below therefore uses only exported constructors and
// carries its own mock + ledger adapter + request helper.

// compressionCacheMock answers based on the prompt it actually
// receives, so the test can tell a trimmed-context answer from a
// full-context one: a request whose last user message is
// "What is 2+2?" gets TRIMMED-ANSWER when the provider saw a
// short (trimmed) prompt and FULL-ANSWER when it saw the full
// history. Anything else gets GENERIC-ANSWER.
type compressionCacheMock struct {
	mu       sync.Mutex
	last     map[string]any
	hitCount int
	srv      *httptest.Server
}

func newCompressionCacheMock(t *testing.T) *compressionCacheMock {
	t.Helper()
	m := &compressionCacheMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		m.mu.Lock()
		m.last = decoded
		m.hitCount++
		m.mu.Unlock()
		content := "GENERIC-ANSWER"
		if msgs, ok := decoded["messages"].([]any); ok && len(msgs) > 0 {
			if last, ok := msgs[len(msgs)-1].(map[string]any); ok && last["content"] == "What is 2+2?" {
				if len(msgs) <= 3 {
					content = "TRIMMED-ANSWER"
				} else {
					content = "FULL-ANSWER"
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-mock","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *compressionCacheMock) hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hitCount
}

func (m *compressionCacheMock) messageCount(t *testing.T) int {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.last["messages"].([]any)
	if !ok {
		t.Fatalf("mock received no messages: %v", m.last)
	}
	return len(msgs)
}

// compressionCacheLedgerAdapter mirrors the production wiring in
// cmd/codvian-router (ledgerAdapter): the engine's RecordAttempt
// calls land in the async writer, so the real ledger sees the
// rows. Compression fields ride along with the usage.
type compressionCacheLedgerAdapter struct {
	w *ledger.AsyncWriter
}

func (a *compressionCacheLedgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	a.w.Record(ledger.Usage{
		Provider: providerName, Model: model, OK: ok, LatencyMS: latencyMs,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens,
		CachedTokens: usage.CachedTokens, ReasoningTokens: usage.ReasoningTokens, CacheWriteTokens: usage.CacheWriteTokens,
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		LocalEstimate:           usage.LocalEstimate,
		SplitKnown:              !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
	})
}

func (a *compressionCacheLedgerAdapter) RecordAttemptForKey(keyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	u := ledger.Usage{
		GatewayKeyID: keyID,
		Provider:     providerName, Model: model, OK: ok, LatencyMS: latencyMs,
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens,
		CachedTokens: usage.CachedTokens, ReasoningTokens: usage.ReasoningTokens, CacheWriteTokens: usage.CacheWriteTokens,
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		LocalEstimate:           usage.LocalEstimate,
		SplitKnown:              !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
	}
	a.w.Record(u)
}

func (a *compressionCacheLedgerAdapter) RecordShared(providerName, model string, ok bool, latencyMs int64) {
	a.w.RecordShared(providerName, model, ok, latencyMs)
}

type compressionCacheFixture struct {
	srv  *server.Server
	mock *compressionCacheMock
	led  *ledger.Ledger
	w    *ledger.AsyncWriter
}

func newCompressionCacheFixture(t *testing.T) *compressionCacheFixture {
	t.Helper()
	mock := newCompressionCacheMock(t)
	client, err := provider.NewClient(mock.srv.URL, "mock-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	// Close the ledger at test end (registered before the writer
	// cleanup, so LIFO closes the writer first): on Windows an
	// open ledger.db blocks t.TempDir cleanup.
	t.Cleanup(func() { _ = led.Close() })
	w := ledger.NewAsyncWriter(led, nil, ledger.AsyncWriterConfig{})
	t.Cleanup(func() { w.Close() })
	eng := router.NewEngine(map[string]provider.Client{"mock": client}, &compressionCacheLedgerAdapter{w: w}, router.Config{
		MaxAttempts: 5, HedgeDelay: -1,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "mock", Model: "gate-model"}}
		},
	})
	// The production wiring (cmd/codvian-router serve mode):
	// StaleEngine with ServeCache on, metrics wired so the
	// overview feed counts cache hits by layer.
	rec := metrics.NewRecorder()
	serving := degrade.WrapWithSemantic(eng, cache.NewExactCache(500), cache.NewLocalSemanticCache(), degrade.Policy{
		Enabled: true, AllowStale: true, ServeCache: true,
	})
	if se, ok := serving.(*degrade.StaleEngine); ok {
		se.SetCacheMetrics(rec)
	}
	cfg := config.DefaultConfig(t.TempDir())
	srv := server.New(serving,
		server.WithMetrics(rec), server.WithLedger(led), server.WithConfig(cfg),
		server.WithStaticKeys("test-key-123"), server.WithTimeout(10*time.Second),
		server.WithLogger(log.New(io.Discard, "", 0)))
	// Close the server-owned stores at test end: on Windows an
	// open ccr.db blocks t.TempDir cleanup. This fixture builds
	// the server with server.New directly (not newTestServer),
	// so it registers the cleanup itself.
	t.Cleanup(func() { _ = srv.Close() })
	return &compressionCacheFixture{srv: srv, mock: mock, led: led, w: w}
}

func (f *compressionCacheFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func chatBody(t *testing.T, msgs []map[string]string, compression map[string]any) string {
	t.Helper()
	body := map[string]any{"model": "gate-model", "messages": msgs}
	if compression != nil {
		body["compression"] = compression
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(b)
}

func contentOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("response JSON: %v (body %s)", err, rec.Body.String())
	}
	choices, _ := d["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("response has no choices: %s", rec.Body.String())
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	c, _ := msg["content"].(string)
	return c
}

// lossyFiller is one long turn of the history that lossy trimming
// drops: ~170 local tokens, far above the 40-token lossy budget.
var lossyFiller = strings.Repeat("The expedition launched in the morning and travelled for three days through the dark sky. ", 10)

func lossyHistoryMsgs() []map[string]string {
	return []map[string]string{
		{"role": "system", "content": "You are helpful."},
		{"role": "user", "content": "Tell me a long story about the first expedition to the moon, in great detail. " + lossyFiller},
		{"role": "assistant", "content": "Once upon a time, a long time ago, people dreamed of the moon... " + lossyFiller},
		{"role": "user", "content": "Continue the story with the landing itself, every detail you know. " + lossyFiller},
		{"role": "assistant", "content": "The landing was a careful descent through the thin lunar sky... " + lossyFiller},
		{"role": "user", "content": "What is 2+2?"},
	}
}

// TestChatCompletionsLossyResponseNeverServedSemantically is the
// 183-184 repro at the HTTP level: request A asks for lossy
// trimming (budget 40) over a long history; the provider sees the
// trimmed prompt (2 messages) and answers TRIMMED-ANSWER.
// Request B sends the same history with no compression; it must
// get a fresh provider call (mock hits 2) and FULL-ANSWER —
// never A's trimmed answer from the semantic cache, and no
// X-Codvian-Cache header.
func TestChatCompletionsLossyResponseNeverServedSemantically(t *testing.T) {
	f := newCompressionCacheFixture(t)

	rec := f.post(t, chatBody(t, lossyHistoryMsgs(), map[string]any{"lossy_max_prompt_tokens": 40}))
	if rec.Code != http.StatusOK {
		t.Fatalf("lossy status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "TRIMMED-ANSWER" {
		t.Fatalf("lossy content = %q, want TRIMMED-ANSWER", got)
	}
	if n := f.mock.messageCount(t); n != 2 {
		t.Fatalf("provider saw %d messages for the lossy request, want 2 (trimmed)", n)
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless+lossy" {
		t.Fatalf("X-Codvian-Compression = %q, want lossless+lossy", got)
	}
	if got := rec.Header().Get("X-Codvian-Compression-Lossy"); got != "true" {
		t.Fatalf("X-Codvian-Compression-Lossy = %q, want true", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "" {
		t.Fatalf("X-Codvian-Cache = %q on the lossy request, want absent", got)
	}
	if f.mock.hits() != 1 {
		t.Fatalf("mock hits = %d after the lossy request, want 1", f.mock.hits())
	}

	rec = f.post(t, chatBody(t, lossyHistoryMsgs(), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("full-context status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "FULL-ANSWER" {
		t.Fatalf("full-context content = %q, want FULL-ANSWER (never the trimmed answer)", got)
	}
	if n := f.mock.messageCount(t); n != 6 {
		t.Fatalf("provider saw %d messages for the full-context request, want 6", n)
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "" {
		t.Fatalf("X-Codvian-Cache = %q on the full-context request, want absent (fresh provider call)", got)
	}
	if f.mock.hits() != 2 {
		t.Fatalf("mock hits = %d after the full-context request, want 2 (provider must be called again)", f.mock.hits())
	}
}

// TestChatCompletionsLosslessResponseStillSharedSemantically
// proves the exclusion did not kill the safe case at the HTTP
// level: request A is losslessly compressed (duplicate system
// block); request B sends the same prompt already in normal form
// plus one extra system line — different messages (exact misses),
// same last user message — and is served A's answer from the
// semantic cache (mock hits stay 1, X-Codvian-Cache: semantic).
func TestChatCompletionsLosslessResponseStillSharedSemantically(t *testing.T) {
	f := newCompressionCacheFixture(t)

	msgsA := []map[string]string{
		{"role": "system", "content": "You are helpful.   \n\n\nBe concise."},
		{"role": "system", "content": "You are helpful.   \n\n\nBe concise."},
		{"role": "user", "content": "What is the capital of France?"},
	}
	rec := f.post(t, chatBody(t, msgsA, map[string]any{"lossless": true}))
	if rec.Code != http.StatusOK {
		t.Fatalf("lossless status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "GENERIC-ANSWER" {
		t.Fatalf("lossless content = %q, want GENERIC-ANSWER", got)
	}
	if n := f.mock.messageCount(t); n != 2 {
		t.Fatalf("provider saw %d messages for the lossless request, want 2 (deduped system + user)", n)
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless" {
		t.Fatalf("X-Codvian-Compression = %q, want lossless", got)
	}
	if f.mock.hits() != 1 {
		t.Fatalf("mock hits = %d after the lossless request, want 1", f.mock.hits())
	}

	msgsB := []map[string]string{
		{"role": "system", "content": "You are helpful.\n\nBe concise."},
		{"role": "system", "content": "Answer briefly."},
		{"role": "user", "content": "What is the capital of France?"},
	}
	rec = f.post(t, chatBody(t, msgsB, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "GENERIC-ANSWER" {
		t.Fatalf("second content = %q, want GENERIC-ANSWER (lossless semantic hit)", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "semantic" {
		t.Fatalf("X-Codvian-Cache = %q, want semantic", got)
	}
	if f.mock.hits() != 1 {
		t.Fatalf("mock hits = %d after the second request, want 1 (no provider call on a lossless semantic hit)", f.mock.hits())
	}
}

// TestChatCompletionsLossyExactHitStillWorks pins the exact layer
// at the HTTP level: an identical lossy repeat (same history,
// same budget) is served exactly — the provider is not called
// again, X-Codvian-Cache says exact — because the exact key is
// the messages as sent (trimmed), and the repeat trims to the
// same prompt.
func TestChatCompletionsLossyExactHitStillWorks(t *testing.T) {
	f := newCompressionCacheFixture(t)

	body := chatBody(t, lossyHistoryMsgs(), map[string]any{"lossy_max_prompt_tokens": 40})
	rec := f.post(t, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("first lossy status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "TRIMMED-ANSWER" {
		t.Fatalf("first lossy content = %q, want TRIMMED-ANSWER", got)
	}
	rec = f.post(t, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat lossy status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := contentOf(t, rec); got != "TRIMMED-ANSWER" {
		t.Fatalf("repeat lossy content = %q, want TRIMMED-ANSWER (exact hit)", got)
	}
	if got := rec.Header().Get("X-Codvian-Cache"); got != "exact" {
		t.Fatalf("X-Codvian-Cache = %q on the identical lossy repeat, want exact", got)
	}
	if f.mock.hits() != 1 {
		t.Fatalf("mock hits = %d after the identical lossy repeat, want 1 (exact hit, no provider call)", f.mock.hits())
	}
}
