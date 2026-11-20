# T08 — M9.10 MCP gate readiness — VERIFY-ONLY verdict

- **Date:** 2026-11-19 IST (~19:15–19:25 IST)
- **Tester:** T08 (verify-only; built nothing, edited no product code/docs)
- **Worktree:** `~/workspace/codvian-router/` (dirty tree from sibling churn — see git status note below; no commits/pushes made by this tester)
- **Go env:** `export PATH=$HOME/go/bin:$PATH`, `TMPDIR=$HOME/.tmp` for all go commands

## Verdicts

| # | Slice / item | Verdict |
|---|---|---|
| 1 | Fixture coverage audit — 24 tools, protocolVersion 2025-03-26, harness exercises tools | **PASS** |
| 2 | Live stdio smoke — initialize + tools/list, scopes ON by default | **PASS** |
| 3 | WT-01–WT-32 wrong-type/empty MCP error variants | **PARTIAL** (behaviour 32/32 correct; pins 9/32 — see table) |
| F | Fallback: `go build ./...` + `gofmt -l` | **PASS-WITH-NITS** (build green; 2 unformatted files, both sibling-modified whitespace) |

## (1) Fixture coverage audit — PASS

**What was verified**

- Tool registry count: 24 tools — 21 `RegisterTool(Tool{...})` calls in
  `internal/mcp/tools_cost.go` (3), `tools_ops.go` (7), `tools_routing.go` (9),
  `tools_search.go` (2), plus 3 built-ins in `internal/mcp/mcp.go:248-266`
  (`router_status`, `provider_list`, `model_list`).
- Fixture count: **97 files** under `tests/fixtures/mcp/`. Every one of the 24
  tools has a `tools-call-<tool>.json` **success** fixture (verified by parsing
  all fixture requests; 24 distinct tool names).
- `protocolVersion 2025-03-26`: pinned in `internal/mcp/mcp.go:42`
  (`const ProtocolVersion = "2025-03-26"`), asserted by fixture
  `tests/fixtures/mcp/initialize.json:28,46`
  (`"json_paths": {"result.protocolVersion": "2025-03-26"}`), and by the
  `tools-list.json` length pin `json_lengths: {"result.tools": 24}`
  (`tests/fixtures/mcp/tools-list.json:438-439`).
- Harness really exercises the tools: `internal/apitest/mcp_fixture.go`
  drives each fixture request through the real `mcp.Server.Serve` over
  in-memory stdio (`runMCPExchanges`, `RunMCP`, `RunMCPSession`) and asserts
  code + message + JSON paths against the live response — it does not compare
  fixture-to-fixture. `TestMCPFixtures` runs every entry of `mcpFixtureNames`
  (`internal/apitest/mcp_fixture_test.go:796`); `TestMCPFixtureSession` runs
  all 97 in one session (initialize → tools/list → reads → errors → mutations).
- Error-fixture coverage: 54 error cases in the session test
  (`mcp_fixture_test.go:808-812`), including unknown tool/method
  (`error-unknown-tool.json` → `-32602 unknown tool: chat`,
  `error-unknown-method.json` → `-32601`).

**Command + result**

```
go test -count=1 -run 'TestMCPFixtures|TestMCPFixtureSession|TestMCPFilteredFixtures|TestMCPFixtureRecordedResponsesMatch' ./internal/apitest/
ok  github.com/raunakiitp/codvian-router/internal/apitest  0.621s
```

All four targeted tests green (the full 97-fixture set, filtered-list
fixtures, filtered sessions, and recorded-response self-consistency).

## (2) Live stdio smoke — PASS

**What was verified** — temporary probe test
`internal/mcp/wt_t08_probe_test.go` (function `TestT08StdioSmokeProbe`,
**deleted after the run** — `ls` confirms it no longer exists; no product
file was changed):

- Drove the real `mcp.Server.Serve` over in-memory stdio with three lines:
  `initialize` → `tools/list` → `tools/call router_status`.
- `initialize` returned `protocolVersion: "2025-03-26"`.
- `tools/list` returned **24 tools**, in order:
  `router_status provider_list model_list best_combo_for_task cache_flush
  cache_stats check_quota cost_report create_combo db_health db_stats
  explain_route get_combo_metrics heartbeat list_combos memory_add
  memory_clear memory_search pick_fastest_model set_budget_guard
  set_routing_strategy simulate_route switch_combos tool_search`.
- `tools/call router_status` succeeded (result present).
- Scopes enforced ON by default: a server built with plain `New(src, "0.1.0")`
  (no `WithClientIdentity`) defaults to `Anonymous()` (deny-all, fail-closed —
  `internal/mcp/mcp.go:391-400`, `identity.go:94`); its `tools/call
  router_status` was denied with **`-32002` scope denied**. There is no off
  switch (`internal/mcp/scopes.go:10-11`; `internal/mcp/mcp.go:721-722`:
  "Scope enforcement is ON by default with no off switch").

