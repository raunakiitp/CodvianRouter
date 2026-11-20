package guardrails

import (
	"math/rand"
	"strings"
	"testing"
)

// M1.7 streaming acceptance tests (written before the
// StreamRedactor existed): a PEM block split across SSE chunks —
// including splits inside the BEGIN header, inside a 64-char body
// line, and inside the END footer — must never leak a body byte to
// the client. An unclosed block is withheld until Flush, which
// redacts it. Clean text passes through byte-identical at any
// chunking.

func feedChunks(t *testing.T, r *StreamRedactor, chunks []string) string {
	t.Helper()
	var out strings.Builder
	for _, c := range chunks {
		out.WriteString(r.WriteChunk(c))
	}
	return out.String()
}

func splitInto(s string, n int, rng *rand.Rand) []string {
	if n <= 1 {
		return []string{s}
	}
	cuts := make([]int, 0, n-1)
	for i := 0; i < n-1; i++ {
		cuts = append(cuts, rng.Intn(len(s)+1))
	}
	// Sort cuts.
	for i := 1; i < len(cuts); i++ {
		for j := i; j > 0 && cuts[j] < cuts[j-1]; j-- {
			cuts[j], cuts[j-1] = cuts[j-1], cuts[j]
		}
	}
	chunks := make([]string, 0, n)
	prev := 0
	for _, c := range cuts {
		chunks = append(chunks, s[prev:c])
		prev = c
	}
	chunks = append(chunks, s[prev:])
	return chunks
}

func TestStreamRedactorMultiChunkSSE(t *testing.T) {
	rng := rand.New(rand.NewSource(23))
	block := pemBlock(t, "PRIVATE KEY", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 3, rng)
	full := "answer prefix\n" + block + "\nanswer suffix"
	bodyLines := strings.Split(block, "\n")

	// Deterministic adversarial splits: every single split point
	// of the block in two chunks, plus a 1-byte-at-a-time feed.
	for split := 0; split <= len(full); split += 7 {
		r := NewStreamRedactor()
		got := feedChunks(t, r, []string{full[:split], full[split:]}) + r.Flush()
		for _, line := range bodyLines {
			if len(line) == 64 && strings.Contains(got, line) {
				t.Fatalf("split %d: leaked body line %q in %q", split, line, got)
			}
		}
		if strings.Contains(got, "-----END") {
			t.Fatalf("split %d: leaked END footer in %q", split, got)
		}
		if !strings.Contains(got, "[REDACTED:SECRET]") {
			t.Fatalf("split %d: no placeholder in %q", split, got)
		}
		if !strings.Contains(got, "answer prefix") || !strings.Contains(got, "answer suffix") {
			t.Fatalf("split %d: surrounding text lost in %q", split, got)
		}
	}

	// 1-byte chunks over the whole text.
	r := NewStreamRedactor()
	var chunks []string
	for _, ch := range full {
		chunks = append(chunks, string(ch))
	}
	got := feedChunks(t, r, chunks) + r.Flush()
	if got != "answer prefix\n[REDACTED:SECRET]\nanswer suffix" {
		t.Fatalf("1-byte chunks: got %q", got)
	}
	if r.RedactedBlocks() != 1 {
		t.Fatalf("RedactedBlocks() = %d, want 1", r.RedactedBlocks())
	}
}

