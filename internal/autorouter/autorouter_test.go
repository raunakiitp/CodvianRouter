package autorouter

import (
	"math"
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
	if got := len(VirtualModels); got != 6 {
		t.Errorf("VirtualModels = %d entries; want 6", got)
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

// TestRankModelsReordersTier tests the cost/latency-aware tier
// ordering: a ranker that puts the cheapest model first must win
// over list order, and the decision must carry the ranked model.
func TestRankModelsReordersTier(t *testing.T) {
	reverse := func(tier Tier, models []string) []string {
		out := append([]string(nil), models...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	r := New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierQuickQA: {"expensive-model", "cheap-model"},
		},
		RankModels: reverse,
	})
	model, dec, ok := r.Resolve("auto/cheap", "What is two plus two")
	if !ok {
		t.Fatalf("Resolve failed: %+v", dec)
	}
	if model != "cheap-model" {
		t.Errorf("Resolve = %q (want cheap-model: ranker must beat list order)", model)
	}
	if dec.Model != "cheap-model" {
		t.Errorf("decision model = %q (want cheap-model)", dec.Model)
	}
}

// TestRankModelsBadRankerFallsBack proves a ranker that drops,
// duplicates or panics can never break routing: list order stands.
func TestRankModelsBadRankerFallsBack(t *testing.T) {
	cases := map[string]ModelRanker{
		"drops":      func(tier Tier, models []string) []string { return models[:1] },
		"duplicates": func(tier Tier, models []string) []string { return []string{models[0], models[0]} },
		"panics":     func(tier Tier, models []string) []string { panic("ranker blew up") },
		"nil-out":    func(tier Tier, models []string) []string { return nil },
	}
	for name, ranker := range cases {
		r := New(Config{
			DefaultModel: "strong-model",
			TierModels: map[Tier][]string{
				TierQuickQA: {"first-model", "second-model"},
			},
			RankModels: ranker,
		})
		model, _, ok := r.Resolve("auto/cheap", "What is two plus two")
		if !ok {
			t.Fatalf("%s: Resolve failed", name)
		}
		if model != "first-model" {
			t.Errorf("%s: Resolve = %q (want first-model: bad ranker falls back to list order)", name, model)
		}
	}
}

// TestRankModelsNilKeepsListOrder is the no-ranker baseline: first
// listed model wins.
func TestRankModelsNilKeepsListOrder(t *testing.T) {
	r := New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierQuickQA: {"first-model", "second-model"},
		},
	})
	model, _, ok := r.Resolve("auto/cheap", "What is two plus two")
	if !ok || model != "first-model" {
		t.Errorf("Resolve = %q, %v (want first-model, true)", model, ok)
	}
}

// TestRegisterTierModelsLiveAdd proves the live-add path: a model
// registered after construction becomes a candidate of every tier
// (so a live-added provider's models can win tier ordering), with
// no duplicates on repeat registration, and Resolve then honours
// the ranker over the new candidate list.
func TestRegisterTierModelsLiveAdd(t *testing.T) {
	reverse := func(tier Tier, models []string) []string {
		out := append([]string(nil), models...)
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
		return out
	}
	r := New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierQuickQA: {"expensive-model"},
		},
		RankModels: reverse,
	})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 1 || got[0] != "expensive-model" {
		t.Fatalf("ModelsForTier(quick-qa) = %v, want [expensive-model]", got)
	}
	// Empty and blank registrations are no-ops.
	r.RegisterTierModels(nil)
	r.RegisterTierModels([]string{""})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 1 {
		t.Fatalf("empty RegisterTierModels changed tiers: %v", got)
	}
	// A live-added model joins every tier, appended after the
	// configured entries.
	r.RegisterTierModels([]string{"live-model"})
	for _, tier := range AllTiers {
		found := false
		for _, m := range r.ModelsForTier(tier) {
			if m == "live-model" {
				found = true
			}
		}
		if !found {
			t.Errorf("tier %s has no live-model after registration: %v", tier, r.ModelsForTier(tier))
		}
	}
	if got := r.ModelsForTier(TierQuickQA); len(got) != 2 || got[0] != "expensive-model" || got[1] != "live-model" {
		t.Errorf("ModelsForTier(quick-qa) = %v, want [expensive-model live-model]", got)
	}
	// Re-registering the same model (even twice in one call)
	// never duplicates it.
	r.RegisterTierModels([]string{"live-model", "live-model"})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 2 {
		t.Errorf("duplicate RegisterTierModels duplicated entries: %v", got)
	}
	// The ranker (reverse) now sees the new candidate and picks it.
	model, _, ok := r.Resolve("auto/cheap", "What is two plus two")
	if !ok || model != "live-model" {
		t.Errorf("Resolve = %q, %v (want live-model, true: ranker must see live-registered models)", model, ok)
	}
}

