package server

import (
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// Per-key traffic limits (CHECKLIST 195): the in-memory sliding
// 60-second window — RPM counts admitted requests, TPM counts
// locally estimated prompt tokens, both prune as entries age out,
// and a refusal names the wait until the oldest entry ages out.

func TestKeyTrafficLimiterRPM(t *testing.T) {
	l := newKeyTrafficLimiter()
	now := time.Now()
	if ok, _, _ := l.tryAdmit("k1", 0, trafficLimits{RPM: 2}, now); !ok {
		t.Fatal("first request must be admitted")
	}
	if ok, _, _ := l.tryAdmit("k1", 0, trafficLimits{RPM: 2}, now.Add(time.Second)); !ok {
		t.Fatal("second request must be admitted")
	}
	ok, retryAfter, refusal := l.tryAdmit("k1", 0, trafficLimits{RPM: 2}, now.Add(2*time.Second))
	if ok {
		t.Fatal("third request inside the window must be refused")
	}
	if refusal.Limit != "requests_per_minute" {
		t.Errorf("limit = %q, want requests_per_minute", refusal.Limit)
	}
	// The oldest entry (t0) ages out at t0+60s; at t0+2s the wait
	// is 58s.
	if retryAfter != 58*time.Second {
		t.Errorf("retryAfter = %v, want 58s", retryAfter)
	}
	// A different key is unaffected.
	if ok, _, _ := l.tryAdmit("k2", 0, trafficLimits{RPM: 2}, now.Add(2*time.Second)); !ok {
		t.Fatal("a different key must not share the window")
	}
	// After the window slides past, admission works again.
	if ok, _, _ := l.tryAdmit("k1", 0, trafficLimits{RPM: 2}, now.Add(61*time.Second)); !ok {
		t.Fatal("request after the window aged out must be admitted")
	}
}

func TestKeyTrafficLimiterTPM(t *testing.T) {
	l := newKeyTrafficLimiter()
	now := time.Now()
	if ok, _, _ := l.tryAdmit("k1", 6, trafficLimits{TPM: 10}, now); !ok {
		t.Fatal("6 tokens of a 10-token budget must be admitted")
	}
	ok, retryAfter, refusal := l.tryAdmit("k1", 6, trafficLimits{TPM: 10}, now.Add(time.Second))
	if ok {
		t.Fatal("6 more tokens (12 > 10) must be refused")
	}
	if refusal.Limit != "tokens_per_minute" {
		t.Errorf("limit = %q, want tokens_per_minute", refusal.Limit)
	}
	if retryAfter < time.Second || retryAfter > 60*time.Second {
		t.Errorf("retryAfter = %v, want within (0, 60s]", retryAfter)
	}
	// 4 tokens still fit in the remaining budget.
	if ok, _, _ := l.tryAdmit("k1", 4, trafficLimits{TPM: 10}, now.Add(2*time.Second)); !ok {
		t.Fatal("4 tokens (6+4 = 10) must be admitted")
	}
	// A zero-token request (uncountable prompt) consumes a request
	// slot but no token budget.
	if ok, _, _ := l.tryAdmit("k1", 0, trafficLimits{TPM: 10}, now.Add(3*time.Second)); !ok {
		t.Fatal("a zero-token request must be admitted when the token budget is exactly full")
	}
}

func TestKeyTrafficLimiterUnlimitedRecordsUsage(t *testing.T) {
	l := newKeyTrafficLimiter()
	now := time.Now()
	for i := 0; i < 3; i++ {
		if ok, _, _ := l.tryAdmit("k1", 7, trafficLimits{}, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatalf("request %d must be admitted with no limits set", i)
		}
	}
	reqs, toks := l.usage("k1", now.Add(3*time.Second))
	if reqs != 3 || toks != 21 {
		t.Errorf("usage = %d requests / %d tokens, want 3 / 21", reqs, toks)
	}
	// Entries age out of the usage count too: at t+61s the cutoff
	// is t+1s, so the t+0s and t+1s entries are gone (an entry ages
	// out at exactly 60s) and only the t+2s entry remains.
	reqs, toks = l.usage("k1", now.Add(61*time.Second))
	if reqs != 1 || toks != 7 {
		t.Errorf("usage after 61s = %d / %d, want 1 / 7 (only the t+2s entry remains)", reqs, toks)
	}
	// Unknown keys report zero usage, never an error.
	if reqs, toks := l.usage("nope", now); reqs != 0 || toks != 0 {
		t.Errorf("unknown key usage = %d / %d, want 0 / 0", reqs, toks)
	}
}

// M5.2: requests per day is a UTC calendar-day cap — yesterday's
// requests never count, and the retry-after runs to midnight.
func TestKeyTrafficLimiterRPD(t *testing.T) {
	l := newKeyTrafficLimiter()
	lim := trafficLimits{RPD: 3}
	// 2026-11-18 10:00 UTC: three admit, the fourth is refused.
	day := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if ok, _, _ := l.tryAdmit("k1", 0, lim, day.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("request %d of the day must be admitted", i+1)
		}
	}
	ok, retryAfter, refusal := l.tryAdmit("k1", 0, lim, day.Add(3*time.Minute))
	if ok {
		t.Fatal("fourth request of the day must be refused")
	}
	if refusal.Limit != "requests_per_day" {
		t.Errorf("limit = %q, want requests_per_day", refusal.Limit)
	}
	// At 10:03 the wait to midnight is 13h57m, rounded up.
	want := 13*time.Hour + 57*time.Minute
	if retryAfter != want {
		t.Errorf("retryAfter = %v, want %v (to UTC midnight)", retryAfter, want)
	}
	// A different key is unaffected.
	if ok, _, _ := l.tryAdmit("k2", 0, lim, day.Add(3*time.Minute)); !ok {
		t.Fatal("a different key must not share the daily window")
	}
	// Tomorrow the counter resets: yesterday's three do not count.
	next := day.AddDate(0, 0, 1)
	for i := 0; i < 3; i++ {
		if ok, _, _ := l.tryAdmit("k1", 0, lim, next.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("request %d of the new day must be admitted", i+1)
		}
	}
	// usageToday agrees with the limiter's own day count.
	if got := l.usageToday("k1", next.Add(3*time.Minute)); got != 3 {
		t.Errorf("usageToday = %d, want 3", got)
	}
}

