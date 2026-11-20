// G18 Area 7 — prompt-injection severity scoring and the
// 16 KB scan bound.
//
// Severity scale (0–10, per pattern — a pattern's severity is
// what the phrase is trying to do, not how often it appears):
//
//	9–10 direct instruction override ("ignore all previous
//	     instructions") — the request is trying to replace the
//	     system prompt outright;
//	7–8  exfiltration or safety override ("reveal your system
//	     prompt", "override your safety");
//	5–6  role-play / jailbreak framing ("you are now…", "DAN
//	     mode", "jailbreak");
//	1–4  soft reframe ("new instructions:" — suspicious on
//	     its own, decisive only in combination).
//
// Verdict (ScoreInjection): the verdict is taken from the MAX
// severity of any matched pattern and the TOTAL severity of all
// matched patterns —
//
//	block: max >= 8 or total >= 12;
//	warn:  max >= 5 or total >= 8;
//	log:   any match below the warn line;
//	none:  no match.
//
// The thresholds are deliberately conservative on the block
// side: a single "you are now a pirate" (severity 5) warns
// rather than blocks, while any direct override or exfiltration
// attempt blocks. The verdict is advisory for the dashboard
// and for warn/log modes; the request-stage blocking toggle
// (BlockInjection) still blocks on ANY injection match when it
// is on — the scorer does not weaken it.
//
// Scan bound: only the first InjectionScanLimit (16 KB) bytes
// of a text are scanned for injection patterns — in scan()
// (the Check path) and in ScoreInjection alike, so the two
// never disagree. See the comment in scan() for the deliberate
// divergence: injection past 16 KB is unchecked, while PII
// and secret redaction still scan the full text.
package guardrails

import (
	"strings"
)

// InjectionScanLimit is the injection scan bound: 16 KB, the
// same bound the reference router uses. Only the first 16 KB of a text is
// scanned for injection patterns.
const InjectionScanLimit = 16 * 1024

// Injection verdict levels.
const (
	InjectionLevelNone  = "none"
	InjectionLevelLog   = "log"
	InjectionLevelWarn  = "warn"
	InjectionLevelBlock = "block"
)

// injectionSeverity is the severity (0–10 scale, see the file
// header) of each built-in injection pattern, keyed by the
// pattern label in injectionRules.
var injectionSeverity = map[string]int{
	"ignore-instructions":    9,
	"disregard-instructions": 9,
	"forget-instructions":    9,
	"you-are-now":            5,
	"reveal-system-prompt":   8,
	"print-system-prompt":    8,
	"override-instructions":  8,
	"bypass-safety":          7,
	"jailbreak":              6,
	"dan-mode":               6,
	"new-instructions":       4,
}

// InjectionVerdict is the scored outcome of an injection scan:
// the highest single-pattern severity, the summed severity of
// all matched patterns, which patterns matched, and the level
// the thresholds above produce. ScannedBytes is how many bytes
// were actually scanned (the input length, capped at the
// 16 KB bound) — the honest record of what the verdict covers.
type InjectionVerdict struct {
	Level         string   `json:"level"`
	MaxSeverity   int      `json:"max_severity"`
	TotalSeverity int      `json:"total_severity"`
	Patterns      []string `json:"patterns"`
	ScannedBytes  int      `json:"scanned_bytes"`
}

// injectionHead splits s into the head that injection scanning
// covers (at most InjectionScanLimit bytes, cut on a UTF-8
// rune boundary so the head is never a half-rune) and the
// unscanned tail.
func injectionHead(s string) (head, tail string) {
	if len(s) <= InjectionScanLimit {
		return s, ""
	}
	cut := InjectionScanLimit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut], s[cut:]
}

// scoreInjectionVerdict turns matched (pattern, severity)
// pairs into a verdict under the thresholds in the header.
func scoreInjectionVerdict(matched []struct {
	pattern  string
	severity int
}) InjectionVerdict {
	v := InjectionVerdict{Level: InjectionLevelNone, Patterns: []string{}}
	for _, m := range matched {
		if m.severity > v.MaxSeverity {
			v.MaxSeverity = m.severity
		}
		v.TotalSeverity += m.severity
		v.Patterns = append(v.Patterns, m.pattern)
	}
	switch {
	case len(matched) == 0:
		v.Level = InjectionLevelNone
	case v.MaxSeverity >= 8 || v.TotalSeverity >= 12:
		v.Level = InjectionLevelBlock
	case v.MaxSeverity >= 5 || v.TotalSeverity >= 8:
		v.Level = InjectionLevelWarn
	default:
		v.Level = InjectionLevelLog
	}
	return v
}

// ScoreInjection scores the built-in injection patterns in
// text, scanning only the first 16 KB. It is the pure,
// store-free scorer: custom injection patterns are not
// included — use (Store).ScoreInjection for those.
func ScoreInjection(text string) InjectionVerdict {
	head, _ := injectionHead(text)
	folded := foldLower(head)
	found := needlePresence(folded)
	var shapes digitShapes
	if strings.IndexAny(head, "0123456789") >= 0 {
		shapes = scanDigitShapes(head)
	}
	var matched []struct {
		pattern  string
		severity int
	}
	for i, r := range injectionRules {
		if !prefilterRules[len(redactRules)+i].matches(found, shapes) {
			continue
		}
		if !r.re.MatchString(head) {
			continue
		}
		matched = append(matched, struct {
			pattern  string
			severity int
		}{r.pattern, injectionSeverity[r.pattern]})
	}
	v := scoreInjectionVerdict(matched)
	v.ScannedBytes = len(head)
	return v
}

// ScoreInjection scores the built-in injection patterns plus
// this Store's custom injection-kind patterns, scanning only
// the first 16 KB. A custom injection pattern scores its
// configured Severity (0 means the default of 5).
func (s *Store) ScoreInjection(text string) InjectionVerdict {
	v := ScoreInjection(text)
	s.mu.Lock()
	custom := s.custom
	s.mu.Unlock()
	if custom == nil {
		return v
	}
	head, _ := injectionHead(text)
	custom.mu.Lock()
	patterns := make([]storedCustomPattern, len(custom.patterns))
	copy(patterns, custom.patterns)
	custom.mu.Unlock()
	changed := false
	for _, p := range patterns {
		if p.Kind != KindInjection {
			continue
		}
		if !p.re.MatchString(head) {
			continue
		}
		sev := p.Severity
		if sev == 0 {
			sev = 5
		}
		v.Patterns = append(v.Patterns, "custom:"+p.Label)
		if sev > v.MaxSeverity {
			v.MaxSeverity = sev
		}
		v.TotalSeverity += sev
		changed = true
	}
	if !changed {
		return v
	}
	// Re-derive the level from the merged severities.
	switch {
	case v.MaxSeverity >= 8 || v.TotalSeverity >= 12:
		v.Level = InjectionLevelBlock
	case v.MaxSeverity >= 5 || v.TotalSeverity >= 8:
		v.Level = InjectionLevelWarn
	default:
		v.Level = InjectionLevelLog
	}
	return v
}
