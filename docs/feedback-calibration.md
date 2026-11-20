# Feedback routing calibration — calibrated heuristic, not RL

`internal/feedback` turns accumulated user outcome signals
(accept / edit / discard / regenerate) into small bounded
per-tier score nudges for the auto-router. This note documents
the exact formula, the time decay, and what is deliberately
*not* claimed.

**Honest label: this is a calibrated heuristic, not
reinforcement learning.** There is no model training, no
gradient, no exploration policy, and no reward model. It is a
bounded rate adjustment computed from counted outcomes. Do not
describe it as RL, "learning" in the ML sense, or "proven" —
it is a prototype adjustment whose effect is capped so small
that it can only move genuinely borderline routing decisions.

## Formula

Per tier, each signal has a point value (unchanged from the
original fixed rule):

| signal | points |
|---|---|
| accept | +1 |
| edit | −0.5 (usable, but needed fixing) |
| discard | −1 |
| regenerate | −1 |

Each signal is time-decayed (see below) with weight
`w = 2^(−age / half-life)`:

```
effective samples  N = Σ w_i
decayed net points P = Σ points_i · w_i
rate               r = P / N            ∈ [−1, +1]
nudge                  = r × MaxNudge, clamped to ±MaxNudge
```

`MaxNudge = 0.05`. The nudge is added to the tier's raw cosine
score in `autorouter.Classify` (clamped to [0, 1]).

## Why a rate, not net points

The original fixed rule was `nudge = net-points × 0.005`. It
conflated *volume* with *quality*: 10 accepts already saturated
the +0.05 cap, so a heavily-used mediocre tier could out-nudge a
rarely-used excellent one, and a tier's nudge kept growing with
traffic rather than with how good it was. The calibrated rate
fixes that: the nudge now reflects the tier's *observed success
rate*. 10/10 accepts still gives +0.05 and 10/10 discards −0.05,
but 6 accepts + 4 discards gives rate 0.2 → +0.01, 10 accepts +
10 discards gives rate 0 → exactly 0 (active, but neutral), and
9 accepts + 1 discard (rate 0.8 → +0.04) equals 90 accepts +
10 discards — volume alone no longer moves the nudge.

## Time decay

**Half-life: 7 days.** A signal weighs 1 when fresh, 0.5 after
a week, 0.25 after two weeks, ~0.06 after a month (4 half-lives),
and is effectively forgotten after ~30 days.

Why 7 days: signals are sparse per tier in this prototype, so a
1-day half-life would erase evidence before 10 effective samples
ever accumulate, while a 30-day half-life would let a tier's
behaviour from a month ago dominate routing long after it
changed. Seven days sits between the two failure modes.

Decay is computed **at read time** from the stored signal
timestamp (`feedback_events.ts`) — there is no background job
and no deletion. Old rows stay in `feedback.db` (auditable) but
contribute ~0 weight. Because the sample floor is measured in
*effective* samples, a tier whose evidence has all decayed
falls back below the floor and loses its nudge entirely.

## Kept: 10-signal minimum and ±0.05 clamp

Both are unchanged, and the floor is now *stricter* than
before: it is measured in decay-weighted effective samples,
which are always ≤ the raw count. A handful of signals — or a
pile of stale ones — can still never move routing. The clamp is
now a safety net rather than the main limiter (|rate| ≤ 1
already bounds the nudge); it is kept so a future formula change
cannot silently widen the blast radius. The old `Step` (0.005)
constant is retained for API compatibility (`Summary.step`) but
no longer drives the nudge.

One numerical note: the floor comparison uses a 1e-6 tolerance,
so millisecond timestamp noise cannot keep a tier with 10
genuinely fresh signals below the floor (10 signals a few ms old
sum to 10 − ~1e-8, not exactly 10). The tolerance is far below
one signal and cannot activate a tier meaningfully short of
evidence.

## Flag default

