# T04 — CT-5 to CT-8 catalog gap fixes (GAP-TRACK §11) — VERIFY-ONLY verdict

- **Slice:** T04 CT-5 / CT-6 / CT-7 / CT-8
- **Date (IST):** 2026-11-19 ~19:20 IST
- **Tree:** HEAD `6831302`, dirty working tree (uncommitted changes present; see notes per row)
- **Tester role:** verify-only — no code, doc, commit, or push made by this tester (this verdict file is the only write)

## Verdicts

| Row | Verdict | One line |
|-----|---------|----------|
| CT-5 | **NOT-LANDED** | No shrink/0-provider guard anywhere in `Sync` or `fetch`; a 0-provider fetch still records as a successful sync. |
| CT-6 | **PASS** | Field-vs-prefix mismatch rejected + logged-and-skipped; duplicate pairs first-wins with warning; 4/4 CT-6 tests green. |
| CT-7 | **NOT-LANDED** | No `models_fetched_at` stamp anywhere; no scheduled background refresh; age not surfaced in GET /v1/models or dashboard. |
| CT-8 | **NOT-LANDED** | README headline still quotes "614" unqualified; dashboard catalog count shows total only. Routable count quoted nowhere alongside the total. |

## CT-5 — modelsync shrink / 0-provider guard — NOT-LANDED

**Required (GAP-TRACK §11, entry 515):** `Syncer.Sync` (`internal/modelsync/modelsync.go`) must reject a fetched catalog shrinking providers/models beyond a threshold vs previous state (keep last good data, record as failed sync); fail outright on a 0-provider fetch.

**What was verified (code read):**
- `Sync` (`internal/modelsync/modelsync.go:872-1018`): after `s.fetch(ctx)` (line ~904) the code goes straight to `Plan(cat, ...)` — there is no comparison of fetched provider/model counts against the snapshotted `prev` entries (snapshot taken at lines 884-889), no shrink threshold constant, no `len(cat.Providers) == 0` check. A 0-provider fetch plans nothing, `apply(plan)` succeeds on an empty plan, and the fold at lines 996-1013 then sets `s.lastSuccess = &now` and clears `s.lastError` (lines 1014-1015). The exact failure mode GAP-TRACK describes is still present.
- `fetch` (`internal/modelsync/modelsync.go:822-867`): validates HTTP status, size cap (`MaxCatalogBytes`), and JSON parse only — no empty-catalog rejection.
- `grep -rn "shrink|Shrink|threshold|Threshold" internal/modelsync/` returns nothing relevant (only unrelated hits elsewhere, e.g. `internal/router/concurrency_test.go`). No CT-5 test file exists in `internal/modelsync/`.

**Tests run:** none applicable — no CT-5 code or test exists to run. (Targeted modelsync runs below were for CT-6.)

**Evidence:**
- `internal/modelsync/modelsync.go:872` (`func (s *Syncer) Sync`), `:884-889` (prev snapshot, unused for any size check), `:904` fetch → `:918` `plan := Plan(...)` with no guard in between, `:1014-1015` (`lastSuccess` set / `lastError` cleared unconditionally on apply success).
- `internal/modelsync/modelsync.go:822-867` (`fetch` — no empty check).

## CT-6 — LiteLLM parse cross-check — PASS

**Required (GAP-TRACK §11, entry 516):** reject/log-and-skip entries whose `litellm_provider` field disagrees with the key prefix; warn on duplicate (provider, model) pairs.

**What was verified (code read + tests run):**
- `parseLiteLLMCatalog` (`internal/modelsync/modelsync.go:258-360`): doc comment at `:232-257` states the CT-6 rules. Mismatch branch at `:283-293` — when the key carries a provider prefix and the field is non-empty and differs, the entry is skipped with `log.Warn("modelsync: LiteLLM entry provider mismatch, skipping entry", ...)` and never lands in the catalog. Bare key (no prefix) still trusts the field alone (`:275-281`); empty field still falls back to the key prefix. Duplicate branch at `:347-354` — keys sorted first (`:271`), first-wins, later duplicates skipped with `log.Warn("modelsync: duplicate LiteLLM (provider, model) pair, keeping first entry", ...)`.
- Note: the fix is stricter than the GAP-TRACK wording in one documented respect — mismatch is rejected outright, not "rejected/logged-and-skipped" as alternatives; rejection + warning log is the implemented behaviour and matches the "fail-closed" intent.

