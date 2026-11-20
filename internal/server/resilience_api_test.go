// Contract + behaviour tests for GET /api/resilience (M8.16).
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// newM816ResilienceServer builds a server whose live resilience state
// is fully known: openai's breaker OPEN (5/5), cc and glm closed, glm's
// only connection cooling. It reuses autoComboTestEngine (the fake
// engine with live breaker/cooldown reporting).
func newM816ResilienceServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "cc", Name: "cc", Enabled: true},
		{ID: "glm", Name: "glm", Enabled: true},
		{ID: "openai", Name: "openai", Enabled: true},
	}}
	eng := &autoComboTestEngine{
		breakers: []router.BreakerStatus{
			{ProviderID: "openai", Open: true, ConsecutiveFailures: 5, Threshold: 5},
			{ProviderID: "cc"},
			{ProviderID: "glm"},
		},
		cooling: []router.CoolingTarget{
			{ProviderID: "glm", KeyID: "conn-c", Mode: router.CooldownSoftDemote, RemainingSeconds: 30, Failures: 2, Demoted: true},
		},
	}
	return newTestServer(t, eng, WithConfig(cfg))
}

// TestM816ResilienceFixture is the apitest contract test: the handler
// must return the live breaker/cooldown state the fixture pins.
func TestM816ResilienceFixture(t *testing.T) {
	srv := newM816ResilienceServer(t)
	f := apitest.LoadFixture(t, "m8.16", "resilience-200")
	apitest.Run(t, http.HandlerFunc(srv.handleResilience), f)
}

