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

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/plugins"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// denyPlugin is a test-only pre-request plugin that refuses every
// request with a fixed reason.
type denyPlugin struct{}

func (denyPlugin) Name() string        { return "test-denier" }
func (denyPlugin) Description() string { return "test-only denier" }
func (denyPlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPreRequest}
}
func (denyPlugin) PreRequest(_ context.Context, _ *plugins.Request) (bool, string, error) {
	return true, "test policy", nil
}

// errorPlugin is a test-only pre-request plugin that always errors
// (fail-closed path).
type errorPlugin struct{}

func (errorPlugin) Name() string        { return "test-error" }
func (errorPlugin) Description() string { return "test-only error" }
func (errorPlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPreRequest}
}
func (errorPlugin) PreRequest(_ context.Context, _ *plugins.Request) (bool, string, error) {
	return false, "", errors.New("classifier down")
}

// panicPlugin is a test-only pre-request plugin that always panics
// (panic-isolation path).
type panicPlugin struct{}

func (panicPlugin) Name() string        { return "test-panicker" }
func (panicPlugin) Description() string { return "test-only panicker" }
func (panicPlugin) Hooks() []plugins.HookKind {
	return []plugins.HookKind{plugins.HookPreRequest}
}
func (panicPlugin) PreRequest(_ context.Context, _ *plugins.Request) (bool, string, error) {
	panic("test plugin boom")
}

// pluginMockUpstream serves one chat model and records the plugin
// header it saw, if any.
type pluginMockUpstream struct {
	hits       atomic.Int32
	pluginHead atomic.Value // string
}

func (m *pluginMockUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		m.hits.Add(1)
		m.pluginHead.Store(r.Header.Get("X-Codvian-Plugin"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-plugin","object":"chat.completion","model":"plugin-model","choices":[{"message":{"role":"assistant","content":"plugin says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
	}
}

func newPluginEngine(t *testing.T, upstream *httptest.Server) *router.Engine {
	t.Helper()
	client, err := provider.NewClient(upstream.URL, "plugin-key", provider.WithTimeout(2*time.Second))
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
			if model != "plugin-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "plug", Model: "plugin-model"}}
		},
	})
}

