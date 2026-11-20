package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Per-key rolling 5-hour USD budgets (CHECKLIST 139-140): the
// server refuses a capped key BEFORE any provider is called, and
// the dashboard feeds report the live window position.
//
// The tests drive the real Server + real ledger (SQLite) + a
// counting engine: "refused pre-call" is proven by the engine's
// call count staying at zero, not by the absence of an error.

func usdPer1M(v float64) *float64 { return &v }

// keyBudgetSetup builds one issued gateway key, a real ledger, and
// a price book where "priced-model" costs $10/1M in + $30/1M out
// (so 1,000 prompt + 1,000 completion tokens = $0.04) and every
// other model is unpriced.
type keyBudgetSetup struct {
	cfg  *config.Config
	led  *ledger.Ledger
	book *config.PriceBook
	eng  *countingEngine
	raw  string
	id   string
}

func newKeyBudgetSetup(t *testing.T) *keyBudgetSetup {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	key, raw, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = led.Close() })
	book := config.NewPriceBook()
	book.Entries["priced-model"] = config.ModelPrice{InputUSDPer1M: usdPer1M(10), OutputUSDPer1M: usdPer1M(30)}
	return &keyBudgetSetup{
		cfg:  cfg,
		led:  led,
		book: book,
		eng:  &countingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}}},
		raw:  raw,
		id:   key.ID,
	}
}

func (s *keyBudgetSetup) server(t *testing.T, extra ...Option) *Server {
	t.Helper()
	opts := []Option{WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book),
		WithLedgerPrices(ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}})}
	return newTestServer(t, s.eng, append(opts, extra...)...)
}

func (s *keyBudgetSetup) seedPricedSpend(t *testing.T, prompt, completion int) {
	t.Helper()
	prices := ledger.Prices{ledger.PriceKey{Model: "priced-model"}: ledger.ModelPrice{PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6}}
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: "priced-model",
		PromptTokens: prompt, CompletionTokens: completion, SplitKnown: true, OK: true,
	}, prices); err != nil {
		t.Fatalf("seed spend: %v", err)
	}
}

func kbChatBody(model string, stream bool) string {
	if stream {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}],"stream":true}`
	}
	return `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
}

