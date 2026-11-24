package apitest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// mockMCPSource is the deterministic Source behind every MCP
// fixture: 2 mock providers, 2 mock models, fixed status. It mirrors
// the fake source in internal/mcp/mcp_test.go (same ids, names and
// counts) so fixtures recorded here agree with the mcp package's own
// tests. Nothing here is a real provider: "Coder"/"Chatter" are mock
// names, the counts are canned, and no key or base URL exists.
//
// M9.10 seeded state: the mock now also carries fixed,
// deterministic combo / ledger / cost / cache / DB / memory state
// (see newMockMCPSource) so the recorder captures SUCCESS payloads
// for the 21 M9.3/M9.4 tools instead of the errors the old mock
// honestly returned. Everything seeded is canned mock data,
// labelled as such in every fixture's recorded_from note — the
// same discipline as the providers above: fixed ids, names and
// counts, no live provider call, no real ledger, no real price
// book. Nothing here claims to be measured from a real router:
// the combos, ledger cells, cache counts, DB files and memories
// are the fixture's own fixed dataset, chosen so the tools'
// honesty rules stay visible (unpriced usage is UNKNOWN, never
// $0; a disabled provider makes its combo unusable; an unknown
// combo/model/key is an error, not a guess).
//
// Determinism: every value is a literal or derived from literals
// by fixed arithmetic; the only clock is mockNow
// (2026-11-19T12:00:00Z). Two runs record byte-identical
// fixtures. Mutations (create/switch/strategy/guard/memory/
// cache) apply to the one mock instance behind a server, so a
// recorder spec — which gets a fresh server per spec — always
// sees the seed, while the session test sees mutations in
// fixture order (reads run before mutations; see
// mcpFixtureNames, where memory_clear precedes memory_add for
// exactly this reason).
//
// mockNow is the fixture clock: every timestamp the mock emits
// (cost-report windows, DB file times, memory entries) is this
// fixed instant or derived from it — never time.Now().
var mockNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// mockModelCell is one fixed (provider, model) ledger cell: the
// measured history every combo-metric, cost-report and
// pick-fastest answer is derived from. coder-model is priced
// ($3.00 / 1M prompt tokens); chat-model is UNPRICED — its
// tokens are reported as unknown, never as $0.
type mockModelCell struct {
	provider       string
	model          string
	requests       int
	ok             int
	promptTokens   int
	completionTok  int
	costUSD        float64
	priceKnown     bool
	meanLatencyMS  float64
	latencyKnown   bool
	latencyEvents  int
	promptUSDPer1M float64
}

var mockLedgerCells = []mockModelCell{
	{provider: "p1", model: "coder-model", requests: 10, ok: 9, promptTokens: 1000, completionTok: 500, costUSD: 0.012, priceKnown: true, meanLatencyMS: 120.5, latencyKnown: true, latencyEvents: 9, promptUSDPer1M: 3.0},
	{provider: "p2", model: "chat-model", requests: 4, ok: 4, promptTokens: 400, completionTok: 200, costUSD: 0, priceKnown: false, meanLatencyMS: 0, latencyKnown: false, latencyEvents: 0},
}

// mockGuard is one fixed key budget: the rolling 5-hour cap plus
// optional calendar caps and the unpriced-usage policy.
type mockGuard struct {
	limitUSD       float64
	unpricedPolicy string
	daily          *float64
	weekly         *float64
	monthly        *float64
}

func mockF64(v float64) *float64 { return &v }

type mockMCPSource struct {
	combos          []mcp.ComboInfo
	activeCombo     string
	globalStrategy  string
	chainStrategies map[string]string
	guards          map[string]mockGuard
	spentUSD        map[string]float64
	cacheEntries    map[string]int
	memories        map[string][]mcp.MemoryEntry
	nextMemoryID    int
}

// newMockMCPSource returns a freshly seeded mock: combos "fast"
// (p1, fastest) and "cheap" (p2, cheapest — unusable, p2 is
// disabled), key-1 guarded at $10 fail_closed, 10 cache entries
// for the local-operator tenant, one ledger DB file, and two
// seeded memories for the local operator.
func newMockMCPSource() *mockMCPSource {
	return &mockMCPSource{
		combos: []mcp.ComboInfo{
			{Name: "fast", Members: []string{"p1"}, Strategy: "fastest", Usable: true},
			{Name: "cheap", Members: []string{"p2"}, Strategy: "cheapest", Usable: false, UnusableReason: "provider p2 is disabled"},
		},
		globalStrategy:  "priority",
		chainStrategies: map[string]string{},
		guards: map[string]mockGuard{
			"key-1": {limitUSD: 10, unpricedPolicy: "fail_closed", daily: mockF64(20), weekly: mockF64(50), monthly: mockF64(100)},
		},
		spentUSD:     map[string]float64{"key-1": 0.012},
		cacheEntries: map[string]int{"local-operator": 10},
		memories: map[string][]mcp.MemoryEntry{
			"local-operator": {
				{ID: "mem-1", Text: "The router caches repeated prompts to save cost.", CreatedAt: mockNow},
				{ID: "mem-2", Text: "Cache hits are fastest for repeated coding prompts.", CreatedAt: mockNow},
			},
		},
		nextMemoryID: 3,
	}
}

func (m *mockMCPSource) Providers() []mcp.ProviderInfo {
	return []mcp.ProviderInfo{
		{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}, CatalogID: "groq", Verification: "verified-from-docs"},
		{ID: "p2", Name: "Chatter", WireFormat: "anthropic", Enabled: false, Models: []string{"chat-model", "chat-model-2"}},
	}
}

func (m *mockMCPSource) Models() []mcp.ModelInfo {
	return []mcp.ModelInfo{
		{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"},
		{ID: "chat-model", ProviderID: "p2", ProviderName: "Chatter"},
	}
}

func (m *mockMCPSource) Status() mcp.StatusInfo {
	return mcp.StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 2, EnabledProviders: 1, Models: 3, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1}
}

// mockKnownStrategies is the strategy vocabulary the mock
// accepts, mirroring the production names (canonical strategies
// plus the combo execution modes create_combo documents).
var mockKnownStrategies = map[string]bool{
	"priority": true, "cheapest": true, "fastest": true, "most-reliable": true,
	"weighted": true, "lkgp": true, "code": true,
	"fusion": true, "pipeline": true, "jev": true,
}

func mockStrategyRule(strategy string) string {
	switch strategy {
	case "priority":
		return "Declared order first."
	case "cheapest":
		return "Lowest price first."
	case "fastest":
		return "Lowest measured latency first."
	case "most-reliable":
		return "Highest success rate first."
	case "weighted":
		return "Weighted by configured weights."
	case "lkgp":
		return "Lowest known good price first."
	case "code":
		return "Prefer code-capable providers first."
	case "fusion":
		return "Fuse member outputs into one answer."
	case "pipeline":
		return "Run members in sequence, each refining the last."
	case "jev":
		return "Judge-evaluate-verify across members."
	default:
		return "Declared order first."
	}
}

// mockProviderEnabled reports whether a provider id is a known,
// enabled mock provider (effort suffixes stripped).
func mockProviderEnabled(member string) (known, enabled bool) {
	if at := strings.LastIndex(member, "@"); at >= 0 {
		member = member[:at]
	}
	switch member {
	case "p1":
		return true, true
	case "p2":
		return true, false
	default:
		return false, false
	}
}

func (m *mockMCPSource) mockComboUsable(members []string) (bool, string) {
	for _, member := range members {
		known, enabled := mockProviderEnabled(member)
		if !known {
			return false, "unknown provider " + member
		}
		if !enabled {
			return false, "provider " + strings.SplitN(member, "@", 2)[0] + " is disabled"
		}
	}
	return true, ""
}

// M9.3 Source methods over the seeded state above: combos are
// the mock's fixed list (create/switch mutate that list, in
// memory only — nothing is written to disk), metrics/cost/quota
// derive from the fixed ledger cells, and explain/simulate are
// dry runs over the mock registry + the fixed $3.00/1M price
// for coder-model (chat-model is unpriced). Unknown names are
// refused as invalid input, never guessed.
func (m *mockMCPSource) Combos() []mcp.ComboInfo {
	out := make([]mcp.ComboInfo, len(m.combos))
	copy(out, m.combos)
	for i := range out {
		out[i].Active = out[i].Name == m.activeCombo
	}
	return out
}

func (m *mockMCPSource) CreateCombo(ctx context.Context, name string, members []string, strategy string) error {
	// Cancelled first, like the real sources: a cancelled
	// call aborts before validation or mutation.
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if strings.ContainsAny(name, ":/") {
		return fmt.Errorf("%w: combo name %q must not contain \":\" or \"/\"", mcp.ErrInvalidInput, name)
	}
	for _, c := range m.combos {
		if c.Name == name {
			return fmt.Errorf("%w: combo %q already exists", mcp.ErrInvalidInput, name)
		}
	}
	for _, member := range members {
		known, _ := mockProviderEnabled(member)
		if !known {
			return fmt.Errorf("%w: unknown provider %q", mcp.ErrInvalidInput, strings.SplitN(member, "@", 2)[0])
		}
	}
	if !mockKnownStrategies[strategy] {
		return fmt.Errorf("%w: unknown strategy %q", mcp.ErrInvalidInput, strategy)
	}
	usable, reason := m.mockComboUsable(members)
	m.combos = append(m.combos, mcp.ComboInfo{Name: name, Members: append([]string(nil), members...), Strategy: strategy, Usable: usable, UnusableReason: reason})
	return nil
}

func (m *mockMCPSource) ActiveCombo() string { return m.activeCombo }

func (m *mockMCPSource) SetActiveCombo(name string) error {
	if name == "" {
		m.activeCombo = ""
		return nil
	}
	for _, c := range m.combos {
		if c.Name == name {
			m.activeCombo = name
			return nil
		}
	}
	return fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
}

// mockComboMetrics sums the fixed ledger cells of a combo's
// member providers: requests/tokens/cost all-time, success rate
// nil with no requests, cost a lower bound when any member
// model is unpriced, mean latency over recent successful
// events only.
func (m *mockMCPSource) ComboMetrics(name string) (mcp.ComboMetrics, error) {
	var combo *mcp.ComboInfo
	for i := range m.combos {
		if m.combos[i].Name == name {
			combo = &m.combos[i]
			break
		}
	}
	if combo == nil {
		return mcp.ComboMetrics{}, fmt.Errorf("%w: unknown combo %q", mcp.ErrInvalidInput, name)
	}
	met := mcp.ComboMetrics{Name: name, CostKnown: true, Window: "all time"}
	for _, member := range combo.Members {
		base := strings.SplitN(member, "@", 2)[0]
		for _, cell := range mockLedgerCells {
			if cell.provider != base {
				continue
			}
			met.Requests += cell.requests
			met.OK += cell.ok
			met.Errors += cell.requests - cell.ok
			met.PromptTokens += cell.promptTokens
			met.CompletionTokens += cell.completionTok
			met.TotalTokens += cell.promptTokens + cell.completionTok
			met.CostUSD += cell.costUSD
			if !cell.priceKnown {
				met.CostKnown = false
				met.UnpricedTokens += cell.promptTokens + cell.completionTok
				met.UnpricedModels = append(met.UnpricedModels, cell.model)
			}
			if cell.latencyKnown {
				met.MeanLatencyMS = cell.meanLatencyMS
				met.LatencyKnown = true
				met.LatencyEvents += cell.latencyEvents
			}
		}
	}
	if met.Requests > 0 {
		sr := float64(met.OK) / float64(met.Requests)
		met.SuccessRate = &sr
	}
	return met, nil
}

func (m *mockMCPSource) RoutingStrategy(chain string) mcp.StrategyInfo {
	if strat, ok := m.chainStrategies[chain]; ok {
		return mcp.StrategyInfo{Name: strat, Rule: mockStrategyRule(strat), Scope: "per-chain", Chain: chain}
	}
	if chain == "" {
		return mcp.StrategyInfo{Name: m.globalStrategy, Rule: mockStrategyRule(m.globalStrategy), Scope: "global"}
	}
	return mcp.StrategyInfo{Name: m.globalStrategy, Rule: mockStrategyRule(m.globalStrategy), Scope: "global", Chain: chain, Inherited: true}
}

func (m *mockMCPSource) SetRoutingStrategy(chain, strategy string) error {
	if !mockKnownStrategies[strategy] {
		return fmt.Errorf("%w: unknown strategy %q", mcp.ErrInvalidInput, strategy)
	}
	if chain == "" {
		m.globalStrategy = strategy
	} else {
		m.chainStrategies[chain] = strategy
	}
	return nil
}
func (m *mockMCPSource) ExplainRoute(model string) (mcp.RouteExplanation, error) {
	for _, cell := range mockLedgerCells {
		if cell.model != model {
			continue
		}
		attempt := mcp.RouteAttempt{Provider: cell.provider, ProviderName: map[string]string{"p1": "Coder", "p2": "Chatter"}[cell.provider], Model: cell.model, Enabled: cell.provider == "p1", PriceKnown: cell.priceKnown}
		if cell.priceKnown {
			rate := cell.promptUSDPer1M
			attempt.PromptUSDPer1M = &rate
		}
		return mcp.RouteExplanation{
			Model:    model,
			Strategy: m.globalStrategy,
			Rule:     mockStrategyRule(m.globalStrategy),
			Attempts: []mcp.RouteAttempt{attempt},
			Note:     "Dry run — no provider was called. Declared (priority) order; strategies that re-rank do so at request time from live signals this fixture does not observe.",
		}, nil
	}
	return mcp.RouteExplanation{}, fmt.Errorf("%w: unknown model %q", mcp.ErrInvalidInput, model)
}

