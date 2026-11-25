// Gateway-key self status, expiry, regeneration and per-key traffic
// limits (CHECKLIST 195-196, competitor-sweep P2 gaps — the
// operator surfaces comparable gateways expose for their keys):
//
//   - GET /v1/me/status — the caller's own key, introspected: id,
//     fingerprint, created/expiry, spend this period, budget
//     remaining, traffic limits and live window usage. The key
//     value itself is never returned (only the 12-hex fingerprint
//     of the stored SHA-256 hash), and an expired key never reaches
//     the handler at all — the auth wrapper answers 401 naming the
//     expiry first.
//   - POST /admin/keys/{id} — the operator sets an expiry and/or
//     the requests-per-minute + tokens-per-minute limits on one
//     key, persisted to the config file.
//   - POST /admin/keys/{id}/rotate — the operator regenerates one
//     key: the old key is revoked at rotation and a new key is
//     issued in its place (same name/scopes/budget/limits unless
//     overridden), returned exactly once and never logged or
//     stored in plaintext.
//
// All three are Tier 2/3 in the guard table (see guard.go): status
// needs a valid gateway key, update and rotate always need one
// (Tier 2 never honours relaxed auth). Key mutations serialise on
// keyMu and roll the in-memory config back when the file persist
// fails, so the running server and the config file can never
// disagree about which keys exist.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// keyFingerprint is the public fingerprint of a stored gateway key:
// the first 12 hex chars of its stored SHA-256 hash, prefixed so it
// is never mistaken for a key. Enough to match a key to its record
// in a log or a dashboard row; never enough to authenticate with,
// and never the raw key (which is not stored anywhere).
func keyFingerprint(k config.GatewayKey) string {
	h := k.KeyHash
	if len(h) > 12 {
		h = h[:12]
	}
	return "sha256:" + h
}

// ---------------------------------------------------------------------------
// GET /v1/me/status.
// ---------------------------------------------------------------------------

// meStatusBudget is the caller's rolling 5-hour budget position
// (CHECKLIST 139), as GET /v1/me/status reports it. Nil when no
// ledger is wired: spend is then unknown, never zero.
type meStatusBudget struct {
	LimitUSD       *float64   `json:"limit_usd,omitempty"`
	SpentUSD       float64    `json:"spent_usd"`
	RemainingUSD   *float64   `json:"remaining_usd,omitempty"`
	AtLimit        bool       `json:"at_limit"`
	WindowStart    time.Time  `json:"window_start"`
	WindowEnd      time.Time  `json:"window_end"`
	NextRolloff    *time.Time `json:"next_rolloff,omitempty"`
	UnknownEvents  int        `json:"unknown_events"`
	UnknownTokens  int        `json:"unknown_tokens"`
	UnpricedModels []string   `json:"unpriced_models"`
}

// meStatusTraffic is the caller's per-key traffic position
// (CHECKLIST 195): the operator-set limits and the live window
// usage. Remaining is nil on an axis with no limit (unlimited),
// and the usage counts are since server start (in-memory — see
// keytraffic.go), never across a restart.
type meStatusTraffic struct {
	RequestsPerMinute  int  `json:"requests_per_minute"`
	TokensPerMinute    int  `json:"tokens_per_minute"`
	RequestsLastMinute int  `json:"requests_last_minute"`
	TokensLastMinute   int  `json:"tokens_last_minute"`
	RequestsRemaining  *int `json:"requests_remaining,omitempty"`
	TokensRemaining    *int `json:"tokens_remaining,omitempty"`
}

// meStatusUsageWindow is one spend window of GET /v1/me/status
// (M5.6, the reference router's usage.cost / daily / weekly): metered spend
// and tokens in [PeriodStart, PeriodEnd), the number of requests
// that used tokens but had no known metered price, and the same
// usage repriced at the catalog's list prices. ListPriceUSD is
// nil when the window is empty; ListPriceComplete is false when
// any row in the window could not be repriced (unknown token
// split, or no catalog price for its model) — the reported figure
// then covers only the repricable rows, never an invention.
type meStatusUsageWindow struct {
	PeriodStart       time.Time `json:"period_start_at"`
	PeriodEnd         time.Time `json:"period_end_at"`
	SpentUSD          float64   `json:"spent_usd"`
	Tokens            int       `json:"tokens"`
	TotalRequests     int       `json:"total_requests"`
	UnpricedRequests  int       `json:"unpriced_requests"`
	ListPriceUSD      *float64  `json:"list_price_usd,omitempty"`
	ListPriceComplete bool      `json:"list_price_complete"`
}

// meStatusUsage is the usage section of GET /v1/me/status: Cost
// is the key's ACTIVE budget window (the one CheckKeyBudget
// enforces — daily/weekly/monthly per its reset interval, or the
// rolling 5h cap when no calendar limit is set), Daily is the
// UTC calendar day at the key's reset time, Weekly the
// Monday-to-Monday week at the same reset time.
type meStatusUsage struct {
	Cost   meStatusUsageWindow `json:"cost"`
	Daily  meStatusUsageWindow `json:"daily"`
	Weekly meStatusUsageWindow `json:"weekly"`
}

// meStatusAccountQuota is one upstream-account quota row of GET
// /v1/me/status (M5.5, the reference router's accountQuotas in
// apiKeySelfService.ts): for each fair-share pool the caller
// belongs to, each dimension reports the per-account limit, the
// number of backing accounts, the effective limit (per-account ×
// accounts), the caller's own consumption in the window, the
// pool-wide consumption, and the caller's fair share of the
// effective limit. Only the caller's own pools are ever listed —
// a key cannot see another key's pools. (the reference router gates this
// section on scope self:account-quota; we have no such scope —
// the section is inherently per-caller, so it is included for
// any key that belongs to a pool.)
type meStatusAccountQuota struct {
	PoolID          string    `json:"pool_id"`
	PoolName        string    `json:"pool_name"`
	Unit            string    `json:"unit"`
	Window          string    `json:"window"`
	PerAccountLimit float64   `json:"per_account_limit"`
	AccountCount    int       `json:"account_count"`
	EffectiveLimit  float64   `json:"effective_limit"`
	Consumed        float64   `json:"consumed"`
	ConsumedTotal   float64   `json:"consumed_total"`
	Remaining       float64   `json:"remaining"`
	FairShare       float64   `json:"fair_share"`
	NextReset       time.Time `json:"next_reset_at"`
}

