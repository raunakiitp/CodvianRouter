package evalharness

// Long-context corpus for the P1.4 benchmark.
//
// The standard corpus (corpus.go) tops out around 200 tokens, which
// says nothing about behaviour on a real long prompt: the lossy
// engine's middle-dropping and truncation paths, the fidelity gate's
// unit scanning, and latency all change scale. This corpus holds one
// deterministic long case (roughly 8–12k tokens depending on the BPE
// tables) built from fixed literals — no randomness, no clock, no
// network, so its measured numbers are reproducible like every other
// corpus number.
//
// It is a SEPARATE corpus on purpose: mixing it into Corpus() would
// slow the standard table and its golden input-token pins for no
// benefit. Run it via RunBenchmark(Options{Cases:
// LongContextCorpus()}) or the TestPrintLongContextBenchmark entry
// point.

import (
	"fmt"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// LongContextTargetTokens is the approximate size the generated
// document aims at. It is a build target, not a measurement claim —
// the harness reports the exact count it measures.
const LongContextTargetTokens = 10000

// LongContextBudget is the lossy budget for the long case: about a
// third of the expected size, so the lossy run genuinely trims.
const LongContextBudget = 3500

// LongContextCorpus returns the fixed long-context cases.
//
// Two cases, deliberately different:
//
//   - long-context-middle-turn: the long document is a MIDDLE turn. This
//     exposes what the lossy engine does when the bulk of a prompt is
//     droppable by contract — it drops the whole turn, and the
//     benchmark reports the resulting fidelity hit instead of hiding
//     it.
//   - long-context-final-turn: the same size document is the FINAL
//     user message, which the lossy contract pins. The trim then has
//     to happen inside the message (head+tail), which is the case
//     operators actually care about.
func LongContextCorpus() []Case {
	doc := longContextDocument()
	return []Case{
		{
			Name:        "long-context-middle-turn",
			Category:    "long-context-middle",
			Messages:    []provider.Message{{Role: "system", Content: "You are a precise operations analyst. Preserve every numbered fact."}, {Role: "user", Content: doc}, {Role: "user", Content: "Summarise the ledger behaviour across all sections, keeping section numbers."}},
			LossyBudget: LongContextBudget,
		},
		{
			Name:        "long-context-final-turn",
			Category:    "long-context-final",
			Messages:    []provider.Message{{Role: "system", Content: "You are a precise operations analyst. Preserve every numbered fact."}, {Role: "user", Content: "Summarise the ledger behaviour across all sections, keeping section numbers."}, {Role: "user", Content: doc}},
			LossyBudget: LongContextBudget,
		},
	}
}

// longContextDocument builds the deterministic long document:
// numbered paragraphs assembled from a fixed literal, varying only by
// paragraph index — no RNG, no clock.
func longContextDocument() string {
	const paraCount = 90
	paras := make([]string, 0, paraCount)
	for i := 0; i < paraCount; i++ {
		paras = append(paras, fmt.Sprintf(
			"Section %d. The routing ledger records attempt %d for provider pool %d; "+
				"the quota schedule window %d resets at midnight UTC, and the fallback chain "+
				"for tier %d tries the preferred key before any reserve key. "+
				"Observed p50 latency was %d ms with %d%% cache hits across %d requests.",
			i, i, i%4, i%8, i%3, 120+i%40, 40+i%50, 1000+i*7))
	}
	return strings.Join(paras, "\n")
}
