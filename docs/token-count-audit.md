# Token-count accuracy audit (CHECKLIST 229-230)

**Date:** 2026-11-18 · **Harness:** `internal/router/token_audit_test.go`
(`TestTokenCountAudit`) · **Corpus:** `internal/tokencount/audit.go`
(18 cases, 3 per class) · **Runs:** 90 requests (5 wires × 18 cases),
every sample read back from the real ledger.

## What was measured, and what it is not

The whole budget / ledger / pricing stack trusts the local tiktoken
estimate (items 145-146), but its drift per provider wire had never
been measured. This audit measures it: the fixed corpus is sent
through each wire — OpenAI, Anthropic, Gemini, Cohere, Bedrock —
using the **real provider clients and the real engine**, against
local mocks that return **known provider-reported usage**, and the
(local, reported) pair is read back out of the **real ledger row**
(`prompt_tokens` = reported, `local_estimate_prompt_tokens` =
local). Error % = 100 × (local − reported) ÷ reported. Negative
means the local estimate **undercounts** what the provider bills —
the dangerous direction for a fail-closed budget.

**The simulated provider — read this before trusting a number.**
A mock cannot run Anthropic's, Gemini's, Cohere's or Bedrock's real
tokenizer (no local implementation exists for them), so the mocks
simulate a provider that bills the **wire payload it receives**,
tokenized with the same BPE the router used. That isolates the
**coverage drift** — what text the estimator ignores (roles, tool
schemas, per-message framing) — from the **vocabulary drift** (a
different tokenizer splitting the same text differently), which
this audit **cannot measure** and which remains an open unknown for
every non-OpenAI wire. Per-wire mock rules:

- **OpenAI:** the documented cookbook rule — per message the BPE
  count of its content (+ tool-call arguments) + 3 tokens, +1 with
  a name, +3 reply priming, plus the BPE count of the canonical
  JSON of the tool definitions.
- **Anthropic / Cohere:** BPE(system text) + per message BPE(role)
  + BPE(content text, tool calls as name + arguments) + BPE
  (canonical JSON of the tool definitions).
- **Gemini / Bedrock:** the same rule over their wire shapes —
  system instruction / system array, per-content role + parts
  (text as text; functionCall / functionResponse / toolUse /
  toolResult blocks as their semantic content — function name +
  arguments/response JSON, tool-result content as text), plus the
  canonical JSON of the tool declarations / tool config.

Tool-call blocks are counted by **semantic content, not JSON
transport syntax**: the wrapper keys (`type`, `id`,
`tool_use_id`) and JSON escaping are framing no provider
documents billing; counting them as canonical JSON was tried
first and inflated every tool turn by the wrapper (it pushed
the Bedrock tool-conversation case to −26.78% in an
intermediate run — a mock artefact, not an estimator gap — and
the mock was corrected to semantic counting). Completion
usage is the BPE count of the fixed reply text `audit-ok`; the
audit compares **prompt tokens only**. Streaming is not audited:
the engine records streaming attempts as unknown usage (its
standing rule), so there is no reported number to compare.

## Baseline: the old estimator (message text only)

`tokencount.CountPrompt` counted message content, text parts and
tool-call arguments — and nothing else. Measured against the mocks:

| Wire | Corpus class | n | Mean error % | Mean |error| % | Max |error| % |
|---|---|---|---|---|---|
| anthropic | cjk | 3 | -1.51 | 1.51 | 1.56 |
| anthropic | code | 3 | -1.03 | 1.03 | 1.37 |
| anthropic | english | 3 | -2.98 | 2.98 | 7.14 |
| anthropic | hinglish | 3 | -0.77 | 0.77 | 1.18 |
| anthropic | long-repetitive | 3 | -0.11 | 0.11 | 0.14 |
| anthropic | tool-call-heavy | 3 | -78.10 | 78.10 | 88.51 |
| bedrock | cjk | 3 | -1.51 | 1.51 | 1.56 |
| bedrock | code | 3 | -1.03 | 1.03 | 1.37 |
| bedrock | english | 3 | -2.98 | 2.98 | 7.14 |
| bedrock | hinglish | 3 | -0.77 | 0.77 | 1.18 |
| bedrock | long-repetitive | 3 | -0.11 | 0.11 | 0.14 |
| bedrock | tool-call-heavy | 3 | -80.80 | 80.80 | 90.20 |
| cohere | cjk | 3 | -1.51 | 1.51 | 1.56 |
| cohere | code | 3 | -1.03 | 1.03 | 1.37 |
| cohere | english | 3 | -5.63 | 5.63 | 13.33 |
| cohere | hinglish | 3 | -1.13 | 1.13 | 2.22 |
| cohere | long-repetitive | 3 | -0.11 | 0.11 | 0.14 |
| cohere | tool-call-heavy | 3 | -79.19 | 79.19 | 89.25 |
| gemini | cjk | 3 | -1.51 | 1.51 | 1.56 |
| gemini | code | 3 | -1.03 | 1.03 | 1.37 |
| gemini | english | 3 | -2.98 | 2.98 | 7.14 |
| gemini | hinglish | 3 | -0.77 | 0.77 | 1.18 |
| gemini | long-repetitive | 3 | -0.11 | 0.11 | 0.14 |
| gemini | tool-call-heavy | 3 | -78.52 | 78.52 | 89.01 |
| openai | cjk | 3 | -12.27 | 12.27 | 13.95 |
| openai | code | 3 | -5.90 | 5.90 | 7.79 |
| openai | english | 3 | -18.67 | 18.67 | 40.91 |
| openai | hinglish | 3 | -8.79 | 8.79 | 10.00 |
| openai | long-repetitive | 3 | -0.70 | 0.70 | 0.85 |
| openai | tool-call-heavy | 3 | -80.71 | 80.71 | 90.10 |

