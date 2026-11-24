package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

type fakeSource struct {
	providers []ProviderInfo
	models    []ModelInfo
	status    StatusInfo
	// M9.4: tenant -> key -> value in-memory exact cache, and the
	// file-backed memory store (nil until a test wires one).
	cache map[string]map[string]string
	mem   *MemoryStore
	// M9.3: in-memory routing/cost state. Combos are the test's
	// routing groups; strategies map chain ("" = global) to the
	// canonical strategy name; metrics/stats/cost/quota are the
	// canned ledger answers; rates are prompt USD per 1M tokens
	// keyed "provider/model" (a pair absent from rates is
	// unpriced — the fake never invents a rate). ledgerDown makes
	// the ledger-backed methods fail the way a missing ledger
	// does in production. guards records set_budget_guard calls
	// (key id -> limit) for assertion.
	combos map[string]ComboInfo
	active string
	// createComboCtx records the ctx the last CreateCombo
	// call received, so tests can prove the dispatcher
	// threaded the call's context (not Background) to the
	// Source. createComboCtxErr is that ctx's Err() read
	// AT CALL TIME: handleWithIdentity cancels the derived
	// ctx on return (defer cancel), so reading Err() off
	// the recorded ctx afterwards would always say
	// cancelled, live call or not.
	createComboCtx    context.Context
	createComboCtxErr error
	strategies        map[string]string
	metrics           map[string]ComboMetrics
	stats             []ModelStat
	cost              CostReport
	quota             QuotaReport
	rates             map[string]float64
	ledgerDown        bool
	guards            map[string]float64
	guardPol          map[string]string
}

func (f *fakeSource) Providers() []ProviderInfo { return f.providers }
func (f *fakeSource) Models() []ModelInfo       { return f.models }

// --- M9.3 fake ----------------------------------------------------------
// The fake validates like production (same error class) but over
// in-memory state: unknown combos/models/strategies are
// ErrInvalidInput, and the strategy name list mirrors
// internal/strategy.All (+ aliases) — a test asserts the list
// matches strategy.All so the fake cannot drift silently.

var fakeStrategyNames = map[string]string{
	"priority": "priority", "balanced": "balanced", "cheapest": "cheapest",
	"fastest": "fastest", "most-reliable": "most-reliable", "least-latency": "least-latency",
	"weighted": "weighted", "custom-weights": "custom-weights", "code": "code",
	"round-robin": "round-robin", "random": "random", "strict-random": "strict-random",
	"fill-first": "fill-first", "least-used": "least-used", "p2c": "p2c",
	"quota-weighted": "quota-weighted", "headroom": "headroom", "expiry-first": "expiry-first",
	"reset-aware": "reset-aware", "reset-window": "reset-window", "lkgp": "lkgp",
	"context-optimized": "context-optimized", "cache-optimized": "cache-optimized",
	"context-relay": "context-relay", "cost-optimized": "cost-optimized",
	"ship-fast": "ship-fast", "cost-saver": "cost-saver", "quality-first": "quality-first",
	"offline-friendly": "offline-friendly", "reliability-first": "reliability-first",
	"chaos-mode": "chaos-mode",
	// Aliases (strategy.Parse).
	"cheap": "cheapest", "fast": "fastest", "reliable": "most-reliable",
	"latency": "least-latency", "eco": "cost-optimized", "custom": "custom-weights",
}

var fakeStrategyRules = map[string]string{
	"priority": "Declared order first — targets are tried in the order the operator configured them, with no re-ranking.",
	"cheapest": "Lowest cost first — the cheapest provider that is still reasonably reliable wins.",
	"fastest":  "Lowest latency first, with reliability as a guard — the fastest provider that still succeeds wins.",
	"code":     "Cost per solved task — price times expected attempts, failures penalised harshly.",
}

func fakeComboStrategyName(raw string) (string, bool) {
	if canon, ok := fakeStrategyNames[strings.ToLower(strings.TrimSpace(raw))]; ok {
		return canon, true
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "fusion", "pipeline", "jev":
		return strings.ToLower(strings.TrimSpace(raw)), true
	}
	return "", false
}

