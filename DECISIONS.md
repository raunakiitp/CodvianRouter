# Codvian Router — Decisions Log

Each entry: date, decision, reason, alternatives rejected.

---

## 2026-11-20 ~17:02 IST — CI fix approach: trim \r in tokencount + .gitattributes

**Decision:** Fix the CRLF CI failure two ways:
1. Trim `\r` from `parts[1]` in `LoadTiktokenBpe` before `strconv.Atoi` — makes the code robust on any checkout.
2. Add `.gitattributes` with `*.tiktoken binary` — prevents git from ever converting line endings in BPE files.

**Reason:** The root cause is `core.autocrlf=true` on Windows converting `\n` → `\r\n` in `*.tiktoken` files on checkout. CI on Linux never saw this. The code fix is a one-liner; the gitattributes fix is the permanent prevention.

**Alternatives rejected:** Forcing `core.autocrlf=false` globally — would change user's git config, not our place. Fixing only the gitattributes without the code fix — would not help the existing checkout.

---

## 2026-11-20 ~17:02 IST — Working directory is d:\CodvianRouterClone (cloned fresh)

**Decision:** Use `d:\CodvianRouterClone` as the working git repo (cloned from GitHub). The `d:\CodvianRouter` dir was empty (no .git).

**Reason:** `d:\CodvianRouter` had no `.git`. Cloned fresh from `https://github.com/raunakiitp/CodvianRouter` to `d:\CodvianRouterClone`.

---

## 2026-11-20 ~17:02 IST — B3 verification runs without race flag on Windows

**Decision:** Run `go test -count=1` without `-race` for Windows local verification. CI runs with `-race` on Linux (ubuntu-24.04 with gcc).

**Reason:** `go test -race` requires CGo which requires gcc. Windows machine has no gcc. Race testing happens on CI.

---

## 2026-11-20 ~17:13 IST — Sync API reference docs and test CRLF normalization

**Decision:**
1. Regenerated `docs/API_REFERENCE.md`, `docs/llm.txt`, `tests/fixtures/v1/m8.21/routes.json`, and `web/tryit.html` via `python scripts/gen-api-docs.py` to sync with updated `/v1/me/sessions` descriptions from commit `7cfc391`.
2. In `internal/server/api_reference_test.go` (`readRepoFile`) and `internal/server/decision_price_display_test.go`, normalize `\r\n` -> `\n` to ensure exact substring/boundary checks pass consistently across platforms.

**Reason:** Guard table descriptions and doc fixtures were out of sync since AK-3; CRLF normalization prevents Windows-specific string index failures.

---

