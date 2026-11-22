// P3-B: two Server instances with different data dirs must NOT share
// eval state (regression for the package-global store bug). Each
// Server lazily opens its own store under its own cfg.DataDir.
package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestP3BEvalStateIsolatedPerServer(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()
	cfgA := config.DefaultConfig(dirA)
	cfgB := config.DefaultConfig(dirB)

	eng := &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}}}
	ledA, _ := ledger.Open(filepath.Join(t.TempDir(), "a.db"))
	ledB, _ := ledger.Open(filepath.Join(t.TempDir(), "b.db"))
	t.Cleanup(func() { _ = ledA.Close(); _ = ledB.Close() })

	srvA := newTestServer(t, eng, WithConfig(cfgA), WithLedger(ledA))
	srvB := newTestServer(t, eng, WithConfig(cfgB), WithLedger(ledB))

	rec := callEvalScheduleHandler(t, srvA, "POST", "/api/eval/schedules", `{"id":"onlyA","every":"24h"}`)
	if rec.Code != 201 {
		t.Fatalf("create on A status = %d: %s", rec.Code, rec.Body.String())
	}
	rec = callEvalScheduleHandler(t, srvB, "GET", "/api/eval/schedules", "")
	if rec.Code != 200 {
		t.Fatalf("list on B status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"onlyA"`) {
		t.Fatalf("B leaked A's schedule: %s", rec.Body.String())
	}
	// A2 on dirA recovers A's persisted schedule (restart recovery).
	srvA2 := newTestServer(t, eng, WithConfig(cfgA), WithLedger(ledA))
	rec = callEvalScheduleHandler(t, srvA2, "GET", "/api/eval/schedules", "")
	if !strings.Contains(rec.Body.String(), `"onlyA"`) {
		t.Fatalf("A2 did not recover A's schedule: %s", rec.Body.String())
	}
}