func (f *fakeSource) Combos() []ComboInfo {
	out := []ComboInfo{}
	for _, c := range f.combos {
		c.Active = c.Name == f.active
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (f *fakeSource) CreateCombo(ctx context.Context, name string, members []string, strategyName string) error {
	f.createComboCtx = ctx
	f.createComboCtxErr = nil
	if ctx != nil {
		f.createComboCtxErr = ctx.Err()
	}
	// Cancelled first, like production: a cancelled call
	// aborts before validation or mutation.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalidInput)
	}
	if strings.ContainsAny(name, ":/") {
		return fmt.Errorf("%w: name %q must not contain \":\" or \"/\"", ErrInvalidInput, name)
	}
	if len(members) == 0 {
		return fmt.Errorf("%w: members must not be empty", ErrInvalidInput)
	}
	known := make(map[string]bool, len(f.providers))
	for _, p := range f.providers {
		known[p.ID] = true
	}
	for i, m := range members {
		prov := m
		if at := strings.LastIndex(m, "@"); at >= 0 {
			prov = m[:at]
		}
		if !known[prov] {
			return fmt.Errorf("%w: members[%d] names unknown provider %q", ErrInvalidInput, i, prov)
		}
	}
	canon, ok := fakeComboStrategyName(strategyName)
	if !ok {
		return fmt.Errorf("%w: unknown strategy %q", ErrInvalidInput, strategyName)
	}
	if f.combos == nil {
		f.combos = map[string]ComboInfo{}
	}
	if _, exists := f.combos[name]; exists {
		return fmt.Errorf("%w: combo %q already exists", ErrInvalidInput, name)
	}
	f.combos[name] = ComboInfo{Name: name, Members: append([]string(nil), members...), Strategy: canon, Usable: true}
	return nil
}

func (f *fakeSource) ActiveCombo() string { return f.active }

func (f *fakeSource) SetActiveCombo(name string) error {
	if name == "" {
		f.active = ""
		return nil
	}
	if _, ok := f.combos[name]; !ok {
		return fmt.Errorf("%w: unknown combo %q", ErrInvalidInput, name)
	}
	f.active = name
	return nil
}

func (f *fakeSource) ComboMetrics(name string) (ComboMetrics, error) {
	if _, ok := f.combos[name]; !ok {
		return ComboMetrics{}, fmt.Errorf("%w: unknown combo %q", ErrInvalidInput, name)
	}
	if f.ledgerDown {
		return ComboMetrics{}, errors.New("ledger unavailable: no ledger yet (test fake)")
	}
	if m, ok := f.metrics[name]; ok {
		return m, nil
	}
	return ComboMetrics{Name: name, CostKnown: true, Window: "all_time"}, nil
}

func (f *fakeSource) RoutingStrategy(chain string) StrategyInfo {
	global := "priority"
	if f.strategies != nil && f.strategies[""] != "" {
		global = f.strategies[""]
	}
	rule := func(name string) string {
		if r, ok := fakeStrategyRules[name]; ok {
			return r
		}
		return ""
	}
	if chain == "" {
		return StrategyInfo{Name: global, Rule: rule(global), Scope: "global"}
	}
	if f.strategies != nil {
		if st, ok := f.strategies[chain]; ok {
			return StrategyInfo{Name: st, Rule: rule(st), Scope: "chain", Chain: chain}
		}
	}
	return StrategyInfo{Name: global, Rule: rule(global), Scope: "global", Chain: chain, Inherited: true}
}

func (f *fakeSource) SetRoutingStrategy(chain, strategyName string) error {
	canon, ok := fakeStrategyNames[strings.ToLower(strings.TrimSpace(strategyName))]
	if !ok {
		return fmt.Errorf("%w: unknown strategy %q", ErrInvalidInput, strategyName)
	}
	if f.strategies == nil {
		f.strategies = map[string]string{}
	}
	f.strategies[chain] = canon
	return nil
}

func (f *fakeSource) fakeAttempts(model string) ([]RouteAttempt, error) {
	if strings.HasPrefix(model, "auto/") {
		return nil, fmt.Errorf("%w: model %q is a virtual auto-router model", ErrInvalidInput, model)
	}
	var attempts []RouteAttempt
	for _, p := range f.providers {
		offers := false
		for _, m := range p.Models {
			if m == model {
				offers = true
				break
			}
		}
		if !offers {
			continue
		}
		att := RouteAttempt{Provider: p.ID, ProviderName: p.Name, Model: model, Enabled: p.Enabled}
		if rate, ok := f.rates[p.ID+"/"+model]; ok {
			att.PriceKnown = true
			att.PromptUSDPer1M = &rate
		}
		attempts = append(attempts, att)
	}
	if len(attempts) == 0 {
		return nil, fmt.Errorf("%w: unknown model %q (no configured provider offers it)", ErrInvalidInput, model)
	}
	return attempts, nil
}

func (f *fakeSource) ExplainRoute(model string) (RouteExplanation, error) {
	attempts, err := f.fakeAttempts(model)
	if err != nil {
		return RouteExplanation{}, err
	}
	info := f.RoutingStrategy(model)
	return RouteExplanation{
		Model: model, Strategy: info.Name, Rule: info.Rule, Attempts: attempts,
		Note: "test fake: declared order only",
	}, nil
}

func (f *fakeSource) SimulateRoute(model string, promptTokens int) (RouteSimulation, error) {
	if promptTokens < 0 {
		return RouteSimulation{}, fmt.Errorf("%w: prompt_tokens must be >= 0", ErrInvalidInput)
	}
	attempts, err := f.fakeAttempts(model)
	if err != nil {
		return RouteSimulation{}, err
	}
	info := f.RoutingStrategy(model)
	sim := RouteSimulation{Model: model, Strategy: info.Name, PromptTokens: promptTokens, Note: "test fake: dry run only"}
	for i, att := range attempts {
		sa := SimAttempt{Provider: att.Provider, Model: att.Model, PriceKnown: att.PriceKnown}
		if i == len(attempts)-1 {
			sa.OnFailure = "fail the request (no further targets)"
		} else {
			sa.OnFailure = "fail over to the next target on a retryable failure"
		}
		if att.PriceKnown && att.PromptUSDPer1M != nil && promptTokens > 0 {
			cost := float64(promptTokens) * (*att.PromptUSDPer1M) / 1000000
			sa.EstimatedPromptCostUSD = &cost
		}
		sim.Attempts = append(sim.Attempts, sa)
	}
	if len(sim.Attempts) > 0 && sim.Attempts[0].EstimatedPromptCostUSD != nil {
		sim.EstimatedPromptCostUSD = sim.Attempts[0].EstimatedPromptCostUSD
		sim.CostKnown = true
	}
	return sim, nil
}

func (f *fakeSource) ModelStats() []ModelStat {
	if f.ledgerDown {
		return []ModelStat{}
	}
	if f.stats == nil {
		return []ModelStat{}
	}
	return f.stats
}

func (f *fakeSource) CostReport(keyID string, days int) (CostReport, error) {
	if days < 0 || days > 365 {
		return CostReport{}, fmt.Errorf("%w: days must be between 0 (all time) and 365", ErrInvalidInput)
	}
	if f.ledgerDown {
		return CostReport{}, errors.New("ledger unavailable: no ledger yet (test fake)")
	}
	return f.cost, nil
}

func (f *fakeSource) CheckQuota(keyID string) (QuotaReport, error) {
	if strings.TrimSpace(keyID) == "" {
		return QuotaReport{}, fmt.Errorf("%w: key_id must not be empty", ErrInvalidInput)
	}
	if f.ledgerDown {
		return QuotaReport{}, errors.New("ledger unavailable: no ledger yet (test fake)")
	}
	return f.quota, nil
}

func (f *fakeSource) SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, daily, weekly, monthly *float64) error {
	if strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("%w: key_id must not be empty", ErrInvalidInput)
	}
	if limitUSD < 0 {
		return fmt.Errorf("%w: limit_usd must be >= 0", ErrInvalidInput)
	}
	if unpricedPolicy != "" && unpricedPolicy != "fail_closed" && unpricedPolicy != "count_as_zero" {
		return fmt.Errorf("%w: unknown unpriced_policy %q", ErrInvalidInput, unpricedPolicy)
	}
	for name, v := range map[string]*float64{"daily": daily, "weekly": weekly, "monthly": monthly} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%w: %s_limit_usd must be >= 0", ErrInvalidInput, name)
		}
	}
	if f.guards == nil {
		f.guards = map[string]float64{}
		f.guardPol = map[string]string{}
	}
	f.guards[keyID] = limitUSD
	f.guardPol[keyID] = unpricedPolicy
	f.quota = QuotaReport{KeyID: keyID, HasBudget: true, LimitUSD: limitUSD, UnpricedPolicy: unpricedPolicy}
	return nil
}

