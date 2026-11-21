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

	"github.com/raunakiitp/codvian-router/internal/scoring"
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

// ---------------------------------------------------------------------------
// Weekly pattern / 365-day heatmap / period comparison (B8-115)
// ---------------------------------------------------------------------------

// handleAnalyticsWeeklyPattern serves GET /api/analytics/weekly-pattern.
func (s *Server) handleAnalyticsWeeklyPattern(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Real data: the last 12 weeks of daily ledger buckets, summed
	// by UTC weekday. Without a ledger the feed says so instead of
	// returning seven zero rows that look like measured traffic.
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{"pattern": []any{}, "available": false, "note": "no usage ledger on this server"})
		return
	}
	const weeks = 12
	buckets, err := s.ledger.DayBuckets(r.Context(), 7*weeks, time.Now())
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "weekly pattern: "+err.Error())
		return
	}
	var reqs [7]int
	var cost [7]float64
	for _, b := range buckets {
		d := b.Start.UTC().Weekday()
		reqs[d] += b.Requests
		cost[d] += b.CostUSD
	}
	pattern := make([]map[string]any, 7)
	for i := 0; i < 7; i++ {
		pattern[i] = map[string]any{"day": time.Weekday(i).String(), "requests": reqs[i], "cost_usd": cost[i]}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pattern": pattern, "available": true, "window_days": len(buckets), "timezone": "UTC"})
}

// handleAnalyticsHeatmap serves GET /api/analytics/heatmap: one
// bucket per UTC day for the last 365 days, from the ledger.
// level is a 0-4 intensity relative to the busiest day.
func (s *Server) handleAnalyticsHeatmap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{"buckets": []any{}, "count": 0, "available": false, "note": "no usage ledger on this server"})
		return
	}
	days, err := s.ledger.DayBuckets(r.Context(), 365, time.Now())
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "heatmap: "+err.Error())
		return
	}
	peak := 0
	for _, d := range days {
		if d.Requests > peak {
			peak = d.Requests
		}
	}
	buckets := make([]map[string]any, len(days))
	for i, d := range days {
		level := 0
		if peak > 0 && d.Requests > 0 {
			level = 1 + int(math.Floor(3*float64(d.Requests)/float64(peak)))
			if level > 4 {
				level = 4
			}
		}
		buckets[i] = map[string]any{
			"date":     d.Start.UTC().Format("2006-01-02"),
			"requests": d.Requests,
			"cost_usd": d.CostUSD,
			"level":    level,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": buckets, "count": len(buckets), "available": true, "timezone": "UTC"})
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
	// Search requests are not tagged separately in the ledger, so
	// there is nothing measured to report: say so rather than
	// answer zero queries as if that were a measurement.
	writeJSON(w, http.StatusOK, map[string]any{
		"available": false,
		"providers": []string{},
		"note":      "search analytics are not recorded yet: POST /v1/search traffic is not tagged separately in the usage ledger",
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
					// Only what is known: the model is configured
					// with a zero price. No rating or success rate
					// is invented; rankable fields stay absent until
					// they are measured.
					rows = append(rows, map[string]any{
						"provider": p.ID,
						"model":    m.ID,
						"free":     true,
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
}

type scoringInspectorResponse struct {
	Strategy        string          `json:"strategy"`
	Factors         []scoringFactor `json:"factors"`
	Mode            string          `json:"mode"`
	ValuesAvailable bool            `json:"values_available"`
	Note            string          `json:"note"`
}

// handleScoringInspector serves GET /api/analytics/scoring-inspector.
func (s *Server) handleScoringInspector(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// The weights are the ones the engine actually uses for the
	// global strategy (strategy.WeightsFor, the same call behind
	// /dashboard/strategies). Per-candidate factor values are live
	// router signals that this feed does not read, so they are
	// reported as unavailable rather than invented.
	globalStrat := strategy.StrategyPriority
	var custom scoring.Weights
	if rc := s.routingConfig(); rc != nil {
		if gs := rc.GlobalStrategy; gs != "" {
			globalStrat = strategy.Strategy(gs)
		}
		if ww := rc.WeightedWeights; ww != nil {
			custom = scoring.Weights{Latency: ww.Latency, Success: ww.Success, Cost: ww.Cost, Quality: ww.Quality, Quota: ww.Quota}
		}
	}
	_, desc := strategy.Describe(globalStrat)
	wt := strategy.WeightsFor(globalStrat, custom)
	writeJSON(w, http.StatusOK, scoringInspectorResponse{
		Strategy: string(globalStrat),
		Mode:     desc,
		Factors: []scoringFactor{
			{Name: "latency", Weight: wt.Latency},
			{Name: "success", Weight: wt.Success},
			{Name: "cost", Weight: wt.Cost},
			{Name: "quality", Weight: wt.Quality},
			{Name: "quota", Weight: wt.Quota},
		},
		ValuesAvailable: false,
		Note:            "weights are the live configured weights; per-candidate factor values are not exposed by this feed",
	})
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
			// Score, quota and latency are not read by this feed,
			// so they are left out instead of reported as zero.
			candidates = append(candidates, map[string]any{
				"provider": p.ID,
				"name":     p.Name,
				"status":   "unknown",
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
	s.register(mux, "GET", "/api/analytics/weekly-pattern", s.handleAnalyticsWeeklyPattern)
	s.register(mux, "GET", "/api/analytics/heatmap", s.handleAnalyticsHeatmap)
	s.register(mux, "GET", "/api/analytics/utilization", s.handleAnalyticsUtilization)
	s.register(mux, "GET", "/api/analytics/search", s.handleAnalyticsSearch)
	s.register(mux, "GET", "/api/analytics/free-rankings", s.handleAnalyticsFreeRankings)
	s.register(mux, "GET", "/api/analytics/scoring-inspector", s.handleScoringInspector)
	s.register(mux, "GET", "/api/analytics/candidates", s.handleAnalyticsCandidates)
}
