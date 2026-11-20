package router

// LiteLLM v1.111.0-rc.1 failover cluster (CHECKLIST 243-244,
// research-log entries 222-224). Each test names the rule it
// proves; where the behaviour already held, the test is the
// regression proof (noted in the test doc), and where it did
// not, the test failed first (sensitivity is noted per test).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// ---------------------------------------------------------------------------
// (a) A pinned deployment is escapable on a retryable failure.
// ---------------------------------------------------------------------------

// TestPinLeadsDespiteQuotaHint proves the pin actually leads the
// chain: a pinned provider with a lower quota hint is still tried
// first — the quota-hint sort must not demote the pin below other
// targets. FAILED before the fix (pinFirst): the engine served
// "bee" from B, never trying pinned A.
func TestPinLeadsDespiteQuotaHint(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	pinned := func() provider.ChatRequest {
		req := testReq()
		req.Model = "m"
		req.PinProviderID = "a"
		return req
	}
	cfg := testConfig()
	cfg.QuotaHint = func(id string) int {
		if id == "a" {
			return 10
		}
		return 90
	}
	t.Run("non-streaming", func(t *testing.T) {
		a := okClient("aye")
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
		resp, _, err := eng.ChatWithChain(context.Background(), chain, pinned())
		if err != nil || resp == nil || resp.Content != "aye" {
			t.Fatalf("Chat = %+v err = %v, want aye from pinned A despite lower quota hint", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 0 {
			t.Fatalf("calls a=%d b=%d, want 1/0: pin must lead", a.chatCalls(), b.chatCalls())
		}
	})
	t.Run("streaming", func(t *testing.T) {
		a := streamClient([]string{"aye"}, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, pinned())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "aye" {
			t.Fatalf("stream = %q err = %v, want aye from pinned A", out, err)
		}
		if a.streamCalls() != 1 || b.streamCalls() != 0 {
			t.Fatalf("calls a=%d b=%d, want 1/0: pin must lead", a.streamCalls(), b.streamCalls())
		}
	})
}

// TestPinEscapesOnRetryableFailure proves the pin is escapable:
// a retryable failure (429, 5xx, empty stream) on the pinned
// deployment moves the request to the next target — the pinned
// deployment is tried exactly once, never retried while another
// target remains. Regression proof: the loop already advanced
// past a failed pin (CHECKLIST 237); these tests pin the rule
// for every retryable class, both paths.
func TestPinEscapesOnRetryableFailure(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	pinned := func() provider.ChatRequest {
		req := testReq()
		req.Model = "m"
		req.PinProviderID = "a"
		return req
	}
	t.Run("non-streaming-429", func(t *testing.T) {
		a := errClient(&provider.RateLimitError{RetryAfter: 0})
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, pinned())
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee via failover", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1 (no same-provider retry of the pin)", a.chatCalls(), b.chatCalls())
		}
	})
	t.Run("non-streaming-5xx", func(t *testing.T) {
		a := errClient(errors.New("boom"))
		b := okClient("bee")
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		resp, _, err := eng.ChatWithChain(context.Background(), chain, pinned())
		if err != nil || resp == nil || resp.Content != "bee" {
			t.Fatalf("Chat = %+v err = %v, want bee via failover", resp, err)
		}
		if a.chatCalls() != 1 || b.chatCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 1/1", a.chatCalls(), b.chatCalls())
		}
	})
	t.Run("streaming-5xx-before-output", func(t *testing.T) {
		a := streamClient(nil, errors.New("boom"))
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, pinned())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want bee via failover", out, err)
		}
		// M4.8 early-retry: a truncating/transport-blip stream is
		// re-opened on the same target up to 3 times (1 + 3 under
		// the 5-attempt budget, leaving one attempt for the
		// failover) before the chain moves on — the outcome is
		// the same failover, only the same-target count grows.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
	})
	t.Run("streaming-empty-stream", func(t *testing.T) {
		a := streamClient(nil, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, pinned())
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want bee via failover", out, err)
		}
		// M4.8 early-retry: a truncating/transport-blip stream is
		// re-opened on the same target up to 3 times (1 + 3 under
		// the 5-attempt budget, leaving one attempt for the
		// failover) before the chain moves on — the outcome is
		// the same failover, only the same-target count grows.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
	})
}

