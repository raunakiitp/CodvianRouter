// Wait budgets (M4.8, the sector baseline settings.ts:143-159) — opt-in.
//
// The default philosophy is fail-fast: a 429 fails over at once,
// and a Retry-After >= 60 s abandons the target. Wait budgets are
// the opposite philosophy, for operators who would rather wait a
// rate limit out on the same target than switch providers: when
// enabled, a plain rate-limit 429 is slept out (up to 90 s per
// wait, at most 5 waits, 300 s total per request) and the same
// target retried, even when further targets exist.
//
// Never on quota-exhausted 429s (billing state — waiting cannot
// fix it), never on concurrency-limit 429s (fail over now, 1 s
// cool, per M4.4), never on auth/not-found (those are not 429s
// and never reach the rate-limit branch). With budgets on, a
// 60–90 s hint is waited, not abandoned; above 90 s it still
// fails over. Default OFF: fail-fast is preserved unless the
// operator opts in.
package router

import (
	"errors"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// WaitBudgetConfig is the opt-in wait-budget configuration. Zero
// values mean the the sector baseline defaults (90 s / 5 / 300 s).
type WaitBudgetConfig struct {
	// Enabled turns wait budgets on. Default false (fail-fast).
	Enabled bool
	// MaxWaitPerRetry caps one wait (default 90 s). A hint above
	// it is never waited out — the chain fails over instead.
	MaxWaitPerRetry time.Duration
	// MaxRetries caps the waits per request (default 5).
	MaxRetries int
	// TotalBudget caps all waiting per request (default 300 s).
	TotalBudget time.Duration
}

// DefaultWaitBudgetPerRetry is the per-wait cap (90 s).
const DefaultWaitBudgetPerRetry = 90 * time.Second

// DefaultWaitBudgetRetries is the per-request wait count cap (5).
const DefaultWaitBudgetRetries = 5

// DefaultWaitBudgetTotal is the per-request total wait cap (300 s).
const DefaultWaitBudgetTotal = 300 * time.Second

func (c WaitBudgetConfig) perWait() time.Duration {
	if c.MaxWaitPerRetry <= 0 {
		return DefaultWaitBudgetPerRetry
	}
	return c.MaxWaitPerRetry
}

func (c WaitBudgetConfig) retries() int {
	if c.MaxRetries <= 0 {
		return DefaultWaitBudgetRetries
	}
	return c.MaxRetries
}

func (c WaitBudgetConfig) total() time.Duration {
	if c.TotalBudget <= 0 {
		return DefaultWaitBudgetTotal
	}
	return c.TotalBudget
}

// waitBudgetWait decides whether a rate-limit failure may be waited
// out on the same target under the budget, and for how long. wait
// is noteFailure's wait for this attempt (0 for an abandoned hint);
// waitsUsed / waitedTotal are this request's budget consumption so
// far. It returns (0, false) when the budget declines — the caller
// falls through to the normal failover logic.
func (e *Engine) waitBudgetWait(err error, wait time.Duration, waitsUsed int, waitedTotal time.Duration) (time.Duration, bool) {
	cfg := e.cfg.WaitBudgets
	if !cfg.Enabled {
		return 0, false
	}
	// Never on quota-exhausted or concurrency-limit 429s: the
	// body classifies them exactly as noteFailure did (M4.4).
	var rle *provider.RateLimitError
	body := ""
	if errors.As(err, &rle) && rle != nil {
		body = rle.Body
	}
	if wire.IsQuotaExhaustedSignal(body) || wire.IsConcurrencyLimitSignal(body) {
		return 0, false
	}
	if waitsUsed >= cfg.retries() {
		return 0, false
	}
	ra, present := retryAfterOf(err)
	w := wait
	if present {
		// A hint above the per-wait cap is never waited out.
		if ra > cfg.perWait() {
			return 0, false
		}
		// The honoured hint, floored at 10 s and uncapped:
		// for a 60–90 s hint this is the real hint, not the
		// 0 noteFailure reported for the abandoned target.
		w = retryAfterHonouredWait(ra, present, 0)
	}
	if w <= 0 {
		return 0, false
	}
	if waitedTotal+w > cfg.total() {
		return 0, false
	}
	return w, true
}
