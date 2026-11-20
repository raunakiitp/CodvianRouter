# live-browser-package — live-browser click-throughs (CHECKLIST items 110 / 128 / 297)

PREPARATION ONLY. Nothing here has been run in a browser, and no
browser work was attempted from the VM. It is ready to run on
Raunak's own Windows computer (which has a working browser — the
VM's Chromium refuses to start) the moment that computer is paired
/ the worktree is on it.

## What this package is

The genuine live-browser work the VM cannot do, for three
CHECKLIST items that are all open on exactly the same blocker:

- **Item 110** (CHECKLIST.md:119): provider-add form live-browser
  click-through — typing into the fields and clicking "Add provider"
  in a real browser was never performed.
- **Item 128** (CHECKLIST.md:135): same click-through, re-stated —
  the VM's only browser binary refuses to start ("Hatch WebGL domain
  policy is required for browser startup"), and there is no
  puppeteer/playwright/WebSocket client tooling to drive it anyway.
- **Item 297** (CHECKLIST.md:308): dashboard live verification —
  served + DOM-checked, but the real-browser render (screenshots)
  + provider-add click-through were never done.

Everything the click would do was already verified by other means
(item 109 / 127: JS syntax-checked, the exact request shapes driven
by curl, every response branch exercised, real chat 200 through a
just-added provider). What is missing — and what this package does —
is the genuine click in a real browser, plus screenshots proving the
dashboard renders.

## Machine

- Raunak's Windows computer (browser on the same machine as the
  router). **GPU note:** the task brief says "NVIDIA GTX 1050 6GB";
  `MEMORY.md` as updated 2026-11-19 says "NVIDIA RTX 4050, 6 GB".
  Confirm which; it does not affect any step below.
- **UNVERIFIED — getting the worktree onto his computer:** the
  worktree exists only on this VM; the repo is private and nothing
  is pushed (bulk-push rule stands), so a GitHub clone will NOT
  contain this work. Transfer method is TO-CONFIRM at pairing time.
  Do not invent one.

## The dashboard URL — how it differs on his computer

It does not differ except for whose loopback it is.

- Default listen address, verified in code:
  `internal/config/config.go:51-55` —
  `DefaultListen = "127.0.0.1:20128"`, `DefaultPort = 20128`.
  Confirmed in docs: `docs/INSTALL.md:41`
  ("`./codvian-router serve` — listens on 127.0.0.1:20128, auth ON")
  and `docs/ARCHITECTURE.md:67` ("Defaults: listen `127.0.0.1:20128`").
- The dashboard is served locally by the router binary itself
  (`GET /dashboard`, `internal/server/server.go:1368`,
  `s.handleDashboard`). On his computer he runs the same binary
  with the same default config, so the URL is the same:
  **`http://127.0.0.1:20128/dashboard`**
  (open in the browser on that same machine).
- If the router was started with `--port <N>` or `--listen ...`
  (flags parsed in `cmd/codvian-router/main.go:156+`, `--port` /
  `--listen` / `--config`), use that port instead and note it.
  The default is 20128; a Settings-page port field hard-coded to
  `8080` in the page is a known wrong value
  (`docs/GAP-INVENTORY.md:651`, `docs/PARITY.md:812`) — do NOT
  use 8080.
- **UNCONFIRMED:** whether his machine already has an initialized
  router (config + gateway key) or needs a fresh `init`. If fresh,
  see Step 1.

## Step 1 — Start the router (on his computer)

From the worktree root (in WSL, or native Windows — the Go binary
builds for Windows; the exact build command on his machine is
TO-CONFIRM with the transfer method, since the VM builds Linux
binaries):

```bash
# Build the router from this worktree (Linux/WSL form, verified
# from scripts/bench-geekflare.sh:112):
go build -o codvian-router ./cmd/codvian-router
```

Then, in a terminal that stays open:

```bash
# First time only, if no config exists yet:
./codvian-router init
# Prints: "Gateway key (shown ONCE — save it now, it cannot be
# recovered):" followed by the key
# (cmd/codvian-router/main.go:4464). SAVE THE KEY. It is shown once.
# If a config already exists, `init` refuses — use the existing key.

./codvian-router serve
# Default: listens on 127.0.0.1:20128, auth ON.
# Startup prints the dashboard URL; confirm it says 127.0.0.1:20128.
```

Keep this terminal open for the whole session. Verify in the
browser first: `http://127.0.0.1:20128/healthz` should return
`{"status":"ok"}` (no key needed — auth-exempt liveness probe).

