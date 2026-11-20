package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// fakeClient is a provider.Client whose TestKey behaviour is canned.
type fakeClient struct {
	testKeyErr error
	delay      time.Duration
}

func (f *fakeClient) TestKey(ctx context.Context) error {
	if f.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(f.delay):
		}
	}
	return f.testKeyErr
}

func (f *fakeClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("fakeClient: Chat not implemented")
}

func (f *fakeClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return nil, errors.New("fakeClient: ChatStream not implemented")
}

func (f *fakeClient) ListModels(ctx context.Context) ([]string, error) {
	return []string{"model-a"}, nil
}

// mockModelsServer serves a working /v1/models endpoint so reg.Add
// (which uses the registry's real client) succeeds.
func mockModelsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// addProvider registers one provider with the given plaintext key and
// returns its ID. Each provider gets its own mock server (distinct base
// URL) because the registry rejects duplicate base URLs.
func addProvider(t *testing.T, reg *provider.Registry, key string) string {
	t.Helper()
	srv := mockModelsServer(t)
	p, err := reg.Add(srv.URL, key)
	if err != nil {
		t.Fatalf("Add(%q): %v", key, err)
	}
	return p.ID
}

// fakeFactory returns canned clients keyed by the plaintext API key the
// checker passes in (the registry's decrypted key).
func fakeFactory(byKey map[string]*fakeClient) provider.ClientFactory {
	return func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (provider.Client, error) {
		if c, ok := byKey[apiKey]; ok {
			return c, nil
		}
		return &fakeClient{}, nil
	}
}

func TestCheckSuccessReportsStoredHint(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-ok")
	// Operator-set hint must be reported as-is, not invented.
	if err := reg.SetQuotaHint(id, 0.4); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-ok": {delay: 20 * time.Millisecond},
	}))
	st := c.Check(context.Background(), id)
	if !st.OK {
		t.Fatalf("OK = false, LastError = %q", st.LastError)
	}
	if st.ProviderID != id || st.ProviderName == "" {
		t.Errorf("identity = %q/%q", st.ProviderID, st.ProviderName)
	}
	if st.QuotaHint != 0.4 {
		t.Errorf("QuotaHint = %v, want stored 0.4", st.QuotaHint)
	}
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty", st.LastError)
	}
	if st.LatencyMs < 20 {
		t.Errorf("LatencyMs = %d, want >= 20 (fake delay)", st.LatencyMs)
	}
	if st.CheckedAt.IsZero() {
		t.Error("CheckedAt is zero")
	}
	if !st.QuotaResetsAt.IsZero() {
		t.Errorf("QuotaResetsAt = %v, want zero on success", st.QuotaResetsAt)
	}
}

func TestCheckSuccessUnknownHintStaysUnknown(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-unknown")
	c := New(reg, fakeFactory(map[string]*fakeClient{"sk-unknown": {}}))
	st := c.Check(context.Background(), id)
	if !st.OK {
		t.Fatalf("OK = false: %q", st.LastError)
	}
	if st.QuotaHint != provider.QuotaUnknown {
		t.Errorf("QuotaHint = %v, want -1 (unknown, not invented)", st.QuotaHint)
	}
}

func TestCheckQuotaFailure(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-429")
	if err := reg.SetQuotaHint(id, 0.5); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-429": {testKeyErr: &provider.RateLimitError{RetryAfter: 90 * time.Second}},
	}))
	st := c.Check(context.Background(), id)
	if st.OK {
		t.Fatal("OK = true for a 429 failure")
	}
	if !strings.Contains(strings.ToLower(st.LastError), "rate limited") {
		t.Errorf("LastError = %q, want rate-limit text", st.LastError)
	}
	// Failure must not change the stored hint in the Status itself.
	if st.QuotaHint != 0.5 {
		t.Errorf("QuotaHint = %v, want stored 0.5 unchanged", st.QuotaHint)
	}
	if st.QuotaResetsAt.IsZero() {
		t.Error("QuotaResetsAt is zero, want CheckedAt + 90s from Retry-After")
	} else if got := time.Until(st.QuotaResetsAt); got < 80*time.Second || got > 100*time.Second {
		t.Errorf("QuotaResetsAt in %v, want ~90s", got)
	}
	if strings.Contains(st.LastError, "sk-429") {
		t.Error("LastError leaks the API key")
	}
}

