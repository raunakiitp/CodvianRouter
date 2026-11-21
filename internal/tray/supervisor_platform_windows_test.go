//go:build windows

package tray

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// pingPath resolves ping.exe by absolute path: the test child's
// PATH lookup is not reliable in every session (a broken PATH
// makes cmd.exe report "'ping' is not recognized"), while
// %SystemRoot%\System32\ping.exe always exists on Windows.
func pingPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "ping.exe")
	}
	return "ping"
}

// Test-only command helpers: the Unix commands the supervisor tests
// use (sleep / false / sh) do not exist on Windows, so the Windows
// counterparts are cmd.exe invocations with the same observable
// behaviour. The Unix versions live in
// supervisor_platform_unix_test.go.

// sleepCommand returns a command that runs for ~30s (or until
// killed). ping waits without reading stdin: the previous
// `timeout /t 30` refuses redirected stdin ("Input redirection is
// not supported") and exits at once, so the supervisor's child
// never lived long enough for the auto-restart test and the PID
// file churned. ping is a separate grandchild process of cmd.exe
// in the same console control group.
func sleepCommand() (string, []string) {
	return "cmd.exe", []string{"/c", pingPath(), "-n", "31", "127.0.0.1", ">nul"}
}

// failCommand returns a command that exits immediately with status 1.
func failCommand() (string, []string) {
	return "cmd.exe", []string{"/c", "exit", "1"}
}

// restartRequestCommand runs ~2s, then exits with RestartExitCode,
// like a gateway answering POST /admin/restart.
func restartRequestCommand() (string, []string) {
	return "cmd.exe", []string{"/c", pingPath() + " -n 3 127.0.0.1 >nul & exit 75"}
}

// fastRestartCodeCommand exits with RestartExitCode at once: a
// broken child, which must not count as a requested restart.
func fastRestartCodeCommand() (string, []string) {
	return "cmd.exe", []string{"/c", "exit", "75"}
}

// shellSleepCommand returns a shell that spawns a sleep grandchild
// in the same process group, so TestStopKillsProcessGroup proves
// Stop kills the group, not just the direct child: cmd.exe spawns
// ping.exe in the same console control group, and the
// CTRL_BREAK_EVENT reaches both. ping is used instead of timeout
// for the same reason as sleepCommand: timeout refuses redirected
// stdin and exits at once.
func shellSleepCommand() (string, []string) {
	return "cmd.exe", []string{"/c", pingPath(), "-n", "31", "127.0.0.1", ">nul"}
}

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
