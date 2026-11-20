package a2a

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// Store is the SQLite persistence for A2A tasks, following the
// ledger's pattern (internal/ledger.Open): modernc.org/sqlite, no
// cgo, single writer connection, WAL + 5 s busy timeout, parent
// directories created 0700. The database lives at
// <data-dir>/a2a/tasks.db with tables a2a_tasks and a2a_task_events.
//
// Concurrency: every transaction starts with BEGIN IMMEDIATE
// (DSN _txlock=immediate), so a writer holds the write lock from
// the start instead of upgrading a read snapshot mid-transaction;
// Transition additionally retries SQLITE_BUSY a bounded number of
// times and reports exhaustion as the typed ErrBusy, never the
// driver's raw "database is locked" string. Foreign keys are
// enforced (PRAGMA foreign_keys=ON). The a2a/ directory is kept at
// 0700 and tasks.db at 0600, tightened on every open.
// Retention defaults (M9.8 follow-up, docs/design/a2a-retention-design.md
// §1/§3/§4): terminal tasks are pruned when updated_at is older than
// DefaultRetentionDays days; each owner holds at most
// DefaultMaxTasksPerKey tasks, oldest terminal evicted first. These
// are the design defaults; the config wiring slice passes operator
// values in through OpenWithLimits instead.
const (
	DefaultRetentionDays  = 7
	DefaultMaxTasksPerKey = 1000
)

type Store struct {
	db   *sql.DB
	path string
	// retentionDays / maxTasksPerKey are the resolved limits from
	// OpenWithLimits (zero in Limits means the defaults above).
	// The store never reads config: limits arrive as explicit
	// parameters only.
	retentionDays  int
	maxTasksPerKey int
}

const schema = `
CREATE TABLE IF NOT EXISTS a2a_tasks (
  id         TEXT PRIMARY KEY,
  owner      TEXT NOT NULL,
  state      TEXT NOT NULL,
  message    TEXT NOT NULL,
  result     TEXT,
  error      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_owner_created
  ON a2a_tasks(owner, created_at);
-- Prune predicate index (M9.8 retention): the prune is cross-owner
-- on (state, updated_at); the (owner, created_at) index does not
-- serve it.
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_state_updated
  ON a2a_tasks(state, updated_at);
CREATE TABLE IF NOT EXISTS a2a_task_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id    TEXT NOT NULL REFERENCES a2a_tasks(id),
  from_state TEXT NOT NULL DEFAULT '',
  to_state   TEXT NOT NULL,
  at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_a2a_task_events_task
  ON a2a_task_events(task_id, id);
`

// Limits tunes A2A task retention for one store. Zero values mean
// the defaults (DefaultRetentionDays / DefaultMaxTasksPerKey);
// negative values are also treated as defaults — the config layer
// rejects negatives at Validate time, so the store never has to.
type Limits struct {
	RetentionDays  int
	MaxTasksPerKey int
}

func resolveRetentionDays(d int) int {
	if d <= 0 {
		return DefaultRetentionDays
	}
	return d
}

func resolveMaxTasksPerKey(n int) int {
	if n <= 0 {
		return DefaultMaxTasksPerKey
	}
	return n
}

// Open opens (creating if needed) the A2A SQLite store under
// dataDir/a2a/tasks.db, applies the schema, and returns the Store.
// The a2a/ directory is tightened to 0700 (a pre-existing looser
// directory is chmodded down) and tasks.db to 0600 on every open.
// Retention uses the design defaults (7 days, 1,000 tasks per key);
// use OpenWithLimits for explicit limits.
func Open(dataDir string) (*Store, error) {
	return OpenWithLimits(dataDir, Limits{})
}

