// G18 Area 7 — response sanitization, streaming (SSE): the
// engine half of the streaming-sanitization row (the sector baseline:
// opt-in SSE transforms, PII + credential stream transforms).
//
// Buffering rule (exact): the redactor holds at most one
// incomplete line plus, for an over-long line, a 1 KB overlap
// window. Concretely —
//   - A chunk is appended to the buffer; every COMPLETE line
//     (ending in "\n") is redacted whole — a line is complete,
//     so no pattern can straddle the emit boundary except a
//     multi-line PEM block, which the composed PEM
//     StreamRedactor withholds before this redactor ever sees
//     it — and emitted. A clean line passes through
//     byte-for-byte unchanged.
//   - The trailing INCOMPLETE line (no newline yet) is held:
//     a secret split across two chunks lives in that held
//     line until the chunk that completes it arrives, so the
//     split secret is still redacted.
//   - If the held line grows past StreamRedactorMaxLine
//     (16 KB, e.g. one giant JSON line), all but the last
//     StreamRedactorOverlap (1 KB) bytes are flushed as a
//     best-effort redact: every bounded pattern (the longest
//     is ~71 chars) straddling that flush point is inside the
//     overlap and still caught. The documented residual risk
//     is a single match longer than ~1 KB (a very long JWT or
//     generic-token run) split exactly at that flush point —
//     its head could be emitted before the rest arrives.
//     Ordinary SSE traffic is line-delimited and never hits
//     this path.
//   - Flush() redacts and emits whatever is held; it must be
//     called exactly once at stream end.
//
// Modes: redact and warn behave per line as in RedactResponse
// (warn delivers the line unchanged and only counts). Block
// mode for a stream cannot recall what was already sent, so
// it CUTS: the first line with a finding is withheld and
// nothing further is emitted (Blocked reports the cut).
// PEM handling is composed, not duplicated: each chunk runs
// through the existing PEM StreamRedactor first, so an
// unclosed PEM block is withheld in full exactly as before,
// and this redactor only ever sees PEM-free text.
//
// A ResponseStreamRedactor is not safe for concurrent use;
// one instance serves one stream. Findings are aggregated by
// (kind, pattern) and available through Findings(); when the
// redactor is built by (Store).NewResponseStreamRedactor, each
// line's findings are also recorded in the store's per-kind
// and per-pattern counters (and the event feed) under phase
// "response". Stream chunks are not Check calls, so they do
// not increment the checked/evaluation totals.
package guardrails

import (
	"strings"
	"time"
)

// StreamRedactorMaxLine caps the held incomplete line: past
// 16 KB the redactor flushes all but the overlap window.
const StreamRedactorMaxLine = 16 * 1024

// StreamRedactorOverlap is the tail held back when an
// over-long line is flushed: 1 KB.
const StreamRedactorOverlap = 1024

// ResponseStreamRedactor redacts PII/secrets from a streaming
// response chunk by chunk, per the buffering rule above.
type ResponseStreamRedactor struct {
	mode     ResponseGuardMode
	pem      *StreamRedactor
	buf      string
	blocked  bool
	counts   map[string]*Finding
	order    []string
	disabled map[string]bool
	custom   func(string, []Finding) (string, []Finding)
	onFind   func([]Finding, string)
}

// NewResponseStreamRedactor returns a redactor for one stream
// under the given mode (invalid mode behaves as redact). It
// has no store: findings are only available through
// Findings(), never counted anywhere.
func NewResponseStreamRedactor(mode ResponseGuardMode) *ResponseStreamRedactor {
	if !mode.Valid() {
		mode = ResponseModeRedact
	}
	return &ResponseStreamRedactor{mode: mode, pem: NewStreamRedactor(), counts: map[string]*Finding{}}
}

