# T05 (fallback) — PKG-3 Lifecycle Subcommands (GAP-TRACK §8) — VERDICT

- **Date:** 2026-11-19 IST (~19:25 IST; `date` in VM: Thu Nov 19 13:55:29 UTC 2026)
- **Tester:** verify-only subagent (T05 fallback slice)
- **Worktree:** `~/workspace/codvian-router/`, HEAD `6831302`, dirty tree
- **Verdict: NOT-LANDED** (snapshot at check time — see note on in-flight build below)

## What was verified (commands run + results)

1. Read the main dispatch switch `cmd/codvian-router/main.go:521-574` (`switch cmd` in `run()`).
   Cases present: `serve`, `init`, `wizard`, `run`, `keygen`, `eval`, `eval-feedback`, `validate`, `tray`, `doctor`, `configure`, `mcp`, `provider`, `catalog`, `verify-release`, `release`, `db`, `help`, `version`. **No `status`, `stop`, `restart`, `logs`, `health`, or `update` case.**
2. Read the usage text `cmd/codvian-router/main.go:573-660` — no lifecycle subcommand listed.
3. Repo-wide grep for `runStatus|runStop|runRestart|runLogs|runHealth|"health"|"logs"|"restart"|"stop"` over `cmd/` + `internal/` (`*.go`, excluding healthz/healthcheck/log lines): only unrelated hits (chat-completion finish reason `"stop"`, log lines). No lifecycle command implementation anywhere.
4. Confirmed the underlying machinery exists but is unexposed, exactly as GAP-TRACK §8 describes: `internal/tray/supervisor.go:160` `Start`, `:203` `Stop`, `:231` `Restart` (plus `AutoRestarts` `:151`). No PID-file mechanism found in `internal/tray/*.go` (grep for pid file/write: no hits) — so `status`/`stop` would have nothing to consult, matching the GAP-TRACK §9 design note.
5. No live-instance test was possible or attempted: there is no lifecycle CLI to run against a running instance.

## Evidence (file:line)

- Absence: `cmd/codvian-router/main.go:521-574` — dispatch switch has 18 cases, none lifecycle.
- Absence: `cmd/codvian-router/main.go:573-660` — usage text lists no `status`/`stop`/`restart`/`logs`/`health`/`update`.
- Present but unexposed: `internal/tray/supervisor.go:160` (`Start`), `:203` (`Stop`), `:231` (`Restart`).
- GAP-TRACK cross-check: `docs/GAP-TRACK.md:543` (§8 PKG-3 definition) and `:825` (§9 STILL-OPEN record) describe exactly this state.

## Failures / nits

- **NOT-LANDED:** none of the six lifecycle subcommands (`status`/`stop`/`restart`/`logs`/`health`/`update`) exists in the tree at check time.
- **Note — build may be in flight:** `docs/GAP-TRACK.md:827` records "PKG-3 build spawned (Wave 1 top-up, 2026-11-19 ~19:14 IST) — 6 lifecycle subcommands per §8 fix + PID-file design note" and later entries still list PKG-3 as running. This verdict is a snapshot of the tree at ~19:25 IST; if the builder lands after this check, re-verify — do not treat this file as proof the slice is still missing later.
- No live-instance behaviour was tested (nothing to test against); when the slice lands, `status`/`stop`/`restart`/`logs`/`health` must be exercised against a real running instance, including the PID-file + `/healthz` path the §8 fix specifies.
