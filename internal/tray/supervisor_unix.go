//go:build !windows

package tray

import (
	"os/exec"
	"syscall"
	"time"
)

// configureChildProcessGroup puts the child in its own process group
// (Setpgid), so Stop can signal the whole group — grandchildren the
// gateway may spawn included — and so the child never shares this
// process's group.
func configureChildProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopChildProcessGroup terminates the child process group: SIGTERM
// to the whole group, then SIGKILL to the whole group after
// stopGrace if the monitor has not reaped the child (doneCh closes
// when Wait returns). It returns after the escalation signal is
// sent; Stop waits on doneCh for the actual reap.
func stopChildProcessGroup(cmd *exec.Cmd, doneCh chan struct{}) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-doneCh:
	case <-time.After(stopGrace):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