func TestCheckGenericFailureDoesNotTouchHint(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-500")
	if err := reg.SetQuotaHint(id, 0.5); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-500": {testKeyErr: &provider.UpstreamError{StatusCode: 500, Retryable: true, Body: "boom"}},
	}))
	st := c.Check(context.Background(), id)
	if st.OK {
		t.Fatal("OK = true for a 500 failure")
	}
	if st.QuotaHint != 0.5 {
		t.Errorf("QuotaHint = %v, want stored 0.5 unchanged", st.QuotaHint)
	}
	if !st.QuotaResetsAt.IsZero() {
		t.Errorf("QuotaResetsAt = %v, want zero for a generic failure", st.QuotaResetsAt)
	}
	// ApplyHints must leave a generic failure alone.
	c.ApplyHints(reg)
	if got, _ := reg.Get(id); got.QuotaHint != 0.5 {
		t.Errorf("after ApplyHints, QuotaHint = %v, want 0.5 untouched", got.QuotaHint)
	}
}

func TestCheckQuotaErrorTypeAlsoCounts(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-quota")
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-quota": {testKeyErr: &provider.QuotaError{Message: "insufficient_quota", StatusCode: 402}},
	}))
	st := c.Check(context.Background(), id)
	if st.OK {
		t.Fatal("OK = true for a quota failure")
	}
	c.ApplyHints(reg)
	if got, _ := reg.Get(id); got.QuotaHint != 0 {
		t.Errorf("after ApplyHints, QuotaHint = %v, want 0", got.QuotaHint)
	}
}

func TestApplyHints(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id429 := addProvider(t, reg, "sk-429b")
	idOK := addProvider(t, reg, "sk-okb")
	if err := reg.SetQuotaHint(idOK, 0.7); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-429b": {testKeyErr: &provider.RateLimitError{}},
		"sk-okb":  {},
	}))
	for _, id := range []string{id429, idOK} {
		c.Check(context.Background(), id)
	}
	c.ApplyHints(reg)
	if got, _ := reg.Get(id429); got.QuotaHint != 0 {
		t.Errorf("429 provider QuotaHint = %v, want 0", got.QuotaHint)
	}
	// Success must not overwrite an operator-set hint.
	if got, _ := reg.Get(idOK); got.QuotaHint != 0.7 {
		t.Errorf("healthy provider QuotaHint = %v, want 0.7 untouched", got.QuotaHint)
	}
}

func TestApplyHintsWithNoChecksIsNoop(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-x")
	if err := reg.SetQuotaHint(id, 0.3); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	New(reg, nil).ApplyHints(reg)
	if got, _ := reg.Get(id); got.QuotaHint != 0.3 {
		t.Errorf("QuotaHint = %v, want 0.3 untouched", got.QuotaHint)
	}
}

func TestCheckUnknownProvider(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	st := c.Check(context.Background(), "prov_does_not_exist")
	if st.OK {
		t.Fatal("OK = true for an unknown provider")
	}
	if !strings.Contains(st.LastError, "provider not found") {
		t.Errorf("LastError = %q, want 'provider not found'", st.LastError)
	}
	if st.QuotaHint != provider.QuotaUnknown {
		t.Errorf("QuotaHint = %v, want -1", st.QuotaHint)
	}
}

