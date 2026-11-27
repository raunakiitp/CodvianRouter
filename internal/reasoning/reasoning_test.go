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
		// Cohere — replay is model-gated: its reasoning models
		// (command-a-reasoning family) accept the documented
		// unsigned thinking content blocks; other models have no
		// reasoning slot and stay unrepresentable.
		{"cohere reasoning model translates", thinkingState(), WireCohere, "command-a-reasoning-08-2025", StatusTranslated, false},
		{"cohere reasoning model thought translates", thought, WireCohere, "cohere/command-a-reasoning", StatusTranslated, false},
		{"cohere reasoning model signature-only drops", sigOnly, WireCohere, "command-a-reasoning-08-2025", StatusDropped, false},
		{"cohere reasoning model bedrock text translates", bedrockText, WireCohere, "command-a-reasoning-08-2025", StatusTranslated, false},
		{"cohere reasoning model bedrock redacted drops", bedrockRedacted, WireCohere, "command-a-reasoning-08-2025", StatusDropped, false},
		{"cohere non-reasoning model unrepresentable", thinkingState(), WireCohere, "command-r", StatusUnrepresentable, true},
		{"cohere non-reasoning model thought unrepresentable", thought, WireCohere, "command-r-plus", StatusUnrepresentable, true},
		{"cohere non-reasoning model signature-only drops", sigOnly, WireCohere, "command-r", StatusDropped, false},
		// Vertex — the Gemini shape, so Gemini's verdicts.
		{"vertex thinking translates", thinkingState(), WireVertex, "gemini-2.5-pro", StatusTranslated, false},
		{"vertex signature preserved", sigOnly, WireVertex, "publishers/google/models/gemini-2.5-pro", StatusPreserved, false},
		{"vertex bedrock text translates", bedrockText, WireVertex, "gemini-2.5-pro", StatusTranslated, false},
		{"vertex bedrock redacted drops", bedrockRedacted, WireVertex, "gemini-2.5-pro", StatusDropped, false},
		// Bedrock.
		{"bedrock verbatim", bedrockText, WireBedrock, "anthropic.claude-sonnet-4-5", StatusPreserved, false},
		{"bedrock redacted verbatim", bedrockRedacted, WireBedrock, "anthropic.claude-sonnet-4-5", StatusPreserved, false},
		{"bedrock thinking unrepresentable", thinkingState(), WireBedrock, "anthropic.claude-sonnet-4-5", StatusUnrepresentable, true},
		{"bedrock thought unrepresentable", thought, WireBedrock, "anthropic.claude-sonnet-4-5", StatusUnrepresentable, true},
		{"bedrock signature drops", sigOnly, WireBedrock, "anthropic.claude-sonnet-4-5", StatusDropped, false},
		// Unknown wires — the OpenAI-compatible passthrough verdict:
		// text carried opaquely for replay-capable models,
		// unrepresentable only for the provable no-slot case.
		{"unknown wire passthrough replay model translates", thought, "ollama", "deepseek-r1", StatusTranslated, false},
		{"unknown wire passthrough thinking translates", thinkingState(), "openai-compatible", "deepseek/deepseek-r1", StatusTranslated, false},
		{"unknown wire passthrough bedrock text translates", bedrockText, "ollama", "qwq-32b", StatusTranslated, false},
		{"unknown wire passthrough non-replay unrepresentable", thought, "ollama", "llama-3.3-70b", StatusUnrepresentable, true},
		{"unknown wire passthrough empty model unrepresentable", thought, "ollama", "", StatusUnrepresentable, true},
		{"unknown wire passthrough signature-only drops", sigOnly, "ollama", "deepseek-r1", StatusDropped, false},
		{"unknown wire passthrough redacted drops", &State{Blocks: []Block{{Kind: KindRedactedThinking, Data: json.RawMessage(`"e"`)}}}, "ollama", "deepseek-r1", StatusDropped, false},
		{"unknown block kind", &State{Blocks: []Block{{Kind: "mystery"}}}, WireGemini, "gemini", StatusUnrepresentable, true},
		{"unknown block kind cohere", &State{Blocks: []Block{{Kind: "mystery"}}}, WireCohere, "command-a-reasoning-08-2025", StatusUnrepresentable, true},
		{"unknown block kind vertex", &State{Blocks: []Block{{Kind: "mystery"}}}, WireVertex, "gemini-2.5-pro", StatusUnrepresentable, true},
		{"unknown block kind unknown wire", &State{Blocks: []Block{{Kind: "mystery"}}}, "ollama", "deepseek-r1", StatusUnrepresentable, true},
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

