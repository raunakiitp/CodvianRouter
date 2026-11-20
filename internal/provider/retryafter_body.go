// Retry-After hints carried in an upstream error BODY (M4.4).
//
// OmniRoute accountFallback.ts:1581-1618 + retryAfterJson.ts parses the
// hint from the JSON error body when no Retry-After header is present:
// Gemini's google.rpc.RetryInfo (`retryDelay: "30s"`), free-text
// "retry in Xs" / "retry after Xs" shapes, ISO 8601 durations, and
// "resets in 1h2m3s" shapes. Codvian previously parsed the header
// only (parseRetryAfter in client.go); a body hint was silently
// dropped and the router fell back to exponential backoff. The body
// hint is strictly a fallback: a present, parsable header always
// wins (see mapStatusErrorForEndpoint).
package provider

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// retryInfoDelayRE matches a google.rpc.RetryInfo retryDelay value:
// `"retryDelay": "30s"` (protobuf Duration JSON: seconds with an
// optional fractional part, always suffixed "s").
var retryInfoDelayRE = regexp.MustCompile(`"retryDelay"\s*:\s*"([0-9]+(?:\.[0-9]+)?s)"`)

// retryInRE matches free-text "retry in 5s" / "retry after 10 seconds"
// / "retry in 2 minutes" shapes inside an error message.
var retryInRE = regexp.MustCompile(`(?i)retry\s+(?:in|after)\s+([0-9]+(?:\.[0-9]+)?)\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h|milliseconds?|ms)\b`)

// resetsInRE matches "resets in 1h2m3s" / "reset in 90s" shapes: a
// run of <number><unit> legs (h/m/s, optional spaces between legs).
var resetsInRE = regexp.MustCompile(`(?i)resets?\s+in\s+([0-9]+(?:\.[0-9]+)?(?:h|m|s)(?:\s*[0-9]+(?:\.[0-9]+)?(?:h|m|s))*)`)

// isoDurationRE matches ISO 8601 durations: PT30S, PT1M30S, PT2H, P1DT2H.
var isoDurationRE = regexp.MustCompile(`(?i)P(?:([0-9]+)D)?T(?:([0-9]+)H)?(?:([0-9]+)M)?(?:([0-9]+(?:\.[0-9]+)?)S)?`)

// ParseRetryAfterBody extracts a Retry-After duration from an upstream
// error body, or (0, false) when the body carries no usable hint. The
// shapes, in precedence order:
//
//  1. google.rpc.RetryInfo retryDelay ("30s", "1.5s") — Gemini.
//  2. "retry in Xs" / "retry after Xs" free text (any unit).
//  3. "resets in XhYmZs" free text.
//  4. ISO 8601 duration (PT30S, PT1M30S).
//
// A hint of 0 ("retry in 0s") reports present=true with duration 0:
// like an explicit "0" header it is a poisoned hint the router
// floors to 10s — never silently treated as "no hint". A body that
// is not JSON, or that carries no hint shape, reports present=false.
// The body is untrusted upstream text; only the matched duration
// substring is ever parsed, never executed or echoed.
func ParseRetryAfterBody(body string) (time.Duration, bool) {
	if body == "" {
		return 0, false
	}
	// 1. Gemini RetryInfo: prefer the structured field when the
	// body is JSON — but match textually, so a nested RetryInfo
	// inside any envelope shape is found without assuming the
	// envelope layout.
	if m := retryInfoDelayRE.FindStringSubmatch(body); m != nil {
		if d, ok := parseProtobufDuration(m[1]); ok {
			return d, true
		}
	}
	// 2. "retry in Xs" free text.
	if m := retryInRE.FindStringSubmatch(body); m != nil {
		if d, ok := parseNumberUnit(m[1], m[2]); ok {
			return d, true
		}
	}
	// 3. "resets in XhYmZs".
	if m := resetsInRE.FindStringSubmatch(body); m != nil {
		if d, err := time.ParseDuration(normalizeDurationText(m[1])); err == nil && d >= 0 {
			return d, true
		}
	}
	// 4. ISO 8601 duration.
	if m := isoDurationRE.FindStringSubmatch(body); m != nil {
		if d, ok := parseISODuration(m); ok {
			return d, true
		}
	}
	return 0, false
}

// parseProtobufDuration parses a protobuf Duration JSON string:
// "<seconds>s" with an optional fractional part ("30s", "1.5s").
func parseProtobufDuration(s string) (time.Duration, bool) {
	if !strings.HasSuffix(s, "s") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSuffix(s, "s"), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return time.Duration(f * float64(time.Second)), true
}

// parseNumberUnit parses "<number> <unit>" where unit is a seconds /
// minutes / hours / milliseconds word or abbreviation.
func parseNumberUnit(num, unit string) (time.Duration, bool) {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	switch strings.ToLower(unit) {
	case "s", "sec", "secs", "second", "seconds":
		return time.Duration(f * float64(time.Second)), true
	case "m", "min", "mins", "minute", "minutes":
		return time.Duration(f * float64(time.Minute)), true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Duration(f * float64(time.Hour)), true
	case "ms", "millisecond", "milliseconds":
		return time.Duration(f * float64(time.Millisecond)), true
	}
	return 0, false
}

// normalizeDurationText turns a "1h2m3s" / "90s" fragment into a
// time.ParseDuration input: it strips spaces (ParseDuration rejects
// them) and leaves the rest untouched.
func normalizeDurationText(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "")
}

// parseISODuration converts an isoDurationRE match (days, hours,
// minutes, seconds groups) into a duration. At least one group must
// be present — a bare "P" or "PT" is not a duration.
func parseISODuration(m []string) (time.Duration, bool) {
	var d time.Duration
	any := false
	parts := []struct {
		s    string
		mult time.Duration
	}{
		{m[1], 24 * time.Hour},
		{m[2], time.Hour},
		{m[3], time.Minute},
	}
	for _, p := range parts {
		if p.s == "" {
			continue
		}
		n, err := strconv.Atoi(p.s)
		if err != nil || n < 0 {
			return 0, false
		}
		any = true
		d += time.Duration(n) * p.mult
	}
	if m[4] != "" {
		f, err := strconv.ParseFloat(m[4], 64)
		if err != nil || f < 0 {
			return 0, false
		}
		any = true
		d += time.Duration(f * float64(time.Second))
	}
	if !any {
		return 0, false
	}
	return d, true
}

// retryInfoDelayJSON is the structured shape of a RetryInfo entry,
// used only to prove the regex path agrees with a real JSON decode
// in tests (see retryafter_body_test.go). Production parsing is
// textual (ParseRetryAfterBody) so nested envelopes are found.
type retryInfoDelayJSON struct {
	Error struct {
		Details []struct {
			RetryDelay string `json:"retryDelay"`
		} `json:"details"`
	} `json:"error"`
}

// parseRetryInfoDelayJSON decodes a RetryInfo retryDelay from a JSON
// error body the structured way, for tests.
func parseRetryInfoDelayJSON(body string) (time.Duration, bool) {
	var v retryInfoDelayJSON
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return 0, false
	}
	for _, det := range v.Error.Details {
		if det.RetryDelay != "" {
			if d, ok := parseProtobufDuration(det.RetryDelay); ok {
				return d, true
			}
		}
	}
	return 0, false
}
