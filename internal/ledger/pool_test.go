package ledger

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// M5.4: fair-share quota pools — OmniRoute's decideFairShare
// (fairShare.ts) over ledger-derived consumption.

func poolCtx() context.Context { return context.Background() }

func testPool() QuotaPool {
	return QuotaPool{
		ID:                  "pool1",
		Name:                "team",
		SaturationThreshold: 0.5,
		Dimensions: []QuotaDimension{
			{Unit: "requests", Window: "daily", Limit: 100},
		},
		Allocations: []PoolAllocation{
			{KeyID: "k1", Weight: 50, Policy: "hard"},
			{KeyID: "k2", Weight: 50, Policy: "hard"},
		},
	}
}

// seedRequests records n successful requests for key at ts.
func seedRequests(t *testing.T, l *Ledger, key string, ts time.Time, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedUsageRow(t, l, ts, key, "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 10)
	}
}

func TestQuotaPoolCRUD(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	pool := testPool()
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatalf("SetQuotaPool: %v", err)
	}
	got, err := l.GetQuotaPool(ctx, "pool1")
	if err != nil {
		t.Fatalf("GetQuotaPool: %v", err)
	}
	if got.Name != "team" || got.SaturationThreshold != 0.5 || len(got.Dimensions) != 1 || len(got.Allocations) != 2 {
		t.Errorf("GetQuotaPool = %+v, want team/0.5/1 dim/2 allocs", got)
	}
	if got.Dimensions[0].Unit != "requests" || got.Dimensions[0].Limit != 100 {
		t.Errorf("dimension = %+v, want requests/daily/100", got.Dimensions[0])
	}
	list, err := l.ListQuotaPools(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListQuotaPools = %d, err %v, want 1", len(list), err)
	}
	forKey, err := l.PoolsForKey(ctx, "k1")
	if err != nil || len(forKey) != 1 {
		t.Fatalf("PoolsForKey(k1) = %d, err %v, want 1", len(forKey), err)
	}
	if forKey, err := l.PoolsForKey(ctx, "nobody"); err != nil || len(forKey) != 0 {
		t.Fatalf("PoolsForKey(nobody) = %d, err %v, want 0", len(forKey), err)
	}
	// Replace wholesale: new dimensions + allocations.
	pool.Dimensions = []QuotaDimension{{Unit: "tokens", Window: "daily", Limit: 1000}}
	pool.Allocations = []PoolAllocation{{KeyID: "k1", Weight: 100, Policy: "burst"}}
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatalf("SetQuotaPool replace: %v", err)
	}
	got, _ = l.GetQuotaPool(ctx, "pool1")
	if len(got.Dimensions) != 1 || got.Dimensions[0].Unit != "tokens" || len(got.Allocations) != 1 {
		t.Errorf("after replace: %+v, want 1 tokens dim / 1 alloc", got)
	}
	if err := l.DeleteQuotaPool(ctx, "pool1"); err != nil {
		t.Fatalf("DeleteQuotaPool: %v", err)
	}
	if _, err := l.GetQuotaPool(ctx, "pool1"); err == nil {
		t.Error("GetQuotaPool after delete must fail")
	}
}

