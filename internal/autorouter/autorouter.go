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
// "auto/coding", "auto/code" or "auto/reasoning". Each names a
// candidate tier set and a fallback tier; the concrete model comes
// from TierModels (operator-configured, first entry wins) or
// DefaultModel. When no concrete model can be resolved the
// decision carries Model "" and Resolve reports ok=false — the
// caller must surface an honest error, never invent a model.
//
// INTENT TIERS (items 259-260, see intent.go): clients may also
// request "intent/fast", "intent/balanced" or "intent/best". Those
// are NOT classified — the operator defines what each tier means
// as a (model, effort) pair, and the gateway maps the intent to
// that pair. An unmapped tier fails honestly naming the tier.
//
// "auto/code" is the Auto Code mode (items 247–248): same tier
// set as "auto/coding", but the tier's models are ordered by
// expected cost per solved task (see code.go) instead of the
// generic ranker — a different objective, not a tweak.
//
// The shipped utterance sets are CONFIG DEFAULTS, not measured
// traffic and not results: they are the examples the matcher
// compares against, labelled as such wherever they are shown.
package autorouter

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/learned"
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
	"auto/code":      {candidates: []Tier{TierCoding, TierReasoning}, fallback: TierCoding},
	"auto/reasoning": {candidates: []Tier{TierReasoning, TierCoding}, fallback: TierReasoning},
}

// VirtualModels lists every supported "auto/*" name in a stable order.
var VirtualModels = []string{"auto", "auto/fast", "auto/cheap", "auto/coding", "auto/code", "auto/reasoning"}

// IsVirtualModel reports whether model is a supported auto-router
// name ("auto" or "auto/<name>") or an intent virtual model
// ("intent/fast", "intent/balanced", "intent/best" — see
// intent.go). An intent name with an unknown tier
// ("intent/unknown") is NOT virtual: it fails as an unknown model,
// while a known-but-unmapped tier is virtual and fails honestly
// naming the tier.
func IsVirtualModel(model string) bool {
	if _, ok := virtualModels[model]; ok {
		return true
	}
	return IsIntentModel(model)
}

// ---------------------------------------------------------------------------
// Decision.
// ---------------------------------------------------------------------------

