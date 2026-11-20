//go:build windows

package tray

import (
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// Windows supervision — the real native equivalents of the Unix
// process-group behaviour, never a silent no-op:
//
//   - The child is started with CREATE_NEW_PROCESS_GROUP: its PID
//     becomes a console control group ID, so one control event
//     reaches the child AND any grandchildren it spawns in the same
//     group — the same scope SIGTERM-to--PGID has on Unix. (The
//     stdlib syscall package has no GenerateConsoleCtrlEvent, so the
//     event comes from golang.org/x/sys/windows, already a module
//     dependency.)
//   - Graceful stop is CTRL_BREAK_EVENT to that group via
//     GenerateConsoleCtrlEvent. The Go runtime delivers a console
//     control event as SIGINT (os.Interrupt), and the gateway's
//     serve command shuts down gracefully on SIGINT — so this is a
//     real graceful stop, not a kill. A non-Go child gets the
//     default console handler, which terminates it.
//   - Escalation is Process.Kill (TerminateProcess on the direct
//     child).
//
// Honest limitation (see also internal/tray/README.md): Windows has
// no group SIGKILL — TerminateProcess kills only the direct child,
// so a grandchild that ignored the break event can outlive the
// gateway. The graceful event still reaches the whole group, which
// is the case that matters for a well-behaved gateway.
//
// If the supervisor itself has no console (a Windows service, or a
// detached process), GenerateConsoleCtrlEvent fails and Stop goes
// straight to Process.Kill instead of waiting out a grace period
// for a signal that was never delivered. The child still dies; it
// just does not get a graceful window.
func configureChildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// stopChildProcessGroup terminates the child process group (see the
// header note for the Windows semantics): CTRL_BREAK_EVENT to the
// group, then Process.Kill on the direct child after stopGrace if
// the monitor has not reaped it (doneCh closes when Wait returns),
// or immediately if the break event could not be delivered at all.
// It returns after the escalation signal is sent; Stop waits on
// doneCh for the actual reap.
func stopChildProcessGroup(cmd *exec.Cmd, doneCh chan struct{}) {
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid)); err == nil {
		select {
		case <-doneCh:
			return
		case <-time.After(stopGrace):
		}
	}
	// No console to signal through, or the grace period expired:
	// terminate the direct child.
	_ = cmd.Process.Kill()
}
