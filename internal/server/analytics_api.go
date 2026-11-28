package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// M8.17: read-only analytics suite over the stored ledger.
//
//   - GET /api/analytics/summary?window=day|week|month
//   - GET /api/analytics/by-provider?window=day|week|month
//   - GET /api/analytics/by-model?window=day|week|month
//   - GET /api/analytics/costs?window=day|week|month
//   - GET /api/analytics/compression?window=day|week|month
//   - GET /api/analytics/diversity?window=day|week|month
//   - GET /api/analytics/auto-routing?window=day|week|month
//   - GET /api/analytics/combo-health?window=day|week|month
//
// The last four were built by G17, removed as dead code in the
// M10-15 cleanup (never registered/guarded), and rebuilt in Stage 1
// (S1-LEDGER2 + S1-B417-FOLLOWUP) — the guard table is still the
// single auth authority for all eight routes.
//
// PROPOSED REGISTRATION (coordinator owns guard.go / server.go —
// these handlers deliberately do no auth themselves, exactly like
// handleUsageBudget, so the guard table is the single auth
// authority): Tier 3 (MANAGEMENT) + scope ["admin"] on all eight
// routes. Rationale: the roll-ups aggregate spend across ALL
// gateway keys, so they are operator-only; they are read-only, so
// Tier 2 (reserved for destructive routes) does not apply. The
// /api/usage/budget precedent (Tier 3 + admin) is the same shape.
//
// DATA SOURCE — stored rows only, never live traffic. Every
// endpoint aggregates ledger.Summary over one of the ledger's own
// calendar windows (ledger.BudgetWindowFor, UTC, reset 00:00):
// day = [today 00:00, tomorrow 00:00), week = [Monday 00:00, next
// Monday 00:00), month = [1st 00:00, next 1st 00:00). No analytics
// computation runs on the chat-completions request path: these
// handlers run only when the analytics endpoint itself is hit,
// and they read rows the router already recorded.
//
// HONESTY RULES (the the sector baseline failures this slice must not
// repeat):
//   - Unpriced usage is UNKNOWN cost, never $0. ledger.Summary
//     sums cost_usd, and a row with no configured price records
//     cost 0 — indistinguishable in that sum from a genuinely
//     free ($0-priced) row. The priced/unpriced split therefore
//     comes from a second read, ledger.UsageSlices per configured
//     gateway key, which carries PriceKnown per (provider, model)
//     group. Every cost figure is named priced_cost_usd (priced
//     spend only), and cost_status says whether it is complete:
//     "complete" (every successful row priced), "priced_only"
//     (unpriced rows exist — the figure is a lower bound),
//     "no_data" (no rows in the window), "unknown" (the split
//     could not be computed, e.g. no config wired).
//   - No fabricated rollup dimensions. Every breakdown row comes
//     from a GROUP BY over stored usage_events rows (provider /
//     (provider, model)); a dimension with no rows simply does
//     not appear.
//   - No plausible-zeros fallback. When the window holds no rows,
//     has_data is false and cost_status is "no_data" — the totals
//     are the true aggregates of zero rows (0), and the flags say
//     so, instead of zeros dressed up as measured data.
//
// LIMITATIONS (stated, not hidden):
//   - The priced split covers successful, non-shared rows only
//     (UsageSlices semantics: ok=1, shared=0). Failed rows record
//     zero tokens and zero cost, so they cannot affect spend;
//     shared (coalesced-waiter) rows likewise record zero.
//     unpriced_requests / unpriced_tokens are therefore UPPER
//     BOUNDS, not exact counts: UsageSlices groups by
//     (provider, model) and takes MIN(price_known) per group, so
//     one unpriced row taints the whole group (a mixed group
//     reports all its rows as unpriced).
//   - The split is computed over the gateway keys still present
//     in the config (price_split_scope). Rows recorded under a
//     key since deleted from the config are counted in the
//     Summary totals but are absent from the priced/unpriced
//     split — cost_status can then read "complete" while a
//     deleted key's unpriced rows exist. Deleting a key does not
//     delete its history; the split just cannot attribute it.

// analyticsWindow is one ?window= value and its ledger window.
type analyticsWindow struct {
	Name  string    `json:"window"`
	Start time.Time `json:"window_start"`
	End   time.Time `json:"window_end"`
}