// Decision is one routing decision: which tier was picked, how
// confident the matcher is, why, and which concrete model that
// resolves to ("" when no model could be resolved). Scores holds
// the best-utterance cosine similarity per tier, so a caller can
// show exactly why the winner won. Confidence is the winning
// tier's effective score: the raw cosine plus its learned nudge
// when learning applies (so it can differ from Scores[winner] by
// at most the bounded nudge — the Reason names the nudge when one
// applied). LatencyMicros is the measured
// time spent embedding + scoring this prompt (not a claim).
// ID is the decision's stable id ("dec-<router-boot-nanos>-<seq>"):
// the server registers it when the decision is actually served on
// a real /v1 request, and the dashboard feedback endpoint accepts
// signals against it. A decision that was only classified (the
// dashboard preview) also carries an ID, but it is never
// registered — feedback for it is rejected as an unknown decision,
// so preview classifications can never train the router.
// LearnedNudge holds the per-tier score nudge that was applied to
// this decision (feedback-learned weights; only tiers with a
// nonzero nudge or samples appear), and LearnedSamples the signal
// count behind each nudge. Both are empty when no weights provider
// is wired or learning is off — a zero-effect default, never an
// invented weight.
type Decision struct {
	ID           string             `json:"id,omitempty"`
	VirtualModel string             `json:"virtual_model"`
	Tier         Tier               `json:"tier"`
	Confidence   float32            `json:"confidence"`
	Scores       map[string]float32 `json:"scores"`
	Threshold    float32            `json:"threshold"`
	Fallback     bool               `json:"fallback"`
	Reason       string             `json:"reason"`
	Model        string             `json:"model"`
	// RankedPool is the picked tier's candidate models in the
	// order the pool ranker returned them, best first (see
	// ModelRanker): the 16-factor pool order when a ranker is
	// set (the server wires the 16-factor ranker under
	// DefaultWeights16, with the LKGP session pair layered in
	// as session affinity — so the pool is both 16-factor
	// scored and LKGP-sticky), list order otherwise. It is the
	// evidence for WHICH pool order the decision used: the
	// winner (Model) is RankedPool[0] when the pool is
	// non-empty. Absent when the tier had no candidate models
	// (the decision fell back to DefaultModel), and for
	// intent/code decisions, whose rankings already travel in
	// CodeScores (code) or a fixed mapping (intent).
	RankedPool []string `json:"ranked_pool,omitempty"`
	// LatencyMicros is the measured routing-decision time in
	// microseconds (embed + score only; no network, no LLM call).
	LatencyMicros int64 `json:"latency_micros"`
	// LearnedNudge is the feedback-learned score adjustment
	// applied per tier for this decision (bounded, see
	// internal/feedback). Absent when learning is off.
	LearnedNudge map[string]float32 `json:"learned_nudge,omitempty"`
	// LearnedSamples is the per-tier feedback signal count behind
	// LearnedNudge. Absent when learning is off.
	LearnedSamples map[string]int `json:"learned_samples,omitempty"`
	// LearnedWinProb is the learned auto-router's per-tier win
	// probability for this prompt (internal/learned, RouteLLM
	// pattern: P(the tier's answer is kept) predicted from the
	// recorded heuristic scores). Present only when the learned
	// model is ACTIVE — calibrated on real evidence and enabled
	// by the operator flag. Absent means the learned signal
	// contributed nothing to this decision (off / learning /
	// uncalibrated), never a hidden guess.
	LearnedWinProb map[string]float32 `json:"learned_win_prob,omitempty"`
	// LearnedActive reports whether the learned win
	// probabilities above actually applied to this decision.
	LearnedActive bool `json:"learned_active,omitempty"`
	// CodeScores is the auto/code mode's per-model
	// cost-per-solved-task breakdown (see code.go), best first.
	// Present only when the decision was made for "auto/code".
	CodeScores []CodeScore `json:"code_scores,omitempty"`
	// CodePoolDropped names the models pool narrowing removed
	// for this auto/code decision, with the reason each was
	// dropped. Absent when nothing was dropped (or the mode is
	// not auto/code).
	CodePoolDropped map[string]string `json:"code_pool_dropped,omitempty"`
	// CodeNoEvidence reports that this auto/code decision ranked
	// on the documented neutral default (no candidate carried
	// measured task-outcome evidence), so the order is by price
	// only — never a measured rate. Absent when at least one
	// candidate had evidence (or the mode is not auto/code).
	CodeNoEvidence bool `json:"code_no_evidence,omitempty"`
	// IntentTier names the intent tier this decision resolved
	// ("fast", "balanced", "best"), when the request named an
	// "intent/<tier>" virtual model. Absent for auto/* decisions.
	IntentTier IntentTier `json:"intent_tier,omitempty"`
	// Effort is the reasoning effort the resolved intent tier
	// pins ("low", "medium", "high"), when the request named an
	// "intent/<tier>" virtual model whose mapping pins one.
	// Absent when no effort is pinned (or the mode is not
	// intent): the effort is resolved and reported, never
	// invented — and since CHECKLIST 269-270 it is also carried
	// onto the engine attempt by the engine's resolveAuto path,
	// so the provider receives the tier's effort (see
	// provider.ChatRequest.ReasoningEffort for which wires
	// forward it).
	Effort Effort `json:"effort,omitempty"`
}