**Tests run (all green):**
- `export PATH=$HOME/go/bin:$PATH; export TMPDIR=$HOME/.tmp; go test -count=1 -v -run 'TestCT6' ./internal/modelsync/` → all 4 PASS:
  - `TestCT6LiteLLMMismatchRejected` — PASS
  - `TestCT6LiteLLMMismatchRejectedViaExportedParse` — PASS
  - `TestCT6LiteLLMFieldAgreesWithPrefixParses` — PASS
  - `TestCT6LiteLLMDuplicateFirstWinsWarns` — PASS
  - `ok github.com/raunakiitp/codvian-router/internal/modelsync 0.043s`
- `go test -count=1 -v -run 'TestParseLiteLLM|TestLiteLLM' ./internal/modelsync/` → `TestParseLiteLLMCatalog` PASS, `ok ... 0.179s`.
- `go test -count=1 -run 'TestRefresh|TestMergeModelLists' ./internal/provider/` → `ok ... 0.122s` (sanity: provider package still green for the refresh surface).
- Test pins: `internal/modelsync/litellm_ct6_test.go:22,58,74,105` (4 test funcs).

**Transient incident (not a verdict factor):** the first `go test -run 'TestCT6'` invocation failed with `[build failed]` on `internal/modelsync`, while an identical-package run minutes earlier had passed and `go vet ./internal/modelsync/` immediately after returned EXIT 0. Cause: a sibling agent was mid-edit on the shared dirty tree at that moment (tree is dirty across many packages). A retry seconds later passed 4/4. Recorded here as sibling churn, not a code defect.

**Evidence:**
- `internal/modelsync/modelsync.go:232-257` (CT-6 doc comment), `:283-293` (mismatch reject + warn), `:347-354` (duplicate first-wins + warn), `:271` (sorted keys).
- `internal/modelsync/litellm_ct6_test.go:22,58,74,105`.

**Nits:** none. The LiteLLM Syncer is still not constructed anywhere (GAP-TRACK notes only models.dev is wired in `cmd/codvian-router/main.go:1930-1937`); CT-6's fix is explicitly "before Wave 3 constructs the LiteLLM Syncer", so that is out of scope for this row, not a nit.

## CT-7 — models_fetched_at + scheduled refresh + staleness badge — NOT-LANDED

**Required (GAP-TRACK §11, entry 517):** stamp `models_fetched_at` at add/refresh, surface the age in GET /v1/models + dashboard, run a scheduled background refresh with a staleness badge.

**What was verified (code read):**
- `provider.Model` (`internal/provider/provider.go:96-116`): no timestamp field of any kind — no `ModelsFetchedAt`, `FetchedAt`, or `models_fetched_at` JSON tag.
- `grep -rn "ModelsFetchedAt|models_fetched_at|FetchedAt|fetched_at" internal/provider/ internal/server/ cmd/ --include="*.go"` → zero hits. No `AgeSeconds` / `age_seconds` / staleness-badge field anywhere in `internal/provider/`, `internal/server/`, or `cmd/` (searched `models_fetched_at|ModelsFetchedAt|modelsAge|model_age|age_seconds|AgeSeconds` across `*.go`/`*.html`/`*.js`/`*.md` — no hits outside GAP-TRACK).
- `RefreshModels` (`internal/provider/refresh.go:75-127`) is unchanged in shape: manual re-probe only, no timestamp stamping on success (the apply at `:117-131` replaces `p.Models` via `mergeModelLists` with no `FetchedAt` assignment — and there is no field to assign to). `mergeModelLists` (`:176-197`) carries stored records through unchanged; new models arrive as bare `Model{ID: id}`.
- Only caller of `RefreshModels` is the manual handler `POST /dashboard/models/refresh` (`internal/server/models.go:254`); `grep -rn "RefreshModels" --include="*.go" .` (excluding tests) returns only `internal/provider/refresh.go`, `internal/server/models.go:213,254`, `internal/server/server.go:466` (interface wiring). No ticker / scheduled background refresh exists.
- The only timestamp in the whole surface is the manual-refresh response's own `RefreshedAt` (`internal/server/models.go:229` field, `:271` set to `time.Now().UTC()`) — a per-call stamp, not a per-provider/per-model fetched-at, and it is not persisted or surfaced by GET /v1/models.

