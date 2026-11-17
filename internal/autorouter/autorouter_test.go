package autorouter

import (
	"sort"
	"testing"
	"time"
)

func testRouter() *Router {
	return New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierCoding:    {"coder-model"},
			TierChat:      {"chat-model"},
			TierReasoning: {"reasoner-model"},
			TierQuickQA:   {"qa-model"},
		},
	})
}

func TestClassifyRepresentativePrompts(t *testing.T) {
	r := testRouter()
	cases := []struct {
		prompt string
		want   Tier
	}{
		{"Write a Python function to reverse a string", TierCoding},
		{"Fix the null pointer exception in my Java code", TierCoding},
		{"Write a SQL query to count orders by customer", TierCoding},
		{"Tell me a joke about cats", TierChat},
		{"Help me write an email to my team about the delay", TierChat},
		{"Summarize this article in a few sentences for me", TierChat},
		{"Solve this step by step: a train travels 120 km in 2 hours, what is its speed", TierReasoning},
		{"Compare the trade-offs of SQL versus NoSQL databases", TierReasoning},
		{"What year did humans first land on the moon", TierQuickQA},
		{"What is the boiling point of water at sea level", TierQuickQA},
		{"Who wrote the novel Pride and Prejudice", TierQuickQA},
	}
	for _, tc := range cases {
		dec := r.Classify("auto", tc.prompt, 0)
		// The tier must be right. A fallback is only a failure
		// when it would MISROUTE: the fallback tier for "auto"
		// is reasoning, so a reasoning prompt that falls back
		// still lands on the reasoning tier (safe by design) —
		// but a coding/chat/quick-qa prompt that falls back
		// would wrongly land on the strong model instead of its
		// cheap tier, so those must clear the threshold.
		if tc.want != DefaultTier && dec.Fallback {
			t.Errorf("prompt %q: fell back (confidence %.3f, threshold %.2f, scores %v); want tier %s",
				tc.prompt, dec.Confidence, dec.Threshold, dec.Scores, tc.want)
			continue
		}
		if dec.Tier != tc.want {
			t.Errorf("prompt %q: got tier %s (confidence %.3f, scores %v); want %s",
				tc.prompt, dec.Tier, dec.Confidence, dec.Scores, tc.want)
		}
		if dec.Confidence < DefaultThreshold && tc.want != DefaultTier {
			t.Errorf("prompt %q: confidence %.3f below default threshold", tc.prompt, dec.Confidence)
		}
		// Resolve must land on the right concrete model either
		// way — match or safe fallback.
		model, rdec, ok := r.Resolve("auto", tc.prompt)
		if !ok {
			t.Errorf("prompt %q: Resolve failed: %+v", tc.prompt, rdec)
			continue
		}
		wantModel := map[Tier]string{
			TierCoding: "coder-model", TierChat: "chat-model",
			TierReasoning: "reasoner-model", TierQuickQA: "qa-model",
		}[tc.want]
		if model != wantModel {
			t.Errorf("prompt %q: Resolve model %q; want %s (tier %s)", tc.prompt, model, wantModel, tc.want)
		}
	}
}

func TestClassifyUnrelatedPromptFallsBack(t *testing.T) {
	r := testRouter()
	for _, prompt := range []string{
		"zxqv blorp wumpus flibberty gibbet",
		"",
		"   ",
	} {
		dec := r.Classify("auto", prompt, 0)
		if !dec.Fallback {
			t.Errorf("prompt %q: got tier %s without fallback (confidence %.3f); unrelated/empty prompts must fall back, never guess",
				prompt, dec.Tier, dec.Confidence)
		}
		if dec.Tier != DefaultTier {
			t.Errorf("prompt %q: fallback tier %s; want %s", prompt, dec.Tier, DefaultTier)
		}
	}
}

func TestClassifyVirtualModelRestriction(t *testing.T) {
	r := testRouter()
	// A coding prompt under auto/fast may only pick quick-qa/chat, or
	// fall back to chat — never coding.
	dec := r.Classify("auto/fast", "Write a Python function to sort a list", 0)
	if dec.Tier != TierChat && dec.Tier != TierQuickQA {
		t.Errorf("auto/fast coding prompt: tier %s; want chat or quick-qa", dec.Tier)
	}
	// A chat prompt under auto/coding may only pick coding/reasoning,
	// or fall back to coding.
	dec = r.Classify("auto/coding", "Tell me a funny joke", 0)
	if dec.Tier != TierCoding && dec.Tier != TierReasoning {
		t.Errorf("auto/coding chat prompt: tier %s; want coding or reasoning", dec.Tier)
	}
	// Unknown virtual model classifies as "auto".
	dec = r.Classify("auto/unknown", "Write a Python function to sort a list", 0)
	if dec.VirtualModel != "auto" {
		t.Errorf("unknown virtual model: got %q; want \"auto\"", dec.VirtualModel)
	}
}

