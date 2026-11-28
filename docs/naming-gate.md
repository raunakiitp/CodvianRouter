# Naming gate — canonical enforcement and sanctioned exceptions

This document is the authoritative record for the repository's naming gate:
how it is enforced, what it bans, and the narrow, disclosed exceptions.

## 1. What is banned

Any tracked file (code, comments, docs, README, workflows) must not contain:

| Class | Pattern intent |
|---|---|
| reference router | the sector baseline's product description in any casing, with `-`, `_` or space separator |
| System One / SystemOne | the baseline's judge component, **capitalized forms only** (see §3) |
| reference code path prefix | the baseline's source-tree prefix for its service files |
| OmniGlyph | baseline asset name |
| omni-skills | baseline skill-pack name |
| OMNI_ | baseline environment-variable prefix |
| omni-route | baseline product name, all separator variants |
| cheaper-inference | retired service name, all separator variants |

## 2. Where it is enforced (canonical gate)

The **canonical** gate is executable and runs in continuous integration:

* `scripts/ci.sh` runs `git ls-files | python3 scripts/check-naming.py` as a
  named step (`scripts/check-naming.py (whole-tree naming gate)`). CI fails
  when any tracked file matches a banned class.
* `internal/catalog/repo_brand_gate_test.go` mirrors the same rule as a Go
  test (`TestRepoBrandGateSelfTest`, `TestRepoWideSpacedBrandZero`), so a
  plain `go test ./...` also fails on a stain. It additionally proves the
  pattern table itself: one seeded string per class must match, and
  sanctioned lowercase prose must not.
* `scripts/check-naming.sh` is the older shell helper; it covers a narrower
  class set. It is retained for local use and is **not** the gate of record.

Both the Python script and the Go test build their patterns from character
fragments, so the banned literals do not exist as contiguous strings in the
gate source. Those two files are therefore the only files excluded from the
scan — the exclusion is structural, not a content allowance.

## 3. Sanctioned exception: the lowercase retired route path

The retired, fabricated endpoint `POST /v1/systemone` was removed and its
removal is recorded in `CHANGELOG.md`, guarded in `internal/server/guard_test.go`,
asserted absent in `internal/server/backlog_endpoints_test.go`, and probed in
`scripts/audit-fake.sh`. Those references are **lowercase** (`/v1/systemone`,
`systemone\\.completion`).

They are allowed, deliberately:

* The banned class is the **proper noun** naming the baseline's judge
  component. The gate matches `System` + `One` only in capitalized form,
  because the baseline brand is a proper noun. A lowercase URL path is a
  different token: it names a route this project no longer serves.
* The references are **historical and negative** — they prove the route is
  gone. Deleting them would delete the evidence that the fabrication was
  removed, which is worse than citing the old path.
* Renaming the path would rewrite history in the audit script and the
  changelog for cosmetic reasons; the route must keep no live implementation
  either way (proven by the guard and backlog tests).

If a future change adds a **capitalized** `System One`/`SystemOne` reference
for any reason, the gate fails — the exception is scoped to the lowercase
path only.

## 4. Local-only files are not evidence

A gate script that exists only on a developer machine proves nothing. Any
claim that the gate is in force must be reproducible from the repository
alone: `git ls-files | python3 scripts/check-naming.py` (exit 0 = clean) run
against a fresh clone of `main`.
