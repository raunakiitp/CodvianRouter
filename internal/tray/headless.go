//go:build !tray

package tray

import (
	"context"
	"fmt"
)

// RunHeadless starts the supervisor if it is not already running and then
// blocks until ctx ends or the child dies unexpectedly.
//
//   - ctx ends: the child is stopped and ctx.Err() is returned.
//   - child dies unexpectedly (retries exhausted): an error naming the
//     exit is returned.
//   - the supervisor is stopped cleanly from elsewhere: nil is returned.
//
// RunHeadless never shows, logs, or transmits any secret; it only reports
// the gateway's lifecycle state.
func RunHeadless(ctx context.Context, sup *Supervisor) error {
	if sup.Status() != "running" {
		if err := sup.Start(ctx); err != nil {
			return err
		}
	}
	doneCh := sup.doneChan()
	if doneCh == nil {
		// Never started (should not happen after a successful Start).
		return nil
	}
	select {
	case <-ctx.Done():
		_ = sup.Stop()
		return ctx.Err()
	case <-doneCh:
		if err := sup.LastError(); err != nil {
			return fmt.Errorf("tray: gateway exited unexpectedly: %w", err)
		}
		return nil
	}
}
