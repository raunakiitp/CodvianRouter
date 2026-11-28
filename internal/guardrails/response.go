// G18 Area 7 — response sanitization, non-streaming: the
// engine half of the response-sanitization row (the sector baseline:
// opt-in PII_RESPONSE_SANITIZATION, modes redact|warn|block,
// default redact).
//
// Modes:
//   - redact (default): PII/secrets in the response body are
//     replaced by placeholders and the redacted body is what
//     the client gets; the findings are counted and logged.
//   - warn: the body is delivered unchanged; the findings are
//     counted, logged and returned so the operator can see
//     what would have been redacted.
//   - block: the body is withheld entirely (empty body +
//     Blocked) when any finding is present; the attempt stays
//     in the ledger and metrics as an error, exactly like the
//     existing response-block behaviour — what is withheld is
//     only the delivery, never the billing record.
//
// Per-type toggles gate findings and blocking, exactly as in
// Check: a disabled type produces no finding and can never
// block a response. The redacted body itself still strips
// every pattern — toggles gate detection, not redaction (see
// the note in checkFiltered).
//
// The Store's mode (ResponseMode/SetResponseMode) is the
// server's to wire to a setting; the pure RedactResponse takes
// the mode as a parameter so tests and the stream redactor
// can use it without a store.
package guardrails

import (
	"encoding/json"
	"strings"
)

// ResponseGuardMode is a response-sanitization mode:
// redact, warn or block.
type ResponseGuardMode string

const (
	// ResponseModeRedact replaces PII/secrets in the body
	// and delivers the redacted body. It is the default.
	ResponseModeRedact ResponseGuardMode = "redact"
	// ResponseModeWarn delivers the body unchanged and only
	// counts/logs the findings.
	ResponseModeWarn ResponseGuardMode = "warn"
	// ResponseModeBlock withholds the body when any finding
	// is present.
	ResponseModeBlock ResponseGuardMode = "block"
)

// Valid reports whether m is a known response mode.
func (m ResponseGuardMode) Valid() bool {
	switch m {
	case ResponseModeRedact, ResponseModeWarn, ResponseModeBlock:
		return true
	}
	return false
}

// ResponseCheckResult is the outcome of a response check.
type ResponseCheckResult struct {
	// Body is what the client gets: the redacted body in
	// redact mode, the original body in warn mode, and the
	// empty string in block mode when blocked.
	Body string `json:"-"`
	// Findings lists the patterns that matched, by kind +
	// label (labels only, never the matched values).
	Findings []Finding `json:"findings"`
	// Action is the evaluation action recorded for this
	// check: allowed, redacted, warned or blocked.
	Action string `json:"action"`
	// Blocked reports whether the body was withheld.
	Blocked bool `json:"blocked"`
	// Mode is the mode that produced this result.
	Mode ResponseGuardMode `json:"mode"`
}

// ResponseMode returns the Store's response-guardrail mode
// (default redact).
func (s *Store) ResponseMode() ResponseGuardMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.respMode == "" {
		return ResponseModeRedact
	}
	return s.respMode
}

