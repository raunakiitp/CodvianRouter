// Quota schedules (M5.9): the server side — the pre-call gate
// on the chat paths, and the GET/POST /dashboard/quota-schedules
// feeds for the operator.
//
// The gate runs right after the fair-share pool gate (a refused
// request consumes no traffic slot): for every pool the caller's
// key belongs to, the ledger decides (see
// internal/ledger/schedule.go — the sector-typical checkSchedules). A
// block is a 429 + Retry-After when the reopen time is known,
// error type "key_pool_schedule". Every block also fires the
// "quota.exceeded" webhook, best-effort, never blocking the
// refusal. A ledger read failure fails closed with a 503: a
// limit that cannot be read must not be waived. Skipped entirely
// with no ledger, no verifiable key, or no pools for the key.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// checkQuotaSchedules enforces the caller's pool schedules. It
// writes the refusal and returns true when the request must stop
// here, before any provider is called.
func (s *Server) checkQuotaSchedules(w http.ResponseWriter, r *http.Request) bool {
	if s.ledger == nil {
		return false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return false
	}
	ctx := r.Context()
	now := time.Now()
	pools, err := s.ledger.PoolsForKey(ctx, keyID)
	if err != nil {
		s.logger.Printf("quota-schedule: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusServiceUnavailable, "quota schedule check unavailable")
		return true
	}
	for _, pool := range pools {
		block, err := s.ledger.CheckPoolSchedules(ctx, pool, now)
		if err != nil {
			s.logger.Printf("quota-schedule: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusServiceUnavailable, "quota schedule check unavailable")
			return true
		}
		if block != nil {
			s.notifyQuotaExceeded(keyID, pool.ID, block)
			if block.RetryAfterSeconds != nil && *block.RetryAfterSeconds > 0 {
				w.Header().Set("Retry-After", fmt.Sprintf("%d", *block.RetryAfterSeconds))
			}
			writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: block.Error(), Type: "key_pool_schedule"}})
			return true
		}
	}
	return false
}

// notifyQuotaExceeded fires the "quota.exceeded" webhook for a
// schedule block (the sector baseline notifyQuotaExceeded). Best-effort:
// a nil dispatcher (no webhooks configured) is a no-op, and the
// dispatcher itself never blocks or returns errors.
func (s *Server) notifyQuotaExceeded(keyID, poolID string, block *ledger.ScheduleBlock) {
	if s.webhooks == nil {
		return
	}
	payload := map[string]any{
		"key_id":      keyID,
		"pool_id":     poolID,
		"reason":      "schedule-" + block.Reason,
		"schedule_id": block.ScheduleID,
	}
	switch block.Reason {
	case "reserve":
		// The floor that was breached: used is the pool's
		// saturation (0..1), limit is the most saturation the
		// floor allows (1 - percent/100).
		payload["quota"] = "keep-free-reserve"
		payload["used"] = block.Used
		payload["limit"] = 1 - block.Percent/100
		payload["pct"] = block.Used * 100
	case "budget":
		payload["quota"] = "schedule-budget"
		payload["used"] = block.Used
		payload["limit"] = block.Limit
		if block.Limit > 0 {
			payload["pct"] = block.Used / block.Limit * 100
		}
	}
	s.webhooks.Notify(webhooks.QuotaExceededEvent, payload)
}

// ---------------------------------------------------------------------------
// GET/POST /dashboard/quota-schedules.
// ---------------------------------------------------------------------------

// quotaScheduleReserveView is one keep-free floor of a schedule.
type quotaScheduleReserveView struct {
	Window  string  `json:"window"`
	Percent float64 `json:"percent"`
}

// quotaScheduleView is one row of the GET
// /dashboard/quota-schedules payload, with its live verdict.
type quotaScheduleView struct {
	ID           string                     `json:"id"`
	PoolID       string                     `json:"pool_id"`
	Label        string                     `json:"label,omitempty"`
	Days         []int                      `json:"days"`
	StartMinute  int                        `json:"start_minute"`
	EndMinute    int                        `json:"end_minute"`
	Timezone     string                     `json:"timezone,omitempty"`
	Mode         string                     `json:"mode"`
	Reserves     []quotaScheduleReserveView `json:"reserves,omitempty"`
	BudgetValue  *float64                   `json:"budget_value,omitempty"`
	BudgetUnit   string                     `json:"budget_unit,omitempty"`
	BudgetWindow string                     `json:"budget_window,omitempty"`
	Priority     int                        `json:"priority"`
	Enabled      bool                       `json:"enabled"`
	Verdict      string                     `json:"verdict"`
	Reason       string                     `json:"reason,omitempty"`
	RetryAfter   *int                       `json:"retry_after_seconds,omitempty"`
}