// TierWeight is one tier's feedback-learned state as a weights
// provider reports it: the score nudge to apply (0 when the tier
// has too few signals or learning is off), the sample count behind
// it, and the signal breakdown.
type TierWeight struct {
	// Weight is the score nudge in [-0.05, +0.05] (0 when inactive).
	Weight float64
	// Samples is the total feedback signals recorded for the tier.
	Samples int
	// Accepts, Edits, Discards, Regenerations are the per-signal
	// counts behind Samples.
	Accepts       int
	Edits         int
	Discards      int
	Regenerations int
}

// WeightsProvider reports the current feedback-learned per-tier
// weights. It is an interface (not a *feedback.Store) so this
// package stays a leaf: feedback implements it, the server wires
// it in. A nil provider means "no learning": all nudges are 0.
type WeightsProvider interface {
	LearnedWeights() map[Tier]TierWeight
}

// LearnedPredictor reports the learned auto-router's per-tier
// win probabilities for a prompt's raw heuristic scores
// (internal/learned). It is an interface (not a
// *learned.Refresher) so the server wires it in; a nil predictor
// means "no learned model": the learned contribution is exactly
// 0. The second return value reports whether the probabilities
// may compose into routing: false while the model is off,
// learning, or uncalibrated, or while the operator flag is off —
// in that case Classify must ignore the probabilities entirely,
// so routing is exactly unchanged (item-115 no-evidence rule).
type LearnedPredictor interface {
	LearnedWinProbs(scores map[string]float32) (map[Tier]float32, bool)
}

// ---------------------------------------------------------------------------
// Router.
// ---------------------------------------------------------------------------

// ModelRanker orders one tier's candidate models, best first. The
// router calls it whenever it resolves a tier to a concrete model,
// so the first ranked model wins instead of the first listed one.
// A ranker wired by the server orders by real cost (the operator's
// price book), measured latency/success from the engine's live
// provider stats, and the measured quality signal (error-free
// completion rate from real attempts — see internal/scoring), so
// the tier ordering and the cheapest/fastest/most-reliable
// strategies agree on the same signals. A nil ranker keeps the
// configured list order (first entry wins), exactly as before.
// The ranker must return a permutation of its input: it never
// drops or duplicates a model. The router enforces that — a ranker
// that drops or duplicates is ignored and list order stands.
type ModelRanker func(tier Tier, models []string) []string

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
	//
	// TierModels is the CONFIGURED set only. Models of providers
	// added live (after startup) join every tier at add time via
	// RegisterTierModels — see that method: a live-added model is
	// a candidate in every tier until the operator says otherwise,
	// because no tier preference is expressed when a provider is
	// added, and the ranker (not membership) decides by real
	// cost/latency/quality. Config-listed models keep exactly
	// their configured tiers.
	TierModels map[Tier][]string
	// Utterances overrides the shipped default utterance sets
	// per tier. A tier with no override (or an empty list) keeps
	// the shipped defaults; the matcher is never empty.
	Utterances map[Tier][]string
	// RankModels, when set, reorders each tier's candidate models
	// by real cost + measured latency/success + measured quality
	// before the first one wins (see ModelRanker). Nil means list
	// order stands.
	RankModels ModelRanker
	// CodePrices reports a model's blended price for the auto/code
	// mode (see CodePriceFunc). Nil means "no prices wired": the
	// mode still resolves, but every candidate is unpriced and
	// the decision says so.
	CodePrices CodePriceFunc
	// CodePool is the auto/code pool narrowing (allow/exclude
	// lists, capability requirements, guardrail predicate).
	// Zero value means no narrowing.
	CodePool CodePoolFilter
	// CodeOutcomes reports a model's recorded coding-task
	// outcome for the auto/code mode. Nil means "no evidence":
	// every model ranks at the documented neutral default.
	CodeOutcomes CodeOutcomeProvider
	// IntentTiers maps intent tiers to their operator-defined
	// (model, effort) pairs (see intent.go). A tier with no
	// entry is unmapped: a request naming it fails honestly
	// naming the tier, never a silent default model.
	IntentTiers map[IntentTier]IntentMapping
}

