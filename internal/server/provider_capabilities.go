package server

// Provider/model capability metadata API (P1.5).
//
// GET /api/capabilities
//   The full capability matrix: every provider/model pair the router
//   knows about, with each capability's support state and provenance.
//
// GET /api/providers/{id}/capabilities
//   The same data narrowed to one provider.
//
// DESIGN: capability facts are reported, never inferred here. Every
// entry carries the source it came from (catalog docs, models.dev
// sync, live probe, operator config) and its verification state. A
// capability with no affirmative evidence reads "unknown" and is NOT
// presented as supported.

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/capability"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// capabilitiesSource supplies the built capability matrix. Implemented
// by a small adapter over a capability.Index so the server package does
// not import provider internals beyond the registry type.
type capabilitiesSource interface {
	CapabilitiesMatrix() map[string]capability.ModelCapabilities
	RefreshCapabilities()
}

// registryCapabilitySource serves the matrix from one capability
// snapshot. Rebuilds happen only on explicit refresh, never on the
// read path: an API read is O(models) map lookups, and a refresh is a
// bounded in-process rebuild from the same sources (no network I/O —
// the models.dev sync that feeds it is operator-triggered elsewhere).
type registryCapabilitySource struct {
	index *capability.Index
}

func (s registryCapabilitySource) CapabilitiesMatrix() map[string]capability.ModelCapabilities {
	if s.index == nil {
		return nil
	}
	return s.index.Entries()
}

func (s registryCapabilitySource) RefreshCapabilities() {
	if s.index != nil {
		s.index.Refresh()
	}
}

// WithCapabilityRegistry enables the capability API endpoints
// (P1.5) from a provider registry. Without it both endpoints answer
// 503.
func WithCapabilityRegistry(r *provider.Registry) Option {
	return func(s *Server) {
		if r == nil {
			s.capabilitySource = nil
			return
		}
		s.capabilitySource = registryCapabilitySource{index: capability.NewIndex(capability.Builder{Registry: r})}
	}
}

// WithCapabilityIndex enables the capability API endpoints from a
// caller-owned index — the same snapshot the routing engine consults
// (router.CapabilityLookupFunc), so the API can never disagree with
// what routing actually enforced. The caller keeps ownership and may
// refresh it; POST /api/capabilities/refresh refreshes it in place.
func WithCapabilityIndex(idx *capability.Index) Option {
	return func(s *Server) {
		if idx == nil {
			s.capabilitySource = nil
			return
		}
		s.capabilitySource = registryCapabilitySource{index: idx}
	}
}

// capabilityEntryJSON is the wire shape of one capability fact.
type capabilityEntryJSON struct {
	Capability   string `json:"capability"`
	Support      string `json:"support"`
	Source       string `json:"source"`
	Verification string `json:"verification"`
	Confidence   string `json:"confidence,omitempty"`
	ObservedAt   string `json:"observed_at,omitempty"`
	Note         string `json:"note,omitempty"`
}

// capabilityModelJSON is one model's capability set.
type capabilityModelJSON struct {
	Provider     string                `json:"provider"`
	Model        string                `json:"model"`
	Capabilities []capabilityEntryJSON `json:"capabilities"`
}

// handleCapabilities serves GET /api/capabilities.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.capabilitySource == nil {
		writeError(w, http.StatusServiceUnavailable, "capability registry is not configured")
		return
	}
	matrix := s.capabilitySource.CapabilitiesMatrix()
	models := make([]capabilityModelJSON, 0, len(matrix))
	for key, mc := range matrix {
		providerID, modelID := splitCapabilityKey(key)
		models = append(models, capabilityModelJSON{
			Provider:     providerID,
			Model:        modelID,
			Capabilities: capabilityEntries(mc),
		})
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].Model < models[j].Model
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "capabilities.matrix",
		"count":  len(models),
		"models": models,
	})
}

// handleCapabilitiesRefresh serves POST /api/capabilities/refresh: it
// rebuilds the in-process capability snapshot from the same sources
// (registry + opt-in sync state) and reports the new size. It performs
// no network I/O and cannot invent capabilities — a refresh only
// re-reads what the sources already say, so it is safe to expose
// without cost controls. Operators call it after a discovery sync.
func (s *Server) handleCapabilitiesRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.capabilitySource == nil {
		writeError(w, http.StatusServiceUnavailable, "capability registry is not configured")
		return
	}
	s.capabilitySource.RefreshCapabilities()
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "capabilities.refresh",
		"count":  len(s.capabilitySource.CapabilitiesMatrix()),
	})
}

// handleProviderCapabilities serves GET /api/providers/{id}/capabilities.
func (s *Server) handleProviderCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.capabilitySource == nil {
		writeError(w, http.StatusServiceUnavailable, "capability registry is not configured")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "provider id is required")
		return
	}
	matrix := s.capabilitySource.CapabilitiesMatrix()
	models := make([]capabilityModelJSON, 0, 8)
	for key, mc := range matrix {
		providerID, modelID := splitCapabilityKey(key)
		if providerID != id {
			continue
		}
		models = append(models, capabilityModelJSON{
			Provider:     providerID,
			Model:        modelID,
			Capabilities: capabilityEntries(mc),
		})
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].Model < models[j].Model })
	if len(models) == 0 {
		writeError(w, http.StatusNotFound, "unknown provider "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":   "provider.capabilities",
		"provider": id,
		"count":    len(models),
		"models":   models,
	})
}

// splitCapabilityKey splits "provider|model" (the builder's key shape).
func splitCapabilityKey(key string) (string, string) {
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

// capabilityEntries renders one model's capability map in stable order.
func capabilityEntries(mc capability.ModelCapabilities) []capabilityEntryJSON {
	out := make([]capabilityEntryJSON, 0, len(mc))
	for _, e := range mc {
		entry := capabilityEntryJSON{
			Capability:   string(e.Capability),
			Support:      string(e.Support),
			Source:       string(e.Provenance.Source),
			Verification: string(e.Provenance.Verification),
			Confidence:   e.Provenance.Confidence,
			Note:         e.Provenance.Note,
		}
		if !e.Provenance.ObservedAt.IsZero() {
			entry.ObservedAt = e.Provenance.ObservedAt.Format(time.RFC3339)
		}
		out = append(out, entry)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	return out
}