// M5.2: generic rules are sliding windows of any size — a 2-per-hour
// rule refuses the third request inside the hour and admits again
// once the oldest entry slides out; several rules compose.
func TestKeyTrafficLimiterGenericRules(t *testing.T) {
	l := newKeyTrafficLimiter()
	lim := trafficLimits{Rules: []config.RateLimitRule{{Limit: 2, WindowSeconds: 3600}}}
	now := time.Now()
	if ok, _, _ := l.tryAdmit("k1", 0, lim, now); !ok {
		t.Fatal("first request must be admitted")
	}
	if ok, _, _ := l.tryAdmit("k1", 0, lim, now.Add(time.Minute)); !ok {
		t.Fatal("second request must be admitted")
	}
	ok, retryAfter, refusal := l.tryAdmit("k1", 0, lim, now.Add(2*time.Minute))
	if ok {
		t.Fatal("third request inside the hour must be refused")
	}
	if refusal.Limit != "rate_limit_rule" || refusal.RuleLimit != 2 || refusal.RuleWindowSeconds != 3600 {
		t.Errorf("refusal = %+v, want rate_limit_rule 2/3600", refusal)
	}
	// Oldest entry (t0) ages out at t0+1h; at t0+2m the wait is 58m.
	if retryAfter != 58*time.Minute {
		t.Errorf("retryAfter = %v, want 58m", retryAfter)
	}
	// After the window slides past, admission works again.
	if ok, _, _ := l.tryAdmit("k1", 0, lim, now.Add(61*time.Minute)); !ok {
		t.Fatal("request after the window aged out must be admitted")
	}
	// Two rules compose: the tighter one fires first.
	l2 := newKeyTrafficLimiter()
	lim2 := trafficLimits{Rules: []config.RateLimitRule{
		{Limit: 10, WindowSeconds: 60},
		{Limit: 2, WindowSeconds: 3600},
	}}
	if ok, _, _ := l2.tryAdmit("k1", 0, lim2, now); !ok {
		t.Fatal("first request must be admitted under composed rules")
	}
	if ok, _, _ := l2.tryAdmit("k1", 0, lim2, now.Add(time.Second)); !ok {
		t.Fatal("second request must be admitted under composed rules")
	}
	ok, _, refusal = l2.tryAdmit("k1", 0, lim2, now.Add(2*time.Second))
	if ok || refusal.RuleWindowSeconds != 3600 {
		t.Errorf("third request: ok=%v refusal=%+v, want refusal on the 2/3600 rule", ok, refusal)
	}
}

// M5.2: the read-only peek refuses on the request-count axes
// (RPM, RPD, generic rules) without recording a slot, and never
// peeks the token axis.
func TestKeyTrafficLimiterWouldRefuseRPDAndRules(t *testing.T) {
	l := newKeyTrafficLimiter()
	lim := trafficLimits{RPD: 1, Rules: []config.RateLimitRule{{Limit: 1, WindowSeconds: 3600}}}
	now := time.Now()
	if refused, _, _ := l.wouldRefuse("k1", lim, now); refused {
		t.Fatal("peek must not refuse before any request")
	}
	if ok, _, _ := l.tryAdmit("k1", 0, lim, now); !ok {
		t.Fatal("first request must be admitted")
	}
	// The peek sees the RPD ceiling (checked before the rules) and
	// records nothing: a second peek still refuses, and the
	// request count is still 1.
	refused, _, refusal := l.wouldRefuse("k1", lim, now.Add(time.Second))
	if !refused || refusal.Limit != "requests_per_day" {
		t.Errorf("peek after 1 request: refused=%v refusal=%+v, want requests_per_day", refused, refusal)
	}
	if reqs, _ := l.usage("k1", now.Add(time.Second)); reqs != 1 {
		t.Errorf("peek recorded a slot: usage = %d requests, want 1", reqs)
	}
	// A key with only a TPM limit is never peek-refused (the token
	// axis stays post-body-only), even when the token budget is
	// already blown.
	l2 := newKeyTrafficLimiter()
	if ok, _, _ := l2.tryAdmit("k1", 6, trafficLimits{TPM: 10}, now); !ok {
		t.Fatal("setup: 6 tokens of a 10-token budget must be admitted")
	}
	if ok, _, _ := l2.tryAdmit("k1", 6, trafficLimits{TPM: 10}, now.Add(time.Second)); ok {
		t.Fatal("setup: 6 more tokens (12 > 10) must be refused")
	}
	if refused, _, _ := l2.wouldRefuse("k1", trafficLimits{TPM: 10}, now.Add(2*time.Second)); refused {
		t.Fatal("peek must not refuse on the token axis")
	}
}

