// Admin reconciliation for file records that predate ownership scoping
// (AUD-SEC-010).
//
// Before owner scoping, the files index had no owner field, so every
// record deserialises with an empty OwnerKeyID. Those records are NOT
// visible to any authenticated key — visibleTo treats an empty owner as
// matching only an empty caller id, never as a wildcard — which keeps
// them private but would silently STRAND legitimate uploads. The fix is
// not to widen visibility: it is an explicit, admin-authorized, auditable
// migration, plus a forensic listing so an operator can see what exists
// before deciding.
//
// Both handlers require the admin scope: a non-admin key cannot list
// another key's stranded uploads, and cannot claim them.
package server

import (
	"encoding/json"
	"net/http"
)

// filesReclaimRequest is the POST /admin/files/reclaim body. With no
// file_ids every unowned record is reclaimed.
type filesReclaimRequest struct {
	OwnerKeyID string   `json:"owner_key_id"`
	FileIDs    []string `json:"file_ids"`
}

// handleFilesLegacyList serves GET /admin/files/legacy: the forensic view
// of records with no owner. Read-only.
func (s *Server) handleFilesLegacyList(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	if !s.adminScopeOK(r) {
		writeError(w, http.StatusForbidden, "requires scope admin: this action needs an admin gateway key")
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, "files store unavailable")
		return
	}
	unowned, err := st.LegacyUnowned()
	if err != nil {
		writeServerError(w, http.StatusInternalServerError, "files index unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":             "list",
		"unowned_count":      len(unowned),
		"unowned":            unowned,
		"reclaim_endpoint":   "POST /admin/files/reclaim",
		"reclaim_owner_hint": "set owner_key_id (and optionally file_ids) to assign these records to a key",
	})
}

// handleFilesReclaim serves POST /admin/files/reclaim: the supported
// migration for unowned records. Admin-authorized and idempotent.
func (s *Server) handleFilesReclaim(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		s.unauthorized(w, r)
		return
	}
	if !s.adminScopeOK(r) {
		writeError(w, http.StatusForbidden, "requires scope admin: this action needs an admin gateway key")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.filesStore()
	if err != nil {
		writeServerError(w, http.StatusServiceUnavailable, "files store unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
	var req filesReclaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.OwnerKeyID == "" {
		writeError(w, http.StatusBadRequest, "owner_key_id is required: reclaiming to an empty owner would restore exactly the unowned state being repaired")
		return
	}
	reclaimed, remaining, err := st.ReclaimLegacy(req.OwnerKeyID, req.FileIDs)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Printf("files: admin reclaim assigned %d previously unowned file record(s) to a key (%d still unowned)", reclaimed, remaining)
	writeJSON(w, http.StatusOK, map[string]any{
		"object":            "files.reclaim",
		"reclaimed":         reclaimed,
		"remaining_unowned": remaining,
	})
}
