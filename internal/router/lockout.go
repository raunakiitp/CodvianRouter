// Model lockout (M4.3, OmniRoute modelLockoutSettings.ts).
//
// A dead model retried first on every request burns an attempt on
// every request: the provider answers fine, only the model is dead
// at that (provider, connection, model). The lockout remembers that
// triple: after a lockout-eligible failure the triple is locked for
// a cooldown, and the engine skips it (trace: skipped_model_lockout)
// until the cooldown ends — the next target serves instead.
//
// Defaults are OmniRoute's verbatim (modelLockoutSettings.ts):
// OFF by default (enabled: false), error codes 403/404/429/502/503/504,
// base cooldown 120 s, max 30 min, max 10 backoff steps, exponential
// backoff. The cooldown for the Nth consecutive eligible failure is
// min(base * 2^(N-1), max), capped at maxSteps steps. A success on
// the triple clears it — a recovered model serves again at once.
// A 500 is deliberately NOT in the code list (OmniRoute's list has
// no 500 either): a 500 is a provider failure, and the breaker
// already owns provider failures; the lockout owns per-model ones.
package router

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// LockoutConfig is the operator's model-lockout setting. Zero value
// means OFF (Enabled false) — the engine behaves exactly as before.
type LockoutConfig struct {
	// Enabled turns the lockout on. Default false (OmniRoute's
	// default: the feature is off until the operator opts in).
	Enabled bool
	// ErrorCodes are the statuses that lock the triple. Empty
	// means the default list (403/404/429/502/503/504).
	ErrorCodes []int
	// BaseCooldown is the first lockout (default 120 s). 0 means
	// the default. Must be > 0 when Enabled.
	BaseCooldown time.Duration
	// MaxCooldown caps the exponential growth (default 30 min).
	// 0 means the default.
	MaxCooldown time.Duration
	// MaxSteps caps the exponential ladder (default 10). 0 means
	// the default.
	MaxSteps int
}

// DefaultModelLockoutCodes are the statuses that lock a
// (provider, key, model) triple (OmniRoute DEFAULT_MODEL_LOCKOUT_SETTINGS).
var DefaultModelLockoutCodes = []int{403, 404, 429, 502, 503, 504}

// DefaultModelLockoutBase is the first lockout cooldown (120 s).
const DefaultModelLockoutBase = 120 * time.Second

// DefaultModelLockoutMax is the lockout cooldown cap (30 min).
const DefaultModelLockoutMax = 30 * time.Minute

// DefaultModelLockoutSteps is the exponential ladder cap (10).
const DefaultModelLockoutSteps = 10

// lockoutKey is one (provider, key, model) triple. The NUL separator
// cannot appear in a provider ID, a key ID, or a model name the
// engine routes (see ClientKey for the same construction).
type lockoutKey struct {
	provider string
	keyID    string
	model    string
}

// lockoutEntry is one triple's live lockout: how many consecutive
// eligible failures built it, and when the lock ends.
type lockoutEntry struct {
	failures int
	until    time.Time
}

// ModelLockout is the live per-triple lockout table. The zero value
// is unusable — build it with NewModelLockout. Safe for concurrent use.
type ModelLockout struct {
	mu      sync.Mutex
	cfg     LockoutConfig
	codes   map[int]bool
	entries map[lockoutKey]*lockoutEntry
}

// NewModelLockout builds a lockout table from cfg, filling defaults
// for every unset field. A disabled cfg still returns a usable
// table — every method is a no-op until Enabled is true, so callers
// never branch on it.
func NewModelLockout(cfg LockoutConfig) *ModelLockout {
	if len(cfg.ErrorCodes) == 0 {
		cfg.ErrorCodes = DefaultModelLockoutCodes
	}
	if cfg.BaseCooldown <= 0 {
		cfg.BaseCooldown = DefaultModelLockoutBase
	}
	if cfg.MaxCooldown <= 0 {
		cfg.MaxCooldown = DefaultModelLockoutMax
	}
	if cfg.MaxCooldown < cfg.BaseCooldown {
		cfg.MaxCooldown = cfg.BaseCooldown
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = DefaultModelLockoutSteps
	}
	codes := make(map[int]bool, len(cfg.ErrorCodes))
	for _, c := range cfg.ErrorCodes {
		codes[c] = true
	}
	return &ModelLockout{cfg: cfg, codes: codes, entries: map[lockoutKey]*lockoutEntry{}}
}

// Enabled reports whether the lockout is on.
func (m *ModelLockout) Enabled() bool {
	return m != nil && m.cfg.Enabled
}

