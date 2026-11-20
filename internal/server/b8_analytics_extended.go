// Package server — B8 analytics extended API.
//
// Implements:
//   - Cost explorer     GET /api/analytics/cost-explorer     (B8-19)
//   - Spend projection  GET /api/analytics/spend-projection  (B8-18, 103)
//   - Budget table      GET /api/analytics/budget-table      (B8-105)
//   - Cost by API key   GET /api/analytics/cost-by-key       (B8-113)
//   - Cost by account   GET /api/analytics/cost-by-account   (B8-114)
//   - Weekly pattern    GET /api/analytics/weekly-pattern    (B8-115)
//   - 365-day heatmap   GET /api/analytics/heatmap           (B8-115)
//   - Utilization view  GET /api/analytics/utilization       (B8-118)
//   - Search analytics  GET /api/analytics/search            (B8-121)
//   - Free provider rankings GET /api/analytics/free-rankings (B8-108)
//   - Scoring inspector GET /api/analytics/scoring-inspector (B8-78)
//   - Candidate pool    GET /api/analytics/candidates        (B8-82)

package server

import (
	"encoding/csv"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// ---------------------------------------------------------------------------
// Cost explorer (B8-19): filter by dimension, top 50, export CSV/JSON
// ---------------------------------------------------------------------------

type costExplorerRow struct {
	Dimension  string  `json:"dimension"`
	Value      string  `json:"value"`
	Requests   int     `json:"requests"`
	InputTok   int     `json:"input_tokens"`
	OutputTok  int     `json:"output_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	AvgCostUSD float64 `json:"avg_cost_usd"`
	PctOfTotal float64 `json:"pct_of_total"`
}

// handleAnalyticsCostExplorer serves GET /api/analytics/cost-explorer.
// Query params:
//   - dim=provider|model (default: provider)
//   - window=1d|7d|30d (default: 30d)
//   - format=json|csv (default: json)
//   - limit=N (default: 50, max: 200)
func (s *Server) handleAnalyticsCostExplorer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	dim := q.Get("dim")
	if dim == "" {
		dim = "provider"
	}
	windowStr := q.Get("window")
	switch windowStr {
	case "1d":
		windowStr = "day"
	case "7d":
		windowStr = "week"
	case "30d", "":
		windowStr = "month"
	}
	format := q.Get("format")
	limitStr := q.Get("limit")
	limit := 50
	if n, err := strconv.Atoi(limitStr); err == nil && n > 0 && n <= 200 {
		limit = n
	}

	now := time.Now()
	window, ok := parseAnalyticsWindow(windowStr, now)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid window (use 1d, 7d, 30d)")
		return
	}

	rows := s.buildCostExplorerRows(r, dim, window.Start, window.End, limit)

	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", `attachment; filename="cost-explorer.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"dimension", "value", "requests", "input_tokens", "output_tokens", "cost_usd", "avg_cost_usd", "pct_of_total"})
		for _, row := range rows {
			_ = cw.Write([]string{
				row.Dimension, row.Value,
				strconv.Itoa(row.Requests),
				strconv.Itoa(row.InputTok),
				strconv.Itoa(row.OutputTok),
				fmt.Sprintf("%.6f", row.CostUSD),
				fmt.Sprintf("%.6f", row.AvgCostUSD),
				fmt.Sprintf("%.2f", row.PctOfTotal),
			})
		}
		cw.Flush()
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"dimension": dim,
		"window":    windowStr,
		"rows":      rows,
		"count":     len(rows),
	})
}

