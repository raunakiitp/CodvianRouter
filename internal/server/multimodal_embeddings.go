package server

import (
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// handleMultimodalEmbeddings serves POST /v1/multimodal-embeddings.
// It accepts multimodal inputs (text, image parts, objects, or arrays),
// runs admission, budget, and guardrails, and routes through the engine
// with replay exemption.
func (s *Server) handleMultimodalEmbeddings(w http.ResponseWriter, r *http.Request) {
	s.handleEmbedRerank(w, r, provider.EndpointEmbeddings)
}
