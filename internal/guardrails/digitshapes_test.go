package guardrails

// Digit-shape gate tests (CHECKLIST 149-150, benchmark slice).
//
// The gate must be SOUND: every text the regexes match must pass
// it, so a skipped regex can never hide a real phone / card /
// Aadhaar number. The first table is every match shape the three
// patterns can produce (including the merged-run forms where the
// last two digit groups touch); the second table is texts that
// must NOT match — above all the router's own access-log lines,
// which carry digits from the timestamp, "/v1/", status and
// latency but no PII shape. If a future pattern change adds a
// match shape, its vector belongs in the first table first.
import (
	"testing"
)

func TestDigitShapesGateKeepsAllMatches(t *testing.T) {
	mustMatch := []struct {
		text    string
		pattern string
	}{
		// Phone: +91 / +1 forms.
		{"call +91 98765 43210", "phone"},
		{"call +919876543210", "phone"},
		{"call +91-98765-43210", "phone"},
		{"call +1 415 555 0132", "phone"},
		{"call +1 (415) 555-0132", "phone"},
		{"call +14155550132", "phone"},
		// Phone: parenthesised area code, last two groups touching.
		{"call (415) 5550132", "phone"},
		{"call (415) 555 0132", "phone"},
		{"call (415) 555-0132", "phone"},
		// Phone: plain 3-3-4, spaced or dashed (the second separator
		// is mandatory, so "415 5550132" with the last two groups
		// touching does NOT match — it is in the non-match table).
		{"call 415 555 0132", "phone"},
		{"call 415-555-0132", "phone"},
		{"call 415.555.0132", "phone"},
		// Phone: bare 10-digit run.
		{"call 9876543210", "phone"},
		{"call 6123456789", "phone"},
		// Card: spaced, dashed, and unseparated 16 digits.
		{"card 4111 1111 1111 1111 charged", "card"},
		{"card 4111-1111-1111-1111 charged", "card"},
		{"card 4111111111111111 charged", "card"},
		{"card 41111111 11111111 charged", "card"},
		{"card 4111 11111111 1111 charged", "card"},
		// Aadhaar: three 4-digit groups, single spaces.
		{"aadhaar 2345 6789 0123", "aadhaar"},
		{"aadhaar 1234 5678 9012", "aadhaar"},
	}
	for _, tc := range mustMatch {
		found := false
		for _, f := range Findings(tc.text) {
			if f.Pattern == tc.pattern {
				found = true
			}
		}
		if !found {
			t.Errorf("Findings(%q) missed %s (gate skipped a real match)", tc.text, tc.pattern)
		}
		// The gate itself must pass for these texts: a direct
		// shape check catches a gate bug even if some other
		// rule happened to match first.
		shapes := scanDigitShapes(tc.text)
		switch tc.pattern {
		case "phone":
			if !shapes.phone {
				t.Errorf("scanDigitShapes(%q).phone = false, want true", tc.text)
			}
		case "card":
			if !shapes.card {
				t.Errorf("scanDigitShapes(%q).card = false, want true", tc.text)
			}
		case "aadhaar":
			if !shapes.aadhaar {
				t.Errorf("scanDigitShapes(%q).aadhaar = false, want true", tc.text)
			}
		}
	}
}

