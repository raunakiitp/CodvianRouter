# Slice 5 — MCP server + CLI polish: gate report

Date: 2026-11-17 · Go 1.27.4 · Worktree `~/workspace/codvian-router/`
(build only — nothing pushed to GitHub, no GitHub connector or browser used)

> Errata (2026-11-19): this report describes slice 5 as run on 2026-11-17,
> when the server had 3 tools and protocol 2024-11-05. The protocol version
> and tool count stated in this file were updated in place to the current
> values (2025-03-26, 24 tools); the rest of the report preserves the
> as-run state (3 tools, 2024-11-05) and is historical, not current.

## What was built

### 1. MCP server — real tools, read-only, stdio (`internal/mcp/`, new)

`codvian-router mcp` serves MCP (protocol 2025-03-26) over stdio
(newline-delimited JSON-RPC 2.0, the transport MCP clients use for
local servers). Three tools, all backed by the live registry/config
through a `Source` interface — never canned responses:

- `router_status` — listen address, provider/model counts, gateway-key
  count, auto-router configured yes/no, uptime (-1: the stdio server
  has no serving loop, reported honestly, never guessed).
- `provider_list` — id, name, wire format, enabled, models, catalog id,
  verification. Never a base URL, never a key.
- `model_list` — every model from the configured providers, deduplicated,
  plus the virtual `auto/*` models when the auto-router can resolve them
  (same `buildAutoRouter` rule the request path uses).

Protocol surface: `initialize`, `notifications/initialized`,
`tools/list`, `tools/call`, `ping`. Unknown methods → -32601, unknown
tools → -32602, malformed JSON → -32700 (and the loop continues);
notifications get no response. There is deliberately NO chat/sampling
tool: MCP here is a read-only window onto the router, not a second
request path — a tool call can never spend quota or leak a key.
`New(nil, …)` panics, so a server without live state cannot exist.

OmniRoute comparison (research log entry 19/23): OmniRoute ships a
110-tool MCP server plus A2A, guardrails, cloud agents and a plugin
marketplace. That is the kitchen-sink scope entry 23 says not to copy.
Codvian Router ships the 3 read-only tools a client needs to see the
router's state, and nothing else — for now.

### 2. CLI polish (`cmd/codvian-router/doctor_configure_mcp.go`, new)

- `doctor` — real checks with `[PASS]` / `[FAIL]` / `[WARN]` per line
  plus a summary (`doctor: N passed, N failed, N warnings`, exit 1 on
  any FAIL): config file validates; gateway-key count (values never
  printed); data dir exists; per provider, key present-but-not-printed
  (`Registry.DecryptedKey`, non-empty only) AND a live reachability
  probe (health checker key test, a real `/v1/models` call, 5 s
  per-provider timeout); listen port free vs in-use; dashboard
  reachable (only when a server is actually up). WARNs never fail the
  run — zero providers or a stopped server is a state to fix, not a
  broken install. `--port` overrides the address probed, like it does
  for `serve` (found by the live smoke: doctor first ignored `--port`
  and probed the config port while the server ran on 20199 — fixed,
  with a test).
- `configure` — first-run setup in OmniRoute's `configure`/`setup`
  shape (research log entry 19): creates the config + first gateway
  key (printed once) when missing, applies `--port`, adds a provider
  by catalog id with a real key test (`AddCatalogWithOptions` — a bad
  key or dead upstream rejects the add honestly). Flag mode:
  `--provider ID --api-key KEY` or `--api-key-env VAR` (a key on the
  command line lands in shell history, so the env form is documented
  first). With no flags it prompts on stdin; empty answers keep/skip,
  EOF ends prompting, so piped input and `/dev/null` work. Idempotent:
  a second run keeps the config and the key count.
- `mcp` — `codvian-router mcp` runs the stdio server above. Stdout is
  the protocol, so the startup line goes to stderr. Requires an
  existing config (`init`/`configure` first) — without a registry
  there is no live state to serve.
- `serve` prints `mcp: stdio server available via …` on startup.
  Usage text and the file header document all three commands.

### 3. Dashboard — "MCP & CLI" section (`web/index.html`, `internal/server/mcp.go`)