func TestStreamRedactorWithholdsUnclosedPEM(t *testing.T) {
	rng := rand.New(rand.NewSource(29))
	block := pemBlock(t, "PRIVATE KEY", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/", 3, rng)
	unclosed := block[:strings.LastIndex(block, "-----END")]
	r := NewStreamRedactor()
	emitted := r.WriteChunk("prefix\n" + unclosed)
	if strings.Contains(emitted, "MII") || len(emitted) > len("prefix\n") {
		// The body lines are random base64; assert none of them
		// (nor any 8-char run of them) was emitted.
		for _, line := range strings.Split(unclosed, "\n") {
			if len(line) == 64 && strings.Contains(emitted, line[:8]) {
				t.Fatalf("unclosed PEM leaked body into emitted chunk: %q", emitted)
			}
		}
	}
	if emitted != "prefix\n" {
		t.Fatalf("unclosed PEM: WriteChunk emitted %q, want only the prefix", emitted)
	}
	flushed := r.Flush()
	if strings.Contains(flushed, "MII") {
		t.Fatalf("unclosed PEM: Flush leaked body: %q", flushed)
	}
	for _, line := range strings.Split(unclosed, "\n") {
		if len(line) == 64 && strings.Contains(flushed, line) {
			t.Fatalf("unclosed PEM: Flush leaked body line %q", line)
		}
	}
	if !strings.Contains(flushed, "[REDACTED:SECRET]") {
		t.Fatalf("unclosed PEM: Flush did not redact: %q", flushed)
	}
}

func TestStreamRedactorSplitHeader(t *testing.T) {
	// Chunks split inside "-----BEGIN PRIVATE KEY-----" itself.
	parts := []string{"pre ", "-----BEG", "IN PRIVATE KEY-----\n", "QUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJDQUJD\n", "-----END PRIVATE KEY-----", " post"}
	r := NewStreamRedactor()
	got := feedChunks(t, r, parts) + r.Flush()
	if strings.Contains(got, "QUJD") || strings.Contains(got, "-----END") {
		t.Fatalf("split header: leaked PEM content: %q", got)
	}
	if got != "pre [REDACTED:SECRET] post" {
		t.Fatalf("split header: got %q, want %q", got, "pre [REDACTED:SECRET] post")
	}
}

func TestStreamRedactorCleanPassthrough(t *testing.T) {
	text := "Hello, this is a normal streamed answer with no secrets.\nSecond line: 12345 and some-dashes ----- but no PEM.\n"
	rng := rand.New(rand.NewSource(31))
	for _, n := range []int{1, 2, 3, 7, 13} {
		r := NewStreamRedactor()
		got := feedChunks(t, r, splitInto(text, n, rng)) + r.Flush()
		if got != text {
			t.Fatalf("clean text, %d chunks: got %q, want %q", n, got, text)
		}
	}
}

// FuzzStreamRedactorChunkSplits: a PEM-bearing text split at
// pseudo-random cut points must never leak a body line or footer,
// and must round-trip its non-PEM text.
func FuzzStreamRedactorChunkSplits(f *testing.F) {
	f.Add(0, 2)
	f.Add(1, 5)
	f.Add(2, 11)
	f.Add(0, 1)
	f.Fuzz(func(t *testing.T, seed, nchunks int) {
		if nchunks < 1 || nchunks > 32 {
			t.Skip("chunk count out of range")
		}
		if seed < 0 {
			t.Skip("seed out of range")
		}
		rng := rand.New(rand.NewSource(int64(seed)))
		alphabets := []string{
			"ABCDEFGHIJKLMNOPQRSTUVWXYZ",
			"abcdefghijklmnopqrstuvwxyz",
			"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/",
		}
		block := pemBlock(t, "PRIVATE KEY", alphabets[seed%len(alphabets)], 3, rng)
		full := "pre\n" + block + "\npost"
		r := NewStreamRedactor()
		got := feedChunks(t, r, splitInto(full, nchunks, rng)) + r.Flush()
		for _, line := range strings.Split(block, "\n") {
			if len(line) == 64 && strings.Contains(got, line) {
				t.Fatalf("fuzz: leaked body line %q (seed=%d chunks=%d): %q", line, seed, nchunks, got)
			}
		}
		if strings.Contains(got, "-----END") {
			t.Fatalf("fuzz: leaked END footer (seed=%d chunks=%d): %q", seed, nchunks, got)
		}
		if !strings.Contains(got, "[REDACTED:SECRET]") || !strings.Contains(got, "pre") || !strings.Contains(got, "post") {
			t.Fatalf("fuzz: bad output (seed=%d chunks=%d): %q", seed, nchunks, got)
		}
	})
}
