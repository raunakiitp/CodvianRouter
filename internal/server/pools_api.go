package server

// Quota-pools management API (M8.18): POST/GET /api/quota-pools
// and GET/PATCH/DELETE /api/quota-pools/{id}.
//
// A management face over the M5.4/M5.5 fair-share pools that
// already exist in the ledger (internal/ledger/pool.go) and the
// config file (config.QuotaPoolConfig). No second store is
// invented: every read and write here goes through the same
// ledger rows the dashboard feeds (handleQuotaPools /
// handleQuotaPoolUpdate) and the pre-call fair-share gate
// (checkQuotaPools) use, so a pool created here is enforced by
// that gate on the chat paths, not merely stored.
//
// Auth is NOT checked here: like handleUsageBudget, these handlers
// assume the guard table has already enforced management auth.
// Registration (guard.go route table + server.go register) is the
// coordinator's job; the proposed tier is Tier 3 MANAGEMENT with
// the admin scope on all five routes (they carry no credentials
// and cannot redirect traffic, so they are not Tier 2
// ALWAYS_PROTECTED like the provider add/delete routes).
//
// Shares rule (new in this slice, on top of the existing
// validateQuotaPoolUpdate): weights are each member's share of
// the pool, so they must sum sensibly — a pool whose allocations
// are all zero has no share to enforce and is refused with a 400
// naming the "allocations" field, and a sum above 100
// over-allocates the pool and is refused the same way.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// apiQuotaPoolView builds one pool's API row: the stored fields
// plus the live per-dimension consumption in the current window —
// the same shape as the dashboard's quotaPoolView, so the two
// feeds can never disagree about what a pool has consumed.
func (s *Server) apiQuotaPoolView(ctx context.Context, pool ledger.QuotaPool, now time.Time) (quotaPoolView, error) {
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
			return view, err
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
	return view, nil
}

// validateAPIQuotaPoolShares enforces the shares rule on top of
// validateQuotaPoolUpdate: with at least one allocation, the
// weights must sum to something above zero and at most 100.
// Both failures name the "allocations" field. An empty allocation
// list is allowed by the existing validation (a pool with no
// members yet) and is not a shares failure.
func validateAPIQuotaPoolShares(p config.QuotaPoolConfig) error {
	if len(p.Allocations) == 0 {
		return nil
	}
	sum := 0.0
	for _, a := range p.Allocations {
		sum += a.Weight
	}
	if sum == 0 {
		return errors.New("allocations: all shares are zero — at least one allocation must have a weight above 0")
	}
	if sum > 100 {
		return fmt.Errorf("allocations: weights sum to %v, above 100 — shares are percentages of the pool", sum)
	}
	return nil
}

// validateAPIQuotaPool runs the existing pool validation plus the
// shares rule, in that order — so an unknown key or a bad
// dimension is reported before the shares check.
func (s *Server) validateAPIQuotaPool(p config.QuotaPoolConfig) error {
	if err := s.validateQuotaPoolUpdate(p); err != nil {
		return err
	}
	return validateAPIQuotaPoolShares(p)
}

// handleAPIQuotaPools serves POST /api/quota-pools (create, 201)
// and GET /api/quota-pools (list, 200). Proposed tier: Tier 3
// MANAGEMENT, admin scope (see file header).
func (s *Server) handleAPIQuotaPools(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "quota pools unavailable: no ledger wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPIQuotaPoolsList(w, r)
	case http.MethodPost:
		s.handleAPIQuotaPoolsCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAPIQuotaPoolsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	pools, err := s.ledger.ListQuotaPools(ctx)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not list quota pools")
		return
	}
	out := quotaPoolsResponse{Pools: []quotaPoolView{}}
	for _, pool := range pools {
		view, err := s.apiQuotaPoolView(ctx, pool, now)
		if err != nil {
			s.logger.Printf("api/quota-pools: status failed for pool %s: %v", pool.ID, guardrails.RedactLogLine(err.Error()))
			writeServerError(w, http.StatusInternalServerError, "could not read quota pool status")
			return
		}
		out.Pools = append(out.Pools, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAPIQuotaPoolsCreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update quotaPoolUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid quota pool: expected {\"id\": string, \"name\": string, \"saturation_threshold\": number, \"account_count\": number, \"dimensions\": [{\"unit\": \"requests\"|\"tokens\"|\"usd\", \"window\": \"5h\"|\"hourly\"|\"daily\"|\"weekly\"|\"monthly\", \"limit\": number}], \"allocations\": [{\"key_id\": string, \"weight\": number, \"cap_value\": number, \"cap_unit\": string, \"policy\": \"hard\"|\"soft\"|\"burst\"}]}")
		return
	}
	ctx := r.Context()
	if update.ID == "" {
		id, err := newAPIID("pool-")
		if err != nil {
			writeServerError(w, http.StatusInternalServerError, "could not create quota pool")
			return
		}
		update.ID = id
	}
	if _, err := s.ledger.GetQuotaPool(ctx, update.ID); err == nil {
		writeError(w, http.StatusConflict, "quota pool "+update.ID+" already exists")
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		writeServerError(w, http.StatusInternalServerError, "could not read quota pool")
		return
	}
	next := config.QuotaPoolConfig{ID: update.ID}
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
	if err := s.validateAPIQuotaPool(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.ledger.SetQuotaPool(ctx, poolConfigToLedger(next)); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save quota pool")
		return
	}
	if !s.persistAPIQuotaPoolConfig(ctx, next, nil) {
		writeServerError(w, http.StatusInternalServerError, "could not persist quota pool config")
		return
	}
	stored, err := s.ledger.GetQuotaPool(ctx, update.ID)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read created quota pool")
		return
	}
	view, err := s.apiQuotaPoolView(ctx, stored, time.Now())
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read created quota pool")
		return
	}
	writeJSON(w, http.StatusCreated, view)
}

