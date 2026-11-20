// M6.7c: unit tests for the shared semantic window helper
// (internal/provider/semantic_window.go).
//
// The window literals are the SAME literals the engine's
// TestM67SemanticTextWindow and the relay's
// TestM67bRelaySemanticWindow pin, so this file proves the move to
// the shared home changed no byte: if any literal here changes,
// the helper is wrong, not the tests. The cap tests pin the
// boundary (50 served, 51 bypassed) and the raw-count semantics
// (system/developer messages count toward the 50, matching
// OmniRoute's conv.length check — see SemanticTurnsExceeded).
package provider

import (
	"fmt"
	"testing"
)

func semWinMsg(role, text string) Message {
	return Message{Role: role, Content: text}
}

func TestSemanticWindowText(t *testing.T) {
	// The 3-turn store/lookup pair, byte-identical to the engine's
	// and the relay's pinned window strings.
	store := []Message{
		semWinMsg("user", "What is the capital of France?"),
		semWinMsg("assistant", "The capital of France is Paris, a large historic city on the river Seine famous for museums gardens and cafes."),
		semWinMsg("user", "Tell me more about its history."),
	}
	wantStore := "user: What is the capital of France?\nassistant: The capital of France is Paris, a large historic city on the river Seine famous for museums gardens and cafes.\nuser: Tell me more about its history."
	if got := SemanticWindowText(store); got != wantStore {
		t.Fatalf("SemanticWindowText(store) = %q, want %q", got, wantStore)
	}
	lookup := []Message{
		semWinMsg("user", "What is the capital of France?"),
		semWinMsg("assistant", "The capital of Germany is Berlin, a large historic city on the river Spree famous for museums parks and nightlife."),
		semWinMsg("user", "Tell me more about its history."),
	}
	wantLookup := "user: What is the capital of France?\nassistant: The capital of Germany is Berlin, a large historic city on the river Spree famous for museums parks and nightlife.\nuser: Tell me more about its history."
	if got := SemanticWindowText(lookup); got != wantLookup {
		t.Fatalf("SemanticWindowText(lookup) = %q, want %q", got, wantLookup)
	}

	// Last 3 turns only; empty-text messages contribute no line
	// but still occupy their window slot.
	req := []Message{
		semWinMsg("system", "You are helpful."),
		semWinMsg("user", "Old question?"),
		semWinMsg("assistant", "Old answer."),
		semWinMsg("user", "New question?"),
		semWinMsg("assistant", ""),
	}
	if want := "assistant: Old answer.\nuser: New question?"; SemanticWindowText(req) != want {
		t.Fatalf("SemanticWindowText = %q, want %q (last 3 turns only, empty text skipped)", SemanticWindowText(req), want)
	}
	// A single user message keeps the "user: " prefix.
	if got := SemanticWindowText([]Message{semWinMsg("user", "What is 2+2?")}); got != "user: What is 2+2?" {
		t.Fatalf("SemanticWindowText(single) = %q, want %q", got, "user: What is 2+2?")
	}
	// System and developer messages are excluded even inside
	// the window: the window is the same with or without them.
	for _, role := range []string{"system", "developer"} {
		msgs := []Message{semWinMsg(role, "Be brief."), semWinMsg("user", "What is 2+2?")}
		if got := SemanticWindowText(msgs); got != "user: What is 2+2?" {
			t.Fatalf("SemanticWindowText(%s+user) = %q, want %q (%s excluded)", role, got, "user: What is 2+2?", role)
		}
	}
	// A missing role reads as "user".
	if got := SemanticWindowText([]Message{{Content: "What is 2+2?"}}); got != "user: What is 2+2?" {
		t.Fatalf("SemanticWindowText(no role) = %q, want %q (missing role reads as user)", got, "user: What is 2+2?")
	}
	// No text anywhere: "".
	if got := SemanticWindowText([]Message{semWinMsg("assistant", "")}); got != "" {
		t.Fatalf("SemanticWindowText(empty) = %q, want empty", got)
	}
	// Text parts (array content) are joined with a space and
	// trimmed, same as the old messageText.
	parts := []Message{{Role: "user", Parts: []ContentPart{{Type: "text", Text: "Hello"}, {Type: "text", Text: "there"}}}}
	if got := SemanticWindowText(parts); got != "user: Hello there" {
		t.Fatalf("SemanticWindowText(parts) = %q, want %q", got, "user: Hello there")
	}
	// The window helper is pure: it still returns the last-3
	// window for a >50-turn conversation. The cap is enforced by
	// the callers (lookup/store), not here — a 51-turn window
	// text must stay computable so tests can seed and assert
	// against it.
	long := make([]Message, 0, 51)
	for i := 0; i < 51; i++ {
		long = append(long, semWinMsg("user", fmt.Sprintf("Question %d?", i)))
	}
	if got, want := SemanticWindowText(long), "user: Question 48?\nuser: Question 49?\nuser: Question 50?"; got != want {
		t.Fatalf("SemanticWindowText(51 turns) = %q, want %q", got, want)
	}
}

func TestSemanticMessageText(t *testing.T) {
	if got := SemanticMessageText(semWinMsg("user", "hello")); got != "hello" {
		t.Fatalf("SemanticMessageText(Content) = %q, want %q", got, "hello")
	}
	parts := Message{Role: "user", Parts: []ContentPart{
		{Type: "text", Text: "Hello"},
		{Type: "image_url"},
		{Type: "text", Text: "there"},
	}}
	if got := SemanticMessageText(parts); got != "Hello there" {
		t.Fatalf("SemanticMessageText(parts) = %q, want %q (non-text parts skipped, joined with a space)", got, "Hello there")
	}
	if got := SemanticMessageText(semWinMsg("assistant", "")); got != "" {
		t.Fatalf("SemanticMessageText(empty) = %q, want empty", got)
	}
}

func TestSemanticTurnsExceeded(t *testing.T) {
	if SemanticWindowTurns != 3 {
		t.Fatalf("SemanticWindowTurns = %d, want 3", SemanticWindowTurns)
	}
	if SemanticMaxTurns != 50 {
		t.Fatalf("SemanticMaxTurns = %d, want 50 (OmniRoute conversationHistoryThreshold)", SemanticMaxTurns)
	}
	msgs := func(n int) []Message {
		out := make([]Message, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, semWinMsg("user", fmt.Sprintf("Turn %d?", i)))
		}
		return out
	}
	if SemanticTurnsExceeded(msgs(49)) {
		t.Fatal("49 turns must not exceed the cap")
	}
	if SemanticTurnsExceeded(msgs(50)) {
		t.Fatal("50 turns must not exceed the cap: the boundary is strictly greater than 50")
	}
	if !SemanticTurnsExceeded(msgs(51)) {
		t.Fatal("51 turns must exceed the cap")
	}
	// RAW count: system/developer messages count toward the 50,
	// matching OmniRoute's conv.length check on the unfiltered
	// messages array — 1 system + 50 user/assistant = 51 raw
	// bypasses, 1 system + 49 = 50 raw does not.
	withSys := func(nUser int) []Message {
		out := []Message{semWinMsg("system", "Be brief.")}
		return append(out, msgs(nUser)...)
	}
	if SemanticTurnsExceeded(withSys(49)) {
		t.Fatal("1 system + 49 user/assistant (50 raw) must not exceed the cap")
	}
	if !SemanticTurnsExceeded(withSys(50)) {
		t.Fatal("1 system + 50 user/assistant (51 raw) must exceed the cap: system messages count")
	}
}
