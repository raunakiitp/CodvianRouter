// Lifecycle subcommands: status, stop, restart, logs, health.
//
// These are the operator-facing half of the tray supervisor
// (internal/tray): the supervisor writes <config-dir>/router.pid on
// Start and removes it on Stop, and these commands read that file
// plus GET /healthz on the gateway. A gateway started directly with
// `serve` has no PID file — `status` still sees it through /healthz,
// but `stop`/`restart` refuse to act on a process they cannot prove
// is a supervised gateway (see verifyRouterProcess in
// lifecycle_unix.go / lifecycle_windows.go).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/tray"
)

// supervisorPIDFile is the JSON record the tray supervisor writes to
// <config-dir>/router.pid on Start (and refreshes when an automatic
// restart replaces the child) and removes on Stop.
type supervisorPIDFile struct {
	SupervisorPID int `json:"supervisor_pid"`
	GatewayPID    int `json:"gateway_pid"`
	Port          int `json:"port"`
}

func readSupervisorPIDFile(path string) (*supervisorPIDFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rec supervisorPIDFile
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("corrupt PID file %s: %w", path, err)
	}
	if rec.GatewayPID <= 0 {
		return nil, fmt.Errorf("corrupt PID file %s: no gateway pid recorded", path)
	}
	return &rec, nil
}

// resolveLifecyclePort returns the gateway port for the lifecycle
// commands: --port wins, then the config file's listen port, then
// the default 20128 (same resolution as runTray).
func resolveLifecyclePort(opts options, configPath string) int {
	if opts.port > 0 {
		return opts.port
	}
	if cfg, err := config.Load(configPath); err == nil {
		if p, err := cfg.ListenPort(); err == nil && p > 0 {
			return p
		}
	}
	return 20128
}

// healthProbe is the result of one GET /healthz attempt.
type healthProbe struct {
	ok     bool
	status int
	body   string
	rtt    time.Duration
	err    error
}

// probeHealthz GETs http://127.0.0.1:<port>/healthz with a 5s
// timeout. /healthz is auth-exempt, so no gateway key is needed.
func probeHealthz(port int) healthProbe {
	client := &http.Client{Timeout: 5 * time.Second}
	start := time.Now()
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return healthProbe{err: err}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	probe := healthProbe{
		status: resp.StatusCode,
		body:   strings.TrimSpace(string(body)),
		rtt:    time.Since(start),
	}
	if resp.StatusCode == http.StatusOK {
		probe.ok = true
	} else {
		probe.err = fmt.Errorf("HTTP %s", resp.Status)
	}
	return probe
}

func runStatus(opts options, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: status takes no arguments\n")
		return 2
	}
	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := resolveLifecyclePort(opts, configPath)
	pidPath := tray.PIDFilePath(configPath)
	probe := probeHealthz(port)

	running := false
	rec, err := readSupervisorPIDFile(pidPath)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "codvian-router: status: %v\n", err)
			return 1
		}
		fmt.Printf("gateway: pid file %s absent — no supervised gateway recorded\n", pidPath)
	} else if processAlive(rec.GatewayPID) {
		running = true
		fmt.Printf("gateway: running\n  pid: %d (supervisor %d)\n  port: %d\n", rec.GatewayPID, rec.SupervisorPID, rec.Port)
	} else if processAlive(rec.SupervisorPID) {
		// Supervisor alive but the child pid dead: the supervisor
		// restarts a dead child once after a 1s backoff, so this
		// is a transient window, not a stable state.
		fmt.Printf("gateway: restarting (supervisor %d alive, child pid %d dead — automatic restart pending)\n", rec.SupervisorPID, rec.GatewayPID)
	} else {
		// Both pids dead: the supervisor is gone without
		// removing its PID file (killed, crashed, or an older
		// build). Drop the stale record so the next status is
		// clean.
		_ = os.Remove(pidPath)
		fmt.Printf("gateway: not running\n  pid file: %s (stale — pids %d/%d dead, removed)\n", pidPath, rec.SupervisorPID, rec.GatewayPID)
	}

	if probe.ok {
		if !running {
			fmt.Printf("gateway: running (unmanaged — answers /healthz on port %d but has no supervisor PID file; it was probably started with `serve` directly)\n", port)
			running = true
		}
		fmt.Printf("  health: ok (GET /healthz -> 200 in %s, %s)\n", probe.rtt.Round(time.Millisecond), probe.body)
	} else {
		fmt.Printf("  health: unreachable (%v)\n", probe.err)
	}
	if running {
		return 0
	}
	return 1
}

func runHealth(opts options, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: health takes no arguments\n")
		return 2
	}
	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := resolveLifecyclePort(opts, configPath)
	probe := probeHealthz(port)
	if probe.ok {
		fmt.Printf("ok: GET http://127.0.0.1:%d/healthz -> 200 in %s (%s)\n", port, probe.rtt.Round(time.Millisecond), probe.body)
		return 0
	}
	fmt.Fprintf(os.Stderr, "codvian-router: health: GET http://127.0.0.1:%d/healthz failed: %v\n", port, probe.err)
	return 1
}