// meStatusResponse is the GET /v1/me/status payload: everything a
// caller needs to know about its own key, and nothing it must not
// see. It never contains the key value or the full stored hash —
// only the 12-hex fingerprint. Note is set when the key record
// itself is unavailable (no config loaded), so the caller is told
// what is unknown instead of shown zeros.
type meStatusResponse struct {
	KeyID            string          `json:"key_id"`
	Fingerprint      string          `json:"fingerprint"`
	Name             string          `json:"name"`
	Scopes           []string        `json:"scopes"`
	CreatedAt        time.Time       `json:"created_at"`
	ExpiresAt        *time.Time      `json:"expires_at,omitempty"`
	Expired          bool            `json:"expired"`
	ExpiresInSeconds *int64          `json:"expires_in_seconds,omitempty"`
	BudgetUSD        float64         `json:"budget_usd"`
	Budget           *meStatusBudget `json:"budget,omitempty"`
	Usage            *meStatusUsage  `json:"usage,omitempty"`
	Traffic          meStatusTraffic `json:"traffic"`
	// G15 Area 4: the caller's own enforceable policy (IP
	// lists, model access, endpoints, forced model) and the
	// devices seen under this key (fingerprints + counts only —
	// never a raw IP or User-Agent). A caller sees only its own
	// key's policy and devices, never another key's.
	Policy        keyPolicyView          `json:"policy"`
	Devices       []keyDeviceView        `json:"devices,omitempty"`
	AccountQuotas []meStatusAccountQuota `json:"account_quotas,omitempty"`
	Team          *meStatusTeam          `json:"team,omitempty"`
	Note          string                 `json:"note,omitempty"`
}

// handleMeStatus serves GET /v1/me/status: authenticated
// gateway-key introspection. The auth wrapper has already verified
// the key (an expired key answers 401 naming the expiry there, so
// this handler never serves an expired key). A request that
// arrives without a nameable key — auth-relaxed mode, or a
// verifier that cannot name keys — answers 401: there is no
// "me" to report without a caller identity.
// catalogListPrices returns the catalog's published per-model
// prices (USD per 1M tokens) keyed by model ID, from the model
// registry. The registry adapter deduplicates by model ID (first
// provider wins), so when two providers list the same model ID
// at different prices the first registry row's prices apply —
// the list-price figure is informational ("what this usage would
// cost at list"), and the approximation is documented here, not
// hidden. A model with no reported prices is absent from the map.
func (s *Server) catalogListPrices(ctx context.Context) map[string][2]float64 {
	out := map[string][2]float64{}
	if s.registry == nil {
		return out
	}
	models, err := s.registry.Models(ctx)
	if err != nil {
		return out
	}
	for _, m := range models {
		if m.InputPriceUSD > 0 && m.OutputPriceUSD > 0 {
			out[m.ID] = [2]float64{m.InputPriceUSD, m.OutputPriceUSD}
		}
	}
	return out
}

// usageWindow aggregates one key's usage in [start, end) into a
// status window: metered spend + tokens from the ledger slices,
// unpriced requests (rows with no metered price), and the same
// usage repriced at catalog list prices. A slice whose token
// split is unknown, or whose model has no catalog price, is
// left out of the list-price sum and flips ListPriceComplete off
// — the figure then covers only the repricable rows. An empty
// window reports zeros with a nil list price.
func (s *Server) usageWindow(ctx context.Context, keyID string, start, end time.Time, prices map[string][2]float64) (meStatusUsageWindow, error) {
	w := meStatusUsageWindow{PeriodStart: start, PeriodEnd: end, ListPriceComplete: true}
	slices, err := s.ledger.UsageSlices(ctx, keyID, start, end)
	if err != nil {
		return w, err
	}
	if len(slices) == 0 {
		return w, nil
	}
	var list float64
	for _, sl := range slices {
		w.SpentUSD += sl.CostUSD
		w.Tokens += sl.TotalTokens
		w.TotalRequests += sl.Rows
		if !sl.PriceKnown {
			w.UnpricedRequests += sl.Rows
		}
		if !sl.SplitKnown {
			w.ListPriceComplete = false
			continue
		}
		p, ok := prices[sl.Model]
		if !ok {
			w.ListPriceComplete = false
			continue
		}
		list += (float64(sl.PromptTokens)*p[0] + float64(sl.CompletionTokens)*p[1]) / 1e6
	}
	w.ListPriceUSD = &list
	return w, nil
}

// meStatusUsageFor builds the usage section of GET /v1/me/status
// for one key: the active budget window (the one CheckKeyBudget
// enforces — daily/weekly/monthly per the key's reset interval,
// or the rolling 5h cap when no calendar limit is set), the UTC
// calendar day and the Monday-to-Monday week at the key's reset
// time. A window whose ledger read fails is left zeroed and the
// failure logged — reporting is fail-open, the section is never
// a reason to 500 the whole status response.
func (s *Server) meStatusUsageFor(ctx context.Context, keyID string, now time.Time) *meStatusUsage {
	st, err := s.ledger.KeyBudgetStatus(ctx, keyID, now)
	if err != nil {
		s.logger.Printf("me/status: budget status failed for usage: %v", guardrails.RedactLogLine(err.Error()))
		return nil
	}
	prices := s.catalogListPrices(ctx)
	usage := &meStatusUsage{}
	// The cost window is the active one: the calendar window the
	// gate enforces (its period is already computed on the
	// status), or the rolling 5h cap when no calendar limit is
	// set (WindowStart/WindowEnd on the status are that rolling
	// window). The end is now + 1ms, not now: usage rows are
	// stamped in whole milliseconds, so a row recorded in the
	// same millisecond as this call would otherwise fall outside
	// [start, now) and the cost window would under-report the
	// spend the budget's own (unbounded) query just counted.
	costStart, costEnd := st.WindowStart, st.WindowEnd.Add(time.Millisecond)
	switch st.ActiveWindow {
	case "daily":
		costStart, costEnd = st.Daily.PeriodStart, st.Daily.NextReset
	case "weekly":
		costStart, costEnd = st.Weekly.PeriodStart, st.Weekly.NextReset
	case "monthly":
		costStart, costEnd = st.Monthly.PeriodStart, st.Monthly.NextReset
	}
	if w, err := s.usageWindow(ctx, keyID, costStart, costEnd, prices); err != nil {
		s.logger.Printf("me/status: cost window failed for caller: %v", guardrails.RedactLogLine(err.Error()))
	} else {
		usage.Cost = w
	}
	daily := ledger.BudgetWindowFor(ledger.BudgetResetDaily, st.ResetTime, now)
	if w, err := s.usageWindow(ctx, keyID, daily.PeriodStart, daily.NextReset, prices); err != nil {
		s.logger.Printf("me/status: daily window failed for caller: %v", guardrails.RedactLogLine(err.Error()))
	} else {
		usage.Daily = w
	}
	weekly := ledger.BudgetWindowFor(ledger.BudgetResetWeekly, st.ResetTime, now)
	if w, err := s.usageWindow(ctx, keyID, weekly.PeriodStart, weekly.NextReset, prices); err != nil {
		s.logger.Printf("me/status: weekly window failed for caller: %v", guardrails.RedactLogLine(err.Error()))
	} else {
		usage.Weekly = w
	}
	return usage
}

