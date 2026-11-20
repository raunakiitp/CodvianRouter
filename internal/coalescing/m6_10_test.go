package coalescing

// M6.10 — dedup/coalescing gap closure: per-key (tenant) isolation,
// in-flight cap (1,000), per-flight timeout (60 s). The timeout and
// cap are overridden per-group in these tests (g.timeout /
// g.maxInFlight, zero = default) so the 60 s / 1,000 defaults never
// have to be waited out or spawned.

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

func TestRequestKeyForTenantIsolatesTenants(t *testing.T) {
	req := testReq("hello")
	base, err := RequestKey(req)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	// Empty tenant is exactly RequestKey: existing callers that
	// cannot supply a tenant keep today's behaviour bit-for-bit.
	empty, err := RequestKeyForTenant(req, "")
	if err != nil {
		t.Fatalf("RequestKeyForTenant empty: %v", err)
	}
	if empty != base {
		t.Fatalf("empty tenant key %q != base key %q", empty, base)
	}
	ka, err := RequestKeyForTenant(req, "gwkey-aaa")
	if err != nil {
		t.Fatalf("RequestKeyForTenant a: %v", err)
	}
	kb, err := RequestKeyForTenant(req, "gwkey-bbb")
	if err != nil {
		t.Fatalf("RequestKeyForTenant b: %v", err)
	}
	if ka == base {
		t.Fatalf("tenant key equals the unscoped key — tenant was not prefixed")
	}
	if kb == base {
		t.Fatalf("tenant key equals the unscoped key — tenant was not prefixed")
	}
	if ka == kb {
		t.Fatalf("different tenants gave the same key %q — they would share a flight", ka)
	}
	// Same tenant + same request still coalesces.
	ka2, err := RequestKeyForTenant(req, "gwkey-aaa")
	if err != nil {
		t.Fatalf("RequestKeyForTenant a again: %v", err)
	}
	if ka2 != ka {
		t.Fatalf("same tenant gave different keys %q vs %q", ka, ka2)
	}
	// A different request under the same tenant is still a
	// different key.
	other, err := RequestKeyForTenant(testReq("goodbye"), "gwkey-aaa")
	if err != nil {
		t.Fatalf("RequestKeyForTenant other: %v", err)
	}
	if other == ka {
		t.Fatalf("different prompt under the same tenant gave the same key")
	}
	// Unkeyable requests still fail rather than hashing garbage.
	bad := testReq("hello")
	bad.Temperature = math.NaN()
	if _, err := RequestKeyForTenant(bad, "gwkey-aaa"); err == nil {
		t.Fatalf("NaN temperature should fail key derivation")
	}
}

func TestGroupSameTenantSharesFlight(t *testing.T) {
	g := New()
	key, err := RequestKeyForTenant(testReq("hello"), "gwkey-aaa")
	if err != nil {
		t.Fatalf("RequestKeyForTenant: %v", err)
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
		if !shared[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want 1", leaders)
	}
	if stats := g.Stats(); stats.Shared != n-1 || stats.CapBypassed != 0 {
		t.Fatalf("stats = %+v, want shared=%d cap_bypassed=0", stats, n-1)
	}
}

func TestGroupDifferentTenantsNeverShare(t *testing.T) {
	g := New()
	ka, err := RequestKeyForTenant(testReq("hello"), "gwkey-aaa")
	if err != nil {
		t.Fatalf("RequestKeyForTenant a: %v", err)
	}
	kb, err := RequestKeyForTenant(testReq("hello"), "gwkey-bbb")
	if err != nil {
		t.Fatalf("RequestKeyForTenant b: %v", err)
	}
	if ka == kb {
		t.Fatalf("tenant keys collide — test setup is broken")
	}
	var calls int32
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond)
		return &provider.ChatResponse{Content: "ok"}, nil, nil
	}
	var wg sync.WaitGroup
	sharedFlags := make([]bool, 2)
	for i, k := range []string{ka, kb} {
		wg.Add(1)
		go func(i int, k string) {
			defer wg.Done()
			if _, shared := g.Do(context.Background(), k, fn); shared {
				sharedFlags[i] = true
			}
		}(i, k)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one leader per tenant)", got)
	}
	for i, s := range sharedFlags {
		if s {
			t.Fatalf("tenant %d shared a flight with the other tenant", i)
		}
	}
	if stats := g.Stats(); stats.Shared != 0 || stats.Flights != 2 || stats.CapBypassed != 0 {
		t.Fatalf("stats = %+v, want shared=0 flights=2 cap_bypassed=0", stats)
	}
}

