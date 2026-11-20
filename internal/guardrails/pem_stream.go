package guardrails

import "strings"

// StreamRedactor redacts PEM private-key blocks from a streaming
// response, chunk by chunk (M1.7).
//
// A streaming reply cannot be recalled once sent, so the redactor
// withholds rather than repairs: from the moment a private-key
// BEGIN header is seen (even a header split across chunks) nothing
// of the block is emitted until its END footer arrives, at which
// point a single [REDACTED:SECRET] is emitted in its place. An
// unclosed block is withheld until Flush, which redacts it — the
// OmniRoute "CredentialOverflow withholds unclosed PEM" behaviour.
// Clean text passes through unchanged at any chunking, except a
// trailing partial "-----begin" prefix, which is held for the next
// chunk (or emitted verbatim by Flush: a fragment of a header with
// no body behind it carries no secret).
//
// The redactor is PEM-only by design: other secret shapes split
// across chunk boundaries cannot be recognised from one chunk and
// must not be given a false sense of coverage. The server wires
// it into the stream delta path (see the M1.7 report for the call
// sites); the non-streaming whole-block redaction lives in
// redactPEMBlocks and runs inside scan().
//
// A StreamRedactor is not safe for concurrent use; one instance
// serves one stream.
type StreamRedactor struct {
	buf    string // un-emitted tail: withheld PEM block or partial header
	blocks int    // whole blocks replaced so far (incl. Flush)
}

// NewStreamRedactor returns a StreamRedactor for one stream.
func NewStreamRedactor() *StreamRedactor {
	return &StreamRedactor{}
}

// WriteChunk feeds one stream delta and returns the text that is
// safe to emit now. Text before a PEM block, and text after a
// completed block, is emitted immediately; a block in progress is
// withheld in full.
func (r *StreamRedactor) WriteChunk(chunk string) string {
	if chunk == "" {
		return ""
	}
	r.buf += chunk
	var out strings.Builder
	for {
		idx, label, hdrEnd, status := findPEMHeader(r.buf, 0)
		if status == pemNotMarker {
			// No header: emit everything except a trailing
			// partial "-----begin" prefix, which the next
			// chunk may complete.
			hold := partialHeaderSuffixLen(r.buf)
			out.WriteString(r.buf[:len(r.buf)-hold])
			r.buf = r.buf[len(r.buf)-hold:]
			break
		}
		if status == pemIncomplete {
			// Header split across chunks: withhold from its start.
			out.WriteString(r.buf[:idx])
			r.buf = r.buf[idx:]
			break
		}
		if !isPrivateKeyLabel(label) {
			// Some other PEM block: emit through its header and
			// keep scanning the remainder.
			out.WriteString(r.buf[:hdrEnd])
			r.buf = r.buf[hdrEnd:]
			continue
		}
		_, _, fend, fstatus := findPEMFooter(r.buf, hdrEnd)
		if fstatus != pemComplete {
			// Block still open: withhold it in full.
			out.WriteString(r.buf[:idx])
			r.buf = r.buf[idx:]
			break
		}
		out.WriteString(r.buf[:idx])
		out.WriteString(pemPlaceholder)
		r.blocks++
		r.buf = r.buf[fend:]
	}
	return out.String()
}

// Flush ends the stream: any withheld tail is redacted (an
// unclosed PEM block becomes one placeholder) and returned. The
// tail also passes through the full Redact scan, so other secret
// shapes that arrived whole in the tail are redacted too. Call
// exactly once, after the last WriteChunk.
func (r *StreamRedactor) Flush() string {
	if r.buf == "" {
		return ""
	}
	rest, n := redactPEMBlocks(r.buf)
	r.blocks += n
	r.buf = ""
	return Redact(rest)
}

// RedactedBlocks reports how many whole PEM blocks this redactor
// has replaced, including any unclosed block redacted by Flush.
func (r *StreamRedactor) RedactedBlocks() int {
	return r.blocks
}

// partialHeaderSuffixLen returns the length of the longest suffix
// of s that is a case-insensitive prefix of "-----begin" — the
// part of a split header that must be held for the next chunk.
func partialHeaderSuffixLen(s string) int {
	const prefix = "-----begin"
	max := len(prefix)
	if len(s) < max {
		max = len(s)
	}
	for k := max; k > 0; k-- {
		suffix := s[len(s)-k:]
		ok := true
		for i := 0; i < k; i++ {
			if asciiLower(suffix[i]) != prefix[i] {
				ok = false
				break
			}
		}
		if ok {
			return k
		}
	}
	return 0
}
