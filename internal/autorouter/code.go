// Package autorouter — Auto Code mode (CHECKLIST items 247–248,
// research-log entry 228, OpenRouter Auto Code): a
// coding-specialized auto-router mode optimising COST PER SOLVED
// TASK, a genuinely different objective from generic cost/quality,
// built as a mode, not a tweak.
//
// THE OBJECTIVE. For a coding task the operator does not pay per
// request, they pay per SOLVED task: a cheap model that fails the
// task costs every retry, while a pricier model that solves it
// first time costs once. So each candidate is scored by its
// expected cost per solved task:
//
//	CPS = price × E[attempts] × failurePenalty
//	    = price × (1/p) × (1 + K·(1−p))
//
// where price is the model's blended USD per 1M tokens (operator
// price book, same resolution as the tier ranker), p is the
// measured task solve rate for that model, E[attempts] = 1/p is
// the expected number of attempts to one solved task (geometric
// mean), and the failure penalty (K = CodeFailurePenaltyK = 2)
// punishes failure HARSHLY: a model that solves 20% of tasks pays
// 5× the attempts AND a 2.6× penalty, so it loses to a 5× pricier
// model that solves 95% — exactly the behaviour the task
// requires. The penalty is multiplicative on top of the attempt
// count, not instead of it: both the retries AND the wasted work
// cost money.
//
// EVIDENCE — LEARNED FROM REAL OUTCOMES, EVIDENCE-GATED LIKE
// ITEMS 181–182. p comes only from recorded task outcomes: the
// feedback store's accept / edit / discard / regenerate signals
// on decisions actually served to the CODING tier (the server
// feeds them into OutcomeStore from the feedback POST handler —
// the store itself holds no prompt or answer, only per-model
// solved credit). Solved credit per signal: accept = 1 (solved),
// edit = 0.5 (solved after a fix), discard = 0, regenerate = 0
// (failed). A model gets its MEASURED rate only after
// MinCodeSamples (10) labelled coding outcomes; below that its
// rate is unknown and the mode behaves honestly (see below).
// A model with no evidence is NEVER assigned an invented rate.
//
// NO-EVIDENCE BEHAVIOUR (the documented default). With no
// evidence for a model, p is the neutral DefaultSolveRate (0.5)
// for every candidate alike, so the ranking falls back to PRICE
// order and the decision says so in plain text ("no task-outcome
// evidence yet — solve rate assumed 0.50, ranking by price only").
// The default is a property of the MODE, not of any model: it
// cannot make an unevidenced model look good, only order
// unevidenced models by what IS known (their price). A model
// with no configured price at all cannot have a CPS computed:
// it sorts after every priced model and is flagged PriceKnown
// false, never silently treated as free.
//
// POOL NARROWING. Before scoring, NarrowPool applies, in order:
// the allow list (when set, only listed models survive), the
// exclude list, the capability requirement (a model whose
// operator-declared capabilities do not cover every required
// capability is dropped — and a model with NO declared
// capabilities is dropped too when a requirement is set, because
// an unverified capability must not be assumed), and the
// guardrail predicate (a model it rejects is dropped — the live
// wiring rejects models deprecated at every provider offering
// them). Every drop is recorded with its reason in
// NarrowedPool.Dropped, so the decision can show exactly why a
// model never competed. An empty pool fails honestly: no model
// is invented.
//
// SELECTION. "auto/code" is a named virtual model (candidates:
// coding + reasoning tiers, fallback coding) and "auto:code" is
// its strategy alias. The decision carries the full per-model
// breakdown (CodeScores), the pool drops, and a Reason that names
// the winner's CPS, its price, its measured-or-default solve
// rate with the sample count, and the runner-up — so the route
// trace and the decision header can name the mode and its
// reasoning. The mode never touches failover, pinning,
// redaction, or the hot path: it only reorders which model a
// coding-tier decision resolves to.
package autorouter

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
)

// CodeVirtualModel is the auto-code virtual model name: clients
// request model "auto/code" (or the "auto:code" strategy alias)
// to route a coding task by expected cost per solved task.
const CodeVirtualModel = "auto/code"

// DefaultSolveRate is the neutral solve rate assumed for a model
// with no (or too little) recorded task-outcome evidence: 0.5.
// It is a property of the MODE, applied to every unevidenced
// candidate alike, so unevidenced models rank by price only —
// never an invented per-model rate.
const DefaultSolveRate = 0.5