func TestGroupCapBypassRunsUncoalescedAndCounted(t *testing.T) {
	g := New()
	g.maxInFlight = 2 // override the 1,000 default for the test
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	started := make(chan struct{}, 2)
	leaderFn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		started <- struct{}{}
		<-release
		return &provider.ChatResponse{Content: "leader"}, nil, nil
	}
	keys := make([]string, 3)
	for i, prompt := range []string{"one", "two", "three"} {
		k, err := RequestKey(testReq(prompt))
		if err != nil {
			t.Fatalf("RequestKey: %v", err)
		}
		keys[i] = k
	}
	// Occupy both flight slots with blocking leaders.
	leaderDone := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func(k string) {
			_, _ = g.Do(context.Background(), k, leaderFn)
			leaderDone <- struct{}{}
		}(keys[i])
	}
	// Wait until both leaders are registered and inside fn.
	deadline := time.Now().Add(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Until(deadline)):
			t.Fatalf("leaders did not start in time")
		}
	}
	for time.Now().Before(deadline) && g.Stats().InFlight != 2 {
		time.Sleep(time.Millisecond)
	}
	if got := g.Stats().InFlight; got != 2 {
		t.Fatalf("in_flight = %d, want 2 before the bypass call", got)
	}

	// Third distinct key, cap reached: must NOT block on the
	// leaders and must NOT join a queue — it runs its own fn now.
	var bypassCalls int32
	bypassFn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		atomic.AddInt32(&bypassCalls, 1)
		return &provider.ChatResponse{Content: "bypass"}, nil, nil
	}
	done := make(chan Result, 1)
	go func() {
		res, shared := g.Do(context.Background(), keys[2], bypassFn)
		if shared {
			t.Errorf("bypassed call reported shared")
		}
		done <- res
	}()
	select {
	case res := <-done:
		if res.Err != nil {
			t.Fatalf("bypassed call got error: %v", res.Err)
		}
		if res.Resp == nil || res.Resp.Content != "bypass" {
			t.Fatalf("bypassed call got %+v, want its own flight's response", res.Resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("bypassed call blocked instead of running uncoalesced")
	}
	if got := atomic.LoadInt32(&bypassCalls); got != 1 {
		t.Fatalf("bypass fn calls = %d, want exactly 1 (its own flight)", got)
	}
	stats := g.Stats()
	if stats.CapBypassed != 1 {
		t.Fatalf("stats = %+v, want cap_bypassed=1", stats)
	}
	if stats.Flights != 2 {
		t.Fatalf("stats = %+v, want flights=2 (bypass is not a registered flight)", stats)
	}
	if stats.Shared != 0 {
		t.Fatalf("stats = %+v, want shared=0 (bypass must not be folded into Shared)", stats)
	}
	if stats.Requests != 3 {
		t.Fatalf("stats = %+v, want requests=3 (bypass still entered the group)", stats)
	}

	// Release the leaders; a later request registers normally and
	// the bypass count does not move.
	releaseAll()
	for i := 0; i < 2; i++ {
		select {
		case <-leaderDone:
		case <-time.After(2 * time.Second):
			t.Fatalf("leader %d never finished", i)
		}
	}
	if stats := g.Stats(); stats.InFlight != 0 || stats.CapBypassed != 1 {
		t.Fatalf("stats after release = %+v, want in_flight=0 cap_bypassed=1", stats)
	}
	k4, _ := RequestKey(testReq("four"))
	if _, shared := g.Do(context.Background(), k4, bypassFn); shared {
		t.Fatalf("fresh call after release reported shared")
	}
	if stats := g.Stats(); stats.Flights != 3 || stats.CapBypassed != 1 {
		t.Fatalf("stats = %+v, want flights=3 cap_bypassed=1", stats)
	}
}

func TestGroupFlightTimeoutReleasesWaitersAndCleansUp(t *testing.T) {
	g := New()
	g.timeout = 100 * time.Millisecond // override the 60 s default
	key, _ := RequestKey(testReq("slow"))
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()

	var calls int32
	fn := func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// First flight blocks forever (until the test
			// releases it in cleanup): a legacy fn that
			// ignores contexts, so only the flight timeout
			// can release it.
			<-release
			return &provider.ChatResponse{Content: "too late"}, nil, nil
		}
		return &provider.ChatResponse{Content: "fresh"}, nil, nil
	}

	leaderDone := make(chan Result, 1)
	var leaderShared bool
	go func() {
		res, shared := g.Do(context.Background(), key, fn)
		leaderShared = shared
		leaderDone <- res
	}()
	// Let the leader register first.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && g.Stats().InFlight != 1 {
		time.Sleep(time.Millisecond)
	}
	if got := g.Stats().InFlight; got != 1 {
		t.Fatalf("in_flight = %d, want 1 before the waiter joins", got)
	}
	waiterDone := make(chan Result, 1)
	var waiterShared bool
	go func() {
		res, shared := g.Do(context.Background(), key, fn)
		waiterShared = shared
		waiterDone <- res
	}()

	// Both must be released by the timeout (~100 ms), not by the
	// release channel, which stays closed until test cleanup.
	for name, ch := range map[string]chan Result{"leader": leaderDone, "waiter": waiterDone} {
		select {
		case res := <-ch:
			if !errors.Is(res.Err, context.DeadlineExceeded) {
				t.Fatalf("%s got error %v, want context.DeadlineExceeded", name, res.Err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was not released by the flight timeout", name)
		}
	}
	if leaderShared {
		t.Fatalf("timed-out leader reported shared")
	}
	if !waiterShared {
		t.Fatalf("waiter did not report shared — it shared the timed-out flight")
	}
	if stats := g.Stats(); stats.InFlight != 0 {
		t.Fatalf("stats = %+v, want in_flight=0 after timeout cleanup", stats)
	}
	if stats := g.Stats(); stats.Flights != 1 || stats.Shared != 1 {
		t.Fatalf("stats = %+v, want flights=1 shared=1 (waiter shared the failed flight)", stats)
	}

	// A later identical request starts a fresh flight and succeeds.
	res, shared := g.Do(context.Background(), key, fn)
	if shared {
		t.Fatalf("fresh flight reported shared")
	}
	if res.Err != nil {
		t.Fatalf("fresh flight got error: %v", res.Err)
	}
	if res.Resp == nil || res.Resp.Content != "fresh" {
		t.Fatalf("fresh flight got %+v, want the new flight's response", res.Resp)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (timed-out leader + fresh flight)", got)
	}
	if stats := g.Stats(); stats.Flights != 2 || stats.InFlight != 0 {
		t.Fatalf("stats = %+v, want flights=2 in_flight=0", stats)
	}
	releaseAll()
}

func TestGroupFlightTimeoutCancelsLeaderContext(t *testing.T) {
	// DoCtx hands the timeout context to fn: a fn that respects
	// its context must observe the cancellation itself, so the
	// upstream call is actually aborted — not merely abandoned.
	g := New()
	g.timeout = 100 * time.Millisecond
	key, _ := RequestKey(testReq("ctx"))
	var cancelled int32
	fn := func(ctx context.Context) (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		select {
		case <-ctx.Done():
			atomic.StoreInt32(&cancelled, 1)
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Second):
			return &provider.ChatResponse{Content: "never"}, nil, nil
		}
	}
	res, shared := g.DoCtx(context.Background(), key, fn)
	if shared {
		t.Fatalf("leader reported shared")
	}
	if !errors.Is(res.Err, context.DeadlineExceeded) {
		t.Fatalf("leader got error %v, want context.DeadlineExceeded", res.Err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&cancelled) == 0 {
		time.Sleep(time.Millisecond)
	}
	if atomic.LoadInt32(&cancelled) == 0 {
		t.Fatalf("fn never observed its context being cancelled by the flight timeout")
	}
	if stats := g.Stats(); stats.InFlight != 0 {
		t.Fatalf("stats = %+v, want in_flight=0 after timeout cleanup", stats)
	}
}

// TestGroupLeaderPanicSharedAsErrorAndRepanicsInLeader (coordinator
// merge addition, M6.10 review nit 1): a panicking leader fn must
// not hang its waiters — they get a flight error — and the leader's
// Do must still re-panic with the original value after the flight
// entry is cleaned up, preserving the pre-M6.10 synchronous-panic
// behaviour for the leader's caller.
func TestGroupLeaderPanicSharedAsErrorAndRepanicsInLeader(t *testing.T) {
	g := New()
	key, _ := RequestKey(testReq("panic"))
	// The leader's fn waits on joined until the waiter has had a
	// chance to find the registered flight — otherwise the panic
	// fires before any waiter exists and there is nothing to share
	// the error with.
	joined := make(chan struct{})
	leaderDone := make(chan any, 1)
	go func() {
		defer func() { leaderDone <- recover() }()
		_, _ = g.Do(context.Background(), key, func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
			<-joined
			panic("boom")
		})
	}()
	// Wait until the leader's flight is registered before joining
	// as a waiter — otherwise this caller would become the leader.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && g.Stats().InFlight == 0 {
		time.Sleep(time.Millisecond)
	}
	if g.Stats().InFlight == 0 {
		t.Fatal("leader flight never registered")
	}
	type waiterOut struct {
		res    Result
		shared bool
	}
	waiterDone := make(chan waiterOut, 1)
	go func() {
		res, shared := g.Do(context.Background(), key, func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
			return &provider.ChatResponse{Content: "waiter-own"}, nil, nil
		})
		waiterDone <- waiterOut{res, shared}
	}()
	// Let the waiter find the flight (a map lookup), then release
	// the leader into its panic.
	time.Sleep(50 * time.Millisecond)
	close(joined)
	select {
	case w := <-waiterDone:
		if !w.shared {
			t.Fatalf("waiter did not share the leader flight")
		}
		if w.res.Err == nil || !strings.Contains(w.res.Err.Error(), "panicked") {
			t.Fatalf("waiter got error %v, want a flight-panicked error", w.res.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never returned after the leader panicked")
	}
	select {
	case panicked := <-leaderDone:
		if panicked != "boom" {
			t.Fatalf("leader re-panicked with %v, want %q", panicked, "boom")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader Do never returned (no re-panic after cleanup)")
	}
	if stats := g.Stats(); stats.InFlight != 0 {
		t.Fatalf("stats = %+v, want in_flight=0 after panic cleanup", stats)
	}
	// A later identical request starts a fresh flight.
	res, shared := g.Do(context.Background(), key, func() (*provider.ChatResponse, *routetrace.RouteTrace, error) {
		return &provider.ChatResponse{Content: "ok"}, nil, nil
	})
	if shared || res.Err != nil || res.Resp == nil || res.Resp.Content != "ok" {
		t.Fatalf("fresh flight after panic: res=%+v shared=%v, want ok + leader", res, shared)
	}
}
