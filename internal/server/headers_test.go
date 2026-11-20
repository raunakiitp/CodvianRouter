// M2.1/M2.2 header contract tests (Wave 1 Phase B, A8): the
// per-response telemetry headers on EVERY inference response path
// (chat, embeddings, rerank, responses, messages, errors, SSE) and
// correlation-id propagation. Written FIRST — these tests FAIL
// until headers.go + the call-site wiring land.
package server

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// headerTestEngine is a fakeEngine that also reports a canned route
// trace (two attempts: prov-a fails, prov-b serves), a canned
// routing record, and optionally a cache hit — the three sources
// responseMeta reads. It does NOT publish into the attribution
// sink, so the server must fall back to the engine-global trace,
// exactly like the real engine's LastTrace path.
type headerTestEngine struct {
	*fakeEngine
	trace      *routetrace.RouteTrace
	routing    router.RoutingInfo
	hasRouting bool
	cacheHit   bool
	cacheKind  string
	cacheSim   float32
}

func (e *headerTestEngine) LastTrace() *routetrace.RouteTrace { return e.trace }

func (e *headerTestEngine) LastRouting() (router.RoutingInfo, bool) {
	return e.routing, e.hasRouting
}

// The remaining strategyReporter methods: the decision header only
// reads LastRouting, but the interface requires all four, so the
// double must satisfy it (the real engine does).
func (e *headerTestEngine) StrategyState() (string, map[string]string) {
	return "priority", nil
}

func (e *headerTestEngine) SetGlobalStrategy(string) {}

func (e *headerTestEngine) SetChainStrategy(model, strategy string) {}

func (e *headerTestEngine) LastCacheHit() (string, float32, bool) {
	return e.cacheKind, e.cacheSim, e.cacheHit
}

func headerTrace() *routetrace.RouteTrace {
	return &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
		{Provider: "prov-a", Model: "model-a", Outcome: "error", LatencyMs: 12},
		{Provider: "prov-b", Model: "model-b", Outcome: "success", LatencyMs: 34},
	}}
}

func headerPrices() ledger.Prices {
	return ledger.Prices{
		{Provider: "prov-b", Model: "model-b"}: {
			PromptPerToken: 1e-6, CompletionPerToken: 2e-6,
			CachedPromptPerToken: 5e-7, CachedKnown: true,
		},
		{Provider: "prov-e", Model: "emb-model"}: {
			PromptPerToken: 1e-6, CompletionPerToken: 2e-6,
		},
		// Model-level fallback: a cache hit names no provider, so
		// its savings price from the model-level rate.
		{Model: "model-b"}: {
			PromptPerToken: 1e-6, CompletionPerToken: 2e-6,
		},
	}
}

// newHeaderServer builds a server whose engine serves model-b with
// usage 100 in / 50 out (10 cached) and the two-attempt trace above,
// priced by headerPrices. Cost for the chat shape:
// (100-10)*1e-6 + 10*5e-7 + 50*2e-6 = 0.000195 USD.
func newHeaderServer(t *testing.T) (*Server, *headerTestEngine) {
	t.Helper()
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{
			chatResp: provider.ChatResponse{
				Content: "Hello there", Model: "model-b",
				Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 10},
			},
			deltas: []string{"Hello", " world"},
		},
		trace: headerTrace(),
		routing: router.RoutingInfo{
			Provider: "prov-b", Model: "model-b",
			Strategy: "priority", Reason: "first-choice",
		},
		hasRouting: true,
	}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	return srv, eng
}

