package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeChecker is an AsyncChecker the test controls: it waits on
// released (when set) before answering, so a test can prove the
// stream started before the verdict landed.
type fakeChecker struct {
	verdict  AsyncVerdict
	err      error
	released chan struct{}
	seen     chan string // receives the checked text, buffered
}

func (f *fakeChecker) Check(ctx context.Context, text string) (AsyncVerdict, error) {
	if f.seen != nil {
		select {
		case f.seen <- text:
		default:
		}
	}
	if f.released != nil {
		select {
		case <-f.released:
		case <-ctx.Done():
			return AsyncVerdict{}, ctx.Err()
		}
	}
	return f.verdict, f.err
}

func TestConfigValidateAsyncBlockingKind(t *testing.T) {
	// async + a blocking kind (pii/secret/injection) is an honest
	// config error: a secret must never reach the provider while
	// a verdict is still in flight.
	for _, kind := range []string{KindPII, KindSecret, KindInjection} {
		cfg := Config{External: []ExternalConfig{
			{Name: "slow-" + kind, Kind: kind, Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
		}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("async + blocking kind %q must fail validation", kind)
		} else if !strings.Contains(err.Error(), "must stay synchronous") {
			t.Fatalf("async + %q error should name the rule, got: %v", kind, err)
		}
	}
	// The same kinds in sync mode are fine, and a classifier kind
	// is fine in either mode.
	ok := []Config{
		{External: []ExternalConfig{{Name: "a", Kind: KindPII, Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"}}},
		{External: []ExternalConfig{{Name: "a", Kind: KindSecret, Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"}}},
		{External: []ExternalConfig{{Name: "a", Kind: KindInjection, Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"}}},
		{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"}}},
		{External: []ExternalConfig{{Name: "a", Kind: "classifier", Mode: ModeSync, Endpoint: "https://classifier.internal/check"}}},
		{External: []ExternalConfig{{Name: "a", Kind: "classifier", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check", TimeoutMs: 250}}},
		{},
	}
	for i, cfg := range ok {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config %d should validate: %v", i, err)
		}
	}
	bad := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty name", Config{External: []ExternalConfig{{Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"}}}, "name must not be empty"},
		{"duplicate name", Config{External: []ExternalConfig{
			{Name: "a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
			{Name: "a", Kind: "spam", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
		}}, "defined twice"},
		{"empty kind", Config{External: []ExternalConfig{{Name: "a", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"}}}, "kind must not be empty"},
		{"empty mode", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Endpoint: "http://127.0.0.1:9/check"}}}, "mode must be"},
		{"unknown mode", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: "eventually", Endpoint: "http://127.0.0.1:9/check"}}}, "mode must be"},
		{"empty endpoint", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: ModeAsync}}}, "endpoint must not be empty"},
		{"non-http endpoint", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "ftp://x/y"}}}, "not an http(s) URL"},
		{"endpoint no host", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http:///check"}}}, "not an http(s) URL"},
		{"negative timeout", Config{External: []ExternalConfig{{Name: "a", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check", TimeoutMs: -1}}}, "must not be negative"},
	}
	for _, tc := range bad {
		if err := tc.cfg.Validate(); err == nil {
			t.Fatalf("%s: validation must fail", tc.name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error should contain %q, got: %v", tc.name, tc.want, err)
		}
	}
}

func TestHTTPClassifierVerdicts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("classifier must be POSTed to, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("classifier content type = %q, want application/json", ct)
		}
		var body struct {
			Text string `json:"text"`
		}
		if err := jsonDecode(r, &body); err != nil {
			t.Errorf("classifier request body: %v", err)
		}
		if body.Text == "" {
			t.Errorf("classifier got an empty text")
		}
		switch {
		case strings.Contains(body.Text, "block-me"):
			w.Write([]byte(`{"verdict": "block", "reason": "toxicity"}`))
		case strings.Contains(body.Text, "weird"):
			w.Write([]byte(`{"verdict": "maybe"}`))
		case strings.Contains(body.Text, "broken"):
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`boom`))
		default:
			w.Write([]byte(`{"verdict": "allow"}`))
		}
	}))
	defer srv.Close()

	c := NewHTTPClassifier(srv.URL, time.Second)
	ctx := context.Background()
	v, err := c.Check(ctx, "hello world")
	if err != nil || v.Blocked {
		t.Fatalf("allow verdict: verdict=%+v err=%v", v, err)
	}
	v, err = c.Check(ctx, "please block-me now")
	if err != nil || !v.Blocked || v.Reason != "toxicity" {
		t.Fatalf("block verdict: verdict=%+v err=%v", v, err)
	}
	if _, err := c.Check(ctx, "weird verdict"); err == nil {
		t.Fatalf("unknown verdict string must be an error, not a silent allow")
	}
	if _, err := c.Check(ctx, "broken endpoint"); err == nil {
		t.Fatalf("non-200 classifier status must be an error")
	}
	// Timeout: a classifier slower than the timeout is an error,
	// never waited on forever.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Write([]byte(`{"verdict": "allow"}`))
	}))
	defer slow.Close()
	fast := NewHTTPClassifier(slow.URL, 20*time.Millisecond)
	if _, err := fast.Check(ctx, "hello"); err == nil {
		t.Fatalf("classifier past its timeout must error")
	}
}

