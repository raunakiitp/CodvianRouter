// Transcript persistence (CHECKLIST 315-316): the Responses
// transcripts survive a gateway restart.
//
// The tree index already survives a restart (the ledger's
// response_tree table), but the transcripts lived only in the
// in-memory store, so a continuation of a restart-lost response
// answered 404. The transcripts now persist beside the index —
// responses.json in the same data dir as ledger.db — as one
// atomic snapshot: the whole live store rewritten on every
// mutation (create, stream terminal store, delete), so the file
// is always a complete, self-consistent copy of the store,
// including evictions and deletes (an evicted or deleted record
// is gone from the next snapshot, never resurrected by a
// restart). Writes go through a temp file in the same directory
// + fsync + rename + directory fsync, so a crash mid-write
// leaves either the old file or the new one, never a torn one.
//
// The file holds the full records (transcript + served object +
// token counts), so a restart restores continuations exactly:
// the same transcript is re-sent, the same served object is
// retrieved, and the session token counts keep accumulating.
// Records older than the store TTL, or over the store cap, are
// not restored (Store.Restore enforces both), so a restart
// never resurrects a response the live store would already
// have dropped. Counters (created/continuations/evicted) are
// NOT persisted: they restart from the restored set, and the
// dashboard feed reports how many records the restore loaded
// (see Server.RestoreResponses), so a restart is visible,
// never silently re-counted.
//
// The file is the gateway's own data-dir state, like
// benchmark.json: it never leaves the machine, and the
// dashboard feed still strips transcripts — only this file
// (and the in-memory store) ever holds them.
package responses

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// persistVersion is the responses.json format version. A file
// with any other version is refused, never guessed at.
const persistVersion = 1

// persistedRecord is one record as stored in responses.json.
// It mirrors Record field for field (including the transcript
// and served-object JSON, which Record itself never marshals —
// json:"-" — so feeds can never leak them through a Record
// marshal by accident).
type persistedRecord struct {
	ID                  string          `json:"id"`
	Model               string          `json:"model"`
	ProviderID          string          `json:"provider"`
	Status              string          `json:"status"`
	CreatedAt           int64           `json:"created_at"`
	PreviousID          string          `json:"previous_response_id,omitempty"`
	Stream              bool            `json:"stream"`
	Owner               string          `json:"owner,omitempty"`
	TranscriptJSON      json.RawMessage `json:"transcript,omitempty"`
	ResponseJSON        json.RawMessage `json:"response,omitempty"`
	TurnInputTokens     int             `json:"turn_input_tokens,omitempty"`
	TurnOutputTokens    int             `json:"turn_output_tokens,omitempty"`
	SessionInputTokens  int             `json:"session_input_tokens,omitempty"`
	SessionOutputTokens int             `json:"session_output_tokens,omitempty"`
}

// persistedFile is the whole responses.json document.
type persistedFile struct {
	Version int               `json:"version"`
	Records []persistedRecord `json:"records"`
}

func toPersisted(rec Record) persistedRecord {
	return persistedRecord{
		ID:                  rec.ID,
		Model:               rec.Model,
		ProviderID:          rec.ProviderID,
		Status:              rec.Status,
		CreatedAt:           rec.CreatedAt,
		PreviousID:          rec.PreviousID,
		Stream:              rec.Stream,
		Owner:               rec.Owner,
		TranscriptJSON:      rec.TranscriptJSON,
		ResponseJSON:        rec.ResponseJSON,
		TurnInputTokens:     rec.TurnInputTokens,
		TurnOutputTokens:    rec.TurnOutputTokens,
		SessionInputTokens:  rec.SessionInputTokens,
		SessionOutputTokens: rec.SessionOutputTokens,
	}
}

func (p persistedRecord) toRecord() Record {
	return Record{
		ID:                  p.ID,
		Model:               p.Model,
		ProviderID:          p.ProviderID,
		Status:              p.Status,
		CreatedAt:           p.CreatedAt,
		PreviousID:          p.PreviousID,
		Stream:              p.Stream,
		Owner:               p.Owner,
		TranscriptJSON:      p.TranscriptJSON,
		ResponseJSON:        p.ResponseJSON,
		TurnInputTokens:     p.TurnInputTokens,
		TurnOutputTokens:    p.TurnOutputTokens,
		SessionInputTokens:  p.SessionInputTokens,
		SessionOutputTokens: p.SessionOutputTokens,
	}
}

// SaveFile atomically replaces path with a snapshot of recs. An
// empty path is an error, never a silent no-op: the caller must
// have a data dir to persist into. A missing parent directory is
// an error too — the server creates the data dir at startup, so
// a missing one here means the gateway is misconfigured, not
// that persistence should be skipped quietly.
func SaveFile(path string, recs []Record) error {
	if path == "" {
		return fmt.Errorf("responses: no persistence path")
	}
	doc := persistedFile{Version: persistVersion, Records: make([]persistedRecord, 0, len(recs))}
	for _, rec := range recs {
		if rec.ID == "" {
			continue
		}
		doc.Records = append(doc.Records, toPersisted(rec))
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("responses: marshal persistence file: %w", err)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".responses-*.tmp")
	if err != nil {
		return fmt.Errorf("responses: create persistence temp file: %w", err)
	}
	tmpName := tmp.Name()
	// A failed write leaves the temp file behind unless it is
	// removed here; a successful rename removes it by moving.
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("responses: write persistence file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("responses: sync persistence file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("responses: close persistence file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("responses: replace persistence file: %w", err)
	}
	// The rename itself must reach the disk, or a crash could
	// lose the new file while the old one is already gone.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// LoadFile reads path back into records, oldest first (the
// order they were saved in). A missing file is a fresh install,
// not an error: it returns nil, nil. Any other failure —
// unreadable file, invalid JSON, wrong version — is an error:
// the caller logs it and starts with an empty store rather
// than guessing at a corrupt file's contents. Records with an
// empty id are skipped (they can never be retrieved or
// continued); everything else is returned verbatim for
// Store.Restore to apply the TTL and cap.
func LoadFile(path string) ([]Record, error) {
	if path == "" {
		return nil, fmt.Errorf("responses: no persistence path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("responses: read persistence file: %w", err)
	}
	var doc persistedFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("responses: parse persistence file: %w", err)
	}
	if doc.Version != persistVersion {
		return nil, fmt.Errorf("responses: persistence file version %d, want %d", doc.Version, persistVersion)
	}
	out := make([]Record, 0, len(doc.Records))
	for _, p := range doc.Records {
		if p.ID == "" {
			continue
		}
		out = append(out, p.toRecord())
	}
	return out, nil
}
