package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// The router engine satisfies the server's Engine + StreamResumer
// contracts. Asserted here (not in the router tests): the router
// tests cannot import server, because server imports router for
// strategy reporting — that would be an import cycle in test.
var (
	_ Engine        = (*router.Engine)(nil)
	_ StreamResumer = (*router.Engine)(nil)
)

// strategyFakeEngine is a fakeEngine that also reports routing
// strategies, so the strategies feed/update handlers can be tested
// through the real HTTP stack.
type strategyFakeEngine struct {
	fakeEngine
	global  string
	chains  map[string]string
	last    router.RoutingInfo
	hasLast bool
}

func (f *strategyFakeEngine) LastRouting() (router.RoutingInfo, bool) {
	return f.last, f.hasLast
}

func (f *strategyFakeEngine) StrategyState() (string, map[string]string) {
	out := make(map[string]string, len(f.chains))
	for k, v := range f.chains {
		out[k] = v
	}
	return f.global, out
}

func (f *strategyFakeEngine) SetGlobalStrategy(s string) { f.global = s }

func (f *strategyFakeEngine) SetChainStrategy(model, s string) {
	if f.chains == nil {
		f.chains = map[string]string{}
	}
	if s == "" {
		delete(f.chains, model)
		return
	}
	f.chains[model] = s
}

func getStrategies(t *testing.T, srv *Server) strategiesResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/strategies", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/strategies status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var feed strategiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode strategies feed: %v (body %s)", err, rec.Body.String())
	}
	return feed
}

func TestStrategiesFeedEmpty(t *testing.T) {
	srv := newTestServer(t, &strategyFakeEngine{})
	feed := getStrategies(t, srv)
	if len(feed.Strategies) != len(strategy.All) {
		t.Errorf("strategies = %d (want %d: strategy.All)", len(feed.Strategies), len(strategy.All))
	}
	wantIDs := []string{"priority", "balanced", "cheapest", "fastest", "most-reliable", "least-latency", "latency-aware", "weighted", "custom-weights", "code", "round-robin", "random", "strict-random", "fill-first", "least-used", "p2c", "quota-weighted", "headroom", "expiry-first", "reset-aware", "reset-window", "lkgp", "context-optimized", "cache-optimized", "context-relay", "cost-optimized", "ship-fast", "cost-saver", "quality-first", "offline-friendly", "reliability-first", "chaos-mode", "cascade", "sla-aware", "nadir"}
	for i, id := range wantIDs {
		if i >= len(feed.Strategies) || feed.Strategies[i].ID != id {
			t.Errorf("strategies[%d] = %q (want %q)", i, feed.Strategies[i].ID, id)
		}
	}
	if feed.Global != "priority" {
		t.Errorf("global = %q (want priority: the default with no config)", feed.Global)
	}
	if len(feed.Chains) != 0 {
		t.Errorf("chains = %v (want none: no providers configured)", feed.Chains)
	}
	if feed.Last != nil {
		t.Errorf("last = %+v (want null: no request yet)", feed.Last)
	}
	if feed.DeadlineBudgetMs != 0 {
		t.Errorf("deadline_budget_ms = %d (want 0: disabled)", feed.DeadlineBudgetMs)
	}
	if feed.Persisted {
		t.Error("persisted = true (want false: no persist function wired)")
	}
	if feed.WeightedWeights != nil {
		t.Errorf("weighted_weights = %+v (want absent: no custom weights configured)", feed.WeightedWeights)
	}
}

func TestStrategiesFeedLive(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Routing = &config.RoutingConfig{
		GlobalStrategy:   "cheapest",
		DeadlineBudgetMs: 2000,
		ChainStrategies:  map[string]string{"gpt-4o": "fastest"},
		WeightedWeights:  &config.RoutingWeights{Latency: 0.7, Cost: 0.3},
	}
	eng := &strategyFakeEngine{
		global:  "most-reliable", // runtime global beats the config's cheapest
		chains:  map[string]string{"claude-x": "least-latency"},
		last:    router.RoutingInfo{Strategy: "fastest", Model: "gpt-4o", Provider: "fast", At: time.Now()},
		hasLast: true,
	}
	srv := newTestServer(t, eng, WithConfig(cfg), WithModels("gpt-4o", "claude-x"))
	feed := getStrategies(t, srv)
	if feed.Global != "most-reliable" {
		t.Errorf("global = %q (want most-reliable: runtime beats config)", feed.Global)
	}
	if feed.DeadlineBudgetMs != 2000 {
		t.Errorf("deadline_budget_ms = %d (want 2000)", feed.DeadlineBudgetMs)
	}
	if feed.WeightedWeights == nil || feed.WeightedWeights.Latency != 0.7 || feed.WeightedWeights.Cost != 0.3 {
		t.Errorf("weighted_weights = %+v (want latency 0.7, cost 0.3)", feed.WeightedWeights)
	}
	if feed.Last == nil || feed.Last.Strategy != "fastest" || feed.Last.Model != "gpt-4o" || feed.Last.Provider != "fast" {
		t.Errorf("last = %+v (want fastest/gpt-4o/fast)", feed.Last)
	}
	byModel := map[string]chainStrategy{}
	for _, c := range feed.Chains {
		byModel[c.Model] = c
	}
	if len(feed.Chains) != 2 {
		t.Fatalf("chains = %v (want gpt-4o + claude-x)", feed.Chains)
	}
	if got := byModel["gpt-4o"]; got.Strategy != "fastest" || got.Effective != "fastest" {
		t.Errorf("gpt-4o chain = %+v (want override fastest)", got)
	}
	if got := byModel["claude-x"]; got.Strategy != "least-latency" || got.Effective != "least-latency" {
		t.Errorf("claude-x chain = %+v (want runtime override least-latency)", got)
	}
	// The custom-weights entry in the strategy list shows the operator's
	// custom weights, normalised.
	for _, st := range feed.Strategies {
		if st.ID == "custom-weights" {
			if st.Weights.Latency != 0.7 || st.Weights.Cost != 0.3 {
				t.Errorf("custom-weights strategy weights = %+v (want latency 0.7, cost 0.3)", st.Weights)
			}
		}
		if st.ID == "least-latency" {
			if st.Weights.Latency != 0.8 || st.Weights.Cost != 0 {
				t.Errorf("least-latency weights = %+v (want latency 0.8, cost 0)", st.Weights)
			}
		}
	}
}