func TestKeyBudgetCappedKeyRefusedPreCall(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 0.01, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != 402 {
		t.Fatalf("capped key: status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the capped key must be refused before any provider is called", s.eng.chatCalls.Load())
	}
	// Streaming takes the same pre-call gate.
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", true), s.raw)
	if rec.Code != 402 {
		t.Fatalf("capped key (stream): status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	if s.eng.streamCalls.Load() != 0 {
		t.Fatalf("engine ChatStream calls = %d, want 0", s.eng.streamCalls.Load())
	}
}

func TestKeyBudgetUncappedKeyRoutes(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != 200 {
		t.Fatalf("uncapped key: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
}

func TestKeyBudgetUnpricedModelFailClosed400(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("mystery-model", false), s.raw)
	if rec.Code != 400 {
		t.Fatalf("unpriced model under fail_closed: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mystery-model") {
		t.Fatalf("400 body = %s, want it to name the unpriced model", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the unpriced block happens before any provider is called", s.eng.chatCalls.Load())
	}
}

func TestKeyBudgetUnpricedModelCountAsZeroRoutes(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10, ledger.UnpricedCountAsZero); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("mystery-model", false), s.raw)
	if rec.Code != 200 {
		t.Fatalf("unpriced model under count_as_zero: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
}

func TestKeyBudgetAutoModelResolvesForCheck(t *testing.T) {
	s := newKeyBudgetSetup(t)
	if err := s.led.SetKeyBudget(context.Background(), s.id, 10, ledger.UnpricedFailClosed); err != nil {
		t.Fatal(err)
	}
	codingPrompt := "Write a Python function to reverse a string"
	// "auto/coding" resolves to the priced model: the unpriced
	// check sees the concrete model, so the request routes.
	priced := autorouter.New(autorouter.Config{
		TierModels:   map[autorouter.Tier][]string{autorouter.TierCoding: {"priced-model"}},
		DefaultModel: "priced-model",
	})
	srv := s.server(t, WithAutoRouter(priced))
	rec := doReq(t, srv, "POST", "/v1/chat/completions", `{"model":"auto/coding","messages":[{"role":"user","content":"`+codingPrompt+`"}]}`, s.raw)
	if rec.Code != 200 {
		t.Fatalf("auto/coding -> priced model: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// The same virtual model resolving to an unpriced model is a
	// 400 naming that model — the check follows the resolution,
	// it does not bless the virtual name.
	unpriced := autorouter.New(autorouter.Config{
		TierModels:   map[autorouter.Tier][]string{autorouter.TierCoding: {"mystery-model"}},
		DefaultModel: "mystery-model",
	})
	srv2 := s.server(t, WithAutoRouter(unpriced))
	rec = doReq(t, srv2, "POST", "/v1/chat/completions", `{"model":"auto/coding","messages":[{"role":"user","content":"`+codingPrompt+`"}]}`, s.raw)
	if rec.Code != 400 {
		t.Fatalf("auto/coding -> unpriced model: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "mystery-model") {
		t.Fatalf("400 body = %s, want it to name the resolved unpriced model", rec.Body.String())
	}
}

// keyBudgetWindowFeed mirrors one ledger.KeyBudgetWindowStatus in
// the GET /dashboard/key-budgets payload (B2 read-back).
type keyBudgetWindowFeed struct {
	LimitUSD *float64 `json:"limit_usd"`
	SpentUSD float64  `json:"spent_usd"`
}

func (s *keyBudgetSetup) setDWM(t *testing.T, interval ledger.BudgetResetInterval, daily, weekly, monthly *float64) {
	t.Helper()
	kb := ledger.KeyBudget{
		KeyID: s.id, LimitUSD: 1000, UnpricedPolicy: ledger.UnpricedFailClosed,
		DailyLimitUSD: daily, WeeklyLimitUSD: weekly, MonthlyLimitUSD: monthly,
		WarningThreshold: 0.8, ResetInterval: interval, ResetTime: "00:00",
	}
	if err := s.led.SetKeyBudgetConfig(context.Background(), kb); err != nil {
		t.Fatalf("set D/W/M budget: %v", err)
	}
}

func usdPtr(v float64) *float64 { return &v }

// seedUnpricedUsage records one successful attempt whose price is
// unknown (a model with no price anywhere): the row the B2
// closed-period rule must see, never a $0-priced row.
func (s *keyBudgetSetup) seedUnpricedUsage(t *testing.T, model string) {
	t.Helper()
	if _, err := s.led.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID: s.id, Provider: "p", Model: model,
		PromptTokens: 10, CompletionTokens: 0, SplitKnown: true, OK: true,
	}, ledger.Prices{}); err != nil {
		t.Fatalf("seed unpriced usage: %v", err)
	}
}

// keyBudgetsFeed mirrors the GET /dashboard/key-budgets payload.
type keyBudgetsFeed struct {
	Budgets []struct {
		KeyID          string              `json:"key_id"`
		Name           string              `json:"name"`
		HasBudget      bool                `json:"has_budget"`
		LimitUSD       float64             `json:"limit_usd"`
		UnpricedPolicy string              `json:"unpriced_policy"`
		SpentUSD       float64             `json:"spent_usd"`
		RemainingUSD   *float64            `json:"remaining_usd"`
		UnknownEvents  int                 `json:"unknown_events"`
		UnpricedModels []string            `json:"unpriced_models"`
		ActiveWindow   string              `json:"active_window"`
		Daily          keyBudgetWindowFeed `json:"daily"`
		Weekly         keyBudgetWindowFeed `json:"weekly"`
		Monthly        keyBudgetWindowFeed `json:"monthly"`
	} `json:"budgets"`
	RollingWindowH float64 `json:"rolling_window_hours"`
}

func getKeyBudgets(t *testing.T, srv *Server) keyBudgetsFeed {
	t.Helper()
	rec := doReq(t, srv, "GET", "/dashboard/key-budgets", "", "")
	if rec.Code != 200 {
		t.Fatalf("GET key-budgets: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var feed keyBudgetsFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("GET key-budgets is not JSON: %v", err)
	}
	return feed
}

func TestKeyBudgetsFeedRoundTrip(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// The key-budgets POST is admin-scoped (M1.1): widen this
	// test's own fixture key to chat+admin. It stays the single
	// key in the config, so the feed still lists exactly one row.
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := s.cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t, WithConfigPersist(func() error { return s.cfg.Save(path) }))

	feed := getKeyBudgets(t, srv)
	if feed.RollingWindowH != 5 {
		t.Errorf("rolling window = %v hours, want 5", feed.RollingWindowH)
	}
	if len(feed.Budgets) != 1 {
		t.Fatalf("budgets = %d rows, want 1 (the one issued key)", len(feed.Budgets))
	}
	row := feed.Budgets[0]
	if row.KeyID != s.id || row.Name != "test" {
		t.Errorf("row = %+v, want key %s named test", row, s.id)
	}
	if row.HasBudget || row.RemainingUSD != nil {
		t.Errorf("row before any budget = %+v, want has_budget=false, remaining=nil", row)
	}

	// Set a budget through the feed: the ledger row, the live
	// feed, and the config file must all agree afterwards.
	rec := doReq(t, srv, "POST", "/dashboard/key-budgets", `{"key_id":"`+s.id+`","limit_usd":5,"unpriced_policy":"count_as_zero"}`, s.raw)
	if rec.Code != 200 {
		t.Fatalf("POST set: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var setResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &setResp); err != nil {
		t.Fatalf("POST set is not JSON: %v", err)
	}
	if setResp["persisted_to_config"] != true {
		t.Errorf("POST set response = %v, want persisted_to_config=true", setResp)
	}
	feed = getKeyBudgets(t, srv)
	row = feed.Budgets[0]
	if !row.HasBudget || row.LimitUSD != 5 || row.UnpricedPolicy != "count_as_zero" {
		t.Errorf("row after set = %+v, want limit $5 count_as_zero", row)
	}
	if row.RemainingUSD == nil || *row.RemainingUSD != 5 {
		t.Errorf("remaining = %v, want $5", row.RemainingUSD)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "key_budgets") || !strings.Contains(string(saved), s.id) {
		t.Errorf("saved config does not carry the budget: %s", saved)
	}

	// Unknown spend shows up as unknown, never as $0 spent.
	s.seedPricedSpend(t, 1000, 1000) // $0.04 priced
	feed = getKeyBudgets(t, srv)
	if feed.Budgets[0].SpentUSD != 0.04 {
		t.Errorf("spent = $%.4f, want $0.04", feed.Budgets[0].SpentUSD)
	}

	// Delete: the feed, the ledger, and the file all drop it.
	rec = doReq(t, srv, "POST", "/dashboard/key-budgets", `{"key_id":"`+s.id+`","delete":true}`, s.raw)
	if rec.Code != 200 {
		t.Fatalf("POST delete: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	feed = getKeyBudgets(t, srv)
	if feed.Budgets[0].HasBudget {
		t.Errorf("row after delete still has a budget: %+v", feed.Budgets[0])
	}
	saved, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), s.id) && strings.Contains(string(saved), "key_budgets") {
		t.Errorf("saved config still carries the deleted budget: %s", saved)
	}
}

func TestKeyBudgetsFeedValidation(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// The key-budgets POST is admin-scoped (M1.1): widen this
	// test's own fixture key to chat+admin (see RoundTrip).
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	srv := s.server(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"unknown key", `{"key_id":"gk_doesnotexist","limit_usd":1}`, 404},
		{"bad policy", `{"key_id":"` + s.id + `","limit_usd":1,"unpriced_policy":"bogus"}`, 400},
		{"negative limit", `{"key_id":"` + s.id + `","limit_usd":-1}`, 400},
		{"missing key id", `{"limit_usd":1}`, 400},
	} {
		rec := doReq(t, srv, "POST", "/dashboard/key-budgets", tc.body, s.raw)
		if rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", tc.name, rec.Code, tc.want, rec.Body.String())
		}
	}
	// None of the rejected updates left a budget behind.
	if feed := getKeyBudgets(t, srv); feed.Budgets[0].HasBudget {
		t.Errorf("rejected updates left a budget: %+v", feed.Budgets[0])
	}
}

// B2 (M10): per-key daily / weekly / monthly USD limits — set and
// read back through GET/POST /dashboard/key-budgets, enforced in
// the spend path (402 naming the breached window, pre-call), and
// fail-closed on unpriced usage inside a set window's period.
// The boundary rule: UTC calendar periods from the ledger — daily
// [today 00:00, tomorrow 00:00), weekly [Monday 00:00, next Monday
// 00:00), monthly [1st 00:00, next 1st 00:00) at the default reset
// time — so a seeded $0.04 "today" sits inside all three windows
// at once, and yesterday's spend would not count toward today.

func TestKeyBudgetDWMSetAndReadBack(t *testing.T) {
	s := newKeyBudgetSetup(t)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := s.cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t, WithConfigPersist(func() error { return s.cfg.Save(path) }))

	rec := doReq(t, srv, "POST", "/dashboard/key-budgets", `{"key_id":"`+s.id+`","limit_usd":1000,"daily_limit_usd":5,"weekly_limit_usd":20,"monthly_limit_usd":50,"reset_interval":"weekly"}`, s.raw)
	if rec.Code != 200 {
		t.Fatalf("POST set D/W/M: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	feed := getKeyBudgets(t, srv)
	row := feed.Budgets[0]
	if !row.HasBudget || row.LimitUSD != 1000 {
		t.Errorf("row after set = %+v, want 5h limit $1000", row)
	}
	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"daily", row.Daily.LimitUSD, 5},
		{"weekly", row.Weekly.LimitUSD, 20},
		{"monthly", row.Monthly.LimitUSD, 50},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("%s limit = %v, want $%.0f", tc.name, tc.got, tc.want)
		}
	}
	if row.ActiveWindow != "weekly" {
		t.Errorf("active window = %q, want weekly (interval weekly, weekly set)", row.ActiveWindow)
	}
	// Today's priced spend shows up in all three windows.
	s.seedPricedSpend(t, 1000, 1000) // $0.04
	feed = getKeyBudgets(t, srv)
	row = feed.Budgets[0]
	if row.Daily.SpentUSD != 0.04 || row.Weekly.SpentUSD != 0.04 || row.Monthly.SpentUSD != 0.04 {
		t.Errorf("spend = daily $%.4f weekly $%.4f monthly $%.4f, want $0.04 in all three", row.Daily.SpentUSD, row.Weekly.SpentUSD, row.Monthly.SpentUSD)
	}
	// The config file carries the same D/W/M limits (parsed, not
	// string-matched — JSON spacing is the encoder's business).
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var savedCfg struct {
		KeyBudgets map[string]config.KeyBudgetConfig `json:"key_budgets"`
	}
	if err := json.Unmarshal(saved, &savedCfg); err != nil {
		t.Fatalf("saved config is not JSON: %v", err)
	}
	cfgRow, ok := savedCfg.KeyBudgets[s.id]
	if !ok {
		t.Fatalf("saved config has no budget for %s: %s", s.id, saved)
	}
	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"daily", cfgRow.DailyLimitUSD, 5},
		{"weekly", cfgRow.WeeklyLimitUSD, 20},
		{"monthly", cfgRow.MonthlyLimitUSD, 50},
	} {
		if tc.got == nil || *tc.got != tc.want {
			t.Errorf("saved config %s limit = %v, want $%.0f", tc.name, tc.got, tc.want)
		}
	}
}

