// Package webexec — A3 Item 4: web-cookie session storage and
// headless-browser execution for the 37 catalog WebCookie providers.
//
// What this package provides:
//
//   - SessionState: a browser storage state (cookies + per-origin
//     localStorage), compatible in shape with Playwright's storageState
//     format so an operator-exported state can be imported verbatim.
//   - SessionStore: per-catalog-ID storage of that state, ENCRYPTED at
//     rest with the per-data-dir Vault (Argon2id-derived key) following
//     exactly the sealing conventions of internal/server/oauth_conn.go:
//     sealed ciphertext is stored in memory only when no vault is
//     configured, plaintext never touches disk unsealed, and stored
//     secrets are never included in any status/list output.
//   - The executor harness: an Executor interface, an HTTP-level session
//     executor as the DEFAULT implementation, and a pluggable
//     BrowserSessionFactory seam for a real headless browser.
//
// Security model (non-negotiable, mirrors the repo's ToS posture):
//   - NO MITM proxy, NO root-CA installation, NO stealth/anti-detection
//     tricks — EVER. The operator hands over their OWN browser session
//     state for their OWN account, by their own choice; the executor
//     replays it verbatim and nothing else.
//   - A stale storage state (expired/replayed cookies that the provider
//     rejects with 401) surfaces as ErrStaleSession, which the
//     provider-client adapter maps to an upstream 401 so the router
//     fails over instead of retrying a dead session.
//   - A provider quota answer (429) passes through as
//     provider.RateLimitError (with Retry-After when present) so account
//     fallback / pacing sees the real upstream signal.
//   - Temporary chat (no history) is requested where the provider's web
//     UI supports it, so executed prompts leave no persistent
//     conversation in the operator's account.
//
// Headless-browser limitation, stated honestly: the repo's existing
// browser tooling is the Node/Playwright suite under tests/playwright
// (driven by the browser-smoke CI workflow), which cannot be invoked
// from Go tests. No Go headless-browser dependency exists in go.mod and
// the Go-test CI runners do not ship Chrome or Playwright chromium.
// The harness therefore defines BrowserSessionFactory as the seam and
// ships NO browser-backed implementation by default: any future
// chromedp/playwright-go driver registers itself with
// RegisterBrowserFactory and becomes available to every executor.
// Without a registered factory, session capture answers ErrNoBrowser
// with the reason. All executor behavior is exercised in tests against
// mock web surfaces (httptest), never against live provider web UIs.
package webexec