// TestPinEscapeCountedOnce proves the pin-escape counter counts
// one escape per request, not one per hop past the pin: a request
// that escapes the pin and then fails over again still counts a
// single escape.
func TestPinEscapeCountedOnce(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	req := testReq()
	req.Model = "m"
	req.PinProviderID = "a"
	a := errClient(errors.New("boom"))
	b := errClient(errors.New("boom"))
	c := okClient("cee")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, nil, testConfig())
	resp, _, err := eng.ChatWithChain(context.Background(), chain, req)
	if err != nil || resp == nil || resp.Content != "cee" {
		t.Fatalf("Chat = %+v err = %v, want cee via failover", resp, err)
	}
	stats := eng.Failover243Stats()
	if stats.PinEscapes != 1 {
		t.Fatalf("pin escapes = %d, want 1", stats.PinEscapes)
	}
	if stats.LastPin != "a" || stats.LastEscapedTo != "b" {
		t.Fatalf("last escape = %q -> %q, want a -> b", stats.LastPin, stats.LastEscapedTo)
	}
	if stats.LastEscapeAt.IsZero() {
		t.Fatal("last escape time is zero")
	}
}

// ---------------------------------------------------------------------------
// (b) A Responses fallback must never replay a tool call the user
// already saw (engine half: the resumed stream).
// ---------------------------------------------------------------------------

