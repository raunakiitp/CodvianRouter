// Package learned is the learned auto-router upgrade (CHECKLIST
// items 181-182, RouteLLM pattern, research-log entry 124):
// a small router model that predicts, per tier, the probability
// that routing a prompt to that tier WINS — i.e. the user keeps
// the answer — from past recorded outcomes.
//
// WHAT IT LEARNS FROM — AND WHAT IT DOES NOT. Training pairs are
// (prompt features, tier served, outcome) where the prompt
// features are the raw per-tier cosine scores the heuristic
// auto-router already recorded at serve time in
// feedback_decisions.tier_scores_json (metadata numbers only),
// and the outcome is the recorded accept / edit / discard /
// regenerate signal for that decision. NO prompt text, NO answer
// text, NO message content is ever stored or read: the feedback
// store has no column for it, and this package has no field for
// it. A decision recorded without scores is excluded, never
// guessed. This is deliberately a LEARNED CALIBRATION of the
// heuristic scores, not a full RouteLLM matrix-factorization
// router: RouteLLM learns from 256-dim query embeddings, which
// would need a schema change to store embeddings per decision;
// that is a documented future step, not this slice. What the
// 4-score feature vector CAN learn is real: e.g. "when the coding
// score only marginally beats chat, coding wins less often than
// the raw score suggests" — the per-tier logistic model below
// learns exactly that shape from the operator's own traffic.
//
// MODEL. One logistic regression per tier (RouteLLM's
// win-probability idea, local edition): for tier t,
// P(win | scores, tier t) = sigmoid(w_t . scores + b_t), trained
// by plain batch gradient descent (stdlib only, deterministic,
// no randomness, no network, no LLM call). The per-tier model is
// trained ONLY on outcomes of decisions actually served to that
// tier — a tier's win probability is never inferred from another
// tier's outcomes.
//
// CALIBRATION — READ BEFORE TRUSTING A NUMBER. A model that
// predicts probabilities must be measured, not believed. Train
// splits the recorded pairs 70/30 BY TIME (first 70% train, last
// 30% test, same split shape as the feedback A/B evaluation):
// the test half reports the expected calibration error (ECE, 5
// equal-width bins) and the log-loss, against the base-rate
// log-loss (predicting the test set's overall win rate for
// everything). The model is ACTIVE only when all of these hold:
// at least MinEvidence (50) labelled outcomes in total, at least
// MinTestSamples (15) test outcomes to measure calibration on,
// ECE <= MaxECE (0.15), and test log-loss beats the base rate.
// Otherwise the status is "learning" (evidence exists but the
// gate has not passed) or "off" (no evidence at all / the
// operator flag is off — the flag lives outside this package, in
// the server wiring: the refresher reports inactive while it is
// off). An uncalibrated or no-evidence model NEVER routes: the
// predictor returns active=false and the auto-router composes
// exactly nothing from it, per the item-115 no-evidence rule.
//
// COMPOSITION. The learned signal is an ADDITIVE, BOUNDED
// adjustment to the heuristic score, never a replacement: for
// tier t the contribution is (P(win|t) - 0.5) * 2 * MaxLearned
// (+/-0.05, the same blast radius as the feedback nudge), so it
// can only move a genuinely borderline decision. A tier with
// fewer than MinTierSamples (10) training outcomes contributes
// exactly 0 (its win probability is reported as 0.5, "unknown").
//
// HONEST LIMITS. (1) The evaluation can only observe the outcome
// of the tier actually served — a high win probability for a tier
// is evidence that tier did well on similar recorded prompts, not
// proof it would beat the alternatives on a new one (same
// counterfactual limit as the item-115 A/B evaluation). (2) The
// 4-score features cannot distinguish two prompts that score the
// same way — synonyms/paraphrases the heuristic already scores
// low stay indistinguishable here too. (3) Signals are sparse
// per tier in practice, so on most deployments this stays in
// "learning" for a long time; that is the design, not a bug.
package learned

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// Tiers is the fixed feature order: the four auto-router tiers in
// the same stable order autorouter.AllTiers uses. Features are
// the raw cosine scores in this order, plus a bias term.
var Tiers = []string{"coding", "chat", "reasoning", "quick-qa"}

