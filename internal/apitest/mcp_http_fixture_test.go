package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// ---------------------------------------------------------------------------
// Test servers (plain + ledger-backed), built only from exported API.
// They mirror the shapes in internal/server/mcp_http_test.go
// (newMCPHTTPTestServer / TestMCPHTTPM93LedgerBackend), which live in
// _test.go files and are not importable from here.
//
// This construction lives in this _test.go file, not in the runner
// (mcp_http_fixture.go), on purpose: internal/server's test files
// import internal/apitest, so the apitest library must never import
// internal/server or building the server test binary hits an import
// cycle. A package's own _test.go files are never compiled when
// another package imports it, so importing server here is safe.
// ---------------------------------------------------------------------------

// mcpHTTPStubEngine is the minimal Engine behind the HTTP fixture
// servers: Chat/ChatStream answer a canned reply (no MCP tool calls
// the engine's Chat path), and it implements the strategyReporter
// interface the server type-asserts for set_routing_strategy, so a
// strategy change applies at runtime as well as persisting — the
// same shape as the strategyFakeEngine in internal/server tests,
// rebuilt here because that type lives in a _test.go file.
type mcpHTTPStubEngine struct {
	mu     sync.Mutex
	global string
	chains map[string]string
}

func (e *mcpHTTPStubEngine) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi", Model: "stub"}, nil
}

func (e *mcpHTTPStubEngine) ChatStream(_ context.Context, _ provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta != nil {
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "hi", Model: "stub"}, nil
}

func (e *mcpHTTPStubEngine) LastRouting() (router.RoutingInfo, bool) {
	return router.RoutingInfo{}, false
}

func (e *mcpHTTPStubEngine) StrategyState() (string, map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.chains))
	for k, v := range e.chains {
		out[k] = v
	}
	return e.global, out
}

func (e *mcpHTTPStubEngine) SetGlobalStrategy(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.global = s
}

func (e *mcpHTTPStubEngine) SetChainStrategy(model, s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.chains == nil {
		e.chains = map[string]string{}
	}
	if s == "" {
		delete(e.chains, model)
		return
	}
	e.chains[model] = s
}

// newMCPHTTPStubEngine returns the stub engine for HTTP fixture
// servers. Tests that need to observe the applied strategy read it
// back through StrategyState.
func newMCPHTTPStubEngine() *mcpHTTPStubEngine {
	return &mcpHTTPStubEngine{global: "priority"}
}

// mcpHTTPProviders is the DashboardProviderLister behind the HTTP
// fixture servers: a fixed provider list, in order.
type mcpHTTPProviders struct {
	providers []provider.DashboardProvider
}

func (f mcpHTTPProviders) DashboardProviders() []provider.DashboardProvider {
	return f.providers
}

// mcpHTTPStandardProviders returns the standard fixture provider
// feed: p1 Coder (openai, enabled) and p2 Chatter (anthropic,
// enabled) — the same feed the Streamable tests in
// internal/server use.
func mcpHTTPStandardProviders() server.DashboardProviderLister {
	return mcpHTTPProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
		{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
	}}
}

// mcpHTTPRegistry is the ModelRegistry behind the HTTP fixture
// servers: a fixed model list, in order.
type mcpHTTPRegistry struct {
	models []server.Model
}

func (f mcpHTTPRegistry) Models(_ context.Context) ([]server.Model, error) {
	return f.models, nil
}