func TestKeyBudgetDWMValidation(t *testing.T) {
	s := newKeyBudgetSetup(t)
	for i := range s.cfg.GatewayKeys {
		if s.cfg.GatewayKeys[i].ID == s.id {
			s.cfg.GatewayKeys[i].Scopes = []string{"chat", "admin"}
		}
	}
	srv := s.server(t)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"zero daily", `{"key_id":"` + s.id + `","limit_usd":1000,"daily_limit_usd":0}`},
		{"zero weekly", `{"key_id":"` + s.id + `","limit_usd":1000,"weekly_limit_usd":0}`},
		{"zero monthly", `{"key_id":"` + s.id + `","limit_usd":1000,"monthly_limit_usd":0}`},
		{"negative daily", `{"key_id":"` + s.id + `","limit_usd":1000,"daily_limit_usd":-1}`},
		{"negative weekly", `{"key_id":"` + s.id + `","limit_usd":1000,"weekly_limit_usd":-1}`},
		{"negative monthly", `{"key_id":"` + s.id + `","limit_usd":1000,"monthly_limit_usd":-1}`},
	} {
		rec := doReq(t, srv, "POST", "/dashboard/key-budgets", tc.body, s.raw)
		if rec.Code != 400 {
			t.Errorf("%s: status = %d, want 400: %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if feed := getKeyBudgets(t, srv); feed.Budgets[0].HasBudget {
		t.Errorf("rejected D/W/M updates left a budget: %+v", feed.Budgets[0])
	}
}

func TestKeyBudgetDWMEnforcementRefusesOverCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval ledger.BudgetResetInterval
		daily    *float64
		weekly   *float64
		monthly  *float64
		window   string
	}{
		{"daily caps even when interval is weekly", ledger.BudgetResetWeekly, usdPtr(0.01), nil, nil, "daily"},
		{"weekly caps even when interval is daily", ledger.BudgetResetDaily, nil, usdPtr(0.01), nil, "weekly"},
		{"monthly caps even when interval is daily", ledger.BudgetResetDaily, nil, nil, usdPtr(0.01), "monthly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newKeyBudgetSetup(t)
			srv := s.server(t)
			s.setDWM(t, tc.interval, tc.daily, tc.weekly, tc.monthly)
			s.seedPricedSpend(t, 1000, 1000) // $0.04 of a $0.01 cap
			rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
			if rec.Code != 402 {
				t.Fatalf("over-cap %s: status = %d, want 402: %s", tc.window, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.window) {
				t.Fatalf("402 body = %s, want it to name the %s window", rec.Body.String(), tc.window)
			}
			if s.eng.chatCalls.Load() != 0 {
				t.Fatalf("engine Chat calls = %d, want 0 — refused before any provider is called", s.eng.chatCalls.Load())
			}
		})
	}
	t.Run("under every D/W/M cap routes", func(t *testing.T) {
		s := newKeyBudgetSetup(t)
		srv := s.server(t)
		s.setDWM(t, ledger.BudgetResetWeekly, usdPtr(10), usdPtr(50), usdPtr(100))
		s.seedPricedSpend(t, 1000, 1000) // $0.04
		rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
		if rec.Code != 200 {
			t.Fatalf("under-cap: status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		if s.eng.chatCalls.Load() != 1 {
			t.Fatalf("engine Chat calls = %d, want 1", s.eng.chatCalls.Load())
		}
	})
}

