// Package web embeds the approved Codvian Router dashboard (a single
// self-contained HTML file, web/index.html) so the binary serves it
// with no runtime file dependency. Provider data is live from
// GET /api/providers (an unauthenticated feed — it carries no secrets);
// the remaining sections still show sample data, labelled "Demo data"
// in the page, until their live feeds land.
// Design source of truth: the exported artifact at
// ~/workspace/your_files/codvian-router-dashboard/codvian-router-dashboard.html
// (pure black / pure white themes, lime palette, borderless curved
// cards, smooth section transitions, no Playground section).
package web

import _ "embed"

// IndexHTML is the dashboard page served at GET /dashboard.
var (
	//go:embed index.html
	IndexHTML []byte

	// CatalogAuditJSON is the last committed catalog freshness
	// audit (docs/catalog-audit-<date>.json, produced by
	// `codvian-router catalog audit`), served verbatim by
	// GET /dashboard/catalog-audit. Empty until an audit is run.
	//go:embed catalog-audit.json
	CatalogAuditJSON []byte

	// ReleaseVerifyJSON is the last recorded `release verify`
	// run (docs/release-verify-<date>-snapshot.json, the
	// 267-268 snapshot proof re-run by 289-290), served
	// verbatim by GET /dashboard/release-verify. It records
	// the verified tag, archive/SBOM counts and bundle status
	// of that run only — never a prediction about the next
	// release, and never an invented pass. Empty until a run
	// is recorded.
	//go:embed release-verify.json
	ReleaseVerifyJSON []byte
)
