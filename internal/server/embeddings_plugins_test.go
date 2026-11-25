// Plugin hooks on the embeddings/rerank surfaces (CHECKLIST
// 219-220): the same pre-request / post-response pipeline as chat
// completions now runs on POST /v1/embeddings and POST /v1/rerank
// — deny/error fail closed (403 / -32001), panic fails closed as
// a gateway panic (502 / -32603), post-response failures fail
// open, and the replay exemption is preserved (a denial never
// calls the engine, a delivered response is exactly one upstream
// attempt — no retry is ever caused by a plugin).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// embedPluginMockUpstream serves /v1/embeddings and /v1/rerank
// and records, per path, the hit count and the plugin header it
// saw, if any.
type embedPluginMockUpstream struct {
	embedHits  atomic.Int32
	rerankHits atomic.Int32
	embedHead  atomic.Value // string
	rerankHead atomic.Value // string
}

func (m *embedPluginMockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/embeddings":
			m.embedHits.Add(1)
			m.embedHead.Store(r.Header.Get("X-Codvian-Plugin"))
			_, _ = w.Write([]byte(`{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}],"model":"emb-model","usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}`))
		case "/v1/rerank":
			m.rerankHits.Add(1)
			m.rerankHead.Store(r.Header.Get("X-Codvian-Plugin"))
			_, _ = w.Write([]byte(`{"model":"rerank-model","results":[{"index":0,"relevance_score":0.9}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newEmbedPluginEngine(t *testing.T, upstream *httptest.Server) *router.Engine {
	t.Helper()
	client, err := provider.NewClient(upstream.URL, "plugin-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return router.NewEngine(map[string]provider.Client{"plug": client}, nil, router.Config{
		MaxAttempts:      3,
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			switch model {
			case "emb-model":
				return []router.Attempt{{ProviderID: "plug", Model: "emb-model"}}
			case "rerank-model":
				return []router.Attempt{{ProviderID: "plug", Model: "rerank-model"}}
			}
			return nil
		},
	})
}

// pluginFeedCounts decodes GET /dashboard/plugins into
// per-plugin, per-hook counts.
type pluginFeedCounts struct {
	Plugins []struct {
		Name  string `json:"name"`
		Stats map[string]struct {
			Calls   uint64 `json:"calls"`
			Errors  uint64 `json:"errors"`
			Denials uint64 `json:"denials"`
			Panics  uint64 `json:"panics"`
		} `json:"stats"`
		Recent []struct {
			Model            string `json:"model"`
			FinishReason     string `json:"finish_reason"`
			ContentChars     int    `json:"content_chars"`
			PromptTokens     int    `json:"prompt_tokens"`
			CompletionTokens int    `json:"completion_tokens"`
		} `json:"recent"`
	} `json:"plugins"`
}

func fetchPluginFeed(t *testing.T, s *Server) pluginFeedCounts {
	t.Helper()
	feed := doReq(t, s, http.MethodGet, "/dashboard/plugins", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("plugins feed status = %d", feed.Code)
	}
	var out pluginFeedCounts
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode feed: %v (%s)", err, feed.Body.String())
	}
	return out
}

func (f pluginFeedCounts) calls(t *testing.T, plugin, hook string) uint64 {
	t.Helper()
	for _, p := range f.Plugins {
		if p.Name == plugin {
			return p.Stats[hook].Calls
		}
	}
	t.Fatalf("plugin %q not in feed %+v", plugin, f.Plugins)
	return 0
}

// TestEmbeddingsPluginsBuiltinsEndToEnd is the CHECKLIST 219-220
// server gate: one POST /v1/embeddings and one POST /v1/rerank
// through the real server, the real router.Engine and a real
// provider client against a local mock, with both built-in
// plugins enabled — the header-add plugin's header must arrive
// at the upstream on BOTH surfaces, exactly once each, the
// response-log plugin must record both delivered responses, and
// GET /dashboard/plugins must show both surfaces' hook calls in
// its counts. No fakeEngine anywhere.
func TestEmbeddingsPluginsBuiltinsEndToEnd(t *testing.T) {
	mock := &embedPluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newEmbedPluginEngine(t, srv)

	reg := plugins.New()
	if err := plugins.LoadBuiltins(reg, []string{plugins.HeaderAddName, plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))

	rec := doEmbedReq(t, s, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "0.1") {
		t.Fatalf("embeddings body = %s, want the mock's vector", rec.Body.String())
	}
	rec = doEmbedReq(t, s, "/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d1","d2"]}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rerank status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "0.9") {
		t.Fatalf("rerank body = %s, want the mock's score", rec.Body.String())
	}
	// Exactly one upstream attempt per surface: no retry, no replay.
	if got := mock.embedHits.Load(); got != 1 {
		t.Fatalf("embeddings mock hits = %d, want 1", got)
	}
	if got := mock.rerankHits.Load(); got != 1 {
		t.Fatalf("rerank mock hits = %d, want 1", got)
	}
	// The header-add plugin's header reached the upstream on both.
	if h, _ := mock.embedHead.Load().(string); h != "header-add" {
		t.Fatalf("embeddings upstream saw X-Codvian-Plugin = %q, want %q", h, "header-add")
	}
	if h, _ := mock.rerankHead.Load().(string); h != "header-add" {
		t.Fatalf("rerank upstream saw X-Codvian-Plugin = %q, want %q", h, "header-add")
	}
	// The dashboard feed counts both surfaces' hook calls.
	feed := fetchPluginFeed(t, s)
	if got := feed.calls(t, plugins.HeaderAddName, "pre_request"); got != 2 {
		t.Fatalf("header-add pre_request calls = %d, want 2 (one per surface)", got)
	}
	if got := feed.calls(t, plugins.ResponseLogName, "post_response"); got != 2 {
		t.Fatalf("response-log post_response calls = %d, want 2 (one per surface)", got)
	}
	var recentCount int
	byModel := map[string]int{}
	for _, p := range feed.Plugins {
		if p.Name == plugins.ResponseLogName {
			recentCount = len(p.Recent)
			for _, ev := range p.Recent {
				byModel[ev.Model] = ev.PromptTokens
			}
		}
	}
	if recentCount != 2 {
		t.Fatalf("response-log recent = %d events, want 2 (embeddings + rerank)", recentCount)
	}
	if byModel["emb-model"] != 5 {
		t.Fatalf("recent emb-model prompt tokens = %d, want 5: %+v", byModel["emb-model"], byModel)
	}
	if _, ok := byModel["rerank-model"]; !ok {
		t.Fatalf("recent has no rerank-model event: %+v", byModel)
	}
}

// TestEmbeddingsPluginDenyFailsClosed proves a denying plugin
// refuses both surfaces with 403 / -32001 before any provider
// is dialled — the engine is never called, so no retry can
// happen and the replay exemption holds trivially: zero
// upstream attempts, and the denial is counted per surface.
func TestEmbeddingsPluginDenyFailsClosed(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(denyPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	for _, tc := range []struct{ path, body string }{
		{"/v1/embeddings", `{"model":"emb-model","input":"hello"}`},
		{"/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d"]}`},
	} {
		rec := doEmbedReq(t, s, tc.path, tc.body, "test-key-123", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"code":-32001`) || !strings.Contains(rec.Body.String(), "plugin_denied") {
			t.Fatalf("%s body = %s, want 403 / -32001 / plugin_denied", tc.path, rec.Body.String())
		}
	}
	if eng.embedCalls != 0 || eng.rerankCalls != 0 {
		t.Fatalf("engine calls = embeddings %d / rerank %d, want 0 / 0 (denied before dialling, no retry)", eng.embedCalls, eng.rerankCalls)
	}
	feed := fetchPluginFeed(t, s)
	if got := feed.calls(t, "test-denier", "pre_request"); got != 2 {
		t.Fatalf("denier pre_request calls = %d, want 2", got)
	}
	var denials uint64
	for _, p := range feed.Plugins {
		denials += p.Stats["pre_request"].Denials
	}
	if denials != 2 {
		t.Fatalf("denials = %d, want 2 (one per surface)", denials)
	}
}

// TestEmbeddingsPluginErrorFailsClosed proves a plugin that
// errors also refuses both surfaces (403) without dialling.
func TestEmbeddingsPluginErrorFailsClosed(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(errorPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	for _, tc := range []struct{ path, body string }{
		{"/v1/embeddings", `{"model":"emb-model","input":"hello"}`},
		{"/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d"]}`},
	} {
		rec := doEmbedReq(t, s, tc.path, tc.body, "test-key-123", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s status = %d, want 403: %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	if eng.embedCalls != 0 || eng.rerankCalls != 0 {
		t.Fatalf("engine calls = embeddings %d / rerank %d, want 0 / 0 (error fails closed)", eng.embedCalls, eng.rerankCalls)
	}
}

// TestEmbeddingsPluginPanicIsolated proves a panicking plugin
// fails both surfaces honestly with 502 / -32603 (the 165-166
// panic rule) and the gateway survives: a server with the
// built-ins then serves embeddings normally.
func TestEmbeddingsPluginPanicIsolated(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(panicPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	for _, tc := range []struct{ path, body string }{
		{"/v1/embeddings", `{"model":"emb-model","input":"hello"}`},
		{"/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d"]}`},
	} {
		rec := doEmbedReq(t, s, tc.path, tc.body, "test-key-123", nil)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s status = %d, want 502: %s", tc.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "-32603") {
			t.Fatalf("%s body = %s, want JSON-RPC code -32603", tc.path, rec.Body.String())
		}
	}
	if eng.embedCalls != 0 || eng.rerankCalls != 0 {
		t.Fatalf("engine calls = embeddings %d / rerank %d, want 0 / 0 (panic fails closed before dialling)", eng.embedCalls, eng.rerankCalls)
	}
	feed := fetchPluginFeed(t, s)
	var panics uint64
	for _, p := range feed.Plugins {
		panics += p.Stats["pre_request"].Panics
	}
	if panics != 2 {
		t.Fatalf("pre-request panics = %d, want 2 (one per surface)", panics)
	}
	// Gateway survives: a server with the built-ins serves.
	reg2 := plugins.New()
	if err := plugins.LoadBuiltins(reg2, []string{plugins.HeaderAddName, plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s2 := newTestServer(t, eng, WithPlugins(reg2), WithTimeout(10*time.Second))
	rec := doEmbedReq(t, s2, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-panic embeddings status = %d, want 200 (gateway survived): %s", rec.Code, rec.Body.String())
	}
}

// errorPostPlugin is a test-only post-response plugin that always
// errors (fail-open path).
type errorPostPlugin struct{}

func (errorPostPlugin) Name() string        { return "test-post-error" }
func (errorPostPlugin) Description() string { return "test-only post-response error" }
func (errorPostPlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPostResponse}
}
func (errorPostPlugin) PostResponse(_ context.Context, _ *plugins.Request, _ *provider.ChatResponse) error {
	return errors.New("log sink down")
}

// panicPostPlugin is a test-only post-response plugin that always
// panics (fail-open path).
type panicPostPlugin struct{}

func (panicPostPlugin) Name() string        { return "test-post-panicker" }
func (panicPostPlugin) Description() string { return "test-only post-response panicker" }
func (panicPostPlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPostResponse}
}
func (panicPostPlugin) PostResponse(_ context.Context, _ *plugins.Request, _ *provider.ChatResponse) error {
	panic("test post-response boom")
}

// TestEmbeddingsPluginPostResponseFailsOpen proves a
// post-response plugin that errors or panics never withholds an
// embeddings/rerank response: both surfaces still answer 200
// with the upstream body intact, and the failures are counted.
func TestEmbeddingsPluginPostResponseFailsOpen(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(errorPostPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(panicPostPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	rec := doEmbedReq(t, s, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "0.1") {
		t.Fatalf("embeddings status = %d, body = %s, want 200 with the vector intact", rec.Code, rec.Body.String())
	}
	rec = doEmbedReq(t, s, "/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d"]}`, "test-key-123", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "0.92") {
		t.Fatalf("rerank status = %d, body = %s, want 200 with the score intact", rec.Code, rec.Body.String())
	}
	feed := fetchPluginFeed(t, s)
	if got := feed.calls(t, "test-post-error", "post_response"); got != 2 {
		t.Fatalf("post-error post_response calls = %d, want 2", got)
	}
	var errs, panics uint64
	for _, p := range feed.Plugins {
		errs += p.Stats["post_response"].Errors
		panics += p.Stats["post_response"].Panics
	}
	if errs != 2 || panics != 2 {
		t.Fatalf("post_response errors = %d, panics = %d, want 2 / 2", errs, panics)
	}
}

// TestEmbeddingsPluginPostResponseSkippedOnError proves the
// post-response hook runs only on a delivered response: when the
// single upstream attempt fails, no post-response call is made
// and the engine was still called exactly once.
func TestEmbeddingsPluginPostResponseSkippedOnError(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}, embedErr: &provider.UpstreamError{StatusCode: 500, Body: "boom"}}
	reg := plugins.New()
	if err := plugins.LoadBuiltins(reg, []string{plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	rec := doEmbedReq(t, s, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if eng.embedCalls != 1 {
		t.Fatalf("embed calls = %d, want exactly 1 (no retry on failure)", eng.embedCalls)
	}
	feed := fetchPluginFeed(t, s)
	if got := feed.calls(t, plugins.ResponseLogName, "post_response"); got != 0 {
		t.Fatalf("response-log post_response calls = %d, want 0 (no delivered response)", got)
	}
}

// modelRewritePlugin is a test-only pre-request plugin that
// rewrites the request model in place.
type modelRewritePlugin struct{}

func (modelRewritePlugin) Name() string        { return "test-model-rewriter" }
func (modelRewritePlugin) Description() string { return "test-only model rewriter" }
func (modelRewritePlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPreRequest}
}
func (modelRewritePlugin) PreRequest(_ context.Context, req *plugins.Request) (bool, string, error) {
	req.Chat.Model = "rewritten-model"
	return false, "", nil
}

// TestEmbeddingsPluginModelRewriteSyncs proves a pre-request
// plugin's model rewrite reaches the real embeddings/rerank
// request handed to the engine (not just the bookkeeping copy).
func TestEmbeddingsPluginModelRewriteSyncs(t *testing.T) {
	eng := &embedFakeEngine{fakeEngine: &fakeEngine{}}
	reg := plugins.New()
	if err := reg.Register(modelRewritePlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	rec := doEmbedReq(t, s, "/v1/embeddings", `{"model":"emb-model","input":"hello"}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("embeddings status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.lastEmbedModel != "rewritten-model" {
		t.Fatalf("engine saw embeddings model = %q, want %q", eng.lastEmbedModel, "rewritten-model")
	}
	rec = doEmbedReq(t, s, "/v1/rerank", `{"model":"rerank-model","query":"q","documents":["d"]}`, "test-key-123", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rerank status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.lastRerankModel != "rewritten-model" {
		t.Fatalf("engine saw rerank model = %q, want %q", eng.lastRerankModel, "rewritten-model")
	}
}