func (m *mockMCPSource) SimulateRoute(model string, promptTokens int) (mcp.RouteSimulation, error) {
	for _, cell := range mockLedgerCells {
		if cell.model != model {
			continue
		}
		attempt := mcp.SimAttempt{Provider: cell.provider, Model: cell.model, OnFailure: "stop", PriceKnown: cell.priceKnown}
		sim := mcp.RouteSimulation{
			Model:        model,
			Strategy:     m.globalStrategy,
			PromptTokens: promptTokens,
			Attempts:     []mcp.SimAttempt{attempt},
			CostKnown:    cell.priceKnown,
			Note:         "Dry run — no provider was called. Completion cost is never estimated (the completion length is unknown before the call).",
		}
		if cell.priceKnown {
			est := float64(promptTokens) * cell.promptUSDPer1M / 1e6
			sim.Attempts[0].EstimatedPromptCostUSD = &est
			sim.EstimatedPromptCostUSD = &est
		}
		return sim, nil
	}
	return mcp.RouteSimulation{}, fmt.Errorf("%w: unknown model %q", mcp.ErrInvalidInput, model)
}

func (m *mockMCPSource) ModelStats() []mcp.ModelStat {
	out := make([]mcp.ModelStat, 0, len(mockLedgerCells))
	for _, cell := range mockLedgerCells {
		st := mcp.ModelStat{Provider: cell.provider, Model: cell.model, Requests: cell.requests, OK: cell.ok, MeanLatencyMS: cell.meanLatencyMS, LatencyKnown: cell.latencyKnown, LatencyEvents: cell.latencyEvents}
		if cell.requests > 0 {
			sr := float64(cell.ok) / float64(cell.requests)
			st.SuccessRate = &sr
		}
		out = append(out, st)
	}
	return out
}

func (m *mockMCPSource) CostReport(keyID string, days int) (mcp.CostReport, error) {
	// Fixed window: ends at the fixture clock; days>0 counts
	// back from it, days=0 (all time) starts at the fixed
	// ledger epoch 2026-01-01. Never time.Now().
	end := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if days > 0 {
		start = end.AddDate(0, 0, -days)
	}
	rep := mcp.CostReport{
		KeyID: keyID, Days: days, WindowStart: start, WindowEnd: end,
		CostKnown: true, Sampled: false,
		ByModel: []mcp.ModelCost{},
		Note:    "Cost is a lower bound: 600 tokens are unpriced (chat-model) — unpriced usage is UNKNOWN, never $0.",
	}
	for _, cell := range mockLedgerCells {
		rep.Requests += cell.requests
		rep.OK += cell.ok
		rep.Errors += cell.requests - cell.ok
		rep.PromptTokens += cell.promptTokens
		rep.CompletionTokens += cell.completionTok
		rep.TotalTokens += cell.promptTokens + cell.completionTok
		rep.CostUSD += cell.costUSD
		mc := mcp.ModelCost{Provider: cell.provider, Model: cell.model, Requests: cell.requests, PromptTokens: cell.promptTokens, CompletionTokens: cell.completionTok, TotalTokens: cell.promptTokens + cell.completionTok, CostUSD: cell.costUSD, PriceKnown: cell.priceKnown}
		if !cell.priceKnown {
			rep.CostKnown = false
			rep.UnpricedTokens += cell.promptTokens + cell.completionTok
			rep.UnpricedModels = append(rep.UnpricedModels, cell.model)
			mc.UnpricedTokens = cell.promptTokens + cell.completionTok
		}
		rep.ByModel = append(rep.ByModel, mc)
	}
	return rep, nil
}

func (m *mockMCPSource) mockQuota(keyID string) mcp.QuotaReport {
	spent := m.spentUSD[keyID]
	guard, ok := m.guards[keyID]
	rep := mcp.QuotaReport{
		KeyID: keyID, HasBudget: ok, SpentUSD: spent,
		UnpricedPolicy: "fail_closed", ActiveWindow: "5h",
		UnpricedModels: []string{"chat-model"},
	}
	if !ok {
		return rep
	}
	rep.LimitUSD = guard.limitUSD
	rep.UnpricedPolicy = guard.unpricedPolicy
	remaining := guard.limitUSD - spent
	rep.RemainingUSD = &remaining
	rep.AtLimit = spent >= guard.limitUSD
	rep.ActiveLimitUSD = &rep.LimitUSD
	for _, spec := range []struct {
		cap *float64
		dst *mcp.QuotaWindow
	}{
		{guard.daily, &rep.Daily},
		{guard.weekly, &rep.Weekly},
		{guard.monthly, &rep.Monthly},
	} {
		spec.dst.SpentUSD = spent
		if spec.cap != nil {
			limit := *spec.cap
			spec.dst.LimitUSD = &limit
			rem := limit - spent
			spec.dst.RemainingUSD = &rem
			spec.dst.AtLimit = spent >= limit
		}
	}
	return rep
}

func (m *mockMCPSource) CheckQuota(keyID string) (mcp.QuotaReport, error) {
	return m.mockQuota(keyID), nil
}

func (m *mockMCPSource) SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, daily, weekly, monthly *float64) error {
	m.guards[keyID] = mockGuard{limitUSD: limitUSD, unpricedPolicy: unpricedPolicy, daily: daily, weekly: weekly, monthly: monthly}
	return nil
}

func (m *mockMCPSource) PriceKnown(provider, model string) bool {
	for _, cell := range mockLedgerCells {
		if cell.provider == provider && cell.model == model {
			return cell.priceKnown
		}
	}
	return false
}

// M9.4 Source methods over the seeded state: the cache has
// fixed 3-layer counts (exact + persistent present, semantic
// not wired in this process — Present=false, never zero-filled),
// the DB is one fixed ledger.db file, and memory is the
// per-caller in-memory store seeded above. CacheFlush evicts
// only the named tenant's entries; MemoryAdd enforces the
// 10,000-rune limit (runes, not bytes) like the real store.
func (m *mockMCPSource) CacheStats() mcp.CacheStats {
	return mcp.CacheStats{
		Exact:      mcp.CacheLayerStats{Present: true, Entries: 3, Bytes: 32},
		Semantic:   mcp.CacheLayerStats{Present: false},
		Persistent: mcp.CacheLayerStats{Present: true, Entries: 5, Bytes: 500},
		Note:       "mock fixture cache: exact and persistent layers seeded; semantic layer is not wired in this process",
	}
}

func (m *mockMCPSource) CacheFlush(tenant string) (int, error) {
	n := m.cacheEntries[tenant]
	m.cacheEntries[tenant] = 0
	return n, nil
}

func (m *mockMCPSource) mockDBFiles() []mcp.DBFileHealth {
	return []mcp.DBFileHealth{{
		Name: "ledger.db", Exists: true, SizeBytes: 1234, ModifiedAt: mockNow,
		Rows: map[string]int64{"usage_events": 14},
	}}
}

func (m *mockMCPSource) DBHealth() mcp.DBHealth {
	return mcp.DBHealth{Healthy: true, Files: m.mockDBFiles(), Note: "mock fixture databases: ledger.db only"}
}

func (m *mockMCPSource) DBStats() mcp.DBStats {
	return mcp.DBStats{Files: m.mockDBFiles(), Note: "mock fixture databases: ledger.db only"}
}

func (m *mockMCPSource) MemoryAdd(caller, text string) (mcp.MemoryEntry, error) {
	if n := utf8.RuneCountInString(text); n > mcp.MaxMemoryTextLen {
		return mcp.MemoryEntry{}, fmt.Errorf("%w: memory text is %d characters, over the %d limit", mcp.ErrInvalidInput, n, mcp.MaxMemoryTextLen)
	}
	e := mcp.MemoryEntry{ID: fmt.Sprintf("mem-%d", m.nextMemoryID), Text: text, CreatedAt: mockNow}
	m.nextMemoryID++
	m.memories[caller] = append(m.memories[caller], e)
	return e, nil
}

