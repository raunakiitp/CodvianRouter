// M9.9 A2A operator status tests (internal/server/a2a_status.go):
// real HTTP via httptest against a real a2a.Store — no mocks
// standing in for the wire or the counts.
package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// a2aStatusSeedTask creates one task for owner and moves it to the
// target state through the legal transitions, so the seeded state
// is reached the same way production tasks reach it.
func a2aStatusSeedTask(t *testing.T, s *a2a.Store, owner string, target a2a.State) {
	t.Helper()
	task, err := s.Create(owner, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	switch target {
	case a2a.StateSubmitted:
		// Already submitted.
	case a2a.StateWorking:
		if _, err := s.Transition(owner, task.ID, a2a.StateWorking, nil, ""); err != nil {
			t.Fatalf("Transition to working: %v", err)
		}
	case a2a.StateCompleted:
		if _, err := s.Transition(owner, task.ID, a2a.StateWorking, nil, ""); err != nil {
			t.Fatalf("Transition to working: %v", err)
		}
		if _, err := s.Transition(owner, task.ID, a2a.StateCompleted, json.RawMessage(`{"ok":true}`), ""); err != nil {
			t.Fatalf("Transition to completed: %v", err)
		}
	case a2a.StateFailed:
		if _, err := s.Transition(owner, task.ID, a2a.StateWorking, nil, ""); err != nil {
			t.Fatalf("Transition to working: %v", err)
		}
		if _, err := s.Transition(owner, task.ID, a2a.StateFailed, nil, "boom"); err != nil {
			t.Fatalf("Transition to failed: %v", err)
		}
	case a2a.StateCancelled:
		if _, err := s.Transition(owner, task.ID, a2a.StateCancelled, nil, ""); err != nil {
			t.Fatalf("Transition to cancelled: %v", err)
		}
	default:
		t.Fatalf("unknown target state %q", target)
	}
}

// a2aStatusCall issues one request to the status handler and
// decodes the JSON body into a raw map (field-name assertions read
// the wire shape, not a Go struct's tags).
func a2aStatusCall(t *testing.T, h http.HandlerFunc, method string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/api/a2a/status", nil)
	h.ServeHTTP(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s /api/a2a/status response is not JSON (status %d): %v: %s",
			method, rec.Code, err, rec.Body.String())
	}
	return rec.Code, out, rec.Body.String()
}

func TestA2AStatusDisabled(t *testing.T) {
	called := false
	h := A2AStatusHandler(false, func() (*a2a.Store, error) {
		called = true
		return nil, errors.New("open must not be called while disabled")
	})
	code, out, _ := a2aStatusCall(t, h, http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("disabled GET status = %d, want 200", code)
	}
	if called {
		t.Fatalf("disabled handler called open(): no store must be touched while A2A is off")
	}
	if out["enabled"] != false {
		t.Errorf("enabled = %v, want false", out["enabled"])
	}
	wantCounts := map[string]any{
		"submitted": float64(0), "working": float64(0), "completed": float64(0),
		"failed": float64(0), "cancelled": float64(0),
	}
	if !reflect.DeepEqual(out["tasks_by_state"], wantCounts) {
		t.Errorf("tasks_by_state = %v, want all-zero %v", out["tasks_by_state"], wantCounts)
	}
	wantSkills := []any{"smart-routing", "quota-management", "provider-discovery",
		"cost-analysis", "health-report", "list-capabilities",
		"budget-forecasting", "cost-estimation", "route-preview",
		"compression-analytics", "eval-management", "org-reporting"}
	if !reflect.DeepEqual(out["skills"], wantSkills) {
		t.Errorf("skills = %v, want %v", out["skills"], wantSkills)
	}
	if out["protocol_version"] != "2025-03-26" {
		t.Errorf("protocol_version = %v, want 2025-03-26", out["protocol_version"])
	}
}

