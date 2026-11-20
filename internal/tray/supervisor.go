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
// headless runner in headless.go (//go:build !tray). The
// process-group primitives are split by platform —
// supervisor_unix.go (Setpgid + SIGTERM/SIGKILL) and
// supervisor_windows.go (CREATE_NEW_PROCESS_GROUP +
// CTRL_BREAK_EVENT/Process.Kill) — because Windows has no POSIX
// signals or Setpgid; see README.md.
package tray

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Restart policy constants.
const (
	// maxAutoRestarts is the number of times the supervisor restarts the
	// child on its own after an unexpected exit, per Start call.
	maxAutoRestarts = 1
	// restartBackoff is the delay before an automatic restart attempt.
	restartBackoff = time.Second
	// stopGrace is how long Stop waits for a graceful exit before
	// escalating to a force-kill (SIGTERM then SIGKILL to the whole
	// process group on Unix; CTRL_BREAK_EVENT to the group then
	// Process.Kill on Windows — see stopChildProcessGroup in
	// supervisor_unix.go / supervisor_windows.go).
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

// PIDFilePath returns the path of the PID file for a gateway supervised
// with this config file: <config-dir>/router.pid. The file is written
// by the supervisor on Start (and refreshed when an automatic restart
// replaces the child) and removed on Stop, so the `status`/`stop`/
// `restart` CLI commands can find the supervised gateway without
// asking the supervisor process. Empty configPath means "no PID file"
// (NewWithCommand supervisors, used by tests, have no config file).
func PIDFilePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "router.pid")
}

// LogFilePath returns the path of the gateway log file for a gateway
// supervised with this config file: <config-dir>/router.log. The file
// itself is only written when the gateway is started through the
// lifecycle commands (`restart`/`update` redirect the detached
// supervisor's stdout/stderr there, and the gateway child inherits
// them); a gateway started by hand in a terminal logs to that
// terminal instead. Empty configPath means "no log file".
func LogFilePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), "router.log")
}

// pidFile is the on-disk record the supervisor writes to its PID
// file: the supervisor's own pid (so `stop` can signal the
// supervisor, which then stops the child gracefully), the gateway
// child's pid, and the port the child was started with.
type pidFile struct {
	SupervisorPID int `json:"supervisor_pid"`
	GatewayPID    int `json:"gateway_pid"`
	Port          int `json:"port"`
}

// pidFilePath is PIDFilePath for this supervisor's config file.
func (s *Supervisor) pidFilePath() string {
	return PIDFilePath(s.configPath)
}

// writePIDFile records the running gateway child in the PID file
// (atomic temp-file + rename, 0600 — pids are not secrets, but the
// config directory is the operator's private state and nothing else
// needs to read it). It creates the config directory when missing.
// A failure is reported to the caller: Start keeps the gateway
// running and only warns, because a running gateway is more useful
// than a refused start, and the CLI reports the missing PID file
// honestly when it looks for it.
func (s *Supervisor) writePIDFile(gatewayPID int) error {
	path := s.pidFilePath()
	if path == "" {
		return nil
	}
	rec := pidFile{
		SupervisorPID: os.Getpid(),
		GatewayPID:    gatewayPID,
		Port:          s.port,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("tray: cannot encode PID file: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("tray: cannot create PID file directory %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".router-pid-*.tmp")
	if err != nil {
		return fmt.Errorf("tray: cannot create PID file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("tray: cannot write PID file: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("tray: cannot set PID file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("tray: cannot write PID file: %w", err)
	}
	// The rename replaces the previous PID file. On Windows it
	// can fail transiently with "Access is denied" when a
	// scanner (Defender/indexer) briefly holds the existing file
	// open, so retry a few times before giving up — a failed
	// refresh leaves `status`/`stop` pointing at a dead pid.
	if err := renamePIDFile(tmpName, path); err != nil {
		return fmt.Errorf("tray: cannot write PID file: %w", err)
	}
	return nil
}

// renamePIDFile moves tmp over dst, retrying briefly: on Windows a
// freshly-written file can be held open for a moment by a scanner,
// and the first rename then fails with "Access is denied" even
// though nothing is wrong. The retry is harmless on Unix, where
// the first rename succeeds.
func renamePIDFile(tmp, dst string) error {
	var err error
	for i := 0; i < 5; i++ {
		if err = os.Rename(tmp, dst); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return err
}

// removePIDFile deletes the PID file, ignoring a file that is
// already gone. Called when the supervised gateway stops for good,
// so a later `status` does not report a dead pid as running.
func (s *Supervisor) removePIDFile() {
	path := s.pidFilePath()
	if path == "" {
		return
	}
	_ = os.Remove(path)
}
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

	// Record the child in the PID file before publishing the
	// running state: Status()/PID() become visible the moment
	// the lock is released, so a reader (the lifecycle CLI, or a
	// test) must never observe a new pid while the file still
	// holds the old one. A failure here does not stop the
	// gateway — it is already running — but it is reported, so
	// the operator knows `status` will not see this gateway.
	if err := s.writePIDFile(cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: tray: %v\n", err)
	}
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

// Stop terminates the child process group (graceful signal, then a
// force-kill after stopGrace — SIGTERM then SIGKILL on Unix,
// CTRL_BREAK_EVENT then Process.Kill on Windows; see
// stopChildProcessGroup in supervisor_unix.go /
// supervisor_windows.go) and waits for the monitor goroutine to
// reap the child, so no zombie process is left behind. It is
// idempotent.
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
		stopChildProcessGroup(cmd, doneCh)
	}
	if doneCh != nil {
		<-doneCh
	}
	s.removePIDFile()
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
	// child never shares this process's group. The platform split
	// lives in supervisor_unix.go (Setpgid) and
	// supervisor_windows.go (CREATE_NEW_PROCESS_GROUP).
	configureChildProcessGroup(cmd)
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
			s.removePIDFile()
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
			s.removePIDFile()
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
			s.removePIDFile()
			return
		}
		if startErr := next.Start(); startErr != nil {
			s.state = "stopped"
			s.lastErr = fmt.Errorf("tray: automatic restart failed: %w", startErr)
			s.cmd = nil
			doneCh := s.doneCh
			s.mu.Unlock()
			close(doneCh)
			s.removePIDFile()
			return
		}
		s.cmd = next
		s.state = "running"
		// The child pid changed: refresh the PID file before
		// publishing the running state (see Start), so a reader
		// never observes the new pid while the file still holds
		// the old one.
		if err := s.writePIDFile(next.Process.Pid); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: tray: %v\n", err)
		}
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
	s.removePIDFile()
}

// doneChan returns the done channel for the current run (nil when never
// started). Used by RunHeadless.
func (s *Supervisor) doneChan() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.doneCh
}
