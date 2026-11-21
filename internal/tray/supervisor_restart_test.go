package tray

import (
	"context"
	"testing"
	"time"
)

// TestRequestedRestartRelaunches proves a child exiting with
// RestartExitCode after running a while is relaunched at once, with
// a new pid, without spending the crash-restart budget.
func TestRequestedRestartRelaunches(t *testing.T) {
	name, args := restartRequestCommand()
	sup := NewWithCommand(name, args...)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()
	pid1 := sup.PID()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && sup.RequestedRestarts() < 1 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.RequestedRestarts(); got < 1 {
		t.Fatalf("RequestedRestarts = %d, want >= 1", got)
	}
	if got := sup.AutoRestarts(); got != 0 {
		t.Errorf("AutoRestarts = %d, want 0 (a requested restart is not a crash)", got)
	}
	if pid2 := sup.PID(); pid2 == 0 || pid2 == pid1 {
		t.Errorf("pid after restart = %d (before %d), want a new running child", pid2, pid1)
	}
	if err := sup.LastError(); err != nil {
		t.Errorf("LastError = %v, want nil after a requested restart", err)
	}
}

// TestImmediateRestartCodeIsACrash proves a child that exits with
// RestartExitCode at once is handled by the crash policy (one retry,
// then stopped), so a broken binary cannot loop forever.
func TestImmediateRestartCodeIsACrash(t *testing.T) {
	name, args := fastRestartCodeCommand()
	sup := NewWithCommand(name, args...)
	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sup.Stop()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && sup.Status() != "stopped" {
		time.Sleep(50 * time.Millisecond)
	}
	if got := sup.Status(); got != "stopped" {
		t.Fatalf("Status = %q, want stopped", got)
	}
	if got := sup.RequestedRestarts(); got != 0 {
		t.Errorf("RequestedRestarts = %d, want 0", got)
	}
	if got := sup.AutoRestarts(); got != 1 {
		t.Errorf("AutoRestarts = %d, want 1", got)
	}
}

// TestChildGetsSupervisedEnv proves buildCmd marks the child, which
// is how the gateway knows to hand a restart back to the supervisor.
func TestChildGetsSupervisedEnv(t *testing.T) {
	sup := NewWithCommand("x")
	cmd, err := sup.buildCmd()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range cmd.Env {
		if e == SupervisedEnv+"=1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("child env lacks %s=1", SupervisedEnv)
	}
}
