// DB import (POST /admin/db/import, Tier 2 ALWAYS_PROTECTED).
//
// Imports a DB export file (the `codvian-router-db-export/v1` JSON
// the export route writes) and MERGES its usage history into the
// ledger — it never resets or wipes existing history
// (research-log entry 154). Existing rows are untouched; only
// events the ledger does not already hold are added, and exact
// duplicates are skipped, never double-counted. The config section
// of the file is ignored: providers, keys, prices and settings are
// never overwritten by an import.
//
// Credential boundary: gateway-key auth (always required, even
// when Tier 3 auth is relaxed — the Tier 2 wrapper enforces that)
// AND the typed confirmation phrase "IMPORT" in the multipart
// form — a key alone is not enough, exactly like the other Tier 2
// destructive routes. The phrase is deliberately distinct from
// the export's "EXPORT", so a body captured for one cannot
// authorise the other.
//
// The upload is multipart/form-data with two fields: `file` (the
// export JSON) and `confirm` ("IMPORT"). The whole file is
// validated before any row is written (see ledger.ParseExport /
// Ledger.ImportEvents): a corrupt file — bad JSON, wrong format
// version, one bad event among good ones — is rejected with an
// honest error and zero rows are imported. An import is never
// partial and never silent about it.
//
// The response reports what actually happened — events imported,
// duplicates skipped, events in the file, the source's total, and
// the ledger totals afterwards — so the dashboard can show the
// real outcome instead of a generic success.
package server

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/ledger"
)

// dbImportConfirm is the exact phrase the caller must type (and
// send) to import a DB export file.
const dbImportConfirm = "IMPORT"

// dbImportMaxBytes caps the uploaded file: export files are capped
// at 500 events server-side, so 10 MiB leaves ample headroom for a
// hand-merged file while bounding one request's work.
const dbImportMaxBytes = 10 << 20

// dbImportResponse is the POST /admin/db/import payload.
type dbImportResponse struct {
	Imported          int     `json:"imported"`
	Duplicates        int     `json:"duplicates_skipped"`
	EventsInFile      int     `json:"events_in_file"`
	SourceEventsTotal int     `json:"source_events_total"`
	SourceTruncated   bool    `json:"source_truncated"`
	TotalRequests     int     `json:"total_requests"`
	TotalCostUSD      float64 `json:"total_cost_usd"`
	Note              string  `json:"note"`
}

// handleDBImport serves POST /admin/db/import (Tier 2): gateway-key
// auth plus the typed confirmation phrase IMPORT, then a multipart
// export file whose usage history is merged into the ledger.
func (s *Server) handleDBImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.ledger == nil {
		writeError(w, http.StatusServiceUnavailable, "db import unavailable: no ledger configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, dbImportMaxBytes)
	if err := r.ParseMultipartForm(dbImportMaxBytes); err != nil {
		s.logger.Printf("db import: could not parse upload: %v", guardrails.RedactLogLine(err.Error()))
		writeError(w, http.StatusBadRequest, "invalid request: expected a multipart upload with fields \"file\" (the export JSON) and \"confirm\" (\"IMPORT\")")
		return
	}
	if r.FormValue("confirm") != dbImportConfirm {
		writeError(w, http.StatusBadRequest, "confirmation phrase does not match: type "+dbImportConfirm+" to confirm")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid request: no \"file\" field — upload the DB export JSON file")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		s.logger.Printf("db import: could not read upload: %v", guardrails.RedactLogLine(err.Error()))
		writeError(w, http.StatusBadRequest, "could not read the uploaded file")
		return
	}
	exp, err := ledger.ParseExport(data)
	if err != nil {
		// ParseExport already names the problem honestly
		// (wrong format version, corrupt event, …) — pass it
		// through as the error message, never a bare 400.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	imported, duplicates, err := s.ledger.ImportEvents(r.Context(), exp.Usage.Events)
	if err != nil {
		s.logger.Printf("db import: merge failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "could not merge usage history: no events were imported")
		return
	}
	sum, err := s.ledger.Summary(r.Context(), time.Time{}, time.Time{})
	if err != nil {
		s.logger.Printf("db import: summary after import failed: %v", guardrails.RedactLogLine(err.Error()))
		writeServerError(w, http.StatusInternalServerError, "usage history was imported but the new totals could not be read")
		return
	}
	s.logger.Printf("db import: merged %d event(s), skipped %d duplicate(s); ledger now holds %d request(s)", imported, duplicates, sum.Requests)
	writeJSON(w, http.StatusOK, dbImportResponse{
		Imported:          imported,
		Duplicates:        duplicates,
		EventsInFile:      len(exp.Usage.Events),
		SourceEventsTotal: exp.Usage.EventsTotal,
		SourceTruncated:   exp.Usage.Truncated,
		TotalRequests:     sum.Requests,
		TotalCostUSD:      sum.CostUSD,
		Note:              fmt.Sprintf("Merged %d event(s) into the usage history; %d duplicate(s) skipped, never double-counted. Existing history was preserved — nothing was reset or wiped.", imported, duplicates),
	})
}