// TestPluginsBuiltinsEndToEnd is the CHECKLIST 201-202 server gate:
// one POST /v1/chat/completions through the real server, the real
// router.Engine and a real provider client against a local mock,
// with both built-in plugins enabled — the header-add plugin's
// header must arrive at the upstream, the response-log plugin
// must record the response, and GET /dashboard/plugins must show
// both with real counts. No fakeEngine anywhere.
func TestPluginsBuiltinsEndToEnd(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngine(t, srv)

	reg := plugins.New()
	if err := plugins.LoadBuiltins(reg, []string{plugins.HeaderAddName, plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second))

	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "plugin says hi") {
		t.Fatalf("body = %s, want the mock's content", resp.Body.String())
	}
	// The header-add plugin's header reached the upstream.
	if got := mock.hits.Load(); got != 1 {
		t.Fatalf("mock hits = %d, want 1", got)
	}
	if h, _ := mock.pluginHead.Load().(string); h != "header-add" {
		t.Fatalf("upstream saw X-Codvian-Plugin = %q, want %q", h, "header-add")
	}
	// The dashboard feed shows both plugins with real counts.
	feed := doReq(t, s, http.MethodGet, "/dashboard/plugins", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("plugins feed status = %d", feed.Code)
	}
	var out struct {
		Plugins []struct {
			Name  string `json:"name"`
			Stats map[string]struct {
				Calls   uint64 `json:"calls"`
				Errors  uint64 `json:"errors"`
				Denials uint64 `json:"denials"`
				Panics  uint64 `json:"panics"`
			} `json:"stats"`
			Recent []struct {
				Model        string `json:"model"`
				FinishReason string `json:"finish_reason"`
				ContentChars int    `json:"content_chars"`
			} `json:"recent"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode feed: %v (%s)", err, feed.Body.String())
	}
	if len(out.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2: %s", len(out.Plugins), feed.Body.String())
	}
	byName := map[string]int{}
	for i, p := range out.Plugins {
		byName[p.Name] = i
	}
	ha := out.Plugins[byName[plugins.HeaderAddName]]
	if ha.Stats["pre_request"].Calls != 1 {
		t.Fatalf("header-add pre_request calls = %d, want 1", ha.Stats["pre_request"].Calls)
	}
	rl := out.Plugins[byName[plugins.ResponseLogName]]
	if rl.Stats["post_response"].Calls != 1 {
		t.Fatalf("response-log post_response calls = %d, want 1", rl.Stats["post_response"].Calls)
	}
	if len(rl.Recent) != 1 || rl.Recent[0].Model != "plugin-model" || rl.Recent[0].ContentChars != len("plugin says hi") {
		t.Fatalf("response-log recent = %+v, want 1 shape-only event", rl.Recent)
	}
}

// TestPluginsDenyFailsClosed proves a denying plugin refuses the
// request with 403 / -32001 before any provider is dialled: the
// mock is never hit, and the response carries reason
// "plugin_denied".
func TestPluginsDenyFailsClosed(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngine(t, srv)

	reg := plugins.New()
	if err := reg.Register(denyPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), `"code":-32001`) || !strings.Contains(resp.Body.String(), "plugin_denied") {
		t.Fatalf("body = %s, want 403 / -32001 / plugin_denied", resp.Body.String())
	}
	if got := mock.hits.Load(); got != 0 {
		t.Fatalf("mock hits = %d, want 0 (denied before dialling)", got)
	}
	// The denial is counted in the feed.
	feed := doReq(t, s, http.MethodGet, "/dashboard/plugins", "", "")
	var out struct {
		Plugins []struct {
			Stats map[string]struct {
				Denials uint64 `json:"denials"`
			} `json:"stats"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Plugins) != 1 || out.Plugins[0].Stats["pre_request"].Denials != 1 {
		t.Fatalf("feed = %s, want 1 denial counted", feed.Body.String())
	}
}

// TestPluginsErrorFailsClosed proves a plugin that errors also
// refuses the request (403) without dialling the provider.
func TestPluginsErrorFailsClosed(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngine(t, srv)

	reg := plugins.New()
	if err := reg.Register(errorPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.Code, resp.Body.String())
	}
	if got := mock.hits.Load(); got != 0 {
		t.Fatalf("mock hits = %d, want 0 (error fails closed)", got)
	}
}