func (m *mockMCPSource) MemorySearch(caller, query string) ([]mcp.MemoryEntry, error) {
	entries := m.memories[caller]
	lower := strings.ToLower(query)
	out := make([]mcp.MemoryEntry, 0, len(entries))
	for _, e := range entries {
		if n := strings.Count(strings.ToLower(e.Text), lower); n > 0 {
			e.Matches = n
			out = append(out, e)
		}
	}
	// Ranked by occurrence count, then recency (later id =
	// later entry — all seeded entries share the fixed clock).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Matches != out[j].Matches {
			return out[i].Matches > out[j].Matches
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (m *mockMCPSource) MemoryClear(caller string) (int, error) {
	n := len(m.memories[caller])
	delete(m.memories, caller)
	return n, nil
}

// P1.2 ecosystem expansion — mockMCPSource stub implementations.

func (m *mockMCPSource) CapabilityEntries() []mcp.CapabilityEntry {
	out := []mcp.CapabilityEntry{}
	for _, p := range m.Providers() {
		for _, modelID := range p.Models {
			out = append(out, mcp.CapabilityEntry{
				ProviderID: p.ID,
				ModelID:    modelID,
				Capability: "tool_calling",
				Supported:  nil,
				Native:     false,
				Source:     "mock",
				Confidence: 0.0,
			})
		}
	}
	return out
}

func (m *mockMCPSource) QuotaIntelligence(filter string) ([]mcp.QuotaIntelEntry, error) {
	return []mcp.QuotaIntelEntry{}, nil
}

func (m *mockMCPSource) BudgetForecast(keyID string) (mcp.BudgetForecastEntry, error) {
	return mcp.BudgetForecastEntry{
		KeyID:            keyID,
		InsufficientData: true,
		UpdatedAt:        "2026-01-01T00:00:00Z",
	}, nil
}

func (m *mockMCPSource) CompressionStats() mcp.CompressionStatsEntry {
	return mcp.CompressionStatsEntry{}
}

func (m *mockMCPSource) EvalSchedules() ([]mcp.EvalScheduleEntry, error) {
	return []mcp.EvalScheduleEntry{}, nil
}

func (m *mockMCPSource) OrgReport() mcp.OrgReportEntry {
	return mcp.OrgReportEntry{}
}

func newMockMCPServer() *mcp.Server {
	// The fixture harness drives the server in-process as the local
	// operator (same explicit opt-in stdio uses) — New() defaults to
	// Anonymous() deny-all since the M9.1 B1 fix, so a bare New()
	// would answer every tools/call with -32002.
	return mcp.New(newMockMCPSource(), "0.1.0").WithClientIdentity(mcp.LocalOperator())
}

// mcpFixtureNames is every recorded MCP fixture, in session order
// (the realistic client flow: initialize, then list, then calls).
// ORDER MATTERS — the session test runs all of these through ONE
// server, so the mock's mutations accumulate: every read-only
// fixture runs before any mutation, the error fixtures (which
// mutate nothing) run next, and the mutating fixtures run last
// (create → switch → strategy → guard → memory_clear → memory_add
// → cache_flush). memory_clear precedes memory_add because the
// clear count is asserted (2 seeded entries in isolation; an add
// first would make the session clear 3). Each recorder spec gets
// a FRESH server, so in isolation every fixture sees the seed.
// Filtered-list fixtures (P2-8) are NOT in this list: they need a
// filtered server, not the unfiltered one — see
// mcpFilteredFixtureNames and TestMCPFilteredFixtures.
var mcpFixtureNames = []string{
	"initialize",
	"tools-list",
	"tools-call-router-status",
	"tools-call-provider-list",
	"tools-call-model-list",
	"ping",
	"error-unknown-tool",
	"error-unknown-method",
	"tools-call-list-combos",
	"tools-call-get-combo-metrics",
	"tools-call-explain-route",
	"tools-call-simulate-route",
	"tools-call-best-combo-for-task",
	"tools-call-pick-fastest-model",
	"tools-call-cost-report",
	"tools-call-check-quota",
	"tools-call-check-quota-unknown-key",
	"tools-call-cache-stats",
	"tools-call-db-health",
	"tools-call-db-stats",
	"tools-call-heartbeat",
	"tools-call-tool-search",
	"tools-call-memory-search",
	"error-best-combo-empty-task",
	"error-check-quota-missing-key",
	"error-cost-report-days",
	"error-create-combo-ghost-member",
	"error-explain-route-unknown-model",
	"error-get-combo-metrics-unknown-combo",
	"error-memory-add-empty-text",
	"error-memory-add-rune-limit",
	"error-memory-search-missing-query",
	"error-pick-fastest-unknown-provider",
	"error-set-budget-guard-bad-policy",
	"error-set-routing-strategy-unknown",
	"error-simulate-route-unknown-model",
	"error-switch-combos-unknown-combo",
	"error-tool-search-nonstring-query",
	// P1-6 error-path fixtures: the primary error branch per tool
	// over stdio as the local operator (success + primary error
	// branch per tool is the bar). WT-01..WT-08 (GAP-TRACK §12)
	// additionally pin the wrong-type/empty variants of the same
	// validators listed just below. All are
	// non-mutating (rejected before any Source write), so they
	// sit in the error block, before the mutations.
	"error-create-combo-missing-name",
	"error-create-combo-missing-members",
	"error-create-combo-members-nonstring",
	"error-create-combo-duplicate-name",
	"error-create-combo-bad-name",
	"error-create-combo-unknown-strategy",
	"error-switch-combos-missing-name",
	"error-switch-combos-active-false",
	"error-switch-combos-active-nonbool",
	"error-get-combo-metrics-missing-name",
	"error-set-routing-strategy-missing-strategy",
	"error-best-combo-negative-max-cost",
	"error-best-combo-negative-max-latency",
	"error-explain-route-missing-model",
	"error-explain-route-unknown-combo",
	"error-explain-route-combo-no-provider",
	"error-simulate-route-missing-model",
	"error-simulate-route-negative-tokens",
	"error-simulate-route-noninteger-tokens",
	"error-simulate-route-unknown-combo",
	"error-simulate-route-combo-no-provider",
	"error-cost-report-noninteger-days",
	"error-check-quota-nonstring-key",
	"error-set-budget-guard-missing-key",
	"error-set-budget-guard-missing-limit",
	"error-set-budget-guard-negative-limit",
	"error-set-budget-guard-negative-daily",
	"error-pick-fastest-min-requests-zero",
	"error-pick-fastest-min-requests-high",
	// WT-01..WT-16 (GAP-TRACK §12): wrong-type/empty pins for the
	// same validators — create_combo name/members-element/strategy,
	// switch_combos + get_combo_metrics name, set_routing_strategy
	// strategy/chain, explain_route + simulate_route model/combo.
	// All non-mutating (rejected before any Source write), so
	// they sit in the error block, before the mutations.
	"error-create-combo-nonstring-name",
	"error-create-combo-members-empty-element",
	"error-create-combo-nonstring-strategy",
	"error-switch-combos-nonstring-name",
	"error-switch-combos-empty-name",
	"error-get-combo-metrics-nonstring-name",
	"error-get-combo-metrics-empty-name",
	"error-set-routing-strategy-nonstring-strategy",
	"error-set-routing-strategy-empty-strategy",
	"error-set-routing-strategy-nonstring-chain",
	"error-explain-route-nonstring-model",
	"error-explain-route-empty-model",
	"error-explain-route-nonstring-combo",
	"error-simulate-route-nonstring-model",
	"error-simulate-route-empty-model",
	"error-simulate-route-nonstring-combo",
	"tools-call-create-combo",
	"tools-call-switch-combos",
	"tools-call-set-routing-strategy",
	"tools-call-set-budget-guard",
	"tools-call-memory-clear",
	"tools-call-memory-add",
	"tools-call-cache-flush",
}

// mcpFilteredFixtureGroups is the P2-8 filtered-list surface:
// one filtered server per group. The filtered count is pinned
// on tools/list per group — by index (every visible name, last
// index included) plus a json_lengths pin on result.tools — and
// by a tools/call to a filtered-out tool, which must answer
// -32602 unknown tool, the same code as a tool that was never
// registered. Only deny-2 pins the count three ways (tools/list
// + tool_search count + heartbeat tool_count), because it is the
// only group where tool_search and heartbeat themselves survive
// the filter; in allow-3, max-5 and allow-deny those two tools
// are hidden, so calling them is itself a -32602 unknown-tool
// error fixture. The groups are the audit's F4 cases: an
// exclusive allow-list, a MaxTools cap, a deny-list, and the
// deny-wins-over-allow conflict.
var mcpFilteredFixtureGroups = map[string][]string{
	// Allow-list of the 3 built-ins only.
	"allow-3": {
		"tools-list-allow-3",
		"error-call-tool-search-allow-3",
		"error-call-heartbeat-allow-3",
		"error-call-filtered-tool-allow-3",
	},
	// MaxTools cap of 5: the first 5 in Tools() order.
	"max-5": {
		"tools-list-max-5",
		"error-call-tool-search-max-5",
		"error-call-heartbeat-max-5",
		"error-call-filtered-tool-max-5",
	},
	// Deny-list of 2 (cache_stats, db_stats): 22 tools remain,
	// and the survivors shift up in Tools() order. This is the
	// only group where tool_search and heartbeat survive the
	// filter, so only here does the three-way pin run
	// (tools/list + tool_search count + heartbeat tool_count).
	"deny-2": {
		"tools-list-deny-2",
		"tools-call-tool-search-deny-2",
		"tools-call-heartbeat-deny-2",
		"error-call-filtered-tool-deny-2",
	},
	// Deny wins over allow: cache_stats is allow-listed AND
	// denied, so only router_status is visible.
	"allow-deny": {
		"tools-list-allow-deny-conflict",
		"error-call-tool-search-allow-deny",
		"error-call-filtered-tool-allow-deny",
	},
}

// mcpFilteredFixtureNames is every P2-8 filtered fixture, in
// group order (allow-3, max-5, deny-2, allow-deny).
var mcpFilteredFixtureNames = []string{
	"tools-list-allow-3",
	"error-call-tool-search-allow-3",
	"error-call-heartbeat-allow-3",
	"error-call-filtered-tool-allow-3",
	"tools-list-max-5",
	"error-call-tool-search-max-5",
	"error-call-heartbeat-max-5",
	"error-call-filtered-tool-max-5",
	"tools-list-deny-2",
	"tools-call-tool-search-deny-2",
	"tools-call-heartbeat-deny-2",
	"error-call-filtered-tool-deny-2",
	"tools-list-allow-deny-conflict",
	"error-call-tool-search-allow-deny",
	"error-call-filtered-tool-allow-deny",
}

// mcpFilteredFixtureGroup maps a filtered fixture name to its
// group (the filter its server carries).
func mcpFilteredFixtureGroup(name string) string {
	for group, names := range mcpFilteredFixtureGroups {
		for _, n := range names {
			if n == name {
				return group
			}
		}
	}
	return ""
}

// newFilteredMockMCPServer returns the mock-backed fixture server
// with one group's cardinality filter applied. A fresh mock
// Source is built per call, so no filter state leaks between
// groups (the filter maps are keyed by server pointer AND by
// Source — a shared Source would share the filter on the
// handler path; see internal/mcp/tools_search.go).
func newFilteredMockMCPServer(group string) *mcp.Server {
	srv := newMockMCPServer()
	switch group {
	case "allow-3":
		return srv.WithToolFilter([]string{"router_status", "provider_list", "model_list"}, nil)
	case "max-5":
		return srv.WithMaxTools(5)
	case "deny-2":
		return srv.WithToolFilter(nil, []string{"cache_stats", "db_stats"})
	case "allow-deny":
		return srv.WithToolFilter([]string{"router_status", "cache_stats"}, []string{"cache_stats"})
	default:
		panic("apitest: unknown filtered fixture group " + group)
	}
}

// TestMCPFixtures runs every recorded fixture as a single exchange
// against the real mcp server (mock-backed Source).
func TestMCPFixtures(t *testing.T) {
	for _, name := range mcpFixtureNames {
		t.Run(name, func(t *testing.T) {
			f := LoadMCPFixture(t, name)
			RunMCP(t, newMockMCPServer(), f)
		})
	}
}

// TestMCPFixtureSession runs all fixtures in order through ONE
// Serve session — initialize → tools/list → 3 reads → ping →
// 2 errors → 15 reads → 52 error cases → 7 mutations — and asserts
// every response. Totals: 25 tools/call successes (18 reads +
// 7 mutations), 54 error cases. This is the end-to-end proof the
// harness drives a real session, not just isolated exchanges.
// (Order is load-bearing: reads before mutations; see
// mcpFixtureNames.)
func TestMCPFixtureSession(t *testing.T) {
	fixtures := make([]MCPFixture, 0, len(mcpFixtureNames))
	for _, name := range mcpFixtureNames {
		fixtures = append(fixtures, LoadMCPFixture(t, name))
	}
	RunMCPSession(t, newMockMCPServer(), fixtures...)
}

// TestMCPFilteredFixtures runs every P2-8 filtered-list fixture
// as a single exchange against a server carrying that fixture's
// filter — never the unfiltered server, which would announce all
// 32 tools and fail the filtered assertions.
func TestMCPFilteredFixtures(t *testing.T) {
	for _, name := range mcpFilteredFixtureNames {
		t.Run(name, func(t *testing.T) {
			f := LoadMCPFixture(t, name)
			RunMCP(t, newFilteredMockMCPServer(mcpFilteredFixtureGroup(name)), f)
		})
	}
}

// TestMCPFilteredFixtureSessions runs each filter group's
// fixtures in order through ONE filtered server per group — the
// same session discipline as TestMCPFixtureSession, per filter.
// Every fixture in a group is a read or a rejected call, so no
// group mutates state; the session still proves the filtered
// server answers a realistic client flow consistently. In
// deny-2 the flow is list → search → heartbeat → filtered-out
// call; in allow-3, max-5 and allow-deny the search and
// heartbeat calls are themselves -32602 unknown-tool errors,
// because those tools are hidden by the filter.
func TestMCPFilteredFixtureSessions(t *testing.T) {
	for _, group := range []string{"allow-3", "max-5", "deny-2", "allow-deny"} {
		t.Run(group, func(t *testing.T) {
			fixtures := make([]MCPFixture, 0, len(mcpFilteredFixtureGroups[group]))
			for _, name := range mcpFilteredFixtureGroups[group] {
				fixtures = append(fixtures, LoadMCPFixture(t, name))
			}
			RunMCPSession(t, newFilteredMockMCPServer(group), fixtures...)
		})
	}
}

// TestMCPFixtureRecordedResponsesMatch asserts the recorded response
// in each fixture file still satisfies its own assertions — i.e. the
// files on disk are self-consistent (the loader already enforces
// id/error shape; this checks the full assert block).
func TestMCPFixtureRecordedResponsesMatch(t *testing.T) {
	for _, name := range mcpFixtureNames {
		f := LoadMCPFixture(t, name)
		if err := CheckMCPResponse(f.RecordedResponse, f); err != nil {
			t.Errorf("fixture %s: recorded response fails its own assertions: %v", name, err)
		}
	}
	// The filtered fixtures are self-consistent the same way;
	// they are checked here (not in the loop above) because they
	// are not part of the unfiltered session list.
	for _, name := range mcpFilteredFixtureNames {
		f := LoadMCPFixture(t, name)
		if err := CheckMCPResponse(f.RecordedResponse, f); err != nil {
			t.Errorf("fixture %s: recorded response fails its own assertions: %v", name, err)
		}
	}
}

// TestCheckMCPResponseWrongJSONPathFails is the toggle proof for the
// harness's core assertion path: the tools/list response must FAIL
// when the fixture expects a different third tool — and likewise
// when it expects a different LAST tool (the 32-tool pin must
// catch a rename at either end of the list, not just among the
// three original built-ins).
func TestCheckMCPResponseWrongJSONPathFails(t *testing.T) {
	f := LoadMCPFixture(t, "tools-list")
	f.Assert.JSONPaths["result.tools.2.name"] = "provider_list" // wrong on purpose: the third tool is model_list
	if err := CheckMCPResponse(f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a tools/list response whose third tool is model_list against an expectation of provider_list")
	} else if !strings.Contains(err.Error(), "result.tools.2.name") {
		t.Fatalf("wrong-path error should name result.tools.2.name, got: %v", err)
	}
	f = LoadMCPFixture(t, "tools-list")
	f.Assert.JSONPaths["result.tools.23.name"] = "router_status" // wrong on purpose: the last tool is tool_search
	if err := CheckMCPResponse(f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a tools/list response whose last tool is tool_search against an expectation of router_status")
	} else if !strings.Contains(err.Error(), "result.tools.23.name") {
		t.Fatalf("wrong-path error should name result.tools.23.name, got: %v", err)
	}
}

// TestCheckMCPResponseWrongLengthFails is the toggle proof for the
// tools/list length pin (json_lengths result.tools = 32): a
// recorded response with a 25th tool appended — or one tool
// dropped — must FAIL even though every pinned tool name still
// matches. Without the length pin, a silently added tool would
// pass the name-by-index assertions.
func TestCheckMCPResponseWrongLengthFails(t *testing.T) {
	f := LoadMCPFixture(t, "tools-list")
	var resp map[string]any
	if err := json.Unmarshal(f.RecordedResponse, &resp); err != nil {
		t.Fatalf("unmarshal tools-list recorded response: %v", err)
	}
	tools, ok := resp["result"].(map[string]any)["tools"].([]any)
	if !ok {
		t.Fatal("tools-list recorded response has no result.tools array")
	}
	// 25 tools: append a plausible extra tool; every pinned
	// name (0-23) still matches, so only the length pin can fail.
	extra := append(append([]any{}, tools...), map[string]any{"name": "extra_tool", "description": "not a real tool", "inputSchema": map[string]any{"type": "object"}})
	resp["result"].(map[string]any)["tools"] = extra
	raw25, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal 25-tool response: %v", err)
	}
	if err := CheckMCPResponse(raw25, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a tools/list response with 25 tools against a length pin of 32")
	} else if !strings.Contains(err.Error(), "result.tools") || !strings.Contains(err.Error(), "33") {
		t.Fatalf("wrong-length error should name result.tools and length 33, got: %v", err)
	}
	// 23 tools: drop the last tool; the pinned names still
	// match, so again only the length pin can fail.
	resp["result"].(map[string]any)["tools"] = tools[:23]
	raw23, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal 23-tool response: %v", err)
	}
	if err := CheckMCPResponse(raw23, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a tools/list response with 23 tools against a length pin of 32")
	} else if !strings.Contains(err.Error(), "result.tools") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("wrong-length error should name result.tools and length 23, got: %v", err)
	}
}