**Tests run:** `go test -count=1 -run 'TestRefresh|TestMergeModelLists' ./internal/provider/` → `ok ... 0.122s` (existing refresh behaviour still green; no CT-7 test exists to run).

**Evidence:**
- `internal/provider/provider.go:96-116` (Model struct — no timestamp).
- `internal/provider/refresh.go:75-127` (manual-only refresh, no stamping), `:117-131` (apply), `:176-197` (merge, no timestamp).
- `internal/server/models.go:229,271` (only timestamp is the per-call `RefreshedAt`); `:254` (sole `RefreshModels` caller — manual POST handler).
- Absence: no `models_fetched_at` / `ModelsFetchedAt` / scheduled refresh / staleness badge anywhere in `internal/provider/`, `internal/server/`, `cmd/`, `web/index.html`.

## CT-8 — headline quotes routable count alongside total — NOT-LANDED

**Required (GAP-TRACK §11, entry 518):** quote the routable count alongside the total everywhere the headline appears ("614 in the catalog, 371 routable as listed") — `README.md` / `README.v2.md` + dashboard.

**What was verified (code + text read):**
- `README.md:56`: `## Providers — 614 in the catalog`; `README.md:58`: "The catalog lists **614 providers** (...), each with its base URL, wire format, and auth scheme. Add one by catalog ID with just your key — no base URL to look up." No routable figure appears anywhere near the headline; `grep -n "371 routable|routable as listed" README.md README.v2.md` → no hits. Same in `README.v2.md:134,136` (identical headline text).
- Dashboard (`web/index.html:4506-4524`, `renderCatalogList`): the catalog count element is set to `fmtNum(catalogEntries.length) + " providers · live from GET /api/catalog"` — total only, no routable count. The `has_base_url` flag exists per entry in the `GET /api/catalog` feed (per GAP-TRACK), but the headline count does not use it.
- The measurement itself exists and is current: `MeasureBaseURLCoverage()` / `BaseURLCoverageOf` (`internal/catalog/catalog.go:739-762`), `Routable()` (`:695`); pinned by `TestBaseURLCoverage` (`internal/catalog/g7_test.go:71-81`) expecting with-base-URL 379 / routable **370** / template-only 9. Note the GAP-TRACK "371" figure is stale — CT-3 retired ai21, which held a triage base URL, so the honest current figure is **370 routable of 614** (and 379 with a base URL). The fix, when landed, must quote 370, not 371. This is a data note for the builder, not a verdict on CT-8 itself — the row is NOT-LANDED because no routable count is quoted at all.

