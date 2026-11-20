package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M5.7: usage/budget management API (OmniRoute
// /api/usage/budget + /api/usage/om-usage).
//
//   - GET /api/usage/budget?apiKeyId= — management auth
//     (admin scope): the cost summary for one key — calendar
//     limits, warning threshold, reset cadence, last/next reset,
//     spend today / this month / this budget period, the active
//     limit, and the budget check (allow/warn/deny), plus the
//     rolling 5h cap. The shape mirrors OmniRoute's GET
//     /api/usage/budget (getCostSummary + checkBudget).
//   - POST /api/usage/budget — management auth (admin scope):
//     setBudget for one key: daily/weekly/monthly limits,
//     warning threshold, reset interval/time. Absent fields keep
//     their stored values; the 5h cap and unpriced policy are
//     NOT touched (they belong to POST /dashboard/key-budgets).
//     Ledger-first, then config persist with rollback — the
//     same pattern as handleKeyBudgetUpdate.
//   - GET /api/usage/om-usage — the terminal-friendly usage
//     command (OmniRoute's @@om-usage): any valid gateway key,
//     but only when the operator enabled allow_usage_command
//     on that key (403 otherwise). ?format=json answers JSON;
//     the default is plain text for a terminal.

// usageBudgetCheck is the allow/warn/deny verdict of GET
// /api/usage/budget (OmniRoute's checkBudget).
type usageBudgetCheck struct {
	Status string `json:"status"` // allow | warn | deny
	Reason string `json:"reason"`
}

// usageBudgetSummary is the GET /api/usage/budget payload.
type usageBudgetSummary struct {
	APIKeyID          string           `json:"api_key_id"`
	DailyLimitUSD     *float64         `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD    *float64         `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD   *float64         `json:"monthly_limit_usd,omitempty"`
	WarningThreshold  float64          `json:"warning_threshold"`
	ResetInterval     string           `json:"reset_interval"`
	ResetTime         string           `json:"reset_time"`
	BudgetResetAt     time.Time        `json:"budget_reset_at"`
	LastBudgetResetAt time.Time        `json:"last_budget_reset_at"`
	NextResetAt       time.Time        `json:"next_reset_at"`
	PeriodStartAt     time.Time        `json:"period_start_at"`
	TotalCostToday    float64          `json:"total_cost_today"`
	TotalCostMonth    float64          `json:"total_cost_month"`
	TotalCostPeriod   float64          `json:"total_cost_period"`
	ActiveLimitUSD    *float64         `json:"active_limit_usd,omitempty"`
	BudgetCheck       usageBudgetCheck `json:"budget_check"`
	LimitUSD          float64          `json:"limit_usd"`
	SpentUSD          float64          `json:"spent_usd"`
	RemainingUSD      *float64         `json:"remaining_usd,omitempty"`
	AtLimit           bool             `json:"at_limit"`
}

// usageBudgetUpdate is the POST /api/usage/budget body: every
// field optional except apiKeyId; absent fields keep stored
// values. A limit can only be cleared by deleting the whole
// budget (POST /dashboard/key-budgets with delete=true) —
// sending null is not a clear. LimitUSD is the rolling 5h cap:
// it is required ONLY when the key has no budget row yet — a
// row always carries a 5h cap, and 0 means $0 (any spend is over
// the cap), so a fresh row must never be created with an
// invented zero. When a row already exists, an absent LimitUSD
// keeps the stored 5h cap.
type usageBudgetUpdate struct {
	APIKeyID         string   `json:"apiKeyId"`
	LimitUSD         *float64 `json:"limitUsd,omitempty"`
	DailyLimitUSD    *float64 `json:"dailyLimitUsd,omitempty"`
	WeeklyLimitUSD   *float64 `json:"weeklyLimitUsd,omitempty"`
	MonthlyLimitUSD  *float64 `json:"monthlyLimitUsd,omitempty"`
	WarningThreshold *float64 `json:"warningThreshold,omitempty"`
	ResetInterval    *string  `json:"resetInterval,omitempty"`
	ResetTime        *string  `json:"resetTime,omitempty"`
}

func (s *Server) handleUsageBudget(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleUsageBudgetGet(w, r)
	case http.MethodPost:
		s.handleUsageBudgetPost(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleUsageBudgetGet(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "usage budget unavailable: no ledger wired")
		return
	}
	keyID := strings.TrimSpace(r.URL.Query().Get("apiKeyId"))
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "apiKeyId query param is required")
		return
	}
	if s.cfg != nil {
		if _, ok := s.cfg.FindGatewayKey(keyID); !ok {
			writeError(w, http.StatusNotFound, "unknown gateway key "+keyID)
			return
		}
	}
	summary, err := s.usageBudgetSummary(r.Context(), keyID, time.Now())
	if err != nil {
		s.logger.Printf("usage/budget: summary failed for key: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build budget summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// usageBudgetSummary builds the GET /api/usage/budget payload
// for one key: the stored calendar limits, the active window's
// period, spend today / this month / this period from the
// ledger, and the allow/warn/deny verdict.
func (s *Server) usageBudgetSummary(ctx context.Context, keyID string, now time.Time) (*usageBudgetSummary, error) {
	st, err := s.ledger.KeyBudgetStatus(ctx, keyID, now)
	if err != nil {
		return nil, err
	}
	periodStart, periodEnd := st.WindowStart, st.WindowEnd
	switch st.ActiveWindow {
	case "daily":
		periodStart, periodEnd = st.Daily.PeriodStart, st.Daily.NextReset
	case "weekly":
		periodStart, periodEnd = st.Weekly.PeriodStart, st.Weekly.NextReset
	case "monthly":
		periodStart, periodEnd = st.Monthly.PeriodStart, st.Monthly.NextReset
	}
	today := ledger.BudgetWindowFor(ledger.BudgetResetDaily, st.ResetTime, now)
	month := ledger.BudgetWindowFor(ledger.BudgetResetMonthly, st.ResetTime, now)
	todaySpend, err := s.ledger.SpendFor(ctx, keyID, today.PeriodStart)
	if err != nil {
		return nil, fmt.Errorf("usage/budget: spend today: %w", err)
	}
	monthSpend, err := s.ledger.SpendFor(ctx, keyID, month.PeriodStart)
	if err != nil {
		return nil, fmt.Errorf("usage/budget: spend month: %w", err)
	}
	periodSpend, err := s.ledger.SpendFor(ctx, keyID, periodStart)
	if err != nil {
		return nil, fmt.Errorf("usage/budget: spend period: %w", err)
	}
	check := usageBudgetCheck{Status: "allow", Reason: "spend is below the active limit"}
	if st.AtLimit {
		check = usageBudgetCheck{Status: "deny", Reason: "spend has reached the active limit"}
	} else if st.WarningReached {
		check = usageBudgetCheck{Status: "warn", Reason: "spend has reached the warning threshold"}
	}
	out := &usageBudgetSummary{
		APIKeyID:          keyID,
		WarningThreshold:  0.8,
		ResetInterval:     string(st.ResetInterval),
		ResetTime:         st.ResetTime,
		BudgetResetAt:     periodStart,
		LastBudgetResetAt: periodStart,
		NextResetAt:       periodEnd,
		PeriodStartAt:     periodStart,
		TotalCostToday:    todaySpend,
		TotalCostMonth:    monthSpend,
		TotalCostPeriod:   periodSpend,
		ActiveLimitUSD:    st.ActiveLimitUSD,
		BudgetCheck:       check,
		LimitUSD:          st.LimitUSD,
		SpentUSD:          st.SpentUSD,
		AtLimit:           st.AtLimit,
	}
	b, err := s.ledger.GetKeyBudget(ctx, keyID)
	if err == nil {
		out.DailyLimitUSD = b.DailyLimitUSD
		out.WeeklyLimitUSD = b.WeeklyLimitUSD
		out.MonthlyLimitUSD = b.MonthlyLimitUSD
		out.WarningThreshold = b.WarningThreshold
	}
	if st.RemainingUSD != nil {
		out.RemainingUSD = st.RemainingUSD
	}
	return out, nil
}

func (s *Server) handleUsageBudgetPost(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "usage budget unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update usageBudgetUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid usage budget update: expected {\"apiKeyId\": string, \"limitUsd\": number, \"dailyLimitUsd\": number, \"weeklyLimitUsd\": number, \"monthlyLimitUsd\": number, \"warningThreshold\": number, \"resetInterval\": \"daily\"|\"weekly\"|\"monthly\", \"resetTime\": \"HH:MM\"}")
		return
	}
	if update.APIKeyID == "" {
		writeError(w, http.StatusBadRequest, "apiKeyId is required")
		return
	}
	if s.cfg != nil {
		if _, ok := s.cfg.FindGatewayKey(update.APIKeyID); !ok {
			writeError(w, http.StatusNotFound, "unknown gateway key "+update.APIKeyID)
			return
		}
	}
	// B2: the 5h cap keeps its older rule — 0 means $0 (any spend
	// is over it) — but each D/W/M limit must be positive:
	// negative or zero is rejected here with a 400, matching
	// POST /dashboard/key-budgets, instead of slipping through
	// to the ledger and failing as a 500.
	if update.LimitUSD != nil && *update.LimitUSD < 0 {
		writeError(w, http.StatusBadRequest, "limitUsd must not be negative")
		return
	}
	// Deterministic order (daily, weekly, monthly — same as
	// enforceKeyBudgetCaps): with two or more bad windows the
	// first bad one in that order is always named, never
	// whichever key a map happens to yield first.
	for _, win := range []struct {
		name string
		v    *float64
	}{
		{"dailyLimitUsd", update.DailyLimitUSD},
		{"weeklyLimitUsd", update.WeeklyLimitUSD},
		{"monthlyLimitUsd", update.MonthlyLimitUSD},
	} {
		if win.v != nil && *win.v <= 0 {
			writeError(w, http.StatusBadRequest, win.name+" must be positive")
			return
		}
	}
	if update.WarningThreshold != nil && (*update.WarningThreshold < 0 || *update.WarningThreshold > 1) {
		writeError(w, http.StatusBadRequest, "warningThreshold must be between 0 and 1")
		return
	}
	if update.ResetInterval != nil {
		if _, ok := ledger.NormalizeBudgetResetInterval(*update.ResetInterval); !ok {
			writeError(w, http.StatusBadRequest, "unknown resetInterval "+*update.ResetInterval+" (want daily, weekly or monthly)")
			return
		}
	}
	if update.ResetTime != nil {
		if _, ok := ledger.NormalizeBudgetResetTime(*update.ResetTime); !ok {
			writeError(w, http.StatusBadRequest, "malformed resetTime "+*update.ResetTime+" (want HH:MM)")
			return
		}
	}
	ctx := r.Context()
	prev, prevErr := s.ledger.GetKeyBudget(ctx, update.APIKeyID)
	hadPrev := prevErr == nil
	next := prev
	if !hadPrev {
		// A fresh row always carries a 5h cap, and 0 means $0
		// (any spend is over it) — never invent one. The
		// caller sets it here, or first via
		// POST /dashboard/key-budgets.
		if update.LimitUSD == nil {
			writeError(w, http.StatusBadRequest, "limitUsd is required when the key has no budget yet (the 5-hour cap cannot be left unset)")
			return
		}
		next = ledger.KeyBudget{KeyID: update.APIKeyID, LimitUSD: *update.LimitUSD, UnpricedPolicy: ledger.UnpricedFailClosed, WarningThreshold: 0.8, ResetInterval: ledger.BudgetResetDaily, ResetTime: "00:00"}
	} else if update.LimitUSD != nil {
		next.LimitUSD = *update.LimitUSD
	}
	if update.DailyLimitUSD != nil {
		next.DailyLimitUSD = update.DailyLimitUSD
	}
	if update.WeeklyLimitUSD != nil {
		next.WeeklyLimitUSD = update.WeeklyLimitUSD
	}
	if update.MonthlyLimitUSD != nil {
		next.MonthlyLimitUSD = update.MonthlyLimitUSD
	}
	if update.WarningThreshold != nil {
		next.WarningThreshold = *update.WarningThreshold
	}
	if update.ResetInterval != nil {
		if iv, ok := ledger.NormalizeBudgetResetInterval(*update.ResetInterval); ok {
			next.ResetInterval = iv
		}
	}
	if update.ResetTime != nil {
		if rt, ok := ledger.NormalizeBudgetResetTime(*update.ResetTime); ok {
			next.ResetTime = rt
		}
	}
	if err := s.ledger.SetKeyBudgetConfig(ctx, next); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save usage budget")
		return
	}
	persistedToConfig := false
	if s.cfg != nil && s.configPersist != nil {
		if s.cfg.KeyBudgets == nil {
			s.cfg.KeyBudgets = map[string]config.KeyBudgetConfig{}
		}
		cfgRow := s.cfg.KeyBudgets[update.APIKeyID]
		if update.LimitUSD != nil {
			cfgRow.LimitUSD = *update.LimitUSD
		}
		if update.DailyLimitUSD != nil {
			cfgRow.DailyLimitUSD = update.DailyLimitUSD
		}
		if update.WeeklyLimitUSD != nil {
			cfgRow.WeeklyLimitUSD = update.WeeklyLimitUSD
		}
		if update.MonthlyLimitUSD != nil {
			cfgRow.MonthlyLimitUSD = update.MonthlyLimitUSD
		}
		if update.WarningThreshold != nil {
			cfgRow.WarningThreshold = *update.WarningThreshold
		}
		if update.ResetInterval != nil {
			cfgRow.ResetInterval = *update.ResetInterval
		}
		if update.ResetTime != nil {
			cfgRow.ResetTime = *update.ResetTime
		}
		s.cfg.KeyBudgets[update.APIKeyID] = cfgRow
		if err := s.configPersist(); err != nil {
			if hadPrev {
				_ = s.ledger.SetKeyBudgetConfig(ctx, prev)
			} else {
				_ = s.ledger.DeleteKeyBudget(ctx, update.APIKeyID)
			}
			s.logger.Printf("usage/budget: config persist failed, ledger rolled back: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not save usage budget to the config file")
			return
		}
		persistedToConfig = true
	}
	summary, err := s.usageBudgetSummary(ctx, update.APIKeyID, time.Now())
	if err != nil {
		s.logger.Printf("usage/budget: summary failed after update: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "budget saved but the summary could not be built")
		return
	}
	_ = persistedToConfig
	writeJSON(w, http.StatusOK, summary)
}

