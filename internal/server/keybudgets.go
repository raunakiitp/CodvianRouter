// Per-key rolling 5-hour USD budgets (CHECKLIST 139-140): the
// server side — pre-call enforcement on the chat paths, and the
// GET/POST /dashboard/key-budgets feeds for the Usage/Credits
// section.
//
// Enforcement runs BEFORE any provider is called, so a key at its
// limit can never spend another cent: priced spend at the cap is a
// 402, and a request to an unpriced model under the fail_closed
// policy is a 400 naming the unpriced models (the 400 is used only
// when unpriced usage alone is the block — an over-cap key gets
// the 402). The check needs the caller's key ID (from the verified
// gateway key, never from the request body) and the concrete
// model(s) the request would use: a plain model is checked as-is,
// an "auto/*" virtual model is resolved through the server's
// auto-router first, with the configured threshold. When the
// virtual model cannot be resolved the cap check still runs and
// the unpriced check is skipped — the engine will fail that
// request honestly anyway, and a guessed model would be worse
// than no model check.
//
// The check is skipped entirely when no ledger is wired (feeds
// answer empty) or the request arrived without a verifiable key
// (auth-relaxed mode: there is no key to budget). A ledger read
// failure fails closed with a 503: the budget state is unknown,
// and a billing guard must not wave traffic through blind.
//
// B2 (M10): the same gate enforces the per-key daily / weekly /
// monthly USD limits — ALL THREE at once, each against its own
// UTC calendar period's spend from the ledger (daily [today
// resetTime, tomorrow resetTime), weekly [Monday resetTime, next
// Monday resetTime), monthly [1st resetTime, next 1st resetTime),
// resetTime default "00:00"; see ledger.BudgetWindowFor).
// ResetInterval only picks which window the status reports as
// "active"; it never exempts a set cap. Unpriced usage already
// inside a set window's current period closes that period for
// ALL models until it ends (400 naming the models), exactly like
// the 5h budget's month-closed rule — never silently counted as
// $0. A D/W/M limit of 0 is rejected at the POST (nil = unset).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// WithLedgerPrices wires the ledger's per-token price map into the
// server, so the budget's "is this model priced?" question can see
// provider-reported prices too — not just the operator's price
// book. The map is the same one the async ledger writer charges
// with (main.go), so the gate and the ledger can never disagree
// about whether a model is priced.
func WithLedgerPrices(p ledger.Prices) Option {
	return func(s *Server) { s.ledgerPrices = p }
}

// modelPriced reports whether a model has a known price anywhere:
// the operator's price book (pair or model-level) or the ledger's
// price map (which also carries provider-reported prices). A
// configured $0 counts as priced (genuinely free); no entry
// anywhere means unpriced — never $0.
func (s *Server) modelPriced(model string) bool {
	if model == "" {
		return false
	}
	if s.priceBook != nil && s.priceBook.HasPriceForModel(model) {
		return true
	}
	if lp := s.getLedgerPrices(); lp != nil && lp.KnownModel(model) {
		return true
	}
	return false
}

// budgetCheckModels resolves the concrete models a chat request
// would use, for the unpriced check. A plain model is itself; an
// "auto/*" virtual model is classified through the server's
// auto-router over the last user message, with the configured
// threshold. It returns nil when the model cannot be resolved
// (no auto-router, or no concrete model for the picked tier):
// the caller then runs the cap check only, never inventing a
// model to check.
func (s *Server) budgetCheckModels(req *provider.ChatRequest) []string {
	if !autorouter.IsVirtualModel(req.Model) {
		return []string{req.Model}
	}
	if s.autoRouter == nil {
		return nil
	}
	prompt := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			prompt = req.Messages[i].Content
			break
		}
	}
	_, dec, ok := s.autoRouter.ResolveWithThreshold(req.Model, prompt, 0)
	if !ok || dec.Model == "" {
		return nil
	}
	return []string{dec.Model}
}