// MinEvidence is the total number of labelled outcomes needed
// before the model may activate at all.
const MinEvidence = 50

// MinTestSamples is the number of time-split test outcomes needed
// to measure calibration honestly. With MinEvidence outcomes the
// 70/30 split gives 15 test outcomes, exactly this floor.
const MinTestSamples = 15

// MinTierSamples is the number of training outcomes a single tier
// needs before its per-tier model trains at all: below it the
// tier contributes nothing (win probability 0.5, "unknown").
const MinTierSamples = 10

// MaxECE is the largest expected calibration error the model may
// have and still activate: a model whose stated probabilities are
// further than 0.15 from observed win rates on average is not
// trusted to move routing.
const MaxECE = 0.15

// MaxLearned is the bound on the learned score contribution in
// either direction (+/-0.05): the learned signal can only move a
// borderline decision, exactly like the feedback nudge.
const MaxLearned = 0.05

// Status values for Model.Status.
const (
	// StatusOff means no evidence at all: no labelled outcomes
	// recorded, so there is nothing to learn from.
	StatusOff = "off"
	// StatusLearning means evidence exists but the activation
	// gate (evidence + calibration) has not passed yet.
	StatusLearning = "learning"
	// StatusActive means the gate passed: predictions may
	// compose into routing (bounded, see MaxLearned).
	StatusActive = "active"
)

// Pair is one labelled training example: the prompt's recorded
// per-tier heuristic scores (metadata numbers only — never prompt
// or answer text), the tier actually served, the outcome label,
// and when the outcome was recorded (the time split trains on
// the earlier pairs and measures calibration on the later ones).
//
// Label is the win value of the recorded signal: accept = 1,
// edit = 0.5 (usable but needed fixing), discard / regenerate =
// 0. Labels outside [0, 1] are rejected by Train.
type Pair struct {
	Scores map[string]float64
	Tier   string
	Label  float64
	TS     time.Time
}

// Features converts a score map into the fixed-order feature
// vector (4 scores in Tiers order). Missing tiers score 0 — a
// score map that names no known tier at all yields a zero
// vector, which the model treats as "no signal" (sigmoid of the
// bias alone).
func Features(scores map[string]float64) []float64 {
	x := make([]float64, len(Tiers))
	for i, tier := range Tiers {
		x[i] = scores[tier]
	}
	return x
}

// Model is one trained learned router: a per-tier logistic
// regression plus the calibration measurement that decides
// whether it may activate. The zero Model is inactive (status
// "off", no weights): Predict returns 0.5 for every tier.
type Model struct {
	Status string `json:"status"`
	// Evidence is the total number of labelled outcomes the
	// model was trained/measured on.
	Evidence int `json:"evidence"`
	// TrainSamples / TestSamples are the 70/30 time-split sizes.
	TrainSamples int `json:"train_samples"`
	TestSamples  int `json:"test_samples"`
	// ECE is the expected calibration error measured on the
	// test split (0 when there is no test split); LogLoss the
	// test log-loss, BaseLogLoss the base-rate log-loss on the
	// same test split. Active requires ECE <= MaxECE and
	// LogLoss < BaseLogLoss.
	ECE         float64 `json:"calibration_error"`
	LogLoss     float64 `json:"log_loss"`
	BaseLogLoss float64 `json:"base_log_loss"`
	// PerTierSamples is the labelled-outcome count per tier
	// (ALL usable pairs, not just the training split — this is
	// the evidence the dashboard shows; whether the tier
	// actually trained depends on its TRAIN-split count, see
	// PerTierTrained).
	PerTierSamples map[string]int `json:"per_tier_samples"`
	// PerTierTrained names the tiers whose per-tier model
	// actually trained (at least MinTierSamples outcomes).
	PerTierTrained map[string]bool `json:"per_tier_trained"`
	// TrainedAt is when this model was trained (UTC).
	TrainedAt time.Time `json:"trained_at"`
	// GateNote names, in one line, why the model is in its
	// current status — the honest reason, never a bare label.
	GateNote string `json:"gate_note"`
	// weights[tier] is [w0..w3, bias] for the tier's logistic
	// regression; absent when the tier did not train.
	weights map[string][]float64
}

