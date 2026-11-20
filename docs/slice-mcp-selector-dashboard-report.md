# Slice report — MCP selector mode dashboard card (CHECKLIST items 141–142)

Date: 2026-11-18 (IST). Scope: item 142 (dashboard card); item 141 (backend)
is the feed the card reads, so it was audited as part of this slice.
No push to GitHub was made; all work is local in the worktree.

## Row 142 spec (quoted verbatim from CHECKLIST.md)

> MCP selector mode (dashboard, same slice): "MCP selector mode" card in the
> MCP & CLI section in the approved design system (dark pure black / light
> pure white, borderless 26–28px cards, dark-lime palette, no Playground) —
> mode pill (Selector/Direct/Off — no MCP servers), loop cap,
> selector-vs-direct exposure line (2 meta-tools vs all N discovered),
> per-server table (name, up/down + last error, tool count) and live
> tool-call counts (get calls, executions, per-tool counts, loop runs,
> follow-up calls, capped stops), all from the extended
> `GET /dashboard/mcp` feed (Tier 3, auth-exempt; no keys, base URLs or
> command lines); honest empty states when no servers are configured and
> before any tool has run; refreshed on open + every 5 s + Refresh button;
> `node --check` on the dashboard JS clean

## Verdict: already complete — audit + live verification, zero code changes

Every element of row 142 was already present and correct. This slice made
**no edits to any file** (no Go, no HTML/JS). The work was audit + live
verification against a real binary.

## What was already present (line-by-line audit)

### Backend feed — `internal/server/mcp.go` (`handleMCP`, `mcpResponse`)

- `mode` (`selector`/`direct`/`off`), `max_iterations` (loop cap, 0 when
  off), `servers[]` (`name`, `ok`, `tools`, `last_error`), `selector_tools`
  (always the two meta-tools), `direct_tools[]` + `direct_tool_count`
  (every discovered tool, qualified `server__tool` name), `tool_calls`
  (`get_calls`, `execute_calls`, `execute_by_tool`, `loop_runs`,
  `loop_iterations`, `loop_capped`). All populated from
  `Registry.Snapshot()` in `internal/mcptools/mcptools.go:637–670`
  (`Snapshot`/`Stats`), never invented — with no registry wired the mode
  is `"off"` and lists/counters are empty/zero.
- Tier 3, auth-exempt: `internal/server/guard.go:96` —
  `{Method: "GET", Path: "/dashboard/mcp", Tier: TierManagement,
  AuthExempt: true}`; registered at `internal/server/server.go:797`.
- No secrets in the payload: no keys, no base URLs, no MCP server command
  lines. The only `command` field is the router's own launch string
  `"codvian-router mcp"`. `mcpServerFeed` carries name/health/tool count/
  last error only (see the comment at `mcp.go:22–27`).
- Wiring (item 141, verified by reading, not changed): `runServe` in
  `cmd/codvian-router/main.go:1331–1355` builds the registry from the
  `mcp` config block (`mcptools.NewStdioServer` per server,
  `mcptools.New(..., EffectiveMode(), EffectiveMaxIterations())`,
  `Discover()` at startup, down servers logged not fatal) and wires it
  with `server.WithMCPTools` (`internal/server/server.go:555–564`).

### Dashboard card — `web/index.html`

- Card: "MCP selector mode", `web/index.html:1528–1547`, inside the
  MCP & CLI section (`#view-mcp`), using the standard `.card` class.
- Mode pill `#mcpModePill` (JS `index.html:4823–4825`): `Selector` /
  `Direct` / `Off — no MCP servers`.
- Loop cap + exposure line `#mcpSelectorDetail` (`index.html:4826–4848`):
  selector mode renders "injects exactly **2** meta-tools —
  `mcp_tools_get`, `mcp_tools_execute`"; direct mode renders "injects
  all **N** discovered tool schemas"; both render the loop cap as
  "**N** follow-up model calls per request".
- Per-server table `#mcpServers` (`index.html:1540–1543`, rows built at
  `index.html:4850–4860`): Server (name), Status (`Up`, or
  `Down — <last_error>`), Tools (count). Empty state `#mcpServersEmpty`:
  "No MCP servers configured — add a `mcp.servers` entry…".
- Live counts `#mcpSelectorCounts` (`index.html:4861–4871`): get calls,
  executions, agent loops, follow-up model calls, stopped-at-cap, plus a
  per-tool `name × count` list. Before any tool has run:
  "No tool has been executed yet — counters stay at zero until the first
  real call."