Learning stays behind `auto_router.feedback_learned_weights`,
**default OFF**. With the flag off the store still records
signals, but `LearnedWeights` returns all-zero nudges, so
routing is exactly unchanged. Turning it on later applies the
signals accumulated while it was off (decayed to the present).

## Privacy — unchanged

Still local-only, still metadata-only: decision id, tier,
model, decision latency, signal, timestamp. No prompts, no
answers, no message content — there is no column for them and no
code path that accepts them. `TestSchemaStoresNoContent` asserts
this with a forbidden-word check, an exact column allowlist, and
a scan of the schema SQL.

## Limitations (honest)

- **Not proven RL.** No evidence this improves routing quality
  in production; the cap exists precisely because the effect is
  unverified. It needs real traffic and an A/B-style comparison
  before any stronger claim.
- **Signal sparsity.** A tier may take weeks to reach 10
  effective samples; until then it gets no nudge at all.
- **Decay vs. floor interaction.** A tier with old-but-real
  evidence can lose its nudge as that evidence decays, even if
  nothing about the tier changed — that is the intended
  trade-off (fresh evidence wins), but it means nudges can
  flicker off in low-traffic tiers.
- **No per-model calibration.** The nudge is per tier, not per
  model: a bad model inside a good tier is not penalised
  separately.
- **Self-selection bias.** Signals only arrive when a user
  bothers to accept/edit/discard; silent dissatisfaction is
  invisible, and a regenerate is only observed if the client
  reports it.
- **Effective samples are not in `TierWeight`.** The
  `autorouter.TierWeight` struct (owned by the autorouter
  package, out of scope for this slice) reports raw `Samples`
  only; the decay-weighted effective sample count is visible in
  the dashboard `Summary` (`effective_samples` per tier), not in
  the weights the router consumes.

## A/B evaluation (item 115)

`internal/feedback/eval.go` + `codvian-router eval-feedback` compare
the calibrated heuristic against the no-feedback baseline on the
recorded local traffic in `feedback.db`:

- **Instrumentation first.** Every served auto-routing decision
  now records its raw per-tier cosine scores
  (`feedback_decisions.tier_scores_json` — numbers only, metadata
  like the latency, no prompts ever). Without scores a decision
  cannot be replayed, so pre-A/B decisions are excluded from the
  evaluation, never guessed. Old stores gain the column by
  migration on open.
- **Method.** Train/test split by time (default 70/30 by signal
  count; `--cutoff` overrides): train signals freeze into the
  per-tier nudges (decay measured at the cutoff), test decisions
  are the signaled decisions after it. Baseline picks
  `argmax(raw scores)`; feedback picks `argmax(raw scores +
  train nudges)`. Decisions whose served tier is not the raw
  argmax are excluded — the router served a threshold fallback or
  a restricted candidate set there. The outcome is the recorded
  signal's points for the *served* tier; a flip's value is the
  outcome avoided (avoiding a discard scores +1, avoiding an
  accept scores −1). Feedback wins only when avoided points per
  flip are positive; a tie keeps the baseline.
- **Honest limit.** The evaluation observes only the tier
  actually served. It cannot measure what the alternative tier
  would have done for the same prompt. A flip away from a bad
  outcome is evidence the heuristic avoids tiers users rated
  badly, not proof the destination would have done better. The
  verdict is evidence, not proof — and the dashboard shows it
  verbatim in the Feedback section (`GET /dashboard/feedback`
  `eval`), including a baseline win.

**Measured result (this machine, 2026-11-18): no evidence yet.**
The router in this VM has served only bench/mock traffic and no
user has recorded outcome signals, so `eval-feedback` reports
`no-evidence` and learning stays OFF (the default). The eval
mechanics are verified by unit tests with fixture traffic
(feedback-wins, baseline-wins, no-flip, no-evidence, fallback
exclusion) and by a live served-request test that scores
round-trip from decision to store. The verdict on real traffic
is still pending — it needs a user actually rating decisions.
