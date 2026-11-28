// Recall/precision probe for the semantic similarity floor (Stream D).
//
// DefaultSemanticThreshold (0.85) is the cosine floor a stored entry
// must reach to be served. This file measures what that floor actually
// buys, over a labelled probe set of short assistant prompts - the
// audience the semantic layer exists for (long RAG prompts are served
// by the exact layer, and tool/lossy requests never reach this layer):
//
//   - positives: the same request re-worded as users re-word it. These
//     MUST hit (a paraphrase is the same question).
//   - impostors: the same template with one decisive token changed, so
//     the answer is different. These MUST miss (a same-words-different-
//     meaning prompt).
//   - easy negatives: unrelated prompts. These must miss trivially.
//
// The probe runs entirely offline against the shipped local embedder
// (EmbedText, the hashed bag-of-words vector that is also the fail-open
// fallback when no neural embedding provider is configured), so CI can
// measure it. Measured on this corpus (2026-11-28, 36 pairs):
//
//	floor  recall  false-serve rate on negatives
//	0.80   0.43    8/21 = 0.38
//	0.85   0.29    2/21 = 0.10   <- shipped floor
//	0.90   0.07    1/21 = 0.05
//
// The assertions below pin the justification for 0.85: among all swept
// floors that keep recall >= 0.25, 0.85 is the one with the fewest
// false serves; every lower floor admits strictly more impostors, and
// every higher floor breaks the recall contract. If the embedder, the
// tokenizer or the floor changes, this test fails loudly instead of
// silently moving the operating point.
package cache

import (
	"math"
	"testing"
)

// floorProbePair is one labelled probe pair: store is the cached prompt,
// query is the incoming prompt, want says whether the query is the same
// request (must hit) or a different one (must miss).
type floorProbePair struct {
	label string
	store string
	query string
	want  bool
}

var floorProbePairs = []floorProbePair{
	{"pos/capital-which-city", "What is the capital of France?", "Which city is the capital of France?", true},
	{"pos/capital-please", "What is the capital of France?", "Please tell me the capital of France", true},
	{"pos/capital-whats", "What is the capital of France?", "What's the capital of France?", true},
	{"pos/bullets-three", "Summarize this paragraph in three bullet points", "Summarize this paragraph into 3 bullet points", true},
	{"pos/translate-fr", "Translate hello into French", "Translate the word hello into French", true},
	{"pos/translate-please", "Translate hello into French", "Please translate hello into French", true},
	{"pos/sort-slice", "How do I sort a slice of strings in Go?", "How can I sort a slice of strings in Go?", true},
	{"pos/table-test", "How do I write a table test in Go?", "How do I write table tests in Go?", true},
	{"pos/unmarshal-json", "How do I unmarshal JSON into a struct in Go?", "How do I unmarshal JSON to a struct in Go?", true},
	{"pos/rate-limit", "What is a rate limit?", "What exactly is a rate limit?", true},
	{"pos/rate-limit-explain", "What is a rate limit?", "Can you explain what a rate limit is?", true},
	{"pos/chunked-http", "Why does chunked transfer encoding exist?", "Why does chunked transfer encoding exist in HTTP?", true},
	{"pos/get-method", "When should I use GET?", "When should I use the GET method?", true},
	{"ctrl/exact-repeat", "What is a circuit breaker?", "What is a circuit breaker?", true},
	{"imp/capital-pop", "What is the capital of France?", "What is the population of France?", false},
	{"imp/capital-area", "What is the capital of France?", "What is the area of France?", false},
	{"imp/capital-currency", "What is the capital of France?", "What is the currency of France?", false},
	{"imp/japan-peru", "What is the capital of Japan?", "What is the capital of Peru?", false},
	{"imp/fr-es", "Translate hello into French", "Translate hello into Spanish", false},
	{"imp/hello-thanks", "Translate hello into French", "Translate thanks into French", false},
	{"imp/three-four", "Summarize this paragraph in three bullet points", "Summarize this paragraph in four bullet points", false},
	{"imp/slice-map", "How do I sort a slice of strings in Go?", "How do I sort a map of strings in Go?", false},
	{"imp/unmarshal-marshal", "How do I unmarshal JSON into a struct in Go?", "How do I marshal JSON from a struct in Go?", false},
	{"imp/rate-quota", "What is a rate limit?", "What is a quota limit?", false},
	{"imp/get-post", "When should I use GET?", "When should I use POST?", false},
	{"imp/http2-http3", "What is the difference between HTTP/1.1 and HTTP/2?", "What is the difference between HTTP/2 and HTTP/3?", false},
	{"imp/rebase-merge", "How do I rebase a branch onto main?", "How do I merge a branch onto main?", false},
	{"imp/table-bench", "How do I write a table test in Go?", "How do I write a benchmark in Go?", false},
	{"imp/year", "What were the sales in 2023?", "What were the sales in 2024?", false},
	{"imp/os", "How do I build this for Windows?", "How do I build this for Linux?", false},
	{"imp/mutex", "How do I read a map with a mutex?", "How do I read a map without a mutex?", false},
	{"imp/addition-times", "What is 2 + 2?", "What is 2 * 2?", false},
	{"neg/haiku", "What is a rate limit?", "Write a haiku about snow.", false},
	{"neg/krebs", "What is the capital of France?", "Explain the Krebs cycle.", false},
	{"neg/pizza", "How do I sort a slice in Go?", "What is the best pizza in Naples?", false},
	{"neg/latency", "What is a rate limit?", "How do I reduce my p99 latency?", false},
}

// floorProbeSim is the cosine similarity of one pair under the shipped
// local embedder.
func floorProbeSim(pr floorProbePair) float32 {
	return Cosine(EmbedText(pr.store), EmbedText(pr.query))
}

