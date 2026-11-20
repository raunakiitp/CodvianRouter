// Tests for B2-15 candidate-pool endpoint gaps (V-B215 toggle-proven):
// the exclusions store, the ModelLockouts forwarder, and the
// FreeAccessExclusion derivation had zero HTTP-level coverage.
// These test through the public HTTP interface: the registered
// route, the live pool, and real server/engine state — no mocks
// of the handler logic. The only fakes are the engine (a
// lockout-reporting wrapper around the existing autoCombo test
// engine) and the operator overrides file (a real file on disk,
// loaded by the real store).
package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// b525LockoutEngine is autoComboTestEngine plus live model
// lockouts, so the route test proves the handler reads the
// engine's real ModelLockouts() (via the lockoutReporter
// interface), not a canned value.
type b525LockoutEngine struct {
	*autoComboTestEngine
	lockouts []router.LockoutStatus
}

func (e *b525LockoutEngine) ModelLockouts() []router.LockoutStatus {
	return e.lockouts
}

// newB525TestServer mirrors newAutoComboTestServerWith (same tier
// models, same per-provider keys, openai's breaker OPEN, glm's
// only connection cooling) but also returns the gateway key's id
// (needed to write an overrides row for the caller) and wraps the
// engine with a lockout reporter when lockouts are given.
func newB525TestServer(t *testing.T, lockouts []router.LockoutStatus, extra ...Option) (*Server, string, string) {
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
	k, raw, err := config.IssueKey("b525-test", []string{"models"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	base := &autoComboTestEngine{
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
	var eng Engine = base
	if lockouts != nil {
		eng = &b525LockoutEngine{autoComboTestEngine: base, lockouts: lockouts}
	}
	opts := []Option{WithAutoRouter(ar), WithConfig(cfg)}
	opts = append(opts, extra...)
	return newTestServer(t, eng, opts...), raw, k.ID
}

func b525StrOrNil(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// TestB525AutoComboExclusionsStoreHTTP proves the exclusions
// store is live at the endpoint: an excluded connection (via the
// operator-managed auto-candidate-overrides.json) answers
// excluded=true, and only for the (key, channel, connection) the
// row names. The control half (no overrides installed) proves
// the flag is driven by the store, not always-on.
func TestB525AutoComboExclusionsStoreHTTP(t *testing.T) {
	srv, key, keyID := newB525TestServer(t, nil)

	// Control: no overrides installed — nothing is excluded.
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates (no overrides): status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %d rows %+v, want 3 (coding pool)", len(res.Candidates), res.Candidates)
	}
	for _, c := range res.Candidates {
		if c.Excluded {
			t.Fatalf("row %+v excluded with no overrides file installed", c)
		}
	}

	// Install an overrides file excluding cc's conn-a for this
	// key on the coding channel, then re-list.
	dir := t.TempDir()
	path := filepath.Join(dir, "auto-candidate-overrides.json")
	file := fmt.Sprintf(`{"overrides": [{"key_id": %q, "channel": "auto/coding", "connection_id": "conn-a", "excluded": true}]}`, keyID)
	if err := os.WriteFile(path, []byte(file), 0600); err != nil {
		t.Fatalf("write overrides file: %v", err)
	}
	srv.autoOverrides = newAutoCandidateOverrideStore(path)

	rec = doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates (overrides installed): status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res = decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %d rows %+v, want 3 (excluded rows are listed, never dropped)", len(res.Candidates), res.Candidates)
	}
	if c := findAutoComboRow(t, res, "cc/claude-opus"); !c.Excluded {
		t.Fatalf("cc row = %+v, want excluded=true (overrides file excludes conn-a for this key+channel)", c)
	} else if !c.Reachable {
		t.Fatalf("cc row = %+v, want reachable=true (exclusion is a flag, not a drop)", c)
	}
	for _, model := range []string{"openai/gpt-5", "deepseek/deepseek-chat"} {
		if c := findAutoComboRow(t, res, model); c.Excluded {
			t.Fatalf("%s row = %+v, want excluded=false (no overrides row names it)", model, c)
		}
	}
}

// TestB525AutoComboModelLockedHTTP proves the ModelLocked half
// end to end: a model the engine reports live-locked (via the
// StaleEngine.ModelLockouts forwarder in production, via the
// lockoutReporter here) is listed with modelLocked=true and
// reachable=false, while its neighbours are unaffected. The
// control half (no lockouts) proves the flag is driven by the
// engine report, not always-on.
func TestB525AutoComboModelLockedHTTP(t *testing.T) {
	// Control: no lockouts — the cc row is reachable.
	srv, key, _ := newB525TestServer(t, nil)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates (no lockouts): status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if c := findAutoComboRow(t, res, "cc/claude-opus"); !c.Reachable || c.ModelLocked {
		t.Fatalf("cc row without lockouts = %+v, want reachable=true modelLocked=false", c)
	}

	// Locked: cc/conn-a/claude-opus live-locked (bare model id,
	// exactly as the engine's lockout table keys it).
	srv2, key2, _ := newB525TestServer(t, []router.LockoutStatus{
		{ProviderID: "cc", KeyID: "conn-a", Model: "claude-opus", Failures: 3},
	})
	rec = doReq(t, srv2, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key2)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates (lockout live): status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res = decodeAutoComboCandidates(t, rec.Body.Bytes())
	if c := findAutoComboRow(t, res, "cc/claude-opus"); c.Reachable || !c.ModelLocked || c.BreakerState != "CLOSED" {
		t.Fatalf("cc row with lockout = %+v, want reachable=false modelLocked=true breakerState=CLOSED (lockout alone blocks it)", c)
	}
	if c := findAutoComboRow(t, res, "deepseek/deepseek-chat"); !c.Reachable || c.ModelLocked {
		t.Fatalf("deepseek row = %+v, want reachable=true modelLocked=false (lockout is per (provider, key, model) triple)", c)
	}
}

// TestB525AutoComboFreeAccessExclusionHTTP proves the
// FreeAccessExclusion derivation at the HTTP level: a known-$0
// model yields nil, a priced model yields "not-in-catalog" —
// the price-only derivation autoComboFreeAccessExclusion
// documents (known $0 by the price book, else not-in-catalog).
func TestB525AutoComboFreeAccessExclusionHTTP(t *testing.T) {
	srv, key, _ := newB525TestServer(t, nil, WithPriceBook(autoComboTestPriceBook()))

	// Cheap channel: glm-free is known-$0, gpt-5 is priced.
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/cheap/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cheap candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if c := findAutoComboRow(t, res, "glm/glm-free"); c.FreeAccessExclusion != nil {
		t.Fatalf("glm row freeAccessExclusion = %q, want nil (known $0 by the price book)", *c.FreeAccessExclusion)
	}
	if c := findAutoComboRow(t, res, "openai/gpt-5"); c.FreeAccessExclusion == nil || *c.FreeAccessExclusion != "not-in-catalog" {
		t.Fatalf("gpt-5 row freeAccessExclusion = %s, want \"not-in-catalog\" (priced model)", b525StrOrNil(c.FreeAccessExclusion))
	}

	// Coding channel: every model is priced — all report
	// "not-in-catalog".
	rec = doReq(t, srv, http.MethodGet, "/v1/auto-combo/coding/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET coding candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res = decodeAutoComboCandidates(t, rec.Body.Bytes())
	for _, c := range res.Candidates {
		if c.FreeAccessExclusion == nil || *c.FreeAccessExclusion != "not-in-catalog" {
			t.Fatalf("coding row %+v: freeAccessExclusion = %s, want \"not-in-catalog\"", c, b525StrOrNil(c.FreeAccessExclusion))
		}
	}
}
