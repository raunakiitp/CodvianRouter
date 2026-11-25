// Decision header tests (CHECKLIST 199): X-Codvian-Decision on
// every gateway surface — non-streaming chat, streaming chat
// (first chunk), responses (both forms), embeddings and rerank —
// through the real server and the real router.Engine against local
// httptest mocks. The header must name the provider that actually
// served, the strategy that ordered the chain, and the true reason
// (cheapest / fallback-after-N / contract); it must be ABSENT, never
// invented, when the response was served from cache, when every
// provider failed, or when the engine's record cannot be matched to
// this request.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// decisionUpstream is one mock upstream for decision tests: it
// answers chat completions (JSON or SSE), embeddings and rerank,
// echoing the requested model — the way a real provider echoes it.
type decisionUpstream struct {
	chatStatus   int      // 0 = 200
	chatContent  string   // non-streaming content
	streamChunks []string // nil = stream requests get chatStatus too
	streamStatus int      // 0 = 200
	embedStatus  int      // 0 = 200
	hits         atomic.Int32
	chatHits     atomic.Int32
	streamHits   atomic.Int32
}

func newDecisionUpstream(t *testing.T, up *decisionUpstream) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		var reqModel struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqModel)
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if reqModel.Stream {
				up.streamHits.Add(1)
				if up.streamStatus != 0 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(up.streamStatus)
					_, _ = w.Write([]byte(`{"error":{"message":"upstream blew up","type":"server_error"}}`))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for i, chunk := range up.streamChunks {
					_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", reqModel.Model, chunk)
					_ = i
				}
				_, _ = w.Write([]byte("data: [DONE]\n\n"))
				return
			}
			up.chatHits.Add(1)
			if up.chatStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(up.chatStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream blew up","type":"server_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{
				"id": "chatcmpl-x", "object": "chat.completion", "model": reqModel.Model,
				"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": up.chatContent}, "finish_reason": "stop"}},
				"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
			_, _ = w.Write(body)
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			if up.embedStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(up.embedStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream blew up","type":"server_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":` + strconvQuote(reqModel.Model) + `,"usage":{"prompt_tokens":4,"total_tokens":4}}`))
		case strings.HasSuffix(r.URL.Path, "/rerank"):
			if up.embedStatus != 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(up.embedStatus)
				_, _ = w.Write([]byte(`{"error":{"message":"upstream blew up","type":"server_error"}}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":` + strconvQuote(reqModel.Model) + `,"results":[{"index":1,"relevance_score":0.92},{"index":0,"relevance_score":0.31}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func newDecisionClient(t *testing.T, srv *httptest.Server) provider.Client {
	t.Helper()
	c, err := provider.NewClient(srv.URL, "test-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// newDecisionEngine builds a real router.Engine over the given mock
// clients, with one chain per model from chainFor and the given
// global strategy.
func newDecisionEngine(t *testing.T, clients map[string]provider.Client, strategy string, chainFor func(string) []router.Attempt) *router.Engine {
	t.Helper()
	return router.NewEngine(clients, &failoverGateLedger{}, router.Config{
		Strategy: strategy, MaxAttempts: 5, HedgeDelay: -1,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: chainFor,
	})
}

func chainForModel(model string, attempts ...router.Attempt) func(string) []router.Attempt {
	return func(m string) []router.Attempt {
		if m != model {
			return nil
		}
		return attempts
	}
}

func wantDecisionHeader(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	if got := rec.Header().Get("X-Codvian-Decision"); got != want {
		t.Errorf("X-Codvian-Decision = %q, want %q", got, want)
	}
}

func wantNoDecisionHeader(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("X-Codvian-Decision"); got != "" {
		t.Errorf("X-Codvian-Decision = %q, want absent (no provider served this response)", got)
	}
}

func TestDecisionHeaderNonStreamingCheapest(t *testing.T) {
	up := &decisionUpstream{chatContent: "hello"}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("dec-model", router.Attempt{ProviderID: "mock", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=dec-model; strategy=cheapest; reason=cheapest")
	if got := rec.Header().Get("X-Codvian-Strategy"); got == "" {
		t.Errorf("X-Codvian-Strategy absent alongside the decision header")
	}
}

func TestDecisionHeaderFallbackAfterOne(t *testing.T) {
	dead := &decisionUpstream{chatStatus: http.StatusTooManyRequests}
	deadSrv := newDecisionUpstream(t, dead)
	defer deadSrv.Close()
	live := &decisionUpstream{chatContent: "fallback says hi"}
	liveSrv := newDecisionUpstream(t, live)
	defer liveSrv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{
		"dead":     newDecisionClient(t, deadSrv),
		"fallback": newDecisionClient(t, liveSrv),
	}, "cheapest", chainForModel("dec-model",
		router.Attempt{ProviderID: "dead", Model: "dec-model"},
		router.Attempt{ProviderID: "fallback", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback says hi") {
		t.Fatalf("body = %s, want the fallback's content", rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=fallback; model=dec-model; strategy=cheapest; reason=fallback-after-1")
	if got := dead.chatHits.Load(); got != 1 {
		t.Errorf("dead mock chat hits = %d, want 1", got)
	}
	if got := live.chatHits.Load(); got != 1 {
		t.Errorf("fallback mock chat hits = %d, want 1", got)
	}
}

func TestDecisionHeaderContractSort(t *testing.T) {
	up := &decisionUpstream{chatContent: "hello"}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	// Global strategy is balanced; the request's routing.sort
	// ("price" — the contract's word for the cheapest strategy)
	// wins for the strategy AND is the reason: the contract shaped
	// the chain, and the first target served it.
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "balanced",
		chainForModel("dec-model", router.Attempt{ProviderID: "mock", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}],"routing":{"sort":"price"}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=dec-model; strategy=cheapest; reason=contract")
}

func TestDecisionHeaderStreamFirstChunk(t *testing.T) {
	up := &decisionUpstream{streamChunks: []string{"Hello", " world"}}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("dec-model", router.Attempt{ProviderID: "mock", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The header is set before the first chunk is written: it is
	// readable on the response even though the body streamed.
	wantDecisionHeader(t, rec, "provider=mock; model=dec-model; strategy=cheapest; reason=cheapest")
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" world"`) {
		t.Fatalf("stream body missing chunks: %s", body)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]: %s", body)
	}
}

