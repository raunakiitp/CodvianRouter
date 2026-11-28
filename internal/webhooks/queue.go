package webhooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Delivery statuses recorded in the queue/history.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// Delivery is one persisted delivery record: the payload snapshot plus
// its status, last HTTP status, latency and the last error. It is the
// unit the durable queue persists and the delivery history exposes.
type Delivery struct {
	ID         string    `json:"id"`
	EndpointID string    `json:"endpoint_id"`
	Event      string    `json:"event"`
	URL        string    `json:"url"`
	Body       []byte    `json:"body"` // payload snapshot
	Attempts   int       `json:"attempts"`
	Status     string    `json:"status"` // pending|delivered|failed
	HTTPStatus int       `json:"http_status,omitempty"`
	LatencyMs  int64     `json:"latency_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Queue is a durable delivery queue + delivery history. A non-empty path
// persists both to that JSON file after every mutation (atomic temp file
// + rename), so a failed delivery survives a process restart and can be
// redelivered. A "" path is in-memory (tests, config-less servers).
type Queue struct {
	mu      sync.Mutex
	path    string
	pending map[string]*Delivery
	order   []string // pending insertion order
	history []Delivery
	seq     uint64
}

// maxDeliveryHistory bounds the retained history (oldest dropped first).
const maxDeliveryHistory = 500

// OpenQueue opens (or creates) a durable queue at path. An existing
// file's pending items and history are loaded; "" is in-memory.
func OpenQueue(path string) (*Queue, error) {
	q := &Queue{path: path, pending: map[string]*Delivery{}}
	if path == "" {
		return q, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return q, nil
		}
		return nil, fmt.Errorf("webhooks: open queue %s: %w", path, err)
	}
	var snap struct {
		Pending map[string]*Delivery `json:"pending"`
		Order   []string             `json:"order"`
		History []Delivery           `json:"history"`
		Seq     uint64               `json:"seq"`
	}
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, fmt.Errorf("webhooks: decode queue %s: %w", path, err)
	}
	if snap.Pending != nil {
		q.pending = snap.Pending
	}
	q.order = snap.Order
	q.history = snap.History
	q.seq = snap.Seq
	return q, nil
}

// persistLocked writes the queue snapshot atomically. The caller holds q.mu.
func (q *Queue) persistLocked() error {
	if q.path == "" {
		return nil
	}
	snap := struct {
		Pending map[string]*Delivery `json:"pending"`
		Order   []string             `json:"order"`
		History []Delivery           `json:"history"`
		Seq     uint64               `json:"seq"`
	}{Pending: q.pending, Order: q.order, History: q.history, Seq: q.seq}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(q.path), 0o755); err != nil {
		return err
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, q.path)
}

// Enqueue records a delivery as pending and persists it. The delivered
// d.ID/d.CreatedAt are filled in.
func (q *Queue) Enqueue(d *Delivery) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.seq++
	d.ID = fmt.Sprintf("dl-%d", q.seq)
	d.Status = DeliveryPending
	d.CreatedAt = time.Now().UTC()
	d.UpdatedAt = d.CreatedAt
	cp := *d
	q.pending[d.ID] = &cp
	q.order = append(q.order, d.ID)
	return q.persistLocked()
}

// Pending returns the deliveries still awaiting successful delivery
// (both never-attempted and previously-failed), in insertion order.
func (q *Queue) Pending() []Delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Delivery, 0, len(q.order))
	for _, id := range q.order {
		if d, ok := q.pending[id]; ok {
			out = append(out, *d)
		}
	}
	return out
}

// Complete records the outcome of one delivery attempt sequence for id.
// A 2xx (or a nil err with a 2xx status) marks the delivery delivered and
// removes it from the pending set; any other outcome records a failure
// and, when retryable, keeps the delivery pending for a later redelivery
// (across a restart). A terminal (non-retryable) failure is recorded in
// history and removed from pending — redelivering the same body would
// fail identically.
func (q *Queue) Complete(id string, httpStatus int, latencyMs int64, retryable bool, derr error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	d, ok := q.pending[id]
	if !ok {
		return nil
	}
	d.Attempts++
	d.HTTPStatus = httpStatus
	d.LatencyMs = latencyMs
	d.UpdatedAt = time.Now().UTC()
	success := derr == nil && httpStatus >= 200 && httpStatus < 300
	if success {
		d.Status = DeliveryDelivered
	} else {
		d.Status = DeliveryFailed
		if derr != nil {
			d.Error = derr.Error()
		} else {
			d.Error = fmt.Sprintf("http %d", httpStatus)
		}
	}
	rec := *d
	q.history = append(q.history, rec)
	if len(q.history) > maxDeliveryHistory {
		q.history = q.history[len(q.history)-maxDeliveryHistory:]
	}
	if success || !retryable {
		// Terminal outcome: stop retrying this delivery.
		delete(q.pending, id)
		q.order = removeString(q.order, id)
	}
	return q.persistLocked()
}

// History returns delivery records newest-first, bounded by limit (>0).
func (q *Queue) History(limit int) []Delivery {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Delivery, 0, len(q.history))
	for i := len(q.history) - 1; i >= 0; i-- {
		out = append(out, q.history[i])
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func removeString(in []string, s string) []string {
	out := in[:0]
	for _, v := range in {
		if v != s {
			out = append(out, v)
		}
	}
	return out
}
