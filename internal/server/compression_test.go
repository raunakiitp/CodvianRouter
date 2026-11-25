package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// Lossless-first compression through the real HTTP path (CHECKLIST
// 183-184): the provider receives the compressed prompt, the
// response discloses it in headers, the trace feed carries the
// before/after counts, and the ledger (via the Overview feed)
// totals the saving — never silently.

// NOTE (CHECKLIST 177 slice, minimal fix for a concurrent-slice
// build break): this struct had a field `hits int` AND a method
// `hits() int` — Go forbids a field and a method sharing a name.
// Renamed the field to `hitCount` only; the method is unchanged.
// compressionMock is an OpenAI-wire mock that records the request
// body it received, so the test can prove what the provider was
// actually sent.
type compressionMock struct {
	mu       sync.Mutex
	last     map[string]any
	hitCount int
	srv      *httptest.Server
}

func newCompressionMock(t *testing.T) *compressionMock {
	t.Helper()
	m := &compressionMock{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		m.mu.Lock()
		m.last = decoded
		m.hitCount++
		m.mu.Unlock()
		// Streaming callers get SSE; the OpenAI client parses
		// data: lines, so a JSON body would fail the stream.
		if stream, _ := decoded["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-mock\",\"object\":\"chat.completion.chunk\",\"model\":\"gate-model\",\"choices\":[{\"delta\":{\"content\":\"mock says hi\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\ndata: [DONE]\n\n"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-mock","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":"mock says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *compressionMock) hits() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hitCount
}

func (m *compressionMock) messages(t *testing.T) []any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.last["messages"].([]any)
	if !ok {
		t.Fatalf("mock received no messages: %v", m.last)
	}
	return msgs
}

// compressionLedgerAdapter mirrors the production wiring in
// cmd/codvian-router (ledgerAdapter): the engine's RecordAttempt
// calls land in the async writer, so the real ledger — and the
// dashboard feeds served from it — see the rows.
type compressionLedgerAdapter struct {
	w *ledger.AsyncWriter
}

func (a *compressionLedgerAdapter) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	// provider.Usage carries token counts only; the ledger row
	// is a ledger.Usage (provider + model + outcome + latency).
	// (Minimal build fix: the adapter was assigning ledger
	// fields onto provider.Usage, which does not have them.)
	a.w.Record(ledger.Usage{
		Provider:         providerName,
		Model:            model,
		OK:               ok,
		LatencyMS:        latencyMs,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.CachedTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		// Compression rides along with the usage (CHECKLIST
		// 183-184): the router's honest local counts for the
		// compressed prompt, so the ledger can total them.
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		// The split is known exactly as in the production
		// adapter: the upstream reported at least one side of
		// it and it is not a local estimate. Without this,
		// normalizeUsage treats the row as unknown-split and
		// drops the prompt and compression counts.
		LocalEstimate: usage.LocalEstimate,
		SplitKnown:    !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
	})
}

func (a *compressionLedgerAdapter) RecordAttemptForKey(keyID, providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	// Same conversion, with the gateway-key attribution.
	a.w.Record(ledger.Usage{
		GatewayKeyID:     keyID,
		Provider:         providerName,
		Model:            model,
		OK:               ok,
		LatencyMS:        latencyMs,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.CachedTokens,
		ReasoningTokens:  usage.ReasoningTokens,
		CacheWriteTokens: usage.CacheWriteTokens,
		// Compression rides along with the usage (CHECKLIST
		// 183-184): the router's honest local counts for the
		// compressed prompt, so the ledger can total them.
		PromptBeforeCompression: usage.PromptBeforeCompression,
		CompressionSavedTokens:  usage.CompressionSavedTokens,
		// The split is known exactly as in the production
		// adapter (see RecordAttempt above): without it,
		// normalizeUsage treats the row as unknown-split and
		// drops the prompt and compression counts.
		LocalEstimate: usage.LocalEstimate,
		SplitKnown:    !usage.LocalEstimate && (usage.PromptTokens != 0 || usage.CompletionTokens != 0),
	})
}

func (a *compressionLedgerAdapter) RecordShared(providerName, model string, ok bool, latencyMs int64) {
	a.w.RecordShared(providerName, model, ok, latencyMs)
}

type compressionFixture struct {
	srv  *Server
	mock *compressionMock
	led  *ledger.Ledger
	w    *ledger.AsyncWriter
}

func newCompressionFixture(t *testing.T, serverCompression *config.CompressionConfig, engineLosslessModels []string) *compressionFixture {
	t.Helper()
	mock := newCompressionMock(t)
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
	eng := router.NewEngine(map[string]provider.Client{"mock": client}, &compressionLedgerAdapter{w: w}, router.Config{
		MaxAttempts: 5, HedgeDelay: -1,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		CompressionLosslessModels: engineLosslessModels,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "mock", Model: "gate-model"}}
		},
	})
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Compression = serverCompression
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithLedger(led), WithConfig(cfg), WithStaticKeys("test-key-123"), WithTimeout(10*time.Second))
	return &compressionFixture{srv: srv, mock: mock, led: led, w: w}
}

