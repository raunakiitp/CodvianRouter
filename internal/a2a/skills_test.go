// M9.9 — A2A skills tests.
//
// The fake below is a fake SkillSource, NOT a fake router result:
// it provides canned INPUTS (a fixed provider list, combos,
// ledger answers, cache/DB state) and the tests assert the skills
// transform those inputs correctly — every expected value comes
// from the fake's inputs. The fixture mirrors the MCP routing
// fixture (internal/mcp/tools_routing_test.go
// newRoutingTestServer) so the skill ranking cases pin the same
// outcomes as TestM93BestComboForTask.
//
// A note on "real source" tests: the MCP fakeSource is NOT usable
// here — it is defined in internal/mcp's _test.go files (package
// mcp), and Go cannot import another package's test files. The
// production sources are not importable either:
// registryMCPSource lives in package main (cmd/codvian-router),
// and serverMCPSource lives in internal/server, which imports
// internal/a2a — importing it back would be a cycle. So the
// real-source coverage is TestListCapabilitiesLiveRegistries,
// which asserts the skill's strategies and MCP tools against the
// LIVE strategy.All registry and the LIVE mcp.Tools() registry
// (real production code, no fake involved), and
// TestSkillSourceSatisfiedByMCP, which proves at compile time
// that any mcp.Source satisfies SkillSource.
package a2a

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

func f64(v float64) *float64 { return &v }

// fakeSkillSource is a SkillSource over canned inputs. It mirrors
// the production sources' contracts: an unknown combo is
// ErrInvalidInput, a missing metrics entry is zero metrics (a
// combo with no history — CostKnown true, Window "all_time"),
// and ledgerDown makes the ledger-backed methods fail the way a
// missing ledger does in production.
type fakeSkillSource struct {
	providers  []mcp.ProviderInfo
	combos     map[string]mcp.ComboInfo
	metrics    map[string]mcp.ComboMetrics
	cost       mcp.CostReport
	quota      mcp.QuotaReport
	status     mcp.StatusInfo
	cache      mcp.CacheStats
	db         mcp.DBHealth
	strategy   mcp.StrategyInfo
	ledgerDown bool
	// Records, for asserting the caller-identity rule actually
	// reaches the source.
	quotaCalls   int
	lastQuotaKey string
	costCalls    int
	lastCostKey  string
	lastCostDays int
}

func newFakeSkillSource() *fakeSkillSource {
	sr := 0.9
	return &fakeSkillSource{
		providers: []mcp.ProviderInfo{
			{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}, CatalogID: "groq", Verification: "verified-from-docs"},
			{ID: "p2", Name: "Chatter", WireFormat: "anthropic", Enabled: false, Models: []string{"chat-model", "chat-model-2"}},
		},
		combos: map[string]mcp.ComboInfo{
			"fast":  {Name: "fast", Members: []string{"p1"}, Strategy: "fastest", Usable: true},
			"mixed": {Name: "mixed", Members: []string{"p1", "p2"}, Strategy: "priority", Usable: false, UnusableReason: "p2: provider disabled"},
		},
		metrics: map[string]mcp.ComboMetrics{
			"fast": {Name: "fast", Requests: 10, OK: 9, Errors: 1, SuccessRate: &sr, PromptTokens: 1000, CompletionTokens: 500, TotalTokens: 1500, CostUSD: 0.05, CostKnown: true, MeanLatencyMS: 120, LatencyKnown: true, LatencyEvents: 9, Window: "all_time"},
		},
		cost: mcp.CostReport{
			KeyID: "key-1", Days: 7, Requests: 5, OK: 5, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150,
			CostUSD: 0.12, CostKnown: true,
			ByModel: []mcp.ModelCost{{Provider: "p1", Model: "coder-model", Requests: 5, PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CostUSD: 0.12, PriceKnown: true}},
		},
		quota: mcp.QuotaReport{
			KeyID: "key-1", HasBudget: true, LimitUSD: 10, SpentUSD: 2.5, RemainingUSD: f64(7.5),
			UnpricedPolicy: "fail_closed", ActiveWindow: "rolling_5h",
			Daily: mcp.QuotaWindow{LimitUSD: f64(20), SpentUSD: 2.5, RemainingUSD: f64(17.5)},
		},
		status:   mcp.StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 2, EnabledProviders: 1, Models: 3, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1},
		cache:    mcp.CacheStats{Exact: mcp.CacheLayerStats{Present: true, Entries: 3, Bytes: 120}, Semantic: mcp.CacheLayerStats{Present: false}, Persistent: mcp.CacheLayerStats{Present: false}},
		db:       mcp.DBHealth{Healthy: true, Note: "sentinel-note usage_events 99", Files: []mcp.DBFileHealth{{Name: "ledger.db", Exists: true, SizeBytes: 4096, ModifiedAt: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), Rows: map[string]int64{"usage_events": 10}}}},
		strategy: mcp.StrategyInfo{Name: "priority", Rule: "Declared order first — targets are tried in the order the operator configured them, with no re-ranking.", Scope: "global"},
	}
}

func (f *fakeSkillSource) Providers() []mcp.ProviderInfo { return f.providers }

func (f *fakeSkillSource) Combos() []mcp.ComboInfo {
	out := make([]mcp.ComboInfo, 0, len(f.combos))
	for _, c := range f.combos {
		out = append(out, c)
	}
	// Stable order by name, so ranking ties are deterministic.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func (f *fakeSkillSource) ComboMetrics(name string) (mcp.ComboMetrics, error) {
	if _, ok := f.combos[name]; !ok {
		return mcp.ComboMetrics{}, &fakeInvalidInputError{msg: "unknown combo " + name}
	}
	if f.ledgerDown {
		return mcp.ComboMetrics{}, &fakeLedgerError{}
	}
	if m, ok := f.metrics[name]; ok {
		return m, nil
	}
	return mcp.ComboMetrics{Name: name, CostKnown: true, Window: "all_time"}, nil
}

func (f *fakeSkillSource) CheckQuota(keyID string) (mcp.QuotaReport, error) {
	f.quotaCalls++
	f.lastQuotaKey = keyID
	if strings.TrimSpace(keyID) == "" {
		return mcp.QuotaReport{}, &fakeInvalidInputError{msg: "key_id must not be empty"}
	}
	if f.ledgerDown {
		return mcp.QuotaReport{}, &fakeLedgerError{}
	}
	return f.quota, nil
}

func (f *fakeSkillSource) CostReport(keyID string, days int) (mcp.CostReport, error) {
	f.costCalls++
	f.lastCostKey = keyID
	f.lastCostDays = days
	if days < 0 || days > 365 {
		return mcp.CostReport{}, &fakeInvalidInputError{msg: "days must be between 0 (all time) and 365"}
	}
	if f.ledgerDown {
		return mcp.CostReport{}, &fakeLedgerError{}
	}
	return f.cost, nil
}

func (f *fakeSkillSource) Status() mcp.StatusInfo                  { return f.status }
func (f *fakeSkillSource) CacheStats() mcp.CacheStats              { return f.cache }
func (f *fakeSkillSource) DBHealth() mcp.DBHealth                  { return f.db }
func (f *fakeSkillSource) RoutingStrategy(string) mcp.StrategyInfo { return f.strategy }

// P1.2 ecosystem expansion — fakeSkillSource stub implementations.

func (f *fakeSkillSource) CapabilityEntries() []mcp.CapabilityEntry {
	out := []mcp.CapabilityEntry{}
	for _, p := range f.providers {
		for _, m := range p.Models {
			out = append(out, mcp.CapabilityEntry{
				ProviderID: p.ID,
				ModelID:    m,
				Capability: "tool_calling",
				Supported:  nil,
				Native:     false,
				Source:     "test_fake",
				Confidence: 0.0,
			})
		}
	}
	return out
}

