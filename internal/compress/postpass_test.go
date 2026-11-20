package compress

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

const postpassModel = "gpt-4o"

// countPrompt is the whole-prompt token count the budget is
// measured in. Tests compute budgets from it instead of hardcoding
// token numbers, so they pin behaviour, not tokenizer trivia.
func countPrompt(t *testing.T, msgs []provider.Message) int {
	t.Helper()
	c, err := tokencount.CountPrompt(postpassModel, msgs)
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	return c.Tokens
}

// words returns n repetitions of "word ", so rune-fraction
// truncation tracks token-fraction truncation closely (no
// pathological BPE merging).
func words(n int) string {
	return strings.Repeat("word ", n)
}

// singleLossyPass is the PRE-post-pass behaviour: one applyLossy
// call, no loop, no re-measure, no rollback. The toggle tests
// below assert HardBudget beats it; a HardBudget implemented as
// "one applyLossy call" would fail them.
func singleLossyPass(t *testing.T, msgs []provider.Message, budget int) ([]provider.Message, int) {
	t.Helper()
	out, _, _ := applyLossy(postpassModel, msgs, budget)
	return out, countPrompt(t, out)
}

func TestHardBudgetAlreadyFitsIsNoop(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "What is the capital of France?"},
	}
	budget := countPrompt(t, msgs)
	res := HardBudget(postpassModel, msgs, budget)
	if !res.Fitted {
		t.Fatalf("exact-budget prompt must fit, got %+v", res)
	}
	if !reflect.DeepEqual(res.Messages, msgs) {
		t.Fatalf("no-op must return input unchanged, got %+v", res.Messages)
	}
	if len(res.Steps) != 0 || res.Iterations != 0 {
		t.Fatalf("no-op must report no steps/iterations, got %+v", res)
	}
	if res.BeforeTokens != budget || res.AfterTokens != budget || res.Overage != 0 {
		t.Fatalf("counts wrong: %+v", res)
	}
}

func TestHardBudgetExactBoundaryFitAfterDrop(t *testing.T) {
	sys := provider.Message{Role: "system", Content: "You are helpful."}
	filler := provider.Message{Role: "user", Content: words(120)}
	last := provider.Message{Role: "user", Content: "What is the capital of France?"}
	msgs := []provider.Message{sys, filler, last}
	// Budget = exactly the prompt without the droppable middle
	// turn: dropping it lands ON the budget, not under it.
	budget := countPrompt(t, []provider.Message{sys, last})
	if budget >= countPrompt(t, msgs) {
		t.Fatalf("fixture: filler must cost tokens (budget %d vs total %d)", budget, countPrompt(t, msgs))
	}
	res := HardBudget(postpassModel, msgs, budget)
	if !res.Fitted {
		t.Fatalf("dropping the middle turn must fit exactly, got %+v", res)
	}
	if res.AfterTokens != budget {
		t.Fatalf("AfterTokens = %d, want exactly the budget %d", res.AfterTokens, budget)
	}
	if len(res.Messages) != 2 || res.Messages[0].Content != sys.Content || res.Messages[1].Content != last.Content {
		t.Fatalf("wrong survivors: %+v", res.Messages)
	}
	if !contains(res.Steps, StepHardBudgetDropped) {
		t.Fatalf("steps %v should name the drop", res.Steps)
	}
	if contains(res.Steps, StepHardBudgetTruncated) {
		t.Fatalf("steps %v must not claim a truncation: the drop alone fit", res.Steps)
	}
	// The reported AfterTokens is a real recount, markers and
	// all — not arithmetic.
	if got := countPrompt(t, res.Messages); got != res.AfterTokens {
		t.Fatalf("AfterTokens %d != recount %d", res.AfterTokens, got)
	}
}

func TestHardBudgetFitsByTruncation(t *testing.T) {
	sys := provider.Message{Role: "system", Content: words(60)}
	last := provider.Message{Role: "user", Content: words(120)}
	msgs := []provider.Message{sys, last}
	total := countPrompt(t, msgs)
	budget := total - 100
	res := HardBudget(postpassModel, msgs, budget)
	if !res.Fitted {
		t.Fatalf("truncation must fit a 100-token overage, got %+v", res)
	}
	if res.AfterTokens > budget {
		t.Fatalf("AfterTokens %d > budget %d", res.AfterTokens, budget)
	}
	if res.AfterTokens != countPrompt(t, res.Messages) {
		t.Fatalf("AfterTokens %d != recount %d", res.AfterTokens, countPrompt(t, res.Messages))
	}
	if !contains(res.Steps, StepHardBudgetTruncated) {
		t.Fatalf("steps %v should name the truncation", res.Steps)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("pinned messages must survive (truncated, not dropped): %+v", res.Messages)
	}
}