// checkKeyTokenLimits enforces one key's token limits for a
// request (M5.3): the ledger is the counter, so window usage is
// summed from usage_events — no separate counter table to drift.
// provider names the upstream provider when known pre-call (""
// on every path today: the provider is resolved inside the
// engine, so provider-scoped limits are enforced per attempt by
// the engine itself); model names the requested model ("" =
// unknown — model-scoped limits need a model, global limits
// always apply). A breach is a 429 + Retry-After to the window
// reset, error type "key_token_limit", raised before any provider
// is called and before the traffic check (a refused request
// consumes no traffic slot). A ledger read failure fails closed
// with a 503: a limit that cannot be read must not be waived.
func (s *Server) checkKeyTokenLimits(w http.ResponseWriter, r *http.Request, provider, model string) bool {
	if s.ledger == nil {
		return false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" || s.cfg == nil {
		return false
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok || len(key.TokenLimits) == 0 {
		return false
	}
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
	breach, err := s.ledger.CheckKeyTokenLimits(r.Context(), keyID, provider, model, limits, time.Now())
	if err != nil {
		s.logger.Printf("key-token-limits: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusServiceUnavailable, "token limit check unavailable")
		return true
	}
	if breach == nil {
		return false
	}
	seconds := int(time.Until(breach.NextReset) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: breach.Error(), Type: "key_token_limit"}})
	return true
}