func TestDecisionHeaderStreamFallbackAfterOne(t *testing.T) {
	dead := &decisionUpstream{streamStatus: http.StatusInternalServerError}
	deadSrv := newDecisionUpstream(t, dead)
	defer deadSrv.Close()
	live := &decisionUpstream{streamChunks: []string{"fallback ", "stream"}}
	liveSrv := newDecisionUpstream(t, live)
	defer liveSrv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{
		"dead":     newDecisionClient(t, deadSrv),
		"fallback": newDecisionClient(t, liveSrv),
	}, "cheapest", chainForModel("dec-model",
		router.Attempt{ProviderID: "dead", Model: "dec-model"},
		router.Attempt{ProviderID: "fallback", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// The dead attempt errored before any content, so the stream
	// committed on the fallback — the decision names it, with the
	// fallback reason, and the body is the fallback's stream.
	wantDecisionHeader(t, rec, "provider=fallback; model=dec-model; strategy=cheapest; reason=fallback-after-1")
	body := rec.Body.String()
	if !strings.Contains(body, `"content":"fallback "`) || !strings.Contains(body, `"content":"stream"`) {
		t.Fatalf("stream body missing the fallback's chunks: %s", body)
	}
	if got := dead.streamHits.Load(); got != 1 {
		t.Errorf("dead mock stream hits = %d, want 1", got)
	}
	if got := live.streamHits.Load(); got != 1 {
		t.Errorf("fallback mock stream hits = %d, want 1", got)
	}
}

func TestDecisionHeaderAbsentOnFailure(t *testing.T) {
	up := &decisionUpstream{chatStatus: http.StatusInternalServerError}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("dec-model", router.Attempt{ProviderID: "mock", Model: "dec-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"dec-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code == http.StatusOK {
		t.Fatalf("status = 200, want an upstream failure (body %s)", rec.Body.String())
	}
	wantNoDecisionHeader(t, rec)
}

// TestDecisionHeaderAbsentOnCacheHit lives in
// decision_cache_test.go (package server_test): the degrade wrapper
// imports this package, so an in-package test cannot import it.

// decisionMismatchEngine is a fakeEngine whose LastRouting record
// names a different model than the request — the record of another
// (concurrent or earlier) request. The server must withhold the
// header rather than report the wrong model as this request's
// decision.
type decisionMismatchEngine struct {
	*fakeEngine
	routing router.RoutingInfo
}

func (e *decisionMismatchEngine) LastRouting() (router.RoutingInfo, bool) { return e.routing, true }
func (e *decisionMismatchEngine) StrategyState() (string, map[string]string) {
	return e.routing.Strategy, nil
}
func (e *decisionMismatchEngine) SetGlobalStrategy(string) {}

func TestDecisionHeaderAbsentOnModelMismatch(t *testing.T) {
	eng := &decisionMismatchEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Model: "model-a"}},
		routing:    router.RoutingInfo{Provider: "other", Model: "other-model", Strategy: "cheapest", Reason: "cheapest"},
	}
	s := newTestServer(t, eng)
	rec := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantNoDecisionHeader(t, rec)
}

func TestDecisionHeaderResponses(t *testing.T) {
	up := &decisionUpstream{chatContent: "responses answer"}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("resp-model", router.Attempt{ProviderID: "mock", Model: "resp-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/responses",
		`{"model":"resp-model","input":"hi"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=resp-model; strategy=cheapest; reason=cheapest")
}

func TestDecisionHeaderResponsesStream(t *testing.T) {
	up := &decisionUpstream{streamChunks: []string{"Hello", " responses"}}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("resp-model", router.Attempt{ProviderID: "mock", Model: "resp-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/responses",
		`{"model":"resp-model","input":"hi","stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=resp-model; strategy=cheapest; reason=cheapest")
	if !strings.Contains(rec.Body.String(), "Hello") {
		t.Fatalf("stream body missing content: %s", rec.Body.String())
	}
}

func TestDecisionHeaderEmbeddings(t *testing.T) {
	up := &decisionUpstream{}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("emb-model", router.Attempt{ProviderID: "mock", Model: "emb-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/embeddings",
		`{"model":"emb-model","input":"hello"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=emb-model; strategy=cheapest; reason=cheapest")
}

func TestDecisionHeaderRerank(t *testing.T) {
	up := &decisionUpstream{}
	srv := newDecisionUpstream(t, up)
	defer srv.Close()
	eng := newDecisionEngine(t, map[string]provider.Client{"mock": newDecisionClient(t, srv)}, "cheapest",
		chainForModel("rerank-model", router.Attempt{ProviderID: "mock", Model: "rerank-model"}))
	s := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, s, http.MethodPost, "/v1/rerank",
		`{"model":"rerank-model","query":"q","documents":["d1","d2"]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=mock; model=rerank-model; strategy=cheapest; reason=cheapest")
}

func TestDecisionHeaderValue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		info   router.RoutingInfo
		want   string
		wantOK bool
	}{
		{"full record", router.RoutingInfo{Provider: "prov-a", Model: "gpt-4o", Strategy: "cheapest", Reason: "cheapest"}, "provider=prov-a; model=gpt-4o; strategy=cheapest; reason=cheapest", true},
		{"fallback reason", router.RoutingInfo{Provider: "b", Model: "m", Strategy: "balanced", Reason: "fallback-after-2"}, "provider=b; model=m; strategy=balanced; reason=fallback-after-2", true},
		{"region step on a cross-region failover", router.RoutingInfo{Provider: "c", Model: "m", Strategy: "balanced", Reason: "fallback-after-1", RegionStep: router.StepCrossRegion}, "provider=c; model=m; strategy=balanced; reason=fallback-after-1; region_step=cross-region", true},
		{"region step on a self-hosted failover", router.RoutingInfo{Provider: "c", Model: "m3", Strategy: "balanced", Reason: "fallback-after-2", RegionStep: router.StepSelfHosted}, "provider=c; model=m3; strategy=balanced; reason=fallback-after-2; region_step=self-hosted", true},
		{"no region step when the first target served", router.RoutingInfo{Provider: "a", Model: "m", Strategy: "balanced", Reason: "balanced"}, "provider=a; model=m; strategy=balanced; reason=balanced", true},
		{"empty reason falls back to strategy", router.RoutingInfo{Provider: "b", Model: "m", Strategy: "fastest", Reason: ""}, "provider=b; model=m; strategy=fastest; reason=latency", true},
		{"no provider", router.RoutingInfo{Model: "m", Strategy: "balanced", Reason: "balanced"}, "", false},
		{"no strategy", router.RoutingInfo{Provider: "b", Model: "m", Reason: "balanced"}, "", false},
		{"no model", router.RoutingInfo{Provider: "b", Strategy: "balanced", Reason: "balanced"}, "", false},
		{"empty strategy and reason", router.RoutingInfo{Provider: "b", Model: "m"}, "", false},
		{"hostile values sanitised", router.RoutingInfo{Provider: "a;b=c", Model: "m;x", Strategy: "cheapest", Reason: "cheapest"}, "provider=a_b_c; model=m_x; strategy=cheapest; reason=cheapest", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decisionHeaderValue(tc.info)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("decisionHeaderValue(%+v) = %q, %v; want %q, %v", tc.info, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