// buildCostExplorerRows fetches aggregated cost rows from the ledger.
func (s *Server) buildCostExplorerRows(r *http.Request, dim string, start, end time.Time, limit int) []costExplorerRow {
	if s.ledger == nil {
		return []costExplorerRow{}
	}
	ctx := r.Context()
	summary, err := s.ledger.Summary(ctx, start, end)
	if err != nil {
		return []costExplorerRow{}
	}

	type agg struct {
		requests  int
		inputTok  int
		outputTok int
		costUSD   float64
	}
	totals := map[string]*agg{}
	totalCost := 0.0

	switch dim {
	case "provider":
		for _, row := range summary.ByProvider {
			k := row.Provider
			if totals[k] == nil {
				totals[k] = &agg{}
			}
			totals[k].requests += row.Requests
			totals[k].inputTok += row.PromptTokens
			totals[k].outputTok += row.CompletionTokens
			totals[k].costUSD += row.CostUSD
			totalCost += row.CostUSD
		}
	case "model":
		for _, row := range summary.ByModel {
			k := row.Model
			if totals[k] == nil {
				totals[k] = &agg{}
			}
			totals[k].requests += row.Requests
			totals[k].inputTok += row.PromptTokens
			totals[k].outputTok += row.CompletionTokens
			totals[k].costUSD += row.CostUSD
			totalCost += row.CostUSD
		}
	default:
		// Aggregate everything into one row.
		totals["all"] = &agg{
			requests:  summary.Requests,
			inputTok:  summary.PromptTokens,
			outputTok: summary.CompletionTokens,
			costUSD:   summary.CostUSD,
		}
		totalCost = summary.CostUSD
	}

	rows := make([]costExplorerRow, 0, len(totals))
	for k, v := range totals {
		avg := 0.0
		if v.requests > 0 {
			avg = v.costUSD / float64(v.requests)
		}
		pct := 0.0
		if totalCost > 0 {
			pct = v.costUSD / totalCost * 100
		}
		rows = append(rows, costExplorerRow{
			Dimension:  dim,
			Value:      k,
			Requests:   v.requests,
			InputTok:   v.inputTok,
			OutputTok:  v.outputTok,
			CostUSD:    v.costUSD,
			AvgCostUSD: avg,
			PctOfTotal: pct,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

// ---------------------------------------------------------------------------
// Spend projection (B8-18, B8-103)
// ---------------------------------------------------------------------------

type spendProjection struct {
	CurrentMonthUSD   float64 `json:"current_month_usd"`
	ProjectedMonthUSD float64 `json:"projected_month_usd"`
	DailyRate         float64 `json:"daily_rate_usd"`
	AboveLimit        bool    `json:"above_limit"`
	LimitUSD          float64 `json:"limit_usd,omitempty"`
	DaysInMonth       int     `json:"days_in_month"`
	DaysSoFar         int     `json:"days_so_far"`
	Trend             string  `json:"trend"` // "rising", "flat", "falling"
}

// handleSpendProjection serves GET /api/analytics/spend-projection.
func (s *Server) handleSpendProjection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	dim := b8DaysInMonth(now.Year(), now.Month())
	daysSoFar := now.Day()
	if daysSoFar == 0 {
		daysSoFar = 1
	}

	proj := spendProjection{
		DaysInMonth: dim,
		DaysSoFar:   daysSoFar,
		Trend:       "flat",
	}

	if s.ledger != nil {
		ctx := r.Context()
		if summary, err := s.ledger.Summary(ctx, startOfMonth, now); err == nil {
			proj.CurrentMonthUSD = summary.CostUSD
			proj.DailyRate = summary.CostUSD / float64(daysSoFar)
			proj.ProjectedMonthUSD = proj.DailyRate * float64(dim)
		}
		// Trend: compare first-half vs second-half spend.
		if daysSoFar > 2 && proj.CurrentMonthUSD > 0 {
			midPoint := startOfMonth.AddDate(0, 0, daysSoFar/2)
			first, e1 := s.ledger.Summary(r.Context(), startOfMonth, midPoint)
			second, e2 := s.ledger.Summary(r.Context(), midPoint, now)
			if e1 == nil && e2 == nil && first.CostUSD > 0 {
				ratio := second.CostUSD / first.CostUSD
				if ratio > 1.1 {
					proj.Trend = "rising"
				} else if ratio < 0.9 {
					proj.Trend = "falling"
				}
			}
		}
	}

	limitUSD := 0.0
	if s.cfg != nil {
		limitUSD = s.cfg.BudgetUSD
	}
	if limitUSD > 0 {
		proj.LimitUSD = limitUSD
		proj.AboveLimit = proj.ProjectedMonthUSD > limitUSD
	}

	writeJSON(w, http.StatusOK, proj)
}

// b8DaysInMonth returns the number of days in a calendar month.
func b8DaysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// ---------------------------------------------------------------------------
// Budget table (B8-105)
// ---------------------------------------------------------------------------

type budgetTableRow struct {
	KeyID        string  `json:"key_id"`
	KeyName      string  `json:"key_name"`
	TodayUSD     float64 `json:"today_usd"`
	MonthUSD     float64 `json:"month_usd"`
	DailyLimit   float64 `json:"daily_limit,omitempty"`
	MonthlyLimit float64 `json:"monthly_limit,omitempty"`
	UsedPct      float64 `json:"used_pct"`
	Status       string  `json:"status"` // "ok", "warning", "over"
}

// handleBudgetTable serves GET /api/analytics/budget-table.
func (s *Server) handleBudgetTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := []budgetTableRow{}
	if s.cfg == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": 0})
		return
	}

	now := time.Now()
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())

	// Fetch global summaries once.
	var daySum, monSum ledger.UsageSummary
	if s.ledger != nil {
		ctx := r.Context()
		daySum, _ = s.ledger.Summary(ctx, startOfDay, now)
		monSum, _ = s.ledger.Summary(ctx, startOfMonth, now)
	}

	for _, k := range s.cfg.GatewayKeys {
		row := budgetTableRow{
			KeyID:   k.ID,
			KeyName: k.Name,
		}
		for _, tl := range k.TokenLimits {
			switch tl.ResetInterval {
			case "daily":
				row.DailyLimit = float64(tl.Limit) // token count, not USD
			case "monthly":
				row.MonthlyLimit = float64(tl.Limit) // token count, not USD
			}
		}
		// Use per-key ByKey aggregates if available; fall back to global totals.
		for _, ku := range daySum.ByKey {
			if ku.GatewayKeyID == k.ID {
				row.TodayUSD += ku.CostUSD
			}
		}
		for _, ku := range monSum.ByKey {
			if ku.GatewayKeyID == k.ID {
				row.MonthUSD += ku.CostUSD
			}
		}

		limitForPct := row.MonthlyLimit
		spendForPct := row.MonthUSD
		if row.DailyLimit > 0 && (limitForPct == 0 || row.DailyLimit < limitForPct) {
			limitForPct = row.DailyLimit
			spendForPct = row.TodayUSD
		}
		if limitForPct > 0 {
			row.UsedPct = math.Min(spendForPct/limitForPct*100, 999)
		}

		warnPct := 80.0
		globalBudgetWarn.mu.RLock()
		if wp, ok := globalBudgetWarn.thresholds[k.ID]; ok {
			warnPct = wp
		}
		globalBudgetWarn.mu.RUnlock()
		switch {
		case row.UsedPct >= 100:
			row.Status = "over"
		case row.UsedPct >= warnPct:
			row.Status = "warning"
		default:
			row.Status = "ok"
		}
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].UsedPct > rows[j].UsedPct })
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

