# Learned auto-router (items 181–182)

RouteLLM pattern, local edition: a small router model predicts,
per tier, the probability that routing a prompt to that tier
WINS — the user keeps the answer — from past recorded outcomes.
It is a **learned calibration of the heuristic scores**, not a
replacement for them, and it stays OFF until real evidence
activates it.

## What it learns from

Training pairs are `(prompt features, tier served, outcome)`:

- **Prompt features**: the raw per-tier cosine scores the
  heuristic auto-router already recorded at serve time in
  `feedback_decisions.tier_scores_json` (metadata numbers only).
- **Outcome**: the recorded accept / edit / discard / regenerate
  signal for that decision, mapped to a win label
  (accept = 1, edit = 0.5, discard / regenerate = 0).
- **No content, ever**: no prompt text, no answer text, no
  message content is stored or read. The feedback store has no
  column for it, `internal/learned` has no field for it, and
  `feedback.TrainingPairs` excludes decisions recorded without
  scores (never guessed).

Deliberate scope limit: RouteLLM learns from 256-dim query
embeddings. That would need a schema change to store an
embedding per decision; this slice does not make it. The
4-score feature vector can still learn real structure — e.g.
"when the coding score only marginally beats chat, coding wins
less often than the raw score suggests" — which is exactly what
the per-tier logistic models learn from the operator's own
traffic. Richer embedding features are a documented future step.

## Model

One logistic regression per tier
(`internal/learned`, stdlib only, deterministic batch gradient
descent, no randomness, no network, no LLM call):

```
P(win | scores, tier t) = sigmoid(w_t . scores + b_t)
```

A tier's model trains ONLY on outcomes of decisions actually
served to that tier, and only when it has at least 10 training
outcomes (`MinTierSamples`); below that the tier contributes
nothing (win probability reported as 0.5, "unknown").

**Class weighting.** A tier's served outcomes are usually
imbalanced — mostly wins on its own strong prompts, losses
mostly from fallback/misrouted prompts. An unweighted fit lets
the majority class swamp the minority (observed in the live
smoke: 20 wins vs 2 losses for coding gave ECE 0.174, over the
0.15 ceiling). Each example is therefore weighted by inverse
class frequency, so wins and losses contribute equally whatever
their counts; balanced data is unaffected (both weights are 1).
This invents no examples — it only stops the majority class
from shouting down the minority.

## Activation gate (calibration, measured not believed)

Train splits recorded pairs 70/30 **by time** (same split shape
as the item-115 A/B evaluation) and measures on the test split:

- **ECE** (expected calibration error, 5 equal-width bins),
- **log-loss** vs the **base-rate log-loss** (predicting the test
  set's overall win rate for everything).

The model is **active** only when ALL of these hold:

| Condition | Value |
|---|---|
| Total labelled outcomes | ≥ 50 (`MinEvidence`) |
| Evaluable test outcomes | ≥ 15 (`MinTestSamples`) |
| Held-out ECE | ≤ 0.15 (`MaxECE`) |
| Held-out log-loss | beats the base rate |

Otherwise the status is `learning` (evidence exists but the
gate has not passed) or `off` (no evidence at all). The
operator flag `auto_router.learned_router` (default OFF) is a
second, independent gate: while it is off the model still
trains and reports its status honestly, but its predictions
never apply to routing. An uncalibrated or no-evidence model
NEVER routes (item-115 no-evidence rule).

## Composition

The learned signal is an **additive, bounded** adjustment to the
heuristic score, on top of the feedback nudge:

```
contribution(t) = (P(win|t) - 0.5) * 2 * 0.05, clamped to ±0.05
```

A 0.5 ("unknown") probability contributes exactly 0, and even a
perfectly confident prediction can move a score by at most
0.05 — only a borderline decision can flip. The decision JSON
carries `learned_win_prob` (per tier) and `learned_active`
only when the model is active; otherwise those fields are
absent, never a hidden guess.

## Wiring

- `feedback.TrainingPairs` — loads pairs from the feedback store.
- `learned.Refresher` — holds the current trained model,
  retrains from the store when stale (30 s TTL) or after a new
  signal lands (`Invalidate` on `POST /dashboard/feedback`);
  on a load error it keeps the previous model and reports the
  error in the summary.
- Server wires it in `New` (auto-router + feedback store both
  present) via `WithLearnedRouter(enabled)`; `main.go` passes
  the config flag and prints the status at startup.
- `GET /dashboard/learned` — status, evidence, calibration
  error, log-loss, per-tier samples. Auth-exempt dashboard feed
  (counts only).
- Dashboard Auto-router view — "Learned auto-router — win
  probabilities" card fed by the endpoint on the same cycle as
  the feedback card; decision views show the learned win
  probabilities when active.

## Honest limits

1. **Counterfactual limit** (same as item 115): the evaluation
   can only observe the outcome of the tier actually served. A
   high win probability is evidence that tier did well on
   similar recorded prompts, not proof it would beat the
   alternatives on a new one.
2. **Feature limit**: the 4-score features cannot distinguish
   two prompts that score the same way. Synonyms/paraphrases
   the heuristic already scores low stay indistinguishable here.
3. **Sparsity**: signals are sparse per tier in practice, so on
   most deployments this stays in `learning` for a long time.
   That is the design, not a bug.
4. **In-memory model**: the trained weights live in the
   refresher and are rebuilt from `feedback.db` on restart
   (retrain is seconds at most for a local store); a restart
   never invents a model — it retrains from the same recorded
   evidence.
5. Never market RouteLLM's 85% savings headline or any savings
   figure: this slice measures calibration only, and reports no
   cost or quality improvement claim at all.
