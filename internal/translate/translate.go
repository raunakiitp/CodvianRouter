// Package translate implements the wire-format translator pairs the
// gateway needs beyond the four native wires in internal/wire. Each
// pair converts one direction of a provider surface to or from the
// gateway's OpenAI chat-completions shape (or, for claude<->gemini,
// between the Anthropic and Gemini wire shapes), with round-trip
// tests per pair covering request, response, and streaming
// translation.
//
// Request/response/stream shapes are built from the provider's own
// documentation or (where a surface is not publicly documented) from
// documented protocol facts; every divergence from an ideal full
// mapping is recorded in a comment on the pair it applies to — never
// invented silently.
package translate

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/wire"
)

// UnrepresentableError marks a translation the target wire cannot
// represent faithfully. Callers treat it as a failover signal, the
// same way wire.UnrepresentableError is treated for the native
// wires: the request must not be sent silently degraded.
type UnrepresentableError struct {
	Msg string
}

// Error implements error.
func (e *UnrepresentableError) Error() string {
	return "translate: unrepresentable: " + e.Msg
}

// unrep builds an *UnrepresentableError.
func unrep(format string, args ...any) error {
	return &UnrepresentableError{Msg: fmt.Sprintf(format, args...)}
}

// IsUnrepresentable reports whether err is (or wraps) an
// *UnrepresentableError from this package or the native-wire
// *wire.UnrepresentableError (the inner Gemini/Anthropic conversions
// used by pairs like antigravity return the wire type).
func IsUnrepresentable(err error) bool {
	var ue *UnrepresentableError
	if asUnrepresentable(err, &ue) {
		return true
	}
	return wireIsUnrepresentable(err)
}

// wireIsUnrepresentable errors.As-checks the native wire's
// UnrepresentableError.
func wireIsUnrepresentable(err error) bool {
	var we *wire.UnrepresentableError
	return errors.As(err, &we)
}

// asUnrepresentable is errors.As, kept local so the package needs no
// extra import surface beyond the standard library.
func asUnrepresentable(err error, target **UnrepresentableError) bool {
	for err != nil {
		if ue, ok := err.(*UnrepresentableError); ok {
			*target = ue
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// mustJSON encodes s as a JSON string raw message (the OpenAI message
// content field is json.RawMessage in the wire shape).
func mustJSON(s string) json.RawMessage {
	b, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail.
		return json.RawMessage(`""`)
	}
	return b
}

// ---------------------------------------------------------------------------
// SSE parsing (shared by the streaming half of every pair).
// ---------------------------------------------------------------------------

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data string
}

// readSSE parses SSE frames from r, calling fn for each complete event
// (blank line ends an event). Comment lines (":...") are ignored, as
// are lines that are neither "event:" nor "data:". Multiple "data:"
// lines in one event are joined with "\n" per the SSE spec.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	// Deltas can be large; allow up to 1 MiB per SSE line, matching
	// the streaming scanner used across the wire package.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var ev sseEvent
	flush := func() error {
		if ev.name == "" && ev.data == "" {
			return nil
		}
		e := ev
		ev = sseEvent{}
		return fn(e)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // SSE comment / keep-alive
		}
		if name, ok := strings.CutPrefix(line, "event:"); ok {
			ev.name = strings.TrimSpace(name)
			continue
		}
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			data = strings.TrimPrefix(data, " ")
			if ev.data != "" {
				ev.data += "\n"
			}
			ev.data += data
			continue
		}
		// Unknown line: ignored — a stray line must not kill a stream.
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("translate: error reading SSE stream: %w", err)
	}
	return flush()
}
