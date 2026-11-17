// Package autorouter is the local auto-router: it picks which tier
// (and therefore which concrete model) should serve a prompt, in
// milliseconds, with NO extra LLM call and NO network call.
//
// HOW IT WORKS (semantic-router / RouteLLM pattern, local edition):
// the prompt is embedded once with the same deterministic hashed
// bag-of-words embedding the semantic cache uses
// (cache.EmbedText — token unigrams + bigrams hashed into 256
// buckets, L2-normalized), then cosine-matched against curated
// example utterances shipped per tier. The tier whose utterances
// (best single utterance, or the tier's aggregate centroid) score
// highest wins. This is token-overlap similarity,
// NOT a neural embedding model and NOT semantic understanding:
// prompts that share words with an utterance score high, synonyms
// and paraphrases with different words score low. That limitation
// is why the threshold design below is calibrated the way it is.
//
// CALIBRATED THRESHOLD DESIGN — read before tuning the dial:
// a single probability/threshold dial (default 0.5, configurable
// per virtual model) guards every decision. Below the threshold the
// router NEVER guesses: it falls back to the operator's default
// tier/strong model and says so in the Decision (Fallback=true,
// Reason names the confidence and the threshold). A low threshold
// routes more prompts to cheap tiers but risks misrouting; a high
// threshold is safer and routes more to the strong model. The
// default 0.5 was chosen against the shipped utterance sets (see
// the tests: representative prompts for each tier score above it,
// unrelated prompts score below it).
//
// VIRTUAL MODELS: clients request "auto", "auto/fast", "auto/cheap",
// "auto/coding" or "auto/reasoning". Each names a candidate tier set
// and a fallback tier; the concrete model comes from TierModels
// (operator-configured, first entry wins) or DefaultModel. When no
// concrete model can be resolved the decision carries Model "" and
// Resolve reports ok=false — the caller must surface an honest
// error, never invent a model.
//
// The shipped utterance sets are CONFIG DEFAULTS, not measured
// traffic and not results: they are the examples the matcher
// compares against, labelled as such wherever they are shown.
package autorouter

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
)

// Tier is one routing tier: a class of prompt the router can tell
// apart by token overlap, each mapped to concrete models.
type Tier string

// Tiers.
const (
	// TierCoding is code work: writing, fixing, refactoring,
	// reviewing, debugging, tests.
	TierCoding Tier = "coding"
	// TierChat is open-ended conversation: greetings, chat, email
	// drafting, summaries, opinions, translation.
	TierChat Tier = "chat"
	// TierReasoning is multi-step thinking: proofs, puzzles,
	// trade-off analysis, planning, derivation.
	TierReasoning Tier = "reasoning"
	// TierQuickQA is short factual questions with short answers.
	TierQuickQA Tier = "quick-qa"
)

// AllTiers lists every tier in a stable order.
var AllTiers = []Tier{TierCoding, TierChat, TierReasoning, TierQuickQA}

// IsTier reports whether s names a known tier.
func IsTier(s string) bool {
	for _, t := range AllTiers {
		if Tier(s) == t {
			return true
		}
	}
	return false
}

// DefaultThreshold is the confidence a tier match must reach to be
// trusted (0.5). Below it the router falls back to the default tier
// instead of guessing.
const DefaultThreshold = 0.5

// DefaultTier is the fallback tier when no tier clears the
// threshold: the strong/reasoning tier, i.e. the safe choice.
const DefaultTier = TierReasoning

// ---------------------------------------------------------------------------
// Curated default utterances.
//
// These are CONFIG DEFAULTS shipped so the user never has to write
// utterances: each tier's examples cover the common phrasings of
// that kind of prompt. They are not measured traffic, not benchmark
// results, and must never be presented as either. Operators may
// replace them (RouterConfig.Utterances); an empty per-tier list
// falls back to these defaults, never to an empty matcher.
// ---------------------------------------------------------------------------

