// Cancel propagation + always-terminal stream events + non-streaming
// empty-completion failover, proven end to end (CHECKLIST 153):
// the real Server + the real router.Engine + real provider clients
// against local httptest mocks — no fakeEngine.
package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// gateEngine builds a real engine over two real clients with a fixed
// dead-first chain, the same shape as the preread gate's engine.
func gateEngine(t *testing.T, dead, fallback provider.Client) *router.Engine {
	t.Helper()
	return router.NewEngine(map[string]provider.Client{
		"dead":     dead,
		"fallback": fallback,
	}, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "dead", Model: model}, {ProviderID: "fallback", Model: model}}
		},
	})
}

func mustGateClient(t *testing.T, srv *httptest.Server, key string) provider.Client {
	t.Helper()
	c, err := provider.NewClient(srv.URL, key, provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// TestChatCompletionsStreamingClientAbortStopsUpstream is the
// end-to-end proof of (a): a client that disconnects mid-stream
// must stop the mock upstream's writes — the abort propagates
// through the gateway (request ctx → engine ctx → provider client
// ctx → transport abort), so no tokens are spent after the client
// is gone. The mock counts writes after abort: zero new writes
// past a grace window for the one already in flight.
func TestChatCompletionsStreamingClientAbortStopsUpstream(t *testing.T) {
	var mu sync.Mutex
	var writes []time.Time
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		tick := time.NewTicker(40 * time.Millisecond)
		defer tick.Stop()
		for i := 1; ; i++ {
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
				if _, err := fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"t%d \"},\"finish_reason\":null}]}\n\n", i); err != nil {
					return
				}
				fl.Flush()
				mu.Lock()
				writes = append(writes, time.Now())
				mu.Unlock()
			}
		}
	}))
	defer dead.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"bee\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	}))
	defer fallback.Close()

	eng := gateEngine(t, mustGateClient(t, dead, "dead-key"), mustGateClient(t, fallback, "fallback-key"))
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))
	live := httptest.NewServer(srv)
	defer live.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // the abort path cancels explicitly; this covers the never-abort path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, live.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key-123")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	// Read two content chunks, then disconnect: the client is gone.
	var cancelAt time.Time
	seen := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"content":"t`) {
			seen++
			if seen == 2 {
				cancelAt = time.Now()
				cancel()
				break
			}
		}
	}
	if seen != 2 {
		t.Fatalf("client saw %d content chunks, want 2", seen)
	}
	// The gateway's handler must unwind promptly once its ctx ends;
	// give the mock time to prove it stopped writing.
	time.Sleep(600 * time.Millisecond)
	mu.Lock()
	late := 0
	for _, w := range writes {
		if w.After(cancelAt.Add(500 * time.Millisecond)) {
			late++
		}
	}
	total := len(writes)
	mu.Unlock()
	if late != 0 {
		t.Fatalf("mock wrote %d SSE lines after abort (+500ms grace), want 0", late)
	}
	if total > 8 {
		t.Fatalf("mock wrote %d lines total, want <= 8 (2 delivered + in-flight slack)", total)
	}
	if trace := eng.LastTrace(); trace == nil || len(trace.Attempts) != 1 {
		t.Fatalf("trace = %+v, want exactly 1 attempt", trace)
	} else if trace.Attempts[0].Terminal != "cancelled" {
		t.Fatalf("trace attempt terminal = %q, want cancelled", trace.Attempts[0].Terminal)
	}
}

// TestChatCompletionsNonStreamingEmptyFailsOver is the server-level
// proof of (c): a primary that answers 200 with an empty completion
// fails over in the SAME request, before the 200 commits — the
// client gets the fallback's content, never an empty body.
func TestChatCompletionsNonStreamingEmptyFailsOver(t *testing.T) {
	var deadHits, fallbackHits atomic.Int32
	emptyBody := `{"id":"chatcmpl-x","object":"chat.completion","created":1,"model":"gate-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10}}`
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(emptyBody))
	}))
	defer dead.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","created":1,"model":"gate-model","choices":[{"index":0,"message":{"role":"assistant","content":"fallback says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	}))
	defer fallback.Close()

	eng := gateEngine(t, mustGateClient(t, dead, "dead-key"), mustGateClient(t, fallback, "fallback-key"))
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "fallback says hi") {
		t.Fatalf("body missing the fallback's content: %s", rec.Body.String())
	}
	if got := deadHits.Load(); got != 1 {
		t.Fatalf("dead hits = %d, want 1", got)
	}
	if got := fallbackHits.Load(); got != 1 {
		t.Fatalf("fallback hits = %d, want 1", got)
	}
	trace := eng.LastTrace()
	if trace == nil || len(trace.Attempts) != 2 {
		t.Fatalf("trace = %+v, want 2 attempts", trace)
	}
	if got := trace.Attempts[0]; got.Reason != "empty_stream" {
		t.Fatalf("trace attempt 0 = %+v, want reason empty_stream", got)
	}

	// Both targets empty: an honest error, never a fake empty 200.
	eng2 := gateEngine(t, mustGateClient(t, dead, "dead-key"), mustGateClient(t, dead, "dead-key"))
	srv2 := newTestServer(t, eng2, WithTimeout(10*time.Second))
	rec2 := doReq(t, srv2, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec2.Code != http.StatusBadGateway {
		t.Fatalf("all-empty status = %d, want 502 (body %s)", rec2.Code, rec2.Body.String())
	}
}
