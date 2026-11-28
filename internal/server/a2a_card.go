// M9.9 A2A Agent Card: GET /.well-known/agent.json.
//
// A pure handler: it serves only the static discovery document for
// the A2A task interface (POST /a2a, internal/server/a2a.go) and
// carries no task data, no keys, and no provider details. The skill
// list is injected by the caller (internal/a2a.SkillDescriptors,
// wired by the coordinator at route registration) so this file
// never depends on how skills are produced.
//
// Field names follow the A2A agent-card shape (name, description,
// url, version, defaultInputModes, defaultOutputModes,
// capabilities.streaming / capabilities.pushNotifications, skills
// with id/name/description/tags) as published by the sector-typical
// agent-card route — shape only, no the sector baseline code is copied.
package server

import (
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// agentCardCapabilities is the A2A capabilities block.
// pushNotifications is false because no push mechanism exists —
// claiming it would advertise a delivery channel the router
// cannot honour.
type agentCardCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
}

// agentCard is the A2A agent-card document served at
// /.well-known/agent.json.
type agentCard struct {
	Name               string                `json:"name"`
	Description        string                `json:"description"`
	URL                string                `json:"url"`
	Version            string                `json:"version"`
	DefaultInputModes  []string              `json:"defaultInputModes"`
	DefaultOutputModes []string              `json:"defaultOutputModes"`
	Capabilities       agentCardCapabilities `json:"capabilities"`
	Skills             []a2a.SkillDescriptor `json:"skills"`
}

// AgentCardHandler serves GET /.well-known/agent.json: the static
// A2A agent card for this router. skills are served verbatim, in
// the order given; baseURL is the router's public base URL and the
// card's url is baseURL + "/a2a" (the A2A JSON-RPC endpoint).
// version is injected by the caller — the server passes s.version
// (WithVersion, the same source GET /openapi.json reports, with
// the same "" → "0.1.0" fallback applied at registration) — so
// the card can never drift from --version: there is no version
// literal in this file to forget on a bump. The handler itself
// applies no fallback: it serves exactly the version it was
// given. Any method other than GET is rejected with 405.
func AgentCardHandler(skills []a2a.SkillDescriptor, baseURL, version string) http.HandlerFunc {
	// Snapshot the descriptors so a later mutation by the caller
	// cannot change what the card serves. The values are verbatim:
	// id, name, description and tags are passed through untouched.
	// The copy is deep per descriptor: each Tags slice is cloned,
	// so mutating a caller's Tags element after handler creation
	// cannot change the served card.
	snapshot := make([]a2a.SkillDescriptor, len(skills))
	for i, s := range skills {
		snapshot[i] = s
		if s.Tags != nil {
			tags := make([]string, len(s.Tags))
			copy(tags, s.Tags)
			snapshot[i].Tags = tags
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		card := agentCard{
			Name:               "Codvian Router",
			Description:        "Local-first LLM router with an Agent-to-Agent task interface for routing requests across configured providers.",
			URL:                baseURL + "/a2a",
			Version:            version,
			DefaultInputModes:  []string{"text"},
			DefaultOutputModes: []string{"text"},
			Capabilities: agentCardCapabilities{
				Streaming:         true,
				PushNotifications: false,
			},
			Skills: snapshot,
		}
		writeJSON(w, http.StatusOK, card)
	}
}