// quotaSchedulesResponse is the GET /dashboard/quota-schedules payload.
type quotaSchedulesResponse struct {
	Schedules []quotaScheduleView `json:"schedules"`
}

// handleQuotaSchedules serves GET /dashboard/quota-schedules:
// every schedule (or one pool's, with ?pool_id=) with its live
// verdict at now. With no ledger wired it answers an empty list.
func (s *Server) handleQuotaSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := quotaSchedulesResponse{Schedules: []quotaScheduleView{}}
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	now := time.Now()
	poolID := r.URL.Query().Get("pool_id")
	var pools []ledger.QuotaPool
	var err error
	if poolID != "" {
		var pool ledger.QuotaPool
		pool, err = s.ledger.GetQuotaPool(ctx, poolID)
		if err != nil {
			writeError(w, http.StatusNotFound, "quota pool not found")
			return
		}
		pools = []ledger.QuotaPool{pool}
	} else {
		pools, err = s.ledger.ListQuotaPools(ctx)
		if err != nil {
			s.logger.Printf("quota-schedule: list failed: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not list quota schedules")
			return
		}
	}
	for _, pool := range pools {
		schedules, err := s.ledger.ListQuotaSchedules(ctx, pool.ID)
		if err != nil {
			s.logger.Printf("quota-schedule: list failed for pool %s: %v", pool.ID, guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not list quota schedules")
			return
		}
		verdict := ledger.ResolveActiveSchedule(schedules, now)
		for _, sched := range schedules {
			view := quotaScheduleView{
				ID: sched.ID, PoolID: sched.PoolID, Label: sched.Label,
				Days: sched.Days, StartMinute: sched.StartMinute, EndMinute: sched.EndMinute,
				Timezone: sched.Timezone, Mode: sched.Mode,
				BudgetValue: sched.BudgetValue, BudgetUnit: sched.BudgetUnit, BudgetWindow: sched.BudgetWindow,
				Priority: sched.Priority, Enabled: sched.Enabled,
				Reserves: []quotaScheduleReserveView{},
			}
			for _, res := range sched.Reserves {
				view.Reserves = append(view.Reserves, quotaScheduleReserveView{Window: res.Window, Percent: res.Percent})
			}
			// The verdict is per pool: every schedule of the
			// pool reports it, and the matching schedule's id
			// is the one the verdict names.
			view.Verdict = verdict.Kind
			if verdict.Schedule != nil && verdict.Schedule.ID == sched.ID {
				view.Reason = verdict.Reason
				view.RetryAfter = verdict.RetryAfterSeconds
			} else if verdict.Kind == "closed" {
				view.Reason = verdict.Reason
				view.RetryAfter = verdict.RetryAfterSeconds
			}
			out.Schedules = append(out.Schedules, view)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// quotaScheduleUpdate is the POST /dashboard/quota-schedules
// body: a full schedule upsert (reserves replace wholesale when
// present; absent fields keep their stored values), or a delete.
type quotaScheduleUpdate struct {
	ID           string                    `json:"id"`
	PoolID       *string                   `json:"pool_id,omitempty"`
	Label        *string                   `json:"label,omitempty"`
	Days         *[]int                    `json:"days,omitempty"`
	StartMinute  *int                      `json:"start_minute,omitempty"`
	EndMinute    *int                      `json:"end_minute,omitempty"`
	Timezone     *string                   `json:"timezone,omitempty"`
	Mode         *string                   `json:"mode,omitempty"`
	Reserves     *[]config.ScheduleReserve `json:"reserves,omitempty"`
	BudgetValue  *float64                  `json:"budget_value,omitempty"`
	BudgetUnit   *string                   `json:"budget_unit,omitempty"`
	BudgetWindow *string                   `json:"budget_window,omitempty"`
	Priority     *int                      `json:"priority,omitempty"`
	Enabled      *bool                     `json:"enabled,omitempty"`
	Delete       bool                      `json:"delete"`
}

// scheduleConfigToLedger maps a config schedule to the ledger shape.
func scheduleConfigToLedger(s config.QuotaScheduleConfig) ledger.QuotaSchedule {
	out := ledger.QuotaSchedule{
		ID: s.ID, PoolID: s.PoolID, Label: s.Label,
		Days: s.Days, StartMinute: s.StartMinute, EndMinute: s.EndMinute,
		Timezone: s.Timezone, Mode: s.Mode,
		BudgetValue: s.BudgetValue, BudgetUnit: s.BudgetUnit, BudgetWindow: s.BudgetWindow,
		Priority: s.Priority, Enabled: s.Enabled,
		Reserves: []ledger.ReserveRule{},
	}
	for _, r := range s.Reserves {
		out.Reserves = append(out.Reserves, ledger.ReserveRule{Window: r.Window, Percent: r.Percent})
	}
	return out
}

// scheduleLedgerToConfig maps a ledger schedule to the config shape.
func scheduleLedgerToConfig(s ledger.QuotaSchedule) config.QuotaScheduleConfig {
	out := config.QuotaScheduleConfig{
		ID: s.ID, PoolID: s.PoolID, Label: s.Label,
		Days: s.Days, StartMinute: s.StartMinute, EndMinute: s.EndMinute,
		Timezone: s.Timezone, Mode: s.Mode,
		BudgetValue: s.BudgetValue, BudgetUnit: s.BudgetUnit, BudgetWindow: s.BudgetWindow,
		Priority: s.Priority, Enabled: s.Enabled,
		Reserves: []config.ScheduleReserve{},
	}
	for _, r := range s.Reserves {
		out.Reserves = append(out.Reserves, config.ScheduleReserve{Window: r.Window, Percent: r.Percent})
	}
	return out
}

// validateQuotaScheduleUpdate checks one schedule body the same
// way the config validation does: pool exists, days are
// weekdays, minutes in range, mode allow/block, timezone
// loadable, reserves name a known window with a 0-100 percent,
// budget value and unit together.
func (s *Server) validateQuotaScheduleUpdate(ctx context.Context, c config.QuotaScheduleConfig) error {
	if c.ID == "" {
		return errors.New("id is required")
	}
	if c.PoolID == "" {
		return errors.New("pool_id is required")
	}
	if s.ledger != nil {
		if _, err := s.ledger.GetQuotaPool(ctx, c.PoolID); err != nil {
			return fmt.Errorf("pool_id %q does not name a quota pool", c.PoolID)
		}
	}
	for _, d := range c.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("days has %d, which is not a weekday (0-6, 0 = Sunday)", d)
		}
	}
	if c.StartMinute < 0 || c.StartMinute > 1439 {
		return fmt.Errorf("start_minute must be between 0 and 1439 (got %d)", c.StartMinute)
	}
	if c.EndMinute < 1 || c.EndMinute > 1440 {
		return fmt.Errorf("end_minute must be between 1 and 1440 (got %d)", c.EndMinute)
	}
	switch c.Mode {
	case "allow", "block":
	default:
		return fmt.Errorf("mode %q is not a known mode (allow, block)", c.Mode)
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("timezone %q is not a known IANA zone: %v", c.Timezone, err)
		}
	}
	for i, r := range c.Reserves {
		switch r.Window {
		case "5h", "hourly", "daily", "weekly", "monthly", "any":
		default:
			return fmt.Errorf("reserves[%d].window %q is not a known window (5h, hourly, daily, weekly, monthly, any)", i, r.Window)
		}
		if r.Percent < 0 || r.Percent > 100 {
			return fmt.Errorf("reserves[%d].percent must be between 0 and 100 (got %v)", i, r.Percent)
		}
	}
	if c.BudgetValue != nil {
		if *c.BudgetValue <= 0 {
			return fmt.Errorf("budget_value must be positive (got %v)", *c.BudgetValue)
		}
		switch c.BudgetUnit {
		case "requests", "tokens", "usd":
		default:
			return fmt.Errorf("budget_unit %q is not a known unit (requests, tokens, usd)", c.BudgetUnit)
		}
		switch c.BudgetWindow {
		case "5h", "hourly", "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("budget_window %q is not a known window (5h, hourly, daily, weekly, monthly)", c.BudgetWindow)
		}
	} else if c.BudgetUnit != "" || c.BudgetWindow != "" {
		return errors.New("budget_unit/budget_window must be empty without a budget_value")
	}
	return nil
}