func (f *fakeSource) PriceKnown(provider, model string) bool {
	_, ok := f.rates[provider+"/"+model]
	return ok
}
func (f *fakeSource) Status() StatusInfo { return f.status }

// CacheStats counts the fake's in-memory exact layer only; the
// semantic and persistent layers are not wired in tests, so they
// report Present=false — the same honesty rule as production.
func (f *fakeSource) CacheStats() CacheStats {
	entries, bytes := 0, 0
	for _, keys := range f.cache {
		for _, v := range keys {
			entries++
			bytes += len(v)
		}
	}
	return CacheStats{
		Exact:      CacheLayerStats{Present: true, Entries: entries, Bytes: bytes},
		Semantic:   CacheLayerStats{Present: false},
		Persistent: CacheLayerStats{Present: false},
		Note:       "test fake: in-memory exact layer only",
	}
}

// CacheFlush deletes only the named tenant's entries.
func (f *fakeSource) CacheFlush(tenant string) (int, error) {
	keys, ok := f.cache[tenant]
	if !ok {
		return 0, nil
	}
	n := len(keys)
	delete(f.cache, tenant)
	return n, nil
}

// DBHealth/DBStats return a fixed fixture (the fake's equivalent
// of a live database): one existing ledger.db with a known row
// count. It is a test fixture, not a production measurement.
func (f *fakeSource) DBHealth() DBHealth {
	return DBHealth{
		Healthy: true,
		Files: []DBFileHealth{
			{Name: "ledger.db", Exists: true, SizeBytes: 1234, Rows: map[string]int64{"usage_events": 7}},
		},
		Note: "test fake: fixed fixture, not a live database",
	}
}