// OpenWithLimits is Open with explicit retention limits: the
// per-key task cap enforced in Create and the retention window
// PruneExpired measures from. Zero fields mean the defaults.
func OpenWithLimits(dataDir string, lim Limits) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("a2a: empty data dir")
	}
	path := filepath.Join(dataDir, "a2a", "tasks.db")
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("a2a: create directory: %w", err)
		}
		// MkdirAll leaves a pre-existing directory's mode alone,
		// so tighten it explicitly on every open.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("a2a: tighten directory: %w", err)
		}
	}
	// _txlock=immediate: every transaction (Create, Transition)
	// starts with BEGIN IMMEDIATE, so the writer holds the write
	// lock from the start. A deferred transaction would read a
	// snapshot first and could then fail the upgrade with
	// SQLITE_BUSY_SNAPSHOT despite the busy timeout.
	// Build a canonical file: URI. filepath.ToSlash first: on Windows
	// a backslash path becomes host "C:" plus a %5C-escaped path
	// ("file://C:%5CUsers%5C..."), which SQLite cannot open — PRAGMA
	// journal_mode=WAL then fails with "out of memory (1)". A leading
	// "/" keeps the drive letter in the path ("file:///C:/...") and is
	// a no-op on Unix, where the path already starts with "/".
	slashPath := filepath.ToSlash(path)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	dsn := (&url.URL{Scheme: "file", Path: slashPath, RawQuery: "_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("a2a: open sqlite: %w", err)
	}
	// Single writer connection, same as the ledger: SQLite serialises
	// writers, and one connection avoids "database is locked" churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("a2a: enable WAL: %w", err)
	}
	if _, err := db.Exec(`PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("a2a: busy timeout: %w", err)
	}
	// The schema declares REFERENCES a2a_tasks(id); enforce it.
	// (Per-connection pragma; this connection is the only one, see
	// SetMaxOpenConns above.)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("a2a: enable foreign keys: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("a2a: apply schema: %w", err)
	}
	// SQLite creates the file 0644 & umask (observed 0640); tighten
	// it on every open, including reopening a pre-existing file.
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("a2a: tighten database file: %w", err)
	}
	return &Store{
		db: db, path: path,
		retentionDays:  resolveRetentionDays(lim.RetentionDays),
		maxTasksPerKey: resolveMaxTasksPerKey(lim.MaxTasksPerKey),
	}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Path returns the SQLite file path backing this store.
func (s *Store) Path() string { return s.path }

func newTaskID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("a2a: random task id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows, which
// share the same Scan signature; scanTaskRow is the single body
// behind scanTask and scanTaskRows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(row *sql.Row) (*Task, error) {
	return scanTaskRow(row)
}

func scanTaskRows(rows *sql.Rows) (*Task, error) {
	return scanTaskRow(rows)
}

func scanTaskRow(s rowScanner) (*Task, error) {
	var t Task
	var message, result sql.NullString
	var createdAt, updatedAt string
	err := s.Scan(&t.ID, &t.Owner, &t.State, &message, &result, &t.Error, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	t.Message = json.RawMessage(message.String)
	if result.Valid {
		t.Result = json.RawMessage(result.String)
	}
	t.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("a2a: parse created_at: %w", err)
	}
	t.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return nil, fmt.Errorf("a2a: parse updated_at: %w", err)
	}
	return &t, nil
}

const taskColumns = `id, owner, state, message, result, error, created_at, updated_at`

// Create inserts a new task in state Submitted for owner, writes the
// creation event ("" → submitted), and returns the task.
//
// Per-key cap (M9.8 retention, design §3): an owner holds at most
// MaxTasksPerKey tasks. When the owner is already at the cap, the
// oldest TERMINAL tasks for that owner are evicted first — events
// first, then the task rows, in this same transaction — until there
// is room for the new task. Only when the owner has no evictable
// terminal task (all tasks are submitted/working) is the create
// rejected with ErrTaskLimitExceeded. Cap eviction has no off-switch:
// a 0 limit means the 1,000 default, never unlimited.
func (s *Store) Create(owner string, message json.RawMessage) (*Task, error) {
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	if len(message) == 0 {
		message = json.RawMessage("null")
	}
	id, err := newTaskID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	t := &Task{
		ID: id, Owner: owner, State: StateSubmitted,
		Message: message, CreatedAt: now, UpdatedAt: now,
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("a2a: begin create: %w", err)
	}
	defer tx.Rollback()
	if err := s.enforceCap(tx, owner); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`INSERT INTO a2a_tasks (`+taskColumns+`) VALUES (?, ?, ?, ?, NULL, '', ?, ?)`,
		t.ID, t.Owner, t.State, string(t.Message),
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
	); err != nil {
		return nil, fmt.Errorf("a2a: insert task: %w", err)
	}
	if err := insertEvent(tx, t.ID, "", StateSubmitted, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("a2a: commit create: %w", err)
	}
	return t, nil
}

// ErrTaskLimitExceeded is returned by Create when the owner's task
// count is at the per-key cap and every task is non-terminal, so
// there is nothing evictable to make room. The returned error wraps
// this sentinel and names the cap. The HTTP layer maps it to
// JSON-RPC -32603; the store itself enforces the cap, in Create, so
// no caller path can bypass it.
var ErrTaskLimitExceeded = errors.New("a2a: task limit exceeded")

// enforceCap makes room for one more task for owner inside tx: if
// the owner already holds MaxTasksPerKey tasks, the oldest terminal
// tasks are evicted first (events first, then task rows, same
// transaction). It returns ErrTaskLimitExceeded — with the cap named
// — when the owner is at the cap and no terminal task exists to
// evict; the caller must roll the transaction back. The count,
// evict, and the caller's insert are one BEGIN IMMEDIATE
// transaction, so two racing creates serialise on the write lock
// and the per-owner count can never exceed the cap.
func (s *Store) enforceCap(tx *sql.Tx, owner string) error {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM a2a_tasks WHERE owner = ?`, owner).Scan(&count); err != nil {
		return fmt.Errorf("a2a: count tasks for cap: %w", err)
	}
	if count < s.maxTasksPerKey {
		return nil
	}
	need := count - s.maxTasksPerKey + 1
	evicted, err := s.evictOldestTerminal(tx, owner, need)
	if err != nil {
		return err
	}
	if count-evicted >= s.maxTasksPerKey {
		return fmt.Errorf("%w for this key (%d tasks, all non-terminal)", ErrTaskLimitExceeded, s.maxTasksPerKey)
	}
	return nil
}

