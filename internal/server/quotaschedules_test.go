package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/webhooks"
)

// M5.9: quota schedules, server side — the pre-call gate on the
// chat path, the quota.exceeded webhook, and the dashboard
// GET/POST feeds.
//
// The tests drive the real Server + real ledger + counting
// engine. Schedules are built around time.Now() so the tests are
// day-independent: "today" is whatever weekday the run lands on.
func todayWeekday() int { return int(time.Now().Weekday()) }

func scheduleTestPool(t *testing.T, s *keyBudgetSetup, limit float64, threshold float64) {
	t.Helper()
	if err := s.led.SetQuotaPool(context.Background(), ledger.QuotaPool{
		ID: "pool1", Name: "team", SaturationThreshold: threshold,
		Dimensions:  []ledger.QuotaDimension{{Unit: "requests", Window: "daily", Limit: limit}},
		Allocations: []ledger.PoolAllocation{{KeyID: s.id, Weight: 100, Policy: "hard"}},
	}); err != nil {
		t.Fatal(err)
	}
}

func setSchedule(t *testing.T, s *keyBudgetSetup, sched ledger.QuotaSchedule) {
	t.Helper()
	if err := s.led.SetQuotaSchedule(context.Background(), sched); err != nil {
		t.Fatal(err)
	}
}

func scheduleChatBody() string {
	return `{"model":"unpriced-model","messages":[{"role":"user","content":"hello"}]}`
}

func TestQuotaScheduleGateBlackoutBlocksChat(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	scheduleTestPool(t, s, 10000, 0.5)
	// Block all day, every day: always in blackout.
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "blk", PoolID: "pool1", Days: []int{0, 1, 2, 3, 4, 5, 6},
		StartMinute: 0, EndMinute: 1440, Mode: ledger.ScheduleModeBlock, Enabled: true,
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_pool_schedule") {
		t.Errorf("429 body = %s, want error type key_pool_schedule", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "blackout") {
		t.Errorf("429 body = %s, want the blackout reason", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0 — the schedule must refuse before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestQuotaScheduleGateOutsideWindowBlocksWithRetryAfter(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	scheduleTestPool(t, s, 10000, 0.5)
	// Allow only tomorrow 09:00–17:00: today is outside the window.
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "allow", PoolID: "pool1", Days: []int{(todayWeekday() + 1) % 7},
		StartMinute: 9 * 60, EndMinute: 17 * 60, Mode: ledger.ScheduleModeAllow, Enabled: true,
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "outside") {
		t.Errorf("429 body = %s, want the outside-window reason", rec.Body.String())
	}
	ra := rec.Header().Get("Retry-After")
	secs, err := strconv.Atoi(ra)
	if err != nil || secs <= 0 {
		t.Errorf("Retry-After = %q, want positive seconds to the next window", ra)
	}
}

func TestQuotaScheduleGateAllowWindowPasses(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	scheduleTestPool(t, s, 10000, 0.5)
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "allow", PoolID: "pool1", Days: []int{todayWeekday()},
		StartMinute: 0, EndMinute: 1440, Mode: ledger.ScheduleModeAllow, Enabled: true,
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (open window): %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaScheduleGateReserveBlocks(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	// Pool limit 100/day, saturation threshold 1.0: the
	// fair-share gate never strict-blocks before 100%, so the
	// schedule's 20% keep-free floor is what trips.
	scheduleTestPool(t, s, 100, 1.0)
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "allow", PoolID: "pool1", Days: []int{todayWeekday()},
		StartMinute: 0, EndMinute: 1440, Mode: ledger.ScheduleModeAllow, Enabled: true,
		Reserves: []ledger.ReserveRule{{Window: "daily", Percent: 20}},
	})
	for i := 0; i < 81; i++ {
		s.seedPricedSpend(t, 1, 1)
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429 (81/100 leaves 19%% free): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_pool_schedule") || !strings.Contains(rec.Body.String(), "keep-free") {
		t.Errorf("429 body = %s, want key_pool_schedule + keep-free", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine Chat calls = %d, want 0", s.eng.chatCalls.Load())
	}
}

func TestQuotaScheduleGateBudgetBlocks(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	scheduleTestPool(t, s, 10000, 0.5)
	budget := 2.0
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "allow", PoolID: "pool1", Days: []int{todayWeekday()},
		StartMinute: 0, EndMinute: 1440, Mode: ledger.ScheduleModeAllow, Enabled: true,
		BudgetValue: &budget, BudgetUnit: "requests", BudgetWindow: "daily",
	})
	s.seedPricedSpend(t, 1, 1)
	s.seedPricedSpend(t, 1, 1)
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429 (2/2 budget spent): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "key_pool_schedule") || !strings.Contains(rec.Body.String(), "schedule-budget") {
		t.Errorf("429 body = %s, want key_pool_schedule + schedule-budget", rec.Body.String())
	}
}

