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
)
