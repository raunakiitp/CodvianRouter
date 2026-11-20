// G18 Area 7 — custom regex patterns: the engine half of the
// custom-patterns row (OmniRoute: up to 20, safe-regex validated,
// test endpoint in piiCustomPatterns.ts).
//
// What this file does: a store of up to MaxCustomPatterns
// operator-defined patterns, each validated before it is stored,
// a match function the Check path runs alongside the built-ins,
// and TestCustomPattern — the pure function the settings test
// endpoint calls with a candidate pattern and a sample input.
//
// Safe-regex validation rule (exact, documented so the endpoint
// can quote it): a pattern is accepted only if ALL of these
// hold —
//  1. it compiles under Go's regexp (RE2 syntax);
//  2. it is at most MaxCustomPatternLen bytes long;
//  3. it cannot match the empty string (an empty-matching
//     pattern would redact everywhere);
//  4. its syntax tree contains no nested quantifier (a
//     repetition whose subtree contains another repetition,
//     e.g. "(a+)+", "(a*)*" — the classic
//     catastrophic-backtracking shapes) and no repetition of
//     a subexpression that can match empty (e.g. "(a?)+");
//  5. it runs in under 100 ms against a 4 KB adversarial
//     input (a run of "a"s with a trailing mismatch).
//
// Rule 5 is belt-and-braces: Go's regexp is RE2, which is
// linear-time by construction and cannot catastrophically
// backtrack at all — the check exists so a pattern that is
// somehow slow in practice is still rejected, and so the rule
// stays valid if a pattern is ever exported to a backtracking
// engine (for example the dashboard's JS preview).
//
// Custom patterns run inside Check/CheckScoped after the
// built-ins, on the progressively redacted text, with kind
// "pii" by default (or "injection" when Kind is set to
// KindInjection — those ride on the injection scorer instead
// of the PII redaction path; see ScoreInjection). Their
// findings carry pattern "custom:<label>" and count toward
// the per-pattern counters like any built-in. The pure
// Redact()/RedactLogLine() path does NOT run custom patterns:
// it has no store to read them from, and log redaction must
// stay a pure function of the text.
package guardrails

import (
	"fmt"
	"regexp"
	"regexp/syntax"
	"strings"
	"sync"
	"time"
)

// MaxCustomPatterns is the operator pattern cap: 20, matching
// OmniRoute's cap.
const MaxCustomPatterns = 20

// MaxCustomPatternLen caps one pattern at 500 bytes.
const MaxCustomPatternLen = 500

// maxCustomPatternProbe is the wall-clock budget for the
// adversarial-input probe in ValidateCustomPattern: 100 ms.
const maxCustomPatternProbe = 100 * time.Millisecond

// CustomPattern is one operator-defined pattern. ID is the
// stable key for Remove (any non-empty string, e.g. a slug of
// the label); Label is the human name shown in findings as
// "custom:<label>"; Pattern is the RE2 regex; Kind is
// KindPII (default) or KindInjection; Severity is the
// injection severity (0–10) used only when Kind is
// KindInjection — 0 means the default of 5.
type CustomPattern struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Pattern  string `json:"pattern"`
	Kind     string `json:"kind"`
	Severity int    `json:"severity"`
}

// CustomPatternStore holds the operator's custom patterns with
// their compiled forms. The zero value is not usable — use
// NewCustomPatternStore. It is safe for concurrent use.
type CustomPatternStore struct {
	mu       sync.Mutex
	patterns []storedCustomPattern
}

type storedCustomPattern struct {
	CustomPattern
	re *regexp.Regexp
}

// NewCustomPatternStore returns an empty custom-pattern store.
func NewCustomPatternStore() *CustomPatternStore {
	return &CustomPatternStore{}
}

