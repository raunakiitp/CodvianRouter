package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// ---------------------------------------------------------------------------
// Test servers. Built only from exported API. They mirror the shapes
// in internal/server/mcp_http_test.go (newM910MCPHTTPTestServer),
// rebuilt here because that builder lives in a _test.go file and is
// not importable — and rebuilt SELF-CONTAINED (own stub engine,
// own providers/registry, own key issuance) so this file never
// depends on the sibling Streamable fixture files.
//
// One server shape serves every SSE fixture: the full M9.10 key
// set (14 keys, one per scope class), a live ledger in the config
// data dir, one routing combo "fast" over p1+p2, and a memory store
// seeded with one fixed entry for the destructive memory key.
//
// FIXED KEY IDS: every gateway key is issued with a fixed id
// (gk_0000000000000001 … gk_000000000000000e) and a fixed raw
// secret ("crk_" + the same 16 hex chars + 16 zeros), via the
// exported config.GatewayKey struct + config.HashKey —
// AddGatewayKey validates only uniqueness, never the id shape, so
// a fixed id is a legal key. Fixed ids make every key id that
// appears in a recorded response deterministic across re-records
// (check_quota / cost_report / cache_flush tenant / memory_clear
// caller / cross-key -32602 messages), which is what makes the
// re-record determinism check meaningful. The memory seed is fixed
// the same way (fixed entry id + fixed created_at), so
// memory_search and memory_clear re-record byte-identically too.
// The one response that can never be deterministic is memory_add:
// its entry id is crypto/rand and its created_at is wall-clock,
// by production design — see the determinism note on
// TestRecordMCPSSEFixtures.
// ---------------------------------------------------------------------------

// mcpSSEStubEngine is the minimal Engine behind the SSE fixture
// servers: Chat/ChatStream answer a canned reply, and it implements
// the strategyReporter interface the server type-asserts for
// set_routing_strategy, so a strategy change applies at runtime as
// well as persisting.
type mcpSSEStubEngine struct {
	mu     sync.Mutex
	global string
	chains map[string]string
}

func (e *mcpSSEStubEngine) Chat(_ context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "hi", Model: "stub"}, nil
}

func (e *mcpSSEStubEngine) ChatStream(_ context.Context, _ provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta != nil {
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "hi", Model: "stub"}, nil
}

func (e *mcpSSEStubEngine) LastRouting() (router.RoutingInfo, bool) {
	return router.RoutingInfo{}, false
}

func (e *mcpSSEStubEngine) StrategyState() (string, map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]string, len(e.chains))
	for k, v := range e.chains {
		out[k] = v
	}
	return e.global, out
}

func (e *mcpSSEStubEngine) SetGlobalStrategy(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.global = s
}

