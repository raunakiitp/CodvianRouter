//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// stillActive is the exit code GetExitCodeProcess reports while a
// process is still running (STILL_ACTIVE, 259).
const stillActive = 259

// processAlive reports whether a process with this pid exists, via
// OpenProcess + GetExitCodeProcess: a handle that opens and reports
// STILL_ACTIVE means the process is alive. A pid that no longer
// exists fails to open.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// stopProcess asks a process to exit. Windows has no cross-process
// graceful signal a second process can deliver to a console process
// without also signalling its own console group, so this terminates
// the process (TerminateProcess). The supervisor is signalled first
// by `stop`, so the graceful path is the supervisor's own Stop when
// it exits — but a supervisor killed this way cannot run it, which
// is why stopSupervisedGateway then also terminates the gateway
// child directly.
func stopProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// killProcess force-kills a process (TerminateProcess).
func killProcess(pid int) error {
	return stopProcess(pid)
}

// verifyRouterProcess reports whether pid is a codvian-router
// process, so `stop` never signals a random process that reused a
// pid from a stale PID file. It compares the process image name
// against codvian-router.exe.
func verifyRouterProcess(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false
	}
	name := windows.UTF16ToString(buf[:size])
	return strings.EqualFold(filepath.Base(name), "codvian-router.exe")
}

// detachProcess is a no-op on Windows: a child started with
// exec.Command survives the parent's exit (no automatic kill), and
// its output is already redirected to the gateway log file.
func detachProcess(cmd *exec.Cmd) {}
