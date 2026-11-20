# Scope proposal — approved-scope parity denominator (Master Prompt v2 §3.3)

Date: 2026-11-19. Status: **APPROVED by Raunak (voice call, 2026-11-20 ~01:21 IST) — all 6 candidates.**
Approved 2026-11-20 ~01:21 IST by Raunak (voice call): all 6 candidates. Rows 1079/1080/1086/1075/948 leave the approved-scope denominator entirely; row 927 stays in the denominator (only its 11-locale portion is out of scope — the English part stays). Approved-scope denominator: 647 rows (raw 652 minus 5). Current approved-scope parity: strict 37.2%, weighted 51.9% (FULL 241 / PARTIAL 190 / MISSING 214 / WRONG 2). Raw parity is always reported too (37.0% / 51.6% on 652). Nothing in this file changes any PARITY.md verdict.

## Candidates for exclusion (one line + reason each)

- Area 13, Desktop app (Electron) — PARITY.md:1079, MISSING: a full Electron
  app is a separate product surface; the router ships as a single Go binary + CLI.
- Area 13, Self-update mechanism — PARITY.md:1080, MISSING: Electron
  autoUpdater; tied to the desktop app above (binary update is covered separately
  by the PKG-3 lifecycle CLI, which stays in scope).
- Area 13, Extra install surfaces — PARITY.md:1086, MISSING: AUR, Nix flake,
  Podman/Quadlet, Fly.io, Helm/K8s manifests, PWA, Termux/Android, VS Code
  extension — each is a separate distribution channel beyond GoReleaser +
  Docker + install.sh.
- Area 13, Launch/wrap external CLIs — PARITY.md:1075, PARTIAL: 18 per-tool
  setup-* commands (claude/codex/cursor/cline/kilo/roo/crush/zed/goose/aider/
  qwen/gemini/…) are cloud-agent integrations; a single generic `run` exists.
- Area 10, Language packs for rules (11 locales) — PARITY.md:948, MISSING:
  non-English prose-rule packs; English-only compression rules are the shipped scope.
- Area 10, Caveman 11 locales (locale portion only) — PARITY.md:927, PARTIAL:
  English rules are built; only the 11-locale portion is proposed out of scope,
  the rest of the row stays in the denominator.

## Checked and NOT proposed (stay in the denominator)

- TLS stealth / MITM root CA: no standalone parity row exists (grep finds it only
  in the v1 §2.7 MUST-NEVER-COPY list, PARITY.md:390-403) — nothing to exclude.
- Cookie-session providers: already FULL by design — Area 3 row PARITY.md:581
  (ToS-grey providers catalogued and refused at add/load/resolve).
- "67 UI locales": no such row exists in PARITY.md (grep for "67" returns nothing);
  the only locale rows are the 11 compression-rule locales above. The 67 figure
  needs verification against OmniRoute source before any proposal.
- Flat-rate / subscription $0 treatment — PARITY.md:979, MISSING: real pricing
  correctness gap (Codex/Claude Code flat-rate classification), stays in scope.

## Approval

Raunak approved ALL candidates on voice call 2026-11-20 ~01:21 IST. Approved rows move out of the approved-scope denominator only; raw parity (652) is always reported too. Applied: 5 rows fully out (1079, 1080, 1086, 1075, 948); row 927 stays in the denominator (locale portion only).
