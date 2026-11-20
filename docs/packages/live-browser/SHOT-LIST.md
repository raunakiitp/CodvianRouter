# Screenshot shot list — live-browser run (items 110, 128, 297)

Status: **HELD.** Pairing declined for now (Raunak, 2026-11-19 ~15:40 IST) — see
`README.md`. This list is the capture plan for the future agent-run over SSH
(headless Chromium / Playwright on his PC, after he approves pairing). Nothing
here has been captured yet; `screenshots/` is empty on purpose.

## How to shoot

- **Viewport:** 1440×900 minimum. Record light/dark theme in the results template.
- **Framing:** every shot shows the full viewport — nav sidebar + section heading +
  section content. A cropped fragment that hides the nav does not count.
- **Timing:** wait for network idle after each navigation/click before shooting;
  never shoot a "Loading…" spinner and call it the section.
- **Save location:** `docs/packages/live-browser/screenshots/` in the worktree
  (copied back over `scp` after the run). **Filename convention:**
  `<item>-<step>-<slug>.png`, exactly as listed. Do not rename, do not overwrite
  a failed attempt's file — if a step is retaken, save as
  `<filename>-retry2.png` and note it.
- **Keys:** if a shot would capture a typed gateway/provider key, clear the field
  first and retake. Never commit a screenshot containing a key.

## A. Run setup (1 shot)

| Filename | When | Must be visible |
|---|---|---|
| `297-00-startup-console.png` | After `.\codvian-router.exe serve` starts, before opening the browser | PowerShell/console showing the startup lines: `listening on 127.0.0.1:20128`, `dashboard: http://127.0.0.1:20128/dashboard`, `bootstrap: complete` or `bootstrap: PENDING`, version `0.1.0` |

## B. Item 297 — section render sweep (23 shots)

One shot per section in nav order (§4A of the README). Each shot is taken after
clicking that section's nav button and waiting for `.view.on` on its section.

