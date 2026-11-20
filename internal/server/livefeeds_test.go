package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeModelRegistry is a ModelRegistry that also serves the dashboard
// model catalog, so the models feed can be tested without a real
// provider registry.
type fakeModelRegistry struct {
	models []provider.DashboardModel
}

func (f *fakeModelRegistry) Models(context.Context) ([]Model, error) {
	out := []Model{}
	for _, m := range f.models {
		out = append(out, Model{ID: m.ID, Object: "model", OwnedBy: m.ProviderName})
	}
	return out, nil
}

func (f *fakeModelRegistry) DashboardModels() []provider.DashboardModel {
	if f.models == nil {
		return []provider.DashboardModel{}
	}
	return f.models
}

func getJSON(t *testing.T, srv *Server, path string, v any) int {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, path, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want 200 (body %q)", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("GET %s: decode: %v (body %q)", path, err, rec.Body.String())
	}
	return rec.Code
}

func seedLedger(t *testing.T, l *ledger.Ledger) {
	t.Helper()
	ctx := context.Background()
	prices := ledger.Prices{
		{Model: "model-a"}: {PromptPerToken: 0.001, CompletionPerToken: 0.002},
	}
	seed := []ledger.Usage{
		{Provider: "openai", Model: "model-a", PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, SplitKnown: true, OK: true, LatencyMS: 120},
		{Provider: "openai", Model: "model-a", PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, SplitKnown: true, OK: true, LatencyMS: 200},
		{Provider: "anthropic", Model: "model-b", TotalTokens: 40, SplitKnown: false, OK: false, LatencyMS: 5000},
	}
	for _, u := range seed {
		if _, err := l.RecordUsage(ctx, u, prices); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
}

// TestDashboardLiveFeeds proves the Slice 3 feeds report real router
// state — ledger rows, recorder percentiles, registry models, config
// keys — and that a fresh router with no traffic answers honest
// zeros and empty lists instead of invented numbers.
func TestDashboardLiveFeeds(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	defer led.Close()
	seedLedger(t, led)

	rec := metrics.NewRecorder()
	rec.IncRequest("openai", "model-a", "ok")
	rec.IncRequest("openai", "model-a", "ok")
	rec.IncRequest("anthropic", "model-b", "error")
	rec.ObserveLatency("openai", "model-a", 120)
	rec.ObserveLatency("openai", "model-a", 200)
	rec.ObserveLatency("anthropic", "model-b", 5000)
	rec.IncFailover("anthropic", "openai")
	rec.IncCacheHit("exact")
	rec.IncCacheMiss("exact")

	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("default", []string{"chat", "models"}, 10)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatalf("AddGatewayKey: %v", err)
	}

	reg := &fakeModelRegistry{models: []provider.DashboardModel{
		{ID: "model-a", ProviderID: "openai", ProviderName: "OpenAI", WireFormat: "openai", Verification: "verified-from-docs", Enabled: true, ContextWindow: 128000, InputPriceUSD: 0.0000025, OutputPriceUSD: 0.00001},
		{ID: "model-b", ProviderID: "anthropic", ProviderName: "Anthropic", WireFormat: "anthropic", Verification: "verified-from-docs", Enabled: true, ContextWindow: 200000},
	}}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led), WithMetrics(rec), WithConfig(cfg), WithRegistry(reg))

	// --- Overview: range-limited ledger KPIs + recorder percentiles ---
	var ov overviewResponse
	getJSON(t, srv, "/dashboard/overview?range=24h", &ov)
	if ov.Requests != 3 || ov.OK != 2 || ov.Errors != 1 {
		t.Errorf("overview counts = %d/%d/%d, want 3/2/1", ov.Requests, ov.OK, ov.Errors)
	}
	if ov.PrevRequests != 0 {
		t.Errorf("overview prev_requests = %d, want 0 (no traffic in the previous window)", ov.PrevRequests)
	}
	if ov.PromptTokens != 300 || ov.CompletionTokens != 150 || ov.TotalTokens != 490 {
		t.Errorf("overview tokens = %d/%d/%d, want 300/150/490", ov.PromptTokens, ov.CompletionTokens, ov.TotalTokens)
	}
	if ov.UnknownSplitTokens != 40 {
		t.Errorf("overview unknown-split tokens = %d, want 40", ov.UnknownSplitTokens)
	}
	if math.Abs(ov.CostUSD-0.6) > 1e-9 {
		t.Errorf("overview cost = %v, want 0.6", ov.CostUSD)
	}
	if ov.LatencyP50MS != 250 || ov.LatencyP95MS != 5000 {
		t.Errorf("overview latency p50/p95 = %v/%v, want 250/5000 (histogram bucket bounds)", ov.LatencyP50MS, ov.LatencyP95MS)
	}
	if ov.CacheHits != 1 || ov.CacheMisses != 1 || ov.Failovers != 1 {
		t.Errorf("overview cache/failovers = %d/%d/%d, want 1/1/1", ov.CacheHits, ov.CacheMisses, ov.Failovers)
	}
	if len(ov.Buckets) != 24 {
		t.Errorf("overview buckets = %d, want 24 one-hour buckets", len(ov.Buckets))
	}
	var lastBucket ledger.TimeBucket
	if len(ov.Buckets) > 0 {
		lastBucket = ov.Buckets[len(ov.Buckets)-1]
	}
	if lastBucket.Requests != 3 || lastBucket.Errors != 1 {
		t.Errorf("overview last bucket = %+v, want 3 requests, 1 error", lastBucket)
	}
	// An unknown range falls back to 24h instead of failing the feed.
	var ovBad overviewResponse
	getJSON(t, srv, "/dashboard/overview?range=banana", &ovBad)
	if ovBad.Range != "24h" || ovBad.Requests != 3 {
		t.Errorf("overview unknown range = %+v, want 24h fallback with 3 requests", ovBad)
	}

	// --- Models: registry rows joined with real request counts ---
	var md dashboardModelsResponse
	getJSON(t, srv, "/dashboard/models", &md)
	if md.Total != 2 || len(md.Models) != 2 {
		t.Fatalf("models total = %d, want 2", md.Total)
	}
	byID := map[string]dashboardModel{}
	for _, m := range md.Models {
		byID[m.ID] = m
	}
	if byID["model-a"].Requests != 2 || byID["model-a"].ProviderName != "OpenAI" || byID["model-a"].ContextWindow != 128000 {
		t.Errorf("model-a = %+v, want OpenAI, ctx 128000, 2 requests", byID["model-a"])
	}
	if byID["model-b"].Requests != 1 {
		t.Errorf("model-b requests = %d, want 1", byID["model-b"].Requests)
	}

	// --- Logs: newest first, filters select real rows ---
	var lg logsResponse
	getJSON(t, srv, "/dashboard/logs?limit=50&filter=all", &lg)
	if len(lg.Events) != 3 {
		t.Fatalf("logs all = %d rows, want 3", len(lg.Events))
	}
	if lg.Events[0].Provider != "anthropic" || lg.Events[2].Provider != "openai" || lg.Events[2].PromptTokens != 100 {
		t.Errorf("logs order/content = %+v, want newest-first with the first seed row last", lg.Events)
	}
	var lgOK logsResponse
	getJSON(t, srv, "/dashboard/logs?filter=ok", &lgOK)
	if len(lgOK.Events) != 2 {
		t.Errorf("logs ok = %d rows, want 2", len(lgOK.Events))
	}
	var lgFailed logsResponse
	getJSON(t, srv, "/dashboard/logs?filter=failed", &lgFailed)
	if len(lgFailed.Events) != 1 || lgFailed.Events[0].Provider != "anthropic" {
		t.Errorf("logs failed = %+v, want the one anthropic failure", lgFailed.Events)
	}

	// --- Usage: spend roll-up, failovers, budget ---
	var us usageResponse
	getJSON(t, srv, "/dashboard/usage", &us)
	if us.Range != "30d" {
		t.Errorf("usage range = %q, want default 30d", us.Range)
	}
	if us.Summary.Requests != 3 || math.Abs(us.Summary.CostUSD-0.6) > 1e-9 {
		t.Errorf("usage summary = %+v, want 3 requests, $0.60", us.Summary)
	}
	if us.Failovers != 1 || !us.CostEstimated {
		t.Errorf("usage failovers/estimated = %d/%v, want 1/true", us.Failovers, us.CostEstimated)
	}
	if len(us.Summary.ByProvider) != 2 {
		t.Errorf("usage by_provider = %d rows, want 2", len(us.Summary.ByProvider))
	}

	// --- Keys: facts only — never the raw key, never its hash ---
	var keys routerKeysResponse
	getJSON(t, srv, "/dashboard/keys", &keys)
	if keys.Total != 1 || len(keys.Keys) != 1 {
		t.Fatalf("keys total = %d, want 1", keys.Total)
	}
	k := keys.Keys[0]
	if k.ID != key.ID || k.Name != "default" || k.BudgetUSD != 10 {
		t.Errorf("key row = %+v, want the issued key's facts", k)
	}
	if k.Masked != "crk_••••••••" {
		t.Errorf("key masked = %q, want the fixed mask", k.Masked)
	}
	// The raw key and its hash must not appear anywhere in the body.
	recBody := doReq(t, srv, http.MethodGet, "/dashboard/keys", "", "").Body.String()
	if strings.Contains(recBody, raw) || strings.Contains(recBody, key.KeyHash) {
		t.Errorf("keys feed leaks the raw key or its hash")
	}

	// --- Guard: the new feeds are registered and auth-exempt ---
	for _, path := range []string{"/dashboard/overview", "/dashboard/models", "/dashboard/logs", "/dashboard/usage", "/dashboard/keys"} {
		rec := doReq(t, srv, http.MethodGet, path, "", "")
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s without a key: status = %d, want 200 (auth-exempt feed)", path, rec.Code)
		}
	}
}