// mcpHTTPStandardRegistry returns the standard fixture model feed:
// a and b on p1 (Coder), c on p2 (Chatter).
func mcpHTTPStandardRegistry() server.ModelRegistry {
	return mcpHTTPRegistry{models: []server.Model{
		{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
		{ID: "b", ProviderID: "p1", OwnedBy: "Coder"},
		{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
	}}
}

// newMCPHTTPServer builds the real server from eng and opts and
// serves it on loopback via the runner's handler-agnostic starter.
// It mirrors the server tests' newTestServer (static test key +
// discarded logger). The Streamable transport is still DISABLED
// after this returns — enable it with EnableMCPHTTPStreamable.
func newMCPHTTPServer(t *testing.T, eng server.Engine, opts ...server.Option) *httptest.Server {
	t.Helper()
	all := append([]server.Option{
		server.WithStaticKeys("test-key-123"),
		server.WithLogger(log.New(io.Discard, "", 0)),
	}, opts...)
	return StartMCPHTTPTestServer(t, server.New(eng, all...))
}

// mcpHTTPPlainKeys is the key set of the plain fixture server: one
// key per scope class, 5 keys total (router_status asserts
// gateway_keys == 5).
type mcpHTTPPlainKeys struct {
	admin, read, ops, cache, wide string
}

// newMCPHTTPPlainServer builds the plain fixture server: the
// standard p1/p2 + a/b/c feed, no ledger (pick_fastest_model
// answers "backend is not available"), Streamable enabled.
func newMCPHTTPPlainServer(t *testing.T) (*httptest.Server, mcpHTTPPlainKeys) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	keys := mcpHTTPPlainKeys{}
	issue := func(dst *string, name string, scopes []string) {
		_, raw := IssueMCPHTTPKey(t, cfg, name, scopes)
		*dst = raw
	}
	issue(&keys.admin, "admin", []string{"chat", "admin"})
	issue(&keys.read, "reader", []string{"mcp:read:status"})
	issue(&keys.ops, "ops", []string{"mcp:manage:ops"})
	issue(&keys.cache, "cache", []string{"mcp:manage:cache", "mcp:destructive"})
	issue(&keys.wide, "wide", []string{"mcp:read:*"})
	ts := newMCPHTTPServer(t, newMCPHTTPStubEngine(),
		server.WithConfig(cfg),
		server.WithDashboardProviders(mcpHTTPStandardProviders()),
		server.WithRegistry(mcpHTTPStandardRegistry()),
	)
	EnableMCPHTTPStreamable(t, ts.URL, keys.admin)
	return ts, keys
}

// mcpHTTPLedgerKeys is the key set of the ledger fixture server:
// cost reader/manager, router, wide, admin — plus ops (db_stats
// needs mcp:manage:ops against the live ledger) and the two memory
// keys (memory_add/search need mcp:manage:memory; memory_clear
// additionally needs mcp:destructive, which only memory-destr
// holds — mirroring the M910 server's key set). No fixture pins
// this server's key count, so the extra keys are safe here;
// the plain server must stay at exactly 5 keys (router_status
// pins gateway_keys == 5).
type mcpHTTPLedgerKeys struct {
	admin, costReader, costManager, router, wide, ops, memory, memoryDestr string
}

// newMCPHTTPLedgerServer builds the ledger fixture server: the
// same standard feed, plus a real ledger seeded exactly like
// TestMCPHTTPM93LedgerBackend (reader's own usage: model a 1000
// prompt + 500 completion at 120 ms, model c 100 prompt at 90 ms;
// another key's identical a-usage that must never leak into the
// reader's report), prices a $10/$30 and c $5/$15 per 1M, price
// book, routing group "fast" (p1 + p2), and a routing persist that
// writes through to the config. Streamable enabled.
func newMCPHTTPLedgerServer(t *testing.T) (*httptest.Server, mcpHTTPLedgerKeys) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	keys := mcpHTTPLedgerKeys{}
	issued := map[string]config.GatewayKey{}
	issue := func(dst *string, name string, scopes []string) {
		k, raw := IssueMCPHTTPKey(t, cfg, name, scopes)
		issued[name] = k
		*dst = raw
	}
	issue(&keys.costReader, "cost-reader", []string{"mcp:read:cost"})
	issue(&keys.costManager, "cost-manager", []string{"mcp:manage:cost"})
	issue(&keys.router, "router", []string{"mcp:read:routing", "mcp:manage:routing"})
	issue(&keys.wide, "wide", []string{"mcp:read:*"})
	issue(&keys.admin, "admin", []string{"chat", "admin"})
	issue(&keys.ops, "ops", []string{"mcp:manage:ops"})
	issue(&keys.memory, "memory", []string{"mcp:manage:memory"})
	issue(&keys.memoryDestr, "memory-destr", []string{"mcp:manage:memory", "mcp:destructive"})
	// Combo validation reads providers from cfg (not the dashboard
	// feed), so p1/p2 must exist here for create_combo to validate.
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}

	// The ledger lives in cfg.DataDir (not a second temp dir), so
	// the data-dir tools (db_stats/db_health) see the live file —
	// the same layout the M910 server uses. Before this the file
	// sat in an unrelated temp dir and db_stats on this server
	// reported ledger.db as missing.
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := ledger.Prices{
		ledger.PriceKey{Model: "a"}: {PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
		ledger.PriceKey{Model: "c"}: {PromptPerToken: 5.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
	}
	seed := func(keyID, providerID, model string, prompt, completion int, latency int64) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: providerID, Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: true, LatencyMS: latency,
		}, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	readKeyID := issued["cost-reader"].ID
	seed(readKeyID, "p1", "a", 1000, 500, 120)
	seed(readKeyID, "p2", "c", 100, 0, 90)
	seed("other-key", "p1", "a", 1000, 500, 120)

	book := config.NewPriceBook()
	book.Entries["a"] = config.ModelPrice{InputUSDPer1M: floatPtr(10), OutputUSDPer1M: floatPtr(30)}
	book.Entries["c"] = config.ModelPrice{InputUSDPer1M: floatPtr(5), OutputUSDPer1M: floatPtr(15)}
	// One routing group, so get_combo_metrics has a combo to
	// aggregate (members p1 + p2 cover all three seeded rows).
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()

	ts := newMCPHTTPServer(t, newMCPHTTPStubEngine(),
		server.WithConfig(cfg),
		server.WithLedger(led),
		server.WithLedgerPrices(prices),
		server.WithPriceBook(book),
		server.WithDashboardProviders(mcpHTTPStandardProviders()),
		server.WithRegistry(mcpHTTPStandardRegistry()),
		server.WithRoutingPersist(func(update server.RoutingUpdate) error {
			cfg.EnsureRoutingMu()
			cfg.LockRouting()
			defer cfg.UnlockRouting()
			if cfg.Routing == nil {
				cfg.Routing = &config.RoutingConfig{}
			}
			if update.Global != nil {
				cfg.Routing.GlobalStrategy = *update.Global
			}
			if cfg.Routing.ChainStrategies == nil {
				cfg.Routing.ChainStrategies = map[string]string{}
			}
			for m, st := range update.Chains {
				cfg.Routing.ChainStrategies[m] = st
			}
			return nil
		}),
	)
	EnableMCPHTTPStreamable(t, ts.URL, keys.admin)
	return ts, keys
}

func floatPtr(v float64) *float64 { return &v }

// mcpHTTPKeyFor resolves a spec's key name to the raw key of the
// server shape it runs against.
func mcpHTTPKeyFor(shape string, plain mcpHTTPPlainKeys, led mcpHTTPLedgerKeys, name string) string {
	if shape == "plain" {
		switch name {
		case "admin":
			return plain.admin
		case "read":
			return plain.read
		case "ops":
			return plain.ops
		case "cache":
			return plain.cache
		case "wide":
			return plain.wide
		}
	}
	switch name {
	case "admin":
		return led.admin
	case "cost-reader":
		return led.costReader
	case "cost-manager":
		return led.costManager
	case "router":
		return led.router
	case "wide":
		return led.wide
	case "ops":
		return led.ops
	case "memory":
		return led.memory
	case "memory-destr":
		return led.memoryDestr
	}
	return ""
}

// ---------------------------------------------------------------------------
// Fixture specs — one per recorded exchange. The request ids are
// unique across the whole set, so any subset can run in one session
// in spec order. The specs are the single source of truth for the
// fixture tests AND the recorder below.
// ---------------------------------------------------------------------------

// mcpHTTPRecordSpec is one recorded Streamable exchange: which
// server shape and key it runs against, the exact request, and the
// assertions the live response must satisfy.
type mcpHTTPRecordSpec struct {
	name    string
	method  string
	shape   string // "plain" or "ledger"
	key     string // key name within the shape
	request string // exact JSON-RPC request object sent
	assert  MCPHTTPAssert
	notes   string
	// setup, when non-empty, is a list of JSON-RPC request
	// objects run on the same client/session BEFORE the recorded
	// exchange — state the recorded exchange reads (e.g. a
	// memory_add before a memory_search that must find it).
	// Setup is NOT recorded: only the final exchange becomes a
	// fixture file. Every setup request must succeed (HTTP 200,
	// no error envelope) or the record/replay fails — a setup
	// that errors is a broken spec, not a fixture. TestMCPHTTPFixtures
	// runs the same setup, so replay reproduces the recorded
	// state; the session test does not run setup (it builds state
	// by ordering fixtures instead).
	setup []string
}

// mcpHTTPToolNames is the full 24-tool surface in Tools() order:
// the 3 built-ins first, then the registered tools alphabetically.
// The tools-list fixture pins every name (audit F2: the stdio
// fixture pins only the first 3).
var mcpHTTPToolNames = []string{
	"router_status", "provider_list", "model_list",
	"best_combo_for_task", "cache_flush", "cache_stats",
	"check_quota", "cost_report", "create_combo",
	"db_health", "db_stats", "explain_route",
	"get_combo_metrics", "heartbeat", "list_combos",
	"memory_add", "memory_clear", "memory_search",
	"pick_fastest_model", "set_budget_guard",
	"set_routing_strategy", "simulate_route",
	"switch_combos", "tool_search",
}

