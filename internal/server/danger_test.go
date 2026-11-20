package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestDangerRoutesAreTier2: both danger endpoints are classified
// Tier 2 ALWAYS_PROTECTED in the guard table.
func TestDangerRoutesAreTier2(t *testing.T) {
	want := map[string]bool{
		"POST /admin/keys/revoke-all": false,
		"POST /admin/config/reset":    false,
	}
	for _, r := range RouteTable() {
		key := r.Method + " " + r.Path
		if _, ok := want[key]; ok {
			want[key] = true
			if r.Tier != TierAlwaysProtected {
				t.Errorf("%s tier = %s, want %s", key, r.Tier, TierAlwaysProtected)
			}
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("%s missing from the guard route table", key)
		}
	}
}

// dangerTestConfig builds a config with two gateway keys and one
// provider, saved to path, for the danger tests.
func dangerTestConfig(t *testing.T, path string) (*config.Config, string, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	k1, raw1 := mustIssueKey(t)
	k2, raw2 := mustIssueKey(t)
	if err := cfg.AddGatewayKey(k1); err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(k2); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []provider.ProviderConfig{
		{ID: "prov-1", Name: "Mock", BaseURL: "http://127.0.0.1:9", WireFormat: "openai", Enabled: true},
	}
	cfg.BudgetUSD = 5
	cfg.FailClosed = false
	cfg.Routing = &config.RoutingConfig{GlobalStrategy: "cheapest"}
	cfg.Guardrails = &config.GuardrailsConfig{BlockPII: true}
	if err := cfg.SetListenPort(20399); err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = map[string]json.RawMessage{"future_field": json.RawMessage(`"keep-me"`)}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	return cfg, raw1, raw2
}

func dangerPersist(t *testing.T, cfg *config.Config, path string) func() error {
	t.Helper()
	return func() error { return cfg.Save(path) }
}

// TestRevokeAllKeys: a correct typed confirmation + a valid key
// revokes every key for real — the keys stop authenticating, the
// file on disk has none left, and bootstrap becomes pending.
func TestRevokeAllKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, raw1, raw2 := dangerTestConfig(t, path)
	srv := newTestServer(t, &fakeEngine{},
		WithAuthRelaxed(true), // Tier 2 must still demand a key
		WithConfig(cfg),
		WithConfigPersist(dangerPersist(t, cfg, path)),
		WithBootstrapPending(false),
	)

	// No key, even with relaxed Tier 3 auth: 401.
	rec := doReq(t, srv, "POST", "/admin/keys/revoke-all", `{"confirm":"REVOKE ALL"}`, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoke-all without key: status %d, want 401", rec.Code)
	}
	// Wrong confirmation phrase: 400, nothing revoked.
	rec = doReq(t, srv, "POST", "/admin/keys/revoke-all", `{"confirm":"revoke all"}`, raw1)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke-all with wrong phrase: status %d, want 400", rec.Code)
	}
	if len(cfg.GatewayKeys) != 2 {
		t.Fatalf("wrong phrase revoked keys: %d keys left, want 2", len(cfg.GatewayKeys))
	}
	// Missing body: 400.
	rec = doReq(t, srv, "POST", "/admin/keys/revoke-all", "", raw1)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("revoke-all with no body: status %d, want 400", rec.Code)
	}
	// Correct phrase + valid key: 200, both keys gone.
	rec = doReq(t, srv, "POST", "/admin/keys/revoke-all", `{"confirm":"REVOKE ALL"}`, raw1)
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-all: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload revokeAllResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("revoke-all response is not JSON: %v", err)
	}
	if payload.Revoked != 2 || payload.GatewayKeysRemaining != 0 || !payload.BootstrapPending {
		t.Errorf("revoke-all payload = %+v, want revoked=2 remaining=0 bootstrap_pending=true", payload)
	}
	for _, leaked := range []string{raw1, raw2, "key_hash", "sha256"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("revoke-all response leaks %q", leaked)
		}
	}
	// Both keys now fail auth — including the one this call used.
	// (Checked on a Tier 2 route, which never honours relaxed
	// Tier 3 auth, so the 401 proves the key itself is dead.)
	for _, raw := range []string{raw1, raw2} {
		rec = doReq(t, srv, "POST", "/admin/settings/export", "", raw)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("revoked key still authenticates /admin/settings/export: status %d, want 401", rec.Code)
		}
	}
	// The file on disk agrees: no keys left.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.GatewayKeys) != 0 {
		t.Errorf("saved config still has %d gateway key(s), want 0", len(reloaded.GatewayKeys))
	}
	// Bootstrap is pending again (a new first key can be issued).
	rec = doReq(t, srv, "GET", "/dashboard/guard", "", "")
	var guard guardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &guard); err != nil {
		t.Fatalf("guard feed is not JSON: %v", err)
	}
	if !guard.Status.BootstrapPending {
		t.Errorf("bootstrap_pending = false after revoke-all, want true")
	}
}