func TestCheckPoolFairShareGenerous(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pool := testPool() // limit 100/day, k1 50% hard, k2 50% hard
	// Generous mode: total 10 of 100 (10% < 50% threshold).
	// k1 used 8 — under its 50 fair share. Hard allows.
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l, "k1", now.Add(-time.Hour), 8)
	seedRequests(t, l, "k2", now.Add(-time.Hour), 2)
	block, penalized, err := l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil || penalized {
		t.Errorf("generous hard under fair share: block=%v penalized=%v err=%v, want allow", block, penalized, err)
	}
	// Generous mode, hard key past its fair share: still allowed
	// (borrowing), because the pool has slack. Fresh ledger with
	// a 200 limit so k1 at 60 stays under the 50% threshold
	// (60/200 = 30%) while past its 100 fair share... no — the
	// fair share of a 200 limit at weight 50 is 100, so k1 at 60
	// is UNDER it. Use weight 10 instead: fair share 20, k1 at
	// 60 is past it, total 60/200 = 30% stays generous.
	l2 := openKeyBudgetLedger(t)
	pool2 := testPool()
	pool2.Dimensions[0].Limit = 200
	pool2.Allocations[0].Weight = 10
	if err := l2.SetQuotaPool(ctx, pool2); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l2, "k1", now.Add(-time.Hour), 60)
	block, penalized, err = l2.CheckPoolFairShare(ctx, "k1", pool2, now)
	if err != nil || block != nil || penalized {
		t.Errorf("generous hard past fair share (60/20 share, 60/200 total): block=%v penalized=%v err=%v, want allow", block, penalized, err)
	}
	// Generous mode, soft key past its fair share: allowed +
	// penalized.
	pool2.Allocations[0].Policy = "soft"
	if err := l2.SetQuotaPool(ctx, pool2); err != nil {
		t.Fatal(err)
	}
	block, penalized, err = l2.CheckPoolFairShare(ctx, "k1", pool2, now)
	if err != nil || block != nil || !penalized {
		t.Errorf("generous soft past fair share: block=%v penalized=%v err=%v, want allow+penalized", block, penalized, err)
	}
}

func TestCheckPoolFairShareStrict(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pool := testPool() // limit 100/day, threshold 0.5
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Total 60 (60% >= 50%): strict mode.
	seedRequests(t, l, "k1", now.Add(-time.Hour), 40)
	seedRequests(t, l, "k2", now.Add(-time.Hour), 20)
	// k1 at 40 < 50 fair share: hard allows.
	block, penalized, err := l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil || penalized {
		t.Errorf("strict hard under fair share: block=%v penalized=%v err=%v, want allow", block, penalized, err)
	}
	// k1 at 50 = fair share: hard blocks.
	seedRequests(t, l, "k1", now.Add(-30*time.Minute), 10)
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("strict hard at fair share: block=%v err=%v, want fair-share block", block, err)
	}
	// Same position, soft policy: allowed + penalized.
	pool.Allocations[0].Policy = "soft"
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	block, penalized, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil || !penalized {
		t.Errorf("strict soft at fair share: block=%v penalized=%v err=%v, want allow+penalized", block, penalized, err)
	}
	// Same position, burst policy: allowed, not penalized.
	pool.Allocations[0].Policy = "burst"
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	block, penalized, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil || penalized {
		t.Errorf("strict burst at fair share: block=%v penalized=%v err=%v, want clean allow", block, penalized, err)
	}
	// Unknown policy is normalized to hard: blocks.
	pool.Allocations[0].Policy = "weird"
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("unknown policy: block=%v err=%v, want fair-share block (hard)", block, err)
	}
}

func TestCheckPoolFairShareCapsAndGlobal(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pool := testPool()
	cap := 10.0
	pool.Allocations[0].CapValue = &cap
	pool.Allocations[0].CapUnit = "requests"
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// k1 at its cap (10) with the pool nearly empty: the absolute
	// cap fires in generous mode too.
	seedRequests(t, l, "k1", now.Add(-time.Hour), 10)
	block, _, err := l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "cap-absolute" {
		t.Errorf("at cap in generous mode: block=%v err=%v, want cap-absolute", block, err)
	}
	// A cap in a different unit does not fire on this dimension.
	pool.Allocations[0].CapUnit = "tokens"
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil {
		t.Errorf("cap in another unit: block=%v err=%v, want allow", block, err)
	}
	// Global saturation blocks every policy, burst included.
	l2 := openKeyBudgetLedger(t)
	pool2 := testPool()
	pool2.Allocations[0].Policy = "burst"
	if err := l2.SetQuotaPool(ctx, pool2); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l2, "k1", now.Add(-time.Hour), 50)
	seedRequests(t, l2, "k2", now.Add(-time.Hour), 50)
	block, _, err = l2.CheckPoolFairShare(ctx, "k1", pool2, now)
	if err != nil || block == nil || block.Reason != "global-saturated" {
		t.Errorf("total 100/100: block=%v err=%v, want global-saturated", block, err)
	}
	// A key with no allocation in the pool is never gated.
	block, _, err = l2.CheckPoolFairShare(ctx, "outsider", pool2, now)
	if err != nil || block != nil {
		t.Errorf("non-member key: block=%v err=%v, want allow", block, err)
	}
	// A pool with no dimensions always allows.
	pool3 := testPool()
	pool3.Dimensions = nil
	block, _, err = l2.CheckPoolFairShare(ctx, "k1", pool3, now)
	if err != nil || block != nil {
		t.Errorf("dimension-less pool: block=%v err=%v, want allow", block, err)
	}
}

