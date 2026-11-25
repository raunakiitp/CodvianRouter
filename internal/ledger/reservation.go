package ledger

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// M5.8: committed-vs-settled spend (the reference router keyQuota.ts
// pre-request gate + post-response increment, with the race the
// ledger-IS-the-counter rule leaves open).
//
// The hole: CheckKeyBudget reads settled spend, then the request
// proceeds — N parallel requests can all pass before any ledger
// row lands, and the key overspends by N× the estimate. The fix
// is a reservation: before sending, the estimated cost is
// RESERVED (committed); the gate counts committed + settled, so
// the (N+1)th parallel request sees the earlier reservations
// and is refused. When the response lands, the actual cost is
// recorded as a usage row and the reservation is SETTLED
// (deleted) — the caller must record first, then settle, so
// there is never a moment where neither the reservation nor the
// row counts.
//
// Reservations expire: a request's lifetime is bounded by the
// engine timeout (120s) plus stream lifetime, so a 10-minute TTL
// covers every legitimate request with margin, and a crashed or
// abandoned request stops counting against the key after at most
// 10 minutes. Expired rows are pruned lazily on read (and never
// counted). One server process owns the ledger file (local-first
// single binary), so a process-local mutex serialises the
// check+insert — cross-process sharing of one ledger file is not
// a supported deployment.

// SpendReservationTTL bounds how long a committed reservation
// counts against a key's budget.
const SpendReservationTTL = 10 * time.Minute

// ReserveSpend commits an estimated cost against one key's
// budget: it runs the same enforcement as CheckKeyBudget (the
// rolling 5h cap + every set daily/weekly/monthly window, each
// against its own period's spend, + the closed-period rule per
// set window + the unpriced policy), plus the legacy cumulative
// per-key cap (M5.14), which CheckKeyBudget does not enforce.
// The cumulative check counts settled spend plus all
// unexpired reservations for the key plus this estimate, and
// it only bites when FailClosed is set — with FailClosed off,
// an over-cap estimate still inserts a reservation. Only when
// the estimate fits the enforced caps does it insert the
// reservation and return its id. The check and
// the insert are serialised by reserveMu, so two parallel
// reserves cannot both pass on the same headroom. An estimate
// of 0 still inserts a (zero-cost) reservation: the id is the
// caller's settle handle, and the row is harmless. A key with
// no budget row is uncapped: the reservation is a no-op and the
// returned id is "" (SettleReservation("") is a no-op too). The
// caller settles once the request is done — after the actual
// usage row has been recorded.
func (l *Ledger) ReserveSpend(ctx context.Context, keyID string, models []string, priced func(string) bool, estimateUSD float64, now time.Time) (string, error) {
	if keyID == "" {
		return "", errors.New("ledger: reserve spend requires a key id")
	}
	if estimateUSD < 0 {
		return "", fmt.Errorf("ledger: reserve spend estimate must not be negative (got %v)", estimateUSD)
	}
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	b, err := l.GetKeyBudget(ctx, keyID)
	hasKeyBudget := true
	if err != nil {
		if err == sql.ErrNoRows {
			hasKeyBudget = false
		} else {
			return "", fmt.Errorf("ledger: reserve spend budget: %w", err)
		}
	}
	// M5.14: the reservation also guards the cumulative (legacy)
	// per-key cap, not only the rolling key budget — a key with
	// ONLY a cumulative cap used to get no reservation at all, so
	// N parallel requests could all pass its pre-send check
	// (which reads settled spend) and jointly exceed it. The
	// cumulative check and the insert below run under the same
	// reserveMu hold, so the reservation is the atomic backstop
	// for both caps; the server's pre-send check is advisory.
	cum, cumErr := l.GetBudget(ctx, keyID)
	hasCum := false
	if cumErr == nil && cum.CapUSD > 0 {
		hasCum = true
	} else if cumErr != nil && cumErr != sql.ErrNoRows {
		return "", fmt.Errorf("ledger: reserve spend cumulative budget: %w", cumErr)
	}
	if !hasKeyBudget && !hasCum {
		return "", nil
	}
	if hasKeyBudget {
		// The status already counts settled + reserved spend, so
		// the estimate is the only extra. The check and the insert
		// below run under the same reserveMu hold — no other
		// reserve or check can interleave between them.
		st, err := l.keyBudgetStatusWithBudget(ctx, keyID, now, b, true)
		if err != nil {
			return "", err
		}
		if err := enforceKeyBudgetCaps(st, b, estimateUSD); err != nil {
			return "", err
		}
		if err := enforceKeyBudgetUnpriced(ctx, l, keyID, models, priced, now, b, st); err != nil {
			return "", err
		}
	}
	if hasCum {
		reserved, err := l.reservedSpendLocked(ctx, keyID, now)
		if err != nil {
			return "", err
		}
		committed := cum.SpentUSD + reserved
		if l.FailClosed && committed+estimateUSD > cum.CapUSD {
			return "", &BudgetExceededError{
				KeyID:        keyID,
				CapUSD:       cum.CapUSD,
				SpentUSD:     committed,
				RemainingUSD: cum.CapUSD - committed,
			}
		}
	}
	id := newReservationID()
	if _, err := l.db.ExecContext(ctx, `
		INSERT INTO spend_reservations (id, gateway_key_id, estimated_cost_usd, created_ms, expires_ms)
		VALUES (?, ?, ?, ?, ?)`,
		id, keyID, estimateUSD, now.UnixMilli(), now.Add(SpendReservationTTL).UnixMilli()); err != nil {
		return "", fmt.Errorf("ledger: reserve spend insert: %w", err)
	}
	return id, nil
}

// SettleReservation releases one committed reservation. It is
// idempotent (settling twice, or settling an expired/missing
// id, is a no-op) and must be called AFTER the actual usage row
// has been recorded — settling first would open a window where
// neither the reservation nor the row counts.
func (l *Ledger) SettleReservation(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	if _, err := l.db.ExecContext(ctx, `DELETE FROM spend_reservations WHERE id = ?`, id); err != nil {
		return fmt.Errorf("ledger: settle reservation: %w", err)
	}
	return nil
}

// ReservedSpend returns one key's currently committed (reserved
// but unsettled) spend: unexpired reservations only. Expired
// rows are pruned as they are found.
func (l *Ledger) ReservedSpend(ctx context.Context, keyID string, now time.Time) (float64, error) {
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	return l.reservedSpendLocked(ctx, keyID, now)
}

// reservedSpendLocked sums one key's unexpired reservations and
// prunes the expired ones. The caller holds reserveMu (or is a
// reporting read, for which the prune is idempotent and the sum
// a point-in-time view).
func (l *Ledger) reservedSpendLocked(ctx context.Context, keyID string, now time.Time) (float64, error) {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM spend_reservations WHERE gateway_key_id = ? AND expires_ms <= ?`, keyID, now.UnixMilli()); err != nil {
		return 0, fmt.Errorf("ledger: prune expired reservations: %w", err)
	}
	var total float64
	if err := l.rdb.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(estimated_cost_usd), 0) FROM spend_reservations
		WHERE gateway_key_id = ? AND expires_ms > ?`,
		keyID, now.UnixMilli()).Scan(&total); err != nil {
		return 0, fmt.Errorf("ledger: reserved spend: %w", err)
	}
	return total, nil
}

func newReservationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand essentially never fails; a time-based
		// fallback keeps the reservation usable instead of
		// failing the request for an id.
		return fmt.Sprintf("res_%d", time.Now().UnixNano())
	}
	return "res_" + hex.EncodeToString(b[:])
}
