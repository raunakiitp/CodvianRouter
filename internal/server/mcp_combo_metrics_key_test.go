// Integration-B follow-up — KeyScopedComboMetricsSource on the
// production SkillSource (internal/server/mcp_http.go):
// ComboMetricsForKey returns ONE gateway key's measured history
// for one combo, so gateway-key smart-routing gets real per-key
// metrics instead of strategy-only. Every test here must FAIL on
// the pre-change code (negative control: the new method deleted
// from mcp_http.go — the per-key tests fail to compile and the
// InvokeSkill test reports metrics omitted; see the parent
// report for the exact commands and output).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// The production source implements the optional per-key
// interface: if this line ever fails to compile, gateway-key
// smart-routing silently falls back to strategy-only metrics.
var _ a2a.KeyScopedComboMetricsSource = (*serverMCPSource)(nil)

// comboKeyPrices prices models a ($10/$30 per 1M) and c ($5/$15
// per 1M); model b is deliberately unpriced everywhere (no
// ledger price, no price-book entry), for the unpriced test.
func comboKeyPrices() ledger.Prices {
	return ledger.Prices{
		ledger.PriceKey{Model: "a"}: {PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
		ledger.PriceKey{Model: "c"}: {PromptPerToken: 5.0 / 1e6, CompletionPerToken: 15.0 / 1e6},
	}
}

func comboKeyPriceBook() *config.PriceBook {
	book := config.NewPriceBook()
	book.Entries["a"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	book.Entries["c"] = config.ModelPrice{InputUSDPer1M: usdPer1M(5), OutputUSDPer1M: usdPer1M(15)}
	return book
}

// newComboKeyServer builds a server with combo "fast" (members
// p1 + p2), a live ledger, and the prices above, and seeds two
// keys with deliberately different histories for that combo:
//
//   - key-1: 2 successes on p1/a, 1000 prompt + 500 completion
//     each, latencies 120 ms and 80 ms. Cost $0.025 each.
//   - key-2: 10 successes on p1/a, 100 prompt + 50 completion
//     each, 200 ms each ($0.0025 each), plus 2 FAILURES on
//     p2/c, 100 prompt each ($0.0005 each), plus 1 success on
//     p3/z (a NON-member provider — must never appear in the
//     combo metrics for any key).
//
// Expected per-key numbers (see the assertions): key-1
// requests 2 / ok 2 / cost $0.05 / mean latency 100 ms;
// key-2 requests 12 / ok 10 / errors 2 / cost $0.026 / mean
// latency 200 ms over its 10 successful events. The ledger-wide
// aggregate is requests 14 / ok 12 / errors 2 / cost $0.076.
func newComboKeyServer(t *testing.T) (*Server, *ledger.Ledger) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
		{ID: "p2", Name: "Chatter", BaseURL: "https://p2.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "anthropic", Models: []provider.Model{{ID: "c"}}, Enabled: true},
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := comboKeyPrices()
	seed := func(keyID, providerID, model string, prompt, completion int, ok bool, latency int64) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: keyID, Provider: providerID, Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: ok, LatencyMS: latency,
		}, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	seed("key-1", "p1", "a", 1000, 500, true, 120)
	seed("key-1", "p1", "a", 1000, 500, true, 80)
	for i := 0; i < 10; i++ {
		seed("key-2", "p1", "a", 100, 50, true, 200)
	}
	seed("key-2", "p2", "c", 100, 0, false, 50)
	seed("key-2", "p2", "c", 100, 0, false, 50)
	seed("key-2", "p3", "z", 500, 500, true, 10)

	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()

	srv := newTestServer(t, &strategyFakeEngine{},
		WithConfig(cfg), WithLedger(led), WithLedgerPrices(prices), WithPriceBook(comboKeyPriceBook()),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
			{ID: "p2", Name: "Chatter", Enabled: true, WireFormat: "anthropic"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "c", ProviderID: "p2", OwnedBy: "Chatter"},
		}}),
	)
	return srv, led
}

