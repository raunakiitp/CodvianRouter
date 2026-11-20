// Provider-key endpoints (CHECKLIST 175-176): manage one
// provider's API-key pool — add a key, remove a key, change a
// key's role (preferred/reserve).
//
// POST /api/providers/{id}/keys {api_key, role} -> 201 with the
// new key's id/role/fingerprint (never the key itself). The key is
// tested live against the upstream before anything is stored
// (Registry.AddKey), exactly like POST /api/providers tests its
// key; a 401 key is refused, a duplicate credential is refused.
// DELETE /api/providers/{id}/keys/{keyId} -> 200. The last key
// cannot be removed — remove the provider instead.
// POST /api/providers/{id}/keys/{keyId}/role {role} -> 200.
// POST /api/providers/{id}/keys/{keyId}/rotate {api_key} -> 200.
// Rotation replaces the key's credential in place (same ID and
// role — the dashboard's "paste a replacement key" box); the new
// key is tested live against the upstream before anything is
// stored, exactly like an add.
//
// All four are Tier 2 ALWAYS_PROTECTED (see guard.go): they plant
// or change credentials, so gateway-key auth is always required.
// After each mutation the key-changed hook runs (production wires
// it in main.go: it rebuilds that provider's engine clients, so a
// new key routes immediately with no restart). A hook failure does
// NOT fail the request — the key is already stored, so the
// response says honestly that a restart is needed to route through
// it (routing_live=false + note), mirroring the provider add/delete
// hooks.
//
// The responses never carry a secret: key id, role, fingerprint
// and creation time only — never ciphertext, never plaintext.
// Error statuses are distinct per cause: 400 validation /
// last-key refusal, 401 gateway-key auth (guard) or upstream-
// rejected provider key, 404 unknown provider/key, 409 duplicate
// credential, 502 upstream unreachable / key-test failure, 503 no
// registry wired, 500 persist failure.

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ProviderKeyManager is the registry surface the key endpoints
// need. *provider.Registry satisfies it; tests may inject a fake.
type ProviderKeyManager interface {
	Get(id string) (*provider.Provider, bool)
	// List returns every provider in the registry, in registry
	// order (Registry.List). The per-provider surface uses it
	// for name resolution (resolveProviderV1): a caller may
	// prefix a model with a provider's display Name, not only
	// its generated ID.
	List() []*provider.Provider
	AddKey(ctx context.Context, providerID, apiKey, role string) (provider.ProviderKey, error)
	RemoveKey(providerID, keyID string) error
	SetKeyRole(providerID, keyID, role string) error
	// RotateKey replaces one pool key's credential in place,
	// keeping its ID and role (Registry.RotateKey). The
	// dashboard's "paste a replacement key" box drives it:
	// rotating the primary key must not change key identity.
	RotateKey(ctx context.Context, providerID, keyID, apiKey string) (provider.ProviderKey, error)
	// SetEnabled enables or disables a provider and persists the
	// change (Registry.SetEnabled). The dashboard's Connect button
	// drives it: a disabled provider stops routing and shows as
	// Not connected until re-enabled.
	SetEnabled(providerID string, enabled bool) error
}

// ProviderKeyChangedHook runs after a key mutation (add, remove,
// role change or rotation) has been stored in the live registry and persisted.
// Production wires it in main.go: it re-syncs that provider's
// engine clients from its current pool — building clients for new
// or changed keys, dropping clients for removed keys — so the
// change takes effect immediately with no restart. A hook failure
// does NOT fail the request (the key change is already stored —
// a retry would only hit a duplicate or a 404): the handler
// answers with routing_live=false and the note, so the dashboard
// says honestly that a restart is needed. Nil hook (tests,
// servers without an engine) means routing_live=false with the
// same honest note.
type ProviderKeyChangedHook func(ctx context.Context, p *provider.Provider) error

// WithProviderKeyChangedHook wires the hook run after a successful
// provider-key mutation (see ProviderKeyChangedHook).
func WithProviderKeyChangedHook(h ProviderKeyChangedHook) Option {
	return func(s *Server) { s.providerKeyChangedHook = h }
}