// TestRankModelsCheapestPairWins proves the auto-router tier
// ordering ranks on the (provider, model) pair price: the tier's
// models are ordered by each model's cheapest pair, so a model
// that is $60 at one provider but $1 at another beats a $5 model
// — the pair the router would actually serve it through wins,
// not the expensive pairing and not list order.
func TestRankModelsCheapestPairWins(t *testing.T) {
	// Pair prices: model -> provider -> USD per 1M (blended).
	pairPrice := map[string]map[string]float64{
		"expensive-model": {"expensive-provider": 60},
		"cheap-model":     {"expensive-provider": 60, "cheap-provider": 1},
		"mid-model":       {"only-provider": 5},
	}
	cheapestPair := func(tier Tier, models []string) []string {
		out := append([]string(nil), models...)
		best := func(model string) float64 {
			best := math.Inf(1)
			for _, v := range pairPrice[model] {
				if v < best {
					best = v
				}
			}
			return best
		}
		sort.SliceStable(out, func(i, j int) bool { return best(out[i]) < best(out[j]) })
		return out
	}
	r := New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierQuickQA: {"expensive-model", "mid-model", "cheap-model"},
		},
		RankModels: cheapestPair,
	})
	model, dec, ok := r.Resolve("auto/cheap", "What is two plus two")
	if !ok {
		t.Fatalf("Resolve failed: %+v", dec)
	}
	if model != "cheap-model" {
		t.Errorf("Resolve = %q (want cheap-model: its $1 pair beats mid-model's $5 and expensive-model's $60)", model)
	}
}

// TestUnregisterTierModels is the delete counterpart of
// TestRegisterTierModelsLiveAdd: models leave every tier, the rest
// stay put, and unknown/empty removals are no-ops. The caller
// (main.go's provider-removed hook) decides which models to drop —
// only models no remaining provider serves — because tiers hold
// model IDs, not (provider, model) pairs.
func TestUnregisterTierModels(t *testing.T) {
	r := New(Config{
		DefaultModel: "strong-model",
		TierModels: map[Tier][]string{
			TierQuickQA:   {"a-model", "b-model"},
			TierReasoning: {"b-model", "c-model"},
		},
	})
	r.RegisterTierModels([]string{"live-model"})
	// Empty and blank removals are no-ops.
	r.UnregisterTierModels(nil)
	r.UnregisterTierModels([]string{""})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 3 {
		t.Fatalf("empty UnregisterTierModels changed tiers: %v", got)
	}
	// Removing an unknown model is a no-op.
	r.UnregisterTierModels([]string{"no-such-model"})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 3 {
		t.Fatalf("unknown UnregisterTierModels changed tiers: %v", got)
	}
	// Removing b-model drops it from BOTH tiers; the rest stay.
	r.UnregisterTierModels([]string{"b-model", "live-model"})
	if got := r.ModelsForTier(TierQuickQA); len(got) != 1 || got[0] != "a-model" {
		t.Errorf("ModelsForTier(quick-qa) = %v, want [a-model]", got)
	}
	if got := r.ModelsForTier(TierReasoning); len(got) != 1 || got[0] != "c-model" {
		t.Errorf("ModelsForTier(reasoning) = %v, want [c-model]", got)
	}
	// Removing it again is a no-op, and the ranker no longer
	// resolves to it.
	r.UnregisterTierModels([]string{"b-model"})
	if got := r.ModelsForTier(TierReasoning); len(got) != 1 {
		t.Errorf("second UnregisterTierModels changed tiers: %v", got)
	}
	model, _, ok := r.Resolve("auto/cheap", "What is two plus two")
	if !ok || model == "b-model" {
		t.Errorf("Resolve = %q, %v (want a remaining model, true)", model, ok)
	}
}