func mustHeader(t *testing.T, rec *httptest.ResponseRecorder, name, want string) {
	t.Helper()
	if got := rec.Header().Get(name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

func mustHeaderPresent(t *testing.T, rec *httptest.ResponseRecorder, name string) string {
	t.Helper()
	got := rec.Header().Get(name)
	if got == "" {
		t.Errorf("%s absent, want present", name)
	}
	return got
}

func mustHeaderAbsent(t *testing.T, rec *httptest.ResponseRecorder, name string) {
	t.Helper()
	if got := rec.Header().Get(name); got != "" {
		t.Errorf("%s = %q, want absent (unknown value must not be emitted as 0)", name, got)
	}
}

func mustHeaderInt(t *testing.T, rec *httptest.ResponseRecorder, name string, want int) {
	t.Helper()
	got := rec.Header().Get(name)
	n, err := strconv.Atoi(got)
	if err != nil || n != want {
		t.Errorf("%s = %q, want %d", name, got, want)
	}
}

func TestResponseHeadersChatNonStream(t *testing.T) {
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := mustHeaderPresent(t, rec, "X-Codvian-Request-Id"); !strings.HasPrefix(got, "req-") {
		t.Errorf("X-Codvian-Request-Id = %q, want generated req-… id", got)
	}
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	mustHeader(t, rec, "X-Codvian-Provider", "prov-b")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0001950000")
	mustHeaderInt(t, rec, "X-Codvian-Tokens-In", 100)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Out", 50)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Cached", 10)
	mustHeaderInt(t, rec, "X-Codvian-Upstream-Latency-Ms", 46)
	mustHeaderInt(t, rec, "X-Codvian-Fallback-Attempts", 1)
	mustHeader(t, rec, "X-Codvian-Fallback-Path", "prov-a/model-a -> prov-b/model-b")
	if v := mustHeaderPresent(t, rec, "X-Codvian-Latency-Ms"); func() bool {
		n, err := strconv.Atoi(v)
		return err != nil || n < 0
	}() {
		t.Errorf("X-Codvian-Latency-Ms = %q, want non-negative integer ms", v)
	}
	// Router overhead is total minus the trace's upstream time:
	// the fake engine answers instantly while its canned trace
	// claims 46 ms upstream, so the difference is negative and
	// the header is honestly ABSENT here. The arithmetic itself
	// is covered by TestWriteResponseHeadersOverhead with real
	// spans. If present, it must be a non-negative integer.
	if v := rec.Header().Get("X-Codvian-Router-Overhead-Us"); v != "" {
		if n, err := strconv.Atoi(v); err != nil || n < 0 {
			t.Errorf("X-Codvian-Router-Overhead-Us = %q, want non-negative integer µs", v)
		}
	}
	if v := mustHeaderPresent(t, rec, "X-Codvian-Tokens-Per-Second"); func() bool {
		f, err := strconv.ParseFloat(v, 64)
		return err != nil || f <= 0
	}() {
		t.Errorf("X-Codvian-Tokens-Per-Second = %q, want positive number", v)
	}
	// Existing Decision header still emitted alongside the new ones.
	if got := rec.Header().Get("X-Codvian-Decision"); !strings.Contains(got, "provider=prov-b") {
		t.Errorf("X-Codvian-Decision = %q, want provider=prov-b", got)
	}
}

// parseStreamTrailer extracts the `: codvian-meta {...}` SSE comment
// from a streamed body. The trailer carries the values that were
// not known when the stream's headers were sent.
// mustTrailerFloat compares a trailer JSON number with float
// tolerance: 0.000195 does not survive a float64 JSON round-trip
// exactly (0.00019499999999999997), and the trailer carries a
// number, not the 10-dp header string.
func mustTrailerFloat(t *testing.T, m map[string]any, key string, want float64) {
	t.Helper()
	got, ok := m[key].(float64)
	if !ok || math.Abs(got-want) > 1e-12 {
		t.Errorf("trailer %s = %v, want %v", key, m[key], want)
	}
}

func parseStreamTrailer(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, ": codvian-meta "); ok {
			var m map[string]any
			if err := json.Unmarshal([]byte(rest), &m); err != nil {
				t.Fatalf("trailer JSON = %q: %v", rest, err)
			}
			return m
		}
	}
	t.Fatalf("no : codvian-meta trailer in body:\n%s", body)
	return nil
}

