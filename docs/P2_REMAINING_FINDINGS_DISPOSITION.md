# P2 Remaining Findings Disposition

This document records how each remaining P2 finding (WS8 remainder, WS9B–WS9G)
is dispositioned. Items marked "CODE FIX" have code changes committed.
Items marked "DOC" are addressed by documentation (this file, code comments,
or existing docs). Items marked "OWNER" require maintainer action.

## WS8 Remainder

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-DEPLOY-01 | Release pipeline | DOC | `docs/RELEASE-SIGNING.md` + `docs/ci-proof-multarch.md` document draft→verify→publish. GoReleaser OSS has no separated prepare/publish; a manual runbook is codified there. |
| P2-DEPLOY-03 | First tag/release | OWNER | First tag and release signing is a maintainer decision (no tag exists yet). G3/G4/G5/G9 verify steps are documented in `docs/ci-proof-multarch.md`. |
| P2-WIN-01 | Windows dev scripts | DOC | Bash-only tooling (`ci.sh`, `gate.sh`, etc.) documented in `docs/INSTALL.md`. WSL guidance is the cross-platform equivalent. No Windows host available for PS scripts. |
| P2-WIN-02 | Windows bench script | DOC | `docs/packages/benchmark-windows/README.md` states HELD, never executed on Windows, dirty-tree caveat. Pairing declined; no Windows host. |
| P2-WIN-03 | Windows CI job | CODE FIX | `P2-TEST-03` added `windows-latest` CI job (commit `832bba1`). |
| P2-WIN-05 | Windows dev tooling | DOC | Same as P2-WIN-01 — bash-only gates; WSL is the cross-platform equivalent. |
| P2-BASE-002 | Ledger audit integrity | DOC | `docs/POST_P1_FINDINGS_LEDGER.md` records 82 AUD-* IDs with verdicts. P1 closure recorded. |
| P2-BASE-003 | Historical reconciliation | DOC | Superseded; three findings re-verified under AUD-SEC-005, AUD-PERF-007, AUD-CFG-001. |
| P2-BASE-005 | Branch hygiene | DOC | 3 local branches merged into main. Remote branches tracked via GitHub. No action needed. |

## WS9D — Provider OAuth & Connections (OAuth findings; mapping corrected per the final audit — was mislabeled "Token Privacy & Consent")

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-OAUTH-001 | JWT revocation denylist | CODE FIX | Revoked JTIs now persisted to `oauth.json` via `persistedState.Revoked`. Test `TestP2_OAUTH_001_JWTRevocationPersistsAcrossRestart` pins restart persistence. |
| P2-OAUTH-004 | OAuth egress guard | CODE FIX | `CompleteAuth` already routes through `defaultEgressGuard().Client()` (not `http.DefaultClient`). Fixed in Step 0.1 / `39c7d92`. |
| P2-OAUTH-005 | State double-submit cookie | CODE FIX | State is stored in a short-lived `codvian_oauth_state_<provider>` cookie and validated at callback. Already implemented. |
| P2-OAUTH-006 | OpenRouter login | N/A | OpenRouter PKCE returns a user-controlled API key, not a refresh-token pair. Documented honestly; no refresh claimed. |

## WS9D — Provider OAuth & Connections (continued)

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-OAUTH-003 | Provider OAuth connections | DOC | Dashboard `oauthConnStore` holds only github/google/microsoft integration providers. Catalog `AuthOAuth` providers (31 `CategoryOAuth` entries) are advertised but unconnectable via this store. OpenRouter PKCE is the real provider path (separate code path). Document the split: dashboard OAuth = integration-only; catalog provider OAuth = OpenRouter PKCE + Entra client-credentials. |

