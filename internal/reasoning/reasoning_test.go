package reasoning

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func thinkingState() *State {
	return &State{
		Blocks: []Block{
			{Kind: KindThinking, Text: "let me think", Signature: "sig-1"},
			{Kind: KindRedactedThinking, Data: json.RawMessage(`"ZW5jcnlwdGVk"`)},
		},
		SourceWire: WireAnthropic, SourceModel: "claude-sonnet-4-5",
	}
}

func TestCheckMatrix(t *testing.T) {
	thought := &State{Blocks: []Block{{Kind: KindThought, Text: "because x"}}}
	sigOnly := &State{Blocks: []Block{{Kind: KindThoughtSignature, Name: "get_weather", Signature: "sig-g"}}}
	bedrockText := &State{Blocks: []Block{{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"br think","signature":"br-sig"}}}`)}}}
	bedrockRedacted := &State{Blocks: []Block{{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"redactedContent":"aGVsbG8="}`)}}}
	empty := &State{}

	cases := []struct {
		name    string
		state   *State
		wire    string
		model   string
		status  Status
		wantErr bool
	}{
		{"nil state", nil, WireAnthropic, "claude", StatusNone, false},
		{"empty state", empty, WireAnthropic, "claude", StatusNone, false},
		// Anthropic.
		{"anthropic thinking verbatim", thinkingState(), WireAnthropic, "claude-sonnet-4-5", StatusPreserved, false},
		{"anthropic thought text unrepresentable", thought, WireAnthropic, "claude", StatusUnrepresentable, true},
		{"anthropic signature-only drops", sigOnly, WireAnthropic, "claude", StatusDropped, false},
		{"anthropic bedrock text unrepresentable", bedrockText, WireAnthropic, "claude", StatusUnrepresentable, true},
		{"anthropic bedrock redacted drops", bedrockRedacted, WireAnthropic, "claude", StatusDropped, false},
		{"anthropic thinking without signature", &State{Blocks: []Block{{Kind: KindThinking, Text: "x"}}}, WireAnthropic, "claude", StatusUnrepresentable, true},
		// Gemini.
		{"gemini thinking translates", thinkingState(), WireGemini, "gemini-2.5-pro", StatusTranslated, false},
		{"gemini thought translates", thought, WireGemini, "gemini-2.5-pro", StatusTranslated, false},
		{"gemini signature preserved", sigOnly, WireGemini, "gemini-2.5-pro", StatusPreserved, false},
		{"gemini bedrock text translates", bedrockText, WireGemini, "gemini-2.5-pro", StatusTranslated, false},
		{"gemini bedrock redacted drops", bedrockRedacted, WireGemini, "gemini-2.5-pro", StatusDropped, false},
		// OpenAI wire.
		{"openai deepseek translates", thinkingState(), WireOpenAI, "deepseek-reasoner", StatusTranslated, false},
		{"openai deepseek-r1 translates", thought, WireOpenAI, "deepseek/deepseek-r1", StatusTranslated, false},
		{"openai qwq translates", thought, WireOpenAI, "qwq-32b", StatusTranslated, false},
		{"openai gpt-4o unrepresentable", thinkingState(), WireOpenAI, "gpt-4o", StatusUnrepresentable, true},
		{"openai gpt-4o thought unrepresentable", thought, WireOpenAI, "gpt-4o", StatusUnrepresentable, true},
		{"openai gpt-4o signature-only drops", sigOnly, WireOpenAI, "gpt-4o", StatusDropped, false},
		{"openai empty model unrepresentable", thought, WireOpenAI, "", StatusUnrepresentable, true},
		// Cohere.
		{"cohere anything unrepresentable", thinkingState(), WireCohere, "command-r", StatusUnrepresentable, true},
		{"cohere signature unrepresentable", sigOnly, WireCohere, "command-r", StatusUnrepresentable, true},
		// Bedrock.
		{"bedrock verbatim", bedrockText, WireBedrock, "anthropic.claude-sonnet-4-5", StatusPreserved, false},
		{"bedrock redacted verbatim", bedrockRedacted, WireBedrock, "anthropic.claude-sonnet-4-5", StatusPreserved, false},
		{"bedrock thinking unrepresentable", thinkingState(), WireBedrock, "anthropic.claude-sonnet-4-5", StatusUnrepresentable, true},
		{"bedrock thought unrepresentable", thought, WireBedrock, "anthropic.claude-sonnet-4-5", StatusUnrepresentable, true},
		{"bedrock signature drops", sigOnly, WireBedrock, "anthropic.claude-sonnet-4-5", StatusDropped, false},
		// Unknown.
		{"unknown wire", thought, "ollama", "", StatusUnrepresentable, true},
		{"unknown block kind", &State{Blocks: []Block{{Kind: "mystery"}}}, WireGemini, "gemini", StatusUnrepresentable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, detail, err := Check(tc.state, tc.wire, tc.model)
			if status != tc.status {
				t.Fatalf("status = %q, want %q (detail %q, err %v)", status, tc.status, detail, err)
			}
			if tc.wantErr && err == nil {
				t.Fatalf("expected an error, got nil (detail %q)", detail)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr && !IsUnrepresentable(err) {
				t.Fatalf("error is not *UnrepresentableError: %T %v", err, err)
			}
			if status != StatusNone && status != StatusUnrepresentable && detail == "" {
				t.Fatalf("non-empty verdict carries no detail")
			}
			if label := Describe(status, detail); tc.status == StatusNone && label != "" {
				t.Fatalf("none status must describe as empty, got %q", label)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	if got := Describe(StatusPreserved, "2 thinking block(s) replayed verbatim"); got != "preserved: 2 thinking block(s) replayed verbatim" {
		t.Fatalf("Describe preserved = %q", got)
	}
	if got := Describe(StatusTranslated, ""); got != "translated" {
		t.Fatalf("Describe translated bare = %q", got)
	}
	if got := Describe(StatusNone, "x"); got != "" {
		t.Fatalf("Describe none = %q", got)
	}
}

func TestReplayModel(t *testing.T) {
	yes := []string{"deepseek-reasoner", "deepseek-r1", "deepseek/deepseek-r1", "deepseek-chat",
		"deepseek-v4-flash", "deepseek-v4-pro-free", "zen/deepseek-v4", "kimi-k2", "kimi/k2",
		"qwq-32b", "Qwen3-235B-Thinking", "glm-4.5-think", "mimo-v2-flash"}
	for _, m := range yes {
		if !ReplayModel(m) {
			t.Errorf("ReplayModel(%q) = false, want true", m)
		}
	}
	no := []string{"gpt-4o", "gpt-5", "o1", "o3-mini", "claude-sonnet-4-5", "gemini-2.5-pro", "llama-3.3-70b", "", "deepseek", "qwen2.5-72b-instruct"}
	for _, m := range no {
		if ReplayModel(m) {
			t.Errorf("ReplayModel(%q) = true, want false", m)
		}
	}
}

func TestReasoningText(t *testing.T) {
	s := &State{Blocks: []Block{
		{Kind: KindThinking, Text: "first"},
		{Kind: KindThoughtSignature, Signature: "sig"},
		{Kind: KindThought, Text: "second"},
		{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"third","signature":"s"}}}`)},
		{Kind: KindRedactedThinking, Data: json.RawMessage(`"e"`)},
	}}
	if got := s.ReasoningText(); got != "first\n\nsecond\n\nthird" {
		t.Fatalf("ReasoningText = %q", got)
	}
	if got := (&State{}).ReasoningText(); got != "" {
		t.Fatalf("empty ReasoningText = %q", got)
	}
}

func TestStateCloneIsDeep(t *testing.T) {
	s := thinkingState()
	c := s.Clone()
	c.Blocks[0].Text = "mutated"
	c.Blocks[1].Data = json.RawMessage(`"bW9jay"`)
	if s.Blocks[0].Text != "let me think" {
		t.Fatalf("clone shares Text with the original")
	}
	if string(s.Blocks[1].Data) != `"ZW5jcnlwdGVk"` {
		t.Fatalf("clone shares Data with the original: %s", s.Blocks[1].Data)
	}
}

func TestTranscriptKey(t *testing.T) {
	turns := []Turn{
		{Role: "user", Text: "hi"},
		{Role: "assistant", Text: "hello", ToolCalls: []ToolCallRef{{ID: "call_1", Name: "get_weather", Arguments: `{"city":"Paris"}`}}},
	}
	a := TranscriptKey("key-1", turns)
	b := TranscriptKey("key-1", turns)
	if a != b || a == "" {
		t.Fatalf("key is not deterministic: %q vs %q", a, b)
	}
	if TranscriptKey("key-2", turns) == a {
		t.Fatalf("different caller produced the same key")
	}
	if TranscriptKey("key-1", turns[:1]) == a {
		t.Fatalf("different transcript produced the same key")
	}
	// Reasoning state is not part of the key: capture and
	// injection must meet even when one side stripped it.
	if TranscriptKey("", nil) == TranscriptKey("", []Turn{}) {
		// nil and empty transcripts are the same transcript.
	} else {
		t.Fatalf("nil vs empty transcript keys differ")
	}
}

func TestStorePutGetStats(t *testing.T) {
	s := New()
	key := TranscriptKey("k", []Turn{{Role: "user", Text: "hi"}})
	if _, ok := s.Get(key); ok {
		t.Fatalf("empty store hit")
	}
	st := thinkingState()
	s.Put(key, *st)
	got, ok := s.Get(key)
	if !ok {
		t.Fatalf("stored turn missed")
	}
	if len(got.Blocks) != 2 || got.Blocks[0].Signature != "sig-1" {
		t.Fatalf("stored state came back wrong: %+v", got)
	}
	// Mutating the returned state must not mutate the store.
	got.Blocks[0].Text = "mutated"
	got2, _ := s.Get(key)
	if got2.Blocks[0].Text != "let me think" {
		t.Fatalf("Get returned a live reference to the stored state")
	}
	stats := s.Stats()
	if stats.Entries != 1 || stats.Captures != 1 || stats.Hits != 2 || stats.Misses != 1 || stats.Injected != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	s.NoteInjected()
	if s.Stats().Injected != 1 {
		t.Fatalf("injected not counted")
	}
	// Empty states are never stored.
	s.Put(TranscriptKey("k", []Turn{{Role: "user", Text: "x"}}), State{})
	if s.Len() != 1 {
		t.Fatalf("empty state stored, entries = %d", s.Len())
	}
}

func TestStoreTTLAndEviction(t *testing.T) {
	s := NewStore(50*time.Millisecond, 2)
	s.Put("a", *thinkingState())
	s.Put("b", *thinkingState())
	s.Put("c", *thinkingState())
	if s.Len() != 2 {
		t.Fatalf("cap not enforced: entries = %d", s.Len())
	}
	if _, ok := s.Get("a"); ok {
		t.Fatalf("oldest entry survived eviction")
	}
	if s.Stats().Evicted != 1 {
		t.Fatalf("evicted = %d, want 1", s.Stats().Evicted)
	}
	time.Sleep(80 * time.Millisecond)
	if _, ok := s.Get("b"); ok {
		t.Fatalf("expired entry survived")
	}
	if s.Stats().Misses == 0 {
		t.Fatalf("expired lookup did not count as a miss")
	}
	// A repeat Put for the same key replaces without growing.
	s2 := NewStore(time.Hour, 10)
	s2.Put("x", *thinkingState())
	s2.Put("x", *thinkingState())
	if s2.Len() != 1 || s2.Stats().Captures != 2 {
		t.Fatalf("repeat put: len=%d captures=%d", s2.Len(), s2.Stats().Captures)
	}
}

func TestCheckDetailNeverCarriesContent(t *testing.T) {
	// The detail must name kinds and wires only: the reasoning
	// text itself must never appear in it (it would reach the
	// route trace and the dashboard).
	s := &State{Blocks: []Block{{Kind: KindThinking, Text: "SECRET-THOUGHT-12345", Signature: "s"}}}
	_, detail, _ := Check(s, WireGemini, "gemini-2.5-pro")
	if strings.Contains(detail, "SECRET-THOUGHT-12345") {
		t.Fatalf("detail leaks reasoning text: %q", detail)
	}
}
