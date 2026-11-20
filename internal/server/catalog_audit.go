// Catalog-audit feed (CHECKLIST 227-228): GET /dashboard/catalog-audit.
//
// The dashboard's Models view renders this feed: the last catalog
// freshness audit (run by `codvian-router catalog audit` against
// the live models.dev catalog + base-URL/website reachability),
// with its audit date and drift counts. The report is committed
// into the binary (web/catalog-audit.json, embedded via web.go),
// so the feed serves the recorded audit — it never re-fetches
// models.dev itself and never edits the catalog.
//
// The payload carries no secrets: every URL in it comes from the
// public catalog (published in internal/catalog) or the public
// models.dev catalog. It carries no keys, no hashes, and no
// operator-configured provider data, so like the other dashboard
// feeds it is unauthenticated by design.
//
// When no audit has been committed yet (empty embed), the feed
// answers available:false — an honest empty state, not an error
// and never an invented audit.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/raunakiitp/codvian-router/web"
)

// handleCatalogAudit serves GET /dashboard/catalog-audit: the
// committed audit report (web.CatalogAuditJSON), verbatim. A
// missing/empty/unparseable embed answers available:false.
func (s *Server) handleCatalogAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	raw := bytes.TrimSpace(web.CatalogAuditJSON)
	if len(raw) == 0 || !json.Valid(raw) {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"note":      "No catalog audit has been recorded yet — run `codvian-router catalog audit` to produce one.",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