// TestM816ResilienceLiveState proves the feed reads the engine at
// request time (not a startup snapshot): the OPEN breaker, the live
// cooling target and the honest-unknown hedge section are all
// present, and no hedge counts are invented.
func TestM816ResilienceLiveState(t *testing.T) {
	srv := newM816ResilienceServer(t)
	rec := httptest.NewRecorder()
	srv.handleResilience(rec, httptest.NewRequest(http.MethodGet, "/api/resilience", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp resilienceResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.EngineAvailable {
		t.Fatalf("engine_available = false, want true")
	}
	if len(resp.Breakers) != 3 {
		t.Fatalf("breakers = %d, want 3", len(resp.Breakers))
	}
	byID := map[string]resilienceBreakerJSON{}
	for _, b := range resp.Breakers {
		byID[b.ProviderID] = b
	}
	if openai := byID["openai"]; !openai.Open || openai.State != "OPEN" || openai.ConsecutiveFailures != 5 {
		t.Fatalf("openai breaker = %+v, want OPEN with 5 consecutive failures", openai)
	}
	if cc := byID["cc"]; cc.Open || cc.State != "CLOSED" {
		t.Fatalf("cc breaker = %+v, want CLOSED", cc)
	}
	if len(resp.Cooling) != 1 || resp.Cooling[0].ProviderID != "glm" || resp.Cooling[0].RemainingSeconds != 30 {
		t.Fatalf("cooling = %+v, want the live glm target with 30s remaining", resp.Cooling)
	}
	if resp.Hedge.Status != "unknown" || resp.Hedge.MaxBodyBytes != 256<<10 {
		t.Fatalf("hedge = %+v, want status unknown with the 256 KiB mirrored cap", resp.Hedge)
	}
	if !strings.Contains(resp.Hedge.Reason, "no hedge counter") {
		t.Fatalf("hedge reason = %q, want the honest-unknown reason", resp.Hedge.Reason)
	}
}

// TestM816ResilienceUnknownProvider proves an unprobed provider
// reports unknown: a configured provider the engine reports no
// breaker state for is listed as state "unknown", never defaulted
// to closed.
func TestM816ResilienceUnknownProvider(t *testing.T) {
	srv := newM816ResilienceServer(t)
	// "ghost" is configured but the engine never reports it.
	srv.cfg.Providers = append(srv.cfg.Providers, config.ProviderConfig{ID: "ghost", Name: "ghost", Enabled: true})
	rec := httptest.NewRecorder()
	srv.handleResilience(rec, httptest.NewRequest(http.MethodGet, "/api/resilience", nil))
	var resp resilienceResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, b := range resp.Breakers {
		if b.ProviderID == "ghost" {
			found = true
			if b.State != "unknown" {
				t.Fatalf("ghost state = %q, want unknown", b.State)
			}
			if b.Open {
				t.Fatalf("ghost open = true, want false for an unknown provider")
			}
		}
	}
	if !found {
		t.Fatalf("no breaker row for ghost provider; rows = %+v", resp.Breakers)
	}
}

// TestM816ResilienceNoEngine proves the honest empty state: with no
// engine wired the endpoint still answers 200 with engine_available
// false and empty breakers/cooling — never invented state.
func TestM816ResilienceNoEngine(t *testing.T) {
	srv := newTestServer(t, nil)
	rec := httptest.NewRecorder()
	srv.handleResilience(rec, httptest.NewRequest(http.MethodGet, "/api/resilience", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp resilienceResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.EngineAvailable {
		t.Fatalf("engine_available = true, want false with no engine")
	}
	if len(resp.Breakers) != 0 || len(resp.Cooling) != 0 {
		t.Fatalf("breakers/cooling = %d/%d, want 0/0 with no engine", len(resp.Breakers), len(resp.Cooling))
	}
}

// TestM816ResilienceMethodNotAllowed pins the method gate.
func TestM816ResilienceMethodNotAllowed(t *testing.T) {
	srv := newM816ResilienceServer(t)
	rec := httptest.NewRecorder()
	srv.handleResilience(rec, httptest.NewRequest(http.MethodPost, "/api/resilience", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestM816ResilienceToggleNegative proves the contract test is
// toggle-verified: the same fixture asserting a wrong status must
// fail CheckResponse, so a handler that answered differently (or a
// removed handler answering 404) cannot pass.
func TestM816ResilienceToggleNegative(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.16", "resilience-200")
	f.Assert.Status = http.StatusCreated
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := apitest.CheckResponse(resp, []byte(`{}`), f); err == nil {
		t.Fatalf("CheckResponse with assert 201 against a 200 response passed; toggle check is broken")
	}
}

// ---------------------------------------------------------------------------
// B6 (M10): resilience settings read/patch + breaker reset +
// per-model cooldowns.
// ---------------------------------------------------------------------------

// b6TestEngine is a fake engine with MUTABLE breaker/cooldown state
// plus a real breakerResetter implementation, so the B6 handlers
// are proven against live mutable state (trip -> reset -> closed)
// instead of canned values. It is NOT the production engine — but
// it is NOT short of it either: router.Engine DOES implement
// ResetBreaker / ResetAllBreakers (internal/router/router.go:2611
// / :2633), so the production reset path is a real reset, covered
// by the production-wrapped test in
// resilience_breaker_reset_wrapped_test.go — NOT by the 501 test
// below, which covers only an engine wired directly with no
// resetter at all (TestResilienceB6BreakerResetUnsupported501).
type b6TestEngine struct {
	fakeEngine
	mu         sync.Mutex
	breakers   map[string]router.BreakerStatus
	cooling    []router.CoolingTarget
	globalMode string
	chainModes map[string]string
}

func (e *b6TestEngine) BreakerStates() []router.BreakerStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]router.BreakerStatus, 0, len(e.breakers))
	for _, b := range e.breakers {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}

func (e *b6TestEngine) CooldownTargets() []router.CoolingTarget {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]router.CoolingTarget, len(e.cooling))
	copy(out, e.cooling)
	return out
}

func (e *b6TestEngine) CooldownState() (string, map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	chains := make(map[string]string, len(e.chainModes))
	for k, v := range e.chainModes {
		chains[k] = v
	}
	return e.globalMode, chains
}

func (e *b6TestEngine) SetCooldownMode(mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.globalMode = strings.TrimSpace(mode)
}

func (e *b6TestEngine) SetChainCooldownMode(model, mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if strings.TrimSpace(mode) == "" {
		delete(e.chainModes, model)
		return
	}
	if e.chainModes == nil {
		e.chainModes = map[string]string{}
	}
	e.chainModes[model] = strings.TrimSpace(mode)
}

// ResetBreaker clears one provider's breaker: open window,
// consecutive failures, trips and last reason all go, the
// threshold stays. False when the engine knows no breaker for
// that provider at all.
func (e *b6TestEngine) ResetBreaker(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.breakers[provider]
	if !ok {
		return false
	}
	b.Open = false
	b.State = router.BreakerClosed
	b.ConsecutiveFailures = 0
	b.OpenedAt = time.Time{}
	b.RemainingSeconds = 0
	b.LastReason = ""
	b.Trips = 0
	b.WindowOpen = false
	b.WindowFailures = 0
	e.breakers[provider] = b
	return true
}

// ResetAllBreakers resets every known breaker and reports the
// providers it actually reset, in sorted order.
func (e *b6TestEngine) ResetAllBreakers() []string {
	e.mu.Lock()
	ids := make([]string, 0, len(e.breakers))
	for id := range e.breakers {
		ids = append(ids, id)
	}
	e.mu.Unlock()
	sort.Strings(ids)
	reset := []string{}
	for _, id := range ids {
		if e.ResetBreaker(id) {
			reset = append(reset, id)
		}
	}
	return reset
}

// newB6Engine builds a B6 fake with a known live state: openai's
// breaker OPEN (5/5, tripped), cc and glm closed, glm's only
// connection cooling.
func newB6Engine() *b6TestEngine {
	return &b6TestEngine{
		breakers: map[string]router.BreakerStatus{
			"openai": {ProviderID: "openai", Open: true, State: router.BreakerOpen, ConsecutiveFailures: 5, Threshold: 5, LastReason: "upstream-5xx", Trips: 1},
			"cc":     {ProviderID: "cc", State: router.BreakerClosed, Threshold: 12},
			"glm":    {ProviderID: "glm", State: router.BreakerClosed, Threshold: 12},
		},
		cooling: []router.CoolingTarget{
			{ProviderID: "glm", KeyID: "conn-c", Mode: router.CooldownSoftDemote, RemainingSeconds: 30, Failures: 2, Demoted: true},
		},
		chainModes: map[string]string{},
	}
}

// newB6Server builds a server over a B6 fake engine with the three
// providers configured and one known model, so a per-model cooldown
// row survives clearing its override (it then reads as inheriting
// the global mode instead of vanishing from the feed).
func newB6Server(t *testing.T, eng *b6TestEngine, extra ...Option) *Server {
	t.Helper()
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "cc", Name: "cc", Enabled: true},
		{ID: "glm", Name: "glm", Enabled: true},
		{ID: "openai", Name: "openai", Enabled: true},
	}}
	opts := append([]Option{WithConfig(cfg), WithModels("openai/gpt-5")}, extra...)
	return newTestServer(t, eng, opts...)
}