Natural surface, wired to a real endpoint: `GET /dashboard/mcp`
(Tier 3 MANAGEMENT, auth-exempt like the other dashboard feeds —
payload is tool names/descriptions + live counts + listen address,
no secrets). It returns the same `mcp.Tools()` list the stdio server
serves (the two can never drift), plus live provider/model counts
from the registry and a `ready` flag (false + zero counts when no
registry is wired — honest empty state). The dashboard section
renders the tool table, the `codvian-router mcp` command, and the
`doctor`/`configure` commands, refreshed on open + every 5 s while
open + Refresh button. Guard table now holds 31 routes total
(7 LOCAL_ONLY / 1 ALWAYS_PROTECTED / 23 MANAGEMENT), including this
slice's `GET /dashboard/mcp` and other slices' routes that landed
concurrently.
`node --check` on the extracted dashboard JS is clean; "Playground"
is absent.

## Files changed (this slice)

| File | Change |
|---|---|
| `internal/mcp/mcp.go` (new) | MCP server: JSON-RPC 2.0, 3 tools, stdio transport |
| `internal/mcp/mcp_test.go` (new) | 11 tests |
| `cmd/codvian-router/doctor_configure_mcp.go` (new) | `doctor`, `configure`, `mcp`, registry→MCP adapter |
| `cmd/codvian-router/doctor_configure_mcp_test.go` (new) | 10 tests |
| `cmd/codvian-router/main.go` | dispatch + `--provider`/`--api-key`/`--api-key-env` flags + usage + serve line |
| `internal/server/mcp.go` (new) | `GET /dashboard/mcp` handler |
| `internal/server/mcp_test.go` (new) | 3 tests |
| `internal/server/guard.go` | `GET /dashboard/mcp` in the guard table |
| `internal/server/server.go` | register `GET /dashboard/mcp` |
| `internal/server/guard_test.go` | expected route list + tier counts (29→31 total; see note below) |
| `web/index.html` | "MCP & CLI" nav + section + `loadMCP()` |
| `internal/ledger/ledger.go` | **Cross-slice bug fix** — `Buckets` accumulation (see below) |
| `internal/ledger/feeds_test.go` | `TestBucketsEdgeClampMerges` regression test |

## Gate evidence (final run, 2026-11-17 ~16:49 UTC)

- `go build ./...` — OK
- `go vet ./...` — OK
- `gofmt -l cmd internal web` — clean
- `go test -count=1 ./...` — **all 22 packages ok, 563 top-level PASS +
  79 subtests = 642 PASS events, 0 fail** (baseline at slice start:
  19 packages, 487 PASS events; +76 top-level includes this slice's
  25 new tests — 11 MCP, 10 CLI, 3 dashboard feed, 1 ledger
  regression — plus other slices' tests that landed in the same
  worktree during this slice, e.g. `guardrails`, `strategy`)
- `go test -count=1 -race` on touched packages (`mcp`, `server`,
  `ledger`, `cmd/codvian-router`) — **clean**
- Dashboard JS: extracted `<script>` from `web/index.html`,
  `node --check` — **clean**; "Playground" absent (0 matches)
- Live smoke (real binary, isolated HOME, Python mock upstream on
  21071, router on 20199; script `/tmp/slice5-smoke/smoke.sh`) —
  **41/41 checks passed**:
  - **S1** `configure` with piped input (keep port, skip provider) →
    config created, gateway key issued once, dashboard URL printed
  - **S2** `configure --port 20197` on a second HOME → listen
    `127.0.0.1:20197`
  - **S3** `doctor` on the fresh install → exit 0; config/keys/data-dir
    PASS, providers WARN, port-free PASS, dashboard WARN (server not
    running); gateway key never printed
  - **S4** `provider add-catalog fireworks` (mock) → doctor: provider
    key "present (value never printed)", reachability PASS (key test
    ok in 5 ms); provider key never printed
  - **S5** `mcp` stdio: initialize → `2025-03-26`; tools/list → 24
    tools; `provider_list` → live Fireworks provider;
    `model_list` → live `model-a`; `router_status` →
    `127.0.0.1:20128`, `"providers":1`; 5 response lines for 6 input
    lines (notification gets none); no secret in output
  - **S6** `serve` on 20199 → startup line mentions mcp;
    `GET /dashboard/mcp` → stdio, 3 tools, providers 1, models 1;
    dashboard has `view-mcp` + `loadMCP`, no Playground; doctor
    `--port 20199` while serving → port-in-use WARN, dashboard PASS;
    live `POST /v1/chat/completions` with the gateway key → mock
    reply; `POST /admin/shutdown` → 202 and the process exits

