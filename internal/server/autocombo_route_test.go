// Route tests for GET /v1/auto-combo/{channel}/candidates (Phase D,
// builder D1). These test through the public HTTP interface: the
// registered route, the live pool (autorouter tier models +
// per-key connections + engine breaker/cooldown state), and the
// 404/400 error mapping. The pure handler is covered separately
// in autocombo_handler_test.go.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// autoComboTestEngine is the fakeEngine plus live breaker/cooldown
// reporting, so the route test proves the handler reads real
// engine state (not canned values): one provider's breaker is
// OPEN, one connection is cooling.
type autoComboTestEngine struct {
	fakeEngine
	breakers []router.BreakerStatus
	cooling  []router.CoolingTarget
}

func (e *autoComboTestEngine) BreakerStates() []router.BreakerStatus { return e.breakers }
func (e *autoComboTestEngine) CooldownTargets() []router.CoolingTarget {
	return e.cooling
}

// The remaining cooldownReporter methods: this fake never changes
// modes, so they are no-ops / empty state. They exist only so the
// fake satisfies the full interface the route asserts on.
func (e *autoComboTestEngine) CooldownState() (string, map[string]string) {
	return "", nil
}
func (e *autoComboTestEngine) SetCooldownMode(string)              {}
func (e *autoComboTestEngine) SetChainCooldownMode(string, string) {}

// autoComboFakeKeyManager is a ProviderKeyManager backed by an
// in-memory provider map. It stands in for the live
// *provider.Registry (whose Add dials the network, so a real
// registry cannot be built in-process here); the route reads it
// only through the ProviderKeyManager interface, exactly as it
// reads the production registry.
type autoComboFakeKeyManager struct {
	providers map[string]*provider.Provider
}

func (m *autoComboFakeKeyManager) Get(id string) (*provider.Provider, bool) {
	p, ok := m.providers[id]
	if !ok {
		return nil, false
	}
	return p.Clone(), true
}

func (m *autoComboFakeKeyManager) List() []*provider.Provider {
	out := make([]*provider.Provider, 0, len(m.providers))
	for _, p := range m.providers {
		out = append(out, p.Clone())
	}
	return out
}

func (m *autoComboFakeKeyManager) AddKey(ctx context.Context, providerID, apiKey, role string) (provider.ProviderKey, error) {
	return provider.ProviderKey{}, nil
}

func (m *autoComboFakeKeyManager) RemoveKey(providerID, keyID string) error { return nil }
func (m *autoComboFakeKeyManager) SetKeyRole(providerID, keyID, role string) error {
	return nil
}

func (m *autoComboFakeKeyManager) RotateKey(ctx context.Context, providerID, keyID, apiKey string) (provider.ProviderKey, error) {
	return provider.ProviderKey{}, nil
}

func (m *autoComboFakeKeyManager) SetEnabled(providerID string, enabled bool) error {
	p, ok := m.providers[providerID]
	if !ok {
		return provider.ErrProviderNotFound
	}
	p.Enabled = enabled
	return nil
}

// newAutoComboTestServer builds a server whose live state is fully
// known: tier models for coding/reasoning/chat/quick-qa, one key
// per provider in the config, openai's breaker OPEN, glm's only
// connection cooling. It returns the server and a models-scoped
// gateway key: WithConfig replaces the static test verifier with
// the config verifier, so the static "test-key-123" does not
// authenticate here.
func newAutoComboTestServer(t *testing.T) (*Server, string) {
	return newAutoComboTestServerWith(t)
}

