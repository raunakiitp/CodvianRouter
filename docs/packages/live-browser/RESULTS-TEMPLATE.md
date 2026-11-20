# Live-browser run — results template (items 110, 128, 297)

Fill this in **during the run**, not after. Status: **HELD** — pairing declined for
now (Raunak, 2026-11-19 ~15:40 IST). Nothing below has been executed yet; every
field starts blank on purpose. Copy this file to
`RESULTS-2026-<date>-<pc-hostname>.md` in this same folder when a run happens, and
leave this template blank for the next run.

**Never paste a gateway key or a provider API key anywhere in this file, in a
screenshot, or in a log.** If a screenshot captures a typed key, retake it with
the field cleared and delete the bad file.

## 0. Environment (fill in first)

| Field | Value |
|---|---|
| Date / time of run (IST) | |
| Executor | Agent over SSH after pairing approval (Tailscale + OpenSSH Server) — confirm pairing approved: yes / no (if no, STOP, do not run) |
| PC | Windows version + build (e.g. `winver` output), hostname |
| PC tailnet IP used for SSH | |
| Router binary | Path on PC, built from worktree commit (record `git rev-parse HEAD` on the VM side), build command used |
| Router version | (expect `0.1.0`; record actual from startup line) |
| Router started with | Exact command (expect `.\codvian-router.exe serve`; record any flags) |
| Dashboard URL used | (expect `http://127.0.0.1:20128/dashboard`) |
| Startup lines observed | `listening on …` / `dashboard: …` / `bootstrap: complete` or `PENDING` (paste the three lines) |
| Gateway key source | `init` (pre-existing) / `POST /admin/bootstrap` this run / pre-existing key (never paste the key itself) |
| Browser driver | e.g. Playwright + Chromium version, or Chrome `--headless=new` version — exact tool + version |
| Viewport / theme | e.g. 1440×900, dark / light |
| Catalog total observed | `GET /api/catalog` total at run time (expect ~614; record actual) |
| Providers at start | (expect 0) |
| Test provider used | (expect Ollama `ollama-local`; if different, name it + why + whose key) |
| Ollama running? | yes / no — how checked (e.g. `GET http://localhost:11434/v1/models` status) |
| Test provider kept or deleted at end? | deleted / kept (whose decision) |

## 1. Item 110 — provider-add click-through (Built ❌, Verified ❌)

One live pass closes 110 **and** 128 (same click-through, recorded twice).

| Step | Done? | Observed result (verbatim: toast / inline error / counts) | Screenshot |
|---|---|---|---|
| 110-01 Open dashboard → Providers | | | `110-01-dashboard-empty.png` |
| 110-02 Read catalog list + count | | Catalog count observed: ___ (live `GET /api/catalog` total: ___) | `110-02-catalog-list.png` |
| 110-03 Search `ollama` | | | `110-03-catalog-search-ollama.png` |
| 110-04 Click Ollama Add (prefill) | | `#addProvCatalog` shows: ___ | `110-04-form-prefilled.png` |
| 110-05 Type gateway key | | | `110-05-gwkey-entered.png` |
| 110-06 Click Add provider | | Toast observed (verbatim): ___ · Models N: ___ · Wire: ___ | `110-06-added-toast.png` |
| 110-07 Provider listed, 1 configured | | | `110-07-provider-listed.png` |
| 110-08 Models table shows Ollama models | | Model count observed: ___ (feed `GET /dashboard/models`: ___) | `110-08-models-after-add.png` |
| 110-09 Overview KPIs moved | | Providers: ___ · Models: ___ | `110-09-overview-after-add.png` |
| 110-10 Cleanup (delete / keep) | | Deleted / kept — reason: ___ | `110-10-cleanup.png` |
| Neg-1 Empty gateway key → inline error, no request | | Error observed (verbatim): ___ | `110-n1-no-gwkey-error.png` |
| Neg-2 No catalog pick → inline error, no request | | Error observed (verbatim): ___ | `110-n2-no-catalog-error.png` |

**Item 110 verdict:** PASS / FAIL / BLOCKED-ON-ENVIRONMENT (circle one)