**Command + result**

```
go test -count=1 -v -run 'TestT08StdioSmokeProbe' ./internal/mcp/
--- PASS: TestT08StdioSmokeProbe (0.01s)
```

Note: the smoke ran in-process (the same `Serve` code path the
`codvian-router mcp` subcommand wraps) rather than as a spawned binary,
per the harness design documented in `internal/apitest/mcp_fixture.go:23-31`
(apitest cannot import `cmd/main`; the subcommand round-trip is covered by
`cmd/codvian-router/doctor_configure_mcp_test.go`). No live binary was
spawned; no config/registry was needed.

## (3) WT-01–WT-32 — PARTIAL (behaviour 32/32, pins 9/32)

**Behaviour — all 32 verified live, all correct.** Temporary probe test
`TestT08WTProbe` in the same deleted file called each tool with the exact
GAP-TRACK §12 input shape via the existing `callToolErrMsg` helper
(`internal/mcp/tools_routing_test.go:48`) against `newRoutingTestServer`,
`newCostTestServer`, and `newOpsTestServer`. Every variant returned
**-32602 with the exact expected message**:

| Variant | Tool / input | Live result (-32602) |
|---|---|---|
| WT-01 | create_combo `name: 42` | `name: must be a string` |
| WT-02 | create_combo `members: ["p1", ""]` | `members[1]: must not be empty` |
| WT-03 | create_combo `strategy: 42` | `strategy: must be a string` |
| WT-04 | switch_combos `name: 42` | `name: must be a string` |
| WT-05 | switch_combos `name: ""` | `name: must not be empty` |
| WT-06 | get_combo_metrics `name: 42` | `name: must be a string` |
| WT-07 | get_combo_metrics `name: ""` | `name: must not be empty` |
| WT-08 | set_routing_strategy `strategy: 42` | `strategy: must be a string` |
| WT-09 | set_routing_strategy `strategy: ""` | `strategy: must not be empty` |
| WT-10 | set_routing_strategy `chain: 42` | `chain: must be a string` |
| WT-11 | explain_route `model: 42` | `model: must be a string` |
| WT-12 | explain_route `model: ""` | `model: must not be empty` |
| WT-13 | explain_route `combo: 42` | `combo: must be a string` |
| WT-14 | simulate_route `model: 42` | `model: must be a string` |
| WT-15 | simulate_route `model: ""` | `model: must not be empty` |
| WT-16 | simulate_route `combo: 42` | `combo: must be a string` |
| WT-17 | simulate_route `prompt_tokens: "100"` | `prompt_tokens: must be a number` |
| WT-18 | best_combo_for_task `task: ""` | `task: must not be empty` |
| WT-19 | best_combo_for_task `task: 42` | `task: must be a string` |
| WT-20 | best_combo_for_task `max_cost_usd: "5"` | `max_cost_usd: must be a number` |
| WT-21 | best_combo_for_task `max_latency_ms: "5"` | `max_latency_ms: must be a number` |
| WT-22 | pick_fastest_model `provider: 42` | `provider: must be a string` |
| WT-23 | pick_fastest_model `min_requests: "5"` | `min_requests: must be a number` |
| WT-24 | cost_report `key_id: 42` | `key_id: must be a string` |
| WT-25 | cost_report `days: "30"` | `days: must be a number` |
| WT-26 | set_budget_guard `key_id: 42` | `key_id: must be a string` |
| WT-27 | set_budget_guard `unpriced_policy: 42` | `unpriced_policy: must be a string` |
| WT-28 | set_budget_guard `daily_limit_usd: "5"` | `daily_limit_usd: must be a number` |
| WT-29 | set_budget_guard `weekly_limit_usd: "5"` | `weekly_limit_usd: must be a number` |
| WT-30 | set_budget_guard `monthly_limit_usd: "5"` | `monthly_limit_usd: must be a number` |
| WT-31 | memory_add `text: 42` | `memory_add requires a non-empty string argument "text"` |
| WT-32 | memory_search `query: 42` | `memory_search requires a non-empty string argument "query"` |

```
go test -count=1 -v -run 'TestT08WTProbe' ./internal/mcp/
--- PASS: TestT08WTProbe (0.02s)   (all 32 lines logged "... OK")
```

Handler evidence (arg-extraction helpers, all correct):
`reqStringArg` `internal/mcp/tools_routing.go:234-241`;
`reqStringListArg` `:271-278`; `optStringArg` `:251-254`;
`optFloatArg` `:288-291`; `optIntArg` `:298-308`;
`resolveCostKeyID` `internal/mcp/tools_cost.go:182-197`;
`stringArg` `internal/mcp/tools_ops.go:99-110`.

**Pins — only 9 of 32 are pinned by a fixture or test in the current tree**
(grep of the exact expected message across `tests/fixtures/mcp/*.json`,
`internal/mcp/*_test.go`, `internal/apitest/*_test.go`):