// evictOldestTerminal deletes the n oldest terminal tasks for owner
// (oldest updated_at first), events first — a2a_task_events has no
// ON DELETE CASCADE and foreign_keys=ON, so deleting a task row
// while its events exist fails. Returns how many tasks were evicted
// (fewer than n when the owner has fewer terminal tasks).
func (s *Store) evictOldestTerminal(tx *sql.Tx, owner string, n int) (int, error) {
	if n <= 0 {
		return 0, nil
	}
	// LIMIT inside the IN subquery is supported by SQLite.
	sub := `SELECT id FROM a2a_tasks WHERE owner = ? AND state IN ('completed','failed','cancelled') ORDER BY updated_at ASC, id ASC LIMIT ?`
	if _, err := tx.Exec(`DELETE FROM a2a_task_events WHERE task_id IN (`+sub+`)`, owner, n); err != nil {
		return 0, fmt.Errorf("a2a: evict task events: %w", err)
	}
	res, err := tx.Exec(`DELETE FROM a2a_tasks WHERE id IN (`+sub+`)`, owner, n)
	if err != nil {
		return 0, fmt.Errorf("a2a: evict tasks: %w", err)
	}
	evicted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("a2a: evict task count: %w", err)
	}
	return int(evicted), nil
}

// PruneExpired deletes terminal tasks (completed, failed,
// cancelled) whose updated_at is older than the retention window
// — RetentionDays before now — and their events, in one BEGIN
// IMMEDIATE transaction. Non-terminal tasks (submitted, working)
// are NEVER pruned, however old they are: a stuck task is evidence,
// not garbage (design §1). The cutoff is measured from updated_at,
// the time the task BECAME terminal, not created_at. Returns the
// numbers of tasks and events removed. Events are deleted first:
// a2a_task_events has no ON DELETE CASCADE, so deleting a task row
// first fails the foreign-key constraint.
func (s *Store) PruneExpired(now time.Time) (tasks int, events int, err error) {
	return s.PruneExpiredBefore(now.AddDate(0, 0, -s.retentionDays))
}

// PruneExpiredBefore is PruneExpired with an explicit cutoff: it
// deletes terminal tasks with updated_at before cutoff, events
// first, in one transaction, and returns the tasks/events removed.
// The prune loop calls this with a cutoff derived from its own
// retention option; PruneExpired is the same operation with the
// cutoff derived from this store's resolved retention.
func (s *Store) PruneExpiredBefore(cutoff time.Time) (tasks int, events int, err error) {
	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("a2a: begin prune: %w", err)
	}
	defer tx.Rollback()
	terminal := `state IN ('completed','failed','cancelled') AND updated_at < ?`
	evRes, err := tx.Exec(`DELETE FROM a2a_task_events WHERE task_id IN (SELECT id FROM a2a_tasks WHERE `+terminal+`)`, cutoffStr)
	if err != nil {
		return 0, 0, fmt.Errorf("a2a: prune task events: %w", err)
	}
	taskRes, err := tx.Exec(`DELETE FROM a2a_tasks WHERE `+terminal, cutoffStr)
	if err != nil {
		return 0, 0, fmt.Errorf("a2a: prune tasks: %w", err)
	}
	evictedEvents, err := evRes.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("a2a: prune event count: %w", err)
	}
	prunedTasks, err := taskRes.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("a2a: prune task count: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("a2a: commit prune: %w", err)
	}
	return int(prunedTasks), int(evictedEvents), nil
}

