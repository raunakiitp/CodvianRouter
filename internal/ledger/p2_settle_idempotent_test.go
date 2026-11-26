package ledger

// WS6 closure item 4a: one-settle-per-request billing is pinned at
// the storage seam. A reservation is a committed-spend row; settle
// deletes it. The settle itself MUST be idempotent — a retry, a
// re-opened stream, or a double defer must never double-charge or
// error — and a settled id must stop counting toward the key's
// reserved spend immediately.

import (
	"context"
	"testing"
	"time"
)

func TestSettleReservationIsIdempotent(t *testing.T) {
	l := openTestLedger(t)
	ctx := context.Background()
	now := time.Now()
	if err := l.SetKeyBudget(ctx, "key-a", 100.00, UnpricedFailClosed); err != nil {
		t.Fatalf("SetKeyBudget: %v", err)
	}

	id, err := l.ReserveSpend(ctx, "key-a", []string{"m"}, func(string) bool { return true }, 5.00, now)
	if err != nil || id == "" {
		t.Fatalf("ReserveSpend: id=%q err=%v", id, err)
	}

	reserved, err := l.ReservedSpend(ctx, "key-a", now)
	if err != nil {
		t.Fatalf("ReservedSpend after reserve: %v", err)
	}
	if reserved <= 0 {
		t.Fatalf("reserved spend = %v, want the $5.00 commitment counted", reserved)
	}

	// Settle TWICE: the second must be a clean no-op — no error,
	// no double effect. A delete is naturally idempotent; pin it
	// so nobody "fixes" it into upserting or charging again.
	if err := l.SettleReservation(ctx, id); err != nil {
		t.Fatalf("first settle: %v", err)
	}
	if err := l.SettleReservation(ctx, id); err != nil {
		t.Fatalf("second settle must be a no-op, got: %v", err)
	}

	reserved, err = l.ReservedSpend(ctx, "key-a", now)
	if err != nil {
		t.Fatalf("ReservedSpend after settle: %v", err)
	}
	if reserved != 0 {
		t.Fatalf("reserved spend = %v after settle, want 0 — the hold is released exactly once", reserved)
	}
}

func TestSettleReservationEmptyAndUnknownHarmless(t *testing.T) {
	l := openTestLedger(t)
	ctx := context.Background()
	if err := l.SettleReservation(ctx, ""); err != nil {
		t.Fatalf("empty id settle: %v", err)
	}
	if err := l.SettleReservation(ctx, "no-such-reservation"); err != nil {
		t.Fatalf("unknown id settle must be a no-op, got: %v", err)
	}
}
