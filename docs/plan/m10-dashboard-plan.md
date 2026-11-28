# M10 — Dashboard build plan

- **Status:** PLAN ONLY. No code was written for this doc. Nothing was pushed.
- **Date:** 2026-11-19. **Worktree:** `~/workspace/codvian-router/`.
- **Governs:** Master Prompt v1 (M10 follows M9.10; M12 only after M1–M11 complete and verified).
- **Sources read for this plan (all read-only):**
  - `internal parity tracker` Area 8b (139 rows: FULL 7 / PARTIAL 36 / MISSING 91 / WRONG 4; strict 5.0%, weighted 18.0%) and Area 9 dashboard MCP/A2A rows.
  - `internal plan` M10 row ("Dashboard: real, complete, honest (backend first!)" — exit gate: `scripts/audit-fake.sh` returns ZERO UI controls without live backend; Playwright smoke test clicks every control).
  - `web/index.html` — 8,367 lines, measured 2026-11-19 (see §2).
  - `the local reference clone/src/app/(dashboard)/` — study only; no code copied.
  - `internal/server/a2a_status.go` (header comment + handler) and `internal/server/a2a_dashboard_test.go` — the A2A status contract M9.9 codes against.
  - `internal/server/server.go` + `internal/server/guard.go` — every endpoint claimed in §3 was verified as registered there, or marked MISSING.
- **In flight, do not collide:** the M9.9 dashboard builder owns the A2A region of `web/index.html` (`view-a2a`, lines 1976–2032, ids `a2aPill`, `a2aDetail`, `a2aTasks`, `a2aTasksEmpty`, `a2aSkills`, `a2aSkillsEmpty`, `a2aProtoVer`, `a2aRefreshBtn`). M10-13 touches that region only after M9.9 hands it over, and never renames those ids (they are pinned by `internal/server/a2a_dashboard_test.go`).
- **Design rules (Raunak, 2026-11-18 voice calls — non-negotiable):** local-operator console, NOT a hosted-provider storefront; dark pure black `#000000` / light pure white `#FFFFFF`, uniform surfaces, borderless cards 26–28px corners, Plus Jakarta Sans / JetBrains Mono / Source Serif 4 headings, dark-lime palette, jewel-tone 3D status capsules, pill buttons; tables not paragraphs; catalog visible as addable groups even at 0 providers; dropdowns styled per design system (pill, dark-lime), verified open AND closed; Playground stays REMOVED entirely (`"Playground"` = 0 occurrences in `web/index.html`, measured now); live data only, honest empty states; no fake "· saved" toasts.
- **Honesty rules:** a control that is not wired to a real endpoint is removed, not left with a toast. A number the backend does not provide is shown as unknown, never invented. Every PARITY flip needs file:line + test evidence.

## How to read this plan

- **Slice ids** are `M10-0` … `M10-17` (18 slices). **Backend-enabler ids** are `B1` … `B9` (§4) — small Go slices that must land before the dashboard slice that needs them can be DONE. A dashboard slice that needs a `B` slice is **BLOCKED-in-part**: it may build everything its existing endpoints support, but it is not DONE until the `B` slice lands, and the blocked sub-scope must not be faked in the meantime.
- **Endpoint verification:** §3 lists every endpoint the slices read, with its registration line. "EXISTS" means registered in `internal/server/server.go` (or via `registerWebhooksRoutes`) AND classified in `internal/server/guard.go`. Anything else is MISSING and the slice that needs it is BLOCKED.
- **Parallelism constraint (read this first):** the dashboard is ONE file, `web/index.html` (8,367 lines). Truly disjoint *files* are impossible for dashboard slices. Slices are therefore disjoint by **region** (one nav section + its JS functions, named by element id and function prefix — see §5.1) plus disjoint **test files** (`internal/server/dashboard_m10_<n>_test.go`). Two slices never share a section, an element id, or a JS function name. The coordinator merges in numeric slice order; M10-0 (nav slots + auth headers + tokens) lands first and every later slice builds on it. A slice that edits outside its region fails its own Definition-of-Done.

---

## 1. the sector-typical real dashboard inventory

Route tree: `the local reference clone/src/app/(dashboard)/dashboard/` (Next.js App Router; each route is a `page.tsx`, usually a thin shell over a `*Client.tsx`). **No central nav manifest was found** — `src/app/(dashboard)/layout.tsx` is a 29-line wrapper and nav links are distributed across components — so this inventory is from the route tree itself, not from a nav config. Line counts are measured where stated; unmeasured routes are listed with path only.

### 1a. Core operator pages — in scope for M10

| the sector baseline route | What it is (evidence) | File |
|---|---|---|
| Home (`/dashboard`) | Local-operator console home: 4-step quick-start, provider topology, recent requests, update/readiness banners, sponsor banners | `dashboard/page.tsx`, `dashboard/HomePageClient.tsx`, `dashboard/HomeProviderTopologySection.tsx`, `dashboard/FirstRunReadinessCard.tsx` |
| Providers | Provider list as addable groups incl. free-tier grouping; per-provider connections, health, key management | `dashboard/providers/page.tsx` (1,984 lines) |
| Models | Models as a table (pricing, context, capabilities), not prose | `dashboard/models/page.tsx` (1,134 lines) |
| Costs | Cost overview KPIs (Spend 1D/7D/30D side-by-side, requests, avg cost/req), Monthly Forecast, Cost Explorer (dimension filter, top 50, CSV/JSON export) | `dashboard/costs/page.tsx` (7 lines → `CostOverviewTab.tsx`); sub-routes `costs/budget`, `costs/quota-share`, `costs/teams`, `costs/pricing` |
| Logs | Request-logs table (all columns + Reasoning/Cached/Saved) | `dashboard/logs/page.tsx` (250 lines); sub-routes `logs/proxy`, `logs/console`, `logs/timeline`, `logs/activity`, `CompressionLogTab.tsx` |
| Analytics | Auto-routing / compression / diversity analytics + Combo Health (quota risk, forecast, Gini skew, autopilot, scoring inspector) | `dashboard/analytics/page.tsx` (139 lines); `dashboard/analytics/ComboHealthTab.tsx` |
| Combos | Named-combo control centre: CRUD + builder + per-capability endpoint tabs (chat/search/embeddings/rerank/images/video/music/OCR/audio) + duplicate/import/export/test/enable | `dashboard/combos/page.tsx` (5,435 lines); `dashboard/combos/ComboControlCenterClient.tsx`; `dashboard/auto-combo/page.tsx` (5 lines) |
| API Manager (keys) | Gateway-key list, create key, regenerate (confirm dialog), scopes, per-key budgets | `dashboard/api-manager/page.tsx` (5 lines → `ApiManagerPageClient.tsx`) |
| Quota / Limits | Quota-share pools, per-key daily/weekly/monthly $ limits, rolling 5h limit | `dashboard/quota/page.tsx` (43 lines); `dashboard/limits/page.tsx` (5 lines); `dashboard/costs/quota-share/` |
| Cache | Cache stats, tiers, invalidation | `dashboard/cache/page.tsx` (930 lines) |
| Resilience | Breakers, cooldowns, per-model cooldowns, reset | `dashboard/resilience/` (no `page.tsx`; sub-routes `connections`, `cooldowns`, `layout.tsx`) |
| Webhooks | Webhook list/create/edit/delete, event delivery log | `dashboard/webhooks/page.tsx` (5 lines → `WebhooksPageClient.tsx`) |
| MCP | MCP page + Endpoint tabs, transport toggles, tool list with scopes ("110 tools, 33 scopes"), MCP Audit page | `dashboard/mcp/page.tsx` (278 lines) |
| A2A | A2A status/audit page | `dashboard/a2a/page.tsx` (166 lines) |
| Audit | Compliance/audit view (guard denials, key lifecycle, management actions) | `dashboard/audit/page.tsx` (7 lines → `ComplianceTab`) |
| Activity | Activity feed / timeline of operator-visible events | `dashboard/activity/page.tsx` (7 lines → `ActivityFeedClient.tsx`) |
| Health | Health history, provider health, readiness | `dashboard/health/page.tsx` (1,179 lines) |
| Batch | Batch jobs list/create/cancel | `dashboard/batch/page.tsx` (350 lines) |
| Settings | General, Routing, Security (incl. Authz section + PII card — **guardrails live here, not on their own page**), Appearance, Cache, Resilience, Pricing, AI, Advanced, Feature flags, Access tokens, Sidebar, Modality bridge | `dashboard/settings/page.tsx` (33 lines); `dashboard/settings/{general,routing,security,appearance,cache,resilience,pricing,ai,advanced,feature-flags,access-tokens,sidebar,modality-bridge}/` |
| Plugins | Plugin marketplace + installs | `dashboard/plugins/page.tsx` (156 lines) |
| Free tiers / Provider stats / Discovery | Free-tier provider rankings, per-provider stats, provider discovery | `dashboard/free-tiers/page.tsx` (15 lines); `dashboard/free-provider-rankings/`; `dashboard/provider-stats/page.tsx` (524 lines); `dashboard/discovery/page.tsx` (5 lines) |
| API endpoints / Tools | Endpoint inventory, tool/traffic inspector | `dashboard/api-endpoints/page.tsx` (20 lines); `dashboard/endpoint/page.tsx` (5 lines); `dashboard/tools/` (no `page.tsx`; `agent-bridge`, `traffic-inspector`) |
| Usage | Redirects to `/dashboard/logs` | `dashboard/usage/page.tsx` (5 lines, `redirect("/dashboard/logs")`) |
| Conversations | Chat/conversation history | `dashboard/conversations/page.tsx` (1,032 lines) |
| Memory / Search tools / Tokens / Profile | Memory store UI, search-tool config, token accounting, operator profile | `dashboard/memory/page.tsx` (83 lines); `dashboard/search-tools/page.tsx` (5 lines); `dashboard/tokens/page.tsx` (606 lines); `dashboard/profile/page.tsx` (405 lines) |

### 1b. Out of scope — deliberate divergences (do NOT build in M10)

