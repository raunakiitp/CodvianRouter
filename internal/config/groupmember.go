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
