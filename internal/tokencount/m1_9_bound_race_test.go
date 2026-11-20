//go:build race

package tokencount

import "time"

// oneMBBound is the M1.9 timing bound for counting a 1 MiB
// input (TestCountOneMBCompletesQuickly), under the race
// detector: 50 s. Race instrumentation slows the BPE encode
// and the memo ~5-7x on this VM — worst measured under -race
// in isolation was 5.02 s (distinct letters), 3.87 s (distinct
// realistic prose) — so the race bound was first set to 10 s.
// That bound proved unachievable in the FULL gate: with all 40
// packages testing in parallel on this VM's 1.5 shared CPUs,
// the same unchanged code took 22.7 s / 24.5 s (distinct
// letters) and 11.8 s / 11.5 s (distinct realistic) across two
// consecutive full-gate runs (2026-11-18, Phase C), failing the
// gate twice while passing in isolation (9.47 s worst). The
// bound is therefore re-scaled to the full-gate worst with the
// same ~2x headroom the 10 s bound had over the isolated worst.
// The bound still catches the pre-fix pathology by >3x (1 MiB
// of distinct letters unchunked never finished in 180 s), and
// the real speed assertion is the non-race 3 s bound (realistic
// 1 MiB = 542 ms measured) — this file asserts nothing about
// interactive speed, only that the race-detector run does not
// hang or regress grossly. The tokencount code is unmodified;
// only the test's load allowance changed.
const oneMBBound = 50 * time.Second