func TestHardBudgetHonestNonFitPinnedOnly(t *testing.T) {
	sys := provider.Message{Role: "system", Content: words(200)}
	last := provider.Message{Role: "user", Content: words(200)}
	msgs := []provider.Message{sys, last}
	// Budget far below what the pinned cores can reach: both
	// messages truncate to ~10-token cores plus a 9-token
	// marker each, still over budget.
	res := HardBudget(postpassModel, msgs, 20)
	if res.Fitted {
		t.Fatalf("pinned-only prompt must NOT fit a 20-token budget, got %+v", res)
	}
	if res.Overage != res.AfterTokens-20 {
		t.Fatalf("Overage %d != AfterTokens-budget %d", res.Overage, res.AfterTokens-20)
	}
	if res.Overage <= 0 {
		t.Fatalf("non-fit overage must be positive, got %+v", res)
	}
	if res.AfterTokens >= res.BeforeTokens {
		t.Fatalf("post-pass must still shrink the prompt: before %d after %d", res.BeforeTokens, res.AfterTokens)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("pinned messages must survive even in a non-fit: %+v", res.Messages)
	}
	if res.Messages[0].Role != "system" || res.Messages[1].Role != "user" {
		t.Fatalf("pinned roles must survive: %+v", res.Messages)
	}
	if res.Iterations > maxHardBudgetRounds {
		t.Fatalf("iterations %d exceed cap %d", res.Iterations, maxHardBudgetRounds)
	}
}

func TestHardBudgetMarkerCostsNineTokens(t *testing.T) {
	// Pins the number the post-pass's marker accounting rests
	// on: the truncation marker is 9 BPE tokens. If the
	// tokenizer ever changes, this test — not a silent drift —
	// says so.
	c, err := tokencount.Count(postpassModel, truncationMarker)
	if err != nil {
		t.Fatalf("Count(marker): %v", err)
	}
	if c.Tokens != 9 {
		t.Fatalf("truncation marker = %d tokens, want 9", c.Tokens)
	}
}

func TestHardBudgetSmallOverageFitsViaSqueeze(t *testing.T) {
	// Overage (2) smaller than the marker (9): a single lossy
	// pass truncates by 2, adds the 9-token marker, and INFLATES
	// the prompt. The post-pass must never keep that inflated
	// round — and must still fit: the squeeze phase re-aims at
	// budget-18, so its cut removes overage+18 tokens of text and
	// nets the 2 tokens the budget needs. The price is bounded
	// over-trim (up to two markers of extra text cut), which is
	// what buys the hard ≤ budget guarantee.
	sys := provider.Message{Role: "system", Content: words(60)}
	last := provider.Message{Role: "user", Content: words(60)}
	msgs := []provider.Message{sys, last}
	total := countPrompt(t, msgs)
	budget := total - 2

	bypassOut, bypassTotal := singleLossyPass(t, msgs, budget)
	if bypassTotal <= total {
		t.Fatalf("toggle fixture broken: single lossy pass did not inflate (bypass %d vs total %d); fix the fixture, not the test", bypassTotal, total)
	}
	_ = bypassOut

	res := HardBudget(postpassModel, msgs, budget)
	if !res.Fitted {
		t.Fatalf("squeeze must fit a 2-token overage, got %+v", res)
	}
	if res.AfterTokens > budget || res.AfterTokens > total {
		t.Fatalf("fit must be within budget and never inflate: AfterTokens %d budget %d total %d", res.AfterTokens, budget, total)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("pinned messages must survive (truncated, not dropped): %+v", res.Messages)
	}
	if !contains(res.Steps, StepHardBudgetTruncated) {
		t.Fatalf("steps %v should name the truncation", res.Steps)
	}
	// The squeeze over-trims by at most two markers: the fitted
	// total is within 18 tokens under the budget.
	if budget-res.AfterTokens > squeezeAllowance {
		t.Fatalf("squeeze over-trimmed by %d tokens (budget %d after %d); the bound is two markers (%d)", budget-res.AfterTokens, budget, res.AfterTokens, squeezeAllowance)
	}
}

func TestHardBudgetTinyPinnedMessageReportsNonFit(t *testing.T) {
	// A single tiny pinned message below the truncation core:
	// there is nothing extractive to do at all — no drop (it
	// is pinned), no truncation (cutting it would leave less
	// than its ~10-token core). The post-pass must return the
	// input unchanged with the exact overage, never an
	// inflated or gutted message, and the squeeze must not
	// rescue it by destroying the content.
	msgs := []provider.Message{{Role: "user", Content: "hi"}}
	total := countPrompt(t, msgs)
	if total <= 1 {
		t.Fatalf("fixture: tiny message must cost > 1 token, got %d", total)
	}
	res := HardBudget(postpassModel, msgs, 1)
	if res.Fitted {
		t.Fatalf("tiny pinned message must not fit a 1-token budget, got %+v", res)
	}
	if !reflect.DeepEqual(res.Messages, msgs) {
		t.Fatalf("no-progress non-fit must return the input unchanged, got %+v", res.Messages)
	}
	if res.AfterTokens != total || res.Overage != total-1 {
		t.Fatalf("counts wrong: AfterTokens %d (want %d), Overage %d (want %d)", res.AfterTokens, total, res.Overage, total-1)
	}
	if len(res.Steps) != 0 {
		t.Fatalf("no-progress non-fit must claim no steps, got %v", res.Steps)
	}
}

