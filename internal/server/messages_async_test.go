package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// messagesAsyncMock is an Anthropic /v1/messages upstream for the
// async-guardrail relay tests: it either streams one content delta
// and then holds the connection open until its request context ends
// ("hold-stream"), or answers a slow non-streaming JSON reply
// ("slow-json"), or answers at once ("instant-json"). The hold is
// bounded so a starved run cannot block httptest Close. started
// closes the first time any handler runs, so a test can release
// the classifier only after the upstream was provably dialled.
type messagesAsyncMock struct {
	mode       string
	hits       atomic.Int32
	firstChunk chan struct{}
	started    chan struct{}
	once       sync.Once
	startOnce  sync.Once
}

func (m *messagesAsyncMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.started != nil {
			m.startOnce.Do(func() { close(m.started) })
		}
		m.hits.Add(1)
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch m.mode {
		case "hold-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\",\"stop_reason\":null,\"stop_sequence\":null}}\n\n"))
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello \"}}\n\n"))
			w.(http.Flusher).Flush()
			m.once.Do(func() { close(m.firstChunk) })
			select {
			case <-r.Context().Done():
			case <-time.After(30 * time.Second):
			}
		case "slow-json":
			select {
			case <-time.After(3 * time.Second):
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(anthropicJSONReply))
		default: // instant-json
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(anthropicJSONReply))
		}
	}
}

// messagesAsyncServer builds a Server whose relay target for
// claude-sonnet-4-5 is mock (single attempt → verbatim relay), with
// the given async guardrail store wired in.
func messagesAsyncServer(t *testing.T, mock *messagesAsyncMock, store *guardrails.Store, eng Engine) *Server {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	resolver := &fakeRelayResolver{targets: map[string]*relayTarget{
		"claude-sonnet-4-5": {
			ProviderID: "anth", ProviderName: "Anthropic", WireFormat: "anthropic",
			BaseURL: srv.URL, AuthHeaders: map[string]string{"x-api-key": "sk-ant-test"},
			UpstreamModel: "claude-sonnet-4-5", Attempts: 1,
		},
	}}
	return newTestServer(t, eng, WithRelayResolver(resolver), WithGuardrails(store), WithTimeout(60*time.Second))
}

