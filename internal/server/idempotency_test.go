// G17 Area 11 — Idempotency-Key billing safety tests.
//
// The store is exercised directly (temp-dir SQLite) plus the
// Server Begin/Finish check functions end to end. The billing
// proof at the store level is the row count: a replayed key
// creates no second row, so a second ledger write keyed off the
// same execution cannot happen — the repeat never re-executes.
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func idemTestStore(t *testing.T) *IdempotencyStore {
	t.Helper()
	st, err := OpenIdempotencyStore(filepath.Join(t.TempDir(), "idempotency.db"))
	if err != nil {
		t.Fatalf("open idempotency store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func idemHash(body string) string {
	return idempotencyRequestHash(http.MethodPost, "/v1/chat/completions", []byte(body))
}

func idemCount(t *testing.T, st *IdempotencyStore) int {
	t.Helper()
	n, err := st.Count(context.Background())
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestIdempotencyConcurrentDuplicateClaimSingleProceed(t *testing.T) {
	// G17 re-review nit 2: two concurrent identical-key claims
	// must not both proceed. Each goroutine holds its OWN store
	// handle to the same file (the live shape: idempotencyBegin
	// opens a fresh handle per request), so a deferred SQLite
	// transaction alone cannot exclude the race — the
	// process-wide claim mutex must. Handles are opened
	// sequentially first so only the Claim itself races.
	path := filepath.Join(t.TempDir(), "idempotency.db")
	const n = 16
	stores := make([]*IdempotencyStore, n)
	for i := range stores {
		st, err := OpenIdempotencyStore(path)
		if err != nil {
			t.Fatalf("open store %d: %v", i, err)
		}
		t.Cleanup(func() { _ = st.Close() })
		stores[i] = st
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	states := make([]IdempotencyState, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			claim, err := stores[i].Claim(context.Background(), "gw-a", "key-race", idemHash(`{"model":"m"}`))
			if err != nil {
				errs[i] = err
				return
			}
			states[i] = claim.State
		}(i)
	}
	close(start)
	wg.Wait()
	proceed, inflight := 0, 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: claim err = %v", i, errs[i])
		}
		switch states[i] {
		case IdemProceed:
			proceed++
		case IdemInflight:
			inflight++
		default:
			t.Fatalf("goroutine %d: state = %v, want IdemProceed or IdemInflight", i, states[i])
		}
	}
	if proceed != 1 || inflight != n-1 {
		t.Fatalf("proceed = %d, inflight = %d, want 1 and %d", proceed, inflight, n-1)
	}
	if got := idemCount(t, stores[0]); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

func TestIdempotencyClaimThenReplayNoSecondRow(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	hash := idemHash(body)

	// First request: fresh key claims, executes, completes.
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("first claim = %+v, err = %v; want IdemProceed", claim, err)
	}
	respBody := []byte(`{"id":"chatcmpl-1","choices":[]}`)
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, respBody, "application/json", 0.04, true); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n := idemCount(t, st); n != 1 {
		t.Fatalf("rows after first execution = %d, want 1", n)
	}

	// Retry with the same key: the ORIGINAL response replays —
	// same status, body and cost — and no second row appears,
	// so there is nothing to bill a second time.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if claim.State != IdemReplay {
		t.Fatalf("retry claim state = %v, want IdemReplay", claim.State)
	}
	if claim.Record == nil || claim.Record.StatusCode != http.StatusOK ||
		string(claim.Record.ResponseBody) != string(respBody) || claim.Record.CostUSD != 0.04 || !claim.Record.HasCost {
		t.Fatalf("replayed record = %+v, want the original 200 response at $0.04", claim.Record)
	}
	if n := idemCount(t, st); n != 1 {
		t.Fatalf("rows after replay = %d, want 1 (no second billing row)", n)
	}
	// Lookup agrees with Claim.
	rec, found, err := st.Lookup(ctx, "gw-a", "key-1")
	if err != nil || !found || rec == nil || string(rec.ResponseBody) != string(respBody) {
		t.Fatalf("lookup = %+v, found = %v, err = %v", rec, found, err)
	}
}

func TestIdempotencyDifferentKeyExecutes(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash("same-body")
	for _, key := range []string{"key-1", "key-2"} {
		claim, err := st.Claim(ctx, "gw-a", key, hash)
		if err != nil || claim.State != IdemProceed {
			t.Fatalf("claim %q = %+v, err = %v; want IdemProceed", key, claim, err)
		}
	}
	if n := idemCount(t, st); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

func TestIdempotencySameKeyDifferentGatewayKeyExecutes(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash("same-body")
	// Key from gateway-key A completes...
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("gw-a claim = %+v, err = %v", claim, err)
	}
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, []byte(`{"a":1}`), "application/json", 0.01, true); err != nil {
		t.Fatalf("complete gw-a: %v", err)
	}
	// ...but the SAME key under gateway-key B executes normally:
	// keys are scoped per gateway key, never replayed across keys.
	claim, err = st.Claim(ctx, "gw-b", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("gw-b claim = %+v, err = %v; want IdemProceed (no cross-key replay)", claim, err)
	}
}

