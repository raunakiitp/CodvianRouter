// P3-B: scheduled evaluation REST API.
//
// Exposes the eval.Scheduler engine through the management API so an
// operator can create/pause/resume/delete recurring evaluations, read
// their history, and see recent regression flags. All mutations require
// admin scope (guard Tier); reads require gateway-key auth unless the
// dashboard feed gate is open. The scheduler itself runs in a goroutine
// owned by the Server and emits webhook events on completion/regression.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/eval"
)

// evalState holds the lazily-opened P3-B eval store/scheduler on THIS
// Server instance (not package globals), so two Servers with different
// data dirs stay fully isolated. A corrupt persisted store is
// remembered so endpoints answer 503 instead of silently wiping history.
type evalState struct {
	mu        sync.Mutex
	store     *eval.Store
	scheduler *eval.Scheduler
	cancel    context.CancelFunc
	storeErr  error
}

func (s *Server) evalStateInit() *evalState {
	s.evalStateMu.Lock()
	defer s.evalStateMu.Unlock()
	if s.evalState == nil {
		s.evalState = &evalState{}
	}
	return s.evalState
}

// ensureEvalStore lazily opens the eval schedule store at
// <cfg.DataDir>/eval-schedules.json (or an in-memory store when the
// config has no data dir). A corrupt file is remembered so the
// endpoints answer 503 instead of silently wiping history.
func (s *Server) ensureEvalStore() (*eval.Store, error) {
	st := s.evalStateInit()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.storeErr != nil {
		return nil, st.storeErr
	}
	if st.store != nil {
		return st.store, nil
	}
	var path string
	if s.cfg != nil && s.cfg.DataDir != "" {
		path = filepath.Join(s.cfg.DataDir, "eval-schedules.json")
	}
	store, err := eval.Open(path)
	if err != nil {
		st.storeErr = err
		return nil, err
	}
	st.store = store
	return st.store, nil
}

// startEvalScheduler starts the scheduler loop owned by the Server. It
// is called from New once. The scheduler uses the Server's routing
// engine by default (real provider-backed evaluations). Use
// WithEvalEngine to override for testing or mock scenarios.
func (s *Server) startEvalScheduler() {
	if _, err := s.ensureEvalStore(); err != nil {
		return
	}
	st := s.evalStateInit()
	st.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	if st.scheduler == nil {
		eng := s.evalEngine
		if eng == nil {
			eng = s.engine
		}
		// Adapter: server.Engine has Chat and ChatStream; eval.Engine only needs Chat.
		// Since server.Engine implements eval.Engine's Chat method, we can wrap it.
		st.scheduler = &eval.Scheduler{
			Store:     st.store,
			NewEngine: func() eval.Engine { return eng },
			Notify: func(event, scheduleID, runID string) {
				if s.webhooks == nil {
					return
				}
				s.webhooks.Notify(event, map[string]any{
					"schedule_id": scheduleID,
					"run_id":      runID,
				})
			},
		}
	}
	sch := st.scheduler
	st.mu.Unlock()
	go sch.Run(ctx, 15*time.Second)
}

// stopEvalScheduler cancels the scheduler loop (called on Shutdown).
func (s *Server) stopEvalScheduler() {
	st := s.evalStateInit()
	st.mu.Lock()
	if st.cancel != nil {
		st.cancel()
		st.cancel = nil
	}
	st.mu.Unlock()
}

// evalScheduleView is the API projection of a schedule (no internals).
type evalScheduleView struct {
	ID                    string    `json:"id"`
	Suite                 string    `json:"suite"`
	Every                 string    `json:"every"`
	Enabled               bool      `json:"enabled"`
	TimeZone              string    `json:"time_zone,omitempty"`
	MinRunsForBaseline    int       `json:"min_runs_for_baseline,omitempty"`
	RegressionMaxPassDrop float64   `json:"regression_max_pass_rate_drop,omitempty"`
	UseRealEngine         bool      `json:"use_real_engine,omitempty"`
	LastRun               time.Time `json:"last_run,omitempty"`
}