// TestPluginsPanicIsolated proves a panicking plugin fails the
// request honestly with 502 / -32603 (the 165-166 panic rule) and
// the gateway survives: the very next request — with the panicker
// replaced by the built-ins — succeeds.
func TestPluginsPanicIsolated(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngine(t, srv)

	reg := plugins.New()
	if err := reg.Register(panicPlugin{}, false); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "-32603") {
		t.Fatalf("body = %s, want JSON-RPC code -32603", resp.Body.String())
	}
	if got := mock.hits.Load(); got != 0 {
		t.Fatalf("mock hits = %d, want 0 (panic fails closed before dialling)", got)
	}
	// Gateway survives: a server with the built-ins serves.
	reg2 := plugins.New()
	if err := plugins.LoadBuiltins(reg2, []string{plugins.HeaderAddName, plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s2 := newTestServer(t, eng, WithPlugins(reg2), WithTimeout(10*time.Second))
	resp2 := doReq(t, s2, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp2.Code != http.StatusOK {
		t.Fatalf("post-panic status = %d, want 200 (gateway survived): %s", resp2.Code, resp2.Body.String())
	}
}

// TestPluginsEmptyFeedHonest proves GET /dashboard/plugins with no
// registry reports zero plugins and says so, instead of inventing
// any.
func TestPluginsEmptyFeedHonest(t *testing.T) {
	s := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}})
	feed := doReq(t, s, http.MethodGet, "/dashboard/plugins", "", "")
	if feed.Code != http.StatusOK {
		t.Fatalf("status = %d", feed.Code)
	}
	var out struct {
		Plugins       []any  `json:"plugins"`
		InProcessOnly bool   `json:"in_process_only"`
		Note          string `json:"note"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Plugins) != 0 {
		t.Fatalf("plugins = %d, want 0", len(out.Plugins))
	}
	if !out.InProcessOnly || !strings.Contains(out.Note, "No plugins are registered") {
		t.Fatalf("feed = %s, want in-process-only note + honest empty state", feed.Body.String())
	}
}

// TestPluginsStreamPostResponse proves the post-response hook also
// runs for streaming requests (after the stream completes).
func TestPluginsStreamPostResponse(t *testing.T) {
	eng := &fakeEngine{
		chatResp: provider.ChatResponse{Model: "plugin-model", Content: "streamed", FinishReason: "stop"},
		deltas:   []string{"stream", "ed"},
	}
	reg := plugins.New()
	if err := plugins.LoadBuiltins(reg, []string{plugins.ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))
	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.Code, resp.Body.String())
	}
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].Stats[plugins.HookPostResponse].Calls != 1 {
		t.Fatalf("snapshot = %+v, want 1 post-response call", snap)
	}
	if len(snap[0].Recent) != 1 || snap[0].Recent[0].ContentChars != len("streamed") {
		t.Fatalf("recent = %+v, want the streamed response shape", snap[0].Recent)
	}
}

// TestPluginsCostCapEndToEnd is the CHECKLIST 239-240 server gate:
// the real cost-cap plugin on the real chat path against a real
// mock upstream. An over-cap request is refused 402 naming the
// estimated cost and the cap with ZERO provider calls; a cheap
// request passes through unchanged (mock hit once, content
// delivered); an unpriced model is refused 402 naming the missing
// price; and GET /dashboard/plugins shows the cost-cap row with
// the real decisions (2 allowed + 2 denied across the calls).
func TestPluginsCostCapEndToEnd(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngine(t, srv)

	// plugin-model: $1 in / $2 out per 1M tokens. Cap $0.005:
	// "hi" costs ~$0.00206 (7 prompt tokens + 1024 assumed
	// completion) so it passes; a 600-sentence prompt costs
	// ~$0.008 so it is refused.
	cap, err := plugins.NewCostCapPlugin(0.005, func(model string) (float64, float64, bool) {
		if model == "plugin-model" {
			return 1, 2, true
		}
		return 0, 0, false
	})
	if err != nil {
		t.Fatal(err)
	}
	reg := plugins.New()
	if err := reg.Register(cap, true); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	// Over cap: 402 naming cost + cap, zero provider calls.
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 600)
	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"`+long+`"}]}`, "test-key-123")
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("over-cap status = %d, want 402: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "estimated cost $") || !strings.Contains(resp.Body.String(), "cap $0.005000") {
		t.Fatalf("over-cap body = %s, want estimated cost + cap named", resp.Body.String())
	}
	if got := mock.hits.Load(); got != 0 {
		t.Fatalf("mock hits = %d, want 0 (denied before dialling)", got)
	}

	// Under cap: 200, mock hit once, content delivered unchanged.
	resp = doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"plugin-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "plugin says hi") {
		t.Fatalf("under-cap status = %d, body = %s, want 200 with the mock content", resp.Code, resp.Body.String())
	}
	if got := mock.hits.Load(); got != 1 {
		t.Fatalf("mock hits = %d, want 1 (allowed request dialled once)", got)
	}

	// Unpriced model: 402 naming the missing price, still zero
	// extra provider calls (the denial happens before routing).
	resp = doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"unpriced-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("unpriced status = %d, want 402: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "no complete model-level price") {
		t.Fatalf("unpriced body = %s, want the missing price named", resp.Body.String())
	}
	if got := mock.hits.Load(); got != 1 {
		t.Fatalf("mock hits = %d, want still 1", got)
	}

	// The feed shows the cost-cap row with the real decisions:
	// 3 pre-request calls, 2 denials (over-cap + unpriced).
	feed := doReq(t, s, http.MethodGet, "/dashboard/plugins", "", "")
	var out struct {
		Plugins []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Stats       map[string]struct {
				Calls   uint64 `json:"calls"`
				Denials uint64 `json:"denials"`
			} `json:"stats"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(feed.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Plugins) != 1 || out.Plugins[0].Name != "cost-cap" {
		t.Fatalf("feed = %s, want exactly the cost-cap row", feed.Body.String())
	}
	if !strings.Contains(out.Plugins[0].Description, "$0.005000") {
		t.Fatalf("description = %q, want the configured cap in it", out.Plugins[0].Description)
	}
	st := out.Plugins[0].Stats["pre_request"]
	if st.Calls != 3 || st.Denials != 2 {
		t.Fatalf("feed stats = %+v, want 3 calls / 2 denials", st)
	}
}

// costCapLookupFromBook builds the cost-cap plugin's price lookup
// from a real price book — the same construction main.go's
// buildPluginRegistry uses (model-level entries only, both sides
// required). Tests that need the real wiring use this, never a
// hand-rolled lookup, so a wiring drift fails the test.
func costCapLookupFromBook(book *config.PriceBook) plugins.PriceLookup {
	return func(model string) (float64, float64, bool) {
		p, ok := book.LookupModel(model)
		if !ok || p.InputUSDPer1M == nil || p.OutputUSDPer1M == nil {
			return 0, 0, false
		}
		return *p.InputUSDPer1M, *p.OutputUSDPer1M, true
	}
}

// newPluginEngineForModels is newPluginEngine with a permissive
// chain: any requested model routes to the mock provider, so a
// test can prove an allowed request actually reaches the upstream
// (200 + one hit) instead of failing on chain resolution.
func newPluginEngineForModels(t *testing.T, upstream *httptest.Server) *router.Engine {
	t.Helper()
	client, err := provider.NewClient(upstream.URL, "plugin-key", provider.WithTimeout(2*time.Second))
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
			return []router.Attempt{{ProviderID: "plug", Model: model}}
		},
	})
}

// TestPluginsCostCapPairPriceOnlyStillRefused is the CHECKLIST 319
// pin: a model priced ONLY per (provider, model) pair — no
// model-level price anywhere — is refused 402 naming the missing
// model-level price, with zero provider calls. A pair price never
// satisfies the cap, because the plugin runs before routing and
// cannot know which provider would serve the request. The lookup
// is built from a real price book the way main does; the positive
// control adds the model-level price to the same book and proves
// the same model is then allowed through to the mock (200, one
// hit) — so the refusal is about the missing model-level price,
// not the model.
func TestPluginsCostCapPairPriceOnlyStillRefused(t *testing.T) {
	mock := &pluginMockUpstream{}
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	eng := newPluginEngineForModels(t, srv)

	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "plug", Model: "pair-model"}: {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)},
		},
		Entries: map[string]config.ModelPrice{},
	}
	cap, err := plugins.NewCostCapPlugin(0.005, costCapLookupFromBook(book))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugins.New()
	if err := reg.Register(cap, true); err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, eng, WithPlugins(reg), WithTimeout(10*time.Second))

	resp := doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"pair-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusPaymentRequired {
		t.Fatalf("pair-only status = %d, want 402: %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(resp.Body.String(), "no complete model-level price") ||
		!strings.Contains(resp.Body.String(), "pair-model") {
		t.Fatalf("pair-only body = %s, want the missing model-level price named for pair-model", resp.Body.String())
	}
	if got := mock.hits.Load(); got != 0 {
		t.Fatalf("mock hits = %d, want 0 (denied before dialling)", got)
	}

	// Positive control: the same book gains the model-level price
	// and the same request is allowed through to the mock. The
	// lookup closure reads the live book, so no rebuild is needed.
	book.Entries["pair-model"] = config.ModelPrice{InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)}
	resp = doReq(t, s, http.MethodPost, "/v1/chat/completions",
		`{"model":"pair-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "plugin says hi") {
		t.Fatalf("model-level-priced status = %d, body = %s, want 200 with the mock content", resp.Code, resp.Body.String())
	}
	if got := mock.hits.Load(); got != 1 {
		t.Fatalf("mock hits = %d, want 1 (allowed request dialled once)", got)
	}
}

