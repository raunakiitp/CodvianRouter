// P1.9 — scheduled evaluation dashboard: pass-rate trends and
// failed-case details.
//
// The P3-B scheduler already persisted runs and flagged regressions, but
// a run carried only the failing case IDs (no reason) and there was no
// trend view, so an operator could neither triage a failure from history
// nor see whether the pass rate was drifting. These tests pin both.
package eval

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// p19ErrEngine always fails, so a case that expects substrings records a
// missing-substring failure.
type p19ErrEngine struct{}

func (p19ErrEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("upstream exploded")
}

// seedRuns appends completed runs with explicit pass rates at one-minute
// intervals, oldest first, and returns their IDs.
func seedRuns(t *testing.T, st *Store, scheduleID string, rates ...float64) []string {
	t.Helper()
	base := time.Now().Add(-time.Hour)
	ids := make([]string, 0, len(rates))
	for i, rate := range rates {
		run := &Run{
			ScheduleID: scheduleID,
			StartedAt:  base.Add(time.Duration(i) * time.Minute),
			FinishedAt: base.Add(time.Duration(i)*time.Minute + 5*time.Second),
			Status:     "completed",
			PassRate:   rate,
			Total:      10,
			Passed:     int(rate * 10),
		}
		id, err := st.AddRun(run)
		if err != nil {
			t.Fatalf("AddRun: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestP19TrendIsUnknownWithFewerThanTwoCompletedRuns: one number is a
// measurement, not a trend.
func TestP19TrendIsUnknownWithFewerThanTwoCompletedRuns(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	seedRuns(t, st, "s1", 0.5)
	tr := st.PassRateTrend("s1", 20)
	if tr.Direction != "unknown" {
		t.Fatalf("direction = %q, want unknown with one completed run", tr.Direction)
	}
	if tr.CompletedRuns != 1 {
		t.Fatalf("completed runs = %d, want 1", tr.CompletedRuns)
	}
	if tr.LatestPassRate != 0.5 {
		t.Fatalf("latest pass rate = %v, want 0.5", tr.LatestPassRate)
	}
	if len(tr.Points) != 1 {
		t.Fatalf("points = %d, want 1", len(tr.Points))
	}
}

// TestP19TrendDirectionsAndBaseline: up/down/flat, baseline = mean of the
// completed runs BEFORE the latest, points chronological.
func TestP19TrendDirectionsAndBaseline(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	// 0.8, 0.9, 0.7 → latest 0.7, baseline mean(0.8,0.9)=0.85 → down.
	seedRuns(t, st, "s1", 0.8, 0.9, 0.7)
	tr := st.PassRateTrend("s1", 20)
	if tr.Direction != "down" {
		t.Fatalf("direction = %q, want down", tr.Direction)
	}
	if tr.LatestPassRate != 0.7 {
		t.Fatalf("latest = %v, want 0.7", tr.LatestPassRate)
	}
	if diff := tr.BaselinePassRate - 0.85; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("baseline = %v, want 0.85", tr.BaselinePassRate)
	}
	if len(tr.Points) != 3 {
		t.Fatalf("points = %d, want 3", len(tr.Points))
	}
	// Chronological: the first point is the oldest (0.8).
	if tr.Points[0].PassRate != 0.8 || tr.Points[2].PassRate != 0.7 {
		t.Fatalf("points not chronological: %v", tr.Points)
	}

	// Flat: 0.5, 0.5, 0.5.
	st2, _ := Open("")
	seedRuns(t, st2, "s2", 0.5, 0.5, 0.5)
	if got := st2.PassRateTrend("s2", 20).Direction; got != "flat" {
		t.Fatalf("direction = %q, want flat", got)
	}
	// Up: 0.4, 0.5, 0.9.
	st3, _ := Open("")
	seedRuns(t, st3, "s3", 0.4, 0.5, 0.9)
	if got := st3.PassRateTrend("s3", 20).Direction; got != "up" {
		t.Fatalf("direction = %q, want up", got)
	}
}

// TestP19TrendNonCompletedRunsNeverCountAsZero: a failed run is visible
// as a point but does not enter the baseline mean or the latest figure.
func TestP19TrendNonCompletedRunsNeverCountAsZero(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	seedRuns(t, st, "s1", 0.9, 0.9)
	// A failed run after the completed ones: Total 0, PassRate 0.
	if _, err := st.AddRun(&Run{
		ScheduleID: "s1",
		StartedAt:  time.Now(),
		Status:     "failed",
		Error:      "eval: no cases in suite",
	}); err != nil {
		t.Fatal(err)
	}
	tr := st.PassRateTrend("s1", 20)
	if tr.CompletedRuns != 2 {
		t.Fatalf("completed runs = %d, want 2 (the failed run must not count)", tr.CompletedRuns)
	}
	if tr.LatestPassRate != 0.9 {
		t.Fatalf("latest = %v, want 0.9 from the last completed run, not 0 from the failed one", tr.LatestPassRate)
	}
	if len(tr.Points) != 3 {
		t.Fatalf("points = %d, want 3 (the failed run is still visible)", len(tr.Points))
	}
	if tr.Points[2].Status != "failed" {
		t.Fatalf("last point status = %q, want failed", tr.Points[2].Status)
	}
}

// TestP19RunRecordsFailureDetails: a failing case is persisted with its
// reason and missing substrings, not just its ID.
func TestP19RunRecordsFailureDetails(t *testing.T) {
	st, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	sch := &Schedule{ID: "s1", Every: time.Minute, Enabled: true}
	if err := st.Upsert(sch); err != nil {
		t.Fatal(err)
	}
	sched := &Scheduler{Store: st, NewEngine: func() Engine { return p19ErrEngine{} }}
	// tick() launches execute() in a goroutine; call it directly so the
	// assertion does not race the scheduler.
	sched.execute(context.Background(), sch)

	runs := st.History("s1", 10)
	if len(runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(runs))
	}
	run := runs[0]
	if run.Status != "failed" {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if len(run.Failures) == 0 {
		t.Fatalf("no failure details recorded")
	}
	for _, f := range run.Failures {
		if f.CaseID == "" {
			t.Fatalf("failure without a case id")
		}
		if f.Reason == "" && len(f.Missing) == 0 {
			t.Fatalf("failure %q has neither a reason nor missing substrings", f.CaseID)
		}
		if !strings.Contains(f.Reason, "upstream exploded") && len(f.Missing) == 0 {
			t.Fatalf("failure %q reason = %q, want the engine error", f.CaseID, f.Reason)
		}
	}
	// FailedCases must still be present for compatibility.
	if len(run.FailedCases) != len(run.Failures) {
		t.Fatalf("failed_cases (%d) and failures (%d) disagree", len(run.FailedCases), len(run.Failures))
	}
}

// TestP19FailureDetailsSurviveRestart: the detail is persisted and
// reloaded, so history stays triageable after a restart.
func TestP19FailureDetailsSurviveRestart(t *testing.T) {
	path := t.TempDir() + "/eval-store.json"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddRun(&Run{
		ScheduleID:  "s1",
		StartedAt:   time.Now(),
		Status:      "failed",
		FailedCases: []string{"regression/observability"},
		Failures: []CaseFailure{{
			CaseID:  "regression/observability",
			Reason:  "missing expected substrings: request_id",
			Missing: []string{"request_id"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runs := reopened.History("s1", 10)
	if len(runs) != 1 || len(runs[0].Failures) != 1 {
		t.Fatalf("reloaded runs = %d failures = %v, want one run with one failure", len(runs), runs)
	}
	if runs[0].Failures[0].CaseID != "regression/observability" {
		t.Fatalf("reloaded failure = %+v", runs[0].Failures[0])
	}
	if runs[0].Failures[0].Reason == "" {
		t.Fatalf("reloaded failure lost its reason")
	}
}