func (s *Server) evalScheduleViewOf(sch *eval.Schedule) evalScheduleView {
	v := evalScheduleView{
		ID:                    sch.ID,
		Suite:                 sch.Suite,
		Every:                 sch.Every.String(),
		Enabled:               sch.Enabled,
		TimeZone:              sch.TimeZone,
		MinRunsForBaseline:    sch.MinRunsForBaseline,
		RegressionMaxPassDrop: sch.RegressionMaxPassRateDrop,
		UseRealEngine:         sch.UseRealEngine,
		LastRun:               sch.LastRun,
	}
	return v
}

// handleEvalSchedules lists all schedules (GET /api/eval/schedules).
func (s *Server) handleEvalSchedules(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	views := make([]evalScheduleView, 0)
	for _, sch := range st.List() {
		views = append(views, s.evalScheduleViewOf(sch))
	}
	writeJSON(w, http.StatusOK, views)
}

// evalScheduleInput is the POST/PATCH body.
type evalScheduleInput struct {
	ID                    string   `json:"id"`
	Suite                 string   `json:"suite"`
	Every                 string   `json:"every"` // Go duration string
	Enabled               *bool    `json:"enabled"`
	TimeZone              string   `json:"time_zone,omitempty"`
	MinRunsForBaseline    *int     `json:"min_runs_for_baseline,omitempty"`
	RegressionMaxPassDrop *float64 `json:"regression_max_pass_rate_drop,omitempty"`
	MaxConcurrent         *int     `json:"max_concurrent,omitempty"`
	UseRealEngine         *bool    `json:"use_real_engine,omitempty"`
}

// handleEvalScheduleCreate (POST /api/eval/schedules).
func (s *Server) handleEvalScheduleCreate(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	var in evalScheduleInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if in.ID == "" {
		writeError(w, http.StatusBadRequest, "missing schedule id")
		return
	}
	every, err := time.ParseDuration(in.Every)
	if err != nil || every <= 0 {
		writeError(w, http.StatusBadRequest, "invalid or missing 'every' duration (e.g. 1h30m)")
		return
	}
	sch := &eval.Schedule{ID: in.ID, Suite: in.Suite, Every: every, TimeZone: in.TimeZone}
	if in.Enabled != nil {
		sch.Enabled = *in.Enabled
	} else {
		sch.Enabled = true
	}
	if in.MinRunsForBaseline != nil {
		sch.MinRunsForBaseline = *in.MinRunsForBaseline
	}
	if in.RegressionMaxPassDrop != nil {
		sch.RegressionMaxPassRateDrop = *in.RegressionMaxPassDrop
	}
	if in.MaxConcurrent != nil {
		sch.MaxConcurrent = *in.MaxConcurrent
	}
	if in.UseRealEngine != nil {
		sch.UseRealEngine = *in.UseRealEngine
	}
	if err := st.Upsert(sch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.evalScheduleViewOf(sch))
}