// TestPluginsCostCapFeedUnpricedModels is the CHECKLIST 319-320
// feed gate: GET /dashboard/plugins carries a cost_cap block
// listing exactly the served models without a complete
// model-level price, with has_pair_price telling the operator
// which ones they priced per-provider only.
//
// Setup: registry serves priced-model (model-level price),
// pair-model (pair price only), bare-model (no price at all) and
// half-model (model-level price with the output side missing —
// half-known is unknown). Cap $0.005 is on.
func TestPluginsCostCapFeedUnpricedModels(t *testing.T) {
	book := &config.PriceBook{
		Pairs: map[config.PriceKey]config.ModelPrice{
			{Provider: "plug", Model: "pair-model"}: {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)},
		},
		Entries: map[string]config.ModelPrice{
			"priced-model": {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)},
			"half-model":   {InputUSDPer1M: pricePtr(1)},
		},
	}
	cap, err := plugins.NewCostCapPlugin(0.005, costCapLookupFromBook(book))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugins.New()
	if err := reg.Register(cap, true); err != nil {
		t.Fatal(err)
	}
	registry := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "priced-model", ProviderName: "plug"},
		{ID: "pair-model", ProviderName: "plug"},
		{ID: "bare-model", ProviderName: "plug"},
		{ID: "half-model", ProviderName: "plug"},
	}}
	s := newTestServer(t, &fakeEngine{}, WithPlugins(reg), WithRegistry(registry), WithPriceBook(book))

	var feed struct {
		CostCap struct {
			Enabled          bool    `json:"enabled"`
			CapUSDPerRequest float64 `json:"cap_usd_per_request"`
			ModelsChecked    int     `json:"models_checked"`
			Unpriced         []struct {
				Model        string `json:"model"`
				HasPairPrice bool   `json:"has_pair_price"`
			} `json:"unpriced_models"`
			Note string `json:"note"`
		} `json:"cost_cap"`
	}
	getJSON(t, s, "/dashboard/plugins", &feed)
	cc := feed.CostCap
	if !cc.Enabled || cc.CapUSDPerRequest != 0.005 {
		t.Fatalf("cost_cap = %+v, want enabled with cap 0.005", cc)
	}
	if cc.ModelsChecked != 4 {
		t.Fatalf("models_checked = %d, want 4", cc.ModelsChecked)
	}
	// Sorted by model: bare-model (no pair), half-model (no
	// pair — its half model-level price does not count),
	// pair-model (pair price, no model-level price).
	want := []struct {
		model string
		pair  bool
	}{
		{"bare-model", false}, {"half-model", false}, {"pair-model", true},
	}
	if len(cc.Unpriced) != len(want) {
		t.Fatalf("unpriced_models = %+v, want %v", cc.Unpriced, want)
	}
	for i, w := range want {
		if cc.Unpriced[i].Model != w.model || cc.Unpriced[i].HasPairPrice != w.pair {
			t.Fatalf("unpriced_models[%d] = %+v, want %+v", i, cc.Unpriced[i], w)
		}
	}
	if !strings.Contains(cc.Note, "refused 402") {
		t.Fatalf("note = %q, want the 402 consequence named", cc.Note)
	}

	// Cap off (unlimited plugin): enabled, cap 0, no model is
	// listed — nothing is capped, so nothing is refused for price.
	unlimited, err := plugins.NewCostCapPlugin(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg2 := plugins.New()
	if err := reg2.Register(unlimited, true); err != nil {
		t.Fatal(err)
	}
	s2 := newTestServer(t, &fakeEngine{}, WithPlugins(reg2), WithRegistry(registry), WithPriceBook(book))
	var feed2 struct {
		CostCap struct {
			Enabled          bool    `json:"enabled"`
			CapUSDPerRequest float64 `json:"cap_usd_per_request"`
			Unpriced         []any   `json:"unpriced_models"`
			Note             string  `json:"note"`
		} `json:"cost_cap"`
	}
	getJSON(t, s2, "/dashboard/plugins", &feed2)
	if !feed2.CostCap.Enabled || feed2.CostCap.CapUSDPerRequest != 0 || len(feed2.CostCap.Unpriced) != 0 {
		t.Fatalf("unlimited cost_cap = %+v, want enabled, cap 0, no unpriced models", feed2.CostCap)
	}
	if !strings.Contains(feed2.CostCap.Note, "No cap configured") {
		t.Fatalf("unlimited note = %q, want the no-cap state named", feed2.CostCap.Note)
	}

	// Plugin absent: disabled, empty list, honest note.
	s3 := newTestServer(t, &fakeEngine{}, WithRegistry(registry), WithPriceBook(book))
	var feed3 struct {
		CostCap struct {
			Enabled  bool   `json:"enabled"`
			Unpriced []any  `json:"unpriced_models"`
			Note     string `json:"note"`
		} `json:"cost_cap"`
	}
	getJSON(t, s3, "/dashboard/plugins", &feed3)
	if feed3.CostCap.Enabled || len(feed3.CostCap.Unpriced) != 0 {
		t.Fatalf("absent cost_cap = %+v, want disabled with no unpriced models", feed3.CostCap)
	}
	if !strings.Contains(feed3.CostCap.Note, "not enabled") {
		t.Fatalf("absent note = %q, want the not-enabled state named", feed3.CostCap.Note)
	}

	// No price book at all: every served model is unpriced, none
	// has a pair price.
	s4 := newTestServer(t, &fakeEngine{}, WithPlugins(reg), WithRegistry(registry))
	var feed4 struct {
		CostCap struct {
			ModelsChecked int `json:"models_checked"`
			Unpriced      []struct {
				Model        string `json:"model"`
				HasPairPrice bool   `json:"has_pair_price"`
			} `json:"unpriced_models"`
			Note string `json:"note"`
		} `json:"cost_cap"`
	}
	getJSON(t, s4, "/dashboard/plugins", &feed4)
	if feed4.CostCap.ModelsChecked != 4 || len(feed4.CostCap.Unpriced) != 4 {
		t.Fatalf("no-book cost_cap = %+v, want 4 checked / 4 unpriced", feed4.CostCap)
	}
	for _, u := range feed4.CostCap.Unpriced {
		if u.HasPairPrice {
			t.Fatalf("no-book unpriced = %+v, want has_pair_price false throughout", feed4.CostCap.Unpriced)
		}
	}
	if !strings.Contains(feed4.CostCap.Note, "No prices configured") {
		t.Fatalf("no-book note = %q, want the no-prices state named", feed4.CostCap.Note)
	}
}