// parseAnalyticsWindow maps ?window= to the ledger's calendar
// window containing now. "" defaults to "day". Anything else is a
// 400: an unknown window is a client error, not a silent fallback
// to a different period (a silent fallback would mislabel the
// data the caller thinks it is reading).
func parseAnalyticsWindow(raw string, now time.Time) (analyticsWindow, bool) {
	var interval ledger.BudgetResetInterval
	switch raw {
	case "", "day":
		interval, raw = ledger.BudgetResetDaily, "day"
	case "week":
		interval = ledger.BudgetResetWeekly
	case "month":
		interval = ledger.BudgetResetMonthly
	default:
		return analyticsWindow{}, false
	}
	w := ledger.BudgetWindowFor(interval, "00:00", now)
	return analyticsWindow{Name: raw, Start: w.PeriodStart, End: w.NextReset}, true
}

// analyticsPriceSplit is the priced/unpriced split for a window,
// built from ledger.UsageSlices over every configured gateway
// key. unpricedByCombo counts successful rows with no metered
// price per (provider, model); the global counts are the sums.
// A nil split means it could not be computed (no config wired,
// or a slice read failed — the failure is logged by the caller).
type analyticsPriceSplit struct {
	UnpricedRequests int
	UnpricedTokens   int
	// unpricedByCombo[(provider, model)] = successful rows in
	// the group whose price was unknown at record time.
	UnpricedByCombo map[[2]string]int
}

// analyticsSplitFor computes the priced split for [start, end).
// It returns nil (no error) when no config is wired: there is no
// key list to slice over, so the split is honestly unknown rather
// than zero.
func (s *Server) analyticsSplitFor(ctx context.Context, start, end time.Time) (*analyticsPriceSplit, error) {
	if s.cfg == nil {
		return nil, nil
	}
	split := &analyticsPriceSplit{UnpricedByCombo: map[[2]string]int{}}
	for _, k := range s.cfg.GatewayKeys {
		slices, err := s.ledger.UsageSlices(ctx, k.ID, start, end)
		if err != nil {
			return nil, err
		}
		for _, sl := range slices {
			if sl.PriceKnown {
				continue
			}
			split.UnpricedRequests += sl.Rows
			split.UnpricedTokens += sl.TotalTokens
			key := [2]string{sl.Provider, sl.Model}
			split.UnpricedByCombo[key] += sl.Rows
		}
	}
	return split, nil
}

// analyticsCostStatus derives cost_status from the row count and
// the priced split: "no_data" (no rows at all), "complete"
// (rows exist, all priced), "priced_only" (unpriced rows exist —
// priced_cost_usd is a lower bound), "unknown" (the split could
// not be computed).
func analyticsCostStatus(requests int, split *analyticsPriceSplit) string {
	if requests == 0 {
		return "no_data"
	}
	if split == nil {
		return "unknown"
	}
	if split.UnpricedRequests > 0 {
		return "priced_only"
	}
	return "complete"
}

// analyticsCostFields is the cost block shared by the summary and
// costs endpoints. UnpricedRequests / UnpricedTokens are nil when
// the split could not be computed — absent, never zero-as-unknown.
type analyticsCostFields struct {
	PricedCostUSD    float64 `json:"priced_cost_usd"`
	UnpricedRequests *int    `json:"unpriced_requests,omitempty"`
	UnpricedTokens   *int    `json:"unpriced_tokens,omitempty"`
	CostStatus       string  `json:"cost_status"`
	PriceSplitScope  string  `json:"price_split_scope"`
	// T6: the window's provider-REPORTED cost reconciliation.
	// ReportedCostUSD is the sum of costs the upstreams themselves
	// reported (0 when none were); ReportedCostRows counts those
	// rows. It can differ from PricedCostUSD, which is the reported
	// sum where present and the hand-computed priced sum otherwise —
	// the difference is the pricing-vs-billing reconciliation delta,
	// surfaced rather than hidden.
	ReportedCostUSD  float64 `json:"reported_cost_usd"`
	ReportedCostRows int     `json:"reported_cost_rows"`
}