// maxKeyBody caps a provider-key request body (64 KiB: one key and
// a role — nothing here is large).
const maxKeyBody = 64 << 10

// providerKeyView is one pool key in API responses: identity and
// role facts only — never ciphertext, never plaintext. Fingerprint
// is the first 12 hex of the key's SHA-256 (see
// provider.KeyFingerprint); it is empty for a legacy synthesized
// key, whose fingerprint is uncomputable without plaintext — the
// dashboard then shows the key ID instead.
type providerKeyView struct {
	ID          string `json:"id"`
	Role        string `json:"role"`
	Fingerprint string `json:"fingerprint,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func providerKeyViewFrom(k provider.ProviderKey) providerKeyView {
	return providerKeyView{
		ID: k.ID, Role: k.Role, Fingerprint: k.Fingerprint,
		CreatedAt: k.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// keyMutationResponse is the 200/201 payload for a key mutation:
// the affected key plus whether it routes live right now.
// RoutingLive reports whether the engine took the change (the
// hook rebuilt the provider's clients); false carries a
// RoutingNote saying a restart is needed instead of silently
// pretending otherwise.
type keyMutationResponse struct {
	ProviderID  string          `json:"provider_id"`
	Key         providerKeyView `json:"key"`
	RoutingLive bool            `json:"routing_live"`
	RoutingNote string          `json:"routing_note,omitempty"`
}

// runKeyChangedHook runs the key-changed hook for the provider as
// it stands after the mutation and reports whether the change
// routes live. A nil hook or a hook failure means routing_live
// false with the honest restart note — never a failed request.
func (s *Server) runKeyChangedHook(ctx context.Context, p *provider.Provider) (bool, string) {
	const note = "saved, but the live engine has no client for the change yet — restart the router to route through it"
	if s.providerKeyChangedHook == nil {
		return false, note
	}
	if err := s.providerKeyChangedHook(ctx, p); err != nil {
		s.logger.Printf("provider key change: engine hook failed for %s: %v", p.ID, guardrails.RedactLogLine(err.Error()))
		return false, note
	}
	return true, ""
}

// classifyKeyError maps a registry key-mutation failure to a
// distinct HTTP status and an honest message. 401/429/502 use the
// provider-add classifier (same live key test); registry wording
// for 400/404/409 passes through redacted (it names the provider
// or key ID the operator typed, never a secret).
func classifyKeyError(err error) (int, string) {
	msg := err.Error()
	switch {
	case errors.Is(err, provider.ErrProviderNotFound):
		return http.StatusNotFound, "provider not found"
	case strings.Contains(msg, "already holds this key"):
		return http.StatusConflict, "this provider already holds that key"
	case strings.Contains(msg, "holds only one key"):
		return http.StatusBadRequest, "a provider's last key cannot be removed — remove the provider instead"
	case strings.Contains(msg, "holds no key"):
		return http.StatusNotFound, "provider key not found"
	case strings.Contains(msg, "unknown key role"):
		return http.StatusBadRequest, guardrails.RedactLogLine(msg)
	case strings.Contains(msg, "key test failed"), strings.Contains(msg, "upstream"):
		return classifyAddError(err)
	case strings.Contains(msg, "persisting provider key"):
		return http.StatusInternalServerError, "key was verified but could not be saved — check the config file"
	case strings.Contains(msg, "API key is empty"):
		return http.StatusBadRequest, "api_key is required"
	default:
		return http.StatusInternalServerError, "could not change provider key"
	}
}

// handleAddProviderKey serves POST /api/providers/{id}/keys
// (Tier 2: gateway-key auth always required — see guard.go).
func (s *Server) handleAddProviderKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerKeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
		Role   string `json:"role"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxKeyBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// Generic message only: the decode error can quote the
		// request body, which carries the provider API key.
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	req.Role = strings.TrimSpace(req.Role)
	if req.Role == "" {
		req.Role = provider.KeyRolePreferred
	}
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	if !provider.IsKeyRole(req.Role) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown key role %q (want %q or %q)", req.Role, provider.KeyRolePreferred, provider.KeyRoleReserve))
		return
	}
	added, err := s.providerKeyManager.AddKey(r.Context(), id, req.APIKey, req.Role)
	if err != nil {
		s.logger.Printf("provider key add failed for %s: %v", id, guardrails.RedactLogLine(err.Error()))
		status, msg := classifyKeyError(err)
		writeError(w, status, msg)
		return
	}
	p, ok := s.providerKeyManager.Get(id)
	if !ok {
		// The provider vanished between the add and the read
		// (concurrent delete): the key is stored, but there is
		// no provider left to route through it — say so
		// honestly instead of reporting the add as live.
		writeJSON(w, http.StatusCreated, keyMutationResponse{
			ProviderID: id, Key: providerKeyViewFrom(added),
			RoutingLive: false,
			RoutingNote: "key saved, but the provider was removed before the engine could take it — re-add the provider to route through it",
		})
		return
	}
	live, note := s.runKeyChangedHook(r.Context(), p)
	writeJSON(w, http.StatusCreated, keyMutationResponse{
		ProviderID: id, Key: providerKeyViewFrom(added),
		RoutingLive: live, RoutingNote: note,
	})
}

