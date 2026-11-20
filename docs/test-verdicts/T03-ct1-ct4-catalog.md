# T03 — CT-1 to CT-4 catalog gap fixes (GAP-TRACK §11) — VERIFY-ONLY verdict

- Date: 2026-11-19 ~19:14 IST (HEAD `6831302`, dirty tree; no commits/pushes made by this tester)
- Scope: verify-only. No code or doc edits except this file. No changes to CHECKLIST.md / PARITY.md / TEST-VERDICTS.md.
- Commands run (all with `export PATH=$HOME/go/bin:$PATH`, `TMPDIR=$HOME/.tmp`):
  - `go test -count=1 -run 'TestG19IsRemoved|TestCT2|TestRegistryLoadRemoved|TestCheckRemoved|TestIsRemoved' ./internal/provider/ ./internal/catalog/` → **ok** (provider 0.888s, catalog 0.320s)
  - `go test -count=1 -run 'TestG7|TestCT3|TestModelHasCapability|TestCheckRemoved|TestIsRemoved' ./internal/catalog/ ./internal/autorouter/` → **ok** (catalog 0.411s, autorouter 0.021s)
  - `go test -count=1 -run 'TestFreeTierProviders|TestParseCustomPrefix|TestCustomProviderApplyPrefix|TestCT3' ./internal/catalog/` → **ok** (0.789s)
  - Targeted `-run` only; full suite / `-race` NOT run (VM loaded, per task).

## Verdicts

| Row | Verdict | One line |
|-----|---------|----------|
| CT-1 direct-path tool capability pre-check | **NOT-LANDED** | No per-model capability flags on `provider.Model`; no direct-path pre-check; capability-mismatch 400 still stops the chain. |
| CT-2 removed-provider blocklist wired | **PASS-WITH-NITS** | Enforcement is live in add/load/resolve paths and tests pass; but the literal `CheckRemovedBlocklist()` is still test-only — enforcement goes through `IsRemoved*`/`Matches` on the same register. |
| CT-3 dead providers excluded + Retired field | **PASS** | `Retired` field exists; all 4 dead providers flagged with reasons; `All()` and `FreeTierProviders()` exclude them; feed uses `All()`; tests green. |
| CT-4 no-auth ToSGrey + ToolCalling field | **NOT-LANDED** | All 10 no-auth entries still `ToSGrey: false`; no `ToolCalling` field exists anywhere in the `Provider` struct. |

## CT-1 — NOT-LANDED

What was checked (code read, not just grepped):
- `internal/provider/provider.go:96-126` — `Model` struct has `Vision bool` only (plus prices, `Deprecated`, `Region`, `FixedParams`). No tools/JSON capability flag of any kind.
- `internal/provider/dashboard.go:124` — comment states it outright: "There are no capability flags (tools/JSON/vision) in the registry, so the catalog does not claim any."
- `internal/server/models_detail.go:227` — the `?capability=` filter is refused with 400 because "the catalog records no per-model capability flags (tools/JSON/vision), so no model can be matched by capability".
- Whole-tree grep for `stripUnsupportedParams` / `UnsupportedParams`: zero hits. No param-stripping on any request path.
- Direct-path 400 behaviour unchanged: `internal/router/router.go:6191` ("model-shape 400s stop the chain") and the identical-error early stop at `router.go:4154` / stream equivalent ~`router.go:3692-3712`. No capability-mismatch 400 → failover-to-confirmed-capable-target branch exists.
- What DOES exist (and is not CT-1): operator-declared `CodeCapabilities` / `ModelHasCapability` on the auto path only — `internal/autorouter/autovariants.go:1224-1233`, `internal/config/config.go:720-732`. That is config-declared, not `provider.Model` flags from probe/models.dev, and it does not touch the direct path.
- No CT-1 test file exists (grep `CT-1|CT1` over `internal/*_test.go`: no hits).

## CT-2 — PASS-WITH-NITS

