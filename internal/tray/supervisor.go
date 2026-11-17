// Package tray supervises the Codvian Router gateway as a child process
// and (with the tray build tag) exposes it through a system-tray icon.
//
// The supervisor is backend only: it never shows, logs, or transmits any
// secret (gateway keys, provider keys). It only reports the gateway's
// lifecycle state: "running", "stopped", or "unknown" (the child died
// unexpectedly and a restart is pending, or its state cannot be
// determined).
//
// Build tags: the Supervisor itself is tag-free and works in every build.
// The tray icon lives in tray_gui.go (//go:build tray) and the blocking
// headless runner in headless.go (//go:build !tray). See README.md.
package tray

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Restart policy constants.
const (
	// maxAutoRestarts is the number of times the supervisor restarts the
	// child on its own after an unexpected exit, per Start call.
	maxAutoRestarts = 1
	// restartBackoff is the delay before an automatic restart attempt.
	restartBackoff = time.Second
	// stopGrace is how long Stop waits for a graceful SIGTERM exit before
	// escalating to SIGKILL on the whole process group.
	stopGrace = 2 * time.Second
)

// Supervisor keeps the gateway binary alive as a child process.
//
// The zero value is not usable; use New or NewWithCommand.
type Supervisor struct {
	mu sync.Mutex

	configPath string // config file passed to the child via --config
	port       int    // passed via --port when > 0; 0 = port from config
	command    []string

	cmd          *exec.Cmd
	state        string // "running" | "stopped" | "unknown"
	stopping     bool
	autoRestarts int
	lastErr      error

	// doneCh is closed when the monitor goroutine for the current run
	// exits terminally (clean stop, or retries exhausted). It is
	// recreated on every Start.
	doneCh chan struct{}
	// stopReq is closed by Stop to abort a restart backoff. It is
	// recreated on every Start.
	stopReq chan struct{}
}

// New returns a Supervisor for the gateway whose config file is
// <dataDir>/config.json, i.e. dataDir is the config directory (by default
// ~/.config/codvian-router). When dataDir is empty, the default config
// path ~/.config/codvian-router/config.json is used. When port is > 0 it
// is passed to the child as --port; when 0 the port comes from the config
// file.
//
// The child is the codvian-router binary itself, resolved via
// os.Executable, invoked as:
//
//	codvian-router [--port PORT] --config <dataDir>/config.json serve
//
// Choice note: New takes a data directory (not a ConfigPath string)
// because that matches how the rest of the router talks about its config
// location, and the config file name is fixed. NewWithConfig exists for
// callers that already hold an explicit config file path.
func New(dataDir string, port int) *Supervisor {
	if dataDir == "" {
		cfgDir, err := os.UserConfigDir()
		if err == nil {
			dataDir = filepath.Join(cfgDir, "codvian-router")
		}
	}
	return NewWithConfig(filepath.Join(dataDir, "config.json"), port)
}

// NewWithConfig is New for callers that already hold an explicit config
// file path instead of a config directory.
func NewWithConfig(configPath string, port int) *Supervisor {
	return &Supervisor{configPath: configPath, port: port, state: "stopped"}
}

// NewWithCommand returns a Supervisor that spawns the given command
// instead of the gateway binary. Test-only: it exists so tests can use a
// fake child (e.g. "sleep") without a built gateway binary. Do not use it
// in production code.
func NewWithCommand(name string, args ...string) *Supervisor {
	return &Supervisor{
		command: append([]string{name}, args...),
		state:   "stopped",
	}
}

// ConfigPath returns the config file path passed to the child via --config
// (empty for a NewWithCommand supervisor).
func (s *Supervisor) ConfigPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configPath
}

// Status reports the gateway state: "running", "stopped", or "unknown".
// "unknown" means the child exited unexpectedly and an automatic restart
// is pending (or its state could not be determined).
func (s *Supervisor) Status() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// PID returns the child process id, or 0 when no child is running.
func (s *Supervisor) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// LastError returns the error from the most recent unexpected child exit,
// or nil when the child has never died unexpectedly in the current run.
func (s *Supervisor) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// AutoRestarts returns how many automatic restarts have been attempted in
// the current run (at most 1; see the restart policy in README.md).
func (s *Supervisor) AutoRestarts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.autoRestarts
}