- Off-mode empty state (`index.html:4830–4832`): "Selector mode is off —
  no MCP servers are configured, so no MCP tools are injected into any
  request."
- Refresh: on open (`index.html:1934`, `if(view === "mcp"){ loadMCP(); }`),
  every 5 s while the section is open (`index.html:4878–4881`), and the
  Refresh button `#mcpRefreshBtn` (`index.html:1514`, wired at
  `index.html:4874–4875`). Fetch failures leave the last values / honest
  empty states (`.catch` at `index.html:4872`).
- Design system: `.card` is `border:0; border-radius:var(--radius)`
  with `--radius:26px` (`index.html:215`, `:49`); `--surface:#FFFFFF`
  light / `#000000` dark (`index.html:19`, `:59`); dark-lime palette
  (`index.html:33`, `:73`, `:293`). No "Playground" string anywhere in
  `web/index.html` (grep: zero matches).

## What was missing

Nothing. No missing element, no wrong rendering, no invented count found.
One cosmetic inconsistency was noted and deliberately left alone (it is
not a defect — the JS already handles it): with no registry wired the
feed emits `"execute_by_tool": null` (Go nil map), while with a registry
it emits `{}`. The dashboard reads it as `tc.execute_by_tool || {}`,
so both render identically.

## Live verification (real binary, isolated HOME, no providers)

Built with `go build -o /tmp/codvian-router-test ./cmd/codvian-router`
and run with isolated `HOME`/`data_dir` on loopback only.

- **Test A — no `mcp` block** (`GET /dashboard/mcp`, no key — proves
  auth-exempt, HTTP 200): `mode:"off"`, `max_iterations:0`,
  `servers:[]`, `selector_tools:["mcp_tools_get","mcp_tools_execute"]`,
  `direct_tools:[]`, `direct_tool_count:0`, all `tool_calls` zero.
  `GET /dashboard` HTML contains the card (`MCP selector mode`,
  `#mcpModePill`, `#mcpSelectorDetail`, `#mcpSelectorCounts`,
  `#mcpServersEmpty`).
- **Test B — selector mode, cap 7, one real + one broken server.**
  A tiny fake stdio MCP server (`/tmp/mcp-test/fake-mcp.py`,
  newline-delimited JSON-RPC 2.0, `initialize`/`tools/list`/`tools/call`,
  exposing tools `echo` and `add`) plus a server pointing at
  `/nonexistent/binary-xyz`. Feed answered:
  `mode:"selector"`, `max_iterations:7`,
  `servers:[{"name":"fake-tools","ok":true,"tools":2},
  {"name":"broken-srv","ok":false,"tools":0,
  "last_error":"mcp server \"broken-srv\": starting
  \"/nonexistent/binary-xyz\": fork/exec /nonexistent/binary-xyz: no
  such file or directory"}]`,
  `selector_tools:["mcp_tools_get","mcp_tools_execute"]`,
  `direct_tools:[fake-tools__echo, fake-tools__add]` (qualified names,
  descriptions, input schemas), `direct_tool_count:2`, all `tool_calls`
  honestly zero (no tool had been executed yet). Startup log:
  `mcp selector: mode=selector servers=2 up=1 down=1 tools=2 loop cap=7
  (selector injects 2 meta-tools; direct would inject all 2)`.
  (First fake-server attempt failed with `initialize: EOF` because the
  script used `for line in sys.stdin`, which buffers on a pipe; fixed
  by reading with `sys.stdin.readline()` — a test-harness bug, not a
  router bug.)

## Checks run

- `go build ./...` — **PASS** (go1.27.4; only a pre-existing
  GOPATH/GOROOT warning from this VM's Go install).
- `node --check` on the extracted inline dashboard JS (`/tmp/dash.js`,
  the single inline `<script>` in `web/index.html`) — **PASS**.
- `go test` — **not run**, per slice instructions (one full gate runs
  at the end).
- No `CHECKLIST.md` edit (coordinator flips rows after the gate).

## Files changed

None. Deliverable is this report only:
`docs/slice-mcp-selector-dashboard-report.md`.

## Recommended CHECKLIST verdict

- **Item 141 (MCP selector mode, backend): Built ✅, Verified ✅** —
  feed fields, registry wiring, and live selector behaviour confirmed
  against a real binary with a real stdio MCP server (up + down cases).
- **Item 142 (MCP selector mode, dashboard): Built ✅, Verified ✅** —
  every spec element present in the card and rendered from the live
  feed; `node --check` clean; refresh paths (open / 5 s / button) and
  honest empty states confirmed in code and in the live feed responses.