// TestDashboardLiveFeedsEmpty proves the honest empty state: a server
// with no ledger, no recorder traffic, no registry and no config
// answers every Slice 3 feed with zeros and empty lists — never an
// error, never an invented number.
func TestDashboardLiveFeedsEmpty(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	var ov overviewResponse
	getJSON(t, srv, "/dashboard/overview", &ov)
	if ov.Requests != 0 || ov.CostUSD != 0 || len(ov.Buckets) != 0 || len(ov.ByProvider) != 0 {
		t.Errorf("empty overview = %+v, want all zero/empty", ov)
	}
	var md dashboardModelsResponse
	getJSON(t, srv, "/dashboard/models", &md)
	if md.Total != 0 || len(md.Models) != 0 {
		t.Errorf("empty models = %+v, want empty", md)
	}
	var lg logsResponse
	getJSON(t, srv, "/dashboard/logs", &lg)
	if len(lg.Events) != 0 {
		t.Errorf("empty logs = %d rows, want 0", len(lg.Events))
	}
	var us usageResponse
	getJSON(t, srv, "/dashboard/usage", &us)
	if us.Summary.Requests != 0 || us.Summary.CostUSD != 0 {
		t.Errorf("empty usage = %+v, want zeros", us.Summary)
	}
	var keys routerKeysResponse
	getJSON(t, srv, "/dashboard/keys", &keys)
	if keys.Total != 0 || len(keys.Keys) != 0 {
		t.Errorf("empty keys = %+v, want empty", keys)
	}
}
