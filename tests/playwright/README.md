# Playwright browser smoke (G-04)

Drives the REAL dashboard in a browser. Runs in
`.github/workflows/browser-smoke.yml` (ubuntu, Chromium). Synthetic
fixtures only; no real provider keys.

## Local run
    cd tests/playwright && npm install && npx playwright install chromium
    BASE_URL=http://127.0.0.1:20128 npx playwright test

## What it covers
- Dashboard shell loads without console errors.
- Skip link + focus-ring surface present (regression-locked too in Go).
- Reveal-key control is keyboard-operable and never leaks a credential.
- Empty-state containers render honestly (no fabricated rows).

These are smoke checks; deeper served-JS behavior is covered by the Go
dashboard-js tests (internal/server/dashboard_*_test.go).