// DefaultUtterances is the shipped example-utterance set per tier.
var DefaultUtterances = map[Tier][]string{
	TierCoding: {
		"Write a Python function that sorts a list of numbers",
		"Fix this bug in my code, it throws a null pointer error",
		"Refactor this function to be cleaner and faster",
		"Explain why this code fails and how to fix the error",
		"Write a SQL query to join these two tables",
		"Review my code and suggest improvements",
		"How do I implement authentication in a web app",
		"Debug this crash, the program exits with an error",
		"Convert this JavaScript code to Python",
		"Write unit tests for this function",
		"How do I fix a null pointer exception in Java",
		"Write a function that reverses a string in Python",
		"Explain this stack trace and tell me what went wrong",
		"Optimize this slow loop for large inputs",
		"What is the best way to sort a list of objects by date",
	},
	TierChat: {
		"Hello, how are you doing today",
		"Tell me a funny joke",
		"What is the weather like today",
		"Summarize this article for me in a few sentences",
		"Help me write a polite email to my manager",
		"What is the capital of France",
		"Translate this sentence into Spanish",
		"Give me some ideas for a birthday gift",
		"What do you think about electric cars",
		"Can you chat with me, I am bored",
		"What time is it in Tokyo right now",
		"Write a short poem about the rain",
		"How do I say thank you in French",
		"Tell me a story about a brave little dog",
		"What should I cook for dinner tonight",
	},
	TierReasoning: {
		"Solve this problem step by step and show your reasoning",
		"Prove that the square root of two is irrational",
		"Work through this logic puzzle and explain each step",
		"Compare the trade-offs between these two approaches",
		"Design an algorithm and analyze its time complexity",
		"What are the implications of this decision for the future",
		"Analyze this argument and point out any flaws",
		"Plan a strategy for launching a new product",
		"Derive the formula from first principles",
		"Reason about the best move in this chess position",
		"Solve this math word problem step by step",
		"Weigh the pros and cons of renting versus buying a house",
		"Work out this puzzle logically and show each deduction",
		"Explain the reasoning behind this conclusion",
		"Plan the steps needed to solve this optimization problem",
	},
	TierQuickQA: {
		"What is two plus two",
		"Who wrote the novel Pride and Prejudice",
		"What year was the first moon landing",
		"Define photosynthesis in one sentence",
		"How many days are there in a leap year",
		"What does this word mean",
		"When is the next solar eclipse",
		"Where is the Eiffel Tower located",
		"Give me a one-line answer: what is the speed of light",
		"What is the boiling point of water",
		"What is seven plus eight",
		"How many minutes are there in three hours",
		"What is the capital of Japan",
		"Who painted the Mona Lisa",
		"What is the largest planet in our solar system",
	},
}

// ---------------------------------------------------------------------------
// Virtual models.
// ---------------------------------------------------------------------------

// virtualModel describes one "auto/*" name: which tiers it may pick
// from, and which tier it falls back to below the threshold.
type virtualModel struct {
	candidates []Tier
	fallback   Tier
}

// virtualModels maps every supported virtual model name to its tier
// set. "auto" may pick any tier; the specialised names restrict the
// choice to the cheap/fast or the strong side.
var virtualModels = map[string]virtualModel{
	"auto":           {candidates: AllTiers, fallback: DefaultTier},
	"auto/fast":      {candidates: []Tier{TierQuickQA, TierChat}, fallback: TierChat},
	"auto/cheap":     {candidates: []Tier{TierQuickQA, TierChat}, fallback: TierQuickQA},
	"auto/coding":    {candidates: []Tier{TierCoding, TierReasoning}, fallback: TierCoding},
	"auto/reasoning": {candidates: []Tier{TierReasoning, TierCoding}, fallback: TierReasoning},
}

// VirtualModels lists every supported "auto/*" name in a stable order.
var VirtualModels = []string{"auto", "auto/fast", "auto/cheap", "auto/coding", "auto/reasoning"}

// IsVirtualModel reports whether model is a supported auto-router
// name ("auto" or "auto/<name>").
func IsVirtualModel(model string) bool {
	_, ok := virtualModels[model]
	return ok
}

// ---------------------------------------------------------------------------
// Decision.
// ---------------------------------------------------------------------------

// Decision is one routing decision: which tier was picked, how
// confident the matcher is, why, and which concrete model that
// resolves to ("" when no model could be resolved). Scores holds
// the best-utterance cosine similarity per tier, so a caller can
// show exactly why the winner won. LatencyMicros is the measured
// time spent embedding + scoring this prompt (not a claim).
type Decision struct {
	VirtualModel string             `json:"virtual_model"`
	Tier         Tier               `json:"tier"`
	Confidence   float32            `json:"confidence"`
	Scores       map[string]float32 `json:"scores"`
	Threshold    float32            `json:"threshold"`
	Fallback     bool               `json:"fallback"`
	Reason       string             `json:"reason"`
	Model        string             `json:"model"`
	// LatencyMicros is the measured routing-decision time in
	// microseconds (embed + score only; no network, no LLM call).
	LatencyMicros int64 `json:"latency_micros"`
}

// ---------------------------------------------------------------------------
// Router.
// ---------------------------------------------------------------------------