func TestCheckPoolFairShareUnitsAndWindows(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// Tokens dimension: SUM(total_tokens). k1 used 900 of a 1000
	// pool limit with weight 50 (fair share 500) — strict (90%
	// >= 50%), hard blocks.
	pool := QuotaPool{
		ID: "tokens", Name: "tokens",
		Dimensions:  []QuotaDimension{{Unit: "tokens", Window: "daily", Limit: 1000}},
		Allocations: []PoolAllocation{{KeyID: "k1", Weight: 50, Policy: "hard"}},
	}
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.01, 1, 1, 1, 0, 900)
	block, _, err := l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("tokens 900/1000, fair share 500: block=%v err=%v, want fair-share", block, err)
	}
	// Failed rows (ok=0) do not count.
	seedUsageRow(t, l, now.Add(-30*time.Minute), "k1", "openai", "gpt-4o", 0.01, 0, 1, 1, 0, 5000)
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("after failed rows: block=%v err=%v, want the same fair-share block", block, err)
	}
	// USD dimension: SUM(cost_usd).
	poolUSD := QuotaPool{
		ID: "usd", Name: "usd",
		Dimensions:  []QuotaDimension{{Unit: "usd", Window: "daily", Limit: 1.0}},
		Allocations: []PoolAllocation{{KeyID: "k1", Weight: 50, Policy: "hard"}},
	}
	if err := l.SetQuotaPool(ctx, poolUSD); err != nil {
		t.Fatal(err)
	}
	seedUsageRow(t, l, now.Add(-time.Hour), "k1", "openai", "gpt-4o", 0.60, 1, 1, 1, 0, 10)
	block, _, err = l.CheckPoolFairShare(ctx, "k1", poolUSD, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("usd 0.60/1.00, fair share 0.50: block=%v err=%v, want fair-share", block, err)
	}
	// 5h rolling window: 6-hour-old rows age out.
	pool5h := QuotaPool{
		ID: "five", Name: "five",
		Dimensions:  []QuotaDimension{{Unit: "requests", Window: "5h", Limit: 10}},
		Allocations: []PoolAllocation{{KeyID: "k1", Weight: 50, Policy: "hard"}},
	}
	if err := l.SetQuotaPool(ctx, pool5h); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l, "k1", now.Add(-6*time.Hour), 9)
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool5h, now)
	if err != nil || block != nil {
		t.Errorf("6h-old rows in a 5h window: block=%v err=%v, want allow", block, err)
	}
	seedRequests(t, l, "k1", now.Add(-time.Hour), 5)
	// Total 5/10 = 50% >= threshold: strict; k1 at 5 = fair
	// share 5: hard blocks.
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool5h, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("5h window, k1 5/5 fair share: block=%v err=%v, want fair-share", block, err)
	}
	if block != nil && !block.NextReset.After(now) {
		t.Errorf("5h block NextReset = %v, want after now (now+5h bound)", block.NextReset)
	}
}

func TestPoolDimensionStatus(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pool := testPool()
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l, "k1", now.Add(-time.Hour), 30)
	seedRequests(t, l, "k2", now.Add(-time.Hour), 20)
	used, nextReset, err := l.PoolDimensionStatus(ctx, pool, pool.Dimensions[0], now)
	if err != nil {
		t.Fatal(err)
	}
	if used != 50 {
		t.Errorf("pool-wide used = %v, want 50", used)
	}
	if !nextReset.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nextReset = %v, want 2026-11-19 00:00 UTC", nextReset)
	}
}