// TestRevokeAllWithoutPersist answers 503 and revokes nothing when
// no persist function is wired.
func TestRevokeAllWithoutPersist(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw := mustIssueKey(t)
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	rec := doReq(t, srv, "POST", "/admin/keys/revoke-all", `{"confirm":"REVOKE ALL"}`, raw)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("revoke-all without persist: status %d, want 503", rec.Code)
	}
	if len(cfg.GatewayKeys) != 1 {
		t.Errorf("revoke-all without persist changed keys: %d left, want 1", len(cfg.GatewayKeys))
	}
}

// TestResetConfig: a correct typed confirmation resets the config
// to defaults for real — providers and keys gone, budget 0,
// fail-closed back on, routing/guardrails cleared, listen address
// and unknown fields preserved, file on disk matching.
func TestResetConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg, raw1, _ := dangerTestConfig(t, path)
	hookCalls := 0
	srv := newTestServer(t, &fakeEngine{},
		WithAuthRelaxed(true),
		WithConfig(cfg),
		WithConfigPersist(dangerPersist(t, cfg, path)),
		WithDangerResetHook(func() error { hookCalls++; return nil }),
		WithBootstrapPending(false),
	)

	// Wrong phrase: 400, nothing reset.
	rec := doReq(t, srv, "POST", "/admin/config/reset", `{"confirm":"RESET"}`, raw1)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reset with wrong phrase: status %d, want 400", rec.Code)
	}
	if len(cfg.Providers) != 1 || len(cfg.GatewayKeys) != 2 {
		t.Fatalf("wrong phrase reset config: %d providers, %d keys", len(cfg.Providers), len(cfg.GatewayKeys))
	}
	// Correct phrase: 200 with honest counts.
	rec = doReq(t, srv, "POST", "/admin/config/reset", `{"confirm":"RESET CONFIG"}`, raw1)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var payload resetConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("reset response is not JSON: %v", err)
	}
	if payload.ProvidersRemoved != 1 || payload.GatewayKeysRevoked != 2 {
		t.Errorf("reset payload = %+v, want providers_removed=1 gateway_keys_revoked=2", payload)
	}
	if !payload.ProvidersRemovedFromRuntime || hookCalls != 1 {
		t.Errorf("reset runtime hook: removed_from_runtime=%v hookCalls=%d, want true/1", payload.ProvidersRemovedFromRuntime, hookCalls)
	}
	if payload.BudgetUSD != 0 || !payload.FailClosed || payload.Listen != "127.0.0.1:20399" || !payload.BootstrapPending {
		t.Errorf("reset payload = %+v, want budget 0, fail_closed true, listen 127.0.0.1:20399, bootstrap pending", payload)
	}
	if !strings.Contains(payload.Note, "not wiped") {
		t.Errorf("reset note does not say usage history is not wiped: %q", payload.Note)
	}
	// In-memory config is the default shape. Reset restores
	// DefaultConfig, which deliberately carries
	// Routing{DeadlineBudgetMs: 300000} — the RT-1 default
	// (internal/config/config.go:1438). Nil-ing routing on reset
	// would reopen the no-budget hole (one request able to run
	// 600 s), so the honest post-reset shape has routing present
	// with the default budget and providers/keys/guardrails gone.
	if len(cfg.Providers) != 0 || len(cfg.GatewayKeys) != 0 || cfg.BudgetUSD != 0 ||
		!cfg.FailClosed || cfg.Routing == nil || cfg.Routing.DeadlineBudgetMs != config.DefaultDeadlineBudgetMs || cfg.Guardrails != nil {
		t.Errorf("in-memory config after reset: %+v", cfg)
	}
	if cfg.Listen != "127.0.0.1:20399" {
		t.Errorf("reset changed listen to %q, want it preserved", cfg.Listen)
	}
	if cfg.Extensions["future_field"] == nil || string(cfg.Extensions["future_field"]) != `"keep-me"` {
		t.Errorf("reset dropped unknown fields: %v", cfg.Extensions)
	}
	// The file on disk agrees.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Providers) != 0 || len(reloaded.GatewayKeys) != 0 || reloaded.BudgetUSD != 0 {
		t.Errorf("saved config after reset: %d providers, %d keys, budget %v", len(reloaded.Providers), len(reloaded.GatewayKeys), reloaded.BudgetUSD)
	}
	// The saved file carries the same default routing budget (see
	// the in-memory assertion above): reset must not persist a
	// budget-less config.
	if reloaded.Routing == nil || reloaded.Routing.DeadlineBudgetMs != config.DefaultDeadlineBudgetMs {
		t.Errorf("saved config after reset: routing = %+v, want DeadlineBudgetMs %d", reloaded.Routing, config.DefaultDeadlineBudgetMs)
	}
	// The old key no longer authenticates (Tier 2 route, which
	// never honours relaxed Tier 3 auth, so the 401 proves the key
	// itself is dead).
	rec = doReq(t, srv, "POST", "/admin/settings/export", "", raw1)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key still authenticates /admin/settings/export: status %d, want 401", rec.Code)
	}
}