## CHECKLIST rows for the coordinator to merge (do NOT edit CHECKLIST.md here)

| 76 | MCP server (backend): stdio JSON-RPC 2.0 server (`codvian-router mcp`, protocol 2025-03-26) with 24 tools — the three read-only tools built in this slice were `router_status`, `provider_list`, `model_list` — backed by the real registry/config, never canned responses and never a base URL or key; `initialize` / `tools/list` / `tools/call` / `ping`, JSON-RPC errors (-32601/-32602/-32700) for anything else; no chat/sampling tool, so a tool call can never spend quota | ✅ | ✅ |
| 77 | CLI polish + dashboard (same slice): `doctor` (config, gateway-key presence, per-provider key presence + live reachability probe, listen port, dashboard reachability — PASS/FAIL/WARN lines, exit 1 on FAIL, keys never printed, `--port` overrides the probed address) and `configure` (first-run setup: creates config + first gateway key, `--port`, `--provider` + `--api-key`/`--api-key-env` with a real key test, interactive prompts when no flags, idempotent); "MCP & CLI" dashboard section wired to the real endpoint `GET /dashboard/mcp` (same tool list the stdio server offers, live provider/model counts, honest empty state) | ✅ | ✅ |

## Two real bugs found by this slice's gate (both fixed, both proved)

1. **`internal/ledger` `Buckets` overwrote the last bucket (cross-slice
   fix).** `TestDashboardLiveFeeds` failed ~25% of runs (2/10 isolated,
   1/3 package runs, and twice in full-suite runs) with the first
   seeded row missing from the last bucket. Root cause, proved with a
   temporary instrumented test: rows recorded in the same millisecond
   as `Buckets`' internal `now` get bucket index `b == n` and clamp
   onto the last bucket — but the loop ASSIGNED each group into
   `out[b]`, so the clamped group overwrote the last bucket's own
   native group. Fix: accumulate (`+=`) groups into the bucket, with a
   request-weighted mean latency. Regression test
   `TestBucketsEdgeClampMerges` (fixed timestamps, deterministic)
   FAILS on the old code and PASSES on the fixed code. This also
   explains the earlier `TestLedgerFeeds` "early buckets" failure seen
   during this slice.
2. **`doctor` ignored `--port`** (own bug, found by the smoke, not by
   unit tests): it probed the config port while the server ran on the
   `--port` value. Fixed; `TestRunDoctorPortFlagOverridesListen`
   covers it.

One more genuine trap, for the record: `os.IsNotExist` returns FALSE
for a `%w`-wrapped missing-file error (it only unwraps `*PathError`
and friends) — `configure`'s "create when missing" branch never ran
until it was switched to `errors.Is(err, os.ErrNotExist)`.
`config.LoadOrCreate` already uses `errors.Is`; this was the same
mistake in new code.

## Notes for the coordinator

- **Shared-worktree churn.** Several other slices landed in the same
  worktree during this slice (guardrails, strategy, ledger feeds,
  dashboard models/logs/usage/keys). The tree was red at least four
  times while I worked (router mid-refactor, `config.RoutingConfig`
  referenced before its type landed, a router/server test import
  cycle, `server.go` referencing handlers not yet written). I waited
  each out rather than "fixing" other slices' in-flight code. Two
  small collisions needed a merge touch: `guard_test.go`'s expected
  route list (another edit duplicated my `GET /dashboard/mcp` entry —
  duplicate removed) and its tier counts (now 7/1/23). Final gate
  above is on the settled tree.
- **`/tmp` disk pressure.** `/tmp` is a 512 MB tmpfs, 88–92% full
  during this slice; the first `-race` run failed to link with "no
  space left on device". The race gate passed with
  `TMPDIR=$HOME/.cache/tmp`. That is an environment limit, not a code
  failure — but the coordinator's gate should use the same TMPDIR
  workaround (or free `/tmp`) or race runs will fail spuriously.
- **Not done / honest limits.** No MCP `resources`, `prompts`,
  sampling, or HTTP/SSE transport — deliberately out of scope for this
  slice (see the OmniRoute scope note above). `doctor`'s provider
  probe is the key test (`/v1/models`) only: it detects dead keys and
  quota/rate-limit responses, not model-level availability or latency
  under chat load (same documented limit as the health checker).
  `configure`'s interactive prompt cannot disable terminal echo
  (no `golang.org/x/term` in the module) — the prompt says the typed
  key is never printed, and it never is.