func (f *fakeSource) DBStats() DBStats {
	h := f.DBHealth()
	return DBStats{Files: h.Files, Note: h.Note}
}

func (f *fakeSource) MemoryAdd(caller, text string) (MemoryEntry, error) {
	if f.mem == nil {
		return MemoryEntry{}, errors.New("mcp: test fake has no memory store configured")
	}
	return f.mem.Add(caller, text)
}

func (f *fakeSource) MemorySearch(caller, query string) ([]MemoryEntry, error) {
	if f.mem == nil {
		return nil, errors.New("mcp: test fake has no memory store configured")
	}
	return f.mem.Search(caller, query)
}

func (f *fakeSource) MemoryClear(caller string) (int, error) {
	if f.mem == nil {
		return 0, errors.New("mcp: test fake has no memory store configured")
	}
	return f.mem.Clear(caller)
}

// P1.2 ecosystem expansion — fakeSource stub implementations.

func (f *fakeSource) CapabilityEntries() []CapabilityEntry {
	out := []CapabilityEntry{}
	for _, m := range f.models {
		out = append(out, CapabilityEntry{
			ProviderID: m.ProviderID,
			ModelID:    m.ID,
			Capability: "tool_calling",
			Supported:  nil,
			Native:     false,
			Source:     "test_fake",
			Confidence: 0.0,
		})
	}
	return out
}