// MinCodeSamples is the number of labelled coding-task outcomes
// a model needs before its MEASURED solve rate is used. Below it
// the model ranks at DefaultSolveRate with Evidence=false — the
// same evidence-gating shape as the learned auto-router's
// MinTierSamples (items 181–182).
const MinCodeSamples = 10

// CodeFailurePenaltyK is the harsh failure penalty multiplier in
// CPS = price × (1/p) × (1 + K·(1−p)). K = 2: a model solving
// 20% of tasks pays 5× attempts and a 2.6× penalty (13× its
// sticker price per solved task), so a cheap failure loses to a
// pricier solve.
const CodeFailurePenaltyK = 2.0

// UnknownPriceCPS is the cost-per-solved-task value for a model
// with no configured price: it has no computable CPS, so it
// sorts after every priced model. It is math.MaxFloat64, NOT
// +Inf: a decision is JSON-encoded for the dashboard and the
// trace, and encoding/json rejects infinities — a +Inf here
// would fail the whole response. The value is a sentinel, never
// a measured cost.
const UnknownPriceCPS = math.MaxFloat64

// MaxExpectedAttempts caps the expected-attempts term 1/p when
// the measured solve rate is 0 (a model that never solved in
// its recorded outcomes): the true expectation is infinite, but
// the cap keeps the value JSON-encodable while still ranking
// the model last among priced candidates (price × 1e6 × the
// full failure penalty dwarfs any solving model's CPS).
const MaxExpectedAttempts = 1e6

// expectedAttempts returns 1/p capped at MaxExpectedAttempts
// (see above): the expected attempts to one solved task.
func expectedAttempts(p float64) float64 {
	if p <= 0 {
		return MaxExpectedAttempts
	}
	if a := 1 / p; a < MaxExpectedAttempts {
		return a
	}
	return MaxExpectedAttempts
}

// TaskOutcome is one model's recorded coding-task evidence:
// Attempts labelled outcomes (one per served coding decision
// with a recorded signal) and Solved the solved credit summed
// over them (accept 1, edit 0.5, discard/regenerate 0).
type TaskOutcome struct {
	Attempts int     `json:"attempts"`
	Solved   float64 `json:"solved"`
}

// SolveRate returns the measured solve rate (Solved/Attempts)
// and true when the model has enough evidence for it to apply
// (Attempts >= MinCodeSamples). Below the floor it returns
// DefaultSolveRate, false — the caller must treat the rate as
// the documented default, never as measured.
func (o TaskOutcome) SolveRate() (float64, bool) {
	if o.Attempts < MinCodeSamples || o.Attempts <= 0 {
		return DefaultSolveRate, false
	}
	p := o.Solved / float64(o.Attempts)
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return p, true
}

// SolveCredit maps one recorded feedback signal to solved credit
// for a coding task: accept solved it, edit solved it after a
// fix (half credit), discard/regenerate failed it. It reports
// false for anything outside the four signals — an unknown
// signal is never silently mapped to a credit.
func SolveCredit(signal string) (float64, bool) {
	switch strings.ToLower(strings.TrimSpace(signal)) {
	case "accept":
		return 1, true
	case "edit":
		return 0.5, true
	case "discard", "regenerate":
		return 0, true
	default:
		return 0, false
	}
}

// CodeOutcomeProvider reports one model's recorded coding-task
// outcome. It is an interface (not an *OutcomeStore) so the
// router stays a leaf: the server wires the live store in, tests
// wire a stub. A nil provider means "no evidence": every model
// ranks at the documented default.
type CodeOutcomeProvider interface {
	CodeOutcome(model string) (TaskOutcome, bool)
}

// CodePriceFunc reports a model's blended price in USD per 1M
// tokens (operator price book, same resolution as the tier
// ranker). It reports false when no price is configured for the
// model: the model then has no computable CPS and sorts last,
// never as free. A nil func means "no prices wired".
type CodePriceFunc func(model string) (float64, bool)

// ---------------------------------------------------------------------------
// Outcome store: the live per-model evidence, fed from feedback.
// ---------------------------------------------------------------------------

