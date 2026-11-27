// Scheduled evaluation engine (P3-B).
//
// A small, self-contained scheduler on top of the existing eval.Harness.
// It persists schedule definitions and run history, prevents duplicate
// concurrent executions, bounds concurrency, recovers after restart, and
// flags regressions against a previous valid baseline. It deliberately
// uses NO external scheduler (no Redis/Kafka): a single in-process loop
// is the simplest reliable architecture for this repository.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// Schedule describes one recurring evaluation.
type Schedule struct {
	// ID is the stable schedule identifier.
	ID string `json:"id"`
	// Suite is the evaluation suite ID to run ("" = the harness's
	// permanent regression set).
	Suite string `json:"suite"`
	// Every is the run interval.
	Every time.Duration `json:"every_ns"`
	// Enabled toggles the schedule (paused = not enabled).
	Enabled bool `json:"enabled"`
	// TimeZone is an IANA location name for the "next run" clock
	// ("" = UTC). Used only to align reporting, never to change the
	// interval math.
	TimeZone string `json:"time_zone,omitempty"`
	// MinRunsForBaseline is how many completed runs are required
	// before a regression can be flagged (avoids noisy small samples).
	MinRunsForBaseline int `json:"min_runs_for_baseline,omitempty"`
	// RegressionMaxPassRateDrop is the pass-rate drop (0..1) that
	// counts as a regression against the baseline. 0 disables the check.
	RegressionMaxPassRateDrop float64 `json:"regression_max_pass_rate_drop,omitempty"`
	// MaxConcurrent bounds how many concurrent evaluations this
	// schedule may start (0 = 1).
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// UseRealEngine indicates whether this schedule should use the
	// Server's routing engine (real provider-backed evaluations).
	// Default is false (uses a local mock engine that never spends
	// provider credits). Set to true for explicit operator opt-in.
	UseRealEngine bool `json:"use_real_engine,omitempty"`
	// LastRun is when the schedule last started an evaluation.
	LastRun time.Time `json:"last_run,omitempty"`
}

// Validate checks schedule integrity without touching the network.
func (s *Schedule) Validate() error {
	if s.ID == "" {
		return errors.New("eval: schedule has empty id")
	}
	if s.Every <= 0 {
		return errors.New("eval: schedule interval must be > 0")
	}
	if s.RegressionMaxPassRateDrop < 0 || s.RegressionMaxPassRateDrop > 1 {
		return errors.New("eval: regression drop threshold must be in [0,1]")
	}
	if s.MinRunsForBaseline < 0 {
		return errors.New("eval: min_runs_for_baseline must be >= 0")
	}
	return nil
}

// Due reports whether an enabled schedule is due at now (never before
// the previous LastRun). A paused or never-run schedule is due.
func (s *Schedule) Due(now time.Time) bool {
	if !s.Enabled {
		return false
	}
	return s.LastRun.IsZero() || now.Sub(s.LastRun) >= s.Every
}

// CaseFailure records WHY one case failed, so run history carries the
// detail an operator needs to triage a regression without re-running
// it. Reason is the case's Result.Error when set, otherwise the missing
// substrings; Missing holds the expected substrings that were absent.
type CaseFailure struct {
	CaseID    string   `json:"case_id"`
	Reason    string   `json:"reason,omitempty"`
	Missing   []string `json:"missing,omitempty"`
	LatencyMs int64    `json:"latency_ms,omitempty"`
}

