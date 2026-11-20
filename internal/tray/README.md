# internal/tray — gateway supervisor (+ deferred tray icon)

## What it does

`internal/tray` keeps the Codvian Router gateway alive as a child process
and reports its lifecycle state. It is backend only: it never shows, logs,
or transmits any secret (no gateway keys, no provider keys). The only
thing it ever displays is the gateway's state (`running`, `stopped`,
`unknown`) and, in the future tray menu, the port.

- `New(dataDir string, port int) *Supervisor` — supervises the gateway
  whose config file is `<dataDir>/config.json` (by default
  `~/.config/codvian-router/config.json` when `dataDir` is empty). When
  `port > 0` it is passed to the child as `--port`; otherwise the port
  comes from the config file. `NewWithConfig(configPath, port)` is the
  same supervisor for callers that already hold an explicit config path.
- The child is the `codvian-router` binary itself, resolved via
  `os.Executable`, invoked as
  `codvian-router [--port PORT] --config <dataDir>/config.json serve`.
- `NewWithCommand(name, args...)` is a **test-only** constructor that
  spawns an arbitrary command instead of the gateway (tests use `sleep`
  / `false` / `sh`). Do not use it in production code.
- `Status()` returns `"running"`, `"stopped"`, or `"unknown"`.
  `"unknown"` means the child died unexpectedly and an automatic restart
  is pending.
- `Start(ctx)`, `Stop()`, `Restart(ctx)` manage the child. `Start` and
  `Stop` are idempotent. The child is stopped when `ctx` ends.
- `RunHeadless(ctx, sup)` (headless builds) blocks until `ctx` ends or
  the child dies unexpectedly, in which case it returns an error naming
  the exit.

## Build-tag rule

- `supervisor.go` — no build tag. The `Supervisor` API works in every
  build.
- `supervisor_unix.go` — `//go:build !windows`. Own process group via
  `Setpgid`; `Stop` signals the whole group (SIGTERM, then SIGKILL).
- `supervisor_windows.go` — `//go:build windows`. The child runs in
  its own console control group (`CREATE_NEW_PROCESS_GROUP`); `Stop`
  sends `CTRL_BREAK_EVENT` to the group (the Go runtime delivers a
  console control event as SIGINT, and the gateway's `serve` shuts
  down gracefully on SIGINT — so this is a real graceful stop, not a
  kill) and escalates to `Process.Kill` (TerminateProcess on the
  direct child). The event comes from `golang.org/x/sys/windows`
  (the stdlib `syscall` package has no `GenerateConsoleCtrlEvent`);
  `golang.org/x/sys` is a direct module dependency because of this
  file. See the header comment in `supervisor_windows.go` and the
  Windows limitation below.
- `headless.go` — `//go:build !tray`. `RunHeadless` is the blocking
  supervisor loop for builds without a GUI.
- `tray_gui.go` — `//go:build tray`. Would hold the system-tray icon
  (see "Tray icon status" below). It is **not present** in the tree
  right now, so `go build ./...` never needs a GUI toolchain.
- Test helpers are split the same way:
  `supervisor_platform_unix_test.go` (`sleep` / `false` / `sh` +
  `syscall.Kill(pid, 0)` liveness) and
  `supervisor_platform_windows_test.go` (`cmd.exe` / `timeout` /
  `exit 1` + `OpenProcess`/`GetExitCodeProcess` liveness), so the
  tests compile — and run — on Windows too. The Windows tests have
  not been executed in this VM (no Wine); they run in CI.

## Restart policy

- The child runs in its own process group, so `Stop` can signal the
  whole group (grandchildren included).
- On Unix (`Setpgid`): `Stop` sends SIGTERM to the process group,
  waits up to 2s, then escalates to SIGKILL. On Windows
  (`CREATE_NEW_PROCESS_GROUP`): `Stop` sends CTRL_BREAK_EVENT to
  the console control group, waits up to 2s, then escalates to
  `Process.Kill` on the direct child — and escalates immediately,
  without waiting, when the break event cannot be delivered at all
  (the supervisor has no console, e.g. a Windows service or a
  detached process). The monitor goroutine always `Wait()`s on the
  child, so no zombie process is left behind.
- If the child exits unexpectedly (anything other than a clean `Stop`),
  the supervisor waits 1s and restarts it **once** per `Start` call.
  If the restarted child also dies unexpectedly (or the restart itself
  fails), the state becomes `"stopped"` and the exit is kept in
  `LastError()`. No further automatic restarts are attempted until the
  next explicit `Start`/`Restart`.
- `Restart` stops the current child, resets the one-restart budget, and
  starts a fresh child (new PID).

## Tray icon status: DEFERRED (honest status, 2026-11-17)

The GUI tray icon is **deferred pending a GUI toolchain on this VM**.
Attempted 2026-11-17:

- `go get github.com/getlantern/systray@latest` succeeded (v1.2.2).
- `go build -tags tray ./internal/tray/` **failed**: the systray Linux
  backend needs cgo + pkg-config packages that are not installed —
  `ayatana-appindicator3-0.1` (and the `gtk+-3.0` dev headers;
  `pkg-config --modversion gtk+-3.0` reports "not found", although the
  `libgtk-3.so.0` runtime library is present).
- Per the task rule, the GUI file was **deleted** rather than left
  broken, and `go mod tidy` removed the systray dependency again, so
  `go build ./...` (without the tray tag) passes cleanly and the module
  has no GUI dependency.

To enable the tray icon later, on a machine with a GUI toolchain:

1. Install the dev libraries (Debian/Ubuntu:
   `libgtk-3-dev libayatana-appindicator3-dev`).
2. Re-add `github.com/getlantern/systray` (`go get
   github.com/getlantern/systray`).
3. Recreate `tray_gui.go` with `//go:build tray`, exposing
   `RunTray(ctx, sup)`: the menu must have a disabled Status item
   showing `running`/`stopped`/`unknown`, Start, Stop, Restart, and
   Quit — and must never show secrets.
4. Verify with `go build -tags tray ./...` and `go vet -tags tray
   ./internal/tray/`.

## Limitations

- The supervisor manages exactly one gateway child; it does not do
  port health-checks — `Status() == "running"` means the child process
  is alive, not that it is serving HTTP successfully.
- `RunHeadless` tracks the run active when it was called; a concurrent
  `Restart` starts a new run that it does not follow.
- The tray icon does not exist yet (see above); on this VM the
  supervisor is headless-only.
- Windows has no group SIGKILL: `Process.Kill` terminates only the
  direct child, so a grandchild that ignored the CTRL_BREAK_EVENT
  can outlive the gateway. The graceful event still reaches the
  whole group, which is the case that matters for a well-behaved
  gateway. The Windows supervisor tests (`supervisor_test.go` via
  `supervisor_platform_windows_test.go`) have not been executed in
  this VM (no Wine); they run in CI, and the Windows release binary
  has not been executed either — built only, unproven until CI.