func floatEq(t *testing.T, what string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func checkComboMetrics(t *testing.T, what string, m mcp.ComboMetrics, wantReq, wantOK, wantErr, wantPrompt, wantCompletion, wantTotal int, wantCost float64, wantSR *float64, wantLatKnown bool, wantLat float64, wantLatEvents int) {
	t.Helper()
	if m.Requests != wantReq {
		t.Errorf("%s requests = %d, want %d", what, m.Requests, wantReq)
	}
	if m.OK != wantOK {
		t.Errorf("%s ok = %d, want %d", what, m.OK, wantOK)
	}
	if m.Errors != wantErr {
		t.Errorf("%s errors = %d, want %d", what, m.Errors, wantErr)
	}
	if m.PromptTokens != wantPrompt {
		t.Errorf("%s prompt_tokens = %d, want %d", what, m.PromptTokens, wantPrompt)
	}
	if m.CompletionTokens != wantCompletion {
		t.Errorf("%s completion_tokens = %d, want %d", what, m.CompletionTokens, wantCompletion)
	}
	if m.TotalTokens != wantTotal {
		t.Errorf("%s total_tokens = %d, want %d", what, m.TotalTokens, wantTotal)
	}
	floatEq(t, what+" cost_usd", m.CostUSD, wantCost)
	if wantSR == nil {
		if m.SuccessRate != nil {
			t.Errorf("%s success_rate = %v, want nil (no requests)", what, *m.SuccessRate)
		}
	} else if m.SuccessRate == nil {
		t.Errorf("%s success_rate = nil, want %v", what, *wantSR)
	} else {
		floatEq(t, what+" success_rate", *m.SuccessRate, *wantSR)
	}
	if m.LatencyKnown != wantLatKnown {
		t.Errorf("%s latency_known = %v, want %v", what, m.LatencyKnown, wantLatKnown)
	}
	if wantLatKnown {
		floatEq(t, what+" mean_latency_ms", m.MeanLatencyMS, wantLat)
	}
	if m.LatencyEvents != wantLatEvents {
		t.Errorf("%s latency_events = %d, want %d", what, m.LatencyEvents, wantLatEvents)
	}
}

func fptr(f float64) *float64 { return &f }

// TestServerComboMetricsForKeyIsolation: each key sees ONLY its
// own numbers for the same combo — key-1's 2 cheap requests
// must not be polluted by key-2's 12 requests, and key-2's
// numbers must exclude both key-1's rows and its own
// non-member (p3/z) row.
func TestServerComboMetricsForKeyIsolation(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}

	m1, err := src.ComboMetricsForKey("key-1", "fast")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(key-1): %v", err)
	}
	if m1.Name != "fast" || m1.Window != "recent_500_events" {
		t.Errorf("ComboMetricsForKey(key-1): name/window = %q/%q, want fast/recent_500_events", m1.Name, m1.Window)
	}
	if !m1.CostKnown {
		t.Errorf("ComboMetricsForKey(key-1): cost_known = false, want true (models a and c are priced)")
	}
	checkComboMetrics(t, "key-1", m1, 2, 2, 0, 2000, 1000, 3000, 0.05, fptr(1.0), true, 100, 2)

	m2, err := src.ComboMetricsForKey("key-2", "fast")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(key-2): %v", err)
	}
	if !m2.CostKnown {
		t.Errorf("ComboMetricsForKey(key-2): cost_known = false, want true (the unpriced p3/z row is a non-member and must not flip it)")
	}
	if len(m2.UnpricedModels) != 0 {
		t.Errorf("ComboMetricsForKey(key-2): unpriced_models = %v, want none (non-member p3/z excluded)", m2.UnpricedModels)
	}
	checkComboMetrics(t, "key-2", m2, 12, 10, 2, 1200, 500, 1700, 0.026, fptr(10.0/12.0), true, 200, 10)
}

// TestServerComboMetricsForKeyEmptyKey: a key with no rows for
// the combo gets zero metrics and no error — never another
// key's numbers, never a zero-filled fake history.
func TestServerComboMetricsForKeyEmptyKey(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	m, err := src.ComboMetricsForKey("key-with-nothing", "fast")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(empty key): %v", err)
	}
	checkComboMetrics(t, "empty key", m, 0, 0, 0, 0, 0, 0, 0, nil, false, 0, 0)
	if !m.CostKnown {
		t.Errorf("ComboMetricsForKey(empty key): cost_known = false, want true (no unpriced tokens seen)")
	}
}