func mcpHTTPRecordSpecs() []mcpHTTPRecordSpec {
	err32002 := -32002
	ok := func(id float64, paths map[string]any, required ...string) MCPHTTPAssert {
		return MCPHTTPAssert{
			MCPAssert:  MCPAssert{ID: id, RequiredFields: required, JSONPaths: paths},
			HTTPStatus: 200,
		}
	}
	err := func(id float64, code int, msg ...string) MCPHTTPAssert {
		paths := map[string]any{"jsonrpc": "2.0", "error.code": float64(code)}
		if len(msg) > 0 {
			paths["error.message"] = msg[0]
		}
		return MCPHTTPAssert{
			MCPAssert: MCPAssert{ID: id, ExpectError: true, ErrorCode: &code,
				RequiredFields: []string{"error.message"},
				JSONPaths:      paths},
			HTTPStatus: 200,
		}
	}
	toolsListPaths := map[string]any{"jsonrpc": "2.0"}
	for i, name := range mcpHTTPToolNames {
		toolsListPaths[fmt.Sprintf("result.tools.%d.name", i)] = name
	}
	return []mcpHTTPRecordSpec{
		{
			name: "initialize", method: "initialize", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-http-fixture-client","version":"0"}}}`,
			assert: MCPHTTPAssert{
				MCPAssert: MCPAssert{ID: float64(1),
					RequiredFields: []string{"result.protocolVersion", "result.capabilities", "result.serverInfo.name", "result.serverInfo.version"},
					JSONPaths:      map[string]any{"jsonrpc": "2.0", "result.protocolVersion": "2025-03-26", "result.serverInfo.name": "codvian-router", "result.serverInfo.version": "0.1.0"}},
				HTTPStatus:           200,
				RequireSessionHeader: true,
			},
			notes: "Handshake over Streamable HTTP: the server reports protocol 2025-03-26 and its name/version, and issues a Mcp-Session-Id header.",
		},
		{
			name: "tools-list", method: "tools/list", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			assert:  ok(2, toolsListPaths, "result.tools"),
			notes:   "tools/list over Streamable HTTP announces all 24 tools (every name pinned, not just the 3 built-ins); the list is unfiltered by caller scopes.",
		},
		{
			name: "tools-call-router-status", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`,
			assert:  ok(3, map[string]any{"jsonrpc": "2.0", "result.structuredContent.version": "0.1.0", "result.structuredContent.listen": "unknown", "result.structuredContent.providers": float64(2), "result.structuredContent.enabled_providers": float64(2), "result.structuredContent.models": float64(3), "result.structuredContent.gateway_keys": float64(5), "result.structuredContent.auto_router": false, "result.structuredContent.uptime_seconds": nil}, "result.content", "result.structuredContent"),
			notes:   "router_status over Streamable HTTP against the plain server: 2 providers (both enabled), 3 models, 5 gateway keys, no auto-router; uptime is live so presence-only.",
		},
		{
			name: "tools-call-provider-list", method: "tools/call", shape: "plain", key: "wide",
			request: `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`,
			assert:  ok(4, map[string]any{"jsonrpc": "2.0", "result.structuredContent.providers.0.id": "p1", "result.structuredContent.providers.0.name": "Coder", "result.structuredContent.providers.0.wire_format": "openai", "result.structuredContent.providers.0.enabled": true, "result.structuredContent.providers.0.models.0": "a", "result.structuredContent.providers.0.models.1": "b", "result.structuredContent.providers.1.id": "p2", "result.structuredContent.providers.1.enabled": true, "result.structuredContent.providers.1.models.0": "c"}, "result.content", "result.structuredContent.providers"),
			notes:   "provider_list with the mcp:read:* key: both providers with their registry models; no base URL or key appears.",
		},
		{
			name: "tools-call-model-list", method: "tools/call", shape: "plain", key: "wide",
			request: `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`,
			assert:  ok(5, map[string]any{"jsonrpc": "2.0", "result.structuredContent.models.0.id": "a", "result.structuredContent.models.0.provider_id": "p1", "result.structuredContent.models.1.id": "b", "result.structuredContent.models.1.provider_id": "p1", "result.structuredContent.models.2.id": "c", "result.structuredContent.models.2.provider_id": "p2"}, "result.content", "result.structuredContent.models"),
			notes:   "model_list with the mcp:read:* key: models a/b on p1, c on p2.",
		},
		{
			name: "tools-call-provider-list-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`,
			assert:  err(6, err32002, "scope denied: missing required scope(s): mcp:read:providers"),
			notes:   "provider_list with the narrow mcp:read:status key is denied -32002 (scope denial, the most common remote-transport outcome).",
		},
		{
			name: "tools-call-model-list-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`,
			assert:  err(7, err32002, "scope denied: missing required scope(s): mcp:read:models"),
			notes:   "model_list with the narrow mcp:read:status key is denied -32002.",
		},
		{
			name: "tools-call-db-health", method: "tools/call", shape: "plain", key: "ops",
			request: `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"db_health","arguments":{}}}`,
			assert:  ok(8, map[string]any{"jsonrpc": "2.0", "result.structuredContent.healthy": true}, "result.content", "result.structuredContent", "result.structuredContent.files"),
			notes:   "db_health with the mcp:manage:ops key: healthy=true (no database files exist yet, nothing is broken).",
		},
		{
			name: "tools-call-cache-flush", method: "tools/call", shape: "plain", key: "cache",
			request: `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`,
			assert:  ok(9, map[string]any{"jsonrpc": "2.0", "result.structuredContent.flushed": float64(0), "result.structuredContent.tenant": nil}, "result.content", "result.structuredContent"),
			notes:   "cache_flush with the cache key: flushed=0 (the test cache holds nothing); tenant is the gateway-key id (random per run, presence-only) — the per-request identity, not anonymous.",
		},
		{
			name: "tools-call-pick-fastest-model-unavailable", method: "tools/call", shape: "plain", key: "wide",
			request: `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{}}}`,
			assert:  err(10, -32602, "unavailable: model latency backend is not available over this transport (ledger unavailable) — pick_fastest_model ranks only on measured ledger latency"),
			notes:   "pick_fastest_model on the plain server (no ledger wired) answers -32602 saying the backend is not available — never a fake latency ranking.",
		},
		{
			name: "tools-call-pick-fastest-model", method: "tools/call", shape: "ledger", key: "wide",
			request: `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{}}}`,
			assert:  ok(11, map[string]any{"jsonrpc": "2.0", "result.structuredContent.model": "c", "result.structuredContent.provider": "p2", "result.structuredContent.mean_latency_ms": float64(90)}, "result.content", "result.structuredContent"),
			notes:   "pick_fastest_model ranks on measured ledger latency: c at 90 ms beats a at 120 ms.",
		},
		{
			name: "tools-call-explain-route", method: "tools/call", shape: "ledger", key: "wide",
			request: `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"a"}}}`,
			assert:  ok(12, map[string]any{"jsonrpc": "2.0", "result.structuredContent.route.strategy": "priority", "result.structuredContent.route.attempts.0.provider": "p1", "result.structuredContent.route.attempts.0.price_known": true}, "result.content", "result.structuredContent"),
			notes:   "explain_route dry-runs model a from the live feed: strategy priority, one attempt on p1 with a known price.",
		},
		{
			name: "tools-call-simulate-route", method: "tools/call", shape: "ledger", key: "wide",
			request: `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"a","prompt_tokens":1000}}}`,
			assert:  ok(13, map[string]any{"jsonrpc": "2.0", "result.structuredContent.simulation.cost_known": true, "result.structuredContent.simulation.estimated_prompt_cost_usd": float64(0.01)}, "result.content", "result.structuredContent"),
			notes:   "simulate_route for model a, 1000 prompt tokens: cost known, $0.01 (1000 tokens at $10/1M).",
		},
		{
			name: "tools-call-get-combo-metrics", method: "tools/call", shape: "ledger", key: "wide",
			request: `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":"fast"}}}`,
			assert:  ok(14, map[string]any{"jsonrpc": "2.0", "result.structuredContent.metrics.requests": float64(3), "result.structuredContent.metrics.latency_known": true, "result.structuredContent.metrics.mean_latency_ms": float64(110)}, "result.content", "result.structuredContent"),
			notes:   "get_combo_metrics aggregates the ledger over combo fast: 3 requests, mean latency 110 ms.",
		},
		{
			name: "tools-call-cost-report", method: "tools/call", shape: "ledger", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"cost_report","arguments":{}}}`,
			// cost_usd is the float64 sum 0.025 + 0.0005, which the
			// ledger reports as 0.025500000000000002 — an exact pin
			// of 0.0255 would fail on float arithmetic, so the
			// total is presence-only and the per-model costs
			// (which round-trip exactly) pin the $0.0255 total.
			assert: ok(15, map[string]any{"jsonrpc": "2.0", "result.structuredContent.cost_report.requests": float64(2), "result.structuredContent.cost_report.cost_usd": nil, "result.structuredContent.cost_report.cost_known": true, "result.structuredContent.cost_report.key_id": nil, "result.structuredContent.cost_report.by_model.0.model": "a", "result.structuredContent.cost_report.by_model.0.cost_usd": float64(0.025), "result.structuredContent.cost_report.by_model.1.model": "c", "result.structuredContent.cost_report.by_model.1.cost_usd": float64(0.0005)}, "result.content", "result.structuredContent"),
			notes:  "cost_report with no key_id covers the caller's own usage only: 2 requests, $0.0255 — never the other key's rows. key_id is the caller's random id, presence-only.",
		},
		{
			name: "tools-call-check-quota", method: "tools/call", shape: "ledger", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"check_quota","arguments":{}}}`,
			assert:  ok(16, map[string]any{"jsonrpc": "2.0", "result.structuredContent.quota.has_budget": false, "result.structuredContent.quota.key_id": nil}, "result.content", "result.structuredContent"),
			notes:   "check_quota with no key_id checks the caller's own key: no budget set, so has_budget=false (unlimited, no invented cap).",
		},
		{
			name: "tools-call-check-quota-cross-key", method: "tools/call", shape: "ledger", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":"other-key"}}}`,
			assert:  err(17, -32602),
			notes:   "A cross-key check_quota is rejected -32602, not served: a gateway key may only read its own quota. The message names the caller's key id (random per run), so only the code is pinned.",
		},
		{
			name: "tools-call-set-budget-guard", method: "tools/call", shape: "ledger", key: "cost-manager",
			request: `{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"limit_usd":10}}}`,
			assert:  ok(18, map[string]any{"jsonrpc": "2.0", "result.structuredContent.guard_set": true}, "result.content", "result.structuredContent"),
			notes:   "set_budget_guard with no key_id guards the caller's own key: writes a real $10 ledger budget.",
		},
		{
			name: "tools-call-set-budget-guard-cross-key", method: "tools/call", shape: "ledger", key: "cost-manager",
			request: `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"other-key","limit_usd":10}}}`,
			assert:  err(19, -32602),
			notes:   "A cross-key set_budget_guard is rejected -32602 and stores nothing for the other key. The message names the caller's key id (random per run), so only the code is pinned.",
		},
		{
			name: "tools-call-set-routing-strategy", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":"cheapest"}}}`,
			assert:  ok(20, map[string]any{"jsonrpc": "2.0", "result.structuredContent.strategy.name": "cheapest"}, "result.content", "result.structuredContent"),
			notes:   "set_routing_strategy persists the global strategy as cheapest (read-back shows it) and applies it to the engine.",
		},
		{
			name: "tools-call-create-combo", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"new","members":["p1"]}}}`,
			assert:  ok(21, map[string]any{"jsonrpc": "2.0", "result.structuredContent.created": true, "result.structuredContent.combo.name": "new", "result.structuredContent.combo.strategy": "priority", "result.structuredContent.combo.members.0": "p1"}, "result.content", "result.structuredContent"),
			notes:   "create_combo succeeds through the same ComboStore.Create path POST /v1/combos runs: combo new, member p1, strategy priority.",
		},
		{
			name: "tools-call-list-combos", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"list_combos","arguments":{}}}`,
			assert:  ok(22, map[string]any{"jsonrpc": "2.0", "result.structuredContent.combos.0.name": "fast"}, "result.content", "result.structuredContent"),
			notes:   "list_combos on a fresh ledger server lists the configured routing group fast. (In a session that created combo new first, the list is [fast, new] — combos.0 stays fast, so this fixture holds in both flows.)",
		},
		// -- The 9 tools never called as fixtures before M9.10
		// (coverage re-audit 96ea9b6d): each converts the matching
		// assertions from TestMCPHTTPM910NeverCalledTools
		// (internal/server/mcp_http_test.go) into a recorded
		// Streamable exchange. Success fixtures first (ids
		// 23-31), then the error/denial fixtures the same test
		// asserts (ids 32-44).
		{
			name: "tools-call-best-combo-for-task", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat"}}}`,
			assert:  ok(23, map[string]any{"jsonrpc": "2.0", "result.structuredContent.combo.name": "fast", "result.structuredContent.preferred_strategy": "fastest", "result.structuredContent.metrics_known": true, "result.structuredContent.candidates_seen": float64(1)}, "result.content", "result.structuredContent"),
			notes:   "best_combo_for_task ranks on the seeded measured history: \"fast chat\" prefers fastest, combo fast is the only candidate, metrics known (3 seeded requests over its members).",
		},
		{
			name: "tools-call-switch-combos", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"fast"}}}`,
			assert:  ok(24, map[string]any{"jsonrpc": "2.0", "result.structuredContent.active_combo": "fast"}, "result.content", "result.structuredContent"),
			notes:   "switch_combos activates combo fast on this session (per-session active combo — another session does not inherit it).",
		},
		{
			name: "tools-call-cache-stats", method: "tools/call", shape: "plain", key: "cache",
			request: `{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert:  ok(25, map[string]any{"jsonrpc": "2.0", "result.structuredContent.exact.present": false, "result.structuredContent.semantic.present": false, "result.structuredContent.persistent.present": false}, "result.content", "result.structuredContent"),
			notes:   "cache_stats with the cache key: no cache is wired on this server, so every layer honestly reports present=false, never zero-filled.",
		},
		{
			name: "tools-call-db-stats", method: "tools/call", shape: "ledger", key: "ops",
			request: `{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"db_stats","arguments":{}}}`,
			assert:  ok(26, map[string]any{"jsonrpc": "2.0", "result.structuredContent.files.0.name": "ledger.db", "result.structuredContent.files.0.exists": true, "result.structuredContent.files.0.size_bytes": nil, "result.structuredContent.files.0.rows.usage_events": float64(3), "result.structuredContent.files.1.name": "feedback.db", "result.structuredContent.files.1.exists": false}, "result.content", "result.structuredContent", "result.structuredContent.files"),
			notes:   "db_stats with the ops key: the live ledger.db in the data dir exists with the 3 seeded usage_events rows (2 under the cost-reader key, 1 under other-key); feedback.db does not exist yet. size_bytes is live so presence-only.",
		},
		{
			name: "tools-call-heartbeat", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert:  ok(27, map[string]any{"jsonrpc": "2.0", "result.structuredContent.protocol_version": "2025-03-26", "result.structuredContent.tool_count": float64(24), "result.structuredContent.uptime_seconds": nil, "result.structuredContent.audit_log": "enabled"}, "result.content", "result.structuredContent"),
			notes:   "heartbeat with the mcp:read:status key: live snapshot — protocol 2025-03-26, all 24 tools announced, audit log enabled (the HTTP transport opens <data-dir>/mcp/audit.jsonl); uptime is live so presence-only.",
		},
		{
			name: "tools-call-tool-search", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"cache"}}}`,
			assert:  ok(28, map[string]any{"jsonrpc": "2.0", "result.structuredContent.query": "cache", "result.structuredContent.count": float64(3), "result.structuredContent.tools.0.name": "cache_flush", "result.structuredContent.tools.1.name": "cache_stats", "result.structuredContent.tools.2.name": "db_health"}, "result.content", "result.structuredContent"),
			notes:   "\"cache\" matches exactly the three tools whose name or description mentions the cache (cache_flush, cache_stats, db_health — in Tools() order).",
		},
		{
			name: "tools-call-memory-add", method: "tools/call", shape: "ledger", key: "memory",
			request: `{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"prefers dark mode"}}}`,
			assert:  ok(29, map[string]any{"jsonrpc": "2.0", "result.structuredContent.id": nil, "result.structuredContent.text": "prefers dark mode", "result.structuredContent.created_at": nil}, "result.content", "result.structuredContent"),
			notes:   "memory_add stores the note under the caller's own namespace in the file-backed store (<data-dir>/mcp/memory.jsonl) and returns it with its id; id and created_at are per-run values so presence-only.",
		},
		{
			name: "tools-call-memory-search", method: "tools/call", shape: "ledger", key: "memory",
			request: `{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"DARK"}}}`,
			assert:  ok(30, map[string]any{"jsonrpc": "2.0", "result.structuredContent.count": float64(1), "result.structuredContent.entries.0.text": "prefers dark mode", "result.structuredContent.entries.0.matches": float64(1), "result.structuredContent.entries.0.id": nil, "result.structuredContent.entries.0.created_at": nil}, "result.content", "result.structuredContent"),
			notes:   "memory_search finds the caller's own note by case-insensitive substring (query DARK matches \"prefers dark mode\" once). Setup adds the note first on the same session — a fresh server's store is empty, so without the setup this would honestly return count 0.",
			setup:   []string{`{"jsonrpc":"2.0","id":901,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"prefers dark mode"}}}`},
		},
		{
			name: "tools-call-memory-clear", method: "tools/call", shape: "ledger", key: "memory-destr",
			request: `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"memory_clear","arguments":{}}}`,
			assert:  ok(31, map[string]any{"jsonrpc": "2.0", "result.structuredContent.cleared": float64(1), "result.structuredContent.caller": nil, "result.structuredContent.scope": "caller-only"}, "result.content", "result.structuredContent"),
			notes:   "memory_clear with the destructive memory key clears only the caller's own note (cleared=1) and reports the caller's own key id (random per run, presence-only) with scope caller-only. Setup adds the note first on the same session.",
			setup:   []string{`{"jsonrpc":"2.0","id":902,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"destructive key note"}}}`},
		},
		{
			name: "tools-call-best-combo-for-task-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat"}}}`,
			assert:  err(32, err32002, "scope denied: missing required scope(s): mcp:read:routing"),
			notes:   "best_combo_for_task with the narrow mcp:read:status key is denied -32002 (mcp:read:routing required).",
		},
		{
			name: "tools-call-best-combo-for-task-impossible-cost", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat","max_cost_usd":0.0000001}}}`,
			assert:  err(33, -32602, "mcp: invalid input: no combo satisfies the given constraints"),
			notes:   "A cost cap the measured combo cannot satisfy excludes it; with one combo left out, no candidate remains — honest -32602, not a guess.",
		},
		{
			name: "tools-call-switch-combos-denied", method: "tools/call", shape: "ledger", key: "wide",
			request: `{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"fast"}}}`,
			assert:  err(34, err32002, "scope denied: missing required scope(s): mcp:manage:routing"),
			notes:   "switch_combos with the mcp:read:* key is denied -32002 (mcp:manage:routing required; read:* covers read:routing only). M910 used the routerRead key; here the wide key stands in for it — the -32002 code and message are identical to M910's.",
		},
		{
			name: "tools-call-switch-combos-unknown", method: "tools/call", shape: "ledger", key: "router",
			request: `{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"nope"}}}`,
			assert:  err(35, -32602, `mcp: invalid input: unknown combo "nope"`),
			notes:   "switch_combos for an unknown combo name is refused -32602 naming the unknown combo.",
		},
		{
			name: "tools-call-cache-stats-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":36,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert:  err(36, err32002, "scope denied: missing required scope(s): mcp:manage:cache"),
			notes:   "cache_stats with the narrow mcp:read:status key is denied -32002 (mcp:manage:cache required).",
		},
		{
			name: "tools-call-db-stats-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":37,"method":"tools/call","params":{"name":"db_stats","arguments":{}}}`,
			assert:  err(37, err32002, "scope denied: missing required scope(s): mcp:manage:ops"),
			notes:   "db_stats with the narrow mcp:read:status key is denied -32002 (mcp:manage:ops required).",
		},
		{
			name: "tools-call-heartbeat-denied", method: "tools/call", shape: "ledger", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":38,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert:  err(38, err32002, "scope denied: missing required scope(s): mcp:read:status"),
			notes:   "heartbeat with the cost-reader key (mcp:read:cost only) is denied -32002 (mcp:read:status required). No empty-scope key exists on the fixture servers, so a wrong-scope key stands in for it.",
		},
		{
			name: "tools-call-tool-search-denied", method: "tools/call", shape: "ledger", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":39,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"cache"}}}`,
			assert:  err(39, err32002, "scope denied: missing required scope(s): mcp:read:status"),
			notes:   "tool_search with the cost-reader key is denied -32002 (mcp:read:status required). No empty-scope key exists on the fixture servers, so the cost-reader key stands in for it — the -32002 code and message are identical to M910's.",
		},
		{
			name: "tools-call-tool-search-non-string-query", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":42}}}`,
			assert:  err(40, -32602, "tool_search: query must be a string"),
			notes:   "tool_search with a non-string query is refused -32602 naming the query type.",
		},
		{
			name: "tools-call-memory-add-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"x"}}}`,
			assert:  err(41, err32002, "scope denied: missing required scope(s): mcp:manage:memory"),
			notes:   "memory_add with the narrow mcp:read:status key is denied -32002 (mcp:manage:memory required).",
		},
		{
			name: "tools-call-memory-add-empty", method: "tools/call", shape: "ledger", key: "memory",
			request: `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"  "}}}`,
			assert:  err(42, -32602, `memory_add requires a non-empty string argument "text"`),
			notes:   "memory_add with blank text is refused -32602 naming the non-empty rule.",
		},
		{
			name: "tools-call-memory-search-denied", method: "tools/call", shape: "plain", key: "read",
			request: `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"dark mode"}}}`,
			assert:  err(43, err32002, "scope denied: missing required scope(s): mcp:manage:memory"),
			notes:   "memory_search with the narrow mcp:read:status key is denied -32002 (mcp:manage:memory required).",
		},
		{
			name: "tools-call-memory-clear-denied", method: "tools/call", shape: "ledger", key: "memory",
			request: `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"memory_clear","arguments":{}}}`,
			assert:  err(44, err32002, "scope denied: missing required scope(s): mcp:destructive"),
			notes:   "memory_clear with the memory key (mcp:manage:memory but no mcp:destructive) is denied -32002 — clear needs both scopes.",
		},
	}
}

