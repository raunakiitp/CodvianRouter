package mcptools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// cancelMidCallMockMCPServer sleeps 3 s before answering slow_tool
// and answers fast_tool at once, so a test can cancel a slow_tool
// call mid-flight and then prove the next call is unaffected: no
// orphaned reader left racing on the shared stream, no 30 s hang,
// and no other tool's result handed back as fast_tool's.
const cancelMidCallMockMCPServer = `import sys, json, time
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {"name": "cancel-mock", "version": "0"}}})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [
            {"name": "slow_tool", "description": "Sleeps, then answers", "inputSchema": {"type": "object", "properties": {}}},
            {"name": "fast_tool", "description": "Answers at once", "inputSchema": {"type": "object", "properties": {}}}]}})
    elif method == "tools/call":
        name = req.get("params", {}).get("name", "")
        if name == "slow_tool":
            time.sleep(3)
            out = "slow-result"
        elif name == "fast_tool":
            out = "fast-result"
        else:
            send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32602, "message": "unknown tool: " + name}})
            continue
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": out}]}})
`

// slowInitMockMCPServer sleeps 3 s before answering initialize on
// every spawn, so a test can cancel a ListTools call mid-handshake
// and then prove the cancellation was not recorded as a sticky
// start failure: a fresh context must list tools (after a respawn
// and another 3 s initialize sleep).
const slowInitMockMCPServer = `import sys, json, time
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    req = json.loads(line)
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        time.sleep(3)
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2025-03-26", "capabilities": {}, "serverInfo": {"name": "slow-init-mock", "version": "0"}}})
    elif method == "notifications/initialized":
        continue
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "echo", "description": "Echo", "inputSchema": {"type": "object", "properties": {}}}]}})
    elif method == "tools/call":
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": "echo-ok"}]}})
`

func TestStdioServerCancelMidCallThenFastCallSucceeds(t *testing.T) {
	// Regression (MCP bug-hunt H3): cancelling a tools/call
	// mid-flight used to return ctx.Err() without killing the
	// process, leaving the read goroutine blocked in ReadBytes
	// on the shared stream. The next CallTool then spawned a
	// second reader on the same bufio.Reader (-race: concurrent
	// ReadSlice/ReadBytes) and could be handed slow_tool's
	// response as its own result, or hang until the 30 s
	// timeout. The fix kills the process on mid-call cancel,
	// exactly like the timeout path, so the next call respawns
	// a fresh process and gets its own result promptly.
	interp, path := writeSlowMockMCPServer(t, cancelMidCallMockMCPServer)
	srv := NewStdioServer("cancel-mid-call", interp, []string{path})
	defer srv.Close()
	if _, err := srv.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	slowCtx, cancel := context.WithCancel(context.Background())
	slowDone := make(chan error, 1)
	go func() {
		_, err := srv.CallTool(slowCtx, "slow_tool", nil)
		slowDone <- err
	}()
	// The server reads the request and sleeps 3 s on it, so a
	// cancel at 500 ms is certainly mid-call.
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-slowDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled slow call error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled slow call did not return within 10 s")
	}
	// The next call must succeed promptly with fast_tool's OWN
	// result — never slow_tool's, never a 30 s hang. The 10 s
	// context bounds the test if the fix regresses: a hung call
	// fails here in 10 s instead of at the 30 s server timeout.
	fastCtx, fastCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer fastCancel()
	start := time.Now()
	out, err := srv.CallTool(fastCtx, "fast_tool", nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("fast call after cancel: %v", err)
	}
	if out != "fast-result" {
		t.Fatalf("fast call = %q, want fast-result (a stale reader must not swallow or swap the response)", out)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("fast call took %s, want it prompt (well under the 30 s hang the bug caused)", elapsed)
	}
	t.Logf("fast call after mid-call cancel: %q in %s", out, elapsed)
}

func TestStdioServerCancelMidHandshakeIsNotSticky(t *testing.T) {
	// A cancellation during the initialize handshake is a
	// transient caller disconnect, not a wedged server: the
	// process is killed (the in-flight read must end), but the
	// error must NOT be recorded as a sticky start failure —
	// otherwise one HTTP disconnect during discovery would brick
	// the server until restart. A fresh context must list tools
	// (respawn + the mock's 3 s initialize sleep) and call them.
	interp, path := writeSlowMockMCPServer(t, slowInitMockMCPServer)
	srv := NewStdioServer("cancel-handshake", interp, []string{path})
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := srv.ListTools(ctx)
		done <- err
	}()
	// initialize sleeps 3 s, so a cancel at 500 ms is certainly
	// mid-handshake.
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled ListTools error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled ListTools did not return within 10 s")
	}
	tools, err := srv.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools after cancelled handshake: %v (want a retry, not the sticky start error)", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools after retry = %+v, want the echo tool", tools)
	}
	out, err := srv.CallTool(context.Background(), "echo", nil)
	if err != nil || out != "echo-ok" {
		t.Fatalf("echo after retry = %q, %v, want echo-ok", out, err)
	}
}