func decodeB6Settings(t *testing.T, rec *httptest.ResponseRecorder) resilienceSettingsResponse {
	t.Helper()
	var resp resilienceSettingsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode settings: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

func b6BreakerByID(t *testing.T, resp resilienceSettingsResponse, id string) resilienceBreakerJSON {
	t.Helper()
	for _, b := range resp.Breakers {
		if b.ProviderID == id {
			return b
		}
	}
	t.Fatalf("no breaker row for %q; rows = %+v", id, resp.Breakers)
	return resilienceBreakerJSON{}
}

func b6ChainByModel(t *testing.T, resp resilienceSettingsResponse, model string) chainCooldownJSON {
	t.Helper()
	for _, c := range resp.Chains {
		if c.Model == model {
			return c
		}
	}
	t.Fatalf("no chain row for %q; chains = %+v", model, resp.Chains)
	return chainCooldownJSON{}
}

func b6GetSettings(t *testing.T, srv *Server) resilienceSettingsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleResilienceSettings(rec, httptest.NewRequest(http.MethodGet, "/api/resilience/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	return decodeB6Settings(t, rec)
}

// TestResilienceB6SettingsReadReflectsLiveState proves the settings
// read reflects the real breaker/cooldown state: the OPEN breaker,
// the live cooling target and the capability flags (reset
// supported, the honestly-unimplemented settings listed).
func TestResilienceB6SettingsReadReflectsLiveState(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	resp := b6GetSettings(t, srv)
	if resp.CooldownMode != router.CooldownHard {
		t.Fatalf("cooldown_mode = %q, want hard (no override set)", resp.CooldownMode)
	}
	if openai := b6BreakerByID(t, resp, "openai"); !openai.Open || openai.State != "OPEN" || openai.ConsecutiveFailures != 5 || openai.Threshold != 5 {
		t.Fatalf("openai breaker = %+v, want OPEN with 5 consecutive failures, threshold 5", openai)
	}
	if cc := b6BreakerByID(t, resp, "cc"); cc.Open || cc.State != "CLOSED" {
		t.Fatalf("cc breaker = %+v, want CLOSED", cc)
	}
	if len(resp.Cooling) != 1 || resp.Cooling[0].ProviderID != "glm" || resp.Cooling[0].RemainingSeconds != 30 {
		t.Fatalf("cooling = %+v, want the live glm target with 30s remaining", resp.Cooling)
	}
	if !resp.ResetSupported {
		t.Fatalf("reset_supported = false, want true for the resetter engine")
	}
	if resp.Persisted {
		t.Fatalf("persisted = true, want false with no persist function wired")
	}
	wantUnsupported := map[string]bool{"breaker_threshold": false, "breaker_open_for_seconds": false, "cooldown_seconds": false}
	for _, u := range resp.Unsupported {
		if _, ok := wantUnsupported[u.Field]; !ok {
			t.Fatalf("unsupported field %q is not a known unbacked setting", u.Field)
		}
		wantUnsupported[u.Field] = true
		if u.Reason == "" {
			t.Fatalf("unsupported field %q has no reason", u.Field)
		}
	}
	for field, seen := range wantUnsupported {
		if !seen {
			t.Fatalf("unsupported list is missing %q", field)
		}
	}
}

// TestResilienceB6SettingsPatchMode proves PATCH applies the real
// cooldown-mode settings: the global mode and a per-model override
// change, survive a re-read, and are reported persisted when a
// persist function is wired.
func TestResilienceB6SettingsPatchMode(t *testing.T) {
	var persisted []RoutingUpdate
	srv := newB6Server(t, newB6Engine(), WithRoutingPersist(func(u RoutingUpdate) error {
		persisted = append(persisted, u)
		return nil
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(`{"cooldown_mode": "soft-demote", "chain_cooldown_modes": {"openai/gpt-5": "hard"}}`))
	srv.handleResilienceSettingsPatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeB6Settings(t, rec)
	if resp.CooldownMode != router.CooldownSoftDemote {
		t.Fatalf("cooldown_mode = %q, want soft-demote", resp.CooldownMode)
	}
	if !resp.Persisted {
		t.Fatalf("persisted = false, want true with a persist function wired")
	}
	chain := b6ChainByModel(t, resp, "openai/gpt-5")
	if chain.Mode != router.CooldownHard || chain.Effective != router.CooldownHard {
		t.Fatalf("openai/gpt-5 chain = %+v, want mode/effective hard", chain)
	}
	if len(persisted) != 1 || persisted[0].CooldownMode == nil || *persisted[0].CooldownMode != router.CooldownSoftDemote {
		t.Fatalf("persisted updates = %+v, want one carrying the soft-demote global mode", persisted)
	}
	// A re-read shows the same applied state (not a one-shot response).
	reread := b6GetSettings(t, srv)
	if reread.CooldownMode != router.CooldownSoftDemote {
		t.Fatalf("re-read cooldown_mode = %q, want soft-demote", reread.CooldownMode)
	}
	if c := b6ChainByModel(t, reread, "openai/gpt-5"); c.Effective != router.CooldownHard {
		t.Fatalf("re-read openai/gpt-5 effective = %q, want hard", c.Effective)
	}
}

// TestResilienceB6SettingsPatchValidation proves PATCH rejects bad
// ranges with 400 — a breaker threshold of 0, a negative cooldown,
// an unknown mode — and applies nothing on a rejected patch.
func TestResilienceB6SettingsPatchValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"threshold zero", `{"breaker_threshold": 0}`},
		{"threshold negative", `{"breaker_threshold": -3}`},
		{"open-for zero", `{"breaker_open_for_seconds": 0}`},
		{"open-for negative", `{"breaker_open_for_seconds": -30}`},
		{"cooldown zero", `{"cooldown_seconds": 0}`},
		{"cooldown negative", `{"cooldown_seconds": -5}`},
		{"unknown global mode", `{"cooldown_mode": "turbo"}`},
		{"unknown chain mode", `{"chain_cooldown_modes": {"openai/gpt-5": "turbo"}}`},
		{"empty chain model", `{"chain_cooldown_modes": {"": "hard"}}`},
		{"empty patch", `{}`},
		{"malformed json", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newB6Server(t, newB6Engine())
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(tc.body))
			srv.handleResilienceSettingsPatch(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PATCH %s status = %d, want 400 (body: %s)", tc.body, rec.Code, rec.Body.String())
			}
			// Nothing applied: the global mode is still hard.
			if got := b6GetSettings(t, srv); got.CooldownMode != router.CooldownHard {
				t.Fatalf("mode after rejected patch = %q, want hard (nothing applied)", got.CooldownMode)
			}
		})
	}
}