func TestStrategiesUpdate(t *testing.T) {
	eng := &strategyFakeEngine{}
	srv := newTestServer(t, eng, WithModels("gpt-4o", "claude-x"))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"cheapest","chains":{"gpt-4o":"fastest"}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var feed strategiesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatal(err)
	}
	if feed.Global != "cheapest" {
		t.Errorf("after POST global = %q (want cheapest)", feed.Global)
	}
	byModel := map[string]chainStrategy{}
	for _, c := range feed.Chains {
		byModel[c.Model] = c
	}
	if got := byModel["gpt-4o"]; got.Strategy != "fastest" || got.Effective != "fastest" {
		t.Errorf("gpt-4o chain = %+v (want override fastest)", got)
	}
	if got := byModel["claude-x"]; got.Strategy != "" || got.Effective != "cheapest" {
		t.Errorf("claude-x chain = %+v (want inherit -> cheapest)", got)
	}
	if eng.global != "cheapest" || eng.chains["gpt-4o"] != "fastest" {
		t.Errorf("engine state = global %q chains %v (want cheapest / {gpt-4o: fastest})", eng.global, eng.chains)
	}
	// Clearing: empty global + empty chain value restore inheritance.
	rec = doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"","chains":{"gpt-4o":""}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("clear POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed = getStrategies(t, srv)
	if feed.Global != "priority" {
		t.Errorf("after clear global = %q (want priority: the M1.11 default)", feed.Global)
	}
	if byModel := map[string]chainStrategy{}; true {
		for _, c := range feed.Chains {
			byModel[c.Model] = c
		}
		if got := byModel["gpt-4o"]; got.Strategy != "" || got.Effective != "priority" {
			t.Errorf("gpt-4o chain after clear = %+v (want inherit -> priority)", got)
		}
	}
}

func TestStrategiesUpdateRejectsUnknownStrategy(t *testing.T) {
	eng := &strategyFakeEngine{global: "balanced"}
	srv := newTestServer(t, eng, WithModels("gpt-4o"))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"smartest"}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown global strategy status = %d (want 400)", rec.Code)
	}
	if eng.global != "balanced" {
		t.Errorf("engine global changed to %q on a rejected update (want balanced: nothing applied)", eng.global)
	}
	rec = doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"chains":{"gpt-4o":"smartest"}}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown chain strategy status = %d (want 400)", rec.Code)
	}
	if len(eng.chains) != 0 {
		t.Errorf("engine chains changed to %v on a rejected update (want none)", eng.chains)
	}
}

func TestStrategiesUpdatePersistFailureLeavesRuntimeUnchanged(t *testing.T) {
	eng := &strategyFakeEngine{global: "balanced"}
	persisted := 0
	srv := newTestServer(t, eng,
		WithRoutingPersist(func(update RoutingUpdate) error {
			persisted++
			return errors.New("disk full")
		}),
	)
	rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"cheapest"}`, "test-key-123")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("persist failure status = %d (want 500)", rec.Code)
	}
	if persisted != 1 {
		t.Errorf("persist called %d times (want 1)", persisted)
	}
	if eng.global != "balanced" {
		t.Errorf("engine global = %q after failed persist (want balanced: persist runs first)", eng.global)
	}
	// And a successful persist applies the runtime change.
	srv2 := newTestServer(t, eng,
		WithRoutingPersist(func(update RoutingUpdate) error { return nil }),
	)
	rec = doReq(t, srv2, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"cheapest"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("persist success status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if eng.global != "cheapest" {
		t.Errorf("engine global = %q after successful persist (want cheapest)", eng.global)
	}
	if feed := getStrategies(t, srv2); !feed.Persisted {
		t.Error("feed persisted = false with a persist function wired (want true)")
	}
}

func TestDeadlineExceededMapsTo504(t *testing.T) {
	eng := &fakeEngine{chatErr: router.ErrDeadlineExceeded}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("deadline-exceeded status = %d (want 504)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "deadline budget exceeded") {
		t.Errorf("504 body = %s (want it to name the deadline budget)", rec.Body.String())
	}
}

func TestStrategyHeaderOnChatResponse(t *testing.T) {
	eng := &strategyFakeEngine{
		fakeEngine: fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}},
		last:       router.RoutingInfo{Strategy: "cheapest", Model: "model-a", Provider: "cheap"},
		hasLast:    true,
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Codvian-Strategy"); got != "cheapest" {
		t.Errorf("X-Codvian-Strategy = %q (want cheapest)", got)
	}
}
