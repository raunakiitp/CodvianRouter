package ledger

import (
	"context"
	"fmt"
	"time"
)

// M5.6: per-model usage slices for self-service reporting
// (me/status list-price cost, me/sessions). OmniRoute's
// listApiKeyUsageSlices returns the same shape — one row per
// (provider, model) in the window — and summarizeListPrice
// reprices each slice at the catalog's list prices. The metered
// cost (CostUSD) is what the operator's price book charged at
// record time; the list-price cost is computed by the caller
// from the token split and the catalog, so a price change never
// rewrites history.

// ModelUsageSlice is one (provider, model) row of a key's usage
// in a window: successful, non-shared rows only (ok=1, shared=0).
// PriceKnown is false when ANY row in the group had no metered
// price (price_known=0); SplitKnown is false when ANY row had an
// unknown prompt/completion split — such a row cannot be repriced
// at list prices (input vs output rates differ), so the caller
// must mark the window's list price incomplete.
type ModelUsageSlice struct {
	Provider         string
	Model            string
	Rows             int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	PriceKnown       bool
	SplitKnown       bool
}

// UsageSlices returns the per-(provider, model) usage of one key
// in [start, end). Empty (not an error) when the key used nothing
// in the window.
func (l *Ledger) UsageSlices(ctx context.Context, keyID string, start, end time.Time) ([]ModelUsageSlice, error) {
	rows, err := l.db.QueryContext(ctx, `
		SELECT provider, model, COUNT(*),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(total_tokens), 0),
		       COALESCE(SUM(cost_usd), 0),
		       MIN(price_known), MIN(tokens_split_known)
		FROM usage_events
		WHERE gateway_key_id = ? AND ok = 1 AND shared = 0
		  AND ts >= ? AND ts < ?
		GROUP BY provider, model
		ORDER BY provider, model`,
		keyID, start.UnixMilli(), end.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("ledger: usage slices for key: %w", err)
	}
	defer rows.Close()
	var out []ModelUsageSlice
	for rows.Next() {
		var s ModelUsageSlice
		var priceKnown, splitKnown int
		if err := rows.Scan(&s.Provider, &s.Model, &s.Rows, &s.PromptTokens, &s.CompletionTokens, &s.TotalTokens, &s.CostUSD, &priceKnown, &splitKnown); err != nil {
			return nil, fmt.Errorf("ledger: usage slices scan: %w", err)
		}
		s.PriceKnown = priceKnown == 1
		s.SplitKnown = splitKnown == 1
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: usage slices rows: %w", err)
	}
	return out, nil
}
