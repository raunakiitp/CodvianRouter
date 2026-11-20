package server

import (
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/router"
)

// contextFitResponse is the GET /dashboard/context-fit payload:
// the fit-check decision of the most recently finished request
// that reached the pre-call fit gate (CHECKLIST 159) — the local
// prompt estimate, the response reserve, the required tokens, the
// per-model fit verdicts, and which chain served the request
// (primary, the dedicated context-window fallback chain, or none).
// has_decision is false before any request reached the gate: an
// honest empty state, never an invented verdict. The same decision
// is embedded in GET /dashboard/trace and GET /dashboard/models,
// so the Live Routing trace and the Models view render it from
// their own feeds; this endpoint is the standalone copy.
type contextFitResponse struct {
	HasDecision bool                       `json:"has_decision"`
	Decision    *router.ContextFitDecision `json:"decision,omitempty"`
}

// handleContextFit serves GET /dashboard/context-fit.
func (s *Server) handleContextFit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dec, ok := s.lastContextFit()
	if !ok {
		writeJSON(w, http.StatusOK, contextFitResponse{HasDecision: false})
		return
	}
	writeJSON(w, http.StatusOK, contextFitResponse{HasDecision: true, Decision: &dec})
}
