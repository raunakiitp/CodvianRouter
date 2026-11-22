package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/eval"
)

// evalMaxBody bounds the schedule create/update request body.
const evalMaxBody = 64 << 10

// ensureEval lazily creates the scheduled-evaluation store + scheduler
// once, persisted under cfg.DataDir, and starts the scheduler loop.
// Without a data dir it answers 503 (memory-only is never used for the
// server, so a silently-unpersisted schedule can never be mistaken for
// durable).
func (s *Server) ensureEval() error {
	var err error
	s.evalInitOnce.Do(func() {
		var dir string
		if s.cfg == nil || s.cfg.DataDir == "" {
			err = errors.New("eval: unavailable: no data dir wired")
			return
		}
		dir = filepath.Join(s.cfg.DataDir, "eval-schedules.json")
		st, e := eval.Open(dir)
		if e != nil {
			err = e
			return
		}
		s.evalStore = st
		sched := &eval.Scheduler{Store: st}
		sched.NewEngine = func() eval.Engine { return s.engine }
		if s.webhooks != nil {
			sched.Notify = s.evalNotify
		}
		s.evalSched = sched
		ctx, cancel := context.WithCancel(context.Background())
		s.evalCancel = cancel
		go sched.Run(ctx, 30*time.Second)
	})
	return err
}

// evalNotify forwards evaluation events onto the webhook dispatcher
// under the eval.* event names (best-effort).
func (s *Server) evalNotify(event, scheduleID, runID string) {
	if s.webhooks == nil {
		return
	}
	s.webhooks.Notify(event, map[string]any{
		"schedule_id": scheduleID,
		"run_id":      runID,
		"at":          time.Now().UTC(),
	})
}

// evalScheduleView is the API projection of a schedule (no internals).
type evalScheduleView struct {
	ID                        string  `json:"id"`
	Suite                     string  `json:"suite"`
	Every                     string  `json:"every"` // human-readable duration
	Enabled                   bool    `json:"enabled"`
	TimeZone                  string  `json:"time_zone,omitempty"`
	MinRunsForBaseline        int     `json:"min_runs_for_baseline,omitempty"`
	RegressionMaxPassRateDrop float64 `json:"regression_max_pass_rate_drop,omitempty"`
	MaxConcurrent             int     `json:"max_concurrent,omitempty"`
	LastRun                   string  `json:"last_run,omitempty"`
}

func evalViewOf(s *eval.Schedule) evalScheduleView {
	v := evalScheduleView{
		ID:                        s.ID,
		Suite:                     s.Suite,
		Every:                     s.Every.String(),
		Enabled:                   s.Enabled,
		TimeZone:                  s.TimeZone,
		MinRunsForBaseline:        s.MinRunsForBaseline,
		RegressionMaxPassRateDrop: s.RegressionMaxPassRateDrop,
		MaxConcurrent:             s.MaxConcurrent,
	}
	if !s.LastRun.IsZero() {
		v.LastRun = s.LastRun.UTC().Format(time.RFC3339)
	}
	return v
}

type evalSchedulesResponse struct {
	Schedules []evalScheduleView `json:"schedules"`
}

type evalScheduleRequest struct {
	ID                        string   `json:"id"`
	Suite                     string   `json:"suite"`
	Every                     string   `json:"every"`
	Enabled                   *bool    `json:"enabled"`
	MinRunsForBaseline        *int     `json:"min_runs_for_baseline"`
	RegressionMaxPassRateDrop *float64 `json:"regression_max_pass_rate_drop"`
	MaxConcurrent             *int     `json:"max_concurrent"`
}

// handleEvalSchedules serves GET (list) and POST (create).
func (s *Server) handleEvalSchedules(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureEval(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := evalSchedulesResponse{Schedules: []evalScheduleView{}}
		for _, sch := range s.evalStore.List() {
			out.Schedules = append(out.Schedules, evalViewOf(sch))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		s.handleEvalScheduleCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleEvalScheduleCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, evalMaxBody)
	var req evalScheduleRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid schedule: expected {\"id\": string, \"every\": \"30m\", ...}")
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Suite = strings.TrimSpace(req.Suite)
	every, err := time.ParseDuration(req.Every)
	if err != nil || every <= 0 {
		writeError(w, http.StatusBadRequest, "every must be a positive duration (e.g. \"30m\", \"1h\")")
		return
	}
	sch := &eval.Schedule{ID: req.ID, Suite: req.Suite, Every: every, Enabled: true}
	if req.Enabled != nil {
		sch.Enabled = *req.Enabled
	}
	if req.MinRunsForBaseline != nil {
		sch.MinRunsForBaseline = *req.MinRunsForBaseline
	}
	if req.RegressionMaxPassRateDrop != nil {
		sch.RegressionMaxPassRateDrop = *req.RegressionMaxPassRateDrop
	}
	if req.MaxConcurrent != nil {
		sch.MaxConcurrent = *req.MaxConcurrent
	}
	if err := s.evalStore.Upsert(sch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, evalViewOf(sch))
}

// handleEvalScheduleByID serves GET / PUT / DELETE /api/eval/schedules/{id}.
func (s *Server) handleEvalScheduleByID(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureEval(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	sch, ok := s.evalStore.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "schedule not found")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, evalViewOf(sch))
	case http.MethodDelete:
		if err := s.evalStore.Delete(id); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
	case http.MethodPut:
		s.handleEvalScheduleUpdate(w, r, sch)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleEvalScheduleUpdate(w http.ResponseWriter, r *http.Request, sch *eval.Schedule) {
	r.Body = http.MaxBytesReader(w, r.Body, evalMaxBody)
	var req evalScheduleRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid schedule update")
		return
	}
	if req.Every != "" {
		every, err := time.ParseDuration(req.Every)
		if err != nil || every <= 0 {
			writeError(w, http.StatusBadRequest, "every must be a positive duration")
			return
		}
		sch.Every = every
	}
	if req.Suite != "" {
		sch.Suite = strings.TrimSpace(req.Suite)
	}
	if req.Enabled != nil {
		sch.Enabled = *req.Enabled
	}
	if req.MinRunsForBaseline != nil {
		sch.MinRunsForBaseline = *req.MinRunsForBaseline
	}
	if req.RegressionMaxPassRateDrop != nil {
		sch.RegressionMaxPassRateDrop = *req.RegressionMaxPassRateDrop
	}
	if req.MaxConcurrent != nil {
		sch.MaxConcurrent = *req.MaxConcurrent
	}
	if err := s.evalStore.Upsert(sch); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, evalViewOf(sch))
}

