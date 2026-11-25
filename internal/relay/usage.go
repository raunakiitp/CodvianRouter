// Package relay — Anthropic usage extraction (AUD-SEC-002).
//
// The verbatim relay path forwards bytes untouched, so it never went
// through the engine that records ledger spend. That made
// /v1/messages traffic invisible to per-key budgets and quota windows:
// a key could spend an unbounded amount through the Anthropic-compatible
// endpoint. These helpers recover the provider-reported usage from the
// bytes the relay already has in hand, without altering them.
package relay

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// AnthropicUsage is the provider-reported usage of one
// Anthropic-compatible response.
//
// Reported is false when the payload carried no usage object at all —
// the caller must then NOT read the zero values as "this call was
// free". Input tokens arrive in the message_start event (or the
// top-level usage of a non-streaming body); output tokens arrive in
// message_delta (streaming) or the same top-level object.
type AnthropicUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
	Reported         bool
}

// anthropicUsageWire mirrors the fields we read. Both the non-streaming
// body and a message_start/message_delta payload nest the same shape.
type anthropicUsageWire struct {
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Message *struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

func (w anthropicUsageWire) apply(u *AnthropicUsage) {
	merge := func(in, out, cw, cr int) {
		if in > u.InputTokens {
			u.InputTokens = in
		}
		if out > u.OutputTokens {
			u.OutputTokens = out
		}
		if cw > u.CacheWriteTokens {
			u.CacheWriteTokens = cw
		}
		if cr > u.CacheReadTokens {
			u.CacheReadTokens = cr
		}
		u.Reported = true
	}
	if w.Usage != nil {
		merge(w.Usage.InputTokens, w.Usage.OutputTokens, w.Usage.CacheCreationInputTokens, w.Usage.CacheReadInputTokens)
	}
	if w.Message != nil && w.Message.Usage != nil {
		merge(w.Message.Usage.InputTokens, w.Message.Usage.OutputTokens, w.Message.Usage.CacheCreationInputTokens, w.Message.Usage.CacheReadInputTokens)
	}
}

// ParseAnthropicUsage extracts usage from a complete non-streaming
// Anthropic response body. A body without a usage object returns
// Reported=false (never a bogus zero-cost success).
func ParseAnthropicUsage(body []byte) AnthropicUsage {
	var u AnthropicUsage
	if len(body) == 0 {
		return u
	}
	var wire anthropicUsageWire
	if err := json.Unmarshal(body, &wire); err != nil {
		return u
	}
	wire.apply(&u)
	return u
}

// UsageSniffer is an io.Writer that passes every byte through to the
// wrapped writer (so the relay stays byte-for-byte verbatim) while
// scanning SSE `data:` payloads for usage as they stream past.
//
// It keeps only the current partial line, so memory stays bounded
// regardless of response size; a single absurdly long line is dropped
// rather than buffered forever.
type UsageSniffer struct {
	w       io.Writer
	pend    []byte
	usage   AnthropicUsage
	dropped bool
	emitted int
}

// maxSniffLine bounds one pending SSE line. Usage events are tens of
// bytes; anything larger is not something this scanner can use.
const maxSniffLine = 1 << 20

// NewUsageSniffer wraps w.
func NewUsageSniffer(w io.Writer) *UsageSniffer { return &UsageSniffer{w: w} }

func (s *UsageSniffer) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if n > 0 {
		s.emitted += n
		s.absorb(p[:n])
	}
	return n, err
}

// EmittedBytes reports how many bytes the sniffer has passed through —
// the honest size of what the client received, used to price a response
// whose provider reported no usage (AUD-SEC-002).
func (s *UsageSniffer) EmittedBytes() int { return s.emitted }

func (s *UsageSniffer) absorb(p []byte) {
	for len(p) > 0 {
		if s.dropped {
			// Resynchronise on the next newline after an
			// oversized line.
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return
			}
			p = p[i+1:]
			s.dropped = false
			s.pend = s.pend[:0]
			continue
		}
		i := bytes.IndexByte(p, '\n')
		var line []byte
		if i < 0 {
			s.pend = append(s.pend, p...)
			if len(s.pend) > maxSniffLine {
				s.dropped = true
				s.pend = s.pend[:0]
			}
			return
		}
		line = append(s.pend, p[:i]...)
		p = p[i+1:]
		s.pend = s.pend[:0]
		s.scanLine(line)
	}
}

func (s *UsageSniffer) scanLine(line []byte) {
	t := bytes.TrimSpace(line)
	if !bytes.HasPrefix(t, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(t[len("data:"):])
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return // cheap reject: most events carry no usage
	}
	var wire anthropicUsageWire
	if err := json.Unmarshal(payload, &wire); err != nil {
		return
	}
	wire.apply(&s.usage)
}

// Usage returns what the stream reported so far. Reported=false when no
// event carried a usage object (the caller must not treat that as zero
// spend).
func (s *UsageSniffer) Usage() AnthropicUsage { return s.usage }

// IsSSEDataLine reports whether a raw line would be scanned (used by
// tests to document the contract).
func IsSSEDataLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "data:")
}