| the sector baseline route | Why out of scope |
|---|---|
| Playground (`dashboard/playground/page.tsx`, 15 lines → `PlaygroundStudio`) | **Removed by Raunak's decision.** No chat-playground in a local router console; a playground is a spend surface ("a tool call can never spend quota" logic applies to the dashboard too). `M10-0` asserts zero occurrences. |
| Gamification, Leaderboard, Radar, Translator, Kimi/VSCode sponsor banners, Changelog, `cli-code`, `cli-agents`, `cloud-agents`, `acp-agents`, `agent-skills`, `legacy skill pack`, `conductor`, `orchestration`, `relay`, `chaos`, `system` (1proxy/mitm-proxy), `runtime`, `media-providers`, `context`, `onboarding` | Not router-core: engagement features, hosted/cloud agent surfaces, proxy/MITM tooling, or onboarding flows for a different product shape. Recorded here so a builder does not "discover" them mid-slice and add them. `chaos` in particular is backend chaos-harness territory (M4), not a dashboard page. |
| Login / OIDC / "Require login / password change" (Settings → Security) | **DIVERGENCE (parity tracker Area 8b):** the sector baseline has password/OIDC login; we have the gateway-key auth model + loopback binding instead. No dashboard login page in M10. If dashboard auth ever lands it is a backend slice, not M10. |

### 1c. Structural facts about the sector-typical dashboard that shape M10

1. **Home is a 4-step quick-start + topology, not a KPI wall.** The operator lands on "what do I do first", then providers. Our Overview's KPI row + ~11 detail cards (item 295 caveat: KPI row is exactly 4, detail cards below were kept because other verified items depend on them) is the thing to reconcile in M10-1 — list which detail cards survive and why, don't just keep them.
2. **Providers and Models are the two big tables** (1,984 and 1,134 lines). Models is a table, never paragraphs.
3. **Costs is a real product surface** (overview + explorer + budget + quota-share + teams + pricing sub-routes), not one "Usage & Cost" page. Our single Usage view is the thinnest parity area after combos.
4. **Settings is a sectioned page** (general/routing/security/…); Authz and the PII card live under Settings → Security. There is **no Guardrails page and no Route-guards page**.
5. **Usage redirects to Logs.** Our separate Usage view should converge toward Costs, not duplicate Logs.

---

## 2. Our current dashboard inventory vs the sector baseline

Measured 2026-11-19 in `web/index.html` (8,367 lines; `<script>` starts line 2573). **`<p>` count is 240 lines containing `<p` (251 occurrences: 178 in static HTML + 73 in JS-generated markup)** — PARITY's "174 (109 static + 65 JS)" is stale; the count grew during M9.9/sprint-19 work. Do not quote 174. **`"Playground"` = 0 occurrences** — the removal holds. **13 `<select>` elements** (11 static + 2 JS-built); per PARITY's last dropdown inventory 4 were still native-arrow (model provider filter, model category filter, per-provider add-key role, intent-tier effort) and `#11` Listen-address is styled but fake.

### 2a. Our sections (nav order, `web/index.html:537-664`)