func (f *fakeSkillSource) QuotaIntelligence(filter string) ([]mcp.QuotaIntelEntry, error) {
	out := []mcp.QuotaIntelEntry{}
	if f.quota.KeyID != "" {
		state := "healthy"
		if f.quota.AtLimit {
			state = "exhausted"
		} else if f.quota.WarningReached {
			state = "approaching_limit"
		}
		entry := mcp.QuotaIntelEntry{
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

func (f *fakeSkillSource) BudgetForecast(keyID string) (mcp.BudgetForecastEntry, error) {
	return mcp.BudgetForecastEntry{
		KeyID:            keyID,
		InsufficientData: true,
		UpdatedAt:        "2026-01-01T00:00:00Z",
	}, nil
}

func (f *fakeSkillSource) CompressionStats() mcp.CompressionStatsEntry {
	return mcp.CompressionStatsEntry{Layers: 0, LastUpdated: "2026-01-01T00:00:00Z"}
}

func (f *fakeSkillSource) EvalSchedules() ([]mcp.EvalScheduleEntry, error) {
	return []mcp.EvalScheduleEntry{}, nil
}

func (f *fakeSkillSource) OrgReport() mcp.OrgReportEntry {
	return mcp.OrgReportEntry{UpdatedAt: "2026-01-01T00:00:00Z"}
}

// fakeInvalidInputError stands in for mcp.ErrInvalidInput
// wrapping: errors.Is(err, mcp.ErrInvalidInput) must hold, the
// way the production source wraps it.
type fakeInvalidInputError struct{ msg string }

func (e *fakeInvalidInputError) Error() string { return "mcp: invalid input: " + e.msg }
func (e *fakeInvalidInputError) Unwrap() error { return mcp.ErrInvalidInput }

type fakeLedgerError struct{}

func (e *fakeLedgerError) Error() string { return "ledger unavailable: no ledger yet (test fake)" }

// transportSkillSource is a fakeSkillSource that additionally
// implements TransportSource, for the list-capabilities
// transports path.
type transportSkillSource struct {
	*fakeSkillSource
	transports []string
}

func (t *transportSkillSource) Transports() []string { return t.transports }

// keyScopedSkillSource is a fakeSkillSource that additionally
// implements KeyScopedComboMetricsSource, with PER-KEY metrics:
// keyMetrics[keyID][comboName]. Two keys get deliberately
// different measured histories for the SAME combo (key-1:
// 3 requests, $0.01; key-2: 99 requests, $9.99), so a test can
// prove a gateway-key caller sees only its own key's numbers —
// and, on the pre-change code, that the caller instead got the
// fake's ledger-wide metrics (10 requests, $0.05), which match
// NEITHER key. It deliberately does NOT override ComboMetrics:
// the ledger-wide method still returns the fake's global
// numbers, which is exactly what the old handler served to
// every caller.
type keyScopedSkillSource struct {
	*fakeSkillSource
	keyMetrics map[string]map[string]mcp.ComboMetrics
}

func (s *keyScopedSkillSource) ComboMetricsForKey(keyID, name string) (mcp.ComboMetrics, error) {
	if byKey, ok := s.keyMetrics[keyID]; ok {
		if m, ok := byKey[name]; ok {
			return m, nil
		}
	}
	return mcp.ComboMetrics{}, &fakeInvalidInputError{msg: "no metrics for key " + keyID + " combo " + name}
}

func newKeyScopedSkillSource() *keyScopedSkillSource {
	sr1, sr2 := 1.0, 0.0101
	return &keyScopedSkillSource{
		fakeSkillSource: newFakeSkillSource(),
		keyMetrics: map[string]map[string]mcp.ComboMetrics{
			"key-1": {
				"fast": {Name: "fast", Requests: 3, OK: 3, Errors: 0, SuccessRate: &sr1, PromptTokens: 300, CompletionTokens: 150, TotalTokens: 450, CostUSD: 0.01, CostKnown: true, MeanLatencyMS: 50, LatencyKnown: true, LatencyEvents: 3, Window: "all_time"},
			},
			"key-2": {
				"fast": {Name: "fast", Requests: 99, OK: 1, Errors: 98, SuccessRate: &sr2, PromptTokens: 9900, CompletionTokens: 4900, TotalTokens: 14800, CostUSD: 9.99, CostKnown: true, MeanLatencyMS: 9999, LatencyKnown: true, LatencyEvents: 1, Window: "all_time"},
			},
		},
	}
}

// metricsObject pulls the "metrics" object out of a
// smart-routing result for numeric assertions.
func metricsObject(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	m, ok := out["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("result has no metrics object: %v", out)
	}
	return m
}

// resultJSON re-marshals a decoded skill result so a test can
// assert a value is ABSENT from the wire output (a decoded map
// cannot prove absence of a field the handler never emitted,
// but re-marshalling the exact decoded content does: anything
// in the string was in the handler's JSON).
func resultJSON(t *testing.T, out map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(raw)
}

// --- helpers --------------------------------------------------------------

func invokeSkill(t *testing.T, ctx context.Context, src SkillSource, name string, args map[string]any) map[string]any {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	raw, err := InvokeSkill(ctx, src, name, rawArgs)
	if err != nil {
		t.Fatalf("InvokeSkill(%q) error: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("InvokeSkill(%q) returned unparsable JSON: %v", name, err)
	}
	return out
}

func invokeSkillErr(t *testing.T, ctx context.Context, src SkillSource, name string, args map[string]any) error {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	_, err = InvokeSkill(ctx, src, name, rawArgs)
	if err == nil {
		t.Fatalf("InvokeSkill(%q) succeeded, want an error", name)
	}
	return err
}

func comboName(t *testing.T, out map[string]any) string {
	t.Helper()
	combo, ok := out["combo"].(map[string]any)
	if !ok {
		t.Fatalf("result has no combo object: %v", out)
	}
	name, _ := combo["name"].(string)
	return name
}

func joinedReasons(t *testing.T, out map[string]any) string {
	t.Helper()
	raw, ok := out["reasons"].([]any)
	if !ok {
		t.Fatalf("result has no reasons list: %v", out)
	}
	parts := make([]string, 0, len(raw))
	for _, r := range raw {
		s, _ := r.(string)
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

func gatewayCtx(keyID string) context.Context {
	return WithCaller(context.Background(), Caller{Kind: CallerGatewayKey, KeyID: keyID})
}

func operatorCtx() context.Context {
	return WithCaller(context.Background(), Caller{Kind: CallerLocalOperator})
}

// --- descriptors + dispatch -------------------------------------------------

func TestSkillDescriptorsFixedOrder(t *testing.T) {
	want := []string{"smart-routing", "quota-management", "provider-discovery", "cost-analysis", "health-report", "list-capabilities",
		// P1.2 ecosystem expansion skills.
		"budget-forecasting", "cost-estimation", "route-preview", "compression-analytics", "eval-management", "org-reporting"}
	got := SkillDescriptors()
	if len(got) != len(want) {
		t.Fatalf("SkillDescriptors() = %d skills, want %d: %v", len(got), len(want), got)
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("SkillDescriptors()[%d].ID = %q, want %q", i, got[i].ID, id)
		}
		if got[i].Name == "" || got[i].Description == "" {
			t.Errorf("SkillDescriptors()[%d] has an empty name/description: %+v", i, got[i])
		}
		if len(got[i].Tags) == 0 {
			t.Errorf("SkillDescriptors()[%d] has no tags", i)
		}
	}
	// The returned slice is a copy: mutating it must not mutate
	// the registry a later caller sees.
	got[0].ID = "tampered"
	if SkillDescriptors()[0].ID != want[0] {
		t.Errorf("SkillDescriptors() is not a copy — mutating the result mutated the registry")
	}
}

func TestSkillRegistryMatchesDescriptors(t *testing.T) {
	// InvokeSkill's dispatch table and the descriptor list can
	// never drift apart: every described skill must be
	// invokable, and no handler may exist without a descriptor.
	descs := SkillDescriptors()
	if len(skillHandlers) != len(descs) {
		t.Fatalf("skillHandlers has %d entries, SkillDescriptors has %d", len(skillHandlers), len(descs))
	}
	for _, d := range descs {
		if _, ok := skillHandlers[d.ID]; !ok {
			t.Errorf("skill %q is described but has no handler", d.ID)
		}
	}
}

func TestInvokeSkillUnknown(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, context.Background(), src, "teleport", nil)
	if !strings.Contains(err.Error(), `"teleport"`) {
		t.Errorf("unknown-skill error = %q, want it to name the skill", err.Error())
	}
	err = invokeSkillErr(t, context.Background(), src, "", nil)
	if !strings.Contains(err.Error(), `""`) {
		t.Errorf("empty-skill error = %q, want it to name the skill", err.Error())
	}
}

func TestInvokeSkillNilSource(t *testing.T) {
	if _, err := InvokeSkill(context.Background(), nil, "health-report", nil); err == nil {
		t.Errorf("InvokeSkill(nil source) succeeded, want an error")
	}
}

// TestSkillSourceSatisfiedByMCP pins the adapter-free claim at
// compile time: any mcp.Source assigns to SkillSource, so the
// production sources (registryMCPSource, serverMCPSource) need
// no adapter to back the skills.
func TestSkillSourceSatisfiedByMCP(t *testing.T) {
	var mcpSrc mcp.Source
	var _ SkillSource = mcpSrc
}

// --- smart-routing ----------------------------------------------------------

func TestSmartRoutingPrefersFastest(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo = %q, want fast", got)
	}
	if out["preferred_strategy"] != "fastest" {
		t.Errorf("preferred_strategy = %v, want fastest", out["preferred_strategy"])
	}
	if out["metrics_known"] != true {
		t.Errorf("metrics_known = %v, want true", out["metrics_known"])
	}
	if out["candidates_seen"] != float64(2) {
		t.Errorf("candidates_seen = %v, want 2", out["candidates_seen"])
	}
	if !strings.Contains(joinedReasons(t, out), "strategy fastest matches") {
		t.Errorf("reasons = %q, want a strategy-match reason", joinedReasons(t, out))
	}
	// Removing the skill must break this test: the combo, the
	// strategy preference and the measured numbers all come from
	// the skill's ranking over the fake's inputs.
}

func TestSmartRoutingCostCapKeepsMeasured(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "coding", "max_cost_usd": 0.01})
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo under $0.01 cap = %q, want fast ($0.005/request)", got)
	}
	cons, _ := out["constraints"].(map[string]any)
	if cons["max_cost_usd_applied"] != true {
		t.Errorf("constraints = %v, want max_cost_usd_applied=true", cons)
	}
}

