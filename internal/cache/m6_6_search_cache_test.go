package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// M6.6 — search result cache (parity tracker Area 6, "Search cache
// (60 s)", the reference router searchCache.ts:12,150). Every behaviour here
// has a test that fails without the implementation; toggle evidence
// is recorded in the M6.6 report (expiry check removed /
// coalescing join removed → these tests go red).

func TestM66SearchDefaultsMatchReference(t *testing.T) {
	if DefaultSearchTTL != 60*time.Second {
		t.Fatalf("DefaultSearchTTL = %v, want 60s (SEARCH_CACHE_DEFAULT_TTL_MS)", DefaultSearchTTL)
	}
	if DefaultSearchMaxEntries != 500 {
		t.Fatalf("DefaultSearchMaxEntries = %d, want 500 (MAX_CACHE_ENTRIES)", DefaultSearchMaxEntries)
	}
	if DefaultSearchMaxEntryBytes != 256<<10 {
		t.Fatalf("DefaultSearchMaxEntryBytes = %d, want 256 KiB (M6.1)", DefaultSearchMaxEntryBytes)
	}
	if DefaultSearchMaxTotalBytes != 2<<20 {
		t.Fatalf("DefaultSearchMaxTotalBytes = %d, want 2 MiB (M6.1)", DefaultSearchMaxTotalBytes)
	}
}

func TestM66SearchKeyNormalization(t *testing.T) {
	k1 := SearchKey("  Hello   WORLD  ", "brave", "web", 10, "us", "en", nil)
	k2 := SearchKey("hello world", "brave", "web", 10, "us", "en", nil)
	if k1 == "" || k1 != k2 {
		t.Fatalf("normalized keys differ: %q vs %q (lowercase/trim/collapse)", k1, k2)
	}
	for name, k := range map[string]string{
		"different query":    SearchKey("hello mars", "brave", "web", 10, "us", "en", nil),
		"different provider": SearchKey("hello world", "tavily", "web", 10, "us", "en", nil),
		"different type":     SearchKey("hello world", "brave", "news", 10, "us", "en", nil),
		"different count":    SearchKey("hello world", "brave", "web", 5, "us", "en", nil),
		"different country":  SearchKey("hello world", "brave", "web", 10, "in", "en", nil),
		"different language": SearchKey("hello world", "brave", "web", 10, "us", "fr", nil),
		"different filters":  SearchKey("hello world", "brave", "web", 10, "us", "en", map[string]any{"fresh": true}),
	} {
		if k == k1 {
			t.Fatalf("key collides for %s: %q", name, k)
		}
	}
	if got := SearchKey("hello world", "brave", "web", 10, "", "", nil); got == k1 {
		t.Fatalf("empty country/language must key differently from us/en")
	}
}

func TestM66SearchTTLExpiry(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	c.SetWithTTL(ctx, key, "result", 50*time.Millisecond)
	if got, ok := c.Get(ctx, key); !ok || got != "result" {
		t.Fatalf("Get before expiry = %q, %v, want hit", got, ok)
	}
	time.Sleep(80 * time.Millisecond)
	if got, ok := c.Get(ctx, key); ok {
		t.Fatalf("Get after 60s-TTL expiry = %q, true, want miss", got)
	}
	if c.Size() != 0 {
		t.Fatalf("Size after expired access = %d, want 0", c.Size())
	}
}

func TestM66SearchPerEntryCapRefuses(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCacheWithConfig[string](SearchCacheConfig{MaxEntryBytes: 10})
	key := SearchKey("q", "p", "web", 1, "", "", nil)
	c.Set(ctx, key, strings.Repeat("x", 20))
	if c.Size() != 0 {
		t.Fatalf("oversize search result stored, want refused (M6.1 per-entry cap)")
	}
	c.Set(ctx, key, "short")
	if c.Size() != 1 {
		t.Fatalf("at-cap search result not stored, want stored")
	}
}

