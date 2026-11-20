package tokencount

import (
	"fmt"
	"sync"
	"testing"
)

// The Count memo (CHECKLIST 249-250) must return exactly what a
// fresh BPE encode would — same tokens for the same (model, text),
// different tokens where the encodings genuinely differ (CJK:
// cl100k 4 vs o200k 2) — and a second Count of the same text must
// hit the memo instead of re-encoding.
func TestCountMemoReturnsSameCounts(t *testing.T) {
	cases := []struct{ model, text string }{
		{"gpt-4", "hello world"},
		{"gpt-4", "The quick brown fox jumps over the lazy dog"},
		{"gpt-4o", "こんにちは世界"},
		{"gpt-4", "こんにちは世界"},
		{"unknown-model-xyz", "hello world"},
		{"gpt-4", ""},
		{"gpt-4", "Explain migrating monarch butterflies in one sentence."},
	}
	for _, tc := range cases {
		first, err := Count(tc.model, tc.text)
		if err != nil {
			t.Fatalf("Count(%q, %q): %v", tc.model, tc.text, err)
		}
		second, err := Count(tc.model, tc.text)
		if err != nil {
			t.Fatalf("Count again (%q, %q): %v", tc.model, tc.text, err)
		}
		if first != second {
			t.Errorf("Count(%q, %q): first %+v != second %+v (memo must be exact)", tc.model, tc.text, first, second)
		}
	}
	// Encodings must not collide in the memo: the same CJK text
	// counts differently under cl100k and o200k, and both must
	// survive interleaved Counts.
	cl, _ := Count("gpt-4", "こんにちは世界")
	o2, _ := Count("gpt-4o", "こんにちは世界")
	if cl.Tokens != 4 || o2.Tokens != 2 {
		t.Errorf("CJK memo collision: cl100k=%d (want 4), o200k=%d (want 2)", cl.Tokens, o2.Tokens)
	}
	cl2, _ := Count("gpt-4", "こんにちは世界")
	o22, _ := Count("gpt-4o", "こんにちは世界")
	if cl2.Tokens != 4 || o22.Tokens != 2 {
		t.Errorf("CJK memo after interleave: cl100k=%d (want 4), o200k=%d (want 2)", cl2.Tokens, o22.Tokens)
	}
	// The fallback flag must not collide either: "gpt-4" and an
	// unknown model both resolve to cl100k_base and count the
	// same tokens, but report different Fallback values — the
	// memo key carries the flag, so neither overwrites the other.
	known, _ := Count("gpt-4", "hello world")
	unknown, _ := Count("unknown-model-xyz", "hello world")
	if known.Fallback || !unknown.Fallback {
		t.Errorf("fallback memo collision: known=%+v unknown=%+v", known, unknown)
	}
	if known.Tokens != unknown.Tokens {
		t.Errorf("fallback memo changed the count: known=%d unknown=%d", known.Tokens, unknown.Tokens)
	}
}

// Filling the memo past its cap clears it; counts after the clear
// must still be exactly right (a clear costs a re-encode, never a
// wrong answer).
func TestCountMemoBoundedClearKeepsCounts(t *testing.T) {
	for i := 0; i < maxMemoEntries+100; i++ {
		if _, err := Count("gpt-4", fmt.Sprintf("filler prompt number %d about monarch butterflies", i)); err != nil {
			t.Fatalf("Count filler %d: %v", i, err)
		}
	}
	got, err := Count("gpt-4", "hello world")
	if err != nil {
		t.Fatalf("Count after clear: %v", err)
	}
	if got.Tokens != 2 {
		t.Errorf("Count after memo clear = %d, want 2", got.Tokens)
	}
	// And the re-counted value is itself memoised again.
	got2, _ := Count("gpt-4", "hello world")
	if got2 != got {
		t.Errorf("Count after clear, second call %+v != first %+v", got2, got)
	}
}

// Concurrent Counts of the same and different texts must all agree
// with a sequential Count (the memo lock guards the map only;
// encodes run outside it).
func TestCountMemoConcurrent(t *testing.T) {
	texts := []string{"hello world", "The quick brown fox jumps over the lazy dog", "こんにちは世界", "Explain migrating monarch butterflies in one sentence."}
	want := map[string]int{}
	for _, tx := range texts {
		c, err := Count("gpt-4", tx)
		if err != nil {
			t.Fatalf("Count(%q): %v", tx, err)
		}
		want[tx] = c.Tokens
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				tx := texts[(g+i)%len(texts)]
				c, err := Count("gpt-4", tx)
				if err != nil {
					t.Errorf("Count(%q): %v", tx, err)
					return
				}
				if c.Tokens != want[tx] {
					t.Errorf("Count(%q) = %d, want %d", tx, c.Tokens, want[tx])
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

// BenchmarkCountMemoHit is the per-request collapse the profile
// called for: the 2nd/3rd encode of the same prompt in one request
// (traffic estimate, context-fit gate, reconcileUsage) must cost a
// hash + map lookup, not a BPE encode.
func BenchmarkCountMemoHit(b *testing.B) {
	if _, err := Count("gpt-4", "Explain migrating monarch butterflies in one sentence."); err != nil {
		b.Fatalf("Count: %v", err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Count("gpt-4", "Explain migrating monarch butterflies in one sentence."); err != nil {
			b.Fatalf("Count: %v", err)
		}
	}
}

// BenchmarkCountCold measures a first-seen prompt (the latency
// pass: 1,000 distinct prompts) — the memo cannot help here, so
// this is the floor the hit benchmark must beat by an order of
// magnitude for the memo to be worth its SHA-256.
func BenchmarkCountCold(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Count("gpt-4", fmt.Sprintf("Explain topic number %d in one sentence, distinctly worded each time.", i)); err != nil {
			b.Fatalf("Count: %v", err)
		}
	}
}