// pollMessagesAsyncEvent waits for the guardrails feed to record an
// async verdict with the given outcome on the messages surface, and
// fails the test if it never lands. The verdict is recorded on the
// run's background goroutine, so it can land after the handler
// returned — poll, never assert immediately.
func pollMessagesAsyncEvent(t *testing.T, srv *Server, outcome string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		feed := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
		if feed.Code == http.StatusOK {
			var body struct {
				AsyncEvents []struct {
					Outcome string `json:"outcome"`
					Surface string `json:"surface"`
				} `json:"async_events"`
			}
			if err := json.Unmarshal(feed.Body.Bytes(), &body); err == nil {
				for _, ev := range body.AsyncEvents {
					if ev.Outcome == outcome && ev.Surface == "messages" {
						return
					}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("guardrails feed never recorded a %q verdict on surface \"messages\" (last feed: %s)", outcome, feed.Body.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMessagesVerbatimAsyncCutStream is the server gate for
// CHECKLIST 203 (verbatim half): a slow async classifier (BLOCK,
// released only after the first Anthropic delta was already sent)
// cuts a live verbatim relay stream with the Anthropic-protocol
// terminal error event — `event: error` carrying the guardrail kind
// — instead of an upstream error, with no failover, and the stream
// ends there: no [DONE] (Anthropic streams never carry one) and no
// message_stop (the message did not complete normally).
func TestMessagesVerbatimAsyncCutStream(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	mock := &messagesAsyncMock{mode: "hold-stream", firstChunk: make(chan struct{})}
	srv := messagesAsyncServer(t, mock, store, &fakeEngine{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/messages",
			`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":64}`, "test-key-123")
	}()
	select {
	case <-mock.firstChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never sent its first chunk")
	}
	// Let the delta reach the client (localhost delivery is
	// sub-ms when the CPU is free; 1 s tolerates a loaded
	// machine) so the cut is provably mid-stream, not
	// before-commit.
	time.Sleep(time.Second)
	close(classifier.release)
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream was not cut: the handler never returned (the BLOCK verdict did not terminate the relay)")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the SSE headers went out with the first event; body %q)", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `"text":"Hello "`) {
		t.Fatalf("body missing the delta served before the cut:\n%s", body)
	}
	// The terminal frame: the Anthropic error event, carrying the
	// kind — and it comes AFTER the delivered delta.
	evt := strings.Index(body, "event: error")
	if evt < 0 || !strings.Contains(body, "guardrail blocked stream: toxicity") {
		t.Fatalf("body missing the terminal Anthropic error event (guardrail blocked stream: toxicity):\n%s", body)
	}
	if evt < strings.Index(body, `"text":"Hello "`) {
		t.Fatalf("error event precedes the delivered delta (it must terminate the stream, not replace it):\n%s", body)
	}
	// The Anthropic contract: no [DONE] (that is the OpenAI
	// envelope — inventing one would break Anthropic clients) and
	// no message_stop (the message did not complete).
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body must not contain [DONE] — the verbatim relay speaks Anthropic SSE, not OpenAI SSE:\n%s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("body must not contain message_stop — a cut stream did not complete normally:\n%s", body)
	}
	// No Codvian -32001 code inside the Anthropic event: Anthropic
	// defines no policy-refusal error code, and inventing one
	// would be a protocol violation. The code lives on the plain
	// 403 refusal (before commit), not in this event.
	if strings.Contains(body, "-32001") {
		t.Fatalf("body must not carry -32001 inside the Anthropic error event:\n%s", body)
	}
	if strings.Contains(body, "upstream provider error") {
		t.Fatalf("body must not report an upstream error for a policy cut:\n%s", body)
	}
	if mock.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", mock.hits.Load())
	}
	// The dashboard feed shows the verdict on the messages surface.
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeBlockedAfterStart)
}

// TestMessagesVerbatimAsyncWithholdsNonStreaming proves the
// non-streaming verbatim half of CHECKLIST 203: a BLOCK verdict
// that lands while the upstream call is in flight withholds the
// relayed response with 403 / -32001 — and the upstream's content
// never leaks. The provider is still called once (the verdict runs
// concurrently with the call; only the delivery is withheld).
func TestMessagesVerbatimAsyncWithholdsNonStreaming(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	mock := &messagesAsyncMock{mode: "slow-json", firstChunk: make(chan struct{}), started: make(chan struct{})}
	srv := messagesAsyncServer(t, mock, store, &fakeEngine{})
	// Release the classifier only AFTER the upstream was provably
	// dialled: the verdict then lands while the slow upstream reply
	// is still in flight, so the cancel hits the in-flight call and
	// the 403 is a withhold (hits == 1), not a cancel-before-dial
	// (hits == 0, which a fast classifier wins under load).
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/messages",
			`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`, "test-key-123")
	}()
	select {
	case <-mock.started:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream was never dialled")
	}
	close(classifier.release)
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
	if strings.Contains(resp.Body.String(), "Hello from Claude") {
		t.Fatalf("withheld response must not leak its content: %q", resp.Body.String())
	}
	if mock.hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (the provider was called; only the delivery was withheld)", mock.hits.Load())
	}
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeBlockedBeforeDelivery)
}

// TestMessagesTranslatedAsyncCutStream proves the translate half of
// CHECKLIST 203: a BLOCK verdict cuts a translated /v1/messages
// stream (multi-attempt target → engine path) with the same
// Anthropic terminal error event as the verbatim cut — no [DONE],
// no message_stop, no failover to the second target.
func TestMessagesTranslatedAsyncCutStream(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	slowURL, fallbackURL, slowHits, fallbackHits, secondChunk := asyncStreamMocks(t)
	led := &asyncGateLedger{}
	eng := asyncTestEngine(t, slowURL, fallbackURL, led)
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, eng, mock, WithGuardrails(store), WithTimeout(60*time.Second))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/messages",
			`{"model":"multi-model","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":64}`, "test-key-123")
	}()
	select {
	case <-secondChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never sent its second chunk")
	}
	time.Sleep(time.Second)
	close(classifier.release)
	var resp *httptest.ResponseRecorder
	select {
	case resp = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("stream was not cut: the handler never returned")
	}
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", resp.Code, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, `"text":"Hello "`) || !strings.Contains(body, `"text":"world"`) {
		t.Fatalf("body missing the deltas served before the cut:\n%s", body)
	}
	evt := strings.Index(body, "event: error")
	if evt < 0 || !strings.Contains(body, "guardrail blocked stream: toxicity") {
		t.Fatalf("body missing the terminal Anthropic error event:\n%s", body)
	}
	if evt < strings.Index(body, `"text":"world"`) {
		t.Fatalf("error event precedes the delivered deltas:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body must not contain [DONE]:\n%s", body)
	}
	if strings.Contains(body, "message_stop") {
		t.Fatalf("body must not contain message_stop:\n%s", body)
	}
	if strings.Contains(body, "fallback") || strings.Contains(body, "upstream provider error") {
		t.Fatalf("body must not fail over or report an upstream error:\n%s", body)
	}
	if slowHits.Load() != 1 {
		t.Fatalf("slow hits = %d, want 1", slowHits.Load())
	}
	if fallbackHits.Load() != 0 {
		t.Fatalf("fallback hits = %d, want 0 (a policy cut never fails over)", fallbackHits.Load())
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	}
	if trace.Attempts[0].Outcome != guardrails.AsyncBlockOutcome || trace.Attempts[0].Reason != guardrails.AsyncBlockReason {
		t.Fatalf("trace attempt outcome=%q reason=%q, want %q/%q", trace.Attempts[0].Outcome, trace.Attempts[0].Reason, guardrails.AsyncBlockOutcome, guardrails.AsyncBlockReason)
	}
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeBlockedAfterStart)
}

