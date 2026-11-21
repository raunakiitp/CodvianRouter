package server

import (
	"net/http"
	"sort"
	"time"
)

// budgetTableRow is one key's row in GET /api/analytics/budget-table.
// Every number comes from ledger.KeyBudgetStatus, the same status
// the enforcement gate uses, so the table can never disagree with
// what the router actually refuses.
type budgetTableRow struct {
	KeyID           string   `json:"key_id"`
	KeyName         string   `json:"key_name"`
	HasBudget       bool     `json:"has_budget"`
	TodayUSD        float64  `json:"today_usd"`
	MonthUSD        float64  `json:"month_usd"`
	DailyLimitUSD   *float64 `json:"daily_limit_usd,omitempty"`
	MonthlyLimitUSD *float64 `json:"monthly_limit_usd,omitempty"`
	ActiveWindow    string   `json:"active_window"`
	ActiveLimitUSD  *float64 `json:"active_limit_usd,omitempty"`
	UsedPct         *float64 `json:"used_pct,omitempty"`
	Status          string   `json:"status"` // "none", "ok", "warning", "over"
}

// handleBudgetTable serves GET /api/analytics/budget-table. The
// previous version divided USD spend by token-count limits and
// read its warning threshold from an in-memory map nothing set
// for real, so its percentages were meaningless.
func (s *Server) handleBudgetTable(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rows := []budgetTableRow{}
	if s.cfg == nil || s.ledger == nil {
		writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": 0, "available": s.ledger != nil})
		return
	}
	now := time.Now()
	for _, k := range s.cfg.GatewayKeys {
		st, err := s.ledger.KeyBudgetStatus(r.Context(), k.ID, now)
		if err != nil {
			writeServerError(w, http.StatusInternalServerError, "budget table: "+err.Error())
			return
		}
		row := budgetTableRow{
			KeyID:           k.ID,
			KeyName:         k.Name,
			HasBudget:       st.HasBudget,
			TodayUSD:        st.Daily.SpentUSD,
			MonthUSD:        st.Monthly.SpentUSD,
			DailyLimitUSD:   st.Daily.LimitUSD,
			MonthlyLimitUSD: st.Monthly.LimitUSD,
			ActiveWindow:    st.ActiveWindow,
			ActiveLimitUSD:  st.ActiveLimitUSD,
			Status:          "none",
		}
		if st.ActiveLimitUSD != nil && *st.ActiveLimitUSD > 0 {
			spent := st.SpentUSD
			switch st.ActiveWindow {
			case "daily":
				spent = st.Daily.SpentUSD
			case "weekly":
				spent = st.Weekly.SpentUSD
			case "monthly":
				spent = st.Monthly.SpentUSD
			}
			pct := spent / *st.ActiveLimitUSD * 100
			row.UsedPct = &pct
		}
		switch {
		case st.AtLimit:
			row.Status = "over"
		case st.WarningReached:
			row.Status = "warning"
		case st.HasBudget:
			row.Status = "ok"
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		pi, pj := -1.0, -1.0
		if rows[i].UsedPct != nil {
			pi = *rows[i].UsedPct
		}
		if rows[j].UsedPct != nil {
			pj = *rows[j].UsedPct
		}
		return pi > pj
	})
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "count": len(rows), "available": true})
}
