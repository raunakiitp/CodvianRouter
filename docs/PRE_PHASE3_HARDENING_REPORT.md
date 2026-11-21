# Pre-Phase-3 Hardening Report (G-01..G-07)

Execution: full autonomous Pre-Phase-3 hardening, merged to `main` (now `3aa422e`).

## G-01 - ToolCalling metadata / capability detection
Audit snippet `providers_reference.go:5338` said "ToolCalling unknown". Current source models
capability honestly: catalog.ToolCalling (unknown/supported/emulated), Provider.SupportsTools()
true only for supported, Validate rejects bogus enums, TestCT4EmulatedNeverCountsAsToolSupport
pins unknown/empty/emulated as never tool-capable. Auto-router uses its own operator-declared
per-model capabilities and never assumes a missing capability. RESOLVED-by-existing-impl (verified,
not duplicated; no fabricated metadata).

## G-02 - Dashboard keyboard accessibility
skip-link to #mainContent, :focus-visible ring (existing tokens), modal focus helpers + Tab trap +
focus return + Escape on both dialogs. Regression: internal/server/dashboard_a11y_test.go (served
artifact). PR #35 (f870cf8). No palette/layout change.

## G-03 - Reproducible Linux benchmark infra
scripts/README.md documents Linux-authoritative bench rig + Windows limit. bench-infra-smoke.yml
validates infra on ubuntu (prereqs + compile-only go test -c loop). No fabricated numbers. Runner
verdict UNVERIFIED here (403 on job log); main ci passes. PR #36 (3aa422e).

## G-04 - Genuine Playwright browser smoke
tests/playwright suite + browser-smoke.yml (boots real router on ubuntu, Chromium). Synthetic
fixtures only. Runner verdict UNVERIFIED here; main ci passes. PR #37 (fa4fcbf).

## G-05 - Docs drift guard
TestDocumentedAPIRoutesExistInGuardTable: documented routes must exist in guard table. Current
docs pass; in server suite (CI-gated). PR #34 (0845846).

## G-06 - SBOM + vuln gate
scripts/sbom.py (CycloneDX-1.4 over go list -m -json all, --self-check) + supply-chain.yml
(generates/validates SBOM, runs govulncheck; DB unavailable reported, never "zero"). Generated:
43 modules, valid 1.4. PR #36 (3aa422e).

## G-07 - Tooling consolidation
scripts/README.md inventory + maintenance rules. No script deleted. PR #36 (3aa422e).
