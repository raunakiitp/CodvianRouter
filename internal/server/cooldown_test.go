package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// cooldownFakeEngine is a fakeEngine that also reports cooldown
// modes and cooling targets, so the cooldown feed/update handlers
// can be tested through the real HTTP stack.
type cooldownFakeEngine struct {
	fakeEngine
	global   string
	chains   map[string]string
	cooling  []router.CoolingTarget
	breakers []router.BreakerStatus
}

func (f *cooldownFakeEngine) CooldownTargets() []router.CoolingTarget { return f.cooling }

func (f *cooldownFakeEngine) BreakerStates() []router.BreakerStatus { return f.breakers }

func (f *cooldownFakeEngine) CooldownState() (string, map[string]string) {
	out := make(map[string]string, len(f.chains))
	for k, v := range f.chains {
		out[k] = v
	}
	return f.global, out
}

func (f *cooldownFakeEngine) SetCooldownMode(mode string) { f.global = mode }

func (f *cooldownFakeEngine) SetChainCooldownMode(model, mode string) {
	if f.chains == nil {
		f.chains = map[string]string{}
	}
	if mode == "" {
		delete(f.chains, model)
		return
	}
	f.chains[model] = mode
}

func getCooldown(t *testing.T, srv *Server) cooldownResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/cooldown", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/cooldown status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var feed cooldownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode cooldown feed: %v (body %s)", err, rec.Body.String())
	}
	return feed
}

func TestCooldownFeedEmpty(t *testing.T) {
	srv := newTestServer(t, &cooldownFakeEngine{})
	feed := getCooldown(t, srv)
	if len(feed.Modes) != 2 || feed.Modes[0].ID != "hard" || feed.Modes[1].ID != "soft-demote" {
		t.Errorf("modes = %+v, want [hard soft-demote]", feed.Modes)
	}
	if feed.Mode != "hard" {
		t.Errorf("mode = %q, want hard (the default with no config)", feed.Mode)
	}
	if len(feed.Chains) != 0 {
		t.Errorf("chains = %v, want none (no providers configured)", feed.Chains)
	}
	if len(feed.Cooling) != 0 {
		t.Errorf("cooling = %+v, want empty (honest empty state, nothing cooling)", feed.Cooling)
	}
	if len(feed.Breakers) != 0 {
		t.Errorf("breakers = %+v, want empty (honest empty state, no providers known)", feed.Breakers)
	}
	if feed.BreakerRule == "" {
		t.Error("breaker_rule = \"\", want the live tripping rule in the feed")
	}
	if feed.Persisted {
		t.Error("persisted = true, want false (no persist function wired)")
	}
}

