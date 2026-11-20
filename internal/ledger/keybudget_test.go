package ledger

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"
)

// seedUsageRow inserts one usage row verbatim (bypassing
// RecordUsage) so tests can place rows at chosen timestamps and
// with chosen price/split knowledge — the rolling-window math
// depends on exactly those fields.
func seedUsageRow(t *testing.T, l *Ledger, ts time.Time, key, provider, model string, cost float64, ok, splitKnown, priceKnown, shared, total int) {
	t.Helper()
	_, err := l.db.ExecContext(context.Background(), `
		INSERT INTO usage_events
		  (ts, gateway_key_id, provider, model, prompt_tokens,
		   completion_tokens, total_tokens, cached_tokens,
		   tokens_split_known, price_known,
		   cost_usd, ok, latency_ms, shared)
		VALUES (?, ?, ?, ?, 0, 0, ?, 0, ?, ?, ?, ?, 0, ?)`,
		ts.UnixMilli(), key, provider, model, total,
		splitKnown, priceKnown, cost, ok, shared)
	if err != nil {
		t.Fatalf("seed usage row: %v", err)
	}
}

func openKeyBudgetLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := Open(t.TempDir() + "/ledger.db")
	if err != nil {
		t.Fatalf("open test ledger: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestRollingWindowMath(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	key := "gk_testwindow"
	// $1.00 six hours ago: outside the rolling window, must not count.
	seedUsageRow(t, l, now.Add(-6*time.Hour), key, "p", "m", 1.00, 1, 1, 1, 0, 10)
	// $2.00 one hour ago and $0.50 four hours ago: inside, must count.
	seedUsageRow(t, l, now.Add(-1*time.Hour), key, "p", "m", 2.00, 1, 1, 1, 0, 10)
	seedUsageRow(t, l, now.Add(-4*time.Hour), key, "p", "m", 0.50, 1, 1, 1, 0, 10)
	st, err := l.KeyBudgetStatus(ctx, key, now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.SpentUSD != 2.50 {
		t.Errorf("spent = $%.4f, want $2.50 (the 6h-old $1.00 must have aged out)", st.SpentUSD)
	}
	if !st.WindowStart.Equal(now.Add(-RollingWindow).UTC()) {
		t.Errorf("window start = %v, want %v", st.WindowStart, now.Add(-RollingWindow).UTC())
	}
	if !st.WindowEnd.Equal(now.UTC()) {
		t.Errorf("window end = %v, want %v", st.WindowEnd, now.UTC())
	}
	// Next roll-off: the oldest in-window priced spend (4h ago)
	// ages out 1h from now.
	if st.NextRolloff == nil {
		t.Fatal("next rolloff is nil, want the 4h-old spend's roll-off")
	}
	want := now.Add(-4 * time.Hour).Add(RollingWindow)
	if st.NextRolloff.Sub(want) > time.Second || want.Sub(*st.NextRolloff) > time.Second {
		t.Errorf("next rolloff = %v, want ~%v", *st.NextRolloff, want)
	}
	// Boundary: a row just inside the window counts, a row just
	// outside does not.
	seedUsageRow(t, l, now.Add(-RollingWindow).Add(time.Second), key, "p", "m", 0.25, 1, 1, 1, 0, 10)
	seedUsageRow(t, l, now.Add(-RollingWindow).Add(-time.Second), key, "p", "m", 4.00, 1, 1, 1, 0, 10)
	st, err = l.KeyBudgetStatus(ctx, key, now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.SpentUSD != 2.75 {
		t.Errorf("spent = $%.4f, want $2.75 (boundary row in, older row out)", st.SpentUSD)
	}
}

func TestKeyBudgetFailClosedVsCountAsZero(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	key := "gk_testpolicy"
	priced := func(m string) bool { return m == "priced-model" }
	if err := l.SetKeyBudget(ctx, key, 1.00, UnpricedFailClosed); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// Priced spend $0.50 of a $1.00 cap: under the cap.
	seedUsageRow(t, l, now.Add(-time.Hour), key, "p", "priced-model", 0.50, 1, 1, 1, 0, 10)
	// A priced model may proceed under either policy.
	if err := l.CheckKeyBudget(ctx, key, []string{"priced-model"}, priced, now); err != nil {
		t.Errorf("priced model under fail_closed: unexpected error %v", err)
	}
	// An unpriced model is blocked under fail_closed with a 400
	// naming the model — not a 402.
	err := l.CheckKeyBudget(ctx, key, []string{"mystery-model"}, priced, now)
	var ue *UnpricedUsageError
	if !errors.As(err, &ue) {
		t.Fatalf("unpriced model under fail_closed: err = %v, want *UnpricedUsageError", err)
	}
	if len(ue.Models) != 1 || ue.Models[0] != "mystery-model" {
		t.Errorf("unpriced models = %v, want [mystery-model]", ue.Models)
	}
	if !IsUnpricedUsage(err) {
		t.Errorf("IsUnpricedUsage = false for the 400 error")
	}
	if IsBudgetExceeded(err) {
		t.Errorf("unpriced block must not be a BudgetExceededError (402)")
	}
	// Switch the same key to count_as_zero: the same unpriced
	// model now proceeds, counting $0, and priced spend is still
	// enforced.
	if err := l.SetKeyBudget(ctx, key, 1.00, UnpricedCountAsZero); err != nil {
		t.Fatalf("set budget count_as_zero: %v", err)
	}
	if err := l.CheckKeyBudget(ctx, key, []string{"mystery-model"}, priced, now); err != nil {
		t.Errorf("unpriced model under count_as_zero: unexpected error %v", err)
	}
	// Priced spend over the cap is a 402 even for an unpriced
	// model: the cap is the primary block, and the 400 is only
	// for blocks caused by unpriced usage alone.
	seedUsageRow(t, l, now.Add(-30*time.Minute), key, "p", "priced-model", 0.75, 1, 1, 1, 0, 10)
	err = l.CheckKeyBudget(ctx, key, []string{"mystery-model"}, priced, now)
	var be *BudgetExceededError
	if !errors.As(err, &be) {
		t.Fatalf("over-cap check: err = %v, want *BudgetExceededError", err)
	}
	if be.CapUSD != 1.00 || be.SpentUSD != 1.25 {
		t.Errorf("over-cap error = cap $%.2f spent $%.2f, want $1.00 / $1.25", be.CapUSD, be.SpentUSD)
	}
	if IsUnpricedUsage(err) {
		t.Errorf("over-cap block must not be an UnpricedUsageError (400)")
	}
}

func TestKeyBudgetFallbackAttemptsConsumeNoQuota(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	key := "gk_testfallback"
	if err := l.SetKeyBudget(ctx, key, 1.00, UnpricedFailClosed); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// Two failed attempts (a 429, then a 500): exactly what the
	// engine records for them — ok=0, no usage, cost $0.
	seedUsageRow(t, l, now.Add(-time.Minute), key, "dead", "m", 0, 0, 0, 0, 0, 0)
	seedUsageRow(t, l, now.Add(-time.Minute), key, "dead2", "m", 0, 0, 0, 0, 0, 0)
	// The successful fallback attempt: $0.25 of priced spend.
	seedUsageRow(t, l, now.Add(-time.Minute), key, "live", "m", 0.25, 1, 1, 1, 0, 10)
	// A coalesced waiter row, even with a (bogus) cost attached,
	// must never count: shared rows are served, never billed.
	seedUsageRow(t, l, now.Add(-time.Minute), key, "live", "m", 5.00, 1, 1, 1, 1, 10)
	st, err := l.KeyBudgetStatus(ctx, key, now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.SpentUSD != 0.25 {
		t.Errorf("spent = $%.4f, want $0.25 (only the successful fallback attempt)", st.SpentUSD)
	}
	if err := l.CheckKeyBudget(ctx, key, []string{"m"}, func(string) bool { return true }, now); err != nil {
		t.Errorf("key under its cap after fallback attempts: unexpected error %v", err)
	}
}

func TestKeyBudgetUnreportedUsageIsUnknownNotZero(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	key := "gk_testunknown"
	if err := l.SetKeyBudget(ctx, key, 1.00, UnpricedFailClosed); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	// A successful streaming attempt: usage never reported, so
	// split unknown, cost unknown — never $0, never spent.
	seedUsageRow(t, l, now.Add(-time.Hour), key, "p", "stream-model", 0, 1, 0, 0, 0, 0)
	// A successful attempt with a known split but no configured
	// price: unpriced usage, also unknown.
	seedUsageRow(t, l, now.Add(-time.Hour), key, "p", "unpriced-model", 0, 1, 1, 0, 0, 100)
	// A failed attempt: ok=0, $0 — known-zero, not unknown spend.
	seedUsageRow(t, l, now.Add(-time.Hour), key, "p", "m", 0, 0, 0, 0, 0, 0)
	st, err := l.KeyBudgetStatus(ctx, key, now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.SpentUSD != 0 {
		t.Errorf("spent = $%.4f, want $0 (unknown spend is never counted as spent)", st.SpentUSD)
	}
	if st.UnknownEvents != 2 {
		t.Errorf("unknown events = %d, want 2 (streaming + unpriced; the failed attempt is known-zero)", st.UnknownEvents)
	}
	if st.UnknownTokens != 100 {
		t.Errorf("unknown tokens = %d, want 100 (only the unpriced row reported a total)", st.UnknownTokens)
	}
	if len(st.UnpricedModels) != 1 || st.UnpricedModels[0] != "unpriced-model" {
		t.Errorf("unpriced models = %v, want [unpriced-model]", st.UnpricedModels)
	}
	// The key is NOT at its limit: unknown spend never blocks by
	// itself, and priced spend is $0.
	if st.AtLimit {
		t.Errorf("at_limit = true with $0 priced spend and only unknown usage")
	}
}

func TestKeyBudgetGenuinelyFreeIsPriced(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	// A model configured at $0 is genuinely free: priced, not
	// unknown. Record it through the real RecordUsage path.
	free := Prices{PriceKey{Model: "free-model"}: ModelPrice{PromptPerToken: 0, CompletionPerToken: 0}}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "gk_testfree", Provider: "p", Model: "free-model",
		PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true,
	}, free); err != nil {
		t.Fatalf("record free usage: %v", err)
	}
	// And an unpriced model through the same path: same $0 cost
	// column, but unknown.
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "gk_testfree", Provider: "p", Model: "mystery-model",
		PromptTokens: 10, CompletionTokens: 5, SplitKnown: true, OK: true,
	}, free); err != nil {
		t.Fatalf("record unpriced usage: %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "gk_testfree", time.Now())
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.SpentUSD != 0 {
		t.Errorf("spent = $%.4f, want $0", st.SpentUSD)
	}
	if st.UnknownEvents != 1 {
		t.Errorf("unknown events = %d, want 1 (only the unpriced model; the free model is priced)", st.UnknownEvents)
	}
	if len(st.UnpricedModels) != 1 || st.UnpricedModels[0] != "mystery-model" {
		t.Errorf("unpriced models = %v, want [mystery-model]", st.UnpricedModels)
	}
	if !free.Known("p", "free-model") || !free.KnownModel("free-model") {
		t.Errorf("a configured $0 price must count as known")
	}
	if free.Known("p", "mystery-model") || free.KnownModel("mystery-model") {
		t.Errorf("an unpriced model must not count as known")
	}
}

func TestKeyBudgetRestartPersistence(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()
	l, err := Open(dir + "/ledger.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := l.SetKeyBudget(ctx, "gk_testpersist", 2.50, UnpricedCountAsZero); err != nil {
		t.Fatalf("set budget: %v", err)
	}
	prices := Prices{PriceKey{Model: "m"}: ModelPrice{PromptPerToken: 0.00001, CompletionPerToken: 0.00002}}
	if _, err := l.RecordUsage(ctx, Usage{
		GatewayKeyID: "gk_testpersist", Provider: "p", Model: "m",
		PromptTokens: 1000, CompletionTokens: 1000, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("record usage: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Reopen the same database file: the budget row and the spend
	// must both survive the restart.
	l2, err := Open(dir + "/ledger.db")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer l2.Close()
	b, err := l2.GetKeyBudget(ctx, "gk_testpersist")
	if err != nil {
		t.Fatalf("get budget after restart: %v", err)
	}
	if b.LimitUSD != 2.50 || b.UnpricedPolicy != UnpricedCountAsZero {
		t.Errorf("budget after restart = %+v, want limit $2.50 count_as_zero", b)
	}
	st, err := l2.KeyBudgetStatus(ctx, "gk_testpersist", now)
	if err != nil {
		t.Fatalf("status after restart: %v", err)
	}
	// 1000 * $0.00001 + 1000 * $0.00002 = $0.03.
	if math.Abs(st.SpentUSD-0.03) > 1e-9 {
		t.Errorf("spent after restart = $%.4f, want $0.03", st.SpentUSD)
	}
}

func TestKeyBudgetValidationAndUnlimited(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := context.Background()
	now := time.Now()
	if err := l.SetKeyBudget(ctx, "", 1, UnpricedFailClosed); err == nil {
		t.Errorf("empty key id accepted")
	}
	if err := l.SetKeyBudget(ctx, "gk_x", -1, UnpricedFailClosed); err == nil {
		t.Errorf("negative limit accepted")
	}
	if err := l.SetKeyBudget(ctx, "gk_x", 1, "bogus"); err == nil {
		t.Errorf("unknown policy accepted")
	}
	if _, err := l.GetKeyBudget(ctx, "gk_missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing budget: err = %v, want sql.ErrNoRows", err)
	}
	// No budget row: unlimited — the check passes for any model,
	// priced or not, and the status says so while still reporting
	// window spend.
	if err := l.CheckKeyBudget(ctx, "gk_missing", []string{"anything"}, nil, now); err != nil {
		t.Errorf("unlimited key check: unexpected error %v", err)
	}
	st, err := l.KeyBudgetStatus(ctx, "gk_missing", now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.HasBudget || st.RemainingUSD != nil || st.AtLimit {
		t.Errorf("unlimited status = %+v, want has_budget=false, remaining=nil, at_limit=false", st)
	}
	// Set then delete: the key is unlimited again.
	if err := l.SetKeyBudget(ctx, "gk_x", 1, UnpricedFailClosed); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := l.DeleteKeyBudget(ctx, "gk_x"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := l.GetKeyBudget(ctx, "gk_x"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("deleted budget: err = %v, want sql.ErrNoRows", err)
	}
	// A $0 limit blocks everything under enforcement (cap 0 =
	// any spend exceeds it), matching the global budget's rule.
	if err := l.SetKeyBudget(ctx, "gk_zero", 0, UnpricedFailClosed); err != nil {
		t.Fatalf("set zero budget: %v", err)
	}
	if err := l.CheckKeyBudget(ctx, "gk_zero", []string{"m"}, func(string) bool { return true }, now); !IsBudgetExceeded(err) {
		t.Errorf("$0-limit check: err = %v, want BudgetExceededError", err)
	}
}