// sigmoid is the logistic function, overflow-safe.
func sigmoid(z float64) float64 {
	if z >= 0 {
		return 1 / (1 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1 + e)
}

// clipProb keeps a predicted probability out of the exact 0/1
// edges so log-loss stays finite.
func clipProb(p float64) float64 {
	if p < 1e-6 {
		return 1e-6
	}
	if p > 1-1e-6 {
		return 1 - 1e-6
	}
	return p
}

// trainTier fits one tier's logistic regression by batch gradient
// descent on its training pairs: lr 1.0, 2000 epochs, L2
// lambda 0.001. Deterministic (fixed pair order, no randomness),
// stdlib only, and fast: the data is at most tens of thousands
// of 5-number rows. Features are in [0, 1] and labels in
// [0, 1], so the updates are small and the weights stay bounded
// by the L2 term — a tier whose outcomes are all one class still
// converges to a finite weight instead of diverging. (An earlier
// lr 0.1 / 200 epochs, then lr 0.5 / 500 epochs, underfit: on
// cleanly separable test data they predicted ~0.65, then ~0.8,
// for true wins, ECE 0.37 then 0.20 — confident enough to rank
// but not calibrated. The held-out ECE gate is what catches that
// either way; the stronger schedule fits separable data to
// calibrated probabilities.)
func trainTier(pairs []Pair) []float64 {
	w := make([]float64, len(Tiers)+1) // 4 weights + bias, start 0
	const lr = 1.0
	const epochs = 2000
	const lambda = 0.001
	xs := make([][]float64, len(pairs))
	// CLASS WEIGHTING — READ THIS. A tier's served outcomes are
	// usually imbalanced: the tier mostly wins on its own strong
	// prompts, and its losses come mostly from fallback/misrouted
	// prompts the router served it anyway. An unweighted fit lets
	// the majority class swamp the minority: with 20 wins and 2
	// losses the model predicts "win" even for the medium scores
	// the losses actually had, and the held-out ECE gate fails
	// (observed in the live smoke: ECE 0.174 with 20:2 coding
	// outcomes, vs 0.049 on the earlier balanced run). So each
	// example is weighted by inverse class frequency —
	// (nWin+nLoss)/(2*nClass), class split at label 0.5 (an edit
	// at 0.5 counts as the win class for weighting; its label
	// stays 0.5 in the loss itself) — so wins and losses
	// contribute equally to the fit whatever their counts. This
	// is standard practice for imbalanced data; it invents no
	// examples, it only stops the majority class from shouting
	// down the minority. Balanced data is unaffected (both
	// weights are exactly 1). A one-class tier gets weight 1
	// throughout (there is nothing to balance against).
	weights := make([]float64, len(pairs))
	var nWin, nLoss int
	for _, p := range pairs {
		if p.Label >= 0.5 {
			nWin++
		} else {
			nLoss++
		}
	}
	var totalWeight float64
	for i, p := range pairs {
		xs[i] = Features(p.Scores)
		switch {
		case nWin == 0 || nLoss == 0:
			weights[i] = 1
		case p.Label >= 0.5:
			weights[i] = float64(len(pairs)) / (2 * float64(nWin))
		default:
			weights[i] = float64(len(pairs)) / (2 * float64(nLoss))
		}
		totalWeight += weights[i]
	}
	for range epochs {
		grad := make([]float64, len(w))
		for i, p := range pairs {
			x := xs[i]
			z := w[len(Tiers)]
			for j := range Tiers {
				z += w[j] * x[j]
			}
			err := (sigmoid(z) - p.Label) * weights[i]
			for j := range Tiers {
				grad[j] += err * x[j]
			}
			grad[len(Tiers)] += err
		}
		for j := range w {
			grad[j] = grad[j]/totalWeight + lambda*w[j]
			w[j] -= lr * grad[j]
		}
	}
	return w
}

// calibration measures a trained model on the test split: ECE
// over 5 equal-width probability bins plus log-loss, against the
// base-rate log-loss (predicting the test set's overall win rate
// for every outcome). Pairs whose tier did not train are skipped
// (not evaluable) and counted in skipped.
func calibration(m *Model, test []Pair) (ece, logLoss, baseLogLoss float64, evaluated, skipped int) {
	if len(test) == 0 {
		return 0, 0, 0, 0, 0
	}
	var sumLabel float64
	for _, p := range test {
		sumLabel += p.Label
	}
	baseRate := sumLabel / float64(len(test))
	baseLogLoss = logLossOf(baseRate, test)
	bins := make([]struct {
		n            int
		pred, actual float64
	}, 5)
	for _, p := range test {
		w, ok := m.weights[p.Tier]
		if !ok {
			skipped++
			continue
		}
		x := Features(p.Scores)
		z := w[len(Tiers)]
		for j := range Tiers {
			z += w[j] * x[j]
		}
		prob := clipProb(sigmoid(z))
		logLoss += -(p.Label*math.Log(prob) + (1-p.Label)*math.Log(1-prob))
		b := int(prob * 5)
		if b > 4 {
			b = 4
		}
		bins[b].n++
		bins[b].pred += prob
		bins[b].actual += p.Label
		evaluated++
	}
	if evaluated == 0 {
		return 0, 0, baseLogLoss, 0, skipped
	}
	logLoss /= float64(evaluated)
	for _, b := range bins {
		if b.n == 0 {
			continue
		}
		ece += math.Abs(b.pred/float64(b.n)-b.actual/float64(b.n)) * float64(b.n) / float64(evaluated)
	}
	return ece, logLoss, baseLogLoss, evaluated, skipped
}

// logLossOf is the log-loss of predicting the constant rate for
// every test outcome.
func logLossOf(rate float64, test []Pair) float64 {
	rate = clipProb(rate)
	var sum float64
	for _, p := range test {
		sum += -(p.Label*math.Log(rate) + (1-p.Label)*math.Log(1-rate))
	}
	if len(test) == 0 {
		return 0
	}
	return sum / float64(len(test))
}

// Train builds a Model from recorded pairs. Pairs are sorted by
// timestamp and split 70/30 by count (train/test); pairs with an
// unknown tier, a missing/empty score map, or a label outside
// [0, 1] are skipped (never guessed) and counted in Skipped —
// the caller can report that count, but the model itself never
// trains on invented data. A nil/empty pair list yields the
// zero Model (status "off").
func Train(pairs []Pair) *Model {
	m := &Model{
		Status:         StatusOff,
		PerTierSamples: map[string]int{},
		PerTierTrained: map[string]bool{},
		TrainedAt:      time.Now().UTC(),
	}
	usable := make([]Pair, 0, len(pairs))
	for _, p := range pairs {
		if len(p.Scores) == 0 || p.Label < 0 || p.Label > 1 {
			continue
		}
		known := false
		for _, t := range Tiers {
			if p.Tier == t {
				known = true
				break
			}
		}
		if !known {
			continue
		}
		usable = append(usable, p)
	}
	sort.SliceStable(usable, func(i, j int) bool { return usable[i].TS.Before(usable[j].TS) })
	m.Evidence = len(usable)
	if len(usable) == 0 {
		m.GateNote = "no labelled outcomes recorded yet — the learned router has nothing to learn from"
		return m
	}
	cut := len(usable) * 7 / 10
	train, test := usable[:cut], usable[cut:]
	m.TrainSamples = len(train)
	m.TestSamples = len(test)
	m.weights = map[string][]float64{}
	byTier := map[string][]Pair{}
	for _, p := range usable {
		m.PerTierSamples[p.Tier]++
	}
	for _, p := range train {
		byTier[p.Tier] = append(byTier[p.Tier], p)
	}
	for _, tier := range Tiers {
		m.PerTierTrained[tier] = false
		if len(byTier[tier]) >= MinTierSamples {
			m.weights[tier] = trainTier(byTier[tier])
			m.PerTierTrained[tier] = true
		}
	}
	// Calibration is measured even below the evidence floor so
	// the dashboard can show the numbers honestly while the
	// model is still learning.
	ece, ll, base, evaluated, _ := calibration(m, test)
	m.ECE = math.Round(ece*1000) / 1000
	m.LogLoss = math.Round(ll*1000) / 1000
	m.BaseLogLoss = math.Round(base*1000) / 1000
	switch {
	case len(usable) < MinEvidence:
		m.Status = StatusLearning
		m.GateNote = "learning: evidence below the activation floor"
	case evaluated < MinTestSamples:
		m.Status = StatusLearning
		m.GateNote = "learning: too few evaluable test outcomes to measure calibration honestly"
	case evaluated == 0:
		m.Status = StatusLearning
		m.GateNote = "learning: no test outcome could be evaluated (no tier trained)"
	case ece > MaxECE:
		m.Status = StatusLearning
		m.GateNote = "learning: calibration error above the activation ceiling — predictions are not trusted yet"
	case ll >= base:
		m.Status = StatusLearning
		m.GateNote = "learning: predictions do not beat the base-rate predictor on held-out outcomes"
	default:
		m.Status = StatusActive
		m.GateNote = "active: evidence floor met and held-out calibration passes"
	}
	return m
}

// Active reports whether this model's predictions may compose
// into routing.
func (m *Model) Active() bool {
	return m != nil && m.Status == StatusActive
}

// WinProb predicts P(win | scores, tier): the probability that
// routing a prompt with these recorded heuristic scores to tier
// wins (the user keeps the answer). It returns 0.5 ("unknown")
// when the model is not active or the tier never trained —
// never a confident-looking number from an untrusted model.
func (m *Model) WinProb(scores map[string]float64, tier string) float64 {
	if !m.Active() {
		return 0.5
	}
	w, ok := m.weights[tier]
	if !ok {
		return 0.5
	}
	x := Features(scores)
	z := w[len(Tiers)]
	for j := range Tiers {
		z += w[j] * x[j]
	}
	return sigmoid(z)
}

// WinProbs predicts the win probability for every tier at once
// (0.5 for tiers that did not train).
func (m *Model) WinProbs(scores map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(Tiers))
	for _, tier := range Tiers {
		out[tier] = m.WinProb(scores, tier)
	}
	return out
}

