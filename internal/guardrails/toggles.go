// G18 Area 7 — per-type toggles: the engine half of the
// per-type selection row (OmniRoute: 23 builtin types, per-type
// selection, 8 dashboard presets, GET/PUT /api/settings/pii).
//
// What this file does: every built-in pattern label can be
// enabled or disabled independently, and ListTypes reports all
// of them with their state, so the dashboard (M10) can render
// its toggles and presets from this list instead of a hard-coded
// copy, and the settings endpoint (server, Wave 3) can persist
// the selection by calling SetTypeEnabled.
//
// What it deliberately does NOT do: a disabled type is dropped
// from Check findings — so it can never block and is never
// counted — but the Redacted text still strips it. Log redaction
// is always on in this package (see Redact): a toggle gates
// detection and blocking, never log hygiene. Disabling a type
// is not the same as removing the pattern; the pattern still
// runs, its matches are just not reported. Custom patterns are
// not in this list: disabling one is removing it (see
// RemoveCustomPattern).
package guardrails

import (
	"fmt"
	"sort"
)

// TypeState is one built-in pattern type with its toggle state.
type TypeState struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
}

// knownBuiltinType reports whether pattern is a built-in pattern
// label (see builtinTypes). It is the single validity check for
// per-type toggle names — Config.Validate, SetTypeEnabled and
// TypeEnabled all reject anything it does not know.
func knownBuiltinType(pattern string) bool {
	for _, t := range builtinTypes() {
		if t.Pattern == pattern {
			return true
		}
	}
	return false
}

// builtinTypes lists every built-in pattern label with its kind,
// in table order: redact rules first, then the two synthetic
// findings scan() can produce (private-key whole-block PEM
// redaction, generic-token shape redaction), then injection
// rules. This is the single source of truth for toggle
// validation — a label not in this list cannot be toggled.
func builtinTypes() []TypeState {
	out := make([]TypeState, 0, len(redactRules)+len(injectionRules)+2)
	for _, r := range redactRules {
		out = append(out, TypeState{Pattern: r.pattern, Kind: r.kind, Enabled: true})
	}
	out = append(out,
		TypeState{Pattern: "private-key", Kind: KindSecret, Enabled: true},
		TypeState{Pattern: "generic-token", Kind: KindSecret, Enabled: true},
	)
	for _, r := range injectionRules {
		out = append(out, TypeState{Pattern: r.pattern, Kind: KindInjection, Enabled: true})
	}
	return out
}

// listTypesLocked returns every built-in type with its current
// enabled state. The caller holds s.mu.
func (s *Store) listTypesLocked() []TypeState {
	types := builtinTypes()
	for i := range types {
		if s.disabledTypes[types[i].Pattern] {
			types[i].Enabled = false
		}
	}
	sort.Slice(types, func(i, j int) bool {
		if types[i].Kind != types[j].Kind {
			return types[i].Kind < types[j].Kind
		}
		return types[i].Pattern < types[j].Pattern
	})
	return types
}

// ListTypes returns every built-in pattern label with its
// enabled state, sorted by kind then label. The list is the
// dashboard's toggle surface: toggling a type off here is what
// SetTypeEnabled persists.
func (s *Store) ListTypes() []TypeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listTypesLocked()
}

// SetTypeEnabled enables or disables one built-in pattern type.
// An unknown label is an error — a typo must fail loudly at
// write time, never silently leave the operator believing a
// type is toggled. Disabling a type drops its findings from
// Check results (no counting, no events, no blocking); it does
// not change log redaction, which always strips every pattern.
func (s *Store) SetTypeEnabled(pattern string, enabled bool) error {
	known := false
	for _, t := range builtinTypes() {
		if t.Pattern == pattern {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("guardrails: unknown PII type %q (see ListTypes)", pattern)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabledTypes == nil {
		s.disabledTypes = map[string]bool{}
	}
	if enabled {
		delete(s.disabledTypes, pattern)
	} else {
		s.disabledTypes[pattern] = true
	}
	return nil
}

// TypeEnabled reports whether a built-in pattern type is
// currently enabled. An unknown label is an error.
func (s *Store) TypeEnabled(pattern string) (bool, error) {
	known := false
	for _, t := range builtinTypes() {
		if t.Pattern == pattern {
			known = true
			break
		}
	}
	if !known {
		return false, fmt.Errorf("guardrails: unknown PII type %q (see ListTypes)", pattern)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.disabledTypes[pattern], nil
}

// filterDisabledTypes drops findings whose pattern label is
// disabled. It never touches the redacted text.
func (s *Store) filterDisabledTypes(findings []Finding) []Finding {
	s.mu.Lock()
	disabled := s.disabledTypes
	s.mu.Unlock()
	if len(disabled) == 0 || len(findings) == 0 {
		return findings
	}
	kept := findings[:0]
	for _, f := range findings {
		if !disabled[f.Pattern] {
			kept = append(kept, f)
		}
	}
	return kept
}
