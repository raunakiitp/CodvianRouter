# Compression & Fidelity Benchmark (P1.4)

Status: **BUILT & MEASURED 2026-11-24** — reproducible benchmark,
deterministic corpora, fidelity proxies with explicit thresholds, and
machine-readable JSON output. Entry points:

```bash
# runnable binary (JSON or text)
codvian-bench compression -repeats 3
codvian-bench compression -corpus all -json
codvian-bench compression -corpus all -json -stable   # latency omitted: byte-reproducible artifact

# library tests (same measurement, prints the source-of-truth table)
go test ./internal/compress/evalharness/ -run TestPrintCompressionBenchmark -v -count=1
go test ./cmd/codvian-bench/ -run TestCompressionSubcommand -count=1
```

## 1. Audit — what exists (preserved, not rebuilt)

| Area | Implementation | Notes |
|---|---|---|
| Lossless steps | `compress.applyLossless` — redundant-whitespace strip, system/developer dedupe, tool-result canonicalisation (last per `tool_call_id`) | `VerifyLossless` proves the output is exactly `Normalise(input)` — structural, not semantic |
| Lossy trim | `compress.applyLossy` — whole middle turns dropped oldest-first (system/developer, last user, last message of any role pinned), then head+tail truncation with an inline marker | Opt-in only (`compression.lossy_max_prompt_tokens` per request, or `lossless_models` config); never configured silently |
| Lossy fidelity gate | `compress.CheckLossy` | Code fences, complete JSON values, URLs survive whole or drop whole; message order/roles/tool ids/part-carrying messages preserved; on doubt it refuses and the original is sent (`Applied=false`, `StepFidelityGateRefused`) |
| Named modes | `ModeOff/Lite/Standard/Aggressive/Ultra/RTK/Stacked` + style engines (terse, less-code, ponytail, adhd, terse-CJK) | All lossy; run only under the opt-in; `Apply`'s plain path runs none of them |
| Server integration | `applyCompressionPrePass` + `applyCompression` (server), `router.maybeCompress` at every engine entry point | After guardrails, before budget gates and before the coalescing key; headers `X-Codvian-Compression*`; trace `CompressionInfo`; ledger columns `prompt_before_compression_tokens` / `compression_saved_tokens` |
| Prior harness | `internal/compress/evalharness` token-savings table | Kept intact; this work adds the P1.4 layer around it |

## 2. What P1.4 added

- **Fidelity metrics** (`evalharness/fidelity.go`) — six deterministic proxies with raw counts:
  instruction preservation, tool-call preservation, tool-result
  preservation, JSON preservation (+ splice detection), fact
  preservation, prose-word preservation. Definitions and limits are
  documented in the file; **they are structural proxies, not semantic
  proofs**.
- **Latency** — min/p50/max wall-clock `Apply` over a fixed repeat
  count, per case/engine. Durations are machine-dependent, so they are
  in `Report.JSON` but excluded from `Report.JSONStable`.
- **Long context** — `LongContextCorpus()` (~5,966-token prompts):
  one middle-turn document and one pinned final-turn document.
- **Machine-readable output** — `Report.JSON()` / `Report.JSONStable()`,
  plus the `codvian-bench compression` subcommand with `-json`.
- **Explicit thresholds & fail conditions** — `ThresholdsForCase` /
  `Check`: every violation names engine, metric, observed and required.

### Baseline rule (important)

For the **lossless** engine, metrics compare `compress.Normalise(original)`
against the output — canonicalisation (whitespace strip, dedupe,
last-tool-result) is the contract, not loss. For **lossy**, metrics
compare the raw original, because anything absent was dropped by the
client-opted-in trim.

## 3. Measured results (gpt-4o / cl100k_base, 2026-11-24, Go 1.27.4)

Standard corpus, both engines, 13 cases × 2 engines = 26 rows, all
within thresholds (`pass 13 / fail 0` per engine).