// handleQuotaScheduleUpdate serves POST /dashboard/quota-
// schedules. The ledger rows are written first, then the config
// file's quota_schedules block (when a config + persist function
// are wired) so the two never disagree — a config save failure
// rolls the ledger change back and answers 500, exactly like the
// key budgets and the quota pools.
func (s *Server) handleQuotaScheduleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "quota schedules unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update quotaScheduleUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid quota schedule update: expected {\"id\": string, \"pool_id\": string, \"days\": [...], \"start_minute\": number, \"end_minute\": number, \"mode\": \"allow\"|\"block\", \"delete\": bool}")
		return
	}
	if update.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	prev, prevErr := s.ledger.GetQuotaSchedule(ctx, update.ID)
	hadPrev := prevErr == nil
	if update.Delete {
		if err := s.ledger.DeleteQuotaSchedule(ctx, update.ID); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not delete quota schedule")
			return
		}
	} else {
		next := config.QuotaScheduleConfig{ID: update.ID, Mode: "allow", EndMinute: 1440, Enabled: true}
		if hadPrev {
			next = scheduleLedgerToConfig(prev)
		}
		applyScheduleUpdate(&next, &update)
		if err := s.validateQuotaScheduleUpdate(ctx, next); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.ledger.SetQuotaSchedule(ctx, scheduleConfigToLedger(next)); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save quota schedule")
			return
		}
	}
	// Config block: keep the file in step with the ledger, or
	// roll the ledger back when the save fails.
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.QuotaScheduleConfig(nil), s.cfg.QuotaSchedules...)
		if update.Delete {
			kept := s.cfg.QuotaSchedules[:0]
			for _, p := range s.cfg.QuotaSchedules {
				if p.ID != update.ID {
					kept = append(kept, p)
				}
			}
			s.cfg.QuotaSchedules = kept
		} else {
			next := config.QuotaScheduleConfig{ID: update.ID}
			for _, p := range s.cfg.QuotaSchedules {
				if p.ID == update.ID {
					next = p
					break
				}
			}
			applyScheduleUpdate(&next, &update)
			found := false
			for i, p := range s.cfg.QuotaSchedules {
				if p.ID == update.ID {
					s.cfg.QuotaSchedules[i] = next
					found = true
					break
				}
			}
			if !found {
				s.cfg.QuotaSchedules = append(s.cfg.QuotaSchedules, next)
			}
		}
		if err := s.configPersist(); err != nil {
			// Roll the ledger back: runtime and file must never
			// disagree about a quota schedule.
			s.cfg.QuotaSchedules = prevCfg
			if hadPrev {
				_ = s.ledger.SetQuotaSchedule(ctx, prev)
			} else {
				_ = s.ledger.DeleteQuotaSchedule(ctx, update.ID)
			}
			writeServerError(w, http.StatusInternalServerError, "could not persist quota schedule config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// applyScheduleUpdate merges a POST body over a config schedule
// (shared by the ledger-next and config-next builds so the two
// never disagree about which fields a partial update sets).
func applyScheduleUpdate(next *config.QuotaScheduleConfig, update *quotaScheduleUpdate) {
	if update.PoolID != nil {
		next.PoolID = *update.PoolID
	}
	if update.Label != nil {
		next.Label = *update.Label
	}
	if update.Days != nil {
		next.Days = *update.Days
	}
	if update.StartMinute != nil {
		next.StartMinute = *update.StartMinute
	}
	if update.EndMinute != nil {
		next.EndMinute = *update.EndMinute
	}
	if update.Timezone != nil {
		next.Timezone = *update.Timezone
	}
	if update.Mode != nil {
		next.Mode = *update.Mode
	}
	if update.Reserves != nil {
		next.Reserves = *update.Reserves
	}
	if update.BudgetValue != nil {
		next.BudgetValue = update.BudgetValue
	}
	if update.BudgetUnit != nil {
		next.BudgetUnit = *update.BudgetUnit
	}
	if update.BudgetWindow != nil {
		next.BudgetWindow = *update.BudgetWindow
	}
	if update.Priority != nil {
		next.Priority = *update.Priority
	}
	if update.Enabled != nil {
		next.Enabled = *update.Enabled
	}
}