func TestClassifyThresholdOverride(t *testing.T) {
	r := testRouter()
	prompt := "Write a Python function to reverse a string"
	dec := r.Classify("auto", prompt, 0)
	if dec.Fallback {
		t.Fatalf("prompt %q fell back at default threshold; cannot test override", prompt)
	}
	// A threshold above the measured confidence must force fallback.
	decHigh := r.Classify("auto", prompt, 0.99)
	if !decHigh.Fallback {
		t.Errorf("threshold 0.99: got tier %s without fallback (confidence %.3f)", decHigh.Tier, decHigh.Confidence)
	}
	if decHigh.Threshold != 0.99 {
		t.Errorf("threshold override not honoured: got %.2f", decHigh.Threshold)
	}
	// Per-model threshold config: auto/fast at 0.99 forces fallback
	// for a prompt that only partially matches (an exact-duplicate
	// prompt scores 1.0 and can never fall back — that is correct,
	// not a bug: it is a verbatim example utterance).
	r2 := New(Config{Thresholds: map[string]float64{"auto/fast": 0.99}, DefaultModel: "strong-model"})
	dec2 := r2.Classify("auto/fast", "What is the boiling point of water at sea level", 0)
	if !dec2.Fallback {
		t.Errorf("per-model threshold 0.99: got tier %s without fallback (confidence %.3f)", dec2.Tier, dec2.Confidence)
	}
}

func TestResolve(t *testing.T) {
	r := testRouter()
	model, dec, ok := r.Resolve("auto", "Write a Python function to reverse a string")
	if !ok {
		t.Fatalf("Resolve failed: %+v", dec)
	}
	if model != "coder-model" {
		t.Errorf("Resolve coding prompt: model %q; want coder-model", model)
	}
	if dec.Model != model {
		t.Errorf("decision model %q != resolved %q", dec.Model, model)
	}
	// Fallback prompt resolves to the default tier's model.
	model, dec, ok = r.Resolve("auto", "zxqv blorp wumpus")
	if !ok {
		t.Fatalf("Resolve fallback failed: %+v", dec)
	}
	if !dec.Fallback || dec.Tier != TierReasoning {
		t.Errorf("fallback decision %+v; want fallback to reasoning", dec)
	}
	if model != "reasoner-model" {
		t.Errorf("fallback model %q; want reasoner-model", model)
	}
	// Unknown virtual model: ok=false, no invented model.
	if _, _, ok := r.Resolve("gpt-4o", "hello"); ok {
		t.Error("Resolve of a non-auto model reported ok=true")
	}
	// No model configured anywhere: ok=false, Model "".
	bare := New(Config{})
	if model, dec, ok := bare.Resolve("auto", "Write a Python function"); ok || model != "" {
		t.Errorf("bare router Resolve: ok=%v model=%q dec=%+v; want ok=false, empty model", ok, model, dec)
	}
	// Tier without TierModels falls back to DefaultModel.
	partial := New(Config{DefaultModel: "strong-model", TierModels: map[Tier][]string{TierCoding: {"coder-model"}}})
	model, _, ok = partial.Resolve("auto", "What is the capital of France")
	if !ok || model != "strong-model" {
		t.Errorf("tier without models: ok=%v model=%q; want ok=true, strong-model", ok, model)
	}
}

func TestParsePrompt(t *testing.T) {
	msgs := []PromptMessage{
		{Role: "system", Content: "Be helpful."},
		{Role: "user", Content: "First question"},
		{Role: "assistant", Content: "Answer"},
		{Role: "user", Content: "Second question"},
	}
	if got := ParsePrompt(msgs); got != "Second question" {
		t.Errorf("ParsePrompt = %q; want last user message", got)
	}
	if got := ParsePrompt(nil); got != "" {
		t.Errorf("ParsePrompt(nil) = %q; want empty", got)
	}
	parts := []PromptMessage{{Role: "user", TextParts: []string{"hello", "world"}}}
	if got := ParsePrompt(parts); got != "hello\nworld" {
		t.Errorf("ParsePrompt parts = %q; want joined text", got)
	}
}

func TestUtteranceDefaultsPresent(t *testing.T) {
	r := testRouter()
	for _, tier := range AllTiers {
		if n := r.UtteranceCount(tier); n == 0 {
			t.Errorf("tier %s has no utterances", tier)
		}
	}
	if got := len(VirtualModels); got != 5 {
		t.Errorf("VirtualModels = %d entries; want 5", got)
	}
	for _, name := range VirtualModels {
		if !IsVirtualModel(name) {
			t.Errorf("VirtualModels entry %q not recognised", name)
		}
	}
}

// TestClassifyLatency measures the routing-decision time (embed +
// score, no network, no LLM call) and reports the real numbers.
// Target is <= 5 ms per decision; the test fails if the MEAN
// exceeds it — a mean over 1000 decisions on this machine, not a
// single-sample claim.
func TestClassifyLatency(t *testing.T) {
	r := testRouter()
	prompt := "Write a Python function that parses JSON and handles errors gracefully"
	// Warm up.
	for i := 0; i < 100; i++ {
		r.Classify("auto", prompt, 0)
	}
	const n = 1000
	lat := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		r.Classify("auto", prompt, 0)
		lat[i] = time.Since(start)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	var total time.Duration
	for _, d := range lat {
		total += d
	}
	mean := total / n
	p50 := lat[n/2]
	p95 := lat[n*95/100]
	t.Logf("routing-decision latency over %d decisions: mean=%s p50=%s p95=%s max=%s",
		n, mean, p50, p95, lat[n-1])
	if mean > 5*time.Millisecond {
		t.Errorf("mean routing-decision latency %s exceeds 5ms target (p50=%s p95=%s)", mean, p50, p95)
	}
}

// BenchmarkClassify measures the same path under the benchmark
// harness, for `go test -bench`.
func BenchmarkClassify(b *testing.B) {
	r := testRouter()
	prompt := "Write a Python function that parses JSON and handles errors gracefully"
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Classify("auto", prompt, 0)
	}
}
