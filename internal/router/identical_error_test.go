// M4.7d — identical-error early stop (3 consecutive identical
// model-shape 400s stop the chain).
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// modelNotFound400 is the identical model-shape error every
// provider in the test returns: same status, same body.
func modelNotFound400() error {
	return &provider.UpstreamError{StatusCode: 400, Retryable: false, Body: `{"error":{"message":"model 'ghost-9' not found","type":"invalid_request_error"}}`}
}

// TestIdenticalErrorEarlyStop: five targets all return the identical
// 400 model-not-found — the chain stops after 3, the last two are
// never dialled, and the client still sees the real provider error.
func TestIdenticalErrorEarlyStop(t *testing.T) {
	ctx := context.Background()
	clients := map[string]provider.Client{}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		clients[id] = errClient(modelNotFound400())
	}
	eng := NewEngine(clients, nil, Config{HedgeDelay: -1})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c", "d", "e"), coolReq("ghost-9"))
	if err == nil {
		t.Fatalf("want the model-not-found error, got nil")
	}
	for _, id := range []string{"a", "b", "c"} {
		if got := clients[id].(*fakeClient).chatCalls(); got != 1 {
			t.Fatalf("provider %s called %d times, want 1 (the streak counts it)", id, got)
		}
	}
	for _, id := range []string{"d", "e"} {
		if got := clients[id].(*fakeClient).chatCalls(); got != 0 {
			t.Fatalf("provider %s called %d times, want 0 (chain stopped after the 3-streak)", id, got)
		}
	}
}

// TestIdenticalErrorDifferentMessagesKeepGoing: three 400
// model-not-found errors with DIFFERENT messages are not
// identical — the chain tries every target.
func TestIdenticalErrorDifferentMessagesKeepGoing(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.UpstreamError{StatusCode: 400, Body: "model 'ghost-9' not found"})
	b := errClient(&provider.UpstreamError{StatusCode: 400, Body: "The model ghost-9 does not exist"})
	c := errClient(&provider.UpstreamError{StatusCode: 400, Body: "unknown model ghost-9"})
	d := okClient("bee")
	// G11 Area 2: the cross-provider default (3) would stop
	// the chain before d; this test measures identical-error
	// logic, not chain length, so it opts out (4 providers).
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c, "d": d}, nil, Config{HedgeDelay: -1, MaxProvidersPerRequest: 4})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c", "d"), coolReq("ghost-9"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (different messages are not an identical streak)", resp, err)
	}
	for _, cl := range []*fakeClient{a, b, c} {
		if got := cl.chatCalls(); got != 1 {
			t.Fatalf("a provider was called %d times, want 1", got)
		}
	}
}

// TestIdenticalErrorStreakResets: a 500 between two identical 400s
// breaks consecutiveness — the chain tries every target.
func TestIdenticalErrorStreakResets(t *testing.T) {
	ctx := context.Background()
	a := errClient(modelNotFound400())
	b := errClient(&provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"})
	c := errClient(modelNotFound400())
	d := errClient(modelNotFound400())
	e := okClient("bee")
	// G11 Area 2: the cross-provider default (3) would stop
	// the chain before e; this test measures streak resets,
	// not chain length, so it opts out (5 providers).
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c, "d": d, "e": e}, nil, Config{HedgeDelay: -1, MaxProvidersPerRequest: 5})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a", "b", "c", "d", "e"), coolReq("ghost-9"))
	if err != nil || resp.Content != "bee" {
		t.Fatalf("chat = %v, %v, want bee + nil (the 500 reset the streak to 2, not 3)", resp, err)
	}
	if got := e.chatCalls(); got != 1 {
		t.Fatalf("provider E called %d times, want 1 (the chain was not stopped early)", got)
	}
}

// TestIdenticalErrorStream: the same early stop on stream starts —
// three identical 400 model-not-found stream failures stop the
// chain before the fourth target is streamed.
func TestIdenticalErrorStream(t *testing.T) {
	ctx := context.Background()
	clients := map[string]provider.Client{}
	for _, id := range []string{"a", "b", "c", "d"} {
		clients[id] = streamClient(nil, modelNotFound400())
	}
	eng := NewEngine(clients, nil, Config{HedgeDelay: -1})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b", "c", "d"), coolReq("ghost-9"))
	if err != nil {
		t.Fatalf("stream start: %v", err)
	}
	var streamErr error
	for ev := range ch {
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	if streamErr == nil {
		t.Fatalf("want the model-not-found stream error, got none")
	}
	for _, id := range []string{"a", "b", "c"} {
		if got := clients[id].(*fakeClient).streamCalls(); got != 1 {
			t.Fatalf("provider %s streamed %d times, want 1", id, got)
		}
	}
	if got := clients["d"].(*fakeClient).streamCalls(); got != 0 {
		t.Fatalf("provider D streamed %d times, want 0 (chain stopped after the 3-streak)", got)
	}
}

// TestIdenticalModelErrorSignature: only a 400 with a
// model-not-found reason has a signature — a 404 naming a missing
// model, a non-model 400, and a 500 do not.
func TestIdenticalModelErrorSignature(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 model-not-found", modelNotFound400(), true},
		{"404 model-not-found", &provider.UpstreamError{StatusCode: 404, Body: "model 'ghost-9' not found"}, false},
		{"400 other", &provider.UpstreamError{StatusCode: 400, Body: "bad request: temperature out of range"}, false},
		{"500", &provider.UpstreamError{StatusCode: 500, Body: "boom"}, false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if _, ok := identicalModelErrorSignature(tc.err); ok != tc.want {
			t.Fatalf("%s: signature ok = %v, want %v", tc.name, ok, tc.want)
		}
	}
	// Two identical errors share a signature; a different body does not.
	s1, _ := identicalModelErrorSignature(modelNotFound400())
	s2, _ := identicalModelErrorSignature(modelNotFound400())
	s3, _ := identicalModelErrorSignature(&provider.UpstreamError{StatusCode: 400, Body: "model 'other-1' not found"})
	if s1 != s2 {
		t.Fatalf("identical errors have different signatures %q vs %q", s1, s2)
	}
	if s1 == s3 {
		t.Fatalf("different-model errors share a signature %q", s1)
	}
}