// jsonDecode is a small helper so the test server above stays
// readable; it decodes one JSON value from the request body.
func jsonDecode(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func TestAsyncRunStreamStartsBeforeVerdict(t *testing.T) {
	// The core timing claim of async mode: the run starts (and the
	// provider call would start with it) BEFORE the verdict lands.
	// The checker is gated on a channel the test holds shut; while
	// it is shut, no verdict exists — yet StartAsync has already
	// returned and the store shows the guardrail configured.
	release := make(chan struct{})
	seen := make(chan string, 1)
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "toxicity", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store.RegisterChecker("toxicity", &fakeChecker{verdict: AsyncVerdict{Blocked: true, Reason: "toxicity"}, released: release, seen: seen}); err != nil {
		t.Fatalf("register checker: %v", err)
	}
	run := store.StartAsync("some prompt", true)
	if run == nil {
		t.Fatalf("StartAsync must return a run when an async guardrail is configured")
	}
	// No verdict yet: the checker has not even been released, and
	// BlockVerdict must say so (the stream is free to start).
	if _, blocked := run.BlockVerdict(); blocked {
		t.Fatalf("no verdict has landed yet, but BlockVerdict says blocked")
	}
	select {
	case <-run.Blocked():
		t.Fatalf("Blocked channel must not fire before the verdict lands")
	case <-time.After(50 * time.Millisecond):
		// expected: still waiting on the slow classifier
	}
	// The checker did receive the text while the "stream" was
	// already running — the check ran concurrently, not first.
	select {
	case text := <-seen:
		if text != "some prompt" {
			t.Fatalf("checker saw %q, want the request text", text)
		}
	case <-time.After(time.Second):
		t.Fatalf("checker never received the text")
	}
	// Release the verdict: BLOCK. The run records it as
	// blocked_after_start — the stream was in flight (the first
	// byte reached the client, MarkDelivered below) when the
	// verdict landed — and wakes the cut channel.
	run.MarkDelivered()
	close(release)
	select {
	case res := <-run.Blocked():
		if !res.Blocked || res.Kind != "toxicity" {
			t.Fatalf("Blocked channel gave %+v, want the toxicity block", res)
		}
	case <-time.After(time.Second):
		t.Fatalf("Blocked channel did not fire after the BLOCK verdict")
	}
	run.Wait()
	if res, blocked := run.BlockVerdict(); !blocked || res.Reason != "toxicity" {
		t.Fatalf("BlockVerdict after release: blocked=%v res=%+v", blocked, res)
	}
	stats := store.Stats()
	if len(stats.Async) != 1 || stats.Async[0].BlockedAfterStart != 1 {
		t.Fatalf("async stats = %+v, want 1 blocked_after_start", stats.Async)
	}
	if len(stats.AsyncEvents) != 1 || stats.AsyncEvents[0].Outcome != AsyncOutcomeBlockedAfterStart {
		t.Fatalf("async events = %+v, want one blocked_after_start", stats.AsyncEvents)
	}
}

func TestAsyncRunAllowAndError(t *testing.T) {
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "ok-guard", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
		{Name: "down-guard", Kind: "spam", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store.RegisterChecker("ok-guard", &fakeChecker{verdict: AsyncVerdict{Blocked: false}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := store.RegisterChecker("down-guard", &fakeChecker{err: errors.New("classifier down")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	run := store.StartAsync("hello", true)
	run.Wait()
	if _, blocked := run.BlockVerdict(); blocked {
		t.Fatalf("no BLOCK verdict landed, but BlockVerdict says blocked")
	}
	stats := store.Stats()
	byName := map[string]AsyncGuardrailView{}
	for _, v := range stats.Async {
		byName[v.Name] = v
	}
	if byName["ok-guard"].Allowed != 1 {
		t.Fatalf("ok-guard stats = %+v, want 1 allowed", byName["ok-guard"])
	}
	if byName["down-guard"].Errors != 1 {
		t.Fatalf("down-guard stats = %+v, want 1 error (fail open, recorded)", byName["down-guard"])
	}
}

func TestAsyncRunNonStreamingOutcomes(t *testing.T) {
	// Non-streaming: a BLOCK before delivery withholds
	// (blocked_before_delivery); a BLOCK after MarkDelivered is
	// too late (blocked_after_delivery); a BLOCK after Finish is
	// too late even when nothing was delivered (the request ended).
	release := make(chan struct{})
	newStore := func() (*Store, *fakeChecker) {
		s := NewStoreWith(Config{External: []ExternalConfig{
			{Name: "g", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
		}})
		f := &fakeChecker{verdict: AsyncVerdict{Blocked: true}, released: release}
		if err := s.RegisterChecker("g", f); err != nil {
			t.Fatalf("register: %v", err)
		}
		return s, f
	}

	// Before delivery: withhold.
	store, _ := newStore()
	run := store.StartAsync("prompt", false)
	close(release)
	run.Wait()
	if got := store.Stats().Async[0].BlockedBeforeDelivery; got != 1 {
		t.Fatalf("before-delivery block: blocked_before_delivery=%d, want 1", got)
	}

	// After delivery: too late.
	release2 := make(chan struct{})
	store2 := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "g", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store2.RegisterChecker("g", &fakeChecker{verdict: AsyncVerdict{Blocked: true}, released: release2}); err != nil {
		t.Fatalf("register: %v", err)
	}
	run2 := store2.StartAsync("prompt", false)
	run2.MarkDelivered()
	close(release2)
	run2.Wait()
	if got := store2.Stats().Async[0].BlockedAfterDelivery; got != 1 {
		t.Fatalf("after-delivery block: blocked_after_delivery=%d, want 1", got)
	}

	// After Finish: too late even with nothing delivered (the
	// request already ended in an engine error).
	release3 := make(chan struct{})
	store3 := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "g", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store3.RegisterChecker("g", &fakeChecker{verdict: AsyncVerdict{Blocked: true}, released: release3}); err != nil {
		t.Fatalf("register: %v", err)
	}
	run3 := store3.StartAsync("prompt", false)
	run3.Finish()
	close(release3)
	run3.Wait()
	if got := store3.Stats().Async[0].BlockedAfterDelivery; got != 1 {
		t.Fatalf("after-finish block: blocked_after_delivery=%d, want 1", got)
	}
}

func TestAsyncRunNoCheckerIsRecordedError(t *testing.T) {
	// A configured async guardrail with no registered checker
	// records an error verdict — the dashboard shows the guardrail
	// as broken (errors > 0, has_checker false) instead of
	// pretending it ran.
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "ghost", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	run := store.StartAsync("prompt", true)
	if run == nil {
		t.Fatalf("StartAsync must return a run for a configured async guardrail, even with no checker")
	}
	run.Wait()
	v := store.Stats().Async[0]
	if v.HasChecker {
		t.Fatalf("ghost guardrail must report has_checker=false")
	}
	if v.Errors != 1 {
		t.Fatalf("ghost guardrail errors=%d, want 1 recorded error", v.Errors)
	}
}

func TestSyncGuardrailsNeverAsync(t *testing.T) {
	// Sync-mode entries never produce an async run, and async-mode
	// entries never run in the sync check: the two paths are
	// disjoint by mode, so a sync guardrail can never be deferred
	// past the provider call.
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "sync-guard", Kind: "toxicity", Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"},
		{Name: "async-guard", Kind: "spam", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store.RegisterChecker("sync-guard", &fakeChecker{verdict: AsyncVerdict{Blocked: true}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := store.RegisterChecker("async-guard", &fakeChecker{verdict: AsyncVerdict{Blocked: true}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The async run only contains the async entry. The stream is
	// in flight (first byte delivered), so its BLOCK lands as
	// blocked_after_start.
	run := store.StartAsync("prompt", true)
	run.MarkDelivered()
	run.Wait()
	if got := store.Stats().Async; len(got) != 2 {
		t.Fatalf("stats async views = %d, want 2 (both entries listed)", len(got))
	}
	// The sync check only runs the sync entry — and blocks.
	results := store.CheckExternalSync(context.Background(), "prompt")
	if len(results) != 1 || results[0].Name != "sync-guard" || !results[0].Blocked {
		t.Fatalf("sync check results = %+v, want only the sync-guard block", results)
	}
	// The async entry was not consumed by the sync check: its
	// verdict is still only the one from the run.
	for _, v := range store.Stats().Async {
		if v.Name == "async-guard" && v.BlockedAfterStart != 1 {
			t.Fatalf("async-guard stats = %+v, want 1 blocked_after_start from the run only", v)
		}
		if v.Name == "sync-guard" && v.BlockedBeforeDelivery != 1 {
			t.Fatalf("sync-guard stats = %+v, want 1 blocked_before_delivery from the sync check", v)
		}
	}
}

func TestCheckExternalSyncFailsOpenOnError(t *testing.T) {
	// A sync classifier that errors (or is missing) fails OPEN —
	// the request continues — and the error is recorded, so a
	// classifier outage is visible instead of taking the gateway
	// down with it.
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "down", Kind: "toxicity", Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"},
		{Name: "ghost", Kind: "spam", Mode: ModeSync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store.RegisterChecker("down", &fakeChecker{err: errors.New("boom")}); err != nil {
		t.Fatalf("register: %v", err)
	}
	results := store.CheckExternalSync(context.Background(), "prompt")
	if len(results) != 2 {
		t.Fatalf("sync check results = %d, want 2", len(results))
	}
	for _, r := range results {
		if r.Blocked {
			t.Fatalf("error/missing checker must fail open, got blocked: %+v", r)
		}
		if r.Err == nil {
			t.Fatalf("error/missing checker must carry the error: %+v", r)
		}
	}
	for _, v := range store.Stats().Async {
		if v.Errors != 1 {
			t.Fatalf("%s errors=%d, want 1 recorded error", v.Name, v.Errors)
		}
	}
}

func TestStoreStatsAsyncEmptyState(t *testing.T) {
	// No external guardrail configured: the feed carries empty
	// async lists, never null — the dashboard renders its honest
	// empty state.
	stats := NewStore().Stats()
	if stats.Async == nil || len(stats.Async) != 0 {
		t.Fatalf("async views must be an empty list, got %+v", stats.Async)
	}
	if stats.AsyncEvents == nil || len(stats.AsyncEvents) != 0 {
		t.Fatalf("async events must be an empty list, got %+v", stats.AsyncEvents)
	}
	// A store with entries but no verdicts yet lists them with
	// zero counts.
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "g", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	stats = store.Stats()
	if len(stats.Async) != 1 || stats.Async[0].Mode != "async" || stats.Async[0].Allowed != 0 {
		t.Fatalf("unrun async guardrail view = %+v, want mode=async, zero counts", stats.Async)
	}
}

func TestRegisterCheckerRejectsUnknownAndDuplicate(t *testing.T) {
	store := NewStoreWith(Config{External: []ExternalConfig{
		{Name: "g", Kind: "toxicity", Mode: ModeAsync, Endpoint: "http://127.0.0.1:9/check"},
	}})
	if err := store.RegisterChecker("nope", &fakeChecker{}); err == nil {
		t.Fatalf("registering a checker for an unconfigured name must fail")
	}
	if err := store.RegisterChecker("g", &fakeChecker{}); err != nil {
		t.Fatalf("first registration must succeed: %v", err)
	}
	if err := store.RegisterChecker("g", &fakeChecker{}); err == nil {
		t.Fatalf("second registration of the same name must fail")
	}
}