func (e *mcpSSEStubEngine) SetChainStrategy(model, s string) {
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

// newMCPSSEStubEngine returns the stub engine for SSE fixture
// servers.
func newMCPSSEStubEngine() *mcpSSEStubEngine {
	return &mcpSSEStubEngine{global: "priority"}
}

// mcpSSEProviders is the DashboardProviderLister behind the SSE
// fixture servers: a fixed provider list, in order.
type mcpSSEProviders struct {
	providers []provider.DashboardProvider
}

func (f mcpSSEProviders) DashboardProviders() []provider.DashboardProvider {
	return f.providers
}

// mcpSSEStandardProviders returns the standard fixture provider
// feed: p1 Coder (openai, enabled) and p2 Chatter (anthropic,
// enabled).
func mcpSSEStandardProviders() server.DashboardProviderLister {
	return mcpSSEProviders{providers: []provider.DashboardProvider{
		{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
		{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
	}}
}

// mcpSSERegistry is the ModelRegistry behind the SSE fixture
// servers: a fixed model list, in order.
type mcpSSERegistry struct {
	models []server.Model
}

func (f mcpSSERegistry) Models(_ context.Context) ([]server.Model, error) {
	return f.models, nil
}

// mcpSSEStandardRegistry returns the standard fixture model feed:
// a and b on p1 (Coder), c on p2 (Chatter).
func mcpSSEStandardRegistry() server.ModelRegistry {
	return mcpSSERegistry{models: []server.Model{
		{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
		{ID: "b", ProviderID: "p1", OwnedBy: "Coder"},
		{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
	}}
}

// mcpSSEFloatPtr returns a pointer to v (price-book helper).
func mcpSSEFloatPtr(v float64) *float64 { return &v }

// mcpSSEKeys is the key set of one SSE fixture server: one key per
// scope class, 14 keys total (router_status asserts gateway_keys
// == 14). The ID fields are the gateway-key ids, needed for the
// tenant-isolation assertions.
type mcpSSEKeys struct {
	admin, read, empty, wide, routerRead, routerManage, routerDual, costRead, costManager, ops, memory, memory2, memoryDestr, cache string
	routerReadID, costReadID, costManagerID, memoryDestrID, cacheID                                                                 string
	count                                                                                                                           int
}

// issueMCPSSEFixedKey issues a gateway key with a FIXED id and a
// FIXED raw secret into cfg: id "gk_<suffix>", raw "crk_<suffix>"
// + 16 zeros (32 chars after the prefix, all alphanumeric).
// AddGatewayKey accepts a caller-constructed GatewayKey — it checks
// only id uniqueness — and the hash is the exported
// config.HashKey, so the key verifies exactly like an issued one.
func issueMCPSSEFixedKey(t *testing.T, cfg *config.Config, name, suffix string, scopes []string) (raw, id string) {
	t.Helper()
	if len(suffix) != 16 {
		t.Fatalf("sse fixed key %s: suffix %q must be 16 hex chars", name, suffix)
	}
	id = "gk_" + suffix
	raw = "crk_" + suffix + "0000000000000000"
	k := config.GatewayKey{
		ID:        id,
		Name:      name,
		KeyHash:   config.HashKey(raw),
		Scopes:    append([]string(nil), scopes...),
		CreatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatal(err)
	}
	return raw, id
}

// mcpSSESeedMemory writes one fixed memory record for the
// destructive memory key into the server's memory store file
// (<data-dir>/mcp/memory.jsonl) BEFORE the server is built: the
// memory store is file-backed and reads the file on every call,
// so a seeded file is a seeded store. Fixed id + fixed created_at
// make memory_search / memory_clear re-record byte-identically.
func mcpSSESeedMemory(t *testing.T, cfg *config.Config, callerID string) {
	t.Helper()
	dir := filepath.Join(cfg.DataDir, "mcp")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("{\"id\":\"mem-0000000000000001\",\"caller\":%q,\"text\":\"prefers dark mode\",\"created_at\":\"2026-11-19T12:00:00Z\"}\n", callerID)
	if err := os.WriteFile(filepath.Join(dir, "memory.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// newMCPSSEServer builds the SSE fixture server: the standard
// p1/p2 + a/b/c feed, a live ledger in the config data dir (so
// db_stats reports a real ledger.db with real row counts), one
// routing combo "fast" over p1+p2, and the full 14-key set. The
// ledger holds 4 seeded usage rows: for the router-reader key and
// for the cost-reader key alike, model a on p1 (1000 prompt + 500
// completion, 120 ms) and model c on p2 (100 prompt, 90 ms) —
// cost_report / check_quota read the caller's own 2 rows, while
// get_combo_metrics / pick_fastest_model aggregate all 4. SSE
// enabled before return.
func newMCPSSEServer(t *testing.T) (*httptest.Server, mcpSSEKeys) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	keys := mcpSSEKeys{}
	issue := func(dst *string, idDst *string, name, suffix string, scopes []string) {
		raw, id := issueMCPSSEFixedKey(t, cfg, name, suffix, scopes)
		*dst = raw
		if idDst != nil {
			*idDst = id
		}
		keys.count++
	}
	issue(&keys.admin, nil, "admin", "0000000000000001", []string{"chat", "admin"})
	issue(&keys.read, nil, "reader", "0000000000000002", []string{"mcp:read:status"})
	issue(&keys.empty, nil, "empty", "0000000000000003", []string{})
	issue(&keys.wide, nil, "wide", "0000000000000004", []string{"mcp:read:*"})
	issue(&keys.routerRead, &keys.routerReadID, "router-reader", "0000000000000005", []string{"mcp:read:routing"})
	issue(&keys.routerManage, nil, "router-manager", "0000000000000006", []string{"mcp:manage:routing"})
	// Dual-scope routing key: holds both mcp:manage:routing and
	// mcp:read:routing, so one session can both switch_combos and
	// then list_combos (the same-session read after a switch).
	issue(&keys.routerDual, nil, "router-dual", "0000000000000007", []string{"mcp:manage:routing", "mcp:read:routing"})
	issue(&keys.costRead, &keys.costReadID, "cost-reader", "0000000000000008", []string{"mcp:read:cost"})
	issue(&keys.costManager, &keys.costManagerID, "cost-manager", "0000000000000009", []string{"mcp:manage:cost"})
	issue(&keys.ops, nil, "ops", "000000000000000a", []string{"mcp:manage:ops"})
	issue(&keys.memory, nil, "memory", "000000000000000b", []string{"mcp:manage:memory"})
	issue(&keys.memory2, nil, "memory-2", "000000000000000c", []string{"mcp:manage:memory"})
	issue(&keys.memoryDestr, &keys.memoryDestrID, "memory-destr", "000000000000000d", []string{"mcp:manage:memory", "mcp:destructive"})
	issue(&keys.cache, &keys.cacheID, "cache", "000000000000000e", []string{"mcp:manage:cache", "mcp:destructive"})
	// Combo validation reads providers from cfg (not the dashboard
	// feed), so p1/p2 must exist here for create_combo to validate.
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()

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
	for _, keyID := range []string{keys.routerReadID, keys.costReadID} {
		seed(keyID, "p1", "a", 1000, 500, 120)
		seed(keyID, "p2", "c", 100, 0, 90)
	}

	book := config.NewPriceBook()
	book.Entries["a"] = config.ModelPrice{InputUSDPer1M: mcpSSEFloatPtr(10), OutputUSDPer1M: mcpSSEFloatPtr(30)}
	book.Entries["c"] = config.ModelPrice{InputUSDPer1M: mcpSSEFloatPtr(5), OutputUSDPer1M: mcpSSEFloatPtr(15)}

	mcpSSESeedMemory(t, cfg, keys.memoryDestrID)

	ts := httptest.NewServer(server.New(newMCPSSEStubEngine(),
		server.WithStaticKeys("test-key-123"),
		server.WithLogger(log.New(io.Discard, "", 0)),
		server.WithConfig(cfg),
		server.WithLedger(led),
		server.WithLedgerPrices(prices),
		server.WithPriceBook(book),
		server.WithDashboardProviders(mcpSSEStandardProviders()),
		server.WithRegistry(mcpSSEStandardRegistry()),
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
	))
	t.Cleanup(ts.Close)
	EnableMCPHTTPSSE(t, ts.URL, keys.admin)
	return ts, keys
}

// mcpSSEKeyFor resolves a spec's key name to the raw key of the
// SSE fixture server.
func mcpSSEKeyFor(keys mcpSSEKeys, name string) string {
	switch name {
	case "admin":
		return keys.admin
	case "read":
		return keys.read
	case "empty":
		return keys.empty
	case "wide":
		return keys.wide
	case "router":
		return keys.routerRead
	case "router-manage":
		return keys.routerManage
	case "router-dual":
		return keys.routerDual
	case "cost-reader":
		return keys.costRead
	case "cost-manager":
		return keys.costManager
	case "ops":
		return keys.ops
	case "memory":
		return keys.memory
	case "memory-2":
		return keys.memory2
	case "memory-destr":
		return keys.memoryDestr
	case "cache":
		return keys.cache
	}
	return ""
}

// mcpSSEFixtureServer builds a fresh SSE fixture server and
// returns its URL plus the raw key for the spec's key name.
func mcpSSEFixtureServer(t *testing.T, spec mcpSSERecordSpec) (string, string) {
	t.Helper()
	ts, keys := newMCPSSEServer(t)
	return ts.URL, mcpSSEKeyFor(keys, spec.key)
}

// ---------------------------------------------------------------------------
// Fixture specs — one per recorded exchange. The request ids are
// unique across the whole set, so any subset can run in one session
// in spec order. The specs are the single source of truth for the
// fixture tests AND the recorder below.
// ---------------------------------------------------------------------------

// mcpSSERecordSpec is one recorded SSE exchange: which key it runs
// against, the exact request, and the assertions the live response
// must satisfy. Every spec runs against the single SSE fixture
// server shape (newMCPSSEServer): the key name selects the raw key.
type mcpSSERecordSpec struct {
	name    string
	method  string
	key     string // key name within the SSE fixture server
	request string // exact JSON-RPC request object sent
	assert  MCPSSEAssert
	notes   string
}

// mcpSSEToolNames is the full 24-tool surface in Tools() order:
// the 3 built-ins first, then the registered tools alphabetically.
// The tools-list fixture pins every name (the Streamable fixture
// does the same).
var mcpSSEToolNames = []string{
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

func mcpSSERecordSpecs() []mcpSSERecordSpec {
	err32002 := -32002
	// Every SSE POST answers 202: the POST only accepts the
	// message, the result arrives on the stream.
	ok := func(id float64, paths map[string]any, required ...string) MCPSSEAssert {
		return MCPSSEAssert{
			MCPAssert:  MCPAssert{ID: id, RequiredFields: required, JSONPaths: paths},
			HTTPStatus: 202,
		}
	}
	err := func(id float64, code int, msg ...string) MCPSSEAssert {
		paths := map[string]any{"jsonrpc": "2.0", "error.code": float64(code)}
		if len(msg) > 0 {
			paths["error.message"] = msg[0]
		}
		return MCPSSEAssert{
			MCPAssert: MCPAssert{ID: id, ExpectError: true, ErrorCode: &code,
				RequiredFields: []string{"error.message"},
				JSONPaths:      paths},
			HTTPStatus: 202,
		}
	}
	toolsListPaths := map[string]any{"jsonrpc": "2.0"}
	for i, name := range mcpSSEToolNames {
		toolsListPaths[fmt.Sprintf("result.tools.%d.name", i)] = name
	}
	return []mcpSSERecordSpec{
		{
			name: "initialize", method: "initialize", key: "read",
			request: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-sse-fixture-client","version":"0"}}}`,
			assert: MCPSSEAssert{
				MCPAssert: MCPAssert{ID: float64(1),
					RequiredFields: []string{"result.protocolVersion", "result.capabilities", "result.serverInfo.name", "result.serverInfo.version"},
					JSONPaths:      map[string]any{"jsonrpc": "2.0", "result.protocolVersion": "2025-03-26", "result.serverInfo.name": "codvian-router", "result.serverInfo.version": "0.1.0"}},
				HTTPStatus: 202,
			},
			notes: "Handshake over SSE: the server reports protocol 2025-03-26 and its name/version on the stream's message event; the POST itself answers 202.",
		},
		{
			name: "tools-list", method: "tools/list", key: "read",
			request: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			assert: MCPSSEAssert{
				MCPAssert: MCPAssert{ID: float64(2),
					RequiredFields: []string{"result.tools"},
					JSONPaths:      toolsListPaths,
					JSONLengths:    map[string]int{"result.tools": 24}},
				HTTPStatus: 202,
			},
			notes: "tools/list over SSE announces all 24 tools (every name pinned, plus the count — a 25th tool fails even when every pinned name still matches); the list is unfiltered by caller scopes.",
		},
		{
			name: "tools-call-router-status", method: "tools/call", key: "read",
			request: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`,
			assert:  ok(3, map[string]any{"jsonrpc": "2.0", "result.structuredContent.version": "0.1.0", "result.structuredContent.listen": "unknown", "result.structuredContent.providers": float64(2), "result.structuredContent.enabled_providers": float64(2), "result.structuredContent.models": float64(3), "result.structuredContent.gateway_keys": float64(14), "result.structuredContent.auto_router": false, "result.structuredContent.uptime_seconds": nil}, "result.content", "result.structuredContent"),
			notes:   "router_status over SSE: 2 providers (both enabled), 3 models, 14 gateway keys, no auto-router; uptime is live so presence-only.",
		},
		{
			name: "tools-call-provider-list", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`,
			assert:  ok(4, map[string]any{"jsonrpc": "2.0", "result.structuredContent.providers.0.id": "p1", "result.structuredContent.providers.0.name": "Coder", "result.structuredContent.providers.0.wire_format": "openai", "result.structuredContent.providers.0.enabled": true, "result.structuredContent.providers.0.models.0": "a", "result.structuredContent.providers.0.models.1": "b", "result.structuredContent.providers.1.id": "p2", "result.structuredContent.providers.1.enabled": true, "result.structuredContent.providers.1.models.0": "c"}, "result.content", "result.structuredContent.providers"),
			notes:   "provider_list with the mcp:read:* key: both providers with their registry models; no base URL or key appears.",
		},
		{
			name: "tools-call-model-list", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`,
			assert:  ok(5, map[string]any{"jsonrpc": "2.0", "result.structuredContent.models.0.id": "a", "result.structuredContent.models.0.provider_id": "p1", "result.structuredContent.models.1.id": "b", "result.structuredContent.models.1.provider_id": "p1", "result.structuredContent.models.2.id": "c", "result.structuredContent.models.2.provider_id": "p2"}, "result.content", "result.structuredContent.models"),
			notes:   "model_list with the mcp:read:* key: models a/b on p1, c on p2.",
		},
		{
			name: "tools-call-best-combo-for-task", method: "tools/call", key: "router",
			request: `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat"}}}`,
			assert:  ok(6, map[string]any{"jsonrpc": "2.0", "result.structuredContent.combo.name": "fast", "result.structuredContent.preferred_strategy": "fastest", "result.structuredContent.metrics_known": true, "result.structuredContent.candidates_seen": float64(1)}, "result.content", "result.structuredContent"),
			notes:   "best_combo_for_task over SSE ranks on the seeded measured history — 'fast chat' prefers fastest, combo 'fast' is the only candidate, metrics known. Same assertion as TestMCPHTTPSSESuccessPathM910, now as a fixture.",
		},
		{
			name: "tools-call-cache-flush", method: "tools/call", key: "cache",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`,
			assert:  ok(7, map[string]any{"jsonrpc": "2.0", "result.structuredContent.flushed": float64(0), "result.structuredContent.tenant": "gk_000000000000000e"}, "result.content", "result.structuredContent"),
			notes:   "cache_flush with the cache key: flushed=0 (the test cache holds nothing); tenant is the cache key's fixed id — the per-request identity, not anonymous.",
		},
		{
			name: "tools-call-cache-stats", method: "tools/call", key: "cache",
			request: `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert:  ok(8, map[string]any{"jsonrpc": "2.0", "result.structuredContent.exact.present": false, "result.structuredContent.semantic.present": false, "result.structuredContent.persistent.present": false}, "result.content", "result.structuredContent"),
			notes:   "cache_stats: no cache is wired on this server, so every layer honestly reports present=false, never zero-filled.",
		},
		{
			name: "tools-call-check-quota", method: "tools/call", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"check_quota","arguments":{}}}`,
			assert:  ok(9, map[string]any{"jsonrpc": "2.0", "result.structuredContent.quota.has_budget": false, "result.structuredContent.quota.key_id": "gk_0000000000000008", "result.structuredContent.quota.spent_usd": nil}, "result.content", "result.structuredContent"),
			notes:   "check_quota with no key_id checks the caller's own key: no budget set, so has_budget=false (unlimited, no invented cap). key_id is the cost-reader's fixed id; spent_usd is the float sum 0.0255…, presence-only.",
		},
		{
			name: "tools-call-cost-report", method: "tools/call", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"cost_report","arguments":{}}}`,
			// cost_usd is the float64 sum 0.025 + 0.0005, which the
			// ledger reports as 0.025500000000000002 — an exact pin
			// of 0.0255 would fail on float arithmetic, so the
			// total is presence-only and the per-model costs
			// (which round-trip exactly) pin the $0.0255 total.
			assert: ok(10, map[string]any{"jsonrpc": "2.0", "result.structuredContent.cost_report.requests": float64(2), "result.structuredContent.cost_report.cost_usd": nil, "result.structuredContent.cost_report.cost_known": true, "result.structuredContent.cost_report.key_id": "gk_0000000000000008", "result.structuredContent.cost_report.by_model.0.model": "a", "result.structuredContent.cost_report.by_model.0.cost_usd": float64(0.025), "result.structuredContent.cost_report.by_model.1.model": "c", "result.structuredContent.cost_report.by_model.1.cost_usd": float64(0.0005)}, "result.content", "result.structuredContent"),
			notes:  "cost_report with no key_id covers the caller's own usage only: 2 requests, $0.0255 — never the other keys' rows. key_id is the cost-reader's fixed id.",
		},
		{
			name: "tools-call-create-combo", method: "tools/call", key: "router-manage",
			request: `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"new","members":["p1"]}}}`,
			assert:  ok(11, map[string]any{"jsonrpc": "2.0", "result.structuredContent.created": true, "result.structuredContent.combo.name": "new", "result.structuredContent.combo.strategy": "priority", "result.structuredContent.combo.members.0": "p1"}, "result.content", "result.structuredContent"),
			notes:   "create_combo succeeds through the same ComboStore.Create path POST /v1/combos runs: combo new, member p1, strategy priority.",
		},
		{
			name: "tools-call-db-health", method: "tools/call", key: "ops",
			request: `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"db_health","arguments":{}}}`,
			assert:  ok(12, map[string]any{"jsonrpc": "2.0", "result.structuredContent.healthy": true}, "result.content", "result.structuredContent", "result.structuredContent.files"),
			notes:   "db_health with the mcp:manage:ops key: healthy=true (the seeded ledger.db is readable).",
		},
		{
			name: "tools-call-db-stats", method: "tools/call", key: "ops",
			request: `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"db_stats","arguments":{}}}`,
			assert:  ok(13, map[string]any{"jsonrpc": "2.0", "result.structuredContent.files.0.name": "ledger.db", "result.structuredContent.files.0.exists": true, "result.structuredContent.files.0.size_bytes": nil, "result.structuredContent.files.0.rows.usage_events": float64(4)}, "result.content", "result.structuredContent.files"),
			notes:   "db_stats: live ledger.db in the data dir, 4 seeded usage rows; size_bytes is live (presence-only, never pinned). Same assertion as TestMCPHTTPSSESuccessPathM910, now as a fixture.",
		},
		{
			name: "tools-call-explain-route", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"a"}}}`,
			assert:  ok(14, map[string]any{"jsonrpc": "2.0", "result.structuredContent.route.strategy": "priority", "result.structuredContent.route.attempts.0.provider": "p1", "result.structuredContent.route.attempts.0.price_known": true}, "result.content", "result.structuredContent"),
			notes:   "explain_route dry-runs model a from the live feed: strategy priority, one attempt on p1 with a known price.",
		},
		{
			name: "tools-call-get-combo-metrics", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":"fast"}}}`,
			assert:  ok(15, map[string]any{"jsonrpc": "2.0", "result.structuredContent.metrics.requests": float64(4), "result.structuredContent.metrics.latency_known": true, "result.structuredContent.metrics.mean_latency_ms": float64(105)}, "result.content", "result.structuredContent"),
			notes:   "get_combo_metrics aggregates the ledger over combo fast across all keys: 4 requests, mean latency 105 ms ((120+90+120+90)/4).",
		},
		{
			name: "tools-call-heartbeat", method: "tools/call", key: "read",
			request: `{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert:  ok(16, map[string]any{"jsonrpc": "2.0", "result.structuredContent.protocol_version": "2025-03-26", "result.structuredContent.tool_count": float64(24), "result.structuredContent.audit_log": "enabled", "result.structuredContent.uptime_seconds": nil}, "result.content", "result.structuredContent"),
			notes:   "heartbeat: live snapshot — protocol version, all 24 tools announced, audit log enabled (the HTTP transport opens <data-dir>/mcp/audit.jsonl); uptime is live so presence-only.",
		},
		{
			name: "tools-call-list-combos", method: "tools/call", key: "router",
			request: `{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"list_combos","arguments":{}}}`,
			assert:  ok(17, map[string]any{"jsonrpc": "2.0", "result.structuredContent.combos.0.name": "fast", "result.structuredContent.count": float64(1)}, "result.content", "result.structuredContent", "result.structuredContent.active_combo"),
			notes:   "list_combos on a fresh SSE server lists the configured routing group fast (count 1); active_combo is present (nil on a fresh session — presence-only, since a switched session reports the combo name here).",
		},
		{
			name: "tools-call-memory-add", method: "tools/call", key: "memory-destr",
			request: `{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"prefers dark mode"}}}`,
			assert:  ok(18, map[string]any{"jsonrpc": "2.0", "result.structuredContent.text": "prefers dark mode", "result.structuredContent.id": nil, "result.structuredContent.created_at": nil}, "result.content", "result.structuredContent", "result.structuredContent.id"),
			notes:   "memory_add stores the note under the caller's own tenant and returns its text back; id and created_at are random per run / wall-clock, so presence-only (one of the four fixtures whose recorded bytes vary on re-record — see the recorder's determinism note).",
		},
		{
			name: "tools-call-memory-clear", method: "tools/call", key: "memory-destr",
			request: `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"memory_clear","arguments":{}}}`,
			assert:  ok(19, map[string]any{"jsonrpc": "2.0", "result.structuredContent.cleared": float64(1), "result.structuredContent.caller": "gk_000000000000000d", "result.structuredContent.scope": "caller-only"}, "result.content", "result.structuredContent"),
			notes:   "memory_clear deletes the caller's own seeded memory (cleared=1) and reports the caller's fixed key id — other tenants untouched (nothing else is seeded).",
		},
		{
			name: "tools-call-memory-search", method: "tools/call", key: "memory-destr",
			request: `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"dark mode"}}}`,
			assert:  ok(20, map[string]any{"jsonrpc": "2.0", "result.structuredContent.count": float64(1), "result.structuredContent.entries.0.id": "mem-0000000000000001", "result.structuredContent.entries.0.text": "prefers dark mode", "result.structuredContent.entries.0.matches": float64(1)}, "result.content", "result.structuredContent.entries"),
			notes:   "memory_search for 'dark mode' finds the one seeded memory (fixed id, fixed created_at): occurrence count 1, not a relevance score.",
		},
		{
			name: "tools-call-pick-fastest-model", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{}}}`,
			assert:  ok(21, map[string]any{"jsonrpc": "2.0", "result.structuredContent.model": "c", "result.structuredContent.provider": "p2", "result.structuredContent.mean_latency_ms": float64(90)}, "result.content", "result.structuredContent"),
			notes:   "pick_fastest_model ranks on measured ledger latency: c at 90 ms beats a at 120 ms.",
		},
		{
			name: "tools-call-set-budget-guard", method: "tools/call", key: "cost-manager",
			request: `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"limit_usd":10}}}`,
			assert:  ok(22, map[string]any{"jsonrpc": "2.0", "result.structuredContent.guard_set": true, "result.structuredContent.quota.key_id": "gk_0000000000000009", "result.structuredContent.quota.has_budget": true, "result.structuredContent.quota.limit_usd": float64(10)}, "result.content", "result.structuredContent"),
			notes:   "set_budget_guard with no key_id guards the caller's own key: writes a real $10 ledger budget for the cost-manager key.",
		},
		{
			name: "tools-call-set-routing-strategy", method: "tools/call", key: "router-manage",
			request: `{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":"cheapest"}}}`,
			assert:  ok(23, map[string]any{"jsonrpc": "2.0", "result.structuredContent.strategy.name": "cheapest"}, "result.content", "result.structuredContent"),
			notes:   "set_routing_strategy persists the global strategy as cheapest (read-back shows it) and applies it to the engine.",
		},
		{
			name: "tools-call-simulate-route", method: "tools/call", key: "wide",
			request: `{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"a","prompt_tokens":1000}}}`,
			assert:  ok(24, map[string]any{"jsonrpc": "2.0", "result.structuredContent.simulation.cost_known": true, "result.structuredContent.simulation.estimated_prompt_cost_usd": float64(0.01)}, "result.content", "result.structuredContent"),
			notes:   "simulate_route for model a, 1000 prompt tokens: cost known, $0.01 (1000 tokens at $10/1M).",
		},
		{
			name: "tools-call-switch-combos", method: "tools/call", key: "router-manage",
			request: `{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"fast"}}}`,
			assert:  ok(25, map[string]any{"jsonrpc": "2.0", "result.structuredContent.active_combo": "fast"}, "result.content", "result.structuredContent"),
			notes:   "switch_combos activates 'fast' on this session (per-session active combo).",
		},
		{
			name: "tools-call-tool-search", method: "tools/call", key: "read",
			request: `{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"cache"}}}`,
			assert:  ok(26, map[string]any{"jsonrpc": "2.0", "result.structuredContent.count": float64(3), "result.structuredContent.tools.0.name": "cache_flush", "result.structuredContent.tools.1.name": "cache_stats", "result.structuredContent.tools.2.name": "db_health"}, "result.content", "result.structuredContent"),
			notes:   "tool_search for 'cache' matches exactly the three tools whose name or description mentions the cache (cache_flush, cache_stats, db_health), in Tools() order. Same assertion as TestMCPHTTPSSESuccessPathM910, now as a fixture.",
		},
		{
			name: "tools-call-provider-list-denied", method: "tools/call", key: "read",
			request: `{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`,
			assert:  err(27, err32002, "scope denied: missing required scope(s): mcp:read:providers"),
			notes:   "provider_list with the narrow mcp:read:status key is denied -32002 (scope denial, the most common remote-transport outcome).",
		},
		{
			name: "tools-call-model-list-denied", method: "tools/call", key: "read",
			request: `{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`,
			assert:  err(28, err32002, "scope denied: missing required scope(s): mcp:read:models"),
			notes:   "model_list with the narrow mcp:read:status key is denied -32002 — the denial TestMCPHTTPSSEFlow proves over SSE, now as a fixture.",
		},
		{
			name: "tools-call-check-quota-cross-key", method: "tools/call", key: "cost-reader",
			request: `{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":"other-key"}}}`,
			assert:  err(29, -32602, "mcp: invalid input: key_id \"other-key\" is not the authenticated caller \"gk_0000000000000008\" — a gateway key may only read or set its own quota/budget"),
			notes:   "A cross-key check_quota is rejected -32602, not served: a gateway key may only read its own quota. The message names the caller's fixed key id, so the full message is pinned.",
		},
		{
			name: "tools-call-set-budget-guard-cross-key", method: "tools/call", key: "cost-manager",
			request: `{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"other-key","limit_usd":10}}}`,
			assert:  err(30, -32602, "mcp: invalid input: key_id \"other-key\" is not the authenticated caller \"gk_0000000000000009\" — a gateway key may only read or set its own quota/budget"),
			notes:   "A cross-key set_budget_guard is rejected -32602 and stores nothing for the other key. The message names the caller's fixed key id, so the full message is pinned.",
		},
		{
			name: "tools-call-create-combo-denied", method: "tools/call", key: "router",
			request: `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"new","members":["p1"]}}}`,
			assert:  err(31, err32002, "scope denied: missing required scope(s): mcp:manage:routing"),
			notes:   "create_combo with the router-reader key (mcp:read:routing only) is denied -32002: create_combo requires mcp:manage:routing (RegisterToolScopes in internal/mcp/tools_routing.go; there is no mcp:manage:combos scope, and -32602 is invalid-params, not scope denial). The request is identical to the create-combo success fixture, so the key is the only difference; the scope check runs before dispatch, so no combo is created. Same code/message as the stdio TestM93ScopeDenial denial and the Streamable switch-combos denial.",
		},
	}
}

