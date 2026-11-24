// AUD-CODE-001 regression tests: a security-sensitive key state change
// that could not be persisted must NOT report success.
//
// Before the fix, persistConfig swallowed its error, so disable/enable/
// revoke answered 200 while nothing was written: the in-memory state and
// the file diverged and the change vanished on restart — the operator
// believed a key was revoked while it came back alive.
package server

import (
	"errors"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

var errP13Persist = errors.New("disk full")

// p13KeyStateServer builds a server whose config persistence fails.
func p13KeyStateServer(t *testing.T) (*Server, *config.Config, string, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	_, rawAdmin := issueScopedKey(t, cfg, "p13-admin", []string{"admin", "chat"})
	worker, _ := issueScopedKey(t, cfg, "p13-worker", []string{"chat"})

	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithConfigPersist(func() error { return errP13Persist }),
	)
	return srv, cfg, rawAdmin, worker.ID
}

func p13WorkerDisabled(cfg *config.Config, id string) bool {
	for _, k := range cfg.GatewayKeys {
		if k.ID == id {
			return k.Disabled
		}
	}
	return false
}

func p13KeyPresent(cfg *config.Config, id string) bool {
	for _, k := range cfg.GatewayKeys {
		if k.ID == id {
			return true
		}
	}
	return false
}

func TestAUD_CODE_001DisableReportsFailureAndRollsBack(t *testing.T) {
	srv, cfg, rawAdmin, workerID := p13KeyStateServer(t)

	rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+workerID+"/disable", "", rawAdmin)
	if rec.Code == http.StatusOK {
		t.Fatalf("disable answered 200 although the config write failed: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("disable status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if p13WorkerDisabled(cfg, workerID) {
		t.Error("in-memory state says disabled after a failed write (state diverged from the file)")
	}
}

func TestAUD_CODE_001EnableReportsFailureAndRollsBack(t *testing.T) {
	srv, cfg, rawAdmin, workerID := p13KeyStateServer(t)
	// Start from disabled so enabling is a real state change.
	for i := range cfg.GatewayKeys {
		if cfg.GatewayKeys[i].ID == workerID {
			cfg.GatewayKeys[i].Disabled = true
		}
	}
	rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+workerID+"/enable", "", rawAdmin)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("enable status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if !p13WorkerDisabled(cfg, workerID) {
		t.Error("in-memory state says enabled after a failed write")
	}
}

func TestAUD_CODE_001RevokeReportsFailureAndKeepsKey(t *testing.T) {
	srv, cfg, rawAdmin, workerID := p13KeyStateServer(t)

	rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+workerID+"/revoke", "", rawAdmin)
	if rec.Code == http.StatusOK {
		t.Fatalf("revoke answered 200 although the config write failed: %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("revoke status = %d, want 500 (%s)", rec.Code, rec.Body.String())
	}
	if !p13KeyPresent(cfg, workerID) {
		t.Fatal("key was removed from memory after a failed write: it would come back on restart while the operator believes it revoked")
	}
	// The rollback re-inserts at the original position, so key order is
	// preserved (a revocation is not a reordering).
	if len(cfg.GatewayKeys) != 2 {
		t.Fatalf("key count after rollback = %d, want 2", len(cfg.GatewayKeys))
	}
	if cfg.GatewayKeys[1].ID != workerID {
		t.Errorf("rolled-back key sits at index %d, want its original index 1 (order %v)",
			1, []string{cfg.GatewayKeys[0].ID, cfg.GatewayKeys[1].ID})
	}
}

func TestAUD_CODE_001SuccessPathStillPersists(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	_, rawAdmin := issueScopedKey(t, cfg, "p13-admin2", []string{"admin", "chat"})
	worker, _ := issueScopedKey(t, cfg, "p13-worker2", []string{"chat"})
	persists := 0
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		WithConfigPersist(func() error { persists++; return nil }),
	)
	rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+worker.ID+"/disable", "", rawAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable with working persistence: status %d, body %s", rec.Code, rec.Body.String())
	}
	if persists != 1 {
		t.Errorf("configPersist calls = %d, want 1", persists)
	}
	if !p13WorkerDisabled(cfg, worker.ID) {
		t.Error("key not disabled in memory on the success path")
	}
}

// TestAUD_CODE_001NilHookIsNotAnError: no persistence wired (tests,
// embedded use) keeps the old behaviour of a successful state change.
func TestAUD_CODE_001NilHookIsNotAnError(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	_, rawAdmin := issueScopedKey(t, cfg, "p13-admin3", []string{"admin", "chat"})
	worker, _ := issueScopedKey(t, cfg, "p13-worker3", []string{"chat"})
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	rec := doLocalReq(t, srv, http.MethodPost, "/admin/keys/"+worker.ID+"/disable", "", rawAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable without a persistence hook: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}