func TestSmartRoutingUnknownCostNotExcluded(t *testing.T) {
	src := newFakeSkillSource()
	// A cost cap below every measured cost excludes "fast";
	// "mixed" has no priced history, so its cost is unknown and
	// the constraint is reported not-applied — it is NOT
	// excluded.
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "coding", "max_cost_usd": 0.001})
	if got := comboName(t, out); got != "mixed" {
		t.Errorf("best combo under $0.001 cap = %q, want mixed (unknown cost, not excluded)", got)
	}
	cons, _ := out["constraints"].(map[string]any)
	if cons["max_cost_usd_applied"] != false {
		t.Errorf("constraints = %v, want max_cost_usd_applied=false for unknown-cost combo", cons)
	}
}

func TestSmartRoutingLatencyCap(t *testing.T) {
	src := newFakeSkillSource()
	// "fast" measures 120 ms, over the 100 ms cap, so it is
	// excluded; "mixed" has no measured latency, so the cap is
	// not applied to it and it wins.
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat", "max_latency_ms": 100})
	if got := comboName(t, out); got != "mixed" {
		t.Errorf("best combo under 100ms cap = %q, want mixed (fast is 120ms, mixed latency unknown)", got)
	}
	cons, _ := out["constraints"].(map[string]any)
	if cons["max_latency_ms_applied"] != false {
		t.Errorf("constraints = %v, want max_latency_ms_applied=false", cons)
	}
	// A cap the measured combo satisfies keeps it.
	out = invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat", "max_latency_ms": 200})
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo under 200ms cap = %q, want fast", got)
	}
	cons, _ = out["constraints"].(map[string]any)
	if cons["max_latency_ms_applied"] != true {
		t.Errorf("constraints = %v, want max_latency_ms_applied=true", cons)
	}
}

func TestSmartRoutingZeroCapMeansNoCap(t *testing.T) {
	src := newFakeSkillSource()
	// 0 is the documented no-cap sentinel, not a $0 / 0 ms cap:
	// "fast" measures $0.005/request and 120 ms, so a real zero
	// cap would exclude it and "mixed" would win. Both zero caps
	// together — and each alone — must behave exactly like no cap.
	for _, args := range []map[string]any{
		{"task": "coding", "max_cost_usd": 0, "max_latency_ms": 0},
		{"task": "coding", "max_cost_usd": 0},
		{"task": "coding", "max_latency_ms": 0},
	} {
		out := invokeSkill(t, operatorCtx(), src, "smart-routing", args)
		if got := comboName(t, out); got != "fast" {
			t.Errorf("best combo with zero caps %v = %q, want fast (0 = no cap, not a $0/0ms cap)", args, got)
		}
		cons, _ := out["constraints"].(map[string]any)
		if cons["max_cost_usd_applied"] != nil || cons["max_latency_ms_applied"] != nil {
			t.Errorf("constraints with zero caps %v = %v, want neither cap applied", args, cons)
		}
	}
}

func TestSmartRoutingReasonsIndependentAcrossCalls(t *testing.T) {
	src := newFakeSkillSource()
	src.ledgerDown = true
	// skillSmartRouting appends the no-history note to the best
	// combo's reasons; that append must go to a fresh copy, never
	// into the candidate's backing array. The aliasing was latent
	// (candidates are rebuilt per call and never retained), so the
	// observable invariant is pinned instead: repeated calls
	// return identical, independent reasons, the appended note
	// exactly once each, and mutating one call's decoded reasons
	// cannot change the next call's.
	out1 := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	out2 := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	r1, r2 := joinedReasons(t, out1), joinedReasons(t, out2)
	if r1 != r2 {
		t.Errorf("reasons differ across calls:\nfirst:  %q\nsecond: %q", r1, r2)
	}
	if n := strings.Count(r1, "ranked on strategy match alone"); n != 1 {
		t.Errorf("no-history note appears %d times in %q, want exactly once", n, r1)
	}
	// Mutate the first call's decoded reasons, then call again:
	// the next call must be unaffected.
	raw1, _ := out1["reasons"].([]any)
	out1["reasons"] = append(raw1, "tampered")
	out3 := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	if got := joinedReasons(t, out3); got != r1 {
		t.Errorf("reasons after mutating a prior result = %q, want %q", got, r1)
	}
}

func TestSmartRoutingUnpricedIsLowerBound(t *testing.T) {
	src := newFakeSkillSource()
	sr := 0.75
	src.combos["cheap"] = mcp.ComboInfo{Name: "cheap", Members: []string{"p1"}, Strategy: "cheapest", Usable: true}
	src.metrics["cheap"] = mcp.ComboMetrics{Name: "cheap", Requests: 4, OK: 3, SuccessRate: &sr, CostUSD: 0.02, CostKnown: false, UnpricedTokens: 50, Window: "all_time"}
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "cheapest summarisation"})
	if got := comboName(t, out); got != "cheap" {
		t.Errorf("best combo = %q, want cheap (strategy cheapest matches)", got)
	}
	if !strings.Contains(joinedReasons(t, out), "is a lower bound") {
		t.Errorf("reasons = %q, want the unpriced lower-bound note, never $0", joinedReasons(t, out))
	}
}

