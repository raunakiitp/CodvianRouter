// Account-level fallback strategy tests (M3 Area 1, PARITY row
// 388) + preferred/reserve in the auto-combo candidate path
// (row 425).
package router

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// accountGroupChain builds one (provider, model) group: keys in
// the given order, reserve flags per the parallel slice.
func accountGroupChain(provider, model string, keys []string, reserve []bool) []Attempt {
	chain := make([]Attempt, len(keys))
	for i, k := range keys {
		r := false
		if i < len(reserve) {
			r = reserve[i]
		}
		chain[i] = Attempt{ProviderID: provider, Model: model, KeyID: k, Reserve: r}
	}
	return chain
}

func accountKeyOrder(chain []Attempt) []string {
	out := make([]string, len(chain))
	for i, att := range chain {
		out[i] = att.KeyID
	}
	return out
}

func sameOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestParseAccountFallbackStrategy(t *testing.T) {
	valid := []string{"priority", "fill-first", "round-robin", "p2c", "random", "least-used", "cost-optimized", "strict-random", "weighted"}
	for _, v := range valid {
		if _, ok := ParseAccountFallbackStrategy(v); !ok {
			t.Errorf("Parse(%q) = invalid, want valid", v)
		}
	}
	if _, ok := ParseAccountFallbackStrategy(" Round-Robin "); !ok {
		t.Errorf("Parse(%q) = invalid, want valid (case/space tolerant)", " Round-Robin ")
	}
	// expiry-first is BLOCKED: no per-key quota-expiry source
	// exists anywhere, so it must never parse as valid.
	for _, v := range []string{"expiry-first", "EXPIRY-FIRST", "", "no-such-strategy"} {
		if _, ok := ParseAccountFallbackStrategy(v); ok {
			t.Errorf("Parse(%q) = valid, want invalid", v)
		}
	}
}

func TestAccountFallbackStrategyFor(t *testing.T) {
	eng := NewEngine(nil, nil, Config{})
	if got := eng.accountFallbackStrategyFor("prov"); got != AccountFallbackFillFirst {
		t.Errorf("default strategy = %q, want fill-first (OmniRoute default)", got)
	}
	eng = NewEngine(nil, nil, Config{AccountFallbackStrategy: "least-used"})
	if got := eng.accountFallbackStrategyFor("prov"); got != AccountFallbackLeastUsed {
		t.Errorf("global strategy = %q, want least-used", got)
	}
	eng = NewEngine(nil, nil, Config{
		AccountFallbackStrategy:   "least-used",
		AccountFallbackStrategies: map[string]string{"prov": "random", "bad": "expiry-first"},
	})
	if got := eng.accountFallbackStrategyFor("prov"); got != AccountFallbackRandom {
		t.Errorf("per-provider override = %q, want random", got)
	}
	if got := eng.accountFallbackStrategyFor("other"); got != AccountFallbackLeastUsed {
		t.Errorf("other provider = %q, want global least-used", got)
	}
	if got := eng.accountFallbackStrategyFor("bad"); got != AccountFallbackLeastUsed {
		t.Errorf("invalid per-provider value = %q, want fall-through to global least-used", got)
	}
}

func TestOrderAccountAttemptsPriorityKeepsOrder(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "priority"})
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k1", "k2", "k3"})
}

func TestOrderAccountAttemptsLeastUsed(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "least-used"})
	// Real attempts only: k1 used 3x, k2 used 1x, k3 never.
	eng.noteAttemptStats("prov", "k1", true, 10)
	eng.noteAttemptStats("prov", "k1", true, 10)
	eng.noteAttemptStats("prov", "k1", false, 10)
	eng.noteAttemptStats("prov", "k2", true, 10)
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k3", "k2", "k1"})
}

func TestOrderAccountAttemptsRoundRobinRotates(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "round-robin"})
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k1", "k2", "k3"})
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k2", "k3", "k1"})
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k3", "k1", "k2"})
	sameOrder(t, accountKeyOrder(eng.orderAccountAttempts(chain)), []string{"k1", "k2", "k3"})
}

func TestOrderAccountAttemptsStrictRandomDeckCoversAll(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "strict-random"})
	eng.rand = rand.New(rand.NewSource(7))
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
	leaders := map[string]bool{}
	var last string
	for i := 0; i < 3; i++ {
		ordered := eng.orderAccountAttempts(chain)
		if len(ordered) != 3 {
			t.Fatalf("cycle %d: %d attempts, want 3", i, len(ordered))
		}
		leaders[ordered[0].KeyID] = true
		last = ordered[0].KeyID
	}
	if len(leaders) != 3 {
		t.Errorf("one deck cycle led with %v, want all 3 keys leading exactly once", leaders)
	}
	// Next cycle reshuffles with a different first leader than
	// the last leader of the previous cycle.
	next := eng.orderAccountAttempts(chain)
	if next[0].KeyID == last {
		t.Errorf("reshuffled deck leads with %q, same as last leader %q", next[0].KeyID, last)
	}
}

func TestOrderAccountAttemptsRandomAndP2CArePermutations(t *testing.T) {
	for _, strategy := range []string{"random", "p2c", "weighted"} {
		eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: strategy})
		eng.rand = rand.New(rand.NewSource(3))
		chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
		ordered := eng.orderAccountAttempts(chain)
		seen := map[string]int{}
		for _, att := range ordered {
			seen[att.KeyID]++
		}
		for _, k := range []string{"k1", "k2", "k3"} {
			if seen[k] != 1 {
				t.Errorf("strategy %q: key %q appears %d times, want exactly once", strategy, k, seen[k])
			}
		}
	}
}