// TestCheckMCPResponseNonArrayLengthFails is the toggle proof for
// the non-array branch of the tools/list length pin
// (json_lengths result.tools = 32). It asserts BOTH branches with
// the same fixture, so it genuinely discriminates instead of only
// documenting one outcome (the weak shape the M9.3 review flagged
// in TestM93ToggleCreateComboHandlerNeutralised: a toggle test
// that checks a single branch passes even if the checker always
// fails, or always succeeds):
//
//   - PASS branch: the genuine recorded 32-tool array passes — a
//     checker that unconditionally errors would fail here.
//   - FAIL branch: three non-array values (string, object,
//     number) must each fail naming result.tools with the
//     "want an array of length 32" message — proving the branch
//     triggers on any non-array, not just on one canned value.
//   - DISTINCTNESS branch: a 23-tool array must fail with the
//     DIFFERENT "got array length 23" message and never with
//     "want an array" — proving the test pins the non-array
//     branch specifically, not just any failure. Without the
//     non-array branch, a non-array value would fail with the
//     misleading "got array length 0, want 32" error (len of a
//     nil slice is 0) instead of the clear "want an array" error.
func TestCheckMCPResponseNonArrayLengthFails(t *testing.T) {
	// Drop the per-tool JSONPaths pins (result.tools.N.name /
	// result.tools.N.inputSchema): CheckMCPResponse runs JSONPaths
	// before JSONLengths, so those pins would fail first on a
	// non-array tools value and mask the branch under test. The
	// length pin (json_lengths result.tools = 32) stays in place.
	load := func(t *testing.T) MCPFixture {
		t.Helper()
		f := LoadMCPFixture(t, "tools-list")
		for path := range f.Assert.JSONPaths {
			if strings.HasPrefix(path, "result.tools.") {
				delete(f.Assert.JSONPaths, path)
			}
		}
		return f
	}
	decodeTools := func(t *testing.T, f MCPFixture) (map[string]any, []any) {
		t.Helper()
		var resp map[string]any
		if err := json.Unmarshal(f.RecordedResponse, &resp); err != nil {
			t.Fatalf("unmarshal tools-list recorded response: %v", err)
		}
		tools, ok := resp["result"].(map[string]any)["tools"].([]any)
		if !ok {
			t.Fatal("tools-list recorded response has no result.tools array")
		}
		return resp, tools
	}
	marshal := func(t *testing.T, resp map[string]any) []byte {
		t.Helper()
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatalf("marshal tools-list response: %v", err)
		}
		return raw
	}

	// PASS branch: the genuine 32-tool array passes the length pin.
	if f := load(t); true {
		if err := CheckMCPResponse(f.RecordedResponse, f); err != nil {
			t.Fatalf("genuine 32-tool tools/list response should pass the length pin: %v", err)
		}
	}

	// FAIL branch: every non-array shape fails with the
	// "want an array" message, never with a length-mismatch one.
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"string", "not an array"},
		{"object", map[string]any{"name": "not an array"}},
		{"number", float64(32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := load(t)
			resp, _ := decodeTools(t, f)
			resp["result"].(map[string]any)["tools"] = tc.value
			if err := CheckMCPResponse(marshal(t, resp), f); err == nil {
				t.Fatalf("CheckMCPResponse accepted a tools/list response whose result.tools is a %s against a length pin of 32", tc.name)
			} else if !strings.Contains(err.Error(), "result.tools") || !strings.Contains(err.Error(), "want an array of length 32") {
				t.Fatalf("non-array error should name result.tools and want an array of length 32, got: %v", err)
			} else if strings.Contains(err.Error(), "got array length") {
				t.Fatalf("non-array error must say 'want an array', not the length-mismatch message, got: %v", err)
			}
		})
	}

	// DISTINCTNESS branch: a short array fails with the
	// length-mismatch message, not the "want an array" one.
	f := load(t)
	resp, tools := decodeTools(t, f)
	resp["result"].(map[string]any)["tools"] = tools[:23]
	if err := CheckMCPResponse(marshal(t, resp), f); err == nil {
		t.Fatal("CheckMCPResponse accepted a tools/list response with 23 tools against a length pin of 32")
	} else if !strings.Contains(err.Error(), "result.tools") || !strings.Contains(err.Error(), "got array length 23") {
		t.Fatalf("wrong-length error should name result.tools and got array length 23, got: %v", err)
	} else if strings.Contains(err.Error(), "want an array") {
		t.Fatalf("wrong-length error must not say 'want an array' (that message belongs to the non-array branch), got: %v", err)
	}
}

// comboCreator is the minimal CreateCombo surface the toggle test
// below exercises: the seeded mock and the refusing wrapper both
// satisfy it, so one helper asserts both branches.
type comboCreator interface {
	CreateCombo(ctx context.Context, name string, members []string, strategy string) error
	Combos() []mcp.ComboInfo
}

// refusingMockMCPSource is the toggle-OFF Source: CreateCombo
// always refuses with ErrInvalidInput, so any assertion that a
// creation happened must fail against it.
type refusingMockMCPSource struct{ *mockMCPSource }

func (r *refusingMockMCPSource) CreateCombo(ctx context.Context, name string, members []string, strategy string) error {
	return fmt.Errorf("%w: combo %q refused (toggle off)", mcp.ErrInvalidInput, name)
}

// assertCreateComboOutcome runs the SAME assertions against either
// branch of the CreateCombo toggle: wantCreated selects the
// toggle-ON (live mock) outcome versus the toggle-OFF (refusing
// mock) outcome. The test only discriminates if both branches
// are asserted here — a test that checked only one branch would
// pass against a CreateCombo that always succeeded, or always
// refused (the weak shape the M9.3 review flagged in
// TestM93ToggleCreateComboHandlerNeutralised, which documents
// rather than performs its toggle).
func assertCreateComboOutcome(t *testing.T, src comboCreator, wantCreated bool) {
	t.Helper()
	before := src.Combos()
	err := src.CreateCombo(context.Background(), "toggle-combo", []string{"p1"}, "fastest")
	after := src.Combos()
	if wantCreated {
		if err != nil {
			t.Fatalf("toggle-on CreateCombo: unexpected error: %v", err)
		}
		if len(after) != len(before)+1 {
			t.Fatalf("toggle-on CreateCombo: combo count = %d, want %d (one combo stored)", len(after), len(before)+1)
		}
		var created *mcp.ComboInfo
		for i := range after {
			if after[i].Name == "toggle-combo" {
				created = &after[i]
			}
		}
		if created == nil {
			t.Fatal("toggle-on CreateCombo: combo toggle-combo not in Combos() after a nil error")
		}
		if len(created.Members) != 1 || created.Members[0] != "p1" || created.Strategy != "fastest" || !created.Usable {
			t.Fatalf("toggle-on CreateCombo: stored combo = %+v, want members [p1], strategy fastest, usable", *created)
		}
		return
	}
	if err == nil {
		t.Fatal("toggle-off CreateCombo: expected a refusal, got a nil error")
	}
	if !errors.Is(err, mcp.ErrInvalidInput) {
		t.Fatalf("toggle-off CreateCombo: error = %v, want ErrInvalidInput", err)
	}
	if len(after) != len(before) {
		t.Fatalf("toggle-off CreateCombo: combo count = %d, want %d (nothing stored)", len(after), len(before))
	}
	for _, c := range after {
		if c.Name == "toggle-combo" {
			t.Fatal("toggle-off CreateCombo: combo toggle-combo was stored despite the refusal")
		}
	}
}

// TestMockCreateComboToggle is the CreateCombo toggle test for the
// apitest mock Source (mockMCPSource.CreateCombo,
// mcp_fixture_test.go:233): toggle ON (the live seeded mock) the
// create succeeds and the combo is stored with its members,
// strategy and usability; toggle OFF (the refusing wrapper) the
// same call is refused with ErrInvalidInput and stores nothing.
// Both branches run the same assertions via
// assertCreateComboOutcome, so flipping the behaviour flips the
// test — verified by the negative control (overlay a neutralised
// mock CreateCombo: this test FAILS; restore: it PASSES).
func TestMockCreateComboToggle(t *testing.T) {
	assertCreateComboOutcome(t, newMockMCPSource(), true)
	assertCreateComboOutcome(t, &refusingMockMCPSource{newMockMCPSource()}, false)
}

// TestCheckMCPResponseWrongIDFails: a response whose id does not
// match the fixture must fail, even when everything else matches.
func TestCheckMCPResponseWrongIDFails(t *testing.T) {
	f := LoadMCPFixture(t, "ping")
	f.Assert.ID = float64(99) // wrong on purpose: the response id is 6
	if err := CheckMCPResponse(f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a response with id 6 against assert.id 99")
	} else if !strings.Contains(err.Error(), "id") {
		t.Fatalf("wrong-id error should name id, got: %v", err)
	}
}

// TestCheckMCPResponseWrongErrorCodeFails: an error response with the
// wrong code must fail.
func TestCheckMCPResponseWrongErrorCodeFails(t *testing.T) {
	f := LoadMCPFixture(t, "error-unknown-tool")
	want := -32601 // wrong on purpose: unknown tool is -32602
	f.Assert.ErrorCode = &want
	if err := CheckMCPResponse(f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted error code -32602 against an expectation of -32601")
	} else if !strings.Contains(err.Error(), "error.code") {
		t.Fatalf("wrong-code error should name error.code, got: %v", err)
	}
}

// TestCheckMCPResponseMissingRequiredFieldFails: a missing required
// field must fail.
func TestCheckMCPResponseMissingRequiredFieldFails(t *testing.T) {
	f := LoadMCPFixture(t, "initialize")
	f.Assert.RequiredFields = []string{"result.nonexistent_field"}
	if err := CheckMCPResponse(f.RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted a response missing a required field")
	}
}

// TestCheckMCPResponseErrorWhenResultExpectedFails: an error
// response must fail a fixture that expects a result, and vice versa.
func TestCheckMCPResponseErrorWhenResultExpectedFails(t *testing.T) {
	f := LoadMCPFixture(t, "ping")
	if err := CheckMCPResponse(LoadMCPFixture(t, "error-unknown-method").RecordedResponse, f); err == nil {
		t.Fatal("CheckMCPResponse accepted an error response against a fixture expecting a result")
	}
	errFixture := LoadMCPFixture(t, "error-unknown-tool")
	if err := CheckMCPResponse(LoadMCPFixture(t, "ping").RecordedResponse, errFixture); err == nil {
		t.Fatal("CheckMCPResponse accepted a result response against a fixture expecting an error")
	}
}

// TestCheckMCPResponsePresenceOnlyNullPasses: a null expected value
// means "must exist, any value".
func TestCheckMCPResponsePresenceOnlyNullPasses(t *testing.T) {
	f := LoadMCPFixture(t, "tools-call-router-status")
	f.Assert.JSONPaths = map[string]any{"result.structuredContent.uptime_seconds": nil}
	if err := CheckMCPResponse(f.RecordedResponse, f); err != nil {
		t.Fatalf("presence-only null assertion should pass: %v", err)
	}
}

// TestCheckMCPResponseNonJSONFails: a non-JSON response must fail.
func TestCheckMCPResponseNonJSONFails(t *testing.T) {
	f := LoadMCPFixture(t, "ping")
	if err := CheckMCPResponse([]byte("not json{"), f); err == nil {
		t.Fatal("CheckMCPResponse accepted a non-JSON response")
	}
}

func TestLoadMCPFixtureRejectsNonFixture(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m9.0","name":"bad","fixture":false,` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"assert":{"id":1}}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP fixture with \"fixture\": false")
	}
}

func TestLoadMCPFixtureRejectsBadSchemaVersion(t *testing.T) {
	raw := `{"schema_version":2,"slice":"m9.0","name":"bad","fixture":true,` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"assert":{"id":1}}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted schema_version 2")
	}
}

// writeMCPFixtureFile writes raw JSON to a temp file and returns its path.
func writeMCPFixtureFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write bad fixture: %v", err)
	}
	return path
}

func TestLoadMCPFixtureRejectsUnknownField(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m9.0","name":"bad","fixture":true,` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"assert":{"id":1},"bogus_field":1}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP fixture with an unknown field")
	}
}