Two systematic gaps, both in the dangerous direction:

1. **Tool definitions were never counted** — −78% to −81% mean
   error on tool-call-heavy prompts on **all five wires**. A
   tool-heavy request the gate priced at 60 tokens was billed
   ~350. Any fail-closed budget decision on a tool-using request
   was wrong by up to 5×.
2. **Per-message framing was never counted** — OpenAI's cookbook
   overhead (3/message + 3 priming) is −6% to −19% on OpenAI
   prompts of every class (−40.91% worst case: the 13-token
   `eng-short` prompt, where 9 framing tokens dominate). Native
   wires were only −0.1% to −3% — a role label per message.

## The fix

`tokencount.CountPromptWire(model, wire, req)` — message text, **plus
the tool definitions** (BPE count of the tools-array JSON, the same
JSON the wire clients serialise), **plus per-wire framing**: the
OpenAI cookbook rule (3/message + 3 priming) for the OpenAI wire,
one role-label token per message for native wires. Bedrock gets its
tools JSON reshaped the way Converse receives it
(`toolSpec` + `inputSchema.json` + `toolChoice {"auto":{}}`,
mirroring `wire.convertBedrockTools`) — the OpenAI shape misses
that reshaping by ~9 tokens a request, a systematic undercount on
tool-heavy Bedrock traffic. `CountPromptFull` is the wire-agnostic
reference (OpenAI rule) for callers that estimate **before**
routing: the pre-send budget gates
(`internal/server/requestbudget.go`, `server.go` budget check,
`keytraffic.go` TPM estimator), the engine's strict-budget gate
and the context-window fit check. The router's reconciliation
(`reconcileUsage`) uses the per-wire count from the serving
client's `WireFormat()`. `CountPrompt` itself is unchanged
(message text only — compression's before/after comparison still
uses it, consistently on both sides).

## Re-measurement: after the fix

| Wire | Corpus class | n | Mean error % | Mean |error| % | Max |error| % |
|---|---|---|---|---|---|
| anthropic | cjk | 3 | +0.00 | 0.00 | 0.00 |
| anthropic | code | 3 | +0.00 | 0.00 | 0.00 |
| anthropic | english | 3 | +2.98 | 2.98 | 7.14 |
| anthropic | hinglish | 3 | +0.53 | 0.53 | 1.12 |
| anthropic | long-repetitive | 3 | +0.00 | 0.00 | 0.00 |
| anthropic | tool-call-heavy | 3 | +5.01 | 5.01 | 6.90 |
| bedrock | cjk | 3 | +0.00 | 0.00 | 0.00 |
| bedrock | code | 3 | +0.00 | 0.00 | 0.00 |
| bedrock | english | 3 | +2.98 | 2.98 | 7.14 |
| bedrock | hinglish | 3 | +0.53 | 0.53 | 1.12 |
| bedrock | long-repetitive | 3 | +0.00 | 0.00 | 0.00 |
| bedrock | tool-call-heavy | 3 | -2.31 | 2.31 | 5.96 |
| cohere | cjk | 3 | +0.00 | 0.00 | 0.00 |
| cohere | code | 3 | +0.00 | 0.00 | 0.00 |
| cohere | english | 3 | +0.00 | 0.00 | 0.00 |
| cohere | hinglish | 3 | +0.16 | 0.16 | 0.48 |
| cohere | long-repetitive | 3 | +0.00 | 0.00 | 0.00 |
| cohere | tool-call-heavy | 3 | -0.31 | 0.67 | 1.47 |
| gemini | cjk | 3 | +0.00 | 0.00 | 0.00 |
| gemini | code | 3 | +0.00 | 0.00 | 0.00 |
| gemini | english | 3 | +2.98 | 2.98 | 7.14 |
| gemini | hinglish | 3 | +0.53 | 0.53 | 1.12 |
| gemini | long-repetitive | 3 | +0.00 | 0.00 | 0.00 |
| gemini | tool-call-heavy | 3 | +2.52 | 2.52 | 3.08 |
| openai | cjk | 3 | +0.00 | 0.00 | 0.00 |
| openai | code | 3 | +0.00 | 0.00 | 0.00 |
| openai | english | 3 | +0.00 | 0.00 | 0.00 |
| openai | hinglish | 3 | +0.00 | 0.00 | 0.00 |
| openai | long-repetitive | 3 | +0.00 | 0.00 | 0.00 |
| openai | tool-call-heavy | 3 | -0.81 | 0.81 | 0.99 |

