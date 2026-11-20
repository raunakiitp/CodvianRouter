package cache

import (
	"context"
	"testing"
	"time"
)

// touch (CHECKLIST 249-250) must move a key to the front of the
// recency order in place: middle key, already-front key, and a
// brand-new key (Set path) must all leave the order exactly as
// the old allocating implementation did — the LRU eviction test
// pins the behaviour end to end, this pins the order itself.
func TestExactCacheTouchOrder(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	resp := CachedResponse{Content: "x"}
	for _, k := range []string{"a", "b", "c"} {
		c.Set(ctx, k, resp, time.Minute)
	}
	order := func() []string {
		c.mu.Lock()
		defer c.mu.Unlock()
		out := make([]string, len(c.order))
		copy(out, c.order)
		return out
	}
	want := func(got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("order = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("order = %v, want %v", got, want)
			}
		}
	}
	// Sets touch in turn: last set is front.
	want(order(), []string{"c", "b", "a"})
	// Touch the middle key: front, rest keep relative order.
	c.mu.Lock()
	c.touch("b")
	c.mu.Unlock()
	want(order(), []string{"b", "c", "a"})
	// Touch the tail key.
	c.mu.Lock()
	c.touch("a")
	c.mu.Unlock()
	want(order(), []string{"a", "b", "c"})
	// Touch the front key: no change.
	c.mu.Lock()
	c.touch("a")
	c.mu.Unlock()
	want(order(), []string{"a", "b", "c"})
	// Touch a new key (the Set path): front, nothing dropped.
	c.mu.Lock()
	c.touch("new")
	c.mu.Unlock()
	want(order(), []string{"new", "a", "b", "c"})
}

// The point of the in-place touch is zero allocation: the old
// append([]string{key}, c.order...) allocated a fresh slice on
// every cache Get-hit and Set (~71 MB churn per ~24k requests in
// the memstats slice). A touch of an existing key must allocate
// nothing.
func TestExactCacheTouchZeroAlloc(t *testing.T) {
	ctx := context.Background()
	c := NewExactCache(10)
	resp := CachedResponse{Content: "x"}
	for _, k := range []string{"a", "b", "c", "d"} {
		c.Set(ctx, k, resp, time.Minute)
	}
	allocs := testing.AllocsPerRun(100, func() {
		c.mu.Lock()
		c.touch("b")
		c.mu.Unlock()
	})
	if allocs != 0 {
		t.Errorf("touch(existing key) = %v allocs/run, want 0", allocs)
	}
	// A Get-hit (the hot path) goes through touch under the lock;
	// it allocates only the time.Now call's escape, not a slice —
	// pin the total low rather than exactly zero.
	allocs = testing.AllocsPerRun(100, func() {
		c.Get(ctx, "c")
	})
	if allocs > 1 {
		t.Errorf("Get(hit) = %v allocs/run, want <= 1 (no per-hit slice)", allocs)
	}
}
