package tokencount

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// Known cl100k_base counts, cross-checked against OpenAI's
// published tiktoken examples: "hello world" is 2 tokens,
// "The quick brown fox jumps over the lazy dog" is 9. These pin
// the vendored BPE tables — a corrupted or swapped table fails
// here, not silently in production counts.
func TestCountKnownTexts(t *testing.T) {
	cases := []struct {
		model    string
		text     string
		want     int
		encoding string
		fallback bool
	}{
		{"gpt-4", "", 0, "cl100k_base", false},
		{"gpt-4", "hello world", 2, "cl100k_base", false},
		{"gpt-4", "Hello, world!", 4, "cl100k_base", false},
		{"gpt-4", "The quick brown fox jumps over the lazy dog", 9, "cl100k_base", false},
		{"gpt-4", "Write a Python function to reverse a string", 8, "cl100k_base", false},
		{"gpt-4", "Hello", 1, "cl100k_base", false},
		{"gpt-3.5-turbo", "hello world", 2, "cl100k_base", false},
		// o200k_base: same on ASCII, different on CJK — the
		// encodings genuinely differ, so both are pinned.
		{"gpt-4o", "hello world", 2, "o200k_base", false},
		{"gpt-4o", "The quick brown fox jumps over the lazy dog", 9, "o200k_base", false},
		{"gpt-4o", "こんにちは世界", 2, "o200k_base", false},
		{"gpt-4", "こんにちは世界", 4, "cl100k_base", false},
		// Unknown models never error: they fall back to
		// cl100k_base and say so.
		{"unknown-model-xyz", "hello world", 2, "cl100k_base", true},
		{"auto/cheap", "hello world", 2, "cl100k_base", true},
		{"llama-3-70b", "hello world", 2, "cl100k_base", true},
		// o200k-era prefixes resolve without the fallback flag.
		{"gpt-4o-mini", "hello world", 2, "o200k_base", false},
		{"o1-preview", "hello world", 2, "o200k_base", false},
		{"gpt-5", "hello world", 2, "o200k_base", false},
	}
	for _, tc := range cases {
		got, err := Count(tc.model, tc.text)
		if err != nil {
			t.Fatalf("Count(%q, %q): unexpected error: %v", tc.model, tc.text, err)
		}
		if got.Tokens != tc.want || got.Encoding != tc.encoding || got.Fallback != tc.fallback {
			t.Errorf("Count(%q, %q) = %+v, want tokens=%d encoding=%s fallback=%v",
				tc.model, tc.text, got, tc.want, tc.encoding, tc.fallback)
		}
	}
}

// A prompt that literally contains a special-token string must
// count as ordinary text, never panic (Encode would panic on
// disallowed specials; Count uses EncodeOrdinary).
func TestCountSpecialTokenTextDoesNotPanic(t *testing.T) {
	got, err := Count("gpt-4", "<|endoftext|>")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.Tokens != 7 {
		t.Errorf("Count(<|endoftext|>) = %d, want 7", got.Tokens)
	}
}

func TestCountPrompt(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "hello world"},
	}
	got, err := CountPrompt("gpt-4", msgs)
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	// "You are helpful.\n" + "hello world\n" — the count must be
	// the count of the joined text, not of either part alone.
	want, err := Count("gpt-4", "You are helpful.\nhello world\n")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.Tokens != want.Tokens {
		t.Errorf("CountPrompt = %d, want %d (joined text)", got.Tokens, want.Tokens)
	}
	if got.Tokens == 0 {
		t.Errorf("CountPrompt counted 0 for non-empty messages")
	}

	// Text parts and tool-call arguments count too.
	msgs2 := []provider.Message{
		{Role: "user", Parts: []provider.ContentPart{{Type: "text", Text: "hello world"}}},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{Function: wire.ToolCallFunction{Arguments: `{"x": 1}`}}}},
	}
	got2, err := CountPrompt("gpt-4", msgs2)
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want2, _ := Count("gpt-4", "hello world\n{\"x\": 1}\n")
	if got2.Tokens != want2.Tokens {
		t.Errorf("CountPrompt(parts/tools) = %d, want %d", got2.Tokens, want2.Tokens)
	}

	// Empty messages count 0, and never error.
	got3, err := CountPrompt("gpt-4", nil)
	if err != nil || got3.Tokens != 0 {
		t.Errorf("CountPrompt(nil) = %+v, err %v, want 0 tokens", got3, err)
	}
}

// The offline loader must never attempt a network fetch: an
// encoding with no vendored table (p50k_base) errors from the
// loader, and the error names the offline loader.
func TestOfflineLoaderRefusesUnknownTable(t *testing.T) {
	installLoader()
	_, err := embeddedLoader{}.LoadTiktokenBpe("https://openaipublic.blob.core.windows.net/encodings/p50k_base.tiktoken")
	if err == nil {
		t.Fatalf("offline loader answered a non-vendored BPE URL without error")
	}
}
