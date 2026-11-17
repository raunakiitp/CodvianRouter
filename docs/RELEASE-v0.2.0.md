# Release notes — v0.2.0 (DRAFT)

> **DRAFT — DO NOT PUBLISH.** The Phase 2/3 backend below is **wired
> into the binary and gate-verified** by the coordinator on 2026-11-17
> (224/224 tests, live mock smoke green — see [CHECKLIST.md](../CHECKLIST.md)).
> Nothing in this file describes a released version yet: no v0.2.0 tag
> exists, no release binary has been cut, no npm package or Docker image
> has been published. This draft exists so the release can be assembled
> quickly once Raunak approves publishing — not as a claim that it
> already has.

Planned scope: Phase 2 + Phase 3 backend (live observability, health and
degradation, scoring-based routing, packaging, tray supervision). No UI:
UI is paused until Raunak supplies reference sites.

## Status of each item

| Item | Status |
|---|---|
| `internal/metrics` — Prometheus-style counters + local JSON trace log (stdlib only, no Prometheus client lib, no OTLP) | **Gate-verified 2026-11-17** — wired: `GET /metrics` (auth-gated), requests/latency/in-flight/failovers recorded; 8 unit tests |
| `internal/eval` — honest eval harness (pass = every expected substring present within the latency budget, nothing more) | **Gate-verified 2026-11-17** — wired: `eval --model MODEL` subcommand, smoke cases 3/3 vs live mocks; 9 unit tests |
| `internal/health` — provider key/quota health checks; never invents a quota value (only a quota-type failure ever lowers a hint, to 0) | **Gate-verified 2026-11-17** — wired: watcher probes at startup + every 60 s; 16 unit tests |
| `internal/degrade` — opt-in serve-stale on total provider failure, always with a visible staleness marker, never silently | **Gate-verified 2026-11-17** — wired and enabled in `serve`; live-verified (stale 200 + `X-Codvian-Stale`, uncached → 502); implements `server.StreamResumer`; 23 unit tests |
| `internal/scoring` — cost/latency/quality/reliability ranking wired via `router.Config.OrderHook` | **Gate-verified 2026-11-17** — wired as ModeBalanced, fed by health-probe signals + quota hints; cost/quality honestly unknown (0); 11 unit tests |
| `internal/tray` — gateway supervisor + optional system-tray icon (`tray` build tag); backend only, never handles secrets | **Gate-verified 2026-11-17** — wired: `tray` subcommand; auto-restart live-verified (`kill -9` child → healthz back); 7 unit tests |
| `internal/config` extensions + `SCHEMA.md` | **Gate-verified 2026-11-17** — `validate` subcommand added; config tests 16 → 21 |
| Semantic cache wired into the server/engine request path | **Not done** — package built in Phase 1 (256-dim hashed bag-of-words, cosine ≥ 0.85, NOT a neural model); wiring is still open work, not in v0.2.0 |
| Smart routing modes (`auto` model, fast/cheap/quality-first) | **Partially built** — 5 scoring modes wired (Balanced default); the `auto` model name does not work yet |
| Packaging: `Dockerfile`, `docker-compose.yml`, `package.json` + `bin/codvian-router.js`, `scripts/install.sh`, `.goreleaser.yml` | **Files in repo, not published** — Docker image not built/run here (no Docker in the VM), npm/installer happy paths untested; **no image published, no npm package published, no release cut**; see [INSTALL.md](INSTALL.md) |
| Docs: `docs/INSTALL.md`, `docs/DEMO.md`, `docs/ARCHITECTURE.md`, `CHANGELOG.md` | **Gate-verified 2026-11-17** — numbers updated to the integration gate (224/224, failover 52.4–55.0 ms) |

## What v0.2.0 will contain (gate is green — awaiting release approval)

- Everything in v0.1.0 (Phase 1 MVP — see [CHANGELOG.md](../CHANGELOG.md)).
- The six Phase 2/3 packages above, wired into the binary by the
  coordinator and verified by the single test gate (2026-11-17).
- Install paths that work from a release: Docker image, npm package,
  install script, and GoReleaser binaries (Linux/macOS/Windows ×
  amd64/arm64) — **only after the first release is actually published**.
- A reproducible demo (see [DEMO.md](DEMO.md)) proving 429 failover and
  150 ms hedging against local mock upstreams.

## What v0.2.0 will NOT contain

- No UI / dashboard (paused — awaiting reference sites from Raunak).
- No `auto` smart-routing model unless the gate verifies it (currently
  unbuilt).
- No hosted service, no telemetry by default, no default credentials —
  the local-first, auth-always-on posture from v0.1.0 is unchanged.
- No neural semantic embeddings — the semantic cache remains a local
  token-overlap cache by design.

## Release checklist (for the coordinator)

- [x] Phase 2/3 packages wired into `main.go` / `server.go` / `router.go`
- [x] Single test gate green (`go build`, `go vet`, `gofmt`, full
      `go test`, live mock-upstream smoke test incl. 429 failover, hedge,
      quota-skip, stale, eval, tray, `--listen`)
- [x] Benchmarks re-measured and recorded in `CHECKLIST.md`
- [ ] `Version` bumped to `0.2.0` in `cmd/codvian-router/main.go` and
      `package.json`
- [ ] Git tag `v0.2.0` cut; GoReleaser release published (binaries +
      `checksums.txt`); npm package published; Docker image published
- [ ] `CHANGELOG.md` moved from `[Unreleased]` to a dated `[0.2.0]` entry
- [ ] This file deleted or replaced by the published release notes

## Numbers (integration gate, 2026-11-17 — measured, not carried over)

- 429 failover 52.4–55.0 ms; hedge 203.8–205.9 ms; quota-skip 52.4–52.8 ms
  (localhost mocks, 50 ms upstream)
- Stale degradation: same prompt after mocks killed → 200 +
  `X-Codvian-Stale: true`; uncached prompt → 502
- `eval --model bench-model`: 3/3 passed (100%); `/metrics` failover
  counter 5 after the failover run; `tray` auto-restart verified
- Startup 73 ms to healthz-ready; idle RSS ~32 MB; binary 17.3 MB
- Test counts: integration gate 224/224 (cache 21, config 21, crypto 15,
  degrade 23, eval 9, health 16, ledger 12, metrics 8, provider 29,
  router 26, routetrace 2, scoring 11, server 21, tray 7, cmd/codvian-router 3)