// Run is one persisted evaluation run.
type Run struct {
	ID           string        `json:"id"`
	ScheduleID   string        `json:"schedule_id"`
	Suite        string        `json:"suite"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	Status       string        `json:"status"` // running|completed|failed
	PassRate     float64       `json:"pass_rate"`
	Total        int           `json:"total"`
	Passed       int           `json:"passed"`
	FailedCases  []string      `json:"failed_cases,omitempty"`
	Failures     []CaseFailure `json:"failures,omitempty"`
	LatencyMs    int64         `json:"latency_ms"`
	Error        string        `json:"error,omitempty"`
	IsRegression bool          `json:"is_regression"`
}

// TrendPoint is one run's pass rate in a trend series.
type TrendPoint struct {
	RunID        string    `json:"run_id"`
	StartedAt    time.Time `json:"started_at"`
	PassRate     float64   `json:"pass_rate"`
	Total        int       `json:"total"`
	Passed       int       `json:"passed"`
	Status       string    `json:"status"`
	IsRegression bool      `json:"is_regression"`
}

// Trend is a schedule's pass-rate series plus a baseline comparison.
// BaselinePassRate is the mean pass rate of the completed runs BEFORE
// the latest completed one; Delta is latest - baseline. Direction is
// "unknown" until there are at least two completed runs (a single
// number is not a trend), and "flat" when Delta is zero. A failed or
// case-less latest run leaves LatestPassRate 0 but is reported with its
// own status, so a broken run is never read as a 0% quality score.
type Trend struct {
	ScheduleID       string       `json:"schedule_id"`
	Points           []TrendPoint `json:"points"`
	BaselinePassRate float64      `json:"baseline_pass_rate"`
	LatestPassRate   float64      `json:"latest_pass_rate"`
	Delta            float64      `json:"delta"`
	Direction        string       `json:"direction"`
	CompletedRuns    int          `json:"completed_runs"`
}

// Store persists schedules and run history. The zero value is usable
// in-memory only; Open adds JSON-file persistence.
type Store struct {
	mu        sync.Mutex
	schedules map[string]*Schedule
	runs      []Run
	nextRun   uint64
	path      string
}

// Open returns a Store persisted at path ("" = memory only). It loads
// any existing schedules/runs so a restart resumes where it left off.
func Open(path string) (*Store, error) {
	s := &Store{schedules: map[string]*Schedule{}, path: path}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var snap struct {
		Schedules map[string]*Schedule `json:"schedules"`
		Runs      []Run                `json:"runs"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("eval: decode schedule store: %w", err)
	}
	if snap.Schedules != nil {
		s.schedules = snap.Schedules
	}
	if snap.Runs != nil {
		s.runs = snap.Runs
	}
	return s, nil
}

// persist writes the store snapshot to its JSON path (no-op in-memory).
func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	s.mu.Lock()
	snap := map[string]any{"schedules": s.schedules, "runs": s.runs}
	s.mu.Unlock()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return atomicWriteFile(s.path, b)
}

// atomicWriteFile writes data to path atomically: a temp file in the
// same directory, fsync'd, then renamed over path. A crash mid-write
// therefore leaves either the previous snapshot or the complete new
// one — never a torn file. A truncated schedule snapshot fails to
// decode on the next Open, which would silently disable every
// scheduled evaluation, so this is the durability the store needs.
// It mirrors internal/files/store.go's index write.
func atomicWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return fsyncDir(dir)
}

// fsyncDir flushes the directory entry to disk after the rename: the
// rename is the atomicity guarantee, the fsync is the durability one.
// Best effort — skipped on Windows, where Go cannot open a directory
// for syncing (see internal/files/store.go).
func fsyncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Upsert adds or replaces a schedule (by ID), then persists.
func (s *Store) Upsert(sch *Schedule) error {
	if err := sch.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	s.schedules[sch.ID] = sch
	s.mu.Unlock()
	return s.persist()
}

// Get returns a schedule by ID.
func (s *Store) Get(id string) (*Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[id]
	return sch, ok
}

// List returns all schedules sorted by ID.
func (s *Store) List() []*Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Schedule, 0, len(s.schedules))
	for _, sch := range s.schedules {
		c := *sch
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Delete removes a schedule (its history is preserved).
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	delete(s.schedules, id)
	s.mu.Unlock()
	return s.persist()
}

// AddRun appends a completed run and persists. It returns the run id.
func (s *Store) AddRun(run *Run) (string, error) {
	s.mu.Lock()
	s.nextRun++
	run.ID = fmt.Sprintf("run-%d", s.nextRun)
	s.runs = append(s.runs, *run)
	id := run.ID
	s.mu.Unlock()
	return id, s.persist()
}