// mcpHTTPSpecByName indexes the specs by fixture name.
func mcpHTTPSpecByName() map[string]mcpHTTPRecordSpec {
	out := map[string]mcpHTTPRecordSpec{}
	for _, s := range mcpHTTPRecordSpecs() {
		out[s.name] = s
	}
	return out
}

// mcpHTTPFixtureServers builds a fresh server of the spec's shape
// and returns its URL plus the raw key for the spec's key name.
func mcpHTTPFixtureServers(t *testing.T, spec mcpHTTPRecordSpec) (string, string) {
	t.Helper()
	if spec.shape == "plain" {
		ts, keys := newMCPHTTPPlainServer(t)
		return ts.URL, mcpHTTPKeyFor("plain", keys, mcpHTTPLedgerKeys{}, spec.key)
	}
	ts, keys := newMCPHTTPLedgerServer(t)
	return ts.URL, mcpHTTPKeyFor("ledger", mcpHTTPPlainKeys{}, keys, spec.key)
}

// ---------------------------------------------------------------------------
// Fixture tests
// ---------------------------------------------------------------------------

// mcpHTTPPostSetup runs a spec's setup requests (if any) on an
// already-initialized client, failing the test unless each one
// answers HTTP 200 with a success envelope. Setup builds the state
// the recorded exchange reads; it is never itself a fixture.
func mcpHTTPPostSetup(t *testing.T, client *MCPHTTPClient, spec mcpHTTPRecordSpec) {
	t.Helper()
	for _, req := range spec.setup {
		status, _, body := client.Post(t, json.RawMessage(req))
		if status != 200 {
			t.Fatalf("spec %s: setup request %s: HTTP status %d, want 200: %s", spec.name, req, status, body)
		}
		var env struct {
			Error *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("spec %s: setup request %s: response is not JSON: %v (%s)", spec.name, req, err, body)
		}
		if env.Error != nil {
			t.Fatalf("spec %s: setup request %s failed: %d %s", spec.name, req, env.Error.Code, env.Error.Message)
		}
	}
}

