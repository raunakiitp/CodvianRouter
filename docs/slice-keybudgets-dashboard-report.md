# Slice report — items 139–140: per-key rolling 5-hour USD budgets dashboard card

## Row 140 spec (quoted from CHECKLIST.md)

> Per-key rolling 5-hour USD budgets (dashboard, same slice): "Per-key rolling budgets · 5 h" card in Usage & Cost, fed by live `GET /dashboard/key-budgets` (Tier 3 MANAGEMENT, in the guard table + OpenAPI; MANAGEMENT 36→38) — per-key table (key, limit + policy, spent in window, remaining with at-limit pill, window end + next roll-off, honest "unknown — N events" with unpriced models listed) + set/delete form (`POST /dashboard/key-budgets`: ledger first, then the config file's `key_budgets` block, ledger rolled back on a failed save; 404 unknown key, 400 bad policy/negative limit) — refreshed with the Usage 5 s cycle + Refresh button; honest empty states (no keys, no budget = "No limit" with window spend still shown). `node --check` on the dashboard JS clean; "Playground" absent. Live smoke 2026-11-18 (real binary + two Python mocks, 22/22 checks in scripts/smoke-keybudgets.py): $0.05 cap seeded from the config file → 2 priced calls = $0.05 spent → 3rd call 402 with 0 provider calls; fallback (dead 429 → live 200) cost exactly $0.025; count_as_zero routes an unpriced model, fail_closed 400s naming it with 0 provider calls; unknown spend shown as 1 event / mystery-model; restart keeps budget + spent and still 402s. One ordering note: the balanced strategy reorders by live signals, so the dead-first fallback only holds before the live provider has success history — the quota property itself is order-independent and is covered by the unit test.

## What was already present (audit, line by line)

Audited against row 140; everything below was already implemented and correct — no rebuild was needed.

**Card + table** — `web/index.html:1671`: "Per-key rolling budgets · 5 h" card in Usage & Cost, with columns Key / Limit / Spent · 5 h / Remaining / Window / Unknown spend. `applyKeyBudgets()` (`web/index.html:3249`): key name + key ID; limit + policy or "No limit"; priced spent; remaining with `<span class="pill down">at limit</span>`; "last 5 h · ends <window_end> · next roll-off <next_rolloff>"; honest `unknown — N events (M tokens) · unpriced: <models>` or "none". Unpriced spend is never shown as $0.

**Feed** — `GET /dashboard/key-budgets` in `internal/server/keybudgets.go:196` (`handleKeyBudgets`): every config gateway key plus any ledger-only budget row, sorted, with live `KeyBudgetStatus` (ledger JSON contract `has_budget`, `limit_usd`, `unpriced_policy`, `spent_usd`, `remaining_usd`, `at_limit`, `window_start`, `window_end`, `next_rolloff`, `unknown_events`, `unknown_tokens`, `unpriced_models` — all present and matching what the JS reads). Keys with no budget still report window spend. Empty list when no ledger is wired.

**Form** — `web/index.html:1679` (key select, limit input, policy select, Set/Delete buttons) + `saveKeyBudget()` (`web/index.html:3306`): client-side negative/NaN limit guard, confirm dialog on delete, POSTs JSON, reloads the feed on success and on failure.

**POST handler** — `handleKeyBudgetUpdate` (`internal/server/keybudgets.go:252`): ledger first, then the config file's `key_budgets` block via `configPersist`; on a failed save the ledger change is rolled back (previous row restored, or the new row deleted); 404 for an unknown key, 400 for a negative limit or an unknown policy ("" = keep/default fail_closed, per `ledger.NormalizeUnpricedPolicy`). Startup applies the config `key_budgets` block into the ledger (`cmd/codvian-router/main.go:625-637`); config validation rejects negative limits, unknown policies, and budgets naming non-existent keys (`internal/config/config.go:645-665`); `Clone()` deep-copies the block.

**Refresh** — `loadUsage()` calls `loadKeyBudgets()` (`web/index.html:3233`); the Usage view refreshes on the shared 5 s interval (`web/index.html:4689-4702`, `setInterval(..., 5000)`), and the card has its own Refresh button (`keyBudgetRefreshBtn`, `web/index.html:3304-3305`).

**Guard + OpenAPI** — both routes are Tier 3 MANAGEMENT in the guard table (`internal/server/guard.go:103-104`), registered in `internal/server/server.go:804-805`. OpenAPI is generated live from the guard table (`internal/server/openapi.go:89`, `for _, route := range RouteTable()`), so the entries appear in `/openapi.json` automatically — verified live (see below). (The "MANAGEMENT 36→38" count in row 140 is stale: the guard table now has 39 TierManagement entries because other slices added routes since; the two key-budgets entries are present.)

**Empty states** — no keys: `keyBudgetEmpty` ("No router keys yet — issue a key first…") shows when the feed returns zero rows; no budget: "No limit" with window spend still shown. No sample/fake data anywhere in the card.

**"Playground" absent** — `grep -i playground web/index.html` returns nothing.

## What was missing — one real bug, found by live verification

**Config-file persistence silently corrupted the config file and lost budget changes on restart.**

`internal/config/config.go` preserves unknown top-level fields across Load→Save via `Extensions`, keyed off `knownConfigFields` — but `key_budgets` was the **only** `Config` struct field missing from that list (verified by diffing all 17 struct JSON tags against the map). Consequence, reproduced end to end with the pre-fix binary:

1. Config file seeded with `key_budgets: {gk_…: {limit_usd: 5, fail_closed}}`.
2. `POST /dashboard/key-budgets` `{limit_usd: 2.5, count_as_zero}` → HTTP 200, `persisted_to_config: true`; GET confirmed the ledger held 2.5/count_as_zero.
3. But the saved config file contained **two** `key_budgets` blocks — the new one from the struct field plus the stale one spliced back in from `Extensions`.
4. After a restart, the JSON decoder took the **last** duplicate (the stale $5/fail_closed), and startup re-applied that stale value into the ledger — GET showed `limit_usd: 5, fail_closed`. The operator's change was silently lost, contradicting the handler's own "runtime and file must never disagree" invariant.

The row-140 live smoke (22/22) did not catch this: it seeded the cap from the config file and never POSTed a changed budget before restarting, so no re-save ever duplicated the block.

## What I changed (exactly one line)

**File: `internal/config/config.go`, line 319** — added `"key_budgets": true` to `knownConfigFields`:

```go
	"mcp":               true,
	"pools":             true,
	"key_budgets":       true,
}
```

No other file was changed. No config test references `knownConfigFields` or `key_budgets`, so nothing else needed updating. (The worktree has no `.git` directory, so no diff is attached; the change is the single line above.)

## Verification performed

- `go build ./...` — PASS (Go 1.27.4; `go` is at `~/go/bin`, not on the default PATH). Note the harmless `GOPATH == GOROOT` warning from that toolchain layout.
- `node --check` on the single extracted inline `<script>` block from `web/index.html` (175,119 chars) — clean.
- Live binary (built to `/tmp`, isolated HOME, port 20128):
  - `GET /dashboard` → HTTP 200, card present (2 matches for "Per-key rolling budgets").
  - `GET /dashboard/key-budgets` → one row: `has_budget: true, limit_usd: 5, unpriced_policy: fail_closed, spent_usd: 0, remaining_usd: 5, at_limit: false`, window start/end 5 h apart, `next_rolloff: null`, `unknown_events: 0`, `rolling_window_hours: 5`.
  - `POST` set $2.50 / count_as_zero → 200, `persisted_to_config: true`; config file now has **exactly one** `key_budgets` block with the new values.
  - `POST` unknown key → 404 `unknown gateway key gk_nope`; negative limit → 400 `limit_usd must not be negative`; bad policy → 400 `unknown unpriced_policy bogus (want fail_closed or count_as_zero)`.
  - **Restart after fix** → GET still shows `limit_usd: 2.5, count_as_zero` (previously reverted to $5/fail_closed — bug gone).
  - `POST` delete → 200, `persisted_to_config: true`; GET shows `has_budget: false` with spend still reported; config file's `key_budgets` block removed entirely.
  - `GET /openapi.json` → lists `/dashboard/key-budgets`.
- Test servers and temp files under `/tmp` were removed after the checks; nothing was pushed to GitHub and Codvian was not touched.

## Pending notes for the coordinator

- **`go test -race` on touched packages is still PENDING** (per the task instruction and the item-139 backend gate note): I did not run `go test` at all and did not run `-race`. The final gate runs it.
- The fix touches `internal/config/config.go`, shared config code — but it is a one-line, behaviour-correcting addition to a set that was missing exactly one field; the final gate's config + server tests cover it.
- The pre-existing 22/22 live smoke (`scripts/smoke-keybudgets.py`) was not re-run (out of scope for this slice's allowed checks); it does not cover the POST-then-restart path that this bug lived on. A future smoke run should add: POST a changed budget, restart, GET must show the changed budget.

## Recommended CHECKLIST verdict

- **Item 139 (backend):** no change — already ✅/✅; the fix is in shared config code, not the item-139 backend.
- **Item 140 (dashboard):** ✅ Built, ✅ Verified — the card, feed, form, refresh, empty states, guard/OpenAPI entries and error paths were all already present and are now live-verified; the one genuine defect (config-file duplication losing POSTed budgets on restart) is fixed with a one-line change and re-verified end to end including restart persistence. The only outstanding item is the final-gate `-race` run, which is pending by instruction, not by defect.
