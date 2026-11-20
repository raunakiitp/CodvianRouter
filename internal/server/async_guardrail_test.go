package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// asyncGateLedger records full ledger rows (incl. usage) for the
// async-guardrail server tests, so the test can prove the cut
// attempt billed only the prompt estimate.
type asyncGateLedger struct {
	mu   sync.Mutex
	rows []asyncGateLedgerRow
}

type asyncGateLedgerRow struct {
	provider string
	model    string
	ok       bool
	usage    provider.Usage
}

func (l *asyncGateLedger) RecordAttempt(providerID, model string, ok bool, _ int64, usage provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, asyncGateLedgerRow{provider: providerID, model: model, ok: ok, usage: usage})
}

func (l *asyncGateLedger) snapshot() []asyncGateLedgerRow {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]asyncGateLedgerRow, len(l.rows))
	copy(out, l.rows)
	return out
}

// asyncClassifier is a controllable slow classifier: it waits on
// release before answering with verdict, so the test can prove the
// stream started before the verdict landed.
type asyncClassifier struct {
	srv     *httptest.Server
	release chan struct{}
	verdict string
	reason  string
	hits    atomic.Int32
}

func newAsyncClassifier(t *testing.T, verdict, reason string) *asyncClassifier {
	t.Helper()
	c := &asyncClassifier{release: make(chan struct{}), verdict: verdict, reason: reason}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)
		<-c.release
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"verdict": %q, "reason": %q}`, c.verdict, c.reason)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *asyncClassifier) url() string { return c.srv.URL }

// asyncGuardrailStore builds a guardrails store with one external
// guardrail (name "toxicity", kind "toxicity") in the given mode,
// backed by the given classifier's HTTP endpoint.
func asyncGuardrailStore(t *testing.T, mode guardrails.Mode, classifier *asyncClassifier) *guardrails.Store {
	t.Helper()
	store := guardrails.NewStoreWith(guardrails.Config{External: []guardrails.ExternalConfig{
		{Name: "toxicity", Kind: "toxicity", Mode: mode, Endpoint: classifier.url()},
	}})
	if err := store.RegisterChecker("toxicity", guardrails.NewHTTPClassifier(classifier.url(), 5*time.Second)); err != nil {
		t.Fatalf("RegisterChecker: %v", err)
	}
	return store
}

// asyncStreamMocks builds the two-provider mock setup for the
// streaming async tests: "slow" streams two chunks then holds the
// connection open until its request context ends (the cut), and
// "fallback" answers a full stream (it must never be dialled after
// a policy cut). Two chunks, plus the settle delay in the test,
// make the mid-stream cut deterministic: the first content chunk
// commits at once (the preread holds only non-content leading
// events) and the second forwards live, so both have reached the
// client before the verdict lands — the gateway then terminates
// a stream that is provably already in flight.
func asyncStreamMocks(t *testing.T) (slowURL, fallbackURL string, slowHits, fallbackHits *atomic.Int32, secondChunk chan struct{}) {
	t.Helper()
	var slow, fallback atomic.Int32
	secondChunk = make(chan struct{})
	var once sync.Once
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slow.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"world\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		once.Do(func() { close(secondChunk) })
		// Hold the stream open: the only way out is the client
		// going away (the async cut cancelling the engine
		// context) — never a third chunk or [DONE]. The hold is
		// bounded so a starved run cannot block httptest Close
		// on a leaked connection.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	t.Cleanup(slowSrv.Close)
	fallbackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallback.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"fallback \"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(fallbackSrv.Close)
	return slowSrv.URL, fallbackSrv.URL, &slow, &fallback, secondChunk
}

func asyncTestEngine(t *testing.T, slowURL, fallbackURL string, led *asyncGateLedger) *router.Engine {
	t.Helper()
	slowClient, err := provider.NewClient(slowURL, "slow-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient slow: %v", err)
	}
	fallbackClient, err := provider.NewClient(fallbackURL, "fallback-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient fallback: %v", err)
	}
	return router.NewEngine(map[string]provider.Client{
		"slow":     slowClient,
		"fallback": fallbackClient,
	}, led, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "slow", Model: model}, {ProviderID: "fallback", Model: model}}
		},
	})
}

// TestAsyncGuardrailCutsStream is the server gate for CHECKLIST
// 185: a slow async classifier (BLOCK, released only after the
// first chunk was already sent) cuts a live stream with the
// terminal refusal frame — the same 403 / -32001 policy-refusal
// shape as the sync path — instead of an upstream error, with no
// failover, and the ledger bills only the prompt estimate.
func TestAsyncGuardrailCutsStream(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	slowURL, fallbackURL, slowHits, fallbackHits, secondChunk := asyncStreamMocks(t)
	led := &asyncGateLedger{}
	eng := asyncTestEngine(t, slowURL, fallbackURL, led)
	rec := metrics.NewRecorder()
	srv := newTestServer(t, eng, WithGuardrails(store), WithMetrics(rec), WithTimeout(60*time.Second))

	// The request runs in its own goroutine: doReq blocks until
	// the handler returns, and the test must release the
	// classifier mid-stream.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/chat/completions",
			`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	}()
	// Release the BLOCK verdict only after the provider sent its
	// second chunk — the stream started before the verdict
	// landed. The settle sleep lets both chunks reach the client
	// (localhost delivery is sub-ms when the CPU is free; 3 s
	// tolerates a loaded machine) so the cut is provably
	// mid-stream, not before-commit.
	select {
	case <-secondChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never sent its second chunk")
	}
	time.Sleep(3 * time.Second)
	close(classifier.release)
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream was not cut: the handler never returned (the BLOCK verdict did not terminate it)")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the SSE headers went out with the first chunk; body %q)", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	// Both chunks reached the client before the cut: the stream
	// was genuinely in flight when the verdict landed, and the
	// gateway terminated it — the mock never sent a third chunk
	// or [DONE], so the refusal frame below is the terminator.
	if !strings.Contains(body, `"content":"Hello "`) || !strings.Contains(body, `"content":"world"`) {
		t.Fatalf("body missing the chunks served before the cut:\n%s", body)
	}
	// The terminal refusal frame: policy_refusal, -32001, reason
	// toxicity — the same shape as the sync refusal — and it
	// comes AFTER the delivered chunk.
	refusal := strings.Index(body, "policy_refusal")
	if refusal < 0 || !strings.Contains(body, "-32001") || !strings.Contains(body, "toxicity") {
		t.Fatalf("body missing the terminal refusal frame (policy_refusal / -32001 / toxicity):\n%s", body)
	}
	if refusal < strings.Index(body, `"content":"world"`) {
		t.Fatalf("refusal frame precedes the delivered chunks (it must terminate the stream, not replace it):\n%s", body)
	}
	// Always-terminal: the client waiting on the terminator never hangs.
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing [DONE] after the refusal frame:\n%s", body)
	}
	// No failover, no second chunk, no upstream error frame.
	if strings.Contains(body, "fallback") || strings.Contains(body, "upstream provider error") {
		t.Fatalf("body must not fail over or report an upstream error:\n%s", body)
	}
	if slowHits.Load() != 1 {
		t.Fatalf("slow hits = %d, want 1", slowHits.Load())
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("fallback hits = %d, want 0 (a policy cut never fails over)", fallbackHits.Load())
	}
	// The trace records the cut, not a provider failure.
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	}
	if trace.Attempts[0].Outcome != guardrails.AsyncBlockOutcome || trace.Attempts[0].Reason != guardrails.AsyncBlockReason {
		t.Fatalf("trace attempt outcome=%q reason=%q, want %q/%q", trace.Attempts[0].Outcome, trace.Attempts[0].Reason, guardrails.AsyncBlockOutcome, guardrails.AsyncBlockReason)
	}
	// The ledger bills only the prompt estimate: failed row,
	// prompt tokens present, completion 0 (never invented).
	rows := led.snapshot()
	if len(rows) != 1 {
		t.Fatalf("ledger rows = %+v, want exactly 1 (the cut attempt)", rows)
	}
	if rows[0].ok || rows[0].usage.PromptTokens <= 0 || !rows[0].usage.LocalEstimate || rows[0].usage.CompletionTokens != 0 {
		t.Fatalf("ledger row = %+v, want failed + local prompt estimate + 0 completion", rows[0])
	}
	// The dashboard feed shows the async verdict honestly.
	feed := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("guardrails feed status = %d, want 200", feed.Code)
	}
	var feedBody struct {
		Async []struct {
			Name              string `json:"name"`
			Mode              string `json:"mode"`
			BlockedAfterStart uint64 `json:"blocked_after_start"`
		} `json:"async"`
		AsyncEvents []struct {
			Outcome string `json:"outcome"`
		} `json:"async_events"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &feedBody); err != nil {
		t.Fatalf("guardrails feed is not JSON: %v (body %q)", err, feed.Body.String())
	}
	if len(feedBody.Async) != 1 || feedBody.Async[0].Name != "toxicity" || feedBody.Async[0].Mode != "async" || feedBody.Async[0].BlockedAfterStart != 1 {
		t.Fatalf("guardrails feed async = %+v, want toxicity/async/1 blocked_after_start", feedBody.Async)
	}
	if len(feedBody.AsyncEvents) != 1 || feedBody.AsyncEvents[0].Outcome != guardrails.AsyncOutcomeBlockedAfterStart {
		t.Fatalf("guardrails feed async_events = %+v, want one blocked_after_start", feedBody.AsyncEvents)
	}
	// /metrics carries the async verdict, and the cut is NOT an
	// upstream failure.
	m := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if m.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", m.Code)
	}
	if !strings.Contains(m.Body.String(), `codvian_router_guardrail_evaluations_total{phase="async",action="blocked_after_start"} 1`) {
		t.Fatalf("metrics missing the async blocked_after_start evaluation:\n%s", m.Body.String())
	}
	if !strings.Contains(m.Body.String(), "codvian_router_upstream_failures_total 0") {
		t.Fatalf("metrics must show 0 upstream failures for a policy cut:\n%s", m.Body.String())
	}
}

// TestAsyncGuardrailCutBeforeCommit proves the other side of the
// cut: when the BLOCK verdict lands before the provider has sent
// any content, nothing was committed to the client, so the cut is
// the plain 403 JSON refusal — identical to the sync refusal —
// with no content leaked. Deterministic by construction: the
// classifier is released at once while the provider waits 500 ms
// before its first chunk, so the verdict lands ~500 ms before any
// content could exist. The trace and ledger still record the cut
// honestly.
func TestAsyncGuardrailCutBeforeCommit(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Slow first byte: the BLOCK verdict (released at once)
		// lands long before this chunk could exist.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello \"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		// Bounded hold: the cut cancels the client connection,
		// but a starved run must not block httptest Close.
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer slowSrv.Close()
	client, err := provider.NewClient(slowSrv.URL, "slow-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"slow": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "slow", Model: model}} },
	})
	srv := newTestServer(t, eng, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/chat/completions",
			`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	}()
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("request never returned after the BLOCK verdict")
	}
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (nothing was committed, so the cut is the plain refusal; body %q)", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "-32001") || !strings.Contains(resp.Body.String(), "toxicity") {
		t.Fatalf("body missing the policy refusal (-32001 / toxicity): %q", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "Hello") {
		t.Fatalf("the held chunk must not leak into the refusal: %q", resp.Body.String())
	}
	trace := eng.LastTrace()
	// The cut landed before anything was delivered — but exactly
	// when it landed relative to the first attempt is a race the
	// test must not pin down: on a free machine the verdict lands
	// before the engine dials (trace: zero attempts, ledger: zero
	// rows — nothing attempted, nothing billed, not even a prompt
	// estimate); on a loaded machine the attempt may already be
	// in flight when the verdict lands (trace: one attempt with
	// outcome async_guardrail_blocked, ledger: one failed row
	// with the prompt estimate and 0 completion). Both are honest
	// records of the same event — the client saw the same plain
	// 403 with nothing delivered — so the test asserts the
	// invariant (trace and ledger agree, and a recorded attempt
	// is the cut with prompt-estimate-only billing), not the
	// timing. The refusal itself is recorded where it belongs:
	// the guardrail store, as blocked_before_delivery.
	if trace == nil {
		t.Fatalf("trace is nil, want a trace (possibly with 0 attempts)")
	}
	rows := led.snapshot()
	if len(trace.Attempts) != len(rows) {
		t.Fatalf("trace attempts = %d but ledger rows = %d, want them to agree", len(trace.Attempts), len(rows))
	}
	// Evidence for the repeated-run record (CHECKLIST 231):
	// which branch this run took — the verdict beat the engine
	// to the entry (0 attempts, nothing dialled) or the engine
	// entered first and the cut landed on the attempt (1).
	t.Logf("cut-before-commit branch: trace attempts=%d ledger rows=%d", len(trace.Attempts), len(rows))
	switch len(trace.Attempts) {
	case 0:
	case 1:
		if trace.Attempts[0].Outcome != guardrails.AsyncBlockOutcome || trace.Attempts[0].Reason != guardrails.AsyncBlockReason {
			t.Fatalf("trace attempt outcome=%q reason=%q, want %q/%q", trace.Attempts[0].Outcome, trace.Attempts[0].Reason, guardrails.AsyncBlockOutcome, guardrails.AsyncBlockReason)
		}
		if rows[0].ok || rows[0].usage.PromptTokens <= 0 || !rows[0].usage.LocalEstimate || rows[0].usage.CompletionTokens != 0 {
			t.Fatalf("ledger row = %+v, want failed + local prompt estimate + 0 completion", rows[0])
		}
	default:
		t.Fatalf("trace attempts = %+v, want 0 or 1 (one cut attempt at most)", trace.Attempts)
	}
	stats := store.Stats()
	if len(stats.Async) != 1 || stats.Async[0].BlockedBeforeDelivery != 1 {
		t.Fatalf("async stats = %+v, want 1 blocked_before_delivery", stats.Async)
	}
}