// M5.3: TPM post-response true-up — the newest entry whose
// recorded tokens equal the estimate is corrected to the actuals.
func TestKeyTrafficLimiterTrueUp(t *testing.T) {
	l := newKeyTrafficLimiter()
	lim := trafficLimits{TPM: 100}
	now := time.Now()
	if ok, _, _ := l.tryAdmit("k1", 6, lim, now); !ok {
		t.Fatal("6 tokens of a 100-token budget must be admitted")
	}
	if !l.trueUp("k1", 6, 10, now.Add(time.Second)) {
		t.Fatal("trueUp must find the 6-token entry")
	}
	if _, toks := l.usage("k1", now.Add(time.Second)); toks != 10 {
		t.Errorf("usage tokens = %d, want 10 after true-up", toks)
	}
	// A wrong estimate matches nothing.
	if l.trueUp("k1", 6, 99, now.Add(2*time.Second)) {
		t.Error("trueUp with a stale estimate must return false")
	}
	if _, toks := l.usage("k1", now.Add(2*time.Second)); toks != 10 {
		t.Errorf("usage tokens = %d, want 10 (unchanged)", toks)
	}
	// Two same-estimate entries are corrected one at a time,
	// newest first, so the window sum stays right.
	l2 := newKeyTrafficLimiter()
	l2.tryAdmit("k1", 6, lim, now)
	l2.tryAdmit("k1", 6, lim, now.Add(time.Second))
	if !l2.trueUp("k1", 6, 8, now.Add(2*time.Second)) || !l2.trueUp("k1", 6, 8, now.Add(2*time.Second)) {
		t.Fatal("both same-estimate entries must be correctable")
	}
	if _, toks := l2.usage("k1", now.Add(2*time.Second)); toks != 16 {
		t.Errorf("usage tokens = %d, want 16 (8+8)", toks)
	}
	// An entry that aged out of the window cannot be trued up.
	l3 := newKeyTrafficLimiter()
	l3.tryAdmit("k1", 6, lim, now)
	if l3.trueUp("k1", 6, 10, now.Add(61*time.Second)) {
		t.Error("trueUp must return false once the entry aged out")
	}
	// An unknown key has nothing to correct.
	if l.trueUp("nope", 6, 10, now) {
		t.Error("trueUp on an unknown key must return false")
	}
}

// TestKeyTrafficLimiterParallelRace (M5.14): the traffic limiter
// is the exit-gate proof for TPM/RPM — tryAdmit holds the mutex
// across the check AND the record, so 20 parallel admissions
// against RPM 5 + TPM 100 admit exactly 5 (the request axis binds
// first) and no parallel pair can both read "4 admitted" and both
// record. A check-then-act limiter would admit all 20.
func TestKeyTrafficLimiterParallelRace(t *testing.T) {
	l := newKeyTrafficLimiter()
	lim := trafficLimits{RPM: 5, TPM: 100}
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	ok := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admitted, _, _ := l.tryAdmit("k1", 10, lim, now)
			ok <- admitted
		}()
	}
	wg.Wait()
	close(ok)
	admitted := 0
	for a := range ok {
		if a {
			admitted++
		}
	}
	if admitted != 5 {
		t.Errorf("parallel tryAdmit admitted = %d, want exactly 5 (RPM 5)", admitted)
	}
	// The token axis binds the same way: TPM 30 with 10 tokens
	// per request admits exactly 3.
	l2 := newKeyTrafficLimiter()
	lim2 := trafficLimits{TPM: 30}
	ok2 := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admitted, _, _ := l2.tryAdmit("k2", 10, lim2, now)
			ok2 <- admitted
		}()
	}
	wg.Wait()
	close(ok2)
	admitted2 := 0
	for a := range ok2 {
		if a {
			admitted2++
		}
	}
	if admitted2 != 3 {
		t.Errorf("parallel tryAdmit (TPM) admitted = %d, want exactly 3 (TPM 30 / 10 tokens)", admitted2)
	}
}