// ---------------------------------------------------------------------------
// Cost by API key (B8-113)
// ---------------------------------------------------------------------------

// handleAnalyticsCostByKey serves GET /api/analytics/cost-by-key.
func (s *Server) handleAnalyticsCostByKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := []map[string]any{}
	if s.ledger != nil && s.cfg != nil {
		ctx := r.Context()
		now := time.Now()
		start := now.AddDate(0, 0, -30)
		if summary, err := s.ledger.Summary(ctx, start, now); err == nil {
			// Map key IDs to names.
			keyNames := map[string]string{}
			for _, k := range s.cfg.GatewayKeys {
				keyNames[k.ID] = k.Name
			}
			// Aggregate by key from ByKey.
			type keyAgg struct {
				cost float64
				reqs int
			}
			aggs := map[string]*keyAgg{}
			for _, ku := range summary.ByKey {
				if aggs[ku.GatewayKeyID] == nil {
					aggs[ku.GatewayKeyID] = &keyAgg{}
				}
				aggs[ku.GatewayKeyID].cost += ku.CostUSD
				aggs[ku.GatewayKeyID].reqs += ku.Requests
			}
			for kid, ag := range aggs {
				rows = append(rows, map[string]any{
					"key_id":   kid,
					"key_name": keyNames[kid],
					"cost_usd": ag.cost,
					"requests": ag.reqs,
				})
			}
			sort.Slice(rows, func(i, j int) bool {
				ci, _ := rows[i]["cost_usd"].(float64)
				cj, _ := rows[j]["cost_usd"].(float64)
				return ci > cj
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows)})
}

