// Request-scoped outcome attribution under concurrency (CHECKLIST
// 317-318): N parallel requests with distinct decisions each
// attribute to their own decision/trace, even when one request is
// delayed so a neighbour's decision lands in the engine-global
// slots first.
package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// attributionEngine builds an engine whose AutoRoute hook returns
// a distinct decision per prompt ("slow-task" -> the slow coding
// decision, "fast-task-<i>" -> a distinct quick-qa decision) and
// whose single provider delays the slow prompt, so the slow
// request is still in flight when the fast ones finish — the
// exact interleaving where the old global-last read
// misattributed.
func attributionEngine(t *testing.T, slowDelay time.Duration) *Engine {
	t.Helper()
	slow := &fakeClient{}
	slow.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if prompt := lastUserPrompt(req); strings.Contains(prompt, "slow-task") {
			select {
			case <-time.After(slowDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	slow.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if prompt := lastUserPrompt(req); strings.Contains(prompt, "slow-task") {
			select {
			case <-time.After(slowDelay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if onDelta != nil {
			if err := onDelta("ok"); err != nil {
				return nil, err
			}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	eng := NewEngine(map[string]provider.Client{"p": slow}, nil, Config{
		HedgeDelay: -1, // hedging off: one attempt per request, deterministic
		// Limiter off: 8 parallel requests on one provider must
		// all be admitted — the Gradient2 limiter would learn
		// from the slow request's RTT and shed a fast neighbour
		// under load ("adaptive concurrency limit reached"),
		// which is limiter behaviour, not attribution behaviour.
		DisableAdaptiveLimiter: true,
		ChainFor: func(model string) []Attempt {
			return []Attempt{{ProviderID: "p", Model: model}}
		},
		AutoRoute: func(model, prompt string) (string, autorouter.Decision, bool) {
			if strings.Contains(prompt, "slow-task") {
				return "model-slow", autorouter.Decision{
					ID: "dec-slow", VirtualModel: "auto",
					Tier: autorouter.TierCoding, Confidence: 0.9, Model: "model-slow",
				}, true
			}
			if strings.HasPrefix(prompt, "fast-task-") {
				id := "dec-" + prompt
				return "model-" + prompt, autorouter.Decision{
					ID: id, VirtualModel: "auto",
					Tier: autorouter.TierQuickQA, Confidence: 0.9, Model: "model-" + prompt,
				}, true
			}
			return "", autorouter.Decision{}, false
		},
	})
	return eng
}

func attributionReq(prompt string) provider.ChatRequest {
	return provider.ChatRequest{Model: "auto", Messages: []provider.Message{{Role: "user", Content: prompt}}}
}

// TestAttributionUnderConcurrency is the 317-318 sensitivity
// proof: 8 parallel auto-routed requests with distinct decisions,
// one delayed 500 ms so its neighbours finish (and publish their
// decisions/traces into the engine globals) while it is still in
// flight. Each request's own sink must hold exactly its own
// decision and trace — never a neighbour's. On the old code there
// was no sink at all: the server read the globals after the
// engine call returned, and the control below shows what that
// read returns while the slow request is in flight — the
// neighbour's decision and trace, not the slow request's.
func TestAttributionUnderConcurrency(t *testing.T) {
	eng := attributionEngine(t, 500*time.Millisecond)

	// Control — the old path's hazard, pinned with the old API
	// only: while the slow request is in flight and a fast
	// neighbour has already finished, the engine-global reads
	// return the NEIGHBOUR's decision and trace.
	slowAtt := NewAttribution()
	slowCtx := WithAttribution(context.Background(), slowAtt)
	slowDone := make(chan error, 1)
	go func() {
		_, err := eng.Chat(slowCtx, attributionReq("slow-task"))
		slowDone <- err
	}()
	// Let the slow request enter the provider call.
	time.Sleep(50 * time.Millisecond)
	fastAtt := NewAttribution()
	fastCtx := WithAttribution(context.Background(), fastAtt)
	if _, err := eng.Chat(fastCtx, attributionReq("fast-task-0")); err != nil {
		t.Fatalf("fast neighbour Chat: %v", err)
	}
	// The fast neighbour finished; the slow request is still
	// sleeping. The globals now name the neighbour.
	if tr := eng.LastTrace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-fast-task-0" {
		t.Fatalf("LastTrace while slow in flight = %+v, want the fast neighbour's trace (model-fast-task-0)", tr)
	}
	if dec, ok := eng.LastAutoDecision(); !ok || dec.ID != "dec-fast-task-0" {
		t.Fatalf("LastAutoDecision while slow in flight = %+v (ok=%v), want the fast neighbour's decision dec-fast-task-0", dec, ok)
	}
	// ...and the slow request's own sink holds nothing of the
	// neighbour's: its decision is its own (published at
	// resolve time, before dialling) and its trace is still
	// absent (published only when the call finishes).
	if dec, ok := slowAtt.Decision(); ok && dec.ID != "dec-slow" {
		t.Fatalf("slow sink Decision while in flight = %+v, want dec-slow or none — never the neighbour's dec-fast-task-0", dec)
	}
	if tr := slowAtt.Trace(); tr != nil {
		t.Fatalf("slow sink Trace while in flight = %+v, want nil (neighbour's trace must never leak in)", tr)
	}
	if dec, ok := fastAtt.Decision(); !ok || dec.ID != "dec-fast-task-0" {
		t.Fatalf("fast sink Decision = %+v (ok=%v), want dec-fast-task-0", dec, ok)
	}
	if tr := fastAtt.Trace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-fast-task-0" {
		t.Fatalf("fast sink Trace = %+v, want one attempt on model-fast-task-0", tr)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow Chat: %v", err)
	}
	if dec, ok := slowAtt.Decision(); !ok || dec.ID != "dec-slow" {
		t.Fatalf("slow sink Decision after return = %+v (ok=%v), want dec-slow", dec, ok)
	}
	if tr := slowAtt.Trace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-slow" {
		t.Fatalf("slow sink Trace after return = %+v, want one attempt on model-slow", tr)
	}

	// Main run — 8 parallel requests, one slow, seven fast, all
	// behind a start barrier so the burst is a real contention
	// window.
	const n = 8
	prompts := make([]string, n)
	prompts[0] = "slow-task"
	for i := 1; i < n; i++ {
		prompts[i] = fmt.Sprintf("fast-task-%d", i)
	}
	atts := make([]*Attribution, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			atts[i] = NewAttribution()
			ctx := WithAttribution(context.Background(), atts[i])
			_, errs[i] = eng.Chat(ctx, attributionReq(prompts[i]))
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d (%s): Chat: %v", i, prompts[i], errs[i])
		}
		wantID := "dec-" + prompts[i]
		if i == 0 {
			wantID = "dec-slow"
		}
		dec, ok := atts[i].Decision()
		if !ok || dec.ID != wantID {
			t.Fatalf("request %d (%s): sink Decision = %+v (ok=%v), want id %q", i, prompts[i], dec, ok, wantID)
		}
		wantModel := "model-" + prompts[i]
		if i == 0 {
			wantModel = "model-slow"
		}
		tr := atts[i].Trace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != wantModel || tr.Attempts[0].Provider != "p" {
			t.Fatalf("request %d (%s): sink Trace = %+v, want one attempt on provider p model %q", i, prompts[i], tr, wantModel)
		}
	}
}

// TestAttributionStreamUnderConcurrency proves the same
// request-scoping on the streaming path (runStream publishes the
// trace; resolveAuto publishes the decision): two parallel
// streams with distinct decisions each hold their own in their
// sink, even though the slow stream is still running when the
// fast one ends.
func TestAttributionStreamUnderConcurrency(t *testing.T) {
	eng := attributionEngine(t, 300*time.Millisecond)
	slowAtt, fastAtt := NewAttribution(), NewAttribution()
	slowDone := make(chan error, 1)
	go func() {
		_, err := eng.ChatStream(WithAttribution(context.Background(), slowAtt), attributionReq("slow-task"), func(string) error { return nil })
		slowDone <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if _, err := eng.ChatStream(WithAttribution(context.Background(), fastAtt), attributionReq("fast-task-0"), func(string) error { return nil }); err != nil {
		t.Fatalf("fast stream: %v", err)
	}
	if dec, ok := fastAtt.Decision(); !ok || dec.ID != "dec-fast-task-0" {
		t.Fatalf("fast stream sink Decision = %+v (ok=%v), want dec-fast-task-0", dec, ok)
	}
	if tr := fastAtt.Trace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-fast-task-0" {
		t.Fatalf("fast stream sink Trace = %+v, want one attempt on model-fast-task-0", tr)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow stream: %v", err)
	}
	if dec, ok := slowAtt.Decision(); !ok || dec.ID != "dec-slow" {
		t.Fatalf("slow stream sink Decision = %+v (ok=%v), want dec-slow", dec, ok)
	}
	if tr := slowAtt.Trace(); tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-slow" {
		t.Fatalf("slow stream sink Trace = %+v, want one attempt on model-slow", tr)
	}
}

// TestAttributionStreamEarlyErrorPublishesTrace proves the
// stream entry rejections (empty chain here) publish their
// attempt-less trace into the request's sink too, so the
// server's outcome recording for a refused stream sees this
// request's (empty) trace instead of a neighbour's.
func TestAttributionStreamEarlyErrorPublishesTrace(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"p": &fakeClient{}}, nil, Config{
		HedgeDelay: -1,
		ChainFor:   func(model string) []Attempt { return nil },
	})
	att := NewAttribution()
	ctx := WithAttribution(context.Background(), att)
	_, err := eng.ChatStreamWithChain(ctx, nil, attributionReq("slow-task"))
	if err == nil {
		t.Fatal("ChatStreamWithChain on an empty chain: want an error, got nil")
	}
	tr := att.Trace()
	if tr == nil {
		t.Fatal("sink Trace after empty-chain stream rejection: want the attempt-less trace, got nil")
	}
	if len(tr.Attempts) != 0 {
		t.Fatalf("sink Trace attempts = %d, want 0 (no provider was dialled)", len(tr.Attempts))
	}
}

// TestAttributionCoalescedWaiterSharesFlightTrace proves a
// coalesced waiter — two identical requests sharing one flight —
// attributes to the shared flight's trace: the waiter's sink
// holds the leader's trace (the flight that served it), not a
// stale trace from an earlier request.
func TestAttributionCoalescedWaiterSharesFlightTrace(t *testing.T) {
	eng := attributionEngine(t, 300*time.Millisecond)
	// Identical prompts coalesce (same model, same messages).
	atts := [2]*Attribution{NewAttribution(), NewAttribution()}
	errs := [2]error{}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx := WithAttribution(context.Background(), atts[i])
			_, errs[i] = eng.Chat(ctx, attributionReq("slow-task"))
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < 2; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: Chat: %v", i, errs[i])
		}
		dec, ok := atts[i].Decision()
		if !ok || dec.ID != "dec-slow" {
			t.Fatalf("request %d: sink Decision = %+v (ok=%v), want dec-slow", i, dec, ok)
		}
		tr := atts[i].Trace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Model != "model-slow" {
			t.Fatalf("request %d: sink Trace = %+v, want one attempt on model-slow", i, tr)
		}
	}
}