// TestMCPHTTPFixtures runs every recorded Streamable fixture as a
// single exchange against a fresh real server: initialize opens a
// session, every other fixture opens its own session first with
// the raw (unrecorded) Initialize step and then drives the one
// recorded exchange.
func TestMCPHTTPFixtures(t *testing.T) {
	for _, spec := range mcpHTTPRecordSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			f := LoadMCPHTTPFixture(t, spec.name)
			baseURL, rawKey := mcpHTTPFixtureServers(t, spec)
			client := NewMCPHTTPClient(baseURL, rawKey)
			if spec.method == "initialize" {
				RunMCPHTTP(t, client, f)
				return
			}
			client.Initialize(t)
			mcpHTTPPostSetup(t, client, spec)
			RunMCPHTTP(t, client, f)
			client.Terminate(t)
		})
	}
}

// mcpHTTPSessionGroup is one client session in the fixture-session
// test: a server shape, a key, and the fixtures driven over that
// session in order (initialize first in every group).
type mcpHTTPSessionGroup struct {
	shape    string
	key      string
	fixtures []string
}

// TestMCPHTTPFixtureSession runs all fixtures in session order
// through real Streamable sessions — initialize → tools/list →
// tools/call — grouped by server shape and key, terminating each
// session at the end. This is the end-to-end proof the harness
// drives real sessions, not just isolated exchanges.
func TestMCPHTTPFixtureSession(t *testing.T) {
	specs := mcpHTTPSpecByName()
	groups := []mcpHTTPSessionGroup{
		{shape: "plain", key: "read", fixtures: []string{"initialize", "tools-list", "tools-call-router-status", "tools-call-provider-list-denied", "tools-call-model-list-denied"}},
		{shape: "plain", key: "wide", fixtures: []string{"initialize", "tools-call-provider-list", "tools-call-model-list", "tools-call-pick-fastest-model-unavailable"}},
		{shape: "plain", key: "ops", fixtures: []string{"initialize", "tools-call-db-health"}},
		{shape: "plain", key: "cache", fixtures: []string{"initialize", "tools-call-cache-flush"}},
		{shape: "ledger", key: "wide", fixtures: []string{"initialize", "tools-call-pick-fastest-model", "tools-call-explain-route", "tools-call-simulate-route", "tools-call-get-combo-metrics"}},
		{shape: "ledger", key: "cost-reader", fixtures: []string{"initialize", "tools-call-cost-report", "tools-call-check-quota", "tools-call-check-quota-cross-key"}},
		{shape: "ledger", key: "cost-manager", fixtures: []string{"initialize", "tools-call-set-budget-guard", "tools-call-set-budget-guard-cross-key"}},
		{shape: "ledger", key: "router", fixtures: []string{"initialize", "tools-call-set-routing-strategy", "tools-call-create-combo", "tools-call-list-combos"}},
		// M9.10 never-called tools, grouped by key. The memory
		// group builds state by fixture order, not setup: the
		// add runs first in the session, so the search finds it
		// (count 1) and the clear-denied call then proves the
		// non-destructive key cannot clear. The memory-destr
		// group likewise adds first, then clears its own note
		// (cleared=1). The read group proves the heartbeat /
		// tool-search successes plus the denials that key hits.
		{shape: "plain", key: "read", fixtures: []string{"initialize", "tools-call-heartbeat", "tools-call-tool-search", "tools-call-tool-search-non-string-query", "tools-call-best-combo-for-task-denied", "tools-call-cache-stats-denied", "tools-call-db-stats-denied", "tools-call-memory-add-denied", "tools-call-memory-search-denied"}},
		{shape: "plain", key: "cache", fixtures: []string{"initialize", "tools-call-cache-stats"}},
		{shape: "ledger", key: "memory", fixtures: []string{"initialize", "tools-call-memory-add", "tools-call-memory-search", "tools-call-memory-add-empty", "tools-call-memory-clear-denied"}},
		{shape: "ledger", key: "memory-destr", fixtures: []string{"initialize", "tools-call-memory-add", "tools-call-memory-clear"}},
		{shape: "ledger", key: "router", fixtures: []string{"initialize", "tools-call-best-combo-for-task", "tools-call-best-combo-for-task-impossible-cost", "tools-call-switch-combos", "tools-call-switch-combos-unknown"}},
		{shape: "ledger", key: "ops", fixtures: []string{"initialize", "tools-call-db-stats"}},
		{shape: "ledger", key: "cost-reader", fixtures: []string{"initialize", "tools-call-heartbeat-denied", "tools-call-tool-search-denied"}},
		{shape: "ledger", key: "wide", fixtures: []string{"initialize", "tools-call-switch-combos-denied"}},
	}
	for _, g := range groups {
		t.Run(g.shape+"/"+g.key, func(t *testing.T) {
			var baseURL, rawKey string
			if g.shape == "plain" {
				ts, keys := newMCPHTTPPlainServer(t)
				baseURL, rawKey = ts.URL, mcpHTTPKeyFor("plain", keys, mcpHTTPLedgerKeys{}, g.key)
			} else {
				ts, keys := newMCPHTTPLedgerServer(t)
				baseURL, rawKey = ts.URL, mcpHTTPKeyFor("ledger", mcpHTTPPlainKeys{}, keys, g.key)
			}
			client := NewMCPHTTPClient(baseURL, rawKey)
			fixtures := make([]MCPHTTPFixture, 0, len(g.fixtures))
			for _, name := range g.fixtures {
				if _, ok := specs[name]; !ok {
					t.Fatalf("session group %s/%s references unknown fixture %q", g.shape, g.key, name)
				}
				fixtures = append(fixtures, LoadMCPHTTPFixture(t, name))
			}
			RunMCPHTTPSession(t, client, fixtures...)
		})
	}
}