// OutcomeStore is the thread-safe in-memory tally of recorded
// coding-task outcomes per model, fed by the server from the
// feedback POST handler (one Record per newly recorded signal;
// duplicates never reach it — the feedback store enforces one
// signal per decision). It records metadata only: model, tier,
// signal — no prompt, no answer, no content of any kind.
//
// SCOPE — CODING TIER ONLY. Record counts a signal only when
// tier is "coding": the mode's evidence is coding-task outcomes,
// and a reasoning/chat/quick-qa decision is not one, even when
// it happened to serve a code prompt. Feedback events carry no
// virtual-model field, so a reasoning-tier decision served under
// auto/code cannot be told apart from one served under
// auto/reasoning — those outcomes are NOT counted (documented
// gap, not silent inclusion).
//
// PERSISTENCE — RESTORED AT STARTUP (item 265). The tally is
// rebuilt from the feedback store at server startup (see
// Restore): the raw signals persist in SQLite, and the server
// loads the per-model outcome query into a fresh store before
// serving, so a model keeps its measured solve rate across
// restarts. Within one run Record still tallies live signals on
// top of the restored tally. The 10-outcome evidence gate still
// applies at scoring time: a restored model below the floor
// ranks at the documented default, exactly like a fresh one.
type OutcomeStore struct {
	mu       sync.RWMutex
	outcomes map[string]TaskOutcome
}

// NewOutcomeStore returns an empty outcome store.
func NewOutcomeStore() *OutcomeStore {
	return &OutcomeStore{outcomes: make(map[string]TaskOutcome)}
}

// Record tallies one recorded signal for model. Only tier
// "coding" counts (see the scope note above); any other tier,
// an empty model, or an unknown signal is a no-op — never an
// error, never a guessed credit.
func (s *OutcomeStore) Record(model, tier, signal string) {
	if s == nil || model == "" || tier != "coding" {
		return
	}
	credit, ok := SolveCredit(signal)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.outcomes[model]
	o.Attempts++
	o.Solved += credit
	s.outcomes[model] = o
}

// Restore replaces the tally with outcomes rebuilt from the
// persisted feedback store (called once at server startup, before
// serving, when a feedback store is wired). It is a rebuild, not
// a merge: the store starts empty at startup, so the restored
// tally IS the recorded history. A nil map is a no-op (empty
// history stays empty). Entries with an empty model, negative
// attempts, negative solved credit, or solved credit above the
// attempt count (impossible — at most 1 credit per outcome) are
// skipped, never stored: a corrupt row must not invent a solve
// rate. Live Record calls after startup tally on top of the
// restored values.
func (s *OutcomeStore) Restore(outcomes map[string]TaskOutcome) {
	if s == nil || outcomes == nil {
		return
	}
	next := make(map[string]TaskOutcome, len(outcomes))
	for model, o := range outcomes {
		if model == "" || o.Attempts < 0 || o.Solved < 0 || o.Solved > float64(o.Attempts) {
			continue
		}
		next[model] = o
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes = next
}

// CodeOutcome implements CodeOutcomeProvider: the recorded tally
// for model, or false when the model has no recorded outcomes.
func (s *OutcomeStore) CodeOutcome(model string) (TaskOutcome, bool) {
	if s == nil {
		return TaskOutcome{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.outcomes[model]
	return o, ok
}

// Snapshot returns a copy of every model's recorded tally (for
// the dashboard evidence feed). Empty when nothing is recorded
// — an honest empty state, never invented traffic.
func (s *OutcomeStore) Snapshot() map[string]TaskOutcome {
	if s == nil {
		return map[string]TaskOutcome{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]TaskOutcome, len(s.outcomes))
	for m, o := range s.outcomes {
		out[m] = o
	}
	return out
}

// ---------------------------------------------------------------------------
// Pool narrowing.
// ---------------------------------------------------------------------------

// CodePoolFilter is the auto/code pool narrowing for one
// decision: allow/exclude lists, capability requirements, and a
// guardrail predicate. All fields are operator configuration;
// zero value means "no narrowing" (every candidate competes).
type CodePoolFilter struct {
	// Allow, when non-empty, is the allowlist: only listed
	// models survive.
	Allow []string
	// Exclude is the denylist: listed models never compete.
	Exclude []string
	// RequireCapabilities lists the capabilities every surviving
	// model must have (e.g. "tools"). A model whose declared
	// capabilities miss any required one is dropped — and a
	// model with NO declared capabilities is dropped too when
	// the list is non-empty, because an unverified capability
	// must not be assumed.
	RequireCapabilities []string
	// Capabilities is the operator-declared per-model capability
	// set (config auto_router.code_capabilities). A model absent
	// from the map has no declared capabilities.
	Capabilities map[string][]string
	// Guardrail, when set, is the pool guardrail: a model it
	// rejects is dropped. The live wiring rejects models
	// deprecated at every provider offering them. A nil
	// guardrail passes every model.
	Guardrail func(model string) bool
}

// NarrowedPool is the result of NarrowPool: the surviving models
// in input order, every dropped model with its reason, and notes
// about narrowing that did not drop anything.
type NarrowedPool struct {
	Models  []string
	Dropped map[string]string
	Notes   []string
}

// NarrowPool applies f to models in order: allowlist, then
// denylist, then capability requirements, then the guardrail
// predicate. It never drops a model silently: every drop lands
// in Dropped with a human-readable reason. The result keeps
// input order among survivors (scoring, not narrowing, decides
// the winner).
func NarrowPool(models []string, f CodePoolFilter) NarrowedPool {
	out := NarrowedPool{Dropped: make(map[string]string)}
	allow := make(map[string]struct{}, len(f.Allow))
	for _, m := range f.Allow {
		if m != "" {
			allow[m] = struct{}{}
		}
	}
	exclude := make(map[string]struct{}, len(f.Exclude))
	for _, m := range f.Exclude {
		if m != "" {
			exclude[m] = struct{}{}
		}
	}
	for _, m := range models {
		switch {
		case len(allow) > 0:
			if _, ok := allow[m]; !ok {
				out.Dropped[m] = "not on the auto/code allow list"
				continue
			}
		}
		if _, bad := exclude[m]; bad {
			out.Dropped[m] = "on the auto/code exclude list"
			continue
		}
		if len(f.RequireCapabilities) > 0 {
			have := make(map[string]struct{}, len(f.Capabilities[m]))
			for _, c := range f.Capabilities[m] {
				have[strings.ToLower(strings.TrimSpace(c))] = struct{}{}
			}
			missing := ""
			for _, req := range f.RequireCapabilities {
				if _, ok := have[strings.ToLower(strings.TrimSpace(req))]; !ok {
					missing = req
					break
				}
			}
			if missing != "" {
				if len(f.Capabilities[m]) == 0 {
					out.Dropped[m] = fmt.Sprintf("capabilities unknown — cannot verify required %q", missing)
				} else {
					out.Dropped[m] = fmt.Sprintf("missing required capability %q", missing)
				}
				continue
			}
		}
		if f.Guardrail != nil && !f.Guardrail(m) {
			out.Dropped[m] = "rejected by the auto/code pool guardrail"
			continue
		}
		out.Models = append(out.Models, m)
	}
	if len(f.Allow) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("allow list applied (%d models)", len(allow)))
	}
	if len(f.RequireCapabilities) > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("required capabilities: %s", strings.Join(f.RequireCapabilities, ", ")))
	}
	return out
}