Notes (incl. any step done differently from the README and why):

## 2. Item 128 — provider-add click-through, restated (Built ❌, Verified ❌)

Same pass as §1. Record here only what differs, if anything.

| Item | Verdict | Evidence |
|---|---|---|
| 128 | PASS / FAIL / BLOCKED-ON-ENVIRONMENT | Same screenshots as §1 (list any additional file): |

Notes:

## 3. Item 297 — dashboard live verification (Built ✅, Verified ⬜)

One row per section. `Render` = section visible in a real browser with no blank
page, no console error, no stuck "Loading…". `Feed match` = content matches the
live feed named (spot-check at least Overview, catalog count, models).

| # | Section | Render (✓/✗) | Feed match (✓/✗) | Observed (empty-state wording / counts / anomalies, verbatim) | Screenshot |
|---|---|---|---|---|---|
| 0 | Startup console | | — | | `297-00-startup-console.png` |
| 1 | Overview | | | KPIs: prov ___ mod ___ req ___ route ___ | `297-01-overview.png` |
| 2 | Providers | | | Catalog count: ___ configured: ___ | `297-02-providers.png` |
| 3 | Models | | | | `297-03-models.png` |
| 4 | Auto-router | | | | `297-04-autorouter.png` |
| 5 | Strategies | | | `code` option present? yes / no | `297-05-strategies.png` |
| 6 | Cooldown | | | | `297-06-cooldown.png` |
| 7 | Coalescing | | | | `297-07-coalescing.png` |
| 8 | Concurrency | | | | `297-08-concurrency.png` |
| 9 | Pools | | | | `297-09-pools.png` |
| 10 | Route guards (no nav btn — how opened: ___) | | | | `297-10-guard.png` |
| 11 | Guardrails (no nav btn — how opened: ___) | | | Toggle states: PII ___ Secrets ___ Injection ___ | `297-11-guardrails.png` |
| 12 | Plugins | | | | `297-12-plugins.png` |
| 13 | MCP & CLI | | | | `297-13-mcp.png` |
| 14 | A2A | | | A2A enabled? yes / no; empty state shown? | `297-14-a2a.png` |
| 15 | API Keys | | | Key count: ___ | `297-15-keys.png` |
| 16 | Request Logs | | | | `297-16-logs.png` |
| 17 | Usage & Cost | | | | `297-17-usage.png` |
| 18 | Benchmark | | | | `297-18-benchmark.png` |
| 19 | Verification | | | Proven: ___ Unproven: ___ | `297-19-verification.png` |
| 20 | Settings | | | (toggles/port are KNOWN ISSUE K3 — record observed, do not PASS on them) | `297-20-settings.png` |
| 21 | Cache | | | | `297-21-cache.png` |
| 22 | Combos | | | | `297-22-combos.png` |
| 23 | Orphan nav (button clicked: ___) | | — | Behaviour: silent no-op / console error (verbatim if error): ___ | `297-23-orphan-nav.png` |

Console errors seen anywhere (verbatim, or "none"):

"Playground" occurrences seen in the rendered dashboard: 0 / ___ (if >0, quote where — item 297 FAIL)

**Item 297 verdict:** PASS / FAIL / PARTIAL (circle one)

Notes (incl. any section that could not be opened and why):

## 4. Known issues seen this run (README §6)

| ID | Seen? | Observed behaviour |
|---|---|---|
| K1 Orphaned nav (activity/resilience/health/webhooks/analytics/batch/responses/audit) | yes / no | |
| K2 Guard/Guardrails have no nav button | yes / no | How opened: |
| K3 Settings fake toggles / Port 8080 | yes / no | |
| K4 "Playground" present | yes / no | Where: |
| K5 Catalog total ≠ 614 | yes / no | Live total: |

Any NEW issue found (not in README §6 — describe + screenshot filename):

## 5. Sign-off

- Run by (agent/session):
- Date/time finished (IST):
- Screenshots copied back to `docs/packages/live-browser/screenshots/` (36 expected — count actually present: ___; missing: ___ with reasons):
- Checklist flip recommended by coordinator (not by this template): 110 ___ · 128 ___ · 297 ___
