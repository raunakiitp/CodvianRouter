// Exported breaker reset on the production engine (B6 follow-up):
// router.Engine must satisfy the server's breakerResetter method
// set (internal/server/resilience_api.go: ResetBreaker(provider)
// bool + ResetAllBreakers() []string) so POST
// /api/resilience/breakers/reset stops answering 501 against the
// production engine. These tests drive real requests through the
// engine (fake clients, no network), never a fake engine.
package router

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// breakerResetterMethodSet mirrors the server's breakerResetter
// interface (internal/server/resilience_api.go) without importing
// server — router cannot import server — so the compile-time
// assertion below proves Engine satisfies that method set.
type breakerResetterMethodSet interface {
	ResetBreaker(provider string) bool
	ResetAllBreakers() []string
}

var _ breakerResetterMethodSet = (*Engine)(nil)

// TestResetBreakerReopensTrippedBreaker: five 500s trip provider
// A's breaker (testConfig threshold 5); ResetBreaker returns true
// and the breaker reads CLOSED with 0 failures and 0 trips; the
// very next request is attempted against A again (not skipped as
// open) and serves once the upstream recovers.
func TestResetBreakerReopensTrippedBreaker(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	flaky := errClient(fail500)
	eng := NewEngine(map[string]provider.Client{"a": flaky}, nil, testConfig())
	chain := chainFor("a")
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
			t.Fatalf("request %d: want the 500, got nil", i+1)
		}
	}
	if st := breakerStatesByID(eng)["a"]; !st.Open || st.ConsecutiveFailures != 5 {
		t.Fatalf("before reset: %+v, want OPEN with 5 consecutive failures", st)
	}
	if calls := flaky.chatCalls(); calls != 5 {
		t.Fatalf("provider called %d times before reset, want 5", calls)
	}
	if !eng.ResetBreaker("a") {
		t.Fatalf("ResetBreaker(\"a\") = false, want true (breaker is tripped)")
	}
	if st := breakerStatesByID(eng)["a"]; st.Open || st.State != BreakerClosed || st.ConsecutiveFailures != 0 || st.Trips != 0 {
		t.Fatalf("after reset: %+v, want CLOSED, 0 failures, 0 trips", st)
	}
	// The upstream recovers: the next request must be attempted
	// against A at once — no skipped_breaker, no waiting out the
	// 1-minute open window.
	flaky.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "recovered"}, nil
	}
	resp, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil {
		t.Fatalf("request after reset: %v (the provider must be tried again at once)", err)
	}
	if resp.Content != "recovered" {
		t.Fatalf("content = %q, want the recovered provider's answer", resp.Content)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace = %+v, want exactly one successful attempt (no skipped_breaker)", trace.Attempts)
	}
	if calls := flaky.chatCalls(); calls != 6 {
		t.Fatalf("provider called %d times after reset, want 6 (the post-reset request reached it)", calls)
	}
}

// TestResetBreakerUnknownProvider: resetting a provider the engine
// holds no breaker state for returns false — an unknown id, and a
// registered provider that never recorded a breaker-counted
// failure (no state exists for it yet).
func TestResetBreakerUnknownProvider(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, testConfig())
	if eng.ResetBreaker("no-such-provider") {
		t.Fatalf("ResetBreaker(\"no-such-provider\") = true, want false (unknown provider)")
	}
	if eng.ResetBreaker("a") {
		t.Fatalf("ResetBreaker(\"a\") = true, want false (registered but never failed: no breaker state)")
	}
	if got := eng.ResetAllBreakers(); len(got) != 0 {
		t.Fatalf("ResetAllBreakers() = %v, want empty (nothing tripped)", got)
	}
}

// TestResetBreakerDegradedState: reset also works on a breaker
// that has counted failures but has not tripped yet — OAuth
// degrades at 5 failures, opens at 8 — and reports true.
func TestResetBreakerDegradedState(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileOAuth},
		map[string]provider.Client{"a": errClient(fail500), "b": okClient("bee")})
	chain := chainFor("a", "b")
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerDegraded || st.Open {
		t.Fatalf("before reset: %+v, want DEGRADED, not open", st)
	}
	if !eng.ResetBreaker("a") {
		t.Fatalf("ResetBreaker(\"a\") = false, want true (degraded breaker holds state)")
	}
	if st := breakerStatesByID(eng)["a"]; st.State != BreakerClosed || st.ConsecutiveFailures != 0 {
		t.Fatalf("after reset: %+v, want CLOSED with 0 failures", st)
	}
}