func TestDigitShapesGateSkipsNonMatches(t *testing.T) {
	mustNotMatch := []string{
		// The router's own access-log lines: timestamp + /v1/ +
		// status + latency carry digits but no PII shape.
		"2026/11/17 20:37:59 method=POST path=/v1/chat/completions model=model-a status=200 latency_ms=5",
		"method=POST path=/v1/chat/completions model=model-a status=200 latency_ms=1234",
		"2026/11/18 02:05:47 method=GET path=/v1/models status=200 latency_ms=12",
		// Plain numbers that are not a phone / card / Aadhaar.
		"call 1234 in 2026",
		"version 2.0.1 released 2026",
		"room 123, floor 4, 2026",
		"the year 2026 and 12345 things",
		// +91 without its digit groups is not a phone number.
		"dial +91 for India",
		"91 12345 67890 is not a call",
		// Runs too short / gaps too wide.
		"123 4567 8901",
		"1234 567 890",
		// The plain 3-3-4 form needs its second separator: with
		// the last two groups touching there is no match (the
		// gate still runs the regex — the run shape allows it —
		// but the regex correctly finds nothing).
		"415 5550132",
	}
	for _, text := range mustNotMatch {
		for _, f := range Findings(text) {
			if f.Pattern == "phone" || f.Pattern == "card" || f.Pattern == "aadhaar" {
				t.Errorf("Findings(%q) wrongly matched %s", text, f.Pattern)
			}
		}
		if got := RedactLogLine(text); got != text {
			t.Errorf("RedactLogLine(%q) = %q, want unchanged", text, got)
		}
	}
	// The gate itself must refuse the log lines, so the regexes
	// are never even attempted on them.
	for _, text := range mustNotMatch[:3] {
		shapes := scanDigitShapes(text)
		if shapes.phone || shapes.card || shapes.aadhaar {
			t.Errorf("scanDigitShapes(%q) = %+v, want all shapes false", text, shapes)
		}
	}
}

// TestDigitShapesGateNeverHidesAMatch is the soundness property:
// for a corpus of digit-heavy strings, whenever any digit regex
// matches, the gate for that rule must have passed. It runs the
// regexes directly (bypassing the gate) and compares against the
// gate, so a gate bug fails here even if Findings happens to agree
// through another rule.
func TestDigitShapesGateNeverHidesAMatch(t *testing.T) {
	corpus := []string{
		"call +91 98765 43210", "call +1 (415) 555-0132", "call (415) 5550132",
		"call 415 555 0132", "call 9876543210", "card 4111 1111 1111 1111",
		"card 4111111111111111", "aadhaar 1234 5678 9012",
		"2026/11/17 20:37:59 method=POST path=/v1/chat/completions status=200 latency_ms=5",
		"123 4567 8901", "1234 5678 9012 3456 7890", "12 34 56 78 90",
		"tel +91-98765-43210 ext 5", "4111-1111-1111-1111", "0000 0000 0000 0000",
		"my number is 4155550132 call me", "(800) 555 0199", "800-555-0199",
		"version 1.2.3.4.5.6.7.8.9.10", "2026-11-18T02:05:47Z",
	}
	shapeOf := map[string]int{"phone": digitShapePhone, "card": digitShapeCard, "aadhaar": digitShapeAadhaar}
	for _, text := range corpus {
		shapes := scanDigitShapes(text)
		for _, r := range redactRules {
			shape, ok := shapeOf[r.pattern]
			if !ok {
				continue
			}
			direct := r.re.FindAllStringIndex(text, -1)
			if len(direct) == 0 {
				continue
			}
			var gate bool
			switch shape {
			case digitShapePhone:
				gate = shapes.phone
			case digitShapeCard:
				gate = shapes.card
			case digitShapeAadhaar:
				gate = shapes.aadhaar
			}
			if !gate {
				t.Errorf("gate skipped %s on %q (regex matches %d times)", r.pattern, text, len(direct))
			}
			// NOTE: no Findings-agreement check here on purpose.
			// Findings runs the rules in table order on the
			// progressively redacted text, so an earlier rule may
			// consume a later rule's substring: on "4111 1111
			// 1111 1111" the card rule matches first and the
			// aadhaar regex then finds nothing left to match —
			// that is the designed first-match-wins order, not
			// a gate failure. The gate check above is the
			// soundness property; TestRedactCardAndAadhaar pins
			// the overlap behaviour.
		}
	}
}

// BenchmarkRedactLogLine pins the hot-path cost of the per-request
// log redaction (CHECKLIST 149): the access-log line carries digits
// but no PII shape, so the digit regexes must be skipped by the
// gate and the call must stay in the low single-digit microseconds.
func BenchmarkRedactLogLine(b *testing.B) {
	line := "2026/11/17 20:37:59 method=POST path=/v1/chat/completions model=model-a status=200 latency_ms=5\n"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RedactLogLine(line)
	}
}
