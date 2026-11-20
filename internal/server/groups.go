// User-named routing groups feed (CHECKLIST 174).
//
// GET /dashboard/groups lists the operator-defined routing groups
// from the config file (routing.groups) with live member health:
// each member's enabled flag and display name from the config,
// its live circuit-breaker state, and its real attempt record
// from the engine (attempts, successes, success rate, EWMA
// latency). It is Tier 3 MANAGEMENT, auth-exempt like the other
// dashboard feeds: group names, member provider IDs and live
// counters only — no keys, no base URLs, no message content. An
// install with no groups configured answers an empty list — the
// honest empty state, never an invented group.
package server

import (
	"net/http"
	"sort"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// providerStatsReporter is optionally implemented by engines that
// can report live per-provider attempt records (router.Engine,
// and degrade.StaleEngine which forwards it).
type providerStatsReporter interface {
	AllProviderStats() []router.ProviderStats
}

// groupMemberJSON is one member of a routing group in the feed:
// the provider's identity from the config file, whether it is
// currently routed to, whether its circuit breaker is open, and
// its live attempt record from the engine — real attempts only,
// so SuccessRate and LatencyMs are measured, never assumed.
// HasSignal is false before any attempt: the dashboard then shows
// "no attempts yet" instead of a 0% success rate or a 0 ms
// latency it never measured. Effort is the reasoning effort this
// entry runs at (CHECKLIST 255): "" for no explicit effort, else
// one of the effort levels — the same provider may appear twice
// in one group at different efforts, and each entry is a distinct
// (model, effort) route. The live counters are per provider, not
// per effort (the engine measures attempts per provider), so two
// entries of one provider share the same measured record — the
// effort is what distinguishes them, never a split of the counts.
type groupMemberJSON struct {
	ProviderID  string  `json:"provider_id"`
	Name        string  `json:"name,omitempty"`
	Effort      string  `json:"effort,omitempty"`
	Enabled     bool    `json:"enabled"`
	BreakerOpen bool    `json:"breaker_open"`
	Attempts    int     `json:"attempts"`
	Successes   int     `json:"successes"`
	SuccessRate float64 `json:"success_rate"`
	LatencyMs   float64 `json:"latency_ms"`
	HasSignal   bool    `json:"has_signal"`
}

// groupJSON is one routing group in the feed: its name and its
// members in config order.
type groupJSON struct {
	Name    string            `json:"name"`
	Members []groupMemberJSON `json:"members"`
}

// groupsResponse is the GET /dashboard/groups payload.
type groupsResponse struct {
	Groups []groupJSON `json:"groups"`
}

// groupsFeed builds the live groups feed: group definitions from
// the config file, joined with live member health from the
// engine. Group names are sorted so the feed is deterministic;
// members keep their config order (the try order).
func (s *Server) groupsFeed() groupsResponse {
	groups := []groupJSON{}
	if s.cfg == nil {
		return groupsResponse{Groups: groups}
	}
	// M8.14 race fix, round 2: no s.cfg.Routing nil check
	// here — that load raced routingPersist's pointer swap.
	// GroupsSnapshot takes the nil check under the routing
	// read lock and returns nil when no routing block exists.
	// Display names + enabled flags come from the config file —
	// the engine only knows provider IDs, and the dashboard must
	// show "acme-openai", not "prov_66dc…".
	names := map[string]string{}
	enabled := map[string]bool{}
	for _, p := range s.cfg.Providers {
		if p.ID == "" {
			continue
		}
		names[p.ID] = p.Name
		enabled[p.ID] = p.Enabled
	}
	// Live health comes from the engine: breaker state per
	// provider, and the real attempt record (successes, EWMA
	// latency). Either feed may be absent (an engine that
	// reports neither) — the members then read as no-signal,
	// never as healthy or broken.
	breakers := map[string]bool{}
	if rep, ok := s.engine.(breakerReporter); ok {
		for _, b := range rep.BreakerStates() {
			breakers[b.ProviderID] = b.Open
		}
	}
	stats := map[string]router.ProviderStats{}
	if rep, ok := s.engine.(providerStatsReporter); ok {
		for _, st := range rep.AllProviderStats() {
			stats[st.ProviderID] = st
		}
	}
	// M8.14 race fix: snapshot the groups under the shared
	// routing read lock first — a combo mirror may rewrite
	// the live map while this feed renders. Members come
	// from the snapshot copy, never the live map.
	defs := s.cfg.GroupsSnapshot()
	sortedNames := make([]string, 0, len(defs))
	for name := range defs {
		sortedNames = append(sortedNames, name)
	}
	sort.Strings(sortedNames)
	for _, name := range sortedNames {
		members := make([]groupMemberJSON, 0, len(defs[name]))
		for _, id := range defs[name] {
			// (CHECKLIST 255): a member may carry an effort
			// ("provider@low") — the feed shows each entry
			// separately with its effort, so the same provider
			// twice at different efforts reads as two distinct
			// entries. Members were validated at config load, so
			// ParseGroupMember cannot fail here; an unparseable
			// member is shown under its raw string with no
			// effort rather than dropped.
			providerID, effort, err := config.ParseGroupMember(id)
			if err != nil {
				providerID, effort = id, ""
			}
			m := groupMemberJSON{
				ProviderID:  providerID,
				Effort:      effort,
				Name:        names[providerID],
				Enabled:     enabled[providerID],
				BreakerOpen: breakers[providerID],
			}
			if st, ok := stats[providerID]; ok {
				m.Attempts = st.Attempts
				m.Successes = st.Successes
				m.SuccessRate = st.SuccessRate
				m.LatencyMs = st.LatencyMs
				m.HasSignal = true
			}
			members = append(members, m)
		}
		groups = append(groups, groupJSON{Name: name, Members: members})
	}
	return groupsResponse{Groups: groups}
}

// handleGroups serves GET /dashboard/groups.
func (s *Server) handleGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, s.groupsFeed())
}
