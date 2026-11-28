# Naming gate — canonical enforcement and sanctioned exceptions

This document is the authoritative record for the repository's naming gate:
how it is enforced, what it bans, and the narrow, disclosed exceptions.

It deliberately never writes the banned terms. Each class below is identified
by *shape* (the regex class the gate uses), not by the literal string — so this
documented policy cannot itself trip the gate it describes.

## 1. What is banned

Any tracked file (code, comments, docs, README, workflows) must not contain any
of these classes. The gate's pattern shapes:

| Class | Gate pattern shape |
|---|---|
| baseline product (prose) | `reference` + separator + `router` — any casing, `-`/`_`/space separator |
| baseline judge component | `System` + `One`, **contiguous OR space-separated, capitalized only** (see §3) |
| baseline source-tree prefix | `open` + `-`/`_` + `sse` |
| baseline glyph asset | `omni` + optional space + `glyph` — any casing |
| baseline skill pack | `omni` + `-`/`_`/space + `skills` — any casing |
| baseline env-var prefix | `OMNI` + `_` |
| baseline product (identifier) | `omni` + `-`/`_`/space + `route` — any casing |
| retired service | `cheaper` + `-`/`_`/space + `inference` — any casing |

The literal names live only inside the two gate files' pattern tables, where
they are assembled from character fragments at runtime, so neither the gate
nor this document contains them as contiguous strings.

## 2. Where it is enforced (canonical gate)

The canonical gate is executable and runs in continuous integration:

* `scripts/ci.sh` runs `git ls-files | python3 scripts/check-naming.py` as a
  named step (`scripts/check-naming.py (whole-tree naming gate)`). CI fails
  when any tracked file matches a banned class.
* `internal/catalog/repo_brand_gate_test.go` mirrors the same rule as a Go
  test (`TestRepoBrandGateSelfTest`, `TestRepoWideSpacedBrandZero`), so a
  plain `go test ./...` also fails on a stain. It additionally proves the
  pattern table itself: one seeded string per class must match, and
  sanctioned lowercase prose must not.
* `scripts/check-naming.sh` is the older shell helper with a narrower class
  set. It is retained for local use and is **not** the gate of record.

Both the Python script and the Go test build their patterns from character
fragments, so the banned literals do not exist as contiguous strings in the
gate source. Those two files are therefore the only files excluded from the
scan — a structural exclusion: they must carry the pattern tables.

## 3. Sanctioned exception: the lowercase retired route path

The retired, fabricated endpoint whose path is `/v1/` + lowercase
`systemone` was removed, and its removal is recorded in `CHANGELOG.md`,
guarded in `internal/server/guard_test.go`, asserted absent in
`internal/server/backlog_endpoints_test.go`, and probed in
`scripts/audit-fake.sh`. Those references are **lowercase**.

They are allowed, deliberately:

* The banned class is the **capitalized proper noun** naming the baseline's
  judge component. The gate matches that class in capitalized form only,
  because the baseline brand is a proper noun. A lowercase URL path is a
  different token: it names a route this project no longer serves.
* The references are **historical and negative** — they prove the route is
  gone. Deleting them would delete the evidence that the fabrication was
  removed, which is worse than citing the old path.
* Renaming the path would rewrite history in the audit script and the
  changelog for cosmetic reasons; the route must keep no live implementation
  either way (proven by the guard and backlog tests).

If a future change adds a **capitalized** reference to that judge component
for any reason, the gate fails — the exception is scoped to the lowercase
retired path only.

## 4. Local-only files are not evidence

A gate script that exists only on a developer machine proves nothing. Any
claim that the gate is in force must be reproducible from the repository
alone: `git ls-files | python3 scripts/check-naming.py` (exit 0 = clean) run
against a fresh clone of `main`. The CI step above is that proof.