// Get returns the task with id owned by owner. A wrong owner — or a
// missing id — returns ErrNotFound, indistinguishable from absence.
func (s *Store) Get(owner, id string) (*Task, error) {
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	t, err := scanTask(s.db.QueryRow(
		`SELECT `+taskColumns+` FROM a2a_tasks WHERE id = ? AND owner = ?`, id, owner))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("a2a: get task: %w", err)
	}
	return t, nil
}

// ListByOwner returns owner's tasks, newest first. There is no
// list-all: every list is scoped to exactly one owner.
func (s *Store) ListByOwner(owner string) ([]*Task, error) {
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	rows, err := s.db.Query(
		`SELECT `+taskColumns+` FROM a2a_tasks WHERE owner = ? ORDER BY created_at DESC, id DESC`, owner)
	if err != nil {
		return nil, fmt.Errorf("a2a: list tasks: %w", err)
	}
	defer rows.Close()
	out := []*Task{}
	for rows.Next() {
		t, err := scanTaskRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("a2a: list tasks: %w", err)
	}
	return out, nil
}

// CountsByState returns the number of tasks in each of the five
// states, across ALL owners, as totals only.
//
// This does NOT violate owner isolation: the result contains no
// task content, no task ids, no owner ids, and no per-owner
// breakdown — only five aggregate numbers keyed by state name, so
// no caller can learn whether any particular owner (or task)
// exists. It is an operator-only aggregate: the only consumer is
// the GET /api/a2a/status endpoint, which is MANAGEMENT-tier and
// admin-scoped (the coordinator registers it with that guard), so
// it is never reachable by a gateway-key caller.
func (s *Store) CountsByState() (map[State]int, error) {
	counts := map[State]int{
		StateSubmitted: 0,
		StateWorking:   0,
		StateCompleted: 0,
		StateFailed:    0,
		StateCancelled: 0,
	}
	rows, err := s.db.Query(`SELECT state, COUNT(*) FROM a2a_tasks GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("a2a: count tasks by state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, fmt.Errorf("a2a: scan task count: %w", err)
		}
		if _, ok := counts[State(state)]; ok {
			counts[State(state)] = n
		}
		// An unknown state string (never written by this package)
		// is ignored: the map's keys are exactly the five states.
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("a2a: count tasks by state: %w", err)
	}
	return counts, nil
}

// Events returns the append-only event log for the task owned by
// owner, in write order. A wrong owner returns ErrNotFound.
func (s *Store) Events(owner, id string) ([]Event, error) {
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	// Owner-scoped existence check first: a wrong owner must not
	// learn whether the task (or its events) exist.
	if _, err := s.Get(owner, id); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(
		`SELECT id, task_id, from_state, to_state, at FROM a2a_task_events WHERE task_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("a2a: list events: %w", err)
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var at string
		if err := rows.Scan(&e.ID, &e.TaskID, &e.FromState, &e.ToState, &at); err != nil {
			return nil, fmt.Errorf("a2a: scan event: %w", err)
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("a2a: parse event time: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("a2a: list events: %w", err)
	}
	return out, nil
}

func insertEvent(tx *sql.Tx, taskID string, from, to State, at time.Time) error {
	if _, err := tx.Exec(
		`INSERT INTO a2a_task_events (task_id, from_state, to_state, at) VALUES (?, ?, ?, ?)`,
		taskID, string(from), string(to), at.Format(time.RFC3339Nano),
	); err != nil {
		return fmt.Errorf("a2a: insert event: %w", err)
	}
	return nil
}

// Transition moves the task owned by owner to state to, validating
// the move against the legal transition graph. The task row update
// and the event-log row are written atomically in one transaction:
// either both land or neither does, so an illegal transition writes
// no event and a failed write leaves no half-moved task.
//
// result is stored only on Completed and errStr only on Failed;
// passing either with any other target is rejected with
// ErrIllegalTransition and nothing is written.
//
// Concurrency: the transaction runs under BEGIN IMMEDIATE (see
// Open), so two racing transitions serialise on the write lock and
// exactly one wins; the loser re-reads the new state and gets
// ErrIllegalTransition. If the write lock cannot be acquired even
// after bounded retries, the typed ErrBusy is returned — never the
// driver's raw "database is locked" string.
func (s *Store) Transition(owner, id string, to State, result json.RawMessage, errStr string) (*Task, error) {
	if owner == "" {
		return nil, ErrEmptyOwner
	}
	if len(result) > 0 && to != StateCompleted {
		return nil, fmt.Errorf("%w: result may only be stored when completing (target %s)", ErrIllegalTransition, to)
	}
	if errStr != "" && to != StateFailed {
		return nil, fmt.Errorf("%w: error may only be stored when failing (target %s)", ErrIllegalTransition, to)
	}
	var err error
	var t *Task
	for attempt := 0; attempt < maxTransitionAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 10 * time.Millisecond)
		}
		t, err = s.transitionOnce(owner, id, to, result, errStr)
		if err == nil {
			return t, nil
		}
		if !isBusyError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: write lock not acquired after %d attempts", ErrBusy, maxTransitionAttempts)
}

