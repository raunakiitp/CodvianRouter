# T06 — M10 dashboard slices verdict (verify-only)

- **Tester:** T06 (verify-only; no code/doc edits except this file, no commits, no pushes)
- **Date (IST):** 2026-11-19 ~19:50 IST
- **Tree:** HEAD `6831302`, working tree dirty (all M10 work uncommitted, incl. all `dashboard_m10_*_test.go` files untracked). Nothing pushed.
- **Binary:** `go build -o /tmp/t06-router ./cmd/codvian-router` — green (warning only: GOPATH==GOROOT).
- **Live smoke:** `/tmp/t06-router --port 20261 serve` with `HOME=/tmp/t06-home` (fresh temp config/data dir, 0 providers, 0 keys at start). Bootstrap key issued via loopback-only `POST /admin/bootstrap` → `crk_XEk9…` (key used only for this smoke; temp data dir, server killed after).
- **Dashboard served:** `GET /dashboard` → 200, 796,534 bytes, sha256 `9cfa2d0a79f798e56020179188ed895b41310f18b43ce2695e920c0ae95d1f41` — byte-identical to `web/index.html`.
- **Targeted tests:** `go test ./internal/server/ -run 'TestDashboardM10_2|TestDashboardM10_3|TestDashboardM10_7|TestDashboardM10_8|TestDashboardM10_9|TestDashboardM10_10|TestDashboardM10_13' -count=1` → `ok github.com/raunakiitp/codvian-router/internal/server 41.106s` (all 33 tests in scope green; no full suite, no -race per instructions).

## Verdicts

| Slice | Verdict |
|---|---|
| M10-2 Providers | **PASS** |
| M10-3 Models | **PASS** |
| M10-8 Logs + Timeline | **PASS** |
| M10-9 Audit / Compliance | **PASS-WITH-NITS** |
| M10-10 Cache page | **PASS** (earlier recorded FAIL is stale — see note) |
| M10-13 MCP polish + A2A | **PASS** |
| Keys D/W/M nit-fix | **PASS** |

## M10-2 Providers — PASS

What was verified:
- Served HTML contains `#catalogCard`, `#catalogListBody`, `#catalogSearch`, `#catalogCount` and the "Free Tier Providers" group (`web/index.html:1092` comment, `:1102` card head with `#freeTierCount` + `#testAllFreeTierBtn`).
- Live feed `GET /api/catalog` → 200: `{"providers": [...], "total": 610}` — 610 providers, 162 with `has_free_tier: true`, 38 `tos_grey`. (Plan said 614 at plan time; the live total is now 610 — live data drift, not a hard-code. No `614` literal anywhere in the served HTML.)
- ToS-grey handling in JS: `web/index.html:4316` (label), `:4349`/`:4352` (`opt.disabled = true`), `:4384`, `:4566` (add blocked with honest error "is ToS-grey … cannot be added as a routing target").
- Free Tier group renders from the feed (`:4415` comment + shared renderer `:4371`).
- Tests green: `TestDashboardM10_2ProvidersSection`, `TestDashboardM10_2CatalogFeed`, `TestDashboardM10_2ProvidersFeed`.