// errGatewayUnmanaged is returned by stopSupervisedGateway when a
// gateway answers /healthz but no supervisor PID file exists: the
// gateway is running, but it was started directly (e.g. `serve`),
// so these commands cannot prove which process it is and will not
// signal it.
var errGatewayUnmanaged = errors.New("gateway is running but not supervised (no PID file)")

// stopSupervisedGateway stops the gateway recorded in the PID file
// and reports whether one was running. It returns
// (stopped, unmanaged, err):
//
//   - stopped=true: a supervised gateway was running and is stopped now.
//   - stopped=false, unmanaged=false: nothing was running.
//   - unmanaged=true: a gateway answers /healthz but there is no PID
//     file — the caller must refuse to act on it.
//   - err != nil: a gateway is (or may be) running and could not be
//     stopped, or a recorded pid could not be verified as a
//     codvian-router process (see verifyRouterProcess).
//
// The supervisor is signalled first so it stops the child
// gracefully through its own Stop; only when the supervisor is gone
// (or wedged) is the gateway pid signalled directly, and only after
// verifying it is really a codvian-router process — a stale PID file
// must never cause a random process to be killed.
func stopSupervisedGateway(configPath string, port int) (stopped bool, unmanaged bool, err error) {
	pidPath := tray.PIDFilePath(configPath)
	rec, readErr := readSupervisorPIDFile(pidPath)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			if probeHealthz(port).ok {
				return false, true, nil
			}
			return false, false, nil
		}
		return false, false, readErr
	}
	gatewayAlive := processAlive(rec.GatewayPID)
	supervisorAlive := processAlive(rec.SupervisorPID)
	if !gatewayAlive && !supervisorAlive {
		// Stale record: both pids dead. Drop it so the next
		// lifecycle command starts clean.
		_ = os.Remove(pidPath)
		return false, false, nil
	}
	// Never signal this process itself: a corrupt PID file
	// naming the caller's own pid would otherwise kill the CLI.
	self := os.Getpid()
	if supervisorAlive && rec.SupervisorPID != self {
		if !verifyRouterProcess(rec.SupervisorPID) {
			return false, false, fmt.Errorf("supervisor pid %d is not a codvian-router process — refusing to signal it (stale PID file %s? remove it by hand if the gateway is gone)", rec.SupervisorPID, pidPath)
		}
		if sigErr := stopProcess(rec.SupervisorPID); sigErr != nil {
			return false, false, fmt.Errorf("cannot signal supervisor pid %d: %w", rec.SupervisorPID, sigErr)
		}
	}
	// Wait up to 10s for the supervisor to stop the child
	// gracefully and exit itself.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(rec.GatewayPID) && !processAlive(rec.SupervisorPID) {
			_ = os.Remove(pidPath)
			return true, false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The supervisor is gone or wedged but the gateway is still
	// up: signal the gateway directly (verified first), then
	// escalate to a force-kill.
	if processAlive(rec.GatewayPID) && rec.GatewayPID != self {
		if !verifyRouterProcess(rec.GatewayPID) {
			return true, false, fmt.Errorf("gateway pid %d is still running but is not a codvian-router process — refusing to signal it (stale PID file %s? remove it by hand if the gateway is gone)", rec.GatewayPID, pidPath)
		}
		_ = stopProcess(rec.GatewayPID)
		deadline = time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(rec.GatewayPID) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if processAlive(rec.GatewayPID) && rec.GatewayPID != self {
		_ = killProcess(rec.GatewayPID)
		deadline = time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(rec.GatewayPID) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if processAlive(rec.GatewayPID) {
		return true, false, fmt.Errorf("gateway pid %d did not stop", rec.GatewayPID)
	}
	_ = os.Remove(pidPath)
	return true, false, nil
}

func runStop(opts options, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: stop takes no arguments\n")
		return 2
	}
	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := resolveLifecyclePort(opts, configPath)
	stopped, unmanaged, err := stopSupervisedGateway(configPath, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: stop: %v\n", err)
		return 1
	}
	if unmanaged {
		fmt.Fprintf(os.Stderr, "codvian-router: stop: gateway answers /healthz on port %d but has no supervisor PID file — it was started directly (e.g. `serve`); stop that process directly instead\n", port)
		return 1
	}
	if !stopped {
		fmt.Println("gateway: not running")
		return 0
	}
	fmt.Println("gateway: stopped")
	return 0
}