The audit test enforces, per (wire, class) cell: mean error
≥ −5% and max |error| ≤ 20%. Every cell passes, most exactly.

## Budget-safety verdicts per wire

- **OpenAI — SAFE.** Residual 0.00% on every non-tool class and
  −0.81% on tool-heavy (max 0.99%). Pre-send gates using the
  OpenAI reference rule are exact under the cookbook model.
- **Anthropic — SAFE, with a noted overcount.** Tool-heavy
  +5.01% mean (max 6.90%): the Anthropic reshaped tools JSON is
  a few tokens per tool shorter than the OpenAI shape the
  estimator counts. Overcounting is the safe direction — budgets
  refuse marginally early on tool-heavy Anthropic traffic, never
  late. English +2.98% is the +1-token system-message role
  residual (see below).
- **Gemini — SAFE.** Same shape as Anthropic: tool-heavy +2.52%
  overcount (safe direction), english +2.98%.
- **Cohere — SAFE.** Every cell ≤ 0.67% mean |error|.
- **Bedrock — SAFE after the fix.** Tool-heavy −2.31% mean (max
  5.96%) — inside the router's materiality floor (20 tokens /
  10%); the remaining gap is toolConfig key-order / merge noise
  of a few tokens. Before the fix it was −80.80%: the wire whose
  budget decisions were most at risk.

## Known residuals (all safe-direction, ≤ 1 token per request)

- **+1 token on requests with a system message** (Anthropic,
  Gemini, Bedrock english +2.98%, hinglish +0.53%): the
  estimator adds one role token per message including system
  messages, but those wires carry system outside the message
  list, with no role token. Worst case +7.14% on the 14-token
  `eng-short` prompt; ≤ 0.5% on prompts ≥ 200 tokens.
  Deliberately left: removing it would mean special-casing
  system messages per wire for a 1-token gain, and the
  `CountPrompt` newline joining already contributes ±1 token
  per message through BPE merges — chasing the last token
  would overfit the estimator to this mock.
- **Anthropic/Gemini tool reshaping** (+5.01% / +2.52% on
  tool-heavy): the estimator counts the OpenAI-shape tools
  JSON; those wires' reshaped JSON is a few tokens shorter.
  Safe direction; not chased for the same reason.
- **Vocabulary drift is unmeasured** (see the simulation note
  above): a real Anthropic/Gemini/Cohere/Bedrock tokenizer can
  split the same text differently from cl100k/o200k. The
  dashboard's live drift view is the tripwire — if real traffic
  shows a systematic undercount on a wire, it appears there
  first, and the fix is a measured per-wire correction, never a
  guessed constant.

## Live drift (dashboard)

The ledger now records the local estimate alongside the
provider-reported split on every row
(`local_estimate_prompt_tokens` column; `Ledger.DriftSummary`;
`UsageSummary.Drift`), and the Usage view renders it per
(provider, model) from `GET /dashboard/usage` — compared count,
mean error, mean |error|, max |error| — from live traffic only.
Rows without both numbers are excluded, never treated as
agreement; an empty ledger shows the honest empty state.

## What the live smoke caught: `wireFor` missed keyed clients

The audit harness builds its clients with
`provider.NewClient(..., provider.WithWireFormat(...))` under one
bare provider identity, so `Engine.wireFor` always found them and
the table above is the estimator's true per-wire behaviour. In
production, the engine's clients map is keyed by
(provider, key) identities (`ClientKey`, CHECKLIST 175): a
provider with a key pool is stored under `"provider\x00key"`,
never under the bare provider ID — at startup and live-added
alike. `wireFor` did a bare-ID lookup only, so it missed every
such client and silently fell back to the OpenAI-reference
framing. The live smoke (`scripts/smoke-token-count-audit.py`,
real binary + OpenAI/Anthropic/Bedrock mocks) caught it: a
two-message Anthropic request measured a local estimate of 26
against 18 reported (**+44.44%**), and Bedrock the same — the
OpenAI rule (3/message + 3 priming) applied where the native
rule (1/message) belongs. The fix (`internal/router/router.go`):
`wireFor` looks up the bare identity first, then scans for any
keyed client of the same provider — the wire format is a
property of the provider, not the key, so any match is the right
one. Regression test `TestWireForFindsKeyedNativeClient`
(sensitivity-proven: fails 9 vs 4 without the fix). After the
fix the smoke re-measures **16/16 green**: OpenAI 26 vs 26
(0.00%), Anthropic 19 vs 18 (+5.56% — the documented +1-token
system-message residual), Bedrock 19 vs 18 (+5.56%),
tool-call-heavy OpenAI 116 vs 115 (+0.87%), the strict-budget
402 proof, and live drift rows for all three wires. The
overcount was in the safe direction for budgets (refuse early,
never late), but it inflated the drift view, the divergence
stats and every fallback count on native wires — it is fixed,
not documented away.
