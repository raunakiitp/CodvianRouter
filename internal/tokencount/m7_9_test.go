package tokencount

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// M7.9 (a): a base64 data URI pasted inside a TEXT string must not
// be BPE-counted in full. Image PARTS were already skipped; only
// the in-text-string case was missing (parity tracker Area 10 "Base64
// image data-URI exclusion", PARTIAL). The URI is deleted before
// counting, so the count equals the count of the surrounding text
// with the URI removed — never the ~thousands of tokens a pasted
// base64 blob would otherwise cost a budget gate.

const m79DataURI = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func TestCountPromptStripsDataURIInText(t *testing.T) {
	// (1) URI in the middle of real text: count == surrounding
	// text with the URI deleted (joined with the '\n' separator
	// CountPrompt adds per segment).
	text := "Describe this image: " + m79DataURI + " Be concise."
	got, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: text}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want, err := Count("gpt-4", "Describe this image:  Be concise.\n")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.Tokens != want.Tokens {
		t.Errorf("CountPrompt with data URI = %d, want %d (surrounding text only)", got.Tokens, want.Tokens)
	}
	// Without the strip the same text costs the whole blob:
	// pin the gap so the test is sensitivity-proven.
	raw, _ := Count("gpt-4", text+"\n")
	if raw.Tokens <= want.Tokens+10 {
		t.Errorf("sanity: raw count %d not much larger than stripped %d — blob too short to prove the strip", raw.Tokens, want.Tokens)
	}
	t.Logf("M7.9 data-URI in text: stripped=%d raw=%d (blob would have cost +%d)", got.Tokens, raw.Tokens, raw.Tokens-got.Tokens)

	// (2) A message that IS only a data URI counts ~0.
	only, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: m79DataURI}})
	if err != nil {
		t.Fatalf("CountPrompt(data-URI-only): %v", err)
	}
	if only.Tokens != 0 {
		t.Errorf("CountPrompt(data-URI-only) = %d, want 0", only.Tokens)
	}

	// (3) Multiple URIs in one string: all deleted.
	two := "First " + m79DataURI + " middle " + m79DataURI + " last."
	got2, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: two}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want2, _ := Count("gpt-4", "First  middle  last.\n")
	if got2.Tokens != want2.Tokens {
		t.Errorf("CountPrompt(two URIs) = %d, want %d", got2.Tokens, want2.Tokens)
	}

	// (4) Generic data:…;base64 (not just image/*) is stripped.
	pdf := "data:application/pdf;base64,JVBERi0xLjQKJcOkw7zDtsOfCjIgMCBvYmoKPDwvTGVuZ3RoIDMvRmlsdGVyL0ZsYXRlRGVjb2RlPj4Kc3RyZWFtCnicS8zJTFUoZCvNLCvJzM5VKMpMTlWyMQrJy8xRsE4sLknMS8vOz1UoysxLjE1M7ewtAAAMTYC0KZW5kc3RyZWFtCmVuZG9iago="
	got3, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: "Attached: " + pdf + " thanks."}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want3, _ := Count("gpt-4", "Attached:  thanks.\n")
	if got3.Tokens != want3.Tokens {
		t.Errorf("CountPrompt(pdf data URI) = %d, want %d", got3.Tokens, want3.Tokens)
	}

	// (5) A non-base64 data: URI is ordinary text and is untouched.
	plain := "See data:text/plain,hello world for details."
	got4, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: plain}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want4, _ := Count("gpt-4", plain+"\n")
	if got4.Tokens != want4.Tokens {
		t.Errorf("CountPrompt(non-base64 data URI) = %d, want %d (must count in full)", got4.Tokens, want4.Tokens)
	}

	// (6) The strip also applies to text PARTS (array form), and
	// an image_url part alongside stays skipped as before.
	partMsg := provider.Message{Role: "user", Parts: []provider.ContentPart{
		{Type: "text", Text: "Look: " + m79DataURI + " done."},
		{Type: "image_url", ImageURL: &wire.ImageURLPart{URL: m79DataURI}},
	}}
	got5, err := CountPrompt("gpt-4", []provider.Message{partMsg})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want5, _ := Count("gpt-4", "Look:  done.\n")
	if got5.Tokens != want5.Tokens {
		t.Errorf("CountPrompt(text part + image part) = %d, want %d", got5.Tokens, want5.Tokens)
	}

	// (7) A data URI glued to words (no surrounding spaces) still
	// strips; the words either side are counted.
	glued := "prefix" + m79DataURI + "suffix"
	got6, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: glued}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want6, _ := Count("gpt-4", "prefixsuffix\n")
	if got6.Tokens != want6.Tokens {
		t.Errorf("CountPrompt(glued URI) = %d, want %d", got6.Tokens, want6.Tokens)
	}
}

