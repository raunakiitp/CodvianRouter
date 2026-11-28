package config

// M5.11: internal scoped budgets (config side) — the operator's
// own caps on total spend (the sector baseline lib/usage/budgetGuard.ts:
// InternalBudgetLimit). At startup the entries are applied into
// the ledger (the durable store), and the dashboard's POST
// /dashboard/internal-budgets writes both the ledger rows and
// this block, so the two never drift apart.
//
// Scope is global (every key's spend) / provider / model / pool;
// Period is a UTC calendar window (daily / weekly / monthly);
// LimitType is currency (USD) / tokens / requests.
// WarningThreshold is the used fraction (0-1) at which the
// evaluation reports warn instead of allow; 0 means the default
// 0.8 (the same rule as the key budgets).
type InternalBudgetConfig struct {
	ID               string  `json:"id"`
	Scope            string  `json:"scope"`
	ProviderID       string  `json:"provider_id,omitempty"`
	ModelID          string  `json:"model_id,omitempty"`
	PoolID           string  `json:"pool_id,omitempty"`
	Period           string  `json:"period"`
	LimitType        string  `json:"limit_type"`
	LimitValue       float64 `json:"limit_value"`
	WarningThreshold float64 `json:"warning_threshold"`
	Enabled          bool    `json:"enabled"`
}