Evidence: `web/index.html:1092-1102`, `:4316`, `:4349-4352`, `:4566`; `internal/server/dashboard_m10_2_test.go`.
Nits: none. Note: live catalog total moved 614 → 610 since the plan; any doc still saying 614 is stale (not this slice's text — no 614 in the dashboard).

## M10-3 Models — PASS

What was verified:
- Served HTML `web/index.html:1164` (`#view-models`): table with `#modelBody`, headers Model / Provider / Type / Context / Input / 1M / Output / 1M / **Price source** / Requests / Fit / Health / Status; `#modelRefreshBtn`, `#modelSyncStatus` ("Live price/context sync"), honest empty state `#modelEmpty`: "No models yet — connect a provider first." + Add-a-provider button; Prices link present.
- Live feed `GET /dashboard/models` → 200: `{"models":[],"total":0,"deprecated_hidden":0}` — honest empty at 0 providers.
- Free-tier / key-link columns correctly ABSENT per B9 verify-first: no `has_free`/`key_url`/`key_creation_url`/`signup_url` field exists in the models feed (grep over `internal/server/models*.go`, `dashboard*.go`: 0 hits); `dashboard_m10_3_test.go:7-8,103-106,121-127` pins their absence in the Models section. DoD allows the columns to be deferred to B9 when the feed lacks them.
- Tests green: `TestDashboardM10_3`, `TestDashboardM10_3FeedsExist`.

Evidence: `web/index.html:1164`, table head ~`:1224` (`<th>Price source</th>`), `#modelBody`, `#modelEmpty`; `internal/server/dashboard_m10_3_test.go:7-8`.
Nits: none.

## M10-8 Logs + Timeline — PASS

What was verified:
- Served HTML `web/index.html:1947` (`#view-logs`): view switcher `#logViewSeg` with `data-logview="all|proxy|console|timeline"`; panels `#logViewAll` (`#logBody`, `#logEmpty`: "No requests yet — send your first request and it appears here."), `#logViewProxy` (`#logProxyBody`), `#logViewConsole` (`#logConsoleLines`, `#consoleKey` + `#consoleLoadBtn` — admin key typed fresh, never stored), `#logViewTimeline` (`#logTimelineList` + latest-route-trace card `#logTrace`); label "Proxy, Console and Timeline are filtered views of the same request log — one feed, not separate data sources."
- Live feeds: `GET /dashboard/logs` → 200, `GET /dashboard/trace` → 200, `GET /api/logs` → 200 with key (401 without — honest auth), real redacted server lines (`{"captured":true,"redaction":"always-on","source":"server logger",...}`).
- Tests green: `TestDashboardM10_8LogsViewsRender`, `TestDashboardM10_8FilteredViewsAreSubsetsOfOneFeed` (each view a subset of the one feed).

Evidence: `web/index.html:1947-2015` (Logs section); `internal/server/dashboard_m10_8_test.go:55-95`.
Nits: none. (The plan's "Activity timeline" is built as the Timeline view + `#logTrace` card, pinned by the passing test — no separate "Activity" section id exists, and none is required by the test/DoD.)

## M10-9 Audit / Compliance — PASS-WITH-NITS

What was verified:
- Served HTML: top-level nav `data-view="audit"` (`web/index.html:659`), section `#view-audit` (~`:3500`); `#m10_9RefreshBtn`, key card `#m10_9KeyCard` ("Audit needs an admin-scoped gateway key…"), Guard denials KPIs (`#m10_9DenialsTotalNum`, `#m10_9LocalOnly`, `#m10_9GuardrailBlocked`, `#m10_9SecRefTotal`), MCP audit table (`#m10_9McpBody`, 7 columns) with Newer/Older pager (`#m10_9McpPrevBtn` / `#m10_9McpNextBtn`, `#m10_9McpPage`), and the read-only/retype sentence verbatim at `web/index.html:3584` ("The feed offers no actor/action filter and no pagination for guard denials, key lifecycle, or management posts — only the MCP entries above page (50 per page, newest first). This view is read-only… retype it in the sidebar before Refresh or paging…"). (Plan cited :3586 audit-time / :3582 re-verified; current tree has it at :3584 — line drift only, sentence identical.)
- Live feed `GET /dashboard/audit` → 200 with key (401 without): `guard_denials.available: true` (live counters, total 0), `mcp_audit.available: true` (cap 10000, dropped 0), `key_lifecycle.available: false`, `management_posts.available: false` — both with honest notes explaining no structured store exists yet and rows are never reconstructed by parsing log text. No prompts/keys/message content in any row (identifiers + counts only).
- Tests green: `TestDashboardM10_9AuditRenders`, `TestDashboardM10_9FeedValuesEqualSmoke`, `TestDashboardM10_9PureHelpersUnderNode`.

Evidence: `web/index.html:3519-3584`; `internal/server/dashboard_m10_9_test.go`.
Nits (non-blocking, honest incompleteness already disclosed in the feed itself):
- N1: `key_lifecycle` and `management_posts` categories are `available: false` — the Audit page cannot list key events or management POSTs until a structured store lands (B4 follow-up, backend work, not dashboard). The UI discloses this via the feed notes; nothing is faked. Verdict stays PASS-WITH-NITS rather than PASS because two of the four promised audit categories are empty by design-limitation.

## M10-10 Cache page — PASS

What was verified — this CONTRADICTS the earlier recorded "M10-10 FAIL — Cache nested in Settings, Cache page invisible" (MEMORY.md status line). That FAIL is stale relative to the current tree:
- Top-level nav: `<button class="nav-item" data-view="cache">` at `web/index.html:631`.
- Top-level section: `<section class="view" id="view-cache">` at `web/index.html:2901-2972` — a sibling of Settings, not nested: Settings spans `:2481-2898`, Cache spans `:2901-2972` (structural check: `cache nested in settings: False`). The only "cache" mention inside Settings is the "Prompt-cache pinning" read-only status row (`:2504-2505`, `#stCachePin`), not a Cache page.
- Server routes exist and are live: `GET /api/cache/stats` (guard `internal/server/guard.go:219`, registered `internal/server/server.go:1502`) → 200: exact/semantic/totals all 0 at rest + honest `savings_note` ("savings unavailable: no savings recorder is wired…"); `DELETE /api/cache` (guard `:223`, registered `server.go:1506`) → 401 without key, 200 with admin key: `{"evicted":{"exact":0,"reasoning":0,"semantic":0},"flushed":{"exact":true,"reasoning":false,"semantic":true},"total_evicted":0,"notes":{"reasoning":"reasoning cache not flushed: the serving engine does not expose a reasoning flush"}}`.
- Page content: KPIs Cache hits/misses (`:2926-2931`), Tokens saved (`:2951-2955`, `#cachePageSavingsTokens`/`#cachePageSavingsCost`), "Cache-write cost (estimate)" card (`:2962`) with the 1.25×–2.5× band labelled an estimate and Anthropic 5m 1.25× / 1h 2× tiers noted as backend per-tier accounting (`:2964`); JS `loadCachePage()` (`:9217`) fetches `/api/cache/stats` and flushes via `DELETE /api/cache` (`:9285`), wired on view-open (`:3675`).
- Tests green: `TestDashboardM10_10CacheRenders`, `TestDashboardM10_10CacheNotNestedInSettings`, `TestDashboardM10_10StatsEqualFeed`, `TestDashboardM10_10FlushRoundTrip`, `TestDashboardM10_10MemoryOnlySavingsNote`, `TestDashboardM10_10PureHelpersUnderNode`.

Evidence: `web/index.html:631`, `:2901-2972`, `:2962-2964`; `internal/server/guard.go:219,223`; `internal/server/server.go:1502,1506`; `internal/server/dashboard_m10_10_test.go`.
Nits: none.

## M10-13 MCP polish + A2A — PASS

What was verified:
- MCP section `web/index.html:1775` (`#view-mcp`): tools table with "Required scopes" column; live feed `GET /dashboard/mcp` → 200 with tools carrying `scopes` (e.g. `router_status` → `["mcp:read:status"]`, `provider_list` → `["mcp:read:providers"]`); `GET /api/mcp/audit` → 200.
- Transport toggle round-trip LIVE: `POST /api/mcp/status {"transport":"sse","enabled":true}` → 200 `{"sse":{"enabled":true,...}}`; `{"enabled":false}` → 200 `{"sse":{"enabled":false,...}}`; no key → 401; `{"transport":"stdio"}` → 400 "unknown transport: want sse or streamable" (honest validation; dashboard JS `setMCPTransport(transport, enabled)` at `:12273` sends `{"transport","enabled"}`).
- M9.7 nit N1 FIXED in-tree: `internal/server/mcp.go:237` now calls `mcpHTTPStateFor(s).counts()` (live session counts) instead of hardcoding `Sessions: 0`; live `GET /api/mcp/status` returns `{"sse":{"enabled":false,"sessions":0},"streamable":{"enabled":false,"sessions":0}}` — 0 is the true count with no live sessions. (Could not create a live MCP session via curl to prove a nonzero count; at-rest correctness verified.)
- A2A section `#view-a2a` with `#a2aPill`, `#a2aTasks`, `#a2aSkills`, `#a2aProtoVer`, `#a2aRefreshBtn`; live feed `GET /api/a2a/status` → 200 with key: `{"enabled":false,"tasks_by_state":{"cancelled":0,"completed":0,"failed":0,"submitted":0,"working":0},"skills":["smart-routing","quota-management","provider-discovery","cost-analysis","health-report","list-capabilities"],"protocol_version":"2025-03-26"}` — exactly the §3.2 contract (5-state all-zero, 6 skills, 2025-03-26). Disabled empty state honest: `web/index.html:12423` ("A2A is disabled — no tasks are accepted and no task counts are live… Enable it with `a2a_enabled` in config, then restart.").
- Negatives in served HTML: "Playground" = 0 occurrences; "/api/a2a/tasks" = 0 occurrences; "8080" literal = 0 occurrences.
- Tests green: `TestDashboardM10_13A2ASectionRenders`, `TestDashboardM10_13A2AFeedDisabled`, `TestDashboardM10_13A2AFeedEnabledLiveCounts`, `TestDashboardM10_13A2AInlineScriptSyntax`, `TestDashboardM10_13MCPSectionRenders`, `TestDashboardM10_13MCPFeedsAnswerKeyless`, `TestDashboardM10_13MCPTransportToggleNeedsAdminKey`, `TestDashboardM10_13MCPFeedFailureHonest`, `TestDashboardM10_13MCPInlineScriptSyntax`.

Evidence: `web/index.html:1775`, `:12273`, `:12423`; `internal/server/mcp.go:230-240`; `internal/server/dashboard_m10_13_test.go`, `dashboard_m10_13_mcp_test.go`.
Nits: none.

## Keys D/W/M nit-fix — PASS

What was verified:
- Served HTML Keys manage form (`web/index.html:1908-1915`): Daily / Weekly / Monthly `$ cap` inputs (`#keyManageDaily`, `#keyManageWeekly`, `#keyManageMonthly`) + help text at `:1936`: "Daily/weekly/monthly $ caps save separately (Save $ caps) and apply immediately; empty leaves a stored cap unchanged. A key with no rolling 5 h budget yet cannot take D/W/M caps — set it first under Costs → Per-key rolling budgets." (Correct path — Costs, not Usage — per the M10-17 gate decision (c).)
- Live round-trip against the smoke binary (bootstrap key `gk_19cbe13b313025e3`): `POST /api/usage/budget {"apiKeyId":…,"limitUsd":0}` → 200 (creates the 5 h row); `POST /api/usage/budget {"apiKeyId":…,"dailyLimitUsd":2.5,"weeklyLimitUsd":10,"monthlyLimitUsd":50}` → 200 with `daily_limit_usd: 2.5, weekly_limit_usd: 10, monthly_limit_usd: 50`; `GET /dashboard/key-budgets` → 200 shows the same caps (`daily.limit_usd: 2.5`, `weekly.limit_usd: 10`, `has_budget: true`, 5 h `limit_usd: 0` untouched).
- Tests green: `TestDashboardM10_7DwmLimitsRoundTrip`, `TestDashboardM10_7DwmLimitsFreshKeyRequiresLimitUsd`, `TestDashboardM10_7PrefillKeyManageForm`, `TestDashboardM10_7DisableRoundTrip`, `TestDashboardM10_7RevokeRoundTrip`, `TestDashboardM10_7RotateRoundTrip`, `TestDashboardM10_7CreateStillMissing`, `TestDashboardM10_7ServedHTML`, `TestDashboardM10_7InlineScriptSyntax`.

Evidence: `web/index.html:1908-1936`; `internal/server/dashboard_m10_7_test.go:308-353` (nit 4: key-budgets feed carries the caps the prefill consumes).
Nits: none. Note: "+ New router key" create still honestly unwired (B1 pending) — out of scope for this nit-fix, disclosed by `TestDashboardM10_7CreateStillMissing`.

## Cross-cutting checks

- No fake/sample data found in the verified sections; all empty states are honest one-liners from live feeds.
- `node --check` on the dashboard inline script is covered by the passing `*InlineScriptSyntax` tests (M10-9, M10-10, M10-13); not re-run separately.
- The "· saved" string in the served HTML occurs only in legitimate live-value contexts (e.g. "Quota hits saved", "Tokens saved", "Compression (…) · saved N", "Combo X saved" after a real save) — no fake Settings "· saved" toast was found in the verified slices. (Settings fake-handler removal is M10-14's slice, not verified here.)
- LOG.md contains no M10 slice review entries — the task's "review PASS/PASS-WITH-NITS per LOG.md" could not be confirmed from LOG.md; verdicts above are from independent re-verification (live binary + tests + file:line), not from LOG.md.
- Server killed after the smoke; temp dirs `/tmp/t06-home`, `/tmp/t06-data` and binary `/tmp/t06-router` left for the coordinator; no repo files touched except this verdict file.

## Failures

None blocking. One PASS-WITH-NITS (M10-9: key-lifecycle + management-posts audit categories `available: false` — backend structured-store work still pending, honestly disclosed in the feed).