// persistAPIQuotaPoolConfig writes one pool row into the config
// file (when a config + persist function are wired) and rolls the
// ledger back when the save fails. prev is the pre-change pool
// (nil for a create). It returns false when the save failed —
// it does NOT answer the request itself; the caller writes the
// 500. With no config wired it returns true: the ledger is the
// durable store.
func (s *Server) persistAPIQuotaPoolConfig(ctx context.Context, next config.QuotaPoolConfig, prev *ledger.QuotaPool) bool {
	if s.cfg == nil || s.configPersist == nil {
		return true
	}
	prevCfg := append([]config.QuotaPoolConfig(nil), s.cfg.QuotaPools...)
	found := false
	for i, p := range s.cfg.QuotaPools {
		if p.ID == next.ID {
			s.cfg.QuotaPools[i] = next
			found = true
			break
		}
	}
	if !found {
		s.cfg.QuotaPools = append(s.cfg.QuotaPools, next)
	}
	if err := s.configPersist(); err != nil {
		s.cfg.QuotaPools = prevCfg
		if prev == nil {
			_ = s.ledger.DeleteQuotaPool(ctx, next.ID)
		} else {
			_ = s.ledger.SetQuotaPool(ctx, *prev)
		}
		return false
	}
	return true
}

// handleAPIQuotaPool serves GET /api/quota-pools/{id} (200),
// PATCH /api/quota-pools/{id} (200, partial update) and DELETE
// /api/quota-pools/{id} (200). Proposed tier: Tier 3 MANAGEMENT,
// admin scope (see file header).
func (s *Server) handleAPIQuotaPool(w http.ResponseWriter, r *http.Request) {
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "quota pools unavailable: no ledger wired")
		return
	}
	poolID := strings.TrimSpace(r.PathValue("id"))
	if poolID == "" {
		writeError(w, http.StatusBadRequest, "quota pool id is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAPIQuotaPoolGet(w, r, poolID)
	case http.MethodPatch:
		s.handleAPIQuotaPoolPatch(w, r, poolID)
	case http.MethodDelete:
		s.handleAPIQuotaPoolDelete(w, r, poolID)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAPIQuotaPoolGet(w http.ResponseWriter, r *http.Request, poolID string) {
	pool, err := s.ledger.GetQuotaPool(r.Context(), poolID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "unknown quota pool "+poolID)
			return
		}
		writeServerError(w, http.StatusInternalServerError, "could not read quota pool")
		return
	}
	view, err := s.apiQuotaPoolView(r.Context(), pool, time.Now())
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read quota pool status")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleAPIQuotaPoolPatch(w http.ResponseWriter, r *http.Request, poolID string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var update quotaPoolUpdate
	if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
		writeError(w, http.StatusBadRequest, "invalid quota pool patch: expected a JSON object with any of {\"name\", \"saturation_threshold\", \"account_count\", \"dimensions\", \"allocations\"}")
		return
	}
	ctx := r.Context()
	prev, err := s.ledger.GetQuotaPool(ctx, poolID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "unknown quota pool "+poolID)
			return
		}
		writeServerError(w, http.StatusInternalServerError, "could not read quota pool")
		return
	}
	next := poolLedgerToConfig(prev)
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
	if err := s.validateAPIQuotaPool(next); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.ledger.SetQuotaPool(ctx, poolConfigToLedger(next)); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not save quota pool")
		return
	}
	prevCopy := prev
	if !s.persistAPIQuotaPoolConfig(ctx, next, &prevCopy) {
		writeServerError(w, http.StatusInternalServerError, "could not persist quota pool config")
		return
	}
	stored, err := s.ledger.GetQuotaPool(ctx, poolID)
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read updated quota pool")
		return
	}
	view, err := s.apiQuotaPoolView(ctx, stored, time.Now())
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not read updated quota pool")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) handleAPIQuotaPoolDelete(w http.ResponseWriter, r *http.Request, poolID string) {
	ctx := r.Context()
	prev, err := s.ledger.GetQuotaPool(ctx, poolID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "unknown quota pool "+poolID)
			return
		}
		writeServerError(w, http.StatusInternalServerError, "could not read quota pool")
		return
	}
	if err := s.ledger.DeleteQuotaPool(ctx, poolID); err != nil {
		writeServerError(w, http.StatusInternalServerError, "could not delete quota pool")
		return
	}
	if s.cfg != nil && s.configPersist != nil {
		prevCfg := append([]config.QuotaPoolConfig(nil), s.cfg.QuotaPools...)
		kept := s.cfg.QuotaPools[:0]
		for _, p := range s.cfg.QuotaPools {
			if p.ID != poolID {
				kept = append(kept, p)
			}
		}
		s.cfg.QuotaPools = kept
		if err := s.configPersist(); err != nil {
			s.cfg.QuotaPools = prevCfg
			_ = s.ledger.SetQuotaPool(ctx, prev)
			writeServerError(w, http.StatusInternalServerError, "could not persist quota pool config")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