// spawnSupervisedGateway starts a detached `tray` supervisor for
// configPath/port (the same process `codvian-router tray` runs in
// the foreground) and returns immediately: the supervisor outlives
// this CLI. The supervisor's stdout/stderr — which the gateway
// child inherits — are appended to <config-dir>/router.log, so
// `logs` can tail the gateway's output; a gateway started by hand
// in a terminal logs to that terminal instead.
func spawnSupervisedGateway(configPath string, port int) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot resolve gateway binary: %w", err)
	}
	args := []string{}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}
	if port > 0 {
		args = append(args, "--port", strconv.Itoa(port))
	}
	args = append(args, "tray")
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	logPath := tray.LogFilePath(configPath)
	if logPath != "" {
		if dir := filepath.Dir(logPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("cannot create log directory %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("cannot open gateway log %s: %w", logPath, err)
		}
		defer f.Close()
		cmd.Stdout = f
		cmd.Stderr = f
	}
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start tray supervisor: %w", err)
	}
	// Do not Wait: the supervisor outlives this CLI process.
	return nil
}

// waitForGateway polls for the supervised gateway to appear (PID
// file with a live gateway pid) and answer /healthz, up to timeout.
// It returns the gateway pid (0 when none appeared) and whether
// /healthz answered 200.
func waitForGateway(configPath string, port int, timeout time.Duration) (pid int, healthy bool) {
	pidPath := tray.PIDFilePath(configPath)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec, err := readSupervisorPIDFile(pidPath); err == nil && processAlive(rec.GatewayPID) {
			pid = rec.GatewayPID
			if probeHealthz(port).ok {
				return pid, true
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if rec, err := readSupervisorPIDFile(pidPath); err == nil && processAlive(rec.GatewayPID) {
		pid = rec.GatewayPID
	}
	return pid, false
}

func runRestart(opts options, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: restart takes no arguments\n")
		return 2
	}
	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	port := resolveLifecyclePort(opts, configPath)
	stopped, unmanaged, err := stopSupervisedGateway(configPath, port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: %v\n", err)
		return 1
	}
	if unmanaged {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: gateway answers /healthz on port %d but has no supervisor PID file — stop that process directly, then run `codvian-router restart` to start a supervised gateway\n", port)
		return 1
	}
	if stopped {
		fmt.Println("gateway: stopped (restarting)")
	}
	if err := spawnSupervisedGateway(configPath, port); err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: %v\n", err)
		return 1
	}
	pid, healthy := waitForGateway(configPath, port, 15*time.Second)
	if pid == 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: supervisor started but no gateway PID file appeared at %s; see the gateway log at %s\n", tray.PIDFilePath(configPath), tray.LogFilePath(configPath))
		return 1
	}
	fmt.Printf("gateway: restarted (pid %d, port %d)\n", pid, port)
	if !healthy {
		fmt.Fprintf(os.Stderr, "codvian-router: restart: warning: gateway pid %d is up but /healthz did not answer within 15s; see the gateway log at %s\n", pid, tray.LogFilePath(configPath))
		return 1
	}
	fmt.Printf("  health: ok (GET /healthz -> 200)\n")
	return 0
}

// tailLogFile returns the last n lines of the file at path (n <= 0
// means the whole file). At most the last 4 MiB are read, so a
// long-lived gateway log cannot blow up the CLI's memory; a file
// larger than that is tailed from its last 4 MiB.
func tailLogFile(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const maxWindow = 4 << 20
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > maxWindow {
		start = info.Size() - maxWindow
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	// A window cut mid-line drops the partial first line.
	if start > 0 {
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// followLogFile prints lines appended to path until ctx ends (the
// caller cancels it on SIGINT/SIGTERM). Polling, not inotify: the
// log is written by a child process on the same machine and a
// 500 ms poll is fast enough for tail -f duty.
func followLogFile(ctx context.Context, path string, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := out.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if err != nil {
				break
			}
		}
	}
}

func runLogs(opts options, args []string) int {
	if len(args) > 0 {
		fmt.Fprintf(os.Stderr, "codvian-router: logs takes no arguments\n")
		return 2
	}
	configPath, err := resolveConfigPath(opts.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "codvian-router: %v\n", err)
		return 1
	}
	logPath := tray.LogFilePath(configPath)
	lines := 50
	if opts.logsLinesSet {
		lines = opts.logsLines
	}
	tail, err := tailLogFile(logPath, lines)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "codvian-router: logs: no gateway log at %s\n", logPath)
			fmt.Fprintf(os.Stderr, "  the gateway logs to the terminal of the process that started it.\n")
			fmt.Fprintf(os.Stderr, "  start it with `codvian-router restart` (or `update`) to capture its output to this file.\n")
			return 1
		}
		fmt.Fprintf(os.Stderr, "codvian-router: logs: %v\n", err)
		return 1
	}
	for _, line := range tail {
		fmt.Println(line)
	}
	if opts.logsFollow {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := followLogFile(ctx, logPath, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "codvian-router: logs: %v\n", err)
			return 1
		}
	}
	return 0
}