// analyticsCostBlock builds the cost block for a window with
// `requests` rows, `pricedCost` priced spend, and the reported-cost
// reconciliation (T6): `reportedCost`/`reportedRows` are the exact
// sum of provider-reported costs and how many rows carried one —
// zero when nothing was reported, never dressed up as free. The
// scope string names what the split covers (or why it is
// unavailable).
func analyticsCostBlock(requests int, pricedCost float64, split *analyticsPriceSplit, reportedCost float64, reportedRows int) analyticsCostFields {
	b := analyticsCostFields{
		PricedCostUSD:    pricedCost,
		CostStatus:       analyticsCostStatus(requests, split),
		PriceSplitScope:  "gateway_keys_in_config",
		ReportedCostUSD:  reportedCost,
		ReportedCostRows: reportedRows,
	}
	if split == nil {
		b.PriceSplitScope = "unavailable_no_config"
		return b
	}
	b.UnpricedRequests = &split.UnpricedRequests
	b.UnpricedTokens = &split.UnpricedTokens
	return b
}

// analyticsNote is the human-readable honesty note attached when
// the response needs one: an empty window, a priced-only cost
// figure, or a complete cost figure whose split covers only the
// keys still in config. Empty otherwise (omitted from the JSON).
func analyticsNote(hasData bool, status string) string {
	if !hasData {
		return "no usage rows in this window"
	}
	if status == "priced_only" {
		return "priced_cost_usd covers priced rows only; unpriced rows exist, so the true cost is unknown and higher"
	}
	if status == "unknown" {
		return "the priced/unpriced split could not be computed, so priced_cost_usd may omit unpriced spend"
	}
	if status == "complete" {
		// Minimal honest fix (M8.17 nit 1): "complete" can be
		// false when rows recorded under a key since deleted
		// from the config exist — the split iterates only
		// s.cfg.GatewayKeys. The note says so; counting those
		// out-of-scope rows (a ledger out-of-scope count) is a
		// follow-up, not built here.
		return "the priced/unpriced split covers gateway keys still in config only; rows recorded under a deleted key are counted in the totals but absent from the split"
	}
	return ""
}

// joinNotes combines honesty notes; empty halves are dropped.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "; " + b
	}
}

// analyticsReconciliationNote (T6): when the window carries both
// provider-reported costs and a priced-computed total, say how far
// they sit apart — pricing tables vs upstream billing disagreeing is
// exactly the signal an operator must see, never a silent average.
// Empty when nothing was reported or the figures agree.
func analyticsReconciliationNote(priced, reported float64, reportedRows int) string {
	if reportedRows <= 0 {
		return ""
	}
	delta := priced - reported
	if delta > 0.0001 || delta < -0.0001 {
		return "pricing-vs-billing delta: priced_cost_usd " + fmt.Sprintf("%.4f", priced) +
			" vs upstream-reported " + fmt.Sprintf("%.4f", reported) + " (" + fmt.Sprintf("%.4f", delta) +
			") across " + strconv.Itoa(reportedRows) + " reported row(s)"
	}
	return ""
}