// TestPluginsCostCapFeedAllPriced proves the honest empty state
// the other way: when every served model has a complete
// model-level price, the list is empty and the note says so.
func TestPluginsCostCapFeedAllPriced(t *testing.T) {
	book := &config.PriceBook{
		Entries: map[string]config.ModelPrice{
			"priced-model": {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)},
		},
	}
	cap, err := plugins.NewCostCapPlugin(0.005, costCapLookupFromBook(book))
	if err != nil {
		t.Fatal(err)
	}
	reg := plugins.New()
	if err := reg.Register(cap, true); err != nil {
		t.Fatal(err)
	}
	registry := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "priced-model", ProviderName: "plug"},
	}}
	s := newTestServer(t, &fakeEngine{}, WithPlugins(reg), WithRegistry(registry), WithPriceBook(book))
	var feed struct {
		CostCap struct {
			Unpriced []any  `json:"unpriced_models"`
			Note     string `json:"note"`
		} `json:"cost_cap"`
	}
	getJSON(t, s, "/dashboard/plugins", &feed)
	if len(feed.CostCap.Unpriced) != 0 {
		t.Fatalf("unpriced_models = %+v, want empty", feed.CostCap.Unpriced)
	}
	if !strings.Contains(feed.CostCap.Note, "Every served model has a complete model-level price") {
		t.Fatalf("note = %q, want the all-priced state named", feed.CostCap.Note)
	}
}