// ErrBusy is returned when a transition cannot acquire the SQLite
// write lock after bounded retries. Callers get this typed error
// (or ErrIllegalTransition, when the loser re-reads the winner's
// new state) — never the driver's raw "database is locked" string.
var ErrBusy = errors.New("a2a: database busy")

// maxTransitionAttempts bounds the SQLITE_BUSY retry loop in
// Transition. Each attempt already waits up to the 5 s busy timeout
// inside BEGIN IMMEDIATE, so retries only cover the case where the
// lock is held longer than that.
const maxTransitionAttempts = 5

// SQLite result codes for lock contention: SQLITE_BUSY (5) and its
// extended code SQLITE_BUSY_SNAPSHOT (517), the snapshot error a
// deferred read-then-write transaction could hit.
const (
	sqliteBusy         = 5
	sqliteBusySnapshot = 517
)

// isBusyError reports whether err is SQLite lock contention,
// detected via the driver's typed error code (with a string
// fallback for errors that arrive without the code).
func isBusyError(err error) bool {
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) {
		return sqlErr.Code() == sqliteBusy || sqlErr.Code() == sqliteBusySnapshot
	}
	return strings.Contains(err.Error(), "database is locked")
}

// transitionOnce is a single Transition attempt: one BEGIN IMMEDIATE
// transaction holding the read, the legality check, the row update
// and the event insert.
func (s *Store) transitionOnce(owner, id string, to State, result json.RawMessage, errStr string) (*Task, error) {
	// BEGIN IMMEDIATE via the _txlock=immediate DSN (see Open).
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("a2a: begin transition: %w", err)
	}
	defer tx.Rollback()
	cur, err := scanTask(tx.QueryRow(
		`SELECT `+taskColumns+` FROM a2a_tasks WHERE id = ? AND owner = ?`, id, owner))
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("a2a: load task for transition: %w", err)
	}
	if !IsLegalTransition(cur.State, to) {
		return nil, fmt.Errorf("%w: %s -> %s", ErrIllegalTransition, cur.State, to)
	}
	now := time.Now().UTC()
	var resultArg any
	if result != nil {
		resultArg = string(result)
	}
	if _, err := tx.Exec(
		`UPDATE a2a_tasks SET state = ?, result = ?, error = ?, updated_at = ? WHERE id = ? AND owner = ?`,
		string(to), resultArg, errStr, now.Format(time.RFC3339Nano), id, owner,
	); err != nil {
		return nil, fmt.Errorf("a2a: update task: %w", err)
	}
	if err := insertEvent(tx, id, cur.State, to, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("a2a: commit transition: %w", err)
	}
	cur.State = to
	cur.Result = result
	cur.Error = errStr
	cur.UpdatedAt = now
	return cur, nil
}

// Cancel moves the task owned by owner to Cancelled. Legal only
// from Submitted or Working (terminal tasks cannot be cancelled);
// a wrong owner returns ErrNotFound.
func (s *Store) Cancel(owner, id string) (*Task, error) {
	return s.Transition(owner, id, StateCancelled, nil, "")
}
