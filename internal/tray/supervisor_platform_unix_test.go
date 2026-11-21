//go:build !windows

package tray

import "syscall"

// Test-only command helpers: the supervisor tests spawn real child
// processes, and the Unix commands (sleep / false / sh) do not exist
// on Windows. The Windows counterparts live in
// supervisor_platform_windows_test.go.

// sleepCommand returns a command that runs for ~30s (or until
// killed).
func sleepCommand() (string, []string) {
	return "sleep", []string{"30"}
}

// failCommand returns a command that exits immediately with status 1.
func failCommand() (string, []string) {
	return "false", nil
}

// restartRequestCommand runs ~2s, then exits with RestartExitCode,
// like a gateway answering POST /admin/restart.
func restartRequestCommand() (string, []string) {
	return "sh", []string{"-c", "sleep 2; exit 75"}
}

// fastRestartCodeCommand exits with RestartExitCode at once: a
// broken child, which must not count as a requested restart.
func fastRestartCodeCommand() (string, []string) {
	return "sh", []string{"-c", "exit 75"}
}

// shellSleepCommand returns a shell that spawns a sleep grandchild
// in the same process group, so TestStopKillsProcessGroup proves
// Stop kills the group, not just the direct child.
func shellSleepCommand() (string, []string) {
	return "sh", []string{"-c", "sleep 30"}
}

// processAlive reports whether a process with this pid exists.
// syscall.Kill(pid, 0) sends no signal; it only tests existence.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
