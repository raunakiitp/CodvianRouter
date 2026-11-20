package degrade

import (
	"fmt"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestSystemPromptHash(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hi"},
	}
	h1 := SystemPromptHash(msgs)
	if h1 == "" {
		t.Fatal("system prompt must hash to a non-empty value")
	}
	if len(h1) != 24 {
		t.Fatalf("hash %q must be 24 hex chars", h1)
	}
	// Same system prompt, different user turn: same hash — the
	// freeze keys on the prefix, not the request.
	h2 := SystemPromptHash([]provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "something else entirely"},
	})
	if h1 != h2 {
		t.Fatalf("same system prompt must hash identically: %q vs %q", h1, h2)
	}
	// Different system prompt: different hash.
	h3 := SystemPromptHash([]provider.Message{{Role: "system", Content: "You are terse."}})
	if h1 == h3 {
		t.Fatal("different system prompts must hash differently")
	}
	// Developer role joins the prefix; user-only has none.
	h4 := SystemPromptHash([]provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "developer", Content: "Be concise."},
	})
	if h4 == h1 {
		t.Fatal("developer text must join the hashed prefix")
	}
	if got := SystemPromptHash([]provider.Message{{Role: "user", Content: "hi"}}); got != "" {
		t.Fatalf("user-only prompt must hash to empty, got %q", got)
	}
	if got := SystemPromptHash(nil); got != "" {
		t.Fatalf("nil messages must hash to empty, got %q", got)
	}
	if got := SystemPromptHash([]provider.Message{{Role: "system", Content: "   "}}); got != "" {
		t.Fatalf("blank system prompt must hash to empty, got %q", got)
	}
	// Part-carrying system message hashes its text parts.
	h5 := SystemPromptHash([]provider.Message{{
		Role: "system",
		Parts: []provider.ContentPart{
			{Type: "text", Text: "You are helpful."},
		},
	}})
	if h5 != h1 {
		t.Fatalf("text-part system prompt must hash like its string form: %q vs %q", h5, h1)
	}
}

func TestPrefixFreezeThreshold(t *testing.T) {
	msgs := []provider.Message{{Role: "system", Content: "You are helpful."}}
	f := NewPrefixFreeze(true, 3)
	if f.IsFrozen(msgs) {
		t.Fatal("must not be frozen before any observation")
	}
	f.Observe(msgs)
	f.Observe(msgs)
	if f.IsFrozen(msgs) {
		t.Fatal("must not be frozen at 2/3 observations")
	}
	if got := f.Observations(SystemPromptHash(msgs)); got != 2 {
		t.Fatalf("observations = %d, want 2", got)
	}
	f.Observe(msgs)
	if !f.IsFrozen(msgs) {
		t.Fatal("must be frozen at 3/3 observations")
	}
	// Isolation per hash: another prompt is still unfrozen...
	// until it is observed itself.
	other := []provider.Message{{Role: "system", Content: "You are terse."}}
	if f.IsFrozen(other) {
		t.Fatal("a different system prompt must not inherit the freeze")
	}
	f.Observe(other)
	if got := f.Entries(); got != 2 {
		t.Fatalf("entries = %d, want 2 (one per observed prompt)", got)
	}
}

func TestPrefixFreezeDisabledIsNoop(t *testing.T) {
	msgs := []provider.Message{{Role: "system", Content: "You are helpful."}}
	f := NewPrefixFreeze(false, 3)
	for i := 0; i < 10; i++ {
		f.Observe(msgs)
	}
	if f.IsFrozen(msgs) {
		t.Fatal("disabled observer must never freeze")
	}
	if got := f.Observations(SystemPromptHash(msgs)); got != 0 {
		t.Fatalf("disabled observer must record nothing, got %d observations", got)
	}
	if f.Entries() != 0 {
		t.Fatalf("disabled observer must track nothing, got %d entries", f.Entries())
	}
}

func TestPrefixFreezeDefaultThresholdAndReset(t *testing.T) {
	// threshold <= 0 means the default (3).
	f := NewPrefixFreeze(true, 0)
	msgs := []provider.Message{{Role: "system", Content: "You are helpful."}}
	f.Observe(msgs)
	f.Observe(msgs)
	if f.IsFrozen(msgs) {
		t.Fatal("default threshold must be 3, frozen at 2")
	}
	f.Observe(msgs)
	if !f.IsFrozen(msgs) {
		t.Fatal("default threshold must be 3, not frozen at 3")
	}
	f.Reset()
	if f.IsFrozen(msgs) || f.Entries() != 0 {
		t.Fatal("Reset must clear all observations")
	}
}