func (s *Server) handleMeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		writeError(w, http.StatusUnauthorized, "a gateway key is required to identify the caller")
		return
	}
	now := time.Now()
	out := meStatusResponse{KeyID: keyID, Scopes: []string{}}
	if s.cfg == nil {
		out.Note = "key record unavailable: the server runs without a loaded config, so expiry, budget and traffic limits are unknown"
		writeJSON(w, http.StatusOK, out)
		return
	}
	key, ok := s.cfg.FindGatewayKey(keyID)
	if !ok {
		// The verifier named a key the config no longer holds
		// (a static-verifier key, or a key rotated away since
		// the verifier was built): report the id honestly and
		// say the record is unknown, never invent one.
		out.Note = "key record unavailable: this key id is not in the loaded config, so expiry, budget and traffic limits are unknown"
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Fingerprint = keyFingerprint(key)
	out.Name = key.Name
	out.Scopes = key.Scopes
	if out.Scopes == nil {
		out.Scopes = []string{}
	}
	// G15 Area 4: the caller's own policy + devices.
	out.Policy = keyPolicyViewOf(key)
	out.Devices = s.keyDevicesFor(keyID)
	out.CreatedAt = key.CreatedAt
	out.ExpiresAt = key.ExpiresAt
	out.Expired = key.Expired(now)
	if key.ExpiresAt != nil {
		secs := int64(key.ExpiresAt.Sub(now) / time.Second)
		out.ExpiresInSeconds = &secs
	}
	out.BudgetUSD = key.BudgetUSD
	if s.ledger != nil {
		if st, err := s.ledger.KeyBudgetStatus(r.Context(), keyID, now); err == nil {
			b := &meStatusBudget{
				SpentUSD: st.SpentUSD, RemainingUSD: st.RemainingUSD,
				AtLimit: st.AtLimit, WindowStart: st.WindowStart,
				WindowEnd: st.WindowEnd, NextRolloff: st.NextRolloff,
				UnknownEvents: st.UnknownEvents, UnknownTokens: st.UnknownTokens,
				UnpricedModels: st.UnpricedModels,
			}
			if st.HasBudget {
				limit := st.LimitUSD
				b.LimitUSD = &limit
			}
			if b.UnpricedModels == nil {
				b.UnpricedModels = []string{}
			}
			out.Budget = b
		} else {
			s.logger.Printf("me/status: budget status failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		}
	}
	reqs, toks := s.keyTrafficUsage(keyID)
	tr := meStatusTraffic{
		RequestsPerMinute: key.RequestsPerMinute, TokensPerMinute: key.TokensPerMinute,
		RequestsLastMinute: reqs, TokensLastMinute: toks,
	}
	if key.RequestsPerMinute > 0 {
		rem := key.RequestsPerMinute - reqs
		if rem < 0 {
			rem = 0
		}
		tr.RequestsRemaining = &rem
	}
	if key.TokensPerMinute > 0 {
		rem := key.TokensPerMinute - toks
		if rem < 0 {
			rem = 0
		}
		tr.TokensRemaining = &rem
	}
	out.Traffic = tr
	// Usage windows (M5.6): metered spend + tokens and the
	// list-price repricing for the active budget window, the
	// calendar day and the week. Fail-open: a missing section
	// means the ledger read failed, logged above.
	if s.ledger != nil {
		out.Usage = s.meStatusUsageFor(r.Context(), keyID, now)
	}
	// Account quotas (M5.5): the caller's fair-share pools, one
	// row per pool dimension — per-account limit, backing
	// accounts, effective limit, own + pool-wide consumption,
	// and the caller's fair share. Reporting is fail-open (a
	// ledger read failure logs and omits the section — the gate
	// itself still fails closed; a status read must not 500 the
	// whole response), and only the caller's own pools appear.
	if s.ledger != nil {
		if pools, err := s.ledger.PoolsForKey(r.Context(), keyID); err != nil {
			s.logger.Printf("me/status: pool list failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		} else {
			for _, pool := range pools {
				var weight float64
				for _, a := range pool.Allocations {
					if a.KeyID == keyID {
						weight = a.Weight
						break
					}
				}
				for _, dim := range pool.Dimensions {
					used, total, nextReset, err := s.ledger.PoolKeyDimensionUsage(r.Context(), pool, dim, keyID, now)
					if err != nil {
						s.logger.Printf("me/status: pool usage failed for caller: %v", guardrails.RedactLogLine(err.Error()))
						continue
					}
					effective := pool.EffectiveLimit(dim)
					remaining := effective - total
					if remaining < 0 {
						remaining = 0
					}
					out.AccountQuotas = append(out.AccountQuotas, meStatusAccountQuota{
						PoolID:          pool.ID,
						PoolName:        pool.Name,
						Unit:            dim.Unit,
						Window:          dim.Window,
						PerAccountLimit: dim.Limit,
						AccountCount:    pool.EffectiveAccountCount(),
						EffectiveLimit:  effective,
						Consumed:        used,
						ConsumedTotal:   total,
						Remaining:       remaining,
						FairShare:       weight / 100 * effective,
						NextReset:       nextReset,
					})
				}
			}
		}
	}
	// Team (M5.12): the caller's cost center and its shared budget
	// in the current window. Fail-open like the sections above: a
	// ledger read failure logs and omits the section.
	if s.ledger != nil {
		if team, err := s.ledger.ActiveTeamForKey(r.Context(), keyID); err != nil {
			s.logger.Printf("me/status: team lookup failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		} else if team != nil && team.MaxBudgetUSD != nil {
			if st, err := s.teamStatus(r.Context(), *team, now); err != nil {
				s.logger.Printf("me/status: team status failed for caller: %v", guardrails.RedactLogLine(err.Error()))
			} else {
				out.Team = &meStatusTeam{
					TeamID: team.ID, TeamName: team.Name,
					MaxBudgetUSD: team.MaxBudgetUSD, BudgetDuration: team.BudgetDuration,
					WindowStart: st.WindowStart, WindowEnd: st.WindowEnd,
					EstimatedListCostUSD: st.EstimatedListCostUSD, MeteredCostUSD: st.MeteredCostUSD,
					HasUnpricedUsage: st.HasUnpricedUsage, Exceeded: st.Exceeded,
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------------------
// POST /admin/keys/{id} — set expiry + traffic limits.
// ---------------------------------------------------------------------------

// keyUpdateRequest is one POST /admin/keys/{id} change. Every field
// is optional: absent means unchanged. ExpiresAt is a pointer so
// "absent" and "null" stay distinct from "set" — null alone does
// not clear; ClearExpiry does. A negative limit is a 400, never
// clamped silently. Disabled is a pointer for the same reason:
// absent means unchanged, while present (true or false) disables
// or re-enables the key (M1.3) — the reversible kill-switch. The
// change takes effect on the next request, with no restart, and is
// written to the audit log. Guardrails is a pointer to a slice for
// the same reason: absent means unchanged, while a present list
// (including an empty one, which clears the key's list) replaces
// it — and every name in it must be registered (CHECKLIST 245),
// or the whole update is a 400 naming the unknown name.
type keyUpdateRequest struct {
	ExpiresAt         *time.Time `json:"expires_at"`
	ClearExpiry       bool       `json:"clear_expiry"`
	RequestsPerMinute *int       `json:"requests_per_minute"`
	TokensPerMinute   *int       `json:"tokens_per_minute"`
	Disabled          *bool      `json:"disabled"`
	Guardrails        *[]string  `json:"guardrails"`
	AllowUsageCommand *bool      `json:"allow_usage_command"`
	// M5.10: the allowed-hours window (nil = unchanged; a present
	// object with enabled=false clears the schedule), the
	// per-request throttle delay and the concurrent-session cap.
	AccessSchedule  *config.AccessScheduleConfig `json:"access_schedule"`
	ThrottleDelayMs *int                         `json:"throttle_delay_ms"`
	MaxSessions     *int                         `json:"max_sessions"`
	// G15 Area 4 policy fields (pointers: absent = unchanged, a
	// present empty list clears that list). Template is accepted
	// only so it can be rejected with a clear error — it is a
	// creation-time preset, not an update operation.
	Template         *string   `json:"template"`
	IPAllowlist      *[]string `json:"ip_allowlist"`
	IPBlocklist      *[]string `json:"ip_blocklist"`
	ModelAccessMode  *string   `json:"model_access_mode"`
	AllowedModels    *[]string `json:"allowed_models"`
	BlockedModels    *[]string `json:"blocked_models"`
	AllowedEndpoints *[]string `json:"allowed_endpoints"`
	ForcedModel      *string   `json:"forced_model"`
}

// policyRequest renders this update's policy section as a
// keyPolicyRequest for resolve().
func (u keyUpdateRequest) policyRequest() *keyPolicyRequest {
	return &keyPolicyRequest{
		Template:    u.Template,
		IPAllowlist: u.IPAllowlist, IPBlocklist: u.IPBlocklist,
		ModelAccessMode: u.ModelAccessMode, AllowedModels: u.AllowedModels,
		BlockedModels: u.BlockedModels, AllowedEndpoints: u.AllowedEndpoints,
		ForcedModel: u.ForcedModel,
	}
}

// keyUpdateResponse is the POST /admin/keys/{id} reply: the updated
// key as the dashboard feed shows it, plus whether the change
// reached the config file.
type keyUpdateResponse struct {
	Key               routerKeyView `json:"key"`
	Policy            keyPolicyView `json:"policy"`
	PersistedToConfig bool          `json:"persisted_to_config"`
	Note              string        `json:"note,omitempty"`
}

// handleKeyUpdate serves POST /admin/keys/{id} (Tier 2): it sets an
// expiry and/or the per-key traffic limits on one gateway key. The
// in-memory config is mutated first, then persisted; a persist
// failure rolls the mutation back and answers 500, so the running
// server and the file never disagree. With no persist function
// wired the change applies in memory only and says so.
func (s *Server) handleKeyUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "key update unavailable: no config loaded")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update keyUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key update: expected {\"expires_at\": \"RFC3339\" (optional), \"clear_expiry\": bool, \"requests_per_minute\": number, \"tokens_per_minute\": number}")
		return
	}
	if update.RequestsPerMinute != nil && *update.RequestsPerMinute < 0 {
		writeError(w, http.StatusBadRequest, "requests_per_minute must not be negative")
		return
	}
	if update.TokensPerMinute != nil && *update.TokensPerMinute < 0 {
		writeError(w, http.StatusBadRequest, "tokens_per_minute must not be negative")
		return
	}
	if update.ThrottleDelayMs != nil && *update.ThrottleDelayMs < 0 {
		writeError(w, http.StatusBadRequest, "throttle_delay_ms must not be negative")
		return
	}
	if update.MaxSessions != nil && *update.MaxSessions < 0 {
		writeError(w, http.StatusBadRequest, "max_sessions must not be negative")
		return
	}
	// M5.10 access schedule: from/until are HH:MM (required
	// when enabled), days are weekdays 0-6, tz must load.
	if sch := update.AccessSchedule; sch != nil {
		if sch.Enabled {
			if _, err := parseHHMM(sch.From); err != nil {
				writeError(w, http.StatusBadRequest, "access_schedule.from is required (HH:MM) when the schedule is enabled")
				return
			}
			if _, err := parseHHMM(sch.Until); err != nil {
				writeError(w, http.StatusBadRequest, "access_schedule.until is required (HH:MM) when the schedule is enabled")
				return
			}
		} else {
			if sch.From != "" {
				if _, err := parseHHMM(sch.From); err != nil {
					writeError(w, http.StatusBadRequest, "access_schedule.from is not HH:MM")
					return
				}
			}
			if sch.Until != "" {
				if _, err := parseHHMM(sch.Until); err != nil {
					writeError(w, http.StatusBadRequest, "access_schedule.until is not HH:MM")
					return
				}
			}
		}
		for _, d := range sch.Days {
			if d < 0 || d > 6 {
				writeError(w, http.StatusBadRequest, "access_schedule.days must be weekdays 0-6 (0 = Sunday)")
				return
			}
		}
		if sch.TZ != "" {
			if _, err := time.LoadLocation(sch.TZ); err != nil {
				writeError(w, http.StatusBadRequest, "access_schedule.tz is not a known IANA zone: "+err.Error())
				return
			}
		}
	}
	// Security cluster (CHECKLIST 245, LiteLLM #51080): a guardrail
	// name the key is given must be registered — a built-in kind
	// or a configured external guardrail. A typo'd name fails
	// loudly here, naming the name, instead of silently leaving
	// the key believing it is guarded. The refusal is counted
	// as a security refusal for the dashboard.
	if update.Guardrails != nil {
		if err := s.guardrails.Config().ValidateGuardrailNames(*update.Guardrails); err != nil {
			s.guardrails.RecordSecurityRefusal(guardrails.SecurityRefusalUnknownGuardrail)
			writeError(w, http.StatusBadRequest, "invalid key update: "+err.Error())
			return
		}
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	idx := -1
	for i, k := range s.cfg.GatewayKeys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "unknown gateway key "+id)
		return
	}
	snapshot := s.cfg.Clone()
	key := &s.cfg.GatewayKeys[idx]
	// G15 Area 4: resolve the policy merge BEFORE mutating
	// anything, so a 400 leaves the stored key untouched (the
	// same fail-before-mutate rule as the guardrail check
	// above). Explicit fields merge onto the stored policy.
	var mergedPolicy *config.KeyPolicy
	if update.policyRequest().hasPolicy() {
		merged, err := update.policyRequest().resolveOnto(key.PolicyOf(), false)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid key policy: "+err.Error())
			return
		}
		mergedPolicy = &merged
	}
	if update.ClearExpiry {
		key.ExpiresAt = nil
	} else if update.ExpiresAt != nil {
		exp := *update.ExpiresAt
		key.ExpiresAt = &exp
	}
	if update.RequestsPerMinute != nil {
		key.RequestsPerMinute = *update.RequestsPerMinute
	}
	if update.TokensPerMinute != nil {
		key.TokensPerMinute = *update.TokensPerMinute
	}
	if update.Guardrails != nil {
		key.Guardrails = append([]string(nil), *update.Guardrails...)
	}
	// M1.3: disable / re-enable. The flag is applied under the
	// same keyMu + persist + rollback as every other field, so
	// the running server and the file never disagree about which
	// keys authenticate. The audit entry is written below, once
	// the change is durable.
	disabledChanged := false
	if update.Disabled != nil && *update.Disabled != key.Disabled {
		disabledChanged = true
		key.Disabled = *update.Disabled
	}
	// M5.7: the om-usage gate. Same keyMu + persist + rollback
	// as every other field.
	if update.AllowUsageCommand != nil {
		key.AllowUsageCommand = *update.AllowUsageCommand
	}
	// M5.10: access schedule / throttle delay / max sessions.
	// Same keyMu + persist + rollback as every other field.
	// A present schedule replaces the stored one wholesale
	// (an enabled=false object clears the window); absent
	// means unchanged.
	if update.AccessSchedule != nil {
		sch := *update.AccessSchedule
		if sch.Days != nil {
			sch.Days = append([]int(nil), sch.Days...)
		}
		key.AccessSchedule = &sch
	}
	if update.ThrottleDelayMs != nil {
		key.ThrottleDelayMs = *update.ThrottleDelayMs
	}
	if update.MaxSessions != nil {
		key.MaxSessions = *update.MaxSessions
	}
	// G15 Area 4: the merged policy (validated above, before
	// any mutation) replaces the stored policy wholesale.
	if mergedPolicy != nil {
		key.ApplyPolicy(*mergedPolicy)
	}
	persisted := false
	if s.configPersist != nil {
		if err := s.configPersist(); err != nil {
			// M8.14 race fix, round 3: RestoreFrom, not
			// `*s.cfg = *snapshot` — the plain assignment
			// swapped the Routing pointer (and the live
			// mutexes) without holding routingMu. Lock order
			// on this path: keyMu -> pricesMu -> routingMu;
			// no path takes routingMu/pricesMu and then
			// keyMu, so no cycle.
			s.cfg.RestoreFrom(snapshot)
			s.logger.Printf("key update: config persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not save key update to the config file")
			return
		}
		persisted = true
	}
	updated := *key
	view := routerKeyView{
		ID: updated.ID, Name: updated.Name, Scopes: updated.Scopes,
		BudgetUSD: updated.BudgetUSD, CreatedAt: updated.CreatedAt,
		Masked: "crk_••••••••", ExpiresAt: updated.ExpiresAt,
		Expired: updated.Expired(time.Now()), Fingerprint: keyFingerprint(updated),
		RequestsPerMinute: updated.RequestsPerMinute, TokensPerMinute: updated.TokensPerMinute,
		Guardrails:      updated.Guardrails,
		AccessSchedule:  updated.AccessSchedule,
		ThrottleDelayMs: updated.ThrottleDelayMs,
		MaxSessions:     updated.MaxSessions,
	}
	if view.Scopes == nil {
		view.Scopes = []string{}
	}
	resp := keyUpdateResponse{Key: view, Policy: keyPolicyViewOf(updated), PersistedToConfig: persisted}
	if disabledChanged {
		if updated.Disabled {
			s.logger.Printf("key disable: key %s disabled", updated.ID)
		} else {
			s.logger.Printf("key disable: key %s re-enabled", updated.ID)
		}
	}
	if updated.Disabled {
		resp.Note = "the key is now disabled and will no longer authenticate"
	} else if updated.Expired(time.Now()) {
		resp.Note = "the key is now expired and will no longer authenticate"
	} else if !persisted {
		resp.Note = "applied in memory only: no config persist is configured, so the change is lost on restart"
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// DELETE /admin/keys/{id} — permanently revoke one key.
// ---------------------------------------------------------------------------

// keyDeleteResponse is the DELETE /admin/keys/{id} reply.
type keyDeleteResponse struct {
	KeyID             string `json:"key_id"`
	Revoked           bool   `json:"revoked"`
	PersistedToConfig bool   `json:"persisted_to_config"`
	Note              string `json:"note,omitempty"`
}

// handleKeyDelete serves DELETE /admin/keys/{id} (Tier 2): it
// permanently revokes one gateway key — the record is removed from
// the config (persisted), so the key stops authenticating at once
// and does not come back on restart. The typed confirmation phrase
// "DELETE <id>" names the key, so a replayed body cannot revoke a
// different one (the danger-zone pattern, as in provider delete).
// Without a persist function it answers 503 rather than removing
// a key that would reappear on restart while the caller believes
// it revoked. The reversible kill-switch is POST /admin/keys/{id}
// {"disabled":true}; this endpoint is the permanent removal.
func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg == nil {
		writeError(w, http.StatusServiceUnavailable, "key revoke unavailable: no config loaded")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	if _, ok := s.readDangerConfirm(w, r, "DELETE "+id); !ok {
		return
	}
	if s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "key revoke unavailable: config persistence is not configured")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	idx := -1
	for i, k := range s.cfg.GatewayKeys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "unknown gateway key "+id)
		return
	}
	snapshot := s.cfg.Clone()
	s.cfg.GatewayKeys = append(s.cfg.GatewayKeys[:idx], s.cfg.GatewayKeys[idx+1:]...)
	if err := s.configPersist(); err != nil {
		// M8.14 race fix, round 3: RestoreFrom — see the key
		// update rollback above (keyMu -> pricesMu -> routingMu).
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("key revoke: config persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save revoked key to the config file: the key is still valid")
		return
	}
	s.logger.Printf("key revoke: key %s revoked (permanent)", id)
	writeJSON(w, http.StatusOK, keyDeleteResponse{
		KeyID: id, Revoked: true, PersistedToConfig: true,
		Note: "The key no longer authenticates. This is permanent — issue a new key if one is still needed.",
	})
}

// ---------------------------------------------------------------------------
// POST /admin/keys/{id}/rotate — regenerate one key.
// ---------------------------------------------------------------------------

// keyRotateRequest is one POST /admin/keys/{id}/rotate change. Name
// and expiry are optional overrides: absent means the new key
// inherits the old key's name and expiry (ClearExpiry drops the
// expiry instead). Scopes, budget and traffic limits are always
// inherited — rotation replaces the credential, never the policy.
type keyRotateRequest struct {
	Name        string     `json:"name,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ClearExpiry bool       `json:"clear_expiry,omitempty"`
}

// keyRotateResponse is the POST /admin/keys/{id}/rotate reply. The
// raw key is returned exactly once — callers must save it now,
// because only its hash is stored and it is never logged.
type keyRotateResponse struct {
	KeyID       string     `json:"key_id"`
	Name        string     `json:"name"`
	GatewayKey  string     `json:"gateway_key"`
	RotatedFrom string     `json:"rotated_from"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Fingerprint string     `json:"fingerprint"`
	Note        string     `json:"note"`
}

// handleKeyRotate serves POST /admin/keys/{id}/rotate (Tier 2): it
// revokes the named key and issues a fresh one in its place. The
// old key stops authenticating the moment the rotation is applied
// (the verifier reads the live config), and the new key is the
// only one that works afterwards. The raw key is returned once in
// the response and never written to any log. The config file must
// persist — without a persist function the endpoint answers 503
// rather than minting a key that would vanish on restart while
// the old key is already dead.
func (s *Server) handleKeyRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg == nil || s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "key rotation unavailable: config persistence is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req keyRotateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key rotation: expected {\"name\": string (optional), \"expires_at\": \"RFC3339\" (optional), \"clear_expiry\": bool}")
		return
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	idx := -1
	for i, k := range s.cfg.GatewayKeys {
		if k.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, "unknown gateway key "+id)
		return
	}
	old := s.cfg.GatewayKeys[idx]
	name := old.Name
	if strings.TrimSpace(req.Name) != "" {
		name = strings.TrimSpace(req.Name)
	}
	newKey, raw, err := config.IssueKey(name, old.Scopes, old.BudgetUSD)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not issue replacement key")
		return
	}
	newKey.RequestsPerMinute = old.RequestsPerMinute
	newKey.TokensPerMinute = old.TokensPerMinute
	// Rotation replaces the credential, never the policy: the
	// guardrail list is inherited as-is (CHECKLIST 245).
	newKey.Guardrails = append([]string(nil), old.Guardrails...)
	// G15 Area 4: rotation inherits the whole policy, or it
	// would silently drop the key's IP allowlist / model
	// restrictions the moment it is rotated — a security
	// regression, not a credential refresh. The same rule
	// covers the neighbouring per-key fields the old code did
	// not copy (RPD, rate/token limits, access schedule,
	// throttle, max sessions): the documented contract is
	// "scopes, budget and traffic limits are always inherited".
	newKey.RequestsPerDay = old.RequestsPerDay
	newKey.RateLimits = append([]config.RateLimitRule(nil), old.RateLimits...)
	newKey.TokenLimits = append([]config.TokenLimit(nil), old.TokenLimits...)
	if old.AccessSchedule != nil {
		sch := *old.AccessSchedule
		sch.Days = append([]int(nil), old.AccessSchedule.Days...)
		newKey.AccessSchedule = &sch
	}
	newKey.ThrottleDelayMs = old.ThrottleDelayMs
	newKey.MaxSessions = old.MaxSessions
	newKey.AllowUsageCommand = old.AllowUsageCommand
	newKey.ApplyPolicy(old.PolicyOf())
	// A disabled key stays disabled across rotation (M1.3):
	// rotating must not silently re-enable a killed key.
	newKey.Disabled = old.Disabled
	if req.ClearExpiry {
		newKey.ExpiresAt = nil
	} else if req.ExpiresAt != nil {
		exp := *req.ExpiresAt
		newKey.ExpiresAt = &exp
	} else {
		newKey.ExpiresAt = old.ExpiresAt
	}
	snapshot := s.cfg.Clone()
	s.cfg.GatewayKeys = append(s.cfg.GatewayKeys[:idx], s.cfg.GatewayKeys[idx+1:]...)
	s.cfg.GatewayKeys = append(s.cfg.GatewayKeys, newKey)
	if err := s.configPersist(); err != nil {
		// M8.14 race fix, round 3: RestoreFrom — see the key
		// update rollback above (keyMu -> pricesMu -> routingMu).
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("key rotate: config persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save rotated key to the config file: the old key is still valid")
		return
	}
	s.logger.Printf("key rotate: key %s rotated (old key revoked, new key issued)", old.ID)
	writeJSON(w, http.StatusCreated, keyRotateResponse{
		KeyID: newKey.ID, Name: newKey.Name, GatewayKey: raw,
		RotatedFrom: old.ID, ExpiresAt: newKey.ExpiresAt,
		Fingerprint: keyFingerprint(newKey),
		Note:        "Save this key now — it is shown once and cannot be recovered. The old key no longer works. Use it as: Authorization: Bearer <key>.",
	})
}