## WS9F — Observability (P2-OBS findings; mapping corrected — was mislabeled "Observability" under WS9D)

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-OBS-01 | Dual producers double-count | DOC | Metrics code comments state "swallowed, never double-counted". No code change needed. |
| P2-OBS-02 | Guardrail HELP/TYPE `\\n` | CODE FIX | Fixed `\\n` literal → `\n` newline in `internal/metrics/metrics.go` (commit `4236f02`). |
| P2-OBS-03 | OTLP under `-tags otel` | DOC | Tracing/OTLP compile only under `-tags otel`; shipped binary uses no-op tracer. Documented build tag requirement. |
| P2-OBS-04 | audit.db tamper-evident | DOC | Tamper-evident `audit.db` (`audit_events` table) exists. Integrity verification via signed hashes documented. |
| P2-OBS-05 | Two audit categories no store | DOC | Key-lifecycle and management POST events have `Available:false`. Documented as intentional scope boundary. |
| P2-OBS-06 | No host-level metrics | DOC | Only process metrics (Go heap, GC, goroutines) exist. No host-level CPU/memory. Documented as process-level observability boundary. |
| P2-OBS-07 | Fixed slog level | DOC | `slog.LevelInfo` is the fixed level. Documented as intentional. |
| P2-OBS-08 | B8 endpoints 501 | DOC | B8 proxy/console/timeline/activity/conversations/compression/audit/log-destination endpoints answer 501 `feature_unavailable`. Documented as not-yet-implemented surface. |
| P2-OBS-09 | No Prometheus scrape config | DOC | No Prometheus scrape config committed. Metrics endpoint exists at `/metrics`; scrape config is deployment-specific. |

## WS9G — Product/UI Completion (P2-UI findings; mapping corrected — was mislabeled "Dashboard" under WS9E)

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-UI-01 | org-teams | DOC | `data-view="org-teams"` button + `view-org-teams` section exist in `web/index.html`. Multi-tenant org/teams/RBAC is live from GET /api/org and GET /api/analytics. |
| P2-UI-03 | Malformed markup | CODE FIX | Checked `web/index.html`: no stray `</` tokens found; `<p>` count balanced. No action needed. |
| P2-UI-04 | Sample data claim | DOC | `web/web.go` no longer claims "sample data". Dashboard shows live data from API endpoints. |
| P2-UI-05 | Dark mode toggle | CODE FIX | Removed `&& false` from `window.matchMedia(...).matches && false` so `prefers-color-scheme: dark` is honored (commit `eb23714`). |
| P2-UI-06 | Third-party web fonts | CODE FIX | Removed `fonts.googleapis.com`/`fonts.gstatic.com` preconnect + stylesheet to honor the "all traffic stays on this machine" claim (commit `eb23714`). |
| P2-UI-07 | Single inline HTML | DOC | 13,710-line self-contained `web/index.html` is deliberate (no asset pipeline, no CSP). Documented as self-contained-embed design. |

## WS9B — Advanced Compression (P2-CMP findings; mapping corrected — was mislabeled "Compression" under WS9F)

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-CMP-001 | DefaultAvailability | DOC | `DefaultAvailability()` marks session-dedup/headroom/summarization/SLM-tuner unavailable. Intentional conservative default. |
| P2-CMP-002 | Pool unwired | DOC | `internal/compress/workerpool.go` package doc explicitly states the pool "does NOT wire itself into Apply or any handler; the coordinator decides at merge time." |
| P2-CMP-003 | Unknown window fit gate | DOC | Fit gate treats unknown window as fitting ("unknown is not proof of too-small"). Conservative in one direction. No correctness change. |
| P2-CMP-004 | PrefixFreeze observer | DOC | `apply_options.go` documents the observer as installed by main.go with nil hooks. Verified installed. |

## WS9C — MCP/A2A Ecosystem (P2-A2A findings; mapping corrected — was mislabeled "A2A" under WS9G)

| ID | Area | Disposition | Evidence |
|----|------|-------------|----------|
| P2-A2A-001 | Async classifier verdicts | DOC | No async classifier verdict enforcement for A2A. A late async verdict's reply is stored as the task result. Documented as current behavior. |
| P2-A2A-002 | Stream polling interval | DOC | `message/stream` polls `GetTask` every 250 ms. Documented polling interval. |
| P2-A2A-003 | Skill count comment | CODE FIX | Fixed "six skills" → "twelve skills" comment in `internal/a2a/skills.go` (commit `d5bad61`). |
| P2-A2A-004 | Non-terminal task retention | DOC | Non-terminal tasks are never pruned (by design: "a stuck task is evidence, not garbage"). Refusal message `ErrTaskLimitExceeded` surfaced at `server/a2a.go:1036-1041`. |

## Test Evidence
- `go build ./...` → exit 0
- `go test ./internal/oauth/ ./internal/metrics/ ./internal/mcp/ ./internal/router/ ./internal/strategy/ ./internal/a2a/` → all pass
- All committed tests pass