func TestSmartRoutingLedgerDownRanksOnStrategyAlone(t *testing.T) {
	src := newFakeSkillSource()
	src.ledgerDown = true
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo with ledger down = %q, want fast (strategy match alone)", got)
	}
	if out["metrics_known"] != false {
		t.Errorf("metrics_known = %v, want false", out["metrics_known"])
	}
	if !strings.Contains(joinedReasons(t, out), "ranked on strategy match alone") {
		t.Errorf("reasons = %q, want the no-history note", joinedReasons(t, out))
	}
}

func TestSmartRoutingErrors(t *testing.T) {
	src := newFakeSkillSource()
	if err := invokeSkillErr(t, operatorCtx(), src, "smart-routing", nil); !strings.Contains(err.Error(), "task: required") {
		t.Errorf("missing task error = %q, want task: required", err.Error())
	}
	if err := invokeSkillErr(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "x", "max_cost_usd": -1}); !strings.Contains(err.Error(), "max_cost_usd: must be >= 0") {
		t.Errorf("negative cost cap error = %q", err.Error())
	}
	if err := invokeSkillErr(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "x", "max_latency_ms": "soon"}); !strings.Contains(err.Error(), "max_latency_ms: must be a number") {
		t.Errorf("bad latency cap error = %q", err.Error())
	}
	empty := newFakeSkillSource()
	empty.combos = map[string]mcp.ComboInfo{}
	if err := invokeSkillErr(t, operatorCtx(), empty, "smart-routing", map[string]any{"task": "x"}); !strings.Contains(err.Error(), "no combos configured") {
		t.Errorf("no-combos error = %q", err.Error())
	}
	// A cost cap every MEASURABLE combo fails excludes
	// everything: with only "fast" configured (measured
	// $0.005/request), a $0.0001 cap leaves no candidate. (A
	// combo with unknown cost is never excluded — see
	// TestSmartRoutingUnknownCostNotExcluded — so this case
	// needs a fixture without one.)
	onlyFast := newFakeSkillSource()
	onlyFast.combos = map[string]mcp.ComboInfo{
		"fast": {Name: "fast", Members: []string{"p1"}, Strategy: "fastest", Usable: true},
	}
	if err := invokeSkillErr(t, operatorCtx(), onlyFast, "smart-routing", map[string]any{"task": "fast chat", "max_cost_usd": 0.0001}); !strings.Contains(err.Error(), "no combo satisfies") {
		t.Errorf("all-excluded error = %q", err.Error())
	}
}

func TestTaskPreferredStrategyKeywords(t *testing.T) {
	cases := map[string]string{
		"debug this crash":       "code",
		"write a program":        "code",
		"quick realtime answer":  "fastest",
		"cheapest way to do it":  "cheapest",
		"most reliable provider": "most-reliable",
		"hello there":            "",
	}
	for task, want := range cases {
		if got := taskPreferredStrategy(task); got != want {
			t.Errorf("taskPreferredStrategy(%q) = %q, want %q", task, got, want)
		}
	}
}

// --- quota-management ---------------------------------------------------------

func TestQuotaManagementOwnKey(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "quota-management", nil)
	if src.quotaCalls != 1 || src.lastQuotaKey != "key-1" {
		t.Errorf("CheckQuota called %d times with key %q, want 1 call with key-1", src.quotaCalls, src.lastQuotaKey)
	}
	quota, ok := out["quota"].(map[string]any)
	if !ok {
		t.Fatalf("result has no quota object: %v", out)
	}
	if quota["key_id"] != "key-1" || quota["limit_usd"] != float64(10) || quota["spent_usd"] != float64(2.5) {
		t.Errorf("quota = %v, want key-1 limit 10 spent 2.5", quota)
	}
	if quota["has_budget"] != true {
		t.Errorf("has_budget = %v, want true", quota["has_budget"])
	}
}

func TestQuotaManagementForeignKeyRejected(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "quota-management", map[string]any{"key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) || !strings.Contains(err.Error(), `"key-1"`) {
		t.Errorf("foreign-key error = %q, want it to name both the supplied and the caller key", err.Error())
	}
	if src.quotaCalls != 0 {
		t.Errorf("CheckQuota was called %d times after a rejected foreign key — the ledger must never be read for another key", src.quotaCalls)
	}
}

func TestQuotaManagementLocalOperatorNamesAnyKey(t *testing.T) {
	src := newFakeSkillSource()
	invokeSkill(t, operatorCtx(), src, "quota-management", map[string]any{"key_id": "key-9"})
	if src.lastQuotaKey != "key-9" {
		t.Errorf("local operator key resolved to %q, want key-9", src.lastQuotaKey)
	}
}

func TestQuotaManagementNoCallerFailsClosed(t *testing.T) {
	src := newFakeSkillSource()
	// No WithCaller on the context: the skill must refuse, not
	// guess whose key it reads.
	err := invokeSkillErr(t, context.Background(), src, "quota-management", nil)
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q, want the caller-identity refusal", err.Error())
	}
	if src.quotaCalls != 0 {
		t.Errorf("CheckQuota was called without a caller — fail-closed violated")
	}
}

func TestQuotaManagementMissingKeyID(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, operatorCtx(), src, "quota-management", nil)
	if !strings.Contains(err.Error(), "key_id: required") {
		t.Errorf("missing-key error = %q, want key_id: required", err.Error())
	}
}

func TestQuotaManagementLedgerDown(t *testing.T) {
	src := newFakeSkillSource()
	src.ledgerDown = true
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "quota-management", nil)
	if !strings.Contains(err.Error(), "unavailable:") {
		t.Errorf("ledger-down error = %q, want the unavailable: prefix", err.Error())
	}
}

// --- provider-discovery -------------------------------------------------------

func TestProviderDiscovery(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "provider-discovery", nil)
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	if out["count"] != float64(2) {
		t.Errorf("count = %v, want 2", out["count"])
	}
	providers, ok := out["providers"].([]any)
	if !ok || len(providers) != 2 {
		t.Fatalf("providers = %v, want 2 entries", out["providers"])
	}
	first, _ := providers[0].(map[string]any)
	if first["id"] != "p1" || first["enabled"] != true {
		t.Errorf("first provider = %v, want p1 enabled", first)
	}
	models, _ := first["models"].([]any)
	if len(models) != 1 || models[0] != "coder-model" {
		t.Errorf("p1 models = %v, want [coder-model]", models)
	}
	second, _ := providers[1].(map[string]any)
	if second["id"] != "p2" || second["enabled"] != false {
		t.Errorf("second provider = %v, want p2 disabled", second)
	}
	if _, has := second["catalog_id"]; has {
		t.Errorf("second provider = %v, want no catalog_id (omitempty) for p2", second)
	}
	// No base URL or key anywhere in the output: ProviderInfo
	// carries neither, and the skill must not add them.
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "base_url") || strings.Contains(string(raw), "api_key") {
		t.Errorf("provider output leaks a base URL or key: %s", raw)
	}
}

