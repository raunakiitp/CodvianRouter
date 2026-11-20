// Intent tiers (CHECKLIST 259-260, research-log entry 234 — Aiport):
// `fast` / `balanced` / `best` as a user-facing alias layer over
// (model, effort) routing — the simple-UX answer to "which model do
// I pick". The operator defines what each tier means (which (model,
// effort) pair it maps to, in config `auto_router.intent_tiers` or
// at runtime via POST /dashboard/intent-tiers); the gateway maps
// the intent to that pair. A request names a tier instead of a
// model by using the virtual model `intent/<tier>` (the same
// virtual-model convention as `auto/*`): `intent/fast`,
// `intent/balanced`, `intent/best`. An intent tier with no mapping
// is an honest error naming the tier — never a silent default
// model, never a guess at what "fast" should mean.
//
// EFFORT: low / medium / high, the reasoning-effort half of the
// pair — the same strings as provider.ReasoningEffortLow /
// Medium / High (the engine's per-attempt effort, CHECKLIST 255).
// The effort is resolved, recorded (decision, response headers,
// outcome stats) and reported — and since CHECKLIST 269-270 it
// is also carried onto the engine attempt: the engine's
// resolveAuto path (internal/router) stamps the tier's effort
// onto the request and onto every attempt that carries no effort
// of its own, so the provider actually receives the tier's
// effort on the wire (see provider.ChatRequest.ReasoningEffort
// for which wires forward it) and the trace records it per
// attempt. A routing-group entry's own effort still wins over
// the tier on that entry.
package autorouter

import (
	"fmt"
	"strings"
	"time"
)

// Effort is one reasoning-effort level of an intent-tier mapping.
// "" means unset: the tier maps to a model with no effort pinned.
type Effort string

// Effort levels.
const (
	// EffortLow is the cheapest / fastest reasoning effort.
	EffortLow Effort = "low"
	// EffortMedium is the middle reasoning effort.
	EffortMedium Effort = "medium"
	// EffortHigh is the deepest reasoning effort.
	EffortHigh Effort = "high"
)

// ParseEffort parses an effort name: "" (unset), "low", "medium"
// or "high" (case-insensitive, surrounding whitespace ignored).
// Anything else is an error naming the value — never silently
// coerced to a neighbouring level.
func ParseEffort(s string) (Effort, error) {
	switch Effort(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return "", nil
	case EffortLow:
		return EffortLow, nil
	case EffortMedium:
		return EffortMedium, nil
	case EffortHigh:
		return EffortHigh, nil
	default:
		return "", fmt.Errorf("unknown effort %q (want \"\", \"low\", \"medium\" or \"high\")", s)
	}
}

// IsEffort reports whether s names a known effort level ("",
// "low", "medium", "high").
func IsEffort(s string) bool {
	_, err := ParseEffort(s)
	return err == nil
}

// IntentTier is one intent tier: fast / balanced / best.
type IntentTier string

// Intent tiers.
const (
	// IntentFast is the "just answer quickly" tier.
	IntentFast IntentTier = "fast"
	// IntentBalanced is the middle tier.
	IntentBalanced IntentTier = "balanced"
	// IntentBest is the "best answer, cost/latency second" tier.
	IntentBest IntentTier = "best"
)

// AllIntentTiers lists every intent tier in a stable order.
var AllIntentTiers = []IntentTier{IntentFast, IntentBalanced, IntentBest}

// IsIntentTier reports whether s names a known intent tier.
func IsIntentTier(s string) bool {
	for _, t := range AllIntentTiers {
		if IntentTier(s) == t {
			return true
		}
	}
	return false
}

// IntentModelPrefix is the virtual-model prefix for intent tiers.
const IntentModelPrefix = "intent/"

// IntentModelName returns the virtual model name for a tier
// ("intent/fast").
func IntentModelName(tier IntentTier) string {
	return IntentModelPrefix + string(tier)
}

// IntentVirtualModelNames lists every intent virtual model name in
// a stable order. All three are listed whether or not they are
// mapped: an unmapped tier is a valid request target that answers
// with an honest error naming the tier, never a silent default.
func IntentVirtualModelNames() []string {
	out := make([]string, 0, len(AllIntentTiers))
	for _, t := range AllIntentTiers {
		out = append(out, IntentModelName(t))
	}
	return out
}

// ParseIntentModel parses a virtual model name of the form
// "intent/<tier>": it returns the tier and true when the name is
// exactly that shape with a KNOWN tier. "intent" with no tier,
// "intent/unknown", and any other model name return false — an
// unknown tier name is not a virtual model at all (the request
// fails as an unknown model), while a known-but-unmapped tier is
// a virtual model whose resolution fails honestly naming the tier.
func ParseIntentModel(model string) (IntentTier, bool) {
	if !strings.HasPrefix(model, IntentModelPrefix) {
		return "", false
	}
	tier := IntentTier(strings.TrimSpace(model[len(IntentModelPrefix):]))
	if tier == "" || !IsIntentTier(string(tier)) {
		return "", false
	}
	return tier, true
}

