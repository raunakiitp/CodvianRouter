// Log redaction, default-on (research-log entries 150/152).
//
// Portkey 1.15.2 leaked prompts AND provider keys to an open log
// stream unless NODE_ENV=production; LiteLLM's spend-log token
// redaction is OFF by default. Codvian Router must never have this
// class of bug, so redaction here does not depend on any call site
// remembering to redact, any config flag, or any environment
// variable:
//
//   - Every line the server logger writes passes through a
//     redactWriter (see New), which runs guardrails.RedactLogLine
//     over each complete line before it reaches the underlying
//     writer. A future log call that forgets RedactLogLine can
//     still never emit a key-shaped value (or PII) to the log
//     stream. There is no off switch: wrapRedactingLogger has no
//     "disable" path and no environment variable consults it.
//   - logSafeError is for errors that come from an upstream: an
//     upstream error body is capped at 200 chars by the provider
//     client, but it can still echo prompt/response text verbatim
//     (some upstreams echo the request in the error), and no
//     pattern redactor can strip arbitrary prompt text. So for
//     upstream-shaped errors the log line carries the status code
//     only — never the body. Other errors are redacted for
//     key/PII shapes before logging.
//
// Prompt/response bodies are never logged anywhere in the server:
// the access log records method/path/model/status/latency only, the
// ledger stores token counts and IDs only, and the trace stores
// attempt outcomes only. This file is the backstop that makes that
// true even if a call site regresses.
package server

import (
	"bytes"
	"errors"
	"io"
	"log"
	"strconv"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// redactWriter wraps an io.Writer and redacts every complete line
// before it is written through. Partial lines are buffered until
// their newline arrives, so a secret split across two Write calls
// is still caught. It is safe for concurrent use (log.Logger
// serialises its own writes, but a wrapped logger can be shared).
type redactWriter struct {
	mu  sync.Mutex
	out io.Writer
	buf []byte
}

func newRedactWriter(out io.Writer) *redactWriter {
	if out == nil {
		out = io.Discard
	}
	return &redactWriter{out: out}
}

// Write redacts each newline-terminated line in p and forwards it.
// A trailing partial line is held until the next Write completes
// it; it is never written unredacted.
func (w *redactWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := w.buf[:i+1]
		w.buf = append([]byte(nil), w.buf[i+1:]...)
		redacted := guardrails.RedactLogLine(string(line))
		if _, err := io.WriteString(w.out, redacted); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// wrapRedactingLogger returns l with its output routed through a
// redactWriter, so every line it emits is redacted first. A logger
// that is already wrapped is returned unchanged (double-wrapping
// is harmless but pointless). A nil logger is returned as-is.
func wrapRedactingLogger(l *log.Logger) *log.Logger {
	if l == nil {
		return nil
	}
	if _, ok := l.Writer().(*redactWriter); ok {
		return l
	}
	l.SetOutput(newRedactWriter(l.Writer()))
	return l
}

// logSafeError renders err for a log line. Upstream-shaped errors
// carry an upstream body that may echo prompt/response text, which
// no pattern redactor can strip, so only the status code (and the
// error kind) is logged — never the body. Any other error is
// redacted for key/PII shapes. The returned string is safe to log
// even on a logger that was never wrapped.
func logSafeError(err error) string {
	if err == nil {
		return ""
	}
	var upErr *provider.UpstreamError
	if errors.As(err, &upErr) {
		return "upstream error: HTTP " + strconv.Itoa(upErr.StatusCode)
	}
	var qErr *provider.QuotaError
	if errors.As(err, &qErr) {
		if qErr.StatusCode != 0 {
			return "upstream quota error: HTTP " + strconv.Itoa(qErr.StatusCode)
		}
		return "upstream quota error"
	}
	var rlErr *provider.RateLimitError
	if errors.As(err, &rlErr) {
		return "upstream rate limited"
	}
	return guardrails.RedactLogLine(err.Error())
}
