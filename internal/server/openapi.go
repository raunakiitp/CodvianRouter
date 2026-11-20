// OpenAPI spec for Codvian Router (GET /openapi.json).
//
// The spec is GENERATED from guardRouteTable — the same single
// source of truth the route guard enforces — so it cannot drift
// from the routes the server actually serves. Every operation
// carries the guard tier as a vendor extension, exactly as
// the reference router's OpenAPI consistency check does (research-log entry
// 143): x-loopback-only for Tier 1 (LOCAL_ONLY) and
// x-always-protected for Tier 2 (ALWAYS_PROTECTED); Tier 3
// operations carry neither. TestOpenAPIConsistency is the
// consistency script: it fails if any registered route is missing
// from the spec, any spec operation is missing from the table, or
// any extension disagrees with the tier.
//
// The spec is intentionally minimal — paths, methods, a one-line
// summary per operation, and the tier extensions. It documents
// the management surface (the routes whose behaviour is stable
// and operator-facing); request/response schemas for the chat
// endpoint follow the OpenAI-compatible wire format and are not
// re-specified here.
package server

import (
	"net/http"
	"strings"
)

// openAPIInfo is the spec's info block.
type openAPIInfo struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

// openAPIOperation is one method on one path.
type openAPIOperation struct {
	Summary         string `json:"summary"`
	LoopbackOnly    *bool  `json:"x-loopback-only,omitempty"`
	AlwaysProtected *bool  `json:"x-always-protected,omitempty"`
	SpawnCapable    *bool  `json:"x-spawn-capable,omitempty"`
}

// openAPISpec is the whole document.
type openAPISpec struct {
	OpenAPI string                                 `json:"openapi"`
	Info    openAPIInfo                            `json:"info"`
	Servers []openAPIServer                        `json:"servers,omitempty"`
	Paths   map[string]map[string]openAPIOperation `json:"paths"`
}

type openAPIServer struct {
	URL string `json:"url"`
}

func boolPtr(b bool) *bool { return &b }

// handleOpenAPI serves GET /openapi.json: the machine-readable
// spec generated live from the guard route table. It is Tier 3
// with AuthExempt (like the other dashboard feeds): the spec
// describes the public surface and carries no secrets — route
// paths, tiers, and one-line descriptions only.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	version := s.version
	if version == "" {
		version = "0.1.0"
	}
	spec := openAPISpec{
		OpenAPI: "3.0.3",
		Info: openAPIInfo{
			Title:   "Codvian Router API",
			Version: version,
		},
		Paths: map[string]map[string]openAPIOperation{},
	}
	if addr := s.listenAddr; addr != "" {
		host := addr
		if strings.HasPrefix(host, ":") {
			host = "127.0.0.1" + host
		}
		spec.Servers = []openAPIServer{{URL: "http://" + host}}
	}
	for _, route := range RouteTable() {
		methods, ok := spec.Paths[route.Path]
		if !ok {
			methods = map[string]openAPIOperation{}
			spec.Paths[route.Path] = methods
		}
		op := openAPIOperation{Summary: route.Description}
		switch route.Tier {
		case TierLocalOnly:
			op.LoopbackOnly = boolPtr(true)
		case TierAlwaysProtected:
			op.AlwaysProtected = boolPtr(true)
		}
		if route.SpawnCapable {
			op.SpawnCapable = boolPtr(true)
		}
		methods[strings.ToLower(route.Method)] = op
	}
	writeJSON(w, http.StatusOK, spec)
}
