package adaptive

// P2-CAP-002: Process-wide admission cap — optional global semaphore
// bounding aggregate in-flight requests regardless of provider/key count.
// Fail-open when unset (nil).

import (
	"sync"
	"testing"
)

func TestGlobalSemaphoreTryAcquireRelease(t *testing.T) {
	s := newGlobalSemaphore(2)

	// Two acquisitions should succeed
	if !s.TryAcquire() {
		t.Error("first TryAcquire should succeed")
	}
	if !s.TryAcquire() {
		t.Error("second TryAcquire should succeed")
	}
	if s.InFlight() != 2 {
		t.Errorf("InFlight = %d, want 2", s.InFlight())
	}

	// Third should fail (cap reached)
	if s.TryAcquire() {
		t.Error("third TryAcquire should fail (cap=2)")
	}

	// Release one, then acquire should succeed
	s.Release()
	if s.InFlight() != 1 {
		t.Errorf("InFlight after release = %d, want 1", s.InFlight())
	}
	if !s.TryAcquire() {
		t.Error("TryAcquire after release should succeed")
	}
	if s.InFlight() != 2 {
		t.Errorf("InFlight after re-acquire = %d, want 2", s.InFlight())
	}

	// Release all
	s.Release()
	s.Release()
	if s.InFlight() != 0 {
		t.Errorf("InFlight after all releases = %d, want 0", s.InFlight())
	}

	// Double-release is a no-op (guarded)
	s.Release()
	if s.InFlight() != 0 {
		t.Errorf("InFlight after double-release = %d, want 0", s.InFlight())
	}
}

func TestGlobalTryAcquireRelease(t *testing.T) {
	// Unset (nil) global limiter admits everything
	if !GlobalTryAcquire() {
		t.Error("GlobalTryAcquire with nil limiter should admit")
	}
	GlobalRelease()
	if GlobalInFlight() != 0 {
		t.Errorf("GlobalInFlight with nil limiter = %d, want 0", GlobalInFlight())
	}

	// Set a capacity of 1
	SetGlobalLimiter(1)
	if GlobalInFlight() != 0 {
		t.Errorf("GlobalInFlight after SetGlobalLimiter(1) = %d, want 0", GlobalInFlight())
	}
	if !GlobalTryAcquire() {
		t.Error("first GlobalTryAcquire should succeed")
	}
	if GlobalInFlight() != 1 {
		t.Errorf("GlobalInFlight after acquire = %d, want 1", GlobalInFlight())
	}
	if GlobalTryAcquire() {
		t.Error("second GlobalTryAcquire should fail (cap=1)")
	}
	GlobalRelease()
	if GlobalInFlight() != 0 {
		t.Errorf("GlobalInFlight after GlobalRelease = %d, want 0", GlobalInFlight())
	}

	// Disable again
	SetGlobalLimiter(0)
	if !GlobalTryAcquire() {
		t.Error("GlobalTryAcquire after disable should admit")
	}
	GlobalRelease()
}

// TestGlobalSemaphoreConcurrency verifies that at most 'cap' goroutines
// can hold the semaphore simultaneously.
func TestGlobalSemaphoreConcurrency(t *testing.T) {
	const cap = 3
	s := newGlobalSemaphore(cap)

	var (
		mu      sync.Mutex
		maxSeen int
		current int
		wg      sync.WaitGroup
	)

	for i := 0; i < cap*3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !s.TryAcquire() {
				return
			}
			defer s.Release()

			mu.Lock()
			current++
			if current > maxSeen {
				maxSeen = current
			}
			current--
			mu.Unlock()
		}()
	}
	wg.Wait()

	if maxSeen > cap {
		t.Errorf("max concurrent acquires = %d, want <= %d (cap)", maxSeen, cap)
	}
}
