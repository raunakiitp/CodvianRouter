package tokencount

import (
	"math"
	"sync"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Runtime calibration of the local token estimate against
// provider-reported usage (M7.9; parity tracker Area 10 "Runtime
// calibration vs provider-reported usage", was MISSING).
//
// The shape is the reference router's published calibration
// (open-sse/services/estimatorCalibration.ts, #14931 — studied,
// not copied: this file is an independent Go implementation of
// the same published parameters): one exponential moving average
// of actual/estimated per (provider, model, tools-present)
// bucket, α=0.25, applied only once a bucket holds ≥3 samples,
// clamped to [0.15, 2.0]. Cold start is factor 1.0 —
// byte-identical to the uncalibrated count — and state is
// in-memory only, so a restart re-learns from 1.0.
//
// Where the parameters are adapted, and why (not copied blind):
//   - the reference router calibrates a chars/4 guard estimate and rejects
//     observations with estimated < 1000 tokens, because a tiny
//     chars/4 estimate carries an unstable ratio. Our estimate is
//     an exact BPE count whose residual is a few framing tokens,
//     so the floor is 100 tokens instead: below that the ratio is
//     framing noise, not tokenizer density; at ≥100 a few tokens
//     of framing move the ratio by at most a few percent.
//   - the reference router's buckets live in an LRU capped at 1000. Ours
//     cap at 1024 buckets and, when full, drop NEW buckets rather
//     than evicting learned ones: evicting a learned bucket would
//     silently return a hot (provider, model) pair to factor 1.0.
//   - the reference router keys on (provider, model, tools). Ours does the
//     same; the ledger already carries both sides of the
//     comparison per (provider, model) (ledger.Usage:
//     PromptTokens = reported, LocalEstimatePromptTokens =
//     local; Ledger.DriftSummary) — but the ledger stores NO
//     tools flag, so a ledger-only feed cannot build the tools
//     bucket. The live feed comes from the router's
//     reconcileUsage, which holds providerID, model, req.Tools,
//     the local count and the reported count for the same
//     request: one RecordCalibrationObservation call there per
//     compared request, and CountPromptWireCalibrated at the
//     post-routing estimate sites
//     (Engine.localPromptCountCalibrated). Pre-routing
//     CountPromptFull sites have neither provider nor wire and
//     stay uncalibrated.
//
// The calibrator never touches pricing or the ledger: it only
// scales a local ESTIMATE for pre-send budget/context decisions,
// and the provider-reported number is still recorded verbatim
// wherever one exists.

// Calibration parameters — the α / sample floor / clamp match
// the reference router's published values (see the package doc above).
const (
	calibrationAlpha      = 0.25
	calibrationMinSamples = 3
	calibrationFactorMin  = 0.15
	calibrationFactorMax  = 2.0
	// calibrationRatioMin/Max reject wild observations (shape
	// discontinuities, aggregated multi-leg usage), same bounds
	// as the reference router's.
	calibrationRatioMin = 0.05
	calibrationRatioMax = 20.0
	// calibrationMinEstimate is the adaptation documented above:
	// 100, not the reference router's 1000 (chars/4 vs BPE).
	calibrationMinEstimate = 100
	maxCalibrationBuckets  = 1024
)

type calibrationKey struct {
	provider     string
	model        string
	toolsPresent bool
}

type calibrationBucket struct {
	factor  float64
	samples int
}

// Calibrator learns actual/estimated ratios per (provider,
// model, tools) bucket. The zero value is not usable — use
// NewCalibrator. All methods are safe for concurrent use.
type Calibrator struct {
	mu      sync.Mutex
	buckets map[calibrationKey]*calibrationBucket
}

// NewCalibrator returns an empty calibrator (all factors 1.0).
func NewCalibrator() *Calibrator {
	return &Calibrator{buckets: make(map[calibrationKey]*calibrationBucket)}
}

func clampCalibrationFactor(v float64) float64 {
	return math.Min(calibrationFactorMax, math.Max(calibrationFactorMin, v))
}

// Record absorbs one (estimated, reported) observation for the
// bucket. It is a no-op when the observation cannot carry a
// stable ratio: estimated < calibrationMinEstimate, reported
// <= 0, or the ratio outside [0.05, 20]. The first observation
// seeds the bucket at clamp(ratio); later ones move the EMA by
// α towards clamp(ratio). Samples are counted even before the
// factor applies (the factor gates on >= 3 samples).
func (c *Calibrator) Record(provider, model string, toolsPresent bool, estimated, reported int) {
	if estimated < calibrationMinEstimate || reported <= 0 {
		return
	}
	ratio := float64(reported) / float64(estimated)
	if ratio < calibrationRatioMin || ratio > calibrationRatioMax {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := calibrationKey{provider: provider, model: model, toolsPresent: toolsPresent}
	b, ok := c.buckets[key]
	if !ok {
		if len(c.buckets) >= maxCalibrationBuckets {
			return // Full: drop a new bucket, never evict a learned one.
		}
		c.buckets[key] = &calibrationBucket{factor: clampCalibrationFactor(ratio), samples: 1}
		return
	}
	b.factor = clampCalibrationFactor(b.factor + calibrationAlpha*(clampCalibrationFactor(ratio)-b.factor))
	b.samples++
}

// Factor returns the calibration factor for the bucket: 1.0 on
// cold start (no bucket, or fewer than 3 samples), otherwise
// the EMA clamped to [0.15, 2.0].
func (c *Calibrator) Factor(provider, model string, toolsPresent bool) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.buckets[calibrationKey{provider: provider, model: model, toolsPresent: toolsPresent}]
	if !ok || b.samples < calibrationMinSamples {
		return 1
	}
	return b.factor
}

// Bucket reports a bucket's factor and sample count for
// introspection (tests, future diagnostics). ok is false when
// the bucket has never been observed.
func (c *Calibrator) Bucket(provider, model string, toolsPresent bool) (factor float64, samples int, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, found := c.buckets[calibrationKey{provider: provider, model: model, toolsPresent: toolsPresent}]
	if !found {
		return 0, 0, false
	}
	return b.factor, b.samples, true
}

// Apply scales tokens by the bucket's factor: the input
// unchanged on cold start or for non-positive inputs, otherwise
// max(1, round(tokens * factor)).
func (c *Calibrator) Apply(provider, model string, toolsPresent bool, tokens int) int {
	if tokens <= 0 {
		return tokens
	}
	f := c.Factor(provider, model, toolsPresent)
	if f == 1 {
		return tokens
	}
	return max(1, int(math.Round(float64(tokens)*f)))
}

// Reset forgets everything learned (tests; a restart does the
// same for the process-wide calibrator).
func (c *Calibrator) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buckets = make(map[calibrationKey]*calibrationBucket)
}