// Stripping is a CountPrompt-level rule, not a Count-level one:
// raw Count still counts a pasted URI in full, so a caller that
// counts an arbitrary string (tool schemas, responses) is not
// silently changed by this slice.
func TestCountPromptStripsMixedCaseDataURI(t *testing.T) {
	// M7.9 review nit 1: mixed-case "dAtA:" bypassed the strip
	// (the old fast path checked only data:/DATA:/Data:).
	// The scheme/marker match is case-insensitive end to end.
	payload := strings.TrimPrefix(m79DataURI, "data:image/png;base64,")
	mixed := "dAtA:image/png;bAsE64," + payload
	text := "Describe this image: " + mixed + " Be concise."
	got, err := CountPrompt("gpt-4", []provider.Message{{Role: "user", Content: text}})
	if err != nil {
		t.Fatalf("CountPrompt: %v", err)
	}
	want, err := Count("gpt-4", "Describe this image:  Be concise.\n")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got.Tokens != want.Tokens {
		t.Errorf("mixed-case data URI count = %d, want %d (surrounding text only)", got.Tokens, want.Tokens)
	}
}

func TestCountStillCountsDataURIInFull(t *testing.T) {
	raw, err := Count("gpt-4", m79DataURI)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if raw.Tokens < 10 {
		t.Errorf("Count(data URI) = %d, want >= 10 (raw Count must not strip)", raw.Tokens)
	}
}

// M7.9 (b): memo audit, measured not asserted from a comment.
// Workload: 200 distinct prompts, each counted 3 times — the
// per-request shape the profile called for (per-key traffic
// estimate, context-fit gate, reconcileUsage). The numbers are
// logged so the report can quote them; the assertions pin the
// shape (2 of 3 calls hit) and the order-of-magnitude gap.
func TestMemoAuditMeasured(t *testing.T) {
	const distinct = 200
	prompts := make([]string, distinct)
	for i := range prompts {
		prompts[i] = fmt.Sprintf("M7.9 memo audit prompt number %d about migrating monarch butterflies and their routes.", i)
	}
	// Warm the encoder so build cost lands outside the timing.
	if _, err := Count("gpt-4", prompts[0]); err != nil {
		t.Fatalf("warm-up Count: %v", err)
	}
	hitsBefore, missesBefore := memoHits.Load(), memoMisses.Load()

	start := time.Now()
	for r := 0; r < 3; r++ {
		for _, p := range prompts {
			if _, err := Count("gpt-4", p); err != nil {
				t.Fatalf("Count: %v", err)
			}
		}
	}
	workloadNanos := time.Since(start).Nanoseconds()
	hits, misses := memoHits.Load()-hitsBefore, memoMisses.Load()-missesBefore
	total := hits + misses
	if total != 3*distinct {
		t.Errorf("memo audit workload: %d Count calls observed, want %d", total, 3*distinct)
	}
	hitRate := float64(hits) / float64(total)
	t.Logf("M7.9 memo audit workload: %d calls (%d distinct x3), hits=%d misses=%d hit-rate=%.3f total-time=%d ns (%.1f ns/call)",
		total, distinct, hits, misses, hitRate, workloadNanos, float64(workloadNanos)/float64(total))
	if hitRate < 0.60 {
		t.Errorf("memo hit rate %.3f < 0.60 (want ~0.667: first count cold, 2nd/3rd hit)", hitRate)
	}

	// Hit latency vs cold-encode latency, same text shape.
	hitText := "Explain migrating monarch butterflies in one sentence, with the usual detail."
	if _, err := Count("gpt-4", hitText); err != nil {
		t.Fatalf("warm Count: %v", err)
	}
	const hitN = 2000
	start = time.Now()
	for i := 0; i < hitN; i++ {
		if _, err := Count("gpt-4", hitText); err != nil {
			t.Fatalf("Count hit: %v", err)
		}
	}
	hitNanos := time.Since(start).Nanoseconds() / hitN
	const coldN = 200
	start = time.Now()
	for i := 0; i < coldN; i++ {
		p := fmt.Sprintf("M7.9 memo audit cold prompt number %d about migrating monarch butterflies and their routes.", i)
		if _, err := Count("gpt-4", p); err != nil {
			t.Fatalf("Count cold: %v", err)
		}
	}
	coldNanos := time.Since(start).Nanoseconds() / coldN
	t.Logf("M7.9 memo audit latency: hit=%d ns/op (n=%d) cold-encode=%d ns/op (n=%d) ratio=%.1fx",
		hitNanos, hitN, coldNanos, coldN, float64(coldNanos)/float64(hitNanos))
	if hitNanos*5 >= coldNanos {
		t.Errorf("memo hit %d ns/op not >=5x faster than cold encode %d ns/op", hitNanos, coldNanos)
	}

	// CountPrompt repeats re-join the text per call (PARITY note)
	// but must still hit the Count memo: a repeat must cost a
	// hash + join, not a BPE encode.
	msgs := []provider.Message{{Role: "user", Content: strings.Repeat("The quick brown fox jumps over the lazy dog. ", 10)}}
	if _, err := CountPrompt("gpt-4", msgs); err != nil {
		t.Fatalf("warm CountPrompt: %v", err)
	}
	const repN = 500
	start = time.Now()
	for i := 0; i < repN; i++ {
		if _, err := CountPrompt("gpt-4", msgs); err != nil {
			t.Fatalf("CountPrompt repeat: %v", err)
		}
	}
	repNanos := time.Since(start).Nanoseconds() / repN
	t.Logf("M7.9 memo audit CountPrompt repeat: %d ns/op (n=%d, joined text re-hashed per call, BPE memoised)", repNanos, repN)
	if repNanos >= coldNanos {
		t.Errorf("CountPrompt repeat %d ns/op not faster than cold encode %d ns/op", repNanos, coldNanos)
	}
}

