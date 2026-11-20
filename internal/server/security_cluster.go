// Security cluster (CHECKLIST 245, LiteLLM #47028/#51254): the
// request-time enforcement for client-supplied overrides and
// admin deployment pins.
//
// The rule, stated once: client input never overrides
// admin/routing config. Two halves:
//
//   - A client-supplied Vertex location/project (vertex_location,
//     vertex_project, vertex_ai_location, vertex_ai_project) is
//     rejected with a 400 naming the field, unless the admin
//     opted in (config AllowClientVertexOverride). The rejection
//     is counted as a security refusal, so the dashboard shows
//     real counts. The rejected value is never logged.
//   - A deployment's pinned parameters (providers[].models[].
//     fixed_params) outrank the client's body fields: the pinned
//     value is what the request is sent with, and the response
//     carries X-Codvian-Deployment-Override naming the applied
//     fields, so the replacement is never silent.
//
// Both run before any provider is dialled, on every translated
// request surface (chat completions, Responses, translated
// Messages, embeddings/rerank). The verbatim Anthropic relay
// forwards the client body unchanged by design, so deployment
// pins do not apply there — see docs/security-cluster-245-246.md.
package server

import (
	"net/http"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// checkClientVertexOverride rejects a request carrying a
// client-supplied Vertex location/project unless the admin opted
// in. It returns true when the request was refused (the caller
// must stop). A nil override means the body carried none. The
// refusal is counted as a security refusal; the error names the
// field, never the value.
func (s *Server) checkClientVertexOverride(w http.ResponseWriter, ov *provider.VertexOverride) bool {
	if ov == nil {
		return false
	}
	if s.cfg != nil && s.cfg.AllowClientVertexOverride {
		return false
	}
	s.guardrails.RecordSecurityRefusal(guardrails.SecurityRefusalVertexOverride)
	writeError(w, http.StatusBadRequest, "client-supplied "+ov.Field+" is not allowed: the Vertex location/project a request lands in is set by the admin's provider config, not by the caller (admin opt-in: allow_client_vertex_override)")
	return true
}

// applyDeploymentParams enforces the requested model's admin
// deployment pins on req: every pinned value replaces the
// client's value for that field. The first provider in config
// order whose entry for this model carries pins wins — the
// operator controls that order, and a model offered by several
// providers without pins simply has nothing to enforce. The
// response carries X-Codvian-Deployment-Override naming the
// applied fields, so a displaced client value is visible, never
// silent. Virtual models (auto/*, group:*) name no single
// deployment entry, so no pin applies to them. With no config
// loaded there are no pins to apply.
func (s *Server) applyDeploymentParams(w http.ResponseWriter, req *provider.ChatRequest) {
	if s.cfg == nil || req == nil {
		return
	}
	for _, p := range s.cfg.Providers {
		for _, m := range p.Models {
			if m.ID != req.Model || m.FixedParams == nil {
				continue
			}
			applied := provider.ApplyFixedParams(req, m)
			if len(applied) > 0 {
				w.Header().Set("X-Codvian-Deployment-Override", strings.Join(applied, ","))
				s.logger.Printf("deployment params: model %q provider %q applied admin pins: %s", req.Model, p.ID, strings.Join(applied, ","))
			}
			return
		}
	}
}