// TestResetBreakerClearsWindowBreaker: the failure-window breaker
// trips on flapping (local profile: 2 failures in 5 min) even with
// only 1 consecutive failure; ResetBreaker must clear the window
// record too, or the provider would stay skipped. After the reset
// the next request is attempted against the provider again.
func TestResetBreakerClearsWindowBreaker(t *testing.T) {
	ctx := context.Background()
	var calls int
	flap := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls%2 == 1 {
			return nil, &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	eng := profileEngine(map[string]AuthProfile{"a": AuthProfileLocal},
		map[string]provider.Client{"a": flap})
	chain := chainFor("a")
	// Req 1 fails, req 2 succeeds, req 3 fails: window fills (2
	// failures in 5 min) and the window breaker opens.
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("req 1: want the 500, got nil")
	}
	if resp, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err != nil || resp.Content != "ok" {
		t.Fatalf("req 2 = %v, %v, want ok + nil", resp, err)
	}
	if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
		t.Fatalf("req 3: want the 500, got nil")
	}
	st := breakerStatesByID(eng)["a"]
	if !st.WindowOpen || !st.Open || st.State != BreakerOpen {
		t.Fatalf("before reset: %+v, want WindowOpen + Open + state OPEN", st)
	}
	if st.WindowFailures != 2 {
		t.Fatalf("window failures = %d, want 2", st.WindowFailures)
	}
	if !eng.ResetBreaker("a") {
		t.Fatalf("ResetBreaker(\"a\") = false, want true (window breaker is open)")
	}
	if st := breakerStatesByID(eng)["a"]; st.Open || st.WindowOpen || st.State != BreakerClosed || st.ConsecutiveFailures != 0 || st.WindowFailures != 0 {
		t.Fatalf("after reset: %+v, want CLOSED, window closed, 0 consecutive + 0 window failures", st)
	}
	// Req 4 (4th upstream call, the success leg of the flap) is
	// attempted again — the window no longer skips the provider.
	resp, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil || resp.Content != "ok" {
		t.Fatalf("req 4 after reset = %v, %v, want ok + nil", resp, err)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace = %+v, want exactly one successful attempt (no skipped_breaker)", trace.Attempts)
	}
	if calls != 4 {
		t.Fatalf("provider called %d times, want 4 (req 4 reached it)", calls)
	}
}

// TestResetAllBreakers: with A and B both tripped (5x500 each)
// and C never failed, ResetAllBreakers returns exactly [a b]
// sorted, both read CLOSED with 0 failures afterwards, C is
// omitted, a second call returns empty, and both providers route
// again once recovered.
func TestResetAllBreakers(t *testing.T) {
	ctx := context.Background()
	fail500 := &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}
	fa := errClient(fail500)
	fb := errClient(fail500)
	eng := NewEngine(map[string]provider.Client{
		"a": fa, "b": fb, "c": okClient("cee"),
	}, nil, testConfig())
	chain := chainFor("a", "b")
	for i := 0; i < 5; i++ {
		if _, _, err := eng.ChatWithChain(ctx, chain, coolReq("m")); err == nil {
			t.Fatalf("request %d: want an error, got nil", i+1)
		}
	}
	for _, id := range []string{"a", "b"} {
		if st := breakerStatesByID(eng)[id]; !st.Open {
			t.Fatalf("before reset: %s = %+v, want OPEN", id, st)
		}
	}
	if got := fa.chatCalls(); got != 5 {
		t.Fatalf("provider a called %d times, want 5", got)
	}
	if got := fb.chatCalls(); got != 5 {
		t.Fatalf("provider b called %d times, want 5", got)
	}
	reset := eng.ResetAllBreakers()
	if len(reset) != 2 || reset[0] != "a" || reset[1] != "b" {
		t.Fatalf("ResetAllBreakers() = %v, want [a b] (c never failed, so it is omitted)", reset)
	}
	for _, id := range []string{"a", "b"} {
		if st := breakerStatesByID(eng)[id]; st.Open || st.State != BreakerClosed || st.ConsecutiveFailures != 0 {
			t.Fatalf("after reset: %s = %+v, want CLOSED with 0 failures", id, st)
		}
	}
	if got := eng.ResetAllBreakers(); len(got) != 0 {
		t.Fatalf("second ResetAllBreakers() = %v, want empty (nothing left tripped)", got)
	}
	// Both providers route again once recovered.
	fa.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "aye"}, nil
	}
	fb.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "bee"}, nil
	}
	resp, trace, err := eng.ChatWithChain(ctx, chain, coolReq("m"))
	if err != nil || resp.Content != "aye" {
		t.Fatalf("request after reset-all = %v, %v, want aye + nil", resp, err)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace = %+v, want exactly one successful attempt on a", trace.Attempts)
	}
}