func (f *compressionFixture) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, f.srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
}

func (f *compressionFixture) trace(t *testing.T) map[string]any {
	t.Helper()
	rec := doReq(t, f.srv, http.MethodGet, "/dashboard/trace", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("trace feed status = %d, want 200", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("trace feed JSON: %v", err)
	}
	return d
}

func (f *compressionFixture) overview(t *testing.T) map[string]any {
	t.Helper()
	f.w.Flush()
	rec := doReq(t, f.srv, http.MethodGet, "/dashboard/overview", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview feed status = %d, want 200", rec.Code)
	}
	var d map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("overview feed JSON: %v", err)
	}
	return d
}

const compressionSystemDup = `{"model":"gate-model","messages":[
	{"role":"system","content":"You are helpful.   \n\n\n\nBe concise."},
	{"role":"system","content":"You are helpful.   \n\n\n\nBe concise."},
	{"role":"user","content":"What is 2+2?"}],
	"compression":{"lossless":true}}`

func TestChatCompletionsCompressionLossless(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	rec := f.post(t, compressionSystemDup)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	// The provider got the compressed prompt: one system message.
	msgs := f.mock.messages(t)
	if len(msgs) != 2 {
		t.Fatalf("provider saw %d messages, want 2 (deduped system + user)", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["content"] != "You are helpful.\n\nBe concise." {
		t.Fatalf("provider system = %q, want cleaned single copy", sys["content"])
	}
	// The response discloses the compression, never silently.
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless" {
		t.Fatalf("X-Codvian-Compression = %q, want lossless", got)
	}
	saved := rec.Header().Get("X-Codvian-Compression-Saved-Tokens")
	if saved == "" || saved == "0" {
		t.Fatalf("X-Codvian-Compression-Saved-Tokens = %q, want > 0", saved)
	}
	if got := rec.Header().Get("X-Codvian-Compression-Lossy"); got != "" {
		t.Fatalf("X-Codvian-Compression-Lossy = %q, want absent (lossless only)", got)
	}
	// The trace feed carries the same figures, honestly measured.
	tr := f.trace(t)
	comp, ok := tr["compression"].(map[string]any)
	if !ok {
		t.Fatalf("trace compression missing: %v", tr)
	}
	if comp["applied"] != true || comp["verified"] != true || comp["lossy_applied"] == true {
		t.Fatalf("trace compression = %v, want applied+verified, no lossy", comp)
	}
	before, _ := tokencount.CountPrompt("gate-model", []provider.Message{
		{Role: "system", Content: "You are helpful.   \n\n\n\nBe concise."},
		{Role: "system", Content: "You are helpful.   \n\n\n\nBe concise."},
		{Role: "user", Content: "What is 2+2?"},
	})
	if int(comp["before_tokens"].(float64)) != before.Tokens {
		t.Fatalf("trace before_tokens = %v, want %d", comp["before_tokens"], before.Tokens)
	}
	if int(comp["saved_tokens"].(float64)) != int(comp["before_tokens"].(float64))-int(comp["after_tokens"].(float64)) {
		t.Fatalf("trace saved != before-after: %v", comp)
	}
	// The ledger totals the saving in the Overview feed.
	ov := f.overview(t)
	cov, ok := ov["compression"].(map[string]any)
	if !ok {
		t.Fatalf("overview compression missing: %v", ov)
	}
	if cov["requests_with_compression"] != float64(1) {
		t.Fatalf("overview requests_with_compression = %v, want 1", cov["requests_with_compression"])
	}
	if cov["tokens_saved"] != comp["saved_tokens"] {
		t.Fatalf("overview tokens_saved = %v, want trace saved %v", cov["tokens_saved"], comp["saved_tokens"])
	}
}

func TestChatCompletionsCompressionOffByDefault(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	body := `{"model":"gate-model","messages":[
		{"role":"system","content":"You are helpful."},
		{"role":"system","content":"You are helpful."},
		{"role":"user","content":"hi"}]}`
	rec := f.post(t, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(f.mock.messages(t)) != 3 {
		t.Fatalf("provider saw %d messages, want 3 (untouched)", len(f.mock.messages(t)))
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "" {
		t.Fatalf("X-Codvian-Compression = %q, want absent", got)
	}
	if tr := f.trace(t); tr["compression"] != nil {
		t.Fatalf("trace compression = %v, want absent", tr["compression"])
	}
	ov := f.overview(t)
	if cov := ov["compression"].(map[string]any); cov["requests_with_compression"] != float64(0) || cov["tokens_saved"] != float64(0) {
		t.Fatalf("overview compression = %v, want honest zeros", cov)
	}
}

func TestChatCompletionsCompressionLossy(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	var sb strings.Builder
	sb.WriteString(`{"model":"gate-model","messages":[{"role":"system","content":"You are helpful."}`)
	for i := 0; i < 10; i++ {
		sb.WriteString(` ,{"role":"user","content":"filler ` + strings.Repeat("word ", 30) + `"}`)
		sb.WriteString(` ,{"role":"assistant","content":"reply ` + strings.Repeat("word ", 30) + `"}`)
	}
	sb.WriteString(`], "compression":{"lossy_max_prompt_tokens":80}}`)
	rec := f.post(t, sb.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless+lossy" {
		t.Fatalf("X-Codvian-Compression = %q, want lossless+lossy", got)
	}
	if got := rec.Header().Get("X-Codvian-Compression-Lossy"); got != "true" {
		t.Fatalf("X-Codvian-Compression-Lossy = %q, want true", got)
	}
	msgs := f.mock.messages(t)
	if len(msgs) >= 21 {
		t.Fatalf("provider saw %d messages, want fewer than 21 (lossy trimmed)", len(msgs))
	}
	tr := f.trace(t)
	comp, ok := tr["compression"].(map[string]any)
	if !ok || comp["lossy_applied"] != true {
		t.Fatalf("trace compression = %v, want lossy_applied", tr["compression"])
	}
	if int(comp["after_tokens"].(float64)) > 80 {
		t.Fatalf("trace after_tokens = %v, want <= 80", comp["after_tokens"])
	}
}

func TestChatCompletionsCompressionInvalid(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	// Unknown key inside "compression": 400 naming it, never
	// silently ignored.
	rec := f.post(t, `{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"compression":{"lossy_max":10}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown compression key: status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "lossy_max") {
		t.Fatalf("400 body = %s, want it to name the field", rec.Body.String())
	}
	// Negative lossy budget: 400.
	rec = f.post(t, `{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"compression":{"lossy_max_prompt_tokens":-5}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative lossy budget: status = %d, want 400", rec.Code)
	}
	// No provider was called for either refusal.
	if f.mock.hits() != 0 {
		t.Fatalf("mock hits = %d, want 0 (refused before any provider call)", f.mock.hits())
	}
}

// TestChatCompletionsCompressionInvalidMode (M7 merge wave):
// an unknown compression.mode is a 400 naming the field, refused
// before any provider call; a known mode passes validation.
func TestChatCompletionsCompressionInvalidMode(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	rec := f.post(t, `{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"compression":{"mode":"medium"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mode") {
		t.Fatalf("400 body = %s, want it to name the field", rec.Body.String())
	}
	if f.mock.hits() != 0 {
		t.Fatalf("mock hits = %d, want 0 (refused before any provider call)", f.mock.hits())
	}
	// A known mode passes validation (the mock answers 200).
	rec = f.post(t, `{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"compression":{"mode":"standard"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("known mode: status = %d, want 200", rec.Code)
	}
}

func TestChatCompletionsCompressionModelConfig(t *testing.T) {
	cfg := &config.CompressionConfig{LosslessModels: []string{"gate-model"}}
	f := newCompressionFixture(t, cfg, []string{"gate-model"})
	body := `{"model":"gate-model","messages":[
		{"role":"system","content":"Repeat.  \n\n\n\nRepeat."},
		{"role":"system","content":"Repeat.  \n\n\n\nRepeat."},
		{"role":"user","content":"hi"}]}`
	rec := f.post(t, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(f.mock.messages(t)) != 2 {
		t.Fatalf("listed model: provider saw %d messages, want 2", len(f.mock.messages(t)))
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless" {
		t.Fatalf("X-Codvian-Compression = %q, want lossless (model config)", got)
	}
	tr := f.trace(t)
	comp, ok := tr["compression"].(map[string]any)
	if !ok || comp["source"] != "model-config" {
		t.Fatalf("trace compression = %v, want source model-config", tr["compression"])
	}
}

func TestChatCompletionsCompressionStreaming(t *testing.T) {
	f := newCompressionFixture(t, nil, nil)
	body := `{"model":"gate-model","stream":true,"messages":[
		{"role":"system","content":"You are helpful.   \n\n\n\nBe concise."},
		{"role":"system","content":"You are helpful.   \n\n\n\nBe concise."},
		{"role":"user","content":"What is 2+2?"}],
		"compression":{"lossless":true}}`
	rec := doReq(t, f.srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Compression"); got != "lossless" {
		t.Fatalf("stream X-Codvian-Compression = %q, want lossless", got)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("stream body missing [DONE]: %s", rec.Body.String())
	}
	if len(f.mock.messages(t)) != 2 {
		t.Fatalf("stream provider saw %d messages, want 2", len(f.mock.messages(t)))
	}
	tr := f.trace(t)
	if comp, ok := tr["compression"].(map[string]any); !ok || comp["applied"] != true {
		t.Fatalf("stream trace compression = %v, want applied", tr["compression"])
	}
}