func (f *fakeSource) QuotaIntelligence(filter string) ([]QuotaIntelEntry, error) {
	out := []QuotaIntelEntry{}
	if f.quota.KeyID != "" {
		state := "healthy"
		if f.quota.AtLimit {
			state = "exhausted"
		} else if f.quota.WarningReached {
			state = "approaching_limit"
		}
		entry := QuotaIntelEntry{
			Kind:       "key_budget",
			ID:         f.quota.KeyID,
			Name:       f.quota.KeyID,
			Scope:      "key",
			Target:     f.quota.KeyID,
			BudgetUSD:  f.quota.LimitUSD,
			Consumed:   f.quota.SpentUSD,
			State:      state,
			Source:     "test_fake",
			Confidence: 0.95,
			UpdatedAt:  "2026-01-01T00:00:00Z",
			Filters:    map[string]string{"key_id": f.quota.KeyID},
		}
		if f.quota.LimitUSD > 0 {
			entry.UsedPct = f.quota.SpentUSD / f.quota.LimitUSD * 100
		}
		out = append(out, entry)
	}
	return out, nil
}

func (f *fakeSource) BudgetForecast(keyID string) (BudgetForecastEntry, error) {
	return BudgetForecastEntry{
		KeyID:            keyID,
		InsufficientData: true,
		UpdatedAt:        "2026-01-01T00:00:00Z",
	}, nil
}

func (f *fakeSource) CompressionStats() CompressionStatsEntry {
	return CompressionStatsEntry{Layers: 0, LastUpdated: "2026-01-01T00:00:00Z"}
}

func (f *fakeSource) EvalSchedules() ([]EvalScheduleEntry, error) {
	return []EvalScheduleEntry{}, nil
}

func (f *fakeSource) OrgReport() OrgReportEntry {
	return OrgReportEntry{UpdatedAt: "2026-01-01T00:00:00Z"}
}

func newTestServer() *Server {
	// The tool-behaviour tests below are about the tools, not the
	// identity default, so this helper opts into the local
	// operator explicitly — the same explicit opt-in production
	// stdio uses. A bare New() server denies every tool (see
	// TestNewDefaultsToAnonymousDenyAll, the M9.1 B1 regression
	// test); nothing here may rely on the default allowing.
	return New(&fakeSource{
		providers: []ProviderInfo{
			{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}, CatalogID: "groq", Verification: "verified-from-docs"},
			{ID: "p2", Name: "Chatter", WireFormat: "anthropic", Enabled: false, Models: []string{"chat-model", "chat-model-2"}},
		},
		models: []ModelInfo{
			{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"},
			{ID: "chat-model", ProviderID: "p2", ProviderName: "Chatter"},
		},
		status: StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 2, EnabledProviders: 1, Models: 3, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1},
	}, "0.1.0").WithClientIdentity(LocalOperator())
}

func call(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	raw := s.Handle(context.Background(), []byte(msg))
	if raw == nil {
		t.Fatalf("Handle(%s) returned nil, want a response", msg)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	return out
}

func TestInitialize(t *testing.T) {
	// Sends the current protocol version. The server ignores the
	// client's sent version entirely and always answers
	// ProtocolVersion (mcp.go:538); there is no old-version
	// negotiation to test here.
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize result missing: %v", out)
	}
	// Literal pin, not just == ProtocolVersion: a revert of the
	// const must fail this test (see also
	// TestM95ProtocolVersionIs20250326).
	if result["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want 2025-03-26", result["protocolVersion"])
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info["name"] != ServerName || info["version"] != "0.1.0" {
		t.Errorf("serverInfo = %v, want name %s version 0.1.0", info, ServerName)
	}
}