func TestQuotaScheduleGateFiresWebhook(t *testing.T) {
	s, _ := keyStatusSetup(t)
	var mu sync.Mutex
	var raw []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		raw = body
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hook.Close()
	d := webhooks.New([]webhooks.Endpoint{{ID: "ops", URL: hook.URL, Events: []string{webhooks.QuotaExceededEvent}}}, hook.Client(), nil)
	srv := s.server(t, WithWebhooks(d))
	scheduleTestPool(t, s, 10000, 0.5)
	setSchedule(t, s, ledger.QuotaSchedule{
		ID: "blk", PoolID: "pool1", Days: []int{0, 1, 2, 3, 4, 5, 6},
		StartMinute: 0, EndMinute: 1440, Mode: ledger.ScheduleModeBlock, Enabled: true,
	})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("chat status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		delivered := raw != nil
		mu.Unlock()
		if delivered || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if raw == nil {
		t.Fatal("no quota.exceeded webhook delivered")
	}
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("webhook body is not JSON: %v", err)
	}
	if env["event"] != webhooks.QuotaExceededEvent {
		t.Errorf("event = %v, want %q", env["event"], webhooks.QuotaExceededEvent)
	}
	data, ok := env["data"].(map[string]any)
	if !ok || data["reason"] != "schedule-blackout" || data["pool_id"] != "pool1" || data["key_id"] != s.id {
		t.Errorf("data = %v, want reason schedule-blackout / pool1 / this key", env["data"])
	}
}

func TestQuotaScheduleGateNoPoolAllows(t *testing.T) {
	s, _ := keyStatusSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, "POST", "/v1/chat/completions", scheduleChatBody(), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (no pools): %s", rec.Code, rec.Body.String())
	}
}

func TestQuotaSchedulesDashboardRoundTrip(t *testing.T) {
	s, cfgPath := keyStatusSetup(t)
	srv := s.server(t, s.persistOpt(cfgPath))
	// The quota-schedules POST is admin-scoped: widen this test's
	// own fixture key to chat+admin (same pattern as the pool
	// tests).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	// The schedule needs a pool: create one via the pools POST.
	createPool := `{"id":"pool1","name":"team","saturation_threshold":0.5,` +
		`"dimensions":[{"unit":"requests","window":"daily","limit":100}],` +
		`"allocations":[{"key_id":"` + s.id + `","weight":100,"policy":"hard"}]}`
	if rec := doReq(t, srv, "POST", "/dashboard/quota-pools", createPool, s.raw); rec.Code != http.StatusOK {
		t.Fatalf("pool POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// Create a schedule via POST: allow today, all day.
	day := strconv.Itoa(todayWeekday())
	create := `{"id":"sched1","pool_id":"pool1","label":"work hours","days":[` + day + `],` +
		`"start_minute":0,"end_minute":1440,"mode":"allow","priority":1,"enabled":true,` +
		`"reserves":[{"window":"daily","percent":20}],"budget_value":90,"budget_unit":"requests","budget_window":"daily"}`
	rec := doReq(t, srv, "POST", "/dashboard/quota-schedules", create, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// GET lists it with a live "active" verdict (today, all day).
	rec = doReq(t, srv, "GET", "/dashboard/quota-schedules", "", s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out quotaSchedulesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Schedules) != 1 || out.Schedules[0].ID != "sched1" {
		t.Fatalf("GET schedules = %+v, want 1 schedule sched1", out.Schedules)
	}
	got := out.Schedules[0]
	if got.Verdict != "active" || got.Label != "work hours" || len(got.Reserves) != 1 || got.BudgetValue == nil || *got.BudgetValue != 90 {
		t.Errorf("schedule = %+v, want active / work hours / 1 reserve / budget 90", got)
	}
	// ?pool_id filters; an unknown pool 404s.
	rec = doReq(t, srv, "GET", "/dashboard/quota-schedules?pool_id=pool1", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Schedules) != 1 {
		t.Fatalf("GET ?pool_id=pool1 = %d schedules, want 1: %s", len(out.Schedules), rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/quota-schedules?pool_id=nope", "", s.raw)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET ?pool_id=nope status = %d, want 404", rec.Code)
	}
	// Bad bodies are rejected, never stored.
	for _, bad := range []string{
		`{"id":"s2","pool_id":"nope","days":[3],"start_minute":0,"end_minute":1440,"mode":"allow"}`,
		`{"id":"s2","pool_id":"pool1","days":[3],"start_minute":0,"end_minute":1440,"mode":"maybe"}`,
		`{"id":"s2","pool_id":"pool1","days":[7],"start_minute":0,"end_minute":1440,"mode":"allow"}`,
		`{"id":"s2","pool_id":"pool1","days":[3],"start_minute":0,"end_minute":1440,"mode":"allow","budget_unit":"requests"}`,
		`{"id":"s2","pool_id":"pool1","days":[3],"start_minute":0,"end_minute":1440,"mode":"allow","timezone":"No/Such_Zone"}`,
	} {
		rec := doReq(t, srv, "POST", "/dashboard/quota-schedules", bad, s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bad schedule POST status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
	}
	// Delete removes it.
	rec = doReq(t, srv, "POST", "/dashboard/quota-schedules", `{"id":"sched1","delete":true}`, s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "GET", "/dashboard/quota-schedules", "", s.raw)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Schedules) != 0 {
		t.Errorf("GET after delete = %d schedules, want 0", len(out.Schedules))
	}
}