func TestCooldownFeedLive(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Routing = &config.RoutingConfig{
		CooldownMode:       "soft-demote",
		ChainCooldownModes: map[string]string{"gpt-4o": "hard"},
	}
	// Display names for the cooling targets resolve from the
	// config file — the feed must carry them, not just IDs.
	cfg.Providers = []config.ProviderConfig{
		{ID: "slow-prov", Name: "Slow Provider"},
		{ID: "hot-prov", Name: "Hot Provider"},
	}
	eng := &cooldownFakeEngine{
		global: "hard", // runtime global beats the config's soft-demote
		chains: map[string]string{"claude-x": "soft-demote"},
		cooling: []router.CoolingTarget{
			{ProviderID: "slow-prov", Mode: "soft-demote", Until: time.Now().Add(4 * time.Second), RemainingSeconds: 4, Failures: 2, Demoted: true},
			{ProviderID: "hot-prov", Mode: "hard", Until: time.Now().Add(30 * time.Second), RemainingSeconds: 30, Failures: 0, Demoted: false},
		},
		breakers: []router.BreakerStatus{
			{ProviderID: "slow-prov", Open: false, ConsecutiveFailures: 2, Threshold: 5, LastReason: "upstream_error", Ignored4xx: 3},
			{ProviderID: "hot-prov", Open: true, ConsecutiveFailures: 5, Threshold: 5, OpenedAt: time.Now(), RemainingSeconds: 25, LastReason: "timeout", Ignored4xx: 0},
		},
	}
	srv := newTestServer(t, eng, WithConfig(cfg), WithModels("gpt-4o", "claude-x"))
	feed := getCooldown(t, srv)
	if feed.Mode != "hard" {
		t.Errorf("mode = %q, want hard (runtime global beats config)", feed.Mode)
	}
	if len(feed.Chains) != 2 {
		t.Fatalf("chains = %+v, want 2", feed.Chains)
	}
	// Chains are sorted by model: claude-x first, gpt-4o second.
	if feed.Chains[0].Model != "claude-x" || feed.Chains[0].Mode != "soft-demote" || feed.Chains[0].Effective != "soft-demote" {
		t.Errorf("chains[0] = %+v, want claude-x soft-demote", feed.Chains[0])
	}
	if feed.Chains[1].Model != "gpt-4o" || feed.Chains[1].Mode != "hard" || feed.Chains[1].Effective != "hard" {
		t.Errorf("chains[1] = %+v, want gpt-4o hard", feed.Chains[1])
	}
	if len(feed.Cooling) != 2 {
		t.Fatalf("cooling = %+v, want 2 targets", feed.Cooling)
	}
	if feed.Cooling[0].ProviderID != "slow-prov" || !feed.Cooling[0].Demoted || feed.Cooling[0].Failures != 2 || feed.Cooling[0].RemainingSeconds != 4 {
		t.Errorf("cooling[0] = %+v, want demoted slow-prov, failures 2, remaining 4s", feed.Cooling[0])
	}
	if feed.Cooling[0].Name != "Slow Provider" || feed.Cooling[1].Name != "Hot Provider" {
		t.Errorf("cooling names = %q / %q, want Slow Provider / Hot Provider (resolved from the config, not raw IDs)", feed.Cooling[0].Name, feed.Cooling[1].Name)
	}
	if feed.Cooling[1].ProviderID != "hot-prov" || feed.Cooling[1].Demoted || feed.Cooling[1].RemainingSeconds != 30 {
		t.Errorf("cooling[1] = %+v, want hard hot-prov, not demoted, remaining 30s", feed.Cooling[1])
	}
	if len(feed.Breakers) != 2 {
		t.Fatalf("breakers = %+v, want 2 states", feed.Breakers)
	}
	if b := feed.Breakers[0]; b.ProviderID != "slow-prov" || b.Open || b.ConsecutiveFailures != 2 || b.Threshold != 5 || b.LastReason != "upstream_error" || b.Ignored4xx != 3 {
		t.Errorf("breakers[0] = %+v, want closed slow-prov, 2/5, upstream_error, 3 ignored 4xx", b)
	}
	if b := feed.Breakers[0]; b.Name != "Slow Provider" {
		t.Errorf("breakers[0].Name = %q, want Slow Provider (resolved from the config, not the raw ID)", b.Name)
	}
	if b := feed.Breakers[1]; b.ProviderID != "hot-prov" || !b.Open || b.ConsecutiveFailures != 5 || b.LastReason != "timeout" || b.RemainingSeconds != 25 {
		t.Errorf("breakers[1] = %+v, want open hot-prov, 5/5, timeout, 25s left", b)
	}
	if feed.BreakerRule == "" || !strings.Contains(feed.BreakerRule, "4xx") {
		t.Errorf("breaker_rule = %q, want the live rule naming 4xx", feed.BreakerRule)
	}
}

