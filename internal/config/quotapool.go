// Fair-share quota pools (M5.4, the reference router lib/quota/dimensions.ts):
// a pool shares one quota across its member gateway keys. The
// config file declares pools; the ledger stores them (see
// internal/ledger/pool.go) and the server syncs config → ledger
// at startup, the same way key_budgets are synced.
package config

// QuotaPoolDimensionConfig is one (unit x window) axis of a pool:
// the global pool limit for that unit inside that window. Unit is
// requests | tokens | usd — "percent" is rejected (it has no
// meaning without an external quota to take a percentage of).
// Window is 5h | hourly | daily | weekly | monthly.
type QuotaPoolDimensionConfig struct {
	Unit   string  `json:"unit"`
	Window string  `json:"window"`
	Limit  float64 `json:"limit"`
}

// QuotaPoolAllocationConfig is one member key's share of a pool:
// Weight 0-100 (its share of the pool), an optional absolute Cap
// (CapValue in CapUnit — the cap fires only on dimensions of that
// unit), and Policy hard | soft | burst ("" = hard).
type QuotaPoolAllocationConfig struct {
	KeyID    string   `json:"key_id"`
	Weight   float64  `json:"weight"`
	CapValue *float64 `json:"cap_value,omitempty"`
	CapUnit  string   `json:"cap_unit,omitempty"`
	Policy   string   `json:"policy,omitempty"`
}

// QuotaPoolConfig is one fair-share pool. SaturationThreshold
// 0-1 (0 = the 0.5 default) flips generous mode into strict mode
// once the pool's global usage reaches that fraction of the limit.
// AccountCount is the number of upstream accounts backing the
// pool (M5.5): the dimension limit is per-account, and the pool's
// effective limit is limit × accounts. 0 = 1 (a single account).
type QuotaPoolConfig struct {
	ID                  string                      `json:"id"`
	Name                string                      `json:"name"`
	SaturationThreshold float64                     `json:"saturation_threshold,omitempty"`
	AccountCount        int                         `json:"account_count,omitempty"`
	Dimensions          []QuotaPoolDimensionConfig  `json:"dimensions"`
	Allocations         []QuotaPoolAllocationConfig `json:"allocations"`
}