// defaultCalibrator is the process-wide calibrator the router
// feeds from reconcileUsage and the estimate sites read via
// CountPromptWireCalibrated. Cold start it is empty and every
// factor is 1.0; it learns from live traffic as compared
// requests arrive.
var defaultCalibrator = NewCalibrator()

// RecordCalibrationObservation feeds one compared request into
// the process-wide calibrator: estimated is the UNCALIBRATED
// local count, reported the provider-reported prompt tokens,
// toolsPresent whether the request carried tools. The observation
// MUST use the uncalibrated local: recording a calibrated local
// would feed the calibrator its own output, and the EMA would
// learn the square root of the true ratio instead of the ratio.
// Call it from the one place that holds all four values for the
// same request — the router's reconcileUsage — once per compared
// request, never for local-estimate fallback rows (no reported
// number) or shared / aggregated rows (their prompt_tokens
// double-count re-sent history).
func RecordCalibrationObservation(provider, model string, toolsPresent bool, estimated, reported int) {
	defaultCalibrator.Record(provider, model, toolsPresent, estimated, reported)
}

// CalibrationFactor is the process-wide factor for a bucket
// (1.0 cold). Exposed for diagnostics and tests.
func CalibrationFactor(provider, model string, toolsPresent bool) float64 {
	return defaultCalibrator.Factor(provider, model, toolsPresent)
}

// ResetCalibrationForTests forgets the process-wide calibrator
// (tests only).
func ResetCalibrationForTests() {
	defaultCalibrator.Reset()
}

// CountPromptWireCalibrated is CountPromptWire with the learned
// calibration applied: the wire count for (provider, model,
// tools) scaled by the bucket factor once the bucket holds ≥3
// samples, unchanged otherwise. The provider is a parameter because
// CountPromptWire only knows the wire — the bucket key needs
// the provider identity.
func CountPromptWireCalibrated(providerID, model, wire string, req provider.ChatRequest) (TokenCount, error) {
	base, err := CountPromptWire(model, wire, req)
	if err != nil {
		return TokenCount{}, err
	}
	calibrated := defaultCalibrator.Apply(providerID, model, len(req.Tools) > 0, base.Tokens)
	return TokenCount{Tokens: calibrated, Encoding: base.Encoding, Fallback: base.Fallback, Estimated: base.Estimated}, nil
}