// TestResilienceB6SettingsPatchUnsupported501 proves a PATCH with a
// valid but unbacked value answers 501 naming the field — and that
// a mixed patch (real mode + unbacked threshold) applies nothing,
// so a patch can never half-apply.
func TestResilienceB6SettingsPatchUnsupported501(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		field string
	}{
		{"threshold", `{"breaker_threshold": 12}`, "breaker_threshold"},
		{"open-for", `{"breaker_open_for_seconds": 30}`, "breaker_open_for_seconds"},
		{"cooldown", `{"cooldown_seconds": 60}`, "cooldown_seconds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newB6Server(t, newB6Engine())
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(tc.body))
			srv.handleResilienceSettingsPatch(rec, req)
			if rec.Code != http.StatusNotImplemented {
				t.Fatalf("PATCH %s status = %d, want 501 (body: %s)", tc.body, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.field) {
				t.Fatalf("501 body = %s, want it to name %q", rec.Body.String(), tc.field)
			}
		})
	}
	t.Run("mixed patch applies nothing", func(t *testing.T) {
		srv := newB6Server(t, newB6Engine())
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(`{"cooldown_mode": "soft-demote", "breaker_threshold": 12}`))
		srv.handleResilienceSettingsPatch(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("mixed PATCH status = %d, want 501 (body: %s)", rec.Code, rec.Body.String())
		}
		if got := b6GetSettings(t, srv); got.CooldownMode != router.CooldownHard {
			t.Fatalf("mode after 501 patch = %q, want hard (nothing applied)", got.CooldownMode)
		}
	})
}