func TestProviderDiscoveryFilters(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "provider-discovery", map[string]any{"enabled_only": true})
	if out["count"] != float64(1) {
		t.Errorf("enabled_only count = %v, want 1", out["count"])
	}
	out = invokeSkill(t, operatorCtx(), src, "provider-discovery", map[string]any{"query": "chatter"})
	if out["count"] != float64(1) {
		t.Errorf("query=chatter count = %v, want 1", out["count"])
	}
	providers, _ := out["providers"].([]any)
	first, _ := providers[0].(map[string]any)
	if first["id"] != "p2" {
		t.Errorf("query=chatter matched %v, want p2 (name match)", first["id"])
	}
	out = invokeSkill(t, operatorCtx(), src, "provider-discovery", map[string]any{"query": "P1"})
	if out["count"] != float64(1) {
		t.Errorf("query=P1 count = %v, want 1 (id match, case-insensitive)", out["count"])
	}
	out = invokeSkill(t, operatorCtx(), src, "provider-discovery", map[string]any{"query": "zzz"})
	if out["count"] != float64(0) {
		t.Errorf("query=zzz count = %v, want 0", out["count"])
	}
	if _, ok := out["providers"].([]any); !ok {
		t.Errorf("empty providers must still be a JSON array, got %v", out["providers"])
	}
}

// --- cost-analysis ------------------------------------------------------------

func TestCostAnalysis(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "cost-analysis", map[string]any{"days": 7})
	if src.costCalls != 1 || src.lastCostKey != "key-1" || src.lastCostDays != 7 {
		t.Errorf("CostReport called %d times with key %q days %d, want 1 call with key-1 days 7", src.costCalls, src.lastCostKey, src.lastCostDays)
	}
	rep, ok := out["cost_report"].(map[string]any)
	if !ok {
		t.Fatalf("result has no cost_report object: %v", out)
	}
	// Same numbers cost_report returns: the skill passes the
	// ledger report through, it does not recompute it.
	if rep["requests"] != float64(5) || rep["cost_usd"] != float64(0.12) || rep["cost_known"] != true {
		t.Errorf("cost_report = %v, want requests 5 cost $0.12 cost_known true", rep)
	}
}

func TestCostAnalysisKeyRule(t *testing.T) {
	src := newFakeSkillSource()
	// Foreign key rejected, ledger untouched.
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "cost-analysis", map[string]any{"key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) {
		t.Errorf("foreign-key error = %q", err.Error())
	}
	if src.costCalls != 0 {
		t.Errorf("CostReport was called after a rejected foreign key")
	}
	// Local operator omitting key_id reports on ALL keys ("").
	invokeSkill(t, operatorCtx(), src, "cost-analysis", map[string]any{"days": 30})
	if src.lastCostKey != "" || src.lastCostDays != 30 {
		t.Errorf("operator all-keys call reached the source as key %q days %d, want key \"\" days 30", src.lastCostKey, src.lastCostDays)
	}
	// No caller fails closed.
	if err := invokeSkillErr(t, context.Background(), src, "cost-analysis", nil); !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q", err.Error())
	}
}

func TestCostAnalysisDaysValidation(t *testing.T) {
	src := newFakeSkillSource()
	if err := invokeSkillErr(t, gatewayCtx("key-1"), src, "cost-analysis", map[string]any{"days": 366}); !strings.Contains(err.Error(), "days: must be between 0 (all time) and 365") {
		t.Errorf("days=366 error = %q", err.Error())
	}
	if err := invokeSkillErr(t, gatewayCtx("key-1"), src, "cost-analysis", map[string]any{"days": 1.5}); !strings.Contains(err.Error(), "days: must be an integer") {
		t.Errorf("days=1.5 error = %q", err.Error())
	}
}

// --- health-report --------------------------------------------------------------

func TestHealthReport(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "health-report", nil)
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	if out["status"] != "ok" {
		t.Errorf("status = %v, want ok", out["status"])
	}
	router, ok := out["router"].(map[string]any)
	if !ok || router["version"] != "0.1.0" || router["providers"] != float64(2) {
		t.Errorf("router = %v, want version 0.1.0 with 2 providers", out["router"])
	}
	providers, ok := out["providers"].(map[string]any)
	if !ok || providers["total"] != float64(2) || providers["enabled"] != float64(1) {
		t.Errorf("providers = %v, want total 2 enabled 1", out["providers"])
	}
	disabled, _ := providers["disabled"].([]any)
	if len(disabled) != 1 || disabled[0] != "p2" {
		t.Errorf("disabled = %v, want [p2]", disabled)
	}
	cache, ok := out["cache"].(map[string]any)
	if !ok {
		t.Fatalf("result has no cache object: %v", out)
	}
	exact, _ := cache["exact"].(map[string]any)
	if exact["present"] != true || exact["entries"] != float64(3) {
		t.Errorf("cache.exact = %v, want present with 3 entries", exact)
	}
	semantic, _ := cache["semantic"].(map[string]any)
	if semantic["present"] != false {
		t.Errorf("cache.semantic = %v, want present=false (unseen layer, never zero-filled)", semantic)
	}
	dbs, ok := out["databases"].(map[string]any)
	if !ok || dbs["healthy"] != true {
		t.Errorf("databases = %v, want healthy true", out["databases"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "no liveness probe") {
		t.Errorf("note = %q, want the honesty note about probes", note)
	}
}

func TestHealthReportDegraded(t *testing.T) {
	src := newFakeSkillSource()
	src.db.Healthy = false
	src.db.Files = []mcp.DBFileHealth{{Name: "ledger.db", Exists: true, Error: "cannot open"}}
	out := invokeSkill(t, operatorCtx(), src, "health-report", nil)
	if out["status"] != "degraded" {
		t.Errorf("status = %v, want degraded when a database is unhealthy", out["status"])
	}
}

// --- list-capabilities ------------------------------------------------------------

// TestListCapabilitiesLiveRegistries is the REAL-source test for
// this skill: the strategies and MCP tools it reports are
// asserted against the LIVE strategy.All registry and the LIVE
// mcp.Tools() registry — real production code, no fake involved.
// If either registry gains or loses an entry, this test fails
// until the skill (which reads the same registries) agrees.
func TestListCapabilitiesLiveRegistries(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "list-capabilities", nil)

	strategies, ok := out["strategies"].([]any)
	if !ok {
		t.Fatalf("result has no strategies list: %v", out)
	}
	if len(strategies) != len(strategy.All) {
		t.Fatalf("strategies = %d, want %d (live strategy.All)", len(strategies), len(strategy.All))
	}
	for i, st := range strategy.All {
		entry, _ := strategies[i].(map[string]any)
		if entry["id"] != string(st) {
			t.Errorf("strategies[%d].id = %v, want %q (live registry order)", i, entry["id"], string(st))
		}
		name, rule := strategy.Describe(st)
		if entry["name"] != name || entry["rule"] != rule {
			t.Errorf("strategies[%d] = %v, want name %q rule %q from strategy.Describe", i, entry, name, rule)
		}
	}

	tools, ok := out["mcp_tools"].(map[string]any)
	if !ok {
		t.Fatalf("result has no mcp_tools object: %v", out)
	}
	live := mcp.Tools()
	if tools["count"] != float64(len(live)) {
		t.Errorf("mcp_tools.count = %v, want %d (live mcp.Tools())", tools["count"], len(live))
	}
	names, _ := tools["names"].([]any)
	if len(names) != len(live) {
		t.Fatalf("mcp_tools.names = %d, want %d", len(names), len(live))
	}
	for i, tool := range live {
		if names[i] != tool.Name {
			t.Errorf("mcp_tools.names[%d] = %v, want %q (live registry order)", i, names[i], tool.Name)
		}
	}

	router, ok := out["router"].(map[string]any)
	if !ok || router["version"] != "0.1.0" || router["auto_router"] != true {
		t.Errorf("router = %v, want version 0.1.0 auto_router true", out["router"])
	}
	// The local operator sees the full router facts, including
	// the gateway_keys count — the gateway-key shape below
	// (TestListCapabilitiesGatewayKeyNoRouterFacts) must NOT.
	if router["gateway_keys"] != float64(1) {
		t.Errorf("router.gateway_keys = %v, want 1 for the local operator", router["gateway_keys"])
	}
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	global, _ := router["global_strategy"].(map[string]any)
	if global["name"] != "priority" {
		t.Errorf("global_strategy = %v, want priority (from the fake's RoutingStrategy)", global)
	}
	skills, ok := out["skills"].([]any)
	if !ok || len(skills) != 12 {
		t.Fatalf("skills = %v, want 12 descriptors (6 baseline + 6 P1.2)", out["skills"])
	}
	first, _ := skills[0].(map[string]any)
	if first["id"] != "smart-routing" {
		t.Errorf("skills[0].id = %v, want smart-routing", first["id"])
	}
	// No source implements TransportSource here: the field must
	// be omitted, with a note saying why — never a hardcoded
	// list.
	if _, has := out["transports"]; has {
		t.Errorf("transports present without a TransportSource: %v", out["transports"])
	}
	note, _ := out["note"].(string)
	if !strings.Contains(note, "does not expose transport state") {
		t.Errorf("note = %q, want the transports-omitted note", note)
	}
}