// SetResponseMode sets the Store's response-guardrail mode. An
// unknown mode is an error and leaves the mode unchanged.
func (s *Store) SetResponseMode(mode ResponseGuardMode) error {
	if !mode.Valid() {
		return &unknownModeError{mode: mode}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respMode = mode
	return nil
}

type unknownModeError struct {
	mode ResponseGuardMode
}

func (e *unknownModeError) Error() string {
	return "guardrails: unknown response mode " + string(e.mode) + " (want redact, warn or block)"
}

// RedactResponse applies a response mode to one response body
// (plain text). It is the pure function: it scans the body,
// redacts or withholds per the mode, and reports the findings
// and the action. An invalid mode behaves as redact.
func RedactResponse(body string, mode ResponseGuardMode) ResponseCheckResult {
	if !mode.Valid() {
		mode = ResponseModeRedact
	}
	redacted, findings := scan(body)
	res := ResponseCheckResult{Mode: mode, Findings: findings}
	if len(findings) == 0 {
		res.Body = body
		res.Action = ActionAllowed
		return res
	}
	switch mode {
	case ResponseModeWarn:
		res.Body = body
		res.Action = ActionWarned
	case ResponseModeBlock:
		res.Body = ""
		res.Action = ActionBlocked
		res.Blocked = true
	default: // redact
		res.Body = redacted
		res.Action = ActionRedacted
	}
	return res
}

// CheckResponse is the Store version of RedactResponse: it
// applies the mode, then records the check under phase
// "response" (checked/redacted/blocked totals, per-kind and
// per-pattern counters, one evaluation, one event per finding
// kind). Per-type toggles and custom patterns apply exactly
// as in Check. The Store's own mode is used when mode is
// empty; an invalid non-empty mode behaves as redact.
func (s *Store) CheckResponse(body string, mode ResponseGuardMode) ResponseCheckResult {
	if mode == "" {
		mode = s.ResponseMode()
	}
	redacted, findings := scan(body)
	findings = s.filterDisabledTypes(findings)
	redacted, findings = s.applyCustomPatterns(redacted, findings)
	res := ResponseCheckResult{Mode: mode, Findings: findings}
	if len(findings) == 0 {
		res.Body = body
		res.Action = ActionAllowed
		s.mu.Lock()
		s.checked++
		s.recordEvalLocked(PhaseResponse, ActionAllowed)
		s.mu.Unlock()
		return res
	}
	switch mode {
	case ResponseModeWarn:
		res.Body = body
		res.Action = ActionWarned
	case ResponseModeBlock:
		res.Body = ""
		res.Action = ActionBlocked
		res.Blocked = true
	default:
		res.Body = redacted
		res.Action = ActionRedacted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFindingsLocked(PhaseResponse, findings, res.Action, res.Action == ActionRedacted, truncateRunes(redacted, maxPreviewRunes))
	return res
}

// ResponseText extracts the text surfaces of a relay response
// body for guardrail detection: OpenAI choices[].message
// .content (string or content-part array), Anthropic
// content[].text, Responses-API output[].content[].text, and
// plain {"content": "..."} / {"text": "..."} objects. When the
// body is not JSON, or none of those shapes is present, the
// raw body is returned — scanning the whole JSON is the safe
// fallback, and a secret in any field is still caught.
func ResponseText(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return trimmed
	}
	var parts []string
	collectContentParts(decoded, &parts)
	if len(parts) == 0 {
		return trimmed
	}
	return strings.Join(parts, "\n")
}

// collectContentParts walks a decoded JSON value and appends
// the text surfaces named in ResponseText. It descends only
// into the shapes that carry response text — "choices",
// "output", "content" and "message" — so model names, usage
// counts and finish reasons are never treated as text to
// scan.
func collectContentParts(v any, parts *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if content, ok := t["content"]; ok {
			collectContentValue(content, parts)
		}
		if text, ok := t["text"].(string); ok {
			*parts = append(*parts, text)
		}
		if choices, ok := t["choices"].([]any); ok {
			for _, ch := range choices {
				if m, ok := ch.(map[string]any); ok {
					if msg, ok := m["message"]; ok {
						collectContentParts(msg, parts)
					}
					if content, ok := m["content"]; ok {
						collectContentValue(content, parts)
					}
				}
			}
		}
		if output, ok := t["output"].([]any); ok {
			for _, item := range output {
				if m, ok := item.(map[string]any); ok {
					if content, ok := m["content"]; ok {
						collectContentValue(content, parts)
					}
				}
			}
		}
	case []any:
		for _, item := range t {
			collectContentParts(item, parts)
		}
	case string:
		*parts = append(*parts, t)
	}
}

// collectContentValue appends the text in a "content" value:
// a plain string, or an array of parts each carrying "text".
func collectContentValue(content any, parts *[]string) {
	switch t := content.(type) {
	case string:
		*parts = append(*parts, t)
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					*parts = append(*parts, text)
				}
			}
		}
	}
}

// CheckResponsePayload checks a relay response body (raw JSON).
// Detection and redaction both run on the raw body, so the
// findings always describe exactly what was redacted in the
// delivered body: scanning the extracted text alone would miss
// a secret the model put in a tool-call argument or another
// non-text field, and the two scans could then disagree about
// what matched. ResponseText stays available for callers that
// want text-surface-only detection (for example to name what
// the model said in a finding preview). In redact mode the
// returned Body is the raw JSON with every PII/secret match
// redacted in place — the JSON structure is preserved, only
// the values change. In warn mode the raw body is returned
// unchanged; in block mode the body is withheld. Counting is
// under phase "response", as in CheckResponse.
func (s *Store) CheckResponsePayload(body []byte, mode ResponseGuardMode) ResponseCheckResult {
	if mode == "" {
		mode = s.ResponseMode()
	}
	raw := string(body)
	rawRedacted, findings := scan(raw)
	findings = s.filterDisabledTypes(findings)
	rawRedacted, findings = s.applyCustomPatterns(rawRedacted, findings)
	res := ResponseCheckResult{Mode: mode, Findings: findings}
	if len(findings) == 0 {
		res.Body = raw
		res.Action = ActionAllowed
		s.mu.Lock()
		s.checked++
		s.recordEvalLocked(PhaseResponse, ActionAllowed)
		s.mu.Unlock()
		return res
	}
	switch mode {
	case ResponseModeWarn:
		res.Body = raw
		res.Action = ActionWarned
	case ResponseModeBlock:
		res.Body = ""
		res.Action = ActionBlocked
		res.Blocked = true
	default:
		res.Body = rawRedacted
		res.Action = ActionRedacted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordFindingsLocked(PhaseResponse, findings, res.Action, res.Action == ActionRedacted, truncateRunes(rawRedacted, maxPreviewRunes))
	return res
}