// M7.9 (c): runtime calibration. Parameters match the sector-typical
// published calibration (estimatorCalibration.ts #14931): EMA
// α=0.25 per (provider, model, tools) bucket, factor applies at
// ≥3 samples, clamped [0.15, 2.0]. The calibrator is built and
// unit-tested here; the live feed (router reconcileUsage) and
// the estimate-site switch to CountPromptWireCalibrated are
// outside this slice (internal/tokencount/ only), so the
// process-wide calibrator stays cold (factor 1.0) until that
// wiring lands — see calibration.go.
func TestCalibrationColdStartIsFactorOne(t *testing.T) {
	c := NewCalibrator()
	if f := c.Factor("openai", "gpt-4", false); f != 1 {
		t.Errorf("cold Factor = %v, want 1.0", f)
	}
	if got := c.Apply("openai", "gpt-4", false, 1000); got != 1000 {
		t.Errorf("cold Apply(1000) = %d, want 1000", got)
	}
	if _, _, ok := c.Bucket("openai", "gpt-4", false); ok {
		t.Errorf("cold Bucket ok=true, want false")
	}
}

func TestCalibrationAppliesOnlyAtThreeSamples(t *testing.T) {
	c := NewCalibrator()
	// Ratio 1.2 every time: factor must stay 1.0 at 1-2 samples.
	c.Record("openai", "gpt-4", false, 1000, 1200)
	if f := c.Factor("openai", "gpt-4", false); f != 1 {
		t.Errorf("after 1 sample Factor = %v, want 1.0", f)
	}
	c.Record("openai", "gpt-4", false, 1000, 1200)
	if f := c.Factor("openai", "gpt-4", false); f != 1 {
		t.Errorf("after 2 samples Factor = %v, want 1.0", f)
	}
	c.Record("openai", "gpt-4", false, 1000, 1200)
	if f := c.Factor("openai", "gpt-4", false); f != 1.2 {
		t.Errorf("after 3 samples Factor = %v, want 1.2", f)
	}
	if got := c.Apply("openai", "gpt-4", false, 1000); got != 1200 {
		t.Errorf("Apply(1000) = %d, want 1200", got)
	}
}