// newAutoComboTestServerWith is newAutoComboTestServer plus extra
// server options (e.g. WithPriceBook for the free-tier tests).
func newAutoComboTestServerWith(t *testing.T, extra ...Option) (*Server, string) {
	t.Helper()
	ar := autorouter.New(autorouter.Config{TierModels: map[autorouter.Tier][]string{
		autorouter.TierCoding:    {"cc/claude-opus", "openai/gpt-5"},
		autorouter.TierReasoning: {"openai/gpt-5", "deepseek/deepseek-chat"},
		autorouter.TierChat:      {"openai/gpt-5"},
		autorouter.TierQuickQA:   {"glm/glm-free"},
	}})
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "cc", Name: "cc", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-a"}}},
		{ID: "openai", Name: "openai", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-b"}}},
		{ID: "deepseek", Name: "deepseek", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-d"}}},
		{ID: "glm", Name: "glm", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-c"}}},
	}}
	k, raw, err := config.IssueKey("autocombo-test", []string{"models"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	eng := &autoComboTestEngine{
		breakers: []router.BreakerStatus{
			{ProviderID: "openai", Open: true, ConsecutiveFailures: 5, Threshold: 5},
			{ProviderID: "cc"},
			{ProviderID: "deepseek"},
			{ProviderID: "glm"},
		},
		cooling: []router.CoolingTarget{
			{ProviderID: "glm", KeyID: "conn-c", Mode: router.CooldownSoftDemote, RemainingSeconds: 30, Failures: 2, Demoted: true},
		},
	}
	opts := []Option{WithAutoRouter(ar), WithConfig(cfg)}
	opts = append(opts, extra...)
	return newTestServer(t, eng, opts...), raw
}

// autoComboTestPriceBook prices every tier model in the test
// server: glm-free is $0, the other three are priced. Entries
// are keyed by bare model id, exactly as production's free
// derivation reads them (BlendedUSDPer1M).
func autoComboTestPriceBook() *config.PriceBook {
	return &config.PriceBook{
		Entries: map[string]config.ModelPrice{
			"glm-free":      {InputUSDPer1M: pricePtr(0), OutputUSDPer1M: pricePtr(0)},
			"gpt-5":         {InputUSDPer1M: pricePtr(2.5), OutputUSDPer1M: pricePtr(10)},
			"claude-opus":   {InputUSDPer1M: pricePtr(5), OutputUSDPer1M: pricePtr(15)},
			"deepseek-chat": {InputUSDPer1M: pricePtr(1), OutputUSDPer1M: pricePtr(2)},
		},
	}
}

// TestAutoComboRouteBestFreeNarrowsToZeroPrice proves the free
// TierFilter is applied from the live price book: best-free lists
// only the known-$0 model. Removing the narrowing fails this
// test (the whole 4-model union would be listed).
func TestAutoComboRouteBestFreeNarrowsToZeroPrice(t *testing.T) {
	srv, key := newAutoComboTestServerWith(t, WithPriceBook(autoComboTestPriceBook()))
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/best-free/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET best-free candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if res.Channel != "auto/best-free" {
		t.Fatalf("channel = %q, want auto/best-free", res.Channel)
	}
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d rows %+v, want 1 (only glm/glm-free is $0)", len(res.Candidates), res.Candidates)
	}
	if c := findAutoComboRow(t, res, "glm/glm-free"); c.Provider != "glm" || c.ConnectionID != "conn-c" {
		t.Fatalf("glm row = %+v, want provider glm conn-c", c)
	}
}

// TestAutoComboRouteBestFreeFailOpenWithoutPrices proves the free
// filter is fail-open exactly like the resolver's: with no price
// book no model can be proven $0, so the whole union is listed
// (dispatch would do the same — the resolver's tier filter
// fail-opens on an empty match).
func TestAutoComboRouteBestFreeFailOpenWithoutPrices(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/best-free/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET best-free candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 4 {
		t.Fatalf("candidates = %d rows %+v, want 4 (whole tier union, fail-open: no price data)", len(res.Candidates), res.Candidates)
	}
}

// TestAutoComboRouteBestReasoningNarrowsByCategory proves the
// reasoning category filter is applied from live tier membership:
// best-reasoning lists only deepseek-chat. openai/gpt-5 is in
// the reasoning tier but its derived category is "coding" —
// first-wins in AllTiers order (Coding precedes Reasoning), the
// same derivation production uses — so it is not a
// reasoning-category model and is excluded, exactly as the
// resolver's filter would exclude it. Removing the narrowing
// fails this test (gpt-5 would be listed too).
func TestAutoComboRouteBestReasoningNarrowsByCategory(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/best-reasoning/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET best-reasoning candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d rows %+v, want 1 (only deepseek/deepseek-chat is reasoning-category)", len(res.Candidates), res.Candidates)
	}
	if c := findAutoComboRow(t, res, "deepseek/deepseek-chat"); c.Provider != "deepseek" || c.ConnectionID != "conn-d" {
		t.Fatalf("deepseek row = %+v, want provider deepseek conn-d", c)
	}
}

func decodeAutoComboCandidates(t *testing.T, body []byte) AutoComboCandidatesResult {
	t.Helper()
	var res AutoComboCandidatesResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("candidates body is not JSON: %v (%s)", err, body)
	}
	return res
}

func findAutoComboRow(t *testing.T, res AutoComboCandidatesResult, model string) AutoComboCandidate {
	t.Helper()
	for _, c := range res.Candidates {
		if c.Model == model {
			return c
		}
	}
	t.Fatalf("candidates %+v contain no row for model %q", res.Candidates, model)
	return AutoComboCandidate{}
}