func TestKeyBudgetDWMUnpricedFailClosed(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	// Daily $10 cap, fail_closed, no priced spend — but one
	// unpriced attempt earlier today closes the day for ALL
	// models, exactly like the 5h budget's month-closed rule.
	s.setDWM(t, ledger.BudgetResetDaily, usdPtr(10), nil, nil)
	s.seedUnpricedUsage(t, "ghost-model")
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != 400 {
		t.Fatalf("unpriced usage inside the day: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ghost-model") {
		t.Fatalf("400 body = %s, want it to name the unpriced model", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "daily") {
		t.Fatalf("400 body = %s, want it to name the closed daily window", rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 0 {
		t.Fatalf("engine Chat calls = %d, want 0 — the closed day refuses before any provider is called", s.eng.chatCalls.Load())
	}
	// count_as_zero opts out: same usage, the request routes.
	s2 := newKeyBudgetSetup(t)
	kb := ledger.KeyBudget{
		KeyID: s2.id, LimitUSD: 1000, UnpricedPolicy: ledger.UnpricedCountAsZero,
		DailyLimitUSD: usdPtr(10), WarningThreshold: 0.8,
		ResetInterval: ledger.BudgetResetDaily, ResetTime: "00:00",
	}
	if err := s2.led.SetKeyBudgetConfig(context.Background(), kb); err != nil {
		t.Fatal(err)
	}
	s2.seedUnpricedUsage(t, "ghost-model")
	srv2 := s2.server(t)
	rec = doReq(t, srv2, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s2.raw)
	if rec.Code != 200 {
		t.Fatalf("count_as_zero with unpriced usage in the day: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

func TestKeyBudgetDWMPerKeyIsolation(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// A second key with the same scopes and no budget at all.
	keyB, rawB, err := config.IssueKey("test-b", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(keyB); err != nil {
		t.Fatal(err)
	}
	srv := s.server(t)
	// Key A only: $0.01 daily cap, $0.04 spent today.
	s.setDWM(t, ledger.BudgetResetDaily, usdPtr(0.01), nil, nil)
	s.seedPricedSpend(t, 1000, 1000)
	rec := doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), s.raw)
	if rec.Code != 402 {
		t.Fatalf("key A over its daily cap: status = %d, want 402: %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", kbChatBody("priced-model", false), rawB)
	if rec.Code != 200 {
		t.Fatalf("key B (no budget): status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1 (key B's request only)", s.eng.chatCalls.Load())
	}
}
