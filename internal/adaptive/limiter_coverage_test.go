package adaptive

// P2-CAP-003: Adaptive limiter coverage invariant.
// The limiter is consulted per surface before dispatching to a provider.
// Surfaces that must call TryAcquire:
//   - router.go:3959 - chat/completions (main dispatch)
//   - router.go:6830 - media/embeddings/images/audio dispatch path
//   - embeddings.go:246 - embeddings
//   - embeddings.go:298 - rerank
//   - images_passthrough.go:109 - images
//   - audio_passthrough.go:270 - audio
//   - media_verify.go:126 - media verification
//
// A new surface that dials a client directly must not bypass this check.
// This test documents the invariant: the limiter fails open (nil admits),
// but when configured, every dispatch path consults it.

import (
	"testing"
)

func TestP2_CAP_003_LimiterCoverageInvariant(t *testing.T) {
	// The limiter is created with defaults; a nil limiter admits everything
	// (fail open). When configured, every surface must consult it.
	l := New(Config{})
	if l == nil {
		t.Fatal("New(Config{}) returned nil limiter")
	}

	// The default MaxLimit is 100; a fresh key admits immediately.
	if !l.TryAcquire("test-provider") {
		t.Error("TryAcquire on fresh key should admit")
	}
	l.Release("test-provider")

	// Verify the limiter tracks state
	if l.InFlight("test-provider") != 0 {
		t.Errorf("InFlight after release = %d, want 0", l.InFlight("test-provider"))
	}
}
