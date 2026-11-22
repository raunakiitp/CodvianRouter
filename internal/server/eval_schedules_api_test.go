// P3-B: scheduled-evaluation REST API + guard classification.
//
// Drives the real handlers (direct call, the sibling webhooks pattern)
// against a server with a persisted config data dir, plus verifies the
// guard tier table classifies every eval route. The scheduler loop
// itself is covered by internal/eval/schedule_test.go; this proves the
// server layer exposes and persists the store.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func callEvalScheduleHandler(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	// The mux sets {id} path values in production; direct-handler
	// calls must set them explicitly for {id} routes.
	if i := strings.Index(path, "/api/eval/schedules/"); i >= 0 {
		rest := path[len("/api/eval/schedules/"):]
		if !strings.HasSuffix(rest, "/history") {
			id := rest
			if j := strings.IndexByte(id, '/'); j >= 0 {
				id = id[:j]
			}
			if id != "" && id != "schedules" {
				req.SetPathValue("id", id)
			}
		}
	}
	rec := httptest.NewRecorder()
	switch {
	case method == http.MethodPost && path == "/api/eval/schedules":
		srv.handleEvalScheduleCreate(rec, req)
	case method == http.MethodGet && path == "/api/eval/schedules":
		srv.handleEvalSchedules(rec, req)
	case strings.HasSuffix(path, "/history"):
		srv.handleEvalScheduleHistory(rec, req)
	case method == http.MethodDelete:
		srv.handleEvalScheduleDelete(rec, req)
	default:
		srv.handleEvalScheduleByID(rec, req)
	}
	return rec
}

func decodeEval(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rec.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rec.Body.String())
	}
}

func TestP3BScheduledEvalAPICRUD(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)

	// Create.
	rec := callEvalScheduleHandler(t, srv, "POST", "/api/eval/schedules",
		`{"id":"nightly","suite":"permanent","every":"24h","enabled":true,"regression_max_pass_rate_drop":0.2,"min_runs_for_baseline":3}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created evalScheduleView
	decodeEval(t, rec, &created)
	if created.ID != "nightly" || created.Every != "24h0m0s" || created.Enabled != true {
		t.Fatalf("created = %+v, want nightly/24h/enabled", created)
	}

	// List.
	rec = callEvalScheduleHandler(t, srv, "GET", "/api/eval/schedules", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var list []evalScheduleView
	decodeEval(t, rec, &list)
	if len(list) != 1 || list[0].ID != "nightly" {
		t.Fatalf("list = %+v, want exactly nightly", list)
	}

	// Pause (PATCH enabled:false).
	rec = callEvalScheduleHandler(t, srv, "PATCH", "/api/eval/schedules/nightly", `{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d: %s", rec.Code, rec.Body.String())
	}
	var patched evalScheduleView
	decodeEval(t, rec, &patched)
	if patched.Enabled {
		t.Fatal("patch did not disable the schedule")
	}

	// History is empty before any run.
	rec = callEvalScheduleHandler(t, srv, "GET", "/api/eval/schedules/nightly/history", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d", rec.Code)
	}
	var runs []evalRunView
	decodeEval(t, rec, &runs)
	if len(runs) != 0 {
		t.Fatalf("history = %+v, want empty", runs)
	}

	// Delete then 404.
	rec = callEvalScheduleHandler(t, srv, "DELETE", "/api/eval/schedules/nightly", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", rec.Code)
	}
	rec = callEvalScheduleHandler(t, srv, "GET", "/api/eval/schedules/nightly", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get-after-delete status = %d, want 404", rec.Code)
	}

	// Validation: bad interval refused.
	rec = callEvalScheduleHandler(t, srv, "POST", "/api/eval/schedules", `{"id":"bad","every":"-1s"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad-interval status = %d, want 400", rec.Code)
	}
}

func TestP3BEvalGuardClassification(t *testing.T) {
	want := map[string]GuardTier{
		"GET /api/eval/schedules":            TierManagement,
		"POST /api/eval/schedules":           TierAlwaysProtected,
		"GET /api/eval/schedules/{id}":       TierManagement,
		"PATCH /api/eval/schedules/{id}":     TierAlwaysProtected,
		"DELETE /api/eval/schedules/{id}":    TierAlwaysProtected,
		"GET /api/eval/schedules/{id}/history": TierManagement,
	}
	for _, r := range RouteTable() {
		key := r.Method + " " + r.Path
		if tier, ok := want[key]; ok {
			if r.Tier != tier {
				t.Errorf("%s classified %s, want %s", key, r.Tier, tier)
			}
			delete(want, key)
		}
	}
	if len(want) != 0 {
		t.Errorf("eval routes missing from guard table: %v", want)
	}
}