Evidence the fix is in the tree and enforced:
- `internal/provider/removed.go` — runtime enforcement: `IsRemovedVariant()` (per-provider, keyless-only nuance) and `IsRemovedBaseURL()` (host / host+path match, mirrors `catalog.IsRemovedDomain`).
- `internal/provider/registry.go:507` — `addProvider` rejects any base URL matching a removed provider (`RemovedProviderError`).
- `internal/provider/registry.go:679-693` — `AddCatalogWithOptions` refuses removed catalog IDs before lookup (`refuseRemovedCatalogID`, `:788`) and refuses the removed variant post-lookup (`rem.Matches(entry)`).
- `internal/provider/registry.go:839-898` — `resolveCatalogForLoad` refuses removed IDs (`:788` via `refuseRemovedCatalogID`) and removed-variant entries, and re-checks an explicit config base URL against removed domains (`:898`).
- `internal/provider/registry.go:395` — config-file load path also checks `IsRemovedBaseURL` for non-catalog-linked providers (covers the bypass `resolveCatalogForLoad` never sees).
- Register: `internal/catalog/removed.go:113-168`, 5 entries (puter, theoldllm, gemini-business, suno, opencode-keyless-only); `CheckRemovedBlocklist()` seed guard at `:160`.
- Tests green (see commands above): `TestG19IsRemovedVariant`, `TestG19IsRemovedBaseURL`, `TestCT2AddCatalogRefusesRemovedIDs`, `TestCT2AddCustomSimilarHostUnaffected`, `TestRegistryLoadRemovedProviders` (`internal/provider/removed_test.go`, `internal/provider/registry_catalog_test.go:540`).

Nit (does not block the PASS — the acceptance behaviour, zero re-add, is met):
- The task text says "`CheckRemovedBlocklist` wired into addProvider + resolveCatalogForLoad". Literally, `CheckRemovedBlocklist()` still has zero production callers — its only call site is `internal/catalog/removed_test.go:70-71`. The production enforcement reuses the same 5-entry register through `catalog.IsRemoved` / `RemovedProvider.Matches` / `IsRemovedBaseURL` instead. Functionally equivalent, but a future grep for the named function will still report it unwired. Coordinator may either accept this as the implementation or rename the wiring to call `CheckRemovedBlocklist`'s register directly.

## CT-3 — PASS

Evidence:
- Provider-level field exists: `internal/catalog/catalog.go:301-312` — `Retired bool` + `RetiredReason string` (required when `Retired`, enforced in `Validate()` at `:360`) + `RetiredOn string`. Comment cites CT-3.
- All four dead providers flagged: blackbox `internal/catalog/providers_reference.go:2020-2021` (`Retired: true`, 404 reason); galadriel `:2159-2160`; ai21 `:2179-2180`; bytez `:2861-2862`. All keep `BaseURLs: nil`.
- Exclusion: `All()` filters retired (`internal/catalog/catalog.go:455-465`); `FreeTierProviders()` iterates `All()` (`:768-776`), so retired free-tier flags no longer inflate the count; catalog feed uses `All()` (`internal/server/providers_add.go:362`). `RetiredProviders()` / `RetiredByID()` / `IsRetired()` at `catalog.go:467-497` preserve the reason for refusal messages.
- Count pin: `FreeTierCount() == 162` asserted in `internal/catalog/g7_test.go:152` (165 − blackbox/ai21/bytez), test green.
- Tests green: `TestCT3RetiredProvidersExcludedFromAll` etc. in `internal/catalog/ct3_retired_test.go`.

Nit (recorded, not blocking):
- bytez `RetiredReason` (`providers_reference.go:2862`) admits no shutdown was verified ("bytez.com was live when checked 2026-11-19") — it is retired for having no recorded base URL, not for a verified discontinuation. The reason text is honest about that, so this is a data-quality note, not a defect.

## CT-4 — NOT-LANDED

