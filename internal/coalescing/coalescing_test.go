package coalescing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

func testReq(prompt string) provider.ChatRequest {
	return provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: prompt}},
	}
}

func TestRequestKeyStableAndSensitive(t *testing.T) {
	a, err := RequestKey(testReq("hello"))
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	b, err := RequestKey(testReq("hello"))
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	if a != b {
		t.Fatalf("same request gave different keys %q vs %q", a, b)
	}
	if c, _ := RequestKey(testReq("goodbye")); c == a {
		t.Fatalf("different prompt gave the same key")
	}
	other := testReq("hello")
	other.Temperature = 0.7
	if c, _ := RequestKey(other); c == a {
		t.Fatalf("different temperature gave the same key")
	}
	other = testReq("hello")
	other.Model = "other-model"
	if c, _ := RequestKey(other); c == a {
		t.Fatalf("different model gave the same key")
	}
	other = testReq("hello")
	other.Messages = []provider.Message{
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "again"},
	}
	if c, _ := RequestKey(other); c == a {
		t.Fatalf("extra message gave the same key")
	}
	// Reordered messages are a different request.
	other = provider.ChatRequest{Model: "m", Messages: []provider.Message{
		{Role: "user", Content: "second"},
		{Role: "user", Content: "first"},
	}}
	swapped := provider.ChatRequest{Model: "m", Messages: []provider.Message{
		{Role: "user", Content: "first"},
		{Role: "user", Content: "second"},
	}}
	k1, _ := RequestKey(other)
	k2, _ := RequestKey(swapped)
	if k1 == k2 {
		t.Fatalf("reordered messages gave the same key")
	}
}

func TestCoalescable(t *testing.T) {
	if !Coalescable(testReq("hi")) {
		t.Fatalf("plain request should be coalescable")
	}
	withTools := testReq("hi")
	withTools.Tools = []provider.ToolDefinition{{Type: "function"}}
	if Coalescable(withTools) {
		t.Fatalf("tool-using request must not be coalescable")
	}
	withChoice := testReq("hi")
	withChoice.ToolChoice = []byte(`"auto"`)
	if Coalescable(withChoice) {
		t.Fatalf("tool_choice request must not be coalescable")
	}
	toolHistory := provider.ChatRequest{Model: "m", Messages: []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", Content: "result"},
	}}
	if Coalescable(toolHistory) {
		t.Fatalf("tool-role history must not be coalescable")
	}
	toolCalls := provider.ChatRequest{Model: "m", Messages: []provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "1"}}},
	}}
	if Coalescable(toolCalls) {
		t.Fatalf("tool-call history must not be coalescable")
	}
	// Responses creates mint a distinct stateful object per call
	// (CHECKLIST 169): a fresh create and a continuation must
	// never share a flight — and the flight key does not carry
	// the endpoint or the continuation id, so a Responses create
	// could otherwise collide with an identical chat completion.
	respReq := testReq("hi")
	respReq.Endpoint = provider.EndpointResponses
	if Coalescable(respReq) {
		t.Fatalf("Responses create must not be coalescable")
	}
	contReq := testReq("hi")
	contReq.PreviousResponseID = "resp_123"
	if Coalescable(contReq) {
		t.Fatalf("Responses continuation must not be coalescable")
	}
}

func TestGroupSharesOneCall(t *testing.T) {
	g := New()
	key, err := RequestKey(testReq("hello"))
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	var calls int32
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(150 * time.Millisecond)
		return &provider.ChatResponse{Content: "shared answer"}, &routetrace.RouteTrace{}, nil
	}
	const n = 10
	var wg sync.WaitGroup
	results := make([]Result, n)
	shared := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], shared[i] = g.Do(context.Background(), key, fn)
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
	leaders := 0
	for i := 0; i < n; i++ {
		if results[i].Err != nil {
			t.Fatalf("caller %d got error: %v", i, results[i].Err)
		}
		if results[i].Resp == nil || results[i].Resp.Content != "shared answer" {
			t.Fatalf("caller %d got wrong response: %+v", i, results[i].Resp)
		}
		if !shared[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want 1", leaders)
	}
	stats := g.Stats()
	if stats.Requests != n || stats.Flights != 1 || stats.Shared != n-1 {
		t.Fatalf("stats = %+v, want requests=%d flights=1 shared=%d", stats, n, n-1)
	}
	if stats.UpstreamAvoided != n-1 || stats.QuotaSaved != n-1 {
		t.Fatalf("stats = %+v, want upstream_avoided/quota_saved = %d", stats, n-1)
	}
	if stats.InFlight != 0 {
		t.Fatalf("in_flight = %d after all flights done, want 0", stats.InFlight)
	}
}