| Filename | Section (`data-view` / `#view-…`) | Must be visible |
|---|---|---|
| `297-01-overview.png` | `overview` / `#view-overview` | 4 KPI cards only in the KPI row; Providers `0 / 0` "None yet"; Models `0` "None yet"; Requests `0`; quick-start card; chart empty state "No requests yet — …" if zero traffic |
| `297-02-providers.png` | `providers` / `#view-providers` | `#addProviderCard`, `#freeTierCard`, `#catalogCard`; `#catalogCount` live total; 0 configured; `#provVertexLine` |
| `297-03-models.png` | `models` / `#view-models` | Empty table + `#modelEmpty` "No models yet — connect a provider first." with "Add a provider" button |
| `297-04-autorouter.png` | `autorouter` / `#view-autorouter` | Live classification card; auto/* models labelled virtual |
| `297-05-strategies.png` | `strategies` / `#view-strategies` | Strategy picker options; **no `code` option** visible |
| `297-06-cooldown.png` | `cooldown` / `#view-cooldown` | Global + per-chain modes, live values |
| `297-07-coalescing.png` | `coalescing` / `#view-coalescing` | Live single-flight counters |
| `297-08-concurrency.png` | `concurrency` / `#view-concurrency` | Live limiter state |
| `297-09-pools.png` | `pools` / `#view-pools` | Pool state; empty at 0 providers |
| `297-10-guard.png` | `guard` / `#view-guard` | Tier 1/2/3 tables. No nav button exists — record how it was opened (see README §6 K2) |
| `297-11-guardrails.png` | `guardrails` / `#view-guardrails` | Toggles table with `#grTogglePII`, `#grToggleSecrets`, `#grToggleInjection`. No nav button exists — record how it was opened |
| `297-12-plugins.png` | `plugins` / `#view-plugins` | Registry snapshot; honest zero state |
| `297-13-mcp.png` | `mcp` / `#view-mcp` | Live tool list + counts |
| `297-14-a2a.png` | `a2a` / `#view-a2a` | A2A status; "A2A is disabled" empty state if disabled |
| `297-15-keys.png` | `keys` / `#view-keys` | `#routerKeyCount`, key rows, `#newKeyBtn` |
| `297-16-logs.png` | `logs` / `#view-logs` | Request-logs table with all columns (incl. Reasoning/Cached/Saved); empty at zero traffic |
| `297-17-usage.png` | `usage` / `#view-usage` | Spend/tokens/failovers; zeros at idle |
| `297-18-benchmark.png` | `benchmark` / `#view-benchmark` | Latest run + history; honest missed-target verdicts if present |
| `297-19-verification.png` | `verification` / `#view-verification` | `#verifProven`, `#verifUnproven`, `#verifRows` table |
| `297-20-settings.png` | `settings` / `#view-settings` | Strategy picker `#strategySel`; toggles + Listen/Port rows (see README §6 K3 — do not judge these as working) |
| `297-21-cache.png` | `cache` / `#view-cache` | Per-layer stats + savings; zeros at idle |
| `297-22-combos.png` | `combos` / `#view-combos` | "No combos yet — create one" empty state at 0 combos |
| `297-23-orphan-nav.png` | (no section) | After clicking an orphaned nav button (e.g. `activity`): the previously visible section is unchanged and no JS error appeared. One shot is enough — note in results which button was clicked; repeat per button only if one behaves differently |

## C. Items 110 + 128 — provider-add click-through (10 shots + 2 negative)

Taken during §4B of the README, in order. Steps 110-01–110-10 map 1:1 to these
filenames. If the Ollama precondition fails (Ollama not running), stop at the
step that fails, shoot what the page actually shows, and record the run as
blocked-on-environment — do not substitute a different provider silently.

| Filename | Step | Must be visible |
|---|---|---|
| `110-01-dashboard-empty.png` | 110-01 | Providers section, 0 configured, empty-state KPIs/catalog |
| `110-02-catalog-list.png` | 110-02 | Full catalog list; `#catalogCount` total; Add buttons; any ToS-grey row with disabled Add |
| `110-03-catalog-search-ollama.png` | 110-03 | `#catalogSearch` containing `ollama`; list filtered to Ollama rows |
| `110-04-form-prefilled.png` | 110-04 | `#addProviderCard` with `#addProvCatalog` = Ollama; hint text under the picker |
| `110-05-gwkey-entered.png` | 110-05 | Gateway-key field filled (masked `••••`); **if the driver screenshot would expose the key, shoot with the field unfocused / masked — never a readable key** |
| `110-06-added-toast.png` | 110-06 | Success toast `Added Ollama — N models, <wire> wire`; button back to "Add provider"; both key fields cleared |
| `110-07-provider-listed.png` | 110-07 | Ollama pinned on top in `#provGrid` with live status; `#catalogConfiguredCount` = `1 configured` |
| `110-08-models-after-add.png` | 110-08 | Models table rows for Ollama's models; `#modelCount` matches |
| `110-09-overview-after-add.png` | 110-09 | Overview KPIs moved: Providers `1 / 1`, Models live = Ollama model count |
| `110-10-cleanup.png` | 110-10 | Final state: 0 providers again (deleted) with KPIs back to empty — or Ollama still listed, if kept at Raunak's request (note which) |
| `110-n1-no-gwkey-error.png` | Negative 1 | Inline `#addProvError` "Paste your gateway key — adding a provider requires gateway-key auth."; no toast; nothing added |
| `110-n2-no-catalog-error.png` | Negative 2 | Inline `#addProvError` "Pick a catalog provider, or switch to Custom."; no toast; nothing added |

## Count check

- Setup: 1 · Section sweep: 23 (22 sections + 1 orphan-nav) · Click-through: 12
  (10 + 2 negative) · **Total: 36 PNGs.** The results template must list all 36
  (or mark the missing ones with the reason). A run that claims PASS with fewer
  files is incomplete.
