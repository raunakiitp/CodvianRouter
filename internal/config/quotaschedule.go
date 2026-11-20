package config

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
}

// QuotaExceededEvent is one of the two webhook events emitted
// today (M5.9, the reference router "quota.exceeded": "A usage threshold (e.g.
// 95% of quota) was reached."). It fires, best-effort, whenever
// a quota schedule blocks a request (blackout, outside-window,
// keep-free reserve, schedule budget).
const QuotaExceededEvent = "quota.exceeded"

// BudgetWarningEvent is the second webhook event emitted today
// (M8.19): an internal budget crossed its warning threshold.
// It fires, best-effort and edge-triggered (once per crossing),
// from the budget machinery via notifyBudgetWarning.
const BudgetWarningEvent = "budget.warning"
