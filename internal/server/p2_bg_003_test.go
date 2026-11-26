package server

// P2-BG-003: background worker registry shutdown safety tests.

import (
	"context"
	"io"
	"log"
	"sync"
	"testing"
)

// TestBackgroundWorkerRegistry verifies that RegisterBackgroundWorker and
// StopAllBackgroundWorkers correctly register and cancel all workers.
func TestBackgroundWorkerRegistry(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	// Initially no workers
	if len(s.bgWorkers) != 0 {
		s.bgWorkers = make(map[string]context.CancelFunc)
	}

	// Register two workers with cancel functions that track whether they were called
	called1 := false
	called2 := false
	cancel1 := func() { called1 = true }
	cancel2 := func() { called2 = true }

	s.RegisterBackgroundWorker("worker-1", cancel1)
	s.RegisterBackgroundWorker("worker-2", cancel2)

	if len(s.bgWorkers) != 2 {
		t.Errorf("bgWorkers has %d entries, want 2", len(s.bgWorkers))
	}

	// Stop all workers
	s.StopAllBackgroundWorkers()

	if !called1 {
		t.Error("worker-1 cancel was not called")
	}
	if !called2 {
		t.Error("worker-2 cancel was not called")
	}
	if len(s.bgWorkers) != 0 {
		t.Errorf("bgWorkers has %d entries after stop, want 0", len(s.bgWorkers))
	}

	// Registering the same name twice replaces (doesn't leak the old cancel)
	called3 := false
	cancel3 := func() { called3 = true }
	s.RegisterBackgroundWorker("worker-1", cancel3)
	s.RegisterBackgroundWorker("worker-1", cancel1) // replaces cancel3

	s.StopAllBackgroundWorkers()

	if called3 {
		t.Error("old cancel3 was called even though it was replaced")
	}
	if !called1 {
		t.Error("worker-1 cancel (second registration) was not called")
	}
}

// TestBackgroundWorkerConcurrentRegister verifies thread-safe registration.
func TestBackgroundWorkerConcurrentRegister(t *testing.T) {
	s := &Server{logger: log.New(io.Discard, "", 0)}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.RegisterBackgroundWorker("worker-"+string(rune('0'+i)), func() {})
		}(i)
	}
	wg.Wait()

	if len(s.bgWorkers) != 10 {
		t.Errorf("bgWorkers has %d entries, want 10", len(s.bgWorkers))
	}

	s.StopAllBackgroundWorkers()
	if len(s.bgWorkers) != 0 {
		t.Errorf("bgWorkers has %d entries after stop, want 0", len(s.bgWorkers))
	}
}
