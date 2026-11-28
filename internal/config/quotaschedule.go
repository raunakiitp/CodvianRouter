package config

import "github.com/raunakiitp/codvian-router/internal/webhooks"

// M5.9: quota schedules (config side) + webhook endpoints.
//
// QuotaScheduleConfig is the operator-configured time-aware quota
// window on a pool (the reference router lib/quota/schedules.ts). At startup
// the entries are applied into the ledger (the durable store),
// and the dashboard's POST /dashboard/quota-schedules writes
// both the ledger rows and this block, so the two never drift
// apart. Days are JS getDay() values, 0 = Sunday .. 6 =
// Saturday; start/end are minutes from local midnight (end <=
// start wraps past midnight); mode is "allow" (whitelist) or
// "block" (blackout).
type QuotaScheduleConfig struct {
	ID           string            `json:"id"`
	PoolID       string            `json:"pool_id"`
	Label        string            `json:"label,omitempty"`
	Days         []int             `json:"days,omitempty"`
	StartMinute  int               `json:"start_minute"`
	EndMinute    int               `json:"end_minute"`
	Timezone     string            `json:"timezone,omitempty"`
	Mode         string            `json:"mode"`
	Reserves     []ScheduleReserve `json:"reserves,omitempty"`
	BudgetValue  *float64          `json:"budget_value,omitempty"`
	BudgetUnit   string            `json:"budget_unit,omitempty"`
	BudgetWindow string            `json:"budget_window,omitempty"`
	Priority     int               `json:"priority"`
	Enabled      bool              `json:"enabled"`
}

// ScheduleReserve is one "keep this much free" rule: the free
// fraction of the pool's quota in Window must stay at or above
// Percent (0-100). Window "any" means the most-consumed window.
type ScheduleReserve struct {
	Window  string  `json:"window"`
	Percent float64 `json:"percent"`
}

// WebhookConfig is one outbound webhook endpoint. Events is the
// list of event names this endpoint subscribes to; Secret, when
// set, signs each delivery with HMAC-SHA256 (header
// X-Codvian-Signature-256). Deliveries are best-effort and never
// block the request that triggered them.
type WebhookConfig struct {
	ID     string   `json:"id"`
	URL    string   `json:"url"`
	Events []string `json:"events,omitempty"`
	Secret string   `json:"secret,omitempty"`
	// Kind selects the delivery payload shape. "" (default) is the
	// generic signed JSON envelope. "slack", "discord" and "telegram"
	// are chat integrations (internal/webhooks/integrations): the body
	// is built in the platform format and is NOT HMAC-signed. An
	// unknown non-empty kind is rejected at validation.
	Kind string `json:"kind,omitempty"`
	// Metadata carries optional per-endpoint secrets for chat kinds.
	// For telegram it holds a sealed bot token under "bot_token"
	// (unsealed via the config Vault at delivery); the destination
	// chat_id always lives in the endpoint URL.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// QuotaExceededEvent is the webhook event for a quota threshold
// being reached ("quota.exceeded": "A usage threshold (e.g.
// 95% of quota) was reached."). It fires, best-effort, whenever
// a quota schedule blocks a request (blackout, outside-window,
// keep-free reserve, schedule budget).
//
// These constants are ALIASES of the canonical names in
// internal/webhooks; the config-file validator accepts exactly
// webhooks.KnownEvents() (see KnownWebhookEvents), so the config set
// and the registration API set are the same set by construction.
const QuotaExceededEvent = webhooks.QuotaExceededEvent

// BudgetWarningEvent: an internal budget crossed its warning threshold.
// It fires, best-effort and edge-triggered (once per crossing), from
// the budget machinery via notifyBudgetWarning.
const BudgetWarningEvent = webhooks.BudgetWarningEvent

// ProviderDegradedEvent / ProviderRecoveredEvent are edge-triggered
// provider-health signals fired from genuine circuit-breaker
// CLOSED->OPEN / OPEN->CLOSED transitions (see
// router.Engine.SetBreakerEvents and the server adapter that turns
// them into dispatcher.Notify calls).
const (
	ProviderDegradedEvent  = webhooks.ProviderDegradedEvent
	ProviderRecoveredEvent = webhooks.ProviderRecoveredEvent
)

// KnownWebhookEvents returns the canonical webhook event set the
// config-file validator accepts. It is the SAME set the registration
// API accepts (both delegate to webhooks.KnownEvents), so a persisted
// subscription that the API accepted always survives the next
// startup's config validation.
func KnownWebhookEvents() []string { return webhooks.KnownEvents() }

// knownWebhookEvent reports whether ev is in the canonical set.
func knownWebhookEvent(ev string) bool {
	for _, k := range webhooks.KnownEvents() {
		if ev == k {
			return true
		}
	}
	return false
}
