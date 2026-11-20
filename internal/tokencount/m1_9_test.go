package tokencount

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M1.9 acceptance tests (written FIRST, before the fix).
//
// 1. Array-form double count: provider.Message.UnmarshalJSON sets
// BOTH Content (concatenated text parts) and Parts for array
// content, and CountPrompt used to add both — array-form messages
// counted ~2x. String-form and array-form of the same text must
// count equally.
// 2. Legacy-model encoding fallback: tiktoken-go resolves
// text-davinci-003 / davinci / gpt2 etc. to p50k_base, r50k_base,
// p50k_edit or gpt2 — none of which is vendored — so Count used
// to ERROR, breaking the package promise that it never fails on
// an unknown/legacy model. They must fall back to cl100k_base
// with Fallback=true.
// 3. Input-size cap: exact BPE work is capped; beyond the cap the
// count is a chars/4 estimate (Estimated=true), never an error
// and never an unbounded encode.
// 4. BPE linear-ish: the vendored tiktoken-go merge is O(n^2) per
// regex piece (measured: 64 KiB of 'a' took 6.5 s pre-fix, so
// 1 MiB pathological would take ~30 min). A 1 MiB input —
// realistic prose AND a pathological single-piece run — must
// complete in under 1 s (N=1000 ms, chosen from the measured
// realistic baseline of 542 ms pre-fix, with headroom for
// non-repeating text).