func TestCheckTimeout(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-slow")
	c := New(reg, fakeFactory(map[string]*fakeClient{
		// Blocks until the check context is cancelled.
		"sk-slow": {delay: time.Hour},
	}))
	c.timeout = 80 * time.Millisecond // same-package test seam; production default is 5s
	start := time.Now()
	st := c.Check(context.Background(), id)
	elapsed := time.Since(start)
	if st.OK {
		t.Fatal("OK = true for a timed-out check")
	}
	if !strings.Contains(st.LastError, "deadline exceeded") {
		t.Errorf("LastError = %q, want context deadline exceeded", st.LastError)
	}
	if elapsed > 2*time.Second {
		t.Errorf("Check took %v, want ~80ms timeout, not the fake's 1h delay", elapsed)
	}
	if st.LatencyMs < 80 || st.LatencyMs > 2000 {
		t.Errorf("LatencyMs = %d, want ~80", st.LatencyMs)
	}
}

func TestCheckNilFactoryUsesRegistryClient(t *testing.T) {
	// Nil factory -> reg.ClientFor -> real client against the mock server
	// used by Add, so the key test succeeds.
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-real")
	c := New(reg, nil)
	st := c.Check(context.Background(), id)
	if !st.OK {
		t.Fatalf("OK = false with nil factory: %q", st.LastError)
	}
}

func TestCheckAllConcurrentAndOrdered(t *testing.T) {
	reg := provider.NewRegistry(nil)
	ids := []string{
		addProvider(t, reg, "sk-a"),
		addProvider(t, reg, "sk-b"),
		addProvider(t, reg, "sk-c"),
		addProvider(t, reg, "sk-fast"),
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-a":    {delay: 600 * time.Millisecond},
		"sk-b":    {delay: 600 * time.Millisecond},
		"sk-c":    {delay: 600 * time.Millisecond},
		"sk-fast": {},
	}))
	start := time.Now()
	results := c.CheckAll(context.Background())
	elapsed := time.Since(start)
	if len(results) != len(ids) {
		t.Fatalf("CheckAll returned %d results, want %d", len(results), len(ids))
	}
	for i, st := range results {
		if st.ProviderID != ids[i] {
			t.Errorf("results[%d].ProviderID = %q, want %q (registry order)", i, st.ProviderID, ids[i])
		}
		if !st.OK {
			t.Errorf("results[%d] OK = false: %q", i, st.LastError)
		}
	}
	// Sequential would take ~1.8s (3 x 600ms); concurrent must be well under that.
	if elapsed > 1200*time.Millisecond {
		t.Errorf("CheckAll took %v, want < 1.2s (slow providers must run concurrently)", elapsed)
	}
}

