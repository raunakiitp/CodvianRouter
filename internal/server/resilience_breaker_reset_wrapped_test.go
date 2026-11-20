// Regression test for reviewer 1ff6dc23 FAIL on builder d2c40977:
// POST /api/resilience/breakers/reset returned 501 in production
// wiring even though router.Engine implements breakerResetter,
// because the production engine is NOT the bare router.Engine —
// it is degrade.WrapWithSemantic(...) (cmd/codvian-router/main.go)
// optionally re-wrapped by degrade.WithSavings — and *StaleEngine
// forwarded BreakerStates/CooldownTargets/... but had NO
// ResetBreaker/ResetAllBreakers, so the handler's
// s.engine.(breakerResetter) assertion failed. These tests prove
// the reset is a real reset (200, or 404 for an unknown provider)
// through the PRODUCTION wrappers, not the bare engine.
//
// This file is package server_test (external), not package server:
// the server package's tests cannot import internal/degrade —
// degrade imports server, so that would be an import cycle. The
// fakes below therefore carry their own minimal engine plus live
// mutable breaker state, and the tests drive the real HTTP route
// (server.New + ServeHTTP) with a gateway key, exactly like a
// production caller.
package server_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

// breakerResetState is the mutable breaker/cooldown state shared by
// the two fakes below, mirroring b6TestEngine (package server):
// openai's breaker OPEN (5/5, tripped), cc and glm closed, glm's
// only connection cooling.
type breakerResetState struct {
	mu         sync.Mutex
	breakers   map[string]router.BreakerStatus
	cooling    []router.CoolingTarget
	globalMode string
	chainModes map[string]string
}

func newBreakerResetState() *breakerResetState {
	return &breakerResetState{
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

func (s *breakerResetState) BreakerStates() []router.BreakerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]router.BreakerStatus, 0, len(s.breakers))
	for _, b := range s.breakers {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProviderID < out[j].ProviderID })
	return out
}

func (s *breakerResetState) CooldownTargets() []router.CoolingTarget {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]router.CoolingTarget, len(s.cooling))
	copy(out, s.cooling)
	return out
}

func (s *breakerResetState) CooldownState() (string, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	chains := make(map[string]string, len(s.chainModes))
	for k, v := range s.chainModes {
		chains[k] = v
	}
	return s.globalMode, chains
}

func (s *breakerResetState) SetCooldownMode(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalMode = strings.TrimSpace(mode)
}

func (s *breakerResetState) SetChainCooldownMode(model, mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(mode) == "" {
		delete(s.chainModes, model)
		return
	}
	if s.chainModes == nil {
		s.chainModes = map[string]string{}
	}
	s.chainModes[model] = strings.TrimSpace(mode)
}

// breakerResetWrappedEngine is a fake engine WITH a real
// breakerResetter implementation, like router.Engine (ResetBreaker
// / ResetAllBreakers, internal/router/router.go:2611/:2633). The
// production wrappers are exercised by wrapping this engine —
// never the bare engine — so a missing forward in the wrapper
// fails here exactly as it failed in production.
type breakerResetWrappedEngine struct {
	*breakerResetState
}

func (e *breakerResetWrappedEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}

func (e *breakerResetWrappedEngine) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}

// ResetBreaker clears one provider's breaker, mirroring
// b6TestEngine: open window, consecutive failures, trips and last
// reason all go, the threshold stays. False when the engine knows
// no breaker for that provider at all.
func (e *breakerResetWrappedEngine) ResetBreaker(provider string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, ok := e.breakers[provider]
	if !ok {
		return false
	}
	b.Open = false
	b.State = router.BreakerClosed
	b.ConsecutiveFailures = 0
	b.LastReason = ""
	b.Trips = 0
	b.WindowOpen = false
	b.WindowFailures = 0
	e.breakers[provider] = b
	return true
}