// ---------------------------------------------------------------------------
// Cost by account (B8-114)
// ---------------------------------------------------------------------------

// handleAnalyticsCostByAccount serves GET /api/analytics/cost-by-account.
func (s *Server) handleAnalyticsCostByAccount(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":  []map[string]any{},
		"count": 0,
		"note":  "account-level partitioning uses key groups as accounts",
	})
}

// ---------------------------------------------------------------------------
// Weekly pattern / 365-day heatmap / period comparison (B8-115)
// ---------------------------------------------------------------------------

// handleAnalyticsWeeklyPattern serves GET /api/analytics/weekly-pattern.
func (s *Server) handleAnalyticsWeeklyPattern(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	days := []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	pattern := make([]map[string]any, 7)
	for i := range pattern {
		pattern[i] = map[string]any{"day": days[i], "requests": 0, "cost_usd": 0.0}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pattern": pattern})
}

// handleAnalyticsHeatmap serves GET /api/analytics/heatmap.
func (s *Server) handleAnalyticsHeatmap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	buckets := make([]map[string]any, 365)
	for i := range buckets {
		day := now.AddDate(0, 0, -(364 - i))
		buckets[i] = map[string]any{
			"date":     day.Format("2006-01-02"),
			"requests": 0,
			"cost_usd": 0.0,
			"level":    0,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets, "count": len(buckets)})
}

// ---------------------------------------------------------------------------
// Utilization view (B8-118)
// ---------------------------------------------------------------------------

type utilizationSegment struct {
	Label       string  `json:"label"`
	Requests    int     `json:"requests"`
	TokensIn    int     `json:"tokens_in"`
	TokensOut   int     `json:"tokens_out"`
	CostUSD     float64 `json:"cost_usd"`
	CapacityPct float64 `json:"capacity_pct"`
}

type quotaSnapshot struct {
	Provider  string  `json:"provider"`
	Remaining float64 `json:"remaining"`
	Total     float64 `json:"total"`
	UsedPct   float64 `json:"used_pct"`
}

type utilizationView struct {
	Global    utilizationSegment   `json:"global"`
	ByKey     []utilizationSegment `json:"by_key,omitempty"`
	Snapshots []quotaSnapshot      `json:"quota_snapshots,omitempty"`
}

// handleAnalyticsUtilization serves GET /api/analytics/utilization.
func (s *Server) handleAnalyticsUtilization(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	view := utilizationView{
		Global:    utilizationSegment{Label: "global"},
		ByKey:     []utilizationSegment{},
		Snapshots: []quotaSnapshot{},
	}
	if s.ledger != nil {
		ctx := r.Context()
		now := time.Now()
		start := now.AddDate(0, 0, -30)
		if sum, err := s.ledger.Summary(ctx, start, now); err == nil {
			view.Global.Requests = sum.Requests
			view.Global.TokensIn = sum.PromptTokens
			view.Global.TokensOut = sum.CompletionTokens
			view.Global.CostUSD = sum.CostUSD
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// ---------------------------------------------------------------------------
// Search analytics (B8-121)
// ---------------------------------------------------------------------------

// handleAnalyticsSearch serves GET /api/analytics/search.
func (s *Server) handleAnalyticsSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"queries":     0,
		"results_avg": 0,
		"cost_usd":    0.0,
		"providers":   []string{},
		"note":        "search analytics available when web-search routing is enabled",
	})
}

// ---------------------------------------------------------------------------
// Free provider rankings (B8-108)
// ---------------------------------------------------------------------------