func TestResponseHeadersChatStream(t *testing.T) {
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing [DONE]:\n%s", body)
	}
	// What is known at stream start rides in the headers.
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	// The rest rides in the final SSE comment trailer.
	m := parseStreamTrailer(t, body)
	if m["provider"] != "prov-b" {
		t.Errorf("trailer provider = %v, want prov-b", m["provider"])
	}
	mustTrailerFloat(t, m, "cost_usd", 0.000195)
	if m["tokens_in"] != float64(100) || m["tokens_out"] != float64(50) || m["tokens_cached"] != float64(10) {
		t.Errorf("trailer tokens = in %v out %v cached %v, want 100/50/10", m["tokens_in"], m["tokens_out"], m["tokens_cached"])
	}
	if m["fallback_attempts"] != float64(1) {
		t.Errorf("trailer fallback_attempts = %v, want 1", m["fallback_attempts"])
	}
	if m["upstream_latency_ms"] != float64(46) {
		t.Errorf("trailer upstream_latency_ms = %v, want 46", m["upstream_latency_ms"])
	}
}

func TestResponseHeadersEmbeddings(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	eng.trace = &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
		{Provider: "prov-e", Model: "emb-model", Outcome: "success", LatencyMs: 5},
	}}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	rec := doEmbedReq(t, srv, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	mustHeader(t, rec, "X-Codvian-Provider", "prov-e")
	mustHeader(t, rec, "X-Codvian-Model", "emb-model")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0000040000")
	mustHeaderInt(t, rec, "X-Codvian-Tokens-In", 4)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Out", 0)
	mustHeaderInt(t, rec, "X-Codvian-Upstream-Latency-Ms", 5)
	mustHeaderInt(t, rec, "X-Codvian-Fallback-Attempts", 0)
	mustHeaderPresent(t, rec, "X-Codvian-Latency-Ms")
}

func TestResponseHeadersRerank(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	eng.trace = &routetrace.RouteTrace{Attempts: []routetrace.AttemptTrace{
		{Provider: "prov-e", Model: "m", Outcome: "success", LatencyMs: 7},
	}}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	rec := doEmbedReq(t, srv, "/v1/rerank", `{"model":"m","query":"q","documents":["d"]}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Provider", "prov-e")
	mustHeader(t, rec, "X-Codvian-Model", "m")
	mustHeaderInt(t, rec, "X-Codvian-Upstream-Latency-Ms", 7)
	// Rerank reports no usage: tokens and cost stay ABSENT, never 0.
	mustHeaderAbsent(t, rec, "X-Codvian-Tokens-In")
	mustHeaderAbsent(t, rec, "X-Codvian-Tokens-Out")
	mustHeaderAbsent(t, rec, "X-Codvian-Cost-USD")
}

func TestResponseHeadersResponses(t *testing.T) {
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/responses",
		`{"model":"model-b","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	mustHeader(t, rec, "X-Codvian-Provider", "prov-b")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0001950000")
	mustHeaderInt(t, rec, "X-Codvian-Tokens-In", 100)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Out", 50)
	mustHeaderInt(t, rec, "X-Codvian-Fallback-Attempts", 1)
	// Retrieve of the stored object carries the correlation headers too.
	var obj struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil || obj.ID == "" {
		t.Fatalf("create body = %s, want response object with id", rec.Body.String())
	}
	rec2 := doReq(t, srv, http.MethodGet, "/v1/responses/"+obj.ID, "", "test-key-123")
	if rec2.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d, body = %s", rec2.Code, rec2.Body.String())
	}
	mustHeaderPresent(t, rec2, "X-Codvian-Request-Id")
	mustHeader(t, rec2, "X-Codvian-Model", "model-b")
}