// Config tunes a Router. Zero values select the documented defaults.
type Config struct {
	// Threshold is the default confidence dial (0.5 when <= 0 or
	// > 1). A tier match below it falls back instead of guessing.
	Threshold float64
	// Thresholds overrides Threshold per virtual model name
	// ("auto/fast", ...). Entries outside (0, 1] are ignored.
	Thresholds map[string]float64
	// DefaultTier is the fallback tier for "auto" when no tier
	// clears the threshold. "" selects DefaultTier (reasoning).
	DefaultTier Tier
	// DefaultModel is the concrete model used when the picked
	// tier has no TierModels entry. "" means "no model" — Resolve
	// then reports ok=false rather than inventing one.
	DefaultModel string
	// TierModels maps a tier to its concrete model IDs in
	// preference order; the first entry wins. A tier with no
	// entry falls back to DefaultModel.
	TierModels map[Tier][]string
	// Utterances overrides the shipped default utterance sets
	// per tier. A tier with no override (or an empty list) keeps
	// the shipped defaults; the matcher is never empty.
	Utterances map[Tier][]string
}

// Router is a ready-to-use auto-router: utterance vectors are
// embedded once at construction, so Classify/Resolve only embed the
// incoming prompt and score it — milliseconds, no network, no LLM.
type Router struct {
	threshold    float32
	thresholds   map[string]float32
	defaultTier  Tier
	defaultModel string
	tierModels   map[Tier][]string
	// vectors[tier] holds the pre-embedded utterance vectors.
	vectors map[Tier][][]float32
	// centroids[tier] holds the tier's mean utterance vector,
	// L2-normalized: the tier's aggregate vocabulary. Scoring takes
	// the max of the best single utterance and the centroid, so a
	// prompt that shares words with the tier as a whole (but no one
	// utterance closely) still scores fairly.
	centroids map[Tier][]float32
}

// New builds a Router from cfg, applying the documented defaults.
// Utterance vectors are embedded once here, at construction.
func New(cfg Config) *Router {
	threshold := float32(cfg.Threshold)
	if threshold <= 0 || threshold > 1 {
		threshold = DefaultThreshold
	}
	thresholds := make(map[string]float32, len(cfg.Thresholds))
	for name, v := range cfg.Thresholds {
		if v > 0 && v <= 1 {
			thresholds[name] = float32(v)
		}
	}
	defaultTier := cfg.DefaultTier
	if !IsTier(string(defaultTier)) {
		defaultTier = DefaultTier
	}
	tierModels := make(map[Tier][]string, len(cfg.TierModels))
	for tier, models := range cfg.TierModels {
		if len(models) == 0 {
			continue
		}
		cp := make([]string, len(models))
		copy(cp, models)
		tierModels[tier] = cp
	}
	vectors := make(map[Tier][][]float32, len(AllTiers))
	centroids := make(map[Tier][]float32, len(AllTiers))
	for _, tier := range AllTiers {
		utterances := cfg.Utterances[tier]
		if len(utterances) == 0 {
			utterances = DefaultUtterances[tier]
		}
		vecs := make([][]float32, 0, len(utterances))
		for _, u := range utterances {
			vecs = append(vecs, cache.EmbedText(u))
		}
		vectors[tier] = vecs
		centroids[tier] = centroid(vecs)
	}
	return &Router{
		threshold:    threshold,
		thresholds:   thresholds,
		defaultTier:  defaultTier,
		defaultModel: cfg.DefaultModel,
		tierModels:   tierModels,
		vectors:      vectors,
		centroids:    centroids,
	}
}

// ThresholdFor returns the effective threshold for a virtual model
// name: the per-model override when set, else the router default.
func (r *Router) ThresholdFor(virtualModel string) float32 {
	if v, ok := r.thresholds[virtualModel]; ok {
		return v
	}
	return r.threshold
}

// centroid returns the L2-normalized mean of vecs (zero vector when
// empty). It is the tier's aggregate vocabulary, still pure
// token-overlap — no meaning beyond shared words.
func centroid(vecs [][]float32) []float32 {
	mean := make([]float32, cache.SemanticVectorDims)
	if len(vecs) == 0 {
		return mean
	}
	for _, v := range vecs {
		for i, x := range v {
			mean[i] += x
		}
	}
	var sum float64
	for i := range mean {
		mean[i] /= float32(len(vecs))
		sum += float64(mean[i]) * float64(mean[i])
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		return mean
	}
	for i := range mean {
		mean[i] = float32(float64(mean[i]) / norm)
	}
	return mean
}

// tierScore is the best cosine similarity of vec against a tier: the
// max of the best single utterance and the tier centroid.
func (r *Router) tierScore(vec []float32, tier Tier) float32 {
	best := cache.Cosine(vec, r.centroids[tier])
	for _, u := range r.vectors[tier] {
		if sim := cache.Cosine(vec, u); sim > best {
			best = sim
		}
	}
	return best
}