func TestToolsList(t *testing.T) {
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	// 32 tools: 24 baseline (3 built-in + 9 routing + 3 cost + 7 ops + 2 search)
	// plus 8 P1.2 ecosystem-expansion tools.
	if len(tools) != 32 {
		t.Fatalf("tools/list returned %d tools, want 32: %v", len(tools), tools)
	}
	want := map[string]bool{"router_status": true, "provider_list": true, "model_list": true,
		"list_combos": true, "create_combo": true, "switch_combos": true, "get_combo_metrics": true,
		"set_routing_strategy": true, "best_combo_for_task": true, "explain_route": true,
		"simulate_route": true, "pick_fastest_model": true,
		"cost_report": true, "check_quota": true, "set_budget_guard": true,
		"cache_stats": true, "cache_flush": true, "db_health": true, "db_stats": true,
		"memory_add": true, "memory_search": true, "memory_clear": true,
		"tool_search": true, "heartbeat": true,
		// P1.2 ecosystem expansion tools.
		"provider_discovery": true, "capability_discovery": true, "quota_intelligence": true,
		"budget_forecasting": true, "route_preview": true, "compression_analytics": true,
		"eval_management": true, "org_reporting": true}
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		name, _ := m["name"].(string)
		if !want[name] {
			t.Errorf("unexpected tool in tools/list: %v", name)
		}
		delete(want, name)
		if m["inputSchema"] == nil {
			t.Errorf("tool %v has no inputSchema", m["name"])
		}
	}
	if len(want) != 0 {
		t.Errorf("missing tools: %v", want)
	}
}

func TestProviderListReturnsLiveData(t *testing.T) {
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	providers, _ := sc["providers"].([]any)
	if len(providers) != 2 {
		t.Fatalf("provider_list returned %d providers, want 2", len(providers))
	}
	first, _ := providers[0].(map[string]any)
	if first["name"] != "Coder" || first["wire_format"] != "openai" {
		t.Errorf("first provider = %v, want Coder/openai", first)
	}
	// No secrets ever: no base URL, no key in any provider entry.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "base_url") || strings.Contains(string(raw), "api_key") {
		t.Errorf("provider_list leaked a secret field: %s", raw)
	}
}

func TestModelListReturnsLiveData(t *testing.T) {
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	models, _ := sc["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("model_list returned %d models, want 2", len(models))
	}
}

