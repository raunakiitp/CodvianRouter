package eval

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// erroringSchedEngine makes every case fail (a nil response + error),
// so a harness run over the permanent suite yields Passed == 0 and the
// run is recorded as "failed".
type erroringSchedEngine struct{}

func (erroringSchedEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("upstream down")
}

// blankSchedEngine returns an empty (successful) response for every
// case: every substring case fails, but the expectation-less
// empty-prompt case passes, so the run is "completed" with a low (but
// nonzero) pass rate — the shape a completed regression takes.
type blankSchedEngine struct{}

func (blankSchedEngine) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: ""}, nil
}

// evalEvents spins a scheduler wired to capture its Notify events, runs
// one tick, and returns the events after the store has recorded a run.
func runSchedulerOnce(t *testing.T, sch *Schedule, eng func() Engine, baselinePassRates ...float64) (*Scheduler, []string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Scheduler{Store: &Store{schedules: map[string]*Schedule{}}}
	if err := s.Store.Upsert(sch); err != nil {
		t.Fatal(err)
	}
	// Seed completed baselines (past StartedAt) so a low-pass-rate run
	// is compared against a real baseline.
	for i, pr := range baselinePassRates {
		_, _ = s.Store.AddRun(&Run{
			ScheduleID: sch.ID, Status: "completed", PassRate: pr, Total: 8, Passed: int(pr * 8),
			StartedAt: time.Now().Add(-time.Duration(i+1) * time.Minute),
		})
	}
	var mu sync.Mutex
	var events []string
	s.Notify = func(ev, _, _ string) { mu.Lock(); events = append(events, ev); mu.Unlock() }
	s.NewEngine = eng
	s.tick(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for {
		hist := s.Store.History(sch.ID, 5)
		// History includes seeded baselines, so wait for the new run: one
		// more row than the seed count.
		if len(hist) > len(baselinePassRates) {
			mu.Lock()
			got := append([]string(nil), events...)
			mu.Unlock()
			if len(got) > 0 {
				return s, got
			}
		}
		if time.Now().After(deadline) {
			mu.Lock()
			got := append([]string(nil), events...)
			mu.Unlock()
			t.Fatalf("scheduler did not record a run/event within 3s: hist=%d events=%v", len(hist), got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A4 item 2: a FAILED run (every case failed) emits eval.run.failed —
// not the old behaviour of firing only when the store write failed.
// The run is also shown to regress against a completed baseline, and
// eval.run.failed still wins: a failed run must emit failed whether or
// not it regressed.
func TestSchedulerEmitsRunFailedOnFailedRun(t *testing.T) {
	sch := &Schedule{ID: "f", Every: time.Hour, Enabled: true, Suite: "permanent",
		RegressionMaxPassRateDrop: 0.2, MinRunsForBaseline: 1}
	s, events := runSchedulerOnce(t, sch, func() Engine { return erroringSchedEngine{} }, 1.0)
	if len(events) != 1 || events[0] != webhooks.EvalRunFailedEvent {
		t.Fatalf("events = %v, want exactly [%s]", events, webhooks.EvalRunFailedEvent)
	}
	hist := s.Store.History("f", 5)
	if len(hist) == 0 || hist[0].Status != "failed" {
		t.Fatalf("recorded run = %+v, want status failed", hist)
	}
	if !hist[0].IsRegression {
		t.Errorf("run was expected to also regress vs the 1.0 baseline; got IsRegression=false (the failed event must still win)")
	}
}

// A4 item 2 (negative half): a SUCCESSFUL run emits eval.completed,
// never eval.run.failed — so the failed event tracks the RUN, not any
// store hiccup.
func TestSchedulerEmitsCompletedOnSuccess(t *testing.T) {
	sch := &Schedule{ID: "ok", Every: time.Hour, Enabled: true, Suite: "permanent"}
	_, events := runSchedulerOnce(t, sch, func() Engine { return fakeSchedEngine{pass: true} })
	if len(events) != 1 || events[0] != webhooks.EvalCompletedEvent {
		t.Fatalf("events = %v, want exactly [%s]", events, webhooks.EvalCompletedEvent)
	}
}

// A4 item 2 companion: a COMPLETED run that drops below its baseline
// emits eval.regression.
func TestSchedulerEmitsRegressionOnCompletedDrop(t *testing.T) {
	sch := &Schedule{ID: "r", Every: time.Hour, Enabled: true, Suite: "permanent",
		RegressionMaxPassRateDrop: 0.2, MinRunsForBaseline: 1}
	s, events := runSchedulerOnce(t, sch, func() Engine { return blankSchedEngine{} }, 1.0)
	if len(events) != 1 || events[0] != webhooks.EvalRegressionEvent {
		t.Fatalf("events = %v, want exactly [%s]", events, webhooks.EvalRegressionEvent)
	}
	hist := s.Store.History("r", 5)
	if len(hist) == 0 || hist[0].Status != "completed" || !hist[0].IsRegression {
		t.Fatalf("recorded run = %+v, want a completed regression", hist)
	}
}