// NewResponseStreamRedactor returns a store-wired redactor for
// one stream: each line's findings are recorded in the store's
// per-kind/per-pattern counters and event feed (phase
// "response"), per-type toggles are honoured for findings and
// for the block-mode cut, and custom patterns run alongside
// the built-ins. The mode defaults to the store's mode when
// mode is empty.
func (s *Store) NewResponseStreamRedactor(mode ResponseGuardMode) *ResponseStreamRedactor {
	if mode == "" {
		mode = s.ResponseMode()
	}
	if !mode.Valid() {
		mode = ResponseModeRedact
	}
	s.mu.Lock()
	disabled := make(map[string]bool, len(s.disabledTypes))
	for k, v := range s.disabledTypes {
		disabled[k] = v
	}
	s.mu.Unlock()
	r := NewResponseStreamRedactor(mode)
	r.disabled = disabled
	r.custom = s.applyCustomPatterns
	r.onFind = func(findings []Finding, preview string) {
		if len(findings) == 0 {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		action := ActionRedacted
		if mode == ResponseModeWarn {
			action = ActionWarned
		} else if mode == ResponseModeBlock {
			action = ActionBlocked
		}
		s.recordStreamFindingsLocked(findings, action, preview)
	}
	return r
}

// recordStreamFindingsLocked counts one stream line's
// findings in the per-kind and per-pattern totals and the
// event feed (phase "response"). It deliberately does NOT
// touch the checked/redacted/evaluation totals: stream lines
// are not Check calls, and a 200-line stream must not look
// like 200 evaluations. A block-mode cut increments the
// blocked total once — the cut fires on exactly one line,
// the first with a finding, because no later line is ever
// processed. The caller holds s.mu.
func (s *Store) recordStreamFindingsLocked(findings []Finding, action string, preview string) {
	if s.byKind == nil {
		s.byKind = map[string]uint64{}
	}
	if s.byPattern == nil {
		s.byPattern = map[string]uint64{}
	}
	for _, f := range findings {
		s.byKind[f.Kind] += uint64(f.Count)
		s.byPattern[f.Pattern] += uint64(f.Count)
	}
	if action == ActionBlocked {
		s.blocked++
	}
	for _, f := range findings {
		s.events = append(s.events, Event{
			At:      time.Now().UTC(),
			Kind:    f.Kind,
			Pattern: f.Pattern,
			Source:  PhaseResponse,
			Blocked: action == ActionBlocked,
			Preview: preview,
		})
	}
	if len(s.events) > maxEvents {
		s.events = append([]Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
}

// WriteChunk feeds one stream delta and returns the text that
// is safe to emit now. In block mode after a cut it returns
// "" for every further chunk.
func (r *ResponseStreamRedactor) WriteChunk(chunk string) string {
	if r.blocked {
		return ""
	}
	if chunk == "" {
		return ""
	}
	// PEM first: a PEM block in progress is withheld here,
	// so the line logic below never sees a half-block.
	text := r.pem.WriteChunk(chunk)
	r.buf += text
	var out strings.Builder
	for {
		idx := strings.IndexByte(r.buf, '\n')
		if idx < 0 {
			break
		}
		line := r.buf[:idx+1]
		r.buf = r.buf[idx+1:]
		out.WriteString(r.processLine(line))
		if r.blocked {
			// Cut mid-chunk: the rest of this chunk's
			// complete lines are dropped with it.
			r.buf = ""
			return out.String()
		}
	}
	if len(r.buf) > StreamRedactorMaxLine {
		head := r.buf[:len(r.buf)-StreamRedactorOverlap]
		r.buf = r.buf[len(r.buf)-StreamRedactorOverlap:]
		out.WriteString(r.processLine(head))
	}
	return out.String()
}

// Flush ends the stream: any held tail is redacted (or
// withheld in block mode after a cut) and returned, and the
// PEM redactor's own withheld tail is flushed through the
// same path. Call exactly once, after the last WriteChunk.
func (r *ResponseStreamRedactor) Flush() string {
	if r.blocked {
		r.buf = ""
		_ = r.pem.Flush()
		return ""
	}
	tail := r.pem.Flush()
	r.buf += tail
	if r.buf == "" {
		return ""
	}
	rest := r.processLine(r.buf)
	r.buf = ""
	return rest
}

// Blocked reports whether block mode has cut this stream.
func (r *ResponseStreamRedactor) Blocked() bool {
	return r.blocked
}

// Findings returns the cumulative findings for this stream,
// aggregated by (kind, pattern), in first-seen order.
func (r *ResponseStreamRedactor) Findings() []Finding {
	out := make([]Finding, 0, len(r.order))
	for _, key := range r.order {
		out = append(out, *r.counts[key])
	}
	return out
}

// processLine redacts one complete line (or the held tail at
// Flush) and returns what may be emitted for it. Findings are
// aggregated and, when the redactor is store-wired, recorded.
func (r *ResponseStreamRedactor) processLine(line string) string {
	redacted, findings := scan(line)
	if r.custom != nil {
		redacted, findings = r.custom(redacted, findings)
	}
	if len(r.disabled) > 0 && len(findings) > 0 {
		kept := findings[:0]
		for _, f := range findings {
			if !r.disabled[f.Pattern] {
				kept = append(kept, f)
			}
		}
		findings = kept
	}
	if len(findings) == 0 {
		if r.mode == ResponseModeBlock {
			return line
		}
		return line
	}
	for _, f := range findings {
		key := f.Kind + "\x00" + f.Pattern
		if existing, ok := r.counts[key]; ok {
			existing.Count += f.Count
		} else {
			cp := f
			r.counts[key] = &cp
			r.order = append(r.order, key)
		}
	}
	preview := truncateRunes(redacted, maxPreviewRunes)
	if r.onFind != nil {
		r.onFind(findings, preview)
	}
	switch r.mode {
	case ResponseModeWarn:
		return line
	case ResponseModeBlock:
		r.blocked = true
		return ""
	default:
		return redacted
	}
}
