// Package adaptive implements per-(provider, key) adaptive concurrency
// limiting for Codvian Router, following the Gradient2 algorithm from
// Netflix's concurrency-limits library (the same idea Envoy's adaptive
// concurrency uses): the in-flight cap for a provider moves with the
// gradient between the provider's current (smoothed) RTT and its
// long-term minimum RTT.
//
//	newLimit = limit * (rttMin / rttSmoothed) + queueSize
//
// When a provider slows, its gradient drops below 1 and the cap
// shrinks, so fewer new calls are admitted to it and the router sheds
// load to healthy providers BEFORE its queue grows and before 429s
// appear. When it is fast again the gradient returns to ~1 and the
// cap grows back (by queueSize per sample, so recovery is gradual,
// not a flood).
//
// Failure policy — FAIL OPEN: this package has no error return on the
// admission path (TryAcquire returns a bool, never an error), and a
// nil *Limiter admits everything. A limiter failure, a missing
// limiter, or a key with no state must never black out traffic: the
// router treats "no limiter" and "no state" as "admit". The only way
// a call is shed is an explicit, measured decision — in-flight at the
// current cap for a provider whose RTT has actually degraded.
//
// Keys: the caller keys state by provider ID (the provider's identity
// in the registry — one entry per provider API key). Two providers
// sharing one upstream host still get separate caps, because they are
// separate billing/rate-limit identities.
//
// This package is a leaf: stdlib only, no internal imports, so the
// router, server and metrics packages can all use it without an
// import cycle.
package adaptive

import (
	"sort"
	"sync"
	"time"
)

// Defaults: the initial cap, the floor it can shrink to, and the
// ceiling it can grow to. The floor is 2, not 1: a single slow probe
// must always be possible, otherwise a provider that has slowed
// could never prove it recovered. The ceiling (100) bounds a fast
// provider's growth so one provider cannot swallow the whole
// process's concurrency.
const (
	DefaultInitialLimit = 20.0
	DefaultMinLimit     = 2.0
	DefaultMaxLimit     = 100.0
	// queueSize is the Gradient2 queue-size term: each completed
	// sample adds this much headroom, so the cap grows by ~1 per
	// fast sample and shrinks multiplicatively per slow sample.
	defaultQueueSize = 1.0
	// smoothing is the EWMA weight for the current RTT: 0.2 makes
	// the smoothed RTT react to a slowdown within a handful of
	// samples without jittering on one slow call.
	defaultSmoothing = 0.2
	// baselineDrift is how fast the long-term minimum RTT is
	// allowed to rise when every recent sample is slower than it:
	// 0.0002 per sample means a permanently-slower provider's
	// baseline adapts over thousands of samples instead of pinning
	// the gradient (and the cap) at the floor forever, while a
	// handful of slow samples cannot move the baseline at all
	// (5 samples at 400ms vs a 5ms baseline move it by ~0.4ms).
	// A faster sample always lowers the baseline immediately.
	defaultBaselineDrift = 0.0002
)

// ErrLimited is returned (as a sentinel error) by callers that need
// an error value for a shed attempt — the limiter itself never
// returns it. It is defined here so the router and the limiter agree
// on one value: errors.Is(err, ErrLimited) means "admission refused
// by the adaptive cap", never an upstream failure, so it must not
// trip a circuit breaker or be recorded as a failed attempt.
var ErrLimited = errLimited{}

type errLimited struct{}

func (errLimited) Error() string { return "adaptive concurrency limit reached" }

// Config tunes a Limiter. Zero values get the documented defaults.
type Config struct {
	// InitialLimit is the cap a provider starts with, before any
	// RTT has been measured. Default 20.
	InitialLimit float64
	// MinLimit is the floor the cap can shrink to. Default 2.
	MinLimit float64
	// MaxLimit is the ceiling the cap can grow to. Default 100.
	MaxLimit float64
}

