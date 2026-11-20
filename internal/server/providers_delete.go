// Provider-delete endpoint (DELETE /api/providers/{id}).
//
// Removing a live-added provider is the counterpart of POST
// /api/providers: the registry entry is deleted AND persisted
// (config file), the live engine client is dropped (so the next
// request never routes to it), its models leave the auto-router
// tiers (only models no remaining provider still serves — tiers
// hold model IDs, not (provider, model) pairs), and its probe
// history is pruned. Tier registration stops being append-only.
//
// Guard: Tier 2 (ALWAYS_PROTECTED — gateway-key auth always
// required) plus a typed confirmation phrase in the JSON body,
// "DELETE <id>" naming the provider being removed — the same
// danger-zone pattern as REVOKE ALL / RESET CONFIG, so a delete
// can never fire from a stray click or a replayed request for a
// different provider. The phrase is checked before the provider
// is even looked up.
//
// The response never carries a secret: no provider API key and no
// base URL. Error statuses are distinct per cause: 400 bad
// confirmation / bad id, 401 gateway-key auth (guard), 404
// unknown provider, 503 no registry wired, 500 persist failure.
package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ProviderRemover is the registry surface DELETE
// /api/providers/{id} needs: look a provider up, then delete it
// (persisting the change). *provider.Registry satisfies it; tests
// may inject a fake.
type ProviderRemover interface {
	Get(id string) (*provider.Provider, bool)
	Remove(id string) error
}

// ProviderRemovedHook runs after DELETE /api/providers/{id} has
// removed the provider from the live registry and persisted it.
// Production wires it in main.go: it drops the provider's client
// from the engine and from the openStack client map, unregisters
// its models from the auto-router tiers (only models no remaining
// provider serves), and prunes its probe history — so the removal
// takes effect immediately with no restart. A hook failure does
// NOT fail the delete (the provider is already gone — a retry
// would only hit a 404): the handler answers 200 with
// live_removed=false and the note, so the dashboard says honestly
// that a restart is needed to fully drop it. Nil hook (tests,
// servers without an engine) means live_removed=false with the
// same honest note.
type ProviderRemovedHook func(ctx context.Context, p *provider.Provider) error

// WithProviderRemovedHook wires the hook run after a successful
// provider delete (see ProviderRemovedHook).
func WithProviderRemovedHook(h ProviderRemovedHook) Option {
	return func(s *Server) { s.providerRemovedHook = h }
}

// removedProviderResponse is the 200 payload for a deleted
// provider: identity and wiring facts only — never the key, never
// the base URL. LiveRemoved reports whether the provider stopped
// routing immediately (the engine dropped its client); false
// carries a Note saying a restart is needed instead of silently
// pretending otherwise.
type removedProviderResponse struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Models      []string `json:"models"`
	LiveRemoved bool     `json:"live_removed"`
	Note        string   `json:"note,omitempty"`
}

func removedProviderFrom(p *provider.Provider, liveRemoved bool, note string) removedProviderResponse {
	models := make([]string, 0, len(p.Models))
	for _, m := range p.Models {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return removedProviderResponse{ID: p.ID, Name: p.Name, Models: models, LiveRemoved: liveRemoved, Note: note}
}

// handleDeleteProvider serves DELETE /api/providers/{id} (Tier 2:
// gateway-key auth always required — see guard.go). It checks the
// typed confirmation phrase, deletes the provider from the live
// registry (persisted), and makes the removal live immediately
// when an engine hook is wired.
func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerRemover == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	// Typed confirmation first (danger-zone pattern): the phrase
	// names the provider, so a replayed body cannot delete a
	// different one.
	if _, ok := s.readDangerConfirm(w, r, "DELETE "+id); !ok {
		return
	}
	p, ok := s.providerRemover.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := s.providerRemover.Remove(id); err != nil {
		if errors.Is(err, provider.ErrProviderNotFound) {
			writeError(w, http.StatusNotFound, "provider not found")
			return
		}
		s.logger.Printf("provider delete failed: %v", guardrails.RedactLogLine(err.Error()))
		writeError(w, http.StatusInternalServerError, "could not remove provider")
		return
	}
	// The provider is gone from the registry and the config file.
	// Make it stop routing immediately when an engine hook is
	// wired; a hook failure (or no hook) never fails the delete —
	// the response says honestly that a restart is needed to fully
	// drop it.
	liveRemoved := false
	note := "removed and saved, but the live engine may still route through it until restart"
	if s.providerRemovedHook != nil {
		if hookErr := s.providerRemovedHook(r.Context(), p); hookErr != nil {
			s.logger.Printf("provider delete: engine hook failed for %s: %v", p.ID, guardrails.RedactLogLine(hookErr.Error()))
		} else {
			liveRemoved = true
			note = ""
		}
	}
	writeJSON(w, http.StatusOK, removedProviderFrom(p, liveRemoved, note))
}