// mcpSSESpecByName indexes the specs by fixture name.
func mcpSSESpecByName() map[string]mcpSSERecordSpec {
	out := map[string]mcpSSERecordSpec{}
	for _, s := range mcpSSERecordSpecs() {
		out[s.name] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixture tests
// ---------------------------------------------------------------------------

// TestMCPSSEFixtures runs every recorded SSE fixture as a single
// exchange against a fresh real server: Open opens the stream,
// initialize is driven as a fixture, every other fixture opens its
// own stream first with the raw (unrecorded) Initialize step and
// then drives the one recorded exchange.
func TestMCPSSEFixtures(t *testing.T) {
	for _, spec := range mcpSSERecordSpecs() {
		t.Run(spec.name, func(t *testing.T) {
			f := LoadMCPSSEFixture(t, spec.name)
			baseURL, rawKey := mcpSSEFixtureServer(t, spec)
			client := NewMCPSSEClient(baseURL, rawKey)
			client.Open(t)
			defer client.Close(t)
			if spec.method != "initialize" {
				client.Initialize(t)
			}
			RunMCPSSE(t, client, f)
		})
	}
}

// mcpSSESessionGroup is one client session in the fixture-session
// test: a key, and the fixtures driven over that session in order
// (initialize first in every group). Each group builds a fresh
// server, so state never leaks between groups.
type mcpSSESessionGroup struct {
	key      string
	fixtures []string
}

// TestMCPSSEFixtureSession runs all fixtures in session order
// through real SSE sessions — GET opens the stream → initialize →
// tools/list → tools/call — grouped by key, closing each stream
// at the end. This is the end-to-end proof the harness drives
// real sessions, not just isolated exchanges. Stateful fixtures
// are ordered so their assertions hold in sequence: the memory
// group searches the seeded entry, then clears it, then adds a
// new note; the router-manage group switches and creates before
// changing the global strategy.
func TestMCPSSEFixtureSession(t *testing.T) {
	specs := mcpSSESpecByName()
	groups := []mcpSSESessionGroup{
		{key: "read", fixtures: []string{"initialize", "tools-list", "tools-call-router-status", "tools-call-heartbeat", "tools-call-tool-search", "tools-call-provider-list-denied", "tools-call-model-list-denied"}},
		{key: "wide", fixtures: []string{"initialize", "tools-call-provider-list", "tools-call-model-list", "tools-call-pick-fastest-model", "tools-call-explain-route", "tools-call-simulate-route", "tools-call-get-combo-metrics"}},
		{key: "ops", fixtures: []string{"initialize", "tools-call-db-health", "tools-call-db-stats"}},
		{key: "cache", fixtures: []string{"initialize", "tools-call-cache-stats", "tools-call-cache-flush"}},
		{key: "router", fixtures: []string{"initialize", "tools-call-best-combo-for-task", "tools-call-list-combos", "tools-call-create-combo-denied"}},
		{key: "router-manage", fixtures: []string{"initialize", "tools-call-switch-combos", "tools-call-create-combo", "tools-call-set-routing-strategy"}},
		{key: "router-dual", fixtures: []string{"initialize", "tools-call-switch-combos", "tools-call-list-combos"}},
		{key: "cost-reader", fixtures: []string{"initialize", "tools-call-cost-report", "tools-call-check-quota", "tools-call-check-quota-cross-key"}},
		{key: "cost-manager", fixtures: []string{"initialize", "tools-call-set-budget-guard", "tools-call-set-budget-guard-cross-key"}},
		{key: "memory-destr", fixtures: []string{"initialize", "tools-call-memory-search", "tools-call-memory-clear", "tools-call-memory-add"}},
	}
	for _, g := range groups {
		t.Run(g.key, func(t *testing.T) {
			ts, keys := newMCPSSEServer(t)
			rawKey := mcpSSEKeyFor(keys, g.key)
			if rawKey == "" {
				t.Fatalf("session group references unknown key %q", g.key)
			}
			client := NewMCPSSEClient(ts.URL, rawKey)
			fixtures := make([]MCPSSEFixture, 0, len(g.fixtures))
			for _, name := range g.fixtures {
				if _, ok := specs[name]; !ok {
					t.Fatalf("session group %s references unknown fixture %q", g.key, name)
				}
				fixtures = append(fixtures, LoadMCPSSEFixture(t, name))
			}
			RunMCPSSESession(t, client, fixtures...)
		})
	}
}

// TestMCPSSEPostAfterCloseIs404 proves the claim in the fixture
// library's package doc (mcp_sse_fixture.go:39-40): closing the
// GET terminates the session, so a further POST to the same
// session_id is a 404. It opens a session with MCPSSEClient,
// closes it, then POSTs tools/list to the same session id and
// requires 404.
//
// Note the restore step: Close clears the client's session id by
// design (mcp_sse_fixture.go:513-524), and Post refuses to run
// with no session open — so the saved id is put back (same
// package, test-only) purely to address the terminated session.
//
// The POST below is polled, not sent after a fixed sleep: the
// server's GET handler needs a moment to notice the disconnect
// and delete the session, and a fixed pause would fail spuriously
// if that moment ever ran long. Polling repeats a status-only
// POST (not client.Post, which on a 202 would wait for a message
// event on the now-closed stream and fail) every 10ms until the
// session is gone (404) or a 2s deadline expires.
func TestMCPSSEPostAfterCloseIs404(t *testing.T) {
	ts, keys := newMCPSSEServer(t)
	rawKey := mcpSSEKeyFor(keys, "read")
	if rawKey == "" {
		t.Fatal("session group references unknown key \"read\"")
	}
	client := NewMCPSSEClient(ts.URL, rawKey)
	sessionID := client.Open(t)
	client.Close(t)
	if got := client.SessionID(); got != "" {
		t.Fatalf("Close: SessionID() = %q, want \"\" (Close clears the session id)", got)
	}
	client.sessionID = sessionID // re-address the terminated session for the POST below
	var status int
	var body []byte
	pollClient := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(2 * time.Second)
	for {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/api/mcp/sse?session_id="+sessionID, strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"tools/list"}`))
		if err != nil {
			t.Fatalf("POST after Close: build POST: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+rawKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := pollClient.Do(req)
		if err != nil {
			t.Fatalf("POST after Close: POST /api/mcp/sse: %v", err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		status = resp.StatusCode
		if status == http.StatusNotFound || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != http.StatusNotFound {
		t.Fatalf("POST after Close: status %d, want 404 (session terminated): %s", status, body)
	}
}

// TestMCPSSEFixtureRecordedResponsesMatch asserts the recorded
// response in each fixture file still satisfies its own assertions
// — i.e. the files on disk are self-consistent. The POST status
// recorded for SSE is 202 (the POST only accepts the message).
func TestMCPSSEFixtureRecordedResponsesMatch(t *testing.T) {
	for _, spec := range mcpSSERecordSpecs() {
		f := LoadMCPSSEFixture(t, spec.name)
		if err := CheckMCPSSEResponse(f.RecordedHTTPStatus, f.RecordedResponse, f); err != nil {
			t.Errorf("fixture %s: recorded response fails its own assertions: %v", spec.name, err)
		}
	}
}

// TestMCPSSEFixtureSpecsCoverTwentyFourTools pins the gate's
// requirement: the recorded fixtures must cover all 24 tools, each
// at least once as a success call. Denial-only fixtures do not
// count toward a tool's coverage.
func TestMCPSSEFixtureSpecsCoverTwentyFourTools(t *testing.T) {
	want := []string{
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
	covered := map[string]bool{}
	for _, spec := range mcpSSERecordSpecs() {
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
		if !spec.assert.ExpectError {
			covered[req.Params.Name] = true
		}
	}
	for _, tool := range want {
		if !covered[tool] {
			t.Errorf("no SSE success fixture covers tool %q", tool)
		}
	}
	if len(covered) != len(want) {
		t.Errorf("covered %d distinct tools, want exactly the %d tools", len(covered), len(want))
	}
}

// ---------------------------------------------------------------------------
// Toggle proofs for the SSE assertion path
// ---------------------------------------------------------------------------

func TestCheckMCPSSEResponseWrongStatusFails(t *testing.T) {
	f := LoadMCPSSEFixture(t, "tools-list")
	if err := CheckMCPSSEResponse(200, f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPSSEResponse accepted POST 200 against an expectation of 202")
	} else if !strings.Contains(err.Error(), "http status") {
		t.Fatalf("wrong-status error should name http status, got: %v", err)
	}
}

func TestCheckMCPSSEResponseNoMessageDataFails(t *testing.T) {
	f := LoadMCPSSEFixture(t, "tools-list")
	if err := CheckMCPSSEResponse(202, nil, f); err == nil {
		t.Fatal("CheckMCPSSEResponse accepted a 202 POST with no message event data")
	} else if !strings.Contains(err.Error(), "no message event data") {
		t.Fatalf("no-data error should name the missing message event, got: %v", err)
	}
}

func TestCheckMCPSSEResponseWrongJSONPathFails(t *testing.T) {
	f := LoadMCPSSEFixture(t, "tools-list")
	f.Assert.JSONPaths["result.tools.0.name"] = "provider_list" // wrong on purpose: the first tool is router_status
	if err := CheckMCPSSEResponse(202, f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPSSEResponse accepted a tools/list response whose first tool is router_status against an expectation of provider_list")
	} else if !strings.Contains(err.Error(), "result.tools.0.name") {
		t.Fatalf("wrong-path error should name result.tools.0.name, got: %v", err)
	}
}

func TestMCPSSEFixturePathRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"..", "../x", "a/b", "a\\b", ""} {
		if _, err := MCPSSEFixturePath(bad); err == nil {
			t.Fatalf("MCPSSEFixturePath accepted name=%q", bad)
		}
	}
}

// writeMCPSSEFixtureFile writes raw JSON to a temp file and returns
// its path (for loader-rejection tests).
func writeMCPSSEFixtureFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	return path
}

func mcpSSEFixtureJSON(overrides string) string {
	return `{"schema_version":1,"slice":"m9.10","name":"bad","fixture":true,"transport":"sse","key":"wide",` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"recorded_http_status":202,` +
		`"assert":{"id":1,"http_status":202}` + overrides + `}`
}

func TestLoadMCPSSEFixtureRejectsNonFixture(t *testing.T) {
	raw := strings.Replace(mcpSSEFixtureJSON(""), `"fixture":true`, `"fixture":false`, 1)
	if _, err := loadMCPSSEFixtureFile(writeMCPSSEFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-SSE fixture with \"fixture\": false")
	}
}

func TestLoadMCPSSEFixtureRejectsBadTransport(t *testing.T) {
	raw := strings.Replace(mcpSSEFixtureJSON(""), `"transport":"sse"`, `"transport":"streamable"`, 1)
	if _, err := loadMCPSSEFixtureFile(writeMCPSSEFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-SSE fixture with transport streamable")
	}
}

func TestLoadMCPSSEFixtureRejectsStatusMismatch(t *testing.T) {
	raw := strings.Replace(mcpSSEFixtureJSON(""), `"recorded_http_status":202`, `"recorded_http_status":200`, 1)
	if _, err := loadMCPSSEFixtureFile(writeMCPSSEFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-SSE fixture whose recorded_http_status disagrees with assert.http_status")
	}
}

func TestLoadMCPSSEFixtureRejectsUnknownField(t *testing.T) {
	raw := mcpSSEFixtureJSON(`,"bogus_field":1`)
	if _, err := loadMCPSSEFixtureFile(writeMCPSSEFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP-SSE fixture with an unknown field")
	}
}

// ---------------------------------------------------------------------------
// Recorder
// ---------------------------------------------------------------------------

// TestRecordMCPSSEFixtures re-records every fixture by running the
// real server over httptest and capturing the stream's message
// event verbatim. It is skipped unless
// CODVIAN_RECORD_MCP_SSE_FIXTURES=1, so the normal suite never
// rewrites fixtures. The recorded_response written to disk is the
// captured message-event data, not a hand-written value; the test
// refuses to write a file whose assertions do not match the
// captured response.
//
// Determinism note (measured 2026-11-19, two consecutive
// re-records): 26 of 30 files are byte-identical across re-records.
// Four vary, each on exactly one live value inside an otherwise
// verbatim capture — never on an asserted contract field:
//   - tools-call-db-health.json and tools-call-db-stats.json:
//     files.0.modified_at, the ledger.db mtime (ns precision).
//   - tools-call-cost-report.json: cost_report.window_end, the
//     wall clock at call time (ns precision).
//   - tools-call-memory-add.json: the entry id (crypto/rand) and
//     created_at (wall clock, seconds).
//
// Everything else is deterministic by construction: fixed
// gateway-key ids, a fixed memory seed (fixed entry id + fixed
// created_at), a fixed ledger seed, and presence-only assertions
// on uptime / ledger size. (router_status / heartbeat uptime
// stayed 0 across the measured re-records, but that is sub-second
// timing luck, not a guarantee — uptime is asserted
// presence-only for exactly that reason.) If a re-record changes
// any file other than those four, or changes one of the four
// anywhere but its named live field, that is a real regression,
// not noise. Faking determinism by rewriting the live values
// (fixed mtimes, fixed clock) would falsify the verbatim capture,
// so the recorder does not do it.
//
// To re-record: CODVIAN_RECORD_MCP_SSE_FIXTURES=1 go test
// ./internal/apitest/ -run TestRecordMCPSSEFixtures -count=1
func TestRecordMCPSSEFixtures(t *testing.T) {
	if os.Getenv("CODVIAN_RECORD_MCP_SSE_FIXTURES") != "1" {
		t.Skip("recorder only: set CODVIAN_RECORD_MCP_SSE_FIXTURES=1 to re-record fixtures")
	}
	for _, spec := range mcpSSERecordSpecs() {
		baseURL, rawKey := mcpSSEFixtureServer(t, spec)
		client := NewMCPSSEClient(baseURL, rawKey)
		client.Open(t)
		var status int
		var data []byte
		if spec.method == "initialize" {
			// Capture the initialize exchange itself.
			status, data = client.Post(t, json.RawMessage(spec.request))
			if status != 202 {
				t.Fatalf("record %s: initialize: POST status %d, want 202: %s", spec.name, status, data)
			}
		} else {
			client.Initialize(t)
			status, data = client.Post(t, json.RawMessage(spec.request))
		}
		client.Close(t)
		// Prove the assertions match the captured response BEFORE
		// writing anything: a hand-written assert that disagrees
		// with reality fails here instead of landing in the file.
		f := MCPSSEFixture{
			SchemaVersion: 1, Slice: "m9.10", Name: spec.name, IsFixture: true,
			Transport:          "sse",
			RecordedFrom:       "recorded 2026-11-19 by running the real internal/server Server (exported API only: server.New + fixed test gateway keys + With* options, standard p1/p2 + a/b/c feed; ledger-backed: seeded ledger for the routing-reader and cost-reader keys, prices a $10/$30 and c $5/$15 per 1M, routing group fast; memory seeded with one fixed entry for the destructive memory key) over httptest (loopback) via GET /api/mcp/sse (endpoint event) + POST /api/mcp/sse?session_id= with a test gateway key; message-event data captured verbatim. Recorded data only, no live provider call.",
			Key:                spec.key,
			Method:             spec.method,
			Request:            json.RawMessage(spec.request),
			RecordedResponse:   json.RawMessage(data),
			RecordedHTTPStatus: status,
			Assert:             spec.assert,
			Notes:              spec.notes,
		}
		if err := CheckMCPSSEResponse(status, data, f); err != nil {
			t.Fatalf("record %s: assertions do not match the captured response: %v (response: %s)", spec.name, err, data)
		}
		if err := validateMCPSSEFixture(f); err != nil {
			t.Fatalf("record %s: fixture fails validation: %v", spec.name, err)
		}
		raw, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatalf("record %s: marshal: %v", spec.name, err)
		}
		path, err := MCPSSEFixturePath(spec.name)
		if err != nil {
			t.Fatalf("record %s: path: %v", spec.name, err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("record %s: write %s: %v", spec.name, path, err)
		}
		fmt.Printf("recorded tests/fixtures/mcp-sse/%s.json\n", spec.name)
	}
}