func TestA2AStatusEnabledSeededCounts(t *testing.T) {
	s, err := a2a.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open test store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ownerA := a2a.OwnerIDForKey("status-key-one")
	ownerB := a2a.OwnerIDForKey("status-key-two")
	// Owner A: 2 submitted, 1 working, 1 completed, 1 failed, 1 cancelled.
	a2aStatusSeedTask(t, s, ownerA, a2a.StateSubmitted)
	a2aStatusSeedTask(t, s, ownerA, a2a.StateSubmitted)
	a2aStatusSeedTask(t, s, ownerA, a2a.StateWorking)
	a2aStatusSeedTask(t, s, ownerA, a2a.StateCompleted)
	a2aStatusSeedTask(t, s, ownerA, a2a.StateFailed)
	a2aStatusSeedTask(t, s, ownerA, a2a.StateCancelled)
	// Owner B: 1 submitted, 1 completed.
	a2aStatusSeedTask(t, s, ownerB, a2a.StateSubmitted)
	a2aStatusSeedTask(t, s, ownerB, a2a.StateCompleted)

	h := A2AStatusHandler(true, func() (*a2a.Store, error) { return s, nil })
	code, out, _ := a2aStatusCall(t, h, http.MethodGet)
	if code != http.StatusOK {
		t.Fatalf("enabled GET status = %d, want 200", code)
	}
	if out["enabled"] != true {
		t.Errorf("enabled = %v, want true", out["enabled"])
	}
	// Totals across BOTH owners: submitted 3, working 1,
	// completed 2, failed 1, cancelled 1. If the handler counted
	// only one owner (or returned zeros), this fails.
	wantCounts := map[string]any{
		"submitted": float64(3), "working": float64(1), "completed": float64(2),
		"failed": float64(1), "cancelled": float64(1),
	}
	if !reflect.DeepEqual(out["tasks_by_state"], wantCounts) {
		t.Errorf("tasks_by_state = %v, want %v", out["tasks_by_state"], wantCounts)
	}
	if out["protocol_version"] != "2025-03-26" {
		t.Errorf("protocol_version = %v, want 2025-03-26", out["protocol_version"])
	}
	// No owner or task identifier may leak into the response.
	body, _ := json.Marshal(out)
	for _, leaked := range []string{ownerA, ownerB, "status-key-one", "status-key-two"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("response leaks identifier %q: %s", leaked, body)
		}
	}
}

func TestA2AStatusOpenErrorIs503NotFakeZeros(t *testing.T) {
	h := A2AStatusHandler(true, func() (*a2a.Store, error) {
		return nil, errors.New("disk gone")
	})
	code, _, body := a2aStatusCall(t, h, http.MethodGet)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("open-error GET status = %d, want 503", code)
	}
	if !strings.Contains(body, "store unavailable") {
		t.Errorf("503 body %q does not say the store is unavailable", body)
	}
	if strings.Contains(body, "tasks_by_state") {
		t.Errorf("503 body must not carry (fake) task counts: %s", body)
	}
}

func TestA2AStatusCountsErrorIs503NotFakeZeros(t *testing.T) {
	// Drive the CountsByState() failure branch (a2a_status.go
	// counts query), distinct from the open() failure covered by
	// TestA2AStatusOpenErrorIs503NotFakeZeros: open() succeeds
	// and returns a store whose underlying DB is already closed,
	// so the count query fails with "sql: database is closed".
	s, err := a2a.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open test store: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close test store: %v", err)
	}
	h := A2AStatusHandler(true, func() (*a2a.Store, error) { return s, nil })
	code, _, body := a2aStatusCall(t, h, http.MethodGet)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("counts-error GET status = %d, want 503", code)
	}
	if !strings.Contains(body, "store unavailable") {
		t.Errorf("503 body %q does not say the store is unavailable", body)
	}
	if strings.Contains(body, "tasks_by_state") {
		t.Errorf("503 body must not carry (fake) task counts: %s", body)
	}
}

func TestA2AStatusMethodNotAllowed(t *testing.T) {
	h := A2AStatusHandler(false, func() (*a2a.Store, error) {
		t.Fatal("non-GET request must not reach open()")
		return nil, nil
	})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/api/a2a/status", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
}

func TestA2AStatusExactShape(t *testing.T) {
	h := A2AStatusHandler(false, func() (*a2a.Store, error) {
		t.Fatal("disabled handler must not call open()")
		return nil, nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/a2a/status", nil)
	h.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("response is not JSON: %v: %s", err, rec.Body.String())
	}
	// Exact top-level field names — the dashboard codes against them.
	wantTop := map[string]bool{
		"enabled": true, "tasks_by_state": true, "skills": true, "protocol_version": true,
	}
	if len(raw) != len(wantTop) {
		t.Fatalf("top-level fields = %v, want exactly %v", a2aStatusKeysOf(raw), wantTop)
	}
	for k := range raw {
		if !wantTop[k] {
			t.Errorf("unexpected top-level field %q", k)
		}
	}
	var counts map[string]json.RawMessage
	if err := json.Unmarshal(raw["tasks_by_state"], &counts); err != nil {
		t.Fatalf("tasks_by_state is not an object: %v", err)
	}
	wantStates := map[string]bool{
		"submitted": true, "working": true, "completed": true, "failed": true, "cancelled": true,
	}
	if len(counts) != len(wantStates) {
		t.Fatalf("tasks_by_state fields = %v, want exactly %v", a2aStatusKeysOf(counts), wantStates)
	}
	for k := range counts {
		if !wantStates[k] {
			t.Errorf("unexpected tasks_by_state field %q", k)
		}
	}
	// Typed decode as well: the struct tags must match the wire shape.
	var decoded a2aStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("response does not decode into a2aStatusResponse: %v", err)
	}
	if decoded.ProtocolVersion != "2025-03-26" || len(decoded.Skills) != 12 || len(decoded.TasksByState) != 5 {
		t.Errorf("decoded shape mismatch: %+v", decoded)
	}
}

func a2aStatusKeysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
