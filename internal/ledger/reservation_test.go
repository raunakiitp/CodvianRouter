package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// M5.8: committed-vs-settled spend — reservations close the
// parallel-request race in CheckKeyBudget.
func TestReserveSpend(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// $1.00 5h cap, no calendar limits.
	if err := l.SetKeyBudget(ctx, "k1", 1.00, UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	priced := func(string) bool { return true }
	id, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.60, now)
	if err != nil || id == "" {
		t.Fatalf("reserve $0.60 of $1.00: id=%q err=%v", id, err)
	}
	// Committed spend is visible immediately.
	reserved, _ := l.ReservedSpend(ctx, "k1", now)
	if reserved != 0.60 {
		t.Errorf("reserved = %v, want 0.60", reserved)
	}
	// The plain check now sees the committed $0.60.
	if err := l.CheckKeyBudget(ctx, "k1", nil, priced, now); err != nil {
		t.Errorf("check after reserve: %v, want nil (0.60 < 1.00)", err)
	}
	// A second $0.60 estimate does NOT fit (0.60 + 0.60 > 1.00).
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.60, now); !errors.As(err, new(*BudgetExceededError)) {
		t.Errorf("second reserve $0.60: err=%v, want *BudgetExceededError", err)
	}
	// Settle the first: the headroom returns.
	if err := l.SettleReservation(ctx, id); err != nil {
		t.Fatal(err)
	}
	if reserved, _ := l.ReservedSpend(ctx, "k1", now); reserved != 0 {
		t.Errorf("reserved after settle = %v, want 0", reserved)
	}
	id2, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.60, now)
	if err != nil {
		t.Fatalf("reserve after settle: %v", err)
	}
	// Settling twice is a no-op.
	if err := l.SettleReservation(ctx, id); err != nil {
		t.Errorf("double settle: %v, want nil", err)
	}
	// Settled spend + reservations combine: settle the $0.60,
	// record $0.50 settled, then a $0.60 estimate must fail
	// (0.50 + 0.60 > 1.00) while $0.40 fits (0.90 < 1.00).
	if err := l.SettleReservation(ctx, id2); err != nil {
		t.Fatal(err)
	}
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "p", "m", 0.50, 1, 1, 1, 0, 100)
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.60, now); !errors.As(err, new(*BudgetExceededError)) {
		t.Errorf("reserve $0.60 over $0.50 settled: err=%v, want *BudgetExceededError", err)
	}
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.40, now); err != nil {
		t.Errorf("reserve $0.40 over $0.50 settled: %v, want nil (0.90 < 1.00)", err)
	}
}

func TestReserveSpendParallel(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := l.SetKeyBudget(ctx, "k1", 1.00, UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	priced := func(string) bool { return true }
	// 10 parallel reserves of $0.30 against a $1.00 cap: at
	// most 3 can fit (3 × 0.30 = 0.90; a 4th would reach 1.20).
	// Without the reservation this race admits all 10.
	var wg sync.WaitGroup
	ok := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.30, now)
			ok <- err == nil
		}()
	}
	wg.Wait()
	close(ok)
	admitted := 0
	for good := range ok {
		if good {
			admitted++
		}
	}
	if admitted != 3 {
		t.Errorf("parallel reserves admitted = %d, want exactly 3", admitted)
	}
	reserved, _ := l.ReservedSpend(ctx, "k1", now)
	if reserved < 0.8999 || reserved > 0.9001 {
		t.Errorf("reserved after parallel race = %v, want 0.90", reserved)
	}
}

func TestReserveSpendExpiry(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	t0 := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := l.SetKeyBudget(ctx, "k1", 1.00, UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	priced := func(string) bool { return true }
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.90, t0); err != nil {
		t.Fatal(err)
	}
	// Just before expiry the reservation still counts.
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.20, t0.Add(SpendReservationTTL-time.Second)); !errors.As(err, new(*BudgetExceededError)) {
		t.Errorf("reserve before expiry: err=%v, want *BudgetExceededError", err)
	}
	// After expiry it is gone (pruned lazily on read).
	later := t0.Add(SpendReservationTTL + time.Minute)
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.90, later); err != nil {
		t.Errorf("reserve after expiry: %v, want nil", err)
	}
	if reserved, _ := l.ReservedSpend(ctx, "k1", later); reserved != 0.90 {
		t.Errorf("reserved after expiry + new reserve = %v, want 0.90 (old row pruned)", reserved)
	}
}