// ValidateCustomPattern applies the safe-regex rule documented
// in this file's header. It returns nil only when the pattern
// is safe to store and run.
func ValidateCustomPattern(pattern string) error {
	if len(pattern) == 0 {
		return fmt.Errorf("guardrails: custom pattern must not be empty")
	}
	if len(pattern) > MaxCustomPatternLen {
		return fmt.Errorf("guardrails: custom pattern is %d bytes, over the %d-byte cap", len(pattern), MaxCustomPatternLen)
	}
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return fmt.Errorf("guardrails: custom pattern does not compile: %v", err)
	}
	if canMatchEmpty(re) {
		return fmt.Errorf("guardrails: custom pattern can match the empty string")
	}
	if hasNestedRepeat(re) {
		return fmt.Errorf("guardrails: custom pattern has a nested quantifier (a repetition inside a repetition) — rejected as unsafe")
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("guardrails: custom pattern does not compile: %v", err)
	}
	// Adversarial-input probe: a long run of "a" with a
	// trailing mismatch is the input nested quantifiers blow
	// up on in backtracking engines. RE2 is linear-time, so
	// this is expected to take microseconds; the budget is
	// the backstop.
	probe := strings.Repeat("a", 4096) + "!"
	start := time.Now()
	compiled.FindAllStringIndex(probe, -1)
	if time.Since(start) > maxCustomPatternProbe {
		return fmt.Errorf("guardrails: custom pattern took too long on the adversarial probe (>%s)", maxCustomPatternProbe)
	}
	return nil
}

// canMatchEmpty reports whether the syntax tree can match the
// empty string.
func canMatchEmpty(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText, syntax.OpWordBoundary,
		syntax.OpNoWordBoundary:
		return true
	case syntax.OpLiteral:
		return len(re.Rune) == 0
	case syntax.OpCharClass, syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return false
	case syntax.OpCapture:
		return canMatchEmpty(re.Sub[0])
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			if !canMatchEmpty(sub) {
				return false
			}
		}
		return true
	case syntax.OpAlternate:
		for _, sub := range re.Sub {
			if canMatchEmpty(sub) {
				return true
			}
		}
		return false
	case syntax.OpRepeat:
		return re.Min == 0 || canMatchEmpty(re.Sub[0])
	case syntax.OpQuest, syntax.OpStar:
		return true
	case syntax.OpPlus:
		return canMatchEmpty(re.Sub[0])
	}
	return false
}

// hasNestedRepeat reports whether the syntax tree contains a
// repetition whose subtree contains another repetition, or a
// repetition of a subexpression that can match empty — the two
// shapes that make backtracking engines blow up.
func hasNestedRepeat(re *syntax.Regexp) bool {
	if re.Op == syntax.OpRepeat || re.Op == syntax.OpPlus || re.Op == syntax.OpStar || re.Op == syntax.OpQuest {
		sub := re.Sub[0]
		if canMatchEmpty(sub) {
			return true
		}
		if containsRepeat(sub) {
			return true
		}
	}
	for _, sub := range re.Sub {
		if hasNestedRepeat(sub) {
			return true
		}
	}
	return false
}

// containsRepeat reports whether the tree contains any
// repetition node.
func containsRepeat(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpRepeat, syntax.OpPlus, syntax.OpStar, syntax.OpQuest:
		return true
	}
	for _, sub := range re.Sub {
		if containsRepeat(sub) {
			return true
		}
	}
	return false
}

// Add validates and stores a pattern. It fails when the store
// already holds MaxCustomPatterns, when the ID or label is
// empty, when the ID is already taken, when the kind is not
// "pii" or "injection", or when the pattern fails validation.
func (c *CustomPatternStore) Add(p CustomPattern) error {
	if p.ID == "" {
		return fmt.Errorf("guardrails: custom pattern id must not be empty")
	}
	if p.Label == "" {
		return fmt.Errorf("guardrails: custom pattern label must not be empty")
	}
	kind := p.Kind
	if kind == "" {
		kind = KindPII
	}
	if kind != KindPII && kind != KindInjection {
		return fmt.Errorf("guardrails: custom pattern kind must be %q or %q, got %q", KindPII, KindInjection, p.Kind)
	}
	if err := ValidateCustomPattern(p.Pattern); err != nil {
		return err
	}
	compiled, err := regexp.Compile(p.Pattern)
	if err != nil {
		return fmt.Errorf("guardrails: custom pattern does not compile: %v", err)
	}
	p.Kind = kind
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.patterns) >= MaxCustomPatterns {
		return fmt.Errorf("guardrails: custom pattern cap reached (%d) — remove one first", MaxCustomPatterns)
	}
	for _, existing := range c.patterns {
		if existing.ID == p.ID {
			return fmt.Errorf("guardrails: custom pattern id %q already exists", p.ID)
		}
	}
	c.patterns = append(c.patterns, storedCustomPattern{CustomPattern: p, re: compiled})
	return nil
}