// TestMCPHTTPFixtureRecordedResponsesMatch asserts the recorded
// response in each fixture file still satisfies its own assertions
// — i.e. the files on disk are self-consistent. The Mcp-Session-Id
// header cannot be recorded (session ids are random per run), so
// the initialize fixture is checked with a placeholder header.
func TestMCPHTTPFixtureRecordedResponsesMatch(t *testing.T) {
	for _, spec := range mcpHTTPRecordSpecs() {
		f := LoadMCPHTTPFixture(t, spec.name)
		header := ""
		if f.Assert.RequireSessionHeader {
			header = "recorded-placeholder"
		}
		if err := CheckMCPHTTPResponse(f.RecordedHTTPStatus, header, f.RecordedResponse, f); err != nil {
			t.Errorf("fixture %s: recorded response fails its own assertions: %v", spec.name, err)
		}
	}
}

// TestMCPHTTPFixtureSpecsCoverTwentyFourTools pins the M9.10
// gate's full set: the recorded fixtures must cover all 24 tools
// in the tools-list (the 15 the Streamable tests already exercised
// plus the 9 never-called ones), each at least once.
func TestMCPHTTPFixtureSpecsCoverTwentyFourTools(t *testing.T) {
	want := mcpHTTPToolNames
	covered := map[string]bool{}
	for _, spec := range mcpHTTPRecordSpecs() {
		var req struct {
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if spec.method != "tools/call" {
			continue
		}
		if err := json.Unmarshal([]byte(spec.request), &req); err != nil {
			t.Fatalf("spec %s: request is not JSON: %v", spec.name, err)
		}
		covered[req.Params.Name] = true
	}
	for _, tool := range want {
		if !covered[tool] {
			t.Errorf("no Streamable fixture covers tool %q", tool)
		}
	}
	if len(covered) != len(want) {
		t.Errorf("covered %d distinct tools, want exactly the %d Streamable-exercised tools", len(covered), len(want))
	}
}

// ---------------------------------------------------------------------------
// Toggle proofs for the HTTP assertion path
// ---------------------------------------------------------------------------

func TestCheckMCPHTTPResponseWrongStatusFails(t *testing.T) {
	f := LoadMCPHTTPFixture(t, "tools-list")
	if err := CheckMCPHTTPResponse(404, "", f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPHTTPResponse accepted HTTP 404 against an expectation of 200")
	} else if !strings.Contains(err.Error(), "http status") {
		t.Fatalf("wrong-status error should name http status, got: %v", err)
	}
}

func TestCheckMCPHTTPResponseMissingSessionHeaderFails(t *testing.T) {
	f := LoadMCPHTTPFixture(t, "initialize")
	if err := CheckMCPHTTPResponse(200, "", f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPHTTPResponse accepted an initialize response with no Mcp-Session-Id header")
	} else if !strings.Contains(err.Error(), "Mcp-Session-Id") {
		t.Fatalf("missing-header error should name Mcp-Session-Id, got: %v", err)
	}
}

func TestCheckMCPHTTPResponseWrongJSONPathFails(t *testing.T) {
	f := LoadMCPHTTPFixture(t, "tools-list")
	f.Assert.JSONPaths["result.tools.0.name"] = "provider_list" // wrong on purpose: the first tool is router_status
	if err := CheckMCPHTTPResponse(200, "", f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPHTTPResponse accepted a tools/list response whose first tool is router_status against an expectation of provider_list")
	} else if !strings.Contains(err.Error(), "result.tools.0.name") {
		t.Fatalf("wrong-path error should name result.tools.0.name, got: %v", err)
	}
}

func TestMCPHTTPFixturePathRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"..", "../x", "a/b", "a\\b", ""} {
		if _, err := MCPHTTPFixturePath(bad); err == nil {
			t.Fatalf("MCPHTTPFixturePath accepted name=%q", bad)
		}
	}
}

