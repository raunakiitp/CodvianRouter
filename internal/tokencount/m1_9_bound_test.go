//go:build !race

package tokencount

import "time"

// oneMBBound is the M1.9 timing bound for counting a 1 MiB
// input (TestCountOneMBCompletesQuickly), without the race
// detector: 6 s. Slowest non-race measurement on this VM in
// isolation was 1.65 s (distinct letters, 2026-11-18) and
// 1.40 s (distinct realistic prose, slow-CPU run); typical runs
// measure 0.33–0.85 s realistic, 0.64–1.65 s distinct letters,
// ~0.01–0.02 s repeated 'a'. The bound was first set to 3 s,
// which proved to have no headroom under full-gate load: with
// all 40 packages testing in parallel on this VM's 1.5 shared
// CPUs, the same unchanged code took 3.009 s (distinct letters)
// and failed the 3 s bound by 9 ms (2026-11-18, Phase C). The
// bound is therefore re-scaled to the full-gate worst with ~2x
// headroom, the same methodology as the race bound. It still
// catches the pre-fix pathology by 30x (1 MiB of distinct
// letters unchunked never finished in 180 s). The tokencount
// code is unmodified; only the test's load allowance changed.
const oneMBBound = 6 * time.Second