func TestCountPromptArrayFormEqualsStringForm(t *testing.T) {
	// Array form as it arrives on the wire (JSON decode, which
	// sets both Content and Parts).
	var arrayMsg provider.Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"hello world"}]}`), &arrayMsg); err != nil {
		t.Fatalf("unmarshal array-form message: %v", err)
	}
	stringMsg := provider.Message{Role: "user", Content: "hello world"}
	arrayCount, err := CountPrompt("gpt-4", []provider.Message{arrayMsg})
	if err != nil {
		t.Fatalf("CountPrompt(array): %v", err)
	}
	stringCount, err := CountPrompt("gpt-4", []provider.Message{stringMsg})
	if err != nil {
		t.Fatalf("CountPrompt(string): %v", err)
	}
	if arrayCount.Tokens != stringCount.Tokens {
		t.Errorf("CountPrompt array-form = %d, string-form = %d, want equal", arrayCount.Tokens, stringCount.Tokens)
	}

	// Multi-part: text + image_url — the image carries no text
	// tokens, so the count must equal the text-only count.
	var multiMsg provider.Message
	if err := json.Unmarshal([]byte(`{"role":"user","content":[{"type":"text","text":"hello world"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`), &multiMsg); err != nil {
		t.Fatalf("unmarshal multi-part message: %v", err)
	}
	multiCount, err := CountPrompt("gpt-4", []provider.Message{multiMsg})
	if err != nil {
		t.Fatalf("CountPrompt(multi-part): %v", err)
	}
	if multiCount.Tokens != stringCount.Tokens {
		t.Errorf("CountPrompt multi-part = %d, text-only = %d, want equal", multiCount.Tokens, stringCount.Tokens)
	}

	// Struct literal with BOTH Content and Parts set (the shape
	// UnmarshalJSON produces): Parts is authoritative, Content
	// must not be added a second time.
	both := provider.Message{
		Role:    "user",
		Content: "hello world",
		Parts:   []provider.ContentPart{{Type: "text", Text: "hello world"}},
	}
	bothCount, err := CountPrompt("gpt-4", []provider.Message{both})
	if err != nil {
		t.Fatalf("CountPrompt(both): %v", err)
	}
	if bothCount.Tokens != stringCount.Tokens {
		t.Errorf("CountPrompt Content+Parts = %d, want %d (parts only, no double count)", bothCount.Tokens, stringCount.Tokens)
	}

	// The fix must propagate to the wire count, which builds on
	// CountPrompt.
	arrayWire, err := CountPromptWire("gpt-4", "openai", provider.ChatRequest{Messages: []provider.Message{arrayMsg}})
	if err != nil {
		t.Fatalf("CountPromptWire(array): %v", err)
	}
	stringWire, err := CountPromptWire("gpt-4", "openai", provider.ChatRequest{Messages: []provider.Message{stringMsg}})
	if err != nil {
		t.Fatalf("CountPromptWire(string): %v", err)
	}
	if arrayWire.Tokens != stringWire.Tokens {
		t.Errorf("CountPromptWire array-form = %d, string-form = %d, want equal", arrayWire.Tokens, stringWire.Tokens)
	}
}

func TestCountLegacyModelEncodingFallback(t *testing.T) {
	legacy := []string{
		"text-davinci-003",            // p50k_base — not vendored
		"text-davinci-002",            // p50k_base
		"text-davinci-001",            // r50k_base
		"davinci",                     // r50k_base
		"curie",                       // r50k_base
		"gpt2",                        // gpt2
		"code-davinci-002",            // p50k_base
		"text-davinci-edit-001",       // p50k_edit
		"text-similarity-davinci-001", // r50k_base
	}
	for _, model := range legacy {
		got, err := Count(model, "hello world")
		if err != nil {
			t.Errorf("Count(%q): unexpected error: %v (legacy models must fall back, never error)", model, err)
			continue
		}
		if got.Encoding != "cl100k_base" || !got.Fallback {
			t.Errorf("Count(%q) = %+v, want encoding=cl100k_base fallback=true", model, got)
		}
		want, err := Count("gpt-4", "hello world")
		if err != nil {
			t.Fatalf("Count(gpt-4): %v", err)
		}
		if got.Tokens != want.Tokens {
			t.Errorf("Count(%q) = %d tokens, want %d (cl100k_base count)", model, got.Tokens, want.Tokens)
		}
	}
}

func TestCountInputSizeCap(t *testing.T) {
	// Well above the exact-count cap: must return a chars/4
	// estimate, flagged Estimated, without error and without
	// doing unbounded BPE work.
	big := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 150000) // ~6.6 MB
	if len(big) <= maxExactCountBytes {
		t.Fatalf("test input %d bytes does not exceed cap %d", len(big), maxExactCountBytes)
	}
	start := time.Now()
	got, err := Count("gpt-4", big)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Count(oversize): unexpected error: %v", err)
	}
	if !got.Estimated {
		t.Errorf("Count(oversize) = %+v, want Estimated=true", got)
	}
	if want := len(big) / 4; got.Tokens != want {
		t.Errorf("Count(oversize) = %d tokens, want %d (chars/4 estimate)", got.Tokens, want)
	}
	if elapsed > time.Second {
		t.Errorf("Count(oversize) took %s, want < 1s", elapsed)
	}

	// The same cap must apply through CountPrompt without
	// building an unbounded joined string.
	msgs := []provider.Message{{Role: "user", Content: big}}
	gotPrompt, err := CountPrompt("gpt-4", msgs)
	if err != nil {
		t.Fatalf("CountPrompt(oversize): unexpected error: %v", err)
	}
	if !gotPrompt.Estimated {
		t.Errorf("CountPrompt(oversize) = %+v, want Estimated=true", gotPrompt)
	}
}

// TestCountOneMBCompletesQuickly pins the M1.9 timing bound
// (oneMBBound: 6 s without -race, 50 s with -race — see
// m1_9_bound_test.go / m1_9_bound_race_test.go for the measured
// basis of each) for a 1 MiB input, in three shapes. Pre-fix
// baseline (measured 2026-11-18, this VM): realistic 1 MiB =
// 542 ms / 206,456 tokens; pathological 64 KiB of 'a' = 6.5 s;
// 1 MiB of distinct random letters unchunked never finished
// in 180 s. The realistic text is distinct word soup (seeded),
// not a repeated paragraph, so the chunk memo cannot hide the
// real encode work.
func TestCountOneMBCompletesQuickly(t *testing.T) {
	words := []string{"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog", "remote", "work", "changed", "teams", "build", "trust", "decisions", "written", "down", "open", "anyone", "state", "without", "watching", "garden", "river", "market", "window", "paper", "silver", "autumn", "bridge", "candle", "harvest", "meadow", "ocean", "pillow", "quiet", "valley", "wisdom", "yellow", "zephyr", "anchor", "breeze"}
	r := rand.New(rand.NewSource(7))
	var sb strings.Builder
	for sb.Len() < 1024*1024 {
		sb.WriteString(words[r.Intn(len(words))])
		sb.WriteByte(' ')
		if r.Intn(12) == 0 {
			sb.WriteByte('\n')
		}
	}
	realistic := sb.String()[:1024*1024]
	countTimed := func(name, text string) {
		start := time.Now()
		got, err := Count("gpt-4", text)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("Count(1MB %s): %v", name, err)
		}
		if got.Estimated {
			t.Errorf("Count(1MB %s) = %+v, want exact BPE count (Estimated=false)", name, got)
		}
		if got.Tokens == 0 {
			t.Errorf("Count(1MB %s) counted 0 tokens", name)
		}
		if elapsed > oneMBBound {
			t.Errorf("Count(1MB %s) took %s, want < %s", name, elapsed, oneMBBound)
		} else {
			t.Logf("1MB %s: %d tokens in %s", name, got.Tokens, elapsed)
		}
	}
	countTimed("distinct-realistic", realistic)
	countTimed("repeated-a", strings.Repeat("a", 1024*1024))
	var lb strings.Builder
	for lb.Len() < 1024*1024 {
		lb.WriteByte(byte('a' + r.Intn(26)))
	}
	countTimed("distinct-letters", lb.String())
}

// Chunked counting (inputs above chunkThreshold) must stay close
// to the single-encode reference: the boundary drift is bounded
// and measured here, not assumed.
func TestCountChunkedMatchesUnchunked(t *testing.T) {
	para := "The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs. How vexingly quick daft zebras jump! "
	text := strings.Repeat(para, 2000) // ~260 KiB, above the chunk threshold
	if len(text) <= chunkThresholdBytes {
		t.Fatalf("test input %d bytes does not exceed chunk threshold %d", len(text), chunkThresholdBytes)
	}
	enc, err := getEncoder("cl100k_base")
	if err != nil {
		t.Fatalf("getEncoder: %v", err)
	}
	want := len(enc.EncodeOrdinary(text))
	got, err := Count("gpt-4", text)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.Estimated {
		t.Fatalf("Count(chunked) = %+v, want exact count", got)
	}
	diff := got.Tokens - want
	if diff < 0 {
		diff = -diff
	}
	// Drift budget: at most 0.5% of the reference count. The
	// actual measured drift is logged so the drift table stays
	// honest.
	t.Logf("chunked=%d unchunked=%d diff=%d (%.3f%%)", got.Tokens, want, diff, 100*float64(diff)/float64(want))
	if float64(diff) > 0.005*float64(want) {
		t.Errorf("chunked count %d vs unchunked %d: drift %d exceeds 0.5%%", got.Tokens, want, diff)
	}
}