func TestResponseHeadersResponsesStream(t *testing.T) {
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/responses",
		`{"model":"model-b","stream":true,"input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body missing [DONE]:\n%s", body)
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	m := parseStreamTrailer(t, body)
	if m["provider"] != "prov-b" {
		t.Errorf("trailer provider = %v, want prov-b", m["provider"])
	}
	mustTrailerFloat(t, m, "cost_usd", 0.000195)
	if m["tokens_in"] != float64(100) || m["tokens_out"] != float64(50) {
		t.Errorf("trailer tokens = in %v out %v, want 100/50", m["tokens_in"], m["tokens_out"])
	}
}

func TestResponseHeadersMessagesTranslated(t *testing.T) {
	srv, _ := newHeaderServer(t)
	// No relay resolver: the request translates through the engine.
	rec := doReq(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"model-b","max_tokens":64,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	mustHeader(t, rec, "X-Codvian-Provider", "prov-b")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0001950000")
	mustHeaderInt(t, rec, "X-Codvian-Tokens-In", 100)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Out", 50)
	mustHeaderInt(t, rec, "X-Codvian-Fallback-Attempts", 1)
	if got := rec.Header().Get("X-Codvian-Relay"); !strings.HasPrefix(got, "translated") {
		t.Errorf("X-Codvian-Relay = %q, want translated…", got)
	}
}

func TestResponseHeadersMessagesStream(t *testing.T) {
	srv, _ := newHeaderServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/messages",
		`{"model":"model-b","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"Hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("body missing message_stop:\n%s", body)
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Model", "model-b")
	m := parseStreamTrailer(t, body)
	if m["provider"] != "prov-b" {
		t.Errorf("trailer provider = %v, want prov-b", m["provider"])
	}
	mustTrailerFloat(t, m, "cost_usd", 0.000195)
}

func TestResponseHeadersErrors(t *testing.T) {
	// Engine failure: 502, correlation headers present, telemetry
	// values that were never measured stay absent.
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{chatErr: errors.New("boom")},
		trace:      headerTrace(),
	}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
	mustHeader(t, rec, "X-Codvian-Version", "test-1.2.3")
	// The trace still names the attempted chain.
	mustHeader(t, rec, "X-Codvian-Provider", "prov-b")
	mustHeaderInt(t, rec, "X-Codvian-Fallback-Attempts", 1)
	mustHeaderAbsent(t, rec, "X-Codvian-Tokens-In")
	mustHeaderAbsent(t, rec, "X-Codvian-Cost-USD")

	// Client error: 400 with the correlation headers.
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")

	// Auth failure: 401 with the correlation headers.
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	mustHeaderPresent(t, rec, "X-Codvian-Request-Id")
}

func TestResponseHeadersUnknownPriceAbsent(t *testing.T) {
	// No prices configured: cost is unknown and must be ABSENT,
	// never a fabricated 0.0000000000. Tokens (reported by the
	// upstream) are still emitted.
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{
			Content: "ok", Model: "model-b",
			Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}},
		trace: headerTrace(),
	}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeaderAbsent(t, rec, "X-Codvian-Cost-USD")
	mustHeaderInt(t, rec, "X-Codvian-Tokens-In", 100)
	mustHeaderInt(t, rec, "X-Codvian-Tokens-Out", 50)
}

func TestCorrelationIDPropagation(t *testing.T) {
	srv, _ := newHeaderServer(t)
	body := `{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`

	// Inbound X-Request-Id is echoed verbatim on both spellings.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Request-Id", "corr-123-abc")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeader(t, rec, "X-Codvian-Request-Id", "corr-123-abc")
	mustHeader(t, rec, "X-Request-Id", "corr-123-abc")

	// Without one, a fresh id is generated — and it differs per request.
	rec1 := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	rec2 := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
	id1 := mustHeaderPresent(t, rec1, "X-Codvian-Request-Id")
	id2 := mustHeaderPresent(t, rec2, "X-Codvian-Request-Id")
	if id1 == id2 {
		t.Errorf("generated request ids identical (%q): must be unique per request", id1)
	}
	if !strings.HasPrefix(id1, "req-") || !strings.HasPrefix(id2, "req-") {
		t.Errorf("generated ids = %q, %q, want req-… prefix", id1, id2)
	}

	// The id is echoed on errors too (guardrail-free 400 here).
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":}`))
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("X-Request-Id", "corr-err-1")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	mustHeader(t, rec, "X-Codvian-Request-Id", "corr-err-1")
}

