# Compression savings — measured (M7.1)

Measured: **2026-11-19 IST** (file dated 2026-11-18 per the M7.1 slice
name). Harness: `internal/compress/evalharness`, model `gpt-4o`
(cl100k_base), Go **1.27.4** (`go version go1.27.4 linux/amd64`).
Corpus: 13 fixed `[]provider.Message` literals in
`internal/compress/evalharness/corpus.go` — no network, no
randomness, deterministic.

## How to re-run (the table below is this command's output, copied verbatim)

```sh
export PATH=$HOME/go/bin:$PATH
go test ./internal/compress/evalharness/ -run TestPrintMeasuredSavingsTable -v -count=1
```

## Method (what the numbers are — and are not)

- Every token count is an **exact local BPE count**
  (`internal/tokencount.CountPrompt`), counted independently by the
  harness before and after `compress.Apply` — never copied from the
  compression result stamp. The harness refuses to print a table if
  its recount disagrees with the stamp, if `saved + output !=
  input`, or if a run inflates a prompt.
- `lossless` = whitespace strip + system dedupe + tool-result
  canonicalisation only. `verified` means
  `compress.VerifyLossless(original, output)` held: the output is
  exactly the canonical normal form of the input (structural
  guarantee, not a semantic proof).
- `lossy` = lossless first, then extractive trimming toward the
  case's fixed `LossyBudget` (whole middle turns dropped
  oldest-first, then head+tail truncation with a marker). Lossy
  output is **not** verified lossless by definition — meaning may
  have changed; the fidelity gate (M7.2) owns that check.
- Counts are **local estimates for gpt-4o**, never
  provider-reported usage. A provider may bill a different number;
  runtime calibration is M7.9.
- Lossy budgets are literals per case in `corpus.go`, chosen when
  the corpus was written: below the case's input size for the long
  cases (so lossy actually trims), above it for `prose-short`
  (budget 200, input 12) and `noop-clean-prose` (budget 200, input
  7), which therefore honestly report 0 saved under lossy.

## Measured savings table

```
case                           engine    in   out  saved  saved%  verified/lossy
go-code-fenced                  lossless   80    77      3    3.8%  verified
go-code-fenced                  lossy      80    71      9   11.2%  lossy-applied
python-code-fenced              lossless   69    67      2    2.9%  verified
python-code-fenced              lossy      69    58     11   15.9%  lossy-applied
json-nested                     lossless  123   121      2    1.6%  verified
json-nested                     lossy     123    93     30   24.4%  lossy-applied
json-tabular                    lossless  150   148      2    1.3%  verified
json-tabular                    lossy     150   116     34   22.7%  lossy-applied
prose-short                     lossless   12    12      0    0.0%  verified
prose-short                     lossy      12    12      0    0.0%  -
prose-long                      lossless  184   177      7    3.8%  verified
prose-long                      lossy     184   128     56   30.4%  lossy-applied
tool-output-json                lossless   78    57     21   26.9%  verified
tool-output-json                lossy      78    15     63   80.8%  lossy-applied
tool-output-ansi                lossless  135   130      5    3.7%  verified
tool-output-ansi                lossy     135    18    117   86.7%  lossy-applied
system-prompt-heavy             lossless  108    52     56   51.9%  verified
system-prompt-heavy             lossy     108    47     61   56.5%  lossy-applied
mixed-code-tool-prose           lossless   87    78      9   10.3%  verified
mixed-code-tool-prose           lossy      87    48     39   44.8%  lossy-applied
noop-clean-prose                lossless    7     7      0    0.0%  verified
noop-clean-prose                lossy       7     7      0    0.0%  -
whitespace-heavy-prose          lossless   48    38     10   20.8%  verified
whitespace-heavy-prose          lossy      48    41      7   14.6%  lossy-applied
multi-turn-history              lossless  119   118      1    0.8%  verified
multi-turn-history              lossy     119    66     53   44.5%  lossy-applied
TOTAL (lossless)                         1200  1082    118    9.8%
TOTAL (lossy)                            1200   720    480   40.0%
```

Per-case lossy budgets (tokens): go-code-fenced 50,
python-code-fenced 45, json-nested 80, json-tabular 100,
prose-short 200, prose-long 120, tool-output-json 40,
tool-output-ansi 100, system-prompt-heavy 35,
mixed-code-tool-prose 50, noop-clean-prose 200,
whitespace-heavy-prose 30, multi-turn-history 80.

## Reading the table (honest observations, not claims)

- **Lossless totals 118 / 1,200 tokens (9.8%).** It is dominated
  by two cases: `system-prompt-heavy` (56 saved — duplicate system
  blocks) and `tool-output-json` (21 saved — stale tool result
  dropped). On code, JSON, and clean prose it saves 1–4%: the
  lossless steps only remove what is provably redundant, so small
  savings on clean input are the correct result, not a failure.
- **Lossy totals 480 / 1,200 tokens (40.0%)**, trimming on 11 of
  13 cases. The largest lossy wins come from dropping whole
  messages (`tool-output-ansi` 135 → 18: the ~100-token tool
  message is dropped; `tool-output-json` 78 → 15), not from
  clever rewriting — there is no rewriting in this pipeline.
- **Lossy does not always reach its budget.** 7 of 13 lossy runs
  finish over budget: `go-code-fenced` 71 against 50,
  `python-code-fenced` 58 against 45, `json-nested` 93 against 80,
  `json-tabular` 116 against 100, `prose-long` 128 against 120,
  `system-prompt-heavy` 47 against 35, `whitespace-heavy-prose`
  41 against 30. This is the documented
  behaviour of `compress.Apply`: pinned messages (system/developer,
  last user, last message) are never dropped, and truncation stops
  rather than shrinking a message below ~10 tokens. The reported
  counts are the real ones, never forced to the budget. (Whether
  "budget not met" should warn the caller is a question for M7.7's
  hard-budget post-pass, not this harness.)
- **Lossy can underperform lossless.** On `whitespace-heavy-prose`,
  lossless saves 10 (48 → 38) but lossy saves only 7 (48 → 41):
  the truncation marker costs more than the cut saves
  (`compress.go` acknowledges this shape). The harness reports both
  numbers honestly; the marker-cost question belongs to M7.7's
  hard-budget post-pass (and to M7.3's inflation guard, which
  catches net-inflating cuts).
- **No case inflated.** No lossless or lossy run produced more
  tokens than its input; the harness would have failed the run
  instead of printing a table.
- **Prior measurements are not re-runs.** CHECKLIST item 183 /
  `docs/token-count-audit.md` recorded lossless 25 → 14 (saved 11)
  and lossy 654 → 69 (saved 585) on different fixtures. Those
  numbers are not comparable with this corpus and were not
  re-measured here; this table supersedes nothing, it measures the
  new fixed corpus only.

## Limitations

- 13 hand-written cases, not production traffic: the corpus
  exercises the shapes named in the M7 plan (code, JSON, prose,
  tool output, system-heavy, mixed, history) but says nothing
  about real user prompts, other languages, or very long contexts
  (the largest case is 184 tokens).
- One model/encoding only (gpt-4o / cl100k_base). Counts for
  other models' encodings will differ; the harness takes a model
  argument but the committed table is gpt-4o only.
- The harness measures the pipeline as it exists today. It builds
  no compressor and cannot show what M7.4 (RTK filter) or M7.5
  (Caveman condensation) would save — those slices re-run this
  harness against their own code.
- `saved%` is per-case and per-engine; the TOTAL rows are
  token-weighted (total saved / total input), not averages of the
  per-case percentages.