// ResetAllBreakers resets every known breaker and reports the
// providers it actually reset, in sorted order.
func (e *breakerResetWrappedEngine) ResetAllBreakers() []string {
	ids := make([]string, 0, len(e.breakers))
	e.mu.Lock()
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

// breakerResetNoResetEngine is the same fake WITHOUT any reset
// method, like an engine that genuinely lacks breakerResetter. It
// pins the wrapper's honest fallback for such an engine (see
// TestResilienceBreakerResetWrappedNoResetter).
type breakerResetNoResetEngine struct {
	*breakerResetState
}

func (e *breakerResetNoResetEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}

func (e *breakerResetNoResetEngine) ChatStream(context.Context, provider.ChatRequest, func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{}, nil
}

// breakerResetWrappedResponse mirrors the reset endpoint's JSON
// (resilienceBreakerResetResponse is unexported in package server).
type breakerResetWrappedResponse struct {
	Reset      []string `json:"reset"`
	ResetCount int      `json:"reset_count"`
	Breakers   []struct {
		ProviderID          string `json:"provider"`
		State               string `json:"state"`
		Open                bool   `json:"open"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
	} `json:"breakers"`
}

func newBreakerResetWrappedServer(t *testing.T, eng server.Engine) *server.Server {
	t.Helper()
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "cc", Name: "cc", Enabled: true},
		{ID: "glm", Name: "glm", Enabled: true},
		{ID: "openai", Name: "openai", Enabled: true},
	}}
	return server.New(eng,
		server.WithConfig(cfg),
		server.WithStaticKeys("test-key-123"),
		server.WithLogger(log.New(io.Discard, "", 0)),
	)
}

func doBreakerResetWrapped(t *testing.T, srv *server.Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer test-key-123")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeBreakerResetWrapped(t *testing.T, rec *httptest.ResponseRecorder) breakerResetWrappedResponse {
	t.Helper()
	var resp breakerResetWrappedResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode reset response: %v (body: %s)", err, rec.Body.String())
	}
	return resp
}

// TestResilienceBreakerResetProductionWrappedEngine is the M9.10
// live-check: POST /api/resilience/breakers/reset against the
// PRODUCTION-wrapped engine must be a real reset — 200 for a known
// provider (and the breaker visibly closed afterwards), 404 for an
// unknown provider — never 501. Both production wrappings are
// covered: WrapWithSemantic alone (cmd/codvian-router/main.go)
// and WithSavings over it (the *savingsEngine embedding, which
// must promote the reset methods through *StaleEngine).
func TestResilienceBreakerResetProductionWrappedEngine(t *testing.T) {
	wrappings := map[string]func(t *testing.T, eng *breakerResetWrappedEngine) server.Engine{
		"wrap-with-semantic": func(t *testing.T, eng *breakerResetWrappedEngine) server.Engine {
			t.Helper()
			wrapped := degrade.WrapWithSemantic(eng, nil, nil, degrade.DefaultPolicy())
			if _, ok := wrapped.(*degrade.StaleEngine); !ok {
				t.Fatalf("WrapWithSemantic returned %T, want *degrade.StaleEngine", wrapped)
			}
			return wrapped
		},
		"with-savings": func(t *testing.T, eng *breakerResetWrappedEngine) server.Engine {
			t.Helper()
			wrapped := degrade.WrapWithSemantic(eng, nil, nil, degrade.DefaultPolicy())
			stale, ok := wrapped.(*degrade.StaleEngine)
			if !ok {
				t.Fatalf("WrapWithSemantic returned %T, want *degrade.StaleEngine", wrapped)
			}
			return degrade.WithSavings(stale, func() cache.SavingsTotals { return cache.SavingsTotals{} })
		},
	}
	for name, wrap := range wrappings {
		t.Run(name, func(t *testing.T) {
			srv := newBreakerResetWrappedServer(t, wrap(t, &breakerResetWrappedEngine{newBreakerResetState()}))

			// Known provider: 200, and the breaker is closed after.
			rec := doBreakerResetWrapped(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "openai"}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("reset openai status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			resp := decodeBreakerResetWrapped(t, rec)
			if resp.ResetCount != 1 || len(resp.Reset) != 1 || resp.Reset[0] != "openai" {
				t.Fatalf("reset openai = %+v, want reset [openai]", resp)
			}
			for _, b := range resp.Breakers {
				if b.ProviderID == "openai" && (b.Open || b.State != "CLOSED" || b.ConsecutiveFailures != 0) {
					t.Fatalf("openai breaker after reset = %+v, want CLOSED with 0 failures", b)
				}
			}

			// Unknown provider: 404, never 501.
			rec = doBreakerResetWrapped(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "no-such-provider"}`)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("reset unknown status = %d, want 404 (body: %s)", rec.Code, rec.Body.String())
			}

			// Settings feed agrees reset is supported through the wrapper.
			rec = doBreakerResetWrapped(t, srv, http.MethodGet, "/api/resilience/settings", "")
			if rec.Code != http.StatusOK {
				t.Fatalf("settings status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
			}
			var settings struct {
				ResetSupported bool `json:"reset_supported"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&settings); err != nil {
				t.Fatalf("decode settings: %v (body: %s)", err, rec.Body.String())
			}
			if !settings.ResetSupported {
				t.Fatalf("reset_supported = false, want true for the production-wrapped engine")
			}
		})
	}
}

// TestResilienceBreakerResetAllProductionWrappedEngine proves
// reset-all through the production wrapper: 200 with every known
// breaker reset and closed afterwards.
func TestResilienceBreakerResetAllProductionWrappedEngine(t *testing.T) {
	eng := &breakerResetWrappedEngine{newBreakerResetState()}
	wrapped := degrade.WrapWithSemantic(eng, nil, nil, degrade.DefaultPolicy())
	srv := newBreakerResetWrappedServer(t, wrapped)
	rec := doBreakerResetWrapped(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"all": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-all status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	resp := decodeBreakerResetWrapped(t, rec)
	if resp.ResetCount != 3 || len(resp.Reset) != 3 {
		t.Fatalf("reset-all = %+v, want all 3 known breakers reset", resp)
	}
	for _, b := range resp.Breakers {
		if b.Open || b.State != "CLOSED" || b.ConsecutiveFailures != 0 {
			t.Fatalf("breaker %q after reset-all = %+v, want CLOSED with 0 failures", b.ProviderID, b)
		}
	}
}

// TestResilienceBreakerResetWrappedNoResetter pins the wrapper's
// honest fallback for a wrapped engine that genuinely exposes no
// reset: the fixed bool/[]string signatures cannot carry a "not
// implemented" distinct from "no breaker known", so the wrapper
// reports nothing reset — ResetBreaker false (the handler answers
// 404, "no breaker known") and ResetAllBreakers nil (the handler
// answers 200 with an empty reset list). The handler's 501 stays
// reachable only for an engine wired directly without the wrapper
// (TestResilienceB6BreakerResetUnsupported501, package server).
func TestResilienceBreakerResetWrappedNoResetter(t *testing.T) {
	eng := &breakerResetNoResetEngine{newBreakerResetState()}
	wrapped := degrade.WrapWithSemantic(eng, nil, nil, degrade.DefaultPolicy())
	srv := newBreakerResetWrappedServer(t, wrapped)

	rec := doBreakerResetWrapped(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"provider": "openai"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("reset openai status = %d, want 404 for a wrapped engine with no resetter (body: %s)", rec.Code, rec.Body.String())
	}
	rec = doBreakerResetWrapped(t, srv, http.MethodPost, "/api/resilience/breakers/reset", `{"all": true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset-all status = %d, want 200 with an empty reset list (body: %s)", rec.Code, rec.Body.String())
	}
	if resp := decodeBreakerResetWrapped(t, rec); resp.ResetCount != 0 || len(resp.Reset) != 0 {
		t.Fatalf("reset-all = %+v, want nothing reset for a wrapped engine with no resetter", resp)
	}
}
