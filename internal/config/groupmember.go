// Routing-group members with a reasoning effort (CHECKLIST 255,
// research-log entries 230–231, Magpie v0.1.4405): a group entry
// is (model, effort), not model alone — the same model at
// different reasoning efforts is a distinct cost/latency/quality
// point, so a group may hold the same provider twice at different
// efforts.
//
// Member syntax in the config file (routing.groups): a plain
// provider ID ("openai") or a provider ID with an effort suffix
// ("openai@low"). The suffix is split on the LAST "@": a provider
// ID may itself contain "@" — only a suffix that is a known effort
// level is read as an effort, anything else is part of the
// provider ID, so an existing member that is not an effort form
// keeps meaning exactly what it meant. A provider ID that
// literally ends in "@low" (or any other level) is ambiguous and
// the effort reading wins — the operator renames the provider if
// that ever matters; no provider ID in the shipped catalog ends
// in an effort level.
package config

import (
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ParseGroupMember parses one routing.groups member into its
// provider ID and reasoning effort ("no explicit effort" when the
// member names no effort). An unknown effort suffix is an error
// naming the member and the level — a typo must fail at load,
// never route at a silently dropped effort.
func ParseGroupMember(member string) (providerID, effort string, err error) {
	member = strings.TrimSpace(member)
	if member == "" {
		return "", "", fmt.Errorf("routing group member is empty")
	}
	at := strings.LastIndex(member, "@")
	if at < 0 {
		return member, "", nil
	}
	suffix := member[at+1:]
	if suffix == "" {
		return "", "", fmt.Errorf("routing group member %q has an empty effort after \"@\": use \"provider\" or \"provider@<effort>\" (one of %s)", member, strings.Join(provider.ReasoningEfforts, ", "))
	}
	if !provider.ValidReasoningEffort(suffix) {
		return "", "", fmt.Errorf("routing group member %q names unknown reasoning effort %q (one of %s)", member, suffix, strings.Join(provider.ReasoningEfforts, ", "))
	}
	id := strings.TrimSpace(member[:at])
	if id == "" {
		return "", "", fmt.Errorf("routing group member %q names an effort but no provider: use \"provider@<effort>\"", member)
	}
	return id, provider.NormalizeReasoningEffort(suffix), nil
}

// validateGroupGraph rejects a cycle among nested group refs
// (routing.groups members of the form "group:<name>"). Each group is
// a node, each "group:" member a directed edge to the inner group; a
// cycle would infinite-loop the recursive expansion at request time,
// so it is a load error naming the cycle path (a -> b -> a). Self
// refs are also cycles. Non-"group:" members are not edges.
func validateGroupGraph(groups map[string][]string) error {
	const (
		white = 0 // unvisited
		gray  = 1 // on the current DFS path
		black = 2 // fully explored
	)
	state := make(map[string]int, len(groups))
	var path []string
	var visit func(name string) error
	visit = func(name string) error {
		switch state[name] {
		case gray:
			// found a cycle: report from the first occurrence
			start := 0
			for i, p := range path {
				if p == name {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), path[start:]...), name)
			return fmt.Errorf("config: routing.groups has a cycle: %s", strings.Join(cycle, " -> "))
		case black:
			return nil
		}
		state[name] = gray
		path = append(path, name)
		for _, m := range groups[name] {
			if !strings.HasPrefix(m, "group:") {
				continue
			}
			inner := strings.TrimSpace(m[len("group:"):])
			if inner == "" {
				continue // empty name already rejected per-member
			}
			if _, ok := groups[inner]; !ok {
				continue // unknown inner already rejected per-member
			}
			if err := visit(inner); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[name] = black
		return nil
	}
	for name := range groups {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

// ExpandGroupMembers flattens one group's member list, recursively
// expanding any "group:<name>" member into the inner group's members
// (depth-first, member order preserved, depth-first). It returns the
// flattened provider-member list and ok=false when the group is
// unknown. A visited set guards against cycles defensively at runtime
// (cycles are also rejected at load by validateGroupGraph): a group
// already on the expansion stack contributes nothing, so the call
// always terminates. The input map is treated read-only.
func ExpandGroupMembers(groups map[string][]string, name string) ([]string, bool) {
	if groups == nil {
		return nil, false
	}
	if _, ok := groups[name]; !ok {
		return nil, false
	}
	var out []string
	var expand func(g string, visited map[string]bool)
	expand = func(g string, visited map[string]bool) {
		if visited[g] {
			return
		}
		visited[g] = true
		members, ok := groups[g]
		if !ok {
			return
		}
		for _, id := range members {
			if rest, isRef := strings.CutPrefix(id, "group:"); isRef {
				expand(strings.TrimSpace(rest), visited)
				continue
			}
			out = append(out, id)
		}
	}
	expand(name, map[string]bool{})
	return out, true
}