// Router is a ready-to-use auto-router: utterance vectors are
// embedded once at construction, so Classify/Resolve only embed the
// incoming prompt and score it — milliseconds, no network, no LLM.
type Router struct {
	threshold    float32
	thresholds   map[string]float32
	defaultTier  Tier
	defaultModel string
	// tierModels is guarded by tierMu: Resolve/modelFor read it
	// on the request path while RegisterTierModels writes it when
	// a provider is added live and UnregisterTierModels removes
	// entries when a provider is deleted live; a restart rebuilds
	// it from config either way.
	tierMu     sync.RWMutex
	tierModels map[Tier][]string
	rankModels ModelRanker
	// vectors[tier] holds the pre-embedded utterance vectors.
	vectors map[Tier][][]float32
	// centroids[tier] holds the tier's mean utterance vector,
	// L2-normalized: the tier's aggregate vocabulary. Scoring takes
	// the max of the best single utterance and the centroid, so a
	// prompt that shares words with the tier as a whole (but no one
	// utterance closely) still scores fairly.
	centroids map[Tier][]float32
	// idBase is the router's boot time in nanoseconds, the stable
	// prefix of every decision ID it mints: IDs stay unique across
	// restarts, so a new process never reuses an id the feedback
	// store already registered under a different decision.
	idBase int64
	// idSeq is the per-decision sequence number, appended to idBase.
	idSeq atomic.Uint64
	// weightsProvider reports the feedback-learned per-tier
	// nudges (nil = no learning). Guarded by weightsMu; Classify
	// reads it under RLock and calls the provider outside the lock.
	weightsMu       sync.RWMutex
	weightsProvider WeightsProvider
	// learnedPredictor reports the learned auto-router's
	// per-tier win probabilities (nil = no learned model).
	// Same locking shape as weightsProvider.
	learnedMu        sync.RWMutex
	learnedPredictor LearnedPredictor
	// codeMu guards the auto/code mode's wiring (prices, pool,
	// outcomes): the server sets them after construction while
	// Resolve reads them on the request path.
	codeMu       sync.RWMutex
	codePrices   CodePriceFunc
	codePool     CodePoolFilter
	codeOutcomes CodeOutcomeProvider
	// intentMu guards the intent-tier mappings: ResolveIntent
	// reads them on the request path while SetIntentTier writes
	// them when the operator (or an approved suggestion) redefines
	// a tier.
	intentMu    sync.RWMutex
	intentTiers map[IntentTier]IntentMapping
	// outcomes is the metadata-only learning loop's recorder
	// (see suggestions.go): per-(task, model) latency/success
	// stats from served requests. Never nil after New.
	outcomes *OutcomeRecorder
	// suggMu guards the suggestion log and its sequence: apply
	// and revert serialise here so two operators cannot apply
	// the same suggestion twice.
	suggMu  sync.Mutex
	suggLog []AppliedChange
	suggSeq uint64
}

// SetCodePrices wires the auto/code mode's price source (the
// server/cmd does this from the operator price book). A nil func
// unwires it: every candidate is then unpriced. Safe for
// concurrent use with Resolve.
func (r *Router) SetCodePrices(fn CodePriceFunc) {
	r.codeMu.Lock()
	defer r.codeMu.Unlock()
	r.codePrices = fn
}

// SetCodePool wires the auto/code mode's pool narrowing (the
// server/cmd does this from config). Safe for concurrent use
// with Resolve.
func (r *Router) SetCodePool(f CodePoolFilter) {
	r.codeMu.Lock()
	defer r.codeMu.Unlock()
	r.codePool = f
}

// SetCodeOutcomeProvider wires the auto/code mode's recorded
// task-outcome evidence (the server does this with its
// OutcomeStore, fed from the feedback POST handler). A nil
// provider unwires it: every model then ranks at the documented
// neutral default. Safe for concurrent use with Resolve.
func (r *Router) SetCodeOutcomeProvider(p CodeOutcomeProvider) {
	r.codeMu.Lock()
	defer r.codeMu.Unlock()
	r.codeOutcomes = p
}