// The new per-wire strategies must obey the same no-leak rule.
func TestCheckDetailNeverCarriesContentNewWires(t *testing.T) {
	s := &State{Blocks: []Block{{Kind: KindThought, Text: "SECRET-THOUGHT-67890"}}}
	hops := []struct {
		wire, model string
	}{
		{WireCohere, "command-a-reasoning-08-2025"},
		{WireVertex, "gemini-2.5-pro"},
		{"ollama", "deepseek-r1"},
		{WireOpenAI, "deepseek-reasoner"},
	}
	for _, h := range hops {
		_, detail, err := Check(s, h.wire, h.model)
		if err != nil {
			t.Fatalf("Check(%s, %s): %v", h.wire, h.model, err)
		}
		if strings.Contains(detail, "SECRET-THOUGHT-67890") {
			t.Fatalf("detail on %s leaks reasoning text: %q", h.wire, detail)
		}
		if detail == "" {
			t.Fatalf("translated verdict on %s carries no detail", h.wire)
		}
	}
}

func TestCohereReplayModel(t *testing.T) {
	yes := []string{"command-a-reasoning-08-2025", "Command-A-Reasoning-08-2025", "cohere/command-a-reasoning", "command-a-reasoning"}
	for _, m := range yes {
		if !CohereReplayModel(m) {
			t.Errorf("CohereReplayModel(%q) = false, want true", m)
		}
	}
	no := []string{"command-r", "command-r-plus", "command-r7b-12-2024", "command-a-03-2025", "command-a-vision-07-2025", ""}
	for _, m := range no {
		if CohereReplayModel(m) {
			t.Errorf("CohereReplayModel(%q) = true, want false", m)
		}
	}
}

// The replay shape must be exactly the documented Cohere v2
// thinking content block: {"type":"thinking","thinking": ...}.
func TestCohereThinkingBlocks(t *testing.T) {
	s := &State{Blocks: []Block{
		{Kind: KindThinking, Text: "first thought", Signature: "sig-anthropic"},
		{Kind: KindThoughtSignature, Signature: "sig-g", Name: "get_weather"},
		{Kind: KindThought, Text: "second thought", Signature: "sig-gemini"},
		{Kind: KindRedactedThinking, Data: json.RawMessage(`"ZW5j"`)},
		{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"bedrock thought","signature":"br"}}}`)},
	}}
	blocks := s.CohereThinkingBlocks()
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 (text-bearing only)", len(blocks))
	}
	want := []string{"first thought", "second thought", "bedrock thought"}
	for i, b := range blocks {
		if b.Type != "thinking" {
			t.Fatalf("block %d type = %q, want \"thinking\"", i, b.Type)
		}
		if b.Thinking != want[i] {
			t.Fatalf("block %d thinking = %q, want %q", i, b.Thinking, want[i])
		}
	}
	// The documented shape, byte for byte.
	raw, err := json.Marshal(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != `{"type":"thinking","thinking":"first thought"}` {
		t.Fatalf("block 0 JSON = %s, want the documented v2 shape", got)
	}
	if (&State{}).CohereThinkingBlocks() != nil {
		t.Fatalf("empty state rendered blocks")
	}
	var nilState *State
	if nilState.CohereThinkingBlocks() != nil {
		t.Fatalf("nil state rendered blocks")
	}
}

// Placeholder injection for tool-call turns: the documented
// sentinel, and exactly the (wire, model) targets that admit an
// unsigned placeholder in their reasoning slot.
func TestToolCallPlaceholderInjection(t *testing.T) {
	cases := []struct {
		name  string
		wire  string
		model string
		want  bool
	}{
		{"openai replay model", WireOpenAI, "deepseek-reasoner", true},
		{"openai non-replay model", WireOpenAI, "gpt-4o", false},
		{"cohere reasoning model", WireCohere, "command-a-reasoning-08-2025", true},
		{"cohere non-reasoning model", WireCohere, "command-r", false},
		{"anthropic never (unsigned thinking rejected)", WireAnthropic, "claude-sonnet-4-5", false},
		{"gemini never (signature continuity)", WireGemini, "gemini-2.5-pro", false},
		{"vertex never (signature continuity)", WireVertex, "gemini-2.5-pro", false},
		{"bedrock never (signed reasoningText)", WireBedrock, "anthropic.claude-sonnet-4-5", false},
		{"unknown wire replay model", "ollama", "deepseek-r1", true},
		{"unknown wire non-replay model", "ollama", "llama-3.3-70b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ok := ToolCallPlaceholder(tc.wire, tc.model)
			if ok != tc.want {
				t.Fatalf("ok = %v, want %v", ok, tc.want)
			}
			if !ok {
				if st != nil {
					t.Fatalf("ok=false returned a state")
				}
				return
			}
			if st == nil || len(st.Blocks) != 1 {
				t.Fatalf("placeholder state = %+v, want one block", st)
			}
			b := st.Blocks[0]
			if b.Kind != KindThought || b.Text != PlaceholderText {
				t.Fatalf("block = %+v, want the documented sentinel thought", b)
			}
			// The sentinel is the documented bracketed form and
			// carries nothing but the marker.
			if !strings.HasPrefix(PlaceholderText, "[REASONING:") || !strings.HasSuffix(PlaceholderText, "]") {
				t.Fatalf("PlaceholderText %q is not the documented bracketed form", PlaceholderText)
			}
			// An admitted placeholder must pass Check on the same
			// target — the hop it rides can never fail because of it.
			status, detail, err := Check(st, tc.wire, tc.model)
			if err != nil || status != StatusTranslated {
				t.Fatalf("Check(placeholder, %s, %s) = %s, %v (detail %q)", tc.wire, tc.model, status, err, detail)
			}
			if st.ReasoningText() != PlaceholderText {
				t.Fatalf("ReasoningText = %q, want the sentinel", st.ReasoningText())
			}
		})
	}
}

// Turn-level handling for a response that contains ONLY reasoning
// content: keep the turn (an empty assistant turn with the
// reasoning attached), never render it, never fail it over; a
// response without text-bearing reasoning stays a plain empty
// completion.
func TestReasoningOnlyTurn(t *testing.T) {
	thoughtState := &State{Blocks: []Block{{Kind: KindThought, Text: "spent the whole budget thinking"}}}
	sigOnlyState := &State{Blocks: []Block{{Kind: KindThoughtSignature, Signature: "sig"}}}
	redactedState := &State{Blocks: []Block{{Kind: KindRedactedThinking, Data: json.RawMessage(`"e"`)}}}

	cases := []struct {
		name      string
		content   string
		toolCalls int
		state     *State
		isOnly    bool
		keep      bool
		visible   string
		capture   bool
	}{
		{"normal turn", "Sunny.", 0, thoughtState, false, true, "Sunny.", true},
		{"normal turn no reasoning", "Sunny.", 0, nil, false, true, "Sunny.", false},
		{"reasoning-only keeps turn", "", 0, thoughtState, true, true, "", true},
		{"reasoning-only thinking state", "", 0, thinkingState(), true, true, "", true},
		{"reasoning-only bedrock text", "", 0, &State{Blocks: []Block{{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"br"}}}`)}}}, true, true, "", true},
		{"opaque-only state is an empty completion", "", 0, sigOnlyState, false, false, "", false},
		{"redacted-only state is an empty completion", "", 0, redactedState, false, false, "", false},
		{"no state is an empty completion", "", 0, nil, false, false, "", false},
		{"empty state is an empty completion", "", 0, &State{}, false, false, "", false},
		{"tool-call turn is not reasoning-only", "", 1, thoughtState, false, true, "", true},
		{"tool-call turn without reasoning", "", 1, nil, false, true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ReasoningOnlyTurn(tc.content, tc.toolCalls, tc.state)
			if d.IsReasoningOnly != tc.isOnly || d.KeepTurn != tc.keep || d.VisibleContent != tc.visible || d.Capturable != tc.capture {
				t.Fatalf("decision = %+v, want isOnly=%v keep=%v visible=%q capture=%v", d, tc.isOnly, tc.keep, tc.visible, tc.capture)
			}
		})
	}
}

