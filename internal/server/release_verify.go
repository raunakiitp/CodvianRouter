// Release-verify feed (CHECKLIST 289-290): GET /dashboard/release-verify.
//
// The dashboard's Benchmark view renders this feed: the last
// recorded `release verify` run — the verified tag, archive/SBOM
// counts and bundle status. The record is committed into the
// binary (web/release-verify.json, embedded via web.go), so the
// feed serves the recorded run — it never re-runs the verifier
// itself and never invents a pass. The first recorded run is the
// 267-268 snapshot proof (re-run by 289-290): an UNSIGNED snapshot,
// so its verdict is FAILED, correctly — the bundle/signature
// checks fail by design and the checks that can pass on an
// unsigned snapshot all pass (checksums 12/12, archives 6/6,
// SBOM 6/6). A real signed release must print VERIFIED; until
// the first tag runs in CI, no such run exists to record.
//
// The payload carries no secrets: tag, counts and notes only —
// no hashes, no keys, no operator-configured provider data, so
// like the other dashboard feeds it is unauthenticated by design.
// Items 307-308: the record also carries a `gate` field stating
// the verdict on the release gate — POST-PUBLISH TRIPWIRE by
// final design, a true pre-publish gate needing GoReleaser Pro —
// so the dashboard card can never present the in-job verify as a
// pre-publish gate. The field is part of the committed record,
// served verbatim like the rest of it.
//
// When no run has been committed yet (empty embed), the feed
// answers available:false — an honest empty state, not an error
// and never an invented run.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/raunakiitp/codvian-router/web"
)

// handleReleaseVerify serves GET /dashboard/release-verify: the
// committed release-verify record (web.ReleaseVerifyJSON),
// verbatim. A missing/empty/unparseable embed answers
// available:false.
func (s *Server) handleReleaseVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	raw := bytes.TrimSpace(web.ReleaseVerifyJSON)
	if len(raw) == 0 || !json.Valid(raw) {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"note":      "No release-verify run has been recorded yet — run `codvian-router release verify <tag> --dir dist/` against a snapshot build to record one.",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
