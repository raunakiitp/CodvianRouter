// P1.9 — scheduled evaluation dashboard: the trends endpoint and the
// failed-case detail carried by run history.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/eval"
)

func p19ServeTrends(t *testing.T, srv *Server, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/eval/schedules/"+id+"/trends"+query, nil)
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	srv.handleEvalScheduleTrends(rec, req)
	return rec
}

func TestP19TrendsEndpointReportsSeriesAndDirection(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	st, err := srv.ensureEvalStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(&eval.Schedule{ID: "nightly", Every: time.Hour, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, rate := range []float64{0.8, 0.9, 0.7} {
		if _, err := st.AddRun(&eval.Run{
			ScheduleID: "nightly",
			StartedAt:  base.Add(time.Duration(i) * time.Minute),
			Status:     "completed",
			PassRate:   rate,
			Total:      10,
			Passed:     int(rate * 10),
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := p19ServeTrends(t, srv, "nightly", "?limit=10")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var tr eval.Trend
	if err := json.NewDecoder(rec.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	if tr.Direction != "down" {
		t.Fatalf("direction = %q, want down", tr.Direction)
	}
	if tr.CompletedRuns != 3 || len(tr.Points) != 3 {
		t.Fatalf("completed=%d points=%d, want 3/3", tr.CompletedRuns, len(tr.Points))
	}
	if tr.LatestPassRate != 0.7 {
		t.Fatalf("latest = %v, want 0.7", tr.LatestPassRate)
	}
	// Points are chronological: oldest first.
	if tr.Points[0].PassRate != 0.8 || tr.Points[2].PassRate != 0.7 {
		t.Fatalf("points not chronological: %v", tr.Points)
	}
}

func TestP19TrendsEndpointUnknownScheduleIs404(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if rec := p19ServeTrends(t, srv, "ghost", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestP19HistoryCarriesFailureDetails(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	st, err := srv.ensureEvalStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRun(&eval.Run{
		ScheduleID:  "nightly",
		StartedAt:   time.Now(),
		Status:      "failed",
		FailedCases: []string{"regression/observability"},
		Failures: []eval.CaseFailure{{
			CaseID:  "regression/observability",
			Reason:  "missing expected substrings: request_id",
			Missing: []string{"request_id"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/eval/schedules/nightly/history", nil)
	req.SetPathValue("id", "nightly")
	rec := httptest.NewRecorder()
	srv.handleEvalScheduleHistory(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var runs []evalRunView
	if err := json.NewDecoder(rec.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || len(runs[0].Failures) != 1 {
		t.Fatalf("runs = %d failures = %v, want one run with one failure", len(runs), runs)
	}
	f := runs[0].Failures[0]
	if f.CaseID != "regression/observability" || f.Reason == "" || len(f.Missing) != 1 {
		t.Fatalf("failure detail = %+v, want id + reason + missing", f)
	}
}