Evidence:
- All 10 no-auth entries still `ToSGrey: false`, verified by parsing `internal/catalog/providers_reference.go`: devin-cli-agentic `:5275`, duckduckgo-web `:5292`, cloudflare-playground `:5309`, veoaifree-web, auggie, zcode, minimax-code, codex-app-server, uncloseai `:5411`, aihorde `:5428`. The five named in GAP-TRACK (duckduckgo-web, cloudflare-playground, devin-cli-agentic, uncloseai, aihorde) are all still `false`; four of them still carry `HasFreeTier: true`.
- No `ToolCalling` field exists: grep `ToolCalling|tool_calling` over `internal/catalog/*.go` → zero hits; `Provider` struct (`internal/catalog/catalog.go:238-330`) has `ToSGrey bool` at `:300` and no tool-calling field (`supported` / `emulated` / `unknown`).
- No CT-4 test file exists (grep `CT-4|CT4|ToolCalling` over `internal/*_test.go`: no hits).
- Mitigation noted in GAP-TRACK still holds (all ten have nil base URLs, un-addable), but the fix itself — flagging + field — is absent.

---

## Fallback slice — PARITY rows 588 / 582 / 583 / 579 stale-text corrections

Rows are `docs/PARITY.md` line numbers (Area 3 table). Verdict here is on **whether each row's current text matches tree reality**, not a status flip (the coordinator owns flips).

| PARITY row (line) | Row text today | Verdict on the text |
|---|---|---|
| 588 Deprecated / retired handling — PARTIAL — "no provider-removal blocklist / takedown process" | **STALE — needs correction beyond the GAP-TRACK proposal** | Blocklist exists AND is now enforced (CT-2 verified PASS above: `registry.go:507,679,788,898`, load `:395`). GAP-TRACK §11 proposed flipping the text to "blocklist exists but unwired (CT-2)" — that proposal is itself now outdated. Correct text: 5-entry blocklist exists and is enforced at add/load/resolve; retired providers excluded from feed + add. Substance now supports FULL (coordinator to confirm). |
| 582 Free-tier marking — PARTIAL — "HasFreeTier ×154; flag stored but surfaced nowhere — zero references in server/dashboard" | **STALE — GAP-TRACK correction confirmed** | `has_free_tier` IS serialized in `GET /api/catalog` (`internal/server/providers_add.go:342` field, `:380` fill, feed from `catalog.All()` at `:362`); count is now 162, pinned by `internal/catalog/g7_test.go:152` (165 − CT-3's 3 retired free-tier entries), not 154. Flip to FULL per GAP-TRACK, CT-3 as the residual. Test `TestFreeTierProviders` green. |
| 583 Free-tier budget / pools / rankings — MISSING — "No pools, no budget aggregation, no rankings; only per-provider flags + notes" | **STALE — GAP-TRACK correction confirmed** | `FreeTierPools()` (`internal/catalog/catalog.go:803`) and `FreeTierRanking()` (`:854`) exist, but have zero production callers (only `internal/catalog/g7_test.go:168,191,198`) and no server/dashboard exposure (grep over `internal/server/` + `web/` for pools/ranking: zero hits); no budgets by design. Correct text is PARTIAL (code exists, unwired), not MISSING. |
| 579 Custom-provider prefix convention — PARTIAL — "No openai-compatible-* prefix convention; generic custom only" | **STALE TEXT, status stays PARTIAL — GAP-TRACK correction confirmed** | `ParseCustomPrefix` / `ApplyPrefix` exist (`internal/catalog/custom.go:110-168`, all 3 prefixes) and are unit-tested (`internal/catalog/prefix_test.go`, green), but their only caller is the test file — `ToProvider()` (`custom.go:218-225`) never applies a prefix and defaults wire to `WireOpenAI`. Behaviour still missing, so PARTIAL stands, with corrected text (convention code exists, unwired). |

## Failures / nits summary

1. CT-1 NOT-LANDED — no capability flags, no direct-path pre-check, no 400 failover (evidence above).
2. CT-4 NOT-LANDED — no ToSGrey flags on no-auth entries, no ToolCalling field (evidence above).
3. CT-2 nit — `CheckRemovedBlocklist()` itself still test-only; enforcement via sibling helpers on the same register.
4. CT-3 nit — bytez retired without a verified shutdown (reason text is honest about it).
5. PARITY 588 — GAP-TRACK's proposed correction ("exists but unwired") is overtaken by the CT-2 landing; needs a fresh correction, not the proposed one.