func TestM66SearchFIFOEviction(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCacheWithConfig[string](SearchCacheConfig{MaxEntries: 2, MaxTotalBytes: 1 << 20})
	k1 := SearchKey("one", "p", "web", 1, "", "", nil)
	k2 := SearchKey("two", "p", "web", 1, "", "", nil)
	k3 := SearchKey("three", "p", "web", 1, "", "", nil)
	c.Set(ctx, k1, "1")
	c.Set(ctx, k2, "2")
	c.Set(ctx, k3, "3")
	if _, ok := c.Get(ctx, k1); ok {
		t.Fatalf("oldest entry survived a full cache (want FIFO eviction, searchCache.ts evictIfNeeded)")
	}
	if _, ok := c.Get(ctx, k2); !ok {
		t.Fatalf("second entry evicted, want kept")
	}
	if st := c.Stats(); st.Evicted != 1 {
		t.Fatalf("Evicted = %d, want 1 (real counter)", st.Evicted)
	}
}

func TestM66SearchCoalescingSharesOneFetch(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	var fetches atomic.Int32
	fetch := func(ctx context.Context) (string, error) {
		fetches.Add(1)
		time.Sleep(100 * time.Millisecond)
		return "shared-result", nil
	}
	const callers = 8
	var wg sync.WaitGroup
	results := make([]string, callers)
	cached := make([]bool, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], cached[i], errs[i] = c.GetOrCoalesce(ctx, key, DefaultSearchTTL, fetch)
		}(i)
	}
	wg.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("fetch ran %d times for %d concurrent identical searches, want 1 (coalescing)", fetches.Load(), callers)
	}
	leaders := 0
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("caller %d error: %v", i, errs[i])
		}
		if results[i] != "shared-result" {
			t.Fatalf("caller %d got %q, want shared-result", i, results[i])
		}
		if !cached[i] {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("leaders = %d, want exactly 1 (the rest are waiters, cached=true)", leaders)
	}
	// The stored result now serves from cache without fetching.
	fetches.Store(0)
	v, wasCached, err := c.GetOrCoalesce(ctx, key, DefaultSearchTTL, func(context.Context) (string, error) {
		fetches.Add(1)
		return "fresh", nil
	})
	if err != nil || v != "shared-result" || !wasCached || fetches.Load() != 0 {
		t.Fatalf("second GetOrCoalesce = %q, cached=%v, err=%v, fetches=%d, want cached shared-result, no fetch", v, wasCached, err, fetches.Load())
	}
	if st := c.Stats(); st.Hits < 7 {
		t.Fatalf("Hits = %d, want >= 7 (waiters count as hits, searchCache.ts)", st.Hits)
	}
}

func TestM66SearchTTLZeroBypassesCacheAndCoalescing(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	var fetches atomic.Int32
	fetch := func(ctx context.Context) (string, error) {
		fetches.Add(1)
		time.Sleep(50 * time.Millisecond)
		return "result", nil
	}
	const callers = 4
	var wg sync.WaitGroup
	cached := make([]bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, cached[i], _ = c.GetOrCoalesce(ctx, key, 0, fetch)
		}(i)
	}
	wg.Wait()
	// ttlMs=0 in the reference router: every concurrent call gets its own
	// independent fetch — no coalescing, no storing.
	if fetches.Load() != callers {
		t.Fatalf("ttl=0 fetch ran %d times for %d callers, want %d (bypass means no coalescing)", fetches.Load(), callers, callers)
	}
	for i, wasCached := range cached {
		if wasCached {
			t.Fatalf("caller %d got cached=true with ttl=0, want false", i)
		}
	}
	if c.Size() != 0 {
		t.Fatalf("Size = %d after ttl=0 fetches, want 0 (nothing stored)", c.Size())
	}
}