func TestCalibrationEMAMath(t *testing.T) {
	c := NewCalibrator()
	// Seed ratio 1.0, then two ratios of 2.0:
	// f1 = 1.0; f2 = 1.0 + 0.25*(2.0-1.0) = 1.25;
	// f3 = 1.25 + 0.25*(2.0-1.25) = 1.4375.
	c.Record("openai", "gpt-4", false, 1000, 1000)
	c.Record("openai", "gpt-4", false, 1000, 2000)
	c.Record("openai", "gpt-4", false, 1000, 2000)
	f, samples, ok := c.Bucket("openai", "gpt-4", false)
	if !ok || samples != 3 {
		t.Fatalf("Bucket = factor %v samples %d ok %v, want 3 samples", f, samples, ok)
	}
	if f != 1.4375 {
		t.Errorf("EMA factor = %v, want 1.4375", f)
	}
	if got := c.Apply("openai", "gpt-4", false, 1000); got != 1438 {
		t.Errorf("Apply(1000) = %d, want 1438 (round(1000*1.4375))", got)
	}
}

func TestCalibrationClampAndRejections(t *testing.T) {
	c := NewCalibrator()
	// Ratio 10 clamps the seed to 2.0; two more keep it there.
	for i := 0; i < 3; i++ {
		c.Record("openai", "gpt-4", false, 1000, 10000)
	}
	if f := c.Factor("openai", "gpt-4", false); f != 2.0 {
		t.Errorf("clamped Factor = %v, want 2.0", f)
	}
	// Wild ratios are rejected, not absorbed: a fresh bucket
	// stays unobserved after ratio 0.01 and ratio 25.
	c.Record("anthropic", "claude-sonnet-4", false, 1000, 10)    // 0.01
	c.Record("anthropic", "claude-sonnet-4", false, 1000, 25000) // 25
	if _, _, ok := c.Bucket("anthropic", "claude-sonnet-4", false); ok {
		t.Errorf("wild-ratio observations created a bucket, want rejected")
	}
	// Small estimates are rejected (framing noise, not density).
	c.Record("anthropic", "claude-sonnet-4", false, 50, 60)
	if _, _, ok := c.Bucket("anthropic", "claude-sonnet-4", false); ok {
		t.Errorf("sub-100 estimate created a bucket, want rejected")
	}
	// Floor boundary (review nit 3): 99 rejected, exactly 100 accepted.
	// (Separate provider so the later zero-report check below still
	// sees a nonexistent bucket for anthropic.)
	c.Record("floor-test", "claude-sonnet-4", false, 99, 119)
	if _, _, ok := c.Bucket("floor-test", "claude-sonnet-4", false); ok {
		t.Errorf("estimate 99 created a bucket, want rejected (floor is 100)")
	}
	c.Record("floor-test", "claude-sonnet-4", false, 100, 120)
	if _, _, ok := c.Bucket("floor-test", "claude-sonnet-4", false); !ok {
		t.Errorf("estimate 100 created no bucket, want accepted (floor is 100)")
	}
	// A zero / negative report is rejected.
	c.Record("anthropic", "claude-sonnet-4", false, 1000, 0)
	if _, _, ok := c.Bucket("anthropic", "claude-sonnet-4", false); ok {
		t.Errorf("zero-report observation created a bucket, want rejected")
	}
	// Low clamp: ratio 0.1 seeds at 0.15 after 3 samples.
	for i := 0; i < 3; i++ {
		c.Record("cohere", "command-r", false, 1000, 100)
	}
	if f := c.Factor("cohere", "command-r", false); f != 0.15 {
		t.Errorf("low-clamped Factor = %v, want 0.15", f)
	}
	if got := c.Apply("cohere", "command-r", false, 1000); got != 150 {
		t.Errorf("Apply(1000) low-clamped = %d, want 150", got)
	}
}

func TestCalibrationBucketsSplitByProviderModelTools(t *testing.T) {
	c := NewCalibrator()
	// Same model, tools vs no-tools: separate buckets.
	for i := 0; i < 3; i++ {
		c.Record("openai", "gpt-4", true, 1000, 1500)  // 1.5
		c.Record("openai", "gpt-4", false, 1000, 1000) // 1.0
	}
	if f := c.Factor("openai", "gpt-4", true); f != 1.5 {
		t.Errorf("tools Factor = %v, want 1.5", f)
	}
	if f := c.Factor("openai", "gpt-4", false); f != 1.0 {
		t.Errorf("no-tools Factor = %v, want 1.0", f)
	}
	// Same model, different provider: separate bucket, still cold.
	if f := c.Factor("azure", "gpt-4", true); f != 1 {
		t.Errorf("other-provider Factor = %v, want 1.0 (cold)", f)
	}
	// Reset forgets everything.
	c.Reset()
	if f := c.Factor("openai", "gpt-4", true); f != 1 {
		t.Errorf("after Reset Factor = %v, want 1.0", f)
	}
}

