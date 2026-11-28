// Declarative per-model fallback chains CRUD API (G11 Area 2,
// parity tracker Area 2 row "Declarative per-model fallback chains API",
// shared with §14b).
//
// the sector-typical evidence is src/domain/fallbackPolicy.ts (register /
// resolve / remove / get-all, backed by SQLite) and the
// GET/POST/DELETE /api/fallback/chains surface. Ours is the same
// shape in our own code: the store lives on the routing engine
// (internal/router/fallback_chains.go — Set/List/Delete/
// FallbackChainFor), and this file is only the HTTP face over it,
// so a chain created here is the same chain the router resolves —
// there is no second store to drift.
//
// This file only implements the handlers. Registration (route
// table, tier, gateway-key auth, scope) belongs to the coordinator
// in server.go/guard.go, which this slice must not touch — the
// exact registration lines are in the G11 report. The handlers
// perform no auth themselves, like the other management APIs in
// this package (the guard table enforces it at registration).
//
// Endpoints (once registered):
//
//	GET    /api/fallback/chains            list all chains
//	GET    /api/fallback/chains?model=M    get one chain (404)
//	POST   /api/fallback/chains            create/update a chain
//	DELETE /api/fallback/chains?model=M    remove a chain (404)
//
// POST body: {"model": "gpt-4o", "entries": [{"provider": "a",
// "model": "", "priority": 0, "enabled": true}]}. "enabled"
// defaults to true when omitted (the sector-typical default); "model"
// defaults to the request-time model when omitted. POST is both
// create and update (like the sector-typical registerFallback): 201 when
// the model had no chain, 200 when it replaced one.
//
// Persistence: in-memory only in this slice (the engine store).
// A persist callback wired to the config file is the coordinator's
// follow-up; responses report persisted=false honestly until then,
// the same convention the combos API uses with no persist function
// wired. Wiring the stored chains into chain resolution
// (chainFor consulting FallbackChainFor) is likewise the
// coordinator's follow-up — until then the store is reachable and
// round-trips, but no request consults it yet.
//
// The store is reached through a narrow interface asserted on
// s.engine, so the handlers do not need server.go's Engine
// interface extended (Wave 3 owns it). An engine that does not
// implement the store answers 503, honestly, instead of a fake
// empty list.
package server

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// fallbackChainStore is the narrow engine surface these handlers
// need (*router.Engine implements it; see
// internal/router/fallback_chains.go).
type fallbackChainStore interface {
	SetFallbackChain(model string, entries []router.FallbackEntry) error
	ListFallbackChains() map[string][]router.FallbackEntry
	DeleteFallbackChain(model string) bool
}

// fallbackChainStoreOf resolves the store from the server's
// engine: nil when no engine is wired or it does not implement
// the store.
func (s *Server) fallbackChainStoreOf() (fallbackChainStore, bool) {
	if s == nil || s.engine == nil {
		return nil, false
	}
	store, ok := s.engine.(fallbackChainStore)
	return store, ok
}

// fallbackChainEntryRequest is one POSTed chain step. Enabled is
// a pointer so an omitted field defaults to true (the sector-typical
// default) instead of Go's false.
type fallbackChainEntryRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Priority int    `json:"priority"`
	Enabled  *bool  `json:"enabled"`
}

// fallbackChainRequest is the POST body: the model the chain
// attaches to and its ordered steps.
type fallbackChainRequest struct {
	Model   string                      `json:"model"`
	Entries []fallbackChainEntryRequest `json:"entries"`
}

// fallbackChainView is one chain in responses.
type fallbackChainView struct {
	Model     string                 `json:"model"`
	Entries   []router.FallbackEntry `json:"entries"`
	Persisted bool                   `json:"persisted"`
}

// fallbackChainsResponse is the GET list body.
type fallbackChainsResponse struct {
	Chains []fallbackChainView `json:"chains"`
}

// handleFallbackChains serves GET (list, or one chain with
// ?model=) and POST (create/update) on /api/fallback/chains.
func (s *Server) handleFallbackChains(w http.ResponseWriter, r *http.Request) {
	store, ok := s.fallbackChainStoreOf()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "fallback chains unavailable: engine does not implement a fallback chain store")
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleFallbackChainsList(w, r, store)
	case http.MethodPost:
		s.handleFallbackChainsCreate(w, r, store)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleFallbackChainsList answers GET: every chain (sorted by
// model, entries in priority order), or one chain when ?model=
// names a stored model (404 otherwise).
func (s *Server) handleFallbackChainsList(w http.ResponseWriter, r *http.Request, store fallbackChainStore) {
	all := store.ListFallbackChains()
	if model := r.URL.Query().Get("model"); model != "" {
		entries, ok := all[model]
		if !ok {
			writeError(w, http.StatusNotFound, "no fallback chain for model "+model)
			return
		}
		writeJSON(w, http.StatusOK, fallbackChainView{Model: model, Entries: entries, Persisted: false})
		return
	}
	models := make([]string, 0, len(all))
	for model := range all {
		models = append(models, model)
	}
	sort.Strings(models)
	out := fallbackChainsResponse{Chains: []fallbackChainView{}}
	for _, model := range models {
		out.Chains = append(out.Chains, fallbackChainView{Model: model, Entries: all[model], Persisted: false})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFallbackChainsCreate answers POST: validates the body
// (400s name the field, via the engine store's validation) and
// stores the chain. 201 when the model had no chain, 200 when
// it replaced one.
func (s *Server) handleFallbackChainsCreate(w http.ResponseWriter, r *http.Request, store fallbackChainStore) {
	var req fallbackChainRequest
	// AUD-SEC-003: bound the body before decoding, like every other handler.
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_, existed := store.ListFallbackChains()[req.Model]
	entries := make([]router.FallbackEntry, 0, len(req.Entries))
	for _, en := range req.Entries {
		enabled := true
		if en.Enabled != nil {
			enabled = *en.Enabled
		}
		entries = append(entries, router.FallbackEntry{
			Provider: en.Provider,
			Model:    en.Model,
			Priority: en.Priority,
			Enabled:  enabled,
		})
	}
	if err := store.SetFallbackChain(req.Model, entries); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	// Echo the stored (priority-sorted) chain, not the
	// submission order, so the response matches a later GET.
	stored := store.ListFallbackChains()[req.Model]
	writeJSON(w, status, fallbackChainView{Model: req.Model, Entries: stored, Persisted: false})
}

// handleFallbackChain serves DELETE /api/fallback/chains?model=M:
// removes the chain, 404 when the model has none.
func (s *Server) handleFallbackChain(w http.ResponseWriter, r *http.Request) {
	store, ok := s.fallbackChainStoreOf()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "fallback chains unavailable: engine does not implement a fallback chain store")
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	model := r.URL.Query().Get("model")
	if model == "" {
		writeError(w, http.StatusBadRequest, "model: empty — DELETE needs ?model=")
		return
	}
	if !store.DeleteFallbackChain(model) {
		writeError(w, http.StatusNotFound, "no fallback chain for model "+model)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": model, "deleted": true, "persisted": false})
}