func TestListCapabilitiesTransports(t *testing.T) {
	src := &transportSkillSource{fakeSkillSource: newFakeSkillSource(), transports: []string{"a2a", "http-gateway"}}
	out := invokeSkill(t, operatorCtx(), src, "list-capabilities", nil)
	transports, ok := out["transports"].([]any)
	if !ok || len(transports) != 2 || transports[0] != "a2a" || transports[1] != "http-gateway" {
		t.Errorf("transports = %v, want [a2a http-gateway] from the TransportSource", out["transports"])
	}
	if _, has := out["note"]; has {
		t.Errorf("note present when transports were reported: %q", out["note"])
	}
}

// --- Integration-B item C: caller scoping ------------------------------
//
// Bug-hunt finding 10: both handlers discarded the caller and
// served ledger-wide / global data to any caller. These tests
// pin the per-caller shapes. Each one FAILS on the pre-change
// code (verified with the pristine copy — see the negative
// control in the task report): the old handlers took `_
// context.Context` and returned the global numbers to a
// gateway-key caller, and succeeded with no caller at all.

// --- smart-routing scoping ------------------------------------------------

func TestSmartRoutingGatewayKeyScopedMetrics(t *testing.T) {
	src := newKeyScopedSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "smart-routing", map[string]any{"task": "fast chat"})
	if out["scope"] != "gateway-key" || out["scoped_to_key"] != "key-1" {
		t.Errorf("scope = %v scoped_to_key = %v, want gateway-key / key-1", out["scope"], out["scoped_to_key"])
	}
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo = %q, want fast", got)
	}
	// The caller's OWN numbers: 3 requests, $0.01 — not the
	// fake's ledger-wide 10 requests / $0.05, and never key-2's
	// 99 requests / $9.99.
	m := metricsObject(t, out)
	if m["requests"] != float64(3) || m["cost_usd"] != float64(0.01) {
		t.Errorf("metrics = %v, want key-1's own 3 requests $0.01", m)
	}
	if m["mean_latency_ms"] != float64(50) {
		t.Errorf("metrics mean_latency_ms = %v, want key-1's own 50ms", m["mean_latency_ms"])
	}
	if !strings.Contains(joinedReasons(t, out), "over 3 requests") {
		t.Errorf("reasons = %q, want key-1's own measured history", joinedReasons(t, out))
	}
	raw := resultJSON(t, out)
	if strings.Contains(raw, "9.99") || strings.Contains(raw, `"requests":99`) {
		t.Errorf("result leaks key-2's metrics: %s", raw)
	}
	if strings.Contains(raw, `"cost_usd":0.05`) || strings.Contains(raw, `"requests":10`) {
		t.Errorf("result leaks the ledger-wide metrics (10 requests, $0.05): %s", raw)
	}
	note, _ := out["scope_note"].(string)
	if !strings.Contains(note, "scoped to the caller's key") {
		t.Errorf("scope_note = %q, want the per-key scoping note", note)
	}
}

func TestSmartRoutingGatewayKeyOmitsLedgerWideMetrics(t *testing.T) {
	// Plain fakeSkillSource: no KeyScopedComboMetricsSource.
	// The ledger-wide numbers (fast: 10 requests, $0.05, 120
	// ms, 90% success) must NOT reach a gateway-key caller —
	// they are other keys' usage. The skill still recommends
	// (strategy match alone) and says so.
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "smart-routing", map[string]any{"task": "fast chat"})
	if out["scope"] != "gateway-key" || out["scoped_to_key"] != "key-1" {
		t.Errorf("scope = %v scoped_to_key = %v, want gateway-key / key-1", out["scope"], out["scoped_to_key"])
	}
	if got := comboName(t, out); got != "fast" {
		t.Errorf("best combo = %q, want fast (strategy match alone)", got)
	}
	if out["metrics_known"] != false {
		t.Errorf("metrics_known = %v, want false — ledger-wide metrics must be omitted, not served", out["metrics_known"])
	}
	m := metricsObject(t, out)
	if m["requests"] != float64(0) || m["cost_usd"] != float64(0) {
		t.Errorf("metrics = %v, want zero metrics (omitted), never the ledger-wide numbers", m)
	}
	if !strings.Contains(joinedReasons(t, out), "ranked on strategy match alone") {
		t.Errorf("reasons = %q, want the strategy-only note", joinedReasons(t, out))
	}
	note, _ := out["scope_note"].(string)
	if !strings.Contains(note, "omitted") || !strings.Contains(note, "KeyScopedComboMetricsSource") {
		t.Errorf("scope_note = %q, want the omission note naming the per-key source", note)
	}
	raw := resultJSON(t, out)
	for _, leaked := range []string{`"cost_usd":0.05`, `"requests":10`, `"mean_latency_ms":120`} {
		if strings.Contains(raw, leaked) {
			t.Errorf("result leaks a ledger-wide metric %s: %s", leaked, raw)
		}
	}
}

func TestSmartRoutingGatewayKeyOwnKeyIDArgAccepted(t *testing.T) {
	src := newKeyScopedSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "smart-routing", map[string]any{"task": "fast chat", "key_id": "key-1"})
	if metricsObject(t, out)["requests"] != float64(3) {
		t.Errorf("metrics = %v, want key-1's own metrics when the caller names its own key", metricsObject(t, out))
	}
}

func TestSmartRoutingGatewayForeignKeyRejected(t *testing.T) {
	src := newKeyScopedSkillSource()
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "smart-routing", map[string]any{"task": "fast chat", "key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) || !strings.Contains(err.Error(), `"key-1"`) {
		t.Errorf("foreign-key error = %q, want it to name both the supplied and the caller key", err.Error())
	}
}

func TestSmartRoutingNoCallerFailsClosed(t *testing.T) {
	src := newFakeSkillSource()
	// No WithCaller on the context: same fail-closed as
	// quota-management — the skill must refuse, not serve
	// ledger-wide metrics to an unauthenticated caller.
	err := invokeSkillErr(t, context.Background(), src, "smart-routing", map[string]any{"task": "fast chat"})
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q, want the caller-identity refusal", err.Error())
	}
}

func TestSmartRoutingLocalOperatorKeepsFullSet(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "smart-routing", map[string]any{"task": "fast chat"})
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	if _, has := out["scoped_to_key"]; has {
		t.Errorf("scoped_to_key present for the local operator: %v", out["scoped_to_key"])
	}
	// Full ledger-wide set, exactly as before the scoping
	// change: 10 requests, $0.05, 120 ms.
	m := metricsObject(t, out)
	if out["metrics_known"] != true || m["requests"] != float64(10) || m["cost_usd"] != float64(0.05) || m["mean_latency_ms"] != float64(120) {
		t.Errorf("metrics = %v metrics_known = %v, want the full ledger-wide 10 requests $0.05 120ms for the operator", m, out["metrics_known"])
	}
}