// M5.5: per-account limits — the dimension limit is per-account
// and the pool's effective limit is limit × backing accounts.
func TestCheckPoolFairSharePerAccountMultiplier(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	// 3 accounts × 100/requests/day = 300 effective; k1 weight
	// 50 → fair share 150.
	pool := testPool()
	pool.AccountCount = 3
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	got, err := l.GetQuotaPool(ctx, "pool1")
	if err != nil || got.AccountCount != 3 {
		t.Fatalf("GetQuotaPool account_count = %d, err %v, want 3", got.AccountCount, err)
	}
	if got.EffectiveLimit(got.Dimensions[0]) != 300 {
		t.Errorf("effective limit = %v, want 300", got.EffectiveLimit(got.Dimensions[0]))
	}
	// Total 200/300 (67% >= 50%): strict. k1 at 100 < 150 fair
	// share: allowed — under a single-account reading (100
	// limit) this would already be globally saturated.
	seedRequests(t, l, "k1", now.Add(-time.Hour), 100)
	seedRequests(t, l, "k2", now.Add(-time.Hour), 100)
	block, _, err := l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block != nil {
		t.Errorf("200/300 total, k1 100/150 fair share: block=%v err=%v, want allow", block, err)
	}
	// k2 at 150 = fair share with total 250 < 300: fair-share
	// block (the global ceiling has not been reached).
	seedRequests(t, l, "k2", now.Add(-30*time.Minute), 50)
	block, _, err = l.CheckPoolFairShare(ctx, "k2", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("k2 at fair share 150, total 250/300: block=%v err=%v, want fair-share", block, err)
	}
	if block != nil && block.EffectiveLimit != 300 {
		t.Errorf("block effective limit = %v, want 300", block.EffectiveLimit)
	}
	// Total 300/300: the global ceiling fires for every key.
	seedRequests(t, l, "k1", now.Add(-15*time.Minute), 50)
	block, _, err = l.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "global-saturated" {
		t.Errorf("total 300/300: block=%v err=%v, want global-saturated", block, err)
	}
	// k1's own fair-share block, isolated: k1 at 150 with k2 at
	// 0 (total 150 < 300) blocks on fair-share, not global.
	l2 := openKeyBudgetLedger(t)
	if err := l2.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l2, "k1", now.Add(-time.Hour), 150)
	block, _, err = l2.CheckPoolFairShare(ctx, "k1", pool, now)
	if err != nil || block == nil || block.Reason != "fair-share" {
		t.Errorf("k1 at fair share 150, total 150/300: block=%v err=%v, want fair-share", block, err)
	}
	// account_count 0 normalizes to 1 (single account).
	pool0 := testPool()
	pool0.ID = "pool0"
	pool0.AccountCount = 0
	if err := l.SetQuotaPool(ctx, pool0); err != nil {
		t.Fatal(err)
	}
	got0, _ := l.GetQuotaPool(ctx, "pool0")
	if got0.EffectiveAccountCount() != 1 || got0.EffectiveLimit(got0.Dimensions[0]) != 100 {
		t.Errorf("account_count 0: effective accounts/limit = %d/%v, want 1/100", got0.EffectiveAccountCount(), got0.EffectiveLimit(got0.Dimensions[0]))
	}
}