// TestServerComboMetricsForKeyWhitespaceKeyID: a padded key
// id (" key-1 ") is normalised before the ledger comparison,
// so it returns that key's metrics — never a silent zero for
// a key that does exist.
func TestServerComboMetricsForKeyWhitespaceKeyID(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	m, err := src.ComboMetricsForKey(" key-1 ", "fast")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(\" key-1 \"): %v", err)
	}
	checkComboMetrics(t, "padded key-1", m, 2, 2, 0, 2000, 1000, 3000, 0.05, fptr(1.0), true, 100, 2)
}

// TestServerComboMetricsForKeyErrors: unknown combo and empty
// key id are invalid input; no ledger is an honest unavailable
// error, never a zero result.
func TestServerComboMetricsForKeyErrors(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	if _, err := src.ComboMetricsForKey("key-1", "no-such-combo"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(unknown combo): err = %v, want invalid input", err)
	}
	if _, err := src.ComboMetricsForKey("", "fast"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(empty key): err = %v, want invalid input", err)
	}
	if _, err := src.ComboMetricsForKey("  ", "fast"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(blank key): err = %v, want invalid input", err)
	}
	// No ledger wired: same honest unavailability as
	// ComboMetrics, never a silent empty result.
	cfg := config.DefaultConfig(t.TempDir())
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1", "p2"}}}
	cfg.UnlockRouting()
	bare := newTestServer(t, &strategyFakeEngine{}, WithConfig(cfg))
	bareSrc := &serverMCPSource{srv: bare, st: mcpHTTPStateFor(bare)}
	if _, err := bareSrc.ComboMetricsForKey("key-1", "fast"); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("ComboMetricsForKey(no ledger): err = %v, want a no-ledger-yet error", err)
	}
}

// TestServerComboMetricsForKeyUnpriced: a row on an unpriced
// model (p1/b — no ledger price, no price-book entry) marks
// the cost unknown with exactly that model's tokens listed;
// the priced rows still contribute their measured cost.
func TestServerComboMetricsForKeyUnpriced(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Name: "Coder", BaseURL: "https://p1.example", APIKeyEncrypted: "sealed-test-key", WireFormat: "openai", Models: []provider.Model{{ID: "a"}, {ID: "b"}}, Enabled: true},
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := comboKeyPrices()
	seed := func(model string, prompt, completion int) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), ledger.Usage{
			GatewayKeyID: "key-1", Provider: "p1", Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: true, LatencyMS: 50,
		}, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	seed("a", 1000, 500) // priced: $0.025
	seed("b", 500, 100)  // unpriced: $0 stored, 600 tokens
	cfg.EnsureRoutingMu()
	cfg.LockRouting()
	cfg.Routing = &config.RoutingConfig{Groups: map[string][]string{"fast": {"p1"}}}
	cfg.UnlockRouting()
	srv := newTestServer(t, &strategyFakeEngine{},
		WithConfig(cfg), WithLedger(led), WithLedgerPrices(prices), WithPriceBook(comboKeyPriceBook()),
		WithDashboardProviders(mcpTestProviders{providers: []provider.DashboardProvider{
			{ID: "p1", Name: "Coder", Enabled: true, WireFormat: "openai"},
		}}),
		WithRegistry(mcpTestRegistry{models: []Model{
			{ID: "a", ProviderID: "p1", OwnedBy: "Coder"},
			{ID: "b", ProviderID: "p1", OwnedBy: "Coder"},
		}}),
	)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	m, err := src.ComboMetricsForKey("key-1", "fast")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(unpriced): %v", err)
	}
	checkComboMetrics(t, "unpriced", m, 2, 2, 0, 1500, 600, 2100, 0.025, fptr(1.0), true, 50, 2)
	if m.CostKnown {
		t.Errorf("unpriced: cost_known = true, want false (model b is unpriced)")
	}
	if m.UnpricedTokens != 600 {
		t.Errorf("unpriced: unpriced_tokens = %d, want 600 (model b's tokens only)", m.UnpricedTokens)
	}
	if len(m.UnpricedModels) != 1 || m.UnpricedModels[0] != "p1/b" {
		t.Errorf("unpriced: unpriced_models = %v, want [p1/b]", m.UnpricedModels)
	}
}

