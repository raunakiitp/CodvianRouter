// Package a2a implements the Agent-to-Agent (A2A) task core for the
// Codvian Router gateway (M9.8).
//
// Shape reference only: the reference router's A2A surface (a2a/route.ts) — the
// JSON-RPC method names (message/send, message/stream, tasks/get,
// tasks/cancel) and the task/event shapes (submitted → working →
// completed | failed | cancelled, persisted in SQLite a2a_tasks +
// a2a_task_events). No the reference router code is copied, and two of its
// behaviours are deliberately NOT reproduced:
//
//   - the reference router's A2A is keyless by default (GHSA-v54m-6rm3-p565).
//     Ours is off unless the operator enables it
//     (config.A2AEnabled, default false) and every call is
//     owner-scoped to a gateway key.
//   - the reference router's owner is a SHA-256 *prefix* of the API key and
//     ownerless tasks are visible to everyone by design
//     (GHSA-jcm5-6wpp-wjj8). Ours uses the FULL SHA-256 hex of the
//     gateway key id (OwnerIDForKey) and there is NO ownerless
//     access: every read/write takes an owner id, there is no
//     list-all, and a wrong owner sees exactly "not found" —
//     never a hint that the task exists.
//
// This package is transport-free: it defines the task model, the
// SQLite store, and the Service/Processor seam. The HTTP layer
// (internal/server/a2a.go, built by a second builder) supplies a
// Processor that calls the real router; this package contains no
// canned/echo processor and never fabricates a completion.
package a2a

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// State is an A2A task lifecycle state. Exactly five exist; no other
// state string is ever stored.
type State string

// Task states. Legal transitions only:
//
//	Submitted → Working → Completed | Failed | Cancelled
//	Working   → Cancelled
//
// Anything else is an error (see ErrIllegalTransition).
const (
	StateSubmitted State = "submitted"
	StateWorking   State = "working"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// legalTransitions maps each state to the states it may move to.
var legalTransitions = map[State][]State{
	StateSubmitted: {StateWorking, StateCancelled},
	StateWorking:   {StateCompleted, StateFailed, StateCancelled},
	// Completed, Failed, Cancelled are terminal: no outgoing edges.
}

// IsLegalTransition reports whether from → to is a legal state move.
func IsLegalTransition(from, to State) bool {
	for _, next := range legalTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// ErrNotFound is returned when a task does not exist *for the given
// owner*. A wrong owner gets exactly this error — never a
// "forbidden, it exists" signal — so owner isolation is
// indistinguishable from absence.
var ErrNotFound = errors.New("a2a: task not found")

// ErrIllegalTransition is returned when a requested state move is
// not in the legal transition graph.
var ErrIllegalTransition = errors.New("a2a: illegal state transition")

// ErrEmptyOwner is returned when a caller passes an empty owner id.
// Empty owners are never accepted: there is no ownerless access.
var ErrEmptyOwner = errors.New("a2a: empty owner")

// Task is one A2A agent task. Message is the caller-supplied
// request payload (raw JSON, as received); Result is the processor's
// raw JSON reply on success; Error is the processor's real error
// string on failure (never a fabricated result).
type Task struct {
	ID        string          `json:"id"`
	Owner     string          `json:"owner"`
	State     State           `json:"state"`
	Message   json.RawMessage `json:"message"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Event is one row of the append-only a2a_task_events log. Every
// state transition — including creation (FromState "") — writes
// exactly one event, in order. Events are never updated or deleted.
type Event struct {
	ID        int64     `json:"id"`
	TaskID    string    `json:"task_id"`
	FromState State     `json:"from_state"`
	ToState   State     `json:"to_state"`
	At        time.Time `json:"at"`
}

// OwnerIDForKey returns the owner id for a gateway key id: the FULL
// SHA-256 hex of the key id (64 hex chars). Full hex, not a prefix:
// the reference router's prefix is strictly weaker and still deterministic,
// and the full hash costs nothing extra. The raw key and the raw
// key id are never stored or logged alongside a task — only this
// hash. An empty key id yields "" so callers must reject it
// (see ErrEmptyOwner); an empty owner must never reach the store.
func OwnerIDForKey(keyID string) string {
	if keyID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(keyID))
	return hex.EncodeToString(sum[:])
}