// handleDeleteProviderKey serves DELETE /api/providers/{id}/keys/{keyId}
// (Tier 2: gateway-key auth always required — see guard.go).
func (s *Server) handleDeleteProviderKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerKeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	keyID := strings.TrimSpace(r.PathValue("keyId"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	p, ok := s.providerKeyManager.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	// Capture the key's view before removal: after RemoveKey the
	// pool no longer holds it, and the response names what was
	// removed. A missing key is a 404, never a silent success.
	key, found := p.KeyByID(keyID)
	if !found {
		writeError(w, http.StatusNotFound, "provider key not found")
		return
	}
	if err := s.providerKeyManager.RemoveKey(id, keyID); err != nil {
		s.logger.Printf("provider key delete failed for %s/%s: %v", id, keyID, guardrails.RedactLogLine(err.Error()))
		status, msg := classifyKeyError(err)
		writeError(w, status, msg)
		return
	}
	// Re-read the provider as it stands after the removal, so
	// the hook re-syncs from the current pool (without the key).
	if p2, ok := s.providerKeyManager.Get(id); ok {
		p = p2
	}
	live, note := s.runKeyChangedHook(r.Context(), p)
	writeJSON(w, http.StatusOK, keyMutationResponse{
		ProviderID: id, Key: providerKeyViewFrom(key),
		RoutingLive: live, RoutingNote: note,
	})
}

// providerEnabledResponse is the 200 payload for POST
// /api/providers/{id}/enabled: the provider's new enabled state
// plus whether it routes live right now (same honest
// routing_live / routing_note contract as keyMutationResponse).
type providerEnabledResponse struct {
	ProviderID  string `json:"provider_id"`
	Enabled     bool   `json:"enabled"`
	RoutingLive bool   `json:"routing_live"`
	RoutingNote string `json:"routing_note,omitempty"`
}

// handleSetProviderEnabled serves POST /api/providers/{id}/enabled
// (Tier 2: gateway-key auth always required — see guard.go).
// It flips a configured provider between routing and Not
// connected, persisting through Registry.SetEnabled. Enabling is
// what the dashboard's provider-card Connect button does; the
// engine already skips disabled providers (fallback chain filter),
// so no restart is needed either way.
func (s *Server) handleSetProviderEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerKeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxKeyBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Enabled == nil {
		writeError(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if _, ok := s.providerKeyManager.Get(id); !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	if err := s.providerKeyManager.SetEnabled(id, *req.Enabled); err != nil {
		s.logger.Printf("provider enabled failed for %s: %v", id, guardrails.RedactLogLine(err.Error()))
		status, msg := classifyKeyError(err)
		writeError(w, status, msg)
		return
	}
	p, ok := s.providerKeyManager.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	live, note := s.runKeyChangedHook(r.Context(), p)
	writeJSON(w, http.StatusOK, providerEnabledResponse{
		ProviderID:  id,
		Enabled:     p.Enabled,
		RoutingLive: live,
		RoutingNote: note,
	})
}

// handleSetProviderKeyRole serves POST /api/providers/{id}/keys/{keyId}/role
// (Tier 2: gateway-key auth always required — see guard.go).
func (s *Server) handleSetProviderKeyRole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerKeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	keyID := strings.TrimSpace(r.PathValue("keyId"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	var req struct {
		Role string `json:"role"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxKeyBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Role = strings.TrimSpace(req.Role)
	if !provider.IsKeyRole(req.Role) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown key role %q (want %q or %q)", req.Role, provider.KeyRolePreferred, provider.KeyRoleReserve))
		return
	}
	if err := s.providerKeyManager.SetKeyRole(id, keyID, req.Role); err != nil {
		s.logger.Printf("provider key role failed for %s/%s: %v", id, keyID, guardrails.RedactLogLine(err.Error()))
		status, msg := classifyKeyError(err)
		writeError(w, status, msg)
		return
	}
	p, ok := s.providerKeyManager.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "provider not found")
		return
	}
	key, found := p.KeyByID(keyID)
	if !found {
		// The role change succeeded but the key is no longer
		// visible (concurrent delete): report the change, not
		// a phantom key.
		writeJSON(w, http.StatusOK, keyMutationResponse{
			ProviderID:  id,
			Key:         providerKeyView{ID: keyID, Role: provider.NormalizeKeyRole(req.Role)},
			RoutingLive: false,
			RoutingNote: "role saved, but the key was removed before the engine could take it",
		})
		return
	}
	live, note := s.runKeyChangedHook(r.Context(), p)
	writeJSON(w, http.StatusOK, keyMutationResponse{
		ProviderID: id, Key: providerKeyViewFrom(key),
		RoutingLive: live, RoutingNote: note,
	})
}

// handleRotateProviderKey serves POST /api/providers/{id}/keys/{keyId}/rotate
// (Tier 2: gateway-key auth always required — see guard.go).
// It replaces one pool key's credential in place, keeping the key's
// ID and role: the dashboard's provider-card "paste a replacement
// key" box rotates the primary key through it. The new key is tested
// live against the upstream by the registry (a 401 key is refused,
// the same credential twice is refused); on success the key-changed
// hook re-syncs the engine clients, so rotation routes immediately
// with no restart.
func (s *Server) handleRotateProviderKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.providerKeyManager == nil {
		writeError(w, http.StatusServiceUnavailable, "provider registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	keyID := strings.TrimSpace(r.PathValue("keyId"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key id is required")
		return
	}
	var req struct {
		APIKey string `json:"api_key"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxKeyBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		// Generic message only: the decode error can quote the
		// request body, which carries the provider API key.
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.APIKey == "" {
		writeError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	rotated, err := s.providerKeyManager.RotateKey(r.Context(), id, keyID, req.APIKey)
	if err != nil {
		s.logger.Printf("provider key rotate failed for %s/%s: %v", id, keyID, guardrails.RedactLogLine(err.Error()))
		status, msg := classifyKeyError(err)
		writeError(w, status, msg)
		return
	}
	p, ok := s.providerKeyManager.Get(id)
	if !ok {
		// The provider vanished between the rotate and the read
		// (concurrent delete): the key is stored, but there is
		// no provider left to route through it — say so
		// honestly instead of reporting the rotate as live.
		writeJSON(w, http.StatusOK, keyMutationResponse{
			ProviderID: id, Key: providerKeyViewFrom(rotated),
			RoutingLive: false,
			RoutingNote: "key rotated, but the provider was removed before the engine could take it — re-add the provider to route through it",
		})
		return
	}
	live, note := s.runKeyChangedHook(r.Context(), p)
	writeJSON(w, http.StatusOK, keyMutationResponse{
		ProviderID: id, Key: providerKeyViewFrom(rotated),
		RoutingLive: live, RoutingNote: note,
	})
}