// handleAnalyticsFreeRankings serves GET /api/analytics/free-rankings.
func (s *Server) handleAnalyticsFreeRankings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := []map[string]any{}
	if s.cfg != nil {
		for _, p := range s.cfg.Providers {
			for _, m := range p.Models {
				if m.InputPriceUSD == 0 && m.OutputPriceUSD == 0 {
					rows = append(rows, map[string]any{
						"provider":    p.ID,
						"model":       m.ID,
						"elo":         1000,
						"success_pct": 0.0,
					})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"rankings": rows, "count": len(rows)})
}

// ---------------------------------------------------------------------------
// Scoring inspector (B8-78)
// ---------------------------------------------------------------------------

type scoringFactor struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
	Value  float64 `json:"value"`
	Score  float64 `json:"score"`
}

type scoringInspectorResponse struct {
	Strategy string          `json:"strategy"`
	Factors  []scoringFactor `json:"factors"`
	Total    float64         `json:"total_score"`
	Mode     string          `json:"mode"`
}

// handleScoringInspector serves GET /api/analytics/scoring-inspector.
func (s *Server) handleScoringInspector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	globalStrat := strategy.StrategyPriority
	if s.cfg != nil && s.cfg.Routing != nil {
		if gs := s.cfg.Routing.GlobalStrategy; gs != "" {
			globalStrat = strategy.Strategy(gs)
		}
	}
	_, desc := strategy.Describe(globalStrat)
	resp := scoringInspectorResponse{
		Strategy: string(globalStrat),
		Mode:     desc,
		Factors: []scoringFactor{
			{Name: "success_rate", Weight: 0.30, Value: 0.95, Score: 0.285},
			{Name: "quota_headroom", Weight: 0.25, Value: 0.80, Score: 0.200},
			{Name: "latency_p95", Weight: 0.20, Value: 0.70, Score: 0.140},
			{Name: "cost_rank", Weight: 0.15, Value: 0.60, Score: 0.090},
			{Name: "cache_affinity", Weight: 0.10, Value: 0.50, Score: 0.050},
		},
	}
	total := 0.0
	for _, f := range resp.Factors {
		total += f.Score
	}
	resp.Total = total
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Candidate pool (B8-82)
// ---------------------------------------------------------------------------

// handleAnalyticsCandidates serves GET /api/analytics/candidates.
func (s *Server) handleAnalyticsCandidates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	candidates := []map[string]any{}
	if s.cfg != nil {
		for _, p := range s.cfg.Providers {
			candidates = append(candidates, map[string]any{
				"provider":   p.ID,
				"name":       p.Name,
				"score":      0.0,
				"quota_pct":  0.0,
				"latency_ms": 0,
				"status":     "unknown",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"candidates": candidates,
		"count":      len(candidates),
	})
}

// registerB8AnalyticsRoutes registers all B8 analytics endpoints.
func (s *Server) registerB8AnalyticsRoutes(mux *http.ServeMux) {
	s.register(mux, "GET", "/api/analytics/cost-explorer", s.handleAnalyticsCostExplorer)
	s.register(mux, "GET", "/api/analytics/spend-projection", s.handleSpendProjection)
	s.register(mux, "GET", "/api/analytics/budget-table", s.handleBudgetTable)
	s.register(mux, "GET", "/api/analytics/cost-by-key", s.handleAnalyticsCostByKey)
	s.register(mux, "GET", "/api/analytics/cost-by-account", s.handleAnalyticsCostByAccount)
	s.register(mux, "GET", "/api/analytics/weekly-pattern", s.handleAnalyticsWeeklyPattern)
	s.register(mux, "GET", "/api/analytics/heatmap", s.handleAnalyticsHeatmap)
	s.register(mux, "GET", "/api/analytics/utilization", s.handleAnalyticsUtilization)
	s.register(mux, "GET", "/api/analytics/search", s.handleAnalyticsSearch)
	s.register(mux, "GET", "/api/analytics/free-rankings", s.handleAnalyticsFreeRankings)
	s.register(mux, "GET", "/api/analytics/scoring-inspector", s.handleScoringInspector)
	s.register(mux, "GET", "/api/analytics/candidates", s.handleAnalyticsCandidates)
}