// cooldownFor is the cooldown for the Nth consecutive eligible
// failure (N >= 1): base * 2^(N-1), capped at maxSteps steps and
// at MaxCooldown.
func (m *ModelLockout) cooldownFor(n int) time.Duration {
	steps := n - 1
	if steps > m.cfg.MaxSteps-1 {
		steps = m.cfg.MaxSteps - 1
	}
	if steps < 0 {
		steps = 0
	}
	d := m.cfg.BaseCooldown * time.Duration(1<<uint(steps))
	if d <= 0 || d > m.cfg.MaxCooldown {
		return m.cfg.MaxCooldown
	}
	return d
}

// Record notes one failure on the triple. It returns (locked, until):
// locked is true when the triple is locked after this failure, until
// is when the lock ends. It is a no-op returning (false, zero) when
// the lockout is off, when the status is not lockout-eligible, or
// when the error is nil.
func (m *ModelLockout) Record(providerID, keyID, model string, status int) (bool, time.Time) {
	if !m.Enabled() || !m.codes[status] {
		return false, time.Time{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := lockoutKey{providerID, keyID, model}
	e := m.entries[k]
	if e == nil {
		e = &lockoutEntry{}
		m.entries[k] = e
	}
	// A lock that already expired restarts the ladder: the model
	// recovered, failed again, and earns the base cooldown — not
	// the rung it died on last time.
	if !e.until.IsZero() && time.Now().After(e.until) {
		e.failures = 0
	}
	e.failures++
	e.until = time.Now().Add(m.cooldownFor(e.failures))
	return true, e.until
}

// Clear removes any lockout on the triple (a success on it).
func (m *ModelLockout) Clear(providerID, keyID, model string) {
	if !m.Enabled() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, lockoutKey{providerID, keyID, model})
}

// Locked reports whether the triple is locked right now, and until
// when. An expired lock reports unlocked (and is pruned).
func (m *ModelLockout) Locked(providerID, keyID, model string) (time.Time, bool) {
	if !m.Enabled() {
		return time.Time{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[lockoutKey{providerID, keyID, model}]
	if e == nil || e.until.IsZero() {
		return time.Time{}, false
	}
	if time.Now().After(e.until) {
		delete(m.entries, lockoutKey{providerID, keyID, model})
		return time.Time{}, false
	}
	return e.until, true
}

// LockoutStatus is one triple's live lockout, for observability.
type LockoutStatus struct {
	ProviderID string
	KeyID      string
	Model      string
	Failures   int
	Until      time.Time
}

// Locks returns every live (unexpired) lockout, in no particular
// order. Empty when the lockout is off or nothing is locked.
func (m *ModelLockout) Locks() []LockoutStatus {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := make([]LockoutStatus, 0, len(m.entries))
	for k, e := range m.entries {
		if e.until.IsZero() || now.After(e.until) {
			continue
		}
		out = append(out, LockoutStatus{ProviderID: k.provider, KeyID: k.keyID, Model: k.model, Failures: e.failures, Until: e.until})
	}
	return out
}

// lockoutRecord notes one attempt failure on the model lockout
// (M4.3): a lockout-eligible status on a real call locks the
// (provider, key, model) triple. Self-guarding: a nil engine, a
// disabled lockout, a cancellation, or a status outside the code
// list records nothing — callers never branch on it.
func (e *Engine) lockoutRecord(att Attempt, err error) {
	if e == nil || e.lockouts == nil || err == nil {
		return
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	e.lockouts.Record(att.ProviderID, att.KeyID, att.Model, lockoutStatusOf(err))
}

// lockoutClear clears any lockout on the triple (a success on it).
func (e *Engine) lockoutClear(att Attempt) {
	if e == nil || e.lockouts == nil {
		return
	}
	e.lockouts.Clear(att.ProviderID, att.KeyID, att.Model)
}

// lockoutLocked reports whether the attempt's triple is locked right
// now, and until when. False when the lockout is off.
func (e *Engine) lockoutLocked(att Attempt) (time.Time, bool) {
	if e == nil || e.lockouts == nil {
		return time.Time{}, false
	}
	return e.lockouts.Locked(att.ProviderID, att.KeyID, att.Model)
}

// ModelLockouts returns every live (unexpired) model lockout, for
// observability. Empty when the lockout is off or nothing is locked.
func (e *Engine) ModelLockouts() []LockoutStatus {
	if e == nil || e.lockouts == nil {
		return nil
	}
	return e.lockouts.Locks()
}

// lockoutStatusOf classifies an attempt error for the lockout: the
// HTTP status when the error carries one, or 0 when it does not
// (transport errors, cancellations, empty streams have no status
// and never lock). Cancellations report 0 — a cancelled call is
// not a failure of the model.
func lockoutStatusOf(err error) int {
	if err == nil {
		return 0
	}
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	return cls.Status
}