// TestAutoComboRouteKnownChannelLivePool proves (a): a known
// channel returns the documented shape with LIVE breaker states —
// the coding pool is coding+reasoning tier models, openai's row
// is listed with reachable:false because its breaker is OPEN
// (never dropped), and cc/deepseek rows are reachable.
func TestAutoComboRouteKnownChannelLivePool(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if res.Channel != "auto/coding" {
		t.Fatalf("channel = %q, want auto/coding", res.Channel)
	}
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %d rows %+v, want 3 (cc/claude-opus, openai/gpt-5, deepseek/deepseek-chat — gpt-5 deduped across tiers)", len(res.Candidates), res.Candidates)
	}
	if c := findAutoComboRow(t, res, "cc/claude-opus"); !c.Reachable || c.BreakerState != "CLOSED" || c.ConnectionID != "conn-a" || c.Provider != "cc" {
		t.Fatalf("cc row = %+v, want reachable CLOSED conn-a", c)
	}
	if c := findAutoComboRow(t, res, "openai/gpt-5"); c.Reachable || c.BreakerState != "OPEN" || c.ConnectionID != "conn-b" {
		t.Fatalf("openai row = %+v, want reachable=false breakerState=OPEN conn-b (breaker OPEN is listed, never dropped)", c)
	}
	if c := findAutoComboRow(t, res, "deepseek/deepseek-chat"); !c.Reachable || c.BreakerState != "CLOSED" || c.ConnectionID != "conn-d" {
		t.Fatalf("deepseek row = %+v, want reachable CLOSED conn-d", c)
	}
}

// TestAutoComboRouteUnknownChannel404 proves (b): an unknown
// channel answers 404.
func TestAutoComboRouteUnknownChannel404(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/bogus/candidates", "", key)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown channel: status %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestAutoComboRouteMalformedChannel400 proves (c): a malformed
// channel answers 400 — a dot is outside the route's
// ^[a-zA-Z0-9:_-]+$ shape, and so is a 129-char segment.
func TestAutoComboRouteMalformedChannel400(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	for _, ch := range []string{"bad.dot", strings.Repeat("a", 129)} {
		rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/"+ch+"/candidates", "", key)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("channel %q: status %d, want 400: %s", ch, rec.Code, rec.Body.String())
		}
	}
}

// TestAutoComboRouteCoolingConnectionListed proves the live
// cooldown read: glm's only connection is cooling, so its row is
// listed with reachable:false and connectionCooldown:true.
func TestAutoComboRouteCoolingConnectionListed(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/cheap/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cheap candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	c := findAutoComboRow(t, res, "glm/glm-free")
	if c.Reachable || !c.ConnectionCooldown || c.ConnectionID != "conn-c" {
		t.Fatalf("glm row = %+v, want reachable=false connectionCooldown=true conn-c", c)
	}
}

// TestAutoComboRouteRegistered proves (e): the route exists in the
// guard table as a non-exempt TierManagement GET with the models
// scope, and the mux actually serves it — without a key it
// answers 401 (auth gate), not 404 (no such route). Removing the
// registration fails both halves.
func TestAutoComboRouteRegistered(t *testing.T) {
	found := false
	for _, r := range RouteTable() {
		if r.Method == http.MethodGet && r.Path == "/v1/auto-combo/{channel}/candidates" {
			found = true
			if r.Tier != TierManagement {
				t.Errorf("route tier = %s, want MANAGEMENT", r.Tier)
			}
			if r.AuthExempt {
				t.Errorf("route is auth-exempt; candidate listing requires a gateway key like the other /v1 GETs")
			}
			hasModels := false
			for _, s := range r.Scopes {
				if s == "models" {
					hasModels = true
				}
			}
			if !hasModels {
				t.Errorf("route scopes = %v, want them to include \"models\"", r.Scopes)
			}
		}
	}
	if !found {
		t.Fatal("guard table has no GET /v1/auto-combo/{channel}/candidates entry")
	}
	srv, _ := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d, want 401 (route registered and auth-gated, not 404)", rec.Code)
	}
}

// TestAutoComboRouteLiveRegistryKeys proves per-key connections
// come from the LIVE provider registry first: with a key manager
// wired, the row's connection id is the registry key id — even
// though the config names a different key for the same provider.
func TestAutoComboRouteLiveRegistryKeys(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	srv.providerKeyManager = &autoComboFakeKeyManager{providers: map[string]*provider.Provider{
		"cc": {ID: "cc", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-live"}}},
	}}
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if c := findAutoComboRow(t, res, "cc/claude-opus"); c.ConnectionID != "conn-live" {
		t.Fatalf("cc row connectionId = %q, want conn-live (live registry wins over the config key conn-a)", c.ConnectionID)
	}
}

// TestAutoComboRouteNoAutoRouter503 proves the honest failure when
// no auto-router is wired: 503, never an invented pool.
func TestAutoComboRouteNoAutoRouter503(t *testing.T) {
	srv := newTestServer(t, &autoComboTestEngine{})
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", "test-key-123")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no auto-router: status %d, want 503: %s", rec.Code, rec.Body.String())
	}
}