func TestIdempotencyExpiredKeyExecutes(t *testing.T) {
	st := idemTestStore(t)
	st.TTL = time.Millisecond
	ctx := context.Background()
	hash := idemHash("body")
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("claim = %+v, err = %v", claim, err)
	}
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, []byte(`{}`), "application/json", 0, true); err != nil {
		t.Fatalf("complete: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	// Expired: executes normally again instead of replaying.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("expired claim = %+v, err = %v; want IdemProceed", claim, err)
	}
}

func TestIdempotencySameKeyDifferentBodyConflicts(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	claim, err := st.Claim(ctx, "gw-a", "key-1", idemHash("body-one"))
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("first claim = %+v, err = %v", claim, err)
	}
	// Same key, different request while in-flight: conflict.
	claim, err = st.Claim(ctx, "gw-a", "key-1", idemHash("body-two"))
	if err != nil || claim.State != IdemConflict {
		t.Fatalf("different-body claim = %+v, err = %v; want IdemConflict", claim, err)
	}
	// Same after completion, too: never replay a different
	// request's response.
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, []byte(`{}`), "application/json", 0, false); err != nil {
		t.Fatalf("complete: %v", err)
	}
	claim, err = st.Claim(ctx, "gw-a", "key-1", idemHash("body-two"))
	if err != nil || claim.State != IdemConflict {
		t.Fatalf("different-body claim after completion = %+v, err = %v; want IdemConflict", claim, err)
	}
}

func TestIdempotencyInflightRepeatDoesNotExecute(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash("body")
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("first claim = %+v, err = %v", claim, err)
	}
	// Still running (no Complete): the repeat is in-flight, not
	// a second execution.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemInflight {
		t.Fatalf("repeat claim = %+v, err = %v; want IdemInflight", claim, err)
	}
}

func TestIdempotencyInflightStaleClaimMayBeReclaimed(t *testing.T) {
	st := idemTestStore(t)
	st.InflightStale = time.Millisecond
	ctx := context.Background()
	hash := idemHash("body")
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("first claim = %+v, err = %v", claim, err)
	}
	time.Sleep(5 * time.Millisecond)
	// The first attempt is assumed crashed past the stale window:
	// the repeat re-claims and executes.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("stale claim = %+v, err = %v; want IdemProceed", claim, err)
	}
}

func TestIdempotencyCompleteWithoutClaimErrors(t *testing.T) {
	st := idemTestStore(t)
	if err := st.Complete(context.Background(), "gw-a", "never-claimed", http.StatusOK, []byte(`{}`), "application/json", 0, false); err == nil {
		t.Fatal("complete without claim: want an error, got nil")
	}
}

func TestIdempotencyValidateKey(t *testing.T) {
	if err := ValidateIdempotencyKey("key-1"); err != nil {
		t.Errorf("valid key: %v", err)
	}
	if err := ValidateIdempotencyKey(strings.Repeat("x", 256)); err == nil {
		t.Error("256-byte key: want an error, got nil")
	}
	if err := ValidateIdempotencyKey("bad\nkey"); err == nil {
		t.Error("control-character key: want an error, got nil")
	}
}

func TestIdempotencyRequestHash(t *testing.T) {
	a := idemHash("body")
	if a != idemHash("body") {
		t.Error("hash is not deterministic")
	}
	if a == idemHash("other-body") {
		t.Error("different bodies hash identically")
	}
	if a == idempotencyRequestHash(http.MethodGet, "/v1/chat/completions", []byte("body")) {
		t.Error("different methods hash identically")
	}
}

func TestIdempotencyTooLargeBodyNotStoredButBilledOnce(t *testing.T) {
	st := idemTestStore(t)
	ctx := context.Background()
	hash := idemHash("body")
	claim, err := st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemProceed {
		t.Fatalf("claim = %+v, err = %v", claim, err)
	}
	big := make([]byte, idempotencyMaxBodyBytes+1)
	if err := st.Complete(ctx, "gw-a", "key-1", http.StatusOK, big, "application/json", 0.5, true); err != nil {
		t.Fatalf("complete oversized: %v", err)
	}
	rec, found, err := st.Lookup(ctx, "gw-a", "key-1")
	if err != nil || !found || rec == nil {
		t.Fatalf("lookup = %+v, found = %v, err = %v", rec, found, err)
	}
	if !rec.BodyTooLarge || len(rec.ResponseBody) != 0 {
		t.Fatalf("record = %+v: want body_too_large with no stored body", rec)
	}
	// A repeat replays the key (no second execution) but the
	// replay itself refuses: the caller must 409 with a new key.
	claim, err = st.Claim(ctx, "gw-a", "key-1", hash)
	if err != nil || claim.State != IdemReplay {
		t.Fatalf("repeat claim = %+v, err = %v; want IdemReplay", claim, err)
	}
	if idempotencyReplay(httptest.NewRecorder(), claim.Record) {
		t.Error("replay of an oversized body: want false (caller must 409), got true")
	}
}