// codeWiring returns the auto/code mode's current wiring
// snapshot (prices func, pool filter, outcomes provider).
func (r *Router) codeWiring() (CodePriceFunc, CodePoolFilter, CodeOutcomeProvider) {
	r.codeMu.RLock()
	defer r.codeMu.RUnlock()
	return r.codePrices, r.codePool, r.codeOutcomes
}

// SetWeightsProvider wires the feedback-learned weights source into
// the router (the server does this when a feedback store is
// configured). A nil provider disables learning again. Safe for
// concurrent use with Classify.
func (r *Router) SetWeightsProvider(p WeightsProvider) {
	r.weightsMu.Lock()
	defer r.weightsMu.Unlock()
	r.weightsProvider = p
}

// SetLearnedPredictor wires the learned auto-router (the server
// does this when a feedback store is configured and the
// learned_router flag is on). A nil predictor disables the
// learned signal again. Safe for concurrent use with Classify.
func (r *Router) SetLearnedPredictor(p LearnedPredictor) {
	r.learnedMu.Lock()
	defer r.learnedMu.Unlock()
	r.learnedPredictor = p
}

// RankModels returns the pool ranker configured on this Router
// (see Config.RankModels), or nil when none is configured. The
// ranker is the same hook ResolveAutoVariantWithPolicyAndRanker
// takes, so server-side variant resolution scores on the same
// live signals as the tier ranking. rankModels is set once at
// construction and never reassigned, so a plain read is safe for
// concurrent use with the request path.
func (r *Router) RankModels() ModelRanker {
	if r == nil {
		return nil
	}
	return r.rankModels
}

// learnedProbs returns the current per-tier learned win
// probabilities and whether they may apply (nil, false when no
// predictor is wired or the model is off / learning /
// uncalibrated). The predictor's own cache bounds how often the
// model is retrained.
func (r *Router) learnedProbs(scores map[string]float32) (map[Tier]float32, bool) {
	r.learnedMu.RLock()
	p := r.learnedPredictor
	r.learnedMu.RUnlock()
	if p == nil {
		return nil, false
	}
	return p.LearnedWinProbs(scores)
}

// learnedNudges returns the current per-tier score nudges from the
// weights provider (nil, nil when none is wired). The provider's
// own cache bounds how often the store is read.
func (r *Router) learnedNudges() (map[Tier]float32, map[Tier]int) {
	r.weightsMu.RLock()
	p := r.weightsProvider
	r.weightsMu.RUnlock()
	if p == nil {
		return nil, nil
	}
	weights := p.LearnedWeights()
	nudges := make(map[Tier]float32, len(weights))
	samples := make(map[Tier]int, len(weights))
	for tier, w := range weights {
		if w.Weight != 0 || w.Samples > 0 {
			nudges[tier] = float32(w.Weight)
			samples[tier] = w.Samples
		}
	}
	if len(nudges) == 0 {
		return nil, nil
	}
	return nudges, samples
}

// nextDecisionID mints a unique id for one decision.
func (r *Router) nextDecisionID() string {
	return fmt.Sprintf("dec-%d-%d", r.idBase, r.idSeq.Add(1))
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
		rankModels:   cfg.RankModels,
		vectors:      vectors,
		centroids:    centroids,
		idBase:       time.Now().UnixNano(),
		codePrices:   cfg.CodePrices,
		codePool:     cfg.CodePool,
		codeOutcomes: cfg.CodeOutcomes,
		intentTiers:  copyIntentTiers(cfg.IntentTiers),
		outcomes:     NewOutcomeRecorder(),
	}
}