func TestPoolKeyDimensionUsage(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	pool := testPool()
	pool.AccountCount = 2
	if err := l.SetQuotaPool(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedRequests(t, l, "k1", now.Add(-time.Hour), 30)
	seedRequests(t, l, "k2", now.Add(-time.Hour), 20)
	used, total, nextReset, err := l.PoolKeyDimensionUsage(ctx, pool, pool.Dimensions[0], "k1", now)
	if err != nil {
		t.Fatal(err)
	}
	if used != 30 || total != 50 {
		t.Errorf("usage = key %v / total %v, want 30 / 50", used, total)
	}
	if !nextReset.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("nextReset = %v, want 2026-11-19 00:00 UTC", nextReset)
	}
	// Migration: a hand-built legacy quota_pools table without
	// account_count gains the column (default 1) on Open.
	dir := t.TempDir()
	legacyPath := dir + "/legacy.db"
	db, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE quota_pools (id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', saturation_threshold REAL NOT NULL DEFAULT 0.5)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_pools (id, name) VALUES ('old', 'old pool')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	led, err := Open(legacyPath)
	if err != nil {
		t.Fatalf("Open legacy pool db: %v", err)
	}
	defer led.Close()
	old, err := led.GetQuotaPool(ctx, "old")
	if err != nil {
		t.Fatalf("GetQuotaPool legacy: %v", err)
	}
	if old.EffectiveAccountCount() != 1 {
		t.Errorf("legacy pool account count = %d, want 1 (migrated default)", old.EffectiveAccountCount())
	}
}

// M5.6: per-model usage slices for self-service reporting.
func TestUsageSlices(t *testing.T) {
	l := openKeyBudgetLedger(t)
	ctx := poolCtx()
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	// k1: two rows same model (grouped), one row another model,
	// one row outside the window, one failed row, one shared row.
	seedUsageRow(t, l, day.Add(1*time.Hour), "k1", "openai", "gpt-5", 1.50, 1, 1, 1, 0, 1000)
	seedUsageRow(t, l, day.Add(2*time.Hour), "k1", "openai", "gpt-5", 0.50, 1, 1, 1, 0, 500)
	seedUsageRow(t, l, day.Add(3*time.Hour), "k1", "anthropic", "claude-x", 2.00, 1, 1, 1, 0, 800)
	seedUsageRow(t, l, day.Add(-1*time.Hour), "k1", "openai", "gpt-5", 9.00, 1, 1, 1, 0, 9000)
	seedUsageRow(t, l, day.Add(4*time.Hour), "k1", "openai", "gpt-5", 3.00, 0, 1, 1, 0, 3000)
	seedUsageRow(t, l, day.Add(5*time.Hour), "k1", "openai", "gpt-5", 4.00, 1, 1, 1, 1, 4000)
	slices, err := l.UsageSlices(ctx, "k1", day, day.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(slices) != 2 {
		t.Fatalf("slices = %d rows, want 2: %+v", len(slices), slices)
	}
	gpt := slices[1]
	if gpt.Provider != "openai" || gpt.Model != "gpt-5" || gpt.Rows != 2 || gpt.CostUSD != 2.00 || gpt.TotalTokens != 1500 {
		t.Errorf("gpt-5 slice = %+v, want 2 rows / $2.00 / 1500 tokens", gpt)
	}
	if !gpt.PriceKnown || !gpt.SplitKnown {
		t.Errorf("gpt-5 flags = priceKnown %v splitKnown %v, want true/true", gpt.PriceKnown, gpt.SplitKnown)
	}
	claude := slices[0]
	if claude.Provider != "anthropic" || claude.Rows != 1 || claude.CostUSD != 2.00 {
		t.Errorf("claude slice = %+v, want 1 row / $2.00", claude)
	}
	// Unpriced row in the group flips PriceKnown off.
	seedUsageRow(t, l, day.Add(6*time.Hour), "k1", "openai", "gpt-5", 0, 1, 1, 0, 0, 100)
	slices, _ = l.UsageSlices(ctx, "k1", day, day.Add(24*time.Hour))
	if slices[1].PriceKnown {
		t.Error("unpriced row in group must flip PriceKnown off")
	}
	if slices[1].CostUSD != 2.00 {
		t.Errorf("cost after unpriced row = %v, want 2.00 (unpriced rows add $0)", slices[1].CostUSD)
	}
	// Empty window: no rows, no error.
	empty, err := l.UsageSlices(ctx, "k1", day.Add(48*time.Hour), day.Add(72*time.Hour))
	if err != nil || len(empty) != 0 {
		t.Errorf("empty window = %d rows err %v, want 0/nil", len(empty), err)
	}
	// Other keys never leak in.
	other, err := l.UsageSlices(ctx, "k2", day, day.Add(24*time.Hour))
	if err != nil || len(other) != 0 {
		t.Errorf("other key = %d rows err %v, want 0/nil", len(other), err)
	}
}
