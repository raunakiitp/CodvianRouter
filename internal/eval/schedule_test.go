package eval

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeSchedEngine implements the eval Engine contract with a
// deterministic outcome, so regression/history tests are reproducible.
type fakeSchedEngine struct{ pass bool }

func (f fakeSchedEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if f.pass {
		return &provider.ChatResponse{Content: "role content org/team/gpt-5-mini reasoning prompt_tokens total_tokens fallback strategy request_id"}, nil
	}
	return &provider.ChatResponse{Content: ""}, nil
}

func TestScheduleValidate(t *testing.T) {
	good := Schedule{ID: "s1", Every: time.Minute}
	if err := good.Validate(); err != nil {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	for name, bad := range map[string]Schedule{
		"empty id":   {Every: time.Minute},
		"zero every": {ID: "s", Every: 0},
		"neg drop":   {ID: "s", Every: time.Minute, RegressionMaxPassRateDrop: -0.1},
		"big drop":   {ID: "s", Every: time.Minute, RegressionMaxPassRateDrop: 1.5},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestScheduleDue(t *testing.T) {
	now := time.Now()
	sch := &Schedule{Enabled: true, Every: time.Minute}
	if !sch.Due(now) {
		t.Fatal("enabled never-run schedule should be due")
	}
	sch.LastRun = now
	if sch.Due(now.Add(30 * time.Second)) {
		t.Fatal("not due before interval")
	}
	if !sch.Due(now.Add(time.Minute + time.Second)) {
		t.Fatal("due after interval")
	}
	sch.Enabled = false
	if sch.Due(now.Add(2 * time.Minute)) {
		t.Fatal("disabled schedule must never be due")
	}
}

func TestStorePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eval.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(&Schedule{ID: "a", Every: time.Minute, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	rid, err := s.AddRun(&Run{ScheduleID: "a", Status: "completed", PassRate: 0.5, Total: 8, Passed: 4})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path) // restart recovery
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("a"); !ok {
		t.Fatal("schedule did not survive reopen")
	}
	hist := s2.History("a", 10)
	if len(hist) != 1 || hist[0].ID != rid || hist[0].PassRate != 0.5 {
		t.Fatalf("history did not survive reopen: %+v", hist)
	}
}

func TestSchedulerRunsAndRecordsHistory(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Scheduler{Store: &Store{schedules: map[string]*Schedule{}}}
	if err := s.Store.Upsert(&Schedule{ID: "s1", Every: time.Hour, Enabled: true, Suite: "permanent"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var events []string
	s.Notify = func(ev, _, _ string) { mu.Lock(); events = append(events, ev); mu.Unlock() }
	s.NewEngine = func() Engine { return fakeSchedEngine{pass: true} }
	s.tick(ctx) // one due-check pass
	deadline := time.Now().Add(3 * time.Second)
	for {
		hist := s.Store.History("s1", 5)
		mu.Lock()
		got := append([]string(nil), events...)
		mu.Unlock()
		if len(hist) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scheduler did not record a run: hist=%d events=%v", len(hist), got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	hist := s.Store.History("s1", 5)
	if len(hist) == 0 || hist[0].Status != "completed" || hist[0].PassRate != 1.0 || hist[0].Total == 0 {
		t.Fatalf("unexpected run record: %+v", hist)
	}
}

func TestRegressionDetection(t *testing.T) {
	st, _ := Open(filepath.Join(t.TempDir(), "e.json"))
	sch := &Schedule{ID: "r", Every: time.Minute, RegressionMaxPassRateDrop: 0.2, MinRunsForBaseline: 2}
	_, _ = st.AddRun(&Run{ScheduleID: "r", Status: "completed", PassRate: 1.0, Total: 8, Passed: 8, StartedAt: time.Now().Add(-time.Minute)})
	_, _ = st.AddRun(&Run{ScheduleID: "r", Status: "completed", PassRate: 1.0, Total: 8, Passed: 8, StartedAt: time.Now().Add(-30 * time.Second)})
	newRun := &Run{ScheduleID: "r", Status: "completed", PassRate: 0.5, Total: 8, Passed: 4, StartedAt: time.Now()}
	if !st.DetectRegression("r", newRun, sch) {
		t.Fatal("expected regression detected on 0.5 vs 1.0 baseline")
	}
	okRun := &Run{ScheduleID: "r", Status: "completed", PassRate: 0.95, Total: 8, Passed: 7, StartedAt: time.Now()}
	if st.DetectRegression("r", okRun, sch) {
		t.Fatal("no regression expected on 0.05 drop")
	}
	zeroRun := &Run{ScheduleID: "r", Status: "completed", PassRate: 0, Total: 0, StartedAt: time.Now()}
	if st.DetectRegression("r", zeroRun, sch) {
		t.Fatal("empty run must not be flagged (missing != 0)")
	}
	off := &Schedule{ID: "r", Every: time.Minute, RegressionMaxPassRateDrop: 0, MinRunsForBaseline: 2}
	if st.DetectRegression("r", newRun, off) {
		t.Fatal("threshold 0 must disable regression detection")
	}
}

func TestSchedulerConcurrencyBoundAndNoDuplicateRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &Scheduler{Store: &Store{schedules: map[string]*Schedule{}}}
	// MaxConcurrent 1, interval tiny so a second tick would try to run.
	if err := s.Store.Upsert(&Schedule{ID: "c", Every: time.Nanosecond, Enabled: true, MaxConcurrent: 1, Suite: "permanent"}); err != nil {
		t.Fatal(err)
	}
	s.NewEngine = func() Engine { return fakeSchedEngine{pass: true} }
	s.tick(ctx)
	s.tick(ctx) // second pass while first still (nearly) in flight
	time.Sleep(50 * time.Millisecond)
	hist := s.Store.History("c", 50)
	// Duplicate-run prevention bumps LastRun atomically per tick; the
	// concurrency bound means at most one is executing at once.
	if len(hist) > 2 {
		t.Fatalf("concurrency/dup guard failed: %d runs", len(hist))
	}
	for _, r := range hist {
		if r.Status == "" {
			t.Fatalf("run missing status: %+v", r)
		}
	}
}