// ---------------------------------------------------------------------------
// Cost-per-solved-task scoring.
// ---------------------------------------------------------------------------

// CodeScore is one candidate's cost-per-solved-task breakdown for
// a decision: the price, the solve rate (measured or the
// documented default), the evidence behind it, and the resulting
// expected cost per solved task. PriceKnown false means no price
// was configured: CostPerSolved is +Inf and the model sorts last.
type CodeScore struct {
	Model string `json:"model"`
	// PriceUSDPer1M is the blended price used (0 when unknown).
	PriceUSDPer1M float64 `json:"price_usd_per_1m"`
	// PriceKnown reports whether a price was configured.
	PriceKnown bool `json:"price_known"`
	// SolveRate is the measured rate when Evidence is true, the
	// documented DefaultSolveRate otherwise.
	SolveRate float64 `json:"solve_rate"`
	// Evidence reports whether SolveRate is measured (at least
	// MinCodeSamples labelled outcomes) rather than the default.
	Evidence bool `json:"evidence"`
	// Samples is the labelled outcome count behind SolveRate (0
	// when unevidenced).
	Samples int `json:"samples"`
	// ExpectedAttempts is 1/SolveRate: the expected attempts to
	// one solved task.
	ExpectedAttempts float64 `json:"expected_attempts"`
	// FailurePenalty is 1 + K·(1−SolveRate): the harsh failure
	// multiplier.
	FailurePenalty float64 `json:"failure_penalty"`
	// CostPerSolved is price × ExpectedAttempts × FailurePenalty
	// (+Inf when the price is unknown): the expected cost of one
	// solved task. Comparable only within one decision.
	CostPerSolved float64 `json:"cost_per_solved_task"`
}