// analyticsSummaryResponse is the GET /api/analytics/summary
// payload: window totals from ledger.Summary, no breakdowns (see
// /by-provider and /by-model for those).
type analyticsSummaryResponse struct {
	analyticsWindow
	HasData                bool    `json:"has_data"`
	Requests               int     `json:"requests"`
	OK                     int     `json:"ok"`
	Errors                 int     `json:"errors"`
	PromptTokens           int     `json:"prompt_tokens"`
	CompletionTokens       int     `json:"completion_tokens"`
	TotalTokens            int     `json:"total_tokens"`
	CachedTokens           int     `json:"cached_tokens"`
	ReasoningTokens        int     `json:"reasoning_tokens"`
	CacheWriteTokens       int     `json:"cache_write_tokens"`
	CacheWriteTokens5m     int     `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h     int     `json:"cache_write_tokens_1h"`
	LocalEstimateTokens    int     `json:"local_estimate_tokens"`
	UnknownSplitTokens     int     `json:"unknown_split_tokens"`
	CompressionRequests    int     `json:"compression_requests"`
	CompressionTokensSaved int     `json:"compression_tokens_saved"`
	MeanLatencyMS          float64 `json:"mean_latency_ms"`
	analyticsCostFields
	Note string `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/summary: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics summary")
		return
	}
	split, err := s.analyticsSplitFor(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/summary: priced split failed: %v", guardrails.RedactLogLine(err.Error()))
		split = nil
	}
	cost := analyticsCostBlock(sum.Requests, sum.CostUSD, split, sum.ReportedCostUSD, sum.ReportedCostRows)
	out := analyticsSummaryResponse{
		analyticsWindow:        win,
		HasData:                sum.Requests > 0,
		Requests:               sum.Requests,
		OK:                     sum.OK,
		Errors:                 sum.Errors,
		PromptTokens:           sum.PromptTokens,
		CompletionTokens:       sum.CompletionTokens,
		TotalTokens:            sum.TotalTokens,
		CachedTokens:           sum.CachedTokens,
		ReasoningTokens:        sum.ReasoningTokens,
		CacheWriteTokens:       sum.CacheWriteTokens,
		CacheWriteTokens5m:     sum.CacheWriteTokens5m,
		CacheWriteTokens1h:     sum.CacheWriteTokens1h,
		LocalEstimateTokens:    sum.LocalEstimateTokens,
		UnknownSplitTokens:     sum.UnknownSplitTokens,
		CompressionRequests:    sum.CompressionRequests,
		CompressionTokensSaved: sum.CompressionTokensSaved,
		MeanLatencyMS:          sum.MeanLatencyMS,
		analyticsCostFields:    cost,
		Note:                   joinNotes(analyticsNote(sum.Requests > 0, cost.CostStatus), analyticsReconciliationNote(cost.PricedCostUSD, sum.ReportedCostUSD, sum.ReportedCostRows)),
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsProviderRow is one provider's window roll-up: the
// ledger.ProviderUsage fields, with cost renamed to priced cost
// and the group's unpriced-request count alongside (a group whose
// rows were all priced reports 0 here — genuinely priced, not
// "unknown", because the split read says so).
type analyticsProviderRow struct {
	Provider              string  `json:"provider"`
	Requests              int     `json:"requests"`
	OK                    int     `json:"ok"`
	Errors                int     `json:"errors"`
	PromptTokens          int     `json:"prompt_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	CachedTokens          int     `json:"cached_tokens"`
	ReasoningTokens       int     `json:"reasoning_tokens"`
	CacheWriteTokens      int     `json:"cache_write_tokens"`
	CacheWriteTokens5m    int     `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h    int     `json:"cache_write_tokens_1h"`
	LocalEstimateTokens   int     `json:"local_estimate_tokens"`
	PricedCostUSD         float64 `json:"priced_cost_usd"`
	UnpricedRequests      int     `json:"unpriced_requests"`
	UnpricedRequestsKnown bool    `json:"unpriced_requests_known"`
}

type analyticsByProviderResponse struct {
	analyticsWindow
	HasData   bool                   `json:"has_data"`
	Providers []analyticsProviderRow `json:"providers"`
	Note      string                 `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsByProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/by-provider: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics by-provider")
		return
	}
	split, err := s.analyticsSplitFor(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/by-provider: priced split failed: %v", guardrails.RedactLogLine(err.Error()))
		split = nil
	}
	rows := []analyticsProviderRow{}
	for _, p := range sum.ByProvider {
		row := analyticsProviderRow{
			Provider: p.Provider, Requests: p.Requests, OK: p.OK, Errors: p.Errors,
			PromptTokens: p.PromptTokens, CompletionTokens: p.CompletionTokens,
			TotalTokens: p.TotalTokens, CachedTokens: p.CachedTokens,
			ReasoningTokens: p.ReasoningTokens, CacheWriteTokens: p.CacheWriteTokens,
			CacheWriteTokens5m: p.CacheWriteTokens5m, CacheWriteTokens1h: p.CacheWriteTokens1h,
			LocalEstimateTokens: p.LocalEstimateTokens, PricedCostUSD: p.CostUSD,
		}
		if split != nil {
			row.UnpricedRequestsKnown = true
			for key, n := range split.UnpricedByCombo {
				if key[0] == p.Provider {
					row.UnpricedRequests += n
				}
			}
		}
		rows = append(rows, row)
	}
	out := analyticsByProviderResponse{
		analyticsWindow: win, HasData: sum.Requests > 0, Providers: rows,
	}
	if !out.HasData {
		out.Note = "no usage rows in this window"
	} else if split == nil {
		out.Note = "the priced/unpriced split could not be computed, so unpriced_requests is unknown per provider"
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsModelRow is one (provider, model) combo's window
// roll-up: the ledger.ModelUsage fields, with cost renamed to
// priced cost and the combo's unpriced-request count alongside.
type analyticsModelRow struct {
	Model                 string  `json:"model"`
	Provider              string  `json:"provider"`
	Requests              int     `json:"requests"`
	PromptTokens          int     `json:"prompt_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	CachedTokens          int     `json:"cached_tokens"`
	ReasoningTokens       int     `json:"reasoning_tokens"`
	CacheWriteTokens      int     `json:"cache_write_tokens"`
	CacheWriteTokens5m    int     `json:"cache_write_tokens_5m"`
	CacheWriteTokens1h    int     `json:"cache_write_tokens_1h"`
	LocalEstimateTokens   int     `json:"local_estimate_tokens"`
	PricedCostUSD         float64 `json:"priced_cost_usd"`
	UnpricedRequests      int     `json:"unpriced_requests"`
	UnpricedRequestsKnown bool    `json:"unpriced_requests_known"`
}

type analyticsByModelResponse struct {
	analyticsWindow
	HasData bool                `json:"has_data"`
	Models  []analyticsModelRow `json:"models"`
	Note    string              `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsByModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/by-model: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics by-model")
		return
	}
	split, err := s.analyticsSplitFor(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/by-model: priced split failed: %v", guardrails.RedactLogLine(err.Error()))
		split = nil
	}
	rows := []analyticsModelRow{}
	for _, m := range sum.ByModel {
		row := analyticsModelRow{
			Model: m.Model, Provider: m.Provider, Requests: m.Requests,
			PromptTokens: m.PromptTokens, CompletionTokens: m.CompletionTokens,
			TotalTokens: m.TotalTokens, CachedTokens: m.CachedTokens,
			ReasoningTokens: m.ReasoningTokens, CacheWriteTokens: m.CacheWriteTokens,
			CacheWriteTokens5m: m.CacheWriteTokens5m, CacheWriteTokens1h: m.CacheWriteTokens1h,
			LocalEstimateTokens: m.LocalEstimateTokens, PricedCostUSD: m.CostUSD,
		}
		if split != nil {
			row.UnpricedRequestsKnown = true
			row.UnpricedRequests = split.UnpricedByCombo[[2]string{m.Provider, m.Model}]
		}
		rows = append(rows, row)
	}
	out := analyticsByModelResponse{
		analyticsWindow: win, HasData: sum.Requests > 0, Models: rows,
	}
	if !out.HasData {
		out.Note = "no usage rows in this window"
	} else if split == nil {
		out.Note = "the priced/unpriced split could not be computed, so unpriced_requests is unknown per model"
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsCostsResponse is the GET /api/analytics/costs payload:
// the window's priced spend plus the unpriced remainder, so the
// caller can tell a complete total from a lower bound. There is
// deliberately no "total cost" field: when unpriced rows exist
// the true total is unknown, and presenting priced_cost_usd as a
// total would be the $0-as-unknown failure. ByKey is the
// per-gateway-key roll-up (the Cost Explorer key dimension):
// one row per key with ledger rows in the window, from
// ledger.Summary.ByKey — priced spend per key plus that key's
// exact unpriced-request count (see ledger.KeyUsage for the
// fail-closed policy). A key with no rows in the window is
// absent from ByKey, never zero-filled.
type analyticsCostsResponse struct {
	analyticsWindow
	HasData  bool              `json:"has_data"`
	Requests int               `json:"requests"`
	ByKey    []analyticsKeyRow `json:"by_key"`
	analyticsCostFields
	Note string `json:"note,omitempty"`
}

// analyticsKeyRow is one gateway key's window roll-up: the
// ledger.KeyUsage fields, with cost renamed to priced cost and
// the key's unpriced-request count alongside. UnpricedRequests
// is exact per row (successful, non-shared rows with no metered
// price — see ledger.KeyUsage), so UnpricedRequestsKnown is
// always true: the count comes from the ledger's stored
// per-row price_known flag, computed in Summary itself, and
// needs neither the config key list nor the global priced split.
type analyticsKeyRow struct {
	GatewayKeyID          string  `json:"gateway_key_id"`
	Requests              int     `json:"requests"`
	OK                    int     `json:"ok"`
	PromptTokens          int     `json:"prompt_tokens"`
	CompletionTokens      int     `json:"completion_tokens"`
	TotalTokens           int     `json:"total_tokens"`
	PricedCostUSD         float64 `json:"priced_cost_usd"`
	UnpricedRequests      int     `json:"unpriced_requests"`
	UnpricedRequestsKnown bool    `json:"unpriced_requests_known"`
}

func (s *Server) handleAnalyticsCosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/costs: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics costs")
		return
	}
	split, err := s.analyticsSplitFor(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/costs: priced split failed: %v", guardrails.RedactLogLine(err.Error()))
		split = nil
	}
	cost := analyticsCostBlock(sum.Requests, sum.CostUSD, split, sum.ReportedCostUSD, sum.ReportedCostRows)
	// Per-key roll-up, straight from the ledger summary: one
	// row per key with rows in the window, in the summary's
	// order. Always a non-nil slice, so an empty window reads
	// "by_key":[] — an honest empty state, never null.
	keys := []analyticsKeyRow{}
	for _, k := range sum.ByKey {
		keys = append(keys, analyticsKeyRow{
			GatewayKeyID: k.GatewayKeyID, Requests: k.Requests, OK: k.OK,
			PromptTokens: k.PromptTokens, CompletionTokens: k.CompletionTokens,
			TotalTokens: k.TotalTokens, PricedCostUSD: k.CostUSD,
			UnpricedRequests: k.UnpricedRequests, UnpricedRequestsKnown: true,
		})
	}
	out := analyticsCostsResponse{
		analyticsWindow:     win,
		HasData:             sum.Requests > 0,
		Requests:            sum.Requests,
		ByKey:               keys,
		analyticsCostFields: cost,
		Note:                joinNotes(analyticsNote(sum.Requests > 0, cost.CostStatus), analyticsReconciliationNote(cost.PricedCostUSD, sum.ReportedCostUSD, sum.ReportedCostRows)),
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Stage 1 (Area 11 analytics endpoints): compression, diversity and
// auto-routing counterparts to the summary / by-provider / by-model /
// costs suite above. Same conventions: read-only over stored ledger
// rows, windowed by ?window=day|week|month, honest empty states.
//
// PROPOSED REGISTRATION (coordinator owns guard.go / server.go —
// these handlers deliberately do no auth themselves, exactly like
// the four above): Tier 3 (MANAGEMENT) + scope ["admin"] on all
// three routes. Rationale: they aggregate traffic/spend across ALL
// gateway keys, so they are operator-only; they are read-only, so
// Tier 2 (reserved for destructive routes) does not apply.
// ---------------------------------------------------------------------------

// analyticsCompressionRow is one provider's compression totals in a
// window, straight from ledger.CompressionByProvider.
type analyticsCompressionRow struct {
	Provider           string  `json:"provider"`
	Requests           int     `json:"requests"`
	PromptBeforeTokens int     `json:"prompt_before_tokens"`
	TokensSaved        int     `json:"tokens_saved"`
	SavingRate         float64 `json:"saving_rate"`
}

// analyticsCompressionResponse is the GET /api/analytics/compression
// payload: the window's compression totals plus the per-provider
// breakdown. A provider with no compressed request in the window
// is absent from ByProvider, never zero-filled. SavingRate is
// saved / prompt-before over the window (0 when nothing was
// compressed) — the share of the prompt compression removed,
// measured from the local estimates stored on each usage row.
type analyticsCompressionResponse struct {
	analyticsWindow
	HasData                 bool                      `json:"has_data"`
	RequestsWithCompression int                       `json:"requests_with_compression"`
	PromptBeforeTokens      int                       `json:"prompt_before_tokens"`
	TokensSaved             int                       `json:"tokens_saved"`
	SavingRate              float64                   `json:"saving_rate"`
	ByProvider              []analyticsCompressionRow `json:"by_provider"`
	Note                    string                    `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsCompression(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	rows, err := s.ledger.CompressionByProvider(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/compression: ledger query failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics compression")
		return
	}
	out := analyticsCompressionResponse{
		analyticsWindow: win,
		HasData:         len(rows) > 0,
		ByProvider:      []analyticsCompressionRow{},
	}
	for _, row := range rows {
		out.RequestsWithCompression += row.Requests
		out.PromptBeforeTokens += row.PromptBeforeTokens
		out.TokensSaved += row.TokensSaved
		out.ByProvider = append(out.ByProvider, analyticsCompressionRow{
			Provider: row.Provider, Requests: row.Requests,
			PromptBeforeTokens: row.PromptBeforeTokens, TokensSaved: row.TokensSaved,
			SavingRate: row.SavingRate,
		})
	}
	if out.PromptBeforeTokens > 0 {
		out.SavingRate = float64(out.TokensSaved) / float64(out.PromptBeforeTokens)
	}
	if !out.HasData {
		out.Note = "no compressed requests in this window"
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsDiversityResponse is the GET /api/analytics/diversity
// payload: how this window's traffic spread across providers and
// models. It is TRAFFIC diversity, derived from the stored usage
// rows in the window — not a live-provider-state report. HHI is
// the Herfindahl concentration index over providers (sum of
// squared request shares): 1.0 means one provider served
// everything, 1/N means N providers served equally. A window with
// no rows reads as zeros with has_data false, never as an
// invented spread.
type analyticsDiversityResponse struct {
	analyticsWindow
	HasData          bool    `json:"has_data"`
	Requests         int     `json:"requests"`
	Providers        int     `json:"providers"`
	Models           int     `json:"models"`
	TopProvider      string  `json:"top_provider,omitempty"`
	TopProviderShare float64 `json:"top_provider_share"`
	TopModel         string  `json:"top_model,omitempty"`
	TopModelShare    float64 `json:"top_model_share"`
	HHI              float64 `json:"hhi"`
	Note             string  `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsDiversity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/diversity: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics diversity")
		return
	}
	out := analyticsDiversityResponse{
		analyticsWindow: win,
		HasData:         sum.Requests > 0,
		Requests:        sum.Requests,
		Providers:       len(sum.ByProvider),
		Models:          len(sum.ByModel),
	}
	if out.HasData {
		for _, p := range sum.ByProvider {
			share := float64(p.Requests) / float64(sum.Requests)
			out.HHI += share * share
			if share > out.TopProviderShare {
				out.TopProviderShare = share
				out.TopProvider = p.Provider
			}
		}
		for _, m := range sum.ByModel {
			share := float64(m.Requests) / float64(sum.Requests)
			if share > out.TopModelShare {
				out.TopModelShare = share
				out.TopModel = m.Model
			}
		}
	} else {
		out.Note = "no usage rows in this window"
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsAutoRoutingProviderRow is one provider's share of
// auto-routed requests in a window.
type analyticsAutoRoutingProviderRow struct {
	Provider string `json:"provider"`
	Requests int    `json:"requests"`
}

// analyticsAutoRoutingResponse is the GET /api/analytics/auto-routing
// payload: the requests the auto dispatcher served in this window,
// grouped by variant, plus the providers that served them. Rows
// with no variant (explicit-model requests, or rows recorded
// before the variant was captured) are excluded — the breakdown is
// auto-routed traffic only. VariantBreakdown is never null: an
// empty window reads as an empty map.
type analyticsAutoRoutingResponse struct {
	analyticsWindow
	HasData          bool                              `json:"has_data"`
	TotalRequests    int                               `json:"total_requests"`
	VariantBreakdown map[string]int                    `json:"variant_breakdown"`
	TopProviders     []analyticsAutoRoutingProviderRow `json:"top_providers"`
	Note             string                            `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsAutoRouting(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	breakdown, err := s.ledger.AutoRoutingBreakdown(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/auto-routing: ledger query failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics auto-routing")
		return
	}
	out := analyticsAutoRoutingResponse{
		analyticsWindow:  win,
		HasData:          breakdown.TotalRequests > 0,
		TotalRequests:    breakdown.TotalRequests,
		VariantBreakdown: breakdown.Variants,
		TopProviders:     []analyticsAutoRoutingProviderRow{},
	}
	for _, p := range breakdown.TopProviders {
		out.TopProviders = append(out.TopProviders, analyticsAutoRoutingProviderRow{
			Provider: p.Provider, Requests: p.Requests,
		})
	}
	if !out.HasData {
		out.Note = "no auto-routed rows recorded in this window — the variant is captured only once the router wires the auto dispatch into the recorded usage"
	}
	writeJSON(w, http.StatusOK, out)
}

// analyticsComboHealthRow is one combo's health in a window: the
// live view (step liveness derived from the provider config at
// read time) plus how many usage rows in the window match it.
// MatchingRequests is an UPPER BOUND, not an exact count: a usage
// row counts when its (provider, model) matches any step of the
// combo, and a row served through a combo is indistinguishable
// in the ledger from one served directly. A step with no model
// matches any model of its provider. Per-combo dedup: a row is
// counted once even when two steps of the same combo match it
// (the (provider, model) groups from Summary.ByModel are already
// unique, and the step pairs are held in a set).
type analyticsComboHealthRow struct {
	Combo            Combo `json:"combo"`
	MatchingRequests int   `json:"matching_requests"`
}

// analyticsComboHealthResponse is the GET /api/analytics/combo-health
// payload: per-combo health for one calendar window.
//
// SCOPE (stated, not hidden): liveness + matching requests only.
// Quota risk, forecast, Gini skew, autopilot and scoring-inspector
// have no data source in this tree and are not reported — nothing
// is estimated or invented for them.
type analyticsComboHealthResponse struct {
	analyticsWindow
	HasData  bool                      `json:"has_data"`
	Requests int                       `json:"requests"`
	Combos   []analyticsComboHealthRow `json:"combos"`
	Note     string                    `json:"note,omitempty"`
}

func (s *Server) handleAnalyticsComboHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "analytics unavailable: no ledger wired")
		return
	}
	win, ok := parseAnalyticsWindow(r.URL.Query().Get("window"), time.Now())
	if !ok {
		writeError(w, http.StatusBadRequest, "unknown window (want day, week or month)")
		return
	}
	// Step liveness comes from ComboStore.view under the store
	// lock (same pattern as handleListCombos): every liveness
	// fact is derived live from the provider config.
	store := s.combosStore()
	store.mu.RLock()
	names := make([]string, 0, len(store.records))
	for name := range store.records {
		names = append(names, name)
	}
	sort.Strings(names)
	views := make([]Combo, 0, len(names))
	for _, name := range names {
		views = append(views, store.view(store.records[name]))
	}
	store.mu.RUnlock()
	sum, err := s.ledger.Summary(r.Context(), win.Start, win.End)
	if err != nil {
		s.logger.Printf("analytics/combo-health: ledger summary failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not build analytics combo-health")
		return
	}
	out := analyticsComboHealthResponse{
		analyticsWindow: win,
		HasData:         len(views) > 0 && sum.Requests > 0,
		Requests:        sum.Requests,
		Combos:          []analyticsComboHealthRow{},
	}
	upperBound := "matching_requests is an upper bound: a usage row counts for a combo when its provider+model matches any step; a row served through a combo is indistinguishable from one served directly. A step with no model matches any model of its provider."
	switch {
	case len(views) == 0:
		out.Note = "no combos configured"
	case sum.Requests == 0:
		out.Note = "no usage rows in this window. " + upperBound
	default:
		out.Note = upperBound
	}
	for _, view := range views {
		// Step pairs as a set: a row matching two steps of the
		// same combo still counts once.
		pairs := make(map[[2]string]bool, len(view.Chain))
		providers := make(map[string]bool)
		for _, step := range view.Chain {
			if step.Model == "" {
				providers[step.Provider] = true
			} else {
				pairs[[2]string{step.Provider, step.Model}] = true
			}
		}
		row := analyticsComboHealthRow{Combo: view}
		for _, m := range sum.ByModel {
			if pairs[[2]string{m.Provider, m.Model}] || providers[m.Provider] {
				row.MatchingRequests += m.Requests
			}
		}
		out.Combos = append(out.Combos, row)
	}
	writeJSON(w, http.StatusOK, out)
}
