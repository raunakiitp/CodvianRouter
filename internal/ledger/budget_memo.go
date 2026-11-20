// Per-request global-budget memo (CHECKLIST 299-300).
//
// The global-budget read (GetBudget, behind CheckBudgetOrError /
// CheckBudgetForEstimate) is the biggest named router cost in the
// 279-280 fresh profile: CheckBudgetOrError at 8.20% cum, one
// SQLite read per request. This memo is the same shape as the
// per-request key-budget memo (CHECKLIST 279-280): one memo per
// HTTP request, attached in withLogging, only successful reads
// memoised (a found row, or its absence as sql.ErrNoRows), real
// database errors never cached, same staleness bound of one
// request, same invalidation discipline on writes.
//
// Honest scope note, measured before this slice landed: on the
// current hot paths the global-budget guard runs exactly once per
// request (enforcePromptBudget, called once per chat / responses /
// embeddings request, makes exactly one GetBudget read — one of
// CheckBudgetOrError or CheckBudgetForEstimate, never both; the
// pre-body admission checks only the key budget). The key-budget
// memo saved a second read because that guard ran twice; this
// memo has no second read to save on today's paths, so do NOT
// expect the profile share to fall the way GetKeyBudget's did
// (11.54% -> 7.52%). The memo is still the specified dedup
// mechanism — any request that hits the guard twice (a future
// pre-body global-budget admission check, a retry inside one
// request, a dashboard handler reading the row twice) shares one
// read — and the before/after profile in
// docs/cpu-profile-2026-11-18-globalbudget.md records what it
// actually saved: nothing beyond noise on the chat path, or the
// number if a second read appears. A memo hit is a map lookup
// (~90 ns, same order as the key-budget memo hit); the remaining
// cost is the one SQLite read per request, which only a
// cross-request cache or a combined budget read could remove —
// both change the staleness bound and are out of scope here.
package ledger

import (
	"context"
	"database/sql"
	"errors"
	"sync"
)

// budgetMemoKey is the context key for the per-request
// global-budget memo (CHECKLIST 299-300).
type budgetMemoKey struct{}

// budgetMemoEntry is one memoised global-budget read: a row that
// was found, or the absence of one (found=false, i.e. the read
// returned sql.ErrNoRows). Real database errors are never cached:
// the next read retries the database instead of memoising a
// failure.
type budgetMemoEntry struct {
	b     Budget
	found bool
}

// budgetMemo is the per-request cache for GetBudget reads. The
// memo lives only as long as the request: a budget set or spent
// reset after the request started is visible to the next request,
// never to this one — the same decision the request already made
// on its first read, so nothing is re-decided mid-request. It is
// keyed by key ID, so a request can never see another key's row.
// The mutex is for callers that share one request context across
// goroutines; the normal path is sequential and uncontended.
type budgetMemo struct {
	mu   sync.Mutex
	rows map[string]budgetMemoEntry
}

func (m *budgetMemo) get(keyID string) (budgetMemoEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.rows[keyID]
	return e, ok
}

func (m *budgetMemo) put(keyID string, e budgetMemoEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]budgetMemoEntry{}
	}
	m.rows[keyID] = e
}

func (m *budgetMemo) invalidate(keyID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, keyID)
}

// WithBudgetMemo returns ctx carrying an empty per-request
// global-budget memo for GetBudget. The server attaches exactly
// one memo per HTTP request (in withLogging, the outermost
// wrapper), so every guard on that request shares one database
// read per key. Never attach a memo to a long-lived context (a
// background job, a server-wide context): the memo is a
// request-lifetime snapshot, and a long-lived one would serve
// stale budget rows to later requests.
func WithBudgetMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, budgetMemoKey{}, &budgetMemo{})
}

func budgetMemoFrom(ctx context.Context) *budgetMemo {
	m, _ := ctx.Value(budgetMemoKey{}).(*budgetMemo)
	return m
}

// invalidateBudgetMemo drops keyID from the memo on ctx, if
// any — a write in the same request must not leave a stale row
// behind for a later read in that same request. The writes are
// SetBudget (cap/period), ResetSpent (spent zeroed) and
// RecordUsage (spent_usd incremented — the memoised row carries
// SpentUSD, unlike the key-budget row, so a spend recorded
// mid-request must invalidate too). The async batch writer and
// the import path run on background contexts that never carry a
// memo, so there the call is a no-op.
func invalidateBudgetMemo(ctx context.Context, keyID string) {
	if m := budgetMemoFrom(ctx); m != nil {
		m.invalidate(keyID)
	}
}

// GetBudgetMemoised is GetBudget with the per-request memo
// applied: when ctx carries a memo (WithBudgetMemo), the row —
// or its absence — is read from the database once per request
// per key and served from the memo after that (CHECKLIST
// 299-300). Without a memo the behaviour is unchanged: a fresh
// database read every call. Only a successful read or a
// sql.ErrNoRows miss is memoised; any other error is returned
// uncached so the next read retries the database.
func (l *Ledger) getBudgetMemoised(ctx context.Context, keyID string) (Budget, error) {
	memo := budgetMemoFrom(ctx)
	if memo != nil {
		if e, ok := memo.get(keyID); ok {
			if !e.found {
				return Budget{}, sql.ErrNoRows
			}
			return e.b, nil
		}
	}
	b, err := l.getBudgetUncached(ctx, keyID)
	if memo != nil {
		switch {
		case err == nil:
			memo.put(keyID, budgetMemoEntry{b: b, found: true})
		case errors.Is(err, sql.ErrNoRows):
			memo.put(keyID, budgetMemoEntry{found: false})
		}
	}
	return b, err
}
