// M8.11: me/quotas v1 surface — the missing halves of the
// self-service API (Area 14 §14a + Area 11 row 724).
//
//   - GET /v1/me/sessions/{id} — one session's detail (the list
//     exists since M5.6 in mesessions.go; this is the detail
//     endpoint the 14a me/sessions row says does not exist).
//     A session id is a session ROOT id; a turn id resolves to
//     its session by walking PreviousID up to the root, so a
//     caller holding any turn can still fetch its session.
//     Unknown ids, other keys' sessions, and keyless (pre-M5.6)
//     records all answer 404 — never 403, so existence of
//     another key's session is never leaked.
//
//   - POST /v1/quotas/check — a pre-flight verdict: would a
//     request for (provider, model, estimated tokens) fit the
//     caller's quota/budget? The verdict is produced by the
//     EXISTING machinery — ledger.CheckKeyBudget (5h cap, active
//     calendar window, unpriced fail_closed policy) and
//     ledger.CheckKeyTokenLimits — plus the same machinery's
//     projection for the estimate itself (ReserveSpend semantics:
//     spent + estimated cost against the cap). It does NOT consult
//     the legacy cumulative per-key cap: live requests are also
//     checked against that cap at reservation time
//     (internal/ledger/reservation.go:75-88, the cumulative check
//     under reserveMu), so a check that passes here can still be
//     refused at send time by a cumulative cap the estimate would
//     breach — the endpoint is a pre-flight against the rolling
//     budget and token limits only, never a guarantee. Adding the
//     cumulative-cap consultation is a follow-up, not this fix.
//
//   - Per-window TOTAL REQUEST COUNTS in me/status (Area 11 row
//     724) — totalRequestsInSlices below is the single
//     definition: the sum of UsageSlices Rows for the window,
//     i.e. successful non-shared ledger rows (ok=1, shared=0),
//     the same population the window's SpentUSD/Tokens already
//     count. The field is live in internal/server/keystatus.go
//     (meStatusUsageWindow.TotalRequests, keystatus.go:102,
//     accumulated as w.TotalRequests += sl.Rows in usageWindow's
//     slices loop, :224 — the same sum, inlined so the window
//     is still a single pass over the slices). The fixture
//     tests/fixtures/v1/m8.11/me-status-total-requests-200.json
//     asserts the field — see TestMeStatusTotalRequestsFixture.
//
// REGISTRATION (coordinator — guard.go + server.go are not this
// slice's files). Proposed guard-table rows, TierManagement,
// gateway-key auth, scope "chat" for both (the session detail
// mirrors GET /v1/responses/{id}, scope chat; the quota check
// pre-flights a chat-model request, so a chat-scoped key must be
// able to call it — a models-only scope would lock out the very
// keys that send the requests being checked):
//
//	{Method: "GET", Path: "/v1/me/sessions/{id}", Tier: TierManagement,
//	 Description: "One agent (Responses) session of the calling API key: the session root's turns, cumulative tokens and list-price cost, plus the per-turn detail; unknown ids, other keys' sessions and keyless records answer 404. Gateway-key auth required.", Scopes: []string{"chat"}},
//	{Method: "POST", Path: "/v1/quotas/check", Tier: TierManagement,
//	 Description: "Pre-flight quota/budget check for one (provider, model, estimated tokens) request: allowed plus the budget, token-limit and price numbers the verdict was judged against, from the same ledger machinery that enforces live requests. Gateway-key auth required.", Scopes: []string{"chat"}},
//
// and in server.go's register block:
//
//	s.register(mux, "GET", "/v1/me/sessions/{id}", s.handleMeSessionDetail)
//	s.register(mux, "POST", "/v1/quotas/check", s.handleQuotasCheck)
//
// No invented prices/models/caps/data anywhere here: unknown
// prices stay unknown (estimated cost nil, price_known false),
// never $0; no cap is invented — every limit reported is a
// configured budget/token-limit row or nil when unlimited.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/responses"
)

const (
	// meSessionDetailMaxTurns bounds the per-turn detail list: a
	// session's totals (turns/tokens/cost) always cover the whole
	// tree, but the detail array is capped and flagged, so one
	// giant session cannot blow up the response.
	meSessionDetailMaxTurns = 1000
	// meSessionRootWalkMax bounds the PreviousID walk from a turn
	// id up to its session root (belt-and-braces against a corrupt
	// cycle; real chains are far shorter).
	meSessionRootWalkMax = 10000
)