**Do NOT paste any real key into chat or into these package files.**
The gateway key is typed into the dashboard form only, and the form
clears both keys on success AND on failure (verified JS,
`web/index.html:4027-4043`).

## Step 2 — Open the dashboard and check it renders (item 297, part 1)

1. Open **`http://127.0.0.1:20128/dashboard`** in a real browser
   (Chrome/Edge/Firefox on his machine — any real browser; the VM
   has none that starts).
2. **Screenshot 1 — Overview:** the page loads (HTTP 200), title /
   "Codvian Router" + "Overview" visible, sidebar nav visible
   (Overview, Providers, Models, Auto-router, Strategies, Cooldown,
   Coalescing, Concurrency, Pools, Plugins, MCP, A2A, API Keys, Logs,
   Usage, Benchmark, Verification, Settings, Combos, Cache,
   Resilience, Health, Webhooks, Analytics, Batch, Responses,
   Activity, Audit — `web/index.html:548-667`). "Playground" must
   NOT appear anywhere (deliberate divergence — removed).
3. Check the Overview cards render with real state, not errors:
   Quick start (4 steps), Traffic, Token mix, Prompt cache,
   Provider topology, Live routing, System metrics, Provider quota,
   Recent requests (`web/index.html:774-1000`).
4. **Screenshot 2 — Overview scrolled** showing the System metrics
   card ("Live process · recorded benchmark").

What success looks like: a rendered page, not a blank page, not a
connection error, no browser console errors that block rendering.
Note any console errors verbatim in the run notes (open DevTools →
Console) — do not dismiss them.

## Step 3 — Provider-add click-through (items 110 / 128)

This is the step the VM could never do. Do it exactly as written —
every field id and label below was read from `web/index.html`,
not guessed.

### 3a — Get a safe upstream to add (no paid key needed)

The form tests the provider key live against the upstream before
storing it (card subtitle, `web/index.html:1046`), so the add needs
a reachable upstream. The no-cost path, verified end-to-end by curl
in item 109 (201 custom add, `routing_live:true`, real chat 200
through the just-added provider with no restart):

1. In a second terminal, start the instant local mock from this
   worktree:
   ```bash
   go build -o codvian-bench ./cmd/codvian-bench
   ./codvian-bench mock --port 21099
   # Subcommand + --port flag verified: cmd/codvian-bench/main.go
   # runMock (flag `--port`, required). Port 21099 is an example
   # free port (used by VM smokes); any free port works — note the
   # one you used. The mock serves GET /v1/models and
   # POST /v1/chat/completions with a fixed tiny reply, zero delay.
   ```
2. Confirm it answers: open `http://127.0.0.1:21099/v1/models`
   in the browser — expect
   `{"object":"list","data":[{"id":"model-a","object":"model"}]}`
   (`cmd/codvian-bench/main.go:modelsBody`).
3. Alternative (optional, only if Raunak prefers): add a REAL
   provider from the catalog with his own real provider key.
   That also satisfies the click-through. Never use a key he does
   not want stored — the mock path exists precisely so no real
   key is needed.

### 3b — The clicks, in order

1. In the dashboard, click **Providers** in the left nav
   (`data-view="providers"`, `web/index.html:552`).
2. **Screenshot 3 — Providers before:** the "Add a provider" card
   (`#addProviderCard`, `web/index.html:1045`) visible with the
   Source dropdown, fields, and "Add provider" button.
   Note the empty state if no providers are configured yet:
   "No providers connected yet" (honest empty state — item 62).
