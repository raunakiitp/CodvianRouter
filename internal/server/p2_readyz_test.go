package server

// AUD-CFG-004: /readyz is READINESS, distinct from /healthz liveness.
// The endpoint must return 200 only when the critical dependencies —
// the engine and, when wired, the ledger — are actually usable, and
// 503 naming the failed check otherwise. The Docker healthcheck
// binary GETs /readyz, so these checks gate container traffic.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
)

func readyzRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/readyz", nil)
}

// TestReadyzReadyWithHealthyLedger: a fully wired server — engine
// plus an open ledger — answers 200 {"status":"ready"}.
func TestReadyzReadyWithHealthyLedger(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, readyzRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("GET /readyz status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("status = %q, want \"ready\"", body["status"])
	}
}

// TestReadyzReadyWithoutLedger: no ledger wired is a legitimate
// fresh-install state (the feeds answer honestly with empty data),
// so the endpoint must NOT mark the server unready for it.
func TestReadyzReadyWithoutLedger(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, readyzRequest())
	if w.Code != http.StatusOK {
		t.Fatalf("GET /readyz without ledger status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ready"`) {
		t.Fatalf("body = %s, want status ready", w.Body.String())
	}
}

// TestReadyzLedgerUnavailable503: a CLOSED ledger (its DB
// connection dead) must fail the readiness check with 503 and the
// failing check named — this is the "dependencies ready" promise the
// healthcheck binary now relies on.
func TestReadyzLedgerUnavailable503(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	// Close the ledger BEFORE wiring cleanup runs (newTestServer
	// only owns the server; the ledger is ours).
	if err := led.Close(); err != nil {
		t.Fatalf("ledger.Close: %v", err)
	}

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, readyzRequest())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz with closed ledger status = %d, want 503: %s", w.Code, w.Body.String())
	}
	var body struct {
		Status  string   `json:"status"`
		Reasons []string `json:"reasons"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if body.Status != "unready" {
		t.Fatalf("status = %q, want \"unready\"", body.Status)
	}
	found := false
	for _, r := range body.Reasons {
		if strings.Contains(r, "ledger unavailable") {
			found = true
		}
	}
	if !found {
		t.Fatalf("reasons = %v, want a ledger-unavailable entry", body.Reasons)
	}
}

// TestReadyzRecoversAfterDependencyRestoration: reopening the
// ledger (wiring a fresh, healthy one) flips the endpoint back to
// 200 — readiness reflects the CURRENT dependency state, not a
// sticky failure.
func TestReadyzRecoversAfterDependencyRestoration(t *testing.T) {
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))
	_ = led.Close()

	w := httptest.NewRecorder()
	srv.ServeHTTP(w, readyzRequest())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("pre-recovery status = %d, want 503", w.Code)
	}

	// Restore: wire a healthy ledger.
	led2, err := ledger.Open(filepath.Join(t.TempDir(), "ledger2.db"))
	if err != nil {
		t.Fatalf("ledger.Open(2): %v", err)
	}
	t.Cleanup(func() { _ = led2.Close() })
	srv.ledger = led2

	w2 := httptest.NewRecorder()
	srv.ServeHTTP(w2, readyzRequest())
	if w2.Code != http.StatusOK {
		t.Fatalf("post-recovery status = %d, want 200: %s", w2.Code, w2.Body.String())
	}
}

// TestReadyzBoundedByTimeout: the ledger ping runs under a bounded
// context (readyzTimeout) — an endpoint that hangs the probe would
// hang the container healthcheck. The bound is asserted indirectly:
// the handler completes quickly against a healthy ledger and the
// timeout constant is the documented bound.
func TestReadyzBoundedByTimeout(t *testing.T) {
	if readyzTimeout <= 0 || readyzTimeout > 5*time.Second {
		t.Fatalf("readyzTimeout = %v, want a small positive bound (<= 5s)", readyzTimeout)
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	srv := newTestServer(t, &fakeEngine{}, WithLedger(led))

	start := time.Now()
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, readyzRequest())
	if elapsed := time.Since(start); elapsed > readyzTimeout+time.Second {
		t.Fatalf("readyz took %v, want within the %v bound", elapsed, readyzTimeout)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}