// ---------------------------------------------------------------------------
// POST /admin/keys — create one gateway key (B1, M10-7 enabler).
// ---------------------------------------------------------------------------

// keyCreateLimits is the "limits" object of POST /admin/keys: the
// per-key traffic caps the GatewayKey record carries. Every field
// is optional; absent means unlimited on that axis (0), matching
// the update endpoint and the dashboard feed. A negative value is
// a 400, never clamped silently. Per-key token limits
// (TokenLimits) and sliding-window rate rules (RateLimits) are NOT
// accepted here — they are separate config shapes owned by other
// slices; an unknown field inside "limits" is a 400 via
// DisallowUnknownFields, so a caller cannot silently believe a
// limit was set when it was ignored.
type keyCreateLimits struct {
	RequestsPerMinute *int `json:"requests_per_minute"`
	TokensPerMinute   *int `json:"tokens_per_minute"`
	RequestsPerDay    *int `json:"requests_per_day"`
}

// keyCreateRequest is one POST /admin/keys body. Name is required.
// Scopes is optional: absent or empty defaults to ["chat",
// "models"] (see handleKeyCreate). BudgetUSD is the key's own spend
// cap in USD (0 = no per-key cap); Budget is the same value under
// the short name the plan's body shape uses — send one of them; if
// both are sent they must agree. ExpiresAt is optional (nil = the
// key never expires), matching rotate/update.
//
// G15 Area 4: Template names a built-in policy template
// (read-only, standard, admin) whose preset is applied first;
// the policy fields below then override the template field by
// field. The policy fields are pointers so "absent" stays
// distinct from "present but empty" (a present empty list clears
// that list). The resolved policy is validated before the key is
// issued.
type keyCreateRequest struct {
	Name      string           `json:"name"`
	Scopes    []string         `json:"scopes"`
	BudgetUSD *float64         `json:"budget_usd"`
	Budget    *float64         `json:"budget"`
	Limits    *keyCreateLimits `json:"limits"`
	ExpiresAt *time.Time       `json:"expires_at"`
	// G15 Area 4 policy (template + overrides).
	Template         *string   `json:"template"`
	IPAllowlist      *[]string `json:"ip_allowlist"`
	IPBlocklist      *[]string `json:"ip_blocklist"`
	ModelAccessMode  *string   `json:"model_access_mode"`
	AllowedModels    *[]string `json:"allowed_models"`
	BlockedModels    *[]string `json:"blocked_models"`
	AllowedEndpoints *[]string `json:"allowed_endpoints"`
	ForcedModel      *string   `json:"forced_model"`
	Guardrails       *[]string `json:"guardrails"`
}