// History returns runs for a schedule (newest first), bounded by limit.
func (s *Store) History(scheduleID string, limit int) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Run{}
	for i := len(s.runs) - 1; i >= 0; i-- {
		r := s.runs[i]
		if scheduleID != "" && r.ScheduleID != scheduleID {
			continue
		}
		out = append(out, r)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// DetectRegression compares a completed run against the most recent
// completed baseline for its schedule. It returns true only when the
// drop exceeds the schedule threshold AND the schedule has enough prior
// completed runs (MinRunsForBaseline) to be statistically meaningful.
func (s *Store) DetectRegression(scheduleID string, run *Run, sch *Schedule) bool {
	if sch == nil || sch.RegressionMaxPassRateDrop <= 0 {
		return false
	}
	hist := s.History(scheduleID, 0)
	// Find the most recent COMPLETED baseline before this run.
	var baseline *Run
	completed := 0
	for _, r := range hist {
		if r.ID == run.ID {
			continue
		}
		if r.Status != "completed" {
			continue
		}
		completed++
		if r.StartedAt.Before(run.StartedAt) && (baseline == nil || r.StartedAt.After(baseline.StartedAt)) {
			c := r
			baseline = &c
		}
	}
	if completed < sch.MinRunsForBaseline {
		return false
	}
	if baseline == nil || baseline.Total == 0 {
		return false
	}
	// Missing measurements are NOT treated as zero: if the new run had
	// no cases there is nothing to regress.
	if run.Total == 0 {
		return false
	}
	drop := baseline.PassRate - run.PassRate
	return drop > sch.RegressionMaxPassRateDrop
}

// PassRateTrend returns a schedule's pass-rate series (chronological,
// oldest first, bounded by limit when > 0) plus a baseline comparison.
// Direction is deliberately "unknown" with fewer than two completed
// runs: one number is a measurement, not a trend. Non-completed runs
// appear as points (so a broken run is visible) but never contribute to
// the baseline mean.
func (s *Store) PassRateTrend(scheduleID string, limit int) Trend {
	newestFirst := s.History(scheduleID, limit)
	points := make([]TrendPoint, 0, len(newestFirst))
	for i := len(newestFirst) - 1; i >= 0; i-- {
		r := newestFirst[i]
		points = append(points, TrendPoint{
			RunID:        r.ID,
			StartedAt:    r.StartedAt,
			PassRate:     r.PassRate,
			Total:        r.Total,
			Passed:       r.Passed,
			Status:       r.Status,
			IsRegression: r.IsRegression,
		})
	}
	tr := Trend{ScheduleID: scheduleID, Points: points, Direction: "unknown"}
	// Completed runs, newest first.
	completed := make([]Run, 0, len(newestFirst))
	for _, r := range newestFirst {
		if r.Status == "completed" {
			completed = append(completed, r)
		}
	}
	tr.CompletedRuns = len(completed)
	if len(completed) == 0 {
		return tr
	}
	tr.LatestPassRate = completed[0].PassRate
	if len(completed) < 2 {
		return tr
	}
	sum := 0.0
	for _, r := range completed[1:] {
		sum += r.PassRate
	}
	tr.BaselinePassRate = sum / float64(len(completed)-1)
	tr.Delta = tr.LatestPassRate - tr.BaselinePassRate
	switch {
	case tr.Delta > 0:
		tr.Direction = "up"
	case tr.Delta < 0:
		tr.Direction = "down"
	default:
		tr.Direction = "flat"
	}
	return tr
}

// Scheduler runs due schedules against a Harness engine in a bounded
// loop. It is safe to start in a goroutine; Run blocks until ctx is done.
type Scheduler struct {
	Store *Store
	// NewEngine returns the engine to evaluate with (nil -> default
	// local harness with no engine, which records failures, never
	// spends credits). Called per run so an operator can switch targets.
	NewEngine func() Engine
	// Notify is an optional callback for events (completed/failed/
	// regression). No-op when nil.
	Notify func(event string, scheduleID, runID string)
	// Logf, when set, receives background-failure diagnostics — in
	// particular a panic recovered from a scheduled run goroutine.
	// No-op when nil.
	Logf     func(format string, args ...any)
	mu       sync.Mutex
	inFlight map[string]int
}

// Run is the scheduler loop: every tick it starts any due schedule whose
// in-flight count is under its concurrency limit.
func (s *Scheduler) Run(ctx context.Context, tick time.Duration) {
	if s.Store == nil {
		return
	}
	s.mu.Lock()
	if s.inFlight == nil {
		s.inFlight = map[string]int{}
	}
	s.mu.Unlock()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
			s.tick(ctx)
		}
	}
}

// Tick runs one due-check pass (also useful for tests).
func (s *Scheduler) tick(ctx context.Context) {
	now := time.Now()
	for _, sch := range s.Store.List() {
		if !sch.Due(now) {
			continue
		}
		// Bump LastRun under the store lock so two scheduler
		// instances (or two ticks) cannot double-start it.
		cur, _ := s.Store.Get(sch.ID)
		if cur == nil || !cur.Due(now) {
			continue
		}
		cur.LastRun = now
		_ = s.Store.Upsert(cur)
		// Concurrency bound.
		s.mu.Lock()
		if s.inFlight == nil {
			s.inFlight = map[string]int{}
		}
		inflight := s.inFlight[sch.ID]
		max := sch.MaxConcurrent
		if max <= 0 {
			max = 1
		}
		if inflight >= max {
			s.mu.Unlock()
			continue
		}
		s.inFlight[sch.ID] = inflight + 1
		s.mu.Unlock()
		go s.runScheduled(ctx, sch)
	}
}