type evalRunView struct {
	ID           string   `json:"id"`
	ScheduleID   string   `json:"schedule_id"`
	Suite        string   `json:"suite"`
	StartedAt    string   `json:"started_at"`
	FinishedAt   string   `json:"finished_at"`
	Status       string   `json:"status"`
	PassRate     float64  `json:"pass_rate"`
	Total        int      `json:"total"`
	Passed       int      `json:"passed"`
	FailedCases  []string `json:"failed_cases,omitempty"`
	LatencyMs    int64    `json:"latency_ms"`
	Error        string   `json:"error,omitempty"`
	IsRegression bool     `json:"is_regression"`
}

type evalHistoryResponse struct {
	Runs []evalRunView `json:"runs"`
}

// handleEvalHistory serves GET /api/eval/schedules/{id}/history.
func (s *Server) handleEvalHistory(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureEval(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	if _, ok := s.evalStore.Get(id); !ok {
		writeError(w, http.StatusNotFound, "schedule not found")
		return
	}
	out := evalHistoryResponse{Runs: []evalRunView{}}
	for _, run := range s.evalStore.History(id, 50) {
		out.Runs = append(out.Runs, evalRunView{
			ID: run.ID, ScheduleID: run.ScheduleID, Suite: run.Suite,
			StartedAt: run.StartedAt.UTC().Format(time.RFC3339), FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339),
			Status: run.Status, PassRate: run.PassRate, Total: run.Total, Passed: run.Passed,
			FailedCases: run.FailedCases, LatencyMs: run.LatencyMs, Error: run.Error, IsRegression: run.IsRegression,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleEvalTrigger serves POST /api/eval/schedules/{id}/trigger and runs
// the suite immediately (synchronously, bounded to the HTTP request).
func (s *Server) handleEvalTrigger(w http.ResponseWriter, r *http.Request) {
	if err := s.ensureEval(); err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	id := r.PathValue("id")
	sch, ok := s.evalStore.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "schedule not found")
		return
	}
	s.evalStore.Upsert(&eval.Schedule{ID: sch.ID, Suite: sch.Suite, Every: sch.Every,
		Enabled: true, MinRunsForBaseline: sch.MinRunsForBaseline,
		RegressionMaxPassRateDrop: sch.RegressionMaxPassRateDrop, MaxConcurrent: sch.MaxConcurrent})
	// Re-run through the scheduler's execution path synchronously.
	h := &eval.Harness{Engine: s.engine, Timeout: 120 * time.Second}
	rep := h.Run(r.Context(), eval.CasesForSuite(sch.Suite))
	started := time.Now()
	run := &eval.Run{ScheduleID: id, Suite: sch.Suite, StartedAt: started, FinishedAt: time.Now(),
		Total: rep.Total, Passed: rep.Passed, LatencyMs: time.Since(started).Milliseconds()}
	for _, res := range rep.Results {
		if !res.OK {
			run.FailedCases = append(run.FailedCases, res.CaseID)
		}
	}
	if rep.Total == 0 {
		run.Status = "failed"
		run.Error = "eval: no cases in suite"
	} else {
		run.Status = "completed"
		run.PassRate = rep.PassRate
	}
	run.IsRegression = s.evalStore.DetectRegression(id, run, sch)
	rid, _ := s.evalStore.AddRun(run)
	writeJSON(w, http.StatusOK, evalRunView{ID: rid, ScheduleID: id, Suite: sch.Suite,
		StartedAt: run.StartedAt.UTC().Format(time.RFC3339), FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339),
		Status: run.Status, PassRate: run.PassRate, Total: run.Total, Passed: run.Passed,
		FailedCases: run.FailedCases, LatencyMs: run.LatencyMs, Error: run.Error, IsRegression: run.IsRegression})
}

// registerEvalRoutes registers the P3-B evaluation management routes.
// Classification (guard.go): schedules are Management/AlwaysProtected
// with the admin scope (operators), matching webhooks.
func (s *Server) registerEvalRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/eval/schedules", s.handleEvalSchedules)
	s.register(mux, "POST", "/api/eval/schedules", s.handleEvalSchedules)
	s.register(mux, "GET", "/api/eval/schedules/{id}", s.handleEvalScheduleByID)
	s.register(mux, "PUT", "/api/eval/schedules/{id}", s.handleEvalScheduleByID)
	s.register(mux, "DELETE", "/api/eval/schedules/{id}", s.handleEvalScheduleByID)
	s.register(mux, "GET", "/api/eval/schedules/{id}/history", s.handleEvalHistory)
	s.register(mux, "POST", "/api/eval/schedules/{id}/trigger", s.handleEvalTrigger)
}
