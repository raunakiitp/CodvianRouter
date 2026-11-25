// P13: Streaming tool-capability pre-check (CT-1 streaming path).
//
// The streaming chain loop must apply the SAME confirmed-incapable
// pre-check as the non-streaming loop (router.go): a tool-bearing
// request never dials a CONFIRMED-incapable model — the target is
// skipped with no attempt spent — while non-tool requests and
// unknown models fail open. These tests prove the streaming loop
// honours the gate, using the same capabilityHook helper and
// fakeClient stream helpers the non-streaming CT-1 tests use.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func toolStreamReq(model string) provider.ChatRequest {
	req := coolReq(model)
	req.Tools = []provider.ToolDefinition{{Type: "function"}}
	return req
}

// TestStreamToolPrecheckSkipsIncapableModel: a tool-bearing streaming
// request never dials a confirmed-incapable model — the streaming
// loop skips it (no attempt spent) and the confirmed-capable target
// serves the stream.
func TestStreamToolPrecheckSkipsIncapableModel(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.UpstreamError{StatusCode: 500, Body: "must never be dialled"})
	b := streamClient([]string{"bee", " "}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, true}, // confirmed incapable
			"b": {true, true},  // confirmed capable
		}),
	})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a", "b"), toolStreamReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "bee " {
		t.Fatalf("stream = %q, want %q", got, "bee ")
	}
	if c := a.streamCalls(); c != 0 {
		t.Fatalf("incapable provider A streamed %d times, want 0 (pre-check skips it)", c)
	}
	if c := b.streamCalls(); c != 1 {
		t.Fatalf("capable provider B streamed %d times, want 1", c)
	}
}

// TestStreamToolPrecheckAllowsNoTools: a plain (non-tool) streaming
// request skips the gate — a confirmed-incapable model is still
// dialled, because it may serve text fine.
func TestStreamToolPrecheckAllowsNoTools(t *testing.T) {
	ctx := context.Background()
	a := streamClient([]string{"text"}, nil)
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, true}, // confirmed incapable of tools
		}),
	})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got string
	for ev := range ch {
		if ev.Err != nil {
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
		got += ev.Delta
	}
	if got != "text" {
		t.Fatalf("stream = %q, want %q", got, "text")
	}
	if c := a.streamCalls(); c != 1 {
		t.Fatalf("provider A streamed %d times, want 1 (non-tool request must still dial)", c)
	}
}

// TestStreamToolPrecheckFailsOpenUnknown: an UNKNOWN capability
// fails open — the target is dialled rather than assumed incapable,
// and the upstream's real 400 comes through.
func TestStreamToolPrecheckFailsOpenUnknown(t *testing.T) {
	ctx := context.Background()
	a := streamClient(nil, &provider.UpstreamError{StatusCode: 400, Body: "This model does not support tool use"})
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay: -1,
		ToolCapability: capabilityHook(map[string][2]bool{
			"a": {false, false}, // unknown: fails open
		}),
	})
	ch, err := eng.ChatStreamWithChain(ctx, chainFor("a"), toolStreamReq("m"))
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	sawErr := false
	for ev := range ch {
		if ev.Err != nil {
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatal("expected the upstream error event for an unknown-capability model (fails open, not assumed incapable)")
	}
	if c := a.streamCalls(); c != 1 {
		t.Fatalf("provider A streamed %d times, want 1 (unknown must be dialled)", c)
	}
}