func TestHardBudgetLoopFitsWhereOnePassCannot(t *testing.T) {
	// Toggle for the loop itself: one applyLossy call leaves
	// the prompt over budget (its rune-fraction cut undershoots
	// by the marker it just added); the post-pass's second
	// round re-truncates the already-truncated message — whose
	// old marker sits in the cut middle, so no second marker is
	// added — and fits.
	sys := provider.Message{Role: "system", Content: words(60)}
	last := provider.Message{Role: "user", Content: words(200)}
	msgs := []provider.Message{sys, last}
	total := countPrompt(t, msgs)
	// 60 tokens over: round 1 cuts ~60 of text but adds the
	// 9-token marker, landing just over budget.
	budget := total - 60

	_, bypassTotal := singleLossyPass(t, msgs, budget)
	if bypassTotal <= budget {
		t.Fatalf("toggle fixture broken: single lossy pass already fits (bypass %d <= budget %d); fix the fixture, not the test", bypassTotal, budget)
	}

	res := HardBudget(postpassModel, msgs, budget)
	if !res.Fitted {
		t.Fatalf("post-pass loop must fit where one pass cannot, got %+v (bypass total %d)", res, bypassTotal)
	}
	if res.AfterTokens > budget || res.AfterTokens != countPrompt(t, res.Messages) {
		t.Fatalf("AfterTokens %d must be <= budget %d and match a recount", res.AfterTokens, budget)
	}
	if res.Iterations < 2 {
		t.Fatalf("fitting here must take >= 2 rounds, got %d", res.Iterations)
	}
}

func TestHardBudgetIdempotent(t *testing.T) {
	fitMsgs := []provider.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: words(120)},
		{Role: "user", Content: "What is the capital of France?"},
	}
	// Exact-boundary budget from the drop test: reuse its shape.
	fitBudget := countPrompt(t, []provider.Message{fitMsgs[0], fitMsgs[2]})
	first := HardBudget(postpassModel, fitMsgs, fitBudget)
	if !first.Fitted {
		t.Fatalf("setup: first pass must fit, got %+v", first)
	}
	second := HardBudget(postpassModel, first.Messages, fitBudget)
	if !reflect.DeepEqual(second.Messages, first.Messages) {
		t.Fatalf("second pass changed a fitted prompt: %+v vs %+v", second.Messages, first.Messages)
	}
	if !second.Fitted || len(second.Steps) != 0 {
		t.Fatalf("second pass on a fitted prompt must be a no-op fit, got %+v", second)
	}

	// And on a non-fit: the second pass starts from the
	// already-truncated cores, makes no progress, and returns
	// them unchanged.
	nofitMsgs := []provider.Message{
		{Role: "system", Content: words(200)},
		{Role: "user", Content: words(200)},
	}
	nofitFirst := HardBudget(postpassModel, nofitMsgs, 20)
	if nofitFirst.Fitted {
		t.Fatalf("setup: pinned-only prompt must not fit, got %+v", nofitFirst)
	}
	nofitSecond := HardBudget(postpassModel, nofitFirst.Messages, 20)
	if !reflect.DeepEqual(nofitSecond.Messages, nofitFirst.Messages) {
		t.Fatalf("second pass changed a non-fit prompt: %+v vs %+v", nofitSecond.Messages, nofitFirst.Messages)
	}
	if nofitSecond.Fitted || nofitSecond.AfterTokens != nofitFirst.AfterTokens || nofitSecond.Overage != nofitFirst.Overage {
		t.Fatalf("second pass must report the same non-fit, got %+v vs %+v", nofitSecond, nofitFirst)
	}
}

