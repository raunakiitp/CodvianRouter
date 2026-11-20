//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// processAlive reports whether a process with this pid exists.
// syscall.Kill(pid, 0) sends no signal; it only tests existence.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// stopProcess asks a process to exit gracefully: SIGTERM. The
// supervisor (tray) catches SIGTERM and stops its gateway child
// gracefully; a gateway child also shuts down gracefully on SIGTERM.
func stopProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// killProcess force-kills a process: SIGKILL.
func killProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

// verifyRouterProcess reports whether pid is a codvian-router
// process, so `stop` never signals a random process that reused a
// pid from a stale PID file. On Linux /proc/<pid>/exe is the
// binary itself and /proc/<pid>/cmdline names it; either match
// counts. On Unixes without /proc (e.g. macOS) the check cannot
// run, so a live pid from the PID file is trusted — the PID file
// is only ever written by the supervisor for its own child.
func verifyRouterProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	if exe, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe")); err == nil {
		base := filepath.Base(exe)
		return base == "codvian-router" || base == "codvian-router.exe"
	}
	if cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		return strings.Contains(string(cmdline), "codvian-router")
	}
	if _, err := os.Stat("/proc"); err != nil {
		// No /proc on this platform: fall back to trusting a
		// live pid from the supervisor's own PID file.
		return processAlive(pid)
	}
	return false
}

// detachProcess makes the spawned supervisor independent of this
// CLI's process group and terminal, so it keeps running after the
// CLI exits (a new session via Setsid).
func detachProcess(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}
