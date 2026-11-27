// Package server — ToS risk-acknowledgement store and endpoint
// (A3 item 6).
//
// Some catalog providers are marked ToSGrey: connecting one means the
// operator hands over their own web session or subscription account,
// by their own choice. The provider's terms of service may forbid
// this kind of use and the connected account can be suspended or
// banned. The provider registry refuses such providers until the risk
// has been acknowledged (see internal/provider/tos_gate.go).
//
// This file owns the acknowledgement record and its API surface:
//
//   - POST /api/providers/{id}/risk-acknowledgement records WHO (the
//     authenticated admin identity the server already resolves — the
//     gateway-key principal, hashed the same way the audit log hashes
//     it, or the dashboard session), WHICH provider and WHEN, and
//     persists it. The registry reads the same store through
//     SetToSAcknowledgement, so the add-time, config-load and
//     request-time gate checks all see the acknowledgement.
//
//   - The POST itself is a normal Tier 2 always-protected request, so
//     the withAudit middleware writes its row into the append-only,
//     tamper-evident audit log (actor key hash, path, timestamp).
//
// The store follows the dashboard's JSON-file convention
// (dashboard_password.json): one JSON file inside the data directory,
// mode 0600, atomic rename on write, loaded lazily on first use, and
// memory-only when no data dir is configured. Acknowledgements are
// not secrets (they name a catalog ID, a hashed actor and a
// timestamp), so the Vault is not needed here — only the same
// directory the Vault lives in.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// tosAcknowledgement is one recorded risk acknowledgement. The actor
// is a hash (first 8 hex of SHA-256 of the gateway-key ID — never the
// raw key) or the literal "dashboard-session" when the request was
// authenticated by the operator's dashboard session cookie, which the
// server intentionally does not turn into a portable identity.
type tosAcknowledgement struct {
	Provider       string    `json:"provider"`
	AcknowledgedBy string    `json:"acknowledged_by"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// tosGateStore holds the ToS risk acknowledgements, one per catalog
// provider ID. Re-acknowledging a provider overwrites the record:
// the latest acknowledgement is the one the gate honours.
type tosGateStore struct {
	mu     sync.Mutex
	path   string // "" = memory-only
	acks   map[string]tosAcknowledgement
	loaded bool
}

func newTosGateStore(dataDir string) *tosGateStore {
	if dataDir == "" {
		return &tosGateStore{acks: map[string]tosAcknowledgement{}}
	}
	return &tosGateStore{
		path: filepath.Join(dataDir, "tos_acknowledgements.json"),
		acks: map[string]tosAcknowledgement{},
	}
}

// loadLocked reads the persisted acknowledgements once. A missing
// file is an empty store, not an error.
func (s *tosGateStore) loadLocked() {
	if s.loaded || s.path == "" {
		s.loaded = true
		return
	}
	s.loaded = true
	b, err := os.ReadFile(s.path)
	if err != nil {
		return // first use: nothing recorded yet
	}
	var recs []tosAcknowledgement
	if json.Unmarshal(b, &recs) != nil {
		return // unreadable file: start empty rather than guess
	}
	for _, rec := range recs {
		if rec.Provider != "" {
			s.acks[rec.Provider] = rec
		}
	}
}

// persistLocked writes the store atomically (temp file in the same
// directory, 0600, rename). A write failure is returned so the
// endpoint can tell the operator the acknowledgement did NOT stick —
// the gate must never silently claim a persisted acknowledgement.
func (s *tosGateStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	recs := make([]tosAcknowledgement, 0, len(s.acks))
	for _, rec := range s.acks {
		recs = append(recs, rec)
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Acknowledge records (or refreshes) the risk acknowledgement for a
// catalog provider and persists it.
func (s *tosGateStore) Acknowledge(providerID, actor string) (tosAcknowledgement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	rec := tosAcknowledgement{
		Provider:       providerID,
		AcknowledgedBy: actor,
		AcknowledgedAt: time.Now().UTC(),
	}
	s.acks[providerID] = rec
	if err := s.persistLocked(); err != nil {
		return tosAcknowledgement{}, err
	}
	return rec, nil
}

// Acknowledged reports whether the risk for a catalog provider has
// been acknowledged. This is the function the provider registry's
// gate reads (SetToSAcknowledgement).
func (s *tosGateStore) Acknowledged(providerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	_, ok := s.acks[providerID]
	return ok
}

// List returns every acknowledgement, newest first (test/status
// surface; the endpoint itself writes, it never reads).
func (s *tosGateStore) List() []tosAcknowledgement {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	out := make([]tosAcknowledgement, 0, len(s.acks))
	for _, rec := range s.acks {
		out = append(out, rec)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AcknowledgedAt.After(out[j-1].AcknowledgedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// tosGateFor lazily initializes the server's acknowledgement store
// from the configured data directory (same dir the Vault uses; the
// store itself is memory-only without one).
func (s *Server) tosGateFor() *tosGateStore {
	s.tosGateMu.Lock()
	defer s.tosGateMu.Unlock()
	if s.tosGate != nil {
		return s.tosGate
	}
	dir := ""
	if s.cfg != nil {
		dir = s.cfg.DataDir
	}
	s.tosGate = newTosGateStore(dir)
	return s.tosGate
}

// ToSAcknowledged reports whether the ToS risk for a catalog provider
// has been acknowledged. It is the production acknowledgement source
// wired into the provider registry's gate (WithProviderRegistry →
// SetToSAcknowledgement).
func (s *Server) ToSAcknowledged(providerID string) bool {
	return s.tosGateFor().Acknowledged(providerID)
}

// handleRiskAcknowledgement serves POST
// /api/providers/{id}/risk-acknowledgement (Tier 2, always protected,
// admin scope): records who, which provider and when, persists the
// record, and — as every request through the server chain — lands in
// the audit log. The refusal layers in the provider registry then let
// the acknowledged provider through the add, config-load and
// request-time gates.
func (s *Server) handleRiskAcknowledgement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	providerID := r.PathValue("id")
	entry, ok := catalog.ByID(providerID)
	if !ok {
		writeError(w, http.StatusNotFound, "unknown catalog provider: "+providerID)
		return
	}
	if !entry.ToSGrey {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("catalog provider %q (%s) is not ToS-grey — risk acknowledgement applies only to ToS-grey providers, and none is needed here", entry.ID, entry.Name))
		return
	}
	// WHO: the authenticated admin identity the server already
	// resolves. A gateway key gives its hashed key ID (the same hash
	// the audit log stores); the dashboard session authenticates the
	// operator without being a portable identity, so it records as
	// "dashboard-session".
	actor := "dashboard-session"
	if keyID := s.callerKeyID(r); keyID != "" {
		actor = hashKeyID(keyID)
	}
	rec, err := s.tosGateFor().Acknowledge(entry.ID, actor)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "risk acknowledgement not persisted: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "acknowledged",
		"provider":        rec.Provider,
		"acknowledged_by": rec.AcknowledgedBy,
		"acknowledged_at": rec.AcknowledgedAt,
		"notice": fmt.Sprintf(
			"%s (%s) is ToS-grey: its terms of service may forbid this kind of use and the account you connect can be suspended or banned. The risk is now acknowledged for this router; adding the provider and routing through it proceed.",
			entry.Name, entry.ID),
	})
}