// Contribution is the bounded score adjustment the learned
// signal contributes for a tier with win probability p:
// (p - 0.5) * 2 * MaxLearned, clamped to +/-MaxLearned. A
// 0.5 ("unknown") probability contributes exactly 0, and even a
// perfectly confident prediction can move a score by at most
// 0.05 — only a borderline decision can flip.
func Contribution(p float64) float64 {
	c := (p - 0.5) * 2 * MaxLearned
	if c > MaxLearned {
		return MaxLearned
	}
	if c < -MaxLearned {
		return -MaxLearned
	}
	return c
}

// ---------------------------------------------------------------------------
// Refresher: the live holder the server wires into the router.
// ---------------------------------------------------------------------------

// PairSource loads the current training pairs (e.g. from the
// feedback store). It is called on refresh; a failure keeps the
// previous model and is reported in Summary, never fatal.
type PairSource func(ctx context.Context) ([]Pair, error)

// refresherTTL bounds how stale a routing decision's learned
// model can be: at most 30 s after a new signal lands (and 0 s
// after Invalidate, which a new signal triggers).
const refresherTTL = 30 * time.Second

// Refresher holds the current trained Model, retraining from
// the PairSource when stale. Enabled is the operator flag
// (config auto_router.learned_router, default OFF): while it is
// off the model still trains and reports its status honestly,
// but Predict reports inactive so routing is exactly unchanged.
type Refresher struct {
	src      PairSource
	mu       sync.Mutex
	enabled  bool
	model    *Model
	loaded   bool
	loadedAt time.Time
	lastErr  error
}