// TestResumeDoesNotReplayDeliveredToolCall proves a resumed
// stream never re-delivers a tool call the client already saw:
// the provider replays the turn from the start, so the same call
// (same id) arrives again — the drain drops it, while a NEW call
// still flows and the reassembled response stays complete.
// FAILED before the fix: the re-emitted call_1 reached onChunk.
func TestResumeDoesNotReplayDeliveredToolCall(t *testing.T) {
	toolOld := provider.StreamChunk{ToolCalls: []provider.ChunkToolCall{
		{Index: 0, ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`},
	}}
	toolNew := provider.StreamChunk{ToolCalls: []provider.ChunkToolCall{
		{Index: 1, ID: "call_2", Name: "get_time", Arguments: `{"zone":"UTC"}`},
	}}
	b := prereadChunks([]provider.StreamChunk{
		{Content: "hello "},
		toolOld,
		toolNew,
		{FinishReason: "tool_calls"},
	}, nil)
	eng := NewEngine(map[string]provider.Client{"b": b}, nil, testConfig())
	req := testReq()
	req.Model = "m"
	// The client already received call_1 on the broken stream;
	// only the text prefix is skipped by runes.
	req.DeliveredToolCallIDs = []string{"call_1"}
	var delivered []provider.StreamChunk
	resp, err := eng.chatStreamDrainChunks(context.Background(),
		[]Attempt{{ProviderID: "b", Model: "m"}}, req, len("hello "),
		func(ck provider.StreamChunk) error {
			delivered = append(delivered, ck)
			return nil
		}, "", false)
	if err != nil || resp == nil {
		t.Fatalf("drain = %+v err = %v", resp, err)
	}
	for _, ck := range delivered {
		for _, tc := range ck.ToolCalls {
			if tc.ID == "call_1" {
				t.Fatalf("resumed stream re-delivered tool call %q (already delivered before the break)", tc.ID)
			}
		}
	}
	sawNew := false
	for _, ck := range delivered {
		for _, tc := range ck.ToolCalls {
			if tc.ID == "call_2" {
				sawNew = true
			}
		}
	}
	if !sawNew {
		t.Fatalf("delivered chunks = %+v, want the NEW call_2 to flow", delivered)
	}
	// The reassembled response still holds both calls: only the
	// wire delivery was deduped, never the response object.
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("resp tool calls = %+v, want both calls reassembled", resp.ToolCalls)
	}
	if stats := eng.Failover243Stats(); stats.ToolReplaysSuppressed != 1 {
		t.Fatalf("tool replays suppressed = %d, want 1", stats.ToolReplaysSuppressed)
	}
}

// TestResumeWithoutDeliveredIDsDeliversEverything proves the
// no-replay filter is inert on a fresh stream: with no delivered
// ids named, every tool call flows exactly as before.
func TestResumeWithoutDeliveredIDsDeliversEverything(t *testing.T) {
	b := prereadChunks([]provider.StreamChunk{
		{Content: "hello "},
		{ToolCalls: []provider.ChunkToolCall{{Index: 0, ID: "call_1", Name: "get_weather", Arguments: "{}"}}},
		{FinishReason: "tool_calls"},
	}, nil)
	eng := NewEngine(map[string]provider.Client{"b": b}, nil, testConfig())
	req := testReq()
	req.Model = "m"
	var delivered []provider.StreamChunk
	resp, err := eng.chatStreamDrainChunks(context.Background(),
		[]Attempt{{ProviderID: "b", Model: "m"}}, req, 0,
		func(ck provider.StreamChunk) error {
			delivered = append(delivered, ck)
			return nil
		}, "", false)
	if err != nil || resp == nil {
		t.Fatalf("drain = %+v err = %v", resp, err)
	}
	saw := false
	for _, ck := range delivered {
		for _, tc := range ck.ToolCalls {
			if tc.ID == "call_1" {
				saw = true
			}
		}
	}
	if !saw {
		t.Fatal("fresh stream suppressed a tool call it should have delivered")
	}
	if stats := eng.Failover243Stats(); stats.ToolReplaysSuppressed != 0 {
		t.Fatalf("tool replays suppressed = %d, want 0 on a fresh stream", stats.ToolReplaysSuppressed)
	}
}

// ---------------------------------------------------------------------------
// (c) A stream that breaks before any output is a same-model-group
// retry, not a fresh chain restart.
// ---------------------------------------------------------------------------

// TestPreOutputBreakIsSameGroupRetry proves a pre-output break
// retries inside the same chain (same model, same request — no
// fresh chain resolution, no different model group): the next
// attempt receives the identical model and messages. Regression
// proof: runStream already continued in-chain; this pins it for
// both the error and the empty-stream break.
func TestPreOutputBreakIsSameGroupRetry(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	req := testReq()
	req.Model = "m"
	wantMsgs := req.Messages
	t.Run("error-before-output", func(t *testing.T) {
		a := streamClient(nil, errors.New("boom"))
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, req)
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want bee via same-group retry", out, err)
		}
		// M4.8 early-retry: a truncating/transport-blip stream is
		// re-opened on the same target up to 3 times (1 + 3 under
		// the 5-attempt budget, leaving one attempt for the
		// failover) before the chain moves on — the outcome is
		// the same failover, only the same-target count grows.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
		got, ok := b.lastStreamReq()
		if !ok || got.Model != "m" || len(got.Messages) != len(wantMsgs) {
			t.Fatalf("B request = model %q messages %+v, want same model group m and the same request", got.Model, got.Messages)
		}
		if trace := eng.LastTrace(); len(trace.Attempts) != 5 || trace.Attempts[4].Outcome != "success" {
			t.Fatalf("trace = %+v, want a-fail x4 then b-success in one chain", trace)
		}
	})
	t.Run("empty-stream-before-output", func(t *testing.T) {
		a := streamClient(nil, nil)
		b := streamClient([]string{"bee"}, nil)
		eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
		ch, err := eng.ChatStreamWithChain(context.Background(), chain, req)
		if err != nil {
			t.Fatalf("ChatStreamWithChain: %v", err)
		}
		out, err := collectStream(t, ch)
		if err != nil || out != "bee" {
			t.Fatalf("stream = %q err = %v, want bee via same-group retry", out, err)
		}
		// M4.8 early-retry: a truncating/transport-blip stream is
		// re-opened on the same target up to 3 times (1 + 3 under
		// the 5-attempt budget, leaving one attempt for the
		// failover) before the chain moves on — the outcome is
		// the same failover, only the same-target count grows.
		if a.streamCalls() != 4 || b.streamCalls() != 1 {
			t.Fatalf("calls a=%d b=%d, want 4/1", a.streamCalls(), b.streamCalls())
		}
	})
}

// ---------------------------------------------------------------------------
// (d) Global fallback config must not recurse: it never re-applies
// to its own fallback calls.
// ---------------------------------------------------------------------------

// TestFallbackConfigNotReapplied proves the configured refusal
// chain is derived at most once per request: a refusal on a
// refusal-chain target continues in-chain (idx++) instead of
// re-deriving the configured chain — the config never applies to
// its own fallback calls. Sensitivity: a re-derivation per
// refusal would call the hook twice here.
func TestFallbackConfigNotReapplied(t *testing.T) {
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}}
	cfg := testConfig()
	derivations := 0
	cfg.RefusalChainFor = func(model string) []Attempt {
		derivations++
		return chain
	}
	a := refusalClient()
	b := refusalClient()
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, cfg)
	req := testReq()
	req.Model = "m"
	_, _, err := eng.ChatWithChain(context.Background(), chain, req)
	if !errors.Is(err, ErrContentRefused) {
		t.Fatalf("err = %v, want ErrContentRefused after the refusal chain runs dry", err)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 {
		t.Fatalf("calls a=%d b=%d, want 1/1: each target tried once, never re-offered", a.chatCalls(), b.chatCalls())
	}
	if derivations != 1 {
		t.Fatalf("refusal chain derived %d times, want 1: the config must not re-apply to its own fallback calls", derivations)
	}
}

// TestContextFallbackChainAppliedOnce proves the context-window
// fallback chain is consulted at most once per request: a
// fallback chain that returns the same too-small models does not
// loop — the request fails honestly with ErrContextTooLarge and
// zero provider calls. Sensitivity: a re-application per hop
// would call the hook more than once (or loop forever).
func TestContextFallbackChainAppliedOnce(t *testing.T) {
	chain := []Attempt{{ProviderID: "small", Model: "small-model"}}
	cfg := testConfig()
	cfg.ContextWindow = windowMap(map[string]int{"small\x00small-model": 8192})
	consultations := 0
	cfg.ContextFallbackChain = func(model string) []Attempt {
		consultations++
		return chain // the same too-small model: must not loop
	}
	eng := NewEngine(map[string]provider.Client{"small": okClient("small says hi")}, nil, cfg)
	req := longPrompt(t, "small-model", 6000)
	req.Model = "small-model"
	_, _, err := eng.ChatWithChain(context.Background(), chain, req)
	if err == nil {
		t.Fatal("want ErrContextTooLarge, got nil")
	}
	var tooLarge *ErrContextTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %T %v, want *ErrContextTooLarge", err, err)
	}
	if consultations != 1 {
		t.Fatalf("context fallback chain consulted %d times, want 1", consultations)
	}
}

// ---------------------------------------------------------------------------
// (e) Reset the upstream connection when a stream closes early.
// ---------------------------------------------------------------------------

// TestEarlyCloseResetsUpstream proves cancelling mid-stream stops
// the upstream promptly: the fake blocks between deltas until its
// ctx ends, so if the engine never cancelled the upstream it
// would wait out its 5s timer and emit a third delta; with the
// reset, the stream closes within the test's 3s bound and no
// third delta is ever delivered. Regression proof: bridgeStream
// already unwound the client call via ctx (and the provider
// client test proves the HTTP abort); this pins the engine half.
func TestEarlyCloseResetsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocking := &fakeClient{streamFn: func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		for _, d := range []string{"t1 ", "t2 "} {
			if err := onDelta(d); err != nil {
				return nil, err
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Second):
		}
		if err := onDelta("t3 "); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "t1 t2 t3 "}, nil
	}}
	eng := NewEngine(map[string]provider.Client{"a": blocking}, nil, testConfig())
	req := testReq()
	req.Model = "m"
	ch, err := eng.ChatStreamWithChain(ctx, []Attempt{{ProviderID: "a", Model: "m"}}, req)
	if err != nil {
		t.Fatalf("ChatStreamWithChain: %v", err)
	}
	var got []string
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for ev := range ch {
			if ev.Delta != "" {
				got = append(got, ev.Delta)
				if len(got) == 2 {
					cancel()
				}
			}
		}
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not close within 3s of cancel: upstream not reset")
	}
	for _, d := range got {
		if d == "t3 " {
			t.Fatalf("delivered %q after cancel: upstream kept generating", got)
		}
	}
}