func TestCheckAllSkipsDisabled(t *testing.T) {
	reg := provider.NewRegistry(nil)
	idOn := addProvider(t, reg, "sk-on")
	idOff := addProvider(t, reg, "sk-off")
	if err := reg.SetEnabled(idOff, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	c := New(reg, fakeFactory(map[string]*fakeClient{"sk-on": {}, "sk-off": {}}))
	results := c.CheckAll(context.Background())
	if len(results) != 1 || results[0].ProviderID != idOn {
		t.Fatalf("CheckAll = %+v, want only the enabled provider %q", results, idOn)
	}
}

func TestLastTracksResults(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-last")
	c := New(reg, fakeFactory(map[string]*fakeClient{"sk-last": {}}))
	if got := c.Last(); len(got) != 0 {
		t.Fatalf("Last() before any check = %v, want empty", got)
	}
	c.Check(context.Background(), id)
	got := c.Last()
	if len(got) != 1 || got[0].ProviderID != id || !got[0].OK {
		t.Fatalf("Last() = %+v, want the one successful check", got)
	}
	// Last() returns copies: mutating the returned slice must not corrupt state.
	got[0].OK = false
	if again := c.Last(); !again[0].OK {
		t.Error("Last() returned a live reference")
	}
}

func TestStartWatcher(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-watch")
	c := New(reg, fakeFactory(map[string]*fakeClient{
		"sk-watch": {testKeyErr: &provider.RateLimitError{}},
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.StartWatcher(ctx, 50*time.Millisecond)
	}()
	// The immediate first run should populate Last() and zero the hint.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Last()) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(c.Last()) != 1 {
		t.Fatal("watcher never populated Last()")
	}
	if got, _ := reg.Get(id); got.QuotaHint != 0 {
		t.Errorf("watcher QuotaHint = %v, want 0 after a 429 check", got.QuotaHint)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartWatcher did not return after ctx cancel")
	}
}

func TestStartWatcherZeroIntervalIsNoop(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.StartWatcher(context.Background(), 0)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("StartWatcher with interval <= 0 did not return immediately")
	}
	if len(c.Last()) != 0 {
		t.Errorf("zero-interval watcher ran checks: %+v", c.Last())
	}
}

func TestCheckDoesNotLeakKeyOnBuildFailure(t *testing.T) {
	// Factory that fails with an error mentioning nothing sensitive, and
	// a key that must never appear in any Status field.
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-topsecret-12345")
	c := New(reg, func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (provider.Client, error) {
		if apiKey != "sk-topsecret-12345" {
			t.Errorf("factory got apiKey %q, want the decrypted key", apiKey)
		}
		return nil, fmt.Errorf("cannot reach upstream")
	})
	st := c.Check(context.Background(), id)
	if st.OK {
		t.Fatal("OK = true when the factory failed")
	}
	for _, field := range []string{st.LastError, st.ProviderName, st.ProviderID} {
		if strings.Contains(field, "sk-topsecret-12345") {
			t.Errorf("Status field leaks the API key: %q", field)
		}
	}
}

// --- Probe history (slice 5) ---

// seedHistory records n samples for id directly, so window math can be
// tested without waiting for real probes.
func seedHistory(c *Checker, id string, at time.Time, ok bool, latencyMs int64, n int) {
	for i := 0; i < n; i++ {
		c.record(Status{ProviderID: id, OK: ok, LatencyMs: latencyMs, CheckedAt: at.Add(time.Duration(i) * time.Second)}, nil)
	}
}

func TestHistoryEmptyProviderReportsNoData(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	sum := c.History("no-such-provider")
	if sum.Samples != 0 {
		t.Errorf("Samples = %d, want 0", sum.Samples)
	}
	if sum.Uptime1h != nil || sum.Uptime24h != nil {
		t.Errorf("uptime = %v/%v, want nil (no data)", sum.Uptime1h, sum.Uptime24h)
	}
	if sum.P50Ms != nil || sum.P95Ms != nil {
		t.Errorf("latency = %v/%v, want nil (no data)", sum.P50Ms, sum.P95Ms)
	}
	if len(sum.Buckets) != 24 {
		t.Fatalf("len(Buckets) = %d, want 24", len(sum.Buckets))
	}
	for i, b := range sum.Buckets {
		if b.Up != 0 || b.Down != 0 || b.P50Ms != nil {
			t.Errorf("bucket %d = %+v, want empty", i, b)
		}
	}
}

func TestHistoryRecordsDownPeriodAndUptimeDrops(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-hist")
	c := New(reg, fakeFactory(map[string]*fakeClient{"sk-hist": {}}))
	now := time.Now().UTC()
	// 10 successful probes 30 min ago, then 10 failed probes just now.
	seedHistory(c, id, now.Add(-30*time.Minute), true, 40, 10)
	seedHistory(c, id, now.Add(-5*time.Minute), false, 5, 10)
	sum := c.historyAt(id, now)
	if sum.Samples != 20 {
		t.Fatalf("Samples = %d, want 20", sum.Samples)
	}
	if sum.Uptime24h == nil || *sum.Uptime24h != 50 {
		t.Errorf("Uptime24h = %v, want 50", sum.Uptime24h)
	}
	if sum.Uptime1h == nil || *sum.Uptime1h != 50 {
		t.Errorf("Uptime1h = %v, want 50 (10 up + 10 down in the hour)", sum.Uptime1h)
	}
	// Latency percentiles come from successful probes only: all 40 ms.
	if sum.P50Ms == nil || *sum.P50Ms != 40 {
		t.Errorf("P50Ms = %v, want 40", sum.P50Ms)
	}
	if sum.P95Ms == nil || *sum.P95Ms != 40 {
		t.Errorf("P95Ms = %v, want 40", sum.P95Ms)
	}
	// Buckets: the down probes land in the current hour's bucket.
	up, down := 0, 0
	for _, b := range sum.Buckets {
		up += b.Up
		down += b.Down
	}
	if up != 10 || down != 10 {
		t.Errorf("buckets total up/down = %d/%d, want 10/10", up, down)
	}
}

func TestHistoryWindowsWithNoProbesReportNoData(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	now := time.Now().UTC()
	// Only old probes: 2 h ago, so the 1 h window is empty but the
	// 24 h window is not.
	seedHistory(c, "p1", now.Add(-2*time.Hour), true, 30, 4)
	sum := c.historyAt("p1", now)
	if sum.Uptime1h != nil {
		t.Errorf("Uptime1h = %v, want nil (no probes in the last hour)", sum.Uptime1h)
	}
	if sum.Uptime24h == nil || *sum.Uptime24h != 100 {
		t.Errorf("Uptime24h = %v, want 100", sum.Uptime24h)
	}
}

func TestHistoryLatencyPercentilesMove(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	now := time.Now().UTC()
	// 100 successful probes with latencies 1..100 ms.
	for i := int64(1); i <= 100; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: i, CheckedAt: now.Add(time.Duration(i) * time.Millisecond)}, nil)
	}
	sum := c.historyAt("p1", now.Add(time.Second))
	if sum.P50Ms == nil || *sum.P50Ms != 50 {
		t.Errorf("P50Ms = %v, want 50", sum.P50Ms)
	}
	if sum.P95Ms == nil || *sum.P95Ms != 95 {
		t.Errorf("P95Ms = %v, want 95", sum.P95Ms)
	}
}

