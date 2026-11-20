package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M5.10: per-key access schedule / throttle delay / max sessions.
//
// The pure schedule function is tested with fixed clocks
// (deterministic); the HTTP tests build windows around now so
// they are day-independent. 2026-11-18 is a Wednesday.
func TestIsWithinAccessSchedule(t *testing.T) {
	wed := func(h, m int) time.Time { return time.Date(2026, 11, 18, h, m, 0, 0, time.UTC) }
	thu := func(h, m int) time.Time { return time.Date(2026, 11, 19, h, m, 0, 0, time.UTC) }
	day := func(h, m int) time.Time { return time.Date(2026, 11, 20, h, m, 0, 0, time.UTC) } // Friday
	work := &config.AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{1, 2, 3, 4, 5}, TZ: "UTC"}
	cases := []struct {
		name string
		s    *config.AccessScheduleConfig
		now  time.Time
		want bool
	}{
		{"nil schedule", nil, wed(10, 0), true},
		{"disabled", &config.AccessScheduleConfig{Enabled: false, From: "09:00", Until: "17:00", Days: []int{3}}, wed(3, 0), true},
		{"inside work hours", work, wed(10, 0), true},
		{"at start (inclusive)", work, wed(9, 0), true},
		{"at end (exclusive)", work, wed(17, 0), false},
		{"before start", work, wed(8, 59), false},
		{"wrong day (Saturday)", work, time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC), false},
		{"empty days never open", &config.AccessScheduleConfig{Enabled: true, From: "00:00", Until: "23:59", Days: nil, TZ: "UTC"}, wed(10, 0), false},
		{"invalid tz fails open", &config.AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{3}, TZ: "No/Such_Zone"}, wed(3, 0), true},
		// Overnight 22:00–06:00, days = Friday only.
		{"overnight Friday evening", &config.AccessScheduleConfig{Enabled: true, From: "22:00", Until: "06:00", Days: []int{5}, TZ: "UTC"}, day(23, 0), true},
		{"overnight Friday before start", &config.AccessScheduleConfig{Enabled: true, From: "22:00", Until: "06:00", Days: []int{5}, TZ: "UTC"}, day(21, 0), false},
		// The the reference router quirk, pinned: Saturday 01:00 is inside
		// the Friday 22:00–06:00 window by the clock, but the
		// days check names only the CURRENT day, so it is
		// closed. The pool schedules (M5.9) handle the wrap;
		// the per-key schedule matches the reference router exactly.
		{"overnight next morning is closed (the reference router quirk)", &config.AccessScheduleConfig{Enabled: true, From: "22:00", Until: "06:00", Days: []int{5}, TZ: "UTC"}, time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC), false},
		// ...unless the morning's own day is listed.
		{"overnight next morning open when its day is listed", &config.AccessScheduleConfig{Enabled: true, From: "22:00", Until: "06:00", Days: []int{5, 6}, TZ: "UTC"}, time.Date(2026, 11, 21, 1, 0, 0, 0, time.UTC), true},
		// Timezone: 09:00–17:00 Asia/Kolkata on Wednesday =
		// 03:30–11:30 UTC.
		{"timezone shift (Kolkata 09:00 = 03:30 UTC)", &config.AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{3}, TZ: "Asia/Kolkata"}, wed(4, 0), true},
		{"timezone shift (Kolkata 08:59 = 03:29 UTC)", &config.AccessScheduleConfig{Enabled: true, From: "09:00", Until: "17:00", Days: []int{3}, TZ: "Asia/Kolkata"}, wed(3, 0), false},
		{"thursday is a different day", work, thu(10, 0), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsWithinAccessSchedule(tc.s, tc.now); got != tc.want {
				t.Errorf("IsWithinAccessSchedule(%+v, %v) = %v, want %v", tc.s, tc.now, got, tc.want)
			}
		})
	}
}

func setKeyAccessSchedule(t *testing.T, s *keyBudgetSetup, sch *config.AccessScheduleConfig) {
	t.Helper()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].AccessSchedule = sch
		}
	}
}

func setKeyThrottleAndSessions(t *testing.T, s *keyBudgetSetup, delayMs, maxSessions int) {
	t.Helper()
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].ThrottleDelayMs = delayMs
			s.cfg.GatewayKeys[i].MaxSessions = maxSessions
		}
	}
}

// nowRelativeWindow builds a from/until pair around now in UTC (the
// TZ these tests declare on the schedule): from = now+fromOff, until =
// now+untilOff, formatted HH:MM. It must use UTC, not local time:
// on a machine whose local zone is IST, time.Now() is 5:30 ahead of
// UTC and a window built from it is evaluated as outside by the
// UTC-clocked gate (same family as the M816 TZ bug).
func nowRelativeWindow(fromOff, untilOff time.Duration) (from, until string) {
	now := time.Now().UTC()
	return now.Add(fromOff).Format("15:04"), now.Add(untilOff).Format("15:04")
}

func TestKeyAccessScheduleGateBlocksChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	from, until := nowRelativeWindow(2*time.Hour, 3*time.Hour)
	setKeyAccessSchedule(t, s, &config.AccessScheduleConfig{
		Enabled: true, From: from, Until: until, Days: []int{todayWeekday()}, TZ: "UTC",
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("chat status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	want := fmt.Sprintf("Access denied outside allowed hours (%s–%s UTC)", from, until)
	if !strings.Contains(rec.Body.String(), want) {
		t.Errorf("403 body = %s, want message %q", rec.Body.String(), want)
	}
	if !strings.Contains(rec.Body.String(), "key_access_schedule") {
		t.Errorf("403 body = %s, want error type key_access_schedule", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the schedule must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestKeyAccessScheduleGateInsideAllows(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	from, until := nowRelativeWindow(-1*time.Hour, 1*time.Hour)
	setKeyAccessSchedule(t, s, &config.AccessScheduleConfig{
		Enabled: true, From: from, Until: until, Days: []int{todayWeekday()}, TZ: "UTC",
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (inside the window): %s", rec.Code, rec.Body.String())
	}
}

func TestKeyAccessScheduleUpdateRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	// The key-update POST is admin-scoped: widen this test's own
	// fixture key to chat+admin.
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	day := todayWeekday()
	body := fmt.Sprintf(`{"access_schedule":{"enabled":true,"from":"09:00","until":"17:00","days":[%d],"tz":"UTC"},"throttle_delay_ms":50,"max_sessions":2}`, day)
	rec := doReq(t, srv, "POST", "/admin/keys/"+s.id, body, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("key update status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Key.AccessSchedule == nil || !out.Key.AccessSchedule.Enabled || out.Key.ThrottleDelayMs != 50 || out.Key.MaxSessions != 2 {
		t.Errorf("updated key = %+v, want schedule enabled + throttle 50 + max sessions 2", out.Key)
	}
	// Bad schedules are 400s, never stored.
	for _, bad := range []string{
		`{"access_schedule":{"enabled":true,"from":"","until":"17:00","days":[3],"tz":"UTC"}}`,
		`{"access_schedule":{"enabled":true,"from":"09:00","until":"17:00","days":[7],"tz":"UTC"}}`,
		`{"access_schedule":{"enabled":true,"from":"09:00","until":"17:00","days":[3],"tz":"No/Such_Zone"}}`,
		`{"throttle_delay_ms":-5}`,
		`{"max_sessions":-1}`,
	} {
		rec := doReq(t, srv, "POST", "/admin/keys/"+s.id, bad, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad key update status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	}
}

func TestKeyThrottleDelay(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	setKeyThrottleAndSessions(t, s, 250, 0)
	start := time.Now()
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	elapsed := time.Since(start)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("chat took %v with a 250ms throttle, want >= 250ms", elapsed)
	}
}

func TestKeyMaxSessionsBlocksSecond(t *testing.T) {
	s, _ := keyStatusSetup(t)
	setKeyThrottleAndSessions(t, s, 0, 1)
	eng := &blockingEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}},
		entered:    make(chan struct{}, 5),
		release:    make(chan struct{}),
	}
	srv := newTestServer(t, eng,
		WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}))
	// Request 1 enters the engine and blocks, holding the only
	// session slot.
	done1 := make(chan int, 1)
	go func() {
		rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
		done1 <- rec.Code
	}()
	select {
	case <-eng.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached the engine")
	}
	// Request 2: the cap is reached → 429, no provider call.
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_max_sessions") {
		t.Errorf("429 body = %s, want error type key_max_sessions", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 must carry a Retry-After header")
	}
	if eng.chatCalls() != 1 {
		t.Errorf("engine Chat calls = %d, want 1 (the refused request never reached it)", eng.chatCalls())
	}
	// Release request 1: it completes, and the slot frees.
	close(eng.release)
	select {
	case code := <-done1:
		if code != http.StatusOK {
			t.Errorf("first chat status = %d, want 200", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first request never completed after release")
	}
	// Request 3: the slot is free again.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("third chat status = %d, want 200 (slot freed): %s", rec.Code, rec.Body.String())
	}
}

func TestKeySessionTracker(t *testing.T) {
	tr := newKeySessionTracker()
	if !tr.tryEnter("k1", 1) {
		t.Fatal("first enter under cap 1: want true")
	}
	if tr.tryEnter("k1", 1) {
		t.Error("second enter under cap 1: want false")
	}
	if !tr.tryEnter("k2", 1) {
		t.Error("a different key must have its own cap")
	}
	tr.exit("k1")
	if !tr.tryEnter("k1", 1) {
		t.Error("enter after exit: want true")
	}
	if !tr.tryEnter("k1", 0) {
		t.Error("cap 0 means no cap: want true")
	}
	// Concurrent enters never exceed the cap.
	tr2 := newKeySessionTracker()
	var wg sync.WaitGroup
	admitted := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			admitted <- tr2.tryEnter("k", 3)
		}()
	}
	wg.Wait()
	close(admitted)
	n := 0
	for ok := range admitted {
		if ok {
			n++
		}
	}
	if n != 3 {
		t.Errorf("concurrent admitted = %d, want exactly 3", n)
	}
}