func TestOrderAccountAttemptsPreservesCrossGroupOrder(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "round-robin"})
	// Two groups interleaved: A slots 0,2; B slot 1. Rotating
	// A must not move B.
	chain := []Attempt{
		{ProviderID: "provA", Model: "m", KeyID: "a1"},
		{ProviderID: "provB", Model: "m", KeyID: "b1"},
		{ProviderID: "provA", Model: "m", KeyID: "a2"},
	}
	ordered := eng.orderAccountAttempts(chain)
	if ordered[1].ProviderID != "provB" {
		t.Errorf("cross-group order moved: %v", ordered)
	}
	// Second call rotates A again: a2 leads the A slots.
	ordered = eng.orderAccountAttempts(chain)
	if ordered[0].KeyID != "a2" || ordered[2].KeyID != "a1" || ordered[1].KeyID != "b1" {
		t.Errorf("second rotation = %v, want [a2 b1 a1]", ordered)
	}
}

// M3 Area 1 (PARITY row 388): round-robin through the full
// orderAttempts path — two requests rotate the leading key of
// a two-key provider. Toggle-verified: if orderAccountAttempts
// is removed from orderAttemptsWithContext, the second request
// still leads with k1 and this test fails.
func TestOrderAttemptsRoundRobinRotatesTwoKeyProvider(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "round-robin"})
	chain := accountGroupChain("prov", "m", []string{"k1", "k2"}, nil)
	ordered, _ := eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k1", "k2"})
	ordered, _ = eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k2", "k1"})
}

// Config-file end-to-end proof (PARITY row 388): an operator
// selects a non-default value in an actual config file, the
// real loader accepts it, and the value reaches the engine —
// the engine built from the loaded file rotates the leading
// key exactly as a hand-built config does.
func TestAccountFallbackConfigFileEndToEnd(t *testing.T) {
	dir := t.TempDir()
	raw := fmt.Sprintf(`{"version":1,"listen":"127.0.0.1:20128","data_dir":%q,"routing":{"account_fallback_strategy":"round-robin","account_fallback_strategies":{"prov":"round-robin"}}}`, dir)
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.AccountFallbackStrategy != "round-robin" || loaded.Routing.AccountFallbackStrategies["prov"] != "round-robin" {
		t.Fatalf("loaded routing = %+v, want round-robin global + per-provider", loaded.Routing)
	}
	eng := NewEngine(nil, nil, Config{
		AccountFallbackStrategy:   loaded.Routing.AccountFallbackStrategy,
		AccountFallbackStrategies: loaded.Routing.AccountFallbackStrategies,
	})
	chain := accountGroupChain("prov", "m", []string{"k1", "k2"}, nil)
	ordered, _ := eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k1", "k2"})
	ordered, _ = eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k2", "k1"})
}

func TestOrderAttemptsAppliesAccountFallbackThenReserveLast(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "least-used"})
	// k1 preferred, used 5x; k2 preferred, unused; k3 reserve,
	// unused. Least-used alone would order k2, k3, k1; the
	// reserve-last tail partition must hold k3 back: k2, k1, k3.
	for i := 0; i < 5; i++ {
		eng.noteAttemptStats("prov", "k1", true, 10)
	}
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, []bool{false, false, true})
	ordered, _ := eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k2", "k1", "k3"})
}

func TestOrderAttemptsPreserveOrderSkipsAccountFallback(t *testing.T) {
	eng := NewEngine(nil, nil, Config{AccountFallbackStrategy: "round-robin"})
	chain := accountGroupChain("prov", "m", []string{"k1", "k2", "k3"}, nil)
	ordered, _ := eng.orderAttempts(chain, "", "", true)
	sameOrder(t, accountKeyOrder(ordered), []string{"k1", "k2", "k3"})
	// A second preserveOrder call must not advance the rotation
	// either — the rotation only runs on engine-ordered chains.
	ordered, _ = eng.orderAttempts(chain, "", "", false)
	sameOrder(t, accountKeyOrder(ordered), []string{"k1", "k2", "k3"})
}

func TestChainForDispatchPreferredReserve(t *testing.T) {
	// Auto-combo candidate path (row 425): a multi-model chain
	// built from interleaved reserve/preferred keys must come
	// out preferred-first, reserve-last, with the resolver's
	// model order untouched within each side.
	chainFor := func(model string) []Attempt {
		switch model {
		case "m1":
			return []Attempt{
				{ProviderID: "provA", Model: "m1", KeyID: "a-res", Reserve: true},
				{ProviderID: "provA", Model: "m1", KeyID: "a-pref"},
			}
		case "m2":
			return []Attempt{
				{ProviderID: "provB", Model: "m2", KeyID: "b-pref"},
				{ProviderID: "provB", Model: "m2", KeyID: "b-res", Reserve: true},
			}
		}
		return nil
	}
	eng := NewEngine(nil, nil, Config{ChainFor: chainFor})
	chain := eng.chainForDispatch("", []string{"m1", "m2"})
	if len(chain) != 4 {
		t.Fatalf("chain = %v, want 4 attempts", chain)
	}
	for i, att := range chain {
		if i < 2 && att.Reserve {
			t.Errorf("chain[%d] = %+v, want preferred attempts first", i, att)
		}
		if i >= 2 && !att.Reserve {
			t.Errorf("chain[%d] = %+v, want reserve attempts last", i, att)
		}
	}
	if chain[0].Model != "m1" || chain[1].Model != "m2" {
		t.Errorf("preferred order = %v, want model order m1 then m2", chain)
	}
}