func TestWriteResponseHeadersCache(t *testing.T) {
	// Unit-level: the cache fields of the choke-point writer.
	srv := newTestServer(t, &fakeEngine{}, WithVersion("v-test"))
	w := httptest.NewRecorder()
	meta := ResponseMeta{
		Cache: "exact", CacheSavedTokens: 150,
		CacheSavedUSD: 0.00015, HasCacheSavings: true, HasCacheSavedUSD: true,
	}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeader(t, w, "X-Codvian-Cache", "exact")
	mustHeaderInt(t, w, "X-Codvian-Cache-Saved-Tokens", 150)
	mustHeader(t, w, "X-Codvian-Cache-Saved-USD", "0.0001500000")
	mustHeader(t, w, "X-Codvian-Version", "v-test")

	w = httptest.NewRecorder()
	meta = ResponseMeta{Cache: "semantic", CacheSimilarity: 0.923, HasCacheSimilarity: true}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeader(t, w, "X-Codvian-Cache", "semantic")
	mustHeader(t, w, "X-Codvian-Cache-Similarity", "0.923")
	// Nothing known about tokens/cost on this path: absent, not 0.
	mustHeaderAbsent(t, w, "X-Codvian-Tokens-In")
	mustHeaderAbsent(t, w, "X-Codvian-Cost-USD")
	mustHeaderAbsent(t, w, "X-Codvian-Fallback-Attempts")
}

func TestWriteResponseHeadersOverhead(t *testing.T) {
	// Overhead arithmetic with real spans: total 100 ms, upstream
	// 46 ms → overhead 54,000 µs; tokens/s over the upstream time
	// (50 completion tokens / 0.046 s ≈ 1086.96).
	srv := newTestServer(t, &fakeEngine{}, WithVersion("v-test"))
	w := httptest.NewRecorder()
	meta := ResponseMeta{
		Provider: "prov-b", Model: "model-b",
		Usage:           provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		HasUsage:        true,
		Latency:         100 * time.Millisecond,
		UpstreamLatency: 46 * time.Millisecond, HasUpstreamLatency: true,
		HasFallback: true, FallbackAttempts: 1, FallbackPath: "prov-a/model-a -> prov-b/model-b",
	}
	srv.writeResponseHeaders(w, nil, meta)
	mustHeaderInt(t, w, "X-Codvian-Latency-Ms", 100)
	mustHeaderInt(t, w, "X-Codvian-Upstream-Latency-Ms", 46)
	mustHeaderInt(t, w, "X-Codvian-Router-Overhead-Us", 54000)
	mustHeader(t, w, "X-Codvian-Tokens-Per-Second", "1086.96")
	mustHeaderInt(t, w, "X-Codvian-Fallback-Attempts", 1)

	// Negative overhead (trace claims more upstream time than the
	// request took — a fabricated trace, or clock skew) is
	// omitted, never emitted as a negative number; the upstream
	// figure itself is still reported.
	w = httptest.NewRecorder()
	meta.Latency = 10 * time.Millisecond
	srv.writeResponseHeaders(w, nil, meta)
	mustHeaderAbsent(t, w, "X-Codvian-Router-Overhead-Us")
	mustHeaderInt(t, w, "X-Codvian-Upstream-Latency-Ms", 46)
}

func TestResponseHeadersCacheHit(t *testing.T) {
	// A cache hit bills nothing (Cost-USD is the known 0) and
	// reports what the hit saved, priced from the real ledger
	// price map: 100 prompt + 50 completion tokens at the
	// prov-b/model-b rates = 100*1e-6 + 50*2e-6 = 0.0002 USD.
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{
			Content: "cached answer", Model: "model-b",
			Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		}},
		cacheHit: true, cacheKind: "exact",
	}
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"))
	srv.SetLedgerPrices(headerPrices())
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	mustHeader(t, rec, "X-Codvian-Cache", "exact")
	mustHeader(t, rec, "X-Codvian-Cost-USD", "0.0000000000")
	mustHeaderInt(t, rec, "X-Codvian-Cache-Saved-Tokens", 150)
	mustHeader(t, rec, "X-Codvian-Cache-Saved-USD", "0.0002000000")
}