func TestHistoryRingIsBounded(t *testing.T) {
	reg := provider.NewRegistry(nil)
	c := New(reg, fakeFactory(nil))
	now := time.Now().UTC()
	// More probes than the cap, all within retention.
	for i := 0; i < MaxHistorySamples+500; i++ {
		c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 1, CheckedAt: now.Add(time.Duration(i) * time.Millisecond)}, nil)
	}
	sum := c.History("p1")
	if sum.Samples != MaxHistorySamples {
		t.Errorf("Samples = %d, want capped at %d", sum.Samples, MaxHistorySamples)
	}
	// Samples older than 48 h are pruned on the next record.
	c.record(Status{ProviderID: "p1", OK: true, LatencyMs: 1, CheckedAt: now.Add(49 * time.Hour)}, nil)
	sum = c.historyAt("p1", now.Add(49*time.Hour))
	if sum.Samples != 1 {
		t.Errorf("Samples after retention prune = %d, want 1", sum.Samples)
	}
}

func TestCheckAppendsToHistory(t *testing.T) {
	reg := provider.NewRegistry(nil)
	id := addProvider(t, reg, "sk-hist2")
	c := New(reg, fakeFactory(map[string]*fakeClient{"sk-hist2": {}}))
	if got := c.Check(context.Background(), id); !got.OK {
		t.Fatalf("Check OK = false: %q", got.LastError)
	}
	if got := c.Check(context.Background(), id); !got.OK {
		t.Fatalf("second Check OK = false: %q", got.LastError)
	}
	sum := c.History(id)
	if sum.Samples != 2 {
		t.Errorf("Samples = %d, want 2 after two Checks", sum.Samples)
	}
	if sum.Uptime24h == nil || *sum.Uptime24h != 100 {
		t.Errorf("Uptime24h = %v, want 100", sum.Uptime24h)
	}
}
