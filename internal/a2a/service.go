package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Processor is the seam the HTTP layer fills in: given a task's
// owner and raw message, it performs the real work (the HTTP
// builder's processor calls the real router) and returns the raw
// JSON result. There is deliberately NO canned, echo, or fallback
// processor in this package: if the processor returns an error the
// task is Failed with that real error — never a fabricated success.
type Processor func(ctx context.Context, owner string, message json.RawMessage) (json.RawMessage, error)

// Service is the owner-scoped A2A task service. Every method takes
// the caller's owner id (OwnerIDForKey of their gateway key id);
// there is no ownerless or list-all path anywhere in this package.
type Service struct {
	store *Store
}

// NewService returns a Service backed by store. store must be
// non-nil (open it first with Open); a nil store is rejected here
// with an error instead of panicking on first use. The error return
// (rather than a panic) was chosen because every caller is inside
// this package and can check it at construction time.
func NewService(store *Store) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("a2a: nil store")
	}
	return &Service{store: store}, nil
}

// Store returns the store backing this service. The status
// handler (GET /api/a2a/status) needs it for its operator-only
// per-state counts; returning the long-lived store creates no
// second store and changes no behaviour.
func (s *Service) Store() *Store { return s.store }

// CreateTask creates a task in state Submitted for owner.
func (s *Service) CreateTask(owner string, message json.RawMessage) (*Task, error) {
	return s.store.Create(owner, message)
}

// GetTask returns the task with id owned by owner, or ErrNotFound
// (including when the id exists under a different owner).
func (s *Service) GetTask(owner, id string) (*Task, error) {
	return s.store.Get(owner, id)
}

// ListTasks returns owner's tasks, newest first. No other owner's
// tasks are ever visible through this method.
func (s *Service) ListTasks(owner string) ([]*Task, error) {
	return s.store.ListByOwner(owner)
}

// CancelTask moves the task owned by owner to Cancelled. Legal only
// from Submitted or Working; terminal tasks return
// ErrIllegalTransition and a wrong owner returns ErrNotFound.
func (s *Service) CancelTask(owner, id string) (*Task, error) {
	return s.store.Cancel(owner, id)
}

// RunTask executes the task owned by owner with processor:
//
//	Submitted → Working → processor() → Completed (result stored)
//	                                      → Failed (real error stored)
//
// The task must be Submitted when RunTask starts, otherwise
// ErrIllegalTransition is returned and nothing runs. If the task
// is cancelled while the processor runs, the final transition
// fails (Cancelled is terminal) and the CURRENT task is returned
// with a nil error instead — cancellation wins and the
// processor's result is not stored, and the caller must not turn
// the lost transition into a second failure. The same applies
// when the task is cancelled in the window between the Working
// transition and the processor call: the state is re-checked
// after the transition and the processor is skipped entirely
// when the task is no longer Working, so a cancelled task can
// never run its processor after cancellation landed.
// A processor error is never turned into a fake success: the task
// is Failed and Task.Error holds the processor's real error string.
// RunTask returns the terminal task with a nil error in both the
// success and the processor-failure cases; tell them apart by
// inspecting Task.State (Failed + Task.Error on failure).
func (s *Service) RunTask(ctx context.Context, owner, id string, processor Processor) (*Task, error) {
	if processor == nil {
		return nil, fmt.Errorf("a2a: nil processor")
	}
	if _, err := s.store.Transition(owner, id, StateWorking, nil, ""); err != nil {
		return nil, err
	}
	task, err := s.store.Get(owner, id)
	if err != nil {
		return nil, err
	}
	// TOCTOU re-check: CancelTask may have landed between the
	// Working transition above and this read. A task that is no
	// longer Working must not run its processor — return the
	// task as it stands (Cancelled) with no error and no work.
	if task.State != StateWorking {
		return task, nil
	}
	result, err := processor(ctx, owner, task.Message)
	if err != nil {
		failed, terr := s.store.Transition(owner, id, StateFailed, nil, err.Error())
		if terr != nil {
			// The only way the Failed transition loses is a
			// concurrent terminal transition — in practice a
			// cancel that landed while the processor ran.
			// Cancellation wins: return the task as it
			// stands (Cancelled) instead of reporting the
			// lost transition as a second failure.
			if errors.Is(terr, ErrIllegalTransition) {
				if cur, gerr := s.store.Get(owner, id); gerr == nil {
					return cur, nil
				}
			}
			return nil, fmt.Errorf("a2a: mark failed: %w (processor error: %v)", terr, err)
		}
		return failed, nil
	}
	done, err := s.store.Transition(owner, id, StateCompleted, result, "")
	if err != nil {
		// As above: a concurrent cancel wins over the
		// completion — return the task as it stands.
		if errors.Is(err, ErrIllegalTransition) {
			if cur, gerr := s.store.Get(owner, id); gerr == nil {
				return cur, nil
			}
		}
		return nil, fmt.Errorf("a2a: mark completed: %w", err)
	}
	return done, nil
}