// TestCooldownFeedLiveEngine proves the feed reports REAL engine
// state end to end: a retryable failure through the real
// router.Engine (soft-demote mode) inside the real Server shows up
// in GET /dashboard/cooldown as a demoted cooling target.
func TestCooldownFeedLiveEngine(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
	}))
	defer deadSrv.Close()
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"good says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer goodSrv.Close()
	deadClient, err := provider.NewClient(deadSrv.URL, "dead-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient dead: %v", err)
	}
	goodClient, err := provider.NewClient(goodSrv.URL, "good-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient good: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"dead": deadClient, "good": goodClient}, nil, router.Config{
		HedgeDelay:   -1, // hedging off: deterministic attempt order
		CooldownMode: router.CooldownSoftDemote,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "dead", Model: model}, {ProviderID: "good", Model: model}}
		},
	})
	srv := newTestServer(t, eng, WithModels("m"))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/chat/completions status = %d, body = %s", rec.Code, rec.Body.String())
	}
	feed := getCooldown(t, srv)
	if len(feed.Cooling) != 1 {
		t.Fatalf("cooling = %+v, want exactly one demoted target", feed.Cooling)
	}
	c := feed.Cooling[0]
	if c.ProviderID != "dead" || c.Mode != "soft-demote" || !c.Demoted || c.Failures != 1 {
		t.Errorf("cooling[0] = %+v, want dead / soft-demote / demoted / failures 1", c)
	}
	if c.RemainingSeconds < 1 || c.RemainingSeconds > 5 {
		t.Errorf("cooling[0].RemainingSeconds = %d, want 1..5 (5 s base)", c.RemainingSeconds)
	}
}

func TestCooldownUpdate(t *testing.T) {
	eng := &cooldownFakeEngine{}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/dashboard/cooldown", `{"cooldown_mode":"soft-demote","chain_cooldown_modes":{"gpt-4o":"hard"}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /dashboard/cooldown status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var feed cooldownResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode cooldown feed: %v", err)
	}
	if feed.Mode != "soft-demote" {
		t.Errorf("mode = %q, want soft-demote after POST", feed.Mode)
	}
	if len(feed.Chains) != 1 || feed.Chains[0].Model != "gpt-4o" || feed.Chains[0].Effective != "hard" {
		t.Errorf("chains = %+v, want gpt-4o hard", feed.Chains)
	}
	// No persist function wired in newTestServer: the update is
	// runtime-only and the feed must say so.
	if feed.Persisted {
		t.Error("persisted = true, want false (no persist function wired)")
	}
}

func TestCooldownUpdateRejectsUnknownMode(t *testing.T) {
	srv := newTestServer(t, &cooldownFakeEngine{})
	for _, body := range []string{
		`{"cooldown_mode":"skip"}`,
		`{"chain_cooldown_modes":{"gpt-4o":"demote"}}`,
		`{"chain_cooldown_modes":{"":"hard"}}`,
		`{"cooldown_mode":123}`,
	} {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/cooldown", body, "test-key-123")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400", body, rec.Code)
		}
	}
	// The failed POSTs must not have changed the mode.
	if feed := getCooldown(t, srv); feed.Mode != "hard" {
		t.Errorf("mode after rejected POSTs = %q, want hard (unchanged)", feed.Mode)
	}
}

func TestCooldownUpdatePersists(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	// WithConfig replaces the static test verifier with the config
	// verifier, so the POST needs a real key issued into this cfg;
	// the cooldown route requires the admin scope (guard table).
	key, raw, err := config.IssueKey("test", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	var saved []RoutingUpdate
	srv := newTestServer(t, &cooldownFakeEngine{}, WithConfig(cfg), WithRoutingPersist(func(u RoutingUpdate) error {
		saved = append(saved, u)
		return nil
	}))
	rec := doReq(t, srv, http.MethodPost, "/dashboard/cooldown", `{"cooldown_mode":"soft-demote"}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(saved) != 1 || saved[0].CooldownMode == nil || *saved[0].CooldownMode != "soft-demote" {
		t.Fatalf("persisted updates = %+v, want one with cooldown_mode soft-demote", saved)
	}
	if feed := getCooldown(t, srv); !feed.Persisted {
		t.Error("persisted = false, want true (persist function wired)")
	}
}

func TestCooldownFeedNoEngine(t *testing.T) {
	// A server with no engine at all still answers the feed with
	// the configured mode and an honest empty cooling list.
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Routing = &config.RoutingConfig{CooldownMode: "soft-demote"}
	srv := newTestServer(t, nil, WithConfig(cfg))
	feed := getCooldown(t, srv)
	if feed.Mode != "soft-demote" {
		t.Errorf("mode = %q, want soft-demote from config", feed.Mode)
	}
	if len(feed.Cooling) != 0 {
		t.Errorf("cooling = %+v, want empty with no engine", feed.Cooling)
	}
}
