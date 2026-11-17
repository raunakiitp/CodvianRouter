package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sort"
	"strconv"
	"testing"
	"time"
)

func TestTFOStatusProbe(t *testing.T) {
	st := TFOStatus()
	if st.Platform == "" {
		t.Fatal("TFO status has no platform")
	}
	if st.Reason == "" {
		t.Fatal("TFO status has no reason")
	}
	if st.Active && !st.Supported {
		t.Fatal("TFO active without supported — contradictory probe")
	}
	if runtime.GOOS == "linux" {
		// This VM's sysctl is the ground truth for the assertion:
		// Active must match the client bit (0x1) of
		// net.ipv4.tcp_fastopen, and the probe must have succeeded.
		if !st.KernelKnown || st.KernelValue == "" {
			t.Fatal("linux TFO status did not read the sysctl")
		}
		v, err := strconv.Atoi(st.KernelValue)
		if err != nil {
			t.Fatalf("sysctl value %q is not a number: %v", st.KernelValue, err)
		}
		if !st.Supported {
			t.Fatalf("linux probe refused TCP_FASTOPEN_CONNECT: %s", st.Reason)
		}
		if wantActive := v&1 != 0; st.Active != wantActive {
			t.Fatalf("Active=%v but sysctl=%d (client bit set=%v): %s", st.Active, v, wantActive, st.Reason)
		}
		t.Logf("TFO status: supported=%v active=%v sysctl=%d platform=%s", st.Supported, st.Active, v, st.Platform)
	}
}

// mockChatServer answers the OpenAI chat-completions shape over
// plain HTTP (TFO is a TCP-level option, so plain HTTP measures
// the dial the same way TLS would, minus the handshake bytes).
func mockChatServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
		case r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// measureColdDials makes n requests, each through a FRESH client
// (fresh Transport, so every request is a cold dial — no pooled
// connection can hide the handshake), and returns the latencies.
// The first dial to a host has no TFO cookie yet, so TFO can only
// pay from the second dial on: the mean therefore measures the
// steady cold-dial cost, which is the honest comparison.
func measureColdDials(t *testing.T, srv *httptest.Server, tfo bool, n int) []time.Duration {
	t.Helper()
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}
	out := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		c, err := NewClient(srv.URL, "k", WithTCPFastOpen(tfo))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		start := time.Now()
		_, err = c.Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("Chat (tfo=%v): %v", tfo, err)
		}
		out = append(out, time.Since(start))
	}
	return out
}

func meanMs(ds []time.Duration) float64 {
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return float64(sum.Microseconds()) / 1000 / float64(len(ds))
}

func TestTFOColdDialLatencyMeasured(t *testing.T) {
	srv := mockChatServer(t)
	defer srv.Close()
	const n = 20

	before := TFOStatus().DialAttempts
	withTFO := measureColdDials(t, srv, true, n)
	afterOn := TFOStatus().DialAttempts
	if got := afterOn - before; got == 0 {
		t.Fatal("TFO dialer made no dial attempts — the Control hook never ran")
	}
	t.Logf("TFO dial attempts during %d cold dials: %d (errors: %d)", n, afterOn-before, TFOStatus().DialErrors)

	beforeOff := TFOStatus().DialAttempts
	withoutTFO := measureColdDials(t, srv, false, n)
	if got := TFOStatus().DialAttempts - beforeOff; got != 0 {
		t.Fatalf("TFO-off dialer ran the Control hook %d times, want 0", got)
	}

	// Sort for a median: one slow dial (GC, scheduling) must not
	// dominate the comparison.
	sorted := func(ds []time.Duration) []time.Duration {
		cp := append([]time.Duration(nil), ds...)
		sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
		return cp
	}
	on, off := sorted(withTFO), sorted(withoutTFO)
	onMean, offMean := meanMs(withTFO), meanMs(withoutTFO)
	onMed, offMed := float64(on[n/2].Microseconds())/1000, float64(off[n/2].Microseconds())/1000
	// Measured numbers only — on localhost the RTT is ~0, so TFO
	// is EXPECTED to show no gain here; the test proves both
	// paths work and the measurement exists, not that TFO wins.
	// The only hard assertion is that TFO does not make cold
	// dials pathologically slower (5x the no-TFO mean).
	t.Logf("cold dial (fresh client per request, n=%d, localhost): TFO mean %.3fms median %.3fms | no-TFO mean %.3fms median %.3fms", n, onMean, onMed, offMean, offMed)
	if onMean > 5*offMean+5 {
		t.Fatalf("TFO cold dials pathologically slower: mean %.3fms vs no-TFO %.3fms", onMean, offMean)
	}
}

func TestTFOTransportIsDefault(t *testing.T) {
	srv := mockChatServer(t)
	defer srv.Close()
	c, err := NewClient(srv.URL, "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*openAIClient)
	if !ok {
		t.Fatalf("default client is %T, want *openAIClient", c)
	}
	if oc.httpClient.Transport == nil {
		t.Fatal("default client has no Transport — the TFO dialer is not installed")
	}
	if _, ok := oc.httpClient.Transport.(*http.Transport); !ok {
		t.Fatalf("Transport is %T, want *http.Transport", oc.httpClient.Transport)
	}
	// A caller-supplied client keeps its own Transport untouched.
	custom := &http.Client{Timeout: time.Second}
	c2, err := NewClient(srv.URL, "k", WithHTTPClient(custom))
	if err != nil {
		t.Fatalf("NewClient custom: %v", err)
	}
	if oc2, ok := c2.(*openAIClient); !ok || oc2.httpClient != custom {
		t.Fatal("WithHTTPClient did not keep the caller's client")
	}
}