func TestM66SearchErrorNeverCached(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	boom := errors.New("upstream search failed")
	_, _, err := c.GetOrCoalesce(ctx, key, DefaultSearchTTL, func(context.Context) (string, error) {
		return "", boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("leader error = %v, want %v", err, boom)
	}
	if c.Size() != 0 {
		t.Fatalf("Size = %d after failed fetch, want 0 (failures are never cached)", c.Size())
	}
	// A later identical search retries upstream instead of
	// serving the failure.
	v, wasCached, err := c.GetOrCoalesce(ctx, key, DefaultSearchTTL, func(context.Context) (string, error) {
		return "ok", nil
	})
	if err != nil || v != "ok" || wasCached {
		t.Fatalf("retry = %q, cached=%v, err=%v, want fresh ok", v, wasCached, err)
	}
	if c.Size() != 1 {
		t.Fatalf("Size = %d after successful retry, want 1", c.Size())
	}
}

func TestM66SearchSharedFailureReachesWaiters(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	boom := errors.New("upstream search failed")
	var fetches atomic.Int32
	fetch := func(ctx context.Context) (string, error) {
		fetches.Add(1)
		time.Sleep(80 * time.Millisecond)
		return "", boom
	}
	const callers = 4
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = c.GetOrCoalesce(ctx, key, DefaultSearchTTL, fetch)
		}(i)
	}
	wg.Wait()
	if fetches.Load() != 1 {
		t.Fatalf("fetch ran %d times, want 1 (one shared failure)", fetches.Load())
	}
	for i, err := range errs {
		if !errors.Is(err, boom) {
			t.Fatalf("waiter %d error = %v, want shared %v", i, err, boom)
		}
	}
	if c.Size() != 0 {
		t.Fatalf("Size = %d after shared failure, want 0", c.Size())
	}
}

func TestM66SearchWaiterContextCancelLeavesLeaderAlone(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	key := SearchKey("hello world", "brave", "web", 10, "", "", nil)
	leaderDone := make(chan struct{})
	fetch := func(ctx context.Context) (string, error) {
		defer close(leaderDone)
		time.Sleep(150 * time.Millisecond)
		return "leader-result", nil
	}
	leaderErr := make(chan error, 1)
	go func() {
		_, _, err := c.GetOrCoalesce(ctx, key, DefaultSearchTTL, fetch)
		leaderErr <- err
	}()
	time.Sleep(30 * time.Millisecond) // let the leader register
	wctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err := c.GetOrCoalesce(wctx, key, DefaultSearchTTL, fetch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter error = %v, want context.Canceled", err)
	}
	select {
	case <-leaderDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("leader did not finish after waiter cancelled")
	}
	if err := <-leaderErr; err != nil {
		t.Fatalf("leader error = %v, want nil (waiter cancel must not disturb the leader)", err)
	}
	if st := c.Stats(); st.InFlight != 0 {
		t.Fatalf("InFlight = %d after flight finished, want 0", st.InFlight)
	}
}

func TestM66SearchStatsAreReal(t *testing.T) {
	ctx := context.Background()
	c := NewSearchCache[string]()
	st := c.Stats()
	if st.Size != 0 || st.Hits != 0 || st.Misses != 0 {
		t.Fatalf("fresh Stats not zero: %+v", st)
	}
	key := SearchKey("q", "p", "web", 1, "", "", nil)
	if _, ok := c.Get(ctx, key); ok {
		t.Fatal("unexpected hit on empty cache")
	}
	c.Set(ctx, key, "v")
	if _, ok := c.Get(ctx, key); !ok {
		t.Fatal("expected hit after Set")
	}
	st = c.Stats()
	if st.Size != 1 || st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("Stats = %+v, want size 1 hits 1 misses 1", st)
	}
	if n := c.FlushAll(); n != 1 || c.Size() != 0 {
		t.Fatalf("FlushAll = %d, Size = %d, want 1/0", n, c.Size())
	}
}