func TestGroupDifferentKeysDoNotShare(t *testing.T) {
	g := New()
	var calls int32
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond)
		return &provider.ChatResponse{Content: "ok"}, nil, nil
	}
	var wg sync.WaitGroup
	for _, prompt := range []string{"one", "two"} {
		k, err := RequestKey(testReq(prompt))
		if err != nil {
			t.Fatalf("RequestKey: %v", err)
		}
		wg.Add(1)
		go func(k string) {
			defer wg.Done()
			if _, shared := g.Do(context.Background(), k, fn); shared {
				t.Errorf("different key was shared")
			}
		}(k)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	if stats := g.Stats(); stats.Shared != 0 || stats.Flights != 2 {
		t.Fatalf("stats = %+v, want shared=0 flights=2", stats)
	}
}

func TestGroupSharedFailureReachesAllWaiters(t *testing.T) {
	g := New()
	key, _ := RequestKey(testReq("boom"))
	want := errors.New("upstream down")
	var calls int32
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond)
		return nil, nil, want
	}
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _ := g.Do(context.Background(), key, fn)
			errs[i] = res.Err
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, want) {
			t.Fatalf("waiter %d got error %v, want %v", i, err, want)
		}
	}
	if stats := g.Stats(); stats.Shared != n-1 {
		t.Fatalf("stats.shared = %d, want %d (a shared failure still counts as shared)", stats.Shared, n-1)
	}
}

func TestGroupWaiterContextCancelDoesNotDisturbLeader(t *testing.T) {
	g := New()
	key, _ := RequestKey(testReq("slow"))
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		time.Sleep(400 * time.Millisecond)
		return &provider.ChatResponse{Content: "done"}, nil, nil
	}
	leaderDone := make(chan Result, 1)
	go func() {
		res, shared := g.Do(context.Background(), key, fn)
		if shared {
			t.Errorf("leader reported shared")
		}
		leaderDone <- res
	}()
	// Let the leader register first.
	time.Sleep(50 * time.Millisecond)
	wctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	res, shared := g.Do(wctx, key, fn)
	if shared {
		t.Fatalf("cancelled waiter reported shared")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("cancelled waiter got %v, want context deadline", res.Err)
	}
	select {
	case res := <-leaderDone:
		if res.Err != nil || res.Resp.Content != "done" {
			t.Fatalf("leader got %+v, want success", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("leader never finished")
	}
	if stats := g.Stats(); stats.Shared != 0 || stats.Flights != 1 {
		t.Fatalf("stats = %+v, want shared=0 flights=1", stats)
	}
}

func TestGroupLeaderContextCancelFailsWaiters(t *testing.T) {
	g := New()
	key, _ := RequestKey(testReq("cancel"))
	// The leader's fn fails with its own ctx error, as a real
	// upstream call would when the leader's context is cancelled.
	lctx, cancel := context.WithCancel(context.Background())
	leaderFn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		<-lctx.Done()
		return nil, nil, lctx.Err()
	}
	leaderDone := make(chan Result, 1)
	go func() {
		res, _ := g.Do(lctx, key, leaderFn)
		leaderDone <- res
	}()
	time.Sleep(50 * time.Millisecond)
	waiterDone := make(chan Result, 1)
	go func() {
		res, shared := g.Do(context.Background(), key, leaderFn)
		if !shared {
			t.Errorf("waiter did not report shared")
		}
		waiterDone <- res
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-leaderDone:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("leader got %v, want context canceled", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("leader never finished")
	}
	select {
	case res := <-waiterDone:
		if !errors.Is(res.Err, context.Canceled) {
			t.Fatalf("waiter got %v, want the leader's context error", res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("waiter never finished")
	}
}