// --- list-capabilities scoping --------------------------------------------

func TestListCapabilitiesGatewayKeyNoRouterFacts(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "list-capabilities", nil)
	if out["scope"] != "gateway-key" || out["scoped_to_key"] != "key-1" {
		t.Errorf("scope = %v scoped_to_key = %v, want gateway-key / key-1", out["scope"], out["scoped_to_key"])
	}
	// No "router" object at all — no gateway_keys count, no
	// listen address, no global strategy, no cross-key facts.
	if _, has := out["router"]; has {
		t.Errorf("router facts present for a gateway-key caller: %v", out["router"])
	}
	raw := resultJSON(t, out)
	// Assert on the JSON KEY form (with colon) and on the
	// values: the scope_note and the skill descriptions
	// legitimately NAME the omitted fields in prose ("no
	// gateway_keys count"), which discloses no value — but no
	// router fact may appear as data.
	for _, leaked := range []string{`"gateway_keys":`, `"global_strategy":`, `"listen":`, `"auto_router":`, `"127.0.0.1"`, `"version":`} {
		if strings.Contains(raw, leaked) {
			t.Errorf("result leaks router fact %q: %s", leaked, raw)
		}
	}
	// What the caller MAY know is still there: its own skills,
	// the strategies, and the MCP tool names (public code
	// registries, no other key's data).
	skills, ok := out["skills"].([]any)
	if !ok || len(skills) != 12 {
		t.Fatalf("skills = %v, want 12 descriptors (6 baseline + 6 P1.2)", out["skills"])
	}
	strategies, ok := out["strategies"].([]any)
	if !ok || len(strategies) != len(strategy.All) {
		t.Fatalf("strategies = %d, want %d (live strategy.All)", len(strategies), len(strategy.All))
	}
	tools, ok := out["mcp_tools"].(map[string]any)
	if !ok || tools["count"] != float64(len(mcp.Tools())) {
		t.Errorf("mcp_tools = %v, want the live mcp.Tools() count %d", out["mcp_tools"], len(mcp.Tools()))
	}
	note, _ := out["scope_note"].(string)
	if !strings.Contains(note, "omitted") || !strings.Contains(note, "gateway_keys") {
		t.Errorf("scope_note = %q, want the router-facts-omitted note", note)
	}
}

func TestListCapabilitiesGatewayForeignKeyRejected(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "list-capabilities", map[string]any{"key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) || !strings.Contains(err.Error(), `"key-1"`) {
		t.Errorf("foreign-key error = %q, want it to name both the supplied and the caller key", err.Error())
	}
}

func TestListCapabilitiesNoCallerFailsClosed(t *testing.T) {
	src := newFakeSkillSource()
	// No WithCaller on the context: same fail-closed as
	// quota-management — the skill must refuse, not serve the
	// global capability set to an unauthenticated caller.
	err := invokeSkillErr(t, context.Background(), src, "list-capabilities", nil)
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q, want the caller-identity refusal", err.Error())
	}
}

func TestListCapabilitiesLocalOperatorKeepsFullSet(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "list-capabilities", nil)
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	router, ok := out["router"].(map[string]any)
	if !ok {
		t.Fatalf("result has no router object for the local operator: %v", out)
	}
	if router["gateway_keys"] != float64(1) || router["version"] != "0.1.0" || router["listen"] != "127.0.0.1:20128" {
		t.Errorf("router = %v, want the full facts (gateway_keys 1, version 0.1.0) for the operator", router)
	}
	global, _ := router["global_strategy"].(map[string]any)
	if global["name"] != "priority" {
		t.Errorf("global_strategy = %v, want priority for the operator", global)
	}
}

// --- Integration-B item D: caller scoping ------------------------------
//
// Bug-hunt finding 10 (same class as item C): provider-discovery
// and health-report discarded the caller and served global
// state to any caller — health-report returned the full
// Status() (version, listen address, gateway_keys count) with
// no identity check at all. These tests pin the per-caller
// shapes. Each one FAILS on the pre-change code (verified with
// the negative control — see the task report): the old handlers
// took `_ context.Context`, succeeded with no caller at all,
// and returned the global numbers to a gateway-key caller.

func TestProviderDiscoveryGatewayKeyScoped(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "provider-discovery", nil)
	if out["scope"] != "gateway-key" || out["scoped_to_key"] != "key-1" {
		t.Errorf("scope = %v scoped_to_key = %v, want gateway-key / key-1", out["scope"], out["scoped_to_key"])
	}
	// Providers are shared operator config, so the gateway
	// caller still gets the full (filtered) list — the same
	// list the operator gets.
	if out["count"] != float64(2) {
		t.Errorf("count = %v, want 2 (shared config, not per-key data)", out["count"])
	}
	providers, ok := out["providers"].([]any)
	if !ok || len(providers) != 2 {
		t.Fatalf("providers = %v, want 2 entries", out["providers"])
	}
	first, _ := providers[0].(map[string]any)
	if first["id"] != "p1" || first["enabled"] != true {
		t.Errorf("first provider = %v, want p1 enabled", first)
	}
	// No router fact anywhere in the result: no version, no
	// listen address, no gateway_keys count.
	raw := resultJSON(t, out)
	for _, leaked := range []string{`"gateway_keys":`, `"version":`, `"listen":`, `"127.0.0.1"`, `"0.1.0"`} {
		if strings.Contains(raw, leaked) {
			t.Errorf("result leaks router fact %q: %s", leaked, raw)
		}
	}
	note, _ := out["scope_note"].(string)
	if !strings.Contains(note, "shared operator configuration") || !strings.Contains(note, "gateway_keys") {
		t.Errorf("scope_note = %q, want the shared-config note naming the excluded router facts", note)
	}
}

func TestProviderDiscoveryGatewayKeyOwnKeyIDArgAccepted(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "provider-discovery", map[string]any{"key_id": "key-1"})
	if out["count"] != float64(2) || out["scoped_to_key"] != "key-1" {
		t.Errorf("result = %v, want the caller's own key accepted with count 2", out)
	}
}

func TestProviderDiscoveryGatewayForeignKeyRejected(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "provider-discovery", map[string]any{"key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) || !strings.Contains(err.Error(), `"key-1"`) {
		t.Errorf("foreign-key error = %q, want it to name both the supplied and the caller key", err.Error())
	}
}

func TestProviderDiscoveryNoCallerFailsClosed(t *testing.T) {
	src := newFakeSkillSource()
	// No WithCaller on the context: same fail-closed as the
	// other scoped skills — the skill must refuse, not serve
	// the registry to an unauthenticated caller.
	err := invokeSkillErr(t, context.Background(), src, "provider-discovery", nil)
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q, want the caller-identity refusal", err.Error())
	}
}

func TestProviderDiscoveryLocalOperatorKeepsFullSet(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "provider-discovery", nil)
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	if _, has := out["scoped_to_key"]; has {
		t.Errorf("scoped_to_key present for the local operator: %v", out["scoped_to_key"])
	}
	if _, has := out["scope_note"]; has {
		t.Errorf("scope_note present for the local operator: %v", out["scope_note"])
	}
	if out["count"] != float64(2) {
		t.Errorf("count = %v, want 2 for the operator", out["count"])
	}
}

// --- health-report scoping ------------------------------------------------