// Remove deletes the pattern with the given ID. It reports
// whether anything was removed.
func (c *CustomPatternStore) Remove(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.patterns {
		if p.ID == id {
			c.patterns = append(c.patterns[:i], c.patterns[i+1:]...)
			return true
		}
	}
	return false
}

// List returns a snapshot of the stored patterns in insertion
// order.
func (c *CustomPatternStore) List() []CustomPattern {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CustomPattern, len(c.patterns))
	for i, p := range c.patterns {
		out[i] = p.CustomPattern
	}
	return out
}

// matchAll runs every stored pattern over text (in insertion
// order), replacing matches with a placeholder and returning
// the rewritten text plus one finding per pattern that matched.
// PII-kind patterns replace with [REDACTED:CUSTOM:<label>];
// injection-kind patterns replace with [REDACTED:INJECTION].
// It is the store half of the Check-path integration; the
// Store half (applyCustomPatterns) adds the findings to the
// result.
func (c *CustomPatternStore) matchAll(text string) (string, []Finding) {
	c.mu.Lock()
	patterns := make([]storedCustomPattern, len(c.patterns))
	copy(patterns, c.patterns)
	c.mu.Unlock()
	var findings []Finding
	out := text
	for _, p := range patterns {
		matches := p.re.FindAllStringIndex(out, -1)
		if len(matches) == 0 {
			continue
		}
		label := "custom:" + p.Label
		placeholder := "[REDACTED:CUSTOM:" + p.Label + "]"
		if p.Kind == KindInjection {
			placeholder = "[REDACTED:INJECTION]"
		}
		findings = append(findings, Finding{Kind: p.Kind, Pattern: label, Count: len(matches)})
		out = p.re.ReplaceAllString(out, placeholder)
	}
	return out, findings
}

// TestCustomPattern is the pure function the settings test
// endpoint calls: it validates pattern under the safe-regex
// rule and, when valid, runs it against input and returns
// whether it matched plus the matched substrings (at most the
// first 10). An invalid pattern returns an error and no
// matches; a valid pattern that does not match returns
// (false, nil, nil).
func TestCustomPattern(pattern, input string) (bool, []string, error) {
	if err := ValidateCustomPattern(pattern); err != nil {
		return false, nil, err
	}
	re := regexp.MustCompile(pattern)
	matches := re.FindAllString(input, 10)
	if len(matches) == 0 {
		return false, nil, nil
	}
	return true, matches, nil
}

// customStore returns the Store's custom-pattern store,
// creating it on first use. The caller must NOT hold s.mu:
// the custom store has its own mutex.
func (s *Store) customStore() *CustomPatternStore {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.custom == nil {
		s.custom = NewCustomPatternStore()
	}
	return s.custom
}

// AddCustomPattern validates and stores one custom pattern on
// this Store.
func (s *Store) AddCustomPattern(p CustomPattern) error {
	return s.customStore().Add(p)
}

// RemoveCustomPattern deletes the custom pattern with the
// given ID. It reports whether anything was removed.
func (s *Store) RemoveCustomPattern(id string) bool {
	return s.customStore().Remove(id)
}

// ListCustomPatterns returns a snapshot of this Store's custom
// patterns in insertion order.
func (s *Store) ListCustomPatterns() []CustomPattern {
	return s.customStore().List()
}

// applyCustomPatterns runs the Store's custom patterns over
// the redacted text and appends their findings. It is a
// no-op (same text, same findings) when no custom pattern is
// stored, so the common path pays one nil check.
func (s *Store) applyCustomPatterns(redacted string, findings []Finding) (string, []Finding) {
	s.mu.Lock()
	custom := s.custom
	s.mu.Unlock()
	if custom == nil {
		return redacted, findings
	}
	out, customFindings := custom.matchAll(redacted)
	if len(customFindings) == 0 {
		return redacted, findings
	}
	return out, append(findings, customFindings...)
}