// omUsageJSON is the ?format=json reply of GET /api/usage/om-usage.
type omUsageJSON struct {
	Allowed        bool      `json:"allowed"`
	KeyID          string    `json:"key_id"`
	Today          float64   `json:"total_cost_today_usd"`
	Week           float64   `json:"total_cost_week_usd"`
	Month          float64   `json:"total_cost_month_usd"`
	ActiveLimitUSD *float64  `json:"active_limit_usd,omitempty"`
	RemainingUSD   *float64  `json:"remaining_usd,omitempty"`
	NextResetAt    time.Time `json:"next_reset_at"`
}

func (s *Server) handleOmUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		s.omUsageDeny(w, r, http.StatusUnauthorized, "a gateway key is required")
		return
	}
	if s.cfg == nil {
		s.omUsageDeny(w, r, http.StatusServiceUnavailable, "usage command unavailable: no config loaded")
		return
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		s.omUsageDeny(w, r, http.StatusUnauthorized, "key record unavailable")
		return
	}
	if !key.AllowUsageCommand {
		s.omUsageDeny(w, r, http.StatusForbidden, "usage command is disabled for this key (the operator enables allow_usage_command per key)")
		return
	}
	if s.ledger == nil {
		s.omUsageDeny(w, r, http.StatusServiceUnavailable, "usage command unavailable: no ledger wired")
		return
	}
	now := time.Now()
	st, err := s.ledger.KeyBudgetStatus(r.Context(), keyID, now)
	if err != nil {
		s.omUsageDeny(w, r, http.StatusInternalServerError, "could not read budget status")
		return
	}
	week := ledger.BudgetWindowFor(ledger.BudgetResetWeekly, st.ResetTime, now)
	month := ledger.BudgetWindowFor(ledger.BudgetResetMonthly, st.ResetTime, now)
	todaySpend, _ := s.ledger.SpendFor(r.Context(), keyID, ledger.BudgetWindowFor(ledger.BudgetResetDaily, st.ResetTime, now).PeriodStart)
	weekSpend, _ := s.ledger.SpendFor(r.Context(), keyID, week.PeriodStart)
	monthSpend, _ := s.ledger.SpendFor(r.Context(), keyID, month.PeriodStart)
	periodEnd := st.WindowEnd
	switch st.ActiveWindow {
	case "daily":
		periodEnd = st.Daily.NextReset
	case "weekly":
		periodEnd = st.Weekly.NextReset
	case "monthly":
		periodEnd = st.Monthly.NextReset
	}
	out := omUsageJSON{
		Allowed: true, KeyID: keyID,
		Today: todaySpend, Week: weekSpend, Month: monthSpend,
		ActiveLimitUSD: st.ActiveLimitUSD, RemainingUSD: st.RemainingUSD,
		NextResetAt: periodEnd,
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		writeJSON(w, http.StatusOK, out)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "usage for key %s\n", keyID)
	fmt.Fprintf(w, "  today: $%.4f\n", out.Today)
	fmt.Fprintf(w, "  week:  $%.4f\n", out.Week)
	fmt.Fprintf(w, "  month: $%.4f\n", out.Month)
	if out.ActiveLimitUSD != nil {
		fmt.Fprintf(w, "  active limit: $%.2f\n", *out.ActiveLimitUSD)
	}
	if out.RemainingUSD != nil {
		fmt.Fprintf(w, "  remaining: $%.4f\n", *out.RemainingUSD)
	}
	fmt.Fprintf(w, "  next reset: %s\n", out.NextResetAt.UTC().Format(time.RFC3339))
}

// omUsageDeny answers a refused om-usage call in the caller's
// chosen shape: JSON when ?format=json, plain text otherwise —
// the same shape rule as the success path, so a terminal client
// never has to parse JSON for an error.
func (s *Server) omUsageDeny(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if strings.EqualFold(r.URL.Query().Get("format"), "json") {
		writeJSON(w, code, map[string]any{"allowed": false, "error": map[string]string{"message": msg}})
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, msg)
}