// writeMCPHTTPFixtureFile writes raw JSON to a temp file and returns
// its path (for loader-rejection tests).
func writeMCPHTTPFixtureFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	return path
}

func mcpHTTPFixtureJSON(overrides string) string {
	return `{"schema_version":1,"slice":"m9.10","name":"bad","fixture":true,"transport":"streamable","key":"wide",` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"recorded_http_status":200,` +
		`"assert":{"id":1,"http_status":200}` + overrides + `}`
}

func TestLoadMCPHTTPFixtureRejectsNonFixture(t *testing.T) {
	raw := strings.Replace(mcpHTTPFixtureJSON(""), `"fixture":true`, `"fixture":false`, 1)
	if _, err := loadMCPHTTPFixtureFile(writeMCPHTTPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-HTTP fixture with \"fixture\": false")
	}
}

func TestLoadMCPHTTPFixtureRejectsBadTransport(t *testing.T) {
	raw := strings.Replace(mcpHTTPFixtureJSON(""), `"transport":"streamable"`, `"transport":"sse"`, 1)
	if _, err := loadMCPHTTPFixtureFile(writeMCPHTTPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-HTTP fixture with transport sse")
	}
}

func TestLoadMCPHTTPFixtureRejectsStatusMismatch(t *testing.T) {
	raw := strings.Replace(mcpHTTPFixtureJSON(""), `"recorded_http_status":200`, `"recorded_http_status":201`, 1)
	if _, err := loadMCPHTTPFixtureFile(writeMCPHTTPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-HTTP fixture whose recorded_http_status disagrees with assert.http_status")
	}
}

func TestLoadMCPHTTPFixtureRejectsUnknownField(t *testing.T) {
	raw := mcpHTTPFixtureJSON(`,"bogus_field":1`)
	if _, err := loadMCPHTTPFixtureFile(writeMCPHTTPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-HTTP fixture with an unknown field")
	}
}

// ---------------------------------------------------------------------------
// Recorder
// ---------------------------------------------------------------------------

// TestRecordMCPHTTPFixtures re-records every fixture by running the
// real server over httptest and capturing the response verbatim. It
// is skipped unless CODVIAN_RECORD_MCP_HTTP_FIXTURES=1, so the
// normal suite never rewrites fixtures. The recorded_response
// written to disk is the captured bytes, not a hand-written value;
// the test refuses to write a file whose assertions do not match
// the captured response.
//
// To re-record: CODVIAN_RECORD_MCP_HTTP_FIXTURES=1 go test
// ./internal/apitest/ -run TestRecordMCPHTTPFixtures -count=1
func TestRecordMCPHTTPFixtures(t *testing.T) {
	if os.Getenv("CODVIAN_RECORD_MCP_HTTP_FIXTURES") != "1" {
		t.Skip("recorder only: set CODVIAN_RECORD_MCP_HTTP_FIXTURES=1 to re-record fixtures")
	}
	for _, spec := range mcpHTTPRecordSpecs() {
		baseURL, rawKey := mcpHTTPFixtureServers(t, spec)
		client := NewMCPHTTPClient(baseURL, rawKey)
		var status int
		var sessionHeader string
		var body []byte
		if spec.method == "initialize" {
			// Capture the initialize exchange itself (the
			// session header is asserted, never recorded: ids
			// are random per run).
			status, sessionHeader, body = client.Post(t, json.RawMessage(spec.request))
			if status != 200 || sessionHeader == "" {
				t.Fatalf("record %s: initialize: status %d session %q: %s", spec.name, status, sessionHeader, body)
			}
		} else {
			client.Initialize(t)
			mcpHTTPPostSetup(t, client, spec)
			status, sessionHeader, body = client.Post(t, json.RawMessage(spec.request))
			_ = sessionHeader
		}
		// Prove the assertions match the captured response BEFORE
		// writing anything: a hand-written assert that disagrees
		// with reality fails here instead of landing in the file.
		f := MCPHTTPFixture{
			SchemaVersion: 1, Slice: "m9.10", Name: spec.name, IsFixture: true,
			Transport: "streamable",
			RecordedFrom: "recorded 2026-11-19 by running the real internal/server Server (exported API only: server.New + config.IssueKey + With* options, standard p1/p2 + a/b/c feed" +
				func() string {
					if spec.shape == "ledger" {
						return "; ledger-backed: seeded ledger, prices a $10/$30 and c $5/$15 per 1M, routing group fast"
					}
					return "; no ledger wired"
				}() + ") over httptest (loopback) via POST /api/mcp/stream with a test gateway key; response captured verbatim. Recorded data only, no live provider call.",
			Key:                spec.key,
			Method:             spec.method,
			Request:            json.RawMessage(spec.request),
			RecordedResponse:   json.RawMessage(body),
			RecordedHTTPStatus: status,
			Assert:             spec.assert,
			Notes:              spec.notes,
		}
		if err := CheckMCPHTTPResponse(status, sessionHeader, body, f); err != nil {
			t.Fatalf("record %s: assertions do not match the captured response: %v (response: %s)", spec.name, err, body)
		}
		if err := validateMCPHTTPFixture(f); err != nil {
			t.Fatalf("record %s: fixture fails validation: %v", spec.name, err)
		}
		raw, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatalf("record %s: marshal: %v", spec.name, err)
		}
		path, err := MCPHTTPFixturePath(spec.name)
		if err != nil {
			t.Fatalf("record %s: path: %v", spec.name, err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("record %s: write %s: %v", spec.name, path, err)
		}
		fmt.Printf("recorded tests/fixtures/mcp-http/%s.json\n", spec.name)
	}
}