func TestHardBudgetNoProgressTerminates(t *testing.T) {
	// Pinned-only, tiny budget, part-carrying messages that
	// can never be truncated: there is nothing to do at all,
	// so the loop must end after the first no-progress round
	// instead of spinning.
	msgs := []provider.Message{
		{Role: "system", Content: words(80)},
		{Role: "user", Content: "answer", Parts: []provider.ContentPart{{Type: "text", Text: "answer"}}},
	}
	res := HardBudget(postpassModel, msgs, 10)
	if res.Fitted {
		t.Fatalf("must not fit, got %+v", res)
	}
	if res.Iterations > maxHardBudgetRounds {
		t.Fatalf("iterations %d exceed cap %d", res.Iterations, maxHardBudgetRounds)
	}
	if res.Iterations > 8 {
		t.Fatalf("no-progress loop must end quickly, took %d iterations", res.Iterations)
	}
	if res.Overage != res.AfterTokens-10 {
		t.Fatalf("Overage %d != AfterTokens-budget %d", res.Overage, res.AfterTokens-10)
	}
}

func TestHardBudgetFreezeSystemPromptFitsViaOthers(t *testing.T) {
	// System prompt is the longest message, so an unfrozen
	// pass would truncate it first. Frozen, the cut must come
	// from the last user turn instead — and the fit must still
	// happen.
	sys := provider.Message{Role: "system", Content: words(300)}
	last := provider.Message{Role: "user", Content: words(200)}
	msgs := []provider.Message{sys, last}
	total := countPrompt(t, msgs)
	budget := total - 60

	res := HardBudgetWithOptions(postpassModel, msgs, budget, HardBudgetOptions{FreezeSystemPrompt: true})
	if !res.Fitted {
		t.Fatalf("frozen pass must fit by cutting the user turn, got %+v", res)
	}
	if res.Messages[0].Content != sys.Content {
		t.Fatalf("frozen system prompt must be byte-identical, got %q", res.Messages[0].Content)
	}
	if !reflect.DeepEqual(res.Messages[0], sys) {
		t.Fatalf("frozen system message must survive whole (role/parts included): %+v", res.Messages[0])
	}
	if res.Messages[1].Content == last.Content {
		t.Fatalf("the cut must have come from the user turn, which is unchanged")
	}
	if !strings.Contains(res.Messages[1].Content, truncationMarker) {
		t.Fatalf("user turn should carry the truncation marker, got %q", res.Messages[1].Content)
	}
}

func TestHardBudgetFreezeSystemPromptHonestNonFit(t *testing.T) {
	// Frozen system prompt alone exceeds the budget: no cut
	// anywhere else can fix that, and the system prompt must
	// still come back byte-identical with the exact overage.
	sys := provider.Message{Role: "system", Content: words(300)}
	last := provider.Message{Role: "user", Content: words(60)}
	msgs := []provider.Message{sys, last}
	sysTokens := countPrompt(t, []provider.Message{sys})
	budget := sysTokens - 10 // system alone is 10 over
	res := HardBudgetWithOptions(postpassModel, msgs, budget, HardBudgetOptions{FreezeSystemPrompt: true})
	if res.Fitted {
		t.Fatalf("frozen system alone over budget must not fit, got %+v", res)
	}
	if res.Messages[0].Content != sys.Content {
		t.Fatalf("frozen system prompt must be byte-identical even in a non-fit, got %q", res.Messages[0].Content)
	}
	if res.Overage != res.AfterTokens-budget {
		t.Fatalf("Overage %d != AfterTokens-budget %d", res.Overage, res.AfterTokens-budget)
	}
	// Without the freeze the same prompt DOES fit (the system
	// prompt gets truncated): the freeze is what costs the
	// fit, and the difference is honest, not a bug.
	unfrozen := HardBudget(postpassModel, msgs, budget)
	if !unfrozen.Fitted {
		t.Fatalf("control: unfrozen pass must fit by truncating the system prompt, got %+v", unfrozen)
	}
	if reflect.DeepEqual(unfrozen.Messages[0], sys) {
		t.Fatalf("control: unfrozen pass must have truncated the system prompt, got %+v", unfrozen.Messages[0])
	}
}

func TestHardBudgetDoesNotMutateInput(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: words(100)},
		{Role: "user", Content: words(100)},
		{Role: "user", Content: words(100)},
	}
	want := append([]provider.Message(nil), msgs...)
	res := HardBudget(postpassModel, msgs, 50)
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("HardBudget mutated its input: %+v vs %+v", msgs, want)
	}
	_ = res
	frozen := HardBudgetWithOptions(postpassModel, msgs, 50, HardBudgetOptions{FreezeSystemPrompt: true})
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("frozen HardBudget mutated its input: %+v vs %+v", msgs, want)
	}
	_ = frozen
}

func TestHardBudgetEmptyPrompt(t *testing.T) {
	res := HardBudget(postpassModel, nil, 100)
	if !res.Fitted || res.AfterTokens != 0 {
		t.Fatalf("empty prompt must fit, got %+v", res)
	}
	res = HardBudget(postpassModel, []provider.Message{}, 0)
	if !res.Fitted {
		t.Fatalf("empty prompt at budget 0 must fit, got %+v", res)
	}
}
