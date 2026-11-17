package tray

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitFor polls cond until it is true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewConfigPath(t *testing.T) {
	sup := New("/tmp/codvian-router-test", 8080)
	if got := sup.ConfigPath(); got != "/tmp/codvian-router-test/config.json" {
		t.Fatalf("ConfigPath() = %q, want %q", got, "/tmp/codvian-router-test/config.json")
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("initial Status() = %q, want stopped", got)
	}
	if got := sup.PID(); got != 0 {
		t.Fatalf("initial PID() = %d, want 0", got)
	}
}

func TestStatusTransitions(t *testing.T) {
	ctx := context.Background()
	sup := NewWithCommand("sleep", "30")
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("initial Status() = %q, want stopped", got)
	}
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })
	if pid := sup.PID(); pid <= 0 {
		t.Fatalf("PID() = %d, want > 0 while running", pid)
	}
	// Second Start is a no-op and keeps the same child.
	pid1 := sup.PID()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("second Start() error: %v", err)
	}
	if pid := sup.PID(); pid != pid1 {
		t.Fatalf("second Start() replaced child: PID %d -> %d", pid1, pid)
	}
	if err := sup.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status() after Stop = %q, want stopped", got)
	}
	if pid := sup.PID(); pid != 0 {
		t.Fatalf("PID() after Stop = %d, want 0", pid)
	}
	// Second Stop is a no-op.
	if err := sup.Stop(); err != nil {
		t.Fatalf("second Stop() error: %v", err)
	}
}

func TestRestartReplacesProcess(t *testing.T) {
	ctx := context.Background()
	sup := NewWithCommand("sleep", "30")
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	defer func() { _ = sup.Stop() }()
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })
	pid1 := sup.PID()
	if err := sup.Restart(ctx); err != nil {
		t.Fatalf("Restart() error: %v", err)
	}
	waitFor(t, 2*time.Second, "status running after restart", func() bool { return sup.Status() == "running" })
	pid2 := sup.PID()
	if pid2 == 0 || pid2 == pid1 {
		t.Fatalf("Restart() did not replace process: PID %d -> %d", pid1, pid2)
	}
	// The old child must be gone (no leaked process).
	if err := syscall.Kill(pid1, 0); err == nil {
		t.Fatalf("old child PID %d still exists after Restart", pid1)
	}
}

func TestUnexpectedExitRestartsOnce(t *testing.T) {
	ctx := context.Background()
	// "false" exits immediately with status 1: an unexpected exit.
	sup := NewWithCommand("false")
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	// One automatic restart must be attempted (after the 1s backoff).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sup.AutoRestarts() < 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.AutoRestarts(); got != 1 {
		t.Fatalf("AutoRestarts() = %d, want exactly 1", got)
	}
	// The restarted child also exits immediately, so the supervisor must
	// end up stopped with the exit recorded — and must not restart again.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sup.Status() != "stopped" {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status() after retries exhausted = %q, want stopped", got)
	}
	if got := sup.AutoRestarts(); got != 1 {
		t.Fatalf("AutoRestarts() after exhaustion = %d, want exactly 1", got)
	}
	if err := sup.LastError(); err == nil {
		t.Fatal("LastError() = nil, want the unexpected exit recorded")
	} else if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("LastError() = %v, want it to name exit status 1", err)
	}
	if pid := sup.PID(); pid != 0 {
		t.Fatalf("PID() after exhaustion = %d, want 0", pid)
	}
}

func TestStopKillsProcessGroup(t *testing.T) {
	ctx := context.Background()
	// sh spawns a sleep grandchild in the same process group; Stop must
	// kill the group, not just sh.
	sup := NewWithCommand("sh", "-c", "sleep 30")
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })
	pid := sup.PID()
	if pid <= 0 {
		t.Fatalf("PID() = %d, want > 0", pid)
	}
	if err := sup.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	// No zombie / lingering process: the pid must no longer exist.
	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("child PID %d still exists after Stop (leaked process)", pid)
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status() after Stop = %q, want stopped", got)
	}
}

func TestRunHeadlessChildDiesUnexpectedly(t *testing.T) {
	ctx := context.Background()
	sup := NewWithCommand("false")
	err := RunHeadless(ctx, sup)
	if err == nil {
		t.Fatal("RunHeadless() = nil, want an error naming the unexpected exit")
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("RunHeadless() error = %v, want it to name exit status 1", err)
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status() after RunHeadless = %q, want stopped", got)
	}
}

func TestRunHeadlessContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sup := NewWithCommand("sleep", "30")
	done := make(chan error, 1)
	go func() { done <- RunHeadless(ctx, sup) }()
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("RunHeadless() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunHeadless() did not return after ctx cancel")
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status() after ctx cancel = %q, want stopped", got)
	}
	if err := syscall.Kill(sup.PID(), 0); sup.PID() != 0 && err == nil {
		t.Fatal("child still alive after ctx cancel")
	}
}