// CostPerSolved computes the expected cost per solved task for
// one price and outcome: CPS = price × (1/p) × (1 + K·(1−p)).
// p is the measured solve rate when the outcome has enough
// evidence, else the documented DefaultSolveRate; evidence
// reports which. A non-positive or NaN price is treated as
// unknown (UnknownPriceCPS) — a free-or-negative price must
// never rank as cheapest on a broken number. (A genuinely free
// model, price exactly 0, scores CPS 0: free is free.) A zero
// solve rate caps the attempts term at MaxExpectedAttempts
// (see above) so the result stays finite and JSON-encodable.
func CostPerSolved(price float64, outcome TaskOutcome) (cps, p float64, evidence bool) {
	p, evidence = outcome.SolveRate()
	penalty := 1 + CodeFailurePenaltyK*(1-p)
	if math.IsNaN(price) || price < 0 {
		return UnknownPriceCPS, p, evidence
	}
	return price * expectedAttempts(p) * penalty, p, evidence
}

// ScoreCodeModels scores every candidate by expected cost per
// solved task, best (cheapest per solved task) first. Unknown
// prices sort last (CPS +Inf, stable input order among them);
// ties keep input order. It never drops or duplicates a model.
// A nil prices func means "no prices wired": every model is
// unpriced and the input order stands, flagged honestly. A nil
// outcomes provider means "no evidence": every model ranks at
// the documented default.
func ScoreCodeModels(models []string, prices CodePriceFunc, outcomes CodeOutcomeProvider) []CodeScore {
	out := make([]CodeScore, 0, len(models))
	for _, m := range models {
		sc := CodeScore{Model: m}
		var outcome TaskOutcome
		if outcomes != nil {
			outcome, _ = outcomes.CodeOutcome(m)
		}
		sc.Samples = outcome.Attempts
		price, known := 0.0, false
		if prices != nil {
			price, known = prices(m)
		}
		if known && !math.IsNaN(price) && price >= 0 {
			sc.PriceUSDPer1M = price
			sc.PriceKnown = true
		}
		var p float64
		var evidence bool
		if sc.PriceKnown {
			sc.CostPerSolved, p, evidence = CostPerSolved(price, outcome)
		} else {
			_, p, evidence = CostPerSolved(0, outcome)
			sc.CostPerSolved = UnknownPriceCPS
		}
		sc.SolveRate = p
		sc.Evidence = evidence
		sc.ExpectedAttempts = expectedAttempts(p)
		sc.FailurePenalty = 1 + CodeFailurePenaltyK*(1-p)
		out = append(out, sc)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CostPerSolved < out[j].CostPerSolved })
	return out
}

// AnyCodeEvidence reports whether any scored candidate carries
// measured evidence (as opposed to the documented default).
func AnyCodeEvidence(scores []CodeScore) bool {
	for _, sc := range scores {
		if sc.Evidence {
			return true
		}
	}
	return false
}

// codeReason renders the auto/code suffix appended to a
// decision's Reason: the winner's CPS with its price, solve rate
// (measured with sample count, or the documented default), and
// the runner-up, so the trace and header can name the reasoning.
// pool is the narrowing result (its drops are named when any).
// fmtCPS formats a cost-per-solved-task value: "$X.XX", or
// "unknown" when the price was unconfigured (CPS
// UnknownPriceCPS sentinel).
func fmtCPS(cps float64) string {
	if cps >= UnknownPriceCPS {
		return "unknown (no price configured)"
	}
	return fmt.Sprintf("$%.2f", cps)
}

func codeReason(scores []CodeScore, pool NarrowedPool) string {
	if len(scores) == 0 {
		return "; auto/code: pool empty after narrowing — no model to score"
	}
	w := scores[0]
	rate := ""
	if w.Evidence {
		rate = fmt.Sprintf("measured solve rate %.2f from %d coding outcomes", w.SolveRate, w.Samples)
	} else {
		rate = fmt.Sprintf("no task-outcome evidence yet — solve rate assumed %.2f (documented neutral default), ranking by price only", DefaultSolveRate)
	}
	price := "price unknown"
	if w.PriceKnown {
		price = fmt.Sprintf("price $%.2f/1M", w.PriceUSDPer1M)
	}
	s := fmt.Sprintf("; auto/code: %s at %s expected cost per solved task (%s, %s, %.2f expected attempts, failure penalty %.2f)",
		w.Model, fmtCPS(w.CostPerSolved), price, rate, w.ExpectedAttempts, w.FailurePenalty)
	if len(scores) > 1 {
		r := scores[1]
		s += fmt.Sprintf("; runner-up %s %s", r.Model, fmtCPS(r.CostPerSolved))
	}
	if len(pool.Dropped) > 0 {
		dropped := make([]string, 0, len(pool.Dropped))
		for m, why := range pool.Dropped {
			dropped = append(dropped, m+" ("+why+")")
		}
		sort.Strings(dropped)
		s += "; pool dropped: " + strings.Join(dropped, ", ")
	}
	return s
}