// policyRequest renders this create request's policy section as
// a keyPolicyRequest for resolve().
func (req keyCreateRequest) policyRequest() *keyPolicyRequest {
	return &keyPolicyRequest{
		Template: req.Template, IPAllowlist: req.IPAllowlist,
		IPBlocklist: req.IPBlocklist, ModelAccessMode: req.ModelAccessMode,
		AllowedModels: req.AllowedModels, BlockedModels: req.BlockedModels,
		AllowedEndpoints: req.AllowedEndpoints, ForcedModel: req.ForcedModel,
	}
}

// keyCreateResponse is the POST /admin/keys reply. The raw key is
// returned exactly once, in GatewayKey — callers must save it now,
// because only its hash is stored and it is never logged. The Key
// record is the same routerKeyView the dashboard feed shows: it
// carries the masked value and the fingerprint, never the raw key
// or the full hash.
type keyCreateResponse struct {
	KeyID             string        `json:"key_id"`
	Name              string        `json:"name"`
	GatewayKey        string        `json:"gateway_key"`
	Key               routerKeyView `json:"key"`
	Fingerprint       string        `json:"fingerprint"`
	Policy            keyPolicyView `json:"policy"`
	PersistedToConfig bool          `json:"persisted_to_config"`
	Note              string        `json:"note"`
}

