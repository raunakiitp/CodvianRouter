# Live-browser verification package — Codvian Router (CHECKLIST items 110, 128, 297)

## STATUS: HELD — PREPARATION ONLY, NOT EXECUTED

- **Prepared:** 2026-11-19, on this VM, from the worktree only. No browser ran. No
  click-through has been performed. **Items 110, 128 and 297 are NOT done.**
- **Pairing DECLINED FOR NOW** (Raunak, 2026-11-19 ~15:40 IST, voice call): his
  Windows PC is busy with other work and he does not want it disturbed — no
  Tailscale / OpenSSH Server setup, nothing runs on his computer, work stays
  VM-only. This package is therefore **HELD for whenever Raunak approves pairing
  in future**. Do NOT present it as "ready the moment it is paired", and do NOT
  run any part of it on his machine until he says so.
- **Future executor (whenever approved): the agent, over SSH** — the pairing path
  found by research 2026-11-19 ~15:40 IST is Tailscale + Windows OpenSSH Server
  (report: `~/workspace/goals/codvian-router/hidden_files/pairing-research-2026-11-19.md`).
  After Raunak completes that ~10-minute one-time setup and approves it, the agent
  drives his PC directly over SSH and runs this runbook itself. **Raunak does not
  click anything by hand in this runbook** — every step below is written as an
  agent-executed browser action (headless Chromium / Playwright driven over SSH),
  with the same step, expected observation and screenshot filename either way.
- **What this package is:** a self-contained runbook + shot list + results template so
  the future agent run needs no re-research. **What it is not:** evidence that any
  click-through happened. The `screenshots/` folder in this package is empty on
  purpose — it is the destination for the future run's captures.
- **Scope guard:** this package creates new files only. It edits no code, no
  dashboard file, and not `CHECKLIST.md` / `parity tracker` / `PLAN.md`. Closing items
  110/128/297 in the checklist is the coordinator's job, after the results template
  in this package is filled in from a real run.

## 1. What each item requires — quoted verbatim from CHECKLIST.md

Row texts copied character-for-character from `CHECKLIST.md` (file lines 119, 135,
308, as read 2026-11-19). The `Built` / `Verified` columns are as they stand today.

### Item 110 — file line 119, Built ❌, Verified ❌

> | 110 | Dashboard provider-add form live-browser click-through: NOT DONE — no generic subagent in this environment has live-browser access, so typing into the fields and clicking "Add provider" in a real browser was never performed; everything the click would do was verified by other means (JS syntax-checked, the exact requests driven by curl, all response branches exercised), but a genuine click-through still needs an agent with live-browser access | ❌ | ❌ |

### Item 128 — file line 135, Built ❌, Verified ❌