// copyIntentTiers copies an intent-tier mapping set, dropping
// entries with an empty model or an unknown tier/effort — a bad
// config entry must not become a mapping that fails at request
// time (config validation already rejects these at load; this is
// the belt-and-braces copy for direct New callers).
func copyIntentTiers(in map[IntentTier]IntentMapping) map[IntentTier]IntentMapping {
	out := make(map[IntentTier]IntentMapping, len(in))
	for tier, m := range in {
		if !IsIntentTier(string(tier)) || strings.TrimSpace(m.Model) == "" || !IsEffort(string(m.Effort)) {
			continue
		}
		out[tier] = IntentMapping{Model: strings.TrimSpace(m.Model), Effort: m.Effort}
	}
	return out
}

// RegisterTierModels adds models as candidates of EVERY tier.
// It is the live-add path: when POST /api/providers adds a
// provider, the server calls this with the new provider's model
// IDs (only when the provider routes live), so the models become
// tier candidates immediately with no restart and no config edit.
//
// Why every tier: the router cannot know a new model's capability
// class from its ID, and guessing one (from the name, say) would
// be invented knowledge. Membership is candidacy only — the
// ranker decides within each tier by real cost + measured
// latency/success + measured quality, exactly as for
// config-listed models. A model with no attempts yet therefore
// ranks on its configured price (neutral when unpriced) plus
// neutral latency/success/quality, like any config-listed unknown
// model: it can win a tier on price alone before its first
// attempt, and measured failures demote it after. A provider
// with no model list (chat-only gateway) registers nothing —
// there is no model ID to rank.
//
// Honest limits: registration is deduped (a second add of the same
// model is a no-op); removal is explicit via UnregisterTierModels.
// Empty model IDs are skipped. Safe for concurrent use with
// Resolve.
func (r *Router) RegisterTierModels(models []string) {
	uniq := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		uniq = append(uniq, m)
	}
	if len(uniq) == 0 {
		return
	}
	r.tierMu.Lock()
	defer r.tierMu.Unlock()
	for _, tier := range AllTiers {
		cur := r.tierModels[tier]
		have := make(map[string]struct{}, len(cur)+len(uniq))
		for _, m := range cur {
			have[m] = struct{}{}
		}
		for _, m := range uniq {
			if _, dup := have[m]; dup {
				continue
			}
			have[m] = struct{}{}
			cur = append(cur, m)
		}
		r.tierModels[tier] = cur
	}
}

// UnregisterTierModels removes models from every tier's candidate
// list (the counterpart of RegisterTierModels, for a live provider
// delete). The caller must pass only models no remaining provider
// still serves — a model ID shared by two providers must stay a
// candidate while either provider remains, because tiers hold model
// IDs, not (provider, model) pairs. Removing a model that is not
// registered is a no-op; removing a model's last tier entry leaves
// that tier with no candidates from this path (its configured
// entries are unaffected — only the listed IDs are removed).
// Safe for concurrent use with Resolve.
func (r *Router) UnregisterTierModels(models []string) {
	drop := make(map[string]struct{}, len(models))
	for _, m := range models {
		if m != "" {
			drop[m] = struct{}{}
		}
	}
	if len(drop) == 0 {
		return
	}
	r.tierMu.Lock()
	defer r.tierMu.Unlock()
	for _, tier := range AllTiers {
		cur := r.tierModels[tier]
		if len(cur) == 0 {
			continue
		}
		kept := cur[:0]
		for _, m := range cur {
			if _, gone := drop[m]; !gone {
				kept = append(kept, m)
			}
		}
		// Keep the slice non-nil when it empties, so a tier that
		// loses its last candidate still reports an (empty) list
		// rather than nil.
		if len(kept) == 0 {
			kept = []string{}
		}
		r.tierModels[tier] = kept
	}
}

