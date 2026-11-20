// User-named routing groups (CHECKLIST 173–174), server level:
// the live HTTP path — a group-addressed chat routes through the
// group's members, an unknown group is a 400 naming it, and
// GET /dashboard/groups lists the groups with live member health
// from the real engine.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// groupStack builds the real Server + real router.Engine + three
// real provider clients against local mocks, for model "m". Group
// "fast" = [a, b]; c is outside every group. The config carries
// the group definitions and the provider display names, wired via
// WithConfig so the groups feed can join them, plus one issued
// gateway key (returned raw) so the chat requests authenticate
// against the config verifier WithConfig installs.
func groupStack(t *testing.T) (*Server, map[string]*contractMock, *router.Engine, string) {
	t.Helper()
	mocks := map[string]*contractMock{}
	clients := map[string]provider.Client{}
	for _, id := range []string{"a", "b", "c"} {
		m := newContractMock(t, id)
		mocks[id] = m
		c, err := provider.NewClient(m.server.URL, id+"-key", provider.WithTimeout(2*time.Second))
		if err != nil {
			t.Fatalf("NewClient %s: %v", id, err)
		}
		clients[id] = c
	}
	eng := router.NewEngine(clients, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "m" {
				return nil
			}
			return []router.Attempt{
				{ProviderID: "a", Model: "m"},
				{ProviderID: "b", Model: "m"},
				{ProviderID: "c", Model: "m"},
			}
		},
		GroupChain: func(group, model string) ([]router.Attempt, bool) {
			if group != "fast" || model != "m" {
				return nil, false
			}
			return []router.Attempt{
				{ProviderID: "a", Model: "m"},
				{ProviderID: "b", Model: "m"},
			}, true
		},
	})
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", Name: "Alpha", BaseURL: mocks["a"].server.URL, Enabled: true},
		{ID: "b", Name: "Beta", BaseURL: mocks["b"].server.URL, Enabled: true},
		{ID: "c", Name: "Gamma", BaseURL: mocks["c"].server.URL, Enabled: true},
	}
	cfg.Routing = &config.RoutingConfig{
		Groups: map[string][]string{"fast": {"a", "b"}},
	}
	key, raw, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	cfg.GatewayKeys = []config.GatewayKey{key}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second), WithConfig(cfg))
	return srv, mocks, eng, raw
}

func groupChat(t *testing.T, srv *Server, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, key)
}

func groupsFeed(t *testing.T, srv *Server) groupsResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/groups", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/groups status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp groupsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode groups: %v (body %s)", err, rec.Body.String())
	}
	return resp
}

// TestServerGroupRoutesByName: routing.group "fast" serves from a
// (first member) and never touches c, and the trace names the
// group.
func TestServerGroupRoutesByName(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"group":"fast"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-a") {
		t.Fatalf("body = %s, want the a mock's content (first group member)", rec.Body.String())
	}
	if got := mocks["c"].hits.Load(); got != 0 {
		t.Fatalf("c hits = %d, want 0: outside the group, never tried", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Fatalf("trace contract = %+v, want group fast", tr.Contract)
	}
	if len(tr.Attempts) != 1 || tr.Attempts[0].Provider != "a" {
		t.Fatalf("trace attempts = %+v, want one attempt on a", tr.Attempts)
	}
}

// TestServerGroupAlias: the "group:<name>/<model>" model alias
// addresses the group over plain HTTP.
func TestServerGroupAlias(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"group:fast/m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-a") {
		t.Fatalf("body = %s, want the a mock's content", rec.Body.String())
	}
	if got := mocks["c"].hits.Load(); got != 0 {
		t.Fatalf("c hits = %d, want 0", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Fatalf("trace contract = %+v, want group fast", tr.Contract)
	}
}

// TestServerGroupUnknown400: an unknown group is a 400 naming it,
// before any provider is dialled — never a silent fallback.
func TestServerGroupUnknown400(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"group":"nope"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Fatalf("body = %s, want it to name the unknown group", rec.Body.String())
	}
	for id, m := range mocks {
		if got := m.hits.Load(); got != 0 {
			t.Fatalf("%s hits = %d, want 0: rejected before dialling", id, got)
		}
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Group != "nope" || tr.Contract.Error == "" {
		t.Fatalf("trace contract = %+v, want group nope with an error", tr.Contract)
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("trace attempts = %d, want 0", len(tr.Attempts))
	}
}

// TestServerGroupsFeed: GET /dashboard/groups lists the group with
// its members in config order, display names joined from the
// config, and live attempt health from the engine — no signal
// before any request, real counts after one.
func TestServerGroupsFeed(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	resp := groupsFeed(t, srv)
	if len(resp.Groups) != 1 {
		t.Fatalf("groups = %+v, want one group", resp.Groups)
	}
	g := resp.Groups[0]
	if g.Name != "fast" {
		t.Fatalf("group name = %q, want fast", g.Name)
	}
	if len(g.Members) != 2 {
		t.Fatalf("members = %+v, want [a b]", g.Members)
	}
	if g.Members[0].ProviderID != "a" || g.Members[0].Name != "Alpha" || !g.Members[0].Enabled {
		t.Errorf("member[0] = %+v, want a/Alpha/enabled", g.Members[0])
	}
	if g.Members[1].ProviderID != "b" || g.Members[1].Name != "Beta" || !g.Members[1].Enabled {
		t.Errorf("member[1] = %+v, want b/Beta/enabled", g.Members[1])
	}
	for i, m := range g.Members {
		if m.HasSignal || m.Attempts != 0 || m.BreakerOpen {
			t.Errorf("member[%d] = %+v, want no signal before any request", i, m)
		}
	}
	// One request through the group: a served it, b was never
	// tried — the feed must say exactly that, from live data.
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"group":"fast"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200", rec.Code)
	}
	resp = groupsFeed(t, srv)
	g = resp.Groups[0]
	if !g.Members[0].HasSignal || g.Members[0].Attempts != 1 || g.Members[0].Successes != 1 || g.Members[0].SuccessRate != 1 {
		t.Errorf("member[0] after one request = %+v, want 1 attempt, 1 success, rate 1", g.Members[0])
	}
	// A sub-millisecond local mock legitimately measures 0 ms —
	// the EWMA is a measured value, never invented; it just
	// must not be negative.
	if g.Members[0].LatencyMs < 0 {
		t.Errorf("member[0] latency = %v, want a measured value (>= 0)", g.Members[0].LatencyMs)
	}
	if g.Members[1].HasSignal || g.Members[1].Attempts != 0 {
		t.Errorf("member[1] after one request = %+v, want still no signal (never tried)", g.Members[1])
	}
	_ = mocks
}

// TestServerGroupsFeedEmpty: no groups configured answers an empty
// list — the honest empty state, never an invented group.
func TestServerGroupsFeedEmpty(t *testing.T) {
	srv, _, _ := contractStack(t)
	resp := groupsFeed(t, srv)
	if len(resp.Groups) != 0 {
		t.Fatalf("groups = %+v, want empty", resp.Groups)
	}
}