func (c Config) withDefaults() Config {
	if c.InitialLimit <= 0 {
		c.InitialLimit = DefaultInitialLimit
	}
	if c.MinLimit <= 0 {
		c.MinLimit = DefaultMinLimit
	}
	if c.MaxLimit <= 0 {
		c.MaxLimit = DefaultMaxLimit
	}
	if c.MinLimit > c.InitialLimit {
		c.MinLimit = c.InitialLimit
	}
	if c.MaxLimit < c.InitialLimit {
		c.MaxLimit = c.InitialLimit
	}
	return c
}

// state is the per-key limiter state.
type state struct {
	limit     float64
	inFlight  int
	rttMinMs  float64 // long-term minimum RTT, ms
	rttSmooth float64 // EWMA-smoothed RTT, ms
	samples   uint64
	admitted  uint64
	rejected  uint64
	// learned is the latest upstream rate-limit signal for this
	// key (M5.13), or nil when nothing has been learned yet.
	learned *RateLimitSignals
}

// State is a snapshot of one key's limiter state, for the dashboard
// and /metrics. Limit is the current adaptive cap (fractional — the
// admission check uses it directly, so a limit of 2.5 admits 2
// concurrent calls). RTTMs is the smoothed RTT, BaselineMs the
// long-term minimum RTT; the ratio BaselineMs/RTTMs is the gradient.
// Samples is how many completed calls fed the estimate; before any
// sample the RTT fields are 0 and Limit is the initial cap.
// PacedLimit is the cap the learned upstream rate-limit signals
// would impose right now (0 when no fresh signal applies — the
// cap is then purely the Gradient2 value).
type State struct {
	Provider   string  `json:"provider"`
	Limit      float64 `json:"limit"`
	InFlight   int     `json:"in_flight"`
	RTTMs      float64 `json:"rtt_ms"`
	BaselineMs float64 `json:"baseline_ms"`
	Samples    uint64  `json:"samples"`
	Admitted   uint64  `json:"admitted"`
	Rejected   uint64  `json:"rejected"`
	PacedLimit float64 `json:"paced_limit,omitempty"`
}

// Limiter is a per-key adaptive concurrency limiter. The zero value
// is not usable — use New. A nil *Limiter is usable as a fail-open
// stub: every method on it admits / no-ops.
type Limiter struct {
	cfg  Config
	mu   sync.Mutex
	keys map[string]*state
}

// New returns a Limiter with the given config (defaults when zero).
func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg.withDefaults(), keys: make(map[string]*state)}
}

// get returns the state for key, creating it at the initial cap.
// Caller must hold l.mu.
func (l *Limiter) get(key string) *state {
	st, ok := l.keys[key]
	if !ok {
		st = &state{limit: l.cfg.InitialLimit}
		l.keys[key] = st
	}
	return st
}

// TryAcquire admits one call for key when in-flight is below the
// current cap, and records it as in-flight. It returns false when
// the cap is reached (the caller should shed to another provider).
// It FAILS OPEN: a nil limiter, or a key with no state, admits.
func (l *Limiter) TryAcquire(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.get(key)
	if float64(st.inFlight) >= st.limit {
		st.rejected++
		return false
	}
	st.inFlight++
	st.admitted++
	return true
}

// Release ends one in-flight call for key. Releasing a key that was
// never acquired (or twice) is a no-op — it clamps at 0 instead of
// going negative and corrupting the cap.
func (l *Limiter) Release(key string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.keys[key]
	if !ok || st.inFlight <= 0 {
		return
	}
	st.inFlight--
}

