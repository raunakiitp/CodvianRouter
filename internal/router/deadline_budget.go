// Deadline-failure sub-budget (G11 Area 2, parity tracker Area 2 row
// "Deadline-failure sub-budget ending combo with clean 504").
//
// the reference router's evidence is comboPredicates.ts:189-226 (readiness
// deadline failures counted against the combo) and
// executeTargetAttempt.ts:1344-1438 (the combo ends when the
// readiness/deadline budget is exhausted). Ours had the whole
// per-combo DeadlineBudget → clean 504, but no readiness-deadline
// failure counting: one slow target could burn the whole request
// budget on repeated per-target timeouts before the combo gave up.
//
// The sub-budget counts readiness-deadline failures — an attempt
// that died on its own per-target timeout (perTargetTimeoutError,
// pertarget.go) or on a deadline while the request itself was still
// alive — and ends the combo with a clean 504
// (ErrDeadlineExceeded) once the count reaches
// Config.DeadlineFailureMax (default 3, i.e. three slow targets in
// one request is enough to stop burning the budget). The counter is
// per-request state in the chat loop, never engine state, so
// concurrent requests cannot pollute each other.
//
// Deliberate scope: counting lives in the sequential (non-hedged)
// chat path. A hedged first attempt that times out is not counted:
// the hedge fires once at the head of the request (at most two
// deadline failures), so it cannot exhaust a 3-budget on its own,
// but it also does not contribute — a request that hedges and then
// fails sequentially can undercount by up to two. Wiring the hedge
// branch into the same counter is a follow-up, not a gap in the
// sequential rule. The stream loop is likewise a follow-up: its
// attempts are bounded by preread + the throughput watchdog, not
// by per-target timeouts, so the readiness signal there is a
// different shape.
package router

import (
	"context"
	"errors"
)

// DefaultDeadlineFailureMax is the default readiness-deadline
// failure sub-budget: this many per-target/readiness deadline
// failures in one request end the combo with a clean 504.
const DefaultDeadlineFailureMax = 3

// isReadinessDeadlineFailure reports whether err is a readiness /
// per-target deadline failure: the attempt's own timeout fired
// (perTargetTimeoutError) or a deadline fired while the request
// context was still alive. A request-level expiry is NOT counted
// here — the loop-top ctx check ends the request on that, and the
// existing DeadlineBudget mapping already turns it into a 504.
func isReadinessDeadlineFailure(err error) bool {
	if err == nil {
		return false
	}
	var pte *perTargetTimeoutError
	if errors.As(err, &pte) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// deadlineSubBudgetHit counts one failure against the request's
// readiness-deadline sub-budget and reports whether the sub-budget
// is now exhausted. Non-deadline failures never touch the count.
// A non-positive max disables the sub-budget (negative in config);
// the default (3) is applied by withDefaults, so a zero max never
// reaches here.
func (e *Engine) deadlineSubBudgetHit(err error, failures *int) bool {
	max := e.cfg.DeadlineFailureMax
	if max <= 0 {
		return false
	}
	if !isReadinessDeadlineFailure(err) {
		return false
	}
	*failures++
	return *failures >= max
}