func TestHealthReportGatewayKeyScoped(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, gatewayCtx("key-1"), src, "health-report", nil)
	if out["scope"] != "gateway-key" || out["scoped_to_key"] != "key-1" {
		t.Errorf("scope = %v scoped_to_key = %v, want gateway-key / key-1", out["scope"], out["scoped_to_key"])
	}
	// No "router" object at all — no version, listen address,
	// gateway_keys count, provider/model counts, auto-router
	// flag, or uptime.
	if _, has := out["router"]; has {
		t.Errorf("router facts present for a gateway-key caller: %v", out["router"])
	}
	raw := resultJSON(t, out)
	// Assert on the JSON KEY form (with colon) and on the
	// values: the scope_note legitimately NAMES the omitted
	// fields in prose ("gateway_keys count", "cache
	// entries/bytes counters", "row counts"), which discloses
	// no value — but no router fact or aggregate counter may
	// appear as data.
	for _, leaked := range []string{`"gateway_keys":`, `"version":`, `"listen":`, `"auto_router":`, `"127.0.0.1"`, `"0.1.0"`, `"entries":`, `"bytes":`, `"rows"`, `"modified_at":`} {
		if strings.Contains(raw, leaked) {
			t.Errorf("result leaks %q: %s", leaked, raw)
		}
	}
	// What the caller MAY know is still there: the verdict,
	// the provider configuration, cache presence, and database
	// health without row counts.
	if out["status"] != "ok" {
		t.Errorf("status = %v, want ok", out["status"])
	}
	providers, ok := out["providers"].(map[string]any)
	if !ok || providers["total"] != float64(2) || providers["enabled"] != float64(1) {
		t.Errorf("providers = %v, want total 2 enabled 1 (shared config)", out["providers"])
	}
	disabled, _ := providers["disabled"].([]any)
	if len(disabled) != 1 || disabled[0] != "p2" {
		t.Errorf("disabled = %v, want [p2]", disabled)
	}
	cache, ok := out["cache"].(map[string]any)
	if !ok {
		t.Fatalf("result has no cache object: %v", out)
	}
	exact, _ := cache["exact"].(map[string]any)
	if exact["present"] != true {
		t.Errorf("cache.exact = %v, want present=true (presence only, no counters)", exact)
	}
	if _, has := exact["entries"]; has {
		t.Errorf("cache.exact carries entries for a gateway-key caller: %v", exact)
	}
	semantic, _ := cache["semantic"].(map[string]any)
	if semantic["present"] != false {
		t.Errorf("cache.semantic = %v, want present=false", semantic)
	}
	dbs, ok := out["databases"].(map[string]any)
	if !ok || dbs["healthy"] != true {
		t.Errorf("databases = %v, want healthy true", out["databases"])
	}
	// Regression guard (Nit 3): the free-text database note is
	// dropped for gateway-key callers, not passed through. The
	// fake carries a sentinel note with a count in it
	// ("sentinel-note usage_events 99"), so re-adding the old
	// `if db.Note != "" { databases["note"] = db.Note }`
	// passthrough must fail this test — the note would appear
	// both as a "note" key on the databases map and on the wire.
	if _, has := dbs["note"]; has {
		t.Errorf("databases carries the free-text note for a gateway-key caller: %v", dbs["note"])
	}
	if strings.Contains(raw, "sentinel-note") {
		t.Errorf("result leaks the sentinel database note: %s", raw)
	}
	files, _ := dbs["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("databases.files = %v, want 1 file", dbs["files"])
	}
	file, _ := files[0].(map[string]any)
	if file["name"] != "ledger.db" || file["exists"] != true || file["size_bytes"] != float64(4096) {
		t.Errorf("databases.files[0] = %v, want ledger.db exists size 4096 (file facts, no rows)", file)
	}
	if _, has := file["rows"]; has {
		t.Errorf("databases.files[0] carries ledger-wide row counts for a gateway-key caller: %v", file)
	}
	note, _ := out["scope_note"].(string)
	if !strings.Contains(note, "gateway_keys") || !strings.Contains(note, "cache entries/bytes") || !strings.Contains(note, "row counts") || !strings.Contains(note, "database note") || !strings.Contains(note, "no per-key health view") {
		t.Errorf("scope_note = %q, want it to name every omitted fact", note)
	}
}

func TestHealthReportGatewayKeyDegradedVerdict(t *testing.T) {
	// The gateway caller still gets the verdict: a degraded
	// database degrades the status it sees, with the file error
	// (a router fact, not a key's data) still reported.
	src := newFakeSkillSource()
	src.db.Healthy = false
	src.db.Files = []mcp.DBFileHealth{{Name: "ledger.db", Exists: true, Error: "cannot open"}}
	out := invokeSkill(t, gatewayCtx("key-1"), src, "health-report", nil)
	if out["status"] != "degraded" {
		t.Errorf("status = %v, want degraded when a database is unhealthy", out["status"])
	}
	dbs, _ := out["databases"].(map[string]any)
	if dbs["healthy"] != false {
		t.Errorf("databases = %v, want healthy false", out["databases"])
	}
	files, _ := dbs["files"].([]any)
	file, _ := files[0].(map[string]any)
	if file["error"] != "cannot open" {
		t.Errorf("databases.files[0] = %v, want the file error still reported", file)
	}
	if _, has := out["router"]; has {
		t.Errorf("router facts present for a gateway-key caller: %v", out["router"])
	}
}

func TestHealthReportGatewayForeignKeyRejected(t *testing.T) {
	src := newFakeSkillSource()
	err := invokeSkillErr(t, gatewayCtx("key-1"), src, "health-report", map[string]any{"key_id": "key-2"})
	if !strings.Contains(err.Error(), `"key-2"`) || !strings.Contains(err.Error(), `"key-1"`) {
		t.Errorf("foreign-key error = %q, want it to name both the supplied and the caller key", err.Error())
	}
}

func TestHealthReportNoCallerFailsClosed(t *testing.T) {
	src := newFakeSkillSource()
	// No WithCaller on the context: same fail-closed as the
	// other scoped skills — the skill must refuse, not serve
	// router status to an unauthenticated caller.
	err := invokeSkillErr(t, context.Background(), src, "health-report", nil)
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("no-caller error = %q, want the caller-identity refusal", err.Error())
	}
}

func TestHealthReportLocalOperatorKeepsFullSet(t *testing.T) {
	src := newFakeSkillSource()
	out := invokeSkill(t, operatorCtx(), src, "health-report", nil)
	if out["scope"] != "local-operator" {
		t.Errorf("scope = %v, want local-operator", out["scope"])
	}
	if _, has := out["scoped_to_key"]; has {
		t.Errorf("scoped_to_key present for the local operator: %v", out["scoped_to_key"])
	}
	if _, has := out["scope_note"]; has {
		t.Errorf("scope_note present for the local operator: %v", out["scope_note"])
	}
	router, ok := out["router"].(map[string]any)
	if !ok {
		t.Fatalf("result has no router object for the local operator: %v", out)
	}
	if router["version"] != "0.1.0" || router["listen"] != "127.0.0.1:20128" || router["gateway_keys"] != float64(1) || router["providers"] != float64(2) || router["models"] != float64(3) || router["auto_router"] != true {
		t.Errorf("router = %v, want the full Status() for the operator", router)
	}
	cache, _ := out["cache"].(map[string]any)
	exact, _ := cache["exact"].(map[string]any)
	if exact["entries"] != float64(3) || exact["bytes"] != float64(120) {
		t.Errorf("cache.exact = %v, want the full counters (3 entries, 120 bytes) for the operator", exact)
	}
	dbs, _ := out["databases"].(map[string]any)
	files, _ := dbs["files"].([]any)
	file, _ := files[0].(map[string]any)
	rows, _ := file["rows"].(map[string]any)
	if rows["usage_events"] != float64(10) {
		t.Errorf("databases.files[0].rows = %v, want usage_events 10 for the operator", file["rows"])
	}
	// The operator path keeps the free-text note (skills.go:969
	// passes the whole DBHealth through) — this also proves the
	// sentinel in the fake is real, so the gateway test's
	// absence assertion is not vacuous.
	if dbs["note"] != "sentinel-note usage_events 99" {
		t.Errorf("databases.note = %v, want the sentinel note kept for the operator", dbs["note"])
	}
}