// validGatewayScope reports whether scope is a scope a new key may
// carry: one of the HTTP scopes ("chat", "models", "admin" — the
// only scopes the codebase issues today, see the vocabulary comment
// on TestScopeEnforcedTableDriven), or a valid MCP grant (a known
// mcp: scope, or a trailing-* wildcard covering one — validMCPGrant,
// the same check the MCP transport applies at request time). The
// bare "*" local-operator grant is rejected by validMCPGrant: it
// must never reach a network identity.
func validGatewayScope(scope string) bool {
	switch scope {
	case "chat", "models", "admin":
		return true
	}
	return validMCPGrant(scope)
}

// handleKeyCreate serves POST /admin/keys (Tier 2, admin scope —
// enforced by the guard table at registration, like rotate/revoke;
// guard row in guard.go, registered in server.go):
// it mints one new gateway key via config.IssueKey — the same
// key-issue path bootstrap uses (bootstrapIssuer in
// cmd/codvian-router/main.go) — appends the record to the config
// and persists it. The raw key is returned once in the response
// and never written to any log (only the key id is logged). The
// config file must persist — without a persist function the
// endpoint answers 503 rather than minting a key that would vanish
// on restart while the caller believes it exists. A persist
// failure rolls the in-memory config back and answers 500, so the
// running server and the file never disagree about which keys exist.
//
// Scope default: an absent or empty scopes list means
// ["chat", "models"] — the same default the headless-run wizard
// issues for a working key. Bootstrap's ["chat", "models",
// "admin"] is the first-operator-key default, not a general one;
// an admin key must be asked for explicitly. A key with no scopes
// at all would authenticate yet be able to call nothing, so the
// default keeps a created key usable instead of silently useless.
func (s *Server) handleKeyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.cfg == nil || s.configPersist == nil {
		writeError(w, http.StatusServiceUnavailable, "key creation unavailable: config persistence is not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req keyCreateRequest
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid key creation: expected {\"name\": string, \"scopes\": [string] (optional), \"budget_usd\": number (optional), \"limits\": {\"requests_per_minute\": number, \"tokens_per_minute\": number, \"requests_per_day\": number} (optional), \"expires_at\": \"RFC3339\" (optional)}")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// G15 Area 4: a named template presets scopes, budget,
	// limits, guardrails and policy; every explicit field in
	// the body overrides the template field by field. An
	// unknown template name is a 400 naming the name.
	var tmpl keyTemplate
	hasTemplate := false
	if req.Template != nil {
		var ok bool
		tmpl, ok = findKeyTemplate(strings.TrimSpace(*req.Template))
		if !ok {
			writeError(w, http.StatusBadRequest, "unknown key template "+*req.Template)
			return
		}
		hasTemplate = true
	}
	scopes := req.Scopes
	if len(scopes) == 0 {
		if hasTemplate {
			scopes = tmpl.Scopes
		} else {
			scopes = []string{"chat", "models"}
		}
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		if sc == "" {
			writeError(w, http.StatusBadRequest, "scopes must not contain an empty scope")
			return
		}
		if _, dup := seen[sc]; dup {
			writeError(w, http.StatusBadRequest, "duplicate scope "+sc)
			return
		}
		seen[sc] = struct{}{}
		if !validGatewayScope(sc) {
			writeError(w, http.StatusBadRequest, "unknown scope "+sc+": scopes are \"chat\", \"models\", \"admin\", or a valid mcp: scope (e.g. \"mcp:read:cost\", \"mcp:read:*\")")
			return
		}
	}
	var budget float64
	if req.BudgetUSD != nil && req.Budget != nil && *req.BudgetUSD != *req.Budget {
		writeError(w, http.StatusBadRequest, "budget and budget_usd disagree: send one of them")
		return
	}
	if req.BudgetUSD != nil {
		budget = *req.BudgetUSD
	} else if req.Budget != nil {
		budget = *req.Budget
	} else if hasTemplate {
		budget = tmpl.BudgetUSD
	}
	if budget < 0 {
		writeError(w, http.StatusBadRequest, "budget must not be negative")
		return
	}
	// G15 Area 4: the resolved policy (template first, explicit
	// fields overriding) is validated before anything is issued
	// — an invalid IP entry or model mode is a 400, never a
	// stored-but-unenforced policy.
	policy, err := req.policyRequest().resolve(true)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid key policy: "+err.Error())
		return
	}
	guardrailsList := []string(nil)
	if req.Guardrails != nil {
		guardrailsList = *req.Guardrails
	} else if hasTemplate {
		guardrailsList = tmpl.Guardrails
	}
	if len(guardrailsList) > 0 {
		if err := s.guardrails.Config().ValidateGuardrailNames(guardrailsList); err != nil {
			s.guardrails.RecordSecurityRefusal(guardrails.SecurityRefusalUnknownGuardrail)
			writeError(w, http.StatusBadRequest, "invalid key creation: "+err.Error())
			return
		}
	}
	var rpm, tpm, rpd int
	if hasTemplate {
		rpm, tpm, rpd = tmpl.RequestsPerMinute, tmpl.TokensPerMinute, tmpl.RequestsPerDay
	}
	if req.Limits != nil {
		if req.Limits.RequestsPerMinute != nil {
			if *req.Limits.RequestsPerMinute < 0 {
				writeError(w, http.StatusBadRequest, "limits.requests_per_minute must not be negative")
				return
			}
			rpm = *req.Limits.RequestsPerMinute
		}
		if req.Limits.TokensPerMinute != nil {
			if *req.Limits.TokensPerMinute < 0 {
				writeError(w, http.StatusBadRequest, "limits.tokens_per_minute must not be negative")
				return
			}
			tpm = *req.Limits.TokensPerMinute
		}
		if req.Limits.RequestsPerDay != nil {
			if *req.Limits.RequestsPerDay < 0 {
				writeError(w, http.StatusBadRequest, "limits.requests_per_day must not be negative")
				return
			}
			rpd = *req.Limits.RequestsPerDay
		}
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	// The bootstrap key-issue path: IssueKey mints the raw key and
	// returns the record holding only its SHA-256 hash. No second
	// key-generation path exists anywhere in this handler.
	newKey, raw, err := config.IssueKey(name, scopes, budget)
	if err != nil {
		// Unreachable after the validation above (IssueKey fails
		// only on an empty name or a negative budget), but a
		// failure here must never mint a half-created key.
		writeServerError(w, http.StatusInternalServerError, "could not issue gateway key")
		return
	}
	newKey.RequestsPerMinute = rpm
	newKey.TokensPerMinute = tpm
	newKey.RequestsPerDay = rpd
	// G15 Area 4: template preset + explicit overrides land on
	// the record before it is stored (rotation inherits them
	// from here on — see handleKeyRotate).
	newKey.ApplyPolicy(policy)
	newKey.Guardrails = append([]string(nil), guardrailsList...)
	if hasTemplate {
		newKey.ThrottleDelayMs = tmpl.ThrottleDelayMs
		newKey.MaxSessions = tmpl.MaxSessions
	}
	if req.ExpiresAt != nil {
		exp := *req.ExpiresAt
		newKey.ExpiresAt = &exp
	}
	snapshot := s.cfg.Clone()
	if err := s.cfg.AddGatewayKey(newKey); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not store gateway key")
		return
	}
	if err := s.configPersist(); err != nil {
		// M8.14 race fix, round 3: RestoreFrom — see the key
		// update rollback above (keyMu -> pricesMu -> routingMu).
		s.cfg.RestoreFrom(snapshot)
		s.logger.Printf("key create: config persist failed, config rolled back: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not save new key to the config file")
		return
	}
	// Log the id only — never the raw key.
	s.logger.Printf("key create: key %s created", newKey.ID)
	view := routerKeyView{
		ID: newKey.ID, Name: newKey.Name, Scopes: newKey.Scopes,
		BudgetUSD: newKey.BudgetUSD, CreatedAt: newKey.CreatedAt,
		Masked: "crk_••••••••", ExpiresAt: newKey.ExpiresAt,
		Expired: newKey.Expired(time.Now()), Fingerprint: keyFingerprint(newKey),
		RequestsPerMinute: newKey.RequestsPerMinute, TokensPerMinute: newKey.TokensPerMinute,
	}
	if view.Scopes == nil {
		view.Scopes = []string{}
	}
	writeJSON(w, http.StatusCreated, keyCreateResponse{
		KeyID: newKey.ID, Name: newKey.Name, GatewayKey: raw, Key: view,
		Fingerprint: keyFingerprint(newKey), Policy: keyPolicyViewOf(newKey),
		PersistedToConfig: true,
		Note:              "Save this key now — it is shown once and cannot be recovered. Use it as: Authorization: Bearer <key>.",
	})
}
