package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestChatCompletionsStreamingBatched is the end-to-end half of
// items 135-136: seven tokens emitted back-to-back by the engine
// must all reach the client as seven SSE events plus [DONE],
// with hybrid batching on (the default) and with it off. The
// batcher may delay a token by up to 8 ms, but it must never
// drop one or strand one past the end of the handler — the
// deferred Close flushes whatever is still pending, and [DONE]
// flushes immediately.
func TestChatCompletionsStreamingBatched(t *testing.T) {
	deltas := []string{"tok0", "tok1", "tok2", "tok3", "tok4", "tok5", "tok6"}
	body := `{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`
	for _, batching := range []bool{true, false} {
		eng := &fakeEngine{deltas: deltas}
		var opts []Option
		if !batching {
			opts = append(opts, WithSSEBatching(false))
		}
		srv := newTestServer(t, eng, opts...)
		rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body, "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("batching=%v: status = %d, body = %s", batching, rec.Code, rec.Body.String())
		}
		got := rec.Body.String()
		for _, d := range deltas {
			if n := strings.Count(got, `"content":"`+d+`"`); n != 1 {
				t.Fatalf("batching=%v: token %q appears %d times, want 1:\n%s", batching, d, n, got)
			}
		}
		if n := strings.Count(got, "data: "); n != len(deltas)+2 {
			// 7 token events + 1 finish_reason event + [DONE].
			t.Fatalf("batching=%v: %d SSE data lines, want %d:\n%s", batching, n, len(deltas)+2, got)
		}
		if !strings.HasSuffix(strings.TrimSpace(got), "data: [DONE]") {
			t.Fatalf("batching=%v: stream does not end with [DONE]:\n%s", batching, got)
		}
		if !rec.Flushed {
			t.Fatalf("batching=%v: response was never flushed", batching)
		}
	}
}