| # | Our section | Lines | Status vs the sector baseline | Notes |
|---|---|---|---|---|
| 1 | Overview | 665–917 | PARTIAL | KPI row exactly 4 (item 295 DONE); ~11 detail cards below exceed the item-295 4-card cap — M10-1 reconciles. Spend is 24h/7d/30d range-segmented, no side-by-side 1D/7D/30D, no avg cost/req. |
| 2 | Providers | 918–1033 | PARTIAL → near-have | Catalog-as-addable-groups landed (item 291: 614 live from `GET /api/catalog`, configured pinned on top, ToS-grey rows disabled Add, search). M10-2 verifies + closes gaps (Free Tier group). |
| 3 | Models | 1034–1098 | PARTIAL | Table rows only (item 292 DONE). Price-source column + `models.dev` synced marker landed (item 180). Free-tier marking + key-creation links not surfaced (B9 verify-first). |
| 4 | Auto-router | 1099–1202 | PARTIAL | 6 virtual models (auto, auto/fast, auto/cheap, auto/coding, auto/code, auto/reasoning + intent/*); no router-strategy picker, no sla/nadir/lkgp. Reads `GET /dashboard/auto-route`, `/dashboard/auto-route/last`. |
| 5 | Routing strategies | 1203–1272 | PARTIAL | Picker offers 7 names incl. wrong `code` (backend `isStrategyName` accepts only balanced, cheapest, cheap, fastest, fast, most-reliable, reliable, least-latency, latency, weighted); 18/22 the sector baseline strategies MISSING at backend level (M3 territory, not M10 — M10 renders exactly what the backend accepts). |
| 6 | Cooldown | 1273–1339 | PARTIAL | Global + per-chain modes live (`GET/POST /dashboard/cooldown`). No per-model cooldowns, no breaker reset (B6). |
| 7 | Request coalescing | 1340–1388 | HAVE (narrow) | Live single-flight counters from `GET /dashboard/coalescing`. the sector baseline has no direct equivalent page; keep — it is a real operator signal, not a storefront extra. |
| 8 | Concurrency & TCP Fast Open | 1389–1431 | HAVE (narrow) | Live limiter state from `GET /dashboard/concurrency`. Same justification as coalescing. |
| 9 | Connection pools | 1432–1475 | HAVE (narrow) | Live per-(provider,key) pool state from `GET /dashboard/pools`. |
| 10 | Route guards | 1476–1619 | OURS-ONLY → REMOVE as page (§6) | Live Tier 1/2/3 tables from `GET /dashboard/guard` — real and correct, but the sector baseline puts Authz under Settings → Security. Fold into Settings; keep the feed. |
| 11 | Guardrails | 1620–1787 | OURS-ONLY → REMOVE as page (§6) | 3 kind toggles (PII/Secrets/Injection, ids `grTogglePII/Secrets/Injection`) wired to `GET/POST /dashboard/guardrails` (item 296). the sector baseline model is backend-only + PII card in Security. Fold into Settings → Security; keep the feed. |
| 12 | Plugins | 1788–1863 | PARTIAL | Live registry snapshot from `GET /dashboard/plugins`, honest zero state. No install UI — **deliberate**: `POST /admin/plugin/load` is 501 by design (loading arbitrary code = arbitrary code execution). Never add install UI in M10. |
| 13 | MCP & CLI | 1864–1975 | PARTIAL | `GET /dashboard/mcp` + audit view (`GET /api/mcp/audit`) + transport toggles (`POST /api/mcp/status`). 24 tools / 11 scopes vs the sector baseline 110 / 33; no heartbeat, no session tracking (recorded gap, no M10 backend proposed). |
| 14 | A2A | 1976–2032 | PARTIAL — M9.9 in flight | Section + ids pinned by `a2a_dashboard_test.go`. Reads `GET /api/a2a/status` — **handler exists (`internal/server/a2a_status.go:70`) but is NOT registered in `server.go`**; registration is the M9.9 coordinator's job (see §3.2). No per-owner task list, no task submission from the dashboard (both forbidden by that test). |
| 15 | API Keys | 2033–2088 | PARTIAL | Rotate wired (`POST /admin/keys/{id}/rotate`); expiry/limits wired (`POST /admin/keys/{id}`). Create NOT wired (no backend route — B1). Single-key revoke NOT wired though `DELETE /admin/keys/{id}` exists. Single-key disable NOT wired though `POST /admin/keys/{id}` `{"disabled":true}` exists (`guard.go:330`). |
| 16 | Request Logs | 2089–2139 | HAVE | All columns + Reasoning/Cached/Saved; `GET /dashboard/logs`, All/ok/failed filter. |
| 17 | Usage & Cost | 2140–2240 | PARTIAL | Spend/tokens/failovers from `GET /dashboard/usage`. No Cost Explorer, no forecast, no quota-share/teams UI — although `GET /dashboard/quota-pools` and `GET /dashboard/teams` already exist and are **read by nothing** in the dashboard (0 refs). |
| 18 | Benchmark | 2241–2391 | OURS-ONLY → KEEP (justified) | Live from `GET /dashboard/benchmark` + history + memstats + release-verify; missed-target verdicts shown honestly. the sector baseline has no benchmark page, but this is our own measured-evidence surface (Master Prompt: measured wins, reproducible), not a storefront extra. Keep, do not expand in M10. |
| 19 | Verification status | 2392–2439 | OURS-ONLY → KEEP (justified) | Honest what-is-proven / what-still-needs-CI-or-a-browser status from `GET /dashboard/verification`. Same justification as Benchmark. Keep. |
| 20 | Settings | 2440–2565 | WRONG-BEHAVIOUR (in part) | Strategy picker wired (live). **5 switches are display-only with a false "· saved" toast**: Automatic failover, Prompt-cache pinning, Cost-aware routing, Health checks, Request logging (`web/index.html:2460-2499`, generic handler ~6052–6056 flips `aria-checked` and toasts — no fetch, no persistence). **Listen address select (2491) has no handler; Port input is hard-coded `8080` (real default is 20128).** Session-stickiness toggle: same fake pattern (parity tracker Area 8b). |

### 2b. Gap table — the sector baseline pages/sections vs ours

| the sector baseline page/section | Ours | Verdict | Our file:line / backend |
|---|---|---|---|
| Home 4-step quick-start + topology | Overview (KPI row + detail cards) | PARTIAL | `web/index.html:665-917`; `GET /dashboard/overview` (`server.go:1343`) |
| Providers as addable groups (+ Free Tier group) | Providers (catalog list, item 291) | PARTIAL | `web/index.html:918-1033`; `GET /api/catalog` (`server.go:1412`), `GET /api/providers` (`server.go:1406`) |
| Models table | Models table | PARTIAL | `web/index.html:1034-1098`; `GET /dashboard/models` (`server.go:1344`) |
| Costs: KPIs 1D/7D/30D + forecast + Cost Explorer + quota-share + teams | Usage & Cost only (no explorer/forecast/pools/teams UI) | MISSING (UI only — backend exists) | Feeds exist, 0 refs in dashboard: `GET /dashboard/quota-pools`, `GET /dashboard/teams`, `GET /api/analytics/costs` |
| Combos CRUD + builder + per-capability tabs | Nothing | MISSING (UI only — CRUD API exists) | `GET/POST/PATCH/DELETE /v1/combos` (`server.go:1518-1522`); 0 refs in dashboard |
| API Manager: create key | "+ New router key" button, unwired toast | MISSING (backend too — B1) | `web/index.html:2039`, handler `:6038`; no `POST /admin/keys` route exists |
| API Manager: regenerate | Wired | HAVE | `POST /admin/keys/{id}/rotate` (`server.go:1544`) |
| API Manager: single-key disable / revoke | Buttons exist, unwired toasts | PARTIAL (backend exists, UI unwired) | `DELETE /admin/keys/{id}` (`server.go:1543`); `POST /admin/keys/{id}` (`server.go:1542`, `guard.go:330`) |
| Per-key daily/weekly/monthly $ limits | Only rolling 5h | MISSING (backend too — B2) | `GET/POST /dashboard/key-budgets` (`server.go:1386-1387`) is 5h-only |
| Per-key device tracking | "not tracked" label only | MISSING (cut — no backend; keep honest label) | — |
| Cache page | Nothing (feed exists, unread) | MISSING (UI only) | `GET /api/cache/stats` (`server.go:1439`), `DELETE /api/cache` (`server.go:1443`); 0 refs |
| Resilience page (breakers/cooldowns/reset) | Cooldown section only (read + global modes) | PARTIAL (read UI only; settings/reset B6) | `GET /api/resilience` (`server.go:1421`); 0 refs |
| Webhooks page | Nothing (API exists, unread) | MISSING (UI only; edit B5) | `GET/POST /api/webhooks`, `GET/DELETE /api/webhooks/{id}` via `registerWebhooksRoutes` (`webhooks_api.go:425`, `guard.go:213-217`); 0 refs; no PUT |
| MCP page + audit + transport toggles + heartbeat | Page + audit + toggles; no heartbeat/session tracking | PARTIAL | `web/index.html:1864-1975`; `GET /dashboard/mcp` (`server.go:1366`), `GET /api/mcp/audit` (`server.go:1367`), `GET/POST /api/mcp/status` (`server.go:1368-1369`) |
| A2A page | Section built by M9.9; feed unregistered | PARTIAL (dependency — §3.2) | `web/index.html:1976-2032`; handler `a2a_status.go:70`, NOT in `server.go` |
| Audit / Compliance view | Nothing (guard denials counted only) | MISSING (backend too — B4) | No `/dashboard/audit` route; `GET /api/mcp/audit` is MCP-only |
| Activity feed / timeline | Latest-route-trace card only | MISSING (composable from existing feeds) | `GET /dashboard/logs` (`server.go:1348`), `GET /dashboard/trace` (`server.go:1398`) |
| Health page | No page (feeds exist, partly read) | MISSING (UI only) | `GET /dashboard/memstats` (`server.go:1403`), `GET /dashboard/readiness` (`server.go:1525`), `GET /dashboard/benchmark` (`server.go:1401`) |
| Batch page | Nothing (API exists) | MISSING (UI only; read-only in M10) | `POST/GET /v1/batches` (`server.go` batches block); gateway-key auth — see M10-16 |
| Analytics page (auto-routing/compression/diversity + Combo Health) | Nothing (feeds exist, unread) | MISSING (UI only; Combo Health cut — no backend) | `GET /api/analytics/summary|by-provider|by-model|costs` (`server.go:1416-1419`); 0 refs |
| Logs: proxy / console / timeline / conversations / compression log | Request-logs table only | MISSING (proxy/console/timeline as filtered views of the same feed; conversations CUT) | `GET /dashboard/logs`; `GET /api/logs` (`server.go:1422`); no conversations backend — Responses is our equivalent (§5 M10-16) |
| Settings: 5 toggles + listen/port | Display-only / fake values | WRONG-BEHAVIOUR (B7/B8 or remove) | `web/index.html:2460-2499`, `:2491`; no backend endpoint for any of them |
| Settings → Security Authz + PII card | Route-guards page + Guardrails page (separate top-level pages) | OURS-ONLY → fold into Settings (§6) | `GET /dashboard/guard` (`server.go:1365`), `GET/POST /dashboard/guardrails` (`server.go:1376-1377`) |
| Plugins marketplace + install | Registry snapshot only, honest zero state | PARTIAL (install deliberately absent — 501 by design) | `GET /dashboard/plugins` (`server.go:1378`) |
| Free-tier marking / key-creation links in Models | Stored (`hasFree` ×154 in catalog) but surfaced nowhere | PARTIAL (B9 verify-first) | `GET /dashboard/models`; verify field presence at build |
| Responses (ours-only feed, no section) | Feed exists, read in trace views only | PARTIAL | `GET /dashboard/responses`, `/dashboard/responses/tree` (`server.go:1523-1524`) |
| Prices feed | Feed exists, read by nothing | MISSING (UI only) | `GET /dashboard/prices` (`server.go:1400`); 0 refs — Models shows cheapest pair only (known caveat) |

**Single largest dashboard gap vs the sector baseline: named combos.** the sector-typical Combos page is a 5,435-line control centre — the primary routing workflow (CRUD + builder + per-capability endpoint tabs + duplicate/import/export/test/enable) — and we have **zero combos UI** even though a live `GET/POST/PATCH/DELETE /v1/combos` CRUD API already exists. It is the only the sector baseline primary workflow with no dashboard presence at all. (Close second: Costs — Cost Explorer, forecast, and quota-share/teams UI are all missing while their backend feeds already exist and are unread.)

---

## 3. Endpoint verification (all checked in `internal/server/server.go` / `guard.go`, 2026-11-19)

### 3.1 Endpoints the slices may read — all EXIST

| Endpoint | Registered (`server.go`) | Guard row | Dashboard reads it today? |
|---|---|---|---|
| `GET /dashboard/overview?range=24h\|7d\|30d` | :1343 | `guard.go:90` | Yes |
| `GET /dashboard/providers` | :1342 | `guard.go:89` | Yes |
| `GET /api/providers` (+ `POST`, `DELETE /api/providers/{id}`, key add/delete/role) | :1406–1411 | `guard.go:170-175` | Yes (GET/POST/DELETE; role via key form) |
| `GET /api/catalog` | :1412 | `guard.go:176` | Yes |
| `GET /dashboard/models` (+ `POST /dashboard/models/refresh`) | :1344, :1347 | `guard.go:91,94` | Yes |
| `GET /dashboard/strategies` (+ `POST`) | :1380–1381 | `guard.go:142-143` | Yes |
| `GET /dashboard/cooldown` (+ `POST`) | :1382–1383 | `guard.go:144-145` | Yes |
| `GET /dashboard/coalescing` | :1361 | `guard.go:108` | Yes |
| `GET /dashboard/concurrency` | :1363 | `guard.go:112` | Yes |
| `GET /dashboard/pools` | :1364 | `guard.go:113` | Yes |
| `GET /dashboard/guard` | :1365 | `guard.go:114` | Yes |
| `GET /dashboard/guardrails` (+ `POST`) | :1376–1377 | `guard.go:138-139` | Yes |
| `GET /dashboard/plugins` | :1378 | `guard.go:140` | Yes |
| `GET /dashboard/mcp` | :1366 | `guard.go:115` | Yes |
| `GET /api/mcp/audit` | :1367 | — (M9.7) | Yes |
| `GET/POST /api/mcp/status` | :1368–1369 | — (M9.7) | Yes |
| `GET /dashboard/keys` | :1350 | `guard.go:97` | Yes |
| `POST /admin/keys/{id}`, `DELETE /admin/keys/{id}`, `POST /admin/keys/{id}/rotate`, `POST /admin/keys/revoke-all` | :1541–1544 | `guard.go:327-330` | Partial (update + rotate wired; single revoke/disable unwired) |
| `GET /dashboard/logs` | :1348 | `guard.go:95` | Yes |
| `GET /api/logs` | :1422 | — | No (0 refs; use for Logs tabs if needed) |
| `GET /dashboard/usage` | :1349 | `guard.go:96` | Yes |
| `GET /dashboard/key-budgets` (+ `POST`) | :1386–1387 | `guard.go:148-149` | Yes |
| `GET /dashboard/quota-pools` (+ `POST`) | :1388–1389 | `guard.go:150-151` | **No — 0 refs** |
| `GET /dashboard/quota-schedules` (+ `POST`) | :1390–1391 | `guard.go:152-153` | No (out of M10 scope; do not add UI — schedule UI is M12 unless a slice justifies it) |
| `GET /dashboard/internal-budgets` (+ `POST`) | :1392–1393 | `guard.go:154-155` | No (out of M10 scope, same as above) |
| `GET /dashboard/teams` (+ `POST`, `POST /dashboard/teams/{id}/members`) | :1394–1396 | `guard.go:156-158` | **No — 0 refs** |
| `GET /dashboard/request-budgets` | :1397 | `guard.go:159` | Yes |
| `GET /dashboard/trace`, `GET /dashboard/context-fit` | :1398–1399 | `guard.go:160-161` | Trace yes; context-fit **no — 0 refs** |
| `GET /dashboard/prices` | :1400 | `guard.go:162` | **No — 0 refs** |
| `GET /dashboard/benchmark`, `/dashboard/benchmark/history`, `/dashboard/memstats`, `/dashboard/streaming` | :1401–1404 | `guard.go:163-165` | Yes |
| `GET /dashboard/responses`, `/dashboard/responses/tree` | :1523–1524 | `guard.go:110-111` | Partly (trace views) |
| `GET /dashboard/readiness`, `GET /dashboard/headless` | :1525–1526 | `guard.go:166-167` | Yes |
| `GET /dashboard/auto-route`, `/dashboard/auto-route/last` | :1351–1352 | `guard.go:98-99` | Yes |
| `GET /dashboard/feedback` (+ `POST`), `/dashboard/learned`, `/dashboard/code-outcomes`, `/dashboard/intent-tiers` (+ `POST`), `/dashboard/suggestions` (+ `POST`), `/dashboard/reasoning`, `/dashboard/groups`, `/dashboard/stream-lifetime`, `/dashboard/catalog-audit`, `/dashboard/release-verify`, `/dashboard/verification` | :1353–1360, :1362, :1384–1385, :1345–1346, :1379 | `guard.go:100-107,109,146-147,92-93,141` | Yes (existing sections; M10 does not rebuild them except where a slice says so) |
| `GET /api/analytics/summary|by-provider|by-model|costs` | :1416–1419 | — | **No — 0 refs** |
| `GET /api/resilience` | :1421 | — | **No — 0 refs** |
| `GET /api/cache/stats`, `DELETE /api/cache` | :1439, :1443 | — | **No — 0 refs** |
| `GET/PATCH /api/pricing` | :1441–1442 | — | No (pricing edit UI is M12; M10 reads `GET /dashboard/prices` only) |
| `GET/POST /api/webhooks`, `GET/DELETE /api/webhooks/{id}` | `registerWebhooksRoutes` (`webhooks_api.go:425`) | `guard.go:213-217` | **No — 0 refs** |
| `GET/POST/PATCH/DELETE /v1/combos` (+ `GET /v1/combos/{id}`) | :1518–1522 | `guard.go:304-308` | **No — 0 refs** |
| `POST/GET /v1/batches` (+ per-id, cancel) | batches block (`server.go`) | — | No |
| `GET /v1/models`, `GET /v1/me/status` | :1446, batches block | — | No (dashboard uses `/dashboard/models`; do not switch) |

**Auth note (M1.2 fallout, still open):** the mutating dashboard POSTs are admin-scoped (`guard.go` `Scopes: ["admin"]`). The dashboard JS must send `Authorization` for **all** of them — not just the 8 named in M1.2 (that count predates the quota-pools/teams/schedules/internal-budgets POSTs). M10-0 enumerates the current admin-scoped POST set from `guard.go` and wires the header for every one; a POST that 401s in the live smoke fails M10-0's gate.

### 3.2 `GET /api/a2a/status` — EXISTS as a handler, NOT registered (M9.9 dependency)

- Handler: `internal/server/a2a_status.go:70` (`A2AStatusHandler`). Contract, from the file's header comment (`a2a_status.go:1-22`) — **the M10 A2A slice must code against this exact shape and no other**:
  - `{"enabled": false, "tasks_by_state": {"submitted": 0, "working": 0, "completed": 0, "failed": 0, "cancelled": 0}, "skills": ["smart-routing", "quota-management", "provider-discovery", "cost-analysis", "health-report", "list-capabilities"], "protocol_version": "2025-03-26"}`.
  - All five state keys are always present, even at zero — the dashboard never nil-checks a missing key.
  - `enabled == false`: all-zero counts, fixed skills list, protocol version — and the store is NEVER opened (no `tasks.db` created while A2A is off).
  - `enabled == true`: live per-state counts; store failure → **503 with honest "store unavailable"**, never fake zeros.
  - GET only; any other method → 405. Tier MANAGEMENT, admin scope; **never reachable by a gateway-key caller** (the handler assumes the route registration enforces this).
  - The skills list is a literal, not a registry read — no A2A skill registry exists yet.
- **Registration is the M9.9 coordinator's job** (the header comment says so: "NOT enforced here — the coordinator registers this route"). As of this plan, `server.go` registers only `POST /a2a` (`server.go:1462`); no `GET /api/a2a/status` registration exists. M10-13 (A2A) is therefore **BLOCKED on the M9.9 coordinator landing the registration**. At M10 start the coordinator verifies the route answers 200 with the exact shape above; if it does not, M10-13 does not start and does not work around it by inventing another endpoint.
- **Forbidden in the A2A section** (pinned by `internal/server/a2a_dashboard_test.go`): any `/api/a2a/tasks` endpoint call, any per-owner task list, any task submission from the dashboard, and any "Playground" string anywhere in the dashboard. The section links the agent card at `/.well-known/agent.json` and shows the `a2a_enabled` state and "A2A is disabled" empty state.

---

## 4. Backend-enabler slices (B1–B9) — Go work M10 depends on

These are NOT dashboard slices. Each names its endpoint, files, and the dashboard slice it unblocks. B-slices run before (or in parallel with, but landing before) their dashboard slice's DONE. If a B-slice is cut, the dashboard slice ships without that sub-scope and says so — it never fakes it.

| Id | Endpoint / feature | Files (verify exact file at build; do not invent new files until checked) | Unblocks | Notes |
|---|---|---|---|---|
| B1 | `POST /admin/keys` — create one gateway key (name, scopes, budget, limits), returned exactly once, never logged | Keys handler file holding `handleKeyUpdate`/`handleKeyRotate` + `internal/server/guard.go` row + `server.go` registration; reuse the existing key-issue path from bootstrap | M10-7 (create-key button) | Admin scope, Tier AlwaysProtected, like rotate/revoke. No dashboard slice may mint keys client-side. |
| B2 | Per-key daily / weekly / monthly USD limits (extend key-budgets beyond rolling 5h: new fields + enforcement + `GET/POST /dashboard/key-budgets` extension) | `internal/server/keybudgets.go`, `internal/ledger/` enforcement | M10-7 (D/W/M limits UI) | Fail-closed on unpriced usage, same as the 5h budget. |
| B3 | Combos duplicate / import / export / test-run / enable-disable | `internal/server/combos_api.go` + combos store | M10-5 (those five actions) | CRUD already exists; B3 is the five missing verbs only. Test-run must not spend quota silently — read-only simulation or an explicit operator-confirmed spend, decided in the B3 slice doc. |
| B4 | `GET /dashboard/audit` — audit feed: guard denials, key lifecycle events, management POSTs, MCP audit join | New `internal/server/audit.go` (new file is fine here — it is a new feed) + guard row + registration | M10-9 (Audit page — fully blocked without it) | Counts + redacted identifiers only; no prompts, no keys, no message content. |
| B5 | `PUT /api/webhooks/{id}` — edit a webhook | `internal/server/webhooks_api.go` | M10-12 (webhook edit) | Same auth/validation as create; localhost targets stay rejected by default. |
| B6 | Resilience settings read/patch + breaker reset + per-model cooldowns | `internal/server/resilience_api.go` | M10-11 (Resilience settings) | Read-only page ships without B6; settings/reset do not. |
| B7 | Read-only server-info feed (`GET /dashboard/server` or extend `/dashboard/readiness`) exposing the actual listen address + port | `internal/server/server.go` (small handler) + guard row | M10-14 (Listen/Port controls) | **Alternative, preferred if B7 is expensive:** delete the Listen/Port controls in M10-14 and show the listen address as read-only text in Verification status. B7 exists only if the controls are kept. |
| B8 | Settings toggles backend: automatic failover / prompt-cache pinning / cost-aware routing / health checks / request logging as real persisted config + `GET/POST /dashboard/settings` | `internal/server/` (new settings handler) + `internal/config/` persistence | M10-14 (Settings toggles) | **Alternative, preferred:** remove the 5 switches in M10-14 and replace with read-only status lines from live feeds (e.g. failover state from `/dashboard/trace`, logging state from `/dashboard/logs` availability). B8 exists only if the switches are kept as controls. |
| B9 | `GET /dashboard/models` carries `has_free` + key-creation URL per model (verify-first) | `internal/server/models.go` (verify) | M10-3 (free-tier marking, key links) | **Verify first:** if the feed already carries these fields, B9 is zero-work and M10-3 just renders them. Do not build B9 until M10-3 proves the fields absent. |

**Blocked-on-backend count: 7 of 18 M10 slices are blocked-in-part** (M10-5, M10-7, M10-9, M10-11, M10-12, M10-13, M10-14). **1 slice is fully blocked** (M10-9 Audit page, pending B4). **1 slice is conditionally blocked** (M10-13 A2A section, pending the M9.9 coordinator registering `GET /api/a2a/status` — not a B-slice, a handover dependency). No slice is blocked on a missing read endpoint for its core scope — every core read feed already exists (§3.1).

---

## 5. M10 slices (ordered)

### 5.1 Regions and merge protocol (applies to every slice)

`web/index.html` regions (HTML section + the JS functions that only that section calls). A slice owns its region exclusively:

| Region | Lines (HTML) | Owner slice |
|---|---|---|
| CSS tokens + nav (`<nav id="mainNav">`) | 1–664 | M10-0 (all other slices request nav changes from M10-0, they do not edit the nav) |
| Overview | 665–917 | M10-1 |
| Providers | 918–1033 | M10-2 |
| Models | 1034–1098 | M10-3 |
| Auto-router | 1099–1202 | M10-4 (with Strategies; see below) |
| Strategies | 1203–1272 | M10-4 |
| Cooldown | 1273–1339 | M10-11 (with Resilience/Health) |
| Coalescing / Concurrency / Pools | 1340–1475 | — (no slice; already HAVE, verified in M10-17 gate only) |
| Route guards + Guardrails | 1476–1787 | M10-14 (fold into Settings; region deleted, not rebuilt) |
| Plugins | 1788–1863 | — (no slice; verified in M10-17 gate only) |
| MCP & CLI | 1864–1975 | M10-13 |
| A2A | 1976–2032 | M10-13 (after M9.9 handover) |
| API Keys | 2033–2088 | M10-7 |
| Request Logs | 2089–2139 | M10-8 |
| Usage & Cost → Costs | 2140–2240 | M10-6 (renames/expands this section in place) |
| Benchmark / Verification | 2241–2439 | — (no slice; verified in M10-17 gate only) |
| Settings | 2440–2565 | M10-14 |
| NEW sections (Combos, Cache, Resilience, Health, Webhooks, Analytics, Batch, Responses, Activity, Audit) | appended after Settings in M10-0's nav order | Owning slice, in M10-0-assigned slots |
| Shared JS helpers (`$`, `$$`, `toast`, `escHtml`, fetch wrapper) | in `<script>` | M10-0 only (auth-header change lives here) |

Rules: M10-0 lands first. Later slices run in parallel on disjoint regions; the coordinator merges in numeric order and re-runs `node --check` + the dashboard HTML tests after each merge. A merge conflict in `web/index.html` is resolved by the coordinator only, never by two builders editing the same hunk. **No slice edits `CHECKLIST.md`, `parity tracker`, or `PLAN.md`** — the coordinator owns those flips at the M10-17 gate.

### M10-0 — Foundations: nav, auth headers, design tokens, baselines

- **Goal:** nav that matches the M10 end-state (new sections slotted, Guard/Guardrails entries removed per §6 — the sections themselves are removed by M10-14, the nav entries by this slice in coordination with it); every admin-scoped dashboard POST sends `Authorization`; baselines recorded so later slices can prove improvement.
- **Files:** `web/index.html` CSS + nav only (lines 1–664) + shared JS fetch wrapper; new test `internal/server/dashboard_m10_0_test.go`.
- **Endpoint:** none new. Enumerate the CURRENT admin-scoped dashboard POST set from `internal/server/guard.go` (at plan time: `/dashboard/models/refresh`, `/dashboard/feedback`, `/dashboard/intent-tiers`, `/dashboard/suggestions`, `/dashboard/guardrails`, `/dashboard/strategies`, `/dashboard/cooldown`, `/dashboard/key-budgets`, `/dashboard/quota-pools`, `/dashboard/quota-schedules`, `/dashboard/internal-budgets`, `/dashboard/teams`, `/dashboard/teams/{id}/members`, `/api/providers` + key sub-routes, `/admin/keys/*`) and wire the operator-key `Authorization` header for every one. The key itself is typed fresh per action (existing pattern in the Keys section) and never stored.
- **Tests:** dashboard HTML test asserting each new nav entry exists and `data-view="guard"` / `data-view="guardrails"` are gone; a test that drives each admin POST without auth and expects 401/403 (extends the M1.2 pattern to the full current set); `node --check` on extracted JS; record baselines in the slice report: `<p>` lines (240), `<select>` count (13), nav item count, Playground occurrences (0).
- **Definition-of-Done:** nav matches the §5.1 end-state; all admin POSTs from the live smoke return 401 without a key and 200 with one; baselines recorded; zero "Playground" strings; no other section touched.

### M10-1 — Overview: reconcile the KPI row and the detail cards

- **Goal:** Overview becomes the local-operator landing page: 4-step quick-start (Create a router key → Connect a provider → Point your client at the router → Monitor, each step linking to its section), provider topology summary, exactly-4 KPI cards, and a justified list of detail cards — every card below the KPI row names the verified item that depends on it (item-295 caveat), the rest are removed.
- **Files:** `web/index.html` region 665–917 only; `internal/server/dashboard_m10_1_test.go`.
- **Endpoints (EXIST):** `GET /dashboard/overview?range=24h|7d|30d` (`server.go:1343`) for the range-segmented spend line (one range at a time). No new backend. (Avg cost/req is NOT an Overview feed: it is computed from `GET /api/analytics/costs` (`server.go:1419`) and rendered in Costs, M10-6 — see the M10-17 gate decision below.)
- **Tests:** KPI row is exactly 4 cards in the served HTML (DOM check); the Overview spend line shows one range at a time from `GET /dashboard/overview?range=24h|7d|30d` (range-segmented, never side-by-side 1D/7D/30D); the Overview section contains no avg cost/req string (no feed the Overview reads supplies one); `node --check`; live smoke on a 0-provider binary shows the quick-start, not empty cards.
- **Definition-of-Done:** 4 KPIs; range-segmented spend live (one range at a time); no side-by-side 1D/7D/30D and no avg cost/req in Overview; every surviving detail card justified in the slice report by item id; removed cards listed; 0 paragraphs over 2 lines in the section.
- **Decisions recorded at the M10-17 gate (2026-11-19) — built reality, not a silent rewrite:**
  - **(a) Spend/avg-cost: §2a was right, this section's Tests/DoD were wrong.** The plan's §2a row 1 ("Spend is 24h/7d/30d range-segmented, no side-by-side 1D/7D/30D, no avg cost/req") matches what was built and verified; the M10-1 Tests/DoD lines above previously claimed "side-by-side 1D/7D/30D spend + avg cost/req live" and are corrected here to match. Evidence: `internal/server/dashboard_m10_1_test.go:4-6` (header: "range-segmented spend line (one range at a time, never side-by-side 1D/7D/30D and never an avg cost/req — no feed supplies one)") and `:125-134` (banned strings `avg cost` / `cost/req` / `1D/7D/30D` in the Overview section); built HTML has a single spend line `web/index.html:837` (`id="overviewSpend"`), rendered one range at a time (`:6709-6714`). The side-by-side Spend 24h/7d/30d KPIs and avg cost/req DO exist — in Costs, not Overview (`web/index.html:2058-2068` KPI row, `:2089-2092` "Avg cost / request · this month", JS `:7241+` from `GET /api/analytics/costs`) — which is why M10-6 keeps that claim and M10-1 drops it.
  - **(b) Quick-start order: built key-first, a deliberate divergence.** The plan's M10-1 goal previously listed "add provider → add key → pick strategy → send first request"; what was built and pinned by the passing test is "Create a router key → Connect a provider → Point your client at the router → Monitor". This order is kept deliberately: key-first matches bootstrap reality (the first key is issued by bootstrap on loopback or `codvian-router init`, and no provider can be added usefully before a router key exists). Evidence: `web/index.html:780-782` (four steps in order; line numbers verified at edit time — the file is in flux as other slices land, so cite by step text + test); `internal/server/dashboard_m10_1_test.go:58-61` (the four step strings as want strings) and `:113` (steps asserted in quick-start order).

### M10-2 — Providers: verify and close the catalog-as-addable-groups page

- **Goal:** confirm item-291 work end to end and close its gaps: grouped catalog sections **including a "Free Tier Providers" group**, search, ToS-grey rows with disabled Add, configured providers pinned on top with live status, jewel-tone verification capsules (Verified from docs / Reference metadata / Unverified).
- **Files:** `web/index.html` region 918–1033 only; `internal/server/dashboard_m10_2_test.go`.
- **Endpoints (EXIST):** `GET /api/catalog` (`server.go:1412`, 614 entries live), `GET /api/providers` (`server.go:1406`), `POST /api/providers` (`server.go:1407`), `DELETE /api/providers/{id}` (`server.go:1408`). No new backend.
- **Tests:** served page contains `#catalogCard`, `#catalogListBody`, `#catalogSearch`, `#catalogCount`; catalog total in the smoke equals `GET /api/catalog` total (never hard-coded 614); a ToS-grey row's Add button is disabled in the DOM; provider-add via the form against the live binary returns the same result as the equivalent curl (click-through itself is item 110/128 — still pending, §8).
- **Definition-of-Done:** Free Tier group present; groups render from the live feed at 0 and N providers; no hard-coded provider counts anywhere in the section.

### M10-3 — Models: finish the table

- **Goal:** Models stays table-only and gains the two missing columns: free-tier marking and key-creation link, **if** the feed carries them (B9 verify-first — check `GET /dashboard/models` for `has_free` + key URL before writing any code; if absent, this slice ships the table without those columns and B9 is queued, the columns are not faked). Keep the price-source column (`models.dev` vs operator) + synced marker from item 180, and the refresh button.
- **Files:** `web/index.html` region 1034–1098 only; `internal/server/dashboard_m10_3_test.go`.
- **Endpoints (EXIST):** `GET /dashboard/models` (`server.go:1344`), `POST /dashboard/models/refresh` (`server.go:1347`). `GET /dashboard/prices` (`server.go:1400`) is the per-pair price truth — M10-3 links to it from the table (a "Prices" link per row or a section link); it does not duplicate its data into the Models table.
- **Tests:** table has zero `<p>`-prose rows; every row's price/source matches `GET /dashboard/prices` for that pair; refresh POST re-probes and the smoke shows updated counts or honest per-provider failure reasons; `node --check`.
- **Definition-of-Done:** table-only; price-source + synced markers live; free-tier/key-link columns present iff the feed provides them, otherwise explicitly deferred to B9 in the slice report.

### M10-4 — Strategies + Auto-router: render exactly what the backend accepts

- **Goal:** the strategy picker lists only names the backend accepts (from the live `GET /dashboard/strategies` feed — at plan time: balanced, cheapest, cheap, fastest, fast, most-reliable, reliable, least-latency, latency, weighted); remove wrong `code`; label the 6 auto/* virtual models as virtual models, not strategies; no sla/nadir/lkgp options (they do not exist in the backend — rendering them would be fake). Auto-router section keeps its live classification + last-decision card.
- **Files:** `web/index.html` regions 1099–1202 (Auto-router) + 1203–1272 (Strategies); `internal/server/dashboard_m10_4_test.go`.
- **Endpoints (EXIST):** `GET/POST /dashboard/strategies` (`server.go:1380-1381`), `GET /dashboard/auto-route`, `GET /dashboard/auto-route/last` (`server.go:1351-1352`). No new backend.
- **Tests:** picker options in the served page equal the accepted-name set from the live feed (a test that posts each offered name and expects success, and posts `code` and expects rejection); changing the picker POSTs the real global strategy and the smoke confirms the new value on re-read; `node --check`.
- **Definition-of-Done:** no `code` option; no strategy name offered that the backend rejects; auto/* labelled virtual; the 18 missing the sector baseline strategies are listed in the slice report as backend-MISSING (M3), not papered over.

### M10-5 — Combos: named-combo CRUD UI (new view) — BLOCKED-in-part on B3

- **Goal:** the largest gap (§2b): a Combos view with list/create/edit/delete/rename over named routing combos, each combo showing its ordered provider/model steps + strategy, wired to the live CRUD API. Per-capability endpoint tabs (chat/search/embeddings/…) render only for capabilities the combo actually has steps for — no empty tabs.
- **Files:** `web/index.html` NEW section (M10-0-assigned slot) + its JS functions only; `internal/server/dashboard_m10_5_test.go`. Disjoint from all other slices.
- **Endpoints (EXIST):** `GET/POST/PATCH/DELETE /v1/combos`, `GET /v1/combos/{id}` (`server.go:1518-1522`, `guard.go:304-308`). **BLOCKED sub-scope:** duplicate / import / export / test-run / enable-disable — no backend verbs exist; those five actions are NOT built until B3 lands. The view must not show disabled buttons for them (no fake controls) — it simply has no such buttons until B3.
- **Tests:** create → list shows it → edit → delete round-trip against the live binary; combo created in the dashboard routes immediately (group mirror — verify via `GET /dashboard/groups`); `node --check`; dashboard HTML test asserting the section exists and contains no "duplicate"/"import"/"export" buttons until B3 lands.
- **Definition-of-Done:** full CRUD live; zero combos at 0 reads "No combos yet — create one" (not an empty table); B3 actions absent, not disabled; slice report lists them as pending on B3.

### M10-6 — Costs: the missing product surface (new/expanded view) — replaces "Usage & Cost"

- **Goal:** turn the thin Usage view into Costs: side-by-side Spend 1D/7D/30D + requests + active providers/models + avg cost/req (shared with M10-1's feed, rendered here as the Costs KPI row); Cost Explorer lite — dimension filter (provider/model only — no feed reports cost by gateway key, so the UI offers provider/model with an honest note), top-50 table, CSV export of the shown rows; Monthly Forecast as a **labelled linear projection** ("estimate from the last 30 days, not a commitment"); quota-share pools UI (create/replace/delete, member allocations, saturation); teams UI (create/replace/delete, member assignment).
- **Files:** `web/index.html` region 2140–2240 (expanded in place) only; `internal/server/dashboard_m10_6_test.go`.
- **Endpoints (EXIST):** `GET /dashboard/usage` (`server.go:1349`), `GET /api/analytics/costs` (`server.go:1419`), `GET/POST /dashboard/quota-pools` (`server.go:1388-1389`), `GET/POST /dashboard/teams` + members (`server.go:1394-1396`). No new backend. (Quota-schedules and internal-budgets feeds exist but get NO UI in M10 — see §3.1.)
- **Tests:** KPI values equal `/api/analytics/costs` for the same range; explorer top-50 rows sum to the feed totals; CSV export contains exactly the shown rows; pool create/delete round-trip; team create + member assign round-trip; forecast carries the word "estimate" and its basis in the UI; `node --check`.
- **Definition-of-Done:** all five sub-surfaces live (KPIs, explorer, forecast-labelled, pools, teams); no CSV export of anything but the shown rows; no forecast presented as a measured number.

### M10-7 — Keys: wire what the backend already offers; create/limits BLOCKED on B1/B2

- **Goal:** finish the Keys view honestly. Wire single-key disable (`POST /admin/keys/{id}` `{"disabled":true}`) and single-key revoke (`DELETE /admin/keys/{id}`, typed confirmation `DELETE <id>` per `guard.go:330`) — both backends exist, the buttons currently toast "isn't wired yet" (`web/index.html:6043-6046`). Keep rotate + expiry/limits wiring. Per-key device tracking stays an honest "not tracked" label (no backend — cut, not faked).
- **Files:** `web/index.html` region 2033–2088 only; `internal/server/dashboard_m10_7_test.go`.
- **Endpoints (EXIST):** `GET /dashboard/keys` (`server.go:1350`), `POST /admin/keys/{id}` (`server.go:1542`), `DELETE /admin/keys/{id}` (`server.go:1543`), `POST /admin/keys/{id}/rotate` (`server.go:1544`). **BLOCKED sub-scope:** create key (no `POST /admin/keys` — B1); daily/weekly/monthly per-key limits (5h-only budget — B2). Until B1 lands, "+ New router key" keeps its honest toast (it already says it isn't wired — that toast is honest and stays; it must not be replaced by a fake success).
- **Tests:** disable round-trip (key stops authenticating, re-enable restores it); revoke round-trip with typed confirmation (key stops authenticating, row removed); rotate round-trip (old key dead at once, new key shown once); create button still shows the honest unwired toast until B1; `node --check`.
- **Definition-of-Done:** disable + revoke live; create + D/W/M limits explicitly pending on B1/B2 in the slice report; no "not tracked" claim removed or softened.
- **Decision recorded at the M10-17 gate (2026-11-19) — (c) Per-key rolling budgets live in Costs, not Usage.** The correct path is **Costs → Per-key rolling budgets**; the brief's "Usage → Per-key rolling budgets" wording was wrong because **no Usage section exists anywhere in the dashboard** — there is no `view-usage` section (0 occurrences in `web/index.html`) and no `data-view="usage"` nav item; the nav item is Costs (`web/index.html:607`, `data-view="costs"`). The card itself is inside the Costs section: "Per-key rolling budgets · 5 h" at `web/index.html:2148` (card head, inside `view-costs` at `:2041`, fed by `GET/POST /dashboard/key-budgets`). The built Keys help text already uses the correct path — `web/index.html:1936`: "A key with no rolling 5 h budget yet cannot take D/W/M caps — set it first under Costs → Per-key rolling budgets." Any citation saying "Usage → Per-key rolling budgets" must be corrected to "Costs → Per-key rolling budgets". (Note: the gate-prep audit's G5 cited the card at `:2152` — that line is the `<tbody id="keyBudgetBody">`; the card head is `:2148`.)

### M10-8 — Logs polish + Activity timeline (composed, no new backend)

- **Goal:** Logs table stays as-is (HAVE). Add three filtered views of the SAME feed — Proxy (attempts with provider/model), Console (server-side event lines from `GET /api/logs`), Timeline (chronological attempts incl. the latest-route-trace card) — each labelled as a view of the request log, never as a separate data source. Add an Activity timeline section composed from `GET /dashboard/logs` + `GET /dashboard/trace` (recent requests, failovers, guard denials counted — no invented events).
- **Files:** `web/index.html` region 2089–2139 (Logs) + NEW Activity section; `internal/server/dashboard_m10_8_test.go`.
- **Endpoints (EXIST):** `GET /dashboard/logs` (`server.go:1348`), `GET /api/logs` (`server.go:1422`), `GET /dashboard/trace` (`server.go:1398`). No new backend.
- **Tests:** each filtered view's rows are a subset of the same `GET /dashboard/logs` response (test asserts counts match per filter); Activity events are traceable to a log row or a trace (no orphan events); `node --check`.
- **Definition-of-Done:** 3 log views + Activity timeline live, all sourced from the two feeds; zero events without a backing row.

### M10-9 — Audit / Compliance page (new view) — FULLY BLOCKED on B4

- **Goal:** the Audit page the sector baseline has (`ComplianceTab`): guard denials, key lifecycle events, management POSTs, MCP audit — identifiers and counts only.
- **Files:** NEW section (M10-0 slot); `internal/server/dashboard_m10_9_test.go`.
- **Endpoints:** NONE exist for this page. `GET /api/mcp/audit` (`server.go:1367`) is MCP-only and must not be presented as a full audit. **This slice does not start until B4 (`GET /dashboard/audit`) lands.** No interim version from partial feeds — a partial audit presented as an audit would be fake completeness.
- **Tests (after B4):** every audit row maps to a real event (guard denial, key event, management POST, or MCP audit entry); no prompts/keys/message content in any row; `node --check`.
- **Definition-of-Done:** page live only after B4; before B4, the nav slot stays empty (no placeholder page, no "coming soon" card).
- **Decision recorded at the M10-17 gate (2026-11-19) — (d) M10-9 line-reference correction (gate-prep G6).** The Audit read-only/retype sentence ("The feed offers no actor/action filter and no pagination for guard denials, key lifecycle, or management posts — only the MCP entries above page (50 per page, newest first). This view is read-only: there are no audit mutation controls because none exist. The operator key is never stored, so retype it in the sidebar before Refresh or paging (Older/Newer), or the load aborts.") is at **`web/index.html:3586` as recorded by the gate-prep audit, re-verified at `:3582` just before this edit** — the file is in flux while other M10 slices land above it, so the line drifts; cite by sentence text, not line alone. It is **not** at `:3575`: line 3575 is the Newer/Older pager button row (`<div style="display:flex; gap:10px; ...">` holding `#m10_9McpPrevBtn` / `#m10_9McpNextBtn`). No `:3575` reference existed in this plan file itself — the wrong number lived in the gate-prep audit record — so this note is the correction on the record: any citation saying 3575 for this sentence must read 3586 (audit-time) / 3582 (re-verified at edit time).

### M10-10 — Cache page (new view)

- **Goal:** Cache as a first-class page: hit/miss + tokens saved from the live stats feed, per-layer state, flush action with confirmation, and the honest savings note (memory-only vs persisted). Cache-write tier economics shown as the 1.25×–2.5× **band** the dashboard currently estimates (parity tracker Area 6) — labelled an estimate, with the Anthropic 5m 1.25× / 1h 2× tiers noted as the backend's per-tier accounting, not as dashboard-measured savings.
- **Files:** NEW section; `internal/server/dashboard_m10_10_test.go`.
- **Endpoints (EXIST):** `GET /api/cache/stats` (`server.go:1439`), `DELETE /api/cache` (`server.go:1443`). No new backend, no `/dashboard/cache` feed needed — the slice reads the `/api` feed directly.
- **Tests:** stats in the smoke equal `GET /api/cache/stats`; flush round-trip (stats reset, per-layer eviction counts shown); memory-only state shows the `savings_note`, never a fabricated tokens-saved number; `node --check`.
- **Definition-of-Done:** stats + flush live; savings honesty note present; no invented cache savings.

### M10-11 — Resilience + Health (new views) — settings BLOCKED-in-part on B6

- **Goal:** two read-only views. Resilience: live breaker/cooldown state from the resilience feed + the existing Cooldown section linked (not duplicated). Health: process health from memstats + readiness + benchmark (live values only; health-history ring shown if present, never synthesised).
- **Files:** NEW Resilience + Health sections + Cooldown region 1273–1339 (link only, no rebuild); `internal/server/dashboard_m10_11_test.go`.
- **Endpoints (EXIST):** `GET /api/resilience` (`server.go:1421`), `GET /dashboard/memstats` (`server.go:1403`), `GET /dashboard/readiness` (`server.go:1525`), `GET /dashboard/benchmark` (`server.go:1401`), `GET /dashboard/cooldown` (`server.go:1382`). **BLOCKED sub-scope:** breaker reset, resilience settings read/patch, per-model cooldowns — no backend (B6). No such controls until B6.
- **Tests:** resilience values equal `GET /api/resilience`; health values equal memstats/readiness responses; `node --check`.
- **Definition-of-Done:** both views live and read-only; no reset/settings controls present until B6.

### M10-12 — Webhooks page (new view) — edit BLOCKED on B5

- **Goal:** webhook list/create/delete + delivery status, with the honest loopback rule surfaced (localhost targets rejected by default; register through the config file — `webhooks_api.go:152`).
- **Files:** NEW section; `internal/server/dashboard_m10_12_test.go`.
- **Endpoints (EXIST):** `GET/POST /api/webhooks`, `GET/DELETE /api/webhooks/{id}` (via `registerWebhooksRoutes`, `webhooks_api.go:425`). **BLOCKED sub-scope:** edit — no `PUT /api/webhooks/{id}` (B5). Until B5, there is no edit button (delete + re-create is the honest path and is labelled as such).
- **Tests:** create → list shows it → delete round-trip; budget.warning delivery proven by the existing backend test (cite it, do not re-prove); loopback URL create fails with the honest error shown; `node --check`.
- **Definition-of-Done:** list/create/delete live; no edit button until B5.

### M10-13 — MCP polish + A2A section — A2A BLOCKED on M9.9 registration

- **Goal:** MCP page: tool list with scopes (24 tools / 11 scopes — the real numbers, never the sector-typical 110/33), live provider/model counts, selector state + counters, audit view, transport toggles (`POST /api/mcp/status`). Record as a known gap (no heartbeat, no session tracking) in the slice report; no M10 backend proposed for it. A2A section: verify + polish the M9.9 section against the exact contract in §3.2 — status capsule, 5-state task-count table, 6-skill table, protocol version, agent-card link, "A2A is disabled" empty state.
- **Files:** `web/index.html` regions 1864–1975 (MCP) + 1976–2032 (A2A, after M9.9 handover only); `internal/server/dashboard_m10_13_test.go`. Extends (does not rewrite) `a2a_dashboard_test.go` assertions.
- **Endpoints (EXIST):** `GET /dashboard/mcp` (`server.go:1366`), `GET /api/mcp/audit` (`server.go:1367`), `GET/POST /api/mcp/status` (`server.go:1368-1369`). **A2A:** `GET /api/a2a/status` — handler exists, registration pending M9.9 coordinator (§3.2). If unregistered at M10-13 start, the A2A half of this slice does not start.
- **Tests:** MCP tool list equals `GET /dashboard/mcp` tools; audit view rows equal `GET /api/mcp/audit`; transport toggle round-trip via `POST /api/mcp/status`; A2A: enabled=false smoke shows all-zero 5-state table + fixed 6 skills + `2025-03-26`; enabled=true smoke shows live counts; store-failure path shows the honest 503 message; test asserts no `/api/a2a/tasks` string and no "Playground" string in the served dashboard.
- **Definition-of-Done:** MCP page complete per its real numbers; A2A section matches §3.2 exactly, or is explicitly pending on the M9.9 registration in the slice report.

### M10-14 — Settings repair + fold-in of Guardrails and Route guards

- **Goal:** Settings stops lying. (a) The 5 display-only switches (Automatic failover, Prompt-cache pinning, Cost-aware routing, Health checks, Request logging — `web/index.html:2460-2499`) are **removed as controls** and replaced with read-only status lines sourced from live feeds (failover state from `/dashboard/trace`, logging state from `/dashboard/logs` availability, etc.) — unless B8 lands first, in which case they become real controls wired to `GET/POST /dashboard/settings`. Either way the generic fake handler (~6052–6056, toast "· saved") is deleted. (b) Listen address + Port: **removed as controls** unless B7 lands; the actual listen address is shown as read-only text (from B7, or in Verification status). The hard-coded `8080` is deleted either way. (c) Fold-in per §6: Guardrails toggles become a "PII & content" card and the Route-guard tier tables become a "Route authorization" table, both under Settings → Security; the top-level Guard/Guardrails nav entries and sections are deleted.
- **Files:** `web/index.html` regions 1476–1787 (deleted) + 2440–2565 (Settings); `internal/server/dashboard_m10_14_test.go`. Coordinate nav-entry removal with M10-0.
- **Endpoints (EXIST):** `GET/POST /dashboard/strategies`, `GET/POST /dashboard/cooldown`, `GET/POST /dashboard/guardrails` (`server.go:1376-1377`), `GET /dashboard/guard` (`server.go:1365`). **BLOCKED sub-scope:** B7 (listen/port read feed), B8 (toggles backend) — only if the controls are kept; the default plan removes them and needs no backend.
- **Tests:** served page contains zero `.switch` elements with the fake handler; no "· saved" toast string in the served JS for a control without a fetch; no `8080` literal; Guardrails toggles still POST to `/dashboard/guardrails` from their new home; Route-guard tables still read `GET /dashboard/guard`; `node --check`.
- **Definition-of-Done:** zero fake controls in Settings; Guardrails + Route guards live under Settings → Security with the same feeds; top-level pages gone.

### M10-15 — Analytics page (new view) — Combo Health cut

- **Goal:** Analytics from the existing feeds: summary KPIs, by-provider, by-model, costs — plus auto-routing / compression / diversity tabs **only where the feed provides them** (summary/by-provider/by-model/costs exist; auto-routing, compression, and diversity counterparts do not — those tabs are absent, not empty). Combo Health is CUT: no backend combo-metrics endpoint exists; do not build a health table from unrelated numbers.
- **Files:** NEW section; `internal/server/dashboard_m10_15_test.go`.
- **Endpoints (EXIST):** `GET /api/analytics/summary|by-provider|by-model|costs` (`server.go:1416-1419`). No new backend.
- **Tests:** every number on the page traces to one of the four feeds (test asserts the smoke values equal the feed responses); `node --check`.
- **Definition-of-Done:** analytics live for the four existing feeds; no auto-routing/compression/diversity tabs; no Combo Health table; the cut is recorded in the slice report.

### M10-16 — Batch (read-only) + Responses section — Conversations cut

- **Goal:** Batch: read-only job list from `/v1/batches` (status, model, counts). Create/cancel are NOT wired in M10 — batch creation spends quota through a gateway-key-authenticated API and the dashboard's operator-key UX for it is undecided; a read-only list is the honest scope. Responses: a section from `GET /dashboard/responses` + `/dashboard/responses/tree` (identifiers, model, status, continuation chain, counts only — never input/output text). Conversations: CUT — the sector-typical Conversations is chat history we do not store; Responses is our equivalent and is labelled as such ("Responses, not conversations — the router stores response identifiers and chains, not chat transcripts").
- **Files:** NEW Batch + Responses sections; `internal/server/dashboard_m10_16_test.go`.
- **Endpoints (EXIST):** `GET /v1/batches` (`server.go` batches block), `GET /dashboard/responses`, `GET /dashboard/responses/tree` (`server.go:1523-1524`). No new backend.
- **Tests:** batch rows equal the `/v1/batches` response; responses rows equal the feed (identifiers only — test asserts no input/output text appears in the rendered section); `node --check`.
- **Definition-of-Done:** both sections live, read-only; Conversations explicitly cut in the slice report with the Responses-equivalence note.

### M10-17 — Exit gate (coordinator-owned)

- **Goal:** prove M10, then flip PARITY honestly.
- **Files:** none in the dashboard (coordinator runs the gate; slice builders fix what it finds in their own regions). **This doc does not edit `CHECKLIST.md`, `parity tracker`, or `PLAN.md`** — the coordinator does the flips with file:line + test evidence.
- **Gate (all must pass):**
  1. `scripts/audit-fake.sh` returns ZERO UI controls without live backend (PLAN.md M10 exit gate). Any control the gate flags is removed or wired — no exceptions, no "honest toast" leftovers except the two explicitly kept: "+ New router key" (pending B1) and provider-add's CLI fallback (item 110/128 path) — and both must be re-justified at the gate, not grandfathered.
  2. Playwright smoke test clicks every control (PLAN.md M10 exit gate). Control inventory = every `<button>`, `<select>`, `<input>` in the served page; each click is traced to a network request or a state change, and the result is recorded.
  3. Full Go test suite green on touched packages; `go vet` + `gofmt` clean; `node --check` clean on extracted dashboard JS.
  4. Live-binary smoke (real binary, isolated HOME, port 20128): every section renders; every feed in §3.1 answers 200 (or honest 503/empty); served dashboard HTML is byte-identical to `web/index.html` (sha256).
  5. `<p>` recount + dropdown recount (targets in §8); "Playground" = 0; no `8080` literal; no "· saved" toast for an unwired control.
  6. parity tracker Area 8b + Area 9 dashboard rows flipped only with file:line + test evidence per row; rows that stay MISSING name the B-slice or the cut decision.
  7. Live-browser verification (§8) attempted; items 110/128/297 stay pending until it actually happens — the gate does not mark them done.
- **Definition-of-Done:** all seven gate items pass or are explicitly recorded as pending with the reason; M10 is not declared done while any B-blocked sub-scope is faked.

---

## 6. Decision: Guardrails page and Route-guards page — REMOVE as pages, FOLD into Settings

**Decision: remove both as top-level nav pages. Fold both into Settings → Security. Keep both backend feeds exactly as they are.**

This is the default Raunak's correction requires (remove unless a concrete operator need is documented). The operator need IS documented below — and it is met better inside Settings than by a top-level page, which is exactly the sector-typical shape (Authz section + PII card under Settings → Security; no Guardrails page, no Route-guards page).

| Page | Concrete operator need (documented) | Why it does not need a top-level page | Where it lands |
|---|---|---|---|
| Guardrails | An operator must be able to see and change what the router blocks (PII / secrets / prompt-injection) and see redaction counts + recent redacted events — this is a live safety control, not decoration. Backend: `GET/POST /dashboard/guardrails` (`server.go:1376-1377`), 3 kind toggles (ids `grTogglePII/Secrets/Injection`, `web/index.html:1748-1758`, wired per item 296). | The need is "change 3 toggles + see counts", a card-sized job. A full page for 3 toggles is the paragraph-heavy, storefront-shaped over-build Raunak corrected. the sector-typical model is backend-only guardrails + a PII card in Security. | Settings → Security, as a "PII & content" card: the same 3 toggles (same ids, same POST), redaction counts, recent redacted events. Per-key guardrails in the Keys view stay where they are. |
| Route guards | An operator must be able to audit the auth posture: which routes are Tier 1/2/3, which are admin-scoped, which are auth-exempt — this is the evidence that M1's scope work holds. Backend: `GET /dashboard/guard` (`server.go:1365`), live Tier 1/2/3 tables. | The need is "audit the route table", a settings/security concern, not a daily workflow. A top-level page elevates an audit table to a primary workflow and duplicates Settings → Security's job. the sector baseline puts Authz under Settings → Security. | Settings → Security, as a "Route authorization" table: the same live Tier 1/2/3 tables from `GET /dashboard/guard`, plus the tunnel + DB-export rows that live there today. |

**What is deleted:** nav entries `data-view="guard"` and `data-view="guardrails"`, sections `view-guard` (`web/index.html:1476-1619`) and `view-guardrails` (`:1620-1787`). **What is kept:** both feeds, both sets of controls, both tables — moved, not removed. **What is NOT decided here:** whether the guardrails toggles gain per-type selection (23 builtin types in the sector baseline vs our 3 kind toggles — parity tracker Area 6/8b, MISSING). That is a backend gap, not an M10 layout question; M10 renders the 3 toggles honestly and records per-type selection as MISSING.

---

## 7. The 0-providers empty state (catalog-as-addable-groups)

Applies at 0 configured providers — the state Raunak found empty everywhere on 2026-11-18. The rule for every section: **at 0 providers the dashboard shows the catalog/action, never an empty table and never a zero-filled KPI.**

| Section | 0-provider state |
|---|---|
| Overview | 4-step quick-start (Create a router key → Connect a provider → Point your client at the router → Monitor — key-first order, deliberate divergence recorded in M10-1 at the M10-17 gate) + "0 providers live" stated plainly. KPI cards show "—" / "No data yet", never 0s that imply measured zeros. Detail cards that need traffic are hidden, not zero-filled. |
| Providers | **Catalog-as-addable-groups (already landed, item 291 — M10-2 verifies):** grouped catalog sections incl. "Free Tier Providers", search box, per-row Add (disabled + reason for ToS-grey rows), configured providers pinned on top with live status. The catalog total comes from `GET /api/catalog` (614 live, never hard-coded). Empty state text is one line: "No providers connected yet — add one below." |
| Models | Table with one line: "No models yet — add a provider" + link to Providers. No sample rows. (Item 292 DONE; M10-3 keeps it.) |
| Strategies / Auto-router / Cooldown / Coalescing / Concurrency / Pools | Live feeds with honest empty states: strategy picker still works (global strategy is settable with 0 providers); auto-router classification runs (no provider called); counters read 0 with "no traffic yet" labelling, not as performance claims. |
| Keys / Logs / Usage & Cost / Analytics | Keys: list + "No key yet" (bootstrap path shown). Logs: empty table + "No requests yet". Costs: "No spend yet" (not $0.00 presented as a measured total — $0.00 is only shown when the ledger actually has rows summing to zero). |
| Combos / Cache / Webhooks / Batch / Responses | "No combos yet — create one", "No cache entries", "No webhooks yet", "No batch jobs yet", "No responses yet" — each with the single action that creates the first one (or the honest note that creation is CLI-only where that is true). |
| MCP / A2A | MCP: tool list + counts still render (tools exist without providers; provider/model counts read 0 honestly). A2A: "A2A is disabled" empty state when `enabled` is false; all-zero 5-state table (the contract guarantees the keys exist). |
| Settings | Fully usable at 0 providers (strategy, cooldown, guardrails toggles, key management). The 5 fake switches are gone per M10-14, so there is nothing to fake at 0 providers. |

**Design tokens for empty states (all sections):** one short line of text + one primary pill button to the action that fixes it. No paragraphs, no lorem, no sample data. The "No providers connected yet" string is the canonical empty-state copy; do not invent variants per section.

---

## 8. Verification plan

### 8.1 What is verified in this VM (no live browser needed)

- `go build ./...`; `go vet` + `gofmt -l` clean on touched packages; full test suite on touched packages, `-race` on `internal/server`.
- `node --check` on the dashboard JS extracted from the served page.
- Live-binary smoke (real binary, isolated HOME, port 20128): every §3.1 feed answers 200 (or honest 503/empty); served HTML sha256-identical to `web/index.html`; DOM-level checks via Python `html.parser` on the served page: `<p>` recount, `<select>` recount, nav inventory, "Playground" = 0, no `8080` literal, no fake "· saved" strings.
- `scripts/audit-fake.sh` returns ZERO UI controls without live backend (M10-17 gate item 1).
- **Targets:** `<p>` lines down from 240 (target: 0 paragraphs over 2 lines anywhere; every surviving `<p>` is a ≤2-line hint — the item-293 standard, re-measured because the count drifted to 240); all 13 `<select>` elements styled per design system (pill, dark-lime, no native arrow) — the 4 known native-arrow ones (model provider filter `:1065`, model category filter `:1069`, per-provider add-key role `:2922`, intent-tier effort `:6628`) are the explicit fix list; Listen-address select either wired via B7 or removed.

### 8.2 What needs a live browser (cannot be done in this VM — stays pending)

- **Items 110 / 128 / 297 stay pending.** Item 110 and 128 are the dashboard provider-add form live-browser click-through (typing into the fields and clicking "Add provider" in a real browser). Item 297 is the dashboard-simplification live verification (every section checked in a real browser render, with screenshots; provider-add click-through re-attempted in the same pass). They are pending because **no live browser works in this VM**: `/opt/meta-chromium/chrome` refuses to start ("Hatch WebGL domain policy is required for browser startup"), and there is no puppeteer/playwright/WebSocket client tooling to drive it. Everything the click would do has been verified by other means (JS syntax-checked, exact request shapes driven by curl, all response branches exercised, real chat 200 — per items 110/128) — but a genuine click-through still needs an agent with a working live browser, or Raunak. **Do not claim 110/128/297 done in any M10 slice report or gate.**
- **Live-browser click-through checklist** (for the agent/Raunak who runs it — each item needs a screenshot):
  1. Every nav section renders (all 20 current + all new M10 sections) in light AND dark mode; screenshot per section.
  2. Provider-add form: type a real provider + key, click Add, screenshot the success state; then delete it, screenshot the empty state.
  3. Every `<select>` opened AND closed (13 selects): screenshot open state (styled options list, dark-lime) and closed state; the 4 fixed ones get individual screenshots.
  4. Combos: create → edit → delete round-trip by clicking only.
  5. Keys: disable, revoke (typed confirmation), rotate — by clicking only.
  6. Settings: every surviving switch/toggle clicked; confirm the state persists on reload (or the read-only status line updates).
  7. 0-provider state: fresh binary, screenshot Overview + Providers + Models.
  8. A2A: disabled state screenshot; enabled state (if Raunak enables it) screenshot.
  9. Playwright smoke (M10-17 gate item 2) clicks every control — this is the automated form of the same list; it runs wherever a working browser exists, not in this VM.

### 8.3 Evidence standard per slice

Each slice report states: what was built (file:line), which live feed it reads (endpoint + registration line), the test command + result, the live-smoke result, and what is still pending (B-slice id or live-browser item). A slice that cannot show the live feed answering is not DONE. PARITY flips happen at the M10-17 gate only, one row at a time, with file:line + test evidence.

---

## 9. Non-goals (do not build in M10)

- Playground (removed, stays removed). Gamification, leaderboard, radar, translator, sponsor banners, cloud-agents/CLI-agent pages, MITM/1proxy tooling, chaos page.
- Dashboard login / OIDC (divergence — gateway-key model stands).
- Plugin install UI (`POST /admin/plugin/load` is 501 by design).
- Per-type PII selection (23 types), per-model cooldowns, breaker reset, webhook edit, combo duplicate/import/export/test/enable, key creation, per-key D/W/M limits, full audit feed — all backend gaps owned by B1–B9 or recorded MISSING; the dashboard does not fake any of them.
- Pricing edit UI (`PATCH /api/pricing` stays API-only), quota-schedules UI, internal-budgets UI (feeds exist; no M10 surface — adding them is scope creep, not parity).
- Any new backend route invented by a dashboard slice. If a slice needs an endpoint that §3 says MISSING, it stops and names the B-slice; it does not add the route itself.
- `CHECKLIST.md` / `parity tracker` / `PLAN.md` edits by slice builders (coordinator only, at M10-17).

---

## 10. Summary for the coordinator

| Item | Count / answer |
|---|---|
| Plan file | `docs/plan/m10-dashboard-plan.md` (this file) |
| M10 slices | **18** (M10-0 … M10-17) |
| Slices blocked-in-part on backend | **7** (M10-5 on B3; M10-7 on B1+B2; M10-9 fully blocked on B4; M10-11 on B6; M10-12 on B5; M10-13 A2A half on M9.9 registration; M10-14 on B7/B8 only if the controls are kept — default plan removes them, needing no backend) |
| Backend-enabler slices | **9** (B1–B9; B9 is verify-first, may be zero-work) |
| Slices blocked on a missing core read endpoint | **0** — every core read feed already exists (§3.1) |
| Single largest dashboard gap vs the sector baseline | **Named combos: zero UI for the sector-typical 5,435-line Combos control centre, while a live `/v1/combos` CRUD API already exists** (M10-5; duplicate/import/export/test/enable pending B3). Close second: Costs (explorer/forecast/pools/teams UI missing while the feeds exist unread). |
| Guardrails / Route guards | Remove both as top-level pages; fold into Settings → Security (§6); feeds and controls kept. |
| Live-browser items | 110 / 128 / 297 stay pending — no working browser in this VM; do not claim them done (§8.2). |
| Order | M10-0 first (nav + auth + tokens), then M10-1…M10-16 in parallel on disjoint regions (merge in numeric order), M10-17 gate last. M10-9 waits for B4; M10-13's A2A half waits for the M9.9 coordinator registering `GET /api/a2a/status`. |
