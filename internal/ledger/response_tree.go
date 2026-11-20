// Response tree storage (CHECKLIST 237-238): the durable index of
// the Responses conversation tree — one row per served POST
// /v1/responses create, linking each response to its parent
// (previous_response_id). Two responses naming the same parent are
// two branches of that parent; the tree is the parent/children
// graph, not a single chain.
//
// Deliberately metadata only: ids, model, serving provider,
// status, creation time and the stream flag — never input or
// output text. The in-memory responses store (internal/responses)
// stays the continuation source of truth (it holds the transcript
// a branch re-sends); this table is the durable tree index, so the
// tree survives a restart even though the transcripts do not — a
// continuation of a response lost to a restart still answers 404,
// because the transcript is gone, even when the tree still shows
// the node. That asymmetry is honest and documented in
// docs/responses-api.md.
//
// Migration: the table is created by the schema in ledger.go
// (CREATE TABLE IF NOT EXISTS), which Open runs on every open — a
// database created by an older build gains the table (empty) on
// first open, with no backfill: rows recorded before this slice
// have no tree entry, and the tree endpoint shows only what was
// recorded since. Never retroactively invented.
package ledger

import (
	"database/sql"
	"fmt"
)

// ResponseTreeNode is one node of the Responses tree: a served
// response's identity in the tree. ParentID is "" for a fresh
// create (a tree root).
type ResponseTreeNode struct {
	ID        string `json:"id"`
	ParentID  string `json:"parent_id,omitempty"`
	Model     string `json:"model"`
	Provider  string `json:"provider,omitempty"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	Stream    bool   `json:"stream"`
}

// RecordResponseTree upserts one tree node: a create records its
// node, a stream's terminal store re-records the same id with its
// final status (INSERT OR REPLACE, so the row is never duplicated
// and the latest status wins).
func (l *Ledger) RecordResponseTree(n ResponseTreeNode) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("ledger: no database")
	}
	stream := 0
	if n.Stream {
		stream = 1
	}
	_, err := l.db.Exec(`INSERT OR REPLACE INTO response_tree
		(id, parent_id, model, provider, status, created_at, stream)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.ParentID, n.Model, n.Provider, n.Status, n.CreatedAt, stream)
	if err != nil {
		return fmt.Errorf("ledger: record response tree node: %w", err)
	}
	return nil
}

// ResponseTreeNode returns the tree node for id, and whether one
// was recorded. A response created before this slice (or on a
// gateway whose ledger was wiped) has no node — that is a miss,
// never a synthesised node.
func (l *Ledger) ResponseTreeNode(id string) (ResponseTreeNode, bool, error) {
	var n ResponseTreeNode
	if l == nil || l.db == nil {
		return n, false, fmt.Errorf("ledger: no database")
	}
	var stream int
	err := l.db.QueryRow(`SELECT id, parent_id, model, provider, status,
		created_at, stream FROM response_tree WHERE id = ?`, id).
		Scan(&n.ID, &n.ParentID, &n.Model, &n.Provider, &n.Status, &n.CreatedAt, &stream)
	if err == sql.ErrNoRows {
		return n, false, nil
	}
	if err != nil {
		return n, false, fmt.Errorf("ledger: read response tree node: %w", err)
	}
	n.Stream = stream != 0
	return n, true, nil
}

// ResponseTreeChildren returns the direct children of parentID —
// the branches off that response — oldest first (creation order).
// created_at is second-granularity, so two branches created in
// the same second tie: rowid (insertion order) breaks the tie,
// which is creation order except for a re-recorded stream row
// (INSERT OR REPLACE moves it to the end — the row then reflects
// its latest record time, honestly). An empty parentID is never
// queried: roots are found by ResponseTreeAll, not by asking for
// children of "".
func (l *Ledger) ResponseTreeChildren(parentID string) ([]ResponseTreeNode, error) {
	if l == nil || l.db == nil {
		return nil, fmt.Errorf("ledger: no database")
	}
	rows, err := l.db.Query(`SELECT id, parent_id, model, provider, status,
		created_at, stream FROM response_tree WHERE parent_id = ?
		ORDER BY created_at ASC, rowid ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("ledger: list response tree children: %w", err)
	}
	defer rows.Close()
	return scanResponseTreeNodes(rows)
}

// ResponseTreeAll returns up to limit tree nodes, oldest first —
// the whole forest the tree endpoint renders. limit <= 0 means
// 500: the ledger is unbounded over time, but a dashboard render
// is not the place to dump every response the gateway ever served.
func (l *Ledger) ResponseTreeAll(limit int) ([]ResponseTreeNode, error) {
	if l == nil || l.db == nil {
		return nil, fmt.Errorf("ledger: no database")
	}
	if limit <= 0 {
		limit = 500
	}
	rows, err := l.db.Query(`SELECT id, parent_id, model, provider, status,
		created_at, stream FROM response_tree
		ORDER BY created_at ASC, rowid ASC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("ledger: list response tree: %w", err)
	}
	defer rows.Close()
	return scanResponseTreeNodes(rows)
}

// DeleteResponseTree removes the tree node for id (a DELETE
// /v1/responses/{id}). Its children keep their parent_id naming
// the deleted id: the tree endpoint renders them as roots (their
// parent is gone), with the parent id still visible — a deleted
// parent is shown as missing, never silently re-parented. Deleting
// an id with no node is a no-op, never an error.
func (l *Ledger) DeleteResponseTree(id string) error {
	if l == nil || l.db == nil {
		return fmt.Errorf("ledger: no database")
	}
	if _, err := l.db.Exec(`DELETE FROM response_tree WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ledger: delete response tree node: %w", err)
	}
	return nil
}

func scanResponseTreeNodes(rows *sql.Rows) ([]ResponseTreeNode, error) {
	out := []ResponseTreeNode{}
	for rows.Next() {
		var n ResponseTreeNode
		var stream int
		if err := rows.Scan(&n.ID, &n.ParentID, &n.Model, &n.Provider, &n.Status, &n.CreatedAt, &stream); err != nil {
			return nil, fmt.Errorf("ledger: scan response tree node: %w", err)
		}
		n.Stream = stream != 0
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ledger: scan response tree nodes: %w", err)
	}
	return out, nil
}