// IsIntentModel reports whether model is an intent virtual model
// ("intent/fast", "intent/balanced", "intent/best").
func IsIntentModel(model string) bool {
	_, ok := ParseIntentModel(model)
	return ok
}

// IntentMapping is one intent tier's operator-defined meaning: the
// concrete model it maps to, and the reasoning effort pinned for
// it ("" = no effort pinned).
type IntentMapping struct {
	Model  string `json:"model"`
	Effort Effort `json:"effort,omitempty"`
}

// String renders a mapping for the suggestion log: "model=X" plus
// " effort=Y" when an effort is pinned; "unmapped" for the zero
// value, so a revert entry can say what it restored.
func (m IntentMapping) String() string {
	if m.Model == "" {
		return "unmapped"
	}
	if m.Effort != "" {
		return "model=" + m.Model + " effort=" + string(m.Effort)
	}
	return "model=" + m.Model
}

// IntentTiers returns a copy of the router's current intent-tier
// mappings (only mapped tiers appear). Safe for concurrent use.
func (r *Router) IntentTiers() map[IntentTier]IntentMapping {
	r.intentMu.RLock()
	defer r.intentMu.RUnlock()
	out := make(map[IntentTier]IntentMapping, len(r.intentTiers))
	for tier, m := range r.intentTiers {
		out[tier] = m
	}
	return out
}

// ResolveIntent returns the tier's current mapping and whether the
// tier is mapped at all. An unmapped tier reports ok=false — the
// caller must surface an honest error naming the tier, never a
// silent default model. Safe for concurrent use.
func (r *Router) ResolveIntent(tier IntentTier) (IntentMapping, bool) {
	r.intentMu.RLock()
	defer r.intentMu.RUnlock()
	m, ok := r.intentTiers[tier]
	return m, ok && m.Model != ""
}

// SetIntentTier defines (or redefines) one intent tier's mapping:
// the operator's definition of what the tier means. It validates
// the tier name, requires a non-empty model, and rejects an
// unknown effort — a bad definition is an error, never a stored
// mapping that fails later at request time. Safe for concurrent
// use with Resolve. An approved learning suggestion applies
// through this method, so the change and the revert share one
// code path.
func (r *Router) SetIntentTier(tier IntentTier, m IntentMapping) error {
	if !IsIntentTier(string(tier)) {
		return fmt.Errorf("unknown intent tier %q (want fast, balanced or best)", tier)
	}
	if strings.TrimSpace(m.Model) == "" {
		return fmt.Errorf("intent tier %q needs a model (empty model is not a mapping)", tier)
	}
	if !IsEffort(string(m.Effort)) {
		return fmt.Errorf("intent tier %q: unknown effort %q (want \"\", \"low\", \"medium\" or \"high\")", tier, m.Effort)
	}
	r.intentMu.Lock()
	defer r.intentMu.Unlock()
	if r.intentTiers == nil {
		r.intentTiers = make(map[IntentTier]IntentMapping)
	}
	r.intentTiers[tier] = IntentMapping{Model: strings.TrimSpace(m.Model), Effort: m.Effort}
	return nil
}

// resolveIntent resolves an "intent/<tier>" virtual model to its
// operator-defined (model, effort) pair. It does NOT classify: an
// intent is explicit, not inferred, so the decision carries no
// tier scores, no threshold and no confidence — Confidence stays 0
// and the Reason says the mapping is operator-defined. An
// unmapped tier returns ok=false with a decision whose Reason
// names the tier and where to map it.
func (r *Router) resolveIntent(virtualModel string, tier IntentTier) (string, Decision, bool) {
	start := time.Now()
	dec := Decision{
		ID:           r.nextDecisionID(),
		VirtualModel: virtualModel,
		IntentTier:   tier,
	}
	mapping, ok := r.ResolveIntent(tier)
	if !ok {
		dec.Reason = fmt.Sprintf("intent tier %q is not mapped: set auto_router.intent_tiers.%s in the config file (or POST /dashboard/intent-tiers) — no silent default model is used", tier, tier)
		dec.LatencyMicros = time.Since(start).Microseconds()
		return "", dec, false
	}
	dec.Model = mapping.Model
	dec.Effort = mapping.Effort
	effort := string(mapping.Effort)
	if effort == "" {
		effort = "unset"
	}
	dec.Reason = fmt.Sprintf("intent tier %q maps to model %s at effort %q (operator-defined)", tier, mapping.Model, effort)
	dec.LatencyMicros = time.Since(start).Microseconds()
	return mapping.Model, dec, true
}
