# live-browser-package — CHECKLIST (package-local)

Tick each item as it is done in the real browser. This file is local
to the package — it does NOT replace repo `CHECKLIST.md`.
"Pass" means: seen with human eyes in a real browser on Raunak's
computer, with a screenshot filed. Anything verified by curl / code
reading before does NOT count toward these items.

## Item 110 — Dashboard provider-add form live-browser click-through

Repo status: NOT DONE ❌ (CHECKLIST.md:119).
What it requires: typing into the fields and clicking "Add provider"
in a real browser. Everything the click would do was verified by
other means (JS syntax-checked, exact requests driven by curl, all
response branches exercised), but a genuine click-through still
needs an agent with live-browser access.

- [ ] Dashboard opened at `http://127.0.0.1:20128/dashboard`
      (or noted `--port` value) — Screenshot 1
- [ ] Providers view opened; "Add a provider" card (`#addProviderCard`)
      visible — Screenshot 3
- [ ] Source set to Custom; Base URL `http://127.0.0.1:<mock-port>`
      typed; Display name typed; Provider API key typed;
      Gateway key pasted — Screenshot 4 (filled form, before click)
- [ ] "Add provider" clicked (button showed "Testing key…" while running)
- [ ] Success toast seen: `Added <name> — 1 model, openai wire`
      (+ "added and saved, but restart the router to route through it"
      ONLY if `routing_live === false`) — Screenshot 5
- [ ] Both key fields EMPTY after the click (cleared on success)
- [ ] New provider appears in the provider list with its badge
- [ ] `model-a` listed in Models — Screenshot 6
- [ ] Real chat request through the just-added provider returns
      HTTP 200 (Step 3c) — status recorded: ___
- [ ] OR, if it failed: red `#addProvError` text + HTTP status
      recorded verbatim, no retry with invented values — Screenshot 5-alt

## Item 128 — Dashboard provider-add form live-browser click-through (restated)

Repo status: NOT DONE ❌ (CHECKLIST.md:135).
What it requires: the same genuine click-through as item 110. The
VM blocker was specific: the only browser binary present refuses to
start ("Hatch WebGL domain policy is required for browser startup")
and there is no puppeteer/playwright/WebSocket client tooling.
This item closes when item 110's steps above are done in a real
browser — it is the same click, recorded once, satisfying both rows.

- [ ] Item 110 steps above completed in a real browser
      (same screenshots satisfy both items — do not repeat the run
      just to tick a second box)

## Item 297 — Dashboard simplification, live verification

Repo status: Built ✅ / Verified ⬜ (CHECKLIST.md:308).
What it requires: new dashboard served from the built binary and
every section checked in a real browser render (screenshots);
provider-add form click-through re-attempted in the same pass.
Already verified without a browser: dashboard byte-identical to
`web/index.html` (sha256); ids `#catalogCard`, `#catalogListBody`,
`#catalogSearch`, `#catalogCount`, `#modelsBadge`, `#kpiReqLabel`,
`data-kpi` prov/mod/req/route, `#grTogglePII` / `#grToggleSecrets` /
`#grToggleInjection` present in the served page; `GET /api/catalog`
live total 614; `/api/providers` + `/dashboard/models` +
`/dashboard/overview` respond; DOM-level check: 109 `<p>`, 0 over
24 words; "Playground" 0 occurrences. NOT DONE: real browser render
(screenshots) + provider-add click-through.

- [ ] Overview renders (Screenshot 1) + scrolled System metrics
      (Screenshot 2); "Playground" absent
- [ ] Providers renders: `#catalogCard`, `#catalogSearch`,
      `#catalogCount` (live count, not hard-coded), addable catalog
      rows, `#freeTierCard` (Screenshot 297-providers)
- [ ] Models renders; `model-a` listed after the add (Screenshot 6)
- [ ] Auto-router / Strategies / Cooldown / Coalescing /
      Concurrency / Pools each render (screenshots 297-<view>)
- [ ] Plugins / MCP / A2A each render; A2A status pill visible
- [ ] API Keys / Logs / Usage each render
- [ ] Benchmark renders — new run shown if the bench ran
      (Step 5), else honest empty state (Screenshot 297-benchmark)
- [ ] Verification / Settings / Combos / Cache / Resilience /
      Health / Webhooks / Analytics / Batch / Responses /
      Activity / Audit each render (screenshots 297-<view>)
- [ ] Settings Port field recorded as-is (known wrong `8080`,
      `internal parity tracker:812`) — screenshot, not fixed in-session
- [ ] No section shows a paragraph over 2 lines (item 293);
      any long paragraph screenshotted + recorded
- [ ] No browser console error blocks rendering; any console
      errors recorded verbatim in `live-browser-notes.txt`
- [ ] Provider-add click-through re-attempted in this pass
      (items 110/128 above)
