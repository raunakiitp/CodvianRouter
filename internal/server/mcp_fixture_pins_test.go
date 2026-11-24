// A1 — MCP error-fixture pins (WT-09..WT-16): the 8 error fixtures
// under tests/fixtures/mcp for explain_route / simulate_route /
// set_routing_strategy with empty or wrong-typed model / strategy /
// chain arguments. Each fixture is replayed through a live
// mcp.Server (LocalOperator identity — a bare New() defaults to
// Anonymous deny-all) and the live -32602 error code + message must
// equal the message the fixture pins. The fixture's own recorded
// response must satisfy the same assertion (self-consistency).
//
// The three handlers validate arguments before touching the
// Source, so the stub Source below is never called — it exists
// only because mcp.New panics on a nil Source.
package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// pinStubSource is a never-called Source for the pins test: every
// method returns the zero value. The pinned calls fail at argument
// validation, before any Source method runs.
type pinStubSource struct{}

func (pinStubSource) Providers() []mcp.ProviderInfo { return nil }
func (pinStubSource) Models() []mcp.ModelInfo       { return nil }
func (pinStubSource) Combos() []mcp.ComboInfo       { return nil }
func (pinStubSource) CreateCombo(ctx context.Context, name string, members []string, strategy string) error {
	return nil
}
func (pinStubSource) ActiveCombo() string              { return "" }
func (pinStubSource) SetActiveCombo(name string) error { return nil }
func (pinStubSource) ComboMetrics(name string) (mcp.ComboMetrics, error) {
	return mcp.ComboMetrics{}, nil
}
func (pinStubSource) RoutingStrategy(chain string) mcp.StrategyInfo { return mcp.StrategyInfo{} }
func (pinStubSource) SetRoutingStrategy(chain, strategy string) error {
	return nil
}
func (pinStubSource) ExplainRoute(model string) (mcp.RouteExplanation, error) {
	return mcp.RouteExplanation{}, nil
}
func (pinStubSource) SimulateRoute(model string, promptTokens int) (mcp.RouteSimulation, error) {
	return mcp.RouteSimulation{}, nil
}
func (pinStubSource) ModelStats() []mcp.ModelStat { return nil }
func (pinStubSource) CostReport(keyID string, days int) (mcp.CostReport, error) {
	return mcp.CostReport{}, nil
}
func (pinStubSource) CheckQuota(keyID string) (mcp.QuotaReport, error) {
	return mcp.QuotaReport{}, nil
}
func (pinStubSource) SetBudgetGuard(keyID string, limitUSD float64, unpricedPolicy string, dailyLimitUSD, weeklyLimitUSD, monthlyLimitUSD *float64) error {
	return nil
}
func (pinStubSource) PriceKnown(provider, model string) bool { return false }
func (pinStubSource) Status() mcp.StatusInfo                 { return mcp.StatusInfo{} }
func (pinStubSource) CacheStats() mcp.CacheStats             { return mcp.CacheStats{} }
func (pinStubSource) CacheFlush(tenant string) (int, error)  { return 0, nil }
func (pinStubSource) DBHealth() mcp.DBHealth                 { return mcp.DBHealth{} }
func (pinStubSource) DBStats() mcp.DBStats                   { return mcp.DBStats{} }
func (pinStubSource) MemoryAdd(caller, text string) (mcp.MemoryEntry, error) {
	return mcp.MemoryEntry{}, nil
}
func (pinStubSource) MemorySearch(caller, query string) ([]mcp.MemoryEntry, error) {
	return nil, nil
}
func (pinStubSource) MemoryClear(caller string) (int, error) { return 0, nil }

// P1.2 ecosystem expansion — pinStubSource stub implementations.

func (pinStubSource) CapabilityEntries() []mcp.CapabilityEntry        { return nil }
func (pinStubSource) QuotaIntelligence(filter string) ([]mcp.QuotaIntelEntry, error) {
	return nil, nil
}
func (pinStubSource) BudgetForecast(keyID string) (mcp.BudgetForecastEntry, error) {
	return mcp.BudgetForecastEntry{}, nil
}
func (pinStubSource) CompressionStats() mcp.CompressionStatsEntry {
	return mcp.CompressionStatsEntry{}
}
func (pinStubSource) EvalSchedules() ([]mcp.EvalScheduleEntry, error) {
	return nil, nil
}
func (pinStubSource) OrgReport() mcp.OrgReportEntry {
	return mcp.OrgReportEntry{}
}

// mcpPinFixtureNames is the 8 fixtures this test pins, in tool
// order. Each fixture's assert.json_paths["error.message"] is the
// exact -32602 message the live server must produce.
var mcpPinFixtureNames = []string{
	"error-explain-route-empty-model",
	"error-explain-route-nonstring-combo",
	"error-explain-route-nonstring-model",
	"error-set-routing-strategy-empty-strategy",
	"error-set-routing-strategy-nonstring-chain",
	"error-simulate-route-empty-model",
	"error-simulate-route-nonstring-combo",
	"error-simulate-route-nonstring-model",
}

type mcpPinFixture struct {
	Name             string          `json:"name"`
	Request          json.RawMessage `json:"request"`
	RecordedResponse json.RawMessage `json:"recorded_response"`
	Assert           struct {
		JSONPaths map[string]any `json:"json_paths"`
	} `json:"assert"`
}

type mcpPinError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func mcpPinErrorOf(t *testing.T, raw json.RawMessage) *mcpPinError {
	t.Helper()
	var resp struct {
		Error *mcpPinError `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v: %s", err, raw)
	}
	return resp.Error
}

func TestMCPFixturePins(t *testing.T) {
	srv := mcp.New(pinStubSource{}, "0.1.0-test").WithClientIdentity(mcp.LocalOperator())
	for _, name := range mcpPinFixtureNames {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "mcp", name+".json"))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var f mcpPinFixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			wantMsg, ok := f.Assert.JSONPaths["error.message"].(string)
			if !ok || wantMsg == "" {
				t.Fatalf("fixture %s: no assert error.message pinned", name)
			}
			// Live replay: the fixture's own request through the
			// real MCP server must answer the pinned -32602.
			live := srv.Handle(context.Background(), f.Request)
			liveErr := mcpPinErrorOf(t, live)
			if liveErr == nil || liveErr.Code != -32602 || liveErr.Message != wantMsg {
				t.Fatalf("live reply: %+v, want code -32602 message %q (raw: %s)", liveErr, wantMsg, live)
			}
			// Self-consistency: the recorded response must pin
			// the same code + message.
			recErr := mcpPinErrorOf(t, f.RecordedResponse)
			if recErr == nil || recErr.Code != -32602 || recErr.Message != wantMsg {
				t.Fatalf("recorded response: %+v, want code -32602 message %q", recErr, wantMsg)
			}
		})
	}
}
