// AUD-PERF-001 regression tests.
//
// The chat stream used to accumulate the FULL text it had sent in a
// strings.Builder that was only ever read for its length, so every
// in-flight stream held its entire completion in memory to compute a
// boolean. It is now a byte counter, which carries the same
// "has anything been delivered yet" information at O(1) memory.
//
// These tests pin the SEMANTICS the counter must preserve — the
// before/after split around the first delivered byte — and the
// benchmark reports what the handler retains per streamed byte.
package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

const p13StreamReq = `{"model":"test-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`

// TestAUD_PERF_001ErrorBeforeAnyContentIsStillAStatusError: with nothing
// delivered the handler must still be able to answer with an error
// status (nothing has been committed to the wire).
func TestAUD_PERF_001ErrorBeforeAnyContentIsStillAStatusError(t *testing.T) {
	eng := &fakeEngine{deltas: []string{"never-sent"}, streamErr: errors.New("upstream died"), failAfter: 0}
	srv := newTestServer(t, eng)

	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", p13StreamReq, "test-key-123")
	if rec.Code < 400 {
		t.Fatalf("status = %d, want an error status (nothing was delivered yet): %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "never-sent") {
		t.Fatalf("a delta was delivered although the stream failed before it: %s", rec.Body.String())
	}
}

// TestAUD_PERF_001ErrorAfterContentKeepsTheStream: once content is out,
// the client keeps the delivered bytes and gets a terminal frame — the
// counter must report "content sent" exactly like the builder did.
func TestAUD_PERF_001ErrorAfterContentKeepsTheStream(t *testing.T) {
	eng := &fakeEngine{deltas: []string{"he", "llo"}, streamErr: errors.New("upstream died"), failAfter: 2}
	srv := newTestServer(t, eng)

	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", p13StreamReq, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 once content has been delivered: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"he", "llo"} {
		if !strings.Contains(body, want) {
			t.Fatalf("delivered content %q missing from the stream:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("stream missing the terminal frame:\n%s", body)
	}
}

// TestAUD_PERF_001SuccessStreamUnaffected: a clean stream is unchanged.
func TestAUD_PERF_001SuccessStreamUnaffected(t *testing.T) {
	eng := &fakeEngine{deltas: []string{"a", "b", "c"}, failAfter: -1}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", p13StreamReq, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"a", "b", "c", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q:\n%s", want, body)
		}
	}
}

// BenchmarkAUD_PERF_001StreamRetention reports the bytes the handler
// allocates per streamed response. The counter makes this independent of
// the response size (before the fix, retention was O(response bytes):
// the same text was written twice, once to the client and once into the
// builder).
func BenchmarkAUD_PERF_001StreamRetention(b *testing.B) {
	deltas := make([]string, 512)
	for i := range deltas {
		deltas[i] = strings.Repeat("x", 256) // 128 KiB of streamed content
	}
	eng := &fakeEngine{deltas: deltas, failAfter: -1}
	srv := New(eng, WithStaticKeys("test-key-123"))
	defer srv.Close()

	req := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(p13StreamReq))
		r.Header.Set("Authorization", "Bearer test-key-123")
		return r
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &discardResponseWriter{header: http.Header{}}
		srv.ServeHTTP(w, req())
		if w.status != http.StatusOK {
			b.Fatalf("benchmark request answered %d (the streaming path must be measured)", w.status)
		}
	}
}

// discardResponseWriter drains the stream without retaining it, so the
// benchmark measures the HANDLER's retention, not the recorder's.
type discardResponseWriter struct {
	header http.Header
	status int
	n      int
}

func (d *discardResponseWriter) Header() http.Header { return d.header }
func (d *discardResponseWriter) WriteHeader(s int)   { d.status = s }
func (d *discardResponseWriter) Write(p []byte) (int, error) {
	d.n += len(p)
	return len(p), nil
}
func (d *discardResponseWriter) Flush() {}

// p13BigStreamEngine delivers n chunks of size bytes, then blocks until
// released — so a test can measure the heap WHILE the stream is live.
type p13BigStreamEngine struct {
	n       int
	size    int
	mid     chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *p13BigStreamEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "x", Model: req.Model, FinishReason: "stop"}, nil
}

func (e *p13BigStreamEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	chunk := strings.Repeat("z", e.size)
	for i := 0; i < e.n; i++ {
		if err := onDelta(chunk); err != nil {
			return nil, err
		}
	}
	e.once.Do(func() { close(e.mid) })
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &provider.ChatResponse{Content: chunk, Model: req.Model, FinishReason: "stop"}, nil
}

// TestAUD_PERF_001LiveStreamDoesNotRetainResponse measures the heap
// WHILE a large stream is in flight. The old code accumulated every
// delivered byte in a strings.Builder that outlived the writes for the
// whole stream, so a busy gateway held concurrency x response bytes.
// The counter holds none of it: growth stays flat regardless of size.
func TestAUD_PERF_001LiveStreamDoesNotRetainResponse(t *testing.T) {
	const (
		chunks    = 128
		chunkSize = 64 << 10 // 8 MiB streamed in total
		maxGrowth = 3 << 20  // 3 MiB of slack for SSE framing buffers
	)
	eng := &p13BigStreamEngine{n: chunks, size: chunkSize, mid: make(chan struct{}), release: make(chan struct{})}
	srv := newTestServer(t, eng)

	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w := &discardResponseWriter{header: http.Header{}}
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(p13StreamReq))
		req.Header.Set("Authorization", "Bearer test-key-123")
		srv.ServeHTTP(w, req)
	}()

	select {
	case <-eng.mid:
	case <-time.After(30 * time.Second):
		close(eng.release)
		t.Fatal("engine never delivered its chunks")
	}

	var during runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&during)
	growth := int64(during.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("heap growth while %d MiB was in flight: %d bytes", (chunks*chunkSize)>>20, growth)

	close(eng.release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("stream did not finish after release")
	}

	if growth > maxGrowth {
		t.Fatalf("handler retained %d bytes of a %d MiB stream (budget %d): the response text must not be buffered",
			growth, (chunks*chunkSize)>>20, maxGrowth)
	}
}
