// Package web embeds the approved Codvian Router dashboard (a single
// self-contained HTML file, web/index.html) so the binary serves it
// with no runtime file dependency. The dashboard is a static UI with
// sample data; all live data flows through the authenticated API.
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