// NewRefresher builds a Refresher over src. It trains lazily on
// first use (Current/Predict), never at construction, so a slow
// or failing store cannot block server startup.
func NewRefresher(src PairSource, enabled bool) *Refresher {
	return &Refresher{src: src, enabled: enabled}
}

// SetEnabled flips the operator flag. Learning data keeps
// accumulating either way; only the effect on routing is gated.
func (r *Refresher) SetEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
}

// Enabled reports the operator flag.
func (r *Refresher) Enabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enabled
}

// Invalidate forces the next Current/Predict to retrain from the
// source (called when a new signal lands). It resets the load
// TIME, not the loaded flag: on a source error after invalidate
// the previous model is kept (see Current), never dropped.
func (r *Refresher) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loadedAt = time.Time{}
}

// Current returns the current model, retraining from the source
// when never loaded or stale. On a source error the previous
// model is kept (or the zero model when nothing was ever loaded)
// and the error is recorded for Summary.
func (r *Refresher) Current(ctx context.Context) *Model {
	r.mu.Lock()
	if r.loaded && time.Since(r.loadedAt) < refresherTTL {
		m := r.model
		r.mu.Unlock()
		return m
	}
	r.mu.Unlock()
	pairs, err := r.src(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.lastErr = err
		if !r.loaded {
			r.model = Train(nil)
			r.loaded = true
			r.loadedAt = time.Now()
		}
		return r.model
	}
	r.lastErr = nil
	r.model = Train(pairs)
	r.loaded = true
	r.loadedAt = time.Now()
	return r.model
}