// TestCalibrationBucketCapDropsNewBuckets (review nit 3): at the
// 1024-bucket cap a NEW bucket is dropped, never a learned one.
func TestCalibrationBucketCapDropsNewBuckets(t *testing.T) {
	c := NewCalibrator()
	// One learned bucket first: 3 samples at ratio 1.5.
	for i := 0; i < 3; i++ {
		c.Record("learned", "gpt-4", false, 1000, 1500)
	}
	if f := c.Factor("learned", "gpt-4", false); f != 1.5 {
		t.Fatalf("learned Factor = %v, want 1.5", f)
	}
	// Fill the remaining buckets with distinct providers.
	for i := 0; i < maxCalibrationBuckets-1; i++ {
		c.Record(fmt.Sprintf("prov-%04d", i), "gpt-4", false, 1000, 1000)
	}
	// The 1025th bucket is dropped.
	c.Record("prov-overflow", "gpt-4", false, 1000, 1000)
	if _, _, ok := c.Bucket("prov-overflow", "gpt-4", false); ok {
		t.Errorf("bucket over the cap was created, want dropped")
	}
	// The learned bucket survives the cap with its factor intact.
	if f := c.Factor("learned", "gpt-4", false); f != 1.5 {
		t.Errorf("learned Factor after cap = %v, want 1.5 (learned buckets are never evicted)", f)
	}
}

// End to end through the process-wide calibrator and the
// calibrated wire count: cold it equals CountPromptWire exactly;
// once 3 observations land for the bucket it scales the wire
// count by the learned factor.
func TestCountPromptWireCalibrated(t *testing.T) {
	ResetCalibrationForTests()
	longText := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 30)
	req := provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: longText}}}
	base, err := CountPromptWire("gpt-4", "openai", req)
	if err != nil {
		t.Fatalf("CountPromptWire: %v", err)
	}
	if base.Tokens < 100 {
		t.Fatalf("test prompt too short for calibration floor: %d tokens", base.Tokens)
	}
	cold, err := CountPromptWireCalibrated("openai", "gpt-4", "openai", req)
	if err != nil {
		t.Fatalf("CountPromptWireCalibrated cold: %v", err)
	}
	if cold.Tokens != base.Tokens {
		t.Errorf("cold calibrated = %d, want base %d (factor 1.0)", cold.Tokens, base.Tokens)
	}
	// Three observations at ratio ~1.2 for this bucket. Note the
	// ratio is reported/base, not exactly 1.2 (368/307), so the
	// factor assertion below uses a tolerance and the scaled
	// count is computed from the learned factor itself.
	reported := int(float64(base.Tokens) * 1.2)
	for i := 0; i < 3; i++ {
		RecordCalibrationObservation("openai", "gpt-4", false, base.Tokens, reported)
	}
	learned := CalibrationFactor("openai", "gpt-4", false)
	if math.Abs(learned-1.2) > 0.01 {
		t.Errorf("learned Factor = %v, want ~1.2", learned)
	}
	hot, err := CountPromptWireCalibrated("openai", "gpt-4", "openai", req)
	if err != nil {
		t.Fatalf("CountPromptWireCalibrated hot: %v", err)
	}
	want := int(math.Round(float64(base.Tokens) * learned))
	if hot.Tokens != want {
		t.Errorf("hot calibrated = %d, want %d (base %d x %.4f)", hot.Tokens, want, base.Tokens, learned)
	}
	// A different provider is still cold — the factor does not leak.
	other, _ := CountPromptWireCalibrated("azure", "gpt-4", "openai", req)
	if other.Tokens != base.Tokens {
		t.Errorf("other-provider calibrated = %d, want base %d", other.Tokens, base.Tokens)
	}
	t.Logf("M7.9 calibration e2e: base=%d reported=%d factor=1.2 calibrated=%d", base.Tokens, reported, hot.Tokens)
	ResetCalibrationForTests()
}