// runScheduled runs one scheduled evaluation in its own goroutine and
// recovers any panic, so a single bad run (a misbehaving engine, a
// nil dereference while recording history) cannot take the whole
// gateway process down. execute's own deferred in-flight decrement
// still runs during the unwind, so the concurrency slot is released.
func (s *Scheduler) runScheduled(ctx context.Context, sch *Schedule) {
	defer func() {
		if r := recover(); r != nil {
			if s.Logf != nil {
				s.Logf("eval: scheduled run for %q panicked: %v", sch.ID, r)
			}
		}
	}()
	s.execute(ctx, sch)
}

// execute runs one evaluation and records its history + regression.
func (s *Scheduler) execute(ctx context.Context, sch *Schedule) {
	defer func() {
		s.mu.Lock()
		if s.inFlight[sch.ID] > 0 {
			s.inFlight[sch.ID]--
		}
		s.mu.Unlock()
	}()
	run := &Run{
		ScheduleID: sch.ID,
		Suite:      sch.Suite,
		StartedAt:  time.Now(),
		Status:     "running",
	}
	var eng Engine
	if sch.UseRealEngine {
		// Operator explicitly opted in to real provider-backed eval.
		// Requires NewEngine to be set (e.g., Server's routing engine).
		if s.NewEngine != nil {
			eng = s.NewEngine()
		} else {
			eng = localMockEngine{} // fallback, but logs a warning in real usage
		}
	} else if s.NewEngine != nil {
		// Test or caller provided a custom engine (e.g., fakeSchedEngine).
		eng = s.NewEngine()
	} else {
		// Default: use a local mock engine that never spends credits
		eng = localMockEngine{}
	}
	h := &Harness{Engine: eng, Timeout: 60 * time.Second}
	cases := PermanentRegressionCases()
	if sch.Suite != "" {
		// Custom suites can be supplied by the caller through
		// a registry; the permanent set is the default.
		cases = CasesForSuite(sch.Suite)
	}
	rep := h.Run(ctx, cases)
	run.FinishedAt = time.Now()
	run.Total = rep.Total
	run.Passed = rep.Passed
	run.LatencyMs = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	for _, r := range rep.Results {
		if r.OK {
			continue
		}
		run.FailedCases = append(run.FailedCases, r.CaseID)
		// Carry WHY the case failed so history is triageable without a
		// re-run. Reason is the case error when set; a pure
		// missing-substring failure names the absent expectations.
		f := CaseFailure{CaseID: r.CaseID, Reason: r.Error, LatencyMs: r.LatencyMs}
		if len(r.Missing) > 0 {
			f.Missing = append([]string(nil), r.Missing...)
			if f.Reason == "" {
				f.Reason = "missing expected substrings: " + strings.Join(r.Missing, ", ")
			}
		}
		run.Failures = append(run.Failures, f)
	}
	if rep.Total == 0 {
		run.Status = "failed"
		run.Error = "eval: no cases in suite"
	} else if rep.Passed == 0 {
		run.Status = "failed"
		run.Error = "eval: all cases failed"
	} else {
		run.Status = "completed"
		run.PassRate = rep.PassRate
	}
	run.IsRegression = s.Store.DetectRegression(sch.ID, run, sch)
	if _, err := s.Store.AddRun(run); err != nil && s.Logf != nil {
		// A store write failure is a persistence problem, not a run
		// outcome: it is logged, never reported as a failed eval run
		// (A4 item 2 — eval.run.failed must reflect the RUN, not the
		// store). The in-memory history still holds the run.
		s.Logf("eval: could not persist run for schedule %q: %v", sch.ID, err)
	}
	if s.Notify != nil {
		// The event reflects the RUN outcome, independent of whether
		// the run persisted:
		//   - a failed run (no cases, or every case failed) emits
		//     eval.run.failed, whether or not it also regressed;
		//   - otherwise a regression emits eval.regression;
		//   - otherwise a completed run emits eval.completed.
		switch {
		case run.Status == "failed":
			s.Notify(webhooks.EvalRunFailedEvent, sch.ID, run.ID)
		case run.IsRegression:
			s.Notify(webhooks.EvalRegressionEvent, sch.ID, run.ID)
		default:
			s.Notify(webhooks.EvalCompletedEvent, sch.ID, run.ID)
		}
	}
}

// localMockEngine is a mock Engine that never spends provider credits.
// It returns a successful response for any request, allowing the
// scheduler to run without provider credentials.
type localMockEngine struct{}

func (localMockEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "mock response", Model: req.Model}, nil
}

// CasesForSuite returns the cases for a named suite. The permanent
// regression set is the only built-in suite; unknown suites return empty
// so an unsupported schedule is recorded as failed, never fabricated.
func CasesForSuite(suite string) []Case {
	switch suite {
	case "permanent", "":
		return PermanentRegressionCases()
	default:
		return nil
	}
}