// ModelsForTier returns a copy of the tier's current candidate
// model IDs in order (configured entries first, live-registered
// entries appended in registration order). Empty when the tier
// has no candidates.
func (r *Router) ModelsForTier(tier Tier) []string {
	r.tierMu.RLock()
	defer r.tierMu.RUnlock()
	return append([]string(nil), r.tierModels[tier]...)
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
		ID:           r.nextDecisionID(),
		VirtualModel: virtualModel,
		Scores:       scores,
		Threshold:    threshold,
	}
	vec := cache.EmbedText(prompt)
	for _, tier := range AllTiers {
		scores[string(tier)] = r.tierScore(vec, tier)
	}
	// Feedback-learned nudges (Phase 2 prototype): each candidate
	// tier's effective score is its raw cosine score plus its
	// learned nudge, clamped to [0, 1]. The nudge is bounded to
	// +/-0.05 and applies only after 10 signals for that tier (see
	// internal/feedback), so it can only move a genuinely
	// borderline decision — never promote a tier that scored far
	// below the winner. Scores stay raw (they are cosine
	// similarities); the applied nudge is reported separately in
	// LearnedNudge, so the decision view can show exactly what
	// learning contributed.
	nudges, samples := r.learnedNudges()
	if len(nudges) > 0 {
		dec.LearnedNudge = make(map[string]float32, len(nudges))
		dec.LearnedSamples = make(map[string]int, len(samples))
		for tier, n := range nudges {
			dec.LearnedNudge[string(tier)] = n
			dec.LearnedSamples[string(tier)] = samples[tier]
		}
	}
	// Learned win probabilities (RouteLLM pattern, see
	// internal/learned): per-tier P(win) predicted from the
	// recorded heuristic scores, composed as a bounded additive
	// signal — learned.Contribution maps a probability to at most
	// +/-0.05 — on top of the heuristic score and the feedback
	// nudge. The predictor reports active=false while the model
	// is off, learning, or uncalibrated (or the operator flag is
	// off); then the learned contribution is exactly 0 and
	// routing is unchanged (item-115 no-evidence rule).
	winProbs, learnedActive := r.learnedProbs(scores)
	if learnedActive && len(winProbs) > 0 {
		dec.LearnedWinProb = make(map[string]float32, len(winProbs))
		dec.LearnedActive = true
		for tier, p := range winProbs {
			dec.LearnedWinProb[string(tier)] = p
		}
	}
	effective := func(tier Tier) float32 {
		s := scores[string(tier)] + nudges[tier]
		if learnedActive {
			s += float32(learned.Contribution(float64(winProbs[tier])))
		}
		if s < 0 {
			return 0
		}
		if s > 1 {
			return 1
		}
		return s
	}
	// Pick the best-scoring candidate tier, by effective score.
	bestTier := vm.fallback
	var best float32 = -1
	for _, tier := range vm.candidates {
		if s := effective(tier); s > best {
			best, bestTier = s, tier
		}
	}
	dec.Confidence = best
	nudgeNote := ""
	if n := nudges[bestTier]; n != 0 {
		nudgeNote = fmt.Sprintf(" (learned nudge %+.3f from %d signals)", n, samples[bestTier])
	}
	learnedNote := ""
	if learnedActive {
		learnedNote = fmt.Sprintf(" (learned win-prob %.2f for %s)", winProbs[bestTier], bestTier)
	}
	if best < threshold {
		dec.Tier = vm.fallback
		dec.Fallback = true
		dec.Reason = fmt.Sprintf("best match %.3f (%s) below threshold %.2f; falling back to %s tier instead of guessing%s%s",
			best, bestTier, threshold, vm.fallback, nudgeNote, learnedNote)
	} else {
		dec.Tier = bestTier
		dec.Reason = fmt.Sprintf("best match %.3f (%s) at or above threshold %.2f%s%s", best, bestTier, threshold, nudgeNote, learnedNote)
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
	// Intent tiers resolve directly to their operator-defined
	// (model, effort) pair — no classification, no prompt needed.
	if tier, ok := ParseIntentModel(virtualModel); ok {
		return r.resolveIntent(virtualModel, tier)
	}
	if !IsVirtualModel(virtualModel) {
		return "", Decision{VirtualModel: virtualModel, Reason: fmt.Sprintf("unknown virtual model %q", virtualModel)}, false
	}
	dec := r.Classify(virtualModel, prompt, threshold)
	if virtualModel == CodeVirtualModel {
		return r.resolveCode(dec)
	}
	dec.Model, dec.RankedPool = r.modelFor(dec.Tier)
	if dec.Model == "" {
		dec.Reason += "; no concrete model configured for tier " + string(dec.Tier)
		return "", dec, false
	}
	return dec.Model, dec, true
}