3. In the "Add a provider" card:
   - **Source** (`#addProvMode`): select **Custom — any
     OpenAI-compatible base URL**. (Catalog mode is the default;
     the mock is not in the catalog, so Custom is the correct mode
     for this test. The catalog path — picking an entry from
     `#addProvCatalog`, fed live from `GET /api/catalog` — is a
     second, optional click-through; see Step 3d.)
   - **Base URL** (`#addProvBaseUrl`, label changes to "— required"
     in Custom mode): type `http://127.0.0.1:21099`
     (or the port actually used in 3a).
   - **Display name** (`#addProvName`, appears in Custom mode):
     type e.g. `Mock local` (optional — derived from the base URL
     when empty).
   - **Provider API key** (`#addProvKey`): type any non-empty
     string, e.g. `mock-key-a` (the mock ignores it; Custom mode
     requires a key — empty key shows "Provider API key is
     required for a custom provider.").
   - **Gateway key** (`#addProvGwKey`, placeholder `crk_…`):
     paste the gateway key from Step 1 (shown once by `init`).
     Empty gateway key shows "Paste your gateway key — adding a
     provider requires gateway-key auth."
4. **Screenshot 4 — Form filled:** all five fields filled, before
   clicking. The screenshot must show the filled Base URL and the
   "Add provider" button (`#addProvSubmit`, label "Add provider").
   (Keys are password fields — their values will show as dots;
   that is correct and expected. Do NOT screenshot a page that
   exposes a real key in plain text.)
5. Click **"Add provider"**. While it runs the button reads
   "Testing key…" and is disabled (verified JS).
6. **Screenshot 5 — Success toast:** on success a toast reads
   `Added <name> — 1 model, openai wire` (exact format verified,
   `web/index.html:4027-4043`: `"Added " + name + " — " + N +
   " model(s), " + wire + " wire"`), plus
   " — added and saved, but restart the router to route through it"
   ONLY if `routing_live === false`. Both key fields must be
   EMPTY after the click (cleared on success AND on failure).
   The new provider must appear in the provider list with its
   verification badge, and its model (`model-a`) in Models.
7. If it fails instead: **Screenshot 5-alt — Error shown:**
   the red `#addProvError` text under the form must show the
   server's error verbatim (e.g. 401 / 502 / 409 / 403 wording),
   never a toast pretending the add worked. Record the exact
   message + HTTP status (DevTools → Network → `POST /api/providers`)
   and STOP — that failure is the result; do not retry with
   different invented values.

What success looks like (the bar from item 109, now by click):
201, toast with the real name / model count / wire, provider in
the list, `model-a` listed in `/dashboard/models`, and a real chat
request through it returning 200 (Step 4).

### 3c — Prove the added provider routes (same session)

1. Click **Models** in the nav; confirm `model-a` is listed.
   **Screenshot 6 — Models:** `model-a` visible.
2. Send one real chat request through the router (any client —
   curl from a terminal is fine; this part is not a browser step):
   ```bash
   curl -s http://127.0.0.1:20128/v1/chat/completions \
     -H "Authorization: Bearer <gateway-key>" \
     -H "Content-Type: application/json" \
     -d '{"model":"model-a","messages":[{"role":"user","content":"hello"}],"stream":false}'
   ```
   Expect HTTP 200 with the mock's fixed reply (`"content":"ok"`).
   (Endpoint + auth shape verified in `docs/INSTALL.md:115` and
   `cmd/codvian-bench/main.go:oneRequest`.) Record the status code.
   **UNVERIFIED:** the exact curl above was not run against the
   mock-through-router path from the browser session — it is the
   item-109 shape, re-stated; if it returns non-200, record the
   exact body/status and stop.

### 3d — Optional second click-through: catalog mode

Only if time allows, and only after 3b succeeds:

1. Back on Providers, in "Add a provider", set Source to
   **Catalog provider** (`#addProvMode` = `catalog`).
2. The Catalog provider dropdown (`#addProvCatalog`) must be
   populated live from `GET /api/catalog` (614 entries per
   item 297; the count shown in `#catalogCount` comes from the
   feed, never hard-coded). Pick any non-ToS-grey entry with a
   base URL on file. ToS-grey entries are shown disabled with
   "ToS-grey — cannot be added" — clicking one must NOT add it
   (the form refuses it client-side AND the server refuses it).
3. Fill Provider API key + Gateway key, click "Add provider",
   and screenshot the toast / error exactly as in 3b.
   **Do NOT complete this with a real paid key unless Raunak
   explicitly wants that provider added.** If no suitable
   catalog entry + key is available, record "catalog path not
   attempted — no key available" and stop; 3b alone closes
   items 110/128.

## Step 4 — Dashboard live verification, every section (item 297)

Item 297 requires every section checked in a real browser render.
Work through the nav in order and screenshot each view that loads;
for views that are empty on a fresh router, the honest empty state
IS the pass (never invent data):

| # | View (nav `data-view`) | What to check |
|---|---|---|
| 1 | Overview | Step 2 above |
| 2 | Providers | Step 3 above; Catalog card `#catalogCard` with search `#catalogSearch`, count `#catalogCount`, addable rows (`web/index.html:1122`); Free Tier Providers card `#freeTierCard`; provider list |
| 3 | Models | `model-a` listed after Step 3c; pricing shown as a table/legend, not a long paragraph |
| 4 | Auto-router | Cards render (Try a routing decision, Last live decision, Feedback, Learned, Intent tiers, Suggestions) |
| 5 | Strategies | Global strategy, Last live strategy, Per-chain strategies, Strategy trees |
| 6 | Cooldown / Coalescing / Concurrency / Pools | Each view loads; empty states read as empty |
| 7 | Plugins / MCP / A2A | Each view loads; A2A status pill visible (`#a2aPill`) |
| 8 | API Keys | Provider keys card ("Stored encrypted · AES-256-GCM") |
| 9 | Logs | Request log loads (may be empty / demo-labelled per item 62) |
| 10 | Usage | Usage & Cost loads |
| 11 | Benchmark | See Step 5 |
| 12 | Verification | Verification view loads |
| 13 | Settings | Loads. **Known bug — do NOT "fix" by clicking:** the Port input is hard-coded `value="8080"` (wrong — real default is 20128) and the Listen select has no handler (`docs/PARITY.md:812`). Screenshot it as-is and record it; changing it is fleet work, not this session. |
| 14 | Combos / Cache / Resilience / Health / Webhooks / Analytics / Batch / Responses / Activity / Audit | Each view loads without a blank page or console error |

**Item 297's specific ids to confirm present** (from CHECKLIST.md:308):
`#catalogCard`, `#catalogListBody`, `#catalogSearch`,
`#catalogCount`, `#modelsBadge`, `#kpiReqLabel`,
`data-kpi` prov/mod/req/route, `#grTogglePII` / `#grToggleSecrets` /
`#grToggleInjection`. They were verified present in the served HTML
on the VM; the browser check is that they render and (for the
toggles) respond to a click.
**Paragraph check:** no section may carry a paragraph over 2 lines
(item 293 — 0 long paragraphs in the served page). If a rendered
section shows a long paragraph, screenshot it and record it.

**Screenshots 7+ — one per view** (or per group if a view is
identical to the last): filename `297-<view>.png`, e.g.
`297-providers.png`, `297-benchmark.png`. A screenshot must show
the view name + its content, not just the nav.

## Step 5 — Benchmark section (items 150 / 208 — only after the bench ran)

If `bench-package/` was run first with `CODVIAN_BENCH_OUT` set:

1. Click **Benchmark** in the nav (`data-view="benchmark"`).
2. **Screenshot — Benchmark:** the Benchmark view
   (`web/index.html:2569`, `#benchHistoryCard`,
   `#benchDefinitiveCard`, `#benchKeyBudgetCard`,
   `#benchGlobalBudgetCard`, `#benchCpuCard`, `#benchE1Card`,
   `#benchReleaseCard`) must show the NEW run: both passes with
   added-latency / RPS / idle / ready and their target verdicts
   (`added_latency_met`, `rps_met`, `ready_met`, `idle_met` from
   `GET /dashboard/benchmark`, `internal/server/benchmark.go`).
   Misses must be shown as misses.
3. If the bench has NOT run yet, the Benchmark section must show
   the honest empty state ("No benchmark run recorded yet") —
   screenshot that instead and record it; do NOT treat an empty
   state as a failure.

## Step 6 — Send everything back to the fleet

Save all screenshots in one folder on his computer, e.g.
`Desktop\codvian-router-live-browser\` (suggested location only —
any folder works; use PNG, one file per screenshot number above).
Send back:

1. **Screenshots 1–6** (required) + **7+** (one per view) +
   Benchmark screenshot (Step 5).
2. **A short notes file** (`live-browser-notes.txt`) with: date/time,
   browser used, dashboard URL used (port), whether the router was
   fresh `init` or existing, mock port used, the exact toast text
   from Step 3b, the chat-request status from 3c, any console errors
   verbatim, and anything that did not load or looked wrong.
3. **Any error text verbatim** (red `#addProvError` message, HTTP
   status from DevTools Network) if Step 3b failed — that is a
   result, not a reason to retry blindly.

Do NOT edit `CHECKLIST.md`, `web/index.html`, or any repo file based
on what the browser shows — the fleet records the verification and
fixes anything found. Do NOT push anything. Do NOT paste any key
into the notes file or chat.

## UNVERIFIED / TO-CONFIRM (do not guess these at run time)

1. How the worktree gets onto his computer (no push; clone will
   not contain this work).
2. Whether his machine already has an initialized router or needs
   a fresh `init` (Step 1 covers both).
3. The exact build command on native Windows (the Linux/WSL form
   is verified from the bench script; a native-Windows build was
   not verified from the tree).
4. GPU model: GTX 1050 (task brief) vs RTX 4050 (`MEMORY.md`).
5. The curl in Step 3c is the item-109 shape re-stated for this
   session; its exact output in this session is unrecorded until run.
6. No button label or URL in this file was invented: every id,
   label, route and toast format above cites the file:line it was
   read from. Anything not citable is marked UNVERIFIED above.