func TestRouterStatusReturnsLiveData(t *testing.T) {
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`)
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	if sc["providers"] != float64(2) || sc["enabled_providers"] != float64(1) || sc["models"] != float64(3) {
		t.Errorf("router_status = %v, want providers 2 enabled 1 models 3", sc)
	}
	if sc["listen"] != "127.0.0.1:20128" || sc["auto_router"] != true {
		t.Errorf("router_status = %v, want listen 127.0.0.1:20128 auto_router true", sc)
	}
	// The text content carries the same live payload.
	content, _ := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("router_status content has %d entries, want 1", len(content))
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "127.0.0.1:20128") {
		t.Errorf("router_status text does not carry the live status: %s", text)
	}
}

func TestUnknownMethodAndTool(t *testing.T) {
	out := call(t, newTestServer(), `{"jsonrpc":"2.0","id":6,"method":"resources/list"}`)
	errBody, _ := out["error"].(map[string]any)
	if errBody["code"] != float64(-32601) {
		t.Errorf("unknown method code = %v, want -32601", errBody["code"])
	}
	out = call(t, newTestServer(), `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"chat","arguments":{}}}`)
	errBody, _ = out["error"].(map[string]any)
	if errBody["code"] != float64(-32602) {
		t.Errorf("unknown tool code = %v, want -32602", errBody["code"])
	}
	if !strings.Contains(errBody["message"].(string), "chat") {
		t.Errorf("unknown tool message %v does not name the tool", errBody["message"])
	}
}

func TestParseErrorAndNotification(t *testing.T) {
	s := newTestServer()
	raw := s.Handle(context.Background(), []byte(`{not json`))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse-error response is not JSON: %v", err)
	}
	errBody, _ := out["error"].(map[string]any)
	if errBody["code"] != float64(-32700) {
		t.Errorf("parse error code = %v, want -32700", errBody["code"])
	}
	// A notification gets no response at all.
	if got := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); got != nil {
		t.Errorf("notification returned %s, want nil", got)
	}
	if got := s.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","method":"ping"}`)); got != nil {
		t.Errorf("ping notification returned %s, want nil", got)
	}
	// ... but ping as a request does answer.
	out = call(t, s, `{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	if out["result"] == nil {
		t.Errorf("ping request has no result: %v", out)
	}
}

func TestServeStdioRoundTrip(t *testing.T) {
	s := newTestServer()
	in := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n" +
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"provider_list\",\"arguments\":{}}}\n" +
			"\n" + // blank lines are skipped, not errors
			"{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/list\"}\n")
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("Serve wrote %d response lines, want 3 (notification gets none): %s", len(lines), out.String())
	}
	var third map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &third); err != nil {
		t.Fatalf("third response is not JSON: %v", err)
	}
	result, _ := third["result"].(map[string]any)
	if tools, _ := result["tools"].([]any); len(tools) != 24 {
		t.Errorf("tools/list via Serve returned %d tools, want 24", len(tools))
	}
	if !strings.Contains(lines[1], "Coder") {
		t.Errorf("provider_list via Serve does not carry live data: %s", lines[1])
	}
}

func TestNewPanicsOnNilSource(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("New(nil, ...) did not panic")
		}
	}()
	_ = New(nil, "0.1.0")
}

func TestToolsMatchesDashboardList(t *testing.T) {
	// Tools() is the single source for the dashboard MCP section;
	// every tool must have a non-empty description.
	for _, tool := range Tools() {
		if tool.Name == "" || tool.Description == "" {
			t.Errorf("tool with empty name/description: %+v", tool)
		}
	}
}

// ---------------------------------------------------------------------------
// MCP transport hardening: H5 (oversized stdio line) + H6 (request
// context threading). See internal/mcp/mcp.go.
// ---------------------------------------------------------------------------

// TestServeOversizedLineKeepsServing proves H5: one stdio line over
// the 1 MiB cap answers -32700 for that line and the server keeps
// serving the lines after it — it does not die (the old
// bufio.Scanner loop returned on ErrTooLong and the process
// exited). The oversized content is never echoed back.
func TestServeOversizedLineKeepsServing(t *testing.T) {
	s := newTestServer()
	oversized := strings.Repeat("x", mcpMaxStdioLineBytes+1024)
	in := strings.NewReader(
		oversized + "\n" +
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}` + "\n")
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("Serve wrote %d response lines, want 3 (one -32700, then initialize + provider_list): %s", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first response is not JSON: %v (%s)", err, lines[0])
	}
	errObj, _ := first["error"].(map[string]any)
	code, _ := errObj["code"].(float64)
	if code != -32700 {
		t.Errorf("oversized line error code = %v, want -32700", code)
	}
	if msg, _ := errObj["message"].(string); !strings.Contains(msg, "1 MiB") {
		t.Errorf("oversized line error message = %q, want it to name the 1 MiB cap", msg)
	}
	if strings.Contains(out.String(), strings.Repeat("x", 64)) {
		t.Errorf("oversized line content was echoed back in the response")
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("second response is not JSON: %v (%s)", err, lines[1])
	}
	result, _ := second["result"].(map[string]any)
	if result["protocolVersion"] != ProtocolVersion {
		t.Errorf("initialize after the oversized line = %v, want protocolVersion %q (server stopped serving)", result["protocolVersion"], ProtocolVersion)
	}
	if !strings.Contains(lines[2], "Coder") {
		t.Errorf("provider_list after the oversized line does not carry live data: %s", lines[2])
	}
}