// ---------------------------------------------------------------------------
// GET /v1/me/sessions/{id}.
// ---------------------------------------------------------------------------

// meSessionTurn is one turn of a session detail: the response
// record's own tokens and its list price at the catalog's current
// prices (nil when the turn's model has no catalog price —
// unknown, never $0).
type meSessionTurn struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	Model        string    `json:"model"`
	Provider     string    `json:"provider,omitempty"`
	Status       string    `json:"status"`
	InputTokens  int       `json:"input_tokens"`
	OutputTokens int       `json:"output_tokens"`
	ListPriceUSD *float64  `json:"list_price_usd,omitempty"`
}

// meSessionDetailResponse is the GET /v1/me/sessions/{id}
// payload: the list row (meSessionRow, reused verbatim so list
// and detail can never disagree about a session's totals) plus
// the per-turn detail, oldest first.
type meSessionDetailResponse struct {
	meSessionRow
	Object               string          `json:"object"`
	TurnsDetail          []meSessionTurn `json:"turns_detail"`
	TurnsDetailTruncated bool            `json:"turns_detail_truncated"`
}

// meSessionRoot walks a record id up to its session root (the
// record with no PreviousID), following PreviousID links. ok is
// false when the id is unknown, the chain breaks, or the walk
// exceeds the cap / hits a cycle — all honest 404s, never a
// partial session.
func (s *Server) meSessionRoot(id string) (responses.Record, bool) {
	if s.responses == nil {
		return responses.Record{}, false
	}
	seen := map[string]bool{}
	cur := id
	for i := 0; i < meSessionRootWalkMax; i++ {
		if seen[cur] {
			return responses.Record{}, false
		}
		seen[cur] = true
		rec, ok := s.responses.Get(cur)
		if !ok {
			return responses.Record{}, false
		}
		if rec.PreviousID == "" {
			return rec, true
		}
		cur = rec.PreviousID
	}
	return responses.Record{}, false
}

// meSessionTurns collects every turn of the session rooted at
// root (the whole tree, branches included — the same walk
// meSessionRow aggregates), oldest first, each priced at the
// catalog's current list prices. truncated is true when the
// detail list was capped at meSessionDetailMaxTurns; the totals
// in meSessionRow are never capped.
func (s *Server) meSessionTurns(root responses.Record, prices map[string][2]float64) ([]meSessionTurn, bool) {
	seen := map[string]bool{}
	stack := []string{root.ID}
	var turns []meSessionTurn
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		rec, ok := s.responses.Get(id)
		if !ok {
			continue
		}
		t := meSessionTurn{
			ID:           rec.ID,
			CreatedAt:    time.Unix(rec.CreatedAt, 0).UTC(),
			Model:        rec.Model,
			Provider:     rec.ProviderID,
			Status:       rec.Status,
			InputTokens:  rec.TurnInputTokens,
			OutputTokens: rec.TurnOutputTokens,
		}
		if p, ok := prices[rec.Model]; ok {
			v := (float64(rec.TurnInputTokens)*p[0] + float64(rec.TurnOutputTokens)*p[1]) / 1e6
			t.ListPriceUSD = &v
		}
		turns = append(turns, t)
		stack = append(stack, s.responses.Children(id)...)
	}
	sort.Slice(turns, func(a, b int) bool {
		if !turns[a].CreatedAt.Equal(turns[b].CreatedAt) {
			return turns[a].CreatedAt.Before(turns[b].CreatedAt)
		}
		return turns[a].ID < turns[b].ID
	})
	if len(turns) > meSessionDetailMaxTurns {
		return turns[:meSessionDetailMaxTurns], true
	}
	return turns, false
}