func TestReserveSpendUnpricedAndNoRow(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// No budget row: uncapped, no-op id.
	id, err := l.ReserveSpend(ctx, "ghost", []string{"m"}, nil, 5.00, now)
	if err != nil || id != "" {
		t.Errorf("reserve with no row: id=%q err=%v, want \"\"/nil", id, err)
	}
	// Unpriced model under fail_closed: the reserve refuses
	// with the same error CheckKeyBudget returns.
	if err := l.SetKeyBudget(ctx, "k1", 10.00, UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	unpriced := func(m string) bool { return m != "no-price" }
	if _, err := l.ReserveSpend(ctx, "k1", []string{"no-price"}, unpriced, 0.10, now); !errors.As(err, new(*UnpricedUsageError)) {
		t.Errorf("reserve unpriced model: err=%v, want *UnpricedUsageError", err)
	}
	// Negative estimate: a caller bug, refused loudly.
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, unpriced, -1, now); err == nil {
		t.Error("negative estimate: want an error")
	}
}

// TestReserveSpendParallelCumulativeCap (M5.14): the cumulative
// (legacy) per-key cap gets the same reservation protection as the
// rolling key budget. 10 parallel reserves of $0.30 against a
// $1.00 cumulative-only cap (no key_budgets row at all): exactly 3
// admitted, the rest refused with *BudgetExceededError. Before
// M5.14 ReserveSpend returned "" with no key-budget row, so all 10
// passed the pre-send check and jointly spent $3.00.
func TestReserveSpendParallelCumulativeCap(t *testing.T) {
	l := openKeyBudgetLedger(t)
	l.FailClosed = true
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	if err := l.SetBudget(ctx, "k1", 1.00, "daily"); err != nil {
		t.Fatal(err)
	}
	priced := func(string) bool { return true }
	var wg sync.WaitGroup
	ok := make(chan bool, 10)
	refusedBudget := make(chan bool, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.30, now)
			ok <- err == nil
			refusedBudget <- errors.As(err, new(*BudgetExceededError))
		}()
	}
	wg.Wait()
	close(ok)
	close(refusedBudget)
	admitted, refused := 0, 0
	for good := range ok {
		if good {
			admitted++
		}
	}
	for b := range refusedBudget {
		if b {
			refused++
		}
	}
	if admitted != 3 {
		t.Errorf("parallel cumulative reserves admitted = %d, want exactly 3", admitted)
	}
	if refused != 7 {
		t.Errorf("parallel cumulative reserves refused with *BudgetExceededError = %d, want 7", refused)
	}
	reserved, _ := l.ReservedSpend(ctx, "k1", now)
	if reserved < 0.8999 || reserved > 0.9001 {
		t.Errorf("reserved after parallel race = %v, want 0.90", reserved)
	}
}

// TestCheckBudgetForEstimateSeesReservations (M5.14): the
// cumulative pre-send check counts outstanding reservations as
// committed spend — a $0.60 reservation against a $1.00 cap plus a
// $0.50 estimate refuses, where the settled-only read passed it.
func TestCheckBudgetForEstimateSeesReservations(t *testing.T) {
	l := openKeyBudgetLedger(t)
	l.FailClosed = true
	ctx := context.Background()
	// Real clock, not a fixed date: checkBudgetDetailed prunes
	// reservations against wall-clock now (same as
	// CheckBudgetForEstimate's key-budget status read), so a
	// reservation stamped hours in the past would already be
	// expired by its 10-minute TTL.
	now := time.Now()
	if err := l.SetBudget(ctx, "k1", 1.00, "daily"); err != nil {
		t.Fatal(err)
	}
	priced := func(string) bool { return true }
	if _, err := l.ReserveSpend(ctx, "k1", []string{"m"}, priced, 0.60, now); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, _, err := l.CheckBudgetForEstimate(ctx, "k1", 0.50); !errors.As(err, new(*BudgetExceededError)) {
		t.Errorf("check with $0.60 reserved + $0.50 estimate: err=%v, want *BudgetExceededError", err)
	}
	if _, _, err := l.CheckBudgetForEstimate(ctx, "k1", 0.40); err != nil {
		t.Errorf("check with $0.60 reserved + $0.40 estimate: err=%v, want nil (0.60+0.40 = 1.00 fits)", err)
	}
}