// TestMessagesTranslatedAsyncWithholdsNonStreaming proves the
// non-streaming translate half of CHECKLIST 203: a BLOCK verdict
// withholds the translated response with 403 / -32001, and the
// attempt stays in the ledger as the engine recorded it (the
// provider was called and billed).
func TestMessagesTranslatedAsyncWithholdsNonStreaming(t *testing.T) {
	classifier := newAsyncClassifier(t, "block", "toxicity")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1,"model":"multi-model","choices":[{"index":0,"message":{"role":"assistant","content":"served"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer slow.Close()
	client, err := provider.NewClient(slow.URL, "slow-key", provider.WithTimeout(5*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &asyncGateLedger{}
	eng := router.NewEngine(map[string]provider.Client{"slow": client}, led, router.Config{
		MaxAttempts: 5, HedgeDelay: -1, BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(model string) []router.Attempt { return []router.Attempt{{ProviderID: "slow", Model: model}} },
	})
	mock := &anthropicMock{jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, eng, mock, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/messages",
			`{"model":"multi-model","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`, "test-key-123")
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
	rows := led.snapshot()
	if len(rows) != 1 || !rows[0].ok {
		t.Fatalf("ledger rows = %+v, want 1 successful row (the provider was called and billed)", rows)
	}
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeBlockedBeforeDelivery)
}

// TestMessagesAsyncAllowPassesThrough proves the happy path on the
// relay: an async ALLOW verdict changes nothing — the verbatim
// stream passes through byte-identical (message_stop included) and
// the non-streaming reply is untouched, with no refusal frame.
func TestMessagesAsyncAllowPassesThrough(t *testing.T) {
	classifier := newAsyncClassifier(t, "allow", "")
	store := asyncGuardrailStore(t, guardrails.ModeAsync, classifier)
	mock := &anthropicMock{streamBody: anthropicSSEReply, jsonBody: anthropicJSONReply}
	srv := newMessagesServer(t, &fakeEngine{}, mock, WithGuardrails(store), WithTimeout(60*time.Second))
	close(classifier.release)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":64}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != anthropicSSEReply {
		t.Fatalf("stream body differs from the upstream SSE (an ALLOW verdict must not alter the relay):\n got %q\nwant %q", rec.Body.String(), anthropicSSEReply)
	}
	rec2 := doReq(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`, "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("non-stream status = %d, want 200 (body %q)", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != anthropicJSONReply {
		t.Fatalf("non-stream body differs from the upstream JSON:\n got %q\nwant %q", rec2.Body.String(), anthropicJSONReply)
	}
	// The verdict is recorded on the messages surface.
	deadline := time.Now().Add(10 * time.Second)
	for {
		stats := store.Stats()
		if len(stats.Async) == 1 && stats.Async[0].Allowed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("async stats = %+v, want 2 allowed (one per request)", store.Stats().Async)
		}
		time.Sleep(50 * time.Millisecond)
	}
	pollMessagesAsyncEvent(t, srv, guardrails.AsyncOutcomeAllowed)
}

// TestAnthropicPolicyRefusalEvent pins the exact terminal frame
// (CHECKLIST 203): an `event: error` event, no [DONE], no
// message_stop, kind in the message, no invented -32001 code.
func TestAnthropicPolicyRefusalEvent(t *testing.T) {
	frame := string(anthropicPolicyRefusalEvent("toxicity"))
	if !strings.HasPrefix(frame, "event: error\ndata: ") || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame = %q, want an `event: error` SSE event", frame)
	}
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	data := strings.TrimPrefix(frame, "event: error\ndata: ")
	data = strings.TrimSuffix(data, "\n\n")
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("frame data is not JSON: %v (%q)", err, frame)
	}
	if payload.Type != "error" || payload.Error.Type != "api_error" || payload.Error.Message != "guardrail blocked stream: toxicity" {
		t.Fatalf("frame payload = %+v, want type error / api_error / guardrail blocked stream: toxicity", payload)
	}
	if strings.Contains(frame, "[DONE]") || strings.Contains(frame, "message_stop") || strings.Contains(frame, "-32001") {
		t.Fatalf("frame must not contain [DONE], message_stop or -32001: %q", frame)
	}
}