func TestStdioServerCtxDeadlineIsDeterministic(t *testing.T) {
	// Regression (H3 nit-fix, Nit 1): callLocked computed
	// deadline = min(now + s.timeout, ctx.Deadline()) and then
	// selected between <-ctx.Done() and <-time.After(...). When
	// the ctx deadline was the effective deadline both cases
	// became ready at the same instant and Go's select picked
	// randomly: ctx.Done() win -> context.DeadlineExceeded
	// (transient, non-sticky); timer win -> plain "timed out
	// after 30s" (sticky via fail() when it landed
	// mid-handshake). The fix makes the timeout branch check
	// ctx.Err() first, so a ctx-deadline expiry is always the
	// ctx error. This test proves determinism: 20 calls whose
	// ctx deadline (500 ms) is the effective deadline against a
	// 30 s server timeout must ALL return
	// context.DeadlineExceeded — never the timeout error.
	// Pre-fix each iteration is ~a coin flip, so 20/20 passing
	// by luck is ~1e-6; the test fails on the un-fixed code.
	interp, path := writeSlowMockMCPServer(t, slowCallMockMCPServer)
	srv := NewStdioServerWithTimeout("ctx-deadline", interp, []string{path}, 30*time.Second)
	defer srv.Close()
	if _, err := srv.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := srv.CallTool(ctx, "echo", nil)
		cancel()
		if err == nil {
			t.Fatalf("iteration %d: CallTool succeeded, want context.DeadlineExceeded", i)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: CallTool error = %v, want context.DeadlineExceeded (a timer win misclassifies the ctx expiry as a sticky timeout)", i, err)
		}
		if strings.Contains(err.Error(), "timed out") {
			t.Fatalf("iteration %d: CallTool error = %v, want the bare ctx error, not a timeout message", i, err)
		}
	}
	// The ctx expiry was transient, not a sticky start failure:
	// a fresh context must list tools again (respawn).
	tools, err := srv.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools after ctx-deadline calls: %v (want a retry, not the sticky start error)", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools after retry = %+v, want the echo tool", tools)
	}
}

func TestStdioServerTimeoutMessageNamesEffectiveDuration(t *testing.T) {
	// Regression (H3 nit-fix, Nit 3): the timeout message
	// printed s.timeout ("timed out after 30s") even when the
	// call ended after ~500 ms on its ctx deadline. The fix
	// prints the effective deadline duration instead.
	//
	// Note the Nit 1 interaction: once the timeout branch
	// returns ctx.Err() when the ctx deadline fired, the timeout
	// message is never emitted at all when the ctx deadline is
	// the effective deadline — so there is no "timed out after
	// 30s" message left to misname in that case, and sub-case B
	// asserts exactly that (the ctx error, with no misleading
	// timeout message). Sub-case A proves the message itself
	// names the effective duration when it IS emitted (server
	// timeout effective: 2 s, not the 30 s default, and not any
	// ctx duration).
	interp, path := writeSlowMockMCPServer(t, slowCallMockMCPServer)

	// A: server timeout (5 s) is the effective deadline — the
	// message must name 5 s. 5 s, not 2 s: ListTools must first
	// spawn python3 and finish the initialize handshake, which
	// takes seconds under full-gate load on 1.5 shared CPUs
	// (the same reason the existing slow-call test uses 2 s
	// instead of 200 ms) — while the 10 s tools/call sleep
	// still times out at 5 s with 2x margin.
	srv := NewStdioServerWithTimeout("timeout-msg", interp, []string{path}, 5*time.Second)
	defer srv.Close()
	if _, err := srv.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	_, err := srv.CallTool(context.Background(), "echo", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out after 5s") {
		t.Fatalf("server-timeout CallTool error = %v, want it to name the effective duration (timed out after 5s)", err)
	}
	if strings.Contains(err.Error(), "30s") {
		t.Fatalf("server-timeout CallTool error = %v, want no 30s in the message", err)
	}

	// B: ctx deadline (500 ms) is the effective deadline against
	// a 30 s server timeout — the call must end with the ctx
	// error, never with "timed out after 30s".
	srv2 := NewStdioServerWithTimeout("timeout-msg-ctx", interp, []string{path}, 30*time.Second)
	defer srv2.Close()
	if _, err := srv2.ListTools(context.Background()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := srv2.CallTool(ctx, "echo", nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: CallTool error = %v, want context.DeadlineExceeded", i, err)
		}
		if strings.Contains(err.Error(), "timed out after 30s") {
			t.Fatalf("iteration %d: CallTool error = %v, want no misleading 'timed out after 30s' for a ~500 ms ctx expiry", i, err)
		}
	}
}