// handleEvalScheduleByID handles GET (read) and PATCH (partial update)
// of one schedule.
func (s *Server) handleEvalScheduleByID(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		sch, ok := st.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "no such schedule")
			return
		}
		writeJSON(w, http.StatusOK, s.evalScheduleViewOf(sch))
	case http.MethodPatch:
		sch, ok := st.Get(id)
		if !ok {
			writeError(w, http.StatusNotFound, "no such schedule")
			return
		}
		var in evalScheduleInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
			return
		}
		if in.Every != "" {
			every, err := time.ParseDuration(in.Every)
			if err != nil || every <= 0 {
				writeError(w, http.StatusBadRequest, "invalid 'every' duration")
				return
			}
			sch.Every = every
		}
		if in.Enabled != nil {
			sch.Enabled = *in.Enabled
		}
		if in.TimeZone != "" {
			sch.TimeZone = in.TimeZone
		}
		if in.MinRunsForBaseline != nil {
			sch.MinRunsForBaseline = *in.MinRunsForBaseline
		}
		if in.RegressionMaxPassDrop != nil {
			sch.RegressionMaxPassRateDrop = *in.RegressionMaxPassDrop
		}
		if in.MaxConcurrent != nil {
			sch.MaxConcurrent = *in.MaxConcurrent
		}
		if in.UseRealEngine != nil {
			sch.UseRealEngine = *in.UseRealEngine
		}
		if err := st.Upsert(sch); err != nil {
			writeError(w, http.StatusBadRequest, "invalid schedule: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.evalScheduleViewOf(sch))
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleEvalScheduleDelete (DELETE /api/eval/schedules/{id}).
func (s *Server) handleEvalScheduleDelete(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	if err := st.Delete(r.PathValue("id")); err != nil {
		writeError(w, http.StatusInternalServerError, "delete failed: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// evalRunView is the history projection.
type evalRunView struct {
	ID           string             `json:"id"`
	ScheduleID   string             `json:"schedule_id"`
	Suite        string             `json:"suite"`
	StartedAt    time.Time          `json:"started_at"`
	FinishedAt   time.Time          `json:"finished_at"`
	Status       string             `json:"status"`
	PassRate     float64            `json:"pass_rate"`
	Total        int                `json:"total"`
	Passed       int                `json:"passed"`
	FailedCases  []string           `json:"failed_cases,omitempty"`
	Failures     []eval.CaseFailure `json:"failures,omitempty"`
	LatencyMs    int64              `json:"latency_ms"`
	Error        string             `json:"error,omitempty"`
	IsRegression bool               `json:"is_regression"`
}

func (s *Server) evalRunViewOf(run eval.Run) evalRunView {
	return evalRunView{
		ID:           run.ID,
		ScheduleID:   run.ScheduleID,
		Suite:        run.Suite,
		StartedAt:    run.StartedAt,
		FinishedAt:   run.FinishedAt,
		Status:       run.Status,
		PassRate:     run.PassRate,
		Total:        run.Total,
		Passed:       run.Passed,
		FailedCases:  run.FailedCases,
		Failures:     run.Failures,
		LatencyMs:    run.LatencyMs,
		Error:        run.Error,
		IsRegression: run.IsRegression,
	}
}

// handleEvalScheduleHistory (GET /api/eval/schedules/{id}/history) lists
// runs newest-first, optionally limited by ?limit=.
func (s *Server) handleEvalScheduleHistory(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs := st.History(r.PathValue("id"), limit)
	views := make([]evalRunView, 0, len(runs))
	for _, rn := range runs {
		views = append(views, s.evalRunViewOf(rn))
	}
	writeJSON(w, http.StatusOK, views)
}

// handleEvalScheduleTrends (GET /api/eval/schedules/{id}/trends) returns
// the schedule's pass-rate series (oldest first, ?limit= default 20) plus
// a baseline comparison. Direction is "unknown" until two completed runs
// exist — one number is a measurement, not a trend.
func (s *Server) handleEvalScheduleTrends(w http.ResponseWriter, r *http.Request) {
	st, err := s.ensureEvalStore()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "eval store unavailable: "+err.Error())
		return
	}
	id := r.PathValue("id")
	if _, ok := st.Get(id); !ok {
		writeError(w, http.StatusNotFound, "no such schedule")
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	writeJSON(w, http.StatusOK, st.PassRateTrend(id, limit))
}

// registerEvalRoutes wires the P3-B evaluation API. guard rows already
// exist, so register() will not panic on a missing classification.
func (s *Server) registerEvalRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/eval/schedules", s.handleEvalSchedules)
	s.register(mux, "POST", "/api/eval/schedules", s.handleEvalScheduleCreate)
	s.register(mux, "GET", "/api/eval/schedules/{id}", s.handleEvalScheduleByID)
	s.register(mux, "PATCH", "/api/eval/schedules/{id}", s.handleEvalScheduleByID)
	s.register(mux, "DELETE", "/api/eval/schedules/{id}", s.handleEvalScheduleDelete)
	s.register(mux, "GET", "/api/eval/schedules/{id}/history", s.handleEvalScheduleHistory)
	s.register(mux, "GET", "/api/eval/schedules/{id}/trends", s.handleEvalScheduleTrends)
}
