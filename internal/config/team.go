package config

// TeamConfig is one cost center in the config file (M5.12): a named
// grouping of gateway keys with an optional shared rolling budget.
// Members are key ids; a key belongs to at most one team. Status is
// "active" or "archived" ("" = active); an archived team refuses new
// members and enforces no budget.
type TeamConfig struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Description    string   `json:"description,omitempty"`
	Status         string   `json:"status,omitempty"`
	MaxBudgetUSD   *float64 `json:"max_budget_usd,omitempty"`
	BudgetDuration string   `json:"budget_duration,omitempty"` // "", "1d", "7d", "30d"
	Members        []string `json:"members,omitempty"`
}
