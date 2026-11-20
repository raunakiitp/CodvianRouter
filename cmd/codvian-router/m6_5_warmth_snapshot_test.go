package main

import (
	"testing"
	"time"
)

// TestCacheWarmthSnapshotMemoizes (M6.5 production install): the
// warmth func served to the hook must memoize per (provider, model)
// for the TTL — one ledger read per pair per window, never a fresh
// query per chain target inside the hook.
func TestCacheWarmthSnapshotMemoizes(t *testing.T) {
	calls := 0
	snap := newCacheWarmthSnapshot(time.Minute, func(provider, model string) (float64, bool) {
		calls++
		return 0.75, true
	})
	for i := 0; i < 5; i++ {
		frac, ok := snap.warmth("anthropic", "claude-sonnet-4-5")
		if !ok || frac != 0.75 {
			t.Fatalf("call %d: frac=%v ok=%v, want 0.75 true", i, frac, ok)
		}
	}
	if calls != 1 {
		t.Fatalf("query calls = %d, want 1 (memoized within TTL)", calls)
	}
	// A different pair is a different key: one more query.
	if _, ok := snap.warmth("openai", "gpt-5"); !ok {
		t.Fatal("second pair: ok=false, want true")
	}
	if calls != 2 {
		t.Fatalf("query calls = %d, want 2 (one per pair)", calls)
	}
}

// TestCacheWarmthSnapshotExpires (M6.5): after the TTL the entry is
// re-read, so a pair that gains warmth is picked up within a window.
func TestCacheWarmthSnapshotExpires(t *testing.T) {
	calls := 0
	snap := newCacheWarmthSnapshot(time.Millisecond, func(provider, model string) (float64, bool) {
		calls++
		return 0.5, true
	})
	if _, ok := snap.warmth("anthropic", "claude-sonnet-4-5"); !ok {
		t.Fatal("first call: ok=false, want true")
	}
	time.Sleep(2 * time.Millisecond)
	if _, ok := snap.warmth("anthropic", "claude-sonnet-4-5"); !ok {
		t.Fatal("second call after TTL: ok=false, want true")
	}
	if calls != 2 {
		t.Fatalf("query calls = %d, want 2 (re-read after TTL)", calls)
	}
}

// TestCacheWarmthSnapshotQueryErrorIsUnmeasured (M6.5): a ledger
// error yields ok=false — unmeasured, never a guessed 0 — and the
// hook therefore returns declared order for that pair.
func TestCacheWarmthSnapshotQueryErrorIsUnmeasured(t *testing.T) {
	snap := newCacheWarmthSnapshot(time.Minute, func(provider, model string) (float64, bool) {
		return 0, false
	})
	frac, ok := snap.warmth("anthropic", "claude-sonnet-4-5")
	if ok || frac != 0 {
		t.Fatalf("frac=%v ok=%v, want 0 false", frac, ok)
	}
}