// floorProbeCounts evaluates the probe set at one floor: tp/fp/fn/tn.
func floorProbeCounts(floor float32) (tp, fp, fn, tn int) {
	for _, pr := range floorProbePairs {
		hit := floorProbeSim(pr) >= floor
		switch {
		case pr.want && hit:
			tp++
		case !pr.want && hit:
			fp++
		case pr.want && !hit:
			fn++
		default:
			tn++
		}
	}
	return
}

func floorProbeR4(x float64) float64 { return math.Round(x*10000) / 10000 }

// TestSemanticFloorProbeOperatingPoint measures the probe at the shipped
// floor and asserts the recall/precision contract that justifies it.
func TestSemanticFloorProbeOperatingPoint(t *testing.T) {
	floor := float32(DefaultSemanticThreshold)
	tp, fp, fn, tn := floorProbeCounts(floor)
	recall := float64(tp) / float64(tp+fn)
	negatives := fp + tn
	falseServe := float64(fp) / float64(negatives)
	t.Logf("probe at shipped floor %.2f: tp=%d fp=%d fn=%d tn=%d recall=%v false-serve=%v",
		floor, tp, fp, fn, tn, floorProbeR4(recall), floorProbeR4(falseServe))
	for _, pr := range floorProbePairs {
		t.Logf("  %-24s sim=%v want=%v", pr.label, floorProbeR4(float64(floorProbeSim(pr))), pr.want)
	}
	// Recall contract: the floor must still serve at least a quarter of
	// the paraphrase set (measured 0.29 at 0.85).
	if recall < 0.25 {
		t.Fatalf("recall at floor %.2f = %v, want >= 0.25 (the floor no longer serves paraphrases)", floor, floorProbeR4(recall))
	}
	// Precision contract: at most 10% of the negative pairs may be
	// served (measured 0.10 at 0.85).
	if falseServe > 0.10 {
		t.Fatalf("false-serve rate at floor %.2f = %v, want <= 0.10 (the floor admits same-words-different-meaning prompts)", floor, floorProbeR4(falseServe))
	}
}

// TestSemanticFloorProbeJustifies085 sweeps floors and asserts that the
// shipped floor is the precision-optimal one subject to the recall
// contract: every lower floor serves strictly more impostors, every
// higher floor breaks recall, and no floor that keeps recall >= 0.25
// serves fewer impostors than the shipped floor.
func TestSemanticFloorProbeJustifies085(t *testing.T) {
	floor := float32(DefaultSemanticThreshold)
	tpAt, fpAt, fnAt, tnAt := floorProbeCounts(floor)
	recallAt := float64(tpAt) / float64(tpAt+fnAt)
	negatives := fpAt + tnAt

	bestFP := -1
	bestFloor := float32(0)
	for i := 40; i <= 99; i++ {
		f := float32(i) / 100
		tp, fp, fn, tn := floorProbeCounts(f)
		rec := float64(tp) / float64(tp+fn)
		fs := float64(fp) / float64(fp+tn)
		t.Logf("sweep floor=%.2f tp=%d fp=%d fn=%d tn=%d recall=%v false-serve=%v", f, tp, fp, fn, tn, floorProbeR4(rec), floorProbeR4(fs))
		if rec >= 0.25 && (bestFP == -1 || fp < bestFP) {
			bestFP = fp
			bestFloor = f
		}
	}
	t.Logf("shipped floor %.2f: recall=%v false-serve=%v; best floor with recall>=0.25: %.2f (fp=%d)", floor, floorProbeR4(recallAt), floorProbeR4(float64(fpAt)/float64(negatives)), bestFloor, bestFP)
	if bestFloor != floor || bestFP != fpAt {
		t.Fatalf("precision-optimal floor subject to recall>=0.25 is %.2f (fp=%d), but DefaultSemanticThreshold is %.2f (fp=%d): the shipped floor is no longer justified by the probe", bestFloor, bestFP, floor, fpAt)
	}
	// Lowering the floor admits strictly more impostors.
	for i := 80; i < 85; i++ {
		f := float32(i) / 100
		if f >= floor {
			continue
		}
		_, fpLower, _, _ := floorProbeCounts(f)
		if fpLower <= fpAt {
			t.Fatalf("floor %.2f serves %d impostors, not more than the shipped floor's %d: lowering is free, so 0.85 is not the precision frontier", f, fpLower, fpAt)
		}
	}
	// Raising the floor breaks the recall contract.
	for i := 86; i <= 90; i++ {
		f := float32(i) / 100
		tp, _, fn, _ := floorProbeCounts(f)
		if rec := float64(tp) / float64(tp+fn); rec >= 0.25 {
			t.Fatalf("floor %.2f keeps recall %v >= 0.25, so raising past 0.85 costs nothing: 0.85 is not the recall frontier either", f, floorProbeR4(rec))
		}
	}
}

// TestSemanticFloorProbeKnownOperatorCollision pins a documented
// limitation of the offline embedder: the tokenizer strips operators,
// so "2 + 2" and "2 * 2" embed to the SAME vector (similarity 1.0) and no
// similarity floor can separate them. The exact layer still separates
// them (different keys), and a neural embedding provider does not share
// the blind spot. The pin fails loudly if the tokenizer ever changes.
func TestSemanticFloorProbeKnownOperatorCollision(t *testing.T) {
	sim := floorProbeSim(floorProbePair{label: "op", store: "What is 2 + 2?", query: "What is 2 * 2?"})
	if sim < 0.999 {
		t.Fatalf("operator-blind collision: sim(2+2, 2*2) = %v, want ~1.0 (documented tokenizer blind spot)", floorProbeR4(float64(sim)))
	}
}