// TestAsyncGuardrailWithholdsNonStreaming proves the
// non-streaming half of CHECKLIST 185: a BLOCK verdict that lands
// while the provider call is in flight withholds the response with
// 403 / -32001 — and the attempt stays in the ledger as the engine
// recorded it (the provider was called and billed).
func TestAsyncGuardrailWithholdsNonStreaming(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	// The provider answers slowly (3 s) so the verdict (released
	// at once, so it lands as soon as the async run starts) is
	// in hand long before the response could be delivered. The
	// 3 s margin tolerates a loaded machine: a 500 ms margin
	// flaked when the classifier's HTTP call was starved of CPU.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1,"model":"gate-model","choices":[{"index":0,"message":{"role":"assistant","content":"served"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer slow.Close()
	client, err := provider.NewClient(slow.URL, "slow-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"slow": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "slow", Model: model}} },
	})
	srv := newTestServer(t, eng, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/chat/completions",
			`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	}()
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("non-streaming request never returned")
	}
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "-32001") || !strings.Contains(resp.Body.String(), "toxicity") {
		t.Fatalf("body missing the policy refusal (-32001 / toxicity): %q", resp.Body.String())
	}
	if strings.Contains(resp.Body.String(), "served") {
		t.Fatalf("withheld response must not leak its content: %q", resp.Body.String())
	}
	// The provider was called and billed: the ledger row is the
	// engine's success row (same treatment as a response-guardrail
	// block — what is withheld is only the delivery).
	rows := led.snapshot()
	if len(rows) != 1 || !rows[0].ok {
		t.Fatalf("ledger rows = %+v, want 1 successful row (the provider was called and billed)", rows)
	}
	// The feed records the withhold.
	stats := store.Stats()
	if len(stats.Async) != 1 || stats.Async[0].BlockedBeforeDelivery != 1 {
		t.Fatalf("async stats = %+v, want 1 blocked_before_delivery", stats.Async)
	}
}

// TestAsyncGuardrailAllowPassesThrough proves the happy path: an
// async ALLOW verdict changes nothing — the stream completes with
// [DONE] and the verdict is recorded as allowed.
func TestAsyncGuardrailAllowPassesThrough(t *testing.T) {
	classifier := newAsyncClassifier(t, "allow", "")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	// A fast provider that completes at once (the point is the
	// stream is NOT cut by a refusal when the verdict is allow).
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer fast.Close()
	client, err := provider.NewClient(fast.URL, "fast-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"fast": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "fast", Model: model}} },
	})
	srv := newTestServer(t, eng, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"done"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing the completed stream:\n%s", body)
	}
	if strings.Contains(body, "policy_refusal") || strings.Contains(body, "-32001") {
		t.Fatalf("an ALLOW verdict must not produce a refusal frame:\n%s", body)
	}
	// The verdict lands on the run's background context — it can
	// arrive after the (instant) response is delivered, so poll
	// for the recorded verdict instead of asserting immediately.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stats := store.Stats()
		if len(stats.Async) == 1 && stats.Async[0].Allowed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async stats = %+v, want 1 allowed (the verdict never landed)", store.Stats().Async)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStreamCutBeforeEngineEntryPublishesTrace is the regression
// test for CHECKLIST 231-232: a stream cut before the engine is
// entered must still leave a route trace. The server cuts a stream
// by cancelling its context with an AsyncBlockError cause when the
// async BLOCK verdict lands; when that verdict lands before the
// engine's first attempt starts, ChatStreamWithChainStrategy used
// to return on its entry ctx.Err() check without publishing any
// trace — LastTrace stayed nil on a fresh engine, or stale (the
// previous request's trace) on a reused one — and
// TestAsyncGuardrailCutBeforeCommit failed 5/5 in isolation with
// "trace is nil" on a quiet machine (where the verdict wins the
// race), while passing under load (where the engine wins it and
// the cut lands on the attempt instead). The non-streaming path
// never had this hole: ChatWithChainStrategy builds the trace
// first and publishes it on every early return, as do the
// contract/group rejections on the stream path. This test pins
// the same invariant for the stream entry directly and
// deterministically — a pre-cancelled context, no timing, no
// HTTP — so it fails without the fix on every run and passes
// with it.
func TestStreamCutBeforeEngineEntryPublishesTrace(t *testing.T) {
	// A fast provider that completes at once, so the first call
	// leaves a real trace behind (one successful attempt) — the
	// stale trace a missing publish would leave in place.
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer fast.Close()
	client, err := provider.NewClient(fast.URL, "fast-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"fast": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "fast", Model: model}} },
	})
	req := provider.ChatRequest{Model: "gate-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}, Stream: true}
	noop := func(provider.StreamChunk) error { return nil }
	if _, err := eng.ChatStreamChunks(context.Background(), req, noop); err != nil {
		t.Fatalf("seed stream: %v", err)
	}
	seed := eng.LastTrace()
	if seed == nil || len(seed.Attempts) != 1 || seed.Attempts[0].Outcome != "success" {
		t.Fatalf("seed trace = %+v, want one successful attempt", seed)
	}
	// The async guardrail cut, as the server delivers it: the
	// stream context cancelled with the AsyncBlockError cause
	// BEFORE the engine is entered — the BLOCK verdict landed
	// before the first attempt could start.
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&guardrails.AsyncBlockError{Guardrail: "toxicity", Kind: "toxicity"})
	if _, err := eng.ChatStreamChunks(ctx, req, noop); err == nil {
		t.Fatal("ChatStreamChunks on a pre-cancelled context returned nil error")
	}
	// The cut request still leaves a trace — an attempt-less one
	// (no provider was dialled), like the contract/group
	// rejections — instead of nil on a fresh engine, or the
	// previous request's successful trace on a reused one.
	// Without the fix both readings lie: the dashboard trace
	// feed would show the seed request as the latest trace
	// after a request that never reached any provider.
	trace := eng.LastTrace()
	if trace == nil {
		t.Fatal("trace is nil after a pre-entry cut (want an attempt-less trace)")
	}
	if len(trace.Attempts) != 0 {
		t.Fatalf("trace attempts = %+v, want none (the stale seed trace was not replaced)", trace.Attempts)
	}
}

// TestStreamEmptyChainPublishesTrace is the regression test for
// CHECKLIST 251-252: a stream refused for an empty fallback
// chain must still leave a route trace. The 231-232 slice fixed
// the pre-entry ctx-cancel return the same way and flagged this
// adjacent hole deliberately NOT fixed: ChatStreamWithChainStrategy
// returned "empty fallback chain" without publishing any trace,
// so LastTrace stayed nil on a fresh engine, or stale — the
// previous request's successful trace — on a reused one, and the
// dashboard trace feed showed the seed request as the latest
// trace after a request that never reached any provider. This
// test pins the same invariant for the empty-chain return
// directly and deterministically — a model no chain serves, no
// timing, no HTTP — so it fails without the fix on every run
// and passes with it. It reuses the seed-then-refuse shape of
// TestStreamCutBeforeEngineEntryPublishesTrace: the engine's
// ChainFor serves "gate-model" from the fast provider and
// nothing for any other model, so the second call takes the
// len(chain) == 0 early return.
func TestStreamEmptyChainPublishesTrace(t *testing.T) {
	// A fast provider that completes at once, so the first call
	// leaves a real trace behind (one successful attempt) — the
	// stale trace a missing publish would leave in place.
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n"))
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer fast.Close()
	client, err := provider.NewClient(fast.URL, "fast-key", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"fast": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "fast", Model: model}}
		},
	})
	req := provider.ChatRequest{Model: "gate-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}, Stream: true}
	noop := func(provider.StreamChunk) error { return nil }
	if _, err := eng.ChatStreamChunks(context.Background(), req, noop); err != nil {
		t.Fatalf("seed stream: %v", err)
	}
	seed := eng.LastTrace()
	if seed == nil || len(seed.Attempts) != 1 || seed.Attempts[0].Outcome != "success" {
		t.Fatalf("seed trace = %+v, want one successful attempt", seed)
	}
	// No chain serves this model: resolveGroupChain passes the
	// empty chain through (no group named) and the engine
	// refuses before any provider is dialled.
	emptyReq := provider.ChatRequest{Model: "no-chain-model", Messages: []provider.Message{{Role: "user", Content: "hi"}}, Stream: true}
	if _, err := eng.ChatStreamChunks(context.Background(), emptyReq, noop); err == nil {
		t.Fatal("ChatStreamChunks with an empty chain returned nil error")
	} else if !strings.Contains(err.Error(), "empty fallback chain") {
		t.Fatalf("ChatStreamChunks error = %v, want \"empty fallback chain\"", err)
	}
	// The refused request still leaves a trace — an attempt-less
	// one (no provider was dialled), like the contract/group
	// rejections and the pre-entry ctx-cancel return — instead
	// of the previous request's successful trace. (Endpoint is
	// whatever traceEndpointFor returns for this request — ""
	// for plain chat completions, set only for /v1/messages —
	// the same shape the 231-232 fix publishes, so no endpoint
	// assertion here: the stale-seed check above is the
	// invariant.)
	trace := eng.LastTrace()
	if trace == nil {
		t.Fatal("trace is nil after an empty-chain refusal (want an attempt-less trace)")
	}
	if len(trace.Attempts) != 0 {
		t.Fatalf("trace attempts = %+v, want none (the stale seed trace was not replaced)", trace.Attempts)
	}
}

// TestSyncExternalGuardrailBlocksPreCall proves sync mode for an
// external guardrail: a BLOCK verdict refuses the request before
// any provider is dialled (the provider is never hit), with the
// same 403 / -32001 as the built-in blocking toggles.
func TestSyncExternalGuardrailBlocksPreCall(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeSync, classifier)
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	client, err := provider.NewClient(upstream.URL, "k", provider.WithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"up": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "up", Model: model}} },
	})
	srv := newTestServer(t, eng, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32001") || !strings.Contains(rec.Body.String(), "toxicity") {
		t.Fatalf("body missing the policy refusal (-32001 / toxicity): %q", rec.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream hits = %d, want 0 (a sync block refuses before dialling)", hits.Load())
	}
	if len(led.snapshot()) != 0 {
		t.Fatalf("ledger rows = %+v, want none (a sync-blocked request never reaches the engine)", led.snapshot())
	}
}

// TestGuardrailsPostPreservesAsyncEntries proves the dashboard
// POST (toggles only) does not wipe configured external
// guardrails, and that a POST carrying an async blocking kind is
// a 400 naming the guardrail.
func TestGuardrailsPostPreservesAsyncEntries(t *testing.T) {
	classifier := newAsyncClassifier(t, "allow", "")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	srv := newTestServer(t, &fakeEngine{}, WithGuardrails(store))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"block_pii":true,"block_secrets":false,"block_injection":false}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST guardrails status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	stats := store.Stats()
	if len(stats.Async) != 1 || stats.Async[0].Name != "toxicity" || stats.Async[0].Mode != "async" {
		t.Fatalf("POST toggles wiped the async entries: async = %+v", stats.Async)
	}
	if !stats.Config.BlockPII {
		t.Fatalf("POST toggles did not apply: config = %+v", stats.Config)
	}
	// An async blocking kind via POST is a 400.
	bad := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"external":[{"name":"slow-pii","kind":"pii","mode":"async","endpoint":"http://127.0.0.1:9/check"}]}`, "test-key-123")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("POST async+pii status = %d, want 400 (body %q)", bad.Code, bad.Body.String())
	}
	if !strings.Contains(bad.Body.String(), "slow-pii") {
		t.Fatalf("POST async+pii error should name the guardrail: %q", bad.Body.String())
	}
	// And the store is unchanged after the rejected POST.
	if stats := store.Stats(); len(stats.Async) != 1 || stats.Async[0].Name != "toxicity" {
		t.Fatalf("rejected POST changed the store: async = %+v", stats.Async)
	}
}