// resolveCode resolves an "auto/code" decision to its concrete
// model by expected cost per solved task (see code.go): narrow
// the picked tier's pool, score the survivors, pick the cheapest
// per solved task. It returns ok=false when the pool narrows to
// nothing (no model is invented) — the decision still carries
// the narrowing record, so the caller can show why.
func (r *Router) resolveCode(dec Decision) (string, Decision, bool) {
	prices, pool, outcomes := r.codeWiring()
	narrowed := NarrowPool(r.tierModelList(dec.Tier), pool)
	if len(narrowed.Models) == 0 {
		dec.Reason += "; auto/code: pool empty after narrowing — no model to score"
		if len(narrowed.Dropped) > 0 {
			dec.CodePoolDropped = narrowed.Dropped
		}
		return "", dec, false
	}
	scores := ScoreCodeModels(narrowed.Models, prices, outcomes)
	dec.CodeScores = scores
	if len(narrowed.Dropped) > 0 {
		dec.CodePoolDropped = narrowed.Dropped
	}
	if !AnyCodeEvidence(scores) {
		dec.CodeNoEvidence = true
	}
	dec.Model = scores[0].Model
	dec.Reason += codeReason(scores, narrowed)
	return dec.Model, dec, true
}

// tierModelList returns a copy of the tier's current candidate model
// IDs in order (the list modelFor ranks from).
func (r *Router) tierModelList(tier Tier) []string {
	r.tierMu.RLock()
	defer r.tierMu.RUnlock()
	return append([]string(nil), r.tierModels[tier]...)
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

// modelFor returns the concrete model for a tier and the tier's
// candidate models in ranked order, best first: the tier's first
// TierModels entry (after the configured RankModels reorders them,
// when one is set), else DefaultModel, else "". The ranked pool is
// the order the ranker returned (a permutation of the tier's
// candidates, enforced here) or list order when no ranker is set;
// it is nil when the tier has no candidates at all (the model then
// falls back to DefaultModel).
//
// The ranker runs on every resolution, so a tier's winner follows
// live cost/latency/quality rather than list order — but the ranker
// is trusted only as far as it returns a permutation: a ranker that
// drops, duplicates or reorders nothing (or panics — recovered
// here, because a routing decision must never crash the server)
// falls back to list order.
func (r *Router) modelFor(tier Tier) (model string, ranked []string) {
	// Copy the candidate list under RLock; the ranker runs on
	// the copy, so a concurrent live registration can neither
	// race the read nor block behind ranker code.
	r.tierMu.RLock()
	models := append([]string(nil), r.tierModels[tier]...)
	r.tierMu.RUnlock()
	if len(models) == 0 {
		return r.defaultModel, nil
	}
	if r.rankModels != nil {
		ranked := func() (out []string) {
			defer func() {
				if recover() != nil {
					out = nil
				}
			}()
			return r.rankModels(tier, append([]string(nil), models...))
		}()
		if isPermutation(ranked, models) {
			models = ranked
		}
	}
	return models[0], models
}

// isPermutation reports whether ranked contains exactly the same
// elements as models, each once (order aside).
func isPermutation(ranked, models []string) bool {
	if len(ranked) != len(models) {
		return false
	}
	seen := make(map[string]int, len(models))
	for _, m := range models {
		seen[m]++
	}
	for _, m := range ranked {
		if seen[m] == 0 {
			return false
		}
		seen[m]--
	}
	return true
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
