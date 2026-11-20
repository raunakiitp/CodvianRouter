package tray

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPIDFilePaths(t *testing.T) {
	// Expectations are built with filepath.Join: on Windows the
	// separator is a backslash, so "/" literals never match.
	cfg := filepath.Join("/home/u", ".config", "codvian-router", "config.json")
	wantPID := filepath.Join("/home/u", ".config", "codvian-router", "router.pid")
	wantLog := filepath.Join("/home/u", ".config", "codvian-router", "router.log")
	if got := PIDFilePath(""); got != "" {
		t.Fatalf("PIDFilePath(\"\") = %q, want empty", got)
	}
	if got := PIDFilePath(cfg); got != wantPID {
		t.Fatalf("PIDFilePath = %q, want %q", got, wantPID)
	}
	if got := LogFilePath(""); got != "" {
		t.Fatalf("LogFilePath(\"\") = %q, want empty", got)
	}
	if got := LogFilePath(cfg); got != wantLog {
		t.Fatalf("LogFilePath = %q, want %q", got, wantLog)
	}
}

// TestSupervisorPIDFileLifecycle proves Start writes the PID file
// (supervisor pid + gateway pid + port) and Stop removes it. The
// supervisor under test spawns a fake child (sleep) but carries a
// real config path, set directly: NewWithCommand has no config
// file, and the PID file lives next to the config file.
func TestSupervisorPIDFileLifecycle(t *testing.T) {
	dir := t.TempDir()
	name, args := sleepCommand()
	sup := NewWithCommand(name, args...)
	sup.configPath = filepath.Join(dir, "config.json")
	pidPath := PIDFilePath(sup.configPath)

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("PID file %s missing after Start: %v", pidPath, err)
	}
	var rec struct {
		SupervisorPID int `json:"supervisor_pid"`
		GatewayPID    int `json:"gateway_pid"`
		Port          int `json:"port"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("PID file %s is not valid JSON: %v", pidPath, err)
	}
	if rec.SupervisorPID != os.Getpid() {
		t.Fatalf("supervisor_pid = %d, want %d (this test process)", rec.SupervisorPID, os.Getpid())
	}
	if rec.GatewayPID != sup.PID() || rec.GatewayPID <= 0 {
		t.Fatalf("gateway_pid = %d, want supervisor PID() = %d", rec.GatewayPID, sup.PID())
	}

	if err := sup.Stop(); err != nil {
		t.Fatalf("Stop() error: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file %s still exists after Stop (err = %v)", pidPath, err)
	}
}

// TestSupervisorPIDFileRefreshedOnAutoRestart proves the PID file
// tracks the child across an automatic restart: kill the child,
// wait for the supervisor to restart it, and the recorded gateway
// pid must be the new child's pid.
func TestSupervisorPIDFileRefreshedOnAutoRestart(t *testing.T) {
	dir := t.TempDir()
	name, args := sleepCommand()
	sup := NewWithCommand(name, args...)
	sup.configPath = filepath.Join(dir, "config.json")
	pidPath := PIDFilePath(sup.configPath)
	defer func() { _ = sup.Stop() }()

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	waitFor(t, 2*time.Second, "status running", func() bool { return sup.Status() == "running" })
	oldPID := sup.PID()
	if oldPID <= 0 {
		t.Fatalf("PID() = %d, want > 0", oldPID)
	}
	// Kill the child directly: the supervisor must restart it once.
	proc, err := os.FindProcess(oldPID)
	if err != nil {
		t.Fatalf("FindProcess(%d): %v", oldPID, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("Kill(%d): %v", oldPID, err)
	}
	waitFor(t, 5*time.Second, "auto-restart with a new pid", func() bool {
		return sup.Status() == "running" && sup.PID() != oldPID && sup.PID() > 0
	})
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("PID file %s missing after auto-restart: %v", pidPath, err)
	}
	var rec struct {
		GatewayPID int `json:"gateway_pid"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("PID file %s is not valid JSON: %v", pidPath, err)
	}
	if rec.GatewayPID != sup.PID() {
		t.Fatalf("PID file gateway_pid = %d, want new child pid %d", rec.GatewayPID, sup.PID())
	}
}

// TestSupervisorPIDFileRemovedOnUnexpectedExit proves the PID file
// does not outlive a supervisor whose child died unexpectedly and
// exhausted its restart budget.
func TestSupervisorPIDFileRemovedOnUnexpectedExit(t *testing.T) {
	dir := t.TempDir()
	name, args := failCommand()
	sup := NewWithCommand(name, args...)
	sup.configPath = filepath.Join(dir, "config.json")
	pidPath := PIDFilePath(sup.configPath)

	if err := sup.Start(context.Background()); err != nil {
		t.Fatalf("Start() error: %v", err)
	}
	// The child exits at once, is restarted once after a 1s
	// backoff, exits again, and the supervisor stops for good.
	waitFor(t, 10*time.Second, "status stopped", func() bool { return sup.Status() == "stopped" })
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("PID file %s still exists after the child died unexpectedly (err = %v)", pidPath, err)
	}
}