func (s *Server) handleMeSessionDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		writeError(w, http.StatusUnauthorized, "a gateway key is required to identify the caller")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id is required")
		return
	}
	// Ledger agent sessions (AK-3): an "as_" id is a rollup
	// row, resolved from the ledger — AgentSessions is already
	// key-scoped, so another key's row can never match. A ledger
	// row has no per-turn records, so the detail is the list row
	// with an empty turns_detail. A miss (or a ledger read
	// failure) falls through to the Responses walk, which 404s
	// as today — never a partial session.
	if s.ledger != nil && strings.HasPrefix(id, "as_") {
		if sessions, err := s.ledger.AgentSessions(r.Context(), keyID, 1000); err == nil {
			for _, sess := range sessions {
				if sess.ID == id {
					writeJSON(w, http.StatusOK, meSessionDetailResponse{
						meSessionRow:         meSessionRowFromLedger(sess),
						Object:               "session",
						TurnsDetail:          []meSessionTurn{},
						TurnsDetailTruncated: false,
					})
					return
				}
			}
		}
	}
	root, ok := s.meSessionRoot(id)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown session "+id)
		return
	}
	// Ownership is checked on the ROOT, so a turn id belonging
	// to another key's session is a 404 too — the caller learns
	// nothing about a session that is not theirs. Keyless
	// (pre-M5.6) sessions belong to nobody, exactly like the
	// list's rule.
	if root.KeyID == "" || root.KeyID != keyID {
		writeError(w, http.StatusNotFound, "unknown session "+id)
		return
	}
	prices := s.catalogListPrices(r.Context())
	row := s.meSessionRow(root, prices)
	turns, truncated := s.meSessionTurns(root, prices)
	out := meSessionDetailResponse{
		meSessionRow:         row,
		Object:               "session",
		TurnsDetail:          turns,
		TurnsDetailTruncated: truncated,
	}
	if out.TurnsDetail == nil {
		out.TurnsDetail = []meSessionTurn{}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /v1/quotas/check.
// ---------------------------------------------------------------------------

// quotaCheckRequest is one POST /v1/quotas/check body. Provider is
// optional ("" = unknown — provider-scoped token limits then do
// not apply, and pricing falls back to the model-level price).
// Model is required. EstimatedTokens is the total estimated
// token count; EstimatedPromptTokens / EstimatedCompletionTokens
// are the optional split — when all three are present the split
// must sum to the total, or the request is a 400 (a silently
// ignored contradiction would price the wrong thing).
type quotaCheckRequest struct {
	Provider                  string `json:"provider,omitempty"`
	Model                     string `json:"model"`
	EstimatedTokens           *int   `json:"estimated_tokens,omitempty"`
	EstimatedPromptTokens     *int   `json:"estimated_prompt_tokens,omitempty"`
	EstimatedCompletionTokens *int   `json:"estimated_completion_tokens,omitempty"`
}

// quotaCheckBudget is the budget half of a quota-check verdict:
// the configured cap (nil = unlimited — no budget row), the live
// spend the gate counts (settled + reserved, from
// KeyBudgetStatus), what is left, the window the gate enforces,
// and the projected spend once this request's estimated cost is
// added (the ReserveSpend projection).
type quotaCheckBudget struct {
	LimitUSD       *float64 `json:"limit_usd,omitempty"`
	SpentUSD       float64  `json:"spent_usd"`
	RemainingUSD   *float64 `json:"remaining_usd,omitempty"`
	Window         string   `json:"window"`
	AtLimit        bool     `json:"at_limit"`
	ProjectedSpent *float64 `json:"projected_spent_usd,omitempty"`
	HasBudget      bool     `json:"has_budget"`
	UnpricedPolicy string   `json:"unpriced_policy,omitempty"`
}

// quotaCheckTokenLimit is one applicable per-key token limit in a
// quota-check verdict: the configured limit, the window's used
// tokens (from the ledger, same population the gate counts),
// what is left, whether this request's estimate would push the
// window over the limit, and when the window resets. Used is
// computed from UsageSlices — see quotaCheckTokenUsed — never
// estimated.
type quotaCheckTokenLimit struct {
	Scope       string    `json:"scope"`
	ScopeValue  string    `json:"scope_value,omitempty"`
	Limit       int       `json:"limit"`
	Used        int       `json:"used"`
	Remaining   int       `json:"remaining"`
	WouldExceed bool      `json:"would_exceed"`
	WindowStart time.Time `json:"window_start"`
	NextReset   time.Time `json:"next_reset_at"`
}

// quotaCheckResponse is the POST /v1/quotas/check payload. The
// verdict is always HTTP 200 for a check that ran: allowed=false
// carries the reason (budget_exceeded, unpriced_model,
// token_limit_exceeded), never an error status — a refusal the
// machinery would make is a verdict, not a failure of the check
// itself. EstimatedCostUSD is nil when the model has no known
// price (unknown, never $0); EstimatedCostBasis names how the
// estimate was priced ("split" when a prompt/completion split
// was given, "input_rate_only" when only a total was given —
// the input rate then prices the whole total, a lower bound
// whenever output costs more than input).
type quotaCheckResponse struct {
	Object             string                 `json:"object"`
	Allowed            bool                   `json:"allowed"`
	Reason             string                 `json:"reason"`
	Provider           string                 `json:"provider,omitempty"`
	Model              string                 `json:"model"`
	EstimatedTokens    int                    `json:"estimated_tokens"`
	EstimatedCostUSD   *float64               `json:"estimated_cost_usd,omitempty"`
	EstimatedCostBasis string                 `json:"estimated_cost_basis,omitempty"`
	PriceKnown         bool                   `json:"price_known"`
	Budget             quotaCheckBudget       `json:"budget"`
	TokenLimits        []quotaCheckTokenLimit `json:"token_limits"`
	UnpricedModels     []string               `json:"unpriced_models"`
	Detail             string                 `json:"detail,omitempty"`
}

// quotaCheckTokenUsed sums one key's used tokens in a window from
// UsageSlices, filtered by a token limit's scope. UsageSlices
// counts successful non-shared rows (ok=1, shared=0); the gate's
// own counter (tokenWindowUsed) counts ok=1 rows without the
// shared filter — the two agree because shared (coalesced
// waiter) rows always carry zero tokens, so they contribute
// nothing to either sum. The verdict itself never depends on
// this helper: CheckKeyTokenLimits is the gate, this is only the
// reporting/projection number shown beside it.
func (s *Server) quotaCheckTokenUsed(ctx context.Context, keyID string, lim ledger.TokenLimitConfig, window ledger.BudgetWindow) (int, error) {
	slices, err := s.ledger.UsageSlices(ctx, keyID, window.PeriodStart, window.NextReset)
	if err != nil {
		return 0, err
	}
	used := 0
	for _, sl := range slices {
		switch lim.Scope {
		case "provider":
			if sl.Provider == lim.ScopeValue {
				used += sl.TotalTokens
			}
		case "model":
			if sl.Model == lim.ScopeValue {
				used += sl.TotalTokens
			}
		default: // global
			used += sl.TotalTokens
		}
	}
	return used, nil
}

// quotaCheckTokenLimitConfigs converts one key's configured token
// limits into the ledger's shape (the same conversion
// checkKeyTokenLimits uses — a limit configured here and a limit
// enforced live can never disagree about scope, value, or
// enabled-ness).
func quotaCheckTokenLimitConfigs(key config.GatewayKey) []ledger.TokenLimitConfig {
	limits := make([]ledger.TokenLimitConfig, 0, len(key.TokenLimits))
	for _, tl := range key.TokenLimits {
		limits = append(limits, ledger.TokenLimitConfig{
			Scope:         tl.Scope,
			ScopeValue:    tl.ScopeValue,
			Limit:         tl.Limit,
			ResetInterval: tl.ResetInterval,
			ResetTime:     tl.ResetTime,
			Enabled:       tl.IsEnabled(),
		})
	}
	return limits
}

func (s *Server) handleQuotasCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		writeError(w, http.StatusUnauthorized, "a gateway key is required to identify the caller")
		return
	}
	// A quota check with no ledger is fail-closed with a 503:
	// the budget state is unknown, and a billing guard must not
	// wave a verdict through blind (same rule as
	// checkKeyBudgetForModel).
	if s.ledger == nil {
		writeServerError(w, http.StatusServiceUnavailable, "quota check unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req quotaCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid quota check: expected {\"provider\": string (optional), \"model\": string, \"estimated_tokens\": number (optional), \"estimated_prompt_tokens\": number (optional), \"estimated_completion_tokens\": number (optional)}")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Model = strings.TrimSpace(req.Model)
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	estTotal, estPrompt, estCompletion := 0, 0, 0
	hasSplit := req.EstimatedPromptTokens != nil || req.EstimatedCompletionTokens != nil
	if req.EstimatedPromptTokens != nil {
		estPrompt = *req.EstimatedPromptTokens
	}
	if req.EstimatedCompletionTokens != nil {
		estCompletion = *req.EstimatedCompletionTokens
	}
	if req.EstimatedTokens != nil {
		estTotal = *req.EstimatedTokens
	} else if hasSplit {
		estTotal = estPrompt + estCompletion
	}
	if estTotal < 0 || estPrompt < 0 || estCompletion < 0 {
		writeError(w, http.StatusBadRequest, "estimated token counts must not be negative")
		return
	}
	if hasSplit && req.EstimatedTokens != nil && estPrompt+estCompletion != estTotal {
		writeError(w, http.StatusBadRequest, "estimated_prompt_tokens + estimated_completion_tokens must equal estimated_tokens")
		return
	}

	now := time.Now()
	ctx := r.Context()
	out := quotaCheckResponse{
		Object:          "quota_check",
		Allowed:         true,
		Reason:          "ok",
		Provider:        req.Provider,
		Model:           req.Model,
		EstimatedTokens: estTotal,
		TokenLimits:     []quotaCheckTokenLimit{},
		UnpricedModels:  []string{},
	}

	// 1. The live budget gate, verbatim: the 5h cap, the active
	// calendar window, and the unpriced fail_closed policy.
	// A refusal here is the verdict — never overridden below.
	err := s.ledger.CheckKeyBudget(ctx, keyID, []string{req.Model}, s.modelPriced, now)
	if err != nil {
		var be *ledger.BudgetExceededError
		var ue *ledger.UnpricedUsageError
		switch {
		case errors.As(err, &be):
			out.Allowed = false
			out.Reason = "budget_exceeded"
			out.Detail = be.Error()
		case errors.As(err, &ue):
			out.Allowed = false
			out.Reason = "unpriced_model"
			out.Detail = ue.Error()
			out.UnpricedModels = append([]string(nil), ue.Models...)
		default:
			s.logger.Printf("quotas/check: budget check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusServiceUnavailable, "quota check unavailable: budget check failed")
			return
		}
	}

	// The estimate's cost, priced at the operator's price book
	// (pair, then model-level, then synced — pairPrice's order).
	// Unknown price: cost stays nil, never $0.
	price, _ := s.pairPrice(req.Provider, req.Model)
	priceKnown := price.Known() && (price.InputUSDPer1M != nil || price.OutputUSDPer1M != nil)
	out.PriceKnown = priceKnown
	var estCost *float64
	if priceKnown {
		var cost float64
		if hasSplit {
			cost = (float64(estPrompt)*priceOrZero(price.InputUSDPer1M) + float64(estCompletion)*priceOrZero(price.OutputUSDPer1M)) / 1e6
			out.EstimatedCostBasis = "split"
		} else {
			cost = float64(estTotal) * priceOrZero(price.InputUSDPer1M) / 1e6
			out.EstimatedCostBasis = "input_rate_only"
		}
		estCost = &cost
		out.EstimatedCostUSD = estCost
	}

	// 2. The live budget position, for the numbers the verdict
	// was judged against — and the estimate's projection
	// (ReserveSpend semantics: spent + estimated cost against
	// the cap). A projection that reaches the cap denies, even
	// when the current spend alone fits: a real request with
	// this prompt would be reserved and refused the same way.
	st, err := s.ledger.KeyBudgetStatus(ctx, keyID, now)
	if err != nil {
		s.logger.Printf("quotas/check: budget status failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusServiceUnavailable, "quota check unavailable: budget status failed")
		return
	}
	budget := quotaCheckBudget{
		SpentUSD: st.SpentUSD, RemainingUSD: st.RemainingUSD,
		Window: st.ActiveWindow, AtLimit: st.AtLimit, HasBudget: st.HasBudget,
	}
	if st.HasBudget {
		limit := st.LimitUSD
		budget.LimitUSD = &limit
		budget.UnpricedPolicy = string(st.UnpricedPolicy)
	}
	out.Budget = budget
	if out.Allowed && st.HasBudget && estCost != nil {
		projected := st.SpentUSD + *estCost
		out.Budget.ProjectedSpent = &projected
		if projected >= st.LimitUSD {
			out.Allowed = false
			out.Reason = "budget_exceeded"
			out.Detail = "estimated cost would reach the budget cap"
		} else if st.ActiveLimitUSD != nil {
			var activeSpent float64
			switch st.ActiveWindow {
			case "daily":
				activeSpent = st.Daily.SpentUSD
			case "weekly":
				activeSpent = st.Weekly.SpentUSD
			case "monthly":
				activeSpent = st.Monthly.SpentUSD
			}
			if activeSpent+*estCost >= *st.ActiveLimitUSD {
				out.Allowed = false
				out.Reason = "budget_exceeded"
				out.Detail = "estimated cost would reach the " + st.ActiveWindow + " budget cap"
			}
		}
	}

	// 3. The live token-limit gate, verbatim, plus per-limit
	// reporting. A breach the gate already sees denies; the
	// projection (used + estimate against the limit) denies
	// too — the check answers whether THIS request fits, and
	// its own tokens count against the headroom. Note the
	// asymmetry, documented honestly: the live gate admits the
	// request that pushes usage over a token limit and refuses
	// the NEXT one (CheckKeyTokenLimits breaches only once used
	// >= limit), while this check refuses the over-pushing
	// request itself. The check is never more permissive than
	// the gate — only more conservative.
	if s.cfg != nil {
		if key, ok := s.cfg.FindGatewayKey(keyID); ok {
			limits := quotaCheckTokenLimitConfigs(key)
			if len(limits) > 0 {
				if breach, err := s.ledger.CheckKeyTokenLimits(ctx, keyID, req.Provider, req.Model, limits, now); err != nil {
					s.logger.Printf("quotas/check: token limit check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
					writeServerError(w, http.StatusServiceUnavailable, "quota check unavailable: token limit check failed")
					return
				} else if breach != nil && out.Allowed {
					out.Allowed = false
					out.Reason = "token_limit_exceeded"
					out.Detail = breach.Error()
				}
			}
			for _, lim := range limits {
				if !lim.Enabled || lim.Limit <= 0 {
					continue
				}
				applies := false
				switch lim.Scope {
				case "global":
					applies = true
				case "provider":
					applies = req.Provider != "" && req.Provider == lim.ScopeValue
				case "model":
					applies = req.Model == lim.ScopeValue
				default:
					continue
				}
				if !applies {
					continue
				}
				interval, ok := ledger.NormalizeBudgetResetInterval(lim.ResetInterval)
				if !ok {
					continue
				}
				resetTime, ok := ledger.NormalizeBudgetResetTime(lim.ResetTime)
				if !ok {
					continue
				}
				window := ledger.BudgetWindowFor(interval, resetTime, now)
				used, err := s.quotaCheckTokenUsed(ctx, keyID, lim, window)
				if err != nil {
					s.logger.Printf("quotas/check: token usage failed for caller: %v", guardrails.RedactLogLine(err.Error()))
					writeServerError(w, http.StatusServiceUnavailable, "quota check unavailable: token usage failed")
					return
				}
				remaining := lim.Limit - used
				if remaining < 0 {
					remaining = 0
				}
				row := quotaCheckTokenLimit{
					Scope: lim.Scope, ScopeValue: lim.ScopeValue, Limit: lim.Limit,
					Used: used, Remaining: remaining,
					WouldExceed: used+estTotal >= lim.Limit,
					WindowStart: window.PeriodStart, NextReset: window.NextReset,
				}
				out.TokenLimits = append(out.TokenLimits, row)
				if out.Allowed && row.WouldExceed && estTotal > 0 {
					out.Allowed = false
					out.Reason = "token_limit_exceeded"
					out.Detail = "estimated tokens would reach the token limit"
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// Per-window total request counts for me/status (Area 11 row 724).
// ---------------------------------------------------------------------------

// totalRequestsInSlices is the single definition of a me/status
// window's total request count: the sum of UsageSlices Rows —
// successful, non-shared ledger rows (ok=1, shared=0) in the
// window, the same population the window's SpentUSD and Tokens
// already count. Failed attempts (ok=0, zero tokens) and shared
// coalesced waiter rows (zero tokens) are not in the total: the
// count is the number of requests the window's spend and tokens
// describe, never an estimate. The coordinator's keystatus.go
// patch inlines this exact sum in the usageWindow loop
// (`w.TotalRequests += sl.Rows`); this helper is the tested
// definition of that sum (see TestTotalRequestsInSlices).
func totalRequestsInSlices(slices []ledger.ModelUsageSlice) int {
	total := 0
	for _, sl := range slices {
		total += sl.Rows
	}
	return total
}