- **Pinned (9):** WT-01 (`error-create-combo-nonstring-name.json`),
  WT-02 (`error-create-combo-members-empty-element.json`),
  WT-03 (`error-create-combo-nonstring-strategy.json`),
  WT-04 (`error-switch-combos-nonstring-name.json`),
  WT-05 (`error-switch-combos-empty-name.json`),
  WT-06 (`error-get-combo-metrics-nonstring-name.json`),
  WT-07 (`error-get-combo-metrics-empty-name.json`),
  WT-08 (`error-set-routing-strategy-nonstring-strategy.json`),
  WT-24 **partially** — the message `key_id: must be a string` is pinned
  only by the **check_quota** fixture
  (`error-check-quota-nonstring-key.json`, also asserted in
  `internal/apitest/mcp_fixture_test.go:1803`); no fixture/test ever calls
  **cost_report** with a non-string `key_id`. Counted as pinned-message /
  unpinned-per-tool.
- **Unpinned (23):** WT-09, WT-10, WT-11, WT-12, WT-13, WT-14, WT-15, WT-16,
  WT-17, WT-18, WT-19, WT-20, WT-21, WT-22, WT-23, WT-25, WT-26, WT-27,
  WT-28, WT-29, WT-30, WT-31, WT-32 — exact-message grep = 0 hits in all
  fixtures/tests. Neighbouring branches ARE pinned for several (e.g. WT-17:
  non-integer `prompt_tokens` fixture exists, wrong-type does not; WT-25:
  range + non-integer `days` pinned, wrong-type not; WT-31/WT-32: the
  collapsed message is pinned for empty/missing shapes only, never for a
  non-string literal).
- **Correction to GAP-TRACK §12 (docs/GAP-TRACK.md:719-774):** its "missing
  pin" claims for WT-01–WT-08 are now **stale** — fixtures for those eight
  variants exist in the current tree (listed above) and pass in
  `TestMCPFixtures`. The remaining 23 unpinned claims in §12 still hold
  (re-verified by grep just now). Also confirmed: fixture
  `error-best-combo-empty-task.json` passes `arguments:{}` and asserts
  `task: required` — it pins the *missing* branch, not the empty-string
  branch (WT-18), exactly as §12 notes; the fixture name is misleading.

**Why PARTIAL, not PASS:** behaviour is correct for all 32, but per the
task rule "Behaviour-correct-but-unpinned is PARTIAL, not PASS", 23 of 32
variants have no test pin — a future refactor of the shared helpers could
regress them silently. The fix slice needs 23 new fixtures (or unit-test
assertions), one per unpinned variant, plus a genuinely-empty-task fixture
to replace the misnamed `error-best-combo-empty-task` pin.

## Fallback slice — PASS-WITH-NITS

- Loadavg at run time: `4.61 12.10 12.79` (1-min 4.61 < ~8 threshold) — build
  run was permitted.
- `go build ./...` — **exit code 0, no errors** (clean build of the whole
  tree; only the benign GOPATH/GOROOT warning). Note: an earlier piped
  invocation reported `build exit: 1`, which was `grep`'s exit code (no
  lines matched the filter), not the build's — re-run with the exit code
  captured directly confirmed `go build exit code: 0`.
- `gofmt -l` over `cmd internal web tests scripts` — **2 unformatted files**
  (both sibling-modified, whitespace-only alignment nits; this tester did
  not touch them):
  - `internal/autorouter/autovariants_test.go` — `gofmt -d` line 544:
    `"gpt-4o":          {"Vision", "tools"}` alignment (one extra space).
  - `internal/provider/provider.go` — `gofmt -d` line 109:
    `Vision         bool` alignment (struct-field spacing).

## Notes / nits

1. **Misnamed fixture** (nit, not a behaviour failure):
   `tests/fixtures/mcp/error-best-combo-empty-task.json` passes
   `arguments:{}` and asserts `task: required`; it does not test an empty
   task string. The WT-18 empty-string variant is behaviour-correct (probe
   above) but unpinned.
2. **GAP-TRACK §12 staleness** (docs only, for the coordinator): WT-01–WT-08
   "missing pin" entries are outdated; the fixtures now exist and pass.
   The coordinator should update §12 rather than this tester (verify-only).
3. **Dirty tree at verification time:** `git status --porcelain` showed
   sibling modifications across the tree (e.g. `M CHECKLIST.md`,
   `M cmd/codvian-bench/main.go`, `M README.md`); this tester made no
   changes — the only file it created (`internal/mcp/wt_t08_probe_test.go`)
   was deleted, and the only file it wrote is this verdict file.
   Nothing was committed or pushed.
4. `go test` printed the benign warning `both GOPATH and GOROOT are the
   same directory (/home/hatch/go)`; it does not affect results.