// TestResetBreakerPersistsAcrossReload (review follow-up, Part A):
// reset -> persist -> reload round-trip. Trip provider a (testConfig
// threshold 5), ResetBreaker, reload from the same store file: the
// breaker must stay reset (CLOSED, 0 failures, 0 trips), not
// resurrected from a stale on-disk snapshot. Then a failure recorded
// after the reset must itself persist: reload again and it reads
// back as 1 consecutive failure.
func TestResetBreakerPersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breakers.json")
	cfg := testConfig()
	cfg.BreakerStorePath = path
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	for i := 0; i < 5; i++ {
		eng.breakerFailure("a", "boom")
	}
	if st := breakerStatesByID(eng)["a"]; !st.Open || st.ConsecutiveFailures != 5 || st.Trips != 1 {
		t.Fatalf("before reset: %+v, want OPEN with 5 failures, 1 trip", st)
	}
	if !eng.ResetBreaker("a") {
		t.Fatalf("ResetBreaker(\"a\") = false, want true (breaker is tripped)")
	}
	// "Restart": a fresh engine on the same store path.
	eng2 := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	if st := breakerStatesByID(eng2)["a"]; st.Open || st.State != BreakerClosed || st.ConsecutiveFailures != 0 || st.Trips != 0 {
		t.Fatalf("after reset + reload: %+v, want CLOSED, 0 failures, 0 trips (reset breaker resurrected from disk)", st)
	}
	// A failure after the reset persists too.
	eng2.breakerFailure("a", "post-reset")
	eng3 := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	if st := breakerStatesByID(eng3)["a"]; st.Open || st.State != BreakerClosed || st.ConsecutiveFailures != 1 || st.Trips != 0 {
		t.Fatalf("after failure-after-reset + reload: %+v, want CLOSED with 1 failure, 0 trips", st)
	}
}

// TestBreakerConcurrentResetAndFailurePersistsLatest (review
// follow-up, Part A): the lost-update race in saveBreakerStore.
// Two saves racing — a reset's save and a failure's save — must
// leave the file matching the final live state, never a stale
// snapshot renamed over a newer one. Each round re-establishes
// breaker state, races ResetBreaker against breakerFailure, then
// reloads from disk and compares the persisted consecutive-failure
// record against the live one (presence in e.breakers plus the
// count — the exact record the store persists; the window breaker
// is deliberately not persisted, so it is not compared). A stale
// rename shows up as a mismatch in either direction: a reset
// breaker resurrected on disk, or a real post-reset failure
// dropped from disk. On the unfixed helper this fails
// intermittently (the interleaving is timing-dependent); with
// serialized saves it cannot fail.
func TestBreakerConcurrentResetAndFailurePersistsLatest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "breakers.json")
	cfg := testConfig()
	cfg.BreakerStorePath = path
	eng := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
	liveRecord := func(e *Engine) (present bool, failures int) {
		e.mu.Lock()
		defer e.mu.Unlock()
		st, ok := e.breakers["a"]
		if !ok || st == nil {
			return false, 0
		}
		return true, st.consecutiveFailures
	}
	const rounds = 100
	for round := 0; round < rounds; round++ {
		for i := 0; i < 3; i++ {
			eng.breakerFailure("a", "boom")
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			eng.ResetBreaker("a")
		}()
		go func() {
			defer wg.Done()
			eng.breakerFailure("a", "post-reset")
		}()
		wg.Wait()
		livePresent, liveFailures := liveRecord(eng)
		reloaded := NewEngine(map[string]provider.Client{"a": okClient("aye")}, nil, cfg)
		diskPresent, diskFailures := liveRecord(reloaded)
		if livePresent != diskPresent || liveFailures != diskFailures {
			t.Fatalf("round %d: live breaker present=%v failures=%d, but reloaded file has present=%v failures=%d — a stale save renamed over a newer one",
				round, livePresent, liveFailures, diskPresent, diskFailures)
		}
		// Clear the round's leftover state so every round
		// starts from the same known state.
		eng.ResetBreaker("a")
	}
}