// Record feeds one completed call's RTT into the gradient for key.
// Only successful calls should be recorded: a failed call's latency
// is not the provider's serving RTT (a fast 429 or a connection
// refusal says nothing about how fast it answers). A nil limiter
// no-ops. Recording never blocks admission: it only moves the cap.
func (l *Limiter) Record(key string, rtt time.Duration, ok bool) {
	if l == nil || !ok || rtt < 0 {
		return
	}
	rttMs := float64(rtt.Microseconds()) / 1000
	if rttMs < 0.05 {
		// Timer-resolution floor: the router measures RTT in
		// whole milliseconds, so a sub-50µs RTT is
		// indistinguishable from 0 — record it as 50µs rather
		// than 0, so a 0 can never zero out the baseline and
		// divide the gradient by nothing.
		rttMs = 0.05
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.get(key)
	st.samples++
	if st.rttMinMs == 0 || rttMs < st.rttMinMs {
		st.rttMinMs = rttMs
	} else {
		// No faster sample: let the baseline drift slowly upward
		// so a permanently-slower provider eventually gets a
		// gradient near 1 again instead of a permanently floored cap.
		st.rttMinMs += (rttMs - st.rttMinMs) * defaultBaselineDrift
	}
	if st.rttSmooth == 0 {
		st.rttSmooth = rttMs
	} else {
		st.rttSmooth = st.rttSmooth*(1-defaultSmoothing) + rttMs*defaultSmoothing
	}
	gradient := 1.0
	if st.rttSmooth > 0 && st.rttMinMs > 0 {
		gradient = st.rttMinMs / st.rttSmooth
		if gradient > 1 {
			gradient = 1
		}
		if gradient < 0 {
			gradient = 0
		}
	}
	next := st.limit*gradient + defaultQueueSize
	if next < l.cfg.MinLimit {
		next = l.cfg.MinLimit
	}
	if next > l.cfg.MaxLimit {
		next = l.cfg.MaxLimit
	}
	st.limit = next
}

// ApplyLearnedLimit paces key's cap from one upstream rate-limit
// signal (M5.13). The latest signal wins (an upstream's remaining
// only moves as requests consume it, so the newest header is the
// current truth); an empty signal is ignored. The paced cap is
// clamp(MaxLimit * remainingFraction, MinLimit, MaxLimit) and only
// ever shrinks the stored cap, never raises it — when a window
// resets, the cap recovers gradually through normal Gradient2
// growth instead of flooding the upstream at once. A nil limiter
// no-ops. Stale signals (past their reset, or older than the
// 5-minute TTL) are stored but never pace: the next response
// re-learns, and the cap recovers through Record in the meantime.
func (l *Limiter) ApplyLearnedLimit(key string, sig RateLimitSignals, now time.Time) {
	if l == nil || sig.Empty() {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.get(key)
	cp := sig
	st.learned = &cp
	paced := l.pacedLimitLocked(st, now)
	if paced > 0 && paced < st.limit {
		st.limit = paced
	}
}

// PacedLimit returns the cap the freshest learned signal for key
// would impose right now, and whether any fresh signal applies. A
// nil limiter, a key with no signal, or a stale signal reports
// (0, false).
func (l *Limiter) PacedLimit(key string, now time.Time) (float64, bool) {
	if l == nil {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.keys[key]
	if !ok {
		return 0, false
	}
	paced := l.pacedLimitLocked(st, now)
	if paced <= 0 {
		return 0, false
	}
	return paced, true
}

// pacedLimitLocked computes the paced cap from the state's latest
// learned signal: 0 when no fresh signal applies. Caller must hold
// l.mu.
func (l *Limiter) pacedLimitLocked(st *state, now time.Time) float64 {
	if st.learned == nil || !st.learned.Fresh(now) {
		return 0
	}
	paced := l.cfg.MaxLimit * st.learned.RemainingFraction()
	if paced < l.cfg.MinLimit {
		paced = l.cfg.MinLimit
	}
	if paced > l.cfg.MaxLimit {
		paced = l.cfg.MaxLimit
	}
	return paced
}

// Snapshot returns the current state of every key with state, sorted
// by provider key. A nil limiter returns an empty list. Keys that
// were never used have no entry: the dashboard shows providers with
// no entry as "no traffic yet", never an invented limit.
func (l *Limiter) Snapshot() []State {
	if l == nil {
		return []State{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	out := make([]State, 0, len(l.keys))
	for k, st := range l.keys {
		out = append(out, State{
			Provider:   k,
			Limit:      st.limit,
			InFlight:   st.inFlight,
			RTTMs:      st.rttSmooth,
			BaselineMs: st.rttMinMs,
			Samples:    st.samples,
			Admitted:   st.admitted,
			Rejected:   st.rejected,
			PacedLimit: l.pacedLimitLocked(st, now),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// StateFor returns the snapshot for one key and whether it has any
// state (false before the key is ever used). A nil limiter reports
// no state.
func (l *Limiter) StateFor(key string) (State, bool) {
	if l == nil {
		return State{}, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.keys[key]
	if !ok {
		return State{}, false
	}
	return State{
		Provider:   key,
		Limit:      st.limit,
		InFlight:   st.inFlight,
		RTTMs:      st.rttSmooth,
		BaselineMs: st.rttMinMs,
		Samples:    st.samples,
		Admitted:   st.admitted,
		Rejected:   st.rejected,
		PacedLimit: l.pacedLimitLocked(st, time.Now()),
	}, true
}

// Limit returns the current cap for key (the initial cap before any
// sample). A nil limiter returns the default initial cap.
func (l *Limiter) Limit(key string) float64 {
	if l == nil {
		return DefaultInitialLimit
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.get(key).limit
}

// HasHeadroom reports whether key can admit another call right now
// (in-flight below the cap). A nil limiter always has headroom —
// fail open.
func (l *Limiter) HasHeadroom(key string) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.get(key)
	return float64(st.inFlight) < st.limit
}

// InFlight returns the current in-flight count for key (0 when the
// key has no state). A nil limiter returns 0.
func (l *Limiter) InFlight(key string) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if st, ok := l.keys[key]; ok {
		return st.inFlight
	}
	return 0
}

// ---------------------------------------------------------------------------
// Global admission cap (P2-CAP-002): an optional process-wide ceiling on
// in-flight requests, consulted before dispatch in addition to the
// per-key adaptive limiter. Nil (unset) means fail open — no process-wide
// cap — which preserves existing behaviour. When set, it bounds aggregate
// concurrency regardless of how many providers/keys are active.
// P2-CAP-002: No process-wide in-flight cap existed; each key could reach
// MaxLimit=100, so aggregate concurrency ≈ P×100 unbounded goroutines.

// globalSemaphore is a channel-based weighted semaphore: TryAcquire
// non-blockingly reserves one slot, Release frees it.
type globalSemaphore struct {
	ch    chan struct{}
	mu    sync.Mutex
	inFlight int
}

func newGlobalSemaphore(capacity int) *globalSemaphore {
	return &globalSemaphore{ch: make(chan struct{}, capacity)}
}

func (s *globalSemaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		s.mu.Lock()
		s.inFlight++
		s.mu.Unlock()
		return true
	default:
		return false
	}
}

func (s *globalSemaphore) Release() {
	select {
	case <-s.ch:
	default:
		// Should not happen if Release matches TryAcquire, but
		// guard against double-release.
	}
	s.mu.Lock()
	if s.inFlight > 0 {
		s.inFlight--
	}
	s.mu.Unlock()
}

func (s *globalSemaphore) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

var globalLimiter *globalSemaphore

// SetGlobalLimiter installs the process-wide admission cap. Nil
// disables it (fail open). Called once at startup.
func SetGlobalLimiter(capacity int) {
	if capacity <= 0 {
		globalLimiter = nil
		return
	}
	globalLimiter = newGlobalSemaphore(capacity)
}

// GlobalTryAcquire admits one call under the process-wide cap.
// Returns false when the aggregate cap is reached. Nil limiter
// admits everything (fail open).
func GlobalTryAcquire() bool {
	if globalLimiter == nil {
		return true
	}
	return globalLimiter.TryAcquire()
}

// GlobalRelease frees one slot under the process-wide cap.
func GlobalRelease() {
	if globalLimiter == nil {
		return
	}
	globalLimiter.Release()
}

// GlobalInFlight returns the current aggregate in-flight count under
// the process-wide cap (0 when unset). For dashboard exposure.
func GlobalInFlight() int {
	if globalLimiter == nil {
		return 0
	}
	return globalLimiter.InFlight()
}