// checkKeyBudgetForModel enforces one key's rolling budget for a
// request to the given concrete model ("" = model unknown, and
// "auto/*" virtual models count as unknown here: the relay path
// does not know the resolved model yet, so the unpriced check is
// skipped rather than guessed — the cap check still runs). It
// writes the refusal and returns true when the request must stop
// here, before any provider is called.
func (s *Server) checkKeyBudgetForModel(w http.ResponseWriter, r *http.Request, model string) bool {
	if s.ledger == nil {
		return false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return false
	}
	var models []string
	if model != "" && !autorouter.IsVirtualModel(model) {
		models = []string{model}
	}
	err := s.ledger.CheckKeyBudget(r.Context(), keyID, models, s.modelPriced, time.Now())
	if err == nil {
		return false
	}
	var be *ledger.BudgetExceededError
	if errors.As(err, &be) {
		writeError(w, http.StatusPaymentRequired, be.Error())
		return true
	}
	var ue *ledger.UnpricedUsageError
	if errors.As(err, &ue) {
		writeError(w, http.StatusBadRequest, ue.Error())
		return true
	}
	s.logger.Printf("key-budget: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
	writeServerError(w, http.StatusServiceUnavailable, "budget check unavailable")
	return true
}

// checkKeyBudget enforces the caller's rolling budget for a chat
// request: the concrete model is resolved (auto/* included) and
// the request is refused before any provider is called when the
// key is at its cap (402) or would use unpriced models under the
// fail_closed policy (400). It writes the refusal and returns true
// when the request must stop here.
// reserveKeyBudget is checkKeyBudget with the M5.8 reservation:
// before the provider is called, the prompt's estimated cost is
// COMMITTED against the key's budget (ledger.ReserveSpend), so
// parallel requests cannot all pass on the same headroom — the
// gate counts committed + settled spend. It returns the
// reservation id the caller must settle (defer
// s.settleReservation) once the request is done, after the
// engine has recorded the actual usage row; "" means no
// reservation exists (no ledger, no key, or an uncapped key).
// The estimate covers the prompt only — the completion is
// unknown before sending — so the reservation is a floor: the
// actual cost settles afterwards, and any overshoot is caught
// by the next request's check. An unpriced prompt under
// fail_closed is refused here (400), exactly like the plain
// check; under count_as_zero the estimate is $0 and the reserve
// is a no-op id.
func (s *Server) reserveKeyBudget(w http.ResponseWriter, r *http.Request, req *provider.ChatRequest) (string, bool) {
	if s.ledger == nil {
		return "", false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return "", false
	}
	estimate := s.estimatePromptCost(req)
	id, err := s.ledger.ReserveSpend(r.Context(), keyID, s.budgetCheckModels(req), s.modelPriced, estimate, time.Now())
	if err == nil {
		return id, false
	}
	var be *ledger.BudgetExceededError
	if errors.As(err, &be) {
		writeError(w, http.StatusPaymentRequired, be.Error())
		return "", true
	}
	var ue *ledger.UnpricedUsageError
	if errors.As(err, &ue) {
		writeError(w, http.StatusBadRequest, ue.Error())
		return "", true
	}
	s.logger.Printf("key-budget: reserve failed for caller: %v", guardrails.RedactLogLine(err.Error()))
	writeServerError(w, http.StatusServiceUnavailable, "budget check unavailable")
	return "", true
}

// settleReservation releases one committed reservation. It is
// best-effort: a failure only leaves the row to its TTL expiry
// (10 minutes), never fails the request that already served.
func (s *Server) settleReservation(id string) {
	if id == "" || s.ledger == nil {
		return
	}
	if err := s.ledger.SettleReservation(context.Background(), id); err != nil {
		s.logger.Printf("key-budget: settle reservation failed: %v", guardrails.RedactLogLine(err.Error()))
	}
}

// estimatePromptCost prices one request's prompt at the
// model-level price (the provider is unknown before routing):
// prompt tokens × input price per token. 0 when the prompt
// cannot be counted or the model has no known input price.
func (s *Server) estimatePromptCost(req *provider.ChatRequest) float64 {
	if req == nil {
		return 0
	}
	tokens := estimatePromptTokens(req)
	if tokens <= 0 {
		return 0
	}
	price, _ := s.pairPrice("", req.Model)
	if price.InputUSDPer1M == nil {
		return 0
	}
	return float64(tokens) * (*price.InputUSDPer1M / 1e6)
}

// ---------------------------------------------------------------------------
// GET/POST /dashboard/key-budgets.
// ---------------------------------------------------------------------------

// keyBudgetView is one row of the GET /dashboard/key-budgets
// payload: the key's identity (ID + operator name, never a raw key
// or hash) plus its live rolling-window position. A key with no
// budget still reports its window spend and unknown usage, so the
// operator can see what a budget would cap before setting one.
type keyBudgetView struct {
	KeyID string `json:"key_id"`
	Name  string `json:"name"`
	ledger.KeyBudgetStatus
}

// keyBudgetsResponse is the GET /dashboard/key-budgets payload.
type keyBudgetsResponse struct {
	Budgets        []keyBudgetView `json:"budgets"`
	RollingWindowH float64         `json:"rolling_window_hours"`
	WindowNote     string          `json:"window_note"`
}

// handleKeyBudgets serves GET /dashboard/key-budgets: every
// gateway key from the config (plus any ledger budget row for a
// key the config no longer names) with its live rolling 5-hour
// position. With no ledger wired it answers an empty list.
func (s *Server) handleKeyBudgets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := keyBudgetsResponse{
		Budgets:        []keyBudgetView{},
		RollingWindowH: ledger.RollingWindow.Hours(),
		WindowNote:     "Rolling window: spend in the last 5 hours only, ageing out continuously — no calendar-day reset. Spent is priced spend only; unreported or unpriced usage is listed separately as unknown, never counted as $0.",
	}
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	now := time.Now()
	names := map[string]string{}
	seen := map[string]bool{}
	if s.cfg != nil {
		for _, k := range s.cfg.GatewayKeys {
			names[k.ID] = k.Name
			seen[k.ID] = true
		}
	}
	if rows, err := s.ledger.ListKeyBudgets(ctx); err == nil {
		for _, b := range rows {
			seen[b.KeyID] = true
		}
	} else {
		s.logger.Printf("key-budget: list failed: %v", guardrails.RedactLogLine(err.Error()))
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st, err := s.ledger.KeyBudgetStatus(ctx, id, now)
		if err != nil {
			s.logger.Printf("key-budget: status failed for key: %v", guardrails.RedactLogLine(err.Error()))
			continue
		}
		out.Budgets = append(out.Budgets, keyBudgetView{KeyID: id, Name: names[id], KeyBudgetStatus: st})
	}
	writeJSON(w, http.StatusOK, out)
}