// No regression: the known wires that already replay keep their
// exact verdicts after the new strategies landed.
func TestKnownWireReplayUnchanged(t *testing.T) {
	hops := []struct {
		name       string
		state      *State
		wire       string
		model      string
		wantStatus Status
	}{
		{"anthropic verbatim", thinkingState(), WireAnthropic, "claude-sonnet-4-5", StatusPreserved},
		{"gemini translated", thinkingState(), WireGemini, "gemini-2.5-pro", StatusTranslated},
		{"gemini signature preserved", &State{Blocks: []Block{{Kind: KindThoughtSignature, Name: "get_weather", Signature: "sig-g"}}}, WireGemini, "gemini-2.5-pro", StatusPreserved},
		{"openai replay model translated", thinkingState(), WireOpenAI, "deepseek-reasoner", StatusTranslated},
		{"bedrock verbatim", &State{Blocks: []Block{{Kind: KindBedrock, Data: json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"t","signature":"s"}}}`)}}}, WireBedrock, "anthropic.claude-sonnet-4-5", StatusPreserved},
	}
	for _, h := range hops {
		t.Run(h.name, func(t *testing.T) {
			status, detail, err := Check(h.state, h.wire, h.model)
			if err != nil || status != h.wantStatus {
				t.Fatalf("Check(%s) = %s, %v (detail %q), want %s with no error", h.wire, status, err, detail, h.wantStatus)
			}
			if detail == "" {
				t.Fatalf("verdict on %s carries no detail", h.wire)
			}
		})
	}
}