**Tests run:** none applicable — this row is copy + dashboard text; no test pins the headline wording. (The coverage measurement is pinned by `TestBaseURLCoverage`, but that test was not re-run here — it belongs to the catalog package, outside this slice's targeted `go test` scope; its expectation text was read at `internal/catalog/g7_test.go:71-81`.)

**Evidence:**
- `README.md:56,58`; `README.v2.md:134,136` (614-only headline).
- `web/index.html:4521` (`count.textContent = fmtNum(catalogEntries.length) + " providers · live from GET /api/catalog"` — total only).
- `internal/catalog/catalog.go:695` (`Routable()`), `:739-762` (coverage measurement exists but is not quoted in the headline).
- `internal/catalog/g7_test.go:78` (current honest figure: routable 370, not 371).

## Commands run (summary)

```sh
export PATH=$HOME/go/bin:$PATH; export TMPDIR=$HOME/.tmp
go test -count=1 -v -run 'TestCT6' ./internal/modelsync/
# → 4/4 PASS, ok github.com/raunakiitp/codvian-router/internal/modelsync 0.043s
#   (first attempt hit a transient [build failed] from sibling mid-edit churn;
#    go vet passed immediately after; retry passed 4/4 — see CT-6 section)
go test -count=1 -v -run 'TestParseLiteLLM|TestLiteLLM' ./internal/modelsync/
# → TestParseLiteLLMCatalog PASS, ok ... 0.179s
go test -count=1 -run 'TestRefresh|TestMergeModelLists' ./internal/provider/
# → ok github.com/raunakiitp/codvian-router/internal/provider 0.122s
```

No full-suite or `-race` run (per task instruction; VM loaded). No commits, no pushes, no edits outside this file.

## Fallback — PARITY row 589 FULL-flip evidence — PASS-WITH-NITS

**Row:** `internal parity tracker:589` — "Verified-overlay data preservation": "Overlay replaces whole entry, dropping HasFreeTier for 11 providers (cerebras, cloudflare-ai, deepseek, fireworks, gemini, groq, mistral, nvidia, ollama-cloud, openrouter, vertex)" — currently marked `WRONG-BEHAVIOUR | internal/catalog/verified.go | UNVERIFIED` in the file. The evidence below supports flipping it to FULL; the flip itself is the coordinator's to apply (this tester does not edit parity tracker).

**What was verified (code read):**
- `mergeVerified` exists at `internal/catalog/catalog.go:579-624` (task cited `:507-556`; line numbers shifted by sibling edits — the call site is `:507-527` in `allIncludingRetired`, the function itself is `:559-624`). Doc comment at `:559-578` states the OR-merge rule explicitly.
- OR-merges: `merged.HasFreeTier = ref.HasFreeTier || ver.HasFreeTier` (`:621`) and `merged.ToSGrey = ref.ToSGrey || ver.ToSGrey` (`:622`). Verified facts win only where stated (Name, Website, BaseURLs, etc.); reference-only facts (Alias, static Models list, FreeTierNote when verified is silent) survive.
- Call site `allIncludingRetired` (`:519-528`) uses `out[i] = mergeVerified(out[i], p)` instead of the old whole-entry replace, with a comment (`:520-526`) naming the exact 11-provider defect.
- The function returns a fresh copy and mutates neither input (`:580`).

**Test pin exists and passes:**
- `TestVerifiedOverlayPreservesReferenceFreeTier` (`internal/catalog/g7_test.go:20-60`): checks all 11 providers keep `HasFreeTier` after the overlay (cloudflare-ai under post-move ID `cloudflare-workers-ai`; novita-ai as reference-only control), checks `FreeTierNote` survives verbatim per provider, and checks verified facts still win (groq base URL `https://api.groq.com/openai/v1`).
- Command: `export PATH=$HOME/go/bin:$PATH; export TMPDIR=$HOME/.tmp; go test -count=1 -v -run 'TestVerifiedOverlayPreservesReferenceFreeTier|TestBaseURLCoverage' ./internal/catalog/` → both PASS (`TestVerifiedOverlayPreservesReferenceFreeTier` 0.11s, `TestBaseURLCoverage` 0.00s), `ok github.com/raunakiitp/codvian-router/internal/catalog 0.203s`.

**Nit (why PASS-WITH-NITS, not clean PASS):** the `ToSGrey` OR-merge (`catalog.go:622`) is present in code but has no dedicated merge test — `grep -rn "ToSGrey" internal/catalog/*_test.go` finds only `TestToSGreyProvidersAreFlagged` (`catalog_test.go:142`), which checks web-cookie providers are flagged, not that a reference-only `ToSGrey` survives a verified overlay. The row's stated defect is the `HasFreeTier` drop, which is fully pinned, so this does not block the FULL flip — but a one-case overlay test for `ToSGrey` would close the gap.

**Evidence:**
- `internal/catalog/catalog.go:559-578` (merge rule doc), `:579-624` (`mergeVerified`), `:621-622` (OR-merges), `:507-528` (call site).
- `internal/catalog/g7_test.go:20` (test pin), `:13-19` (row reference comment).
- `internal parity tracker:589` (row text, still WRONG-BEHAVIOUR/UNVERIFIED in file — flip pending coordinator).