// Predict returns per-tier win probabilities for scores plus
// whether they may compose into routing (model active AND the
// operator flag on). It never blocks routing on a slow store:
// the cached model is used when fresh.
func (r *Refresher) Predict(ctx context.Context, scores map[string]float64) (map[string]float64, bool) {
	m := r.Current(ctx)
	r.mu.Lock()
	enabled := r.enabled
	r.mu.Unlock()
	if !enabled || !m.Active() {
		probs := make(map[string]float64, len(Tiers))
		for _, tier := range Tiers {
			probs[tier] = 0.5
		}
		return probs, false
	}
	return m.WinProbs(scores), true
}

// Summary is the GET /dashboard/learned payload: the operator
// flag, the model's status and calibration, the evidence counts,
// and the last load error ("" when the last load succeeded).
type Summary struct {
	Enabled        bool            `json:"enabled"`
	Status         string          `json:"status"`
	GateNote       string          `json:"gate_note"`
	Evidence       int             `json:"evidence"`
	TrainSamples   int             `json:"train_samples"`
	TestSamples    int             `json:"test_samples"`
	CalibrationErr float64         `json:"calibration_error"`
	LogLoss        float64         `json:"log_loss"`
	BaseLogLoss    float64         `json:"base_log_loss"`
	MinEvidence    int             `json:"min_evidence"`
	MaxECE         float64         `json:"max_calibration_error"`
	MaxLearned     float64         `json:"max_learned"`
	PerTierSamples map[string]int  `json:"per_tier_samples"`
	PerTierTrained map[string]bool `json:"per_tier_trained"`
	LastErr        string          `json:"last_error,omitempty"`
}

// Summary returns the current status for the dashboard, training
// first when nothing is loaded yet.
func (r *Refresher) Summary(ctx context.Context) Summary {
	m := r.Current(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Summary{
		Enabled:        r.enabled,
		Status:         m.Status,
		GateNote:       m.GateNote,
		Evidence:       m.Evidence,
		TrainSamples:   m.TrainSamples,
		TestSamples:    m.TestSamples,
		CalibrationErr: m.ECE,
		LogLoss:        m.LogLoss,
		BaseLogLoss:    m.BaseLogLoss,
		MinEvidence:    MinEvidence,
		MaxECE:         MaxECE,
		MaxLearned:     MaxLearned,
		PerTierSamples: m.PerTierSamples,
		PerTierTrained: m.PerTierTrained,
	}
	if r.lastErr != nil {
		s.LastErr = r.lastErr.Error()
	}
	// The dashboard must never show "active" while the operator
	// flag is off: the model may be evidence-ready, but routing
	// does not use it.
	if !r.enabled && s.Status == StatusActive {
		s.Status = StatusLearning
		s.GateNote = "ready on evidence, but the operator flag is off — enable with auto_router.learned_router"
	}
	return s
}
