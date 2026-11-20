// Group reason in the decision header (CHECKLIST 217): the
// `reason=group` value the 199-200 slice documented was covered
// only by a router unit test (internal/router/decision_test.go) —
// these are the server-level tests, through the real Server + real
// router.Engine against local mocks, proving the header a client
// actually sees names the group reason, the serving member, and
// the right provider/model — for both ways of naming a group
// (routing.group and the group:<name>/<model> alias) — and that a
// fallback inside the group still reports fallback-after-N, which
// outranks group by the documented precedence.
package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// TestDecisionHeaderGroupReason: a request through routing.group
// "fast" served by its first member carries
// reason=group — the header the 199-200 slice left covered only
// at router level. The groupStack engine has no configured
// strategy, so the effective strategy is the default "priority".
func TestDecisionHeaderGroupReason(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"group":"fast"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-a") {
		t.Fatalf("body = %s, want the a mock's content (first group member)", rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=a; model=m; strategy=priority; reason=group")
	if got := mocks["c"].hits.Load(); got != 0 {
		t.Errorf("c hits = %d, want 0: outside the group, never tried", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Errorf("trace contract = %+v, want group fast", tr.Contract)
	}
}

// TestDecisionHeaderGroupAliasReason: the same group addressed by
// the "group:fast/m" model alias carries reason=group with the
// resolved concrete model — never the virtual alias string.
func TestDecisionHeaderGroupAliasReason(t *testing.T) {
	srv, mocks, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"group:fast/m","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-a") {
		t.Fatalf("body = %s, want the a mock's content", rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=a; model=m; strategy=priority; reason=group")
	if got := mocks["c"].hits.Load(); got != 0 {
		t.Errorf("c hits = %d, want 0", got)
	}
}

// groupFallbackStack is groupStack with a dead first member: mock
// "a" answers 429 on chat, "b" serves. Group "fast" = [a, b].
func groupFallbackStack(t *testing.T) (*Server, *decisionUpstream, *decisionUpstream, string) {
	t.Helper()
	deadUp := &decisionUpstream{chatStatus: http.StatusTooManyRequests}
	deadSrv := newDecisionUpstream(t, deadUp)
	liveUp := &decisionUpstream{chatContent: "served-by-b"}
	liveSrv := newDecisionUpstream(t, liveUp)
	clients := map[string]provider.Client{
		"a": newDecisionClient(t, deadSrv),
		"b": newDecisionClient(t, liveSrv),
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
		{ID: "a", Name: "Alpha", BaseURL: deadSrv.URL, Enabled: true},
		{ID: "b", Name: "Beta", BaseURL: liveSrv.URL, Enabled: true},
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
	return srv, deadUp, liveUp, raw
}

// TestDecisionHeaderGroupFallbackBeatsGroup: the first group member
// 429s and the second serves — the header reports
// fallback-after-1, not group: a fallback is the most specific
// fact about why THIS provider served, so it wins over group by
// the documented precedence (internal/router/decision.go).
func TestDecisionHeaderGroupFallbackBeatsGroup(t *testing.T) {
	srv, deadUp, liveUp, key := groupFallbackStack(t)
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"routing":{"group":"fast"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "served-by-b") {
		t.Fatalf("body = %s, want the b mock's content (second group member)", rec.Body.String())
	}
	wantDecisionHeader(t, rec, "provider=b; model=m; strategy=priority; reason=fallback-after-1")
	if got := deadUp.chatHits.Load(); got != 1 {
		t.Errorf("dead mock chat hits = %d, want 1", got)
	}
	if got := liveUp.chatHits.Load(); got != 1 {
		t.Errorf("live mock chat hits = %d, want 1", got)
	}
	tr := contractTrace(t, srv)
	if tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Errorf("trace contract = %+v, want group fast", tr.Contract)
	}
	if len(tr.Attempts) != 2 || tr.Attempts[1].Provider != "b" {
		t.Errorf("trace attempts = %+v, want [a-failed, b-served]", tr.Attempts)
	}
}