// TestResilienceB6SettingsPatchPersistFailure proves a persist
// failure answers 500 and leaves the runtime mode unchanged.
func TestResilienceB6SettingsPatchPersistFailure(t *testing.T) {
	srv := newB6Server(t, newB6Engine(), WithRoutingPersist(func(RoutingUpdate) error {
		return errTestPersist
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(`{"cooldown_mode": "soft-demote"}`))
	srv.handleResilienceSettingsPatch(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PATCH status = %d, want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := b6GetSettings(t, srv); got.CooldownMode != router.CooldownHard {
		t.Fatalf("mode after failed persist = %q, want hard (unchanged)", got.CooldownMode)
	}
}

// TestResilienceB6BreakerResetTripped is the core reset proof: trip
// a breaker in-test (openai is OPEN in the fixture), reset it
// through the handler, and assert the live feed then shows it
// CLOSED with zero consecutive failures.
func TestResilienceB6BreakerResetTripped(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	before := b6GetSettings(t, srv)
	if openai := b6BreakerByID(t, before, "openai"); !openai.Open {
		t.Fatalf("fixture openai breaker is not OPEN; cannot prove a reset: %+v", openai)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/resilience/breakers/reset", strings.NewReader(`{"provider": "openai"}`))
	srv.handleResilienceBreakerReset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp resilienceBreakerResetResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	if len(resp.Reset) != 1 || resp.Reset[0] != "openai" || resp.ResetCount != 1 {
		t.Fatalf("reset response = %+v, want reset [openai] count 1", resp)
	}
	after := b6GetSettings(t, srv)
	if openai := b6BreakerByID(t, after, "openai"); openai.Open || openai.State != "CLOSED" || openai.ConsecutiveFailures != 0 {
		t.Fatalf("openai breaker after reset = %+v, want CLOSED with 0 consecutive failures", openai)
	}
}

// TestResilienceB6BreakerResetErrors pins the reset error mapping:
// unknown provider 404, empty request 400, wrong method 405.
func TestResilienceB6BreakerResetErrors(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	cases := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{"unknown provider", http.MethodPost, `{"provider": "ghost"}`, http.StatusNotFound},
		{"empty request", http.MethodPost, `{}`, http.StatusBadRequest},
		{"empty provider", http.MethodPost, `{"provider": "  "}`, http.StatusBadRequest},
		{"malformed json", http.MethodPost, `{nope`, http.StatusBadRequest},
		{"wrong method", http.MethodGet, `{"provider": "openai"}`, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/resilience/breakers/reset", strings.NewReader(tc.body))
			srv.handleResilienceBreakerReset(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("reset %s status = %d, want %d (body: %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestResilienceB6BreakerResetAll proves reset-all closes every
// tripped breaker and reports exactly the providers it reset.
func TestResilienceB6BreakerResetAll(t *testing.T) {
	eng := newB6Engine()
	// Trip a second breaker in-test: glm joins openai as OPEN.
	eng.mu.Lock()
	eng.breakers["glm"] = router.BreakerStatus{ProviderID: "glm", Open: true, State: router.BreakerOpen, ConsecutiveFailures: 12, Threshold: 12, LastReason: "upstream-5xx", Trips: 2}
	eng.mu.Unlock()
	srv := newB6Server(t, eng)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/resilience/breakers/reset", strings.NewReader(`{"all": true}`))
	srv.handleResilienceBreakerReset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-all status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp resilienceBreakerResetResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode reset-all response: %v", err)
	}
	if resp.ResetCount != 3 || len(resp.Reset) != 3 {
		t.Fatalf("reset-all = %+v, want all 3 known breakers reset", resp)
	}
	after := b6GetSettings(t, srv)
	for _, id := range []string{"openai", "glm", "cc"} {
		if b := b6BreakerByID(t, after, id); b.Open || b.State != "CLOSED" || b.ConsecutiveFailures != 0 {
			t.Fatalf("%s breaker after reset-all = %+v, want CLOSED with 0 failures", id, b)
		}
	}
}

// TestResilienceB6BreakerResetUnsupported501 proves the handler's
// fallback for an engine wired directly with no breakerResetter:
// the auto-combo fake — which, UNLIKE router.Engine (it DOES
// implement ResetBreaker / ResetAllBreakers,
// internal/router/router.go:2611/:2633) — gets 501, never a faked
// success. The production engine never takes this path: it reaches
// the handler through the degrade.StaleEngine wrapper, which
// forwards reset (see
// resilience_breaker_reset_wrapped_test.go).
func TestResilienceB6BreakerResetUnsupported501(t *testing.T) {
	srv := newM816ResilienceServer(t) // autoComboTestEngine: no resetter
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/resilience/breakers/reset", strings.NewReader(`{"provider": "openai"}`))
	srv.handleResilienceBreakerReset(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("reset status = %d, want 501 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no backing") {
		t.Fatalf("501 body = %s, want the honest no-backing reason", rec.Body.String())
	}
	// And the settings feed reports reset as unsupported for it.
	settings := b6GetSettings(t, srv)
	if settings.ResetSupported {
		t.Fatalf("reset_supported = true, want false for an engine without breakerResetter")
	}
}

// TestResilienceB6PerModelCooldownRoundTrip proves per-model
// cooldown set/clear round-trips through live state: set a mode
// for a model, see it effective on re-read, clear it, see the
// model inherit the global mode again.
func TestResilienceB6PerModelCooldownRoundTrip(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	model := "openai/gpt-5"

	// Set.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/resilience/cooldowns", strings.NewReader(`{"model": "`+model+`", "mode": "soft-demote"}`))
	srv.handleResilienceCooldownSet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("set status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeB6Settings(t, rec); b6ChainByModel(t, got, model).Effective != router.CooldownSoftDemote {
		t.Fatalf("chain after set = %+v, want effective soft-demote", got.Chains)
	}
	if reread := b6GetSettings(t, srv); b6ChainByModel(t, reread, model).Effective != router.CooldownSoftDemote {
		t.Fatalf("re-read chain = %+v, want effective soft-demote", reread.Chains)
	}

	// Clear, through the DELETE route with a real path value (the
	// handler reads r.PathValue("model"), which only a mux sets —
	// and the pattern needs the {model...} wildcard, because the
	// model id contains a slash).
	mux := http.NewServeMux()
	mux.Handle("DELETE /api/resilience/cooldowns/{model...}", http.HandlerFunc(srv.handleResilienceCooldownClear))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/resilience/cooldowns/"+model, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := decodeB6Settings(t, rec); b6ChainByModel(t, got, model).Effective != router.CooldownHard {
		t.Fatalf("chain after clear = %+v, want effective hard (inherit global)", got.Chains)
	}
	if reread := b6GetSettings(t, srv); b6ChainByModel(t, reread, model).Effective != router.CooldownHard {
		t.Fatalf("re-read chain after clear = %+v, want effective hard", reread.Chains)
	}
}

// TestResilienceB6PerModelCooldownErrors pins the set/clear error
// mapping: unknown mode, empty model/mode, wrong method, and a
// DELETE with no path value (no mux) all fail honestly.
func TestResilienceB6PerModelCooldownErrors(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	setCases := []struct {
		name   string
		method string
		body   string
		want   int
	}{
		{"unknown mode", http.MethodPost, `{"model": "openai/gpt-5", "mode": "turbo"}`, http.StatusBadRequest},
		{"empty model", http.MethodPost, `{"model": "", "mode": "hard"}`, http.StatusBadRequest},
		{"empty mode", http.MethodPost, `{"model": "openai/gpt-5", "mode": ""}`, http.StatusBadRequest},
		{"malformed json", http.MethodPost, `{nope`, http.StatusBadRequest},
		{"wrong method", http.MethodGet, `{"model": "openai/gpt-5", "mode": "hard"}`, http.StatusMethodNotAllowed},
	}
	for _, tc := range setCases {
		t.Run("set "+tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, "/api/resilience/cooldowns", strings.NewReader(tc.body))
			srv.handleResilienceCooldownSet(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("set %s status = %d, want %d (body: %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	t.Run("clear without path value", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, "/api/resilience/cooldowns/openai/gpt-5", nil)
		srv.handleResilienceCooldownClear(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("clear without mux status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("clear wrong method", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/resilience/cooldowns/openai/gpt-5", nil)
		srv.handleResilienceCooldownClear(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("clear GET status = %d, want 405 (body: %s)", rec.Code, rec.Body.String())
		}
	})
}

// TestResilienceB6SettingsMethodNotAllowed pins the method gates on
// the settings handlers.
func TestResilienceB6SettingsMethodNotAllowed(t *testing.T) {
	srv := newB6Server(t, newB6Engine())
	rec := httptest.NewRecorder()
	srv.handleResilienceSettings(rec, httptest.NewRequest(http.MethodPatch, "/api/resilience/settings", strings.NewReader(`{}`)))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET handler with PATCH status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.handleResilienceSettingsPatch(rec, httptest.NewRequest(http.MethodGet, "/api/resilience/settings", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH handler with GET status = %d, want 405", rec.Code)
	}
}