> | 128 | Dashboard provider-add form live-browser click-through: NOT DONE — no live-browser access exists in this environment: `/opt/meta-chromium/chrome` (the only browser binary present) refuses to start in this VM — `Hatch WebGL domain policy is required for browser startup` with both `--headless=new` and `--headless` — and there is no puppeteer/playwright/WebSocket client tooling to drive it anyway. Typing into the fields and clicking "Add provider" in a real browser was therefore never performed. Everything the click would do was verified by other means (item 127: JS syntax-checked, the form's exact request shapes driven by curl, every response branch exercised, real chat 200). A genuine click-through still needs an agent with a working live browser | ❌ | ❌ |

### Item 297 — file line 308, Built ✅, Verified ⬜

> | 297 | Dashboard simplification — live verification (UI track): new dashboard served from the built binary and every section checked in a real browser render (screenshots); provider-add form click-through re-attempted in the same pass (items 110/128 stay open until a live browser actually clicks). Built 2026-11-18, Verified ⬜ — NOT DONE (browser part): served from the built binary and verified by every non-browser means — dashboard byte-identical to web/index.html (sha256), all new ids present in the served page (#catalogCard, #catalogListBody, #catalogSearch, #catalogCount, #modelsBadge, #kpiReqLabel, data-kpi prov/mod/req/route, #grTogglePII/Secrets/Injection), GET /api/catalog live total 614, /api/providers + /dashboard/models + /dashboard/overview respond, node --check clean on the extracted script, DOM-level check (Python html.parser) on the served page: 109 <p>, 0 over 24 words, Playground 0. NOT DONE: real browser render (screenshots) + provider-add click-through — no live browser in this VM, Chromium refuses to start ("Hatch WebGL domain policy is required for browser startup"). Items 110/128 stay open until a live browser actually clicks. Gate section below. | ✅ | ⬜ |

**Reading note.** Items 110 and 128 are the *same* click-through recorded twice (110 from
the form-verification slice, 128 restating it with the VM browser-failure detail).
**One live click-through pass satisfies both** — run §4 once and record it against both
item numbers. Item 297 is the wider job: every dashboard section checked in a real
browser render (screenshots), with the provider-add click-through re-attempted in the
same pass. Nothing in rows 110/128/297 is ambiguous about *what* is wanted; the only
thing missing on the VM was a working browser, which is why this package exists.

**What was already verified by other means (do NOT re-do as if unproven, do NOT claim
the click is done):** the form's JS is syntax-checked; the exact `POST /api/providers`
request shapes were driven by curl; every response branch was exercised; a real chat
returned 200 (item 128). The live-browser pass only has to prove the *browser* part:
a human/agent typing into the real fields and clicking the real button in a real
render, and the dashboard reacting as the curl tests predict.

## 2. Confirmed facts — verified from code in this tree, 2026-11-19

Do not re-derive these at run time; if the tree has moved on, re-verify the cited
file:line first and note the drift in the results template.

| Fact | Value | Evidence (file:line) |
|---|---|---|
| Dashboard URL | `http://127.0.0.1:20128/dashboard` | Default listen `127.0.0.1:20128` — `internal/config/config.go:52` (`DefaultListen`), `:55` (`DefaultPort = 20128`); startup prints `dashboard: http://%s/dashboard` — `cmd/codvian-router/main.go:4108`; route `GET /dashboard` — `internal/server/server.go:1368`, handler `handleDashboard` at `:1913`, serves embedded `web/index.html` (`web/web.go:17`, `//go:embed index.html`) |
| Router version | `0.1.0` | `cmd/codvian-router/main.go:80` (`const Version = "0.1.0"`) |
| Binary (Windows) | `codvian-router.exe` | Build: `go build -o codvian-router.exe ./cmd/codvian-router` — `docs/INSTALL.md:92`; Linux equivalent `go build -o codvian-router ./cmd/codvian-router` — `docs/INSTALL.md:39` |
| Start command | `codvian-router.exe serve` (default subcommand is `serve` — `cmd/codvian-router/main.go:517`) | `docs/INSTALL.md:39-45` |
| Config / data dir (Windows) | config `%AppData%\codvian-router\config.json`; data `%USERPROFILE%\.local\share\codvian-router` | `docs/INSTALL.md:92` |
| Auth | ON by default; dashboard mutating calls need a gateway key (`Authorization: Bearer crk_…`) | `cmd/codvian-router/main.go:4128-4130` startup lines; form comment `web/index.html:1039-1044` |
| First key — `init` path | `codvian-router.exe init` creates config + first gateway key, shown ONCE, cannot be recovered | `docs/INSTALL.md:39-56` |
| First key — bootstrap path | `POST /admin/bootstrap`, loopback-only, only while no gateway key exists; second call → 409; returns `{"gateway_key": …}` once | `internal/server/guard.go:358` (classification), handler `internal/server/guard.go:653-690`; startup line `cmd/codvian-router/main.go:4128`: `bootstrap: PENDING (no gateway key yet — POST /admin/bootstrap, loopback-only, issues the first key once)` |
| Provider-add endpoint | `POST /api/providers` (+ `GET /api/providers`, `DELETE /api/providers/{id}`, `GET /api/catalog`) | `internal/server/server.go:1434-1440` |
| Provider-add live test | Server tests the key live against the upstream (`/v1/models` probe) before storing it encrypted | `web/index.html:1039-1044` comment; `internal/server/providers_add.go:84-85`, `:147` |
| Catalog total (2026-11-18) | 614 live entries from `GET /api/catalog` | Item 297 row text (§1); re-check live at run time, never hard-code |
| Dashboard file size at packaging | `web/index.html`, 10,205 lines | Measured 2026-11-19 in this tree |

## 3. Preconditions — all must be true before step 1 of §4

Check in order. If any fails, STOP and record it in the results template as
"blocked on precondition" — do not improvise around it.

1. **Pairing approved by Raunak.** Status today: DECLINED FOR NOW (see header).
   The future run starts only after he approves the Tailscale + OpenSSH Server
   setup and it is verified working (SSH command succeeds from the VM).
2. **Router binary built from the current tree on his PC.** From PowerShell in the
   synced worktree: `go build -o codvian-router.exe ./cmd/codvian-router`. If Go
   is not installed on his PC, cross-compile on the VM (`GOOS=windows go build`)
   and copy the `.exe` over with `scp` — then note that in the results template.
   Never run a stale binary from an earlier date and call it the current build.
3. **Router running:** `.\codvian-router.exe serve` in PowerShell. Confirm the
   startup lines show `listening on 127.0.0.1:20128`, `dashboard:
   http://127.0.0.1:20128/dashboard`, and either `bootstrap: complete` or
   `bootstrap: PENDING`. **Do NOT use `--headless`** — headless mode serves no
   dashboard (`/dashboard*` answers 404; `cmd/codvian-router/main.go:4101-4105`).
4. **A gateway key exists and is in the agent's hands for this run only.**
   Either it already exists (from the 2026-11-18 live run or `init`), or — only if
   the startup line says `bootstrap: PENDING` — issue the first key with
   `POST /admin/bootstrap` from the PC itself (loopback-only; e.g.
   `curl -X POST http://127.0.0.1:20128/admin/bootstrap` in PowerShell, or an
   equivalent loopback POST). Save the returned key immediately: it is shown once
   and cannot be recovered. If bootstrap answers 409, a key already exists — do
   NOT re-bootstrap; use the existing key. **Never paste any key into a file,
   log, screenshot, or the results template.** Keys are typed fresh per action by
   the browser automation and cleared after the request (that is the form's own
   behaviour — `web/index.html:4027-4043`).
5. **A headless browser driver is available on his PC.** At run time the agent
   installs/uses headless Chromium (or Playwright, which drives Chromium) on the
   PC — the exact driver is chosen at execution time and recorded in the results
   template, because no Playwright/puppeteer tooling exists on the VM today and
   none is pre-installed on his PC by this package. Minimum capability required:
   load a page, type into fields, click, wait for network idle, take a PNG
   screenshot. Plain `chrome --headless=new --screenshot=…` suffices for the
   render shots in §5; the click-through in §4 needs CDP/Playwright, not the
   `--screenshot` one-shot flag.
6. **Test-provider decision (item 110/128 only).** Preferred: **Ollama**
   (catalog id `ollama-local`, verified entry, no auth, base URL
   `http://localhost:11434` — `internal/catalog/verified.go:288-300`). The form
   itself says "No-key local providers (e.g. Ollama) work via the Catalog picker"
   (`web/index.html:1080`). **Condition:** Ollama must actually be running on his
   PC and serving `/v1/models` — the server live-tests the key/probe before
   storing, so if Ollama is not running the add will fail and that is an
   *environment* failure, not a dashboard bug; record it as such and do not
   force a PASS. If Ollama is not running, either start it first (only with
   Raunak's approval at the time — it is his PC) or use a keyed provider whose
   API key Raunak supplies at run time. **Never invent, borrow, or hard-code a
   provider key.** If no test provider can be added honestly, items 110/128 stay
   open and the run records why.
7. **Dashboard loads:** `GET http://127.0.0.1:20128/dashboard` returns 200 and the
   served bytes are the current `web/index.html` (spot-check: the page contains
   `#catalogCard`). If it 404s, the router is probably in `--headless` mode —
   restart without it (precondition 3).

## 4. Click-through steps — executed by the agent over SSH, headless browser

General rules for every step: drive the **real rendered page** at
`http://127.0.0.1:20128/dashboard` (never `curl` the API as a substitute for a
click — that is what items 110/128 are *not*); wait for network idle after each
navigation/click before judging what is visible; take the screenshot named in §5
for that step; record the observed result verbatim in `RESULTS-TEMPLATE.md`.
Screenshots go to `docs/packages/live-browser/screenshots/` in the worktree
(copied back over `scp` after the run).

### 4A. Item 297 — full-section render sweep (do first; it also proves the dashboard loads)

For **each** nav section that has a real section element, click its nav button
(`#mainNav .nav-item[data-view="<name>"]`), wait for the section to become visible
(`.view.on` on `#view-<name>`), and screenshot it. Sections and their verified
element ids (`web/index.html` line numbers measured 2026-11-19):

| # | Nav (`data-view`) | Section id (line) | What must be visible |
|---|---|---|---|
| 1 | `overview` | `#view-overview` (:720) | Exactly 4 KPI cards (`data-kpi` prov/mod/req/route, :749-766); Providers connected `0 / 0`, pill "None yet", note "Add a provider to begin"; Models live `0`, "None yet"; Requests `0`; Routing status shows live strategy or "—"; quick-start card `#quickStartCard`; chart empty state `#chartEmpty` ("No requests yet — …") if zero traffic |
| 2 | `providers` | `#view-providers` (:1006) | `#addProviderCard`, `#freeTierCard`, `#catalogCard` with `#catalogCount` showing the live total (614 on 2026-11-18 — re-check, never hard-code); configured-pinned-on-top area `#provGrid` empty at 0 providers; `#provVertexLine` "No Vertex AI providers configured — add one from the catalog." |
| 3 | `models` | `#view-models` (:1166) | Table with `#modelBody` empty and `#modelEmpty` visible: "No models yet — connect a provider first." + "Add a provider" button (`data-goto="providers"`); `#modelCount`; `#modelSyncStatus` |
| 4 | `autorouter` | `#view-autorouter` (:1251) | Live classification card; virtual models labelled as virtual (auto, auto/fast, auto/cheap, auto/coding, auto/reasoning) |
| 5 | `strategies` | `#view-strategies` (:1356) | Strategy picker listing only backend-accepted names (balanced, cheapest, cheap, fastest, fast, most-reliable, reliable, least-latency, latency, weighted); **no `code` option** |
| 6 | `cooldown` | `#view-cooldown` (:1426) | Global + per-chain modes from live feed |
| 7 | `coalescing` | `#view-coalescing` (:1493) | Live single-flight counters (zeros at idle are honest, not "—" invented) |
| 8 | `concurrency` | `#view-concurrency` (:1542) | Live limiter state |
| 9 | `pools` | `#view-pools` (:1585) | Live per-(provider,key) pool state; empty at 0 providers |
| 10 | `guard` | `#view-guard` (:1629) | Tier 1/2/3 tables from live feed. **Note:** no nav button exists for this section — reach it only via a `data-goto="guard"` link if one is present, else load it by calling the view switch directly in the driver (`go("guard")` equivalent). Record how it was reached. |
| 11 | `guardrails` | `#view-guardrails` (:1773) | Toggles table with `#grTogglePII` (:1901), `#grToggleSecrets` (:1906), `#grToggleInjection` (:1911) |
| 12 | `plugins` | `#view-plugins` (:1941) | Registry snapshot; honest zero state at 0 plugins |
| 13 | `mcp` | `#view-mcp` (:2017) | Live tool list + counts |
| 14 | `a2a` | `#view-a2a` (:2129) | A2A status; "A2A is disabled" empty state if disabled (enabled=false, all-zero counts, protocol version shown) |
| 15 | `keys` | `#view-keys` (:2186) | `#routerKeyCount`, `#routerKeys` rows, `#newKeyBtn`; `#routerKeysEmpty` only if genuinely zero keys |
| 16 | `logs` | `#view-logs` (:2242) | Request-logs table, all columns incl. Reasoning/Cached/Saved; empty at zero traffic |
| 17 | `usage` | `#view-usage` (:2336) | Spend/tokens/failovers; zeros at idle, never invented numbers |
| 18 | `benchmark` | `#view-benchmark` (:2566) | Latest run + history; missed-target verdicts shown honestly if present |
| 19 | `verification` | `#view-verification` (:2717) | `#verifProven`, `#verifUnproven`, `#verifRows` table, `#verifNote` |
| 20 | `settings` | `#view-settings` (:2765) | Strategy picker `#strategySel`; see Known Issue K3 before judging the toggles/port |
| 21 | `cache` | `#view-cache` (:2891) | Per-layer stats + savings; zeros at idle |
| 22 | `combos` | `#view-combos` (:2967) | "No combos yet — create one" empty state at 0 combos (not an empty table) |

**PASS for item 297** (all must hold): every section above renders in a real browser
with no blank page, no JS console error, no endlessly-spinning "Loading…", and its
content matches its live feed (spot-check at least Overview KPIs vs
`GET /dashboard/overview`, catalog count vs `GET /api/catalog`, models table vs
`GET /dashboard/models`); every screenshot in §5 exists and shows the named section;
the provider-add click-through (§4B) was re-attempted in the same pass. **FAIL** if
any section renders blank/errors, shows invented numbers, or a screenshot is missing.
Orphaned nav buttons (§6, K1) are recorded as a known issue, not as a 297 FAIL on
their own — but if clicking one throws a JS error, that *is* a FAIL: record the
console error verbatim.

**Honest-empty-state expectations (0 providers configured, the state Raunak himself
saw on 2026-11-18 ~11:50 IST):** Overview KPIs read `0`, pills "None yet" / "No
providers", notes "Add a provider to begin" / "No connected providers"; Providers
shows the catalog as addable groups with 0 configured; Models shows one line
"No models yet — connect a provider first."; chart shows "No requests yet — …".
Empty must read as empty. Any non-zero number with 0 providers, any invented
provider/model name, or any "· saved" toast for an action that did not persist is a
FAIL — screenshot it and quote it in the results.

### 4B. Items 110 + 128 — provider-add form click-through (one pass closes both)

Context: Raunak himself ran the Windows exe on 2026-11-18 ~11:50 IST (router v0.1.0,
`127.0.0.1:20128`, auth ON, 0 providers, bootstrap PENDING) and **declined the
live-browser provider-add click-through at the time** — so 110/128 stay pending by
his choice. This package is neutral preparation, not a nag: the run below happens
only if/when he approves the pairing work in future.

Preconditions §3.6 (test provider) and §3.4 (gateway key) must hold first.

| Step | Action (agent, headless browser) | Element | Expected observation |
|---|---|---|---|
| 110-01 | Open `http://127.0.0.1:20128/dashboard`, click nav `Providers` | `#mainNav [data-view="providers"]`, `#view-providers` | Providers section visible; screenshot `110-01-dashboard-empty.png` |
| 110-02 | Read the catalog: `#catalogCount` text, and `#catalogListBody` rows | `#catalogCount`, `#catalogSearch`, `#catalogListBody` | Count equals live `GET /api/catalog` total; rows are name + Add button; ToS-grey rows (if any shown) have disabled Add |
| 110-03 | In `#catalogSearch`, type `ollama` | `#catalogSearch` | List filters to Ollama rows only; screenshot `110-03-catalog-search-ollama.png` |
| 110-04 | Click Ollama's row Add button (pre-fills the form above) | row Add button in `#catalogListBody` | `#addProvCatalog` now shows Ollama (`ollama-local`); `#addProvCatalogHint` shows its hint; screenshot `110-04-form-prefilled.png` |
| 110-05 | Type the gateway key into `#addProvGwKey` (leave `#addProvKey` empty — Ollama needs no key) | `#addProvGwKey` | Field holds the pasted key (masked); no error shown |
| 110-06 | Click **Add provider**; wait for the button to return to "Add provider" (it shows "Testing key…" while the live probe runs) | `#addProvSubmit` (:1089) | Toast: `Added Ollama — N models, <wire> wire` (N = models returned by the live `/v1/models` probe); `#addProvKey` and `#addProvGwKey` are cleared by the page; screenshot `110-06-added-toast.png` |
| 110-07 | Verify the add landed: providers list now shows Ollama pinned on top with live status; `#catalogConfiguredCount` reads `1 configured` | `#provGrid`, `#catalogConfiguredCount` | Ollama present with status from `GET /api/providers`; screenshot `110-07-provider-listed.png` |
| 110-08 | Click nav `Models`; check the table | `#view-models`, `#modelBody`, `#modelCount` | Ollama's models appear as table rows (count matches `GET /dashboard/models`); screenshot `110-08-models-after-add.png` |
| 110-09 | Click nav `Overview`; check KPIs | `data-kpi` prov/mod | Providers connected now `1 / 1` (or live/total per feed), Models live = Ollama model count; screenshot `110-09-overview-after-add.png` |
| 110-10 | **Cleanup decision (record it):** either delete the test provider now (provider card remove control in `#provGrid` → typed phrase `DELETE <id>` + gateway key typed fresh — the typed phrase is required, `web/index.html:3485`) and confirm the list is back to 0, **or** leave Ollama configured because Raunak wants it. Default: delete it, so the run leaves his machine as found. Screenshot the final state `110-10-cleanup.png` | `#provGrid` remove control | After delete: 0 providers, KPIs back to empty state; or, if kept: note "kept at Raunak's request" in results |

**What the click actually does (so the agent can tell a real success from a fake
one):** the submit handler (`web/index.html:3989-4050`) reads `#addProvGwKey`
(required — empty shows inline `#addProvError` "Paste your gateway key — …"),
builds `{"catalog_id": "ollama-local"}` (plus optional `endpoint`/`variant`/
`base_url` overrides if filled), and `fetch`es `POST /api/providers` with
`Authorization: Bearer <gateway key>`. On success it toasts
`Added <name> — <N> models, <wire> wire` (plus " — added and saved, but restart the
router to route through it" if `routing_live === false`), clears both key fields,
and reloads the providers + models feeds. On failure it shows the server's error
message inline in `#addProvError` — **that inline error, not a toast, is the
failure signal**.

**PASS for items 110 + 128** (all must hold): steps 110-01–110-09 completed by typing
and clicking in the real rendered page (not by `curl`); the success toast appeared
with a model count matching the live probe; the provider appeared in the list and
its models in the Models table; Overview KPIs moved from empty to the new live
values; screenshots 110-01–110-10 exist. **FAIL** if: the button does nothing; an
inline error appears (quote it — e.g. a 401 means the gateway key was wrong, a probe
failure means Ollama was not reachable, a ToS-grey rejection means the wrong catalog
row was picked); the toast claims success but the provider/models do not appear;
or any step had to be done via `curl`/DevTools console instead of the UI. A FAIL
caused by Ollama not running is an *environment* FAIL — record it as such, do not
relabel it a dashboard bug, and do not retry with a different provider unless
Raunak supplies a key.

**Negative checks (do these in the same pass, they are cheap and they are what the
curl tests could not prove):** with the gateway-key field empty, click Add provider
→ inline error "Paste your gateway key — …" must appear and **no** request may be
sent (screenshot `110-n1-no-gwkey-error.png`); with Catalog mode and no catalog
provider picked, click Add provider → inline error "Pick a catalog provider, or
switch to Custom." (screenshot `110-n2-no-catalog-error.png`). Both are PASS if the
inline error shows and nothing is added; FAIL if a request is sent or a success
toast appears.

## 5. Screenshots to capture

Full shot list with filenames and what must be visible: see
[`SHOT-LIST.md`](SHOT-LIST.md) in this package (36 shots: `297-00` setup,
`297-01`–`297-22` for the section sweep, `297-23` orphan-nav, `110-01`–`110-10`
for the click-through, `110-n1`–`110-n2` for the negative checks).
Save every PNG to `docs/packages/live-browser/screenshots/` in the worktree
(copied back over `scp` after the run). Filename convention:
`<item>-<step>-<slug>.png`, e.g. `110-01-dashboard-empty.png`. Viewport: 1440×900
minimum, light or dark theme as the page defaults — record which in the results.
Every screenshot must show the full viewport including the nav sidebar and the
section heading; a cropped fragment that hides the nav does not count.

## 6. Known issues to watch for (verified in this tree — not blockers, record if seen)

- **K1 — Orphaned nav buttons (8), no section.** Nav buttons exist with NO matching
  `<section id="view-…">`: `activity` (`web/index.html:659`), `resilience` (:635),
  `health` (:639), `webhooks` (:643), `analytics` (:647), `batch` (:651),
  `responses` (:655), `audit` (:663). Verified by grep 2026-11-19: each has 1 nav
  match, 0 section matches. The `go()` handler (`web/index.html:3080`) returns
  early — `var target = $("#view-" + view); if(!target) return;` — so clicking one
  silently does nothing: no section change, no error, not even the nav active-state
  update. This is the M10-0 orphaned-nav situation (the plan slotted these sections
  for later M10 slices). **Watch for:** click each once during the 297 sweep and
  record "no-op, no error" (expected) vs any console error (FAIL). Do NOT click
  them expecting a section. (The task brief named `data-view="activity"` as the
  example; the full orphaned set is eight, listed above.)
- **K2 — Sections with no nav button.** `view-guard` (:1629) and `view-guardrails`
  (:1773) exist but have no nav button (the nav list at :548-663 skips them; the
  `titles` map at :3077 still lists them). Reachable only via `data-goto` links or
  a direct view switch. Record how each was opened.
- **K3 — Settings: 5 display-only toggles + fake Port value.** Settings
  (`#view-settings`, :2765-2891) has switches labelled Automatic failover,
  Prompt-cache pinning, Cost-aware routing, Health checks, Request logging that
  flip `aria-checked` and toast "· saved" with no fetch and no persistence (M10
  plan §2a row 20; the switch buttons are visible in the section markup). The Port
  input is hard-coded `8080` (`web/index.html:2820`) while the real default is
  20128, and the Listen-address row (:2815) has no handler. **Watch for:** do NOT
  treat a "saved" toast after flipping one of these as a PASS for anything — it
  proves nothing. Record the behaviour as observed; fixing it is M10-14's job,
  not this run's.
- **K4 — "Playground" must be 0.** Item 297 requires zero occurrences of
  "Playground" in the rendered dashboard. If any screenshot or the served page
  shows one, that is a 297 FAIL — record it verbatim.
- **K5 — Catalog total is live, not 614.** Item 297 cites 614 as the 2026-11-18
  live total. If the run-time total differs, that is drift, not failure — record
  the live number and move on.

## 7. Results

Fill in [`RESULTS-TEMPLATE.md`](RESULTS-TEMPLATE.md) during the run — environment
block first, then one row per item with steps done, observed result, PASS/FAIL,
screenshot filenames, and notes. **Never paste a gateway key or a provider API key
into the template, a screenshot, or a log.** If a screenshot accidentally captures
a typed key, retake it with the field cleared and delete the bad file.

## 8. File inventory (this package)

| File | Purpose |
|---|---|
| `README.md` (this file) | Operator runbook: status, quoted rows, confirmed facts, preconditions, steps, PASS/FAIL, known issues |
| `SHOT-LIST.md` | Explicit shot list: one shot per step, filename, what must be visible, save location |
| `RESULTS-TEMPLATE.md` | Per-item results tables + environment block, to fill in during the run |
| `screenshots/` | Destination for the future run's PNG captures (empty until the run) |