func TestPrefixFreezeBounded(t *testing.T) {
	f := NewPrefixFreeze(true, 1)
	// Observe more distinct prefixes than the cap: the store
	// must stay bounded and the oldest must be evicted first.
	for i := 0; i < MaxPrefixFreezeEntries+50; i++ {
		f.ObserveHash(fmt.Sprintf("prefix-%06d", i))
	}
	if got := f.Entries(); got > MaxPrefixFreezeEntries {
		t.Fatalf("entries = %d, must stay <= %d", got, MaxPrefixFreezeEntries)
	}
	if got := f.Entries(); got != MaxPrefixFreezeEntries {
		t.Fatalf("entries = %d, want exactly the cap %d", got, MaxPrefixFreezeEntries)
	}
	if got := f.Observations("prefix-000000"); got != 0 {
		t.Fatalf("oldest hash must be evicted, still has %d observations", got)
	}
	// The newest hash must still be tracked (and frozen at
	// threshold 1).
	last := fmt.Sprintf("prefix-%06d", MaxPrefixFreezeEntries+49)
	if !f.IsFrozenHash(last) {
		t.Fatalf("newest hash %q must still be tracked and frozen", last)
	}
}

func TestResolveCacheAwareStrategy(t *testing.T) {
	// Caching provider: aggressive/ultra downgrade, prefix
	// skipped, deterministic only — even with no cache_control
	// marker (OmniRoute #3955: automatic prefix caching).
	for _, tc := range []struct {
		in, want string
	}{
		{"aggressive", "standard"},
		{"ultra", "standard"},
		{"standard", "standard"},
		{"lite", "lite"},
		{"none", "none"},
	} {
		got := ResolveCacheAwareStrategy(tc.in, true)
		if got.Strategy != tc.want {
			t.Fatalf("caching %q -> %q, want %q", tc.in, got.Strategy, tc.want)
		}
		if !got.SkipSystemPrompt || !got.DeterministicOnly {
			t.Fatalf("caching %q must skip the system prompt + deterministic only, got %+v", tc.in, got)
		}
	}
	// Non-caching provider: everything passes through untouched.
	for _, in := range []string{"aggressive", "ultra", "standard", "lite", "none"} {
		got := ResolveCacheAwareStrategy(in, false)
		if got.Strategy != in || got.SkipSystemPrompt || got.DeterministicOnly {
			t.Fatalf("non-caching %q must pass through unchanged, got %+v", in, got)
		}
	}
}

func TestResolvePreserveSystemPrompt(t *testing.T) {
	// always: preserved whatever the cache state.
	for _, caching := range []bool{false, true} {
		for _, cc := range []bool{false, true} {
			if !ResolvePreserveSystemPrompt(SystemPromptPreserveAlways, caching, cc) {
				t.Fatalf("always must preserve (caching=%v cc=%v)", caching, cc)
			}
			if ResolvePreserveSystemPrompt(SystemPromptPreserveNever, caching, cc) {
				t.Fatalf("never must not preserve (caching=%v cc=%v)", caching, cc)
			}
		}
	}
	// whenNoCache: preserve exactly when there IS a cache to
	// protect — provider caches, or cache_control present.
	cases := []struct {
		caching, cc, want bool
	}{
		{false, false, false},
		{true, false, true},
		{false, true, true},
		{true, true, true},
	}
	for _, tc := range cases {
		if got := ResolvePreserveSystemPrompt(SystemPromptPreserveWhenNoCache, tc.caching, tc.cc); got != tc.want {
			t.Fatalf("whenNoCache(caching=%v cc=%v) = %v, want %v", tc.caching, tc.cc, got, tc.want)
		}
	}
	// Absent/unknown mode: the "always" default.
	if !ResolvePreserveSystemPrompt("", false, false) {
		t.Fatal("empty mode must default to preserve (always)")
	}
	if !ResolvePreserveSystemPrompt("bogus", false, false) {
		t.Fatal("unknown mode must default to preserve (always)")
	}
}