| case | cat | engine | in | out | saved | instr | tools | json | facts | prose | result |
|---|---|---|---|---|---|---|---|---|---|---|---|
| go-code-fenced | code | lossless | 80 | 77 | 3 (3.8%) | 1.00 | – | – | 1.00 | 1.00 | PASS |
| go-code-fenced | code | lossy | 80 | 80 | 0 | 1.00 | – | – | 1.00 | 1.00 | PASS |
| python-code-fenced | code | lossless | 69 | 67 | 2 (2.9%) | 1.00 | – | 1.00 | 1.00 | 1.00 | PASS |
| python-code-fenced | code | lossy | 69 | 69 | 0 | 1.00 | – | 1.00 | 1.00 | 1.00 | PASS |
| json-nested | json | lossless | 123 | 121 | 2 | – | – | 1.00 | 1.00 | 1.00 | PASS |
| json-nested | json | lossy | 123 | 123 | 0 | – | – | 1.00 | 1.00 | 1.00 | PASS |
| json-tabular | json | lossless | 150 | 148 | 2 | – | – | 1.00 | 1.00 | 1.00 | PASS |
| json-tabular | json | lossy | 150 | 150 | 0 | – | – | 1.00 | 1.00 | 1.00 | PASS |
| prose-short | prose | lossless | 12 | 12 | 0 | – | – | – | – | 1.00 | PASS |
| prose-short | prose | lossy | 12 | 12 | 0 | – | – | – | – | 1.00 | PASS |
| prose-long | prose | lossless | 184 | 177 | 7 (3.8%) | – | – | – | – | 1.00 | PASS |
| prose-long | prose | lossy | 184 | 113 | 71 (38.6%) | – | – | – | – | 0.56 | PASS |
| tool-output-json | tool-output | lossless | 78 | 57 | 21 (26.9%) | 1.00 | 1.00 | 1.00 | 1.00 | 1.00 | PASS |
| tool-output-json | tool-output | lossy | 78 | 15 | 63 (80.8%) | 1.00 | 0.00 | 0.00 | 0.33 | 0.70 | PASS |
| tool-output-ansi | tool-output | lossless | 135 | 130 | 5 (3.7%) | 1.00 | – | – | 1.00 | 1.00 | PASS |
| tool-output-ansi | tool-output | lossy | 135 | 18 | 117 (86.7%) | 1.00 | – | – | 0.00 | 0.75 | PASS |
| system-prompt-heavy | system-heavy | lossless | 108 | 52 | 56 (51.9%) | 1.00 | – | – | 1.00 | 1.00 | PASS |
| system-prompt-heavy | system-heavy | lossy | 108 | 38 | 70 (64.8%) | 1.00 | – | – | 0.67 | 0.71 | PASS |
| mixed-code-tool-prose | mixed | lossless | 87 | 78 | 9 (10.3%) | 1.00 | – | 1.00 | 1.00 | 1.00 | PASS |
| mixed-code-tool-prose | mixed | lossy | 87 | 48 | 39 (44.8%) | 1.00 | – | 0.33 | 0.75 | 0.30 | PASS |
| noop-clean-prose | prose | lossless | 7 | 7 | 0 | – | – | – | – | 1.00 | PASS |
| noop-clean-prose | prose | lossy | 7 | 7 | 0 | – | – | – | – | 1.00 | PASS |
| whitespace-heavy-prose | prose | lossless | 48 | 38 | 10 (20.8%) | – | – | – | 1.00 | 1.00 | PASS |
| whitespace-heavy-prose | prose | lossy | 48 | 22 | 26 (54.2%) | – | – | – | 0.00 | 0.27 | PASS |
| multi-turn-history | history | lossless | 119 | 118 | 1 (0.8%) | 1.00 | – | – | 1.00 | 1.00 | PASS |
| multi-turn-history | history | lossy | 119 | 66 | 53 (44.5%) | 1.00 | – | – | 0.00 | 0.72 | PASS |

`–` = metric has an empty denominator for that prompt (nothing to
preserve). Empty denominators are reported as 0 in the JSON but are
**vacuous** for the gate — see §4.

Long-context corpus (5,966 tokens each):

| case | engine | in | out | saved | prose | result |
|---|---|---|---|---|---|---|
| long-context-middle-turn | lossless | 5966 | 5966 | 0 | 1.00 | PASS |
| long-context-middle-turn | lossy | 5966 | 26 | 5940 (99.6%) | 0.04 | PASS |
| long-context-final-turn | lossless | 5966 | 5966 | 0 | 1.00 | PASS |
| long-context-final-turn | lossy | 5966 | 3491 | 2475 (41.5%) | 0.80 | PASS |

**Finding worth stating plainly:** when the bulk of a long prompt is a
droppable *middle* turn, the lossy engine drops the whole turn — 99.6%
saved, 4% prose survival. That is contract-legal (the client opted into
lossy trimming) and the benchmark reports it rather than hiding it;
that category deliberately carries no prose floor. When the same
document is the pinned *final* turn, the trim happens inside the
message and prose survival is 0.80.

Totals: lossless 1,200 → 1,082 tokens (9.8% saved, all rows verified);
lossy 1,200 → 761 (36.6% saved).

## 4. Thresholds (explicit, and why)

Lossless profile — structural, all floors 1.0 plus `VerifyLossless`:
the pipeline guarantees the canonical normal form, so anything less
means the transform is not lossless.

Lossy profile — universal invariants:
- instruction preservation ≥ 1.0 (instruction text is pinned),
- JSON splices = 0 (a cut value whose fragment reappears is corruption,
  not loss),
- no inflation (`output ≤ input`).

Per-category regression floors (max-wins over the baseline):
`json` → JSON preservation ≥ 0.9; content categories → prose ≥ 0.2;
`long-context-final` → prose ≥ 0.5. Floors were set from the measured
run above; tightening after a deliberate pipeline change is intended,
silently deleting one is not.

A metric with an empty denominator never fails a threshold — "this
prompt contains no tool calls" is neither a perfect score nor a
violation.

## 5. Determinism & honesty limits

- Everything except latency is a pure function of (model, corpus);
  `TestBenchmarkStableJSONDeterministic` pins byte-identical stable JSON
  across runs. Latency lives only in `Report.JSON`, never affects Pass.
- Token counts are exact local BPE counts, independently recounted by
  the harness and cross-checked against the compression stamp — a
  disagreement fails the run instead of printing a table.
- Inflation is an error, not a clamped zero.
- Fidelity ratios are structural proxies on normalised text. They do
  not prove meaning preservation for lossy trims; the report is a
  measurement, not an endorsement.
- Machine-readable sample: `docs/compression-benchmark-2026-11-24.json`.

## 6. Tests

`internal/compress/evalharness/benchmark_test.go`:
`TestPrintCompressionBenchmark`, `TestPrintLongContextBenchmark`,
`TestBenchmarkStableJSONDeterministic`, `TestThresholdGateCatchesViolations`
(tamper-per-metric, splice detection, vacuous-denominator, profile
separation), `TestFidelityMetricDefinitions` (identity, dropped
instruction, cut-JSON splice vs dropped-whole).

`cmd/codvian-bench/compression_test.go`: JSON shape and corpus size,
text-table contents, long corpus, exit codes 0/2.
