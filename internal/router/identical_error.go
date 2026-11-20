// Identical-error early stop (M4.7d, the reference router comboPredicates.ts
// IDENTICAL_MODEL_ERROR_STREAK / hasIdenticalModelErrorStreak).
//
// A malformed/unsupported request shape (e.g. a model no provider
// has) fails the SAME way against every fallback target, since it
// is a property of the request, not of any one provider. Once 3
// *consecutive* targets have failed with the identical model-shape
// error — same kind (model), same status (400), same error string
// — retrying the remaining fallbacks cannot succeed either; it
// only burns attempts and wall-clock time. The chain stops early.
//
// The signature is exact, like the reference router's (their check is
// `e.error === first.error`): status 400, taxonomy reason
// model_not_found, and the error string verbatim. Other 400s never
// reach this check — the taxonomy already stops the chain on the
// first non-model-not-found 400 (DecisionStop) — and a different
// error string, or any non-400 failure in between, resets the
// streak. The stop preserves lastErr, so the client still sees the
// real provider error, not a synthetic one.
package router

import (
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// identicalModelErrorStreak is how many consecutive identical
// model-shape 400s stop the chain (the reference router: 3).
const identicalModelErrorStreak = 3

// identicalModelErrorSignature returns the streak signature of err
// when it is a model-shape 400 (kind model, status 400), and
// false otherwise.
func identicalModelErrorSignature(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	cls := provider.ClassifyError(err, provider.EndpointChatCompletions)
	if cls.Status != 400 || cls.Reason != provider.ReasonModelNotFound {
		return "", false
	}
	return fmt.Sprintf("model|400|%s", err.Error()), true
}

// identicalErrorTracker is the per-request streak state. One
// tracker per attempt loop (Chat and stream each have their own).
type identicalErrorTracker struct {
	sig    string
	streak int
}

// note records one attempt error and reports whether the streak
// reached identicalModelErrorStreak — the chain must stop.
func (t *identicalErrorTracker) note(err error) bool {
	sig, ok := identicalModelErrorSignature(err)
	if !ok {
		t.sig = ""
		t.streak = 0
		return false
	}
	if sig == t.sig {
		t.streak++
	} else {
		t.sig = sig
		t.streak = 1
	}
	return t.streak >= identicalModelErrorStreak
}