func TestIdempotencyPruneExpired(t *testing.T) {
	st := idemTestStore(t)
	st.TTL = time.Millisecond
	ctx := context.Background()
	for _, key := range []string{"key-1", "key-2"} {
		claim, err := st.Claim(ctx, "gw-a", key, idemHash("body"))
		if err != nil || claim.State != IdemProceed {
			t.Fatalf("claim %q = %+v, err = %v", key, claim, err)
		}
		if err := st.Complete(ctx, "gw-a", key, http.StatusOK, []byte(`{}`), "application/json", 0, false); err != nil {
			t.Fatalf("complete %q: %v", key, err)
		}
	}
	time.Sleep(5 * time.Millisecond)
	n, err := st.PruneExpired(ctx)
	if err != nil || n != 2 {
		t.Fatalf("prune = %d, err = %v; want 2", n, err)
	}
	if n := idemCount(t, st); n != 0 {
		t.Fatalf("rows after prune = %d, want 0", n)
	}
}

// --- Server Begin / Finish / Replay -------------------------------------

func idemBeginReq(t *testing.T, key string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

func TestIdempotencyBeginFinishReplay(t *testing.T) {
	s, srv := analyticsSetup(t)
	ctx := context.Background()
	body := `{"model":"m","messages":[]}`
	hash := idemHash(body)

	// No key: executes normally, unprotected.
	begin, err := srv.idempotencyBegin(ctx, s.id, idemBeginReq(t, ""), hash)
	if err != nil || begin.State != IdemNone {
		t.Fatalf("no-key begin = %+v, err = %v; want IdemNone", begin, err)
	}
	// No gateway key: never replayed (cannot scope safely).
	begin, err = srv.idempotencyBegin(ctx, "", idemBeginReq(t, "key-1"), hash)
	if err != nil || begin.State != IdemNone {
		t.Fatalf("anonymous begin = %+v, err = %v; want IdemNone", begin, err)
	}
	// Invalid key: a client error, not a silent proceed.
	if _, err := srv.idempotencyBegin(ctx, s.id, idemBeginReq(t, strings.Repeat("x", 256)), hash); err == nil {
		t.Fatal("oversized key: want an error, got nil")
	}

	// First request with a key: proceeds; Finish stores it.
	begin, err = srv.idempotencyBegin(ctx, s.id, idemBeginReq(t, "key-1"), hash)
	if err != nil || begin.State != IdemProceed || begin.Store == nil {
		t.Fatalf("first begin = %+v, err = %v; want IdemProceed with a store", begin, err)
	}
	respBody := []byte(`{"id":"chatcmpl-9"}`)
	srv.idempotencyFinish(ctx, begin, http.StatusOK, respBody, "application/json", 0.04, true)

	// Retry: replays the ORIGINAL response — same body, same cost.
	begin, err = srv.idempotencyBegin(ctx, s.id, idemBeginReq(t, "key-1"), hash)
	if err != nil || begin.State != IdemReplay || begin.Record == nil {
		t.Fatalf("retry begin = %+v, err = %v; want IdemReplay", begin, err)
	}
	rec := httptest.NewRecorder()
	if !idempotencyReplay(rec, begin.Record) {
		t.Fatal("replay: want true, got false")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != string(respBody) {
		t.Fatalf("replayed response = %d %q, want 200 %q", rec.Code, rec.Body.String(), respBody)
	}
	if got := rec.Header().Get("X-Idempotency-Replayed"); got != "true" {
		t.Errorf("X-Idempotency-Replayed = %q, want true", got)
	}
	if got := rec.Header().Get("X-Codvian-Cost-USD"); got != "0.0400000000" {
		t.Errorf("replayed X-Codvian-Cost-USD = %q, want 0.0400000000", got)
	}

	// Same key, different gateway key: proceeds (no cross-key replay).
	begin, err = srv.idempotencyBegin(ctx, "gw-other", idemBeginReq(t, "key-1"), hash)
	if err != nil || begin.State != IdemProceed {
		t.Fatalf("other-key begin = %+v, err = %v; want IdemProceed", begin, err)
	}
	// Release the claim: this begin is never executed, so its
	// store must be closed here — a Proceed begin owns its
	// handle until Finish or Release, and a leaked handle
	// keeps idempotency.db open, failing TempDir cleanup on
	// Windows (the file cannot be deleted while open).
	srv.idempotencyRelease(ctx, begin)
	// Finish is a no-op for a non-Proceed begin (no panic, no store use).
	srv.idempotencyFinish(ctx, idempotencyBegin{State: IdemReplay}, http.StatusOK, []byte(`{}`), "application/json", 0, false)
}

func TestIdempotencyBeginNoDataDirFailsOpen(t *testing.T) {
	// A server with no config (hence no data dir) cannot open the
	// store: Begin fails OPEN — the request executes normally
	// instead of being refused for a safety-store outage.
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "hi"}})
	begin, err := srv.idempotencyBegin(context.Background(), "gw-a", idemBeginReq(t, "key-1"), idemHash("body"))
	if err != nil || begin.State != IdemNone {
		t.Fatalf("begin without data dir = %+v, err = %v; want IdemNone (fail open)", begin, err)
	}
}