// TestResetConfigRollbackOnPersistFailure: a failing save leaves
// the config untouched and answers 500.
func TestResetConfigRollbackOnPersistFailure(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw := mustIssueKey(t)
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []provider.ProviderConfig{
		{ID: "prov-1", Name: "Mock", BaseURL: "http://127.0.0.1:9", Enabled: true},
	}
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithConfigPersist(func() error { return errTestPersist }),
		WithBootstrapPending(false),
	)
	rec := doReq(t, srv, "POST", "/admin/config/reset", `{"confirm":"RESET CONFIG"}`, raw)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("reset with failing persist: status %d, want 500", rec.Code)
	}
	if len(cfg.Providers) != 1 || len(cfg.GatewayKeys) != 1 {
		t.Errorf("failed reset mutated config: %d providers, %d keys", len(cfg.Providers), len(cfg.GatewayKeys))
	}
}

// TestResetConfigHookFailure: when the registry hook fails, the
// reset still succeeds (the file is saved) but the response says
// honestly that providers stay loaded until restart.
func TestResetConfigHookFailure(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	key, raw := mustIssueKey(t)
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithConfigPersist(dangerPersist(t, cfg, path)),
		WithDangerResetHook(func() error { return errTestPersist }),
	)
	rec := doReq(t, srv, "POST", "/admin/config/reset", `{"confirm":"RESET CONFIG"}`, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset with failing hook: status %d, want 200", rec.Code)
	}
	var payload resetConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("reset response is not JSON: %v", err)
	}
	if payload.ProvidersRemovedFromRuntime {
		t.Errorf("reset with failing hook claims runtime removal, want false")
	}
	if !strings.Contains(payload.Note, "restart") {
		t.Errorf("reset note does not mention restart: %q", payload.Note)
	}
}

// errTestPersist is the sentinel persist/hook failure for the
// rollback tests.
var errTestPersist = errors.New("test persist failure")