// Start launches the gateway child. It is idempotent: starting an
// already-running supervisor is a no-op returning nil. The child is
// stopped when ctx ends.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.state == "running" || s.state == "unknown" {
		s.mu.Unlock()
		return nil
	}
	cmd, err := s.buildCmd()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if err := cmd.Start(); err != nil {
		s.state = "stopped"
		s.lastErr = err
		s.mu.Unlock()
		return fmt.Errorf("tray: failed to start gateway: %w", err)
	}
	s.cmd = cmd
	s.state = "running"
	s.stopping = false
	s.autoRestarts = 0
	s.lastErr = nil
	s.doneCh = make(chan struct{})
	s.stopReq = make(chan struct{})
	s.mu.Unlock()

	go s.monitor()
	if ctx != nil && ctx.Done() != nil {
		go func() {
			<-ctx.Done()
			_ = s.Stop()
		}()
	}
	return nil
}

// Stop terminates the child process group (SIGTERM, then SIGKILL after
// stopGrace) and waits for the monitor goroutine to reap the child, so no
// zombie process is left behind. It is idempotent.
func (s *Supervisor) Stop() error {
	s.mu.Lock()
	if s.state == "stopped" {
		s.mu.Unlock()
		return nil
	}
	s.stopping = true
	if s.stopReq != nil {
		close(s.stopReq)
		s.stopReq = nil
	}
	cmd := s.cmd
	doneCh := s.doneCh
	s.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		// Kill the whole process group so grandchildren (e.g. helpers the
		// gateway may spawn) do not outlive the gateway.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-doneCh:
			return nil
		case <-time.After(stopGrace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if doneCh != nil {
		<-doneCh
	}
	return nil
}

// Restart stops the current child (if any) and starts a fresh one. The
// automatic-restart budget resets.
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(); err != nil {
		return err
	}
	return s.Start(ctx)
}

// buildCmd constructs the child command. Callers must hold s.mu only for
// reading config fields; it does not lock.
func (s *Supervisor) buildCmd() (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if len(s.command) > 0 {
		cmd = exec.Command(s.command[0], s.command[1:]...)
	} else {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("tray: cannot resolve gateway binary: %w", err)
		}
		args := []string{}
		if s.port > 0 {
			args = append(args, "--port", strconv.Itoa(s.port))
		}
		args = append(args, "--config", s.configPath, "serve")
		cmd = exec.Command(exe, args...)
	}
	// Own process group so Stop can signal the whole group, and so the
	// child never shares this process's group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	return cmd, nil
}

// monitor waits on the child, reaping it (no zombie), and implements the
// restart policy: on an unexpected exit it waits restartBackoff and
// restarts the child once; if that child also dies unexpectedly (or the
// restart itself fails) the state becomes "stopped" with LastError set.
func (s *Supervisor) monitor() {
	for {
		s.mu.Lock()
		cmd := s.cmd
		s.mu.Unlock()
		if cmd == nil {
			s.finishStopped(nil)
			return
		}
		err := cmd.Wait() // reaps the child; no zombie remains
		s.mu.Lock()
		if s.stopping {
			s.state = "stopped"
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			return
		}
		// Unexpected exit.
		s.lastErr = err
		if s.autoRestarts >= maxAutoRestarts {
			s.state = "stopped"
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			return
		}
		s.autoRestarts++
		s.state = "unknown"
		stopReq := s.stopReq
		s.mu.Unlock()

		timer := time.NewTimer(restartBackoff)
		select {
		case <-timer.C:
		case <-stopReq:
			timer.Stop()
			s.finishStopped(nil)
			return
		}

		s.mu.Lock()
		if s.stopping {
			s.state = "stopped"
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			return
		}
		next, buildErr := s.buildCmd()
		if buildErr != nil {
			s.state = "stopped"
			s.lastErr = buildErr
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			return
		}
		if startErr := next.Start(); startErr != nil {
			s.state = "stopped"
			s.lastErr = fmt.Errorf("tray: automatic restart failed: %w", startErr)
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			return
		}
		s.cmd = next
		s.state = "running"
		s.mu.Unlock()
		// Loop back to Wait on the new child.
	}
}

// finishStopped marks the supervisor stopped and closes the run's done
// channel. Used when the monitor exits outside the Wait path (e.g. Stop
// during a restart backoff).
func (s *Supervisor) finishStopped(err error) {
	s.mu.Lock()
	s.state = "stopped"
	if err != nil {
		s.lastErr = err
	}
	s.cmd = nil
	doneCh := s.doneCh
	s.mu.Unlock()
	if doneCh != nil {
		// doneCh is closed exactly once per run: monitor is the only
		// closer and it returns right after closing.
		close(doneCh)
	}
}

// doneChan returns the done channel for the current run (nil when never
// started). Used by RunHeadless.
func (s *Supervisor) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneCh
}
