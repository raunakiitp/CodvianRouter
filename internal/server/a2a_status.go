// M9.9 A2A operator status: GET /api/a2a/status.
//
// The dashboard codes against this exact response shape — do not
// rename fields:
//
//	{"enabled": false, "tasks_by_state": {"submitted": 0, "working": 0,
//	 "completed": 0, "failed": 0, "cancelled": 0},
//	 "skills": ["smart-routing", "quota-management", "provider-discovery",
//	 "cost-analysis", "health-report", "list-capabilities"],
//	 "protocol_version": "2025-03-26"}
//
// Guard (NOT enforced here — the coordinator registers this route):
// Tier MANAGEMENT, admin scope. The handler itself never checks
// scopes; it assumes the route registration did. The counts are
// operator-only aggregates (see Store.CountsByState), so the
// endpoint must never be reachable by a gateway-key caller.
//
// The handler does not close the store open() returns: ownership
// of the store's lifetime belongs to the opener (the production
// opener returns the server's long-lived store, like a2aService).
package server

import (
	"fmt"
	"net/http"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// a2aStatusSkills returns the M9.9 skill list, derived from the
// A2A skill registry (a2a.SkillDescriptors) in its fixed order,
// so the status response can never drift from the registry when
// a skill is added, removed, or reordered there. A fresh slice
// is built on every call; callers cannot mutate the registry.
func a2aStatusSkills() []string {
	descs := a2a.SkillDescriptors()
	ids := make([]string, len(descs))
	for i, d := range descs {
		ids[i] = d.ID
	}
	return ids
}

// a2aStatusResponse is the exact GET /api/a2a/status shape.
type a2aStatusResponse struct {
	Enabled         bool           `json:"enabled"`
	TasksByState    map[string]int `json:"tasks_by_state"`
	Skills          []string       `json:"skills"`
	ProtocolVersion string         `json:"protocol_version"`
}

// a2aStatusTasksByState converts the store's per-state totals into
// the response map, always carrying all five state keys (even at
// zero) so the dashboard never has to nil-check a missing key.
func a2aStatusTasksByState(counts map[a2a.State]int) map[string]int {
	out := map[string]int{
		string(a2a.StateSubmitted): 0,
		string(a2a.StateWorking):   0,
		string(a2a.StateCompleted): 0,
		string(a2a.StateFailed):    0,
		string(a2a.StateCancelled): 0,
	}
	for state, n := range counts {
		if _, ok := out[string(state)]; ok {
			out[string(state)] = n
		}
	}
	return out
}

// A2AStatusHandler serves GET /api/a2a/status.
//
//   - enabled == false: enabled:false with all-zero counts, the
//     fixed skills list and the protocol version — and open() is
//     NEVER called, so no tasks.db is created and no store is
//     touched while A2A is off.
//   - enabled == true: open() is called and the live per-state
//     counts are returned. If open() (or the count query) fails,
//     the handler answers 503 with an honest "store unavailable"
//     error — never fake zeros.
//   - Any method other than GET answers 405.
func A2AStatusHandler(enabled bool, open func() (*a2a.Store, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		skills := a2aStatusSkills()
		if !enabled {
			writeJSON(w, http.StatusOK, a2aStatusResponse{
				Enabled:         false,
				TasksByState:    a2aStatusTasksByState(nil),
				Skills:          skills,
				ProtocolVersion: mcp.ProtocolVersion,
			})
			return
		}
		store, err := open()
		if err != nil {
			writeServerError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("a2a: store unavailable: %v", err))
			return
		}
		counts, err := store.CountsByState()
		if err != nil {
			writeServerError(w, http.StatusServiceUnavailable,
				fmt.Sprintf("a2a: store unavailable: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, a2aStatusResponse{
			Enabled:         true,
			TasksByState:    a2aStatusTasksByState(counts),
			Skills:          skills,
			ProtocolVersion: mcp.ProtocolVersion,
		})
	}
}
