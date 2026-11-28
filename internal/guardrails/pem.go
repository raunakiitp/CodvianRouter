package guardrails

import "strings"

// Whole-block PEM private-key redaction (M1.7).
//
// The old rule matched only the BEGIN header line
// (`-----BEGIN (?:[A-Z ]+)?PRIVATE KEY-----`); the base64 body and
// the END footer passed through verbatim, and the generic-token
// heuristic only masked mixed-case runs — pure-lowercase and
// pure-uppercase 64-char body lines leaked untouched. A PEM block
// is redacted here as one span: header, body and footer, replaced
// by a single [REDACTED:SECRET].
//
// Why a hand-written scanner instead of a regex: the sector-typical
// whole-block pattern uses a negative lookahead
// (`(?:[^-]|-(?!----))*`), which Go's RE2 engine does not support.
// The scanner below is linear (one pass, no backtracking) and
// case-insensitive on the markers, so a lowercased
// `-----begin private key-----` is caught too.
//
// Scope: PRIVATE KEY blocks only (plain, RSA/EC/DSA/OPENSSH/PGP,
// ENCRYPTED). A CERTIFICATE block is public by design and is left
// alone. An unclosed block (BEGIN with no END footer) is redacted
// from the header to end of text: a truncated key is still a key.

const pemPlaceholder = "[REDACTED:SECRET]"

// pemMarkerStatus is what parsePEMMarker found at a candidate.
const (
	pemNotMarker  = iota // "-----begin" not followed by a space: ordinary text
	pemIncomplete        // marker starts here but its closing ----- has not arrived yet
	pemComplete          // full marker, label + end index known
)

// asciiLower lowercases one ASCII byte; non-ASCII bytes pass
// through unchanged (PEM markers are ASCII).
func asciiLower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// indexASCIIFold finds needle (lowercase ASCII) in s,
// case-insensitively on ASCII letters, starting at from. It works
// on the original text so byte offsets stay valid for slicing —
// foldLower can change byte length on non-ASCII input, so indices
// from the folded copy must never be used to slice the original.
func indexASCIIFold(s, needle string, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i+len(needle) <= len(s); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if asciiLower(s[i+j]) != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// parsePEMMarker parses the marker starting at s[i], where s[i:]
// begins with "-----begin" (kwLen 5) or "-----end" (kwLen 3),
// case-insensitive. It returns the folded label, the index just
// past the closing -----, and the status. A marker is "-----" +
// BEGIN/END + spaces + label + "-----".
func parsePEMMarker(s string, i int, kwLen int) (label string, end int, status int) {
	j := i + 5 + kwLen // dashes + keyword
	if j >= len(s) {
		return "", 0, pemIncomplete
	}
	if s[j] != ' ' {
		return "", 0, pemNotMarker
	}
	k := j
	for k < len(s) && s[k] == ' ' {
		k++
	}
	c := strings.Index(s[k:], "-----")
	if c < 0 {
		return "", 0, pemIncomplete
	}
	label = strings.TrimSpace(foldLower(s[k : k+c]))
	return label, k + c + 5, pemComplete
}

// isPrivateKeyLabel reports whether a folded PEM label names a
// private key: the label itself or anything ending in it
// ("rsa private key", "encrypted private key", ...).
func isPrivateKeyLabel(label string) bool {
	return strings.HasSuffix(label, "private key")
}

// findPEMHeader finds the next complete-or-incomplete PEM BEGIN
// marker at or after from. It returns the marker start, its
// folded label, the index past the marker, and the status
// (pemNotMarker is never returned: non-markers are skipped).
// When status is pemIncomplete the header itself is split — the
// caller must withhold from idx, never emit past it.
func findPEMHeader(s string, from int) (idx int, label string, end int, status int) {
	for {
		i := indexASCIIFold(s, "-----begin", from)
		if i < 0 {
			return 0, "", 0, pemNotMarker
		}
		label, end, status = parsePEMMarker(s, i, 5)
		switch status {
		case pemComplete:
			return i, label, end, pemComplete
		case pemIncomplete:
			return i, "", 0, pemIncomplete
		default: // pemNotMarker ("-----beginner", ...): keep looking
			from = i + 1
		}
	}
}

// findPEMFooter finds the next END footer for a private key at or
// after from: the first complete END marker whose label ends in
// "private key". Other END markers (e.g. END CERTIFICATE) are
// skipped. It returns the footer start, its label, the index past
// the footer, and the status: pemComplete when a qualifying
// footer was found, pemIncomplete when an END marker is split at
// the end of the text, pemNotMarker when there is no END marker
// at all.
func findPEMFooter(s string, from int) (idx int, label string, end int, status int) {
	for {
		i := indexASCIIFold(s, "-----end", from)
		if i < 0 {
			return 0, "", 0, pemNotMarker
		}
		label, end, status = parsePEMMarker(s, i, 3)
		switch status {
		case pemComplete:
			if isPrivateKeyLabel(label) {
				return i, label, end, pemComplete
			}
			from = end // some other block's footer: keep looking
		case pemIncomplete:
			return i, "", 0, pemIncomplete
		default:
			from = i + 1
		}
	}
}

// redactPEMBlocks replaces every whole private-key PEM block in
// text with one [REDACTED:SECRET] and returns the result plus the
// number of blocks redacted. An unclosed block is redacted from
// its header to end of text. Text with no BEGIN marker is
// returned unchanged (zero allocations beyond the gate scan).
func redactPEMBlocks(text string) (string, int) {
	if indexASCIIFold(text, "-----begin", 0) < 0 {
		return text, 0
	}
	var b strings.Builder
	b.Grow(len(text))
	count := 0
	rest := text
	for {
		idx, label, hdrEnd, status := findPEMHeader(rest, 0)
		if status == pemNotMarker {
			b.WriteString(rest)
			break
		}
		if status == pemIncomplete {
			// Header cut off at end of text: nothing after idx
			// can be proven safe.
			b.WriteString(rest[:idx])
			b.WriteString(pemPlaceholder)
			count++
			break
		}
		if !isPrivateKeyLabel(label) {
			// Some other PEM block (e.g. CERTIFICATE): emit it
			// untouched and keep scanning after its header.
			b.WriteString(rest[:hdrEnd])
			rest = rest[hdrEnd:]
			continue
		}
		_, _, fend, fstatus := findPEMFooter(rest, hdrEnd)
		if fstatus != pemComplete {
			// Unclosed block: redact header to end of text.
			b.WriteString(rest[:idx])
			b.WriteString(pemPlaceholder)
			count++
			break
		}
		b.WriteString(rest[:idx])
		b.WriteString(pemPlaceholder)
		count++
		rest = rest[fend:]
	}
	return b.String(), count
}