func TestLoadMCPFixtureRejectsMethodMismatch(t *testing.T) {
	// The fixture's method field must match the request's method.
	raw := `{"schema_version":1,"slice":"m9.0","name":"bad","fixture":true,` +
		`"method":"tools/list","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"result":{}},` +
		`"assert":{"id":1}}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an MCP fixture whose method does not match its request")
	}
}

func TestLoadMCPFixtureRejectsErrorWithoutCode(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m9.0","name":"bad","fixture":true,` +
		`"method":"resources/list","request":{"jsonrpc":"2.0","id":1,"method":"resources/list"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}},` +
		`"assert":{"id":1,"expect_error":true}}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted an error fixture with no error_code")
	}
}

func TestLoadMCPFixtureRejectsRecordedIDMismatch(t *testing.T) {
	raw := `{"schema_version":1,"slice":"m9.0","name":"bad","fixture":true,` +
		`"method":"ping","request":{"jsonrpc":"2.0","id":1,"method":"ping"},` +
		`"recorded_response":{"jsonrpc":"2.0","id":2,"result":{}},` +
		`"assert":{"id":1}}`
	if _, err := loadMCPFixtureFile(writeMCPFixtureFile(t, raw)); err == nil {
		t.Fatal("loader accepted a fixture whose recorded response id does not match assert.id")
	}
}

func TestLoadMCPFixtureMissingFileErrors(t *testing.T) {
	if _, err := loadMCPFixtureFile(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Fatal("loader accepted a missing MCP fixture file")
	}
}

func TestMCPFixturePathRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"..", "../x", "a/b", "a\\b", ""} {
		if _, err := MCPFixturePath(bad); err == nil {
			t.Fatalf("MCPFixturePath accepted name=%q", bad)
		}
	}
}

// --- Recorder -----------------------------------------------------
//
// TestRecordMCPFixtures re-records every fixture by running the real
// mcp Server (mock-backed Source) over in-memory stdio and capturing
// the response verbatim. It is skipped unless
// CODVIAN_RECORD_MCP_FIXTURES=1, so the normal suite never rewrites
// fixtures. The recorded_response written to disk is the captured
// bytes, not a hand-written value; the test refuses to write a file
// whose assertions do not match the captured response.
//
// To re-record: CODVIAN_RECORD_MCP_FIXTURES=1 go test ./internal/apitest/ -run TestRecordMCPFixtures -count=1

type mcpRecordSpec struct {
	name    string
	method  string
	request string // exact JSON-RPC request line sent
	assert  MCPAssert
	notes   string
	// filterGroup names the P2-8 cardinality filter the recorder
	// server carries for this spec ("" = unfiltered). The test
	// runner uses the same group to pick the server, so a
	// recorded filtered response is always replayed against the
	// filter that produced it.
	filterGroup string
}

func mcpRecordSpecs() []mcpRecordSpec {
	err32602 := -32602
	err32601 := -32601
	return []mcpRecordSpec{
		{
			name:    "initialize",
			method:  "initialize",
			request: `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-fixture-recorder","version":"0"}}}`,
			assert: MCPAssert{ID: float64(1), RequiredFields: []string{"result.protocolVersion", "result.capabilities", "result.serverInfo.name", "result.serverInfo.version"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.protocolVersion": "2025-03-26", "result.serverInfo.name": "codvian-router", "result.serverInfo.version": "0.1.0"}},
			notes: "Handshake: the server reports protocol 2025-03-26 and its name/version; capabilities carry the tools key only.",
		},
		{
			name:    "tools-list",
			method:  "tools/list",
			request: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
			assert: MCPAssert{ID: float64(2), RequiredFields: []string{"result.tools"},
				JSONPaths:   map[string]any{"jsonrpc": "2.0"},
				JSONLengths: map[string]int{"result.tools": 32}},
			notes: "The full current tool surface: 32 tools in alphabetical order as returned by the MCP server. Every tool carries an inputSchema (nil = presence-only). The length pin (json_lengths result.tools = 32) fails if a 33rd tool appears or one goes missing.",
		},
		{
			name:    "tools-call-router-status",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`,
			assert: MCPAssert{ID: float64(3), RequiredFields: []string{"result.content", "result.structuredContent"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.version": "0.1.0", "result.structuredContent.listen": "127.0.0.1:20128", "result.structuredContent.providers": float64(2), "result.structuredContent.enabled_providers": float64(1), "result.structuredContent.models": float64(3), "result.structuredContent.gateway_keys": float64(1), "result.structuredContent.auto_router": true, "result.structuredContent.uptime_seconds": float64(-1)}},
			notes: "router_status against the mock registry: 2 providers (1 enabled), 3 models, 1 gateway key, auto-router on, uptime -1 (no serving loop).",
		},
		{
			name:    "tools-call-provider-list",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`,
			assert: MCPAssert{ID: float64(4), RequiredFields: []string{"result.content", "result.structuredContent.providers"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.providers.0.id": "p1", "result.structuredContent.providers.0.name": "Coder", "result.structuredContent.providers.0.wire_format": "openai", "result.structuredContent.providers.0.enabled": true, "result.structuredContent.providers.1.id": "p2", "result.structuredContent.providers.1.enabled": false}},
			notes: "provider_list: mock providers only; no base URL and no API key appears in any entry.",
		},
		{
			name:    "tools-call-model-list",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`,
			assert: MCPAssert{ID: float64(5), RequiredFields: []string{"result.content", "result.structuredContent.models"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.models.0.id": "coder-model", "result.structuredContent.models.0.provider_id": "p1", "result.structuredContent.models.1.id": "chat-model", "result.structuredContent.models.1.provider_id": "p2"}},
			notes: "model_list: mock models with their provider ids.",
		},
		{
			name:    "tools-call-list-combos",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"list_combos","arguments":{}}}`,
			assert: MCPAssert{ID: float64(9), RequiredFields: []string{"result.content", "result.structuredContent.combos"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.count": float64(2), "result.structuredContent.active_combo": nil, "result.structuredContent.combos.0.name": "fast", "result.structuredContent.combos.0.members.0": "p1", "result.structuredContent.combos.0.strategy": "fastest", "result.structuredContent.combos.0.usable": true, "result.structuredContent.combos.1.name": "cheap", "result.structuredContent.combos.1.members.0": "p2", "result.structuredContent.combos.1.strategy": "cheapest", "result.structuredContent.combos.1.usable": false}},
			notes: "list_combos against the seeded mock: combos fast (p1, fastest, usable) and cheap (p2, cheapest, unusable — p2 is disabled); no session-active combo yet.",
		},
		{
			name:    "tools-call-get-combo-metrics",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":"fast"}}}`,
			assert: MCPAssert{ID: float64(10), RequiredFields: []string{"result.content", "result.structuredContent.metrics"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.metrics.name": "fast", "result.structuredContent.metrics.requests": float64(10), "result.structuredContent.metrics.ok": float64(9), "result.structuredContent.metrics.errors": float64(1), "result.structuredContent.metrics.success_rate": 0.9, "result.structuredContent.metrics.total_tokens": float64(1500), "result.structuredContent.metrics.cost_usd": 0.012, "result.structuredContent.metrics.cost_known": true, "result.structuredContent.metrics.mean_latency_ms": 120.5, "result.structuredContent.metrics.latency_known": true, "result.structuredContent.metrics.latency_events": float64(9)}},
			notes: "get_combo_metrics for fast: measured mock ledger history (10 requests, 9 ok, $0.012 priced, 120.5 ms mean latency).",
		},
		{
			name:    "tools-call-explain-route",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"coder-model"}}}`,
			assert: MCPAssert{ID: float64(11), RequiredFields: []string{"result.content", "result.structuredContent.route"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.route.model": "coder-model", "result.structuredContent.route.strategy": "priority", "result.structuredContent.route.rule": "Declared order first.", "result.structuredContent.route.attempts.0.provider": "p1", "result.structuredContent.route.attempts.0.provider_name": "Coder", "result.structuredContent.route.attempts.0.model": "coder-model", "result.structuredContent.route.attempts.0.enabled": true, "result.structuredContent.route.attempts.0.price_known": true, "result.structuredContent.route.attempts.0.prompt_usd_per_1m": float64(3)}},
			notes: "explain_route for coder-model: dry run over the mock registry — p1 only (p2 does not offer the model), price known at $3.00/1M prompt tokens.",
		},
		{
			name:    "tools-call-simulate-route",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"coder-model","prompt_tokens":1000}}}`,
			assert: MCPAssert{ID: float64(12), RequiredFields: []string{"result.content", "result.structuredContent.simulation"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.simulation.model": "coder-model", "result.structuredContent.simulation.strategy": "priority", "result.structuredContent.simulation.prompt_tokens": float64(1000), "result.structuredContent.simulation.estimated_prompt_cost_usd": 0.003, "result.structuredContent.simulation.cost_known": true, "result.structuredContent.simulation.attempts.0.provider": "p1", "result.structuredContent.simulation.attempts.0.model": "coder-model", "result.structuredContent.simulation.attempts.0.on_failure": "stop", "result.structuredContent.simulation.attempts.0.price_known": true, "result.structuredContent.simulation.attempts.0.estimated_prompt_cost_usd": 0.003}},
			notes: "simulate_route for coder-model at 1000 prompt tokens: single-attempt fallback tree, first-attempt prompt cost $0.003 from the fixed $3.00/1M mock price.",
		},
		{
			name:    "tools-call-best-combo-for-task",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat","max_latency_ms":200}}}`,
			assert: MCPAssert{ID: float64(13), RequiredFields: []string{"result.content", "result.structuredContent.combo"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.combo.name": "fast", "result.structuredContent.metrics_known": true, "result.structuredContent.preferred_strategy": "fastest", "result.structuredContent.candidates_seen": float64(2), "result.structuredContent.constraints.max_latency_ms_applied": true}},
			notes: "best_combo_for_task for 'fast chat' with a 200 ms cap: fast wins on strategy match (fastest) with measured latency 120.5 ms; cheap's latency is unknown so the cap is reported not-applied for it, not treated as 0 ms.",
		},
		{
			name:    "tools-call-pick-fastest-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{}}}`,
			assert: MCPAssert{ID: float64(14), RequiredFields: []string{"result.content", "result.structuredContent.model"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.model": "coder-model", "result.structuredContent.provider": "p1", "result.structuredContent.mean_latency_ms": 120.5, "result.structuredContent.requests": float64(10), "result.structuredContent.latency_events": float64(9), "result.structuredContent.success_rate": 0.9}},
			notes: "pick_fastest_model: coder-model wins on measured mean latency (120.5 ms over 9 events); chat-model has no measured latency so it is not eligible.",
		},
		{
			name:    "tools-call-cost-report",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"cost_report","arguments":{"key_id":"key-1","days":7}}}`,
			assert: MCPAssert{ID: float64(15), RequiredFields: []string{"result.content", "result.structuredContent.cost_report"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.cost_report.key_id": "key-1", "result.structuredContent.cost_report.days": float64(7), "result.structuredContent.cost_report.window_start": "2026-11-12T00:00:00Z", "result.structuredContent.cost_report.window_end": "2026-11-19T00:00:00Z", "result.structuredContent.cost_report.requests": float64(14), "result.structuredContent.cost_report.ok": float64(13), "result.structuredContent.cost_report.errors": float64(1), "result.structuredContent.cost_report.total_tokens": float64(2100), "result.structuredContent.cost_report.cost_usd": 0.012, "result.structuredContent.cost_report.cost_known": false, "result.structuredContent.cost_report.unpriced_tokens": float64(600), "result.structuredContent.cost_report.unpriced_models.0": "chat-model", "result.structuredContent.cost_report.sampled": false, "result.structuredContent.cost_report.by_model.0.model": "coder-model", "result.structuredContent.cost_report.by_model.0.cost_usd": 0.012, "result.structuredContent.cost_report.by_model.0.price_known": true, "result.structuredContent.cost_report.by_model.1.model": "chat-model", "result.structuredContent.cost_report.by_model.1.price_known": false, "result.structuredContent.cost_report.by_model.1.unpriced_tokens": float64(600)}},
			notes: "cost_report for key-1 over 7 days: 14 requests, $0.012 priced — a LOWER BOUND, cost_known=false, because chat-model's 600 tokens are unpriced (UNKNOWN, never $0). Fixed window ending at the fixture clock.",
		},
		{
			name:    "tools-call-check-quota",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":"key-1"}}}`,
			assert: MCPAssert{ID: float64(16), RequiredFields: []string{"result.content", "result.structuredContent.quota"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.quota.key_id": "key-1", "result.structuredContent.quota.has_budget": true, "result.structuredContent.quota.limit_usd": float64(10), "result.structuredContent.quota.spent_usd": 0.012, "result.structuredContent.quota.remaining_usd": 9.988, "result.structuredContent.quota.at_limit": false, "result.structuredContent.quota.unpriced_policy": "fail_closed", "result.structuredContent.quota.active_window": "5h", "result.structuredContent.quota.active_limit_usd": float64(10), "result.structuredContent.quota.warning_reached": false, "result.structuredContent.quota.daily.limit_usd": float64(20), "result.structuredContent.quota.daily.remaining_usd": 19.988, "result.structuredContent.quota.weekly.limit_usd": float64(50), "result.structuredContent.quota.monthly.limit_usd": float64(100)}},
			notes: "check_quota for key-1: seeded $10 rolling 5-hour guard, $0.012 spent, $9.988 remaining; daily $20 / weekly $50 / monthly $100 calendar caps.",
		},
		{
			name:    "tools-call-check-quota-unknown-key",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":74,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":"key-ghost"}}}`,
			assert: MCPAssert{ID: float64(74), RequiredFields: []string{"result.content", "result.structuredContent.quota"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.quota.key_id": "key-ghost", "result.structuredContent.quota.has_budget": false, "result.structuredContent.quota.limit_usd": float64(0), "result.structuredContent.quota.spent_usd": float64(0), "result.structuredContent.quota.unpriced_policy": "fail_closed", "result.structuredContent.quota.active_window": "5h"}},
			notes: "check_quota for an unknown key (key-ghost): no guard exists, so has_budget=false — the key is reported with $0 spent and no limit, never an error; the unpriced policy stays fail_closed and the window stays 5h.",
		},
		{
			name:    "tools-call-cache-stats",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert: MCPAssert{ID: float64(17), RequiredFields: []string{"result.content", "result.structuredContent.exact"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.exact.present": true, "result.structuredContent.exact.entries": float64(3), "result.structuredContent.exact.bytes": float64(32), "result.structuredContent.semantic.present": false, "result.structuredContent.persistent.present": true, "result.structuredContent.persistent.entries": float64(5), "result.structuredContent.persistent.bytes": float64(500)}},
			notes: "cache_stats: exact (3 entries, 32 bytes) and persistent (5 entries, 500 bytes) layers seeded; semantic is not wired in this process so it reports present=false, never zero-filled.",
		},
		{
			name:    "tools-call-db-health",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"db_health","arguments":{}}}`,
			assert: MCPAssert{ID: float64(18), RequiredFields: []string{"result.content", "result.structuredContent.files"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.healthy": true, "result.structuredContent.files.0.name": "ledger.db", "result.structuredContent.files.0.exists": true, "result.structuredContent.files.0.size_bytes": float64(1234), "result.structuredContent.files.0.rows.usage_events": float64(14)}},
			notes: "db_health: the single seeded mock database ledger.db (1234 bytes, 14 usage_events) opens cleanly.",
		},
		{
			name:    "tools-call-db-stats",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"db_stats","arguments":{}}}`,
			assert: MCPAssert{ID: float64(19), RequiredFields: []string{"result.content", "result.structuredContent.files"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.files.0.name": "ledger.db", "result.structuredContent.files.0.exists": true, "result.structuredContent.files.0.size_bytes": float64(1234), "result.structuredContent.files.0.rows.usage_events": float64(14)}},
			notes: "db_stats: same per-file facts as db_health, without the verdict.",
		},
		{
			name:    "tools-call-heartbeat",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert: MCPAssert{ID: float64(20), RequiredFields: []string{"result.content", "result.structuredContent.tool_count"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.uptime_seconds": float64(-1), "result.structuredContent.tool_count": float64(32), "result.structuredContent.protocol_version": "2025-03-26", "result.structuredContent.audit_log": "disabled"}},
			notes: "heartbeat: liveness snapshot — uptime -1 (no serving loop), 32 announced tools, protocol 2025-03-26, audit log disabled (not wired in the fixture server).",
		},
		{
			name:    "tools-call-tool-search",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":"combo"}}}`,
			assert: MCPAssert{ID: float64(21), RequiredFields: []string{"result.content", "result.structuredContent.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.query": "combo", "result.structuredContent.count": float64(7), "result.structuredContent.tools.0.name": "best_combo_for_task", "result.structuredContent.tools.6.name": "switch_combos"}},
			notes: "tool_search for 'combo': 7 hits (name or description contains 'combo'), in Tools() order.",
		},
		{
			name:    "tools-call-memory-search",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"memory_search","arguments":{"query":"cache"}}}`,
			assert: MCPAssert{ID: float64(22), RequiredFields: []string{"result.content", "result.structuredContent.entries"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.count": float64(2), "result.structuredContent.entries.0.id": "mem-2", "result.structuredContent.entries.0.matches": float64(1), "result.structuredContent.entries.1.id": "mem-1", "result.structuredContent.entries.1.matches": float64(1)}},
			notes: "memory_search for 'cache': both seeded local-operator memories match once (occurrence count, not a relevance score); equal counts rank the later entry first.",
		},
		{
			name:    "error-best-combo-empty-task",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{}}}`,
			assert: MCPAssert{ID: float64(23), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "task: required"}},
			notes: "best_combo_for_task without a task is invalid params (-32602).",
		},
		{
			name:    "error-check-quota-missing-key",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"check_quota","arguments":{}}}`,
			assert: MCPAssert{ID: float64(32), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "key_id: required"}},
			notes: "check_quota without a key_id is invalid params (-32602): the local operator must name the key (a gateway-key caller would default to its own id).",
		},
		{
			name:    "error-cost-report-days",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"cost_report","arguments":{"key_id":"key-1","days":400}}}`,
			assert: MCPAssert{ID: float64(25), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "days: must be between 0 (all time) and 365"}},
			notes: "cost_report with days=400 (max 365) is invalid params (-32602).",
		},
		{
			name:    "error-create-combo-ghost-member",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"ghost-combo","members":["ghost"],"strategy":"priority"}}}`,
			assert: MCPAssert{ID: float64(26), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown provider \"ghost\""}},
			notes: "create_combo with an unconfigured member is invalid params (-32602); members must be configured providers.",
		},
		{
			name:    "error-explain-route-unknown-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"ghost-model"}}}`,
			assert: MCPAssert{ID: float64(27), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown model \"ghost-model\""}},
			notes: "explain_route for a model no provider offers is invalid params (-32602).",
		},
		{
			name:    "error-get-combo-metrics-unknown-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":"ghost"}}}`,
			assert: MCPAssert{ID: float64(28), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown combo \"ghost\""}},
			notes: "get_combo_metrics for an unknown combo is invalid params (-32602).",
		},
		{
			name:    "error-memory-add-empty-text",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":""}}}`,
			assert: MCPAssert{ID: float64(29), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "memory_add requires a non-empty string argument \"text\""}},
			notes: "memory_add with empty text is invalid params (-32602).",
		},
		{
			name:    "error-memory-add-rune-limit",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":30,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"` + strings.Repeat("a", 10001) + `"}}}`,
			assert: MCPAssert{ID: float64(30), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: memory text is 10001 characters, over the 10000 limit"}},
			notes: "memory_add with 10,001 runes exceeds the 10,000-rune limit and is refused, never silently truncated (-32602). The mock wraps the refusal with mcp.ErrInvalidInput exactly like the real store, so opsStoreErr returns the validation message as-is (never the generic store-unavailable message).",
		},
		{
			name:    "error-memory-search-missing-query",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":31,"method":"tools/call","params":{"name":"memory_search","arguments":{}}}`,
			assert: MCPAssert{ID: float64(31), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "memory_search requires a non-empty string argument \"query\""}},
			notes: "memory_search without a query is invalid params (-32602).",
		},
		{
			name:    "error-pick-fastest-unknown-provider",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":32,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{"provider":"ghost"}}}`,
			assert: MCPAssert{ID: float64(32), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "no measured model latency for provider \"ghost\" (need at least 1 successful events with a recorded latency)"}},
			notes: "pick_fastest_model restricted to a provider with no measured latency errors honestly instead of guessing (-32602).",
		},
		{
			name:    "error-set-budget-guard-bad-policy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":33,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"key-1","limit_usd":5,"unpriced_policy":"ignore_it"}}}`,
			assert: MCPAssert{ID: float64(33), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown unpriced_policy \"ignore_it\" (want fail_closed or count_as_zero)"}},
			notes: "set_budget_guard with an unknown unpriced_policy is invalid params (-32602).",
		},
		{
			name:    "error-set-routing-strategy-unknown",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":34,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":"bogus-strategy"}}}`,
			assert: MCPAssert{ID: float64(34), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown strategy \"bogus-strategy\""}},
			notes: "set_routing_strategy with an unknown strategy is invalid params (-32602).",
		},
		{
			name:    "error-simulate-route-unknown-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":35,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"ghost-model"}}}`,
			assert: MCPAssert{ID: float64(35), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown model \"ghost-model\""}},
			notes: "simulate_route for a model no provider offers is invalid params (-32602).",
		},
		{
			name:    "error-switch-combos-unknown-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":36,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"ghost"}}}`,
			assert: MCPAssert{ID: float64(36), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown combo \"ghost\""}},
			notes: "switch_combos to an unknown combo is invalid params (-32602).",
		},
		{
			name:    "error-tool-search-nonstring-query",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":37,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":123}}}`,
			assert: MCPAssert{ID: float64(37), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "tool_search: query must be a string"}},
			notes: "tool_search with a non-string query is invalid params (-32602).",
		},
		// P1-6 error-path fixtures: the primary error branch per tool
		// over stdio as the local operator. Every one is
		// a -32602 invalid-params refusal from the handler or the
		// mock Source; none mutates state, so they sit in the
		// error block of the session order.
		{
			name:    "error-create-combo-missing-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":45,"method":"tools/call","params":{"name":"create_combo","arguments":{"members":["p1"]}}}`,
			assert: MCPAssert{ID: float64(45), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: required"}},
			notes: "create_combo without a name is invalid params (-32602).",
		},
		{
			name:    "error-create-combo-missing-members",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":46,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"nogroup"}}}`,
			assert: MCPAssert{ID: float64(46), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "members: required"}},
			notes: "create_combo without members is invalid params (-32602): a combo needs at least one member.",
		},
		{
			name:    "error-create-combo-members-nonstring",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":47,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"nogroup","members":["p1",123]}}}`,
			assert: MCPAssert{ID: float64(47), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "members[1]: must be a string"}},
			notes: "create_combo with a non-string member is invalid params (-32602): members are provider ids, never coerced from numbers.",
		},
		{
			name:    "error-create-combo-duplicate-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":48,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"fast","members":["p1"],"strategy":"priority"}}}`,
			assert: MCPAssert{ID: float64(48), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: combo \"fast\" already exists"}},
			notes: "create_combo with the name of a seeded combo is invalid params (-32602): combo names are unique, never silently overwritten.",
		},
		{
			name:    "error-create-combo-bad-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":49,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"a:b","members":["p1"],"strategy":"priority"}}}`,
			assert: MCPAssert{ID: float64(49), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: combo name \"a:b\" must not contain \":\" or \"/\""}},
			notes: "create_combo with \":\" in the name is invalid params (-32602): the routing-group name rule forbids \":\" and \"/\".",
		},
		{
			name:    "error-create-combo-unknown-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":50,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"newcombo","members":["p1"],"strategy":"bogus"}}}`,
			assert: MCPAssert{ID: float64(50), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown strategy \"bogus\""}},
			notes: "create_combo with an unknown strategy is invalid params (-32602) — the same vocabulary check as set_routing_strategy, on the create path.",
		},
		{
			name:    "error-switch-combos-missing-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":51,"method":"tools/call","params":{"name":"switch_combos","arguments":{}}}`,
			assert: MCPAssert{ID: float64(51), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: required"}},
			notes: "switch_combos without a name is invalid params (-32602).",
		},
		{
			name:    "error-switch-combos-active-false",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":52,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"fast","active":false}}}`,
			assert: MCPAssert{ID: float64(52), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: combo \"fast\" is not the active combo"}},
			notes: "switch_combos with active=false for a combo that is not active is invalid params (-32602): clearing only ever clears the active combo, never a bystander.",
		},
		{
			name:    "error-switch-combos-active-nonbool",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":53,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"fast","active":"yes"}}}`,
			assert: MCPAssert{ID: float64(53), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "active: must be a boolean"}},
			notes: "switch_combos with a non-boolean active flag is invalid params (-32602): the flag is never coerced from a string.",
		},
		{
			name:    "error-get-combo-metrics-missing-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":54,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{}}}`,
			assert: MCPAssert{ID: float64(54), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: required"}},
			notes: "get_combo_metrics without a name is invalid params (-32602).",
		},
		{
			name:    "error-set-routing-strategy-missing-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":55,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{}}}`,
			assert: MCPAssert{ID: float64(55), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "strategy: required"}},
			notes: "set_routing_strategy without a strategy is invalid params (-32602).",
		},
		{
			name:    "error-best-combo-negative-max-cost",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":56,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat","max_cost_usd":-1}}}`,
			assert: MCPAssert{ID: float64(56), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "max_cost_usd: must be >= 0"}},
			notes: "best_combo_for_task with a negative cost cap is invalid params (-32602): 0 is the no-cap sentinel, a negative cap is meaningless.",
		},
		{
			name:    "error-best-combo-negative-max-latency",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":57,"method":"tools/call","params":{"name":"best_combo_for_task","arguments":{"task":"fast chat","max_latency_ms":-5}}}`,
			assert: MCPAssert{ID: float64(57), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "max_latency_ms: must be >= 0"}},
			notes: "best_combo_for_task with a negative latency cap is invalid params (-32602): 0 is the no-cap sentinel, a negative cap is meaningless.",
		},
		{
			name:    "error-explain-route-missing-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":58,"method":"tools/call","params":{"name":"explain_route","arguments":{}}}`,
			assert: MCPAssert{ID: float64(58), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: required"}},
			notes: "explain_route without a model is invalid params (-32602).",
		},
		{
			name:    "error-explain-route-unknown-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":59,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"coder-model","combo":"ghost"}}}`,
			assert: MCPAssert{ID: float64(59), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown combo \"ghost\""}},
			notes: "explain_route restricted to an unknown combo is invalid params (-32602): the combo is resolved before the attempt order is filtered.",
		},
		{
			name:    "error-explain-route-combo-no-provider",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":60,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"chat-model","combo":"fast"}}}`,
			assert: MCPAssert{ID: float64(60), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: combo \"fast\" offers no provider for this model"}},
			notes: "explain_route for chat-model restricted to combo fast is invalid params (-32602): fast's only member p1 does not offer chat-model, so the restricted order would be empty — reported, never an empty order.",
		},
		{
			name:    "error-simulate-route-missing-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":61,"method":"tools/call","params":{"name":"simulate_route","arguments":{}}}`,
			assert: MCPAssert{ID: float64(61), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: required"}},
			notes: "simulate_route without a model is invalid params (-32602).",
		},
		{
			name:    "error-simulate-route-negative-tokens",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":62,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"coder-model","prompt_tokens":-1}}}`,
			assert: MCPAssert{ID: float64(62), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "prompt_tokens: must be >= 0"}},
			notes: "simulate_route with negative prompt_tokens is invalid params (-32602): a prompt cannot have a negative length.",
		},
		{
			name:    "error-simulate-route-noninteger-tokens",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":63,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"coder-model","prompt_tokens":1.5}}}`,
			assert: MCPAssert{ID: float64(63), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "prompt_tokens: must be an integer"}},
			notes: "simulate_route with a fractional prompt_tokens is invalid params (-32602): token counts are integers, never rounded silently.",
		},
		{
			name:    "error-simulate-route-unknown-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":64,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"coder-model","combo":"ghost"}}}`,
			assert: MCPAssert{ID: float64(64), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: unknown combo \"ghost\""}},
			notes: "simulate_route restricted to an unknown combo is invalid params (-32602) — the same combo resolution as explain_route, on the simulate path.",
		},
		{
			name:    "error-simulate-route-combo-no-provider",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":65,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"chat-model","combo":"fast"}}}`,
			assert: MCPAssert{ID: float64(65), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "mcp: invalid input: combo \"fast\" offers no provider for model \"chat-model\""}},
			notes: "simulate_route for chat-model restricted to combo fast is invalid params (-32602): no kept attempt, so there is no fallback tree to price — reported, never an empty tree.",
		},
		{
			name:    "error-cost-report-noninteger-days",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":66,"method":"tools/call","params":{"name":"cost_report","arguments":{"key_id":"key-1","days":1.5}}}`,
			assert: MCPAssert{ID: float64(66), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "days: must be an integer"}},
			notes: "cost_report with fractional days is invalid params (-32602): windows are whole days (the 0–365 range check is the separate, already-pinned branch).",
		},
		{
			name:    "error-check-quota-nonstring-key",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":67,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":123}}}`,
			assert: MCPAssert{ID: float64(67), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "key_id: must be a string"}},
			notes: "check_quota with a non-string key_id is invalid params (-32602): key ids are strings, never coerced from numbers.",
		},
		{
			name:    "error-set-budget-guard-missing-key",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":68,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"limit_usd":5}}}`,
			assert: MCPAssert{ID: float64(68), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "key_id: required"}},
			notes: "set_budget_guard without a key_id is invalid params (-32602): the local operator must name the key (a gateway-key caller would default to its own id).",
		},
		{
			name:    "error-set-budget-guard-missing-limit",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":69,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"key-1"}}}`,
			assert: MCPAssert{ID: float64(69), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "limit_usd: required"}},
			notes: "set_budget_guard without limit_usd is invalid params (-32602): the rolling cap is required, never defaulted.",
		},
		{
			name:    "error-set-budget-guard-negative-limit",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":70,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"key-1","limit_usd":-5}}}`,
			assert: MCPAssert{ID: float64(70), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "limit_usd: must be >= 0"}},
			notes: "set_budget_guard with a negative rolling cap is invalid params (-32602): 0 means no spend allowed, a negative cap is meaningless.",
		},
		{
			name:    "error-set-budget-guard-negative-daily",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":71,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"key-1","limit_usd":5,"daily_limit_usd":-10}}}`,
			assert: MCPAssert{ID: float64(71), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "daily_limit_usd: must be >= 0"}},
			notes: "set_budget_guard with a negative daily cap is invalid params (-32602): the calendar caps share the >= 0 rule (daily pinned here as the representative cap).",
		},
		{
			name:    "error-pick-fastest-min-requests-zero",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":72,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{"min_requests":0}}}`,
			assert: MCPAssert{ID: float64(72), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "min_requests: must be >= 1"}},
			notes: "pick_fastest_model with min_requests=0 is invalid params (-32602): eligibility needs at least one measured request.",
		},
		{
			name:    "error-pick-fastest-min-requests-high",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":73,"method":"tools/call","params":{"name":"pick_fastest_model","arguments":{"min_requests":100}}}`,
			assert: MCPAssert{ID: float64(73), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "no measured model latency yet (need at least 100 successful events with a recorded latency)"}},
			notes: "pick_fastest_model with min_requests=100 errors honestly (-32602) instead of guessing: no mock model has 100 measured requests, so nothing is eligible.",
		},
		// WT-01..WT-16 (GAP-TRACK §12): wrong-type/empty pins for
		// the same validators the P1-6 fixtures above cover only
		// on their primary branch. Each asserts the exact
		// refusal message, not just -32602.
		{
			name:    "error-create-combo-nonstring-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":75,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":42,"members":["p1"]}}}`,
			assert: MCPAssert{ID: float64(75), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: must be a string"}},
			notes: "WT-01: create_combo with a non-string name is invalid params (-32602): combo names are strings, never coerced from numbers.",
		},
		{
			name:    "error-create-combo-members-empty-element",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":76,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"nogroup","members":["p1",""]}}}`,
			assert: MCPAssert{ID: float64(76), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "members[1]: must not be empty"}},
			notes: "WT-02: create_combo with an empty members element is invalid params (-32602): every member must be a non-empty provider id.",
		},
		{
			name:    "error-create-combo-nonstring-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":77,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"nogroup","members":["p1"],"strategy":42}}}`,
			assert: MCPAssert{ID: float64(77), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "strategy: must be a string"}},
			notes: "WT-03: create_combo with a non-string strategy is invalid params (-32602): strategies are named strings, never coerced from numbers.",
		},
		{
			name:    "error-switch-combos-nonstring-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":78,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":42}}}`,
			assert: MCPAssert{ID: float64(78), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: must be a string"}},
			notes: "WT-04: switch_combos with a non-string name is invalid params (-32602): combo names are strings, never coerced from numbers.",
		},
		{
			name:    "error-switch-combos-empty-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":79,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":""}}}`,
			assert: MCPAssert{ID: float64(79), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: must not be empty"}},
			notes: "WT-05: switch_combos with an empty name is invalid params (-32602): the missing-name fixture pins only the absent key, not the empty string.",
		},
		{
			name:    "error-get-combo-metrics-nonstring-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":80,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":42}}}`,
			assert: MCPAssert{ID: float64(80), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: must be a string"}},
			notes: "WT-06: get_combo_metrics with a non-string name is invalid params (-32602): combo names are strings, never coerced from numbers.",
		},
		{
			name:    "error-get-combo-metrics-empty-name",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":81,"method":"tools/call","params":{"name":"get_combo_metrics","arguments":{"name":""}}}`,
			assert: MCPAssert{ID: float64(81), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "name: must not be empty"}},
			notes: "WT-07: get_combo_metrics with an empty name is invalid params (-32602): the missing-name fixture pins only the absent key, not the empty string.",
		},
		{
			name:    "error-set-routing-strategy-nonstring-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":82,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":42}}}`,
			assert: MCPAssert{ID: float64(82), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "strategy: must be a string"}},
			notes: "WT-08: set_routing_strategy with a non-string strategy is invalid params (-32602): strategies are named strings, never coerced from numbers.",
		},
		{
			name:    "error-set-routing-strategy-empty-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":83,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":""}}}`,
			assert: MCPAssert{ID: float64(83), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "strategy: must not be empty"}},
			notes: "WT-09: set_routing_strategy with an empty strategy is invalid params (-32602): the missing-strategy fixture pins only the absent key, not the empty string.",
		},
		{
			name:    "error-set-routing-strategy-nonstring-chain",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":84,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":"fastest","chain":42}}}`,
			assert: MCPAssert{ID: float64(84), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "chain: must be a string"}},
			notes: "WT-10: set_routing_strategy with a non-string chain is invalid params (-32602): chains are model ids or combo names as strings, never coerced from numbers.",
		},
		{
			name:    "error-explain-route-nonstring-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":85,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":42}}}`,
			assert: MCPAssert{ID: float64(85), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: must be a string"}},
			notes: "WT-11: explain_route with a non-string model is invalid params (-32602): models are named strings, never coerced from numbers.",
		},
		{
			name:    "error-explain-route-empty-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":86,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":""}}}`,
			assert: MCPAssert{ID: float64(86), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: must not be empty"}},
			notes: "WT-12: explain_route with an empty model is invalid params (-32602): the missing-model fixture pins only the absent key, not the empty string.",
		},
		{
			name:    "error-explain-route-nonstring-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":87,"method":"tools/call","params":{"name":"explain_route","arguments":{"model":"coder-model","combo":42}}}`,
			assert: MCPAssert{ID: float64(87), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "combo: must be a string"}},
			notes: "WT-13: explain_route with a non-string combo is invalid params (-32602): combos are named strings, never coerced from numbers.",
		},
		{
			name:    "error-simulate-route-nonstring-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":88,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":42}}}`,
			assert: MCPAssert{ID: float64(88), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: must be a string"}},
			notes: "WT-14: simulate_route with a non-string model is invalid params (-32602): models are named strings, never coerced from numbers.",
		},
		{
			name:    "error-simulate-route-empty-model",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":89,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":""}}}`,
			assert: MCPAssert{ID: float64(89), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "model: must not be empty"}},
			notes: "WT-15: simulate_route with an empty model is invalid params (-32602): the missing-model fixture pins only the absent key, not the empty string.",
		},
		{
			name:    "error-simulate-route-nonstring-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":90,"method":"tools/call","params":{"name":"simulate_route","arguments":{"model":"coder-model","combo":42}}}`,
			assert: MCPAssert{ID: float64(90), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "combo: must be a string"}},
			notes: "WT-16: simulate_route with a non-string combo is invalid params (-32602): combos are named strings, never coerced from numbers.",
		},
		{
			name:    "tools-call-create-combo",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":38,"method":"tools/call","params":{"name":"create_combo","arguments":{"name":"balanced","members":["p1","p2"],"strategy":"priority"}}}`,
			assert: MCPAssert{ID: float64(38), RequiredFields: []string{"result.content", "result.structuredContent.combo"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.created": true, "result.structuredContent.combo.name": "balanced", "result.structuredContent.combo.members.0": "p1", "result.structuredContent.combo.members.1": "p2", "result.structuredContent.combo.strategy": "priority", "result.structuredContent.combo.usable": false}},
			notes: "create_combo 'balanced' (p1 + p2, priority): stored and read back; usable=false because p2 is disabled. Runs after all reads so the session's list_combos still sees the 2 seeded combos.",
		},
		{
			name:    "tools-call-switch-combos",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":39,"method":"tools/call","params":{"name":"switch_combos","arguments":{"name":"cheap"}}}`,
			assert: MCPAssert{ID: float64(39), RequiredFields: []string{"result.content", "result.structuredContent.active_combo"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.active_combo": "cheap"}},
			notes: "switch_combos to cheap: sets the session-active combo only — the serving router has no single active combo, so this never changes what it routes.",
		},
		{
			name:    "tools-call-set-routing-strategy",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":40,"method":"tools/call","params":{"name":"set_routing_strategy","arguments":{"strategy":"fastest"}}}`,
			assert: MCPAssert{ID: float64(40), RequiredFields: []string{"result.content", "result.structuredContent.strategy"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.chain": "", "result.structuredContent.strategy.name": "fastest", "result.structuredContent.strategy.scope": "global", "result.structuredContent.previous.name": "priority"}},
			notes: "set_routing_strategy to fastest (global): replies with the stored strategy and the previous one (priority). Runs after explain/simulate so the session still sees priority there.",
		},
		{
			name:    "tools-call-set-budget-guard",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":41,"method":"tools/call","params":{"name":"set_budget_guard","arguments":{"key_id":"key-1","limit_usd":5,"unpriced_policy":"count_as_zero","daily_limit_usd":10}}}`,
			assert: MCPAssert{ID: float64(41), RequiredFields: []string{"result.content", "result.structuredContent.quota"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.guard_set": true, "result.structuredContent.quota.key_id": "key-1", "result.structuredContent.quota.has_budget": true, "result.structuredContent.quota.limit_usd": float64(5), "result.structuredContent.quota.spent_usd": 0.012, "result.structuredContent.quota.remaining_usd": 4.988, "result.structuredContent.quota.unpriced_policy": "count_as_zero", "result.structuredContent.quota.daily.limit_usd": float64(10), "result.structuredContent.quota.daily.remaining_usd": 9.988}},
			notes: "set_budget_guard for key-1 ($5 rolling cap, count_as_zero, $10 daily): the reply re-reads the stored guard, not the request echo. Runs after check_quota so the session still sees the seeded $10 guard there.",
		},
		{
			name:    "tools-call-memory-clear",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{"name":"memory_clear","arguments":{}}}`,
			assert: MCPAssert{ID: float64(42), RequiredFields: []string{"result.content", "result.structuredContent.cleared"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.cleared": float64(2), "result.structuredContent.caller": "local-operator", "result.structuredContent.scope": "caller-only"}},
			notes: "memory_clear: deletes the caller's own 2 seeded memories (local-operator tenant only). Runs before memory_add so the cleared count is the seeded 2 in both isolation and session.",
		},
		{
			name:    "tools-call-memory-add",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":43,"method":"tools/call","params":{"name":"memory_add","arguments":{"text":"The router caches repeated prompts."}}}`,
			assert: MCPAssert{ID: float64(43), RequiredFields: []string{"result.content", "result.structuredContent.id"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.id": "mem-3", "result.structuredContent.text": "The router caches repeated prompts.", "result.structuredContent.created_at": "2026-11-19T12:00:00Z"}},
			notes: "memory_add: stores one memory under the local-operator namespace; id mem-3 follows the seeded mem-1/mem-2 (counter-based, so it is the same after a clear).",
		},
		{
			name:    "tools-call-cache-flush",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":44,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`,
			assert: MCPAssert{ID: float64(44), RequiredFields: []string{"result.content", "result.structuredContent.flushed"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.flushed": float64(10), "result.structuredContent.tenant": "local-operator", "result.structuredContent.scope": "caller-only"}},
			notes: "cache_flush: evicts the caller's own 10 seeded entries (local-operator tenant only); other tenants untouched. Runs last so cache_stats still sees the seeded counts.",
		},
		{
			name:    "ping",
			method:  "ping",
			request: `{"jsonrpc":"2.0","id":6,"method":"ping"}`,
			assert:  MCPAssert{ID: float64(6), RequiredFields: []string{"result"}, JSONPaths: map[string]any{"jsonrpc": "2.0"}},
			notes:   "ping answers an empty result object.",
		},
		{
			name:    "error-unknown-tool",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"chat","arguments":{}}}`,
			assert: MCPAssert{ID: float64(7), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: chat"}},
			notes: "Unknown tool name is a JSON-RPC -32602 (invalid params), not a result.",
		},
		{
			name:    "error-unknown-method",
			method:  "resources/list",
			request: `{"jsonrpc":"2.0","id":8,"method":"resources/list"}`,
			assert: MCPAssert{ID: float64(8), ExpectError: true, ErrorCode: &err32601, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32601), "error.message": "method not found: resources/list"}},
			notes: "Unknown method is a JSON-RPC -32601 (method not found).",
		},
		// P2-8 filtered-list fixtures (audit finding F4): the
		// cardinality filter pinned per group. Every group pins
		// the visible set on tools/list — by index (every visible
		// name, last index included) plus a json_lengths pin on
		// result.tools — and a tools/call to a filtered-out tool,
		// which must answer -32602 unknown tool exactly as a tool
		// that was never registered. Only deny-2 pins the count
		// three ways (tools/list + tool_search count + heartbeat
		// tool_count): in allow-3, max-5 and allow-deny,
		// tool_search and heartbeat are themselves hidden by the
		// filter, so calling them is a -32602 unknown-tool error
		// fixture instead — itself a filter proof, since a hidden
		// tool is uncallable, not just unlisted. Each spec records
		// against its group's filtered server (filterGroup); the
		// runner replays it against the same filter.
		{
			name:    "tools-list-allow-3",
			method:  "tools/list",
			request: `{"jsonrpc":"2.0","id":101,"method":"tools/list"}`,
			assert: MCPAssert{ID: float64(101), RequiredFields: []string{"result.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0",
					"result.tools.0.name": "router_status", "result.tools.0.inputSchema": nil,
					"result.tools.1.name": "provider_list", "result.tools.1.inputSchema": nil,
					"result.tools.2.name": "model_list", "result.tools.2.inputSchema": nil},
				JSONLengths: map[string]int{"result.tools": 3}},
			notes:       "tools/list under an exclusive allow-list of the 3 built-ins: only router_status, provider_list, model_list are announced — the other 21 tools are invisible, not listed-then-denied. The json_lengths pin (result.tools = 3) fails if a 4th tool leaks in or one goes missing.",
			filterGroup: "allow-3",
		},
		{
			name:    "error-call-tool-search-allow-3",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":102,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":""}}}`,
			assert: MCPAssert{ID: float64(102), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: tool_search"}},
			notes:       "tool_search is itself hidden by the allow-3 filter (only the 3 built-ins are allow-listed), so calling it is -32602 unknown tool — a filtered-out tool is uncallable, not just unlisted. The filtered count (3) is pinned on tools-list-allow-3 by index + json_lengths, not here.",
			filterGroup: "allow-3",
		},
		{
			name:    "error-call-heartbeat-allow-3",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":103,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert: MCPAssert{ID: float64(103), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: heartbeat"}},
			notes:       "heartbeat is itself hidden by the allow-3 filter (only the 3 built-ins are allow-listed), so calling it is -32602 unknown tool — a filtered-out tool is uncallable, not just unlisted. The filtered count (3) is pinned on tools-list-allow-3 by index + json_lengths, not here.",
			filterGroup: "allow-3",
		},
		{
			name:    "error-call-filtered-tool-allow-3",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":104,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert: MCPAssert{ID: float64(104), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: cache_stats"}},
			notes:       "Calling cache_stats under the allow-3 filter is -32602 unknown tool — a filtered-out tool is indistinguishable from one that was never registered, not a scope denial (-32002).",
			filterGroup: "allow-3",
		},
		{
			name:    "tools-list-max-5",
			method:  "tools/list",
			request: `{"jsonrpc":"2.0","id":105,"method":"tools/list"}`,
			assert: MCPAssert{ID: float64(105), RequiredFields: []string{"result.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0",
					"result.tools.0.name": "router_status", "result.tools.0.inputSchema": nil,
					"result.tools.1.name": "provider_list", "result.tools.1.inputSchema": nil,
					"result.tools.2.name": "model_list", "result.tools.2.inputSchema": nil,
					"result.tools.3.name": "best_combo_for_task", "result.tools.3.inputSchema": nil,
					"result.tools.4.name": "cache_flush", "result.tools.4.inputSchema": nil},
				JSONLengths: map[string]int{"result.tools": 5}},
			notes:       "tools/list under a MaxTools cap of 5: the first 5 in Tools() order (builtins first, then alphabetical) — the cap keeps the head of the list, it never reorders. The json_lengths pin (result.tools = 5) fails if the cap lets a 6th tool through or drops one.",
			filterGroup: "max-5",
		},
		{
			name:    "error-call-tool-search-max-5",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":106,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":""}}}`,
			assert: MCPAssert{ID: float64(106), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: tool_search"}},
			notes:       "tool_search sits past the MaxTools-5 cap (last in Tools() order), so calling it is -32602 unknown tool — the cap hides the tail of the list, not just its announcement. The capped count (5) is pinned on tools-list-max-5 by index + json_lengths, not here.",
			filterGroup: "max-5",
		},
		{
			name:    "error-call-heartbeat-max-5",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":107,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert: MCPAssert{ID: float64(107), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: heartbeat"}},
			notes:       "heartbeat sits past the MaxTools-5 cap, so calling it is -32602 unknown tool — the cap hides the tail of the list, not just its announcement. The capped count (5) is pinned on tools-list-max-5 by index + json_lengths, not here.",
			filterGroup: "max-5",
		},
		{
			name:    "error-call-filtered-tool-max-5",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":108,"method":"tools/call","params":{"name":"check_quota","arguments":{"key_id":"key-1"}}}`,
			assert: MCPAssert{ID: float64(108), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: check_quota"}},
			notes:       "Calling check_quota (6th in Tools() order, past the MaxTools-5 cap) is -32602 unknown tool — the cap hides the tail of the list, not just its announcement.",
			filterGroup: "max-5",
		},
		{
			name:    "tools-list-deny-2",
			method:  "tools/list",
			request: `{"jsonrpc":"2.0","id":109,"method":"tools/list"}`,
			assert: MCPAssert{ID: float64(109), RequiredFields: []string{"result.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0",
					"result.tools.0.name": "router_status", "result.tools.0.inputSchema": nil,
					"result.tools.1.name": "provider_list", "result.tools.1.inputSchema": nil,
					"result.tools.2.name": "model_list", "result.tools.2.inputSchema": nil,
					"result.tools.3.name": "best_combo_for_task", "result.tools.3.inputSchema": nil,
					"result.tools.4.name": "cache_flush", "result.tools.4.inputSchema": nil,
					"result.tools.5.name": "check_quota", "result.tools.5.inputSchema": nil,
					"result.tools.8.name": "db_health", "result.tools.8.inputSchema": nil,
					"result.tools.9.name": "explain_route", "result.tools.9.inputSchema": nil,
					"result.tools.21.name": "tool_search", "result.tools.21.inputSchema": nil},
				JSONLengths: map[string]int{"result.tools": 22}},
			notes:       "tools/list with cache_stats and db_stats denied: 22 tools remain and the survivors shift up — check_quota moves to index 5 (was 6), db_health to 8 (was 9), tool_search is last at 21 (was 23). The denied tools leave no gap. The json_lengths pin (result.tools = 22) fails if a denied tool leaks back in or a survivor goes missing.",
			filterGroup: "deny-2",
		},
		{
			name:    "tools-call-tool-search-deny-2",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":114,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":""}}}`,
			assert: MCPAssert{ID: float64(114), RequiredFields: []string{"result.content", "result.structuredContent.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.query": "", "result.structuredContent.count": float64(22), "result.structuredContent.tools.0.name": "router_status", "result.structuredContent.tools.21.name": "tool_search"}},
			notes:       "tool_search with an empty query under the deny-2 filter: count 22 — the search agrees with the filtered tools/list. tool_search itself survives this filter, so the three-way pin runs in this group only.",
			filterGroup: "deny-2",
		},
		{
			name:    "tools-call-heartbeat-deny-2",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":115,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`,
			assert: MCPAssert{ID: float64(115), RequiredFields: []string{"result.content", "result.structuredContent.tool_count"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "result.structuredContent.uptime_seconds": float64(-1), "result.structuredContent.tool_count": float64(22), "result.structuredContent.protocol_version": "2025-03-26", "result.structuredContent.audit_log": "disabled"}},
			notes:       "heartbeat under the deny-2 filter: tool_count 22 — the liveness snapshot agrees with the filtered tools/list.",
			filterGroup: "deny-2",
		},
		{
			name:    "error-call-filtered-tool-deny-2",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":110,"method":"tools/call","params":{"name":"db_stats","arguments":{}}}`,
			assert: MCPAssert{ID: float64(110), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: db_stats"}},
			notes:       "Calling denied db_stats is -32602 unknown tool — a deny-listed tool is invisible, never merely denied on call.",
			filterGroup: "deny-2",
		},
		{
			name:    "tools-list-allow-deny-conflict",
			method:  "tools/list",
			request: `{"jsonrpc":"2.0","id":111,"method":"tools/list"}`,
			assert: MCPAssert{ID: float64(111), RequiredFields: []string{"result.tools"},
				JSONPaths: map[string]any{"jsonrpc": "2.0",
					"result.tools.0.name": "router_status", "result.tools.0.inputSchema": nil},
				JSONLengths: map[string]int{"result.tools": 1}},
			notes:       "tools/list when cache_stats is both allow-listed and denied: deny wins — only router_status is announced. The conflict resolves to invisible, never to visible. The json_lengths pin (result.tools = 1) fails if cache_stats leaks in.",
			filterGroup: "allow-deny",
		},
		{
			name:    "error-call-tool-search-allow-deny",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":112,"method":"tools/call","params":{"name":"tool_search","arguments":{"query":""}}}`,
			assert: MCPAssert{ID: float64(112), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: tool_search"}},
			notes:       "tool_search is not in the allow-deny conflict's allow-list (only router_status survives), so calling it is -32602 unknown tool — a filtered-out tool is uncallable, not just unlisted. The conflict-resolved count (1) is pinned on tools-list-allow-deny-conflict by index + json_lengths, not here.",
			filterGroup: "allow-deny",
		},
		{
			name:    "error-call-filtered-tool-allow-deny",
			method:  "tools/call",
			request: `{"jsonrpc":"2.0","id":113,"method":"tools/call","params":{"name":"cache_stats","arguments":{}}}`,
			assert: MCPAssert{ID: float64(113), ExpectError: true, ErrorCode: &err32602, RequiredFields: []string{"error.message"},
				JSONPaths: map[string]any{"jsonrpc": "2.0", "error.code": float64(-32602), "error.message": "unknown tool: cache_stats"}},
			notes:       "Calling cache_stats when it is both allow-listed and denied is -32602 unknown tool — the deny side of the conflict governs the call as well as the list.",
			filterGroup: "allow-deny",
		},
	}
}

// mcpFilterGroupDescription is the one-line filter definition
// recorded in a filtered fixture's recorded_from note, so the
// file states which server produced it.
func mcpFilterGroupDescription(group string) string {
	switch group {
	case "allow-3":
		return "allow-list router_status, provider_list, model_list"
	case "max-5":
		return "MaxTools cap of 5"
	case "deny-2":
		return "deny-list cache_stats, db_stats"
	case "allow-deny":
		return "allow-list router_status, cache_stats with cache_stats denied (deny wins)"
	default:
		return "unknown filter group " + group
	}
}

func TestRecordMCPFixtures(t *testing.T) {
	if os.Getenv("CODVIAN_RECORD_MCP_FIXTURES") != "1" {
		t.Skip("recorder only: set CODVIAN_RECORD_MCP_FIXTURES=1 to re-record fixtures")
	}
	for _, spec := range mcpRecordSpecs() {
		srv := newMockMCPServer()
		if spec.filterGroup != "" {
			srv = newFilteredMockMCPServer(spec.filterGroup)
		}
		responses := runMCPExchanges(t, srv, [][]byte{[]byte(spec.request)})
		if len(responses) != 1 {
			t.Fatalf("record %s: server wrote %d response lines, want 1", spec.name, len(responses))
		}
		// Prove the assertions match the captured response BEFORE
		// writing anything: a hand-written assert that disagrees
		// with reality fails here instead of landing in the file.
		recordedFrom := "recorded 2026-11-19 by running the real internal/mcp Server (mock-backed deterministic Source: 2 mock providers, 2 mock models, seeded combos fast/cheap, fixed ledger cells with coder-model priced at $3.00/1M and chat-model unpriced, key-1 $10 guard, 10 local-operator cache entries, ledger.db, 2 seeded memories; fixed clock 2026-11-19T12:00:00Z) in-process over stdio via mcp.Serve; response captured verbatim. Mock data only, no live provider call."
		if spec.filterGroup != "" {
			recordedFrom += " Filtered server (" + spec.filterGroup + "): " + mcpFilterGroupDescription(spec.filterGroup) + "."
		}
		f := MCPFixture{
			SchemaVersion: 1, Slice: "m9.0", Name: spec.name, IsFixture: true,
			RecordedFrom:     recordedFrom,
			Method:           spec.method,
			Request:          json.RawMessage(spec.request),
			RecordedResponse: json.RawMessage(responses[0]),
			Assert:           spec.assert,
			Notes:            spec.notes,
		}
		if err := CheckMCPResponse(responses[0], f); err != nil {
			t.Fatalf("record %s: assertions do not match the captured response: %v (response: %s)", spec.name, err, responses[0])
		}
		if err := validateMCPFixture(f); err != nil {
			t.Fatalf("record %s: fixture fails validation: %v", spec.name, err)
		}
		raw, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			t.Fatalf("record %s: marshal: %v", spec.name, err)
		}
		path, err := MCPFixturePath(spec.name)
		if err != nil {
			t.Fatalf("record %s: path: %v", spec.name, err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("record %s: write %s: %v", spec.name, path, err)
		}
		fmt.Printf("recorded tests/fixtures/mcp/%s.json\n", spec.name)
	}
}