// Classify scores prompt against every tier and returns the
// decision for virtualModel, WITHOUT resolving a concrete model
// (Decision.Model is ""). thresholdOverride, when > 0 and <= 1,
// replaces the configured threshold for this one decision — the
// dashboard preview dial. An empty or token-less prompt scores 0
// everywhere and falls back, never guessing from nothing.
func (r *Router) Classify(virtualModel, prompt string, thresholdOverride float32) Decision {
	start := time.Now()
	vm, ok := virtualModels[virtualModel]
	if !ok {
		vm = virtualModels["auto"]
		virtualModel = "auto"
	}
	threshold := r.ThresholdFor(virtualModel)
	if thresholdOverride > 0 && thresholdOverride <= 1 {
		threshold = thresholdOverride
	}
	scores := make(map[string]float32, len(AllTiers))
	dec := Decision{
		VirtualModel: virtualModel,
		Scores:       scores,
		Threshold:    threshold,
	}
	vec := cache.EmbedText(prompt)
	for _, tier := range AllTiers {
		scores[string(tier)] = r.tierScore(vec, tier)
	}
	// Pick the best-scoring candidate tier.
	bestTier := vm.fallback
	var best float32 = -1
	for _, tier := range vm.candidates {
		if s := scores[string(tier)]; s > best {
			best, bestTier = s, tier
		}
	}
	dec.Confidence = best
	if best < threshold {
		dec.Tier = vm.fallback
		dec.Fallback = true
		dec.Reason = fmt.Sprintf("best match %.3f (%s) below threshold %.2f; falling back to %s tier instead of guessing",
			best, bestTier, threshold, vm.fallback)
	} else {
		dec.Tier = bestTier
		dec.Reason = fmt.Sprintf("best match %.3f (%s) at or above threshold %.2f", best, bestTier, threshold)
	}
	dec.LatencyMicros = time.Since(start).Microseconds()
	return dec
}

// ResolveWithThreshold is Resolve with a one-off threshold
// override (0 = use the configured threshold): the dashboard
// preview dial classifies with the slider's value without changing
// the router's configured threshold. The returned decision carries
// the effective threshold that was applied.
func (r *Router) ResolveWithThreshold(virtualModel, prompt string, threshold float32) (string, Decision, bool) {
	if !IsVirtualModel(virtualModel) {
		return "", Decision{VirtualModel: virtualModel, Reason: fmt.Sprintf("unknown virtual model %q", virtualModel)}, false
	}
	dec := r.Classify(virtualModel, prompt, threshold)
	dec.Model = r.modelFor(dec.Tier)
	if dec.Model == "" {
		dec.Reason += "; no concrete model configured for tier " + string(dec.Tier)
		return "", dec, false
	}
	return dec.Model, dec, true
}

// Resolve classifies prompt for virtualModel and resolves the picked
// (or fallback) tier to a concrete model: the tier's first
// TierModels entry, else DefaultModel. It returns ok=false when the
// virtual model name is unknown or no concrete model can be
// resolved — the caller must surface an honest error, never invent
// a model. The decision is returned either way, so the caller can
// show why the request failed.
func (r *Router) Resolve(virtualModel, prompt string) (string, Decision, bool) {
	return r.ResolveWithThreshold(virtualModel, prompt, 0)
}

// modelFor returns the concrete model for a tier: the tier's first
// TierModels entry, else DefaultModel, else "".
func (r *Router) modelFor(tier Tier) string {
	if models, ok := r.tierModels[tier]; ok && len(models) > 0 {
		return models[0]
	}
	return r.defaultModel
}

// UtteranceCount returns how many example utterances the router
// compares against for a tier (shipped defaults unless overridden).
func (r *Router) UtteranceCount(tier Tier) int {
	return len(r.vectors[tier])
}

// ParsePrompt extracts the routing prompt from a chat request's
// messages: the last user message's text (content string, plus any
// text parts). It returns "" when there is no user message.
func ParsePrompt(messages []PromptMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		return strings.TrimSpace(messages[i].Text())
	}
	return ""
}

// PromptMessage is the minimal message shape ParsePrompt needs, so
// callers can adapt their own message type without this package
// importing a provider type.
type PromptMessage struct {
	Role    string
	Content string
	// TextParts carries text content parts when Content is empty.
	TextParts []string
}

// Text returns the message text: Content when set, else the text
// parts joined with newlines.
func (m PromptMessage) Text() string {
	if m.Content != "" {
		return m.Content
	}
	return strings.Join(m.TextParts, "\n")
}