// TestServeLongLineUnderCap pins the reader behind Serve: a valid
// line far longer than the 64 KiB read buffer but under the 1 MiB
// cap is reassembled across buffer refills and answered, not
// truncated or refused.
func TestServeLongLineUnderCap(t *testing.T) {
	s := newTestServer()
	pad := strings.Repeat(" ", 200*1024)
	in := strings.NewReader(pad + `{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")
	var out bytes.Buffer
	if err := s.Serve(in, &out); err != nil {
		t.Fatalf("Serve returned error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("Serve wrote %d response lines, want 1: %s", len(lines), out.String())
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, lines[0])
	}
	if _, ok := env["result"]; !ok {
		t.Errorf("long under-cap ping response = %v, want a result", env)
	}
}

// blockingContextSource is a ContextSource whose ComboMetrics
// blocks until the bound context is done, then answers with the
// context error — a stand-in for a hung ledger call. With no
// bound context (the pre-H6 behaviour: the Source never sees
// the request ctx) it blocks on a 10-second timer instead, so a
// test that waits only 5 seconds fails there — the test fails
// without the fix, promptly, rather than hanging the suite.
type blockingContextSource struct {
	Source
	ctx context.Context
}

func (b *blockingContextSource) WithContext(ctx context.Context) Source {
	cp := *b
	cp.ctx = ctx
	return &cp
}

func (b *blockingContextSource) ComboMetrics(name string) (ComboMetrics, error) {
	if b.ctx == nil {
		select {
		case <-time.After(10 * time.Second):
			return ComboMetrics{}, nil
		}
	}
	<-b.ctx.Done()
	return ComboMetrics{}, b.ctx.Err()
}

// deadlineRecordingSource is a ContextSource whose ComboMetrics
// reports (on a buffered channel) whether the bound context
// carries a deadline, then answers immediately.
type deadlineRecordingSource struct {
	Source
	ctx        context.Context
	deadlineCh chan bool
}

func (s *deadlineRecordingSource) WithContext(ctx context.Context) Source {
	cp := *s
	cp.ctx = ctx
	return &cp
}

func (s *deadlineRecordingSource) ComboMetrics(name string) (ComboMetrics, error) {
	_, ok := s.ctx.Deadline()
	select {
	case s.deadlineCh <- ok:
	default:
	}
	return ComboMetrics{Name: name, CostKnown: true, Window: "all_time"}, nil
}

const h6ComboMetricsCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":"fast"}}}`

// TestHandleWithIdentityThreadsContext proves H6: a tool call
// whose backing store blocks is aborted as soon as the request
// context is cancelled (a disconnected client), instead of
// hanging the handler goroutine.
func TestHandleWithIdentityThreadsContext(t *testing.T) {
	src := &blockingContextSource{Source: &fakeSource{
		combos: map[string]ComboInfo{"fast": {Name: "fast", Members: []string{"p1"}, Strategy: "priority", Usable: true}},
	}}
	srv := New(src, "0.1.0").WithClientIdentity(LocalOperator())
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the client went away before dispatch
	done := make(chan []byte, 1)
	go func() { done <- srv.HandleWithIdentity(ctx, []byte(h6ComboMetricsCall), LocalOperator()) }()
	select {
	case raw := <-done:
		if raw == nil {
			t.Fatal("HandleWithIdentity returned nil, want an error envelope")
		}
		var env map[string]any
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("response is not JSON: %v (%s)", err, raw)
		}
		errObj, _ := env["error"].(map[string]any)
		code, _ := errObj["code"].(float64)
		msg, _ := errObj["message"].(string)
		if code != -32602 || !strings.Contains(msg, "context canceled") {
			t.Errorf("cancelled tool call = code %v msg %q, want -32602 naming the cancelled context", code, msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HandleWithIdentity did not return within 5s for a cancelled context: the tool call is not honouring ctx (H6)")
	}
}

// TestHandleWithIdentityAppliesToolCallTimeout proves the other
// half of H6: even a request that arrives with a bare
// context.Background() is bounded — the context the Source sees
// carries a deadline (the 30-second tool-call timeout), so a
// slow store cannot run unbounded.
func TestHandleWithIdentityAppliesToolCallTimeout(t *testing.T) {
	src := &deadlineRecordingSource{
		Source: &fakeSource{
			combos: map[string]ComboInfo{"fast": {Name: "fast", Members: []string{"p1"}, Strategy: "priority", Usable: true}},
		},
		deadlineCh: make(chan bool, 1),
	}
	srv := New(src, "0.1.0").WithClientIdentity(LocalOperator())
	raw := srv.HandleWithIdentity(context.Background(), []byte(h6ComboMetricsCall), LocalOperator())
	if raw == nil {
		t.Fatal("HandleWithIdentity returned nil, want a result envelope")
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if _, ok := env["result"]; !ok {
		t.Fatalf("tool call failed, want a result: %v", env)
	}
	select {
	case ok := <-src.deadlineCh:
		if !ok {
			t.Error("the context bound to the Source has no deadline: the tool-call timeout was not applied (H6)")
		}
	default:
		t.Error("the Source never saw a bound context: WithContext was not called (H6)")
	}
}