// keyBudgetUpdate is one POST /dashboard/key-budgets change: set a
// key's rolling budget, or delete it (Delete true). UnpricedPolicy
// "" means keep the existing policy (fail_closed for a new row).
type keyBudgetUpdate struct {
	KeyID          string  `json:"key_id"`
	LimitUSD       float64 `json:"limit_usd"`
	UnpricedPolicy string  `json:"unpriced_policy"`
	Delete         bool    `json:"delete"`
	// M5.1 window fields: pointers so "absent" means "keep the
	// stored value" — a window limit can only be cleared by
	// deleting the whole budget and re-creating it (nil here is
	// not a clear). Same for the selector fields. B2: a D/W/M
	// limit of 0 is rejected, like a negative one — nil means
	// unset; 0 would refuse every request while looking like a
	// configured budget (the ledger rejects 0 too; this 400 is
	// the clean error for it).
	DailyLimitUSD    *float64 `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD   *float64 `json:"weekly_limit_usd,omitempty"`
	MonthlyLimitUSD  *float64 `json:"monthly_limit_usd,omitempty"`
	WarningThreshold *float64 `json:"warning_threshold,omitempty"`
	ResetInterval    *string  `json:"reset_interval,omitempty"`
	ResetTime        *string  `json:"reset_time,omitempty"`
}

// handleKeyBudgetUpdate serves POST /dashboard/key-budgets. The
// ledger row is written first, then the config file's key_budgets
// block (when a config + persist function are wired) so the two
// never disagree — a config save failure rolls the ledger change
// back and answers 500, exactly like the danger-zone actions.
// Without a config wired the budget lives in the ledger only,
// which is still the durable store: it survives a restart.
func (s *Server) handleKeyBudgetUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "key budgets unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update keyBudgetUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key budget update: expected {\"key_id\": string, \"limit_usd\": number, \"unpriced_policy\": \"fail_closed\"|\"count_as_zero\", \"delete\": bool}")
		return
	}
	if update.KeyID == "" {
		writeError(w, http.StatusBadRequest, "key_id is required")
		return
	}
	if s.cfg != nil {
		if _, ok := s.cfg.FindGatewayKey(update.KeyID); !ok {
			writeError(w, http.StatusNotFound, "unknown gateway key "+update.KeyID)
			return
		}
	}
	if !update.Delete {
		if update.LimitUSD < 0 {
			writeError(w, http.StatusBadRequest, "limit_usd must not be negative")
			return
		}
		if _, ok := ledger.NormalizeUnpricedPolicy(update.UnpricedPolicy); !ok {
			writeError(w, http.StatusBadRequest, "unknown unpriced_policy "+update.UnpricedPolicy+" (want fail_closed or count_as_zero)")
			return
		}
		// M5.1 window fields, B2 validation: each D/W/M limit
		// must be positive — negative or zero is rejected; the
		// interval must be a known name; the reset time HH:MM;
		// the warning threshold 0–1. (The 5-hour LimitUSD keeps
		// its older rule — 0 = no spend allowed — because that
		// field is a plain float64 and an omitted value decodes
		// as 0; only the pointer D/W/M fields can tell "absent"
		// from "zero".)
		for name, v := range map[string]*float64{"daily_limit_usd": update.DailyLimitUSD, "weekly_limit_usd": update.WeeklyLimitUSD, "monthly_limit_usd": update.MonthlyLimitUSD} {
			if v != nil && *v <= 0 {
				writeError(w, http.StatusBadRequest, name+" must be positive")
				return
			}
		}
		if update.WarningThreshold != nil && (*update.WarningThreshold < 0 || *update.WarningThreshold > 1) {
			writeError(w, http.StatusBadRequest, "warning_threshold must be between 0 and 1")
			return
		}
		if update.ResetInterval != nil {
			if _, ok := ledger.NormalizeBudgetResetInterval(*update.ResetInterval); !ok {
				writeError(w, http.StatusBadRequest, "unknown reset_interval "+*update.ResetInterval+" (want daily, weekly or monthly)")
				return
			}
		}
		if update.ResetTime != nil {
			if _, ok := ledger.NormalizeBudgetResetTime(*update.ResetTime); !ok {
				writeError(w, http.StatusBadRequest, "malformed reset_time "+*update.ResetTime+" (want HH:MM)")
				return
			}
		}
	}
	ctx := r.Context()
	policy, _ := ledger.NormalizeUnpricedPolicy(update.UnpricedPolicy)
	// Keep the previous row so a failed config persist can roll
	// the ledger change back.
	prev, prevErr := s.ledger.GetKeyBudget(ctx, update.KeyID)
	hadPrev := prevErr == nil
	if update.Delete {
		if err := s.ledger.DeleteKeyBudget(ctx, update.KeyID); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not delete key budget")
			return
		}
	} else {
		// M5.1: merge the window fields over the previous row
		// (or the defaults for a new row) and write the full
		// row — absent fields keep their stored values.
		next := prev
		if !hadPrev {
			next = ledger.KeyBudget{KeyID: update.KeyID, WarningThreshold: 0.8, ResetInterval: ledger.BudgetResetDaily, ResetTime: "00:00"}
		}
		next.LimitUSD = update.LimitUSD
		next.UnpricedPolicy = policy
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
			writeServerError(w, http.StatusInternalServerError, "could not save key budget")
			return
		}
	}
	// Config block: keep the file in step with the ledger, or
	// roll the ledger back when the save fails.
	persistedToConfig := false
	if s.cfg != nil && s.configPersist != nil {
		if s.cfg.KeyBudgets == nil {
			s.cfg.KeyBudgets = map[string]config.KeyBudgetConfig{}
		}
		if update.Delete {
			delete(s.cfg.KeyBudgets, update.KeyID)
		} else {
			// Preserve the stored policy spelling when the
			// caller did not name one and a row already exists.
			stored := update.UnpricedPolicy
			if stored == "" {
				if prev, ok := s.cfg.KeyBudgets[update.KeyID]; ok {
					stored = prev.UnpricedPolicy
				}
			}
			// M5.1: the config row carries the same merged
			// window fields as the ledger row — absent fields
			// keep their stored values.
			cfgRow := s.cfg.KeyBudgets[update.KeyID]
			cfgRow.LimitUSD = update.LimitUSD
			cfgRow.UnpricedPolicy = stored
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
			s.cfg.KeyBudgets[update.KeyID] = cfgRow
		}
		if err := s.configPersist(); err != nil {
			// Roll the ledger back: runtime and file must never
			// disagree about a money cap.
			if hadPrev {
				_ = s.ledger.SetKeyBudgetConfig(ctx, prev)
			} else if update.Delete {
				// Nothing to restore: the row did not exist
				// before, and the delete already removed it.
			} else {
				_ = s.ledger.DeleteKeyBudget(ctx, update.KeyID)
			}
			s.logger.Printf("key-budget: config persist failed, ledger rolled back: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not save key budget to the config file")
			return
		}
		persistedToConfig = true
	}
	s.logger.Printf("key-budget: updated (delete=%v persisted_to_config=%v)", update.Delete, persistedToConfig)
	writeJSON(w, http.StatusOK, map[string]any{
		"key_id": update.KeyID, "deleted": update.Delete,
		"persisted_to_config": persistedToConfig,
	})
}