// TestServerComboMetricsLedgerWideUnchanged: the existing
// ledger-wide ComboMetrics still returns the aggregate over
// BOTH keys (plus neither key's non-member row) — the new
// per-key method changed no ledger-wide behaviour.
func TestServerComboMetricsLedgerWideUnchanged(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	m, err := src.ComboMetrics("fast")
	if err != nil {
		t.Fatalf("ComboMetrics(fast): %v", err)
	}
	if m.Window != "all_time" {
		t.Errorf("ComboMetrics: window = %q, want all_time (ledger-wide is unchanged)", m.Window)
	}
	if !m.CostKnown {
		t.Errorf("ComboMetrics: cost_known = false, want true")
	}
	// 14 requests = key-1's 2 + key-2's 12; the p3/z row is a
	// non-member and stays out. Latency mean is
	// (120+80+10*200)/12 = 2200/12.
	checkComboMetrics(t, "ledger-wide", m, 14, 12, 2, 3200, 1500, 4700, 0.076, fptr(12.0/14.0), true, 2200.0/12.0, 12)
}

// TestServerComboMetricsForKeyViaInvokeSkill: end to end through
// a2a.InvokeSkill — a gateway-key caller gets MEASURED metrics
// (metrics_known true, its own numbers, scope gateway-key with
// a per-key scope note), not the omitted-metrics path the
// pre-change source took. The WithContext-bound copy (what the
// production a2aSkillSource hands InvokeSkill) must expose the
// optional interface too, or the production path would silently
// fall back to strategy-only.
func TestServerComboMetricsForKeyViaInvokeSkill(t *testing.T) {
	srv, _ := newComboKeyServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	ctx := context.Background()
	if _, ok := src.WithContext(ctx).(a2a.KeyScopedComboMetricsSource); !ok {
		t.Fatal("WithContext-bound source does not implement KeyScopedComboMetricsSource — the production a2aSkillSource path would fall back to strategy-only metrics")
	}
	invoke := func(keyID string) map[string]any {
		t.Helper()
		callerCtx := a2a.WithCaller(ctx, a2a.Caller{Kind: a2a.CallerGatewayKey, KeyID: keyID})
		raw, err := a2a.InvokeSkill(callerCtx, src, "smart-routing", json.RawMessage(`{"task":"write fast code"}`))
		if err != nil {
			t.Fatalf("InvokeSkill smart-routing as %s: %v", keyID, err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("InvokeSkill smart-routing as %s: decode: %v", keyID, err)
		}
		return out
	}
	check := func(keyID string, wantRequests float64, wantCost float64) {
		t.Helper()
		out := invoke(keyID)
		if out["metrics_known"] != true {
			t.Errorf("smart-routing as %s: metrics_known = %v, want true (measured per-key metrics, not omitted)", keyID, out["metrics_known"])
		}
		if out["scope"] != "gateway-key" {
			t.Errorf("smart-routing as %s: scope = %v, want gateway-key", keyID, out["scope"])
		}
		if out["scoped_to_key"] != keyID {
			t.Errorf("smart-routing as %s: scoped_to_key = %v, want %q", keyID, out["scoped_to_key"], keyID)
		}
		note, _ := out["scope_note"].(string)
		if !strings.Contains(note, "scoped to the caller's key") {
			t.Errorf("smart-routing as %s: scope_note = %q, want the per-key note, not the omitted-metrics note", keyID, note)
		}
		if strings.Contains(note, "omitted") {
			t.Errorf("smart-routing as %s: scope_note = %q, want no omission language", keyID, note)
		}
		combo, _ := out["combo"].(map[string]any)
		if combo["name"] != "fast" {
			t.Errorf("smart-routing as %s: combo = %v, want fast", keyID, combo["name"])
		}
		metrics, _ := out["metrics"].(map[string]any)
		if metrics == nil {
			t.Fatalf("smart-routing as %s: no metrics in result: %v", keyID, out)
		}
		if metrics["requests"] != wantRequests {
			t.Errorf("smart-routing as %s: metrics.requests = %v, want %v (own key only)", keyID, metrics["requests"], wantRequests)
		}
		cost, _ := metrics["cost_usd"].(float64)
		if math.Abs(cost-wantCost) > 1e-9 {
			t.Errorf("smart-routing as %s: metrics.cost_usd = %v, want %v (own key only)", keyID, cost, wantCost)
		}
	}
	check("key-1", 2, 0.05)
	check("key-2", 12, 0.026)
}
