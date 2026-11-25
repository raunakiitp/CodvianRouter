// M1.5 — POST /admin/cache/flush handler.
//
// The handler lives here; route registration and auth are NOT this
// package's job. The coordinator wires it into the guard table as
// POST /admin/cache/flush (admin scope) after A1's guard-table work
// merges — until then nothing exposes it, so it cannot be reached
// without the admin credential the guard table enforces.
//
// Body (JSON, optional): {"key_id": "<gateway key id>"} flushes
// only that key's entries; an empty body (or {}) flushes everything.
// Response: {"flushed": N, "scope": "all"|"key", "key_id": "..."}.
// Any non-POST method gets 405; malformed JSON gets 400.
// Marshal failure returns 500; never 200 with truncated body.
package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

// FlushStore is what the flush handler needs: a cache that can drop
// everything or drop one tenant's entries. PersistentCache satisfies
// it; ExactCache alone does not (it records no tenant per entry).
type FlushStore interface {
	FlushAll(ctx context.Context) int
	FlushTenant(ctx context.Context, tenant string) int
}

// NewFlushHandler returns the POST /admin/cache/flush handler for store.
func NewFlushHandler(store FlushStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			KeyID string `json:"key_id"`
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
		}
		var flushed int
		var scope string
		if body.KeyID != "" {
			flushed = store.FlushTenant(r.Context(), body.KeyID)
			scope = "key"
		} else {
			flushed = store.FlushAll(r.Context())
			scope = "all"
		}
		out := map[string]any{"flushed": flushed, "scope": scope}
		if body.KeyID != "" {
			out["key_id"] = body.KeyID
		}
		buf := &bytes.Buffer{}
		enc := json.NewEncoder(buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(out); err != nil {
			http.Error(w, "failed to encode response", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(buf.Bytes())
	})
}
