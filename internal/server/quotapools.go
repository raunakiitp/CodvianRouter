// Fair-share quota pools (M5.4): the server side — the pre-call
// gate on the chat paths, and the GET/POST /dashboard/quota-pools
// feeds for the operator.
//
// The gate runs BEFORE any provider is called and before the
// traffic check (a refused request consumes no traffic slot): for
// every pool the caller's key belongs to, the ledger decides
// (see internal/ledger/pool.go — the reference router's decideFairShare).
// A block is a 429 + Retry-After to the window reset, error type
// "key_pool_limit". A soft-policy key allowed past its fair share
// is still served, with the X-Codvian-FairShare-Penalized header
// naming the pool so the operator can see the pressure. A ledger
// read failure fails closed with a 503: a limit that cannot be
// read must not be waived. Skipped entirely with no ledger, no
// verifiable key, or no pools for the key.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// checkQuotaPools enforces the caller's fair-share pools. It
// writes the refusal and returns true when the request must stop
// here, before any provider is called.
func (s *Server) checkQuotaPools(w http.ResponseWriter, r *http.Request) bool {
	if s.ledger == nil {
		return false
	}
	keyID := s.callerKeyID(r)
	if keyID == "" {
		return false
	}
	ctx := r.Context()
	now := time.Now()
	pools, err := s.ledger.PoolsForKey(ctx, keyID)
	if err != nil {
		s.logger.Printf("quota-pool: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusServiceUnavailable, "quota pool check unavailable")
		return true
	}
	for _, pool := range pools {
		block, penalized, err := s.ledger.CheckPoolFairShare(ctx, keyID, pool, now)
		if err != nil {
			s.logger.Printf("quota-pool: check failed for caller: %v", guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusServiceUnavailable, "quota pool check unavailable")
			return true
		}
		if block != nil {
			seconds := int(time.Until(block.NextReset) / time.Second)
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
			writeJSON(w, http.StatusTooManyRequests, errorBody{Error: errorDetail{Message: block.Error(), Type: "key_pool_limit"}})
			return true
		}
		if penalized {
			w.Header().Set("X-Codvian-FairShare-Penalized", pool.ID)
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// GET/POST /dashboard/quota-pools.
// ---------------------------------------------------------------------------

// quotaPoolDimensionView is one dimension of a pool with its live
// pool-wide consumption in the current window. Limit is the
// EFFECTIVE limit (per-account limit × backing accounts, M5.5) —
// what enforcement actually caps at.
type quotaPoolDimensionView struct {
	Unit            string  `json:"unit"`
	Window          string  `json:"window"`
	Limit           float64 `json:"limit"`
	PerAccountLimit float64 `json:"per_account_limit"`
	AccountCount    int     `json:"account_count"`
	Consumed        float64 `json:"consumed"`
	NextReset       string  `json:"next_reset_at"`
	UsedPercent     float64 `json:"used_percent"`
}

// quotaPoolAllocationView is one member key's share of a pool.
type quotaPoolAllocationView struct {
	KeyID    string   `json:"key_id"`
	Weight   float64  `json:"weight"`
	CapValue *float64 `json:"cap_value,omitempty"`
	CapUnit  string   `json:"cap_unit,omitempty"`
	Policy   string   `json:"policy"`
}

// quotaPoolView is one row of the GET /dashboard/quota-pools payload.
type quotaPoolView struct {
	ID                  string                    `json:"id"`
	Name                string                    `json:"name"`
	SaturationThreshold float64                   `json:"saturation_threshold"`
	Dimensions          []quotaPoolDimensionView  `json:"dimensions"`
	Allocations         []quotaPoolAllocationView `json:"allocations"`
}

// quotaPoolsResponse is the GET /dashboard/quota-pools payload.
type quotaPoolsResponse struct {
	Pools []quotaPoolView `json:"pools"`
}

// handleQuotaPools serves GET /dashboard/quota-pools: every pool
// with its dimensions (limit + live pool-wide consumption in the
// current window) and allocations. With no ledger wired it
// answers an empty list.
func (s *Server) handleQuotaPools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := quotaPoolsResponse{Pools: []quotaPoolView{}}
	if s.ledger == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	ctx := r.Context()
	now := time.Now()
	pools, err := s.ledger.ListQuotaPools(ctx)
	if err != nil {
		s.logger.Printf("quota-pool: list failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not list quota pools")
		return
	}
	for _, pool := range pools {
		view := quotaPoolView{
			ID:                  pool.ID,
			Name:                pool.Name,
			SaturationThreshold: pool.SaturationThreshold,
			Dimensions:          []quotaPoolDimensionView{},
			Allocations:         []quotaPoolAllocationView{},
		}
		if view.SaturationThreshold == 0 {
			view.SaturationThreshold = 0.5
		}
		for _, dim := range pool.Dimensions {
			used, nextReset, err := s.ledger.PoolDimensionStatus(ctx, pool, dim, now)
			if err != nil {
				s.logger.Printf("quota-pool: status failed for pool %s: %v", pool.ID, guardrails.RedactLogLine(err.Error()))
				writeServerError(w, http.StatusInternalServerError, "could not read quota pool status")
				return
			}
			effectiveLimit := pool.EffectiveLimit(dim)
			usedPercent := 0.0
			if effectiveLimit > 0 {
				usedPercent = used / effectiveLimit
			}
			view.Dimensions = append(view.Dimensions, quotaPoolDimensionView{
				Unit:            dim.Unit,
				Window:          dim.Window,
				Limit:           effectiveLimit,
				PerAccountLimit: dim.Limit,
				AccountCount:    pool.EffectiveAccountCount(),
				Consumed:        used,
				NextReset:       nextReset.UTC().Format(time.RFC3339),
				UsedPercent:     usedPercent,
			})
		}
		for _, a := range pool.Allocations {
			view.Allocations = append(view.Allocations, quotaPoolAllocationView{
				KeyID:    a.KeyID,
				Weight:   a.Weight,
				CapValue: a.CapValue,
				CapUnit:  a.CapUnit,
				Policy:   a.Policy,
			})
		}
		out.Pools = append(out.Pools, view)
	}
	writeJSON(w, http.StatusOK, out)
}

// quotaPoolUpdate is the POST /dashboard/quota-pools body: a full
// pool upsert (dimensions/allocations replace wholesale when
// present; absent fields keep their stored values), or a delete.
type quotaPoolUpdate struct {
	ID                  string                              `json:"id"`
	Name                *string                             `json:"name,omitempty"`
	SaturationThreshold *float64                            `json:"saturation_threshold,omitempty"`
	AccountCount        *int                                `json:"account_count,omitempty"`
	Dimensions          *[]config.QuotaPoolDimensionConfig  `json:"dimensions,omitempty"`
	Allocations         *[]config.QuotaPoolAllocationConfig `json:"allocations,omitempty"`
	Delete              bool                                `json:"delete"`
}

// poolConfigToLedger maps a config pool to the ledger shape.
func poolConfigToLedger(p config.QuotaPoolConfig) ledger.QuotaPool {
	out := ledger.QuotaPool{
		ID:                  p.ID,
		Name:                p.Name,
		SaturationThreshold: p.SaturationThreshold,
		AccountCount:        p.AccountCount,
		Dimensions:          []ledger.QuotaDimension{},
		Allocations:         []ledger.PoolAllocation{},
	}
	for _, d := range p.Dimensions {
		out.Dimensions = append(out.Dimensions, ledger.QuotaDimension{Unit: d.Unit, Window: d.Window, Limit: d.Limit})
	}
	for _, a := range p.Allocations {
		out.Allocations = append(out.Allocations, ledger.PoolAllocation{
			KeyID: a.KeyID, Weight: a.Weight, CapValue: a.CapValue, CapUnit: a.CapUnit, Policy: a.Policy,
		})
	}
	return out
}

// poolLedgerToConfig maps a ledger pool to the config shape.
func poolLedgerToConfig(p ledger.QuotaPool) config.QuotaPoolConfig {
	out := config.QuotaPoolConfig{
		ID:                  p.ID,
		Name:                p.Name,
		SaturationThreshold: p.SaturationThreshold,
		AccountCount:        p.AccountCount,
		Dimensions:          []config.QuotaPoolDimensionConfig{},
		Allocations:         []config.QuotaPoolAllocationConfig{},
	}
	for _, d := range p.Dimensions {
		out.Dimensions = append(out.Dimensions, config.QuotaPoolDimensionConfig{Unit: d.Unit, Window: d.Window, Limit: d.Limit})
	}
	for _, a := range p.Allocations {
		out.Allocations = append(out.Allocations, config.QuotaPoolAllocationConfig{
			KeyID: a.KeyID, Weight: a.Weight, CapValue: a.CapValue, CapUnit: a.CapUnit, Policy: a.Policy,
		})
	}
	return out
}

// validateQuotaPoolUpdate checks one pool body the same way the
// config validation does: known units/windows/policies, positive
// limits and caps, weights 0-100, allocations naming existing
// keys (when a config is wired), no duplicate dimensions or keys.
func (s *Server) validateQuotaPoolUpdate(p config.QuotaPoolConfig) error {
	if p.ID == "" {
		return errors.New("id is required")
	}
	if p.Name == "" {
		return errors.New("name must not be empty")
	}
	if p.SaturationThreshold < 0 || p.SaturationThreshold > 1 {
		return fmt.Errorf("saturation_threshold must be between 0 and 1 (got %v)", p.SaturationThreshold)
	}
	if p.AccountCount < 0 {
		return fmt.Errorf("account_count must not be negative (got %d)", p.AccountCount)
	}
	if len(p.Dimensions) == 0 {
		return errors.New("at least one dimension is required")
	}
	seenDims := map[string]bool{}
	for i, d := range p.Dimensions {
		switch d.Unit {
		case "requests", "tokens", "usd":
		default:
			return fmt.Errorf("dimensions[%d].unit %q is not a known unit (requests, tokens, usd)", i, d.Unit)
		}
		switch d.Window {
		case "5h", "hourly", "daily", "weekly", "monthly":
		default:
			return fmt.Errorf("dimensions[%d].window %q is not a known window (5h, hourly, daily, weekly, monthly)", i, d.Window)
		}
		if d.Limit <= 0 {
			return fmt.Errorf("dimensions[%d].limit must be positive (got %v)", i, d.Limit)
		}
		key := d.Unit + ":" + d.Window
		if seenDims[key] {
			return fmt.Errorf("duplicate dimension %q", key)
		}
		seenDims[key] = true
	}
	seenKeys := map[string]bool{}
	for i, a := range p.Allocations {
		if a.KeyID == "" {
			return fmt.Errorf("allocations[%d] has an empty key_id", i)
		}
		if seenKeys[a.KeyID] {
			return fmt.Errorf("key %q is allocated twice", a.KeyID)
		}
		seenKeys[a.KeyID] = true
		if s.cfg != nil {
			if _, ok := s.cfg.FindGatewayKey(a.KeyID); !ok {
				return fmt.Errorf("allocations[%d] names a gateway key that does not exist (%q)", i, a.KeyID)
			}
		}
		if a.Weight < 0 || a.Weight > 100 {
			return fmt.Errorf("allocations[%d].weight must be between 0 and 100 (got %v)", i, a.Weight)
		}
		switch a.Policy {
		case "", "hard", "soft", "burst":
		default:
			return fmt.Errorf("allocations[%d].policy %q is not a known policy (hard, soft, burst)", i, a.Policy)
		}
		if a.CapValue != nil {
			if *a.CapValue <= 0 {
				return fmt.Errorf("allocations[%d].cap_value must be positive (got %v)", i, *a.CapValue)
			}
			switch a.CapUnit {
			case "requests", "tokens", "usd":
			default:
				return fmt.Errorf("allocations[%d].cap_unit %q is not a known unit (requests, tokens, usd)", i, a.CapUnit)
			}
		} else if a.CapUnit != "" {
			return fmt.Errorf("allocations[%d].cap_unit must be empty without a cap_value", i)
		}
	}
	return nil
}

// handleQuotaPoolUpdate serves POST /dashboard/quota-pools. The
// ledger rows are written first, then the config file's
// quota_pools block (when a config + persist function are wired)
// so the two never disagree — a config save failure rolls the
// ledger change back and answers 500, exactly like the key
// budgets. Without a config wired the pool lives in the ledger
// only, which is still the durable store: it survives a restart.
func (s *Server) handleQuotaPoolUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "quota pools unavailable: no ledger wired")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update quotaPoolUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid quota pool update: expected {\"id\": string, \"name\": string, \"saturation_threshold\": number, \"dimensions\": [...], \"allocations\": [...], \"delete\": bool}")
		return
	}
	if update.ID == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	ctx := r.Context()
	prev, prevErr := s.ledger.GetQuotaPool(ctx, update.ID)
	hadPrev := prevErr == nil
	if update.Delete {
		if err := s.ledger.DeleteQuotaPool(ctx, update.ID); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not delete quota pool")
			return
		}
	} else {
		next := config.QuotaPoolConfig{ID: update.ID}
		if hadPrev {
			next = poolLedgerToConfig(prev)
		}
		if update.Name != nil {
			next.Name = *update.Name
		}
		if update.SaturationThreshold != nil {
			next.SaturationThreshold = *update.SaturationThreshold
		}
		if update.AccountCount != nil {
			next.AccountCount = *update.AccountCount
		}
		if update.Dimensions != nil {
			next.Dimensions = *update.Dimensions
		}
		if update.Allocations != nil {
			next.Allocations = *update.Allocations
		}
		if err := s.validateQuotaPoolUpdate(next); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := s.ledger.SetQuotaPool(ctx, poolConfigToLedger(next)); err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not save quota pool")
			return
		}
	}
	// Config block: keep the file in step with the ledger, or
	// roll the ledger back when the save fails.
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.QuotaPoolConfig(nil), s.cfg.QuotaPools...)
		if update.Delete {
			kept := s.cfg.QuotaPools[:0]
			for _, p := range s.cfg.QuotaPools {
				if p.ID != update.ID {
					kept = append(kept, p)
				}
			}
			s.cfg.QuotaPools = kept
		} else {
			next := config.QuotaPoolConfig{ID: update.ID}
			for _, p := range s.cfg.QuotaPools {
				if p.ID == update.ID {
					next = p
					break
				}
			}
			if update.Name != nil {
				next.Name = *update.Name
			}
			if update.SaturationThreshold != nil {
				next.SaturationThreshold = *update.SaturationThreshold
			}
			if update.AccountCount != nil {
				next.AccountCount = *update.AccountCount
			}
			if update.Dimensions != nil {
				next.Dimensions = *update.Dimensions
			}
			if update.Allocations != nil {
				next.Allocations = *update.Allocations
			}
			found := false
			for i, p := range s.cfg.QuotaPools {
				if p.ID == update.ID {
					s.cfg.QuotaPools[i] = next
					found = true
					break
				}
			}
			if !found {
				s.cfg.QuotaPools = append(s.cfg.QuotaPools, next)
			}
		}
		if err := s.configPersist(); err != nil {
			// Roll the ledger back: runtime and file must never
			// disagree about a quota pool.
			s.cfg.QuotaPools = prevCfg
			if hadPrev {
				_ = s.ledger.SetQuotaPool(ctx, prev)
			} else {
				_ = s.ledger.DeleteQuotaPool(ctx, update.ID)
			}
			writeServerError(w, http.StatusInternalServerError, "could not persist quota pool config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
