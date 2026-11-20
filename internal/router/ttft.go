// Predictive TTFT breaker (M4.7a, the reference router comboPredicates.ts).
//
// A provider whose streams are slow to produce a first byte will
// be slow again: the engine records every stream's time-to-first-
// byte (TTFT) per provider and, once it has at least 5 samples,
// skips a target whose average TTFT exceeds
// Config.PredictiveTtftMs — before dialling it, so the request
// starts on a fast target instead of waiting out a slow one. The
// skip only fires when another target remains in the chain: a slow
// provider still serves when it is the only one left (slow beats
// failing). 0 disables the prediction entirely (the default).
//
// TTFT is recorded at the commit point (onCommit in runStream):
// the first content byte reaching the client. Attempts that fail
// before commit never record a sample — a dead provider's TTFT is
// the breaker's business, not this predictor's.
package router

import "time"

// ttftMinSamples is the fewest samples before a prediction is
// trusted (the reference router: min 5).
const ttftMinSamples = 5

// ttftWindow is how many recent TTFT samples are kept per provider.
// Old samples age out so a provider that recovers is un-skipped
// within a window of streams.
const ttftWindow = 20

// ttftStats is the rolling TTFT record for one provider.
type ttftStats struct {
	samples []time.Duration // ring, at most ttftWindow entries
	next    int
	sum     time.Duration
}

// add records one first-byte latency.
func (s *ttftStats) add(d time.Duration) {
	if len(s.samples) < ttftWindow {
		s.samples = append(s.samples, d)
		s.sum += d
		return
	}
	s.sum += d - s.samples[s.next]
	s.samples[s.next] = d
	s.next = (s.next + 1) % ttftWindow
}

// avg returns the mean TTFT and the sample count.
func (s *ttftStats) avg() (time.Duration, int) {
	if len(s.samples) == 0 {
		return 0, 0
	}
	return s.sum / time.Duration(len(s.samples)), len(s.samples)
}

// recordTTFT records one stream's time-to-first-byte for a provider.
func (e *Engine) recordTTFT(providerID string, d time.Duration) {
	if d < 0 {
		return
	}
	e.ttftMu.Lock()
	defer e.ttftMu.Unlock()
	st, ok := e.ttft[providerID]
	if !ok {
		st = &ttftStats{}
		e.ttft[providerID] = st
	}
	st.add(d)
}

// ttftAvg returns a provider's mean TTFT and sample count.
func (e *Engine) ttftAvg(providerID string) (time.Duration, int) {
	e.ttftMu.Lock()
	defer e.ttftMu.Unlock()
	st, ok := e.ttft[providerID]
	if !ok {
		return 0, 0
	}
	return st.avg()
}

// ttftSlow reports whether a provider is predicted slow: the
// predictor is on, it has at least ttftMinSamples, and its mean
// TTFT exceeds the configured threshold.
func (e *Engine) ttftSlow(providerID string) bool {
	threshold := e.cfg.PredictiveTtftMs
	if threshold <= 0 {
		return false
	}
	avg, n := e.ttftAvg(providerID)
	return n >= ttftMinSamples && avg > time.Duration(threshold)*time.Millisecond
}

// PredictiveTTFT reports a provider's mean TTFT in milliseconds and
// its sample count, for observability (dashboard / tests).
func (e *Engine) PredictiveTTFT(providerID string) (avgMs float64, samples int) {
	avg, n := e.ttftAvg(providerID)
	return float64(avg.Microseconds()) / 1000, n
}
